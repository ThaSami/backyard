package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
)

// Normalized auth modes.
const (
	ModeNone   = "none"
	ModeOIDC   = "oidc"
	ModeIAP    = "iap"
	ModeALB    = "alb"
	ModeHeader = "header"
)

// Paths served by Handler.
const (
	PathPrefix   = "/__backyard/auth/"
	LoginPath    = PathPrefix + "login"
	CallbackPath = PathPrefix + "callback"
	LogoutPath   = PathPrefix + "logout"
	CLIPath      = PathPrefix + "cli"
)

// Cookie names.
const (
	SessionCookie = "backyard_session"
	OAuthCookie   = "backyard_oauth"
	AnonCookie    = "backyard_anon"
)

const (
	anonPrefix  = "anon:"
	tokenPrefix = "token:"
	// defaultHTTPTimeout bounds discovery, JWKS and key fetches when the
	// configured client has no timeout of its own.
	defaultHTTPTimeout = 15 * time.Second
	// maxBearerBytes bounds the size of an Authorization bearer token.
	maxBearerBytes = 16 << 10
)

// source tells finalize which post-processing an identity needs.
type source int

const (
	srcIDP      source = iota // IdP / proxy claims or browser session
	srcAPIToken               // Backyard API token
	srcStatic                 // static token from auth.tokens
	srcAnon                   // anonymous guest (mode none)
)

type staticToken struct {
	name  string
	sum   [sha256.Size]byte
	admin bool
}

// authenticator implements Authenticator for every mode.
type authenticator struct {
	cfg        *config.Config
	mode       string
	bearerOnly bool
	log        *slog.Logger
	now        func() time.Time
	client     *http.Client
	dir        identity.Directory

	tokens  *Tokens
	static  []staticToken
	idp     *idpBearer // nil when IdP bearer JWTs are not accepted
	trusted []*net.IPNet

	hub          *url.URL // parsed server.public_url (nil if unset)
	cookieDomain string   // "" = host-only cookies
	secure       bool
	important    map[string]bool // lowercased admin/allowed groups

	oidc   *oidcLogin
	sess   *sessionCodec
	anon   *anonCodec
	iap    *iapVerifier
	alb    *albVerifier
	header *headerAuth
}

var _ Authenticator = (*authenticator)(nil)

// normalizeMode lowercases a mode and resolves the aliases accepted by the
// configuration ("okta", "entra", … → "oidc"; "" → "none").
func normalizeMode(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	switch m {
	case "", "off", "disabled":
		return ModeNone
	case "okta", "entra", "azuread", "azure-ad", "google", "auth0", "keycloak", "openid":
		return ModeOIDC
	}
	return m
}

// newBase builds the mode-independent parts of an authenticator.
func newBase(cfg *config.Config, opts Options) (*authenticator, error) {
	if cfg == nil {
		return nil, errors.New("auth: nil config")
	}
	if len(cfg.Server.Secret) < 32 {
		return nil, errors.New("auth: server.secret must be at least 32 characters")
	}
	a := &authenticator{
		cfg:  cfg,
		mode: normalizeMode(cfg.Auth.Mode),
		log:  opts.Logger,
		now:  opts.Now,
		dir:  opts.Directory,
	}
	if a.log == nil {
		a.log = slog.Default()
	}
	if a.now == nil {
		a.now = time.Now
	}
	a.client = boundedClient(opts.HTTPClient)
	a.tokens = NewTokens(cfg.Server.Secret, cfg.Auth.TokenTTL.D())
	a.tokens.now = a.now

	for i, st := range cfg.Auth.Tokens {
		name := strings.TrimSpace(st.Name)
		raw, err := hex.DecodeString(strings.TrimSpace(st.SHA256))
		if name == "" || err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("auth: auth.tokens[%d]: name and a 64-char hex sha256 are required", i)
		}
		t := staticToken{name: name, admin: st.Admin}
		copy(t.sum[:], raw)
		a.static = append(a.static, t)
	}
	for _, p := range cfg.Server.TrustedProxies {
		n, err := config.ParseCIDROrIP(p)
		if err != nil {
			return nil, fmt.Errorf("auth: server.trusted_proxies: %w", err)
		}
		a.trusted = append(a.trusted, n)
	}
	if pu := strings.TrimSpace(cfg.Server.PublicURL); pu != "" {
		u, err := url.Parse(strings.TrimSuffix(pu, "/"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("auth: server.public_url must be an absolute http(s) URL, got %q", pu)
		}
		u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""
		u.Host = strings.ToLower(u.Host)
		a.hub = u
	}
	a.secure = a.hub != nil && a.hub.Scheme == "https"
	a.cookieDomain = cookieDomainFor(cfg)
	a.important = make(map[string]bool)
	for _, g := range append(append([]string(nil), cfg.Auth.AdminGroups...), cfg.Auth.AllowedGroups...) {
		a.important[strings.ToLower(strings.TrimSpace(g))] = true
	}
	return a, nil
}

// boundedClient returns c (or a default client) with a request timeout.
func boundedClient(c *http.Client) *http.Client {
	if c == nil {
		return &http.Client{Timeout: defaultHTTPTimeout}
	}
	if c.Timeout > 0 {
		return c
	}
	cp := *c
	cp.Timeout = defaultHTTPTimeout
	return &cp
}

// cookieDomainFor returns the Domain attribute for cookies shared between the
// hub and site hosts: ".<base_domain>" in host mode, "" (host-only) in path
// mode and for localhost-style, IP or single-label base domains, which
// browsers refuse as cookie domains. When the public URL's host is not
// within base_domain (so the hub could not set such a cookie), the hub host
// is used instead.
func cookieDomainFor(cfg *config.Config) string {
	if strings.EqualFold(cfg.Server.URLMode, "path") {
		return ""
	}
	d := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.Server.BaseDomain), "."))
	if h, _, err := net.SplitHostPort(d); err == nil {
		d = h
	}
	if u, err := url.Parse(strings.TrimSpace(cfg.Server.PublicURL)); err == nil && u.Hostname() != "" {
		hub := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if d == "" || (hub != d && !strings.HasSuffix(hub, "."+d)) {
			d = hub
		}
	}
	if d == "" || !strings.Contains(d, ".") || d == "localhost" || strings.HasSuffix(d, ".localhost") ||
		net.ParseIP(strings.Trim(d, "[]")) != nil {
		return ""
	}
	return "." + d
}

// Mode implements Authenticator.
func (a *authenticator) Mode() string { return a.mode }

// LoginURL implements Authenticator.
func (a *authenticator) LoginURL(returnTo string) string {
	if a.oidc == nil || a.hub == nil {
		return ""
	}
	u := a.hub.String() + LoginPath
	if returnTo != "" {
		u += "?rd=" + url.QueryEscape(returnTo)
	}
	return u
}

// hubURL returns the hub's root URL with a trailing slash.
func (a *authenticator) hubURL() string {
	if a.hub == nil {
		return "/"
	}
	return a.hub.String() + "/"
}

// Authenticate implements Authenticator.
//
// A bearer token is tried first. When it is invalid the mode's own
// credentials (session cookie, proxy assertion/headers) are tried next — so a
// proxy that forwards its own Authorization header (IAP programmatic access,
// oauth2-proxy, …) does not break proxy authentication — and the bearer error
// is reported only if those are absent too (that includes infrastructure
// errors, e.g. an unreachable IdP, which must not block a request the proxy
// already authenticated). A valid token of a user rejected by the gates is
// final (ErrForbidden). In mode "none" an invalid bearer token is always an
// error: silently downgrading it to a guest would hide an expired CLI token
// (and the Cloudflare edition rejects it the same way).
func (a *authenticator) Authenticate(w http.ResponseWriter, r *http.Request) (*identity.User, error) {
	var bearerErr error
	if tok, ok := bearerToken(r); ok {
		u, err := a.authenticateBearer(r.Context(), tok)
		if err == nil {
			return u, nil
		}
		if errors.Is(err, ErrForbidden) {
			return nil, err
		}
		bearerErr = err
		a.log.Debug("auth: bearer token rejected", "error", err)
	}
	if bearerErr != nil && a.mode == ModeNone {
		return nil, bearerErr
	}
	if a.bearerOnly {
		if bearerErr != nil {
			return nil, bearerErr
		}
		return nil, fmt.Errorf("%w: no bearer token", ErrUnauthenticated)
	}
	u, err := a.authenticateBrowser(w, r)
	if err != nil && bearerErr != nil && errors.Is(err, ErrUnauthenticated) {
		return nil, bearerErr
	}
	return u, err
}

// authenticateBrowser authenticates with the mode's non-bearer credentials.
func (a *authenticator) authenticateBrowser(w http.ResponseWriter, r *http.Request) (*identity.User, error) {
	switch a.mode {
	case ModeNone:
		return a.authenticateAnon(w, r)
	case ModeOIDC:
		return a.authenticateSession(r)
	case ModeIAP:
		return a.authenticateIAP(r)
	case ModeALB:
		return a.authenticateALB(r)
	case ModeHeader:
		return a.authenticateHeader(r)
	}
	return nil, fmt.Errorf("%w: unsupported auth mode %q", ErrUnauthenticated, a.mode)
}

// bearerToken extracts the token of an "Authorization: Bearer" header.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(rest)
	return tok, tok != ""
}

// authenticateBearer resolves a static token, a Backyard API token or an
// IdP-issued JWT.
func (a *authenticator) authenticateBearer(ctx context.Context, tok string) (*identity.User, error) {
	if len(tok) > maxBearerBytes {
		return nil, fmt.Errorf("%w: bearer token too large", ErrUnauthenticated)
	}
	if st := a.matchStatic(tok); st != nil {
		return a.staticUser(st), nil
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: unrecognized bearer token", ErrUnauthenticated)
	}
	iss, _ := peekIssuer(parts[1])
	if iss == TokenIssuer {
		u, err := a.tokens.Verify(tok)
		if err != nil {
			return nil, err
		}
		return a.apiTokenUser(ctx, u)
	}
	if a.idp == nil {
		return nil, fmt.Errorf("%w: unrecognized bearer token", ErrUnauthenticated)
	}
	claims, err := a.idp.verify(ctx, tok)
	if errors.Is(err, errKeysUnavailable) {
		return nil, fmt.Errorf("auth: cannot verify identity provider token: %w", err)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: invalid identity provider token: %w", ErrUnauthenticated, err)
	}
	return a.finalize(ctx, a.mapClaims(claims), srcIDP)
}

// matchStatic compares the token's sha256 against every configured static
// token in constant time.
func (a *authenticator) matchStatic(tok string) *staticToken {
	if len(a.static) == 0 {
		return nil
	}
	sum := sha256.Sum256([]byte(tok))
	match := -1
	for i := range a.static {
		if subtle.ConstantTimeCompare(sum[:], a.static[i].sum[:]) == 1 && match < 0 {
			match = i
		}
	}
	if match < 0 {
		return nil
	}
	return &a.static[match]
}

func (a *authenticator) staticByName(name string) *staticToken {
	for i := range a.static {
		if a.static[i].name == name {
			return &a.static[i]
		}
	}
	return nil
}

func (a *authenticator) staticUser(st *staticToken) *identity.User {
	id := tokenPrefix + st.name
	return &identity.User{ID: id, Name: st.name, Admin: st.admin || a.cfg.Auth.IsAdmin(id, "", nil)}
}

// apiTokenUser applies mode rules to a verified API token's user.
func (a *authenticator) apiTokenUser(ctx context.Context, u *identity.User) (*identity.User, error) {
	switch {
	case strings.HasPrefix(u.ID, anonPrefix):
		if a.mode != ModeNone {
			return nil, fmt.Errorf("%w: anonymous tokens are only valid with auth.mode=none", ErrUnauthenticated)
		}
		u.Anonymous = true
		return a.finalize(ctx, u, srcAnon)
	case strings.HasPrefix(u.ID, tokenPrefix):
		st := a.staticByName(strings.TrimPrefix(u.ID, tokenPrefix))
		if st == nil {
			return nil, fmt.Errorf("%w: static token principal no longer configured", ErrUnauthenticated)
		}
		return a.staticUser(st), nil
	}
	return a.finalize(ctx, u, srcAPIToken)
}

// mapClaims maps IdP claims with the configured mapping.
func (a *authenticator) mapClaims(claims map[string]any) *identity.User {
	return identity.MapClaims(claims, a.cfg.Identity.Claims, a.cfg.Identity.ExtraClaims)
}

// finalize enriches u from the directory, computes Admin and applies the
// allowed_domains / allowed_groups gates.
func (a *authenticator) finalize(ctx context.Context, u *identity.User, src source) (*identity.User, error) {
	if u == nil || strings.TrimSpace(u.ID) == "" {
		return nil, fmt.Errorf("%w: identity has neither an email nor a subject", ErrUnauthenticated)
	}
	switch src {
	case srcStatic:
		return u, nil
	case srcAnon:
		u.Anonymous = true
		u.Admin = a.cfg.Auth.IsAdmin(u.ID, "", nil)
		return u, nil
	}
	u.Anonymous = false
	if a.dir != nil && u.Email != "" {
		p, err := a.dir.Lookup(ctx, u.Email)
		if err != nil {
			a.log.Warn("auth: directory lookup failed", "user", u.Email, "error", err)
		} else if p != nil {
			u.Enrich(p)
		}
	}
	u.Admin = a.cfg.Auth.IsAdmin(u.ID, u.Email, u.Groups)
	if err := a.gate(u); err != nil {
		return nil, err
	}
	return u, nil
}

// gate enforces auth.allowed_domains and auth.allowed_groups. Admins pass.
func (a *authenticator) gate(u *identity.User) error {
	if u.Admin {
		return nil
	}
	if doms := a.cfg.Auth.AllowedDomains; len(doms) > 0 {
		if !domainAllowed(identity.EmailDomain(u.Email), doms) {
			return fmt.Errorf("%w: %s is not in an allowed domain", ErrForbidden, displayID(u))
		}
	}
	if groups := a.cfg.Auth.AllowedGroups; len(groups) > 0 {
		ok := false
		for _, g := range groups {
			if u.InGroup(strings.TrimSpace(g)) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%w: %s is not a member of an allowed group", ErrForbidden, displayID(u))
		}
	}
	return nil
}

// domainAllowed reports whether domain matches an entry ("acme.com" exactly,
// "*.acme.com" or ".acme.com" for subdomains).
func domainAllowed(domain string, allowed []string) bool {
	if domain == "" {
		return false
	}
	for _, d := range allowed {
		d = strings.ToLower(strings.TrimSpace(d))
		d = strings.TrimPrefix(d, "@")
		switch {
		case d == "":
		case strings.HasPrefix(d, "*."):
			if strings.HasSuffix(domain, d[1:]) {
				return true
			}
		case strings.HasPrefix(d, "."):
			if strings.HasSuffix(domain, d) {
				return true
			}
		case domain == d:
			return true
		}
	}
	return false
}

func displayID(u *identity.User) string {
	if u.Email != "" {
		return u.Email
	}
	return u.ID
}

// peerTrusted reports whether the request's direct peer is a trusted proxy.
func (a *authenticator) peerTrusted(r *http.Request) bool {
	if len(a.trusted) == 0 {
		return false
	}
	ip := remoteIP(r.RemoteAddr)
	if ip == nil {
		return false
	}
	for _, n := range a.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// remoteIP parses the IP of a RemoteAddr ("ip:port", "[ip]:port" or "ip").
func remoteIP(addr string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(addr), "[]")
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	return net.ParseIP(host)
}

// newCookie builds a cookie with Backyard's standard attributes: HttpOnly,
// SameSite=Lax, and Secure when server.public_url is https (SPEC §8).
func (a *authenticator) newCookie(name, value, domain, path string, maxAge time.Duration) *http.Cookie {
	// Secure is scheme-dependent on purpose: browsers drop Secure cookies set
	// over plain http (dev servers and http-only intranet hosts), which would
	// make login impossible there.
	c := &http.Cookie{ //nolint:gosec // G124: Secure follows server.public_url's scheme (see above).
		Name:     name,
		Value:    value,
		Path:     path,
		Domain:   domain,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	}
	if maxAge > 0 {
		c.MaxAge = int(maxAge / time.Second)
		c.Expires = a.now().Add(maxAge).UTC()
	} else {
		c.MaxAge = -1
		c.Expires = time.Unix(1, 0).UTC()
	}
	return c
}
