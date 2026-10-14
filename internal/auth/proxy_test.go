package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
)

// ---------------------------------------------------------------------------
// IAP

const (
	iapAud  = "/projects/123/global/backendServices/456"
	iapAud2 = "/projects/123/global/backendServices/789"
)

type iapEnv struct {
	a     *authenticator
	key   *ecdsa.PrivateKey
	kid   string
	jwks  *jwksServer
	clock *fakeClock
}

func newIAPEnv(t *testing.T) *iapEnv {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := newJWKSServer(t, ecJWK(t, &key.PublicKey, "iap-1"), rsaJWK(&rsaKey.PublicKey, "rsa-1"))
	clock := newClock()
	cfg := baseConfig("iap")
	cfg.Auth.IAP.Audience = iapAud
	cfg.Auth.IAP.AdditionalAudiences = []string{iapAud2}
	cfg.Auth.IAP.JWKSURL = jwks.URL
	return &iapEnv{a: newAuth(t, cfg, Options{Now: clock.Now}), key: key, kid: "iap-1", jwks: jwks, clock: clock}
}

func (e *iapEnv) assertion(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	now := e.clock.Now()
	claims := map[string]any{
		"iss": IAPIssuer, "aud": iapAud, "sub": "accounts.google.com:1234567890",
		"email": "alice@acme.com", "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(), "hd": "acme.com",
	}
	if mutate != nil {
		mutate(claims)
	}
	return signJWT(t, jwt.SigningMethodES256, e.key, e.kid, claims)
}

func iapReq(assertion string) *http.Request {
	r := req("GET", "https://foo.backyard.test/")
	if assertion != "" {
		r.Header.Set(IAPHeader, assertion)
	}
	return r
}

func TestIAP(t *testing.T) {
	e := newIAPEnv(t)
	u := mustAuth(t, e.a, iapReq(e.assertion(t, nil)))
	if u.ID != "alice@acme.com" || u.Name != "alice" || u.Anonymous {
		t.Fatalf("user = %+v", u)
	}
	if u := mustAuth(t, e.a, iapReq(e.assertion(t, func(c map[string]any) { c["aud"] = iapAud2 }))); u.ID != "alice@acme.com" {
		t.Fatalf("additional audience = %+v", u)
	}
	// Legacy prefixes and no email: id from sub.
	u = mustAuth(t, e.a, iapReq(e.assertion(t, func(c map[string]any) { delete(c, "email") })))
	if u.ID != "sub:1234567890" {
		t.Fatalf("sub user = %+v", u)
	}
	// gcip claims take precedence.
	u = mustAuth(t, e.a, iapReq(e.assertion(t, func(c map[string]any) {
		c["email"] = "securetoken.google.com/proj:bob@partner.example"
		c["gcip"] = map[string]any{"email": "bob@partner.example", "name": "Bob Partner", "iss": "ignored"}
	})))
	if u.ID != "bob@partner.example" || u.Name != "Bob Partner" {
		t.Fatalf("gcip user = %+v", u)
	}
	// Keys are cached.
	hits := e.jwks.hits
	mustAuth(t, e.a, iapReq(e.assertion(t, nil)))
	if e.jwks.hits != hits {
		t.Fatalf("jwks refetched: %d → %d", hits, e.jwks.hits)
	}
}

func TestIAPRejects(t *testing.T) {
	e := newIAPEnv(t)
	now := e.clock.Now()
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	valid := e.assertion(t, nil)
	cases := map[string]string{
		"missing":       "",
		"wrong aud":     e.assertion(t, func(c map[string]any) { c["aud"] = "/projects/1/global/backendServices/2" }),
		"wrong iss":     e.assertion(t, func(c map[string]any) { c["iss"] = "https://accounts.google.com" }),
		"expired":       e.assertion(t, func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }),
		"future iat":    e.assertion(t, func(c map[string]any) { c["iat"] = now.Add(time.Hour).Unix() }),
		"no iat":        e.assertion(t, func(c map[string]any) { delete(c, "iat") }),
		"other key":     signJWT(t, jwt.SigningMethodES256, otherKey, "iap-1", map[string]any{"iss": IAPIssuer, "aud": iapAud, "email": "a@acme.com", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}),
		"rs256":         signJWT(t, jwt.SigningMethodRS256, rsaKey, "rsa-1", map[string]any{"iss": IAPIssuer, "aud": iapAud, "email": "a@acme.com", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}),
		"tampered":      valid[:len(valid)-4] + "AAAA",
		"garbage":       "garbage",
		"no identity":   e.assertion(t, func(c map[string]any) { delete(c, "email"); delete(c, "sub") }),
		"hs256 confuse": signJWT(t, jwt.SigningMethodHS256, []byte("secret"), "iap-1", map[string]any{"iss": IAPIssuer, "aud": iapAud, "email": "a@acme.com", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := e.a.Authenticate(nil, iapReq(tok)); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// Proxy modes have no login URL; /login only redirects to rd (the proxy
	// signs the browser in), and there is no callback or logout.
	if e.a.LoginURL("https://x") != "" || e.a.Mode() != ModeIAP {
		t.Fatal("iap login/mode")
	}
	if res := serve(e.a.Handler(), req("GET", "https://backyard.test"+LoginPath+"?rd=%2Fdocs")); res.StatusCode != http.StatusFound ||
		res.Header.Get("Location") != "https://backyard.test/docs" {
		t.Fatalf("login = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, p := range []string{CallbackPath, LogoutPath} {
		if res := serve(e.a.Handler(), req("GET", "https://backyard.test"+p)); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d", p, res.StatusCode)
		}
	}
}

func TestIAPGatesAndAdmin(t *testing.T) {
	e := newIAPEnv(t)
	e.a.cfg.Auth.AllowedDomains = []string{"acme.com"}
	if u := mustAuth(t, e.a, iapReq(e.assertion(t, func(c map[string]any) { c["email"] = "boss@acme.com" }))); !u.Admin {
		t.Fatalf("boss = %+v", u)
	}
	_, err := e.a.Authenticate(nil, iapReq(e.assertion(t, func(c map[string]any) { c["email"] = "x@other.example" })))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------------------
// ALB

const albARN = "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/backyard/abc"

type albEnv struct {
	a     *authenticator
	key   *ecdsa.PrivateKey
	clock *fakeClock
	mu    sync.Mutex
	hits  map[string]int
}

func newALBEnv(t *testing.T, signer string) *albEnv {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	rsaDER, _ := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rsaDER})
	e := &albEnv{key: key, clock: newClock(), hits: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kid := strings.TrimPrefix(r.URL.Path, "/")
		e.mu.Lock()
		e.hits[kid]++
		e.mu.Unlock()
		switch kid {
		case "kid-1":
			_, _ = w.Write(pemKey)
		case "kid-rsa":
			_, _ = w.Write(rsaPEM)
		case "kid-junk":
			_, _ = w.Write([]byte("not pem"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := baseConfig("alb")
	cfg.Auth.ALB.Region = "us-east-1"
	cfg.Auth.ALB.KeyURL = srv.URL + "/"
	cfg.Auth.ALB.SignerARN = signer
	e.a = newAuth(t, cfg, Options{Now: e.clock.Now})
	return e
}

// token builds an ALB-style token with padded base64url segments.
func (e *albEnv) token(t *testing.T, kid string, hdr, payload map[string]any, pad bool) string {
	t.Helper()
	now := e.clock.Now()
	h := map[string]any{"typ": "JWT", "alg": "ES256", "kid": kid, "signer": albARN,
		"iss": "https://idp.example", "client": "client-1", "exp": now.Add(2 * time.Minute).Unix()}
	for k, v := range hdr {
		if v == nil {
			delete(h, k)
		} else {
			h[k] = v
		}
	}
	p := map[string]any{"sub": "u-1", "email": "alice@acme.com", "name": "Alice", "exp": now.Add(2 * time.Minute).Unix(),
		"iss": "https://idp.example"}
	for k, v := range payload {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	enc := base64.RawURLEncoding
	if pad {
		enc = base64.URLEncoding
	}
	hb, _ := json.Marshal(h)
	pb, _ := json.Marshal(p)
	input := enc.EncodeToString(hb) + "." + enc.EncodeToString(pb)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, e.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + enc.EncodeToString(sig)
}

func albReq(tok string) *http.Request {
	r := req("GET", "https://foo.backyard.test/")
	if tok != "" {
		r.Header.Set(ALBHeader, tok)
	}
	return r
}

func TestALB(t *testing.T) {
	e := newALBEnv(t, albARN)
	// Padded (as ALB emits) and unpadded segments are both accepted.
	for _, pad := range []bool{true, false} {
		u := mustAuth(t, e.a, albReq(e.token(t, "kid-1", nil, nil, pad)))
		if u.ID != "alice@acme.com" || u.Name != "Alice" {
			t.Fatalf("pad=%v user = %+v", pad, u)
		}
	}
	// Groups from userinfo claims.
	u := mustAuth(t, e.a, albReq(e.token(t, "kid-1", nil, map[string]any{"groups": "eng,backyard-admins"}, true)))
	if !u.Admin || !u.InGroup("eng") {
		t.Fatalf("groups user = %+v", u)
	}
	// Payload exp missing: header exp is used.
	if _, err := e.a.Authenticate(nil, albReq(e.token(t, "kid-1", nil, map[string]any{"exp": nil}, true))); err != nil {
		t.Fatalf("header exp: %v", err)
	}
	e.mu.Lock()
	hits := e.hits["kid-1"]
	e.mu.Unlock()
	if hits != 1 {
		t.Fatalf("key fetched %d times, want 1 (cached)", hits)
	}
	if e.a.LoginURL("x") != "" || e.a.Mode() != ModeALB {
		t.Fatal("alb login/mode")
	}
}

func TestALBRejects(t *testing.T) {
	e := newALBEnv(t, albARN)
	now := e.clock.Now()
	good := e.token(t, "kid-1", nil, nil, true)
	parts := strings.Split(good, ".")
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	other := &albEnv{key: otherKey, clock: e.clock}
	cases := map[string]string{
		"missing":         "",
		"wrong signer":    e.token(t, "kid-1", map[string]any{"signer": "arn:aws:elasticloadbalancing:us-east-1:999:loadbalancer/app/evil/1"}, nil, true),
		"no signer":       e.token(t, "kid-1", map[string]any{"signer": nil}, nil, true),
		"expired":         e.token(t, "kid-1", map[string]any{"exp": now.Add(-time.Hour).Unix()}, map[string]any{"exp": now.Add(-time.Hour).Unix()}, true),
		"no exp":          e.token(t, "kid-1", map[string]any{"exp": nil}, map[string]any{"exp": nil}, true),
		"wrong alg":       e.token(t, "kid-1", map[string]any{"alg": "HS256"}, nil, true),
		"no kid":          e.token(t, "kid-1", map[string]any{"kid": nil}, nil, true),
		"traversal kid":   e.token(t, "../../etc/passwd", nil, nil, true),
		"unknown kid":     e.token(t, "kid-404", nil, nil, true),
		"rsa key":         e.token(t, "kid-rsa", nil, nil, true),
		"junk key":        e.token(t, "kid-junk", nil, nil, true),
		"other signature": other.token(t, "kid-1", nil, nil, true),
		"payload swap":    parts[0] + "." + strings.Split(e.token(t, "kid-1", nil, map[string]any{"email": "boss@acme.com"}, true), ".")[1] + "." + parts[2],
		"short signature": parts[0] + "." + parts[1] + "." + b64([]byte("abc")),
		"two parts":       parts[0] + "." + parts[1],
		"bad header":      "%%%." + parts[1] + "." + parts[2],
		"huge":            strings.Repeat("a", maxALBTokenBytes+1),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := e.a.Authenticate(nil, albReq(tok)); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// Failed key ids are negatively cached.
	e.mu.Lock()
	before := e.hits["kid-404"]
	e.mu.Unlock()
	_, _ = e.a.Authenticate(nil, albReq(e.token(t, "kid-404", nil, nil, true)))
	e.mu.Lock()
	after := e.hits["kid-404"]
	e.mu.Unlock()
	if after != before {
		t.Fatalf("unknown kid refetched within negative TTL (%d → %d)", before, after)
	}
	e.clock.Advance(albNegativeTTL + time.Second)
	_, _ = e.a.Authenticate(nil, albReq(e.token(t, "kid-404", nil, nil, true)))
	e.mu.Lock()
	after = e.hits["kid-404"]
	e.mu.Unlock()
	if after != before+1 {
		t.Fatalf("unknown kid not retried after negative TTL (%d → %d)", before, after)
	}
}

func TestALBWithoutSignerCheck(t *testing.T) {
	e := newALBEnv(t, "")
	if u := mustAuth(t, e.a, albReq(e.token(t, "kid-1", map[string]any{"signer": "anything"}, nil, true))); u.ID != "alice@acme.com" {
		t.Fatalf("user = %+v", u)
	}
	// Default key URL derived from the region.
	cfg := baseConfig("alb")
	cfg.Auth.ALB.Region = "eu-west-1"
	cfg.Auth.ALB.KeyURL = ""
	a := newAuth(t, cfg, Options{})
	if a.alb.keyURL != "https://public-keys.auth.elb.eu-west-1.amazonaws.com" {
		t.Fatalf("key url = %q", a.alb.keyURL)
	}
}

func TestDecodeSegment(t *testing.T) {
	want := []byte{0xfb, 0xff, 0xbf, 0x01}
	for _, s := range []string{"-_-_AQ", "-_-_AQ==", "+/+/AQ==", "+/+/AQ"} {
		got, err := decodeSegment(s)
		if err != nil || string(got) != string(want) {
			t.Errorf("decodeSegment(%q) = %x, %v", s, got, err)
		}
	}
	if _, err := decodeSegment("!!"); err == nil {
		t.Fatal("garbage decoded")
	}
}

// ---------------------------------------------------------------------------
// Header

func headerReq(remote string, headers map[string]string) *http.Request {
	r := req("GET", "https://foo.backyard.test/")
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestHeaderMode(t *testing.T) {
	cfg := baseConfig("header")
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8", "2001:db8::1"}
	cfg.Auth.Header.EmailHeader = "X-Forwarded-Email"
	cfg.Auth.Header.NameHeader = "X-Forwarded-Name"
	cfg.Auth.Header.GroupsHeader = "X-Forwarded-Groups"
	a := newAuth(t, cfg, Options{})

	u := mustAuth(t, a, headerReq("10.1.2.3:5555", map[string]string{
		"X-Forwarded-Email": "Alice@Acme.com", "X-Forwarded-Name": "Alice Smith", "X-Forwarded-Groups": "eng, backyard-admins",
	}))
	if u.ID != "alice@acme.com" || u.Name != "Alice Smith" || !u.InGroup("eng") || !u.Admin {
		t.Fatalf("user = %+v", u)
	}
	// IPv6 and IPv4-mapped peers.
	if u := mustAuth(t, a, headerReq("[2001:db8::1]:443", map[string]string{"X-Forwarded-Email": "b@acme.com"})); u.ID != "b@acme.com" || u.Name != "b" {
		t.Fatalf("ipv6 = %+v", u)
	}
	if u := mustAuth(t, a, headerReq("[::ffff:10.0.0.9]:443", map[string]string{"X-Forwarded-Email": "c@acme.com"})); u.ID != "c@acme.com" {
		t.Fatalf("mapped = %+v", u)
	}
	// Legacy IAP-style prefix and usernames.
	if u := mustAuth(t, a, headerReq("10.0.0.1:1", map[string]string{"X-Forwarded-Email": "accounts.google.com:d@acme.com"})); u.ID != "d@acme.com" {
		t.Fatalf("prefixed = %+v", u)
	}
	if u := mustAuth(t, a, headerReq("10.0.0.1:1", map[string]string{"X-Forwarded-Email": "jdoe"})); u.ID != "sub:jdoe" || u.Name != "jdoe" || u.Email != "" {
		t.Fatalf("username = %+v", u)
	}

	// Untrusted peers and missing headers.
	for _, r := range []*http.Request{
		headerReq("192.168.1.1:5555", map[string]string{"X-Forwarded-Email": "boss@acme.com"}),
		headerReq("[2001:db8::2]:1", map[string]string{"X-Forwarded-Email": "boss@acme.com"}),
		headerReq("garbage", map[string]string{"X-Forwarded-Email": "boss@acme.com"}),
		headerReq("10.0.0.1:1", nil),
		headerReq("10.0.0.1:1", map[string]string{"X-Forwarded-Email": strings.Repeat("a", maxHeaderValueBytes+1)}),
	} {
		if _, err := a.Authenticate(nil, r); !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("%s: err = %v", r.RemoteAddr, err)
		}
	}
}

func TestHeaderModeEasyAuth(t *testing.T) {
	cfg := baseConfig("header")
	cfg.Server.TrustedProxies = []string{"0.0.0.0/0"}
	cfg.Auth.Header.EmailHeader = "X-MS-CLIENT-PRINCIPAL-NAME"
	a := newAuth(t, cfg, Options{})
	principal := map[string]any{
		"auth_typ": "aad", "name_typ": "name", "role_typ": "http://schemas.microsoft.com/ws/2008/06/identity/claims/role",
		"claims": []map[string]string{
			{"typ": "name", "val": "Frank Azure"},
			{"typ": "groups", "val": "00000000-aaaa"},
			{"typ": "http://schemas.microsoft.com/ws/2008/06/identity/claims/role", "val": "Backyard.Admin"},
			{"typ": "preferred_username", "val": "frank@acme.com"},
		},
	}
	pj, _ := json.Marshal(principal)
	u := mustAuth(t, a, headerReq("203.0.113.5:1", map[string]string{
		"X-MS-CLIENT-PRINCIPAL-NAME": "frank@acme.com",
		"X-MS-CLIENT-PRINCIPAL":      base64.StdEncoding.EncodeToString(pj),
	}))
	if u.ID != "frank@acme.com" || u.Name != "Frank Azure" || !u.InGroup("00000000-aaaa") || !u.InGroup("Backyard.Admin") {
		t.Fatalf("user = %+v", u)
	}
	// Broken principal is ignored.
	u = mustAuth(t, a, headerReq("203.0.113.5:1", map[string]string{
		"X-MS-CLIENT-PRINCIPAL-NAME": "frank@acme.com", "X-MS-CLIENT-PRINCIPAL": "%%%",
	}))
	if u.Name != "frank" {
		t.Fatalf("user = %+v", u)
	}
}

type headerJWTEnv struct {
	a     *authenticator
	key   *rsa.PrivateKey
	clock *fakeClock
}

func newHeaderJWTEnv(t *testing.T, trusted []string) *headerJWTEnv {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	jwks := newJWKSServer(t, rsaJWK(&key.PublicKey, "cf-1"))
	clock := newClock()
	cfg := baseConfig("header")
	cfg.Server.TrustedProxies = trusted
	cfg.Auth.Header.JWTHeader = "Cf-Access-Jwt-Assertion"
	cfg.Auth.Header.GroupsHeader = "X-Groups"
	cfg.Auth.Header.JWKSURL = jwks.URL
	cfg.Auth.Header.Issuer = "https://acme.cloudflareaccess.com"
	cfg.Auth.Header.Audiences = []string{"aud-tag-1"}
	return &headerJWTEnv{a: newAuth(t, cfg, Options{Now: clock.Now}), key: key, clock: clock}
}

func (e *headerJWTEnv) jwt(t *testing.T, mutate func(map[string]any)) string {
	now := e.clock.Now()
	c := map[string]any{"iss": "https://acme.cloudflareaccess.com", "aud": []any{"aud-tag-1"}, "email": "gina@acme.com",
		"sub": "cf-sub", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "type": "app"}
	if mutate != nil {
		mutate(c)
	}
	return signJWT(t, jwt.SigningMethodRS256, e.key, "cf-1", c)
}

func TestHeaderModeJWT(t *testing.T) {
	e := newHeaderJWTEnv(t, nil)
	tok := e.jwt(t, nil)
	// Any peer when trusted_proxies is empty: the JWT is the trust anchor.
	u := mustAuth(t, e.a, headerReq("198.51.100.7:1", map[string]string{"Cf-Access-Jwt-Assertion": tok}))
	if u.ID != "gina@acme.com" || u.Name != "gina" {
		t.Fatalf("user = %+v", u)
	}
	// "Bearer " prefix tolerated.
	if _, err := e.a.Authenticate(nil, headerReq("198.51.100.7:1", map[string]string{"Cf-Access-Jwt-Assertion": "Bearer " + tok})); err != nil {
		t.Fatal(err)
	}
	// Groups header ignored from untrusted peers.
	u = mustAuth(t, e.a, headerReq("198.51.100.7:1", map[string]string{"Cf-Access-Jwt-Assertion": tok, "X-Groups": "backyard-admins"}))
	if u.Admin || len(u.Groups) != 0 {
		t.Fatalf("groups from untrusted peer: %+v", u)
	}
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	now := e.clock.Now()
	bad := map[string]string{
		"missing":      "",
		"wrong aud":    e.jwt(t, func(c map[string]any) { c["aud"] = []any{"other"} }),
		"wrong iss":    e.jwt(t, func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" }),
		"expired":      e.jwt(t, func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }),
		"other key":    signJWT(t, jwt.SigningMethodRS256, otherKey, "cf-1", map[string]any{"iss": "https://acme.cloudflareaccess.com", "aud": "aud-tag-1", "email": "x@acme.com", "exp": now.Add(time.Hour).Unix()}),
		"hs256":        signJWT(t, jwt.SigningMethodHS256, []byte("k"), "cf-1", map[string]any{"iss": "https://acme.cloudflareaccess.com", "aud": "aud-tag-1", "email": "x@acme.com", "exp": now.Add(time.Hour).Unix()}),
		"email header": "",
	}
	for name, tok := range bad {
		t.Run(name, func(t *testing.T) {
			h := map[string]string{}
			if tok != "" {
				h["Cf-Access-Jwt-Assertion"] = tok
			}
			if name == "email header" {
				h["X-Forwarded-Email"] = "boss@acme.com"
			}
			if _, err := e.a.Authenticate(nil, headerReq("198.51.100.7:1", h)); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestHeaderModeJWTWithTrustedProxies(t *testing.T) {
	e := newHeaderJWTEnv(t, []string{"10.0.0.0/8"})
	tok := e.jwt(t, nil)
	if _, err := e.a.Authenticate(nil, headerReq("198.51.100.7:1", map[string]string{"Cf-Access-Jwt-Assertion": tok})); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("untrusted peer err = %v", err)
	}
	u := mustAuth(t, e.a, headerReq("10.9.9.9:1", map[string]string{"Cf-Access-Jwt-Assertion": tok, "X-Groups": "backyard-admins"}))
	if !u.Admin || !u.InGroup("backyard-admins") {
		t.Fatalf("trusted groups: %+v", u)
	}
}

func TestRemoteIP(t *testing.T) {
	tests := map[string]string{
		"10.0.0.1:80":        "10.0.0.1",
		"[::1]:80":           "::1",
		"10.0.0.1":           "10.0.0.1",
		"[fe80::1%eth0]:443": "fe80::1",
		"":                   "<nil>",
		"host:80":            "<nil>",
	}
	for in, want := range tests {
		if got := remoteIP(in).String(); got != want {
			t.Errorf("remoteIP(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCookieDomainFor(t *testing.T) {
	tests := []struct {
		base, mode, public, want string
	}{
		{"backyard.acme.com", "host", "https://backyard.acme.com", ".backyard.acme.com"},
		{"Backyard.Acme.com.", "", "", ".backyard.acme.com"},
		{"backyard.acme.com", "path", "https://backyard.acme.com", ""},
		{"localhost", "host", "http://localhost:8080", ""},
		{"dev.localhost", "host", "", ""},
		{"127.0.0.1", "host", "http://127.0.0.1:8080", ""},
		{"backyard", "host", "", ""},
		{"lvh.me", "host", "http://lvh.me:8080", ".lvh.me"},
		{"", "host", "", ""},
		{"", "host", "https://backyard.acme.com", ".backyard.acme.com"},
		// Hub inside base_domain: base_domain is used.
		{"acme.com", "host", "https://hub.acme.com", ".acme.com"},
		// Hub outside base_domain: the hub host is used.
		{"apps.acme.com", "host", "https://backyard.acme.com", ".backyard.acme.com"},
	}
	for _, tc := range tests {
		cfg := &config.Config{}
		cfg.Server.BaseDomain, cfg.Server.URLMode, cfg.Server.PublicURL = tc.base, tc.mode, tc.public
		if got := cookieDomainFor(cfg); got != tc.want {
			t.Errorf("cookieDomainFor(%q, %q, %q) = %q, want %q", tc.base, tc.mode, tc.public, got, tc.want)
		}
	}
}

func TestDirectoryErrorTolerated(t *testing.T) {
	e := newIAPEnv(t)
	e.a.dir = failingDir{}
	if u := mustAuth(t, e.a, iapReq(e.assertion(t, nil))); u.ID != "alice@acme.com" {
		t.Fatalf("user = %+v", u)
	}
}

type failingDir struct{}

func (failingDir) Lookup(context.Context, string) (*identity.User, error) {
	return nil, errors.New("down")
}
