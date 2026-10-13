package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
)

func TestAuthBrowserRedirect(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "x"})
	h.auth.set(nil, auth.ErrUnauthenticated)

	w := h.get("https://foo.backyard.test/page?a=1&b=2")
	expectStatus(t, w, http.StatusFound)
	want := hubURL + "/__backyard/auth/login?rd=https://foo.backyard.test/page?a=1&b=2"
	if got := w.Header().Get("Location"); got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	// Path mode keeps the /s/<site> prefix in the return URL.
	p := newHarness(t, pathMode())
	p.auth.set(nil, auth.ErrUnauthenticated)
	w = p.get("https://backyard.test/s/foo/x")
	expectStatus(t, w, http.StatusFound)
	if got := w.Header().Get("Location"); !strings.HasSuffix(got, "rd=https://backyard.test/s/foo/x") {
		t.Fatalf("Location = %q", got)
	}
	// Hub pages and file downloads redirect too.
	for _, u := range []string{"https://backyard.test/", "https://backyard.test/docs", "https://foo.backyard.test/__backyard/files/01ABC/x.png"} {
		if w := h.get(u); w.Code != http.StatusFound {
			t.Errorf("%s: status %d, want 302", u, w.Code)
		}
	}
	// HEAD redirects as well; POST gets a 401 page.
	if w := h.serve(req{method: "HEAD", url: "https://foo.backyard.test/"}); w.Code != http.StatusFound {
		t.Fatalf("HEAD status = %d", w.Code)
	}
}

func TestAuthNoLoginURL(t *testing.T) {
	h := newHarness(t)
	h.auth.loginBase = ""
	h.auth.set(nil, auth.ErrUnauthenticated)
	w := h.get("https://foo.backyard.test/")
	expectStatus(t, w, http.StatusUnauthorized)
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "Sign in required") {
		t.Fatalf("body = %q", w.Body.String())
	}
	w = h.api("GET", "https://foo.backyard.test/__backyard/api/me", nil)
	e := expectError(t, w, http.StatusUnauthorized, "unauthenticated")
	if e.Error.LoginURL != "" {
		t.Fatalf("login_url = %q, want none", e.Error.LoginURL)
	}
	if strings.Contains(w.Body.String(), "login_url") {
		t.Fatalf("login_url must be omitted: %s", w.Body.String())
	}
}

func TestAuthAPI401(t *testing.T) {
	h := newHarness(t)
	h.auth.set(nil, auth.ErrUnauthenticated)
	tests := []struct {
		name    string
		referer string
		wantRD  string
	}{
		{"no referer", "", "https://foo.backyard.test/"},
		{"same-origin referer", "https://foo.backyard.test/app?x=1", "https://foo.backyard.test/app?x=1"},
		{"foreign referer", "https://evil.example/", "https://foo.backyard.test/"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hdr []string
			if tc.referer != "" {
				hdr = []string{"Referer", tc.referer}
			}
			w := h.get("https://foo.backyard.test/__backyard/api/db", hdr...)
			e := expectError(t, w, http.StatusUnauthorized, "unauthenticated")
			if want := hubURL + "/__backyard/auth/login?rd=" + tc.wantRD; e.Error.LoginURL != want {
				t.Fatalf("login_url = %q, want %q", e.Error.LoginURL, want)
			}
		})
	}
	// Browser asking for JSON on a page gets JSON? No: pages always redirect
	// for GET. A POST to a page with Accept: application/json gets JSON 401.
	h.loginBase(hubURL + "/__backyard/auth/login")
	w := h.serve(req{method: "POST", url: "https://foo.backyard.test/x", hdr: map[string]string{"Accept": "application/json"}})
	expectStatus(t, w, http.StatusMethodNotAllowed)
}

func (h *harness) loginBase(s string) {
	h.auth.mu.Lock()
	defer h.auth.mu.Unlock()
	h.auth.loginBase = s
}

func TestAuthForbiddenAndErrors(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "x"})
	h.auth.set(nil, auth.ErrForbidden)
	w := h.get("https://foo.backyard.test/")
	expectStatus(t, w, http.StatusForbidden)
	if !strings.Contains(w.Body.String(), "Access denied") {
		t.Fatalf("body = %q", w.Body.String())
	}
	w = h.api("GET", "https://foo.backyard.test/__backyard/api/me", nil)
	expectError(t, w, http.StatusForbidden, "forbidden")

	h.auth.set(nil, errors.New("jwks fetch failed"))
	w = h.api("GET", "https://foo.backyard.test/__backyard/api/me", nil)
	e := expectError(t, w, http.StatusInternalServerError, "internal")
	if strings.Contains(e.Error.Message, "jwks") {
		t.Fatalf("internal error leaked: %q", e.Error.Message)
	}
}

func TestAuthRoutesOnSiteRedirectToHub(t *testing.T) {
	h := newHarness(t)
	w := h.get("https://foo.backyard.test/__backyard/auth/logout?x=1")
	expectStatus(t, w, http.StatusFound)
	if got := w.Header().Get("Location"); got != hubURL+"/__backyard/auth/logout?x=1" {
		t.Fatalf("Location = %q", got)
	}
	w = h.get("https://foo.backyard.test/__backyard/auth/login")
	loc := w.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil || u.Host != "backyard.test" || u.Path != "/__backyard/auth/login" || u.Query().Get("rd") != "https://foo.backyard.test/" {
		t.Fatalf("Location = %q", loc)
	}
	// On the hub, the auth handler serves them (no authentication first).
	h.auth.set(nil, auth.ErrUnauthenticated)
	w = h.get("https://backyard.test/__backyard/auth/callback?code=1")
	expectStatus(t, w, 200)
	if w.Body.String() != "auth handler /__backyard/auth/callback" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestCSRF(t *testing.T) {
	host := newHarness(t)
	path := newHarness(t, pathMode())
	tests := []struct {
		name   string
		h      *harness
		url    string
		hdr    map[string]string
		status int
	}{
		{"missing header", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{}, 403},
		{"wrong header", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "true"}, 403},
		{"header no origin", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "1"}, 201},
		{"same origin", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "https://foo.backyard.test"}, 201},
		{"same origin default port", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "https://FOO.backyard.test:443"}, 201},
		{"other site", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "https://bar.backyard.test"}, 403},
		{"hub origin host mode", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "https://backyard.test"}, 403},
		{"http scheme", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "http://foo.backyard.test"}, 403},
		{"null origin", host, "https://foo.backyard.test/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "null"}, 403},
		{"path mode hub origin", path, "https://backyard.test/s/foo/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "https://backyard.test"}, 201},
		{"path mode foreign", path, "https://backyard.test/s/foo/__backyard/api/db/posts", map[string]string{"X-Backyard": "1", "Origin": "https://evil.example"}, 403},
		{"hub tokens no header", host, "https://backyard.test/__backyard/api/tokens", map[string]string{"Origin": "https://backyard.test"}, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.h.serve(req{method: "POST", url: tc.url, body: strings.NewReader(`{"a":1}`), hdr: tc.hdr})
			expectStatus(t, w, tc.status)
			if tc.status == 403 {
				expectError(t, w, 403, "forbidden")
			}
		})
	}
	// GET needs neither header nor origin check.
	w := host.get("https://foo.backyard.test/__backyard/api/db", "Origin", "https://evil.example")
	expectStatus(t, w, 200)
}

func TestUserTouchedInCache(t *testing.T) {
	h := newHarness(t)
	w := h.api("GET", "https://foo.backyard.test/__backyard/api/me", nil)
	expectStatus(t, w, 200)
	u := decode[map[string]any](t, w)
	if u["id"] != "alice@acme.com" || u["name"] != "Alice" {
		t.Fatalf("me = %v", u)
	}
	waitBackground(t, h)
	if n := h.docs.collectionLen("_system", "_users"); n != 1 {
		t.Fatalf("user cache entries = %d, want 1", n)
	}
	// Lookup through the API.
	w = h.api("GET", "https://foo.backyard.test/__backyard/api/users/ALICE@acme.com", nil)
	expectStatus(t, w, 200)
	if got := decode[map[string]any](t, w); got["email"] != "alice@acme.com" {
		t.Fatalf("lookup = %v", got)
	}
	w = h.api("GET", "https://foo.backyard.test/__backyard/api/users/nobody@acme.com", nil)
	expectError(t, w, 404, "not_found")
	w = h.api("GET", "https://foo.backyard.test/__backyard/api/users/not-an-email", nil)
	expectError(t, w, 400, "bad_request")

	// Anonymous users are never cached.
	g := newHarness(t)
	g.auth.set(guest, nil)
	g.api("GET", "https://foo.backyard.test/__backyard/api/me", nil)
	waitBackground(t, g)
	if n := g.docs.collectionLen("_system", "_users"); n != 0 {
		t.Fatalf("anonymous user cached")
	}
}

func waitBackground(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.srv.bg.wait(ctx); err != nil {
		t.Fatalf("background work: %v", err)
	}
}

func TestSiteMeta(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "<title>Foo</title>"})
	w := h.api("GET", "https://foo.backyard.test/__backyard/api/site", nil)
	expectStatus(t, w, 200)
	m := decode[map[string]any](t, w)
	if m["name"] != "foo" || m["url"] != "https://foo.backyard.test/" {
		t.Fatalf("site = %v", m)
	}
	if owners, ok := m["owners"].([]any); !ok || owners == nil {
		t.Fatalf("owners = %v", m["owners"])
	}
	w = h.api("GET", "https://ghost.backyard.test/__backyard/api/site", nil)
	expectError(t, w, 404, "not_found")
}

func TestAPIRouting(t *testing.T) {
	h := newHarness(t)
	w := h.api("GET", "https://foo.backyard.test/__backyard/api/nope", nil)
	expectError(t, w, 404, "not_found")
	w = h.api("DELETE", "https://foo.backyard.test/__backyard/api/me", nil)
	expectError(t, w, 405, "bad_request")
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q", allow)
	}
	w = h.api("GET", "https://foo.backyard.test/__backyard/api", nil)
	expectError(t, w, 404, "not_found")
	// Hub-only endpoints are not on sites and vice versa.
	w = h.api("GET", "https://foo.backyard.test/__backyard/api/sites", nil)
	expectError(t, w, 404, "not_found")
	w = h.api("GET", "https://backyard.test/__backyard/api/db", nil)
	expectError(t, w, 404, "not_found")
}

func TestRealtimeDisabledByConfig(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.Realtime.Disabled = true }))
	w := h.api("GET", "https://foo.backyard.test/__backyard/api/ws", nil)
	expectError(t, w, http.StatusNotImplemented, "unsupported")
}
