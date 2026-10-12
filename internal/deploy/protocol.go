// Package deploy implements the deploy gateway protocol (SPEC §7): the server
// side (Gateway, mountable inside the main server, as `backyard gateway`, or as
// an AWS Lambda function), the Go client used by the CLI and MCP server, and
// the local file collection/ignore rules shared by every deploy method.
package deploy

import (
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/sites"
)

// Info is returned by GET {gateway}/info (no auth).
type Info struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	PublicURL   string            `json:"public_url"`
	BaseDomain  string            `json:"base_domain"`
	URLMode     string            `json:"url_mode"`
	GatewayURL  string            `json:"gateway_url"`
	Auth        AuthInfo          `json:"auth"`
	Storage     *blob.Description `json:"storage,omitempty"`
	Methods     []string          `json:"methods"`
	RsyncTarget string            `json:"rsync_target,omitempty"`
	Limits      Limits            `json:"limits"`
}

// AuthInfo tells clients how to obtain a token.
type AuthInfo struct {
	Mode        string      `json:"mode"`
	CLILoginURL string      `json:"cli_login_url,omitempty"`
	Device      *DeviceInfo `json:"device,omitempty"`
	// AnonymousDeploy is true when deploys need no token (auth.mode=none + allow_anonymous).
	AnonymousDeploy bool `json:"anonymous_deploy"`
}

// DeviceInfo enables the OAuth device authorization grant against the IdP.
type DeviceInfo struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"client_id"`
	Scopes   []string `json:"scopes"`
}

// Limits advertised to clients.
type Limits struct {
	MaxSiteBytes int64 `json:"max_site_bytes"`
	MaxFiles     int   `json:"max_files"`
	MaxFileBytes int64 `json:"max_file_bytes"`
}

// FileEntry describes a local file in a deploy request.
type FileEntry struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type,omitempty"`
}

// CreateDeployRequest is the body of POST {G}/sites/{site}/deploys.
type CreateDeployRequest struct {
	Files   []FileEntry `json:"files"`
	Message string      `json:"message,omitempty"`
}

// Upload is one file the client must PUT.
type Upload struct {
	Path    string            `json:"path"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers,omitempty"`
}

// CreateDeployResponse lists uploads to perform.
type CreateDeployResponse struct {
	DeployID  string    `json:"deploy_id"`
	Uploads   []Upload  `json:"uploads"`
	Unchanged []string  `json:"unchanged"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CommitRequest is the body of POST {G}/sites/{site}/deploys/{id}/commit.
type CommitRequest struct {
	Files []FileEntry `json:"files"`
}

// CommitResponse is returned after a successful commit.
type CommitResponse struct {
	Site    sites.Meta `json:"site"`
	URL     string     `json:"url"`
	Deleted int        `json:"deleted"`
}

// SiteFile is one entry of GET {G}/sites/{site}/files.
type SiteFile struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// OwnersRequest is the body of POST {G}/sites/{site}/owners.
type OwnersRequest struct {
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

// ErrorBody is the JSON error envelope shared with the site API.
type ErrorBody struct {
	Error struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		LoginURL string `json:"login_url,omitempty"`
	} `json:"error"`
}
