# Backyard on AWS: ECS Fargate behind an ALB, S3 for sites and uploads,
# DynamoDB or RDS PostgreSQL for backyard.db, optional ElastiCache Valkey for
# realtime fan-out, Secrets Manager for credentials and an optional Lambda
# deploy gateway. See README.md for the architecture and usage.

data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

data "aws_region" "current" {}

locals {
  account_id = data.aws_caller_identity.current.account_id
  partition  = data.aws_partition.current.partition
  region     = data.aws_region.current.region

  tags = merge({ "app" = "backyard", "backyard:deployment" = var.name }, var.tags)

  public_url     = "https://${var.domain}"
  host_mode      = var.url_mode == "host"
  container_name = "backyard"
  container_port = 8080

  # ---------------------------------------------------------------------------
  # Scaling and pub/sub selection
  # ---------------------------------------------------------------------------
  max_tasks      = var.enable_autoscaling ? max(var.desired_count, var.autoscaling_max_capacity) : var.desired_count
  multi_instance = local.max_tasks > 1

  pubsub_type = (
    var.pubsub != "auto" ? var.pubsub :
    var.database == "postgres" ? "postgres" :
    local.multi_instance ? "redis" : "memory"
  )
  create_redis = local.pubsub_type == "redis"

  # ---------------------------------------------------------------------------
  # Authentication
  # ---------------------------------------------------------------------------
  alb_auth = var.auth_mode == "alb"
  # nonsensitive: only whether a secret was supplied, never its value.
  has_oidc_client_secret = nonsensitive(var.oidc_client_secret != null && var.oidc_client_secret != "")
  oidc_secret_enabled    = var.auth_mode == "oidc" && local.has_oidc_client_secret

  # ---------------------------------------------------------------------------
  # AI
  # ---------------------------------------------------------------------------
  bedrock_region = coalesce(var.bedrock_region, local.region)

  bedrock_image_model = (
    var.bedrock_image_model != null ? var.bedrock_image_model :
    contains(["us-east-1", "eu-west-1", "ap-northeast-1"], local.bedrock_region) ? "amazon.nova-canvas-v1:0" : ""
  )

  ai_key_names = nonsensitive(sort(keys(var.ai_api_keys)))
  has_ai_keys  = length(local.ai_key_names) > 0

  default_ai_config = {
    default_model = "smart"
    providers = {
      # Claude through the Bedrock Messages API endpoint (bedrock-mantle).
      claude = {
        type     = "anthropic"
        platform = "bedrock"
        region   = local.bedrock_region
      }
      # Any other Bedrock model through Converse / InvokeModel.
      bedrock = {
        type   = "bedrock"
        region = local.bedrock_region
      }
    }
    models = merge(
      {
        smart = {
          provider    = "claude"
          model       = var.bedrock_chat_model
          description = "Most capable model, for reasoning, writing and code."
        }
      },
      var.bedrock_fast_model == null ? {} : {
        fast = {
          provider    = "claude"
          model       = var.bedrock_fast_model
          description = "Fast, low-cost model for simple tasks and high volume."
        }
      },
      var.bedrock_embedding_model == null ? {} : {
        embed = {
          provider    = "bedrock"
          model       = var.bedrock_embedding_model
          kind        = "embedding"
          description = "Text embeddings."
        }
      },
      local.bedrock_image_model == "" ? {} : {
        image = {
          provider    = "bedrock"
          model       = local.bedrock_image_model
          kind        = "image"
          description = "Image generation."
        }
      },
    )
  }

  # The `ai:` section body as YAML ("" = no AI). ai_config is decoded and
  # re-encoded so the result is always well-formed and correctly indented.
  ai_yaml = (
    var.ai_config != null ? trimspace(yamlencode(yamldecode(var.ai_config))) :
    var.enable_bedrock ? trimspace(yamlencode(local.default_ai_config)) : ""
  )

  # Sections from config_overrides. The Lambda gateway gets them too (sites,
  # identity and rate limits affect deploys) except warehouse, whose sources
  # may reference secrets that only the ECS tasks receive.
  config_overrides         = try(yamldecode(var.config_overrides), {})
  gateway_config_overrides = { for k, v in local.config_overrides : k => v if k != "warehouse" }

  # ---------------------------------------------------------------------------
  # Rendered backyard.yaml (server) and the Lambda gateway variant
  # ---------------------------------------------------------------------------
  oidc_settings = var.oidc_issuer == null ? null : {
    issuer           = var.oidc_issuer
    client_id        = var.auth_mode == "oidc" && var.oidc_client_id != null ? var.oidc_client_id : ""
    client_secret    = local.oidc_secret_enabled
    scopes           = var.auth_mode == "oidc" ? var.oidc_scopes : []
    use_userinfo     = var.auth_mode == "oidc" && var.oidc_use_userinfo
    device_client_id = var.oidc_device_client_id != null ? var.oidc_device_client_id : ""
  }

  config_vars = {
    display_name    = var.display_name
    domain          = var.domain
    public_url      = local.public_url
    url_mode        = var.url_mode
    trusted_proxies = local.alb_subnet_cidrs
    idle_timeout    = "${var.alb_idle_timeout + 60}s"

    auth_mode       = var.auth_mode
    admins          = var.admins
    admin_groups    = var.admin_groups
    allowed_domains = var.allowed_domains
    allowed_emails  = var.allowed_emails
    allowed_groups  = var.allowed_groups
    static_tokens   = var.static_tokens
    # In none mode an issuer only matters for device-flow bearer tokens, so the
    # oidc block is emitted only when a device client id makes it usable.
    oidc = (
      local.oidc_settings == null ? null :
      var.auth_mode == "oidc" || var.oidc_device_client_id != null ? local.oidc_settings : null
    )
    alb = local.alb_auth ? { region = local.region, signer_arn = aws_lb.this.arn } : null

    bucket     = aws_s3_bucket.this.id
    region     = local.region
    kms_key_id = var.kms_key_arn != null ? var.kms_key_arn : ""

    dynamodb_table = var.database == "dynamodb" ? aws_dynamodb_table.this[0].name : ""
    db_max_conns   = var.db_max_connections_per_task

    allow_anonymous = var.allow_anonymous_deploys

    log_level  = var.log_level
    access_log = var.access_log
  }

  backyard_config_yaml = templatefile("${path.module}/templates/backyard.yaml.tftpl", merge(local.config_vars, {
    gateway     = false
    gateway_url = local.lambda_gateway_url
    database    = var.database
    pubsub      = local.pubsub_type
    ai_yaml     = local.ai_yaml
    overrides   = length(local.config_overrides) == 0 ? "" : trimspace(yamlencode(local.config_overrides))
  }))

  gateway_config_yaml = templatefile("${path.module}/templates/backyard.yaml.tftpl", merge(local.config_vars, {
    gateway     = true
    gateway_url = ""
    database    = "memory"
    pubsub      = "memory"
    ai_yaml     = ""
    overrides   = length(local.gateway_config_overrides) == 0 ? "" : trimspace(yamlencode(local.gateway_config_overrides))
  }))
}

# Warn (without failing) about the most dangerous combination.
check "public_without_authentication" {
  assert {
    condition     = !(var.auth_mode == "none" && !var.internal && contains(var.allowed_cidrs, "0.0.0.0/0"))
    error_message = "auth_mode is \"none\" on an internet-facing ALB open to 0.0.0.0/0: anyone on the internet can use this Backyard and mint deploy tokens. Set auth_mode to oidc or alb, set internal = true, or narrow allowed_cidrs."
  }
}
