# Backyard on Google Cloud (Terraform)

This module deploys [Backyard](../../../docs/spec/SPEC.md) on Google Cloud the way
Shopify runs Quick: every request goes through **Identity-Aware Proxy (IAP)** on a
global HTTPS load balancer, so a folder of HTML becomes a URL that only people in
your company can open, and each site gets the built-in backend (database and
realtime, files, AI, BigQuery, websockets, identity).

What it creates:

- **Cloud Run** service running `backyard serve`. Ingress is limited to the load
  balancer, the default `run.app` URL is disabled, CPU is always allocated
  (websockets), session affinity is on and the request timeout is 60 minutes.
- **Global external Application Load Balancer** with a static IP, HTTP→HTTPS
  redirect, a TLS 1.2+ policy and a Google-managed **wildcard certificate** from
  Certificate Manager (DNS authorization).
- **IAP** on the main backend service, using the Google-managed OAuth client, with
  `roles/iap.httpsResourceAccessor` for `iap_members`. A second backend service
  without IAP serves the deploy gateway to CLIs and agents that present Backyard
  bearer tokens.
- **Cloud SQL for PostgreSQL 16** (documents, and realtime fan-out with
  LISTEN/NOTIFY). Cloud Run connects through the Cloud SQL connector over a Unix
  socket. You can use an existing PostgreSQL instead.
- **GCS bucket** for site files and uploads: private, uniform access, CORS for
  signed uploads, and optional versioning.
- **Secret Manager** secrets for the server secret, the database DSN, the OIDC
  client secret and AI keys.
- **Artifact Registry remote repository** that proxies `ghcr.io`, because Cloud
  Run cannot pull from GHCR directly.
- A **least-privilege runtime service account**, plus optional **Vertex AI**
  models (Gemini, and Claude on Vertex) and an optional **BigQuery** warehouse.
- **Cloud DNS** records when you pass `dns_zone_name`. Without it, the records are
  listed in the outputs.

## Architecture

```
                    DNS: backyard.example.com, *.backyard.example.com ─▶ global IP
                                              │
 browser ──HTTPS──▶ Global external Application Load Balancer (EXTERNAL_MANAGED)
 CLI / agent        │  certificate map: backyard.example.com + *.backyard.example.com
                    │  port 80 ─▶ 301 https://
                    │
                    │  URL map (route rules, all hosts)
                    │   ├─ /__backyard/healthz ─────────────────────────┐
                    │   ├─ /__backyard/api/deploy/info ─────────────────┤
                    │   ├─ /__backyard/api/deploy/uploads/* ────────────┤  backend "deploy"
                    │   ├─ /__backyard/api/deploy/* + Authorization ────┤  (no IAP; Backyard
                    │   ├─ Claude connector machine paths ──────────────┤   checks bearer tokens)
                    │   └─ everything else ─▶ backend "app" ─── IAP ────┤
                    │                         (Google sign-in, IAM:      │
                    │                          iap.httpsResourceAccessor)│
                    ▼                                                    ▼
          serverless NEG ─────────────────▶ Cloud Run "backyard" (ingress: internal + load balancer)
                                             │  env BACKYARD_CONFIG_YAML (rendered backyard.yaml)
                                             │  env BACKYARD_SECRET, DATABASE_URL, … ◀── Secret Manager
                                             │  runtime SA: backyard-runtime@
                                             ├─▶ GCS bucket   sites/<site>/…, uploads/…, _meta/…
                                             ├─▶ Cloud SQL    /cloudsql/<connection> (Unix socket)
                                             ├─▶ Vertex AI    Gemini, Claude (optional)
                                             └─▶ BigQuery     read-only warehouse (optional)
          image: ghcr.io/thasami/backyard ──▶ Artifact Registry remote repo ──▶ Cloud Run
```

In IAP mode, Backyard verifies the signed `X-Goog-IAP-JWT-Assertion` header on every
request. The expected audience is
`/projects/<project number>/global/backendServices/<app backend id>`, which the
module renders into `auth.iap.audience`.

## Prerequisites

- A Google Cloud **project with billing**. To use IAP with the Google-managed OAuth
  client, the project must belong to a **Google Cloud organization**: only that
  organization's users can sign in. For other setups, see *IAP notes* below.
- **Terraform 1.6 or later**, and credentials for the Google providers, for example
  `gcloud auth application-default login`.
- Permission to create the resources. Project **Owner** is enough. With narrower
  roles you need: Service Usage Admin, Cloud Run Admin, Compute Load Balancer Admin,
  Certificate Manager Owner, Cloud SQL Admin, Storage Admin, Secret Manager Admin,
  Artifact Registry Admin, Service Account Admin, Project IAM Admin, IAP Policy
  Admin, Service Account User, and DNS Administrator if the module manages DNS.
- A **domain** you control, for example `backyard.example.com`. In the default
  host mode, sites are served at `<site>.backyard.example.com`, so you need a
  wildcard record.
- For **Claude on Vertex AI**, enable the model (default `claude-opus-5-5`) in
  Vertex AI **Model Garden** once per project. To skip Claude, set
  `vertex_claude_model = ""`.
- The container image must start `backyard serve` by default (or set
  `container_args`) and listen on `$PORT`.

## Quickstart

```bash
cd deploy/terraform/gcp/examples/basic
cp terraform.tfvars.example terraform.tfvars   # set project_id, domain, iap_members
terraform init
terraform apply
terraform output dns_records                   # create these unless dns_zone_name is set
```

1. **DNS.** Create the records listed in `dns_records`: an `A` record for the apex
   and the wildcard pointing at `load_balancer_ip`, plus the `CNAME` for
   `_acme-challenge.<domain>`. The certificate is issued a few minutes after the
   CNAME resolves; until then HTTPS handshakes fail. Check progress with
   `gcloud certificate-manager certificates describe <name>-cert`.
2. Open `hub_url`. IAP asks you to sign in with Google, then the hub loads.
3. Sign in the CLI and deploy a site:

   ```bash
   backyard login --server https://backyard.example.com   # = terraform output cli_login_command
   backyard init hello && cd hello && backyard deploy
   ```

To use the module from your own configuration:

```hcl
module "backyard" {
  source = "github.com/thasami/backyard//deploy/terraform/gcp"

  project_id  = "acme-backyard"
  region      = "europe-west1"
  domain      = "backyard.acme.com"
  iap_members = ["domain:acme.com"]
  admins      = ["alice@acme.com"]

  dns_zone_name      = "acme-com"     # Cloud DNS zone serving acme.com (optional)
  bigquery_warehouse = true
  bigquery_datasets  = ["analytics", "shared-data.events"]
}
```

Pin a released image instead of relying on `latest`, for example
`image = "ghcr.io/thasami/backyard:0.1.0"`. Changing `image` rolls out a new
Cloud Run revision.

## IAP notes

**Who gets in.** `iap_members` is authoritative for
`roles/iap.httpsResourceAccessor` on the app backend service. Use
`domain:example.com`, `group:…` or `user:…`. Leave it empty to manage that
role outside this module (Terraform then warns that nobody has access yet).
You can also limit users inside Backyard with `allowed_domains` and
`allowed_emails`, and grant admin rights with `admins`.

**Google-managed vs. custom OAuth client.** By default IAP uses a Google-managed
OAuth client, so no consent screen or client setup is needed, but only users in
the project's organization can sign in. To admit external or consumer accounts,
create an OAuth client yourself and pass `iap_oauth2_client_id` and
`iap_oauth2_client_secret`.

**How CLIs and agents authenticate.** IAP only works for browsers, so CLIs use
Backyard API tokens:

1. `backyard login --server https://<domain>` opens
   `https://<domain>/__backyard/auth/cli?redirect_uri=http://127.0.0.1:<port>/callback`
   in your browser. The request goes through IAP like any other page.
2. Backyard knows who you are from the IAP assertion. It mints a Backyard API token
   (an HS256 JWT signed with a key derived from `BACKYARD_SECRET`, valid for 30
   days by default) and redirects to the CLI's loopback listener.
   `--device` is not available in IAP mode. On headless machines, open
   `/__backyard/auth/cli?mode=display` and paste the token with
   `backyard login --token …`.
3. Gateway calls such as `/__backyard/api/deploy/*`, and token requests to
   `/__backyard/api/tokens`, that carry an `Authorization` header are routed to
   the **deploy backend, which bypasses IAP**. Backyard itself rejects any call without a valid token. Uploads use
   signed GCS URLs, so file bytes never pass through the load balancer.
   `/__backyard/api/deploy/info`, `/__backyard/api/deploy/uploads/*` (signed,
   expiring upload URLs) and `/__backyard/healthz` also bypass IAP. Client-supplied
   `X-Goog-*` IAP headers are stripped on these routes.
4. The hub's drag-and-drop deploy sends no `Authorization` header, so it stays
   behind IAP and uses your IAP identity.
5. For CI, either mint a token with `backyard token create`, or add a static token
   to `static_tokens` (store only its SHA-256, from `backyard token hash`) and set
   `BACKYARD_TOKEN` in the pipeline. `backyard token create` posts to
   `/__backyard/api/tokens` with your API token, so that path also goes to the
   deploy backend when it carries an `Authorization` header. A token minted this
   way never outlives the token that minted it, so removing someone from
   `iap_members` still cuts them off when their last browser-issued token
   expires (30 days by default).

The site APIs (`backyard.db`, `ai`, websockets and so on) always go through IAP:
they are meant for browsers.

**Claude connector.** The [Claude connector](#claude-connector)'s four machine paths —
`/__backyard/mcp`, `/__backyard/oauth/*`, `/.well-known/oauth-authorization-server` and
`/.well-known/oauth-protected-resource/__backyard/mcp` — also go to the deploy backend, with or
without an `Authorization` header (Claude's first call and the OAuth calls carry none), and with
the IAP headers stripped. Backyard checks its own tokens there and answers `404` until the
connector is on. `/__backyard/auth/authorize` stays behind IAP: the IAP login is the connector's
sign-in.

**No IAP bypass via Cloud Run.** The service accepts traffic only from the load
balancer (`INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER`) and its `run.app` URL is
disabled (`disable_default_url`). The invoker IAM check is turned off because the
load balancer does not attach Google identity tokens: IAP authenticates users,
and the deploy backend relies on Backyard tokens. The IAP service agent still gets
`roles/run.invoker`, as Google's setup guide recommends.

**Alternatives.** Set `auth_mode = "oidc"` to use Backyard's built-in OpenID Connect
login with Okta, Entra ID, Google Workspace, Auth0 or Keycloak. IAP and the bypass
backend are then not created. Pass `oidc = { issuer, client_id, … }` and
`oidc_client_secret`, and register `https://<domain>/__backyard/auth/callback` as
the redirect URI. With `device_client_id` set, CLIs can also use the device flow.
`auth_mode = "none"` serves anonymous users, for labs and demos only: it puts
every site, the database API and the AI proxy on the internet, so the module
warns unless `security_policy` names a Cloud Armor policy that restricts
source IPs. Deploys still need a token (`static_tokens`).

## Configuration notes

- **backyard.yaml.** The module renders `templates/backyard.yaml.tftpl` into the
  `BACKYARD_CONFIG_YAML` environment variable; see the `backyard_config_yaml`
  output. The rendered file contains no secrets:
  - `server.secret` comes from `BACKYARD_SECRET`.
  - The database DSN comes from `DATABASE_URL`.
  - Other secrets appear as `${ENV}` references, which Backyard expands from
    Secret Manager-backed variables at startup.

  To add sections the module does not generate (`identity`, `sites`, `files`,
  `ratelimit`, `realtime`, `mcp`), use `extra_config_yaml`; it is appended verbatim and
  its `${ENV}` references are expanded at startup. Mount additional secrets with
  `extra_secret_env`. Everything the module renders itself is escaped, so a
  literal `${…}` in, say, a named query or the display name stays literal.
  Secrets that end up in the YAML (`oidc_client_secret`, AI keys) must not
  contain `"`, `\` or control characters; the variables validate this.
- **Cloud Run size limits.** Cloud Run rejects HTTP/1 requests over 32 MiB and
  cannot send unchunked responses over 32 MiB. Backyard serves site files and
  uploads with a `Content-Length`, so the module caps `files.max_upload_bytes`
  and `sites.max_file_bytes` at 31 MiB. A `files` or `sites` section in
  `extra_config_yaml` replaces the module's, so set those limits there as well;
  the module warns otherwise. The deploy itself uploads straight to the bucket
  through signed URLs.
- **Database.** By default the module creates Cloud SQL `db-custom-1-3840` (1 vCPU,
  3.75 GB, zonal), with daily backups, point-in-time recovery, connector-only
  access and deletion protection. Every Cloud Run instance opens up to
  `database_max_conns` document-store connections, up to `max(4, cpu)` realtime
  publisher connections and one LISTEN connection, so the budget is
  `max_instances × (database_max_conns + max(4, cpu) + 1)`. It must stay below
  the instance's `max_connections` minus 3 reserved for superusers (100 for
  3.75–6 GB of memory, by default). The module computes this for
  `db-custom-*`, `db-f1-micro` and `db-g1-small` tiers and warns when the
  budget is exceeded. Raise the limit with `database_max_connections`, which sets
  the Cloud SQL flag and restarts the instance.
  For an existing PostgreSQL (for example AlloyDB or a shared instance), use
  `database = "external-postgres"` with `external_database_url_secret`, which keeps
  the DSN out of Terraform state, or with `external_database_url`. To connect to a
  private IP from Cloud Run, add Direct VPC egress (not managed by this module).
- **AI.** With `vertex_ai = true`, the module defines these aliases on Vertex AI:
  - `smart`: Claude on Vertex, `vertex_claude_model`.
  - `fast`: Gemini, `vertex_gemini_model`.
  - `image` and `embed`.

  They authenticate as the runtime service account, which gets
  `roles/aiplatform.user`; no API keys are needed. `anthropic_api_key` and
  `openai_api_key` add first-party providers; without Vertex AI the Anthropic
  key also provides `smart` (`claude-opus-5-5`) and `fast` (`claude-haiku-5-5`).
  Add or override aliases with `ai_models` (`effort` sets the Claude effort
  level per alias):

  ```hcl
  openai_api_key = var.openai_api_key
  ai_models = {
    gpt = { provider = "openai", model = "gpt-5", description = "GPT-5" }
  }
  ```
- **Warehouse.** `bigquery_warehouse = true` adds the source `bigquery` (jobs billed
  to `bigquery_project_id`). It grants `roles/bigquery.jobUser` on that project and
  `roles/bigquery.dataViewer` on `bigquery_datasets`, or on the whole project if the
  list is empty. Add vetted queries with `warehouse_queries`. Ad-hoc `SELECT`s
  are off by default, as in Backyard, because every site would run them with
  the service account's access. Enable them with `warehouse_allow_raw_sql =
  true`, and keep `bigquery_datasets` narrow if you do. The BigQuery API must be
  enabled in `bigquery_project_id`.
- **Direct bucket deploys.** Principals in `bucket_writer_members` get
  `roles/storage.objectUser` on the bucket with an IAM condition that limits it to
  objects below `<storage_prefix>sites/`, plus `roles/storage.objectViewer`
  limited to the bucket itself (listing only, as rsync needs). They can therefore
  publish any site's files, but cannot read or change `_meta/` (site owners,
  manifests) or anyone's uploads; the server rebuilds `_meta` when the CLI calls
  the refresh endpoint after the sync. The `gcs` method (`gcloud storage rsync`)
  is then advertised to CLIs, and the bucket name is shown in `/info`. gcloud's
  parallel composite uploads keep temporary objects outside `sites/`, so they are
  refused: if a very large file fails to upload, run
  `gcloud config set storage/parallel_composite_upload_enabled False`.
- **Client IPs.** `trusted_proxies` is empty on purpose. Google's front ends pass a
  client-supplied `X-Forwarded-Host` through, and Backyard trusts X-Forwarded-Host
  and X-Forwarded-For together, so trusting the platform would let a page spoof the
  site host. As a result, anonymous rate limits (`auth_mode = "none"`) are shared
  per instance. Authenticated users are rate-limited per user. The Claude
  connector's sign-in and token requests are charged to the person they identify;
  the ones that identify no one (failed or junk requests from the internet, which
  bypass IAP) and app registrations share one budget per instance, so a flood of
  them makes those answer `429` but never locks out people who connect or refresh.
- **Path mode.** `url_mode = "path"` serves sites at `https://<domain>/s/<site>/`
  with a single, non-wildcard certificate and DNS record. All sites then share one
  origin.

## Claude connector

The [Claude connector](../../../docs/claude-connector.md) lets people publish sites from Claude
with their Google (IAP) or OIDC sign-in. Turn it on with:

```hcl
extra_config_yaml = <<-EOT
  mcp:
    enabled: true
EOT
```

The connector URL is `https://<domain>/__backyard/mcp`.

- **`auth_mode = "iap"`**: the URL map routes the four machine paths to the deploy backend
  without IAP (see [IAP notes](#iap-notes)). The routes exist whether or not the connector is on;
  after upgrading the module, run `terraform apply`.
- **`auth_mode = "oidc"`**: nothing else to do; the IdP application stays as it is.
- **Cloud Armor**: hosted Claude calls the hub from `160.79.104.0/21` (IPv4). If
  `security_policy` restricts source addresses, admit that range for the four machine paths.
- **Egress**: Backyard fetches Claude's client metadata from `https://claude.ai` at the first
  connect. Without outbound internet access from Cloud Run, add `clients: []` to the `mcp`
  section.
- **Size**: publish calls carry up to 9 MiB, well below Cloud Run's 32 MiB request limit.

## Costs

These are rough list prices in us-central1 as of 2026. Check the
[pricing calculator](https://cloud.google.com/products/calculator) for your
region.

| Component | Default | Approx. monthly |
|---|---|---|
| Cloud Run, CPU always allocated | 1 vCPU / 1 GiB, `min_instances = 1` | ~$45–65 per always-on instance, plus scale-out |
| Cloud SQL PostgreSQL | `db-custom-1-3840`, zonal, 10 GB SSD, backups | ~$50–60 (HA roughly doubles it) |
| Load balancer forwarding rules | HTTPS + HTTP redirect (IPv4) | ~$18 (first 5 rules are billed as one), plus per-GB data processing |
| Certificate Manager | one managed certificate (apex + wildcard) | cents (check current pricing) |
| IAP | | no charge |
| GCS, Secret Manager, Artifact Registry | small sites | a few dollars |
| Vertex AI / BigQuery | usage-based | pay per token / bytes scanned |

Ways to lower the bill:

- For a lab, use `min_instances = 0` (cold starts, and idle websockets are dropped)
  and `database_tier = "db-g1-small"`.
- Turn off point-in-time recovery (`database_point_in_time_recovery = false`).

For a zero-cost setup, use the Cloudflare edition instead. It runs on the
Workers free plan, in the spirit of
[OpenQuick](https://github.com/topmass/openquick), and serves the same client
API, CLI and agent tooling. See [`deploy/cloudflare`](../../cloudflare/) and its
Terraform module [`deploy/terraform/cloudflare`](../cloudflare/).

## Operations

- **Upgrades.** Change `image` and run `terraform apply`. The new revision must pass
  `/__backyard/readyz` (database and bucket reachable, migrations done) before it
  gets traffic.
- **Secret rotation.** Run `terraform apply -replace=module.backyard.random_password.backyard_secret`.
  This signs everyone out and revokes all Backyard API tokens.
- **Database access for humans.** The instance only accepts Cloud SQL connector
  connections, so use the Cloud SQL Auth Proxy:
  `cloud-sql-proxy <database_connection_name>`.
- **Destroying.** Set `deletion_protection = false` (and `bucket_force_destroy =
  true` if the bucket is not empty), apply, then `terraform destroy`. APIs stay
  enabled.

## Troubleshooting

- **TLS errors right after apply.** The certificate is still being provisioned. Check
  that the `_acme-challenge` CNAME and the A records resolve.
- **The first apply fails with "Permission denied on secret" or the revision does not
  become ready.** IAM grants can take a minute to propagate. Run `terraform apply`
  again.
- **IAP says "You don't have access".** Add the user, or one of their groups, to
  `iap_members`. The Google-managed OAuth client admits organization members only.
- **AI calls fail with 404 or permission errors for Claude.** Enable the model in
  Vertex AI Model Garden, or set `vertex_claude_model = ""`.
- **Organization policies.** `constraints/run.allowedIngress` must allow
  `internal-and-cloud-load-balancing`. A policy that forbids disabling the Cloud Run
  invoker check would block the deploy backend. The Cloud SQL instance needs a
  public IP for the connector (no authorized networks), so
  `constraints/sql.restrictPublicIp` must not be enforced; otherwise use
  `database = "external-postgres"`.
- **Large files fail to upload or load with HTTP 413 or 500.** Cloud Run's
  32 MiB HTTP/1 limit applies; see *Cloud Run size limits* above.

## Testing the module

```bash
cd deploy/terraform/gcp
terraform init -backend=false
terraform validate
terraform test        # offline, Google providers mocked (Terraform >= 1.7)

# Render the configurations of configcheck/testdata and of every run in
# tests/, and load each with Backyard's own config loader (strict keys,
# ${ENV} expansion, defaults, validation) in the container's environment:
cd ../../.. && BACKYARD_TEST_TERRAFORM=1 go test ./deploy/terraform/gcp/configcheck/
```

CI runs both (`make tf-configcheck` runs the configcheck tests of every module).

## Main inputs

| Variable | Default | Purpose |
|---|---|---|
| `project_id`, `domain` | — (required) | where and under which name to deploy |
| `region` | `us-central1` | Cloud Run, Cloud SQL, NEG, mirror |
| `auth_mode` | `iap` | `iap`, `oidc` or `none` |
| `iap_members` | `[]` | who may pass IAP |
| `url_mode` | `host` | `host` (`<site>.<domain>`) or `path` (`/s/<site>/`) |
| `image` | `ghcr.io/thasami/backyard:latest` | container image (mirrored via Artifact Registry) |
| `database` | `cloudsql` | or `external-postgres` |
| `database_tier` | `db-custom-1-3840` | Cloud SQL machine type |
| `database_max_conns` | `4` | pool size per instance (see the connection budget) |
| `min_instances` / `max_instances` | `1` / `10` | Cloud Run scaling |
| `vertex_ai` | `true` | Gemini and Claude on Vertex AI |
| `bigquery_warehouse` | `false` | BigQuery for `backyard.warehouse` |
| `dns_zone_name` | `""` | let the module create DNS records |
| `deletion_protection` | `true` | protect Cloud Run and Cloud SQL |

See [variables.tf](variables.tf) for all inputs with validations, and
[outputs.tf](outputs.tf) for the outputs: `hub_url`, `load_balancer_ip`,
`dns_records`, `bucket`, `cli_login_command`, `service_account_email`,
`iap_audience`, `backyard_config_yaml` and others.
