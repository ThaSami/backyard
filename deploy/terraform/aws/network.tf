# Networking: either a new two-tier VPC (public subnets for the ALB and NAT,
# private subnets for tasks, RDS and ElastiCache) or an existing VPC.

locals {
  create_vpc = var.vpc_id == null

  azs = local.create_vpc ? slice(data.aws_availability_zones.available[0].names, 0, var.az_count) : []

  nat_gateway_count = local.create_vpc && !var.tasks_in_public_subnets ? (var.single_nat_gateway ? 1 : var.az_count) : 0

  vpc_id             = local.create_vpc ? aws_vpc.this[0].id : var.vpc_id
  public_subnet_ids  = local.create_vpc ? aws_subnet.public[*].id : var.public_subnet_ids
  private_subnet_ids = local.create_vpc ? aws_subnet.private[*].id : var.private_subnet_ids

  alb_subnet_ids  = var.internal ? local.private_subnet_ids : local.public_subnet_ids
  task_subnet_ids = var.tasks_in_public_subnets ? local.public_subnet_ids : local.private_subnet_ids

  # server.trusted_proxies: the ALB nodes connect to the tasks from addresses
  # in the ALB subnets, so only those ranges are trusted for X-Forwarded-For
  # and X-Forwarded-Proto. Trusting the whole VPC would let any client inside
  # it (VPN users of an internal ALB, other workloads) spoof its address in
  # rate limits and audit logs.
  alb_subnet_cidrs = local.create_vpc ? [
    for i in range(var.az_count) : cidrsubnet(var.vpc_cidr, 4, var.internal ? i + 8 : i)
  ] : distinct([for s in data.aws_subnet.alb : s.cidr_block])
}

data "aws_availability_zones" "available" {
  count = local.create_vpc ? 1 : 0

  state = "available"

  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

data "aws_subnet" "alb" {
  for_each = local.create_vpc ? toset([]) : toset(local.alb_subnet_ids)

  id = each.value

  lifecycle {
    postcondition {
      condition     = self.vpc_id == var.vpc_id
      error_message = "Subnet ${self.id} is in ${self.vpc_id}, not in vpc_id (${var.vpc_id})."
    }
  }
}

resource "aws_vpc" "this" {
  count = local.create_vpc ? 1 : 0

  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = merge(local.tags, { Name = var.name })

  lifecycle {
    precondition {
      condition     = length(data.aws_availability_zones.available[0].names) >= var.az_count
      error_message = "The region has fewer available availability zones than az_count."
    }
  }
}

# Lock down the default security group (nothing in this module uses it).
resource "aws_default_security_group" "this" {
  count = local.create_vpc ? 1 : 0

  vpc_id = aws_vpc.this[0].id

  tags = merge(local.tags, { Name = "${var.name}-default-unused" })
}

resource "aws_internet_gateway" "this" {
  count = local.create_vpc ? 1 : 0

  vpc_id = aws_vpc.this[0].id

  tags = merge(local.tags, { Name = var.name })
}

resource "aws_subnet" "public" {
  count = local.create_vpc ? var.az_count : 0

  vpc_id                  = aws_vpc.this[0].id
  availability_zone       = local.azs[count.index]
  cidr_block              = cidrsubnet(var.vpc_cidr, 4, count.index)
  map_public_ip_on_launch = false

  tags = merge(local.tags, { Name = "${var.name}-public-${local.azs[count.index]}", Tier = "public" })
}

resource "aws_subnet" "private" {
  count = local.create_vpc ? var.az_count : 0

  vpc_id            = aws_vpc.this[0].id
  availability_zone = local.azs[count.index]
  cidr_block        = cidrsubnet(var.vpc_cidr, 4, count.index + 8)

  tags = merge(local.tags, { Name = "${var.name}-private-${local.azs[count.index]}", Tier = "private" })
}

resource "aws_route_table" "public" {
  count = local.create_vpc ? 1 : 0

  vpc_id = aws_vpc.this[0].id

  tags = merge(local.tags, { Name = "${var.name}-public" })
}

resource "aws_route" "public_internet" {
  count = local.create_vpc ? 1 : 0

  route_table_id         = aws_route_table.public[0].id
  destination_cidr_block = "0.0.0.0/0"
  gateway_id             = aws_internet_gateway.this[0].id
}

resource "aws_route_table_association" "public" {
  count = local.create_vpc ? var.az_count : 0

  subnet_id      = aws_subnet.public[count.index].id
  route_table_id = aws_route_table.public[0].id
}

resource "aws_eip" "nat" {
  count = local.nat_gateway_count

  domain = "vpc"

  tags = merge(local.tags, { Name = "${var.name}-nat-${local.azs[count.index]}" })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_nat_gateway" "this" {
  count = local.nat_gateway_count

  allocation_id = aws_eip.nat[count.index].id
  subnet_id     = aws_subnet.public[count.index].id

  tags = merge(local.tags, { Name = "${var.name}-${local.azs[count.index]}" })

  depends_on = [aws_internet_gateway.this]
}

resource "aws_route_table" "private" {
  count = local.create_vpc ? var.az_count : 0

  vpc_id = aws_vpc.this[0].id

  tags = merge(local.tags, { Name = "${var.name}-private-${local.azs[count.index]}" })
}

resource "aws_route" "private_nat" {
  count = local.nat_gateway_count > 0 ? var.az_count : 0

  route_table_id         = aws_route_table.private[count.index].id
  destination_cidr_block = "0.0.0.0/0"
  nat_gateway_id         = aws_nat_gateway.this[var.single_nat_gateway ? 0 : count.index].id
}

resource "aws_route_table_association" "private" {
  count = local.create_vpc ? var.az_count : 0

  subnet_id      = aws_subnet.private[count.index].id
  route_table_id = aws_route_table.private[count.index].id
}

# Free gateway endpoints keep S3 and DynamoDB traffic off the NAT gateway.
resource "aws_vpc_endpoint" "gateway" {
  for_each = local.create_vpc ? toset(["s3", "dynamodb"]) : toset([])

  vpc_id            = aws_vpc.this[0].id
  service_name      = "com.amazonaws.${local.region}.${each.key}"
  vpc_endpoint_type = "Gateway"
  route_table_ids   = concat(aws_route_table.public[*].id, aws_route_table.private[*].id)

  tags = merge(local.tags, { Name = "${var.name}-${each.key}" })
}

# -----------------------------------------------------------------------------
# Security groups
# -----------------------------------------------------------------------------

resource "aws_security_group" "alb" {
  name_prefix = "${var.name}-alb-"
  description = "Backyard load balancer"
  vpc_id      = local.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-alb" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "alb_cidr" {
  for_each = { for p in setproduct(var.allowed_cidrs, [80, 443]) : "${p[0]}-${p[1]}" => { cidr = p[0], port = p[1] } }

  security_group_id = aws_security_group.alb.id
  description       = "HTTP(S) from allowed networks"
  ip_protocol       = "tcp"
  from_port         = each.value.port
  to_port           = each.value.port
  cidr_ipv4         = each.value.cidr

  tags = local.tags
}

resource "aws_vpc_security_group_ingress_rule" "alb_sg" {
  count = length(var.allowed_security_group_ids) * 2

  security_group_id            = aws_security_group.alb.id
  description                  = "HTTP(S) from allowed security groups"
  ip_protocol                  = "tcp"
  from_port                    = count.index % 2 == 0 ? 80 : 443
  to_port                      = count.index % 2 == 0 ? 80 : 443
  referenced_security_group_id = var.allowed_security_group_ids[floor(count.index / 2)]

  tags = local.tags
}

resource "aws_vpc_security_group_egress_rule" "alb_to_tasks" {
  security_group_id            = aws_security_group.alb.id
  description                  = "To Backyard tasks"
  ip_protocol                  = "tcp"
  from_port                    = local.container_port
  to_port                      = local.container_port
  referenced_security_group_id = aws_security_group.tasks.id

  tags = local.tags
}

# authenticate-oidc: the ALB calls the IdP token and userinfo endpoints.
resource "aws_vpc_security_group_egress_rule" "alb_to_idp" {
  count = local.alb_auth ? 1 : 0

  security_group_id = aws_security_group.alb.id
  description       = "HTTPS to the identity provider (authenticate-oidc)"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = "0.0.0.0/0"

  tags = local.tags
}

resource "aws_security_group" "tasks" {
  name_prefix = "${var.name}-tasks-"
  description = "Backyard ECS tasks"
  vpc_id      = local.vpc_id

  tags = merge(local.tags, { Name = "${var.name}-tasks" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "tasks_from_alb" {
  security_group_id            = aws_security_group.tasks.id
  description                  = "HTTP from the load balancer"
  ip_protocol                  = "tcp"
  from_port                    = local.container_port
  to_port                      = local.container_port
  referenced_security_group_id = aws_security_group.alb.id

  tags = local.tags
}

# Tasks reach AWS APIs, the IdP, AI providers and the image registry.
resource "aws_vpc_security_group_egress_rule" "tasks_all" {
  security_group_id = aws_security_group.tasks.id
  description       = "Outbound to AWS APIs, identity and AI providers, registries"
  ip_protocol       = "-1"
  cidr_ipv4         = "0.0.0.0/0"

  tags = local.tags
}
