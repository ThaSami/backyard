variable "region" {
  description = "AWS region to deploy into."
  type        = string
  default     = "us-east-1"
}

variable "domain" {
  description = "Base domain, e.g. backyard.acme.com."
  type        = string
}

variable "route53_zone_id" {
  description = "Route 53 hosted zone containing the domain (null: create the DNS records yourself, see the module README)."
  type        = string
  default     = null
}

variable "auth_mode" {
  description = "none | oidc | alb"
  type        = string
  default     = "oidc"
}

variable "oidc_issuer" {
  description = "OIDC issuer, e.g. https://acme.okta.com/oauth2/default."
  type        = string
  default     = null
}

variable "oidc_client_id" {
  description = "Client id of the Okta web application."
  type        = string
  default     = null
}

variable "oidc_client_secret" {
  description = "Client secret of the Okta web application."
  type        = string
  default     = null
  sensitive   = true
}

variable "oidc_device_client_id" {
  description = "Optional client id of an Okta native app with the device authorization grant (backyard login --device)."
  type        = string
  default     = null
}

variable "admin_groups" {
  description = "IdP groups whose members are Backyard admins."
  type        = list(string)
  default     = []
}

variable "allowed_cidrs" {
  description = "Networks allowed to reach the load balancer."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "enable_lambda_gateway" {
  description = "Deploy the Lambda deploy gateway (requires `make lambda`)."
  type        = bool
  default     = true
}
