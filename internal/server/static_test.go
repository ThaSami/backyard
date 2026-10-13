package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

func TestStaticRules(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{
		"index.html":      "root index",
		"about.html":      "about page",
		"docs/index.html": "docs index",
		"app.js":          "console.log(1)",
		"style.css":       "body{}",
		"data.json":       `{"a":1}`,
		"img/logo.png":    "PNG",
		"README":          "plain",
		"blog/post.html":  "post",
	})
	h.deploy("spa", map[string]string{"index.html": "spa root", "200.html": "spa shell"})
	h.deploy("custom", map[string]string{"index.html": "c", "404.html": "custom 404"})
	h.deploy("both", map[string]string{"200.html": "shell", "404.html": "nf"})

	tests := []struct {
		name   string
		url    string
		status int
		body   string
		ctype  string
		loc    string
	}{
		{"root index", "https://foo.backyard.test/", 200, "root index", "text/html; charset=utf-8", ""},
		{"explicit index", "https://foo.backyard.test/index.html", 200, "root index", "text/html; charset=utf-8", ""},
		{"dir with slash", "https://foo.backyard.test/docs/", 200, "docs index", "text/html; charset=utf-8", ""},
		{"dir without slash", "https://foo.backyard.test/docs?x=1&y=2", 301, "", "", "/docs/?x=1&y=2"},
		{"clean url", "https://foo.backyard.test/about", 200, "about page", "text/html; charset=utf-8", ""},
		{"clean url nested", "https://foo.backyard.test/blog/post", 200, "post", "text/html; charset=utf-8", ""},
		{"exact html", "https://foo.backyard.test/about.html", 200, "about page", "text/html; charset=utf-8", ""},
		{"js", "https://foo.backyard.test/app.js", 200, "console.log(1)", "text/javascript; charset=utf-8", ""},
		{"css", "https://foo.backyard.test/style.css", 200, "body{}", "text/css; charset=utf-8", ""},
		{"json", "https://foo.backyard.test/data.json", 200, `{"a":1}`, "application/json", ""},
		{"png", "https://foo.backyard.test/img/logo.png", 200, "PNG", "image/png", ""},
		{"no extension", "https://foo.backyard.test/README", 200, "plain", "application/octet-stream", ""},
		{"missing builtin 404", "https://foo.backyard.test/nope", 404, "Page not found", "text/html; charset=utf-8", ""},
		{"missing dir slash", "https://foo.backyard.test/nope/", 404, "Page not found", "text/html; charset=utf-8", ""},
		{"spa fallback", "https://spa.backyard.test/some/route", 200, "spa shell", "text/html; charset=utf-8", ""},
		{"spa root", "https://spa.backyard.test/", 200, "spa root", "text/html; charset=utf-8", ""},
		{"custom 404", "https://custom.backyard.test/missing", 404, "custom 404", "text/html; charset=utf-8", ""},
		{"200 wins over 404", "https://both.backyard.test/x", 200, "shell", "text/html; charset=utf-8", ""},
		{"index missing falls back", "https://both.backyard.test/", 200, "shell", "text/html; charset=utf-8", ""},
		{"unknown site", "https://ghost.backyard.test/", 404, "Site not found", "text/html; charset=utf-8", ""},
		{"unknown site deep", "https://ghost.backyard.test/a/b", 404, "There is no site named", "text/html; charset=utf-8", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := h.get(tc.url)
			expectStatus(t, w, tc.status)
			if tc.loc != "" {
				if got := w.Header().Get("Location"); got != tc.loc {
					t.Fatalf("Location = %q, want %q", got, tc.loc)
				}
				return
			}
			if !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.body)
			}
			if got := w.Header().Get("Content-Type"); got != tc.ctype {
				t.Fatalf("Content-Type = %q, want %q", got, tc.ctype)
			}
		})
	}

	// Built-in pages link to the hub.
	w := h.get("https://ghost.backyard.test/")
	if !strings.Contains(w.Body.String(), `href="https://backyard.test/"`) {
		t.Fatalf("site-not-found page lacks hub link: %s", w.Body.String())
	}
}

func TestStaticHeaders(t *testing.T) {
	h := newHarness(t)
	mod := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h.deploy("foo", map[string]string{"index.html": "hello world"})
	w := h.get("https://foo.backyard.test/")
	expectStatus(t, w, 200)
	hd := w.Header()
	want := map[string]string{
		"Cache-Control":          "no-cache",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
		"Last-Modified":          mod.Format(http.TimeFormat),
		"Content-Length":         "11",
		"Accept-Ranges":          "bytes",
	}
	for k, v := range want {
		if got := hd.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if et := hd.Get("ETag"); !strings.HasPrefix(et, `"`) || len(et) < 4 {
		t.Errorf("ETag = %q", et)
	}
	if hd.Get("X-Frame-Options") != "" {
		t.Error("X-Frame-Options must not be set")
	}
	// 404.html responses are not cacheable and carry no validators.
	h.deploy("custom", map[string]string{"404.html": "nf"})
	w = h.get("https://custom.backyard.test/x")
	expectStatus(t, w, 404)
	if w.Header().Get("ETag") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("404 headers: %v", w.Header())
	}
}

func TestConditionalAndRange(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"file.txt": "0123456789"})
	u := "https://foo.backyard.test/file.txt"
	w := h.get(u)
	etag := w.Header().Get("ETag")
	lm := w.Header().Get("Last-Modified")

	tests := []struct {
		name    string
		hdr     []string
		method  string
		status  int
		body    string
		crange  string
		clength string
	}{
		{"if-none-match", []string{"If-None-Match", etag}, "GET", 304, "", "", ""},
		{"if-none-match list weak", []string{"If-None-Match", `"x", W/` + etag}, "GET", 304, "", "", ""},
		{"if-none-match star", []string{"If-None-Match", "*"}, "GET", 304, "", "", ""},
		{"if-none-match miss", []string{"If-None-Match", `"other"`}, "GET", 200, "0123456789", "", "10"},
		{"if-modified-since", []string{"If-Modified-Since", lm}, "GET", 304, "", "", ""},
		{"if-modified-since old", []string{"If-Modified-Since", "Mon, 01 Jan 2024 00:00:00 GMT"}, "GET", 200, "0123456789", "", ""},
		{"inm beats ims", []string{"If-None-Match", `"other"`, "If-Modified-Since", lm}, "GET", 200, "0123456789", "", ""},
		{"if-match fail", []string{"If-Match", `"other"`}, "GET", 412, "", "", ""},
		{"if-match ok", []string{"If-Match", etag}, "GET", 200, "0123456789", "", ""},
		{"range", []string{"Range", "bytes=2-5"}, "GET", 206, "2345", "bytes 2-5/10", "4"},
		{"range open", []string{"Range", "bytes=7-"}, "GET", 206, "789", "bytes 7-9/10", "3"},
		{"range suffix", []string{"Range", "bytes=-3"}, "GET", 206, "789", "bytes 7-9/10", "3"},
		{"range suffix large", []string{"Range", "bytes=-30"}, "GET", 206, "0123456789", "bytes 0-9/10", "10"},
		{"range clamp end", []string{"Range", "bytes=8-100"}, "GET", 206, "89", "bytes 8-9/10", "2"},
		{"range unsatisfiable", []string{"Range", "bytes=10-"}, "GET", 416, "", "bytes */10", ""},
		{"range multi ignored", []string{"Range", "bytes=0-1,4-5"}, "GET", 200, "0123456789", "", "10"},
		{"range malformed ignored", []string{"Range", "bytes=abc"}, "GET", 200, "0123456789", "", ""},
		{"range other unit ignored", []string{"Range", "items=0-1"}, "GET", 200, "0123456789", "", ""},
		{"if-range match", []string{"Range", "bytes=0-0", "If-Range", etag}, "GET", 206, "0", "bytes 0-0/10", "1"},
		{"if-range mismatch", []string{"Range", "bytes=0-0", "If-Range", `"stale"`}, "GET", 200, "0123456789", "", ""},
		{"if-range date", []string{"Range", "bytes=0-0", "If-Range", lm}, "GET", 206, "0", "", ""},
		{"head", nil, "HEAD", 200, "", "", "10"},
		{"head range", []string{"Range", "bytes=0-3"}, "HEAD", 206, "", "bytes 0-3/10", "4"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := h.serve(req{method: tc.method, url: u, hdr: pairs(tc.hdr...)})
			expectStatus(t, w, tc.status)
			if tc.status == 304 || tc.method == "HEAD" {
				if w.Body.Len() != 0 {
					t.Fatalf("unexpected body %q", w.Body.String())
				}
			} else if tc.body != "" && w.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.body)
			}
			if tc.crange != "" && w.Header().Get("Content-Range") != tc.crange {
				t.Fatalf("Content-Range = %q, want %q", w.Header().Get("Content-Range"), tc.crange)
			}
			if tc.clength != "" && w.Header().Get("Content-Length") != tc.clength {
				t.Fatalf("Content-Length = %q, want %q", w.Header().Get("Content-Length"), tc.clength)
			}
			if tc.status == 304 && w.Header().Get("ETag") != etag {
				t.Fatalf("304 without ETag")
			}
		})
	}
}

func TestLargeObjectStreamingAndRange(t *testing.T) {
	h := newHarness(t)
	big := bytes.Repeat([]byte("abcdefghij"), (maxCachedObject/10)+100) // > 1 MiB
	h.blob.set(sites.SiteKey("foo", "big.bin"), big, time.Now())
	u := "https://foo.backyard.test/big.bin"

	w := h.get(u)
	expectStatus(t, w, 200)
	if !bytes.Equal(w.Body.Bytes(), big) {
		t.Fatalf("body mismatch: %d bytes", w.Body.Len())
	}
	w = h.get(u, "Range", "bytes=10-19")
	expectStatus(t, w, 206)
	if w.Body.String() != "abcdefghij" {
		t.Fatalf("range body = %q", w.Body.String())
	}
	_, _, ranges := h.blob.counts()
	if ranges != 1 {
		t.Fatalf("GetRange calls = %d, want 1", ranges)
	}
	// HEAD of a large object never opens it.
	_, getsBefore, _ := h.blob.counts()
	w = h.serve(req{method: "HEAD", url: u})
	expectStatus(t, w, 200)
	if _, gets, _ := h.blob.counts(); gets != getsBefore {
		t.Fatalf("HEAD opened the object")
	}
	if w.Header().Get("Content-Length") != strconv.Itoa(len(big)) {
		t.Fatalf("Content-Length = %s", w.Header().Get("Content-Length"))
	}
}

func TestStaticMethodNotAllowed(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "x"})
	w := h.serve(req{method: "POST", url: "https://foo.backyard.test/"})
	expectStatus(t, w, 405)
	if got := w.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}
}

func TestStaticCache(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "v1"})
	cache := h.srv.cache
	now := time.Now()
	cache.now = func() time.Time { return now }

	for range 3 {
		if w := h.get("https://foo.backyard.test/"); w.Body.String() != "v1" {
			t.Fatalf("body = %q", w.Body.String())
		}
	}
	stats, gets, _ := h.blob.counts()
	if stats != 1 || gets != 1 {
		t.Fatalf("blob calls stat=%d get=%d, want 1/1 (cached)", stats, gets)
	}
	// Overwritten in place: still cached until the TTL passes.
	h.deploy("foo", map[string]string{"index.html": "v2"})
	if w := h.get("https://foo.backyard.test/"); w.Body.String() != "v1" {
		t.Fatalf("body = %q, want cached v1", w.Body.String())
	}
	now = now.Add(h.cfg.Storage.CacheTTL.D() + time.Millisecond)
	if w := h.get("https://foo.backyard.test/"); w.Body.String() != "v2" {
		t.Fatalf("body = %q, want v2 after TTL", w.Body.String())
	}
	// Negative lookups are cached too.
	h.get("https://foo.backyard.test/missing")
	s1, _, _ := h.blob.counts()
	h.get("https://foo.backyard.test/missing")
	s2, _, _ := h.blob.counts()
	if s1 != s2 {
		t.Fatalf("negative lookups not cached: %d → %d stats", s1, s2)
	}
	// PurgeSite drops entries immediately.
	h.deploy("foo", map[string]string{"index.html": "v3"})
	h.srv.PurgeSite("foo")
	if w := h.get("https://foo.backyard.test/"); w.Body.String() != "v3" {
		t.Fatalf("body = %q after purge", w.Body.String())
	}
}

func TestStaticCacheDisabled(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.Storage.CacheBytes = -1 }))
	if h.srv.cache != nil {
		t.Fatal("cache should be disabled")
	}
	h.deploy("foo", map[string]string{"index.html": "v1"})
	h.get("https://foo.backyard.test/")
	h.deploy("foo", map[string]string{"index.html": "v2"})
	if w := h.get("https://foo.backyard.test/"); w.Body.String() != "v2" {
		t.Fatalf("body = %q", w.Body.String())
	}
	w := h.get("https://foo.backyard.test/", "Range", "bytes=0-0")
	expectStatus(t, w, 206)
	if w.Body.String() != "v" {
		t.Fatalf("range body = %q", w.Body.String())
	}
}

func TestObjectCacheLRU(t *testing.T) {
	c := newObjectCache(1000, time.Minute)
	c.put(&cacheEntry{key: "a", data: make([]byte, 200)})
	c.put(&cacheEntry{key: "b", data: make([]byte, 200)})
	if _, ok := c.get("a"); !ok { // touch a → b is LRU
		t.Fatal("a missing")
	}
	c.put(&cacheEntry{key: "c", data: make([]byte, 200)})
	if _, ok := c.get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Fatal("a evicted")
	}
	if _, used := c.size(); used > 1000 {
		t.Fatalf("used %d > max", used)
	}
	// Oversized entries are not cached.
	c.put(&cacheEntry{key: "huge", data: make([]byte, 2000)})
	if _, ok := c.get("huge"); ok {
		t.Fatal("oversized entry cached")
	}
	c.purgePrefix("a")
	if _, ok := c.get("a"); ok {
		t.Fatal("purge failed")
	}
	var nilCache *objectCache
	nilCache.put(&cacheEntry{key: "x"})
	if _, ok := nilCache.get("x"); ok {
		t.Fatal("nil cache returned an entry")
	}
}

func TestDevSites(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	write := func(p, c string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "dev v1")
	write("about.html", "about")
	write("sub/index.html", "sub")
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(h *harness, d *Deps) { d.DevSites = map[string]string{"local": dir} })

	w := h.get("https://local.backyard.test/")
	if w.Code != 200 || w.Body.String() != "dev v1" {
		t.Fatalf("dev root: %d %q", w.Code, w.Body.String())
	}
	write("index.html", "dev v2") // live, no cache
	if w := h.get("https://local.backyard.test/"); w.Body.String() != "dev v2" {
		t.Fatalf("not live: %q", w.Body.String())
	}
	if w := h.get("https://local.backyard.test/about"); w.Body.String() != "about" {
		t.Fatalf("clean url: %q", w.Body.String())
	}
	if w := h.get("https://local.backyard.test/sub"); w.Code != 301 || w.Header().Get("Location") != "/sub/" {
		t.Fatalf("dir redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := h.get("https://local.backyard.test/index.html", "Range", "bytes=4-5"); w.Code != 206 || w.Body.String() != "v2" {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	for _, p := range []string{"/link.txt", "/linkdir/secret.txt", "/%2e%2e/" + filepath.Base(outside) + "/secret.txt"} {
		w := h.get("https://local.backyard.test" + p)
		if strings.Contains(w.Body.String(), "secret") && w.Code == 200 {
			t.Fatalf("%s escaped the dev directory: %d %q", p, w.Code, w.Body.String())
		}
	}
	// Missing file in an existing dev site → page not found (site exists).
	w = h.get("https://local.backyard.test/missing")
	expectStatus(t, w, 404)
	if !strings.Contains(w.Body.String(), "Page not found") {
		t.Fatalf("body = %q", w.Body.String())
	}
	// GET /site is synthesized for undeployed dev sites.
	w = h.api("GET", "https://local.backyard.test/__backyard/api/site", nil)
	expectStatus(t, w, 200)
	m := decode[map[string]any](t, w)
	if m["name"] != "local" || m["url"] != "https://local.backyard.test/" {
		t.Fatalf("site meta = %v", m)
	}
	// Dev sites appear in the hub list.
	w = h.api("GET", "https://backyard.test/__backyard/api/sites", nil)
	if !strings.Contains(w.Body.String(), `"name":"local"`) {
		t.Fatalf("sites = %s", w.Body.String())
	}
}
