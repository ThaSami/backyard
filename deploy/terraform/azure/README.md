# Backyard on Azure (Terraform)

This module deploys [Backyard](../../../docs/spec/SPEC.md) on Azure: the
single Go binary on **Azure Container Apps**, site files and uploads in **Blob
Storage**, documents and realtime fan-out in **Azure Database for PostgreSQL
flexible server**, login through **Entra ID** (or Okta / any OIDC IdP), and,
optionally, keyless **Azure OpenAI**. **Azure Front Door Standard** serves the
wildcard domain that host-mode sites need (`https://<site>.backyard.acme.com`).

```
                       ┌──────────── Entra ID / Okta (OIDC) ────────────┐
                       │                                                │
browser ─TLS─▶ Azure Front Door Standard ─HTTPS─▶ Container Apps ingress ─▶ backyard serve (:8080)
   backyard.acme.com     (apex + *.backyard.acme.com,   (Host = app FQDN,         │  managed identity (AZURE_CLIENT_ID)
   foo.backyard.acme.com  Key Vault or managed cert,    X-Forwarded-Host =        ├─▶ Blob Storage (private container,
                          no caching, websockets)       original host)            │    user-delegation SAS for uploads)
                                                                                  ├─▶ PostgreSQL 16 flexible server
CLI / agent ─▶ https://backyard.acme.com/__backyard/api/deploy ───presigned PUT───┘    (database + LISTEN/NOTIFY pub/sub)
                                                                                  └─▶ Azure OpenAI (Entra ID, no keys)
```

What gets created:

| Area | Resources |
|---|---|
| Base | resource group (or an existing one), Log Analytics workspace (or an existing one) |
| Identity | user-assigned identity for the app (blob data + delegator, OpenAI user, AcrPull); a separate *edge* identity for Front Door / Container Apps to read the TLS certificate from Key Vault |
| Compute | Container Apps environment (workload profiles, Consumption) + container app: external ingress on 8080, transport `auto` (HTTP/1.1, HTTP/2, websockets), 1–N replicas with an HTTP concurrency scale rule, startup/liveness/readiness probes on `/__backyard/healthz` |
| Edge | Front Door Standard profile, endpoint, origin (the container app), route without caching, custom domains `<domain>` and `*.<domain>`, Key Vault certificate secret — or a custom domain bound directly on the container app (`edge = "containerapp"`) |
| Storage | StorageV2 account (TLS 1.2, Entra-only: shared keys disabled, no public blobs, soft delete), private container, CORS for presigned PUTs from the hub |
| Database | PostgreSQL 16 flexible server, `backyard` database, generated password, firewall (*allow Azure services*) or VNet integration (delegated subnet + private DNS zone) |
| Auth | Entra ID app registration (redirect URI, group claims, client secret, optional assignment/consent/device flow) — or your existing IdP app |
| AI | optional Azure OpenAI account + model deployments (keyless), optional Anthropic/OpenAI keys |
| DNS | optional Azure DNS records (hub, wildcard, validation); otherwise exported as `dns_records` |

## Why Front Door for the wildcard domain

Backyard's default **host** URL mode serves each site on its own subdomain, so
the platform needs a wildcard hostname (`*.backyard.acme.com`) with a wildcard
certificate in front of a single application.

* **Azure Container Apps** binds custom domains per hostname. Its documentation
  ([custom domains and certificates](https://learn.microsoft.com/azure/container-apps/custom-domains-certificates))
  covers exactly two domain types — *apex* (A record + `asuid` TXT) and
  *subdomain* (CNAME + `asuid.<subdomain>` TXT) — with an uploaded PFX or a
  Key Vault certificate, and documents no wildcard hostname binding. The
  environment-level [custom DNS suffix](https://learn.microsoft.com/azure/container-apps/environment-custom-dns-suffix)
  does take a wildcard certificate, but it maps `<app-name>.<suffix>` to
  *different apps*, not every subdomain to one app.
* **Azure Front Door Standard** documents
  [wildcard domains](https://learn.microsoft.com/azure/frontdoor/front-door-wildcard-domain)
  with a bring-your-own certificate from Key Vault (read through a managed
  identity, automatically following renewals when the *latest* version is
  used) or a Front Door managed certificate, and supports
  [WebSockets](https://learn.microsoft.com/azure/frontdoor/standard-premium/websocket)
  without extra configuration.

So, in host mode, Front Door terminates `<domain>` and `*.<domain>` and
forwards to the container app's default `*.azurecontainerapps.io` FQDN.
Container Apps routes on the `Host` header, so Front Door rewrites `Host` to
that FQDN and passes the browser's host in `X-Forwarded-Host` (Front Door
always overwrites that header, so clients cannot spoof it). Backyard honours
`X-Forwarded-Host` only from `server.trusted_proxies`; the module trusts the
private ranges the Container Apps ingress connects from (RFC 1918, CGNAT
`100.64.0.0/10`, loopback, and the infrastructure subnet it creates with
`vnet_integration`) plus — for client-IP resolution in `X-Forwarded-For` —
the `AzureFrontDoor.Backend` service-tag ranges. With your own subnets
outside RFC 1918 space, add their CIDRs to `extra_trusted_proxies`. Because
the ingress itself is trusted, a client that calls the container app's
default hostname directly can choose the host Backyard sees; that grants
nothing a request through Front Door would not (browser sessions are cookies
on `.<domain>`), and `frontdoor_restrict_origin` closes the path.

Trade-offs of the Front Door edge (all documented by Microsoft):

* **WebSockets:** 5-minute idle timeout (Backyard pings every 25 s), connections
  are dropped after at most 2 hours (the SDK reconnects and re-subscribes), and
  a Standard profile supports about 3,000 concurrent websocket connections.
* **Server-Sent Events are not supported**, so `backyard.ai.stream()` responses
  arrive in one piece instead of token by token. Non-streaming AI calls are
  unaffected (`frontdoor_response_timeout_seconds` defaults to the 240 s max).
* **Managed certificates** are validated with `_dnsauth` TXT records. Front Door
  does *not* rotate managed wildcard certificates automatically, nor managed
  certificates on a zone apex served through an A/alias record or CNAME
  flattening: 45 days before expiry the domain goes to *Pending revalidation*
  and needs a fresh validation token (shown in the portal; with
  `dns_zone_name`, `terraform apply` publishes the token Azure currently
  reports). Use a Key Vault certificate (`tls_certificate`) in host mode — plans
  print a warning (check `frontdoor_wildcard_certificate`) until you do.
* Front Door adds a fixed monthly base fee.

If none of this fits, use **path mode** (`url_mode = "path"`, sites at
`https://<domain>/s/<site>/`): with `edge = "containerapp"` the domain is bound
directly on the container app (Host preserved, streaming works, no Front Door);
with `domain = null` Backyard runs on the container app's own
`*.azurecontainerapps.io` hostname — handy for a lab.

| `url_mode` | `domain` | `edge` | Result |
|---|---|---|---|
| `host` (default) | set | `frontdoor` (default) | Front Door: `<domain>` + `*.<domain>` |
| `path` | set | `frontdoor` | Front Door: `<domain>` only |
| `path` | set | `containerapp` | domain bound on the container app (needs `tls_certificate`) |
| `path` | `null` | – | `https://<app>.<env>.<region>.azurecontainerapps.io` |

## Prerequisites

* Terraform >= 1.6 to use the module; running `terraform init`/`validate`/`test`
  inside this directory needs >= 1.7 because `tests/` uses mock providers.
  Providers: `hashicorp/azurerm` 5.x, `hashicorp/azuread` 3.x, `Azure/azapi`
  2.x (only for sticky sessions), `hashicorp/random`, `hashicorp/time`.
* An identity that can create resources **and role assignments** in the target
  subscription/resource group (Owner, or Contributor + Role Based Access
  Control Administrator), and — for the generated Entra ID application — create
  app registrations (e.g. *Application Developer*).
* azurerm 5.x no longer registers resource providers automatically. Register
  `Microsoft.App`, `Microsoft.Cdn`, `Microsoft.CognitiveServices`,
  `Microsoft.DBforPostgreSQL`, `Microsoft.KeyVault`, `Microsoft.ManagedIdentity`,
  `Microsoft.Network`, `Microsoft.OperationalInsights` and `Microsoft.Storage`
  once, or list them in the provider's `resource_providers_to_register` (see
  [examples/basic](examples/basic/main.tf)).
* Set `storage_use_azuread = true` on the azurerm provider: the storage account
  has shared keys disabled.

## Quick start

```hcl
module "backyard" {
  source = "git::https://github.com/thasami/backyard.git//deploy/terraform/azure?ref=v0.2.0"

  location      = "westeurope"
  domain        = "backyard.acme.com"
  dns_zone_name = "acme.com"            # Azure DNS zone; omit to create records yourself
  admins        = ["alice@acme.com"]

  tls_certificate = {                   # recommended in host mode
    key_vault_id     = "/subscriptions/…/resourceGroups/security/providers/Microsoft.KeyVault/vaults/acme-tls"
    certificate_name = "backyard-wildcard" # SANs: backyard.acme.com, *.backyard.acme.com
  }
}
```

```sh
terraform init && terraform apply
terraform output dns_records            # without dns_zone_name: create these records
$(terraform output -raw cli_login_command)
backyard init hello && cd hello && backyard deploy
```

The first apply takes 10–20 minutes (Postgres, Front Door domain validation).
With managed certificates, HTTPS on the custom domains starts working once
Front Door has validated the `_dnsauth` records and issued the certificates.

## DNS and TLS

**Records.** With `dns_zone_name` (a public Azure DNS zone; `domain` must be the
zone itself or a name in it) the module creates:

| edge | Record | Target |
|---|---|---|
| `frontdoor` | `<domain>` | CNAME → Front Door endpoint (alias **A** record at a zone apex) |
| `frontdoor`, host mode | `*.<domain>` | CNAME → Front Door endpoint |
| `frontdoor`, managed certificate | `_dnsauth.<domain>` | TXT: the validation tokens of both custom domains |
| `containerapp` | `<domain>` | CNAME → container app FQDN (A → environment static IP at a zone apex) |
| `containerapp` | `asuid.<domain>` | TXT: Container Apps domain verification ID |

Without `dns_zone_name` the same list is in the `dns_records` output (FQDNs; at
a zone apex use your provider's ALIAS/ANAME/CNAME flattening for Front Door).
Front Door validates domains asynchronously (up to ~15 minutes after the TXT
record appears; the portal shows the exact expected record if validation stays
*Pending*). With a Key Vault certificate, Front Door approves the domains
covered by the certificate and no TXT record is needed. Managed certificates
are issued by DigiCert: if the zone has CAA records, add `0 issue "digicert.com"`.

**Bring your own certificate** (`tls_certificate`): import a PFX (RSA, full
chain) into a Key Vault in the **same subscription**, covering `<domain>` and,
in host mode, `*.<domain>`. The module grants the edge identity read access —
*Key Vault Secrets User* on the certificate's secret for RBAC vaults, a Get
access policy for access-policy vaults (`tls_key_vault_grant_access`) — and
references the **versionless** certificate, so renewing it in Key Vault rotates
it at the edge automatically (Front Door within 72 hours; Container Apps within
12 hours). The identity running Terraform needs read access to the certificate
metadata. If the vault has a firewall, allow trusted Microsoft services.

**edge `containerapp`** (path mode): Azure validates DNS before it binds a
hostname. With `dns_zone_name` everything happens in one apply. Without it,
the first apply creates the app and leaves the domain unbound
(`bind_custom_domain` defaults to binding only when DNS is managed); create the
two records from `dns_records`, then apply again with `bind_custom_domain = true`.

## Authentication

`auth_mode = "oidc"` (default) uses Backyard's built-in OIDC login
(authorization code + PKCE; session cookie on `.<domain>`).

* **Entra ID (default).** With no `oidc_issuer` the module creates an app
  registration (single tenant) with redirect URI
  `https://<domain>/__backyard/auth/callback`, the optional `email` claim, group
  claims (`entra_group_membership_claims`, default `SecurityGroup`), delegated
  Microsoft Graph permissions `openid profile email offline_access User.Read`,
  a client secret (stored as a Container Apps secret; rotate with
  `entra_client_secret_rotation`) and an enterprise application.
  * Group claims are **object IDs** — put object IDs in `admin_groups` /
    `allowed_groups`. For display names use
    `entra_group_membership_claims = ["ApplicationGroup"]` with
    `entra_group_claim_names = true` and assign the groups to the app (this also
    avoids Entra's 200-group overage limit).
  * `entra_assignment_required = true` + `entra_assigned_principal_object_ids`
    restricts sign-in to assigned users/groups.
  * `entra_grant_admin_consent = true` pre-consents the Graph permissions
    (needs a privileged Entra role); otherwise users consent on first login.
  * `entra_enable_device_flow = true` enables `backyard login --device` for
    headless machines (public client flows on the app).
  * Users without a `mail` attribute have no `email` claim and are identified by
    `sub`; to use the UPN instead add
    `extra_config_yaml = "identity:\n  claims:\n    email: preferred_username\n"`.
* **Okta, Google, Auth0, Keycloak or an existing Entra app.** Set `oidc_issuer`,
  `oidc_client_id` and `oidc_client_secret`, and register the `oidc_redirect_uri`
  output at the IdP. Okta: issuer `https://<org>.okta.com/oauth2/default`, add a
  `groups` claim to the authorization server (or set `oidc_use_userinfo = true`),
  and `oidc_device_client_id` for a native app enabling device flow.
* **`auth_mode = "none"`** gives everyone an anonymous identity — labs only.
  Deploys then need a static token (`static_tokens`) unless
  `allow_anonymous_deploy = true`.

CI can deploy with static tokens: `backyard token hash <token>` prints the
SHA-256 that goes into `static_tokens`; the token itself never reaches
Terraform.

## Claude connector

The [Claude connector](../../../docs/claude-connector.md) lets people publish sites from Claude
with their Entra ID or Okta sign-in. Turn it on with
`extra_config_yaml = "mcp:\n  enabled: true\n"`; the connector URL is
`https://<domain>/__backyard/mcp`. Front Door and Container Apps ingress do not sign people in
(Backyard's own OIDC login does), so there are no proxy exemptions to add, and the IdP
application stays as it is. A WAF policy attached to the Front Door profile must not inspect
or cap the request body of `/__backyard/mcp` below 10 MiB (publish calls carry up to 9 MiB),
and must admit Anthropic's `160.79.104.0/21` (IPv4) if it restricts sources. By default
Backyard fetches Claude's client metadata from `https://claude.ai` at the first connect; without
outbound internet access add `clients: []` to the `mcp` section.

## Storage

Backyard uses its managed identity (`AZURE_CLIENT_ID`, picked up by
`DefaultAzureCredential`) with **Storage Blob Data Contributor** on the
container and **Storage Blob Delegator** on the account, and signs presigned
upload/download URLs with user delegation keys — no account keys anywhere. The
blob service has a CORS rule for the hub origin so the hub's drag-and-drop
deploy can PUT straight to Blob Storage (`storage_cors_extra_origins` adds more).

Direct deploys (`backyard deploy --method azure`, which runs
`az storage blob sync`) need **Storage Blob Data Contributor** on the container
for the deploying users/groups/CI identities (`storage_deployer_principal_ids`)
and Entra auth in the Azure CLI: `export AZURE_STORAGE_AUTH_MODE=login`. The
CLI calls the gateway's refresh endpoint afterwards so the hub picks up the
change. The module adds an ABAC condition to those role assignments: blob
reads, writes, deletes and listings are limited to `<storage_prefix>sites/`, so
deployers can publish any site's files but cannot read or change `_meta/`
(site owners, manifests) or anyone's uploads. Conditions do not cover the
role's container-level permissions (a deployer could still delete the whole
container); container soft delete (`storage_soft_delete_days`, 7 by default)
makes that recoverable.

## Database and networking

PostgreSQL 16 flexible server (`postgres_sku_name`, default `B_Standard_B2s`)
backs both `database.type: postgres` and `pubsub.type: postgres`
(LISTEN/NOTIFY), so any number of replicas share documents, realtime events
and presence. Each replica opens up to `postgres_max_conns_per_replica` (10)
connections plus one for pub/sub — keep `max_replicas × 11` well below the
SKU's `max_connections`. Use a General Purpose SKU with
`postgres_high_availability = true` for zone-redundant HA.

* **Public mode (default):** the server has a public endpoint with the
  *allow Azure services* firewall rule (`postgres_allow_azure_services`), which
  is how Container Apps without a VNet (no stable outbound IP) reach it. That
  rule admits **every Azure IP, including other customers' subscriptions**:
  only TLS (`sslmode=require`) and the generated 40-character password protect
  the admin login, so a leaked `DATABASE_URL` or Terraform state can be used from
  any VM in Azure. The plan warns about it (check
  `postgres_open_to_all_of_azure`); use VNet integration for production.
* **VNet integration (`vnet_integration = true`):** the module creates a
  virtual network with a Container Apps subnet (`/23` default, `/27` minimum,
  delegated to `Microsoft.App/environments`) and a Postgres subnet delegated to
  `Microsoft.DBforPostgreSQL/flexibleServers`, plus a private DNS zone; the
  server has no public endpoint. Bring your own subnets with
  `existing_container_apps_subnet_id` / `existing_postgres_subnet_id` (and
  `existing_postgres_private_dns_zone_id`). `zone_redundant = true` makes the
  Container Apps environment zone redundant (requires VNet integration).
* **Existing Postgres:** `database_url = "postgres://…?sslmode=require"` skips
  the server. Backyard holds a `LISTEN` connection and migration advisory
  locks, so connect directly (or through a *session*-mode pooler) — not
  through a transaction-mode PgBouncer such as the flexible server's built-in
  one on port 6432.

## AI

`azure_openai_enabled = true` creates an Azure OpenAI account with local
(key) auth disabled; the app identity gets **Cognitive Services OpenAI User**
and Backyard's `azure-openai` provider authenticates with Entra ID. Each entry
of `azure_openai_deployments` becomes a deployment whose name is the Backyard
model alias (defaults: `smart` = gpt-5.4 `2026-03-05`, `fast` = gpt-5.4-mini
`2026-03-17`, `embed` = text-embedding-3-small `1`, all GlobalStandard; these
versions retire in late 2027 / early 2028 per Azure's model retirement
schedule). Model names, versions, SKUs and quota differ per region — adjust
them (and `azure_openai_location`) to what your subscription offers.

`anthropic_api_key` / `openai_api_key` (Container Apps secrets) enable the
`anthropic` / `openai` providers for aliases in `ai_models`; `ai_default_model`
and friends pick defaults:

```hcl
anthropic_api_key = var.anthropic_api_key
ai_models = {
  smart = { provider = "anthropic", model = "claude-opus-5-5", description = "Claude", effort = "high" }
}
```

## Configuration and secrets

The module renders `backyard.yaml` from
[`templates/backyard.yaml.tftpl`](templates/backyard.yaml.tftpl) into the
`BACKYARD_CONFIG_YAML` environment variable (also the `backyard_config_yaml`
output). Backyard decodes it strictly, so the template only uses keys from
`internal/config/config.go`. The YAML never contains secrets: Container Apps
secrets are injected as environment variables — `BACKYARD_SECRET` (random,
signs sessions/tokens) and `DATABASE_URL` are read by Backyard directly (the
latter fills `database.postgres.dsn`, which pub/sub reuses), while
`OIDC_CLIENT_SECRET`, `ANTHROPIC_API_KEY` and `OPENAI_API_KEY` are referenced
from the YAML as `'${VAR}'` and expanded at startup (so they must not contain
single quotes or line breaks; the variables validate this).

* `extra_config_yaml` appends sections the module does not render
  (`identity`, `warehouse`, `sites`, `files`, `ratelimit`, `realtime`, `mcp`), e.g.
  `sites:\n  ownership: owner\n`. Reference secrets as `${VAR}` and pass them
  with `extra_secret_env` (e.g. a warehouse DSN); plain values go in `extra_env`.
* Terraform state contains the generated secrets — keep it in a private,
  encrypted backend.
* Rotating `BACKYARD_SECRET` (`terraform apply -replace=module.backyard.random_password.backyard_secret`)
  signs everyone out and revokes all API tokens.

## Operations

* **Upgrades:** pin `image` (tag or digest) and change it to roll out a new
  revision (single revision mode, zero downtime with `min_replicas >= 2`).
  Private registries: `image_registry` (ACR via the app identity with AcrPull,
  or username + `image_registry_password`, which is then required).
* **Scaling:** `min_replicas` (keep >= 1), `max_replicas`,
  `concurrent_requests_per_replica`, `cpu`/`memory` (Consumption profile: memory
  = 2 × cpu). Backyard is stateless; `sticky_sessions` exists but is not needed.
* **Logs:** container logs go to Log Analytics
  (`ContainerAppConsoleLogs_CL | where ContainerAppName_s == "backyard"`).
  Backyard logs JSON (`log_level`, `access_log`).
* **Hardening:** attach a WAF policy to the Front Door profile
  (`frontdoor_profile_id` output). `frontdoor_restrict_origin = true` limits the
  container app's ingress to the `AzureFrontDoor.Backend` IPv4 ranges so the
  default `*.azurecontainerapps.io` hostname cannot be reached directly (the
  list is read at plan time; re-apply periodically to pick up changes). Those
  ranges are shared by every Front Door profile, and Container Apps cannot
  check the `X-Azure-FDID` header, so another tenant's Front Door could still
  reach the origin (bypassing your WAF, not authentication). Changes to the
  service-tag list also change `server.trusted_proxies`, which rolls out a
  new revision. Set `vnet_integration = true` so Postgres has no public
  endpoint: in the default public mode its firewall admits all of Azure (see
  [Database and networking](#database-and-networking)).
* **Verify the edge** after the first apply: `curl -s https://<domain>/__backyard/api/info`
  should report your `public_url`, and a deployed site must load at
  `https://<site>.<domain>/`. If sites answer with the hub or a "not found"
  page instead, the original host is not reaching Backyard. With
  `access_log = true`, requests then log `host` = the container app FQDN and
  `ip` = the untrusted ingress peer; add that range to `extra_trusted_proxies`.

## Inputs (main ones)

All inputs, with validation, are in [variables.tf](variables.tf).

| Name | Default | Description |
|---|---|---|
| `location` | – | Azure region |
| `name` | `backyard` | name prefix (2–20 chars) |
| `domain` | `null` | base domain; `null` = container app hostname (path mode) |
| `url_mode` | `host` | `host` or `path` |
| `edge` | `frontdoor` | `frontdoor` or `containerapp` (path mode only) |
| `tls_certificate` | `null` | `{ key_vault_id, certificate_name }`; `null` = Front Door managed certificates |
| `dns_zone_name` | `null` | Azure DNS zone to manage records in |
| `auth_mode` | `oidc` | `oidc` or `none` |
| `oidc_issuer` / `oidc_client_id` / `oidc_client_secret` | `null` | existing IdP app (Okta, …); `null` = generated Entra ID app |
| `admins`, `admin_groups`, `allowed_domains`, `allowed_emails`, `allowed_groups` | `[]` | access control |
| `static_tokens` | `[]` | CI tokens (`name`, `sha256`, `admin`) |
| `image` | `ghcr.io/thasami/backyard:latest` | container image |
| `cpu` / `memory` | `0.5` / `1Gi` | per replica |
| `min_replicas` / `max_replicas` | `1` / `5` | scaling bounds |
| `sticky_sessions` | `false` | Container Apps session affinity (via azapi) |
| `vnet_integration` | `false` | private Postgres + Container Apps in a VNet |
| `database_url` | `null` | use an existing Postgres instead of creating one |
| `postgres_sku_name` / `postgres_version` | `B_Standard_B2s` / `16` | flexible server |
| `storage_replication_type` | `LRS` | prefer `ZRS`/`GZRS` where available |
| `storage_deployer_principal_ids` | `[]` | principals allowed to `az storage blob sync` |
| `azure_openai_enabled` | `false` | create Azure OpenAI + `azure_openai_deployments` |
| `anthropic_api_key` / `openai_api_key` / `ai_models` | `null` / `{}` | other AI providers and aliases |
| `extra_config_yaml` | `""` | extra backyard.yaml sections |

## Outputs

| Name | Description |
|---|---|
| `hub_url` | `https://<domain>` |
| `cli_login_command` | `backyard login --server https://<domain>` |
| `container_app_fqdn` | default `*.azurecontainerapps.io` hostname (Front Door origin) |
| `frontdoor_endpoint_hostname` | `*.azurefd.net` target of the CNAMEs |
| `dns_records` | records to create (`name`, `type`, `value`, `purpose`) |
| `storage_account_name` / `storage_container_name` | blob storage for sites and uploads |
| `managed_identity_client_id` / `managed_identity_principal_id` | the app identity |
| `postgres_fqdn` | flexible server hostname |
| `entra_application_client_id` / `oidc_issuer` / `oidc_redirect_uri` | login configuration |
| `azure_openai_endpoint` / `ai_models` | AI configuration |
| `backyard_config_yaml` | rendered configuration (no secrets) |

## Testing

```sh
cd deploy/terraform/azure
terraform fmt -recursive -check
terraform init -backend=false && terraform validate
terraform test          # offline: azurerm/azuread/azapi/time are mocked
```

`tests/module.tftest.hcl` plans and applies (against mocks) the main
combinations — Front Door host mode with Entra ID and managed certificates
(including the renewal warning), Key Vault certificates with origin
restriction and sticky sessions, `edge = "containerapp"` with Okta, an
existing database and VNet integration, existing subnets in peered VNets, an
existing resource group with an ACR image, and a lab without a domain using
Azure OpenAI — asserting the resources (and that no credentials are rendered
into `backyard.yaml`), and checks that invalid inputs fail at plan time.

Backyard decodes `backyard.yaml` strictly, so a renamed or removed key breaks
startup. `configcheck/` therefore loads the configuration every run gives the
Container App with Backyard's own config loader, in the container's
environment (CI runs it; `make tf-configcheck` runs all modules'):

```sh
BACKYARD_TEST_TERRAFORM=1 go test ./deploy/terraform/azure/configcheck/   # from the repository root
```
