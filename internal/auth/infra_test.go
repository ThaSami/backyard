package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// flakyServer answers 503 until healthy is set, then delegates to h.
type flakyServer struct {
	*httptest.Server
	healthy atomic.Bool
	hits    atomic.Int32
}

func newFlakyServer(t *testing.T, h http.Handler) *flakyServer {
	t.Helper()
	s := &flakyServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		if !s.healthy.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// isInfra reports whether err is an infrastructure error (neither
// ErrUnauthenticated nor ErrForbidden).
func isInfra(err error) bool {
	return err != nil && !errors.Is(err, ErrUnauthenticated) && !errors.Is(err, ErrForbidden)
}

func TestIAPKeyServerOutageIsInfrastructureError(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	jwks := newJWKSServer(t, ecJWK(t, &key.PublicKey, "iap-1"))
	srv := newFlakyServer(t, jwks.Config.Handler)
	clock := newClock()
	cfg := baseConfig("iap")
	cfg.Auth.IAP.Audience = iapAud
	cfg.Auth.IAP.JWKSURL = srv.URL
	a := newAuth(t, cfg, Options{Now: clock.Now})
	e := &iapEnv{a: a, key: key, kid: "iap-1", clock: clock}

	_, err = a.Authenticate(nil, iapReq(e.assertion(t, nil)))
	if !isInfra(err) || !errors.Is(err, errKeysUnavailable) {
		t.Fatalf("outage: err = %v, want an infrastructure error", err)
	}
	// The CLI handoff reports the outage instead of "not signed in".
	r := iapReq(e.assertion(t, nil))
	r.URL.Path = CLIPath
	r.URL.RawQuery = "mode=display"
	res := serve(a.Handler(), r)
	if b := body(t, res); res.StatusCode != http.StatusServiceUnavailable || strings.Contains(b, "<textarea") {
		t.Fatalf("cli during outage: %d %s", res.StatusCode, b)
	}
	// A missing assertion is still a plain 401, outage or not.
	if _, err := a.Authenticate(nil, iapReq("")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no assertion: %v", err)
	}

	// Recovery (after the refresh throttle) restores normal operation, and
	// bad signatures are credentials errors again.
	srv.healthy.Store(true)
	clock.Advance(jwksMinRefresh + time.Second)
	if u := mustAuth(t, a, iapReq(e.assertion(t, nil))); u.ID != "alice@acme.com" {
		t.Fatalf("after recovery: %+v", u)
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	forged := (&iapEnv{key: other, kid: "iap-1", clock: clock}).assertion(t, nil)
	if _, err := a.Authenticate(nil, iapReq(forged)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("forged after recovery: %v", err)
	}
}

func TestALBKeyServerOutageIsInfrastructureError(t *testing.T) {
	e := newALBEnv(t, albARN)
	healthyURL := e.a.alb.keyURL
	down := newFlakyServer(t, http.NotFoundHandler())
	e.a.alb.keyURL = down.URL

	tok := e.token(t, "kid-1", nil, nil, true)
	if _, err := e.a.Authenticate(nil, albReq(tok)); !isInfra(err) {
		t.Fatalf("outage: err = %v", err)
	}
	// The negative cache keeps the classification without refetching.
	hits := down.hits.Load()
	if _, err := e.a.Authenticate(nil, albReq(tok)); !isInfra(err) {
		t.Fatalf("cached outage: err = %v", err)
	}
	if down.hits.Load() != hits {
		t.Fatal("key refetched inside the negative TTL")
	}
	// A 404 for a forged kid is a credentials error.
	down.healthy.Store(true)
	if _, err := e.a.Authenticate(nil, albReq(e.token(t, "kid-x", nil, nil, true))); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown kid: %v", err)
	}
	// After the negative TTL the real key server is used again.
	e.a.alb.keyURL = healthyURL
	e.clock.Advance(albNegativeTTL + time.Second)
	if u := mustAuth(t, e.a, albReq(e.token(t, "kid-1", nil, nil, true))); u.ID != "alice@acme.com" {
		t.Fatalf("after recovery: %+v", u)
	}
}

func TestHeaderJWTKeyServerOutageIsInfrastructureError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := newJWKSServer(t, rsaJWK(&key.PublicKey, "cf-1"))
	srv := newFlakyServer(t, jwks.Config.Handler)
	clock := newClock()
	cfg := baseConfig("header")
	cfg.Auth.Header.JWTHeader = "Cf-Access-Jwt-Assertion"
	cfg.Auth.Header.JWKSURL = srv.URL
	cfg.Auth.Header.Issuer = "https://team.cloudflareaccess.com"
	cfg.Auth.Header.Audiences = []string{"aud-1"}
	a := newAuth(t, cfg, Options{Now: clock.Now})
	tok := signJWT(t, jwt.SigningMethodRS256, key, "cf-1", map[string]any{
		"iss": "https://team.cloudflareaccess.com", "aud": []string{"aud-1"}, "email": "alice@acme.com",
		"sub": "s-1", "iat": clock.Now().Unix(), "exp": clock.Now().Add(time.Hour).Unix(),
	})
	r := headerReq("203.0.113.5:1", map[string]string{"Cf-Access-Jwt-Assertion": tok})
	if _, err := a.Authenticate(nil, r); !isInfra(err) {
		t.Fatalf("outage: err = %v", err)
	}
	srv.healthy.Store(true)
	clock.Advance(jwksMinRefresh + time.Second)
	if u := mustAuth(t, a, headerReq("203.0.113.5:1", map[string]string{"Cf-Access-Jwt-Assertion": tok})); u.ID != "alice@acme.com" {
		t.Fatalf("after recovery: %+v", u)
	}
}

func TestIdPBearerKeyOutageIsInfrastructureError(t *testing.T) {
	clock := newClock()
	idp := newMockIdP(t, clock)
	cfg := baseConfig("iap")
	cfg.Auth.IAP.Audience = iapAud
	cfg.Auth.OIDC.Issuer = idp.issuer()
	cfg.Auth.OIDC.DeviceClientID = "backyard-cli"
	a := newAuth(t, cfg, Options{Now: clock.Now})
	// Discovery succeeded; now the JWKS endpoint becomes unreachable.
	p := a.idp.currentProvider()
	if p == nil {
		t.Fatal("no provider after eager discovery")
	}
	p.keys.url = idp.issuer() + "/missing-jwks"
	tok := idp.accessToken(map[string]any{"aud": "backyard-cli"})
	if _, err := a.Authenticate(nil, bearer(req("GET", "https://backyard.test/"), tok)); !isInfra(err) {
		t.Fatalf("jwks outage: err = %v", err)
	}
	// A token from another issuer never reaches the key server.
	foreign := signJWT(t, jwt.SigningMethodRS256, idp.key, idp.kid, map[string]any{
		"iss": "https://other.example", "aud": "backyard-cli", "sub": "x", "exp": clock.Now().Add(time.Hour).Unix(),
	})
	if _, err := a.Authenticate(nil, bearer(req("GET", "https://backyard.test/"), foreign)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("foreign issuer: err = %v", err)
	}
}

func TestHeaderModeRejectsDuplicatedIdentityHeaders(t *testing.T) {
	cfg := baseConfig("header")
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.Auth.Header.EmailHeader = "X-Forwarded-Email"
	cfg.Auth.Header.NameHeader = "X-Forwarded-Name"
	cfg.Auth.Header.GroupsHeader = "X-Forwarded-Groups"
	a := newAuth(t, cfg, Options{})
	for _, h := range []string{"X-Forwarded-Email", "X-Forwarded-Name", "X-Forwarded-Groups"} {
		t.Run(h, func(t *testing.T) {
			r := headerReq("10.0.0.1:1", map[string]string{
				"X-Forwarded-Email": "alice@acme.com", "X-Forwarded-Name": "Alice", "X-Forwarded-Groups": "eng",
			})
			// A client-supplied value followed by the proxy's appended one.
			first := map[string]string{"X-Forwarded-Email": "boss@acme.com", "X-Forwarded-Name": "Boss", "X-Forwarded-Groups": "backyard-admins"}[h]
			r.Header[h] = append([]string{first}, r.Header.Values(h)...)
			if u, err := a.Authenticate(nil, r); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("duplicated %s: user %+v, err %v", h, u, err)
			}
		})
	}
	// A single comma-separated groups value is fine.
	u := mustAuth(t, a, headerReq("10.0.0.1:1", map[string]string{"X-Forwarded-Email": "alice@acme.com", "X-Forwarded-Groups": "eng,ops"}))
	if !u.InGroup("ops") {
		t.Fatalf("groups = %v", u.Groups)
	}
}

func TestBearerInfrastructureErrorFallsBackToProxyIdentity(t *testing.T) {
	e := newIAPEnv(t)
	clock := e.clock
	idp := newMockIdP(t, clock)
	e.a.cfg.Auth.OIDC.Issuer = idp.issuer()
	e.a.cfg.Auth.OIDC.DeviceClientID = "backyard-cli"
	e.a.idp = e.a.newIDPBearer()
	e.a.idp.client = &http.Client{Transport: failingTransport{}} // IdP unreachable
	tok := idp.accessToken(map[string]any{"aud": "backyard-cli"})

	// IdP down + valid IAP assertion: the proxy identity wins.
	r := iapReq(e.assertion(t, nil))
	r.Header.Set("Authorization", "Bearer "+tok)
	if u := mustAuth(t, e.a, r); u.ID != "alice@acme.com" {
		t.Fatalf("user = %+v", u)
	}
	// IdP down and no proxy identity: the infrastructure error is reported.
	r = iapReq("")
	r.Header.Set("Authorization", "Bearer "+tok)
	if _, err := e.a.Authenticate(nil, r); !isInfra(err) {
		t.Fatalf("err = %v, want an infrastructure error", err)
	}
}
