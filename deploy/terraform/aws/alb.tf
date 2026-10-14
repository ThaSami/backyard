# Application Load Balancer: HTTPS listener (optionally with ALB OIDC
# authentication), HTTP -> HTTPS redirect and the target group of the tasks.

locals {
  # ALB authenticate-oidc endpoints: explicit, or from the issuer's discovery
  # document (fetched at plan time).
  oidc_discovery = length(data.http.oidc_discovery) > 0 ? jsondecode(data.http.oidc_discovery[0].response_body) : null

  alb_oidc = !local.alb_auth ? null : {
    issuer                 = var.oidc_issuer
    authorization_endpoint = var.oidc_endpoints != null ? var.oidc_endpoints.authorization_endpoint : try(local.oidc_discovery.authorization_endpoint, null)
    token_endpoint         = var.oidc_endpoints != null ? var.oidc_endpoints.token_endpoint : try(local.oidc_discovery.token_endpoint, null)
    user_info_endpoint     = var.oidc_endpoints != null ? var.oidc_endpoints.user_info_endpoint : try(local.oidc_discovery.userinfo_endpoint, null)
  }

  # Paths that bypass ALB authentication entirely (probes).
  alb_public_paths = ["/__backyard/healthz", "/__backyard/readyz"]
  # Deploy gateway paths and the token API: authenticated if the browser has
  # an ALB session (the hub's drag-and-drop deploy and token page), otherwise
  # passed through so CLIs and CI can deploy and run `backyard token create`
  # with Backyard bearer tokens, which Backyard verifies itself (a token minted
  # with a token never outlives it).
  alb_deploy_paths = ["/__backyard/api/deploy", "/__backyard/api/deploy/*", "/__backyard/api/tokens"]
  # The Claude connector's machine paths (SPEC §8.4): its MCP endpoint, the
  # OAuth endpoints and the two discovery documents check their own tokens and
  # never read the ALB's identity headers. Exempt unconditionally: Backyard
  # answers 404 there until mcp.enabled. /__backyard/auth/authorize stays
  # behind the ALB login, which is the connector's sign-in.
  alb_mcp_paths = ["/__backyard/mcp", "/__backyard/oauth/*", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"]
}

data "http" "oidc_discovery" {
  count = local.alb_auth && var.oidc_endpoints == null && var.oidc_issuer != null ? 1 : 0

  url = "${var.oidc_issuer}/.well-known/openid-configuration"

  request_headers = {
    Accept = "application/json"
  }

  lifecycle {
    postcondition {
      condition     = self.status_code == 200 && can(jsondecode(self.response_body).authorization_endpoint) && can(jsondecode(self.response_body).token_endpoint) && can(jsondecode(self.response_body).userinfo_endpoint)
      error_message = "Could not read authorization_endpoint, token_endpoint and userinfo_endpoint from <oidc_issuer>/.well-known/openid-configuration; check oidc_issuer or set oidc_endpoints explicitly."
    }
  }
}

resource "aws_lb" "this" {
  name               = var.name
  internal           = var.internal
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = local.alb_subnet_ids

  idle_timeout               = var.alb_idle_timeout
  drop_invalid_header_fields = true
  enable_http2               = true

  dynamic "access_logs" {
    for_each = var.alb_access_logs_bucket == null ? [] : [1]
    content {
      bucket  = var.alb_access_logs_bucket
      prefix  = var.alb_access_logs_prefix
      enabled = true
    }
  }

  tags = merge(local.tags, { Name = var.name })

  lifecycle {
    precondition {
      condition     = length(local.alb_subnet_ids) >= 2
      error_message = "The ALB needs at least two subnets in different AZs: set public_subnet_ids (internet-facing) or private_subnet_ids (internal = true) when using an existing VPC."
    }
    precondition {
      condition     = !local.alb_auth || (var.oidc_issuer != null && var.oidc_client_id != null && local.has_oidc_client_secret)
      error_message = "auth_mode = \"alb\" requires oidc_issuer, oidc_client_id and oidc_client_secret."
    }
    precondition {
      condition     = var.auth_mode != "oidc" || (var.oidc_issuer != null && var.oidc_client_id != null)
      error_message = "auth_mode = \"oidc\" requires oidc_issuer and oidc_client_id (and normally oidc_client_secret)."
    }
    precondition {
      condition     = !var.allow_anonymous_deploys || var.auth_mode == "none"
      error_message = "allow_anonymous_deploys is only valid with auth_mode = \"none\"."
    }
    precondition {
      # An internal ALB reaches the IdP token/userinfo endpoints through NAT.
      condition     = !(local.alb_auth && var.internal && local.create_vpc && local.nat_gateway_count == 0)
      error_message = "auth_mode = \"alb\" with internal = true needs a NAT gateway for the ALB to reach the identity provider; do not combine it with tasks_in_public_subnets in the module-created VPC."
    }
  }
}

resource "aws_wafv2_web_acl_association" "this" {
  count = var.waf_web_acl_arn == null ? 0 : 1

  resource_arn = aws_lb.this.arn
  web_acl_arn  = var.waf_web_acl_arn
}

resource "aws_lb_target_group" "app" {
  name                 = "${var.name}-app"
  port                 = local.container_port
  protocol             = "HTTP"
  protocol_version     = "HTTP1"
  target_type          = "ip"
  vpc_id               = local.vpc_id
  deregistration_delay = 30

  health_check {
    enabled             = true
    path                = "/__backyard/healthz"
    matcher             = "200"
    protocol            = "HTTP"
    port                = "traffic-port"
    interval            = 15
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  tags = merge(local.tags, { Name = "${var.name}-app" })
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type = "redirect"

    redirect {
      protocol    = "HTTPS"
      port        = "443"
      status_code = "HTTP_301"
    }
  }

  tags = local.tags
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = var.ssl_policy
  certificate_arn   = local.certificate_arn

  routing_http_response_strict_transport_security_header_value = var.hsts_header

  dynamic "default_action" {
    for_each = local.alb_auth ? [local.alb_oidc] : []
    content {
      type  = "authenticate-oidc"
      order = 1

      authenticate_oidc {
        issuer                     = default_action.value.issuer
        authorization_endpoint     = default_action.value.authorization_endpoint
        token_endpoint             = default_action.value.token_endpoint
        user_info_endpoint         = default_action.value.user_info_endpoint
        client_id                  = var.oidc_client_id
        client_secret              = var.oidc_client_secret
        scope                      = join(" ", var.oidc_scopes)
        session_timeout            = var.alb_session_timeout
        session_cookie_name        = "AWSELBAuthSessionCookie"
        on_unauthenticated_request = "authenticate"
      }
    }
  }

  default_action {
    type             = "forward"
    order            = local.alb_auth ? 2 : 1
    target_group_arn = aws_lb_target_group.app.arn
  }

  tags = local.tags
}

# The ALB never sets X-Forwarded-Host but passes a client-supplied one through
# unchanged, and Backyard honours that header from its trusted proxies (the
# ALB subnets) when routing hosts to sites (SPEC §2). Reject it at the edge so
# clients cannot choose the host Backyard routes by. Evaluated before the ALB
# authentication rules.
resource "aws_lb_listener_rule" "reject_forwarded_host" {
  count = var.reject_forwarded_host_header ? 1 : 0

  listener_arn = aws_lb_listener.https.arn
  priority     = 1

  condition {
    http_header {
      http_header_name = "X-Forwarded-Host"
      values           = ["*"]
    }
  }

  action {
    type = "fixed-response"

    fixed_response {
      content_type = "text/plain"
      message_body = "Requests with an X-Forwarded-Host header are not accepted.\n"
      status_code  = "400"
    }
  }

  tags = local.tags
}

# ALB mode: health/readiness probes are never authenticated.
resource "aws_lb_listener_rule" "public" {
  count = local.alb_auth ? 1 : 0

  listener_arn = aws_lb_listener.https.arn
  priority     = 10

  condition {
    path_pattern {
      values = local.alb_public_paths
    }
  }

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }

  tags = local.tags
}

# ALB mode: the Claude connector's machine paths are never authenticated by the
# ALB (Claude calls them without a browser session). No host condition, as in
# the public rule: on site hosts Backyard answers 404 at the machine paths and
# demands the ALB's signed header for a site's own /.well-known files.
resource "aws_lb_listener_rule" "mcp" {
  count = local.alb_auth ? 1 : 0

  listener_arn = aws_lb_listener.https.arn
  priority     = 15

  condition {
    path_pattern {
      values = local.alb_mcp_paths
    }
  }

  action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.app.arn
  }

  tags = local.tags
}

# ALB mode: the embedded deploy gateway and the token API accept bearer tokens
# from CLIs, and the browser's ALB session when present
# (on_unauthenticated_request = "allow").
resource "aws_lb_listener_rule" "deploy" {
  count = local.alb_auth ? 1 : 0

  listener_arn = aws_lb_listener.https.arn
  priority     = 20

  condition {
    path_pattern {
      values = local.alb_deploy_paths
    }
  }

  action {
    type  = "authenticate-oidc"
    order = 1

    authenticate_oidc {
      issuer                     = local.alb_oidc.issuer
      authorization_endpoint     = local.alb_oidc.authorization_endpoint
      token_endpoint             = local.alb_oidc.token_endpoint
      user_info_endpoint         = local.alb_oidc.user_info_endpoint
      client_id                  = var.oidc_client_id
      client_secret              = var.oidc_client_secret
      scope                      = join(" ", var.oidc_scopes)
      session_timeout            = var.alb_session_timeout
      session_cookie_name        = "AWSELBAuthSessionCookie"
      on_unauthenticated_request = "allow"
    }
  }

  action {
    type             = "forward"
    order            = 2
    target_group_arn = aws_lb_target_group.app.arn
  }

  tags = local.tags
}
