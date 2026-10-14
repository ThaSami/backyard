# Configurations rendered by configcheck_test.go: each run's
# backyard_config_yaml output is loaded with Backyard's config loader.
# Google providers are mocked; nothing is created.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }

  mock_resource "google_compute_backend_service" {
    defaults = {
      generated_id = 4242424242
    }
  }

  mock_resource "google_compute_global_address" {
    defaults = {
      address = "203.0.113.10"
    }
  }

  mock_resource "google_sql_database_instance" {
    defaults = {
      connection_name = "acme-backyard:us-central1:backyard-pg-abc123"
    }
  }

  mock_resource "google_service_account" {
    defaults = {
      name   = "projects/acme-backyard/serviceAccounts/backyard-runtime@acme-backyard.iam.gserviceaccount.com"
      email  = "backyard-runtime@acme-backyard.iam.gserviceaccount.com"
      member = "serviceAccount:backyard-runtime@acme-backyard.iam.gserviceaccount.com"
    }
  }

  mock_resource "google_secret_manager_secret_version" {
    defaults = {
      version = "1"
    }
  }

  mock_resource "google_certificate_manager_dns_authorization" {
    defaults = {
      dns_resource_record = [{
        name = "_acme-challenge.backyard.example.com."
        type = "CNAME"
        data = "0123abcd.1.authorize.certificatemanager.goog."
      }]
    }
  }

  mock_resource "google_certificate_manager_certificate_map" {
    defaults = {
      id = "projects/acme-backyard/locations/global/certificateMaps/backyard-certs"
    }
  }
}

mock_provider "google-beta" {
  mock_resource "google_project_service_identity" {
    defaults = {
      email = "service-123456789012@gcp-sa-iap.iam.gserviceaccount.com"
    }
  }
}

variables {
  project_id  = "acme-backyard"
  domain      = "backyard.example.com"
  iap_members = ["domain:example.com"]
}

# Defaults: IAP, Cloud SQL, Vertex AI (Gemini + Claude).
run "iap_vertex" {}

# Built-in OIDC, an external database, first-party AI keys and most of the
# optional auth/server settings, with values that need YAML quoting and a
# literal dollar-brace sequence.
run "oidc_full" {
  variables {
    display_name = "Acme's \"Yard\" $${HOME}: #1"
    auth_mode    = "oidc"
    iap_members  = []
    oidc = {
      issuer           = "https://acme.okta.com/oauth2/default"
      client_id        = "0oa123"
      scopes           = ["openid", "profile", "email", "groups"]
      use_userinfo     = true
      device_client_id = "0oa456"
      api_audiences    = ["api://backyard"]
    }
    oidc_client_secret           = "oidc-secret"
    database                     = "external-postgres"
    external_database_url_secret = "projects/shared-db/secrets/backyard-dsn"
    vertex_ai                    = false
    anthropic_api_key            = "sk-ant-test"
    openai_api_key               = "sk-openai-test"
    openai_base_url              = "https://llm.internal.example.com/v1"
    ai_models = {
      gpt  = { provider = "openai", model = "gpt-5", description = "GPT: $${not_expanded}" }
      deep = { provider = "anthropic", model = "claude-opus-5-5", effort = "high", max_tokens = 32000 }
    }
    ai_default_model = "deep"
    admins           = ["alice@example.com"]
    admin_groups     = ["backyard-admins"]
    allowed_domains  = ["example.com"]
    allowed_emails   = ["pat@contractor.example"]
    allowed_groups   = ["eng"]
    static_tokens = [
      { name = "ci", sha256 = "0000000000000000000000000000000000000000000000000000000000000000", admin = true },
    ]
    trusted_proxies = ["10.0.0.0/8"]
    storage_prefix  = "/backyard"
    log_level       = "debug"
    access_log      = true
  }
}

# Anonymous labs setup in path mode without AI.
run "none_path" {
  variables {
    auth_mode       = "none"
    url_mode        = "path"
    vertex_ai       = false
    iap_members     = []
    security_policy = "backyard-allowlist"
  }
}

# BigQuery warehouse with raw SQL and a named query, direct bucket deploys
# and extra configuration sections with an environment reference.
run "warehouse_extra" {
  variables {
    bigquery_warehouse      = true
    bigquery_project_id     = "acme-analytics"
    bigquery_datasets       = ["events"]
    warehouse_allow_raw_sql = true
    warehouse_queries = {
      weekly_active_users = {
        sql         = "SELECT day, users, '$${literal}' AS tag FROM events.wau WHERE team = @team"
        description = "WAU per team"
        params      = ["team"]
      }
    }
    bucket_writer_members = ["group:eng@example.com"]
    extra_config_yaml     = <<-YAML
      identity:
        directory:
          type: csv
          path: $${DIRECTORY_PATH}
      sites:
        ownership: owner
        max_file_bytes: 10485760
      ratelimit:
        ai_per_minute: 5
    YAML
  }
}
