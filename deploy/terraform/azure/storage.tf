# Blob storage for site files (sites/<site>/...), metadata (_meta/...) and
# uploads (uploads/<site>/...). Backyard authenticates with its managed
# identity (DefaultAzureCredential) and signs presigned URLs with user
# delegation keys, so shared keys can stay disabled.

resource "azurerm_storage_account" "this" {
  name                = local.storage_account_name
  resource_group_name = local.resource_group_name
  location            = var.location

  account_kind             = "StorageV2"
  account_tier             = "Standard"
  account_replication_type = var.storage_replication_type
  access_tier              = "Hot"

  min_tls_version                  = "TLS1_2"
  https_traffic_only_enabled       = true
  allow_nested_items_to_be_public  = false
  shared_access_key_enabled        = var.storage_shared_key_enabled
  default_to_oauth_authentication  = true
  cross_tenant_replication_enabled = false
  # Public network access stays at its default (enabled): browsers PUT
  # presigned uploads and `az storage blob sync` runs from developer machines.
  # Every request is still authorised (Entra ID or a short-lived
  # user-delegation SAS).

  blob_properties {
    versioning_enabled = var.storage_versioning_enabled

    dynamic "delete_retention_policy" {
      for_each = var.storage_soft_delete_days > 0 ? [1] : []
      content {
        days = var.storage_soft_delete_days
      }
    }

    dynamic "container_delete_retention_policy" {
      for_each = var.storage_soft_delete_days > 0 ? [1] : []
      content {
        days = var.storage_soft_delete_days
      }
    }

    # The hub's drag-and-drop deploy uploads files straight to presigned blob
    # URLs (deploy gateway, SPEC section 7.3), which needs CORS.
    cors_rule {
      allowed_origins    = distinct(concat([local.public_url], var.storage_cors_extra_origins))
      allowed_methods    = ["GET", "HEAD", "PUT", "OPTIONS"]
      allowed_headers    = ["*"]
      exposed_headers    = ["ETag", "Content-Length", "Content-Type", "x-ms-request-id", "x-ms-version"]
      max_age_in_seconds = 3600
    }
  }

  tags = local.tags

  depends_on = [azurerm_resource_group.this]
}

resource "azurerm_storage_container" "this" {
  name                  = var.storage_container_name
  storage_account_id    = azurerm_storage_account.this.id
  container_access_type = "private"
}
