// Package e2etest runs the real `backyard` command tree against the real
// deploy gateway (filesystem storage, static-token auth) to verify the CLI
// and the gateway agree on the wire protocol.
package e2etest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/cli"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/mcpserver"
	"github.com/thasami/backyard/internal/sites"
)

const ciToken = "e2e-static-token-0123456789"

// startGateway runs the real gateway mounted like the embedded one.
func startGateway(t *testing.T) (url, storeRoot string) {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256([]byte(ciToken))
	raw := "server:\n  base_domain: backyard.test\n  url_mode: path\n  secret: e2e-secret-0123456789abcdef0123456789\n" +
		"auth:\n  mode: none\n  tokens:\n    - name: ci\n      sha256: " + hex.EncodeToString(sum[:]) + "\n"
	cfg, err := config.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	storeRoot = t.TempDir()
	cfg.Storage.Type = "fs"
	cfg.Storage.FS.Root = storeRoot
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	store, err := fs.New(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	authn, err := auth.NewBearer(ctx, cfg, auth.Options{Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gw, err := deploy.NewGateway(deploy.GatewayOptions{
		Config: cfg, Store: store, Registry: sites.NewRegistry(store, cfg), Auth: authn,
		BaseURL: srv.URL + deploy.EmbeddedPath, Logger: log, Version: "e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(deploy.EmbeddedPath+"/", http.StripPrefix(deploy.EmbeddedPath, gw))
	return srv.URL, storeRoot
}

// run executes the real CLI with isolated config.
func run(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	streams := cli.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errOut}
	root := cli.NewRootCommand(streams)
	root.SetArgs(args)
	if err := root.ExecuteContext(context.Background()); err != nil {
		return out.String(), errOut.String() + err.Error(), 1
	}
	return out.String(), errOut.String(), 0
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, code := run(t, args...)
	if code != 0 {
		t.Fatalf("backyard %s: exit %d\n%s\n%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

func TestCLIAgainstRealGateway(t *testing.T) {
	for _, k := range []string{clientconfig.EnvServer, clientconfig.EnvGateway, clientconfig.EnvToken, clientconfig.EnvProfile, clientconfig.EnvDeployMethod} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("BROWSER", "true") // never launch a real browser

	server, storeRoot := startGateway(t)
	// An unauthenticated request is refused and explained.
	if _, errOut, code := run(t, "login", "--server", server, "--token", "wrong"); code == 0 || !strings.Contains(errOut, "rejected") {
		t.Fatalf("bad token: %d %s", code, errOut)
	}
	mustRun(t, "login", "--server", server, "--token", ciToken)
	cfgPath := filepath.Join(home, ".config", "backyard", "config.json")
	f, err := clientconfig.Load(cfgPath)
	if err != nil || f.Profile("default") == nil || f.Profile("default").User.ID != "token:ci" {
		t.Fatalf("profile: %+v %v", f, err)
	}

	site := filepath.Join(t.TempDir(), "e2e-site")
	files := map[string]string{
		"index.html":               "<html><head><title>E2E Site</title></head><body>hi</body></html>",
		"js/app.js":                "console.log('e2e')",
		".well-known/security.txt": "contact: x",
		".git/HEAD":                "ref",
		"CLAUDE.md":                "docs",
	}
	for p, c := range files {
		full := filepath.Join(site, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out := mustRun(t, "deploy", site)
	if !strings.Contains(out, "/s/e2e-site/") {
		t.Fatalf("deploy stdout = %q", out)
	}
	for _, p := range []string{"index.html", "js/app.js", ".well-known/security.txt"} {
		if _, err := os.Stat(filepath.Join(storeRoot, "sites", "e2e-site", filepath.FromSlash(p))); err != nil {
			t.Errorf("not stored: %s (%v)", p, err)
		}
	}
	for _, p := range []string{".git/HEAD", "CLAUDE.md"} {
		if _, err := os.Stat(filepath.Join(storeRoot, "sites", "e2e-site", filepath.FromSlash(p))); err == nil {
			t.Errorf("ignored file stored: %s", p)
		}
	}

	// Redeploy with one change.
	if err := os.WriteFile(filepath.Join(site, "js", "app.js"), []byte("console.log(2)"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = mustRun(t, "deploy", site, "--json")
	var res struct {
		Uploaded, Unchanged int
		URL                 string
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || res.Uploaded != 1 || res.Unchanged != 2 {
		t.Fatalf("redeploy: %v %s", err, out)
	}

	out = mustRun(t, "sites", "list", "--json")
	var list struct{ Sites []sites.Meta }
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Sites) != 1 || list.Sites[0].Title != "E2E Site" {
		t.Fatalf("sites list: %v %s", err, out)
	}

	pulled := filepath.Join(t.TempDir(), "copy")
	mustRun(t, "pull", "e2e-site", pulled)
	if b, err := os.ReadFile(filepath.Join(pulled, "js", "app.js")); err != nil || string(b) != "console.log(2)" {
		t.Fatalf("pulled app.js = %q %v", b, err)
	}

	// The MCP server sees the same site.
	srv := mcpserver.New(mcpserver.Options{ConfigPath: cfgPath})
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "backyard_read_site_file",
		Arguments: map[string]any{"site": "e2e-site", "path": "js/app.js"}})
	if err != nil || r.IsError || r.Content[0].(*mcp.TextContent).Text != "console.log(2)" {
		t.Fatalf("mcp read: %v %+v", err, r)
	}

	mustRun(t, "sites", "delete", "e2e-site", "-y")
	out = mustRun(t, "sites", "list", "--json")
	if err := json.Unmarshal([]byte(out), &list); err != nil || len(list.Sites) != 0 {
		t.Fatalf("after delete: %s", out)
	}
}
