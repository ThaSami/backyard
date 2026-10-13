package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/cli/scaffold"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/version"
	"github.com/thasami/backyard/templates"
)

func TestInitWritesSkipsAndForces(t *testing.T) {
	h := newHarness(t)
	h.writeProfile("default", &clientconfig.Profile{Server: "https://backyard.acme.com"}, true)

	out := h.mustRun("init", "lunch-vote")
	dir := filepath.Join(h.cwd, "lunch-vote")
	for _, p := range []string{"index.html", ".backyard.json", "CLAUDE.md", "AGENTS.md", ".claude/skills/backyard/SKILL.md"} {
		if !fileExists(filepath.Join(dir, filepath.FromSlash(p))) {
			t.Errorf("missing %s", p)
		}
		if !strings.Contains(out, "+ "+p) {
			t.Errorf("output lacks + %s:\n%s", p, out)
		}
	}
	if !strings.Contains(out, "cd lunch-vote") || !strings.Contains(out, "http://lunch-vote.localhost:4321/") {
		t.Fatalf("next steps missing:\n%s", out)
	}
	claude, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if !strings.Contains(string(claude), "https://backyard.acme.com") {
		t.Fatalf("server not passed to templates: %s", claude)
	}
	pf, err := clientconfig.FindProject(dir)
	if err != nil || pf == nil || pf.Site != "lunch-vote" {
		t.Fatalf("project file: %+v %v", pf, err)
	}

	// Re-running keeps edited files.
	writeFiles(t, dir, map[string]string{"CLAUDE.md": "mine", "index.html": "my page"})
	out = h.mustRun("init", "lunch-vote", "--agents", "claude,cursor")
	if got, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); string(got) != "mine" {
		t.Fatalf("CLAUDE.md overwritten: %s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "index.html")); string(got) != "my page" {
		t.Fatalf("index.html overwritten: %s", got)
	}
	if !strings.Contains(out, "= CLAUDE.md (exists, kept)") || !strings.Contains(out, "+ .cursor/rules/backyard.mdc") || !strings.Contains(out, "--force") {
		t.Fatalf("output:\n%s", out)
	}

	// --force overwrites.
	h.mustRun("init", "lunch-vote", "--force", "--json")
	var res scaffold.Result
	if err := json.Unmarshal([]byte(h.out.String()), &res); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(res.Overwritten, "CLAUDE.md") || !slices.Contains(res.Overwritten, "index.html") {
		t.Fatalf("force result = %+v", res)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "index.html")); string(got) != "<title>lunch-vote</title>" {
		t.Fatalf("index.html = %s", got)
	}
}

func TestInitNamesAndValidation(t *testing.T) {
	h := newHarness(t)
	// No name: the current folder is initialized under its own name.
	h.cwd = filepath.Join(h.cwd, "My Dashboard")
	if err := os.MkdirAll(h.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun("init")
	if !strings.Contains(out, `"my-dashboard"`) || !fileExists(filepath.Join(h.cwd, "index.html")) || strings.Contains(out, "cd ") {
		t.Fatalf("output:\n%s", out)
	}
	// --dir with a name.
	h.mustRun("init", "x1", "--dir", "web")
	if !fileExists(filepath.Join(h.cwd, "web", ".backyard.json")) {
		t.Fatal("--dir ignored")
	}
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"init", "Bad_Name"}, "invalid site name"},
		{[]string{"init", "admin"}, "reserved"},
		{[]string{"init", "ok", "--agents", "claude,emacs"}, "unknown agent"},
		{[]string{"init", "ok2", "--template", "broken"}, "unknown template"},
		{[]string{"init", "a", "b"}, "accepts at most 1 arg"},
	}
	for _, tt := range tests {
		if errOut := h.mustFail(tt.args...); !strings.Contains(errOut, tt.want) {
			t.Errorf("%v: stderr = %s", tt.args, errOut)
		}
	}
}

func TestScaffoldWriteRefusesEscapes(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../evil", "/abs", "a/../../evil", `a\b`, ""} {
		if _, err := scaffold.Write(dir, map[string][]byte{bad: []byte("x")}, false); err == nil {
			t.Errorf("Write accepted %q", bad)
		}
	}
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := scaffold.Write(dir, map[string][]byte{"link/x.txt": []byte("x")}, true); err == nil {
			t.Fatal("wrote through a symlink")
		}
		if entries, _ := os.ReadDir(outside); len(entries) != 0 {
			t.Fatal("file written outside the target")
		}
	}
}

func TestPull(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	g.AddSite("tool", map[string]string{"index.html": "<h1>tool</h1>", "js/lib.js": "export {}", "img/logo.bin": "\x00\x01"}, "bob@acme.com")

	out := h.mustRun("pull", "tool")
	dir := filepath.Join(h.cwd, "tool")
	for p, want := range map[string]string{"index.html": "<h1>tool</h1>", "js/lib.js": "export {}", "img/logo.bin": "\x00\x01"} {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q %v", p, got, err)
		}
	}
	if !strings.Contains(out, "3 files downloaded") {
		t.Fatalf("stdout = %s", out)
	}
	// Unchanged files are skipped; local edits are protected.
	writeFiles(t, dir, map[string]string{"index.html": "local edit"})
	errOut := h.mustFail("pull", "tool")
	if !strings.Contains(errOut, "index.html") || !strings.Contains(errOut, "--force") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.mustRun("pull", "tool", "--force", "--json")
	var res pullResult
	if err := json.Unmarshal([]byte(h.out.String()), &res); err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Downloaded, ",") != "index.html" || len(res.Unchanged) != 2 {
		t.Fatalf("pull result = %+v", res)
	}
	// Custom target dir.
	h.mustRun("pull", "tool", "copy")
	if !fileExists(filepath.Join(h.cwd, "copy", "js", "lib.js")) {
		t.Fatal("pull into dir")
	}
	// Missing site.
	if errOut := h.mustFail("pull", "nope"); !strings.Contains(errOut, "not_found") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestPullStreamsLargeFiles(t *testing.T) {
	old := pullStreamAbove
	pullStreamAbove = 8
	t.Cleanup(func() { pullStreamAbove = old })
	h := newHarness(t)
	g := loggedIn(t, h)
	big := strings.Repeat("0123456789", 100)
	g.AddSite("media", map[string]string{"video/clip.bin": big, "a.txt": "small", "dir with space/ü.txt": "unicode path!"}, "bob@acme.com")
	h.mustRun("pull", "media")
	for p, want := range map[string]string{"video/clip.bin": big, "a.txt": "small", "dir with space/ü.txt": "unicode path!"} {
		got, err := os.ReadFile(filepath.Join(h.cwd, "media", filepath.FromSlash(p)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %.20q %v", p, got, err)
		}
	}
	if entries, _ := filepath.Glob(filepath.Join(h.cwd, "media", "video", ".backyard-pull-*")); len(entries) != 0 {
		t.Fatalf("temporary files left: %v", entries)
	}

	// Content that does not match the listing (changed meanwhile, or a
	// broken proxy) is rejected and nothing is written.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sites/media/files/long.bin":
			_, _ = w.Write([]byte(big + "more"))
		case "/sites/media/files/bad.bin":
			_, _ = w.Write([]byte(strings.Repeat("x", len(big))))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"no such file"}}`))
		}
	}))
	defer srv.Close()
	c := deploy.NewClient(srv.URL, "")
	sum := sha256.Sum256([]byte(big))
	dir := t.TempDir()
	for name, want := range map[string]string{"long.bin": "bytes", "bad.bin": "checksum", "gone.bin": "not_found"} {
		target := filepath.Join(dir, name)
		err := streamSiteFile(t.Context(), c, "media", deploy.SiteFile{Path: name, Size: int64(len(big)), SHA256: hex.EncodeToString(sum[:])}, target)
		if err == nil || !strings.Contains(err.Error(), want) || fileExists(target) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
}

func TestPullPathSafety(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	g.AddSite("evil", map[string]string{"index.html": "x"}, "mallory@acme.com")
	g.SetFileRaw("evil", "../escape.txt", []byte("pwned"))
	errOut := h.mustFail("pull", "evil")
	if !strings.Contains(errOut, "refusing to pull") {
		t.Fatalf("stderr = %s", errOut)
	}
	if fileExists(filepath.Join(h.cwd, "escape.txt")) || fileExists(filepath.Join(h.cwd, "evil", "index.html")) {
		t.Fatal("pull wrote files despite an unsafe path")
	}

	if runtime.GOOS == "windows" {
		return
	}
	// A symlink inside the target pointing outside is never followed.
	g2 := loggedIn(t, h)
	g2.AddSite("site2", map[string]string{"sub/file.txt": "x"}, "bob@acme.com")
	outside := t.TempDir()
	target := filepath.Join(h.cwd, "site2")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(target, "sub")); err != nil {
		t.Fatal(err)
	}
	errOut = h.mustFail("pull", "site2", "--force")
	if !strings.Contains(errOut, "symlink") {
		t.Fatalf("stderr = %s", errOut)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("pull wrote through a symlink")
	}
}

func TestTokenHash(t *testing.T) {
	h := newHarness(t)
	sum := sha256.Sum256([]byte("s3cret-token"))
	want := hex.EncodeToString(sum[:])
	out := h.mustRun("token", "hash", "s3cret-token", "--name", "github ci")
	if !strings.HasPrefix(out, want+"\n") || !strings.Contains(out, "auth:\n  tokens:\n    - name: \"github ci\"\n      sha256: "+want) {
		t.Fatalf("stdout = %s", out)
	}
	h.stdin = "s3cret-token\n"
	h.mustRun("token", "hash", "--json")
	var res map[string]string
	if err := json.Unmarshal([]byte(h.out.String()), &res); err != nil {
		t.Fatal(err)
	}
	if res["sha256"] != want || res["name"] != "ci" {
		t.Fatalf("json = %v", res)
	}
	h.stdin = ""
	if errOut := h.mustFail("token", "hash", "-"); !strings.Contains(errOut, "empty token") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestTokenCreate(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	out := h.mustRun("token", "create", "--name", "gha", "--ttl", "90d")
	if strings.TrimSpace(out) != g.MintedToken {
		t.Fatalf("stdout = %q", out)
	}
	reqs := g.TokenRequests()
	if len(reqs) != 1 || reqs[0].Auth != "Bearer alice-token" || reqs[0].Body["name"] != "gha" || reqs[0].Body["ttl_hours"] != float64(2160) {
		t.Fatalf("token requests = %+v", reqs)
	}
	for _, r := range g.Requests() {
		if r.Path == "/__backyard/api/tokens" && r.CSRF != "1" {
			t.Fatal("missing X-Backyard header")
		}
	}
	if errOut := h.mustFail("token", "create", "--ttl", "5m"); !strings.Contains(errOut, "at least 1h") {
		t.Fatalf("stderr = %s", errOut)
	}
	if errOut := h.mustFail("token", "create", "--ttl", "soon"); !strings.Contains(errOut, "invalid --ttl") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.writeProfile("default", &clientconfig.Profile{Server: g.URL}, true)
	if errOut := h.mustFail("token", "create"); !strings.Contains(errOut, "not logged in") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestSitesCommands(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	g.AddSite("lunch-vote", map[string]string{"index.html": "x"}, "bob@acme.com")
	g.AddSite("dashboard", map[string]string{"index.html": "y", "a.js": "z"}, "alice@acme.com")

	out := h.mustRun("sites", "list")
	if !strings.HasPrefix(out, "NAME") || !strings.Contains(out, "lunch-vote") || !strings.Contains(out, "dashboard") ||
		strings.Index(out, "dashboard") > strings.Index(out, "lunch-vote") {
		t.Fatalf("table:\n%s", out)
	}
	h.mustRun("sites", "list", "--json")
	var list struct{ Sites []sites.Meta }
	if err := json.Unmarshal([]byte(h.out.String()), &list); err != nil || len(list.Sites) != 2 {
		t.Fatalf("json: %v %s", err, h.out.String())
	}
	h.mustRun("sites", "ls", "LUNCH", "--json")
	if err := json.Unmarshal([]byte(h.out.String()), &list); err != nil || len(list.Sites) != 1 || list.Sites[0].Name != "lunch-vote" {
		t.Fatalf("filter: %v %s", err, h.out.String())
	}

	out = h.mustRun("sites", "info", "dashboard")
	if !strings.Contains(out, "Files:") || !strings.Contains(out, "2 (") || !strings.Contains(out, g.SiteURL("dashboard")) {
		t.Fatalf("info:\n%s", out)
	}
	if errOut := h.mustFail("sites", "info", "missing"); !strings.Contains(errOut, "not_found") {
		t.Fatalf("stderr = %s", errOut)
	}

	// Delete requires confirmation.
	if errOut := h.mustFail("sites", "delete", "lunch-vote"); !strings.Contains(errOut, "-y") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.stdinTTY, h.stdin = true, "no\n"
	h.mustFail("sites", "delete", "lunch-vote")
	if _, ok := g.Meta("lunch-vote"); !ok {
		t.Fatal("deleted without confirmation")
	}
	h.stdin = "yes\n"
	h.mustRun("sites", "delete", "lunch-vote")
	if _, ok := g.Meta("lunch-vote"); ok {
		t.Fatal("not deleted")
	}
	h.stdinTTY = false
	h.mustRun("sites", "rm", "dashboard", "-y")
	if _, ok := g.Meta("dashboard"); ok {
		t.Fatal("not deleted with -y")
	}
	out = h.mustRun("sites", "list")
	if out != "" || !strings.Contains(h.err.String(), "No sites yet") {
		t.Fatalf("empty list: %q %q", out, h.err.String())
	}
}

func TestOpen(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	out := h.mustRun("open", "lunch")
	if strings.TrimSpace(out) != g.SiteURL("lunch") || slices.Index(h.openedURLs(), g.SiteURL("lunch")) < 0 {
		t.Fatalf("open site: %q %v", out, h.openedURLs())
	}
	out = h.mustRun("open")
	if strings.TrimSpace(out) != g.URL+"/" {
		t.Fatalf("open hub: %q", out)
	}
	// Host mode URLs.
	g.Info.URLMode = "host"
	g.Info.PublicURL = "https://backyard.acme.com"
	h.mustRun("open", "lunch", "--json", "--server", g.URL)
	var res map[string]string
	_ = json.Unmarshal([]byte(h.out.String()), &res)
	if res["url"] != "https://lunch.backyard.acme.com/" {
		t.Fatalf("host-mode url = %v", res)
	}
	if errOut := h.mustFail("open", "Not_Valid"); !strings.Contains(errOut, "invalid site name") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestSkillInstall(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("skill", "install")
	dir := filepath.Join(h.home, ".claude", "skills", "backyard")
	for _, p := range []string{"SKILL.md", "references/api.md", "references/sub/extra.md"} {
		if !fileExists(filepath.Join(dir, filepath.FromSlash(p))) {
			t.Errorf("missing %s", p)
		}
	}
	if !strings.Contains(out, dir) {
		t.Fatalf("stdout = %s", out)
	}
	// Existing install: needs confirmation (non-interactive → error).
	writeFiles(t, dir, map[string]string{"stale.md": "old"})
	if errOut := h.mustFail("skill", "install"); !strings.Contains(errOut, "-y") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.mustRun("skill", "install", "-y")
	if fileExists(filepath.Join(dir, "stale.md")) {
		t.Fatal("old files kept")
	}
	h.stdinTTY, h.stdin = true, "y\n"
	h.mustRun("skill", "install")
	h.stdinTTY = false
	// Project install.
	h.mustRun("skill", "install", "--project")
	if !fileExists(filepath.Join(h.cwd, ".claude", "skills", "backyard", "SKILL.md")) {
		t.Fatal("project install")
	}
	if errOut := h.mustFail("skill", "install", "--project", "--global"); !strings.Contains(errOut, "either") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestConfigCommands(t *testing.T) {
	h := newHarness(t)
	out := h.mustRun("config", "example")
	if !strings.Contains(out, "server:") {
		t.Fatalf("example = %.200s", out)
	}
	bad := filepath.Join(h.cwd, "bad.yaml")
	writeFiles(t, h.cwd, map[string]string{"bad.yaml": "server:\n  url_mode: weird\nstorage:\n  type: s3\n"})
	if code := h.run("config", "validate", bad); code != 1 {
		t.Fatalf("exit = %d", code)
	}
	errOut := h.err.String()
	for _, want := range []string{"Invalid configuration", "server.base_domain is required", "url_mode", "storage.s3.bucket", "server.secret"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "Error:") {
		t.Fatalf("errors printed twice:\n%s", errOut)
	}
	writeFiles(t, h.cwd, map[string]string{"good.yaml": "server:\n  base_domain: backyard.acme.com\n  secret: 0123456789abcdef0123456789abcdef\n"})
	out = h.mustRun("config", "validate", filepath.Join(h.cwd, "good.yaml"))
	if !strings.Contains(out, "Configuration OK") || !strings.Contains(out, "https://backyard.acme.com") {
		t.Fatalf("validate = %s", out)
	}
	h.mustRun("config", "validate", "--dev", "--json", filepath.Join(h.cwd, "good.yaml"))
	var res map[string]any
	if err := json.Unmarshal([]byte(h.out.String()), &res); err != nil || res["valid"] != true {
		t.Fatalf("json = %s", h.out.String())
	}
	if code := h.run("config", "validate", "--json", bad); code != 1 || !strings.Contains(h.out.String(), `"valid": false`) {
		t.Fatalf("json invalid: %d %s", code, h.out.String())
	}
}

func TestVersionAndUsageErrors(t *testing.T) {
	h := newHarness(t)
	if out := h.mustRun("version"); !strings.Contains(out, version.Version) {
		t.Fatalf("version = %s", out)
	}
	if out := h.mustRun("--version"); !strings.Contains(out, version.Version) {
		t.Fatalf("--version = %s", out)
	}
	h.mustRun("version", "--json")
	if !strings.Contains(h.out.String(), `"version"`) {
		t.Fatal("version json")
	}
	if errOut := h.mustFail("frobnicate"); !strings.Contains(errOut, "unknown command") || !strings.Contains(errOut, "backyard --help") {
		t.Fatalf("stderr = %s", errOut)
	}
	if out := h.mustRun("--help"); !strings.Contains(out, "deploy") || !strings.Contains(out, "cloudflare") {
		t.Fatalf("help = %s", out)
	}
	if out := h.mustRun(); !strings.Contains(out, "Usage") {
		t.Fatalf("bare = %s", out)
	}
}

func TestServeGatewayDev(t *testing.T) {
	h := newHarness(t)
	t.Setenv("BACKYARD_SECRET", "0123456789abcdef0123456789abcdef-test")
	h.mustRun("serve", "--dev")
	// A missing config file fails before anything runs.
	if errOut := h.mustFail("gateway", "--config", filepath.Join(h.cwd, "missing.yaml")); !strings.Contains(errOut, "read config") {
		t.Fatalf("stderr = %s", errOut)
	}
	if len(h.served) != 1 {
		t.Fatalf("served = %d", len(h.served))
	}
	if c := h.served[0]; !c.cfg.Server.Dev || c.cfg.Auth.Mode != "none" || c.opts.GatewayOnly || c.opts.Logger == nil {
		t.Fatalf("serve --dev: %+v %+v", c.cfg.Server, c.opts)
	}
	writeFiles(t, h.cwd, map[string]string{"gw.yaml": "server:\n  base_domain: backyard.acme.com\n  secret: 0123456789abcdef0123456789abcdef\nstorage:\n  type: s3\n  s3:\n    bucket: b\n"})
	h.mustRun("gateway", "-c", filepath.Join(h.cwd, "gw.yaml"))
	if c := h.served[1]; !c.opts.GatewayOnly || c.cfg.Storage.S3.Bucket != "b" {
		t.Fatalf("gateway: %+v", c.opts)
	}

	// dev: project file site, custom port, no browser.
	site := filepath.Join(h.cwd, "proj")
	writeFiles(t, site, map[string]string{".backyard.json": `{"site":"lunch","dir":"public"}`, "public/index.html": "x"})
	h.cwd = site
	out := h.mustRun("dev", "--port", "4555", "--no-open")
	if strings.TrimSpace(out) != "http://lunch.localhost:4555/" {
		t.Fatalf("dev stdout = %q", out)
	}
	c := h.served[2]
	if c.cfg.Server.Listen != "127.0.0.1:4555" || c.cfg.Server.PublicURL != "http://localhost:4555" || c.cfg.Server.BaseDomain != "localhost" ||
		c.cfg.Server.URLMode != "host" || !c.cfg.Server.Dev || c.opts.DevSites["lunch"] != filepath.Join(site, "public") ||
		!filepath.IsAbs(c.cfg.Storage.FS.Root) || !c.cfg.Deploy.AllowAnonymous {
		t.Fatalf("dev config: %+v %+v %+v", c.cfg.Server, c.cfg.Storage, c.opts)
	}
	if len(h.openedURLs()) != 0 {
		t.Fatal("--no-open opened a browser")
	}
	// dev [dir] --site with an explicit folder; invalid inputs.
	other := filepath.Join(h.cwd, "other")
	writeFiles(t, other, map[string]string{"index.html": "y"})
	h.mustRun("dev", "other", "--site", "custom", "--no-open")
	if h.served[3].opts.DevSites["custom"] != other {
		t.Fatalf("dev sites = %v", h.served[3].opts.DevSites)
	}
	if errOut := h.mustFail("dev", "--port", "70000"); !strings.Contains(errOut, "invalid --port") {
		t.Fatalf("stderr = %s", errOut)
	}
	if errOut := h.mustFail("dev", "missing-dir"); !strings.Contains(errOut, "not a directory") {
		t.Fatalf("stderr = %s", errOut)
	}
	if errOut := h.mustFail("dev", "other", "--site", "api"); !strings.Contains(errOut, "reserved") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestCloudflareSetup(t *testing.T) {
	h := newHarness(t)
	repo := filepath.Join(h.cwd, "backyard")
	cf := filepath.Join(repo, "deploy", "cloudflare")
	writeFiles(t, cf, map[string]string{"package.json": cfPackageJSON, "package-lock.json": "{}"})

	// node/npm are required.
	h.cwd = filepath.Join(repo, "internal")
	if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "nodejs.org") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.tools["node"], h.tools["npm"] = true, true
	h.mustRun("cloudflare", "setup", "--", "--worker-name", "acme-backyard")
	calls := h.runner.Calls()
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Name != "npm" || strings.Join(calls[0].Args, " ") != "ci" || calls[0].Dir != cf || calls[0].Stdout == nil {
		t.Fatalf("install = %+v", calls[0])
	}
	if strings.Join(calls[1].Args, " ") != "run setup -- --worker-name acme-backyard" || calls[1].Dir != cf || calls[1].Stdin == nil {
		t.Fatalf("setup = %+v", calls[1])
	}
	// Without a lock file: npm install; --skip-install skips it; --dir works from anywhere.
	os.Remove(filepath.Join(cf, "package-lock.json"))
	h.cwd = t.TempDir()
	h.mustRun("cloudflare", "setup", "--dir", cf)
	if c := h.runner.Calls(); strings.Join(c[2].Args, " ") != "install" || strings.Join(c[3].Args, " ") != "run setup" {
		t.Fatalf("calls = %+v", c[2:])
	}
	h.mustRun("cloudflare", "setup", "--dir", cf, "--skip-install")
	if c := h.runner.Calls(); len(c) != 5 || strings.Join(c[4].Args, " ") != "run setup" {
		t.Fatalf("calls = %+v", c)
	}
	// Errors.
	if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "--download") || !strings.Contains(errOut, "deploy/cloudflare") {
		t.Fatalf("stderr = %s", errOut)
	}
	if errOut := h.mustFail("cloudflare", "setup", "--dir", h.cwd); !strings.Contains(errOut, "package.json") {
		t.Fatalf("stderr = %s", errOut)
	}
	if errOut := h.mustFail("cloudflare", "setup", "stray"); !strings.Contains(errOut, `after "--"`) {
		t.Fatalf("stderr = %s", errOut)
	}
	h.runner.fn = func(c Command) error {
		if len(c.Args) > 0 && c.Args[0] == "run" {
			return &ExitError{Code: 3, Err: os.ErrInvalid}
		}
		return nil
	}
	if code := h.run("cloudflare", "setup", "--dir", cf); code != 3 {
		t.Fatalf("exit = %d", code)
	}
}

// tarGz builds a gzipped tarball with the given entries (name → content;
// names ending in "/" are directories, "->target" suffix makes a symlink).
func tarGz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := keys(entries)
	for _, name := range names {
		content := entries[name]
		switch {
		case strings.HasSuffix(name, "/"):
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
		case strings.HasPrefix(content, "->"):
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: strings.TrimPrefix(content, "->")})
		default:
			_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
			_, _ = tw.Write([]byte(content))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCloudflareSetupDownload(t *testing.T) {
	h := newHarness(t)
	h.tools["node"], h.tools["npm"] = true, true
	archive := tarGz(t, map[string]string{
		"backyard-main/": "",
		"backyard-main/deploy/cloudflare/package.json":  cfPackageJSON,
		"backyard-main/deploy/cloudflare/src/worker.ts": "export default {}",
		"backyard-main/web/sdk/backyard.js":             "sdk",
		"backyard-main/link":                            "->/etc/passwd",
	})
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.URL.Path != "/main" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(archive)
	}))
	defer srv.Close()
	d := h.deps()
	d.downloadBase = srv.URL + "/"
	run := func(args ...string) (int, string) {
		h.out.Reset()
		h.err.Reset()
		streams := IOStreams{In: strings.NewReader(h.stdin), Out: &h.out, ErrOut: &h.err}
		code := execute(t.Context(), newRootCommand(streams, d), streams, args)
		return code, h.err.String()
	}
	if code, errOut := run("cloudflare", "setup", "--download"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	cf := filepath.Join(h.cache, "backyard", "src", "main", "deploy", "cloudflare")
	if gotPath != "/main" || !fileExists(filepath.Join(cf, "src", "worker.ts")) ||
		!fileExists(filepath.Join(h.cache, "backyard", "src", "main", "web", "sdk", "backyard.js")) {
		t.Fatalf("download: path=%s", gotPath)
	}
	if _, err := os.Lstat(filepath.Join(h.cache, "backyard", "src", "main", "link")); err == nil {
		t.Fatal("symlink extracted")
	}
	if c := h.runner.Calls(); len(c) != 2 || c[1].Dir != cf {
		t.Fatalf("calls = %+v", c)
	}
	// Second run reuses the cached download without --download.
	gotPath = ""
	if code, errOut := run("cloudflare", "setup"); code != 0 || gotPath != "" {
		t.Fatalf("exit %d (path %q): %s", code, gotPath, errOut)
	}
	// Unknown ref.
	if code, errOut := run("cloudflare", "setup", "--download", "--ref", "v9.9.9"); code == 0 || !strings.Contains(errOut, "HTTP 404") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if code, errOut := run("cloudflare", "setup", "--download", "--ref", "../x"); code == 0 || !strings.Contains(errOut, "invalid --ref") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	for _, name := range []string{"root/../../evil.txt", "root/a/../../../evil.txt"} {
		dest := t.TempDir()
		archive := tarGz(t, map[string]string{name: "x"})
		err := extractTarGz(bytes.NewReader(archive), dest)
		if err == nil && fileExists(filepath.Join(filepath.Dir(dest), "evil.txt")) {
			t.Fatalf("%s escaped", name)
		}
	}
	if err := extractTarGz(strings.NewReader("not gzip"), t.TempDir()); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestDefaultSourceRef(t *testing.T) {
	for in, want := range map[string]string{"0.1.0": "v0.1.0", "v1.2.3": "v1.2.3", "0.1.0-dev": "main", "": "main"} {
		if got := defaultSourceRef(in); got != want {
			t.Errorf("defaultSourceRef(%q) = %q", in, got)
		}
	}
	for ref, ok := range map[string]bool{"main": true, "v1.2.3": true, "feature/x": true, "../x": false, "-x": false, "a b": false} {
		if validRef(ref) != ok {
			t.Errorf("validRef(%q) != %v", ref, ok)
		}
	}
}

func TestHelpers(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 5 << 20: "5.0 MiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q", n, got)
		}
	}
	if truncate("héllo wörld", 5) != "héll…" || truncate("abc", 5) != "abc" {
		t.Error("truncate")
	}
	if defaultSiteName("/x/My App/dist") != "my-app" || defaultSiteName("/x/tool") != "tool" {
		t.Error("defaultSiteName")
	}
	if shellQuote("") != "''" || shellQuote("a b") != "'a b'" || shellQuote("--x=y") != "--x=y" {
		t.Error("shellQuote")
	}
	if yamlScalar("ci") != "ci" || yamlScalar("a: b") != `"a: b"` {
		t.Error("yamlScalar")
	}
	_ = clitest.GatewayPath
}

func TestPanicsAreReported(t *testing.T) {
	h := newHarness(t)
	h.render = func(templates.InitOptions) (map[string][]byte, error) { panic("templates: not implemented") }
	if code := h.run("init", "ok"); code != 2 {
		t.Fatalf("exit = %d", code)
	}
	if errOut := h.err.String(); !strings.Contains(errOut, "internal error: templates: not implemented") || strings.Contains(errOut, "goroutine") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.run("init", "ok", "--verbose")
	if !strings.Contains(h.err.String(), "goroutine") {
		t.Fatalf("verbose stderr lacks the stack: %s", h.err.String())
	}
}
