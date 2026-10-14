# Minimal Backyard on Google Cloud: IAP in front of Cloud Run, Cloud SQL,
# a GCS bucket, Vertex AI models and Google-managed certificates for
# backyard.example.com and *.backyard.example.com.
#
#   cp terraform.tfvars.example terraform.tfvars   # edit
#   terraform init
#   terraform apply
#
# Then point the records from `terraform output dns_records` at your DNS (or
# set dns_zone_name to let the module do it) and run the command printed by
# `terraform output cli_login_command`.

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.0"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "~> 8.0"
    }
  }

  # Keep state remote and encrypted: it contains generated secrets.
  # backend "gcs" {
  #   bucket = "acme-terraform-state"
  #   prefix = "backyard"
  # }
}

variable "project_id" {
  description = "Google Cloud project to deploy into."
  type        = string
}

variable "region" {
  description = "Region for Cloud Run and Cloud SQL."
  type        = string
  default     = "us-central1"
}

variable "domain" {
  description = "Hub domain, e.g. backyard.example.com (sites at <site>.backyard.example.com)."
  type        = string
}

variable "iap_members" {
  description = "Who may use Backyard, e.g. [\"domain:example.com\"] or [\"group:everyone@example.com\"]."
  type        = list(string)
}

variable "admins" {
  description = "Backyard admins (emails)."
  type        = list(string)
  default     = []
}

variable "dns_zone_name" {
  description = "Cloud DNS managed zone serving the domain (empty: create the records yourself)."
  type        = string
  default     = ""
}

provider "google" {
  project = var.project_id
  region  = var.region
}

provider "google-beta" {
  project = var.project_id
  region  = var.region
}

module "backyard" {
  source = "../.."

  project_id    = var.project_id
  region        = var.region
  domain        = var.domain
  iap_members   = var.iap_members
  admins        = var.admins
  dns_zone_name = var.dns_zone_name

  # Defaults worth knowing about:
  # auth_mode           = "iap"          # or "oidc" (with var.oidc) / "none"
  # database            = "cloudsql"     # or "external-postgres"
  # database_tier       = "db-custom-1-3840"
  # min_instances       = 1
  # vertex_ai           = true           # Gemini + Claude on Vertex AI
  # bigquery_warehouse  = false
  # deletion_protection = true           # set false before `terraform destroy`
}

output "hub_url" {
  description = "Open this in a browser."
  value       = module.backyard.hub_url
}

output "cli_login_command" {
  description = "Sign the Backyard CLI in."
  value       = module.backyard.cli_login_command
}

output "dns_records" {
  description = "DNS records to create when dns_zone_name is empty."
  value       = module.backyard.dns_records
}

output "load_balancer_ip" {
  description = "Load balancer IPv4 address."
  value       = module.backyard.load_balancer_ip
}

output "bucket" {
  description = "Bucket with site files."
  value       = module.backyard.bucket
}
