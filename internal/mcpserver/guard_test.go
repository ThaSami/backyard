package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
)

func mkdir(t *testing.T, dir string, files ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("<h1>"+f+"</h1>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// TestDirectoriesStayInTheWorkingTree: an agent can only scaffold into or
// publish folders of the project it was started in, unless it names
// another existing folder by its absolute path; never the filesystem root
// or the home directory.
func TestDirectoriesStayInTheWorkingTree(t *testing.T) {
	e := newEnv(t, true, nil)
	root := filepath.Dir(e.cwd)
	home := filepath.Join(root, "home")
	mkdir(t, home)
	t.Setenv("HOME", home)
	outside := filepath.Join(root, "outside")
	mkdir(t, outside, "index.html")
	if err := os.Symlink(outside, filepath.Join(e.cwd, "link")); err != nil {
		t.Fatal(err)
	}

	refused := []struct {
		tool string
		dir  string
		want string
	}{
		{"backyard_init", "../escaped", "outside the working directory"},
		{"backyard_init", "sub/../../escaped", "outside the working directory"},
		{"backyard_init", "link/new", "outside the working directory"}, // through a symlink
		{"backyard_init", filepath.Join(root, "escaped"), "not an existing directory"},
		{"backyard_init", "~", "home directory"},
		{"backyard_init", "~/new-site", "not an existing directory"},
		{"backyard_init", "x\x00y", "NUL"},
		{"backyard_deploy", "../outside", "outside the working directory"},
		{"backyard_deploy", "link", "outside the working directory"},
		{"backyard_deploy", "/", "filesystem root"},
		{"backyard_deploy", home, "home directory"},
		{"backyard_deploy", filepath.Join(root, "nope"), "not an existing directory"},
	}
	for _, tt := range refused {
		res := e.call(tt.tool, map[string]any{"dir": tt.dir, "site": "escape"})
		if !res.IsError || !strings.Contains(text(res), tt.want) {
			t.Errorf("%s %q: error=%v %q, want %q", tt.tool, tt.dir, res.IsError, text(res), tt.want)
		}
	}
	if exists(filepath.Join(root, "escaped")) || exists(filepath.Join(outside, "new")) || exists(filepath.Join(home, "new-site")) ||
		exists(filepath.Join(outside, ".backyard.json")) {
		t.Fatal("a refused backyard_init wrote files")
	}
	if e.g.SiteFiles("escape") != nil || e.g.Count(http.MethodPost, "/deploys") != 0 {
		t.Fatal("a refused backyard_deploy reached the gateway")
	}

	// Inside the working tree, relative or absolute, new folders are fine.
	var initOut InitOutput
	decode(t, e.call("backyard_init", map[string]any{"dir": filepath.Join(e.cwd, "abs-new"), "site": "abs-new"}), &initOut)
	if !exists(filepath.Join(e.cwd, "abs-new", "index.html")) {
		t.Fatalf("init inside the working tree = %+v", initOut)
	}
	decode(t, e.call("backyard_init", map[string]any{"dir": ".", "site": "here"}), &initOut)
	if initOut.Dir != e.cwd {
		t.Fatalf("init . = %+v", initOut)
	}
	// Elsewhere, an explicit absolute (or ~/) path of an existing folder.
	mkdir(t, filepath.Join(home, "proj"))
	decode(t, e.call("backyard_init", map[string]any{"dir": "~/proj", "site": "proj"}), &initOut)
	if initOut.Dir != filepath.Join(home, "proj") || !exists(filepath.Join(home, "proj", "index.html")) {
		t.Fatalf("init ~/proj = %+v", initOut)
	}
	var out DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{"dir": outside, "site": "outside"}), &out)
	if e.g.SiteFiles("outside")["index.html"] != "<h1>index.html</h1>" {
		t.Fatalf("explicit absolute deploy = %+v", out)
	}
}

// TestCredentialFoldersAreRefused: an agent (confused, or prompt-injected
// by a site file it read) cannot publish or scaffold into folders holding
// credentials. The default ignore rules only apply below the deployed
// folder, so deploying ~/.ssh itself would upload the private keys in it,
// and ~/.config/backyard the CLI's own bearer and refresh tokens.
func TestCredentialFoldersAreRefused(t *testing.T) {
	e := newEnv(t, true, nil)
	root := filepath.Dir(e.cwd)
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	mkdir(t, filepath.Join(home, ".ssh"), "id_ed25519")
	mkdir(t, filepath.Join(home, ".aws"), "credentials")
	mkdir(t, filepath.Join(home, ".config", "backyard"), "config.json")
	mkdir(t, filepath.Join(home, ".config", "backyard", "cloudflare"), "backyard.json")
	mkdir(t, filepath.Join(home, "Library", "Keychains"), "login.keychain-db")
	mkdir(t, filepath.Join(root, ".hidden", "site"), "index.html")
	mkdir(t, filepath.Join(root, "xdg", "backyard"), "config.json")
	if err := os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(root, "keys")); err != nil {
		t.Fatal(err)
	}

	refused := []struct{ dir, want string }{
		{"~/.ssh", "~/.ssh"},
		{filepath.Join(home, ".ssh"), "~/.ssh"},
		{"~/.aws", "~/.aws"},
		{"~/.config", "credentials"},
		{"~/.config/backyard", "credentials"},
		{"~/.config/backyard/cloudflare", "credentials"},
		{"~/Library/Keychains", "~/Library"},
		{filepath.Join(root, "keys"), "~/.ssh"},            // a symlink to ~/.ssh
		{filepath.Join(root, ".hidden", "site"), "hidden"}, // dot folder outside the working tree
		{root, "credentials"},                              // would upload this server's config.json
	}
	for _, tool := range []string{"backyard_deploy", "backyard_init"} {
		for _, tt := range refused {
			res := e.call(tool, map[string]any{"dir": tt.dir, "site": "loot"})
			if !res.IsError || !strings.Contains(text(res), tt.want) {
				t.Errorf("%s %q: error=%v %q, want %q", tool, tt.dir, res.IsError, text(res), tt.want)
			}
		}
	}
	// $XDG_CONFIG_HOME moves the CLI's config folder; its parent would
	// upload backyard/config.json.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	for _, dir := range []string{filepath.Join(root, "xdg"), filepath.Join(root, "xdg", "backyard")} {
		if res := e.call("backyard_deploy", map[string]any{"dir": dir, "site": "loot"}); !res.IsError || !strings.Contains(text(res), "credentials") {
			t.Errorf("deploy %q: %q", dir, text(res))
		}
	}
	if e.g.SiteFiles("loot") != nil || e.g.Count(http.MethodPost, "/deploys") != 0 {
		t.Fatal("a refused deploy reached the gateway")
	}
	for _, p := range []string{filepath.Join(home, ".ssh", ".backyard.json"), filepath.Join(home, ".aws", "index.html"),
		filepath.Join(root, "keys", "CLAUDE.md")} {
		if exists(p) {
			t.Fatalf("a refused backyard_init wrote %s", p)
		}
	}

	// An agent started in the home directory cannot reach them relatively.
	e.cwd = home
	for _, dir := range []string{".ssh", ".config/backyard", "./.aws"} {
		if res := e.call("backyard_deploy", map[string]any{"dir": dir, "site": "loot"}); !res.IsError {
			t.Errorf("deploy %q from home: %q", dir, text(res))
		}
	}
	// But an agent started inside a hidden folder (a tool's worktree)
	// works on its project, and build output in dot folders of the
	// working tree is fine.
	wt := filepath.Join(home, ".cursor", "worktrees", "app")
	mkdir(t, filepath.Join(wt, ".output", "public"), "index.html")
	e.cwd = wt
	var out DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{"dir": ".output/public", "site": "app"}), &out)
	if e.g.SiteFiles("app")["index.html"] != "<h1>index.html</h1>" {
		t.Fatalf("deploy from a worktree = %+v", out)
	}
}

// TestDeployRefusesReincludedCredentials: the default rules skip a CLI
// config file in a dot folder of the deployed folder, but "!" lines in its
// .backyardignore or the project's "ignore" list can re-include dotfiles;
// the deploy then checks with those rules and refuses.
func TestDeployRefusesReincludedCredentials(t *testing.T) {
	var cfg string
	e := newEnv(t, true, func(o *Options) {
		cwd, _ := o.Getwd()
		cfg = filepath.Join(cwd, "proj", ".backyard-cli", "config.json")
		o.ConfigPath = cfg
	})
	proj := filepath.Join(e.cwd, "proj")
	mkdir(t, proj, "index.html")
	mkdir(t, filepath.Dir(cfg))
	raw, err := os.ReadFile(e.cfg) // the logged-in profile
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// Default rules: the dot folder is skipped, the deploy is fine.
	var out DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{"dir": proj, "site": "proj"}), &out)
	if files := e.g.SiteFiles("proj"); len(files) != 1 || files["index.html"] == "" {
		t.Fatalf("deployed %v", files)
	}
	for _, tc := range []struct{ file, content string }{
		{".backyardignore", "!.*\n"},
		{".backyardignore", "!.backyard-cli/\n"},
		{".backyard.json", `{"site":"proj","ignore":["!.backyard-cli/"]}`},
	} {
		_ = os.Remove(filepath.Join(proj, ".backyardignore"))
		_ = os.Remove(filepath.Join(proj, ".backyard.json"))
		if err := os.WriteFile(filepath.Join(proj, tc.file), []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		res := e.call("backyard_deploy", map[string]any{"dir": proj, "site": "proj"})
		if !res.IsError || !strings.Contains(text(res), "credentials") {
			t.Errorf("%s %q: error=%v %q", tc.file, tc.content, res.IsError, text(res))
		}
	}
	if n := e.g.Count(http.MethodPost, "/deploys"); n != 1 {
		t.Fatalf("%d deploys reached the gateway, want only the first", n)
	}
}

// TestHomeAsWorkingDirectory: an agent started in the home directory still
// cannot publish the whole home directory.
func TestHomeAsWorkingDirectory(t *testing.T) {
	e := newEnv(t, true, nil)
	mkdir(t, e.cwd, "index.html")
	t.Setenv("HOME", e.cwd)
	for _, tool := range []string{"backyard_deploy", "backyard_init"} {
		if res := e.call(tool, map[string]any{"dir": ".", "site": "home"}); !res.IsError || !strings.Contains(text(res), "home directory") {
			t.Errorf("%s .: %q", tool, text(res))
		}
	}
	mkdir(t, filepath.Join(e.cwd, "site"), "index.html")
	var out DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{"dir": "site"}), &out)
	if out.Site != "site" {
		t.Fatalf("deploy = %+v", out)
	}
}

func TestReadSiteFileRefusesBinaries(t *testing.T) {
	e := newEnv(t, true, nil)
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 20)
	e.g.AddSite("assets", map[string]string{
		"logo.PNG":     "looks like text but is an image type",
		"font.woff2":   "wOF2",
		"data.txt":     "\x01\x02\x03\x04 header",
		"utf16.txt":    "h\x00i\x00",
		"latin1.txt":   "caf\xe9",
		"bell.txt":     prose + "\a" + prose, // a stray control character is still text
		"ansi.log":     "\x1b[31mred\x1b[0m\n",
		"readme.md":    "# Hello\n",
		"icon.svg":     `<svg xmlns="http://www.w3.org/2000/svg"/>`,
		"index.html":   "<!doctype html>",
		"lib/x.min.js": "export const x=1;\f\n",
	}, "bob@acme.com")
	for _, p := range []string{"logo.PNG", "font.woff2", "data.txt", "utf16.txt", "latin1.txt"} {
		res := e.call("backyard_read_site_file", map[string]any{"site": "assets", "path": p})
		if !res.IsError || !strings.Contains(text(res), "binary") {
			t.Errorf("%s: error=%v %q", p, res.IsError, text(res))
		}
	}
	// Binary file types are refused without downloading them.
	for _, p := range []string{"/files/logo.PNG", "/files/font.woff2"} {
		if n := e.g.Count(http.MethodGet, p); n != 0 {
			t.Errorf("%s was downloaded %d times", p, n)
		}
	}
	for _, p := range []string{"bell.txt", "ansi.log", "readme.md", "icon.svg", "index.html", "lib/x.min.js"} {
		if res := e.call("backyard_read_site_file", map[string]any{"site": "assets", "path": p}); res.IsError {
			t.Errorf("%s: %q", p, text(res))
		}
	}
}

func TestLooksBinary(t *testing.T) {
	for in, want := range map[string]bool{
		"":                 false,
		"plain\ttext\r\n":  false,
		"emoji 🌱 ok":       false,
		"\x00":             true,
		"\xff\xfe":         true,
		"a\x01":            true,
		"\x7f\x7f\x7fabcd": true,
	} {
		if got := looksBinary([]byte(in)); got != want {
			t.Errorf("looksBinary(%q) = %v, want %v", in, got, want)
		}
	}
}

// echoGateway is a misbehaving gateway that repeats the caller's
// credentials back in errors, outputs and files, and hands out a presigned
// storage URL whose upload fails.
func echoGateway(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(auth, "Bearer ")
		rest := strings.TrimPrefix(r.URL.Path, clitest.GatewayPath)
		reply := func(status int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(v)
		}
		fail := func(status int, code, msg string) {
			var eb deploy.ErrorBody
			eb.Error.Code, eb.Error.Message = code, msg
			reply(status, eb)
		}
		switch {
		case rest == "/info":
			reply(200, deploy.Info{Name: "Backyard", PublicURL: srv.URL, URLMode: "path", GatewayURL: srv.URL + clitest.GatewayPath,
				Auth: deploy.AuthInfo{Mode: "oidc"}, Methods: []string{"api"}, Limits: deploy.Limits{MaxSiteBytes: 1 << 20, MaxFiles: 10, MaxFileBytes: 1 << 20}})
		case rest == "/me":
			reply(200, identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice " + tok})
		case rest == "/sites":
			fail(401, "unauthenticated", "the credentials "+auth+" are not valid here")
		case rest == "/sites/leaky/files/config.js":
			_, _ = fmt.Fprintf(w, "export const token = %q;\n", tok)
		case rest == "/sites/leaky" && r.Method == http.MethodGet:
			fail(404, "not_found", "no such site")
		case rest == "/sites/leaky/deploys" && r.Method == http.MethodPost:
			reply(200, deploy.CreateDeployResponse{DeployID: "d1", ExpiresAt: time.Now().Add(time.Hour), Unchanged: []string{},
				Uploads: []deploy.Upload{{Path: "index.html", Method: http.MethodPut,
					URL: "https://storage.example/bucket/sites/leaky/index.html?X-Amz-Credential=AKIAEXAMPLE&X-Amz-Security-Token=STS-SESSION-SECRET&X-Amz-Signature=5ec7e75160"}}})
		default:
			fail(404, "not_found", "no route "+rest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// storageDown fails every request to storage.example like a TLS problem
// (not retried), and passes everything else through.
type storageDown struct{}

func (storageDown) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == "storage.example" {
		if r.Body != nil {
			r.Body.Close()
		}
		return nil, errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestNoToolPrintsTokens: credentials never reach the agent, whatever the
// gateway sends back — in errors, structured outputs or file contents.
func TestNoToolPrintsTokens(t *testing.T) {
	const token = "alice-secret-token-0123456789"
	const refresh = "alice-refresh-token-abcdef"
	gw := echoGateway(t)
	e := newEnv(t, false, func(o *Options) { o.HTTP = &http.Client{Transport: storageDown{}} })
	f := &clientconfig.File{Current: "default"}
	f.Set("default", &clientconfig.Profile{Server: gw.URL, Token: token, RefreshToken: refresh, Login: clientconfig.LoginDevice})
	if err := f.Save(e.cfg); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(e.cwd, "leaky"), "index.html")

	calls := []struct {
		tool    string
		args    map[string]any
		isError bool
		want    string
	}{
		{"backyard_list_sites", nil, true, "credentials Bearer " + redacted},
		{"backyard_whoami", nil, false, `"name":"Alice ` + redacted + `"`},
		{"backyard_read_site_file", map[string]any{"site": "leaky", "path": "config.js"}, false, `token = "` + redacted + `"`},
		{"backyard_deploy", map[string]any{"dir": "leaky"}, true, "https://storage.example/bucket/sites/leaky/index.html?" + redacted},
	}
	for _, c := range calls {
		res := e.call(c.tool, c.args)
		raw, _ := json.Marshal(res)
		for _, secret := range []string{token, refresh, "STS-SESSION-SECRET", "5ec7e75160", "AKIAEXAMPLE"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s leaked %q: %s", c.tool, secret, raw)
			}
		}
		if res.IsError != c.isError || !strings.Contains(text(res), c.want) {
			t.Errorf("%s: error=%v %q, want %q", c.tool, res.IsError, text(res), c.want)
		}
	}
	var who WhoamiOutput
	decode(t, e.call("backyard_whoami", nil), &who)
	if who.User != "alice@acme.com" || who.Name != "Alice "+redacted {
		t.Fatalf("whoami = %+v", who)
	}
}

// TestPanicMessagesAreRedacted: a panic carrying a credential does not
// hand it to the agent.
func TestPanicMessagesAreRedacted(t *testing.T) {
	e := newEnv(t, true, func(o *Options) {
		o.Docs = func(string) (string, error) { panic("cannot use alice-token here") }
	})
	res := e.call("backyard_docs", nil)
	if !res.IsError || strings.Contains(text(res), "alice-token") || !strings.Contains(text(res), redacted) {
		t.Fatalf("panic result = %v %q", res.IsError, text(res))
	}
}

func TestSecrets(t *testing.T) {
	e := newEnv(t, true, nil)
	f, err := clientconfig.Load(e.cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.Set("work", &clientconfig.Profile{Server: "https://work.example", Token: "work-profile-token", RefreshToken: "work-refresh-token"})
	f.Set("short", &clientconfig.Profile{Server: "https://short.example", Token: "short"})
	if err := f.Save(e.cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv(clientconfig.EnvToken, "environment-token-value")
	s := &server{o: Options{ConfigPath: e.cfg, Overrides: clientconfig.Overrides{Token: "flag-token-value"}}}
	s.remember(&clientconfig.Session{Resolved: &clientconfig.Resolved{Token: "old-refreshed-token", Profile: &clientconfig.Profile{}}})
	got := s.secrets()
	for _, want := range []string{"alice-token", "work-profile-token", "work-refresh-token", "environment-token-value", "flag-token-value", "old-refreshed-token"} {
		if !strings.Contains(strings.Join(got, "\n"), want) {
			t.Errorf("secrets lack %q: %v", want, got)
		}
	}
	for _, v := range got {
		if v == "short" {
			t.Errorf("a %d-character value would be redacted from ordinary text", len(v))
		}
	}
	for i := 1; i < len(got); i++ {
		if len(got[i]) > len(got[i-1]) {
			t.Fatalf("secrets not longest first: %v", got)
		}
	}
}

func TestRedactError(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJhbGljZSJ9.c2lnbmF0dXJl"
	tests := []struct{ in, want string }{
		{"plain failure: check the name with backyard_list_sites", "plain failure: check the name with backyard_list_sites"},
		{"no Backyard server configured: run `backyard login --server https://backyard.example.com`",
			"no Backyard server configured: run `backyard login --server https://backyard.example.com`"},
		{"missing bearer credentials", "missing bearer credentials"},
		{"refresh token: response without a token", "refresh token: response without a token"},
		{"header Authorization: Bearer abcdefghijklmnop0123 rejected", "header Authorization: Bearer " + redacted + " rejected"},
		{"got " + jwt, "got " + redacted},
		{"callback ?token=s3cr3t-value&state=1", "callback ?token=" + redacted + "&state=1"},
		{`idp said {"access_token":"opaque-access-token"}`, `idp said {"access_token":"` + redacted + `"}`},
		{`Put "https://gw.example/__backyard/api/deploy/uploads/cap-ABC123/x?y=1": EOF`,
			`Put "https://gw.example/__backyard/api/deploy/uploads/` + redacted + `?` + redacted + `": EOF`},
		{"see https://u:p@host.example/a?sig=1.", "see https://host.example/a?" + redacted + "."},
		{"site at https://backyard.example.com/s/lunch/, ok", "site at https://backyard.example.com/s/lunch/, ok"},
		{"known my-known-secret-1 here", "known " + redacted + " here"},
	}
	for _, tt := range tests {
		if got := redactError(tt.in, []string{"my-known-secret-1"}); got != tt.want {
			t.Errorf("redactError(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

func TestScrubStructuredOutput(t *testing.T) {
	s := &server{o: Options{Overrides: clientconfig.Overrides{Token: `odd"token\with-quotes`}}}
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `x odd"token\with-quotes y`}}}
	res, out, err := scrub(s, res, WhoamiOutput{User: "u", Name: `odd"token\with-quotes`}, errors.New(`bad odd"token\with-quotes`))
	if out.Name != redacted || out.User != "u" {
		t.Errorf("out = %+v", out)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != "x "+redacted+" y" {
		t.Errorf("text = %q", got)
	}
	if err == nil || err.Error() != "bad "+redacted {
		t.Errorf("err = %v", err)
	}
}
