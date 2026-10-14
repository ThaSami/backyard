# ECS Fargate: cluster, task definition (backyard serve), service behind the
# ALB and CPU target-tracking autoscaling.

locals {
  container_environment = merge(
    var.extra_environment,
    {
      BACKYARD_CONFIG_YAML = local.backyard_config_yaml
      AWS_REGION           = local.region
    },
  )

  container_definition = merge(
    {
      name      = local.container_name
      image     = var.image
      essential = true

      portMappings = [{
        name          = "http"
        containerPort = local.container_port
        hostPort      = local.container_port
        protocol      = "tcp"
        appProtocol   = "http"
      }]

      environment = [for k in sort(keys(local.container_environment)) : { name = k, value = local.container_environment[k] }]
      secrets     = local.container_secrets

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          awslogs-group         = aws_cloudwatch_log_group.service.name
          awslogs-region        = local.region
          awslogs-stream-prefix = "backyard"
          mode                  = "non-blocking"
          max-buffer-size       = "25m"
        }
      }

      # Websockets: one descriptor per connection.
      ulimits = [{ name = "nofile", softLimit = 65536, hardLimit = 65536 }]

      linuxParameters = { initProcessEnabled = true }
      stopTimeout     = 30
    },
    var.container_command == null ? {} : { command = var.container_command },
    var.image_pull_secret_arn == null ? {} : { repositoryCredentials = { credentialsParameter = var.image_pull_secret_arn } },
  )

  # Valid Fargate memory sizes (MiB) per CPU size (Linux):
  # https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-tasks-services.html#fargate-tasks-size
  fargate_memory_options = {
    "256"   = [512, 1024, 2048]
    "512"   = range(1024, 4097, 1024)
    "1024"  = range(2048, 8193, 1024)
    "2048"  = range(4096, 16385, 1024)
    "4096"  = range(8192, 30721, 1024)
    "8192"  = range(16384, 61441, 4096)
    "16384" = range(32768, 122881, 8192)
  }

  capacity_providers = var.use_fargate_spot ? [
    { capacity_provider = "FARGATE", base = 1, weight = 0 },
    { capacity_provider = "FARGATE_SPOT", base = 0, weight = 1 },
    ] : [
    { capacity_provider = "FARGATE", base = 0, weight = 1 },
  ]
}

resource "aws_cloudwatch_log_group" "service" {
  name              = "/ecs/${var.name}"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = local.tags
}

resource "aws_ecs_cluster" "this" {
  name = var.name

  setting {
    name  = "containerInsights"
    value = var.container_insights ? "enabled" : "disabled"
  }

  tags = local.tags
}

resource "aws_ecs_cluster_capacity_providers" "this" {
  cluster_name       = aws_ecs_cluster.this.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]

  default_capacity_provider_strategy {
    capacity_provider = "FARGATE"
    base              = 0
    weight            = 1
  }
}

resource "aws_ecs_task_definition" "this" {
  family                   = var.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.cpu
  memory                   = var.memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = var.cpu_architecture
  }

  container_definitions = jsonencode([local.container_definition])

  tags = local.tags

  lifecycle {
    precondition {
      condition     = local.pubsub_type != "postgres" || var.database == "postgres"
      error_message = "pubsub = \"postgres\" requires database = \"postgres\"."
    }
    precondition {
      condition     = local.pubsub_type != "memory" || !local.multi_instance
      error_message = "pubsub = \"memory\" only works with a single task; set desired_count = 1 and enable_autoscaling = false (or autoscaling_max_capacity = 1), or use pubsub = \"auto\"."
    }
    precondition {
      condition     = contains(lookup(local.fargate_memory_options, tostring(var.cpu), []), var.memory)
      error_message = "memory ${var.memory} is not a valid Fargate size for cpu ${var.cpu}; valid values: ${join(", ", lookup(local.fargate_memory_options, tostring(var.cpu), []))} (see https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-tasks-services.html#fargate-tasks-size)."
    }
    precondition {
      condition     = length(local.task_subnet_ids) >= 1
      error_message = "No subnets for the ECS tasks: set private_subnet_ids (or public_subnet_ids with tasks_in_public_subnets = true) when using an existing VPC."
    }
  }
}

resource "aws_ecs_service" "this" {
  name            = var.name
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.this.arn
  desired_count   = var.desired_count

  platform_version                  = "LATEST"
  enable_execute_command            = var.enable_execute_command
  enable_ecs_managed_tags           = true
  propagate_tags                    = "SERVICE"
  health_check_grace_period_seconds = var.health_check_grace_period
  wait_for_steady_state             = var.wait_for_steady_state

  deployment_minimum_healthy_percent = 100
  deployment_maximum_percent         = 200

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }

  dynamic "capacity_provider_strategy" {
    for_each = local.capacity_providers
    content {
      capacity_provider = capacity_provider_strategy.value.capacity_provider
      base              = capacity_provider_strategy.value.base
      weight            = capacity_provider_strategy.value.weight
    }
  }

  network_configuration {
    subnets          = local.task_subnet_ids
    security_groups  = [aws_security_group.tasks.id]
    assign_public_ip = var.tasks_in_public_subnets
  }

  load_balancer {
    target_group_arn = aws_lb_target_group.app.arn
    container_name   = local.container_name
    container_port   = local.container_port
  }

  tags = local.tags

  # The autoscaling target owns the running task count.
  lifecycle {
    ignore_changes = [desired_count]
  }

  # Tasks need the listener, their IAM permissions, every secret value and
  # (in a new VPC) the egress routes before the first one starts.
  depends_on = [
    aws_lb_listener.https,
    aws_iam_role_policy.task,
    aws_iam_role_policy.execution,
    aws_iam_role_policy_attachment.task_extra,
    aws_ecs_cluster_capacity_providers.this,
    aws_secretsmanager_secret_version.backyard_secret,
    aws_secretsmanager_secret_version.oidc_client_secret,
    aws_secretsmanager_secret_version.ai_keys,
    aws_secretsmanager_secret_version.database_url,
    aws_secretsmanager_secret_version.redis_url,
    aws_route.public_internet,
    aws_route.private_nat,
    aws_route_table_association.public,
    aws_route_table_association.private,
    aws_vpc_endpoint.gateway,
  ]
}

# The scalable target always exists: min = desired_count, max = desired_count
# (fixed size) or autoscaling_max_capacity.
resource "aws_appautoscaling_target" "this" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.this.name}/${aws_ecs_service.this.name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.desired_count
  max_capacity       = local.max_tasks

  tags = local.tags
}

resource "aws_appautoscaling_policy" "cpu" {
  count = local.max_tasks > var.desired_count ? 1 : 0

  name               = "${var.name}-cpu"
  policy_type        = "TargetTrackingScaling"
  service_namespace  = aws_appautoscaling_target.this.service_namespace
  resource_id        = aws_appautoscaling_target.this.resource_id
  scalable_dimension = aws_appautoscaling_target.this.scalable_dimension

  target_tracking_scaling_policy_configuration {
    target_value       = var.autoscaling_cpu_target
    scale_in_cooldown  = 300
    scale_out_cooldown = 60

    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
  }
}
