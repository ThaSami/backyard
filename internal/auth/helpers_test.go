package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
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

const testSecret = "test-secret-0123456789abcdef0123456789abcdef"

// fakeClock is a settable clock shared by the authenticator and mock servers.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Now().Truncate(time.Second)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// baseConfig returns a defaulted config for mode on https://backyard.test.
func baseConfig(mode string) *config.Config {
	cfg := &config.Config{}
	cfg.Server.BaseDomain = "backyard.test"
	cfg.Server.PublicURL = "https://backyard.test"
	cfg.Server.Secret = testSecret
	cfg.Auth.Mode = mode
	cfg.Auth.Admins = []string{"boss@acme.com"}
	cfg.Auth.AdminGroups = []string{"backyard-admins"}
	cfg.ApplyDefaults()
	return cfg
}

// newAuth builds an authenticator or fails the test.
func newAuth(t *testing.T, cfg *config.Config, opts Options) *authenticator {
	t.Helper()
	a, err := New(context.Background(), cfg, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a.(*authenticator)
}

// req builds a request for an absolute URL with a remote address and cookies.
func req(method, target string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "192.0.2.10:40000"
	for _, c := range cookies {
		if c != nil {
			r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
		}
	}
	return r
}

// serve runs h on r and returns the response.
func serve(h http.Handler, r *http.Request) *http.Response {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Result()
}

// body reads a response body.
func body(t *testing.T, res *http.Response) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// cookieNamed returns the last Set-Cookie with name, or nil.
func cookieNamed(res *http.Response, name string) *http.Cookie {
	var found *http.Cookie
	for _, c := range res.Cookies() {
		if c.Name == name {
			found = c
		}
	}
	return found
}

// mustAuth authenticates r and fails the test on error.
func mustAuth(t *testing.T, a Authenticator, r *http.Request) *identity.User {
	t.Helper()
	u, err := a.Authenticate(httptest.NewRecorder(), r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return u
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// rsaJWK renders an RSA public key as a JWK.
func rsaJWK(pub *rsa.PublicKey, kid string) map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// ecJWK renders a P-256 public key as a JWK.
func ecJWK(t *testing.T, pub *ecdsa.PublicKey, kid string) map[string]any {
	t.Helper()
	raw, err := pub.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"kty": "EC", "crv": "P-256", "kid": kid, "alg": "ES256", "use": "sig",
		"x": b64(raw[1:33]), "y": b64(raw[33:65]),
	}
}

// jwksServer serves a JWKS document and counts requests.
type jwksServer struct {
	*httptest.Server
	mu   sync.Mutex
	keys []map[string]any
	hits int
}

func newJWKSServer(t *testing.T, keys ...map[string]any) *jwksServer {
	t.Helper()
	s := &jwksServer{keys: keys}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		doc := map[string]any{"keys": s.keys}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(s.Close)
	return s
}

// signJWT signs claims with method and key, setting kid.
func signJWT(t *testing.T, method jwt.SigningMethod, key any, kid string, claims map[string]any) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, jwt.MapClaims(claims))
	if kid != "" {
		tok.Header["kid"] = kid
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mapDirectory is an identity.Directory backed by a map.
type mapDirectory map[string]*identity.User

func (m mapDirectory) Lookup(_ context.Context, email string) (*identity.User, error) {
	return m[strings.ToLower(email)].Clone(), nil
}
