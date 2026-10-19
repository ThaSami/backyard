# Basic Backyard deployment on Azure using the module in ../.. (copy this
# directory and change `source` to a pinned Git reference, e.g.
# "git::https://github.com/thasami/backyard.git//deploy/terraform/azure?ref=v0.4.0").
#
#   az login                              # an account that can create resources,
#                                         # role assignments and an Entra ID app
#   cat > terraform.tfvars <<'EOF'
#   subscription_id = "00000000-0000-0000-0000-000000000000"
#   domain          = "backyard.acme.com"
#   dns_zone_name   = "acme.com"          # Azure DNS zone; omit to create records yourself
#   admins          = ["alice@acme.com"]
#   EOF
#   terraform init && terraform apply
#   terraform output dns_records          # when not using an Azure DNS zone
#   terraform output -raw cli_login_command

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = ">= 5.0, < 6.0"
    }
    azuread = {
      source  = "hashicorp/azuread"
      version = ">= 3.0, < 4.0"
    }
    azapi = {
      source  = "Azure/azapi"
      version = ">= 2.0, < 3.0"
    }
  }

  # Keep state remote and private: it contains generated secrets (database
  # password, Backyard secret, Entra client secret).
  # backend "azurerm" {
  #   resource_group_name  = "tfstate"
  #   storage_account_name = "acmetfstate"
  #   container_name       = "tfstate"
  #   key                  = "backyard.tfstate"
  #   use_azuread_auth     = true
  # }
}

provider "azurerm" {
  subscription_id = var.subscription_id

  # azurerm 5.x registers no resource providers by default; these are the ones
  # the module uses (registration needs subscription-level rights once).
  resource_providers_to_register = [
    "Microsoft.App",
    "Microsoft.Cdn",
    "Microsoft.CognitiveServices",
    "Microsoft.DBforPostgreSQL",
    "Microsoft.KeyVault",
    "Microsoft.ManagedIdentity",
    "Microsoft.Network",
    "Microsoft.OperationalInsights",
    "Microsoft.Storage",
  ]

  # The storage account has shared keys disabled; use Entra ID for data-plane calls.
  storage_use_azuread = true

  features {}
}

provider "azuread" {}

# Only used for optional features the azurerm provider lacks (sticky sessions).
provider "azapi" {
  subscription_id = var.subscription_id
}

variable "subscription_id" {
  description = "Azure subscription to deploy into."
  type        = string
}

variable "location" {
  description = "Azure region."
  type        = string
  default     = "westeurope"
}

variable "domain" {
  description = "Base domain, e.g. backyard.acme.com (hub at https://<domain>, sites at https://<site>.<domain>)."
  type        = string
}

variable "dns_zone_name" {
  description = "Azure DNS zone containing domain (null: create the records from the dns_records output yourself)."
  type        = string
  default     = null
}

variable "dns_zone_resource_group_name" {
  description = "Resource group of dns_zone_name (defaults to the deployment resource group)."
  type        = string
  default     = null
}

variable "tls_certificate" {
  description = "Optional Key Vault certificate covering domain and *.domain (recommended: Front Door managed wildcard certificates are not rotated automatically)."
  type = object({
    key_vault_id     = string
    certificate_name = string
  })
  default = null
}

variable "admins" {
  description = "Admin user emails."
  type        = list(string)
  default     = []
}

variable "image" {
  description = "Backyard container image (pin a version in production)."
  type        = string
  default     = "ghcr.io/thasami/backyard:latest"
}

module "backyard" {
  source = "../.."

  name     = "backyard"
  location = var.location
  image    = var.image

  domain                       = var.domain
  url_mode                     = "host"
  dns_zone_name                = var.dns_zone_name
  dns_zone_resource_group_name = var.dns_zone_resource_group_name
  tls_certificate              = var.tls_certificate

  # Built-in OIDC login with a generated Entra ID app registration.
  auth_mode = "oidc"
  admins    = var.admins

  # Keyless Azure OpenAI for backyard.ai (aliases smart, fast, embed).
  azure_openai_enabled = true

  tags = {
    environment = "production"
  }
}

output "hub_url" {
  description = "Backyard hub URL."
  value       = module.backyard.hub_url
}

output "cli_login_command" {
  description = "Run this to point the Backyard CLI at the deployment."
  value       = module.backyard.cli_login_command
}

output "dns_records" {
  description = "DNS records to create when dns_zone_name is not set."
  value       = module.backyard.dns_records
}

output "container_app_fqdn" {
  description = "Default hostname of the container app (Front Door origin)."
  value       = module.backyard.container_app_fqdn
}

output "storage_account_name" {
  description = "Storage account holding site files."
  value       = module.backyard.storage_account_name
}
