# CLI reference

One binary, `backyard`, is the server, the deploy CLI and the MCP server. This page mirrors
`backyard <command> --help`.

```text
Build and publish sites:   deploy · dev · init · mcp · pull · skill
Account and sites:         login · logout · open · sites · token · whoami
Run and operate Backyard:  cloudflare · config · gateway · serve · version
Other:                     completion · help
```

## Global flags

Every command accepts:

| Flag | Environment | Meaning |
|---|---|---|
| `--server <url>` | `BACKYARD_SERVER` | Backyard server (hub) URL. |
| `--gateway <url>` | `BACKYARD_GATEWAY` | Deploy gateway URL, if not `<server>/__backyard/api/deploy`. |
| `--token <token>` | `BACKYARD_TOKEN` | API token (overrides the stored login). |
| `--profile <name>` | `BACKYARD_PROFILE` | CLI profile to use (default `default`). |
| `--json` | | Machine-readable JSON output (for scripts and agents; of the `backyard cloudflare` commands, only `status` takes it). |
| `-v`, `--verbose` | | Verbose logging. |
| `-h`, `--help` | | Help for any command. |
| `--version` | | Print the version (root command). |

### Environment

| Variable | Meaning |
|---|---|
| `BACKYARD_SERVER`, `BACKYARD_GATEWAY`, `BACKYARD_TOKEN`, `BACKYARD_PROFILE` | As the flags above. |
| `BACKYARD_DEPLOY_METHOD` | Default for `deploy --method`. |
| `XDG_CONFIG_HOME` | Location of the CLI config (`$XDG_CONFIG_HOME/backyard/config.json`). |
| `BROWSER` | Command used to open URLs. |
| `BACKYARD_GITHUB_URL` | GitHub base URL of the release download of `backyard cloudflare` (default `https://github.com`). |
| `BACKYARD_CONFIG`, `BACKYARD_CONFIG_YAML`, `BACKYARD_SECRET`, `PORT`, … | Server configuration, see [configuration](configuration.md#loading). |

### Files

- `~/.config/backyard/config.json` (mode `0600`) — profiles: server, gateway, token and its
  expiry, device-flow refresh token, preferred deploy method, the server's `/info`. Several
  servers: `backyard login --profile staging --server …`, then `--profile staging`.
- `.backyard.json` — per-project settings (`site`, `dir`, `method`, `ignore`), searched in the
  current folder and its parents. `dir` is relative to the file and must stay inside its
  folder: an absolute path, a `..` that leaves it or a symlink leading out is an error.
- `.backyardignore` — gitignore-style deploy exclusions at the root of the deployed folder.

---

## Build and publish sites

### `backyard init [site]`

Create a new site: `index.html`, `.backyard.json`, `.backyardignore`, `CLAUDE.md`, `AGENTS.md`
and the Backyard skill for coding agents (`.claude/skills/backyard`). Existing files are never
overwritten unless `--force`. With a site name and no `--dir`, the site is created in
`./<site>`; without a site name the current folder is initialized and its name becomes the site
name.

| Flag | Default | Meaning |
|---|---|---|
| `--agents <list>` | `claude,codex` | Agent integrations: `claude` (CLAUDE.md + skill), `codex` (AGENTS.md), `cursor` (`.cursor/rules/backyard.mdc`), `copilot` (`.github/copilot-instructions.md`). |
| `-d`, `--dir <dir>` | `./<site>`, or `.` without a site name | Directory to initialize. |
| `-f`, `--force` | `false` | Overwrite existing files. |
| `-t`, `--template <name>` | `blank` | `blank` or `app` (a starter app with a shared, live list). |

```sh
backyard init lunch-vote
backyard init --agents claude,cursor,copilot
backyard init dashboard --template app --dir ./web
```

### `backyard dev [dir]`

Serve a folder as a live site at `http://<site>.localhost:<port>/` with a complete local backend
(database, realtime, files, AI if configured); edits show up on reload. Data is kept in
`./.backyard-data`. Pass `--config` to use AI providers or a warehouse from a server config; the
server still runs in dev mode on localhost. Besides the site and hub URLs it prints a `check:`
line, `curl -s -H 'Host: <site>.localhost:<port>' http://127.0.0.1:<port>/`, for tools that
cannot resolve `*.localhost` (containers, CI, scripts); `http://localhost:<port>/` is the dev
hub, not the site.

| Flag | Default | Meaning |
|---|---|---|
| `-c`, `--config <file>` | none | Optional server config (AI providers, warehouse…). |
| `-p`, `--port <n>` | `4321` | Port to listen on (127.0.0.1). |
| `--site <name>` | `.backyard.json` site or the folder name | Site name. |
| `--no-open` | `false` | Do not open the browser. |

### `backyard deploy [dir]`

Publish a folder as a site. Deploys are diffed (unchanged files are not uploaded) and overwrite
the site in place, like rsync: files missing locally are deleted. The site name comes from
`--site`, `.backyard.json` or the folder name. See [deploying sites](deploying-sites.md).

| Flag | Default | Meaning |
|---|---|---|
| `-s`, `--site <name>` | `.backyard.json` site or folder name | Site name. |
| `-m`, `--method <m>` | `auto` (`BACKYARD_DEPLOY_METHOD`) | `auto`, `api`, `s3`, `gcs`, `azure` or `rsync`. |
| `-n`, `--dry-run` | `false` | Show what would be deployed without changing anything. |
| `-y`, `--yes` | `false` | Do not ask before overwriting a site last deployed by someone else. |
| `--message <text>` | none | Deploy message (api method). |
| `--open` | `false` | Open the site in the browser afterwards. |
| `--concurrency <n>` | `8` | Parallel uploads (api method). |

Methods: `api` uploads through the deploy gateway (default when logged in); `s3` runs `aws s3
sync` into the server's bucket; `gcs` runs `gcloud storage rsync` (or `gsutil rsync`); `azure`
runs `az storage blob sync`; `rsync` targets the server's `deploy.rsync_target`; `auto` picks
`api` when the gateway is reachable and you are logged in (or anonymous deploys are allowed),
else the first direct method the server advertises whose tool is installed.

Never uploaded: `.git/`, `.backyard.json` and `.backyardignore` (in any letter case, at any
depth; nothing can re-include them). Ignored by default: `node_modules/`, `.claude/`,
`.cursor/`, `.github/`, `CLAUDE.md`, `AGENTS.md`, `.DS_Store` and dotfiles (except
`.well-known/`), plus the lines of `.backyardignore` and the `.backyard.json` `ignore` list,
which may re-include those defaults with `!` (e.g. `!.htaccess`). See
[deploying sites](deploying-sites.md#backyardignore).

The direct methods (`s3`, `gcs`, `azure`) never delete what their excludes match, so after the
sync the CLI lists the site through the gateway and removes stored files that the ignore rules
exclude (rsync does this itself with `--delete-excluded`), then refreshes the site. A failed
removal fails the deploy; without a usable gateway a note says what was left.

```sh
backyard deploy
backyard deploy dist --site lunch-vote
backyard deploy --dry-run
backyard deploy --method s3 -y
```

With `--json`: `{site, url, method, dry_run, uploaded, unchanged, deleted, bytes, files,
command, cleanup, meta}` — `command` is the sync command of a direct method and `cleanup` the
commands removing ignored files (listed by dry runs too).

### `backyard pull <site> [dir]`

Download the deployed files of a site into `dir` (default `./<site>`) — handy to fork or remix
another site. Local files that differ from the site are not overwritten unless `--force`.

Files a deploy never publishes are skipped, even if the site holds them and even with
`--force`: dotfiles and dot-folders (`.git/`, `.claude/`, `.env`…; `.well-known/` is kept),
`node_modules/`, `.backyard.json`, `.backyardignore`, `CLAUDE.md` and `AGENTS.md`, compared the
way case-insensitive and Windows file systems resolve names. A site cannot plant repository
metadata (a crafted `.git/config`), project settings or agent instructions in your folder. The
skipped paths are listed on stderr; with `--json` the output is `{site, dir, downloaded,
unchanged, skipped}`.

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--force` | `false` | Overwrite local files that differ (never skipped files). |

### `backyard skill install`

Install the Backyard skill (API reference, recipes and deploy instructions for coding agents)
into `~/.claude/skills/backyard` (`--global`, the default) or `./.claude/skills/backyard`
(`--project`). An existing copy is replaced after confirmation.

| Flag | Default | Meaning |
|---|---|---|
| `--global` | yes | Install into `~/.claude/skills`. |
| `--project` | | Install into `./.claude/skills`. |
| `-y`, `--yes` | `false` | Replace an existing installation without asking. |

### `backyard mcp`

Run a Model Context Protocol server over stdin/stdout so coding agents (Claude Code, Cursor,
Codex…) can read the Backyard docs, scaffold and deploy sites, and browse other sites' code,
using this CLI's login. Logs go to stderr. Tools: `backyard_docs`, `backyard_init`,
`backyard_deploy`, `backyard_list_sites`, `backyard_site_files`, `backyard_read_site_file`,
`backyard_whoami`, `backyard_open_url` (see [agents](agents.md#mcp-server)).

Start it in the project folder: `backyard_init` and `backyard_deploy` only use folders inside
the working directory, and any other folder only when the agent names it by the absolute path of
an existing directory; the filesystem root, the home directory, folders holding credentials
(`~/.ssh`, `~/.config`, `~/Library`, `~/AppData`, the CLI's own config folder…) and hidden
folders outside the working directory are always refused. Without a `dir`, `backyard_deploy`
publishes the project's `.backyard.json` `dir`, like `backyard deploy`.
`backyard_read_site_file` refuses binary file types without downloading them, and its content
is untrusted data written by other people. Tool results and errors never contain tokens or
signed upload URLs.

```sh
claude mcp add backyard -- backyard mcp
```

---

## Account and sites

### `backyard login`

Sign in to a Backyard server and store the credentials in a CLI profile. By default the CLI
opens your browser, where your company login authorizes a personal API token. Servers that allow
anonymous deploys need no credentials. If Backyard is behind an identity-aware proxy (IAP, ALB,
Cloudflare Access…), pass the proxy-exempt deploy gateway with `--gateway` (an external gateway
advertised by the server is adopted automatically). See
[authentication](authentication.md#cli-login).

| Flag | Meaning |
|---|---|
| `--device` | Use the OAuth device authorization flow against your IdP (if the server enables it). |
| `--no-browser` | Do not open a browser; open the printed URL anywhere and paste the token. |
| `--token <t>` (global) | Store an existing token; `-` reads it from stdin. |
| `--server`, `--gateway`, `--profile` (global) | Which server, gateway and profile. |

```sh
backyard login --server https://backyard.acme.com
backyard login --server https://backyard.acme.com --gateway https://deploy.backyard.acme.com
echo "$BACKYARD_TOKEN" | backyard login --server https://backyard.acme.com --token -
backyard login --profile staging --server https://backyard-staging.acme.com --device
```

### `backyard logout`

Remove the credentials of the active profile (the server settings are kept).

### `backyard whoami`

Show the signed-in user, server, profile and login method.

### `backyard open [site]`

Open a site (or, without a name, the hub) in the browser.

### `backyard sites`

| Command | Meaning |
|---|---|
| `backyard sites list [filter]` (alias `ls`) | List the sites you can see — every site, unless the server lists each site only to its owners and admins (`sites.listing: owners`, see [owners-only site listing](deploying-sites.md#owners-only-site-listing)); `filter` matches name, title or description. |
| `backyard sites info <site>` | Show a site's details (title, owners, last deploy, size). Finds any site by name, listed to you or not: "not found" means the name is free. |
| `backyard sites delete <site>` (alias `rm`) | Delete a site and its files. `-y`/`--yes` skips the confirmation. Needs a signed-in identity (not a guest); in ownership mode a site that has owners can be deleted only by them and admins. |

### `backyard token`

| Command | Meaning |
|---|---|
| `backyard token create` | Mint a personal API token acting as you, e.g. for CI (`BACKYARD_TOKEN`). Printed on stdout. Flags: `--name` (default `cli`, shown in audit logs), `--ttl` (default `720h`; `90d` style accepted; at most the server's `auth.token_ttl`). When your stored credential is a Backyard token (a browser `backyard login`), the new token expires no later than that login — a token cannot extend its own lifetime; after `backyard login --device` it gets the full `--ttl`. Behind an identity-aware proxy, `POST /__backyard/api/tokens` with an `Authorization` header must be exempt from the proxy (the AWS, Google Cloud and Cloudflare Terraform modules do this). |
| `backyard token hash [token]` | Print the hex SHA-256 of a static bearer token and the `backyard.yaml` snippet that accepts it. `-` (or nothing) reads the token from stdin. Flag: `--name` (default `ci`). |

```sh
backyard token create --name github-actions --ttl 168h
openssl rand -hex 32 | tee ci-token.txt | backyard token hash --name ci
```

---

## Run and operate Backyard

### `backyard serve`

Run the Backyard server: hub, sites, site APIs, websockets and the embedded deploy gateway.
Configuration comes from `--config`, `$BACKYARD_CONFIG_YAML`, `$BACKYARD_CONFIG` or
`./backyard.yaml`. `--dev` fills in a zero-dependency local setup (SQLite + files under
`./.backyard-data`, no auth). Stops gracefully on `SIGINT`/`SIGTERM`.

| Flag | Meaning |
|---|---|
| `-c`, `--config <file>` | Config file (default `$BACKYARD_CONFIG` or `./backyard.yaml`). |
| `--dev` | Developer defaults: localhost, no auth, local storage. |

### `backyard gateway`

Run only the deploy gateway as a standalone service: no sites, no site APIs, bearer-token
authentication only. Use it when the hub sits behind an identity-aware proxy that CLIs cannot
pass. Flag: `-c`, `--config <file>`.

### `backyard config`

| Command | Meaning |
|---|---|
| `backyard config example` | Print a fully commented example `backyard.yaml`. |
| `backyard config validate [file]` | Load a config (file argument, else `$BACKYARD_CONFIG_YAML`, `$BACKYARD_CONFIG` or `./backyard.yaml`) with defaults and environment expansion applied, and report every problem. `--dev` applies dev defaults first. |

### `backyard cloudflare`

Run Backyard on your own Cloudflare account, on the free plan: one Worker with SQLite-backed
Durable Objects, Workers AI and an optional R2 bucket. No checkout, build or Terraform needed.
See [Cloudflare](cloudflare.md).

| Command | Meaning |
|---|---|
| `backyard cloudflare setup` | Deploy or upgrade Backyard on your Cloudflare account (Workers free plan); re-running keeps every choice: secrets, the deploy token, R2, AI, the site listing, the domain, Access and the `--var` overrides. It logs in with wrangler (in the browser) when needed, asks which account to use when the login reaches several, registers the account's workers.dev subdomain when it has none, creates the R2 bucket when R2 is enabled, sets the secrets and deploys. A machine without saved setup state adopts the deployed configuration first, and a deploy that would turn a live Cloudflare Access off is refused. |
| `backyard cloudflare domain add <host>` | Serve Backyard on your own domain: `<host>` becomes a Workers custom domain (Cloudflare creates its DNS record and certificate) and the hub moves there, with sites at `https://<host>/s/<site>/`. With `--host-mode` sites get their own hosts, `https://<site>.<host>/`: it also adds the route `*.<host>/*` and a proxied wildcard DNS record, created with `BACKYARD_CF_API_TOKEN` (a token with Zone · DNS · Edit) or added by you in the dashboard from the printed values (Backyard detects it). Existing DNS records and other Workers' domains are never replaced without `--replace-dns-record` or `--take-over`. |
| `backyard cloudflare domain remove` | Remove the custom domain this tool added: back to workers.dev. |
| `backyard cloudflare access enable` | Turn on company login with Cloudflare Access (needs a custom domain): two self-hosted Access applications, one for people (who may sign in: `--allow-email`, `--allow-email-domain`) and one that lets the machine endpoints the Worker authenticates itself (deploys, token minting, health checks) through. With `BACKYARD_CF_API_TOKEN` (Access: Apps and Policies · Edit and Access: Organizations, Identity Providers, and Groups · Read) Backyard creates them; otherwise it prints the dashboard steps and detects the result (`--team` and `--aud` name applications you already have). Access is verified at the edge before the Worker switches to it. |
| `backyard cloudflare access disable` | Turn company login (Cloudflare Access) off; `--delete-apps` also deletes the applications this tool created. |
| `backyard cloudflare status` | Read-only live check of the Worker, its domain and Cloudflare Access: versions, URLs, the R2 bucket, workers.dev, the custom domain (and its route, DNS record and certificate in host mode), Access and the deploy token. Each check prints ok, pending or failed. It never logs in: without a wrangler login the live Cloudflare checks are skipped. |

These commands run **backyard-cloudflare**, the engine of the Workers edition (Node.js):

- With `--dir`, the engine is that checkout's `deploy/cloudflare`. A `backyard` built from source
  also uses the checkout it runs in (found from the current directory, never outside its
  repository); a release build uses a checkout only with `--dir`. Before the engine runs,
  `npm ci` installs its dependencies (`npm install` when there is no lockfile), unless
  `node_modules` is newer than `package-lock.json` or `--skip-install` is given.
- Otherwise it is the package of this CLI release,
  `<BACKYARD_GITHUB_URL>/thasami/backyard/releases/download/v<version>/backyard-cloudflare-<version>.tgz`
  (`BACKYARD_GITHUB_URL` defaults to `https://github.com`). It is installed once with npm into
  your user cache (`backyard/cloudflare-engine/<sha256>`) after its SHA-256 has been checked
  against the digest built into this CLI; later runs reuse it without network access. A new
  install removes the older engines there, keeping the previous one and this release's.
- `--package` installs another package, a local `.tgz` or an https URL, which is not verified
  against the release. Development builds (`go install`, `make build`) have no package pinned:
  they need a checkout, `--dir` or `--package` (`make cloudflare-pack` builds one), or install a
  release build (`install.sh`). Once a Backyard release has published it to npm,
  `npx backyard-cloudflare@latest setup` followed by the `backyard login --server <hub> --token <token>`
  it prints works too; until then npm has nothing, or something that is not Backyard, under
  that name.

Requires Node.js 22 or later and npm. Afterwards `setup` and `domain add`/`remove` log this CLI
in to the Worker with its deploy token (unless `--no-login`), and the profiles on the Worker's
previous URL or its workers.dev URL move to its new URL, keeping their credentials. `setup`
then prints the next step (`backyard init`, `backyard deploy`) when the current profile is one
for the Worker; when this CLI cannot log in (the deploy token is not saved on this machine, or
the Worker rejects it), it prints the `backyard login --server <hub> --token …` command to run
instead. With `--no-login` the engine prints that command itself. The Worker's profile becomes
the current one, except when the current profile is for another server and the Worker already
has a profile of its own: a domain change or a setup re-run then updates that profile and keeps
the current one (`--profile` picks the profile explicitly).
`access enable`/`disable` refresh the cached server info of the profiles on the Worker. While
Cloudflare Access is on, a personal login (`backyard login --server <hub>`, with or without
`--token`) is never replaced by the deploy token; a profile named with `--profile` is still
logged in, unless it is that personal login.

Flags of this CLI, for every subcommand (not passed to the engine). Everything else, and
everything after a literal `--`, goes to the engine unchanged:
`backyard cloudflare setup -- --help` shows the engine's own help. `--server`, `--gateway` and
`--token` are not used here, and `--json` is an engine option of `backyard cloudflare status`
only (`setup`, `domain` and `access` reject it with exit code 2).

| Flag | Meaning |
|---|---|
| `--dir <path>` | Path to `deploy/cloudflare` of a Backyard checkout (default: found from the current directory in builds from source). |
| `--package <path\|url>` | A backyard-cloudflare `.tgz` (local file or https URL) to use instead of this release's package. |
| `--skip-install` | Checkout only: skip `npm ci` (skipped anyway when `node_modules` is newer than `package-lock.json`). |
| `--no-login` | Do not log this CLI in or update its profiles afterwards. |
| `--profile <name>` | CLI profile to log in (env `BACKYARD_PROFILE`). |
| `-v`, `--verbose` | Verbose logging. |

Global options of the engine, for every subcommand:

| Option | Meaning |
|---|---|
| `--worker-name <name>` (alias `--name`) | Worker name (default `backyard`). |
| `-y`, `--yes` | Answer yes to confirmations (never implies `--take-over`, `--replace-dns-record` or `--confirm-auth-change`). |
| `--non-interactive` | Never prompt (also when `CI` is set or there is no terminal). |
| `-h`, `--help` | Show help. |

`backyard cloudflare setup [options]`:

| Option | Meaning |
|---|---|
| `--r2` | Use an R2 bucket for big files (default when R2 is enabled). |
| `--no-r2` | Never use R2 (remembered). |
| `--r2-bucket <name>` | Bucket name (default `<worker>-files`). |
| `--ai` | Serve AI with the Workers AI binding (default; turns AI back on after `--no-ai`). |
| `--no-ai` | Turn AI off: no Workers AI binding, AI calls answer `501` (remembered). |
| `--site-listing <all\|owners>` | List every site (`all`, the default), or each site only to its owners and admins (`owners`; remembered). |
| `--var <KEY:VALUE>` | Override a Worker variable (remembered; `KEY:` drops it). Repeatable. |
| `--rotate-token` | Issue a new `DEPLOY_TOKEN`. |
| `--rotate-secret` | Replace `BACKYARD_SECRET` (revokes every API token). |
| `--skip-deploy` | Only (re)set secrets. |
| `--workers-subdomain <name>` | workers.dev subdomain to register when the account has none. |
| `--workers-dev` | Serve on workers.dev (default). |
| `--no-workers-dev` | Stop serving on workers.dev (needs a custom domain). |
| `--confirm-auth-change` | Allow a deploy that turns Cloudflare Access off or changes its team. |
| `--dry-run` | Build and validate only (no login, no changes). |

`backyard cloudflare domain add <host> [options]` (`domain remove` takes only the global options):

| Option | Meaning |
|---|---|
| `--host-mode` | Sites at `https://<site>.<host>/` (wildcard route and DNS record). |
| `--zone <zone>` | Cloudflare zone of `<host>` (default: found automatically). |
| `--take-over` | Move `<host>` or the `*.<host>/*` route from another Worker to this one. |
| `--replace-dns-record` | Replace existing DNS records at `<host>`. |
| `--wait <seconds>` | How long to wait for DNS, certificate and readiness (default 300). |

`backyard cloudflare access enable [options]`:

| Option | Meaning |
|---|---|
| `--allow-email <email>` | Allow this email. Repeatable. |
| `--allow-email-domain <domain>` | Allow every email at this domain. Repeatable. |
| `--team <team>` | Zero Trust team of existing Access applications. |
| `--aud <tag>` | AUD tag of the existing hub application. |
| `--wait <seconds>` | How long to wait for Access (default 120; 900 while waiting for dashboard steps). |

`backyard cloudflare access disable [options]`:

| Option | Meaning |
|---|---|
| `--delete-apps` | Also delete the Access applications and policies this tool created (needs `BACKYARD_CF_API_TOKEN`). |

`backyard cloudflare status [options]`:

| Option | Meaning |
|---|---|
| `--json` | Machine-readable output. |

Exit codes (the engine's, kept by this CLI, also after it logged in):

| Code | Meaning |
|---|---|
| `0` | Done, or nothing to do (`status`: every check is ok). |
| `1` | A Cloudflare, wrangler, network or file operation failed (a re-run resumes); `status`: the Worker does not exist. |
| `2` | Usage error, or input or confirmation needed in non-interactive mode. |
| `3` | Refused for safety (conflict, foreign object, auth downgrade…): nothing was changed. |
| `4` | Applied, but verification (DNS, certificate, readiness, Access) is pending or failed: re-running the same command resumes. `status`: a check is pending or failed. |
| `130` | Interrupted. |

```sh
backyard cloudflare setup
backyard cloudflare setup --worker-name team --no-r2
backyard cloudflare setup --no-ai                                      # an account without Workers AI
backyard cloudflare setup --site-listing owners                        # people see only their own sites
backyard cloudflare setup --non-interactive --workers-subdomain acme   # CI
backyard cloudflare setup --package ./dist-npm/backyard-cloudflare-0.4.0.tgz
backyard cloudflare domain add backyard.acme.com
backyard cloudflare domain add acme-sites.dev --host-mode
backyard cloudflare access enable --allow-email-domain acme.com
backyard cloudflare access disable --delete-apps
backyard cloudflare status --json
```

### `backyard version`

Print the version. `backyard completion bash|zsh|fish|powershell` generates shell completion.
