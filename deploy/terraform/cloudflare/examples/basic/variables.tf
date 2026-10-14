variable "account_id" {
  description = "Cloudflare account id."
  type        = string
}

variable "zone_name" {
  description = "Zone that holds base_domain, e.g. \"example.com\"."
  type        = string
}

variable "base_domain" {
  description = "Hub hostname, e.g. \"backyard.example.com\"."
  type        = string
}

variable "url_mode" {
  description = "\"path\" or \"host\"."
  type        = string
  default     = "path"
}

variable "worker_name" {
  description = "Name of the deployed Worker."
  type        = string
  default     = "backyard"
}

variable "allowed_email_domains" {
  description = "Email domains allowed in."
  type        = list(string)
  default     = []
}

variable "allowed_emails" {
  description = "Email addresses allowed in."
  type        = list(string)
  default     = []
}

variable "okta_account_url" {
  description = "Okta org URL (empty = no Okta identity provider)."
  type        = string
  default     = ""
}

variable "okta_client_id" {
  description = "Okta OIDC client id."
  type        = string
  default     = ""
}

variable "okta_client_secret" {
  description = "Okta OIDC client secret."
  type        = string
  default     = ""
  sensitive   = true
}

variable "allowed_okta_groups" {
  description = "Okta groups allowed in."
  type        = list(string)
  default     = []
}
