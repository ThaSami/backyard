package server

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/identity"
)

// reqInfo is the per-request state shared by the middleware and handlers.
// It is created by the middleware and mutated only by the goroutine serving
// the request.
type reqInfo struct {
	start     time.Time
	requestID string
	rec       *statusRecorder

	host     string // client-visible hostname, lowercased, without port
	hostPort string // client-visible host[:port], lowercased
	scheme   string // "http" | "https" as seen by the client
	origin   string // normalized scheme://host[:port] of the request
	clientIP string
	origURI  string // request URI (escaped path + query) before prefix stripping

	area   string         // metrics label
	site   string         // site name; "" on the hub
	prefix string         // "/s/<site>" for path-mode site requests, else ""
	user   *identity.User // set after authentication
}

type reqInfoKey struct{}

// info returns the request state (never nil inside the middleware chain).
func info(r *http.Request) *reqInfo {
	if ri, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
		return ri
	}
	return &reqInfo{start: time.Now(), area: "other", scheme: "http"}
}

func withInfo(ctx context.Context, ri *reqInfo) context.Context {
	return context.WithValue(ctx, reqInfoKey{}, ri)
}

// originalURL is the absolute URL the client requested.
func (ri *reqInfo) originalURL() string { return ri.origin + ri.origURI }

// siteRoot is the absolute URL of the current site's root (or the hub's).
func (ri *reqInfo) siteRoot() string { return ri.origin + ri.prefix + "/" }

// resolveClient fills host, scheme, origin and client IP, honouring
// X-Forwarded-Host/Proto/For only from trusted proxies.
func (s *Server) resolveClient(ri *reqInfo, r *http.Request) {
	peer := remoteIP(r.RemoteAddr)
	trusted := peer != nil && s.isTrusted(peer)

	hostPort := r.Host
	if hostPort == "" && r.URL != nil {
		hostPort = r.URL.Host
	}
	scheme := s.hubScheme
	if r.TLS != nil {
		scheme = "https"
	}
	if trusted {
		if h := firstValue(r.Header.Get("X-Forwarded-Host")); h != "" && validHostHeader(h) {
			hostPort = h
		}
		switch p := strings.ToLower(firstValue(r.Header.Get("X-Forwarded-Proto"))); p {
		case "http", "https":
			scheme = p
		}
	}
	hostPort = strings.ToLower(strings.TrimSpace(hostPort))
	ri.hostPort = hostPort
	ri.host = hostnameOf(hostPort)
	ri.scheme = scheme
	ri.origin = normalizeOrigin(scheme, hostPort)

	ri.clientIP = ""
	if peer != nil {
		ri.clientIP = peer.String()
	}
	if trusted {
		if ip := s.forwardedFor(r.Header.Values("X-Forwarded-For")); ip != nil {
			ri.clientIP = ip.String()
		}
	}
	if ri.clientIP == "" {
		ri.clientIP = "unknown"
	}
}

func (s *Server) isTrusted(ip net.IP) bool {
	for _, n := range s.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedFor walks the X-Forwarded-For chain from the right and returns the
// first address that is not a trusted proxy (or the leftmost valid one when
// every hop is trusted).
func (s *Server) forwardedFor(values []string) net.IP {
	var hops []string
	for _, v := range values {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	var leftmost net.IP
	for i := len(hops) - 1; i >= 0; i-- {
		ip := parseIPLoose(hops[i])
		if ip == nil {
			// Garbage in the chain: everything left of it is untrustworthy.
			break
		}
		leftmost = ip
		if !s.isTrusted(ip) {
			return ip
		}
	}
	return leftmost
}

// remoteIP parses the IP of an http.Request.RemoteAddr.
func remoteIP(addr string) net.IP {
	if addr == "" {
		return nil
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	return parseIPLoose(addr)
}

// parseIPLoose parses "1.2.3.4", "1.2.3.4:5", "[::1]", "[::1]:5" or "::1".
func parseIPLoose(s string) net.IP {
	s = strings.TrimSpace(s)
	if ip := net.ParseIP(strings.Trim(s, "[]")); ip != nil {
		return normalizeIP(ip)
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		if ip := net.ParseIP(h); ip != nil {
			return normalizeIP(ip)
		}
	}
	return nil
}

func normalizeIP(ip net.IP) net.IP {
	if v4 := ip.To4(); v4 != nil {
		return v4
	}
	return ip
}

func firstValue(h string) string {
	v, _, _ := strings.Cut(h, ",")
	return strings.TrimSpace(v)
}

// validHostHeader rejects values that cannot be a host[:port].
func validHostHeader(h string) bool {
	if h == "" || len(h) > 255 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == ':', c == '[', c == ']', c == '_':
		default:
			return false
		}
	}
	return true
}

// hostnameOf strips the port from host[:port] and a trailing dot.
func hostnameOf(hostPort string) string {
	h := hostPort
	if strings.HasPrefix(h, "[") {
		if end := strings.IndexByte(h, ']'); end > 0 {
			h = h[1:end]
		}
	} else if i := strings.LastIndexByte(h, ':'); i >= 0 && strings.Count(h, ":") == 1 {
		h = h[:i]
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// normalizeOrigin renders scheme://host[:port] with default ports removed.
func normalizeOrigin(scheme, hostPort string) string {
	scheme = strings.ToLower(scheme)
	hostPort = strings.ToLower(strings.TrimSpace(hostPort))
	switch {
	case scheme == "https" && strings.HasSuffix(hostPort, ":443"):
		hostPort = strings.TrimSuffix(hostPort, ":443")
	case scheme == "http" && strings.HasSuffix(hostPort, ":80"):
		hostPort = strings.TrimSuffix(hostPort, ":80")
	}
	if hostPort == "" {
		return ""
	}
	return scheme + "://" + strings.TrimSuffix(hostPort, ".")
}

// parseOrigin normalizes an Origin (or any absolute URL) to its origin. It
// returns "" for opaque or non-http(s) values.
func parseOrigin(v string) string {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return ""
	}
	return normalizeOrigin(u.Scheme, u.Host)
}

// stripPrefix returns a shallow copy of r whose URL path has prefix removed
// (like http.StripPrefix). The result path always starts with "/".
func stripPrefix(r *http.Request, prefix string) *http.Request {
	p := strings.TrimPrefix(r.URL.Path, prefix)
	if p == "" || p[0] != '/' {
		p = "/" + p
	}
	rp := ""
	if r.URL.RawPath != "" {
		if t, ok := strings.CutPrefix(r.URL.RawPath, prefix); ok {
			rp = t
			if rp == "" || rp[0] != '/' {
				rp = "/" + rp
			}
		}
	}
	return withPath(r, p, rp)
}

// withPath returns a shallow copy of r with a different URL path.
func withPath(r *http.Request, p, rawPath string) *http.Request {
	r2 := new(http.Request)
	*r2 = *r
	r2.URL = new(url.URL)
	*r2.URL = *r.URL
	r2.URL.Path = p
	r2.URL.RawPath = rawPath
	return r2
}

// cleanURLPath cleans an URL path like path.Clean but keeps a trailing slash.
func cleanURLPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	// Fast path: already clean.
	if !strings.Contains(p, "//") && !strings.Contains(p, "/./") && !strings.Contains(p, "/../") &&
		!strings.HasSuffix(p, "/.") && !strings.HasSuffix(p, "/..") {
		return p
	}
	trailing := strings.HasSuffix(p, "/")
	np := path.Clean(p)
	if trailing && np != "/" {
		np += "/"
	}
	return np
}
