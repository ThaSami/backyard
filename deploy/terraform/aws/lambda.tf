# Optional standalone deploy gateway (SPEC §7) on AWS Lambda behind a function
# URL. CLIs deploy through it (advertised as deploy.gateway_url); uploads go
# straight to presigned S3 URLs, so the 6 MB Lambda payload limit only applies
# to gateway responses.
#
# Authentication: the function URL is public (authorization NONE, plus the
# resource-based policy that makes it callable) because the gateway verifies
# Backyard API tokens, static tokens and (with an OIDC issuer) IdP-issued
# device-flow JWTs itself. It never accepts anonymous deploys.
#
# server.secret: the function never receives the secret itself. Its
# environment names the Secrets Manager secret the ECS service also reads
# (BACKYARD_SECRET_ARN), and cmd/backyard-lambda fetches the value at cold
# start with the gateway role, which may call secretsmanager:GetSecretValue on
# that secret only (plus kms:Decrypt through Secrets Manager with
# var.kms_key_arn). BACKYARD_SECRET must not be set as well: the binary refuses
# to start when both are. BACKYARD_SECRET_VERSION_ID is not read by the binary;
# it changes the function configuration whenever the secret gets a new value
# (rotation with -replace=random_password.backyard_secret), so that every warm
# instance is replaced and the next cold start reads the new value.

locals {
  lambda_zip_path = var.lambda_zip_path != null ? var.lambda_zip_path : "${path.module}/../../../dist/backyard-lambda.zip"
  lambda_zip_ok   = fileexists(local.lambda_zip_path)

  lambda_gateway_url = var.enable_lambda_gateway ? trimsuffix(aws_lambda_function_url.gateway[0].function_url, "/") : ""

  gateway_environment = {
    BACKYARD_CONFIG_YAML       = local.gateway_config_yaml
    BACKYARD_SECRET_ARN        = aws_secretsmanager_secret.backyard_secret.arn
    BACKYARD_SECRET_VERSION_ID = aws_secretsmanager_secret_version.backyard_secret.version_id
  }
}

resource "aws_cloudwatch_log_group" "gateway" {
  count = var.enable_lambda_gateway ? 1 : 0

  name              = "/aws/lambda/${var.name}-gateway"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = local.tags
}

resource "aws_lambda_function" "gateway" {
  count = var.enable_lambda_gateway ? 1 : 0

  function_name = "${var.name}-gateway"
  description   = "Backyard deploy gateway"
  role          = aws_iam_role.gateway[0].arn

  filename         = local.lambda_zip_path
  source_code_hash = local.lambda_zip_ok ? filebase64sha256(local.lambda_zip_path) : null
  runtime          = "provided.al2023"
  handler          = "bootstrap"
  architectures    = ["arm64"]

  memory_size                    = var.lambda_memory_size
  timeout                        = var.lambda_timeout
  reserved_concurrent_executions = var.lambda_reserved_concurrency == null ? -1 : var.lambda_reserved_concurrency
  kms_key_arn                    = var.kms_key_arn

  environment {
    variables = local.gateway_environment
  }

  logging_config {
    log_format = "Text"
    log_group  = aws_cloudwatch_log_group.gateway[0].name
  }

  tags = local.tags

  lifecycle {
    precondition {
      condition     = local.lambda_zip_ok
      error_message = "Lambda package not found at lambda_zip_path. Build it with `make lambda` (dist/backyard-lambda.zip), set lambda_zip_path, or set enable_lambda_gateway = false to use the embedded gateway only."
    }
    precondition {
      # Lambda limits the total size of environment variables to 4 KB.
      condition     = nonsensitive(length(jsonencode(local.gateway_environment))) < 4000
      error_message = "The rendered gateway configuration exceeds the 4 KB Lambda environment limit; shorten config_overrides or disable the Lambda gateway."
    }
    precondition {
      # The function URL is reachable from the internet whatever internal and
      # allowed_cidrs say, so anonymous deploys would be open to everyone.
      condition     = !var.allow_anonymous_deploys
      error_message = "allow_anonymous_deploys cannot be combined with the Lambda gateway, whose function URL is public: set enable_lambda_gateway = false so anonymous deploys only reach the embedded gateway behind the ALB."
    }
  }

  depends_on = [aws_iam_role_policy.gateway]
}

resource "aws_lambda_function_url" "gateway" {
  count = var.enable_lambda_gateway ? 1 : 0

  function_name      = aws_lambda_function.gateway[0].function_name
  authorization_type = "NONE"
  invoke_mode        = "BUFFERED"
}

# A function URL with authorization NONE still needs a resource-based policy
# granting public access; only the console and SAM add it automatically. Since
# October 2025 both lambda:InvokeFunctionUrl and lambda:InvokeFunction are
# required (the latter limited to calls through the function URL).
resource "aws_lambda_permission" "gateway_url" {
  count = var.enable_lambda_gateway ? 1 : 0

  statement_id           = "FunctionURLAllowPublicAccess"
  action                 = "lambda:InvokeFunctionUrl"
  function_name          = aws_lambda_function.gateway[0].function_name
  principal              = "*"
  function_url_auth_type = "NONE"
}

resource "aws_lambda_permission" "gateway_invoke" {
  count = var.enable_lambda_gateway ? 1 : 0

  statement_id             = "FunctionURLInvokeAllowPublicAccess"
  action                   = "lambda:InvokeFunction"
  function_name            = aws_lambda_function.gateway[0].function_name
  principal                = "*"
  invoked_via_function_url = true
}
