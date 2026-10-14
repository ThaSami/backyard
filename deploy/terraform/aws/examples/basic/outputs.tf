output "hub_url" {
  description = "Open this in a browser."
  value       = module.backyard.hub_url
}

output "cli_login_command" {
  description = "Run this to connect the backyard CLI."
  value       = module.backyard.cli_login_command
}

output "gateway_url" {
  description = "Deploy gateway URL."
  value       = module.backyard.gateway_url
}

output "oidc_redirect_uris" {
  description = "Register these sign-in redirect URIs in the Okta application."
  value       = module.backyard.oidc_redirect_uris
}

output "certificate_validation_records" {
  description = "Create these CNAMEs at your DNS provider when route53_zone_id is not set."
  value       = module.backyard.certificate_validation_records
}

output "dns_records" {
  description = "Point these names at the load balancer when route53_zone_id is not set."
  value       = module.backyard.dns_records
}
