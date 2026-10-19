# Terraform: Backyard on Cloudflare (domain + Access)

**Optional.** You don't need Terraform to run the Backyard Workers edition
([`deploy/cloudflare`](../../cloudflare)): `backyard cloudflare domain add <host> [--host-mode]` and
`backyard cloudflare access enable` (or `npx backyard-cloudflare@latest …`, once a Backyard
release has published it to npm) put the Worker on your own domain and behind Cloudflare
Access from the terminal, and check the result. Use this module if you manage Cloudflare as
infrastructure as code anyway.

It puts the Worker on your own domain and, optionally, behind **Cloudflare Access**. Access is
free for up to 50 users and supports Okta, Entra ID, Google Workspace, GitHub, SAML/OIDC and
one-time PIN.

**One owner per Worker.** Manage a Worker's domain and Access either with this module or with
the CLI commands, not both. Their objects have different names (the module's Access
applications and policies are called `Backyard…` after `access_app_name`, the CLI's
`Backyard (<worker>)…`), and with an API token `access enable` refuses (exit 3) to change an
Access application it did not create, such as this module's.

The module does **not** deploy the Worker's code. `backyard cloudflare setup` does that (or
`npx backyard-cloudflare@latest setup` once a Backyard release has published it to npm, or
`npm run setup` in a checkout of `deploy/cloudflare`; Node.js 22 or later; see
[the edition's quickstart](../../cloudflare/README.md#quickstart-free-plan)), and it keeps the
Durable Objects, secrets and variables. Terraform manages what sits around the Worker:

| Resource | When |
| --- | --- |
| `cloudflare_workers_custom_domain` for `base_domain` (DNS record and certificate included) | `url_mode = "path"` |
| Proxied `AAAA` records `base_domain` and `*.base_domain` → `100::` | `url_mode = "host"` and `manage_dns` |
| `cloudflare_workers_route` for `base_domain/*` and `*.base_domain/*` | `url_mode = "host"` |
| `cloudflare_certificate_pack` (advanced) for `*.base_domain` | host mode below the apex with `advanced_certificate` (paid ACM) |
| Access application over the hub (and `*.base_domain` in host mode) with an allow policy | `enable_access` (default) |
| Access application with a bypass policy for `/__backyard/api/deploy`, `/__backyard/api/deploy/*`, `/__backyard/api/tokens`, `/__backyard/healthz`, `/__backyard/readyz` and the Claude connector's `/__backyard/mcp`, `/__backyard/oauth/*`, `/.well-known/oauth-authorization-server` and `/.well-known/oauth-protected-resource/__backyard/mcp` (on the hub host only) | `enable_access` |
| Okta identity provider | `okta_account_url` set |

The bypass is needed because the CLI talks to the deploy gateway with bearer tokens
(`DEPLOY_TOKEN`, or API tokens minted through the browser login), and upload URLs are signed.
`backyard token create` likewise sends its API token to `/__backyard/api/tokens`; a token
minted that way never expires later than the one that asked for it, so a token cannot renew
itself: chaining tokens never keeps access past the token that `backyard login` minted through
Access. The Worker authenticates those requests itself: bearer tokens, or the
`CF_Authorization` cookie, whose Access JWT it verifies (browser requests on these paths carry
no `Cf-Access-Jwt-Assertion`). The [Claude connector](../../../docs/claude-connector.md)'s
four machine paths check the connector's own tokens and never read cookies or Access headers;
until `mcp_enabled` they answer `404`. Every other path requires an Access login,
`/__backyard/auth/authorize` (the connector's sign-in) and the sites' `/.well-known` included.
The Worker also verifies the Access JWT (`AUTH_MODE=access`), so the `workers.dev` URL is
protected too. After upgrading the module, `terraform apply` adds the connector's paths to the
bypass application.

## Usage

```sh
# 1. Deploy the Worker (workers.dev URL, AUTH_MODE=none).
backyard cloudflare setup               # or, once it is on npm: npx backyard-cloudflare@latest setup
#   add --no-ai when ai_enabled = false (e.g. no Workers AI on the account)

# 2. Domain + Access.
cd deploy/terraform/cloudflare/examples/basic
cp terraform.tfvars.example terraform.tfvars   # account_id, zone_name, base_domain, who may sign in
export CLOUDFLARE_API_TOKEN=…
terraform init && terraform apply

# 3. Point the Worker at the domain and turn on Access verification (remembered by setup).
terraform output -raw setup_command
#   npm run setup -- --worker-name backyard --ai --site-listing all --var ACCESS_AUD:…
#                    --var ACCESS_TEAM_DOMAIN:acme.cloudflareaccess.com --var AUTH_MODE:access
#                    --var BASE_DOMAIN:backyard.example.com --var MCP_ENABLED:false --var URL_MODE:path
cd ../../../../cloudflare && <run it>          # in a checkout of deploy/cloudflare

# 4. Sign in from the CLI (browser login through Access).
backyard login --server https://backyard.example.com
```

Without a checkout, step 3 uses the module's `setup_args` output, the same arguments without
`npm run setup --`. `examples/basic` exports it; in your own configuration, re-export it
(`output "setup_args" { value = module.backyard.setup_args }`), then:

```sh
backyard cloudflare setup $(terraform output -raw setup_args)
npx backyard-cloudflare@latest setup $(terraform output -raw setup_args)   # the same without the backyard CLI (once on npm)
```

They always include `--var MCP_ENABLED:true` or `--var MCP_ENABLED:false`: `mcp_enabled = true`
turns on the [Claude connector](../../../docs/claude-connector.md) (publishing from Claude with
the Access sign-in), and setting it back to `false` and running `setup` again turns it off (setup
keeps the variables it is not given, so the module never leaves this one out). Likewise they
always include `--ai` or `--no-ai`: `ai_enabled = false` deploys without the Workers AI binding
and with `AI_ENABLED=false`, so every AI call of the sites answers `501 unsupported` (for an
account where Workers AI is not available), and setting it back to `true` turns AI on again at
the next `setup` (setup remembers `--no-ai` until it is given `--ai`). And they always include
`--site-listing all` or `--site-listing owners`: `site_listing = "owners"` lists each site only to
its owners and admins in the hub, `GET /sites` and the Claude connector (it implies
`OWNERSHIP=owner`; it hides sites from the directory and is not access control, since anyone who
may open sites can still open one by its URL; a site without owners, such as one deployed
before, goes to its next signed-in deployer), and setting it back to `"all"` lists every site
again at the next `setup` and turns ownership off again, so anyone may overwrite any site, unless
`OWNERSHIP=owner` is set (`setup --var OWNERSHIP:owner`, remembered; see
[Owners-only site listing](../../cloudflare/README.md#owners-only-site-listing)).

Since `setup_args` always carry `--site-listing`, this module version needs the `backyard` CLI
or the `backyard-cloudflare` package (or a checkout) of the same release or newer to run them:
an older `setup` rejects `--site-listing` (`unknown option`, exit 2, nothing changed). Upgrade
the CLI together with the module `?ref=`, or use `npx backyard-cloudflare@latest`.

These variables are then managed by Terraform: `setup` deploys and remembers them, and the
CLI's `domain add` and `access enable` refuse to take them over (exit 2, with how to drop them
with `--var KEY:`).

```hcl
module "backyard" {
  source = "git::https://github.com/thasami/backyard.git//deploy/terraform/cloudflare?ref=v0.4.0"

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
   `backyard cloudflare setup --var ADMIN_GROUPS:backyard-admins`.

With an Okta provider that already exists, pass `okta_identity_provider_id` instead of the
`okta_*` credentials.

### Host mode and certificates

Universal SSL (free) covers `<zone>` and `*.<zone>`, which is one wildcard level. Sites at
`<site>.example.com` therefore work for free with `base_domain = "example.com"`. Sites at
`<site>.backyard.example.com` need `advanced_certificate = true`, which uses Advanced
Certificate Manager (paid). A `check` block warns when neither applies.

Backyard's deploys (`setup` and `scripts/cloudflare-ci.mjs`) never change routes or custom
domains, so they leave the module's alone. Leave `routes` out of `wrangler.jsonc` anyway: a plain
`wrangler deploy` of a `wrangler.jsonc` that lists routes would change them.

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
| `bypass_paths` | gateway + token minting + health + the Claude connector's four machine paths (nine paths) | Paths below `/__backyard/` (or one of the two OAuth discovery documents) that skip Access. |
| `mcp_enabled` | `false` | Turn on the [Claude connector](../../../docs/claude-connector.md): `MCP_ENABLED=true` in `worker_vars` and `setup_args` (`false` otherwise, which turns a connector that was on off at the next `setup`). |
| `ai_enabled` | `true` | Serve AI to sites: `--ai` in `setup_args`. `false` puts `--no-ai` there instead: no Workers AI binding and `AI_ENABLED=false`, so every AI endpoint answers `501 unsupported` (for an account without Workers AI); back to `true`, the next `setup` turns AI on again. |
| `site_listing` | `"all"` | Who sees each site in the site lists: `all` (everyone) or `owners` (its owners and admins; implies `OWNERSHIP=owner`, so only owners and admins may change a site that has owners; not access control). `setup_args` always carry `--site-listing <value>` (older `setup` rejects it), so `"all"` lists every site again at the next `setup`, and turns ownership off again unless `OWNERSHIP=owner` is set (`setup --var OWNERSHIP:owner`). |
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
| `setup_args` | The arguments of `setup` that apply them: `--worker-name <worker> --ai\|--no-ai --site-listing all\|owners --var KEY:VALUE …`, for `backyard cloudflare setup` or `npx backyard-cloudflare setup`. |
| `setup_command` | The `npm run setup -- <setup_args>` command that applies them in a checkout. |

## Tests

```sh
terraform init -backend=false && terraform validate
terraform test        # offline: the provider is mocked, nothing is created
```
