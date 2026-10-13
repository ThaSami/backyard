package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// handleWS serves GET /ws: the authenticated user and site are handed to the
// realtime hub, which performs the Origin check and the upgrade.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ri := info(r)
	ri.area = "ws"
	if s.deps.Realtime == nil || s.cfg.Realtime.Disabled {
		s.writeAPIError(w, r, errUnsupported("realtime is not enabled on this server"))
		return
	}
	// The connection outlives server.read_timeout/write_timeout; the hub
	// manages liveness with pings.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	s.deps.Realtime.ServeWS(w, r, ri.site, ri.user, s.wsOrigins(r))
}

// wsOrigins lists the origins allowed to open this site's websocket (the
// realtime hub compares them with the Origin header exactly, ignoring
// case; without an Origin header, or with one naming the request's own
// Host, it allows the connection by itself):
//
//   - the site's canonical origin (host mode: <site>.<base_domain> with the
//     public URL's scheme and port; path mode: the hub origin, which all
//     sites share) and the origin the request was made to (which can
//     differ: a non-default port, X-Forwarded-Host from a trusted proxy,
//     a dev-mode alias such as 127.0.0.1);
//   - in path mode, the hub origin;
//   - with realtime.allow_cross_site_ws, the requesting page's origin when
//     it is the hub or any site of this server (isBackyardOrigin), both
//     normalized and exactly as sent.
func (s *Server) wsOrigins(r *http.Request) []string {
	ri := info(r)
	var out []string
	add := func(o string) {
		if o == "" {
			return
		}
		for _, x := range out {
			if strings.EqualFold(x, o) {
				return
			}
		}
		out = append(out, o)
	}
	add(s.siteOrigin(ri.site))
	add(ri.origin)
	if s.pathMode {
		add(s.hubOrigin)
	}
	if s.cfg.Realtime.AllowCrossSiteWS {
		if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" && s.isBackyardOrigin(o, ri) {
			add(parseOrigin(o))
			add(strings.TrimRight(o, "/"))
		}
	}
	return out
}

// siteOrigin is the canonical origin of a site: in host mode
// <scheme>://<site>.<base_domain>[:port] with the scheme and port of
// server.public_url (sites are routed by base_domain, which can differ from
// the public URL's host), in path mode the hub origin.
func (s *Server) siteOrigin(site string) string {
	if s.pathMode || site == "" {
		return s.hubOrigin
	}
	return normalizeOrigin(s.hubScheme, net.JoinHostPort(site+"."+s.baseDomain, s.hubPort))
}

// isBackyardOrigin reports whether o (an Origin header) is the origin of
// the hub or, in host mode, of a site of this server: the hub host or
// <site>.<base_domain>, with the scheme and port of server.public_url or of
// the current request (ri). Values that are not a bare
// scheme://host[:port] are rejected.
func (s *Server) isBackyardOrigin(o string, ri *reqInfo) bool {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || u.User != nil || u.Opaque != "" || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	port := portOf(u, scheme)
	hubSchemePort := scheme == s.hubScheme && port == s.hubPort
	reqSchemePort := scheme == ri.scheme && port == portOf(&url.URL{Host: ri.hostPort}, ri.scheme)
	if !hubSchemePort && !reqSchemePort {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == s.hubHostname || host == s.baseDomain {
		return true
	}
	if s.pathMode {
		return false
	}
	label, ok := strings.CutSuffix(host, "."+s.baseDomain)
	return ok && label != "" && !strings.Contains(label, ".") && s.validSite(label)
}

// portOf returns u's port, or the default port of scheme.
func portOf(u *url.URL, scheme string) string {
	if p := u.Port(); p != "" {
		return p
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}
