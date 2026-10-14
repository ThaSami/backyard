package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/keys"
)

// Client identification (SPEC §8.4): a client_id is either an allowlisted
// Client ID Metadata Document URL (CIMD) or a stateless, HMAC-signed id from
// dynamic registration (DCR). Codes only ever go to an exact entry of
// mcp.redirect_uris or to an http loopback URI.

// CIMD fetch limits.
const (
	cimdTimeout      = 5 * time.Second
	cimdMaxBytes     = 5120
	cimdMinCache     = 60 * time.Second
	cimdMaxCache     = 3600 * time.Second
	cimdDefaultCache = 300 * time.Second
	cimdFailThrottle = 10 * time.Second
)

var errCIMDRedirect = errors.New("redirects are not followed")

// mcpClient is a resolved client_id.
type mcpClient struct {
	id        string
	kind      string // clientKindCIMD | clientKindDCR
	name      string // sanitized display name
	host      string // CIMD: the client_id URL's host; DCR: ""
	redirects []string
}

// clientError is a client_id that cannot be used; page names the error page
// of the authorize endpoint.
type clientError struct {
	status int
	title  string
	msg    string
	detail string
}

func (e *clientError) Error() string { return e.title + ": " + e.msg }

func errUnknownApp() *clientError {
	return &clientError{status: http.StatusBadRequest, title: "Unknown app",
		msg: "This sign-in link names an app that Backyard does not know. Go back to the app and connect again."}
}

// resolveClient resolves a client_id for the authorize endpoint (it may
// fetch a client metadata document).
func (m *MCP) resolveClient(ctx context.Context, id string) (*mcpClient, *clientError) {
	switch {
	case strings.HasPrefix(id, "https://"):
		if !m.clientSet[id] {
			return nil, &clientError{status: http.StatusBadRequest, title: "App not allowed",
				msg:    "The app " + id + " is not allowed to connect to this Backyard.",
				detail: "An administrator can allow it by adding its client metadata URL to mcp.clients."}
		}
		doc, err := m.cimdDocument(ctx, id)
		if err != nil {
			return nil, &clientError{status: http.StatusBadGateway, title: "Could not verify the app",
				msg: "Backyard could not load the app's client metadata document " + id + ": " + err.Error() + ".",
				detail: "Backyard needs outbound HTTPS to that address. Without it, an administrator can set mcp.clients: [] " +
					"and the app can be added with \"Register automatically\" instead."}
		}
		return doc, nil
	case strings.HasPrefix(id, prefixDCRClient):
		c, ok := m.verifyDCRClient(id)
		if !ok {
			return nil, errUnknownApp()
		}
		return c, nil
	}
	return nil, errUnknownApp()
}

// knownClient reports whether id is an allowlisted CIMD URL or a DCR id with
// a valid MAC (token and revocation endpoints; never fetches).
func (m *MCP) knownClient(id string) bool {
	if strings.HasPrefix(id, "https://") {
		return m.clientSet[id]
	}
	_, ok := m.verifyDCRClient(id)
	return ok
}

// clientHost is the host shown for a client_id: the URL host of a CIMD
// client, "" for a registered one.
func clientHost(id string) string {
	if !strings.HasPrefix(id, "https://") {
		return ""
	}
	u, err := url.Parse(id)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// ---------------------------------------------------------------------------
// Dynamic client registration (stateless, RFC 7591)

// dcrPayload is the signed content of a DCR client_id (field order is the
// wire order).
type dcrPayload struct {
	V int      `json:"v"`
	N string   `json:"n"`
	R []string `json:"r"`
	T int64    `json:"t"`
}

// signDCRClient returns the client_id for a registration.
func (m *MCP) signDCRClient(p dcrPayload) string {
	return prefixDCRClient + keys.Sign(m.clientKey, compactJSON(p))
}

// verifyDCRClient checks a DCR client_id and returns its client.
func (m *MCP) verifyDCRClient(id string) (*mcpClient, bool) {
	raw, ok := strings.CutPrefix(id, prefixDCRClient)
	if !ok || len(id) > maxDCRClientID {
		return nil, false
	}
	b, err := keys.Verify(m.clientKey, raw)
	if err != nil {
		return nil, false
	}
	var p dcrPayload
	if err := json.Unmarshal(b, &p); err != nil || p.V != 1 || len(p.R) == 0 || len(p.R) > maxDCRRedirectURIs {
		return nil, false
	}
	name := sanitizeClientName(p.N)
	return &mcpClient{id: id, kind: clientKindDCR, name: name, redirects: p.R}, true
}

// serveRegister implements POST /__backyard/oauth/register (SPEC §8.4.3).
func (m *MCP) serveRegister(w http.ResponseWriter, r *http.Request) {
	fail := func(code, desc string) { writeOAuthError(w, http.StatusBadRequest, code, desc) }
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		fail("invalid_client_metadata", "the request body must be application/json")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOAuthBody))
	if err != nil {
		fail("invalid_client_metadata", "the request body must be one JSON object of at most 16 KiB")
		return
	}
	var meta map[string]json.RawMessage
	if err := decodeOne(body, &meta); err != nil || meta == nil {
		fail("invalid_client_metadata", "the request body must be one JSON object of at most 16 KiB")
		return
	}
	var uris []string
	rawURIs, present := meta["redirect_uris"]
	if !present || json.Unmarshal(rawURIs, &uris) != nil || len(uris) == 0 || len(uris) > maxDCRRedirectURIs {
		fail("invalid_redirect_uri", "redirect_uris must be an array of 1 to 4 redirect URIs")
		return
	}
	for _, u := range uris {
		if len(u) > maxRedirectURIBytes {
			fail("invalid_redirect_uri", "redirect URIs are limited to 512 bytes")
			return
		}
		if !m.redirectAllowed(u) {
			fail("invalid_redirect_uri", "redirect URI "+u+" is not allowed on this server "+
				"(allowed: the URIs in mcp.redirect_uris and http loopback URIs)")
			return
		}
	}
	name := ""
	if raw, ok := meta["client_name"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &name); err != nil {
			fail("invalid_client_metadata", "client_name must be a string")
			return
		}
	}
	name = sanitizeClientName(name)
	now := m.now().Unix()
	id := m.signDCRClient(dcrPayload{V: 1, N: name, R: uris, T: now})
	if len(id) > maxDCRClientID {
		fail("invalid_client_metadata", "too many or too long redirect_uris")
		return
	}
	m.log.Debug("auth: mcp client registered", "client_name", name, "redirect_uris", len(uris))
	writeOAuthJSON(w, http.StatusCreated, struct {
		ClientID                string   `json:"client_id"`
		ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
		ClientName              string   `json:"client_name"`
		RedirectURIs            []string `json:"redirect_uris"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	}{id, now, name, uris, []string{"authorization_code", "refresh_token"}, []string{"code"}, "none"})
}

// decodeOne decodes exactly one JSON value from b into v.
func decodeOne(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

// sanitizeClientName is cleanClientName, or "MCP client" when nothing is
// left.
func sanitizeClientName(s string) string {
	if s = cleanClientName(s); s == "" {
		return "MCP client"
	}
	return s
}

// cleanClientName strips control characters (Cc) and bidi controls
// (U+200E, U+200F, U+202A–U+202E, U+2066–U+2069) from a client-provided
// name, trims it and cuts it to 64 code points.
func cleanClientName(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.Map(func(r rune) rune {
		switch {
		case unicode.Is(unicode.Cc, r):
			return -1
		case r == '\u200E' || r == '\u200F', r >= '\u202A' && r <= '\u202E', r >= '\u2066' && r <= '\u2069':
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxClientNameRunes {
		s = strings.TrimSpace(string([]rune(s)[:maxClientNameRunes]))
	}
	return s
}

// ---------------------------------------------------------------------------
// Redirect policy and matching (SPEC §8.4.4)

// redirectAllowed applies the redirect policy: an exact entry of
// mcp.redirect_uris, or an http loopback URI.
func (m *MCP) redirectAllowed(uri string) bool {
	if m.redirectSet[uri] {
		return true
	}
	_, ok := parseLoopbackURI(uri)
	return ok
}

// loopbackURI is a parsed http loopback redirect URI.
type loopbackURI struct {
	host string // "localhost", "127.0.0.1" or "[::1]"
	port string // "" when absent
	rest string // path and query, as sent
}

// parseLoopbackURI parses an http loopback URI: scheme http, host exactly
// localhost, 127.0.0.1 or [::1], an optional port, no userinfo or fragment,
// any path and query.
func parseLoopbackURI(raw string) (loopbackURI, bool) {
	var l loopbackURI
	s, ok := strings.CutPrefix(raw, "http://")
	if !ok || strings.ContainsAny(raw, "#\\") || strings.ContainsFunc(raw, unicode.IsSpace) {
		return l, false
	}
	if _, err := url.Parse(raw); err != nil {
		return l, false
	}
	authority := s
	if i := strings.IndexAny(s, "/?"); i >= 0 {
		authority, l.rest = s[:i], s[i:]
	}
	if strings.Contains(authority, "@") {
		return l, false
	}
	tail := ""
	switch {
	case strings.HasPrefix(authority, "[::1]"):
		l.host, tail = "[::1]", authority[len("[::1]"):]
	default:
		l.host, tail, _ = strings.Cut(authority, ":")
		if strings.Contains(authority, ":") {
			tail = ":" + tail
		}
	}
	switch l.host {
	case "localhost", "127.0.0.1", "[::1]":
	default:
		return l, false
	}
	if tail != "" {
		p, ok := strings.CutPrefix(tail, ":")
		n, err := strconv.Atoi(p)
		if !ok || err != nil || n < 1 || n > 65535 || strings.TrimLeft(p, "0123456789") != "" {
			return l, false
		}
		l.port = p
	}
	return l, true
}

// redirectMatches reports whether a requested redirect_uri matches a
// registered one: byte equality, except that for loopback URIs the port is
// ignored on both sides (scheme, host string, path and query must be equal).
func redirectMatches(registered, requested string) bool {
	if strings.Contains(requested, "#") {
		return false
	}
	if u, err := url.Parse(requested); err != nil || u.User != nil {
		return false
	}
	reg, regLoop := parseLoopbackURI(registered)
	req, reqLoop := parseLoopbackURI(requested)
	if regLoop && reqLoop {
		return reg.host == req.host && reg.rest == req.rest
	}
	return registered == requested
}

// clientRedirect returns the requested redirect_uri when it matches one of
// the client's registered URIs and passes the current redirect policy.
func (m *MCP) clientRedirect(c *mcpClient, requested string) bool {
	if requested == "" || !m.redirectAllowed(requested) {
		return false
	}
	for _, reg := range c.redirects {
		if redirectMatches(reg, requested) {
			return true
		}
	}
	return false
}

// canonicalResource canonicalizes a resource parameter (SPEC §8.4.1): an absolute
// http(s) URI without userinfo, query or fragment; scheme and host
// lowercased, the default port dropped, one trailing "/" removed from a
// non-empty path, percent-encoding kept as is.
func canonicalResource(v string) (string, bool) {
	if strings.ContainsAny(v, "?#") {
		return "", false
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" || u.Host == "" {
		return "", false
	}
	_, rest, _ := strings.Cut(v, "://")
	path := ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		path = rest[i:]
	}
	host := strings.ToLower(u.Host)
	if port := u.Port(); port == defaultPort("", u.Scheme) {
		host = strings.TrimSuffix(host, ":"+port)
	}
	if path != "" && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}
	return u.Scheme + "://" + host + path, true
}

// resourceOK reports whether a present resource parameter names R.
func (m *MCP) resourceOK(v string) bool {
	c, ok := canonicalResource(v)
	return ok && c == m.resource
}

// ---------------------------------------------------------------------------
// Client ID Metadata Documents (SPEC §8.4.3)

// cimdEntry is the cache state of one allowlisted client metadata URL.
type cimdEntry struct {
	doc       *mcpClient // cached document (nil when none)
	expires   time.Time
	failErr   error // last failure, served until failUntil
	failUntil time.Time
	call      *cimdCall // in-flight fetch
}

// cimdCall is one in-flight fetch shared by concurrent requests.
type cimdCall struct {
	done chan struct{}
	doc  *mcpClient
	err  error
}

// cimdDocument returns the client of an allowlisted metadata URL from the
// cache or a fetch. Concurrent requests share one fetch; a failure is not
// cached but the URL is not fetched again for 10 s.
func (m *MCP) cimdDocument(ctx context.Context, id string) (*mcpClient, error) {
	m.cimdMu.Lock()
	e := m.cimd[id]
	if e == nil {
		e = &cimdEntry{}
		m.cimd[id] = e
	}
	now := m.now()
	switch {
	case e.doc != nil && now.Before(e.expires):
		doc := e.doc
		m.cimdMu.Unlock()
		return doc, nil
	case e.failErr != nil && now.Before(e.failUntil):
		err := e.failErr
		m.cimdMu.Unlock()
		return nil, err
	case e.call != nil:
		c := e.call
		m.cimdMu.Unlock()
		select {
		case <-c.done:
			return c.doc, c.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c := &cimdCall{done: make(chan struct{})}
	e.call = c
	m.cimdMu.Unlock()

	// The fetch outlives the request that started it: others may wait on it.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cimdTimeout)
	doc, ttl, err := m.fetchCIMD(fctx, id)
	cancel()

	m.cimdMu.Lock()
	e.call = nil
	switch {
	case err != nil:
		e.doc, e.failErr, e.failUntil = nil, err, m.now().Add(cimdFailThrottle)
		m.log.Warn("auth: mcp client metadata document could not be loaded", "client_id", id, "error", err)
	case ttl > 0:
		e.doc, e.expires, e.failErr = doc, m.now().Add(ttl), nil
	default: // no-store / no-cache: used for this request only
		e.doc, e.failErr = nil, nil
	}
	m.cimdMu.Unlock()
	c.doc, c.err = doc, err
	close(c.done)
	return doc, err
}

// fetchCIMD fetches and validates one client metadata document. ttl is how
// long it may be cached (0: not at all).
func (m *MCP) fetchCIMD(ctx context.Context, id string) (*mcpClient, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return nil, 0, errors.New("invalid URL")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.cimdClient.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			err = ue.Err
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, 0, errors.New("the request timed out after 5 s")
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, 0, fmt.Errorf("it answered with a redirect (HTTP %d), which is not followed", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("it answered HTTP %d", resp.StatusCode)
	}
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (mt != "application/json" && !strings.HasSuffix(mt, "+json")) {
		return nil, 0, errors.New("it is not JSON (Content-Type " + strconv.Quote(truncate(resp.Header.Get("Content-Type"), 64)) + ")")
	}
	if resp.ContentLength > cimdMaxBytes {
		return nil, 0, fmt.Errorf("it is larger than %d bytes", cimdMaxBytes)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cimdMaxBytes+1))
	if err != nil {
		return nil, 0, errors.New("reading it failed")
	}
	if len(body) > cimdMaxBytes {
		return nil, 0, fmt.Errorf("it is larger than %d bytes", cimdMaxBytes)
	}
	c, err := m.parseCIMD(id, body)
	if err != nil {
		return nil, 0, err
	}
	return c, cimdCacheTTL(resp.Header.Values("Cache-Control")), nil
}

// parseCIMD validates a client metadata document (SPEC §8.4.3).
func (m *MCP) parseCIMD(id string, body []byte) (*mcpClient, error) {
	if !utf8.Valid(body) {
		return nil, errors.New("it is not UTF-8 JSON")
	}
	var doc map[string]json.RawMessage
	if err := decodeOne(body, &doc); err != nil || doc == nil {
		return nil, errors.New("it is not a JSON object")
	}
	var cid string
	if json.Unmarshal(doc["client_id"], &cid) != nil || cid != id {
		return nil, errors.New("its client_id is not its own URL")
	}
	var uris []string
	if raw, ok := doc["redirect_uris"]; !ok || json.Unmarshal(raw, &uris) != nil || len(uris) == 0 || len(uris) > 10 {
		return nil, errors.New("redirect_uris must be an array of 1 to 10 URIs")
	}
	var allowed []string
	for _, u := range uris {
		if m.redirectAllowed(u) {
			allowed = append(allowed, u)
		}
	}
	if len(allowed) == 0 {
		return nil, errors.New("none of its redirect_uris is allowed on this server (mcp.redirect_uris and http loopback URIs)")
	}
	if raw, ok := doc["token_endpoint_auth_method"]; ok {
		var method string
		if json.Unmarshal(raw, &method) != nil || method != "none" {
			return nil, errors.New("it asks for client authentication (token_endpoint_auth_method), which Backyard does not support")
		}
	}
	for _, k := range []string{"client_secret", "client_secret_expires_at"} {
		if _, ok := doc[k]; ok {
			return nil, errors.New("it contains a client secret")
		}
	}
	host := clientHost(id)
	name := host
	if raw, ok := doc["client_name"]; ok && string(raw) != "null" {
		var n string
		if json.Unmarshal(raw, &n) != nil {
			return nil, errors.New("client_name is not a string")
		}
		if n = cleanClientName(n); n != "" {
			name = n
		}
	}
	return &mcpClient{id: id, kind: clientKindCIMD, name: name, host: host, redirects: allowed}, nil
}

// cimdCacheTTL returns how long a document may be cached according to its
// Cache-Control: max-age clamped to [60 s, 3600 s], 300 s without one, 0 for
// no-store or no-cache.
func cimdCacheTTL(values []string) time.Duration {
	ttl := cimdDefaultCache
	for _, v := range values {
		for _, d := range strings.Split(v, ",") {
			name, val, _ := strings.Cut(strings.TrimSpace(d), "=")
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "no-store", "no-cache":
				return 0
			case "max-age":
				if n, err := strconv.ParseInt(strings.Trim(strings.TrimSpace(val), `"`), 10, 64); err == nil {
					ttl = time.Duration(n) * time.Second
					ttl = max(ttl, cimdMinCache)
					ttl = min(ttl, cimdMaxCache)
				}
			}
		}
	}
	return ttl
}
