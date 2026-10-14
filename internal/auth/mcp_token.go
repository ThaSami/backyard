package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

// Token formats (SPEC §8.4.8): every value is the prefix plus
// keys.Sign over compact JSON, keyed per purpose. They are opaque to
// clients and reference the grant record, which every use reads.

// codePayload is the content of an authorization code.
type codePayload struct {
	G string `json:"g"`
	X int64  `json:"x"`
}

// refreshPayload is the content of a refresh token; RT(gid, n) is
// deterministic, so a grace answer returns the identical string.
type refreshPayload struct {
	G string `json:"g"`
	N int64  `json:"n"`
}

// accessPayload is the content of an access token.
type accessPayload struct {
	G string `json:"g"`
	I int64  `json:"i"`
	X int64  `json:"x"`
}

var verifierRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// errRedirectGone: the redirect policy (SPEC §8.4.4) applies at token time too, so
// removing an address from mcp.redirect_uris ends its connections.
const errRedirectGone = "the app's redirect address is no longer allowed on this server"

// signToken returns prefix + keys.Sign(key, compact JSON of v).
func signToken(prefix string, key []byte, v any) string {
	return prefix + keys.Sign(key, compactJSON(v))
}

// parseToken verifies prefix and MAC of raw and decodes its payload into v
// (unknown fields and wrong types fail).
func parseToken(raw, prefix string, key []byte, v any) bool {
	s, ok := strings.CutPrefix(raw, prefix)
	if !ok {
		return false
	}
	b, err := keys.Verify(key, s)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v) == nil
}

func (m *MCP) signCode(gid string, exp int64) string {
	return signToken(prefixCode, m.codeKey, codePayload{G: gid, X: exp})
}

func (m *MCP) parseCode(raw string) (codePayload, bool) {
	var p codePayload
	return p, parseToken(raw, prefixCode, m.codeKey, &p) && validGrantID(p.G)
}

func (m *MCP) signRefresh(gid string, n int64) string {
	return signToken(prefixRefresh, m.refreshKey, refreshPayload{G: gid, N: n})
}

func (m *MCP) parseRefresh(raw string) (refreshPayload, bool) {
	var p refreshPayload
	return p, parseToken(raw, prefixRefresh, m.refreshKey, &p) && validGrantID(p.G) && p.N >= 1
}

func (m *MCP) parseAccess(raw string) (accessPayload, bool) {
	var p accessPayload
	return p, len(raw) <= maxMCPBearer && parseToken(raw, prefixAccess, m.accessKey, &p) && validGrantID(p.G)
}

// issueAccess mints an access token for g: 1 h, never past the grant's
// expiry. It returns the token and its lifetime in seconds.
func (m *MCP) issueAccess(g *grantRecord) (string, int64) {
	now := m.now().Unix()
	exp := min(now+int64(mcpAccessTTL.Seconds()), g.Exp)
	return signToken(prefixAccess, m.accessKey, accessPayload{G: g.ID, I: now, X: exp}), exp - now
}

// readOAuthForm reads an application/x-www-form-urlencoded body of at most
// 16 KiB. A parameter sent twice is an error (RFC 6749 §3.1).
func readOAuthForm(w http.ResponseWriter, r *http.Request) (url.Values, string) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, "the request body must be application/x-www-form-urlencoded"
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOAuthBody))
	if err != nil {
		return nil, "the request body is limited to 16 KiB"
	}
	if !utf8.Valid(body) {
		return nil, "the request body is not UTF-8"
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, "the request body is not a valid form"
	}
	for k, v := range form {
		if len(v) > 1 {
			return nil, "parameter " + truncate(k, 64) + " is repeated"
		}
	}
	return form, ""
}

// requestID returns the request id the server middleware put on the
// response ("-" when none).
func requestID(w http.ResponseWriter, r *http.Request) string {
	if id := w.Header().Get("X-Request-Id"); id != "" {
		return id
	}
	return "-"
}

// serverError answers 500 server_error.
func (m *MCP) serverError(w http.ResponseWriter, r *http.Request, what string, err error) {
	m.log.Error("auth: mcp "+what+" failed", "error", err)
	writeOAuthError(w, http.StatusInternalServerError, "server_error", "request "+requestID(w, r))
}

// tokenClient authenticates the public client of a token or revocation
// request: client_id in the body and/or as HTTP Basic with an empty
// password, no secret, and a known client (no fetch). It writes the error
// response itself.
func (m *MCP) tokenClient(w http.ResponseWriter, r *http.Request, form url.Values) (string, bool) {
	invalidClient := func(challenge bool, desc string) (string, bool) {
		if challenge {
			w.Header().Set("WWW-Authenticate", `Basic realm="Backyard"`)
		}
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", desc)
		return "", false
	}
	id := form.Get("client_id")
	if auths := r.Header.Values("Authorization"); len(auths) > 0 {
		user, pass, ok := r.BasicAuth()
		if len(auths) > 1 || !ok {
			return invalidClient(true, "only public clients are supported: send client_id without a secret")
		}
		user, uerr := url.QueryUnescape(user)
		pass, perr := url.QueryUnescape(pass)
		if uerr != nil || perr != nil || pass != "" || user == "" {
			return invalidClient(true, "only public clients are supported: send client_id without a secret")
		}
		if id != "" && id != user {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_id differs between the body and the Authorization header")
			return "", false
		}
		id = user
	}
	if form.Get("client_secret") != "" {
		return invalidClient(true, "only public clients are supported: send client_id without a secret")
	}
	if id == "" {
		return invalidClient(false, "client_id is required")
	}
	if !m.knownClient(id) {
		return invalidClient(false, "unknown client")
	}
	return id, true
}

// tokenResponse is a successful token endpoint answer.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// serveToken implements POST /__backyard/oauth/token (SPEC §8.4.6).
func (m *MCP) serveToken(w http.ResponseWriter, r *http.Request) {
	form, bad := readOAuthForm(w, r)
	if bad != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", bad)
		return
	}
	clientID, ok := m.tokenClient(w, r, form)
	if !ok {
		return
	}
	if res, ok := form["resource"]; ok && !m.resourceOK(res[0]) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource must be "+m.resource)
		return
	}
	switch gt := form.Get("grant_type"); gt {
	case "authorization_code":
		m.exchangeCode(w, r, form, clientID)
	case "refresh_token":
		m.refreshToken(w, r, form, clientID)
	case "":
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "grant_type is required")
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

// exchangeCode redeems an authorization code (grant_type=authorization_code).
func (m *MCP) exchangeCode(w http.ResponseWriter, r *http.Request, form url.Values, clientID string) {
	invalid := func(desc string) { writeOAuthError(w, http.StatusBadRequest, "invalid_grant", desc) }
	code, verifier := form.Get("code"), form.Get("code_verifier")
	if code == "" || verifier == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code and code_verifier are required")
		return
	}
	now := m.now().Unix()
	p, ok := m.parseCode(code)
	if !ok || p.X < now {
		invalid("the authorization code is invalid or expired")
		return
	}
	g, err := m.loadGrant(r.Context(), p.G)
	if err != nil {
		m.serverError(w, r, "code exchange", err)
		return
	}
	// A redeemed grant is not rejected here: a replay is only acted on
	// once PKCE has verified (rotate detects it).
	if g == nil || g.CodeExp < now {
		invalid("the authorization code is invalid or expired")
		return
	}
	if g.ClientID != clientID {
		invalid("the authorization code was issued to another client")
		return
	}
	if !m.redirectAllowed(g.RedirectURI) {
		invalid(errRedirectGone)
		return
	}
	if ru, ok := form["redirect_uri"]; ok && ru[0] != g.RedirectURI {
		invalid("redirect_uri does not match the authorization request")
		return
	}
	if !verifierRe.MatchString(verifier) || !pkceMatches(verifier, g.CodeChallenge) {
		invalid("PKCE verification failed")
		return
	}
	outcome, g2, err := m.rotate(r.Context(), g.ID, 0, false)
	if err != nil {
		m.serverError(w, r, "code exchange", err)
		return
	}
	switch {
	case outcome == rotateReuse:
		m.audit(r.Context(), r, slog.LevelWarn, "mcp.code_replay", g.User.ID, g, true)
		m.audit(r.Context(), r, slog.LevelInfo, "mcp.revoke", g.User.ID, g, true, "via", "replay")
		invalid("the authorization code was already used; the connection has been revoked")
		return
	case outcome != rotateOK || g2 == nil:
		invalid("the authorization code is invalid or expired")
		return
	}
	if !m.regateForToken(w, r, g2) {
		return
	}
	at, expiresIn := m.issueAccess(g2)
	if expiresIn <= 0 {
		invalid("the connection has expired")
		return
	}
	m.audit(r.Context(), r, slog.LevelInfo, "mcp.token", g2.User.ID, g2, false)
	writeOAuthJSON(w, http.StatusOK, tokenResponse{
		AccessToken: at, TokenType: "Bearer", ExpiresIn: expiresIn,
		RefreshToken: m.signRefresh(g2.ID, 1), Scope: g2.Scope,
	})
}

// refreshToken rotates a refresh token (grant_type=refresh_token).
func (m *MCP) refreshToken(w http.ResponseWriter, r *http.Request, form url.Values, clientID string) {
	invalid := func(desc string) { writeOAuthError(w, http.StatusBadRequest, "invalid_grant", desc) }
	raw := form.Get("refresh_token")
	if raw == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	p, ok := m.parseRefresh(raw)
	if !ok {
		invalid("the refresh token is invalid")
		return
	}
	g, err := m.loadGrant(r.Context(), p.G)
	if err != nil {
		m.serverError(w, r, "refresh", err)
		return
	}
	if g == nil || !g.live(m.now().Unix()) {
		invalid("the connection was revoked or has expired")
		return
	}
	if g.ClientID != clientID {
		invalid("the refresh token was issued to another client")
		return
	}
	if !m.redirectAllowed(g.RedirectURI) {
		invalid(errRedirectGone)
		return
	}
	scope := g.Scope
	if s := strings.Fields(form.Get("scope")); len(s) > 0 {
		granted := strings.Fields(g.Scope)
		var out []string
		for _, x := range s {
			if !containsString(granted, x) {
				writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "the requested scope exceeds the granted scope ("+g.Scope+")")
				return
			}
			if !containsString(out, x) {
				out = append(out, x)
			}
		}
		scope = strings.Join(out, " ")
	}
	outcome, g2, err := m.rotate(r.Context(), g.ID, p.N, true)
	if err != nil {
		m.serverError(w, r, "refresh", err)
		return
	}
	switch {
	case outcome == rotateReuse:
		m.audit(r.Context(), r, slog.LevelWarn, "mcp.refresh_reuse", g.User.ID, g, true)
		m.audit(r.Context(), r, slog.LevelInfo, "mcp.revoke", g.User.ID, g, true, "via", "replay")
		invalid("the refresh token was already used; the connection has been revoked")
		return
	case (outcome != rotateOK && outcome != rotateGrace) || g2 == nil:
		invalid("the connection was revoked or has expired")
		return
	}
	if !m.regateForToken(w, r, g2) {
		return
	}
	at, expiresIn := m.issueAccess(g2)
	if expiresIn <= 0 {
		invalid("the connection has expired")
		return
	}
	writeOAuthJSON(w, http.StatusOK, tokenResponse{
		AccessToken: at, TokenType: "Bearer", ExpiresIn: expiresIn,
		RefreshToken: m.signRefresh(g2.ID, p.N+1), Scope: scope,
	})
}

// regateForToken re-gates the grant's user at the token endpoint; on a
// policy failure it revokes the grant and answers invalid_grant.
func (m *MCP) regateForToken(w http.ResponseWriter, r *http.Request, g *grantRecord) bool {
	_, err := m.regate(r.Context(), g)
	switch {
	case err == nil:
		return true
	case errors.Is(err, errPolicy):
		if _, rerr := m.revokeGrant(r.Context(), r, g.ID, nil, "policy"); rerr != nil {
			m.log.Error("auth: mcp grant revocation failed", "grant", shortGID(g.ID), "error", rerr)
		}
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", descPolicy)
	default:
		m.serverError(w, r, "re-gating", err)
	}
	return false
}

// pkceMatches checks BASE64URL(SHA-256(verifier)) against the challenge in
// constant time.
func pkceMatches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// errPolicy marks a user who no longer passes the gates (or a subject the
// current auth mode does not accept).
var errPolicy = errors.New("auth: access removed by policy")

// regate rebuilds the user of a grant's snapshot and applies the current
// policy (SPEC §8.4.8): guests only while auth.mode is none, never static-token
// principals, everyone else through finalize (directory enrichment, admin
// recomputed, allowed_domains / allowed_emails / allowed_groups).
func (m *MCP) regate(ctx context.Context, g *grantRecord) (*identity.User, error) {
	u := g.identityUser()
	switch {
	case strings.HasPrefix(u.ID, anonPrefix):
		if m.a.mode != ModeNone {
			return nil, errPolicy
		}
		return m.a.finalize(ctx, u, srcAnon)
	case strings.HasPrefix(u.ID, tokenPrefix), u.Anonymous:
		return nil, errPolicy
	}
	out, err := m.a.finalize(ctx, u, srcAPIToken)
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrUnauthenticated) {
		return nil, errPolicy
	}
	return out, err
}

// verifyAccessToken implements MCP.VerifyAccessToken (SPEC §8.4.8). There is no
// cache: every call reads the grant, so revocation is immediate.
func (m *MCP) verifyAccessToken(ctx context.Context, raw string) (*identity.User, *Grant, error) {
	p, ok := m.parseAccess(raw)
	if !ok {
		return nil, nil, &tokenError{desc: descMalformed}
	}
	now := m.now().Unix()
	if p.X <= now {
		return nil, nil, &tokenError{desc: descExpired}
	}
	g, err := m.loadGrant(ctx, p.G)
	if err != nil {
		return nil, nil, err
	}
	if g == nil || !g.live(now) {
		return nil, nil, &tokenError{desc: descRevoked}
	}
	if g.Resource != m.resource {
		return nil, nil, &tokenError{desc: descAudience}
	}
	u, err := m.regate(ctx, g)
	if errors.Is(err, errPolicy) {
		if _, rerr := m.revokeGrant(ctx, nil, g.ID, nil, "policy"); rerr != nil {
			m.log.Error("auth: mcp grant revocation failed", "grant", shortGID(g.ID), "error", rerr)
		}
		return nil, nil, &tokenError{desc: descPolicy}
	}
	if err != nil {
		return nil, nil, err
	}
	out := g.public()
	return u, &out, nil
}

// serveRevoke implements POST /__backyard/oauth/revoke (RFC 7009, SPEC §8.4.7). It
// answers 200 for unknown, malformed or foreign tokens, so it is no oracle.
func (m *MCP) serveRevoke(w http.ResponseWriter, r *http.Request) {
	form, bad := readOAuthForm(w, r)
	if bad != "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", bad)
		return
	}
	clientID, ok := m.tokenClient(w, r, form)
	if !ok {
		return
	}
	tok := form.Get("token")
	if tok == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	gid := ""
	if p, ok := m.parseRefresh(tok); ok {
		gid = p.G
	} else if p, ok := m.parseAccess(tok); ok {
		gid = p.G
	}
	if gid != "" {
		g, err := m.loadGrant(r.Context(), gid)
		if err != nil {
			m.serverError(w, r, "revocation", err)
			return
		}
		if g != nil && g.ClientID == clientID {
			if _, err := m.revokeGrant(r.Context(), r, gid, nil, "rfc7009"); err != nil {
				m.serverError(w, r, "revocation", err)
				return
			}
		}
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}
