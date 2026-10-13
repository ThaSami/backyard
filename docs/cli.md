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
| `--json` | | Machine-readable JSON output (for scripts and agents). |
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
| `BACKYARD_CONFIG`, `BACKYARD_CONFIG_YAML`, `BACKYARD_SECRET`, `PORT`, … | Server configuration, see [configuration](configuration.md#loading). |

### Files

- `~/.config/backyard/config.json` (mode `0600`) — profiles: server, gateway, token and its
  expiry, device-flow refresh token, preferred deploy method, the server's `/info`. Several
  servers: `backyard login --profile staging --server …`, then `--profile staging`.
- `.backyard.json` — per-project settings (`site`, `dir`, `method`, `ignore`), searched in the
  current folder and its parents.
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
server still runs in dev mode on localhost.

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

Ignored by default: `.git/`, `node_modules/`, `.claude/`, `.cursor/`, `.github/`, `CLAUDE.md`,
`AGENTS.md`, `.backyard.json`, dotfiles (except `.well-known/`) plus `.backyardignore` lines.

```sh
backyard deploy
backyard deploy dist --site lunch-vote
backyard deploy --dry-run
backyard deploy --method s3 -y
```

With `--json`: `{site, url, method, dry_run, uploaded, unchanged, deleted, bytes, files,
command, meta}`.

### `backyard pull <site> [dir]`

Download the deployed files of a site into `dir` (default `./<site>`) — handy to fork or remix
another site. Local files that differ from the site are not overwritten unless `--force`.

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--force` | `false` | Overwrite local files that differ. |

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
| `backyard sites list [filter]` (alias `ls`) | List sites; `filter` matches name, title or description. |
| `backyard sites info <site>` | Show a site's details (title, owners, last deploy, size). |
| `backyard sites delete <site>` (alias `rm`) | Delete a site and its files. `-y`/`--yes` skips the confirmation. Admins, or owners in ownership mode. |

### `backyard token`

| Command | Meaning |
|---|---|
| `backyard token create` | Mint a personal API token acting as you, e.g. for CI (`BACKYARD_TOKEN`). Printed on stdout. Flags: `--name` (default `cli`, shown in audit logs), `--ttl` (default `720h`; `90d` style accepted; at most the server's `auth.token_ttl`). |
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

### `backyard cloudflare setup [-- setup-args…]`

Provision (or upgrade — re-running is safe) the Backyard Workers edition on your own Cloudflare
account, on the free plan: one Worker plus SQLite-backed Durable Objects, Workers AI, and an
optional R2 bucket. It runs `npm ci` and `npm run setup` in `deploy/cloudflare`, which logs in
with wrangler (browser) if needed, creates secrets and deploys. Afterwards this CLI is logged in
to the new server with its deploy token. Requires Node.js 20+ and npm. See
[Cloudflare](cloudflare.md).

| Flag | Default | Meaning |
|---|---|---|
| `--dir <path>` | found from the current directory | Path to `deploy/cloudflare`. |
| `--download` | `false` | Download the Backyard source from GitHub when no checkout is found. |
| `--ref <ref>` | this CLI's version (`main` for dev builds) | Git tag or branch to download. |
| `--no-login` | `false` | Do not log this CLI in to the new server afterwards. |
| `--skip-install` | `false` | Skip `npm ci`. |

Arguments after `--` go to the setup script, e.g. `-- --worker-name team`, `-- --r2`,
`-- --var AUTH_MODE:access`, `-- --rotate-token`.

```sh
backyard cloudflare setup
backyard cloudflare setup --download
backyard cloudflare setup --dir ~/src/backyard/deploy/cloudflare -- --worker-name backyard
```

### `backyard version`

Print the version. `backyard completion bash|zsh|fish|powershell` generates shell completion.
