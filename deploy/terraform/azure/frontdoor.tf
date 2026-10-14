# Azure Front Door Standard in front of the container app (edge "frontdoor").
#
# Azure Container Apps binds custom domains per hostname (apex or subdomain,
# validated with asuid TXT records) and documents no wildcard hostname
# binding, so the wildcard *.<domain> that host-mode sites need terminates on
# Front Door instead, which supports wildcard custom domains with a
# bring-your-own Key Vault certificate (or a managed certificate). Front Door
# forwards everything to the app's default FQDN with Host rewritten to that
# FQDN (Container Apps routes on Host) and X-Forwarded-Host carrying the
# original host, which Backyard trusts from the private ingress (see
# server.trusted_proxies). Caching stays disabled: websockets need it off and
# sites are served with Cache-Control: no-cache anyway.

data "azurerm_network_service_tags" "frontdoor" {
  count = local.use_frontdoor && (var.frontdoor_trust_backend_ips || var.frontdoor_restrict_origin) ? 1 : 0

  location = var.location
  service  = "AzureFrontDoor.Backend"
}

locals {
  frontdoor_domains = local.use_frontdoor ? merge(
    { hub = var.domain },
    local.host_mode ? { sites = "*.${var.domain}" } : {},
  ) : {}
}

resource "azurerm_cdn_frontdoor_profile" "this" {
  count = local.use_frontdoor ? 1 : 0

  name                     = "${var.name}-afd"
  resource_group_name      = local.resource_group_name
  sku_name                 = "Standard_AzureFrontDoor"
  response_timeout_seconds = var.frontdoor_response_timeout_seconds

  # Front Door reads bring-your-own certificates from Key Vault with this
  # identity ("Key Vault Secrets User").
  dynamic "identity" {
    for_each = local.byo_certificate ? [1] : []
    content {
      type         = "UserAssigned"
      identity_ids = [azurerm_user_assigned_identity.edge[0].id]
    }
  }

  tags = local.tags

  depends_on = [azurerm_resource_group.this]
}

resource "azurerm_cdn_frontdoor_endpoint" "this" {
  count = local.use_frontdoor ? 1 : 0

  name                     = "${var.name}-${local.suffix}"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this[0].id
  enabled                  = true
  tags                     = local.tags
}

resource "azurerm_cdn_frontdoor_origin_group" "this" {
  count = local.use_frontdoor ? 1 : 0

  name                     = "${var.name}-app"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this[0].id
  session_affinity_enabled = false

  # A single origin: Front Door recommends disabling health probes (traffic
  # goes to the only origin either way); Container Apps probes the replicas.
  load_balancing {
    sample_size                        = 4
    successful_samples_required        = 3
    additional_latency_in_milliseconds = 50
  }
}

resource "azurerm_cdn_frontdoor_origin" "app" {
  count = local.use_frontdoor ? 1 : 0

  name                          = "${var.name}-containerapp"
  cdn_frontdoor_origin_group_id = azurerm_cdn_frontdoor_origin_group.this[0].id
  enabled                       = true

  host_name                      = azurerm_container_app.this.ingress[0].fqdn
  origin_host_header             = azurerm_container_app.this.ingress[0].fqdn
  http_port                      = 80
  https_port                     = 443
  certificate_name_check_enabled = true
  priority                       = 1
  weight                         = 1000
}

resource "azurerm_cdn_frontdoor_secret" "tls" {
  count = local.use_frontdoor && local.byo_certificate ? 1 : 0

  name                     = "${var.name}-tls"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this[0].id

  secret {
    customer_certificate {
      # Versionless: Front Door follows certificate renewals in Key Vault.
      key_vault_certificate_id = data.azurerm_key_vault_certificate.tls[0].versionless_id
    }
  }

  depends_on = [time_sleep.rbac_propagation]
}

resource "azurerm_cdn_frontdoor_custom_domain" "this" {
  for_each = local.frontdoor_domains

  name                     = "${var.name}-${each.key}"
  cdn_frontdoor_profile_id = azurerm_cdn_frontdoor_profile.this[0].id
  host_name                = each.value
  dns_zone_id              = local.dns_zone_id

  tls {
    certificate_type        = local.byo_certificate ? "CustomerCertificate" : "ManagedCertificate"
    cdn_frontdoor_secret_id = local.byo_certificate ? azurerm_cdn_frontdoor_secret.tls[0].id : null
    minimum_version         = "TLS12"
  }

  timeouts {
    create = "60m"
    update = "60m"
    delete = "60m"
  }
}

resource "azurerm_cdn_frontdoor_route" "app" {
  count = local.use_frontdoor ? 1 : 0

  name                          = "${var.name}-all"
  cdn_frontdoor_endpoint_id     = azurerm_cdn_frontdoor_endpoint.this[0].id
  cdn_frontdoor_origin_group_id = azurerm_cdn_frontdoor_origin_group.this[0].id
  cdn_frontdoor_origin_ids      = [azurerm_cdn_frontdoor_origin.app[0].id]
  cdn_frontdoor_custom_domain_ids = [
    for k in sort(keys(local.frontdoor_domains)) : azurerm_cdn_frontdoor_custom_domain.this[k].id
  ]

  enabled                = true
  patterns_to_match      = ["/*"]
  supported_protocols    = ["Http", "Https"]
  https_redirect_enabled = true
  forwarding_protocol    = "HttpsOnly"
  # Only the custom domains reach Backyard: the *.azurefd.net host would not
  # match server.base_domain.
  link_to_default_domain = false

  # No cache block: caching disabled (required for websockets).
}

resource "azurerm_cdn_frontdoor_custom_domain_association" "this" {
  for_each = local.frontdoor_domains

  cdn_frontdoor_custom_domain_id = azurerm_cdn_frontdoor_custom_domain.this[each.key].id
  cdn_frontdoor_route_ids        = [azurerm_cdn_frontdoor_route.app[0].id]
}
