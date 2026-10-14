package deploy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

// TestRefreshIsRateLimited checks that refreshes (which re-read every file
// of a site) draw from the deploys_per_hour bucket.
func TestRefreshIsRateLimited(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { c.RateLimit.DeploysPerHour = 2 }))
	ctx := context.Background()
	if _, err := e.store.Put(ctx, sites.SiteKey("synced", "index.html"), strings.NewReader("<title>S</title>"), -1, ""); err != nil {
		t.Fatal(err)
	}
	c := e.client("alice")
	for range 2 {
		if _, err := c.Refresh(ctx, "synced"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.Refresh(ctx, "synced")
	apiErr(t, err, http.StatusTooManyRequests, CodeRateLimited)
	// The same bucket covers deploys.
	resp, body := e.do("POST", "/sites/synced/deploys", "alice", CreateDeployRequest{Files: entriesFor(map[string]string{"a": "b"})})
	wantStatus(t, resp, body, http.StatusTooManyRequests, CodeRateLimited)
	// Buckets are per actor; refreshing a missing site is a 404.
	resp, body = e.do("POST", "/sites/missing-site/refresh", "bob", nil, "X-Backyard", "1")
	wantStatus(t, resp, body, http.StatusNotFound, CodeNotFound)
	if _, err := e.client("bob").Refresh(ctx, "synced"); err != nil {
		t.Errorf("bob's refresh: %v", err)
	}
	// Without deploy.allow_anonymous, guests cannot refresh at all.
	resp, body = e.do("POST", "/sites/synced/refresh", "", nil, "X-Backyard", "1")
	wantStatus(t, resp, body, http.StatusUnauthorized, CodeUnauthenticated)
}

// commit posts a commit for id with the given files.
func (e *env) commit(token, site, id string, files []FileEntry) (*http.Response, []byte) {
	e.t.Helper()
	return e.do("POST", "/sites/"+site+"/deploys/"+url.PathEscape(id)+"/commit", token, CommitRequest{Files: files})
}

// TestCommitKeepsVerifiedDigest checks that a commit claiming a new digest
// for a file it never uploaded does not poison the manifest: the next
// deploy of that content must upload it.
func TestCommitKeepsVerifiedDigest(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>V</title>", "app.js": "aaaa"})
	c := e.client("alice")
	if _, err := c.Deploy(ctx, "digest", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}

	// A deploy whose plan says app.js changed ("bbbb", same size) commits
	// without uploading it (e.g. a client that crashed mid-way or a
	// concurrent deploy restored the old bytes).
	next := map[string]string{"index.html": "<title>V</title>", "app.js": "bbbb"}
	cr := e.createDeploy("alice", "digest", next)
	if len(cr.Uploads) != 1 || cr.Uploads[0].Path != "app.js" {
		t.Fatalf("plan = %+v", cr)
	}
	resp, body := e.commit("alice", "digest", cr.DeployID, entriesFor(next))
	wantStatus(t, resp, body, http.StatusOK, "")

	files, err := c.SiteFiles(ctx, "digest")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "app.js" && f.SHA256 != sha256Hex([]byte("aaaa")) {
			t.Errorf("app.js listed with sha256 %s, want the stored content's", f.SHA256)
		}
	}

	writeTree(t, dir, next)
	res, err := c.Deploy(ctx, "digest", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || string(e.object("digest", "app.js")) != "bbbb" {
		t.Errorf("redeploy uploaded %d files; app.js = %q", res.Uploaded, e.object("digest", "app.js"))
	}
}

// TestUploadDetectsInPlaceEdit edits a file (same size, new bytes) between
// the plan and the uploads: the client must fail rather than commit a
// digest that does not describe what was stored.
func TestUploadDetectsInPlaceEdit(t *testing.T) {
	for _, presigned := range []bool{false, true} {
		name := "gateway uploads"
		if presigned {
			name = "presigned uploads"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if presigned {
				ps := httptest.NewServer(e.store.PresignHandler())
				defer ps.Close()
				e.store.EnablePresign(ps.URL)
			}
			ctx := context.Background()
			dir := t.TempDir()
			writeTree(t, dir, map[string]string{"index.html": "<title>E</title>", "data.txt": "original"})
			c := e.client("alice")
			_, err := c.Deploy(ctx, "edit", dir, DeployOptions{Concurrency: 1, Progress: func(ev ProgressEvent) {
				if ev.Phase == "plan" {
					writeTree(t, dir, map[string]string{"data.txt": "ORIGINAL"})
				}
			}})
			if err == nil || !strings.Contains(err.Error(), "changed since it was scanned") {
				t.Fatalf("deploy with a file edited in place: %v", err)
			}
			if ok, _ := e.reg.Exists(ctx, "edit"); ok {
				if m, err := e.reg.GetMeta(ctx, "edit"); err == nil {
					t.Errorf("deploy was committed: %+v", m)
				}
			}
			// A fresh deploy picks up the new content.
			res, err := c.Deploy(ctx, "edit", dir, DeployOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := string(e.object("edit", "data.txt")); got != "ORIGINAL" || res.Site.FileCount != 2 {
				t.Errorf("data.txt = %q; result %+v", got, res)
			}
		})
	}
}

// TestUploadTokensAreNotLogged checks that a failing upload does not write
// its capability token to the logs.
func TestUploadTokensAreNotLogged(t *testing.T) {
	e := newEnv(t)
	cr := e.createDeploy("alice", "nolog", map[string]string{"a.txt": "a"})
	e.store.FailPut = func(string) error { return errors.New("disk on fire") }
	up := cr.Uploads[0]
	resp, body := e.do("PUT", up.URL, "", "a")
	wantStatus(t, resp, body, http.StatusInternalServerError, CodeInternal)
	logs := e.logs.String()
	tok := strings.TrimPrefix(up.URL, e.srv.URL+"/uploads/")
	if !strings.Contains(logs, "disk on fire") || !strings.Contains(logs, "/uploads/[redacted]") {
		t.Errorf("failure not logged as expected:\n%s", logs)
	}
	if strings.Contains(logs, tok) {
		t.Error("upload token written to the logs")
	}
}

// TestDerivedBaseURLKeepsMountPrefix mounts a gateway without a configured
// URL under a prefix: upload URLs and /info must include the prefix.
func TestDerivedBaseURLKeepsMountPrefix(t *testing.T) {
	cfg := newTestConfig(t)
	store := newEnv(t).store
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gw, err := NewGateway(GatewayOptions{Config: cfg, Store: store, Auth: &fakeAuth{mode: auth.ModeNone, users: defaultUsers()}})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(EmbeddedPath+"/", http.StripPrefix(EmbeddedPath, gw))

	ctx := context.Background()
	c := NewClient(srv.URL+EmbeddedPath, "alice")
	inf, err := c.Info(ctx)
	if err != nil || inf.GatewayURL != srv.URL+EmbeddedPath {
		t.Fatalf("info = %+v, %v", inf, err)
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>Prefixed</title>"})
	res, err := c.Deploy(ctx, "prefixed", dir, DeployOptions{})
	if err != nil || res.Site.Title != "Prefixed" {
		t.Fatalf("deploy = %+v, %v", res, err)
	}
}

func TestMountPrefix(t *testing.T) {
	for _, tc := range []struct {
		requestURI, path, rawPath, want string
	}{
		{"/__backyard/api/deploy/info", "/info", "", "/__backyard/api/deploy"},
		{"/__backyard/api/deploy/info?x=1", "/info", "", "/__backyard/api/deploy"},
		{"/info", "/info", "", ""},
		{"/gw/sites/a%20b/files", "/sites/a b/files", "", "/gw"},
		{"/gw/sites/x/files/a%2Fb", "/sites/x/files/a/b", "/sites/x/files/a%2Fb", "/gw"},
		{"http://proxy.example/p/info", "/info", "", "/p"},
		{"", "/info", "", ""},
		{"*", "/info", "", ""},
		{"/other/path", "/info", "", ""},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RequestURI = tc.requestURI
		r.URL.Path, r.URL.RawPath = tc.path, tc.rawPath
		if got := mountPrefix(r); got != tc.want {
			t.Errorf("mountPrefix(%q, %q) = %q, want %q", tc.requestURI, tc.path, got, tc.want)
		}
	}
}

// TestHashingBody covers the client-side upload digest.
func TestHashingBody(t *testing.T) {
	var nilBody *hashingBody
	if n, sum := nilBody.digest(); n != 0 || sum != sha256Hex(nil) {
		t.Errorf("nil body digest = %d %s", n, sum)
	}
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	hb, err := openHashing(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2)
	for {
		if _, err := hb.Read(buf); err != nil {
			break
		}
	}
	if err := hb.Close(); err != nil {
		t.Fatal(err)
	}
	if n, sum := hb.digest(); n != 5 || sum != sha256Hex([]byte("hello")) {
		t.Errorf("digest = %d %s", n, sum)
	}
	if _, err := openHashing(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("opening a missing file succeeded")
	}
}

// TestFileDirectoryTransitionsOnFS deploys a layout where a file becomes a
// directory and back on the fs store, which cannot hold both at once.
func TestFileDirectoryTransitionsOnFS(t *testing.T) {
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Skipf("fs store unavailable: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e := newEnv(t, withStore(store))
	ctx := context.Background()
	c := e.client("alice")
	steps := []map[string]string{
		{"index.html": "<title>T</title>", "docs": "docs as a file", "keep/a.txt": "a"},
		{"index.html": "<title>T</title>", "docs/index.html": "docs as a directory", "docs/deep/x.txt": "x", "keep/a.txt": "a"},
		{"index.html": "<title>T</title>", "docs": "a file again", "keep/a.txt": "a"},
	}
	for i, tree := range steps {
		dir := t.TempDir()
		writeTree(t, dir, tree)
		res, err := c.Deploy(ctx, "transit", dir, DeployOptions{})
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		got := e.siteKeys("transit")
		if len(got) != len(tree) || res.Site.FileCount != len(tree) {
			t.Errorf("step %d: stored %q", i, got)
		}
		for p, want := range tree {
			if b := string(e.object("transit", p)); b != want {
				t.Errorf("step %d: %s = %q, want %q", i, p, b, want)
			}
		}
	}
}

func TestTreeConflicts(t *testing.T) {
	stored := func(keys ...string) []blob.ObjectInfo {
		var out []blob.ObjectInfo
		for _, k := range keys {
			out = append(out, blob.ObjectInfo{Key: k})
		}
		return out
	}
	files := func(paths ...string) []FileEntry {
		var out []FileEntry
		for _, p := range paths {
			out = append(out, FileEntry{Path: p})
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		files   []FileEntry
		current []blob.ObjectInfo
		want    []string
	}{
		{"none", files("a", "b/c"), stored("a", "b/c", "old.txt"), nil},
		{"file becomes dir", files("docs/index.html"), stored("docs", "other"), []string{"docs"}},
		{"deep file becomes dir", files("a/b/c/d"), stored("a/b"), []string{"a/b"}},
		{"dir becomes file", files("docs"), stored("docs/index.html", "docs/x/y", "docsx"), []string{"docs/index.html", "docs/x/y"}},
		{"prefix is not a dir", files("doc"), stored("docs/a"), nil},
		{"same path kept", files("a/b"), stored("a/b"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := treeConflicts(tc.files, tc.current); !slices.Equal(got, tc.want) {
				t.Errorf("treeConflicts = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTimedOutRequestsAreNotRepeated checks that a request whose response
// does not arrive in time is sent once: the gateway may still be working on
// it (a refresh hashing a large site), so repeating it would only add load.
func TestTimedOutRequestsAreNotRepeated(t *testing.T) {
	var posts, gets atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		} else {
			gets.Add(1)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)
	c := NewClient(srv.URL, "tok")
	tr := c.HTTP.Transport.(*http.Transport)
	if tr.ResponseHeaderTimeout < longRequestTimeout {
		t.Errorf("ResponseHeaderTimeout %v is shorter than the gateway's %v", tr.ResponseHeaderTimeout, longRequestTimeout)
	}
	tr.ResponseHeaderTimeout = 50 * time.Millisecond

	if _, err := c.Refresh(context.Background(), "slow"); err == nil {
		t.Error("refresh succeeded")
	}
	if _, err := c.Info(context.Background()); err == nil {
		t.Error("info succeeded")
	}
	if p, g := posts.Load(), gets.Load(); p != 1 || g != 1 {
		t.Errorf("sent %d POSTs and %d GETs, want 1 each", p, g)
	}
}

// TestCorruptSiteMetadata checks that an unreadable meta document fails
// closed in ownership mode (only admins may act, and their deploy repairs
// it) and does not block anyone when ownership is off.
func TestCorruptSiteMetadata(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>Fixed</title>"})
	corrupt := func(e *env) {
		t.Helper()
		if _, err := e.store.Put(ctx, sites.MetaKey("bad"), strings.NewReader("{"), -1, "application/json"); err != nil {
			t.Fatal(err)
		}
		if _, err := e.store.Put(ctx, sites.SiteKey("bad", "index.html"), strings.NewReader("old"), -1, ""); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("ownership", func(t *testing.T) {
		e := newEnv(t, withConfig(func(c *config.Config) { c.Sites.Ownership = "owner" }))
		corrupt(e)
		_, err := e.client("alice").Deploy(ctx, "bad", dir, DeployOptions{})
		if ae := apiErr(t, err, http.StatusForbidden, CodeForbidden); !strings.Contains(ae.Message, "unreadable") {
			t.Errorf("message = %q", ae.Message)
		}
		apiErr(t, e.client("alice").DeleteSite(ctx, "bad"), http.StatusForbidden, CodeForbidden)
		// Listing still works.
		if list, err := e.client("alice").ListSites(ctx); err != nil || len(list) != 1 {
			t.Errorf("list = %+v, %v", list, err)
		}
		// An admin's deploy rewrites the document (and becomes owner).
		res, err := e.client("admin").Deploy(ctx, "bad", dir, DeployOptions{})
		if err != nil || res.Site.Title != "Fixed" || len(res.Site.Owners) != 1 {
			t.Fatalf("admin deploy = %+v, %v", res, err)
		}
		if m, err := e.reg.GetMeta(ctx, "bad"); err != nil || m.Title != "Fixed" {
			t.Errorf("meta after repair = %+v, %v", m, err)
		}
	})
	t.Run("no ownership", func(t *testing.T) {
		e := newEnv(t)
		corrupt(e)
		if res, err := e.client("alice").Deploy(ctx, "bad", dir, DeployOptions{}); err != nil || res.Site.Title != "Fixed" {
			t.Fatalf("deploy = %+v, %v", res, err)
		}
	})
}
