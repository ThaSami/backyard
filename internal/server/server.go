// Package server is Backyard's HTTP layer (SPEC §2–§6): host/path routing,
// static site serving, the site API (identity, db, files, ai, warehouse,
// websocket), the hub (UI, docs, llms.txt, site list, tokens), the embedded
// deploy gateway mount, health checks, rate limiting and metrics.
//
// # Routing
//
// The request host (Host, or X-Forwarded-Host from server.trusted_proxies) is
// classified as the hub (server.base_domain or the public URL's host), a site
// (a single DNS label under base_domain, host mode only) or an unknown host
// (404). With server.dev, IP-literal and localhost host names also reach the
// hub (other names stay unknown, which defeats DNS rebinding against a local
// dev server). In path mode sites live under /s/<site>/ on the hub host. The path
// prefix /__backyard/ is reserved on every host: site files can never shadow
// it. /__backyard/healthz and /__backyard/readyz answer on any host without
// authentication.
//
// # Security
//
// Every other request is authenticated with the configured
// auth.Authenticator, except the SDK files, the auth endpoints, the deploy
// gateway (which authenticates by itself) and the machine paths of the
// Claude connector. Unauthenticated browser page loads are redirected to the
// login URL; API calls get 401 JSON with "login_url". State-changing API
// calls must carry "X-Backyard: 1" and, when present, an Origin equal to the
// request's own origin (path mode: the hub origin). Site API calls are rate
// limited per user (anonymous: per client IP).
//
// # Claude connector
//
// With mcp.enabled (SPEC §8.4, §14.1) the hub also serves the remote MCP
// endpoint /__backyard/mcp, the OAuth endpoints
// /__backyard/oauth/{register,token,revoke} and the discovery documents
// /.well-known/oauth-authorization-server and
// /.well-known/oauth-protected-resource/__backyard/mcp. These machine paths
// authenticate by themselves (the endpoint accepts only MCP access tokens)
// and never read cookies or identity-proxy headers; they exist on the
// canonical hub host only and answer 404 while the connector is off. The
// sign-in and consent page /__backyard/auth/authorize is an auth endpoint
// like the others, and /__backyard/api/mcp/grants a hub API. Registration
// and revocation are rate limited per client IP; authorize, the token
// endpoint and the MCP endpoint per user (authorize and token: per client
// IP for requests that identify no one).
//
// # Timeouts
//
// No http.TimeoutHandler is used: streaming responses (AI server-sent events,
// websockets, large downloads and uploads) extend or clear the connection
// deadlines themselves, and backend calls are bounded by contexts.
//
// # Large responses on Cloud Run
//
// Cloud Run rejects HTTP/1 responses that declare a Content-Length above
// 32 MiB. When K_SERVICE is set, static files and uploads above that size
// are streamed without a Content-Length (chunked), and byte ranges above it
// are shortened to 32 MiB (see serveObject).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/ai"
	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/ratelimit"
	"github.com/thasami/backyard/internal/realtime"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/warehouse"
)

// Deps are the components the server needs. AI, Warehouse, Realtime and
// Gateway may be nil (feature disabled → 404/unsupported responses).
type Deps struct {
	Config    *config.Config
	Logger    *slog.Logger
	Blob      blob.Store
	Docs      docstore.Store
	Sites     *sites.Registry
	Auth      auth.Authenticator
	Tokens    *auth.Tokens
	Users     *identity.UserCache
	Realtime  realtime.Hub
	AI        ai.Service
	Warehouse warehouse.Service
	// Gateway is the embedded deploy gateway, mounted at /__backyard/api/deploy
	// on the hub host with that prefix stripped. GET /__backyard/api/info is
	// served by forwarding to the gateway's /info.
	Gateway http.Handler
	// MCP is the Claude connector's authorization server (auth.MCPOf(Auth));
	// nil while mcp.enabled is false. It serves the discovery documents and
	// /__backyard/oauth/*, and backs the grants API.
	MCP *auth.MCP
	// MCPEndpoint serves POST /__backyard/mcp (internal/mcpremote); nil
	// answers the disabled 404.
	MCPEndpoint http.Handler
	// DevSites maps site names to local directories served live (backyard dev).
	// Like deploys, they never expose hidden paths (a segment starting with
	// "." other than ".well-known").
	DevSites map[string]string
	// Version string reported by /__backyard/api/info.
	Version string
}

// Server is the assembled HTTP application.
type Server struct {
	deps Deps

	cfg  *config.Config
	log  *slog.Logger
	docs docstore.Store // Deps.Docs, wrapped with docstore.WithLimits if needed

	handler   http.Handler
	siteMux   *http.ServeMux
	hubMux    *http.ServeMux
	metrics   *metrics
	limiter   *ratelimit.Limiter
	cache     *objectCache // nil when storage.cache_bytes < 0
	siteCache *existsCache
	touches   *touchThrottle

	baseDomain   string // lowercased, no trailing dot
	hubHostname  string // hostname of server.public_url
	hubOrigin    string // normalized origin of server.public_url
	hubScheme    string // scheme of server.public_url
	hubPort      string // port of server.public_url (explicit or the scheme's default)
	publicURL    string // server.public_url without trailing slash
	pathMode     bool
	trusted      []*net.IPNet
	devSites     map[string]string // site → absolute directory
	reserved     []string
	metricsOnHub bool
	gatewayOn    bool // deploy gateway mounted at /__backyard/api/deploy

	// maxFixedLength, when positive, is the largest Content-Length an
	// HTTP/1 response of serveObject may declare (see responseLengthCap).
	maxFixedLength int64

	bg *background // background work (user cache touches, audit writes)
}

// New validates deps and builds the server.
//
// Deps.Docs is wrapped with docstore.WithLimits(database.max_doc_bytes,
// database.max_docs_per_site) unless it already exposes an Unwrap method
// (i.e. the caller wrapped it). The embedded deploy gateway is mounted only
// when deploy.embedded_gateway is not false; GET /__backyard/api/info is
// forwarded to Deps.Gateway whenever it is set.
func New(d Deps) (*Server, error) {
	if d.Config == nil {
		return nil, errors.New("server: Deps.Config is required")
	}
	var missing []string
	if d.Blob == nil {
		missing = append(missing, "Blob")
	}
	if d.Docs == nil {
		missing = append(missing, "Docs")
	}
	if d.Sites == nil {
		missing = append(missing, "Sites")
	}
	if d.Auth == nil {
		missing = append(missing, "Auth")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("server: missing dependencies: %s", strings.Join(missing, ", "))
	}
	cfg := d.Config
	if d.Logger == nil {
		d.Logger = slog.Default()
	}

	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.Server.BaseDomain), "."))
	if base == "" {
		return nil, errors.New("server: server.base_domain is required")
	}
	pub, err := url.Parse(strings.TrimSpace(cfg.Server.PublicURL))
	if err != nil || (pub.Scheme != "http" && pub.Scheme != "https") || pub.Host == "" {
		return nil, fmt.Errorf("server: server.public_url must be an absolute http(s) URL, got %q", cfg.Server.PublicURL)
	}
	scheme := strings.ToLower(pub.Scheme)

	s := &Server{
		deps:        d,
		cfg:         cfg,
		log:         d.Logger.With("component", "server"),
		baseDomain:  base,
		hubHostname: strings.ToLower(strings.TrimSuffix(pub.Hostname(), ".")),
		hubScheme:   scheme,
		hubOrigin:   normalizeOrigin(scheme, pub.Host),
		hubPort:     portOf(pub, scheme),
		publicURL:   scheme + "://" + strings.ToLower(pub.Host),
		pathMode:    cfg.Server.URLMode == "path",
		reserved:    cfg.Sites.Reserved,
		touches:     newTouchThrottle(),
		bg:          newBackground(),
		gatewayOn:   d.Gateway != nil && (cfg.Deploy.EmbeddedGateway == nil || *cfg.Deploy.EmbeddedGateway),
	}
	s.metricsOnHub = cfg.Telemetry.Metrics && cfg.Telemetry.MetricsListen == ""
	s.maxFixedLength = responseLengthCap()

	for _, p := range cfg.Server.TrustedProxies {
		n, err := config.ParseCIDROrIP(p)
		if err != nil {
			return nil, fmt.Errorf("server: server.trusted_proxies: %w", err)
		}
		s.trusted = append(s.trusted, n)
	}

	if len(d.DevSites) > 0 {
		s.devSites = make(map[string]string, len(d.DevSites))
		for name, dir := range d.DevSites {
			if err := sites.ValidateName(name, nil); err != nil {
				return nil, fmt.Errorf("server: dev site: %w", err)
			}
			if strings.TrimSpace(dir) == "" {
				return nil, fmt.Errorf("server: dev site %q: empty directory", name)
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return nil, fmt.Errorf("server: dev site %q: %w", name, err)
			}
			s.devSites[name] = abs
		}
	}

	s.docs = d.Docs
	if _, wrapped := d.Docs.(interface{ Unwrap() docstore.Store }); !wrapped {
		s.docs = docstore.WithLimits(d.Docs, cfg.Database.MaxDocBytes, cfg.Database.MaxDocsPerSite)
	}

	ttl := cfg.Storage.CacheTTL.D()
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	s.cache = newObjectCache(cfg.Storage.CacheBytes, ttl)
	s.siteCache = newExistsCache(ttl)
	s.limiter = ratelimit.New(cfg.RateLimit)

	var wsConns func() float64
	if d.Realtime != nil {
		hub := d.Realtime
		wsConns = func() float64 { return float64(hub.Stats().Connections) }
	}
	s.metrics = newMetrics(d.Version, wsConns)

	s.siteMux = s.siteRoutes()
	s.hubMux = s.hubRoutes()
	s.handler = s.middleware(http.HandlerFunc(s.route))
	return s, nil
}

// cloudRunMaxResponse is the largest response Cloud Run's HTTP/1 front end
// accepts with a Content-Length (32 MiB); larger responses must be streamed
// with chunked transfer encoding (or served over end-to-end HTTP/2).
const cloudRunMaxResponse = 32 << 20

// responseLengthCap returns the Content-Length limit of HTTP/1 responses
// for the platform the process runs on: cloudRunMaxResponse on Cloud Run
// (detected by K_SERVICE, which Cloud Run and Knative set for every
// service), else 0 (no limit). Streaming without a Content-Length works on
// every platform, so a false positive (plain Knative) costs nothing but the
// length hint.
func responseLengthCap() int64 {
	if os.Getenv("K_SERVICE") != "" {
		return cloudRunMaxResponse
	}
	return 0
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

// MetricsHandler returns the Prometheus handler (for telemetry.metrics_listen).
func (s *Server) MetricsHandler() http.Handler { return s.metrics.handler }

// OnWSFrame counts one websocket frame received from (dir "in") or written to
// (dir "out") a client. Pass it as realtime.Options.OnFrame (it is safe to
// call before New returns through a closure, and concurrently).
func (s *Server) OnWSFrame(dir string) {
	if s == nil || s.metrics == nil {
		return
	}
	switch dir {
	case "in", "out":
		s.metrics.wsMessages.WithLabelValues(dir).Inc()
	}
}

// ObserveDeploy counts a deploy for the deploys_total metric. Deploys through
// the embedded gateway are counted automatically; this is for other paths
// (the Claude connector's publishes, method "mcp"; a standalone gateway
// sharing the process).
func (s *Server) ObserveDeploy(method, result string) {
	if s == nil || s.metrics == nil {
		return
	}
	s.metrics.deploys.WithLabelValues(method, result).Inc()
}

// PurgeSite drops cached static files and the cached existence of a site, so
// a deploy is visible immediately instead of after storage.cache_ttl. Deploys
// through the embedded gateway purge automatically.
func (s *Server) PurgeSite(site string) {
	if s == nil {
		return
	}
	s.cache.purgePrefix(sites.SiteKey(site, ""))
	s.siteCache.forget(site)
}

// Close waits (bounded by ctx) for background work such as user-cache
// writes and audit records to finish. The handler keeps working after Close
// (work started meanwhile is waited for too); call it after the HTTP server
// has shut down and before closing Deps.Docs.
func (s *Server) Close(ctx context.Context) error {
	if err := s.bg.wait(ctx); err != nil {
		return fmt.Errorf("server: waiting for background work: %w", err)
	}
	return nil
}
