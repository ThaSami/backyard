package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
)

func TestMCPRegister(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	for _, uri := range []string{claudeCB, "https://claude.com/api/mcp/auth_callback", "http://localhost/callback",
		"http://127.0.0.1:53682/callback", "http://[::1]:1/cb", "http://localhost:8080", "http://localhost/cb?x=1"} {
		res := e.do(postJSON(OAuthRegisterPath, `{"client_name":"Claude Code","redirect_uris":["`+uri+`"]}`))
		m := jsonBody(t, res)
		if res.StatusCode != http.StatusCreated {
			t.Errorf("register %s = %d %v", uri, res.StatusCode, m)
			continue
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Content-Type") != "application/json" {
			t.Errorf("register headers = %v", res.Header)
		}
		id, _ := m["client_id"].(string)
		if !strings.HasPrefix(id, "bydc_") || len(id) > 1024 || m["client_name"] != "Claude Code" ||
			m["token_endpoint_auth_method"] != "none" || m["client_id_issued_at"] != float64(e.clock.Now().Unix()) {
			t.Errorf("register body = %v", m)
		}
		if fmt.Sprint(m["redirect_uris"]) != "["+uri+"]" || fmt.Sprint(m["grant_types"]) != "[authorization_code refresh_token]" ||
			fmt.Sprint(m["response_types"]) != "[code]" {
			t.Errorf("register lists = %v", m)
		}
		c, ok := e.m.verifyDCRClient(id)
		if !ok || c.name != "Claude Code" || c.kind != clientKindDCR || len(c.redirects) != 1 || c.redirects[0] != uri {
			t.Errorf("verifyDCRClient = %+v %v", c, ok)
		}
	}

	refused := map[string]struct{ body, code string }{
		"foreign https":        {`{"redirect_uris":["https://evil.example/cb"]}`, "invalid_redirect_uri"},
		"not exact":            {`{"redirect_uris":["https://claude.ai/other"]}`, "invalid_redirect_uri"},
		"trailing slash":       {`{"redirect_uris":["https://claude.ai/api/mcp/auth_callback/"]}`, "invalid_redirect_uri"},
		"http non-loopback":    {`{"redirect_uris":["http://claude.ai/x"]}`, "invalid_redirect_uri"},
		"custom scheme":        {`{"redirect_uris":["cursor://cb"]}`, "invalid_redirect_uri"},
		"fragment":             {`{"redirect_uris":["http://localhost/cb#x"]}`, "invalid_redirect_uri"},
		"userinfo":             {`{"redirect_uris":["http://u@localhost/cb"]}`, "invalid_redirect_uri"},
		"loopback lookalike":   {`{"redirect_uris":["http://localhost.evil.example/cb"]}`, "invalid_redirect_uri"},
		"uppercase localhost":  {`{"redirect_uris":["http://LOCALHOST/cb"]}`, "invalid_redirect_uri"},
		"bad port":             {`{"redirect_uris":["http://localhost:0/cb"]}`, "invalid_redirect_uri"},
		"five uris":            {`{"redirect_uris":["http://localhost/1","http://localhost/2","http://localhost/3","http://localhost/4","http://localhost/5"]}`, "invalid_redirect_uri"},
		"513 bytes":            {`{"redirect_uris":["http://localhost/` + strings.Repeat("a", 513-len("http://localhost/")) + `"]}`, "invalid_redirect_uri"},
		"no uris":              {`{"client_name":"x"}`, "invalid_redirect_uri"},
		"empty uris":           {`{"redirect_uris":[]}`, "invalid_redirect_uri"},
		"uris not strings":     {`{"redirect_uris":[1]}`, "invalid_redirect_uri"},
		"not an object":        {`["http://localhost/cb"]`, "invalid_client_metadata"},
		"not json":             {`redirect_uris=http://localhost/cb`, "invalid_client_metadata"},
		"trailing data":        {`{"redirect_uris":["http://localhost/cb"]} {}`, "invalid_client_metadata"},
		"null":                 {`null`, "invalid_client_metadata"},
		"leading BOM":          {"\ufeff" + `{"redirect_uris":["http://localhost/cb"]}`, "invalid_client_metadata"},
		"malformed escape":     {`{"redirect_uris":["http://localhost/%zz"]}`, "invalid_redirect_uri"},
		"control character":    {`{"redirect_uris":["http://localhost/a\u0001b"]}`, "invalid_redirect_uri"},
		"name not a string":    {`{"client_name":5,"redirect_uris":["http://localhost/cb"]}`, "invalid_client_metadata"},
		"client_id over limit": {`{"redirect_uris":["http://localhost/` + strings.Repeat("a", 490) + `","http://localhost/` + strings.Repeat("b", 490) + `"]}`, "invalid_client_metadata"},
	}
	for name, tc := range refused {
		res := e.do(postJSON(OAuthRegisterPath, tc.body))
		m := jsonBody(t, res)
		if res.StatusCode != http.StatusBadRequest || m["error"] != tc.code {
			t.Errorf("%s: %d %v, want 400 %s", name, res.StatusCode, m, tc.code)
		}
	}
	if m := jsonBody(t, e.do(postJSON(OAuthRegisterPath, refused["foreign https"].body))); m["error_description"] !=
		"redirect URI https://evil.example/cb is not allowed on this server (allowed: the URIs in mcp.redirect_uris and http loopback URIs)" {
		t.Errorf("invalid_redirect_uri description = %q", m["error_description"])
	}
	if m := jsonBody(t, e.do(postJSON(OAuthRegisterPath, refused["client_id over limit"].body))); m["error_description"] != "too many or too long redirect_uris" {
		t.Errorf("client_id limit description = %q", m["error_description"])
	}
	// text/plain and a body over 16 KiB.
	r := postJSON(OAuthRegisterPath, `{"redirect_uris":["http://localhost/cb"]}`)
	r.Header.Set("Content-Type", "text/plain")
	if res := e.do(r); res.StatusCode != http.StatusBadRequest || jsonBody(t, res)["error"] != "invalid_client_metadata" {
		t.Errorf("text/plain: %d", res.StatusCode)
	}
	r = postJSON(OAuthRegisterPath, `{"redirect_uris":["http://localhost/cb"]}`)
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	if res := e.do(r); res.StatusCode != http.StatusCreated {
		t.Errorf("json with parameters: %d", res.StatusCode)
	}
	big := `{"x":"` + strings.Repeat("y", 16<<10) + `","redirect_uris":["http://localhost/cb"]}`
	if res := e.do(postJSON(OAuthRegisterPath, big)); res.StatusCode != http.StatusBadRequest {
		t.Errorf("17 KiB body: %d", res.StatusCode)
	}

	// Unsupported metadata is accepted and replaced by what was registered.
	res := e.do(postJSON(OAuthRegisterPath, `{"redirect_uris":["http://localhost/cb"],"token_endpoint_auth_method":"client_secret_basic",`+
		`"grant_types":["authorization_code","urn:ietf:params:oauth:grant-type:jwt-bearer"],"response_types":["token"],`+
		`"scope":"admin","jwks_uri":"https://evil.example/jwks","software_statement":"x.y.z","logo_uri":"https://x/l.png"}`))
	m := jsonBody(t, res)
	if res.StatusCode != http.StatusCreated || m["token_endpoint_auth_method"] != "none" ||
		fmt.Sprint(m["grant_types"]) != "[authorization_code refresh_token]" || fmt.Sprint(m["response_types"]) != "[code]" ||
		m["client_name"] != "MCP client" {
		t.Errorf("replaced metadata = %d %v", res.StatusCode, m)
	}
	if _, ok := m["scope"]; ok {
		t.Error("the response must not echo unsupported members")
	}

	// Wrong method, unknown OAuth path, foreign host.
	if res := e.do(req("GET", testIssuer+OAuthRegisterPath)); res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "POST" {
		t.Errorf("GET register = %d %q", res.StatusCode, res.Header.Get("Allow"))
	}
	if res := e.do(oauthPost("/__backyard/oauth/foo", nil)); res.StatusCode != http.StatusNotFound {
		t.Errorf("/oauth/foo = %d", res.StatusCode)
	}
	r = postJSON(OAuthRegisterPath, `{"redirect_uris":["http://localhost/cb"]}`)
	r.Host = "site.backyard.test"
	if res := e.do(r); res.StatusCode != http.StatusNotFound {
		t.Errorf("register on a site host = %d", res.StatusCode)
	}
}

func TestMCPClientNameSanitizing(t *testing.T) {
	rle, lri, lrm, nul := string(rune(0x202E)), string(rune(0x2066)), string(rune(0x200E)), string(rune(0))
	cases := map[string]string{
		"Claude":                              "Claude",
		"  Claude Code  ":                     "Claude Code",
		"Cla" + rle + "ude" + lri + "x" + lrm: "Claudex",
		"a\tb\nc" + nul + "d\x7f":             "abcd",
		"":                                    "MCP client",
		"   " + rle:                           "MCP client",
		strings.Repeat("é", 65):               strings.Repeat("é", 64),
		strings.Repeat("x", 63) + "  yz":      strings.Repeat("x", 63),
		"bad \xff utf8":                       "bad " + string(rune(0xFFFD)) + " utf8",
		"Ünïcødé 名前":                          "Ünïcødé 名前",
	}
	for in, want := range cases {
		if got := sanitizeClientName(in); got != want {
			t.Errorf("sanitizeClientName(%q) = %q, want %q", in, got, want)
		}
	}
	// The registered name is sanitized, and shown sanitized at consent.
	e := newMCPEnv(t, ModeNone, nil)
	name := "Evil" + rle + "App" + strings.Repeat("z", 70)
	b, _ := json.Marshal(map[string]any{"client_name": name, "redirect_uris": []string{loopbackCB}})
	m := jsonBody(t, e.do(postJSON(OAuthRegisterPath, string(b))))
	want := ("EvilApp" + strings.Repeat("z", 70))[:64]
	if m["client_name"] != want {
		t.Errorf("registered name = %q, want %q", m["client_name"], want)
	}
}

func TestMCPTamperedClientID(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("App", loopbackCB)
	_, c := pkce()
	tampered := []string{
		id[:len(id)-2] + "xx",
		"bydc_" + strings.Replace(id[5:], ".", "x.", 1),
		strings.Replace(id, "bydc_", "bydx_", 1),
		"bydc_",
		"",
		"some-client",
	}
	// A DCR id signed with another deployment's secret.
	other := newMCPEnv(t, ModeNone, func(c *config.Config) { c.Server.Secret = strings.Repeat("o", 40) })
	tampered = append(tampered, other.register("App", loopbackCB))
	for _, bad := range tampered {
		res := e.authorize(newBrowser(), authzParams(bad, loopbackCB, c, "s"))
		if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(body(t, res), "Unknown app") {
			t.Errorf("client_id %q: %d %q", bad, res.StatusCode, res.Header.Get("Location"))
		}
	}
}

func TestMCPRedirectMatching(t *testing.T) {
	cases := []struct {
		registered, requested string
		match                 bool
	}{
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback", true},
		{"https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback/", false},
		{"https://claude.ai/api/mcp/auth_callback", "https://CLAUDE.ai/api/mcp/auth_callback", false},
		{"http://localhost/callback", "http://localhost:53682/callback", true},
		{"http://127.0.0.1:9/callback", "http://127.0.0.1:41000/callback", true},
		{"http://localhost/callback", "http://127.0.0.1:53682/callback", false},
		{"http://[::1]/cb", "http://[::1]:1/cb", true},
		{"http://localhost/callback", "http://localhost:1/callback?x=1", false},
		{"http://localhost/callback?a=1", "http://localhost:5/callback?a=1", true},
		{"http://localhost/callback", "https://localhost/callback", false},
		// More: fragments and userinfo never match; ports are not wildcards
		// for https.
		{"http://localhost/cb", "http://localhost:5/cb#frag", false},
		{"http://localhost/cb", "http://u@localhost:5/cb", false},
		{"https://claude.ai:443/cb", "https://claude.ai/cb", false},
		{"http://localhost:5/cb", "http://localhost:5/cb", true},
		{"http://localhost", "http://localhost:8/", false},
		{"http://localhost", "http://localhost:8", true},
	}
	for _, tc := range cases {
		if got := redirectMatches(tc.registered, tc.requested); got != tc.match {
			t.Errorf("redirectMatches(%q, %q) = %v, want %v", tc.registered, tc.requested, got, tc.match)
		}
	}
}

func TestMCPRedirectPolicy(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	for uri, want := range map[string]bool{
		claudeCB: true, "https://claude.com/api/mcp/auth_callback": true,
		"https://claude.ai/api/mcp/auth_callback/": false, "https://claude.ai/api/mcp/auth_callback?x=1": false,
		"http://localhost/cb": true, "http://127.0.0.1/cb": true, "http://[::1]/cb": true, "http://localhost:65535/x?y=z": true,
		"http://localhost:65536/cb": false, "http://localhost:/cb": false, "http://localhost:-1/cb": false,
		"http://127.0.0.2/cb": false, "http://[::1]x/cb": false, "http://localhost.:5/cb": false,
		"http://localhost/c b": false, "http://localhost\\@evil.example/": false, "https://localhost/cb": false,
		"HTTP://localhost/cb": false, "javascript:alert(1)": false, "": false,
		// Not RFC 3986 (url.Parse fails) or a space: refused on both editions.
		"http://localhost/%zz": false, "http://localhost/a%2": false, "http://localhost/a\u0001b": false,
		"http://localhost/a\u007fb": false, "http://localhost/a\u0085b": false, "http://localhost/?a=\u0001": false,
		// Query escapes are not checked; U+FEFF is not a space.
		"http://localhost/?a=%zz": true, "http://localhost?a=%zz": true, "http://localhost/a%2Fb": true,
		"http://localhost/a\ufeffb": true,
	} {
		if got := e.m.redirectAllowed(uri); got != want {
			t.Errorf("redirectAllowed(%q) = %v, want %v", uri, got, want)
		}
	}
	// mcp.redirect_uris: [] leaves loopback only.
	e = newMCPEnv(t, ModeNone, func(c *config.Config) { c.MCP.RedirectURIs = []string{} })
	if e.m.redirectAllowed(claudeCB) || !e.m.redirectAllowed(loopbackCB) {
		t.Error("empty mcp.redirect_uris")
	}
}

func TestMCPResourceCanonicalization(t *testing.T) {
	e := newMCPEnv(t, ModeNone, func(c *config.Config) { c.Server.PublicURL = "https://hub.example" })
	for in, valid := range map[string]bool{
		"https://hub.example/__backyard/mcp":       true,
		"HTTPS://HUB.EXAMPLE:443/__backyard/mcp/":  true,
		"https://hub.example/__backyard/mcp?x=1":   false,
		"https://hub.example/__backyard/mcp#f":     false,
		"http://hub.example/__backyard/mcp":        false,
		"https://other.example/__backyard/mcp":     false,
		"https://hub.example/__backyard/MCP":       false,
		"https://u@hub.example/__backyard/mcp":     false,
		"":                                         false,
		"https://hub.example/__backyard/mcp//":     false,
		"https://hub.example:8443/__backyard/mcp":  false,
		"https://hub.example/__backyard/%6dcp":     false,
		"https://hub.example/__backyard/mcp?":      false,
		"hub.example/__backyard/mcp":               false,
		"https://hub.example/x/../__backyard/mcp":  false,
		"https://hub.example./__backyard/mcp":      false,
		"https://hub.example/__backyard/mcp/extra": false,
	} {
		if got := e.m.resourceOK(in); got != valid {
			c, _ := canonicalResource(in)
			t.Errorf("resourceOK(%q) = %v (canonical %q), want %v", in, got, c, valid)
		}
	}
}

// cimdServer serves a client metadata document over TLS and counts fetches.
type cimdServer struct {
	*httptest.Server
	mu      sync.Mutex
	hits    int
	status  int
	ctype   string
	cache   string
	body    func(id string) string
	release chan struct{} // when set, requests block until it is closed
}

func newCIMDServer(t *testing.T) *cimdServer {
	t.Helper()
	s := &cimdServer{status: http.StatusOK, ctype: "application/json"}
	s.body = func(id string) string {
		return `{"client_id":"` + id + `","client_name":"Claude","redirect_uris":["` + claudeCB + `","http://localhost/callback"],` +
			`"grant_types":["authorization_code","refresh_token","urn:ietf:params:oauth:grant-type:jwt-bearer"],` +
			`"token_endpoint_auth_method":"none","logo_uri":"https://claude.ai/logo.png"}`
	}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		status, ctype, cache, rel := s.status, s.ctype, s.cache, s.release
		s.mu.Unlock()
		if rel != nil {
			<-rel
		}
		if r.Header.Get("Accept") != "application/json" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			http.Error(w, "unexpected request headers", http.StatusTeapot)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, s.URL+"/client", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", ctype)
		if cache != "" {
			w.Header().Set("Cache-Control", cache)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(s.body("https://" + r.Host + r.URL.Path)))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *cimdServer) set(f func(s *cimdServer)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *cimdServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// cimdEnv is a hub whose mcp.clients allowlists the test server's /client
// (and /redirect) documents.
func cimdEnv(t *testing.T, s *cimdServer) (*mcpEnv, string) {
	t.Helper()
	id := s.URL + "/client"
	e := newMCPEnv(t, ModeNone, func(c *config.Config) {
		c.MCP.Clients = []string{id, s.URL + "/redirect"}
	}, func(o *Options) { o.MCPHTTPClient = s.Client() })
	return e, id
}

func TestMCPCIMD(t *testing.T) {
	s := newCIMDServer(t)
	e, id := cimdEnv(t, s)
	_, ch := pkce()

	// Success: the consent page shows the verified name and host.
	res := e.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s1"))
	_, page := consentFields(t, res)
	host := strings.TrimPrefix(s.URL, "https://")
	if !strings.Contains(page, "<strong>Claude</strong> — verified by <strong>"+host+"</strong>") {
		t.Errorf("consent app line:\n%s", page)
	}
	// Cached: no second fetch; a loopback redirect of the document works.
	consentFields(t, e.authorize(newBrowser(), authzParams(id, "http://localhost:4444/callback", ch, "s2")))
	if s.count() != 1 {
		t.Errorf("fetches = %d, want 1 (cached)", s.count())
	}
	// A redirect URI the document does not list.
	res = e.authorize(newBrowser(), authzParams(id, "http://localhost:4444/other", ch, "s3"))
	if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(body(t, res), "Redirect address not allowed") {
		t.Errorf("unlisted redirect: %d", res.StatusCode)
	}

	// Not allowlisted: never fetched, never redirected. A client metadata
	// URL is not allowed with a redirect that leaves this computer; anything
	// else (here an IP literal host, a query) is unknown.
	before := s.count()
	for other, title := range map[string]string{
		"https://example.com/x": "App not allowed", "https://localhost/client": "App not allowed",
		s.URL + "/other": "Unknown app", id + "/": "Unknown app", id + "?x=1": "Unknown app",
	} {
		res := e.authorize(newBrowser(), authzParams(other, claudeCB, ch, "s"))
		if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(body(t, res), title) {
			t.Errorf("non-allowlisted %s: %d, want %s", other, res.StatusCode, title)
		}
	}
	// Only "https://" (exactly) names a metadata document.
	if res := e.authorize(newBrowser(), authzParams(strings.ToUpper(id[:5])+id[5:], claudeCB, ch, "s")); res.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body(t, res), "Unknown app") {
		t.Errorf("HTTPS:// client_id: %d", res.StatusCode)
	}
	if s.count() != before {
		t.Errorf("a non-allowlisted URL was fetched (%d → %d)", before, s.count())
	}
	if !strings.Contains(body(t, e.authorize(newBrowser(), authzParams("https://example.com/x", claudeCB, ch, ""))), "mcp.clients") {
		t.Error("the App not allowed page must name mcp.clients")
	}
}

func TestMCPCIMDFailures(t *testing.T) {
	_, ch := pkce()
	doc := func(members string) func(s *cimdServer) {
		return func(s *cimdServer) {
			s.body = func(id string) string { return `{"client_id":"` + id + `",` + members + `}` }
		}
	}
	cases := []struct {
		name   string
		setup  func(s *cimdServer)
		path   string
		reason string
		rules  bool // loaded, but it breaks the document rules (else: the fetch failed)
	}{
		{"client_id mismatch", func(s *cimdServer) {
			s.body = func(string) string { return `{"client_id":"https://claude.ai/x","redirect_uris":["` + claudeCB + `"]}` }
		}, "/client", "client_id", true},
		{"redirect", nil, "/redirect", "redirect", false},
		{"non-200", func(s *cimdServer) { s.status = http.StatusNotFound }, "/client", "HTTP 404", false},
		{"wrong content type", func(s *cimdServer) { s.ctype = "text/html" }, "/client", "not JSON", false},
		{"too large", func(s *cimdServer) {
			s.body = func(id string) string {
				doc := `{"client_id":"` + id + `","redirect_uris":["` + claudeCB + `"],"pad":"`
				return doc + strings.Repeat("x", 5121-len(doc)-2) + `"}`
			}
		}, "/client", "5120 bytes", false},
		{"private_key_jwt", doc(`"redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_method":"private_key_jwt"`),
			"/client", "client authentication", true},
		{"methods without none", doc(`"redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_method":"none",` +
			`"token_endpoint_auth_methods_supported":["private_key_jwt"]`), "/client", "client authentication", true},
		{"methods not strings", doc(`"redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_methods_supported":"none"`),
			"/client", "token_endpoint_auth_methods_supported is not an array of strings", true},
		{"methods null", doc(`"redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_methods_supported":null`),
			"/client", "token_endpoint_auth_methods_supported is not an array of strings", true},
		// Every member must be a string, as on the Worker (null is not "").
		{"methods with null", doc(`"redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_methods_supported":["none",null]`),
			"/client", "token_endpoint_auth_methods_supported is not an array of strings", true},
		{"methods with a number", doc(`"redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_methods_supported":["none",1]`),
			"/client", "token_endpoint_auth_methods_supported is not an array of strings", true},
		{"client secret", doc(`"redirect_uris":["` + claudeCB + `"],"client_secret":"s3cret"`), "/client", "client secret", true},
		{"no allowed redirect", doc(`"redirect_uris":["https://evil.example/cb"]`), "/client", "redirect_uris", true},
		{"eleven redirects", doc(`"redirect_uris":[` + strings.Repeat(`"`+claudeCB+`",`, 10) + `"` + claudeCB + `"]`),
			"/client", "redirect_uris", true},
		{"redirect_uris with null", doc(`"redirect_uris":["` + claudeCB + `",null]`),
			"/client", "redirect_uris must be an array of 1 to 10 URIs", true},
		{"redirect_uris with a number", doc(`"redirect_uris":[1,"` + claudeCB + `"]`),
			"/client", "redirect_uris must be an array of 1 to 10 URIs", true},
		{"redirect_uris null", doc(`"redirect_uris":null`), "/client", "redirect_uris must be an array of 1 to 10 URIs", true},
		{"redirect_uris a string", doc(`"redirect_uris":"` + claudeCB + `"`), "/client", "redirect_uris must be an array of 1 to 10 URIs", true},
		{"client_name not a string", doc(`"redirect_uris":["` + claudeCB + `"],"client_name":7`), "/client", "client_name", true},
		{"not an object", func(s *cimdServer) { s.body = func(string) string { return `[1]` } }, "/client", "JSON object", true},
		{"not UTF-8", func(s *cimdServer) {
			s.body = func(id string) string { return `{"client_id":"` + id + `","client_name":"a` + "\xff" + `"}` }
		}, "/client", "UTF-8", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCIMDServer(t)
			if tc.setup != nil {
				s.set(tc.setup)
			}
			e, _ := cimdEnv(t, s)
			res := e.authorize(newBrowser(), authzParams(s.URL+tc.path, claudeCB, ch, "s"))
			b := html.UnescapeString(body(t, res))
			if res.StatusCode != http.StatusBadGateway || res.Header.Get("Location") != "" || !strings.Contains(b, "Could not verify the app") ||
				!strings.Contains(b, tc.reason) || !strings.Contains(b, "Register automatically") || !strings.Contains(b, s.URL+tc.path) {
				t.Errorf("%d %q:\n%s", res.StatusCode, res.Header.Get("Location"), b)
			}
			// The detail names the remedy of the cause.
			want, other := cimdFetchDetail, cimdRulesDetail
			if tc.rules {
				want, other = other, want
			}
			if !strings.Contains(b, want) || strings.Contains(b, other) {
				t.Errorf("detail: want %q in\n%s", want, b)
			}
			// Within the 10 s failure throttle the same failure, with the
			// same detail, is served without a fetch.
			n := s.count()
			e.clock.Advance(time.Second)
			res = e.authorize(newBrowser(), authzParams(s.URL+tc.path, claudeCB, ch, "s"))
			b = html.UnescapeString(body(t, res))
			if res.StatusCode != http.StatusBadGateway || !strings.Contains(b, tc.reason) || !strings.Contains(b, want) ||
				strings.Contains(b, other) || s.count() != n {
				t.Errorf("throttled: %d, fetches %d → %d; want %q in\n%s", res.StatusCode, n, s.count(), want, b)
			}
		})
	}
	// The exact 5120-byte document is accepted; unknown grant types and
	// members are ignored; a +json media type is JSON.
	s := newCIMDServer(t)
	s.set(func(s *cimdServer) {
		s.ctype = "application/vnd.example+json; charset=utf-8"
		s.body = func(id string) string {
			doc := `{"client_id":"` + id + `","client_name":"Big","redirect_uris":["` + claudeCB + `"],"grant_types":["urn:x"],"pad":"`
			return doc + strings.Repeat("x", 5120-len(doc)-2) + `"}`
		}
	})
	e, id := cimdEnv(t, s)
	if res := e.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s")); res.StatusCode != http.StatusOK {
		t.Errorf("5120-byte document: %d %s", res.StatusCode, body(t, res))
	}
}

// ChatGPT's published document (https://chatgpt.com/oauth/client.json)
// prefers private_key_jwt in the singular member and lists none among
// token_endpoint_auth_methods_supported (MCP SEP-3149): the list wins.
func TestMCPCIMDAuthMethodsList(t *testing.T) {
	_, ch := pkce()
	s := newCIMDServer(t)
	s.set(func(s *cimdServer) {
		s.body = func(id string) string {
			return `{"client_id":"` + id + `","client_uri":"https://chatgpt.com/","redirect_uris":["` + claudeCB + `"],` +
				`"token_endpoint_auth_method":"private_key_jwt","token_endpoint_auth_methods_supported":["none","private_key_jwt"],` +
				`"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"client_name":"ChatGPT",` +
				`"logo_uri":"https://persistent.oaistatic.com/sonic/misc/openai-logo.png","token_endpoint_auth_signing_alg":"RS256",` +
				`"jwks_uri":"https://chatgpt.com/oauth/jwks.json"}`
		}
	})
	e, id := cimdEnv(t, s)
	res := e.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s"))
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "ChatGPT") {
		t.Errorf("ChatGPT's document: %d %s", res.StatusCode, b)
	}
}

func TestMCPCIMDCache(t *testing.T) {
	_, ch := pkce()
	cases := []struct {
		cache string
		ttl   time.Duration
	}{
		{"", 300 * time.Second},
		{"max-age=10", 60 * time.Second},
		{"public, max-age=600", 600 * time.Second},
		{"max-age=999999", 3600 * time.Second},
		{"no-store", 0},
		{"no-cache, max-age=600", 0},
	}
	for _, tc := range cases {
		if got := cimdCacheTTL([]string{tc.cache}); got != tc.ttl {
			t.Errorf("cimdCacheTTL(%q) = %v, want %v", tc.cache, got, tc.ttl)
		}
		s := newCIMDServer(t)
		s.set(func(s *cimdServer) { s.cache = tc.cache })
		e, id := cimdEnv(t, s)
		get := func() {
			t.Helper()
			if res := e.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s")); res.StatusCode != http.StatusOK {
				t.Fatalf("%q: %d %s", tc.cache, res.StatusCode, body(t, res))
			}
		}
		get()
		get()
		if tc.ttl == 0 {
			if s.count() != 2 {
				t.Errorf("%q: fetches = %d, want 2 (not cached)", tc.cache, s.count())
			}
			continue
		}
		if s.count() != 1 {
			t.Errorf("%q: fetches = %d, want 1", tc.cache, s.count())
		}
		e.clock.Advance(tc.ttl - time.Second)
		get()
		if s.count() != 1 {
			t.Errorf("%q: refetched before %v", tc.cache, tc.ttl)
		}
		e.clock.Advance(2 * time.Second)
		get()
		if s.count() != 2 {
			t.Errorf("%q: not refetched after %v (fetches %d)", tc.cache, tc.ttl, s.count())
		}
	}
}

func TestMCPCIMDFailureThrottle(t *testing.T) {
	_, ch := pkce()
	s := newCIMDServer(t)
	s.set(func(s *cimdServer) { s.status = http.StatusServiceUnavailable })
	e, id := cimdEnv(t, s)
	try := func() int {
		res := e.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s"))
		_ = body(t, res)
		return res.StatusCode
	}
	if st := try(); st != http.StatusBadGateway || s.count() != 1 {
		t.Fatalf("first failure: %d, fetches %d", st, s.count())
	}
	// Within 10 s: the same 502 without a fetch, even once the document is back.
	s.set(func(s *cimdServer) { s.status = http.StatusOK })
	e.clock.Advance(9 * time.Second)
	if st := try(); st != http.StatusBadGateway || s.count() != 1 {
		t.Fatalf("throttled: %d, fetches %d", st, s.count())
	}
	// After 10 s the failure is forgotten (never cached).
	e.clock.Advance(2 * time.Second)
	if st := try(); st != http.StatusOK || s.count() != 2 {
		t.Fatalf("after the throttle: %d, fetches %d", st, s.count())
	}
}

func TestMCPCIMDSingleFlight(t *testing.T) {
	_, ch := pkce()
	s := newCIMDServer(t)
	release := make(chan struct{})
	s.set(func(s *cimdServer) { s.release = release })
	e, id := cimdEnv(t, s)
	var wg sync.WaitGroup
	var ok atomic.Int32
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := e.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s"))
			if res.StatusCode == http.StatusOK {
				ok.Add(1)
			}
		}()
	}
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	if ok.Load() != 20 || s.count() != 1 {
		t.Fatalf("ok = %d, fetches = %d; want 20 and 1", ok.Load(), s.count())
	}
}

// Requests that wait for a shared fetch that fails get the detail of its
// cause, as the request that started it does: a failed fetch, or a
// document that breaks the rules.
func TestMCPCIMDSingleFlightFailure(t *testing.T) {
	_, ch := pkce()
	for _, tc := range []struct {
		name        string
		setup       func(s *cimdServer)
		reason      string
		want, other string
	}{
		{"fetch", func(s *cimdServer) { s.status = http.StatusServiceUnavailable }, "HTTP 503", cimdFetchDetail, cimdRulesDetail},
		{"rules", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":["https://evil.example/cb"]}`
			}
		}, "redirect_uris", cimdRulesDetail, cimdFetchDetail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newCIMDServer(t)
			release := make(chan struct{})
			s.set(func(s *cimdServer) { tc.setup(s); s.release = release })
			e, id := cimdEnv(t, s)
			var wg sync.WaitGroup
			pages := make([]string, 10)
			codes := make([]int, len(pages))
			for i := range pages {
				wg.Add(1)
				go func() {
					defer wg.Done()
					res := e.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s"))
					codes[i], pages[i] = res.StatusCode, html.UnescapeString(body(t, res))
				}()
			}
			time.Sleep(200 * time.Millisecond)
			close(release)
			wg.Wait()
			if s.count() != 1 {
				t.Errorf("fetches = %d, want 1", s.count())
			}
			for i, b := range pages {
				if codes[i] != http.StatusBadGateway || !strings.Contains(b, tc.reason) || !strings.Contains(b, tc.want) || strings.Contains(b, tc.other) {
					t.Errorf("request %d: %d; want %q in\n%s", i, codes[i], tc.want, b)
				}
			}
		})
	}
}

func TestMCPCIMDGuardedByDefault(t *testing.T) {
	// Without the test client the SSRF guard refuses the loopback test server.
	s := newCIMDServer(t)
	_, ch := pkce()
	e := newMCPEnv(t, ModeNone, func(c *config.Config) { c.MCP.Clients = []string{s.URL + "/client"} })
	res := e.authorize(newBrowser(), authzParams(s.URL+"/client", claudeCB, ch, "s"))
	if b := body(t, res); res.StatusCode != http.StatusBadGateway || !strings.Contains(b, "address not allowed") {
		t.Fatalf("guarded fetch: %d\n%s", res.StatusCode, b)
	}
	if s.count() != 0 {
		t.Fatalf("the guarded client reached the server %d times", s.count())
	}
	// The token endpoint never fetches: an allowlisted client is known.
	if !e.m.knownClient(s.URL+"/client") || e.m.knownClient(s.URL+"/other") {
		t.Fatal("knownClient")
	}
	_ = url.Values{}
}

// A Content-Type is compared by its media type alone, as on the Worker: a
// malformed parameter (one without a value, one repeated) does not turn a
// JSON or form body away at any endpoint of the connector, nor a client ID
// metadata document (SPEC §8.4).
func TestMCPMediaTypeParameters(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	r := postJSON(OAuthRegisterPath, `{"redirect_uris":["`+loopbackCB+`"]}`)
	r.Header.Set("Content-Type", "Application/JSON; charset")
	res := e.do(r)
	m := jsonBody(t, res)
	id, _ := m["client_id"].(string)
	if res.StatusCode != http.StatusCreated || id == "" {
		t.Fatalf("register with a malformed parameter: %d %v", res.StatusCode, m)
	}

	f := newFlow(id, loopbackCB)
	b := newBrowser()
	form, _ := consentFields(t, e.authorize(b, authzParams(id, loopbackCB, f.challenge, f.state)))
	form.Set("decision", "allow")
	r = b.request(http.MethodPost, testIssuer+AuthorizePath, form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	code := redirectParams(t, e.do(r), http.StatusSeeOther, loopbackCB).Get("code")

	r = oauthPost(OAuthTokenPath, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {f.verifier},
		"client_id": {id}, "redirect_uri": {loopbackCB},
	})
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; a=1; a=2")
	res = e.do(r)
	m = jsonBody(t, res)
	rt, _ := m["refresh_token"].(string)
	if res.StatusCode != http.StatusOK || rt == "" {
		t.Fatalf("token exchange with a repeated parameter: %d %v", res.StatusCode, m)
	}
	r = oauthPost(OAuthRevokePath, url.Values{"token": {rt}, "client_id": {id}})
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset")
	if res := e.do(r); res.StatusCode != http.StatusOK {
		t.Errorf("revoke with a malformed parameter: %d %s", res.StatusCode, body(t, res))
	}
	if e.grantCount() != 0 {
		t.Error("the revocation did not revoke the grant")
	}

	s := newCIMDServer(t)
	s.set(func(s *cimdServer) { s.ctype = "application/json; charset" })
	ce, cid := cimdEnv(t, s)
	_, ch := pkce()
	consentFields(t, ce.authorize(newBrowser(), authzParams(cid, claudeCB, ch, "s")))
}

// The registration body is UTF-8 JSON, as every other body of the
// connector: invalid bytes are refused, not replaced (SPEC §8.4.3).
func TestMCPRegisterNotUTF8(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	res := e.do(postJSON(OAuthRegisterPath, `{"redirect_uris":["`+loopbackCB+`"],"client_name":"a`+"\xff"+`b"}`))
	if m := jsonBody(t, res); res.StatusCode != http.StatusBadRequest || m["error"] != "invalid_client_metadata" {
		t.Errorf("a body that is not UTF-8: %d %v; want 400 invalid_client_metadata", res.StatusCode, m)
	}
}

// opencode's client metadata URL: an app on the user's computer that names
// itself by its own document, which no deployment lists.
const opencodeCIMD = "https://opencode.ai/oauth/opencode/client.json"

// hermesCIMD is Hermes Agent's client metadata URL, 75 characters: longer
// than the 64 code points a registered or document name is cut to.
const hermesCIMD = "https://nousresearch.github.io/hermes-agent/docs/oauth/client-metadata.json"

// markupCIMD is a client metadata URL of the strict form with markup in its
// path, and markupCIMDHTML the same escaped as page text: wherever an
// unlisted app's name is shown, it must not fake a line such as "verified
// by".
const (
	markupCIMD     = `https://evil.example/x</strong><strong>Claude</strong>&"'<!--`
	markupCIMDHTML = `https://evil.example/x&lt;/strong&gt;&lt;strong&gt;Claude&lt;/strong&gt;&amp;&#34;&#39;&lt;!--`
)

// fetchCounter is an MCP HTTP client transport that counts and fails every
// request: a client metadata document fetch, of any URL, reaches it.
type fetchCounter struct{ n atomic.Int32 }

func (c *fetchCounter) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("no network in this test")
}

// unlistedEnv is a hub with the default mcp.clients whose metadata fetches
// all reach fc.
func unlistedEnv(t *testing.T, fc *fetchCounter, mutate func(*config.Config)) *mcpEnv {
	t.Helper()
	return newMCPEnv(t, ModeNone, mutate, func(o *Options) { o.MCPHTTPClient = &http.Client{Transport: fc} })
}

// TestMCPUnlistedCIMD: a client metadata URL outside mcp.clients connects
// from this computer (a loopback redirect URI), is never fetched, and is
// shown by its address as not verified (SPEC §8.4.3, §8.4.4).
func TestMCPUnlistedCIMD(t *testing.T) {
	fc := &fetchCounter{}
	e := unlistedEnv(t, fc, nil)
	ctx := context.Background()
	const cb = "http://127.0.0.1:53682/callback"
	_, ch := pkce()

	_, page := consentFields(t, e.authorize(newBrowser(), authzParams(opencodeCIMD, cb, ch, "s")))
	for _, want := range []string{
		`<p id="consent-app"><strong>` + opencodeCIMD + `</strong> (address provided by the app, not verified) wants to use Backyard as you.</p>`,
		"Backyard will send the approval to <strong>an app on this computer (http://127.0.0.1:53682)</strong>. Only continue if you just started the sign-in from an app on this computer.",
		"Only allow this if you started connecting " + opencodeCIMD + " to Backyard yourself just now.",
		"overflow-wrap:anywhere",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q\n%s", want, page)
		}
	}
	if strings.Contains(page, "verified by") {
		t.Error("an unlisted metadata document is shown as verified")
	}
	// The name is escaped (items 3 and 7, the hidden fields).
	if !unlistedClientURL(markupCIMD) {
		t.Fatalf("unlistedClientURL(%q) = false", markupCIMD)
	}
	_, page = consentFields(t, e.authorize(newBrowser(), authzParams(markupCIMD, cb, ch, "s")))
	for _, want := range []string{
		`<p id="consent-app"><strong>` + markupCIMDHTML + `</strong> (address provided by the app, not verified) wants to use Backyard as you.</p>`,
		"Only allow this if you started connecting " + markupCIMDHTML + " to Backyard yourself just now.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q\n%s", want, page)
		}
	}
	for _, unwanted := range []string{markupCIMD, "<strong>Claude</strong>", "<!--"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("consent page contains %q\n%s", unwanted, page)
		}
	}
	// There is no document, so no registered list: any loopback URI.
	for _, uri := range []string{"http://localhost/callback", "http://localhost:27890/callback", "http://[::1]:5/cb?x=1", "http://127.0.0.1:9"} {
		consentFields(t, e.authorize(newBrowser(), authzParams(opencodeCIMD, uri, ch, "s")))
	}

	// The whole flow: consent, code exchange, an MCP access token, refresh.
	f := newFlow(opencodeCIMD, cb)
	at, rt := e.tokens(newBrowser(), f)
	u, g, err := e.m.VerifyAccessToken(ctx, at)
	if err != nil || !u.Anonymous || g.ClientID != opencodeCIMD || g.ClientKind != "unlisted_metadata_document" ||
		g.ClientName != opencodeCIMD || g.ClientHost != "" || g.RedirectURI != cb {
		t.Fatalf("VerifyAccessToken = %+v %+v %v", u, g, err)
	}
	gs, err := e.m.Grants(ctx, u.ID)
	if err != nil || len(gs) != 1 || gs[0].ClientKind != clientKindUnlisted || gs[0].ClientHost != "" || gs[0].ClientName != opencodeCIMD {
		t.Fatalf("Grants = %+v %v", gs, err)
	}
	q := docstore.Query{Limit: 100}
	_ = q.Normalize(100)
	audit, _ := e.docs.Query(ctx, docstore.SystemSite, auditCollection, q)
	if len(audit.Docs) != 1 || audit.Docs[0].Data["client_kind"] != clientKindUnlisted || audit.Docs[0].Data["client_host"] != "" ||
		audit.Docs[0].Data["client_name"] != opencodeCIMD || audit.Docs[0].Data["redirect_host"] != "127.0.0.1:53682" {
		t.Errorf("audit = %+v", audit.Docs)
	}
	res := e.refresh(f, rt, nil)
	m := jsonBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("refresh: %d %v", res.StatusCode, m)
	}
	// Codes and refresh tokens stay bound to the client_id.
	other := newFlow("https://pi.dev/oauth/client.json", cb)
	oauthErr(t, e.refresh(other, m["refresh_token"].(string), nil), http.StatusBadRequest, "invalid_grant")
	code := e.code(newBrowser(), f)
	other.verifier = f.verifier
	oauthErr(t, e.exchange(other, code, nil), http.StatusBadRequest, "invalid_grant")
	if res := e.exchange(f, code, url.Values{"redirect_uri": {"http://127.0.0.1:53683/callback"}}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("exchange with another redirect_uri: %d", res.StatusCode)
	}
	// RFC 7009 revocation by the same client.
	if res := e.do(oauthPost(OAuthRevokePath, url.Values{"token": {m["refresh_token"].(string)}, "client_id": {opencodeCIMD}})); res.StatusCode != http.StatusOK {
		t.Errorf("revoke: %d", res.StatusCode)
	}
	if _, _, err := e.m.VerifyAccessToken(ctx, m["access_token"].(string)); !isDesc(err, descRevoked) {
		t.Errorf("after revocation: %v", err)
	}

	// Never fetched, never cached.
	if n := fc.n.Load(); n != 0 {
		t.Fatalf("an unlisted client metadata URL was fetched %d times", n)
	}
	e.m.cimdMu.Lock()
	cached := len(e.m.cimd)
	e.m.cimdMu.Unlock()
	if cached != 0 {
		t.Errorf("cimd cache entries = %d", cached)
	}
	// The counter does see fetches: an allowlisted document is fetched (and
	// fails with the fetch detail).
	res = e.authorize(newBrowser(), authzParams(claudeCIMD, claudeCB, ch, "s"))
	if b := html.UnescapeString(body(t, res)); res.StatusCode != http.StatusBadGateway || !strings.Contains(b, cimdFetchDetail) || fc.n.Load() != 1 {
		t.Errorf("allowlisted document: %d, fetches %d\n%s", res.StatusCode, fc.n.Load(), b)
	}

	// With mcp.clients: [] the rule still applies (nothing is fetched anyway).
	none := unlistedEnv(t, fc, func(c *config.Config) { c.MCP.Clients = []string{} })
	consentFields(t, none.authorize(newBrowser(), authzParams(opencodeCIMD, cb, ch, "s")))
	if fc.n.Load() != 1 {
		t.Error("mcp.clients: [] fetched an unlisted document")
	}
}

// TestMCPUnlistedCIMDNotLocal: an unlisted metadata document whose sign-in
// does not return to this computer is not allowed, also at the consent POST.
func TestMCPUnlistedCIMDNotLocal(t *testing.T) {
	fc := &fetchCounter{}
	e := unlistedEnv(t, fc, nil)
	_, ch := pkce()
	for _, uri := range []string{claudeCB, "https://chatgpt.com/connector_platform_oauth_redirect", "https://evil.example/cb",
		"http://opencode.ai/callback", "https://localhost/callback", "http://localhost/cb#x", "http://localhost:0/cb", "http://u@localhost/cb", ""} {
		params := authzParams(opencodeCIMD, uri, ch, "s")
		if uri == "" {
			params.Del("redirect_uri")
		}
		res := e.authorize(newBrowser(), params)
		b := html.UnescapeString(body(t, res))
		if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(b, "<h1>App not allowed</h1>") ||
			!strings.Contains(b, "The app "+opencodeCIMD+" may connect to this Backyard only from this computer (a sign-in that returns to localhost).") ||
			!strings.Contains(b, "An administrator can allow it from anywhere by adding its client metadata URL to mcp.clients and its redirect address to mcp.redirect_uris.") {
			t.Errorf("redirect %q: %d %q\n%s", uri, res.StatusCode, res.Header.Get("Location"), b)
		}
	}
	// A consent form whose redirect_uri was changed to Claude's callback.
	br := newBrowser()
	fields, _ := consentFields(t, e.authorize(br, authzParams(opencodeCIMD, "http://localhost:4000/cb", ch, "s")))
	fields.Set("redirect_uri", claudeCB)
	res := e.consentPost(br, fields, "allow")
	if b := body(t, res); res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(b, "App not allowed") {
		t.Errorf("consent POST: %d %q\n%s", res.StatusCode, res.Header.Get("Location"), b)
	}
	if e.grantCount() != 0 || fc.n.Load() != 0 {
		t.Errorf("grants %d, fetches %d", e.grantCount(), fc.n.Load())
	}
	// The address in the message is escaped.
	res = e.authorize(newBrowser(), authzParams(markupCIMD, claudeCB, ch, "s"))
	b := body(t, res)
	if res.StatusCode != http.StatusBadRequest || !strings.Contains(b, "<h1>App not allowed</h1>") ||
		!strings.Contains(b, "<p>The app "+markupCIMDHTML+" may connect to this Backyard only from this computer (a sign-in that returns to localhost).</p>") {
		t.Errorf("markup in the client_id: %d\n%s", res.StatusCode, b)
	}
	for _, unwanted := range []string{markupCIMD, "<strong>Claude</strong>", "<!--"} {
		if strings.Contains(b, unwanted) {
			t.Errorf("App not allowed page contains %q\n%s", unwanted, b)
		}
	}
}

// TestMCPUnlistedCIMDFullName: an unlisted app's name is its client_id
// exactly as sent, never cut to 64 code points like a registered or
// document name: on the consent page, in the grant and in the audit (SPEC
// §8.4.3).
func TestMCPUnlistedCIMDFullName(t *testing.T) {
	if len(hermesCIMD) <= maxClientNameRunes {
		t.Fatalf("%q is not longer than %d characters", hermesCIMD, maxClientNameRunes)
	}
	fc := &fetchCounter{}
	e := unlistedEnv(t, fc, nil)
	ctx := context.Background()
	const cb = "http://127.0.0.1:27890/callback"
	_, ch := pkce()
	_, page := consentFields(t, e.authorize(newBrowser(), authzParams(hermesCIMD, cb, ch, "s")))
	for _, want := range []string{
		`<p id="consent-app"><strong>` + hermesCIMD + `</strong> (address provided by the app, not verified) wants to use Backyard as you.</p>`,
		"Only allow this if you started connecting " + hermesCIMD + " to Backyard yourself just now.",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q\n%s", want, page)
		}
	}
	at, _ := e.tokens(newBrowser(), newFlow(hermesCIMD, cb))
	u, g, err := e.m.VerifyAccessToken(ctx, at)
	if err != nil || g.ClientName != hermesCIMD || g.ClientKind != clientKindUnlisted {
		t.Fatalf("VerifyAccessToken = %+v %v", g, err)
	}
	gs, err := e.m.Grants(ctx, u.ID)
	if err != nil || len(gs) != 1 || gs[0].ClientName != hermesCIMD {
		t.Fatalf("Grants = %+v %v", gs, err)
	}
	q := docstore.Query{Limit: 100}
	_ = q.Normalize(100)
	audit, _ := e.docs.Query(ctx, docstore.SystemSite, auditCollection, q)
	if len(audit.Docs) != 1 || audit.Docs[0].Data["client_name"] != hermesCIMD {
		t.Errorf("audit = %+v", audit.Docs)
	}
	if n := fc.n.Load(); n != 0 {
		t.Errorf("fetches = %d", n)
	}
}

// TestMCPUnlistedCIMDInvalidURL: an https client_id that is neither
// allowlisted nor a client metadata URL (rule 1 of SPEC §12) is unknown, at
// the authorize and the token endpoint.
func TestMCPUnlistedCIMDInvalidURL(t *testing.T) {
	fc := &fetchCounter{}
	e := unlistedEnv(t, fc, nil)
	_, ch := pkce()
	long := "https://opencode.ai/" + strings.Repeat("a", 512-len("https://opencode.ai/"))
	for _, id := range []string{
		"http://opencode.ai/oauth/client.json", "HTTPS://opencode.ai/oauth/client.json",
		"https://opencode.ai/oauth/client.json?x=1", "https://opencode.ai/oauth/client.json?",
		"https://opencode.ai/oauth/client.json#f", "https://opencode.ai/", "https://opencode.ai",
		"https://203.0.113.5/client.json", "https://[2001:db8::1]/client.json", "https://127.0.0.1/client.json",
		"https://opencode.ai/a/../client.json", "https://opencode.ai/./client.json", "https://opencode.ai/a/%2e%2e/client.json",
		"https://u@opencode.ai/client.json", "https://opencode.ai/a b", "https://opencode.ai/a\\b", "https://opencode.ai/%zz",
		"https:///client.json", long + "a",
		// The strict form of an address shown as the app's name.
		"https://opencode.ai:443/client.json", "https://opencode.ai./client.json", "https://open_code.ai/client.json",
		"https://open..code.ai/client.json", "https://.opencode.ai/client.json",
		"https://1.2.3.999/client.json", "https://op\u202eencode.ai/client.json", "https://opencode.ai/\u202eclient.json",
		"https://opencode.ai/a\u00a0b", "https://opencode.ai/a\u0085b", "https://exämple.com/client.json",
		"https://opencode.ai/a%2Fb", "https://opencode.ai/a%2fb", "https://opencode.ai/a%2", "https://opencode.ai/a%",
		"https://xn--80ak6aa92e.com/client.json", "https://apps.XN--p1ai/client.json",
		"https://example.123/client.json", "https://example.0x1f/client.json", "https://foo.0X/a", "https://1.2.3.0x4/a",
	} {
		res := e.authorize(newBrowser(), authzParams(id, "http://localhost/callback", ch, "s"))
		if b := body(t, res); res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(b, "Unknown app") {
			t.Errorf("client_id %q: %d\n%s", id, res.StatusCode, b)
		}
		if e.m.knownClient(id) {
			t.Errorf("knownClient(%q)", id)
		}
		r := oauthPost(OAuthTokenPath, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"byrt_x"}, "client_id": {id}})
		oauthErr(t, e.do(r), http.StatusUnauthorized, "invalid_client")
	}
	// 512 bytes is a client metadata URL, shown whole; escapes stay as sent.
	if _, page := consentFields(t, e.authorize(newBrowser(), authzParams(long, "http://localhost/callback", ch, "s"))); !strings.Contains(page,
		"<strong>"+long+"</strong> (address provided by the app, not verified)") {
		t.Errorf("the 512-byte address is not shown whole:\n%s", page)
	}
	for _, id := range []string{"https://example.com/%E2%80%AEclient.json", "https://Example.COM/a@b/~c!$&'()*+,;=:x.json",
		"https://my-app.example.co.uk/oauth/client", "https://x/y", "https://1a.example/2", "https://foo.0xg/a", "https://0x1f.example/a"} {
		if !unlistedClientURL(id) {
			t.Errorf("unlistedClientURL(%q) = false", id)
		}
	}
	if !e.m.knownClient(long) || !e.m.knownClient(opencodeCIMD) || !e.m.knownClient(claudeCIMD) {
		t.Error("knownClient refuses a client metadata URL")
	}
	if fc.n.Load() != 0 {
		t.Errorf("fetches = %d", fc.n.Load())
	}
}

// TestMCPDelistedCIMD: once its URL leaves mcp.clients, a metadata document
// client is an unlisted one: it keeps only the connections that return to
// this computer, and is no longer fetched. A listed URL outside the strict
// form of an unlisted one (here: with a port) is then no client at all:
// every connection ends, loopback ones included.
func TestMCPDelistedCIMD(t *testing.T) {
	s := newCIMDServer(t)
	const id = "https://example.com/client"          // served by s (namedClient)
	const portID = "https://example.com:8443/client" // a valid mcp.clients entry, not of the strict form
	if !config.ValidMCPClientURL(portID) || unlistedClientURL(portID) {
		t.Fatalf("%s: ValidMCPClientURL %v, unlistedClientURL %v", portID, config.ValidMCPClientURL(portID), unlistedClientURL(portID))
	}
	e := newMCPEnv(t, ModeNone, func(c *config.Config) { c.MCP.Clients = []string{id, portID} },
		func(o *Options) { o.MCPHTTPClient = s.namedClient() })
	hosted := newFlow(id, claudeCB)
	_, hostedRT := e.tokens(newBrowser(), hosted)
	pending := newFlow(id, claudeCB)
	pendingCode := e.code(newBrowser(), pending)
	local := newFlow(id, "http://localhost:4444/callback")
	_, localRT := e.tokens(newBrowser(), local)
	portLocal := newFlow(portID, "http://localhost:4444/callback")
	portAT, portRT := e.tokens(newBrowser(), portLocal)
	if s.count() != 2 {
		t.Fatalf("fetches = %d", s.count())
	}

	// The administrator removes it from mcp.clients (same secret and store).
	e2 := newMCPEnv(t, ModeNone, nil, func(o *Options) { o.Docs = e.docs; o.MCPHTTPClient = s.namedClient() })
	if m := oauthErr(t, e2.refresh(hosted, hostedRT, nil), http.StatusBadRequest, "invalid_grant"); m["error_description"] != errRedirectGone {
		t.Errorf("hosted refresh: %v", m)
	}
	oauthErr(t, e2.exchange(pending, pendingCode, nil), http.StatusBadRequest, "invalid_grant")
	if res := e2.refresh(local, localRT, nil); res.StatusCode != http.StatusOK {
		t.Errorf("local refresh: %d %s", res.StatusCode, body(t, res))
	}
	if m := oauthErr(t, e2.refresh(portLocal, portRT, nil), http.StatusUnauthorized, "invalid_client"); m["error_description"] != "unknown client" {
		t.Errorf("local refresh of %s: %v", portID, m)
	}
	oauthErr(t, e2.do(oauthPost(OAuthRevokePath, url.Values{"token": {portAT}, "client_id": {portID}})), http.StatusUnauthorized, "invalid_client")
	_, pch := pkce()
	if res := e2.authorize(newBrowser(), authzParams(portID, "http://localhost:4444/callback", pch, "s")); res.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body(t, res), "Unknown app") {
		t.Errorf("authorize %s from this computer: %d", portID, res.StatusCode)
	}
	// Signing in again: not with Claude's callback; from this computer as
	// an unlisted app, without a fetch.
	_, ch := pkce()
	if res := e2.authorize(newBrowser(), authzParams(id, claudeCB, ch, "s")); res.StatusCode != http.StatusBadRequest ||
		!strings.Contains(body(t, res), "App not allowed") {
		t.Errorf("authorize with Claude's callback: %d", res.StatusCode)
	}
	_, page := consentFields(t, e2.authorize(newBrowser(), authzParams(id, "http://localhost:4444/callback", ch, "s")))
	if !strings.Contains(page, "<strong>"+id+"</strong> (address provided by the app, not verified)") {
		t.Errorf("consent page:\n%s", page)
	}
	if s.count() != 2 {
		t.Errorf("fetches = %d after delisting", s.count())
	}
}

// namedClient is an HTTP client that reaches s for every host name (its
// certificate is valid for example.com).
func (s *cimdServer) namedClient() *http.Client {
	tlsConf := s.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: tlsConf,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, s.Listener.Addr().String())
		},
	}}
}
