// Package auth authenticates requests (SPEC §8): browser sessions via the
// built-in OIDC login (Okta, Entra ID, Google…), identity-aware proxies (Google
// IAP, AWS ALB OIDC, trusted headers), Backyard API tokens, static tokens and
// IdP-issued bearer JWTs (device flow). In mode "none" everyone is an
// anonymous guest identified by a cookie.
package auth

import (
	"errors"
	"net/http"

	"github.com/thasami/backyard/internal/identity"
)

var (
	// ErrUnauthenticated: no (valid) credentials were presented.
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	// ErrForbidden: authenticated but not allowed (domain/group gate).
	ErrForbidden = errors.New("auth: forbidden")
)

// Authenticator resolves the user behind a request.
//
// Authenticate returns (user, nil) on success, (nil, ErrUnauthenticated) when
// the request carries no usable credentials, (nil, ErrForbidden) when the user
// is authenticated but rejected by allowed_domains/allowed_groups, or another
// error for infrastructure failures. Bearer tokens (API, static, IdP JWT) are
// accepted in every mode. In mode "none" Authenticate never fails: it returns
// an anonymous user and, when w is non-nil, sets the backyard_anon cookie.
type Authenticator interface {
	Authenticate(w http.ResponseWriter, r *http.Request) (*identity.User, error)
	// LoginURL returns the URL a browser should be redirected to in order to
	// log in and come back to returnTo, or "" when interactive login is not
	// handled by Backyard (iap/alb/header/none).
	LoginURL(returnTo string) string
	// Handler serves /__backyard/auth/* on the hub host (login, callback,
	// logout, cli). It expects the full request path.
	Handler() http.Handler
	// Mode returns the normalized auth mode.
	Mode() string
}

// TokenClaims are the claims of a Backyard API token (SPEC §8.3).
type TokenClaims struct {
	Subject string   `json:"sub"`
	Email   string   `json:"email,omitempty"`
	Name    string   `json:"name,omitempty"`
	Groups  []string `json:"groups,omitempty"`
	Admin   bool     `json:"admin,omitempty"`
	Scope   string   `json:"scope"`
}
