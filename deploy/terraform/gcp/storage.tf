# GCS bucket holding site files (sites/<site>/…), metadata (_meta/…) and
# uploads (uploads/<site>/…). Private, uniform bucket-level access; browsers
# PUT deploy files to V4 signed URLs, hence the CORS rule for the hub origin.
#
# GCS uses responseHeader both for preflight (Access-Control-Allow-Headers)
# and for Access-Control-Expose-Headers, so it must list every header a
# browser sends with a signed PUT: Content-Type and the signed
# x-goog-content-length-range that pins the upload's size (internal/blob/gcs).
# A header missing here fails the preflight, and with it every hub upload.
locals {
  bucket_cors_response_headers = [
    "Content-Type",
    "Content-Length",
    "Content-MD5",
    "ETag",
    "x-goog-content-length-range",
    "x-goog-hash",
    "x-goog-generation",
  ]
}

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
    response_header = local.bucket_cors_response_headers
    max_age_seconds = 3600
  }

  depends_on = [google_project_service.apis]
}

resource "google_storage_bucket_iam_member" "runtime_object_admin" {
  bucket = google_storage_bucket.sites.name
  role   = "roles/storage.objectAdmin"
  member = google_service_account.runtime.member
}

# Direct-sync deployers (`backyard deploy --method gcs`: gcloud storage rsync
# into <prefix>sites/<site>/, then POST .../refresh, after which the server
# rebuilds _meta itself). Like the AWS deployer policy and the systemd rrsync
# command, they may only write below <prefix>sites/: _meta/ (site owners,
# manifests) and every site's uploads/ stay out of reach.
locals {
  # storage.gcs.prefix as Backyard normalises it (internal/blob: no leading
  # slash, one trailing slash).
  storage_key_prefix = trim(var.storage_prefix, "/") == "" ? "" : "${trim(var.storage_prefix, "/")}/"
  # IAM resource name prefix of the site files.
  deployer_object_prefix = "projects/_/buckets/${google_storage_bucket.sites.name}/objects/${local.storage_key_prefix}sites/"
}

resource "google_storage_bucket_iam_member" "writers" {
  for_each = toset(var.bucket_writer_members)

  bucket = google_storage_bucket.sites.name
  role   = "roles/storage.objectUser"
  member = each.value

  condition {
    title       = "Backyard site files only"
    description = "Create, read, overwrite and delete objects below ${local.storage_key_prefix}sites/ (direct-sync deploys)."
    expression  = "resource.name.startsWith(\"${local.deployer_object_prefix}\")"
  }
}

# rsync lists the destination, and storage.objects.list is checked against
# the bucket, never an object, so the prefix condition above cannot grant it.
# objectViewer limited to the bucket resource grants listing (names and
# metadata) without reading any object.
resource "google_storage_bucket_iam_member" "writers_list" {
  for_each = toset(var.bucket_writer_members)

  bucket = google_storage_bucket.sites.name
  role   = "roles/storage.objectViewer"
  member = each.value

  condition {
    title       = "Backyard bucket listing only"
    description = "storage.objects.list for direct-sync deploys; object reads stay limited to sites/."
    expression  = "resource.type == \"storage.googleapis.com/Bucket\""
  }
}
