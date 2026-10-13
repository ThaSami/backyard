# Managed identities and role assignments.
#
#  * app identity  - used by the Backyard container (DefaultAzureCredential via
#                    AZURE_CLIENT_ID): blob data access, user-delegation SAS,
#                    Azure OpenAI and (optionally) AcrPull.
#  * edge identity - used by Front Door / the Container Apps environment to
#                    read the TLS certificate from Key Vault. Kept separate so
#                    the application itself never gets access to the private key.

resource "azurerm_user_assigned_identity" "app" {
  name                = "${var.name}-app"
  location            = var.location
  resource_group_name = local.resource_group_name
  tags                = local.tags

  depends_on = [azurerm_resource_group.this]
}

resource "azurerm_user_assigned_identity" "edge" {
  count = local.byo_certificate ? 1 : 0

  name                = "${var.name}-edge"
  location            = var.location
  resource_group_name = local.resource_group_name
  tags                = local.tags

  depends_on = [azurerm_resource_group.this]
}

# Read/write/delete blobs in the Backyard container only.
resource "azurerm_role_assignment" "app_blob_data" {
  scope                = azurerm_storage_container.this.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = azurerm_user_assigned_identity.app.principal_id
  principal_type       = "ServicePrincipal"
  description          = "Backyard: site files and uploads"
}

# Request user delegation keys (account-level action) to sign presigned
# upload/download URLs without an account key.
resource "azurerm_role_assignment" "app_blob_delegator" {
  scope                = azurerm_storage_account.this.id
  role_definition_name = "Storage Blob Delegator"
  principal_id         = azurerm_user_assigned_identity.app.principal_id
  principal_type       = "ServicePrincipal"
  description          = "Backyard: user-delegation SAS for presigned URLs"
}

# Direct-sync deployers (`backyard deploy --method azure`: az storage blob sync
# into <prefix>sites/<site>, then POST .../refresh, after which the server
# rebuilds _meta itself). Like the AWS deployer policy and the systemd rrsync
# command, they may only touch blobs below <prefix>sites/: an ABAC condition
# limits reads, writes and deletes to that path and listing to that prefix, so
# _meta/ (site owners, manifests) and every site's uploads/ stay out of reach.
locals {
  # storage.azure.prefix as Backyard normalises it (internal/blob: no leading
  # slash, one trailing slash).
  storage_key_prefix    = trim(trimspace(var.storage_prefix), "/") == "" ? "" : "${trim(trimspace(var.storage_prefix), "/")}/"
  deployer_sites_prefix = "${local.storage_key_prefix}sites/"

  deployer_blob_condition = <<-EOT
    (
     (
      !(ActionMatches{'Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read'} AND NOT SubOperationMatches{'Blob.List'})
      AND
      !(ActionMatches{'Microsoft.Storage/storageAccounts/blobServices/containers/blobs/write'})
      AND
      !(ActionMatches{'Microsoft.Storage/storageAccounts/blobServices/containers/blobs/add/action'})
      AND
      !(ActionMatches{'Microsoft.Storage/storageAccounts/blobServices/containers/blobs/delete'})
     )
     OR
     (
      @Resource[Microsoft.Storage/storageAccounts/blobServices/containers/blobs:path] StringStartsWith '${local.deployer_sites_prefix}'
     )
    )
    AND
    (
     (
      !(ActionMatches{'Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read'} AND SubOperationMatches{'Blob.List'})
     )
     OR
     (
      @Request[Microsoft.Storage/storageAccounts/blobServices/containers/blobs:prefix] StringStartsWith '${local.deployer_sites_prefix}'
     )
    )
  EOT
}

resource "azurerm_role_assignment" "deployers_blob_data" {
  for_each = toset(var.storage_deployer_principal_ids)

  scope                = azurerm_storage_container.this.id
  role_definition_name = "Storage Blob Data Contributor"
  principal_id         = each.value
  description          = "Backyard: direct deploys (az storage blob sync) below ${local.deployer_sites_prefix}"
  condition            = local.deployer_blob_condition
  condition_version    = "2.0"

  lifecycle {
    precondition {
      # The prefix is a quoted string in the ABAC condition.
      condition     = !strcontains(local.deployer_sites_prefix, "'") && !strcontains(local.deployer_sites_prefix, "\\")
      error_message = "storage_prefix must not contain quotes or backslashes when storage_deployer_principal_ids is set."
    }
  }
}

resource "azurerm_role_assignment" "app_openai" {
  count = var.azure_openai_enabled ? 1 : 0

  scope                = azurerm_cognitive_account.openai[0].id
  role_definition_name = "Cognitive Services OpenAI User"
  principal_id         = azurerm_user_assigned_identity.app.principal_id
  principal_type       = "ServicePrincipal"
  description          = "Backyard: AI proxy"
}

resource "azurerm_role_assignment" "app_acr_pull" {
  count = local.registry_uses_identity ? 1 : 0

  scope                = var.image_registry.acr_id
  role_definition_name = "AcrPull"
  principal_id         = azurerm_user_assigned_identity.app.principal_id
  principal_type       = "ServicePrincipal"
  description          = "Backyard: pull the container image"
}

# -----------------------------------------------------------------------------
# TLS certificate access (bring-your-own certificate in Key Vault)
# -----------------------------------------------------------------------------

locals {
  byo_certificate        = local.custom_domain && var.tls_certificate != null
  registry_uses_identity = var.image_registry != null ? var.image_registry.acr_id != null : false

  tls_key_vault_parts = local.byo_certificate ? regex("(?i)^/subscriptions/[^/]+/resourceGroups/([^/]+)/providers/Microsoft\\.KeyVault/vaults/([^/]+)$", var.tls_certificate.key_vault_id) : []
}

data "azurerm_key_vault" "tls" {
  count = local.byo_certificate ? 1 : 0

  resource_group_name = local.tls_key_vault_parts[0]
  name                = local.tls_key_vault_parts[1]
}

data "azurerm_key_vault_certificate" "tls" {
  count = local.byo_certificate ? 1 : 0

  key_vault_id = var.tls_certificate.key_vault_id
  name         = var.tls_certificate.certificate_name
}

# RBAC vaults: Key Vault Secrets User on the certificate's secret only.
resource "azurerm_role_assignment" "edge_tls_secret" {
  count = local.byo_certificate && var.tls_key_vault_grant_access && try(data.azurerm_key_vault.tls[0].rbac_authorization_enabled, false) ? 1 : 0

  scope                = "${var.tls_certificate.key_vault_id}/secrets/${var.tls_certificate.certificate_name}"
  role_definition_name = "Key Vault Secrets User"
  principal_id         = azurerm_user_assigned_identity.edge[0].principal_id
  principal_type       = "ServicePrincipal"
  description          = "Backyard: TLS certificate for Front Door / Container Apps"
}

# Access-policy vaults: Get on secrets and certificates.
resource "azurerm_key_vault_access_policy" "edge_tls" {
  count = local.byo_certificate && var.tls_key_vault_grant_access && !try(data.azurerm_key_vault.tls[0].rbac_authorization_enabled, true) ? 1 : 0

  key_vault_id            = var.tls_certificate.key_vault_id
  tenant_id               = azurerm_user_assigned_identity.edge[0].tenant_id
  object_id               = azurerm_user_assigned_identity.edge[0].principal_id
  secret_permissions      = ["Get"]
  certificate_permissions = ["Get"]
}

# Azure RBAC is eventually consistent: give new role assignments time to
# propagate before the container app starts (or Front Door / Container Apps
# fetch the certificate), so first boots do not fail with 403s.
resource "time_sleep" "rbac_propagation" {
  create_duration = var.rbac_propagation_wait

  triggers = {
    app_blob_data      = azurerm_role_assignment.app_blob_data.id
    app_blob_delegator = azurerm_role_assignment.app_blob_delegator.id
    app_openai         = join(",", azurerm_role_assignment.app_openai[*].id)
    app_acr_pull       = join(",", azurerm_role_assignment.app_acr_pull[*].id)
    edge_tls_secret    = join(",", azurerm_role_assignment.edge_tls_secret[*].id)
    edge_tls_policy    = join(",", azurerm_key_vault_access_policy.edge_tls[*].id)
  }
}
