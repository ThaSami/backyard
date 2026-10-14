package auth

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/thasami/backyard/internal/config"
)

// maxForeignCookiePathSegments bounds the request path prefixes at which
// expireForeignCookies looks for planted cookies (it keeps the number of
// Set-Cookie headers small; API paths are at most 7 segments deep).
const maxForeignCookiePathSegments = 8

// Planted cookies. In host mode every site is a subdomain of the cookie
// domain and runs its author's JavaScript, which can set a non-HttpOnly
// cookie with the same name as a Backyard cookie for Domain=.<base_domain>
// (or a parent domain) and a longer Path. Browsers send such a cookie
// alongside — and before — the HttpOnly one Backyard set, on every host. A
// sealed session or a signed guest id is not tied to a browser, so a site
// author can plant their own valid one in visitors' browsers.
//
// Backyard therefore never picks one of several valid cookies that name
// different users: it fails closed (ErrCookieConflict), so planting degrades
// to a sign-out on the paths the planted cookie covers. It also expires every
// variant of the cookie that it did not set itself (best effort; see
// expireForeignCookies), which removes the planted cookie in the usual cases
// so the next request works again. The cookie Backyard set itself is left
// alone: expiring it would leave the planted cookie as the only one.

// expireForeignCookies expires the cookies called name that a browser may
// have sent with r but that Backyard did not set. Backyard sets its cookies
// only at Path=/ for a.cookieDomain (host-only when that is empty); every
// other combination of a domain that matches Backyard's hosts (host-only, the
// cookie domain — or the hub host — and its parent domains) and a path that
// matches r's path is foreign: either a stale host-only cookie from an
// earlier configuration or a cookie planted by a site's page. Cookies the
// browser keeps for other paths or in a partition are out of reach.
func (a *authenticator) expireForeignCookies(w http.ResponseWriter, r *http.Request, name string) {
	if w == nil {
		return
	}
	own := strings.TrimPrefix(a.cookieDomain, ".")
	domains := []string{""} // host-only
	base := own
	if base == "" && a.hub != nil {
		base = a.hub.Hostname()
	}
	if config.CookieDomainCapable(base) {
		for d := base; strings.Contains(d, "."); d = d[strings.IndexByte(d, '.')+1:] {
			domains = append(domains, d)
		}
	}
	paths := cookiePathPrefixes(requestPath(r))
	for _, d := range domains {
		for _, p := range paths {
			if d == own && p == "/" {
				continue
			}
			http.SetCookie(w, a.newCookie(name, "", d, p, 0))
		}
	}
}

// requestPath is the path of the URL the browser requested, percent-encoded
// as the browser sent it (cookie paths are matched against that form). It
// comes from the request line, since handlers may see a request whose path
// has been stripped of a prefix.
func requestPath(r *http.Request) string {
	uri := r.RequestURI
	if uri == "" {
		return r.URL.EscapedPath()
	}
	if strings.HasPrefix(uri, "/") {
		p, _, _ := strings.Cut(uri, "?")
		return p
	}
	if u, err := url.ParseRequestURI(uri); err == nil {
		return u.EscapedPath()
	}
	return r.URL.EscapedPath()
}

// cookiePathPrefixes returns the cookie paths that path-match p (RFC 6265
// §5.1.4): "/", then each prefix of p that ends just before or just after a
// "/", then p itself — up to maxForeignCookiePathSegments segments. Prefixes
// a Set-Cookie Path attribute cannot carry are skipped.
func cookiePathPrefixes(p string) []string {
	out := []string{"/"}
	if p == "" || p[0] != '/' {
		return out
	}
	seen := map[string]bool{"/": true}
	add := func(s string) {
		if seen[s] || !validCookiePath(s) {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	segments := 0
	for i := 1; i < len(p); i++ {
		if p[i] != '/' {
			continue
		}
		if segments++; segments > maxForeignCookiePathSegments {
			return out
		}
		add(p[:i])
		add(p[:i+1])
	}
	if segments < maxForeignCookiePathSegments {
		add(p)
	}
	return out
}

// validCookiePath reports whether s can be sent as a Path attribute
// unchanged.
func validCookiePath(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c >= 0x7f || c == ';' {
			return false
		}
	}
	return true
}
