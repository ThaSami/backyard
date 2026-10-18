package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

const previewSVG = `<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720"><rect width="1280" height="720" fill="#eee"/></svg>`

// putPreviewSite stores site with its files and a meta document naming
// preview (the deploy paths compute it; the route only reads it).
func (h *harness) putPreviewSite(site, preview string, files map[string]string) {
	h.t.Helper()
	h.deploy(site, files)
	m := &sites.Meta{Name: site, Title: site, Preview: preview, CreatedAt: time.Now(), UpdatedAt: time.Now(), FileCount: len(files)}
	if err := h.srv.deps.Sites.PutMeta(context.Background(), m); err != nil {
		h.t.Fatal(err)
	}
}

func previewURL(site string) string {
	return "https://backyard.test/__backyard/api/sites/" + site + "/preview"
}

func TestHubPreview(t *testing.T) {
	h := newHarness(t)
	h.putPreviewSite("demo", "img/cover.svg", map[string]string{"index.html": "<title>Demo</title>", "img/cover.svg": previewSVG})

	w := h.get(previewURL("demo"))
	expectStatus(t, w, http.StatusOK)
	if w.Body.String() != previewSVG {
		t.Fatalf("body = %q", w.Body.String())
	}
	hdr := w.Header()
	want := map[string]string{
		"Content-Type":            "image/svg+xml",
		"Content-Security-Policy": sites.PreviewCSP,
		"X-Content-Type-Options":  "nosniff",
		"Cache-Control":           "private, no-cache",
		"Content-Length":          strconv.Itoa(len(previewSVG)),
	}
	for k, v := range want {
		if got := hdr.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if hdr.Get("Content-Security-Policy") != "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'" {
		t.Errorf("CSP = %q", hdr.Get("Content-Security-Policy"))
	}
	etag := hdr.Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 3 {
		t.Errorf("ETag = %q, want a strong ETag", etag)
	}
	for _, k := range []string{"Content-Disposition", "X-Frame-Options", "Access-Control-Allow-Origin", "Set-Cookie"} {
		if v := hdr.Get(k); v != "" {
			t.Errorf("unexpected %s: %q", k, v)
		}
	}

	// A version makes it cacheable for a year; an empty one does not.
	w = h.get(previewURL("demo") + "?v=2026-10-18T12%3A00%3A00.000000Z")
	expectStatus(t, w, http.StatusOK)
	if got := w.Header().Get("Cache-Control"); got != "private, max-age=31536000, immutable" {
		t.Errorf("versioned Cache-Control = %q", got)
	}
	if w.Header().Get("ETag") != etag || w.Header().Get("Content-Security-Policy") != sites.PreviewCSP {
		t.Errorf("versioned headers = %v", w.Header())
	}
	for _, q := range []string{"?v=", "?x=1", "?V=1"} {
		w = h.get(previewURL("demo") + q)
		expectStatus(t, w, http.StatusOK)
		if got := w.Header().Get("Cache-Control"); got != "private, no-cache" {
			t.Errorf("%s: Cache-Control = %q", q, got)
		}
	}

	// Conditional GET.
	w = h.get(previewURL("demo"), "If-None-Match", etag)
	expectStatus(t, w, http.StatusNotModified)
	if w.Body.Len() != 0 || w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "private, no-cache" {
		t.Errorf("304 = %v %q", w.Header(), w.Body.String())
	}
	w = h.get(previewURL("demo")+"?v=1", "If-None-Match", `"other", `+etag)
	expectStatus(t, w, http.StatusNotModified)
	if w.Header().Get("Cache-Control") != "private, max-age=31536000, immutable" {
		t.Errorf("versioned 304 Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	expectStatus(t, h.get(previewURL("demo"), "If-None-Match", `"other"`), http.StatusOK)

	// HEAD: the headers of GET, no body.
	w = h.serve(req{method: http.MethodHead, url: previewURL("demo")})
	expectStatus(t, w, http.StatusOK)
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(previewSVG)) ||
		w.Header().Get("Content-Type") != "image/svg+xml" || w.Header().Get("Content-Security-Policy") != sites.PreviewCSP {
		t.Errorf("HEAD = %v %q", w.Header(), w.Body.String())
	}

	// A single byte range, as for any served object.
	w = h.get(previewURL("demo"), "Range", "bytes=0-3")
	expectStatus(t, w, http.StatusPartialContent)
	if w.Body.String() != previewSVG[:4] || w.Header().Get("Content-Security-Policy") != sites.PreviewCSP {
		t.Errorf("range = %q %v", w.Body.String(), w.Header())
	}

	// The listing carries the field the hub keys the request on.
	w = h.api("GET", "https://backyard.test/__backyard/api/sites", nil)
	expectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"preview":"img/cover.svg"`) {
		t.Errorf("sites = %s", w.Body.String())
	}
}

func TestHubPreviewContentType(t *testing.T) {
	h := newHarness(t)
	png := "\x89PNG\r\n\x1a\nfake"
	// The stored type is ignored: the extension decides.
	h.deploy("pics", map[string]string{"index.html": "x"})
	if _, err := h.blob.Put(context.Background(), sites.SiteKey("pics", "Shot.PNG"), strings.NewReader(png), int64(len(png)), "text/html"); err != nil {
		t.Fatal(err)
	}
	h.putPreviewSite("pics", "Shot.PNG", map[string]string{"index.html": "x"})
	for ext, ct := range sites.PreviewTypes {
		site := "t-" + ext
		h.putPreviewSite(site, "preview."+ext, map[string]string{"preview." + ext: "img"})
		w := h.get(previewURL(site))
		expectStatus(t, w, http.StatusOK)
		if got := w.Header().Get("Content-Type"); got != ct {
			t.Errorf("%s: Content-Type = %q, want %q", ext, got, ct)
		}
	}
	w := h.get(previewURL("pics"))
	expectStatus(t, w, http.StatusOK)
	if got := w.Header().Get("Content-Type"); got != "image/png" || w.Body.String() != png {
		t.Errorf("Content-Type = %q, body %q", got, w.Body.String())
	}
}

func TestHubPreviewNotFound(t *testing.T) {
	h := newHarness(t)
	h.deploy("synced", map[string]string{"index.html": "x", "preview.png": "png"}) // no meta document
	h.putPreviewSite("plain", "", map[string]string{"index.html": "x", "preview.png": "png"})
	h.putPreviewSite("gone", "preview.png", map[string]string{"index.html": "x"})
	h.putPreviewSite("html", "index.html", map[string]string{"index.html": "<script>alert(1)</script>"})
	h.putPreviewSite("bmp", "a.bmp", map[string]string{"a.bmp": "bmp"})
	h.putPreviewSite("noext", "cover", map[string]string{"cover": "x"})
	h.putPreviewSite("escape", "../plain/preview.png", map[string]string{"index.html": "x"})
	h.putPreviewSite("absolute", "/preview.png", map[string]string{"preview.png": "png"})
	if err := h.blob.Delete(context.Background(), sites.SiteKey("gone", "index.html")); err != nil {
		t.Fatal(err)
	}
	h.blob.set(sites.MetaKey("corrupt"), []byte("{not json"), time.Now())
	h.deploy("corrupt", map[string]string{"preview.png": "png"})

	for _, site := range []string{"nope", "synced", "plain", "gone", "html", "bmp", "noext", "escape", "absolute", "corrupt", "Bad_Name", "a%2Fb", "-x"} {
		w := h.get(previewURL(site))
		expectError(t, w, http.StatusNotFound, "not_found")
		if w.Header().Get("Content-Security-Policy") != "" || strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("%s: 404 headers = %v", site, w.Header())
		}
		w = h.serve(req{method: http.MethodHead, url: previewURL(site)})
		expectStatus(t, w, http.StatusNotFound)
	}
	// Neighbouring paths are no endpoints.
	for _, u := range []string{
		"https://backyard.test/__backyard/api/sites/plain",
		"https://backyard.test/__backyard/api/sites/plain/preview/x",
		"https://backyard.test/__backyard/api/sites/plain/previews",
	} {
		expectError(t, h.get(u), http.StatusNotFound, "not_found")
	}
}

func TestHubPreviewMethods(t *testing.T) {
	h := newHarness(t)
	h.putPreviewSite("demo", "preview.svg", map[string]string{"preview.svg": previewSVG})
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		w := h.api(m, previewURL("demo"), nil)
		expectError(t, w, http.StatusMethodNotAllowed, "bad_request")
		if got := w.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("%s: Allow = %q", m, got)
		}
	}
	// Like every hub API route, the site API host does not have it.
	expectError(t, h.get("https://demo.backyard.test/__backyard/api/sites/demo/preview"), http.StatusNotFound, "not_found")
}

func TestHubPreviewPathMode(t *testing.T) {
	h := newHarness(t, pathMode())
	h.putPreviewSite("demo", "preview.svg", map[string]string{"preview.svg": previewSVG})
	w := h.get(previewURL("demo"))
	expectStatus(t, w, http.StatusOK)
	if w.Body.String() != previewSVG || w.Header().Get("Content-Security-Policy") != sites.PreviewCSP {
		t.Fatalf("path mode = %d %v", w.Code, w.Header())
	}
	expectError(t, h.get("https://backyard.test/s/demo/__backyard/api/sites/demo/preview"), http.StatusNotFound, "not_found")
}

func TestHubPreviewAuth(t *testing.T) {
	h := newHarness(t)
	h.putPreviewSite("demo", "preview.svg", map[string]string{"preview.svg": previewSVG})

	// Guests (auth.mode none) see it like the site list.
	h.auth.set(guest, nil)
	expectStatus(t, h.get(previewURL("demo")), http.StatusOK)

	// Without credentials an API route answers 401 JSON, never a redirect
	// (an <img> then shows the placeholder).
	h.auth.set(nil, auth.ErrUnauthenticated)
	e := expectError(t, h.get(previewURL("demo")), http.StatusUnauthorized, "unauthenticated")
	if e.Error.LoginURL == "" {
		t.Errorf("no login_url")
	}
	h.auth.loginBase = ""
	expectError(t, h.get(previewURL("demo")), http.StatusUnauthorized, "unauthenticated")
	h.auth.set(nil, auth.ErrForbidden)
	expectError(t, h.get(previewURL("demo")), http.StatusForbidden, "forbidden")
}

func TestHubPreviewRealAuthModes(t *testing.T) {
	// auth.mode none: a guest is minted, as for /__backyard/api/sites.
	none := newHarness(t, func(h *harness, d *Deps) {
		h.cfg.Auth.Mode = "none"
		a, err := auth.New(t.Context(), h.cfg, auth.Options{})
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
		d.Auth = a
	})
	none.putPreviewSite("demo", "preview.svg", map[string]string{"preview.svg": previewSVG})
	w := none.get(previewURL("demo"))
	expectStatus(t, w, http.StatusOK)
	if w.Body.String() != previewSVG {
		t.Fatalf("none: body %q", w.Body.String())
	}

	// auth.mode header: the proxy's identity is required.
	hdr := newHarness(t, func(h *harness, d *Deps) {
		h.cfg.Auth.Mode = "header"
		h.cfg.Auth.Header.EmailHeader = "X-Email"
		h.cfg.Server.TrustedProxies = []string{"192.0.2.0/24"} // httptest's RemoteAddr
		a, err := auth.New(t.Context(), h.cfg, auth.Options{})
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
		d.Auth = a
	})
	hdr.putPreviewSite("demo", "preview.svg", map[string]string{"preview.svg": previewSVG})
	expectStatus(t, hdr.get(previewURL("demo"), "X-Email", "alice@acme.com"), http.StatusOK)
	expectError(t, hdr.get(previewURL("demo")), http.StatusUnauthorized, "unauthenticated")
}

func TestHubPreviewDevSites(t *testing.T) {
	dir := t.TempDir()
	for p, c := range map[string]string{"index.html": `<meta property="og:image" content="preview.svg">`, "preview.svg": previewSVG} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := newHarness(t, func(h *harness, d *Deps) { d.DevSites = map[string]string{"local": dir, "shadow": dir} })
	// A stored site of the same name is shadowed by the dev site.
	h.putPreviewSite("shadow", "preview.svg", map[string]string{"preview.svg": previewSVG})
	for _, site := range []string{"local", "shadow"} {
		expectError(t, h.get(previewURL(site)), http.StatusNotFound, "not_found")
	}
	w := h.api("GET", "https://backyard.test/__backyard/api/sites", nil)
	expectStatus(t, w, http.StatusOK)
	if strings.Contains(w.Body.String(), `"preview"`) {
		t.Errorf("dev sites listed with a preview: %s", w.Body.String())
	}
}

func TestHubPreviewCacheAndStorage(t *testing.T) {
	h := newHarness(t)
	h.putPreviewSite("demo", "preview.svg", map[string]string{"preview.svg": previewSVG})
	first := h.get(previewURL("demo"))
	expectStatus(t, first, http.StatusOK)

	// A deploy purges the site from the cache: the new bytes and ETag show.
	v2 := strings.Replace(previewSVG, "#eee", "#ddd", 1)
	h.putPreviewSite("demo", "preview.svg", map[string]string{"preview.svg": v2})
	h.srv.PurgeSite("demo")
	w := h.get(previewURL("demo"))
	if w.Body.String() != v2 || w.Header().Get("ETag") == first.Header().Get("ETag") {
		t.Errorf("after purge: %q %q", w.Body.String(), w.Header().Get("ETag"))
	}
	expectStatus(t, h.get(previewURL("demo"), "If-None-Match", first.Header().Get("ETag")), http.StatusOK)

	// Storage failures are not a 404.
	h.srv.PurgeSite("demo")
	h.blob.mu.Lock()
	h.blob.statHook = func(key string) error {
		if strings.HasSuffix(key, "/preview.svg") {
			return context.DeadlineExceeded
		}
		return nil
	}
	h.blob.mu.Unlock()
	w = h.get(previewURL("demo"))
	if w.Code == http.StatusOK || w.Code == http.StatusNotFound || !bytes.Contains(w.Body.Bytes(), []byte(`"error"`)) {
		t.Errorf("storage failure = %d %s", w.Code, w.Body.String())
	}
}

// The 1 MiB cap (SPEC §7.2) holds when serving, not only when the preview
// is chosen: bytes stored under the preview path by a deploy that was never
// committed (uploads replace files in place, SPEC §7.3) or by a direct sync
// are not served when they exceed it.
func TestHubPreviewSizeCap(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []option
	}{
		{"cached", nil},
		{"uncached", []option{withConfig(func(c *config.Config) { c.Storage.CacheBytes = -1 })}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			h.putPreviewSite("demo", "cover.svg", map[string]string{"index.html": "x", "cover.svg": previewSVG})
			expectStatus(t, h.get(previewURL("demo")), http.StatusOK)
			key := sites.SiteKey("demo", "cover.svg")

			// At the cap: served.
			atCap := strings.Repeat("x", sites.MaxPreviewBytes)
			h.blob.set(key, []byte(atCap), time.Now())
			h.srv.PurgeSite("demo")
			for _, q := range []string{"", "?v=1"} {
				w := h.get(previewURL("demo") + q)
				expectStatus(t, w, http.StatusOK)
				if w.Body.Len() != sites.MaxPreviewBytes || w.Header().Get("Content-Length") != strconv.Itoa(sites.MaxPreviewBytes) {
					t.Fatalf("%q: at the cap = %d bytes, Content-Length %q", q, w.Body.Len(), w.Header().Get("Content-Length"))
				}
			}

			// One byte over, while the meta still names the file: the 404.
			h.blob.set(key, []byte(atCap+"x"), time.Now())
			h.srv.PurgeSite("demo")
			for _, q := range []string{"", "?v=1"} {
				for _, rq := range []req{
					{url: previewURL("demo") + q},
					{url: previewURL("demo") + q, hdr: map[string]string{"Range": "bytes=0-3"}},
					{method: http.MethodHead, url: previewURL("demo") + q},
				} {
					w := h.serve(rq)
					if w.Code != http.StatusNotFound {
						t.Fatalf("%s %q %v: over the cap = %d, %d bytes", rq.method, q, rq.hdr, w.Code, w.Body.Len())
					}
					if rq.method == "" {
						expectError(t, w, http.StatusNotFound, "not_found")
					}
					if w.Header().Get("Content-Security-Policy") != "" || strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
						t.Errorf("%q: 404 headers = %v", q, w.Header())
					}
				}
			}
			// The site host still serves the file itself, as it serves any file.
			if w := h.get("https://demo.backyard.test/cover.svg"); w.Code != http.StatusOK || w.Body.Len() != sites.MaxPreviewBytes+1 {
				t.Errorf("site host = %d, %d bytes", w.Code, w.Body.Len())
			}
		})
	}
}

// A versioned preview is cached by browsers for a year, so it must show the
// bytes of the version it names. It is read from storage, not through the
// object cache: a redeploy through another instance, or a standalone
// gateway, does not purge this instance's cache, and even locally the purge
// runs after the meta is written.
func TestHubPreviewVersionedReadsStorage(t *testing.T) {
	a := newHarness(t)
	shared := a.blob
	b := newHarness(t, func(h *harness, d *Deps) { h.blob = shared; d.Blob = shared })
	now := time.Now()
	a.srv.cache.now = func() time.Time { return now } // A's cache entries do not expire in this test

	ctx := context.Background()
	key := sites.SiteKey("demo", "preview.svg")
	oldSVG, newSVG := "<svg>OLD</svg>", "<svg>NEW</svg>"
	t1 := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	t2 := t1.Add(2 * time.Second)
	commit := func(updated time.Time) {
		t.Helper()
		if err := b.srv.deps.Sites.PutMeta(ctx, &sites.Meta{Name: "demo", Preview: "preview.svg", CreatedAt: t1, UpdatedAt: updated, FileCount: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// version is what the hub puts in ?v=: the listed updated_at.
	version := func(h *harness) string {
		t.Helper()
		w := h.get("https://backyard.test/__backyard/api/sites")
		expectStatus(t, w, http.StatusOK)
		var out struct {
			Sites []struct {
				Name      string `json:"name"`
				UpdatedAt string `json:"updated_at"`
			} `json:"sites"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, s := range out.Sites {
			if s.Name == "demo" && s.UpdatedAt != "" {
				return s.UpdatedAt
			}
		}
		t.Fatalf("demo not listed: %s", w.Body.String())
		return ""
	}

	shared.set(key, []byte(oldSVG), t1)
	commit(t1)
	v1 := version(a)
	// A serves the first version; its object cache now holds the file.
	if w := a.get(previewURL("demo")); w.Body.String() != oldSVG {
		t.Fatalf("A unversioned = %q", w.Body.String())
	}
	w := a.get(previewURL("demo") + "?v=" + url.QueryEscape(v1))
	expectStatus(t, w, http.StatusOK)
	oldETag := w.Header().Get("ETag")
	if w.Body.String() != oldSVG || w.Header().Get("Cache-Control") != previewCacheVersioned {
		t.Fatalf("A v1 = %q %v", w.Body.String(), w.Header())
	}

	// Redeploy through B: uploads first, then the meta, then B's own purge.
	shared.set(key, []byte(newSVG), t2)
	commit(t2)
	b.srv.PurgeSite("demo")
	v2 := version(a)
	if v2 == v1 {
		t.Fatalf("version unchanged: %q", v2)
	}
	// Premise: A's cache still has the old bytes. The unversioned response
	// is no-cache and heals within storage.cache_ttl.
	if w := a.get(previewURL("demo")); w.Body.String() != oldSVG || w.Header().Get("Cache-Control") != previewCacheUnversioned {
		t.Fatalf("A unversioned after redeploy = %q %v, want the cached old bytes", w.Body.String(), w.Header())
	}
	// The new version URL gets the new bytes on every instance.
	for _, tc := range []struct {
		name string
		h    *harness
	}{{"A", a}, {"B", b}} {
		u := previewURL("demo") + "?v=" + url.QueryEscape(v2)
		w := tc.h.get(u)
		expectStatus(t, w, http.StatusOK)
		if w.Body.String() != newSVG || w.Header().Get("Cache-Control") != previewCacheVersioned ||
			w.Header().Get("ETag") == oldETag || w.Header().Get("Content-Security-Policy") != sites.PreviewCSP {
			t.Errorf("%s v2 = %q %v", tc.name, w.Body.String(), w.Header())
		}
		// A browser revalidating with the old ETag gets the new object, not a 304.
		expectStatus(t, tc.h.get(u, "If-None-Match", oldETag), http.StatusOK)
		w = tc.h.serve(req{method: http.MethodHead, url: u})
		if w.Code != http.StatusOK || w.Header().Get("Content-Length") != strconv.Itoa(len(newSVG)) {
			t.Errorf("%s HEAD v2 = %d %v", tc.name, w.Code, w.Header())
		}
	}
}
