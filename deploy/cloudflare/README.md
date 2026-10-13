# Backyard on Cloudflare (Workers edition)

Run Backyard on your own Cloudflare account, on the **free plan**: one Worker plus
SQLite-backed Durable Objects, Workers AI, and an optional R2 bucket. You don't need a
server, a database, or a credit card.

This edition implements the same public contract as the Go server (see `docs/spec/SPEC.md`
§2–§8 and §16): the same URLs, `backyard.js` SDK, site API, websocket protocol, deploy gateway
and API tokens. The `backyard` CLI, the Claude skill and the MCP server work with it
unchanged. The idea comes from [OpenQuick](https://github.com/topmass/openquick): it deploys
straight to Cloudflare in a couple of minutes.

```
                        ┌─────────────────────────── your Cloudflare account ───────────────────────────┐
 browser / CLI / agent ─┤  Worker "backyard"  ── router · auth · SDK, hub & docs · deploy gateway · AI   │
                        │     │            │                                                             │
                        │     ▼            ▼                                                             │
                        │  SiteDO (one per site, SQLite)       RegistryDO (one, SQLite)                  │
                        │  files · documents · uploads ·       site list · users · daily guardrail       │
                        │  websocket hub (hibernation)         counters                                  │
                        │     │                                                                         │
                        │     └── R2 bucket (optional, files > 5 MiB)        Workers AI (binding)        │
                        └───────────────────────────────────────────────────────────────────────────────┘
```

## Contents

- [Quickstart (free plan)](#quickstart-free-plan)
- [What `npm run setup` does](#what-npm-run-setup-does)
- [Free-plan limits and Backyard's guardrails](#free-plan-limits-and-backyards-guardrails)
- [URL modes](#url-modes)
- [Custom domain and Cloudflare Access (Okta, Entra ID, Google…)](#custom-domain-and-cloudflare-access)
- [AI models](#ai-models)
- [Configuration reference](#configuration-reference)
- [Deploying from GitHub Actions](#deploying-from-github-actions)
- [Local development and tests](#local-development-and-tests)
- [Differences from the Go edition](#differences-from-the-go-edition)
- [Upgrading, backups and removal](#upgrading-backups-and-removal)
- [Troubleshooting](#troubleshooting)

## Quickstart (free plan)

You need:

- a Cloudflare account (the free plan is enough), with a `workers.dev` subdomain. The first
  time you open **Workers & Pages** in the dashboard, Cloudflare asks you to pick one.
- Node.js 20 or later.

```sh
git clone https://github.com/thasami/backyard.git
cd backyard/deploy/cloudflare
npm ci
npm run setup
```

If you have the `backyard` CLI, `backyard cloudflare setup` runs the same script. It finds this
folder in a checkout, or downloads the source with `--download`. Arguments after `--` go to
the script, for example `backyard cloudflare setup -- --worker-name team`. Afterwards the CLI
is already logged in to the new server.

The script opens the Cloudflare login in your browser if needed. At the end it prints
something like:

```
✔ Backyard is live at https://backyard.<you>.workers.dev

Connect the CLI (keep the token secret — it can deploy and delete sites):

  backyard login --server https://backyard.<you>.workers.dev --token byd_…
```

Then publish your first site:

```sh
backyard login --server https://backyard.<you>.workers.dev --token byd_…
backyard init my-site && cd my-site
backyard deploy            # → https://backyard.<you>.workers.dev/s/my-site/
```

By default (`AUTH_MODE=none`), anyone with a site's link can open it, the way Quick and
OpenQuick work. Visitors get an anonymous identity (a signed cookie). Deploying requires the
`DEPLOY_TOKEN` (share it with whoever publishes, or set `ALLOW_ANONYMOUS_DEPLOY=true` for a
playground). To limit who can **see** sites, and to give everyone a personal login and API
tokens, put
[Cloudflare Access](#custom-domain-and-cloudflare-access) in front. It's free for up to
50 users.

## What `npm run setup` does

You can re-run it at any time to upgrade. Each run:

1. Bundles the SDK (`web/sdk`), the hub UI (`web/hub`) and the agent docs (`skills/backyard`) into
   `src/generated` (`npm run sync`).
2. Checks `wrangler whoami` and runs `wrangler login` when needed. In CI it uses
   `CLOUDFLARE_API_TOKEN` (and `CLOUDFLARE_ACCOUNT_ID`) instead.
3. Picks the account: `CLOUDFLARE_ACCOUNT_ID`, the one used last time, the only one, or (when
   your login can reach several) asks once. Every later step runs against that account.
4. Creates (or reuses) the R2 bucket `<worker>-files` and binds it as `R2`, so big files spill
   to R2 automatically, the OpenQuick way. If R2 isn't enabled on the account (a one-time
   opt-in in the dashboard; 10 GB are free), it says so and continues without it; the next run
   tries again. `--no-r2` turns this off for good.
5. Works out the secrets: what the Worker already has is kept, what is missing is generated
   - `BACKYARD_SECRET`: 32 random bytes, created once. Never printed.
   - `DEPLOY_TOKEN`: `byd_…`, created once and remembered.

   Only wrangler's explicit "Worker not found" counts as a first deploy. If the secrets can't
   be listed (an authentication, permission or network problem, or output that isn't the
   JSON list), setup stops and changes nothing: a new `BACKYARD_SECRET` would revoke every
   API token.
6. Runs `wrangler deploy` with the new secrets attached (`--secrets-file`, a private 0600
   temporary file removed right after), so the first version never runs without
   `BACKYARD_SECRET`. On the first deploy this creates the Durable Object classes (SQLite
   storage, migration `v1`).
7. Waits for `/__backyard/readyz` and prints the hub URL and the `backyard login` command.
   The token is shown only on an interactive terminal. In CI (`CI=true`) or when the output
   is redirected, it is masked (`byd_…wxyz`) and the command refers to the state file (or
   to `$DEPLOY_TOKEN` when you passed one).

State (the deploy token, URL, account, R2 choice and `--var` overrides) is saved to
`~/.config/backyard/cloudflare/<worker>.json` (mode 0600, honors `XDG_CONFIG_HOME`), so a plain
`npm run setup` keeps every earlier choice. A state file that isn't valid JSON stops the setup
rather than silently issuing a new deploy token.

| Option | Effect |
| --- | --- |
| `--worker-name <name>` | Deploy under another Worker name (default `backyard`). Use it to run several platforms. |
| `--r2-bucket <name>` | Use this R2 bucket instead of `<worker>-files`. |
| `--no-r2` / `--r2` | Never use R2 (remembered; files are capped at 25 MiB) / use it again. |
| `--var KEY:VALUE` | Override a variable from `wrangler.jsonc` (repeatable, remembered). `--var KEY:` drops an override. |
| `--rotate-token` | Issue a new `DEPLOY_TOKEN`. The old one stops working. |
| `--rotate-secret` | Replace `BACKYARD_SECRET`. This revokes every API token and anonymous identity. |
| `--skip-deploy` | Only (re)set the secrets. |

Environment: `DEPLOY_TOKEN=…` sets a token of your choice. `CLOUDFLARE_API_TOKEN` and
`CLOUDFLARE_ACCOUNT_ID` enable non-interactive use (a login that reaches several accounts
needs `CLOUDFLARE_ACCOUNT_ID` when there is no terminal to ask in).

The setup script itself is tested offline against a fake `wrangler`
(`npm run test:scripts`).

## Free-plan limits and Backyard's guardrails

The Cloudflare free-plan quotas that matter most are listed below, as of this writing. Check
[Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/) for current numbers.

| Resource | Free plan | What it means for Backyard |
| --- | --- | --- |
| Worker requests | 100,000 / day | Each page view, API call and deploy request counts. |
| Worker CPU time | 10 ms / request | Static serving and API calls fit easily. Durable Objects do the heavy lifting. |
| Durable Object requests | 100,000 / day | Roughly one per API call or static file served. Websocket messages bill at 20:1. |
| Durable Object SQLite storage | 5 GB total, 100,000 rows written / day | Site files and documents live here. |
| Request body size | 100 MB | This is why files are capped at 95 MiB with R2. |
| Workers AI | 10,000 neurons / day | Enough for a few hundred small chats a day. |
| R2 (optional) | 10 GB, 1M writes and 10M reads / month, free egress | Holds files larger than `FILE_INLINE_MAX`. |
| Cloudflare Access (optional) | 50 users | Login with Okta, Entra ID, Google, GitHub, one-time PIN… |

When a quota runs out, Cloudflare returns errors until the daily reset (00:00 UTC). Backyard
adds its own **guardrails**, so one person or one runaway page can't drain the account (and
page requests for site names that were never deployed are answered without creating a Durable
Object, so probing random names costs no storage writes). The
guardrails are daily counters per user (per IP when anonymous) plus platform-wide caps, kept
in `RegistryDO`. When a cap is hit, the API answers `429` with `Retry-After` (seconds until
00:00 UTC). Tune them with the `LIMITS` variable (JSON, merged over the defaults; `0` means
unlimited for the daily counters):

```sh
npm run setup -- --var 'LIMITS:{"ai_per_day":50,"max_sites":100}'
```

| `LIMITS` key | Default | Meaning |
| --- | --- | --- |
| `max_sites` | 500 | Sites on the platform (redeploying an existing site is always allowed). |
| `max_site_bytes` | 200 MiB | Total size of one site's files. |
| `max_files` | 2000 | Files per site. |
| `max_file_bytes` | 95 MiB | Bytes per deployed file. The hard cap is 25 MiB without R2 and 95 MiB with R2. |
| `max_upload_bytes` | 95 MiB | Bytes per uploaded file (`backyard.files.upload`). Same hard caps. |
| `max_doc_bytes` | 256 KiB | JSON size of one document. |
| `max_docs_per_site` | 100,000 | Documents per site. |
| `max_list` | 1000 | Largest `limit` of a query. |
| `ai_per_day` | 200 | AI calls per user/IP per day. |
| `ai_images_per_day` | 30 | Generated images per user/IP per day. |
| `ai_platform_per_day` | 2000 | AI calls per day, whole platform. This guards your neuron allowance. |
| `ai_images_platform_per_day` | 300 | Generated images per day, whole platform. |
| `ai_max_tokens` | 4096 | Upper bound for `max_tokens` of a chat. |
| `deploys_per_day` | 200 | Deploys per user/IP per day. |
| `uploads_per_day` | 500 | File uploads per user/IP per day. |
| `db_writes_per_day` | 20,000 | Document writes per user/IP per site per day (counted in the site's Durable Object). |
| `ws_conns_per_user` | 50 | Concurrent websockets per user per site. |
| `ws_rooms_per_conn` / `ws_subs_per_conn` | 50 / 100 | Rooms joined and database subscriptions per connection. |
| `ws_messages_per_second` / `ws_message_burst` | 60 / 120 | Token bucket for client frames. |
| `ws_max_message_bytes` / `ws_max_presence_bytes` | 64 KiB / 8 KiB | Frame and presence-state size. |

## URL modes

| Mode | Hub | Sites | Needs |
| --- | --- | --- | --- |
| **path** (default) | `https://backyard.<you>.workers.dev/` | `…/s/<site>/` | Nothing |
| path + custom domain | `https://backyard.example.com/` | `https://backyard.example.com/s/<site>/` | A zone on Cloudflare (Workers custom domain, free) |
| **host** | `https://backyard.example.com/` | `https://<site>.backyard.example.com/` | Wildcard DNS + Worker routes, and a certificate that covers the wildcard |

Path mode works everywhere, but all sites share one origin with each other **and with the
hub**. They share `localStorage`, cookies and service workers, and a page on any site can call
the hub API as its visitor (list sites, mint the visitor an API token). Use it for a team that
trusts every site author. Host mode gives every site its own origin.

Universal SSL (free) covers `example.com` and `*.example.com`, which is **one** wildcard level.
For sites at `<site>.example.com`, use the zone apex as `BASE_DOMAIN`. For
`<site>.backyard.example.com`, you need an Advanced Certificate (Advanced Certificate
Manager, paid). The Terraform module can order one (`advanced_certificate = true`).

Switch modes with variables (they're remembered):

```sh
npm run setup -- --var URL_MODE:host --var BASE_DOMAIN:example.com
```

The routes and DNS records for host mode come from
[`deploy/terraform/cloudflare`](../terraform/cloudflare). Or add them by hand: a proxied
`AAAA *.example.com → 100::` record, a proxied record for the hub host, and Worker routes
`example.com/*` and `*.example.com/*` pointing to the `backyard` Worker. `wrangler deploy` leaves
routes alone as long as `wrangler.jsonc` has no `routes`.

## Custom domain and Cloudflare Access

Cloudflare Access (Zero Trust, free for up to 50 users) puts your company login in front of the
hub and every site. Access handles the identity provider (Okta, Microsoft Entra ID, Google
Workspace, GitHub, SAML/OIDC, or one-time PIN by email). The Worker (`AUTH_MODE=access`) verifies
the `Cf-Access-Jwt-Assertion` of every request against your team's signing keys and the
application's AUD tag. Because of that check, the Worker stays protected even if someone
reaches it through `workers.dev`. User names and groups come from Access' `get-identity`
endpoint, and `ADMIN_GROUPS` can name IdP groups.

The deploy gateway (`/__backyard/api/deploy/*`) and the health checks must **bypass** Access.
The CLI authenticates there with bearer tokens (`DEPLOY_TOKEN`, or API tokens minted in the
browser by `backyard login`), and upload URLs are signed. Everything else stays behind
Access.

### With Terraform (recommended)

[`deploy/terraform/cloudflare`](../terraform/cloudflare) creates the custom domain (path mode),
or the wildcard DNS and routes (host mode). It also creates the Access application, an allow
policy (emails, email domains, Access groups, Okta groups) and the bypass application.
Optionally it creates the Okta identity provider.

```sh
npm run setup                                   # 1. deploy the Worker (workers.dev)
cd ../terraform/cloudflare/examples/basic       # 2. domain + Access
cp terraform.tfvars.example terraform.tfvars    #    account_id, zone_name, base_domain, who may sign in
export CLOUDFLARE_API_TOKEN=…                   #    Zone: DNS Edit, Workers Routes Edit (+ SSL and Certificates Edit
                                                #    for advanced_certificate); Account: Workers Scripts Edit,
                                                #    Access: Apps and Policies Edit, Access: Organizations,
                                                #    Identity Providers, and Groups Edit (Read is enough without Okta)
terraform init && terraform apply
terraform output -raw setup_command             # 3. switch the Worker to the domain + Access
# → npm run setup -- --worker-name backyard --var ACCESS_AUD:… --var ACCESS_TEAM_DOMAIN:acme.cloudflareaccess.com
#                   --var AUTH_MODE:access --var BASE_DOMAIN:backyard.example.com --var URL_MODE:path
cd ../../../../cloudflare && <run that command>
backyard login --server https://backyard.example.com   # browser login through Access
```

### Okta

1. In Okta, go to **Applications → Create App Integration** and pick **OIDC, Web Application**.
   - Sign-in redirect URI: `https://<team>.cloudflareaccess.com/cdn-cgi/access/callback`
   - Groups claim (**Sign On** tab → OpenID Connect ID Token): name `groups`, filter
     *Matches regex* `.*`. Then Access receives the user's groups.
   - Assign the people or groups who should use Backyard.
2. Pass the app's credentials to Terraform, and allow groups:

   ```hcl
   okta_account_url    = "https://acme.okta.com"
   okta_client_id      = "0oa…"
   okta_client_secret  = "…"                  # or TF_VAR_okta_client_secret
   allowed_okta_groups = ["backyard-users"]
   ```

   Because the Okta provider is the only allowed login method, the module skips the Access
   login page (`auto_redirect_to_identity`).
3. Optional: make an Okta group admin of Backyard (admins can delete any site):
   `npm run setup -- --var ADMIN_GROUPS:backyard-admins`

Entra ID, Google Workspace and the others work the same way. Add the identity provider in
**Zero Trust → Settings → Authentication** (or with Terraform), then pass its id in
`identity_provider_ids`. To allow its groups, create an Access group and pass it in
`allowed_access_group_ids`.

### By hand (dashboard)

1. **Workers & Pages → backyard → Settings → Domains & Routes**: add a custom domain, for example
   `backyard.example.com` (path mode).
2. **Zero Trust → Access → Applications → Add → Self-hosted**: application domain
   `backyard.example.com` (in host mode, also `*.backyard.example.com`), with a policy that
   allows your users. Copy the **Application Audience (AUD) tag**.
3. Add a second self-hosted application for `backyard.example.com/__backyard/api/deploy`,
   `…/__backyard/api/deploy/*`, `…/__backyard/healthz` and `…/__backyard/readyz`, with a
   **Bypass** policy that includes *Everyone*.
4. `npm run setup -- --var AUTH_MODE:access --var ACCESS_TEAM_DOMAIN:<team> --var ACCESS_AUD:<aud> --var BASE_DOMAIN:backyard.example.com`

Optional: once the custom domain works, set `"workers_dev": false` in `wrangler.jsonc` to turn
off the `workers.dev` URL.

## AI

`backyard.ai.chat`, `ai.image` and `ai.embed` go through the Worker. Sites name **aliases**,
never provider model ids. The defaults run on Workers AI, which needs no API key and is
covered by the free daily allowance:

| Alias | Kind | Model |
| --- | --- | --- |
| `smart` (default chat) | chat | `@cf/google/gemma-4-26b-a4b-it` |
| `fast` | chat | `@cf/meta/llama-3.1-8b-instruct-fp8` |
| `image` | image | `@cf/black-forest-labs/flux-1-schnell` |
| `embed` | embedding | `@cf/baai/bge-base-en-v1.5` (768 dimensions) |

Define your own aliases with the `AI_MODELS` variable (JSON; it replaces the defaults). An alias
can use Workers AI, Anthropic, or any OpenAI-compatible API:

```jsonc
// wrangler.jsonc → vars.AI_MODELS (one line in practice), or npm run setup -- --var 'AI_MODELS:{…}'
{
  "smart":  { "provider": "workers-ai", "model": "@cf/google/gemma-4-26b-a4b-it", "kind": "chat" },
  "claude": { "provider": "anthropic",  "model": "claude-opus-5-5", "kind": "chat",
              "description": "Claude for hard problems", "max_tokens": 8192, "effort": "high" },
  "gpt":    { "provider": "openai",     "model": "gpt-5-mini", "kind": "chat" },
  "image":  { "provider": "workers-ai", "model": "@cf/black-forest-labs/flux-1-schnell", "kind": "image" },
  "embed":  { "provider": "workers-ai", "model": "@cf/baai/bge-base-en-v1.5", "kind": "embedding" }
}
```

- API keys are **secrets**: `npx wrangler secret put ANTHROPIC_API_KEY` and
  `npx wrangler secret put OPENAI_API_KEY`. `GET /__backyard/api/ai/models` lists only the
  aliases whose provider is usable (AI binding present, key set).
- `OPENAI_BASE_URL` points the `openai` provider at any OpenAI-compatible endpoint. A
  non-default base URL works without a key.
- Defaults: `AI_DEFAULT_MODEL` (chat), `AI_DEFAULT_IMAGE_MODEL`, `AI_DEFAULT_EMBEDDING_MODEL`.
  Without them, Backyard picks `default`/`smart`/`chat`/`fast`, then `image`, then `embed`.
- `AI_GATEWAY_ID` routes Workers AI calls through [AI Gateway](https://developers.cloudflare.com/ai-gateway/)
  for caching and analytics. `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL` can point at a
  gateway URL too.
- `AI_ALLOW_RAW_MODELS=true` also accepts `@cf/…` and `provider/model` names from sites.
  `AI_EXPOSE_MODEL_IDS=true` shows model ids in `/ai/models`. `AI_SYSTEM_PROMPT` is prepended
  to every chat.
- Every AI call counts against `ai_per_day` and `ai_platform_per_day`. Images also count
  against the image caps.

## Configuration reference

Set variables in `wrangler.jsonc` → `vars`, with `npm run setup -- --var KEY:VALUE` (remembered),
or with `wrangler deploy --var KEY:VALUE` (that deploy only). Secrets go through
`npx wrangler secret put NAME`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `BASE_DOMAIN` | request host | Hub host. Required for host mode and custom domains. |
| `PUBLIC_URL` | `https://<BASE_DOMAIN>` or request origin | Absolute hub URL (origin only). |
| `URL_MODE` | `path` | `path` or `host` (host needs `BASE_DOMAIN`). |
| `AUTH_MODE` | `none` | `none` (anonymous cookies) or `access` (Cloudflare Access). |
| `ACCESS_TEAM_DOMAIN` | — | `acme` or `acme.cloudflareaccess.com`. |
| `ACCESS_AUD` | — | Application AUD tag (comma-separated for several). |
| `ACCESS_GET_IDENTITY` | `true` | Fetch name and groups from Access `get-identity`. |
| `ALLOW_ANONYMOUS_DEPLOY` | `false` | `true` lets anyone deploy without a token. Fine for a playground; `LIMITS` still apply. |
| `ADMINS` / `ADMIN_GROUPS` | — | Comma-separated admin emails and IdP group names. |
| `OWNERSHIP` | `none` | `none` (anyone may overwrite a site, the Quick way) or `owner` (only owners and admins). |
| `SERVER_NAME` | `Backyard` | Name shown in the hub. |
| `RESERVED_SITES` | — | Extra reserved site names (comma-separated). |
| `LIMITS` | see above | Guardrails (JSON). |
| `FILE_INLINE_MAX` | `5242880` | Files above this size go to R2 when bound (max 25 MiB). |
| `TOKEN_TTL_HOURS` | `720` | Longest lifetime of an API token. |
| `AI_*`, `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL` | — | See [AI](#ai). |
| `WAREHOUSE_URL` | — | Optional: proxy `/warehouse/*` to a Backyard Go server's site API (secret `WAREHOUSE_TOKEN`). |

| Secret | Meaning |
| --- | --- |
| `BACKYARD_SECRET` | At least 32 characters. Signs anonymous identities, API tokens, deploy ids and upload URLs. The keys are derived like the Go server's (HKDF-SHA256), so the two editions interoperate. |
| `DEPLOY_TOKEN` | Static bearer token with admin rights for the CLI and CI (deploy, list, delete). Like the Go server's static tokens, it can't mint API tokens. |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` | Optional AI providers. |

Configuration problems (bad JSON, unknown `LIMITS` keys, missing Access settings) are logged.
Watch them with `npx wrangler tail`.

## Deploying from GitHub Actions

The repository ships `.github/workflows/cloudflare-deploy.yml`, which tests, deploys and uploads
secrets from repository secrets. To deploy from your own pipeline instead, here's the minimal
version (the secrets `CLOUDFLARE_API_TOKEN` — "Edit Cloudflare Workers" template —
`CLOUDFLARE_ACCOUNT_ID`, `BACKYARD_SECRET` and `DEPLOY_TOKEN`):

```yaml
name: Deploy Backyard to Cloudflare
on:
  workflow_dispatch:
  push:
    branches: [main]
    paths: [deploy/cloudflare/**, web/sdk/**, web/hub/**, skills/**]

jobs:
  deploy:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: deploy/cloudflare
    env:
      CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
      CLOUDFLARE_ACCOUNT_ID: ${{ secrets.CLOUDFLARE_ACCOUNT_ID }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
        with:
          node-version: 22
      - run: npm ci
      - run: npm test
      # Bundles the SDK/hub/docs and deploys. Variables for this deploy: --var KEY:VALUE.
      - run: npm run deploy:ci -- --var AUTH_MODE:none
      - name: Secrets
        env:
          BACKYARD_SECRET: ${{ secrets.BACKYARD_SECRET }}
          DEPLOY_TOKEN: ${{ secrets.DEPLOY_TOKEN }}
        run: |
          umask 077
          node -e 'const e=process.env;require("fs").writeFileSync("secrets.json",JSON.stringify({BACKYARD_SECRET:e.BACKYARD_SECRET,DEPLOY_TOKEN:e.DEPLOY_TOKEN}))'
          npx wrangler secret bulk secrets.json
          rm -f secrets.json
```

`npm run setup` also works in CI. It reads `CLOUDFLARE_API_TOKEN` and `DEPLOY_TOKEN` from the
environment. Its state file lives on the runner, though, so pin the secrets as shown above.

## Local development and tests

```sh
npm ci
npm test             # vitest inside workerd (@cloudflare/vitest-plugin) + offline tests of scripts/setup.mjs;
                     # no Cloudflare account needed (test:worker / test:scripts run one half)
npm run typecheck    # tsc --noEmit (strict)
npm run dev:local    # wrangler dev --env local: no login needed; Workers AI off (AI answers 501), R2 simulated
npm run dev          # wrangler dev with the real Workers AI binding (needs `wrangler login`)
npm run build        # bundle check (wrangler deploy --dry-run)
```

The Worker bundles the SDK, hub UI and agent docs from `../../web` and `../../skills`, so build
and deploy from a complete checkout of the repository. `npm run build`, `npm run deploy`,
`npm run setup` and wrangler's own build step for `wrangler deploy` / `versions upload` (also a
plain `npx wrangler deploy`) stop with a list of what is missing instead of shipping
placeholders; `npm run sync`, `npm test` and `wrangler dev` use placeholders with a warning.
`BACKYARD_REPO_ROOT` points the sync at another checkout.

`npm test` also runs the shared query-semantics cases of SPEC §5.2
(`internal/docstore/conformance/querycases.json`, the same file the Go backends run, copied to
`test/generated/` by `npm run sync`) against the Worker over HTTP.

`wrangler dev` reads `.dev.vars` for secrets, for example:

```sh
printf 'BACKYARD_SECRET=%s\nDEPLOY_TOKEN=dev-token\n' "$(openssl rand -hex 32)" > .dev.vars
npm run dev:local -- --port 8787
backyard login --server http://localhost:8787 --token dev-token
```

`src/generated/` and `test/generated/` are produced by `npm run sync` (it runs automatically
before `dev`, `test`, `typecheck`, `deploy`, and as wrangler's build step). Don't edit them.

Layout: `src/index.ts` (entry), `router.ts` (URLs), `hub.ts` (hub, CLI login, tokens), `site-api.ts`
(site API), `gateway.ts` (deploy protocol), `site-do.ts` / `registry-do.ts` (Durable Objects),
`db/` (document query engine), `ai/` (providers), `auth.ts` / `jwt.ts` / `crypto.ts` (identity,
tokens, keys).

## Differences from the Go edition

| Area | Go server | Cloudflare edition |
| --- | --- | --- |
| Sign-in | `none`, OIDC (Okta, Entra ID, Google…), IAP, ALB, trusted headers, several static tokens | `none` (anonymous cookies) or Cloudflare Access (any IdP Access supports, SAML included). One static `DEPLOY_TOKEN`. API tokens and the CLI browser login are identical. |
| Warehouse | BigQuery, Athena, Postgres | `501 unsupported`, unless `WAREHOUSE_URL` proxies a Go server. |
| AI providers | Anthropic, OpenAI, Bedrock, Gemini | Workers AI (default, free), Anthropic, and OpenAI-compatible APIs. |
| Rate limits | Per-minute token buckets plus quotas | Daily caps per user/IP and platform-wide (`LIMITS`). Document write caps are counted in each site's Durable Object and reset if it's evicted. |
| File size | Configurable | 25 MiB per file without R2, 95 MiB with R2 (100 MB request limit). |
| Deploy methods | `api`, plus direct sync (`s3`, `gcs`, `azure`, `rsync`) | `api` only (the HTTP gateway protocol). `/__backyard/api/info` reports `"edition": "cloudflare"`. |
| Realtime | In memory, or Redis/Postgres pub/sub across replicas | Each site's websocket hub runs in its own Durable Object (hibernation). Same protocol. The server sends no keep-alive pings (they would wake hibernated objects): the SDK's `{"t":"ping"}` every 25 s is answered with `{"t":"pong"}` by the runtime without waking the object, and closed connections are detected by the runtime. |
| Uploads | Buffered by the server | Streamed: the site's Durable Object parses the multipart body and writes the file to SQLite or, above `FILE_INLINE_MAX`, to R2 in 10 MiB parts, so neither memory nor the Worker's CPU limit caps uploads below the file limits. |
| Hub UI, SDK, docs | Embedded in the binary | Bundled at deploy time from `web/` and `skills/`. Redeploy to update. |
| Site owners endpoint | Supported | Supported when `OWNERSHIP=owner`, otherwise `501`. |

Everything a site or agent uses behaves the same on both editions: the `backyard.js` SDK and its
preamble, `/__backyard/api/*` (CSRF rule, error envelope, flattened documents, the full query
language), file uploads, AI streaming (SSE), the websocket protocol, the deploy gateway
protocol, API tokens (HS256; a token minted by one edition with the same secret verifies on the
other), `/llms.txt` and `/docs`.

## Upgrading, backups and removal

- **Upgrade:** `git pull && npm ci && npm run setup`. Secrets, the deploy token and `--var`
  overrides are kept. Durable Object data survives deploys.
- **Back up a site:** `backyard pull <site> ./backup` downloads its files. To export
  documents, query them through the site API (`/__backyard/api/db/<collection>`).
- **Remove:** `npx wrangler delete --name backyard`. This **permanently deletes** every site and
  document. Then remove the R2 bucket (`npx wrangler r2 bucket delete backyard-files`) and run
  `terraform destroy` if you used the module.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `register a workers.dev subdomain first` | Open **Workers & Pages** in the dashboard once, pick a subdomain, and re-run `npm run setup`. |
| `503` "BACKYARD_SECRET is not set" | `npm run setup -- --skip-deploy`, or `npx wrangler secret put BACKYARD_SECRET` (≥ 32 chars). |
| `npm run dev`: "Failed to start the remote proxy session" | Workers AI only runs remotely. Run `npx wrangler login`, or use `npm run dev:local`. |
| `backyard login`/`deploy` gets an HTML login page or `302` | Access is protecting the deploy gateway. Add the bypass application (the Terraform module does). |
| `401` "Cloudflare Access token" errors in the browser | `ACCESS_AUD` or `ACCESS_TEAM_DOMAIN` don't match the Access application (`terraform output`). |
| `413` on deploy | A file is above the per-file cap (25 MiB without R2): enable R2 on the account and re-run `npm run setup`. Or a guardrail (`max_sites`, `max_site_bytes`, `max_files`) was hit. |
| `429` with `Retry-After` | A daily guardrail was hit. Raise it with `LIMITS`, or wait until 00:00 UTC. |
| R2 bucket could not be created | Enable R2 once in the dashboard (it's free, but requires opting in), then re-run `npm run setup` (add `--r2` if you turned it off with `--no-r2`). |
| `npm run setup` asks which account | Your login reaches several accounts. Answer once (it's remembered), or set `CLOUDFLARE_ACCOUNT_ID`. |
| Anything else | `npx wrangler tail` streams the Worker's logs, including configuration problems. |
