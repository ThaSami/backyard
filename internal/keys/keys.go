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
	// CLIConfirm signs the confirmation form of a cross-site CLI login
	// (/__backyard/auth/cli). The value never leaves one deployment, so the
	// Cloudflare edition need not derive it identically.
	CLIConfirm = "backyard/cli-confirm/v1"
	// The remote MCP connector's authorization server (SPEC §8.4). These
	// values reference per-deployment grant state, so they never
	// interoperate between deployments or editions.
	MCPClient  = "backyard/mcp-client/v1"  // stateless DCR client_ids
	MCPConsent = "backyard/mcp-consent/v1" // consent page CSRF tokens
	MCPCode    = "backyard/mcp-code/v1"    // authorization codes
	MCPRefresh = "backyard/mcp-refresh/v1" // refresh tokens
	MCPAccess  = "backyard/mcp-access/v1"  // access tokens
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
