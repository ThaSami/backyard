package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"
)

func TestJWKSKeySetThrottling(t *testing.T) {
	clock := newClock()
	k1, _ := rsa.GenerateKey(rand.Reader, 2048)
	k2, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := newJWKSServer(t, rsaJWK(&k1.PublicKey, "k1"))
	ks := newJWKSKeySet(srv.URL, srv.Client(), clock.Now, slog.Default())
	v := oidc.NewVerifier("https://issuer.example", ks, &oidc.Config{SkipClientIDCheck: true, Now: clock.Now})
	ctx := context.Background()
	mint := func(key *rsa.PrivateKey, kid string) string {
		now := clock.Now()
		return signJWT(t, jwt.SigningMethodRS256, key, kid, map[string]any{
			"iss": "https://issuer.example", "sub": "s", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})
	}
	hits := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return srv.hits
	}

	if _, err := v.Verify(ctx, mint(k1, "k1")); err != nil {
		t.Fatal(err)
	}
	if hits() != 1 {
		t.Fatalf("hits = %d", hits())
	}
	// A storm of forged tokens (known kid, wrong key) and unknown kids causes
	// at most one download per jwksMinRefresh.
	for range 50 {
		if _, err := v.Verify(ctx, mint(k2, "k1")); err == nil {
			t.Fatal("forged token accepted")
		}
		if _, err := v.Verify(ctx, mint(k2, "random-kid")); err == nil {
			t.Fatal("unknown kid accepted")
		}
	}
	if hits() != 1 {
		t.Fatalf("hits after storm = %d, want 1 (inside the refresh window)", hits())
	}
	clock.Advance(jwksMinRefresh + time.Second)
	for range 50 {
		_, _ = v.Verify(ctx, mint(k2, "random-kid"))
	}
	if hits() != 2 {
		t.Fatalf("hits after second storm = %d, want 2 (one per window)", hits())
	}

	// Rotation: k2 is published; it is picked up once the window has passed.
	srv.mu.Lock()
	srv.keys = append(srv.keys, rsaJWK(&k2.PublicKey, "k2"))
	srv.mu.Unlock()
	if _, err := v.Verify(ctx, mint(k2, "k2")); err == nil {
		t.Fatal("new key used before the refresh window elapsed")
	}
	clock.Advance(jwksMinRefresh + time.Second)
	if _, err := v.Verify(ctx, mint(k2, "k2")); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	// Tokens without kid try every key.
	if _, err := v.Verify(ctx, mint(k2, "")); err != nil {
		t.Fatalf("no kid: %v", err)
	}

	// A stale set is refreshed; a failed refresh keeps the old keys.
	before := hits()
	srv.mu.Lock()
	srv.keys = nil // the refresh will find no usable keys
	srv.mu.Unlock()
	clock.Advance(jwksMaxAge + time.Second)
	if _, err := v.Verify(ctx, mint(k1, "k1")); err != nil {
		t.Fatalf("stale set: %v", err)
	}
	ks.bg.Wait()
	if hits() != before+1 {
		t.Fatalf("stale set not refreshed (%d → %d)", before, hits())
	}
	if _, err := v.Verify(ctx, mint(k1, "k1")); err != nil {
		t.Fatalf("after failed refresh: %v", err)
	}
}

func TestJWKSKeySetStaleRefreshDoesNotBlock(t *testing.T) {
	clock := newClock()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	release := make(chan struct{})
	var slow atomic.Bool
	jwks := newJWKSServer(t, rsaJWK(&key.PublicKey, "k1"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if slow.Load() {
			<-release // a hanging key server
		}
		jwks.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	ks := newJWKSKeySet(srv.URL, srv.Client(), clock.Now, slog.Default())
	v := oidc.NewVerifier("https://issuer.example", ks, &oidc.Config{SkipClientIDCheck: true, Now: clock.Now})
	mint := func() string {
		now := clock.Now()
		return signJWT(t, jwt.SigningMethodRS256, key, "k1", map[string]any{
			"iss": "https://issuer.example", "sub": "s", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})
	}
	if _, err := v.Verify(context.Background(), mint()); err != nil {
		t.Fatal(err)
	}
	slow.Store(true)
	clock.Advance(jwksMaxAge + time.Second)
	done := make(chan error, 1)
	go func() {
		// Several requests while the background refresh hangs.
		for range 5 {
			if _, err := v.Verify(context.Background(), mint()); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verification blocked on the stale-set refresh")
	}
	if !ks.refreshing.Load() {
		t.Fatal("no background refresh running")
	}
}

func TestJWKSKeySetUnreachable(t *testing.T) {
	clock := newClock()
	ks := newJWKSKeySet("http://127.0.0.1:1/jwks", &http.Client{Timeout: time.Second}, clock.Now, slog.Default())
	if _, err := ks.VerifySignature(context.Background(), "e30.e30.sig"); err == nil {
		t.Fatal("expected error")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()
	ks = newJWKSKeySet(bad.URL, bad.Client(), clock.Now, slog.Default())
	if _, err := ks.fetch(context.Background()); err == nil {
		t.Fatal("expected status error")
	}
}

func TestParseJWKS(t *testing.T) {
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	weak, _ := rsa.GenerateKey(rand.Reader, 1024)
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	edPub, _, _ := ed25519.GenerateKey(rand.Reader)
	p384Raw, _ := p384.PublicKey.Bytes()

	enc := rsaJWK(&rsaKey.PublicKey, "enc")
	enc["use"] = "enc"
	evenE := rsaJWK(&rsaKey.PublicKey, "even")
	evenE["e"] = b64(big.NewInt(4).Bytes())
	badEC := ecJWK(t, &p256.PublicKey, "bad-ec")
	badEC["x"] = b64([]byte("short"))
	offCurve := ecJWK(t, &p256.PublicKey, "off")
	offCurve["y"] = b64(make([]byte, 32))
	keys := []any{
		rsaJWK(&rsaKey.PublicKey, "rsa"),
		ecJWK(t, &p256.PublicKey, "p256"),
		map[string]any{"kty": "EC", "crv": "P-384", "kid": "p384", "x": b64(p384Raw[1:49]), "y": b64(p384Raw[49:])},
		map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "ed", "x": b64(edPub)},
		rsaJWK(&weak.PublicKey, "weak"),
		enc, evenE, badEC, offCurve,
		map[string]any{"kty": "OKP", "crv": "X25519", "kid": "x", "x": b64(make([]byte, 32))},
		map[string]any{"kty": "oct", "kid": "sym", "k": "c2VjcmV0"},
		map[string]any{"kty": "EC", "crv": "P-192", "kid": "p192"},
		"not an object",
	}
	b, _ := json.Marshal(map[string]any{"keys": keys})
	got, err := parseJWKS(b)
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, k := range got {
		kids = append(kids, k.kid)
	}
	want := []string{"rsa", "p256", "p384", "ed"}
	if len(kids) != len(want) {
		t.Fatalf("kids = %v, want %v", kids, want)
	}
	for i := range want {
		if kids[i] != want[i] {
			t.Fatalf("kids = %v, want %v", kids, want)
		}
	}
	if _, err := parseJWKS([]byte("{")); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestJWTKeyID(t *testing.T) {
	tests := map[string]string{
		b64([]byte(`{"kid":"abc","alg":"RS256"}`)) + ".e30.x": "abc",
		b64([]byte(`{"alg":"RS256"}`)) + ".e30.x":             "",
		"nodots":                      "",
		"!!!.e30.x":                   "",
		b64([]byte(`[1]`)) + ".e30.x": "",
	}
	for in, want := range tests {
		if got := jwtKeyID(in); got != want {
			t.Errorf("jwtKeyID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHeaderModeEdDSA(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	jwks := newJWKSServer(t, map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "ed-1", "x": b64(pub)})
	clock := newClock()
	cfg := baseConfig("header")
	cfg.Auth.Header.JWTHeader = "X-Pomerium-Jwt-Assertion"
	cfg.Auth.Header.JWKSURL = jwks.URL
	a := newAuth(t, cfg, Options{Now: clock.Now})
	now := clock.Now()
	tok := signJWT(t, jwt.SigningMethodEdDSA, priv, "ed-1", map[string]any{
		"iss": "https://authenticate.example", "aud": "backyard.test", "email": "hal@acme.com",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	r := req("GET", "https://backyard.test/")
	r.Header.Set("X-Pomerium-Jwt-Assertion", tok)
	if u := mustAuth(t, a, r); u.ID != "hal@acme.com" {
		t.Fatalf("user = %+v", u)
	}
	// No issuer/audience configured: any issuer accepted, but the signature still matters.
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	r.Header.Set("X-Pomerium-Jwt-Assertion", signJWT(t, jwt.SigningMethodEdDSA, otherPriv, "ed-1", map[string]any{
		"email": "hal@acme.com", "exp": now.Add(time.Hour).Unix()}))
	if _, err := a.Authenticate(nil, r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}
