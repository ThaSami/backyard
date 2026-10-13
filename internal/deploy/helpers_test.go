package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/sites/sitestest"
)

const testSecret = "test-secret-0123456789abcdef0123456789"

// fakeAuth is a scriptable auth.Authenticator.
type fakeAuth struct {
	mode       string
	users      map[string]*identity.User // bearer token → user
	bearerOnly bool                      // like auth.NewBearer: no guests
	loginURL   string                    // "" → LoginURL returns ""
	failWith   error                     // returned for every request when set
}

var _ auth.Authenticator = (*fakeAuth)(nil)

func (a *fakeAuth) Authenticate(w http.ResponseWriter, r *http.Request) (*identity.User, error) {
	if a.failWith != nil {
		return nil, a.failWith
	}
	if h := r.Header.Get("Authorization"); h != "" {
		tok := strings.TrimPrefix(h, "Bearer ")
		if tok == "forbidden" {
			return nil, auth.ErrForbidden
		}
		if u, ok := a.users[tok]; ok {
			c := *u
			return &c, nil
		}
		// Like the real authenticator: an invalid bearer token is an error
		// in every mode (never silently a guest).
		return nil, auth.ErrUnauthenticated
	}
	if a.mode == auth.ModeNone && !a.bearerOnly {
		id := "0123456789abcdef"
		if c, err := r.Cookie(auth.AnonCookie); err == nil {
			id = c.Value
		} else if w != nil {
			http.SetCookie(w, &http.Cookie{Name: auth.AnonCookie, Value: id, Path: "/"})
		}
		return &identity.User{ID: "anon:" + id, Name: "Guest", Anonymous: true}, nil
	}
	return nil, auth.ErrUnauthenticated
}

func (a *fakeAuth) LoginURL(returnTo string) string {
	if a.loginURL == "" {
		return ""
	}
	return a.loginURL + "?rd=" + url.QueryEscape(returnTo)
}

func (a *fakeAuth) Handler() http.Handler { return http.NotFoundHandler() }
func (a *fakeAuth) Mode() string          { return a.mode }

// Well-known test identities (token → user).
var (
	alice = &identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice"}
	bob   = &identity.User{ID: "bob@acme.com", Email: "bob@acme.com", Name: "Bob"}
	admin = &identity.User{ID: "root@acme.com", Email: "root@acme.com", Name: "Root", Admin: true}
	carol = &identity.User{ID: "carol@acme.com", Email: "carol@acme.com", Name: "Carol", Groups: []string{"ops"}}
	ci    = &identity.User{ID: "token:ci", Name: "ci"}
	// guestTok is the identity of an API token minted for a guest in
	// auth.mode none (valid, but anonymous).
	guestTok = &identity.User{ID: "anon:feedfacefeedface", Name: "Guest feed", Anonymous: true}
)

func defaultUsers() map[string]*identity.User {
	return map[string]*identity.User{"alice": alice, "bob": bob, "admin": admin, "carol": carol, "ci": ci, "guest": guestTok}
}

// env is a running gateway with an in-memory store.
type env struct {
	t         *testing.T
	cfg       *config.Config
	store     *sitestest.MemStore // nil when another blob.Store is used
	blobStore blob.Store
	reg       *sites.Registry
	auth      *fakeAuth
	gw        *Gateway
	srv       *httptest.Server
	deleteMu  sync.Mutex
	deleted   []string
	hookErr   error
	logs      *lockedWriter
}

type envOption func(*env)

func withConfig(fn func(*config.Config)) envOption { return func(e *env) { fn(e.cfg) } }

func withAuth(fn func(*fakeAuth)) envOption { return func(e *env) { fn(e.auth) } }

// withStore replaces the in-memory store with another blob.Store.
func withStore(s blob.Store) envOption {
	return func(e *env) {
		e.blobStore = s
		e.store, _ = s.(*sitestest.MemStore)
	}
}

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.BaseDomain = "backyard.test"
	cfg.Server.Secret = testSecret
	cfg.Auth.Mode = "none"
	cfg.ApplyDefaults()
	return cfg
}

func newEnv(t *testing.T, opts ...envOption) *env {
	t.Helper()
	e := &env{t: t, cfg: newTestConfig(t), logs: &lockedWriter{}}
	e.store = sitestest.NewMemStore()
	e.blobStore = e.store
	e.auth = &fakeAuth{mode: auth.ModeNone, users: defaultUsers()}
	for _, o := range opts {
		o(e)
	}
	e.reg = sites.NewRegistry(e.blobStore, e.cfg)
	var gw *Gateway
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gw.ServeHTTP(w, r) }))
	t.Cleanup(e.srv.Close)
	logger := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var err error
	gw, err = NewGateway(GatewayOptions{
		Config:   e.cfg,
		Store:    e.blobStore,
		Registry: e.reg,
		Auth:     e.auth,
		BaseURL:  e.srv.URL,
		Logger:   logger,
		Version:  "test",
		OnDeleteSite: func(ctx context.Context, site string) error {
			e.deleteMu.Lock()
			defer e.deleteMu.Unlock()
			e.deleted = append(e.deleted, site)
			return e.hookErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.gw = gw
	return e
}

// lockedWriter is a concurrency-safe log buffer.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedWriter) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (e *env) client(token string) *Client { return NewClient(e.srv.URL, token) }

// do sends a raw request to the gateway.
func (e *env) do(method, path, token string, body any, hdr ...string) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(j)
	}
	target := path
	if !strings.HasPrefix(path, "http") {
		target = e.srv.URL + path
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp, data
}

// errCode decodes the error envelope of a response body.
func errCode(t *testing.T, body []byte) ErrorBody {
	t.Helper()
	var eb ErrorBody
	if err := json.Unmarshal(body, &eb); err != nil {
		t.Fatalf("not a JSON error body: %q", body)
	}
	return eb
}

// wantStatus fails unless resp has the given status (and error code, if any).
func wantStatus(t *testing.T, resp *http.Response, body []byte, status int, code string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, status, body)
	}
	if code != "" {
		if got := errCode(t, body).Error.Code; got != code {
			t.Fatalf("error code = %q, want %q; body: %s", got, code, body)
		}
	}
}

// apiErr asserts err is an *APIError with the given status.
func apiErr(t *testing.T, err error, status int, code string) *APIError {
	t.Helper()
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("error %v (%T) is not an *APIError", err, err)
	}
	if ae.Status != status || (code != "" && ae.Code != code) {
		t.Fatalf("APIError = %+v, want status %d code %q", ae, status, code)
	}
	return ae
}

// writeTree creates files (path → content) under dir.
func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// siteKeys returns the site-relative keys stored for site.
func (e *env) siteKeys(site string) []string {
	e.t.Helper()
	files, err := e.reg.Files(context.Background(), site)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Key)
	}
	return out
}

// object returns the content of a site file (nil when missing).
func (e *env) object(site, p string) []byte {
	e.t.Helper()
	rc, _, err := e.blobStore.Get(context.Background(), sites.SiteKey(site, p))
	if errors.Is(err, blob.ErrNotFound) {
		return nil
	}
	if err != nil {
		e.t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// entriesFor builds a file list (with correct hashes) from contents.
func entriesFor(files map[string]string) []FileEntry {
	var out []FileEntry
	for p, c := range files {
		out = append(out, FileEntry{Path: p, Size: int64(len(c)), SHA256: sha256Hex([]byte(c))})
	}
	return out
}
