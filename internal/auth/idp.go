package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	// discoveryTimeout bounds one OIDC discovery request.
	discoveryTimeout = 15 * time.Second
	// discoveryRetry is the minimum delay between failed discovery attempts.
	discoveryRetry = 30 * time.Second
	// googleIssuer may appear without its scheme in Google ID tokens (go-oidc
	// accepts that variant for Google only).
	googleIssuer         = "https://accounts.google.com"
	googleIssuerNoScheme = "accounts.google.com"
)

// idpBearer verifies JWTs issued by auth.oidc.issuer and presented as bearer
// tokens (device-flow access/ID tokens). A token is accepted when its
// signature, issuer and expiry verify and its aud, azp or cid claim names
// device_client_id, client_id or one of api_audiences.
type idpBearer struct {
	issuer     string
	skipIssuer bool
	audiences  map[string]bool
	client     *http.Client
	now        func() time.Time
	log        *slog.Logger

	discoverMu sync.Mutex // serializes discovery attempts

	mu       sync.Mutex // guards the fields below
	provider *idpProvider
	verifier *oidc.IDTokenVerifier
	lastErr  error
	lastTry  time.Time
}

// idpProvider is a discovered OpenID provider with a throttled key set
// shared by the login flow and bearer verification.
type idpProvider struct {
	provider *oidc.Provider
	issuer   string
	keys     *jwksKeySet
	algs     []string
}

// verifier returns an ID token verifier using the provider's key set.
func (p *idpProvider) verifier(clientID string, skipIssuer bool, now func() time.Time) *oidc.IDTokenVerifier {
	return oidc.NewVerifier(p.issuer, p.keys, &oidc.Config{
		ClientID:             clientID,
		SkipClientIDCheck:    clientID == "",
		SkipIssuerCheck:      skipIssuer,
		SupportedSigningAlgs: p.algs,
		Now:                  now,
	})
}

// discover performs OIDC discovery against issuer using client.
func discover(ctx context.Context, client *http.Client, issuer string, skipIssuerCheck bool,
	now func() time.Time, log *slog.Logger) (*idpProvider, error) {
	ctx = oidc.ClientContext(ctx, client)
	if skipIssuerCheck {
		ctx = oidc.InsecureIssuerURLContext(ctx, issuer)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, discoveryTimeout)
		defer cancel()
	}
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("auth: oidc discovery for %s: %w", issuer, err)
	}
	var meta struct {
		JWKSURI string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}
	if err := p.Claims(&meta); err != nil {
		return nil, fmt.Errorf("auth: oidc discovery for %s: %w", issuer, err)
	}
	if u, err := url.Parse(meta.JWKSURI); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("auth: oidc discovery for %s: invalid jwks_uri %q", issuer, meta.JWKSURI)
	}
	var algs []string
	for _, a := range meta.Algs {
		if slices.Contains(asymmetricAlgs, a) {
			algs = append(algs, a)
		}
	}
	if len(algs) == 0 {
		algs = []string{oidc.RS256}
	}
	return &idpProvider{
		provider: p,
		issuer:   issuer,
		keys:     newJWKSKeySet(meta.JWKSURI, client, now, log),
		algs:     algs,
	}, nil
}

// currentProvider returns the discovered provider, or nil.
func (b *idpBearer) currentProvider() *idpProvider {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.provider
}

// setProvider installs an already discovered provider.
func (b *idpBearer) setProvider(p *idpProvider) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.provider = p
	// The audience is checked against several values in verify.
	b.verifier = p.verifier("", b.skipIssuer, b.now)
}

// ensure returns the verifier, discovering the provider on first use. Failed
// discoveries are retried at most every discoveryRetry.
func (b *idpBearer) ensure(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	b.mu.Lock()
	v := b.verifier
	b.mu.Unlock()
	if v != nil {
		return v, nil
	}
	b.discoverMu.Lock()
	defer b.discoverMu.Unlock()
	b.mu.Lock()
	if b.verifier != nil {
		v := b.verifier
		b.mu.Unlock()
		return v, nil
	}
	if b.lastErr != nil && b.now().Sub(b.lastTry) < discoveryRetry {
		err := b.lastErr
		b.mu.Unlock()
		return nil, err
	}
	b.lastTry = b.now()
	b.mu.Unlock()

	// Discovery is detached from the request's cancellation so one aborted
	// request does not poison the retry window for everyone else.
	p, err := discover(context.WithoutCancel(ctx), b.client, b.issuer, b.skipIssuer, b.now, b.log)
	if err != nil {
		b.mu.Lock()
		b.lastErr = err
		b.mu.Unlock()
		b.log.Warn("auth: identity provider discovery failed; bearer JWTs from it are rejected until it succeeds",
			"issuer", b.issuer, "error", err)
		return nil, err
	}
	b.setProvider(p)
	b.mu.Lock()
	b.lastErr = nil
	v = b.verifier
	b.mu.Unlock()
	return v, nil
}

// verify validates raw and returns its claims.
//
// Tokens naming another issuer are rejected before any network traffic, so
// arbitrary JWTs cannot trigger discovery. When the provider or its keys
// cannot be reached the error wraps errKeysUnavailable.
func (b *idpBearer) verify(ctx context.Context, raw string) (map[string]any, error) {
	if !b.skipIssuer {
		parts := strings.Split(raw, ".")
		if len(parts) != 3 {
			return nil, errors.New("malformed token")
		}
		iss, _ := peekIssuer(parts[1])
		if iss != b.issuer && !(b.issuer == googleIssuer && iss == googleIssuerNoScheme) {
			return nil, fmt.Errorf("token issued by %q, not by %s", truncate(iss, 128), b.issuer)
		}
	}
	v, err := b.ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errKeysUnavailable, err)
	}
	tok, err := v.Verify(ctx, raw)
	if err != nil {
		if p := b.currentProvider(); p != nil {
			if kerr := p.keys.unavailable(raw); kerr != nil {
				return nil, kerr
			}
		}
		return nil, err
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	if !audienceMatches(tok.Audience, claims, b.audiences) {
		return nil, errors.New("token audience is not accepted")
	}
	return claims, nil
}

// newIDPBearer returns the IdP bearer verifier for cfg.Auth.OIDC, or nil when
// no issuer or no acceptable audience is configured.
func (a *authenticator) newIDPBearer() *idpBearer {
	o := a.cfg.Auth.OIDC
	issuer := strings.TrimSpace(o.Issuer)
	if issuer == "" {
		return nil
	}
	aud := stringSet(append([]string{o.DeviceClientID, o.ClientID}, o.APIAudiences...)...)
	if len(aud) == 0 {
		a.log.Warn("auth: auth.oidc.issuer is set but no client_id, device_client_id or api_audiences; IdP bearer tokens are not accepted")
		return nil
	}
	return &idpBearer{
		issuer:     issuer,
		skipIssuer: o.InsecureSkipIssuerCheck,
		audiences:  aud,
		client:     a.client,
		now:        a.now,
		log:        a.log,
	}
}
