package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
)

func TestWSHandoff(t *testing.T) {
	tests := []struct {
		name    string
		opts    []option
		url     string
		origin  string
		site    string
		origins []string
	}{
		{"host mode", nil, "https://foo.backyard.test/__backyard/api/ws", "", "foo",
			[]string{"https://foo.backyard.test"}},
		{"host mode with port", nil, "https://foo.backyard.test:8443/__backyard/api/ws", "", "foo",
			[]string{"https://foo.backyard.test", "https://foo.backyard.test:8443"}},
		{"path mode", []option{pathMode()}, "https://backyard.test/s/foo/__backyard/api/ws", "", "foo",
			[]string{"https://backyard.test"}},
		{"cross-site denied by default", nil, "https://foo.backyard.test/__backyard/api/ws", "https://bar.backyard.test", "foo",
			[]string{"https://foo.backyard.test"}},
		{"cross-site allowed", []option{withConfig(func(c *config.Config) { c.Realtime.AllowCrossSiteWS = true })},
			"https://foo.backyard.test/__backyard/api/ws", "https://bar.backyard.test", "foo",
			[]string{"https://foo.backyard.test", "https://bar.backyard.test"}},
		{"cross-site foreign origin", []option{withConfig(func(c *config.Config) { c.Realtime.AllowCrossSiteWS = true })},
			"https://foo.backyard.test/__backyard/api/ws", "https://evil.example", "foo",
			[]string{"https://foo.backyard.test"}},
		{"cross-site wrong scheme", []option{withConfig(func(c *config.Config) { c.Realtime.AllowCrossSiteWS = true })},
			"https://foo.backyard.test/__backyard/api/ws", "http://bar.backyard.test", "foo",
			[]string{"https://foo.backyard.test"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.opts...)
			var hdr []string
			if tc.origin != "" {
				hdr = []string{"Origin", tc.origin}
			}
			w := h.get(tc.url, hdr...)
			expectStatus(t, w, http.StatusTeapot)
			calls := h.hub.wsCalls()
			if len(calls) != 1 {
				t.Fatalf("ServeWS calls = %d", len(calls))
			}
			c := calls[0]
			if c.site != tc.site || c.user == nil || c.user.ID != "alice@acme.com" {
				t.Fatalf("call = %+v", c)
			}
			if !slices.Equal(c.origins, tc.origins) {
				t.Fatalf("origins = %v, want %v", c.origins, tc.origins)
			}
		})
	}
	h := newHarness(t, func(h *harness, d *Deps) { d.Realtime = nil })
	expectError(t, h.get("https://foo.backyard.test/__backyard/api/ws"), http.StatusNotImplemented, "unsupported")
	h = newHarness(t)
	h.auth.set(nil, auth.ErrUnauthenticated)
	expectError(t, h.get("https://foo.backyard.test/__backyard/api/ws"), http.StatusUnauthorized, "unauthenticated")
	if len(h.hub.wsCalls()) != 0 {
		t.Fatal("unauthenticated websocket handed off")
	}
}

func scrape(t *testing.T, h *harness) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.srv.MetricsHandler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Fatalf("metrics status %d", w.Code)
	}
	return w.Body.String()
}

func TestMetrics(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "x"})
	h.hub.conns = 3
	h.get("https://foo.backyard.test/")
	h.get("https://foo.backyard.test/missing")
	h.api("POST", dbURL+"/m", map[string]any{"a": 1})
	h.api("GET", dbURL+"/m/nope", nil)
	h.api("POST", aiURL+"/chat", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "x"}}, "model": "smart"})
	h.api("POST", whURL+"/query", map[string]any{"sql": "SELECT 1"})
	h.srv.OnWSFrame("in")
	h.srv.OnWSFrame("out")
	h.srv.OnWSFrame("out")
	h.srv.OnWSFrame("bogus")

	out := scrape(t, h)
	for _, want := range []string{
		`backyard_http_requests_total{area="static",code="200"} 1`,
		`backyard_http_requests_total{area="static",code="404"} 1`,
		`backyard_http_requests_total{area="api",code="201"} 1`,
		`backyard_http_request_duration_seconds_bucket{area="static"`,
		`backyard_db_ops_total{op="create",result="ok"} 1`,
		`backyard_db_ops_total{op="get",result="not_found"} 1`,
		`backyard_ai_requests_total{model="smart",result="ok"} 1`,
		`backyard_ai_tokens_total{direction="input",model="smart"} 12`,
		`backyard_ai_tokens_total{direction="output",model="smart"} 40`,
		`backyard_warehouse_queries_total{result="ok",source="unknown"} 1`,
		`backyard_ws_connections 3`,
		`backyard_ws_messages_total{dir="in"} 1`,
		`backyard_ws_messages_total{dir="out"} 2`,
		`backyard_build_info{version="test"} 1`,
		`go_goroutines`,
		`process_`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	if strings.Contains(out, "bogus") {
		t.Error("unknown ws direction recorded")
	}

	// Not exposed on the hub unless telemetry.metrics && no metrics_listen.
	w := h.get("https://backyard.test/__backyard/metrics")
	expectError(t, w, 404, "not_found")
	on := newHarness(t, withConfig(func(c *config.Config) { c.Telemetry.Metrics = true }))
	w = on.get("https://backyard.test/__backyard/metrics")
	expectStatus(t, w, 200)
	if !strings.Contains(w.Body.String(), "backyard_build_info") {
		t.Fatalf("hub metrics = %s", truncate(w.Body.String(), 300))
	}
	// Sites never expose metrics.
	expectError(t, on.get("https://foo.backyard.test/__backyard/metrics"), 404, "not_found")
	// Authentication is required.
	on.auth.set(nil, auth.ErrUnauthenticated)
	expectError(t, on.get("https://backyard.test/__backyard/metrics"), 401, "unauthenticated")
	sep := newHarness(t, withConfig(func(c *config.Config) {
		c.Telemetry.Metrics = true
		c.Telemetry.MetricsListen = ":9090"
	}))
	expectError(t, sep.get("https://backyard.test/__backyard/metrics"), 404, "not_found")
}

func TestPanicRecovery(t *testing.T) {
	h := newHarness(t)
	h.gw.panicMsg = "gateway exploded"
	w := h.get("https://backyard.test/__backyard/api/info")
	e := expectError(t, w, http.StatusInternalServerError, "internal")
	if strings.Contains(e.Error.Message, "exploded") {
		t.Fatalf("panic value leaked: %q", e.Error.Message)
	}
	if !strings.Contains(h.logText(), "gateway exploded") {
		t.Fatalf("panic not logged")
	}
	// HTML page for browser routes.
	h.auth.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999")
		panic("auth exploded")
	})
	w = h.get("https://backyard.test/__backyard/auth/login")
	expectStatus(t, w, http.StatusInternalServerError)
	if !strings.Contains(w.Header().Get("Content-Type"), "text/html") || w.Header().Get("Content-Length") == "999" {
		t.Fatalf("headers = %v", w.Header())
	}
	if w.Header().Get("X-Request-Id") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("global headers lost: %v", w.Header())
	}

	// A panic after the response started aborts the connection.
	h.auth.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "partial")
		panic("late")
	})
	srv := httptest.NewServer(h.h)
	defer srv.Close()
	r, _ := http.NewRequest("GET", srv.URL+"/__backyard/auth/x", nil)
	r.Host = "backyard.test"
	resp, err := srv.Client().Do(r)
	if err == nil {
		_, err = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("expected a truncated response")
	}
	out := scrape(t, h)
	if !strings.Contains(out, `backyard_http_requests_total{area="auth",code="500"}`) {
		t.Fatalf("panic not counted: %s", out)
	}
}

func TestAPIRateLimitPerUserAndIP(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.APIPerMinute = 2 }))
	me := "https://foo.backyard.test/__backyard/api/me"
	expectStatus(t, h.get(me), 200)
	expectStatus(t, h.get(me), 200)
	w := h.get(me)
	expectError(t, w, http.StatusTooManyRequests, "rate_limited")
	if ra := w.Header().Get("Retry-After"); ra != "30" {
		t.Fatalf("Retry-After = %q, want 30", ra)
	}
	// Other users have their own bucket; static files are not API calls.
	h.auth.set(alice.Clone(), nil)
	other := alice.Clone()
	other.ID = "bob@acme.com"
	h.auth.set(other, nil)
	expectStatus(t, h.get(me), 200)

	// Anonymous users are keyed by client IP.
	g := newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.APIPerMinute = 1 }))
	g.auth.set(guest, nil)
	expectStatus(t, g.serve(req{url: me, remote: "198.51.100.1:1"}), 200)
	expectStatus(t, g.serve(req{url: me, remote: "198.51.100.1:2"}), http.StatusTooManyRequests)
	expectStatus(t, g.serve(req{url: me, remote: "198.51.100.2:1"}), 200)

	// Disabled.
	d := newHarness(t, withConfig(func(c *config.Config) {
		c.RateLimit.APIPerMinute = 1
		c.RateLimit.Disabled = true
	}))
	for range 5 {
		expectStatus(t, d.get(me), 200)
	}
	if !strings.Contains(scrape(t, h), `backyard_rate_limited_total{category="api"} 1`) {
		t.Fatal("rejection not counted")
	}
}

func TestStorageErrors(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "x"})
	h.blob.statHook = func(string) error { return errors.New("s3 down at bucket-internal") }
	w := h.get("https://foo.backyard.test/")
	expectStatus(t, w, http.StatusInternalServerError)
	if strings.Contains(w.Body.String(), "bucket-internal") || !strings.Contains(w.Body.String(), "request id") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if !strings.Contains(h.logText(), "bucket-internal") {
		t.Fatal("storage error not logged")
	}
	h.blob.statHook = nil

	h.blob.putErr = errors.New("disk full")
	res := h.upload(filesURL, part{field: "file", filename: "a.txt", content: "abc"})
	if res.code != http.StatusInternalServerError {
		t.Fatalf("upload with failing store = %d %s", res.code, res.body)
	}
	h.blob.putErr = nil

	before := len(h.blob.keys())
	h.docs.fail("create", docstore.ErrQuota)
	res = h.upload(filesURL, part{field: "file", filename: "a.txt", content: "abc"})
	if res.code != http.StatusRequestEntityTooLarge || len(h.blob.keys()) != before {
		t.Fatalf("quota upload = %d, blobs %d → %d", res.code, before, len(h.blob.keys()))
	}
	w = h.api("POST", aiURL+"/image", map[string]any{"prompt": "x"})
	expectError(t, w, http.StatusRequestEntityTooLarge, "too_large")
	h.docs.fail("create", nil)

	h.srv.ObserveDeploy("s3", "ok")
	if !strings.Contains(scrape(t, h), `backyard_deploys_total{method="s3",result="ok"} 1`) {
		t.Fatal("ObserveDeploy not counted")
	}
}
