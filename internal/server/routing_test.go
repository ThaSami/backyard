package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
)

func TestNewValidation(t *testing.T) {
	base := func() Deps {
		h := newHarness(t)
		return h.srv.deps
	}
	tests := []struct {
		name   string
		mutate func(*Deps)
		want   string
	}{
		{"no config", func(d *Deps) { d.Config = nil }, "Config"},
		{"no blob", func(d *Deps) { d.Blob = nil }, "Blob"},
		{"no docs", func(d *Deps) { d.Docs = nil }, "Docs"},
		{"no sites", func(d *Deps) { d.Sites = nil }, "Sites"},
		{"no auth", func(d *Deps) { d.Auth = nil }, "Auth"},
		{"bad public url", func(d *Deps) {
			c := *d.Config
			c.Server.PublicURL = "backyard.test"
			d.Config = &c
		}, "public_url"},
		{"no base domain", func(d *Deps) {
			c := *d.Config
			c.Server.BaseDomain = ""
			d.Config = &c
		}, "base_domain"},
		{"bad proxy", func(d *Deps) {
			c := *d.Config
			c.Server.TrustedProxies = []string{"nope"}
			d.Config = &c
		}, "trusted_proxies"},
		{"bad dev site", func(d *Deps) { d.DevSites = map[string]string{"Bad_Name": t.TempDir()} }, "dev site"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := base()
			tc.mutate(&d)
			_, err := New(d)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestHostRouting(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "foo index"})
	tests := []struct {
		name   string
		url    string
		host   string
		status int
		body   string
	}{
		{"hub", "https://backyard.test/", "", 200, "hub index"},
		{"hub with port", "https://backyard.test:8443/", "", 200, "hub index"},
		{"hub uppercase", "https://backyard.test/", "BACKYARD.TEST", 200, "hub index"},
		{"hub trailing dot", "https://backyard.test/", "backyard.test.", 200, "hub index"},
		{"site", "https://foo.backyard.test/", "", 200, "foo index"},
		{"site with port", "https://foo.backyard.test:8443/", "", 200, "foo index"},
		{"site uppercase", "https://foo.backyard.test/", "FOO.Backyard.Test", 200, "foo index"},
		{"unknown site", "https://bar.backyard.test/", "", 404, "Site not found"},
		{"reserved site", "https://www.backyard.test/", "", 404, "Site not found"},
		{"invalid label", "https://foo_bar.backyard.test/", "", 404, "Site not found"},
		{"nested subdomain", "https://a.foo.backyard.test/", "", 404, "Unknown host"},
		{"other domain", "https://evil.example/", "", 404, "Unknown host"},
		{"suffix trick", "https://foobackyard.test/", "", 404, "Unknown host"},
		{"healthz unknown host", "https://10.1.2.3/__backyard/healthz", "", 200, "ok"},
		{"healthz site", "https://foo.backyard.test/__backyard/healthz", "", 200, "ok"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hdr []string
			if tc.host != "" {
				hdr = []string{"Host", tc.host}
			}
			w := h.get(tc.url, hdr...)
			expectStatus(t, w, tc.status)
			if !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("body %q does not contain %q", w.Body.String(), tc.body)
			}
		})
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "x"})
	for _, u := range []string{"https://foo.backyard.test/", "https://backyard.test/", "https://foo.backyard.test/missing", "https://nope.example/"} {
		w := h.get(u)
		if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", u, got)
		}
		if got := w.Header().Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
			t.Errorf("%s: Referrer-Policy = %q", u, got)
		}
		if got := w.Header().Get("X-Frame-Options"); got != "" {
			t.Errorf("%s: X-Frame-Options = %q, want none", u, got)
		}
		if got := w.Header().Get("X-Request-Id"); len(got) < 8 {
			t.Errorf("%s: X-Request-Id = %q", u, got)
		}
	}
}

func TestRequestID(t *testing.T) {
	h := newHarness(t)
	w := h.get("https://backyard.test/__backyard/healthz", "X-Request-Id", "abc-123_def.456")
	if got := w.Header().Get("X-Request-Id"); got != "abc-123_def.456" {
		t.Fatalf("request id = %q", got)
	}
	w = h.get("https://backyard.test/__backyard/healthz", "X-Request-Id", "bad id with spaces")
	if got := w.Header().Get("X-Request-Id"); got == "bad id with spaces" || len(got) != 24 {
		t.Fatalf("request id = %q, want a generated one", got)
	}
}

func TestDevModeUnknownHostIsHub(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.Server.Dev = true }))
	w := h.get("http://127.0.0.1:8080/")
	expectStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), "hub index") {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestTrustedProxyHeaders(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
	}))
	h.deploy("foo", map[string]string{"index.html": "foo index"})

	// Trusted proxy: X-Forwarded-Host selects the site.
	w := h.serve(req{url: "http://internal:8080/", remote: "10.1.1.1:5555", hdr: map[string]string{
		"X-Forwarded-Host": "foo.backyard.test", "X-Forwarded-Proto": "https",
	}})
	expectStatus(t, w, 200)
	if w.Body.String() != "foo index" {
		t.Fatalf("body = %q", w.Body.String())
	}
	// Untrusted peer: the header is ignored → unknown host.
	w = h.serve(req{url: "http://internal:8080/", remote: "192.0.2.9:5555", hdr: map[string]string{
		"X-Forwarded-Host": "foo.backyard.test",
	}})
	expectStatus(t, w, 404)

	// X-Forwarded-Proto decides the request origin (CSRF check).
	w = h.serve(req{method: "POST", url: "http://foo.backyard.test/__backyard/api/db/posts", remote: "10.1.1.1:5555",
		body: strings.NewReader(`{"a":1}`),
		hdr:  map[string]string{"X-Backyard": "1", "Origin": "http://foo.backyard.test", "X-Forwarded-Proto": "http"}})
	expectStatus(t, w, 201)
}

func TestForwardedFor(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8", "192.168.1.1"}
	}))
	tests := []struct {
		remote string
		xff    []string
		want   string
	}{
		{"10.0.0.1:1", []string{"203.0.113.7"}, "203.0.113.7"},
		{"10.0.0.1:1", []string{"198.51.100.1, 203.0.113.7, 10.0.0.2"}, "203.0.113.7"},
		{"10.0.0.1:1", []string{"198.51.100.1", "203.0.113.7"}, "203.0.113.7"},
		{"10.0.0.1:1", []string{"10.0.0.3, 192.168.1.1"}, "10.0.0.3"},
		{"10.0.0.1:1", []string{"garbage, 203.0.113.7"}, "203.0.113.7"},
		{"10.0.0.1:1", nil, "10.0.0.1"},
		{"192.0.2.1:1", []string{"203.0.113.7"}, "192.0.2.1"},
		{"[2001:db8::1]:443", nil, "2001:db8::1"},
	}
	for _, tc := range tests {
		r, _ := http.NewRequest("GET", "https://backyard.test/", nil)
		r.RemoteAddr = tc.remote
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		ri := &reqInfo{}
		h.srv.resolveClient(ri, r)
		if ri.clientIP != tc.want {
			t.Errorf("remote %s xff %v: client ip = %s, want %s", tc.remote, tc.xff, ri.clientIP, tc.want)
		}
	}
}

func TestUncleanPathRedirect(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		target string
		loc    string
	}{
		{"/a//b", "/a/b"},
		{"/a/./b/", "/a/b/"},
		{"/a/../__backyard/api/me", "/__backyard/api/me"},
	}
	for _, tc := range tests {
		r := httptestRequest("GET", "https://foo.backyard.test"+tc.target)
		r.URL.Path = tc.target
		r.URL.RawPath = ""
		w := serveRaw(h, r)
		expectStatus(t, w, http.StatusMovedPermanently)
		if got := w.Header().Get("Location"); got != tc.loc {
			t.Errorf("%s: Location = %q, want %q", tc.target, got, tc.loc)
		}
	}
	r := httptestRequest("POST", "https://foo.backyard.test/x")
	r.URL.Path = "/a/../__backyard/api/db/x"
	w := serveRaw(h, r)
	expectStatus(t, w, http.StatusBadRequest)
}

func TestPathModeRouting(t *testing.T) {
	h := newHarness(t, pathMode())
	h.deploy("foo", map[string]string{"index.html": "foo index", "about.html": "about", "docs/index.html": "docs"})

	w := h.get("https://backyard.test/s/foo?x=1")
	expectStatus(t, w, http.StatusMovedPermanently)
	if got := w.Header().Get("Location"); got != "/s/foo/?x=1" {
		t.Fatalf("Location = %q", got)
	}
	w = h.get("https://backyard.test/s/foo/")
	expectStatus(t, w, 200)
	if w.Body.String() != "foo index" {
		t.Fatalf("body = %q", w.Body.String())
	}
	w = h.get("https://backyard.test/s/foo/about")
	if w.Code != 200 || w.Body.String() != "about" {
		t.Fatalf("clean url: %d %q", w.Code, w.Body.String())
	}
	w = h.get("https://backyard.test/s/foo/docs?q=2")
	expectStatus(t, w, http.StatusMovedPermanently)
	if got := w.Header().Get("Location"); got != "/s/foo/docs/?q=2" {
		t.Fatalf("Location = %q", got)
	}
	w = h.get("https://backyard.test/s/")
	expectStatus(t, w, 404)
	w = h.get("https://backyard.test/s/nope/")
	expectStatus(t, w, 404)
	if !strings.Contains(w.Body.String(), "Site not found") {
		t.Fatalf("body = %q", w.Body.String())
	}
	// Hub still works.
	w = h.get("https://backyard.test/")
	if !strings.Contains(w.Body.String(), "hub index") {
		t.Fatalf("hub body = %q", w.Body.String())
	}
	// Site subdomains are not sites in path mode.
	w = h.get("https://foo.backyard.test/")
	expectStatus(t, w, 404)

	// Site API under the prefix.
	w = h.api("POST", "https://backyard.test/s/foo/__backyard/api/db/posts", map[string]any{"t": 1}, "Origin", "https://backyard.test")
	expectStatus(t, w, 201)
	if n := h.docs.collectionLen("foo", "posts"); n != 1 {
		t.Fatalf("docs in foo/posts = %d", n)
	}
	// No site at the root API.
	w = h.api("GET", "https://backyard.test/__backyard/api/db", nil)
	expectError(t, w, 404, "not_found")
}

func TestReadyz(t *testing.T) {
	h := newHarness(t)
	w := h.get("https://anything.example/__backyard/readyz")
	expectStatus(t, w, 200)
	h.docs.pingErr = errors.New("db down: secret-host:5432")
	w = h.get("https://backyard.test/__backyard/readyz")
	expectStatus(t, w, http.StatusServiceUnavailable)
	if strings.Contains(w.Body.String(), "secret-host") || !strings.Contains(w.Body.String(), "database") {
		t.Fatalf("body = %q", w.Body.String())
	}
	h.docs.pingErr = nil
	h.blob.pingErr = errors.New("bucket gone")
	w = h.get("https://backyard.test/__backyard/readyz")
	expectStatus(t, w, http.StatusServiceUnavailable)
	if !strings.Contains(w.Body.String(), "storage") {
		t.Fatalf("body = %q", w.Body.String())
	}
	// Health checks never authenticate.
	h.auth.set(nil, errors.New("must not be called"))
	h.blob.pingErr = nil
	if w := h.get("https://backyard.test/__backyard/healthz"); w.Code != 200 {
		t.Fatalf("healthz = %d", w.Code)
	}
}

func TestReservedPrefixNotShadowed(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"__backyard/secret.txt": "shadow", "index.html": "x"})
	w := h.get("https://foo.backyard.test/__backyard/secret.txt")
	expectError(t, w, 404, "not_found")
	w = h.get("https://backyard.test/__backyard/nope")
	expectError(t, w, 404, "not_found")
}

func TestAccessLog(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.Telemetry.AccessLog = true }))
	h.get("https://backyard.test/__backyard/healthz?token=secret")
	logs := h.logText()
	if !strings.Contains(logs, "http request") || !strings.Contains(logs, "/__backyard/healthz") {
		t.Fatalf("access log missing: %s", logs)
	}
	if strings.Contains(logs, "token=secret") {
		t.Fatalf("query string logged: %s", logs)
	}
}
