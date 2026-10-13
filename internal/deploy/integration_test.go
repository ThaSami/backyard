package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/azure"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/blob/s3"
	"github.com/thasami/backyard/internal/config"
)

// runBackendE2E deploys, redeploys, edits and deletes a site against a real
// blob.Store implementation.
func runBackendE2E(t *testing.T, store blob.Store, wantPresigned bool) {
	t.Helper()
	ctx := context.Background()
	e := newEnv(t, withStore(store))
	dir := t.TempDir()
	tree := map[string]string{
		"index.html":           "<html><head><title>Backend E2E</title></head></html>",
		"css/site.css":         "body { color: red }",
		"img/pixel.gif":        "GIF89a\x01\x00\x01\x00\x80\x00\x00\xff\xff\xff\x00\x00\x00!",
		"deep/a/b/c/d.txt":     "deep",
		"unicode/日本語 file.txt": "ユニコード",
		"empty.txt":            "",
	}
	writeTree(t, dir, tree)
	c := e.client("alice")

	cr := e.createDeploy("alice", "e2e", map[string]string{"index.html": tree["index.html"]})
	selfHosted := strings.HasPrefix(cr.Uploads[0].URL, e.srv.URL+"/uploads/")
	if selfHosted == wantPresigned {
		t.Fatalf("upload URL %q: presigned = %v, want %v", cr.Uploads[0].URL, !selfHosted, wantPresigned)
	}

	res, err := c.Deploy(ctx, "e2e", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != len(tree) || res.Site.Title != "Backend E2E" || res.Site.FileCount != len(tree) {
		t.Fatalf("first deploy = %+v", res)
	}
	for p, want := range tree {
		if got := string(e.object("e2e", p)); got != want {
			t.Errorf("%s = %q, want %q", p, got, want)
		}
	}
	info, err := store.Stat(ctx, "sites/e2e/css/site.css")
	if err != nil {
		t.Fatal(err)
	}
	if info.ContentType != "text/css; charset=utf-8" {
		t.Errorf("css content type = %q", info.ContentType)
	}

	res, err = c.Deploy(ctx, "e2e", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 0 || res.Unchanged != len(tree) {
		t.Errorf("second deploy = %+v", res)
	}

	writeTree(t, dir, map[string]string{"css/site.css": "body { color: blue }"})
	if err := os.Remove(filepath.Join(dir, "deep", "a", "b", "c", "d.txt")); err != nil {
		t.Fatal(err)
	}
	res, err = c.Deploy(ctx, "e2e", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Deleted != 1 || res.Unchanged != len(tree)-2 {
		t.Errorf("third deploy = %+v", res)
	}
	if e.object("e2e", "deep/a/b/c/d.txt") != nil {
		t.Error("removed file still stored")
	}

	if selfHosted {
		// A corrupt upload never replaces the stored file.
		cr := e.createDeploy("alice", "e2e", map[string]string{"css/site.css": "body { color: green }"})
		resp, body := e.do("PUT", cr.Uploads[0].URL, "", "body { color: GREEN }")
		wantStatus(t, resp, body, 400, CodeBadRequest)
		if got := string(e.object("e2e", "css/site.css")); got != "body { color: blue }" {
			t.Errorf("site.css after corrupt upload = %q", got)
		}
	}

	files, err := c.SiteFiles(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(tree)-1 {
		t.Errorf("files = %+v", files)
	}
	for _, f := range files {
		if f.SHA256 == "" {
			t.Errorf("%s has no sha256 after an API deploy", f.Path)
		}
	}
	if got, err := c.ReadFile(ctx, "e2e", "unicode/日本語 file.txt", 0); err != nil || string(got) != tree["unicode/日本語 file.txt"] {
		t.Errorf("ReadFile(unicode) = %q, %v", got, err)
	}

	// Direct sync + refresh.
	if _, err := store.Put(ctx, "sites/e2e/synced.html", strings.NewReader("synced"), 6, ""); err != nil {
		t.Fatal(err)
	}
	m, err := c.Refresh(ctx, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	if m.FileCount != len(tree) || m.Title != "Backend E2E" {
		t.Errorf("refresh = %+v", m)
	}

	list, err := c.ListSites(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "e2e" {
		t.Errorf("list = %+v, %v", list, err)
	}
	if err := c.DeleteSite(ctx, "e2e"); err != nil {
		t.Fatal(err)
	}
	if res, err := store.List(ctx, "", ""); err != nil || len(res.Objects) != 0 {
		var keys []string
		for _, o := range res.Objects {
			keys = append(keys, o.Key)
		}
		t.Errorf("objects left after delete: %v (%v)", keys, err)
	}
	if !slices.Equal(e.deleted, []string{"e2e"}) {
		t.Errorf("delete hook = %v", e.deleted)
	}
}

func TestIntegrationFSStore(t *testing.T) {
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Skipf("fs store unavailable: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runBackendE2E(t, store, false)
}

func TestIntegrationS3PresignedFake(t *testing.T) {
	backend := s3mem.New()
	faker := gofakes3.New(backend, gofakes3.WithLogger(gofakes3.DiscardLog()))
	ts := httptest.NewServer(faker.Server())
	t.Cleanup(ts.Close)
	if err := backend.CreateBucket("deploy-test"); err != nil {
		t.Fatal(err)
	}
	store, err := s3.New(context.Background(), config.S3Config{
		Bucket:          "deploy-test",
		Region:          "us-east-1",
		Prefix:          "tenant/",
		Endpoint:        ts.URL,
		ForcePathStyle:  true,
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
	})
	if err != nil {
		t.Skipf("s3 store unavailable: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runBackendE2E(t, store, true)
}

func TestIntegrationAzuritePresigned(t *testing.T) {
	cs := os.Getenv("BACKYARD_TEST_AZURITE")
	if cs == "" {
		t.Skip("BACKYARD_TEST_AZURITE not set")
	}
	svc, err := service.NewClientFromConnectionString(cs, nil)
	if err != nil {
		t.Fatalf("azurite client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := svc.GetProperties(ctx, nil); err != nil {
		t.Skipf("Azurite unreachable: %v", err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	name := "deploy-" + hex.EncodeToString(b)
	if _, err := svc.CreateContainer(context.Background(), name, nil); err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = svc.DeleteContainer(ctx, name, nil)
	})
	store, err := azure.New(context.Background(), config.AzureConfig{Container: name, ConnectionString: cs})
	if err != nil {
		t.Fatalf("azure store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.PresignPut(context.Background(), "probe.txt", "text/plain", time.Minute); err != nil {
		t.Skipf("azure store cannot presign here: %v", err)
	}
	runBackendE2E(t, store, true)
}
