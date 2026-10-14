# Operations

How to watch, back up, scale and upgrade a Backyard server. The deployment kits implement most
of this already; this page explains what they rely on.

## Health checks

| Endpoint | Meaning | Auth |
|---|---|---|
| `GET /__backyard/healthz` | Liveness: the process serves HTTP. Always `200 ok`. | none, any host |
| `GET /__backyard/readyz` | Readiness: pings the document store and blob storage (5 s budget). `200 ok`, or `503 not ready: database, storage`; details go to the log. | none, any host |

Use `healthz` for liveness and container start-up probes, `readyz` for load balancer and
readiness probes. Both answer on every host name, so probes need no `Host` header. The
container image is distroless (no shell, no curl) — probe over HTTP from the orchestrator.

## Metrics

Enable Prometheus metrics with `telemetry.metrics: true`. They are served either at
`/__backyard/metrics` on the hub — **authenticated** like any page, so scrape with a
[static token](authentication.md#static-tokens) as bearer — or, recommended, on a separate
listener without authentication: `telemetry.metrics_listen: ":9090"` serves `/metrics` there
(keep that port private). The Helm chart ships a `ServiceMonitor`.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `backyard_http_requests_total` | counter | `area`, `code` | Requests by area (`static`, `api`, `ws`, `files`, `sdk`, `hub`, `docs`, `auth`, `deploy`, `health`, `metrics`, `mcp`, `oauth`, `unknown_host`, `other`) and status code. `mcp` is the [Claude connector's](claude-connector.md) endpoint `/__backyard/mcp`, `oauth` its OAuth endpoints and discovery documents (`/__backyard/oauth/*`, `/.well-known/oauth-*`); its sign-in page `/__backyard/auth/authorize` counts as `auth`, its grants API as `api` |
| `backyard_http_request_duration_seconds` | histogram | `area` | Request latency |
| `backyard_deploys_total` | counter | `method` (`api`, `sync`, `mcp`), `result` (`ok`, `error`) | Deploy commits through the embedded gateway, refreshes after direct syncs, and publishes by the Claude connector (`mcp`) |
| `backyard_db_ops_total` | counter | `op`, `result` | Document store operations (`result`: `ok`, `not_found`, `conflict`, `too_large`, …) |
| `backyard_ai_requests_total` | counter | `model`, `result` | AI requests per alias |
| `backyard_ai_tokens_total` | counter | `model`, `direction` (`input`, `output`) | AI tokens per alias |
| `backyard_ws_connections` | gauge | | Open websockets on this instance |
| `backyard_ws_messages_total` | counter | `dir` (`in`, `out`) | Websocket frames |
| `backyard_warehouse_queries_total` | counter | `source`, `result` | Warehouse queries |
| `backyard_rate_limited_total` | counter | `category` (`api`, `db_write`, `ai`, `warehouse`, `upload`) | Requests rejected by rate limits |
| `backyard_build_info` | gauge | `version` | Always 1 |
| `go_*`, `process_*` | | | Go runtime and process collectors |

Useful alerts: `readyz` failing; a rising rate of `code=~"5.."`; `backyard_ai_requests_total`
with `result` other than `ok` (provider outage or exhausted quota);
`backyard_rate_limited_total` spikes (a runaway page); `backyard_deploys_total{result="error"}`.

## Logs and audit

Logs are structured (`log/slog`) on stderr: JSON by default, `text` in dev
(`telemetry.log_format`), level `telemetry.log_level`. `telemetry.access_log: true` adds one
line per request with method, host, path, status, bytes, duration, area, client IP, site, user
and `request_id`. Every response carries `X-Request-Id` (an incoming valid `X-Request-Id` is
kept), and every `5xx` failure is logged at error level with its request id — ask users for the
`X-Request-Id` of a failing call.

**Audit events** are log lines with `"audit": true` and the acting user and client IP:

| Event | Source |
|---|---|
| `auth: login`, `auth: login denied by allowed_domains/allowed_emails/allowed_groups` | OIDC login |
| `auth: issued CLI token` | browser → CLI token hand-off (`confirmed` when the user confirmed a cross-site request) |
| `auth: CLI login confirmation rejected …` | a confirmation form that expired or was not issued for this user and session |
| `auth: request carries session cookies of different users …`, `… guest cookies of different guests …` | conflicting (possibly planted) cookies; the request was refused and foreign cookie variants expired |
| `token.issue` | `POST /__backyard/api/tokens` (`backyard token create`); `from_api_token` when an API token minted it (its lifetime was capped) |
| `deploy.create`, `deploy.commit`, `deploy.reject_uploads`, `deploy.clear_conflicts` | deploy gateway (files, bytes, new paths, deletions; `deploy.commit` also counts the files whose digest it `verified` and those `superseded` by another deploy) |
| `site.refresh`, `site.delete`, `site.owners` | deploy gateway |
| `mcp.authorize`, `mcp.deny` | [Claude connector](claude-connector.md) consent page: allowed or cancelled (user, `client_name`, `client_kind`, `client_host`, `redirect_host`, `grant` — the first 8 characters of the connection id) |
| `mcp.consent_rejected` (warn) | a consent form that expired, was not issued to this user or session, or was not sent from Backyard's page |
| `mcp.token` | a sign-in code exchanged for tokens |
| `mcp.code_replay`, `mcp.refresh_reuse` (warn) | a code redeemed twice, an old refresh token used again: the connection is deleted |
| `mcp.revoke` | a connection deleted; `via` = `rfc7009` (the app), `hub` (hub card or grants API; `count` for a person's connections), `policy` (a gate no longer passes) or `replay` |
| `deploy.commit` with `via=mcp` | a `backyard_publish` call, with `client` (the app's name) and `grant` |

Ship the log stream to your SIEM for retention. In addition, the server stores `token.issue`,
`mcp.authorize`, `mcp.code_replay`, `mcp.refresh_reuse` and `mcp.revoke` records in the
document store (site `_system`, collection `_audit`), which is not reachable through the site
API. The Claude connector's connections live in the same site (`_mcp_grants`, `_mcp_rotations`;
expired ones are pruned as new ones are created), and so do its publish leases (`_mcp_locks`, one
per site while a publish runs; a lease left behind by a crashed replica expires after 300 s, and
taking it over leaves a claim in `_mcp_lock_takeovers` that later takeovers prune).

## Backups

Back up three things: the **document store**, the **blob storage** and the **secrets**
(`server.secret` — without it every session, API token and in-flight deploy becomes invalid,
though no data is lost).

| Component | How |
|---|---|
| SQLite | Online snapshot with the sqlite3 CLI: `sqlite3 /var/lib/backyard/backyard.db ".backup '/backups/backyard-$(date +%F).db'"` (or `VACUUM INTO`). A crash-consistent volume snapshot (VolumeSnapshot, EBS/PD snapshot) also works thanks to the WAL. Restore by stopping the server and replacing the file (remove stale `-wal`/`-shm` files). |
| PostgreSQL | Managed backups/PITR (RDS, Cloud SQL, Azure), or `pg_dump -Fc` / `pg_restore`. All tables are prefixed `backyard_` in `database.postgres.schema`. |
| DynamoDB | Point-in-time recovery and on-demand backups of the table. |
| `fs` storage | Archive or snapshot `storage.fs.root` (`sites/`, `uploads/`, `_meta/`); `restic`, `borg` or volume snapshots. |
| S3 / GCS / Azure Blob | Bucket versioning with lifecycle rules (the Terraform modules enable it), cross-region replication or periodic sync to a backup bucket. |

Per-site backups for site owners: `backyard pull <site>` downloads the deployed files; data can
be exported through the site API (`GET /__backyard/api/db/<collection>` with `limit`/`offset`).

Restoring storage and database to different points in time is safe: uploads whose metadata is
missing are invisible, and a site whose manifest disagrees with storage is repaired by the next
deploy (or `POST /__backyard/api/deploy/sites/<site>/refresh`).

## Scaling to several instances

A single instance (1–2 vCPU) comfortably serves a few thousand employees. To run several
replicas behind a load balancer, every piece of state must be shared:

| Component | Requirement |
|---|---|
| Document store | `postgres` or `dynamodb` (not `sqlite` or `memory`) |
| Blob storage | `s3`, `gcs`, `azure`, or `fs` on a ReadWriteMany volume |
| Pub/sub | `postgres` (LISTEN/NOTIFY) or `redis` (not `memory`) |
| `server.secret` | identical on every instance (and on a Lambda gateway) |

Nothing else is needed: sessions are stateless cookies, tokens are stateless JWTs, and the
realtime hub gossips presence through pub/sub, so session affinity is optional. Things to know:

- **Rate limits are per instance**: with N replicas a user can make up to N times the
  configured rate. Size limits accordingly.
- **Caches** (static objects, "site exists") are per instance and trusted for
  `storage.cache_ttl` (5 s). The instance that commits a deploy purges its own cache at once;
  others catch up within the TTL.
- **Websockets** need load balancer support and an idle timeout of at least 60 s (the server
  pings every 25 s). On Cloud Run, allocate CPU always and raise the request timeout (the GCP
  module uses 60 minutes).
- **Postgres pub/sub** needs a direct (session-mode) connection; a transaction-pooling
  PgBouncer breaks LISTEN.
- **Postgres connections**: each instance opens up to `database.postgres.max_conns` (20) for
  documents, plus — with Postgres pub/sub — one LISTEN connection and a publishing pool of 2–4
  (`pool_max_conns` in the pub/sub DSN overrides it). Keep replicas × that total below the
  database's `max_connections`.
- **Cloud Run** rejects HTTP/1 responses that declare more than 32 MiB; Backyard detects Cloud
  Run (`K_SERVICE`) and streams larger site files and uploads chunked, and shortens range
  responses to 32 MiB (clients fetch the rest).
- The `database.max_docs_per_site` quota is soft across instances (it can be overshot slightly).
- Autoscale on CPU or request concurrency; `server.shutdown_timeout` (20 s) lets in-flight
  requests finish, and websockets are closed so clients reconnect to another instance.

## Upgrades

Backyard is one binary (or image). To upgrade:

1. Read the [changelog](../CHANGELOG.md) for breaking changes.
2. Replace the binary or image tag and restart (rolling restarts are fine with several
   instances).
3. PostgreSQL migrations run on start (`database.postgres.auto_migrate: true`), serialized
   across instances by an advisory lock; SQLite and DynamoDB need no migration step.
4. Redeploy a standalone or Lambda gateway with the same version.
5. The Cloudflare edition upgrades by re-running setup: `backyard cloudflare setup` with the new
   CLI release (it installs that release's package), `npx backyard-cloudflare@latest setup` (once
   a Backyard release has published it to npm), or `npm run setup` in an updated checkout. Every
   earlier choice is kept. With Cloudflare Access, run `backyard cloudflare access enable` (or
   `terraform apply` of the Cloudflare module) again when the bypass list changes — for the
   [Claude connector](claude-connector.md) it now holds nine paths; updating a bypass
   application the CLI created needs `BACKYARD_CF_API_TOKEN` again (or the destinations added in
   the dashboard); `backyard cloudflare status` reports missing ones as `access-connector`.
6. Terraform kits: `terraform apply` with the new module version, which may change proxy
   exemptions (as for the Claude connector's machine paths in the AWS, GCP and Cloudflare
   modules).

Sites never need changes: the SDK is served by the server, so pages pick up the new version on
the next load.

## Rotating secrets

| Secret | Effect of rotation |
|---|---|
| `server.secret` | Everyone is signed out, every API token, deploy id and upload URL becomes invalid, and every Claude connector connection ends (people connect again; connectors added with "Register automatically" must be removed and added again in Claude). Run `backyard login` again; update CI tokens minted with `backyard token create`. Static tokens are unaffected. |
| A static token | Add the new hash to `auth.tokens`, update the pipeline, remove the old hash. |
| OIDC client secret | Update `auth.oidc.client_secret`; sessions survive. |
| Provider and database credentials | Update the environment and restart; nothing else changes. |
