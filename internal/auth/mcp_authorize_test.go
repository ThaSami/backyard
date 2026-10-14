package auth

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

// TestMCPAuthorizeOrder: every failure before the redirect URI is validated
// renders a page and never redirects; every later failure redirects to the
// client with error, state and iss.
func TestMCPAuthorizeOrder(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("App", loopbackCB, "http://localhost/cb?app=1")
	_, ch := pkce()
	valid := func() url.Values { return authzParams(id, loopbackCB, ch, "st") }
	with := func(mut func(v url.Values)) url.Values {
		v := valid()
		mut(v)
		return v
	}
	// Registered while claude.ai was allowed; the policy is re-checked.
	claudeID := e.register("App", claudeCB)
	noClaude := newMCPEnv(t, ModeNone, func(c *config.Config) { c.MCP.RedirectURIs = []string{} })
	noClaude.m.clientKey = e.m.clientKey

	pages := []struct {
		name   string
		env    *mcpEnv
		raw    string // raw query (instead of params)
		params url.Values
		status int
		title  string
	}{
		{"uri too long", e, "", with(func(v url.Values) { v.Set("prompt", strings.Repeat("x", 2048)) }), 400, "Invalid authorization request"},
		{"repeated client_id", e, "", with(func(v url.Values) { v.Add("client_id", id) }), 400, "Invalid authorization request"},
		{"repeated redirect_uri", e, "", with(func(v url.Values) { v.Add("redirect_uri", loopbackCB) }), 400, "Invalid authorization request"},
		{"repeated state", e, "", with(func(v url.Values) { v.Add("state", "x") }), 400, "Invalid authorization request"},
		{"repeated scope", e, "", with(func(v url.Values) { v.Add("scope", "mcp") }), 400, "Invalid authorization request"},
		{"repeated response_mode", e, "", with(func(v url.Values) { v.Add("response_mode", "query"); v.Add("response_mode", "query") }), 400, "Invalid authorization request"},
		{"bad query", e, "client_id=%zz", nil, 400, "Invalid authorization request"},
		{"missing client_id", e, "", with(func(v url.Values) { v.Del("client_id") }), 400, "Unknown app"},
		{"unknown client_id", e, "", with(func(v url.Values) { v.Set("client_id", "bydc_nope") }), 400, "Unknown app"},
		{"non-allowlisted cimd", e, "", with(func(v url.Values) { v.Set("client_id", "https://evil.example/client") }), 400, "App not allowed"},
		{"missing redirect_uri", e, "", with(func(v url.Values) { v.Del("redirect_uri") }), 400, "Redirect address not allowed"},
		{"unregistered redirect", e, "", with(func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:9/other") }), 400, "Redirect address not allowed"},
		{"other loopback host", e, "", with(func(v url.Values) { v.Set("redirect_uri", "http://localhost:9/callback") }), 400, "Redirect address not allowed"},
		{"fragment", e, "", with(func(v url.Values) { v.Set("redirect_uri", loopbackCB+"#x") }), 400, "Redirect address not allowed"},
		{"policy re-checked", noClaude, "", authzParams(claudeID, claudeCB, ch, "st"), 400, "Redirect address not allowed"},
		{"bad redirect wins over bad response_type", e, "", with(func(v url.Values) {
			v.Set("redirect_uri", "https://evil.example/cb")
			v.Set("response_type", "token")
		}), 400, "Redirect address not allowed"},
	}
	for _, tc := range pages {
		raw := tc.raw
		if tc.params != nil {
			raw = tc.params.Encode()
		}
		for _, method := range []string{"GET"} {
			r := newBrowser().request(method, testIssuer+AuthorizePath+"?"+raw, nil)
			res := tc.env.do(r)
			b := body(t, res)
			if res.StatusCode != tc.status || res.Header.Get("Location") != "" || !strings.Contains(b, tc.title) ||
				!strings.HasPrefix(res.Header.Get("Content-Type"), "text/html") {
				t.Errorf("%s: %d %q, want %d page %q\n%s", tc.name, res.StatusCode, res.Header.Get("Location"), tc.status, tc.title, b)
			}
		}
	}

	redirects := []struct {
		name, code string
		params     url.Values
		noState    bool
	}{
		{"response_type token", "unsupported_response_type", with(func(v url.Values) { v.Set("response_type", "token") }), false},
		{"response_type missing", "unsupported_response_type", with(func(v url.Values) { v.Del("response_type") }), false},
		{"request object", "request_not_supported", with(func(v url.Values) { v.Set("request", "eyJ") }), false},
		{"request_uri", "request_uri_not_supported", with(func(v url.Values) { v.Set("request_uri", "urn:x") }), false},
		{"response_mode fragment", "invalid_request", with(func(v url.Values) { v.Set("response_mode", "fragment") }), false},
		{"challenge method missing", "invalid_request", with(func(v url.Values) { v.Del("code_challenge_method") }), false},
		{"challenge method plain", "invalid_request", with(func(v url.Values) { v.Set("code_challenge_method", "plain") }), false},
		{"challenge missing", "invalid_request", with(func(v url.Values) { v.Del("code_challenge") }), false},
		{"challenge short", "invalid_request", with(func(v url.Values) { v.Set("code_challenge", ch[:42]) }), false},
		{"challenge bad chars", "invalid_request", with(func(v url.Values) { v.Set("code_challenge", ch[:42]+"=") }), false},
		{"foreign resource", "invalid_target", with(func(v url.Values) { v.Set("resource", "https://other.example/__backyard/mcp") }), false},
		{"empty resource", "invalid_target", with(func(v url.Values) { v.Set("resource", "") }), false},
		{"state too long", "invalid_request", with(func(v url.Values) { v.Set("state", strings.Repeat("s", 513)) }), true},
	}
	for _, tc := range redirects {
		res := e.authorize(newBrowser(), tc.params)
		q := redirectParams(t, res, http.StatusFound, loopbackCB+"?")
		if q.Get("error") != tc.code || q.Get("iss") != testIssuer || q.Get("error_description") == "" {
			t.Errorf("%s: query %v, want error %s", tc.name, q, tc.code)
		}
		if tc.noState != (q.Get("state") == "") || (!tc.noState && q.Get("state") != "st") {
			t.Errorf("%s: state = %q", tc.name, q.Get("state"))
		}
		if res.Header.Get("Cache-Control") == "" || res.Header.Get("Referrer-Policy") != "no-referrer" ||
			res.Header.Get("Set-Cookie") != "" {
			t.Errorf("%s: headers %v", tc.name, res.Header)
		}
	}
	// A request without state redirects without one; a registered URI with
	// a query gets "&".
	v := with(func(v url.Values) { v.Del("state"); v.Set("response_type", "token") })
	if loc := e.authorize(newBrowser(), v).Header.Get("Location"); strings.Contains(loc, "state=") {
		t.Errorf("absent state echoed: %s", loc)
	}
	v = with(func(v url.Values) {
		v.Set("redirect_uri", "http://localhost:7/cb?app=1")
		v.Set("response_type", "token")
	})
	if loc := e.authorize(newBrowser(), v).Header.Get("Location"); !strings.HasPrefix(loc, "http://localhost:7/cb?app=1&error=unsupported_response_type&") {
		t.Errorf("query separator: %s", loc)
	}
	// A 2048-byte request is still served.
	pad := 2048 - len(AuthorizePath+"?"+with(func(v url.Values) { v.Set("prompt", "") }).Encode())
	ok := with(func(v url.Values) { v.Set("prompt", strings.Repeat("x", pad)) })
	if res := e.authorize(newBrowser(), ok); res.StatusCode != http.StatusOK {
		t.Errorf("2048-byte request: %d %s", res.StatusCode, body(t, res))
	}
}

func TestMCPAuthorizeScopesAndExtras(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("App", loopbackCB)
	_, ch := pkce()
	for scope, want := range map[string]string{
		"": "mcp", "mcp": "mcp", "offline_access": "mcp offline_access", "mcp offline_access": "mcp offline_access",
		"admin openid": "mcp", "offline_access admin": "mcp offline_access",
	} {
		v := authzParams(id, loopbackCB, ch, "st")
		if scope == "" {
			v.Del("scope")
		} else {
			v.Set("scope", scope)
		}
		v.Set("prompt", "none")
		v.Set("nonce", "n")
		v.Set("login_hint", "x")
		v.Set("response_mode", "query")
		v.Del("resource")
		fields, _ := consentFields(t, e.authorize(newBrowser(), v))
		if fields.Get("scope") != want || fields.Get("resource") != testResource {
			t.Errorf("scope %q: granted %q resource %q", scope, fields.Get("scope"), fields.Get("resource"))
		}
	}
}

func TestMCPAuthorizeRouting(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("App", loopbackCB)
	_, ch := pkce()
	q := "?" + authzParams(id, loopbackCB, ch, "st").Encode()
	for _, m := range []string{"PUT", "DELETE", "HEAD", "OPTIONS"} {
		res := e.do(req(m, testIssuer+AuthorizePath+q))
		if res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "GET, POST" {
			t.Errorf("%s = %d %q", m, res.StatusCode, res.Header.Get("Allow"))
		}
	}
	res := e.do(req("GET", "https://other.backyard.test"+AuthorizePath+q))
	if b := body(t, res); res.StatusCode != http.StatusNotFound || !strings.Contains(b, "Use "+testResource) || res.Header.Get("Location") != "" {
		t.Errorf("non-canonical host = %d %s", res.StatusCode, b)
	}
	// Forwarded host from a trusted proxy counts; from anyone else it does not.
	e = newMCPEnv(t, ModeNone, func(c *config.Config) { c.Server.TrustedProxies = []string{"10.0.0.0/8"} })
	id = e.register("App", loopbackCB)
	q = "?" + authzParams(id, loopbackCB, ch, "st").Encode()
	r := req("GET", "http://10.1.2.3:8080"+AuthorizePath+q)
	r.RemoteAddr = "10.0.0.7:1234"
	r.Header.Set("X-Forwarded-Host", "backyard.test")
	if res := e.do(r); res.StatusCode != http.StatusOK {
		t.Errorf("trusted X-Forwarded-Host: %d", res.StatusCode)
	}
	r = req("GET", "https://evil.example"+AuthorizePath+q)
	r.Header.Set("X-Forwarded-Host", "backyard.test")
	if res := e.do(r); res.StatusCode != http.StatusNotFound {
		t.Errorf("untrusted X-Forwarded-Host: %d", res.StatusCode)
	}
}

func TestMCPAuthorizeModeNone(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("Claude Code", "http://localhost/callback")
	_, ch := pkce()
	b := newBrowser()
	res := e.authorize(b, authzParams(id, "http://localhost:53682/callback", ch, "st"))
	guest := cookieNamed(res, AnonCookie)
	if guest == nil {
		t.Fatal("no guest cookie minted")
	}
	fields, page := consentFields(t, res)
	gid, _ := newAnonCodec(testSecret).parse(guest.Value)
	for _, want := range []string{
		"<strong>Guest " + gid[:4] + "</strong>. This Backyard has no sign-in; the app will act as this browser's guest.",
		`<strong>"Claude Code"</strong> (name provided by the app, not verified)`,
		"Backyard will send the approval to <strong>an app on this computer (http://localhost:53682)</strong>. Only continue if you just started the sign-in from an app on this computer.",
		"The app can see the sites on backyard.test and read their files, and publish or change sites as you.",
		"Only allow this if you started connecting Claude Code to Backyard yourself just now.",
		"This connection lasts until " + e.clock.Now().Add(720*time.Hour).UTC().Format("2006-01-02") + " unless you disconnect it",
		`<button type="submit" name="decision" value="allow" id="allow">Allow</button>`,
		`<button type="submit" name="decision" value="deny" id="deny">Cancel</button>`,
		"animation: by-arm 0s linear 0.8s forwards", "@keyframes by-arm { to { visibility: visible } }",
		"<title>Connect an app to Backyard · Backyard</title>", "<h1>Connect an app to Backyard</h1>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	for _, unwanted := range []string{"Sign out", "autofocus", "<script"} {
		if strings.Contains(page, unwanted) {
			t.Errorf("consent page contains %q", unwanted)
		}
	}
	want := url.Values{
		"response_type": {"code"}, "client_id": {id}, "redirect_uri": {"http://localhost:53682/callback"}, "state": {"st"},
		"code_challenge": {ch}, "code_challenge_method": {"S256"}, "scope": {"mcp offline_access"}, "resource": {testResource},
		"csrf": fields["csrf"],
	}
	if fields.Encode() != want.Encode() || len(fields.Get("csrf")) > 128 {
		t.Errorf("hidden fields = %v\nwant %v", fields, want)
	}
	// Without state the hidden field is omitted.
	f2, _ := consentFields(t, e.authorize(b, authzParams(id, "http://localhost:1/callback", ch, "")))
	if _, ok := f2["state"]; ok {
		t.Error("absent state rendered")
	}
	// The same guest is used on the next request (cookie kept).
	res = e.authorize(b, authzParams(id, "http://localhost:53682/callback", ch, "st"))
	if cookieNamed(res, AnonCookie) != nil {
		t.Error("a second guest cookie was minted")
	}
	// Consent is shown whatever Sec-Fetch-Site says.
	r := b.request("GET", testIssuer+AuthorizePath+"?"+authzParams(id, "http://localhost:53682/callback", ch, "st").Encode(), nil)
	r.Header.Set("Sec-Fetch-Site", "none")
	consentFields(t, e.do(r))
	// Guest cookies of two guests: 401, no redirect.
	c1 := e.a.newCookie(AnonCookie, keys.Sign(e.a.anon.key, []byte("0123456789abcdef")), "", "/", time.Hour)
	c2 := e.a.newCookie(AnonCookie, keys.Sign(e.a.anon.key, []byte("fedcba9876543210")), "", "/", time.Hour)
	conflict := newBrowser()
	conflict.extra = []*http.Cookie{c1, c2}
	res = e.authorize(conflict, authzParams(id, "http://localhost:53682/callback", ch, "st"))
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("Location") != "" || !strings.Contains(body(t, res), "Conflicting sign-in cookies") {
		t.Errorf("cookie conflict = %d", res.StatusCode)
	}
}

func TestMCPConsentHeaders(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	_, ch := pkce()
	cases := map[string]string{
		claudeCB:                          "https://claude.ai",
		"http://localhost:53682/callback": "http://localhost:53682",
		loopbackCB:                        "http://127.0.0.1:9",
		"http://[::1]:5/cb":               "http:",
	}
	for redirect, origin := range cases {
		id := e.register("App", redirect)
		res := e.authorize(newBrowser(), authzParams(id, redirect, ch, "st"))
		_, page := consentFields(t, res)
		h := res.Header
		want := "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self' " + origin
		if h.Get("Content-Security-Policy") != want {
			t.Errorf("%s: CSP = %q\nwant %q", redirect, h.Get("Content-Security-Policy"), want)
		}
		if h.Get("X-Frame-Options") != "DENY" || !strings.Contains(h.Get("Cache-Control"), "no-store") ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: headers %v", redirect, h)
		}
		if redirect == claudeCB && !strings.Contains(page, "Backyard will send the approval to <strong>https://claude.ai</strong>.") {
			t.Errorf("https return line:\n%s", page)
		}
	}
}

func TestMCPCanConnectApps(t *testing.T) {
	guest := &identity.User{ID: "anon:0123456789abcdef", Anonymous: true}
	static := &identity.User{ID: "token:ci"}
	person := &identity.User{ID: "alice@acme.com", Email: "alice@acme.com"}
	for _, tc := range []struct {
		mode string
		u    *identity.User
		want bool
	}{
		{ModeNone, guest, true}, {ModeOIDC, guest, false}, {ModeHeader, guest, false},
		{ModeNone, static, false}, {ModeOIDC, static, false},
		{ModeOIDC, person, true}, {ModeIAP, person, true},
	} {
		if got := canConnectApps(tc.mode, tc.u); got != tc.want {
			t.Errorf("canConnectApps(%s, %s) = %v", tc.mode, tc.u.ID, got)
		}
	}
}

func TestMCPAuthorizeProxyModes(t *testing.T) {
	_, ch := pkce()
	envs := map[string]*mcpEnv{
		"header": newMCPEnv(t, ModeHeader, func(c *config.Config) {
			c.Server.TrustedProxies = []string{"10.0.0.0/8"}
			c.Auth.Header.EmailHeader = "X-Forwarded-Email"
			c.Auth.AllowedDomains = []string{"acme.com"}
		}),
		"iap": newMCPEnv(t, ModeIAP, func(c *config.Config) { c.Auth.IAP.Audience = iapAud }),
		"alb": newMCPEnv(t, ModeALB, func(c *config.Config) { c.Auth.ALB.Region = "us-east-1" }),
	}
	for mode, e := range envs {
		id := e.register("App", loopbackCB)
		res := e.authorize(newBrowser(), authzParams(id, loopbackCB, ch, "st"))
		b := body(t, res)
		if res.StatusCode != http.StatusUnauthorized || res.Header.Get("Location") != "" ||
			!strings.Contains(b, "Sign-in required") || !strings.Contains(b, "/__backyard/auth/authorize must stay behind it") {
			t.Errorf("%s without identity: %d %q\n%s", mode, res.StatusCode, res.Header.Get("Location"), b)
		}
	}
	e := envs["header"]
	id := e.register("App", loopbackCB)
	trusted := func(email string) *browser {
		b := newBrowser()
		b.remote = "10.0.0.7:1234"
		b.headers.Set("X-Forwarded-Email", email)
		return b
	}
	res := e.authorize(trusted("alice@acme.com"), authzParams(id, loopbackCB, ch, "st"))
	if _, page := consentFields(t, res); !strings.Contains(page, "You are signed in as <strong>alice@acme.com</strong>.") ||
		strings.Contains(page, "Sign out") {
		t.Errorf("header consent:\n%s", page)
	}
	res = e.authorize(trusted("mallory@evil.test"), authzParams(id, loopbackCB, ch, "st"))
	if b := body(t, res); res.StatusCode != http.StatusForbidden || res.Header.Get("Location") != "" || !strings.Contains(b, "Access denied") {
		t.Errorf("forbidden: %d\n%s", res.StatusCode, b)
	}
	// A bearer token is never a browser identity here.
	tok, _, _ := e.a.tokens.Issue(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com"}, time.Hour)
	b := newBrowser()
	b.headers.Set("Authorization", "Bearer "+tok)
	if res := e.authorize(b, authzParams(id, loopbackCB, ch, "st")); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("bearer at authorize: %d", res.StatusCode)
	}
}

func TestMCPAuthorizeOIDC(t *testing.T) {
	e := newMCPEnv(t, ModeOIDC, nil)
	id := e.register("App", loopbackCB)
	_, ch := pkce()
	params := authzParams(id, loopbackCB, ch, "st")
	// Pad the request to the 2048-byte maximum.
	params.Set("prompt", "")
	params.Set("prompt", strings.Repeat("p", 2048-len(AuthorizePath+"?"+params.Encode())))
	uri := AuthorizePath + "?" + params.Encode()
	if len(uri) != 2048 {
		t.Fatalf("uri length %d", len(uri))
	}
	res := e.authorize(newBrowser(), params)
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusFound || loc != e.a.LoginURL(testIssuer+uri) {
		t.Fatalf("not signed in: %d %q", res.StatusCode, loc)
	}
	// The return URL survives the login's rd sanitizing, and the login
	// state cookie stays a browser-storable size.
	lu, _ := url.Parse(loc)
	if got := e.a.sanitizeRedirect(lu.Query().Get("rd")); got != testIssuer+uri {
		t.Fatalf("rd sanitized to %q", got)
	}
	ls := serve(e.a.Handler(), req("GET", loc))
	sc := cookieNamed(ls, e.a.oidc.stateCookie)
	if ls.StatusCode != http.StatusFound || sc == nil || len(sc.Name)+len(sc.Value) >= 4096 || len(sc.String()) >= 4096 {
		t.Fatalf("login: %d, state cookie %d bytes", ls.StatusCode, len(sc.String()))
	}
	cb := e.oidc.authorize(ls.Header.Get("Location"))
	done := serve(e.a.Handler(), req("GET", cb, sc))
	if done.StatusCode != http.StatusFound || done.Header.Get("Location") != testIssuer+uri {
		t.Fatalf("callback: %d %q", done.StatusCode, done.Header.Get("Location"))
	}
	session := cookieNamed(done, SessionCookie)
	_, page := consentFields(t, e.authorize(newBrowser(session), params))
	if !strings.Contains(page, "You are signed in as <strong>alice@acme.com</strong>.") ||
		!strings.Contains(page, `<a href="https://backyard.test/__backyard/auth/logout">Not you? Sign out</a>`) {
		t.Errorf("oidc consent:\n%s", page)
	}
	// Forbidden by the gates.
	e2 := newMCPEnv(t, ModeOIDC, func(c *config.Config) { c.Auth.AllowedEmails = []string{"bob@acme.com"} })
	e2.oidc.idp.setUser(map[string]any{"sub": "00u-carl", "email": "carl@other.example", "name": "Carl"})
	sess := forbiddenSession(t, e2)
	id2 := e2.register("App", loopbackCB)
	res = e2.authorize(newBrowser(sess), authzParams(id2, loopbackCB, ch, "st"))
	if b := body(t, res); res.StatusCode != http.StatusForbidden || !strings.Contains(b, "Access denied") || !strings.Contains(b, "Sign out") {
		t.Errorf("forbidden oidc: %d\n%s", res.StatusCode, b)
	}
}

// forbiddenSession seals a session for the IdP's current user without the
// gates (as if the user had been allowed when signing in).
func forbiddenSession(t *testing.T, e *mcpEnv) *http.Cookie {
	t.Helper()
	v, err := e.a.sess.seal(&identity.User{ID: "carl@other.example", Email: "carl@other.example", Name: "Carl"},
		e.clock.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: SessionCookie, Value: v}
}

func TestMCPConsentPost(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("App", loopbackCB)
	other := e.register("Other", loopbackCB)
	f := newFlow(id, loopbackCB)
	b := newBrowser()
	fields, _ := consentFields(t, e.authorize(b, authzParams(id, loopbackCB, f.challenge, f.state)))

	// Allow: 303 with code, state and iss; the grant is pending.
	q := redirectParams(t, e.consentPost(b, fields, "allow"), http.StatusSeeOther, loopbackCB+"?")
	if !strings.HasPrefix(q.Get("code"), prefixCode) || q.Get("state") != f.state || q.Get("iss") != testIssuer || len(q) != 3 {
		t.Fatalf("allow: %v", q)
	}
	if e.grantCount() != 1 || len(e.auditActions()) != 1 || e.auditActions()[0] != "mcp.authorize" {
		t.Fatalf("grants %d, audit %v", e.grantCount(), e.auditActions())
	}
	// Deny: access_denied, no grant.
	q = redirectParams(t, e.consentPost(b, fields, "deny"), http.StatusSeeOther, loopbackCB+"?")
	if q.Get("error") != "access_denied" || q.Get("state") != f.state || q.Get("iss") != testIssuer {
		t.Fatalf("deny: %v", q)
	}
	if e.grantCount() != 1 {
		t.Fatal("deny created a grant")
	}
	// No or another decision.
	for _, d := range []string{"", "maybe"} {
		if res := e.consentPost(b, fields, d); res.StatusCode != http.StatusBadRequest || res.Header.Get("Location") != "" {
			t.Errorf("decision %q: %d", d, res.StatusCode)
		}
	}

	forbidden := func(name string, res *http.Response, text string) {
		t.Helper()
		if b := body(t, res); res.StatusCode != http.StatusForbidden || res.Header.Get("Location") != "" || !strings.Contains(b, text) {
			t.Errorf("%s: %d %q\n%s", name, res.StatusCode, res.Header.Get("Location"), b)
		}
	}
	const expired = "This approval page expired or was not issued to you. Go back to the app and connect again."
	tamper := func(k, v string) url.Values {
		c := url.Values{}
		for kk, vv := range fields {
			c[kk] = append([]string(nil), vv...)
		}
		if v == "" {
			c.Del(k)
		} else {
			c.Set(k, v)
		}
		return c
	}
	_, ch2 := pkce()
	for k, v := range map[string]string{
		"client_id":      other,
		"redirect_uri":   "http://127.0.0.1:10/callback",
		"state":          "other-state",
		"code_challenge": ch2,
		"scope":          "mcp",
		"csrf":           fields.Get("csrf")[:len(fields.Get("csrf"))-2] + "AA",
	} {
		forbidden("tampered "+k, e.consentPost(b, tamper(k, v), "allow"), expired)
	}
	forbidden("no csrf", e.consentPost(b, tamper("csrf", ""), "allow"), expired)
	forbidden("removed state", e.consentPost(b, tamper("state", ""), "allow"), expired)
	// Tampered fields that fail validation take their normal route.
	if q := redirectParams(t, e.consentPost(b, tamper("resource", "https://other.example/__backyard/mcp"), "allow"),
		http.StatusSeeOther, loopbackCB); q.Get("error") != "invalid_target" {
		t.Errorf("tampered resource: %v", q)
	}
	if q := redirectParams(t, e.consentPost(b, tamper("resource", ""), "allow"), http.StatusSeeOther, loopbackCB); q.Get("error") != "invalid_request" {
		t.Errorf("missing resource: %v", q)
	}
	if q := redirectParams(t, e.consentPost(b, tamper("response_type", ""), "allow"), http.StatusSeeOther, loopbackCB); q.Get("error") != "unsupported_response_type" {
		t.Errorf("missing response_type: %v", q)
	}
	if q := redirectParams(t, e.consentPost(b, tamper("code_challenge_method", ""), "allow"), http.StatusSeeOther, loopbackCB); q.Get("error") != "invalid_request" {
		t.Errorf("missing code_challenge_method: %v", q)
	}
	dup := tamper("state", "")
	dup["state"] = []string{f.state, f.state}
	if res := e.consentPost(b, dup, "allow"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("repeated field: %d", res.StatusCode)
	}
	// Another guest (another browser) cannot use this form.
	forbidden("another user", e.consentPost(newBrowser(), fields, "allow"), expired)
	// Browser signals.
	for name, h := range map[string][2]string{
		"cross-site": {"Sec-Fetch-Site", "cross-site"}, "same-site": {"Sec-Fetch-Site", "same-site"},
		"foreign origin": {"Origin", "https://evil.example"}, "site origin": {"Origin", "https://evil.backyard.test"},
		"cors": {"Sec-Fetch-Mode", "cors"}, "iframe": {"Sec-Fetch-Dest", "iframe"},
	} {
		r := b.request(http.MethodPost, testIssuer+AuthorizePath, tamper("decision", "allow"))
		r.Header.Set(h[0], h[1])
		forbidden(name, e.do(r), "This approval was not sent from the Backyard page")
	}
	// The hub origin (or no signals at all) is fine.
	r := b.request(http.MethodPost, testIssuer+AuthorizePath, tamper("decision", "allow"))
	r.Header.Set("Origin", testIssuer)
	if res := e.do(r); res.StatusCode != http.StatusSeeOther {
		t.Errorf("hub origin: %d %s", res.StatusCode, body(t, res))
	}
	if res := e.do(b.request(http.MethodPost, testIssuer+AuthorizePath, tamper("decision", "allow"))); res.StatusCode != http.StatusSeeOther {
		t.Errorf("no browser signals: %d", res.StatusCode)
	}
	// Not a form, or too big.
	r = b.request(http.MethodPost, testIssuer+AuthorizePath, fields)
	r.Header.Set("Content-Type", "application/json")
	if res := e.do(r); res.StatusCode != http.StatusBadRequest {
		t.Errorf("json consent: %d", res.StatusCode)
	}
	big := tamper("decision", "allow")
	big.Set("pad", strings.Repeat("x", 16<<10))
	if res := e.do(b.request(http.MethodPost, testIssuer+AuthorizePath, big)); res.StatusCode != http.StatusBadRequest {
		t.Errorf("17 KiB consent: %d", res.StatusCode)
	}
	// The CSRF token expires after 600 s, plus 60 s of skew.
	e.clock.Advance(650 * time.Second)
	if res := e.consentPost(b, fields, "allow"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("within skew: %d", res.StatusCode)
	}
	e.clock.Advance(20 * time.Second)
	forbidden("expired csrf", e.consentPost(b, fields, "allow"), expired)
	// A CSRF token signed with another purpose's key is not accepted.
	cliMAC := formMAC(e.a.confirmKey, []any{"mcp-consent"})
	forbidden("foreign key", e.consentPost(b, tamper("csrf", formToken(e.clock.Now().Unix()+60, cliMAC)), "allow"), expired)
}

func TestMCPConsentPostOIDC(t *testing.T) {
	e := newMCPEnv(t, ModeOIDC, nil)
	id := e.register("App", loopbackCB)
	f := newFlow(id, loopbackCB)
	s1 := e.oidc.session()
	b := newBrowser(s1)
	fields, _ := consentFields(t, e.authorize(b, authzParams(id, loopbackCB, f.challenge, f.state)))
	// Another sign-in of the same user (a new session) invalidates the form.
	s2 := e.oidc.session()
	res := e.consentPost(newBrowser(s2), fields, "allow")
	if res.StatusCode != http.StatusForbidden || !strings.Contains(body(t, res), "expired or was not issued to you") {
		t.Errorf("another session: %d", res.StatusCode)
	}
	// Signed out: a page that links to the login, returning to the GET
	// form (no redirect: the consent page's form-action would block the
	// hop to the IdP).
	res = e.consentPost(newBrowser(), fields, "allow")
	page := body(t, res)
	links := regexp.MustCompile(`<a href="([^"]+)"`).FindStringSubmatch(page)
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("Location") != "" || len(links) != 2 ||
		!strings.Contains(page, "Sign in again") || !strings.Contains(res.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("signed out: %d %v %s", res.StatusCode, res.Header, page)
	}
	loc := html.UnescapeString(links[1])
	lu, _ := url.Parse(loc)
	rd := lu.Query().Get("rd")
	if lu.Path != LoginPath || !strings.HasPrefix(rd, testIssuer+AuthorizePath+"?response_type=code&client_id=") ||
		strings.Contains(rd, "csrf") || strings.Contains(rd, "decision") || e.a.sanitizeRedirect(rd) != rd {
		t.Fatalf("signed out: link %q", loc)
	}
	back, _ := url.Parse(rd)
	if back.Query().Get("state") != f.state || back.Query().Get("resource") != testResource {
		t.Errorf("rd query = %v", back.Query())
	}
	// The original session works.
	q := redirectParams(t, e.consentPost(b, fields, "allow"), http.StatusSeeOther, loopbackCB)
	if q.Get("code") == "" {
		t.Errorf("allow: %v", q)
	}
	_ = context.Background()
}

// A signed-out browser in oidc mode is sent to the login (or shown the
// sign-in-ended page) whatever junk from its address has spent: those answers
// identify no one, are cheap and only lead to the IdP, so they are not
// charged to the address (SPEC §11).
func TestMCPAuthorizeSignedOutNotLimited(t *testing.T) {
	var mu sync.Mutex
	counts := map[string]int{}
	allow := func(k string) (bool, time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		counts[k]++
		return counts[k] <= 5, time.Minute
	}
	e := newMCPEnv(t, ModeOIDC, nil, func(o *Options) { o.MCPAllow = allow })
	id := e.register("App", loopbackCB)
	f := newFlow(id, loopbackCB)
	fields, _ := consentFields(t, e.authorize(newBrowser(e.oidc.session()), authzParams(id, loopbackCB, f.challenge, f.state)))
	junk := func() *http.Response {
		return e.do(newBrowser().request(http.MethodGet, testIssuer+AuthorizePath+"?client_id=x", nil))
	}
	limited := false
	for range 10 {
		if junk().StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("junk was never limited")
	}
	params := authzParams(id, loopbackCB, f.challenge, f.state)
	for range 3 {
		res := e.authorize(newBrowser(), params)
		if loc := res.Header.Get("Location"); res.StatusCode != http.StatusFound || !strings.HasPrefix(loc, e.a.LoginURL("")+"?rd=") {
			t.Fatalf("signed out, after junk: %d %q", res.StatusCode, loc)
		}
		res = e.consentPost(newBrowser(), fields, "allow")
		if b := body(t, res); res.StatusCode != http.StatusUnauthorized || !strings.Contains(b, "Your Backyard sign-in ended") {
			t.Fatalf("signed-out consent, after junk: %d\n%s", res.StatusCode, b)
		}
	}
	// Junk stays limited.
	if res := junk(); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("junk after the login redirects: %d", res.StatusCode)
	}
}
