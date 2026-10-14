package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
)

// Options are optional dependencies for authenticators.
type Options struct {
	// Directory enriches users after claim mapping (optional).
	Directory identity.Directory
	// HTTPClient is used for OIDC discovery and JWKS/key fetches (default: a
	// client with a 15 s timeout; a client without a timeout gets one).
	HTTPClient *http.Client
	// Logger (default slog.Default()).
	Logger *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
	// Docs stores the grants of the MCP connector's authorization server;
	// required by New when mcp.enabled is true (nil for NewBearer).
	Docs docstore.Store
	// MCPHTTPClient replaces the SSRF-guarded client that fetches client
	// metadata documents (tests only). Redirects are never followed and the
	// 5 s timeout applies either way.
	MCPHTTPClient *http.Client
}

// New builds the authenticator for cfg.Auth.Mode. For mode "oidc" it performs
// OIDC discovery against the issuer (ctx bounds that call).
//
// HTTPClient defaults to a client with a 15 s timeout (a client without a
// timeout gets one). When auth.oidc.issuer is configured in a mode other than
// oidc (device flow behind a proxy), discovery for IdP bearer tokens is
// attempted here and, if the IdP is unreachable, retried lazily.
func New(ctx context.Context, cfg *config.Config, opts Options) (Authenticator, error) {
	a, err := newBase(cfg, opts)
	if err != nil {
		return nil, err
	}
	switch a.mode {
	case ModeNone:
		a.anon = newAnonCodec(cfg.Server.Secret)
	case ModeOIDC:
		o := cfg.Auth.OIDC
		if strings.TrimSpace(o.Issuer) == "" || strings.TrimSpace(o.ClientID) == "" {
			return nil, fmt.Errorf("auth: auth.oidc.issuer and auth.oidc.client_id are required for auth.mode=oidc")
		}
		p, err := discover(ctx, a.client, strings.TrimSpace(o.Issuer), o.InsecureSkipIssuerCheck, a.now, a.log)
		if err != nil {
			return nil, err
		}
		if a.oidc, err = a.newOIDCLogin(p); err != nil {
			return nil, err
		}
		if a.sess, err = newSessionCodec(cfg.Server.Secret, a.now); err != nil {
			return nil, err
		}
		if a.idp = a.newIDPBearer(); a.idp != nil {
			a.idp.setProvider(p)
		}
	case ModeIAP:
		if a.iap, err = a.newIAPVerifier(); err != nil {
			return nil, err
		}
	case ModeALB:
		if a.alb, err = a.newALBVerifier(); err != nil {
			return nil, err
		}
	case ModeHeader:
		if a.header, err = a.newHeaderAuth(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("auth: auth.mode must be one of none, oidc, iap, alb, header; got %q", cfg.Auth.Mode)
	}
	if a.mode != ModeOIDC {
		a.initIDPBearer(ctx)
	}
	if cfg.MCP.Enabled {
		if a.mcp, err = newMCP(a, opts); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// NewBearer builds a bearer-only authenticator for the standalone deploy
// gateway: it accepts Backyard API tokens, static tokens and (if
// cfg.Auth.OIDC.Issuer is set) IdP-issued JWTs — never cookies or proxy
// headers. Handler() serves nothing and LoginURL returns "".
//
// IdP discovery is attempted here but never fails construction: when the IdP
// is unreachable it is retried lazily when the first IdP token arrives.
func NewBearer(ctx context.Context, cfg *config.Config, opts Options) (Authenticator, error) {
	a, err := newBase(cfg, opts)
	if err != nil {
		return nil, err
	}
	a.bearerOnly = true
	a.initIDPBearer(ctx)
	return a, nil
}

// initIDPBearer sets up IdP bearer verification with a best-effort eager
// discovery.
func (a *authenticator) initIDPBearer(ctx context.Context) {
	if a.idp = a.newIDPBearer(); a.idp == nil {
		return
	}
	if _, err := a.idp.ensure(ctx); err != nil {
		a.log.Warn("auth: identity provider discovery failed; will retry on demand",
			"issuer", a.idp.issuer, "error", err)
	}
}
