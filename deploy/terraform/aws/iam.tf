# IAM: least-privilege roles for the ECS tasks (application permissions),
# the ECS task execution (image pull, logs, secrets), the Lambda gateway and
# an optional policy for direct `aws s3 sync` deploys.

locals {
  bucket_arn = aws_s3_bucket.this.arn

  bedrock_model_arns = var.bedrock_model_arns != null ? var.bedrock_model_arns : [
    "arn:${local.partition}:bedrock:*::foundation-model/*",
    "arn:${local.partition}:bedrock:*:${local.account_id}:inference-profile/*",
    "arn:${local.partition}:bedrock:*:${local.account_id}:application-inference-profile/*",
  ]
  # Claude through the Bedrock Messages API endpoint (bedrock-mantle) authorises
  # bedrock-mantle:CreateInference against a project
  # (arn:<partition>:bedrock-mantle:<region>:<account>:project/<id>; requests
  # without an explicit project use the account's default project). Only this
  # account's projects are allowed.
  bedrock_mantle_arns = ["arn:${local.partition}:bedrock-mantle:*:${local.account_id}:project/*"]

  # <account>.dkr.ecr.<region>.amazonaws.com/<repository>[:tag|@digest]
  ecr_image = regex("^(?:(?P<account>[0-9]{12})\\.dkr\\.ecr(?:-fips)?\\.(?P<region>[a-z0-9-]+)\\.amazonaws\\.com(?:\\.cn)?/(?P<repository>[^:@]+))?", var.image)
  ecr_repository_arn = (
    local.ecr_image.account == null ? null :
    "arn:${local.partition}:ecr:${local.ecr_image.region}:${local.ecr_image.account}:repository/${local.ecr_image.repository}"
  )
}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "task_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }

    # Confused-deputy protection (recommended for ECS task roles).
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [local.account_id]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = ["arn:${local.partition}:ecs:${local.region}:${local.account_id}:*"]
    }
  }
}

# -----------------------------------------------------------------------------
# Task role (what Backyard itself may do)
# -----------------------------------------------------------------------------

data "aws_iam_policy_document" "storage_access" {
  statement {
    sid       = "BucketList"
    actions   = ["s3:ListBucket", "s3:GetBucketLocation", "s3:ListBucketMultipartUploads"]
    resources = [local.bucket_arn]
  }

  statement {
    sid = "BucketObjects"
    actions = [
      "s3:GetObject",
      "s3:PutObject",
      "s3:DeleteObject",
      "s3:AbortMultipartUpload",
      "s3:ListMultipartUploadParts",
    ]
    resources = ["${local.bucket_arn}/*"]
  }

  dynamic "statement" {
    for_each = var.kms_key_arn == null ? [] : [var.kms_key_arn]
    content {
      sid       = "Kms"
      actions   = ["kms:Decrypt", "kms:Encrypt", "kms:GenerateDataKey", "kms:DescribeKey"]
      resources = [statement.value]
    }
  }
}

data "aws_iam_policy_document" "task" {
  source_policy_documents = [data.aws_iam_policy_document.storage_access.json]

  dynamic "statement" {
    for_each = aws_dynamodb_table.this[*].arn
    content {
      sid = "DocumentStore"
      actions = [
        "dynamodb:DescribeTable",
        "dynamodb:GetItem",
        "dynamodb:BatchGetItem",
        "dynamodb:Query",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem",
        "dynamodb:DeleteItem",
        "dynamodb:BatchWriteItem",
        "dynamodb:ConditionCheckItem",
      ]
      resources = [statement.value, "${statement.value}/index/*"]
    }
  }

  dynamic "statement" {
    for_each = var.enable_bedrock ? [1] : []
    content {
      sid = "BedrockRuntime"
      # Converse and ConverseStream are authorised by these two actions.
      actions   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
      resources = local.bedrock_model_arns
    }
  }

  dynamic "statement" {
    for_each = var.enable_bedrock ? [1] : []
    content {
      sid       = "BedrockMessagesApi"
      actions   = ["bedrock-mantle:CreateInference"]
      resources = local.bedrock_mantle_arns
    }
  }

  # First use of a third-party model in the account subscribes to it through
  # AWS Marketplace; only allowed when called via Bedrock.
  dynamic "statement" {
    for_each = var.enable_bedrock && var.bedrock_allow_marketplace_subscribe ? [1] : []
    content {
      sid       = "BedrockModelEnablement"
      actions   = ["aws-marketplace:ViewSubscriptions", "aws-marketplace:Subscribe"]
      resources = ["*"]

      condition {
        test     = "StringEquals"
        variable = "aws:CalledViaLast"
        values   = ["bedrock-mantle.amazonaws.com", "bedrock.amazonaws.com"]
      }
    }
  }

  dynamic "statement" {
    for_each = var.enable_execute_command ? [1] : []
    content {
      sid = "EcsExec"
      actions = [
        "ssmmessages:CreateControlChannel",
        "ssmmessages:CreateDataChannel",
        "ssmmessages:OpenControlChannel",
        "ssmmessages:OpenDataChannel",
      ]
      resources = ["*"]
    }
  }
}

resource "aws_iam_role" "task" {
  name                 = "${var.name}-task"
  description          = "Backyard ECS task role"
  assume_role_policy   = data.aws_iam_policy_document.task_assume.json
  max_session_duration = 3600

  tags = local.tags
}

resource "aws_iam_role_policy" "task" {
  name   = "backyard"
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task.json
}

resource "aws_iam_role_policy_attachment" "task_extra" {
  count = length(var.task_role_policy_arns)

  role       = aws_iam_role.task.name
  policy_arn = var.task_role_policy_arns[count.index]
}

# -----------------------------------------------------------------------------
# Task execution role (ECS agent: image pull, logs, secrets)
# -----------------------------------------------------------------------------

data "aws_iam_policy_document" "execution" {
  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.service.arn}:*"]
  }

  statement {
    sid     = "Secrets"
    actions = ["secretsmanager:GetSecretValue"]
    resources = concat(
      local.managed_secret_arns,
      local.extra_secretsmanager_arns,
      var.image_pull_secret_arn == null ? [] : [var.image_pull_secret_arn],
    )
  }

  dynamic "statement" {
    for_each = length(local.extra_ssm_parameter_arns) > 0 ? [1] : []
    content {
      sid       = "Parameters"
      actions   = ["ssm:GetParameters"]
      resources = local.extra_ssm_parameter_arns
    }
  }

  dynamic "statement" {
    for_each = var.kms_key_arn == null ? [] : [var.kms_key_arn]
    content {
      sid       = "SecretsKms"
      actions   = ["kms:Decrypt"]
      resources = [statement.value]
    }
  }

  dynamic "statement" {
    for_each = local.ecr_repository_arn == null ? [] : [1]
    content {
      sid       = "EcrAuth"
      actions   = ["ecr:GetAuthorizationToken"]
      resources = ["*"]
    }
  }

  dynamic "statement" {
    for_each = local.ecr_repository_arn == null ? [] : [local.ecr_repository_arn]
    content {
      sid       = "EcrPull"
      actions   = ["ecr:BatchCheckLayerAvailability", "ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage"]
      resources = [statement.value]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "${var.name}-execution"
  description        = "Backyard ECS task execution role"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json

  tags = local.tags
}

resource "aws_iam_role_policy" "execution" {
  name   = "backyard-execution"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution.json
}

# -----------------------------------------------------------------------------
# Lambda deploy gateway role
# -----------------------------------------------------------------------------

data "aws_iam_policy_document" "lambda_assume" {
  statement {
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

data "aws_iam_policy_document" "gateway" {
  count = var.enable_lambda_gateway ? 1 : 0

  source_policy_documents = [data.aws_iam_policy_document.storage_access.json]

  statement {
    sid       = "Logs"
    actions   = ["logs:CreateLogStream", "logs:PutLogEvents"]
    resources = ["${aws_cloudwatch_log_group.gateway[0].arn}:*"]
  }
}

resource "aws_iam_role" "gateway" {
  count = var.enable_lambda_gateway ? 1 : 0

  name               = "${var.name}-gateway"
  description        = "Backyard Lambda deploy gateway"
  assume_role_policy = data.aws_iam_policy_document.lambda_assume.json

  tags = local.tags
}

resource "aws_iam_role_policy" "gateway" {
  count = var.enable_lambda_gateway ? 1 : 0

  name   = "backyard-gateway"
  role   = aws_iam_role.gateway[0].id
  policy = data.aws_iam_policy_document.gateway[0].json
}

# -----------------------------------------------------------------------------
# Direct deploy policy (backyard deploy --method s3 / aws s3 sync)
# -----------------------------------------------------------------------------

data "aws_iam_policy_document" "deployer" {
  count = var.create_deployer_policy ? 1 : 0

  statement {
    sid       = "ListSites"
    actions   = ["s3:ListBucket"]
    resources = [local.bucket_arn]

    condition {
      test     = "StringLike"
      variable = "s3:prefix"
      values   = ["sites/*"]
    }
  }

  statement {
    sid       = "BucketLocation"
    actions   = ["s3:GetBucketLocation"]
    resources = [local.bucket_arn]
  }

  statement {
    sid       = "SyncSiteFiles"
    actions   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:AbortMultipartUpload"]
    resources = ["${local.bucket_arn}/sites/*"]
  }

  dynamic "statement" {
    for_each = var.kms_key_arn == null ? [] : [var.kms_key_arn]
    content {
      sid       = "Kms"
      actions   = ["kms:Decrypt", "kms:GenerateDataKey"]
      resources = [statement.value]
    }
  }
}

resource "aws_iam_policy" "deployer" {
  count = var.create_deployer_policy ? 1 : 0

  name        = "${var.name}-deployer"
  description = "Deploy Backyard sites directly to S3 (aws s3 sync / backyard deploy --method s3)"
  policy      = data.aws_iam_policy_document.deployer[0].json

  tags = local.tags
}
