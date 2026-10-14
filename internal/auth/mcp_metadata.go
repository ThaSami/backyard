package auth

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// prmDocument is the protected resource metadata (RFC 9728, SPEC §8.4.2). Field
// order is the wire order.
type prmDocument struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
	ResourceName           string   `json:"resource_name"`
}

// asDocument is the authorization server metadata (RFC 8414, SPEC §8.4.2).
type asDocument struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint"`
	RevocationEndpoint                     string   `json:"revocation_endpoint"`
	ScopesSupported                        []string `json:"scopes_supported"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	ResponseModesSupported                 []string `json:"response_modes_supported"`
	GrantTypesSupported                    []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported"`
	AuthorizationResponseISSSupported      bool     `json:"authorization_response_iss_parameter_supported"`
	// Present only while mcp.clients is not empty (otherwise clients fall
	// back to dynamic registration).
	ClientIDMetadataDocumentSupported bool `json:"client_id_metadata_document_supported,omitempty"`
}

// resourceMetadata returns the protected resource metadata document.
func (m *MCP) resourceMetadata() []byte {
	return compactJSON(prmDocument{
		Resource:               m.resource,
		AuthorizationServers:   []string{m.issuer},
		ScopesSupported:        []string{scopeMCP},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           m.name,
	})
}

// serverMetadata returns the authorization server metadata document.
func (m *MCP) serverMetadata() []byte {
	return compactJSON(asDocument{
		Issuer:                                 m.issuer,
		AuthorizationEndpoint:                  m.issuer + AuthorizePath,
		TokenEndpoint:                          m.issuer + OAuthTokenPath,
		RegistrationEndpoint:                   m.issuer + OAuthRegisterPath,
		RevocationEndpoint:                     m.issuer + OAuthRevokePath,
		ScopesSupported:                        []string{scopeMCP, scopeOffline},
		ResponseTypesSupported:                 []string{"code"},
		ResponseModesSupported:                 []string{"query"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:          []string{"S256"},
		AuthorizationResponseISSSupported:      true,
		ClientIDMetadataDocumentSupported:      len(m.clients) > 0,
	})
}

// serveMetadata serves the two discovery documents on the canonical hub
// host (GET, HEAD).
func (m *MCP) serveMetadata(w http.ResponseWriter, r *http.Request) {
	if m == nil {
		writeAPIError(w, http.StatusNotFound, "not_found", notEnabledText)
		return
	}
	var doc []byte
	switch r.URL.Path {
	case ResourceMetadataPath:
		doc = m.resourceMetadata()
	case ASMetadataPath:
		doc = m.serverMetadata()
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if !m.onCanonicalHost(r) {
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowedJSON(w, "GET, HEAD")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Content-Length", strconv.Itoa(len(doc)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(doc)
	}
}

// serveOAuth dispatches /__backyard/oauth/{register,token,revoke}.
func (m *MCP) serveOAuth(w http.ResponseWriter, r *http.Request) {
	if m == nil {
		writeAPIError(w, http.StatusNotFound, "not_found", notEnabledText)
		return
	}
	var h func(http.ResponseWriter, *http.Request)
	switch r.URL.Path {
	case OAuthRegisterPath:
		h = m.serveRegister
	case OAuthTokenPath:
		h = m.serveToken
		w = m.limited(w, r, oauthTooMany)
	case OAuthRevokePath:
		h = m.serveRevoke
	default:
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if !m.onCanonicalHost(r) {
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowedJSON(w, "POST")
		return
	}
	h(w, r)
}

// onCanonicalHost reports whether r is addressed to H, the hub host of the
// issuer (case-insensitive, port ignored). The host is the Host header, or
// X-Forwarded-Host from a trusted proxy, as the server resolves it.
func (m *MCP) onCanonicalHost(r *http.Request) bool {
	return m.a.requestHostname(r) == m.host
}

// requestHostname returns the client-visible hostname of r: lowercased,
// without port and trailing dot.
func (a *authenticator) requestHostname(r *http.Request) string {
	hp := r.Host
	if hp == "" && r.URL != nil {
		hp = r.URL.Host
	}
	if a.peerTrusted(r) {
		if v, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ","); strings.TrimSpace(v) != "" {
			hp = strings.TrimSpace(v)
		}
	}
	hp = strings.ToLower(strings.TrimSpace(hp))
	if strings.HasPrefix(hp, "[") {
		if end := strings.IndexByte(hp, ']'); end > 0 {
			hp = hp[1:end]
		}
	} else if i := strings.LastIndexByte(hp, ':'); i >= 0 && strings.Count(hp, ":") == 1 {
		hp = hp[:i]
	}
	return strings.TrimSuffix(hp, ".")
}

// clientIP returns the client address of r for audit records: the peer, or
// the first untrusted hop of X-Forwarded-For (walked from the right) when the
// peer is a trusted proxy.
func (a *authenticator) clientIP(r *http.Request) string {
	peer := remoteIP(r.RemoteAddr)
	if peer == nil {
		return ""
	}
	if !a.peerTrusted(r) {
		return peer.String()
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hops = append(hops, h)
			}
		}
	}
	ip := peer
	for i := len(hops) - 1; i >= 0; i-- {
		hop := remoteIP(hops[i])
		if hop == nil {
			break
		}
		ip = hop
		if !a.trustedIP(hop) {
			break
		}
	}
	return ip.String()
}

// trustedIP reports whether ip is in server.trusted_proxies.
func (a *authenticator) trustedIP(ip net.IP) bool {
	for _, n := range a.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// originOf returns the normalized origin (lowercase scheme://host[:port],
// default port removed) of an absolute http(s) URL, or "".
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	hp := strings.ToLower(u.Host)
	if port := u.Port(); port == defaultPort("", u.Scheme) {
		hp = strings.TrimSuffix(hp, ":"+port)
	}
	return u.Scheme + "://" + hp
}

// compactJSON encodes v without HTML escaping and without a trailing
// newline.
func compactJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return []byte("{}")
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

// writeAPIError writes the SPEC §5 error envelope.
func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	body := compactJSON(map[string]any{"error": map[string]string{"code": code, "message": message}})
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// methodNotAllowedJSON answers 405 on a JSON path.
func methodNotAllowedJSON(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeAPIError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
}

// writeOAuthJSON writes an OAuth endpoint response (no-store).
func writeOAuthJSON(w http.ResponseWriter, status int, v any) {
	body := compactJSON(v)
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// oauthErrorBody is the RFC 6749 §5.2 error response.
type oauthErrorBody struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// writeOAuthError writes an OAuth error response.
func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeOAuthJSON(w, status, oauthErrorBody{Error: code, Description: desc})
}
