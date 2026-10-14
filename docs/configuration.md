# Configuration reference

The server (`backyard serve`, `backyard gateway`, the Lambda gateway) reads one YAML document,
`backyard.yaml`. This page lists every key with its type, default and meaning. The schema is
defined in [`internal/config/config.go`](../internal/config/config.go); a fully commented
example ships in the binary:

```sh
backyard config example > backyard.yaml     # annotated example (internal/config/example.yaml)
backyard config validate backyard.yaml      # load with defaults + env expansion, report every problem
backyard config validate --dev              # validate as `serve --dev` would see it
```

## Loading

The first source that exists wins:

1. `--config <file>` (`-c`) on `backyard serve` / `backyard gateway`;
2. the YAML text in the environment variable `BACKYARD_CONFIG_YAML` (handy on PaaS and Lambda);
3. the file named by `BACKYARD_CONFIG`;
4. `./backyard.yaml`.

With none of them the configuration is empty, which is only valid with `--dev`.

Decoding is **strict**: unknown keys are errors, so typos never pass silently. Then, in order:
dev defaults (with `--dev` or `server.dev: true`), environment overrides, production defaults,
validation (all problems are reported at once).

### Environment variable expansion

Every value can reference the environment before the YAML is parsed:

| Syntax | Result |
|---|---|
| `${VAR}` | the value of `VAR`, or an empty string when unset or empty |
| `${VAR:-default}` | the value of `VAR`, or `default` when unset or empty |
| `$${` | a literal `${` |

```yaml
server:
  secret: ${BACKYARD_SECRET}
database:
  type: postgres
  postgres:
    dsn: ${DATABASE_URL}
ai:
  providers:
    anthropic:
      api_key: ${ANTHROPIC_API_KEY:-}
```

Keep secrets out of the file and pass them as environment variables (Kubernetes secrets, ECS
secrets, Secret Manager, `systemd` `EnvironmentFile=`…).

### Environment overrides

A few well-known variables apply even without `${…}` references, when the corresponding key is
empty:

| Variable | Fills |
|---|---|
| `BACKYARD_SECRET` | `server.secret` |
| `BACKYARD_BASE_DOMAIN` | `server.base_domain` |
| `BACKYARD_PUBLIC_URL` | `server.public_url` |
| `DATABASE_URL` | `database.postgres.dsn` (only with `database.type: postgres`) |
| `PORT` | `server.listen` becomes `:$PORT` (Cloud Run, Container Apps, Heroku-style platforms) |

Other variables read by the server: `K_SERVICE` (Cloud Run detected: responses over 32 MiB are
streamed), the cloud SDKs' standard credentials (`AWS_*`, `GOOGLE_APPLICATION_CREDENTIALS`,
`AZURE_*`), `ANTHROPIC_API_KEY` (when the anthropic provider has no `api_key`) and
`GEMINI_API_KEY` / `GOOGLE_API_KEY` (gemini without `api_key`). The Lambda gateway also reads
`BACKYARD_GATEWAY_URL` and `BACKYARD_SECRET_ARN` (see [deploying sites](deploying-sites.md#the-aws-lambda-gateway)).
CLI variables (`BACKYARD_SERVER`, `BACKYARD_TOKEN`, …) are listed in the [CLI reference](cli.md#environment).

### Value formats

- **Durations** accept Go syntax (`90s`, `15m`, `12h`), a plain integer number of seconds
  (`3600`), or days with a `d` suffix (`30d`).
- **Sizes** are integers in bytes (`104857600` = 100 MiB).
- For numeric limits, `0` (or leaving the key out) selects the default. For the `ratelimit.*`
  limits and `database.max_docs_per_site` a negative value means unlimited (that limit is
  off); `storage.cache_bytes: -1` disables the cache.

### Dev mode

`backyard serve --dev` (or `server.dev: true`) fills in a zero-dependency local setup before the
normal defaults: `server.base_domain: localhost`, `auth.mode: none`, `storage.type: fs` with
`storage.fs.root: .backyard-data`, `database.type: sqlite` (in that folder), `pubsub.type:
memory`, `deploy.allow_anonymous: true` (with `auth.mode: none`), a random secret (sessions and
tokens do not survive a restart unless you set `BACKYARD_SECRET`) and text logs. Dev mode also
lets IP-literal and `localhost` host names reach the hub. Anything you configure explicitly
wins. Never use dev mode on a shared network.

---

## `server`

| Key | Type | Default | Description |
|---|---|---|---|
| `server.listen` | string | `:$PORT`, else `:8080` | Listen address. |
| `server.base_domain` | string | **required** (dev: `localhost`) | The hub's host name. Sites live at `<site>.<base_domain>` in host mode. Point the apex and a wildcard DNS record at the server. With `auth.mode: oidc` in host mode it must be able to carry a cookie (see [`auth.oidc`](#authoidc)). |
| `server.public_url` | string | `https://<base_domain>` (`http://` plus the listen port for `localhost`/`127.*`/`lvh.me`) | The externally visible hub URL. Used for redirects, cookies, CLI login and site URLs (in host mode `<site>.<base_domain>` with this URL's scheme and port). |
| `server.url_mode` | `host` \| `path` | `host` | `host`: one subdomain per site (needs wildcard DNS + TLS). `path`: sites at `<public_url>/s/<site>/` on one host (sites then share an origin — see [security](security.md#host-mode-vs-path-mode)). |
| `server.secret` | string | **required**, ≥ 32 characters (dev: random) | Root secret. Every signing/encryption key (sessions, API tokens, deploy ids, upload URLs, anonymous ids) is derived from it. Generate with `openssl rand -hex 32`. Rotating it signs everybody out and revokes all API tokens. |
| `server.trusted_proxies` | list of CIDRs/IPs | `[]` | Peers whose `X-Forwarded-For/-Host/-Proto` are honoured, and who may send identity headers in `auth.mode: header`. Set it to your load balancer ranges. |
| `server.dev` | bool | `false` | Developer defaults (see [dev mode](#dev-mode)). |
| `server.name` | string | `Backyard` | Name shown in the hub. |
| `server.read_timeout` | duration | `60s` | HTTP read timeout (uploads extend it). |
| `server.write_timeout` | duration | `120s` | HTTP write timeout. Websockets, AI streams and large downloads are exempt. |
| `server.idle_timeout` | duration | `120s` | Keep-alive idle timeout. |
| `server.shutdown_timeout` | duration | `20s` | Grace period for in-flight requests on `SIGTERM`/`SIGINT`. |

## `auth`

See [authentication](authentication.md) for step-by-step setups.

| Key | Type | Default | Description |
|---|---|---|---|
| `auth.mode` | `none` \| `oidc` \| `iap` \| `alb` \| `header` | `none` | How people sign in. `okta`, `entra`, `azuread`, `azure-ad`, `google`, `auth0`, `keycloak` and `openid` are accepted aliases of `oidc`; `off` and `disabled` mean `none`. |
| `auth.admins` | list of strings | `[]` | User ids / emails with admin rights (delete any site, overwrite in ownership mode). |
| `auth.admin_groups` | list of strings | `[]` | Groups whose members are admins. |
| `auth.allowed_domains` | list of strings | `[]` (anyone) | Only these email domains may use Backyard (`403` otherwise). |
| `auth.allowed_emails` | list of strings | `[]` | These exact (verified) email addresses may use Backyard too: an address passes when its domain is in `allowed_domains` or it is listed here. With only this list set, only the listed people get in. |
| `auth.allowed_groups` | list of strings | `[]` (anyone) | Only members of these groups may use Backyard. |
| `auth.token_ttl` | duration | `720h` | Lifetime — and maximum lifetime — of API tokens minted by the server (CLI login, `backyard token create`). A token minted with an API token as the credential expires no later than that token. Also how long a [Claude connector](#mcp) connection lasts after its consent. |
| `auth.tokens` | list | `[]` | Static bearer tokens, e.g. for CI: `[{name, sha256, admin}]`. |
| `auth.tokens[].name` | string | — | Shown in audit logs; the token acts as user `token:<name>`. |
| `auth.tokens[].sha256` | string | — | Hex SHA-256 of the token (64 characters). `backyard token hash` prints it. The token itself is never stored. |
| `auth.tokens[].admin` | bool | `false` | Give the token admin rights. |

### `auth.oidc`

Built-in OpenID Connect login (authorization code + PKCE) for `auth.mode: oidc`. When `issuer`
is set in another mode, IdP-issued bearer tokens (CLI device flow) are accepted there too.

In host mode the session cookie is set for `.<base_domain>` so that every site sees it.
Validation (`backyard config validate`, and `backyard serve` at startup) therefore rejects
`auth.mode: oidc` with `server.url_mode: host` in two cases — otherwise every site visit would
bounce through the IdP forever:

- `server.base_domain` cannot carry a cookie `Domain`: `localhost`, `*.localhost`, an IP
  address or a single-label name. The error reads
  `server.base_domain "localhost" cannot carry a cookie Domain …`.
- The host of `server.public_url` is neither `base_domain` nor one of its subdomains
  (`public_url: https://hub.acme.com` with `base_domain: apps.acme.com`). The error reads
  `server.public_url host "hub.acme.com" must be server.base_domain "apps.acme.com" or one of its subdomains …`.

Use `url_mode: path` then, or — for local tests — a real domain whose subdomains resolve to
loopback, such as `lvh.me`; a hub on a subdomain of `base_domain`
(`public_url: https://hub.apps.acme.com`) is fine.

| Key | Type | Default | Description |
|---|---|---|---|
| `auth.oidc.issuer` | URL | **required** for `oidc` | Issuer URL (discovery at `/.well-known/openid-configuration`). Okta: `https://<org>.okta.com/oauth2/default`; Entra ID: `https://login.microsoftonline.com/<tenant>/v2.0`; Google: `https://accounts.google.com`. |
| `auth.oidc.client_id` | string | **required** for `oidc` | Client id of the web application. |
| `auth.oidc.client_secret` | string | `""` | Client secret. |
| `auth.oidc.scopes` | list | `[openid, profile, email]` | Requested scopes. Add `groups` where the IdP needs a scope for the groups claim. |
| `auth.oidc.redirect_url` | URL | `<public_url>/__backyard/auth/callback` | Redirect URI registered at the IdP. |
| `auth.oidc.session_ttl` | duration | `12h` | Browser session lifetime. |
| `auth.oidc.use_userinfo` | bool | `false` | Also call the userinfo endpoint and merge its claims (for IdPs that keep groups or profile fields out of the ID token). |
| `auth.oidc.device_client_id` | string | `""` | Public/native client id that enables `backyard login --device` (OAuth device authorization grant). |
| `auth.oidc.device_scopes` | list | `[openid, profile, email, offline_access]` | Scopes for the device flow (`offline_access` lets the CLI refresh its token). |
| `auth.oidc.api_audiences` | list | `[]` | Extra audiences accepted for IdP-issued bearer JWTs (besides `client_id` and `device_client_id`). |
| `auth.oidc.insecure_skip_issuer_check` | bool | `false` | Skip the issuer comparison for IdPs with non-standard issuer URLs. Avoid. |

### `auth.iap`

Google Cloud Identity-Aware Proxy (`auth.mode: iap`).

| Key | Type | Default | Description |
|---|---|---|---|
| `auth.iap.audience` | string | **required** for `iap` | Expected `aud`: `/projects/<number>/global/backendServices/<id>` (load balancer) or `/projects/<number>/locations/<region>/services/<name>` (Cloud Run). |
| `auth.iap.additional_audiences` | list | `[]` | Further accepted audiences (e.g. a second backend service). |
| `auth.iap.jwks_url` | URL | `https://www.gstatic.com/iap/verify/public_key-jwk` | Signing keys. |

### `auth.alb`

AWS Application Load Balancer OIDC authentication (`auth.mode: alb`).

| Key | Type | Default | Description |
|---|---|---|---|
| `auth.alb.region` | string | **required** for `alb` | Region of the load balancer (selects the public key endpoint). |
| `auth.alb.signer_arn` | string | `""` | Load balancer ARN expected in the JWT `signer` header. Strongly recommended. |
| `auth.alb.key_url` | URL | `https://public-keys.auth.elb.<region>.amazonaws.com` | Public key endpoint override. |

### `auth.header`

Trusted identity headers from a proxy (`auth.mode: header`): oauth2-proxy, Pomerium, Cloudflare
Access, Azure Easy Auth, nginx `auth_request`…

| Key | Type | Default | Description |
|---|---|---|---|
| `auth.header.email_header` | string | `""` | Header with the user's email, e.g. `X-Forwarded-Email`, `X-Auth-Request-Email`, `X-MS-CLIENT-PRINCIPAL-NAME`. |
| `auth.header.name_header` | string | `""` | Optional display-name header. |
| `auth.header.groups_header` | string | `""` | Optional comma-separated groups header. |
| `auth.header.jwt_header` | string | `""` | Optional header carrying a signed JWT to verify (e.g. `Cf-Access-Jwt-Assertion`). With it, the JWT is the source of truth. |
| `auth.header.jwks_url` | URL | `""` | JWKS for `jwt_header` (required with it). |
| `auth.header.issuer` | string | `""` | Expected JWT issuer (recommended). |
| `auth.header.audiences` | list | `[]` | Accepted JWT audiences (recommended). |

Validation: `header` mode needs `email_header` or `jwt_header`, and `server.trusted_proxies`
unless a JWT is verified.

## `identity`

| Key | Type | Default | Description |
|---|---|---|---|
| `identity.claims.email` | string | `email` | Claim holding the email (the user id). Use `preferred_username` for Entra ID users without a mail attribute. With the default `email` claim, addresses the IdP marks `email_verified: false` are ignored. |
| `identity.claims.name` | string | `name` | Display name (falls back to `given_name family_name`, the email's local part, `preferred_username`, `sub`). |
| `identity.claims.title` | string | `title` | Job title. |
| `identity.claims.team` | string | `department` | Team. |
| `identity.claims.slack` | string | `slack` | Slack handle. |
| `identity.claims.avatar` | string | `picture` | Avatar URL. |
| `identity.claims.groups` | string | `groups` | Groups (array or string). |
| `identity.extra_claims` | list | `[]` | Further claims copied verbatim into `user.extra`. |
| `identity.directory.type` | `""` \| `csv` \| `json` | `""` (none) | Optional people directory that enriches and overrides profiles, and answers `backyard.id.lookup(email)` for people who never signed in. |
| `identity.directory.path` | path | `""` | CSV with a header row containing `email` (recognised: `name`, `title`, `team`, `slack`, `avatar_url`, `groups` separated by `;`; other columns go to `extra`), or a JSON array of user objects. |
| `identity.directory.reload` | duration | `0` (load once) | Re-read the file when it changed, at most this often. |

## `storage`

Where site files and uploads live. See [storage and databases](storage-and-databases.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `storage.type` | `fs` \| `s3` \| `gcs` \| `azure` | `fs` | Blob store. |
| `storage.cache_bytes` | int | `67108864` (64 MiB) | In-memory cache for static objects ≤ 1 MiB; `-1` disables it. |
| `storage.cache_ttl` | duration | `5s` | How long cached objects (and "site exists" answers) are trusted. Deploys through the embedded gateway purge the cache immediately. |
| `storage.fs.root` | path | `/var/lib/backyard` (dev: `.backyard-data`) | Root directory; also the `rsync` target and the default home of the SQLite file. |
| `storage.s3.bucket` | string | **required** for `s3` | Bucket. |
| `storage.s3.region` | string | SDK default | Region (`auto` is used for R2 endpoints). |
| `storage.s3.prefix` | string | `""` | Key prefix inside the bucket (normalized to `prefix/`). |
| `storage.s3.endpoint` | URL | `""` | S3-compatible endpoint (MinIO, Cloudflare R2, Ceph, Backblaze B2). Switches to path-style addressing. |
| `storage.s3.force_path_style` | bool | `false` | Path-style addressing without a custom endpoint. |
| `storage.s3.access_key_id` / `secret_access_key` | string | `""` | Static credentials; default is the AWS credential chain (env, IRSA, ECS/EC2 roles). |
| `storage.s3.sse_kms_key_id` | string | `""` | Write with SSE-KMS using this key. |
| `storage.gcs.bucket` | string | **required** for `gcs` | Bucket. |
| `storage.gcs.prefix` | string | `""` | Key prefix. |
| `storage.gcs.credentials_file` | path | `""` | Service account JSON; default Application Default Credentials. |
| `storage.gcs.signer_email` | string | auto-detected | Service account that signs upload URLs through the IAM Credentials API when ADC has no private key (Cloud Run, GKE Workload Identity); needs `roles/iam.serviceAccountTokenCreator` on itself. |
| `storage.gcs.endpoint` | URL | `""` | Endpoint override (emulators). |
| `storage.azure.account` | string | **required** for `azure` (or `connection_string`) | Storage account. |
| `storage.azure.container` | string | **required** for `azure` | Container. |
| `storage.azure.prefix` | string | `""` | Key prefix. |
| `storage.azure.connection_string` | string | `""` | Connection string (Azurite, shared key); default `DefaultAzureCredential` (managed identity, workload identity, Azure CLI). |
| `storage.azure.account_key` | string | `""` | Shared key (enables service SAS without user delegation). |
| `storage.azure.endpoint` | URL | `https://<account>.blob.core.windows.net/` | Endpoint override. |

## `database`

The document store behind `backyard.db`, plus the user cache, upload metadata and audit records.

| Key | Type | Default | Description |
|---|---|---|---|
| `database.type` | `sqlite` \| `postgres` \| `dynamodb` \| `memory` | `sqlite` | Backend. `memory` loses everything on restart (tests, demos). |
| `database.max_doc_bytes` | int | `262144` (256 KiB) | Largest document: its JSON data as stored (`<`, `>` and `&` count one byte each). Keep it below ~390000 on DynamoDB (400 KB item limit). Above about 1 MiB a document no longer fits one realtime event: subscribers get the change with `doc: null` and the SDK re-reads the document ([SPEC §6](spec/SPEC.md#6-realtime-websocket-get-__backyardapiws)). |
| `database.max_list` | int | `1000` | Largest `limit` of a list or query (default page size is 100). |
| `database.max_docs_per_site` | int | `100000` | Soft per-site document quota (`413 too_large` beyond it); negative = unlimited. |
| `database.sqlite.path` | path | `<storage.fs.root>/backyard.db` | Database file (WAL mode; single node). |
| `database.postgres.dsn` | string | `$DATABASE_URL` | `postgres://user:pass@host:5432/db?sslmode=require`. |
| `database.postgres.max_conns` | int | `20` | Document store connection pool size per instance. It takes precedence over a `pool_max_conns` parameter in the DSN. |
| `database.postgres.schema` | string | `public` | Schema for Backyard's tables. |
| `database.postgres.auto_migrate` | bool | `true` | Create and migrate tables on start (serialized with an advisory lock). |
| `database.dynamodb.table` | string | `backyard` | Table name (partition key `pk`, sort key `sk`, both strings). |
| `database.dynamodb.region` | string | SDK default | Region. |
| `database.dynamodb.endpoint` | URL | `""` | DynamoDB Local / LocalStack. |
| `database.dynamodb.create_table` | bool | `false` | Create the table (on-demand billing) when missing. |
| `database.dynamodb.max_scan` | int | `10000` | Items read per query before in-memory filtering and sorting; larger collections are only partially searched. |

## `pubsub`

Cross-instance fan-out for realtime events. One instance needs nothing.

| Key | Type | Default | Description |
|---|---|---|---|
| `pubsub.type` | `memory` \| `postgres` \| `redis` | `memory` | `memory` is single-instance only. |
| `pubsub.redis.url` | URL | **required** for `redis` | `redis://[:password@]host:6379/0` or `rediss://…` (Redis, Valkey, ElastiCache, Memorystore). |
| `pubsub.postgres.dsn` | string | `database.postgres.dsn` | LISTEN/NOTIFY database. Pub/sub opens one LISTEN connection plus a publishing pool of 2–4 connections (half the CPU count, clamped); a `pool_max_conns` parameter in the DSN sets the pool size instead — also when the DSN is inherited from `database.postgres.dsn`. |
| `pubsub.postgres.channel` | string | `backyard_events` | Notification channel. |

## `ai`

See [AI providers](ai-providers.md) for one example per provider.

| Key | Type | Default | Description |
|---|---|---|---|
| `ai.enabled` | bool | `true` when `ai.models` is not empty | Turn the AI API on or off. |
| `ai.default_model` | string | first chat alias (preferring `default`, `smart`, `chat`, `fast`) | Alias used when a request names none. |
| `ai.default_image_model` | string | first image alias (`image`, `images`…) | Default for `/ai/image`. |
| `ai.default_embedding_model` | string | first embedding alias (`embed`, `embedding`…) | Default for `/ai/embed`. |
| `ai.providers.<name>` | map | — | Upstream provider instances (see below). |
| `ai.models.<alias>` | map | — | Aliases clients use (see below). Aliases may not contain `/`. |
| `ai.allow_raw_models` | bool | `false` | Let clients pass `"<provider>/<model-id>"` instead of an alias. |
| `ai.expose_model_ids` | bool | `false` | Include provider model ids in `GET /ai/models`. |
| `ai.max_tokens` | int | `8192` | Cap on `max_tokens` per request (requests without one get min(4096, cap)). |
| `ai.request_timeout` | duration | `120s` | Per upstream call (streaming and image generation get at least 10 minutes). |
| `ai.system_prompt` | string | `""` | Prepended to every request's system prompt (company policy, tone…). |

`ai.providers.<name>` (the type defaults to the map key, so `anthropic: {}` is enough with
`ANTHROPIC_API_KEY` set):

| Key | Type | Default | Description |
|---|---|---|---|
| `type` | `anthropic` \| `openai` \| `azure-openai` \| `bedrock` \| `gemini` \| `ollama` | the key name | Provider kind. |
| `api_key` | string | `""` | API key (anthropic falls back to `ANTHROPIC_API_KEY`; gemini to `GEMINI_API_KEY`/`GOOGLE_API_KEY`; bedrock treats it as a Bedrock API key). |
| `base_url` | URL | provider default (`ollama`: `http://localhost:11434/v1`) | API base URL (OpenAI-compatible servers, gateways, proxies). |
| `headers` | map | `{}` | Extra HTTP headers on every upstream request (e.g. `cf-aig-authorization`). |
| `platform` | `anthropic` \| `vertex` \| `bedrock` | `anthropic` | anthropic only: first-party API, Claude on Vertex AI, or Claude on Bedrock (Mantle Messages API). gemini: `vertex` selects Vertex AI. |
| `refusal_fallback` | bool | `true` | anthropic first-party API: enable server-side refusal fallbacks where supported. |
| `effort` | `low` \| `medium` \| `high` \| `xhigh` \| `max` | model default | anthropic: default effort for models that support it. |
| `organization` / `openai_project` | string | `""` | openai: `OpenAI-Organization` / `OpenAI-Project` headers. |
| `endpoint` | URL | — | azure-openai (required): `https://<resource>.openai.azure.com`. |
| `api_version` | string | `2024-10-21` | azure-openai API version. |
| `use_azure_ad` | bool | `false` | azure-openai: authenticate with Entra ID (`DefaultAzureCredential`) instead of `api_key`. |
| `region` | string | `AWS_REGION` | bedrock, and anthropic with `platform: bedrock`. |
| `project_id` | string | ADC project | gemini on Vertex AI, anthropic with `platform: vertex`. |
| `location` | string | `global` | Vertex AI location. |

`ai.models.<alias>`:

| Key | Type | Default | Description |
|---|---|---|---|
| `provider` | string | **required** | A key of `ai.providers`. |
| `model` | string | **required** | Provider model id, e.g. `claude-opus-5-5`, `gpt-image-1`, `text-embedding-3-small`. |
| `kind` | `chat` \| `image` \| `embedding` | `chat` | What the alias is for. |
| `description` | string | `""` | Shown by `GET /ai/models`. |
| `max_tokens` | int | `0` (global cap) | Lower per-alias cap (cannot exceed `ai.max_tokens`). |
| `effort` | string | `""` | anthropic effort override for this alias. |

## `warehouse`

See [warehouse](warehouse.md).

| Key | Type | Default | Description |
|---|---|---|---|
| `warehouse.sources.<name>` | map | `{}` | Analytics sources (none = warehouse disabled, `501`). |
| `warehouse.default_source` | string | `default`, or the only source | Source for raw SQL without `source`. |
| `warehouse.allow_raw_sql` | bool | `false` | Allow ad-hoc SQL from sites (otherwise only named queries). |
| `warehouse.allow_any_statement` | bool | `false` | Disable the single-`SELECT`/`WITH` statement guard. Avoid. |
| `warehouse.max_rows` | int | `10000` | Row cap per result (`truncated: true` beyond). Results are also capped at 32 MiB of row data (fixed). |
| `warehouse.timeout` | duration | `60s` | Query timeout. |
| `warehouse.queries.<name>.source` | string | `default_source` | Source of a named query. |
| `warehouse.queries.<name>.sql` | string | **required** | SQL with `@param` placeholders. |
| `warehouse.queries.<name>.params` | list | `[]` | Parameter names (all required at call time). |
| `warehouse.queries.<name>.description` | string | `""` | Shown by `GET /warehouse`. |

`warehouse.sources.<name>`:

| Key | Type | Applies to | Description |
|---|---|---|---|
| `type` | `bigquery` \| `postgres` \| `athena` | all | Engine (`postgres` covers Redshift, AlloyDB, Aurora, Neon, CockroachDB…). |
| `description` | string | all | Shown by `GET /warehouse`. |
| `project_id` | string | bigquery (required) | Project that runs (and pays for) query jobs. |
| `location` | string | bigquery | Job location (`US`, `EU`, `us-central1`…). |
| `max_bytes_billed` | int | bigquery | Per-query billing cap in bytes. |
| `credentials_file` | path | bigquery | Credentials JSON; default ADC. |
| `dsn` | string | postgres (required) | Connection string of a **read-only** role. |
| `region` | string | athena (required) | AWS region. |
| `workgroup` | string | athena | Workgroup (needs `output_location` if the workgroup has none). |
| `database` / `catalog` | string | athena | Default database and catalog. |
| `output_location` | string | athena | `s3://…` results location. |

## `sites`

| Key | Type | Default | Description |
|---|---|---|---|
| `sites.ownership` | `none` \| `owner` | `none` | `none`: anyone may overwrite any site (the Quick way). `owner`: the first deployer owns a site; only owners and admins may redeploy or delete it; owners can be managed through the gateway. |
| `sites.reserved` | list | `www, api, admin, hub, auth, static, assets, cdn, docs, mail, backyard, s` | Names that can never be deployed. Setting the key replaces the list. |
| `sites.max_site_bytes` | int | `524288000` (500 MiB) | Total size of a site's files. |
| `sites.max_files` | int | `10000` | Files per site. |
| `sites.max_file_bytes` | int | `104857600` (100 MiB) | Size of one deployed file. |

## `files`

| Key | Type | Default | Description |
|---|---|---|---|
| `files.max_upload_bytes` | int | `104857600` (100 MiB) | Largest `backyard.files.upload` (and generated image). |

## `deploy`

| Key | Type | Default | Description |
|---|---|---|---|
| `deploy.methods` | list of `api`, `s3`, `gcs`, `azure`, `rsync` | `[api]` plus the storage-native method (`s3`/`gcs`/`azure`; `rsync` for fs when `rsync_target` is set) | Methods advertised to CLIs in `/info`, in preference order. |
| `deploy.rsync_target` | string | `""` | `user@host:/var/lib/backyard/sites` for `--method rsync`. |
| `deploy.gateway_url` | URL | `""` | External gateway (Lambda function URL, a proxy-exempt host) advertised to CLIs; they adopt it at login when reachable. |
| `deploy.allow_anonymous` | bool | `false` (dev: `true`) | Deploy without any token. Only allowed with `auth.mode: none`. |
| `deploy.presign` | bool | `true` | Upload straight to the bucket with presigned URLs (s3/gcs/azure). `false` routes uploads through the gateway. |
| `deploy.expose_storage` | bool | `true` | Include bucket/container details in `/info` (needed for direct-sync methods). |
| `deploy.embedded_gateway` | bool | `true` | Mount the gateway at `/__backyard/api/deploy`. Disable when only a standalone gateway may deploy. |

## `mcp`

The [Claude connector](claude-connector.md): a remote MCP endpoint at
`<public_url>/__backyard/mcp` with Backyard's own OAuth sign-in
([SPEC §8.4](spec/SPEC.md#84-remote-mcp-connector-oauth-21-authorization-server)). Off by
default; while it is off every connector path answers `404`.

| Key | Type | Default | Description |
|---|---|---|---|
| `mcp.enabled` | bool | `false` | Serve the Claude connector: `<public_url>/__backyard/mcp` plus Backyard's OAuth endpoints (SPEC §8.4). Needs an https `server.public_url` without a path. |
| `mcp.clients` | list of URLs | the two Claude client metadata URLs | Client ID Metadata Document URLs that may connect. Only these are fetched. `[]` turns CIMD off, so apps register automatically. |
| `mcp.redirect_uris` | list of URLs | `[https://claude.ai/api/mcp/auth_callback, https://claude.com/api/mcp/auth_callback]` | Exact https addresses that may receive sign-in codes. http loopback addresses are always allowed. |
| `mcp.max_publish_bytes` | int | `4194304` | Decoded file bytes per `backyard_publish` call. The MCP request body limit is 2× this + 1 MiB. |

The two Claude client metadata URLs are `https://claude.ai/oauth/mcp-oauth-client-metadata`
and `https://claude.ai/oauth/claude-code-client-metadata`. Leaving a list out keeps its
default; an explicit `[]` stays empty. Entries are compared byte for byte, so write them exactly
(their case is kept). The connector also uses `auth.token_ttl` (how long a connection lasts),
`ratelimit.api_per_minute` (OAuth requests per client IP, MCP requests per user),
`ratelimit.deploys_per_hour` (publishes per user), `server.name` (the name in the discovery
document), `deploy.allow_anonymous` and `deploy.embedded_gateway`. A standalone gateway
validates the section but ignores it.

Validation reports every problem at once, with these messages (the entry rules apply even
while the connector is off):

- `mcp.clients: "…" is not a client metadata URL (an https URL with a path, without query, fragment or user info, at most 512 bytes)`
  — also for an IP-literal host, a path of `/` or with `.`/`..` segments (`https://claude.ai`
  alone fails: it has no path).
- `mcp.redirect_uris: "…" is not an https redirect URI without fragment or user info (at most 512 bytes; loopback redirect URIs are always allowed and are not listed)`
- `mcp.redirect_uris: "…" points at this Backyard or one of its sites` — its host is the
  `public_url` host, `server.base_domain` or a subdomain of it.
- `mcp.max_publish_bytes must be between 65536 and 67108864 and at most sites.max_site_bytes`
  — the comparison with `sites.max_site_bytes` applies only with `mcp.enabled: true`.
- `mcp.enabled requires server.public_url to be https (http only on localhost, *.localhost or loopback addresses) without a path, query or fragment`
- `auth.oidc.redirect_url must not be /__backyard/auth/authorize while mcp.enabled is true`

## `ratelimit`

Per-user token buckets, **per instance** (anonymous users are keyed by client IP). Limited
requests get `429 rate_limited` with `Retry-After`. `0` (or leaving a key out) selects the
default; a negative value means unlimited (that one limit is off).

| Key | Type | Default | Description |
|---|---|---|---|
| `ratelimit.disabled` | bool | `false` | Turn every limit off. |
| `ratelimit.api_per_minute` | int | `600` | All site and hub API calls, and the Claude connector's requests (OAuth per client IP, MCP per user); negative = unlimited. |
| `ratelimit.db_writes_per_minute` | int | `300` | Document writes and file deletes; negative = unlimited. |
| `ratelimit.ai_per_minute` | int | `20` | AI calls; negative = unlimited. |
| `ratelimit.ai_tokens_per_day` | int | `2000000` | Input + output AI tokens per user per day (resets 00:00 UTC); negative = unlimited. |
| `ratelimit.deploys_per_hour` | int | `60` | Deploy creates and refreshes (enforced by the gateway), and Claude connector publishes; negative = unlimited. |
| `ratelimit.uploads_per_minute` | int | `60` | `backyard.files.upload` calls; negative = unlimited. |
| `ratelimit.warehouse_per_minute` | int | `30` | Warehouse queries; negative = unlimited. |

## `realtime`

| Key | Type | Default | Description |
|---|---|---|---|
| `realtime.disabled` | bool | `false` | Turn websockets off (`501`). |
| `realtime.max_rooms_per_conn` | int | `50` | Rooms one connection may join. |
| `realtime.max_subs_per_conn` | int | `100` | Collection subscriptions per connection. |
| `realtime.messages_per_second` | int | `60` | Sustained client frames per connection. |
| `realtime.message_burst` | int | `120` | Burst size of that bucket. |
| `realtime.max_message_bytes` | int | `65536` | Largest frame. |
| `realtime.max_conns_per_user` | int | `50` | Concurrent websockets per user (more get `429`). |
| `realtime.max_presence_bytes` | int | `8192` | Largest presence state. |
| `realtime.allow_cross_site_ws` | bool | `false` | Let pages on other Backyard sites (and the hub) open a site's websocket. |

## `telemetry`

| Key | Type | Default | Description |
|---|---|---|---|
| `telemetry.log_level` | `debug` \| `info` \| `warn` \| `error` | `info` | Log level. |
| `telemetry.log_format` | `json` \| `text` | `json` (dev: `text`) | Log format. |
| `telemetry.metrics` | bool | `false` | Expose Prometheus metrics. |
| `telemetry.metrics_listen` | string | `""` | Serve `/metrics` on a separate address (e.g. `:9090`, no authentication) instead of the authenticated `/__backyard/metrics` on the hub. Recommended in production. |
| `telemetry.access_log` | bool | `false` | Log every request (method, host, path, status, bytes, duration, user, request id). |

---

## A production example

```yaml
server:
  base_domain: backyard.acme.com
  secret: ${BACKYARD_SECRET}
  trusted_proxies: ["10.0.0.0/8"]
auth:
  mode: oidc
  admin_groups: [backyard-admins]
  allowed_domains: [acme.com]
  tokens:
    - name: github-actions
      sha256: ${CI_TOKEN_SHA256}
  oidc:
    issuer: https://acme.okta.com/oauth2/default
    client_id: ${OIDC_CLIENT_ID}
    client_secret: ${OIDC_CLIENT_SECRET}
    device_client_id: ${OIDC_DEVICE_CLIENT_ID:-}
storage:
  type: s3
  s3: { bucket: acme-backyard, region: us-east-1 }
database:
  type: postgres            # dsn from DATABASE_URL
pubsub:
  type: postgres            # LISTEN/NOTIFY on the same database
ai:
  providers:
    anthropic: { api_key: "${ANTHROPIC_API_KEY}" }
  models:
    smart: { provider: anthropic, model: claude-opus-5-5, description: Most capable }
    fast:  { provider: anthropic, model: claude-haiku-5-5, description: Fast and inexpensive }
telemetry:
  metrics: true
  metrics_listen: ":9090"
```
