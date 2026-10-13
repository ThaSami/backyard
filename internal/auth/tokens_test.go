package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

func TestTokensRoundTrip(t *testing.T) {
	clock := newClock()
	tk := NewTokens(testSecret, 0)
	tk.now = clock.Now
	u := &identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice", Groups: []string{"eng"}, Admin: true}
	tok, exp, err := tk.Issue(u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := clock.Now().Add(DefaultTokenTTL).UTC(); !exp.Equal(want) {
		t.Fatalf("exp = %v, want %v", exp, want)
	}
	got, err := tk.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != u.ID || got.Email != u.Email || got.Name != "Alice" || !got.Admin || !got.InGroup("eng") || got.Anonymous {
		t.Fatalf("user = %+v", got)
	}

	// Claims per SPEC §8.3.
	parts := strings.Split(tok, ".")
	payload, _ := decodeSegment(parts[1])
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"iss", "sub", "email", "name", "groups", "admin", "scope", "jti", "iat", "exp"} {
		if _, ok := claims[k]; !ok {
			t.Errorf("claim %q missing: %v", k, claims)
		}
	}
	if claims["iss"] != "backyard" || claims["scope"] != "api" || len(claims["jti"].(string)) != 32 {
		t.Fatalf("claims = %v", claims)
	}
	header, _ := decodeSegment(parts[0])
	if !strings.Contains(string(header), `"HS256"`) {
		t.Fatalf("header = %s", header)
	}

	// Unique jti per token.
	tok2, _, _ := tk.Issue(u, time.Hour)
	if tok2 == tok {
		t.Fatal("tokens not unique")
	}

	// Anonymous subjects.
	at, _, _ := tk.Issue(&identity.User{ID: "anon:0123456789abcdef", Name: "Guest 0123"}, time.Hour)
	if got, _ := tk.Verify(at); got == nil || !got.Anonymous {
		t.Fatalf("anon = %+v", got)
	}
	// Name falls back to the subject.
	nt, _, _ := tk.Issue(&identity.User{ID: "sub:42"}, time.Hour)
	if got, _ := tk.Verify(nt); got == nil || got.Name != "sub:42" {
		t.Fatalf("no-name = %+v", got)
	}
}

func TestTokensIssueErrors(t *testing.T) {
	tk := NewTokens(testSecret, time.Hour)
	if _, _, err := tk.Issue(nil, 0); err == nil {
		t.Fatal("nil user")
	}
	if _, _, err := tk.Issue(&identity.User{Name: "x"}, 0); err == nil {
		t.Fatal("no id")
	}
	var nilTokens *Tokens
	if _, _, err := nilTokens.Issue(&identity.User{ID: "a"}, 0); err == nil {
		t.Fatal("nil tokens")
	}
	if _, err := nilTokens.Verify("x"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("nil tokens verify")
	}
}

func TestTokensExpiry(t *testing.T) {
	clock := newClock()
	tk := NewTokens(testSecret, time.Hour)
	tk.now = clock.Now
	tok, _, err := tk.Issue(&identity.User{ID: "a@x.io", Email: "a@x.io"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute + 20*time.Second) // within the 30 s leeway
	if _, err := tk.Verify(tok); err != nil {
		t.Fatalf("within leeway: %v", err)
	}
	clock.Advance(15 * time.Second)
	if _, err := tk.Verify(tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired err = %v", err)
	}
}

func TestTokensRejectForgeries(t *testing.T) {
	clock := newClock()
	tk := NewTokens(testSecret, time.Hour)
	tk.now = clock.Now
	key := keys.Derive(testSecret, keys.APIToken)
	now := clock.Now()
	valid := map[string]any{
		"iss": "backyard", "sub": "mallory@acme.com", "email": "mallory@acme.com", "admin": true,
		"scope": "api", "jti": "x", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range valid {
			m[kk] = vv
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	noneTok, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims(valid)).SignedString(jwt.UnsafeAllowNoneSignatureType)

	cases := map[string]string{
		"control (valid)":     signJWT(t, jwt.SigningMethodHS256, key, "", valid),
		"wrong key":           signJWT(t, jwt.SigningMethodHS256, keys.Derive("other-secret-0123456789abcdef0123456789", keys.APIToken), "", valid),
		"session key":         signJWT(t, jwt.SigningMethodHS256, keys.Derive(testSecret, keys.Session), "", valid),
		"raw secret":          signJWT(t, jwt.SigningMethodHS256, []byte(testSecret), "", valid),
		"alg none":            noneTok,
		"RS256":               signJWT(t, jwt.SigningMethodRS256, rsaKey, "", valid),
		"ES256":               signJWT(t, jwt.SigningMethodES256, ecKey, "", valid),
		"HS512":               signJWT(t, jwt.SigningMethodHS512, key, "", valid),
		"wrong issuer":        signJWT(t, jwt.SigningMethodHS256, key, "", with("iss", "evil")),
		"no issuer":           signJWT(t, jwt.SigningMethodHS256, key, "", with("iss", nil)),
		"wrong scope":         signJWT(t, jwt.SigningMethodHS256, key, "", with("scope", "deploy")),
		"no exp":              signJWT(t, jwt.SigningMethodHS256, key, "", with("exp", nil)),
		"no iat":              signJWT(t, jwt.SigningMethodHS256, key, "", with("iat", nil)),
		"future iat":          signJWT(t, jwt.SigningMethodHS256, key, "", with("iat", now.Add(time.Hour).Unix())),
		"expired":             signJWT(t, jwt.SigningMethodHS256, key, "", with("exp", now.Add(-time.Hour).Unix())),
		"no subject":          signJWT(t, jwt.SigningMethodHS256, key, "", with("sub", nil)),
		"blank subject":       signJWT(t, jwt.SigningMethodHS256, key, "", with("sub", "  ")),
		"garbage":             "not.a.jwt",
		"empty":               "",
		"huge":                strings.Repeat("a", maxTokenBytes+1),
		"tampered payload":    tamperPayload(signJWT(t, jwt.SigningMethodHS256, key, "", valid)),
		"two segments":        "eyJhbGciOiJIUzI1NiJ9.eyJpc3MiOiJiYWNreWFyZCJ9",
		"signature stripped":  strings.Join(strings.Split(signJWT(t, jwt.SigningMethodHS256, key, "", valid), ".")[:2], ".") + ".",
		"sub not string type": signJWT(t, jwt.SigningMethodHS256, key, "", with("sub", 42)),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tk.Verify(tok)
			if name == "control (valid)" {
				if err != nil {
					t.Fatalf("control: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func tamperPayload(tok string) string {
	parts := strings.Split(tok, ".")
	b, _ := decodeSegment(parts[1])
	b = []byte(strings.Replace(string(b), "mallory", "admin00", 1))
	parts[1] = b64(b)
	return strings.Join(parts, ".")
}

func TestTokensIssueStaysVerifiable(t *testing.T) {
	tk := NewTokens(testSecret, time.Hour)
	groups := make([]string, 2000)
	for i := range groups {
		groups[i] = "group-with-a-fairly-long-name-" + strings.Repeat("x", 20) + "-" + string(rune('a'+i%26)) + strings.Repeat("y", i%7)
	}
	groups[0] = "backyard-admins"
	u := &identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: strings.Repeat("Ä", 10000), Groups: groups}
	tok, _, err := tk.Issue(u, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) > maxTokenBytes {
		t.Fatalf("token is %d bytes, above the %d Verify accepts", len(tok), maxTokenBytes)
	}
	got, err := tk.Verify(tok)
	if err != nil {
		t.Fatalf("Verify of an issued token: %v", err)
	}
	if n := len([]rune(got.Name)); n != maxTokenNameRunes {
		t.Fatalf("name has %d runes, want %d", n, maxTokenNameRunes)
	}
	if len(got.Groups) == 0 || len(got.Groups) >= len(groups) || got.Groups[0] != "backyard-admins" {
		t.Fatalf("groups = %d (first %q)", len(got.Groups), got.Groups[0])
	}
	if len(u.Groups) != 2000 || len([]rune(u.Name)) != 10000 {
		t.Fatal("Issue modified the caller's user")
	}
	// An identity that cannot fit at all is an error, not an unusable token.
	if _, _, err := tk.Issue(&identity.User{ID: strings.Repeat("a", maxTokenBytes)}, 0); err == nil {
		t.Fatal("oversized subject issued")
	}
}
