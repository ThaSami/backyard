output "public_url" {
  description = "Hub URL."
  value       = module.backyard.public_url
}

output "access_aud" {
  description = "ACCESS_AUD for the Worker."
  value       = module.backyard.access_aud
}

output "access_team_domain" {
  description = "ACCESS_TEAM_DOMAIN for the Worker."
  value       = module.backyard.access_team_domain
}

output "setup_command" {
  description = "Run in deploy/cloudflare to switch the Worker to this domain and Access."
  value       = module.backyard.setup_command
}
