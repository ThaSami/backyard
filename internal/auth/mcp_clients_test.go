package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
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

	// Not allowlisted: never fetched, never redirected.
	before := s.count()
	for _, other := range []string{s.URL + "/other", "https://example.com/x", id + "/", id + "?x=1"} {
		res := e.authorize(newBrowser(), authzParams(other, claudeCB, ch, "s"))
		if res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" || !strings.Contains(body(t, res), "App not allowed") {
			t.Errorf("non-allowlisted %s: %d", other, res.StatusCode)
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
	cases := []struct {
		name   string
		setup  func(s *cimdServer)
		path   string
		reason string
	}{
		{"client_id mismatch", func(s *cimdServer) {
			s.body = func(string) string { return `{"client_id":"https://claude.ai/x","redirect_uris":["` + claudeCB + `"]}` }
		}, "/client", "client_id"},
		{"redirect", nil, "/redirect", "redirect"},
		{"non-200", func(s *cimdServer) { s.status = http.StatusNotFound }, "/client", "HTTP 404"},
		{"wrong content type", func(s *cimdServer) { s.ctype = "text/html" }, "/client", "not JSON"},
		{"too large", func(s *cimdServer) {
			s.body = func(id string) string {
				doc := `{"client_id":"` + id + `","redirect_uris":["` + claudeCB + `"],"pad":"`
				return doc + strings.Repeat("x", 5121-len(doc)-2) + `"}`
			}
		}, "/client", "5120 bytes"},
		{"private_key_jwt", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_method":"private_key_jwt"}`
			}
		}, "/client", "client authentication"},
		{"methods without none", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_method":"none",` +
					`"token_endpoint_auth_methods_supported":["private_key_jwt"]}`
			}
		}, "/client", "client authentication"},
		{"methods not strings", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_methods_supported":"none"}`
			}
		}, "/client", "token_endpoint_auth_methods_supported"},
		{"methods null", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":["` + claudeCB + `"],"token_endpoint_auth_methods_supported":null}`
			}
		}, "/client", "token_endpoint_auth_methods_supported"},
		{"client secret", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":["` + claudeCB + `"],"client_secret":"s3cret"}`
			}
		}, "/client", "client secret"},
		{"no allowed redirect", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":["https://evil.example/cb"]}`
			}
		}, "/client", "redirect_uris"},
		{"eleven redirects", func(s *cimdServer) {
			s.body = func(id string) string {
				return `{"client_id":"` + id + `","redirect_uris":[` + strings.Repeat(`"`+claudeCB+`",`, 10) + `"` + claudeCB + `"]}`
			}
		}, "/client", "redirect_uris"},
		{"not an object", func(s *cimdServer) { s.body = func(string) string { return `[1]` } }, "/client", "JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newCIMDServer(t)
			if tc.setup != nil {
				s.set(tc.setup)
			}
			e, _ := cimdEnv(t, s)
			res := e.authorize(newBrowser(), authzParams(s.URL+tc.path, claudeCB, ch, "s"))
			b := body(t, res)
			if res.StatusCode != http.StatusBadGateway || res.Header.Get("Location") != "" || !strings.Contains(b, "Could not verify the app") ||
				!strings.Contains(b, tc.reason) || !strings.Contains(b, "Register automatically") || !strings.Contains(b, s.URL+tc.path) {
				t.Errorf("%d %q:\n%s", res.StatusCode, res.Header.Get("Location"), b)
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
