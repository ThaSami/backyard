package e2etest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/sites"
)

// TestCLIAgainstCloudflareWorker runs the real CLI (with the real init
// templates) against a running Cloudflare Workers edition (SPEC §16) on
// its free-tier defaults — no user login, deploys gated by DEPLOY_TOKEN —
// covering the OpenQuick-style flow: login with the deploy token, init,
// deploy, list, pull, redeploy, delete. For example:
//
//	cd deploy/cloudflare && npx wrangler dev --env local --var DEPLOY_TOKEN:<t> --var BACKYARD_SECRET:<s>
//	BACKYARD_TEST_WORKER_URL=http://127.0.0.1:8787 BACKYARD_TEST_WORKER_TOKEN=<t> go test ./internal/cli/e2etest/
func TestCLIAgainstCloudflareWorker(t *testing.T) {
	base := strings.TrimSuffix(os.Getenv("BACKYARD_TEST_WORKER_URL"), "/")
	token := os.Getenv("BACKYARD_TEST_WORKER_TOKEN")
	if base == "" || token == "" {
		t.Skip("BACKYARD_TEST_WORKER_URL / BACKYARD_TEST_WORKER_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	info, err := deploy.NewClient(base+deploy.EmbeddedPath, "").Info(ctx)
	cancel()
	if err != nil {
		t.Skipf("worker not reachable at %s: %v", base, err)
	}
	for _, k := range []string{clientconfig.EnvServer, clientconfig.EnvGateway, clientconfig.EnvToken, clientconfig.EnvProfile, clientconfig.EnvDeployMethod} {
		t.Setenv(k, "")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("BROWSER", "true")

	// Without a token (and without a terminal) the CLI explains which token
	// a Workers server needs.
	if !info.Auth.AnonymousDeploy && info.Auth.Mode == "none" {
		if _, errOut, code := run(t, "login", "--server", base); code == 0 || !strings.Contains(errOut, "DEPLOY_TOKEN") {
			t.Fatalf("login without token: %d %s", code, errOut)
		}
	}
	if _, errOut, code := run(t, "login", "--server", base, "--token", "not-the-deploy-token"); code == 0 || !strings.Contains(errOut, "rejected") {
		t.Fatalf("bad token: %d %s", code, errOut)
	}
	mustRun(t, "login", "--server", base, "--token", token)
	var who struct {
		User struct {
			ID    string `json:"id"`
			Admin bool   `json:"admin"`
		} `json:"user"`
		Server string `json:"server"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "--json", "whoami")), &who); err != nil || who.User.ID != "token:deploy" || who.Server != base {
		t.Fatalf("whoami = %+v (%v)", who, err)
	}

	b := make([]byte, 4)
	_, _ = rand.Read(b)
	site := "cli-e2e-" + hex.EncodeToString(b)
	t.Cleanup(func() { run(t, "sites", "delete", site, "-y") })
	dir := filepath.Join(t.TempDir(), "project")
	mustRun(t, "init", site, "--dir", dir, "--template", "app")
	if err := os.WriteFile(filepath.Join(dir, "data.json"), []byte(`{"ok":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var res struct {
		URL       string `json:"url"`
		Uploaded  int    `json:"uploaded"`
		Unchanged int    `json:"unchanged"`
		Deleted   int    `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(mustRun(t, "--json", "deploy", dir)), &res); err != nil || res.Uploaded != 4 {
		t.Fatalf("deploy: %+v %v", res, err)
	}
	if want := clientconfig.SiteURL(info, base, site); res.URL != want {
		t.Fatalf("url = %q, want %q", res.URL, want)
	}
	// The deployed page is served; agent files are not.
	if body := get(t, res.URL); !strings.Contains(body, "/__backyard/backyard.js") {
		t.Fatalf("index = %.200q", body)
	}
	if code := status(t, res.URL+"CLAUDE.md"); code != http.StatusNotFound {
		t.Fatalf("CLAUDE.md served: %d", code)
	}

	// A redeploy uploads nothing; a deleted local file is deleted remotely.
	if err := json.Unmarshal([]byte(mustRun(t, "--json", "deploy", dir)), &res); err != nil || res.Uploaded != 0 || res.Unchanged != 4 {
		t.Fatalf("redeploy: %+v %v", res, err)
	}
	if err := os.Remove(filepath.Join(dir, "data.json")); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(mustRun(t, "--json", "deploy", dir, "--dry-run")), &res); err != nil || res.Deleted != 1 {
		t.Fatalf("dry run: %+v %v", res, err)
	}

	var list struct{ Sites []sites.Meta }
	if err := json.Unmarshal([]byte(mustRun(t, "--json", "sites", "list", site)), &list); err != nil || len(list.Sites) != 1 ||
		list.Sites[0].UpdatedBy != "token:deploy" {
		t.Fatalf("sites list: %+v %v", list, err)
	}
	pulled := filepath.Join(t.TempDir(), "copy")
	mustRun(t, "pull", site, pulled)
	if b, err := os.ReadFile(filepath.Join(pulled, "data.json")); err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("pulled data.json = %q %v", b, err)
	}

	mustRun(t, "sites", "delete", site, "-y")
	if _, errOut, code := run(t, "sites", "info", site); code == 0 || !strings.Contains(errOut, "not_found") {
		t.Fatalf("after delete: %d %s", code, errOut)
	}
}

func get(t *testing.T, u string) string {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %.200s", u, resp.StatusCode, b)
	}
	return string(b)
}

func status(t *testing.T, u string) int {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
