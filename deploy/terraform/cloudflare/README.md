# Terraform: Backyard on Cloudflare (domain + Access)

This module puts the Backyard Workers edition ([`deploy/cloudflare`](../../cloudflare)) on your
own domain and, optionally, behind **Cloudflare Access**. Access is free for up to 50 users
and supports Okta, Entra ID, Google Workspace, GitHub, SAML/OIDC and one-time PIN.

The module does **not** deploy the Worker's code. `npm run setup` (or `wrangler deploy`) does
that, and it keeps the Durable Objects, secrets and variables. Terraform manages what sits
around the Worker:

| Resource | When |
| --- | --- |
| `cloudflare_workers_custom_domain` for `base_domain` (DNS record and certificate included) | `url_mode = "path"` |
| Proxied `AAAA` records `base_domain` and `*.base_domain` → `100::` | `url_mode = "host"` and `manage_dns` |
| `cloudflare_workers_route` for `base_domain/*` and `*.base_domain/*` | `url_mode = "host"` |
| `cloudflare_certificate_pack` (advanced) for `*.base_domain` | host mode below the apex with `advanced_certificate` (paid ACM) |
| Access application over the hub (and `*.base_domain` in host mode) with an allow policy | `enable_access` (default) |
| Access application with a bypass policy for `/__backyard/api/deploy`, `/__backyard/api/deploy/*`, `/__backyard/healthz` and `/__backyard/readyz` | `enable_access` |
| Okta identity provider | `okta_account_url` set |

The bypass is needed because the CLI talks to the deploy gateway with bearer tokens
(`DEPLOY_TOKEN`, or API tokens minted through the browser login), and upload URLs are signed.
The Worker authenticates those requests itself. Every other path requires an Access login.
The Worker also verifies the Access JWT (`AUTH_MODE=access`), so the `workers.dev` URL is
protected too.

## Usage

```sh
# 1. Deploy the Worker (workers.dev URL, AUTH_MODE=none).
cd deploy/cloudflare && npm ci && npm run setup

# 2. Domain + Access.
cd ../terraform/cloudflare/examples/basic
cp terraform.tfvars.example terraform.tfvars   # account_id, zone_name, base_domain, who may sign in
export CLOUDFLARE_API_TOKEN=…
terraform init && terraform apply

# 3. Point the Worker at the domain and turn on Access verification (remembered by setup).
terraform output -raw setup_command
#   npm run setup -- --worker-name backyard --var ACCESS_AUD:… --var ACCESS_TEAM_DOMAIN:acme.cloudflareaccess.com
#                    --var AUTH_MODE:access --var BASE_DOMAIN:backyard.example.com --var URL_MODE:path
cd ../../../../cloudflare && <run it>

# 4. Sign in from the CLI (browser login through Access).
backyard login --server https://backyard.example.com
```

```hcl
module "backyard" {
  source = "git::https://github.com/thasami/backyard.git//deploy/terraform/cloudflare?ref=v0.1.0"

  account_id  = "0123456789abcdef0123456789abcdef"
  zone_name   = "example.com"
  base_domain = "backyard.example.com"
  url_mode    = "path"                       # or "host" for <site>.<base_domain>

  allowed_email_domains = ["example.com"]

  # Okta (optional): creates the identity provider and allows these groups.
  okta_account_url    = "https://example.okta.com"
  okta_client_id      = var.okta_client_id
  okta_client_secret  = var.okta_client_secret
  allowed_okta_groups = ["backyard-users"]
}
```

API token permissions:

- Zone: **DNS Edit** and **Workers Routes Edit** (plus **SSL and Certificates Edit** for
  `advanced_certificate`).
- Account: **Workers Scripts Edit**, **Access: Apps and Policies Edit**, and
  **Access: Organizations, Identity Providers, and Groups** (Edit when creating the Okta
  provider, Read otherwise).

### Okta setup

1. In Okta, create an app integration of type **OIDC, Web Application**:
   - Sign-in redirect URI: `https://<team>.cloudflareaccess.com/cdn-cgi/access/callback`
   - **Sign On** tab → OpenID Connect ID Token → Groups claim: type *Filter*, name `groups`,
     *Matches regex* `.*`.
   - Assign it to the people or groups who should use Backyard.
2. Set `okta_account_url`, `okta_client_id`, `okta_client_secret` (`TF_VAR_okta_client_secret`
   keeps it out of files) and `allowed_okta_groups`.
3. To make an Okta group admin of Backyard (admins can delete any site), run
   `npm run setup -- --var ADMIN_GROUPS:backyard-admins`.

With an Okta provider that already exists, pass `okta_identity_provider_id` instead of the
`okta_*` credentials.

### Host mode and certificates

Universal SSL (free) covers `<zone>` and `*.<zone>`, which is one wildcard level. Sites at
`<site>.example.com` therefore work for free with `base_domain = "example.com"`. Sites at
`<site>.backyard.example.com` need `advanced_certificate = true`, which uses Advanced
Certificate Manager (paid). A `check` block warns when neither applies.

`wrangler deploy` only replaces a Worker's routes when `wrangler.jsonc` lists some, so leave
`routes` out of `wrangler.jsonc` while this module manages them.

## Inputs

| Name | Default | Description |
| --- | --- | --- |
| `account_id` | — | Cloudflare account id. |
| `zone_name` | — | Zone containing `base_domain`. |
| `base_domain` | — | Hub hostname (`backyard.example.com`, or the zone apex). |
| `url_mode` | `"path"` | `path` or `host`. |
| `worker_name` | `"backyard"` | Deployed Worker. |
| `manage_dns` | `true` | Host mode: create the proxied wildcard and hub records. |
| `dns_record_content` | `"100::"` | Target of those records (originless placeholder). |
| `advanced_certificate` / `certificate_authority` | `false` / `"google"` | Advanced certificate for `*.base_domain`. |
| `enable_access` | `true` | Create the Access applications and policies. |
| `access_app_name` / `access_session_duration` | `"Backyard"` / `"24h"` | Application name and login lifetime. |
| `allowed_emails`, `allowed_email_domains`, `allowed_access_group_ids`, `allowed_okta_groups` | `[]` | Who may sign in (any match). At least one is required with Access. |
| `identity_provider_ids` | `[]` | Login methods to offer (empty = all of the account's). |
| `auto_redirect_to_identity` | auto | Skip the Access login page. Defaults to on when exactly one IdP is allowed. |
| `bypass_paths` | gateway + health | Paths below `/__backyard/` that skip Access. |
| `okta_account_url`, `okta_client_id`, `okta_client_secret`, `okta_authorization_server_id`, `okta_idp_name` | `""` | Create an Okta identity provider. |
| `okta_identity_provider_id` | `""` | Existing Okta provider for `allowed_okta_groups`. |

## Outputs

| Name | Description |
| --- | --- |
| `public_url`, `gateway_url`, `site_url_pattern` | Where the hub, deploy gateway and sites live. |
| `access_aud` | AUD tag of the Access application, used as the Worker's `ACCESS_AUD`. |
| `access_team_domain` | `<team>.cloudflareaccess.com`, used as the Worker's `ACCESS_TEAM_DOMAIN`. |
| `okta_identity_provider_id` | Okta provider id (created or passed in). |
| `worker_vars` | Map of the Worker variables for this setup. |
| `setup_command` | The `npm run setup -- --var …` command that applies them. |

## Tests

```sh
terraform init -backend=false && terraform validate
terraform test        # offline: the provider is mocked, nothing is created
```
