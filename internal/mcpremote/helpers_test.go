package mcpremote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/memory"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/sites/sitestest"
)

const (
	testSecret = "test-secret-0123456789abcdef0123456789"
	hubURL     = "https://hub.backyard.test"
	hubHost    = "hub.backyard.test"
	prmURL     = hubURL + "/.well-known/oauth-protected-resource/__backyard/mcp"

	aliceToken = "byat_alice.0000000000000000000000"
	bobToken   = "byat_bob.000000000000000000000000"
	guestToken = "byat_guest.0000000000000000000000"
	adminToken = "byat_admin.0000000000000000000000"
)

var (
	alice = &identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice"}
	bob   = &identity.User{ID: "bob@acme.com", Email: "bob@acme.com", Name: "Bob"}
	root  = &identity.User{ID: "root@acme.com", Email: "root@acme.com", Name: "Root", Admin: true}
	guest = &identity.User{ID: "anon:0123456789abcdef", Name: "Guest 0123", Anonymous: true}
)

// tokenErr is an invalid-token error of the fake authorization server.
type tokenErr struct{ desc string }

func (e *tokenErr) Error() string        { return "invalid token: " + e.desc }
func (e *tokenErr) Is(target error) bool { return target == auth.ErrInvalidToken }

// fakeAuth is a scriptable AuthServer.
type fakeAuth struct {
	mu      sync.Mutex
	issuer  string
	grants  map[string]*auth.Grant // token → grant (with the user snapshot)
	bad     map[string]string      // token → error_description
	infra   error                  // returned for every token when set
	origins map[string]bool
	maxBody int64
}

var _ AuthServer = (*fakeAuth)(nil)

func newFakeAuth() *fakeAuth {
	f := &fakeAuth{
		issuer:  hubURL,
		grants:  map[string]*auth.Grant{},
		bad:     map[string]string{},
		origins: map[string]bool{"https://claude.ai": true, "https://claude.com": true},
		maxBody: 2*config.DefaultMCPMaxPublishBytes + 1<<20,
	}
	for tok, u := range map[string]*identity.User{aliceToken: alice, bobToken: bob, guestToken: guest, adminToken: root} {
		f.grants[tok] = &auth.Grant{
			ID: "0123abcd" + strings.Repeat("e", 24), ClientID: "https://claude.ai/oauth/mcp-oauth-client-metadata",
			ClientName: "Claude", ClientKind: "metadata_document", ClientHost: "claude.ai",
			RedirectURI: "https://claude.ai/api/mcp/auth_callback", Scope: "mcp offline_access", Resource: hubURL + "/__backyard/mcp",
			User: *u, CreatedAt: time.Unix(1_700_000_000, 0), ExpiresAt: time.Unix(1_702_592_000, 0),
		}
	}
	return f
}

func (f *fakeAuth) VerifyAccessToken(_ context.Context, raw string) (*identity.User, *auth.Grant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.infra != nil {
		return nil, nil, f.infra
	}
	if desc, ok := f.bad[raw]; ok {
		return nil, nil, &tokenErr{desc}
	}
	g, ok := f.grants[raw]
	if !ok {
		return nil, nil, &tokenErr{"malformed token"}
	}
	u, gc := g.User, *g
	return &u, &gc, nil
}

func (f *fakeAuth) Challenge(err error) string {
	if err == nil {
		return `Bearer resource_metadata="` + f.ResourceMetadataURL() + `", scope="mcp"`
	}
	desc := "malformed token"
	var te *tokenErr
	if errors.As(err, &te) {
		desc = te.desc
	}
	return `Bearer error="invalid_token", error_description="` + desc + `", resource_metadata="` + f.ResourceMetadataURL() + `", scope="mcp"`
}

func (f *fakeAuth) ResourceMetadataURL() string {
	return f.issuer + "/.well-known/oauth-protected-resource/__backyard/mcp"
}
func (f *fakeAuth) Resource() string            { return f.issuer + "/__backyard/mcp" }
func (f *fakeAuth) Issuer() string              { return f.issuer }
func (f *fakeAuth) MaxBodyBytes() int64         { return f.maxBody }
func (f *fakeAuth) AllowedOrigin(o string) bool { return f.origins[o] }

// stubAuthn is the gateway's authenticator (Publish never authenticates).
type stubAuthn struct{}

func (stubAuthn) Authenticate(http.ResponseWriter, *http.Request) (*identity.User, error) {
	return nil, auth.ErrUnauthenticated
}
func (stubAuthn) LoginURL(string) string { return "" }
func (stubAuthn) Handler() http.Handler  { return http.NotFoundHandler() }
func (stubAuthn) Mode() string           { return auth.ModeNone }

// lockedBuffer is a concurrency-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// tenv is an endpoint over in-memory storage.
type tenv struct {
	t     *testing.T
	cfg   *config.Config
	store *sitestest.MemStore
	reg   *sites.Registry
	docs  docstore.Store
	gw    *deploy.Gateway
	auth  *fakeAuth
	h     *handler
	logs  *lockedBuffer

	mu        sync.Mutex
	published []string // OnPublish calls: "<site> ok|error"
	bodies    []string // every response body
}

type tenvOption struct {
	cfg       func(*config.Config)
	noGateway bool
	opts      func(*Options)
}

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(""))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.PublicURL = hubURL
	cfg.Server.BaseDomain = "backyard.test"
	cfg.Server.Secret = testSecret
	cfg.Auth.Mode = auth.ModeOIDC
	cfg.ApplyDefaults()
	return cfg
}

func newTenv(t *testing.T, o ...tenvOption) *tenv {
	t.Helper()
	e := &tenv{t: t, cfg: newTestConfig(t), store: sitestest.NewMemStore(), docs: memory.New(), auth: newFakeAuth(), logs: &lockedBuffer{}}
	var opt tenvOption
	for _, x := range o {
		if x.cfg != nil {
			x.cfg(e.cfg)
		}
		opt.noGateway = opt.noGateway || x.noGateway
		if x.opts != nil {
			opt.opts = x.opts
		}
	}
	logger := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.reg = sites.NewRegistry(e.store, e.cfg)
	if !opt.noGateway {
		gw, err := deploy.NewGateway(deploy.GatewayOptions{Config: e.cfg, Store: e.store, Registry: e.reg, Auth: stubAuthn{}, Logger: logger, Version: "test"})
		if err != nil {
			t.Fatal(err)
		}
		e.gw = gw
	}
	opts := Options{
		Config: e.cfg, Auth: e.auth, Sites: e.reg, Docs: e.docs, Gateway: e.gw, Version: "1.2.3", Logger: logger,
		OnPublish: func(site string, ok bool) {
			e.mu.Lock()
			defer e.mu.Unlock()
			res := "error"
			if ok {
				res = "ok"
			}
			e.published = append(e.published, site+" "+res)
		},
		ClientIP: func(*http.Request) string { return "203.0.113.9" },
	}
	if opt.opts != nil {
		opt.opts(&opts)
	}
	h, err := newHandler(opts)
	if err != nil {
		t.Fatal(err)
	}
	e.h = h
	return e
}

// request builds a POST to the endpoint with the headers of a well-behaved
// client; hdr pairs override them ("" removes a header).
func request(body string, hdr ...string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "https://"+hubHost+"/__backyard/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+aliceToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2025-11-25")
	for i := 0; i+1 < len(hdr); i += 2 {
		if hdr[i+1] == "" {
			r.Header.Del(hdr[i])
		} else {
			r.Header.Set(hdr[i], hdr[i+1])
		}
	}
	return r
}

// serve runs r through the handler and checks the invariants of every
// answer: no session id, no cookie, no-store.
func (e *tenv) serve(r *http.Request) *httptest.ResponseRecorder {
	e.t.Helper()
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	for _, k := range []string{"Mcp-Session-Id", "Set-Cookie"} {
		if v := w.Header().Get(k); v != "" {
			e.t.Fatalf("response carries %s: %q", k, v)
		}
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		e.t.Fatalf("Cache-Control = %q", cc)
	}
	e.mu.Lock()
	e.bodies = append(e.bodies, w.Body.String())
	e.mu.Unlock()
	return w
}

func (e *tenv) post(body string, hdr ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.serve(request(body, hdr...))
}

// rpc sends a request and decodes the JSON-RPC response (HTTP 200).
func (e *tenv) rpc(method string, params any, hdr ...string) (result json.RawMessage, rerr *rpcError) {
	e.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": 7, "method": method}
	if params != nil {
		msg["params"] = params
	}
	b, err := json.Marshal(msg)
	if err != nil {
		e.t.Fatal(err)
	}
	w := e.post(string(b), hdr...)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		e.t.Fatalf("%s: status %d (%s): %s", method, w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		e.t.Fatalf("%s: %v: %s", method, err, w.Body)
	}
	if resp.JSONRPC != "2.0" || string(resp.ID) != "7" {
		e.t.Fatalf("%s: envelope %s", method, w.Body)
	}
	return resp.Result, resp.Error
}

// tool calls a tool as the holder of token and returns its text.
func (e *tenv) tool(token, name string, args any) (string, bool) {
	e.t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	res, rerr := e.rpc("tools/call", params, "Authorization", "Bearer "+token)
	if rerr != nil {
		e.t.Fatalf("%s: JSON-RPC error %d %s", name, rerr.Code, rerr.Message)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil || len(out.Content) != 1 || out.Content[0].Type != "text" {
		e.t.Fatalf("%s: result %s (%v)", name, res, err)
	}
	return out.Content[0].Text, out.IsError
}

// toolOK calls a tool that must succeed and decodes its JSON text into v
// (when not nil).
func (e *tenv) toolOK(token, name string, args, v any) string {
	e.t.Helper()
	text, isErr := e.tool(token, name, args)
	if isErr {
		e.t.Fatalf("%s failed: %s", name, text)
	}
	if v != nil {
		if err := json.Unmarshal([]byte(text), v); err != nil {
			e.t.Fatalf("%s: %v: %s", name, err, text)
		}
	}
	return text
}

// toolErr calls a tool that must fail with a text containing fragment.
func (e *tenv) toolErr(token, name string, args any, fragment string) string {
	e.t.Helper()
	text, isErr := e.tool(token, name, args)
	if !isErr || !strings.Contains(text, fragment) {
		e.t.Fatalf("%s = %v %q, want an error containing %q", name, isErr, text, fragment)
	}
	return text
}

// object returns a stored site file (nil when missing).
func (e *tenv) object(site, p string) []byte {
	e.t.Helper()
	rc, _, err := e.store.Get(context.Background(), sites.SiteKey(site, p))
	if err != nil {
		return nil
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

func (e *tenv) putObject(site, p string, content []byte) {
	e.t.Helper()
	if _, err := e.store.Put(context.Background(), sites.SiteKey(site, p), bytes.NewReader(content), int64(len(content)), "text/plain"); err != nil {
		e.t.Fatal(err)
	}
}

// files builds a backyard_publish files argument from path → content
// pairs (utf-8).
func files(pairs ...string) []map[string]any {
	out := []map[string]any{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"path": pairs[i], "content": pairs[i+1]})
	}
	return out
}
