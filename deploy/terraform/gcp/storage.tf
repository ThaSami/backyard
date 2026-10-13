# GCS bucket holding site files (sites/<site>/…), metadata (_meta/…) and
# uploads (uploads/<site>/…). Private, uniform bucket-level access; browsers
# PUT deploy files to V4 signed URLs, hence the CORS rule for the hub origin.

resource "google_storage_bucket" "sites" {
  project       = var.project_id
  name          = local.bucket_name
  location      = local.bucket_location
  storage_class = "STANDARD"
  labels        = local.labels
  force_destroy = var.bucket_force_destroy

  uniform_bucket_level_access = true
  public_access_prevention    = "enforced"

  versioning {
    enabled = var.bucket_versioning
  }

  soft_delete_policy {
    retention_duration_seconds = var.bucket_soft_delete_days * 86400
  }

  dynamic "lifecycle_rule" {
    for_each = var.bucket_versioning ? [1] : []
    content {
      action {
        type = "Delete"
      }
      condition {
        with_state                 = "ARCHIVED"
        days_since_noncurrent_time = var.bucket_noncurrent_version_days
      }
    }
  }

  # Abandoned resumable / multipart uploads.
  lifecycle_rule {
    action {
      type = "AbortIncompleteMultipartUpload"
    }
    condition {
      age = 1
    }
  }

  cors {
    origin          = [local.public_url]
    method          = ["GET", "HEAD", "PUT"]
    response_header = ["Content-Type", "Content-Length", "Content-MD5", "ETag", "x-goog-hash", "x-goog-generation"]
    max_age_seconds = 3600
  }

  depends_on = [google_project_service.apis]
}

resource "google_storage_bucket_iam_member" "runtime_object_admin" {
  bucket = google_storage_bucket.sites.name
  role   = "roles/storage.objectAdmin"
  member = google_service_account.runtime.member
}

# Direct-sync deployers (`backyard deploy --method gcs`).
resource "google_storage_bucket_iam_member" "writers" {
  for_each = toset(var.bucket_writer_members)

  bucket = google_storage_bucket.sites.name
  role   = "roles/storage.objectUser"
  member = each.value
}
