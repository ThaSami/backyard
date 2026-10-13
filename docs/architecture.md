# Architecture

Backyard is one Go binary (`backyard`) that is the server, the deploy gateway, the CLI and the
MCP server, plus a TypeScript Worker that implements the same contract on Cloudflare. The
normative description of every URL and wire format is the [specification](spec/SPEC.md).

## Components

```
                      ┌──────────── company IdP (Okta · Entra ID · Google · …) ────────────┐
                      │                                                                     │
 browser ──HTTPS──▶ [ built-in OIDC │ Google IAP │ AWS ALB │ trusted proxy │ none ] ──▶ backyard serve
                                                                                         │
            ┌────────────────────────────────────────────────────────────────────────────┤
            │ host routing: hub host · <site>.<base_domain> · /s/<site>/ (path mode)      │
            ├─ static sites, uploads ──────────────▶ blob store   fs │ s3 │ gcs │ azure
            ├─ /__backyard/api/db, files metadata ─▶ docstore     sqlite │ postgres │ dynamodb
            ├─ /__backyard/api/ws ─────────────────▶ realtime hub ─▶ pub/sub  memory │ postgres │ redis
            ├─ /__backyard/api/ai ─────────────────▶ AI router    Anthropic · OpenAI(-compatible) · Azure OpenAI
            │                                                     · Bedrock · Gemini/Vertex · Ollama
            ├─ /__backyard/api/warehouse ──────────▶ warehouse    BigQuery · Postgres/Redshift · Athena
            └─ /__backyard/api/deploy ─────────────▶ deploy gateway (embedded)

 CLI · CI · agent ──▶ deploy gateway: embedded │ `backyard gateway` │ AWS Lambda ──presigned PUT──▶ bucket
                 └──▶ or direct sync: aws s3 sync · gcloud storage rsync · az storage blob sync · rsync
```

| Package | Responsibility |
|---|---|
| `cmd/backyard` | The binary (cobra CLI): `serve`, `gateway`, `dev`, `deploy`, `mcp`, … |
| `cmd/backyard-lambda` | The deploy gateway as an AWS Lambda function |
| `internal/app` | Wiring: configuration → components → `http.Handler`; run and graceful shutdown |
| `internal/config` | Schema, loading (`${ENV}` expansion), defaults, validation |
| `internal/server` | HTTP: host/path routing, static serving, site API, hub, health, metrics, rate limits |
| `internal/auth` | Authenticators (oidc, iap, alb, header, none), sessions, API and static tokens, CLI login |
| `internal/keys` | Per-purpose keys derived from `server.secret` (HKDF-SHA256) |
| `internal/identity` | The `User` type, claim mapping, people directory, user cache |
| `internal/docstore` (+ backends) | JSON document store, reference query evaluator, quotas, conformance suite |
| `internal/blob` (+ backends) | Object storage: fs, S3, GCS, Azure |
| `internal/pubsub` (+ backends) | Cross-instance fan-out: memory, Postgres, Redis |
| `internal/realtime` | Websocket hub: subscriptions, rooms, presence gossip |
| `internal/ai` (+ providers, `router`) | Provider-neutral AI API, alias routing, SSRF-guarded image fetching |
| `internal/warehouse` (+ sources, `service`) | Read-only SQL, statement guard, named queries |
| `internal/sites` | Site names, metadata, manifests, registry |
| `internal/deploy` | Gateway protocol (server), Go client, ignore rules, Lambda adapter |
| `internal/ratelimit` | Token buckets and daily AI token budgets |
| `internal/cli`, `internal/clientconfig`, `internal/mcpserver` | CLI commands, local profiles, MCP tools |
| `web` | Embedded SDK (`web/sdk/backyard.{js,mjs,d.ts}`) and hub UI (`web/hub`) |
| `skills`, `templates` | Embedded agent docs and `backyard init` templates |
| `deploy/cloudflare` | The Workers edition |

`internal/app` builds, in order: blob storage, the site registry, the document store (wrapped
with size and quota limits), the pub/sub broker, the people directory, the authenticator, the
realtime hub, the AI router, the warehouse service, the embedded deploy gateway and the HTTP
server; `Close` releases them in reverse. With `backyard gateway` only storage, a bearer-only
authenticator and the gateway are built.

## Request flow

Every request passes the same middleware: request id, security headers, client resolution
(`X-Forwarded-*` only from `server.trusted_proxies`), panic recovery, metrics and the optional
access log. Then:

1. **Path canonicalization** — `//`, `/./`, `/../` are redirected or rejected.
2. **Health** — `/__backyard/healthz` and `/readyz` answer on any host.
3. **Host classification** — the hub host (`server.base_domain` or the public URL's host), a
   site (one DNS label under the base domain, host mode), or unknown (`404`). In path mode,
   `/s/<site>/…` on the hub is a site and the prefix is stripped.
4. **Site requests** — `/__backyard/backyard.{js,mjs,d.ts}` (SDK with a per-request context
   preamble, no auth), `/__backyard/auth/*` (redirected to the hub), `/__backyard/api/*` (site
   API: authenticate → CSRF check → API rate limit → handler), `/__backyard/files/*` (uploads),
   any other `/__backyard/*` (404), else a static file.
5. **Hub requests** — SDK, `/__backyard/auth/*` (login flows), `/__backyard/api/info`,
   `/__backyard/api/deploy/*` (gateway), `/__backyard/metrics`, `/__backyard/api/*` (hub API:
   `me`, `sites`, `tokens`), else the hub UI, `/docs` and `/llms*.txt`.

**A page load:** the browser asks for `https://lunch-vote.backyard.acme.com/` → the
authenticator resolves the user (session cookie, proxy assertion, header or bearer token) or
redirects to the login → static serving resolves `index.html` (clean URLs, `200.html` SPA
fallback, `404.html`) from `sites/lunch-vote/` through a small LRU cache → `ETag`/`304` and
`Cache-Control: no-cache`.

**An API write:** `POST /__backyard/api/db/votes` → authenticate → `X-Backyard: 1` and Origin
check → rate limits (`api`, `db_write`) → document store (validated, size/quota-checked) →
realtime event published to the pub/sub topic `rt.lunch-vote.db` → every instance pushes a `db`
frame to its subscribers → `201` with the flattened document.

**A websocket:** `GET /__backyard/api/ws` → authenticate → Origin check → upgrade → `hello` →
subscriptions and rooms; presence is gossiped between instances through pub/sub.

**An `api` deploy:** the CLI collects and hashes files (ignore rules) → `POST
…/sites/<site>/deploys` (auth, ownership, limits, diff against the manifest) → signed deploy id
and upload URLs bound to each file's size and SHA-256 (presigned bucket URLs, or the gateway's
own signed `PUT …/uploads/<token>`) → parallel `PUT`s → `POST …/deploys/<id>/commit` (verify
sizes — a mismatch fails with `409`, and with Azure SAS uploads also deletes the object —,
record the digests of the bytes actually stored, write manifest and metadata, delete files not
in the list) → the server purges its caches.

**A browser → CLI login:** `backyard login` listens on `127.0.0.1:<port>` and opens
`/__backyard/auth/cli?redirect_uri=…&state=…` → the page is authenticated by the IdP or proxy →
(a cross-site navigation must be confirmed first) → Backyard mints an API token and redirects
to the loopback URL → the CLI stores it.

## Data layout

**Blob storage** (after an optional prefix):

| Key | Content |
|---|---|
| `sites/<site>/<path>` | Site files |
| `_meta/sites/<site>.json` | `SiteMeta`: title, description, created/updated by and at, last deploy id, file count, bytes, owners |
| `_meta/manifests/<site>.json` | `{site, updated_at, files: [{path, size, sha256, etag}]}` |
| `uploads/<site>/<id>/<name>` | Uploaded files and generated images |

The hub's site list is `_meta/sites/*` plus first-level prefixes under `sites/` (sites
deployed by a plain rsync appear too).

**Document store** — documents are addressed by `(site, collection, id)`:

| Namespace | Collections |
|---|---|
| `<site>` | user collections (`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`), and system collections starting with `_`: `_files` (upload metadata) |
| `_system` | `_users` (user cache for `GET /users/{email}`), `_audit` (token issuance records) |

System collections are never listed or reachable through the site API. Each document stores its
JSON data plus `created_at`, `updated_at` (microseconds, UTC), `created_by`, `updated_by` and
`version`; the wire form flattens data, `id` and `_meta`.

**Pub/sub topics:** `rt.<site>.db` (database changes) and `rt.<site>.room.<room>` (messages
and presence gossip).

**Client side:** `~/.config/backyard/config.json` (CLI profiles, mode 0600), `.backyard.json`
and `.backyardignore` (per project), `./.backyard-data` (`serve --dev` and `backyard dev`).

## Two editions, one contract

| | Go server | Cloudflare Worker |
|---|---|---|
| Code | `cmd/`, `internal/` | `deploy/cloudflare/src` |
| State | blob store + docstore + pub/sub | `SiteDO` per site, `RegistryDO`, optional R2 |
| Shared | SDK (`web/sdk`), hub UI (`web/hub`), skill (`skills/`), query test cases (`internal/docstore/conformance/querycases.json`) | bundled from the same files at deploy time |

The black-box conformance suite (`conformance/`, run with `node --test`) and the shared query
cases run against both; any change to the public contract lands in the spec, both editions and
the suite together.
