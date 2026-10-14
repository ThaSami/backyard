output "public_url" {
  description = "Hub URL (backyard login --server <public_url>)."
  value       = local.public_url
}

output "gateway_url" {
  description = "Deploy gateway URL."
  value       = "${local.public_url}/__backyard/api/deploy"
}

output "site_url_pattern" {
  description = "Where sites live."
  value       = local.host_mode ? "https://<site>.${var.base_domain}/" : "${local.public_url}/s/<site>/"
}

output "access_aud" {
  description = "AUD tag of the Access application: the Worker's ACCESS_AUD."
  value       = var.enable_access ? cloudflare_zero_trust_access_application.backyard[0].aud : ""
}

output "access_team_domain" {
  description = "Zero Trust team domain (<team>.cloudflareaccess.com): the Worker's ACCESS_TEAM_DOMAIN."
  value       = var.enable_access ? data.cloudflare_zero_trust_organization.this[0].auth_domain : ""
}

output "okta_identity_provider_id" {
  description = "Id of the Okta identity provider (created here or passed in)."
  value       = local.okta_idp_id
}

locals {
  worker_vars = merge(
    {
      BASE_DOMAIN = var.base_domain
      URL_MODE    = var.url_mode
      AUTH_MODE   = var.enable_access ? "access" : "none"
    },
    var.enable_access ? {
      ACCESS_TEAM_DOMAIN = data.cloudflare_zero_trust_organization.this[0].auth_domain
      ACCESS_AUD         = cloudflare_zero_trust_access_application.backyard[0].aud
    } : {},
  )
}

output "worker_vars" {
  description = "Variables the Worker needs for this setup."
  value       = local.worker_vars
}

output "setup_command" {
  description = "Redeploys the Worker with these variables (run in deploy/cloudflare; they are remembered for later runs)."
  value = join(" ", concat(
    ["npm run setup --", "--worker-name", var.worker_name],
    [for k, v in local.worker_vars : "--var ${k}:${v}"],
  ))
}
