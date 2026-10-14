# DNS records for the hub, sites and certificate issuance. Created in Cloud
# DNS when dns_zone_name is set; always listed in the dns_records output so
# they can be created at any other DNS provider.

locals {
  manage_dns = var.dns_zone_name != ""

  dns_auth_record = local.create_certificate ? one(google_certificate_manager_dns_authorization.domain[0].dns_resource_record) : null

  address_records = concat(
    [for host in local.hostnames : {
      name = "${host}."
      type = "A"
      data = google_compute_global_address.ipv4.address
    }],
    var.enable_ipv6 ? [for host in local.hostnames : {
      name = "${host}."
      type = "AAAA"
      data = google_compute_global_address.ipv6[0].address
    }] : [],
  )

  dns_records = concat(
    local.address_records,
    local.dns_auth_record != null ? [{
      name = local.dns_auth_record.name
      type = local.dns_auth_record.type
      data = local.dns_auth_record.data
    }] : [],
  )

  # Keys must be known at plan time (record values are not).
  managed_address_records = local.manage_dns ? {
    for r in concat(
      [for host in local.hostnames : { key = "${host}/A", name = "${host}.", type = "A" }],
      var.enable_ipv6 ? [for host in local.hostnames : { key = "${host}/AAAA", name = "${host}.", type = "AAAA" }] : [],
    ) : r.key => r
  } : {}
}

resource "google_dns_record_set" "address" {
  for_each = local.managed_address_records

  project      = local.dns_project
  managed_zone = var.dns_zone_name
  name         = each.value.name
  type         = each.value.type
  ttl          = var.dns_ttl
  rrdatas      = [each.value.type == "A" ? google_compute_global_address.ipv4.address : google_compute_global_address.ipv6[0].address]

  depends_on = [google_project_service.apis]
}

resource "google_dns_record_set" "certificate_authorization" {
  count = local.manage_dns && local.create_certificate ? 1 : 0

  project      = local.dns_project
  managed_zone = var.dns_zone_name
  name         = local.dns_auth_record.name
  type         = local.dns_auth_record.type
  ttl          = var.dns_ttl
  rrdatas      = [local.dns_auth_record.data]

  depends_on = [google_project_service.apis]
}
