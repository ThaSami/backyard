output "hub_url" {
  description = "Public URL of the Backyard hub."
  value       = local.public_url
}

output "base_domain" {
  description = "server.base_domain of the deployment."
  value       = local.base_domain
}

output "url_mode" {
  description = "host (sites at https://<site>.<base_domain>/) or path (sites at <hub_url>/s/<site>/)."
  value       = var.url_mode
}

output "site_url_example" {
  description = "Where a site named \"hello\" is served."
  value       = var.url_mode == "host" ? "https://hello.${local.base_domain}/" : "${local.public_url}/s/hello/"
}

output "cli_login_command" {
  description = "Command that points the Backyard CLI at this deployment."
  value       = "backyard login --server ${local.public_url}"
}

output "resource_group_name" {
  description = "Resource group holding the deployment."
  value       = local.resource_group_name
}

output "container_app_name" {
  description = "Name of the Backyard container app."
  value       = azurerm_container_app.this.name
}

output "container_app_fqdn" {
  description = "Default *.azurecontainerapps.io hostname of the container app (Front Door's origin)."
  value       = azurerm_container_app.this.ingress[0].fqdn
}

output "container_app_environment_id" {
  description = "ID of the Container Apps environment."
  value       = azurerm_container_app_environment.this.id
}

output "container_app_environment_static_ip" {
  description = "Static inbound IP of the Container Apps environment (A record target for an apex domain with edge \"containerapp\")."
  value       = azurerm_container_app_environment.this.static_ip_address
}

output "frontdoor_endpoint_hostname" {
  description = "Front Door endpoint hostname (*.azurefd.net) that the domain and wildcard CNAMEs point to. null without Front Door."
  value       = local.frontdoor_host
}

output "frontdoor_profile_id" {
  description = "Front Door profile ID (attach a WAF security policy here). null without Front Door."
  value       = local.use_frontdoor ? azurerm_cdn_frontdoor_profile.this[0].id : null
}

output "dns_records" {
  description = "DNS records the deployment needs (created automatically when dns_zone_name is set): list of {name, type, value, purpose}."
  value       = local.dns_records
}

output "dns_records_managed" {
  description = "Whether the module manages the dns_records in Azure DNS (dns_zone_name set)."
  value       = local.manage_dns
}

output "storage_account_name" {
  description = "Storage account holding site files and uploads (storage.azure.account)."
  value       = azurerm_storage_account.this.name
}

output "storage_container_name" {
  description = "Blob container holding site files and uploads (storage.azure.container)."
  value       = azurerm_storage_container.this.name
}

output "storage_account_id" {
  description = "Resource ID of the storage account."
  value       = azurerm_storage_account.this.id
}

output "direct_deploy_command" {
  description = "Direct-to-storage deploy (needs az login with Storage Blob Data Contributor on the container, see storage_deployer_principal_ids)."
  value       = "AZURE_STORAGE_AUTH_MODE=login backyard deploy --method azure"
}

output "managed_identity_client_id" {
  description = "Client ID of the app's user-assigned managed identity (AZURE_CLIENT_ID)."
  value       = azurerm_user_assigned_identity.app.client_id
}

output "managed_identity_principal_id" {
  description = "Principal (object) ID of the app's user-assigned managed identity, for extra role assignments (e.g. a warehouse)."
  value       = azurerm_user_assigned_identity.app.principal_id
}

output "edge_identity_principal_id" {
  description = "Principal ID of the identity Front Door / the Container Apps environment use to read the TLS certificate from Key Vault. null without tls_certificate."
  value       = local.byo_certificate ? azurerm_user_assigned_identity.edge[0].principal_id : null
}

output "postgres_fqdn" {
  description = "FQDN of the created PostgreSQL flexible server (null with database_url)."
  value       = local.create_postgres ? azurerm_postgresql_flexible_server.this[0].fqdn : null
}

output "postgres_server_id" {
  description = "Resource ID of the created PostgreSQL flexible server (null with database_url)."
  value       = local.create_postgres ? azurerm_postgresql_flexible_server.this[0].id : null
}

output "oidc_issuer" {
  description = "OIDC issuer Backyard trusts (null with auth_mode \"none\")."
  value       = local.oidc_enabled ? local.oidc.issuer : null
}

output "oidc_redirect_uri" {
  description = "Redirect URI to register at the IdP (registered automatically on the generated Entra ID app)."
  value       = local.callback_url
}

output "entra_application_client_id" {
  description = "Client ID of the generated Entra ID application (null when not created)."
  value       = local.create_entra ? azuread_application.backyard[0].client_id : null
}

output "entra_application_object_id" {
  description = "Object ID of the generated Entra ID application (null when not created)."
  value       = local.create_entra ? azuread_application.backyard[0].object_id : null
}

output "azure_openai_endpoint" {
  description = "Endpoint of the created Azure OpenAI account (null when disabled)."
  value       = var.azure_openai_enabled ? local.azure_openai_endpoint : null
}

output "ai_models" {
  description = "Backyard AI model aliases configured by the module (alias => provider/model)."
  value       = { for alias, m in local.ai_models : alias => "${m.provider}/${m.model}" }
}

output "log_analytics_workspace_id" {
  description = "Log Analytics workspace receiving container logs."
  value       = local.log_analytics_workspace_id
}

output "backyard_config_yaml" {
  description = "Rendered backyard.yaml (BACKYARD_CONFIG_YAML). Contains no secrets: they are referenced as $${VAR} and injected from Container Apps secrets."
  value       = local.backyard_config_yaml
}
