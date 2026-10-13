output "hub_url" {
  description = "URL of the Backyard hub."
  value       = local.public_url
}

output "site_url_pattern" {
  description = "URL pattern of deployed sites."
  value       = local.host_mode ? "https://<site>.${var.domain}/" : "${local.public_url}/s/<site>/"
}

output "gateway_url" {
  description = "Deploy gateway base URL advertised to CLIs: the Lambda function URL when enabled, else the embedded gateway."
  value       = var.enable_lambda_gateway ? local.lambda_gateway_url : "${local.public_url}/__backyard/api/deploy"
}

output "lambda_function_name" {
  description = "Name of the Lambda deploy gateway function (null when disabled)."
  value       = one(aws_lambda_function.gateway[*].function_name)
}

output "cli_login_command" {
  description = "Command developers run to point the backyard CLI at this deployment."
  value       = var.enable_lambda_gateway ? "backyard login --server ${local.public_url} --gateway ${local.lambda_gateway_url}" : "backyard login --server ${local.public_url}"
}

output "oidc_redirect_uris" {
  description = "Sign-in redirect URIs to register in the IdP application (empty with auth_mode = \"none\")."
  value = (
    var.auth_mode == "oidc" ? ["${local.public_url}/__backyard/auth/callback"] :
    var.auth_mode == "alb" ? (local.host_mode ? ["${local.public_url}/oauth2/idpresponse", "https://*.${var.domain}/oauth2/idpresponse"] : ["${local.public_url}/oauth2/idpresponse"]) :
    []
  )
}

output "bucket" {
  description = "S3 bucket holding sites (sites/<site>/...) and uploads."
  value       = aws_s3_bucket.this.id
}

output "bucket_arn" {
  description = "ARN of the S3 bucket."
  value       = aws_s3_bucket.this.arn
}

output "deployer_policy_arn" {
  description = "IAM policy for direct `aws s3 sync` deploys (attach to CI roles / developer groups); null when disabled."
  value       = one(aws_iam_policy.deployer[*].arn)
}

output "alb_dns_name" {
  description = "DNS name of the load balancer (target of the apex and wildcard records)."
  value       = aws_lb.this.dns_name
}

output "alb_zone_id" {
  description = "Route 53 zone id of the load balancer (for ALIAS records in other zones)."
  value       = aws_lb.this.zone_id
}

output "alb_arn" {
  description = "ARN of the load balancer (auth.alb.signer_arn in ALB mode)."
  value       = aws_lb.this.arn
}

output "certificate_arn" {
  description = "ACM certificate used by the HTTPS listener."
  value       = local.certificate_arn
}

output "certificate_validation_records" {
  description = "CNAME records that validate the ACM certificate. Create them at your DNS provider when route53_zone_id is not set (see README)."
  value       = local.create_certificate && !local.manage_dns ? local.certificate_validation_records : []
}

output "dns_records" {
  description = "DNS records pointing the domain (and wildcard) at the ALB. Created automatically with route53_zone_id; otherwise create them yourself (CNAME, or ALIAS/ANAME at the apex)."
  value       = [for h in local.alb_hostnames : { name = h, type = "CNAME", value = aws_lb.this.dns_name }]
}

output "ecs_cluster_name" {
  description = "ECS cluster name."
  value       = aws_ecs_cluster.this.name
}

output "ecs_service_name" {
  description = "ECS service name."
  value       = aws_ecs_service.this.name
}

output "task_role_arn" {
  description = "IAM role assumed by Backyard tasks (attach extra permissions here, e.g. for warehouse sources)."
  value       = aws_iam_role.task.arn
}

output "execution_role_arn" {
  description = "ECS task execution role."
  value       = aws_iam_role.execution.arn
}

output "log_group_name" {
  description = "CloudWatch log group of the Backyard service."
  value       = aws_cloudwatch_log_group.service.name
}

output "database" {
  description = "Document store in use and its location."
  value = {
    type     = var.database
    table    = one(aws_dynamodb_table.this[*].name)
    endpoint = one(aws_db_instance.this[*].endpoint)
  }
}

output "pubsub" {
  description = "Realtime pub/sub backend in use (memory, redis or postgres) and the Valkey endpoint when created."
  value = {
    type     = local.pubsub_type
    endpoint = one(aws_elasticache_replication_group.this[*].primary_endpoint_address)
  }
}

output "secret_arns" {
  description = "Secrets Manager secrets created by the module."
  value = {
    backyard_secret    = aws_secretsmanager_secret.backyard_secret.arn
    oidc_client_secret = one(aws_secretsmanager_secret.oidc_client_secret[*].arn)
    ai_api_keys        = one(aws_secretsmanager_secret.ai_keys[*].arn)
    database_url       = one(aws_secretsmanager_secret.database_url[*].arn)
    redis_url          = one(aws_secretsmanager_secret.redis_url[*].arn)
  }
}

output "vpc_id" {
  description = "VPC of the deployment."
  value       = local.vpc_id
}

output "private_subnet_ids" {
  description = "Private subnets (tasks, RDS, ElastiCache)."
  value       = local.private_subnet_ids
}

output "public_subnet_ids" {
  description = "Public subnets (internet-facing ALB, NAT)."
  value       = local.public_subnet_ids
}

output "security_group_ids" {
  description = "Security groups of the ALB and the tasks."
  value = {
    alb   = aws_security_group.alb.id
    tasks = aws_security_group.tasks.id
  }
}

output "backyard_config_yaml" {
  description = "The rendered backyard.yaml passed to the service as BACKYARD_CONFIG_YAML (contains no secret values; secrets are environment references)."
  value       = local.backyard_config_yaml
}
