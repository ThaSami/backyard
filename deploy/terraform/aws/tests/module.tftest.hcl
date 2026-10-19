# Offline tests for the AWS module: `terraform test` (Terraform >= 1.7).
# Every provider is mocked, so no AWS credentials are needed and nothing is
# created. The runs check resource selection (database, pub/sub, auth modes,
# networking) and the rendered backyard.yaml. configcheck/ loads every
# configuration these runs render (ECS service and Lambda gateway) with
# Backyard's own config loader (BACKYARD_TEST_TERRAFORM=1 go test
# ./deploy/terraform/aws/configcheck/), which catches keys the schema no
# longer accepts.

mock_provider "aws" {
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/test"
      user_id    = "AIDATEST"
    }
  }

  mock_data "aws_region" {
    defaults = {
      name   = "us-east-1"
      region = "us-east-1"
    }
  }

  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }

  mock_data "aws_availability_zones" {
    defaults = {
      names = ["us-east-1a", "us-east-1b", "us-east-1c"]
    }
  }

  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }

  # Existing-VPC runs use vpc-0123456789abcdef0.
  mock_data "aws_subnet" {
    defaults = {
      vpc_id     = "vpc-0123456789abcdef0"
      cidr_block = "10.1.0.0/20"
    }
  }

  mock_resource "aws_lb" {
    defaults = {
      arn      = "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/backyard/50dc6c495c0c9188"
      dns_name = "backyard-123.us-east-1.elb.amazonaws.com"
      zone_id  = "Z35SXDOTRQ7X7K"
    }
  }

  mock_resource "aws_lb_target_group" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/backyard-app/73e2d6bc24d8a067"
    }
  }

  mock_resource "aws_lb_listener" {
    defaults = {
      arn = "arn:aws:elasticloadbalancing:us-east-1:123456789012:listener/app/backyard/50dc6c495c0c9188/f2f7dc8efc522ab2"
    }
  }

  mock_resource "aws_iam_role" {
    defaults = {
      arn = "arn:aws:iam::123456789012:role/backyard-test"
    }
  }

  mock_resource "aws_iam_policy" {
    defaults = {
      arn = "arn:aws:iam::123456789012:policy/backyard-deployer"
    }
  }

  mock_resource "aws_acm_certificate" {
    defaults = {
      arn = "arn:aws:acm:us-east-1:123456789012:certificate/0f0f0f0f-0f0f-0f0f-0f0f-0f0f0f0f0f0f"
      domain_validation_options = [
        {
          domain_name           = "backyard.acme.com"
          resource_record_name  = "_abc.backyard.acme.com."
          resource_record_type  = "CNAME"
          resource_record_value = "_def.acm-validations.aws."
        },
        {
          domain_name           = "*.backyard.acme.com"
          resource_record_name  = "_abc.backyard.acme.com."
          resource_record_type  = "CNAME"
          resource_record_value = "_def.acm-validations.aws."
        },
      ]
    }
  }

  mock_resource "aws_acm_certificate_validation" {
    defaults = {
      certificate_arn = "arn:aws:acm:us-east-1:123456789012:certificate/0f0f0f0f-0f0f-0f0f-0f0f-0f0f0f0f0f0f"
    }
  }

  mock_resource "aws_lambda_function_url" {
    defaults = {
      function_url = "https://abcdefghijklmnop.lambda-url.us-east-1.on.aws/"
    }
  }

  mock_resource "aws_s3_bucket" {
    defaults = {
      arn = "arn:aws:s3:::backyard-sites-123456789012-us-east-1"
    }
  }

  mock_resource "aws_dynamodb_table" {
    defaults = {
      arn = "arn:aws:dynamodb:us-east-1:123456789012:table/backyard"
    }
  }

  mock_resource "aws_secretsmanager_secret" {
    defaults = {
      arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:backyard/x-AbCdEf"
    }
  }

  mock_resource "aws_cloudwatch_log_group" {
    defaults = {
      arn = "arn:aws:logs:us-east-1:123456789012:log-group:/ecs/backyard"
    }
  }

  mock_resource "aws_db_instance" {
    defaults = {
      address  = "backyard.abc.us-east-1.rds.amazonaws.com"
      endpoint = "backyard.abc.us-east-1.rds.amazonaws.com:5432"
      port     = 5432
    }
  }

  mock_resource "aws_elasticache_replication_group" {
    defaults = {
      primary_endpoint_address = "master.backyard-pubsub.abc.use1.cache.amazonaws.com"
      port                     = 6379
    }
  }
}

mock_provider "random" {
  mock_resource "random_password" {
    defaults = {
      result = "Abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKL"
    }
  }
}

mock_provider "http" {
  mock_data "http" {
    defaults = {
      status_code   = 200
      response_body = "{\"issuer\":\"https://acme.okta.com/oauth2/default\",\"authorization_endpoint\":\"https://acme.okta.com/oauth2/default/v1/authorize\",\"token_endpoint\":\"https://acme.okta.com/oauth2/default/v1/token\",\"userinfo_endpoint\":\"https://acme.okta.com/oauth2/default/v1/userinfo\"}"
    }
  }
}

variables {
  domain          = "backyard.acme.com"
  route53_zone_id = "Z0123456789ABCDEFGHIJ"
  # Any existing file satisfies the package check; the provider is mocked.
  lambda_zip_path = "versions.tf"
  # auth_mode defaults to none; keep the public_without_authentication check quiet.
  allowed_cidrs = ["198.51.100.0/24"]
}

# -----------------------------------------------------------------------------

run "defaults_dynamodb_with_redis" {
  command = apply

  assert {
    condition     = length(aws_dynamodb_table.this) == 1 && length(aws_db_instance.this) == 0
    error_message = "DynamoDB is the default database."
  }

  assert {
    condition     = length(aws_elasticache_replication_group.this) == 1 && output.pubsub.type == "redis"
    error_message = "More than one task with DynamoDB must use the Valkey pub/sub."
  }

  assert {
    condition     = length(aws_vpc.this) == 1 && length(aws_subnet.private) == 2 && length(aws_nat_gateway.this) == 1
    error_message = "A two-AZ VPC with a single NAT gateway is created by default."
  }

  assert {
    condition     = length(aws_route53_record.alb) == 2 && length(aws_lb_listener_rule.deploy) == 0 && length(aws_lb_listener_rule.mcp) == 0
    error_message = "Apex and wildcard records are created; no ALB auth rules outside alb mode."
  }

  assert {
    condition     = output.gateway_url == "https://abcdefghijklmnop.lambda-url.us-east-1.on.aws"
    error_message = "The Lambda function URL (without trailing slash) is the gateway URL."
  }

  assert {
    condition     = output.cli_login_command == "backyard login --server https://backyard.acme.com --gateway https://abcdefghijklmnop.lambda-url.us-east-1.on.aws"
    error_message = "Unexpected cli_login_command: ${output.cli_login_command}"
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).server.base_domain == "backyard.acme.com" &&
      yamldecode(output.backyard_config_yaml).server.public_url == "https://backyard.acme.com" &&
      yamldecode(output.backyard_config_yaml).server.url_mode == "host" &&
      yamldecode(output.backyard_config_yaml).server.trusted_proxies == ["10.40.0.0/20", "10.40.16.0/20"] &&
      yamldecode(output.backyard_config_yaml).server.idle_timeout == "360s" &&
      !can(yamldecode(output.backyard_config_yaml).server.secret)
    )
    error_message = "Unexpected server section."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).auth.mode == "none" &&
      yamldecode(output.backyard_config_yaml).storage.type == "s3" &&
      yamldecode(output.backyard_config_yaml).storage.s3.region == "us-east-1" &&
      yamldecode(output.backyard_config_yaml).database.type == "dynamodb" &&
      yamldecode(output.backyard_config_yaml).database.dynamodb.table == "backyard" &&
      yamldecode(output.backyard_config_yaml).pubsub.type == "redis" &&
      yamldecode(output.backyard_config_yaml).pubsub.redis.url == "$${REDIS_URL}" &&
      yamldecode(output.backyard_config_yaml).deploy.gateway_url == "https://abcdefghijklmnop.lambda-url.us-east-1.on.aws"
    )
    error_message = "Unexpected auth/storage/database/pubsub/deploy sections."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).ai.default_model == "smart" &&
      yamldecode(output.backyard_config_yaml).ai.providers.claude.type == "anthropic" &&
      yamldecode(output.backyard_config_yaml).ai.providers.claude.platform == "bedrock" &&
      yamldecode(output.backyard_config_yaml).ai.providers.claude.region == "us-east-1" &&
      yamldecode(output.backyard_config_yaml).ai.models.smart.model == "anthropic.claude-opus-5-5" &&
      yamldecode(output.backyard_config_yaml).ai.models.embed.kind == "embedding" &&
      yamldecode(output.backyard_config_yaml).ai.models.image.model == "amazon.nova-canvas-v1:0"
    )
    error_message = "Unexpected default Bedrock AI configuration."
  }

  assert {
    condition = (
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).database.type == "memory" &&
      !can(yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).ai) &&
      !can(yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).deploy) &&
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).storage.s3.bucket == output.bucket
    )
    error_message = "Unexpected Lambda gateway configuration."
  }

  assert {
    condition     = [for s in jsondecode(aws_ecs_task_definition.this.container_definitions)[0].secrets : s.name] == ["BACKYARD_SECRET", "REDIS_URL"]
    error_message = "The container receives BACKYARD_SECRET and REDIS_URL as ECS secrets."
  }

  assert {
    condition = (
      sort(keys(aws_lambda_function.gateway[0].environment[0].variables)) == tolist(["BACKYARD_CONFIG_YAML", "BACKYARD_SECRET_ARN", "BACKYARD_SECRET_VERSION_ID"]) &&
      aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_SECRET_ARN == aws_secretsmanager_secret.backyard_secret.arn &&
      aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_SECRET_VERSION_ID == aws_secretsmanager_secret_version.backyard_secret.version_id &&
      !strcontains(jsonencode(aws_lambda_function.gateway[0].environment[0].variables), nonsensitive(random_password.backyard_secret.result)) &&
      !can(yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).server.secret)
    )
    error_message = "The Lambda gateway gets the secret's ARN (BACKYARD_SECRET_ARN), never the secret itself (BACKYARD_SECRET or server.secret: cmd/backyard-lambda refuses both at once)."
  }

  assert {
    condition = (
      sort(flatten([for st in data.aws_iam_policy_document.gateway[0].statement : st.resources if st.sid == "ServerSecret"])) == tolist([aws_secretsmanager_secret.backyard_secret.arn]) &&
      sort(flatten([for st in data.aws_iam_policy_document.gateway[0].statement : st.actions if st.sid == "ServerSecret"])) == tolist(["secretsmanager:GetSecretValue"]) &&
      length([for st in data.aws_iam_policy_document.gateway[0].statement : st if st.sid == "ServerSecretKms"]) == 0
    )
    error_message = "The gateway role may read the server secret (and needs no KMS grant without kms_key_arn)."
  }

  assert {
    # The headers a presigned S3 upload may carry (internal/blob/s3 PresignPut).
    condition = alltrue([
      for h in ["content-type", "content-md5", "x-amz-checksum-sha256", "x-amz-sdk-checksum-algorithm", "x-amz-server-side-encryption", "x-amz-server-side-encryption-aws-kms-key-id"] :
      contains(one(aws_s3_bucket_cors_configuration.this.cors_rule).allowed_headers, "*") ||
      contains([for a in one(aws_s3_bucket_cors_configuration.this.cors_rule).allowed_headers : lower(a)], h)
    ]) && contains(one(aws_s3_bucket_cors_configuration.this.cors_rule).allowed_methods, "PUT")
    error_message = "The bucket CORS rule must allow presigned PUTs with their signed headers."
  }

  assert {
    condition     = length(output.certificate_validation_records) == 0 && length(aws_acm_certificate.this) == 1 && length(aws_route53_record.certificate_validation) == 2
    error_message = "With a Route 53 zone the certificate is validated automatically."
  }

  assert {
    condition = (
      aws_lambda_function_url.gateway[0].authorization_type == "NONE" &&
      aws_lambda_permission.gateway_url[0].action == "lambda:InvokeFunctionUrl" &&
      aws_lambda_permission.gateway_url[0].principal == "*" &&
      aws_lambda_permission.gateway_url[0].function_url_auth_type == "NONE" &&
      aws_lambda_permission.gateway_invoke[0].action == "lambda:InvokeFunction" &&
      aws_lambda_permission.gateway_invoke[0].principal == "*" &&
      aws_lambda_permission.gateway_invoke[0].invoked_via_function_url
    )
    error_message = "The public function URL needs both resource-based policy statements (InvokeFunctionUrl and InvokeFunction via the URL)."
  }

  assert {
    condition = (
      length(aws_lb_listener_rule.reject_forwarded_host) == 1 &&
      aws_lb_listener_rule.reject_forwarded_host[0].priority == 1 &&
      one(aws_lb_listener_rule.reject_forwarded_host[0].condition[*].http_header[0].http_header_name) == "X-Forwarded-Host" &&
      aws_lb_listener_rule.reject_forwarded_host[0].action[0].fixed_response[0].status_code == "400"
    )
    error_message = "Client-supplied X-Forwarded-Host headers are rejected before any other rule."
  }

  assert {
    condition = (
      contains(flatten([for st in data.aws_iam_policy_document.task.statement : st.resources if st.sid == "BedrockMessagesApi"]), "arn:aws:bedrock-mantle:*:123456789012:project/*") &&
      length(flatten([for st in data.aws_iam_policy_document.task.statement : st.resources if st.sid == "BedrockMessagesApi"])) == 1
    )
    error_message = "bedrock-mantle:CreateInference is limited to this account's projects."
  }

  assert {
    condition     = jsondecode(aws_ecs_task_definition.this.container_definitions)[0].environment[0].name == "AWS_REGION" && length(jsondecode(aws_ecs_task_definition.this.container_definitions)[0].environment) == 2
    error_message = "Only BACKYARD_CONFIG_YAML and AWS_REGION are plain environment variables."
  }
}

run "single_task_memory_pubsub" {
  command = apply

  variables {
    desired_count                = 1
    enable_autoscaling           = false
    enable_lambda_gateway        = false
    enable_bedrock               = false
    reject_forwarded_host_header = false
  }

  assert {
    condition     = length(aws_elasticache_replication_group.this) == 0 && output.pubsub.type == "memory"
    error_message = "A single task uses in-memory pub/sub."
  }

  assert {
    condition     = length(aws_appautoscaling_policy.cpu) == 0 && aws_appautoscaling_target.this.max_capacity == 1
    error_message = "Without autoscaling the service has a fixed size."
  }

  assert {
    condition     = !can(yamldecode(output.backyard_config_yaml).ai) && !can(yamldecode(output.backyard_config_yaml).deploy)
    error_message = "No AI section without Bedrock or ai_config; no deploy section without Lambda gateway."
  }

  assert {
    condition     = output.gateway_url == "https://backyard.acme.com/__backyard/api/deploy" && output.cli_login_command == "backyard login --server https://backyard.acme.com"
    error_message = "The embedded gateway is used without the Lambda gateway."
  }

  assert {
    condition     = length(aws_lambda_function.gateway) == 0 && length(aws_lambda_permission.gateway_url) == 0 && length(aws_lambda_permission.gateway_invoke) == 0
    error_message = "No Lambda resources without the Lambda gateway."
  }

  assert {
    condition     = length(aws_lb_listener_rule.reject_forwarded_host) == 0
    error_message = "The X-Forwarded-Host rule can be turned off."
  }
}

run "postgres" {
  command = apply

  variables {
    database = "postgres"
  }

  assert {
    condition     = length(aws_db_instance.this) == 1 && length(aws_dynamodb_table.this) == 0 && length(aws_elasticache_replication_group.this) == 0
    error_message = "Postgres replaces DynamoDB and the Valkey cluster."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).database.type == "postgres" &&
      yamldecode(output.backyard_config_yaml).database.postgres.dsn == "$${DATABASE_URL}" &&
      yamldecode(output.backyard_config_yaml).database.postgres.max_conns == 10 &&
      yamldecode(output.backyard_config_yaml).pubsub.type == "postgres" &&
      yamldecode(output.backyard_config_yaml).pubsub.postgres.dsn == "$${DATABASE_URL}"
    )
    error_message = "Postgres database and LISTEN/NOTIFY pub/sub read DATABASE_URL."
  }

  assert {
    condition     = aws_secretsmanager_secret_version.database_url[0].secret_string == "postgres://backyard:${random_password.db[0].result}@backyard.abc.us-east-1.rds.amazonaws.com:5432/backyard?sslmode=require"
    error_message = "DATABASE_URL is built from the RDS endpoint."
  }

  assert {
    condition     = contains([for s in jsondecode(aws_ecs_task_definition.this.container_definitions)[0].secrets : s.name], "DATABASE_URL")
    error_message = "DATABASE_URL is injected as an ECS secret."
  }

  assert {
    condition     = aws_db_instance.this[0].storage_encrypted && !aws_db_instance.this[0].publicly_accessible && aws_db_instance.this[0].deletion_protection
    error_message = "RDS is encrypted, private and protected."
  }
}

run "oidc_mode" {
  command = apply

  variables {
    auth_mode             = "oidc"
    oidc_issuer           = "https://acme.okta.com/oauth2/default"
    oidc_client_id        = "0oa-web"
    oidc_client_secret    = "s3cr3t"
    oidc_device_client_id = "0oa-cli"
    oidc_scopes           = ["openid", "profile", "email", "groups"]
    admin_groups          = ["backyard-admins"]
    allowed_domains       = ["acme.com"]
    allowed_emails        = ["pat@contractor.example"]
    static_tokens         = [{ name = "ci", sha256 = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", admin = true }]
  }

  assert {
    condition     = length(aws_secretsmanager_secret.oidc_client_secret) == 1
    error_message = "The client secret is stored in Secrets Manager."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).auth.mode == "oidc" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.issuer == "https://acme.okta.com/oauth2/default" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.client_id == "0oa-web" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.client_secret == "$${OIDC_CLIENT_SECRET}" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.device_client_id == "0oa-cli" &&
      yamldecode(output.backyard_config_yaml).auth.oidc.scopes == ["openid", "profile", "email", "groups"] &&
      yamldecode(output.backyard_config_yaml).auth.admin_groups == ["backyard-admins"] &&
      yamldecode(output.backyard_config_yaml).auth.allowed_domains == ["acme.com"] &&
      yamldecode(output.backyard_config_yaml).auth.allowed_emails == ["pat@contractor.example"] &&
      yamldecode(output.backyard_config_yaml).auth.tokens[0].name == "ci" &&
      yamldecode(output.backyard_config_yaml).auth.tokens[0].admin == true
    )
    error_message = "Unexpected oidc auth section."
  }

  assert {
    condition = (
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).auth.mode == "oidc" &&
      !can(yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).auth.oidc.client_secret) &&
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).auth.oidc.device_client_id == "0oa-cli"
    )
    error_message = "The Lambda gateway verifies IdP tokens without the client secret."
  }

  assert {
    condition     = output.oidc_redirect_uris == tolist(["https://backyard.acme.com/__backyard/auth/callback"])
    error_message = "Unexpected redirect URIs."
  }
}

run "alb_mode" {
  command = apply

  variables {
    auth_mode          = "alb"
    oidc_issuer        = "https://acme.okta.com/oauth2/default"
    oidc_client_id     = "0oa-web"
    oidc_client_secret = "s3cr3t"
  }

  assert {
    condition     = length(aws_lb_listener_rule.public) == 1 && length(aws_lb_listener_rule.deploy) == 1
    error_message = "ALB mode adds the unauthenticated probe rule and the deploy rule."
  }

  assert {
    condition     = aws_lb_listener.https.default_action[0].type == "authenticate-oidc" && aws_lb_listener.https.default_action[0].authenticate_oidc[0].token_endpoint == "https://acme.okta.com/oauth2/default/v1/token"
    error_message = "The HTTPS listener authenticates with the discovered OIDC endpoints."
  }

  assert {
    condition     = aws_lb_listener_rule.deploy[0].action[0].authenticate_oidc[0].on_unauthenticated_request == "allow" && aws_lb_listener_rule.public[0].action[0].type == "forward"
    error_message = "Deploy paths pass unauthenticated requests through; probes are never authenticated."
  }

  assert {
    # `backyard token create` posts to the token API with a Backyard bearer
    # token and has no ALB session cookie.
    condition = (
      contains(one(aws_lb_listener_rule.deploy[0].condition).path_pattern[0].values, "/__backyard/api/tokens") &&
      contains(one(aws_lb_listener_rule.deploy[0].condition).path_pattern[0].values, "/__backyard/api/deploy/*") &&
      length(one(aws_lb_listener_rule.deploy[0].condition).path_pattern[0].values) <= 5
    )
    error_message = "The pass-through rule must cover the deploy gateway and POST /__backyard/api/tokens (at most 5 path values per ALB rule)."
  }

  assert {
    # The Claude connector's machine paths (SPEC §8.4): Claude calls them
    # without an ALB session, and they ship whether or not mcp is enabled.
    condition = (
      length(aws_lb_listener_rule.mcp) == 1 &&
      aws_lb_listener_rule.mcp[0].priority == 15 &&
      toset(one(aws_lb_listener_rule.mcp[0].condition).path_pattern[0].values) == toset(["/__backyard/mcp", "/__backyard/oauth/*", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"]) &&
      length(one(aws_lb_listener_rule.mcp[0].condition).path_pattern[0].values) == 4 &&
      length(aws_lb_listener_rule.mcp[0].condition) == 1 &&
      length(aws_lb_listener_rule.mcp[0].action) == 1 &&
      aws_lb_listener_rule.mcp[0].action[0].type == "forward" &&
      aws_lb_listener_rule.mcp[0].action[0].target_group_arn == aws_lb_target_group.app.arn &&
      length(aws_lb_listener_rule.mcp[0].action[0].authenticate_oidc) == 0
    )
    error_message = "ALB mode forwards exactly the four connector machine paths without authenticate-oidc (rule mcp, priority 15, between public and deploy)."
  }

  assert {
    condition     = aws_lb_listener_rule.public[0].priority < aws_lb_listener_rule.mcp[0].priority && aws_lb_listener_rule.mcp[0].priority < aws_lb_listener_rule.deploy[0].priority
    error_message = "Rule order: public (10), mcp (15), deploy (20)."
  }

  assert {
    condition     = !contains(flatten([for r in [aws_lb_listener_rule.public[0], aws_lb_listener_rule.mcp[0], aws_lb_listener_rule.deploy[0]] : one(r.condition).path_pattern[0].values]), "/__backyard/auth/authorize")
    error_message = "/__backyard/auth/authorize stays behind the ALB login (the connector's sign-in)."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).auth.mode == "alb" &&
      yamldecode(output.backyard_config_yaml).auth.alb.region == "us-east-1" &&
      yamldecode(output.backyard_config_yaml).auth.alb.signer_arn == "arn:aws:elasticloadbalancing:us-east-1:123456789012:loadbalancer/app/backyard/50dc6c495c0c9188" &&
      !can(yamldecode(output.backyard_config_yaml).auth.oidc) &&
      length(aws_secretsmanager_secret.oidc_client_secret) == 0
    )
    error_message = "Unexpected alb auth section."
  }

  assert {
    condition     = length(aws_vpc_security_group_egress_rule.alb_to_idp) == 1
    error_message = "The ALB may reach the IdP in alb mode."
  }

  assert {
    condition     = output.oidc_redirect_uris == tolist(["https://backyard.acme.com/oauth2/idpresponse", "https://*.backyard.acme.com/oauth2/idpresponse"])
    error_message = "Unexpected redirect URIs."
  }
}

run "path_mode_without_zone_existing_vpc" {
  command = apply

  variables {
    url_mode           = "path"
    route53_zone_id    = null
    vpc_id             = "vpc-0123456789abcdef0"
    public_subnet_ids  = ["subnet-a", "subnet-b"]
    private_subnet_ids = ["subnet-c", "subnet-d"]
    internal           = true
  }

  override_data {
    target = data.aws_subnet.alb["subnet-c"]
    values = { vpc_id = "vpc-0123456789abcdef0", cidr_block = "10.1.128.0/20" }
  }

  override_data {
    target = data.aws_subnet.alb["subnet-d"]
    values = { vpc_id = "vpc-0123456789abcdef0", cidr_block = "10.1.144.0/20" }
  }

  assert {
    condition     = length(aws_vpc.this) == 0 && length(aws_nat_gateway.this) == 0 && length(aws_route53_record.alb) == 0
    error_message = "No VPC, NAT or Route 53 records are created."
  }

  assert {
    condition     = aws_lb.this.internal && tolist(aws_lb.this.subnets) == tolist(["subnet-c", "subnet-d"])
    error_message = "An internal ALB is placed in the private subnets."
  }

  assert {
    condition     = length(aws_acm_certificate.this[0].subject_alternative_names) == 0
    error_message = "Path mode needs no wildcard certificate."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).server.url_mode == "path" &&
      yamldecode(output.backyard_config_yaml).server.trusted_proxies == ["10.1.128.0/20", "10.1.144.0/20"]
    )
    error_message = "Path mode; only the (internal) ALB's subnets are trusted proxies."
  }

  assert {
    condition     = jsonencode(output.dns_records) == jsonencode([{ name = "backyard.acme.com", type = "CNAME", value = "backyard-123.us-east-1.elb.amazonaws.com" }])
    error_message = "Only the apex record is needed in path mode."
  }

  assert {
    condition     = length(output.certificate_validation_records) == 1 && output.certificate_validation_records[0].name == "_abc.backyard.acme.com." && length(aws_route53_record.certificate_validation) == 0
    error_message = "Without a zone the (deduplicated) validation records are exported instead of created."
  }
}

run "custom_ai_and_overrides" {
  command = apply

  variables {
    ai_api_keys      = { ANTHROPIC_API_KEY = "sk-ant-test", OPENAI_API_KEY = "sk-test" }
    ai_config        = <<-EOT
      default_model: smart
      providers:
        anthropic:
          type: anthropic
          api_key: "$${ANTHROPIC_API_KEY}"
      models:
        smart:
          provider: anthropic
          model: claude-opus-5-5
      max_tokens: 16000
    EOT
    config_overrides = <<-EOT
      sites:
        ownership: owner
      warehouse:
        sources:
          default:
            type: postgres
            dsn: "$${WAREHOUSE_DSN}"
    EOT
    extra_secrets    = { WAREHOUSE_DSN = "arn:aws:secretsmanager:us-east-1:123456789012:secret:warehouse-AbCdEf:dsn::" }
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).ai.providers.anthropic.api_key == "$${ANTHROPIC_API_KEY}" &&
      yamldecode(output.backyard_config_yaml).ai.max_tokens == 16000 &&
      !can(yamldecode(output.backyard_config_yaml).ai.providers.claude) &&
      yamldecode(output.backyard_config_yaml).sites.ownership == "owner" &&
      yamldecode(output.backyard_config_yaml).warehouse.sources.default.dsn == "$${WAREHOUSE_DSN}"
    )
    error_message = "ai_config and config_overrides are rendered verbatim."
  }

  assert {
    condition = (
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).sites.ownership == "owner" &&
      !can(yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).warehouse)
    )
    error_message = "The gateway gets the sites overrides but not the warehouse."
  }

  assert {
    condition = [for s in jsondecode(aws_ecs_task_definition.this.container_definitions)[0].secrets : s] == [
      { name = "BACKYARD_SECRET", valueFrom = "arn:aws:secretsmanager:us-east-1:123456789012:secret:backyard/x-AbCdEf" },
      { name = "REDIS_URL", valueFrom = "arn:aws:secretsmanager:us-east-1:123456789012:secret:backyard/x-AbCdEf" },
      { name = "ANTHROPIC_API_KEY", valueFrom = "arn:aws:secretsmanager:us-east-1:123456789012:secret:backyard/x-AbCdEf:ANTHROPIC_API_KEY::" },
      { name = "OPENAI_API_KEY", valueFrom = "arn:aws:secretsmanager:us-east-1:123456789012:secret:backyard/x-AbCdEf:OPENAI_API_KEY::" },
      { name = "WAREHOUSE_DSN", valueFrom = "arn:aws:secretsmanager:us-east-1:123456789012:secret:warehouse-AbCdEf:dsn::" },
    ]
    error_message = "AI keys and extra secrets are injected as ECS secrets."
  }

  assert {
    condition     = contains(local.extra_secretsmanager_arns, "arn:aws:secretsmanager:us-east-1:123456789012:secret:warehouse-AbCdEf")
    error_message = "The execution role may read the extra secret (without the JSON key suffix)."
  }
}

run "hardened_options" {
  command = apply

  variables {
    image                   = "123456789012.dkr.ecr.us-east-1.amazonaws.com/platform/backyard:v0.1.0"
    image_pull_secret_arn   = "arn:aws:secretsmanager:us-east-1:123456789012:secret:registry-AbCdEf"
    container_command       = ["serve"]
    kms_key_arn             = "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"
    use_fargate_spot        = true
    cpu_architecture        = "ARM64"
    enable_execute_command  = true
    tasks_in_public_subnets = true
    single_nat_gateway      = false
    az_count                = 3
    waf_web_acl_arn         = "arn:aws:wafv2:us-east-1:123456789012:regional/webacl/backyard/abc"
    bedrock_image_model     = ""
    bedrock_fast_model      = null
  }

  assert {
    condition     = length(aws_nat_gateway.this) == 0 && length(aws_subnet.public) == 3 && aws_ecs_service.this.network_configuration[0].assign_public_ip
    error_message = "Tasks in public subnets need no NAT gateway and get public IPs."
  }

  assert {
    condition     = local.ecr_repository_arn == "arn:aws:ecr:us-east-1:123456789012:repository/platform/backyard"
    error_message = "The ECR repository of the image is detected for pull permissions."
  }

  assert {
    condition = (
      jsondecode(aws_ecs_task_definition.this.container_definitions)[0].command == ["serve"] &&
      jsondecode(aws_ecs_task_definition.this.container_definitions)[0].repositoryCredentials.credentialsParameter == "arn:aws:secretsmanager:us-east-1:123456789012:secret:registry-AbCdEf" &&
      aws_ecs_task_definition.this.runtime_platform[0].cpu_architecture == "ARM64"
    )
    error_message = "Command, registry credentials and architecture are applied."
  }

  assert {
    condition     = sort([for c in aws_ecs_service.this.capacity_provider_strategy : "${c.capacity_provider}:${c.base}:${c.weight}"]) == tolist(["FARGATE:1:0", "FARGATE_SPOT:0:1"])
    error_message = "Fargate Spot is used beyond the first task."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).storage.s3.sse_kms_key_id == "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555" &&
      !can(yamldecode(output.backyard_config_yaml).ai.models.image) &&
      !can(yamldecode(output.backyard_config_yaml).ai.models.fast) &&
      aws_dynamodb_table.this[0].server_side_encryption[0].kms_key_arn == "arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"
    )
    error_message = "The customer-managed key is used for S3 writes and DynamoDB; disabled aliases are omitted."
  }

  assert {
    condition     = length(aws_wafv2_web_acl_association.this) == 1
    error_message = "The WAF web ACL is associated with the ALB."
  }

  assert {
    condition = (
      sort(flatten([for st in data.aws_iam_policy_document.gateway[0].statement : st.resources if st.sid == "ServerSecretKms"])) == tolist(["arn:aws:kms:us-east-1:123456789012:key/11111111-2222-3333-4444-555555555555"]) &&
      sort(flatten([for st in data.aws_iam_policy_document.gateway[0].statement : st.actions if st.sid == "ServerSecretKms"])) == tolist(["kms:Decrypt"]) &&
      sort(flatten([for st in data.aws_iam_policy_document.gateway[0].statement : [for c in st.condition : c.values if c.variable == "kms:ViaService"] if st.sid == "ServerSecretKms"])) == tolist(["secretsmanager.us-east-1.amazonaws.com"])
    )
    error_message = "With a customer-managed key the gateway role may decrypt the server secret through Secrets Manager."
  }
}

# -----------------------------------------------------------------------------
# Misconfigurations are rejected at plan time.
# -----------------------------------------------------------------------------

run "alb_mode_requires_client_secret" {
  command = plan

  variables {
    auth_mode      = "alb"
    oidc_issuer    = "https://acme.okta.com/oauth2/default"
    oidc_client_id = "0oa-web"
  }

  expect_failures = [aws_lb.this]
}

run "memory_pubsub_rejected_with_many_tasks" {
  command = plan

  variables {
    pubsub = "memory"
  }

  expect_failures = [aws_ecs_task_definition.this]
}

run "ai_config_rejects_wrapped_section" {
  command = plan

  variables {
    ai_config = "ai:\n  default_model: smart\n"
  }

  expect_failures = [var.ai_config]
}

run "config_overrides_rejects_managed_sections" {
  command = plan

  variables {
    config_overrides = "auth:\n  mode: none\n"
  }

  expect_failures = [var.config_overrides]
}

run "warns_when_public_without_authentication" {
  command = plan

  variables {
    allowed_cidrs = ["0.0.0.0/0"]
  }

  expect_failures = [check.public_without_authentication]
}

run "rejects_invalid_inputs" {
  command = plan

  variables {
    domain        = "https://Backyard.acme.com/"
    vpc_cidr      = "10.0.0.0"
    allowed_cidrs = ["::/0"]
    static_tokens = [{ name = "ci", sha256 = "not-a-hash" }]
    extra_secrets = { DATABASE_URL = "arn:aws:secretsmanager:us-east-1:123456789012:secret:x-AbCdEf" }
  }

  expect_failures = [var.domain, var.vpc_cidr, var.allowed_cidrs, var.static_tokens, var.extra_secrets]
}

run "rejects_secret_values_that_break_the_config" {
  command = plan

  variables {
    auth_mode          = "oidc"
    oidc_issuer        = "https://acme.okta.com/oauth2/default"
    oidc_client_id     = "0oa-web"
    oidc_client_secret = "abc\"def"
    ai_api_keys        = { ANTHROPIC_API_KEY = "line1\nline2" }
    oidc_endpoints = {
      authorization_endpoint = "http://acme.okta.com/oauth2/default/v1/authorize"
      token_endpoint         = "https://acme.okta.com/oauth2/default/v1/token"
      user_info_endpoint     = "https://acme.okta.com/oauth2/default/v1/userinfo"
    }
    db_max_connections_per_task = 1
  }

  expect_failures = [var.oidc_client_secret, var.ai_api_keys, var.oidc_endpoints, var.db_max_connections_per_task]
}

run "rejects_anonymous_deploys_with_public_lambda_gateway" {
  command = plan

  variables {
    allow_anonymous_deploys = true
  }

  expect_failures = [aws_lambda_function.gateway]
}

run "anonymous_deploys_through_the_alb_only" {
  command = apply

  variables {
    allow_anonymous_deploys = true
    enable_lambda_gateway   = false
    internal                = true
  }

  assert {
    condition     = yamldecode(output.backyard_config_yaml).deploy.allow_anonymous == true && length(aws_lambda_function.gateway) == 0
    error_message = "Anonymous deploys are allowed through the embedded gateway only."
  }
}

# Mocked data sources are only read (and their postconditions checked) on apply.
run "rejects_existing_subnets_outside_the_vpc" {
  command = apply

  variables {
    vpc_id             = "vpc-0123456789abcdef0"
    public_subnet_ids  = ["subnet-a", "subnet-b"]
    private_subnet_ids = ["subnet-c", "subnet-d"]
  }

  override_data {
    target = data.aws_subnet.alb["subnet-b"]
    values = { vpc_id = "vpc-0fedcba9876543210", cidr_block = "10.9.0.0/20" }
  }

  expect_failures = [data.aws_subnet.alb]
}

run "alb_mode_with_device_flow_and_explicit_endpoints" {
  command = apply

  variables {
    auth_mode             = "alb"
    oidc_issuer           = "https://acme.okta.com/oauth2/default"
    oidc_client_id        = "0oa-web"
    oidc_client_secret    = "s3cr3t"
    oidc_device_client_id = "0oa-cli"
    oidc_endpoints = {
      authorization_endpoint = "https://login.acme.com/authorize"
      token_endpoint         = "https://login.acme.com/token"
      user_info_endpoint     = "https://login.acme.com/userinfo"
    }
  }

  assert {
    condition = (
      length(data.http.oidc_discovery) == 0 &&
      aws_lb_listener.https.default_action[0].authenticate_oidc[0].authorization_endpoint == "https://login.acme.com/authorize" &&
      aws_lb_listener_rule.deploy[0].action[0].authenticate_oidc[0].token_endpoint == "https://login.acme.com/token"
    )
    error_message = "Explicit endpoints skip OIDC discovery."
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).auth.mode == "alb" &&
      yamldecode(output.backyard_config_yaml).auth.oidc == { issuer = "https://acme.okta.com/oauth2/default", device_client_id = "0oa-cli" } &&
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).auth.oidc == { issuer = "https://acme.okta.com/oauth2/default", device_client_id = "0oa-cli" } &&
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).auth.alb.signer_arn == aws_lb.this.arn
    )
    error_message = "In ALB mode the server and the Lambda gateway accept device-flow JWTs (issuer + device client id only; the web client id and secret stay with the ALB)."
  }
}

run "alb_mode_with_the_claude_connector" {
  command = apply

  variables {
    auth_mode          = "alb"
    oidc_issuer        = "https://acme.okta.com/oauth2/default"
    oidc_client_id     = "0oa-web"
    oidc_client_secret = "s3cr3t"
    config_overrides   = <<-EOT
      mcp:
        enabled: true
      sites:
        ownership: owner
    EOT
  }

  assert {
    condition     = yamldecode(output.backyard_config_yaml).mcp.enabled == true && yamldecode(output.backyard_config_yaml).server.public_url == "https://backyard.acme.com"
    error_message = "config_overrides accepts mcp; the server gets it (with an https public_url without a path)."
  }

  assert {
    condition = (
      !can(yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).mcp) &&
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).sites.ownership == "owner"
    )
    error_message = "The Lambda gateway gets the sites overrides but not mcp (the connector is served by the hub only)."
  }

  assert {
    condition     = length(aws_lb_listener_rule.mcp) == 1
    error_message = "The connector's machine paths bypass the ALB login."
  }
}

# Owners-only site listing: a sites section with listing alone (ownership is
# implied); the Lambda gateway serves GET /sites, so it gets the key too.
# configcheck loads both configs and checks the implied ownership.
run "owners_only_site_listing" {
  command = apply

  variables {
    auth_mode          = "oidc"
    oidc_issuer        = "https://acme.okta.com/oauth2/default"
    oidc_client_id     = "0oa-web"
    oidc_client_secret = "s3cr3t"
    admins             = ["platform@acme.com"]
    config_overrides   = <<-EOT
      sites:
        listing: owners
    EOT
  }

  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).sites == { listing = "owners" } &&
      yamldecode(output.backyard_config_yaml).auth.admins == ["platform@acme.com"]
    )
    error_message = "config_overrides passes sites.listing to the ECS service as it is."
  }

  assert {
    condition = (
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).sites == { listing = "owners" } &&
      yamldecode(aws_lambda_function.gateway[0].environment[0].variables.BACKYARD_CONFIG_YAML).auth.admins == ["platform@acme.com"]
    )
    error_message = "The Lambda gateway gets sites.listing and the admins, so its GET /sites lists like the hub's."
  }
}

run "rejects_invalid_fargate_size" {
  command = plan

  variables {
    cpu    = 256
    memory = 4096
  }

  expect_failures = [aws_ecs_task_definition.this]
}

run "rejects_fargate_memory_above_the_cpu_range" {
  command = plan

  variables {
    cpu    = 4096
    memory = 32768
  }

  expect_failures = [aws_ecs_task_definition.this]
}

run "rejects_fargate_memory_off_the_increment" {
  command = plan

  variables {
    cpu    = 512
    memory = 1536
  }

  expect_failures = [aws_ecs_task_definition.this]
}

run "accepts_the_largest_fargate_size" {
  command = plan

  variables {
    cpu    = 16384
    memory = 122880
  }

  assert {
    condition     = aws_ecs_task_definition.this.cpu == "16384" && aws_ecs_task_definition.this.memory == "122880"
    error_message = "16 vCPU / 120 GiB is a valid Fargate size."
  }
}
