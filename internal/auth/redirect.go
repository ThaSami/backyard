package auth

import (
	"errors"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// maxRedirectBytes bounds the length of an rd parameter.
const maxRedirectBytes = 4096

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// sanitizeRedirect validates a post-login return URL (the rd parameter).
//
// Accepted are relative paths starting with a single "/" (resolved on the
// hub) and absolute URLs with the hub's scheme and port whose host is the hub
// host (or base domain) or a single-label subdomain of it, i.e. a site host.
// Anything else — other hosts, protocol-relative URLs, backslashes, control
// characters, userinfo — is replaced by the hub URL, so login can never be
// turned into an open redirect.
func (a *authenticator) sanitizeRedirect(rd string) string {
	hub := a.hubURL()
	rd = strings.TrimSpace(rd)
	if rd == "" || len(rd) > maxRedirectBytes || strings.ContainsRune(rd, '\\') ||
		strings.ContainsFunc(rd, unicode.IsControl) {
		return hub
	}
	if strings.HasPrefix(rd, "/") {
		if strings.HasPrefix(rd, "//") {
			return hub
		}
		u, err := url.Parse(rd)
		if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || a.isAuthFlowPath(u.Path) {
			return hub
		}
		if a.hub != nil {
			// Make it absolute so it also works when the login was started
			// from a site host.
			return a.hub.String() + rd
		}
		return rd
	}
	if a.hub == nil {
		return hub
	}
	u, err := url.Parse(rd)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return hub
	}
	if !strings.EqualFold(u.Scheme, a.hub.Scheme) || !a.isOwnHost(u) || a.isAuthFlowPath(u.Path) {
		return hub
	}
	return u.String()
}

// isAuthFlowPath reports whether p is one of the login flow's own endpoints
// (login, callback, logout). Returning there after a login would loop or
// immediately sign the user out; /cli stays allowed (the CLI handoff resumes
// after login).
func (a *authenticator) isAuthFlowPath(p string) bool {
	p = path.Clean("/" + p)
	if a.oidc != nil && a.oidc.callbackPath != "" && p == path.Clean(a.oidc.callbackPath) {
		return true
	}
	// Site-relative forms (path mode "/s/<site>/__backyard/auth/…") reach the
	// same endpoints.
	if i := strings.Index(p, PathPrefix); i > 0 {
		p = p[i:]
	}
	switch p {
	case LoginPath, CallbackPath, LogoutPath:
		return true
	}
	return false
}

// isOwnHost reports whether u points at the hub or a site host. In path mode
// sites live under the hub's origin, so only the hub host itself qualifies.
func (a *authenticator) isOwnHost(u *url.URL) bool {
	if defaultPort(u.Port(), u.Scheme) != defaultPort(a.hub.Port(), a.hub.Scheme) {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return false
	}
	bases := []string{strings.ToLower(a.hub.Hostname())}
	if bd := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(a.cfg.Server.BaseDomain), ".")); bd != "" && bd != bases[0] {
		bases = append(bases, bd)
	}
	pathMode := strings.EqualFold(a.cfg.Server.URLMode, "path")
	for _, base := range bases {
		if host == base {
			return true
		}
		if pathMode {
			continue
		}
		if label, ok := strings.CutSuffix(host, "."+base); ok && dnsLabelRe.MatchString(label) {
			return true
		}
	}
	return false
}

// defaultPort returns port, or the scheme's default port when empty.
func defaultPort(port, scheme string) string {
	if port != "" {
		return port
	}
	if strings.EqualFold(scheme, "https") {
		return "443"
	}
	return "80"
}

// validateLoopbackRedirect checks a CLI login redirect_uri: it must be
// http://127.0.0.1|localhost|[::1]:<port>/<path> without userinfo or
// fragment.
func validateLoopbackRedirect(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("redirect_uri is required")
	}
	if len(raw) > maxRedirectBytes || strings.ContainsFunc(raw, unicode.IsControl) || strings.ContainsRune(raw, '\\') {
		return nil, errors.New("redirect_uri is malformed")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("redirect_uri is malformed")
	}
	if u.Scheme != "http" {
		return nil, errors.New("redirect_uri must use http:// on a loopback address")
	}
	if u.User != nil || u.Opaque != "" || u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("redirect_uri must not contain userinfo or a fragment")
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1":
	default:
		return nil, errors.New("redirect_uri must point at 127.0.0.1, localhost or [::1]")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("redirect_uri must include a port")
	}
	if u.Path != "" && !strings.HasPrefix(u.Path, "/") {
		return nil, errors.New("redirect_uri path must be absolute")
	}
	return u, nil
}
