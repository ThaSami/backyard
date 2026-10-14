# Security model

Backyard makes it trivially easy for anyone in a company to publish a page with a database, AI
and access to company data. That is only safe inside a well-defined boundary. This page states
the threat model, the mechanisms that enforce it and how to harden an installation. To report a
vulnerability, see [SECURITY.md](../SECURITY.md).

## The trust bubble

Backyard's core assumption, inherited from Shopify's Quick: **everyone who can sign in is a
colleague**.

- **Outsiders** — anyone your identity provider has not authenticated — must not be able to
  read a page, call an API or deploy. This is enforced on every request by the configured
  [auth mode](authentication.md); `auth.allowed_domains`, `auth.allowed_emails` and
  `auth.allowed_groups` narrow it further.
- **Insiders** can read and write the data of **every** site: there are no per-site or
  per-document permissions, and `_meta.created_by` is a UX hint, not an access control. Any
  employee can open any site, query its collections, read its uploads and redeploy it (unless
  `sites.ownership: owner`). Sites should not hold data that not every employee may see.
- **Site authors** run arbitrary JavaScript on their site's origin — that is the product. The
  platform must keep one site's code from acting on another site's data or on the hub.

`auth.mode: none` removes the bubble entirely: use it only on a laptop or an isolated lab
network.

## Host mode vs path mode

| | Host mode (`server.url_mode: host`, default) | Path mode (`url_mode: path`) |
|---|---|---|
| URLs | `https://<site>.backyard.acme.com/` | `https://backyard.acme.com/s/<site>/` |
| Needs | wildcard DNS and a wildcard certificate | one hostname |
| Origins | **each site is its own origin**, separate from the hub | **all sites and the hub share one origin** |
| Site JS vs other sites' APIs | blocked (CSRF header + Origin check, no CORS) | possible: same origin |
| Site JS vs hub API (e.g. minting an API token for the visitor) | blocked | possible |
| `localStorage`, service workers, non-HttpOnly cookies | per site | shared by all sites |

Path mode is a deliberate trade-off for environments without wildcard DNS or TLS. Use it only
when every site author is trusted with every visitor's session — a page on any site can act as
its visitor everywhere on the platform. Host mode is strongly recommended.

## Requests and CSRF

- **Authentication on every request.** Pages, uploads, APIs, websockets and the hub all go
  through the authenticator; only `/__backyard/healthz`, `/__backyard/readyz`, the SDK files,
  `/__backyard/auth/*`, `/__backyard/api/info` and the gateway's signed upload URLs are public.
- **CSRF.** Every state-changing API request must carry `X-Backyard: 1`, and an `Origin`
  header, when present, must be the request's own origin (path mode: the hub's). Browsers cannot
  add custom headers to cross-site forms, and cross-origin `fetch` with a custom header needs a
  CORS preflight that Backyard never approves. The deploy gateway applies the same rule, except
  for bearer-token requests without browser cross-site signals (`Origin`, `Sec-Fetch-Site`) and
  for signed upload URLs.
- **Websockets** check `Origin` against the site's origin (path mode: the hub); cross-site
  connections need `realtime.allow_cross_site_ws`.
- **Redirects.** The login `rd` parameter only accepts the hub, a site host or a relative path
  (in every mode, including the proxy modes where `/__backyard/auth/login` just redirects to
  it); the CLI hand-off only redirects to loopback addresses.
- **CLI login.** Any web page can navigate a signed-in browser to `/__backyard/auth/cli`. A
  request that arrives as such a cross-site navigation therefore gets a confirmation step on
  Backyard's own page before a token is minted and handed to a loopback port (or shown, with
  `mode=display`); only the URL the CLI opens itself proceeds directly. The confirmation form
  posts back with a CSRF token that is bound to the user (and the sign-in session) and expires
  after 10 minutes, and the page's CSP lets the form go only to Backyard and the CLI's loopback
  address.
- **Host routing.** Only the hub host and single-label subdomains of `server.base_domain` are
  served; anything else is `404`. Forwarded headers (`X-Forwarded-Host`, `-Proto`, `-For`) are
  honoured only from `server.trusted_proxies`. In dev mode, unknown host names stay `404` so a
  DNS-rebinding page cannot reach a local server.
- **Response headers:** `X-Content-Type-Options: nosniff`, `Referrer-Policy:
  strict-origin-when-cross-origin`, `Cache-Control: no-cache` (pages) or `no-store` (auth,
  errors). There is **no** `X-Frame-Options`: embedding sites in sites is a feature. If you need
  clickjacking protection against external origins, add
  `Content-Security-Policy: frame-ancestors https://backyard.acme.com https://*.backyard.acme.com`
  at your reverse proxy.

## Cookies and sessions

| Cookie | Content | Lifetime |
|---|---|---|
| `backyard_session` (oidc mode) | AES-256-GCM-sealed `{user, exp}` | `auth.oidc.session_ttl` (12 h) |
| `__Host-backyard_oauth` (https) / `backyard_oauth` (http), oidc mode | signed login state (state, PKCE verifier, nonce, return URL) | 10 min |
| `backyard_anon` (none mode) | signed anonymous id | 1 year |

All are `HttpOnly` and `SameSite=Lax`, and `Secure` when `server.public_url` is https. The
session and guest cookies use `Path=/` and, in host mode, `Domain=.<base_domain>` so one login
covers every site. The login state cookie is set by the hub alone: over https it is a
`__Host-` cookie (`Secure`, `Path=/`, no `Domain`), which no site can set or overwrite; over
http it is `backyard_oauth` at `Path=/__backyard/auth/`. Because every site is "same-site"
with the others, `SameSite` alone is not a CSRF defence — the custom header and Origin checks
above are.

**Planted cookies.** Site JavaScript cannot read these cookies, but in host mode a site's page
can *set* a cookie with the same name for the parent domain or a longer path, and browsers
send it along with Backyard's own. A sealed session or a signed guest id is not tied to a
browser, so a site author could try to plant their own session or guest cookie in visitors'
browsers (session fixation), or their own login state (login CSRF). Backyard defends against
both:

- the login state cookie is a `__Host-` cookie over https, and a planted `backyard_oauth` is
  ignored;
- when a request carries valid `backyard_session` (or `backyard_anon`) cookies of **different**
  users, Backyard never picks one: the request is unauthenticated (`401`, or the login
  redirect), and every variant of the cookie that Backyard does not set itself (host-only,
  parent domains, longer paths) is expired, so the next request normally works again. The
  attempt is logged as an audit event. Where a planted cookie cannot be reached (a partitioned
  cookie, a very deep path) requests keep failing closed, which is no worse than the sign-out
  any site could already cause. One gap remains: a visitor with no session of their own and
  only a planted one is signed in as its owner, because the sealed value is not tied to a
  browser.

The Cloudflare edition applies the same rule to its guest cookie and to Cloudflare Access's
`CF_Authorization` cookie. Host mode over plain http cannot protect the login state (no
`__Host-` without `Secure`); the server logs a warning at startup — use https outside local
tests.

## Secrets, keys and tokens

- **`server.secret`** (≥ 32 characters) is the root of every key. Each use derives its own
  32-byte key with HKDF-SHA256 (salt `backyard`, info `backyard/session/v1`,
  `backyard/oauth-state/v1`, `backyard/api-token/v1`, `backyard/deploy/v1`,
  `backyard/upload/v1`, `backyard/anon/v1`, `backyard/cli-confirm/v1`), so a key for one purpose
  can never forge another.
  Store it in a secret manager; never in the config file.
- **API tokens** are HS256 JWTs (issuer `backyard`, scope `api`) with the user's identity, valid
  for `auth.token_ttl` (720 h by default, also the maximum). Guests and static tokens cannot
  mint them. A token minted with an API token as the credential (`POST /__backyard/api/tokens`
  with a bearer token, e.g. `backyard token create` after a browser login) expires no later
  than that token, so a token cannot extend its own lifetime and access ends once the tokens
  minted from the last real sign-in expire; tokens minted for a browser session, a proxy
  identity or an IdP-issued JWT (device flow) get the full lifetime. The CLI stores tokens in a
  `0600` file.
- **Static tokens** are stored only as SHA-256 hashes (`auth.tokens`) and compared in constant
  time; rotate one by adding the new hash before removing the old one.
- **IdP tokens** (device flow) are verified against the issuer's keys with issuer and audience
  checks (`client_id`, `device_client_id`, `api_audiences`).
- **Rotation.** Tokens are stateless, so revocation is global: rotating `server.secret` signs
  everyone out and invalidates every API token, deploy id and upload URL at once (also on the
  Lambda gateway, which must share the secret). Keep automation tokens short-lived.
- Provider keys (AI, warehouse, storage) live only on the server; pages never see them.

## Rate limits and quotas

Per user (per client IP for guests), per instance, `429` with `Retry-After`:

| Limit | Default |
|---|---|
| API calls | 600/min |
| Document writes | 300/min |
| AI calls / AI tokens | 20/min / 2,000,000 per day |
| Warehouse queries | 30/min |
| Uploads | 60/min |
| Deploys | 60/h |
| Websocket frames | 60/s (burst 120) per connection; 50 connections per user |

Size limits: documents 256 KiB, 100,000 documents per site, uploads 100 MiB, sites 500 MiB /
10,000 files / 100 MiB per file, websocket frames 64 KiB, presence 8 KiB; other JSON request
bodies are bounded too (queries and most bodies 1 MiB, AI chat 32 MiB). See
[configuration](configuration.md#ratelimit).

## Uploads and deployed files

- Upload names are sanitized (base name only, no control, separator or bidi characters).
- Uploaded files are served with `Content-Disposition: inline`. The declared type comes from
  the uploader, so only known **passive** types — raster images, audio and video, plain text,
  CSV, PDF, JSON and `application/octet-stream` — are served as they are; everything else
  (HTML, SVG, any XML including `text/xsl` and `+xml` types, scripts, unknown types) also gets
  `Content-Security-Policy: sandbox`, so an uploaded document can never run script as the
  site. Both editions use the same allowlist ([SPEC §3](spec/SPEC.md#3-static-serving)).
  Deployed site files are served as-is, because pages are the product.
- Deploy paths are validated (relative, no `..`, no leading `/`, nothing under `__backyard/`,
  no `.git` segment in any letter case); `/__backyard/` can never be shadowed by a site file,
  and repository metadata can neither be deployed nor read back through the gateway.
- Uploads during a deploy are bound to the declared size and SHA-256 where the backend allows
  it (the gateway's own endpoint: both; S3: the size and, on AWS, the SHA-256; GCS: the size;
  Azure SAS: neither). The commit verifies sizes before deleting anything and fails with `409`
  when a listed file is missing or has the wrong size. With presigned Azure SAS uploads it also
  deletes such a wrong-size object (unless it is the previously committed version), so no
  unverified object outlives a commit; elsewhere a wrong-size object can only be another
  deploy's verified upload and stays. The manifest records the digest of the bytes actually
  stored, never just the one the client declared. Deploy ids and upload URLs are HMAC-signed
  and expire after one hour, and only authorized deployers get them (rate-limited, audited as
  `deploy.create`).
- The `fs` store confines every operation to its root and refuses symlinks that escape their
  directory, so a racing `rsync` cannot redirect writes.
- Direct-sync deployers in the Terraform modules (AWS, Google Cloud, Azure) and the systemd
  rsync target may only write below `<prefix>sites/`, never `_meta/` or `uploads/` (on Azure the
  role's container-level actions remain; container soft delete makes them recoverable).

## The CLI and agents

A site is written by whoever deployed it, so its files are untrusted on your machine:

- `backyard pull` never writes what a deploy never publishes — `.git/`, other dotfiles and
  dot-folders (`.claude/`, `.env`…; `.well-known/` is kept), `node_modules/`, `.backyard.json`,
  `.backyardignore`, `CLAUDE.md`, `AGENTS.md` — even with `--force`, comparing names the way
  case-insensitive and Windows file systems do. A pulled folder therefore cannot carry a
  crafted `.git/config` (which git would act on), project settings or agent instructions; the
  skipped paths are listed.
- A `.backyard.json` `dir` must stay inside the project (no absolute paths, `..` or symlinks
  leading out), so a cloned or pulled project file cannot point `backyard deploy` at an
  unrelated folder. `.git/` and the project files are never uploaded, whatever the ignore
  configuration says.
- The CLI uses a profile's cached server info (bucket, rsync target) only for that profile's
  server, and `backyard login` offers the Cloudflare `DEPLOY_TOKEN` saved by setup only to the
  server or gateway URL it was saved for — never to a server that merely claims that Worker's
  public URL.
- Transport errors of `backyard deploy` never print signed upload URLs: their query strings
  and upload capabilities are redacted, and storage error bodies are reduced to their code and
  message.
- `backyard mcp` keeps agents inside the project folder and away from credential folders, and
  its results never contain tokens or signed URLs (see [agents](agents.md#mcp-server)).

## Outbound requests (SSRF)

Backyard fetches remote content in one place on behalf of users: image URLs in AI chat
messages, for providers that need bytes. That fetcher only allows http(s), refuses loopback,
private, link-local, multicast and other non-public addresses **on the dialled IP** (so DNS
rebinding cannot bypass it), caps each image at 20 MiB and each request at 64 MiB, and never
echoes URL credentials or query strings in errors. Provider error messages are sanitized before
they reach clients. An image URL of one of the site's own uploads (what `files.upload` returns,
site-relative or on the site's own origin) is never fetched: the server reads that file from
storage, and only from the requesting site's uploads — another site's file, an unknown file or
any other site-relative URL is rejected (`400`). Its type is detected from the bytes (png,
jpeg, gif or webp), and it counts against the same 20 MiB and 64 MiB limits.

## Error messages

Server-side failures do not reveal infrastructure: storage and database errors and warehouse
engine and connection errors (which can name hosts, users and databases) reach the page as
generic messages, and the server logs the cause with the request id that every response
carries as `X-Request-Id` (warehouse failures name it in the message too). AI provider messages
are sanitized and mapped to stable status codes.

## Warehouse

Pages never see warehouse credentials. Use a **read-only role** for every source (the real
boundary), keep `warehouse.allow_raw_sql` off unless every employee may read everything that
role can, prefer named queries, and cap BigQuery cost with `max_bytes_billed`. Postgres queries
run in read-only, rolled-back transactions over the extended protocol; raw SQL must be a single
`SELECT`/`WITH`. Results are capped at `warehouse.max_rows` and at 32 MiB of row data, enforced
while rows are read, so one wide query cannot exhaust the server's memory. Details in
[warehouse](warehouse.md#read-only-by-construction).

## Logging and audit

Backyard avoids logging secrets, tokens, warehouse SQL and parameters, and URL credentials.
Audit events (logins,
token issuance, deploys, refreshes, deletions, owner changes) are logged with `"audit": true`
and the acting user and client IP; see [operations](operations.md#logs-and-audit).

## Hardening checklist

- [ ] An identity-aware mode (`oidc`, `iap`, `alb`, `header`) — never `none` on a shared network.
- [ ] `server.secret` from a secret manager, 32+ random characters (`openssl rand -hex 32`).
- [ ] Host mode with a wildcard certificate (path mode only if every site author is trusted).
- [ ] `server.trusted_proxies` set to exactly your load balancers / proxies.
- [ ] `auth.allowed_domains` / `allowed_emails` / `allowed_groups` (always, when anyone outside
      your organisation can complete the sign-in, e.g. Google with personal accounts), and
      `admins` / `admin_groups` for a small group.
- [ ] Behind IAP/ALB: `auth.iap.audience` / `auth.alb.signer_arn` set, and Backyard reachable
      only through the proxy (plus the proxy-exempt routes for the deploy gateway and
      bearer-token `POST /__backyard/api/tokens`).
- [ ] `header` mode without `jwt_header`: `server.trusted_proxies` lists only the proxies (on
      Kubernetes, enable the Helm chart's `networkPolicy`, which the chart requires otherwise).
- [ ] Read-only warehouse roles; raw SQL off unless intended.
- [ ] `sites.ownership: owner` if teams must not overwrite each other's sites.
- [ ] Short `auth.token_ttl` for automation; static CI tokens rotated.
- [ ] Metrics on a private `telemetry.metrics_listen`, not the public hub.
- [ ] TLS everywhere (`server.public_url` with `https://` makes cookies `Secure`).
- [ ] Backups of the database and the bucket (see [operations](operations.md#backups)).
