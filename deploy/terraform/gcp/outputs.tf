output "hub_url" {
  description = "URL of the Backyard hub."
  value       = local.public_url
}

output "site_url_pattern" {
  description = "Where a site named <site> is served."
  value       = local.host_mode ? "https://<site>.${var.domain}/" : "${local.public_url}/s/<site>/"
}

output "deploy_gateway_url" {
  description = "Deploy gateway base URL (reachable with Backyard bearer tokens, bypassing IAP)."
  value       = "${local.public_url}/__backyard/api/deploy"
}

output "cli_login_command" {
  description = "Command that signs the Backyard CLI in: it opens the hub in a browser (through IAP / SSO) and stores a Backyard API token."
  value       = "backyard login --server ${local.public_url}"
}

output "load_balancer_ip" {
  description = "Global IPv4 address of the load balancer."
  value       = google_compute_global_address.ipv4.address
}

output "load_balancer_ipv6" {
  description = "Global IPv6 address of the load balancer (enable_ipv6)."
  value       = var.enable_ipv6 ? google_compute_global_address.ipv6[0].address : null
}

output "dns_records" {
  description = "DNS records the deployment needs (name, type, data). Created automatically when dns_zone_name is set; otherwise create them at your DNS provider. Certificates are issued once the CNAME resolves."
  value       = local.dns_records
}

output "dns_records_managed" {
  description = "Whether the dns_records were created in Cloud DNS by this module."
  value       = local.manage_dns
}

output "bucket" {
  description = "GCS bucket holding site files (sites/<site>/…) and uploads."
  value       = google_storage_bucket.sites.name
}

output "service_account_email" {
  description = "Runtime service account of the Backyard Cloud Run service."
  value       = google_service_account.runtime.email
}

output "cloud_run_service" {
  description = "Name of the Backyard Cloud Run service."
  value       = google_cloud_run_v2_service.backyard.name
}

output "image" {
  description = "Image reference Cloud Run pulls (the Artifact Registry mirror path when mirroring)."
  value       = local.effective_image
}

output "iap_audience" {
  description = "Expected audience of IAP assertions (auth.iap.audience); null unless auth_mode = \"iap\"."
  value       = local.iap ? local.iap_audience : null
}

output "iap_backend_service" {
  description = "IAP-protected backend service (grant roles/iap.httpsResourceAccessor on it to admit more users)."
  value       = local.iap ? google_compute_backend_service.app.name : null
}

output "database_connection_name" {
  description = "Cloud SQL connection name (project:region:instance); null with external-postgres."
  value       = var.database == "cloudsql" ? local.database_connection_name : null
}

output "certificate_map" {
  description = "Certificate Manager certificate map attached to the HTTPS proxy."
  value       = local.certificate_map_id
}

output "backyard_config_yaml" {
  description = "The rendered backyard.yaml (BACKYARD_CONFIG_YAML). Contains no secrets; check it with `backyard config validate`."
  value       = local.backyard_config_yaml
}
