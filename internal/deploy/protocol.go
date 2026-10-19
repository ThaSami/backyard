// Package deploy implements the deploy gateway protocol (SPEC §7): the server
// side (Gateway, mountable inside the main server, as `backyard gateway`, or as
// an AWS Lambda function), the Go client used by the CLI and MCP server, and
// the local file collection/ignore rules shared by every deploy method.
//
// # Upload integrity
//
// A deploy is planned (create), uploaded file by file, then committed. The
// plan validates every declared size against the per-file and per-site
// limits, and each upload is bound to its declared size and SHA-256:
//
//   - The gateway's own endpoint (PUT {G}/uploads/{token}; fs storage or
//     deploy.presign: false) streams the body through a verifier, so a body
//     of another size or digest is never stored.
//   - Presigned storage URLs bind what the backend can enforce (see
//     blob.PresignPutOptions): on S3 the size is signed (Content-Length, or
//     the Content-MD5 of the empty body) and, on AWS, the SHA-256 as
//     x-amz-checksum-sha256, so S3 rejects any other body; on GCS
//     x-goog-content-length-range pins the size (GCS cannot check SHA-256);
//     an Azure SAS URL cannot restrict the body at all.
//
// The commit is the second line of defence: every listed file must exist
// with its declared size, or the commit fails with 409 conflict naming the
// paths. A stored object of the wrong size is left alone where every upload
// URL binds the size (the gateway's endpoint, S3, GCS): it can only be
// another deploy's verified upload or a direct sync, which rsync semantics
// keep. Only where presigned uploads cannot bind the size (Azure SAS) is a
// wrong-size object that is not the version the previous manifest recorded
// deleted, so no unverified object of the wrong size outlives a commit
// attempt there.
//
// # Overlapping deploys
//
// Deploys of a site may overlap (two CI runs): their uploads write the same
// paths and the last write wins per file, as with rsync. The commit
// therefore never trusts a declared digest for a stored object the previous
// manifest does not describe (this deploy's upload, or whatever replaced it
// since): it records the digest of the bytes stored now with their etag,
// taken from the store where it verified one itself (S3 checksums of
// presigned uploads, blob.DigestReporter) and otherwise hashed from the
// object, which costs a read of the uploaded bytes. A later deploy of
// content that was overwritten is then never wrongly "unchanged".
//
// # Uncommitted uploads
//
// Uploads write directly to sites/<site>/<path> (rsync semantics; deploys
// are not atomic), so an upload that is never committed stays in storage:
//
//   - A path that is not in the current manifest (a new file) becomes an
//     orphan object: it is served like any object under the site but is not
//     in the manifest, never counts as "unchanged", and is deleted by the
//     next successful commit (which deletes everything it does not list) or
//     with the site; a refresh after a direct sync adopts it instead.
//   - A path in the current manifest is replaced in place, as rsync would.
//
// This is accepted and bounded rather than cleaned up by a janitor: upload
// URLs expire with their deploy id (DeployTTL), are bound to sizes the plan
// checked against the limits (except on Azure, see above), and are only
// issued to the site's authorized deployers by a rate-limited create whose
// audit record ("deploy.create") counts the uploads, their bytes and the
// new paths among them.
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
	// SiteListing is sites.listing (SPEC §7.1): "all" or "owners", always
	// present.
	SiteListing string `json:"site_listing"`
	// MCPURL is the hub's remote MCP endpoint (SPEC §7.1, §14.1): present
	// while the Claude connector is enabled, on the hub's embedded gateway
	// only.
	MCPURL string `json:"mcp_url,omitempty"`
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
