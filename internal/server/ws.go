package server

import (
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

// wsOrigins lists the origins allowed to open this site's websocket: the
// site's own origin (and the request's), the hub in path mode, and — with
// realtime.allow_cross_site_ws — the requesting page if it is any Backyard
// site.
func (s *Server) wsOrigins(r *http.Request) []string {
	ri := info(r)
	var out []string
	add := func(o string) {
		if o == "" {
			return
		}
		for _, x := range out {
			if x == o {
				return
			}
		}
		out = append(out, o)
	}
	add(parseOrigin(s.cfg.SiteURL(ri.site)))
	add(ri.origin)
	if s.pathMode {
		add(s.hubOrigin)
	}
	if s.cfg.Realtime.AllowCrossSiteWS {
		if o := r.Header.Get("Origin"); o != "" && s.isBackyardOrigin(o) {
			add(parseOrigin(o))
		}
	}
	return out
}

// isBackyardOrigin reports whether o is the hub or a site origin.
func (s *Server) isBackyardOrigin(o string) bool {
	u, err := url.Parse(strings.TrimSpace(o))
	if err != nil || u.Host == "" {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	pub, err := url.Parse(s.publicURL)
	if err != nil {
		return false
	}
	if scheme != s.hubScheme || portOf(u, scheme) != portOf(pub, s.hubScheme) {
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

func portOf(u *url.URL, scheme string) string {
	if p := u.Port(); p != "" {
		return p
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}
