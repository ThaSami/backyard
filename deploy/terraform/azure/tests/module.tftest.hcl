# Offline tests for the Azure module: `terraform init -backend=false &&
# terraform test` (Terraform >= 1.7). azurerm, azuread, azapi and time are
# mocked, so no Azure credentials are needed and nothing is created. The runs
# check resource selection (edge, DNS, auth, database, networking, AI) and the
# rendered backyard.yaml. configcheck/ loads every configuration these runs
# give the Container App with Backyard's own config loader, which decodes
# strictly (BACKYARD_TEST_TERRAFORM=1 go test
# ./deploy/terraform/azure/configcheck/).

mock_provider "azurerm" {
  mock_data "azurerm_network_service_tags" {
    defaults = {
      address_prefixes = ["13.107.208.0/24", "20.21.37.40/29", "2603:1000:4::50/124"]
      ipv4_cidrs       = ["13.107.208.0/24", "20.21.37.40/29"]
      ipv6_cidrs       = ["2603:1000:4::50/124"]
    }
  }

  mock_data "azurerm_key_vault" {
    defaults = {
      rbac_authorization_enabled = true
      vault_uri                  = "https://acme-tls.vault.azure.net/"
    }
  }

  mock_data "azurerm_key_vault_certificate" {
    defaults = {
      versionless_id        = "https://acme-tls.vault.azure.net/certificates/backyard-wildcard"
      versionless_secret_id = "https://acme-tls.vault.azure.net/secrets/backyard-wildcard"
    }
  }

  mock_data "azurerm_dns_zone" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/dns/providers/Microsoft.Network/dnsZones/acme.com"
    }
  }

  mock_resource "azurerm_container_app_environment" {
    defaults = {
      default_domain    = "happy-hill-1234abcd.westeurope.azurecontainerapps.io"
      id                = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.App/managedEnvironments/backyard-env"
      static_ip_address = "20.50.60.70"
    }
  }

  mock_resource "azurerm_user_assigned_identity" {
    defaults = {
      client_id    = "22222222-2222-2222-2222-222222222222"
      id           = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.ManagedIdentity/userAssignedIdentities/backyard-app"
      principal_id = "33333333-3333-3333-3333-333333333333"
      tenant_id    = "00000000-0000-0000-0000-0000000000aa"
    }
  }

  mock_resource "azurerm_cognitive_account" {
    defaults = {
      endpoint = "https://backyard-aoai-abc123.openai.azure.com/"
      id       = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.CognitiveServices/accounts/backyard-aoai-abc123"
    }
  }

  mock_resource "azurerm_postgresql_flexible_server" {
    defaults = {
      fqdn = "backyard-pg-abc123.postgres.database.azure.com"
      id   = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.DBforPostgreSQL/flexibleServers/backyard-pg-abc123"
    }
  }

  mock_resource "azurerm_resource_group" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg"
    }
  }

  mock_resource "azurerm_log_analytics_workspace" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.OperationalInsights/workspaces/backyard-logs"
    }
  }

  mock_resource "azurerm_storage_account" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Storage/storageAccounts/backyardabc123"
    }
  }

  mock_resource "azurerm_storage_container" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Storage/storageAccounts/backyardabc123/blobServices/default/containers/backyard"
    }
  }

  mock_resource "azurerm_virtual_network" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Network/virtualNetworks/backyard-vnet"
    }
  }

  mock_resource "azurerm_subnet" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Network/virtualNetworks/backyard-vnet/subnets/container-apps"
    }
  }

  mock_resource "azurerm_private_dns_zone" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Network/privateDnsZones/backyard-abc123.private.postgres.database.azure.com"
    }
  }

  mock_resource "azurerm_container_app" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.App/containerApps/backyard"
    }
  }

  mock_resource "azurerm_container_app_environment_certificate" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.App/managedEnvironments/backyard-env/certificates/backyard-tls"
    }
  }

  mock_resource "azurerm_cdn_frontdoor_profile" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Cdn/profiles/backyard-afd"
    }
  }

  mock_resource "azurerm_cdn_frontdoor_origin_group" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Cdn/profiles/backyard-afd/originGroups/backyard-app"
    }
  }

  mock_resource "azurerm_cdn_frontdoor_origin" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Cdn/profiles/backyard-afd/originGroups/backyard-app/origins/backyard-containerapp"
    }
  }

  mock_resource "azurerm_cdn_frontdoor_secret" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Cdn/profiles/backyard-afd/secrets/backyard-tls"
    }
  }

  mock_resource "azurerm_cdn_frontdoor_custom_domain" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Cdn/profiles/backyard-afd/customDomains/backyard-hub"
    }
  }

  mock_resource "azurerm_cdn_frontdoor_route" {
    defaults = {
      id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Cdn/profiles/backyard-afd/afdEndpoints/backyard-abc123/routes/backyard-all"
    }
  }

  mock_resource "azurerm_cdn_frontdoor_endpoint" {
    defaults = {
      host_name = "backyard-abc123-g0h9cqfxexa8bchk.z01.azurefd.net"
      id        = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/backyard-rg/providers/Microsoft.Cdn/profiles/backyard-afd/afdEndpoints/backyard-abc123"
    }
  }
}

mock_provider "azuread" {
  mock_data "azuread_client_config" {
    defaults = {
      tenant_id = "00000000-0000-0000-0000-0000000000aa"
      object_id = "00000000-0000-0000-0000-0000000000cc"
    }
  }

  mock_data "azuread_application_published_app_ids" {
    defaults = {
      result = {
        MicrosoftGraph = "00000003-0000-0000-c000-000000000000"
      }
    }
  }

  mock_data "azuread_service_principal" {
    defaults = {
      object_id = "44444444-4444-4444-4444-444444444444"
      oauth2_permission_scope_ids = {
        openid         = "37f7f235-527c-4136-accd-4a02d197296e"
        profile        = "14dad69e-099b-42c9-810b-d002981feec1"
        email          = "64a6cdd6-aab1-4aaf-94b8-3cc8405e90d0"
        offline_access = "7427e0e9-2fba-42fe-b0c0-848c9e6a8182"
        "User.Read"    = "e1fe6dd8-ba31-4d61-89e7-88639da4683d"
      }
    }
  }

  mock_resource "azuread_application" {
    defaults = {
      client_id = "55555555-5555-5555-5555-555555555555"
      object_id = "66666666-6666-6666-6666-666666666666"
      id        = "/applications/66666666-6666-6666-6666-666666666666"
    }
  }
}

mock_provider "azapi" {}

mock_provider "time" {}

variables {
  location = "westeurope"
  # The module's default (true) makes every plan with a module-created public
  # Postgres warn (check postgres_open_to_all_of_azure); frontdoor_host_mode_entra
  # covers that default, the other runs stay quiet.
  postgres_allow_azure_services = false
}

# -----------------------------------------------------------------------------
# Default architecture: host mode behind Front Door, Entra ID login, managed
# certificates, Azure DNS, Postgres in public mode.
# -----------------------------------------------------------------------------

run "frontdoor_host_mode_entra" {
  command = apply

  variables {
    domain         = "backyard.acme.com"
    dns_zone_name  = "acme.com"
    admins         = ["alice@acme.com"]
    admin_groups   = ["7c9e6679-7425-40de-944b-e07fc1f90ae7", "team-$${HOME}"]
    allowed_emails = ["pat@contractor.example"]
    static_tokens  = [{ name = "ci", sha256 = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", admin = true }]
    # The module's default (the file-level variables turn it off).
    postgres_allow_azure_services = true
  }

  # Managed wildcard certificate: the plan warns (check block). So does the
  # default public Postgres endpoint, open to every Azure IP.
  expect_failures = [check.frontdoor_wildcard_certificate, check.postgres_open_to_all_of_azure]

  # --- rendered values ------------------------------------------------------
  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).server.base_domain == "backyard.acme.com" &&
      yamldecode(output.backyard_config_yaml).server.public_url == "https://backyard.acme.com" &&
      yamldecode(output.backyard_config_yaml).server.url_mode == "host" &&
      yamldecode(output.backyard_config_yaml).server.listen == ":8080"
    )
    error_message = "server section is wrong."
  }

  assert {
    condition     = contains(yamldecode(output.backyard_config_yaml).server.trusted_proxies, "100.64.0.0/10") && contains(yamldecode(output.backyard_config_yaml).server.trusted_proxies, "13.107.208.0/24")
    error_message = "Container Apps ingress and Front Door backend ranges must be trusted proxies."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).auth.mode == "oidc" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.issuer == "https://login.microsoftonline.com/00000000-0000-0000-0000-0000000000aa/v2.0" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.client_id == "55555555-5555-5555-5555-555555555555" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.client_secret == "$${OIDC_CLIENT_SECRET}" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.scopes == ["openid", "profile", "email"] &&
      yamldecode(output.backyard_config_yaml).auth.admins == ["alice@acme.com"] &&
      yamldecode(output.backyard_config_yaml).auth.allowed_emails == ["pat@contractor.example"] &&
      yamldecode(output.backyard_config_yaml).auth.tokens[0].admin == true
    )
    error_message = "auth section is wrong."
  }

  assert {
    # "$${" in the YAML text is Backyard's escape for a literal "${".
    condition     = contains(yamldecode(output.backyard_config_yaml).auth.admin_groups, "team-$$${HOME}")
    error_message = "Free text must be escaped against Backyard's $${VAR} expansion."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).storage.type == "azure" &&
      yamldecode(output.backyard_config_yaml).storage.azure.account == azurerm_storage_account.this.name &&
      yamldecode(output.backyard_config_yaml).storage.azure.container == "backyard" &&
      yamldecode(output.backyard_config_yaml).database.type == "postgres" &&
      yamldecode(output.backyard_config_yaml).database.postgres.max_conns == 10 &&
      yamldecode(output.backyard_config_yaml).pubsub.type == "postgres"
    )
    error_message = "storage/database/pubsub sections are wrong."
  }

  # The DSN reaches Backyard through the DATABASE_URL override (never as YAML
  # text, where arbitrary DSN characters could break quoting).
  assert {
    condition     = !can(yamldecode(output.backyard_config_yaml).database.postgres.dsn) && !can(yamldecode(output.backyard_config_yaml).pubsub.postgres) && !strcontains(output.backyard_config_yaml, "DATABASE_URL}")
    error_message = "The database DSN must not be rendered into backyard.yaml."
  }

  assert {
    condition     = !strcontains(output.backyard_config_yaml, "postgres://")
    error_message = "backyard.yaml must not contain the database credentials."
  }

  assert {
    condition     = !can(yamldecode(output.backyard_config_yaml).ai) && !can(yamldecode(output.backyard_config_yaml).deploy)
    error_message = "No AI models or deploy overrides were configured."
  }

  # --- resources ------------------------------------------------------------
  assert {
    condition     = length(azurerm_cdn_frontdoor_custom_domain.this) == 2 && azurerm_cdn_frontdoor_custom_domain.this["sites"].host_name == "*.backyard.acme.com" && azurerm_cdn_frontdoor_custom_domain.this["hub"].host_name == "backyard.acme.com"
    error_message = "Front Door needs the hub and wildcard custom domains."
  }

  assert {
    condition     = azurerm_cdn_frontdoor_custom_domain.this["sites"].tls[0].certificate_type == "ManagedCertificate" && length(azurerm_cdn_frontdoor_secret.tls) == 0
    error_message = "Without tls_certificate Front Door managed certificates are used."
  }

  assert {
    condition     = azurerm_cdn_frontdoor_route.app[0].link_to_default_domain == false && length(azurerm_cdn_frontdoor_route.app[0].cache) == 0 && azurerm_cdn_frontdoor_route.app[0].forwarding_protocol == "HttpsOnly"
    error_message = "Route must not cache (websockets) and must not serve the azurefd.net host."
  }

  assert {
    condition     = azurerm_cdn_frontdoor_origin.app[0].origin_host_header == azurerm_container_app.this.ingress[0].fqdn
    error_message = "Front Door must send the container app FQDN as Host."
  }

  assert {
    condition = (
      azurerm_dns_cname_record.frontdoor_hub[0].name == "backyard" &&
      azurerm_dns_cname_record.frontdoor_sites[0].name == "*.backyard" &&
      azurerm_dns_txt_record.frontdoor_validation[0].name == "_dnsauth.backyard" &&
      length(azurerm_dns_txt_record.frontdoor_validation[0].record) == 2 &&
      length(azurerm_dns_a_record.frontdoor_hub) == 0
    )
    error_message = "DNS records for a subdomain of the zone are wrong."
  }

  assert {
    condition     = length(azuread_application.backyard) == 1 && contains(azuread_application.backyard[0].web[0].redirect_uris, "https://backyard.acme.com/__backyard/auth/callback")
    error_message = "Entra ID application must register the Backyard callback."
  }

  assert {
    condition     = toset(azuread_application.backyard[0].group_membership_claims) == toset(["SecurityGroup"])
    error_message = "Entra ID application must emit group claims."
  }

  assert {
    condition = (
      length(azurerm_postgresql_flexible_server.this) == 1 &&
      azurerm_postgresql_flexible_server.this[0].version == "16" &&
      azurerm_postgresql_flexible_server.this[0].public_network_access_enabled == true &&
      length(azurerm_postgresql_flexible_server_firewall_rule.azure_services) == 1
    )
    error_message = "Postgres 16 in public mode with the Azure services rule expected."
  }

  assert {
    condition     = azurerm_role_assignment.app_blob_data.role_definition_name == "Storage Blob Data Contributor" && azurerm_role_assignment.app_blob_delegator.role_definition_name == "Storage Blob Delegator"
    error_message = "The app identity needs blob data access and user-delegation SAS rights."
  }

  assert {
    condition     = azurerm_storage_account.this.shared_access_key_enabled == false && contains(azurerm_storage_account.this.blob_properties[0].cors_rule[0].allowed_origins, "https://backyard.acme.com") && contains(azurerm_storage_account.this.blob_properties[0].cors_rule[0].allowed_methods, "PUT")
    error_message = "Storage must be Entra-only and allow presigned PUTs from the hub."
  }

  assert {
    # The headers a SAS upload must carry (internal/blob/azure PresignPut).
    condition = alltrue([
      for h in ["x-ms-blob-type", "content-type"] :
      contains(azurerm_storage_account.this.blob_properties[0].cors_rule[0].allowed_headers, "*") ||
      contains([for a in azurerm_storage_account.this.blob_properties[0].cors_rule[0].allowed_headers : lower(a)], h)
    ])
    error_message = "The storage CORS rule must allow the headers of a presigned upload (x-ms-blob-type, Content-Type)."
  }

  assert {
    condition = alltrue([
      for n in ["BACKYARD_CONFIG_YAML", "AZURE_CLIENT_ID", "BACKYARD_SECRET", "DATABASE_URL", "OIDC_CLIENT_SECRET"] :
      contains([for e in azurerm_container_app.this.template[0].container[0].env : e.name], n)
    ])
    error_message = "Container env is missing module-managed variables."
  }

  assert {
    condition     = length([for e in azurerm_container_app.this.template[0].container[0].env : e if e.name == "DATABASE_URL" && e.secret_name == "database-url"]) == 1
    error_message = "DATABASE_URL must come from a Container Apps secret."
  }

  assert {
    condition = (
      azurerm_container_app.this.ingress[0].target_port == 8080 &&
      azurerm_container_app.this.ingress[0].transport == "auto" &&
      azurerm_container_app.this.ingress[0].external_enabled == true &&
      azurerm_container_app.this.template[0].min_replicas == 1 &&
      azurerm_container_app.this.template[0].container[0].liveness_probe[0].path == "/__backyard/healthz" &&
      azurerm_container_app.this.template[0].container[0].image == "ghcr.io/thasami/backyard:latest"
    )
    error_message = "Container app ingress/probes/scale are wrong."
  }

  assert {
    condition     = length(azurerm_container_app.this.ingress[0].ip_security_restriction) == 0 && length(azapi_resource_action.sticky_sessions_on) == 0
    error_message = "Origin restriction and sticky sessions are opt-in."
  }

  assert {
    condition     = output.cli_login_command == "backyard login --server https://backyard.acme.com" && output.hub_url == "https://backyard.acme.com"
    error_message = "Outputs are wrong."
  }

  assert {
    condition     = length(output.dns_records) == 4 && output.dns_records_managed
    error_message = "Expected hub, wildcard and two validation records."
  }
}

# -----------------------------------------------------------------------------
# Bring-your-own Key Vault certificate on Front Door, origin lock-down, sticky
# sessions, records exported instead of managed.
# -----------------------------------------------------------------------------

run "frontdoor_byo_certificate" {
  command = apply

  variables {
    domain = "backyard.acme.com"
    tls_certificate = {
      key_vault_id     = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/security/providers/Microsoft.KeyVault/vaults/acme-tls"
      certificate_name = "backyard-wildcard"
    }
    frontdoor_restrict_origin = true
    sticky_sessions           = true
  }

  assert {
    condition = (
      azurerm_cdn_frontdoor_secret.tls[0].secret[0].customer_certificate[0].key_vault_certificate_id == "https://acme-tls.vault.azure.net/certificates/backyard-wildcard" &&
      azurerm_cdn_frontdoor_custom_domain.this["sites"].tls[0].certificate_type == "CustomerCertificate" &&
      azurerm_cdn_frontdoor_custom_domain.this["hub"].tls[0].cdn_frontdoor_secret_id == azurerm_cdn_frontdoor_secret.tls[0].id
    )
    error_message = "Front Door must use the versionless Key Vault certificate for both domains."
  }

  assert {
    condition     = length(azurerm_role_assignment.edge_tls_secret) == 1 && azurerm_role_assignment.edge_tls_secret[0].role_definition_name == "Key Vault Secrets User" && length(azurerm_key_vault_access_policy.edge_tls) == 0
    error_message = "RBAC vaults get a Key Vault Secrets User assignment for the edge identity."
  }

  assert {
    condition     = azurerm_cdn_frontdoor_profile.this[0].identity[0].type == "UserAssigned"
    error_message = "Front Door needs the edge identity to read Key Vault."
  }

  assert {
    condition     = length(azurerm_container_app.this.ingress[0].ip_security_restriction) == 2 && alltrue([for r in azurerm_container_app.this.ingress[0].ip_security_restriction : r.action == "Allow"])
    error_message = "Origin restriction must allow only the Front Door IPv4 ranges."
  }

  assert {
    condition     = length(azapi_resource_action.sticky_sessions_on) == 1 && length(azapi_resource_action.sticky_sessions_off) == 1
    error_message = "Sticky sessions need the PATCH actions."
  }

  assert {
    condition     = !output.dns_records_managed && length(output.dns_records) == 2 && length(azurerm_dns_txt_record.frontdoor_validation) == 0
    error_message = "With a Key Vault certificate no validation TXT records are needed; records are exported."
  }
}

# -----------------------------------------------------------------------------
# Path mode with the domain bound directly on the container app (zone apex),
# Okta, existing database, VNet integration.
# -----------------------------------------------------------------------------

run "containerapp_edge_okta_vnet" {
  command = apply

  variables {
    domain        = "acme-tools.com"
    dns_zone_name = "acme-tools.com"
    url_mode      = "path"
    edge          = "containerapp"
    tls_certificate = {
      key_vault_id     = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/security/providers/Microsoft.KeyVault/vaults/acme-tls"
      certificate_name = "acme-tools"
    }
    auth_mode          = "oidc"
    oidc_issuer        = "https://acme.okta.com/oauth2/default"
    oidc_client_id     = "0oa1b2c3d4"
    oidc_client_secret = "okta-secret"
    oidc_use_userinfo  = true
    oidc_session_ttl   = "8h"
    token_ttl          = "168h"
    database_url       = "postgres://u:p@db.internal:5432/backyard?sslmode=require"
    vnet_integration   = true
    min_replicas       = 2
    max_replicas       = 10
    cpu                = 1
    memory             = "2Gi"
    # Uniformly indented on purpose: the module normalises it to top level.
    extra_config_yaml = <<EOT
      sites:
        ownership: owner
        listing: owners
      ratelimit:
        ai_per_minute: 10
      mcp:
        enabled: true
      warehouse:
        sources:
          default:
            type: postgres
            dsn: "$${WAREHOUSE_DSN}"
        queries:
          signups:
            sql: |
              SELECT day, count(*) AS n
              FROM signups
              WHERE team = @team
              GROUP BY day
            params: [team]
    EOT
    extra_secret_env = {
      WAREHOUSE_DSN = "postgres://reader:pw@dwh.internal:5432/dwh?sslmode=require"
    }
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).server.url_mode == "path" &&
      yamldecode(output.backyard_config_yaml).server.base_domain == "acme-tools.com" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.issuer == "https://acme.okta.com/oauth2/default" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.client_id == "0oa1b2c3d4" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.use_userinfo == true &&
      yamldecode(output.backyard_config_yaml).auth.oidc.session_ttl == "8h" &&
      yamldecode(output.backyard_config_yaml).auth.token_ttl == "168h" &&
      !can(yamldecode(output.backyard_config_yaml).auth.oidc.scopes) &&
      yamldecode(output.backyard_config_yaml).sites.ownership == "owner" &&
      yamldecode(output.backyard_config_yaml).sites.listing == "owners" &&
      yamldecode(output.backyard_config_yaml).ratelimit.ai_per_minute == 10 &&
      yamldecode(output.backyard_config_yaml).warehouse.sources.default.dsn == "$${WAREHOUSE_DSN}" &&
      startswith(yamldecode(output.backyard_config_yaml).warehouse.queries.signups.sql, "SELECT day")
    )
    error_message = "Okta / path mode / extra config rendering is wrong."
  }

  assert {
    # The Claude connector: extra_config_yaml accepts an mcp section (it needs
    # an https public_url without a path; Okta stays Backyard's sign-in).
    condition     = yamldecode(output.backyard_config_yaml).mcp.enabled == true && yamldecode(output.backyard_config_yaml).server.public_url == "https://acme-tools.com"
    error_message = "extra_config_yaml must accept the mcp section."
  }

  assert {
    condition     = length(azuread_application.backyard) == 0 && length(azurerm_cdn_frontdoor_profile.this) == 0
    error_message = "No Entra application or Front Door expected."
  }

  assert {
    condition     = length(azurerm_postgresql_flexible_server.this) == 0 && length(random_password.postgres) == 0 && length(azurerm_subnet.postgres) == 0 && length(azurerm_private_dns_zone.postgres) == 0
    error_message = "database_url must skip the flexible server and its network."
  }

  assert {
    condition     = length(azurerm_virtual_network.this) == 1 && azurerm_container_app_environment.this.infrastructure_subnet_id == azurerm_subnet.apps[0].id
    error_message = "VNet integration must place the environment in the apps subnet."
  }

  assert {
    condition = (
      azurerm_container_app_environment_certificate.tls[0].certificate_key_vault[0].key_vault_secret_id == data.azurerm_key_vault_certificate.tls[0].versionless_secret_id &&
      azurerm_container_app_environment_certificate.tls[0].certificate_key_vault[0].identity == azurerm_user_assigned_identity.edge[0].id &&
      azurerm_container_app_custom_domain.this[0].name == "acme-tools.com" &&
      azurerm_container_app_custom_domain.this[0].certificate_binding_type == "SniEnabled"
    )
    error_message = "The domain must be bound on the container app with the Key Vault certificate."
  }

  assert {
    condition = (
      azurerm_dns_a_record.app_hub[0].name == "@" &&
      length(azurerm_dns_a_record.app_hub[0].records) == 1 &&
      contains(azurerm_dns_a_record.app_hub[0].records, "20.50.60.70") &&
      azurerm_dns_txt_record.app_asuid[0].name == "asuid" &&
      length(azurerm_dns_cname_record.app_hub) == 0
    )
    error_message = "Apex domain on the container app needs an A record and asuid TXT."
  }

  assert {
    condition     = length([for e in azurerm_container_app.this.template[0].container[0].env : e if e.name == "WAREHOUSE_DSN" && e.secret_name == "env-warehouse-dsn"]) == 1
    error_message = "extra_secret_env must be injected from a Container Apps secret."
  }

  assert {
    condition     = azurerm_container_app.this.template[0].max_replicas == 10 && azurerm_container_app.this.template[0].container[0].cpu == 1
    error_message = "Scaling/size variables not applied."
  }
}

# -----------------------------------------------------------------------------
# edge "containerapp" without a managed DNS zone: the domain is not bound until
# the operator has created the exported records; access-policy Key Vault.
# -----------------------------------------------------------------------------

run "containerapp_edge_unmanaged_dns_access_policy_vault" {
  command = apply

  override_data {
    target = data.azurerm_key_vault.tls[0]
    values = {
      rbac_authorization_enabled = false
    }
  }

  variables {
    domain   = "tools.acme.com"
    url_mode = "path"
    edge     = "containerapp"
    tls_certificate = {
      key_vault_id     = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/security/providers/Microsoft.KeyVault/vaults/acme-tls"
      certificate_name = "tools"
    }
  }

  assert {
    condition     = length(azurerm_container_app_custom_domain.this) == 0 && length(azurerm_container_app_environment_certificate.tls) == 1
    error_message = "Without managed DNS the hostname must not be bound on the first apply."
  }

  assert {
    condition     = length(azurerm_key_vault_access_policy.edge_tls) == 1 && length(azurerm_role_assignment.edge_tls_secret) == 0 && contains(azurerm_key_vault_access_policy.edge_tls[0].secret_permissions, "Get")
    error_message = "Access-policy vaults need a Get policy for the edge identity."
  }

  assert {
    condition = (
      length(output.dns_records) == 2 &&
      output.dns_records[0].name == "tools.acme.com" && output.dns_records[0].type == "CNAME" &&
      output.dns_records[1].name == "asuid.tools.acme.com" && output.dns_records[1].type == "TXT"
    )
    error_message = "The CNAME and asuid TXT records must be exported."
  }

  assert {
    condition     = azurerm_container_app_environment.this.identity[0].type == "UserAssigned"
    error_message = "The environment needs the edge identity to read the certificate."
  }
}

run "containerapp_edge_bind_after_dns" {
  command = apply

  variables {
    domain             = "tools.acme.com"
    url_mode           = "path"
    edge               = "containerapp"
    bind_custom_domain = true
    tls_certificate = {
      key_vault_id     = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/security/providers/Microsoft.KeyVault/vaults/acme-tls"
      certificate_name = "tools"
    }
  }

  assert {
    condition     = length(azurerm_container_app_custom_domain.this) == 1 && azurerm_container_app_custom_domain.this[0].name == "tools.acme.com"
    error_message = "bind_custom_domain = true must bind the hostname."
  }
}

# -----------------------------------------------------------------------------
# Existing subnets in two peered virtual networks.
# -----------------------------------------------------------------------------

run "existing_subnets_in_peered_vnets" {
  command = plan

  variables {
    url_mode                          = "path"
    vnet_integration                  = true
    existing_container_apps_subnet_id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/spoke-apps/subnets/aca"
    existing_postgres_subnet_id       = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/spoke-data/subnets/pg"
  }

  assert {
    condition     = length(azurerm_virtual_network.this) == 0 && azurerm_container_app_environment.this.infrastructure_subnet_id == var.existing_container_apps_subnet_id
    error_message = "Existing subnets must be used as-is."
  }

  assert {
    condition = (
      azurerm_private_dns_zone_virtual_network_link.postgres[0].virtual_network_id == "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/spoke-data" &&
      azurerm_private_dns_zone_virtual_network_link.postgres_apps[0].virtual_network_id == "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/net/providers/Microsoft.Network/virtualNetworks/spoke-apps"
    )
    error_message = "The Postgres private DNS zone must be linked to both virtual networks."
  }

  assert {
    condition     = azurerm_postgresql_flexible_server.this[0].public_network_access_enabled == false && azurerm_postgresql_flexible_server.this[0].delegated_subnet_id == var.existing_postgres_subnet_id && length(azurerm_postgresql_flexible_server_firewall_rule.azure_services) == 0
    error_message = "VNet-integrated Postgres must be private."
  }
}

# -----------------------------------------------------------------------------
# Lab setup: no domain (default *.azurecontainerapps.io host, path mode), no
# login, Azure OpenAI plus an Anthropic key.
# -----------------------------------------------------------------------------

run "default_hostname_lab_with_ai" {
  command = apply

  variables {
    url_mode               = "path"
    auth_mode              = "none"
    allow_anonymous_deploy = true
    azure_openai_enabled   = true
    anthropic_api_key      = "sk-ant-test"
    ai_models = {
      claude = { provider = "anthropic", model = "claude-opus-5-5", description = "Claude $${not-env}", effort = "high", max_tokens = 32000 }
    }
    ai_default_model           = "claude"
    ai_default_embedding_model = "embed"
    display_name               = "Acme \"Backyard\" <labs>"
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).server.base_domain == "backyard.happy-hill-1234abcd.westeurope.azurecontainerapps.io" &&
      yamldecode(output.backyard_config_yaml).server.public_url == "https://backyard.happy-hill-1234abcd.westeurope.azurecontainerapps.io" &&
      yamldecode(output.backyard_config_yaml).server.name == "Acme \"Backyard\" <labs>" &&
      yamldecode(output.backyard_config_yaml).auth.mode == "none" &&
      !can(yamldecode(output.backyard_config_yaml).auth.oidc) &&
      yamldecode(output.backyard_config_yaml).deploy.allow_anonymous == true
    )
    error_message = "Lab deployment rendering is wrong."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).ai.providers.azure.type == "azure-openai" &&
      yamldecode(output.backyard_config_yaml).ai.providers.azure.endpoint == "https://backyard-aoai-abc123.openai.azure.com" &&
      yamldecode(output.backyard_config_yaml).ai.providers.azure.use_azure_ad == true &&
      !can(yamldecode(output.backyard_config_yaml).ai.providers.azure.api_key) &&
      yamldecode(output.backyard_config_yaml).ai.providers.anthropic.api_key == "$${ANTHROPIC_API_KEY}" &&
      !can(yamldecode(output.backyard_config_yaml).ai.providers.openai)
    )
    error_message = "AI providers are wrong."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).ai.models.smart.provider == "azure" &&
      yamldecode(output.backyard_config_yaml).ai.models.smart.model == "smart" &&
      yamldecode(output.backyard_config_yaml).ai.models.embed.kind == "embedding" &&
      yamldecode(output.backyard_config_yaml).ai.models.claude.model == "claude-opus-5-5" &&
      yamldecode(output.backyard_config_yaml).ai.models.claude.effort == "high" &&
      yamldecode(output.backyard_config_yaml).ai.models.claude.max_tokens == 32000 &&
      !can(yamldecode(output.backyard_config_yaml).ai.models.smart.effort) &&
      yamldecode(output.backyard_config_yaml).ai.models.claude.description == "Claude $$${not-env}" &&
      yamldecode(output.backyard_config_yaml).ai.default_model == "claude" &&
      yamldecode(output.backyard_config_yaml).ai.default_embedding_model == "embed"
    )
    error_message = "AI models are wrong (descriptions must escape $${ for Backyard's env expansion)."
  }

  assert {
    condition     = length(azurerm_cognitive_deployment.openai) == 3 && azurerm_cognitive_account.openai[0].local_auth_enabled == false && azurerm_role_assignment.app_openai[0].role_definition_name == "Cognitive Services OpenAI User"
    error_message = "Azure OpenAI must be keyless with the app identity as OpenAI User."
  }

  assert {
    condition = (
      azurerm_cognitive_deployment.openai["smart"].model[0].name == "gpt-5.4" &&
      azurerm_cognitive_deployment.openai["fast"].model[0].name == "gpt-5.4-mini" &&
      azurerm_cognitive_deployment.openai["embed"].model[0].name == "text-embedding-3-small" &&
      azurerm_cognitive_deployment.openai["smart"].version_upgrade_option == "OnceCurrentVersionExpired"
    )
    error_message = "Default Azure OpenAI deployments are wrong."
  }

  assert {
    condition     = contains([for e in azurerm_container_app.this.template[0].container[0].env : e.name], "ANTHROPIC_API_KEY") && !contains([for e in azurerm_container_app.this.template[0].container[0].env : e.name], "OIDC_CLIENT_SECRET")
    error_message = "Secret env vars must follow the configured providers."
  }

  assert {
    condition     = length(azurerm_cdn_frontdoor_profile.this) == 0 && length(output.dns_records) == 0 && output.frontdoor_endpoint_hostname == null
    error_message = "No edge resources without a domain."
  }
}

# -----------------------------------------------------------------------------
# Existing resource group, ACR pulled with the app identity, a created VNet in
# non-RFC 1918 address space (its ingress subnet must be a trusted proxy).
# -----------------------------------------------------------------------------

run "existing_rg_acr_public_address_space_vnet" {
  command = apply

  variables {
    url_mode                   = "path"
    auth_mode                  = "none"
    create_resource_group      = false
    resource_group_name        = "platform"
    vnet_integration           = true
    vnet_address_space         = ["20.42.0.0/16"]
    container_apps_subnet_cidr = "20.42.0.0/23"
    postgres_subnet_cidr       = "20.42.2.0/24"
    image                      = "acme.azurecr.io/backyard@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    image_registry = {
      server = "acme.azurecr.io"
      acr_id = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/shared/providers/Microsoft.ContainerRegistry/registries/acme"
    }
  }

  assert {
    condition     = length(azurerm_resource_group.this) == 0 && length(data.azurerm_resource_group.this) == 1 && azurerm_container_app.this.resource_group_name == "platform"
    error_message = "An existing resource group must be used as-is."
  }

  assert {
    condition = (
      azurerm_role_assignment.app_acr_pull[0].role_definition_name == "AcrPull" &&
      azurerm_container_app.this.registry[0].identity == azurerm_user_assigned_identity.app.id &&
      azurerm_container_app.this.registry[0].password_secret_name == null
    )
    error_message = "ACR images must be pulled with the app identity (AcrPull), without a password."
  }

  assert {
    condition     = contains(yamldecode(output.backyard_config_yaml).server.trusted_proxies, "20.42.0.0/23")
    error_message = "The created Container Apps subnet must be a trusted proxy."
  }

  assert {
    condition     = azurerm_postgresql_flexible_server.this[0].public_network_access_enabled == false && azurerm_postgresql_flexible_server.this[0].delegated_subnet_id == azurerm_subnet.postgres[0].id
    error_message = "VNet-integrated Postgres must use the created delegated subnet."
  }
}

# -----------------------------------------------------------------------------
# Invalid combinations fail at plan time.
# -----------------------------------------------------------------------------

# Direct-sync deployers (az storage blob sync) may touch <prefix>sites/ only:
# _meta/ (site owners, manifests) and uploads/ stay with the server.
run "direct_deployers_limited_to_sites" {
  command = plan

  variables {
    url_mode                       = "path"
    database_url                   = "postgres://u:p@db.internal:5432/backyard?sslmode=require"
    storage_deployer_principal_ids = ["00000000-0000-0000-0000-0000000000aa", "00000000-0000-0000-0000-0000000000bb"]
  }

  assert {
    condition = alltrue([
      for a in azurerm_role_assignment.deployers_blob_data :
      a.role_definition_name == "Storage Blob Data Contributor" &&
      a.condition_version == "2.0" &&
      strcontains(a.condition, "@Resource[Microsoft.Storage/storageAccounts/blobServices/containers/blobs:path] StringStartsWith 'sites/'") &&
      strcontains(a.condition, "@Request[Microsoft.Storage/storageAccounts/blobServices/containers/blobs:prefix] StringStartsWith 'sites/'") &&
      alltrue([for action in ["blobs/read'} AND NOT SubOperationMatches{'Blob.List'}", "blobs/write'}", "blobs/add/action'}", "blobs/delete'}", "blobs/read'} AND SubOperationMatches{'Blob.List'}"] :
      strcontains(a.condition, "ActionMatches{'Microsoft.Storage/storageAccounts/blobServices/containers/${action}")])
    ]) && length(azurerm_role_assignment.deployers_blob_data) == 2
    error_message = "Deployers' blob reads, writes, deletes and listings must be limited to sites/ by an ABAC condition."
  }

  assert {
    condition     = azurerm_role_assignment.app_blob_data.condition == null
    error_message = "The server's own identity keeps access to the whole container (_meta, uploads)."
  }
}

run "direct_deployers_with_storage_prefix" {
  command = plan

  variables {
    url_mode                       = "path"
    database_url                   = "postgres://u:p@db.internal:5432/backyard?sslmode=require"
    storage_prefix                 = " /team-a/ "
    storage_deployer_principal_ids = ["00000000-0000-0000-0000-0000000000aa"]
  }

  assert {
    condition = (
      strcontains(azurerm_role_assignment.deployers_blob_data["00000000-0000-0000-0000-0000000000aa"].condition, "blobs:path] StringStartsWith 'team-a/sites/'") &&
      strcontains(azurerm_role_assignment.deployers_blob_data["00000000-0000-0000-0000-0000000000aa"].condition, "blobs:prefix] StringStartsWith 'team-a/sites/'")
    )
    error_message = "The condition must use the storage prefix as Backyard normalises it (team-a/sites/)."
  }
}

run "direct_deployers_reject_quote_in_prefix" {
  command = plan

  variables {
    url_mode                       = "path"
    database_url                   = "postgres://u:p@db.internal:5432/backyard?sslmode=require"
    storage_prefix                 = "it's"
    storage_deployer_principal_ids = ["00000000-0000-0000-0000-0000000000aa"]
  }

  expect_failures = [azurerm_role_assignment.deployers_blob_data]
}

run "host_mode_requires_domain" {
  command = plan

  expect_failures = [terraform_data.preconditions]
}

run "containerapp_edge_rejects_host_mode" {
  command = plan

  variables {
    domain = "backyard.acme.com"
    edge   = "containerapp"
    tls_certificate = {
      key_vault_id     = "/subscriptions/00000000-0000-0000-0000-0000000000bb/resourceGroups/security/providers/Microsoft.KeyVault/vaults/acme-tls"
      certificate_name = "backyard"
    }
  }

  expect_failures = [terraform_data.preconditions]
}

run "cpu_memory_mismatch" {
  command = plan

  variables {
    url_mode = "path"
    cpu      = 1
    memory   = "1Gi"
  }

  expect_failures = [terraform_data.preconditions]
}

run "ai_model_without_key" {
  command = plan

  variables {
    url_mode  = "path"
    ai_models = { smart = { provider = "openai", model = "gpt-5" } }
  }

  expect_failures = [terraform_data.preconditions]
}

run "anonymous_deploy_requires_auth_none" {
  command = plan

  variables {
    url_mode               = "path"
    allow_anonymous_deploy = true
  }

  expect_failures = [terraform_data.preconditions]
}

run "extra_config_cannot_override_module_sections" {
  command = plan

  variables {
    url_mode          = "path"
    extra_config_yaml = "auth:\n  mode: none\n"
  }

  expect_failures = [var.extra_config_yaml]
}

run "reserved_env_names" {
  command = plan

  variables {
    url_mode  = "path"
    extra_env = { DATABASE_URL = "postgres://evil" }
  }

  expect_failures = [azurerm_container_app.this]
}

run "invalid_domain" {
  command = plan

  variables {
    domain = "https://backyard.acme.com"
  }

  expect_failures = [var.domain]
}

run "invalid_secret_env_name" {
  command = plan

  variables {
    url_mode         = "path"
    extra_secret_env = { "lower_case_" = "x" }
  }

  expect_failures = [var.extra_secret_env]
}

run "name_with_double_hyphen" {
  command = plan

  variables {
    name     = "my--backyard"
    url_mode = "path"
  }

  expect_failures = [var.name]
}

run "invalid_database_url" {
  command = plan

  variables {
    url_mode     = "path"
    database_url = "mysql://u:p@db:3306/backyard"
  }

  expect_failures = [var.database_url]
}

run "client_secret_with_quote" {
  command = plan

  variables {
    domain             = "backyard.acme.com"
    url_mode           = "path"
    oidc_issuer        = "https://acme.okta.com/oauth2/default"
    oidc_client_id     = "0oa1b2c3d4"
    oidc_client_secret = "abc'def"
  }

  expect_failures = [var.oidc_client_secret]
}

run "registry_without_password" {
  command = plan

  variables {
    url_mode       = "path"
    image          = "registry.acme.com/backyard:0.1.0"
    image_registry = { server = "registry.acme.com", username = "bot" }
  }

  expect_failures = [terraform_data.preconditions]
}

run "invalid_ai_effort" {
  command = plan

  variables {
    url_mode          = "path"
    anthropic_api_key = "sk-ant-test"
    ai_models         = { smart = { provider = "anthropic", model = "claude-opus-5-5", effort = "extreme" } }
  }

  expect_failures = [var.ai_models]
}
