package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
)

// cfPackageJSON is a minimal deploy/cloudflare/package.json.
const cfPackageJSON = `{"name":"backyard-cloudflare","scripts":{"setup":"node scripts/setup.mjs"}}`

// cloudflareWorker is a fake Workers-edition server: no user login, deploys
// gated by DEPLOY_TOKEN (SPEC §16 defaults).
func cloudflareWorker(t *testing.T) *clitest.Gateway {
	t.Helper()
	g := clitest.NewGateway(t)
	g.Info.Auth = deploy.AuthInfo{Mode: "none"}
	g.Tokens["byd_secret"] = identity.User{ID: "token:deploy", Name: "deploy", Admin: true}
	return g
}

// fakeSetupScript makes the fake runner act like `npm run setup`: it writes
// the state file setup.mjs keeps for the Worker.
func fakeSetupScript(h *harness, worker, url, token string) {
	h.runner.fn = func(c Command) error {
		if len(c.Args) < 2 || c.Args[0] != "run" || c.Args[1] != "setup" {
			return nil
		}
		dir := filepath.Join(h.home, ".config", "backyard", "cloudflare")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		b, _ := json.Marshal(map[string]any{"url": url, "deployToken": token, "updatedAt": time.Now().UTC()})
		return os.WriteFile(filepath.Join(dir, worker+".json"), b, 0o600)
	}
}

func TestCloudflareSetupLogsTheCLIIn(t *testing.T) {
	h := newHarness(t)
	h.tools["node"], h.tools["npm"] = true, true
	cf := filepath.Join(h.cwd, "deploy", "cloudflare")
	writeFiles(t, cf, map[string]string{"package.json": cfPackageJSON, "package-lock.json": "{}"})
	g := cloudflareWorker(t)
	fakeSetupScript(h, "backyard", g.URL+"/", "byd_secret")

	h.mustRun("cloudflare", "setup")
	if !strings.Contains(h.err.String(), "now logged in to "+g.URL) {
		t.Fatalf("stderr = %s", h.err.String())
	}
	f := h.loadConfig()
	p := f.Profile("default")
	if f.Current != "default" || p == nil || p.Server != g.URL || p.Token != "byd_secret" || p.Login != clientconfig.LoginToken ||
		p.User == nil || p.User.ID != "token:deploy" || p.Info == nil || p.Info.Auth.Mode != "none" {
		t.Fatalf("config = %+v / %+v", f, p)
	}

	// The OpenQuick flow: init, then deploy with no login step.
	writeFiles(t, h.cwd, map[string]string{"site/index.html": "<title>x</title>"})
	h.mustRun("deploy", "site", "--site", "lunch")
	if m, ok := g.Meta("lunch"); !ok || m.UpdatedBy != "token:deploy" {
		t.Fatalf("site not deployed with the deploy token: %+v", m)
	}

	// A profile for another server is kept: the Worker gets its own profile.
	h.writeProfile("default", &clientconfig.Profile{Server: "https://backyard.acme.com", Token: "corp"}, true)
	fakeSetupScript(h, "team-board", g.URL, "byd_secret")
	h.mustRun("cloudflare", "setup", "--", "--worker-name", "team-board")
	f = h.loadConfig()
	if f.Current != "team-board" || f.Profile("team-board").Server != g.URL || f.Profile("default").Token != "corp" {
		t.Fatalf("config = %+v", f)
	}

	// --profile picks the profile; --no-login skips the login.
	h.mustRun("cloudflare", "setup", "--profile", "edge", "--", "--name=team-board")
	if f = h.loadConfig(); f.Current != "edge" || f.Profile("edge").Token != "byd_secret" {
		t.Fatalf("config = %+v", f)
	}
	h.writeProfile("default", &clientconfig.Profile{Server: "https://backyard.acme.com", Token: "corp"}, true)
	h.mustRun("cloudflare", "setup", "--no-login")
	if f = h.loadConfig(); f.Current != "default" {
		t.Fatalf("--no-login changed the current profile: %+v", f)
	}
}

func TestCloudflareSetupLoginEdgeCases(t *testing.T) {
	h := newHarness(t)
	h.tools["node"], h.tools["npm"] = true, true
	cf := filepath.Join(h.cwd, "deploy", "cloudflare")
	writeFiles(t, cf, map[string]string{"package.json": cfPackageJSON, "package-lock.json": "{}"})

	// The script wrote nothing (e.g. "-- --help"): no login, no error.
	h.mustRun("cloudflare", "setup", "--", "--help")
	if f := h.loadConfig(); len(f.Profiles) != 0 {
		t.Fatalf("profiles = %+v", f.Profiles)
	}

	// A stale state file from an earlier run is ignored.
	dir := filepath.Join(h.home, ".config", "backyard", "cloudflare")
	writeFiles(t, dir, map[string]string{"old.json": `{"url":"https://old.example.workers.dev","deployToken":"x"}`})
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "old.json"), past, past); err != nil {
		t.Fatal(err)
	}
	h.mustRun("cloudflare", "setup")
	if f := h.loadConfig(); len(f.Profiles) != 0 {
		t.Fatalf("profiles = %+v", f.Profiles)
	}

	// The server rejects the token: warn, keep the config untouched.
	g := cloudflareWorker(t)
	fakeSetupScript(h, "backyard", g.URL, "wrong")
	h.mustRun("cloudflare", "setup")
	if !strings.Contains(h.err.String(), "did not accept its deploy token") {
		t.Fatalf("stderr = %s", h.err.String())
	}
	if f := h.loadConfig(); len(f.Profiles) != 0 {
		t.Fatalf("profiles = %+v", f.Profiles)
	}

	// Not answering yet (workers.dev propagation): saved with a warning.
	notYet := httptest.NewServer(http.NotFoundHandler())
	defer notYet.Close()
	fakeSetupScript(h, "backyard", notYet.URL, "byd_secret")
	h.mustRun("cloudflare", "setup")
	if !strings.Contains(h.err.String(), "not answering yet") {
		t.Fatalf("stderr = %s", h.err.String())
	}
	if p := h.loadConfig().Profile("default"); p == nil || p.Server != notYet.URL || p.Token != "byd_secret" {
		t.Fatalf("profile = %+v", p)
	}

	// No URL in the state: point at the printed login command.
	fakeSetupScript(h, "backyard", "", "byd_secret")
	h.mustRun("cloudflare", "setup")
	if !strings.Contains(h.err.String(), "backyard login --server") {
		t.Fatalf("stderr = %s", h.err.String())
	}
}

func TestCloudflareSetupIgnoresOtherProjects(t *testing.T) {
	h := newHarness(t)
	h.tools["node"], h.tools["npm"] = true, true
	// Another project's deploy/cloudflare: its npm scripts must not run.
	writeFiles(t, filepath.Join(h.cwd, "deploy", "cloudflare"), map[string]string{
		"package.json": `{"name":"someone-elses-worker","scripts":{"setup":"rm -rf ~"}}`})
	if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "could not find") {
		t.Fatalf("stderr = %s", errOut)
	}
	if c := h.runner.Calls(); len(c) != 0 {
		t.Fatalf("ran %+v", c)
	}
	// An explicit --dir needs a setup script.
	writeFiles(t, filepath.Join(h.cwd, "nosetup"), map[string]string{"package.json": `{"name":"backyard-cloudflare"}`})
	if errOut := h.mustFail("cloudflare", "setup", "--dir", "nosetup"); !strings.Contains(errOut, `"setup" script`) {
		t.Fatalf("stderr = %s", errOut)
	}
	// The real package in this repository is recognized.
	if !isBackyardCloudflare(filepath.Join("..", "..", "deploy", "cloudflare")) {
		t.Fatal("deploy/cloudflare of this repository not recognized")
	}
}

func TestSetupWorkerName(t *testing.T) {
	for in, want := range map[string]string{
		"":                            "",
		"--worker-name team":          "team",
		"--r2 --worker-name=team":     "team",
		"--name team --r2":            "team",
		"--name=x":                    "x",
		"--worker-name":               "",
		"--var NAME:x --rotate-token": "",
	} {
		if got := setupWorkerName(strings.Fields(in)); got != want {
			t.Errorf("setupWorkerName(%q) = %q, want %q", in, got, want)
		}
	}
	if profileNameFor("team-board") != "team-board" || profileNameFor("") != "cloudflare" || profileNameFor("a b") != "cloudflare" {
		t.Fatal("profileNameFor")
	}
}

func TestExtractTarGzDirectoryAfterItsFiles(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "repo-main/a/b.txt", Typeflag: tar.TypeReg, Mode: 0o755, Size: 1})
	_, _ = tw.Write([]byte("x"))
	_ = tw.WriteHeader(&tar.Header{Name: "repo-main/a/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "sha"}})
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := extractTarGz(&buf, dest); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dest, "a", "b.txt"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("b.txt: %v %v", st, err)
	}
}
