package deploy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites/sitestest"
)

// overlapSetup is one storage/upload configuration for the overlapping
// deploy tests.
type overlapSetup struct {
	name string
	env  func(t *testing.T) *env
}

func overlapSetups() []overlapSetup {
	return []overlapSetup{
		{"gateway uploads", func(t *testing.T) *env { return newEnv(t) }},
		{"presigned uploads", func(t *testing.T) *env { return presignEnv(t) }},
		{"presigned uploads with store digests", func(t *testing.T) *env {
			e := presignEnv(t)
			e.store.SetReportDigests(true)
			return e
		}},
		{"fs store", func(t *testing.T) *env {
			store, err := fs.New(t.TempDir())
			if err != nil {
				t.Skipf("fs store unavailable: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return newEnv(t, withStore(store))
		}},
		{"azure without presigned uploads", func(t *testing.T) *env {
			e := newEnv(t, withConfig(func(c *config.Config) { off := false; c.Deploy.Presign = &off }))
			e.store.Desc = blob.Description{Type: "azure"}
			return e
		}},
	}
}

// siteFileSHA returns the sha256 GET /sites/{site}/files reports for p.
func (e *env) siteFileSHA(site, p string) string {
	e.t.Helper()
	files, err := e.client("alice").SiteFiles(context.Background(), site)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == p {
			return f.SHA256
		}
	}
	e.t.Fatalf("%s not listed", p)
	return ""
}

// Two deploys of a site overlap (CI builds N and N+1) and index.html
// differs in size: A creates, B creates, A uploads, B uploads, A commits,
// B commits. A's commit fails, but B's upload was verified (or its size
// bound) by B's own upload URL: it must stay, so that B's commit succeeds
// and the site keeps an index.html, as with rsync.
func TestOverlappingDeploysKeepVerifiedUploads(t *testing.T) {
	for _, tc := range overlapSetups() {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.env(t)
			ctx := context.Background()
			c := e.client("alice")
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"index.html": "<title>build 0</title>", "app.js": "app"})
			if _, err := c.Deploy(ctx, "ci", dir, DeployOptions{}); err != nil {
				t.Fatal(err)
			}

			a := map[string]string{"index.html": "<title>build N</title>", "app.js": "app"}   // 22 bytes
			b := map[string]string{"index.html": "<title>build N+1</title>", "app.js": "app"} // 24 bytes
			crA := e.createDeploy("alice", "ci", a)
			crB := e.createDeploy("alice", "ci", b)
			if got := e.upload(uploadFor(t, crA, "index.html"), a["index.html"]); got != http.StatusOK {
				t.Fatalf("A's upload: %d", got)
			}
			if got := e.upload(uploadFor(t, crB, "index.html"), b["index.html"]); got != http.StatusOK {
				t.Fatalf("B's upload: %d", got)
			}

			resp, body := e.commit("alice", "ci", crA.DeployID, entriesFor(a))
			wantStatus(t, resp, body, http.StatusConflict, CodeConflict)
			msg := errCode(t, body).Error.Message
			if !strings.Contains(msg, "index.html (24 bytes in storage, expected 22)") || strings.Contains(msg, "deleted") {
				t.Errorf("A's commit: %s", msg)
			}
			if got := string(e.object("ci", "index.html")); got != b["index.html"] {
				t.Fatalf("index.html after A's failed commit = %q, want B's upload kept", got)
			}
			if strings.Contains(e.logs.String(), "deploy.reject_uploads") {
				t.Error("B's verified upload was rejected as untrusted")
			}

			resp, body = e.commit("alice", "ci", crB.DeployID, entriesFor(b))
			wantStatus(t, resp, body, http.StatusOK, "")
			if got := string(e.object("ci", "index.html")); got != b["index.html"] {
				t.Errorf("index.html after B's commit = %q", got)
			}
			if got := e.siteFileSHA("ci", "index.html"); got != sha256Hex([]byte(b["index.html"])) {
				t.Errorf("index.html sha256 = %s, want B's", got)
			}
			// B's content is now unchanged for a redeploy of B.
			writeTree(t, dir, b)
			res, err := c.Deploy(ctx, "ci", dir, DeployOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Uploaded != 0 || res.Unchanged != 2 {
				t.Errorf("redeploy of B = %+v", res)
			}
		})
	}
}

// Same overlap with files of the same size (a fixed-length build hash):
// A uploads app.js=AAAA, B uploads BBBB, A commits, B commits. The
// manifest must describe what is stored (BBBB), so that a later deploy (or
// rollback) of AAAA uploads app.js instead of calling it unchanged.
func TestOverlappingDeploysRecordStoredDigest(t *testing.T) {
	for _, tc := range overlapSetups() {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.env(t)
			ctx := context.Background()
			c := e.client("alice")
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"index.html": "<title>T</title>", "app.js": "0000"})
			if _, err := c.Deploy(ctx, "same", dir, DeployOptions{}); err != nil {
				t.Fatal(err)
			}

			a := map[string]string{"index.html": "<title>T</title>", "app.js": "AAAA"}
			b := map[string]string{"index.html": "<title>T</title>", "app.js": "BBBB"}
			crA := e.createDeploy("alice", "same", a)
			crB := e.createDeploy("alice", "same", b)
			if got := e.upload(uploadFor(t, crA, "app.js"), a["app.js"]); got != http.StatusOK {
				t.Fatalf("A's upload: %d", got)
			}
			if got := e.upload(uploadFor(t, crB, "app.js"), b["app.js"]); got != http.StatusOK {
				t.Fatalf("B's upload: %d", got)
			}

			resp, body := e.commit("alice", "same", crA.DeployID, entriesFor(a))
			wantStatus(t, resp, body, http.StatusOK, "")
			if got := e.siteFileSHA("same", "app.js"); got != sha256Hex([]byte("BBBB")) {
				t.Errorf("after A's commit app.js is listed with sha256 %s, want the stored content's (BBBB)", got)
			}
			if logs := e.logs.String(); !strings.Contains(logs, `"superseded":1`) {
				t.Errorf("A's commit did not report the superseded file:\n%s", logs)
			}
			resp, body = e.commit("alice", "same", crB.DeployID, entriesFor(b))
			wantStatus(t, resp, body, http.StatusOK, "")
			if got := e.siteFileSHA("same", "app.js"); got != sha256Hex([]byte("BBBB")) {
				t.Errorf("after B's commit app.js is listed with sha256 %s, want BBBB's", got)
			}

			// A later deploy (or rollback) of A's content uploads app.js.
			writeTree(t, dir, a)
			res, err := c.Deploy(ctx, "same", dir, DeployOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Uploaded != 1 || res.Unchanged != 1 {
				t.Errorf("redeploy of A = %+v", res)
			}
			if got := string(e.object("same", "app.js")); got != "AAAA" {
				t.Errorf("app.js = %q after redeploying A, want AAAA", got)
			}
		})
	}
}

// siteReads counts the reads of site files (not metadata) of a MemStore.
type siteReads struct {
	*sitestest.MemStore
	n atomic.Int64
}

func (s *siteReads) Get(ctx context.Context, key string) (io.ReadCloser, *blob.ObjectInfo, error) {
	if strings.HasPrefix(key, "sites/") {
		s.n.Add(1)
	}
	return s.MemStore.Get(ctx, key)
}

func (s *siteReads) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if strings.HasPrefix(key, "sites/") {
		s.n.Add(1)
	}
	return s.MemStore.GetRange(ctx, key, off, n)
}

// The commit takes the digests of uploaded files from the store when it
// verified them itself (S3 checksums) and reads the objects otherwise.
func TestCommitDigestSources(t *testing.T) {
	for _, reported := range []bool{false, true} {
		ms := sitestest.NewMemStore()
		ms.SetReportDigests(reported)
		ps := httptest.NewServer(ms.PresignHandler())
		t.Cleanup(ps.Close)
		ms.EnablePresign(ps.URL)
		store := &siteReads{MemStore: ms}
		e := newEnv(t, withStore(store))
		tree := map[string]string{"a.txt": "aaaa", "b/c.css": "body{}", "d.bin": "\x00\x01"}
		dir := t.TempDir()
		writeTree(t, dir, tree)
		var before int64
		_, err := e.client("alice").Deploy(context.Background(), "src", dir, DeployOptions{Progress: func(ev ProgressEvent) {
			if ev.Phase == "commit" {
				before = store.n.Load()
			}
		}})
		if err != nil {
			t.Fatal(err)
		}
		want := int64(len(tree))
		if reported {
			want = 0
		}
		if got := store.n.Load() - before; got != want {
			t.Errorf("store digests %v: the commit read %d site files, want %d", reported, got, want)
		}
		for p, content := range tree {
			if got := e.siteFileSHA("src", p); got != sha256Hex([]byte(content)) {
				t.Errorf("%s sha256 = %s", p, got)
			}
		}
	}
}

// uploadsBindSize: only presigned Azure SAS URLs leave the size unbound.
func TestUploadsBindSize(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		typ     string
		presign *bool
		want    bool
	}{
		{"azure", nil, false},
		{"azure", &on, false},
		{"azure", &off, true},
		{"s3", nil, true},
		{"gcs", nil, true},
		{"fs", nil, true},
		{"memory", nil, true},
	} {
		ms := sitestest.NewMemStore()
		ms.Desc = blob.Description{Type: tc.typ}
		e := newEnv(t, withStore(ms), withConfig(func(c *config.Config) { c.Deploy.Presign = tc.presign }))
		if got := e.gw.uploadsBindSize(); got != tc.want {
			t.Errorf("%s (presign %v): uploadsBindSize = %v, want %v", tc.typ, tc.presign, got, tc.want)
		}
	}
}
