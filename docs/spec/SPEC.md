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
| `internal/keys` | per-purpose keys derived from `server.secret` (§8.3) |
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
  The hub answers on `base_domain` and on the host of `public_url`.
* **Host mode** (default): hub at `public_url`, site `foo` at
  `<scheme>://foo.<base_domain>[:<port>]/` with the scheme and port of `public_url`. Sites are
  always named under `base_domain`, also when the host of `public_url` differs from it. Only
  single-label subdomains are sites. `foo.localhost` works out of the box for local dev
  (browsers resolve `*.localhost` to loopback) when `base_domain: localhost`.
* **Path mode** (`server.url_mode: path`, for environments without wildcard DNS/TLS): site `foo`
  at `<public_url>/s/foo/`. Sites then share an origin (documented trade-off).
* Request host is taken from `Host` (or `X-Forwarded-Host` only when the peer is in
  `server.trusted_proxies`). Port is ignored for matching. Any other host → `404` ("unknown
  host").
* **Dev mode** (`server.dev`): additionally, a request without a `Host`, or whose host is an IP
  literal, `localhost` or a `*.localhost` name that is not a site host, reaches the hub. Every
  other unknown name stays a `404`, so a DNS-rebinding page (`evil.example → 127.0.0.1`) cannot
  use a local dev server as its own origin.
* Reserved path prefix on **every** host: `/__backyard/`. Site files can never shadow it.
* Non-canonical paths (`//`, `/./`, `/../`) are redirected to the cleaned path (`301`, GET/HEAD)
  or rejected (`400`) before routing. The Cloudflare Workers runtime resolves dot segments
  (`/./`, `/../`, also percent-encoded as `%2e`) before the Worker sees a request, so on that
  edition such a request is handled as its cleaned path instead (no redirect; a write is not
  rejected); `//` is redirected or rejected as above. Either way no handler ever sees a
  non-canonical path.

Every site URL that either edition, the CLI, the hub or the MCP server produces (`SiteMeta.url`,
the deploy commit's `url`, the URLs they print) follows these rules:

| `public_url` | `base_domain` | `url_mode` | Site `foo` |
|---|---|---|---|
| `https://backyard.acme.com` | `backyard.acme.com` | `host` | `https://foo.backyard.acme.com/` |
| `https://hub.acme.com` | `apps.acme.com` | `host` | `https://foo.apps.acme.com/` |
| `http://localhost:8080` | `localhost` | `host` | `http://foo.localhost:8080/` |
| `https://backyard.acme.com` | `backyard.acme.com` | `path` | `https://backyard.acme.com/s/foo/` |

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

On site hosts `/.well-known/*` stays site content (the OAuth discovery documents of §8.4 exist
only on the hub), `/__backyard/mcp` and `/__backyard/oauth/*` answer the JSON `404`, and
`/__backyard/auth/authorize` is redirected to the hub like the other auth paths.

Hub host:

| Path | Purpose |
|---|---|
| `/` | hub UI: site directory/search, drag-and-drop deploy, CLI setup instructions |
| `/docs` | API docs (renders `skills/backyard/references/api.md`) |
| `/docs/skill.md`, `/docs/<ref>.md`, `/docs/references/<ref>.md` | the skill documents as `text/markdown` |
| `/llms.txt`, `/llms-full.txt` | agent-oriented docs (SKILL.md; SKILL.md + all references) |
| `/__backyard/backyard.{js,mjs,d.ts}` | SDK (hub UI uses it too) |
| `/__backyard/api/info` | server info (§7.1, same as deploy `info`) |
| `/__backyard/api/me` | current user |
| `/__backyard/api/sites` | site list (`{sites:[SiteMeta]}`) |
| `/__backyard/api/tokens` | `POST` mint a personal API token (§8.3) |
| `/__backyard/api/deploy/...` | embedded deploy gateway (§7) |
| `/__backyard/api/mcp/grants` | the signed-in user's Claude connector connections; admins: anyone's (§8.4) |
| `/__backyard/auth/{login,callback,logout,cli,authorize}` | auth flows (§8; `authorize` is the connector's sign-in and consent page, §8.4) |
| `/__backyard/mcp` | remote MCP endpoint for Claude (§14.1; when enabled) |
| `/__backyard/oauth/{token,register,revoke}` | the connector's OAuth endpoints (§8.4; when enabled) |
| `/.well-known/oauth-protected-resource/__backyard/mcp`, `/.well-known/oauth-authorization-server` | OAuth discovery (§8.4; hub only, when enabled) |
| `/__backyard/healthz` | liveness `200 ok` (no auth) |
| `/__backyard/readyz` | readiness: pings db + blob; `503 not ready: <checks>` on failure (no auth) |
| `/__backyard/metrics` | Prometheus, **authenticated** (only if `telemetry.metrics` and no `telemetry.metrics_listen`) |

`/__backyard/healthz` and `/readyz` are served on any host without authentication. Hub pages and
docs require authentication like sites; the SDK files, `/__backyard/auth/*`,
`/__backyard/api/info`, the deploy gateway, and the remote MCP endpoint and the OAuth endpoints
of §8.4 (which authenticate by themselves) do not.

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
nosniff`, `Referrer-Policy: strict-origin-when-cross-origin`, `X-Request-Id`. **No**
`X-Frame-Options` (embedding sites in sites is a feature). `HEAD` supported. Range requests
supported. Methods other than GET/HEAD → `405`.

On Cloud Run (detected by `K_SERVICE`), whose HTTP/1 front end rejects responses that declare a
`Content-Length` above 32 MiB, the Go server adapts larger HTTP/1 responses of site files and
uploads: a full response is streamed without a `Content-Length` (chunked), and a `206` range
response is shortened to the first 32 MiB of the requested range — its `Content-Range` names
the bytes actually sent, and clients request the rest (RFC 9110 §15.3.7). HTTP/2 requests are
not affected.

Uploaded files (`/__backyard/files/{id}/{name}`, §5.3) are authenticated like site files and
served with `Content-Disposition: inline; filename=…` and `Cache-Control: private, no-cache`.
The stored type comes from the uploading client, so it cannot be trusted: only **passive**
types are served without a sandbox — `image/png`, `image/apng`, `image/jpeg`, `image/gif`,
`image/webp`, `image/avif`, `image/bmp`, `image/x-icon`, `image/vnd.microsoft.icon`,
`text/plain`, `text/csv`, `application/pdf`, `application/json`, `application/octet-stream`,
and `audio/*` or `video/*` whose subtype has no `+` suffix. Every other type (HTML, SVG, any
XML including `text/xsl` and every `+xml` type, scripts, unknown or unparsable types)
additionally gets `Content-Security-Policy: sandbox`, so an uploaded document can never run
script with the site's origin.

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
  true}` keyed by the signed `backyard_anon` cookie (1 year; `Domain` as in §8.3). Static-token
  users are `{id: "token:<name>"}`; neither kind is recorded in the user cache.

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
* Every response (API or not) carries `X-Request-Id`: the request's own `X-Request-Id` when it
  is 8–128 characters of `A-Z a-z 0-9 . _ -` (so an id set by a proxy in front is kept), else a
  generated one. Logs use the same id, and server-side failures whose details are withheld
  from the client name it (§5.5).
* Unknown endpoints → `404 not_found`; a known path with the wrong method → `405` with code
  `bad_request` and an `Allow` header. A feature that is not configured (AI, warehouse,
  realtime, tokens) → `501 unsupported`.
* Timestamps are RFC 3339 strings in UTC, formatted like Go's `time.RFC3339Nano` (fractional
  seconds without trailing zeros, e.g. `2026-01-02T03:04:05.12Z`). Document timestamps
  (`_meta.created_at`/`updated_at`, `FileInfo.created_at`) have **microsecond** precision on
  every backend; because the fraction has a variable width they do not sort as strings — the
  server compares them chronologically (§5.2). Presence `joined_at` uses a fixed width (§6).

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
stored as data (`id` is used as the document id on create / must match on put). `_meta.version`
starts at 1 and every write (PUT, PATCH) increments it. Max document size
`database.max_doc_bytes` (default 256 KiB) of JSON data, measured as stored (without HTML
escaping: `<`, `>` and `&` count one byte each); a larger document, or a create that would
exceed `database.max_docs_per_site`, fails with `413 too_large`.

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
  `^[A-Za-z_][A-Za-z0-9_-]{0,63}$`, max depth 8. System fields: `id` (string),
  `_meta.created_at`, `_meta.updated_at` (timestamps), `_meta.created_by`, `_meta.updated_by`
  (strings) and `_meta.version` (number).
* ops: `==`, `!=`, `<`, `<=`, `>`, `>=`, `in`, `not-in` (value = array of ≤ 30 scalars),
  `contains` (field is an array containing the scalar value).
* Values are JSON scalars (string, number, boolean, null). **Comparisons only match values of the
  same JSON type**, for every operator: `"3"` never equals `3`, `true` never equals `1`, `<` on a
  string field with a number value matches nothing.
  * `!=` matches when the field exists, has the value's type and differs from it — so
    `!= null` matches **nothing**, and `!= 2` does not match `"2"` or a missing field.
  * `not-in` matches when the field is `!=` every listed value (and hence has the same type as
    each); `in` when it equals any of them.
  * A missing field never matches any operator. `== null` matches an explicit `null` only;
    `<= null` / `>= null` match an explicit `null`; `< null` / `> null` match nothing.
  * `contains` matches an array field with an element of the value's type equal to it.
  * The timestamp fields compare chronologically against RFC 3339 string values (truncated to
    microseconds); any other value for them matches nothing.
* Ordering: values are ranked across types as missing/null < boolean < number < string < array
  < object, then within a type numbers numerically, strings by byte order, `false < true`
  (arrays and objects tie). Descending order reverses the whole comparison, so missing/`null`
  sort **first** ascending and **last** descending. Ties are broken by `id` ascending. Default
  order: `_meta.created_at asc, id asc`. `GET /db/{c}` also accepts
  `orderBy=field[,field2]&order=asc|desc`.
* `limit` default 100, max `database.max_list` (default 1000). `has_more` true if more matched.
  A page may hold **fewer** than `limit` documents and still have `has_more` (the Cloudflare
  edition ends a page at its byte budget, §16; every page holds at least one document), so
  clients page by advancing `offset` by the number of documents received, not by `limit`.
* Every successful write publishes a realtime event (§6).

### 5.3 Files

| Method & path | Body | Result |
|---|---|---|
| `POST /files` | `multipart/form-data`, field `file` (+ optional `name`) | `201` FileInfo |
| `GET /files?limit=&offset=` | | `{"files": [FileInfo], "has_more": bool}` newest first |
| `GET /files/{id}` | | FileInfo \| 404 |
| `DELETE /files/{id}` | | `204` |

`FileInfo = {id, name, url, size, content_type, created_at, created_by}`; `url` is the
site-relative path `/__backyard/files/{id}/{urlescaped name}` (path mode: prefixed with
`/s/{site}`). `name` is sanitized (base name only, no control or reserved characters). `limit`
defaults to 100, max `database.max_list`. Max size `files.max_upload_bytes` (default 100 MiB;
larger → `413 too_large`). Bytes stored at blob key `uploads/<site>/<id>/<name>`; metadata in the
system collection `_files`. Uploads count against `ratelimit.uploads_per_minute`; deletes
against `db_writes_per_minute`. Serving rules: §3.

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
  or `{"type":"error","error":{"code":…,"message":…}}`. While the model is silent the server
  sends SSE comment lines `: ping` (every 15 s) to keep proxies from timing out; clients must
  ignore lines starting with `:`.
* `stop_reason` normalized to: `end_turn`, `max_tokens`, `stop_sequence`, `refusal`, `other`.
* Image parts: `data` (base64, or a `data:` URL) with `media_type`, or `url` — a `data:` URL, the
  URL of one of the site's own uploads, or an **absolute public** http(s) URL.
  * A URL of one of the site's own uploads is read server-side, from storage, with no fetch:
    the site-relative `/__backyard/files/{id}/{name}` that `files.upload` returns (path mode:
    with `/s/{site}` in front), or the same path as an absolute URL on the site's own origin.
    Any other site-relative URL, an unknown file or another site's file (`/s/{other}/…`) →
    `400 bad_request`. The media type of an upload is detected from its bytes (png, jpeg, gif,
    webp; anything else → `400`), not taken from its stored `content_type`.
  * Other URLs are passed to providers that fetch them (OpenAI, Azure OpenAI, the first-party
    Claude API) or downloaded by an SSRF-guarded fetcher (public addresses only).
  * Inline images ≤ 20 MiB each; images loaded from URLs (uploads and downloads together) ≤ 64
    MiB per request, and ≤ 20 MiB each.
  * The OpenAI-style part `{"type":"image_url","image_url":{"url":"…"}}` (or `"image_url":"…"`)
    is accepted as an alias of `{"type":"image","url":"…"}` by both editions.
* `max_tokens` is capped by `ai.max_tokens` and a smaller per-alias `max_tokens`; without one
  the request gets min(4096, cap). `model` in responses is always the alias. An unknown alias →
  `404 not_found`; an alias of the wrong kind → `400 bad_request`; AI not configured → `501
  unsupported`.
* Provider failures are mapped by the provider's HTTP status (in a stream: an `error` event with
  the same code):

  | Provider answer | Response |
  |---|---|
  | `504`, `408`, or no answer within `ai.request_timeout` | `504 upstream_error` |
  | `429` (the provider throttles the server's account, which all users share) | `429 rate_limited` with `Retry-After` when the provider said when to retry (≤ 1 h), else `503 upstream_error` |
  | `400`, `422` (the provider rejected the request) | `400 bad_request` |
  | `413` | `413 too_large` |
  | anything else (credentials, outages, malformed responses) | `502 upstream_error` |

  Provider messages are sanitized before they reach the client.

`POST /ai/image` `{prompt, model?, size?: "1024x1024", n?: 1}` →
`{"model": "image", "images": [{"id": "01J…", "url": "/__backyard/files/01J…/image-1.png", "content_type": "image/png"}]}`.
Images are stored as site files (`id` is the file id; they appear in `GET /files`). `n` is at
most 10 on the Go server and 4 on the Cloudflare edition (more → `400 bad_request`).

`POST /ai/embed` `{input: string | string[], model?}` →
`{"model": "embed", "embeddings": [[…], …], "usage": {"input_tokens": 8, "output_tokens": 0}}` —
one vector per input string (a single string yields one vector). At most 2048 strings (8 MiB in
total) on the Go server and 100 on the Cloudflare edition (more → `400 bad_request`).

`GET /ai/models` → `{"default": "smart", "models": [ModelInfo]}` with
`ModelInfo = {name, kind: "chat"|"image"|"embedding", provider, model?, description?}`; `model`
(the provider model id) is present only if `ai.expose_model_ids`.

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
* `GET /warehouse` → `{"sources": [{name, type, description}], "queries": [{name, description, params: [..], source}]}`.
* Errors: no warehouse configured → `501 unsupported`; raw SQL disabled or statement rejected →
  `403 forbidden`; unknown source or named query → `404 not_found`; missing/unknown named
  parameters or a placeholder/param count mismatch → `400 bad_request`; timeouts → `504
  upstream_error`; engine and connection failures → `502 upstream_error` (other server-side
  failures `500 internal`). Messages of `4xx` errors describe the caller's query (syntax errors,
  unknown columns, missing parameters) and are passed on; `502`/`500` messages are generic and
  name the request id (`X-Request-Id`, §5) under which the server logged the cause, because
  engine and connection errors can reveal host, user and database names. Results are capped at
  `warehouse.max_rows` rows **and** at 32 MiB of row data (their JSON encoding plus a small
  per-value overhead); the rows stop at the last one that fits both caps and the result has
  `truncated: true`. Sources enforce the size cap while they read (Postgres cancels the
  statement and never buffers a row larger than what is left of the cap; BigQuery and Athena
  fetch no further pages). A single Postgres row too large for the cap ends the result; when it
  is the first row, `columns` may be empty. Queries are bounded by `warehouse.timeout`.

### 5.6 JavaScript SDK (`window.backyard`)

Public surface (TypeScript notation; `web/sdk/backyard.d.ts` is normative and also declares the
optional extras: `signal` options, `ChatStream.abort()`/`.text`, `room.on("error")`,
`socket.cid`/`.user`/`.connect()`/`.disconnect()`/`.on("error")`, `onError` callbacks).
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
posts.where(field, op, value) → Query            // chainable; ops as in §5.2 (SDK aliases: = === !== <> array-contains "not in")
     .where(...).orderBy(field, "desc").limit(20).offset(0)
await query.get() → Doc[]                        // also: await posts.find(queryObject) → Doc[]
                                                 // arrays from list/find/query.get carry a non-enumerable hasMore
await query.first() → Doc|null
const unsubscribe = posts.subscribe({ onCreate?(doc), onUpdate?(doc), onDelete?(id), onChange?(event) })
const stop = posts.watch(callback: (docs: Doc[]) => void, query?: Query) // live query: calls back with
                                                 // the full result now and after every change (debounced 50 ms)
await backyard.db.collections() → string[]

// Files
await backyard.files.upload(file: File|Blob, { name? }) → FileInfo   // FileInfo = {id,name,url,size,content_type,created_at,created_by}
await backyard.files.list({ limit?, offset? }) → FileInfo[]                // + non-enumerable hasMore
await backyard.files.get(id) → FileInfo|null
await backyard.files.delete(id) → void

// AI
await backyard.ai.chat(input: string | Message[], { model?, system?, maxTokens?, temperature?, json? })
      → { content: string, model, stop_reason, usage, json?: any }   // json parsed when opts.json
for await (const chunk of backyard.ai.stream(input, opts)) { chunk.text }   // async iterable; also
      backyard.ai.stream(input, { ...opts, onText(text) }) and `await stream.done` → final result
await backyard.ai.image(prompt, { model?, size?, n? }) → { id, url, content_type, model?, images: [{id,url,content_type}] }
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

Client → server (`id` is an optional client correlation id echoed in `ack`/`joined`/`error`;
as a compatible extension, `leave`, `state` and `send` frames that carry an `id` are
acknowledged with `{"t":"ack","id"}` on success):

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

`doc` is the flattened document after the change and `null` for deletes. A `create` or `update`
frame may also carry `"doc": null` (with `doc_id` and `op` as usual) when the event is too large
to broadcast: the Go server sends the change without its document when the encoded event would
exceed its 1 MiB pub/sub payload limit, which is possible when `database.max_doc_bytes` is
raised. Clients read such a document with `GET /db/{c}/{doc_id}`; the SDK's `subscribe` does so
before it delivers the event (in arrival order), so `onCreate`/`onUpdate` always receive a
document. Events are encoded without HTML escaping.

`Member = {"cid","user":UserSummary,"state":{…},"joined_at":RFC3339}`;
`UserSummary = {id, name, email, avatar_url}`. `joined_at` has a fixed width of exactly three
fractional digits (`2026-01-02T03:04:05.120Z`) so members order correctly as strings; `members`
lists are sorted by `joined_at`, then `cid`. Room names `^[A-Za-z0-9_\-:.]{1,128}$`.

Errors never close the connection: invalid frames are answered with an `error` frame (§5 codes:
`bad_request`, `forbidden`, `not_found` — e.g. `state`/`send` for a room not joined or `to` an
unknown member —, `too_large`, `rate_limited`, `internal`). Frames far above the size limit close
the connection (1009); clients that cannot keep up with their outbound queue are closed (1008).
Limits (`realtime.*`): 50 rooms & 100 subscriptions per connection; 60 msgs/s sustained (burst
120) per connection; frames ≤ 64 KiB; presence state ≤ 8 KiB; 50 connections per user (more are
refused with HTTP `429` before the upgrade). Server pings every 25 s; idle connections without
pong for 60 s are closed. `realtime.disabled` → `GET /ws` answers `501 unsupported`.

Origin check: a browser `Origin` must be the site's own origin (host mode) or the hub origin
(path mode); with `realtime.allow_cross_site_ws` the hub and any site of the same server are
allowed too.

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
            "device": { "issuer": "https://acme.okta.com/oauth2/default", "client_id": "0oa…", "scopes": ["openid","profile","email","offline_access"] },
            "anonymous_deploy": false },
  "storage": { "type": "s3", "bucket": "acme-backyard", "prefix": "", "region": "us-east-1" },
  "methods": ["api", "s3"],
  "limits": { "max_site_bytes": 524288000, "max_files": 10000, "max_file_bytes": 104857600 } }
```

`storage` reveals only what direct-sync clients need (`type`, `bucket`/`container`, `account`,
`prefix`, `region`), and only if `deploy.expose_storage` (default true). `auth.cli_login_url` is
present in the `oidc`, `iap`, `alb` and `header` modes; `auth.device` whenever
`auth.oidc.issuer` and `auth.oidc.device_client_id` are set (in any mode);
`auth.anonymous_deploy` (bool) is true when deploys need no token (`auth.mode: none` +
`deploy.allow_anonymous`). `gateway_url` is `deploy.gateway_url` when set. `rsync_target` is
omitted when empty. The Cloudflare edition adds `"edition": "cloudflare"`. While the Claude
connector is enabled (§8.4), the hub's embedded gateway adds `"mcp_url": "<R>"`, the remote MCP
endpoint `<public_url>/__backyard/mcp` (§14.1); it is omitted when the connector is disabled and
on standalone gateways.

### 7.2 Sites

* `GET G/me` → `User` (auth required — anonymous when anonymous deploys are allowed; lets CLIs
  verify a token)
* `GET G/sites` → `{"sites": [SiteMeta]}`
* `GET G/sites/{site}` → `SiteMeta` | 404
* `GET G/sites/{site}/files` → `{"files": [{"path","size","sha256"?,"updated_at"}]}`
* `GET G/sites/{site}/files/{path…}` → raw bytes (for `backyard pull` / agents reading other sites)
* `DELETE G/sites/{site}` → `204` (ownership policy applies; admins always allowed)
* `POST G/sites/{site}/owners` `{"add": [emails], "remove": [emails]}` → SiteMeta (ownership mode only)
* `POST G/sites/{site}/refresh` → rebuild manifest + meta from storage after a direct sync → SiteMeta

`SiteMeta = {name, url, title, description, created_at, created_by, updated_at, updated_by,
deploy_id, file_count, bytes, owners: []}`. `title` is extracted from `<title>` of the root
`index.html` at commit/refresh; `description` from `<meta name="description">`. `deploy_id` here
is a short opaque id of the last deploy (`d_<hex>`), not the signed deploy token.

### 7.3 Deploy (diffed, rsync semantics)

1. `POST G/sites/{site}/deploys`
   `{"files": [{"path": "index.html", "size": 1234, "sha256": "<hex>", "content_type": "text/html"}], "message": "…"}`
   → `{"deploy_id": "<signed>", "uploads": [{"path", "url", "method": "PUT", "headers": {…}}], "unchanged": ["app.js"], "expires_at": "…"}`
   Paths: relative, `/`-separated, no `..`, no leading `/`, ≤ 1024 bytes, may not start with
   `__backyard/`, and no segment may be `.git` in any letter case (deploys never publish
   repository metadata, §13; both gateways apply the same rule to deploy plans, commits,
   uploads and `GET G/sites/{site}/files/{path…}`, answering `400 bad_request`). Limits from
   §7.1 enforced. A file is `unchanged` when the current manifest has the same sha256 **and**
   the manifest-recorded etag+size still match storage (detects out-of-band syncs). Ownership:
   in `sites.ownership: owner` mode the first deployer becomes owner; later deploys require
   being an owner or admin.
2. Client `PUT`s each upload URL with **all** the given headers and the raw bytes. Upload URLs
   are either presigned storage URLs (s3/gcs/azure) or `G/uploads/{signed-token}` (fs storage,
   `deploy.presign: false`, or credentials that cannot presign). Every upload is bound to the
   declared `size` and `sha256` as far as the store can enforce it:
   * the self-hosted upload endpoint streams the body through a verifier; a body of another size
     or digest fails the request and is never stored;
   * an S3 URL signs the size (a signed `Content-Length`, or for an empty file the `Content-MD5`
     of the empty body) and, when flexible checksums are enabled (AWS S3 by default; a custom
     endpoint only when `AWS_REQUEST_CHECKSUM_CALCULATION` asks for them, since many
     S3-compatible stores reject the header), the SHA-256 as `x-amz-checksum-sha256`, so S3
     rejects any other body;
   * a GCS URL pins the size with a signed `x-goog-content-length-range: <size>,<size>` (GCS
     cannot verify a SHA-256);
   * an Azure SAS URL cannot restrict the body at all; the commit's size check (step 3) covers
     it.
3. `POST G/sites/{site}/deploys/{deploy_id}/commit` `{"files": [... same list ...]}` → verifies
   every listed file exists with the right size, deletes objects under `sites/<site>/` not in the
   list, writes manifest + SiteMeta → `{"site": SiteMeta, "url": "https://site.base/", "deleted": 3}`.
   A listed file that is missing or stored with another size fails the commit with `409
   conflict` naming the paths (the first 10). A stored object of the wrong size is left in place
   where every upload URL binds the size (the gateway's own upload endpoint, S3, GCS): it is the
   verified upload of another, overlapping deploy, or a direct sync. Only with presigned Azure
   SAS uploads, which bind nothing, is a wrong-size object that is not the version the previous
   manifest recorded deleted first, so no unverified object outlives a commit attempt (a
   previously committed version that this deploy did not replace is kept). For every listed
   file whose stored object the previous manifest does not describe, the commit records the
   SHA-256 of the bytes stored **now**, with their etag: a checksum the store verified itself
   where it reports one (S3 `x-amz-checksum-sha256`), otherwise the hash of the object as read.
   The declared digest only says what this deploy uploaded; another deploy may have replaced
   the object since. A file that vanishes or changes size during that check fails the commit
   with `409` too.

`deploy_id` is an HMAC-signed, expiring (1 h) token binding site + actor; commit rejects a
mismatched actor or site; upload URLs expire with it. Deploys are not atomic (same as rsync);
the commit makes deletions last so a site is never empty mid-deploy. Deploys of one site may
overlap; the last write wins per file, and the manifest records what storage holds. Uploads
write directly to `sites/<site>/<path>`, so the uploads of a deploy that is never committed
stay in storage: a path of the current manifest is replaced in place (as rsync would), and a
new path becomes an object that is served like any other file of the site but is not in the
manifest (never `unchanged`) until a later commit deletes it as unlisted, a refresh adopts it,
or the site is deleted. The Cloudflare edition differs here (§16). State-changing gateway
requests need `X-Backyard: 1` unless they carry a bearer token without browser cross-site
signals (`Origin`, `Sec-Fetch-Site`); upload URLs are exempt (the signed token is the
capability). Creates and refreshes (not commits) count against `ratelimit.deploys_per_hour` per
actor (anonymous: per client IP).

## 8. Authentication

`auth.mode`: `none` | `oidc` | `iap` | `alb` | `header` (`okta`, `entra`, `azuread`, `azure-ad`,
`google`, `auth0`, `keycloak`, `openid` are aliases of `oidc`; `off`/`disabled`/empty mean
`none`). In all modes, `Authorization: Bearer` Backyard API tokens and static tokens are also
accepted (API + deploy), and admins are `auth.admins` ∪ members of `auth.admin_groups`. Optional
gates (admins pass them): `auth.allowed_domains` / `auth.allowed_emails` — when either is set,
the user's verified email must have a listed domain or be a listed address (case-insensitive) —
and `auth.allowed_groups` (403 otherwise). A bearer token is tried first; if it is invalid the
mode's own credentials are tried, and the bearer error is reported only when those are absent
(in mode `none` an invalid bearer token is always `401`). MCP access tokens (§8.4) are accepted
only at `/__backyard/mcp`, which accepts nothing else.

### 8.1 `oidc` (Okta, Entra ID, Google, Auth0, Keycloak…)

* Authorization-code + PKCE, `nonce`, against `auth.oidc.issuer` (discovery).
* `GET /__backyard/auth/login?rd=<url>` (hub) → stores `{state, verifier, nonce, rd}` in a 10-min
  signed login state cookie → IdP. `rd` must be an absolute URL on the hub or a site host
  (or a relative path) — anything else is replaced by the hub URL (no open redirects). When
  `server.public_url` is https the state cookie is `__Host-backyard_oauth` (`Secure; Path=/`,
  no `Domain`), which only the hub itself can set, so a site's page cannot plant its author's
  login state (login CSRF); a `backyard_oauth` cookie is then ignored. Over http (browsers
  refuse `__Host-` without `Secure`) it is `backyard_oauth` at `Path=/__backyard/auth/` (`/`
  with a callback path outside it), and in host mode the server logs a startup warning that
  the login state is not protected.
* `GET /__backyard/auth/callback` → exchange, verify ID token, map claims (+ optional userinfo),
  enforce allowed domains/groups → session cookie `backyard_session` (AES-GCM sealed JSON
  `{user, exp}`, key derived from `server.secret`; `Domain=.<base_domain>` in host mode, none in
  path mode; `HttpOnly; Secure; SameSite=Lax; Path=/`; TTL `auth.oidc.session_ttl`, default 12h).
* In host mode site hosts only see the session through that `Domain` cookie, so `oidc` with
  `url_mode: host` requires a `base_domain` that browsers accept as a cookie domain — not
  `localhost`, `*.localhost`, an IP address or a single-label name (§8.3) — and a `public_url`
  whose host is `base_domain` or one of its subdomains. Configuration validation rejects other
  combinations (otherwise every site page would bounce through the IdP endlessly); use path
  mode, or for local tests a real domain whose subdomains resolve to loopback (`lvh.me`).
* `GET /__backyard/auth/logout` → clears cookie → IdP `end_session_endpoint` if advertised, else hub.
* Unauthenticated browser `GET` of HTML on any host → `302` to login with `rd` = original URL.
  API requests → `401` JSON with `login_url`.
* Device flow for CLIs (optional `auth.oidc.device_client_id`): advertised in `info.auth.device`;
  the API and gateway accept IdP JWTs whose `aud`/`azp`/`cid` matches `client_id`,
  `device_client_id` or `auth.oidc.api_audiences` and whose issuer matches. This works in every
  mode where `auth.oidc.issuer` is set (e.g. device flow behind IAP or an ALB); the CLI refreshes
  such tokens with the `offline_access` refresh token.

### 8.2 `iap`, `alb`, `header`

* `iap`: verify `X-Goog-IAP-JWT-Assertion` (ES256, keys
  `https://www.gstatic.com/iap/verify/public_key-jwk`, `iss=https://cloud.google.com/iap`,
  `aud=auth.iap.audience`). Identity: `email`, `sub`; `gcip` claims if present.
* `alb`: verify `X-Amzn-Oidc-Data` (ES256; key PEM from
  `https://public-keys.auth.elb.<region>.amazonaws.com/<kid>`; header `signer` must equal
  `auth.alb.signer_arn` when set). Claims are the IdP userinfo claims.
* `header`: trust `auth.header.email_header` (+ name/groups headers) **only** from peers in
  `server.trusted_proxies`; optionally verify a JWT header (`jwt_header`) against
  `jwks_url`/`issuer`/`audiences` (Cloudflare Access, Pomerium, oauth2-proxy…) — the JWT is then
  the trust anchor, and if `trusted_proxies` is also set the peer must be trusted too. An
  identity header present more than once is rejected; an `accounts.google.com:` prefix is
  stripped. Azure Easy Auth works with `email_header: X-MS-CLIENT-PRINCIPAL-NAME` (name and
  roles are read from `X-MS-CLIENT-PRINCIPAL`).
* Behind these proxies the CLI gets a token by opening `/__backyard/auth/cli` in the browser
  (the proxy authenticates the browser); deploy traffic goes to a gateway path that the proxy
  exempts (ALB rule / separate GCP backend service) or to the standalone gateway (Lambda).
* In these modes and in `none` (and always on the Cloudflare edition) Backyard does not run a
  login itself: the proxy in front authenticates page loads. `GET
  /__backyard/auth/login?rd=<url>` therefore answers `302` straight to `rd` (`Cache-Control:
  no-store`), sanitized by the rules of §8.1, so that a `login_url` always leads somewhere
  useful. (On the Go server `/callback` and `/logout` exist only in `oidc` mode.)

### 8.3 Backyard API tokens

**Key derivation.** Every secret-keyed component uses its own key
`keys.Derive(server.secret, label)` = HKDF-SHA256(ikm = secret, salt = `"backyard"`,
info = label, 32 bytes) with the labels `backyard/session/v1`, `backyard/oauth-state/v1`,
`backyard/api-token/v1`, `backyard/deploy/v1`, `backyard/upload/v1`, `backyard/anon/v1`,
`backyard/cli-confirm/v1`, and for the Claude connector (§8.4) `backyard/mcp-client/v1`
(registered `client_id`s), `backyard/mcp-consent/v1` (consent CSRF tokens),
`backyard/mcp-code/v1` (authorization codes), `backyard/mcp-refresh/v1` (refresh tokens) and
`backyard/mcp-access/v1` (access tokens). Both editions derive keys identically, so tokens
interoperate given the same secret. (`backyard/cli-confirm/v1` only signs CLI login
confirmation forms, which never leave the deployment that issued them, so it is not required
to interoperate between editions. Likewise the five `mcp-*` labels sign values that reference
one deployment's grant records, so they never interoperate between deployments or editions.)

HS256 JWTs signed with `keys.Derive(secret, "backyard/api-token/v1")`:
`{iss: "backyard", sub: user.id, email, name, groups, admin, scope: "api", jti, iat, exp}`
(≤ 16 KiB: an overlong name is shortened and trailing groups dropped, keeping admin and
allowed groups; 30 s clock leeway). Default TTL `auth.token_ttl` (720h). Minted by
`/__backyard/auth/cli` (browser → CLI loopback) and `POST /__backyard/api/tokens`
`{name?, ttl_hours?}` → `200 {token, expires_at, name?}` (`ttl_hours` ≤ `auth.token_ttl`, default
that TTL; anonymous users and static tokens get `403`; needs `X-Backyard: 1`). A token minted
by a request whose credential is a Backyard API token expires **no later than that token**:
`ttl_hours` and the default are capped at its remaining lifetime (whole seconds, never past
its `exp`; `expires_at` reports the result), and a presented token with almost nothing left
gets `401 unauthenticated`. So a token cannot extend its own lifetime, and offboarding at the
IdP ends access once the last token minted from a fresh authentication expires. Tokens minted
for a browser session, a proxy identity (or Cloudflare Access) or an IdP-issued JWT (device
flow) get the full lifetime. Revocation = rotate `server.secret`. Static tokens:
`auth.tokens: [{name, sha256, admin?}]` (hex sha256 of the token string; `backyard token hash`
prints it); they authenticate as `{id: "token:<name>", name, admin: <bool>}` and cannot mint
API tokens.

CLI login (`GET /__backyard/auth/cli?redirect_uri=http://127.0.0.1:PORT/callback&state=…`):
requires an authenticated browser user (session cookie or proxy identity — bearer tokens are not
accepted, so a token cannot renew itself); `redirect_uri` must be
`http://127.0.0.1|localhost|[::1]:<port>/…` and `state` (≤ 1 KiB, required with a
`redirect_uri`) is passed back; answers `302` (`Cache-Control: no-store`,
`Referrer-Policy: no-referrer`) to `redirect_uri?token=…&expires_at=…&state=…`. With
`?mode=display` the token is shown on a page for copy/paste (headless machines). Methods other than `GET` and `POST` → `405`.

**Cross-site confirmation.** Any web page can navigate a signed-in browser to this URL, and
whatever listens on the loopback port (not necessarily the CLI the user started) would receive
the token. So a request that another site may have started mints nothing — with a
`redirect_uri` and with `mode=display` alike — and answers `200` with a confirmation page
instead. That is a `GET` whose `Sec-Fetch-Site` is present and neither `none` (a navigation the
user started, as when the CLI opens the URL) nor `same-origin` — `same-site` included, because
sites are subdomains of the hub — or, without `Sec-Fetch-Site`, whose `Referer` has another
origin than the hub (or the request's own). The page names the user and the loopback origin and
holds an `application/x-www-form-urlencoded` form (≤ 16 KiB) that `POST`s `redirect_uri`,
`state`, `mode` and a CSRF token `csrf` back to `/__backyard/auth/cli`. The page is served with
`Referrer-Policy: no-referrer` and a CSP whose `form-action` allows only `'self'` and the
loopback origin (`http:` for `[::1]`, which CSP cannot name), so the form's redirect may reach
the CLI and nothing else. The CSRF token is `<unix expiry>.<base64url HMAC-SHA256>`, keyed with
`backyard/cli-confirm/v1`, over the user id, the browser session where there is one (the Go
server in `oidc` mode binds the session cookie; elsewhere the proxy or guest cookie identifies
the browser on every request, and the user id alone binds it), `redirect_uri`, `state`, `mode`
and the expiry, 10 minutes after the page was rendered. A `POST` is refused with `403` when its
`Sec-Fetch-Site` is present and not `same-origin`, when its `Origin` is present and neither the
hub's (or the request's own) nor `null` (what the page's referrer policy produces), or when its
CSRF token does not verify for the current user, session and request. A verified `POST` mints
the API token like a direct request and answers `303` to the loopback URL (or shows the token
for `mode=display`).

Cookies are `HttpOnly; SameSite=Lax; Path=/` and `Secure` when `server.public_url` is https
(the http login state cookie of §8.1 excepted). Their `Domain` is `.<base_domain>` in host
mode, and host-only in path mode and for `localhost`, IP or single-label base domains
(browsers reject those as cookie domains).

**Conflicting cookies fail closed.** Site JavaScript cannot read these cookies, but in host
mode a site's page can set a cookie of the same name for the parent domain or a longer `Path`,
which browsers send alongside — and before — Backyard's own; a sealed session or signed guest
id is not tied to a browser, so a site author could plant their own. Backyard therefore reads
every `backyard_session` (mode `none`: `backyard_anon`) cookie of a request, ignores invalid
ones, and when valid ones name different users treats the request as unauthenticated (`401`,
or the login redirect; never the first cookie). It then expires, best effort, every variant
of that cookie it does not set itself: host-only and the cookie domain (or hub host) and its
parent domains, crossed with each path prefix of the request (up to 8 segments) — never its
own `Domain=.<base_domain>` (or host-only) `Path=/` cookie, which would leave the planted one
alone. In the usual case the next request works again. Cookies the server cannot reach (a
partitioned cookie, a deeper path) keep failing closed. So in mode `none`, too, a browser
request can be refused with `401`. Several valid cookies of the same user are fine.

### 8.4 Remote MCP connector: OAuth 2.1 authorization server

Claude (claude.ai, Desktop, mobile, Cowork, and Claude Code) can use Backyard as a remote MCP
server, a "custom connector" (§14.1). The connector signs people in with the company sign-in
Backyard already uses; Claude never talks to the company IdP.

#### 8.4.1 Roles, URLs and enablement

Backyard is both the protected resource (`/__backyard/mcp`) and the OAuth 2.1 authorization
server (AS) for it. Okta (through `auth.mode: oidc`), IAP, the ALB, a trusted proxy or Cloudflare
Access only sign the **browser** in at `/__backyard/auth/authorize`; Backyard then shows its own
consent page and issues its own tokens.

* **P** (issuer, "hub URL"): the canonical form of the hub's public URL — lowercase scheme and
  host, no default port, no path, no trailing slash. Go: `server.public_url`, which must then
  have no path (§12). Worker: `PUBLIC_URL`, else derived from `BASE_DOMAIN`, else the request
  origin (§16). Example: `https://backyard.acme.com`. **H** is the hostname of P.
* **R** (resource) = `P + "/__backyard/mcp"`; **M** (protected resource metadata URL) =
  `P + "/.well-known/oauth-protected-resource/__backyard/mcp"`.
* **Canonical host:** a request is on the canonical hub host when its hostname (compared
  case-insensitively, port ignored) equals H. Go takes the host as §2 does (trusted
  `X-Forwarded-Host` included); the Worker takes the URL's hostname.
* **Loopback host:** `localhost`, any `*.localhost` name, `127.0.0.0/8` literals and `[::1]`.
  P may be plain `http` only on a loopback host (development and tests; §8.4.11).
* **Resource canonicalization** (authorize and token requests): parse the value as an absolute
  URI and reject it when the scheme is not http/https or when it has a userinfo, query or
  fragment part; then lowercase the scheme and host, drop `:443` for https and `:80` for http,
  remove exactly one trailing `/` from a non-empty path, and leave percent-encoding as is. The
  result must equal R byte for byte.

**Enablement.** Opt-in on both editions: Go `mcp.enabled` (default `false`, §12), Worker
`MCP_ENABLED` (unset = off, §16). While it is off — on the Worker also while the MCP variables
are invalid or the hub origin is neither https nor a loopback host — every path of the table
below answers `404`: the JSON paths with
`{"error":{"code":"not_found","message":"the Claude connector is not enabled on this server"}}`,
`/__backyard/auth/authorize` with an HTML page of the same text, and `/__backyard/api/info`
omits `mcp_url` (§7.1). The standalone gateway and the Lambda function serve none of these
paths.

| Path | Methods | Canonical hub host only | Behind the identity proxy |
|---|---|---|---|
| `/.well-known/oauth-protected-resource/__backyard/mcp` | GET, HEAD | yes | **no** (exempt, §8.4.10) |
| `/.well-known/oauth-authorization-server` | GET, HEAD | yes | **no** |
| `/__backyard/oauth/register` | POST | yes | **no** |
| `/__backyard/oauth/token` | POST | yes | **no** |
| `/__backyard/oauth/revoke` | POST | yes | **no** |
| `/__backyard/mcp` | POST | yes | **no** |
| `/__backyard/auth/authorize` | GET, POST | yes | **yes** |
| `/__backyard/api/mcp/grants`, `/__backyard/api/mcp/grants/{id}` | GET, DELETE | any hub host | yes (normal hub API) |

* **Site hosts:** `/.well-known/*` stays static site content; `/__backyard/mcp` and
  `/__backyard/oauth/*` answer the JSON `404`; `/__backyard/auth/authorize` gets the usual `302`
  to the same path and query on the hub.
* **A hub host that is not canonical:** the JSON paths answer `404 not_found` (message
  `not found`); authorize answers a `404` HTML page ("Use <P>/__backyard/mcp").
* **Wrong method:** the JSON paths answer `405` with `Allow` (`GET, HEAD` or `POST`) and
  `{"error":{"code":"bad_request","message":"method not allowed"}}`; authorize answers a `405`
  page with `Allow: GET, POST`. At `/__backyard/mcp` the method is checked after
  authentication (§14.1).
* **No CORS** on any of these paths: no `Access-Control-*` headers, and `OPTIONS` → `405`.
* **No cookies** are read or set on the six machine paths (the first six rows); the Worker never
  mints a guest cookie there. `/__backyard/oauth/<other>` and any other `/.well-known/oauth-*`
  path of the hub → `404` JSON, answered before anything authenticates.
* Metric areas (§15): `oauth` for `/.well-known/oauth-*` and `/__backyard/oauth/*`, `mcp` for
  `/__backyard/mcp`, `auth` for authorize, `api` for the grants API.

#### 8.4.2 Discovery documents

`GET /.well-known/oauth-protected-resource/__backyard/mcp` (RFC 9728) and
`GET /.well-known/oauth-authorization-server` (RFC 8414) answer `200`,
`Content-Type: application/json`, `Cache-Control: public, max-age=300`; `HEAD` gives the same
headers without a body. The protected resource metadata, with the keys in this order
(`<server name>` is `server.name` on Go and the `/info` `name` on the Worker, default
`Backyard`):

```json
{"resource":"<R>","authorization_servers":["<P>"],"scopes_supported":["mcp"],"bearer_methods_supported":["header"],"resource_name":"<server name>"}
```

The authorization server metadata, with the keys in this order:

```json
{"issuer":"<P>",
 "authorization_endpoint":"<P>/__backyard/auth/authorize",
 "token_endpoint":"<P>/__backyard/oauth/token",
 "registration_endpoint":"<P>/__backyard/oauth/register",
 "revocation_endpoint":"<P>/__backyard/oauth/revoke",
 "scopes_supported":["mcp","offline_access"],
 "response_types_supported":["code"],
 "response_modes_supported":["query"],
 "grant_types_supported":["authorization_code","refresh_token"],
 "token_endpoint_auth_methods_supported":["none"],
 "revocation_endpoint_auth_methods_supported":["none"],
 "code_challenge_methods_supported":["S256"],
 "authorization_response_iss_parameter_supported":true,
 "client_id_metadata_document_supported":true}
```

`client_id_metadata_document_supported` is present only while the effective client allowlist
(`mcp.clients` / `MCP_CLIENTS`) is non-empty; without it clients fall back to registration.
`offline_access` appears only in the AS metadata. The root
`/.well-known/oauth-protected-resource` and `/.well-known/openid-configuration` are not served
(every `401` names M, and there are no ID tokens).

#### 8.4.3 Client identification

There are no pre-registered clients and no client secrets. A `client_id` is resolved like this:

* **`https://…` — a Client ID Metadata Document (CIMD).** It must be byte-equal to an entry of
  the effective `mcp.clients` / `MCP_CLIENTS` (default
  `https://claude.ai/oauth/mcp-oauth-client-metadata` and
  `https://claude.ai/oauth/claude-code-client-metadata`); otherwise it is refused ("App not
  allowed") and nothing is fetched. `client_kind` is `metadata_document`, the display name is
  the document's sanitized `client_name` (else the URL host), and the verified host is the URL
  host.
* **`bydc_…` — a registered client.** Its MAC must verify (`backyard/mcp-client/v1`), else
  "Unknown app". `client_kind` is `registered`; name and redirect URIs come from the signed
  payload.
* **Anything else** → "Unknown app".

**CIMD fetch** (only allowlisted URLs; the token and revoke endpoints never fetch): a `GET` with
`Accept: application/json`, no cookies or credentials, no redirects (any `3xx` is an error),
5 s in total, only `200`, a `Content-Type` of `application/json` or ending in `+json`
(parameters allowed), at most 5120 bytes, UTF-8 JSON that is an object. The Go server dials
through its SSRF guard (public addresses only, also through an `HTTPS_PROXY`). The document
must have `client_id` equal to the URL; `redirect_uris` an array of 1–10 strings of which at
least one passes the redirect policy (§8.4.4; the others are ignored);
`token_endpoint_auth_method` absent or `"none"`; no `client_secret` or
`client_secret_expires_at`; `client_name` optional (sanitized as below; not a string → failure;
empty after sanitizing → the URL host). Other members,
`grant_types` included, are ignored. Any failure renders "Could not verify the app" with the
reason. A success is cached for `max-age` clamped to 60–3600 s (300 s without one; `no-store` or
`no-cache`: this request only); failures are never cached, but the URL is not fetched again for
10 s and requests in that window get the same failure; concurrent requests for one URL share
one fetch. The cache is per process (Go) or per isolate (Worker).

**`POST /__backyard/oauth/register`** (RFC 7591, stateless; no authentication, no
`X-Backyard`). The body is one JSON object (`Content-Type: application/json`, parameters
allowed, ≤ 16 KiB), else `400 {"error":"invalid_client_metadata","error_description":"…"}`.

* `redirect_uris` is required: 1–4 strings of ≤ 512 bytes, each passing the redirect policy,
  else `400 {"error":"invalid_redirect_uri","error_description":"redirect URI <uri> is not
  allowed on this server (allowed: the URIs in mcp.redirect_uris and http loopback URIs)"}`
  (the Worker names `MCP_REDIRECT_URIS`); a missing or malformed `redirect_uris` (not an array
  of 1–4 strings, an entry over 512 bytes) is `invalid_redirect_uri` too.
* `client_name` (optional; JSON `null` counts as absent, any other non-string →
  `invalid_client_metadata`): control characters (Cc) and the bidi controls U+200E, U+200F,
  U+202A–U+202E and U+2066–U+2069 are removed, the result trimmed and cut to 64 code points;
  empty or absent → `MCP client`.
* Every other member (`token_endpoint_auth_method`, `grant_types`, `response_types`, `scope`,
  `application_type`, `software_statement`, `logo_uri`, `client_uri`, `contacts`, `jwks`,
  `jwks_uri`, …) is accepted and ignored; the response states what was registered.
* `client_id` = `"bydc_" + Sign(backyard/mcp-client/v1, {"v":1,"n":<client_name>,"r":[<redirect_uris as sent>],"t":<unix seconds>})`
  (compact JSON, keys in this order; Sign as in §8.4.8). A `client_id` longer than 1024 bytes →
  `400 invalid_client_metadata` "too many or too long redirect_uris". Nothing is stored; a
  registration stays valid until `server.secret` changes, and the redirect policy is checked
  again on every use.
* `201`, `Content-Type: application/json`, `Cache-Control: no-store`:
  `{"client_id":"bydc_…","client_id_issued_at":<t>,"client_name":"…","redirect_uris":[…],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`.
* The Go server rate-limits it per client IP (§11):
  `429 {"error":"invalid_request","error_description":"too many requests"}` with `Retry-After`.

#### 8.4.4 Redirect URIs

A redirect URI is allowed — at registration, authorize, the consent `POST` and the token
endpoint — if and only if it is (a) byte-equal to an entry of the effective `mcp.redirect_uris` /
`MCP_REDIRECT_URIS` (https; default `https://claude.ai/api/mcp/auth_callback` and
`https://claude.com/api/mcp/auth_callback`), or (b) a **loopback URI**: scheme `http`, host
string exactly `localhost`, `127.0.0.1` or `[::1]`, an optional port, no userinfo, no fragment,
any path and query — but no backslash, ASCII control character or Unicode white space (Go's
`unicode.IsSpace`; U+FEFF is not one) anywhere, and no `%` before the query that does not start
a two-hex-digit escape. Nothing else, ever.

A requested `redirect_uri` matches a registered one when both are https and byte-equal, or both
are loopback URIs with equal scheme, host string, path and query, the port being ignored on
both sides (`localhost`, `127.0.0.1` and `[::1]` are different hosts). A URI with a fragment or
userinfo never matches.

| Registered | Requested | Match |
|---|---|---|
| `https://claude.ai/api/mcp/auth_callback` | same | yes |
| `https://claude.ai/api/mcp/auth_callback` | `https://claude.ai/api/mcp/auth_callback/` | no |
| `https://claude.ai/api/mcp/auth_callback` | `https://CLAUDE.ai/api/mcp/auth_callback` | no |
| `http://localhost/callback` | `http://localhost:53682/callback` | yes |
| `http://127.0.0.1:9/callback` | `http://127.0.0.1:41000/callback` | yes |
| `http://localhost/callback` | `http://127.0.0.1:53682/callback` | no |
| `http://[::1]/cb` | `http://[::1]:1/cb` | yes |
| `http://localhost/callback` | `http://localhost:1/callback?x=1` | no |
| `http://localhost/callback?a=1` | `http://localhost:5/callback?a=1` | yes |
| `http://localhost/callback` | `https://localhost/callback` | no |

Resource canonicalization (§8.4.1) with R = `https://hub.example/__backyard/mcp`:

| Input | Valid |
|---|---|
| `https://hub.example/__backyard/mcp` | yes |
| `HTTPS://HUB.EXAMPLE:443/__backyard/mcp/` | yes |
| `https://hub.example/__backyard/mcp?x=1` | no |
| `https://hub.example/__backyard/mcp#f` | no |
| `http://hub.example/__backyard/mcp` | no |
| `https://other.example/__backyard/mcp` | no |
| `https://hub.example/__backyard/MCP` | no |
| `https://u@hub.example/__backyard/mcp` | no |
| `""` (present but empty) | no |

#### 8.4.5 Authorization and consent

**`GET /__backyard/auth/authorize`.** The checks run in this order and the first failure
decides:

1. Go only: the rate limit (§11), which applies to whichever answer the steps below give: a
   request is charged to the person once step 6 has identified them (per client IP for a
   guest), any other request to its client IP; a spent budget turns the answer into a `429`
   page "Too many requests" with `Retry-After`.
2. The raw request URI (path + query) is longer than 2048 bytes, the query cannot be parsed (a
   `;` separator or a malformed `%`-escape), or one of `response_type`, `client_id`,
   `redirect_uri`, `code_challenge`, `code_challenge_method`, `resource`, `scope`, `state`,
   `response_mode` is repeated → `400` page "Invalid authorization request".
3. `client_id` (§8.4.3): missing or unknown → `400` page "Unknown app"; not allowlisted → `400`
   page "App not allowed" naming `mcp.clients` (Worker: `MCP_CLIENTS`); CIMD failure → `502`
   page "Could not verify the app" naming the URL and the reason and suggesting outbound HTTPS
   or "Register automatically".
4. `redirect_uri` is required; it must match one of the client's URIs and pass the current
   redirect policy (§8.4.4), else `400` page "Redirect address not allowed".
5. Steps 1–4 never redirect. From here on every error is a **`302`** to the redirect URI with
   `error`, `error_description`, `state` (when one was sent and is ≤ 512 bytes) and `iss=<P>`,
   URL-encoded, appended with `?` (or `&` when the URI already has a query), with
   `Cache-Control: no-store` and `Referrer-Policy: no-referrer`:
   * `response_type` ≠ `code` (absent included) → `unsupported_response_type`;
   * `request` present → `request_not_supported`; `request_uri` present →
     `request_uri_not_supported`;
   * `response_mode` present and ≠ `query` → `invalid_request`;
   * `code_challenge_method` ≠ `S256`, absent included → `invalid_request` "PKCE with S256 is
     required";
   * `code_challenge` not matching `^[A-Za-z0-9_-]{43}$` → `invalid_request`;
   * `resource` present and not canonicalizing to R → `invalid_target` (absent means R);
   * `state` longer than 512 bytes → `invalid_request`, sent without `state` (an empty `state`
     counts as absent);
   * `scope` is split on spaces: the granted scope is `mcp`, plus ` offline_access` when
     requested; other values are ignored, never an error. Unknown parameters (`prompt`,
     `nonce`, `login_hint`, …) are ignored.
6. The browser identity of the mode (§8.4.9; bearer tokens are never accepted): an
   infrastructure error → `503` page; refused by `allowed_domains`, `allowed_emails` or
   `allowed_groups` → `403` page "Access denied" (with a sign-out link where one exists);
   conflicting cookies → the `401` page of §8.3; Go `oidc` without a session → `302` to the
   login with `rd` = `P` + the request URI; a proxy mode or Worker `access` without an identity →
   `401` page; an anonymous user outside mode `none`, or a static-token identity → `403` page
   "This sign-in cannot connect apps".
7. `200`: the consent page. Consent is **always** shown — on every authorization, never
   remembered, whatever `Sec-Fetch-Site` says.

**The consent page** carries `Cache-Control: no-store`, `Referrer-Policy: no-referrer`,
`X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff` and

```
Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self' <ORIGIN>
```

where `<ORIGIN>` is the redirect URI's origin (`https://claude.ai`, `http://localhost:53682`,
`http://127.0.0.1:9`; `http:` for `[::1]`, which CSP cannot name). The Worker may add its
`script-src 'nonce-…'`; the page has no scripts. All values are HTML-escaped, and wording may
differ between editions, but every element is required:

1. the title "Connect an app to Backyard";
2. the user: "You are signed in as **<email, else name, else id>**", or in mode `none`
   "**Guest <first 4 hex>**. This Backyard has no sign-in; the app will act as this browser's
   guest"; a "Not you? Sign out" link to `/__backyard/auth/logout` where a logout exists (Go
   `oidc`, the Worker);
3. the app: CIMD "**<client_name>** — verified by **<client_id host>**"; registered
   "**"<client_name>"** (name provided by the app, not verified)";
4. where the approval goes: https "Backyard will send the approval to **<redirect origin>**";
   loopback "Backyard will send the approval to **an app on this computer (<redirect
   origin>)**. Only continue if you just started the sign-in from an app on this computer.";
5. what the app can do: "See the sites on <H> and read their files, and publish or change sites
   as you. Published sites are visible to everyone who can sign in to this Backyard.";
6. the lifetime: "This connection lasts until <YYYY-MM-DD (UTC) of consent + family lifetime>
   unless you disconnect it on the Backyard hub.";
7. the forwarding warning: "Only allow this if you started connecting <client_name> to Backyard
   yourself just now. Anyone who sent you this link would get this access.";
8. `<form id="consent" method="post" action="/__backyard/auth/authorize">` with the hidden inputs
   `response_type` (`code`), `client_id`, `redirect_uri`, `state` (omitted when absent),
   `code_challenge`, `code_challenge_method` (`S256`), `scope` (the granted scope), `resource`
   (R) and `csrf`, and the buttons
   `<button type="submit" name="decision" value="allow" id="allow">Allow</button>` and
   `<button type="submit" name="decision" value="deny" id="deny">Cancel</button>` (no
   `autofocus`);
9. click-delay arming against double-clickjacking: the buttons start `visibility:hidden` and are
   revealed by `animation: by-arm 0s linear 0.8s forwards` with
   `@keyframes by-arm { to { visibility: visible } }` in an inline `<style>`.

The CSRF token is `"<unix exp>.<base64url(HMAC-SHA256(backyard/mcp-consent/v1 key, message))>"`
(≤ 128 characters, `exp` = render time + 600 s) over the UTF-8 compact JSON array
`["mcp-consent", exp, user.id, binding, client_id, redirect_uri, state, code_challenge, scope, resource]`,
where `state` is `""` when absent, `scope` is the granted scope, `resource` is R and `binding` is
the raw `backyard_session` cookie that authenticated the request on the Go server in `oidc`
mode, `""` everywhere else.

**`POST /__backyard/auth/authorize`** (the consent decision):

1. The body must be `application/x-www-form-urlencoded`, ≤ 16 KiB, UTF-8 → else a `400` page.
2. Go only: the rate limit, as for `GET` (step 4 identifies the person).
3. Browser signals → `403` page "This approval was not sent from the Backyard page", no
   redirect, when `Sec-Fetch-Site` is present and ≠ `same-origin`; or `Origin` is present and
   neither the hub origin, the request's own origin nor `null`; or `Sec-Fetch-Mode` is present
   and ≠ `navigate`; or `Sec-Fetch-Dest` is present and ≠ `document`.
4. GET steps 3–6 run again on the form fields, plus the parsing and repeated-field checks of
   step 2 (the 2048-byte cap applies only to `GET`); redirect errors use **`303`**.
   `response_type`, `code_challenge_method` and `resource` are required in the form (a missing
   one fails as in step 5; a missing `resource` → `invalid_request` "resource is required"). Go
   `oidc` without a session → `401` page "Your Backyard sign-in ended" with a link to the login
   with `rd` = the `GET` form of the request (a link, not a redirect: the consent page's
   `form-action` applies to every redirect of the form submission and would block the hop to
   the IdP).
5. `csrf` must verify for the current user, binding and fields, with `exp` ≥ now − 60 s → else
   `403` page "This approval page expired or was not issued to you. Go back to the app and
   connect again." (audit `mcp.consent_rejected`).
6. `decision=deny` → `303` to the redirect URI with `error=access_denied`, `state` and `iss`
   (audit `mcp.deny`). `decision=allow` → a pending grant is created (§8.4.8), the code is
   `"byac_" + Sign(backyard/mcp-code/v1, {"g":<gid>,"x":<now+300>})`, and the answer is `303`
   to `redirect_uri` with `code`, `state` and `iss` (same query rules; `Cache-Control:
   no-store`, `Referrer-Policy: no-referrer`; audit `mcp.authorize`); a storage failure → `503`
   page. Any other `decision` → `400` page.

#### 8.4.6 Token endpoint

`POST /__backyard/oauth/token` never reads cookies and needs no `X-Backyard`. The body is
`application/x-www-form-urlencoded`, ≤ 16 KiB; a repeated parameter or a body that cannot be
parsed (a `;` separator, a malformed `%`-escape) → `invalid_request`. Every
answer has `Content-Type: application/json`, `Cache-Control: no-store` and `Pragma: no-cache`;
errors are `{"error":"<code>","error_description":"<text>"}` with status `400`, except
`invalid_client` (`401`). Storage errors →
`500 {"error":"server_error","error_description":"request <X-Request-Id>"}`. The Go server
rate-limits it (§11): a request whose `code` or `refresh_token` names a grant of the
authenticated client is charged to the grant's user before anything changes (per client IP for
a guest), any other request to its client IP; a spent budget turns the answer into a `429` as
for registration (without `WWW-Authenticate`).

**Client authentication** (public clients only): `client_id` is required, in the body or as
`Authorization: Basic base64(client_id:)` with an **empty** password (the scheme in any case,
exactly one space, padded standard base64; both parts form-urlencoded as in RFC 6749 §2.3.1);
when both are given they must be equal (else `invalid_request`). Any other `Authorization`
header (an unpadded value or more than one space included), a non-empty Basic
password or a non-empty `client_secret` → `401 invalid_client` with
`WWW-Authenticate: Basic realm="Backyard"` (so do two `Authorization` headers). A missing
`client_id`, or one that is not an allowlisted CIMD URL or a `bydc_` id with a valid MAC
(nothing is fetched here) → `401 invalid_client` without a challenge. A present `resource` is
checked (`invalid_target`) before `grant_type`; a missing `grant_type` → `invalid_request`.

**`grant_type=authorization_code`.** `code` and `code_verifier` are required. `redirect_uri` is
optional; when present it must equal the grant's byte for byte. `resource` is optional; when
present it must canonicalize to R, else `invalid_target`. Then, each failure being
`invalid_grant`:

1. `code` has the `byac_` prefix, a valid MAC and `x` ≥ now;
2. the grant exists and its code has not expired (a grant already redeemed is not rejected
   here: the replay is detected in step 6, after PKCE has verified);
3. `client_id` equals the grant's;
4. the `redirect_uri` rule above;
5. `code_verifier` matches `^[A-Za-z0-9._~-]{43,128}$` and `BASE64URL(SHA-256(ascii(verifier)))`
   equals the grant's `code_challenge` (constant-time) — a failure here consumes and revokes
   nothing;
6. redemption: `rotate(gid, 0, grace=false)` (§8.4.8). `reuse` — a replay of a valid request —
   has revoked the grant: `invalid_grant`, audit `mcp.code_replay` and `mcp.revoke`
   (`via=replay`); `missing` → `invalid_grant`;
7. re-gating (§8.4.8); a failure revokes the grant: `invalid_grant` "access removed by policy";
8. success (audit `mcp.token`):
   `{"access_token":"byat_…","token_type":"Bearer","expires_in":<int>,"refresh_token":"byrt_…","scope":"<granted scope>"}`
   with `expires_in` = min(3600, grant expiry − now) and the refresh token of generation 1. A
   refresh token is always issued.

**`grant_type=refresh_token`.** `refresh_token` is required. `scope` is optional and must be a
subset of the granted scope (else `invalid_scope`); it only changes the `scope` of this answer.
`resource` is optional and must canonicalize to R (else `invalid_target`). Then:

1. `byrt_` prefix, a valid MAC and `n` ≥ 1, else `invalid_grant`;
2. the grant exists, has been redeemed and has not expired, else `invalid_grant`;
3. `client_id` equals the grant's, else `invalid_grant` (steps 1–3 revoke nothing);
4. `rotate(gid, n, grace=true)`: `ok` → the refresh token of generation n+1; `grace` → the same
   generation n+1 token another request already received (refresh tokens are deterministic per
   generation); `reuse` → the grant is revoked, `invalid_grant`, audit `mcp.refresh_reuse` and
   `mcp.revoke` (`via=replay`);
   `missing` → `invalid_grant`;
5. re-gating; a failure revokes the grant: `invalid_grant` "access removed by policy".

A narrowed `scope` is answered deduplicated, in the order requested.

A success has the same shape with a new access token and the generation n+1 refresh token.
Refreshes are not audited. Other grant types → `unsupported_grant_type`.

#### 8.4.7 Revocation and the grants API

**`POST /__backyard/oauth/revoke`** (RFC 7009): a form body ≤ 16 KiB with the required `token`
(`token_type_hint` is ignored) and the client authentication of §8.4.6. A `byrt_` or `byat_`
token with a valid MAC whose grant exists and belongs to that `client_id` deletes the grant
(audit `mcp.revoke`, `via=rfc7009`). The answer is always `200` with an empty body — also for
unknown, expired or malformed tokens and tokens of other clients, so the endpoint is no oracle.
A missing `token` → `400 invalid_request`; client authentication failures → `401
invalid_client`; the form is checked first, then the client, then `token`. A storage error →
`500 server_error` as for the token endpoint. The Go server rate-limits it per client IP.

**`/__backyard/api/mcp/grants`** is a normal hub API (browser session, proxy identity, API token
or static token, with `X-Backyard: 1` on `DELETE` and the API rate limit); MCP access tokens are
not accepted there, and it answers `404` while the connector is disabled.

* `GET /__backyard/api/mcp/grants[?user=<id>]` → `200 {"grants":[Grant…]}`: without `user` the
  caller's own grants, with `user` any person's (admins only, else `403 forbidden`); live grants
  only (redeemed, not expired), newest first, at most 200. A `Grant` is
  `{"id":"<gid>","user":{"id":"…","email":"…","name":"…"},"client_name":"…","client_kind":"metadata_document"|"registered","client_host":"<client_id host or \"\">","redirect_host":"<host of redirect_uri>","scope":"mcp …","created_at":"<RFC3339>","expires_at":"<RFC3339>"}`.
* `DELETE /__backyard/api/mcp/grants/{id}` → `204`, for the grant's user or an admin; someone
  else's grant or an unknown id → `404`. Audit `mcp.revoke` (`via=hub`).
* `DELETE /__backyard/api/mcp/grants[?user=<id>]` → `200 {"revoked":<n>}`: the caller's own
  grants, or with `user` ≠ the caller any person's (admins only, else `403`). Audit `mcp.revoke`
  (`via=hub`, `count`).
* Guests (mode `none`) see and revoke their own grants. A static token acts as `token:<name>`,
  which owns no grants; an admin static token may use `?user=`.

The hub UI shows a "Use Backyard in Claude" card while `info.mcp_url` exists: the URL with a copy
button, the setup steps, the caller's connections with a "Disconnect" button each, and for
admins a form that disconnects one person.

#### 8.4.8 Tokens, grants and rotation

All tokens are opaque. `Sign(L, payload)` is `base64url(payload) "." base64url(HMAC-SHA256(key,
payload))` with `key = keys.Derive(server.secret, L)` (§8.3), over compact JSON with the keys in
the order shown; verification checks the prefix, then the MAC, then the JSON field types.

| Item | Format | Payload | Notes |
|---|---|---|---|
| registered client id | `bydc_` + Sign(`backyard/mcp-client/v1`) | `{"v":1,"n":name,"r":[uris],"t":iat}` | ≤ 1024 bytes; no expiry; valid until `server.secret` changes |
| authorization code | `byac_` + Sign(`backyard/mcp-code/v1`) | `{"g":gid,"x":exp}` | 300 s; single use |
| refresh token | `byrt_` + Sign(`backyard/mcp-refresh/v1`) | `{"g":gid,"n":n}` | n ≥ 1; deterministic for (gid, n) |
| access token | `byat_` + Sign(`backyard/mcp-access/v1`) | `{"g":gid,"i":iat,"x":exp}` | exp = min(iat + 3600, grant expiry); ≤ 4096 bytes |
| consent CSRF | `<exp>.<base64url MAC>` (`backyard/mcp-consent/v1`) | the array of §8.4.5 | 600 s |

`gid` is 32 lowercase hex characters (16 random bytes). Logs, audit records, tool results and
error messages never contain these values.

**The grant record** — one per consent — is the single source of truth: `id` (gid), `user_id`,
`user` (the snapshot `{id, email, name, groups, anonymous}` taken at consent), `client_id`,
`client_name`, `client_kind` (`metadata_document` or `registered`), `redirect_uri` (exactly as
authorized), `code_challenge`, `scope` (`mcp` or `mcp offline_access`), `resource` (R at
consent), `created_at` (unix s), `code_exp` (`created_at` + 300), `redeemed_at` (set at
redemption) and `exp` (pending: `code_exp` + 600; after redemption: `created_at` + the family
lifetime, which is `auth.token_ttl` on Go and `TOKEN_TTL_HOURS` on the Worker, counted from
consent and never extended). Revocation deletes the record; a missing grant fails every check.
Storage: §10 (Go) and §16 (Worker). Expired grants and rotation markers are pruned, a bounded
page at a time, when grants are created.

**`rotate(gid, from, grace)`** is the only state transition (codes: `from=0, grace=false`;
refreshing generation n: `from=n, grace=true`):

```
rotate(gid, from, grace, now):
  g := grant(gid); if g missing or g.exp <= now → "missing"
  (from ≥ 1 additionally requires g.redeemed_at, else "missing")
  if from == 0 and g.redeemed_at → revoke(gid) → return "reuse"
  if from ≥ 1 and marker(gid, from) does not exist → revoke(gid) → return "reuse"
  if from ≥ 2: delete marker (gid, from-1)
  try atomically create marker (gid, from+1) with at=now
    success → if from == 0: g.redeemed_at = now, g.exp = g.created_at + family lifetime
              return "ok"
    conflict →
      m := marker(gid, from+1)
      if grace and now - m.at <= 60 s and marker(gid, from+2) does not exist → return "grace"
      revoke(gid) → return "reuse"
```

Markers are created in order only, and only the newest two are kept (a grant holds at most two,
so refreshing adds no state): a presented generation without its marker, a conflict at n+1
together with an existing marker n+2, and a code of a redeemed grant all prove an old token. Go
creates the marker with an atomic docstore `Create` (a conflict is the conflict) after the
delete has succeeded, and re-checks after a successful `Create` that marker `from` still exists
(for a code: that the grant is still unredeemed) — else the request read the store before later
rotations and has filled a gap: revoke, `reuse`. The Worker runs the whole function in one
RegistryDO transaction.

**Verifying an access token** (every request to `/__backyard/mcp`; there is no cache, so a
revocation takes effect at once): the `byat_` prefix, ≤ 4096 bytes, a valid MAC and valid JSON
(else `malformed token`); `x` > now (else `token expired`); the grant exists, is redeemed and has
not expired (else `token revoked`); its `resource` equals the current R (else `token not issued
for this server`); then the user is rebuilt from the snapshot and **re-gated**: an `anon:`
subject only while the mode is `none`, never a `token:` subject; on Go everything else goes
through the API-token path (directory enrichment, admin recomputed from the configuration,
`allowed_domains` / `allowed_emails` / `allowed_groups`); on the Worker admin is recomputed from
`ADMINS`/`ADMIN_GROUPS`. A gate failure deletes the grant (`access removed by policy`; audit
`mcp.revoke`, `via=policy`). The same re-gating runs at the code exchange and at every refresh.

#### 8.4.9 Browser identity per mode

The machine endpoints behave the same in every mode; only the browser step at
`/__backyard/auth/authorize` differs:

| Edition / mode | Identity at `/__backyard/auth/authorize` | Not signed in | Notes |
|---|---|---|---|
| Go `oidc` | the `backyard_session` cookie | `302` to the login with `rd` = the authorize URL | consent CSRF bound to the session cookie |
| Go `iap` / `alb` / `header` | the signed proxy assertion / trusted header | `401` page: "Sign-in required. The identity proxy did not authenticate this request; /__backyard/auth/authorize must stay behind it." | the four machine paths must be exempt (§8.4.10) |
| Go `none` | the guest cookie `backyard_anon` (minted if absent) | — | grant subject `anon:<16 hex>`; publishing needs `deploy.allow_anonymous`; outside dev mode the server logs `mcp: auth.mode none: every connector connection is an anonymous guest` |
| Worker `access` | `Cf-Access-Jwt-Assertion` / `CF_Authorization` | `401` page as above | conflicting Access cookies → `401` page |
| Worker `none` | the guest cookie | — | publishing needs `ALLOW_ANONYMOUS_DEPLOY`; `DEPLOY_TOKEN` plays no part |
| Worker, any other `AUTH_MODE` | — | — | the whole Worker answers `503` (§16) |

In every mode an anonymous user (outside mode `none`) or a static-token identity gets the `403`
page "This sign-in cannot connect apps".

#### 8.4.10 Proxy exemptions

An identity proxy in front of Backyard (IAP, an ALB with OIDC, Cloudflare Access, oauth2-proxy,
Pomerium, Easy Auth) must let exactly these four machine paths through without a login:

* `/__backyard/mcp`
* `/__backyard/oauth/*`
* `/.well-known/oauth-authorization-server`
* `/.well-known/oauth-protected-resource/__backyard/mcp`

`/__backyard/auth/authorize` **must stay** behind the proxy: the proxy's login is the sign-in.
None of the four paths reads cookies or identity headers, so exempting them — even through a
"trusted" `header`-mode peer — cannot spoof an identity, and the deployment kits exempt them
unconditionally; while the connector is off Backyard answers them with `404` itself. The
Cloudflare edition's Access bypass list is in §16.

#### 8.4.11 Deviations from the standards

1. **http issuer.** Plain-http P (and so http AS endpoints) is allowed only on loopback hosts,
   for development and tests. RFC 8414 and MCP require https.
2. **Loopback ports.** The port is ignored for `localhost` as well as for loopback IP literals
   (RFC 8252 §7.3 names IP literals; Claude Code uses `localhost`).
3. **Missing `MCP-Protocol-Version`.** Treated as `2025-06-18`, not `2025-03-26`, because
   2025-03-26 (with its batching) is not supported.
4. **Refresh grace.** A refresh token that was rotated ≤ 60 s ago, and whose successor has not
   been used, returns that same successor instead of triggering reuse revocation (OAuth 2.1,
   section 4.3.1, requires revocation on reuse).
5. **Revocation answers.** RFC 7009 revocation answers `200` for tokens of other clients instead
   of an error, so the endpoint is not an oracle.
6. **CIMD scope.** CIMD is supported only for allowlisted `client_id` URLs (a domain trust
   policy, which MCP permits); other URL client ids are refused without a fetch.
7. **Scopes.** Unknown requested scopes are ignored and the granted scope is returned.
8. **Login before consent.** The upstream login (Okta via Backyard's OIDC client, or a proxy)
   happens before the consent page. It creates only Backyard's own browser session, never an
   MCP grant. The consent page is shown on every authorization before any code exists, so the
   "consent before upstream state" rule of the MCP proxy guidance is met in substance.
9. **Token-endpoint `redirect_uri`.** Optional, as in OAuth 2.1; when present it must match.
10. **`code_challenge_method`.** Absent means `plain` per OAuth, and is therefore refused: S256
    is required.

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

The Claude connector (§8.4) keeps its state in `_system` too (quotas do not apply):
`_system/_mcp_grants/<gid>` (the grant record, `id` = the document id),
`_system/_mcp_rotations/<gid>.<n>` (`{"gid","n","at":<unix ms>,"exp":<the grant family's
exp>}`, created with an atomic `Create` so that a code is redeemed and a refresh token rotated
exactly once; the newest two per grant are kept, §8.4.8), `_system/_mcp_locks/<site>` (the publish lease
`{"exp":<unix s>,"holder":<32 hex>}`, §14.1) and
`_system/_mcp_lock_takeovers/<site>.<version>.<µs>.<k>` (`{"exp"}`, the claim of an expired lease
taken over, §14.1). The Cloudflare edition keeps the same records in
`RegistryDO` tables (§16).

## 11. Limits & rate limiting

Token buckets per instance (`ratelimit`), keyed by user id (or client IP when anonymous); each
bucket holds one period's worth and refills continuously: `api_per_minute` (all site and hub API
calls, default 600), `db_writes_per_minute` (document writes and file deletes, default 300),
`ai_per_minute` (default 20), `ai_tokens_per_day` (default 2,000,000 input+output tokens, reset
at 00:00 UTC), `warehouse_per_minute` (default 30), `uploads_per_minute` (default 60) and
`deploys_per_hour` (enforced by the deploy gateway, default 60). `ratelimit.disabled` turns them
off; a negative value disables a single limit — `ai_tokens_per_day` included — and 0 (or leaving
the key out) selects the default. With N instances the effective limits are up to N times
higher. Per-site storage limits: `database.max_docs_per_site` (default 100,000; negative =
unlimited; a soft quota, as instances count independently), `database.max_doc_bytes`; per-site
deploy limits: `sites.max_site_bytes`, `sites.max_files`, `sites.max_file_bytes`.

The Claude connector (§8.4, §14.1) adds no rate-limit keys on the Go server. Registration and
revocation are charged to `api_per_minute` keyed by client IP. Authorize and the token endpoint
are charged to `api_per_minute` keyed by the person a request identifies (the signed-in browser,
the user of the grant a code or refresh token names; per client IP for guests) and by client IP
when it identifies no one — except that a valid authorization request of a browser without a
Backyard session in `oidc` mode (the redirect to the login, or the "sign-in ended" page of the
consent `POST`) is not charged — so failed requests from one address (all of a proxy's clients
when `trusted_proxies` cannot resolve them) never lock out the people who connect through it,
signed in or not. The MCP endpoint is charged to `api_per_minute` keyed by the connection's user
(`429 rate_limited` with `Retry-After`), and `backyard_publish` to `deploys_per_hour` keyed by
that user (guests included). The Cloudflare edition's daily caps are in §16. Its fixed limits:

| Limit | Value |
|---|---|
| authorization code lifetime | 300 s |
| consent CSRF token lifetime | 600 s (verification accepts up to 60 s of clock skew) |
| access token lifetime | 3600 s, never past the grant's expiry |
| refresh family lifetime | `auth.token_ttl` (Go, default 720h) / `TOKEN_TTL_HOURS` (Worker, default 720), counted from consent |
| refresh grace window | 60 s |
| retention of an unredeemed grant | code expiry + 600 s |
| authorize request URI (path + query) | ≤ 2048 bytes |
| `state` | ≤ 512 bytes |
| registered `client_id` | ≤ 1024 bytes |
| registered `redirect_uris` | 1–4 entries of ≤ 512 bytes |
| displayed `client_name` | ≤ 64 code points after sanitizing |
| CIMD fetch | 5 s, ≤ 5120 bytes, no redirects, cached 60–3600 s (default 300), failures throttled 10 s |
| OAuth request bodies (register, token, revoke, consent `POST`) | ≤ 16 KiB |
| bearer token at `/__backyard/mcp` | ≤ 4096 bytes |
| MCP request body | ≤ 2 × the publish cap + 1 MiB (Go default 9437184, Worker default 5242880) |
| decoded file bytes per `backyard_publish` call | Go `mcp.max_publish_bytes` (default 4194304); Worker `LIMITS.mcp_publish_bytes` (default 2097152) |
| files per publish call | ≤ 1000 in `files`, ≤ 1000 in `delete` |
| `backyard_read_site_file` | ≤ 131072 bytes, text only |
| `backyard_list_sites` / `backyard_site_files` | ≤ 200 sites / ≤ 2000 files |
| Go MCP request deadline | 5 min (write, set before the body is read; the paced read ends by then too) |
| Go MCP body pace | byte n due 10 s + n / 65536 s after the body is first read (an average of ≥ 64 KiB/s), else `400` |
| Go large-body admission | ≤ 4 concurrent MCP requests with a body > 1 MiB or of unknown length, ≤ 1 of them per user; others wait ≤ 30 s, then `429` |
| Go publish lease | 300 s |

## 12. Configuration

YAML (`backyard.yaml`). Sources, first match wins: `--config <file>`, inline YAML in
`BACKYARD_CONFIG_YAML`, the file named by `BACKYARD_CONFIG`, `./backyard.yaml`. `${VAR}` and
`${VAR:-default}` are expanded from the environment before parsing (an unset or empty variable
yields the default or `""`; `$${` escapes a literal `${`). Unknown keys are errors. Durations
accept Go syntax (`12h`), a plain number of seconds, or days (`90d`). After parsing,
`BACKYARD_SECRET`, `BACKYARD_BASE_DOMAIN` and `BACKYARD_PUBLIC_URL` fill `server.secret`,
`server.base_domain` and `server.public_url` when those are empty, and `DATABASE_URL` fills
`database.postgres.dsn` for `database.type: postgres`. If `server.listen` is empty, `:$PORT`
(Cloud Run / Container Apps) else `:8080`.
`backyard config example` prints a fully commented example; `backyard config validate` checks
one. See `internal/config/config.go` for the authoritative schema; `--dev` (or `server.dev:
true`) fills in: `base_domain: localhost`, `auth.mode: none`, fs storage + sqlite under
`./.backyard-data`, memory pubsub, `deploy.allow_anonymous: true` (with `auth.mode: none`), a
random (ephemeral) secret unless one is configured, and text logs; it also enables the dev host
rules of §2.

Validation reports every problem at once and also rejects inconsistent combinations, among
them `auth.mode: oidc` with `url_mode: host` unless `base_domain` can carry a cookie `Domain`
(not `localhost`, `*.localhost`, an IP address or a single-label name) and the host of
`public_url` is `base_domain` or one of its subdomains (§8.1), and `deploy.allow_anonymous`
without `auth.mode: none`.

The Claude connector (§8.4) is configured in the `mcp` section: `mcp.enabled` (default
`false`), `mcp.clients` (the allowlisted client metadata document URLs; default the two Claude
documents of §8.4.3; an explicit `[]` turns CIMD off, so clients register automatically),
`mcp.redirect_uris` (the exact https addresses that may receive codes; default the two Claude
callbacks of §8.4.4; loopback addresses are always allowed and never listed) and
`mcp.max_publish_bytes` (decoded file bytes per `backyard_publish` call, default 4194304).
Entries are trimmed but their case is kept, as they are compared byte for byte. Validation
rejects, whether or not the connector is enabled: a `mcp.clients` entry that is not an https URL
with a host that is not an IP literal, a path other than `/` and empty, no `.`/`..` segments, no
query, fragment or userinfo, at most 512 bytes; a `mcp.redirect_uris` entry that is not an https
URL without fragment or userinfo of at most 512 bytes, or whose host is the `public_url` host,
`base_domain` or a subdomain of it; and a `mcp.max_publish_bytes` outside 65536–67108864. With
`mcp.enabled` it also requires a `mcp.max_publish_bytes` of at most `sites.max_site_bytes` (so a
small `sites.max_site_bytes` stays valid while the connector is off), an http(s) `server.public_url`
without a path, query or fragment that is https unless its host is a loopback host (§8.4.1), and
in `auth.mode: oidc` an `auth.oidc.redirect_url` whose path is not `/__backyard/auth/authorize`.
The standalone gateway validates `mcp` but ignores it.

## 13. CLI

```
backyard serve   [-c f] [--dev]                run the server
backyard gateway [-c f]                        run only the deploy gateway (§7, bearer tokens only)
backyard init    [site] [-d dir] [--agents claude,codex,cursor,copilot] [-t blank|app] [-f]
backyard dev     [dir] [-p 4321] [--site s] [-c f] [--no-open]   local preview with a local backend
backyard deploy  [dir] [-s site] [-m auto|api|s3|gcs|azure|rsync] [-n] [-y] [--message m] [--open] [--concurrency 8]
backyard login   [--device] [--no-browser]     (plus the global --server/--gateway/--token/--profile)
backyard logout | whoami
backyard sites   list [filter] | info <site> | delete <site> [-y]
backyard open    [site]                        open site (or hub) in the browser
backyard pull    <site> [dir] [-f]             download a site's files (never what deploys never publish)
backyard token   create [--name cli] [--ttl 720h|90d] | hash [token|-] [--name ci]
backyard skill   install [--global|--project] [-y]
backyard mcp                                   MCP server over stdio (for agents)
backyard config  example | validate [file] [--dev]
backyard cloudflare setup|domain add <host>|domain remove|access enable|access disable|status [--dir d] [--package p] [--no-login] [--skip-install] [options]
backyard version | completion <shell>
```

Global flags: `--server`, `--gateway`, `--token`, `--profile`, `--json` (machine-readable
output), `-v`/`--verbose`.

Local CLI config: `$XDG_CONFIG_HOME/backyard/config.json` (`~/.config/backyard/config.json`):
`{"current": "default", "profiles": {"default": {"server": "…", "gateway": "…", "token": "…",
"token_expires_at": "…", "method": "auto", "refresh_token": "…", "device": {…}}}}` (mode 0600).
Env overrides: `BACKYARD_SERVER`, `BACKYARD_GATEWAY`, `BACKYARD_TOKEN`, `BACKYARD_PROFILE`,
`BACKYARD_DEPLOY_METHOD`.

Project file `.backyard.json` (searched in the current directory and its parents; `dir` is
relative to the file and must name a folder inside the file's folder — an absolute path, a
`..` that leaves it or a symlink leading out is an error): `{"site": "lunch-vote", "dir": ".",
"method": "auto", "ignore": […]}`. Deploy ignore rules come in two tiers:

* **Never uploaded**, whatever the configuration: `.git/` (at any depth), `.backyard.json` and
  `.backyardignore`, in any letter case. No `!` line can re-include them, and the gateways
  reject `.git` path segments anyway (§7.3).
* **Ignored by default**: `node_modules/`, `.claude/`, `.cursor/`, `.github/`, `CLAUDE.md`,
  `AGENTS.md`, `.DS_Store` and every other path segment starting with `.` except
  `.well-known/`. The lines of `.backyardignore` in the deployed folder's root and the
  `.backyard.json` `ignore` list come after these defaults and may re-include them with `!`
  (e.g. `!.htaccess`).

The pattern syntax is gitignore's (`#` comments, `!` negation where the last matching pattern
wins, trailing `/`, leading or inner `/` anchors, `**`; as in git, a path inside an excluded
directory cannot be re-included; a UTF-8 byte order mark at the start of `.backyardignore` is
ignored). `backyard pull` never writes a path that the default rules exclude (both tiers),
even with `-f` and even if the site holds it (published by a direct sync, the raw gateway API
or before these rules); it lists such paths as skipped. Segments are compared the way
case-insensitive and Windows file systems resolve them.

Deploy methods (direct methods compute their excludes by walking the folder with the same
rules as `api`, resolve a symlinked folder first and never follow symlinks out of it):
* `api` — §7 protocol via the gateway.
* `s3` — `aws s3 sync <dir> s3://<bucket>/<prefix>sites/<site>/ --delete --no-follow-symlinks [--region r] [--endpoint-url e] --exclude …`
  (each excluded directory as `dir/*` and file by its escaped path, plus name globs that mean
  the same in `fnmatch`)
* `gcs` — `gcloud storage rsync <dir> gs://<bucket>/<prefix>sites/<site> --recursive --delete-unmatched-destination-objects --exclude=<regex>`
  (falls back to `gsutil -m rsync -r -d -e -x <regex>`)
* `azure` — `az storage blob sync --account-name <a> -c <container> -s <dir> -d <prefix>sites/<site> --delete-destination true [--exclude-pattern "p1;p2"] [--exclude-path "d1/;f2"]`
* `rsync` — `rsync -az --delete --delete-excluded --filter='P .backyard-tmp-*' --include=…/--exclude=… <dir>/ <rsync_target>/<site>/`
  (`--delete-excluded` removes files the rules exclude from the target; the protect rule
  keeps the `fs` store's in-flight temporary files)
* `auto` — `api` if a gateway is reachable and the user has a token (or anonymous deploys are
  allowed); otherwise the first advertised direct method whose tool is installed.

The sync tools never delete destination files that their excludes match. So after an `s3`,
`gcs` or `azure` sync, when the gateway is usable (reachable, with a token or anonymous
deploys), the CLI lists the site (`GET G/sites/{site}/files`) and removes the stored files the
ignore rules exclude (`aws s3 rm <dest> --recursive --exclude '*' --include <escaped path>…`,
`gcloud storage rm` / `gsutil -m rm` — names containing `*?[]#` cannot be addressed and are
reported — or `az storage blob delete`); a failed removal fails the deploy. Then it calls `POST
G/sites/{site}/refresh` (best effort). Without a usable gateway it prints a note instead. The
CLI uses a profile's cached `/info` (storage, rsync target) only for that profile's own
server, never when `--server`/`BACKYARD_SERVER` names another one.

Login flows (`backyard login`): `--token` (or `-` for stdin) stores a given token; `--device`
runs the OAuth device grant against `info.auth.device`; a server with anonymous deploys needs
nothing; `auth.mode: none` without them asks for a static deploy token; otherwise the browser
loopback flow via `info.auth.cli_login_url` (`--no-browser`: `?mode=display` and paste). An
external gateway advertised in `info.gateway_url` is adopted when reachable.

## 14. Agent integration

* `skills/backyard/SKILL.md` (+ `references/api.md`, `references/recipes.md`,
  `references/deploy.md`) — canonical agent docs; embedded in the binary.
* `backyard init` writes `index.html`, `.backyard.json`, `.backyardignore`, and per
  `--agents` (default `claude,codex`): `CLAUDE.md` + `.claude/skills/backyard/**` (claude),
  `AGENTS.md` (codex), `.cursor/rules/backyard.mdc` (cursor),
  `.github/copilot-instructions.md` (copilot). Existing files are kept unless `--force`.
* Claude Code plugin: `.claude-plugin/marketplace.json` + `.claude-plugin/plugin.json` at the repo
  root (skills, slash commands `/backyard:new`, `/backyard:deploy`, `/backyard:docs` in
  `commands/`, MCP server `backyard mcp`).
* MCP tools (`backyard mcp`): `backyard_docs`, `backyard_init`, `backyard_deploy`,
  `backyard_list_sites`, `backyard_site_files`, `backyard_read_site_file`, `backyard_whoami`,
  `backyard_open_url`. `backyard_init` and `backyard_deploy` use folders inside the working
  directory; any other folder only when named by the absolute path of an existing directory,
  and never the filesystem root, the home directory or a folder that holds credentials (the
  CLI's config folder or one whose deploy would include its config file, hidden folders of the
  home directory such as `~/.ssh`, and `~/Library` or `~/AppData`, unless the agent was started
  inside one), nor a hidden folder outside the working directory. Without `dir`,
  `backyard_deploy` publishes the current project like `backyard deploy` (its `.backyard.json`
  `dir`) and reports the folder it published; naming the root of a project whose `dir` points
  elsewhere is refused. `backyard_read_site_file` refuses binary file types without
  downloading them and returns at most 256 KiB of text, which is untrusted data. Tool results
  and errors never contain tokens or signed upload URLs.
* Hub serves `/llms.txt` and `/llms-full.txt`.
* Remote MCP server for Claude (the "custom connector"): `<public_url>/__backyard/mcp` with
  Backyard's own OAuth sign-in (§8.4) and the tools `backyard_docs`, `backyard_whoami`,
  `backyard_list_sites`, `backyard_site_files`, `backyard_read_site_file` and
  `backyard_publish` (§14.1).

### 14.1 Remote MCP endpoint

`POST /__backyard/mcp` on the canonical hub host (§8.4.1) is an MCP server over Streamable HTTP:
stateless, JSON responses only — no sessions, no SSE streams, no JSON-RPC batches. It supports
the protocol versions `2025-11-25` and `2025-06-18`. Both editions implement the JSON-RPC
handling themselves, so they answer identically.

**Gate.** Every request passes these steps in this order; the HTTP-level errors (steps 3–9) use
the §5 envelope unless noted:

1. The routing rules of §8.4.1 (canonical host; disabled → `404`).
2. Go only: until step 9 the connection keeps `server.read_timeout` and `write_timeout`, so a
   request refused before its body is read (whose unread body the server drains) is held no
   longer than any other.
3. **Origin.** Absent is fine. Present, it must equal (case-insensitively) the hub origin, the
   request's own origin, the origin of an https entry of the effective `mcp.redirect_uris` /
   `MCP_REDIRECT_URIS`, or the origin of an entry of `mcp.clients` / `MCP_CLIENTS` (by default
   `https://claude.ai` and `https://claude.com`); otherwise
   `403 {"error":{"code":"forbidden","message":"requests from this origin are not accepted"}}`.
4. **Credentials.** More than one `Authorization` header — also one value that joins several
   `<scheme> <credentials>` parts with commas, as proxies and the Workers runtime merge repeated
   headers — → `400` (`bad_request`) with
   `WWW-Authenticate: Bearer error="invalid_request", resource_metadata="<M>"`. No
   `Authorization: Bearer <token>` (the scheme is case-insensitive; other schemes count as
   absent) → `401` with exactly `WWW-Authenticate: Bearer resource_metadata="<M>", scope="mcp"`
   and `{"error":{"code":"unauthenticated","message":"connect this app to Backyard to sign in"}}`.
   An invalid token (§8.4.8) → `401` with exactly
   `WWW-Authenticate: Bearer error="invalid_token", error_description="<desc>", resource_metadata="<M>", scope="mcp"`
   and the same body, where `<desc>` is one of `malformed token` (wrong prefix, over 4096
   bytes, bad MAC), `token expired`, `token revoked` (the grant is missing, unredeemed or
   expired), `token not issued for this server` (the grant's resource is not the current R) and
   `access removed by policy` (re-gating failed; the grant is deleted). Only MCP access tokens
   are accepted: API tokens, static tokens, `DEPLOY_TOKEN`, IdP and Access JWTs are
   `malformed token`, and cookies and proxy identity headers are never read.
5. Go only: `api_per_minute` keyed by the user (§11) → `429 rate_limited` with `Retry-After`.
6. A method other than `POST` (`GET`, `DELETE`, `PUT`, `PATCH`, `OPTIONS`, `HEAD`) → `405` with
   `Allow: POST`.
7. A `Content-Type` whose media type is not `application/json` → `415` (`bad_request`). An
   `Accept` header that admits none of `application/json`, `application/*` and `*/*` → `400`
   (`bad_request`); no `Accept` is fine.
8. **`MCP-Protocol-Version`.** Absent means `2025-06-18` (§8.4.11). Any value other than
   `2025-11-25` and `2025-06-18` → `400`, `Content-Type: application/json`,
   `{"jsonrpc":"2.0","id":null,"error":{"code":-32022,"message":"Unsupported protocol version","data":{"supported":["2025-11-25","2025-06-18"],"requested":"<value>"}}}`.
9. **Body.** A `Content-Length` above the limit (§11) → `413` before reading; a body that
   exceeds it while reading → `413`; both
   `{"error":{"code":"too_large","message":"MCP requests are limited to <N> bytes; publish over several calls"}}`.
   Go only: a body over 1 MiB or of unknown length needs one of 4 admission slots — a user holds
   at most one, so their other large requests wait for it — waited for ≤ 30 s in all, else
   `429 rate_limited` with `Retry-After: 5`; then the write deadline becomes now + 5 min and the
   body is read at a minimum pace: byte n must arrive within 10 s + n / 65536 s (an average of
   64 KiB/s, never later than the 5 min), else `400 bad_request` (the connection's read deadline
   moves with the body, and becomes now + 5 min once it is read).
10. **JSON-RPC parsing** (`400`, `id: null`): invalid JSON — a body that is not valid UTF-8
    included — →
    `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`; a JSON array →
    `-32600` "Invalid Request: JSON-RPC batches are not supported"; not an object, `jsonrpc` ≠
    `"2.0"`, a `method` that is not a string, or an `id` that is neither a string nor an integer
    (`null`, fractional numbers and objects included) → `-32600` "Invalid Request". Member
    names are matched exactly (case-sensitively), and a string's unpaired surrogate escape
    (`"\ud800"`) decodes to U+FFFD (as Go's `encoding/json` does); an echoed string `id` keeps
    its surrogate escapes.
11. **Dispatch.** A notification (`method` without `id`) or a client response (no `method`, a
    `result` or `error`) → `202` with an empty body. A request → `200`,
    `Content-Type: application/json`, `Cache-Control: no-store`, one JSON-RPC response echoing
    `id`. No response carries `Mcp-Session-Id` or sets a cookie; an incoming `Mcp-Session-Id`
    or `Last-Event-ID` is ignored, and no request needs an earlier `initialize`. Methods:
    * `initialize`: `params.protocolVersion` must be a string (else `-32602` "Invalid
      params"); the result is
      `{"protocolVersion":"<the requested version if supported, else 2025-11-25>","capabilities":{"tools":{}},"serverInfo":{"name":"backyard","title":"Backyard","version":"<build version>"},"instructions":"<the catalog's instructions>"}`;
    * `ping` → `{}`;
    * `tools/list` → `{"tools": <the catalog's tools, verbatim>}` (`params.cursor` is ignored;
      there is no `nextCursor`);
    * `tools/call` → below;
    * any other method (`resources/list`, `prompts/list`, `logging/setLevel`,
      `completion/complete`, `server/discover`, …) → `-32601` "Method not found" (HTTP `200`);
    * an internal failure → `-32603` "Internal error (request <X-Request-Id>)".
12. Worker only: `tools/call` charges `LIMITS.mcp_calls_per_day` per user before the tool runs
    (§16); exhausted → HTTP `429` (§5 envelope, `Retry-After`). Only calls that name a tool of
    the catalog with valid `params` count (`-32602` answers and other methods are free; a
    schema violation of the arguments counts).

**The tool catalog** `internal/mcpremote/tools.json` is normative, as `backyard.d.ts` is for the
SDK: its `instructions` are the `initialize` instructions, and `tools/list` returns its `tools`
verbatim and in order (the Go server embeds the file; the Worker build generates
`src/generated/mcp.ts` from it). The tools: `backyard_docs` (Backyard's documentation, topic
`skill`, `api`, `recipes` or `deploy`), `backyard_whoami` (the account, whether it may publish,
the hub URL, the site URL pattern and the limits), `backyard_list_sites`, `backyard_site_files`,
`backyard_read_site_file` (read-only) and `backyard_publish` (destructive).

**`tools/call`.** `params` must be an object with a string `name` and an object `arguments` or
none (`{}`), else `-32602` "Invalid params"; an unknown tool → `-32602` "Unknown tool: <name>".
The arguments are validated against the tool's `inputSchema` (the keywords the catalog uses);
a violation is a tool error `Invalid arguments: <detail>`. A success is
`{"content":[{"type":"text","text":<T>}]}` where `T` is the Markdown document
(`backyard_docs`), the file's text (`backyard_read_site_file`, invalid UTF-8 replaced by
U+FFFD) or the compact JSON of the result object; there is no `structuredContent`. A tool
failure is `{"content":[{"type":"text","text":<message>}],"isError":true}` with a message of the
table below. Results and errors never contain tokens, codes, registered `client_id`s, signed
URLs or the server secret.

* `backyard_whoami` →
  `{"user":{"id","email"?,"name","admin","anonymous"},"hub_url":"<P>","site_url_template":"<the site URL of §2 with {site} for the name>","can_publish":<bool>,"limits":{"max_publish_bytes","max_files","max_file_bytes","max_site_bytes","max_read_bytes":131072},"connection":{"client":"<client_name>","expires_at":"<RFC3339 grant expiry>"}}`.
  `can_publish` is true when the user is not anonymous or anonymous deploys are allowed (and,
  on Go, the embedded gateway is on). On the Worker `max_publish_bytes` is
  `LIMITS.mcp_publish_bytes`.
* `backyard_list_sites` (`query`: case-insensitive substring of name, title or description) →
  `{"sites":[{"name","url","title"?,"description"?,"updated_at"?,"updated_by"?,"file_count","bytes","owners"?}],"total":<matches>,"truncated":<bool>}`,
  most recently updated first, then by name; `owners` only with ownership `owner`.
* `backyard_site_files` → `{"site","url","files":[{"path","size"}],"total","truncated"}`, what
  is served, sorted by path.
* `backyard_read_site_file` checks, in order: the site exists (E22), the path is valid (E8), the
  extension is not a binary type (E24), the file exists (E23), its size is ≤ 131072 bytes (E25),
  and no NUL byte in its first 8 KiB (E24).

**`backyard_publish`** (`site`, `files: [{path, content, encoding?: "utf-8"|"base64",
content_type?}]`, `delete?: [paths]`, `replace?`, `overwrite?`) merges by default: the listed
files are written, the site's other files are kept, `delete` removes paths, and `replace: true`
makes the site exactly the given files. The steps, normative on both editions:

1. Go: `deploy.embedded_gateway: false` → E18. An anonymous user without anonymous deploys →
   E17.
2. The site name (E2) and the reserved names (E3): the reserved list of CLI deploys — on Go the
   effective `sites.reserved` (§9: its defaults unless the key is set), on the Worker the
   defaults plus `RESERVED_SITES`.
3. `files` and `delete` both empty → E4; `replace` with a non-empty `delete` → E5; `replace`
   with empty `files` → E6.
4. Per file: a valid path (E8, the rules of §7.3), no duplicates (E9), not also in `delete`
   (E10), the content decoded — `utf-8` (default) is the string's UTF-8 bytes, `base64` takes
   the standard or URL alphabet with optional padding and no whitespace (else E7) — at most
   `max_file_bytes` (E12), and a `content_type` of ≤ 255 characters without control characters
   (else E1; default: by extension). The decoded total must stay within the per-call cap (E11),
   and the `delete` paths must be valid (E8).
5. Concurrency: Go takes a publish lease — `Create` of `_system/_mcp_locks/<site>` with
   `exp` = now + 300 s and a random `holder`. On a conflict a live lease → E20. An expired one
   is taken over only by the taker that first claims the state it read: an atomic `Create` of
   the takeover claim `_system/_mcp_lock_takeovers/<site>.<version>.<created_at in µs>.<k>`
   (`{"exp"}` of the new lease; k = 1, or the next k while claim k has expired, its claimant
   having died before it wrote; a live claim → E20 with nothing written), then an `Update` of the
   lease to the new fields, which must be the very next write to the document as read (same
   `created_at`, `version` + 1), else E20 (a lease gone in between → the `Create` is retried
   once). No taker deletes or rewrites a lease another one holds; expired claims are pruned at
   takeovers. When the publish ends the lease is deleted if it still names its `holder` and
   expires no sooner than 30 s from now (a lease left behind expires by itself). The Worker
   needs none (step 9 is one transaction). CLI and hub deploys take no lease and keep the
   semantics of §7.3.
6. Ownership (`sites.ownership: owner` / `OWNERSHIP=owner`): the caller must be an owner or an
   admin (E15); guests are never owners; the first publisher of a new site becomes its owner
   unless anonymous.
7. **Overwrite guard:** when the site exists and its `updated_by` is non-empty and equals
   (case-insensitively) neither the user's id, email nor actor name, and `overwrite` is not
   true → E16; nothing is changed and no quota is charged.
8. Quotas: Go `deploys_per_hour` keyed `user:<id>` (guests included) → E19; Worker the site cap
   for a new site (E27) and `deploys_per_day` keyed `user:<id>` → E19.
9. The result set F: merge F = (current files − `delete` − the paths in `files`) ∪ `files`
   (deleting a path that does not exist is a warning); replace F = `files`. F empty → E6; more
   than `max_files` files or more than `max_site_bytes` bytes → E13; a path that is both a file
   and a folder of another → E14. A file whose content is unchanged is not written again.
   Kept files keep their manifest entries (Go adopts objects stored out of band, as a refresh
   does). The manifest and SiteMeta (`updated_by`, `deploy_id`, title and description from
   `index.html`, owners on first publish) are written before anything is deleted; the Worker
   switches the file list in one transaction (`deployMerge`, §16).
10. Audit `deploy.commit` with `via=mcp`, `client` and `grant` (§15); metrics
    `deploys_total{method="mcp"}`.

The result: `{"site","url","created","files","uploaded","unchanged","deleted","bytes","warnings":[…]}`
— `files` and `bytes` describe the site afterwards, `uploaded` counts written files,
`unchanged` listed files that were not rewritten, `deleted` removed paths. Warnings:
`No index.html at the site root: <url> answers 404 until one is published.` when F has no
`index.html`; `<path> was sent as text but looks binary; binary files need "encoding":
"base64".`; `"<path>" in delete does not exist.`.

**Tool errors** (both editions use these templates; sizes formatted like `1.2 MiB`, times in
UTC; conformance matches only the fragments `reserved` (E3), `base64` (E7), `per call` (E11),
`overwrite` (E16) and `binary` (E24)):

| Id | Text |
|---|---|
| E1 | `Invalid arguments: <detail>` |
| E2 | `"<site>" is not a valid site name: use lowercase letters, digits and '-' (at most 63 characters), e.g. "lunch-poll".` |
| E3 | `"<site>" is reserved on this Backyard; choose another name.` |
| E4 | `Nothing to publish: give files, delete, or both.` |
| E5 | `Use either "replace" or "delete", not both.` |
| E6 | `A site needs at least one file.` |
| E7 | `files[<i>] (<path>): "content" is not valid base64.` |
| E8 | `"<path>" is not an allowed path: <reason>.` |
| E9 | `"<path>" appears more than once.` |
| E10 | `"<path>" is both in files and in delete.` |
| E11 | `This call carries <X> of file content; the limit is <Y> per call. Send the files over several calls (files you do not list are kept).` |
| E12 | `<path> is <X>; files may be at most <Y>.` |
| E13 | `The site would have <n> files; this Backyard allows <max> per site.` or `The site would be <X>; this Backyard allows <Y> per site.` |
| E14 | `These paths would be both a file and a folder: <list>.` |
| E15 | `<site> belongs to <owners>; you are not an owner. Ask an owner to add you, or publish under another name.` |
| E16 | `<site> was last published by <who> on <YYYY-MM-DD HH:MM> UTC. Nothing was changed. Ask the user whether to replace it; if so, call again with "overwrite": true, otherwise choose another name.` |
| E17 | `This Backyard has no sign-in and does not accept anonymous publishing, so this connection can only read. Ask the Backyard admin to set up sign-in or allow anonymous deploys.` |
| E18 | `Publishing through this Backyard's hub is turned off (deploy.embedded_gateway: false).` |
| E19 | `Publishing limit reached (<n> per <hour on Go, day on the Worker>). Try again in <m> minutes.` |
| E20 | `Another publish to <site> is in progress; call backyard_publish again when it has finished.` |
| E21 | `Publishing failed: Backyard could not store the files (request <id>). Publishing the same files again completes it.` |
| E22 | `There is no site named "<site>" on this Backyard.` |
| E23 | `<site> has no file "<path>".` |
| E24 | `<path> is a binary file and cannot be read as text; link to it by URL instead.` |
| E25 | `<path> is <X>; files up to 128 KiB can be read.` |
| E27 | `This Backyard already has its maximum number of sites (<n>).` |

The ids are stable labels (E26 is unused). Internal failures of `backyard_publish` are E21; those
of the read tools are `Internal error (request <id>)`. The limits are in §11.

## 15. Observability

`log/slog` JSON (or text) logs; `telemetry.access_log` adds one line per request (with
`request_id`, the `X-Request-Id` of every response, §5). Audit events (logins, CLI and API
token issuance, deploy create/commit, refresh, site deletion, owner changes) are logged with
`"audit": true`; the server also stores API token issuance records in `_system/_audit` (best
effort). The Claude connector (§8.4) adds these audit events, each with the fields `user`,
`client_name`, `client_kind`, `client_host`, `redirect_host`, `grant` (the first 8 hex
characters of the grant id) and `ip` where available, and never a token:

| Event | When | Go | Worker |
|---|---|---|---|
| `mcp.authorize` | consent allowed | log + stored | log |
| `mcp.deny` | consent denied | log | log |
| `mcp.consent_rejected` | a consent `POST` failed its CSRF or browser-signal checks | log (warn) | log (warn) |
| `mcp.token` | a code was exchanged | log | log |
| `mcp.code_replay` | a code was redeemed a second time (after PKCE verified) | log (warn) + stored | log (warn) |
| `mcp.refresh_reuse` | an old refresh token generation was presented | log (warn) + stored | log (warn) |
| `mcp.revoke` | any revocation; `via` = `rfc7009`, `hub`, `policy` or `replay`, `count` for bulk | log + stored | log |
| `deploy.commit` | a `backyard_publish` (§14.1) | the gateway's audit with `via=mcp`, `client`, `grant` | the same fields |

Stored records (`_system/_audit`, shape `{action, user, at, ip?, …fields}`) thus include API
token issuance, `mcp.authorize`, `mcp.code_replay`, `mcp.refresh_reuse` and `mcp.revoke`.
Prometheus metrics (prefix `backyard_`): `http_requests_total{area,code}`,
`http_request_duration_seconds{area}` (areas include `mcp` for `/__backyard/mcp` and `oauth`
for `/.well-known/oauth-*` and `/__backyard/oauth/*`), `deploys_total{method=api|sync|mcp,result}`,
`db_ops_total{op,result}`, `ai_requests_total{model,result}`, `ai_tokens_total{model,direction}`,
`ws_connections`, `ws_messages_total{dir}`, `warehouse_queries_total{source,result}`,
`rate_limited_total{category}`, `build_info{version}`, plus the Go runtime and process
collectors. They are served at `/__backyard/metrics` on the hub (authenticated) or, with
`telemetry.metrics_listen`, at `/metrics` on that separate listener (no authentication).

## 16. Cloudflare edition (free tier)

A TypeScript Cloudflare Worker in `deploy/cloudflare/` implements the **same public contract**
(§2–§7 URLs, site API, websocket protocol, deploy gateway protocol, SDK preamble) so that the same
`backyard.js`, CLI, skill and MCP server work unchanged. Inspired by OpenQuick
(github.com/topmass/openquick). Runs on the Workers **free plan** (Durable Objects with SQLite
storage are available on free); R2 and paid features are optional.

* **Topology:** one Worker (router, auth, SDK/hub/docs serving, deploy gateway) + one
  `SiteDO` Durable Object per site (SQLite: site files, manifest, documents, uploads, websocket
  hub with hibernation) + one `RegistryDO` (site list/metadata, platform-wide counters/quotas).
* **URLs:** default path mode on `https://<worker>.<account>.workers.dev/s/<site>/`; a custom
  domain in path mode (a Workers custom domain); or host mode = the hub's custom domain + a
  proxied `*.<base_domain>` record + the route `*.<base_domain>/*` (Universal SSL covers one
  wildcard level; deeper needs Advanced Certificate Manager). Configured via `vars`
  (`BASE_DOMAIN`, `URL_MODE`), which `backyard cloudflare domain add` sets.
* **Storage:** files ≤ `FILE_INLINE_MAX` (default 5 MiB) stored chunked in DO SQLite (chunks <
  2 MB); larger files go to the optional `R2` binding (`sites/<site>/…`, `uploads/<site>/…`);
  the per-file cap is 25 MiB without R2 and 95 MiB with it (100 MB request limit). Deploy uploads
  use self-hosted upload URLs (§7.3), verified (size + SHA-256) before anything is stored. Site
  files are content-addressed: the commit switches the site's whole file list in one
  transaction (so deploys are atomic on this edition), and content uploaded by deploys that
  never commit is garbage-collected by a later commit once it is an hour old.
* **Database:** documents in the site DO's SQLite with the exact query semantics of §5.2
  (shared conformance tests). A query page travels from the DO to the Worker as one RPC result,
  so it carries at most about 8 MiB of stored JSON: a page cut short holds fewer than `limit`
  documents (always at least one) and says `has_more: true`, and clients advance `offset` by
  the number of documents received (§5.2). Realtime is local to the site DO (single instance
  per site); `db` frames always carry the document.
* **AI:** `workers-ai` provider via the `AI` binding (free daily allowance) with configurable
  aliases (`AI_MODELS` JSON var); optional `anthropic`/`openai`(-compatible) providers via
  secrets (optionally through Cloudflare AI Gateway). Image generation via Workers AI image
  models. Embeddings via Workers AI. Smaller per-request limits than the Go server (§5.4): at
  most 4 images per `/ai/image` request and 100 inputs per `/ai/embed` request. Chat images
  follow §5.4 exactly (the site's own uploads are read from the DO or R2; 20 MiB per image,
  64 MiB per request).
* **Warehouse:** not available on Cloudflare (`501 unsupported`) unless `WAREHOUSE_URL` points
  to a Backyard Go server's warehouse API (proxy) — optional.
* **Auth:** `AUTH_MODE` = `none` (anonymous cookie identity; deploys need `DEPLOY_TOKEN` unless
  `ALLOW_ANONYMOUS_DEPLOY`) or `access` (Cloudflare Access: verify `Cf-Access-Jwt-Assertion`
  against `https://<team>.cloudflareaccess.com/cdn-cgi/access/certs` with `ACCESS_AUD`; Okta,
  Entra ID, Google etc. are configured as Access identity providers). Any other `AUTH_MODE`
  (a typo, or a Go mode such as `oidc`) **fails closed**: every request except
  `/__backyard/healthz` answers `503`, and `/__backyard/readyz` reports the problem, because
  falling back to `none` would make public a Worker meant to sit behind Access. Backyard API
  tokens (§8.3, HS256 with `BACKYARD_SECRET`), the `/__backyard/auth/cli` browser flow with its
  cross-site confirmation (`GET` or `POST`, CSRF key `backyard/cli-confirm/v1`, bound to the
  user), the cap that keeps a bearer-minted token from outliving the presenting token, and the
  conflicting-cookie rule (§8.3) work identically; the rule also covers Access's own
  `CF_Authorization` cookie (valid cookies, or cookie and header, naming different users →
  `401`). Unlike the Go server's proxy modes (§8.2), `/__backyard/auth/logout` (`GET`/`POST`)
  exists in both modes: it clears the guest cookie (`none`) or redirects to
  `/cdn-cgi/access/logout` (`access`).
  Access must bypass the nine paths the Worker authenticates itself — `/__backyard/api/deploy`
  and `/__backyard/api/deploy/*` (bearer tokens, signed upload URLs, the public `info`),
  `/__backyard/api/tokens` (`backyard token create` sends an API token), the health checks
  `/__backyard/healthz` and `/__backyard/readyz`, and the Claude connector's machine paths
  (§8.4.10) `/__backyard/mcp`, `/__backyard/oauth/*`, `/.well-known/oauth-authorization-server`
  and `/.well-known/oauth-protected-resource/__backyard/mcp`, which are bypassed whether or not
  the connector is enabled — with a Bypass policy (or service tokens);
  `backyard cloudflare access enable` creates or verifies this (or the optional Terraform module
  in `deploy/terraform/cloudflare`). In host mode the bypass names only the hub host, so the
  sites' own `/.well-known` files stay behind Access. `/__backyard/auth/authorize` stays behind
  the main Access application.
* **Claude connector** (§8.4, §14.1), off unless `MCP_ENABLED` is true (`true/1/yes/on`).
  `MCP_CLIENTS` (default the two Claude client metadata documents; set but empty: no CIMD, so
  clients register) and `MCP_REDIRECT_URIS` (default the two Claude callbacks; set but empty:
  loopback redirects only) are comma or whitespace separated and follow the entry rules of §12
  (using `BASE_DOMAIN` and the `PUBLIC_URL` host). An invalid entry does not make the Worker
  fail closed: it turns the connector off (every connector path `404`) and adds a problem line
  to `/__backyard/readyz`; the connector is also off unless the hub origin is https or a
  loopback host. Grants live in `RegistryDO` tables (`mcp_grants`: `id`, `user_id`, `data`,
  `created_at`, `code_exp`, `redeemed_at`, `exp`; `mcp_rotations`: `gid`, `n`, `at`, `exp`,
  primary key (`gid`, `n`), the newest two generations per grant), both indexed by `exp`, and
  `rotate` (§8.4.8) runs in one transaction; each consent prunes up to 500 expired rows of each. `backyard_publish`
  is atomic per site: `SiteDO.deployMerge` computes the merged file set, checks the limits and
  switches the manifest in one transaction, so concurrent calls never lose each other's files;
  a call that the site limits refuse (E6, E13, E14) for the site as it is uploads nothing.
  It is capped at `LIMITS.mcp_publish_bytes` (default 2097152) decoded bytes per call, and every
  `tools/call` counts against `LIMITS.mcp_calls_per_day` (default 5000 per user; `429` when
  exhausted). On the Workers Free plan's CPU limit large publish calls (base64 decoding and
  hashing) may fail; lower `mcp_publish_bytes` there if they do.
* **Guardrails (free tier):** per-user/IP daily caps for AI calls, generated images, deploys,
  uploads and document writes, platform-wide daily AI caps, and a platform-wide site cap
  (reserved atomically, so parallel first deploys cannot exceed it), stored in `RegistryDO`;
  configurable via the `LIMITS` JSON var (which also holds the connector's `mcp_publish_bytes`
  and `mcp_calls_per_day`). The image caps count images: an `/ai/image` request
  with `n: 4` uses 4, and is refused (`429`) without running the model when fewer are left.
* **Deploying the platform:** the npm package `backyard-cloudflare` holds a prebuilt Worker and
  its engine (Node.js 22+, wrangler pinned): `npx backyard-cloudflare setup` (wrangler login,
  workers.dev subdomain, secrets, optional R2 bucket, deploy, prints the `backyard login`
  command); `backyard cloudflare setup` installs the package of its own release (SHA-256
  checked) and logs the CLI in; `npm run setup` in a checkout of `deploy/cloudflare` builds from
  source. `domain add <host> [--host-mode]` and `access enable` add the custom domain and
  Cloudflare Access; deploys (setup and the CI deployer) never change routes or custom domains,
  and refuse to turn a live Access off unless confirmed. `/info` (§7.1) reports the build
  version (the release version, `<package version>-dev` for checkout builds). GitHub Actions
  example included.

## 17. Cross-edition API conformance

`conformance/` is a black-box HTTP + websocket test-suite (Node 22, `node --test`, no
dependencies beyond `ws` if needed) run against a base URL (`BACKYARD_URL`, `BACKYARD_TOKEN`). It
deploys a test site via the gateway protocol and exercises §5–§7. CI runs it against the Go server
(`backyard serve --dev`) and the Worker (`wrangler dev`).

`tests/mcp.test.mjs` covers the Claude connector (§8.4, §14.1) without a browser: discovery,
registration, the authorize and consent checks, the token, refresh and revocation endpoints, the
MCP gate, the tools and the grants API. It is skipped when `/info` has no `mcp_url`, unless
`BACKYARD_CONFORMANCE_MCP=1` makes that a failure. Both runners enable the connector in every
pass and set that variable. The Go runner adds an `oidc` pass: the server in `auth.mode: oidc`
against a mock OpenID Connect provider (`conformance/mock/idp.mjs`), which signs two people in
without a page, running only `tests/mcp.test.mjs`.
