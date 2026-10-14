package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/memory"
	"github.com/thasami/backyard/internal/mcpremote"
)

// The connector tests run the real authorization server in auth.mode
// header: a trusted proxy (192.0.2.0/24, httptest's default peer) names the
// user in X-Forwarded-Email. Requests from other peers carry no identity.
const (
	connEmailHeader = "X-Forwarded-Email"
	connRedirect    = "http://127.0.0.1:9/callback"
	connResource    = hubURL + "/__backyard/mcp"
	staticAdminTok  = "static-admin-token-0123456789"
	disabledBody    = `{"error":{"code":"not_found","message":"the Claude connector is not enabled on this server"}}`
)

// connector is the authorization server side of a connector harness.
type connector struct {
	h     *harness
	mcp   *auth.MCP
	docs  docstore.Store // the grant store (also the authorization server's audit records)
	clock *connClock
	// endpoint records the requests that reached Deps.MCPEndpoint.
	mu       sync.Mutex
	seen     []*http.Request
	reqIDSet []bool // X-Request-Id was set on the response before the endpoint ran
}

// connClock is the authorization server's clock.
type connClock struct{ ns atomic.Int64 }

func (c *connClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *connClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// withConnector enables the connector with the real authorization server and
// the real MCP endpoint (wrapped to record requests). mutate adjusts the
// configuration first. Pass it after the options that change the config.
func withConnector(cn *connector, mutate func(*config.Config)) option {
	return func(h *harness, d *Deps) {
		cn.h = h
		c := h.cfg
		c.Auth.Mode = "header"
		c.Auth.Header.EmailHeader = connEmailHeader
		c.Auth.Admins = []string{"boss@acme.com"}
		sum := sha256.Sum256([]byte(staticAdminTok))
		c.Auth.Tokens = []config.StaticToken{{Name: "ci", SHA256: hex.EncodeToString(sum[:]), Admin: true}}
		c.Server.TrustedProxies = []string{"192.0.2.0/24"}
		c.MCP.Enabled = true
		if mutate != nil {
			mutate(c)
		}
		cn.clock = &connClock{}
		cn.clock.ns.Store(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano())
		cn.docs = memory.New()
		authn, err := auth.New(context.Background(), c, auth.Options{
			Docs: cn.docs, Logger: d.Logger, Now: cn.clock.Now,
			MCPAllow: func(k string) (bool, time.Duration) { return h.srv.AllowAPI(k) },
		})
		if err != nil {
			h.t.Fatalf("auth.New: %v", err)
		}
		cn.mcp = auth.MCPOf(authn)
		if cn.mcp == nil {
			h.t.Fatal("connector not enabled")
		}
		endpoint := mcpremote.New(mcpremote.Options{
			Config: c, Auth: cn.mcp, Blob: h.blob, Docs: cn.docs,
			Allow:         func(k string) (bool, time.Duration) { return h.srv.AllowAPI(k) },
			RequestOrigin: RequestOrigin, ClientIP: ClientIP,
			Logger: d.Logger, Now: cn.clock.Now,
		})
		d.Auth = authn
		d.MCP = cn.mcp
		d.MCPEndpoint = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cn.mu.Lock()
			cn.seen = append(cn.seen, r)
			cn.reqIDSet = append(cn.reqIDSet, w.Header().Get("X-Request-Id") != "")
			cn.mu.Unlock()
			endpoint.ServeHTTP(w, r)
		})
	}
}

// newConnector builds a harness with the connector enabled.
func newConnector(t *testing.T, mutate func(*config.Config), opts ...option) *connector {
	t.Helper()
	cn := &connector{}
	newHarness(t, append(opts, withConnector(cn, mutate))...)
	return cn
}

// as returns the identity headers of a signed-in user (empty: none).
func as(email string) map[string]string {
	if email == "" {
		return map[string]string{}
	}
	return map[string]string{connEmailHeader: email}
}

// do serves a request with extra headers.
func (cn *connector) do(method, target, body string, hdr map[string]string) *httptest.ResponseRecorder {
	cn.h.t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	rq := req{method: method, url: target, hdr: hdr}
	if rd != nil {
		rq.body = rd
	}
	if r, ok := hdr["RemoteAddr"]; ok {
		rq.remote = r
		delete(hdr, "RemoteAddr")
	}
	return cn.h.serve(rq)
}

// register registers a DCR client.
func (cn *connector) register(name string) string {
	cn.h.t.Helper()
	b, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": []string{connRedirect}})
	w := cn.do("POST", hubURL+"/__backyard/oauth/register", string(b), map[string]string{"Content-Type": "application/json"})
	expectStatus(cn.h.t, w, http.StatusCreated)
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || !strings.HasPrefix(out.ClientID, "bydc_") {
		cn.h.t.Fatalf("register: %v %s", err, w.Body.String())
	}
	return out.ClientID
}

var connHiddenRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// tokens is one connection's credentials.
type tokens struct {
	clientID, access, refresh string
}

// connect runs register → authorize → consent → token as email.
func (cn *connector) connect(email, app string) tokens {
	t := cn.h.t
	t.Helper()
	id := cn.register(app)
	code, verifier := cn.code(email, id)
	w := cn.do("POST", hubURL+"/__backyard/oauth/token", cn.exchangeForm(id, code, verifier), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	expectStatus(t, w, http.StatusOK)
	var out struct {
		Access  string `json:"access_token"`
		Refresh string `json:"refresh_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Access == "" {
		t.Fatalf("token: %v %s", err, w.Body.String())
	}
	cn.clock.advance(2 * time.Second) // distinct created_at for the next connection
	return tokens{clientID: id, access: out.Access, refresh: out.Refresh}
}

// authorizeQuery is the query of an authorization request of client id.
func authorizeQuery(id, verifier string) url.Values {
	sum := sha256.Sum256([]byte(verifier))
	return url.Values{
		"response_type": {"code"}, "client_id": {id}, "redirect_uri": {connRedirect},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"resource": {connResource}, "scope": {"mcp offline_access"}, "state": {"st"},
	}
}

// code runs authorize → consent as email for client id and returns the
// code and its PKCE verifier.
func (cn *connector) code(email, id string) (code, verifier string) {
	t := cn.h.t
	t.Helper()
	vb := make([]byte, 32)
	_, _ = rand.Read(vb)
	verifier = base64.RawURLEncoding.EncodeToString(vb)
	hdr := as(email)
	hdr["Sec-Fetch-Site"] = "cross-site"
	w := cn.do("GET", hubURL+auth.AuthorizePath+"?"+authorizeQuery(id, verifier).Encode(), "", hdr)
	expectStatus(t, w, http.StatusOK)
	form := url.Values{}
	for _, m := range connHiddenRe.FindAllStringSubmatch(w.Body.String(), -1) {
		form.Add(m[1], html.UnescapeString(m[2]))
	}
	if form.Get("csrf") == "" {
		t.Fatalf("no consent form:\n%s", w.Body.String())
	}
	form.Set("decision", "allow")
	hdr = as(email)
	hdr["Content-Type"] = "application/x-www-form-urlencoded"
	hdr["Sec-Fetch-Site"] = "same-origin"
	hdr["Sec-Fetch-Mode"] = "navigate"
	hdr["Sec-Fetch-Dest"] = "document"
	hdr["Origin"] = hubURL
	w = cn.do("POST", hubURL+auth.AuthorizePath, form.Encode(), hdr)
	expectStatus(t, w, http.StatusSeeOther)
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		t.Fatalf("allow redirect = %q", w.Header().Get("Location"))
	}
	return loc.Query().Get("code"), verifier
}

// exchangeForm is the token request that redeems code.
func (cn *connector) exchangeForm(id, code, verifier string) string {
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier},
		"client_id": {id}, "redirect_uri": {connRedirect}, "resource": {connResource},
	}.Encode()
}

// mcpCall sends a JSON-RPC ping to the MCP endpoint with an access token.
func (cn *connector) mcpCall(token string) *httptest.ResponseRecorder {
	cn.h.t.Helper()
	return cn.do("POST", hubURL+"/__backyard/mcp", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{
		"Content-Type": "application/json", "Authorization": "Bearer " + token, "RemoteAddr": "198.51.100.7:1",
	})
}

// grantList is the body of GET /__backyard/api/mcp/grants.
type grantList struct {
	Grants []struct {
		ID   string `json:"id"`
		User struct {
			ID    string `json:"id"`
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
		ClientName   string `json:"client_name"`
		ClientKind   string `json:"client_kind"`
		ClientHost   string `json:"client_host"`
		RedirectHost string `json:"redirect_host"`
		Scope        string `json:"scope"`
		CreatedAt    string `json:"created_at"`
		ExpiresAt    string `json:"expires_at"`
	} `json:"grants"`
}

// grants lists connections as email (with an optional ?user=).
func (cn *connector) grants(email, user string) *httptest.ResponseRecorder {
	cn.h.t.Helper()
	u := hubURL + "/__backyard/api/mcp/grants"
	if user != "" {
		u += "?user=" + url.QueryEscape(user)
	}
	return cn.do("GET", u, "", as(email))
}

// revoke sends a DELETE of the grants API as email, with the CSRF header.
func (cn *connector) revoke(email, path string) *httptest.ResponseRecorder {
	cn.h.t.Helper()
	hdr := as(email)
	hdr["X-Backyard"] = "1"
	return cn.do("DELETE", hubURL+"/__backyard/api/mcp/grants"+path, "", hdr)
}

// audits returns the stored mcp.revoke records of the authorization server.
func (cn *connector) revokeAudits(t *testing.T) []map[string]any {
	t.Helper()
	q := docstore.Query{Limit: 1000}
	_ = q.Normalize(1000)
	page, err := cn.docs.Query(context.Background(), docstore.SystemSite, "_audit", q)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, d := range page.Docs {
		if d.Data["action"] == "mcp.revoke" {
			out = append(out, d.Data)
		}
	}
	return out
}

// noCORSNoCookies checks the rules shared by every connector path.
func noCORSNoCookies(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	for k := range w.Header() {
		if strings.HasPrefix(strings.ToLower(k), "access-control-") {
			t.Errorf("CORS header %s on a connector path", k)
		}
	}
	if c := w.Header().Values("Set-Cookie"); len(c) > 0 {
		t.Errorf("Set-Cookie on a connector path: %v", c)
	}
}

func TestConnectorDisabled(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ method, url string }{
		{"GET", hubURL + "/.well-known/oauth-authorization-server"},
		{"GET", hubURL + "/.well-known/oauth-protected-resource/__backyard/mcp"},
		{"POST", hubURL + "/__backyard/oauth/register"},
		{"POST", hubURL + "/__backyard/oauth/token"},
		{"POST", hubURL + "/__backyard/oauth/revoke"},
		{"POST", hubURL + "/__backyard/oauth/foo"},
		{"POST", hubURL + "/__backyard/mcp"},
		{"GET", hubURL + "/__backyard/mcp"},
	} {
		t.Run(tc.method+" "+strings.TrimPrefix(tc.url, hubURL), func(t *testing.T) {
			w := h.serve(req{method: tc.method, url: tc.url, body: strings.NewReader("{}"), hdr: map[string]string{"Content-Type": "application/json"}})
			expectStatus(t, w, http.StatusNotFound)
			var got, want any
			_ = json.Unmarshal([]byte(disabledBody), &want)
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || !jsonEqual(got, want) {
				t.Fatalf("body = %s, want %s", w.Body.String(), disabledBody)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
			}
			noCORSNoCookies(t, w)
		})
	}
	if n := h.auth.calls; n != 0 {
		t.Errorf("the machine paths authenticated %d times", n)
	}

	// The grants API answers 404 too, signed in or not (before apiGate, so
	// no authentication and no CSRF check).
	calls := h.auth.calls
	expectError(t, h.serve(req{url: hubURL + "/__backyard/api/mcp/grants"}), http.StatusNotFound, "not_found")
	expectError(t, h.serve(req{method: "DELETE", url: hubURL + "/__backyard/api/mcp/grants/0123456789abcdef0123456789abcdef"}),
		http.StatusNotFound, "not_found")
	if h.auth.calls != calls {
		t.Errorf("the disabled grants API authenticated %d times", h.auth.calls-calls)
	}
	expectError(t, h.api("GET", hubURL+"/__backyard/api/mcp/grants", nil), http.StatusNotFound, "not_found")
	expectError(t, h.api("DELETE", hubURL+"/__backyard/api/mcp/grants", nil), http.StatusNotFound, "not_found")
	e := expectError(t, h.api("DELETE", hubURL+"/__backyard/api/mcp/grants/0123456789abcdef0123456789abcdef", nil), http.StatusNotFound, "not_found")
	if e.Error.Message != "the Claude connector is not enabled on this server" {
		t.Errorf("message = %q", e.Error.Message)
	}

	// Authorize goes to the authenticator (which answers its own 404 page),
	// without the per-IP limit while the connector is off.
	h = newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.APIPerMinute = 1 }))
	for range 3 {
		w := h.get(hubURL + "/__backyard/auth/authorize?client_id=x")
		expectStatus(t, w, http.StatusOK)
		if w.Body.String() != "auth handler /__backyard/auth/authorize" {
			t.Fatalf("authorize body = %q", w.Body.String())
		}
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestConnectorRouting(t *testing.T) {
	cn := newConnector(t, nil)
	h := cn.h
	h.deploy("foo", map[string]string{
		"index.html":                             "foo",
		".well-known/oauth-authorization-server": "site file",
		".well-known/oauth-protected-resource/x": "site prm",
	})

	// Discovery documents on the canonical hub host.
	w := h.get(hubURL + "/.well-known/oauth-authorization-server")
	expectStatus(t, w, http.StatusOK)
	var meta map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil || meta["issuer"] != hubURL ||
		meta["token_endpoint"] != hubURL+"/__backyard/oauth/token" {
		t.Fatalf("AS metadata = %s", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}
	noCORSNoCookies(t, w)
	w = h.get(hubURL + "/.well-known/oauth-protected-resource/__backyard/mcp")
	expectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"resource":"`+connResource+`"`) {
		t.Fatalf("PRM = %s", w.Body.String())
	}
	w = h.serve(req{method: "HEAD", url: hubURL + "/.well-known/oauth-authorization-server"})
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %q", w.Code, w.Body.String())
	}
	w = h.serve(req{method: "POST", url: hubURL + "/.well-known/oauth-authorization-server"})
	expectStatus(t, w, http.StatusMethodNotAllowed)
	if w.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("Allow = %q", w.Header().Get("Allow"))
	}
	// Other /.well-known/oauth-* paths of the hub are JSON 404s, answered
	// without authentication (the root protected resource metadata is not
	// served).
	w = h.serve(req{url: hubURL + "/.well-known/oauth-protected-resource"})
	expectError(t, w, http.StatusNotFound, "not_found")
	if strings.Contains(w.Body.String(), "authorization_servers") || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("the root protected resource metadata: %q %v", w.Body.String(), w.Header())
	}

	// OAuth endpoints: unknown paths and methods.
	expectError(t, h.serve(req{method: "POST", url: hubURL + "/__backyard/oauth/foo"}), http.StatusNotFound, "not_found")
	w = h.serve(req{method: "GET", url: hubURL + "/__backyard/oauth/token"})
	expectError(t, w, http.StatusMethodNotAllowed, "bad_request")
	if w.Header().Get("Allow") != "POST" {
		t.Errorf("Allow = %q", w.Header().Get("Allow"))
	}
	w = h.serve(req{method: "OPTIONS", url: hubURL + "/__backyard/oauth/register", hdr: map[string]string{
		"Origin": "https://claude.ai", "Access-Control-Request-Method": "POST",
	}})
	expectStatus(t, w, http.StatusMethodNotAllowed)
	noCORSNoCookies(t, w)

	// The MCP endpoint is reached without authentication by the server, with
	// the request id already on the response; it answers the challenge.
	w = h.serve(req{method: "POST", url: hubURL + "/__backyard/mcp", body: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
		hdr: map[string]string{"Content-Type": "application/json", connEmailHeader: "alice@acme.com"}})
	expectError(t, w, http.StatusUnauthorized, "unauthenticated")
	if got, want := w.Header().Get("WWW-Authenticate"), `Bearer resource_metadata="`+hubURL+`/.well-known/oauth-protected-resource/__backyard/mcp", scope="mcp"`; got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
	noCORSNoCookies(t, w)
	cn.mu.Lock()
	if len(cn.seen) != 1 || !cn.reqIDSet[0] {
		t.Fatalf("endpoint calls = %d, request id set = %v", len(cn.seen), cn.reqIDSet)
	}
	r0 := cn.seen[0]
	cn.mu.Unlock()
	if RequestOrigin(r0) != hubURL || ClientIP(r0) != "192.0.2.1" {
		t.Errorf("RequestOrigin = %q, ClientIP = %q", RequestOrigin(r0), ClientIP(r0))
	}
	// Only the exact path is the endpoint.
	expectError(t, h.serve(req{method: "POST", url: hubURL + "/__backyard/mcp/"}), http.StatusNotFound, "not_found")

	// Authorize on the hub: the authenticator's pages.
	w = h.serve(req{url: hubURL + "/__backyard/auth/authorize?client_id=bydc_nope", hdr: as("alice@acme.com")})
	expectStatus(t, w, http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "Unknown app") || w.Header().Get("Location") != "" {
		t.Fatalf("authorize: %d %q %s", w.Code, w.Header().Get("Location"), truncate(w.Body.String(), 300))
	}

	// Site hosts: /.well-known stays site content; the machine paths are the
	// reserved JSON 404; authorize redirects to the hub.
	for p, want := range map[string]string{
		"/.well-known/oauth-authorization-server": "site file",
		"/.well-known/oauth-protected-resource/x": "site prm",
	} {
		w := h.serve(req{url: "https://foo.backyard.test" + p, hdr: as("alice@acme.com")})
		expectStatus(t, w, http.StatusOK)
		if w.Body.String() != want {
			t.Errorf("%s = %q", p, w.Body.String())
		}
	}
	expectStatus(t, h.serve(req{url: "https://foo.backyard.test/.well-known/oauth-protected-resource/__backyard/mcp", hdr: as("alice@acme.com")}), http.StatusNotFound)
	for _, p := range []string{"/__backyard/mcp", "/__backyard/oauth/token", "/__backyard/oauth/register"} {
		w := h.serve(req{method: "POST", url: "https://foo.backyard.test" + p})
		e := expectError(t, w, http.StatusNotFound, "not_found")
		if !strings.Contains(e.Error.Message, "no such endpoint") {
			t.Errorf("%s: %q", p, e.Error.Message)
		}
	}
	w = h.get("https://foo.backyard.test/__backyard/auth/authorize?client_id=a&state=b")
	expectStatus(t, w, http.StatusFound)
	if loc := w.Header().Get("Location"); loc != hubURL+"/__backyard/auth/authorize?client_id=a&state=b" {
		t.Errorf("Location = %q", loc)
	}
	cn.mu.Lock()
	if len(cn.seen) != 1 {
		t.Errorf("site host requests reached the MCP endpoint")
	}
	cn.mu.Unlock()

	// Metric areas.
	out := scrape(t, h)
	for _, want := range []string{
		`backyard_http_requests_total{area="oauth",code="200"}`,
		`backyard_http_requests_total{area="oauth",code="404"}`,
		`backyard_http_requests_total{area="oauth",code="405"}`,
		`backyard_http_requests_total{area="mcp",code="401"}`,
		`backyard_http_requests_total{area="auth",code="400"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %s", want)
		}
	}
}

func TestConnectorPathMode(t *testing.T) {
	cn := newConnector(t, nil, pathMode())
	h := cn.h
	h.deploy("foo", map[string]string{"index.html": "foo", ".well-known/oauth-authorization-server": "site file"})
	w := h.serve(req{url: hubURL + "/s/foo/.well-known/oauth-authorization-server", hdr: as("alice@acme.com")})
	expectStatus(t, w, http.StatusOK)
	if w.Body.String() != "site file" {
		t.Fatalf("site .well-known = %q", w.Body.String())
	}
	w = h.get(hubURL + "/.well-known/oauth-authorization-server")
	expectStatus(t, w, http.StatusOK)
	if !strings.Contains(w.Body.String(), `"issuer":"`+hubURL+`"`) {
		t.Fatalf("hub metadata = %s", w.Body.String())
	}
	e := expectError(t, h.serve(req{method: "POST", url: hubURL + "/s/foo/__backyard/mcp"}), http.StatusNotFound, "not_found")
	if !strings.Contains(e.Error.Message, "no such endpoint") {
		t.Errorf("site path MCP: %q", e.Error.Message)
	}
	tok := cn.connect("alice@acme.com", "Path app")
	expectStatus(t, cn.mcpCall(tok.access), http.StatusOK)
}

func TestConnectorNonCanonicalHub(t *testing.T) {
	// The base domain is a hub host, but the canonical host is the public URL's.
	cn := newConnector(t, func(c *config.Config) { c.Server.PublicURL = "https://hub.backyard.test" })
	h := cn.h
	for _, tc := range []struct{ method, path string }{
		{"GET", "/.well-known/oauth-authorization-server"},
		{"GET", "/.well-known/oauth-protected-resource/__backyard/mcp"},
		{"POST", "/__backyard/oauth/token"},
		{"POST", "/__backyard/mcp"},
	} {
		w := h.serve(req{method: tc.method, url: "https://backyard.test" + tc.path, body: strings.NewReader("{}"),
			hdr: map[string]string{"Content-Type": "application/json"}})
		expectError(t, w, http.StatusNotFound, "not_found")
		if strings.Contains(w.Body.String(), "not enabled") {
			t.Errorf("%s: the disabled message on a non-canonical host", tc.path)
		}
	}
	w := h.serve(req{url: "https://backyard.test/__backyard/auth/authorize?client_id=x", hdr: as("alice@acme.com")})
	expectStatus(t, w, http.StatusNotFound)
	if !strings.Contains(w.Body.String(), "https://hub.backyard.test/__backyard/mcp") {
		t.Fatalf("authorize on a non-canonical host: %s", truncate(w.Body.String(), 400))
	}
	w = h.get("https://hub.backyard.test/.well-known/oauth-authorization-server")
	expectStatus(t, w, http.StatusOK)
}

func TestConnectorRateLimits(t *testing.T) {
	cn := newConnector(t, func(c *config.Config) { c.RateLimit.APIPerMinute = 1 })
	h := cn.h
	type call struct {
		name, method, path, body, ct string
	}
	for i, c := range []call{
		{"register", "POST", "/__backyard/oauth/register", `{"redirect_uris":["` + connRedirect + `"]}`, "application/json"},
		{"token", "POST", "/__backyard/oauth/token", "grant_type=refresh_token&refresh_token=x&client_id=y", "application/x-www-form-urlencoded"},
		{"revoke", "POST", "/__backyard/oauth/revoke", "token=x&client_id=y", "application/x-www-form-urlencoded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ip := "198.51.100." + strconv.Itoa(i+1) + ":4000"
			first := h.serve(req{method: c.method, url: hubURL + c.path, body: strings.NewReader(c.body), hdr: map[string]string{"Content-Type": c.ct}, remote: ip})
			if first.Code == http.StatusTooManyRequests {
				t.Fatalf("first request limited")
			}
			w := h.serve(req{method: c.method, url: hubURL + c.path, body: strings.NewReader(c.body), hdr: map[string]string{"Content-Type": c.ct}, remote: ip})
			expectStatus(t, w, http.StatusTooManyRequests)
			if got := strings.TrimSpace(w.Body.String()); got != `{"error":"invalid_request","error_description":"too many requests"}` {
				t.Errorf("body = %s", got)
			}
			if w.Header().Get("Retry-After") == "" || w.Header().Get("Cache-Control") != "no-store" ||
				!strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
				t.Errorf("headers = %v", w.Header())
			}
			// Another address has its own budget.
			other := h.serve(req{method: c.method, url: hubURL + c.path, body: strings.NewReader(c.body), hdr: map[string]string{"Content-Type": c.ct}, remote: "203.0.113." + strconv.Itoa(i+1) + ":4000"})
			if other.Code == http.StatusTooManyRequests {
				t.Errorf("another address is limited")
			}
		})
	}
	t.Run("authorize", func(t *testing.T) {
		// Through the trusted proxy the client address is X-Forwarded-For.
		hdr := func() map[string]string {
			return map[string]string{connEmailHeader: "alice@acme.com", "X-Forwarded-For": "198.51.100.200"}
		}
		first := h.serve(req{url: hubURL + "/__backyard/auth/authorize?client_id=x", hdr: hdr()})
		expectStatus(t, first, http.StatusBadRequest)
		w := h.serve(req{url: hubURL + "/__backyard/auth/authorize?client_id=x", hdr: hdr()})
		expectStatus(t, w, http.StatusTooManyRequests)
		if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "Too many requests") {
			t.Fatalf("429 page: %q %s", w.Header().Get("Content-Type"), truncate(w.Body.String(), 300))
		}
		if w.Header().Get("Retry-After") == "" || !strings.HasPrefix(w.Header().Get("Cache-Control"), "no-store") {
			t.Errorf("headers = %v", w.Header())
		}
		// The consent POST shares the limit.
		p := hdr()
		p["Content-Type"] = "application/x-www-form-urlencoded"
		expectStatus(t, h.serve(req{method: "POST", url: hubURL + "/__backyard/auth/authorize", body: strings.NewReader("decision=allow"), hdr: p}), http.StatusTooManyRequests)
	})
	// Metadata is not limited.
	for range 3 {
		expectStatus(t, h.serve(req{url: hubURL + "/.well-known/oauth-authorization-server", remote: "198.51.100.1:4000"}), http.StatusOK)
	}
	if !strings.Contains(scrape(t, h), `backyard_rate_limited_total{category="api"} 5`) {
		t.Errorf("rejections not counted:\n%s", scrape(t, h))
	}
}

// Behind one shared address (a proxy that is not trusted, as on Cloud
// Run), junk sign-in requests that identify no one exhaust only the
// address's budget: code exchanges, refreshes and the authorize page of a
// signed-in person are charged to that person (SPEC §11).
func TestConnectorRateLimitsSharedAddress(t *testing.T) {
	cn := newConnector(t, func(c *config.Config) { c.RateLimit.APIPerMinute = 10 })
	junk := func() *httptest.ResponseRecorder {
		return cn.do("POST", hubURL+"/__backyard/oauth/token", "grant_type=refresh_token&refresh_token=x&client_id=y",
			map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	}
	alice := cn.connect("alice@acme.com", "App") // a live connection
	id := cn.register("Second app")
	code, verifier := cn.code("alice@acme.com", id)
	limited := false
	for range 20 {
		if w := junk(); w.Code == http.StatusTooManyRequests {
			if got := strings.TrimSpace(w.Body.String()); got != `{"error":"invalid_request","error_description":"too many requests"}` ||
				w.Header().Get("Retry-After") == "" || w.Header().Get("WWW-Authenticate") != "" {
				t.Fatalf("429 = %v %s", w.Header(), got)
			}
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("junk token requests were never limited")
	}
	form := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	expectStatus(t, cn.do("POST", hubURL+"/__backyard/oauth/token", cn.exchangeForm(id, code, verifier), form), http.StatusOK)
	refresh := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {alice.refresh}, "client_id": {alice.clientID}}
	expectStatus(t, cn.do("POST", hubURL+"/__backyard/oauth/token", refresh.Encode(), form), http.StatusOK)
	hdr := as("alice@acme.com")
	hdr["Sec-Fetch-Site"] = "cross-site"
	expectStatus(t, cn.do("GET", hubURL+auth.AuthorizePath+"?"+authorizeQuery(id, "v").Encode(), "", hdr), http.StatusOK)
	// Requests that identify no one stay limited: junk, an authorization
	// request that fails before the person is asked, a registration.
	expectStatus(t, junk(), http.StatusTooManyRequests)
	expectStatus(t, cn.do("GET", hubURL+auth.AuthorizePath+"?client_id=x", "", as("bob@acme.com")), http.StatusTooManyRequests)
	expectStatus(t, cn.do("POST", hubURL+"/__backyard/oauth/register", `{"redirect_uris":["`+connRedirect+`"]}`,
		map[string]string{"Content-Type": "application/json"}), http.StatusTooManyRequests)
}

func TestAllowAPI(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.APIPerMinute = 1 }))
	if ok, _ := h.srv.AllowAPI("user:alice"); !ok {
		t.Fatal("first call refused")
	}
	ok, wait := h.srv.AllowAPI("user:alice")
	if ok || wait <= 0 {
		t.Fatalf("second call: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := h.srv.AllowAPI("user:bob"); !ok {
		t.Fatal("another key refused")
	}
	// The per-user MCP key is not the site API's: alice's API calls go on.
	expectStatus(t, h.get("https://foo.backyard.test/__backyard/api/me"), http.StatusOK)
	if !strings.Contains(scrape(t, h), `backyard_rate_limited_total{category="api"} 1`) {
		t.Error("refusal not counted")
	}
	var nilSrv *Server
	if ok, _ := nilSrv.AllowAPI("user:x"); !ok {
		t.Error("a nil server refuses")
	}
}

func TestRequestOriginAndClientIPOutsideServer(t *testing.T) {
	r := httptest.NewRequest("POST", "https://Hub.Example:443/__backyard/mcp", nil)
	r.RemoteAddr = "203.0.113.9:5555"
	if got := RequestOrigin(r); got != "https://hub.example" {
		t.Errorf("RequestOrigin = %q", got)
	}
	if got := ClientIP(r); got != "203.0.113.9" {
		t.Errorf("ClientIP = %q", got)
	}
	// Inside the server, trusted proxy headers count.
	cn := newConnector(t, nil)
	cn.h.serve(req{method: "POST", url: "http://10.0.0.1/__backyard/mcp", hdr: map[string]string{
		"X-Forwarded-Host": "backyard.test", "X-Forwarded-Proto": "https", "X-Forwarded-For": "198.51.100.50",
	}})
	cn.mu.Lock()
	defer cn.mu.Unlock()
	if len(cn.seen) != 1 {
		t.Fatalf("endpoint calls = %d", len(cn.seen))
	}
	if RequestOrigin(cn.seen[0]) != hubURL || ClientIP(cn.seen[0]) != "198.51.100.50" {
		t.Errorf("RequestOrigin = %q, ClientIP = %q", RequestOrigin(cn.seen[0]), ClientIP(cn.seen[0]))
	}
}

func TestConnectorGrantsAPI(t *testing.T) {
	cn := newConnector(t, nil)
	a1 := cn.connect("alice@acme.com", "First app")
	a2 := cn.connect("alice@acme.com", "Second app")
	b1 := cn.connect("bob@acme.com", "Bob's app")
	for _, tok := range []tokens{a1, a2, b1} {
		expectStatus(t, cn.mcpCall(tok.access), http.StatusOK)
	}

	// Own list, newest first, in the documented shape.
	w := cn.grants("alice@acme.com", "")
	expectStatus(t, w, http.StatusOK)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	list := decode[grantList](t, w)
	if len(list.Grants) != 2 || list.Grants[0].ClientName != "Second app" || list.Grants[1].ClientName != "First app" {
		t.Fatalf("alice's grants = %+v", list.Grants)
	}
	g := list.Grants[0]
	created, err1 := time.Parse(time.RFC3339, g.CreatedAt)
	expires, err2 := time.Parse(time.RFC3339, g.ExpiresAt)
	if len(g.ID) != 32 || g.User.ID != "alice@acme.com" || g.User.Email != "alice@acme.com" || g.ClientKind != "registered" ||
		g.ClientHost != "" || g.RedirectHost != "127.0.0.1:9" || g.Scope != "mcp offline_access" ||
		err1 != nil || err2 != nil || expires.Sub(created) != 720*time.Hour || !strings.HasSuffix(g.CreatedAt, "Z") {
		t.Fatalf("grant = %+v", g)
	}
	body := cn.grants("alice@acme.com", "").Body.String()
	for _, secret := range []string{a1.access, a1.refresh, a1.clientID, "byat_", "byrt_", "bydc_"} {
		if strings.Contains(body, secret) {
			t.Fatalf("the grants API leaks %q", truncate(secret, 12))
		}
	}

	// ?user= on GET is for admins only, even naming oneself (SPEC §8.4.7).
	expectError(t, cn.grants("alice@acme.com", "alice@acme.com"), http.StatusForbidden, "forbidden")
	expectError(t, cn.grants("alice@acme.com", "bob@acme.com"), http.StatusForbidden, "forbidden")
	bl := decode[grantList](t, cn.grants("boss@acme.com", "bob@acme.com"))
	if len(bl.Grants) != 1 || bl.Grants[0].ClientName != "Bob's app" {
		t.Fatalf("admin view of bob = %+v", bl.Grants)
	}
	if l := decode[grantList](t, cn.grants("carol@acme.com", "")); len(l.Grants) != 0 {
		t.Errorf("carol's grants = %+v", l.Grants)
	}

	// A static admin token owns nothing but may look at anyone.
	static := map[string]string{"Authorization": "Bearer " + staticAdminTok, "RemoteAddr": "198.51.100.9:1"}
	if l := decode[grantList](t, cn.do("GET", hubURL+"/__backyard/api/mcp/grants", "", static)); len(l.Grants) != 0 {
		t.Errorf("static token's own grants = %+v", l.Grants)
	}
	static = map[string]string{"Authorization": "Bearer " + staticAdminTok, "RemoteAddr": "198.51.100.9:1"}
	if l := decode[grantList](t, cn.do("GET", hubURL+"/__backyard/api/mcp/grants?user=alice%40acme.com", "", static)); len(l.Grants) != 2 {
		t.Errorf("static admin view of alice = %d grants", len(l.Grants))
	}

	// MCP access tokens are not API credentials.
	for _, p := range []string{"/__backyard/api/mcp/grants", "/__backyard/api/me"} {
		w := cn.do("GET", hubURL+p, "", map[string]string{"Authorization": "Bearer " + a1.access, "RemoteAddr": "198.51.100.9:1"})
		expectError(t, w, http.StatusUnauthorized, "unauthenticated")
	}

	// DELETE one: CSRF header required; someone else's and unknown ids are 404.
	hdr := as("alice@acme.com")
	expectError(t, cn.do("DELETE", hubURL+"/__backyard/api/mcp/grants/"+list.Grants[1].ID, "", hdr), http.StatusForbidden, "forbidden")
	bobID := bl.Grants[0].ID
	expectError(t, cn.revoke("alice@acme.com", "/"+bobID), http.StatusNotFound, "not_found")
	expectError(t, cn.revoke("alice@acme.com", "/0123456789abcdef0123456789abcdef"), http.StatusNotFound, "not_found")
	expectError(t, cn.revoke("alice@acme.com", "/not-a-grant-id"), http.StatusNotFound, "not_found")
	expectStatus(t, cn.mcpCall(b1.access), http.StatusOK)

	first := list.Grants[1].ID
	w = cn.revoke("alice@acme.com", "/"+first)
	expectStatus(t, w, http.StatusNoContent)
	if w.Body.Len() != 0 {
		t.Errorf("204 with a body: %q", w.Body.String())
	}
	// Immediate: the access token of that connection stops working.
	w = cn.mcpCall(a1.access)
	expectStatus(t, w, http.StatusUnauthorized)
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), `error_description="token revoked"`) {
		t.Errorf("WWW-Authenticate = %q", w.Header().Get("WWW-Authenticate"))
	}
	expectStatus(t, cn.mcpCall(a2.access), http.StatusOK)
	expectError(t, cn.revoke("alice@acme.com", "/"+first), http.StatusNotFound, "not_found")
	audits := cn.revokeAudits(t)
	if len(audits) != 1 || audits[0]["via"] != "hub" || audits[0]["user"] != "alice@acme.com" {
		t.Fatalf("revoke audits = %v", audits)
	}
	if ip, _ := audits[0]["ip"].(string); ip == "" || ip == "unknown" {
		t.Errorf("hub revoke audit ip = %v", audits[0]["ip"])
	}

	// Bulk: own connections (naming oneself is fine on DELETE); another
	// person's only for admins.
	expectError(t, cn.revoke("alice@acme.com", "?user=bob%40acme.com"), http.StatusForbidden, "forbidden")
	w = cn.revoke("alice@acme.com", "?user=alice%40acme.com")
	expectStatus(t, w, http.StatusOK)
	if got := strings.TrimSpace(w.Body.String()); got != `{"revoked":1}` {
		t.Fatalf("bulk revoke = %s", got)
	}
	expectStatus(t, cn.mcpCall(a2.access), http.StatusUnauthorized)
	w = cn.revoke("boss@acme.com", "?user=bob%40acme.com")
	expectStatus(t, w, http.StatusOK)
	if got := strings.TrimSpace(w.Body.String()); got != `{"revoked":1}` {
		t.Fatalf("admin bulk revoke = %s", got)
	}
	expectStatus(t, cn.mcpCall(b1.access), http.StatusUnauthorized)
	if l := decode[grantList](t, cn.grants("boss@acme.com", "bob@acme.com")); len(l.Grants) != 0 {
		t.Errorf("bob still has %d grants", len(l.Grants))
	}
	audits = cn.revokeAudits(t)
	bulk := 0
	for _, a := range audits {
		if a["via"] != "hub" {
			t.Errorf("audit via = %v", a["via"])
		}
		if a["subject"] == "bob@acme.com" && a["user"] == "boss@acme.com" {
			bulk++
		}
	}
	if len(audits) != 3 || bulk != 1 {
		t.Errorf("revoke audits = %v", audits)
	}
	// The server itself wrote no connector audit records.
	if n := cn.h.docs.collectionLen("_system", "_audit"); n != 0 {
		t.Errorf("the server audited %d records", n)
	}

	// An admin disconnects one of someone else's connections.
	c1 := cn.connect("carol@acme.com", "Carol's app")
	cl := decode[grantList](t, cn.grants("carol@acme.com", ""))
	expectStatus(t, cn.revoke("boss@acme.com", "/"+cl.Grants[0].ID), http.StatusNoContent)
	expectStatus(t, cn.mcpCall(c1.access), http.StatusUnauthorized)
}

func TestConnectorGrantsAPIExpired(t *testing.T) {
	cn := newConnector(t, func(c *config.Config) { c.Auth.TokenTTL = config.Duration(48 * time.Hour) })
	cn.connect("alice@acme.com", "App")
	if l := decode[grantList](t, cn.grants("alice@acme.com", "")); len(l.Grants) != 1 {
		t.Fatalf("grants = %d", len(l.Grants))
	}
	cn.clock.advance(49 * time.Hour)
	if l := decode[grantList](t, cn.grants("alice@acme.com", "")); len(l.Grants) != 0 {
		t.Fatalf("an expired grant is listed: %+v", l.Grants)
	}
}
