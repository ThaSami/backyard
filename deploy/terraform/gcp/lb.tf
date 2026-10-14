# Global external Application Load Balancer in front of Cloud Run.
#
# Two backend services share one serverless NEG:
#   app    – everything; IAP-protected when auth_mode = "iap".
#   deploy – (IAP mode only) the deploy gateway paths used by CLIs and agents
#            with Backyard bearer tokens, which IAP would otherwise reject:
#              /__backyard/healthz                 (uptime checks)
#              /__backyard/api/deploy/info         (unauthenticated discovery)
#              /__backyard/api/deploy/uploads/*    (self-authenticating signed URLs)
#              /__backyard/api/deploy/*  with an Authorization header
#              /__backyard/api/tokens    with an Authorization header
#                                        (`backyard token create`; a token
#                                        minted with a token never outlives it)
#            Browser calls to the gateway and the token API (the hub's
#            drag-and-drop deploy and token page) carry no Authorization
#            header and stay behind IAP.
#            The Claude connector's machine paths (SPEC §8.4), with or without
#            an Authorization header (Claude's first call and the OAuth calls
#            carry none; Backyard checks its own MCP tokens there and never
#            reads IAP headers; it answers 404 until mcp.enabled):
#              /__backyard/mcp
#              /__backyard/oauth/*
#              /.well-known/oauth-authorization-server
#              /.well-known/oauth-protected-resource/__backyard/mcp
#            /__backyard/auth/authorize stays behind IAP: the IAP login is the
#            connector's sign-in.

resource "google_compute_global_address" "ipv4" {
  project      = var.project_id
  name         = "${var.name}-ipv4"
  address_type = "EXTERNAL"
  ip_version   = "IPV4"
  labels       = local.labels

  depends_on = [google_project_service.apis]
}

resource "google_compute_global_address" "ipv6" {
  count = var.enable_ipv6 ? 1 : 0

  project      = var.project_id
  name         = "${var.name}-ipv6"
  address_type = "EXTERNAL"
  ip_version   = "IPV6"
  labels       = local.labels

  depends_on = [google_project_service.apis]
}

# References the Cloud Run service by name (not by resource) to avoid a cycle:
# the service's configuration embeds the IAP audience of the backend below.
resource "google_compute_region_network_endpoint_group" "run" {
  project               = var.project_id
  name                  = "${var.name}-neg"
  region                = var.region
  network_endpoint_type = "SERVERLESS"

  cloud_run {
    service = local.service_name
  }

  depends_on = [google_project_service.apis]
}

resource "google_compute_backend_service" "app" {
  project               = var.project_id
  name                  = "${var.name}-app"
  description           = "Backyard hub and sites${local.iap ? " (IAP)" : ""}."
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTPS"
  security_policy       = var.security_policy != "" ? var.security_policy : null

  backend {
    group = google_compute_region_network_endpoint_group.run.id
  }

  dynamic "iap" {
    for_each = local.iap ? [1] : []
    content {
      enabled = true
      # Empty: Google-managed OAuth client (organization users only).
      oauth2_client_id     = local.has_iap_oauth_client ? var.iap_oauth2_client_id : null
      oauth2_client_secret = local.has_iap_oauth_client ? var.iap_oauth2_client_secret : null
    }
  }

  log_config {
    enable      = var.lb_log_sample_rate > 0
    sample_rate = var.lb_log_sample_rate > 0 ? var.lb_log_sample_rate : null
  }
}

resource "google_compute_backend_service" "deploy" {
  count = local.iap ? 1 : 0

  project               = var.project_id
  name                  = "${var.name}-deploy"
  description           = "Backyard deploy gateway for bearer-token clients (no IAP; Backyard authenticates every call)."
  load_balancing_scheme = "EXTERNAL_MANAGED"
  protocol              = "HTTPS"
  security_policy       = var.security_policy != "" ? var.security_policy : null

  backend {
    group = google_compute_region_network_endpoint_group.run.id
  }

  log_config {
    enable      = var.lb_log_sample_rate > 0
    sample_rate = var.lb_log_sample_rate > 0 ? var.lb_log_sample_rate : null
  }
}

# Authoritative for the role on this backend service. With iap_members empty
# the module leaves IAP access to be managed elsewhere.
resource "google_iap_web_backend_service_iam_binding" "access" {
  count = local.iap && length(var.iap_members) > 0 ? 1 : 0

  project             = var.project_id
  web_backend_service = google_compute_backend_service.app.name
  role                = "roles/iap.httpsResourceAccessor"
  members             = var.iap_members
}

locals {
  # IAP headers are only trustworthy when IAP set them; strip client-supplied
  # copies on routes that bypass IAP (defense in depth: Backyard verifies the
  # signed assertion anyway).
  iap_headers = ["X-Goog-IAP-JWT-Assertion", "X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id"]

  deploy_routes = [
    { full_path = "/__backyard/healthz", prefix = null, needs_authorization = false },
    { full_path = "/__backyard/api/deploy/info", prefix = null, needs_authorization = false },
    { full_path = null, prefix = "/__backyard/api/deploy/uploads/", needs_authorization = false },
    { full_path = null, prefix = "/__backyard/api/deploy/", needs_authorization = true },
    { full_path = "/__backyard/api/tokens", prefix = null, needs_authorization = true },
    # The Claude connector's machine paths, the last four (docs/contract_test.go
    # compares them with the other kits).
    { full_path = "/__backyard/mcp", prefix = null, needs_authorization = false },
    { full_path = null, prefix = "/__backyard/oauth/", needs_authorization = false },
    { full_path = "/.well-known/oauth-authorization-server", prefix = null, needs_authorization = false },
    { full_path = "/.well-known/oauth-protected-resource/__backyard/mcp", prefix = null, needs_authorization = false },
  ]
}

resource "google_compute_url_map" "https" {
  project         = var.project_id
  name            = "${var.name}-https"
  description     = "Backyard routing."
  default_service = google_compute_backend_service.app.id

  dynamic "host_rule" {
    for_each = local.iap ? [1] : []
    content {
      hosts        = ["*"]
      path_matcher = "backyard"
    }
  }

  dynamic "path_matcher" {
    for_each = local.iap ? [1] : []
    content {
      name            = "backyard"
      default_service = google_compute_backend_service.app.id

      dynamic "route_rules" {
        for_each = local.deploy_routes
        content {
          priority = route_rules.key + 1
          service  = google_compute_backend_service.deploy[0].id

          match_rules {
            full_path_match = route_rules.value.full_path
            prefix_match    = route_rules.value.prefix

            dynamic "header_matches" {
              for_each = route_rules.value.needs_authorization ? [1] : []
              content {
                header_name   = "Authorization"
                present_match = true
              }
            }
          }

          header_action {
            request_headers_to_remove = local.iap_headers
          }
        }
      }
    }
  }
}

resource "google_compute_ssl_policy" "modern" {
  project         = var.project_id
  name            = "${var.name}-tls"
  profile         = "MODERN"
  min_tls_version = "TLS_1_2"

  depends_on = [google_project_service.apis]
}

resource "google_compute_target_https_proxy" "https" {
  project         = var.project_id
  name            = "${var.name}-https"
  url_map         = google_compute_url_map.https.id
  certificate_map = local.certificate_map_ref
  ssl_policy      = google_compute_ssl_policy.modern.id
}

resource "google_compute_global_forwarding_rule" "https" {
  for_each = local.lb_addresses

  project               = var.project_id
  name                  = "${var.name}-https-${each.key}"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_protocol           = "TCP"
  ip_address            = each.value
  port_range            = "443"
  target                = google_compute_target_https_proxy.https.id
  labels                = local.labels
}

# Port 80: permanent redirect to HTTPS.
resource "google_compute_url_map" "http_redirect" {
  count = var.http_redirect ? 1 : 0

  project     = var.project_id
  name        = "${var.name}-http-redirect"
  description = "Redirect HTTP to HTTPS."

  default_url_redirect {
    https_redirect         = true
    redirect_response_code = "MOVED_PERMANENTLY_DEFAULT"
    strip_query            = false
  }

  depends_on = [google_project_service.apis]
}

resource "google_compute_target_http_proxy" "http_redirect" {
  count = var.http_redirect ? 1 : 0

  project = var.project_id
  name    = "${var.name}-http-redirect"
  url_map = google_compute_url_map.http_redirect[0].id
}

resource "google_compute_global_forwarding_rule" "http_redirect" {
  for_each = var.http_redirect ? local.lb_addresses : {}

  project               = var.project_id
  name                  = "${var.name}-http-${each.key}"
  load_balancing_scheme = "EXTERNAL_MANAGED"
  ip_protocol           = "TCP"
  ip_address            = each.value
  port_range            = "80"
  target                = google_compute_target_http_proxy.http_redirect[0].id
  labels                = local.labels
}

locals {
  lb_addresses = merge(
    { ipv4 = google_compute_global_address.ipv4.id },
    var.enable_ipv6 ? { ipv6 = google_compute_global_address.ipv6[0].id } : {},
  )
}
