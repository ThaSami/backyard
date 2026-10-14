# Secret Manager secrets exposed to Cloud Run as environment variables.
#
# Managed here: BACKYARD_SECRET (sessions, API tokens, deploy ids), the
# database DSN, the OIDC client secret and AI provider keys. Their values are
# in Terraform state, so keep state in an encrypted, access-controlled
# backend (e.g. a GCS bucket). Secrets created outside Terraform
# (external_database_url_secret, extra_secret_env) are referenced, never read.

resource "random_password" "backyard_secret" {
  # Rotating it (terraform apply -replace=…) signs everyone out and revokes
  # every Backyard API token.
  length  = 64
  special = false
}

locals {
  manage_database_url = var.database == "cloudsql" || local.has_external_db_url

  # ENV name => secret id suffix. Keys depend only on non-secret inputs so they
  # are known at plan time.
  managed_secrets = merge(
    { BACKYARD_SECRET = "server-secret" },
    local.manage_database_url ? { DATABASE_URL = "database-url" } : {},
    var.auth_mode == "oidc" && local.has_oidc_client_secret ? { OIDC_CLIENT_SECRET = "oidc-client-secret" } : {},
    local.has_anthropic_key ? { ANTHROPIC_API_KEY = "anthropic-api-key" } : {},
    local.has_openai_key ? { OPENAI_API_KEY = "openai-api-key" } : {},
  )

  managed_secret_values = {
    BACKYARD_SECRET    = random_password.backyard_secret.result
    DATABASE_URL       = var.database == "cloudsql" ? local.cloudsql_dsn : var.external_database_url
    OIDC_CLIENT_SECRET = var.oidc_client_secret
    ANTHROPIC_API_KEY  = var.anthropic_api_key
    OPENAI_API_KEY     = var.openai_api_key
  }

  # ENV name => full secret resource name, for secrets managed elsewhere.
  external_secrets = {
    for env, id in merge(
      var.database == "external-postgres" && var.external_database_url_secret != "" ? { DATABASE_URL = var.external_database_url_secret } : {},
      var.extra_secret_env,
    ) : env => startswith(id, "projects/") ? id : "projects/${var.project_id}/secrets/${id}"
  }
}

resource "google_secret_manager_secret" "managed" {
  for_each = local.managed_secrets

  project   = var.project_id
  secret_id = "${var.name}-${each.value}"
  labels    = local.labels

  replication {
    auto {}
  }

  depends_on = [google_project_service.apis]
}

resource "google_secret_manager_secret_version" "managed" {
  for_each = local.managed_secrets

  secret      = google_secret_manager_secret.managed[each.key].id
  secret_data = local.managed_secret_values[each.key]

  lifecycle {
    # Cloud Run pins these versions: create the new one and roll out the new
    # revision before the old version is destroyed, so instances starting in
    # between can still read it.
    create_before_destroy = true
  }
}

resource "google_secret_manager_secret_iam_member" "managed" {
  for_each = local.managed_secrets

  project   = var.project_id
  secret_id = google_secret_manager_secret.managed[each.key].secret_id
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.runtime.member
}

resource "google_secret_manager_secret_iam_member" "external" {
  for_each = local.external_secrets

  project   = split("/", each.value)[1]
  secret_id = split("/", each.value)[3]
  role      = "roles/secretmanager.secretAccessor"
  member    = google_service_account.runtime.member
}
