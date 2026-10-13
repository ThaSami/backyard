package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
)

// oidcEnv is a hub (https://backyard.test) in oidc mode against a mock IdP.
type oidcEnv struct {
	t     *testing.T
	idp   *mockIdP
	cfg   *config.Config
	a     *authenticator
	clock *fakeClock
	// noFollow does not follow redirects (for the IdP's authorize endpoint).
	noFollow *http.Client
}

func newOIDCEnv(t *testing.T, mutate func(*config.Config), opts ...func(*Options)) *oidcEnv {
	t.Helper()
	clock := newClock()
	idp := newMockIdP(t, clock)
	cfg := baseConfig("okta")
	cfg.Auth.OIDC.Issuer = idp.issuer()
	cfg.Auth.OIDC.ClientID = idp.clientID
	cfg.Auth.OIDC.ClientSecret = idp.secret
	cfg.Auth.OIDC.DeviceClientID = "backyard-cli"
	cfg.Auth.OIDC.APIAudiences = []string{"api://backyard"}
	if mutate != nil {
		mutate(cfg)
	}
	o := Options{Now: clock.Now, HTTPClient: idp.srv.Client()}
	for _, f := range opts {
		f(&o)
	}
	return &oidcEnv{
		t: t, idp: idp, cfg: cfg, a: newAuth(t, cfg, o), clock: clock,
		noFollow: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// startLogin hits /login and returns the IdP URL and the state cookie.
func (e *oidcEnv) startLogin(rd string) (string, *http.Cookie) {
	e.t.Helper()
	target := "https://backyard.test" + LoginPath
	if rd != "" {
		target += "?rd=" + url.QueryEscape(rd)
	}
	res := serve(e.a.Handler(), req("GET", target))
	if res.StatusCode != http.StatusFound {
		e.t.Fatalf("login status = %d", res.StatusCode)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, e.idp.issuer()+"/authorize?") {
		e.t.Fatalf("login redirect = %q", loc)
	}
	sc := cookieNamed(res, OAuthCookie)
	if sc == nil || sc.Value == "" {
		e.t.Fatal("no state cookie")
	}
	return loc, sc
}

// authorize simulates the user signing in at the IdP and returns the
// callback URL the IdP redirects to.
func (e *oidcEnv) authorize(idpURL string) string {
	e.t.Helper()
	resp, err := e.noFollow.Get(idpURL)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("authorize status = %d: %s", resp.StatusCode, body(e.t, resp))
	}
	return resp.Header.Get("Location")
}

// login runs a full login and returns the callback response.
func (e *oidcEnv) login(rd string) *http.Response {
	e.t.Helper()
	idpURL, sc := e.startLogin(rd)
	cb := e.authorize(idpURL)
	return serve(e.a.Handler(), req("GET", cb, sc))
}

// session runs a successful login and returns the session cookie.
func (e *oidcEnv) session() *http.Cookie {
	e.t.Helper()
	res := e.login("")
	if res.StatusCode != http.StatusFound {
		e.t.Fatalf("callback status = %d: %s", res.StatusCode, body(e.t, res))
	}
	c := cookieNamed(res, SessionCookie)
	if c == nil {
		e.t.Fatal("no session cookie")
	}
	return c
}

func TestOIDCLoginFlow(t *testing.T) {
	e := newOIDCEnv(t, nil)
	rd := "https://foo.backyard.test/page?x=1#top"
	idpURL, sc := e.startLogin(rd)

	// The authorization request carries PKCE S256, nonce, state and scopes.
	u, _ := url.Parse(idpURL)
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" ||
		q.Get("state") == "" || q.Get("redirect_uri") != "https://backyard.test/__backyard/auth/callback" ||
		q.Get("scope") != "openid profile email" {
		t.Fatalf("authorize params = %v", q)
	}
	if sc.Path != PathPrefix || !sc.HttpOnly || !sc.Secure || sc.SameSite != http.SameSiteLaxMode ||
		sc.MaxAge != 600 || sc.Domain != "" {
		t.Fatalf("state cookie attrs = %+v", sc)
	}

	cb := e.authorize(idpURL)
	res := serve(e.a.Handler(), req("GET", cb, sc))
	if res.StatusCode != http.StatusFound {
		t.Fatalf("callback = %d: %s", res.StatusCode, body(t, res))
	}
	if loc := res.Header.Get("Location"); loc != rd {
		t.Fatalf("callback redirect = %q, want %q", loc, rd)
	}
	if res.Header.Get("Cache-Control") == "" {
		t.Fatal("callback must not be cacheable")
	}
	sess := cookieNamed(res, SessionCookie)
	if sess == nil {
		t.Fatal("no session cookie")
	}
	if strings.TrimPrefix(sess.Domain, ".") != "backyard.test" || sess.Path != "/" || !sess.HttpOnly ||
		!sess.Secure || sess.SameSite != http.SameSiteLaxMode || sess.MaxAge != int((12*time.Hour).Seconds()) {
		t.Fatalf("session cookie attrs = %+v", sess)
	}
	if len(sess.Value) > maxSessionCookieBytes {
		t.Fatalf("session cookie too large: %d", len(sess.Value))
	}
	if c := cookieNamed(res, OAuthCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("state cookie not cleared: %+v", c)
	}

	// The session works on a site host (shared cookie domain).
	user := mustAuth(t, e.a, req("GET", "https://foo.backyard.test/", sess))
	if user.ID != "alice@acme.com" || user.Email != "alice@acme.com" || user.Name != "Alice Smith" ||
		!user.Admin || user.Anonymous || !user.InGroup("eng") {
		t.Fatalf("user = %+v", user)
	}

	// Expired after session_ttl.
	e.clock.Advance(12*time.Hour + time.Second)
	if _, err := e.a.Authenticate(nil, req("GET", "https://foo.backyard.test/", sess)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired session err = %v", err)
	}
}

func TestOIDCSessionTampering(t *testing.T) {
	e := newOIDCEnv(t, nil)
	sess := e.session()
	tampered := []string{
		"",
		"not-base64!!",
		b64([]byte("short")),
		sess.Value[:len(sess.Value)-2] + "AA",
		strings.Repeat("A", 10000),
	}
	flip := []byte(sess.Value)
	if flip[20] == 'A' {
		flip[20] = 'B'
	} else {
		flip[20] = 'A'
	}
	tampered = append(tampered, string(flip))
	for i, v := range tampered {
		_, err := e.a.Authenticate(nil, req("GET", "https://backyard.test/", &http.Cookie{Name: SessionCookie, Value: v}))
		if !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("case %d: err = %v", i, err)
		}
	}
	// A session sealed with another secret is rejected.
	other, err := newSessionCodec("another-secret-0123456789abcdef0123456789", e.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := other.seal(&identity.User{ID: "mallory@acme.com", Email: "mallory@acme.com"}, e.clock.Now().Add(time.Hour), nil)
	if _, err := e.a.Authenticate(nil, req("GET", "https://backyard.test/", &http.Cookie{Name: SessionCookie, Value: v})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("foreign session err = %v", err)
	}
	// A stale cookie does not shadow a valid one.
	r := req("GET", "https://backyard.test/", &http.Cookie{Name: SessionCookie, Value: "garbage"}, sess)
	if u := mustAuth(t, e.a, r); u.ID != "alice@acme.com" {
		t.Fatalf("user = %+v", u)
	}
	// No cookie at all.
	if _, err := e.a.Authenticate(nil, req("GET", "https://backyard.test/")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no cookie err = %v", err)
	}
}

func TestOIDCCallbackFailures(t *testing.T) {
	t.Run("state mismatch", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, sc := e.startLogin("")
		cb, _ := url.Parse(e.authorize(idpURL))
		q := cb.Query()
		q.Set("state", "forged")
		cb.RawQuery = q.Encode()
		res := serve(e.a.Handler(), req("GET", cb.String(), sc))
		assertNoSession(t, res, http.StatusBadRequest)
	})
	t.Run("state from another login", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, _ := e.startLogin("")
		_, sc2 := e.startLogin("")
		res := serve(e.a.Handler(), req("GET", e.authorize(idpURL), sc2))
		assertNoSession(t, res, http.StatusBadRequest)
	})
	t.Run("missing state cookie", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, _ := e.startLogin("")
		res := serve(e.a.Handler(), req("GET", e.authorize(idpURL)))
		assertNoSession(t, res, http.StatusBadRequest)
		if b := body(t, res); !strings.Contains(b, "Try again") {
			t.Fatalf("no retry link: %s", b)
		}
	})
	t.Run("expired state cookie", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, sc := e.startLogin("")
		cb := e.authorize(idpURL)
		e.clock.Advance(11 * time.Minute)
		assertNoSession(t, serve(e.a.Handler(), req("GET", cb, sc)), http.StatusBadRequest)
	})
	t.Run("forged state cookie", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, sc := e.startLogin("")
		cb := e.authorize(idpURL)
		forged := &http.Cookie{Name: OAuthCookie, Value: sc.Value[:len(sc.Value)-3] + "xyz"}
		assertNoSession(t, serve(e.a.Handler(), req("GET", cb, forged)), http.StatusBadRequest)
	})
	t.Run("pkce mismatch", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, sc := e.startLogin("")
		u, _ := url.Parse(idpURL)
		q := u.Query()
		q.Set("code_challenge", b64([]byte("01234567890123456789012345678901")))
		u.RawQuery = q.Encode()
		res := serve(e.a.Handler(), req("GET", e.authorize(u.String()), sc))
		assertNoSession(t, res, http.StatusBadGateway)
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		e.idp.wrongNonce = true
		assertNoSession(t, e.login(""), http.StatusBadRequest)
	})
	t.Run("code replay", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, sc := e.startLogin("")
		cb := e.authorize(idpURL)
		if res := serve(e.a.Handler(), req("GET", cb, sc)); res.StatusCode != http.StatusFound {
			t.Fatalf("first = %d", res.StatusCode)
		}
		assertNoSession(t, serve(e.a.Handler(), req("GET", cb, sc)), http.StatusBadGateway)
	})
	t.Run("missing code", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, sc := e.startLogin("")
		cb, _ := url.Parse(e.authorize(idpURL))
		q := cb.Query()
		q.Del("code")
		cb.RawQuery = q.Encode()
		assertNoSession(t, serve(e.a.Handler(), req("GET", cb.String(), sc)), http.StatusBadRequest)
	})
	t.Run("idp error", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		idpURL, sc := e.startLogin("https://foo.backyard.test/")
		u, _ := url.Parse(idpURL)
		state := u.Query().Get("state")
		res := serve(e.a.Handler(), req("GET",
			"https://backyard.test/__backyard/auth/callback?error=access_denied&error_description=%3Cb%3Enope%3C%2Fb%3E&state="+url.QueryEscape(state), sc))
		assertNoSession(t, res, http.StatusForbidden)
		b := body(t, res)
		if strings.Contains(b, "<b>nope</b>") || !strings.Contains(b, "&lt;b&gt;nope") {
			t.Fatalf("error description not escaped: %s", b)
		}
		if !strings.Contains(b, url.QueryEscape("https://foo.backyard.test/")) {
			t.Fatalf("retry link should keep rd: %s", b)
		}
	})
	t.Run("crafted idp error link", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		_, sc := e.startLogin("")
		for _, c := range []*http.Cookie{nil, sc} {
			// No state, or a state that does not match the browser's login.
			res := serve(e.a.Handler(), req("GET",
				"https://backyard.test/__backyard/auth/callback?state=forged&error=Call%20%2B1-555-0100%20now&error_description=Your%20account%20is%20locked", c))
			b := body(t, res)
			if res.StatusCode != http.StatusBadRequest || strings.Contains(b, "locked") || strings.Contains(b, "555") {
				t.Fatalf("crafted text rendered (%d): %s", res.StatusCode, b)
			}
		}
	})
	t.Run("no identity", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		e.idp.setUser(map[string]any{"sub": "", "name": "Nobody"})
		assertNoSession(t, e.login(""), http.StatusForbidden)
	})
	t.Run("foreign authorized party", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		e.idp.setUser(map[string]any{"sub": "00u-alice", "email": "alice@acme.com", "azp": "other-client"})
		assertNoSession(t, e.login(""), http.StatusBadGateway)
	})
	t.Run("several audiences without authorized party", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		e.idp.setUser(map[string]any{"sub": "00u-alice", "email": "alice@acme.com", "aud": []any{e.idp.clientID, "other-client"}})
		assertNoSession(t, e.login(""), http.StatusBadGateway)
	})
	t.Run("method not allowed", func(t *testing.T) {
		e := newOIDCEnv(t, nil)
		res := serve(e.a.Handler(), req("POST", "https://backyard.test"+CallbackPath))
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d", res.StatusCode)
		}
	})
}

func TestOIDCAuthorizedPartyAccepted(t *testing.T) {
	e := newOIDCEnv(t, nil)
	e.idp.setUser(map[string]any{"sub": "00u-alice", "email": "alice@acme.com",
		"aud": []any{e.idp.clientID, "other-client"}, "azp": e.idp.clientID})
	if res := e.login(""); res.StatusCode != http.StatusFound || cookieNamed(res, SessionCookie) == nil {
		t.Fatalf("callback = %d: %s", res.StatusCode, body(t, res))
	}
}

func TestCheckAuthorizedParty(t *testing.T) {
	tests := []struct {
		name string
		aud  []string
		azp  any
		ok   bool
	}{
		{"single audience, no azp", []string{"c"}, nil, true},
		{"single audience, own azp", []string{"c"}, "c", true},
		{"single audience, foreign azp", []string{"c"}, "x", false},
		{"several audiences, own azp", []string{"c", "x"}, "c", true},
		{"several audiences, no azp", []string{"c", "x"}, nil, false},
		{"several audiences, foreign azp", []string{"c", "x"}, "x", false},
		{"non-string azp is ignored", []string{"c"}, 42.0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := map[string]any{}
			if tt.azp != nil {
				claims["azp"] = tt.azp
			}
			if err := checkAuthorizedParty(tt.aud, claims, "c"); (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func assertNoSession(t *testing.T, res *http.Response, status int) {
	t.Helper()
	if res.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", res.StatusCode, status, body(t, res))
	}
	if c := cookieNamed(res, SessionCookie); c != nil && c.Value != "" {
		t.Fatalf("unexpected session cookie %+v", c)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content type = %q", ct)
	}
}

func TestOIDCGates(t *testing.T) {
	e := newOIDCEnv(t, func(c *config.Config) {
		c.Auth.AllowedDomains = []string{"acme.com"}
		c.Auth.Admins = nil
		c.Auth.AdminGroups = nil
	})
	e.idp.setUser(map[string]any{"sub": "evil", "email": "eve@evil.example", "name": "Eve"})
	res := e.login("")
	assertNoSession(t, res, http.StatusForbidden)
	if b := body(t, res); !strings.Contains(b, "eve@evil.example") || !strings.Contains(b, "Access denied") {
		t.Fatalf("forbidden page = %s", b)
	}

	// Allowed user, then the gate tightens: the existing session is refused.
	e.idp.setUser(map[string]any{"sub": "a", "email": "alice@acme.com", "groups": []any{"eng"}})
	sess := e.session()
	if u := mustAuth(t, e.a, req("GET", "https://backyard.test/", sess)); u.Admin {
		t.Fatal("alice must not be admin")
	}
	e.cfg.Auth.AllowedGroups = []string{"finance"}
	if _, err := e.a.Authenticate(nil, req("GET", "https://backyard.test/", sess)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want forbidden", err)
	}
}

func TestOIDCUserInfoAndDirectory(t *testing.T) {
	dir := mapDirectory{"alice@acme.com": {ID: "alice@acme.com", Email: "alice@acme.com", Title: "Staff Engineer",
		Groups: []string{"dir-group"}}}
	e := newOIDCEnv(t, func(c *config.Config) {
		c.Auth.OIDC.UseUserInfo = true
		c.Identity.ExtraClaims = []string{"location"}
	}, func(o *Options) { o.Directory = dir })
	e.idp.userinfo = map[string]any{"department": "Platform", "location": "Ottawa", "iss": "https://evil"}
	sess := e.session()
	u := mustAuth(t, e.a, req("GET", "https://backyard.test/", sess))
	if u.Team != "Platform" || u.Extra["location"] != "Ottawa" || u.Title != "Staff Engineer" || !u.InGroup("dir-group") {
		t.Fatalf("user = %+v", u)
	}

	// userinfo for a different subject is rejected.
	e.idp.userinfoSub = "someone-else"
	assertNoSession(t, e.login(""), http.StatusBadGateway)
}

func TestOIDCLogout(t *testing.T) {
	e := newOIDCEnv(t, nil)
	res := serve(e.a.Handler(), req("GET", "https://backyard.test"+LogoutPath))
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Scheme+"://"+loc.Host+loc.Path != e.idp.issuer()+"/logout" ||
		loc.Query().Get("post_logout_redirect_uri") != "https://backyard.test/" ||
		loc.Query().Get("client_id") != "backyard-web" || loc.Query().Has("id_token_hint") {
		t.Fatalf("logout redirect = %s", loc)
	}
	var domainCleared, hostCleared bool
	for _, c := range res.Cookies() {
		if c.Name == SessionCookie && c.MaxAge < 0 {
			if c.Domain != "" {
				domainCleared = true
			} else {
				hostCleared = true
			}
		}
	}
	if !domainCleared || !hostCleared {
		t.Fatalf("session cookies not cleared: %v", res.Header["Set-Cookie"])
	}

	// Without an end_session_endpoint logout returns to the hub.
	clock := newClock()
	idp := newMockIdP(t, clock)
	idp.noEndSession = true
	cfg := baseConfig("oidc")
	cfg.Auth.OIDC.Issuer, cfg.Auth.OIDC.ClientID, cfg.Auth.OIDC.ClientSecret = idp.issuer(), idp.clientID, idp.secret
	a := newAuth(t, cfg, Options{Now: clock.Now})
	res = serve(a.Handler(), req("POST", "https://backyard.test"+LogoutPath))
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "https://backyard.test/" {
		t.Fatalf("logout = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestOIDCLoginURLAndRouting(t *testing.T) {
	e := newOIDCEnv(t, nil)
	if got := e.a.LoginURL("https://foo.backyard.test/a?b=c"); got !=
		"https://backyard.test/__backyard/auth/login?rd=https%3A%2F%2Ffoo.backyard.test%2Fa%3Fb%3Dc" {
		t.Fatalf("LoginURL = %q", got)
	}
	if got := e.a.LoginURL(""); got != "https://backyard.test/__backyard/auth/login" {
		t.Fatalf("LoginURL(\"\") = %q", got)
	}
	if e.a.Mode() != ModeOIDC {
		t.Fatalf("Mode = %q", e.a.Mode())
	}
	for _, p := range []string{"/__backyard/auth/", "/__backyard/auth/other", "/login"} {
		if res := serve(e.a.Handler(), req("GET", "https://backyard.test"+p)); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d", p, res.StatusCode)
		}
	}
	// An open-redirect rd is replaced by the hub at login time.
	idpURL, sc := e.startLogin("https://evil.example/")
	res := serve(e.a.Handler(), req("GET", e.authorize(idpURL), sc))
	if res.Header.Get("Location") != "https://backyard.test/" {
		t.Fatalf("redirect = %q", res.Header.Get("Location"))
	}
}

func TestOIDCCustomRedirectURL(t *testing.T) {
	e := newOIDCEnv(t, func(c *config.Config) {
		c.Auth.OIDC.RedirectURL = "https://backyard.test/oauth2/callback"
	})
	idpURL, sc := e.startLogin("")
	if sc.Path != "/" {
		t.Fatalf("state cookie path = %q", sc.Path)
	}
	cb := e.authorize(idpURL)
	if !strings.HasPrefix(cb, "https://backyard.test/oauth2/callback?") {
		t.Fatalf("callback = %s", cb)
	}
	if res := serve(e.a.Handler(), req("GET", cb, sc)); res.StatusCode != http.StatusFound || cookieNamed(res, SessionCookie) == nil {
		t.Fatalf("custom callback = %d", res.StatusCode)
	}
}

func TestOIDCSessionPathModeAndLocalhost(t *testing.T) {
	e := newOIDCEnv(t, func(c *config.Config) { c.Server.URLMode = "path" })
	if c := e.session(); c.Domain != "" {
		t.Fatalf("path mode cookie domain = %q", c.Domain)
	}
	e = newOIDCEnv(t, func(c *config.Config) {
		c.Server.BaseDomain = "localhost"
		c.Server.PublicURL = "http://localhost:8080"
		c.Auth.OIDC.RedirectURL = "http://localhost:8080/__backyard/auth/callback"
	})
	idpURL, sc := e.startLogin("")
	if sc.Secure {
		t.Fatal("http hub must not set Secure cookies")
	}
	res := serve(e.a.Handler(), req("GET", e.authorize(idpURL), sc))
	c := cookieNamed(res, SessionCookie)
	if c == nil || c.Domain != "" || c.Secure {
		t.Fatalf("localhost session cookie = %+v", c)
	}
}

func TestOIDCDiscoveryFailure(t *testing.T) {
	cfg := baseConfig("oidc")
	cfg.Auth.OIDC.Issuer = "http://127.0.0.1:1"
	cfg.Auth.OIDC.ClientID = "x"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := New(ctx, cfg, Options{}); err == nil || !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionTruncation(t *testing.T) {
	clock := newClock()
	c, err := newSessionCodec(testSecret, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	groups := make([]string, 0, 400)
	for i := range 400 {
		groups = append(groups, "group-with-a-fairly-long-name-"+strings.Repeat("x", i%7)+"-"+string(rune('a'+i%26))+time.Duration(i).String())
	}
	groups = append(groups, "backyard-admins")
	u := &identity.User{ID: "a@acme.com", Email: "a@acme.com", Name: "A", AvatarURL: "https://a",
		Groups: groups, Extra: map[string]any{"k": "v"}}
	important := func(g string) bool { return g == "backyard-admins" }
	v, err := c.seal(u, clock.Now().Add(time.Hour), important)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) > maxSessionCookieBytes {
		t.Fatalf("sealed len = %d", len(v))
	}
	got, err := c.open(v)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) == 0 || len(got.Groups) >= len(groups) || got.Groups[0] != "backyard-admins" {
		t.Fatalf("groups = %d first=%q", len(got.Groups), got.Groups[0])
	}
	if got.Extra["k"] != "v" || got.AvatarURL != "https://a" {
		t.Fatalf("optional fields dropped too eagerly: %+v", got)
	}

	// Huge extra data is dropped as a last resort.
	u2 := &identity.User{ID: "b@acme.com", Name: "B", Extra: map[string]any{"blob": strings.Repeat("x", 5000)}}
	v, err = c.seal(u2, clock.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.open(v); got == nil || got.Extra != nil {
		t.Fatalf("extra not dropped: %+v", got)
	}
	// Impossible to fit.
	if _, err := c.seal(&identity.User{ID: strings.Repeat("x", 5000)}, clock.Now().Add(time.Hour), nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := c.seal(&identity.User{}, clock.Now(), nil); err == nil {
		t.Fatal("expected error for empty user")
	}
}

func TestOIDCRedirectURLMustBeOnHub(t *testing.T) {
	clock := newClock()
	idp := newMockIdP(t, clock)
	for _, redirect := range []string{
		"https://login.backyard.test/__backyard/auth/callback",
		"https://foo.backyard.test/__backyard/auth/callback",
		"http://backyard.test/__backyard/auth/callback",
		"https://backyard.test:8443/__backyard/auth/callback",
		"/__backyard/auth/callback",
	} {
		cfg := baseConfig("oidc")
		cfg.Auth.OIDC.Issuer = idp.issuer()
		cfg.Auth.OIDC.ClientID = idp.clientID
		cfg.Auth.OIDC.RedirectURL = redirect
		if _, err := New(context.Background(), cfg, Options{Now: clock.Now, HTTPClient: idp.srv.Client()}); err == nil || !strings.Contains(err.Error(), "redirect_url") {
			t.Errorf("redirect_url %q: err = %v", redirect, err)
		}
	}
}

func TestOIDCUserInfoUnsupportedIsIgnored(t *testing.T) {
	clock := newClock()
	idp := newMockIdP(t, clock)
	idp.noUserInfo = true
	cfg := baseConfig("oidc")
	cfg.Auth.OIDC.Issuer = idp.issuer()
	cfg.Auth.OIDC.ClientID = idp.clientID
	cfg.Auth.OIDC.ClientSecret = idp.secret
	cfg.Auth.OIDC.UseUserInfo = true
	e := &oidcEnv{
		t: t, idp: idp, cfg: cfg, clock: clock,
		a:        newAuth(t, cfg, Options{Now: clock.Now, HTTPClient: idp.srv.Client()}),
		noFollow: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if e.a.oidc.useUserInfo {
		t.Fatal("userinfo still enabled without an endpoint")
	}
	if u := mustAuth(t, e.a, req("GET", "https://backyard.test/", e.session())); u.ID != "alice@acme.com" {
		t.Fatalf("user = %+v", u)
	}
}

func TestOIDCUnverifiedEmailDenialIsExplained(t *testing.T) {
	e := newOIDCEnv(t, func(c *config.Config) {
		c.Auth.AllowedDomains = []string{"acme.com"}
		c.Auth.Admins, c.Auth.AdminGroups = nil, nil
	})
	e.idp.setUser(map[string]any{"sub": "kc-123", "email": "alice@acme.com", "email_verified": false, "name": "Alice"})
	res := e.login("")
	b := body(t, res)
	if res.StatusCode != http.StatusForbidden || !strings.Contains(b, "has not verified the email address alice@acme.com") {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
	// A plain gate failure keeps the generic explanation.
	e.idp.setUser(map[string]any{"sub": "x-1", "email": "mallory@evil.example", "email_verified": true})
	res = e.login("")
	if b := body(t, res); res.StatusCode != http.StatusForbidden || strings.Contains(b, "not verified") || !strings.Contains(b, "Ask an administrator") {
		t.Fatalf("status %d: %s", res.StatusCode, b)
	}
}
