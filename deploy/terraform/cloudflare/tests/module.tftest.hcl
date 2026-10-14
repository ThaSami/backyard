# Offline tests for the Cloudflare module: `terraform test` (Terraform >= 1.7).
# The provider is mocked, so no Cloudflare credentials are needed and nothing
# is created. The runs check resource selection per URL mode, the Access
# rules and the Worker variables handed back to setup (setup_args, setup_command).

mock_provider "cloudflare" {
  mock_data "cloudflare_zone" {
    defaults = {
      id   = "023e105f4ecef8ad9ca31a8372d0c353"
      name = "example.com"
    }
  }

  mock_data "cloudflare_zero_trust_organization" {
    defaults = {
      auth_domain = "acme.cloudflareaccess.com"
    }
  }

  mock_resource "cloudflare_zero_trust_access_application" {
    defaults = {
      aud = "737646a56ab1df6ec9bddc7e5ca84eaf3b0768850f3ffb5d74f1534911fe3893"
    }
  }

  mock_resource "cloudflare_zero_trust_access_policy" {
    defaults = {
      id = "f174e90a-fafe-4643-bbbc-4a0ed4fc8415"
    }
  }

  mock_resource "cloudflare_zero_trust_access_identity_provider" {
    defaults = {
      id = "f174e90a-fafe-4643-bbbc-4a0ed4fc8416"
    }
  }
}

variables {
  account_id            = "0123456789abcdef0123456789abcdef"
  zone_name             = "example.com"
  base_domain           = "backyard.example.com"
  allowed_email_domains = ["acme.com"]
}

run "path_mode_with_access" {
  command = apply

  variables {
    allowed_emails = ["contractor@example.org"]
  }

  assert {
    condition     = length(cloudflare_workers_custom_domain.hub) == 1 && cloudflare_workers_custom_domain.hub[0].hostname == "backyard.example.com"
    error_message = "path mode uses a Workers custom domain for the hub"
  }
  assert {
    condition     = cloudflare_workers_custom_domain.hub[0].service == "backyard"
    error_message = "the custom domain points at the backyard Worker"
  }
  assert {
    condition     = length(cloudflare_workers_route.hosts) == 0 && length(cloudflare_dns_record.sites) == 0 && length(cloudflare_certificate_pack.sites) == 0
    error_message = "path mode needs no routes, DNS records or certificates"
  }
  assert {
    condition     = [for d in cloudflare_zero_trust_access_application.backyard[0].destinations : d.uri] == ["backyard.example.com"]
    error_message = "Access protects the hub host"
  }
  assert {
    condition     = length(cloudflare_zero_trust_access_policy.allow[0].include) == 2
    error_message = "one include rule per allowed email and domain"
  }
  assert {
    condition     = cloudflare_zero_trust_access_policy.bypass[0].decision == "bypass"
    error_message = "machine endpoints bypass Access"
  }
  assert {
    condition = toset([for d in cloudflare_zero_trust_access_application.bypass[0].destinations : d.uri]) == toset([
      "backyard.example.com/__backyard/api/deploy",
      "backyard.example.com/__backyard/api/deploy/*",
      "backyard.example.com/__backyard/api/tokens",
      "backyard.example.com/__backyard/healthz",
      "backyard.example.com/__backyard/readyz",
      "backyard.example.com/__backyard/mcp",
      "backyard.example.com/__backyard/oauth/*",
      "backyard.example.com/.well-known/oauth-authorization-server",
      "backyard.example.com/.well-known/oauth-protected-resource/__backyard/mcp",
    ])
    error_message = "the deploy gateway, token minting (backyard token create), health checks and the Claude connector's machine paths bypass Access"
  }
  assert {
    condition     = length(cloudflare_zero_trust_access_application.bypass[0].destinations) == 9 && cloudflare_zero_trust_access_application.bypass[0].domain == "backyard.example.com/__backyard/api/deploy"
    error_message = "nine bypass destinations; the first path stays the application's domain"
  }
  assert {
    condition     = !contains([for d in cloudflare_zero_trust_access_application.bypass[0].destinations : d.uri], "backyard.example.com/__backyard/auth/authorize")
    error_message = "the connector's sign-in (/__backyard/auth/authorize) stays behind Access"
  }
  assert {
    condition     = cloudflare_zero_trust_access_application.backyard[0].allowed_idps == null && cloudflare_zero_trust_access_application.backyard[0].auto_redirect_to_identity == false
    error_message = "without identity_provider_ids every login method is offered"
  }
  assert {
    condition = output.worker_vars == {
      BASE_DOMAIN        = "backyard.example.com"
      URL_MODE           = "path"
      AUTH_MODE          = "access"
      ACCESS_TEAM_DOMAIN = "acme.cloudflareaccess.com"
      ACCESS_AUD         = "737646a56ab1df6ec9bddc7e5ca84eaf3b0768850f3ffb5d74f1534911fe3893"
    }
    error_message = "worker_vars carries the Access settings"
  }
  assert {
    condition     = output.access_aud == "737646a56ab1df6ec9bddc7e5ca84eaf3b0768850f3ffb5d74f1534911fe3893" && output.access_team_domain == "acme.cloudflareaccess.com"
    error_message = "access outputs"
  }
  assert {
    condition     = strcontains(output.setup_command, "--var ACCESS_AUD:737646a5") && startswith(output.setup_command, "npm run setup -- --worker-name backyard ")
    error_message = "setup_command redeploys the Worker with the variables"
  }
  assert {
    condition = output.setup_args == join(" ", [
      "--worker-name backyard",
      "--var ACCESS_AUD:737646a56ab1df6ec9bddc7e5ca84eaf3b0768850f3ffb5d74f1534911fe3893",
      "--var ACCESS_TEAM_DOMAIN:acme.cloudflareaccess.com",
      "--var AUTH_MODE:access",
      "--var BASE_DOMAIN:backyard.example.com",
      "--var URL_MODE:path",
    ])
    error_message = "setup_args are the arguments of setup (backyard cloudflare setup / npx backyard-cloudflare setup), without npm run setup --"
  }
  assert {
    condition     = output.setup_command == "npm run setup -- ${output.setup_args}"
    error_message = "setup_command is npm run setup -- <setup_args>"
  }
  assert {
    condition     = output.site_url_pattern == "https://backyard.example.com/s/<site>/" && output.gateway_url == "https://backyard.example.com/__backyard/api/deploy"
    error_message = "URL outputs in path mode"
  }
}

run "host_mode_at_apex" {
  command = apply

  variables {
    url_mode    = "host"
    base_domain = "example.com"
    worker_name = "team-backyard"
  }

  assert {
    condition     = length(cloudflare_workers_custom_domain.hub) == 0
    error_message = "host mode uses routes, not a custom domain"
  }
  assert {
    condition     = toset([for r in cloudflare_workers_route.hosts : r.pattern]) == toset(["example.com/*", "*.example.com/*"])
    error_message = "routes for the hub and every site host"
  }
  assert {
    condition     = alltrue([for r in cloudflare_workers_route.hosts : r.script == "team-backyard"])
    error_message = "routes point at the Worker"
  }
  assert {
    condition     = cloudflare_dns_record.sites[0].name == "*.example.com" && cloudflare_dns_record.sites[0].proxied && cloudflare_dns_record.hub[0].name == "example.com"
    error_message = "proxied wildcard and hub records"
  }
  assert {
    condition     = length(cloudflare_certificate_pack.sites) == 0
    error_message = "Universal SSL covers *.<zone>"
  }
  assert {
    condition     = toset([for d in cloudflare_zero_trust_access_application.backyard[0].destinations : d.uri]) == toset(["example.com", "*.example.com"])
    error_message = "Access protects the hub and every site host"
  }
  assert {
    condition     = output.site_url_pattern == "https://<site>.example.com/" && output.worker_vars.URL_MODE == "host"
    error_message = "URL outputs in host mode"
  }
}

run "host_mode_below_apex_with_certificate" {
  command = apply

  variables {
    url_mode             = "host"
    advanced_certificate = true
    manage_dns           = false
  }

  assert {
    condition     = toset(cloudflare_certificate_pack.sites[0].hosts) == toset(["example.com", "backyard.example.com", "*.backyard.example.com"])
    error_message = "the advanced certificate covers the site hosts"
  }
  assert {
    condition     = length(cloudflare_dns_record.sites) == 0 && length(cloudflare_dns_record.hub) == 0
    error_message = "manage_dns = false leaves DNS alone"
  }
}

run "okta" {
  command = apply

  variables {
    allowed_email_domains = []
    okta_account_url      = "https://acme.okta.com/"
    okta_client_id        = "0oa1example"
    okta_client_secret    = "not-a-real-secret"
    allowed_okta_groups   = ["backyard-users", "engineering"]
  }

  assert {
    condition     = cloudflare_zero_trust_access_identity_provider.okta[0].type == "okta" && cloudflare_zero_trust_access_identity_provider.okta[0].config.okta_account == "https://acme.okta.com"
    error_message = "an Okta identity provider is created"
  }
  assert {
    condition     = toset([for r in cloudflare_zero_trust_access_policy.allow[0].include : r.okta.name]) == toset(["backyard-users", "engineering"])
    error_message = "one rule per Okta group"
  }
  assert {
    condition     = alltrue([for r in cloudflare_zero_trust_access_policy.allow[0].include : r.okta.identity_provider_id == "f174e90a-fafe-4643-bbbc-4a0ed4fc8416"])
    error_message = "Okta group rules reference the identity provider"
  }
  assert {
    condition     = toset(cloudflare_zero_trust_access_application.backyard[0].allowed_idps) == toset(["f174e90a-fafe-4643-bbbc-4a0ed4fc8416"]) && cloudflare_zero_trust_access_application.backyard[0].auto_redirect_to_identity
    error_message = "a single identity provider is used directly"
  }
}

run "without_access" {
  command = apply

  variables {
    enable_access         = false
    allowed_email_domains = []
  }

  assert {
    condition     = length(cloudflare_zero_trust_access_application.backyard) == 0 && length(cloudflare_zero_trust_access_policy.allow) == 0 && length(cloudflare_zero_trust_access_application.bypass) == 0
    error_message = "no Access resources"
  }
  assert {
    condition     = output.worker_vars == { BASE_DOMAIN = "backyard.example.com", URL_MODE = "path", AUTH_MODE = "none" } && output.access_aud == ""
    error_message = "the Worker runs with AUTH_MODE=none"
  }
  assert {
    condition     = output.setup_args == "--worker-name backyard --var AUTH_MODE:none --var BASE_DOMAIN:backyard.example.com --var URL_MODE:path"
    error_message = "setup_args carry the variables without Access"
  }
}

run "access_needs_someone_allowed" {
  command = plan

  variables {
    allowed_email_domains = []
  }

  expect_failures = [cloudflare_zero_trust_access_policy.allow]
}

run "okta_groups_need_an_identity_provider" {
  command = plan

  variables {
    allowed_okta_groups = ["backyard-users"]
  }

  expect_failures = [cloudflare_zero_trust_access_policy.allow]
}

run "base_domain_must_be_in_the_zone" {
  command = plan

  variables {
    base_domain = "backyard.example.net"
  }

  expect_failures = [terraform_data.checks]
}

run "bypass_paths_stay_internal" {
  command = plan

  variables {
    bypass_paths = ["/s/*"]
  }

  expect_failures = [var.bypass_paths]
}

run "bypass_paths_refuse_the_hub_root" {
  command = plan

  variables {
    bypass_paths = ["/__backyard/healthz", "/"]
  }

  expect_failures = [var.bypass_paths]
}

run "bypass_paths_refuse_other_well_known_documents" {
  command = plan

  variables {
    bypass_paths = ["/__backyard/healthz", "/.well-known/x"]
  }

  expect_failures = [var.bypass_paths]
}

run "bypass_paths_refuse_site_paths" {
  command = plan

  variables {
    bypass_paths = ["/__backyard/healthz", "/s/x"]
  }

  expect_failures = [var.bypass_paths]
}

run "bypass_paths_accept_the_oauth_discovery_documents" {
  command = plan

  variables {
    bypass_paths = ["/__backyard/mcp", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"]
  }

  assert {
    condition     = length(cloudflare_zero_trust_access_application.bypass[0].destinations) == 3
    error_message = "the two discovery documents are valid bypass paths"
  }
}

run "mcp_enabled" {
  command = apply

  variables {
    mcp_enabled = true
  }

  assert {
    condition     = output.worker_vars.MCP_ENABLED == "true" && output.worker_vars.AUTH_MODE == "access"
    error_message = "mcp_enabled adds MCP_ENABLED=true to worker_vars"
  }
  assert {
    condition     = strcontains(output.setup_args, " --var MCP_ENABLED:true ") && output.setup_command == "npm run setup -- ${output.setup_args}"
    error_message = "setup_args (and setup_command) carry --var MCP_ENABLED:true"
  }
}

run "mcp_disabled_by_default" {
  command = apply

  assert {
    condition     = !contains(keys(output.worker_vars), "MCP_ENABLED") && !strcontains(output.setup_args, "MCP_ENABLED")
    error_message = "the connector is opt-in: no MCP_ENABLED without mcp_enabled"
  }
}
