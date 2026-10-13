package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

const (
	// TokenIssuer is the "iss" claim of Backyard API tokens.
	TokenIssuer = "backyard"
	// TokenScope is the "scope" claim of Backyard API tokens.
	TokenScope = "api"
	// DefaultTokenTTL is used when NewTokens is given a non-positive TTL.
	DefaultTokenTTL = 720 * time.Hour
	// tokenLeeway tolerates clock skew when validating exp/iat.
	tokenLeeway = 30 * time.Second
	// maxTokenBytes bounds the size of a token accepted by Verify.
	maxTokenBytes = 16 << 10
	// maxTokenNameRunes bounds the display name carried by a token.
	maxTokenNameRunes = 256
)

// Tokens mints and verifies Backyard API tokens (SPEC §8.3).
//
// Tokens are HS256 JWTs keyed with keys.Derive(server.secret, keys.APIToken)
// carrying {iss:"backyard", sub, email, name, groups, admin, scope:"api",
// jti, iat, exp}. Rotating server.secret revokes every token. A Tokens value
// is immutable and safe for concurrent use.
type Tokens struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// apiTokenClaims is the JWT body: the public TokenClaims plus registered claims.
type apiTokenClaims struct {
	TokenClaims
	Issuer    string           `json:"iss"`
	ID        string           `json:"jti"`
	IssuedAt  *jwt.NumericDate `json:"iat"`
	ExpiresAt *jwt.NumericDate `json:"exp"`
}

func (c *apiTokenClaims) GetExpirationTime() (*jwt.NumericDate, error) { return c.ExpiresAt, nil }
func (c *apiTokenClaims) GetIssuedAt() (*jwt.NumericDate, error)       { return c.IssuedAt, nil }
func (c *apiTokenClaims) GetNotBefore() (*jwt.NumericDate, error)      { return nil, nil }
func (c *apiTokenClaims) GetIssuer() (string, error)                   { return c.Issuer, nil }
func (c *apiTokenClaims) GetSubject() (string, error)                  { return c.Subject, nil }
func (c *apiTokenClaims) GetAudience() (jwt.ClaimStrings, error)       { return nil, nil }

// NewTokens returns a token issuer/verifier keyed from secret.
func NewTokens(secret string, defaultTTL time.Duration) *Tokens {
	if defaultTTL <= 0 {
		defaultTTL = DefaultTokenTTL
	}
	return &Tokens{key: keys.Derive(secret, keys.APIToken), ttl: defaultTTL, now: time.Now}
}

// Issue mints a token for u with scope "api". ttl <= 0 uses the default TTL.
//
// The token always fits the size Verify accepts: an overlong name is
// shortened and, if needed, trailing groups are dropped (callers that care
// which groups survive put the important ones first).
func (t *Tokens) Issue(u *identity.User, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	if t == nil {
		return "", time.Time{}, errors.New("auth: token issuer not configured")
	}
	if u == nil || strings.TrimSpace(u.ID) == "" {
		return "", time.Time{}, errors.New("auth: cannot issue a token for a user without id")
	}
	if ttl <= 0 {
		ttl = t.ttl
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: token id: %w", err)
	}
	now := t.now().Truncate(time.Second)
	expiresAt = now.Add(ttl).UTC()
	claims := &apiTokenClaims{
		TokenClaims: TokenClaims{
			Subject: u.ID,
			Email:   u.Email,
			Name:    truncateRunes(strings.TrimSpace(u.Name), maxTokenNameRunes),
			Groups:  append([]string(nil), u.Groups...),
			Admin:   u.Admin,
			Scope:   TokenScope,
		},
		Issuer:    TokenIssuer,
		ID:        hex.EncodeToString(jti),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	}
	sign := func() (string, error) {
		return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.key)
	}
	if token, err = sign(); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign token: %w", err)
	}
	if len(token) > maxTokenBytes {
		// Keep the largest prefix of groups that fits (binary search).
		all := claims.Groups
		lo, hi := 0, len(all)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			claims.Groups = all[:mid]
			if tok, err := sign(); err == nil && len(tok) <= maxTokenBytes {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		claims.Groups = all[:lo]
		if token, err = sign(); err != nil {
			return "", time.Time{}, fmt.Errorf("auth: sign token: %w", err)
		}
		if len(token) > maxTokenBytes {
			return "", time.Time{}, errors.New("auth: user profile too large for an api token")
		}
	}
	return token, expiresAt, nil
}

// truncateRunes shortens s to at most n runes (no ellipsis).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Verify validates a token and returns the user it represents.
//
// Only HS256 is accepted (no "none", no asymmetric algorithms); iss must be
// "backyard", scope "api", and exp/iat are checked with 30 seconds of
// leeway. Errors wrap ErrUnauthenticated. The returned user carries Admin as
// recorded in the token; subjects starting with "anon:" are anonymous.
func (t *Tokens) Verify(token string) (*identity.User, error) {
	if t == nil {
		return nil, fmt.Errorf("%w: token verifier not configured", ErrUnauthenticated)
	}
	if token == "" || len(token) > maxTokenBytes {
		return nil, fmt.Errorf("%w: malformed api token", ErrUnauthenticated)
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(TokenIssuer),
		jwt.WithLeeway(tokenLeeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(t.now),
	)
	var c apiTokenClaims
	if _, err := parser.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return t.key, nil }); err != nil {
		return nil, fmt.Errorf("%w: invalid api token: %w", ErrUnauthenticated, err)
	}
	if c.Scope != TokenScope {
		return nil, fmt.Errorf("%w: api token has scope %q", ErrUnauthenticated, c.Scope)
	}
	if c.IssuedAt == nil {
		return nil, fmt.Errorf("%w: api token without iat", ErrUnauthenticated)
	}
	sub := strings.TrimSpace(c.Subject)
	if sub == "" {
		return nil, fmt.Errorf("%w: api token without subject", ErrUnauthenticated)
	}
	u := &identity.User{
		ID:        sub,
		Email:     identity.NormalizeEmail(c.Email),
		Name:      strings.TrimSpace(c.Name),
		Groups:    identity.ParseList(c.Groups),
		Admin:     c.Admin,
		Anonymous: strings.HasPrefix(sub, anonPrefix),
	}
	if u.Name == "" {
		u.Name = sub
	}
	return u, nil
}
