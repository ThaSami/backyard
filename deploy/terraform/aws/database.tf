# Document store (DynamoDB or RDS PostgreSQL) and the optional ElastiCache
# Valkey cluster used for realtime pub/sub between tasks.

# -----------------------------------------------------------------------------
# DynamoDB (database = "dynamodb")
# -----------------------------------------------------------------------------

resource "aws_dynamodb_table" "this" {
  count = var.database == "dynamodb" ? 1 : 0

  name                        = coalesce(var.dynamodb_table_name, var.name)
  billing_mode                = "PAY_PER_REQUEST"
  hash_key                    = "pk"
  range_key                   = "sk"
  deletion_protection_enabled = var.deletion_protection

  attribute {
    name = "pk"
    type = "S"
  }

  attribute {
    name = "sk"
    type = "S"
  }

  point_in_time_recovery {
    enabled = var.dynamodb_point_in_time_recovery
  }

  # Without a customer-managed key DynamoDB encrypts with an AWS owned key.
  dynamic "server_side_encryption" {
    for_each = var.kms_key_arn == null ? [] : [var.kms_key_arn]
    content {
      enabled     = true
      kms_key_arn = server_side_encryption.value
    }
  }

  tags = merge(local.tags, { Name = coalesce(var.dynamodb_table_name, var.name) })
}

# -----------------------------------------------------------------------------
# RDS PostgreSQL (database = "postgres")
# -----------------------------------------------------------------------------

locals {
  create_postgres = var.database == "postgres"
  db_name         = "backyard"
  db_username     = "backyard"
}

# Alphanumeric so the password can be embedded in a postgres:// URL verbatim.
resource "random_password" "db" {
  count = local.create_postgres ? 1 : 0

  length  = 40
  special = false
}

# Unique per database so repeated destroy/create cycles never collide.
resource "random_id" "db_final_snapshot" {
  count = local.create_postgres ? 1 : 0

  byte_length = 4
}

# Created up front so the exported PostgreSQL logs get a retention period.
resource "aws_cloudwatch_log_group" "postgres" {
  count = local.create_postgres ? 1 : 0

  name              = "/aws/rds/instance/${var.name}/postgresql"
  retention_in_days = var.log_retention_days
  kms_key_id        = var.log_kms_key_arn

  tags = local.tags
}

resource "aws_db_subnet_group" "this" {
  count = local.create_postgres ? 1 : 0

  name       = var.name
  subnet_ids = local.private_subnet_ids

  tags = merge(local.tags, { Name = var.name })
}

resource "aws_security_group" "db" {
  count = local.create_postgres ? 1 : 0

  name_prefix = "${var.name}-db-"
  description = "Backyard PostgreSQL"
  vpc_id      = local.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-db" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "db_from_tasks" {
  count = local.create_postgres ? 1 : 0

  security_group_id            = aws_security_group.db[0].id
  description                  = "PostgreSQL from Backyard tasks"
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  referenced_security_group_id = aws_security_group.tasks.id

  tags = local.tags
}

resource "aws_db_instance" "this" {
  count = local.create_postgres ? 1 : 0

  identifier     = var.name
  engine         = "postgres"
  engine_version = var.db_engine_version
  instance_class = var.db_instance_class

  db_name  = local.db_name
  username = local.db_username
  password = random_password.db[0].result
  port     = 5432

  allocated_storage     = var.db_allocated_storage
  max_allocated_storage = var.db_max_allocated_storage
  storage_type          = "gp3"
  storage_encrypted     = true
  kms_key_id            = var.kms_key_arn

  db_subnet_group_name   = aws_db_subnet_group.this[0].name
  vpc_security_group_ids = [aws_security_group.db[0].id]
  publicly_accessible    = false
  multi_az               = var.db_multi_az

  backup_retention_period   = var.db_backup_retention_days
  copy_tags_to_snapshot     = true
  deletion_protection       = var.deletion_protection
  skip_final_snapshot       = !var.db_final_snapshot
  final_snapshot_identifier = var.db_final_snapshot ? "${var.name}-final-${random_id.db_final_snapshot[0].hex}" : null

  auto_minor_version_upgrade  = true
  allow_major_version_upgrade = false
  apply_immediately           = false

  performance_insights_enabled          = var.db_performance_insights
  performance_insights_retention_period = var.db_performance_insights ? 7 : null
  performance_insights_kms_key_id       = var.db_performance_insights ? var.kms_key_arn : null

  enabled_cloudwatch_logs_exports = ["postgresql"]

  tags = merge(local.tags, { Name = var.name })

  depends_on = [aws_cloudwatch_log_group.postgres]

  lifecycle {
    precondition {
      condition     = length(local.private_subnet_ids) >= 2
      error_message = "RDS needs at least two private subnets in different AZs (private_subnet_ids)."
    }
  }
}

# -----------------------------------------------------------------------------
# ElastiCache for Valkey (pub/sub between tasks)
# -----------------------------------------------------------------------------

resource "random_password" "redis" {
  count = local.create_redis ? 1 : 0

  length  = 40
  special = false
}

resource "aws_elasticache_subnet_group" "this" {
  count = local.create_redis ? 1 : 0

  name       = "${var.name}-pubsub"
  subnet_ids = local.private_subnet_ids

  tags = merge(local.tags, { Name = "${var.name}-pubsub" })
}

resource "aws_security_group" "redis" {
  count = local.create_redis ? 1 : 0

  name_prefix = "${var.name}-pubsub-"
  description = "Backyard pub/sub (ElastiCache Valkey)"
  vpc_id      = local.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-pubsub" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "redis_from_tasks" {
  count = local.create_redis ? 1 : 0

  security_group_id            = aws_security_group.redis[0].id
  description                  = "Valkey from Backyard tasks"
  ip_protocol                  = "tcp"
  from_port                    = 6379
  to_port                      = 6379
  referenced_security_group_id = aws_security_group.tasks.id

  tags = local.tags
}

resource "aws_elasticache_replication_group" "this" {
  count = local.create_redis ? 1 : 0

  replication_group_id = "${var.name}-pubsub"
  description          = "Backyard realtime pub/sub"

  engine             = "valkey"
  engine_version     = var.redis_engine_version
  node_type          = var.redis_node_type
  num_cache_clusters = var.redis_num_nodes
  port               = 6379
  cluster_mode       = "disabled"

  automatic_failover_enabled = var.redis_num_nodes > 1
  multi_az_enabled           = var.redis_num_nodes > 1

  subnet_group_name  = aws_elasticache_subnet_group.this[0].name
  security_group_ids = [aws_security_group.redis[0].id]

  at_rest_encryption_enabled = true
  kms_key_id                 = var.kms_key_arn
  transit_encryption_enabled = true
  auth_token                 = random_password.redis[0].result

  # Pub/sub only: nothing worth snapshotting.
  snapshot_retention_limit = 0
  apply_immediately        = true

  tags = merge(local.tags, { Name = "${var.name}-pubsub" })
}
