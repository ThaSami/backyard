# Cloud SQL for PostgreSQL: the document store (database.type postgres) and
# the realtime bus (pubsub.type postgres, LISTEN/NOTIFY). Cloud Run reaches it
# through the Cloud SQL connector mounted at /cloudsql (Unix socket, mTLS to
# the instance, IAM-authorized); the instance has a public IP but no
# authorized networks and only accepts connector connections.

resource "random_id" "database_suffix" {
  count = var.database == "cloudsql" ? 1 : 0

  # Cloud SQL instance names cannot be reused for a week after deletion.
  byte_length = 3
}

resource "google_sql_database_instance" "backyard" {
  count = var.database == "cloudsql" ? 1 : 0

  project             = var.project_id
  name                = "${var.name}-pg-${random_id.database_suffix[0].hex}"
  region              = var.region
  database_version    = var.database_version
  deletion_protection = var.deletion_protection

  settings {
    tier                        = var.database_tier
    edition                     = var.database_edition
    availability_type           = var.database_high_availability ? "REGIONAL" : "ZONAL"
    disk_type                   = "PD_SSD"
    disk_size                   = var.database_disk_size_gb
    disk_autoresize             = true
    deletion_protection_enabled = var.deletion_protection
    connector_enforcement       = "REQUIRED"
    user_labels                 = local.labels

    ip_configuration {
      ipv4_enabled = true
      ssl_mode     = "ENCRYPTED_ONLY"
    }

    backup_configuration {
      enabled                        = true
      point_in_time_recovery_enabled = var.database_point_in_time_recovery
      start_time                     = "03:00"
      transaction_log_retention_days = var.database_point_in_time_recovery ? 7 : null

      backup_retention_settings {
        retained_backups = 14
        retention_unit   = "COUNT"
      }
    }

    maintenance_window {
      day          = 7 # Sunday
      hour         = 4 # UTC
      update_track = "stable"
    }

    insights_config {
      query_insights_enabled  = true
      record_application_tags = false
      record_client_address   = false
    }

    dynamic "database_flags" {
      for_each = var.database_max_connections != null ? [var.database_max_connections] : []
      content {
        name  = "max_connections"
        value = tostring(database_flags.value)
      }
    }
  }

  depends_on = [google_project_service.apis]

  lifecycle {
    # disk_autoresize grows the disk; never try to shrink it back.
    ignore_changes = [settings[0].disk_size]
  }
}

resource "google_sql_database" "backyard" {
  count = var.database == "cloudsql" ? 1 : 0

  project  = var.project_id
  instance = google_sql_database_instance.backyard[0].name
  name     = "backyard"

  # Dropping the database fails while Backyard holds connections; deleting the
  # instance removes it anyway.
  deletion_policy = "ABANDON"
}

resource "random_password" "database" {
  count = var.database == "cloudsql" ? 1 : 0

  length  = 32
  special = false # embedded in a keyword/value DSN
}

resource "google_sql_user" "backyard" {
  count = var.database == "cloudsql" ? 1 : 0

  project  = var.project_id
  instance = google_sql_database_instance.backyard[0].name
  name     = "backyard"
  password = random_password.database[0].result

  # PostgreSQL users that own objects cannot be dropped; deleting the instance
  # removes them anyway.
  deletion_policy = "ABANDON"
}

locals {
  database_connection_name = var.database == "cloudsql" ? google_sql_database_instance.backyard[0].connection_name : ""

  # pgx keyword/value DSN over the Cloud SQL Unix socket. TLS is terminated by
  # the connector, hence sslmode=disable on the socket.
  cloudsql_dsn = var.database == "cloudsql" ? join(" ", [
    "host=/cloudsql/${local.database_connection_name}",
    "dbname=${google_sql_database.backyard[0].name}",
    "user=${google_sql_user.backyard[0].name}",
    "password=${random_password.database[0].result}",
    "sslmode=disable",
    "application_name=backyard",
  ]) : ""
}
