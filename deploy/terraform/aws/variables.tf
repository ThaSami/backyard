# -----------------------------------------------------------------------------
# General
# -----------------------------------------------------------------------------

variable "name" {
  description = "Name prefix for every resource (ALB, ECS cluster/service, IAM roles, secrets, log groups...). Lowercase letters, digits and hyphens; at most 24 characters so derived names stay within AWS limits."
  type        = string
  default     = "backyard"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,22}[a-z0-9]$", var.name))
    error_message = "name must be 2-24 characters of lowercase letters, digits and hyphens, start with a letter and not end with a hyphen."
  }
}

variable "tags" {
  description = "Tags applied to every taggable resource created by the module (in addition to any provider default_tags)."
  type        = map(string)
  default     = {}
}

variable "domain" {
  description = "Base domain of the deployment, e.g. \"backyard.acme.com\". The hub is served at https://<domain>/ and, in host url_mode, site \"foo\" at https://foo.<domain>/."
  type        = string

  validation {
    condition     = can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$", var.domain))
    error_message = "domain must be a lowercase DNS name such as backyard.example.com (no scheme, port, path or trailing dot)."
  }
}

variable "url_mode" {
  description = "\"host\" (default): sites live on wildcard subdomains (<site>.<domain>), which needs a wildcard certificate and DNS record. \"path\": sites live at https://<domain>/s/<site>/ (no wildcard DNS/TLS needed; sites share one origin)."
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
}

# -----------------------------------------------------------------------------
# DNS and TLS
# -----------------------------------------------------------------------------

variable "route53_zone_id" {
  description = "Route 53 hosted zone that contains var.domain. A/ALIAS records for the apex (and the wildcard in host mode) are created in it and, unless certificate_arn is set, the ACM certificate is validated through it, which requires a public zone (ACM cannot see private zones: with a private zone for an internal ALB, also pass certificate_arn). When null, the certificate validation records and the DNS records to create are exported as outputs."
  type        = string
  default     = null
}

variable "create_dns_records" {
  description = "Create the A/ALIAS records for the domain (and *.domain in host mode) in route53_zone_id. Ignored when route53_zone_id is null."
  type        = bool
  default     = true
}

variable "certificate_arn" {
  description = "Existing ACM certificate ARN (in the deployment region) covering the domain and, in host mode, *.domain. When null a DNS-validated certificate is requested."
  type        = string
  default     = null

  validation {
    condition     = var.certificate_arn == null ? true : can(regex("^arn:aws[a-z-]*:acm:[a-z0-9-]+:[0-9]{12}:certificate/.+$", var.certificate_arn))
    error_message = "certificate_arn must be an ACM certificate ARN."
  }
}

variable "certificate_validation_timeout" {
  description = "How long apply waits for the ACM certificate to be issued. Without route53_zone_id you must create the validation CNAMEs yourself during this window (see README)."
  type        = string
  default     = "45m"
}

# -----------------------------------------------------------------------------
# Network
# -----------------------------------------------------------------------------

variable "vpc_id" {
  description = "Existing VPC to deploy into. When null (default) a VPC with public and private subnets across az_count availability zones is created."
  type        = string
  default     = null
}

variable "public_subnet_ids" {
  description = "Existing public subnets (one per AZ, at least two) for an internet-facing ALB. Required when vpc_id is set and internal = false or tasks_in_public_subnets = true."
  type        = list(string)
  default     = []
}

variable "private_subnet_ids" {
  description = "Existing private subnets (at least two, with a NAT or egress route unless tasks_in_public_subnets is set) for ECS tasks, RDS, ElastiCache and an internal ALB. Required when vpc_id is set."
  type        = list(string)
  default     = []
}

variable "vpc_cidr" {
  description = "CIDR block of the VPC created when vpc_id is null. Subnets are carved as /prefix+4 blocks (public first, private from the 9th block)."
  type        = string
  default     = "10.40.0.0/16"

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0)) && try(tonumber(split("/", var.vpc_cidr)[1]) >= 16 && tonumber(split("/", var.vpc_cidr)[1]) <= 22, false)
    error_message = "vpc_cidr must be a valid IPv4 CIDR with a prefix between /16 and /22."
  }
}

variable "az_count" {
  description = "Number of availability zones used by the created VPC (2-4)."
  type        = number
  default     = 2

  validation {
    condition     = var.az_count >= 2 && var.az_count <= 4 && floor(var.az_count) == var.az_count
    error_message = "az_count must be an integer between 2 and 4."
  }
}

variable "single_nat_gateway" {
  description = "Use one shared NAT gateway for the created VPC (cheaper) instead of one per AZ (zone-redundant egress)."
  type        = bool
  default     = true
}

variable "tasks_in_public_subnets" {
  description = "Run ECS tasks in the public subnets with public IPs instead of private subnets behind NAT. Saves the NAT gateway cost (no NAT is created in the module VPC); tasks still only accept traffic from the ALB security group."
  type        = bool
  default     = false
}

# -----------------------------------------------------------------------------
# Load balancer
# -----------------------------------------------------------------------------

variable "internal" {
  description = "Create an internal ALB (reachable only from the VPC / connected networks: VPN, Direct Connect, peering) instead of an internet-facing one."
  type        = bool
  default     = false
}

variable "allowed_cidrs" {
  description = "IPv4 CIDR blocks allowed to reach the ALB on ports 80/443. Narrow this to your corporate egress ranges for defence in depth."
  type        = list(string)
  default     = ["0.0.0.0/0"]

  validation {
    condition     = alltrue([for c in var.allowed_cidrs : can(cidrhost(c, 0)) && !can(regex(":", c))])
    error_message = "allowed_cidrs must contain IPv4 CIDR blocks such as 203.0.113.0/24."
  }
}

variable "allowed_security_group_ids" {
  description = "Security groups allowed to reach the ALB on ports 80/443 (e.g. a VPN or bastion security group)."
  type        = list(string)
  default     = []
}

variable "alb_idle_timeout" {
  description = "ALB idle timeout in seconds. Long enough for streamed AI responses; websockets are kept alive by 25 s pings. Backyard's server.idle_timeout is set 60 s higher."
  type        = number
  default     = 300

  validation {
    condition     = var.alb_idle_timeout >= 60 && var.alb_idle_timeout <= 4000
    error_message = "alb_idle_timeout must be between 60 and 4000 seconds."
  }
}

variable "reject_forwarded_host_header" {
  description = "Answer 400 to requests that carry an X-Forwarded-Host header. The ALB passes that header through from clients unchanged while Backyard trusts it from the ALB, so leave this on unless a proxy in front of the ALB sets it deliberately."
  type        = bool
  default     = true
}

variable "ssl_policy" {
  description = "TLS security policy of the HTTPS listener."
  type        = string
  default     = "ELBSecurityPolicy-TLS13-1-2-2021-06"
}

variable "hsts_header" {
  description = "Strict-Transport-Security header value added by the ALB to every HTTPS response (null disables). includeSubDomains is appropriate because every site is a subdomain served over HTTPS."
  type        = string
  default     = "max-age=31536000; includeSubDomains"
}

variable "waf_web_acl_arn" {
  description = "Optional AWS WAFv2 web ACL (REGIONAL scope) to associate with the ALB."
  type        = string
  default     = null
}

variable "alb_access_logs_bucket" {
  description = "Optional existing S3 bucket for ALB access logs (its bucket policy must allow ELB log delivery)."
  type        = string
  default     = null
}

variable "alb_access_logs_prefix" {
  description = "Key prefix for ALB access logs."
  type        = string
  default     = "alb"
}

# -----------------------------------------------------------------------------
# Authentication
# -----------------------------------------------------------------------------

variable "auth_mode" {
  description = "\"none\": no login, anonymous identities (labs/dev; restrict allowed_cidrs or use internal = true). \"oidc\": Backyard's built-in OpenID Connect login (Okta, Entra ID, Google...). \"alb\": the ALB authenticates every request with authenticate-oidc and Backyard verifies the signed X-Amzn-Oidc-Data header."
  type        = string
  default     = "none"

  validation {
    condition     = contains(["none", "oidc", "alb"], var.auth_mode)
    error_message = "auth_mode must be one of none, oidc, alb."
  }
}

variable "oidc_issuer" {
  description = "OIDC issuer URL, e.g. https://acme.okta.com/oauth2/default (Okta custom authorization server) or https://acme.okta.com (org server). Required for auth_mode oidc and alb; optional in none mode (only for CLI device-flow tokens)."
  type        = string
  default     = null

  validation {
    condition     = var.oidc_issuer == null ? true : can(regex("^https://[^\\s]+[^/\\s]$", var.oidc_issuer))
    error_message = "oidc_issuer must be an https:// URL without a trailing slash."
  }
}

variable "oidc_client_id" {
  description = "OIDC client id of the web application (Okta \"Web\" app integration). Required for auth_mode oidc and alb."
  type        = string
  default     = null
}

variable "oidc_client_secret" {
  description = "OIDC client secret of the web application. Required for auth_mode alb; strongly recommended for oidc (stored in Secrets Manager and injected as OIDC_CLIENT_SECRET)."
  type        = string
  default     = null
  sensitive   = true

  validation {
    # Backyard expands "$${OIDC_CLIENT_SECRET}" inside a double-quoted YAML
    # string before parsing, so these characters would corrupt the config.
    condition     = var.oidc_client_secret == null ? true : !can(regex("[\"\\\\\\r\\n]", var.oidc_client_secret))
    error_message = "oidc_client_secret must not contain double quotes, backslashes or line breaks."
  }
}

variable "oidc_device_client_id" {
  description = "Optional OIDC client id of a native app with the device-authorization grant enabled. Enables `backyard login --device` and lets the deploy gateways accept IdP-issued JWTs."
  type        = string
  default     = null
}

variable "oidc_scopes" {
  description = "Scopes requested at login (Backyard's oidc mode and the ALB authenticate-oidc action). Add \"groups\" if your IdP exposes a groups scope/claim used by admin_groups or allowed_groups."
  type        = list(string)
  default     = ["openid", "profile", "email"]

  validation {
    condition     = contains(var.oidc_scopes, "openid")
    error_message = "oidc_scopes must include \"openid\"."
  }
}

variable "oidc_use_userinfo" {
  description = "In oidc mode, also call the IdP userinfo endpoint to enrich the ID token claims (e.g. groups from an Okta org authorization server)."
  type        = bool
  default     = false
}

variable "oidc_endpoints" {
  description = "ALB mode only: explicit authorization/token/userinfo endpoints. When null they are read from <oidc_issuer>/.well-known/openid-configuration at plan time."
  type = object({
    authorization_endpoint = string
    token_endpoint         = string
    user_info_endpoint     = string
  })
  default = null

  validation {
    condition = var.oidc_endpoints == null ? true : alltrue([
      for u in values(var.oidc_endpoints) : can(regex("^https://[^\\s/]+", u))
    ])
    error_message = "oidc_endpoints must be https:// URLs."
  }
}

variable "alb_session_timeout" {
  description = "ALB mode only: lifetime in seconds of the ALB authentication session cookie (default 12 h, matching Backyard's own sessions)."
  type        = number
  default     = 43200

  validation {
    condition     = var.alb_session_timeout >= 60 && var.alb_session_timeout <= 604800
    error_message = "alb_session_timeout must be between 60 and 604800 seconds."
  }
}

variable "admins" {
  description = "User ids / emails with Backyard admin rights (auth.admins)."
  type        = list(string)
  default     = []
}

variable "admin_groups" {
  description = "IdP groups whose members are Backyard admins (auth.admin_groups)."
  type        = list(string)
  default     = []
}

variable "allowed_domains" {
  description = "Restrict access to these email domains (auth.allowed_domains; empty = any authenticated user)."
  type        = list(string)
  default     = []
}

variable "allowed_emails" {
  description = "Also admit these exact email addresses (auth.allowed_emails): with allowed_domains either admits; alone, only the listed addresses get in."
  type        = list(string)
  default     = []
}

variable "allowed_groups" {
  description = "Restrict access to members of these IdP groups (auth.allowed_groups; empty = any authenticated user)."
  type        = list(string)
  default     = []
}

variable "static_tokens" {
  description = "Static bearer tokens for CI (auth.tokens). Only the hex SHA-256 of each token is stored (`backyard token hash <token>` prints it)."
  type = list(object({
    name   = string
    sha256 = string
    admin  = optional(bool, false)
  }))
  default = []

  validation {
    condition     = alltrue([for t in var.static_tokens : can(regex("^[0-9a-f]{64}$", t.sha256)) && length(trimspace(t.name)) > 0])
    error_message = "Each static token needs a name and a 64-character lowercase hex sha256."
  }
}

variable "allow_anonymous_deploys" {
  description = "Allow deploys without any token (deploy.allow_anonymous). Only valid with auth_mode = \"none\" and enable_lambda_gateway = false (the function URL is public); never enable on an internet-facing ALB."
  type        = bool
  default     = false
}

# -----------------------------------------------------------------------------
# Container / ECS
# -----------------------------------------------------------------------------

variable "image" {
  description = "Backyard container image. Pin a version tag (or digest) in production so task restarts never pick up an unexpected release."
  type        = string
  default     = "ghcr.io/thasami/backyard:latest"
}

variable "image_pull_secret_arn" {
  description = "Optional Secrets Manager secret ({\"username\",\"password\"}) used as private registry credentials for the image."
  type        = string
  default     = null
}

variable "container_command" {
  description = "Optional command override for the container (e.g. [\"serve\"] when the image entrypoint is the backyard binary). null uses the image default."
  type        = list(string)
  default     = null
}

variable "cpu" {
  description = "Fargate task CPU units."
  type        = number
  default     = 1024

  validation {
    condition     = contains([256, 512, 1024, 2048, 4096, 8192, 16384], var.cpu)
    error_message = "cpu must be one of 256, 512, 1024, 2048, 4096, 8192, 16384."
  }
}

variable "memory" {
  description = "Fargate task memory (MiB); must be a valid combination with cpu."
  type        = number
  default     = 2048
}

variable "cpu_architecture" {
  description = "Fargate CPU architecture: X86_64 or ARM64 (Graviton, ~20% cheaper; requires a multi-arch image)."
  type        = string
  default     = "X86_64"

  validation {
    condition     = contains(["X86_64", "ARM64"], var.cpu_architecture)
    error_message = "cpu_architecture must be X86_64 or ARM64."
  }
}

variable "desired_count" {
  description = "Baseline number of Backyard tasks (also the autoscaling minimum). With more than one task (or autoscaling above one) a shared pub/sub is configured: Redis/Valkey for DynamoDB, LISTEN/NOTIFY for Postgres."
  type        = number
  default     = 2

  validation {
    condition     = var.desired_count >= 1 && floor(var.desired_count) == var.desired_count
    error_message = "desired_count must be an integer >= 1."
  }
}

variable "enable_autoscaling" {
  description = "Scale the service between desired_count and autoscaling_max_capacity on average CPU. When false the service runs exactly desired_count tasks."
  type        = bool
  default     = true
}

variable "autoscaling_max_capacity" {
  description = "Maximum number of tasks when autoscaling is enabled."
  type        = number
  default     = 6

  validation {
    condition     = var.autoscaling_max_capacity >= 1 && floor(var.autoscaling_max_capacity) == var.autoscaling_max_capacity
    error_message = "autoscaling_max_capacity must be an integer >= 1."
  }
}

variable "autoscaling_cpu_target" {
  description = "Target average CPU utilisation (percent) for target-tracking autoscaling."
  type        = number
  default     = 60

  validation {
    condition     = var.autoscaling_cpu_target >= 10 && var.autoscaling_cpu_target <= 90
    error_message = "autoscaling_cpu_target must be between 10 and 90."
  }
}

variable "use_fargate_spot" {
  description = "Run tasks above the first one on Fargate Spot (cheaper, may be interrupted with two minutes notice; websocket clients reconnect automatically)."
  type        = bool
  default     = false
}

variable "enable_execute_command" {
  description = "Enable ECS Exec (`aws ecs execute-command`) for debugging. Grants the task role the SSM messages permissions."
  type        = bool
  default     = false
}

variable "container_insights" {
  description = "Enable CloudWatch Container Insights on the ECS cluster (additional CloudWatch cost)."
  type        = bool
  default     = true
}

variable "health_check_grace_period" {
  description = "Seconds ECS ignores failing ALB health checks after a task starts."
  type        = number
  default     = 60
}

variable "wait_for_steady_state" {
  description = "Make terraform apply wait until the ECS service deployment is stable."
  type        = bool
  default     = false
}

variable "log_retention_days" {
  description = "CloudWatch Logs retention for the service and Lambda gateway log groups."
  type        = number
  default     = 30

  validation {
    condition     = contains([1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653], var.log_retention_days)
    error_message = "log_retention_days must be a retention period supported by CloudWatch Logs (1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653)."
  }
}

variable "log_kms_key_arn" {
  description = "Optional KMS key for the CloudWatch log groups (its key policy must allow the CloudWatch Logs service principal)."
  type        = string
  default     = null
}

variable "log_level" {
  description = "Backyard log level (telemetry.log_level)."
  type        = string
  default     = "info"

  validation {
    condition     = contains(["debug", "info", "warn", "error"], var.log_level)
    error_message = "log_level must be one of debug, info, warn, error."
  }
}

variable "access_log" {
  description = "Log every HTTP request (telemetry.access_log)."
  type        = bool
  default     = false
}

variable "extra_environment" {
  description = "Additional plain environment variables for the Backyard container (e.g. values referenced from config_overrides). Do not put secrets here; use extra_secrets."
  type        = map(string)
  default     = {}

  validation {
    condition     = alltrue([for k in keys(var.extra_environment) : can(regex("^[A-Za-z_][A-Za-z0-9_]*$", k)) && !contains(["BACKYARD_CONFIG_YAML", "BACKYARD_SECRET", "OIDC_CLIENT_SECRET", "DATABASE_URL", "REDIS_URL", "AWS_REGION"], k)])
    error_message = "extra_environment keys must be valid environment variable names and must not override variables managed by the module."
  }
}

variable "extra_secrets" {
  description = "Additional secret environment variables: name => Secrets Manager secret ARN (optionally suffixed with :json-key:: ) or SSM parameter ARN. The execution role is granted read access."
  type        = map(string)
  default     = {}

  validation {
    condition = alltrue([
      for k, v in var.extra_secrets :
      can(regex("^[A-Za-z_][A-Za-z0-9_]*$", k)) &&
      !contains(["BACKYARD_CONFIG_YAML", "BACKYARD_SECRET", "OIDC_CLIENT_SECRET", "DATABASE_URL", "REDIS_URL", "AWS_REGION"], k) &&
      can(regex("^arn:aws[a-z-]*:(secretsmanager|ssm):", v))
    ])
    error_message = "extra_secrets keys must be environment variable names not managed by the module, and values Secrets Manager or SSM parameter ARNs."
  }
}

variable "task_role_policy_arns" {
  description = "Additional IAM policy ARNs attached to the task role (e.g. Athena/Glue access for a warehouse source)."
  type        = list(string)
  default     = []
}

variable "kms_key_arn" {
  description = "Optional customer-managed KMS key used for S3 (SSE-KMS), DynamoDB, RDS, ElastiCache, Secrets Manager and the Lambda environment. null uses AWS-managed encryption everywhere."
  type        = string
  default     = null
}

# -----------------------------------------------------------------------------
# Storage (S3)
# -----------------------------------------------------------------------------

variable "bucket_name" {
  description = "Name of the S3 bucket holding site files and uploads. Default: <name>-sites-<account id>-<region>."
  type        = string
  default     = null

  validation {
    condition     = var.bucket_name == null ? true : can(regex("^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$", var.bucket_name))
    error_message = "bucket_name must be a valid S3 bucket name."
  }
}

variable "bucket_force_destroy" {
  description = "Allow terraform destroy to delete the bucket even when it contains sites and uploads."
  type        = bool
  default     = false
}

variable "bucket_versioning" {
  description = "Enable S3 versioning (recover overwritten or deleted site files)."
  type        = bool
  default     = true
}

variable "noncurrent_version_expiration_days" {
  description = "Days after which noncurrent object versions are permanently deleted (when versioning is enabled)."
  type        = number
  default     = 30

  validation {
    condition     = var.noncurrent_version_expiration_days >= 1
    error_message = "noncurrent_version_expiration_days must be >= 1."
  }
}

variable "create_deployer_policy" {
  description = "Create an IAM policy granting direct `aws s3 sync` deploy access to sites/* (attach it to CI roles or developer groups that use `backyard deploy --method s3`)."
  type        = bool
  default     = true
}

# -----------------------------------------------------------------------------
# Database
# -----------------------------------------------------------------------------

variable "database" {
  description = "Document store behind backyard.db: \"dynamodb\" (default; serverless, on-demand) or \"postgres\" (Amazon RDS for PostgreSQL)."
  type        = string
  default     = "dynamodb"

  validation {
    condition     = contains(["dynamodb", "postgres"], var.database)
    error_message = "database must be \"dynamodb\" or \"postgres\"."
  }
}

variable "dynamodb_table_name" {
  description = "DynamoDB table name (default: var.name)."
  type        = string
  default     = null
}

variable "dynamodb_point_in_time_recovery" {
  description = "Enable DynamoDB point-in-time recovery (continuous backups)."
  type        = bool
  default     = true
}

variable "deletion_protection" {
  description = "Protect the DynamoDB table and RDS instance from deletion. Set to false and apply before terraform destroy."
  type        = bool
  default     = true
}

variable "db_final_snapshot" {
  description = "Take a final snapshot (<name>-final-<random>) when the RDS instance is deleted."
  type        = bool
  default     = true
}

variable "db_instance_class" {
  description = "RDS instance class (database = \"postgres\")."
  type        = string
  default     = "db.t4g.micro"
}

variable "db_engine_version" {
  description = "RDS PostgreSQL engine version (major, or major.minor)."
  type        = string
  default     = "16"

  validation {
    condition     = can(regex("^16(\\.[0-9]+)?$", var.db_engine_version))
    error_message = "db_engine_version must be PostgreSQL 16 (\"16\" or \"16.x\")."
  }
}

variable "db_allocated_storage" {
  description = "Initial RDS storage in GiB (gp3)."
  type        = number
  default     = 20
}

variable "db_max_allocated_storage" {
  description = "Upper limit for RDS storage autoscaling in GiB (0 disables autoscaling)."
  type        = number
  default     = 100
}

variable "db_max_connections_per_task" {
  description = "Size of each task's PostgreSQL connection pool (database.postgres.max_conns). Keep (autoscaling max tasks x 2 during deployments) x (this + 1 for pub/sub) below the instance's max_connections (about 80 on db.t4g.micro)."
  type        = number
  default     = 10

  validation {
    condition     = var.db_max_connections_per_task >= 2 && var.db_max_connections_per_task <= 200 && floor(var.db_max_connections_per_task) == var.db_max_connections_per_task
    error_message = "db_max_connections_per_task must be an integer between 2 and 200."
  }
}

variable "db_multi_az" {
  description = "Run RDS in Multi-AZ mode (standby in a second AZ)."
  type        = bool
  default     = false
}

variable "db_backup_retention_days" {
  description = "RDS automated backup retention in days."
  type        = number
  default     = 7

  validation {
    condition     = var.db_backup_retention_days >= 1 && var.db_backup_retention_days <= 35
    error_message = "db_backup_retention_days must be between 1 and 35."
  }
}

variable "db_performance_insights" {
  description = "Enable RDS Performance Insights (7-day retention is free)."
  type        = bool
  default     = false
}

# -----------------------------------------------------------------------------
# Pub/sub
# -----------------------------------------------------------------------------

variable "pubsub" {
  description = "Realtime fan-out between tasks. \"auto\" (default): postgres with database = postgres; redis (an ElastiCache Valkey cluster is created) with DynamoDB when more than one task can run; otherwise memory. \"memory\" is only correct for a single task."
  type        = string
  default     = "auto"

  validation {
    condition     = contains(["auto", "memory", "redis", "postgres"], var.pubsub)
    error_message = "pubsub must be one of auto, memory, redis, postgres."
  }
}

variable "redis_node_type" {
  description = "ElastiCache node type for the pub/sub cluster."
  type        = string
  default     = "cache.t4g.micro"
}

variable "redis_num_nodes" {
  description = "Number of ElastiCache nodes (1 = single node; 2+ adds replicas with automatic failover across AZs)."
  type        = number
  default     = 1

  validation {
    condition     = var.redis_num_nodes >= 1 && var.redis_num_nodes <= 6
    error_message = "redis_num_nodes must be between 1 and 6."
  }
}

variable "redis_engine_version" {
  description = "ElastiCache for Valkey engine version (major.minor)."
  type        = string
  default     = "8.0"
}

# -----------------------------------------------------------------------------
# AI
# -----------------------------------------------------------------------------

variable "enable_bedrock" {
  description = "Grant the task role access to Amazon Bedrock (bedrock:InvokeModel* — which also authorises Converse/ConverseStream — and bedrock-mantle:CreateInference for Claude through the Messages API endpoint). When ai_config is null, a default AI config using Claude on Bedrock is generated."
  type        = bool
  default     = true
}

variable "bedrock_region" {
  description = "Region for Bedrock calls (default: the deployment region)."
  type        = string
  default     = null
}

variable "bedrock_chat_model" {
  description = "Bedrock model id behind the default \"smart\" chat alias (Claude in Amazon Bedrock, Messages API)."
  type        = string
  default     = "anthropic.claude-opus-5-5"
}

variable "bedrock_fast_model" {
  description = "Bedrock model id behind the \"fast\" chat alias (null omits the alias)."
  type        = string
  default     = "anthropic.claude-haiku-5-5"
}

variable "bedrock_embedding_model" {
  description = "Bedrock embedding model behind the \"embed\" alias (amazon.titan-embed-* or cohere.embed-*; null omits the alias)."
  type        = string
  default     = "amazon.titan-embed-text-v2:0"
}

variable "bedrock_image_model" {
  description = "Bedrock image model behind the \"image\" alias (Amazon Nova Canvas / Titan Image / Stability). null: amazon.nova-canvas-v1:0 when the Bedrock region offers it (us-east-1, eu-west-1, ap-northeast-1), else no image alias. \"\" disables image generation."
  type        = string
  default     = null
}

variable "bedrock_model_arns" {
  description = "Bedrock resources the task role may invoke. null allows all foundation models and this account's inference profiles."
  type        = list(string)
  default     = null
}

variable "bedrock_allow_marketplace_subscribe" {
  description = "Grant aws-marketplace:Subscribe/ViewSubscriptions (only when called via Bedrock) so the first invocation of a third-party model such as Claude can enable it for the account, as in the AWS managed policy AmazonBedrockMantleInferenceAccess. Set false if an administrator manages model access."
  type        = bool
  default     = true
}

variable "ai_config" {
  description = <<-EOT
    YAML for Backyard's `ai:` section (its body, without the `ai:` key), e.g. providers with api_key: "$${ANTHROPIC_API_KEY}". Overrides the generated Bedrock default. Note that in Terraform strings and tfvars files a literal $${VAR} must be written as $$${VAR}.
  EOT
  type        = string
  default     = null

  validation {
    condition = var.ai_config == null ? true : (
      can(keys(yamldecode(var.ai_config))) &&
      alltrue([for k in try(keys(yamldecode(var.ai_config)), []) : contains([
        "enabled", "default_model", "default_image_model", "default_embedding_model", "providers", "models",
        "allow_raw_models", "expose_model_ids", "max_tokens", "request_timeout", "system_prompt",
      ], k)])
    )
    error_message = "ai_config must be a YAML mapping containing only keys of Backyard's ai section (enabled, default_model, default_image_model, default_embedding_model, providers, models, allow_raw_models, expose_model_ids, max_tokens, request_timeout, system_prompt); do not wrap it in an `ai:` key."
  }
}

variable "ai_api_keys" {
  description = "AI provider credentials, as environment variable name => value (e.g. { ANTHROPIC_API_KEY = \"sk-ant-...\" }). Stored as one JSON secret in Secrets Manager and injected into the container; reference them from ai_config as $${NAME}."
  type        = map(string)
  default     = {}
  sensitive   = true

  validation {
    condition = alltrue([
      for k in keys(var.ai_api_keys) :
      can(regex("^[A-Z_][A-Z0-9_]*$", k)) && !contains(["BACKYARD_CONFIG_YAML", "BACKYARD_SECRET", "OIDC_CLIENT_SECRET", "DATABASE_URL", "REDIS_URL", "AWS_REGION"], k) && !startswith(k, "AWS_")
    ])
    error_message = "ai_api_keys keys must be UPPER_CASE environment variable names that are not managed by the module and do not start with AWS_."
  }

  validation {
    # Referenced as "$${NAME}" inside double-quoted YAML strings, which
    # Backyard expands before parsing.
    condition     = alltrue([for v in values(var.ai_api_keys) : length(v) > 0 && !can(regex("[\"\\\\\\r\\n]", v))])
    error_message = "ai_api_keys values must be non-empty and must not contain double quotes, backslashes or line breaks."
  }
}

# -----------------------------------------------------------------------------
# Additional configuration
# -----------------------------------------------------------------------------

variable "config_overrides" {
  description = "YAML with additional top-level Backyard config sections that the module does not manage: identity, warehouse, sites, files, ratelimit, realtime, mcp (mcp.enabled turns on the Claude connector). Use $$${VAR} for environment references (see extra_environment / extra_secrets)."
  type        = string
  default     = null

  validation {
    condition = var.config_overrides == null ? true : (
      can(keys(yamldecode(var.config_overrides))) &&
      alltrue([for k in try(keys(yamldecode(var.config_overrides)), []) : contains(["identity", "warehouse", "sites", "files", "ratelimit", "realtime", "mcp"], k)])
    )
    error_message = "config_overrides must be a YAML mapping whose keys are among identity, warehouse, sites, files, ratelimit, realtime, mcp (the other sections are generated from module variables)."
  }
}

# -----------------------------------------------------------------------------
# Lambda deploy gateway
# -----------------------------------------------------------------------------

variable "enable_lambda_gateway" {
  description = "Deploy the standalone deploy gateway as an AWS Lambda function with a function URL (advertised to CLIs as deploy.gateway_url). Requires the zip built by `make lambda`."
  type        = bool
  default     = true
}

variable "lambda_zip_path" {
  description = "Path to the Lambda deployment package (a zip containing the `bootstrap` binary). Default: <repository root>/dist/backyard-lambda.zip relative to this module."
  type        = string
  default     = null
}

variable "lambda_memory_size" {
  description = "Lambda gateway memory in MiB (CPU scales with memory)."
  type        = number
  default     = 512

  validation {
    condition     = var.lambda_memory_size >= 128 && var.lambda_memory_size <= 10240
    error_message = "lambda_memory_size must be between 128 and 10240 MiB."
  }
}

variable "lambda_timeout" {
  description = "Lambda gateway timeout in seconds (commits of large sites list and delete many objects)."
  type        = number
  default     = 60

  validation {
    condition     = var.lambda_timeout >= 3 && var.lambda_timeout <= 900
    error_message = "lambda_timeout must be between 3 and 900 seconds."
  }
}

variable "lambda_reserved_concurrency" {
  description = "Reserved concurrency for the Lambda gateway (caps concurrent executions; null = unreserved)."
  type        = number
  default     = null
}

# -----------------------------------------------------------------------------
# Secrets
# -----------------------------------------------------------------------------

variable "secret_recovery_window_days" {
  description = "Secrets Manager recovery window for deleted secrets (0 deletes immediately, useful for short-lived environments; 7-30 otherwise)."
  type        = number
  default     = 7

  validation {
    condition     = var.secret_recovery_window_days == 0 || (var.secret_recovery_window_days >= 7 && var.secret_recovery_window_days <= 30)
    error_message = "secret_recovery_window_days must be 0 or between 7 and 30."
  }
}
