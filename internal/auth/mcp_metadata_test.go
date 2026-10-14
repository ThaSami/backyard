package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
)

const (
	goldenPRM = `{"resource":"https://backyard.test/__backyard/mcp","authorization_servers":["https://backyard.test"],` +
		`"scopes_supported":["mcp"],"bearer_methods_supported":["header"],"resource_name":"Backyard"}`
	goldenAS = `{"issuer":"https://backyard.test",` +
		`"authorization_endpoint":"https://backyard.test/__backyard/auth/authorize",` +
		`"token_endpoint":"https://backyard.test/__backyard/oauth/token",` +
		`"registration_endpoint":"https://backyard.test/__backyard/oauth/register",` +
		`"revocation_endpoint":"https://backyard.test/__backyard/oauth/revoke",` +
		`"scopes_supported":["mcp","offline_access"],"response_types_supported":["code"],` +
		`"response_modes_supported":["query"],"grant_types_supported":["authorization_code","refresh_token"],` +
		`"token_endpoint_auth_methods_supported":["none"],"revocation_endpoint_auth_methods_supported":["none"],` +
		`"code_challenge_methods_supported":["S256"],"authorization_response_iss_parameter_supported":true`
)

func TestMCPMetadataDocuments(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	for path, want := range map[string]string{
		ResourceMetadataPath: goldenPRM,
		ASMetadataPath:       goldenAS + `,"client_id_metadata_document_supported":true}`,
	} {
		res := e.do(req("GET", testIssuer+path))
		if got := body(t, res); res.StatusCode != http.StatusOK || got != want {
			t.Errorf("GET %s = %d\n%s\nwant\n%s", path, res.StatusCode, got, want)
		}
		if res.Header.Get("Content-Type") != "application/json" || res.Header.Get("Cache-Control") != "public, max-age=300" {
			t.Errorf("%s headers = %v", path, res.Header)
		}
		if res.Header.Get("Set-Cookie") != "" || res.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s sets cookies or CORS headers: %v", path, res.Header)
		}
		head := e.do(req("HEAD", testIssuer+path))
		if head.StatusCode != http.StatusOK || body(t, head) != "" || head.Header.Get("Content-Type") != "application/json" ||
			head.Header.Get("Content-Length") != res.Header.Get("Content-Length") {
			t.Errorf("HEAD %s = %d %v", path, head.StatusCode, head.Header)
		}
		for _, m := range []string{"POST", "OPTIONS", "PUT"} {
			r := e.do(req(m, testIssuer+path))
			if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != "GET, HEAD" ||
				body(t, r) != `{"error":{"code":"bad_request","message":"method not allowed"}}` {
				t.Errorf("%s %s = %d %q", m, path, r.StatusCode, r.Header.Get("Allow"))
			}
		}
		// Only the canonical hub host serves them (port and case ignored).
		if r := e.do(req("GET", "https://BACKYARD.test:443"+path)); r.StatusCode != http.StatusOK {
			t.Errorf("canonical host with port: %d", r.StatusCode)
		}
		if r := e.do(req("GET", "https://site.backyard.test"+path)); r.StatusCode != http.StatusNotFound ||
			jsonBody(t, r)["error"].(map[string]any)["code"] != "not_found" {
			t.Errorf("site host: %d", r.StatusCode)
		}
	}
	// The root PRM and openid-configuration are not served.
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/openid-configuration"} {
		r := httptestRecorder(e.m.ServeMetadata, req("GET", testIssuer+p))
		if r.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d", p, r.StatusCode)
		}
	}

	// Without CIMD clients the flag is absent; server.name is resource_name.
	e = newMCPEnv(t, ModeNone, func(c *config.Config) {
		c.MCP.Clients = []string{}
		c.Server.Name = "Acme Backyard"
	})
	if got := body(t, e.do(req("GET", testIssuer+ASMetadataPath))); got != goldenAS+"}" {
		t.Errorf("AS metadata without CIMD:\n%s", got)
	}
	if got := body(t, e.do(req("GET", testIssuer+ResourceMetadataPath))); !strings.HasSuffix(got, `"resource_name":"Acme Backyard"}`) {
		t.Errorf("PRM resource_name: %s", got)
	}
}

func httptestRecorder(h http.HandlerFunc, r *http.Request) *http.Response { return serve(h, r) }

func TestMCPIssuer(t *testing.T) {
	cases := map[string]string{
		"https://Backyard.Acme.com":      "https://backyard.acme.com",
		"https://backyard.acme.com:443":  "https://backyard.acme.com",
		"https://backyard.acme.com:8443": "https://backyard.acme.com:8443",
		"https://backyard.acme.com/":     "https://backyard.acme.com",
		"http://127.0.0.1:8080":          "http://127.0.0.1:8080",
		"http://localhost:80":            "http://localhost",
		"http://[::1]:8080":              "http://[::1]:8080",
	}
	for in, want := range cases {
		p, _, err := mcpIssuer(in)
		if err != nil || p != want {
			t.Errorf("mcpIssuer(%q) = %q, %v; want %q", in, p, err, want)
		}
	}
	for _, bad := range []string{"http://backyard.acme.com", "https://backyard.acme.com/hub", "https://x.example?a=1", "ftp://x.example", ""} {
		if _, _, err := mcpIssuer(bad); err == nil {
			t.Errorf("mcpIssuer(%q) must fail", bad)
		}
	}
	// New refuses an enabled connector without a docstore or with an
	// unusable public URL.
	cfg := baseConfig(ModeNone)
	cfg.MCP.Enabled = true
	if _, err := New(context.Background(), cfg, Options{}); err == nil || !strings.Contains(err.Error(), "document store") {
		t.Errorf("New without Docs: %v", err)
	}
}

func TestMCPDisabled(t *testing.T) {
	a := newAuth(t, baseConfig(ModeNone), Options{})
	if MCPOf(a) != nil {
		t.Fatal("MCPOf must be nil while mcp.enabled is false")
	}
	var m *MCP
	for _, p := range []string{ASMetadataPath, ResourceMetadataPath} {
		res := serve(http.HandlerFunc(m.ServeMetadata), req("GET", testIssuer+p))
		if res.StatusCode != http.StatusNotFound || body(t, res) != `{"error":{"code":"not_found","message":"the Claude connector is not enabled on this server"}}` {
			t.Errorf("disabled %s = %d", p, res.StatusCode)
		}
	}
	res := serve(http.HandlerFunc(m.ServeOAuth), oauthPost(OAuthTokenPath, nil))
	if res.StatusCode != http.StatusNotFound || !strings.Contains(body(t, res), "not enabled") {
		t.Errorf("disabled token endpoint = %d", res.StatusCode)
	}
	page := serve(a.Handler(), req("GET", testIssuer+AuthorizePath+"?client_id=x"))
	if b := body(t, page); page.StatusCode != http.StatusNotFound || !strings.Contains(b, "The Claude connector is not enabled on this server.") ||
		!strings.HasPrefix(page.Header.Get("Content-Type"), "text/html") {
		t.Errorf("disabled authorize = %d %s", page.StatusCode, b)
	}
	if _, _, err := m.VerifyAccessToken(context.Background(), "byat_x"); err == nil {
		t.Error("a nil MCP must not verify tokens")
	}
	if m.Issuer() != "" || m.Resource() != "" || m.MaxBodyBytes() != 0 || m.AllowedOrigin("https://claude.ai") {
		t.Error("nil MCP accessors")
	}
	// A bearer-only authenticator never has one.
	cfg := baseConfig(ModeNone)
	cfg.MCP.Enabled = true
	b, err := NewBearer(context.Background(), cfg, Options{})
	if err != nil || MCPOf(b) != nil {
		t.Fatalf("NewBearer: %v %v", err, MCPOf(b))
	}
}

func TestMCPAccessors(t *testing.T) {
	e := newMCPEnv(t, ModeNone, func(c *config.Config) { c.MCP.MaxPublishBytes = 1 << 20 })
	if MCPOf(e.a) != e.m || e.m.Issuer() != testIssuer || e.m.Resource() != testResource || e.m.ResourceMetadataURL() != testPRM {
		t.Fatalf("accessors: %q %q %q", e.m.Issuer(), e.m.Resource(), e.m.ResourceMetadataURL())
	}
	if e.m.MaxBodyBytes() != 3<<20 {
		t.Fatalf("MaxBodyBytes = %d", e.m.MaxBodyBytes())
	}
	if newMCPEnv(t, ModeNone, nil).m.MaxBodyBytes() != 9437184 {
		t.Fatal("default MaxBodyBytes")
	}
	for o, want := range map[string]bool{
		"https://claude.ai": true, "https://claude.com": true, "HTTPS://CLAUDE.AI": true, "https://claude.ai:443": true,
		testIssuer: true, "https://evil.example": false, "null": false, "": false, "https://claude.ai.evil.example": false,
		"http://claude.ai": false,
	} {
		if got := e.m.AllowedOrigin(o); got != want {
			t.Errorf("AllowedOrigin(%q) = %v", o, got)
		}
	}
	// Custom lists: the origins of https redirect URIs and of clients.
	e = newMCPEnv(t, ModeNone, func(c *config.Config) {
		c.MCP.Clients = []string{"https://apps.example.com:8443/client.json"}
		c.MCP.RedirectURIs = []string{"https://cb.example.org/oauth"}
	})
	for o, want := range map[string]bool{
		"https://apps.example.com:8443": true, "https://cb.example.org": true, "https://claude.ai": false,
	} {
		if got := e.m.AllowedOrigin(o); got != want {
			t.Errorf("custom AllowedOrigin(%q) = %v", o, got)
		}
	}
}

func TestMCPChallenge(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	if got := e.m.Challenge(nil); got != `Bearer resource_metadata="`+testPRM+`", scope="mcp"` {
		t.Errorf("missing-token challenge = %s", got)
	}
	for _, desc := range []string{descMalformed, descExpired, descRevoked, descAudience, descPolicy} {
		want := `Bearer error="invalid_token", error_description="` + desc + `", resource_metadata="` + testPRM + `", scope="mcp"`
		if got := e.m.Challenge(&tokenError{desc: desc}); got != want {
			t.Errorf("challenge = %s\nwant %s", got, want)
		}
	}
	if got := e.m.Challenge(ErrInvalidToken); !strings.Contains(got, `error_description="malformed token"`) {
		t.Errorf("bare ErrInvalidToken challenge = %s", got)
	}
}
