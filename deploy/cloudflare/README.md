# Backyard on Cloudflare (Workers edition)

Run Backyard on your own Cloudflare account, on the **free plan**: one Worker plus
SQLite-backed Durable Objects, Workers AI, and an optional R2 bucket. You don't need a
server, a database, a credit card, a checkout of this repository or Terraform.

This edition implements the same public contract as the Go server (see `docs/spec/SPEC.md`
§2–§8 and §16): the same URLs, `backyard.js` SDK, site API, websocket protocol, deploy gateway
and API tokens. The `backyard` CLI, the Claude skill and the MCP server work with it
unchanged. The idea comes from [OpenQuick](https://github.com/topmass/openquick): one command
deploys straight to Cloudflare in a couple of minutes.

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
- [What `setup` does](#what-setup-does)
- [Your own domain](#your-own-domain)
- [Company login (Cloudflare Access)](#company-login-cloudflare-access)
- [Use Backyard in Claude](#use-backyard-in-claude)
- [Command reference](#command-reference)
- [Credentials and permissions](#credentials-and-permissions)
- [State file](#state-file)
- [Environment variables](#environment-variables)
- [Free-plan limits and Backyard's guardrails](#free-plan-limits-and-backyards-guardrails)
- [URL modes](#url-modes)
- [AI](#ai)
- [Configuration reference](#configuration-reference)
- [With Terraform (optional, infrastructure as code)](#with-terraform-optional-infrastructure-as-code)
- [Deploying from GitHub Actions](#deploying-from-github-actions)
- [Local development and tests](#local-development-and-tests)
- [Building the npm package](#building-the-npm-package)
- [Differences from the Go edition](#differences-from-the-go-edition)
- [Upgrading, backups and removal](#upgrading-backups-and-removal)
- [Troubleshooting](#troubleshooting)

## Quickstart (free plan)

You need:

- a Cloudflare account. The free plan is enough, and the account doesn't need a `workers.dev`
  subdomain yet: `setup` registers one from the terminal when it has none.
- Node.js 22 or later, with npm (wrangler 4 needs Node.js 22).

The engine that does the work is **`backyard-cloudflare`**: a small Node.js CLI with a
prebuilt Worker and one pinned dependency, wrangler. There are three ways to run it, and they
do exactly the same thing: `backyard cloudflare <command>`, `npx backyard-cloudflare@latest
<command>` and, in a checkout, `npm run cf -- <command>`. The examples in this guide use the
first.

> **Before the first release of Backyard** there is no `backyard-cloudflare` on npm and no
> release package for the `backyard` CLI to install. Until then, use a
> [checkout](#from-a-checkout), or build the package yourself with
> `make cloudflare-pack` ([Building the npm package](#building-the-npm-package)). Run
> `npx backyard-cloudflare` only once a Backyard release has published it to npm: until then
> npm serves nothing under that name, or something that is not Backyard.

### With the `backyard` CLI

```sh
curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh
backyard cloudflare setup
backyard init my-site && cd my-site
backyard deploy            # → https://backyard.<you>.workers.dev/s/my-site/
```

`backyard cloudflare setup` installs the `backyard-cloudflare` package of its own release into
your user cache (once, after checking its SHA-256 against the digest built into the CLI), runs
it, and then logs the CLI in to the new Worker with its deploy token, so `backyard deploy`
works right away.

- Engine options go straight after the command (`backyard cloudflare setup --worker-name team`).
  `backyard cloudflare setup --help` shows them together with the CLI's own flags
  ([docs/cli.md](../../docs/cli.md#backyard-cloudflare)); `backyard cloudflare setup -- --help`
  shows the engine's own help.
- When the Worker's URL changes (`domain add`, `domain remove`), the CLI's profiles move to the
  new URL; a current profile for another server stays the current one. While Access is on, a
  personal login (`backyard login --server <hub>`, with or without `--token`) is kept rather
  than replaced by the deploy token.
- A `backyard` built from source (`go install`, `make build`) has no package pinned: run it
  inside a checkout of this repository, where it uses `deploy/cloudflare` automatically, or
  point it at one with `--dir <checkout>/deploy/cloudflare`, or at a package with
  `--package <file or https URL>`, such as a release's
  `https://github.com/thasami/backyard/releases/download/v<version>/backyard-cloudflare-<version>.tgz`
  (not verified against a digest). Or install a release build, as above.

### With only Node.js

Once a Backyard release has published the package to npm:

```sh
npx backyard-cloudflare@latest setup
```

On an interactive terminal it ends with something like:

```
✔ Backyard is live at https://backyard.<you>.workers.dev

Publish in the browser: https://backyard.<you>.workers.dev/#token=byd_…   (the hub asks before using it)

Or connect the CLI (keep the token secret — it can deploy and delete sites):

  backyard login --server https://backyard.<you>.workers.dev --token byd_…
```

The browser link hands the deploy token to the hub, which removes it from the address bar and
asks before using it, so you can drop a folder on the hub without installing anything else.
For `backyard login`, `backyard init` and `backyard deploy`, install the CLI (setup prints this
line too): `curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh`.
Alternatives: `npm i -g backyard-cloudflare && backyard-cloudflare setup`, or a release
tarball, which works even when the package is not on npm:
`npx --package https://github.com/thasami/backyard/releases/download/v<version>/backyard-cloudflare-<version>.tgz backyard-cloudflare setup`.

### From a checkout

```sh
git clone https://github.com/thasami/backyard.git
cd backyard/deploy/cloudflare
npm ci
npm run setup                                # = node bin/backyard-cloudflare.mjs setup
npm run cf -- domain add backyard.acme.com   # every other command: npm run cf -- <command>
```

A checkout builds the Worker from source: it bundles the SDK, the hub UI and the agent docs of
the repository first. `make cloudflare-setup` (with `CF_SETUP_ARGS`) does the same from the
repository root, and prints its next steps as `npm --prefix deploy/cloudflare run cf -- …`,
which run from there too. This is the path for contributors, and the only one before the first
release.

### Then

By default (`AUTH_MODE=none`), anyone with a site's link can open it, the way Quick and
OpenQuick work. Visitors get an anonymous identity (a signed cookie). Deploying requires the
`DEPLOY_TOKEN` (share it with whoever publishes, or set `ALLOW_ANONYMOUS_DEPLOY=true` for a
playground). Each next step is one command, and each command checks its own result:

```sh
backyard cloudflare domain add backyard.acme.com                  # the hub on your own domain
backyard cloudflare access enable --allow-email-domain acme.com   # company login (Cloudflare Access)
backyard cloudflare status                                        # read-only live check
```

[Cloudflare Access](#company-login-cloudflare-access) limits who can **see** sites and gives
everyone a personal login and API tokens. It's free for up to 50 users.

## What `setup` does

Re-run it at any time to upgrade: every choice is kept (secrets, the deploy token, R2, AI, the
domain, Access and your `--var` overrides). Each run:

1. **Builds or finds the Worker.** From the npm package (or the CLI's release package) it
   deploys the prebuilt `dist/worker.js`, and every wrangler call runs in a private temporary
   directory that is removed afterwards, so a read-only global install or the npx cache works.
   In a checkout it first bundles `web/sdk`, `web/hub` and `skills/backyard` into
   `src/generated` and builds from source.
2. **Logs in.** It checks `wrangler whoami` and runs `wrangler login` (in the browser) when
   needed. In CI it uses `CLOUDFLARE_API_TOKEN` (and `CLOUDFLARE_ACCOUNT_ID`) instead. Without
   a terminal and without a login it stops with exit code 2.
3. **Picks the account:** `CLOUDFLARE_ACCOUNT_ID`, the one used last time, the only one, or
   (when your login can reach several) it asks once. Without a terminal, several accounts need
   `CLOUDFLARE_ACCOUNT_ID` (exit 2). Every later step runs against that account.
4. **Registers the workers.dev subdomain** when the account has none: it asks for a name
   (the default comes from the account name), or takes `--workers-subdomain <name>`, checks
   that it is free and registers it. An existing subdomain is used as it is and never renamed.
5. **Adopts the deployed configuration** when this machine has no saved state for the Worker
   but the Worker exists (another machine, a teammate, CI): the R2 bucket, AI off (a Worker
   deployed with `--no-ai`: no Workers AI binding and `AI_ENABLED=false`), the variables that
   differ from the defaults, the custom domain (`BASE_DOMAIN`, `URL_MODE`) and Cloudflare Access
   (`AUTH_MODE`, `ACCESS_TEAM_DOMAIN`, `ACCESS_AUD`). It prints one line, `Adopted the deployed
   configuration of <worker> (no setup state on this machine): …`, and the deploy keeps them.
6. **R2.** It creates (or reuses) the bucket `<worker>-files` and binds it as `R2`, so big
   files spill to R2 automatically. If R2 isn't enabled on the account (a one-time opt-in in
   the dashboard; 10 GB are free), it says so and continues without it; the next run tries
   again. `--no-r2` turns this off for good.
7. **Variables:** `wrangler.jsonc`'s defaults, then your remembered `--var` overrides, then the
   variables that [`domain add`](#your-own-domain),
   [`access enable`](#company-login-cloudflare-access) and [`setup --no-ai`](#turning-ai-off)
   manage. While they manage them, `--var BASE_DOMAIN:…` (and `URL_MODE`, `AUTH_MODE`,
   `ACCESS_*`, `AI_ENABLED`) exits 2 and names the command that changes them.
8. **Runs the auth guard.** A deploy that would turn a live Cloudflare Access off, switch its
   team or open a Worker that fails closed is refused (exit 3, nothing changed), and so is a
   deploy to an existing Worker whose auth mode cannot be read. The message names the cause and
   the remedy: if another machine enabled Access, delete this machine's state file to adopt the
   live configuration; if the change is deliberate, `setup --confirm-auth-change` or
   `access disable`.
9. **Works out the secrets:** what the Worker already has is kept, what is missing is generated.
   - `BACKYARD_SECRET`: 32 random bytes, created once. Never printed.
   - `DEPLOY_TOKEN`: `byd_…`, created once and remembered. A Worker that already has one keeps
     it, also when this machine has no saved state: pass `DEPLOY_TOKEN=…` to remember it here,
     or `--rotate-token` to replace it.

   Only wrangler's explicit "Worker not found" counts as a first deploy. If the secrets can't
   be listed (an authentication, permission or network problem, or output that isn't the
   JSON list), setup stops and changes nothing: a new `BACKYARD_SECRET` would revoke every
   API token.
10. **Deploys** with `wrangler deploy` and the new secrets attached (`--secrets-file`, a private
    0600 temporary file removed right after), so the first version never runs without
    `BACKYARD_SECRET`. On the first deploy this creates the Durable Object classes (SQLite
    storage, migration `v1`). The generated wrangler configuration never contains `routes`:
    deploys never change routes or custom domains (`routes` in `wrangler.jsonc` are ignored
    with a warning). It binds Workers AI as `AI`, unless AI is off (`--no-ai`, remembered).
11. **Waits** for `/__backyard/readyz` and prints the hub URL, the browser link and the
    `backyard login` command, and the next step (`domain add` or `access enable`). The token is
    shown only on an interactive terminal. In CI (`CI=true`) or when the output is redirected,
    it is masked (`byd_…wxyz`) and the command refers to the state file (or to `$DEPLOY_TOKEN`
    when you passed one).

`setup --dry-run` changes nothing and needs no login: it builds the effective configuration,
runs `wrangler deploy --dry-run` with it and prints the variables it would deploy. It works
offline.

Every command except `status` and `setup --dry-run` takes a per-Worker lock, so two runs for the
same Worker cannot interleave.

## Your own domain

The zone must be on Cloudflare in the same account (the free plan is enough). `domain add`
finds it from the host name (or takes `--zone`); a missing zone ends with a link to add it, a
zone whose nameservers are still being switched with a hint to finish that.

### The hub on your domain (path mode)

```sh
backyard cloudflare domain add backyard.acme.com
```

This needs nothing but your wrangler login and no dashboard:

1. It attaches `backyard.acme.com` to the Worker as a **Workers custom domain**: Cloudflare
   creates its DNS record and certificate. Before attaching anything it checks the host, and it
   stops with exit 3 (nothing changed) when the host already has DNS records (a website?) or is
   a custom domain of another Worker. Re-run with `--replace-dns-record` or `--take-over` if
   that is what you want. Other custom domains of this Worker are kept.
2. It deploys with `BASE_DOMAIN=backyard.acme.com` and `URL_MODE=path`.
3. It waits (up to `--wait`, default 300 seconds) until `https://backyard.acme.com/__backyard/readyz`
   answers. If the certificate or DNS is still provisioning it ends with exit 4: check later
   with `backyard cloudflare status`.

Sites are then at `https://backyard.acme.com/s/<site>/`. The `workers.dev` URL keeps working
(`setup --no-workers-dev` turns it off once the domain works), and the `backyard` CLI moves
its profiles from the old URL to the new one.

### Sites on their own hosts (host mode)

```sh
backyard cloudflare domain add acme-sites.dev --host-mode     # sites at https://<site>.acme-sites.dev/
```

Host mode gives every site its own origin (see [URL modes](#url-modes)). On top of path mode,
`domain add --host-mode`:

1. adds the route `*.acme-sites.dev/*` to the Worker (a route of another Worker stops it with
   exit 3, unless `--take-over`);
2. makes sure the proxied wildcard record `AAAA *.acme-sites.dev → 100::` exists. Your wrangler
   login cannot create DNS records, so either export `BACKYARD_CF_API_TOKEN`, an API token with
   **Zone · DNS · Edit** for that zone, and Backyard creates the record, or add it in the
   dashboard from the values it prints:

   ```
   Type AAAA · Name * · IPv6 address 100:: · Proxy status Proxied
   ```

   (`Name` is `*` at the zone apex, `*.<sub>` below it.) Backyard detects the record itself with
   DNS-over-HTTPS: on a terminal it checks every 5 seconds until `--wait`, without a terminal
   it checks once. A record that already resolves is left alone; with the token, an existing
   `*.<host>` record that is not proxied stops it with exit 1;
3. checks that a site host answers over TLS.

Until DNS and the certificate are ready, the Worker runs in path mode and the command ends with
exit 4: the hub already works on `https://acme-sites.dev/`, and sites switch to
`https://<site>.acme-sites.dev/` when you re-run `backyard cloudflare domain add acme-sites.dev --host-mode`.

**Certificates.** Universal SSL (free) covers `<zone>` and `*.<zone>`, one wildcard level. Host
mode at a zone apex (a dedicated zone such as `acme-sites.dev`) therefore works for free. Below
the apex (`sites.acme.com`) the sites need an Advanced Certificate (Advanced Certificate
Manager, paid: SSL/TLS → Edge Certificates → Order, or the Terraform module's
`advanced_certificate`). At an apex, on the other hand, the route `*.<zone>/*` sends every
proxied host name of the zone (`www`, `api`, …) to Backyard, so prefer a zone of its own.
`domain add --host-mode` prints the warning that applies and asks once per host (`--yes`
without a terminal); re-runs repeat the warning without asking again. Answering no changes
nothing (exit 3).

Switching the same host back to path mode (`domain add <host>` without `--host-mode`) deploys
first, then removes the route and the DNS record this tool created (the record only with
`BACKYARD_CF_API_TOKEN`; otherwise it prints the record to delete). Re-running `domain add` with
the same host and mode changes nothing in Cloudflare: it redeploys and checks everything again.

### Back to workers.dev

```sh
backyard cloudflare domain remove
```

It asks first (`--yes` without a terminal; answering no changes nothing, exit 3), deploys
without `BASE_DOMAIN` and `URL_MODE` so that links switch back right away, then removes the
route (host mode), the DNS record (only one this tool created, and only with
`BACKYARD_CF_API_TOKEN`; otherwise it prints the record to delete), and detaches the custom
domain. It never touches routes, custom domains or DNS records of other
Workers. A route or custom domain it cannot remove is printed as a dashboard step, with exit 4.
It is refused (exit 3) while Cloudflare Access is on (`access disable` first) and while
workers.dev is off (`setup --workers-dev` first), since the hub would have no address left.

A Worker has one domain at a time: to move it, `domain remove`, then `domain add` the new host.
While Access is on, `domain add` only re-checks the same host and mode.

## Company login (Cloudflare Access)

Cloudflare Access (Zero Trust, free for up to 50 users) puts your company login in front of the
hub and every site. Access handles the identity provider (Okta, Microsoft Entra ID, Google
Workspace, GitHub, SAML/OIDC, or one-time PIN by email). The Worker (`AUTH_MODE=access`) verifies
the `Cf-Access-Jwt-Assertion` of every request against your team's signing keys and the
application's AUD tag. Because of that check, the Worker stays protected even if someone
reaches it through `workers.dev` (it answers `401` there). User names and groups come from
Access' `get-identity` endpoint, and `ADMIN_GROUPS` can name IdP groups.

The deploy gateway (`/__backyard/api/deploy` and `/__backyard/api/deploy/*`), token minting
(`/__backyard/api/tokens`, for `backyard token create`), the health checks
(`/__backyard/healthz`, `/__backyard/readyz`) and the machine paths of the
[Claude connector](#use-backyard-in-claude) (`/__backyard/mcp`, `/__backyard/oauth/*`,
`/.well-known/oauth-authorization-server` and
`/.well-known/oauth-protected-resource/__backyard/mcp`, which answer `404` until
`MCP_ENABLED` is on) must **bypass** Access. The CLI authenticates there
with bearer tokens (`DEPLOY_TOKEN`, or API tokens minted in the browser by `backyard login`), and
upload URLs are signed. A token minted with an API token never expires later than that token: a
token cannot renew itself, so chaining tokens never keeps access past the lifetime of the token
that `backyard login` minted through Access. The hub's own requests to these paths carry the
`CF_Authorization` cookie, whose Access JWT the Worker verifies itself; if a browser sends
cookies of two different users (in host mode a site's page could plant its author's), the
Worker refuses the request (`401`) instead of picking one. The connector's paths accept only
the connector's own tokens and never read cookies or Access headers. Everything else stays
behind Access, `/__backyard/auth/authorize` (the connector's sign-in) included, and so does
`/.well-known` on site hosts: the bypass names the hub host only.

`backyard login` opens `/__backyard/auth/cli` in the browser. As on the Go server, a request
for it that another site may have started (`Sec-Fetch-Site` other than `none` or `same-origin`,
or a foreign `Referer`) gets a confirmation page first: the token is handed to the CLI's
loopback address only after you click **Allow**.

### Before you start

- A [custom domain](#your-own-domain): Access protects your own host name, not `workers.dev`.
- Zero Trust set up on the account, once: open <https://one.dash.cloudflare.com/>, pick a team
  name and the Free plan. With an API token, `access enable` checks this and stops with exit 2
  if it is missing; the guided steps remind you of it.

```sh
backyard cloudflare access enable --allow-email-domain acme.com
```

Access needs two self-hosted applications: **Backyard (&lt;worker&gt;)** for people, with an
allow policy (who may sign in: `--allow-email`, `--allow-email-domain`, both repeatable), and
**Backyard (&lt;worker&gt;) machine endpoints** with a Bypass policy for the nine paths above.
`access enable` creates them with an API token, or guides you through the dashboard; either
way it verifies the result itself, so nothing has to be copied by hand.

### Automatic, with an API token

Your wrangler login has no Access permissions. Create an API token (My Profile → API Tokens →
Create Custom Token) with **Account · Access: Apps and Policies · Edit** and **Account · Access:
Organizations, Identity Providers, and Groups · Read**, for this one account and with a short
TTL, and export it for this one command:

```sh
BACKYARD_CF_API_TOKEN=… backyard cloudflare access enable --allow-email-domain acme.com
```

On a terminal without that variable, `access enable` offers to do it in the dashboard (guided)
or to paste a token (hidden input). The token stays in memory: it is never stored, printed or
passed to wrangler. Backyard then creates the allow policy, the bypass policy, the bypass
application and the hub application, in that order, saving each id as it goes, so a re-run after
a failure resumes without duplicates. An Access application it did not create that already
covers the host stops it with exit 3 and nothing changed; the message names that application's
AUD, which you can use with `--team` and `--aud` (below), or delete the application.

When `CLOUDFLARE_API_TOKEN` is set (wrangler's own token) it is tried as well; if it lacks the
Access permissions, `access enable` warns and falls back to the guided steps. A
`BACKYARD_CF_API_TOKEN` without them stops with exit 1 and names them.

### Guided, in the dashboard

Without a token, `access enable` prints the two applications with their exact values:

```
Cloudflare Access needs two self-hosted applications
(https://one.dash.cloudflare.com/ → Access → Applications → Add an application → Self-hosted):

  1. Backyard (backyard)
     Public hostname:   backyard.acme.com
     Policy:            Allow · Include · Emails ending in @acme.com
  2. Backyard (backyard) machine endpoints   (paths the Worker authenticates itself)
     Public hostnames:  backyard.acme.com/__backyard/api/deploy
                        backyard.acme.com/__backyard/api/deploy/*
                        backyard.acme.com/__backyard/api/tokens
                        backyard.acme.com/__backyard/healthz
                        backyard.acme.com/__backyard/readyz
                        backyard.acme.com/__backyard/mcp
                        backyard.acme.com/__backyard/oauth/*
                        backyard.acme.com/.well-known/oauth-authorization-server
                        backyard.acme.com/.well-known/oauth-protected-resource/__backyard/mcp
     Policy:            Bypass · Include · Everyone
```

(In host mode the first application also lists `*.<host>`.) On a terminal it then watches
`https://<host>/` until Access answers (up to `--wait`, 900 seconds here), shows the team and
the AUD it found and asks whether to use them; `--yes` does not answer that question. Without a
terminal it prints the steps and exits 2; if Access is already in front of the host, the message
names the `--team` and `--aud` to pass.

Applications you already have (made in the dashboard or by other tooling) are used with
`backyard cloudflare access enable --team acme.cloudflareaccess.com --aud <AUD tag>`.

### What happens next

1. **Edge verification**, before the Worker changes: the team's signing keys are published,
   `https://<host>/` redirects to the Access login of the expected team and application (in
   host mode a site host too), and each of the nine paths reaches the Worker (the four of the
   Claude connector whether or not `MCP_ENABLED` is on). Any failure ends with exit 4 and a
   precise message (for example "the bypass application is missing
   backyard.acme.com/__backyard/api/tokens"), and the Worker is unchanged. Re-run
   `access enable` once it is fixed.
2. It deploys with `AUTH_MODE=access`, `ACCESS_TEAM_DOMAIN` and `ACCESS_AUD`.
3. **Post-checks:** `/__backyard/api/deploy/info` reports Access, `workers.dev` answers `401`,
   and the deploy token (when this machine knows it) still reaches the deploy gateway. A
   failure here is exit 4 ("applied, but …").
4. Teammates sign in with `backyard login --server https://backyard.acme.com` (the browser
   opens your company login).

Re-runs: the same flags change nothing in Access and verify again; new `--allow-email` or
`--allow-email-domain` flags replace the allow list with one policy update, which needs an API
token with the Access permissions again (`BACKYARD_CF_API_TOKEN`, or pasted on a terminal):
without one the command exits 2 and changes nothing.
An update changes only the fields the CLI manages: what you set in the dashboard besides them
(require and exclude rules, the allowed identity providers, more policies on the
applications, custom pages, …) is kept. Without flags a
re-run keeps the remembered list, team and AUD, and applications made in the dashboard are
never taken over by the API mode. The allow list is needed only when the allow policy is
created: without flags and with nothing remembered, a terminal run asks "Who may sign in?". A
consumer mail domain (`gmail.com`, `outlook.com`, …) needs a confirmation, since anyone with
such an address could sign in.

### Identity providers: Okta, Entra ID, Google…

Access offers one-time PINs by email without any setup. To sign in with your identity
provider, add it in **Zero Trust → Settings → Authentication** (login methods). For Okta:

1. In Okta, go to **Applications → Create App Integration** and pick **OIDC, Web Application**.
   - Sign-in redirect URI: `https://<team>.cloudflareaccess.com/cdn-cgi/access/callback`
   - Groups claim (**Sign On** tab → OpenID Connect ID Token): name `groups`, filter
     *Matches regex* `.*`. Then Access receives the user's groups.
   - Assign the people or groups who should use Backyard.
2. In Zero Trust → Settings → Authentication, add **Okta** with the app's client ID, client
   secret and your Okta account URL.
3. To allow Okta (or Access) groups, edit the allow policy **Backyard (&lt;worker&gt;) users** in
   the dashboard (Include → Okta groups). A re-run of `access enable` without allow flags keeps
   what you set there. The CLI itself only writes email and email-domain rules.
4. Optional: make an Okta group admin of Backyard (admins can delete any site):
   `backyard cloudflare setup --var ADMIN_GROUPS:backyard-admins`, or single people with
   `--var ADMINS:alice@acme.com`.

Entra ID, Google Workspace and the others work the same way: add the identity provider under
Zero Trust → Settings → Authentication. The [Terraform module](../terraform/cloudflare) can
also create the Okta provider and Okta-group rules.

### Turning it off

```sh
backyard cloudflare access disable                 # keeps the Access applications
backyard cloudflare access disable --delete-apps   # also deletes what access enable created
```

It asks first (`--yes` without a terminal), then deploys with `AUTH_MODE=none`: sites become
viewable by anyone with a link, on `workers.dev` right away and on your domain once the Access
applications are gone. Without `--delete-apps` it lists the applications that remain. With
it, it deletes the hub application, the bypass application, the allow policy and the bypass
policy, only objects this tool created and recorded, and needs `BACKYARD_CF_API_TOKEN`; a later
`access disable --delete-apps` also deletes what an earlier `access disable` left. Applications
made in the dashboard are deleted there. Answering no changes nothing (exit 3).

## Use Backyard in Claude

The Claude connector turns the hub into a remote MCP server for Claude (claude.ai, Desktop,
mobile, Cowork, Claude Code): people add `https://<hub>/__backyard/mcp` as a custom connector,
sign in, approve Backyard's consent page, and Claude lists, reads and publishes sites as them.
It is off until you turn it on:

```sh
backyard cloudflare setup --var MCP_ENABLED:true
# npx backyard-cloudflare@latest setup --var MCP_ENABLED:true
# npm run setup -- --var MCP_ENABLED:true        (in deploy/cloudflare of a checkout)
```

With the [Terraform module](#with-terraform-optional-infrastructure-as-code), set
`mcp_enabled = true`: its `worker_vars` and `setup_args` outputs always carry `MCP_ENABLED`
(`true`, or `false`, which turns the connector off at the next `setup`).

- **Publishing from Claude needs a sign-in.** With `AUTH_MODE=access` the consent page shows the
  person Access signed in, and Claude publishes as them. With `AUTH_MODE=none` the connector is
  the browser's anonymous guest: it can read sites, and publish only with
  `ALLOW_ANONYMOUS_DEPLOY=true` (which lets anyone publish). `DEPLOY_TOKEN` plays no part in
  the connector, and API tokens are not accepted by it.
- **Access.** After upgrading, re-run `backyard cloudflare access enable` (or `terraform apply`)
  once, so the bypass application holds all nine paths; `backyard cloudflare status` reports
  `access-connector` until it does. Updating a bypass application the CLI created needs the
  admin API token again (`BACKYARD_CF_API_TOKEN`, or paste it when asked); without one, the
  command names the four destinations to add in the dashboard, waits for them (`--wait`; 900
  seconds on a terminal), and ends with exit 4 while they are missing. `/__backyard/auth/authorize` stays behind Access: that login
  is the connector's sign-in, with Okta or any other identity provider of Access.
- **The URL** is shown on the hub's card "Use Backyard in Claude" (and as `mcp_url` in
  `/__backyard/api/info`). The hub URL must be https: `workers.dev` and custom domains are.
- **Limits.** One `backyard_publish` call carries at most 2 MiB of file content
  (`LIMITS.mcp_publish_bytes`); a bigger site goes over several calls, one after another, and big
  media go through the hub's drag-and-drop deploy or `backyard deploy`. Tool calls count against
  `LIMITS.mcp_calls_per_day` (5000 per user) and publishes against `deploys_per_day`. Each
  publish switches the site in one transaction, so parallel calls never lose each other's files.
  On Workers Free, a publish near the cap uses noticeable CPU time; lower
  `mcp_publish_bytes` if such calls fail.
- **Connections** last `TOKEN_TTL_HOURS` (720 by default) from the consent. People disconnect on
  the hub card; admins can disconnect a person there. Rotating `BACKYARD_SECRET` ends every
  connection.
- **Which apps.** `MCP_CLIENTS` and `MCP_REDIRECT_URIS` default to Claude's exact addresses; see
  the [configuration reference](#configuration-reference).

Adding the connector in Claude, the consent page, the tools and troubleshooting:
[docs/claude-connector.md](../../docs/claude-connector.md).

## Command reference

```
backyard cloudflare <command> [options]     # the Go CLI, or:
npx backyard-cloudflare@latest <command> [options]   # once a release has published it to npm
npm run cf -- <command> [options]           # in a checkout (npm run setup -- [options] for setup)
```

With no command the engine prints its help and exits 2; `<command> --help` lists the options of
a command and `--version` prints `backyard-cloudflare <version> (wrangler <version>)`. Hints in
the output name the form you used. The Go CLI's own flags (engine source, login afterwards) are
in [docs/cli.md](../../docs/cli.md#backyard-cloudflare).

Global options, for every command:

| Option | Meaning |
| --- | --- |
| `--worker-name <name>` (alias `--name`) | Worker name (default `backyard`). Use it to run several platforms on one account. |
| `-y`, `--yes` | Answer yes to confirmations. It never implies `--take-over`, `--replace-dns-record` or `--confirm-auth-change`. |
| `--non-interactive` | Never prompt (also when `CI` is set or there is no terminal). Missing input exits 2 and names the flag. |
| `-h`, `--help` | Show help. |

`setup`: deploy or upgrade Backyard; re-running keeps every choice.

| Option | Meaning |
| --- | --- |
| `--r2` | Use an R2 bucket for big files (default when R2 is enabled). |
| `--no-r2` | Never use R2 (remembered; files are capped at 25 MiB). |
| `--r2-bucket <name>` | Bucket name (default `<worker>-files`). |
| `--ai` | Serve AI with the Workers AI binding (the default). Turns AI back on after `--no-ai`. |
| `--no-ai` | Turn AI off (remembered): no Workers AI binding, and `AI_ENABLED=false`, so every AI call of the sites answers `501 unsupported`. For accounts where Workers AI is not available. See [Turning AI off](#turning-ai-off). |
| `--var <KEY:VALUE>` | Override a Worker variable (repeatable, remembered). `--var KEY:` drops an override. Secrets (`BACKYARD_SECRET`, `DEPLOY_TOKEN`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `WAREHOUSE_TOKEN`) are refused (exit 2): they are not variables. |
| `--rotate-token` | Issue a new `DEPLOY_TOKEN`. The old one stops working. |
| `--rotate-secret` | Replace `BACKYARD_SECRET`. This revokes every API token and anonymous identity. |
| `--skip-deploy` | Only (re)set the secrets. |
| `--workers-subdomain <name>` | The workers.dev subdomain to register when the account has none (CI). |
| `--workers-dev` | Serve on workers.dev (default). |
| `--no-workers-dev` | Stop serving on workers.dev (needs a working custom domain; remembered). |
| `--confirm-auth-change` | Allow a deploy that turns Cloudflare Access off or changes its team. |
| `--dry-run` | Build and validate only: no login, no changes. |

`domain add <host>`: the hub on your own domain (path mode), or sites on their own hosts.

| Option | Meaning |
| --- | --- |
| `--host-mode` | Sites at `https://<site>.<host>/` (wildcard route and DNS record). |
| `--zone <zone>` | Cloudflare zone of `<host>` (default: found automatically). |
| `--take-over` | Move `<host>` or the `*.<host>/*` route from another Worker to this one. |
| `--replace-dns-record` | Replace existing DNS records at `<host>`. |
| `--wait <seconds>` | How long to wait for DNS, certificate and readiness (default 300; 0 checks once). |

`domain remove`: remove the custom domain this tool added, back to workers.dev (global options only).

`access enable`: company login with Cloudflare Access (needs a custom domain).

| Option | Meaning |
| --- | --- |
| `--allow-email <email>` | Allow this email (repeatable). |
| `--allow-email-domain <domain>` | Allow every email at this domain, with or without a leading `@` (repeatable). |
| `--team <team>` | Zero Trust team of existing Access applications (`acme` or `acme.cloudflareaccess.com`); goes with `--aud`. |
| `--aud <tag>` | AUD tag of the existing hub application; goes with `--team`. |
| `--wait <seconds>` | How long to wait for Access (default 120; 900 while waiting for dashboard steps). |

`access disable`: turn company login off.

| Option | Meaning |
| --- | --- |
| `--delete-apps` | Also delete the Access applications and policies this tool created (needs `BACKYARD_CF_API_TOKEN`). |

`status`: read-only live check. It takes no lock, never prompts, never writes the state and
never logs in: without a wrangler login the Cloudflare API checks are skipped (and said so,
which makes the exit code 4) while the checks of the public URLs still run. On a machine
without state it shows the live configuration.

| Option | Meaning |
| --- | --- |
| `--json` | Machine-readable output, alone on stdout: `schema`, `worker`, `accountId`, `statePath`, `versions` (`package`, `wrangler`, `deployed`), `urls` (`public`, `workersDev`), `r2Bucket`, `workersDev`, `ai` (`enabled`: `AI_ENABLED` as the Worker reads it; `binding`: Workers AI is bound), `domain` and `access` (`null` when there is none; their `source` is `external` when they are set with `--var`, and the Access `source` is `live` when it was read from the Worker) and `checks`. |

Its checks, each `ok`, `pending` or `failed` with a detail: `worker`, `workers-dev`, `hub`,
`custom-domain`, `route`, `dns`, `tls`, `access-redirect`, `access-bypass`,
`access-connector` (the Claude connector's four bypass paths: `pending` while they are missing
and the connector is off, `failed` once `MCP_ENABLED` is on), `workers-dev-closed` and
`deploy-token` (only when this machine knows the token). Only the checks that apply are
listed.

Exit codes, the same for every command and for the Go CLI:

| Code | Meaning |
| --- | --- |
| `0` | Done, or nothing to do (`status`: every check is ok). |
| `1` | A Cloudflare, wrangler, network or file operation failed. The message lists what was already applied; a re-run resumes. `status`: the Worker does not exist. |
| `2` | Usage error, or input or a confirmation is needed without a terminal: the message names the flag. |
| `3` | Refused for safety: a conflict, an object this tool did not create, an auth downgrade, an unreadable live state, a domain change while Access is on. Nothing was changed. |
| `4` | Applied, but a check (DNS, certificate, readiness, Access) is still pending or failed. Re-running the same command resumes. `status`: a check needs attention. |
| `130` | Interrupted. |

## Credentials and permissions

Backyard uses two credentials:

- **Your wrangler login**: the browser login (`wrangler login`) or `CLOUDFLARE_API_TOKEN`. It
  covers deploys, secrets, R2, the workers.dev subdomain, zones, custom domains and routes.
  Backyard asks wrangler for it (`wrangler auth token`) for its own API calls and never stores
  or prints it.
- **An admin token**, `BACKYARD_CF_API_TOKEN`, only for the two things wrangler's login cannot
  do: the wildcard DNS record of host mode and the Access applications. It is optional (both
  steps have a guided alternative), never stored, never printed and never passed to wrangler.
  Give it only the permissions below, for one account (and zone), with a short TTL.

| Purpose | Credential | API-token permission |
| --- | --- | --- |
| workers.dev subdomain | wrangler login | Account · Workers Scripts · Edit |
| Reading the deployed configuration | wrangler login | Account · Workers Scripts · Read |
| Zone lookup | wrangler login | Zone · Zone · Read |
| Custom domain | wrangler login | the "Edit Cloudflare Workers" template (Workers Scripts Edit, Workers Routes Edit, Zone Read) |
| Routes (host mode) | wrangler login | Zone · Workers Routes · Edit |
| Wildcard DNS record (host mode) | `BACKYARD_CF_API_TOKEN` | Zone · DNS · Edit (that zone only) |
| Access applications and policies | `BACKYARD_CF_API_TOKEN` | Account · Access: Organizations, Identity Providers, and Groups · Read; Account · Access: Apps and Policies · Edit |
| Checks of your public URLs, Access login and DNS-over-HTTPS | none | — |

The browser login of wrangler covers every "wrangler login" row. For CI, an API token from the
"Edit Cloudflare Workers" template does (plus "Workers R2 Storage: Edit" for R2). When
`CLOUDFLARE_API_TOKEN` is set and no `BACKYARD_CF_API_TOKEN`, Backyard tries that token for the
admin steps too.

## State file

Each machine keeps the setup of each Worker in `~/.config/backyard/cloudflare/<worker>.json`
(`$XDG_CONFIG_HOME/backyard/cloudflare/<worker>.json` when that is set), mode 0600 in a 0700
directory, written atomically. A plain re-run therefore keeps every earlier choice. A state file
that isn't valid JSON stops every command rather than silently issuing a new deploy token.
Fields (version 2; files of earlier versions are read and upgraded on the next save, unknown
fields are kept):

| Field | Meaning |
| --- | --- |
| `version` | `2`. |
| `accountId` | The Cloudflare account. |
| `url` | The public hub URL. |
| `deployToken` | The `DEPLOY_TOKEN`, when this machine knows it. |
| `workersDevUrl`, `workersDev` | The workers.dev URL and whether the Worker serves there. |
| `r2`, `r2Bucket` | The R2 choice and bucket. |
| `ai` | `false` after `setup --no-ai`: deploys leave the Workers AI binding out and set `AI_ENABLED=false`. Absent: AI is on (`--ai` removes it). |
| `vars` | Your `--var` overrides. |
| `domain` | Set by `domain add`: `host`, `mode` (`path` or `host`), `zoneId`, `zoneName`, the ids of the custom domain, route and DNS record this tool attached or created, `status` (`active`, `pending-dns`, `pending-certificate`) and `source` (`cli` or `adopted`). |
| `access` | Set by `access enable`: `team`, `aud`, `source` (`api`, `manual` or `adopted`), the ids of the applications and policies it created, and the `allow` list. |
| `accessCreated` | Ids of Access objects this tool created that `access` does not use (yet, or any more): kept so that a re-run reuses them and `access disable --delete-apps` can delete them. |
| `updatedAt` | When it was last saved. |

The admin token is never in it. The state is per machine: on another machine (a teammate, a
new laptop, a CI runner) the first command adopts the deployed configuration instead
([What `setup` does](#what-setup-does)), and only the deploy token is unknown there until you
pass `DEPLOY_TOKEN=…` once. A lock file next to it, `<worker>.lock`, keeps two runs for the same
Worker apart.

## Environment variables

| Variable | Use |
| --- | --- |
| `CLOUDFLARE_API_TOKEN`, `CLOUDFLARE_ACCOUNT_ID` | Wrangler's credentials (CI and agents). A login that reaches several accounts needs `CLOUDFLARE_ACCOUNT_ID` when there is no terminal to ask in. |
| `CLOUDFLARE_API_BASE_URL` | Cloudflare API base for wrangler **and** Backyard's own API calls (the admin token is sent there too). Default `https://api.cloudflare.com/client/v4`. |
| `DEPLOY_TOKEN` | Use (and remember) this deploy token. |
| `BACKYARD_CF_API_TOKEN` | Admin token for the host-mode DNS record and the Access applications; never stored. |
| `BACKYARD_CF_DOH_URL` | DNS-over-HTTPS JSON endpoint for the DNS checks (default `https://cloudflare-dns.com/dns-query`). |
| `BACKYARD_CF_RESULT_FILE`, `BACKYARD_CF_INVOKED_AS`, `BACKYARD_CF_CLI_LOGIN` | For wrappers such as the Go CLI: a JSON file the command writes its result to (no secrets), how hints name the command, and `1` when the wrapper logs its CLI in with the deploy token itself (`setup` then leaves out the `backyard login` line and the next step). |
| `BACKYARD_VERSION` | Build time only (`sync-assets`, `pack.mjs`): the version the Worker reports in `/__backyard/api/deploy/info`. |
| `NO_COLOR`, `CI` | No colors; `CI` also means non-interactive and keeps the deploy token masked. |

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
Object, and other requests that only read a never-used site store nothing: a site's tables are
created by its first write, so probing random names costs no storage writes). The
guardrails are daily counters per user (per IP when anonymous) plus platform-wide caps, kept
in `RegistryDO`. When a cap is hit, the API answers `429` with `Retry-After` (seconds until
00:00 UTC). Tune them with the `LIMITS` variable (JSON, merged over the defaults; `0` means
unlimited for the daily counters). Write it as a string, or as a JSON object when you set it
as a JSON value in `wrangler.jsonc` or the dashboard:

```sh
backyard cloudflare setup --var 'LIMITS:{"ai_per_day":50,"max_sites":100}'
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
| `ai_images_per_day` | 30 | Generated images per user/IP per day (a request for `n` images counts `n`). |
| `ai_platform_per_day` | 2000 | AI calls per day, whole platform. This guards your neuron allowance. |
| `ai_images_platform_per_day` | 300 | Generated images per day, whole platform (each image counts). |
| `ai_max_tokens` | 4096 | Upper bound for `max_tokens` of a chat. |
| `deploys_per_day` | 200 | Deploys per user/IP per day. |
| `uploads_per_day` | 500 | File uploads per user/IP per day. |
| `db_writes_per_day` | 20,000 | Document writes per user/IP per site per day (counted in the site's Durable Object). |
| `ws_conns_per_user` | 50 | Concurrent websockets per user per site. |
| `ws_rooms_per_conn` / `ws_subs_per_conn` | 50 / 100 | Rooms joined and database subscriptions per connection. |
| `ws_messages_per_second` / `ws_message_burst` | 60 / 120 | Token bucket for client frames. |
| `ws_max_message_bytes` / `ws_max_presence_bytes` | 64 KiB / 8 KiB | Frame and presence-state size. |
| `mcp_publish_bytes` | 2 MiB | File content (decoded) per `backyard_publish` call of the [Claude connector](#use-backyard-in-claude). Its request bodies may be 2 × this + 1 MiB. |
| `mcp_calls_per_day` | 5000 | Claude connector tool calls per user per day. Only `tools/call` requests that name a tool count (also when its arguments are invalid); `ping`, `tools/list` and other methods are free. |

## URL modes

| Mode | Hub | Sites | Needs |
| --- | --- | --- | --- |
| **path** (default) | `https://backyard.<you>.workers.dev/` | `…/s/<site>/` | Nothing |
| path + custom domain | `https://backyard.example.com/` | `https://backyard.example.com/s/<site>/` | `domain add backyard.example.com`: a zone on Cloudflare (Workers custom domain, free) |
| **host** | `https://example.com/` | `https://<site>.example.com/` | `domain add example.com --host-mode`: a wildcard route and DNS record, and a certificate that covers the wildcard |

Path mode works everywhere, but all sites share one origin with each other **and with the
hub**. They share `localStorage`, cookies and service workers, and a page on any site can call
the hub API as its visitor (list sites, mint the visitor an API token). Use it for a team that
trusts every site author. Host mode gives every site its own origin.

In host mode the hub itself is a Workers custom domain on the base host, and the route
`*.<host>/*` sends every site host to the Worker. Universal SSL (free) covers `example.com`
and `*.example.com`, which is **one** wildcard level: for sites at `<site>.example.com`, use
the zone apex (ideally a zone of its own, see [Your own domain](#your-own-domain)). For
`<site>.backyard.example.com`, you need an Advanced Certificate (Advanced Certificate Manager,
paid).

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
// backyard cloudflare setup --var 'AI_MODELS:{…}' (one line in practice), or wrangler.jsonc → vars.AI_MODELS
{
  "smart":  { "provider": "workers-ai", "model": "@cf/google/gemma-4-26b-a4b-it", "kind": "chat" },
  "claude": { "provider": "anthropic",  "model": "claude-opus-5-5", "kind": "chat",
              "description": "Claude for hard problems", "max_tokens": 8192, "effort": "high" },
  "gpt":    { "provider": "openai",     "model": "gpt-5-mini", "kind": "chat" },
  "image":  { "provider": "workers-ai", "model": "@cf/black-forest-labs/flux-1-schnell", "kind": "image" },
  "embed":  { "provider": "workers-ai", "model": "@cf/baai/bge-base-en-v1.5", "kind": "embedding" }
}
```

- API keys are **secrets**: `npx wrangler secret put ANTHROPIC_API_KEY --name backyard` and
  `npx wrangler secret put OPENAI_API_KEY --name backyard` (use your `--worker-name`). Setup
  keeps them. `GET /__backyard/api/ai/models` lists only the aliases whose provider is usable
  (AI binding present, key set).
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
- Every AI call counts against `ai_per_day` and `ai_platform_per_day`. Each generated image
  also counts against the image caps (`n: 4` counts 4).
- Chat images: inline base64 (`data`, or a `data:` URL; at most 20 MiB each) is passed on to
  the provider as sent, without decoding it in the Worker. An image `url` that names one of the
  site's own uploads (what `files.upload` returns, site-relative or absolute on the site's
  origin) is read from the site's storage (png, jpeg, gif or webp; 64 MiB per request); other
  absolute URLs are handed to the provider.

### Turning AI off

AI is on by default. To turn it off, for example on an account where Workers AI is not
available (a deploy with the `AI` binding fails there), run setup with `--no-ai`:

```sh
backyard cloudflare setup --no-ai     # remembered: later runs, domain add and access enable keep it
backyard cloudflare setup --ai        # back on
```

With AI off, deploys leave the Workers AI binding out and set `AI_ENABLED=false`. The Worker
then answers every AI endpoint (`/ai/chat`, `/ai/image`, `/ai/embed`, `GET /ai/models`) with
`501 unsupported` and the message `AI is not configured on this server`, before it reads the
request or counts anything against the caps, exactly like the Go server with
`ai.enabled: false`. No provider is called, also not Anthropic or OpenAI aliases with their keys
set. Sites see AI as unavailable: every `backyard.ai` call rejects with the error code
`unsupported`, which the [skill](../../skills/backyard/SKILL.md) already tells agents to handle
gracefully. `status` shows `AI: off`.

The flag wins over the variable, so the binding and `AI_ENABLED` never disagree: while AI is
off, `AI_ENABLED` is managed by `setup` (`--var AI_ENABLED:…` exits 2, and `--no-ai` drops an
earlier `--var AI_ENABLED` override). With AI on, `--var AI_ENABLED:false` turns only the AI
endpoints off and keeps the binding (`--var AI_ENABLED:` drops it again). The Terraform module's
`ai_enabled` variable puts `--ai` or `--no-ai` into its `setup_args`.

## Configuration reference

Set variables with `backyard cloudflare setup --var KEY:VALUE` (repeatable, remembered;
`--var KEY:` drops an override) or in `wrangler.jsonc` → `vars` in a checkout. `BASE_DOMAIN`
and `URL_MODE` belong to `domain add`, `AUTH_MODE`, `ACCESS_TEAM_DOMAIN` and `ACCESS_AUD` to
`access enable`, and `AI_ENABLED` to `setup --no-ai` while AI is off. Every deploy sets all
variables, so a change made in the dashboard lasts until the next `setup`; and don't deploy
with a plain `wrangler deploy`, which would deploy `wrangler.jsonc`'s defaults past the auth
guard. Secrets go through
`npx wrangler secret put NAME --name <worker>`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `BASE_DOMAIN` | request host | Hub host. Required for host mode and custom domains (set by `domain add`). |
| `PUBLIC_URL` | `https://<BASE_DOMAIN>` or request origin | Absolute hub URL: `scheme://host[:port]`, no path, user info or query; lowercased like the Go server's `server.public_url`. |
| `URL_MODE` | `path` | `path` or `host` (host needs `BASE_DOMAIN`; set by `domain add --host-mode`). |
| `AUTH_MODE` | `none` | `none` (anonymous cookies) or `access` (Cloudflare Access, set by `access enable`). Any other value (`oidc`, `okta`, a typo) fails closed: every request but the health checks answers `503` until it is fixed. |
| `ACCESS_TEAM_DOMAIN` | — | `acme` or `acme.cloudflareaccess.com`. |
| `ACCESS_AUD` | — | Application AUD tag (comma-separated for several). |
| `ACCESS_GET_IDENTITY` | `true` | Fetch name and groups from Access `get-identity`. |
| `ALLOW_ANONYMOUS_DEPLOY` | `false` | `true` lets anyone deploy without a token. Fine for a playground; `LIMITS` still apply. |
| `ADMINS` / `ADMIN_GROUPS` | — | Comma-separated admin emails and IdP group names. |
| `OWNERSHIP` | `none` | `none` (anyone may overwrite a site, the Quick way) or `owner` (only owners and admins). |
| `SERVER_NAME` | `Backyard` | Name shown in the hub. |
| `RESERVED_SITES` | — | Extra reserved site names (comma-separated). |
| `LIMITS` | see above | Guardrails (JSON: a string, or an object). |
| `FILE_INLINE_MAX` | `5242880` | Files above this size go to R2 when bound (max 25 MiB). |
| `TOKEN_TTL_HOURS` | `720` | Longest lifetime of an API token. |
| `AI_ENABLED` | `true` | Anything but `true`, `1`, `yes`, `on` or empty (e.g. `false`) turns AI off: every `/__backyard/api/ai/*` endpoint answers `501 unsupported`, like the Go server with `ai.enabled: false`. `setup --no-ai` sets it and drops the Workers AI binding; see [Turning AI off](#turning-ai-off). |
| `AI_*`, `ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL` | — | See [AI](#ai). |
| `MCP_ENABLED` | `false` | `true` serves the [Claude connector](#use-backyard-in-claude) at `<hub>/__backyard/mcp` (needs an https hub URL). Unset or false: every connector path answers `404`. |
| `MCP_CLIENTS` | Claude's two client metadata URLs | Client ID Metadata Document URLs that may connect (comma-separated, compared exactly; only these are fetched). Set but empty (`--var MCP_CLIENTS:,`) turns this off, so apps register automatically; `--var MCP_CLIENTS:` drops the override. |
| `MCP_REDIRECT_URIS` | `https://claude.ai/api/mcp/auth_callback`, `https://claude.com/api/mcp/auth_callback` | Exact https addresses that may receive sign-in codes (comma-separated). http loopback addresses are always allowed. |
| `WAREHOUSE_URL` | — | Optional: proxy `/warehouse/*` to a Backyard Go server's site API (secret `WAREHOUSE_TOKEN`). Its query errors (400/403/404/429/501) pass through; other failures answer `502`/`504 upstream_error` with a generic message naming the request id (details in the Worker log). |

| Secret | Meaning |
| --- | --- |
| `BACKYARD_SECRET` | At least 32 characters. Signs anonymous identities, API tokens, deploy ids and upload URLs. The keys are derived like the Go server's (HKDF-SHA256), so the two editions interoperate. |
| `DEPLOY_TOKEN` | Static bearer token with admin rights for the CLI and CI (deploy, list, delete). Like the Go server's static tokens, it can't mint API tokens. |
| `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` | Optional AI providers. |

Configuration problems (bad JSON, unknown `LIMITS` keys, missing Access settings, a variable of
the wrong JSON type) are logged. Invalid `MCP_*` values turn only the Claude connector off and
are listed by `/__backyard/readyz`. Watch them with `npx wrangler tail <worker>`. Variables
written as JSON values (`"FILE_INLINE_MAX": 1000`, `"LIMITS": {…}`) are taken like their text.

## With Terraform (optional, infrastructure as code)

You don't need Terraform: `domain add` and `access enable` set up everything above. If you
manage Cloudflare with Terraform anyway, [`deploy/terraform/cloudflare`](../terraform/cloudflare)
creates the custom domain (path mode), or the wildcard DNS, routes and an optional Advanced
Certificate (host mode), plus the Access applications, an allow policy (emails, email domains,
Access groups, Okta groups), the bypass application and optionally the Okta identity provider.

**One owner per Worker.** Use either the CLI commands or the module for a Worker's domain and
Access, not both. Their objects have different names (the CLI's are called
`Backyard (<worker>)…`, the module's `Backyard…`), and with an API token `access enable`
refuses (exit 3) to change an Access application it did not create.

With the module, `setup` still deploys the Worker, and the module's outputs hand it the
variables:

```sh
backyard cloudflare setup                         # 1. deploy the Worker (workers.dev); add --no-ai with ai_enabled = false
# 2. terraform apply in your configuration of the module (see its README)
backyard cloudflare setup $(terraform output -raw setup_args)   # 3. domain + Access variables
```

`setup_args` is the module output `--worker-name <worker> --ai|--no-ai --var KEY:VALUE …`
(`--no-ai` when `ai_enabled = false`); the module's
`examples/basic` exports it, and your own root configuration re-exports it the same way
(`output "setup_args" { value = module.backyard.setup_args }`).
`setup_command` holds the same as an `npm run setup -- …` command for a checkout. Variables set
this way are **externally managed**:
`setup` deploys and remembers them as usual, and `domain add`, `domain remove`, `access enable`
and `access disable` leave them alone (`domain add` and `access enable` exit 2 and say how to
drop them with `--var KEY:`).

## Deploying from GitHub Actions

The repository ships `.github/workflows/cloudflare-deploy.yml`, which tests and deploys the
Worker with `scripts/cloudflare-ci.mjs`: fork the repository, add the secrets
`CLOUDFLARE_API_TOKEN` ("Edit Cloudflare Workers" template) and `CLOUDFLARE_ACCOUNT_ID`
(recommended: `DEPLOY_TOKEN`), and run it from the Actions tab. The workflow's header lists
every secret and variable. To deploy from your own pipeline, the core is:

```yaml
name: Deploy Backyard to Cloudflare
on:
  workflow_dispatch:
    inputs:
      confirm_auth_change:
        description: Let this run turn Cloudflare Access off or change its team
        type: boolean
        default: false

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-node@v7
        with:
          node-version: 22
      - run: npm ci
        working-directory: deploy/cloudflare
      - run: node scripts/cloudflare-ci.mjs deploy      # from the repository root
        env:
          CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
          CLOUDFLARE_ACCOUNT_ID: ${{ secrets.CLOUDFLARE_ACCOUNT_ID }}
          DEPLOY_TOKEN: ${{ secrets.DEPLOY_TOKEN }}
          BACKYARD_CF_WORKER_NAME: ${{ vars.BACKYARD_CF_WORKER_NAME }}
          BACKYARD_CF_VARS: ${{ vars.BACKYARD_CF_VARS }}
          BACKYARD_CF_R2_BUCKET: ${{ vars.BACKYARD_CF_R2_BUCKET }}
          BACKYARD_CF_AI: ${{ vars.BACKYARD_CF_AI }}
          # Only a manual run with the box checked lets a deploy past the auth guard.
          BACKYARD_CF_CONFIRM_AUTH_CHANGE: ${{ inputs.confirm_auth_change && 'true' || '' }}
```

The CI deployer is declarative: every deploy sets `wrangler.jsonc`'s variables plus
`BACKYARD_CF_VARS` (a JSON object), and `wrangler.jsonc`'s bindings plus the R2 bucket named in
`BACKYARD_CF_R2_BUCKET`. So to take over a Worker that `setup` made, carry over what
`backyard cloudflare status` shows:

- once `domain add` and `access enable` have set `BASE_DOMAIN`, `URL_MODE`, `AUTH_MODE`,
  `ACCESS_TEAM_DOMAIN` and `ACCESS_AUD`, copy them into `BACKYARD_CF_VARS`;
- if `setup` bound an R2 bucket (the `R2:` line, `r2Bucket` in `--json`; by default
  `<worker>-files`), set `BACKYARD_CF_R2_BUCKET` to it. A deploy without it removes the R2
  binding: the files stored in R2 then answer `500` until a deploy binds the bucket again (the
  objects stay in the bucket), and new files are capped at 25 MiB;
- if `setup` used `--worker-name`, set `BACKYARD_CF_WORKER_NAME` to it (the default is `name`
  in `wrangler.jsonc`, `backyard`);
- if `setup` turned AI off (`--no-ai`: the `AI:` line, `ai` in `--json`), set
  `BACKYARD_CF_AI` to `false`. The CI deployer then leaves the Workers AI binding out and sets
  `AI_ENABLED` to `false`, as `setup --no-ai` does (so `BACKYARD_CF_VARS` and
  `BACKYARD_CF_DEPLOY_ARGS` may not set `AI_ENABLED` then). Empty or `true` keeps AI on: a
  deploy binds Workers AI from `wrangler.jsonc`, which fails on an account without Workers AI.

CI deploys always serve on `workers.dev` (`workers_dev` in `wrangler.jsonc`), even after
`setup --no-workers-dev`. The CI deployer runs the same auth guard as `setup`: a deploy that would turn a live
Cloudflare Access off or change its team, or a deploy to an existing Worker whose auth mode it
cannot read (for example without `CLOUDFLARE_ACCOUNT_ID`), is refused with exit 1. The message
names the live values to put into `BACKYARD_CF_VARS` (or, when the live state is unreadable,
what the credential needs). If the change is deliberate, run that one deploy with
`BACKYARD_CF_CONFIRM_AUTH_CHANGE=true`: in the workflow above, **Run workflow** with
`confirm_auth_change` checked (a repository variable would switch the guard off for every later
deploy too). It never deploys routes or
custom domains: `routes` in `wrangler.jsonc` are ignored with a warning, and
`BACKYARD_CF_DEPLOY_ARGS` may not set routes, domains, another configuration, environment or
Worker name.

`setup` works in CI too: `npx backyard-cloudflare@<version> setup --non-interactive` with
`CLOUDFLARE_API_TOKEN` and `CLOUDFLARE_ACCOUNT_ID` (plus `--workers-subdomain <name>` on an
account without one). A fresh runner has no state file, so it adopts the deployed configuration
each time; pass `DEPLOY_TOKEN` so it knows the token.

## Local development and tests

```sh
npm ci
npm test             # vitest inside workerd (@cloudflare/vitest-plugin) + offline tests of the engine (scripts/);
                     # no Cloudflare account needed (test:worker / test:scripts run one half)
npm run typecheck    # tsc --noEmit (strict)
npm run dev:local    # wrangler dev --env local: no login needed; Workers AI off (AI answers 501), R2 simulated
npm run dev          # wrangler dev with the real Workers AI binding (needs `wrangler login`)
npm run build        # bundle check (wrangler deploy --dry-run)
npm run setup -- --dry-run   # the effective configuration of a setup, validated by wrangler (offline)
```

The engine's tests run against a fake wrangler and a fake Cloudflare API, DNS-over-HTTPS server
and edge on localhost, so they need no account either.

The Worker bundles the SDK, hub UI and agent docs from `../../web` and `../../skills`, so build
from a complete checkout of the repository. `npm run build`, `npm run setup` and wrangler's own
build step for `wrangler deploy` / `versions upload` stop with a list of what is missing
instead of shipping placeholders; `npm run sync`, `npm test` and `wrangler dev` use
placeholders with a warning. `BACKYARD_REPO_ROOT` points the sync at another checkout. The
Worker reports its version in `/__backyard/api/deploy/info`: `<package.json version>-dev` for
checkout and development builds, the release version (`BACKYARD_VERSION` at build time) for
release packages.

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
before `dev`, `test`, `typecheck`, and as wrangler's build step). Don't edit them.

Layout: `src/index.ts` (entry), `router.ts` (URLs), `hub.ts` (hub, CLI login, tokens), `site-api.ts`
(site API), `gateway.ts` (deploy protocol), `site-do.ts` / `registry-do.ts` (Durable Objects),
`db/` (document query engine), `ai/` (providers), `auth.ts` / `jwt.ts` / `crypto.ts` (identity,
tokens, keys). The engine: `bin/backyard-cloudflare.mjs` (entry), `scripts/cli.mjs`
(dispatch), `scripts/{setup,domain,access,status}.mjs` (the commands), `scripts/lib/`
(`commands.json` is the single source of the commands and flags).

## Building the npm package

```sh
make cloudflare-pack                     # → dist-npm/backyard-cloudflare-<version>.tgz and .tgz.sha256
make cloudflare-pack CF_PACK_VERSION=1.2.3
npm run package -- --version 1.2.3 --out /tmp/pkg   # the same, in deploy/cloudflare (after npm ci)
```

`scripts/pack.mjs` bundles the Worker with the real wrangler (offline), stages the package
(`bin/`, the engine scripts, `dist/worker.js`, `dist/wrangler.base.json`, `dist/build.json`, an
`npm-shrinkwrap.json` derived from `package-lock.json`, `README.md` from `npm-readme.md`),
checks its file list and writes the `.tgz` with its SHA-256 next to it. The version defaults to
`package.json`'s. Releases attach the `.tgz` to the GitHub release (listed in `checksums.txt`),
build its digest into the `backyard` CLI and can publish it to npm.

To try a local package:

```sh
npx ./dist-npm/backyard-cloudflare-0.2.0.tgz setup                     # a ./relative path
backyard cloudflare setup --package ./dist-npm/backyard-cloudflare-0.2.0.tgz
```

npx needs the `./` (or `--package <path>`): `npx /absolute/path.tgz` does not work.

## Differences from the Go edition

| Area | Go server | Cloudflare edition |
| --- | --- | --- |
| Sign-in | `none`, OIDC (Okta, Entra ID, Google…), IAP, ALB, trusted headers, several static tokens | `none` (anonymous cookies) or Cloudflare Access (any IdP Access supports, SAML included). One static `DEPLOY_TOKEN`. API tokens and the CLI browser login are identical. |
| Warehouse | BigQuery, Athena, Postgres | `501 unsupported`, unless `WAREHOUSE_URL` proxies a Go server. |
| AI providers | Anthropic, OpenAI, Bedrock, Gemini | Workers AI (default, free), Anthropic, and OpenAI-compatible APIs. |
| Rate limits | Per-minute token buckets plus quotas | Daily caps per user/IP and platform-wide (`LIMITS`). Document write caps are counted in each site's Durable Object and reset if it's evicted. |
| Query pages | Up to `limit` documents | Up to `limit` documents and about 8 MiB of stored JSON: a page travels from the site's Durable Object as one RPC result (32 MiB at most). A page cut short says `has_more`; page with `offset` advanced by the number of documents received. |
| File size | Configurable | 25 MiB per file without R2, 95 MiB with R2 (100 MB request limit). |
| Deploy methods | `api`, plus direct sync (`s3`, `gcs`, `azure`, `rsync`) | `api` only (the HTTP gateway protocol). `/__backyard/api/info` reports `"edition": "cloudflare"`. |
| Realtime | In memory, or Redis/Postgres pub/sub across replicas | Each site's websocket hub runs in its own Durable Object (hibernation). Same protocol. The server sends no keep-alive pings (they would wake hibernated objects): the SDK's `{"t":"ping"}` every 25 s is answered with `{"t":"pong"}` by the runtime without waking the object, and closed connections are detected by the runtime. |
| Uploads | Buffered by the server | Streamed: the site's Durable Object parses the multipart body and writes the file to SQLite or, above `FILE_INLINE_MAX`, to R2 in 10 MiB parts, so neither memory nor the Worker's CPU limit caps uploads below the file limits. |
| Hub UI, SDK, docs | Embedded in the binary | Bundled into the Worker at build time from `web/` and `skills/` (prebuilt in the npm package). Redeploy to update. |
| Site owners endpoint | Supported | Supported when `OWNERSHIP=owner`, otherwise `501`. |
| Claude connector | `mcp.enabled`; 4 MiB per publish call (`mcp.max_publish_bytes`); per-minute rate limits; publishes of one site serialized by a lease | `MCP_ENABLED`; 2 MiB per publish call (`LIMITS.mcp_publish_bytes`); `mcp_calls_per_day`; each publish is one transaction of the site's Durable Object. Same tools, sign-in flow and wire format. |

Everything a site or agent uses behaves the same on both editions: the `backyard.js` SDK and its
preamble, `/__backyard/api/*` (CSRF rule, error envelope, flattened documents, the full query
language), file uploads, AI streaming (SSE), the websocket protocol, the deploy gateway
protocol, API tokens (HS256; a token minted by one edition with the same secret verifies on the
other), `/llms.txt` and `/docs`.

## Upgrading, backups and removal

- **Upgrade:** re-run `setup`: `backyard cloudflare setup` with a newer `backyard` release (it
  installs that release's package), `npx backyard-cloudflare@latest setup` (once it is on npm), or
  `git pull && npm ci && npm run setup` in a checkout. Secrets, the deploy token, R2, AI, the
  domain, Access and `--var` overrides are kept. Durable Object data survives deploys.
  `backyard cloudflare status` shows the package, wrangler and deployed versions.
- **Back up a site:** `backyard pull <site> ./backup` downloads its files. To export
  documents, query them through the site API (`/__backyard/api/db/<collection>`).
- **Remove:** `access disable --delete-apps` and `domain remove` first, if you used them. Then
  `npx wrangler delete backyard`, which **permanently deletes** every site and document,
  remove the R2 bucket (`npx wrangler r2 bucket delete backyard-files`), the state file
  (`~/.config/backyard/cloudflare/backyard.json`), and run `terraform destroy` if you used the
  module.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| Exit code 3, "… Nothing was changed." | Refused for safety; the message names the cause and the flag or command that resolves it (`--take-over`, `--replace-dns-record`, `--team`/`--aud`, `domain remove`, `access disable`). |
| Exit code 4, "applied, but …" | DNS, a certificate, readiness or Access is still pending. Re-run the same command later; `backyard cloudflare status` shows what is left. |
| "Worker … is live with Cloudflare Access … and this deploy would turn it off" | The auth guard. If another machine turned Access on, delete the state file named in the message and re-run to adopt the live configuration. If it is deliberate: `setup --confirm-auth-change`, or `access disable`. In CI, put the live values into `BACKYARD_CF_VARS`, or run that one deploy with `BACKYARD_CF_CONFIRM_AUTH_CHANGE=true` (in GitHub Actions: **Run workflow** with `confirm_auth_change` checked). |
| "could not read the live configuration … so this deploy cannot be checked" | The credential cannot read the Worker's settings (a scoped token needs Account · Workers Scripts · Read, and `CLOUDFLARE_ACCOUNT_ID`). Fix the credential, or confirm with `--confirm-auth-change`. |
| `domain add --host-mode` exits 4 and sites stay on `/s/<site>/` | The wildcard record does not resolve yet: add the printed `AAAA` record (proxied), or re-run with `BACKYARD_CF_API_TOKEN`. Resolvers may cache the old answer for a few minutes; re-run. |
| "the certificate does not cover *.&lt;host&gt; yet" | Below the zone apex the sites need an Advanced Certificate (see [Your own domain](#your-own-domain)); at the apex Universal SSL can take a few minutes. |
| `access enable` exits 4: "the bypass application is missing …" | The machine endpoints application lacks that path: add it in the dashboard, then re-run `access enable`. After an upgrade an application made in the dashboard lacks the Claude connector's four paths: add them (the guided steps list all nine). |
| `status`: `access-connector` pending or failed | The bypass application lacks the Claude connector's paths: re-run `access enable` (or `terraform apply` of the module). |
| Claude cannot connect: `https://<hub>/__backyard/mcp` answers `404` | `MCP_ENABLED` is not on, the hub URL is not https, or an `MCP_*` value is invalid (`/__backyard/readyz` names it). See [Use Backyard in Claude](#use-backyard-in-claude). |
| `backyard_publish` answers "… can only read" | `AUTH_MODE=none` without `ALLOW_ANONYMOUS_DEPLOY`: turn on Access (or allow anonymous deploys). `DEPLOY_TOKEN` does not help here. |
| "Access in front of … belongs to team …" or an AUD mismatch | Another Access application covers the host: pass its `--team`/`--aud`, or fix the applications. |
| "Zero Trust is not set up on this account" | Open <https://one.dash.cloudflare.com/> once, pick a team name and the Free plan, then re-run. |
| "… is set with --var (e.g. by the Terraform module)" | Those variables are externally managed. Drop them with `setup --var BASE_DOMAIN: --var URL_MODE:` (or the `AUTH_MODE`/`ACCESS_*` keys), or keep managing them with Terraform. |
| "backyard-cloudflare needs Node.js 22 or later" | Install Node.js 22 or later (wrangler 4 needs it): <https://nodejs.org/>. |
| Behind an HTTP proxy | wrangler uses `HTTPS_PROXY`. Backyard's own API calls and checks use Node's `fetch`: set `NODE_USE_ENV_PROXY=1` as well, on Node 22.21 or later (older versions ignore it). The engine warns at the start when it sees a proxy variable that `fetch` would ignore. |
| "not logged in to Cloudflare" (exit 2) | No terminal for the browser login: run the command on a terminal once, or set `CLOUDFLARE_API_TOKEN` (and `CLOUDFLARE_ACCOUNT_ID`). |
| "this login can deploy to … Cloudflare accounts" | Set `CLOUDFLARE_ACCOUNT_ID`, or run on a terminal and pick one (it's remembered). |
| "another backyard-cloudflare run for … is in progress" | Wait for it. If none is running, delete the lock file the message names. |
| "this account has no workers.dev subdomain" or "register a workers.dev subdomain first" | Pass `setup --workers-subdomain <name>`, or run `setup` on a terminal and answer the prompt. |
| `503` "BACKYARD_SECRET is not set" | `setup --skip-deploy`, or `npx wrangler secret put BACKYARD_SECRET --name <worker>` (≥ 32 chars). |
| `503` "not configured correctly (AUTH_MODE must be …)" | `AUTH_MODE` is neither `none` nor `access` (a `--var` typo). Set it right with `setup --var AUTH_MODE:access` (plus `ACCESS_TEAM_DOMAIN` and `ACCESS_AUD`), or drop it with `setup --var AUTH_MODE: --confirm-auth-change` (the auth guard asks for the confirmation because this opens the Worker). |
| `401` "sign-in cookies of more than one account" | The browser holds cookies of two users for this domain (a site's page may have set one). Reload; if it persists, clear the domain's cookies. |
| `npm run dev`: "Failed to start the remote proxy session" | Workers AI only runs remotely. Run `npx wrangler login`, or use `npm run dev:local`. |
| `backyard login`/`deploy` gets an HTML login page or `302` | Access is protecting the deploy gateway. Re-run `access enable`: its edge verification names the missing bypass path. |
| `401` "Cloudflare Access token" errors in the browser | `ACCESS_AUD` or `ACCESS_TEAM_DOMAIN` don't match the Access application: re-run `access enable` (or with `--team`/`--aud`). |
| `413` on deploy | A file is above the per-file cap (25 MiB without R2): enable R2 on the account and re-run `setup`. Or a guardrail (`max_sites`, `max_site_bytes`, `max_files`) was hit. |
| `429` with `Retry-After` | A daily guardrail was hit. Raise it with `LIMITS`, or wait until 00:00 UTC. |
| The deploy fails on the Workers AI binding (`AI`), e.g. Workers AI is not available on the account | `setup --no-ai`: deploys without the binding, and the sites' AI calls answer `501 unsupported` ([Turning AI off](#turning-ai-off)). |
| AI calls answer `501` "AI is not configured on this server" | AI is off (`status` shows the `AI:` line): `setup --ai` after `--no-ai`, `setup --var AI_ENABLED:` after `--var AI_ENABLED:false`; or no alias has a usable provider (no Workers AI binding, no API key). |
| R2 bucket could not be created | Enable R2 once in the dashboard (it's free, but requires opting in), then re-run `setup` (add `--r2` if you turned it off with `--no-r2`). |
| `setup` asks which account | Your login reaches several accounts. Answer once (it's remembered), or set `CLOUDFLARE_ACCOUNT_ID`. |
| Anything else | `backyard cloudflare status` checks everything; `npx wrangler tail <worker>` streams the Worker's logs, including configuration problems. |
