// Package mcpremote serves the remote MCP endpoint of the Claude connector,
// POST /__backyard/mcp (SPEC §14.1): Streamable HTTP, stateless, JSON
// responses only, protocol versions 2025-11-25 and 2025-06-18. It is a
// hand-written JSON-RPC handler (the Cloudflare edition's src/mcp.ts is its
// twin) over the shared tool catalog tools.json.
//
// The endpoint accepts only the MCP access tokens of Backyard's own
// authorization server (internal/auth, SPEC §8.4): no cookies, API tokens,
// static tokens or identity-proxy headers. Every request re-checks the
// grant, so a revoked connection stops at once. The tools read the sites
// and publish through the deploy gateway (deploy.Gateway.Publish); a
// docstore lease serializes the connector's publishes of a site across
// replicas.
package mcpremote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/mcpserver"
	"github.com/thasami/backyard/internal/ratelimit"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/version"
)

// AuthServer is what the endpoint needs from the connector's authorization
// server; *auth.MCP implements it.
type AuthServer interface {
	// VerifyAccessToken returns the re-gated user and the grant of a
	// token; errors.Is(err, auth.ErrInvalidToken) means 401, anything else
	// an infrastructure failure.
	VerifyAccessToken(ctx context.Context, raw string) (*identity.User, *auth.Grant, error)
	// Challenge is the exact WWW-Authenticate value for a token error (nil:
	// the missing-token form).
	Challenge(err error) string
	ResourceMetadataURL() string // M
	Resource() string            // R
	Issuer() string              // P, the hub origin
	// AllowedOrigin reports whether an Origin other than the hub's may
	// call the endpoint (the origins of mcp.redirect_uris and mcp.clients).
	AllowedOrigin(origin string) bool
	MaxBodyBytes() int64
}

var _ AuthServer = (*auth.MCP)(nil)

// Options configure the endpoint.
type Options struct {
	Config *config.Config
	// Auth is the authorization server (auth.MCPOf); nil or disabled
	// answers 404 everywhere.
	Auth  AuthServer
	Sites *sites.Registry // default: a registry over Blob
	Blob  blob.Store      // default: Sites.Store()
	// Docs holds the cross-replica publish lease (_system/_mcp_locks).
	Docs docstore.Store
	// Gateway publishes; nil when deploy.embedded_gateway is false.
	Gateway *deploy.Gateway
	// DocsFor returns backyard_docs topics (default mcpserver.DefaultDocs).
	DocsFor func(topic string) (string, error)
	// Allow is the per-user request limit (ratelimit.API) for the key
	// "user:<id>"; nil means unlimited.
	Allow func(key string) (bool, time.Duration)
	// OnPublish runs after every backyard_publish that reached the
	// gateway, ok or not (PurgeSite, ObserveDeploy("mcp", …)).
	OnPublish func(site string, ok bool)
	// RequestOrigin returns the client-visible origin (scheme://host[:port],
	// lowercase) of r, honouring trusted proxy headers; default: from r.TLS
	// and r.Host.
	RequestOrigin func(r *http.Request) string
	// ClientIP returns the caller's address for audit records; default:
	// the host of r.RemoteAddr.
	ClientIP func(r *http.Request) string
	Version  string // serverInfo.version (default version.Version)
	Logger   *slog.Logger
	Now      func() time.Time
}

// Protocol versions (SPEC §14.1).
const (
	versionLatest   = "2025-11-25"
	versionFallback = "2025-06-18" // also the version of requests without the header
)

var supportedVersions = []string{versionLatest, versionFallback}

const (
	// requestDeadline bounds reading the body and writing the answer.
	requestDeadline = 5 * time.Minute
	// bodyGrace and minBodyRate pace the body: byte n is due bodyGrace +
	// n / minBodyRate after the body is first read, so a body sent slower
	// than that on average is cut off (and frees its admission slot).
	bodyGrace   = 10 * time.Second
	minBodyRate = 64 << 10 // bytes per second
	// largeBody is the size above which a request needs an admission slot.
	largeBody = 1 << 20
	// admissionSlots bounds concurrent large requests (their bodies are
	// held in memory); one user holds at most one.
	admissionSlots = 4
	// admissionWait is how long a large request waits for a slot.
	admissionWait = 30 * time.Second
	// defaultMaxBody is the body limit when the authorization server
	// reports none (2 × the default publish cap + 1 MiB).
	defaultMaxBody = 2*config.DefaultMCPMaxPublishBytes + 1<<20
)

// Messages of the gate.
const (
	msgNotEnabled   = "the Claude connector is not enabled on this server"
	msgUnauthorized = "connect this app to Backyard to sign in"
)

// handler serves the endpoint.
type handler struct {
	o         Options
	cfg       *config.Config
	auth      AuthServer
	reg       *sites.Registry
	store     blob.Store
	docs      docstore.Store
	gw        *deploy.Gateway
	docsFor   func(string) (string, error)
	log       *slog.Logger
	now       func() time.Time
	cat       *catalog
	tools     map[string]toolFunc
	hubOrigin string // P
	hubHost   string // H
	maxBody   int64
	slots     chan struct{}
	usersMu   sync.Mutex
	users     map[string]*userSlot // users waiting for or holding a slot
	admitWait time.Duration
	// bodyGrace, minBodyRate and releaseTimeout are constants but for
	// tests.
	bodyGrace      time.Duration
	minBodyRate    float64
	releaseTimeout time.Duration
	version        string
	// publish is Gateway.Publish (replaced by tests).
	publish func(ctx context.Context, req deploy.PublishRequest) (*deploy.PublishResult, *deploy.PublishError)
}

// New returns the endpoint handler. Mount it at /__backyard/mcp on the
// canonical hub host; it applies the gate of SPEC §14.1 itself.
func New(o Options) http.Handler {
	h, err := newHandler(o)
	if err != nil {
		panic(err) // the embedded catalog is static: covered by the tests
	}
	return h
}

func newHandler(o Options) (*handler, error) {
	cat, err := loadCatalog()
	if err != nil {
		return nil, err
	}
	h := &handler{
		o: o, cfg: o.Config, auth: o.Auth, reg: o.Sites, store: o.Blob, docs: o.Docs, gw: o.Gateway,
		docsFor: o.DocsFor, log: o.Logger, now: o.Now, cat: cat, version: o.Version,
		slots: make(chan struct{}, admissionSlots), users: map[string]*userSlot{}, admitWait: admissionWait,
		bodyGrace: bodyGrace, minBodyRate: minBodyRate, releaseTimeout: releaseTimeout,
	}
	if h.cfg == nil {
		h.cfg = &config.Config{}
	}
	if h.store == nil && h.reg != nil {
		h.store = h.reg.Store()
	}
	if h.reg == nil && h.store != nil {
		h.reg = sites.NewRegistry(h.store, h.cfg)
	}
	if h.docsFor == nil {
		h.docsFor = mcpserver.DefaultDocs
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	h.log = h.log.With("component", "mcp")
	if h.now == nil {
		h.now = time.Now
	}
	if h.version == "" {
		h.version = version.Version
	}
	if h.gw != nil {
		h.publish = h.gw.Publish
	}
	if h.auth != nil && h.auth.Resource() != "" {
		h.hubOrigin = normalizeOrigin(h.auth.Issuer())
		h.hubHost = hostnameOf(h.hubOrigin)
		h.maxBody = h.auth.MaxBodyBytes()
	} else {
		h.auth = nil
	}
	if h.maxBody <= 0 {
		h.maxBody = defaultMaxBody
	}
	h.tools = map[string]toolFunc{
		"backyard_docs":           h.toolDocs,
		"backyard_whoami":         h.toolWhoami,
		"backyard_list_sites":     h.toolListSites,
		"backyard_site_files":     h.toolSiteFiles,
		"backyard_read_site_file": h.toolReadSiteFile,
		"backyard_publish":        h.toolPublish,
	}
	for _, name := range cat.names {
		if h.tools[name] == nil {
			return nil, fmt.Errorf("mcpremote: tools.json lists %s, which is not implemented", name)
		}
	}
	if len(cat.names) != len(h.tools) {
		return nil, errors.New("mcpremote: a tool is implemented but not listed in tools.json")
	}
	return h, nil
}

// ServeHTTP runs the gate of SPEC §14.1, in its order.
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	// 1. Enabled, on the canonical hub host.
	if h.auth == nil {
		writeError(w, http.StatusNotFound, "not_found", msgNotEnabled)
		return
	}
	reqOrigin := h.requestOrigin(r)
	if hostnameOf(reqOrigin) != h.hubHost {
		writeError(w, http.StatusNotFound, "not_found", "not found") // as the OAuth paths and the Worker
		return
	}
	// 2. Until the body is read (step 9) the server's read and write
	// timeouts apply, also while an early rejection drains an unread body.
	// 3. Origin.
	if origin := r.Header.Get("Origin"); origin != "" && !h.originAllowed(origin, reqOrigin) {
		writeError(w, http.StatusForbidden, "forbidden", "requests from this origin are not accepted")
		return
	}
	// 4. Credentials: only Backyard MCP access tokens.
	authz := r.Header.Values("Authorization")
	if len(authz) > 1 || (len(authz) == 1 && severalCredentials(authz[0])) {
		hdr.Set("WWW-Authenticate", `Bearer error="invalid_request", resource_metadata="`+h.auth.ResourceMetadataURL()+`"`)
		writeError(w, http.StatusBadRequest, "bad_request", "send exactly one Authorization header")
		return
	}
	token := bearerToken(authz)
	if token == "" {
		hdr.Set("WWW-Authenticate", h.auth.Challenge(nil))
		writeError(w, http.StatusUnauthorized, "unauthenticated", msgUnauthorized)
		return
	}
	user, grant, err := h.auth.VerifyAccessToken(r.Context(), token)
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		hdr.Set("WWW-Authenticate", h.auth.Challenge(err))
		writeError(w, http.StatusUnauthorized, "unauthenticated", msgUnauthorized)
		return
	case err != nil || user == nil || grant == nil:
		id := requestID(w)
		h.log.ErrorContext(r.Context(), "mcp: token check failed", "error", err, "request_id", id)
		writeError(w, http.StatusServiceUnavailable, "internal", "Backyard could not check this connection right now (request "+id+"); try again")
		return
	}
	// 5. Per-user request limit.
	if h.o.Allow != nil {
		if ok, wait := h.o.Allow("user:" + user.ID); !ok {
			hdr.Set("Retry-After", fmt.Sprint(ratelimit.RetryAfterSeconds(wait)))
			writeError(w, http.StatusTooManyRequests, "rate_limited", "rate limit exceeded (api); retry later")
			return
		}
	}
	// 6. Method.
	if r.Method != http.MethodPost {
		hdr.Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	// 7. Media types.
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "bad_request", "send the JSON-RPC message as application/json")
		return
	}
	if accept := strings.Join(r.Header.Values("Accept"), ","); strings.TrimSpace(accept) != "" && !acceptsJSON(accept) {
		writeError(w, http.StatusBadRequest, "bad_request", "the Accept header must admit application/json (this server never streams)")
		return
	}
	// 8. Protocol version.
	protocol := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version"))
	if protocol == "" {
		protocol = versionFallback
	}
	if !supported(protocol) {
		writeRPC(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: nullID, Error: &rpcError{
			Code: codeUnsupportedVersion, Message: "Unsupported protocol version",
			Data: struct {
				Supported []string `json:"supported"`
				Requested string   `json:"requested"`
			}{supportedVersions, truncate(protocol, 100)},
		}})
		return
	}
	// 9. Body.
	tooLarge := fmt.Sprintf("MCP requests are limited to %d bytes; publish over several calls", h.maxBody)
	if r.ContentLength > h.maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", tooLarge)
		return
	}
	large := r.ContentLength < 0 || r.ContentLength > largeBody
	if large {
		release, ok := h.admit(r.Context(), user.ID)
		if !ok {
			hdr.Set("Retry-After", "5")
			writeError(w, http.StatusTooManyRequests, "rate_limited", "too many large MCP requests at once; retry in a few seconds")
			return
		}
		defer release()
	}
	// Authenticated (and admitted): the answer gets the deadline of a large
	// publish. A large body is paced; a small one is read under the
	// server's read timeout.
	rc := http.NewResponseController(w)
	start := time.Now()
	limit := start.Add(requestDeadline)
	_ = rc.SetWriteDeadline(limit)
	in := r.Body
	if large {
		in = &pacedBody{ReadCloser: r.Body, rc: rc, start: start, limit: limit, grace: h.bodyGrace, rate: h.minBodyRate, want: r.ContentLength}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, in, h.maxBody))
	// Once it is read, net/http's background read of the connection
	// inherits the read deadline: the publish's own.
	_ = rc.SetReadDeadline(limit)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", tooLarge)
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		return
	}
	// 10. JSON-RPC parse.
	msg, rerr := parseMessage(body)
	if rerr != nil {
		writeRPC(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: nullID, Error: rerr})
		return
	}
	// 11. Dispatch.
	if msg.kind != kindRequest {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	c := &call{r: r, user: user, grant: grant, protocol: protocol, reqID: requestID(w)}
	writeRPC(w, http.StatusOK, h.dispatch(r.Context(), c, msg))
}

// call is the context of one request.
type call struct {
	r        *http.Request
	user     *identity.User
	grant    *auth.Grant
	protocol string
	reqID    string
}

// dispatch answers a request; panics become -32603.
func (h *handler) dispatch(ctx context.Context, c *call, msg *message) (resp rpcResponse) {
	resp = rpcResponse{JSONRPC: "2.0", ID: msg.id}
	defer func() {
		if v := recover(); v != nil {
			h.log.ErrorContext(ctx, "mcp: panic", "method", msg.method, "panic", fmt.Sprint(v), "stack", string(debug.Stack()),
				"request_id", c.reqID)
			resp.Result, resp.Error = nil, internalError(c)
		}
	}()
	var result any
	var rerr *rpcError
	switch msg.method {
	case "initialize":
		result, rerr = h.initialize(msg.params)
	case "ping":
		result = struct{}{}
	case "tools/list":
		result = struct {
			Tools json.RawMessage `json:"tools"`
		}{h.cat.tools}
	case "tools/call":
		result, rerr = h.callTool(ctx, c, msg.params)
	default:
		rerr = newRPCError(codeMethodNotFound, "Method not found")
	}
	if rerr != nil {
		resp.Error = rerr
		return resp
	}
	b, err := marshal(result)
	if err != nil {
		h.log.ErrorContext(ctx, "mcp: encode result", "method", msg.method, "error", err, "request_id", c.reqID)
		resp.Error = internalError(c)
		return resp
	}
	resp.Result = b
	return resp
}

func internalError(c *call) *rpcError {
	return newRPCError(codeInternalError, "Internal error (request "+c.reqID+")")
}

// initialize negotiates the protocol version.
func (h *handler) initialize(params json.RawMessage) (any, *rpcError) {
	// A map, not a struct: encoding/json would match "ProtocolVersion" too,
	// and the member is the exact key (as on the Worker).
	var p map[string]json.RawMessage
	var v string
	if !isJSONObject(params) || json.Unmarshal(params, &p) != nil ||
		!isJSONString(p["protocolVersion"]) || json.Unmarshal(p["protocolVersion"], &v) != nil {
		return nil, newRPCError(codeInvalidParams, "Invalid params")
	}
	if !supported(v) {
		v = versionLatest
	}
	type serverInfo struct {
		Name    string `json:"name"`
		Title   string `json:"title"`
		Version string `json:"version"`
	}
	return struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      serverInfo     `json:"serverInfo"`
		Instructions    string         `json:"instructions"`
	}{v, map[string]any{"tools": struct{}{}}, serverInfo{"backyard", "Backyard", h.version}, h.cat.instructions}, nil
}

// requestOrigin returns the client-visible origin of r.
func (h *handler) requestOrigin(r *http.Request) string {
	if h.o.RequestOrigin != nil {
		return normalizeOrigin(h.o.RequestOrigin(r))
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return normalizeOrigin(scheme + "://" + r.Host)
}

// clientIP returns the caller's address for audit records.
func (h *handler) clientIP(r *http.Request) string {
	if h.o.ClientIP != nil {
		return h.o.ClientIP(r)
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 && !strings.HasSuffix(host, "]") {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

// originAllowed applies SPEC §14.1 gate step 3 to a present Origin header.
func (h *handler) originAllowed(origin, reqOrigin string) bool {
	o := normalizeOrigin(origin)
	if o == "" {
		return false
	}
	return o == h.hubOrigin || o == reqOrigin || h.auth.AllowedOrigin(o)
}

// admit takes an admission slot for a large body of a user, waiting at most
// admitWait in all: first for the user's own (one large body per user at a
// time, so no one takes all the slots), then for one of the shared ones.
func (h *handler) admit(ctx context.Context, userID string) (release func(), ok bool) {
	h.usersMu.Lock()
	u := h.users[userID]
	if u == nil {
		u = &userSlot{ch: make(chan struct{}, 1)}
		h.users[userID] = u
	}
	u.refs++
	h.usersMu.Unlock()
	unref := func() {
		h.usersMu.Lock()
		if u.refs--; u.refs == 0 {
			delete(h.users, userID)
		}
		h.usersMu.Unlock()
	}
	t := time.NewTimer(h.admitWait)
	defer t.Stop()
	select {
	case u.ch <- struct{}{}:
	case <-t.C:
		unref()
		return nil, false
	case <-ctx.Done():
		unref()
		return nil, false
	}
	select {
	case h.slots <- struct{}{}:
		return func() { <-h.slots; <-u.ch; unref() }, true
	case <-t.C:
	case <-ctx.Done():
	}
	<-u.ch
	unref()
	return nil, false
}

// userSlot is a user's share of the admission slots (one at a time).
type userSlot struct {
	ch   chan struct{} // capacity 1: held while the user holds a slot
	refs int           // requests of the user waiting for or holding it
}

// pacedBody moves the connection's read deadline as the body arrives: byte
// n is due grace + n/rate after start (never after limit), so a body that
// falls behind is cut off. Once the body is read, the deadline is limit,
// which net/http's background read of the connection inherits.
type pacedBody struct {
	io.ReadCloser
	rc           *http.ResponseController
	start, limit time.Time
	grace        time.Duration
	rate         float64 // bytes per second
	n, want      int64   // bytes read; the Content-Length (-1: unknown)
}

func (p *pacedBody) Read(b []byte) (int, error) {
	due := p.start.Add(p.grace + time.Duration(float64(p.n)/p.rate*float64(time.Second)))
	if due.After(p.limit) {
		due = p.limit
	}
	_ = p.rc.SetReadDeadline(due)
	n, err := p.ReadCloser.Read(b)
	p.n += int64(n)
	if err == io.EOF || (p.want >= 0 && p.n >= p.want) {
		_ = p.rc.SetReadDeadline(p.limit)
	}
	return n, err
}

// bearerToken returns the token of an "Authorization: Bearer <token>"
// header (the scheme case-insensitive), or "" when there is none.
func bearerToken(values []string) string {
	if len(values) != 1 {
		return ""
	}
	scheme, tok, ok := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

// credentialPart matches a "<scheme> <credentials>" part of an Authorization
// value.
var credentialPart = regexp.MustCompile("^\\s*[!#$%&'*+.^_`|~0-9A-Za-z-]+\\s+\\S")

// severalCredentials reports whether one Authorization value holds more than
// one credential. Proxies and the Workers runtime join repeated headers with
// ", "; the auth-params of a single credential ("a=1, b=2") never look like
// two "<scheme> <credentials>" parts. The Worker applies the same rule.
func severalCredentials(v string) bool {
	if !strings.Contains(v, ",") {
		return false
	}
	n := 0
	for part := range strings.SplitSeq(v, ",") {
		if credentialPart.MatchString(part) {
			n++
		}
	}
	return n >= 2
}

// acceptsJSON reports whether an Accept value admits a JSON answer.
func acceptsJSON(accept string) bool {
	for part := range strings.SplitSeq(accept, ",") {
		mt, _, _ := strings.Cut(part, ";")
		switch strings.ToLower(strings.TrimSpace(mt)) {
		case "application/json", "application/*", "*/*":
			return true
		}
	}
	return false
}

func supported(v string) bool { return v == versionLatest || v == versionFallback }

// normalizeOrigin returns scheme://host[:port] of an http(s) origin, lowercase
// and without a default port, or "".
func normalizeOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	host := strings.ToLower(u.Host)
	if p := u.Port(); (u.Scheme == "https" && p == "443") || (u.Scheme == "http" && p == "80") {
		host = strings.TrimSuffix(host, ":"+p)
	}
	return u.Scheme + "://" + host
}

// hostnameOf returns the lowercase hostname of a normalized origin.
func hostnameOf(origin string) string {
	u, err := url.Parse(origin)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// requestID returns the id the server's middleware put on the response,
// or a fresh one.
func requestID(w http.ResponseWriter) string {
	if id := w.Header().Get("X-Request-Id"); id != "" {
		return id
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	w.Header().Set("X-Request-Id", id)
	return id
}

// writeError writes the SPEC §5 error envelope.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	type detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	b, _ := marshal(struct {
		Error detail `json:"error"`
	}{detail{code, msg}})
	writeBody(w, status, b)
}

// writeRPC writes one JSON-RPC response.
func writeRPC(w http.ResponseWriter, status int, resp rpcResponse) {
	b, err := marshal(resp)
	if err != nil {
		b = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
	}
	writeBody(w, status, b)
}

func writeBody(w http.ResponseWriter, status int, b []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", fmt.Sprint(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// marshal encodes v as compact JSON without HTML escaping.
func marshal(v any) ([]byte, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(sb.String(), "\n")), nil
}

// truncate shortens s to at most n bytes on a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
