# Cloudflare edition (free tier)

Backyard also runs entirely on **Cloudflare's free Workers plan** — no server, no database, no
bucket, no credit card — in the spirit of [OpenQuick](https://github.com/topmass/openquick). The
Workers edition is a TypeScript Worker in [`deploy/cloudflare`](../deploy/cloudflare/README.md)
that implements the **same public contract** as the Go server: the same URLs, `backyard.js`
SDK and context preamble, site API, query semantics, websocket protocol, deploy gateway, API
tokens, `/llms.txt` and `/docs`. The `backyard` CLI, the Claude skill and the MCP server work
with it unchanged, and a shared conformance suite keeps the two editions in step.

## Set it up

You need a Cloudflare account with a `workers.dev` subdomain and Node.js 20+.

```sh
backyard cloudflare setup --download     # no checkout needed; or run it inside a checkout
# or, from a checkout:
cd deploy/cloudflare && npm ci && npm run setup
```

The setup script logs in with wrangler (browser), creates the secrets (`BACKYARD_SECRET` and a
`DEPLOY_TOKEN`), optionally an R2 bucket (`-- --r2`), deploys the Worker and prints the hub URL.
`backyard cloudflare setup` also logs the CLI in with the deploy token, so this works right
away:

```sh
backyard init my-site && cd my-site
backyard deploy          # → https://backyard.<account>.workers.dev/s/my-site/
```

Re-run setup to upgrade; earlier choices are remembered. Options after `--`:
`--worker-name <name>`, `--r2`, `--var KEY:VALUE` (e.g. `--var AUTH_MODE:access`),
`--rotate-token`. A GitHub Actions workflow and Terraform for a custom domain with Cloudflare
Access ([`deploy/terraform/cloudflare`](../deploy/terraform/cloudflare/README.md)) are included.

## How it maps

| Go server | Workers edition |
|---|---|
| `backyard serve` | One Worker: router, auth, SDK/hub/docs, deploy gateway, AI |
| Blob storage + document store | One `SiteDO` Durable Object per site (SQLite): files up to 5 MiB, documents, upload metadata; optional R2 for bigger files |
| Site list, quotas | One `RegistryDO` |
| Realtime + pub/sub | The site's Durable Object with websocket hibernation (no pub/sub needed) |
| `auth.mode` | `AUTH_MODE=none` (anonymous cookies; deploys need `DEPLOY_TOKEN` unless `ALLOW_ANONYMOUS_DEPLOY`) or `access` (Cloudflare Access verifying `Cf-Access-Jwt-Assertion`, with Okta, Entra ID, Google… as Access identity providers) |
| AI providers | Workers AI binding (free daily allowance) behind `smart`, `fast`, `image`, `embed`; optional Anthropic / OpenAI-compatible keys |
| Warehouse | Not available (`501`), unless `WAREHOUSE_URL` proxies to a Go server |
| Deploy methods | `api` only |
| Rate limits | Daily caps per user / IP and per platform (`LIMITS`) |

URLs default to path mode on `workers.dev` (`/s/<site>/`); a custom domain works in path mode,
and host mode needs a wildcard route and certificate. Note that **path mode shares one origin
between all sites and the hub** — see [security](security.md#host-mode-vs-path-mode).

With the default `AUTH_MODE=none`, anyone with a link can open sites and use their APIs as an
anonymous guest; put Cloudflare Access in front (free for up to 50 users) for a company login.

Within the shared contract, a few edition differences are visible to pages and CLIs (all
listed in the [specification](spec/SPEC.md#16-cloudflare-edition-free-tier)):

- **Deploys are atomic:** site files are content-addressed in the site's Durable Object, every
  upload is verified (size and SHA-256) before it is stored, and the commit switches the whole
  site in one transaction. Uploads of deploys that never commit are garbage-collected later.
- **Smaller AI requests:** at most 4 images per `backyard.ai.image` call (Go server: 10) and
  100 strings per `backyard.ai.embed` call (Go server: 2048). Provider failures map to the same
  status codes as on the Go server.
- **Dot segments:** the Workers runtime resolves `/./` and `/../` in a URL before the Worker
  sees it, so such a URL is served as its cleaned path instead of being redirected.
- **Sign-in:** Cloudflare Access (or nobody, in `none`) signs people in, so
  `/__backyard/auth/login?rd=…` simply redirects to `rd`.

Everything else — free-plan quotas and Backyard's guardrails, configuration variables, AI
model aliases, local development, CI, differences from the Go edition, upgrades and
troubleshooting — is in the edition's [README](../deploy/cloudflare/README.md).
