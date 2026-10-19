# Basic Backyard deployment on AWS using the module in ../.. (copy this
# directory and change `source` to a pinned Git reference, e.g.
# "git::https://github.com/thasami/backyard.git//deploy/terraform/aws?ref=v0.3.0").
#
#   make lambda                       # from the repository root: dist/backyard-lambda.zip
#   cp terraform.tfvars.example terraform.tfvars && $EDITOR terraform.tfvars
#   terraform init && terraform apply
#   terraform output cli_login_command

terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.28, < 7.0"
    }
  }

  # Keep state remote and encrypted: it contains generated secrets.
  # backend "s3" {
  #   bucket       = "acme-terraform-state"
  #   key          = "backyard/terraform.tfstate"
  #   region       = "us-east-1"
  #   encrypt      = true
  #   use_lockfile = true
  # }
}

provider "aws" {
  region = var.region

  default_tags {
    tags = {
      Project   = "backyard"
      ManagedBy = "terraform"
    }
  }
}

module "backyard" {
  source = "../.."

  name            = "backyard"
  domain          = var.domain
  route53_zone_id = var.route53_zone_id

  auth_mode             = var.auth_mode
  oidc_issuer           = var.oidc_issuer
  oidc_client_id        = var.oidc_client_id
  oidc_client_secret    = var.oidc_client_secret
  oidc_device_client_id = var.oidc_device_client_id
  oidc_scopes           = ["openid", "profile", "email"]
  admin_groups          = var.admin_groups
  allowed_cidrs         = var.allowed_cidrs

  # Pin a release in production.
  image = "ghcr.io/thasami/backyard:latest"

  database      = "dynamodb"
  desired_count = 2

  enable_bedrock        = true
  enable_lambda_gateway = var.enable_lambda_gateway
  lambda_zip_path       = "${path.root}/../../../../../dist/backyard-lambda.zip"

  tags = {
    Environment = "production"
  }
}
