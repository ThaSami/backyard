# Changelog

All notable changes to Backyard are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [0.1.0] - Unreleased

The first public version of Backyard: drop a folder of HTML, get an internal URL with a
zero-config backend for every site.

### Added

#### Platform

- One Go binary (`backyard`) that is the server, the deploy CLI, a local dev server and an MCP
  server; container image `ghcr.io/thasami/backyard` (distroless, non-root).
- Host-mode (`<site>.<base_domain>`) and path-mode (`/s/<site>/`) routing; static serving with
  clean URLs, `200.html` SPA fallback, custom `404.html`, ETags, Range requests and a small
  object cache.
- Hub: site directory and search, drag-and-drop deploys, CLI and agent setup, `/docs`,
  `/llms.txt`, `/llms-full.txt`.
- Design specification (`docs/spec/SPEC.md`) covering every URL, wire format and default.

#### Client API (`/__backyard/backyard.js`, `.mjs`, `.d.ts`)

- `backyard.db`: per-site JSON documents with type-strict queries (`==`, `!=`, `<`, `<=`, `>`,
  `>=`, `in`, `not-in`, `contains`), change feeds and live queries.
- `backyard.files`: uploads with Range-capable serving; active content sandboxed.
- `backyard.ai`: chat, streaming (SSE), image generation and embeddings through server-side
  provider aliases.
- `backyard.warehouse`: read-only named queries and (optionally) raw SQL.
- `backyard.socket`: websocket rooms with presence and direct messages; one auto-reconnecting
  socket per page.
- `backyard.id`: the signed-in user and colleague lookup.

#### Backends

- Storage: local filesystem (rsync target, no bucket needed), Amazon S3 and S3-compatible
  stores, Google Cloud Storage, Azure Blob Storage.
- Database: SQLite, PostgreSQL, DynamoDB, in-memory — one shared conformance suite.
- Realtime fan-out: in-memory, PostgreSQL LISTEN/NOTIFY, Redis/Valkey.
- AI: Anthropic (first-party, Vertex AI, Bedrock), OpenAI and compatible servers, Ollama,
  Azure OpenAI, Amazon Bedrock (Converse), Google Gemini (AI Studio, Vertex AI).
- Warehouse: BigQuery, PostgreSQL-compatible (Redshift…), Athena.

#### Authentication

- Modes `oidc` (Okta, Entra ID, Google, Auth0, Keycloak…), `iap`, `alb`, `header`
  (oauth2-proxy, Pomerium, Cloudflare Access, Azure Easy Auth) and `none`.
- Personal API tokens (browser → CLI loopback login, `backyard token create`), hashed static
  tokens for CI, OAuth device flow; per-purpose keys derived from `server.secret` with HKDF.
- Admins, admin groups, allowed domains and groups, optional site ownership.

#### Deploying

- Diffed deploy gateway protocol with presigned uploads, embedded in the server, standalone
  (`backyard gateway`) or as an AWS Lambda function.
- Direct-sync methods: `aws s3 sync`, `gcloud storage rsync`/`gsutil rsync`,
  `az storage blob sync`, `rsync`; `.backyardignore` and `.backyard.json`.

#### Operations

- Rate limits and quotas, audit log events, Prometheus metrics, health and readiness checks,
  graceful shutdown, horizontal scaling with Postgres/DynamoDB and Redis/Postgres pub/sub.
- Deployment kits: Terraform for AWS, Google Cloud, Azure and Cloudflare; a Helm chart; Docker
  Compose and systemd for a single VM without a bucket.

#### Cloudflare edition

- A TypeScript Worker with Durable Objects that implements the same public contract on
  Cloudflare's free plan, with Workers AI, optional R2 and Cloudflare Access;
  `backyard cloudflare setup` provisions it.

#### Agents

- The Backyard Claude skill (API reference, recipes, deploy guide), a Claude Code plugin with
  `/backyard:new`, `/backyard:deploy` and `/backyard:docs`, the `backyard mcp` server, and
  `backyard init` templates for Claude, Codex, Cursor and Copilot.

#### Testing

- A cross-edition API conformance suite (`conformance/`, Node 22, no dependencies) covering the
  deploy gateway protocol, static serving and the SDK, the site API and the websocket protocol;
  the Go server and the Cloudflare Worker must both pass it, in host and in path mode
  (`conformance/run-go.sh`, `conformance/run-worker.sh`, `make conformance`).
- Real-browser end-to-end tests (`e2e/`, Playwright and Chromium) of the hub, the docs, every
  example, the `backyard init` templates, the SDK and the sign-in flows, run offline against a
  fake OpenAI-compatible AI upstream and a mock OpenID Connect provider, with axe-core WCAG 2.1
  AA checks in light and dark mode (`make e2e`).
- `go test ./docs/` keeps the documentation honest: links and anchors, `make` targets,
  configuration keys and specification section references.

### Changed

- AI provider failures map to the same statuses on both editions: timeouts → `504`, provider
  throttling → `429` with `Retry-After` when the provider says when to retry (else `503`),
  rejected requests → `400` (`413` when too large), everything else → `502`; provider messages
  are sanitized.
- `/__backyard/auth/login?rd=…` redirects to `rd` in the modes where a proxy signs people in
  (`none`, `iap`, `alb`, `header`), as on the Cloudflare edition, instead of answering `404`.
- `auth.mode: oidc` in host mode is rejected at startup when `server.base_domain` cannot carry
  a cookie (`localhost`, IP addresses, single-label names) or `server.public_url` is outside
  it, instead of sending every site visit through the IdP in a loop.
- PostgreSQL pub/sub opens a small publishing pool (2–4 connections, by CPU count) plus its
  LISTEN connection; a `pool_max_conns` DSN parameter overrides the pool size.
- On Cloud Run (`K_SERVICE`), site files and uploads over 32 MiB are streamed without a
  `Content-Length` and range responses are capped at 32 MiB, which Cloud Run's front end
  requires.
- AI chat accepts the `url` of one of the site's own uploads (what `backyard.files.upload` and
  `backyard.ai.image` return, site-relative or on the site's origin) as an image: both editions
  read the file from storage — never fetching it — and detect its type from the bytes (png,
  jpeg, gif, webp). Images loaded from URLs share a 64 MiB budget per request; another site's
  file or any other site-relative URL is a `400`.

### Fixed

- Accessibility of the hub, the docs page and the examples (WCAG 2.1 AA, light and dark mode).
- Site URLs in host mode are `<scheme>://<site>.<base_domain>[:<port>]/` everywhere — in the
  CLI (`deploy`, `sites`), the MCP server, the hub and the `backyard init` agent files — also
  when the host of `server.public_url` differs from `server.base_domain`; they used to be
  derived from the public URL's host.
- Cloudflare edition: every response carries `X-Request-Id` (a valid incoming id is kept, as on
  the Go server); an encoded slash (`%2F`) in an API path parameter stays inside that parameter
  instead of splitting the path; chat image URLs are validated like on the Go server (absolute
  http(s) URLs of bounded length); warehouse proxy failures get the Go server's generic,
  request-id-naming messages.

### Security

- Deploy uploads are bound to the size and SHA-256 the plan declared wherever the store can
  enforce it: S3 URLs sign the size (`Content-Length`, or `Content-MD5` for empty files) and,
  on AWS, the SHA-256 (`x-amz-checksum-sha256`), GCS URLs pin the size
  (`x-goog-content-length-range`), the gateway's own upload endpoint verifies both. A commit
  deletes listed objects of the wrong size and fails with `409 conflict` naming the paths, so
  no unverified object outlives it (Azure SAS URLs cannot restrict the body).
- A CLI login that another site may have started (`/__backyard/auth/cli`, also with
  `mode=display`) asks the user for confirmation before a token is minted, on both editions:
  Backyard's own page posts back a CSRF token bound to the user and the browser session.
- `backyard mcp`: `backyard_init` and `backyard_deploy` only use folders inside the working
  directory, or an existing directory the agent names by its absolute path, and never the
  filesystem root, the home directory or folders holding credentials; `backyard_read_site_file`
  refuses binary file types without downloading them and tells agents that file content is
  untrusted data; tool results and errors never contain tokens or signed upload URLs.
- Warehouse engine and connection failures return a generic message naming the request id
  (the details go to the server log), since engine errors can reveal host, user and database
  names.
- Further hardening from an adversarial review of both editions: stricter input validation,
  safer defaults and narrower error messages.

[0.1.0]: https://github.com/thasami/backyard/commits/main
