# Backyard — Cloudflare Workers edition: domain, routing and Cloudflare Access.
#
# Optional infrastructure as code. Without Terraform, `backyard cloudflare
# domain add <host> [--host-mode]` and `backyard cloudflare access enable` (or
# `npx backyard-cloudflare …`) set up the same; use one or the other for a
# Worker, not both.
#
# The Worker itself is deployed by `backyard cloudflare setup` (or `npx
# backyard-cloudflare setup`, or `npm run setup` in deploy/cloudflare; Node.js
# 22+): wrangler manages the code, Durable Objects, secrets and variables, and
# the setup_args output hands this module's variables to setup. This module puts
# it on your domain and, optionally, behind Cloudflare Access:
#
#   path mode  https://<base_domain>/s/<site>/   Workers custom domain (DNS + certificate included)
#   host mode  https://<site>.<base_domain>/     proxied wildcard DNS + Worker routes
#
#   Access     one self-hosted application over the hub (and every site host in
#              host mode) with an allow policy (emails, email domains, Access
#              groups, Okta groups), plus a bypass application for the paths the
#              Worker authenticates itself (deploy gateway, health checks).
#
# Backyard's deploys (setup, scripts/cloudflare-ci.mjs) never change routes or
# custom domains, so they leave what this module creates alone. Keep "routes"
# out of wrangler.jsonc anyway: a plain `wrangler deploy` of a wrangler.jsonc
# that lists routes would change them.

data "cloudflare_zone" "this" {
  filter = {
    name = var.zone_name
  }
}

locals {
  zone_id   = data.cloudflare_zone.this.id
  host_mode = var.url_mode == "host"
  at_apex   = var.base_domain == var.zone_name

  # Hostnames served by the Worker (and protected by Access).
  hosts = local.host_mode ? [var.base_domain, "*.${var.base_domain}"] : [var.base_domain]

  public_url = "https://${var.base_domain}"
}

# base_domain must live in the zone.
resource "terraform_data" "checks" {
  input = var.base_domain

  lifecycle {
    precondition {
      condition     = local.at_apex || endswith(var.base_domain, ".${var.zone_name}")
      error_message = "base_domain (${var.base_domain}) must be zone_name (${var.zone_name}) or one of its subdomains."
    }
  }
}

# --- path mode: a Workers custom domain ----------------------------------------

resource "cloudflare_workers_custom_domain" "hub" {
  count = local.host_mode ? 0 : 1

  account_id = var.account_id
  zone_id    = local.zone_id
  hostname   = var.base_domain
  service    = var.worker_name
}

# --- host mode: wildcard DNS + routes ------------------------------------------

resource "cloudflare_dns_record" "hub" {
  count = local.host_mode && var.manage_dns ? 1 : 0

  zone_id = local.zone_id
  name    = var.base_domain
  type    = "AAAA"
  content = var.dns_record_content
  proxied = true
  ttl     = 1
  comment = "Backyard hub (served by the ${var.worker_name} Worker)"
}

resource "cloudflare_dns_record" "sites" {
  count = local.host_mode && var.manage_dns ? 1 : 0

  zone_id = local.zone_id
  name    = "*.${var.base_domain}"
  type    = "AAAA"
  content = var.dns_record_content
  proxied = true
  ttl     = 1
  comment = "Backyard sites (served by the ${var.worker_name} Worker)"
}

resource "cloudflare_workers_route" "hosts" {
  for_each = local.host_mode ? toset(local.hosts) : toset([])

  zone_id = local.zone_id
  pattern = "${each.value}/*"
  script  = var.worker_name
}

# Universal SSL covers <zone> and *.<zone> only. Sites at <site>.<base_domain>
# below the apex need a certificate for *.<base_domain> (Advanced Certificate
# Manager, a paid add-on).
resource "cloudflare_certificate_pack" "sites" {
  count = local.host_mode && !local.at_apex && var.advanced_certificate ? 1 : 0

  zone_id               = local.zone_id
  type                  = "advanced"
  hosts                 = [var.zone_name, var.base_domain, "*.${var.base_domain}"]
  validation_method     = "txt"
  validity_days         = 90
  certificate_authority = var.certificate_authority
  cloudflare_branding   = false
}

check "host_mode_certificate" {
  assert {
    condition     = !local.host_mode || local.at_apex || var.advanced_certificate
    error_message = "Host mode below the zone apex: Universal SSL does not cover *.${var.base_domain}. Set advanced_certificate = true (Advanced Certificate Manager), use base_domain = \"${var.zone_name}\", or use url_mode = \"path\"."
  }
}
