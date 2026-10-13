package deploy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/sites"
)

// TestIntegrationCloudflareWorker drives the Go client (what `backyard
// deploy`, `backyard pull` and the MCP server use) against a running
// Cloudflare Workers edition (SPEC §16), e.g.
//
//	cd deploy/cloudflare && npm run dev:local
//	BACKYARD_TEST_WORKER_URL=http://127.0.0.1:8787 BACKYARD_TEST_WORKER_TOKEN=<DEPLOY_TOKEN> go test -run Worker ./internal/deploy/
//
// The Worker must run with AUTH_MODE=none; with ALLOW_ANONYMOUS_DEPLOY=true
// the playground rules (guests deploy, only tokens delete) are checked too.
func TestIntegrationCloudflareWorker(t *testing.T) {
	base := strings.TrimSuffix(os.Getenv("BACKYARD_TEST_WORKER_URL"), "/")
	token := os.Getenv("BACKYARD_TEST_WORKER_TOKEN")
	if base == "" || token == "" {
		t.Skip("BACKYARD_TEST_WORKER_URL / BACKYARD_TEST_WORKER_TOKEN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gw := base + EmbeddedPath
	anon := NewClient(gw, "")
	info, err := anon.Info(ctx)
	if err != nil {
		t.Skipf("worker not reachable at %s: %v", base, err)
	}
	if info.GatewayURL == "" || !slices.Contains(info.Methods, "api") || info.Limits.MaxFileBytes <= 0 {
		t.Fatalf("info = %+v", info)
	}
	c := NewClient(info.GatewayURL, token)
	me, err := c.Me(ctx)
	if err != nil || me.Anonymous {
		t.Fatalf("me = %+v, %v", me, err)
	}

	b := make([]byte, 4)
	_, _ = rand.Read(b)
	site := "go-client-" + hex.EncodeToString(b)
	t.Cleanup(func() { _ = c.DeleteSite(context.Background(), site) })

	dir := t.TempDir()
	tree := map[string]string{
		"index.html":               `<!doctype html><title>Worker &amp; Go</title><meta name="description" content="from the Go client">`,
		"css/site.css":             "body { color: red }",
		"img/pixel.gif":            "GIF89a\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00!",
		"docs/a b/ünï code.txt":    "unicode path",
		".well-known/security.txt": "Contact: mailto:security@example.com",
		"empty.txt":                "",
		".env":                     "SECRET=1", // ignored by default
	}
	writeTree(t, dir, tree)
	// Above FILE_INLINE_MAX (5 MiB): stored in R2 when bound, else chunked.
	big := make([]byte, 6<<20+123)
	for i := range big {
		big[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	deployed := len(tree) - 1 + 1

	if info.Auth.AnonymousDeploy {
		// Playground mode: a guest deploys (its identity must survive from
		// create to commit) but cannot delete.
		res, err := anon.Deploy(ctx, site, dir, DeployOptions{})
		if err != nil || res.Uploaded != deployed {
			t.Fatalf("anonymous deploy = %+v, %v", res, err)
		}
		apiErr(t, anon.DeleteSite(ctx, site), http.StatusUnauthorized, CodeUnauthenticated)
		if err := c.DeleteSite(ctx, site); err != nil {
			t.Fatal(err)
		}
	} else {
		// Anonymous deploys are refused with a JSON 401.
		_, err = anon.Deploy(ctx, site, dir, DeployOptions{})
		if ae := apiErr(t, err, http.StatusUnauthorized, CodeUnauthenticated); ae.Message == "" {
			t.Errorf("401 without message: %+v", ae)
		}
	}

	res, err := c.Deploy(ctx, site, dir, DeployOptions{Message: "go client test"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != deployed || res.Site.Title != "Worker & Go" || res.Site.Description != "from the Go client" ||
		res.Site.FileCount != deployed || !strings.Contains(res.URL, site) {
		t.Fatalf("first deploy = %+v", res)
	}

	// Redeploying uploads nothing; a dry run agrees.
	res, err = c.Deploy(ctx, site, dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 0 || res.Unchanged != deployed || res.Deleted != 0 {
		t.Errorf("second deploy = %+v", res)
	}
	dry, err := c.Deploy(ctx, site, dir, DeployOptions{DryRun: true})
	if err != nil || dry.Uploaded != 0 || dry.Unchanged != deployed {
		t.Errorf("dry run = %+v, %v", dry, err)
	}

	// Edit one file, remove another: one upload, one deletion.
	writeTree(t, dir, map[string]string{"css/site.css": "body { color: blue }"})
	if err := os.Remove(filepath.Join(dir, "empty.txt")); err != nil {
		t.Fatal(err)
	}
	res, err = c.Deploy(ctx, site, dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Deleted != 1 || res.Site.FileCount != deployed-1 {
		t.Errorf("third deploy = %+v", res)
	}

	files, err := c.SiteFiles(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
		if f.SHA256 == "" {
			t.Errorf("%s: no sha256 in listing", f.Path)
		}
	}
	slices.Sort(paths)
	want := []string{".well-known/security.txt", "big.bin", "css/site.css", "docs/a b/ünï code.txt", "img/pixel.gif", "index.html"}
	if !slices.Equal(paths, want) {
		t.Errorf("files = %q, want %q", paths, want)
	}
	for _, p := range []string{"docs/a b/ünï code.txt", "img/pixel.gif", "css/site.css"} {
		got, err := c.ReadFile(ctx, site, p, 0)
		wantBody := tree[p]
		if p == "css/site.css" {
			wantBody = "body { color: blue }"
		}
		if err != nil || string(got) != wantBody {
			t.Errorf("read %s = %q, %v", p, got, err)
		}
	}

	got, err := c.ReadFile(ctx, site, "big.bin", 0)
	if err != nil || !bytes.Equal(got, big) {
		t.Errorf("read big.bin: %d bytes, %v", len(got), err)
	}
	if _, err := c.ReadFile(ctx, site, "big.bin", 1<<20); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("read big.bin with a 1 MiB cap: %v", err)
	}

	m, err := c.GetSite(ctx, site)
	if err != nil || m.Name != site || m.Title != "Worker & Go" {
		t.Errorf("get site = %+v, %v", m, err)
	}
	list, err := c.ListSites(ctx)
	if err != nil || !slices.ContainsFunc(list, func(m sites.Meta) bool { return m.Name == site }) {
		t.Errorf("list sites: %d sites, %v", len(list), err)
	}

	if err := c.DeleteSite(ctx, site); err != nil {
		t.Fatal(err)
	}
	_, err = c.GetSite(ctx, site)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
		t.Errorf("get after delete: %v", err)
	}
}
