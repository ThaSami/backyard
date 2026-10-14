package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// decodeSegment decodes a JWT segment tolerantly: padding is optional and the
// standard base64 alphabet is accepted besides the URL-safe one (AWS ALB
// emits padded segments).
func decodeSegment(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	if strings.ContainsAny(s, "+/") {
		s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	}
	return base64.RawURLEncoding.DecodeString(s)
}

// peekIssuer returns the unverified "iss" claim of a JWT payload segment. It
// is only used to route a token to the right verifier.
func peekIssuer(payloadSeg string) (string, error) {
	b, err := decodeSegment(payloadSeg)
	if err != nil {
		return "", err
	}
	var c struct {
		Iss any `json:"iss"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return "", err
	}
	s, ok := c.Iss.(string)
	if !ok {
		return "", errors.New("no issuer")
	}
	return s, nil
}

// decodeClaims unmarshals a JSON claim set preserving numbers as float64.
func decodeClaims(b []byte) (map[string]any, error) {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, errors.New("claims are not a JSON object")
	}
	return m, nil
}

// randomString returns n random bytes, base64url encoded.
func randomString(n int) string {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error (it aborts the program if the
	// system's random source fails).
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// equalConstantTime compares two strings in constant time (for equal lengths).
func equalConstantTime(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// anyIn reports whether any of values is in set.
func anyIn(values []string, set map[string]bool) bool {
	for _, v := range values {
		if set[v] {
			return true
		}
	}
	return false
}

// audienceMatches reports whether any of the token's audiences, or its
// authorized party (azp) or client id (cid, client_id) claim, is accepted.
func audienceMatches(aud []string, claims map[string]any, accepted map[string]bool) bool {
	if anyIn(aud, accepted) {
		return true
	}
	for _, k := range []string{"azp", "cid", "client_id"} {
		if s, ok := claims[k].(string); ok && accepted[s] {
			return true
		}
	}
	return false
}

// stringSet builds a set of the non-empty trimmed values.
func stringSet(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			m[v] = true
		}
	}
	return m
}
