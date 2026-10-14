# Runtime identity for the Backyard Cloud Run service, with least-privilege
# grants. Bucket and secret grants live next to those resources
# (storage.tf, secrets.tf).

resource "google_service_account" "runtime" {
  project      = var.project_id
  account_id   = "${var.name}-runtime"
  display_name = "Backyard runtime (${var.name})"
  description  = "Identity of the ${local.service_name} Cloud Run service."

  depends_on = [google_project_service.apis]
}

# V4 signed URLs: Cloud Run credentials carry no private key, so Backyard signs
# upload URLs with the IAM Credentials SignBlob API as itself.
resource "google_service_account_iam_member" "runtime_self_token_creator" {
  service_account_id = google_service_account.runtime.name
  role               = "roles/iam.serviceAccountTokenCreator"
  member             = google_service_account.runtime.member
}

# Cloud SQL connector (the Cloud Run /cloudsql volume), limited to this
# instance with an IAM condition.
resource "google_project_iam_member" "runtime_cloudsql_client" {
  count = var.database == "cloudsql" ? 1 : 0

  project = var.project_id
  role    = "roles/cloudsql.client"
  member  = google_service_account.runtime.member

  condition {
    title       = "${var.name}-cloudsql-instance"
    description = "Only the Backyard Cloud SQL instance."
    expression  = "resource.name == \"projects/${var.project_id}/instances/${google_sql_database_instance.backyard[0].name}\" && resource.service == \"sqladmin.googleapis.com\""
  }
}

# Gemini and Claude on Vertex AI.
resource "google_project_iam_member" "runtime_vertex_ai" {
  count = var.vertex_ai ? 1 : 0

  project = var.project_id
  role    = "roles/aiplatform.user"
  member  = google_service_account.runtime.member
}

# BigQuery warehouse: run jobs in (and bill them to) the warehouse project ...
resource "google_project_iam_member" "runtime_bigquery_job_user" {
  count = var.bigquery_warehouse ? 1 : 0

  project = local.bigquery_project
  role    = "roles/bigquery.jobUser"
  member  = google_service_account.runtime.member
}

# ... and read the listed datasets (or, with none listed, the whole project).
resource "google_bigquery_dataset_iam_member" "runtime_data_viewer" {
  for_each = var.bigquery_warehouse ? local.bigquery_datasets : {}

  project    = each.value.project
  dataset_id = each.value.dataset
  role       = "roles/bigquery.dataViewer"
  member     = google_service_account.runtime.member
}

resource "google_project_iam_member" "runtime_bigquery_data_viewer" {
  count = var.bigquery_warehouse && length(var.bigquery_datasets) == 0 ? 1 : 0

  project = local.bigquery_project
  role    = "roles/bigquery.dataViewer"
  member  = google_service_account.runtime.member
}

# IAP invokes Cloud Run as its service agent (Google's documented setup). The
# service also disables the invoker check because the deploy backend reaches
# it without IAP, but the grant keeps IAP traffic working if an organization
# policy forces the invoker check back on.
resource "google_project_service_identity" "iap" {
  count    = local.iap ? 1 : 0
  provider = google-beta

  project = var.project_id
  service = "iap.googleapis.com"

  depends_on = [google_project_service.apis]
}

resource "google_cloud_run_v2_service_iam_member" "iap_invoker" {
  count = local.iap ? 1 : 0

  project  = var.project_id
  location = var.region
  name     = google_cloud_run_v2_service.backyard.name
  role     = "roles/run.invoker"
  member   = "serviceAccount:${google_project_service_identity.iap[0].email}"
}
