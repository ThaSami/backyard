package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
)

func siteFixture(t *testing.T, dir string) {
	t.Helper()
	writeFiles(t, dir, map[string]string{
		"index.html":                "<title>Demo</title>hello",
		"app.js":                    "console.log(1)",
		"css/site.css":              "body{}",
		".well-known/security.txt":  "contact: sec@acme.com",
		".git/config":               "[core]",
		"node_modules/lib/index.js": "x",
		"CLAUDE.md":                 "agent docs",
		".env":                      "SECRET=1",
		".backyard.json":            `{"site":"demo"}`,
	})
}

func TestDeployViaAPIUploadsAndCommits(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	dir := filepath.Join(h.cwd, "demo")
	siteFixture(t, dir)

	out := h.mustRun("deploy", "demo")
	if strings.TrimSpace(out) != g.SiteURL("demo") {
		t.Fatalf("stdout = %q, want the site URL", out)
	}
	want := map[string]string{
		"index.html": "<title>Demo</title>hello", "app.js": "console.log(1)", "css/site.css": "body{}",
		".well-known/security.txt": "contact: sec@acme.com",
	}
	got := g.SiteFiles("demo")
	if len(got) != len(want) {
		t.Fatalf("deployed files = %v", keys(got))
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("file %s = %q, want %q", p, got[p], c)
		}
	}
	if g.Uploads() != 4 || g.Count("POST", "/commit") != 1 {
		t.Fatalf("uploads=%d commits=%d", g.Uploads(), g.Count("POST", "/commit"))
	}
	meta, _ := g.Meta("demo")
	if meta.UpdatedBy != alice.ID {
		t.Fatalf("updated_by = %q", meta.UpdatedBy)
	}
	for _, r := range g.Requests() {
		if strings.Contains(r.Path, "/sites/") && r.Auth != "Bearer alice-token" && !strings.Contains(r.Path, "/uploads/") {
			t.Fatalf("request %s %s without bearer: %q", r.Method, r.Path, r.Auth)
		}
	}
	if !strings.Contains(h.err.String(), "Uploaded 4 files") {
		t.Fatalf("stderr = %s", h.err.String())
	}

	// Second deploy: one change, one deletion → one upload.
	writeFiles(t, dir, map[string]string{"app.js": "console.log(2)"})
	if err := os.Remove(filepath.Join(dir, "css", "site.css")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("deploy", "demo", "--json")
	var res deployOutput
	if err := json.Unmarshal([]byte(h.out.String()), &res); err != nil {
		t.Fatalf("json output: %v\n%s", err, h.out.String())
	}
	if res.Uploaded != 1 || res.Unchanged != 2 || res.Deleted != 1 || res.Method != "api" || res.URL != g.SiteURL("demo") {
		t.Fatalf("second deploy = %+v", res)
	}
	if g.SiteFiles("demo")["css/site.css"] != "" || g.SiteFiles("demo")["app.js"] != "console.log(2)" {
		t.Fatalf("after second deploy: %v", g.SiteFiles("demo"))
	}
}

func TestDeployDryRunChangesNothing(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	dir := filepath.Join(h.cwd, "demo")
	siteFixture(t, dir)
	out := h.mustRun("deploy", "demo", "--dry-run")
	if g.Uploads() != 0 || g.Count("POST", "/deploys") != 0 || g.SiteFiles("demo") != nil {
		t.Fatal("dry run changed the server")
	}
	for _, p := range []string{"index.html", "app.js", "css/site.css", ".well-known/security.txt"} {
		if !strings.Contains(out, p) {
			t.Errorf("dry run output lacks %s:\n%s", p, out)
		}
	}
	for _, p := range []string{".git", "node_modules", "CLAUDE.md", ".env"} {
		if strings.Contains(out, p) {
			t.Errorf("dry run lists ignored %s:\n%s", p, out)
		}
	}
}

func TestDeployDryRunWithoutServer(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(h.cwd, "demo")
	siteFixture(t, dir)
	out := h.mustRun("deploy", "demo", "--dry-run")
	if !strings.Contains(out, "index.html") || strings.Contains(out, "CLAUDE.md") || !strings.Contains(h.err.String(), "local files only") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out, h.err.String())
	}
	h.mustRun("--json", "deploy", "demo", "--dry-run")
	var res deployOutput
	if err := json.Unmarshal([]byte(h.out.String()), &res); err != nil || !res.DryRun || res.Uploaded == 0 || res.Uploaded != len(res.Files) {
		t.Fatalf("json = %s (%v)", h.out.String(), err)
	}
	// A real deploy, or a dry run of an explicit direct method, still needs the server.
	if errOut := h.mustFail("deploy", "demo"); !strings.Contains(errOut, "backyard login") {
		t.Fatalf("stderr = %s", errOut)
	}
	if errOut := h.mustFail("deploy", "demo", "--dry-run", "--method", "s3"); !strings.Contains(errOut, "s3") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestDeploySiteNameResolution(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	// Project file in an ancestor with dir "dist"; run from a nested folder.
	proj := filepath.Join(h.cwd, "My Project")
	writeFiles(t, proj, map[string]string{
		".backyard.json":  `{"site": "lunch-vote", "dir": "dist", "ignore": ["*.map"]}`,
		"dist/index.html": "hi",
		"dist/app.js.map": "map",
		"src/main.js":     "src",
	})
	h.cwd = filepath.Join(proj, "src")
	h.mustRun("deploy")
	files := g.SiteFiles("lunch-vote")
	if len(files) != 1 || files["index.html"] != "hi" {
		t.Fatalf("lunch-vote files = %v", keys(files))
	}
	// --site wins over the project file.
	h.mustRun("deploy", "--site", "other")
	if g.SiteFiles("other") == nil {
		t.Fatal("--site ignored")
	}
	// Folder names: build output folders use the parent's name.
	plain := filepath.Join(h.cwd, "..", "..", "Team Dash", "build")
	writeFiles(t, plain, map[string]string{"index.html": "x"})
	h.cwd = filepath.Dir(plain)
	h.mustRun("deploy", "build")
	if g.SiteFiles("team-dash") == nil {
		t.Fatalf("expected site team-dash; requests: %v", g.Requests())
	}
	// Invalid names are rejected before any upload.
	if errOut := h.mustFail("deploy", "build", "--site", "Bad_Name"); !strings.Contains(errOut, "invalid site name") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestDeployWarnsBeforeOverwritingOthersSite(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	g.AddSite("shared", map[string]string{"index.html": "bob's"}, "bob@acme.com")
	dir := filepath.Join(h.cwd, "shared")
	writeFiles(t, dir, map[string]string{"index.html": "alice's"})

	// Interactive and declined → nothing deployed.
	h.stdinTTY, h.stdin = true, "n\n"
	errOut := h.mustFail("deploy", "shared")
	if !strings.Contains(errOut, "last deployed by bob@acme.com") || !strings.Contains(errOut, "cancelled") {
		t.Fatalf("stderr = %s", errOut)
	}
	if g.SiteFiles("shared")["index.html"] != "bob's" {
		t.Fatal("declined deploy changed the site")
	}
	// Interactive and confirmed.
	h.stdin = "y\n"
	h.mustRun("deploy", "shared")
	if g.SiteFiles("shared")["index.html"] != "alice's" {
		t.Fatal("confirmed deploy did not update the site")
	}
	// Own site → no prompt (stdin empty would fail a prompt).
	h.stdin = ""
	writeFiles(t, dir, map[string]string{"index.html": "alice v2"})
	h.mustRun("deploy", "shared")
	if strings.Contains(h.err.String(), "last deployed by") {
		t.Fatalf("warned about own site: %s", h.err.String())
	}
	// Someone else again, -y skips the prompt.
	g.AddSite("shared", map[string]string{"index.html": "bob again"}, "bob@acme.com")
	h.mustRun("deploy", "shared", "-y")
	if !strings.Contains(h.err.String(), "last deployed by bob@acme.com") || g.SiteFiles("shared")["index.html"] != "alice v2" {
		t.Fatalf("-y deploy: stderr=%s files=%v", h.err.String(), g.SiteFiles("shared"))
	}
	// Not a terminal: warn and proceed.
	g.AddSite("shared", map[string]string{"index.html": "bob again"}, "bob@acme.com")
	h.stdinTTY = false
	h.mustRun("deploy", "shared")
	if g.SiteFiles("shared")["index.html"] != "alice v2" {
		t.Fatal("non-interactive deploy did not proceed")
	}
}

func TestDeployAutoMethodSelection(t *testing.T) {
	s3info := func(g *clitest.Gateway) {
		g.Info.Methods = []string{"api", "s3"}
		g.Info.Storage = &blob.Description{Type: "s3", Bucket: "acme-backyard", Prefix: "prod", Region: "us-east-1"}
	}
	t.Run("anonymous deploy uses api", func(t *testing.T) {
		h := newHarness(t)
		g := clitest.NewGateway(t)
		g.Anonymous = true
		g.Info.Auth = deploy.AuthInfo{Mode: "none", AnonymousDeploy: true}
		h.writeProfile("default", &clientconfig.Profile{Server: g.URL, Login: clientconfig.LoginAnonymous}, true)
		writeFiles(t, filepath.Join(h.cwd, "anon"), map[string]string{"index.html": "x"})
		h.mustRun("deploy", "anon")
		if g.SiteFiles("anon") == nil || len(h.runner.Calls()) != 0 {
			t.Fatal("anonymous api deploy failed")
		}
	})
	t.Run("not logged in falls back to s3", func(t *testing.T) {
		h := newHarness(t)
		g := clitest.NewGateway(t)
		s3info(g)
		h.tools["aws"] = true
		h.writeProfile("default", &clientconfig.Profile{Server: g.URL}, true)
		dir := filepath.Join(h.cwd, "site")
		writeFiles(t, dir, map[string]string{"index.html": "x"})
		out := h.mustRun("deploy", "site")
		calls := h.runner.Calls()
		if len(calls) != 1 || calls[0].Name != "aws" || calls[0].Args[3] != "s3://acme-backyard/prod/sites/site/" {
			t.Fatalf("calls = %+v", calls)
		}
		if strings.TrimSpace(out) != g.SiteURL("site") {
			t.Fatalf("stdout = %q", out)
		}
		if g.Count("POST", "/refresh") != 0 {
			t.Fatal("refresh without credentials")
		}
	})
	t.Run("tool missing", func(t *testing.T) {
		h := newHarness(t)
		g := clitest.NewGateway(t)
		s3info(g)
		h.writeProfile("default", &clientconfig.Profile{Server: g.URL}, true)
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x"})
		errOut := h.mustFail("deploy", "site")
		if !strings.Contains(errOut, "no usable deploy method") || !strings.Contains(errOut, "not logged in") || !strings.Contains(errOut, "aws not installed") {
			t.Fatalf("stderr = %s", errOut)
		}
	})
	t.Run("gateway down uses cached info", func(t *testing.T) {
		h := newHarness(t)
		g := clitest.NewGateway(t)
		s3info(g)
		info := g.Info
		h.tools["aws"] = true
		h.writeProfile("default", &clientconfig.Profile{Server: "http://127.0.0.1:1", Token: "t", Info: &info}, true)
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x"})
		h.mustRun("deploy", "site")
		if calls := h.runner.Calls(); len(calls) != 1 || calls[0].Name != "aws" {
			t.Fatalf("calls = %+v", calls)
		}
		if !strings.Contains(h.err.String(), "after the next refresh") {
			t.Fatalf("stderr = %s", h.err.String())
		}
	})
	t.Run("explicit s3 with token refreshes", func(t *testing.T) {
		h := newHarness(t)
		g := loggedIn(t, h)
		s3info(g)
		h.tools["aws"] = true
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x"})
		t.Setenv(clientconfig.EnvDeployMethod, "s3")
		h.mustRun("deploy", "site")
		if len(h.runner.Calls()) != 1 || g.Count("POST", "/sites/site/refresh") != 1 {
			t.Fatalf("calls=%d refresh=%d", len(h.runner.Calls()), g.Count("POST", "/refresh"))
		}
	})
	t.Run("project method", func(t *testing.T) {
		h := newHarness(t)
		g := loggedIn(t, h)
		g.Info.Methods = []string{"api", "rsync"}
		g.Info.RsyncTarget = "deploy@backyard.acme.com:/var/lib/backyard/sites/"
		h.tools["rsync"] = true
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x", ".backyard.json": `{"method":"rsync"}`})
		h.mustRun("deploy", "site")
		calls := h.runner.Calls()
		if len(calls) != 1 || calls[0].Name != "rsync" || calls[0].Args[len(calls[0].Args)-1] != "deploy@backyard.acme.com:/var/lib/backyard/sites/site/" {
			t.Fatalf("calls = %+v", calls)
		}
		// The flag wins over the project file.
		h.mustRun("deploy", "site", "--method", "api")
		if len(h.runner.Calls()) != 1 || g.SiteFiles("site") == nil {
			t.Fatal("--method api ignored")
		}
	})
	t.Run("direct sync failure", func(t *testing.T) {
		h := newHarness(t)
		g := loggedIn(t, h)
		s3info(g)
		h.tools["aws"] = true
		h.runner.fn = func(Command) error { return &ExitError{Code: 2, Err: os.ErrPermission} }
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x"})
		if code := h.run("deploy", "site", "-m", "s3"); code != 2 {
			t.Fatalf("exit = %d, stderr %s", code, h.err.String())
		}
		if g.Count("POST", "/refresh") != 0 {
			t.Fatal("refreshed after a failed sync")
		}
	})
	t.Run("no server", func(t *testing.T) {
		h := newHarness(t)
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x"})
		if errOut := h.mustFail("deploy", "site"); !strings.Contains(errOut, "backyard login --server") {
			t.Fatalf("stderr = %s", errOut)
		}
	})
}

// TestDeployToAnotherServerIgnoresTheProfilesCachedInfo: deploying to a
// server named by --server or BACKYARD_SERVER that is unreachable must not
// fall back to the stored profile's cached info, which describes the
// profile's (production) bucket: the sync would overwrite and delete the
// production copy of the site.
func TestDeployToAnotherServerIgnoresTheProfilesCachedInfo(t *testing.T) {
	prodInfo := &deploy.Info{PublicURL: "https://backyard.prod.example.com", URLMode: "path", Methods: []string{"api", "s3"},
		Storage: &blob.Description{Type: "s3", Bucket: "prod-bucket"}}
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.tools["aws"] = true
		h.writeProfile("default", &clientconfig.Profile{Server: "https://backyard.prod.example.com", Token: "prod-token", Info: prodInfo}, true)
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "staging build"})
		return h
	}
	// The staging server is down: its proxy answers every request with 404.
	down := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(down.Close)
	staging := down.URL
	for name, args := range map[string][]string{
		"flag, s3":   {"deploy", "site", "--server", staging, "--method", "s3", "--site", "demo"},
		"flag, auto": {"deploy", "site", "--server", staging, "--site", "demo"},
		"env, s3":    {"deploy", "site", "--method", "s3", "--site", "demo"},
		"env, auto":  {"deploy", "site", "--site", "demo"},
	} {
		t.Run(name, func(t *testing.T) {
			h := setup(t)
			if strings.HasPrefix(name, "env") {
				t.Setenv(clientconfig.EnvServer, staging)
			}
			errOut := h.mustFail(args...)
			if calls := h.runner.Calls(); len(calls) != 0 {
				t.Fatalf("ran %q against the profile's bucket", calls[0].Argv())
			}
			if strings.Contains(errOut, "prod-bucket") {
				t.Fatalf("stderr = %s", errOut)
			}
		})
	}
	// The profile's own server still falls back to its cache.
	h := setup(t)
	h.writeProfile("default", &clientconfig.Profile{Server: staging, Token: "t", Info: prodInfo}, true)
	h.mustRun("deploy", "site", "--method", "s3", "--site", "demo")
	if calls := h.runner.Calls(); len(calls) != 1 || !slices.Contains(calls[0].Args, "s3://prod-bucket/sites/demo/") {
		t.Fatalf("calls = %+v", calls)
	}
}

// TestDeployRefusesProjectDirOutsideTheProject: a .backyard.json (e.g. one
// that came with a pulled site or a cloned repository) cannot point a plain
// `backyard deploy` at a folder outside the project.
func TestDeployRefusesProjectDirOutsideTheProject(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	private := filepath.Join(h.cwd, "private")
	writeFiles(t, private, map[string]string{"passwords.txt": "hunter2"})
	proj := filepath.Join(h.cwd, "remix")
	for _, dir := range []string{private, "../private", "dist/../../private"} {
		b, _ := json.Marshal(map[string]string{"site": "loot", "dir": dir})
		writeFiles(t, proj, map[string]string{"index.html": "x", ".backyard.json": string(b)})
		h.cwd = proj
		errOut := h.mustFail("deploy")
		if !strings.Contains(errOut, ".backyard.json") || !strings.Contains(errOut, "inside") {
			t.Fatalf("dir %q: stderr = %s", dir, errOut)
		}
		if g.SiteFiles("loot") != nil || g.Uploads() != 0 {
			t.Fatalf("dir %q: published %v", dir, keys(g.SiteFiles("loot")))
		}
		// `backyard dev` refuses it as well.
		if errOut := h.mustFail("dev", "--no-open"); !strings.Contains(errOut, "inside") {
			t.Fatalf("dev: stderr = %s", errOut)
		}
	}
	// An explicit folder argument is the user's choice and still works.
	h.mustRun("deploy", private, "--site", "mine")
	if g.SiteFiles("mine")["passwords.txt"] != "hunter2" {
		t.Fatal("explicit dir argument")
	}
}

// TestDirectDeployRemovesIgnoredFiles: the sync tools never delete
// destination files their excludes match, so a file published earlier and
// ignored now (drafts/, *.pem added to .backyardignore to unpublish it)
// stayed online with the direct methods while the api method's commit
// deletes it. The CLI now removes such files after the sync.
func TestDirectDeployRemovesIgnoredFiles(t *testing.T) {
	setup := func(t *testing.T) (*harness, *clitest.Gateway) {
		h := newHarness(t)
		g := loggedIn(t, h)
		g.Info.Methods = []string{"api", "s3", "gcs"}
		g.Info.Storage = &blob.Description{Type: "s3", Bucket: "acme-backyard", Prefix: "prod", Region: "us-east-1"}
		h.tools["aws"] = true
		g.AddSite("site", map[string]string{"index.html": "old", "drafts/plan.html": "plan", "key.pem": "KEY", "about.html": "a"}, alice.ID)
		writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{
			"index.html": "new", "drafts/plan.html": "plan", "key.pem": "KEY", ".backyardignore": "drafts/\n*.pem\n",
		})
		return h, g
	}
	wantRm := []string{"aws", "s3", "rm", "s3://acme-backyard/prod/sites/site/", "--recursive", "--region", "us-east-1",
		"--exclude", "*", "--include", "drafts/plan.html", "--include", "key.pem"}

	h, g := setup(t)
	h.mustRun("deploy", "site", "-m", "s3", "--dry-run", "--json")
	var dry deployOutput
	if err := json.Unmarshal([]byte(h.out.String()), &dry); err != nil {
		t.Fatal(err)
	}
	if len(dry.Cleanup) != 1 || !reflect.DeepEqual(dry.Cleanup[0], wantRm) || len(h.runner.Calls()) != 0 {
		t.Fatalf("dry run cleanup = %q, calls %d", dry.Cleanup, len(h.runner.Calls()))
	}

	h.mustRun("deploy", "site", "-m", "s3")
	calls := h.runner.Calls()
	if len(calls) != 2 || calls[0].Args[1] != "sync" || !reflect.DeepEqual(calls[1].Argv(), wantRm) {
		t.Fatalf("calls = %q", calls)
	}
	if g.Count("POST", "/sites/site/refresh") != 1 || !strings.Contains(h.err.String(), "Removing 2 published files") {
		t.Fatalf("refresh=%d stderr=%s", g.Count("POST", "/refresh"), h.err.String())
	}

	// A failed removal fails the deploy: the files are still published.
	h, g = setup(t)
	h.runner.fn = func(c Command) error {
		if c.Args[1] == "rm" {
			return &ExitError{Code: 1, Err: os.ErrPermission}
		}
		return nil
	}
	if errOut := h.mustFail("deploy", "site", "-m", "s3"); !strings.Contains(errOut, "still published") {
		t.Fatalf("stderr = %s", errOut)
	}
	if g.Count("POST", "/sites/site/refresh") != 1 {
		t.Fatal("no refresh after the sync")
	}

	// gcs: the objects are named one by one.
	h, g = setup(t)
	g.Info.Storage = &blob.Description{Type: "gcs", Bucket: "acme-sites"}
	h.tools["gcloud"] = true
	h.mustRun("deploy", "site", "-m", "gcs")
	calls = h.runner.Calls()
	if len(calls) != 2 || !reflect.DeepEqual(calls[1].Argv(), []string{"gcloud", "storage", "rm",
		"gs://acme-sites/sites/site/drafts/plan.html", "gs://acme-sites/sites/site/key.pem"}) {
		t.Fatalf("calls = %q", calls)
	}

	// Without a usable gateway nothing can be listed: say so.
	h, g = setup(t)
	h.writeProfile("default", &clientconfig.Profile{Server: g.URL}, true) // logged out
	h.mustRun("deploy", "site", "-m", "s3")
	if len(h.runner.Calls()) != 1 || !strings.Contains(h.err.String(), "were not removed") {
		t.Fatalf("calls = %q, stderr = %s", h.runner.Calls(), h.err.String())
	}

	// rsync deletes excluded files itself (--delete-excluded).
	h, g = setup(t)
	g.Info.Methods = []string{"api", "rsync"}
	g.Info.RsyncTarget = "deploy@host:/srv/sites/"
	h.tools["rsync"] = true
	h.mustRun("deploy", "site", "-m", "rsync")
	calls = h.runner.Calls()
	if len(calls) != 1 || !slices.Contains(calls[0].Args, "--delete-excluded") || g.Count("GET", "/sites/site/files") != 0 {
		t.Fatalf("calls = %q", calls)
	}
}

func TestDeployDirectDryRunPrintsCommand(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	g.Info.Methods = []string{"api", "gcs"}
	g.Info.Storage = &blob.Description{Type: "gcs", Bucket: "acme-sites"}
	h.tools["gsutil"] = true
	writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x", "a/b.txt": "y"})
	h.mustRun("deploy", "site", "-m", "gcs", "--dry-run", "--json")
	var out deployOutput
	if err := json.Unmarshal([]byte(h.out.String()), &out); err != nil {
		t.Fatal(err)
	}
	if out.Command[0] != "gsutil" || !slices.Contains(out.Command, "gs://acme-sites/sites/site") || len(out.Files) != 2 || !out.DryRun {
		t.Fatalf("dry run = %+v", out)
	}
	if len(h.runner.Calls()) != 0 {
		t.Fatal("dry run executed the tool")
	}
}

func TestDeployUnauthorizedHint(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	h.writeProfile("default", &clientconfig.Profile{Server: g.URL, Token: "revoked"}, true)
	writeFiles(t, filepath.Join(h.cwd, "site"), map[string]string{"index.html": "x"})
	errOut := h.mustFail("deploy", "site")
	if !strings.Contains(errOut, "backyard login") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
