# ---------------------------------------------------------------------------
# Project, naming and URLs

variable "project_id" {
  description = "Google Cloud project to deploy Backyard into."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid Google Cloud project ID (6-30 lowercase letters, digits or hyphens)."
  }
}

variable "region" {
  description = "Region for Cloud Run, Cloud SQL, the serverless NEG and the Artifact Registry mirror (e.g. us-central1, europe-west1)."
  type        = string
  default     = "us-central1"

  validation {
    condition     = can(regex("^[a-z]+-[a-z]+[0-9]+$", var.region))
    error_message = "region must be a Google Cloud region such as us-central1."
  }
}

variable "name" {
  description = "Name prefix for every resource (Cloud Run service, load balancer, secrets, …)."
  type        = string
  default     = "backyard"

  validation {
    condition     = can(regex("^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$", var.name))
    error_message = "name must be 1-20 lowercase letters, digits or hyphens, start with a letter and not end with a hyphen."
  }
}

variable "domain" {
  description = "Base domain of the hub, e.g. backyard.example.com. Sites are served at <site>.<domain> (url_mode = \"host\") or <domain>/s/<site>/ (url_mode = \"path\")."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$", var.domain))
    error_message = "domain must be a lowercase fully-qualified domain name without a trailing dot, e.g. backyard.example.com."
  }
}

variable "url_mode" {
  description = "\"host\" serves sites on wildcard subdomains (needs the wildcard certificate and DNS record this module creates); \"path\" serves them under /s/<site>/ on the hub host."
  type        = string
  default     = "host"

  validation {
    condition     = contains(["host", "path"], var.url_mode)
    error_message = "url_mode must be \"host\" or \"path\"."
  }
}

variable "display_name" {
  description = "Name shown in the hub UI (server.name)."
  type        = string
  default     = "Backyard"
}

variable "labels" {
  description = "Labels added to every resource that supports them."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for k, v in var.labels : can(regex("^[a-z][a-z0-9_-]{0,62}$", k)) && can(regex("^[a-z0-9_-]{0,63}$", v))])
    error_message = "Label keys must start with a lowercase letter; keys and values may contain only lowercase letters, digits, '_' and '-' (max 63 characters)."
  }
}

variable "enable_apis" {
  description = "Enable the required Google Cloud APIs. Set to false when APIs are managed elsewhere (e.g. by a project factory)."
  type        = bool
  default     = true
}

variable "deletion_protection" {
  description = "Protect stateful/expensive resources (Cloud Run service, Cloud SQL instance) from accidental deletion. Set to false before `terraform destroy`."
  type        = bool
  default     = true
}

# ---------------------------------------------------------------------------
# Container and Cloud Run

variable "image" {
  description = "Backyard container image. Images outside Artifact Registry / gcr.io (e.g. the default ghcr.io image) are pulled through an Artifact Registry remote repository because Cloud Run cannot pull from GHCR directly."
  type        = string
  default     = "ghcr.io/thasami/backyard:latest"

  validation {
    condition     = can(regex("^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$", var.image)) && !can(regex("^https?://", var.image))
    error_message = "image must be a container image reference without a URL scheme, e.g. ghcr.io/thasami/backyard:0.1.0."
  }
}

variable "mirror_image" {
  description = "Create an Artifact Registry remote repository that proxies the image's registry when the image is not already in Artifact Registry / gcr.io. Disable only if Cloud Run can pull the image directly (e.g. Docker Hub)."
  type        = bool
  default     = true
}

variable "container_args" {
  description = "Override the container arguments (default: the image's CMD, which runs `backyard serve`)."
  type        = list(string)
  default     = null
}

variable "cpu" {
  description = "vCPUs per Cloud Run instance. CPU is always allocated (websockets and presence heartbeats need it), which requires at least 1 vCPU."
  type        = string
  default     = "1"

  validation {
    condition     = contains(["1", "2", "4", "6", "8"], var.cpu)
    error_message = "cpu must be one of 1, 2, 4, 6 or 8."
  }
}

variable "memory" {
  description = "Memory per Cloud Run instance, e.g. 512Mi or 1Gi."
  type        = string
  default     = "1Gi"

  validation {
    condition     = can(regex("^[1-9][0-9]*(Mi|Gi)$", var.memory))
    error_message = "memory must look like 512Mi or 2Gi."
  }
}

variable "min_instances" {
  description = "Minimum Cloud Run instances. Keep at least 1 to avoid cold starts and dropped websockets."
  type        = number
  default     = 1

  validation {
    condition     = var.min_instances >= 0 && var.min_instances <= 100 && floor(var.min_instances) == var.min_instances
    error_message = "min_instances must be an integer between 0 and 100."
  }
}

variable "max_instances" {
  description = "Maximum Cloud Run instances. Instances share state through Cloud SQL (documents) and Postgres LISTEN/NOTIFY (realtime fan-out)."
  type        = number
  default     = 10

  validation {
    condition     = var.max_instances >= 1 && var.max_instances <= 1000 && floor(var.max_instances) == var.max_instances
    error_message = "max_instances must be an integer between 1 and 1000."
  }
}

variable "max_concurrency" {
  description = "Maximum concurrent requests per instance. Every open websocket counts as one request for its whole lifetime."
  type        = number
  default     = 250

  validation {
    condition     = var.max_concurrency >= 1 && var.max_concurrency <= 1000 && floor(var.max_concurrency) == var.max_concurrency
    error_message = "max_concurrency must be an integer between 1 and 1000."
  }
}

variable "disable_default_url" {
  description = "Disable the service's default run.app URL so the load balancer (and therefore IAP) is the only way in, including from VPC networks in the same project."
  type        = bool
  default     = true
}

variable "extra_env" {
  description = "Additional plain environment variables for the Backyard container."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for k in keys(var.extra_env) : can(regex("^[A-Za-z_][A-Za-z0-9_]*$", k)) && !contains(["PORT", "K_SERVICE", "K_REVISION", "K_CONFIGURATION", "BACKYARD_CONFIG_YAML", "BACKYARD_SECRET", "DATABASE_URL", "OIDC_CLIENT_SECRET", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"], k)])
    error_message = "extra_env keys must be valid environment variable names and must not override variables set by the module or Cloud Run (PORT, K_*, BACKYARD_CONFIG_YAML, BACKYARD_SECRET, DATABASE_URL, OIDC_CLIENT_SECRET, ANTHROPIC_API_KEY, OPENAI_API_KEY)."
  }
}

variable "extra_secret_env" {
  description = "Additional environment variables read from existing Secret Manager secrets (latest version), as ENV_NAME => secret id (\"name\" or \"projects/<p>/secrets/<name>\"). The runtime service account is granted secretAccessor on each. Reference them from extra_config_yaml as $${ENV_NAME}."
  type        = map(string)
  default     = {}

  validation {
    condition = alltrue([for k, v in var.extra_secret_env :
      can(regex("^[A-Za-z_][A-Za-z0-9_]*$", k)) && can(regex("^(projects/[^/]+/secrets/)?[A-Za-z0-9_-]{1,255}$", v)) &&
      !contains(["PORT", "K_SERVICE", "K_REVISION", "K_CONFIGURATION", "BACKYARD_CONFIG_YAML", "BACKYARD_SECRET", "DATABASE_URL", "OIDC_CLIENT_SECRET", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"], k)
    ])
    error_message = "extra_secret_env keys must be environment variable names not set by the module or Cloud Run, and values secret ids (\"name\" or \"projects/<project>/secrets/<name>\")."
  }
}

# ---------------------------------------------------------------------------
# Authentication

variable "auth_mode" {
  description = "\"iap\" (Identity-Aware Proxy on the load balancer, like Shopify Quick), \"oidc\" (Backyard's built-in OpenID Connect login: Okta, Entra ID, Google, …; no IAP) or \"none\" (anonymous; labs only)."
  type        = string
  default     = "iap"

  validation {
    condition     = contains(["iap", "oidc", "none"], var.auth_mode)
    error_message = "auth_mode must be \"iap\", \"oidc\" or \"none\"."
  }
}

variable "iap_members" {
  description = "Principals allowed through IAP (roles/iap.httpsResourceAccessor), e.g. [\"domain:example.com\", \"group:eng@example.com\", \"user:alice@example.com\"]. When non-empty the binding is authoritative for the role on the Backyard backend service; leave it empty to manage IAP access outside this module."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for m in var.iap_members : can(regex("^(user|group|domain|serviceAccount|principal|principalSet):.+$", m))])
    error_message = "iap_members entries must be IAM principals such as domain:example.com, group:eng@example.com or user:alice@example.com."
  }
}

variable "iap_oauth2_client_id" {
  description = "Custom OAuth client ID for IAP. Leave empty to use the Google-managed OAuth client, which only admits users of the project's organization."
  type        = string
  default     = ""
}

variable "iap_oauth2_client_secret" {
  description = "Custom OAuth client secret for IAP (with iap_oauth2_client_id)."
  type        = string
  default     = ""
  sensitive   = true
}

variable "oidc" {
  description = "Built-in OIDC login settings (auth_mode = \"oidc\"). The client secret is passed separately in oidc_client_secret. Register <https://DOMAIN>/__backyard/auth/callback as the redirect URI."
  type = object({
    issuer           = string
    client_id        = string
    scopes           = optional(list(string), [])
    use_userinfo     = optional(bool, false)
    device_client_id = optional(string, "")
    api_audiences    = optional(list(string), [])
  })
  default = null

  validation {
    condition     = var.oidc == null ? true : try(startswith(var.oidc.issuer, "https://") && trimspace(var.oidc.client_id) != "", false)
    error_message = "oidc.issuer must be an https:// URL and oidc.client_id must be set."
  }
}

variable "oidc_client_secret" {
  description = "OIDC client secret (stored in Secret Manager). Leave empty for public clients."
  type        = string
  default     = ""
  sensitive   = true

  validation {
    # Backyard expands ${ENV} references before parsing the YAML, so the
    # value lands inside a double-quoted YAML string.
    condition     = !can(regex("[\"\\\\\\x00-\\x1f]", var.oidc_client_secret))
    error_message = "oidc_client_secret must not contain double quotes, backslashes or control characters."
  }
}

variable "admins" {
  description = "Backyard admin user ids / emails (auth.admins)."
  type        = list(string)
  default     = []
}

variable "admin_groups" {
  description = "Groups whose members are Backyard admins (auth.admin_groups; needs a groups claim, i.e. OIDC)."
  type        = list(string)
  default     = []
}

variable "allowed_domains" {
  description = "Only admit users with these email domains (auth.allowed_domains; empty = any). Defense in depth on top of IAP."
  type        = list(string)
  default     = []
}

variable "allowed_emails" {
  description = "Also admit these exact email addresses (auth.allowed_emails): with allowed_domains either admits; alone, only the listed addresses get in."
  type        = list(string)
  default     = []
}

variable "allowed_groups" {
  description = "Only admit members of these groups (auth.allowed_groups; empty = any)."
  type        = list(string)
  default     = []
}

variable "static_tokens" {
  description = "Static bearer tokens for CI (auth.tokens). Store only the SHA-256 hex of each token (`backyard token hash <token>`)."
  type = list(object({
    name   = string
    sha256 = string
    admin  = optional(bool, false)
  }))
  default = []

  validation {
    condition     = alltrue([for t in var.static_tokens : trimspace(t.name) != "" && can(regex("^[0-9a-f]{64}$", t.sha256))])
    error_message = "static_tokens entries need a name and a 64-character lowercase hex sha256."
  }
}

variable "trusted_proxies" {
  description = "CIDRs whose X-Forwarded-* headers Backyard trusts (server.trusted_proxies). Empty by default: Cloud Run preserves the Host header and Google Front Ends pass client-supplied X-Forwarded-Host through, so trusting the platform would let clients spoof the site host."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for c in var.trusted_proxies : can(cidrhost(c, 0))])
    error_message = "trusted_proxies entries must be CIDRs, e.g. 10.0.0.0/8."
  }
}

# ---------------------------------------------------------------------------
# Database

variable "database" {
  description = "\"cloudsql\" creates a Cloud SQL for PostgreSQL instance reached over the Cloud SQL Unix socket; \"external-postgres\" uses an existing PostgreSQL (AlloyDB, self-managed, …) via a DSN secret."
  type        = string
  default     = "cloudsql"

  validation {
    condition     = contains(["cloudsql", "external-postgres"], var.database)
    error_message = "database must be \"cloudsql\" or \"external-postgres\"."
  }
}

variable "database_version" {
  description = "Cloud SQL PostgreSQL major version."
  type        = string
  default     = "POSTGRES_16"

  validation {
    condition     = can(regex("^POSTGRES_(1[4-9]|[2-9][0-9])$", var.database_version))
    error_message = "database_version must be POSTGRES_14 or newer, e.g. POSTGRES_16."
  }
}

variable "database_tier" {
  description = "Cloud SQL machine tier, e.g. db-custom-1-3840 (1 vCPU, 3.75 GB), db-g1-small (shared core, dev only) or db-perf-optimized-N-2 (Enterprise Plus)."
  type        = string
  default     = "db-custom-1-3840"

  validation {
    condition     = can(regex("^db-[a-z0-9-]+$", var.database_tier))
    error_message = "database_tier must be a Cloud SQL tier such as db-custom-1-3840."
  }
}

variable "database_edition" {
  description = "Cloud SQL edition: ENTERPRISE (default; any tier) or ENTERPRISE_PLUS (db-perf-optimized-* tiers only)."
  type        = string
  default     = "ENTERPRISE"

  validation {
    condition     = contains(["ENTERPRISE", "ENTERPRISE_PLUS"], var.database_edition)
    error_message = "database_edition must be ENTERPRISE or ENTERPRISE_PLUS."
  }
}

variable "database_high_availability" {
  description = "Run Cloud SQL as a regional (HA, two-zone) instance. Doubles the instance cost."
  type        = bool
  default     = false
}

variable "database_disk_size_gb" {
  description = "Initial Cloud SQL disk size in GB (auto-grows)."
  type        = number
  default     = 10

  validation {
    condition     = var.database_disk_size_gb >= 10
    error_message = "database_disk_size_gb must be at least 10."
  }
}

variable "database_point_in_time_recovery" {
  description = "Enable Cloud SQL point-in-time recovery (write-ahead log retention) in addition to daily backups."
  type        = bool
  default     = true
}

variable "database_max_conns" {
  description = "Document-store connection pool size per Cloud Run instance (database.postgres.max_conns). Each instance also opens up to max(4, cpu) realtime publisher connections and one LISTEN connection, so keep max_instances * (database_max_conns + max(4, cpu) + 1) below the instance's max_connections minus 3 reserved (100 for db-custom-1-3840); the module warns when the budget is exceeded."
  type        = number
  default     = 4

  validation {
    condition     = var.database_max_conns >= 2 && var.database_max_conns <= 200
    error_message = "database_max_conns must be between 2 and 200."
  }
}

variable "database_max_connections" {
  description = "Cloud SQL max_connections flag. Null keeps Cloud SQL's memory-based default (100 for 3.75-6 GB). Raising it restarts the instance."
  type        = number
  default     = null

  validation {
    condition     = var.database_max_connections == null ? true : (var.database_max_connections >= 14 && var.database_max_connections <= 262142 && floor(var.database_max_connections) == var.database_max_connections)
    error_message = "database_max_connections must be an integer between 14 and 262142."
  }
}

variable "external_database_url" {
  description = "DSN of an existing PostgreSQL for database = \"external-postgres\" (stored in Secret Manager by this module). Prefer external_database_url_secret to keep the DSN out of Terraform state."
  type        = string
  default     = ""
  sensitive   = true
}

variable "external_database_url_secret" {
  description = "Existing Secret Manager secret (\"name\" or \"projects/<p>/secrets/<name>\") whose latest version is the PostgreSQL DSN, for database = \"external-postgres\"."
  type        = string
  default     = ""

  validation {
    condition     = var.external_database_url_secret == "" || can(regex("^(projects/[^/]+/secrets/)?[A-Za-z0-9_-]{1,255}$", var.external_database_url_secret))
    error_message = "external_database_url_secret must be a secret id (\"name\" or \"projects/<project>/secrets/<name>\")."
  }
}

# ---------------------------------------------------------------------------
# Storage

variable "bucket_name" {
  description = "GCS bucket for site files and uploads. Default: <project_id>-<name>-sites."
  type        = string
  default     = ""

  validation {
    condition     = var.bucket_name == "" || can(regex("^[a-z0-9][a-z0-9_.-]{1,61}[a-z0-9]$", var.bucket_name))
    error_message = "bucket_name must be a valid GCS bucket name."
  }
}

variable "bucket_location" {
  description = "Bucket location. Default: the region (lowest latency to Cloud Run)."
  type        = string
  default     = ""
}

variable "bucket_versioning" {
  description = "Keep previous versions of overwritten/deleted site files (object versioning)."
  type        = bool
  default     = false
}

variable "bucket_noncurrent_version_days" {
  description = "With bucket_versioning, delete non-current object versions after this many days."
  type        = number
  default     = 30

  validation {
    condition     = var.bucket_noncurrent_version_days >= 1
    error_message = "bucket_noncurrent_version_days must be at least 1."
  }
}

variable "bucket_soft_delete_days" {
  description = "Soft-delete retention for the bucket in days (0 disables soft delete; sites are overwritten often, so retained bytes cost money)."
  type        = number
  default     = 7

  validation {
    condition     = var.bucket_soft_delete_days == 0 || (var.bucket_soft_delete_days >= 7 && var.bucket_soft_delete_days <= 90)
    error_message = "bucket_soft_delete_days must be 0 or between 7 and 90."
  }
}

variable "bucket_force_destroy" {
  description = "Allow `terraform destroy` to delete the bucket even if it still contains objects."
  type        = bool
  default     = false
}

variable "storage_prefix" {
  description = "Optional key prefix inside the bucket (storage.gcs.prefix), e.g. \"backyard/\"."
  type        = string
  default     = ""

  validation {
    condition     = can(regex("^[A-Za-z0-9._/-]*$", var.storage_prefix)) && !contains(split("/", var.storage_prefix), "..") && !contains(split("/", var.storage_prefix), ".")
    error_message = "storage_prefix may only contain letters, digits, '.', '_', '-' and '/', without \".\" or \"..\" segments."
  }
}

variable "bucket_writer_members" {
  description = "Principals allowed to deploy by syncing straight into the bucket (`backyard deploy --method gcs`, i.e. gcloud storage rsync), granted roles/storage.objectUser on objects below <storage_prefix>sites/ only (IAM condition) and bucket listing. When non-empty the gcs method and bucket details are advertised to CLIs."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for m in var.bucket_writer_members : can(regex("^(user|group|domain|serviceAccount|principal|principalSet):.+$", m))])
    error_message = "bucket_writer_members entries must be IAM principals such as group:eng@example.com."
  }
}

# ---------------------------------------------------------------------------
# AI

variable "vertex_ai" {
  description = "Configure Vertex AI models (Gemini and Claude on Vertex) using the runtime service account; grants roles/aiplatform.user."
  type        = bool
  default     = true
}

variable "vertex_ai_location" {
  description = "Vertex AI location for Gemini and Claude requests (\"global\" gives the best availability)."
  type        = string
  default     = "global"

  validation {
    condition     = can(regex("^[a-z]+(-[a-z]+[0-9]+)?$", var.vertex_ai_location))
    error_message = "vertex_ai_location must be \"global\", a multi-region such as \"us\" or a region such as \"us-central1\"."
  }
}

variable "vertex_claude_model" {
  description = "Claude model on Vertex AI for the \"smart\" alias (enable it in Model Garden first). Empty disables Claude."
  type        = string
  default     = "claude-opus-5-5"
}

variable "vertex_gemini_model" {
  description = "Gemini model for the \"fast\" alias. Empty disables it."
  type        = string
  default     = "gemini-3.8-flash"
}

variable "vertex_image_model" {
  description = "Image model for the \"image\" alias (backyard.ai.image). Empty disables image generation."
  type        = string
  default     = "gemini-3.1-flash-image"
}

variable "vertex_embedding_model" {
  description = "Embedding model for the \"embed\" alias (backyard.ai.embed). Empty disables embeddings."
  type        = string
  default     = "gemini-embedding-001"
}

variable "anthropic_api_key" {
  description = "Anthropic API key (Secret Manager) for a first-party \"anthropic\" provider. Without Vertex AI it also provides the default smart/fast aliases."
  type        = string
  default     = ""
  sensitive   = true

  validation {
    # Backyard expands ${ENV} references before parsing the YAML, so the
    # value lands inside a double-quoted YAML string.
    condition     = !can(regex("[\"\\\\\\x00-\\x1f]", var.anthropic_api_key))
    error_message = "anthropic_api_key must not contain double quotes, backslashes or control characters."
  }
}

variable "openai_api_key" {
  description = "OpenAI (or compatible) API key (Secret Manager) for an \"openai\" provider; reference it from ai_models."
  type        = string
  default     = ""
  sensitive   = true

  validation {
    # Backyard expands ${ENV} references before parsing the YAML, so the
    # value lands inside a double-quoted YAML string.
    condition     = !can(regex("[\"\\\\\\x00-\\x1f]", var.openai_api_key))
    error_message = "openai_api_key must not contain double quotes, backslashes or control characters."
  }
}

variable "openai_base_url" {
  description = "Base URL of an OpenAI-compatible API for the \"openai\" provider (empty = api.openai.com)."
  type        = string
  default     = ""

  validation {
    condition     = var.openai_base_url == "" || can(regex("^https?://[^\\s]+$", var.openai_base_url))
    error_message = "openai_base_url must be an http(s) URL."
  }
}

variable "ai_models" {
  description = "Extra or overriding model aliases (ai.models), merged over the defaults. provider is one of: vertex, claude (Claude on Vertex), anthropic, openai — whichever are enabled."
  type = map(object({
    provider    = string
    model       = string
    kind        = optional(string, "chat")
    description = optional(string, "")
    max_tokens  = optional(number, 0)
    effort      = optional(string, "")
  }))
  default = {}

  validation {
    condition = alltrue([for alias, m in var.ai_models :
      can(regex("^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$", alias)) && contains(["chat", "image", "embedding"], m.kind) && trimspace(m.model) != "" &&
      m.max_tokens >= 0 && floor(m.max_tokens) == m.max_tokens && contains(["", "low", "medium", "high", "xhigh", "max"], m.effort)
    ])
    error_message = "ai_models aliases must be simple names (no '/'), kind must be chat, image or embedding, model must be set, max_tokens a non-negative integer and effort empty or one of low, medium, high, xhigh, max."
  }
}

variable "ai_default_model" {
  description = "Default chat alias (ai.default_model). Empty: Backyard picks smart, then fast."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Warehouse (BigQuery)

variable "bigquery_warehouse" {
  description = "Expose BigQuery to sites through backyard.warehouse (source \"bigquery\"). Grants roles/bigquery.jobUser on bigquery_project_id and roles/bigquery.dataViewer on bigquery_datasets."
  type        = bool
  default     = false
}

variable "bigquery_project_id" {
  description = "Project that runs (and is billed for) BigQuery jobs. Default: project_id."
  type        = string
  default     = ""

  validation {
    condition     = var.bigquery_project_id == "" || can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.bigquery_project_id))
    error_message = "bigquery_project_id must be a valid Google Cloud project ID."
  }
}

variable "bigquery_datasets" {
  description = "Datasets the runtime may read, as \"dataset\" (in bigquery_project_id) or \"project.dataset\". Empty grants dataViewer on the whole bigquery_project_id."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for d in var.bigquery_datasets : can(regex("^([a-z][a-z0-9-]{4,28}[a-z0-9][.:])?[A-Za-z0-9_]+$", d))])
    error_message = "bigquery_datasets entries must look like \"dataset\" or \"project.dataset\"."
  }
}

variable "bigquery_location" {
  description = "BigQuery job location (e.g. US, EU, us-central1)."
  type        = string
  default     = "US"

  validation {
    condition     = can(regex("^[A-Za-z]+(-[a-z]+[0-9]+)?$", var.bigquery_location))
    error_message = "bigquery_location must be a BigQuery location such as US, EU or us-central1."
  }
}

variable "bigquery_max_bytes_billed" {
  description = "Per-query cap on bytes billed (0 = no cap)."
  type        = number
  default     = 10737418240 # 10 GiB

  validation {
    condition     = var.bigquery_max_bytes_billed >= 0 && floor(var.bigquery_max_bytes_billed) == var.bigquery_max_bytes_billed
    error_message = "bigquery_max_bytes_billed must be a non-negative integer."
  }
}

variable "warehouse_allow_raw_sql" {
  description = "Allow ad-hoc (SELECT-only) SQL from every site (warehouse.allow_raw_sql). Off by default, as in Backyard: every employee's page would otherwise read whatever the runtime service account can read. Prefer warehouse_queries, and keep bigquery_datasets narrow when enabling this."
  type        = bool
  default     = false
}

variable "warehouse_queries" {
  description = "Named, pre-approved queries (warehouse.queries) using @name parameters."
  type = map(object({
    sql         = string
    description = optional(string, "")
    params      = optional(list(string), [])
  }))
  default = {}

  validation {
    condition = alltrue([for name, q in var.warehouse_queries :
      can(regex("^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$", name)) && trimspace(q.sql) != "" &&
      alltrue([for p in q.params : can(regex("^[A-Za-z_][A-Za-z0-9_]*$", p))])
    ])
    error_message = "warehouse_queries names must be simple identifiers, sql must be set and params must be @name parameter names."
  }
}

# ---------------------------------------------------------------------------
# Load balancer, certificates and DNS

variable "dns_zone_name" {
  description = "Cloud DNS managed zone (name, not DNS name) that serves domain. When set, the A/AAAA records and the certificate DNS-authorization CNAME are created; otherwise create the records from the dns_records output yourself."
  type        = string
  default     = ""

  validation {
    condition     = var.dns_zone_name == "" || can(regex("^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$", var.dns_zone_name))
    error_message = "dns_zone_name must be a Cloud DNS managed zone name (not its DNS name), e.g. example-com."
  }
}

variable "dns_project_id" {
  description = "Project of dns_zone_name. Default: project_id."
  type        = string
  default     = ""
}

variable "dns_ttl" {
  description = "TTL of the DNS records created in dns_zone_name."
  type        = number
  default     = 300

  validation {
    condition     = var.dns_ttl >= 1 && floor(var.dns_ttl) == var.dns_ttl
    error_message = "dns_ttl must be a positive integer (seconds)."
  }
}

variable "certificate_map" {
  description = "Use an existing Certificate Manager certificate map (id: projects/<p>/locations/global/certificateMaps/<name>) instead of creating managed certificates."
  type        = string
  default     = ""

  validation {
    condition     = var.certificate_map == "" || can(regex("^projects/[^/]+/locations/global/certificateMaps/[^/]+$", var.certificate_map))
    error_message = "certificate_map must be a certificate map id: projects/<project>/locations/global/certificateMaps/<name>."
  }
}

variable "enable_ipv6" {
  description = "Also serve on a global IPv6 address (AAAA records)."
  type        = bool
  default     = false
}

variable "http_redirect" {
  description = "Listen on port 80 and redirect to HTTPS."
  type        = bool
  default     = true
}

variable "security_policy" {
  description = "Cloud Armor security policy (name or self link) attached to the backend services."
  type        = string
  default     = ""
}

variable "lb_log_sample_rate" {
  description = "Fraction of load balancer requests logged (0 disables request logging)."
  type        = number
  default     = 1

  validation {
    condition     = var.lb_log_sample_rate >= 0 && var.lb_log_sample_rate <= 1
    error_message = "lb_log_sample_rate must be between 0 and 1."
  }
}

# ---------------------------------------------------------------------------
# Backyard configuration

variable "log_level" {
  description = "Backyard log level (telemetry.log_level)."
  type        = string
  default     = "info"

  validation {
    condition     = contains(["debug", "info", "warn", "error"], var.log_level)
    error_message = "log_level must be debug, info, warn or error."
  }
}

variable "access_log" {
  description = "Log every HTTP request in Backyard (telemetry.access_log). The load balancer already logs requests."
  type        = bool
  default     = false
}

variable "extra_config_yaml" {
  description = "Additional top-level backyard.yaml sections not generated by this module (identity, sites, files, ratelimit, realtime, mcp; mcp.enabled turns on the Claude connector). $${ENV} references are expanded at startup. A sites or files section replaces the module's Cloud Run size caps (sites.max_file_bytes, files.max_upload_bytes), so keep those at or below 31 MiB."
  type        = string
  default     = ""
}
