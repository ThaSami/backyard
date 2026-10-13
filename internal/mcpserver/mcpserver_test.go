package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/templates"
)

var alice = identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice Smith"}

// env is a configured MCP server connected to a client in memory.
type env struct {
	t   *testing.T
	cs  *mcp.ClientSession
	g   *clitest.Gateway
	cfg string
	cwd string
}

func clearEnv(t *testing.T) {
	for _, k := range []string{clientconfig.EnvServer, clientconfig.EnvGateway, clientconfig.EnvToken,
		clientconfig.EnvProfile, clientconfig.EnvDeployMethod} {
		t.Setenv(k, "")
	}
}

// newEnv starts a fake gateway, writes a logged-in profile (unless
// loggedIn is false) and connects a client.
func newEnv(t *testing.T, loggedIn bool, mutate func(*Options)) *env {
	t.Helper()
	clearEnv(t)
	root := t.TempDir()
	e := &env{t: t, g: clitest.NewGateway(t), cfg: filepath.Join(root, "config.json"), cwd: filepath.Join(root, "work")}
	if err := os.MkdirAll(e.cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	e.g.Tokens["alice-token"] = alice
	if loggedIn {
		f := &clientconfig.File{Current: "default"}
		f.Set("default", &clientconfig.Profile{Server: e.g.URL, Token: "alice-token", Login: clientconfig.LoginBrowser})
		if err := f.Save(e.cfg); err != nil {
			t.Fatal(err)
		}
	}
	o := Options{
		ConfigPath: e.cfg,
		Getwd:      func() (string, error) { return e.cwd, nil },
		Render:     fakeRender,
		Docs: func(topic string) (string, error) {
			switch topic {
			case "", "skill":
				return "# Backyard skill", nil
			case "api":
				return "# API reference\nbackyard.db.collection(...)", nil
			case "deploy":
				return "", errors.New("deploy docs missing")
			}
			return "", nil
		},
	}
	if mutate != nil {
		mutate(&o)
	}
	srv := New(o)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1.0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cs.Close()
		ss.Close()
	})
	e.cs = cs
	return e
}

func fakeRender(o templates.InitOptions) (map[string][]byte, error) {
	files := map[string][]byte{".backyard.json": []byte(`{"site":"` + o.Site + `"}`), "CLAUDE.md": []byte("# " + o.Server)}
	if !o.SkipIndex {
		files["index.html"] = []byte("<title>" + o.Site + "</title>")
	}
	return files, nil
}

// call invokes a tool and returns the result.
func (e *env) call(name string, args map[string]any) *mcp.CallToolResult {
	e.t.Helper()
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: protocol error %v", name, err)
	}
	return res
}

// text concatenates the text content of a result.
func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// decode unmarshals a result's structured content into v.
func decode(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if res.IsError {
		t.Fatalf("tool error: %s", text(res))
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
}

func TestListTools(t *testing.T) {
	e := newEnv(t, true, nil)
	res, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if len(tool.Description) < 40 {
			t.Errorf("%s: description too short: %q", tool.Name, tool.Description)
		}
		if tool.Name == "backyard_docs" {
			b, _ := json.Marshal(tool.InputSchema)
			if !strings.Contains(string(b), `"enum":["skill","api","recipes","deploy"]`) {
				t.Errorf("docs schema lacks the topic enum: %s", b)
			}
		}
		if tool.Name == "backyard_deploy" {
			// dir is optional: without it the current project is published.
			b, _ := json.Marshal(tool.InputSchema)
			if strings.Contains(string(b), `"required"`) {
				t.Errorf("deploy schema requires an argument: %s", b)
			}
		}
		if tool.Name == "backyard_read_site_file" {
			b, _ := json.Marshal(tool.InputSchema)
			if !strings.Contains(string(b), `"required":["site","path"]`) {
				t.Errorf("read schema: %s", b)
			}
		}
	}
	slices.Sort(names)
	want := []string{"backyard_deploy", "backyard_docs", "backyard_init", "backyard_list_sites", "backyard_open_url",
		"backyard_read_site_file", "backyard_site_files", "backyard_whoami"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}
	if ir := e.cs.InitializeResult(); ir == nil || !strings.Contains(ir.Instructions, "backyard_docs") || ir.ServerInfo.Name != "backyard" {
		t.Fatalf("initialize result = %+v", ir)
	}
}

func TestDocs(t *testing.T) {
	e := newEnv(t, false, nil)
	if got := text(e.call("backyard_docs", nil)); got != "# Backyard skill" {
		t.Fatalf("default docs = %q", got)
	}
	if got := text(e.call("backyard_docs", map[string]any{"topic": "api"})); !strings.HasPrefix(got, "# API reference") {
		t.Fatalf("api docs = %q", got)
	}
	tests := []struct {
		topic string
		want  string
	}{
		{"nope", "validating"}, // rejected by the enum schema
		{"deploy", "deploy docs missing"},
		{"recipes", "empty"},
	}
	for _, tt := range tests {
		res := e.call("backyard_docs", map[string]any{"topic": tt.topic})
		if !res.IsError || !strings.Contains(text(res), tt.want) {
			t.Errorf("topic %q: error=%v %q", tt.topic, res.IsError, text(res))
		}
	}
}

func TestListSitesAndFiles(t *testing.T) {
	e := newEnv(t, true, nil)
	e.g.AddSite("lunch-vote", map[string]string{"index.html": "<h1>vote</h1>", "lib/chart.js": "export const chart = 1", "logo.png": "\x89PNG\x00"}, "bob@acme.com")
	e.g.AddSite("dashboard", map[string]string{"index.html": "dash"}, "alice@acme.com")

	var list ListSitesOutput
	decode(t, e.call("backyard_list_sites", nil), &list)
	if list.Total != 2 || len(list.Sites) != 2 || list.Truncated {
		t.Fatalf("list = %+v", list)
	}
	decode(t, e.call("backyard_list_sites", map[string]any{"query": "VOTE"}), &list)
	if list.Total != 1 || list.Sites[0].Name != "lunch-vote" || list.Sites[0].URL != e.g.SiteURL("lunch-vote") ||
		list.Sites[0].UpdatedBy != "bob@acme.com" || list.Sites[0].FileCount != 3 || list.Sites[0].UpdatedAt == "" {
		t.Fatalf("filtered = %+v", list)
	}

	var files SiteFilesOutput
	decode(t, e.call("backyard_site_files", map[string]any{"site": "lunch-vote"}), &files)
	if files.Total != 3 || files.Files[0].Path != "index.html" || files.Files[1].Path != "lib/chart.js" {
		t.Fatalf("files = %+v", files)
	}
	res := e.call("backyard_read_site_file", map[string]any{"site": "lunch-vote", "path": "/lib/chart.js"})
	if res.IsError || text(res) != "export const chart = 1" {
		t.Fatalf("read = %v %q", res.IsError, text(res))
	}
	errorsWanted := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"backyard_read_site_file", map[string]any{"site": "lunch-vote", "path": "logo.png"}, "binary"},
		{"backyard_read_site_file", map[string]any{"site": "lunch-vote", "path": "../etc/passwd"}, "invalid path"},
		{"backyard_read_site_file", map[string]any{"site": "lunch-vote", "path": "missing.js"}, "not_found"},
		{"backyard_read_site_file", map[string]any{"site": "Bad Name", "path": "x"}, "invalid site name"},
		{"backyard_site_files", map[string]any{"site": "nope"}, "backyard_list_sites"},
		{"backyard_read_site_file", map[string]any{"site": "x"}, "validating"},
	}
	for _, tt := range errorsWanted {
		res := e.call(tt.tool, tt.args)
		if !res.IsError || !strings.Contains(text(res), tt.want) {
			t.Errorf("%s %v: error=%v %q", tt.tool, tt.args, res.IsError, text(res))
		}
	}
	// Oversized text file.
	e.g.AddSite("big", map[string]string{"big.txt": strings.Repeat("a", MaxReadBytes+1)}, "x")
	if res := e.call("backyard_read_site_file", map[string]any{"site": "big", "path": "big.txt"}); !res.IsError || !strings.Contains(text(res), "larger than") {
		t.Fatalf("big file: %q", text(res))
	}
}

func TestInitAndDeploy(t *testing.T) {
	e := newEnv(t, true, nil)
	var initOut InitOutput
	decode(t, e.call("backyard_init", map[string]any{"dir": "poll", "site": "team-poll"}), &initOut)
	dir := filepath.Join(e.cwd, "poll")
	if initOut.Dir != dir || initOut.Site != "team-poll" || len(initOut.Created) != 3 || !strings.Contains(initOut.NextSteps, "backyard_deploy") {
		t.Fatalf("init = %+v", initOut)
	}
	claude, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if string(claude) != "# "+e.g.URL {
		t.Fatalf("server not passed: %s", claude)
	}
	decode(t, e.call("backyard_init", map[string]any{"dir": dir, "site": "team-poll"}), &initOut)
	if len(initOut.Created) != 0 || !slices.Contains(initOut.Skipped, "index.html") {
		t.Fatalf("second init = %+v", initOut)
	}
	if res := e.call("backyard_init", map[string]any{"dir": "x", "site": "admin"}); !res.IsError || !strings.Contains(text(res), "reserved") {
		t.Fatalf("reserved: %q", text(res))
	}

	if err := os.WriteFile(filepath.Join(dir, "app.js"), []byte("console.log('poll')"), 0o644); err != nil {
		t.Fatal(err)
	}
	var dry DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{"dir": "poll", "dry_run": true}), &dry)
	if !dry.DryRun || dry.Uploaded != 2 || e.g.SiteFiles("team-poll") != nil {
		t.Fatalf("dry run = %+v", dry)
	}
	res := e.call("backyard_deploy", map[string]any{"dir": "poll", "message": "first"})
	var out DeployOutput
	decode(t, res, &out)
	if out.Site != "team-poll" || out.URL != e.g.SiteURL("team-poll") || out.Uploaded != 2 || !strings.Contains(text(res), out.URL) {
		t.Fatalf("deploy = %+v text %q", out, text(res))
	}
	files := e.g.SiteFiles("team-poll")
	if len(files) != 2 || files["app.js"] != "console.log('poll')" {
		t.Fatalf("deployed files = %v", files)
	}
	// Explicit site name.
	decode(t, e.call("backyard_deploy", map[string]any{"dir": dir, "site": "poll-copy"}), &out)
	if e.g.SiteFiles("poll-copy") == nil {
		t.Fatal("site override ignored")
	}
	for _, tt := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"dir": "missing"}, "not a directory"},
		{map[string]any{"dir": dir, "site": "Not Valid"}, "invalid site name"},
	} {
		if res := e.call("backyard_deploy", tt.args); !res.IsError || !strings.Contains(text(res), tt.want) {
			t.Errorf("%v: %q", tt.args, text(res))
		}
	}
}

// TestDeployUsesTheProjectsDir: like `backyard deploy`, backyard_deploy
// without a dir publishes the folder .backyard.json names ("dir": "dist"),
// and naming such a project's root is refused instead of publishing the
// sources (and deleting the site's real files).
func TestDeployUsesTheProjectsDir(t *testing.T) {
	e := newEnv(t, true, nil)
	proj := filepath.Join(e.cwd, "app")
	mkdir(t, filepath.Join(proj, "src"), "main.ts")
	mkdir(t, filepath.Join(proj, "dist"), "index.html")
	mkdir(t, proj, "package.json")
	if err := os.WriteFile(filepath.Join(proj, ".backyard.json"), []byte(`{"site":"app","dir":"dist"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"app", proj, "app/"} {
		res := e.call("backyard_deploy", map[string]any{"dir": dir})
		if !res.IsError || !strings.Contains(text(res), `"dist"`) || !strings.Contains(text(res), "without") {
			t.Fatalf("deploy %q: error=%v %q", dir, res.IsError, text(res))
		}
	}
	if e.g.SiteFiles("app") != nil {
		t.Fatalf("project root published: %v", e.g.SiteFiles("app"))
	}
	// Without dir, from inside the project (or a subfolder of it).
	for _, cwd := range []string{proj, filepath.Join(proj, "src")} {
		e.cwd = cwd
		var out DeployOutput
		decode(t, e.call("backyard_deploy", map[string]any{"dry_run": true}), &out)
		if out.Dir != filepath.Join(proj, "dist") || out.Site != "app" || out.Files != 1 {
			t.Fatalf("dry run from %s = %+v", cwd, out)
		}
	}
	var out DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{}), &out)
	if files := e.g.SiteFiles("app"); len(files) != 1 || files["index.html"] == "" || out.Dir != filepath.Join(proj, "dist") {
		t.Fatalf("deploy = %+v, files %v", out, files)
	}
	// Naming the build folder explicitly works too (site name from the project).
	e.cwd = filepath.Dir(proj)
	decode(t, e.call("backyard_deploy", map[string]any{"dir": "app/dist", "dry_run": true}), &out)
	if out.Site != "app" || out.Dir != filepath.Join(proj, "dist") {
		t.Fatalf("explicit dist = %+v", out)
	}
	// A project dir leading out of the project is refused.
	if err := os.WriteFile(filepath.Join(proj, ".backyard.json"), []byte(`{"site":"app","dir":"../.."}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e.cwd = proj
	if res := e.call("backyard_deploy", map[string]any{}); !res.IsError || !strings.Contains(text(res), "inside") {
		t.Fatalf("escaping project dir: %q", text(res))
	}
	// Without a project file the working directory is published.
	plain := filepath.Join(filepath.Dir(proj), "plain")
	mkdir(t, plain, "index.html")
	e.cwd = plain
	decode(t, e.call("backyard_deploy", map[string]any{"dry_run": true}), &out)
	if out.Dir != plain || out.Site != "plain" {
		t.Fatalf("plain = %+v", out)
	}
}

func TestDeployRefusesToReplaceSomeoneElsesSite(t *testing.T) {
	e := newEnv(t, true, nil)
	dir := filepath.Join(e.cwd, "board")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.g.AddSite("board", map[string]string{"index.html": "bob's"}, "bob@acme.com")
	res := e.call("backyard_deploy", map[string]any{"dir": dir})
	if !res.IsError || !strings.Contains(text(res), "bob@acme.com") || !strings.Contains(text(res), `"overwrite": true`) {
		t.Fatalf("expected a refusal, got %v %q", res.IsError, text(res))
	}
	if e.g.SiteFiles("board")["index.html"] != "bob's" {
		t.Fatal("site was replaced without overwrite")
	}
	// A dry run is always allowed; overwrite replaces the site.
	var out DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{"dir": dir, "dry_run": true}), &out)
	decode(t, e.call("backyard_deploy", map[string]any{"dir": dir, "overwrite": true}), &out)
	if e.g.SiteFiles("board")["index.html"] != "mine" {
		t.Fatal("overwrite did not deploy")
	}
	// Now alice deployed it last: redeploys need no overwrite.
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("mine v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	decode(t, e.call("backyard_deploy", map[string]any{"dir": dir}), &out)
	if e.g.SiteFiles("board")["index.html"] != "mine v2" {
		t.Fatal("redeploy failed")
	}
}

func TestDeployNotLoggedIn(t *testing.T) {
	e := newEnv(t, false, nil)
	dir := filepath.Join(e.cwd, "s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o644)
	res := e.call("backyard_deploy", map[string]any{"dir": dir})
	if !res.IsError || !strings.Contains(text(res), "backyard login") {
		t.Fatalf("not configured: %q", text(res))
	}
	// Server known but no token and no anonymous deploys.
	f := &clientconfig.File{Current: "default"}
	f.Set("default", &clientconfig.Profile{Server: e.g.URL})
	if err := f.Save(e.cfg); err != nil {
		t.Fatal(err)
	}
	res = e.call("backyard_deploy", map[string]any{"dir": dir})
	if !res.IsError || !strings.Contains(text(res), "not logged in") {
		t.Fatalf("no token: %q", text(res))
	}
	// Anonymous servers accept deploys without a token, also over a site
	// a guest deployed before.
	e.g.Anonymous = true
	e.g.Info.Auth = deploy.AuthInfo{Mode: "none", AnonymousDeploy: true}
	e.g.AddSite("s", map[string]string{"old.html": "x"}, "anon:another-guest")
	var out DeployOutput
	decode(t, e.call("backyard_deploy", map[string]any{"dir": dir}), &out)
	if out.Site != "s" {
		t.Fatalf("anonymous deploy = %+v", out)
	}
	// Revoked token: actionable message.
	f.Set("default", &clientconfig.Profile{Server: e.g.URL, Token: "revoked"})
	_ = f.Save(e.cfg)
	if res := e.call("backyard_list_sites", nil); !res.IsError || !strings.Contains(text(res), "rejected") {
		t.Fatalf("revoked: %q", text(res))
	}
}

func TestWhoamiAndOpenURL(t *testing.T) {
	e := newEnv(t, true, nil)
	var who WhoamiOutput
	decode(t, e.call("backyard_whoami", nil), &who)
	if who.User != alice.ID || who.Name != "Alice Smith" || who.Server != e.g.URL || who.Profile != "default" || who.Login != "browser" {
		t.Fatalf("whoami = %+v", who)
	}
	var u URLOutput
	decode(t, e.call("backyard_open_url", map[string]any{"site": "lunch"}), &u)
	if u.URL != e.g.SiteURL("lunch") {
		t.Fatalf("site url = %q", u.URL)
	}
	decode(t, e.call("backyard_open_url", nil), &u)
	if u.URL != e.g.URL+"/" {
		t.Fatalf("hub url = %q", u.URL)
	}
	if res := e.call("backyard_open_url", map[string]any{"site": "BAD"}); !res.IsError {
		t.Fatal("invalid site accepted")
	}
}

func TestPanicsBecomeToolErrors(t *testing.T) {
	e := newEnv(t, true, func(o *Options) {
		o.Render = func(templates.InitOptions) (map[string][]byte, error) { panic("templates: not implemented") }
	})
	res := e.call("backyard_init", map[string]any{"dir": "x", "site": "ok"})
	if !res.IsError || !strings.Contains(text(res), "internal error") {
		t.Fatalf("panic result = %v %q", res.IsError, text(res))
	}
	// The server keeps working.
	if got := text(e.call("backyard_docs", nil)); got != "# Backyard skill" {
		t.Fatalf("after panic: %q", got)
	}
}
