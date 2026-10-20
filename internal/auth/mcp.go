package auth

// The authorization server of the remote MCP connector (SPEC §8.4): Backyard
// issues its own OAuth 2.1 tokens to MCP clients such as Claude, and signs
// the browser in through the configured auth mode at AuthorizePath.
//
// This file holds the API that internal/mcpremote, internal/server and
// internal/app use (MCP, Grant, MCPOf, ErrInvalidToken and the paths). The
// implementation lives in mcp_*.go:
// discovery documents (mcp_metadata.go), client identification and the
// redirect and resource rules (mcp_clients.go), the authorize endpoint and
// consent page (mcp_authorize.go), the token, revocation and registration
// endpoints and the token formats (mcp_token.go), and the grant store
// (mcp_grants.go).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
	"github.com/thasami/backyard/internal/netguard"
)

// AuthorizePath is served by Authenticator.Handler() (GET, POST).
const AuthorizePath = "/__backyard/auth/authorize"

// Paths of the connector (SPEC §8.4, §14.1). Only AuthorizePath stays behind
// an identity-aware proxy; the others authenticate by themselves.
const (
	MCPPath              = "/__backyard/mcp"
	OAuthPathPrefix      = "/__backyard/oauth/"
	OAuthRegisterPath    = OAuthPathPrefix + "register"
	OAuthTokenPath       = OAuthPathPrefix + "token"
	OAuthRevokePath      = OAuthPathPrefix + "revoke"
	ASMetadataPath       = "/.well-known/oauth-authorization-server"
	ResourceMetadataPath = "/.well-known/oauth-protected-resource" + MCPPath
)

// Fixed limits and lifetimes (SPEC §11).
const (
	mcpCodeTTL          = 300 * time.Second  // authorization code
	mcpConsentTTL       = 600 * time.Second  // consent CSRF token
	mcpConsentSkew      = 60 * time.Second   // accepted past the CSRF expiry
	mcpAccessTTL        = 3600 * time.Second // access token
	mcpRefreshGrace     = 60 * time.Second   // re-presenting a just-rotated refresh token
	mcpPendingRetention = 600 * time.Second  // unredeemed grants, after the code expired
	maxAuthorizeURI     = 2048               // raw path + query of an authorize GET
	maxOAuthState       = 512
	maxDCRClientID      = 1024
	maxDCRRedirectURIs  = 4
	maxRedirectURIBytes = 512
	maxClientNameRunes  = 64
	maxOAuthBody        = 16 << 10 // register, token, revoke, consent POST
	maxMCPBearer        = 4096
	maxGrantList        = 200
)

// Token prefixes (SPEC §8.4.8).
const (
	prefixDCRClient = "bydc_"
	prefixCode      = "byac_"
	prefixRefresh   = "byrt_"
	prefixAccess    = "byat_"
)

// Client kinds of a grant (SPEC §8.4.3): an allowlisted client metadata
// document, a valid client metadata URL that is not allowlisted (never
// fetched; loopback redirects only), a dynamic registration.
const (
	clientKindCIMD     = "metadata_document"
	clientKindUnlisted = "unlisted_metadata_document"
	clientKindDCR      = "registered"
)

// Scopes: one capability set; offline_access is only advertised.
const (
	scopeMCP     = "mcp"
	scopeOffline = "offline_access"
)

// ErrInvalidToken marks an MCP access token that does not authenticate:
// answer 401 with MCP.Challenge(err).
var ErrInvalidToken = errors.New("auth: invalid MCP access token")

// The fixed error_description values of an invalid access token (SPEC §14.1).
const (
	descMalformed  = "malformed token"
	descExpired    = "token expired"
	descRevoked    = "token revoked"
	descAudience   = "token not issued for this server"
	descPolicy     = "access removed by policy"
	notEnabledText = "the Claude connector is not enabled on this server"
)

// tokenError is an ErrInvalidToken with its error_description.
type tokenError struct{ desc string }

func (e *tokenError) Error() string        { return "auth: invalid MCP access token: " + e.desc }
func (e *tokenError) Is(target error) bool { return target == ErrInvalidToken }

// MCP is the connector's authorization server. It is nil when mcp.enabled is
// false.
type MCP struct {
	a    *authenticator
	cfg  *config.Config
	docs docstore.Store
	log  *slog.Logger
	now  func() time.Time

	issuer   string // P: canonical hub origin
	host     string // H: hostname of P
	resource string // R
	prm      string // M
	name     string // server.name (PRM resource_name)

	clients      []string        // effective mcp.clients, in order
	clientSet    map[string]bool // the same, for exact lookup
	redirectSet  map[string]bool // effective mcp.redirect_uris (exact https entries)
	origins      map[string]bool // origins accepted at /__backyard/mcp besides P
	family       time.Duration   // refresh family lifetime (auth.token_ttl)
	maxBody      int64
	clientKey    []byte
	consentKey   []byte
	codeKey      []byte
	refreshKey   []byte
	accessKey    []byte
	cimdClient   *http.Client
	cimdMu       sync.Mutex
	cimd         map[string]*cimdEntry
	limit        func(key string) (bool, time.Duration) // Options.MCPAllow
	bg           sync.WaitGroup                         // background pruning (tests wait for it)
	pruneRunning chan struct{}                          // a token: at most one prune at a time
}

// Grant is one connection: the consent of a user to one MCP client.
type Grant struct {
	ID, ClientID, ClientName, ClientKind, ClientHost, RedirectURI, Scope, Resource string
	// User is the snapshot taken at consent: ID, Email, Name, Groups, Anonymous.
	User                 identity.User
	CreatedAt, ExpiresAt time.Time
}

// newMCP builds the authorization server for a full (non bearer-only)
// authenticator with cfg.MCP.Enabled.
func newMCP(a *authenticator, opts Options) (*MCP, error) {
	cfg := a.cfg
	if opts.Docs == nil {
		return nil, errors.New("auth: mcp.enabled requires a document store for the connector's grants")
	}
	p, host, err := mcpIssuer(cfg.Server.PublicURL)
	if err != nil {
		return nil, err
	}
	m := &MCP{
		a: a, cfg: cfg, docs: opts.Docs, log: a.log, now: a.now,
		issuer: p, host: host, resource: p + MCPPath, prm: p + ResourceMetadataPath,
		name:         strings.TrimSpace(cfg.Server.Name),
		clientSet:    map[string]bool{},
		redirectSet:  map[string]bool{},
		origins:      map[string]bool{},
		family:       cfg.Auth.TokenTTL.D(),
		cimd:         map[string]*cimdEntry{},
		limit:        opts.MCPAllow,
		pruneRunning: make(chan struct{}, 1),
	}
	if m.limit == nil {
		m.limit = allowAll
	}
	if m.name == "" {
		m.name = "Backyard"
	}
	if m.family <= 0 {
		m.family = DefaultTokenTTL
	}
	clients, redirects := cfg.MCP.Clients, cfg.MCP.RedirectURIs
	if clients == nil {
		clients = config.DefaultMCPClients
	}
	if redirects == nil {
		redirects = config.DefaultMCPRedirectURIs
	}
	for _, c := range clients {
		if c = strings.TrimSpace(c); c != "" && !m.clientSet[c] {
			m.clientSet[c] = true
			m.clients = append(m.clients, c)
			if o := originOf(c); o != "" {
				m.origins[o] = true
			}
		}
	}
	for _, r := range redirects {
		if r = strings.TrimSpace(r); r != "" {
			m.redirectSet[r] = true
			if strings.HasPrefix(r, "https://") {
				if o := originOf(r); o != "" {
					m.origins[o] = true
				}
			}
		}
	}
	maxPublish := cfg.MCP.MaxPublishBytes
	if maxPublish <= 0 {
		maxPublish = config.DefaultMCPMaxPublishBytes
	}
	m.maxBody = 2*maxPublish + 1<<20
	secret := cfg.Server.Secret
	m.clientKey = keys.Derive(secret, keys.MCPClient)
	m.consentKey = keys.Derive(secret, keys.MCPConsent)
	m.codeKey = keys.Derive(secret, keys.MCPCode)
	m.refreshKey = keys.Derive(secret, keys.MCPRefresh)
	m.accessKey = keys.Derive(secret, keys.MCPAccess)
	m.cimdClient = cimdHTTPClient(opts.MCPHTTPClient)
	return m, nil
}

// cimdHTTPClient returns the client that fetches client metadata documents:
// the SSRF-guarded transport by default; redirects are never followed.
func cimdHTTPClient(c *http.Client) *http.Client {
	var out http.Client
	if c != nil {
		out = *c
	} else {
		out.Transport = netguard.NewTransport(http.ProxyFromEnvironment)
	}
	out.Timeout = cimdTimeout
	out.Jar = nil
	out.CheckRedirect = func(*http.Request, []*http.Request) error { return errCIMDRedirect }
	return &out
}

// mcpIssuer derives P and H from server.public_url: lowercase scheme and
// host, no default port, no path. http is accepted only on a loopback host
// (config validation guarantees both).
func mcpIssuer(publicURL string) (p, host string, err error) {
	u, perr := url.Parse(strings.TrimSpace(publicURL))
	if perr != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
		(u.Scheme == "http" && !config.LoopbackHost(u.Hostname())) {
		return "", "", fmt.Errorf("auth: mcp.enabled requires server.public_url to be https (http only on loopback hosts) without a path, got %q", publicURL)
	}
	host = strings.ToLower(u.Hostname())
	hp := strings.ToLower(u.Host)
	if port := u.Port(); port == defaultPort("", u.Scheme) {
		hp = strings.TrimSuffix(hp, ":"+port)
	}
	return u.Scheme + "://" + hp, host, nil
}

// MCPOf returns the authorization server of a (nil when MCP is disabled, a is
// bearer-only, or a is not built by New).
func MCPOf(a Authenticator) *MCP {
	if x, ok := a.(*authenticator); ok && x != nil && !x.bearerOnly {
		return x.mcp
	}
	return nil
}

// Issuer returns P, the canonical hub origin.
func (m *MCP) Issuer() string {
	if m == nil {
		return ""
	}
	return m.issuer
}

// Resource returns R, P + "/__backyard/mcp".
func (m *MCP) Resource() string {
	if m == nil {
		return ""
	}
	return m.resource
}

// ResourceMetadataURL returns M, the protected resource metadata URL.
func (m *MCP) ResourceMetadataURL() string {
	if m == nil {
		return ""
	}
	return m.prm
}

// MaxBodyBytes is the MCP request body limit: 2 × mcp.max_publish_bytes + 1 MiB.
func (m *MCP) MaxBodyBytes() int64 {
	if m == nil {
		return 0
	}
	return m.maxBody
}

// AllowedOrigin reports whether a present Origin header is accepted at
// /__backyard/mcp besides the hub origin, which the caller checks
// (SPEC §14.1, gate step 3): the origins of the https entries of
// mcp.redirect_uris and of the entries of mcp.clients (P is accepted too).
func (m *MCP) AllowedOrigin(origin string) bool {
	if m == nil {
		return false
	}
	o := originOf(strings.TrimSpace(origin))
	return o != "" && (o == m.issuer || m.origins[o])
}

// ServeMetadata serves both discovery documents (SPEC §8.4.2).
func (m *MCP) ServeMetadata(w http.ResponseWriter, r *http.Request) { m.serveMetadata(w, r) }

// ServeOAuth serves /__backyard/oauth/{register,token,revoke}.
func (m *MCP) ServeOAuth(w http.ResponseWriter, r *http.Request) { m.serveOAuth(w, r) }

// VerifyAccessToken returns the re-gated user and the grant of an MCP access
// token. errors.Is(err, ErrInvalidToken) means 401 invalid_token (Challenge
// gives the header); any other error is an infrastructure failure (503).
func (m *MCP) VerifyAccessToken(ctx context.Context, raw string) (*identity.User, *Grant, error) {
	if m == nil {
		return nil, nil, &tokenError{desc: descMalformed}
	}
	return m.verifyAccessToken(ctx, raw)
}

// Challenge returns the exact WWW-Authenticate value for err (SPEC §14.1); a nil
// err gives the missing-token form.
func (m *MCP) Challenge(err error) string {
	if m == nil {
		return ""
	}
	if err == nil {
		return `Bearer resource_metadata="` + m.prm + `", scope="` + scopeMCP + `"`
	}
	desc := descMalformed
	var te *tokenError
	if errors.As(err, &te) {
		desc = te.desc
	}
	return `Bearer error="invalid_token", error_description="` + desc + `", resource_metadata="` + m.prm +
		`", scope="` + scopeMCP + `"`
}

// Grants lists the live grants of userID, newest first, at most 200.
func (m *MCP) Grants(ctx context.Context, userID string) ([]Grant, error) {
	if m == nil {
		return nil, nil
	}
	return m.listGrants(ctx, userID)
}

// GrantByID returns a grant, or nil, nil when it does not exist.
func (m *MCP) GrantByID(ctx context.Context, id string) (*Grant, error) {
	if m == nil {
		return nil, nil
	}
	g, err := m.loadGrant(ctx, id)
	if err != nil || g == nil {
		return nil, err
	}
	out := g.public()
	return &out, nil
}

// Revoke deletes one grant; via names the lever (rfc7009, hub, policy, replay).
func (m *MCP) Revoke(ctx context.Context, id string, actor *identity.User, via string) (bool, error) {
	if m == nil {
		return false, nil
	}
	return m.revokeGrant(ctx, nil, id, actor, via)
}

// Close waits for the background work of the authorization server (grant
// pruning) to finish, or for ctx to end. Call it after the HTTP server has
// stopped and before the document store is closed.
func (m *MCP) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		m.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// clientIPKey is the context key of WithClientIP.
type clientIPKey struct{}

// WithClientIP records the client address of the request behind ctx for the
// audit records of Revoke and RevokeUser (the grants API passes the address
// the server resolved).
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// RevokeUser deletes every grant of userID and returns how many there were.
func (m *MCP) RevokeUser(ctx context.Context, userID string, actor *identity.User, via string) (int, error) {
	if m == nil {
		return 0, nil
	}
	return m.revokeUserGrants(ctx, userID, actor, via)
}

// MediaType returns the media type of a Content-Type value, lowercased, with
// its parameters ignored, malformed ones included: the connector's endpoints
// compare only this, as the Worker does (SPEC §8.4, §14.1).
func MediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}
