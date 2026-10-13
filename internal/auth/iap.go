package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/thasami/backyard/internal/identity"
)

const (
	// IAPHeader carries the signed IAP assertion.
	IAPHeader = "X-Goog-IAP-JWT-Assertion"
	// IAPIssuer is the "iss" of IAP assertions.
	IAPIssuer = "https://cloud.google.com/iap"
	// DefaultIAPJWKSURL publishes IAP's ES256 signing keys.
	DefaultIAPJWKSURL = "https://www.gstatic.com/iap/verify/public_key-jwk"
	// clockSkew tolerates small clock differences for iat/exp checks.
	clockSkew = 30 * time.Second
	// googleAccountsPrefix prefixes email/sub values of Google identities.
	googleAccountsPrefix = "accounts.google.com:"
)

// iapVerifier verifies Google Cloud IAP assertions (SPEC §8.2).
type iapVerifier struct {
	verifier  *oidc.IDTokenVerifier
	keys      *jwksKeySet
	audiences map[string]bool
	now       func() time.Time
}

func (a *authenticator) newIAPVerifier() (*iapVerifier, error) {
	c := a.cfg.Auth.IAP
	aud := stringSet(append([]string{c.Audience}, c.AdditionalAudiences...)...)
	if strings.TrimSpace(c.Audience) == "" {
		return nil, errors.New("auth: auth.iap.audience is required for auth.mode=iap")
	}
	jwks := strings.TrimSpace(c.JWKSURL)
	if jwks == "" {
		jwks = DefaultIAPJWKSURL
	}
	ks := newJWKSKeySet(jwks, a.client, a.now, a.log)
	return &iapVerifier{
		keys: ks,
		verifier: oidc.NewVerifier(IAPIssuer, ks, &oidc.Config{
			SkipClientIDCheck:    true, // checked against several audiences below
			SupportedSigningAlgs: []string{oidc.ES256},
			Now:                  a.now,
		}),
		audiences: aud,
		now:       a.now,
	}, nil
}

// verify validates an IAP assertion and returns its identity claims. gcip
// (Identity Platform) claims, when present, take precedence, and the
// "accounts.google.com:" prefix is stripped from email and sub. When the
// signing keys cannot be downloaded the error wraps errKeysUnavailable.
func (v *iapVerifier) verify(ctx context.Context, raw string) (map[string]any, error) {
	tok, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		if kerr := v.keys.unavailable(raw); kerr != nil {
			return nil, kerr
		}
		return nil, err
	}
	if !anyIn(tok.Audience, v.audiences) {
		return nil, fmt.Errorf("unexpected audience %q", tok.Audience)
	}
	if tok.IssuedAt.IsZero() || tok.IssuedAt.After(v.now().Add(clockSkew)) {
		return nil, errors.New("assertion issued in the future or without iat")
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	if g, ok := claims["gcip"].(map[string]any); ok {
		for k, val := range g {
			switch k {
			case "iss", "aud", "exp", "iat", "nbf", "auth_time":
			default:
				if val != nil {
					claims[k] = val
				}
			}
		}
	}
	for _, k := range []string{"email", "sub"} {
		if s, ok := claims[k].(string); ok {
			claims[k] = strings.TrimPrefix(s, googleAccountsPrefix)
		}
	}
	return claims, nil
}

// authenticateIAP authenticates with the IAP assertion header.
func (a *authenticator) authenticateIAP(r *http.Request) (*identity.User, error) {
	raw := strings.TrimSpace(r.Header.Get(IAPHeader))
	if raw == "" {
		return nil, fmt.Errorf("%w: no %s header", ErrUnauthenticated, IAPHeader)
	}
	claims, err := a.iap.verify(r.Context(), raw)
	if errors.Is(err, errKeysUnavailable) {
		return nil, fmt.Errorf("auth: cannot verify IAP assertion: %w", err)
	}
	if err != nil {
		a.log.Warn("auth: invalid IAP assertion", "remote", r.RemoteAddr, "error", err)
		return nil, fmt.Errorf("%w: invalid IAP assertion: %w", ErrUnauthenticated, err)
	}
	return a.finalize(r.Context(), a.mapClaims(claims), srcIdP)
}
