# -----------------------------------------------------------------------------
# General
# -----------------------------------------------------------------------------

variable "name" {
  description = "Name prefix for every resource (container app, environment, identities, Front Door, Postgres, ...). Lowercase letters, digits and hyphens; 2-20 characters so derived names stay within Azure limits. Globally unique names (storage account, Postgres server, Azure OpenAI subdomain) get a random suffix."
  type        = string
  default     = "backyard"

  validation {
    # Container Apps names also reject consecutive hyphens.
    condition     = can(regex("^[a-z][a-z0-9-]{0,18}[a-z0-9]$", var.name)) && !strcontains(var.name, "--")
    error_message = "name must be 2-20 characters of lowercase letters, digits and single hyphens, start with a letter and not end with a hyphen."
  }
}

variable "location" {
  description = "Azure region for all regional resources, e.g. \"westeurope\" or \"eastus2\"."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9]+$", replace(lower(var.location), " ", "")))
    error_message = "location must be an Azure region name such as westeurope or eastus2."
  }
}

variable "resource_group_name" {
  description = "Resource group for every resource. Defaults to \"<name>-rg\". When create_resource_group is false the group must already exist."
  type        = string
  default     = null
}

variable "create_resource_group" {
  description = "Create the resource group. Set to false to deploy into an existing group named resource_group_name."
  type        = bool
  default     = true
}

variable "tags" {
  description = "Tags applied to every taggable resource created by the module."
  type        = map(string)
  default     = {}
}

variable "domain" {
  description = "Base domain of the deployment, e.g. \"backyard.acme.com\". The hub is served at https://<domain>/ and, in host url_mode, site \"foo\" at https://foo.<domain>/. null serves Backyard on the container app's default *.azurecontainerapps.io hostname (path url_mode only; handy for labs and first experiments)."
  type        = string
  default     = null

  validation {
    condition     = var.domain == null ? true : can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$", var.domain))
    error_message = "domain must be a lowercase DNS name such as backyard.example.com (no scheme, port, path, wildcard or trailing dot)."
  }
}

variable "url_mode" {
  description = "\"host\" (default): sites live on wildcard subdomains (<site>.<domain>) served through Azure Front Door with a wildcard custom domain. \"path\": sites live at https://<domain>/s/<site>/ (no wildcard DNS/TLS needed; sites share one origin)."
  type        = string
  default     = "host"

  validation {
    condition     = contains(["host", "path"], var.url_mode)
    error_message = "url_mode must be \"host\" or \"path\"."
  }
}

variable "display_name" {
  description = "Name shown in the hub UI (server.name)."
  type        = string
  default     = "Backyard"

  validation {
    condition     = length(trimspace(var.display_name)) > 0 && length(var.display_name) <= 64
    error_message = "display_name must be 1-64 characters."
  }
}

# -----------------------------------------------------------------------------
# Edge: Front Door / custom domains / TLS
# -----------------------------------------------------------------------------

variable "edge" {
  description = "How the custom domain reaches the container app. \"frontdoor\" (default): Azure Front Door Standard with an apex custom domain plus, in host mode, a wildcard custom domain (Container Apps cannot bind wildcard hostnames; see README). \"containerapp\": bind the domain directly on the container app (path url_mode only; needs tls_certificate). Ignored when domain is null."
  type        = string
  default     = "frontdoor"

  validation {
    condition     = contains(["frontdoor", "containerapp"], var.edge)
    error_message = "edge must be \"frontdoor\" or \"containerapp\"."
  }
}

variable "tls_certificate" {
  description = "Bring-your-own certificate stored in Azure Key Vault (same subscription), e.g. { key_vault_id = \"/subscriptions/.../vaults/acme-tls\", certificate_name = \"backyard-wildcard\" }. It must cover the domain and, in host mode, *.domain (RSA key, full chain). The latest version is always used, so renewing the certificate in Key Vault rotates it automatically. null with edge \"frontdoor\" uses Front Door managed certificates (validated through _dnsauth TXT records; managed wildcard certificates are not rotated automatically - see README). Required for edge \"containerapp\"."
  type = object({
    key_vault_id     = string
    certificate_name = string
  })
  default = null

  validation {
    condition     = var.tls_certificate == null ? true : can(regex("(?i)^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\\.KeyVault/vaults/[^/]+$", var.tls_certificate.key_vault_id))
    error_message = "tls_certificate.key_vault_id must be a Key Vault resource ID (/subscriptions/<sub>/resourceGroups/<rg>/providers/Microsoft.KeyVault/vaults/<name>)."
  }

  validation {
    condition     = var.tls_certificate == null ? true : can(regex("^[0-9A-Za-z-]{1,127}$", var.tls_certificate.certificate_name))
    error_message = "tls_certificate.certificate_name must be a Key Vault certificate name (letters, digits and hyphens)."
  }
}

variable "tls_key_vault_grant_access" {
  description = "Grant the edge managed identity (used by Front Door / the Container Apps environment) read access to the tls_certificate's secret: an RBAC \"Key Vault Secrets User\" role assignment on RBAC vaults, or a Get access policy on access-policy vaults. Disable when access is managed elsewhere; the identity's principal id is exported as edge_identity_principal_id."
  type        = bool
  default     = true
}

variable "frontdoor_response_timeout_seconds" {
  description = "Front Door origin response timeout (16-240). AI requests can take minutes, so the maximum is the default."
  type        = number
  default     = 240

  validation {
    condition     = var.frontdoor_response_timeout_seconds >= 16 && var.frontdoor_response_timeout_seconds <= 240
    error_message = "frontdoor_response_timeout_seconds must be between 16 and 240."
  }
}

variable "frontdoor_trust_backend_ips" {
  description = "Add the AzureFrontDoor.Backend service-tag ranges to server.trusted_proxies so Backyard resolves the real client IP from X-Forwarded-For (used for rate limiting anonymous users). Reads the service tag list at plan time."
  type        = bool
  default     = true
}

variable "frontdoor_restrict_origin" {
  description = "Only accept container app ingress from the AzureFrontDoor.Backend IPv4 ranges, so the default *.azurecontainerapps.io hostname cannot bypass Front Door. Off by default because the rule list is long and Azure may change it; see README."
  type        = bool
  default     = false
}

variable "dns_zone_name" {
  description = "Azure DNS zone (public) that contains domain, e.g. \"acme.com\" or \"backyard.acme.com\". When set, the module creates the hub, wildcard and validation records. When null, the records to create are exported in the dns_records output."
  type        = string
  default     = null
}

variable "dns_zone_resource_group_name" {
  description = "Resource group of dns_zone_name. Defaults to the deployment resource group."
  type        = string
  default     = null
}

variable "dns_ttl" {
  description = "TTL in seconds for DNS records created in dns_zone_name."
  type        = number
  default     = 300

  validation {
    condition     = var.dns_ttl >= 30 && var.dns_ttl <= 86400
    error_message = "dns_ttl must be between 30 and 86400 seconds."
  }
}

variable "bind_custom_domain" {
  description = "edge \"containerapp\" only: bind the domain to the container app. Azure validates DNS (CNAME/A + asuid TXT) before binding, so null (default) binds right away only when the module manages the records (dns_zone_name). Without dns_zone_name: apply, create the records from the dns_records output, then apply again with true."
  type        = bool
  default     = null
}

variable "extra_trusted_proxies" {
  description = "Additional CIDRs/IPs appended to server.trusted_proxies. RFC 1918, CGNAT (100.64.0.0/10, used inside Container Apps environments) and loopback ranges are always trusted because only the Container Apps ingress can reach the container."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for c in var.extra_trusted_proxies : can(cidrhost(c, 0)) || can(cidrhost("${c}/32", 0)) || can(cidrhost("${c}/128", 0))])
    error_message = "extra_trusted_proxies entries must be CIDRs or IP addresses."
  }
}

# -----------------------------------------------------------------------------
# Authentication
# -----------------------------------------------------------------------------

variable "auth_mode" {
  description = "\"oidc\" (default): Backyard's built-in OpenID Connect login. With oidc_issuer = null an Entra ID app registration is created (create_entra_application); set oidc_issuer/oidc_client_id/oidc_client_secret for Okta, Google, Auth0, Keycloak or an existing Entra app. \"none\": no login (anonymous identities; labs and demos only)."
  type        = string
  default     = "oidc"

  validation {
    condition     = contains(["oidc", "none"], var.auth_mode)
    error_message = "auth_mode must be \"oidc\" or \"none\"."
  }
}

variable "oidc_issuer" {
  description = "OIDC issuer URL of an existing IdP application, e.g. \"https://acme.okta.com/oauth2/default\" or \"https://login.microsoftonline.com/<tenant>/v2.0\". Register https://<domain>/__backyard/auth/callback as its redirect URI. null with auth_mode \"oidc\" creates an Entra ID application instead."
  type        = string
  default     = null

  validation {
    condition     = var.oidc_issuer == null ? true : can(regex("^https://[^\\s/]+(/[^\\s]*)?$", var.oidc_issuer))
    error_message = "oidc_issuer must be an https:// URL."
  }
}

variable "oidc_client_id" {
  description = "Client ID of the existing IdP application (required with oidc_issuer)."
  type        = string
  default     = null

  validation {
    condition     = var.oidc_client_id == null ? true : length(trimspace(var.oidc_client_id)) > 0
    error_message = "oidc_client_id must not be empty."
  }
}

variable "oidc_client_secret" {
  description = "Client secret of the existing IdP application (stored as a Container Apps secret). null for public clients using PKCE only."
  type        = string
  default     = null
  sensitive   = true

  validation {
    # Backyard substitutes the secret into a single-quoted YAML scalar.
    condition     = var.oidc_client_secret == null ? true : !can(regex("['\\r\\n]", var.oidc_client_secret))
    error_message = "oidc_client_secret must not contain single quotes or line breaks."
  }
}

variable "oidc_scopes" {
  description = "Scopes requested at login. null uses openid, profile and email."
  type        = list(string)
  default     = null
}

variable "oidc_use_userinfo" {
  description = "Call the IdP userinfo endpoint to enrich claims (useful for Okta group claims)."
  type        = bool
  default     = false
}

variable "oidc_device_client_id" {
  description = "Public/native client ID enabling the CLI device-authorization flow (`backyard login --device`) with an existing IdP. For the generated Entra app use entra_enable_device_flow instead."
  type        = string
  default     = null
}

variable "oidc_session_ttl" {
  description = "Browser session lifetime (auth.oidc.session_ttl), e.g. \"12h\". null keeps Backyard's default (12h)."
  type        = string
  default     = null

  validation {
    condition     = var.oidc_session_ttl == null ? true : can(regex("^[0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h|d)$|^[0-9]+$", var.oidc_session_ttl))
    error_message = "oidc_session_ttl must be a duration such as 12h, 90m or 1d."
  }
}

variable "token_ttl" {
  description = "Lifetime of Backyard API tokens minted for the CLI (auth.token_ttl), e.g. \"720h\". null keeps Backyard's default (720h)."
  type        = string
  default     = null

  validation {
    condition     = var.token_ttl == null ? true : can(regex("^[0-9]+(\\.[0-9]+)?(ns|us|ms|s|m|h|d)$|^[0-9]+$", var.token_ttl))
    error_message = "token_ttl must be a duration such as 720h or 30d."
  }
}

variable "create_entra_application" {
  description = "With auth_mode \"oidc\" and no oidc_issuer: create an Entra ID app registration (redirect URI https://<domain>/__backyard/auth/callback, group claims, client secret) using the azuread provider. Requires permission to create applications in the tenant."
  type        = bool
  default     = true
}

variable "entra_application_display_name" {
  description = "Display name of the generated Entra ID application. Defaults to \"<display_name> (<domain>)\"."
  type        = string
  default     = null
}

variable "entra_sign_in_audience" {
  description = "Sign-in audience of the generated Entra ID application. Keep AzureADMyOrg (single tenant) for an internal platform."
  type        = string
  default     = "AzureADMyOrg"

  validation {
    condition     = contains(["AzureADMyOrg", "AzureADMultipleOrgs"], var.entra_sign_in_audience)
    error_message = "entra_sign_in_audience must be AzureADMyOrg or AzureADMultipleOrgs."
  }
}

variable "entra_group_membership_claims" {
  description = "groupMembershipClaims of the generated Entra ID application: which groups are emitted in the \"groups\" claim (as object IDs unless entra_group_claim_names). Use [\"ApplicationGroup\"] to emit only groups assigned to the app (avoids the 200-group overage limit); [\"None\"] disables group claims."
  type        = list(string)
  default     = ["SecurityGroup"]

  validation {
    condition     = length(var.entra_group_membership_claims) > 0 && alltrue([for c in var.entra_group_membership_claims : contains(["None", "SecurityGroup", "DirectoryRole", "ApplicationGroup", "All"], c)])
    error_message = "entra_group_membership_claims entries must be None, SecurityGroup, DirectoryRole, ApplicationGroup or All."
  }
}

variable "entra_group_claim_names" {
  description = "Emit group display names instead of object IDs (Entra \"cloud_displayname\"; cloud-only groups assigned to the application, so combine with entra_group_membership_claims = [\"ApplicationGroup\"]). With object IDs, list group object IDs in admin_groups / allowed_groups."
  type        = bool
  default     = false
}

variable "entra_assignment_required" {
  description = "Require users to be assigned to the generated Entra ID application (directly or via a group) before they can sign in."
  type        = bool
  default     = false
}

variable "entra_assigned_principal_object_ids" {
  description = "Object IDs of users or groups to assign to the generated Entra ID application (default access role). Use with entra_assignment_required."
  type        = list(string)
  default     = []
}

variable "entra_owners" {
  description = "Additional owner object IDs for the generated Entra ID application and service principal (the identity running Terraform is always an owner)."
  type        = list(string)
  default     = []
}

variable "entra_enable_device_flow" {
  description = "Allow public client flows on the generated Entra ID application and advertise it for the CLI device-code login (`backyard login --device`)."
  type        = bool
  default     = false
}

variable "entra_grant_admin_consent" {
  description = "Grant tenant-wide admin consent for the Microsoft Graph delegated permissions (openid, profile, email, offline_access, User.Read) so users are not prompted. Requires a privileged Entra role for the identity running Terraform."
  type        = bool
  default     = false
}

variable "entra_client_secret_end_date" {
  description = "Expiry (RFC 3339) of the generated Entra ID client secret. null uses the azuread provider default (2 years). Change entra_client_secret_rotation to rotate early."
  type        = string
  default     = null

  validation {
    condition     = var.entra_client_secret_end_date == null ? true : can(formatdate("YYYY", var.entra_client_secret_end_date))
    error_message = "entra_client_secret_end_date must be an RFC 3339 timestamp, e.g. 2028-01-01T00:00:00Z."
  }
}

variable "entra_client_secret_rotation" {
  description = "Any value; changing it rotates the generated Entra ID client secret (and rolls the container app)."
  type        = string
  default     = "1"
}

variable "admins" {
  description = "User emails/ids with admin rights (auth.admins)."
  type        = list(string)
  default     = []
}

variable "admin_groups" {
  description = "Groups whose members are admins (auth.admin_groups). With Entra ID these are group object IDs unless entra_group_claim_names is true."
  type        = list(string)
  default     = []
}

variable "allowed_domains" {
  description = "Only users with these email domains may sign in (auth.allowed_domains). Empty allows any user the IdP authenticates."
  type        = list(string)
  default     = []
}

variable "allowed_emails" {
  description = "These exact email addresses may sign in too (auth.allowed_emails): with allowed_domains either admits; alone, only the listed addresses get in."
  type        = list(string)
  default     = []
}

variable "allowed_groups" {
  description = "Only members of these groups may sign in (auth.allowed_groups). Empty allows any group."
  type        = list(string)
  default     = []
}

variable "static_tokens" {
  description = "Static bearer tokens for CI (auth.tokens): name plus the hex SHA-256 of the token (`backyard token hash <token>`); the token itself never reaches Terraform."
  type = list(object({
    name   = string
    sha256 = string
    admin  = optional(bool, false)
  }))
  default = []

  validation {
    condition     = alltrue([for t in var.static_tokens : can(regex("^[A-Za-z0-9_.-]{1,64}$", t.name)) && can(regex("^[0-9a-f]{64}$", t.sha256))])
    error_message = "static_tokens entries need a name (letters, digits, ._-) and a lowercase 64-character hex sha256."
  }
}

variable "allow_anonymous_deploy" {
  description = "Allow deploys without any token (deploy.allow_anonymous). Only valid with auth_mode \"none\"; never use on a reachable network."
  type        = bool
  default     = false
}

# -----------------------------------------------------------------------------
# Container app
# -----------------------------------------------------------------------------

variable "image" {
  description = "Backyard container image. Pin a version tag (or digest) in production."
  type        = string
  default     = "ghcr.io/thasami/backyard:latest"

  validation {
    condition     = can(regex("^[a-z0-9][a-zA-Z0-9._:/@-]*[a-zA-Z0-9]$", var.image)) && !can(regex("://", var.image))
    error_message = "image must be a container image reference without a URL scheme, e.g. ghcr.io/thasami/backyard:0.1.0."
  }
}

variable "image_registry" {
  description = "Private registry for image, e.g. { server = \"acme.azurecr.io\", acr_id = \"/subscriptions/.../registries/acme\" }. With acr_id the app's managed identity gets AcrPull and pulls without credentials; otherwise username + image_registry_password are used."
  type = object({
    server   = string
    acr_id   = optional(string)
    username = optional(string)
  })
  default = null

  validation {
    condition     = var.image_registry == null ? true : can(regex("^[a-z0-9.-]+(:[0-9]+)?$", var.image_registry.server))
    error_message = "image_registry.server must be a registry host such as acme.azurecr.io."
  }

  validation {
    condition     = var.image_registry == null ? true : (var.image_registry.acr_id != null || var.image_registry.username != null)
    error_message = "image_registry needs acr_id, or username (with image_registry_password)."
  }
}

variable "image_registry_password" {
  description = "Password/token for image_registry.username (stored as a Container Apps secret). Required when image_registry has no acr_id."
  type        = string
  default     = null
  sensitive   = true
}

variable "container_args" {
  description = "Override the container arguments (default: the image's CMD, which runs `backyard serve`)."
  type        = list(string)
  default     = null
}

variable "cpu" {
  description = "vCPUs per replica (Consumption profile). memory must be cpu x 2 GiB, e.g. 0.5 / \"1Gi\", 1 / \"2Gi\"."
  type        = number
  default     = 0.5

  validation {
    condition     = contains([0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5, 4], var.cpu)
    error_message = "cpu must be one of 0.25, 0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5 or 4."
  }
}

variable "memory" {
  description = "Memory per replica, e.g. \"1Gi\" (must be cpu x 2 GiB on the Consumption profile)."
  type        = string
  default     = "1Gi"

  validation {
    condition     = can(regex("^[0-9]+(\\.[0-9]+)?Gi$", var.memory))
    error_message = "memory must look like \"1Gi\" or \"1.5Gi\"."
  }
}

variable "min_replicas" {
  description = "Minimum replicas. Keep >= 1: websockets, presence and Postgres LISTEN need a running replica and cold starts would delay logins."
  type        = number
  default     = 1

  validation {
    condition     = var.min_replicas >= 0 && var.min_replicas <= 300 && floor(var.min_replicas) == var.min_replicas
    error_message = "min_replicas must be an integer between 0 and 300."
  }
}

variable "max_replicas" {
  description = "Maximum replicas. Each replica opens up to postgres_max_conns_per_replica + 1 Postgres connections; size postgres_sku_name accordingly."
  type        = number
  default     = 5

  validation {
    condition     = var.max_replicas >= 1 && var.max_replicas <= 300 && floor(var.max_replicas) == var.max_replicas
    error_message = "max_replicas must be an integer between 1 and 300."
  }
}

variable "concurrent_requests_per_replica" {
  description = "HTTP scale rule: add replicas when concurrent requests (including open websockets) per replica exceed this value."
  type        = number
  default     = 200

  validation {
    condition     = var.concurrent_requests_per_replica >= 1 && floor(var.concurrent_requests_per_replica) == var.concurrent_requests_per_replica
    error_message = "concurrent_requests_per_replica must be a positive integer."
  }
}

variable "sticky_sessions" {
  description = "Enable Container Apps session affinity (sticky sessions) via the azapi provider. Not required: Backyard is stateless and fans realtime events out through Postgres LISTEN/NOTIFY."
  type        = bool
  default     = false
}

variable "readiness_probe_path" {
  description = "Path of the readiness probe. The default /__backyard/healthz only checks the process; /__backyard/readyz also pings the database and storage (stricter, but a database blip then removes every replica from rotation)."
  type        = string
  default     = "/__backyard/healthz"

  validation {
    condition     = contains(["/__backyard/healthz", "/__backyard/readyz"], var.readiness_probe_path)
    error_message = "readiness_probe_path must be /__backyard/healthz or /__backyard/readyz."
  }
}

variable "extra_env" {
  description = "Additional plain environment variables for the container (non-secret). Names must not collide with the module's (BACKYARD_*, DATABASE_URL, AZURE_CLIENT_ID, OIDC_CLIENT_SECRET, *_API_KEY)."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for k in keys(var.extra_env) : can(regex("^[A-Za-z_][A-Za-z0-9_]*$", k))])
    error_message = "extra_env keys must be valid environment variable names."
  }
}

variable "extra_secret_env" {
  description = "Additional secret environment variables (stored as Container Apps secrets named env-<name>), e.g. a warehouse DSN referenced from extra_config_yaml as $${WAREHOUSE_DSN}. Names: upper-case letters, digits and underscores."
  type        = map(string)
  default     = {}
  sensitive   = true

  validation {
    # try(): nonsensitive() rejects unmarked values on older Terraform versions.
    condition = try(
      nonsensitive(alltrue([for k in keys(var.extra_secret_env) : can(regex("^[A-Z][A-Z0-9_]{0,62}[A-Z0-9]$", k))])),
      alltrue([for k in keys(var.extra_secret_env) : can(regex("^[A-Z][A-Z0-9_]{0,62}[A-Z0-9]$", k))]),
    )
    error_message = "extra_secret_env keys must be upper-case environment variable names (A-Z, 0-9, _; not ending in _)."
  }
}

variable "log_level" {
  description = "Backyard log level (telemetry.log_level)."
  type        = string
  default     = "info"

  validation {
    condition     = contains(["debug", "info", "warn", "error"], var.log_level)
    error_message = "log_level must be debug, info, warn or error."
  }
}

variable "access_log" {
  description = "Log every HTTP request (telemetry.access_log)."
  type        = bool
  default     = false
}

variable "log_analytics_workspace_id" {
  description = "Existing Log Analytics workspace for container logs. null creates one."
  type        = string
  default     = null
}

variable "log_retention_days" {
  description = "Retention of the created Log Analytics workspace (30-730 days)."
  type        = number
  default     = 30

  validation {
    condition     = var.log_retention_days >= 30 && var.log_retention_days <= 730
    error_message = "log_retention_days must be between 30 and 730."
  }
}

variable "extra_config_yaml" {
  description = "Additional backyard.yaml sections appended to the rendered config, limited to top-level keys the module does not render: identity, warehouse, sites, files, ratelimit, realtime. $${VAR} references are expanded from the container environment (see extra_env / extra_secret_env)."
  type        = string
  default     = ""

  validation {
    condition     = trimspace(var.extra_config_yaml) == "" || can(keys(yamldecode(var.extra_config_yaml)))
    error_message = "extra_config_yaml must be a YAML mapping."
  }

  validation {
    condition     = trimspace(var.extra_config_yaml) == "" || alltrue([for k in try(keys(yamldecode(var.extra_config_yaml)), []) : contains(["identity", "warehouse", "sites", "files", "ratelimit", "realtime"], k)])
    error_message = "extra_config_yaml may only contain the top-level keys identity, warehouse, sites, files, ratelimit and realtime; the other sections are rendered from module variables."
  }
}

variable "rbac_propagation_wait" {
  description = "How long to wait after creating role assignments before resources that depend on them (container app, Front Door secret, environment certificate) are created. Azure RBAC is eventually consistent."
  type        = string
  default     = "60s"

  validation {
    condition     = can(regex("^[0-9]+(s|m)$", var.rbac_propagation_wait))
    error_message = "rbac_propagation_wait must look like 60s or 2m."
  }
}

# -----------------------------------------------------------------------------
# Networking
# -----------------------------------------------------------------------------

variable "vnet_integration" {
  description = "Run the Container Apps environment in a virtual network and give the Postgres server private access only (delegated subnet + private DNS zone); recommended for production. false (default) gives Postgres a public endpoint whose firewall (postgres_allow_azure_services) admits all of Azure, other tenants included."
  type        = bool
  default     = false
}

variable "vnet_address_space" {
  description = "Address space of the created virtual network (vnet_integration without existing subnets)."
  type        = list(string)
  default     = ["10.42.0.0/16"]

  validation {
    condition     = length(var.vnet_address_space) > 0 && alltrue([for c in var.vnet_address_space : can(cidrhost(c, 0))])
    error_message = "vnet_address_space must contain at least one CIDR."
  }
}

variable "container_apps_subnet_cidr" {
  description = "CIDR of the created Container Apps infrastructure subnet (/27 or larger; delegated to Microsoft.App/environments)."
  type        = string
  default     = "10.42.0.0/23"

  validation {
    condition     = can(cidrhost(var.container_apps_subnet_cidr, 0)) && tonumber(split("/", var.container_apps_subnet_cidr)[1]) <= 27
    error_message = "container_apps_subnet_cidr must be a CIDR of size /27 or larger."
  }
}

variable "postgres_subnet_cidr" {
  description = "CIDR of the created Postgres subnet (delegated to Microsoft.DBforPostgreSQL/flexibleServers)."
  type        = string
  default     = "10.42.2.0/24"

  validation {
    condition     = can(cidrhost(var.postgres_subnet_cidr, 0)) && tonumber(split("/", var.postgres_subnet_cidr)[1]) <= 28
    error_message = "postgres_subnet_cidr must be a CIDR of size /28 or larger."
  }
}

variable "existing_container_apps_subnet_id" {
  description = "Use this existing subnet (delegated to Microsoft.App/environments, /27 or larger) for the Container Apps environment instead of creating a virtual network. Requires vnet_integration."
  type        = string
  default     = null
}

variable "existing_postgres_subnet_id" {
  description = "Use this existing subnet (delegated to Microsoft.DBforPostgreSQL/flexibleServers) for the Postgres server. Required with existing_container_apps_subnet_id unless database_url is set."
  type        = string
  default     = null
}

variable "existing_postgres_private_dns_zone_id" {
  description = "Existing private DNS zone (*.postgres.database.azure.com) for the Postgres server. null creates one linked to the virtual network."
  type        = string
  default     = null
}

variable "zone_redundant" {
  description = "Zone-redundant Container Apps environment (requires vnet_integration and a region with availability zones)."
  type        = bool
  default     = false
}

# -----------------------------------------------------------------------------
# Storage
# -----------------------------------------------------------------------------

variable "storage_account_name" {
  description = "Name of the created storage account (3-24 lowercase letters/digits, globally unique). null derives one from name plus a random suffix."
  type        = string
  default     = null

  validation {
    condition     = var.storage_account_name == null ? true : can(regex("^[a-z0-9]{3,24}$", var.storage_account_name))
    error_message = "storage_account_name must be 3-24 lowercase letters and digits."
  }
}

variable "storage_container_name" {
  description = "Private blob container holding site files and uploads."
  type        = string
  default     = "backyard"

  validation {
    condition     = can(regex("^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){2,62}$", var.storage_container_name))
    error_message = "storage_container_name must be 3-63 lowercase letters, digits and single hyphens."
  }
}

variable "storage_prefix" {
  description = "Optional key prefix inside the container (storage.azure.prefix)."
  type        = string
  default     = ""
}

variable "storage_replication_type" {
  description = "Replication of the storage account. LRS works in every region; prefer ZRS (or GZRS) where available."
  type        = string
  default     = "LRS"

  validation {
    condition     = contains(["LRS", "ZRS", "GRS", "RAGRS", "GZRS", "RAGZRS"], var.storage_replication_type)
    error_message = "storage_replication_type must be LRS, ZRS, GRS, RAGRS, GZRS or RAGZRS."
  }
}

variable "storage_shared_key_enabled" {
  description = "Allow shared key (account key) authorization on the storage account. Backyard and the CLI use Entra ID (managed identity / az login), so keep this false; set the azurerm provider's storage_use_azuread = true (see examples/basic)."
  type        = bool
  default     = false
}

variable "storage_soft_delete_days" {
  description = "Blob and container soft-delete retention in days (0 disables)."
  type        = number
  default     = 7

  validation {
    condition     = var.storage_soft_delete_days >= 0 && var.storage_soft_delete_days <= 365
    error_message = "storage_soft_delete_days must be between 0 and 365."
  }
}

variable "storage_versioning_enabled" {
  description = "Enable blob versioning (keeps every overwritten site file; increases storage costs)."
  type        = bool
  default     = false
}

variable "storage_cors_extra_origins" {
  description = "Extra origins allowed to PUT/GET presigned blob URLs from a browser. The hub origin (where drag-and-drop deploys run) is always allowed."
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for o in var.storage_cors_extra_origins : can(regex("^https?://[^/\\s]+$", o))])
    error_message = "storage_cors_extra_origins entries must be origins such as https://tools.acme.com (no path)."
  }
}

variable "storage_deployer_principal_ids" {
  description = "Entra object IDs (users, groups or service principals) granted Storage Blob Data Contributor on the container, for direct `backyard deploy --method azure` (az storage blob sync with AZURE_STORAGE_AUTH_MODE=login). An ABAC condition limits their blob reads, writes, deletes and listings to <storage_prefix>sites/."
  type        = list(string)
  default     = []
}

# -----------------------------------------------------------------------------
# Database (Azure Database for PostgreSQL flexible server)
# -----------------------------------------------------------------------------

variable "database_url" {
  description = "Bring your own Postgres: a postgres:// DSN (with sslmode, URL-encoded credentials). When set, no flexible server is created. Used for both database and pub/sub (LISTEN/NOTIFY and advisory locks), so it must reach Postgres directly or through a session-mode pooler, not a transaction-mode PgBouncer (e.g. not the flexible server's built-in PgBouncer on port 6432)."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.database_url == null ? true : (var.database_url == "" || can(regex("^postgres(ql)?://[^\\s']+$", var.database_url)))
    error_message = "database_url must be a postgres:// or postgresql:// URL without whitespace or single quotes."
  }
}

variable "postgres_version" {
  description = "PostgreSQL major version."
  type        = string
  default     = "16"

  validation {
    condition     = contains(["14", "15", "16", "17"], var.postgres_version)
    error_message = "postgres_version must be 14, 15, 16 or 17."
  }
}

variable "postgres_sku_name" {
  description = "Flexible server SKU, e.g. B_Standard_B2s (burstable, ~430 connections), GP_Standard_D2ds_v5 (general purpose, required for high availability)."
  type        = string
  default     = "B_Standard_B2s"

  validation {
    condition     = can(regex("^(B|GP|MO)_[A-Za-z0-9_]+$", var.postgres_sku_name))
    error_message = "postgres_sku_name must look like B_Standard_B2s, GP_Standard_D2ds_v5 or MO_Standard_E2ds_v5."
  }
}

variable "postgres_storage_mb" {
  description = "Allocated storage in MB (auto-grow is enabled)."
  type        = number
  default     = 32768

  validation {
    condition     = contains([32768, 65536, 131072, 262144, 524288, 1048576, 2097152, 4193280, 4194304, 8388608, 16777216, 33553408], var.postgres_storage_mb)
    error_message = "postgres_storage_mb must be a flexible server storage size such as 32768, 65536 or 131072."
  }
}

variable "postgres_backup_retention_days" {
  description = "Backup retention in days (7-35)."
  type        = number
  default     = 7

  validation {
    condition     = var.postgres_backup_retention_days >= 7 && var.postgres_backup_retention_days <= 35
    error_message = "postgres_backup_retention_days must be between 7 and 35."
  }
}

variable "postgres_geo_redundant_backup" {
  description = "Geo-redundant backups (cannot be changed after creation)."
  type        = bool
  default     = false
}

variable "postgres_high_availability" {
  description = "Zone-redundant high availability (standby in another zone). Requires a General Purpose or Memory Optimized SKU."
  type        = bool
  default     = false
}

variable "postgres_zone" {
  description = "Availability zone of the primary server (\"1\", \"2\", \"3\") or null to let Azure choose."
  type        = string
  default     = null
}

variable "postgres_admin_login" {
  description = "Administrator login of the created server (Backyard connects with it)."
  type        = string
  default     = "backyard"

  validation {
    condition     = can(regex("^[a-z][a-z0-9_]{0,62}$", var.postgres_admin_login)) && !contains(["azure_superuser", "azure_pg_admin", "admin", "administrator", "root", "guest", "public"], var.postgres_admin_login)
    error_message = "postgres_admin_login must be a lowercase identifier and not a reserved name (admin, administrator, root, guest, public, azure_*)."
  }
}

variable "postgres_database_name" {
  description = "Database created on the server for Backyard."
  type        = string
  default     = "backyard"

  validation {
    condition     = can(regex("^[a-z][a-z0-9_]{0,62}$", var.postgres_database_name))
    error_message = "postgres_database_name must be a lowercase identifier."
  }
}

variable "postgres_allow_azure_services" {
  description = "Public mode only: add Azure's 0.0.0.0 \"allow Azure services\" firewall rule, which is how Container Apps (no stable outbound IP without a VNet) reach the server. It admits every Azure IP, including other customers' subscriptions; connections still need TLS and the generated password, and the module warns (check postgres_open_to_all_of_azure). Use vnet_integration for production."
  type        = bool
  default     = true
}

variable "postgres_allowed_ip_ranges" {
  description = "Public mode only: extra firewall rules, e.g. { office = { start = \"203.0.113.0\", end = \"203.0.113.255\" } }."
  type = map(object({
    start = string
    end   = string
  }))
  default = {}

  validation {
    condition     = alltrue([for k, r in var.postgres_allowed_ip_ranges : can(regex("^[A-Za-z0-9_-]{1,80}$", k)) && can(cidrhost("${r.start}/32", 0)) && can(cidrhost("${r.end}/32", 0))])
    error_message = "postgres_allowed_ip_ranges keys must be simple names and start/end IPv4 addresses."
  }
}

variable "postgres_max_conns_per_replica" {
  description = "database.postgres.max_conns per replica (pub/sub uses one extra connection)."
  type        = number
  default     = 10

  validation {
    condition     = var.postgres_max_conns_per_replica >= 2 && var.postgres_max_conns_per_replica <= 200
    error_message = "postgres_max_conns_per_replica must be between 2 and 200."
  }
}

# -----------------------------------------------------------------------------
# AI
# -----------------------------------------------------------------------------

variable "azure_openai_enabled" {
  description = "Create an Azure OpenAI account (keyless: the app's managed identity gets \"Cognitive Services OpenAI User\") with azure_openai_deployments, exposed to sites as Backyard AI model aliases through provider \"azure\"."
  type        = bool
  default     = false
}

variable "azure_openai_location" {
  description = "Region of the Azure OpenAI account (model availability differs per region). null uses location."
  type        = string
  default     = null
}

variable "azure_openai_public_network_access" {
  description = "Allow public network access to the Azure OpenAI account (required unless you add a private endpoint yourself)."
  type        = bool
  default     = true
}

variable "azure_openai_deployments" {
  description = "Model deployments keyed by Backyard model alias (the deployment name equals the alias). kind is chat, embedding or image. Pin model_version (Azure upgrades a deployment only when its version is retired); check model, version and SKU availability for your region."
  type = map(object({
    model_name    = string
    model_version = string
    model_format  = optional(string, "OpenAI")
    sku_name      = optional(string, "GlobalStandard")
    capacity      = optional(number, 10)
    kind          = optional(string, "chat")
    description   = optional(string, "")
  }))
  # GA versions with a retirement date in late 2027 (Azure model retirement
  # schedule); review them when upgrading the module.
  default = {
    smart = {
      model_name    = "gpt-5.4"
      model_version = "2026-03-05"
      description   = "Most capable model (Azure OpenAI)"
    }
    fast = {
      model_name    = "gpt-5.4-mini"
      model_version = "2026-03-17"
      description   = "Fast, inexpensive model (Azure OpenAI)"
    }
    embed = {
      model_name    = "text-embedding-3-small"
      model_version = "1"
      kind          = "embedding"
      description   = "Text embeddings (Azure OpenAI)"
    }
  }

  validation {
    condition     = alltrue([for k, d in var.azure_openai_deployments : can(regex("^[A-Za-z0-9][A-Za-z0-9_.-]{1,63}$", k)) && contains(["chat", "embedding", "image"], d.kind) && d.capacity >= 1 && length(d.model_version) > 0])
    error_message = "azure_openai_deployments keys must be 2-64 characters (letters, digits, _ . -), with kind chat/embedding/image, capacity >= 1 and a model_version."
  }
}

variable "anthropic_api_key" {
  description = "Anthropic API key (Container Apps secret) enabling provider \"anthropic\" for ai_models."
  type        = string
  default     = null
  sensitive   = true

  validation {
    # Backyard substitutes the key into a single-quoted YAML scalar.
    condition     = var.anthropic_api_key == null ? true : !can(regex("['\\r\\n]", var.anthropic_api_key))
    error_message = "anthropic_api_key must not contain single quotes or line breaks."
  }
}

variable "openai_api_key" {
  description = "OpenAI API key (Container Apps secret) enabling provider \"openai\" for ai_models."
  type        = string
  default     = null
  sensitive   = true

  validation {
    condition     = var.openai_api_key == null ? true : !can(regex("['\\r\\n]", var.openai_api_key))
    error_message = "openai_api_key must not contain single quotes or line breaks."
  }
}

variable "ai_models" {
  description = "Extra Backyard model aliases for providers \"anthropic\" / \"openai\" (needs the matching API key) or \"azure\" (an existing deployment on the module's Azure OpenAI account), e.g. { smart = { provider = \"anthropic\", model = \"claude-opus-5-5\" } }. effort sets the Anthropic effort for the alias (low, medium, high, xhigh or max). Aliases here override azure_openai_deployments with the same name."
  type = map(object({
    provider    = string
    model       = string
    kind        = optional(string, "chat")
    description = optional(string, "")
    max_tokens  = optional(number, 0)
    effort      = optional(string, "")
  }))
  default = {}

  validation {
    condition     = alltrue([for k, m in var.ai_models : !strcontains(k, "/") && length(k) > 0 && contains(["anthropic", "openai", "azure"], m.provider) && contains(["chat", "embedding", "image"], m.kind) && length(m.model) > 0])
    error_message = "ai_models aliases must not contain '/', provider must be anthropic, openai or azure, kind chat/embedding/image, and model non-empty."
  }

  validation {
    condition     = alltrue([for m in values(var.ai_models) : m.max_tokens >= 0 && floor(m.max_tokens) == m.max_tokens && contains(["", "low", "medium", "high", "xhigh", "max"], m.effort)])
    error_message = "ai_models max_tokens must be a non-negative integer and effort one of low, medium, high, xhigh or max."
  }
}

variable "ai_default_model" {
  description = "Default chat alias (ai.default_model). null lets Backyard pick (smart, default, chat or fast first)."
  type        = string
  default     = null
}

variable "ai_default_image_model" {
  description = "Default image alias (ai.default_image_model)."
  type        = string
  default     = null
}

variable "ai_default_embedding_model" {
  description = "Default embedding alias (ai.default_embedding_model)."
  type        = string
  default     = null
}
