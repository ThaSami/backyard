package server

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/thasami/backyard/internal/sites"
)

// Reserved paths (SPEC §2).
const (
	reservedRoot = "/__backyard"
	reservedPfx  = "/__backyard/"
	apiRoot      = "/__backyard/api"
	apiPfx       = "/__backyard/api/"
	authRoot     = "/__backyard/auth"
	authPfx      = "/__backyard/auth/"
	filesPfx     = "/__backyard/files/"
	healthzPath  = "/__backyard/healthz"
	readyzPath   = "/__backyard/readyz"
	metricsPath  = "/__backyard/metrics"
	sdkJSPath    = "/__backyard/backyard.js"
	sdkMJSPath   = "/__backyard/backyard.mjs"
	sdkDTSPath   = "/__backyard/backyard.d.ts"
	infoPath     = "/__backyard/api/info"
	deployRoot   = "/__backyard/api/deploy"
	sitePathPfx  = "/s/"
)

type hostKind int

const (
	hostUnknown hostKind = iota
	hostHub
	hostSite
)

// classifyHost maps a request hostname to the hub, a site or nothing.
func (s *Server) classifyHost(host string) (hostKind, string) {
	if host == "" {
		if s.cfg.Server.Dev {
			return hostHub, ""
		}
		return hostUnknown, ""
	}
	if host == s.baseDomain || host == s.hubHostname {
		return hostHub, ""
	}
	if !s.pathMode {
		if label, ok := strings.CutSuffix(host, "."+s.baseDomain); ok && label != "" && !strings.Contains(label, ".") {
			return hostSite, label
		}
	}
	if s.cfg.Server.Dev && devHubAlias(host) {
		// Local development: IP literals (127.0.0.1, ::1, a LAN IP) and
		// localhost names reach the hub. Other names stay unknown so that a
		// DNS-rebinding page (evil.example → 127.0.0.1) cannot use the dev
		// server's APIs as its own origin.
		return hostHub, ""
	}
	return hostUnknown, ""
}

// devHubAlias reports whether host may stand in for the hub in dev mode.
func devHubAlias(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	return host == "localhost" || strings.HasSuffix(host, ".localhost")
}

// route is the root handler (inside the middleware).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	ri := info(r)
	p := r.URL.Path

	// Non-canonical paths ("//", "/./", "/../") are redirected (GET/HEAD) or
	// rejected, so that no handler ever sees them.
	if cp := cleanURLPath(p); cp != p {
		ri.area = "other"
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			u := &url.URL{Path: cp, RawQuery: r.URL.RawQuery}
			http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
			return
		}
		s.fail(w, r, errBadRequest("non-canonical request path"))
		return
	}

	switch p {
	case healthzPath:
		ri.area = "health"
		s.handleHealthz(w, r)
		return
	case readyzPath:
		ri.area = "health"
		s.handleReadyz(w, r)
		return
	}

	kind, site := s.classifyHost(ri.host)
	switch kind {
	case hostSite:
		s.serveSite(w, r, site)
	case hostHub:
		if s.pathMode && strings.HasPrefix(p, sitePathPfx) {
			s.servePathSite(w, r)
			return
		}
		s.serveHub(w, r)
	default:
		ri.area = "unknown_host"
		e := errNotFound("%q is not a Backyard host.", ri.host)
		e.Title = "Unknown host"
		s.fail(w, r, e)
	}
}

// servePathSite handles /s/<site>/... on the hub host in path mode.
func (s *Server) servePathSite(w http.ResponseWriter, r *http.Request) {
	ri := info(r)
	rest := strings.TrimPrefix(r.URL.Path, sitePathPfx)
	name, _, hasSlash := strings.Cut(rest, "/")
	if name == "" {
		ri.area = "hub"
		s.fail(w, r, errNotFound("No site name in the URL."))
		return
	}
	if !hasSlash {
		ri.area = "static"
		u := &url.URL{Path: r.URL.Path + "/", RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
		return
	}
	prefix := sitePathPfx + name
	ri.prefix = prefix
	s.serveSite(w, stripPrefix(r, prefix), name)
}

// validSite reports whether a site name can exist (pattern, not reserved) or
// is a dev site.
func (s *Server) validSite(name string) bool {
	if _, ok := s.devSites[name]; ok {
		return true
	}
	return sites.ValidateName(name, s.reserved) == nil
}

// serveSite dispatches a request for a site; r's path is site-relative.
func (s *Server) serveSite(w http.ResponseWriter, r *http.Request, site string) {
	ri := info(r)
	ri.site = site
	p := r.URL.Path

	switch {
	case p == healthzPath:
		ri.area = "health"
		s.handleHealthz(w, r)
		return
	case p == readyzPath:
		ri.area = "health"
		s.handleReadyz(w, r)
		return
	}

	if !s.validSite(site) {
		ri.area = "static"
		s.fail(w, r, siteNotFound(site))
		return
	}

	switch {
	case p == sdkJSPath || p == sdkMJSPath || p == sdkDTSPath:
		ri.area = "sdk"
		s.serveSDK(w, r, site)
	case p == authRoot || strings.HasPrefix(p, authPfx):
		ri.area = "auth"
		s.redirectAuthToHub(w, r)
	case p == apiRoot || strings.HasPrefix(p, apiPfx):
		ri.area = "api"
		s.serveSiteAPI(w, r)
	case strings.HasPrefix(p, filesPfx):
		ri.area = "files"
		s.serveUploadedFile(w, r)
	case p == reservedRoot || strings.HasPrefix(p, reservedPfx):
		ri.area = "other"
		s.writeAPIError(w, r, errNotFound("no such endpoint: %s", truncate(p, 200)))
	default:
		ri.area = "static"
		s.serveStatic(w, r, site)
	}
}

// serveHub dispatches a request for the hub host.
func (s *Server) serveHub(w http.ResponseWriter, r *http.Request) {
	ri := info(r)
	p := r.URL.Path
	switch {
	case p == sdkJSPath || p == sdkMJSPath || p == sdkDTSPath:
		ri.area = "sdk"
		s.serveSDK(w, r, "")
	case p == authRoot || strings.HasPrefix(p, authPfx):
		ri.area = "auth"
		s.deps.Auth.Handler().ServeHTTP(w, r)
	case p == infoPath:
		ri.area = "deploy"
		s.serveInfo(w, r)
	case p == deployRoot || strings.HasPrefix(p, deployRoot+"/"):
		ri.area = "deploy"
		s.serveDeploy(w, r)
	case p == metricsPath && s.metricsOnHub:
		ri.area = "metrics"
		s.serveHubMetrics(w, r)
	case p == apiRoot || strings.HasPrefix(p, apiPfx):
		ri.area = "api"
		s.serveHubAPI(w, r)
	case p == reservedRoot || strings.HasPrefix(p, reservedPfx):
		ri.area = "other"
		s.writeAPIError(w, r, errNotFound("no such endpoint: %s", truncate(p, 200)))
	default:
		ri.area = "hub"
		s.serveHubPage(w, r)
	}
}

// redirectAuthToHub sends /__backyard/auth/* on a site to the same path on
// the hub (the auth endpoints live on the hub only).
func (s *Server) redirectAuthToHub(w http.ResponseWriter, r *http.Request) {
	ri := info(r)
	rawQuery := r.URL.RawQuery
	if r.URL.Path == authPfx+"login" && r.URL.Query().Get("rd") == "" {
		if rawQuery != "" {
			rawQuery += "&"
		}
		rawQuery += "rd=" + url.QueryEscape(ri.siteRoot())
	}
	u := s.publicURL + r.URL.EscapedPath()
	if rawQuery != "" {
		u += "?" + rawQuery
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u, http.StatusFound)
}

// truncate shortens s to at most n bytes for messages and logs.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
