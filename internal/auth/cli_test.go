package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
)

func cliURL(redirect, state string, extra ...string) string {
	v := url.Values{}
	if redirect != "" {
		v.Set("redirect_uri", redirect)
	}
	if state != "" {
		v.Set("state", state)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return "https://backyard.test" + CLIPath + "?" + v.Encode()
}

func TestCLIFlowOIDC(t *testing.T) {
	e := newOIDCEnv(t, func(c *config.Config) { c.Auth.TokenTTL = config.Duration(48 * time.Hour) })
	target := cliURL("http://127.0.0.1:5555/callback?keep=1", "st-123")

	// Unauthenticated: redirect to login with rd back to the same CLI URL.
	res := serve(e.a.Handler(), req("GET", target))
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d", res.StatusCode)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	if loc.Path != LoginPath || loc.Query().Get("rd") != target {
		t.Fatalf("login redirect = %s", loc)
	}
	// The login flow returns to the CLI URL (rd survives sanitization).
	idpURL, sc := e.startLogin(loc.Query().Get("rd"))
	cb := serve(e.a.Handler(), req("GET", e.authorize(idpURL), sc))
	if cb.Header.Get("Location") != target {
		t.Fatalf("callback redirect = %q", cb.Header.Get("Location"))
	}
	sess := cookieNamed(cb, SessionCookie)

	res = serve(e.a.Handler(), req("GET", target, sess))
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d: %s", res.StatusCode, body(t, res))
	}
	if !strings.Contains(res.Header.Get("Cache-Control"), "no-store") || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("headers = %v", res.Header)
	}
	dest, _ := url.Parse(res.Header.Get("Location"))
	if dest.Host != "127.0.0.1:5555" || dest.Path != "/callback" || dest.Query().Get("keep") != "1" ||
		dest.Query().Get("state") != "st-123" {
		t.Fatalf("redirect = %s", dest)
	}
	exp, err := time.Parse(time.RFC3339, dest.Query().Get("expires_at"))
	if err != nil || !exp.Equal(e.clock.Now().Add(48*time.Hour)) {
		t.Fatalf("expires_at = %q (%v)", dest.Query().Get("expires_at"), err)
	}
	tk := NewTokens(testSecret, 0)
	tk.now = e.clock.Now
	u, err := tk.Verify(dest.Query().Get("token"))
	if err != nil || u.ID != "alice@acme.com" || !u.Admin {
		t.Fatalf("token user = %+v, %v", u, err)
	}
	// The minted token authenticates against the API.
	if got := mustAuth(t, e.a, bearer(req("GET", "https://foo.backyard.test/__backyard/api/me"), dest.Query().Get("token"))); got.ID != "alice@acme.com" {
		t.Fatalf("api user = %+v", got)
	}

	// Display mode renders the token without a redirect_uri.
	res = serve(e.a.Handler(), req("GET", cliURL("", "", "mode", "display"), sess))
	b := body(t, res)
	if res.StatusCode != http.StatusOK || !strings.Contains(b, "backyard login --server https://backyard.test --token ey") ||
		!strings.Contains(res.Header.Get("Cache-Control"), "no-store") || !strings.Contains(b, "<textarea") ||
		!strings.Contains(res.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatalf("display = %d %v\n%s", res.StatusCode, res.Header, b)
	}

	// A bearer token cannot renew itself through the CLI endpoint.
	res = serve(e.a.Handler(), bearer(req("GET", target), dest.Query().Get("token")))
	if res.StatusCode != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), "https://backyard.test"+LoginPath) {
		t.Fatalf("bearer on cli = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestCLIValidation(t *testing.T) {
	e := newOIDCEnv(t, nil)
	sess := e.session()
	tests := []struct {
		name   string
		url    string
		status int
	}{
		{"missing redirect", cliURL("", "s"), http.StatusBadRequest},
		{"remote redirect", cliURL("http://evil.example:5555/cb", "s"), http.StatusBadRequest},
		{"https redirect", cliURL("https://127.0.0.1:5555/cb", "s"), http.StatusBadRequest},
		{"no port", cliURL("http://127.0.0.1/cb", "s"), http.StatusBadRequest},
		{"missing state", cliURL("http://127.0.0.1:5555/cb", ""), http.StatusBadRequest},
		{"huge state", cliURL("http://127.0.0.1:5555/cb", strings.Repeat("s", 2000)), http.StatusBadRequest},
		{"ok", cliURL("http://[::1]:5555/cb", "s"), http.StatusFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := serve(e.a.Handler(), req("GET", tc.url, sess))
			if res.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", res.StatusCode, tc.status, body(t, res))
			}
			if tc.status == http.StatusBadRequest && res.Header.Get("Location") != "" {
				t.Fatal("redirected on invalid request")
			}
		})
	}
	for _, m := range []string{"POST", "HEAD"} {
		if res := serve(e.a.Handler(), req(m, cliURL("http://127.0.0.1:5555/cb", "s"), sess)); res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s = %d", m, res.StatusCode)
		}
	}
}

func TestCLIForbiddenAndProxyModes(t *testing.T) {
	// Gate failure renders 403.
	e := newOIDCEnv(t, nil)
	sess := e.session()
	e.cfg.Auth.AllowedGroups = []string{"finance"}
	e.cfg.Auth.Admins, e.cfg.Auth.AdminGroups = nil, nil
	if res := serve(e.a.Handler(), req("GET", cliURL("http://127.0.0.1:5555/cb", "s"), sess)); res.StatusCode != http.StatusForbidden {
		t.Fatalf("gated = %d", res.StatusCode)
	}

	// Header mode: the proxy authenticated the browser.
	cfg := baseConfig("header")
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.Auth.Header.EmailHeader = "X-Forwarded-Email"
	a := newAuth(t, cfg, Options{})
	r := req("GET", cliURL("http://127.0.0.1:5555/cb", "s"))
	r.RemoteAddr = "10.1.2.3:5000"
	r.Header.Set("X-Forwarded-Email", "erin@acme.com")
	res := serve(a.Handler(), r)
	if res.StatusCode != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), "http://127.0.0.1:5555/cb?") {
		t.Fatalf("header cli = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// Unauthenticated proxy request: 401 page, no login redirect.
	res = serve(a.Handler(), req("GET", cliURL("http://127.0.0.1:5555/cb", "s")))
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated header cli = %d", res.StatusCode)
	}
	if a.LoginURL("x") != "" {
		t.Fatal("proxy modes have no login URL")
	}
	for _, p := range []string{LoginPath, CallbackPath, LogoutPath} {
		if res := serve(a.Handler(), req("GET", "https://backyard.test"+p)); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d", p, res.StatusCode)
		}
	}
}

func TestCLINoneMode(t *testing.T) {
	cfg := baseConfig("none")
	cfg.Auth.Tokens = []config.StaticToken{{Name: "ci", SHA256: sha("ci")}}
	a := newAuth(t, cfg, Options{})
	res := serve(a.Handler(), req("GET", cliURL("http://localhost:4000/cb", "xyz")))
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d: %s", res.StatusCode, body(t, res))
	}
	dest, _ := url.Parse(res.Header.Get("Location"))
	anon := cookieNamed(res, AnonCookie)
	if anon == nil {
		t.Fatal("anon cookie not set")
	}
	u, err := NewTokens(testSecret, 0).Verify(dest.Query().Get("token"))
	if err != nil || !u.Anonymous || !strings.HasPrefix(u.ID, "anon:") {
		t.Fatalf("anon token = %+v, %v", u, err)
	}
	// The anonymous token is accepted in mode none and maps to the same guest.
	got := mustAuth(t, a, bearer(req("GET", "https://foo.backyard.test/"), dest.Query().Get("token")))
	if got.ID != u.ID || !got.Anonymous {
		t.Fatalf("token guest = %+v", got)
	}
}

func TestCLIRejectsAnonymousOutsideNone(t *testing.T) {
	// A header-mode request that resolves to an anonymous-looking principal is
	// impossible, so exercise the guard directly via a static-token session in
	// oidc mode: static users never come from browsers, but the check must hold.
	e := newOIDCEnv(t, nil)
	v, err := e.a.sess.seal(&identity.User{ID: "token:ci", Name: "ci"}, e.clock.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := serve(e.a.Handler(), req("GET", cliURL("http://127.0.0.1:5555/cb", "s"), &http.Cookie{Name: SessionCookie, Value: v}))
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if _, err := e.a.Authenticate(nil, req("GET", "https://backyard.test/")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal(err)
	}
}

func TestCLITokenKeepsImportantGroups(t *testing.T) {
	cfg := baseConfig("header")
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.Auth.Header.EmailHeader = "X-Forwarded-Email"
	cfg.Auth.Header.GroupsHeader = "X-Forwarded-Groups"
	cfg.Auth.Admins = nil
	a := newAuth(t, cfg, Options{})
	groups := make([]string, 0, 1500)
	for i := range 1500 {
		groups = append(groups, "team-"+strings.Repeat("g", 12)+"-"+strconv.Itoa(i))
	}
	groups = append(groups, "backyard-admins") // last: would be truncated first
	r := req("GET", cliURL("http://127.0.0.1:5555/cb", "s"))
	r.RemoteAddr = "10.1.2.3:5000"
	r.Header.Set("X-Forwarded-Email", "erin@acme.com")
	r.Header.Set("X-Forwarded-Groups", strings.Join(groups, ","))
	res := serve(a.Handler(), r)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("status = %d: %s", res.StatusCode, body(t, res))
	}
	dest, _ := url.Parse(res.Header.Get("Location"))
	tok := dest.Query().Get("token")
	if len(tok) > maxTokenBytes {
		t.Fatalf("token too large: %d", len(tok))
	}
	u := mustAuth(t, a, bearer(req("GET", "https://backyard.test/"), tok))
	if !u.Admin || !u.InGroup("backyard-admins") || len(u.Groups) >= len(groups) {
		t.Fatalf("token user admin=%v groups=%d", u.Admin, len(u.Groups))
	}
}
