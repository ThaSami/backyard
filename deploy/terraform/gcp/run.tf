# Artifact Registry pull-through mirror and the Backyard Cloud Run service.

# Remote repository proxying the image's registry (ghcr.io by default). The
# first pull caches the image; Cloud Run then pulls it from Artifact Registry.
resource "google_artifact_registry_repository" "mirror" {
  count = local.mirror_image ? 1 : 0

  project       = var.project_id
  location      = var.region
  repository_id = local.mirror_repository_id
  description   = "Pull-through mirror of ${local.image_registry} for Backyard (Cloud Run cannot pull from it directly)."
  format        = "DOCKER"
  mode          = "REMOTE_REPOSITORY"
  labels        = local.labels

  remote_repository_config {
    description = "${local.image_registry} (public)"

    docker_repository {
      public_repository = local.image_is_docker ? "DOCKER_HUB" : null

      dynamic "custom_repository" {
        for_each = local.image_is_docker ? [] : [1]
        content {
          uri = "https://${local.image_registry}"
        }
      }
    }
  }

  depends_on = [google_project_service.apis]
}

locals {
  # Secret-backed environment variables: module-managed secrets are pinned to
  # the version Terraform wrote (a change rolls out a new revision); external
  # secrets follow "latest" (redeploy to pick up a rotation).
  secret_env = merge(
    { for env, _ in local.managed_secrets : env => {
      secret  = google_secret_manager_secret.managed[env].id
      version = google_secret_manager_secret_version.managed[env].version
    } },
    { for env, id in local.external_secrets : env => {
      secret  = id
      version = "latest"
    } },
  )
}

resource "google_cloud_run_v2_service" "backyard" {
  project  = var.project_id
  name     = local.service_name
  location = var.region
  labels   = local.labels

  description = "Backyard: internal static sites with a zero-config backend (${var.domain})."

  # Only the load balancer may reach the service, so IAP cannot be bypassed;
  # with the default run.app URL disabled not even VPC-internal callers can.
  ingress              = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
  default_uri_disabled = var.disable_default_url
  # The load balancer does not attach Google identity tokens (IAP authenticates
  # users; the deploy backend relies on Backyard bearer tokens), so Cloud Run's
  # own invoker check must be off. Ingress above keeps the service private.
  invoker_iam_disabled = true
  deletion_protection  = var.deletion_protection

  template {
    service_account                  = google_service_account.runtime.email
    execution_environment            = "EXECUTION_ENVIRONMENT_GEN2"
    max_instance_request_concurrency = var.max_concurrency
    # Websockets live as long as the request: allow the 60-minute maximum.
    timeout = "3600s"
    # Keep a browser on one instance (cookie-based) so realtime reconnects are
    # cheap; correctness does not depend on it (Postgres fans events out).
    session_affinity = true
    labels           = local.labels

    scaling {
      min_instance_count = var.min_instances
      max_instance_count = var.max_instances
    }

    containers {
      name  = "backyard"
      image = local.effective_image
      args  = var.container_args

      ports {
        name           = "http1"
        container_port = 8080 # Cloud Run sets PORT=8080; Backyard listens on :$PORT
      }

      resources {
        limits = {
          cpu    = var.cpu
          memory = var.memory
        }
        # CPU always allocated: websockets, presence heartbeats and LISTEN
        # connections run between requests.
        cpu_idle          = false
        startup_cpu_boost = true
      }

      env {
        name  = "BACKYARD_CONFIG_YAML"
        value = local.backyard_config_yaml
      }

      dynamic "env" {
        for_each = local.secret_env
        content {
          name = env.key
          value_source {
            secret_key_ref {
              secret  = env.value.secret
              version = env.value.version
            }
          }
        }
      }

      dynamic "env" {
        for_each = var.extra_env
        content {
          name  = env.key
          value = env.value
        }
      }

      dynamic "volume_mounts" {
        for_each = var.database == "cloudsql" ? [1] : []
        content {
          name       = "cloudsql"
          mount_path = "/cloudsql"
        }
      }

      # Readiness at startup: database and bucket must answer before the
      # revision receives traffic (also covers schema migrations).
      startup_probe {
        initial_delay_seconds = 0
        period_seconds        = 5
        timeout_seconds       = 5
        failure_threshold     = 36 # 3 minutes

        http_get {
          path = "/__backyard/readyz"
          port = 8080
        }
      }

      liveness_probe {
        period_seconds    = 30
        timeout_seconds   = 5
        failure_threshold = 3

        http_get {
          path = "/__backyard/healthz"
          port = 8080
        }
      }
    }

    dynamic "volumes" {
      for_each = var.database == "cloudsql" ? [1] : []
      content {
        name = "cloudsql"
        cloud_sql_instance {
          instances = [local.database_connection_name]
        }
      }
    }
  }

  traffic {
    type    = "TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST"
    percent = 100
  }

  lifecycle {
    # Set by gcloud / the console when someone deploys by hand.
    ignore_changes = [client, client_version]
  }

  depends_on = [
    terraform_data.validation,
    google_project_service.apis,
    google_artifact_registry_repository.mirror,
    google_secret_manager_secret_iam_member.managed,
    google_secret_manager_secret_iam_member.external,
    google_storage_bucket_iam_member.runtime_object_admin,
    google_service_account_iam_member.runtime_self_token_creator,
    google_project_iam_member.runtime_cloudsql_client,
    google_project_iam_member.runtime_vertex_ai,
    google_project_iam_member.runtime_bigquery_job_user,
    google_project_iam_member.runtime_bigquery_data_viewer,
    google_bigquery_dataset_iam_member.runtime_data_viewer,
    google_sql_database.backyard,
    google_sql_user.backyard,
  ]
}
