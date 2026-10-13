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

[0.1.0]: https://github.com/thasami/backyard/commits/main
