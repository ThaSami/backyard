package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
)

// start runs an App on a random loopback port and returns its base URL.
func start(t *testing.T, cfg *config.Config, opts Options) string {
	t.Helper()
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		t.Fatal(err)
	}
	opts.Listener = ln
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, cfg, opts)
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Run did not return after cancel")
		}
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return "http://" + ln.Addr().String()
}

func devConfig(t *testing.T, port string) *config.Config {
	t.Helper()
	t.Setenv("BACKYARD_CONFIG_YAML", "")
	t.Setenv("BACKYARD_CONFIG", "")
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:" + port
	cfg.Storage.FS.Root = filepath.Join(dir, "data")
	cfg.Database.SQLite.Path = filepath.Join(dir, "db", "backyard.db")
	cfg.ApplyDevDefaults()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func get(t *testing.T, url, host string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServerEndToEnd(t *testing.T) {
	port := freePort(t)
	cfg := devConfig(t, port)
	base := start(t, cfg, Options{})

	if code, body := get(t, base+"/__backyard/healthz", ""); code != 200 {
		t.Fatalf("healthz: %d %s", code, body)
	}
	if code, body := get(t, base+"/__backyard/readyz", ""); code != 200 {
		t.Fatalf("readyz: %d %s", code, body)
	}

	// Deploy a site through the embedded gateway.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("<title>Hello Yard</title><h1>hi</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := deploy.NewClient(base+deploy.EmbeddedPath, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.Deploy(ctx, "demo", src, deploy.DeployOptions{})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if res.Site.Title != "Hello Yard" {
		t.Fatalf("title = %q", res.Site.Title)
	}

	host := "demo.localhost:" + port
	if code, body := get(t, base+"/", host); code != 200 || !strings.Contains(body, "<h1>hi</h1>") {
		t.Fatalf("site: %d %q", code, body)
	}
	if code, body := get(t, base+"/__backyard/backyard.js", host); code != 200 || !strings.Contains(body, "__BACKYARD__") {
		t.Fatalf("sdk: %d", code)
	}

	// Database round trip on the site host.
	req, _ := http.NewRequest(http.MethodPost, base+"/__backyard/api/db/posts", strings.NewReader(`{"title":"Hello"}`))
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Backyard", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || doc["title"] != "Hello" || doc["id"] == "" {
		t.Fatalf("create: %d %v", resp.StatusCode, doc)
	}
	if code, body := get(t, base+"/__backyard/api/db/posts", host); code != 200 || !strings.Contains(body, `"Hello"`) {
		t.Fatalf("list: %d %s", code, body)
	}

	// Hub lists the site.
	if code, body := get(t, base+"/__backyard/api/sites", ""); code != 200 || !strings.Contains(body, `"demo"`) {
		t.Fatalf("sites: %d %s", code, body)
	}
}

func TestGatewayOnly(t *testing.T) {
	cfg := devConfig(t, freePort(t))
	base := start(t, cfg, Options{GatewayOnly: true})
	if code, _ := get(t, base+"/__backyard/healthz", ""); code != 200 {
		t.Fatalf("healthz: %d", code)
	}
	for _, p := range []string{"/info", deploy.EmbeddedPath + "/info"} {
		code, body := get(t, base+p, "")
		if code != 200 || !strings.Contains(body, `"methods"`) {
			t.Fatalf("%s: %d %s", p, code, body)
		}
	}
}
