package auth

// The authorization server of the remote MCP connector (SPEC §8.4): Backyard
// issues its own OAuth 2.1 tokens to MCP clients such as Claude, and signs
// the browser in through the configured auth mode at AuthorizePath.
//
// PINNED API (mcp-design.md §11): the declarations below are the contract
// between the authorization server and its consumers (internal/mcpremote,
// internal/server, internal/app). The bodies are placeholders until the
// authorization server lands.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/thasami/backyard/internal/identity"
)

// AuthorizePath is served by Authenticator.Handler() (GET, POST).
const AuthorizePath = "/__backyard/auth/authorize"

// ErrInvalidToken marks an MCP access token that does not authenticate:
// answer 401 with MCP.Challenge(err).
var ErrInvalidToken = errors.New("auth: invalid MCP access token")

// MCP is the connector's authorization server. It is nil when mcp.enabled is
// false.
type MCP struct{}

// Grant is one connection: the consent of a user to one MCP client.
type Grant struct {
	ID, ClientID, ClientName, ClientKind, ClientHost, RedirectURI, Scope, Resource string
	// User is the snapshot taken at consent: ID, Email, Name, Groups, Anonymous.
	User                 identity.User
	CreatedAt, ExpiresAt time.Time
}

// MCPOf returns the authorization server of a (nil when MCP is disabled, a is
// bearer-only, or a is not built by New).
func MCPOf(a Authenticator) *MCP { return nil }

// Issuer returns P, the canonical hub origin.
func (m *MCP) Issuer() string { return "" }

// Resource returns R, P + "/__backyard/mcp".
func (m *MCP) Resource() string { return "" }

// ResourceMetadataURL returns M, the protected resource metadata URL.
func (m *MCP) ResourceMetadataURL() string { return "" }

// MaxBodyBytes is the MCP request body limit: 2 × mcp.max_publish_bytes + 1 MiB.
func (m *MCP) MaxBodyBytes() int64 { return 0 }

// AllowedOrigin reports whether a present Origin header is accepted at
// /__backyard/mcp besides the hub origin, which the caller checks
// (mcp-design.md §3.10 step 3).
func (m *MCP) AllowedOrigin(origin string) bool { return false }

// ServeMetadata serves both discovery documents (§3.2, §3.3).
func (m *MCP) ServeMetadata(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }

// ServeOAuth serves /__backyard/oauth/{register,token,revoke}.
func (m *MCP) ServeOAuth(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }

// VerifyAccessToken returns the re-gated user and the grant of an MCP access
// token. errors.Is(err, ErrInvalidToken) means 401 invalid_token (Challenge
// gives the header); any other error is an infrastructure failure (503).
func (m *MCP) VerifyAccessToken(ctx context.Context, raw string) (*identity.User, *Grant, error) {
	return nil, nil, ErrInvalidToken
}

// Challenge returns the exact WWW-Authenticate value for err (§3.10); a nil
// err gives the missing-token form.
func (m *MCP) Challenge(err error) string { return "" }

// Grants lists the live grants of userID, newest first, at most 200.
func (m *MCP) Grants(ctx context.Context, userID string) ([]Grant, error) { return nil, nil }

// GrantByID returns a grant, or nil, nil when it does not exist.
func (m *MCP) GrantByID(ctx context.Context, id string) (*Grant, error) { return nil, nil }

// Revoke deletes one grant; via names the lever (rfc7009, hub, policy, replay).
func (m *MCP) Revoke(ctx context.Context, id string, actor *identity.User, via string) (bool, error) {
	return false, nil
}

// RevokeUser deletes every grant of userID and returns how many there were.
func (m *MCP) RevokeUser(ctx context.Context, userID string, actor *identity.User, via string) (int, error) {
	return 0, nil
}
