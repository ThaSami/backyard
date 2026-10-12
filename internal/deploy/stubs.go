package deploy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/aws/aws-lambda-go/events"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

// STUBS: every function in this file is replaced by the deploy implementation.

// GatewayOptions configures a Gateway.
type GatewayOptions struct {
	Config   *config.Config
	Store    blob.Store
	Registry *sites.Registry
	// Auth authenticates requests (the server's Authenticator when embedded,
	// auth.NewBearer for the standalone gateway / Lambda).
	Auth auth.Authenticator
	// BaseURL is the absolute URL the gateway is reachable at (used to build
	// self-hosted upload URLs and Info.GatewayURL), e.g.
	// https://backyard.acme.com/__backyard/api/deploy
	BaseURL string
	// OnDeleteSite is called after a site is deleted (e.g. to drop its
	// database documents). Optional.
	OnDeleteSite func(ctx context.Context, site string) error
	Logger       *slog.Logger
	// Version reported in Info.
	Version string
}

// Gateway serves the deploy protocol. Mount it with the base path stripped:
// requests arrive as "/info", "/sites/…", "/uploads/…".
type Gateway struct{ opts GatewayOptions }

// NewGateway validates options and returns a Gateway.
func NewGateway(opts GatewayOptions) (*Gateway, error) { panic("deploy: not implemented") }

// ServeHTTP implements http.Handler.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { panic("deploy: not implemented") }

// Info returns the /info document.
func (g *Gateway) Info() Info { panic("deploy: not implemented") }

// LambdaHandler adapts an http.Handler to AWS Lambda function URL (and API
// Gateway HTTP API v2) events, including base64 bodies.
func LambdaHandler(h http.Handler) func(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	panic("deploy: not implemented")
}

// ---------------------------------------------------------------------------
// Client

// Client talks to a deploy gateway.
type Client struct {
	BaseURL   string // gateway base URL G
	Token     string // bearer token ("" for anonymous)
	HTTP      *http.Client
	UserAgent string
}

// NewClient returns a client with sane timeouts.
func NewClient(baseURL, token string) *Client { panic("deploy: not implemented") }

// APIError is returned for non-2xx gateway responses.
type APIError struct {
	Status   int
	Code     string
	Message  string
	LoginURL string
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

// Info fetches GET /info.
func (c *Client) Info(ctx context.Context) (*Info, error) { panic("deploy: not implemented") }

// ListSites fetches GET /sites.
func (c *Client) ListSites(ctx context.Context) ([]sites.Meta, error) {
	panic("deploy: not implemented")
}

// GetSite fetches GET /sites/{site}.
func (c *Client) GetSite(ctx context.Context, site string) (*sites.Meta, error) {
	panic("deploy: not implemented")
}

// SiteFiles fetches GET /sites/{site}/files.
func (c *Client) SiteFiles(ctx context.Context, site string) ([]SiteFile, error) {
	panic("deploy: not implemented")
}

// ReadFile fetches GET /sites/{site}/files/{path} (maxBytes caps the read; <=0 = 32 MiB).
func (c *Client) ReadFile(ctx context.Context, site, path string, maxBytes int64) ([]byte, error) {
	panic("deploy: not implemented")
}

// DeleteSite calls DELETE /sites/{site}.
func (c *Client) DeleteSite(ctx context.Context, site string) error { panic("deploy: not implemented") }

// Refresh calls POST /sites/{site}/refresh.
func (c *Client) Refresh(ctx context.Context, site string) (*sites.Meta, error) {
	panic("deploy: not implemented")
}

// SetOwners calls POST /sites/{site}/owners.
func (c *Client) SetOwners(ctx context.Context, site string, add, remove []string) (*sites.Meta, error) {
	panic("deploy: not implemented")
}

// DeployOptions tune Client.Deploy.
type DeployOptions struct {
	Ignore      []string // extra gitignore-style patterns
	Message     string
	Concurrency int // parallel uploads (default 8)
	DryRun      bool
	Progress    func(ProgressEvent)
}

// ProgressEvent reports deploy progress.
type ProgressEvent struct {
	Phase string // "scan" | "plan" | "upload" | "commit" | "done"
	Path  string
	Done  int
	Total int
	Bytes int64
}

// DeployResult summarizes a deploy.
type DeployResult struct {
	Site      sites.Meta
	URL       string
	Uploaded  int
	Unchanged int
	Deleted   int
	Bytes     int64
	Files     []LocalFile // the collected files (also filled for DryRun)
}

// Deploy collects files from dir (CollectFiles), creates a deploy, uploads
// changed files concurrently and commits.
func (c *Client) Deploy(ctx context.Context, site, dir string, opts DeployOptions) (*DeployResult, error) {
	panic("deploy: not implemented")
}

// ---------------------------------------------------------------------------
// Local files

// LocalFile is a file selected for deployment.
type LocalFile struct {
	Path        string // site-relative, "/"-separated
	Abs         string // absolute path on disk
	Size        int64
	SHA256      string // hex
	ContentType string
}

// DefaultIgnore returns the built-in ignore patterns (SPEC §13).
func DefaultIgnore() []string { panic("deploy: not implemented") }

// CollectFiles walks dir applying DefaultIgnore, dir/.backyardignore and
// extra patterns (gitignore syntax), hashing every selected file. Symlinks to
// files are followed; symlinked directories are not.
func CollectFiles(dir string, extra []string) ([]LocalFile, error) { panic("deploy: not implemented") }

// Ignored reports whether a site-relative path is excluded by patterns (used
// by direct-sync methods to build tool-specific exclude flags).
func Ignored(rel string, isDir bool, patterns []string) bool { panic("deploy: not implemented") }
