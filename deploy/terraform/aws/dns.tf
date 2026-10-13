# TLS certificate (ACM, DNS-validated) and Route 53 records.

locals {
  create_certificate = var.certificate_arn == null
  manage_dns         = var.route53_zone_id != null

  # The apex and the wildcard share one validation record; dedupe by name.
  certificate_validation_records = distinct([
    for dvo in flatten(aws_acm_certificate.this[*].domain_validation_options) : {
      name  = dvo.resource_record_name
      type  = dvo.resource_record_type
      value = dvo.resource_record_value
    }
  ])

  certificate_dvo = {
    for dvo in flatten(aws_acm_certificate.this[*].domain_validation_options) : dvo.domain_name => dvo
  }

  certificate_arn = local.create_certificate ? aws_acm_certificate_validation.this[0].certificate_arn : var.certificate_arn

  # Names on the certificate, which must also resolve to the ALB.
  alb_hostnames = local.host_mode ? [var.domain, "*.${var.domain}"] : [var.domain]
}

resource "aws_acm_certificate" "this" {
  count = local.create_certificate ? 1 : 0

  domain_name               = var.domain
  subject_alternative_names = local.host_mode ? ["*.${var.domain}"] : []
  validation_method         = "DNS"

  options {
    certificate_transparency_logging_preference = "ENABLED"
  }

  tags = merge(local.tags, { Name = var.domain })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_route53_record" "certificate_validation" {
  # Keyed by the certificate's domain names (static); the apex and the
  # wildcard produce the same record, hence allow_overwrite.
  for_each = local.create_certificate && local.manage_dns ? toset(local.alb_hostnames) : toset([])

  zone_id         = var.route53_zone_id
  name            = local.certificate_dvo[each.key].resource_record_name
  type            = local.certificate_dvo[each.key].resource_record_type
  records         = [local.certificate_dvo[each.key].resource_record_value]
  ttl             = 300
  allow_overwrite = true
}

resource "aws_acm_certificate_validation" "this" {
  count = local.create_certificate ? 1 : 0

  certificate_arn         = aws_acm_certificate.this[0].arn
  validation_record_fqdns = local.manage_dns ? [for r in aws_route53_record.certificate_validation : r.fqdn] : null

  timeouts {
    create = var.certificate_validation_timeout
  }
}

resource "aws_route53_record" "alb" {
  for_each = local.manage_dns && var.create_dns_records ? toset(local.alb_hostnames) : toset([])

  zone_id = var.route53_zone_id
  name    = each.key
  type    = "A"

  alias {
    name                   = aws_lb.this.dns_name
    zone_id                = aws_lb.this.zone_id
    evaluate_target_health = true
  }
}
