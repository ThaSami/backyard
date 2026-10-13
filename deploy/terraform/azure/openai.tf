# Optional Azure OpenAI account. Keyless: local auth is disabled and the app's
# managed identity gets "Cognitive Services OpenAI User"; Backyard's
# azure-openai provider authenticates with DefaultAzureCredential
# (use_azure_ad). Each deployment name is the Backyard model alias.

resource "azurerm_cognitive_account" "openai" {
  count = var.azure_openai_enabled ? 1 : 0

  name                = local.openai_account_name
  location            = var.azure_openai_location != null ? var.azure_openai_location : var.location
  resource_group_name = local.resource_group_name
  kind                = "OpenAI"
  sku_name            = "S0"

  # Entra ID authentication requires a custom subdomain endpoint.
  custom_subdomain_name         = local.openai_account_name
  local_auth_enabled            = false
  public_network_access_enabled = var.azure_openai_public_network_access

  tags = local.tags

  depends_on = [azurerm_resource_group.this]
}

resource "azurerm_cognitive_deployment" "openai" {
  for_each = var.azure_openai_enabled ? var.azure_openai_deployments : {}

  name                 = each.key
  cognitive_account_id = azurerm_cognitive_account.openai[0].id

  model {
    format  = each.value.model_format
    name    = each.value.model_name
    version = each.value.model_version
  }

  sku {
    name     = each.value.sku_name
    capacity = each.value.capacity
  }

  # Keep the pinned version until Azure retires it (no silent model changes).
  version_upgrade_option = "OnceCurrentVersionExpired"
}
