# Shared data sources, naming and the rendered Backyard configuration.
#
# Request flow (see README.md for the full picture):
#
#   browser ─▶ global external Application Load Balancer (HTTPS, cert map)
#                ├─ /__backyard/healthz, /__backyard/api/deploy/{info,uploads/*}
#                │  and bearer-token deploy calls ─▶ backend "deploy" (no IAP)
#                └─ everything else ─▶ backend "app" (IAP when auth_mode = iap)
#                                         └─▶ serverless NEG ─▶ Cloud Run (internal-LB ingress only)
#                                                               ├─ GCS bucket (sites, uploads)
#                                                               ├─ Cloud SQL Postgres (docs + LISTEN/NOTIFY)
#                                                               ├─ Vertex AI (Gemini, Claude)
#                                                               └─ BigQuery (warehouse)

data "google_project" "this" {
  project_id = var.project_id
}

locals {
  labels = merge({
    "app"        = "backyard"
    "managed-by" = "terraform"
  }, var.labels)

  public_url = "https://${var.domain}"
  host_mode  = var.url_mode == "host"
  iap        = var.auth_mode == "iap"

  # The Cloud Run service name is a plain string so the serverless NEG can
  # reference it without depending on the service (the service depends on the
  # IAP backend service id through BACKYARD_CONFIG_YAML).
  service_name = var.name

  bucket_name     = var.bucket_name != "" ? var.bucket_name : "${var.project_id}-${var.name}-sites"
  bucket_location = var.bucket_location != "" ? var.bucket_location : upper(var.region)

  dns_project      = var.dns_project_id != "" ? var.dns_project_id : var.project_id
  bigquery_project = var.bigquery_project_id != "" ? var.bigquery_project_id : var.project_id

  # Hostnames served by the load balancer / certificate.
  hostnames = local.host_mode ? [var.domain, "*.${var.domain}"] : [var.domain]

  # Booleans derived from secrets reveal only whether a value was given.
  has_oidc_client_secret = nonsensitive(var.oidc_client_secret != "")
  has_anthropic_key      = nonsensitive(var.anthropic_api_key != "")
  has_openai_key         = nonsensitive(var.openai_api_key != "")
  has_external_db_url    = nonsensitive(var.external_database_url != "")
  has_iap_oauth_client   = var.iap_oauth2_client_id != ""
}

# ---------------------------------------------------------------------------
# Container image: Cloud Run pulls only from Artifact Registry (and gcr.io /
# Docker Hub), so other registries such as ghcr.io are proxied through an
# Artifact Registry remote repository.

locals {
  image_ref       = regex("^(?:(?P<registry>[^/]*[.:][^/]*|localhost)/)?(?P<path>.+)$", var.image)
  image_registry  = lower(coalesce(local.image_ref.registry, "docker.io"))
  image_is_docker = contains(["docker.io", "index.docker.io", "registry-1.docker.io"], local.image_registry)
  # Docker Hub official images live under library/.
  image_path = local.image_is_docker && !strcontains(split("@", split(":", local.image_ref.path)[0])[0], "/") ? "library/${local.image_ref.path}" : local.image_ref.path

  image_in_artifact_registry = can(regex("(^|\\.)(gcr\\.io|pkg\\.dev)$", local.image_registry))
  mirror_image               = var.mirror_image && !local.image_in_artifact_registry
  mirror_repository_id       = "${var.name}-mirror"

  effective_image = local.mirror_image ? "${var.region}-docker.pkg.dev/${var.project_id}/${local.mirror_repository_id}/${local.image_path}" : var.image
}

# ---------------------------------------------------------------------------
# AI providers and model aliases (ai.providers / ai.models).

locals {
  ai_providers = merge(
    var.vertex_ai ? {
      vertex = { type = "gemini", platform = "", project_id = var.project_id, location = var.vertex_ai_location, api_key_env = "", base_url = "" }
    } : {},
    var.vertex_ai && var.vertex_claude_model != "" ? {
      claude = { type = "anthropic", platform = "vertex", project_id = var.project_id, location = var.vertex_ai_location, api_key_env = "", base_url = "" }
    } : {},
    local.has_anthropic_key ? {
      anthropic = { type = "anthropic", platform = "anthropic", project_id = "", location = "", api_key_env = "ANTHROPIC_API_KEY", base_url = "" }
    } : {},
    local.has_openai_key ? {
      openai = { type = "openai", platform = "", project_id = "", location = "", api_key_env = "OPENAI_API_KEY", base_url = var.openai_base_url }
    } : {},
  )

  ai_default_aliases = {
    smart = var.vertex_ai && var.vertex_claude_model != "" ? {
      provider = "claude", model = var.vertex_claude_model, kind = "chat", description = "Claude on Vertex AI: best quality", max_tokens = 0, effort = ""
      } : local.has_anthropic_key ? {
      provider = "anthropic", model = "claude-opus-5-5", kind = "chat", description = "Claude: best quality", max_tokens = 0, effort = ""
    } : null
    fast = var.vertex_ai && var.vertex_gemini_model != "" ? {
      provider = "vertex", model = var.vertex_gemini_model, kind = "chat", description = "Gemini on Vertex AI: fast and cheap", max_tokens = 0, effort = ""
      } : local.has_anthropic_key ? {
      provider = "anthropic", model = "claude-haiku-5-5", kind = "chat", description = "Claude: fast and cheap", max_tokens = 0, effort = ""
    } : null
    image = var.vertex_ai && var.vertex_image_model != "" ? {
      provider = "vertex", model = var.vertex_image_model, kind = "image", description = "Image generation (Vertex AI)", max_tokens = 0, effort = ""
    } : null
    embed = var.vertex_ai && var.vertex_embedding_model != "" ? {
      provider = "vertex", model = var.vertex_embedding_model, kind = "embedding", description = "Text embeddings (Vertex AI)", max_tokens = 0, effort = ""
    } : null
  }

  ai_models = merge(
    { for alias, m in local.ai_default_aliases : alias => m if m != null },
    var.ai_models,
  )
}

# ---------------------------------------------------------------------------
# Warehouse (BigQuery) datasets: "dataset" or "project.dataset".

locals {
  bigquery_datasets = {
    for d in var.bigquery_datasets : d => {
      project = length(split(".", replace(d, ":", "."))) == 2 ? split(".", replace(d, ":", "."))[0] : local.bigquery_project
      dataset = length(split(".", replace(d, ":", "."))) == 2 ? split(".", replace(d, ":", "."))[1] : d
    }
  }
}

# ---------------------------------------------------------------------------
# Rendered backyard.yaml (BACKYARD_CONFIG_YAML). Secrets never appear here:
# server.secret comes from BACKYARD_SECRET, the database DSN from DATABASE_URL
# (both applied by Backyard's environment overrides) and other secrets are
# environment references expanded at startup from Secret Manager-backed
# variables.
#
# Backyard expands ${VAR} anywhere in the document before parsing it, and
# "$${" is its escape for a literal "${". The template therefore writes the
# references it means as ${env}NAME} (env is a control character that
# jsonencode always escapes, so no rendered value can contain it); every
# other "${" in the output - e.g. inside SQL or a display name - is escaped,
# and only then do the markers become real references. extra_config_yaml is
# appended verbatim: its references are intentional.

locals {
  iap_audience = local.iap ? "/projects/${data.google_project.this.number}/global/backendServices/${google_compute_backend_service.app.generated_id}" : ""

  deploy_via_gcs = length(var.bucket_writer_members) > 0

  config_sections       = ["server", "auth", "storage", "database", "pubsub", "deploy", "ai", "warehouse", "telemetry"]
  extra_config_sections = ["identity", "sites", "files", "ratelimit", "realtime"]

  # null when empty or invalid (the precondition below reports invalid YAML).
  # No conditional here: sections of different shapes have no common type.
  extra_config      = try(yamldecode(var.extra_config_yaml), null)
  extra_config_keys = try(keys(local.extra_config), [])

  # Cloud Run limits HTTP/1 requests (and unchunked responses) to 32 MiB.
  # Backyard serves site files and uploads with a Content-Length, so larger
  # files would deploy fine but fail when served: cap them with headroom for
  # multipart overhead, unless extra_config_yaml takes over the section.
  cloud_run_body_limit = 31 * 1024 * 1024

  env_ref_marker = "\u0001{"

  backyard_config_rendered = templatefile("${path.module}/templates/backyard.yaml.tftpl", {
    env = local.env_ref_marker

    display_name    = var.display_name
    domain          = var.domain
    public_url      = local.public_url
    url_mode        = var.url_mode
    trusted_proxies = var.trusted_proxies

    auth_mode       = var.auth_mode
    iap_audience    = local.iap_audience
    admins          = var.admins
    admin_groups    = var.admin_groups
    allowed_domains = var.allowed_domains
    allowed_emails  = var.allowed_emails
    allowed_groups  = var.allowed_groups
    static_tokens   = var.static_tokens
    oidc = var.auth_mode == "oidc" && var.oidc != null ? {
      issuer           = var.oidc.issuer
      client_id        = var.oidc.client_id
      client_secret    = local.has_oidc_client_secret
      scopes           = var.oidc.scopes
      use_userinfo     = var.oidc.use_userinfo
      device_client_id = var.oidc.device_client_id
      api_audiences    = var.oidc.api_audiences
    } : null

    bucket         = local.bucket_name
    storage_prefix = var.storage_prefix
    signer_email   = google_service_account.runtime.email

    database_max_conns = var.database_max_conns

    deploy_methods = local.deploy_via_gcs ? ["api", "gcs"] : ["api"]
    expose_storage = local.deploy_via_gcs

    ai_providers     = local.ai_providers
    ai_models        = local.ai_models
    ai_default_model = var.ai_default_model

    warehouse = var.bigquery_warehouse ? {
      project_id       = local.bigquery_project
      location         = var.bigquery_location
      max_bytes_billed = var.bigquery_max_bytes_billed
      allow_raw_sql    = var.warehouse_allow_raw_sql
      queries          = var.warehouse_queries
    } : null

    render_files = !contains(local.extra_config_keys, "files")
    render_sites = !contains(local.extra_config_keys, "sites")
    body_limit   = local.cloud_run_body_limit

    log_level  = var.log_level
    access_log = var.access_log
  })

  backyard_config_yaml = join("", [
    replace(replace(local.backyard_config_rendered, "$${", "$$${"), local.env_ref_marker, "$${"),
    trimspace(var.extra_config_yaml) == "" ? "" : "${trimspace(var.extra_config_yaml)}\n",
  ])
}

# ---------------------------------------------------------------------------
# Cross-variable checks (variable validation blocks cannot reference other
# variables before Terraform 1.9).

resource "terraform_data" "validation" {
  input = var.name

  lifecycle {
    precondition {
      condition     = var.auth_mode != "oidc" || var.oidc != null
      error_message = "auth_mode = \"oidc\" requires the oidc variable (issuer and client_id)."
    }
    precondition {
      condition     = var.database != "external-postgres" || (local.has_external_db_url != (var.external_database_url_secret != ""))
      error_message = "database = \"external-postgres\" requires exactly one of external_database_url or external_database_url_secret."
    }
    precondition {
      condition     = var.min_instances <= var.max_instances
      error_message = "min_instances must not exceed max_instances."
    }
    precondition {
      condition     = (var.database_edition == "ENTERPRISE_PLUS") == startswith(var.database_tier, "db-perf-optimized-")
      error_message = "Cloud SQL ENTERPRISE_PLUS needs a db-perf-optimized-* tier, and db-perf-optimized-* tiers need ENTERPRISE_PLUS."
    }
    precondition {
      condition     = local.has_iap_oauth_client == nonsensitive(var.iap_oauth2_client_secret != "")
      error_message = "Set both iap_oauth2_client_id and iap_oauth2_client_secret, or neither (Google-managed OAuth client)."
    }
    precondition {
      condition     = alltrue([for alias, m in local.ai_models : contains(keys(local.ai_providers), m.provider)])
      error_message = "Every ai_models entry must use an enabled provider: ${join(", ", keys(local.ai_providers))} (vertex/claude need vertex_ai = true, anthropic/openai need their API keys)."
    }
    precondition {
      condition     = var.ai_default_model == "" || contains(keys(local.ai_models), var.ai_default_model)
      error_message = "ai_default_model must name one of the configured model aliases."
    }
    precondition {
      # It is appended to the generated document, so its sections must start
      # in column 0 (an indented block would nest under the last section).
      condition     = trimspace(var.extra_config_yaml) == "" || (can(keys(yamldecode(var.extra_config_yaml))) && can(regex("^(?:[ \t]*\n)*[^ \t\n]", var.extra_config_yaml)))
      error_message = "extra_config_yaml must be a YAML mapping of top-level backyard.yaml sections, starting in column 0."
    }
    precondition {
      condition     = length(setintersection(local.extra_config_keys, local.config_sections)) == 0
      error_message = "extra_config_yaml must not repeat sections generated by this module (${join(", ", local.config_sections)}); use the module variables instead."
    }
    precondition {
      # Backyard rejects unknown keys at startup, which would only show up as
      # a Cloud Run revision that never becomes ready.
      condition     = length(setsubtract(local.extra_config_keys, concat(local.config_sections, local.extra_config_sections))) == 0
      error_message = "extra_config_yaml may only add these backyard.yaml sections: ${join(", ", local.extra_config_sections)}."
    }
    precondition {
      condition     = length(setintersection(keys(var.extra_env), keys(var.extra_secret_env))) == 0
      error_message = "extra_env and extra_secret_env must not define the same variable."
    }
    precondition {
      condition     = length(var.warehouse_queries) == 0 || var.bigquery_warehouse
      error_message = "warehouse_queries requires bigquery_warehouse = true."
    }
    precondition {
      condition     = length(var.bigquery_datasets) == 0 || var.bigquery_warehouse
      error_message = "bigquery_datasets requires bigquery_warehouse = true."
    }
  }
}

check "iap_access" {
  assert {
    condition     = !local.iap || length(var.iap_members) > 0
    error_message = "auth_mode = \"iap\" but iap_members is empty: this module does not manage roles/iap.httpsResourceAccessor, so nobody passes IAP until access is granted elsewhere."
  }
}

check "public_without_auth" {
  assert {
    condition     = var.auth_mode != "none" || var.security_policy != ""
    error_message = "auth_mode = \"none\" without a Cloud Armor security_policy puts every site, the database API and the AI proxy on the public internet. Restrict source IPs with security_policy or use auth_mode = \"iap\"/\"oidc\"."
  }
}

check "warehouse_usable" {
  assert {
    condition     = !var.bigquery_warehouse || var.warehouse_allow_raw_sql || length(var.warehouse_queries) > 0
    error_message = "bigquery_warehouse = true but sites can run neither raw SQL (warehouse_allow_raw_sql) nor named queries (warehouse_queries)."
  }
}

check "cloud_run_body_limits" {
  assert {
    condition = alltrue([
      for section, key in { files = "max_upload_bytes", sites = "max_file_bytes" } :
      !contains(local.extra_config_keys, section) || try(tonumber(local.extra_config[section][key]) <= local.cloud_run_body_limit, false)
    ])
    error_message = "extra_config_yaml replaces the files/sites sections: set files.max_upload_bytes and sites.max_file_bytes to at most ${local.cloud_run_body_limit} (31 MiB). Cloud Run rejects larger HTTP/1 requests and unchunked responses, so bigger files would deploy but fail when served."
  }
}

# Cloud SQL connection budget. Per instance Backyard opens up to
# database_max_conns document-store connections, up to max(4, vCPUs) pooled
# realtime publisher connections and one LISTEN connection. Cloud SQL's
# default max_connections depends on the tier's memory; 3 are reserved for
# superusers.
locals {
  cloudsql_memory_gb = (
    var.database_tier == "db-f1-micro" ? 0.6 :
    var.database_tier == "db-g1-small" ? 1.7 :
    can(regex("^db-custom-[0-9]+-[0-9]+$", var.database_tier)) ? tonumber(split("-", var.database_tier)[3]) / 1024 :
    null
  )
  cloudsql_default_max_connections = local.cloudsql_memory_gb == null ? null : (
    local.cloudsql_memory_gb >= 120 ? 1000 :
    local.cloudsql_memory_gb >= 60 ? 800 :
    local.cloudsql_memory_gb >= 30 ? 600 :
    local.cloudsql_memory_gb >= 15 ? 500 :
    local.cloudsql_memory_gb >= 7.5 ? 400 :
    local.cloudsql_memory_gb >= 6 ? 200 :
    local.cloudsql_memory_gb >= 3.75 ? 100 :
    local.cloudsql_memory_gb >= 1.7 ? 50 : 25
  )
  cloudsql_max_connections    = var.database_max_connections != null ? var.database_max_connections : local.cloudsql_default_max_connections
  database_conns_per_instance = var.database_max_conns + max(4, tonumber(var.cpu)) + 1
  database_conns_needed       = var.max_instances * local.database_conns_per_instance
}

check "database_connections" {
  assert {
    condition     = var.database != "cloudsql" || local.cloudsql_max_connections == null || local.database_conns_needed <= coalesce(local.cloudsql_max_connections, 0) - 3
    error_message = "At max_instances = ${var.max_instances} Backyard may open ${local.database_conns_needed} database connections (${local.database_conns_per_instance} per instance), more than the ${coalesce(local.cloudsql_max_connections, 3) - 3} Cloud SQL allows: lower max_instances or database_max_conns, pick a larger database_tier or raise database_max_connections."
  }
}
