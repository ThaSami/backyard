package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/memory"
	"github.com/thasami/backyard/internal/docstore/sqlite"
)

// The hub of the MCP tests (baseConfig's public URL) and its derived URLs.
const (
	testIssuer   = "https://backyard.test"
	testResource = testIssuer + MCPPath
	testPRM      = testIssuer + ResourceMetadataPath
	claudeCB     = "https://claude.ai/api/mcp/auth_callback"
	claudeCIMD   = "https://claude.ai/oauth/mcp-oauth-client-metadata"
	loopbackCB   = "http://127.0.0.1:9/callback"
)

// mcpEnv is a hub with the connector enabled.
type mcpEnv struct {
	t     *testing.T
	cfg   *config.Config
	a     *authenticator
	m     *MCP
	docs  docstore.Store
	clock *fakeClock
	oidc  *oidcEnv // oidc mode only
}

// newMCPEnv builds an authenticator in mode with mcp.enabled on a memory
// docstore (opts may replace it).
func newMCPEnv(t *testing.T, mode string, mutate func(*config.Config), opts ...func(*Options)) *mcpEnv {
	t.Helper()
	docs := docstore.Store(memory.New())
	withDocs := func(o *Options) {
		if o.Docs == nil {
			o.Docs = docs
		}
	}
	if mode == ModeOIDC {
		oe := newOIDCEnv(t, func(c *config.Config) {
			c.MCP.Enabled = true
			if mutate != nil {
				mutate(c)
			}
		}, append(append([]func(*Options){}, opts...), withDocs)...)
		e := &mcpEnv{t: t, cfg: oe.cfg, a: oe.a, m: oe.a.mcp, docs: oe.a.mcp.docs, clock: oe.clock, oidc: oe}
		t.Cleanup(e.m.waitBackground)
		return e
	}
	clock := newClock()
	cfg := baseConfig(mode)
	cfg.MCP.Enabled = true
	if mutate != nil {
		mutate(cfg)
	}
	o := Options{Now: clock.Now}
	for _, f := range opts {
		f(&o)
	}
	withDocs(&o)
	a := newAuth(t, cfg, o)
	if a.mcp == nil {
		t.Fatal("MCP is not enabled")
	}
	e := &mcpEnv{t: t, cfg: cfg, a: a, m: a.mcp, docs: a.mcp.docs, clock: clock}
	t.Cleanup(e.m.waitBackground)
	return e
}

// sqliteDocs returns an option that stores grants in a fresh sqlite file.
func sqliteDocs(t *testing.T) func(*Options) {
	t.Helper()
	s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "docs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return func(o *Options) { o.Docs = s }
}

// handler routes like the server does for the connector's paths.
func (e *mcpEnv) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case p == ASMetadataPath || p == ResourceMetadataPath:
			e.m.ServeMetadata(w, r)
		case strings.HasPrefix(p, OAuthPathPrefix):
			e.m.ServeOAuth(w, r)
		default:
			e.a.Handler().ServeHTTP(w, r)
		}
	})
}

// do serves r.
func (e *mcpEnv) do(r *http.Request) *http.Response {
	e.t.Helper()
	return serve(e.handler(), r)
}

// oauthPost builds a form POST to the hub.
func oauthPost(path string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, testIssuer+path, strings.NewReader(form.Encode()))
	r.RemoteAddr = "192.0.2.10:40000"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

// postJSON builds a JSON POST to the hub.
func postJSON(path, body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, testIssuer+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.10:40000"
	r.Header.Set("Content-Type", "application/json")
	return r
}

// jsonBody decodes a JSON response into a map.
func jsonBody(t *testing.T, res *http.Response) map[string]any {
	t.Helper()
	var m map[string]any
	b := body(t, res)
	if err := json.Unmarshal([]byte(b), &m); err != nil {
		t.Fatalf("response is not JSON (%d): %q", res.StatusCode, b)
	}
	return m
}

// register registers a DCR client and returns its client_id.
func (e *mcpEnv) register(name string, uris ...string) string {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": uris})
	res := e.do(postJSON(OAuthRegisterPath, string(b)))
	if res.StatusCode != http.StatusCreated {
		e.t.Fatalf("register: %d %s", res.StatusCode, body(e.t, res))
	}
	id, _ := jsonBody(e.t, res)["client_id"].(string)
	if !strings.HasPrefix(id, prefixDCRClient) {
		e.t.Fatalf("client_id = %q", id)
	}
	return id
}

// pkce returns a fresh verifier and its S256 challenge.
func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authzParams are the parameters of a valid authorization request.
func authzParams(clientID, redirect, challenge, state string) url.Values {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {testResource},
		"scope":                 {"mcp offline_access"},
	}
	if state != "" {
		v.Set("state", state)
	}
	return v
}

// browser is a cookie jar with the browser's request headers.
type browser struct {
	cookies map[string]*http.Cookie
	extra   []*http.Cookie // sent as is (several cookies with one name)
	headers http.Header
	remote  string
}

func newBrowser(cookies ...*http.Cookie) *browser {
	b := &browser{cookies: map[string]*http.Cookie{}, headers: http.Header{}, remote: "192.0.2.10:40000"}
	for _, c := range cookies {
		if c != nil {
			b.cookies[c.Name] = c
		}
	}
	return b
}

// request builds a request with the jar's cookies and headers.
func (b *browser) request(method, target string, form url.Values) *http.Request {
	var r *http.Request
	if form != nil {
		r = httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.RemoteAddr = b.remote
	for k, vs := range b.headers {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	for _, c := range b.cookies {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	for _, c := range b.extra {
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return r
}

// keep stores the cookies a response sets.
func (b *browser) keep(res *http.Response) {
	for _, c := range res.Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
			continue
		}
		b.cookies[c.Name] = c
	}
}

// authorize sends the authorization GET.
func (e *mcpEnv) authorize(b *browser, params url.Values) *http.Response {
	e.t.Helper()
	r := b.request(http.MethodGet, testIssuer+AuthorizePath+"?"+params.Encode(), nil)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	res := e.do(r)
	b.keep(res)
	return res
}

var hiddenInputRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// consentFields returns the hidden fields of a consent page.
func consentFields(t *testing.T, res *http.Response) (url.Values, string) {
	t.Helper()
	b := body(t, res)
	if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" {
		t.Fatalf("expected the consent page, got %d %q\n%s", res.StatusCode, res.Header.Get("Location"), b)
	}
	if !strings.Contains(b, `<form id="consent" method="post" action="/__backyard/auth/authorize">`) {
		t.Fatalf("no consent form:\n%s", b)
	}
	v := url.Values{}
	for _, m := range hiddenInputRe.FindAllStringSubmatch(b, -1) {
		v.Add(m[1], html.UnescapeString(m[2]))
	}
	return v, b
}

// consentPost submits the consent form like the browser would from the
// consent page.
func (e *mcpEnv) consentPost(b *browser, fields url.Values, decision string) *http.Response {
	e.t.Helper()
	form := url.Values{}
	for k, v := range fields {
		form[k] = append([]string(nil), v...)
	}
	if decision != "" {
		form.Set("decision", decision)
	}
	r := b.request(http.MethodPost, testIssuer+AuthorizePath, form)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	r.Header.Set("Origin", "null")
	res := e.do(r)
	b.keep(res)
	return res
}

// redirectParams checks that res redirects to the redirect URI (prefix) and
// returns its query.
func redirectParams(t *testing.T, res *http.Response, status int, prefix string) url.Values {
	t.Helper()
	loc := res.Header.Get("Location")
	if res.StatusCode != status || !strings.HasPrefix(loc, prefix) {
		t.Fatalf("expected %d to %s, got %d %q: %s", status, prefix, res.StatusCode, loc, body(t, res))
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

// flow is one client's authorization run.
type flow struct {
	clientID, redirect, verifier, challenge, state string
}

func newFlow(clientID, redirect string) *flow {
	v, c := pkce()
	return &flow{clientID: clientID, redirect: redirect, verifier: v, challenge: c, state: "st-" + randomString(6)}
}

// code runs authorize → consent → allow and returns the code.
func (e *mcpEnv) code(b *browser, f *flow) string {
	e.t.Helper()
	fields, _ := consentFields(e.t, e.authorize(b, authzParams(f.clientID, f.redirect, f.challenge, f.state)))
	q := redirectParams(e.t, e.consentPost(b, fields, "allow"), http.StatusSeeOther, f.redirect)
	if q.Get("state") != f.state || q.Get("iss") != testIssuer || !strings.HasPrefix(q.Get("code"), prefixCode) {
		e.t.Fatalf("allow redirect query = %v", q)
	}
	return q.Get("code")
}

// exchange redeems a code.
func (e *mcpEnv) exchange(f *flow, code string, extra url.Values) *http.Response {
	e.t.Helper()
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {f.verifier},
		"client_id": {f.clientID}, "redirect_uri": {f.redirect},
	}
	for k, v := range extra {
		if v == nil {
			delete(form, k)
		} else {
			form[k] = v
		}
	}
	return e.do(oauthPost(OAuthTokenPath, form))
}

// tokens runs a whole flow and returns the access and refresh tokens.
func (e *mcpEnv) tokens(b *browser, f *flow) (access, refresh string) {
	e.t.Helper()
	res := e.exchange(f, e.code(b, f), nil)
	if res.StatusCode != http.StatusOK {
		e.t.Fatalf("exchange: %d %s", res.StatusCode, body(e.t, res))
	}
	m := jsonBody(e.t, res)
	return m["access_token"].(string), m["refresh_token"].(string)
}

// refresh presents a refresh token.
func (e *mcpEnv) refresh(f *flow, rt string, extra url.Values) *http.Response {
	e.t.Helper()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {f.clientID}}
	for k, v := range extra {
		form[k] = v
	}
	return e.do(oauthPost(OAuthTokenPath, form))
}

// oauthErr checks an OAuth error response.
func oauthErr(t *testing.T, res *http.Response, status int, code string) map[string]any {
	t.Helper()
	m := jsonBody(t, res)
	if res.StatusCode != status || m["error"] != code {
		t.Fatalf("want %d %s, got %d %v", status, code, res.StatusCode, m)
	}
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Pragma") != "no-cache" {
		t.Errorf("OAuth error is cacheable: %v", res.Header)
	}
	return m
}

// grantCount returns the number of stored grants.
func (e *mcpEnv) grantCount() int {
	e.t.Helper()
	q := docstore.Query{Limit: 1000}
	_ = q.Normalize(1000)
	page, err := e.docs.Query(context.Background(), docstore.SystemSite, grantsCollection, q)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(page.Docs)
}

// auditActions returns the stored audit actions in order of creation.
func (e *mcpEnv) auditActions() []string {
	e.t.Helper()
	q := docstore.Query{Limit: 1000}
	_ = q.Normalize(1000)
	page, err := e.docs.Query(context.Background(), docstore.SystemSite, auditCollection, q)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, d := range page.Docs {
		a, _ := d.Data["action"].(string)
		if via, ok := d.Data["via"].(string); ok {
			a += ":" + via
		}
		out = append(out, a)
	}
	return out
}
