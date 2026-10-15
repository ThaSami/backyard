# --- Where Backyard runs -------------------------------------------------------

variable "account_id" {
  description = "Cloudflare account id (Workers & Pages → Account details)."
  type        = string

  validation {
    condition     = can(regex("^[0-9a-f]{32}$", var.account_id))
    error_message = "account_id must be the 32-character hex Cloudflare account id."
  }
}

variable "zone_name" {
  description = "The Cloudflare zone (a domain on your account) that contains base_domain, e.g. \"example.com\"."
  type        = string
}

variable "base_domain" {
  description = "Hub hostname, e.g. \"backyard.example.com\" (or the zone apex). In host mode sites live at <site>.<base_domain>."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z]{2,63}$", var.base_domain))
    error_message = "base_domain must be a lowercase hostname without scheme, port or path."
  }
}

variable "url_mode" {
  description = "\"path\": sites at https://<base_domain>/s/<site>/ (Workers custom domain, free). \"host\": sites at https://<site>.<base_domain>/ (wildcard DNS + Worker routes; see advanced_certificate)."
  type        = string
  default     = "path"

  validation {
    condition     = contains(["path", "host"], var.url_mode)
    error_message = "url_mode must be \"path\" or \"host\"."
  }
}

variable "worker_name" {
  description = "Name of the deployed Worker (`npm run setup` deploys \"backyard\" unless --worker-name is given). Deploy it before applying this module."
  type        = string
  default     = "backyard"
}

variable "manage_dns" {
  description = "Host mode: create the proxied wildcard and hub DNS records. Turn off if you manage them elsewhere. (Path mode needs no records: the Workers custom domain creates its own.)"
  type        = bool
  default     = true
}

variable "dns_record_content" {
  description = "Target of the proxied host-mode records. The Worker answers every request, so the conventional originless placeholder 100:: is used."
  type        = string
  default     = "100::"
}

variable "advanced_certificate" {
  description = "Host mode below the zone apex: order an Advanced Certificate (Advanced Certificate Manager, a paid add-on) covering *.<base_domain>. Universal SSL (free) only covers <zone> and *.<zone>."
  type        = bool
  default     = false
}

variable "certificate_authority" {
  description = "CA for the advanced certificate: \"google\", \"lets_encrypt\" or \"ssl_com\"."
  type        = string
  default     = "google"
}

# --- Cloudflare Access (Zero Trust; free for up to 50 users) -------------------

variable "enable_access" {
  description = "Put Cloudflare Access in front of the hub and sites. Then deploy the Worker with AUTH_MODE=access, ACCESS_TEAM_DOMAIN and ACCESS_AUD (see the worker_vars and setup_command outputs)."
  type        = bool
  default     = true
}

variable "access_app_name" {
  description = "Name of the Access application (shown in the App Launcher)."
  type        = string
  default     = "Backyard"
}

variable "access_session_duration" {
  description = "How long an Access login lasts, e.g. \"24h\" or \"720h\"."
  type        = string
  default     = "24h"
}

variable "allowed_emails" {
  description = "Email addresses allowed in."
  type        = list(string)
  default     = []
}

variable "allowed_email_domains" {
  description = "Email domains allowed in, e.g. [\"acme.com\"]."
  type        = list(string)
  default     = []
}

variable "allowed_access_group_ids" {
  description = "Ids of existing Access groups (Zero Trust → Access → Groups) allowed in."
  type        = list(string)
  default     = []
}

variable "allowed_okta_groups" {
  description = "Okta group names allowed in (needs the Okta identity provider below, or okta_identity_provider_id)."
  type        = list(string)
  default     = []
}

variable "identity_provider_ids" {
  description = "Existing identity providers users may log in with (empty = every login method of the account). The Okta provider created here is added automatically."
  type        = list(string)
  default     = []
}

variable "auto_redirect_to_identity" {
  description = "Skip the Access login page and go straight to the identity provider. Default: on when exactly one identity provider is allowed."
  type        = bool
  default     = null
}

variable "bypass_paths" {
  description = "Hub paths that skip Access because the Worker authenticates them itself: the deploy gateway (DEPLOY_TOKEN / API tokens, signed upload URLs, the public /info), token minting for the CLI (backyard token create sends an API token; a minted token never outlives the one that asked), health checks, and the Claude connector's MCP endpoint, OAuth endpoints and discovery documents (SPEC §8.4; the Worker answers 404 there until mcp_enabled). Change only if you know why."
  type        = list(string)
  default = [
    "/__backyard/api/deploy",
    "/__backyard/api/deploy/*",
    "/__backyard/api/tokens",
    "/__backyard/healthz",
    "/__backyard/readyz",
    "/__backyard/mcp",
    "/__backyard/oauth/*",
    "/.well-known/oauth-authorization-server",
    "/.well-known/oauth-protected-resource/__backyard/mcp",
  ]

  validation {
    condition     = alltrue([for p in var.bypass_paths : startswith(p, "/__backyard/") || contains(["/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"], p)])
    error_message = "bypass_paths must stay below /__backyard/ or be one of the two OAuth discovery documents (never bypass sites or the hub UI)."
  }
}

# --- Claude connector (remote MCP) ----------------------------------------------

variable "mcp_enabled" {
  description = "Serve the Claude connector (remote MCP, SPEC §8.4): MCP_ENABLED=true in worker_vars and setup_args (false otherwise, so setting it back turns the connector off at the next setup), so Claude can publish sites with the user's sign-in. The bypass application already lets its machine paths through."
  type        = bool
  default     = false
}

# --- AI -----------------------------------------------------------------------

variable "ai_enabled" {
  description = "Serve AI to sites (Workers AI binding, SPEC §5.4). false: setup_args carry --no-ai, so setup deploys without the Workers AI binding and with AI_ENABLED=false (every AI endpoint answers 501 unsupported), for an account without Workers AI; true carries --ai, which turns AI back on at the next setup."
  type        = bool
  default     = true
}

# --- Optional: create an Okta identity provider --------------------------------

variable "okta_account_url" {
  description = "Create an Okta identity provider: your Okta org URL, e.g. \"https://acme.okta.com\". Empty = do not create one."
  type        = string
  default     = ""
}

variable "okta_client_id" {
  description = "Client id of the Okta OIDC app integration (sign-in redirect URI: https://<team>.cloudflareaccess.com/cdn-cgi/access/callback)."
  type        = string
  default     = ""
}

variable "okta_client_secret" {
  description = "Client secret of the Okta OIDC app integration."
  type        = string
  default     = ""
  sensitive   = true
}

variable "okta_authorization_server_id" {
  description = "Okta authorization server id (empty = the org authorization server)."
  type        = string
  default     = ""
}

variable "okta_idp_name" {
  description = "Name of the created Okta identity provider."
  type        = string
  default     = "Okta"
}

variable "okta_identity_provider_id" {
  description = "Id of an existing Okta identity provider, for allowed_okta_groups when okta_account_url is empty."
  type        = string
  default     = ""
}
