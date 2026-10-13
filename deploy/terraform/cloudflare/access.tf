# Cloudflare Access (Zero Trust) in front of Backyard.
#
# The Worker (AUTH_MODE=access) verifies the Cf-Access-Jwt-Assertion of every
# request against ACCESS_AUD (output access_aud) and the team's signing keys, so
# Access is enforced even if someone reaches the Worker another way (workers.dev).

locals {
  create_okta = var.enable_access && var.okta_account_url != ""
  okta_idp_id = local.create_okta ? cloudflare_zero_trust_access_identity_provider.okta[0].id : var.okta_identity_provider_id

  allowed_idps = distinct(compact(concat(var.identity_provider_ids, local.create_okta ? [local.okta_idp_id] : [])))
  auto_redirect = (
    var.auto_redirect_to_identity != null ? var.auto_redirect_to_identity : length(local.allowed_idps) == 1
  )

  # One rule per allowed principal; Access lets a user in when any rule matches.
  allow_rules = concat(
    [for e in var.allowed_emails : { email = { email = e } }],
    [for d in var.allowed_email_domains : { email_domain = { domain = d } }],
    [for g in var.allowed_access_group_ids : { group = { id = g } }],
    [for g in var.allowed_okta_groups : { okta = { identity_provider_id = local.okta_idp_id, name = g } }],
  )
}

data "cloudflare_zero_trust_organization" "this" {
  count      = var.enable_access ? 1 : 0
  account_id = var.account_id
}

resource "cloudflare_zero_trust_access_identity_provider" "okta" {
  count = local.create_okta ? 1 : 0

  account_id = var.account_id
  name       = var.okta_idp_name
  type       = "okta"
  config = {
    okta_account            = trimsuffix(var.okta_account_url, "/")
    client_id               = var.okta_client_id
    client_secret           = var.okta_client_secret
    authorization_server_id = var.okta_authorization_server_id != "" ? var.okta_authorization_server_id : null
    # Okta groups reach the Access identity through the app's "groups" claim
    # (Okta: Sign On → OpenID Connect ID Token → Groups claim filter "groups"
    # matches regex ".*"). Backyard reads them for ADMIN_GROUPS and user.groups.
  }

  lifecycle {
    precondition {
      condition     = var.okta_client_id != "" && var.okta_client_secret != ""
      error_message = "okta_account_url is set: okta_client_id and okta_client_secret are required too."
    }
  }
}

resource "cloudflare_zero_trust_access_policy" "allow" {
  count = var.enable_access ? 1 : 0

  account_id       = var.account_id
  name             = "${var.access_app_name} users"
  decision         = "allow"
  include          = local.allow_rules
  session_duration = var.access_session_duration

  lifecycle {
    precondition {
      condition     = length(local.allow_rules) > 0
      error_message = "enable_access is true: allow someone with allowed_emails, allowed_email_domains, allowed_access_group_ids or allowed_okta_groups."
    }
    precondition {
      condition     = length(var.allowed_okta_groups) == 0 || local.okta_idp_id != ""
      error_message = "allowed_okta_groups needs an Okta identity provider: set okta_account_url (to create one) or okta_identity_provider_id."
    }
  }
}

resource "cloudflare_zero_trust_access_policy" "bypass" {
  count = var.enable_access && length(var.bypass_paths) > 0 ? 1 : 0

  account_id = var.account_id
  name       = "${var.access_app_name} machine endpoints"
  decision   = "bypass"
  include    = [{ everyone = {} }]
}

# The hub and (in host mode) every site host.
resource "cloudflare_zero_trust_access_application" "backyard" {
  count = var.enable_access ? 1 : 0

  account_id                 = var.account_id
  name                       = var.access_app_name
  type                       = "self_hosted"
  domain                     = var.base_domain
  destinations               = [for h in local.hosts : { type = "public", uri = h }]
  session_duration           = var.access_session_duration
  allowed_idps               = length(local.allowed_idps) > 0 ? local.allowed_idps : null
  auto_redirect_to_identity  = local.auto_redirect
  app_launcher_visible       = true
  http_only_cookie_attribute = true
  same_site_cookie_attribute = "lax"
  policies = [{
    id         = cloudflare_zero_trust_access_policy.allow[0].id
    precedence = 1
  }]
}

# More specific paths win in Access: the deploy gateway, token minting and the
# health checks are authenticated by the Worker (bearer tokens, signed upload
# URLs, or the CF_Authorization cookie, which it verifies itself).
resource "cloudflare_zero_trust_access_application" "bypass" {
  count = var.enable_access && length(var.bypass_paths) > 0 ? 1 : 0

  account_id           = var.account_id
  name                 = "${var.access_app_name} (machine endpoints)"
  type                 = "self_hosted"
  domain               = "${var.base_domain}${var.bypass_paths[0]}"
  destinations         = [for p in var.bypass_paths : { type = "public", uri = "${var.base_domain}${p}" }]
  app_launcher_visible = false
  policies = [{
    id         = cloudflare_zero_trust_access_policy.bypass[0].id
    precedence = 1
  }]
}
