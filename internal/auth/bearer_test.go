package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func bearer(r *http.Request, tok string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+tok)
	return r
}

func TestStaticTokens(t *testing.T) {
	cfg := baseConfig("none")
	cfg.Auth.Tokens = []config.StaticToken{
		{Name: "ci", SHA256: sha("ci-secret-token")},
		{Name: "ops", SHA256: strings.ToUpper(sha("ops-secret-token")), Admin: true},
	}
	cfg.Auth.AllowedDomains = []string{"acme.com"} // static tokens bypass gates
	a := newAuth(t, cfg, Options{})
	tests := []struct {
		tok   string
		id    string
		admin bool
	}{
		{"ci-secret-token", "token:ci", false},
		{"ops-secret-token", "token:ops", true},
	}
	for _, tc := range tests {
		u := mustAuth(t, a, bearer(req("GET", "https://backyard.test/"), tc.tok))
		if u.ID != tc.id || u.Admin != tc.admin || u.Anonymous || u.Name != strings.TrimPrefix(tc.id, "token:") {
			t.Fatalf("%s: user = %+v", tc.tok, u)
		}
	}
	// Scheme is case-insensitive.
	r := req("GET", "https://backyard.test/")
	r.Header.Set("Authorization", "bearer   ci-secret-token ")
	if u := mustAuth(t, a, r); u.ID != "token:ci" {
		t.Fatalf("lowercase scheme: %+v", u)
	}
	// An unknown token is an error even in mode none (it must not silently
	// become a guest: the CLI would never learn that its token expired).
	rec := httptest.NewRecorder()
	if u, err := a.Authenticate(rec, bearer(req("GET", "https://backyard.test/"), "wrong")); !errors.Is(err, ErrUnauthenticated) || u != nil {
		t.Fatalf("unknown token in none mode = %+v, %v", u, err)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("anonymous cookie set for a rejected bearer token")
	}
	// Without a bearer token the guest identity is used.
	if u := mustAuth(t, a, req("GET", "https://backyard.test/")); !u.Anonymous {
		t.Fatalf("no token in none mode = %+v", u)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"nil", nil, "nil config"},
		{"short secret", func(c *config.Config) { c.Server.Secret = "short" }, "secret"},
		{"bad static hash", func(c *config.Config) { c.Auth.Tokens = []config.StaticToken{{Name: "x", SHA256: "zz"}} }, "auth.tokens[0]"},
		{"static without name", func(c *config.Config) { c.Auth.Tokens = []config.StaticToken{{SHA256: sha("a")}} }, "auth.tokens[0]"},
		{"bad proxy", func(c *config.Config) { c.Server.TrustedProxies = []string{"nope"} }, "trusted_proxies"},
		{"bad public url", func(c *config.Config) { c.Server.PublicURL = "ftp://x" }, "public_url"},
		{"unknown mode", func(c *config.Config) { c.Auth.Mode = "kerberos" }, "auth.mode"},
		{"oidc without issuer", func(c *config.Config) { c.Auth.Mode = "oidc" }, "issuer"},
		{"iap without audience", func(c *config.Config) { c.Auth.Mode = "iap" }, "audience"},
		{"alb without region", func(c *config.Config) { c.Auth.Mode = "alb" }, "region"},
		{"header without headers", func(c *config.Config) { c.Auth.Mode = "header" }, "email_header"},
		{"header without proxies", func(c *config.Config) {
			c.Auth.Mode = "header"
			c.Auth.Header.EmailHeader = "X-Email"
		}, "trusted_proxies"},
		{"header jwt without jwks", func(c *config.Config) {
			c.Auth.Mode = "header"
			c.Auth.Header.JWTHeader = "X-JWT"
		}, "jwks_url"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cfg *config.Config
			if tc.mutate != nil {
				cfg = baseConfig("none")
				tc.mutate(cfg)
			}
			_, err := New(context.Background(), cfg, Options{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestAPITokensViaAuthenticate(t *testing.T) {
	clock := newClock()
	cfg := baseConfig("iap")
	cfg.Auth.IAP.Audience = "aud"
	cfg.Auth.IAP.JWKSURL = "http://127.0.0.1:1/jwks"
	cfg.Auth.AllowedDomains = []string{"acme.com", "*.subsidiary.example"}
	cfg.Auth.Tokens = []config.StaticToken{{Name: "ci", SHA256: sha("ci-token"), Admin: true}}
	dir := mapDirectory{"alice@acme.com": {ID: "alice@acme.com", Email: "alice@acme.com", Title: "Director"}}
	a := newAuth(t, cfg, Options{Now: clock.Now, Directory: dir})
	tk := NewTokens(testSecret, time.Hour)
	tk.now = clock.Now
	issue := func(u *identity.User) string {
		tok, _, err := tk.Issue(u, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	auth := func(tok string) (*identity.User, error) {
		return a.Authenticate(httptest.NewRecorder(), bearer(req("GET", "https://backyard.test/"), tok))
	}

	// Admin is recomputed from config, not taken from the claim.
	u, err := auth(issue(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice", Admin: true}))
	if err != nil || u.Admin || u.Title != "Director" {
		t.Fatalf("alice = %+v, %v", u, err)
	}
	u, err = auth(issue(&identity.User{ID: "boss@acme.com", Email: "boss@acme.com"}))
	if err != nil || !u.Admin {
		t.Fatalf("boss = %+v, %v", u, err)
	}
	// Admin group member from another domain passes the gate.
	u, err = auth(issue(&identity.User{ID: "x@else.example", Email: "x@else.example", Groups: []string{"backyard-admins"}}))
	if err != nil || !u.Admin {
		t.Fatalf("group admin = %+v, %v", u, err)
	}
	if u, err = auth(issue(&identity.User{ID: "s@eu.subsidiary.example", Email: "s@eu.subsidiary.example"})); err != nil || u.Admin {
		t.Fatalf("wildcard domain: %+v, %v", u, err)
	}
	if _, err = auth(issue(&identity.User{ID: "eve@evil.example", Email: "eve@evil.example"})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("evil domain err = %v", err)
	}
	if _, err = auth(issue(&identity.User{ID: "sub:123"})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("no email with domain gate err = %v", err)
	}
	// Anonymous tokens are not valid outside mode none.
	if _, err = auth(issue(&identity.User{ID: "anon:0123456789abcdef"})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("anon token err = %v", err)
	}
	// API tokens minted for a static-token principal follow the static config.
	u, err = auth(issue(&identity.User{ID: "token:ci", Name: "ci"}))
	if err != nil || u.ID != "token:ci" || !u.Admin {
		t.Fatalf("token principal = %+v, %v", u, err)
	}
	if _, err = auth(issue(&identity.User{ID: "token:removed"})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("removed token principal err = %v", err)
	}
	// Garbage bearer and no credentials.
	if _, err = auth("garbage"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("garbage err = %v", err)
	}
	if _, err = auth(strings.Repeat("x", maxBearerBytes+1)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("huge err = %v", err)
	}
	// A JWT from an unknown issuer with no IdP configured.
	foreign := signJWT(t, jwt.SigningMethodHS256, []byte("k"), "", map[string]any{"iss": "https://idp"})
	if _, err = auth(foreign); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("foreign err = %v", err)
	}

	// Allowed groups gate.
	cfg.Auth.AllowedDomains = nil
	cfg.Auth.AllowedGroups = []string{"Eng"}
	if _, err = auth(issue(&identity.User{ID: "g@acme.com", Email: "g@acme.com", Groups: []string{"eng"}})); err != nil {
		t.Fatalf("group member: %v", err)
	}
	if _, err = auth(issue(&identity.User{ID: "h@acme.com", Email: "h@acme.com", Groups: []string{"sales"}})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-member err = %v", err)
	}

	// Allowed emails: alone they admit only the listed addresses.
	cfg.Auth.AllowedGroups = nil
	cfg.Auth.AllowedEmails = []string{" Ana@Gmail.com "}
	if _, err = auth(issue(&identity.User{ID: "ana@gmail.com", Email: "ana@gmail.com"})); err != nil {
		t.Fatalf("listed email: %v", err)
	}
	for _, u := range []*identity.User{
		{ID: "bob@gmail.com", Email: "bob@gmail.com"},
		{ID: "ana@gmail.com.evil.example", Email: "ana@gmail.com.evil.example"},
		{ID: "g@acme.com", Email: "g@acme.com"},
		{ID: "sub:ana"},
	} {
		if _, err = auth(issue(u)); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%s with an email list err = %v", u.ID, err)
		}
	}
	// With allowed domains, either list admits; allowed groups still apply on top.
	cfg.Auth.AllowedDomains = []string{"acme.com"}
	for _, id := range []string{"ana@gmail.com", "g@acme.com"} {
		if _, err = auth(issue(&identity.User{ID: id, Email: id})); err != nil {
			t.Fatalf("%s with domains and emails: %v", id, err)
		}
	}
	if _, err = auth(issue(&identity.User{ID: "bob@gmail.com", Email: "bob@gmail.com"})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unlisted with domains and emails err = %v", err)
	}
	cfg.Auth.AllowedGroups = []string{"Eng"}
	if _, err = auth(issue(&identity.User{ID: "ana@gmail.com", Email: "ana@gmail.com"})); !errors.Is(err, ErrForbidden) {
		t.Fatalf("listed email outside the allowed groups err = %v", err)
	}
	if _, err = auth(issue(&identity.User{ID: "ana@gmail.com", Email: "ana@gmail.com", Groups: []string{"eng"}})); err != nil {
		t.Fatalf("listed email in an allowed group: %v", err)
	}
}

func TestDomainAllowed(t *testing.T) {
	tests := []struct {
		domain  string
		allowed []string
		want    bool
	}{
		{"acme.com", []string{"acme.com"}, true},
		{"acme.com", []string{" ACME.com "}, true},
		{"acme.com", []string{"@acme.com"}, true},
		{"eu.acme.com", []string{"acme.com"}, false},
		{"eu.acme.com", []string{"*.acme.com"}, true},
		{"eu.acme.com", []string{".acme.com"}, true},
		{"acme.com", []string{"*.acme.com"}, false},
		{"evilacme.com", []string{"*.acme.com"}, false},
		{"", []string{"acme.com"}, false},
		{"x.io", []string{""}, false},
	}
	for _, tc := range tests {
		if got := domainAllowed(tc.domain, tc.allowed); got != tc.want {
			t.Errorf("domainAllowed(%q, %v) = %v", tc.domain, tc.allowed, got)
		}
	}
}

func TestIdPBearerTokens(t *testing.T) {
	e := newOIDCEnv(t, nil)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := e.clock.Now()
	tests := []struct {
		name  string
		tok   string
		ok    bool
		email string
	}{
		{"aud device client", e.idp.accessToken(map[string]any{"aud": "backyard-cli"}), true, "carol@acme.com"},
		{"aud array", e.idp.accessToken(map[string]any{"aud": []any{"other", "api://backyard"}}), true, "carol@acme.com"},
		{"aud web client", e.idp.accessToken(map[string]any{"aud": "backyard-web"}), true, "carol@acme.com"},
		{"azp", e.idp.accessToken(map[string]any{"aud": "other", "azp": "backyard-cli"}), true, "carol@acme.com"},
		{"okta cid", e.idp.accessToken(map[string]any{"aud": "api://default", "cid": "backyard-cli"}), true, "carol@acme.com"},
		{"wrong audience", e.idp.accessToken(map[string]any{"aud": "some-other-app"}), false, ""},
		{"no audience", e.idp.accessToken(map[string]any{}), false, ""},
		{"expired", e.idp.accessToken(map[string]any{"aud": "backyard-cli", "exp": now.Add(-time.Minute).Unix()}), false, ""},
		{"wrong issuer", e.idp.accessToken(map[string]any{"aud": "backyard-cli", "iss": "https://evil.example"}), false, ""},
		{"foreign key", signJWT(t, jwt.SigningMethodRS256, otherKey, e.idp.kid,
			map[string]any{"iss": e.idp.issuer(), "aud": "backyard-cli", "sub": "x", "email": "x@acme.com",
				"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}), false, ""},
		{"no email maps to sub", e.idp.accessToken(map[string]any{"aud": "backyard-cli", "email": nil}), true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := e.a.Authenticate(nil, bearer(req("GET", "https://backyard.test/__backyard/api/deploy/me"), tc.tok))
			if !tc.ok {
				if !errors.Is(err, ErrUnauthenticated) {
					t.Fatalf("err = %v, want unauthenticated", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if u.Email != tc.email || (tc.email == "" && u.ID != "sub:00u-carol") {
				t.Fatalf("user = %+v", u)
			}
		})
	}
}

func TestBearerPrecedence(t *testing.T) {
	e := newOIDCEnv(t, nil)
	sess := e.session()
	tk := NewTokens(testSecret, time.Hour)
	tk.now = e.clock.Now
	tok, _, _ := tk.Issue(&identity.User{ID: "dave@acme.com", Email: "dave@acme.com", Name: "Dave"}, time.Hour)

	// A valid bearer wins over the session.
	if u := mustAuth(t, e.a, bearer(req("GET", "https://backyard.test/", sess), tok)); u.ID != "dave@acme.com" {
		t.Fatalf("bearer+session = %+v", u)
	}
	// An invalid bearer falls back to the session.
	if u := mustAuth(t, e.a, bearer(req("GET", "https://backyard.test/", sess), "bogus")); u.ID != "alice@acme.com" {
		t.Fatalf("bad bearer+session = %+v", u)
	}
	// An invalid bearer without other credentials reports the bearer error.
	_, err := e.a.Authenticate(nil, bearer(req("GET", "https://backyard.test/"), tok+"x"))
	if !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "api token") {
		t.Fatalf("err = %v", err)
	}
	// Non-bearer Authorization schemes are ignored.
	r := req("GET", "https://backyard.test/", sess)
	r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if u := mustAuth(t, e.a, r); u.ID != "alice@acme.com" {
		t.Fatalf("basic+session = %+v", u)
	}
}

func TestNewBearer(t *testing.T) {
	e := newOIDCEnv(t, nil)
	b, err := NewBearer(context.Background(), e.cfg, Options{Now: e.clock.Now, HTTPClient: e.idp.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode() != ModeOIDC || b.LoginURL("https://backyard.test/") != "" {
		t.Fatalf("mode=%q login=%q", b.Mode(), b.LoginURL("x"))
	}
	for _, p := range []string{LoginPath, CallbackPath, LogoutPath, CLIPath} {
		if res := serve(b.Handler(), req("GET", "https://backyard.test"+p)); res.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d", p, res.StatusCode)
		}
	}
	// Session cookies are ignored.
	sess := e.session()
	if _, err := b.Authenticate(nil, req("GET", "https://gw.example/me", sess)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("cookie err = %v", err)
	}
	// IdP tokens and API tokens work.
	if u := mustAuth(t, b, bearer(req("GET", "https://gw.example/me"), e.idp.accessToken(map[string]any{"aud": "backyard-cli"}))); u.Email != "carol@acme.com" {
		t.Fatalf("idp = %+v", u)
	}
	tk := NewTokens(testSecret, time.Hour)
	tk.now = e.clock.Now
	tok, _, _ := tk.Issue(&identity.User{ID: "dave@acme.com", Email: "dave@acme.com"}, time.Hour)
	rec := httptest.NewRecorder()
	if u, err := b.Authenticate(rec, bearer(req("GET", "https://gw.example/me"), tok)); err != nil || u.ID != "dave@acme.com" {
		t.Fatalf("api = %+v, %v", u, err)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("bearer authenticator set cookies")
	}
	if _, err := b.Authenticate(nil, bearer(req("GET", "https://gw.example/me"), "nope")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("bad token err = %v", err)
	}

	// Mode none: no anonymous fallback for the standalone gateway.
	nb, err := NewBearer(context.Background(), baseConfig("none"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nb.Authenticate(httptest.NewRecorder(), req("GET", "https://gw.example/me")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("none mode bearer err = %v", err)
	}
	if _, err := NewBearer(context.Background(), nil, Options{}); err == nil {
		t.Fatal("nil config")
	}
}

func TestIdPBearerLazyDiscovery(t *testing.T) {
	clock := newClock()
	idp := newMockIdP(t, clock)
	cfg := baseConfig("none")
	// Unreachable at construction time.
	cfg.Auth.OIDC.Issuer = idp.issuer()
	cfg.Auth.OIDC.DeviceClientID = "backyard-cli"
	b, err := NewBearer(context.Background(), cfg, Options{Now: clock.Now, HTTPClient: &http.Client{Transport: failingTransport{}}})
	if err != nil {
		t.Fatal(err)
	}
	ba := b.(*authenticator)
	tok := idp.accessToken(map[string]any{"aud": "backyard-cli"})
	// An unreachable IdP is an infrastructure error, not bad credentials.
	if _, err := b.Authenticate(nil, bearer(req("GET", "https://gw/"), tok)); !errors.Is(err, errKeysUnavailable) || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
	// Tokens of other issuers are rejected without contacting the IdP.
	foreign := signJWT(t, jwt.SigningMethodRS256, idp.key, idp.kid, map[string]any{
		"iss": "https://evil.example", "sub": "x", "aud": "backyard-cli", "exp": clock.Now().Add(time.Hour).Unix(),
	})
	if _, err := b.Authenticate(nil, bearer(req("GET", "https://gw/"), foreign)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("foreign issuer err = %v", err)
	}
	// The IdP becomes reachable; retry happens after the back-off window.
	ba.idp.client = idp.srv.Client()
	if _, err := b.Authenticate(nil, bearer(req("GET", "https://gw/"), tok)); err == nil {
		t.Fatal("retried inside the back-off window")
	}
	clock.Advance(discoveryRetry + time.Second)
	tok = idp.accessToken(map[string]any{"aud": "backyard-cli"})
	if u, err := b.Authenticate(nil, bearer(req("GET", "https://gw/"), tok)); err != nil || u.Email != "carol@acme.com" {
		t.Fatalf("after recovery = %+v, %v", u, err)
	}

	// No acceptable audience configured: IdP tokens are not accepted at all.
	cfg2 := baseConfig("none")
	cfg2.Auth.OIDC.Issuer = idp.issuer()
	b2, _ := NewBearer(context.Background(), cfg2, Options{})
	if b2.(*authenticator).idp != nil {
		t.Fatal("idp bearer without audiences")
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network down")
}

func TestBoundedClient(t *testing.T) {
	if c := boundedClient(nil); c.Timeout != defaultHTTPTimeout {
		t.Fatal("default timeout")
	}
	orig := &http.Client{}
	if c := boundedClient(orig); c.Timeout != defaultHTTPTimeout || orig.Timeout != 0 {
		t.Fatal("copy with timeout")
	}
	withTimeout := &http.Client{Timeout: time.Second}
	if boundedClient(withTimeout) != withTimeout {
		t.Fatal("kept client")
	}
}

func TestModeNormalization(t *testing.T) {
	for in, want := range map[string]string{
		"": "none", "OFF": "none", "Okta": "oidc", "entra": "oidc", "google": "oidc", "IAP": "iap", "alb": "alb",
	} {
		if got := normalizeMode(in); got != want {
			t.Errorf("normalizeMode(%q) = %q, want %q", in, got, want)
		}
	}
}
