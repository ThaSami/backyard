// Package keys derives purpose-specific keys from server.secret so that a
// session key can never be used to forge an API token or an upload URL.
package keys

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// Labels for the derived keys. Every component must use its own label.
const (
	Session    = "backyard/session/v1"
	OAuthState = "backyard/oauth-state/v1"
	APIToken   = "backyard/api-token/v1"
	Deploy     = "backyard/deploy/v1"
	Upload     = "backyard/upload/v1"
	Anon       = "backyard/anon/v1"
)

// Derive returns a 32-byte key for label derived from secret with HKDF-SHA256.
func Derive(secret, label string) []byte {
	k, err := hkdf.Key(sha256.New, []byte(secret), []byte("backyard"), label, 32)
	if err != nil {
		panic(err) // only possible for invalid lengths
	}
	return k
}

// Sign returns base64url(payload) + "." + base64url(HMAC-SHA256(key, payload)).
func Sign(key, payload []byte) string {
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ErrBadSignature is returned by Verify for malformed or forged values.
var ErrBadSignature = errors.New("keys: bad signature")

// Verify checks a value produced by Sign and returns the payload.
func Verify(key []byte, signed string) ([]byte, error) {
	p, s, ok := strings.Cut(signed, ".")
	if !ok {
		return nil, ErrBadSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, ErrBadSignature
	}
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadSignature
	}
	m := hmac.New(sha256.New, key)
	m.Write(payload)
	if !hmac.Equal(sig, m.Sum(nil)) {
		return nil, ErrBadSignature
	}
	return payload, nil
}
