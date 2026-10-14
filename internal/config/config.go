// Package config defines Backyard's configuration schema, loading (YAML with
// ${ENV} expansion), defaults and validation. It contains no runtime logic for
// the subsystems themselves; every other package reads its section from here.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration document (backyard.yaml).
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Auth      AuthConfig      `yaml:"auth"`
	Identity  IdentityConfig  `yaml:"identity"`
	Storage   StorageConfig   `yaml:"storage"`
	Database  DatabaseConfig  `yaml:"database"`
	PubSub    PubSubConfig    `yaml:"pubsub"`
	AI        AIConfig        `yaml:"ai"`
	Warehouse WarehouseConfig `yaml:"warehouse"`
	Sites     SitesConfig     `yaml:"sites"`
	Files     FilesConfig     `yaml:"files"`
	Deploy    DeployConfig    `yaml:"deploy"`
	MCP       MCPConfig       `yaml:"mcp"`
	RateLimit RateLimitConfig `yaml:"ratelimit"`
	Realtime  RealtimeConfig  `yaml:"realtime"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
}

// ServerConfig controls the HTTP server and URL scheme.
type ServerConfig struct {
	// Listen address, e.g. ":8080". Empty → ":$PORT" if PORT is set, else ":8080".
	Listen string `yaml:"listen"`
	// BaseDomain is the hub host; sites live at <site>.<base_domain> (host mode).
	BaseDomain string `yaml:"base_domain"`
	// PublicURL is the externally visible hub URL, e.g. https://backyard.acme.com.
	// Defaults to https://<base_domain> (http:// for localhost).
	PublicURL string `yaml:"public_url"`
	// URLMode is "host" (default; wildcard subdomains) or "path" (/s/<site>/).
	URLMode string `yaml:"url_mode"`
	// Secret signs/encrypts sessions, API tokens, deploy ids and upload URLs.
	// Must be at least 32 characters. Generated (ephemeral) in dev mode.
	Secret string `yaml:"secret"`
	// TrustedProxies are CIDRs (or IPs) whose X-Forwarded-* headers are trusted
	// and who may supply identity headers in auth.mode=header.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// Dev enables developer defaults (see ApplyDevDefaults).
	Dev bool `yaml:"dev"`
	// Name shown in the hub UI.
	Name string `yaml:"name"`
	// ReadTimeout/WriteTimeout/IdleTimeout for the HTTP server. Websockets and
	// streaming responses are exempt from WriteTimeout.
	ReadTimeout  Duration `yaml:"read_timeout"`
	WriteTimeout Duration `yaml:"write_timeout"`
	IdleTimeout  Duration `yaml:"idle_timeout"`
	// ShutdownTimeout bounds graceful shutdown.
	ShutdownTimeout Duration `yaml:"shutdown_timeout"`
}

// AuthConfig selects and configures the authenticator.
type AuthConfig struct {
	// Mode: none | oidc | iap | alb | header. "okta", "entra", "azuread",
	// "google", "auth0" and "keycloak" are accepted aliases for "oidc".
	Mode string `yaml:"mode"`
	// Admins are user ids/emails with admin rights.
	Admins []string `yaml:"admins"`
	// AdminGroups grant admin rights to members.
	AdminGroups []string `yaml:"admin_groups"`
	// AllowedDomains restricts access to these email domains (empty = any).
	AllowedDomains []string `yaml:"allowed_domains"`
	// AllowedEmails admits these exact addresses too. With AllowedDomains a
	// person passes when either matches; alone, only the listed addresses
	// pass (empty = no per-address list).
	AllowedEmails []string `yaml:"allowed_emails"`
	// AllowedGroups restricts access to members of these groups (empty = any).
	AllowedGroups []string `yaml:"allowed_groups"`
	// TokenTTL is the lifetime of minted Backyard API tokens (default 720h).
	TokenTTL Duration `yaml:"token_ttl"`
	// Tokens are static bearer tokens (e.g. for CI), stored as sha256 hex.
	Tokens []StaticToken `yaml:"tokens"`

	OIDC   OIDCConfig       `yaml:"oidc"`
	IAP    IAPConfig        `yaml:"iap"`
	ALB    ALBConfig        `yaml:"alb"`
	Header HeaderAuthConfig `yaml:"header"`
}

// StaticToken is a pre-shared bearer token.
type StaticToken struct {
	Name   string `yaml:"name"`
	SHA256 string `yaml:"sha256"`
	Admin  bool   `yaml:"admin"`
}

// OIDCConfig configures the built-in OpenID Connect login (Okta, Entra ID, …).
type OIDCConfig struct {
	Issuer       string   `yaml:"issuer"`
	ClientID     string   `yaml:"client_id"`
	ClientSecret string   `yaml:"client_secret"`
	Scopes       []string `yaml:"scopes"` // default: openid profile email (+ groups if GroupsClaim set)
	// RedirectURL defaults to <public_url>/__backyard/auth/callback.
	RedirectURL string `yaml:"redirect_url"`
	// SessionTTL is the browser session lifetime (default 12h).
	SessionTTL Duration `yaml:"session_ttl"`
	// UseUserInfo calls the userinfo endpoint to enrich claims.
	UseUserInfo bool `yaml:"use_userinfo"`
	// DeviceClientID enables the CLI device-authorization flow with this
	// (public/native) client id.
	DeviceClientID string `yaml:"device_client_id"`
	// DeviceScopes default: openid profile email offline_access.
	DeviceScopes []string `yaml:"device_scopes"`
	// APIAudiences are additional accepted audiences for IdP-issued bearer
	// JWTs presented to the API/gateway.
	APIAudiences []string `yaml:"api_audiences"`
	// InsecureSkipIssuerCheck is for IdPs with non-standard issuer URLs (avoid).
	InsecureSkipIssuerCheck bool `yaml:"insecure_skip_issuer_check"`
}

// IAPConfig configures Google Cloud Identity-Aware Proxy verification.
type IAPConfig struct {
	// Audience: /projects/<number>/global/backendServices/<id> (load balancer)
	// or /projects/<number>/locations/<region>/services/<name> (Cloud Run).
	Audience string `yaml:"audience"`
	// AdditionalAudiences are also accepted (e.g. the deploy backend service).
	AdditionalAudiences []string `yaml:"additional_audiences"`
	// JWKSURL override (default https://www.gstatic.com/iap/verify/public_key-jwk).
	JWKSURL string `yaml:"jwks_url"`
}

// ALBConfig configures AWS Application Load Balancer OIDC verification.
type ALBConfig struct {
	Region string `yaml:"region"`
	// SignerARN is the load balancer ARN expected in the JWT "signer" header.
	SignerARN string `yaml:"signer_arn"`
	// KeyURL override (default https://public-keys.auth.elb.<region>.amazonaws.com).
	KeyURL string `yaml:"key_url"`
}

// HeaderAuthConfig configures trusted-proxy header authentication.
type HeaderAuthConfig struct {
	EmailHeader  string `yaml:"email_header"`  // e.g. X-Forwarded-Email, X-MS-CLIENT-PRINCIPAL-NAME
	NameHeader   string `yaml:"name_header"`   // optional
	GroupsHeader string `yaml:"groups_header"` // optional, comma separated
	// Optional JWT verification (Cloudflare Access, Pomerium, …).
	JWTHeader string   `yaml:"jwt_header"`
	JWKSURL   string   `yaml:"jwks_url"`
	Issuer    string   `yaml:"issuer"`
	Audiences []string `yaml:"audiences"`
}

// IdentityConfig maps IdP claims to user profile fields and configures an
// optional directory for enrichment.
type IdentityConfig struct {
	Claims    ClaimsMapping   `yaml:"claims"`
	Directory DirectoryConfig `yaml:"directory"`
	// ExtraClaims are additional claim names copied into User.Extra.
	ExtraClaims []string `yaml:"extra_claims"`
}

// ClaimsMapping names the claim used for each profile field.
type ClaimsMapping struct {
	Email  string `yaml:"email"`  // default "email"
	Name   string `yaml:"name"`   // default "name"
	Title  string `yaml:"title"`  // default "title"
	Team   string `yaml:"team"`   // default "department"
	Slack  string `yaml:"slack"`  // default "slack"
	Avatar string `yaml:"avatar"` // default "picture"
	Groups string `yaml:"groups"` // default "groups"
}

// DirectoryConfig is an optional static people directory.
type DirectoryConfig struct {
	// Type: "" (none) | csv | json.
	Type string `yaml:"type"`
	// Path to the file. CSV header must include "email"; other recognised
	// columns: name,title,team,slack,avatar_url,groups (groups ';'-separated).
	Path string `yaml:"path"`
	// Reload interval (0 = load once).
	Reload Duration `yaml:"reload"`
}

// StorageConfig selects the blob store holding site files and uploads.
type StorageConfig struct {
	// Type: fs | s3 | gcs | azure.
	Type  string      `yaml:"type"`
	FS    FSConfig    `yaml:"fs"`
	S3    S3Config    `yaml:"s3"`
	GCS   GCSConfig   `yaml:"gcs"`
	Azure AzureConfig `yaml:"azure"`
	// CacheBytes is the in-memory cache size for small static files
	// (default 64 MiB; -1 disables the cache).
	CacheBytes int64 `yaml:"cache_bytes"`
	// CacheTTL is how long cached objects are trusted (default 5s).
	CacheTTL Duration `yaml:"cache_ttl"`
}

// FSConfig stores everything on the local filesystem (or a mounted bucket via
// gcsfuse / mountpoint-s3 / blobfuse2). This is the "no bucket" option and the
// target of `backyard deploy --method rsync`.
type FSConfig struct {
	Root string `yaml:"root"` // default /var/lib/backyard (dev: ./.backyard-data)
}

// S3Config for AWS S3 or S3-compatible stores (MinIO, R2, Ceph…).
type S3Config struct {
	Bucket         string `yaml:"bucket"`
	Region         string `yaml:"region"`
	Prefix         string `yaml:"prefix"`
	Endpoint       string `yaml:"endpoint"` // custom endpoint for S3-compatible stores
	ForcePathStyle bool   `yaml:"force_path_style"`
	// Static credentials (optional; default AWS credential chain).
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	// SSEKMSKeyID enables SSE-KMS with this key for writes (optional).
	SSEKMSKeyID string `yaml:"sse_kms_key_id"`
}

// GCSConfig for Google Cloud Storage.
type GCSConfig struct {
	Bucket string `yaml:"bucket"`
	Prefix string `yaml:"prefix"`
	// CredentialsFile optional; default Application Default Credentials.
	CredentialsFile string `yaml:"credentials_file"`
	// SignerEmail is the service account used for V4 signed URLs when ADC has
	// no private key (Cloud Run / GCE); default: auto-detected.
	SignerEmail string `yaml:"signer_email"`
	// Endpoint override (testing / emulators).
	Endpoint string `yaml:"endpoint"`
}

// AzureConfig for Azure Blob Storage.
type AzureConfig struct {
	Account   string `yaml:"account"`
	Container string `yaml:"container"`
	Prefix    string `yaml:"prefix"`
	// ConnectionString optional (Azurite / shared key); default DefaultAzureCredential.
	ConnectionString string `yaml:"connection_string"`
	// AccountKey optional shared key (enables service SAS without user delegation).
	AccountKey string `yaml:"account_key"`
	// Endpoint override, default https://<account>.blob.core.windows.net/.
	Endpoint string `yaml:"endpoint"`
}

// DatabaseConfig selects the document store behind backyard.db.
type DatabaseConfig struct {
	// Type: sqlite | postgres | dynamodb | memory.
	Type     string         `yaml:"type"`
	SQLite   SQLiteConfig   `yaml:"sqlite"`
	Postgres PostgresConfig `yaml:"postgres"`
	DynamoDB DynamoDBConfig `yaml:"dynamodb"`
	// MaxDocBytes caps the JSON size of one document (default 262144).
	MaxDocBytes int `yaml:"max_doc_bytes"`
	// MaxList caps query limit (default 1000).
	MaxList int `yaml:"max_list"`
	// MaxDocsPerSite caps documents per site (default 100000; 0 selects the
	// default, a negative value = unlimited).
	MaxDocsPerSite int `yaml:"max_docs_per_site"`
}

// SQLiteConfig for the embedded single-node database.
type SQLiteConfig struct {
	Path string `yaml:"path"` // default <fs root>/backyard.db
}

// PostgresConfig for PostgreSQL (Cloud SQL, RDS, Azure Flexible Server, …).
type PostgresConfig struct {
	DSN      string `yaml:"dsn"` // postgres://user:pass@host:5432/db?sslmode=require
	MaxConns int    `yaml:"max_conns"`
	// Schema to create tables in (default "public").
	Schema string `yaml:"schema"`
	// AutoMigrate creates/updates tables on start (default true).
	AutoMigrate *bool `yaml:"auto_migrate"`
}

// DynamoDBConfig for Amazon DynamoDB.
type DynamoDBConfig struct {
	Table    string `yaml:"table"` // default "backyard"
	Region   string `yaml:"region"`
	Endpoint string `yaml:"endpoint"` // DynamoDB Local / LocalStack
	// CreateTable creates the table (on-demand billing) if missing.
	CreateTable bool `yaml:"create_table"`
	// MaxScan caps items read per query before in-memory filtering/sorting (default 10000).
	MaxScan int `yaml:"max_scan"`
}

// PubSubConfig selects the cross-instance message bus for realtime.
type PubSubConfig struct {
	// Type: memory (single instance) | postgres | redis. Default memory.
	Type     string         `yaml:"type"`
	Redis    RedisConfig    `yaml:"redis"`
	Postgres PostgresPubSub `yaml:"postgres"`
}

// RedisConfig for Redis / Valkey / ElastiCache / Memorystore.
type RedisConfig struct {
	URL string `yaml:"url"` // redis://[:password@]host:6379/0 or rediss://
}

// PostgresPubSub uses LISTEN/NOTIFY.
type PostgresPubSub struct {
	DSN     string `yaml:"dsn"`     // default database.postgres.dsn
	Channel string `yaml:"channel"` // default "backyard_events"
}

// AIConfig configures providers and model aliases.
type AIConfig struct {
	// Enabled defaults to true when at least one model is configured.
	Enabled *bool `yaml:"enabled"`
	// DefaultModel is the alias used when the client does not specify one.
	DefaultModel string `yaml:"default_model"`
	// DefaultImageModel / DefaultEmbeddingModel aliases.
	DefaultImageModel     string                    `yaml:"default_image_model"`
	DefaultEmbeddingModel string                    `yaml:"default_embedding_model"`
	Providers             map[string]ProviderConfig `yaml:"providers"`
	Models                map[string]ModelConfig    `yaml:"models"`
	// AllowRawModels lets clients pass "provider/model-id" directly.
	AllowRawModels bool `yaml:"allow_raw_models"`
	// ExposeModelIDs includes provider model ids in GET /ai/models.
	ExposeModelIDs bool `yaml:"expose_model_ids"`
	// MaxTokens caps max_tokens per request (default 8192).
	MaxTokens int `yaml:"max_tokens"`
	// RequestTimeout per upstream call (default 120s; streaming: 10m).
	RequestTimeout Duration `yaml:"request_timeout"`
	// SystemPrompt is prepended to every request's system prompt (optional).
	SystemPrompt string `yaml:"system_prompt"`
}

// ProviderConfig configures one upstream provider instance.
type ProviderConfig struct {
	// Type: anthropic | openai | azure-openai | bedrock | gemini | ollama.
	// ("ollama" is openai-compatible with base_url default http://localhost:11434/v1.)
	Type    string            `yaml:"type"`
	APIKey  string            `yaml:"api_key"`
	BaseURL string            `yaml:"base_url"`
	Headers map[string]string `yaml:"headers"`

	// anthropic: platform "anthropic" (default) | "vertex" | "bedrock".
	Platform string `yaml:"platform"`
	// anthropic: enable server-side refusal fallbacks on the first-party API (default true).
	RefusalFallback *bool `yaml:"refusal_fallback"`
	// anthropic: default effort for models that support it (low|medium|high|xhigh|max).
	Effort string `yaml:"effort"`

	// openai: OpenAI-Organization / OpenAI-Project headers.
	Organization  string `yaml:"organization"`
	OpenAIProject string `yaml:"openai_project"`

	// azure-openai
	Endpoint   string `yaml:"endpoint"`    // https://<resource>.openai.azure.com
	APIVersion string `yaml:"api_version"` // default 2024-10-21
	// azure-openai: use Entra ID (DefaultAzureCredential) instead of api_key.
	UseAzureAD bool `yaml:"use_azure_ad"`

	// bedrock / anthropic(platform=bedrock)
	Region string `yaml:"region"`

	// gemini: AI Studio (api_key) or Vertex AI (project+location, ADC).
	// anthropic(platform=vertex) also uses project+location.
	ProjectID string `yaml:"project_id"`
	Location  string `yaml:"location"`
}

// ModelConfig maps an alias to a provider model.
type ModelConfig struct {
	Provider    string `yaml:"provider"` // key in ai.providers
	Model       string `yaml:"model"`    // provider model id
	Kind        string `yaml:"kind"`     // chat (default) | image | embedding
	Description string `yaml:"description"`
	MaxTokens   int    `yaml:"max_tokens"` // per-alias cap
	Effort      string `yaml:"effort"`     // anthropic effort override
}

// WarehouseConfig configures read-only analytics sources.
type WarehouseConfig struct {
	Sources map[string]WarehouseSource `yaml:"sources"`
	// DefaultSource name (default "default", or the only source).
	DefaultSource string `yaml:"default_source"`
	// AllowRawSQL permits ad-hoc SQL from sites (default false).
	AllowRawSQL bool `yaml:"allow_raw_sql"`
	// AllowAnyStatement disables the SELECT/WITH-only guard (default false).
	AllowAnyStatement bool                  `yaml:"allow_any_statement"`
	Queries           map[string]NamedQuery `yaml:"queries"`
	MaxRows           int                   `yaml:"max_rows"` // default 10000
	Timeout           Duration              `yaml:"timeout"`  // default 60s
}

// WarehouseSource is one analytics backend.
type WarehouseSource struct {
	// Type: bigquery | postgres | athena.
	Type        string `yaml:"type"`
	Description string `yaml:"description"`
	// bigquery
	ProjectID       string `yaml:"project_id"`
	Location        string `yaml:"location"`
	MaxBytesBilled  int64  `yaml:"max_bytes_billed"`
	CredentialsFile string `yaml:"credentials_file"`
	// postgres (also Redshift, AlloyDB, Aurora, …)
	DSN string `yaml:"dsn"`
	// athena
	Region         string `yaml:"region"`
	Workgroup      string `yaml:"workgroup"`
	Database       string `yaml:"database"`
	Catalog        string `yaml:"catalog"`
	OutputLocation string `yaml:"output_location"`
}

// NamedQuery is a pre-approved parameterised query.
type NamedQuery struct {
	Source      string   `yaml:"source"`
	SQL         string   `yaml:"sql"`
	Description string   `yaml:"description"`
	Params      []string `yaml:"params"` // names used as @name in SQL
}

// SitesConfig controls site naming and ownership.
type SitesConfig struct {
	// Ownership: none (default, anyone can overwrite — the Quick philosophy) |
	// owner (first deployer owns the site; owners/admins may overwrite/delete).
	Ownership string   `yaml:"ownership"`
	Reserved  []string `yaml:"reserved"`
	// MaxSiteBytes total (default 500 MiB), MaxFiles (default 10000),
	// MaxFileBytes per file (default 100 MiB).
	MaxSiteBytes int64 `yaml:"max_site_bytes"`
	MaxFiles     int   `yaml:"max_files"`
	MaxFileBytes int64 `yaml:"max_file_bytes"`
}

// FilesConfig controls backyard.files uploads.
type FilesConfig struct {
	MaxUploadBytes int64 `yaml:"max_upload_bytes"` // default 100 MiB
}

// DeployConfig controls deploy methods advertised to CLIs and the gateway.
type DeployConfig struct {
	// Methods advertised to CLIs, in preference order. Default: ["api"] plus
	// the storage-native method (s3|gcs|azure|rsync for fs).
	Methods []string `yaml:"methods"`
	// RsyncTarget e.g. deploy@backyard.acme.com:/var/lib/backyard/sites
	RsyncTarget string `yaml:"rsync_target"`
	// GatewayURL advertises an external gateway (Lambda / Cloud Run) to CLIs.
	GatewayURL string `yaml:"gateway_url"`
	// AllowAnonymous allows deploys without any token in auth.mode=none.
	AllowAnonymous bool `yaml:"allow_anonymous"`
	// Presign uses presigned storage URLs for uploads when supported (default true).
	Presign *bool `yaml:"presign"`
	// ExposeStorage includes bucket details in /info (default true).
	ExposeStorage *bool `yaml:"expose_storage"`
	// EmbeddedGateway mounts the gateway at /__backyard/api/deploy (default true).
	EmbeddedGateway *bool `yaml:"embedded_gateway"`
}

// MCPConfig configures the remote MCP connector (SPEC §8.4, §14.1): Claude
// and other MCP clients reach <public_url>/__backyard/mcp with OAuth tokens
// Backyard issues itself after the browser signed in through auth.mode.
type MCPConfig struct {
	// Enabled serves the connector endpoint and Backyard's OAuth endpoints
	// (default false: every connector path answers 404).
	Enabled bool `yaml:"enabled"`
	// Clients are the Client ID Metadata Document URLs that may connect
	// (only these are fetched). Unset: DefaultMCPClients; [] turns CIMD off,
	// so clients register automatically (stateless DCR).
	Clients []string `yaml:"clients"`
	// RedirectURIs are the exact https addresses that may receive sign-in
	// codes (unset: DefaultMCPRedirectURIs). http loopback addresses are
	// always allowed and are not listed.
	RedirectURIs []string `yaml:"redirect_uris"`
	// MaxPublishBytes caps the decoded file bytes of one backyard_publish
	// call (default 4194304). The MCP request body limit is 2× this + 1 MiB.
	MaxPublishBytes int64 `yaml:"max_publish_bytes"`
}

// Defaults of the mcp section.
var (
	// DefaultMCPClients are Claude's client metadata documents (claude.ai,
	// Desktop, mobile and Cowork; Claude Code).
	DefaultMCPClients = []string{
		"https://claude.ai/oauth/mcp-oauth-client-metadata",
		"https://claude.ai/oauth/claude-code-client-metadata",
	}
	// DefaultMCPRedirectURIs are Claude's OAuth callbacks.
	DefaultMCPRedirectURIs = []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://claude.com/api/mcp/auth_callback",
	}
)

// DefaultMCPMaxPublishBytes is the default of mcp.max_publish_bytes.
const DefaultMCPMaxPublishBytes = 4 << 20

// RateLimitConfig sets per-user limits (per instance). For every limit, 0
// selects the default and a negative value = unlimited (that limit is
// disabled); Disabled turns all of them off.
type RateLimitConfig struct {
	Disabled          bool `yaml:"disabled"`
	APIPerMinute      int  `yaml:"api_per_minute"`       // default 600
	DBWritesPerMinute int  `yaml:"db_writes_per_minute"` // default 300
	AIPerMinute       int  `yaml:"ai_per_minute"`        // default 20
	// AITokensPerDay caps input+output AI tokens per user per UTC day
	// (default 2000000; negative = unlimited).
	AITokensPerDay     int64 `yaml:"ai_tokens_per_day"`
	DeploysPerHour     int   `yaml:"deploys_per_hour"`     // default 60
	UploadsPerMinute   int   `yaml:"uploads_per_minute"`   // default 60
	WarehousePerMinute int   `yaml:"warehouse_per_minute"` // default 30
}

// RealtimeConfig controls websockets.
type RealtimeConfig struct {
	Disabled          bool `yaml:"disabled"`
	MaxRoomsPerConn   int  `yaml:"max_rooms_per_conn"`  // default 50
	MaxSubsPerConn    int  `yaml:"max_subs_per_conn"`   // default 100
	MessagesPerSecond int  `yaml:"messages_per_second"` // default 60
	MessageBurst      int  `yaml:"message_burst"`       // default 120
	MaxMessageBytes   int  `yaml:"max_message_bytes"`   // default 65536
	MaxConnsPerUser   int  `yaml:"max_conns_per_user"`  // default 50
	MaxPresenceBytes  int  `yaml:"max_presence_bytes"`  // default 8192
	// AllowCrossSiteWS lets pages on other Backyard sites open this site's websocket (default false).
	AllowCrossSiteWS bool `yaml:"allow_cross_site_ws"`
}

// TelemetryConfig controls logs and metrics.
type TelemetryConfig struct {
	LogLevel  string `yaml:"log_level"`  // debug | info (default) | warn | error
	LogFormat string `yaml:"log_format"` // json (default) | text
	Metrics   bool   `yaml:"metrics"`    // expose Prometheus metrics
	// MetricsListen serves /metrics on a separate address (recommended in prod).
	MetricsListen string `yaml:"metrics_listen"`
	// AccessLog logs every request (default false).
	AccessLog bool `yaml:"access_log"`
}

// Duration is a time.Duration that unmarshals from strings like "12h" or integers (seconds).
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		*d = 0
		return nil
	}
	if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	// Support a "d" (days) suffix in addition to time.ParseDuration units.
	if strings.HasSuffix(s, "d") {
		if days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64); err == nil {
			*d = Duration(time.Duration(days * float64(24*time.Hour)))
			return nil
		}
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// ---------------------------------------------------------------------------
// Loading

// Load reads configuration from (in order of precedence): the file at path (if
// non-empty), the BACKYARD_CONFIG_YAML environment variable, the file named by
// BACKYARD_CONFIG, or ./backyard.yaml if it exists. Missing everything yields an
// empty config (valid only with dev=true). Defaults are applied and the result
// is validated.
func Load(path string, dev bool) (*Config, error) {
	var raw []byte
	var err error
	switch {
	case path != "":
		raw, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
	case os.Getenv("BACKYARD_CONFIG_YAML") != "":
		raw = []byte(os.Getenv("BACKYARD_CONFIG_YAML"))
	case os.Getenv("BACKYARD_CONFIG") != "":
		raw, err = os.ReadFile(os.Getenv("BACKYARD_CONFIG"))
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
	default:
		if b, err := os.ReadFile("backyard.yaml"); err == nil {
			raw = b
		}
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if dev {
		cfg.Server.Dev = true
	}
	if cfg.Server.Dev {
		cfg.ApplyDevDefaults()
	}
	cfg.ApplyEnvOverrides()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Parse expands ${ENV} references and decodes YAML strictly (unknown keys are
// errors). It does not apply defaults or validate.
func Parse(raw []byte) (*Config, error) {
	cfg := &Config{}
	expanded := ExpandEnv(string(raw))
	if strings.TrimSpace(expanded) == "" {
		return cfg, nil
	}
	dec := yaml.NewDecoder(strings.NewReader(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// ExpandEnv replaces ${VAR} and ${VAR:-default} with environment values.
// A literal "$${" escapes to "${".
func ExpandEnv(s string) string {
	const esc = "\x00ESCAPED_DOLLAR_BRACE\x00"
	s = strings.ReplaceAll(s, "$${", esc)
	s = envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(sub[1]); ok && v != "" {
			return v
		}
		return sub[3]
	})
	return strings.ReplaceAll(s, esc, "${")
}

// ApplyEnvOverrides applies a small set of well-known environment variables
// that are convenient on PaaS platforms.
func (c *Config) ApplyEnvOverrides() {
	if v := os.Getenv("BACKYARD_SECRET"); v != "" && c.Server.Secret == "" {
		c.Server.Secret = v
	}
	if v := os.Getenv("BACKYARD_BASE_DOMAIN"); v != "" && c.Server.BaseDomain == "" {
		c.Server.BaseDomain = v
	}
	if v := os.Getenv("BACKYARD_PUBLIC_URL"); v != "" && c.Server.PublicURL == "" {
		c.Server.PublicURL = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" && c.Database.Type == "postgres" && c.Database.Postgres.DSN == "" {
		c.Database.Postgres.DSN = v
	}
	if c.Server.Listen == "" {
		if p := os.Getenv("PORT"); p != "" {
			c.Server.Listen = ":" + p
		}
	}
}

// ApplyDevDefaults fills in a zero-dependency local setup.
func (c *Config) ApplyDevDefaults() {
	c.Server.Dev = true
	if c.Server.BaseDomain == "" {
		c.Server.BaseDomain = "localhost"
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "none"
	}
	if c.Storage.Type == "" {
		c.Storage.Type = "fs"
	}
	if c.Storage.Type == "fs" && c.Storage.FS.Root == "" {
		c.Storage.FS.Root = ".backyard-data"
	}
	if c.Database.Type == "" {
		c.Database.Type = "sqlite"
	}
	if c.PubSub.Type == "" {
		c.PubSub.Type = "memory"
	}
	if c.Auth.Mode == "none" {
		c.Deploy.AllowAnonymous = true
	}
	if c.Server.Secret == "" && os.Getenv("BACKYARD_SECRET") == "" {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		c.Server.Secret = "dev-" + hex.EncodeToString(b)
	}
}

// DefaultReserved are site names that can never be deployed.
var DefaultReserved = []string{"www", "api", "admin", "hub", "auth", "static", "assets", "cdn", "docs", "mail", "backyard", "s"}

// ApplyDefaults fills zero values with production defaults.
func (c *Config) ApplyDefaults() {
	s := &c.Server
	if s.Listen == "" {
		s.Listen = ":8080"
	}
	s.BaseDomain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s.BaseDomain), "."))
	if s.PublicURL == "" && s.BaseDomain != "" {
		scheme := "https"
		if isLocalHost(s.BaseDomain) {
			scheme = "http"
		}
		host := s.BaseDomain
		if scheme == "http" {
			if _, port, err := net.SplitHostPort(s.Listen); err == nil && port != "" && port != "80" {
				host += ":" + port
			}
		}
		s.PublicURL = scheme + "://" + host
	}
	s.PublicURL = strings.TrimSuffix(s.PublicURL, "/")
	if s.URLMode == "" {
		s.URLMode = "host"
	}
	if s.Name == "" {
		s.Name = "Backyard"
	}
	if s.ReadTimeout == 0 {
		s.ReadTimeout = Duration(60 * time.Second)
	}
	if s.WriteTimeout == 0 {
		s.WriteTimeout = Duration(120 * time.Second)
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = Duration(120 * time.Second)
	}
	if s.ShutdownTimeout == 0 {
		s.ShutdownTimeout = Duration(20 * time.Second)
	}

	a := &c.Auth
	a.Mode = strings.ToLower(a.Mode)
	switch a.Mode {
	case "", "off", "disabled":
		a.Mode = "none"
	case "okta", "entra", "azuread", "azure-ad", "google", "auth0", "keycloak", "openid":
		a.Mode = "oidc"
	}
	if a.TokenTTL == 0 {
		a.TokenTTL = Duration(720 * time.Hour)
	}
	if a.OIDC.SessionTTL == 0 {
		a.OIDC.SessionTTL = Duration(12 * time.Hour)
	}
	if len(a.OIDC.Scopes) == 0 {
		a.OIDC.Scopes = []string{"openid", "profile", "email"}
	}
	if len(a.OIDC.DeviceScopes) == 0 {
		a.OIDC.DeviceScopes = []string{"openid", "profile", "email", "offline_access"}
	}
	if a.OIDC.RedirectURL == "" && s.PublicURL != "" {
		a.OIDC.RedirectURL = s.PublicURL + "/__backyard/auth/callback"
	}
	if a.IAP.JWKSURL == "" {
		a.IAP.JWKSURL = "https://www.gstatic.com/iap/verify/public_key-jwk"
	}
	if a.ALB.KeyURL == "" && a.ALB.Region != "" {
		a.ALB.KeyURL = "https://public-keys.auth.elb." + a.ALB.Region + ".amazonaws.com"
	}

	cm := &c.Identity.Claims
	setDefault(&cm.Email, "email")
	setDefault(&cm.Name, "name")
	setDefault(&cm.Title, "title")
	setDefault(&cm.Team, "department")
	setDefault(&cm.Slack, "slack")
	setDefault(&cm.Avatar, "picture")
	setDefault(&cm.Groups, "groups")

	st := &c.Storage
	if st.Type == "" {
		st.Type = "fs"
	}
	if st.Type == "fs" && st.FS.Root == "" {
		st.FS.Root = "/var/lib/backyard"
	}
	if st.CacheBytes == 0 {
		st.CacheBytes = 64 << 20
	}
	if st.CacheTTL == 0 {
		st.CacheTTL = Duration(5 * time.Second)
	}
	st.S3.Prefix = normPrefix(st.S3.Prefix)
	st.GCS.Prefix = normPrefix(st.GCS.Prefix)
	st.Azure.Prefix = normPrefix(st.Azure.Prefix)

	d := &c.Database
	if d.Type == "" {
		d.Type = "sqlite"
	}
	if d.Type == "sqlite" && d.SQLite.Path == "" {
		root := st.FS.Root
		if root == "" {
			root = "/var/lib/backyard"
		}
		d.SQLite.Path = filepath.Join(root, "backyard.db")
	}
	if d.Postgres.MaxConns == 0 {
		d.Postgres.MaxConns = 20
	}
	if d.Postgres.Schema == "" {
		d.Postgres.Schema = "public"
	}
	if d.Postgres.AutoMigrate == nil {
		t := true
		d.Postgres.AutoMigrate = &t
	}
	if d.DynamoDB.Table == "" {
		d.DynamoDB.Table = "backyard"
	}
	if d.DynamoDB.MaxScan == 0 {
		d.DynamoDB.MaxScan = 10000
	}
	if d.MaxDocBytes == 0 {
		d.MaxDocBytes = 256 << 10
	}
	if d.MaxList == 0 {
		d.MaxList = 1000
	}
	if d.MaxDocsPerSite == 0 {
		d.MaxDocsPerSite = 100000
	}

	p := &c.PubSub
	if p.Type == "" {
		p.Type = "memory"
	}
	if p.Type == "postgres" && p.Postgres.DSN == "" {
		p.Postgres.DSN = d.Postgres.DSN
	}
	if p.Postgres.Channel == "" {
		p.Postgres.Channel = "backyard_events"
	}

	ai := &c.AI
	if ai.Enabled == nil {
		e := len(ai.Models) > 0
		ai.Enabled = &e
	}
	if ai.MaxTokens == 0 {
		ai.MaxTokens = 8192
	}
	if ai.RequestTimeout == 0 {
		ai.RequestTimeout = Duration(120 * time.Second)
	}
	for name, m := range ai.Models {
		if m.Kind == "" {
			m.Kind = "chat"
			ai.Models[name] = m
		}
	}
	if ai.DefaultModel == "" {
		ai.DefaultModel = firstModelOfKind(ai.Models, "chat")
	}
	if ai.DefaultImageModel == "" {
		ai.DefaultImageModel = firstModelOfKind(ai.Models, "image")
	}
	if ai.DefaultEmbeddingModel == "" {
		ai.DefaultEmbeddingModel = firstModelOfKind(ai.Models, "embedding")
	}
	for name, pc := range ai.Providers {
		if pc.Type == "" {
			pc.Type = name
		}
		if pc.Type == "ollama" && pc.BaseURL == "" {
			pc.BaseURL = "http://localhost:11434/v1"
		}
		if pc.Type == "azure-openai" && pc.APIVersion == "" {
			pc.APIVersion = "2024-10-21"
		}
		if pc.Type == "anthropic" && pc.Platform == "" {
			pc.Platform = "anthropic"
		}
		ai.Providers[name] = pc
	}

	w := &c.Warehouse
	if w.MaxRows == 0 {
		w.MaxRows = 10000
	}
	if w.Timeout == 0 {
		w.Timeout = Duration(60 * time.Second)
	}
	if w.DefaultSource == "" {
		if _, ok := w.Sources["default"]; ok {
			w.DefaultSource = "default"
		} else if len(w.Sources) == 1 {
			for k := range w.Sources {
				w.DefaultSource = k
			}
		}
	}

	si := &c.Sites
	if si.Ownership == "" {
		si.Ownership = "none"
	}
	if si.Reserved == nil {
		si.Reserved = append([]string(nil), DefaultReserved...)
	}
	if si.MaxSiteBytes == 0 {
		si.MaxSiteBytes = 500 << 20
	}
	if si.MaxFiles == 0 {
		si.MaxFiles = 10000
	}
	if si.MaxFileBytes == 0 {
		si.MaxFileBytes = 100 << 20
	}

	if c.Files.MaxUploadBytes == 0 {
		c.Files.MaxUploadBytes = 100 << 20
	}

	dp := &c.Deploy
	if len(dp.Methods) == 0 {
		dp.Methods = []string{"api"}
		switch st.Type {
		case "s3", "gcs", "azure":
			dp.Methods = append(dp.Methods, st.Type)
		case "fs":
			if dp.RsyncTarget != "" {
				dp.Methods = append(dp.Methods, "rsync")
			}
		}
	}
	if dp.Presign == nil {
		t := true
		dp.Presign = &t
	}
	if dp.ExposeStorage == nil {
		t := true
		dp.ExposeStorage = &t
	}
	if dp.EmbeddedGateway == nil {
		t := true
		dp.EmbeddedGateway = &t
	}

	mc := &c.MCP
	// nil (unset) selects the defaults; an explicit [] stays empty. Entries
	// are compared byte for byte, so only surrounding space is removed.
	if mc.Clients == nil {
		mc.Clients = append([]string(nil), DefaultMCPClients...)
	}
	if mc.RedirectURIs == nil {
		mc.RedirectURIs = append([]string(nil), DefaultMCPRedirectURIs...)
	}
	for i := range mc.Clients {
		mc.Clients[i] = strings.TrimSpace(mc.Clients[i])
	}
	for i := range mc.RedirectURIs {
		mc.RedirectURIs[i] = strings.TrimSpace(mc.RedirectURIs[i])
	}
	if mc.MaxPublishBytes == 0 {
		mc.MaxPublishBytes = DefaultMCPMaxPublishBytes
	}

	r := &c.RateLimit
	setDefaultInt(&r.APIPerMinute, 600)
	setDefaultInt(&r.DBWritesPerMinute, 300)
	setDefaultInt(&r.AIPerMinute, 20)
	if r.AITokensPerDay == 0 {
		r.AITokensPerDay = 2_000_000
	}
	setDefaultInt(&r.DeploysPerHour, 60)
	setDefaultInt(&r.UploadsPerMinute, 60)
	setDefaultInt(&r.WarehousePerMinute, 30)

	rt := &c.Realtime
	setDefaultInt(&rt.MaxRoomsPerConn, 50)
	setDefaultInt(&rt.MaxSubsPerConn, 100)
	setDefaultInt(&rt.MessagesPerSecond, 60)
	setDefaultInt(&rt.MessageBurst, 120)
	setDefaultInt(&rt.MaxMessageBytes, 64<<10)
	setDefaultInt(&rt.MaxConnsPerUser, 50)
	setDefaultInt(&rt.MaxPresenceBytes, 8<<10)

	t := &c.Telemetry
	if t.LogLevel == "" {
		t.LogLevel = "info"
	}
	if t.LogFormat == "" {
		t.LogFormat = "json"
		if c.Server.Dev {
			t.LogFormat = "text"
		}
	}
}

// Validate reports configuration errors (all of them, joined).
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	s := c.Server
	if s.BaseDomain == "" {
		add("server.base_domain is required (e.g. backyard.example.com)")
	}
	if s.URLMode != "host" && s.URLMode != "path" {
		add("server.url_mode must be host or path, got %q", s.URLMode)
	}
	if u, err := url.Parse(s.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("server.public_url must be an absolute http(s) URL, got %q", s.PublicURL)
	}
	if len(s.Secret) < 32 {
		add("server.secret must be at least 32 characters (set BACKYARD_SECRET; generate with: openssl rand -hex 32)")
	}
	for _, p := range s.TrustedProxies {
		if _, err := ParseCIDROrIP(p); err != nil {
			add("server.trusted_proxies: %v", err)
		}
	}

	a := c.Auth
	switch a.Mode {
	case "none":
	case "oidc":
		if a.OIDC.Issuer == "" || a.OIDC.ClientID == "" {
			add("auth.oidc.issuer and auth.oidc.client_id are required for auth.mode=oidc")
		}
	case "iap":
		if a.IAP.Audience == "" {
			add("auth.iap.audience is required for auth.mode=iap")
		}
	case "alb":
		if a.ALB.Region == "" {
			add("auth.alb.region is required for auth.mode=alb")
		}
	case "header":
		if a.Header.EmailHeader == "" && a.Header.JWTHeader == "" {
			add("auth.header.email_header or auth.header.jwt_header is required for auth.mode=header")
		}
		if a.Header.JWTHeader != "" && a.Header.JWKSURL == "" {
			add("auth.header.jwks_url is required when auth.header.jwt_header is set")
		}
		if len(s.TrustedProxies) == 0 && a.Header.JWTHeader == "" {
			add("server.trusted_proxies must be set for auth.mode=header without JWT verification")
		}
	default:
		add("auth.mode must be one of none, oidc, iap, alb, header; got %q", a.Mode)
	}
	for i, e := range a.AllowedEmails {
		e = strings.TrimSpace(e)
		if addr, err := mail.ParseAddress(e); err != nil || !strings.EqualFold(addr.Address, e) {
			add("auth.allowed_emails[%d]: %q is not an email address (domains go in auth.allowed_domains)", i, e)
		}
	}
	for i, t := range a.Tokens {
		if t.Name == "" || len(t.SHA256) != 64 {
			add("auth.tokens[%d]: name and 64-char hex sha256 are required", i)
		}
	}

	switch c.Storage.Type {
	case "fs":
		if c.Storage.FS.Root == "" {
			add("storage.fs.root is required")
		}
	case "s3":
		if c.Storage.S3.Bucket == "" {
			add("storage.s3.bucket is required")
		}
	case "gcs":
		if c.Storage.GCS.Bucket == "" {
			add("storage.gcs.bucket is required")
		}
	case "azure":
		if c.Storage.Azure.Container == "" || (c.Storage.Azure.Account == "" && c.Storage.Azure.ConnectionString == "") {
			add("storage.azure.container and storage.azure.account (or connection_string) are required")
		}
	default:
		add("storage.type must be fs, s3, gcs or azure; got %q", c.Storage.Type)
	}

	switch c.Database.Type {
	case "memory":
	case "sqlite":
		if c.Database.SQLite.Path == "" {
			add("database.sqlite.path is required")
		}
	case "postgres":
		if c.Database.Postgres.DSN == "" {
			add("database.postgres.dsn is required (or set DATABASE_URL)")
		}
	case "dynamodb":
		if c.Database.DynamoDB.Table == "" {
			add("database.dynamodb.table is required")
		}
	default:
		add("database.type must be sqlite, postgres, dynamodb or memory; got %q", c.Database.Type)
	}

	switch c.PubSub.Type {
	case "memory":
	case "redis":
		if c.PubSub.Redis.URL == "" {
			add("pubsub.redis.url is required for pubsub.type=redis")
		}
	case "postgres":
		if c.PubSub.Postgres.DSN == "" {
			add("pubsub.postgres.dsn (or database.postgres.dsn) is required for pubsub.type=postgres")
		}
	default:
		add("pubsub.type must be memory, redis or postgres; got %q", c.PubSub.Type)
	}

	for name, p := range c.AI.Providers {
		switch p.Type {
		case "anthropic":
			switch p.Platform {
			case "anthropic", "vertex", "bedrock":
			default:
				add("ai.providers.%s.platform must be anthropic, vertex or bedrock", name)
			}
		case "openai", "ollama", "bedrock", "gemini":
		case "azure-openai":
			if p.Endpoint == "" {
				add("ai.providers.%s.endpoint is required for azure-openai", name)
			}
		default:
			add("ai.providers.%s.type must be anthropic, openai, azure-openai, bedrock, gemini or ollama; got %q", name, p.Type)
		}
	}
	for alias, m := range c.AI.Models {
		if _, ok := c.AI.Providers[m.Provider]; !ok {
			add("ai.models.%s.provider %q is not defined in ai.providers", alias, m.Provider)
		}
		if m.Model == "" {
			add("ai.models.%s.model is required", alias)
		}
		switch m.Kind {
		case "chat", "image", "embedding":
		default:
			add("ai.models.%s.kind must be chat, image or embedding", alias)
		}
		if strings.Contains(alias, "/") {
			add("ai.models.%s: alias may not contain '/'", alias)
		}
	}
	if c.AI.DefaultModel != "" {
		if _, ok := c.AI.Models[c.AI.DefaultModel]; !ok {
			add("ai.default_model %q is not defined in ai.models", c.AI.DefaultModel)
		}
	}

	for name, src := range c.Warehouse.Sources {
		switch src.Type {
		case "bigquery":
			if src.ProjectID == "" {
				add("warehouse.sources.%s.project_id is required for bigquery", name)
			}
		case "postgres":
			if src.DSN == "" {
				add("warehouse.sources.%s.dsn is required for postgres", name)
			}
		case "athena":
			if src.Region == "" || src.OutputLocation == "" && src.Workgroup == "" {
				add("warehouse.sources.%s: athena needs region and output_location or workgroup", name)
			}
		default:
			add("warehouse.sources.%s.type must be bigquery, postgres or athena", name)
		}
	}
	for name, q := range c.Warehouse.Queries {
		srcName := q.Source
		if srcName == "" {
			srcName = c.Warehouse.DefaultSource
		}
		if _, ok := c.Warehouse.Sources[srcName]; !ok {
			add("warehouse.queries.%s: source %q is not defined", name, srcName)
		}
		if strings.TrimSpace(q.SQL) == "" {
			add("warehouse.queries.%s.sql is required", name)
		}
	}

	if c.Sites.Ownership != "none" && c.Sites.Ownership != "owner" {
		add("sites.ownership must be none or owner")
	}
	for _, m := range c.Deploy.Methods {
		switch m {
		case "api", "s3", "gcs", "azure", "rsync":
		default:
			add("deploy.methods: unknown method %q", m)
		}
	}
	if a.Mode == "oidc" && s.URLMode == "host" && s.BaseDomain != "" {
		// The session cookie is set by the hub with Domain=.<base_domain> so
		// that site hosts see it. Without such a cookie every site page sends
		// the browser through the IdP and back, endlessly.
		bd := bareHost(s.BaseDomain)
		if !CookieDomainCapable(bd) {
			add("server.base_domain %q cannot carry a cookie Domain (browsers refuse localhost, *.localhost, IP addresses "+
				"and single-label names), so with auth.mode=oidc in host mode the sign-in session never reaches site hosts "+
				"and every site redirects to the login endlessly: set server.url_mode: path, or use a real domain "+
				"(for local testing e.g. lvh.me, whose subdomains resolve to 127.0.0.1)", s.BaseDomain)
		} else if u, err := url.Parse(s.PublicURL); err == nil && u.Hostname() != "" {
			if hub := bareHost(u.Hostname()); hub != bd && !strings.HasSuffix(hub, "."+bd) {
				add("server.public_url host %q must be server.base_domain %q or one of its subdomains with auth.mode=oidc "+
					"in host mode: the hub sets the sign-in session cookie for .<base_domain>, and from another host site "+
					"hosts never receive it (every site would redirect to the login endlessly); or set server.url_mode: path",
					hub, bd)
			}
		}
	}
	if c.Deploy.AllowAnonymous && c.Auth.Mode != "none" {
		add("deploy.allow_anonymous is only allowed with auth.mode=none")
	}
	errs = append(errs, c.validateMCP()...)
	switch c.Telemetry.LogFormat {
	case "json", "text":
	default:
		add("telemetry.log_format must be json or text")
	}
	return errors.Join(errs...)
}

// validateMCP checks the mcp section (SPEC §12; rules 1–6 of the connector
// design). The entry rules apply whether or not the connector is enabled.
func (c *Config) validateMCP() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	m := c.MCP
	for _, e := range m.Clients {
		if !validMCPClientURL(e) {
			add("mcp.clients: %q is not a client metadata URL (an https URL with a path, without query, fragment or user info, at most 512 bytes)", e)
		}
	}
	hub := ""
	if u, err := url.Parse(c.Server.PublicURL); err == nil {
		hub = bareHost(u.Hostname())
	}
	base := bareHost(c.Server.BaseDomain)
	for _, e := range m.RedirectURIs {
		u, ok := parseMCPRedirectURI(e)
		if !ok {
			add("mcp.redirect_uris: %q is not an https redirect URI without fragment or user info (at most 512 bytes; loopback redirect URIs are always allowed and are not listed)", e)
		}
		if u == nil {
			continue
		}
		if h := bareHost(u.Hostname()); h != "" && (h == hub || (base != "" && (h == base || strings.HasSuffix(h, "."+base)))) {
			add("mcp.redirect_uris: %q points at this Backyard or one of its sites", e)
		}
	}
	// The comparison with sites.max_site_bytes applies only while the
	// connector is on, so an existing small sites.max_site_bytes keeps
	// validating after an upgrade.
	if v := m.MaxPublishBytes; v < 64<<10 || v > 64<<20 || (m.Enabled && v > c.Sites.MaxSiteBytes) {
		add("mcp.max_publish_bytes must be between 65536 and 67108864 and at most sites.max_site_bytes")
	}
	if !m.Enabled {
		return errs
	}
	if !mcpPublicURLOK(c.Server.PublicURL) {
		add("mcp.enabled requires server.public_url to be https (http only on localhost, *.localhost or loopback addresses) without a path, query or fragment")
	}
	if c.Auth.Mode == "oidc" {
		if u, err := url.Parse(strings.TrimSpace(c.Auth.OIDC.RedirectURL)); err == nil && path.Clean("/"+u.Path) == "/__backyard/auth/authorize" {
			add("auth.oidc.redirect_url must not be /__backyard/auth/authorize while mcp.enabled is true")
		}
	}
	return errs
}

// maxMCPURLBytes bounds the entries of mcp.clients and mcp.redirect_uris.
const maxMCPURLBytes = 512

// validMCPClientURL reports whether e may be listed in mcp.clients: an https
// URL whose host is a name (not an IP literal), with a path other than "/"
// and without dot segments, query, fragment or user info.
func validMCPClientURL(e string) bool {
	if e == "" || len(e) > maxMCPURLBytes || strings.ContainsAny(e, "?#\\ ") {
		return false
	}
	u, err := url.Parse(e)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" || u.Hostname() == "" {
		return false
	}
	if net.ParseIP(strings.Trim(u.Hostname(), "[]")) != nil {
		return false
	}
	if u.Path == "" || u.Path == "/" || !strings.HasPrefix(u.Path, "/") {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// parseMCPRedirectURI parses an mcp.redirect_uris entry. ok reports whether it
// is an https URL with a host and without fragment or user info, at most 512
// bytes; u is the parsed URL whenever it has a host (for further checks).
func parseMCPRedirectURI(e string) (u *url.URL, ok bool) {
	u, err := url.Parse(e)
	if err != nil || u.Host == "" {
		return nil, false
	}
	ok = e != "" && len(e) <= maxMCPURLBytes && u.Scheme == "https" && u.Opaque == "" && u.User == nil &&
		u.Hostname() != "" && !strings.Contains(e, "#") && !strings.ContainsAny(e, "\\ ")
	return u, ok
}

// mcpPublicURLOK reports whether public_url can be the connector's issuer:
// http(s) without a path, query or fragment, and https unless its host is a
// loopback host (localhost, *.localhost, 127.0.0.0/8, [::1]).
func mcpPublicURLOK(pu string) bool {
	u, err := url.Parse(strings.TrimSpace(pu))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.Opaque != "" || u.User != nil {
		return false
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(pu, "#") {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return LoopbackHost(u.Hostname())
	}
	return false
}

// LoopbackHost reports whether h (a hostname without port) is localhost, a
// *.localhost name, a 127.0.0.0/8 literal or ::1.
func LoopbackHost(h string) bool {
	h = bareHost(h)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ---------------------------------------------------------------------------
// Helpers used across packages

// IsAdmin reports whether a user id/email or any group grants admin rights.
func (a AuthConfig) IsAdmin(id, email string, groups []string) bool {
	for _, adm := range a.Admins {
		if strings.EqualFold(adm, id) || (email != "" && strings.EqualFold(adm, email)) {
			return true
		}
	}
	for _, g := range groups {
		for _, ag := range a.AdminGroups {
			if strings.EqualFold(g, ag) {
				return true
			}
		}
	}
	return false
}

// SiteURL returns the public URL of a site (with trailing slash). In host
// mode that is <scheme>://<site>.<base_domain>[:<port>]/ with the scheme and
// port of server.public_url: sites are routed by base_domain, whose name may
// differ from the public URL's host (public_url https://hub.acme.com with
// base_domain apps.acme.com puts site foo at https://foo.apps.acme.com/). In
// path mode it is <public_url>/s/<site>/.
func (c *Config) SiteURL(site string) string {
	u, err := url.Parse(strings.TrimSpace(c.Server.PublicURL))
	if err != nil {
		return ""
	}
	if c.Server.URLMode == "path" {
		return strings.TrimSuffix(strings.TrimSpace(c.Server.PublicURL), "/") + "/s/" + site + "/"
	}
	base := bareHost(c.Server.BaseDomain)
	if base == "" {
		base = strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	}
	if base == "" {
		return ""
	}
	host := site + "." + base
	if p := u.Port(); p != "" {
		host = net.JoinHostPort(host, p)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "https"
	}
	return (&url.URL{Scheme: scheme, Host: host, Path: "/"}).String()
}

// bareHost normalizes a host name: trimmed, lowercased, without a trailing
// dot or a port.
func bareHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimSuffix(strings.Trim(h, "[]"), ".")
}

// CookieDomainCapable reports whether browsers accept ".<host>" as a cookie
// Domain attribute, which host-mode sessions need to reach site hosts. They
// refuse localhost, *.localhost, IP literals and single-label names.
// (Public suffixes such as co.uk are refused too but are not detected here.)
func CookieDomainCapable(host string) bool {
	h := bareHost(host)
	switch {
	case h == "", h == "localhost", strings.HasSuffix(h, ".localhost"),
		net.ParseIP(h) != nil, !strings.Contains(h, "."):
		return false
	}
	return true
}

// HubHost returns the host (with port, if any) of the public URL.
func (c *Config) HubHost() string {
	u, err := url.Parse(c.Server.PublicURL)
	if err != nil {
		return c.Server.BaseDomain
	}
	return u.Host
}

// Secure reports whether cookies should carry the Secure attribute.
func (c *Config) Secure() bool { return strings.HasPrefix(c.Server.PublicURL, "https://") }

// ParseCIDROrIP parses "10.0.0.0/8" or a bare IP into a *net.IPNet.
func ParseCIDROrIP(s string) (*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", s)
		}
		return n, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("invalid IP %q", s)
	}
	bits := 128
	if ip.To4() != nil {
		ip = ip.To4()
		bits = 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

func isLocalHost(h string) bool {
	return h == "localhost" || strings.HasSuffix(h, ".localhost") || h == "127.0.0.1" || strings.HasPrefix(h, "127.") || h == "lvh.me"
}

func setDefault(p *string, v string) {
	if *p == "" {
		*p = v
	}
}

func setDefaultInt(p *int, v int) {
	if *p == 0 {
		*p = v
	}
}

func normPrefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func firstModelOfKind(models map[string]ModelConfig, kind string) string {
	// Deterministic: prefer well-known alias names, then lexical order.
	pref := map[string][]string{
		"chat":      {"default", "smart", "chat", "fast"},
		"image":     {"image", "images"},
		"embedding": {"embed", "embedding", "embeddings"},
	}[kind]
	for _, p := range pref {
		if m, ok := models[p]; ok && (m.Kind == kind || (m.Kind == "" && kind == "chat")) {
			return p
		}
	}
	best := ""
	for name, m := range models {
		k := m.Kind
		if k == "" {
			k = "chat"
		}
		if k == kind && (best == "" || name < best) {
			best = name
		}
	}
	return best
}
