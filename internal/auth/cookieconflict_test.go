package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

// expired lists the "domain path" of the cookies called name that res
// expires (domain "" = host-only).
func expired(res *http.Response, name string) []string {
	var out []string
	for _, c := range res.Cookies() {
		if c.Name == name && c.MaxAge < 0 {
			out = append(out, strings.TrimPrefix(c.Domain, ".")+" "+c.Path)
		}
	}
	return out
}

// TestPlantedSessionCookieFailsClosed: in host mode a site's page can plant
// its author's sealed session for the cookie domain with a longer Path, and
// browsers send it before the visitor's own Path=/ cookie. Backyard must not
// pick either: the request is unauthenticated, and the cookies Backyard did
// not set (other paths, other domains) are expired — never its own
// Domain=.<base_domain>; Path=/ cookie, whose removal would leave only the
// planted one.
func TestPlantedSessionCookieFailsClosed(t *testing.T) {
	e := newOIDCEnv(t, nil)
	alice := e.session()
	e.idp.setUser(map[string]any{"sub": "00u-mallory", "email": "mallory@acme.com", "name": "Mallory"})
	mallory := e.session()
	if alice.Value == mallory.Value {
		t.Fatal("sessions must differ")
	}

	for _, order := range [][]*http.Cookie{{mallory, alice}, {alice, mallory}} {
		for _, target := range []string{
			"https://foo.backyard.test/__backyard/api/db/notes",
			"https://backyard.test/__backyard/api/tokens",
			"https://backyard.test/__backyard/api/deploy",
		} {
			rec := httptest.NewRecorder()
			u, err := e.a.Authenticate(rec, req("GET", target, order...))
			if u != nil || !errors.Is(err, ErrUnauthenticated) || !errors.Is(err, ErrCookieConflict) {
				t.Fatalf("%s: Authenticate = %+v, %v (want a cookie conflict)", target, u, err)
			}
			got := expired(rec.Result(), SessionCookie)
			if slices.Contains(got, "backyard.test /") {
				t.Fatalf("%s: the session cookie Backyard set must not be expired: %v", target, got)
			}
			// The planted variants: the cookie domain at the request's
			// longer paths, parent domains, and stale host-only cookies.
			for _, want := range []string{"backyard.test /__backyard/api", "backyard.test /__backyard/api/", " /"} {
				if !slices.Contains(got, want) {
					t.Fatalf("%s: %q not expired: %v", target, want, got)
				}
			}
		}
	}

	// Without a ResponseWriter nothing is written, and the request still
	// fails closed.
	if _, err := e.a.Authenticate(nil, req("GET", "https://foo.backyard.test/", mallory, alice)); !errors.Is(err, ErrCookieConflict) {
		t.Fatalf("nil writer: err = %v", err)
	}

	// Several cookies of the same user (a stale host-only one next to the
	// domain cookie) are fine, and so is an invalid cookie next to a valid
	// one.
	v, err := e.a.sess.seal(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com"}, e.clock.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	again := &http.Cookie{Name: SessionCookie, Value: v}
	for _, cookies := range [][]*http.Cookie{{again, alice}, {{Name: SessionCookie, Value: "garbage"}, alice}} {
		rec := httptest.NewRecorder()
		u, err := e.a.Authenticate(rec, req("GET", "https://foo.backyard.test/__backyard/api/me", cookies...))
		if err != nil || u.ID != "alice@acme.com" || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("same user = %+v, %v, %v", u, err, rec.Result().Cookies())
		}
	}

	// A bearer token still authenticates the request.
	tok, _, err := e.a.tokens.Issue(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	r := req("GET", "https://foo.backyard.test/__backyard/api/me", mallory, alice)
	r.Header.Set("Authorization", "Bearer "+tok)
	if u := mustAuth(t, e.a, r); u.ID != "alice@acme.com" {
		t.Fatalf("bearer user = %+v", u)
	}

	// The CLI handoff neither mints for the first cookie nor loops through
	// the IdP: it explains the conflict.
	res := serve(e.a.Handler(), req("GET", cliURL("http://127.0.0.1:5555/callback", "st"), mallory, alice))
	if b := body(t, res); res.StatusCode != http.StatusUnauthorized || res.Header.Get("Location") != "" ||
		!strings.Contains(b, "Conflicting sign-in cookies") {
		t.Fatalf("CLI = %d %q: %s", res.StatusCode, res.Header.Get("Location"), b)
	}
	if got := expired(res, SessionCookie); !slices.Contains(got, "backyard.test /__backyard/auth/cli") {
		t.Fatalf("CLI: planted variants not expired: %v", got)
	}
	if b := e.a.browserSessionBinding(req("GET", "https://backyard.test"+CLIPath, mallory, alice)); b != "" {
		t.Fatal("conflicting cookies must not bind a confirmation")
	}
}

// TestPlantedLoginStateCookieIgnored: a site's page can plant a
// backyard_oauth cookie for the cookie domain, which the hub receives. Over
// https the login state lives in a __Host- cookie that only the hub can set,
// so mallory's planted state cannot complete her own login in alice's
// browser (login CSRF).
func TestPlantedLoginStateCookieIgnored(t *testing.T) {
	e := newOIDCEnv(t, nil)
	e.idp.setUser(map[string]any{"sub": "00u-mallory", "email": "mallory@acme.com"})
	idpURL, sc := e.startLogin("")
	if sc.Name != OAuthHostCookie || sc.Path != "/" || sc.Domain != "" || !sc.Secure {
		t.Fatalf("state cookie = %+v (want __Host-backyard_oauth; Secure; Path=/; no Domain)", sc)
	}
	cb := e.authorize(idpURL)

	// The planted cookie carries mallory's valid state under the name a
	// sibling subdomain can set.
	planted := &http.Cookie{Name: OAuthCookie, Value: sc.Value}
	res := serve(e.a.Handler(), req("GET", cb, planted))
	assertNoSession(t, res, http.StatusBadRequest)

	// Over http (no __Host- prefix possible) the cookie keeps its name.
	h := newOIDCEnv(t, func(c *config.Config) {
		c.Server.BaseDomain, c.Server.PublicURL = "lvh.me", "http://lvh.me:8080"
		c.Auth.OIDC.RedirectURL = "http://lvh.me:8080/__backyard/auth/callback"
	})
	if h.a.oidc.stateCookie != OAuthCookie || h.a.oidc.statePath != PathPrefix {
		t.Fatalf("http state cookie = %q path %q", h.a.oidc.stateCookie, h.a.oidc.statePath)
	}
	// The custom callback path outside /__backyard/auth/ uses Path=/ too.
	c := newOIDCEnv(t, func(c *config.Config) { c.Auth.OIDC.RedirectURL = "https://backyard.test/oauth2/callback" })
	if c.a.oidc.stateCookie != OAuthHostCookie || c.a.oidc.statePath != "/" {
		t.Fatalf("custom callback state cookie = %q path %q", c.a.oidc.stateCookie, c.a.oidc.statePath)
	}
}

// TestPlantedGuestCookieFailsClosed: the same rule for backyard_anon in mode
// none — a guest id is a capability for that guest's data.
func TestPlantedGuestCookieFailsClosed(t *testing.T) {
	a := newAuth(t, baseConfig("none"), Options{})
	sign := func(id string) *http.Cookie {
		return &http.Cookie{Name: AnonCookie, Value: keys.Sign(keys.Derive(testSecret, keys.Anon), []byte(id))}
	}
	victim, planted := sign("0123456789abcdef"), sign("fedcba9876543210")

	rec := httptest.NewRecorder()
	u, err := a.Authenticate(rec, req("POST", "https://foo.backyard.test/__backyard/api/db/notes", planted, victim))
	if u != nil || !errors.Is(err, ErrUnauthenticated) || !errors.Is(err, ErrCookieConflict) {
		t.Fatalf("Authenticate = %+v, %v (want a cookie conflict)", u, err)
	}
	got := expired(rec.Result(), AnonCookie)
	if slices.Contains(got, "backyard.test /") || !slices.Contains(got, "backyard.test /__backyard/api") {
		t.Fatalf("expired = %v", got)
	}
	// No replacement guest is minted.
	for _, c := range rec.Result().Cookies() {
		if c.Name == AnonCookie && c.MaxAge > 0 {
			t.Fatalf("minted %+v", c)
		}
	}

	// The same guest twice, or an invalid cookie next to a valid one, is
	// fine.
	for _, cookies := range [][]*http.Cookie{{victim, victim}, {{Name: AnonCookie, Value: "x"}, victim}} {
		rec := httptest.NewRecorder()
		u, err := a.Authenticate(rec, req("GET", "https://foo.backyard.test/", cookies...))
		if err != nil || u.ID != "anon:0123456789abcdef" || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("same guest = %+v, %v, %v", u, err, rec.Result().Cookies())
		}
	}
}

func TestExpireForeignCookies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		target string
		uri    string // request line, when it differs from target's path
		want   []string
	}{
		{
			name:   "host mode",
			target: "https://foo.backyard.test/a/b",
			want: []string{
				" /", " /a", " /a/", " /a/b",
				"backyard.test /a", "backyard.test /a/", "backyard.test /a/b",
			},
		},
		{
			name: "parent domains",
			mutate: func(c *config.Config) {
				c.Server.BaseDomain, c.Server.PublicURL = "backyard.acme.com", "https://backyard.acme.com"
			},
			target: "https://foo.backyard.acme.com/x/",
			want: []string{
				" /", " /x", " /x/",
				"backyard.acme.com /x", "backyard.acme.com /x/",
				"acme.com /", "acme.com /x", "acme.com /x/",
			},
		},
		{
			// The handler sees a stripped path; the browser matched cookies
			// against the request line's.
			name:   "stripped path",
			target: "https://foo.backyard.test/db",
			uri:    "/__backyard/api/db?x=/y",
			want: []string{
				" /", " /__backyard", " /__backyard/", " /__backyard/api", " /__backyard/api/", " /__backyard/api/db",
				"backyard.test /__backyard", "backyard.test /__backyard/", "backyard.test /__backyard/api",
				"backyard.test /__backyard/api/", "backyard.test /__backyard/api/db",
			},
		},
		{
			name:   "path mode",
			mutate: func(c *config.Config) { c.Server.URLMode = "path" },
			target: "https://backyard.test/s/foo",
			want:   []string{" /s", " /s/", " /s/foo", "backyard.test /", "backyard.test /s", "backyard.test /s/", "backyard.test /s/foo"},
		},
		{
			name: "localhost",
			mutate: func(c *config.Config) {
				c.Server.BaseDomain, c.Server.PublicURL = "localhost", "http://localhost:8080"
			},
			target: "http://foo.localhost:8080/p",
			want:   []string{" /p"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig("none")
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			a := newAuth(t, cfg, Options{})
			r := req("GET", tc.target)
			if tc.uri != "" {
				r.RequestURI = tc.uri
			}
			rec := httptest.NewRecorder()
			a.expireForeignCookies(rec, r, SessionCookie)
			got := expired(rec.Result(), SessionCookie)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("expired =\n%q\nwant\n%q", got, want)
			}
			if len(rec.Result().Cookies()) != len(rec.Header().Values("Set-Cookie")) {
				t.Fatal("a Set-Cookie header did not parse")
			}
		})
	}
}

func TestCookiePathPrefixes(t *testing.T) {
	cases := map[string][]string{
		"":         {"/"},
		"*":        {"/"},
		"/":        {"/"},
		"/a":       {"/", "/a"},
		"/a/":      {"/", "/a", "/a/"},
		"/a/b":     {"/", "/a", "/a/", "/a/b"},
		"/a//b":    {"/", "/a", "/a/", "/a//", "/a//b"},
		"/a;b/c":   {"/"},
		"/a%3Bb/c": {"/", "/a%3Bb", "/a%3Bb/", "/a%3Bb/c"},
		"/1/2/3/4/5/6/7/8/9/10": {"/", "/1", "/1/", "/1/2", "/1/2/", "/1/2/3", "/1/2/3/", "/1/2/3/4", "/1/2/3/4/",
			"/1/2/3/4/5", "/1/2/3/4/5/", "/1/2/3/4/5/6", "/1/2/3/4/5/6/", "/1/2/3/4/5/6/7", "/1/2/3/4/5/6/7/",
			"/1/2/3/4/5/6/7/8", "/1/2/3/4/5/6/7/8/"},
	}
	for p, want := range cases {
		if got := cookiePathPrefixes(p); !slices.Equal(got, want) {
			t.Errorf("cookiePathPrefixes(%q) = %q, want %q", p, got, want)
		}
	}
}
