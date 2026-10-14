# Backyard on Azure: Container Apps + Blob Storage + PostgreSQL flexible server,
# fronted by Azure Front Door Standard (wildcard custom domain) or a custom
# domain bound directly on the container app. See README.md for the
# architecture and the reasoning behind each choice.

resource "random_string" "suffix" {
  length  = 6
  special = false
  upper   = false
  numeric = true
}

locals {
  suffix = random_string.suffix.result
  tags = merge({
    application = "backyard"
    managed-by  = "terraform"
  }, var.tags)

  resource_group_name = coalesce(var.resource_group_name, "${var.name}-rg")

  # Names. Globally unique names carry the random suffix.
  app_name             = var.name
  environment_name     = "${var.name}-env"
  storage_account_name = coalesce(var.storage_account_name, "${substr(replace(var.name, "-", ""), 0, 18)}${local.suffix}")
  postgres_server_name = "${var.name}-pg-${local.suffix}"
  openai_account_name  = "${var.name}-aoai-${local.suffix}"

  # URLs.
  custom_domain    = var.domain != null
  use_frontdoor    = local.custom_domain && var.edge == "frontdoor"
  use_app_domain   = local.custom_domain && var.edge == "containerapp"
  host_mode        = var.url_mode == "host"
  app_default_fqdn = "${local.app_name}.${azurerm_container_app_environment.this.default_domain}"
  base_domain      = local.custom_domain ? var.domain : local.app_default_fqdn
  public_url       = "https://${local.base_domain}"
  callback_url     = "${local.public_url}/__backyard/auth/callback"

  # Front Door replaces Host with the container app FQDN (Container Apps
  # routes on Host) and passes the original host in X-Forwarded-Host, which
  # Backyard honours only from trusted proxies. Only the Container Apps
  # ingress (Envoy, private addresses) can reach the container, so private
  # ranges are trusted, plus the infrastructure subnet the module creates
  # (its address space need not be RFC 1918); Front Door's backend ranges are
  # added so the real client IP is resolved from X-Forwarded-For.
  base_trusted_proxies = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "127.0.0.0/8", "::1/128", "fd00::/8"]
  apps_subnet_cidrs    = local.create_vnet ? [var.container_apps_subnet_cidr] : []
  frontdoor_backend_cidrs = (
    local.use_frontdoor && var.frontdoor_trust_backend_ips
    ? data.azurerm_network_service_tags.frontdoor[0].address_prefixes
    : []
  )
  trusted_proxies = distinct(concat(local.base_trusted_proxies, local.apps_subnet_cidrs, local.frontdoor_backend_cidrs, var.extra_trusted_proxies))

  # Authentication.
  oidc_enabled  = var.auth_mode == "oidc"
  create_entra  = local.oidc_enabled && var.oidc_issuer == null && var.create_entra_application
  external_oidc = local.oidc_enabled && var.oidc_issuer != null
  # try(nonsensitive(x), x): sensitive-derived flags drive count/for_each;
  # older Terraform versions reject nonsensitive() on unmarked values.
  external_has_secret = local.external_oidc && try(nonsensitive(var.oidc_client_secret != null && var.oidc_client_secret != ""), var.oidc_client_secret != null && var.oidc_client_secret != "")
  oidc_has_secret     = local.create_entra || local.external_has_secret
  oidc = {
    issuer           = local.create_entra ? "https://login.microsoftonline.com/${data.azuread_client_config.current[0].tenant_id}/v2.0" : replace(var.oidc_issuer != null ? var.oidc_issuer : "", "$${", "$$${")
    client_id        = local.create_entra ? azuread_application.backyard[0].client_id : replace(var.oidc_client_id != null ? var.oidc_client_id : "", "$${", "$$${")
    has_secret       = local.oidc_has_secret
    scopes           = [for s in(var.oidc_scopes != null ? var.oidc_scopes : (local.create_entra ? ["openid", "profile", "email"] : [])) : replace(s, "$${", "$$${")]
    use_userinfo     = var.oidc_use_userinfo
    device_client_id = local.create_entra ? (var.entra_enable_device_flow ? azuread_application.backyard[0].client_id : "") : replace(var.oidc_device_client_id != null ? var.oidc_device_client_id : "", "$${", "$$${")
    session_ttl      = var.oidc_session_ttl != null ? var.oidc_session_ttl : ""
  }
  oidc_client_secret = (
    local.create_entra ? azuread_application_password.backyard[0].value
    : local.external_has_secret ? var.oidc_client_secret
    : null
  )

  # AI providers and model aliases.
  anthropic_enabled = try(nonsensitive(var.anthropic_api_key != null && var.anthropic_api_key != ""), var.anthropic_api_key != null && var.anthropic_api_key != "")
  openai_enabled    = try(nonsensitive(var.openai_api_key != null && var.openai_api_key != ""), var.openai_api_key != null && var.openai_api_key != "")
  azure_openai_endpoint = (
    var.azure_openai_enabled ? trimsuffix(azurerm_cognitive_account.openai[0].endpoint, "/") : ""
  )
  ai_providers = merge(
    var.azure_openai_enabled ? {
      azure = { type = "azure-openai", endpoint = local.azure_openai_endpoint, use_azure_ad = true, api_key_env = "" }
    } : {},
    local.anthropic_enabled ? {
      anthropic = { type = "anthropic", endpoint = "", use_azure_ad = false, api_key_env = "ANTHROPIC_API_KEY" }
    } : {},
    local.openai_enabled ? {
      openai = { type = "openai", endpoint = "", use_azure_ad = false, api_key_env = "OPENAI_API_KEY" }
    } : {},
  )
  # Backyard expands ${VAR} in the raw YAML before parsing it, so free text
  # from variables is escaped ("$${" renders a literal "${").
  ai_models = merge(
    var.azure_openai_enabled ? {
      for alias, d in var.azure_openai_deployments : alias => {
        provider    = "azure"
        model       = alias
        kind        = d.kind
        description = replace(d.description, "$${", "$$${")
        max_tokens  = 0
        effort      = ""
      }
    } : {},
    {
      for alias, m in var.ai_models : alias => {
        provider    = m.provider
        model       = replace(m.model, "$${", "$$${")
        kind        = m.kind
        description = replace(m.description, "$${", "$$${")
        max_tokens  = m.max_tokens
        effort      = m.effort
      }
    },
  )

  # Secrets injected as Container Apps secrets (never rendered into the YAML).
  # DATABASE_URL is not referenced from the YAML at all: Backyard reads it
  # for database.postgres.dsn (pubsub.postgres.dsn defaults to the same
  # value), which keeps arbitrary DSN characters out of the YAML text.
  database_url = (
    local.create_postgres
    ? "postgres://${var.postgres_admin_login}:${random_password.postgres[0].result}@${azurerm_postgresql_flexible_server.this[0].fqdn}:5432/${var.postgres_database_name}?sslmode=require"
    : var.database_url
  )

  backyard_config_yaml = templatefile("${path.module}/templates/backyard.yaml.tftpl", {
    display_name       = replace(var.display_name, "$${", "$$${")
    base_domain        = local.base_domain
    public_url         = local.public_url
    url_mode           = var.url_mode
    trusted_proxies    = local.trusted_proxies
    auth_mode          = var.auth_mode
    admins             = [for s in var.admins : replace(s, "$${", "$$${")]
    admin_groups       = [for s in var.admin_groups : replace(s, "$${", "$$${")]
    allowed_domains    = [for s in var.allowed_domains : replace(s, "$${", "$$${")]
    allowed_emails     = [for s in var.allowed_emails : replace(s, "$${", "$$${")]
    allowed_groups     = [for s in var.allowed_groups : replace(s, "$${", "$$${")]
    token_ttl          = var.token_ttl != null ? var.token_ttl : ""
    static_tokens      = var.static_tokens
    oidc               = local.oidc
    storage_account    = azurerm_storage_account.this.name
    storage_container  = azurerm_storage_container.this.name
    storage_prefix     = replace(var.storage_prefix, "$${", "$$${")
    postgres_max_conns = var.postgres_max_conns_per_replica
    allow_anonymous    = var.allow_anonymous_deploy
    ai_providers       = local.ai_providers
    ai_models          = local.ai_models
    ai_default_model   = var.ai_default_model != null ? var.ai_default_model : ""
    ai_default_image   = var.ai_default_image_model != null ? var.ai_default_image_model : ""
    ai_default_embed   = var.ai_default_embedding_model != null ? var.ai_default_embedding_model : ""
    log_level          = var.log_level
    access_log         = var.access_log
    # Re-serialised so any indentation/quoting style lands as top-level keys.
    extra_config_yaml = trimspace(var.extra_config_yaml) == "" ? "" : chomp(yamlencode(yamldecode(var.extra_config_yaml)))
  })
}

# -----------------------------------------------------------------------------
# Cross-variable checks (Terraform >= 1.6 cannot reference other variables in
# variable validation blocks, so they live here and fail the plan early).
# -----------------------------------------------------------------------------

resource "terraform_data" "preconditions" {
  lifecycle {
    precondition {
      condition     = local.custom_domain || var.url_mode == "path"
      error_message = "url_mode \"host\" needs a domain (wildcard DNS and TLS). Set domain, or use url_mode = \"path\" to serve Backyard on the container app's default hostname."
    }
    precondition {
      condition     = !(local.use_app_domain && var.url_mode == "host")
      error_message = "edge \"containerapp\" only supports url_mode \"path\": Azure Container Apps cannot bind wildcard custom domains. Use edge = \"frontdoor\" for host mode."
    }
    precondition {
      condition     = !local.use_app_domain || var.tls_certificate != null
      error_message = "edge \"containerapp\" needs tls_certificate (a Key Vault certificate for the domain)."
    }
    precondition {
      condition     = !local.oidc_enabled || var.oidc_issuer != null || var.create_entra_application
      error_message = "auth_mode \"oidc\" needs oidc_issuer/oidc_client_id, or create_entra_application = true."
    }
    precondition {
      condition     = var.oidc_issuer == null || var.oidc_client_id != null
      error_message = "oidc_client_id is required with oidc_issuer."
    }
    precondition {
      # Otherwise the registry-password Container Apps secret has no value
      # and the apply fails late.
      condition     = var.image_registry == null || local.registry_uses_identity || (var.image_registry_password != null && var.image_registry_password != "")
      error_message = "image_registry without acr_id needs image_registry_password."
    }
    precondition {
      condition     = !var.allow_anonymous_deploy || var.auth_mode == "none"
      error_message = "allow_anonymous_deploy is only allowed with auth_mode \"none\"."
    }
    precondition {
      condition     = var.max_replicas >= var.min_replicas
      error_message = "max_replicas must be >= min_replicas."
    }
    precondition {
      condition     = tonumber(trimsuffix(var.memory, "Gi")) == var.cpu * 2
      error_message = "memory must be cpu x 2 GiB on the Consumption workload profile (e.g. cpu 0.5 / memory \"1Gi\", cpu 1 / memory \"2Gi\")."
    }
    precondition {
      condition     = !var.zone_redundant || var.vnet_integration
      error_message = "zone_redundant requires vnet_integration."
    }
    precondition {
      condition     = var.vnet_integration || (var.existing_container_apps_subnet_id == null && var.existing_postgres_subnet_id == null)
      error_message = "existing_container_apps_subnet_id / existing_postgres_subnet_id require vnet_integration = true."
    }
    precondition {
      condition     = var.existing_container_apps_subnet_id == null || var.existing_postgres_subnet_id != null || !local.create_postgres
      error_message = "With existing_container_apps_subnet_id, also set existing_postgres_subnet_id (or bring your own database_url)."
    }
    precondition {
      condition     = !var.postgres_high_availability || !startswith(var.postgres_sku_name, "B_")
      error_message = "postgres_high_availability needs a General Purpose or Memory Optimized postgres_sku_name (not burstable B_*)."
    }
    precondition {
      condition     = !var.frontdoor_restrict_origin || local.use_frontdoor
      error_message = "frontdoor_restrict_origin needs domain set and edge = \"frontdoor\"."
    }
    precondition {
      condition     = var.dns_zone_name == null || !local.custom_domain || var.domain == var.dns_zone_name || endswith(coalesce(var.domain, "-"), ".${coalesce(var.dns_zone_name, "-")}")
      error_message = "domain must be dns_zone_name itself or a name inside it."
    }
    precondition {
      condition     = alltrue([for m in values(var.ai_models) : m.provider != "anthropic" || local.anthropic_enabled])
      error_message = "ai_models with provider \"anthropic\" need anthropic_api_key."
    }
    precondition {
      condition     = alltrue([for m in values(var.ai_models) : m.provider != "openai" || local.openai_enabled])
      error_message = "ai_models with provider \"openai\" need openai_api_key."
    }
    precondition {
      condition     = alltrue([for m in values(var.ai_models) : m.provider != "azure" || var.azure_openai_enabled])
      error_message = "ai_models with provider \"azure\" need azure_openai_enabled = true."
    }
    precondition {
      condition = alltrue([
        for pair in [
          [var.ai_default_model, "chat"],
          [var.ai_default_image_model, "image"],
          [var.ai_default_embedding_model, "embedding"],
        ] : pair[0] == null || try(local.ai_models[pair[0]].kind == pair[1], false)
      ])
      error_message = "ai_default_model / ai_default_image_model / ai_default_embedding_model must name a configured alias (ai_models or azure_openai_deployments) of kind chat / image / embedding."
    }
  }
}

# Warning (not an error): Front Door issues managed certificates for wildcard
# domains but never renews them automatically.
check "frontdoor_wildcard_certificate" {
  assert {
    condition     = !(local.use_frontdoor && local.host_mode && !local.byo_certificate)
    error_message = "The wildcard domain *.${coalesce(var.domain, "-")} uses a Front Door managed certificate, which Azure does not renew automatically: 45 days before it expires the domain goes to Pending Revalidation and needs a fresh _dnsauth TXT token. Set tls_certificate to a Key Vault certificate covering ${coalesce(var.domain, "-")} and *.${coalesce(var.domain, "-")} for unattended renewals (see README, DNS and TLS)."
  }
}

# -----------------------------------------------------------------------------
# Resource group and logs
# -----------------------------------------------------------------------------

resource "azurerm_resource_group" "this" {
  count = var.create_resource_group ? 1 : 0

  name     = local.resource_group_name
  location = var.location
  tags     = local.tags
}

# Fails the plan early when an existing resource group is missing.
data "azurerm_resource_group" "this" {
  count = var.create_resource_group ? 0 : 1

  name = local.resource_group_name
}

resource "azurerm_log_analytics_workspace" "this" {
  count = var.log_analytics_workspace_id == null ? 1 : 0

  name                = "${var.name}-logs"
  location            = var.location
  resource_group_name = local.resource_group_name
  sku                 = "PerGB2018"
  retention_in_days   = var.log_retention_days
  tags                = local.tags

  depends_on = [azurerm_resource_group.this]
}

locals {
  log_analytics_workspace_id = coalesce(var.log_analytics_workspace_id, try(azurerm_log_analytics_workspace.this[0].id, null))
}
