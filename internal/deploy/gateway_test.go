package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

const indexHTML = `<!doctype html><html><head><title>Lunch &amp; Vote</title>
<meta name="description" content="Vote on today's lunch"></head><body>hi</body></html>`

func siteTree() map[string]string {
	return map[string]string{
		"index.html":                indexHTML,
		"app.js":                    "console.log('hello')",
		"assets/logo.png":           "\x89PNG\r\n\x1a\n\x00\x00binary\xff\xfe",
		"docs/guide.html":           "<h1>guide</h1>",
		".well-known/security.txt":  "Contact: sec@acme.com",
		".git/config":               "[core]",
		".env":                      "SECRET=1",
		"node_modules/lib/index.js": "module.exports = 1",
		"CLAUDE.md":                 "agent notes",
		"AGENTS.md":                 "agent notes",
		".backyard.json":            `{"site":"lunch"}`,
		"sub/.DS_Store":             "junk",
	}
}

var deployedPaths = []string{".well-known/security.txt", "app.js", "assets/logo.png", "docs/guide.html", "index.html"}

func TestDeployEndToEnd(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeTree(t, dir, siteTree())
	c := e.client("alice")
	ctx := context.Background()

	var mu sync.Mutex
	phases := map[string]int{}
	res, err := c.Deploy(ctx, "lunch", dir, DeployOptions{Message: "first", Progress: func(ev ProgressEvent) {
		mu.Lock()
		phases[ev.Phase]++
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.siteKeys("lunch"); !slices.Equal(got, deployedPaths) {
		t.Fatalf("stored files = %v, want %v", got, deployedPaths)
	}
	for p, content := range siteTree() {
		if slices.Contains(deployedPaths, p) && string(e.object("lunch", p)) != content {
			t.Errorf("content of %s differs", p)
		}
	}
	if res.Uploaded != 5 || res.Unchanged != 0 || res.Deleted != 0 {
		t.Errorf("result counts = %+v", res)
	}
	if res.URL != "https://lunch.backyard.test/" || res.Site.URL != res.URL {
		t.Errorf("URL = %q / %q", res.URL, res.Site.URL)
	}
	if len(res.Files) != 5 {
		t.Errorf("result files = %d", len(res.Files))
	}
	for _, ph := range []string{"scan", "plan", "upload", "commit", "done"} {
		if phases[ph] == 0 {
			t.Errorf("no %q progress event (got %v)", ph, phases)
		}
	}
	if phases["upload"] != 5 {
		t.Errorf("upload events = %d, want 5", phases["upload"])
	}

	m, err := c.GetSite(ctx, "lunch")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Lunch & Vote" || m.Description != "Vote on today's lunch" {
		t.Errorf("title/description = %q / %q", m.Title, m.Description)
	}
	var total int64
	for _, f := range res.Files {
		total += f.Size
	}
	if m.FileCount != 5 || m.Bytes != total {
		t.Errorf("meta counts = %d / %d, want 5 / %d", m.FileCount, m.Bytes, total)
	}
	if m.CreatedBy != "alice@acme.com" || m.UpdatedBy != "alice@acme.com" || !strings.HasPrefix(m.DeployID, "d_") {
		t.Errorf("meta actors/deploy = %+v", m)
	}
	if m.Owners == nil || len(m.Owners) != 0 {
		t.Errorf("owners in ownership=none = %v", m.Owners)
	}

	man, err := e.reg.GetManifest(ctx, "lunch")
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Files) != 5 {
		t.Fatalf("manifest = %+v", man.Files)
	}
	for _, f := range man.Files {
		info, err := e.store.Stat(ctx, sites.SiteKey("lunch", f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if f.ETag != info.ETag || f.Size != info.Size || f.SHA256 != sha256Hex(e.object("lunch", f.Path)) {
			t.Errorf("manifest entry %+v does not match storage", f)
		}
	}
	if !strings.Contains(e.logs.String(), `"audit":true`) || !strings.Contains(e.logs.String(), `"action":"deploy.commit"`) {
		t.Errorf("no audit log line for the commit: %s", e.logs.String())
	}

	// Second deploy of the same tree uploads nothing.
	puts := e.store.PutCount()
	res2, err := c.Deploy(ctx, "lunch", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Uploaded != 0 || res2.Unchanged != 5 || res2.Deleted != 0 || res2.Bytes != 0 {
		t.Errorf("second deploy = %+v", res2)
	}
	if d := e.store.PutCount() - puts; d != 2 { // manifest + meta only
		t.Errorf("second deploy made %d puts, want 2", d)
	}

	// Modify one file, remove one, add one.
	writeTree(t, dir, map[string]string{"app.js": "console.log('v2')", "new.txt": "fresh"})
	if err := os.Remove(filepath.Join(dir, "assets", "logo.png")); err != nil {
		t.Fatal(err)
	}
	res3, err := c.Deploy(ctx, "lunch", dir, DeployOptions{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res3.Uploaded != 2 || res3.Unchanged != 3 || res3.Deleted != 1 {
		t.Errorf("third deploy = %+v", res3)
	}
	want := []string{".well-known/security.txt", "app.js", "docs/guide.html", "index.html", "new.txt"}
	if got := e.siteKeys("lunch"); !slices.Equal(got, want) {
		t.Errorf("files after third deploy = %v, want %v", got, want)
	}
	if string(e.object("lunch", "app.js")) != "console.log('v2')" {
		t.Error("app.js not updated")
	}
	m3, _ := c.GetSite(ctx, "lunch")
	if m3.FileCount != 5 || m3.DeployID == m.DeployID || !m3.CreatedAt.Equal(m.CreatedAt) {
		t.Errorf("meta after third deploy = %+v", m3)
	}
}

func TestDeployIgnoreFile(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":       "x",
		"debug.log":        "x",
		"keep.log":         "x",
		"secret/key.pem":   "x",
		"src/main.ts":      "x",
		"src/main.js":      "x",
		"build/out/a.html": "x",
		IgnoreFile:         "# comment\n*.log\n!keep.log\nsecret/\n/src/*.ts\nbuild/**/*.html\n",
	})
	res, err := e.client("alice").Deploy(context.Background(), "ign", dir, DeployOptions{Ignore: []string{"src/main.js"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index.html", "keep.log"}
	if got := e.siteKeys("ign"); !slices.Equal(got, want) {
		t.Errorf("deployed %v, want %v", got, want)
	}
	if res.Uploaded != 2 {
		t.Errorf("uploaded = %d", res.Uploaded)
	}
}

func TestDeployEmptyDirFails(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".git/HEAD": "ref"})
	if _, err := e.client("alice").Deploy(context.Background(), "empty", dir, DeployOptions{}); err == nil || !strings.Contains(err.Error(), "no files") {
		t.Errorf("deploy of empty dir: %v", err)
	}
	resp, body := e.do("POST", "/sites/empty/deploys", "alice", CreateDeployRequest{Files: []FileEntry{}})
	wantStatus(t, resp, body, 400, CodeBadRequest)
}

func TestOwnership(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) {
		c.Sites.Ownership = "owner"
		c.Auth.Admins = []string{"carol@acme.com"}
	}))
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>Owned</title>"})

	res, err := e.client("alice").Deploy(ctx, "owned", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Site.Owners, []string{"alice@acme.com"}) {
		t.Fatalf("owners after first deploy = %v", res.Site.Owners)
	}

	// Bob is not an owner: deploy, refresh, delete and owners are refused.
	_, err = e.client("bob").Deploy(ctx, "owned", dir, DeployOptions{})
	ae := apiErr(t, err, 403, CodeForbidden)
	if !strings.Contains(ae.Message, "alice@acme.com") {
		t.Errorf("forbidden message = %q", ae.Message)
	}
	_, err = e.client("bob").Refresh(ctx, "owned")
	apiErr(t, err, 403, CodeForbidden)
	apiErr(t, e.client("bob").DeleteSite(ctx, "owned"), 403, CodeForbidden)
	_, err = e.client("bob").SetOwners(ctx, "owned", []string{"bob@acme.com"}, nil)
	apiErr(t, err, 403, CodeForbidden)
	// Anyone can still read.
	if _, err := e.client("bob").GetSite(ctx, "owned"); err != nil {
		t.Fatal(err)
	}

	// A commit is re-authorized: bob cannot commit alice's site even with
	// his own deploy id obtained before ownership existed.
	writeTree(t, dir, map[string]string{"index.html": "<title>v2</title>"})

	// Admins (flag or config list) are always allowed.
	if _, err := e.client("admin").Deploy(ctx, "owned", dir, DeployOptions{}); err != nil {
		t.Fatalf("admin deploy: %v", err)
	}
	if _, err := e.client("carol").Refresh(ctx, "owned"); err != nil {
		t.Fatalf("config admin refresh: %v", err)
	}
	m, _ := e.client("alice").GetSite(ctx, "owned")
	if !slices.Equal(m.Owners, []string{"alice@acme.com"}) || m.UpdatedBy != "carol@acme.com" {
		t.Errorf("meta after admin writes = %+v", m)
	}

	// Alice adds Bob; Bob can now deploy and remove Alice.
	m, err = e.client("alice").SetOwners(ctx, "owned", []string{" Bob@Acme.com ", "bob@acme.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Owners, []string{"alice@acme.com", "bob@acme.com"}) {
		t.Fatalf("owners = %v", m.Owners)
	}
	if _, err := e.client("bob").Deploy(ctx, "owned", dir, DeployOptions{}); err != nil {
		t.Fatalf("co-owner deploy: %v", err)
	}
	m, err = e.client("bob").SetOwners(ctx, "owned", nil, []string{"ALICE@acme.com"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Owners, []string{"bob@acme.com"}) {
		t.Fatalf("owners after removal = %v", m.Owners)
	}
	_, err = e.client("alice").Deploy(ctx, "owned", dir, DeployOptions{})
	apiErr(t, err, 403, CodeForbidden)

	// Invalid owners are rejected.
	_, err = e.client("bob").SetOwners(ctx, "owned", []string{"has space@x"}, nil)
	apiErr(t, err, 400, CodeBadRequest)
	_, err = e.client("bob").SetOwners(ctx, "nosuchsite", []string{"x@y"}, nil)
	apiErr(t, err, 404, CodeNotFound)

	// Admin deletes.
	if err := e.client("admin").DeleteSite(ctx, "owned"); err != nil {
		t.Fatal(err)
	}
}

func TestCommitReauthorizesOwnership(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { c.Sites.Ownership = "owner" }))
	files := map[string]string{"index.html": "x"}
	// Bob starts a deploy on an unowned site…
	resp, body := e.do("POST", "/sites/race/deploys", "bob", CreateDeployRequest{Files: entriesFor(files)})
	wantStatus(t, resp, body, 200, "")
	var bobDeploy CreateDeployResponse
	_ = json.Unmarshal(body, &bobDeploy)
	// …Alice deploys first and becomes the owner…
	dir := t.TempDir()
	writeTree(t, dir, files)
	if _, err := e.client("alice").Deploy(context.Background(), "race", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	// …so Bob's commit is refused.
	resp, body = e.do("POST", "/sites/race/deploys/"+bobDeploy.DeployID+"/commit", "bob", CommitRequest{Files: entriesFor(files)})
	wantStatus(t, resp, body, 403, CodeForbidden)
}

func TestOwnersEndpointRequiresOwnershipMode(t *testing.T) {
	e := newEnv(t)
	_, err := e.client("alice").SetOwners(context.Background(), "x", []string{"a@b"}, nil)
	apiErr(t, err, 501, CodeUnsupported)
}

func TestLimits(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) {
		c.Sites.MaxFiles = 3
		c.Sites.MaxFileBytes = 10
		c.Sites.MaxSiteBytes = 25
	}))
	tests := []struct {
		name  string
		files map[string]string
		code  int
	}{
		{"too many files", map[string]string{"a": "1", "b": "1", "c": "1", "d": "1"}, 413},
		{"file too large", map[string]string{"a": strings.Repeat("x", 11)}, 413},
		{"site too large", map[string]string{"a": strings.Repeat("x", 10), "b": strings.Repeat("x", 10), "c": strings.Repeat("x", 10)}, 413},
		{"within limits", map[string]string{"a": strings.Repeat("x", 10), "b": strings.Repeat("x", 10), "c": "12345"}, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.do("POST", "/sites/limits/deploys", "alice", CreateDeployRequest{Files: entriesFor(tc.files)})
			if tc.code == 413 {
				wantStatus(t, resp, body, 413, CodeTooLarge)
			} else {
				wantStatus(t, resp, body, tc.code, "")
			}
		})
	}
	// The client surfaces the same error.
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"big.bin": strings.Repeat("x", 11)})
	_, err := e.client("alice").Deploy(context.Background(), "limits", dir, DeployOptions{})
	apiErr(t, err, 413, CodeTooLarge)
}

func TestInvalidSiteNames(t *testing.T) {
	e := newEnv(t)
	for _, name := range []string{"Bad_Name", "www", "admin", "-x", strings.Repeat("a", 64), "a.b"} {
		resp, body := e.do("POST", "/sites/"+name+"/deploys", "alice", CreateDeployRequest{Files: entriesFor(map[string]string{"a": "b"})})
		wantStatus(t, resp, body, 400, CodeBadRequest)
	}
	// Reads of reserved names are fine (they simply do not exist).
	resp, body := e.do("GET", "/sites/www", "alice", nil)
	wantStatus(t, resp, body, 404, CodeNotFound)
	resp, body = e.do("GET", "/sites/Bad_Name", "alice", nil)
	wantStatus(t, resp, body, 400, CodeBadRequest)
	if _, err := e.client("alice").Deploy(context.Background(), "Bad", t.TempDir(), DeployOptions{}); !errors.Is(err, sites.ErrInvalidName) {
		t.Errorf("client deploy with bad name: %v", err)
	}
}

func TestInvalidFileLists(t *testing.T) {
	e := newEnv(t)
	good := sha256Hex([]byte("x"))
	tests := []struct {
		name  string
		files []FileEntry
	}{
		{"traversal", []FileEntry{{Path: "../etc/passwd", Size: 1, SHA256: good}}},
		{"inner traversal", []FileEntry{{Path: "a/../../b", Size: 1, SHA256: good}}},
		{"absolute", []FileEntry{{Path: "/etc/passwd", Size: 1, SHA256: good}}},
		{"dot segment", []FileEntry{{Path: "a/./b", Size: 1, SHA256: good}}},
		{"empty segment", []FileEntry{{Path: "a//b", Size: 1, SHA256: good}}},
		{"trailing slash", []FileEntry{{Path: "a/", Size: 1, SHA256: good}}},
		{"empty", []FileEntry{{Path: "", Size: 1, SHA256: good}}},
		{"reserved", []FileEntry{{Path: "__backyard/backyard.js", Size: 1, SHA256: good}}},
		{"reserved bare", []FileEntry{{Path: "__backyard", Size: 1, SHA256: good}}},
		{"backslash", []FileEntry{{Path: `a\b`, Size: 1, SHA256: good}}},
		{"nul", []FileEntry{{Path: "a\x00b", Size: 1, SHA256: good}}},
		{"newline", []FileEntry{{Path: "a\nb", Size: 1, SHA256: good}}},
		{"too long", []FileEntry{{Path: strings.Repeat("a", 1025), Size: 1, SHA256: good}}},
		{"git segment", []FileEntry{{Path: ".git/config", Size: 1, SHA256: good}}},
		{"git segment any case", []FileEntry{{Path: "sub/.GiT/HEAD", Size: 1, SHA256: good}}},
		{"bare git", []FileEntry{{Path: ".git", Size: 1, SHA256: good}}},
		{"duplicate", []FileEntry{{Path: "a", Size: 1, SHA256: good}, {Path: "a", Size: 1, SHA256: good}}},
		{"file and dir", []FileEntry{{Path: "a", Size: 1, SHA256: good}, {Path: "a/b", Size: 1, SHA256: good}}},
		{"negative size", []FileEntry{{Path: "a", Size: -1, SHA256: good}}},
		{"short sha", []FileEntry{{Path: "a", Size: 1, SHA256: "abc"}}},
		{"non-hex sha", []FileEntry{{Path: "a", Size: 1, SHA256: strings.Repeat("g", 64)}}},
		{"bad content type", []FileEntry{{Path: "a", Size: 1, SHA256: good, ContentType: "text/html\r\nX-Evil: 1"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.do("POST", "/sites/files/deploys", "alice", CreateDeployRequest{Files: tc.files})
			wantStatus(t, resp, body, 400, CodeBadRequest)
		})
	}
	t.Run("malformed json", func(t *testing.T) {
		resp, body := e.do("POST", "/sites/files/deploys", "alice", `{"files": [`)
		wantStatus(t, resp, body, 400, CodeBadRequest)
	})
	t.Run("empty body", func(t *testing.T) {
		resp, body := e.do("POST", "/sites/files/deploys", "alice", nil)
		wantStatus(t, resp, body, 400, CodeBadRequest)
	})
	t.Run("uppercase sha accepted", func(t *testing.T) {
		resp, body := e.do("POST", "/sites/files/deploys", "alice", CreateDeployRequest{Files: []FileEntry{{Path: "a", Size: 1, SHA256: strings.ToUpper(good)}}})
		wantStatus(t, resp, body, 200, "")
	})
}

func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { c.Sites.MaxFiles = 1 }))
	big := `{"files":[],"message":"` + strings.Repeat("x", int(e.gw.maxDeployBody())) + `"}`
	resp, body := e.do("POST", "/sites/big/deploys", "alice", big)
	wantStatus(t, resp, body, 413, CodeTooLarge)
}

// createDeploy starts a deploy with raw requests.
func (e *env) createDeploy(token, site string, files map[string]string) CreateDeployResponse {
	e.t.Helper()
	resp, body := e.do("POST", "/sites/"+site+"/deploys", token, CreateDeployRequest{Files: entriesFor(files)})
	wantStatus(e.t, resp, body, 200, "")
	var out CreateDeployResponse
	if err := json.Unmarshal(body, &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func uploadFor(t *testing.T, cr CreateDeployResponse, p string) Upload {
	t.Helper()
	for _, u := range cr.Uploads {
		if u.Path == p {
			return u
		}
	}
	t.Fatalf("no upload for %s in %+v", p, cr.Uploads)
	return Upload{}
}

func TestUploadTokens(t *testing.T) {
	e := newEnv(t)
	files := map[string]string{"index.html": "hello world"}
	cr := e.createDeploy("alice", "tok", files)
	up := uploadFor(t, cr, "index.html")
	if !strings.HasPrefix(up.URL, e.srv.URL+"/uploads/") || up.Method != "PUT" || up.Headers["Content-Type"] != "text/html; charset=utf-8" {
		t.Fatalf("upload = %+v", up)
	}
	if time.Until(cr.ExpiresAt) < 59*time.Minute || time.Until(cr.ExpiresAt) > 61*time.Minute {
		t.Errorf("expires_at = %v", cr.ExpiresAt)
	}

	// Seed a good version to check failed uploads never clobber it.
	e.store.Put(context.Background(), sites.SiteKey("tok", "index.html"), strings.NewReader("previous!!!"), 11, "text/html")

	t.Run("tampered", func(t *testing.T) {
		tok := strings.TrimPrefix(up.URL, e.srv.URL+"/uploads/")
		payload, sig, _ := strings.Cut(tok, ".")
		bad := payload[:len(payload)-2] + "AA." + sig
		resp, body := e.do("PUT", "/uploads/"+bad, "", "hello world")
		wantStatus(t, resp, body, 403, CodeForbidden)
		resp, body = e.do("PUT", "/uploads/garbage", "", "hello world")
		wantStatus(t, resp, body, 403, CodeForbidden)
	})
	t.Run("expired", func(t *testing.T) {
		tok, _ := signClaims(e.gw.uploadKey, uploadClaims{Site: "tok", Path: "index.html", Size: 11, SHA256: sha256Hex([]byte("hello world")), Exp: time.Now().Add(-time.Second).Unix()})
		resp, body := e.do("PUT", "/uploads/"+tok, "", "hello world")
		wantStatus(t, resp, body, 403, CodeForbidden)
	})
	t.Run("signed with deploy key", func(t *testing.T) {
		tok, _ := signClaims(e.gw.deployKey, uploadClaims{Site: "tok", Path: "index.html", Size: 11, SHA256: sha256Hex([]byte("hello world")), Exp: time.Now().Add(time.Hour).Unix()})
		resp, body := e.do("PUT", "/uploads/"+tok, "", "hello world")
		wantStatus(t, resp, body, 403, CodeForbidden)
	})
	t.Run("sha mismatch", func(t *testing.T) {
		resp, body := e.do("PUT", up.URL, "", "HELLO WORLD")
		wantStatus(t, resp, body, 400, CodeBadRequest)
		if got := string(e.object("tok", "index.html")); got != "previous!!!" {
			t.Errorf("object after failed upload = %q", got)
		}
	})
	t.Run("size mismatch", func(t *testing.T) {
		resp, body := e.do("PUT", up.URL, "", "hello")
		wantStatus(t, resp, body, 400, CodeBadRequest)
		resp, body = e.do("PUT", up.URL, "", "hello world and more")
		wantStatus(t, resp, body, 400, CodeBadRequest)
	})
	t.Run("chunked body longer than declared", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", up.URL, struct{ *strings.Reader }{strings.NewReader("hello world!")})
		req.ContentLength = -1
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 413 {
			t.Errorf("status = %d, want 413", resp.StatusCode)
		}
		if got := string(e.object("tok", "index.html")); got != "previous!!!" {
			t.Errorf("object after failed upload = %q", got)
		}
	})
	t.Run("chunked body shorter than declared", func(t *testing.T) {
		req, _ := http.NewRequest("PUT", up.URL, struct{ *strings.Reader }{strings.NewReader("hello")})
		req.ContentLength = -1
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
	t.Run("good upload needs no auth or csrf header", func(t *testing.T) {
		resp, body := e.do("PUT", up.URL, "", "hello world", "Content-Type", "text/html; charset=utf-8")
		wantStatus(t, resp, body, 200, "")
		if got := string(e.object("tok", "index.html")); got != "hello world" {
			t.Errorf("object = %q", got)
		}
		info, _ := e.store.Stat(context.Background(), sites.SiteKey("tok", "index.html"))
		if info.ContentType != "text/html; charset=utf-8" {
			t.Errorf("content type = %q", info.ContentType)
		}
	})
	t.Run("wrong method", func(t *testing.T) {
		resp, body := e.do("POST", up.URL, "alice", "hello world")
		wantStatus(t, resp, body, 405, CodeBadRequest)
		if resp.Header.Get("Allow") != "PUT" {
			t.Errorf("Allow = %q", resp.Header.Get("Allow"))
		}
	})
}

func TestEmptyFileUpload(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"empty.txt": "", "index.html": "x"})
	if _, err := e.client("alice").Deploy(context.Background(), "empty-file", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if b := e.object("empty-file", "empty.txt"); b == nil || len(b) != 0 {
		t.Errorf("empty.txt = %q", b)
	}
}

func TestDeployIDs(t *testing.T) {
	e := newEnv(t)
	files := map[string]string{"index.html": "hi"}
	commit := func(token, site, id string) (*http.Response, []byte) {
		return e.do("POST", "/sites/"+site+"/deploys/"+id+"/commit", token, CommitRequest{Files: entriesFor(files)})
	}
	cr := e.createDeploy("alice", "ids", files)

	t.Run("tampered", func(t *testing.T) {
		payload, sig, _ := strings.Cut(cr.DeployID, ".")
		resp, body := commit("alice", "ids", payload+"x."+sig)
		wantStatus(t, resp, body, 400, CodeBadRequest)
		resp, body = commit("alice", "ids", "not-a-token")
		wantStatus(t, resp, body, 400, CodeBadRequest)
	})
	t.Run("expired", func(t *testing.T) {
		id, _ := signClaims(e.gw.deployKey, deployClaims{Site: "ids", Actor: "alice@acme.com", Exp: time.Now().Add(-time.Minute).Unix(), Nonce: "n"})
		resp, body := commit("alice", "ids", id)
		wantStatus(t, resp, body, 400, CodeBadRequest)
		if !strings.Contains(errCode(t, body).Error.Message, "expired") {
			t.Errorf("message = %s", body)
		}
	})
	t.Run("other site", func(t *testing.T) {
		resp, body := commit("alice", "other", cr.DeployID)
		wantStatus(t, resp, body, 400, CodeBadRequest)
	})
	t.Run("other actor", func(t *testing.T) {
		resp, body := commit("bob", "ids", cr.DeployID)
		wantStatus(t, resp, body, 403, CodeForbidden)
	})
	t.Run("upload token is not a deploy id", func(t *testing.T) {
		tok := strings.TrimPrefix(cr.Uploads[0].URL, e.srv.URL+"/uploads/")
		resp, body := commit("alice", "ids", tok)
		wantStatus(t, resp, body, 400, CodeBadRequest)
	})
	t.Run("missing upload", func(t *testing.T) {
		resp, body := commit("alice", "ids", cr.DeployID)
		wantStatus(t, resp, body, 409, CodeConflict)
		if !strings.Contains(string(body), "index.html (missing)") {
			t.Errorf("body = %s", body)
		}
	})
	t.Run("wrong size in storage", func(t *testing.T) {
		e.store.Put(context.Background(), sites.SiteKey("ids", "index.html"), strings.NewReader("hello"), 5, "")
		resp, body := commit("alice", "ids", cr.DeployID)
		wantStatus(t, resp, body, 409, CodeConflict)
		// Every upload URL of this store binds the size, so the object is
		// another deploy's verified upload or a direct sync: it stays.
		if !strings.Contains(string(body), "index.html (5 bytes in storage, expected 2)") || strings.Contains(string(body), "deleted") {
			t.Errorf("body = %s", body)
		}
		if b := e.object("ids", "index.html"); string(b) != "hello" {
			t.Errorf("object of the wrong size = %q, want it kept", b)
		}
	})
	t.Run("file list revalidated", func(t *testing.T) {
		resp, body := e.do("POST", "/sites/ids/deploys/"+cr.DeployID+"/commit", "alice", CommitRequest{Files: []FileEntry{{Path: "../x", Size: 1, SHA256: sha256Hex([]byte("x"))}}})
		wantStatus(t, resp, body, 400, CodeBadRequest)
	})
	t.Run("ok", func(t *testing.T) {
		e.store.Put(context.Background(), sites.SiteKey("ids", "index.html"), strings.NewReader("hi"), 2, "")
		resp, body := commit("alice", "ids", cr.DeployID)
		wantStatus(t, resp, body, 200, "")
		var out CommitResponse
		_ = json.Unmarshal(body, &out)
		if out.URL != "https://ids.backyard.test/" || out.Site.Name != "ids" || out.Site.FileCount != 1 {
			t.Errorf("commit response = %+v", out)
		}
	})
}

func TestCSRF(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { c.Deploy.AllowAnonymous = true }))
	files := entriesFor(map[string]string{"a.txt": "a"})
	// Cookie/ambient credentials without the header are refused.
	resp, body := e.do("POST", "/sites/csrf/deploys", "", CreateDeployRequest{Files: files})
	wantStatus(t, resp, body, 403, CodeForbidden)
	resp, body = e.do("DELETE", "/sites/csrf", "", nil)
	wantStatus(t, resp, body, 403, CodeForbidden)
	resp, body = e.do("POST", "/sites/csrf/deploys", "", CreateDeployRequest{Files: files}, "X-Backyard", "true")
	wantStatus(t, resp, body, 403, CodeForbidden)
	// With the header it works.
	resp, body = e.do("POST", "/sites/csrf/deploys", "", CreateDeployRequest{Files: files}, "X-Backyard", "1")
	wantStatus(t, resp, body, 200, "")
	// A bearer token is not ambient: no header needed.
	resp, body = e.do("POST", "/sites/csrf/deploys", "alice", CreateDeployRequest{Files: files})
	wantStatus(t, resp, body, 200, "")
	// GETs never need it.
	resp, body = e.do("GET", "/sites", "", nil)
	wantStatus(t, resp, body, 200, "")
	// Basic credentials are ambient (browsers resend them): not exempt.
	resp, body = e.do("POST", "/sites/csrf/deploys", "", CreateDeployRequest{Files: files}, "Authorization", "Basic dXNlcjpwYXNz")
	wantStatus(t, resp, body, 403, CodeForbidden)
	// A bearer token on a browser cross-site request (e.g. injected by a
	// proxy) is not trusted without the header either.
	resp, body = e.do("POST", "/sites/csrf/deploys", "alice", CreateDeployRequest{Files: files}, "Origin", "https://evil.example")
	wantStatus(t, resp, body, 403, CodeForbidden)
	resp, body = e.do("POST", "/sites/csrf/deploys", "alice", CreateDeployRequest{Files: files}, "Sec-Fetch-Site", "cross-site")
	wantStatus(t, resp, body, 403, CodeForbidden)
	resp, body = e.do("POST", "/sites/csrf/deploys", "alice", CreateDeployRequest{Files: files}, "Sec-Fetch-Site", "same-origin")
	wantStatus(t, resp, body, 200, "")
	resp, body = e.do("POST", "/sites/csrf/deploys", "alice", CreateDeployRequest{Files: files}, "Origin", "https://evil.example", "X-Backyard", "1")
	wantStatus(t, resp, body, 200, "")
}

func TestAnonymousRules(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "anon"})

	t.Run("mode none without allow_anonymous", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.client("").Deploy(ctx, "anon", dir, DeployOptions{})
		ae := apiErr(t, err, 401, CodeUnauthenticated)
		if !strings.Contains(ae.Message, "token") {
			t.Errorf("message = %q", ae.Message)
		}
		if _, err := e.client("").ListSites(ctx); err != nil {
			t.Errorf("anonymous read: %v", err)
		}
		u, err := e.client("").Me(ctx)
		if err != nil || !u.Anonymous {
			t.Errorf("anonymous /me = %+v, %v", u, err)
		}
		// Tokens still work.
		if _, err := e.client("ci").Deploy(ctx, "anon", dir, DeployOptions{}); err != nil {
			t.Fatal(err)
		}
		// An invalid token is an error, not a silent guest.
		_, err = e.client("bogus").Me(ctx)
		apiErr(t, err, 401, CodeUnauthenticated)
		_, err = e.client("bogus").Deploy(ctx, "anon", dir, DeployOptions{})
		apiErr(t, err, 401, CodeUnauthenticated)
		// A valid token minted for a guest is that guest: it reads, but
		// cannot deploy, and is told why.
		u, err = e.client("guest").Me(ctx)
		if err != nil || !u.Anonymous || u.ID != guestTok.ID {
			t.Errorf("guest token /me = %+v, %v", u, err)
		}
		_, err = e.client("guest").Deploy(ctx, "anon", dir, DeployOptions{})
		if ae := apiErr(t, err, 401, CodeUnauthenticated); !strings.Contains(ae.Message, "anonymous guest") {
			t.Errorf("guest token deploy message = %q", ae.Message)
		}
	})
	t.Run("mode none with allow_anonymous", func(t *testing.T) {
		e := newEnv(t, withConfig(func(c *config.Config) {
			c.Deploy.AllowAnonymous = true
			c.Sites.Ownership = "owner"
		}))
		res, err := e.client("").Deploy(ctx, "anon", dir, DeployOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Site.UpdatedBy, "anon:") || len(res.Site.Owners) != 0 {
			t.Errorf("anonymous deploy meta = %+v", res.Site)
		}
		if !e.gw.Info().Auth.AnonymousDeploy {
			t.Error("info.auth.anonymous_deploy = false")
		}
		// Guests may deploy and refresh, but deleting sites and changing
		// owners always needs a real identity.
		if _, err := e.client("").Refresh(ctx, "anon"); err != nil {
			t.Errorf("anonymous refresh: %v", err)
		}
		apiErr(t, e.client("").DeleteSite(ctx, "anon"), 401, CodeUnauthenticated)
		_, err = e.client("").SetOwners(ctx, "anon", []string{"me@x"}, nil)
		apiErr(t, err, 401, CodeUnauthenticated)
		// A guest token deploys as its guest and cannot delete either.
		res, err = e.client("guest").Deploy(ctx, "anon", dir, DeployOptions{})
		if err != nil || res.Site.UpdatedBy != guestTok.ID || len(res.Site.Owners) != 0 {
			t.Errorf("guest token deploy = %+v, %v", res, err)
		}
		apiErr(t, e.client("guest").DeleteSite(ctx, "anon"), 401, CodeUnauthenticated)
		if err := e.client("alice").DeleteSite(ctx, "anon"); err != nil {
			t.Errorf("token delete: %v", err)
		}
	})
	t.Run("bearer-only gateway with allow_anonymous", func(t *testing.T) {
		e := newEnv(t,
			withConfig(func(c *config.Config) { c.Deploy.AllowAnonymous = true }),
			withAuth(func(a *fakeAuth) { a.bearerOnly = true }))
		res, err := e.client("").Deploy(ctx, "anon", dir, DeployOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Site.UpdatedBy, "anon:") {
			t.Errorf("guest actor = %q", res.Site.UpdatedBy)
		}
		_, err = e.client("bogus").Deploy(ctx, "anon", dir, DeployOptions{})
		apiErr(t, err, 401, CodeUnauthenticated)
	})
	t.Run("oidc mode requires a token", func(t *testing.T) {
		e := newEnv(t,
			withConfig(func(c *config.Config) { c.Auth.Mode = "oidc" }),
			withAuth(func(a *fakeAuth) { a.mode = auth.ModeOIDC; a.loginURL = "https://backyard.test/__backyard/auth/login" }))
		_, err := e.client("").ListSites(ctx)
		ae := apiErr(t, err, 401, CodeUnauthenticated)
		if ae.LoginURL != "https://backyard.test/__backyard/auth/login?rd=https%3A%2F%2Fbackyard.test%2F" {
			t.Errorf("login_url = %q", ae.LoginURL)
		}
		_, err = e.client("forbidden").ListSites(ctx)
		apiErr(t, err, 403, CodeForbidden)
		if _, err := e.client("alice").ListSites(ctx); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("authenticator failure", func(t *testing.T) {
		e := newEnv(t, withAuth(func(a *fakeAuth) { a.failWith = errors.New("idp down") }))
		_, err := e.client("alice").ListSites(ctx)
		apiErr(t, err, 500, CodeInternal)
	})
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { c.RateLimit.DeploysPerHour = 2 }))
	files := map[string]string{"a": "b"}
	e.createDeploy("alice", "rl", files)
	e.createDeploy("alice", "rl", files)
	resp, body := e.do("POST", "/sites/rl/deploys", "alice", CreateDeployRequest{Files: entriesFor(files)})
	wantStatus(t, resp, body, 429, CodeRateLimited)
	if ra := resp.Header.Get("Retry-After"); ra == "" || ra == "0" {
		t.Errorf("Retry-After = %q", ra)
	}
	// Other users have their own bucket.
	e.createDeploy("bob", "rl", files)
	// The client does not wait for long throttling.
	start := time.Now()
	dir := t.TempDir()
	writeTree(t, dir, files)
	_, err := e.client("alice").Deploy(context.Background(), "rl", dir, DeployOptions{})
	apiErr(t, err, 429, CodeRateLimited)
	if time.Since(start) > 5*time.Second {
		t.Errorf("client waited %v for a long Retry-After", time.Since(start))
	}
}

func TestRateLimitDisabled(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) {
		c.RateLimit.DeploysPerHour = 1
		c.RateLimit.Disabled = true
	}))
	for range 3 {
		e.createDeploy("alice", "rl", map[string]string{"a": "b"})
	}
}

func TestPresignedUploads(t *testing.T) {
	e := newEnv(t)
	var inflight, maxInflight atomic.Int32
	var authSeen atomic.Bool
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			m := maxInflight.Load()
			if n <= m || maxInflight.CompareAndSwap(m, n) {
				break
			}
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Backyard") != "" {
			authSeen.Store(true)
		}
		time.Sleep(20 * time.Millisecond)
		e.store.PresignHandler().ServeHTTP(w, r)
	}))
	defer ps.Close()
	e.store.EnablePresign(ps.URL)

	dir := t.TempDir()
	tree := map[string]string{"index.html": "<title>Presigned</title>"}
	for i := range 6 {
		tree["f"+string(rune('a'+i))+".txt"] = strings.Repeat("z", i)
	}
	writeTree(t, dir, tree)
	cr := e.createDeploy("alice", "pre", map[string]string{"index.html": "<title>Presigned</title>"})
	if !strings.HasPrefix(cr.Uploads[0].URL, ps.URL) || cr.Uploads[0].Headers["Content-Type"] == "" {
		t.Fatalf("upload not presigned: %+v", cr.Uploads[0])
	}
	res, err := e.client("alice").Deploy(context.Background(), "pre", dir, DeployOptions{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 7 || res.Site.Title != "Presigned" {
		t.Errorf("result = %+v", res)
	}
	if authSeen.Load() {
		t.Error("credentials were sent to the presigned storage URL")
	}
	if m := maxInflight.Load(); m > 2 {
		t.Errorf("max concurrent uploads = %d, want <= 2", m)
	}
	if got := e.siteKeys("pre"); len(got) != 7 {
		t.Errorf("stored = %v", got)
	}
}

func TestPresignDisabledByConfig(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) { f := false; c.Deploy.Presign = &f }))
	e.store.EnablePresign("http://storage.invalid")
	cr := e.createDeploy("alice", "nopre", map[string]string{"a": "b"})
	if !strings.HasPrefix(cr.Uploads[0].URL, e.srv.URL+"/uploads/") {
		t.Errorf("upload URL = %s", cr.Uploads[0].URL)
	}
}

func TestOutOfBandChangeIsReuploaded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>A</title>", "app.js": "aaaa"})
	c := e.client("alice")
	if _, err := c.Deploy(ctx, "oob", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	// Someone syncs a different app.js of the same size directly to storage.
	e.store.Put(ctx, sites.SiteKey("oob", "app.js"), strings.NewReader("bbbb"), 4, "")
	res, err := c.Deploy(ctx, "oob", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Unchanged != 1 {
		t.Errorf("redeploy after out-of-band change = %+v", res)
	}
	if string(e.object("oob", "app.js")) != "aaaa" {
		t.Error("app.js not restored")
	}

	// Direct sync + refresh rebuilds the manifest and meta.
	e.store.Put(ctx, sites.SiteKey("oob", "index.html"), strings.NewReader("<title>Synced</title>"), 21, "")
	e.store.Put(ctx, sites.SiteKey("oob", "extra.css"), strings.NewReader("body{}"), 6, "")
	m, err := c.Refresh(ctx, "oob")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Synced" || m.FileCount != 3 || m.UpdatedBy != "alice@acme.com" {
		t.Errorf("refreshed meta = %+v", m)
	}
	files, err := c.SiteFiles(ctx, "oob")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.SHA256 != sha256Hex(e.object("oob", f.Path)) {
			t.Errorf("file %s sha = %q after refresh", f.Path, f.SHA256)
		}
	}
	// After the refresh, deploying the (older) local tree uploads the
	// differing index.html only and deletes extra.css.
	res, err = c.Deploy(ctx, "oob", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Unchanged != 1 || res.Deleted != 1 {
		t.Errorf("deploy after refresh = %+v", res)
	}
	_, err = c.Refresh(ctx, "nosuch")
	apiErr(t, err, 404, CodeNotFound)
}

func TestFilesListingAndRead(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	tree := map[string]string{"index.html": "<title>F</title>", "dir/with space/ü.txt": "unicode", "bin.dat": "\x00\x01\x02\xff"}
	writeTree(t, dir, tree)
	c := e.client("bob")
	if _, err := c.Deploy(ctx, "files", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	// A direct-synced file has no recorded sha256.
	e.store.Put(ctx, sites.SiteKey("files", "direct.txt"), strings.NewReader("d"), 1, "")

	list, err := c.SiteFiles(ctx, "files")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("files = %+v", list)
	}
	for _, f := range list {
		want, inTree := tree[f.Path]
		if inTree && (f.SHA256 != sha256Hex([]byte(want)) || f.Size != int64(len(want))) {
			t.Errorf("listing entry %+v", f)
		}
		if !inTree && f.SHA256 != "" {
			t.Errorf("direct file has sha %q", f.SHA256)
		}
		if f.UpdatedAt.IsZero() {
			t.Errorf("%s has no updated_at", f.Path)
		}
	}
	for p, want := range tree {
		got, err := c.ReadFile(ctx, "files", p, 0)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", p, err)
		}
		if string(got) != want {
			t.Errorf("ReadFile(%q) = %q", p, got)
		}
	}
	if _, err := c.ReadFile(ctx, "files", "index.html", 3); !errors.Is(err, ErrFileTooLarge) {
		t.Errorf("ReadFile over limit: %v", err)
	}
	_, err = c.ReadFile(ctx, "files", "nope.html", 0)
	apiErr(t, err, 404, CodeNotFound)
	_, err = c.SiteFiles(ctx, "nosuch")
	apiErr(t, err, 404, CodeNotFound)
	if _, err := c.ReadFile(ctx, "files", "../x", 0); err == nil {
		t.Error("ReadFile accepted a traversal path")
	}

	resp, body := e.do("GET", "/sites/files/files/index.html", "bob", nil)
	wantStatus(t, resp, body, 200, "")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "sandbox") {
		t.Errorf("CSP = %q", csp)
	}
	if resp.Header.Get("Content-Disposition") != "attachment" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", resp.Header)
	}
	resp, body = e.do("GET", "/sites/files/files/__backyard/x", "bob", nil)
	wantStatus(t, resp, body, 400, CodeBadRequest)
	// Repository metadata is never served back, even when the store holds
	// it (written before the rule, or by a direct sync); it is still listed
	// so that clients can see and remove it.
	e.store.Put(ctx, sites.SiteKey("files", ".git/config"), strings.NewReader("[core]"), 6, "")
	e.store.Put(ctx, sites.SiteKey("files", "sub/.GIT/HEAD"), strings.NewReader("ref"), 3, "")
	for _, p := range []string{".git/config", "sub/.GIT/HEAD", ".git"} {
		resp, body = e.do("GET", "/sites/files/files/"+p, "bob", nil)
		wantStatus(t, resp, body, 400, CodeBadRequest)
		if !strings.Contains(string(body), `.git`) {
			t.Errorf("GET %s: body %s does not name the .git rule", p, body)
		}
		if _, err := c.ReadFile(ctx, "files", p, 0); err == nil || !strings.Contains(err.Error(), `".git" segment`) {
			t.Errorf("ReadFile(%q) = %v, want the client-side .git rule", p, err)
		}
	}
	if list, err := c.SiteFiles(ctx, "files"); err != nil || !slices.ContainsFunc(list, func(f SiteFile) bool { return f.Path == ".git/config" }) {
		t.Errorf("listing without .git/config: %v %+v", err, list)
	}
	resp, _ = e.do("HEAD", "/sites/files/files/index.html", "bob", nil)
	if resp.StatusCode != 200 || resp.ContentLength != int64(len(tree["index.html"])) {
		t.Errorf("HEAD = %d, length %d", resp.StatusCode, resp.ContentLength)
	}
}

func TestListAndGetSites(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.client("alice")
	list, err := c.ListSites(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("empty list = %v, %v", list, err)
	}
	resp, body := e.do("GET", "/sites", "alice", nil)
	if !strings.Contains(string(body), `"sites":[]`) {
		t.Errorf("empty list body = %s", body)
	}
	_ = resp
	for _, name := range []string{"one", "two"} {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"index.html": "<title>" + name + "</title>"})
		if _, err := c.Deploy(ctx, name, dir, DeployOptions{}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	list, err = c.ListSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "two" || list[1].Title != "one" {
		t.Errorf("list = %+v", list)
	}
	_, err = c.GetSite(ctx, "three")
	apiErr(t, err, 404, CodeNotFound)
}

func TestDeleteSite(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "x"})
	c := e.client("alice")
	if _, err := c.Deploy(ctx, "gone", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	e.store.Put(ctx, sites.UploadKey("gone", "id1", "pic.png"), strings.NewReader("png"), 3, "")
	if err := c.DeleteSite(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if keys := e.store.Keys(); len(keys) != 0 {
		t.Errorf("keys left after delete: %v", keys)
	}
	if !slices.Equal(e.deleted, []string{"gone"}) {
		t.Errorf("delete hook calls = %v", e.deleted)
	}
	_, err := c.GetSite(ctx, "gone")
	apiErr(t, err, 404, CodeNotFound)
	apiErr(t, c.DeleteSite(ctx, "gone"), 404, CodeNotFound)
	if !strings.Contains(e.logs.String(), `"action":"site.delete"`) {
		t.Error("no audit line for delete")
	}

	// A failing hook is reported.
	if _, err := c.Deploy(ctx, "gone", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	e.hookErr = errors.New("docstore down")
	apiErr(t, c.DeleteSite(ctx, "gone"), 500, CodeInternal)
	if strings.Contains(e.logs.String(), "docstore down") == false {
		t.Error("hook error not logged")
	}
}

func TestMeAndInfo(t *testing.T) {
	e := newEnv(t, withConfig(func(c *config.Config) {
		c.Auth.Mode = "oidc"
		c.Auth.OIDC.Issuer = "https://idp.example.com"
		c.Auth.OIDC.DeviceClientID = "cli-app"
		c.Deploy.RsyncTarget = "deploy@host:/srv/sites"
	}), withAuth(func(a *fakeAuth) { a.mode = auth.ModeOIDC }))
	e.store.Desc.Type, e.store.Desc.Bucket = "s3", "acme-backyard"
	ctx := context.Background()
	u, err := e.client("alice").Me(ctx)
	if err != nil || u.ID != "alice@acme.com" {
		t.Fatalf("Me = %+v, %v", u, err)
	}
	_, err = e.client("").Me(ctx)
	ae := apiErr(t, err, 401, CodeUnauthenticated)
	if ae.LoginURL != "" {
		t.Errorf("login_url without browser login = %q", ae.LoginURL)
	}

	inf, err := e.client("").Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if inf.Name != "Backyard" || inf.Version != "test" || inf.PublicURL != "https://backyard.test" || inf.BaseDomain != "backyard.test" || inf.URLMode != "host" {
		t.Errorf("info = %+v", inf)
	}
	if inf.GatewayURL != e.srv.URL || inf.RsyncTarget != "deploy@host:/srv/sites" || !slices.Equal(inf.Methods, []string{"api"}) {
		t.Errorf("info gateway/methods = %+v", inf)
	}
	if inf.Auth.Mode != "oidc" || inf.Auth.CLILoginURL != "https://backyard.test/__backyard/auth/cli" || inf.Auth.AnonymousDeploy {
		t.Errorf("info auth = %+v", inf.Auth)
	}
	if inf.Auth.Device == nil || inf.Auth.Device.ClientID != "cli-app" || inf.Auth.Device.Issuer != "https://idp.example.com" || len(inf.Auth.Device.Scopes) == 0 {
		t.Errorf("info device = %+v", inf.Auth.Device)
	}
	if inf.Storage == nil || inf.Storage.Bucket != "acme-backyard" {
		t.Errorf("info storage = %+v", inf.Storage)
	}
	if inf.Limits.MaxFiles != 10000 || inf.Limits.MaxFileBytes != 100<<20 || inf.Limits.MaxSiteBytes != 500<<20 {
		t.Errorf("info limits = %+v", inf.Limits)
	}

	// Info() mirrors /info; storage hidden when expose_storage is false;
	// gateway_url prefers deploy.gateway_url.
	f := false
	e.cfg.Deploy.ExposeStorage = &f
	e.cfg.Deploy.GatewayURL = "https://gw.example.com/"
	inf2 := e.gw.Info()
	if inf2.Storage != nil || inf2.GatewayURL != "https://gw.example.com" {
		t.Errorf("Info() = %+v", inf2)
	}
}

func TestInfoAuthModes(t *testing.T) {
	tests := []struct {
		mode     string
		cliLogin bool
	}{{"none", false}, {"oidc", true}, {"iap", true}, {"alb", true}, {"header", true}}
	for _, tc := range tests {
		e := newEnv(t, withConfig(func(c *config.Config) { c.Auth.Mode = tc.mode }))
		inf := e.gw.Info()
		if (inf.Auth.CLILoginURL != "") != tc.cliLogin || inf.Auth.Device != nil {
			t.Errorf("mode %s: auth info = %+v", tc.mode, inf.Auth)
		}
	}
}

func TestInfoDerivesGatewayURLFromRequest(t *testing.T) {
	cfg := newTestConfig(t)
	gw, err := NewGateway(GatewayOptions{Config: cfg, Store: newEnv(t).store, Auth: &fakeAuth{mode: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/info", nil)
	req.Host = "abc123.lambda-url.us-east-1.on.aws"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	var inf Info
	_ = json.Unmarshal(rec.Body.Bytes(), &inf)
	if inf.GatewayURL != "https://abc123.lambda-url.us-east-1.on.aws" {
		t.Errorf("gateway_url = %q", inf.GatewayURL)
	}
}

func TestRoutingErrors(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/sites/x/files/a//b", "/sites/x/files/a/./b", "/sites/x/files/a/../b"} {
		req := httptest.NewRequest("GET", "/", nil)
		req.URL.Path = p
		rec := httptest.NewRecorder()
		e.gw.ServeHTTP(rec, req)
		if rec.Code != 400 || rec.Header().Get("Location") != "" {
			t.Errorf("GET %s = %d (Location %q)", p, rec.Code, rec.Header().Get("Location"))
		}
	}
	resp, body := e.do("GET", "/nope", "alice", nil)
	wantStatus(t, resp, body, 404, CodeNotFound)
	resp, body = e.do("PUT", "/sites/x", "alice", nil)
	wantStatus(t, resp, body, 405, CodeBadRequest)
	if allow := resp.Header.Get("Allow"); allow != "GET, HEAD, DELETE" {
		t.Errorf("Allow = %q", allow)
	}
	resp, body = e.do("GET", "/sites/x/deploys", "alice", nil)
	wantStatus(t, resp, body, 405, CodeBadRequest)
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("content type = %q", resp.Header.Get("Content-Type"))
	}
}

func TestNewGatewayValidation(t *testing.T) {
	cfg := newTestConfig(t)
	store := newEnv(t).store
	a := &fakeAuth{mode: "none"}
	short := *cfg
	short.Server.Secret = "short"
	for name, opts := range map[string]GatewayOptions{
		"no config":    {Store: store, Auth: a},
		"no store":     {Config: cfg, Auth: a},
		"no auth":      {Config: cfg, Store: store},
		"short secret": {Config: &short, Store: store, Auth: a},
		"bad base url": {Config: cfg, Store: store, Auth: a, BaseURL: "ftp://x"},
	} {
		if _, err := NewGateway(opts); err == nil {
			t.Errorf("%s: NewGateway succeeded", name)
		}
	}
}

func TestStorageFailureIsInternal(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "x"})
	e.store.FailPut = func(key string) error {
		if strings.HasPrefix(key, sites.MetaPrefix) {
			return errors.New("disk full")
		}
		return nil
	}
	_, err := e.client("alice").Deploy(context.Background(), "fail", dir, DeployOptions{})
	ae := apiErr(t, err, 500, CodeInternal)
	if strings.Contains(ae.Message, "disk full") {
		t.Errorf("internal error leaked to the client: %q", ae.Message)
	}
	if !strings.Contains(e.logs.String(), "disk full") {
		t.Error("internal error not logged")
	}
}

// TestEmbeddedMount mounts the gateway like the server does: under
// EmbeddedPath with the prefix stripped.
func TestEmbeddedMount(t *testing.T) {
	cfg := newTestConfig(t)
	store := newEnv(t).store
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	gw, err := NewGateway(GatewayOptions{
		Config:  cfg,
		Store:   store,
		Auth:    &fakeAuth{mode: auth.ModeNone, users: defaultUsers()},
		BaseURL: srv.URL + EmbeddedPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux.Handle(EmbeddedPath+"/", http.StripPrefix(EmbeddedPath, gw))

	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>Embedded</title>", "a b/c.txt": "c"})
	c := NewClient(srv.URL+EmbeddedPath, "alice")
	res, err := c.Deploy(context.Background(), "embedded", dir, DeployOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Site.Title != "Embedded" || res.Uploaded != 2 {
		t.Errorf("result = %+v", res)
	}
	got, err := c.ReadFile(context.Background(), "embedded", "a b/c.txt", 0)
	if err != nil || string(got) != "c" {
		t.Errorf("ReadFile = %q, %v", got, err)
	}
	inf, err := c.Info(context.Background())
	if err != nil || inf.GatewayURL != srv.URL+EmbeddedPath {
		t.Errorf("info = %+v, %v", inf, err)
	}
}

// TestUploadOutlivesServerTimeouts checks that uploads lift the server's
// read/write deadlines.
func TestUploadOutlivesServerTimeouts(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewUnstartedServer(e.gw)
	srv.Config.ReadTimeout = 300 * time.Millisecond
	srv.Config.WriteTimeout = 300 * time.Millisecond
	srv.Start()
	defer srv.Close()
	content := strings.Repeat("slow", 64)
	cr := e.createDeploy("alice", "slow", map[string]string{"big.txt": content})
	path := strings.TrimPrefix(cr.Uploads[0].URL, e.srv.URL)
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < len(content); i += 64 {
			time.Sleep(100 * time.Millisecond) // 400 ms in total
			_, _ = pw.Write([]byte(content[i : i+64]))
		}
		_ = pw.Close()
	}()
	req, _ := http.NewRequest("PUT", srv.URL+path, pr)
	req.ContentLength = int64(len(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("slow upload status = %d", resp.StatusCode)
	}
	if got := string(e.object("slow", "big.txt")); got != content {
		t.Errorf("stored %d bytes", len(got))
	}
}

func TestUploadTimeout(t *testing.T) {
	if d := uploadTimeout(0); d != 5*time.Minute {
		t.Errorf("uploadTimeout(0) = %v", d)
	}
	if d := uploadTimeout(100 << 20); d <= 5*time.Minute || d > 2*time.Hour {
		t.Errorf("uploadTimeout(100 MiB) = %v", d)
	}
	if d := uploadTimeout(1 << 40); d != 2*time.Hour {
		t.Errorf("uploadTimeout(1 TiB) = %v", d)
	}
}
