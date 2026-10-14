# Secrets Manager: everything secret reaches the container as an ECS secret
# (environment variable resolved at task start), never as plain task
# definition environment. Note that generated values are also kept in the
# Terraform state: use an encrypted, access-controlled backend.

# server.secret signs sessions, API tokens, deploy ids and upload URLs.
# Alphanumeric so it is safe anywhere it is expanded.
resource "random_password" "backyard_secret" {
  length  = 48
  special = false
}

resource "aws_secretsmanager_secret" "backyard_secret" {
  name                    = "${var.name}/server-secret"
  description             = "Backyard server.secret (BACKYARD_SECRET). Rotating it invalidates sessions and API tokens; redeploy the service afterwards."
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "backyard_secret" {
  secret_id     = aws_secretsmanager_secret.backyard_secret.id
  secret_string = random_password.backyard_secret.result
}

resource "aws_secretsmanager_secret" "oidc_client_secret" {
  count = local.oidc_secret_enabled ? 1 : 0

  name                    = "${var.name}/oidc-client-secret"
  description             = "Backyard OIDC client secret (OIDC_CLIENT_SECRET)"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "oidc_client_secret" {
  count = local.oidc_secret_enabled ? 1 : 0

  secret_id     = aws_secretsmanager_secret.oidc_client_secret[0].id
  secret_string = var.oidc_client_secret
}

# AI provider keys: one JSON secret, one environment variable per key.
resource "aws_secretsmanager_secret" "ai_keys" {
  count = local.has_ai_keys ? 1 : 0

  name                    = "${var.name}/ai-api-keys"
  description             = "Backyard AI provider credentials (JSON: environment variable name => value)"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "ai_keys" {
  count = local.has_ai_keys ? 1 : 0

  secret_id     = aws_secretsmanager_secret.ai_keys[0].id
  secret_string = jsonencode(var.ai_api_keys)
}

resource "aws_secretsmanager_secret" "database_url" {
  count = local.create_postgres ? 1 : 0

  name                    = "${var.name}/database-url"
  description             = "Backyard PostgreSQL DSN (DATABASE_URL)"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "database_url" {
  count = local.create_postgres ? 1 : 0

  secret_id = aws_secretsmanager_secret.database_url[0].id
  secret_string = format(
    "postgres://%s:%s@%s:%d/%s?sslmode=require",
    local.db_username,
    random_password.db[0].result,
    aws_db_instance.this[0].address,
    aws_db_instance.this[0].port,
    local.db_name,
  )
}

resource "aws_secretsmanager_secret" "redis_url" {
  count = local.create_redis ? 1 : 0

  name                    = "${var.name}/redis-url"
  description             = "Backyard pub/sub Valkey URL (REDIS_URL)"
  kms_key_id              = var.kms_key_arn
  recovery_window_in_days = var.secret_recovery_window_days

  tags = local.tags
}

resource "aws_secretsmanager_secret_version" "redis_url" {
  count = local.create_redis ? 1 : 0

  secret_id = aws_secretsmanager_secret.redis_url[0].id
  secret_string = format(
    "rediss://:%s@%s:%d/0",
    random_password.redis[0].result,
    aws_elasticache_replication_group.this[0].primary_endpoint_address,
    aws_elasticache_replication_group.this[0].port,
  )
}

locals {
  # ECS "secrets" of the Backyard container.
  container_secrets = concat(
    [{ name = "BACKYARD_SECRET", valueFrom = aws_secretsmanager_secret.backyard_secret.arn }],
    local.oidc_secret_enabled ? [{ name = "OIDC_CLIENT_SECRET", valueFrom = aws_secretsmanager_secret.oidc_client_secret[0].arn }] : [],
    local.create_postgres ? [{ name = "DATABASE_URL", valueFrom = aws_secretsmanager_secret.database_url[0].arn }] : [],
    local.create_redis ? [{ name = "REDIS_URL", valueFrom = aws_secretsmanager_secret.redis_url[0].arn }] : [],
    [for k in local.ai_key_names : { name = k, valueFrom = "${aws_secretsmanager_secret.ai_keys[0].arn}:${k}::" }],
    [for k in sort(keys(var.extra_secrets)) : { name = k, valueFrom = var.extra_secrets[k] }],
  )

  # Secrets Manager secret ARNs (without any :json-key:stage:version suffix)
  # and SSM parameter ARNs the execution role must be able to read.
  extra_secretsmanager_arns = distinct([
    for v in values(var.extra_secrets) : join(":", slice(split(":", v), 0, min(7, length(split(":", v))))) if can(regex("^arn:aws[a-z-]*:secretsmanager:", v))
  ])
  extra_ssm_parameter_arns = distinct([
    for v in values(var.extra_secrets) : v if can(regex("^arn:aws[a-z-]*:ssm:", v))
  ])

  managed_secret_arns = concat(
    [aws_secretsmanager_secret.backyard_secret.arn],
    aws_secretsmanager_secret.oidc_client_secret[*].arn,
    aws_secretsmanager_secret.ai_keys[*].arn,
    aws_secretsmanager_secret.database_url[*].arn,
    aws_secretsmanager_secret.redis_url[*].arn,
  )
}
