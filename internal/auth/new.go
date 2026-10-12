package auth

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
)

// Options are optional dependencies for authenticators.
type Options struct {
	// Directory enriches users after claim mapping (optional).
	Directory identity.Directory
	// HTTPClient is used for OIDC discovery and JWKS/key fetches (default http.DefaultClient).
	HTTPClient *http.Client
	// Logger (default slog.Default()).
	Logger *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// New builds the authenticator for cfg.Auth.Mode. For mode "oidc" it performs
// OIDC discovery against the issuer (ctx bounds that call).
// STUB: replaced by the auth implementation.
func New(ctx context.Context, cfg *config.Config, opts Options) (Authenticator, error) {
	panic("auth: not implemented")
}

// NewBearer builds a bearer-only authenticator for the standalone deploy
// gateway: it accepts Backyard API tokens, static tokens and (if
// cfg.Auth.OIDC.Issuer is set) IdP-issued JWTs — never cookies or proxy
// headers. Handler() serves nothing and LoginURL returns "".
// STUB: replaced by the auth implementation.
func NewBearer(ctx context.Context, cfg *config.Config, opts Options) (Authenticator, error) {
	panic("auth: not implemented")
}

// Tokens mints and verifies Backyard API tokens (SPEC §8.3).
type Tokens struct {
	key []byte
	ttl time.Duration
	now func() time.Time
}

// NewTokens returns a token issuer/verifier keyed from secret.
// STUB: replaced by the auth implementation.
func NewTokens(secret string, defaultTTL time.Duration) *Tokens {
	panic("auth: not implemented")
}

// Issue mints a token for u with scope "api". ttl <= 0 uses the default TTL.
// STUB: replaced by the auth implementation.
func (t *Tokens) Issue(u *identity.User, ttl time.Duration) (token string, expiresAt time.Time, err error) {
	panic("auth: not implemented")
}

// Verify validates a token and returns the user it represents.
// STUB: replaced by the auth implementation.
func (t *Tokens) Verify(token string) (*identity.User, error) {
	panic("auth: not implemented")
}
