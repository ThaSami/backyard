package server

// The Claude connector on the hub (SPEC §8.4, §14.1). Its machine paths are
// served by the authorization server (auth.MCP: discovery documents,
// /__backyard/oauth/*) and the MCP endpoint (Deps.MCPEndpoint), which apply
// the canonical-host, method and disabled rules themselves; the server adds
// the per-IP rate limit of registration and revocation and the grants API.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/ratelimit"
)

// msgConnectorDisabled is the 404 message of every connector path while
// mcp.enabled is false (SPEC §8.4.1).
const msgConnectorDisabled = "the Claude connector is not enabled on this server"

// RequestOrigin returns the client-visible origin of r (lowercase
// scheme://host[:port], default port removed), honouring X-Forwarded-Host and
// X-Forwarded-Proto from server.trusted_proxies as the router does. For a
// request that did not pass through the server's handler it falls back to
// r.TLS and r.Host.
func RequestOrigin(r *http.Request) string {
	if ri := infoFromContext(r.Context()); ri != nil {
		return ri.origin
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return normalizeOrigin(scheme, r.Host)
}

// ClientIP returns the client address of r (X-Forwarded-For from
// server.trusted_proxies honoured), as access logs and audit records show it.
// For a request that did not pass through the server's handler it is the
// address of r.RemoteAddr.
func ClientIP(r *http.Request) string {
	if ri := infoFromContext(r.Context()); ri != nil {
		return ri.clientIP
	}
	if ip := remoteIP(r.RemoteAddr); ip != nil {
		return ip.String()
	}
	return "unknown"
}

// AllowAPI consumes one request of the general API limit
// (ratelimit.api_per_minute) for key, e.g. "user:<id>" for the MCP endpoint.
// On refusal it counts rate_limited_total{category="api"} and returns how
// long to wait. A nil Server allows everything.
func (s *Server) AllowAPI(key string) (bool, time.Duration) {
	if s == nil || s.limiter == nil {
		return true, 0
	}
	ok, wait := s.limiter.Allow(ratelimit.API, key)
	if !ok {
		s.metrics.rateLimited.WithLabelValues(string(ratelimit.API)).Inc()
	}
	return ok, wait
}

// allowClientIP charges the API limit keyed by the client IP ("ip:<addr>"),
// the key of the connector's registration and revocation endpoints.
func (s *Server) allowClientIP(r *http.Request) (bool, time.Duration) {
	return s.AllowAPI(ratelimit.Key("", true, info(r).clientIP))
}

// serveAuthorize serves /__backyard/auth/authorize through the
// authenticator, which answers the disabled and wrong-host 404 pages and
// applies the endpoint's rate limit itself (per signed-in person, per
// client IP for requests that identify no one; auth.Options.MCPAllow).
func (s *Server) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	s.deps.Auth.Handler().ServeHTTP(w, r)
}

// serveOAuth serves /__backyard/oauth/*: registration and revocation are
// limited per client IP here, answered in the OAuth error format (RFC 6749
// §5.2); the token endpoint charges the grant's user, or the client IP for
// requests that name no grant, itself (auth.Options.MCPAllow).
func (s *Server) serveOAuth(w http.ResponseWriter, r *http.Request) {
	if s.deps.MCP != nil && r.URL.Path != auth.OAuthTokenPath {
		if ok, wait := s.allowClientIP(r); !ok {
			h := w.Header()
			h.Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(wait)))
			h.Set("Cache-Control", "no-store")
			h.Set("Pragma", "no-cache")
			writeJSON(w, http.StatusTooManyRequests, oauthError{Error: "invalid_request", Description: "too many requests"})
			return
		}
	}
	s.deps.MCP.ServeOAuth(w, r) // nil-safe: the disabled 404
}

// oauthError is an OAuth error response.
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// serveMCP serves POST /__backyard/mcp. The endpoint authenticates by
// itself (MCP access tokens only), so there is no authenticate, CSRF check
// or rate limit here; it also checks the canonical host and charges the
// per-user limit through AllowAPI.
func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	if s.deps.MCPEndpoint == nil {
		s.writeAPIError(w, r, errNotFound(msgConnectorDisabled))
		return
	}
	s.deps.MCPEndpoint.ServeHTTP(w, r)
}

// ---------------------------------------------------------------------------
// The grants API (SPEC §8.4.7): /__backyard/api/mcp/grants on the hub, behind
// apiGate like every hub API. MCP access tokens are not API credentials, so
// the authenticator never accepts them here.

// grantView is a Grant of the grants API.
type grantView struct {
	ID           string        `json:"id"`
	User         grantUserView `json:"user"`
	ClientName   string        `json:"client_name"`
	ClientKind   string        `json:"client_kind"`
	ClientHost   string        `json:"client_host"`
	RedirectHost string        `json:"redirect_host"`
	Scope        string        `json:"scope"`
	CreatedAt    string        `json:"created_at"`
	ExpiresAt    string        `json:"expires_at"`
}

type grantUserView struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func newGrantView(g auth.Grant) grantView {
	return grantView{
		ID:           g.ID,
		User:         grantUserView{ID: g.User.ID, Email: g.User.Email, Name: g.User.Name},
		ClientName:   g.ClientName,
		ClientKind:   g.ClientKind,
		ClientHost:   g.ClientHost,
		RedirectHost: redirectHostOf(g.RedirectURI),
		Scope:        g.Scope,
		CreatedAt:    g.CreatedAt.UTC().Format(time.RFC3339),
		ExpiresAt:    g.ExpiresAt.UTC().Format(time.RFC3339),
	}
}

// redirectHostOf is the lowercase host[:port] of a redirect URI ("" when it
// does not parse).
func redirectHostOf(uri string) string {
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// grantsMCP returns the authorization server, or writes the disabled 404.
func (s *Server) grantsMCP(w http.ResponseWriter, r *http.Request) (*auth.MCP, bool) {
	if s.deps.MCP == nil {
		s.writeAPIError(w, r, errNotFound(msgConnectorDisabled))
		return nil, false
	}
	return s.deps.MCP, true
}

// grantsSubject is the person whose connections a request is about: the
// caller, or with ?user=<id> anyone (admins only; selfOK, for DELETE, also
// lets anyone name themselves).
func grantsSubject(r *http.Request, u *identity.User, selfOK bool) (string, *apiError) {
	who := strings.TrimSpace(r.URL.Query().Get("user"))
	if who == "" || (selfOK && who == u.ID) {
		return u.ID, nil
	}
	if !u.Admin {
		return "", errForbidden("only admins can see or disconnect another person's connections")
	}
	return who, nil
}

// grantsError maps a grant store failure.
func grantsError(ctx context.Context, err error, what string) *apiError {
	if ctx.Err() != nil {
		return errContext(err, what)
	}
	return errInternal(err, "%s failed", what)
}

// handleMCPGrants serves GET /__backyard/api/mcp/grants[?user=<id>]: live
// connections, newest first, at most 200.
func (s *Server) handleMCPGrants(w http.ResponseWriter, r *http.Request) {
	m, ok := s.grantsMCP(w, r)
	if !ok {
		return
	}
	subject, e := grantsSubject(r, info(r).user, false)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	list, err := m.Grants(ctx, subject)
	if err != nil {
		s.writeAPIError(w, r, grantsError(ctx, err, "listing connections"))
		return
	}
	out := make([]grantView, 0, len(list))
	for _, g := range list {
		out = append(out, newGrantView(g))
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": out})
}

// handleMCPRevokeGrant serves DELETE /__backyard/api/mcp/grants/{id}: 204
// for the grant's user or an admin; someone else's grant is as unknown as a
// missing one (404). The authorization server audits mcp.revoke (via=hub).
func (s *Server) handleMCPRevokeGrant(w http.ResponseWriter, r *http.Request) {
	m, ok := s.grantsMCP(w, r)
	if !ok {
		return
	}
	u := info(r).user
	id := r.PathValue("id")
	ctx, cancel := opCtx(r)
	defer cancel()
	g, err := m.GrantByID(ctx, id)
	if err != nil {
		s.writeAPIError(w, r, grantsError(ctx, err, "loading the connection"))
		return
	}
	if g == nil || (g.User.ID != u.ID && !u.Admin) {
		s.writeAPIError(w, r, errNotFound("no such connection"))
		return
	}
	revoked, err := m.Revoke(auth.WithClientIP(ctx, ClientIP(r)), id, u, "hub")
	if err != nil {
		s.writeAPIError(w, r, grantsError(ctx, err, "disconnecting"))
		return
	}
	if !revoked { // disconnected meanwhile
		s.writeAPIError(w, r, errNotFound("no such connection"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// handleMCPRevokeGrants serves DELETE /__backyard/api/mcp/grants[?user=<id>]:
// every connection of a person, pending ones included. The authorization
// server audits mcp.revoke (via=hub, count).
func (s *Server) handleMCPRevokeGrants(w http.ResponseWriter, r *http.Request) {
	m, ok := s.grantsMCP(w, r)
	if !ok {
		return
	}
	u := info(r).user
	subject, e := grantsSubject(r, u, true)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	n, err := m.RevokeUser(auth.WithClientIP(ctx, ClientIP(r)), subject, u, "hub")
	if err != nil {
		s.writeAPIError(w, r, grantsError(ctx, err, "disconnecting"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"revoked": n})
}
