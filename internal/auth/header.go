package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/thasami/backyard/internal/identity"
)

const (
	// easyAuthPrincipalHeader is Azure App Service / Container Apps
	// authentication's base64 JSON principal.
	easyAuthPrincipalHeader = "X-MS-CLIENT-PRINCIPAL"
	// maxHeaderValueBytes bounds identity header values.
	maxHeaderValueBytes = 64 << 10
)

// asymmetricAlgs are the JWS algorithms accepted for proxy JWTs.
var asymmetricAlgs = []string{
	oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512,
	oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA,
}

// headerAuth implements auth.mode=header (SPEC §8.2).
type headerAuth struct {
	emailHeader  string
	nameHeader   string
	groupsHeader string
	jwtHeader    string
	verifier     *oidc.IDTokenVerifier // nil without JWT verification
	keys         *jwksKeySet
	audiences    map[string]bool
}

func (a *authenticator) newHeaderAuth() (*headerAuth, error) {
	c := a.cfg.Auth.Header
	h := &headerAuth{
		emailHeader:  strings.TrimSpace(c.EmailHeader),
		nameHeader:   strings.TrimSpace(c.NameHeader),
		groupsHeader: strings.TrimSpace(c.GroupsHeader),
		jwtHeader:    strings.TrimSpace(c.JWTHeader),
		audiences:    stringSet(c.Audiences...),
	}
	if h.emailHeader == "" && h.jwtHeader == "" {
		return nil, errors.New("auth: auth.header.email_header or auth.header.jwt_header is required for auth.mode=header")
	}
	if h.jwtHeader == "" {
		if len(a.trusted) == 0 {
			return nil, errors.New("auth: server.trusted_proxies must be set for auth.mode=header without JWT verification")
		}
		return h, nil
	}
	jwks := strings.TrimSpace(c.JWKSURL)
	if jwks == "" {
		return nil, errors.New("auth: auth.header.jwks_url is required when auth.header.jwt_header is set")
	}
	issuer := strings.TrimSpace(c.Issuer)
	if issuer == "" {
		a.log.Warn("auth: auth.header.issuer is not set; proxy JWTs are accepted from any issuer signing with jwks_url")
	}
	if len(h.audiences) == 0 {
		a.log.Warn("auth: auth.header.audiences is not set; proxy JWTs are accepted for any audience")
	}
	ks := newJWKSKeySet(jwks, a.client, a.now, a.log)
	h.keys = ks
	h.verifier = oidc.NewVerifier(issuer, ks, &oidc.Config{
		SkipClientIDCheck:    true, // checked against audiences below
		SkipIssuerCheck:      issuer == "",
		SupportedSigningAlgs: asymmetricAlgs,
		Now:                  a.now,
	})
	return h, nil
}

// authenticateHeader authenticates with trusted proxy headers.
//
// Without JWT verification the headers are only honoured when the direct
// peer is in server.trusted_proxies. With jwt_header the JWT is the trust
// anchor; if trusted_proxies is also set the peer must additionally be
// trusted.
func (a *authenticator) authenticateHeader(r *http.Request) (*identity.User, error) {
	h := a.header
	trusted := a.peerTrusted(r)
	if h.verifier != nil {
		if len(a.trusted) > 0 && !trusted {
			return nil, fmt.Errorf("%w: identity headers from untrusted peer", ErrUnauthenticated)
		}
		if err := singleValued(r, h.jwtHeader); err != nil {
			return nil, err
		}
		raw := strings.TrimSpace(r.Header.Get(h.jwtHeader))
		if s, ok := strings.CutPrefix(raw, "Bearer "); ok {
			raw = strings.TrimSpace(s)
		}
		if raw == "" {
			return nil, fmt.Errorf("%w: no %s header", ErrUnauthenticated, h.jwtHeader)
		}
		if len(raw) > maxHeaderValueBytes {
			return nil, fmt.Errorf("%w: %s header too large", ErrUnauthenticated, h.jwtHeader)
		}
		tok, err := h.verifier.Verify(r.Context(), raw)
		if err == nil && len(h.audiences) > 0 && !anyIn(tok.Audience, h.audiences) {
			err = fmt.Errorf("unexpected audience %q", tok.Audience)
		}
		if err != nil && tok == nil {
			if kerr := h.keys.unavailable(raw); kerr != nil {
				return nil, fmt.Errorf("auth: cannot verify proxy JWT: %w", kerr)
			}
		}
		var claims map[string]any
		if err == nil {
			err = tok.Claims(&claims)
		}
		if err != nil {
			a.log.Warn("auth: invalid proxy JWT", "remote", r.RemoteAddr, "error", err)
			return nil, fmt.Errorf("%w: invalid proxy JWT: %w", ErrUnauthenticated, err)
		}
		u := a.mapClaims(claims)
		if trusted && len(u.Groups) == 0 && h.groupsHeader != "" {
			if err := singleValued(r, h.groupsHeader); err != nil {
				return nil, err
			}
			u.Groups = identity.ParseList(headerValue(r, h.groupsHeader))
		}
		return a.finalize(r.Context(), u, srcIdP)
	}

	if !trusted {
		return nil, fmt.Errorf("%w: identity headers from untrusted peer", ErrUnauthenticated)
	}
	for _, name := range []string{h.emailHeader, h.nameHeader, h.groupsHeader, easyAuthPrincipalHeader} {
		if err := singleValued(r, name); err != nil {
			return nil, err
		}
	}
	v := strings.TrimPrefix(headerValue(r, h.emailHeader), googleAccountsPrefix)
	if v == "" {
		return nil, fmt.Errorf("%w: no %s header", ErrUnauthenticated, h.emailHeader)
	}
	u := &identity.User{}
	if email := identity.NormalizeEmail(v); email != "" {
		u.ID, u.Email = email, email
	} else {
		u.ID = "sub:" + v
		u.Name = v
	}
	if h.nameHeader != "" {
		if n := headerValue(r, h.nameHeader); n != "" {
			u.Name = truncateRunes(n, maxTokenNameRunes)
		}
	}
	if h.groupsHeader != "" {
		u.Groups = identity.ParseList(headerValue(r, h.groupsHeader))
	}
	if p := r.Header.Get(easyAuthPrincipalHeader); p != "" && strings.HasPrefix(strings.ToUpper(h.emailHeader), "X-MS-CLIENT-PRINCIPAL") {
		name, groups := parseEasyAuthPrincipal(p)
		if u.Name == "" {
			u.Name = name
		}
		if len(u.Groups) == 0 {
			u.Groups = groups
		}
	}
	if u.Name == "" {
		if at := strings.LastIndexByte(u.Email, '@'); at > 0 {
			u.Name = u.Email[:at]
		} else {
			u.Name = u.ID
		}
	}
	return a.finalize(r.Context(), u, srcIdP)
}

// singleValued rejects a request carrying an identity header more than once.
// A proxy that appends its value instead of replacing the client's would
// otherwise let the client-supplied value (the first one) win.
func singleValued(r *http.Request, name string) error {
	if name != "" && len(r.Header.Values(name)) > 1 {
		return fmt.Errorf("%w: %s header present more than once", ErrUnauthenticated, name)
	}
	return nil
}

// headerValue returns a trimmed, size-bounded header value.
func headerValue(r *http.Request, name string) string {
	v := strings.TrimSpace(r.Header.Get(name))
	if len(v) > maxHeaderValueBytes {
		return ""
	}
	return v
}

// parseEasyAuthPrincipal extracts the display name and groups/roles from an
// X-MS-CLIENT-PRINCIPAL header (base64 JSON
// {"auth_typ","name_typ","role_typ","claims":[{"typ","val"}]}).
func parseEasyAuthPrincipal(v string) (name string, groups []string) {
	if len(v) > maxHeaderValueBytes {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(v))
	if err != nil {
		if b, err = decodeSegment(v); err != nil {
			return "", nil
		}
	}
	var p struct {
		NameTyp string `json:"name_typ"`
		RoleTyp string `json:"role_typ"`
		Claims  []struct {
			Typ string `json:"typ"`
			Val string `json:"val"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return "", nil
	}
	var list []string
	for _, c := range p.Claims {
		switch {
		case c.Typ == "name":
			if name == "" {
				name = strings.TrimSpace(c.Val)
			}
		case c.Typ == "groups" || c.Typ == "roles" || (p.RoleTyp != "" && c.Typ == p.RoleTyp) ||
			c.Typ == "http://schemas.microsoft.com/ws/2008/06/identity/claims/groups":
			list = append(list, c.Val)
		}
	}
	return name, identity.ParseList(list)
}
