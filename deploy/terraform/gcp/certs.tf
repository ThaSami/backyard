# Google-managed certificate for the hub (apex) and, in host mode, every site
# (*.domain), issued through Certificate Manager with DNS authorization (the
# only way to get a Google-managed wildcard certificate). Issuance completes
# once the DNS authorization CNAME resolves (created automatically when
# dns_zone_name is set; see the dns_records output otherwise).

locals {
  create_certificate  = var.certificate_map == ""
  certificate_map_id  = local.create_certificate ? google_certificate_manager_certificate_map.backyard[0].id : var.certificate_map
  certificate_map_ref = "//certificatemanager.googleapis.com/${local.certificate_map_id}"
}

resource "google_certificate_manager_dns_authorization" "domain" {
  count = local.create_certificate ? 1 : 0

  project     = var.project_id
  name        = "${var.name}-dns-auth"
  description = "DNS authorization for ${var.domain}${local.host_mode ? " and *.${var.domain}" : ""}."
  domain      = var.domain
  labels      = local.labels

  depends_on = [google_project_service.apis]
}

resource "google_certificate_manager_certificate" "backyard" {
  count = local.create_certificate ? 1 : 0

  project     = var.project_id
  name        = "${var.name}-cert"
  description = "Backyard hub${local.host_mode ? " and sites" : ""}."
  labels      = local.labels

  managed {
    domains            = local.hostnames
    dns_authorizations = [google_certificate_manager_dns_authorization.domain[0].id]
  }
}

resource "google_certificate_manager_certificate_map" "backyard" {
  count = local.create_certificate ? 1 : 0

  project     = var.project_id
  name        = "${var.name}-certs"
  description = "Backyard load balancer certificates."
  labels      = local.labels

  depends_on = [google_project_service.apis]
}

resource "google_certificate_manager_certificate_map_entry" "hosts" {
  for_each = local.create_certificate ? toset(local.hostnames) : toset([])

  project      = var.project_id
  name         = "${var.name}-${each.value == var.domain ? "apex" : "wildcard"}"
  description  = "Certificate for ${each.value}."
  map          = google_certificate_manager_certificate_map.backyard[0].name
  hostname     = each.value
  certificates = [google_certificate_manager_certificate.backyard[0].id]
  labels       = local.labels
}

# Clients without SNI (or connecting by IP) get the same certificate.
resource "google_certificate_manager_certificate_map_entry" "primary" {
  count = local.create_certificate ? 1 : 0

  project      = var.project_id
  name         = "${var.name}-primary"
  description  = "Default certificate for clients without a matching SNI."
  map          = google_certificate_manager_certificate_map.backyard[0].name
  matcher      = "PRIMARY"
  certificates = [google_certificate_manager_certificate.backyard[0].id]
  labels       = local.labels
}
