# Backyard on Cloudflare: custom domain + Cloudflare Access, using the module
# in ../.. (copy this directory and change `source` to a pinned Git reference,
# e.g. "git::https://github.com/thasami/backyard.git//deploy/terraform/cloudflare?ref=v0.4.0").
#
# Optional: `backyard cloudflare domain add` / `access enable` do the same
# without Terraform (use one or the other for a Worker). Node.js 22+.
#
#   backyard cloudflare setup        # deploy the Worker first (or: npx backyard-cloudflare@latest setup,
#                                    # or npm ci && npm run setup in deploy/cloudflare)
#   cp terraform.tfvars.example terraform.tfvars && $EDITOR terraform.tfvars
#   export CLOUDFLARE_API_TOKEN=...   # Zone: DNS + Workers Routes, Account: Workers Scripts + Access
#   terraform init && terraform apply
#   terraform output -raw setup_command   # run it in deploy/cloudflare (a checkout), or:
#   backyard cloudflare setup $(terraform output -raw setup_args)

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = ">= 5.10, < 6.0"
    }
  }
}

# Reads CLOUDFLARE_API_TOKEN from the environment.
provider "cloudflare" {}

module "backyard" {
  source = "../.."

  account_id  = var.account_id
  zone_name   = var.zone_name
  base_domain = var.base_domain
  url_mode    = var.url_mode
  worker_name = var.worker_name

  allowed_email_domains = var.allowed_email_domains
  allowed_emails        = var.allowed_emails

  okta_account_url    = var.okta_account_url
  okta_client_id      = var.okta_client_id
  okta_client_secret  = var.okta_client_secret
  allowed_okta_groups = var.allowed_okta_groups
}
