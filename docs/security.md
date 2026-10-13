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
  [auth mode](authentication.md); `auth.allowed_domains` and `auth.allowed_groups` narrow it
  further.
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
| `backyard_oauth` | signed login state (state, PKCE verifier, nonce, return URL) | 10 min |
| `backyard_anon` (none mode) | signed anonymous id | 1 year |

All are `HttpOnly`, `SameSite=Lax`, `Path=/`, and `Secure` when `server.public_url` is https.
In host mode they are scoped to `.<base_domain>` so one login covers every site; site
JavaScript cannot read them. Because every site is "same-site" with the others, `SameSite`
alone is not a CSRF defence — the custom header and Origin checks above are.

## Secrets, keys and tokens

- **`server.secret`** (≥ 32 characters) is the root of every key. Each use derives its own
  32-byte key with HKDF-SHA256 (salt `backyard`, info `backyard/session/v1`,
  `backyard/oauth-state/v1`, `backyard/api-token/v1`, `backyard/deploy/v1`,
  `backyard/upload/v1`, `backyard/anon/v1`, `backyard/cli-confirm/v1`), so a key for one purpose
  can never forge another.
  Store it in a secret manager; never in the config file.
- **API tokens** are HS256 JWTs (issuer `backyard`, scope `api`) with the user's identity, valid
  for `auth.token_ttl` (720 h by default, also the maximum). They are minted only for a person
  authenticated by the IdP or proxy (a token cannot renew itself, guests and static tokens
  cannot mint), and the CLI stores them in a `0600` file.
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
- Uploaded files are served with `Content-Disposition: inline` and, for active content (HTML,
  XHTML, SVG, XML), `Content-Security-Policy: sandbox` — an uploaded document can never run
  script as the site. Deployed site files are served as-is, because pages are the product.
- Deploy paths are validated (relative, no `..`, no leading `/`, nothing under `__backyard/`);
  `/__backyard/` can never be shadowed by a site file.
- Uploads during a deploy are bound to the declared size and SHA-256 where the backend allows
  it (the gateway's own endpoint: both; S3: the size and, on AWS, the SHA-256; GCS: the size;
  Azure SAS: neither). The commit verifies sizes before deleting anything, deletes a listed
  object of the wrong size and fails with `409`, so no unverified object outlives a commit.
  Deploy ids and upload URLs are HMAC-signed and expire after one hour, and only authorized
  deployers get them (rate-limited, audited as `deploy.create`).
- The `fs` store confines every operation to its root and refuses symlinks that escape their
  directory, so a racing `rsync` cannot redirect writes.

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
`SELECT`/`WITH`. Details in [warehouse](warehouse.md#read-only-by-construction).

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
- [ ] `auth.allowed_domains` / `allowed_groups`, and `admins` / `admin_groups` for a small group.
- [ ] Behind IAP/ALB: `auth.iap.audience` / `auth.alb.signer_arn` set, and Backyard reachable
      only through the proxy (plus the proxy-exempt deploy route).
- [ ] Read-only warehouse roles; raw SQL off unless intended.
- [ ] `sites.ownership: owner` if teams must not overwrite each other's sites.
- [ ] Short `auth.token_ttl` for automation; static CI tokens rotated.
- [ ] Metrics on a private `telemetry.metrics_listen`, not the public hub.
- [ ] TLS everywhere (`server.public_url` with `https://` makes cookies `Secure`).
- [ ] Backups of the database and the bucket (see [operations](operations.md#backups)).
