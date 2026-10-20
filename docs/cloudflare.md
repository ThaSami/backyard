# Cloudflare edition (free tier)

Backyard also runs entirely on **Cloudflare's free Workers plan** — no server, no database, no
bucket, no credit card — in the spirit of [OpenQuick](https://github.com/topmass/openquick). The
Workers edition is a TypeScript Worker in [`deploy/cloudflare`](../deploy/cloudflare/README.md)
that implements the **same public contract** as the Go server: the same URLs, `backyard.js`
SDK and context preamble, site API, query semantics, websocket protocol, deploy gateway, API
tokens, `/llms.txt` and `/docs`. The `backyard` CLI, the Claude skill and the MCP server work
with it unchanged, and a shared conformance suite keeps the two editions in step.

## Set it up

You need a Cloudflare account (the free plan; a `workers.dev` subdomain is registered for you
if the account has none) and Node.js 22 or later with npm. Terraform is not needed, and with a
release build of the `backyard` CLI (or, once it is on npm, the package alone) neither is a
checkout or a build; any of these works:

```sh
backyard cloudflare setup                 # with the backyard CLI; it logs the CLI in afterwards
npx backyard-cloudflare@latest setup      # with only Node.js, once the package is on npm
cd deploy/cloudflare && npm ci && npm run setup   # from a checkout of this repository
```

All three run the same engine, `backyard-cloudflare`: an npm package with a prebuilt Worker
and a pinned wrangler (a checkout builds the Worker from source). `backyard cloudflare setup`
installs the package of its own release, checked against the SHA-256 built into the CLI. A
`backyard` built from source (`go install`, `make build`) has none pinned: run it inside a
checkout, or pass `--dir <checkout>/deploy/cloudflare` or `--package` with a release's
`https://github.com/thasami/backyard/releases/download/v<version>/backyard-cloudflare-<version>.tgz`
(not verified against a digest). Before the first release of Backyard, the package is neither
on npm nor attached to a release: use a checkout, or build it with `make cloudflare-pack` (see
the [edition's README](../deploy/cloudflare/README.md#building-the-npm-package)). Run
`npx backyard-cloudflare` only once a Backyard release has published it to npm: until then npm
serves nothing under that name, or something that is not Backyard.

Behind an HTTP proxy, wrangler uses `HTTPS_PROXY`, but Backyard's own API calls and checks use
Node's `fetch`: set `NODE_USE_ENV_PROXY=1` as well, on Node 22.21 or later. The engine warns when
it sees a proxy variable that `fetch` would ignore.

Setup logs in with wrangler (browser), registers the account's workers.dev subdomain when it
has none, creates the secrets (`BACKYARD_SECRET` and a `DEPLOY_TOKEN`), an R2 bucket when R2 is
enabled, deploys the Worker and prints the hub URL. `backyard cloudflare setup` also logs the
CLI in with the deploy token, so this works right away:

```sh
backyard init my-site && cd my-site
backyard deploy          # → https://backyard.<account>.workers.dev/s/my-site/
```

Each further step is one command that checks its own result:

```sh
backyard cloudflare domain add backyard.acme.com                  # the hub on your own domain
backyard cloudflare domain add acme-sites.dev --host-mode         # or sites at https://<site>.acme-sites.dev/
backyard cloudflare access enable --allow-email-domain acme.com   # company login with Cloudflare Access
backyard cloudflare status                                        # read-only live check
```

Re-run setup to upgrade: every choice is kept (secrets, the deploy token, R2, AI, the site
listing, the domain, Access and `--var` overrides). A machine without saved state (a teammate,
a new laptop, CI) adopts the deployed configuration, and keeps a Worker's existing
`DEPLOY_TOKEN` unless you pass `--rotate-token` (run it with `DEPLOY_TOKEN=<token>` to remember
the token there). A deploy that would turn a live Cloudflare Access off is refused unless you
confirm it, and deploys never change routes or custom domains. Every command and flag:
[`backyard cloudflare`](cli.md#backyard-cloudflare) and the
[edition's README](../deploy/cloudflare/README.md#command-reference). A GitHub Actions workflow
is included, and an optional Terraform module
([`deploy/terraform/cloudflare`](../deploy/terraform/cloudflare/README.md)) for teams that
manage Cloudflare as code.

## How it maps

| Go server | Workers edition |
|---|---|
| `backyard serve` | One Worker: router, auth, SDK/hub/docs, deploy gateway, AI |
| Blob storage + document store | One `SiteDO` Durable Object per site (SQLite): files up to 5 MiB, documents, upload metadata; optional R2 for bigger files |
| Site list, quotas | One `RegistryDO` |
| Realtime + pub/sub | The site's Durable Object with websocket hibernation (no pub/sub needed) |
| `auth.mode` | `AUTH_MODE=none` (anonymous cookies; deploys need `DEPLOY_TOKEN` unless `ALLOW_ANONYMOUS_DEPLOY`) or `access` (Cloudflare Access verifying `Cf-Access-Jwt-Assertion`, with Okta, Entra ID, Google… as Access identity providers) |
| AI providers | Workers AI binding (free daily allowance) behind `smart`, `fast`, `image`, `embed`; optional Anthropic / OpenAI-compatible keys |
| `ai.enabled: false` | `AI_ENABLED=false` (`setup --no-ai`, which also leaves the Workers AI binding out) |
| `sites.listing: owners` | `SITE_LISTING=owners` (`setup --site-listing owners`; implies `OWNERSHIP=owner`) |
| Warehouse | Not available (`501`), unless `WAREHOUSE_URL` proxies to a Go server |
| Deploy methods | `api` only |
| Rate limits | Daily caps per user / IP and per platform (`LIMITS`) |

URLs default to path mode on `workers.dev` (`/s/<site>/`); `domain add` adds a custom domain in
path mode, and `domain add --host-mode` host mode, which needs a wildcard route, DNS record and
certificate. Note that **path mode shares one origin
between all sites and the hub** — see [security](security.md#host-mode-vs-path-mode).

With the default `AUTH_MODE=none`, anyone with a link can open sites and use their APIs as an
anonymous guest; put Cloudflare Access in front (free for up to 50 users) for a company login.
`AUTH_MODE` must be exactly `none` or `access`: any other value (a typo, or a Go server mode
such as `oidc`) makes the Worker **fail closed** — every request except
`/__backyard/healthz` gets `503`, `/__backyard/readyz` names the problem, and `npx wrangler
tail` shows it — rather than silently running without sign-in.

Access must let through the paths the Worker authenticates itself: `/__backyard/api/deploy`
and `/__backyard/api/deploy/*` (deploys with `DEPLOY_TOKEN` or API tokens, signed upload URLs),
`/__backyard/api/tokens` (`backyard token create`; a minted token never outlives the token that
asked for it), the health checks `/__backyard/healthz` and `/__backyard/readyz`, and the
[Claude connector's](claude-connector.md) machine paths `/__backyard/mcp`, `/__backyard/oauth/*`,
`/.well-known/oauth-authorization-server` and
`/.well-known/oauth-protected-resource/__backyard/mcp` (the Worker answers them with `404`
until `MCP_ENABLED` is set). `backyard cloudflare access enable` creates this Bypass application
(with an API token in `BACKYARD_CF_API_TOKEN`) or prints the dashboard steps, and verifies at
the edge that all nine paths reach the Worker before it switches the Worker to Access; after an
upgrade, run it once more so the bypass application gains the connector's paths (with the API
token again, or it names the destinations to add in the dashboard; `backyard cloudflare status`
reports them as `access-connector`). The optional
[Terraform module](../deploy/terraform/cloudflare/README.md) creates the same (`bypass_paths`).

**Use Backyard in Claude or ChatGPT.** `backyard cloudflare setup --var MCP_ENABLED:true`
(Terraform: `mcp_enabled = true`) turns on the [Claude connector](claude-connector.md) at
`https://<hub>/__backyard/mcp`: people add it to Claude, ChatGPT or another MCP app, which then
publishes sites as them. It signs people in with Cloudflare Access, so publishing from an app
needs `AUTH_MODE=access` (or
`ALLOW_ANONYMOUS_DEPLOY`, which lets anyone publish); `DEPLOY_TOKEN` plays no part. One publish
carries at most 2 MiB of file content (`LIMITS.mcp_publish_bytes`) and tool calls count against
`LIMITS.mcp_calls_per_day`; `MCP_CLIENTS` and `MCP_REDIRECT_URIS` change which apps may connect
from anywhere (apps on the user's computer always can).
Invalid values turn only the connector off and are listed by `/__backyard/readyz`.

**Turning AI off.** AI is on by default. `backyard cloudflare setup --no-ai` (remembered;
Terraform: `ai_enabled = false`) deploys without the Workers AI binding, for an account where
Workers AI is not available, and with `AI_ENABLED=false`: every AI endpoint then answers
`501 unsupported` ("AI is not configured on this server"), exactly as the Go server does with
`ai.enabled: false`, so sites degrade the same way on both. `setup --ai` turns it back on. See
the edition's [README](../deploy/cloudflare/README.md#turning-ai-off).

**Owners-only site listing.** `backyard cloudflare setup --site-listing owners` (remembered;
Terraform: `site_listing = "owners"`; GitHub Actions: `BACKYARD_CF_SITE_LISTING=owners`) sets
`SITE_LISTING=owners`: the hub, `backyard sites list` and `backyard_list_sites` then show each
person only their own sites, and admins (`ADMINS`, `ADMIN_GROUPS`, the `DEPLOY_TOKEN`) every
site. It implies `OWNERSHIP=owner`, which protects a site once it has owners: sites deployed
while `OWNERSHIP` was off have none, so until then any signed-in deployer can replace one and
becomes its owner, and any signed-in user may delete one or set its owners (give the sites that
matter owners right after turning the listing on). With the default `AUTH_MODE=none` every
browser is a guest and is listed no site (only the CLI logged in with the `DEPLOY_TOKEN` sees them; pasting the token into the hub
does not change its list), so it is meant for `AUTH_MODE=access`. It hides sites from the
directory and does not restrict who can open them. `--site-listing all` lists every site again
and turns ownership off with it, unless `OWNERSHIP=owner` is set (`--var OWNERSHIP:owner`). See
[owners-only site listing](deploying-sites.md#owners-only-site-listing).

Within the shared contract, a few edition differences are visible to pages and CLIs (all
listed in the [specification](spec/SPEC.md#16-cloudflare-edition-free-tier)):

- **Deploys are atomic:** site files are content-addressed in the site's Durable Object, every
  upload is verified (size and SHA-256) before it is stored, and the commit switches the whole
  site in one transaction. Uploads of deploys that never commit are garbage-collected later.
- **Smaller AI requests:** at most 4 images per `backyard.ai.image` call (Go server: 10) and
  100 strings per `backyard.ai.embed` call (Go server: 2048). Provider failures map to the same
  status codes as on the Go server. The daily image caps (`LIMITS`) count images, not calls:
  `n: 4` uses four.
- **Query pages have a byte budget:** a page carries at most about 8 MiB of stored documents,
  so with large documents a `list` or query returns fewer than `limit` documents with
  `hasMore: true`. Page by advancing `offset` by the number of documents received
  (`offset += docs.length`), not by `limit` — that works on both editions.
- **Dot segments:** the Workers runtime resolves `/./` and `/../` in a URL before the Worker
  sees it, so such a URL is served as its cleaned path instead of being redirected.
- **Sign-in:** Cloudflare Access (or nobody, in `none`) signs people in, so
  `/__backyard/auth/login?rd=…` simply redirects to `rd`, and `/__backyard/auth/logout` clears
  the guest cookie (`none`) or sends you to Access's logout (`access`). The CLI login's
  cross-site confirmation, the cap on tokens minted with a token and the refusal of conflicting
  sign-in cookies (including planted `CF_Authorization` cookies) work as on the Go server.

Everything else — free-plan quotas and Backyard's guardrails, configuration variables, AI
model aliases, local development, CI, differences from the Go edition, upgrades and
troubleshooting — is in the edition's [README](../deploy/cloudflare/README.md).
