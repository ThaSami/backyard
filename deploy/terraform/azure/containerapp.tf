# Container Apps environment (workload profiles, Consumption) and the Backyard
# container app: external ingress on 8080 (transport auto: HTTP/1.1, HTTP/2
# and websockets), probes on /__backyard/healthz, config rendered into
# BACKYARD_CONFIG_YAML and secrets injected as Container Apps secrets.

resource "random_password" "backyard_secret" {
  length  = 64
  special = false
}

locals {
  extra_secret_env_names = try(nonsensitive(toset(keys(var.extra_secret_env))), toset(keys(var.extra_secret_env)))
  registry_password      = var.image_registry != null && !local.registry_uses_identity

  # Environment variable name -> Container Apps secret name. Kept
  # non-sensitive so it can drive dynamic blocks; values are looked up in
  # secret_values (sensitive).
  secret_env = merge(
    {
      BACKYARD_SECRET = "backyard-secret"
      DATABASE_URL    = "database-url"
    },
    local.oidc_has_secret ? { OIDC_CLIENT_SECRET = "oidc-client-secret" } : {},
    local.anthropic_enabled ? { ANTHROPIC_API_KEY = "anthropic-api-key" } : {},
    local.openai_enabled ? { OPENAI_API_KEY = "openai-api-key" } : {},
    { for k in local.extra_secret_env_names : k => "env-${lower(replace(k, "_", "-"))}" },
  )
  secret_names = toset(concat(values(local.secret_env), local.registry_password ? ["registry-password"] : []))
  secret_values = merge(
    {
      "backyard-secret" = random_password.backyard_secret.result
      "database-url"    = local.database_url
    },
    local.oidc_has_secret ? { "oidc-client-secret" = local.oidc_client_secret } : {},
    local.anthropic_enabled ? { "anthropic-api-key" = var.anthropic_api_key } : {},
    local.openai_enabled ? { "openai-api-key" = var.openai_api_key } : {},
    { for k, v in var.extra_secret_env : "env-${lower(replace(k, "_", "-"))}" => v },
    local.registry_password ? { "registry-password" = var.image_registry_password } : {},
  )

  plain_env = merge(var.extra_env, {
    BACKYARD_CONFIG_YAML = local.backyard_config_yaml
    AZURE_CLIENT_ID      = azurerm_user_assigned_identity.app.client_id
  })

  bind_custom_domain = var.bind_custom_domain != null ? var.bind_custom_domain : local.manage_dns

  reserved_env_names = ["BACKYARD_CONFIG_YAML", "BACKYARD_CONFIG", "BACKYARD_SECRET", "BACKYARD_BASE_DOMAIN", "BACKYARD_PUBLIC_URL", "AZURE_CLIENT_ID", "DATABASE_URL", "OIDC_CLIENT_SECRET", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "PORT"]

  frontdoor_origin_ipv4 = (
    local.use_frontdoor && var.frontdoor_restrict_origin
    ? data.azurerm_network_service_tags.frontdoor[0].ipv4_cidrs
    : []
  )
}

resource "azurerm_container_app_environment" "this" {
  name                = local.environment_name
  location            = var.location
  resource_group_name = local.resource_group_name

  logs_destination           = "log-analytics"
  log_analytics_workspace_id = local.log_analytics_workspace_id

  infrastructure_subnet_id = local.container_apps_subnet_id
  zone_redundancy_enabled  = var.zone_redundant ? true : null

  workload_profile {
    name                  = "Consumption"
    workload_profile_type = "Consumption"
  }

  # The environment reads the TLS certificate from Key Vault (edge
  # "containerapp" only) with the edge identity.
  dynamic "identity" {
    for_each = local.use_app_domain && local.byo_certificate ? [1] : []
    content {
      type         = "UserAssigned"
      identity_ids = [azurerm_user_assigned_identity.edge[0].id]
    }
  }

  tags = local.tags

  depends_on = [azurerm_resource_group.this]
}

resource "azurerm_container_app" "this" {
  name                         = local.app_name
  container_app_environment_id = azurerm_container_app_environment.this.id
  resource_group_name          = local.resource_group_name
  revision_mode                = "Single"
  workload_profile_name        = "Consumption"
  max_inactive_revisions       = 10

  identity {
    type         = "UserAssigned"
    identity_ids = [azurerm_user_assigned_identity.app.id]
  }

  dynamic "registry" {
    for_each = var.image_registry != null ? [var.image_registry] : []
    content {
      server               = registry.value.server
      identity             = local.registry_uses_identity ? azurerm_user_assigned_identity.app.id : null
      username             = local.registry_uses_identity ? null : registry.value.username
      password_secret_name = local.registry_uses_identity ? null : "registry-password"
    }
  }

  dynamic "secret" {
    for_each = local.secret_names
    content {
      name  = secret.value
      value = local.secret_values[secret.value]
    }
  }

  ingress {
    external_enabled           = true
    target_port                = 8080
    transport                  = "auto"
    allow_insecure_connections = false

    traffic_weight {
      latest_revision = true
      percentage      = 100
    }

    dynamic "ip_security_restriction" {
      for_each = local.frontdoor_origin_ipv4
      content {
        name             = "frontdoor-${ip_security_restriction.key}"
        action           = "Allow"
        ip_address_range = ip_security_restriction.value
        description      = "AzureFrontDoor.Backend"
      }
    }
  }

  template {
    min_replicas                     = var.min_replicas
    max_replicas                     = var.max_replicas
    termination_grace_period_seconds = 30

    http_scale_rule {
      name                = "http-concurrency"
      concurrent_requests = tostring(var.concurrent_requests_per_replica)
    }

    container {
      name   = "backyard"
      image  = var.image
      cpu    = var.cpu
      memory = var.memory
      args   = var.container_args

      dynamic "env" {
        for_each = local.plain_env
        content {
          name  = env.key
          value = env.value
        }
      }

      dynamic "env" {
        for_each = local.secret_env
        content {
          name        = env.key
          secret_name = env.value
        }
      }

      startup_probe {
        transport               = "HTTP"
        port                    = 8080
        path                    = "/__backyard/healthz"
        interval_seconds        = 3
        timeout                 = 3
        failure_count_threshold = 30
      }

      liveness_probe {
        transport               = "HTTP"
        port                    = 8080
        path                    = "/__backyard/healthz"
        interval_seconds        = 10
        timeout                 = 5
        failure_count_threshold = 3
      }

      readiness_probe {
        transport               = "HTTP"
        port                    = 8080
        path                    = var.readiness_probe_path
        interval_seconds        = 5
        timeout                 = 5
        failure_count_threshold = 3
        success_count_threshold = 1
      }
    }
  }

  tags = local.tags

  lifecycle {
    precondition {
      condition     = length(setintersection(toset(keys(var.extra_env)), toset(local.reserved_env_names))) == 0 && length(setintersection(local.extra_secret_env_names, toset(local.reserved_env_names))) == 0
      error_message = "extra_env / extra_secret_env must not set variables managed by the module: ${join(", ", local.reserved_env_names)}."
    }
    precondition {
      condition     = length(setintersection(toset(keys(var.extra_env)), local.extra_secret_env_names)) == 0
      error_message = "A variable cannot be in both extra_env and extra_secret_env."
    }
  }

  # The app must not boot before its identity can reach storage / Azure
  # OpenAI and before the database accepts connections.
  depends_on = [
    time_sleep.rbac_propagation,
    azurerm_postgresql_flexible_server_database.backyard,
    azurerm_postgresql_flexible_server_firewall_rule.azure_services,
  ]
}

# -----------------------------------------------------------------------------
# Optional sticky sessions (not exposed by azurerm): JSON-merge PATCH of the
# container app's ingress. Re-applied whenever azurerm updates the app; a
# destroy-time PATCH restores affinity "none" when the option is turned off.
# -----------------------------------------------------------------------------

resource "azapi_resource_action" "sticky_sessions_on" {
  count = var.sticky_sessions ? 1 : 0

  type        = "Microsoft.App/containerApps@2024-03-01"
  resource_id = azurerm_container_app.this.id
  method      = "PATCH"
  when        = "apply"
  body = {
    properties = {
      configuration = {
        ingress = {
          stickySessions = { affinity = "sticky" }
        }
      }
    }
  }

  lifecycle {
    replace_triggered_by = [azurerm_container_app.this]
  }
}

resource "azapi_resource_action" "sticky_sessions_off" {
  count = var.sticky_sessions ? 1 : 0

  type        = "Microsoft.App/containerApps@2024-03-01"
  resource_id = azurerm_container_app.this.id
  method      = "PATCH"
  when        = "destroy"
  body = {
    properties = {
      configuration = {
        ingress = {
          stickySessions = { affinity = "none" }
        }
      }
    }
  }
}

# -----------------------------------------------------------------------------
# edge "containerapp": custom domain bound directly on the container app with
# the Key Vault certificate (path url_mode only).
# -----------------------------------------------------------------------------

resource "azurerm_container_app_environment_certificate" "tls" {
  count = local.use_app_domain && local.byo_certificate ? 1 : 0

  name                         = "${var.name}-tls"
  container_app_environment_id = azurerm_container_app_environment.this.id

  certificate_key_vault {
    identity            = azurerm_user_assigned_identity.edge[0].id
    key_vault_secret_id = data.azurerm_key_vault_certificate.tls[0].versionless_secret_id
  }

  tags = local.tags

  depends_on = [time_sleep.rbac_propagation]
}

resource "azurerm_container_app_custom_domain" "this" {
  count = local.use_app_domain && local.byo_certificate && local.bind_custom_domain ? 1 : 0

  name                                     = var.domain
  container_app_id                         = azurerm_container_app.this.id
  container_app_environment_certificate_id = azurerm_container_app_environment_certificate.tls[0].id
  certificate_binding_type                 = "SniEnabled"

  # Azure validates the hostname (CNAME/A + asuid TXT) before binding.
  depends_on = [
    azurerm_dns_a_record.app_hub,
    azurerm_dns_cname_record.app_hub,
    azurerm_dns_txt_record.app_asuid,
  ]
}
