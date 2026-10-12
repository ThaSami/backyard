# Backyard — Design Specification (v0.1)

> Source of truth for implementers. If code and this document disagree, fix one of them
> deliberately — never silently. Inspired by Shopify's internal "Quick" platform
> (https://shopify.engineering/quick): *a folder of HTML becomes a URL that only
> people inside your company can see, and every site gets a zero-config backend*.

## 0. Principles

1. **A site is a folder.** No frameworks, pipelines or config files are required. Deploying =
   copying a folder (rsync / `aws s3 sync` / `gcloud storage rsync` / `az storage blob sync`
   / the Backyard API). Overwrite to update. Built output (`dist/`) is fine too.
2. **Zero-config client API.** Every site can `<script src="/__backyard/backyard.js">` and get
   `backyard.db`, `backyard.files`, `backyard.ai`, `backyard.warehouse`, `backyard.socket`,
   `backyard.id`. All secrets live on the server.
3. **Small, fixed set of capabilities.** Database (+realtime), file uploads, AI, data warehouse,
   websockets, identity. No custom backends, no cron. Saying no is a feature.
4. **Trust bubble.** Every request is authenticated by the company IdP (Okta/OIDC, Google IAP,
   AWS ALB, any trusted proxy) — or explicitly not (`auth.mode: none`, for labs/dev).
5. **Pluggable everywhere, boring by default.** One Go binary. Storage: fs | s3 | gcs | azure.
   Database: sqlite | postgres | dynamodb | memory. Pub/sub: memory | postgres | redis.
   AI: anthropic | openai(-compatible) | azure-openai | bedrock | gemini/vertex.
   Warehouse: bigquery | postgres(-compatible, incl. Redshift) | athena.
6. **Agent-native.** `backyard init` drops `CLAUDE.md`, `AGENTS.md` and a Claude skill into the
   project; `backyard mcp` is an MCP server; the hub serves `/llms.txt`.
7. **Runs anywhere.** AWS, GCP, Azure (Terraform), any VM or Kubernetes without a bucket
   (fs storage + rsync; docker-compose, Helm, systemd), or Cloudflare's free tier (§16).
8. **Enterprise-ready.** Terraform for AWS/GCP/Azure/Cloudflare, Helm, docker-compose; OIDC/SSO; admin
   groups; optional site ownership; rate limits; audit logs; Prometheus metrics; stateless
   horizontally-scalable server when backed by postgres/dynamodb + redis/postgres pubsub.

## 1. Components

```
                 ┌──────────── company IdP (Okta / Entra / Google) ───────────┐
                 │                                                            │
 browser ──TLS──▶ [ IAP | ALB-OIDC | trusted proxy | none ] ──▶ backyard serve (Go)
                                                                ├─ static sites  ◀── blob store (fs|s3|gcs|azure)
 CLI / agent ──▶ deploy gateway (embedded at /__backyard/api/deploy,                ▲
                 or standalone: AWS Lambda / `backyard gateway`) ──── presigned PUT ┘
                                                                ├─ /__backyard/api/*  ─▶ docstore (sqlite|postgres|dynamodb)
                                                                ├─ websockets         ─▶ pubsub (memory|postgres|redis)
                                                                ├─ AI proxy           ─▶ providers
                                                                └─ warehouse          ─▶ bigquery|postgres|athena
```

Go packages (module `github.com/thasami/backyard`):

| Package | Responsibility |
|---|---|
| `cmd/backyard` | single binary: server + CLI (cobra) |
| `cmd/backyard-lambda` | AWS Lambda build of the deploy gateway |
| `internal/config` | config types, load (YAML + `${ENV}` expansion), defaults, validation |
| `internal/blob` (+`fs`,`s3`,`gcs`,`azure`,`backends`) | object storage |
| `internal/docstore` (+`memory`,`sqlite`,`postgres`,`dynamodb`,`backends`,`conformance`) | JSON document DB |
| `internal/pubsub` (+`memory`,`redis`,`postgres`,`backends`) | cross-instance fan-out |
| `internal/realtime` | websocket hub: db subscriptions, rooms, presence |
| `internal/ai` (+`anthropic`,`openai`,`bedrock`,`gemini`,`router`) | LLM / image / embedding proxy |
| `internal/warehouse` (+`bigquery`,`sqlsource`,`athena`,`service`) | read-only analytics queries |
| `internal/identity` | `User` type, claim mapping, directory |
| `internal/auth` | authenticators, sessions, tokens, login flows |
| `internal/sites` | site names, metadata, manifests, registry |
| `internal/deploy` | deploy gateway (server) + Go client + Lambda adapter |
| `internal/ratelimit` | token buckets |
| `internal/server` | HTTP: host routing, static serving, APIs, hub |
| `internal/app` | wiring: config → components → `http.Handler` |
| `internal/cli`, `internal/clientconfig`, `internal/mcpserver` | CLI commands, local CLI config, MCP server |
| `web` | embedded client SDK (`web/sdk/backyard.{js,mjs,d.ts}`) and hub UI (`web/hub/`) |
| `deploy/cloudflare` | Cloudflare Workers edition (TypeScript, §16) |
| `deploy/terraform/{aws,gcp,azure,cloudflare}`, `deploy/helm`, `deploy/docker-compose`, `deploy/systemd` | infrastructure |
| `conformance` | black-box API test-suite shared by both editions (§17) |
| `skills` | embedded Claude skill (`skills/backyard/…`) |
| `templates` | embedded `backyard init` templates |

## 2. URLs and routing

* `server.base_domain` e.g. `backyard.acme.com`; `server.public_url` e.g. `https://backyard.acme.com`.
* **Host mode** (default): hub at `https://<base_domain>/`, site `foo` at `https://foo.<base_domain>/`.
  Only single-label subdomains are sites. `foo.localhost` works out of the box for local dev
  (browsers resolve `*.localhost` to loopback) when `base_domain: localhost`.
* **Path mode** (`server.url_mode: path`, for environments without wildcard DNS/TLS): site `foo`
  at `https://<base_domain>/s/foo/`. Sites then share an origin (documented trade-off).
* Request host is taken from `Host` (or `X-Forwarded-Host` only when the peer is in
  `server.trusted_proxies`). Port is ignored for matching.
* Reserved path prefix on **every** host: `/__backyard/`. Site files can never shadow it.

Site host (`foo.<base>` or `/s/foo` prefix):

| Path | Purpose |
|---|---|
| `/__backyard/backyard.js` | classic-script SDK, defines `window.backyard` |
| `/__backyard/backyard.mjs` | ES module SDK (`import backyard from "/__backyard/backyard.mjs"`) |
| `/__backyard/backyard.d.ts` | TypeScript declarations |
| `/__backyard/api/...` | site-scoped JSON APIs (§5) |
| `/__backyard/api/ws` | websocket (§6) |
| `/__backyard/files/{id}/{name}` | uploaded file bytes (Range supported) |
| `/__backyard/auth/*` | redirects to the hub's auth endpoints (host mode) |
| anything else | static file from the site folder (§3) |

Hub host:

| Path | Purpose |
|---|---|
| `/` | hub UI: site directory/search, drag-and-drop deploy, CLI setup instructions |
| `/docs` | API docs (renders `skills/backyard/references/api.md`) |
| `/llms.txt`, `/llms-full.txt` | agent-oriented docs (SKILL.md; SKILL.md + all references) |
| `/__backyard/backyard.{js,mjs,d.ts}` | SDK (hub UI uses it too) |
| `/__backyard/api/info` | server info (§7.1, same as deploy `info`) |
| `/__backyard/api/me` | current user |
| `/__backyard/api/sites` | site list (`{sites:[SiteMeta]}`) |
| `/__backyard/api/tokens` | `POST` mint a personal API token (`{name, ttl_hours}` → `{token, expires_at}`) |
| `/__backyard/api/deploy/...` | embedded deploy gateway (§7) |
| `/__backyard/auth/{login,callback,logout,cli}` | auth flows (§8) |
| `/__backyard/healthz` | liveness `200 ok` (no auth) |
| `/__backyard/readyz` | readiness: pings db + blob (no auth) |
| `/__backyard/metrics` | Prometheus (only if `telemetry.metrics` and no `telemetry.metrics_listen`) |

`/__backyard/healthz` and `/readyz` are served on any host without authentication.

### 2.1 SDK context resolution

The server prepends a one-line preamble to every SDK response
(`backyard.js`/`backyard.mjs`), computed per request:

```js
globalThis.__BACKYARD__ = {"v":1,"mode":"host","site":"foo","prefix":"/s/","hub":"https://backyard.acme.com"};
```

`site` is the site the script was requested for (`null` on the hub, or in path mode when
requested at the root `/__backyard/backyard.js`). The SDK computes its API base as:

1. `globalThis.BACKYARD_CONFIG.apiBase` if a page set it explicitly (escape hatch);
2. host mode → `<page origin>/__backyard`;
3. path mode → site from the preamble, else from `location.pathname` matching
   `^<prefix>([^/]+)/` → `<origin><prefix><site>/__backyard`; no site → `<origin>/__backyard` (hub).

The websocket URL is derived from the API base (`ws(s)://…/__backyard/api/ws`). Pages should
always include the SDK with the absolute path `/__backyard/backyard.js`; this works in both modes.
The SDK files in `web/sdk/` are complete standalone files (no build step at serve time) so the
Go server and the Cloudflare Worker serve byte-identical SDKs (plus the preamble).

## 3. Static serving

Blob keys: `sites/<site>/<path>` (see §9). For request path `p` (URL-decoded, cleaned, no `..`):

1. If `p` ends with `/` → try `p + "index.html"`.
2. Else try `p`; if missing and `p + "/index.html"` exists → `301` to `p + "/"` (preserve query);
   else if `p + ".html"` exists → serve it (clean URLs).
3. If nothing matched: if `200.html` exists in the site root → serve it with 200 (SPA fallback);
   else if `404.html` exists → serve it with 404; else a built-in 404 page.
4. Unknown site (no objects under `sites/<site>/`) → built-in "site not found" page (404) with a
   link to the hub.

Headers: `Content-Type` from extension (fallback `application/octet-stream`; `text/*` get
`; charset=utf-8`), `ETag` (blob etag or size+mtime hash), `Last-Modified`, conditional GET
(304), `Cache-Control: no-cache` (sites are overwritten in place), `X-Content-Type-Options:
nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`. **No** `X-Frame-Options` (embedding
sites in sites is a feature). `HEAD` supported. Range requests supported.

A small in-memory LRU cache (`storage.cache_bytes`, `storage.cache_ttl`) fronts blob reads for
objects ≤ 1 MiB.

## 4. Identity

```jsonc
// User (JSON shape returned by /me and embedded in presence/messages)
{
  "id": "alice@acme.com",          // stable: lowercased email, else "sub:<issuer-sub>", else "anon:<rand>"
  "email": "alice@acme.com",
  "name": "Alice Smith",
  "title": "Staff Engineer",       // optional (mapped claim / directory)
  "team": "Platform",              // optional
  "slack": "@alice",               // optional
  "avatar_url": "https://…",       // optional
  "groups": ["eng", "backyard-admins"],
  "anonymous": false,
  "admin": false,
  "extra": {"location": "Ottawa"}  // optional extra mapped claims
}
```

* Claims → fields via `identity.claims` (defaults: `name`, `email`, `title`, `department`→team,
  `slack`, `picture`→avatar_url, `groups`).
* Optional directory (`identity.directory`: CSV or JSON file keyed by email) enriches/overrides.
* Every authenticated API request upserts the user into the system user cache (throttled to once
  per user per 10 min per instance) so `GET /users/{email}` can resolve people who have used
  Backyard.
* `auth.mode: none`: anonymous user `{id: "anon:<16 hex>", name: "Guest <4 hex>", anonymous:
  true}` keyed by the `backyard_anon` cookie (Domain `.<base_domain>`, 1 year).

## 5. Site API (`/__backyard/api`, site-scoped)

Common rules:

* JSON in/out (`Content-Type: application/json`). Errors: HTTP status +
  `{"error": {"code": "not_found", "message": "…"}}`. Codes: `bad_request`, `unauthenticated`,
  `forbidden`, `not_found`, `conflict`, `too_large`, `rate_limited`, `unsupported`,
  `upstream_error`, `internal`.
* **CSRF:** every non-GET/HEAD request must carry header `X-Backyard: 1`; if an `Origin` header
  is present it must equal the request's own origin (host mode) or the hub origin (path mode).
  Otherwise `403 forbidden`. The SDK always sends the header.
* `401` responses include `"login_url"` inside `error` when a browser login is possible.
* Rate limits (§11) return `429` with `Retry-After`.

### 5.1 Identity

* `GET /me` → `User`
* `GET /users/{email}` → `User` | 404
* `GET /site` → `SiteMeta` of the current site

### 5.2 Database

Collections are per site. Collection names: `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (names starting
with `_` are reserved for the system). Document ids: `^[A-Za-z0-9_\-:.]{1,128}$`; generated ids
are 26-char ULIDs (time-sortable).

Wire shape of a document is **flattened**:

```json
{ "id": "01J…", "title": "Hello", "status": "draft",
  "_meta": { "created_at": "2026-…Z", "updated_at": "2026-…Z",
             "created_by": "alice@acme.com", "updated_by": "alice@acme.com", "version": 1 } }
```

On write, the body is the plain data object. Keys `id` and `_meta` in a write body are not
stored as data (`id` is used as the document id on create / must match on put). Max document size
`database.max_doc_bytes` (default 256 KiB) of JSON data.

| Method & path | Body | Result |
|---|---|---|
| `GET /db` | | `{"collections": ["posts", …]}` |
| `POST /db/{c}` | data (optional `id`) | `201` doc; `409 conflict` if `id` exists |
| `GET /db/{c}?limit=&offset=&order=field:asc,field2:desc` | | `{"docs": […], "has_more": bool}` |
| `POST /db/{c}/query` | Query | `{"docs": […], "has_more": bool}` |
| `GET /db/{c}/{id}` | | doc \| 404 |
| `PUT /db/{c}/{id}` | data | doc (create or replace; version+1) |
| `PATCH /db/{c}/{id}` | partial data | doc (shallow merge; a `null` value **deletes** that key); 404 if missing |
| `DELETE /db/{c}/{id}` | | `204` (also when missing) |

Query:

```json
{ "where": [ {"field": "status", "op": "==", "value": "draft"},
             {"field": "votes",  "op": ">=", "value": 3} ],
  "order_by": [ {"field": "votes", "dir": "desc"} ],
  "limit": 50, "offset": 0 }
```

* `field` is a dot path into user data (`author.name`); segments match
  `^[A-Za-z_][A-Za-z0-9_-]{0,63}$`, max depth 8. System fields: `id`, `_meta.created_at`,
  `_meta.updated_at`, `_meta.created_by`, `_meta.updated_by`.
* ops: `==`, `!=`, `<`, `<=`, `>`, `>=`, `in`, `not-in` (value = array of ≤ 30 scalars),
  `contains` (field is an array containing the scalar value).
* Values are JSON scalars (string, number, boolean, null). **Comparisons only match values of the
  same JSON type** (`"3"` never equals `3`; `<` on a string field with a number value matches
  nothing). A missing field never matches `==`, `!=`, `<`…`>=`, `in`, `not-in`, `contains`;
  `== null` matches explicit `null` only. `!=`/`not-in` require the field to exist.
* Ordering within a type: numbers numerically, strings by byte order, `false < true`. Missing /
  `null` values sort **first** ascending and **last** descending. Mixed-type ordering is
  unspecified. Ties are broken by `id` ascending. Default order: `_meta.created_at asc, id asc`.
* `limit` default 100, max `database.max_list` (default 1000). `has_more` true if more matched.
* Every successful write publishes a realtime event (§6).

### 5.3 Files

| Method & path | Body | Result |
|---|---|---|
| `POST /files` | `multipart/form-data`, field `file` (+ optional `name`) | `201` FileInfo |
| `GET /files?limit=&offset=` | | `{"files": [FileInfo]}` newest first |
| `GET /files/{id}` | | FileInfo \| 404 |
| `DELETE /files/{id}` | | `204` |

`FileInfo = {id, name, url, size, content_type, created_at, created_by}`; `url` is the
site-relative path `/__backyard/files/{id}/{urlescaped name}` (path mode: prefixed with
`/s/{site}`). Max size `files.max_upload_bytes` (default 100 MiB). Bytes stored at blob key
`uploads/<site>/<id>/<name>`; metadata in the system collection `_files`.

### 5.4 AI

`POST /ai/chat`

```jsonc
{ "messages": [ {"role": "user", "content": "Summarize my tasks"} ],
  // content may also be [{ "type":"text","text":"…" }, { "type":"image","data":"<base64>","media_type":"image/png" }, { "type":"image","url":"https://…" }]
  "model": "smart",          // alias from ai.models; default ai.default_model
  "system": "You are…",      // optional; role:"system" messages are also accepted and hoisted
  "max_tokens": 4096,        // optional, capped by ai.max_tokens
  "temperature": 0.2,        // optional, ignored by providers/models that reject it
  "json": false,             // optional: ask for a JSON object response
  "stream": false }
```

* non-stream → `{"content": "…", "model": "smart", "stop_reason": "end_turn", "usage": {"input_tokens": 12, "output_tokens": 40}}`
* `stream: true` → `text/event-stream`; each event is one `data: <json>\n\n` line:
  `{"type":"text","text":"…"}` (repeated), then `{"type":"done","model":…,"stop_reason":…,"usage":{…}}`,
  or `{"type":"error","error":{"code":…,"message":…}}`.
* `stop_reason` normalized to: `end_turn`, `max_tokens`, `stop_sequence`, `refusal`, `other`.

`POST /ai/image` `{prompt, model?, size?: "1024x1024", n?: 1}` → `{"images": [{"url": "/__backyard/files/…", "content_type": "image/png"}]}` (images are stored as site files).

`POST /ai/embed` `{input: string | string[], model?}` → `{"model": "embed", "embeddings": [[…], …]}`.

`GET /ai/models` → `{"default": "smart", "models": [{"name": "smart", "kind": "chat", "provider": "anthropic", "description": "…"}]}`
(underlying provider model ids are exposed only if `ai.expose_model_ids`).

Clients choose **aliases**, never raw provider model ids, unless `ai.allow_raw_models` is true
(then `"provider/model-id"` is accepted).

### 5.5 Warehouse

`POST /warehouse/query`
`{ "sql": "SELECT … WHERE team = ?", "params": ["growth"], "source": "default" }` or
`{ "query": "weekly_active_users", "params": {"team": "growth"} }` (named query)
→ `{"columns": [{"name": "day", "type": "DATE"}], "rows": [{"day": "2026-01-01", …}], "row_count": 7, "truncated": false}`

* Raw SQL only if `warehouse.allow_raw_sql`. Statements must start with `SELECT`/`WITH`
  (after comments/whitespace) unless `warehouse.allow_any_statement`. Postgres sources run in a
  `READ ONLY` transaction. Positional params use `?` placeholders in raw SQL (translated per
  engine); named queries use `@name` placeholders.
* `GET /warehouse` → `{"sources": [{name, type, description}], "queries": [{name, description, params: [..]}]}`.

### 5.6 JavaScript SDK (`window.backyard`)

Exact public surface (TypeScript notation; `web/sdk/backyard.d.ts` is normative once written).
All methods return Promises unless noted; failures throw `BackyardError {name:"BackyardError",
code, status, message, loginUrl?}` with `code` from §5. On `401` with `login_url` the SDK
redirects the top-level page to it (disable with `backyard.configure({autoLogin:false})`).

```ts
backyard.ready: Promise<void>                    // resolves when context is resolved (immediate)
backyard.site: { name: string|null, url: string, apiBase: string, mode: "host"|"path" }
backyard.configure(opts: { apiBase?: string, autoLogin?: boolean }): void
backyard.me(): Promise<User>                     // shorthand for backyard.id.me()

// Identity
backyard.id.me(): Promise<User>                  // cached after first call
backyard.id.lookup(email: string): Promise<User|null>

// Database
const posts = backyard.db.collection("posts")   // sync, returns Collection
await posts.create(data, { id? }) → Doc          // Doc = { id, ...data, _meta }
await posts.get(id) → Doc|null
await posts.set(id, data) → Doc                  // create or replace (PUT)
await posts.update(id, patch) → Doc              // shallow merge; null deletes a key
await posts.delete(id) → void
await posts.list({ limit?, offset?, orderBy?: string, order?: "asc"|"desc" }) → Doc[]
posts.where(field, op, value) → Query            // chainable; ops as in §5.2
     .where(...).orderBy(field, "desc").limit(20).offset(0)
await query.get() → Doc[]                        // also: await posts.find(queryObject) → Doc[]
await query.first() → Doc|null
const unsubscribe = posts.subscribe({ onCreate?(doc), onUpdate?(doc), onDelete?(id), onChange?(event) })
const stop = posts.watch(callback: (docs: Doc[]) => void, query?: Query) // live query: calls back with
                                                 // the full result now and after every change (debounced 50 ms)
await backyard.db.collections() → string[]

// Files
await backyard.files.upload(file: File|Blob, { name? }) → FileInfo   // FileInfo = {id,name,url,size,content_type,created_at,created_by}
await backyard.files.list({ limit?, offset? }) → FileInfo[]
await backyard.files.get(id) → FileInfo|null
await backyard.files.delete(id) → void

// AI
await backyard.ai.chat(input: string | Message[], { model?, system?, maxTokens?, temperature?, json? })
      → { content: string, model, stop_reason, usage, json?: any }   // json parsed when opts.json
for await (const chunk of backyard.ai.stream(input, opts)) { chunk.text }   // async iterable; also
      backyard.ai.stream(input, { ...opts, onText(text) }) and `await stream.done` → final result
await backyard.ai.image(prompt, { model?, size?, n? }) → { url, content_type, images: [{url,content_type}] }
await backyard.ai.embed(input: string|string[], { model? }) → number[] | number[][]  // mirrors input shape
await backyard.ai.models() → { default, models: ModelInfo[] }

// Warehouse
await backyard.warehouse.query(sql, params?: any[], { source? }) → { rows, columns, row_count, truncated }
await backyard.warehouse.named(name, params?: object) → { rows, columns, row_count, truncated }
await backyard.warehouse.sources() → { sources, queries }

// Realtime rooms (websockets)
const room = backyard.socket.room("lobby", { state?: object })   // sync; joins lazily/automatically
room.on("message", (data, from) => {})          // from = { cid, user: UserSummary }
room.on("join" | "leave" | "update", (member) => {})
room.on("members", (members: Member[]) => {})   // full list (excluding self) after any presence change
room.members: Member[]                           // current list (excluding self)
room.self: Member|null
room.send(data, { to?: cid, echo?: boolean }): void
room.setState(state: object): void               // replace presence state
room.updateState(partial: object): void          // shallow-merge presence state
room.off(event, handler): void
room.leave(): void
backyard.socket.connected: boolean
backyard.socket.on("open" | "close", handler)   // connection status
```

The SDK keeps one websocket per page, created on first use (subscribe/watch/room), reconnects
with exponential backoff (0.5 s → 10 s, jitter) and transparently re-subscribes and re-joins rooms
(with the latest presence state) after reconnecting.

## 6. Realtime websocket (`GET /__backyard/api/ws`)

JSON text frames, max 64 KiB. Server → client first frame:
`{"t":"hello","cid":"c_8f3a…","user":{User}}`.

Client → server (`id` is an optional client correlation id echoed in `ack`/`joined`/`error`):

| Frame | Meaning |
|---|---|
| `{"t":"sub","id":"s1","collection":"posts"}` | subscribe to db changes of a collection → `{"t":"ack","id":"s1"}` |
| `{"t":"unsub","id":"s1"}` | unsubscribe → ack |
| `{"t":"join","id":"r1","room":"lobby","state":{…}}` | join room with presence state → `{"t":"joined","id":"r1","room":"lobby","self":Member,"members":[Member…]}` (members excludes self) |
| `{"t":"leave","room":"lobby"}` | leave room |
| `{"t":"state","room":"lobby","state":{…}}` | replace own presence state |
| `{"t":"send","room":"lobby","data":any,"to":"c_…"?, "echo":false}` | broadcast (or direct) message |
| `{"t":"ping"}` | → `{"t":"pong"}` |

Server → client:

| Frame | Meaning |
|---|---|
| `{"t":"db","sub":"s1","collection":"posts","op":"create"\|"update"\|"delete","doc":{flattened doc}\|null,"doc_id":"…"}` | db change |
| `{"t":"presence","room":"lobby","op":"join"\|"leave"\|"update","member":Member}` | presence |
| `{"t":"msg","room":"lobby","from":{"cid":"…","user":UserSummary},"data":any}` | message |
| `{"t":"error","id"?:…,"error":{"code","message"}}` | error |

`Member = {"cid","user":UserSummary,"state":{…},"joined_at":RFC3339}`;
`UserSummary = {id, name, email, avatar_url}`. Room names `^[A-Za-z0-9_\-:.]{1,128}$`.
Limits: 50 rooms & 100 subscriptions per connection; 60 msgs/s sustained (burst 120) per
connection; presence state ≤ 8 KiB. Server pings every 25 s; idle connections without pong for
60 s are closed.

Multi-instance: every instance publishes to the pub/sub broker on topics
`rt.<site>.db` and `rt.<site>.room.<room>`; presence is gossiped (join/leave/update/heartbeat
every 20 s, member expiry 60 s, `sync` request on join) so each instance knows all members.

## 7. Deploy gateway protocol

Base URL `G` = `<public_url>/__backyard/api/deploy` (embedded) or the standalone gateway root
(`backyard gateway` or AWS Lambda function URL). Auth: `Authorization: Bearer <token>` where token
is a Backyard API token (§8.3), a static token from `auth.tokens`, or (if `auth.oidc` configured)
an IdP-issued JWT obtained via device flow. On the embedded gateway the normal browser session /
proxy identity is accepted as well (used by the hub's drag-and-drop deploy).

### 7.1 `GET G/info` (no auth)

```json
{ "name": "Backyard", "version": "0.1.0",
  "public_url": "https://backyard.acme.com", "base_domain": "backyard.acme.com", "url_mode": "host",
  "gateway_url": "https://…/__backyard/api/deploy",
  "auth": { "mode": "oidc", "cli_login_url": "https://backyard.acme.com/__backyard/auth/cli",
            "device": { "issuer": "https://acme.okta.com/oauth2/default", "client_id": "0oa…", "scopes": ["openid","profile","email","offline_access"] } },
  "storage": { "type": "s3", "bucket": "acme-backyard", "prefix": "", "region": "us-east-1" },
  "methods": ["api", "s3"], "rsync_target": "",
  "limits": { "max_site_bytes": 524288000, "max_files": 10000, "max_file_bytes": 104857600 } }
```

`storage` reveals only what direct-sync clients need (`type`, `bucket`/`container`, `account`,
`prefix`, `region`), and only if `deploy.expose_storage` (default true).

### 7.2 Sites

* `GET G/me` → `User` (auth required; lets CLIs verify a token)
* `GET G/sites` → `{"sites": [SiteMeta]}`
* `GET G/sites/{site}` → `SiteMeta` | 404
* `GET G/sites/{site}/files` → `{"files": [{"path","size","sha256"?,"updated_at"}]}`
* `GET G/sites/{site}/files/{path…}` → raw bytes (for `backyard pull` / agents reading other sites)
* `DELETE G/sites/{site}` → `204` (ownership policy applies; admins always allowed)
* `POST G/sites/{site}/owners` `{"add": [emails], "remove": [emails]}` → SiteMeta (ownership mode only)
* `POST G/sites/{site}/refresh` → rebuild manifest + meta from storage after a direct sync → SiteMeta

`SiteMeta = {name, url, title, description, created_at, created_by, updated_at, updated_by,
deploy_id, file_count, bytes, owners: []}`. `title` is extracted from `<title>` of the root
`index.html` at commit/refresh; `description` from `<meta name="description">`.

### 7.3 Deploy (diffed, rsync semantics)

1. `POST G/sites/{site}/deploys`
   `{"files": [{"path": "index.html", "size": 1234, "sha256": "<hex>", "content_type": "text/html"}], "message": "…"}`
   → `{"deploy_id": "<signed>", "uploads": [{"path", "url", "method": "PUT", "headers": {…}}], "unchanged": ["app.js"], "expires_at": "…"}`
   Paths: relative, `/`-separated, no `..`, no leading `/`, ≤ 1024 bytes, may not start with
   `__backyard/`. Limits from §7.1 enforced. A file is `unchanged` when the current manifest has
   the same sha256 **and** the manifest-recorded etag+size still match storage (detects
   out-of-band syncs). Ownership: in `sites.ownership: owner` mode the first deployer becomes
   owner; later deploys require being an owner or admin.
2. Client `PUT`s each upload URL with the given headers and the raw bytes. Upload URLs are either
   presigned storage URLs (s3/gcs/azure) or `G/uploads/{signed-token}` (fs storage, or
   `deploy.presign: false`). The self-hosted upload endpoint verifies size + sha256.
3. `POST G/sites/{site}/deploys/{deploy_id}/commit` `{"files": [... same list ...]}` → verifies
   every listed file exists with the right size, deletes objects under `sites/<site>/` not in the
   list, writes manifest + SiteMeta → `{"site": SiteMeta, "url": "https://site.base/"}`.

`deploy_id` is an HMAC-signed, expiring (1 h) token binding site + actor; commit rejects a
mismatched actor or site. Deploys are not atomic (same as rsync); the commit makes deletions last
so a site is never empty mid-deploy.

## 8. Authentication

`auth.mode`: `none` | `oidc` | `iap` | `alb` | `header`. In all modes, `Authorization: Bearer`
Backyard API tokens and static tokens are also accepted (API + deploy), and admins are
`auth.admins` ∪ members of `auth.admin_groups`. Optional gates: `auth.allowed_domains`,
`auth.allowed_groups` (403 otherwise).

### 8.1 `oidc` (Okta, Entra ID, Google, Auth0, Keycloak…)

* Authorization-code + PKCE, `nonce`, against `auth.oidc.issuer` (discovery).
* `GET /__backyard/auth/login?rd=<url>` (hub) → stores `{state, verifier, nonce, rd}` in a 10-min
  signed cookie `backyard_oauth` → IdP. `rd` must be an absolute URL on the hub or a site host
  (or a relative path) — anything else is replaced by the hub URL (no open redirects).
* `GET /__backyard/auth/callback` → exchange, verify ID token, map claims (+ optional userinfo),
  enforce allowed domains/groups → session cookie `backyard_session` (AES-GCM sealed JSON
  `{user, exp}`, key derived from `server.secret`; `Domain=.<base_domain>` in host mode, none in
  path mode; `HttpOnly; Secure; SameSite=Lax; Path=/`; TTL `auth.oidc.session_ttl`, default 12h).
* `GET /__backyard/auth/logout` → clears cookie → IdP `end_session_endpoint` if advertised, else hub.
* Unauthenticated browser `GET` of HTML on any host → `302` to login with `rd` = original URL.
  API requests → `401` JSON with `login_url`.
* Device flow for CLIs (optional `auth.oidc.device_client_id`): advertised in `info.auth.device`;
  the gateway accepts IdP JWTs whose `aud`/`azp`/`cid` matches `device_client_id` (or
  `auth.oidc.api_audiences`) and issuer matches.

### 8.2 `iap`, `alb`, `header`

* `iap`: verify `X-Goog-IAP-JWT-Assertion` (ES256, keys
  `https://www.gstatic.com/iap/verify/public_key-jwk`, `iss=https://cloud.google.com/iap`,
  `aud=auth.iap.audience`). Identity: `email`, `sub`; `gcip` claims if present.
* `alb`: verify `X-Amzn-Oidc-Data` (ES256; key PEM from
  `https://public-keys.auth.elb.<region>.amazonaws.com/<kid>`; header `signer` must equal
  `auth.alb.signer_arn` when set). Claims are the IdP userinfo claims.
* `header`: trust `auth.header.email_header` (+ name/groups headers) **only** from peers in
  `server.trusted_proxies`; optionally verify a JWT header against `jwks_url`/`issuer`/`audience`
  (Cloudflare Access, Pomerium, oauth2-proxy…). Azure Easy Auth works with
  `email_header: X-MS-CLIENT-PRINCIPAL-NAME`.
* Behind these proxies the CLI gets a token by opening `/__backyard/auth/cli` in the browser
  (the proxy authenticates the browser); deploy traffic goes to a gateway path that the proxy
  exempts (ALB rule / separate GCP backend service) or to the standalone gateway (Lambda).

### 8.3 Backyard API tokens

HS256 JWTs signed with a key derived from `server.secret` (`HKDF(secret, "backyard/token")`):
`{iss: "backyard", sub: user.id, email, name, groups, admin, scope: "api", jti, iat, exp}`.
Default TTL `auth.token_ttl` (720h). Minted by `/__backyard/auth/cli` (browser → CLI loopback) and
`POST /__backyard/api/tokens`. Revocation = rotate `server.secret`. Static tokens: `auth.tokens:
[{name, sha256}]` (hex sha256 of the token string; `backyard token hash` prints it); they
authenticate as `{id: "token:<name>", name, admin: <bool>}`.

CLI login (`/__backyard/auth/cli?redirect_uri=http://127.0.0.1:PORT/callback&state=…`): requires an
authenticated browser user; `redirect_uri` must be `http://127.0.0.1|localhost|[::1]:<port>/…`;
redirects to `redirect_uri?token=…&expires_at=…&state=…`. With `?mode=display` the token is shown
on a page for copy/paste (headless machines).

## 9. Storage layout (blob keys, after optional `storage.<type>.prefix`)

| Key | Content |
|---|---|
| `sites/<site>/<path>` | site files (what rsync / s3 sync / gcs rsync write) |
| `_meta/sites/<site>.json` | `SiteMeta` |
| `_meta/manifests/<site>.json` | `{"site","updated_at","files":[{"path","size","sha256","etag"}]}` |
| `uploads/<site>/<id>/<name>` | user uploads (§5.3) and generated images |

Sites known to the hub = `_meta/sites/*` ∪ first-level prefixes under `sites/`.
Site names: `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, not in `sites.reserved`
(default: `www, api, admin, hub, auth, static, assets, cdn, docs, mail, backyard, s`).

## 10. Database system data

System collections live in each site's namespace with names starting with `_` (`_files`), or
in the reserved site `_system` (`_users` user cache, `_audit` audit log). They are never listed by
`GET /db` and cannot be accessed through the site DB API.

## 11. Limits & rate limiting

Token buckets per instance (`ratelimit`), keyed by user id (or client IP when anonymous):
`api_per_minute` (all API, default 600), `db_writes_per_minute` (default 300), `ai_per_minute`
(default 20), `ai_tokens_per_day` (default 2,000,000 output+input tokens, 0 = unlimited),
`deploys_per_hour` (default 60), `uploads_per_minute` (default 60). Per-site storage limits:
`database.max_docs_per_site` (default 100,000), `database.max_doc_bytes`.

## 12. Configuration

YAML (`backyard.yaml`), path from `--config` or `BACKYARD_CONFIG`; or inline YAML in
`BACKYARD_CONFIG_YAML`. `${VAR}` and `${VAR:-default}` are expanded from the environment before
parsing. If `server.listen` is empty, `:$PORT` (Cloud Run / Container Apps) else `:8080`.
`backyard config example` prints a fully commented example; `backyard config validate` checks
one. See `internal/config/config.go` for the authoritative schema; `--dev` (or `server.dev:
true`) fills in: `base_domain: localhost`, `auth.mode: none`, fs storage + sqlite under
`./.backyard-data`, memory pubsub, `deploy.allow_anonymous: true`, random secret.

## 13. CLI

```
backyard serve   [--config f] [--dev]          run the server
backyard gateway [--config f]                  run only the deploy gateway (§7)
backyard init    [site] [--dir d] [--agents claude,codex,cursor,copilot] [--template blank|app]
backyard dev     [dir] [--port 4321] [--site s] local preview server with a local backend
backyard deploy  [dir] [--site s] [--method auto|api|s3|gcs|azure|rsync] [--dry-run] [-y]
backyard login   [--server url] [--gateway url] [--device] [--token t] [--profile p]
backyard logout | whoami
backyard sites list [--json] | sites delete <site> [-y] | sites info <site>
backyard open    [site]                        open site (or hub) in the browser
backyard pull    <site> [dir]                  download a site's files
backyard token   create [--name n] [--ttl 720h] | token hash <token>
backyard skill   install [--global|--project]  install the Claude skill
backyard mcp                                   MCP server over stdio (for agents)
backyard config  example | validate [file]
backyard cloudflare setup [--dir deploy/cloudflare]   provision the Workers edition via wrangler (§16)
backyard version
```

Local CLI config: `$XDG_CONFIG_HOME/backyard/config.json` (`~/.config/backyard/config.json`):
`{"current": "default", "profiles": {"default": {"server": "…", "gateway": "…", "token": "…",
"token_expires_at": "…", "method": "auto", "refresh_token": "…", "device": {…}}}}` (mode 0600).
Env overrides: `BACKYARD_SERVER`, `BACKYARD_GATEWAY`, `BACKYARD_TOKEN`, `BACKYARD_PROFILE`,
`BACKYARD_DEPLOY_METHOD`.

Project file `.backyard.json`: `{"site": "lunch-vote", "dir": ".", "method": "auto", "ignore":
[…]}`. Default deploy ignores: `.git/`, `.backyard.json`, `.backyardignore`, `node_modules/`,
`.claude/`, `.cursor/`, `.github/`, `CLAUDE.md`, `AGENTS.md`, `.DS_Store`, any path segment
starting with `.` except `.well-known/`; plus gitignore-style lines from `.backyardignore`.

Deploy methods:
* `api` — §7 protocol via the gateway.
* `s3` — `aws s3 sync <dir> s3://<bucket>/<prefix>sites/<site>/ --delete --exclude …`
* `gcs` — `gcloud storage rsync <dir> gs://<bucket>/<prefix>sites/<site> --recursive --delete-unmatched-destination-objects --exclude=<regex>` (falls back to `gsutil -m rsync -r -d -x`)
* `azure` — `az storage blob sync --account-name <a> -c <container> -s <dir> -d <prefix>sites/<site> --delete-destination true --exclude-pattern …`
* `rsync` — `rsync -az --delete --exclude … <dir>/ <rsync_target>/<site>/`
* `auto` — `api` if a gateway is reachable and the user has a token; otherwise the first
  advertised direct method whose tool is installed.
After a direct sync the CLI calls `POST G/sites/{site}/refresh` (best effort).

## 14. Agent integration

* `skills/backyard/SKILL.md` (+ `references/api.md`, `references/recipes.md`,
  `references/deploy.md`) — canonical agent docs; embedded in the binary.
* `backyard init` writes `index.html`, `.backyard.json`, `CLAUDE.md`, `AGENTS.md`,
  `.claude/skills/backyard/**` (and `.cursor/rules/backyard.mdc`,
  `.github/copilot-instructions.md` when selected).
* Claude Code plugin: `.claude-plugin/marketplace.json` + `.claude-plugin/plugin.json` at the repo
  root (skills, slash commands in `commands/`, MCP server `backyard mcp`).
* MCP tools (`backyard mcp`): `backyard_docs`, `backyard_init`, `backyard_deploy`,
  `backyard_list_sites`, `backyard_site_files`, `backyard_read_site_file`, `backyard_whoami`,
  `backyard_open_url`.
* Hub serves `/llms.txt` and `/llms-full.txt`.

## 15. Observability

`log/slog` JSON (or text) logs; audit events logged with `"audit": true` and stored in
`_system/_audit`. Prometheus metrics (prefix `backyard_`): `http_requests_total{area,code}`,
`http_request_duration_seconds{area}`, `deploys_total{method,result}`, `db_ops_total{op,result}`,
`ai_requests_total{model,result}`, `ai_tokens_total{model,direction}`, `ws_connections`,
`ws_messages_total{dir}`, `warehouse_queries_total{source,result}`.

## 16. Cloudflare edition (free tier)

A TypeScript Cloudflare Worker in `deploy/cloudflare/` implements the **same public contract**
(§2–§7 URLs, site API, websocket protocol, deploy gateway protocol, SDK preamble) so that the same
`backyard.js`, CLI, skill and MCP server work unchanged. Inspired by OpenQuick
(github.com/topmass/openquick). Runs on the Workers **free plan** (Durable Objects with SQLite
storage are available on free); R2 and paid features are optional.

* **Topology:** one Worker (router, auth, SDK/hub/docs serving, deploy gateway) + one
  `SiteDO` Durable Object per site (SQLite: site files, manifest, documents, uploads, websocket
  hub with hibernation) + one `RegistryDO` (site list/metadata, platform-wide counters/quotas).
* **URLs:** default path mode on `https://<worker>.<account>.workers.dev/s/<site>/`; custom domain
  in path mode; or host mode with a wildcard route `*.<base_domain>/*` (Universal SSL covers one
  wildcard level; deeper needs Advanced Certificate Manager). Configured via `vars`.
* **Storage:** files ≤ `FILE_INLINE_MAX` (default 5 MiB) stored chunked in DO SQLite (chunks <
  2 MB); larger files go to the optional `R2` binding (`sites/<site>/…`, `uploads/<site>/…`);
  without R2 the per-file cap is 25 MiB. Deploy uploads use self-hosted upload URLs (§7.3).
* **Database:** documents in the site DO's SQLite with the exact query semantics of §5.2
  (shared conformance tests). Realtime is local to the site DO (single instance per site).
* **AI:** `workers-ai` provider via the `AI` binding (free daily allowance) with configurable
  aliases (`AI_MODELS` JSON var); optional `anthropic`/`openai`(-compatible) providers via
  secrets (optionally through Cloudflare AI Gateway). Image generation via Workers AI image
  models. Embeddings via Workers AI.
* **Warehouse:** not available on Cloudflare (`501 unsupported`) unless `WAREHOUSE_URL` points
  to a Backyard Go server's warehouse API (proxy) — optional.
* **Auth:** `AUTH_MODE` = `none` (anonymous cookie identity; deploys need `DEPLOY_TOKEN` unless
  `ALLOW_ANONYMOUS_DEPLOY`) or `access` (Cloudflare Access: verify `Cf-Access-Jwt-Assertion`
  against `https://<team>.cloudflareaccess.com/cdn-cgi/access/certs` with `ACCESS_AUD`; Okta,
  Entra ID, Google etc. are configured as Access identity providers). Backyard API tokens (§8.3,
  HS256 with `BACKYARD_SECRET`) and the `/__backyard/auth/cli` browser flow work identically.
  Access must bypass `/__backyard/api/deploy/*` (service tokens or a Bypass policy) — Terraform
  in `deploy/terraform/cloudflare` sets this up.
* **Guardrails (free tier):** per-user/IP daily caps for AI calls, deploys and uploads, and a
  platform-wide site cap, stored in `RegistryDO`; configurable via `LIMITS` JSON var.
* **Deploying the platform:** `cd deploy/cloudflare && npm ci && npm run setup` (wrangler login,
  secrets, optional R2 bucket, deploy, prints the `backyard login` command), or
  `backyard cloudflare setup` which runs the same script; GitHub Actions example included.

## 17. Cross-edition API conformance

`conformance/` is a black-box HTTP + websocket test-suite (Node 22, `node --test`, no
dependencies beyond `ws` if needed) run against a base URL (`BACKYARD_URL`, `BACKYARD_TOKEN`). It
deploys a test site via the gateway protocol and exercises §5–§7. CI runs it against the Go server
(`backyard serve --dev`) and the Worker (`wrangler dev`).
