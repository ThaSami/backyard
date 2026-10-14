package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/version"
)

// GatewayOptions configures a Gateway.
type GatewayOptions struct {
	Config   *config.Config
	Store    blob.Store
	Registry *sites.Registry
	// Auth authenticates requests (the server's Authenticator when embedded,
	// auth.NewBearer for the standalone gateway / Lambda).
	Auth auth.Authenticator
	// BaseURL is the absolute URL the gateway is reachable at (used to build
	// self-hosted upload URLs and Info.GatewayURL), e.g.
	// https://backyard.acme.com/__backyard/api/deploy
	BaseURL string
	// OnDeleteSite is called after a site is deleted (e.g. to drop its
	// database documents). Optional.
	OnDeleteSite func(ctx context.Context, site string) error
	Logger       *slog.Logger
	// Version reported in Info.
	Version string
}

// EmbeddedPath is where the server mounts the embedded gateway.
const EmbeddedPath = "/__backyard/api/deploy"

const (
	// maxOwnersBody bounds POST /sites/{site}/owners bodies.
	maxOwnersBody = 64 << 10
	// maxOwners bounds the number of owners of a site.
	maxOwners = 100
	// lockStripes is the number of per-site mutex stripes.
	lockStripes = 64
)

// Gateway serves the deploy protocol. Mount it with the base path stripped:
// requests arrive as "/info", "/sites/…", "/uploads/…".
type Gateway struct {
	opts GatewayOptions

	cfg       *config.Config
	store     blob.Store
	reg       *sites.Registry
	log       *slog.Logger
	mux       *http.ServeMux
	baseURL   string // "" → derived from each request
	deployKey []byte
	uploadKey []byte
	limiter   *actorLimiter
	trusted   []*net.IPNet
	locks     [lockStripes]sync.Mutex
	now       func() time.Time
}

var _ http.Handler = (*Gateway)(nil)

// NewGateway validates options and returns a Gateway.
//
// When BaseURL is empty, deploy.gateway_url is used; when that is empty too,
// the gateway derives its URL from each request's Host (useful behind an AWS
// Lambda function URL whose address is not known in advance).
func NewGateway(opts GatewayOptions) (*Gateway, error) {
	if opts.Config == nil {
		return nil, errors.New("deploy: GatewayOptions.Config is required")
	}
	if opts.Store == nil {
		return nil, errors.New("deploy: GatewayOptions.Store is required")
	}
	if opts.Auth == nil {
		return nil, errors.New("deploy: GatewayOptions.Auth is required")
	}
	if len(opts.Config.Server.Secret) < 32 {
		return nil, errors.New("deploy: server.secret must be at least 32 characters")
	}
	if opts.Registry == nil {
		opts.Registry = sites.NewRegistry(opts.Store, opts.Config)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = version.Version
	}
	base := strings.TrimSuffix(strings.TrimSpace(opts.BaseURL), "/")
	if base == "" {
		base = strings.TrimSuffix(strings.TrimSpace(opts.Config.Deploy.GatewayURL), "/")
	}
	if base != "" {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("deploy: BaseURL must be an absolute http(s) URL, got %q", base)
		}
	}
	opts.BaseURL = base

	g := &Gateway{
		opts:      opts,
		cfg:       opts.Config,
		store:     opts.Store,
		reg:       opts.Registry,
		log:       opts.Logger.With("component", "deploy"),
		baseURL:   base,
		deployKey: keys.Derive(opts.Config.Server.Secret, keys.Deploy),
		uploadKey: keys.Derive(opts.Config.Server.Secret, keys.Upload),
		now:       time.Now,
	}
	if !opts.Config.RateLimit.Disabled && opts.Config.RateLimit.DeploysPerHour > 0 {
		g.limiter = newActorLimiter(opts.Config.RateLimit.DeploysPerHour)
	}
	for _, p := range opts.Config.Server.TrustedProxies {
		if n, err := config.ParseCIDROrIP(p); err == nil {
			g.trusted = append(g.trusted, n)
		}
	}
	g.routes()
	return g, nil
}

func (g *Gateway) routes() {
	mux := http.NewServeMux()
	type endpoint struct {
		method string
		h      http.HandlerFunc
	}
	// add registers method-specific handlers plus a method-less fallback
	// that answers other methods with a JSON 405.
	add := func(path string, eps ...endpoint) {
		var allowed []string
		for _, ep := range eps {
			allowed = append(allowed, ep.method)
			if ep.method == http.MethodGet {
				allowed = append(allowed, http.MethodHead)
			}
			mux.HandleFunc(ep.method+" "+path, ep.h)
		}
		allow := strings.Join(allowed, ", ")
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Allow", allow)
			writeError(w, r, g.log, errf(http.StatusMethodNotAllowed, CodeBadRequest, "method %s not allowed (allowed: %s)", r.Method, allow))
		})
	}
	get := func(h http.HandlerFunc) endpoint { return endpoint{http.MethodGet, h} }
	post := func(h http.HandlerFunc) endpoint { return endpoint{http.MethodPost, h} }

	add("/info", get(g.handleInfo))
	add("/me", get(g.handleMe))
	add("/sites", get(g.handleListSites))
	add("/sites/{site}", get(g.handleGetSite), endpoint{http.MethodDelete, g.handleDeleteSite})
	add("/sites/{site}/files", get(g.handleFiles))
	add("/sites/{site}/files/{path...}", get(g.handleReadFile))
	add("/sites/{site}/owners", post(g.handleOwners))
	add("/sites/{site}/refresh", post(g.handleRefresh))
	add("/sites/{site}/deploys", post(g.handleCreateDeploy))
	add("/sites/{site}/deploys/{id}/commit", post(g.handleCommit))
	add("/uploads/{token}", endpoint{http.MethodPut, g.handleUpload})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, g.log, notFound("no such endpoint: %s %s", r.Method, truncate(r.URL.Path, 200)))
	})
	g.mux = mux
}

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if v := recover(); v != nil {
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			g.log.ErrorContext(r.Context(), "deploy gateway panic", "panic", v, "stack", string(debug.Stack()))
			writeError(w, r, g.log, internalErr(fmt.Errorf("panic: %v", v)))
		}
	}()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// ServeMux would redirect unclean paths to their cleaned form, but the
	// Location would miss the mount prefix; valid requests are always clean.
	if p := r.URL.Path; p == "" || p[0] != '/' || cleanPath(p) != p {
		writeError(w, r, g.log, badRequest("invalid request path"))
		return
	}
	if !csrfOK(r) {
		writeError(w, r, g.log, forbidden(`missing "X-Backyard: 1" header`))
		return
	}
	g.mux.ServeHTTP(w, r)
}

// cleanPath mirrors ServeMux's path cleaning (keeping a trailing slash).
func cleanPath(p string) string {
	np := path.Clean(p)
	if strings.HasSuffix(p, "/") && np != "/" {
		np += "/"
	}
	return np
}

// csrfOK enforces the custom-header CSRF defence. State-changing requests
// must send "X-Backyard: 1" (cross-site forms cannot set custom headers, and
// cross-origin scripts are stopped by the CORS preflight the gateway never
// answers). A bearer token is not an ambient credential, so a request that
// carries one is exempt — unless it shows browser cross-site signals
// (Origin / Sec-Fetch-Site), since a proxy may inject bearer tokens into
// browser traffic. Upload URLs are exempt: their signed token is the
// capability.
func csrfOK(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if strings.HasPrefix(r.URL.Path, "/uploads/") || r.Header.Get("X-Backyard") == "1" {
		return true
	}
	if !hasBearer(r) || r.Header.Get("Origin") != "" {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "none", "same-origin":
		return true
	}
	return false
}

// hasBearer reports whether r carries an "Authorization: Bearer <token>"
// header (browsers never send those on their own, unlike Basic credentials).
func hasBearer(r *http.Request) bool {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	return ok && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(tok) != ""
}

// Info returns the /info document.
func (g *Gateway) Info() Info { return g.info(g.baseURL) }

func (g *Gateway) info(base string) Info {
	c := g.cfg
	inf := Info{
		Name:        c.Server.Name,
		Version:     g.opts.Version,
		PublicURL:   c.Server.PublicURL,
		BaseDomain:  c.Server.BaseDomain,
		URLMode:     c.Server.URLMode,
		GatewayURL:  base,
		Methods:     append([]string{}, c.Deploy.Methods...),
		RsyncTarget: c.Deploy.RsyncTarget,
		Limits: Limits{
			MaxSiteBytes: c.Sites.MaxSiteBytes,
			MaxFiles:     c.Sites.MaxFiles,
			MaxFileBytes: c.Sites.MaxFileBytes,
		},
		Auth: AuthInfo{Mode: c.Auth.Mode, AnonymousDeploy: g.anonymousDeploy()},
	}
	if gw := strings.TrimSuffix(c.Deploy.GatewayURL, "/"); gw != "" {
		inf.GatewayURL = gw
	}
	if len(inf.Methods) == 0 {
		inf.Methods = []string{"api"}
	}
	switch c.Auth.Mode {
	case auth.ModeOIDC, auth.ModeIAP, auth.ModeALB, auth.ModeHeader:
		if c.Server.PublicURL != "" {
			inf.Auth.CLILoginURL = strings.TrimSuffix(c.Server.PublicURL, "/") + auth.CLIPath
		}
	}
	if o := c.Auth.OIDC; o.DeviceClientID != "" && o.Issuer != "" {
		inf.Auth.Device = &DeviceInfo{Issuer: o.Issuer, ClientID: o.DeviceClientID, Scopes: append([]string{}, o.DeviceScopes...)}
	}
	if c.Deploy.ExposeStorage == nil || *c.Deploy.ExposeStorage {
		d := g.store.Describe()
		inf.Storage = &d
	}
	return inf
}

// anonymousDeploy reports whether deploys need no token.
func (g *Gateway) anonymousDeploy() bool {
	return g.cfg.Auth.Mode == auth.ModeNone && g.cfg.Deploy.AllowAnonymous
}

// requestBase returns the gateway's base URL for r. Without a configured
// URL it is derived from the request: scheme (TLS or X-Forwarded-Proto),
// Host, and the mount prefix that http.StripPrefix removed, if any.
func (g *Gateway) requestBase(r *http.Request) string {
	if g.baseURL != "" {
		return g.baseURL
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + mountPrefix(r)
}

// mountPrefix returns the escaped path prefix stripped from r before it
// reached the gateway (r.RequestURI keeps the original path), or "".
func mountPrefix(r *http.Request) string {
	if r.RequestURI == "" || r.RequestURI == "*" {
		return ""
	}
	orig, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		return ""
	}
	full, rest := orig.EscapedPath(), r.URL.EscapedPath()
	prefix, ok := strings.CutSuffix(full, rest)
	if !ok || prefix == "" || prefix[0] != '/' || strings.ContainsAny(prefix, "?#") {
		return ""
	}
	return strings.TrimSuffix(prefix, "/")
}

// ---------------------------------------------------------------------------
// Authentication and authorization

// access is the level of access a request needs.
type access int

const (
	// accessRead: reading sites, files and /me.
	accessRead access = iota
	// accessDeploy: creating/committing deploys and refreshing a site;
	// anonymous guests qualify when deploy.allow_anonymous is set.
	accessDeploy
	// accessManage: deleting sites and changing owners; always needs a
	// real (non-anonymous) identity.
	accessManage
)

// authenticate resolves the user and checks it is allowed the given access
// level (the per-site ownership policy is checked separately).
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request, need access) (*identity.User, *apiError) {
	bearer := hasBearer(r)
	u, err := g.opts.Auth.Authenticate(w, r)
	switch {
	case err == nil && u != nil:
	case err == nil || errors.Is(err, auth.ErrUnauthenticated):
		// The bearer-only authenticator (standalone gateway) has no guest
		// identity; in mode none an unauthenticated caller is a guest.
		if g.cfg.Auth.Mode == auth.ModeNone && !bearer {
			u = g.guest(r)
			break
		}
		msg := "authentication required (run: backyard login)"
		if bearer {
			msg = "invalid or expired token (run: backyard login)"
		}
		return nil, g.unauthenticated(msg)
	case errors.Is(err, auth.ErrForbidden):
		return nil, forbidden("access denied for this account")
	default:
		return nil, internalErr(fmt.Errorf("authenticate: %w", err))
	}
	// A bearer token that resolves to a guest is a valid API token minted
	// for an anonymous identity (auth.mode none lets guests mint tokens);
	// an invalid token never gets here (Authenticate fails). Guests follow
	// the guest rules either way.
	if u.Anonymous {
		what := ""
		switch {
		case need == accessDeploy && !g.anonymousDeploy():
			what = "deploy"
		case need == accessManage:
			what = "delete sites or change owners"
		}
		if what != "" {
			if bearer {
				return nil, g.unauthenticated("this token belongs to an anonymous guest, which cannot " + what +
					"; use a personal or static token (run: backyard login --token <token>)")
			}
			return nil, g.unauthenticated("a token is required to " + what + " (run: backyard login)")
		}
	}
	return u, nil
}

func (g *Gateway) unauthenticated(msg string) *apiError {
	e := errf(http.StatusUnauthorized, CodeUnauthenticated, "%s", msg)
	e.loginURL = g.opts.Auth.LoginURL(strings.TrimSuffix(g.cfg.Server.PublicURL, "/") + "/")
	return e
}

// guest returns a synthetic anonymous identity keyed by the client address.
func (g *Gateway) guest(r *http.Request) *identity.User {
	h := fnv.New64a()
	_, _ = io.WriteString(h, g.clientIP(r))
	id := fmt.Sprintf("%016x", h.Sum64())
	return &identity.User{ID: "anon:" + id, Name: "Guest " + id[:4], Anonymous: true}
}

// isAdmin reports admin rights from the identity or configuration.
func (g *Gateway) isAdmin(u *identity.User) bool {
	if u == nil || u.Anonymous {
		return false
	}
	return u.Admin || g.cfg.Auth.IsAdmin(u.ID, u.Email, u.Groups)
}

// ownershipOn reports whether sites.ownership is "owner".
func (g *Gateway) ownershipOn() bool { return g.cfg.Sites.Ownership == "owner" }

// authorizeSite applies the ownership policy to a write on site and returns
// the stored meta document (nil when none exists).
func (g *Gateway) authorizeSite(ctx context.Context, u *identity.User, site string) (*sites.Meta, *apiError) {
	meta, err := g.reg.GetMeta(ctx, site)
	switch {
	case errors.Is(err, sites.ErrNotFound):
		meta, err = nil, nil
	case errors.Is(err, sites.ErrCorrupt):
		// The owners are unknown. Without ownership, or for an admin, the
		// write goes ahead and rewrites the document; otherwise fail closed.
		g.log.WarnContext(ctx, "deploy: site metadata is unreadable", "site", site, "error", err)
		if g.ownershipOn() && !g.isAdmin(u) {
			return nil, forbidden("the metadata of site %q is unreadable, so its owners are unknown; an admin must redeploy, refresh or delete it", site)
		}
		meta, err = nil, nil
	}
	if err != nil {
		return nil, internalErr(err)
	}
	if !g.ownershipOn() || g.isAdmin(u) || meta == nil || len(meta.Owners) == 0 {
		return meta, nil
	}
	if !u.Anonymous && meta.IsOwner(u.ID, u.Email) {
		return meta, nil
	}
	return nil, forbidden("site %q is owned by %s; only owners and admins can change it", site, strings.Join(meta.Owners, ", "))
}

// ownerID is the identifier recorded for u in Meta.Owners.
func ownerID(u *identity.User) string { return u.Actor() }

// lockSite serializes writes to one site within this instance.
func (g *Gateway) lockSite(site string) func() {
	h := fnv.New32a()
	_, _ = io.WriteString(h, site)
	m := &g.locks[h.Sum32()%lockStripes]
	m.Lock()
	return m.Unlock
}

// siteName validates the {site} path value. withReserved also rejects
// reserved names (for operations that create sites).
func (g *Gateway) siteName(r *http.Request, withReserved bool) (string, *apiError) {
	name := r.PathValue("site")
	var reserved []string
	if withReserved {
		reserved = g.cfg.Sites.Reserved
	}
	if err := sites.ValidateName(name, reserved); err != nil {
		return "", badRequest("%v", strings.TrimPrefix(err.Error(), "sites: "))
	}
	return name, nil
}

// registryErr maps registry errors to API errors.
func registryErr(site string, err error) *apiError {
	switch {
	case errors.Is(err, sites.ErrNotFound):
		return notFound("site %q not found", site)
	case errors.Is(err, sites.ErrInvalidName):
		return badRequest("invalid site name %q", site)
	}
	return internalErr(err)
}

// audit logs a security-relevant action.
func (g *Gateway) audit(r *http.Request, u *identity.User, action, site string, attrs ...any) {
	args := append([]any{
		"audit", true,
		"action", action,
		"actor", u.Actor(),
		"site", site,
		"ip", g.clientIP(r),
	}, attrs...)
	g.log.InfoContext(r.Context(), "audit: "+action, args...)
}

// clientIP returns the caller's address, honouring X-Forwarded-For only
// when the peer is a trusted proxy.
func (g *Gateway) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !g.isTrusted(host) {
		return host
	}
	xff := r.Header.Values("X-Forwarded-For")
	parts := strings.Split(strings.Join(xff, ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if ip == "" {
			continue
		}
		if !g.isTrusted(ip) {
			return ip
		}
		host = ip
	}
	return host
}

func (g *Gateway) isTrusted(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	for _, n := range g.trusted {
		if n.Contains(p) {
			return true
		}
	}
	return false
}

// rateKey identifies the bucket charged for u's deploys.
func (g *Gateway) rateKey(r *http.Request, u *identity.User) string {
	if u.Anonymous {
		return "ip:" + g.clientIP(r)
	}
	return "user:" + u.ID
}

// chargeDeploy takes one token from u's deploys_per_hour bucket. Creating a
// deploy and refreshing a site (the commit step of a direct sync, which
// re-reads every file) are charged; commits are not, so a deploy costs one.
func (g *Gateway) chargeDeploy(r *http.Request, u *identity.User) *apiError {
	if g.limiter == nil {
		return nil
	}
	ok, wait := g.limiter.allow(g.rateKey(r, u), g.now())
	if ok {
		return nil
	}
	e := errf(http.StatusTooManyRequests, CodeRateLimited, "too many deploys (limit %d per hour); retry in %s",
		g.cfg.RateLimit.DeploysPerHour, wait.Round(time.Second))
	e.retryAfter = wait
	return e
}

// decodeJSON reads a JSON body of at most limit bytes into v.
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any) *apiError {
	body := http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return tooLarge("request body exceeds %d bytes", limit)
		}
		if errors.Is(err, io.EOF) {
			return badRequest("empty request body (want JSON)")
		}
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rate limiting

// actorLimiter is a per-actor token bucket (deploys per hour).
type actorLimiter struct {
	mu        sync.Mutex
	perHour   int
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newActorLimiter(perHour int) *actorLimiter {
	return &actorLimiter{perHour: perHour, buckets: map[string]*bucket{}}
}

// allow takes one token for key, returning the wait time when none is left.
func (l *actorLimiter) allow(key string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 1024 && now.Sub(l.lastSweep) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 2*time.Hour {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(rate.Every(time.Hour/time.Duration(l.perHour)), l.perHour)}
		l.buckets[key] = b
	}
	b.seen = now
	res := b.lim.ReserveN(now, 1)
	if !res.OK() {
		return false, time.Hour
	}
	if d := res.DelayFrom(now); d > 0 {
		res.CancelAt(now)
		return false, d
	}
	return true, 0
}
