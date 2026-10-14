package realtime

import (
	"net/http"
	"net/url"
	"strings"
)

// originAllowed implements the websocket Origin check (cross-site websocket
// hijacking protection):
//
//   - no Origin header (non-browser client) → allowed; such clients do not
//     carry a victim's ambient credentials;
//   - an Origin whose host[:port] equals the request Host → allowed
//     (same origin; default ports are ignored). The scheme cannot be known
//     behind a TLS-terminating proxy and is not compared, except that a
//     request received over TLS never accepts an http:// origin (a page
//     injected into plain-HTTP traffic must not reach the HTTPS site);
//   - otherwise the Origin must exactly equal (case-insensitively, ignoring a
//     trailing slash) one of allowed, e.g. "https://foo.backyard.acme.com".
//
// Opaque origins ("null") and non-http(s) schemes are always rejected unless
// listed verbatim.
func originAllowed(r *http.Request, allowed []string) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	for _, a := range allowed {
		if a != "" && strings.EqualFold(strings.TrimRight(a, "/"), origin) {
			return true
		}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if r.TLS != nil && scheme != "https" {
		return false
	}
	return strings.EqualFold(stripDefaultPort(u.Host, scheme), stripDefaultPort(r.Host, ""))
}

// stripDefaultPort removes ":80"/":443" (for scheme "" both are removed, as
// the request scheme may be hidden by a TLS-terminating proxy).
func stripDefaultPort(host, scheme string) string {
	switch {
	case (scheme == "http" || scheme == "") && strings.HasSuffix(host, ":80"):
		return strings.TrimSuffix(host, ":80")
	case (scheme == "https" || scheme == "") && strings.HasSuffix(host, ":443"):
		return strings.TrimSuffix(host, ":443")
	}
	return host
}
