package server

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

const (
	testSecret = "0123456789abcdef0123456789abcdef-test"
	hubURL     = "https://backyard.test"
)

var testHubFS = fstest.MapFS{
	"index.html":       {Data: []byte("<!doctype html><title>Hub</title>hub index")},
	"docs.html":        {Data: []byte("<!doctype html><title>Docs</title>api docs")},
	"assets/app.js":    {Data: []byte("console.log('hub')")},
	"assets/app.css":   {Data: []byte("body{}")},
	"guide/index.html": {Data: []byte("guide")},
}

func TestMain(m *testing.M) {
	sdkClassic = func() []byte { return []byte("/* classic sdk */\n") }
	sdkModule = func() []byte { return []byte("/* module sdk */\nexport default {};\n") }
	sdkTypes = func() []byte { return []byte("declare const backyard: any;\n") }
	hubFS = func() fs.FS { return testHubFS }
	skillMarkdown = func() string { return "# Backyard skill\n" }
	skillReference = func(name string) string {
		switch name {
		case "api":
			return "# API reference\n"
		case "recipes":
			return "# Recipes\n"
		}
		return ""
	}
	llmsTxt = func(publicURL string) string { return "# Backyard\n> " + publicURL + "/llms-full.txt\n" }
	llmsFullTxt = func(publicURL string) string { return "# Backyard full\n" + publicURL + "\n" }
	os.Exit(m.Run())
}

var (
	alice = &identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice", Groups: []string{"eng"}}
	guest = &identity.User{ID: "anon:0123456789abcdef", Name: "Guest 0123", Anonymous: true}
)

// testConfig returns a defaulted host-mode configuration.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.BaseDomain = "backyard.test"
	cfg.Server.PublicURL = hubURL
	cfg.Server.Secret = testSecret
	cfg.Auth.Mode = "oidc"
	cfg.Storage.Type = "fs"
	cfg.Storage.FS.Root = t.TempDir()
	cfg.Database.Type = "memory"
	cfg.ApplyDefaults()
	return cfg
}

type harness struct {
	t     *testing.T
	cfg   *config.Config
	blob  *memBlob
	docs  *memDocs
	auth  *fakeAuth
	hub   *fakeHub
	ai    *fakeAI
	wh    *fakeWarehouse
	gw    *fakeGateway
	users *identity.UserCache
	srv   *Server
	h     http.Handler
	logs  *strings.Builder
	logMu *lockedWriter
}

// logText returns everything logged so far.
func (h *harness) logText() string {
	h.logMu.mu.Lock()
	defer h.logMu.mu.Unlock()
	return h.logs.String()
}

type option func(h *harness, d *Deps)

func withConfig(f func(*config.Config)) option {
	return func(h *harness, d *Deps) { f(h.cfg) }
}

func pathMode() option {
	return withConfig(func(c *config.Config) { c.Server.URLMode = "path" })
}

func newHarness(t *testing.T, opts ...option) *harness {
	t.Helper()
	h := &harness{
		t:    t,
		cfg:  testConfig(t),
		blob: newMemBlob(),
		docs: newMemDocs(),
		auth: &fakeAuth{user: alice, loginBase: hubURL + "/__backyard/auth/login"},
		hub:  &fakeHub{},
		ai:   newFakeAI(),
		wh:   &fakeWarehouse{enabled: true, res: &warehouseResultFixture},
		gw:   &fakeGateway{},
		logs: &strings.Builder{},
	}
	h.users = identity.NewUserCache(h.docs, nil)
	h.logMu = &lockedWriter{w: h.logs}
	d := Deps{
		Logger:    slog.New(slog.NewTextHandler(h.logMu, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Blob:      h.blob,
		Docs:      h.docs,
		Auth:      h.auth,
		Users:     h.users,
		Realtime:  h.hub,
		AI:        h.ai,
		Warehouse: h.wh,
		Gateway:   h.gw,
		Version:   "test",
	}
	for _, o := range opts {
		o(h, &d)
	}
	d.Config = h.cfg
	if d.Tokens == nil {
		d.Tokens = auth.NewTokens(h.cfg.Server.Secret, h.cfg.Auth.TokenTTL.D())
	}
	d.Sites = sites.NewRegistry(h.blob, h.cfg)
	srv, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.srv = srv
	h.h = srv.Handler()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
	})
	return h
}

// lockedWriter serializes log writes from concurrent goroutines.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// deploy writes site files into the fake blob store.
func (h *harness) deploy(site string, files map[string]string) {
	for p, c := range files {
		h.blob.set(sites.SiteKey(site, p), []byte(c), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	}
}

type req struct {
	method string
	url    string
	body   io.Reader
	hdr    map[string]string
	remote string
}

func (h *harness) serve(rq req) *httptest.ResponseRecorder {
	h.t.Helper()
	if rq.method == "" {
		rq.method = http.MethodGet
	}
	r := httptest.NewRequest(rq.method, rq.url, rq.body)
	for k, v := range rq.hdr {
		if strings.EqualFold(k, "Host") {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	if rq.remote != "" {
		r.RemoteAddr = rq.remote
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func (h *harness) get(url string, hdr ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	return h.serve(req{url: url, hdr: pairs(hdr...)})
}

// api sends a JSON API request with the CSRF header and a same-origin Origin.
func (h *harness) api(method, url string, body any, hdr ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		case []byte:
			rd = strings.NewReader(string(b))
		default:
			buf, err := json.Marshal(b)
			if err != nil {
				h.t.Fatal(err)
			}
			rd = strings.NewReader(string(buf))
		}
	}
	m := map[string]string{"X-Backyard": "1", "Content-Type": "application/json"}
	for k, v := range pairs(hdr...) {
		m[k] = v
	}
	return h.serve(req{method: method, url: url, body: rd, hdr: m})
}

func pairs(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

// decode parses a JSON response body.
func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return v
}

type errResp struct {
	Error struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		LoginURL string `json:"login_url"`
	} `json:"error"`
}

// expectError asserts a JSON error response.
func expectError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) errResp {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body %s", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want JSON; body %s", ct, w.Body.String())
	}
	e := decode[errResp](t, w)
	if e.Error.Code != code {
		t.Fatalf("error code = %q, want %q (%s)", e.Error.Code, code, e.Error.Message)
	}
	if e.Error.Message == "" {
		t.Fatalf("empty error message")
	}
	return e
}

func expectStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; headers %v; body %s", w.Code, status, w.Header(), truncate(w.Body.String(), 500))
	}
}

func httptestRequest(method, url string) *http.Request {
	return httptest.NewRequest(method, url, nil)
}

func serveRaw(h *harness, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r)
	return w
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
