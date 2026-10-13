# Google Cloud APIs used by the deployment. Services are never disabled on
# destroy: other workloads in the project may depend on them.

locals {
  required_apis = toset(concat(
    [
      "artifactregistry.googleapis.com",
      "certificatemanager.googleapis.com",
      "cloudresourcemanager.googleapis.com",
      "compute.googleapis.com",
      "iam.googleapis.com",
      "iamcredentials.googleapis.com", # SignBlob for V4 signed upload URLs
      "run.googleapis.com",
      "secretmanager.googleapis.com",
      "storage.googleapis.com",
    ],
    local.iap ? ["iap.googleapis.com"] : [],
    var.database == "cloudsql" ? ["sqladmin.googleapis.com"] : [],
    var.vertex_ai ? ["aiplatform.googleapis.com"] : [],
    var.bigquery_warehouse ? ["bigquery.googleapis.com"] : [],
    var.dns_zone_name != "" && local.dns_project == var.project_id ? ["dns.googleapis.com"] : [],
  ))
}

resource "google_project_service" "apis" {
  for_each = var.enable_apis ? local.required_apis : toset([])

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}
