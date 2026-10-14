# backyard-cloudflare

[Backyard](https://github.com/thasami/backyard#readme) turns a folder of HTML into an internal URL
with a zero-config backend (database with realtime, files, AI, identity). This package runs it on
**your own Cloudflare account, on the Workers free plan**: one Worker, SQLite-backed Durable
Objects, Workers AI and an optional R2 bucket.

The package ships a prebuilt Worker and pins its one dependency, wrangler. You need no checkout,
no build step and no Terraform: only a Cloudflare account and **Node.js 22 or later**.

## Quickstart

```sh
npx backyard-cloudflare@latest setup
```

`setup` opens the Cloudflare login in your browser when wrangler is not logged in yet, asks which
account to use (only when the login reaches several) and, when the account has no workers.dev
subdomain yet, which one to register. Then it creates the R2 bucket (when R2 is enabled on the
account), deploys the Worker and waits until the hub answers. On an interactive terminal it ends
with the hub URL, a link that publishes from the browser, and the command that connects the
Backyard CLI:

```
✔ Backyard is live at https://backyard.acme.workers.dev
Publish in the browser: https://backyard.acme.workers.dev/#token=byd_…   (the hub asks before using it)
  backyard login --server https://backyard.acme.workers.dev --token byd_…
```

No Backyard CLI yet? The browser link needs none; for `backyard login`, `backyard init` and
`backyard deploy`, install it with
`curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh`.

Later changes take one command each, and each command checks its own result:

```sh
npx backyard-cloudflare@latest domain add backyard.acme.com              # the hub on your own domain
npx backyard-cloudflare@latest domain add acme-sites.dev --host-mode      # or sites at https://<site>.acme-sites.dev/
npx backyard-cloudflare@latest access enable --allow-email-domain acme.com   # company login (Cloudflare Access)
npx backyard-cloudflare@latest status                                     # read-only live check
```

To upgrade, re-run `setup`: secrets, the deploy token, R2, the domain, Access and your variables
are kept. A global install works too: `npm i -g backyard-cloudflare && backyard-cloudflare setup`.
With the Backyard CLI, `backyard cloudflare setup` installs this package (verified against its
release) and logs the CLI in.

## Commands

| Command | What it does |
|---|---|
| `setup` | Deploy or upgrade Backyard; re-running keeps every choice |
| `domain add <host>` | Serve the hub on your own domain (a Workers custom domain); `--host-mode` puts sites on their own hosts |
| `domain remove` | Remove that domain again: back to workers.dev |
| `access enable` | Company login with Cloudflare Access (needs a custom domain) |
| `access disable` | Turn company login off (`--delete-apps` also deletes the Access applications it created) |
| `status` | Read-only live check of the Worker, its domain and Access (`--json`) |

Every command takes `--worker-name <name>` (default `backyard`), `--yes` and `--non-interactive`;
`backyard-cloudflare <command> --help` lists its options and `--version` prints the package and
wrangler versions.

Exit codes: `0` done · `1` failed (a re-run resumes) · `2` usage error, or input needed in
non-interactive mode · `3` refused for safety, nothing was changed · `4` applied, but a check
(DNS, certificate, readiness, Access) is still pending: re-run the same command later · `130` interrupted.

## CI and agents

```sh
export CLOUDFLARE_API_TOKEN=… CLOUDFLARE_ACCOUNT_ID=…    # API token from the "Edit Cloudflare Workers" template
npx backyard-cloudflare@<version> setup --non-interactive --workers-subdomain acme
```

Everything that would prompt has a flag; in non-interactive mode a missing answer exits with code 2
and names the flag. The browser login, the one-time Zero Trust onboarding and the dashboard steps
of the guided mode need a person.

## Permissions

* **Your wrangler login** (the browser login, or `CLOUDFLARE_API_TOKEN`) covers deploys, secrets,
  R2, the workers.dev subdomain, zones, custom domains and routes.
* **Two steps need more:** the wildcard DNS record of `domain add --host-mode` (Zone · DNS · Edit)
  and the Access applications of `access enable` (Account · Access: Apps and Policies · Edit and
  Account · Access: Organizations, Identity Providers, and Groups · Read). Either export an API
  token with those permissions as `BACKYARD_CF_API_TOKEN` for that one command (it is never
  stored and never passed to wrangler), or follow the dashboard steps the command prints: it
  detects and verifies the result itself.
* Deploys never change routes or custom domains, and a deploy that would turn Cloudflare Access
  off is refused unless you confirm it (`setup --confirm-auth-change`).

Behind an HTTP proxy: wrangler uses `HTTPS_PROXY`, but the package's own API calls and checks
use Node's `fetch`, which needs `NODE_USE_ENV_PROXY=1` as well (Node 22.21 or later). The
commands warn when they see a proxy variable without it.

The settings of each Worker are kept in `~/.config/backyard/cloudflare/<worker>.json` (or under
`$XDG_CONFIG_HOME`), mode 0600. On a machine without that file, `setup` adopts the configuration
of the deployed Worker.

## More

* [The full guide](https://github.com/thasami/backyard/tree/main/deploy/cloudflare#readme): custom
  domains, host mode, Cloudflare Access, environment variables, GitHub Actions, troubleshooting.
* [Backyard](https://github.com/thasami/backyard#readme): the CLI, the browser SDK and the other
  ways to run it.

License: Apache-2.0.
