# Changelog

All notable changes to Backyard are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `auth.allowed_emails`: admit exact (verified) email addresses. An address passes when its
  domain is in `auth.allowed_domains` or it is listed here; on its own the list admits only the
  named people — the way to restrict a sign-in anyone can complete, such as Google with personal
  `@gmail.com` accounts. The Terraform modules (AWS, GCP, Azure) take it as `allowed_emails`.
- **Claude connector** (remote MCP, both editions, off by default): the hub serves an MCP
  server at `<public_url>/__backyard/mcp` that people add to Claude (web, Desktop, mobile,
  Cowork, Claude Code) as a custom connector. They sign in with the company login they use for
  Backyard and approve Backyard's consent page; Claude can then find sites, read their files
  and publish sites as them, with no CLI or token. Backyard is the OAuth 2.1 authorization
  server (SPEC §8.4): Claude's published client identity (an allowlisted metadata document) or
  stateless registration, exact redirect URIs, PKCE `S256`, single-use codes, rotating refresh
  tokens and tokens valid only at `/__backyard/mcp`; your IdP needs no change. Tools
  (SPEC §14.1): `backyard_docs`, `backyard_whoami`, `backyard_list_sites`,
  `backyard_site_files`, `backyard_read_site_file` and `backyard_publish`, which merges by
  default and carries at most 4 MiB of file content per call on the Go server (2 MiB on
  Cloudflare; bigger sites go over several calls, big media through the hub or the CLI).
  Publishing as a person needs a sign-in (Go `oidc`, `iap`, `alb`, `header`; Cloudflare
  Access); without one the connector is a guest and publishes only with anonymous deploys.
  Guide: [docs/claude-connector.md](docs/claude-connector.md).
- Configuration `mcp.enabled`, `mcp.clients`, `mcp.redirect_uris` and `mcp.max_publish_bytes`;
  on the Cloudflare edition `MCP_ENABLED`, `MCP_CLIENTS`, `MCP_REDIRECT_URIS` and the `LIMITS`
  keys `mcp_publish_bytes` and `mcp_calls_per_day`.
- Hub card **Use Backyard in Claude** with the connector URL, setup steps and "Your
  connections" (Disconnect; admins can disconnect a person), backed by
  `GET`/`DELETE /__backyard/api/mcp/grants`; `/__backyard/api/info` reports `mcp_url`.
- Audit events `mcp.authorize`, `mcp.deny`, `mcp.consent_rejected`, `mcp.token`,
  `mcp.code_replay`, `mcp.refresh_reuse`, `mcp.revoke`; `deploy.commit` with `via=mcp`; HTTP
  metric areas `mcp` and `oauth` and `backyard_deploys_total{method="mcp"}`.
- Terraform: the AWS module's ALB rule `mcp` (priority 15, `auth_mode = "alb"`) and the GCP
  module's four IAP-less routes exempt the connector's machine paths; `config_overrides` (AWS;
  not passed to the Lambda gateway) and `extra_config_yaml` (GCP, Azure) accept an `mcp`
  section; the Cloudflare module has
  `mcp_enabled`, which adds `--var MCP_ENABLED:true` to `setup_args`.
- `backyard cloudflare status` checks `access-connector`: the connector's paths in the Access
  bypass application (pending before `MCP_ENABLED`, failed after).

### Changed

- The OIDC login denial is logged as `auth: login denied by allowed_domains/allowed_emails/allowed_groups`
  (was `…allowed_domains/allowed_groups`).
- A chat image URL that the SSRF guard blocks now fails with `netguard: address not allowed`
  (was `imagefetch: address not allowed`): the guard is shared with the Claude connector's
  client metadata fetches.
- The Cloudflare Access bypass list (`backyard cloudflare access enable`, the Terraform module's
  `bypass_paths`) grows from five to nine paths: `/__backyard/mcp`, `/__backyard/oauth/*`,
  `/.well-known/oauth-authorization-server` and
  `/.well-known/oauth-protected-resource/__backyard/mcp` reach the Worker without an Access
  login (it answers `404` there until `MCP_ENABLED`). `access enable` verifies them at the edge
  too, so applications made in the dashboard from the earlier steps need the four paths added.
  `bypass_paths` now also accepts the two OAuth discovery documents. **Upgrade:** re-run
  `backyard cloudflare access enable` (or `terraform apply`) once.
- The AWS and GCP modules exempt the connector's four machine paths from the ALB login and IAP
  whether or not the connector is on (Backyard answers `404` there until `mcp.enabled`).
  **Upgrade:** `terraform apply`.

## [0.1.0] - 2026-10-14

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
- Terraform-free setup: the npm package `backyard-cloudflare` ships a prebuilt Worker with a
  pinned wrangler (`npx backyard-cloudflare setup`; a checkout runs it with `npm run setup` and
  `npm run cf -- <command>`), registers the account's workers.dev subdomain from the terminal,
  and adds `domain add <host> [--host-mode]` / `domain remove` (Workers custom domain, wildcard
  route and DNS record, automatic or guided), `access enable` / `access disable` (the two Access
  applications, created with `BACKYARD_CF_API_TOKEN` or from printed dashboard steps, verified
  at the edge before the Worker switches) and a read-only `status` (`--json`). Every command
  and flag is in `deploy/cloudflare/scripts/lib/commands.json`; exit codes 3 (refused, nothing
  changed) and 4 (applied, a check pending) are shared with the Go CLI.
- `backyard cloudflare setup|domain|access|status` runs the package of its own release,
  installed once into the user cache and checked against the SHA-256 built into the CLI (or a
  checkout's `deploy/cloudflare`, or `--package <file or https URL>`), and moves the CLI's
  profiles when the Worker's URL changes. Releases attach `backyard-cloudflare-<version>.tgz`
  (listed in `checksums.txt`) and can publish it to npm (`vars.BACKYARD_NPM_PUBLISH`);
  `make cloudflare-pack` builds it locally. A release build uses a checkout only with `--dir`
  (a build from source finds one from the current directory, never outside its repository);
  `npm ci` is skipped while `node_modules` is newer than `package-lock.json`, and a new engine
  install removes older ones from the cache, keeping the previous one and the release's.
- After `setup` and `domain add`/`remove` the CLI logs itself in with the deploy token, and
  `setup` prints the next step (or the `backyard login` command when it cannot log in). A
  setup re-run or a domain change keeps a current profile for another server current; while
  Access is on, a personal login (browser, device or `--token`) is never replaced by the deploy
  token, and `--profile` is honoured.
- `setup` exits 4 (applied, not verified) when the hub does not become ready, with the Worker's
  own reason; a failed `r2 bucket create` never unbinds a bucket the Worker uses; an interrupted
  deploy leaves no secrets file behind. `access enable` keeps what was added to its Access
  applications in the dashboard (require and exclude rules, other policies, identity
  providers), and a re-run with new `--allow-*` lists needs an API token
  (`BACKYARD_CF_API_TOKEN`, or pasted when asked; exit 2, nothing changed, without one). Behind an HTTP proxy the engine warns until
  `NODE_USE_ENV_PROXY=1` (Node 22.21 or later) makes its own API calls use it.
- Deploys never change routes or custom domains, and an auth guard refuses a deploy that would
  turn a live Cloudflare Access off, change its team or run against an unreadable live state
  (`setup --confirm-auth-change`; in `scripts/cloudflare-ci.mjs`
  `BACKYARD_CF_CONFIRM_AUTH_CHANGE=true`, which `cloudflare-deploy.yml` sets only for a manual
  run with its `confirm_auth_change` input checked). A machine without saved state adopts the
  deployed configuration (R2 bucket, variables, domain, Access).
- `/__backyard/api/deploy/info` reports the Worker's real build version.

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
  configuration keys and specification section references, and contract checks that tie
  statements of the specification and the guides to the code of both editions (key labels,
  site URL examples, limits, MCP tools, passive upload types, direct-sync command lines, the
  ignore rules, the login state cookie, the Cloudflare page budget and Access bypass paths).

### Changed

- AI provider failures map to the same statuses on both editions: timeouts → `504`, provider
  throttling → `429` with `Retry-After` when the provider says when to retry (else `503`),
  rejected requests → `400` (`413` when too large), everything else → `502`; provider messages
  are sanitized. The Go providers now also take the retry time from the upstream
  `Retry-After` (and `retry-after-ms`) headers, not only from the error message.
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
- Deploy ignore rules have two tiers. `.git/`, `.backyard.json` and `.backyardignore` are never
  uploaded, in any letter case and whatever the configuration (see Security). The other
  defaults (`node_modules/`, agent files, dotfiles…) can be re-included on purpose with `!`
  lines in `.backyardignore` or the `.backyard.json` `ignore` list, e.g. `!.htaccess`.
- Direct-sync deploys apply exactly the ignore rules of the `api` method: the excludes are
  computed by walking the folder (the `aws s3 sync` filters used to change anchored and
  re-included patterns), a symlinked deploy folder is resolved first (its excludes used to be
  dropped), and `aws s3 sync --no-follow-symlinks` and `gsutil rsync -e` no longer follow
  symlinks out of the folder. Files published earlier that the ignore rules now exclude are
  removed: `rsync` runs with `--delete-excluded`, and after an `s3`, `gcs` or `azure` sync the
  CLI lists the site through the gateway and deletes them (`deploy --json` shows the commands
  as `cleanup`; a failed removal fails the deploy).
- `backyard dev` prints a `check:` line (`curl -s -H 'Host: <site>.localhost:<port>'
  http://127.0.0.1:<port>/`) for tools that cannot resolve `*.localhost`, and the agent
  instructions use it.
- The `backyard mcp` tool `backyard_deploy` takes an optional `dir`: without it, it publishes
  the project's `.backyard.json` `dir` like `backyard deploy` (naming the root of a project that
  publishes a build folder is refused), and its result names the folder it published.
- Warehouse results are capped at 32 MiB of row data as well as `warehouse.max_rows`
  (`truncated: true`), enforced while rows are read; the `warehouse query` log line reports
  `result_bytes`.
- Realtime events of documents too large for one pub/sub message (about 1 MiB, possible when
  `database.max_doc_bytes` is raised) are delivered with `doc: null` instead of being dropped,
  and the SDK's `subscribe` re-reads such a document before it calls `onCreate`/`onUpdate`.
  Events are no longer HTML-escaped.
- The AWS Lambda deploy gateway gzips large text responses, so deploy plans of thousands of
  files fit its 6 MB response limit; a response that still does not fit (reading back a large
  binary file) is a `413` that names the embedded gateway instead of a `502`.
- The Lambda gateway reads `server.secret` from Secrets Manager at cold start
  (`BACKYARD_SECRET_ARN`); the AWS module no longer puts the secret in the function's
  environment.
- Terraform: the Google Cloud and AWS modules route `POST /__backyard/api/tokens` with an
  `Authorization` header past IAP or the ALB login, and the Cloudflare module adds it to the
  Access bypass paths, so `backyard token create` works behind them. The Azure module warns
  (`postgres_open_to_all_of_azure`) when a public PostgreSQL server admits all of Azure.
- Helm: an optional `networkPolicy` (`enabled`, `from`, `metricsFrom`). **Breaking for one
  setup:** the chart refuses to render `auth.mode: header` without `jwt_header` while
  `trusted_proxies` covers whole ranges (the default private ranges admit every pod), unless
  `networkPolicy.enabled` is set or `trusted_proxies` lists only the proxies.
- Cloudflare edition: the daily image caps count generated images rather than requests, and an
  image request that does not fit what is left is refused before the model runs; a document
  query page carries at most about 8 MiB, so a page may hold fewer than `limit` documents with
  `has_more` — page by advancing `offset` by the documents received (both editions).
- Cloudflare edition: Node.js 22 or later is required (wrangler 4 needs it). `backyard
  cloudflare setup --download`/`--ref` (a source tarball) are replaced by `--package`; a
  `backyard` from `go install` or a source build has no package pinned, so outside a checkout
  it needs `--dir`, `--package` (such as a release's `backyard-cloudflare-<version>.tgz`) or a
  release build of the CLI. The
  `npm run deploy` and `deploy:ci` scripts of `deploy/cloudflare` are removed: they deployed
  `wrangler.jsonc`'s defaults past the saved state and the auth guard, and `setup` and
  `scripts/cloudflare-ci.mjs` replace them. The Terraform module is now
  optional, with a new `setup_args` output for `backyard cloudflare setup`.

### Fixed

- Accessibility of the hub, the docs page and the examples (WCAG 2.1 AA, light and dark mode).
- Site URLs in host mode are `<scheme>://<site>.<base_domain>[:<port>]/` everywhere — in the
  CLI (`deploy`, `sites`), the MCP server, the hub and the `backyard init` agent files — also
  when the host of `server.public_url` differs from `server.base_domain`; they used to be
  derived from the public URL's host.
- Overlapping deploys of one site: a commit no longer deletes another deploy's verified upload
  as unverified (only presigned Azure SAS uploads, which bind nothing, have wrong-size objects
  deleted), and the manifest records the SHA-256 of the bytes actually stored (S3's own
  verified checksum, else read), so a later deploy is never wrongly reported `unchanged`. The
  `deploy.commit` audit record counts the files it `verified` and those `superseded`.
- A `.backyardignore` saved with a UTF-8 byte order mark no longer loses its first rule.
- SQLite: number filters match integers between 2^53 and 2^63 exactly (`==`, `!=`, `<`, `<=`,
  `>`, `>=`, `in`, `not-in`, `contains`).
- `database.max_doc_bytes` counts `<`, `>` and `&` as one byte each (they were counted as their
  six-byte JSON escapes), as the Cloudflare edition does.
- DynamoDB: an update no longer resurrects the data of a document that was deleted and
  re-created concurrently.
- The Google Cloud module's bucket CORS rule allows the headers of signed uploads, so
  drag-and-drop deploys from the hub pass their preflight.
- Cloudflare edition: every response carries `X-Request-Id` (a valid incoming id is kept, as on
  the Go server); an encoded slash (`%2F`) in an API path parameter stays inside that parameter
  instead of splitting the path; chat image URLs are validated like on the Go server (absolute
  http(s) URLs of bounded length, or the site's own uploads); warehouse proxy failures get the Go server's generic,
  request-id-naming messages.
- Cloudflare edition: room, collection field and model names such as `__proto__`,
  `constructor` or `toString` are ordinary names (they used to reach `Object.prototype`, match
  nothing or fail with `500`); presence state with non-Latin-1 text no longer drops joins and
  subscriptions; large document queries no longer fail with `500`; parallel first deploys can
  no longer exceed the platform site cap; requests for unknown sites no longer create storage;
  `LIMITS` and `AI_MODELS` given as JSON variables are honoured; inline chat images are passed
  to providers without being decoded and re-encoded (and their 20 MiB cap is enforced).
- Cloudflare setup keeps a Worker's existing `DEPLOY_TOKEN` when it runs on another machine or
  in CI (it used to replace it, losing the new one in CI); only `--rotate-token` replaces it.
  Its secret-list parsing no longer mistakes a stray `[]` line for the list.

### Security

- Uploads are served without `Content-Security-Policy: sandbox` only when their type is known
  to be passive (raster images, audio/video, plain text, CSV, PDF, JSON, octet-stream), on both
  editions. Every other type is sandboxed — including `text/xsl` and any `+xml` type, which
  used to run script with the site's origin.
- Host mode, planted cookies (both editions): a request carrying valid session (or guest)
  cookies of **different** users is unauthenticated instead of using the first one, and the
  cookie variants Backyard did not set are expired, so a site's page cannot sign its visitors
  in as its author; the Cloudflare edition applies the same rule to Access's
  `CF_Authorization` cookie. Over https the login state cookie is now `__Host-backyard_oauth`,
  which no site can set (login CSRF); a sign-in that is in flight during the upgrade fails
  once with "Sign-in expired" and a "Try again" link.
- `POST /__backyard/api/tokens` with an API token as the credential (e.g. `backyard token
  create`) mints a token that expires no later than the presented one, on both editions, so a
  token can no longer renew itself indefinitely or outlive the person's offboarding. Tokens
  minted for a browser session, a proxy identity or an IdP JWT (device flow) keep the full
  `auth.token_ttl`.
- A CLI login that another site may have started (`/__backyard/auth/cli`, also with
  `mode=display`) asks the user for confirmation before a token is minted, on both editions:
  Backyard's own page posts back a CSRF token bound to the user and the browser session.
- The hub no longer adopts a deploy token from a `#token=` link silently: it asks the gateway
  whose token it is, names that account and the signed-in visitor, and uses (and stores) the
  token only after the visitor confirms; a token the server rejects is ignored with a notice.
- Cloudflare edition: an `AUTH_MODE` other than `none` or `access` (a typo, or a Go mode such
  as `oidc`) now fails closed — every request but the health checks gets `503` — instead of
  silently running without sign-in.
- `backyard pull` never writes what deploys never publish — `.git/` (a crafted `.git/config`
  could run commands), other dotfiles, `node_modules/`, `.backyard.json`, `.backyardignore`,
  `CLAUDE.md`, `AGENTS.md` — even with `--force`, comparing names as case-insensitive and
  Windows file systems do; skipped files are listed (`skipped` in `--json`). Both gateways
  reject `.git` path segments in deploys and file reads, and no ignore configuration can
  re-include `.git/`, `.backyard.json` or `.backyardignore`.
- A `.backyard.json` `dir` must stay inside the project (no absolute path, `..` or symlink
  leading out), so a cloned or pulled project file cannot make `backyard deploy`, `backyard
  dev` or the MCP server publish an unrelated folder.
- `backyard deploy` to a server other than the profile's no longer reuses the profile's cached
  bucket or rsync target (a direct sync with `--delete` could hit the wrong bucket), and
  `backyard login` offers the saved Cloudflare `DEPLOY_TOKEN` only to the server it was saved
  for, never to one that merely claims that Worker's public URL.
- `backyard deploy` errors no longer print signed upload URLs or storage signature details:
  query strings and upload capabilities are redacted, and storage error bodies are reduced to
  their code and message.
- `backyard mcp`: `backyard_init` and `backyard_deploy` only use folders inside the working
  directory, or an existing directory the agent names by its absolute path, and never the
  filesystem root, the home directory or folders holding credentials (the CLI's config folder,
  hidden home folders such as `~/.ssh` or `~/.aws`, `~/Library`, `~/AppData`, hidden folders
  outside the working directory); `backyard_read_site_file` refuses binary file types without
  downloading them and tells agents that file content is untrusted data; tool results and
  errors never contain tokens or signed upload URLs.
- Deploy uploads are bound to the size and SHA-256 the plan declared wherever the store can
  enforce it: S3 URLs sign the size (`Content-Length`, or `Content-MD5` for empty files) and,
  on AWS, the SHA-256 (`x-amz-checksum-sha256`), GCS URLs pin the size
  (`x-goog-content-length-range`), the gateway's own upload endpoint verifies both. A commit
  fails with `409 conflict` naming files that are missing or of the wrong size; with presigned
  Azure SAS URLs, which cannot restrict the body, it also deletes such objects, so no
  unverified object outlives it.
- Warehouse results are capped by size (above), so one wide raw-SQL query can no longer make
  the server buffer gigabytes. Warehouse engine and connection failures return a generic
  message naming the request id (the details go to the server log), since engine errors can
  reveal host, user and database names.
- Terraform: direct-sync deployers on Google Cloud (IAM condition) and Azure (ABAC condition)
  may only write below `<prefix>sites/`, as on AWS and with systemd, not `_meta/` or other
  sites' uploads; existing bindings and role assignments are replaced on the next apply. On
  Azure the role's container-level actions remain (container soft delete makes them
  recoverable). The rendered `backyard.yaml` of every module scenario is now loaded with the
  server's config loader in CI (`make tf-configcheck`).
- Further hardening from an adversarial review of both editions: stricter input validation,
  safer defaults and narrower error messages.

[Unreleased]: https://github.com/thasami/backyard/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/thasami/backyard/releases/tag/v0.1.0
