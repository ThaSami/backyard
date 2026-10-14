# DNS records. With dns_zone_name the module manages them in Azure DNS;
# otherwise the dns_records output lists what to create at your DNS provider.
#
#  edge "frontdoor":     <domain>      CNAME (alias A at a zone apex) -> Front Door endpoint
#                        *.<domain>    CNAME -> Front Door endpoint (host mode)
#                        _dnsauth.<domain> TXT  managed-certificate validation tokens
#                                      (not needed with a Key Vault certificate:
#                                      Front Door approves domains covered by it)
#  edge "containerapp":  <domain>      CNAME (A at a zone apex) -> container app
#                        asuid.<domain> TXT  Container Apps domain verification id

locals {
  manage_dns  = local.custom_domain && var.dns_zone_name != null
  dns_zone_rg = var.dns_zone_resource_group_name != null ? var.dns_zone_resource_group_name : local.resource_group_name
  dns_zone_id = local.manage_dns ? data.azurerm_dns_zone.this[0].id : null

  # Name of the domain relative to the zone ("@" at the zone apex).
  dns_relative = !local.manage_dns ? "" : (
    var.domain == var.dns_zone_name ? "@" : trimsuffix(var.domain, ".${var.dns_zone_name}")
  )
  dns_at_apex = local.dns_relative == "@"
  # Prefix a relative label (e.g. "*", "asuid", "_dnsauth") to the domain's name.
  dns_wildcard_name = local.dns_at_apex ? "*" : "*.${local.dns_relative}"
  dns_dnsauth_name  = local.dns_at_apex ? "_dnsauth" : "_dnsauth.${local.dns_relative}"
  dns_asuid_name    = local.dns_at_apex ? "asuid" : "asuid.${local.dns_relative}"

  frontdoor_host     = local.use_frontdoor ? azurerm_cdn_frontdoor_endpoint.this[0].host_name : null
  managed_validation = local.use_frontdoor && !local.byo_certificate

  # Records to create (FQDNs), for operators without an Azure DNS zone.
  dns_records = concat(
    local.use_frontdoor ? [{
      name    = var.domain
      type    = "CNAME"
      value   = local.frontdoor_host
      purpose = "Hub -> Front Door. At a zone apex use an ALIAS/ANAME (or CNAME flattening); in Azure DNS an alias A record targeting the Front Door endpoint."
    }] : [],
    local.use_frontdoor && local.host_mode ? [{
      name    = "*.${var.domain}"
      type    = "CNAME"
      value   = local.frontdoor_host
      purpose = "Sites (<site>.${var.domain}) -> Front Door."
    }] : [],
    local.managed_validation ? [
      for k in sort(keys(local.frontdoor_domains)) : {
        name    = "_dnsauth.${var.domain}"
        type    = "TXT"
        value   = azurerm_cdn_frontdoor_custom_domain.this[k].validation_token
        purpose = "Front Door managed certificate validation for ${local.frontdoor_domains[k]} (both tokens live in one TXT record set; the Front Door portal shows the expected record)."
      }
    ] : [],
    local.use_app_domain ? [
      {
        name    = var.domain
        type    = "CNAME"
        value   = azurerm_container_app.this.ingress[0].fqdn
        purpose = "Hub -> container app. At a zone apex use an A record to ${azurerm_container_app_environment.this.static_ip_address} instead."
      },
      {
        name    = "asuid.${var.domain}"
        type    = "TXT"
        value   = nonsensitive(azurerm_container_app.this.custom_domain_verification_id)
        purpose = "Container Apps custom domain verification."
      },
    ] : [],
  )
}

data "azurerm_dns_zone" "this" {
  count = local.manage_dns ? 1 : 0

  name                = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
}

# --- edge "frontdoor" ---------------------------------------------------------

resource "azurerm_dns_txt_record" "frontdoor_validation" {
  count = local.manage_dns && local.managed_validation ? 1 : 0

  name                = local.dns_dnsauth_name
  zone_name           = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
  ttl                 = 3600

  dynamic "record" {
    for_each = sort(keys(local.frontdoor_domains))
    content {
      value = azurerm_cdn_frontdoor_custom_domain.this[record.value].validation_token
    }
  }

  tags = local.tags
}

# Zone apex: alias A record (a CNAME is not allowed at the apex).
resource "azurerm_dns_a_record" "frontdoor_hub" {
  count = local.manage_dns && local.use_frontdoor && local.dns_at_apex ? 1 : 0

  name                = "@"
  zone_name           = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
  ttl                 = var.dns_ttl
  target_resource_id  = azurerm_cdn_frontdoor_endpoint.this[0].id
  tags                = local.tags

  depends_on = [azurerm_cdn_frontdoor_route.app, azurerm_cdn_frontdoor_custom_domain_association.this]
}

resource "azurerm_dns_cname_record" "frontdoor_hub" {
  count = local.manage_dns && local.use_frontdoor && !local.dns_at_apex ? 1 : 0

  name                = local.dns_relative
  zone_name           = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
  ttl                 = var.dns_ttl
  record              = local.frontdoor_host
  tags                = local.tags

  depends_on = [azurerm_cdn_frontdoor_route.app, azurerm_cdn_frontdoor_custom_domain_association.this]
}

resource "azurerm_dns_cname_record" "frontdoor_sites" {
  count = local.manage_dns && local.use_frontdoor && local.host_mode ? 1 : 0

  name                = local.dns_wildcard_name
  zone_name           = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
  ttl                 = var.dns_ttl
  record              = local.frontdoor_host
  tags                = local.tags

  depends_on = [azurerm_cdn_frontdoor_route.app, azurerm_cdn_frontdoor_custom_domain_association.this]
}

# --- edge "containerapp" ------------------------------------------------------

resource "azurerm_dns_txt_record" "app_asuid" {
  count = local.manage_dns && local.use_app_domain ? 1 : 0

  name                = local.dns_asuid_name
  zone_name           = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
  ttl                 = var.dns_ttl

  record {
    value = azurerm_container_app.this.custom_domain_verification_id
  }

  tags = local.tags
}

resource "azurerm_dns_a_record" "app_hub" {
  count = local.manage_dns && local.use_app_domain && local.dns_at_apex ? 1 : 0

  name                = "@"
  zone_name           = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
  ttl                 = var.dns_ttl
  records             = [azurerm_container_app_environment.this.static_ip_address]
  tags                = local.tags
}

resource "azurerm_dns_cname_record" "app_hub" {
  count = local.manage_dns && local.use_app_domain && !local.dns_at_apex ? 1 : 0

  name                = local.dns_relative
  zone_name           = var.dns_zone_name
  resource_group_name = local.dns_zone_rg
  ttl                 = var.dns_ttl
  record              = azurerm_container_app.this.ingress[0].fqdn
  tags                = local.tags
}
