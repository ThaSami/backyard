# Deploying sites

A site is a folder. Deploying copies the folder to the server and makes it the site, like
`rsync --delete`: new and changed files are uploaded, unchanged files are skipped, files that no
longer exist locally are removed. Data in the site's database and uploads is never touched.

```sh
backyard deploy                      # the current project (.backyard.json) or folder
backyard deploy dist --site sales-dash
backyard deploy --dry-run            # what would be uploaded and deleted
backyard deploy -y --open            # no overwrite prompt, open the site afterwards
backyard deploy --method s3          # sync straight into the server's bucket
```

The URL is printed at the end: `https://<site>.backyard.acme.com/` in host mode,
`https://backyard.acme.com/s/<site>/` in path mode. People without the CLI can drag a folder
onto the hub's "New site" panel.

## Site names

The name comes from `--site`, else `site` in `.backyard.json`, else the folder name. Names are
DNS labels — `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` — and may not be reserved
(`sites.reserved`, by default `www, api, admin, hub, auth, static, assets, cdn, docs, mail,
backyard, s`).

## Deploy methods

`--method` (or `method` in `.backyard.json`, or `BACKYARD_DEPLOY_METHOD`) picks how the files
travel. Bucket names, prefixes and the rsync target come from the server's
`/__backyard/api/info` — nobody types them.

| Method | What runs | Needs |
|---|---|---|
| `api` | The [deploy gateway protocol](spec/SPEC.md#7-deploy-gateway-protocol): the CLI hashes the folder, the gateway answers with upload URLs for new/changed files only, the CLI uploads them (8 in parallel, `--concurrency`) and commits. | A login or token (or anonymous deploys). No cloud account. |
| `s3` | `aws s3 sync <dir> s3://<bucket>/<prefix>sites/<site>/ --delete --exclude …` | AWS CLI with write access to the bucket. |
| `gcs` | `gcloud storage rsync <dir> gs://<bucket>/<prefix>sites/<site> --recursive --delete-unmatched-destination-objects --exclude=<regex>` (falls back to `gsutil -m rsync -r -d -x`) | gcloud (or gsutil) with bucket access. |
| `azure` | `az storage blob sync --account-name <a> -c <container> -s <dir> -d <prefix>sites/<site> --delete-destination true --exclude-pattern …` | Azure CLI with Storage Blob Data Contributor. |
| `rsync` | `rsync -az --delete --exclude … <dir>/ <rsync_target>/<site>/` | SSH access to `deploy.rsync_target` on a server with `storage.type: fs`. |
| `auto` (default) | `api` when the gateway is reachable and you are logged in (or anonymous deploys are allowed); otherwise the first direct method the server advertises whose tool is installed. | |

After a direct sync the CLI asks the gateway to refresh the site (`POST
…/sites/<site>/refresh`), which rebuilds its manifest, title and description. Direct methods
suit platform teams and CI runners that already hold cloud credentials; everyone else uses
`api`. The server lists the methods it supports in `deploy.methods` (default: `api` plus the
storage-native one — `rsync` for fs storage once `deploy.rsync_target` is set).

### What a deploy guarantees

- **Diffed:** a file is skipped when the server's manifest has the same SHA-256 and the stored
  object still matches the manifest (so a change made by a direct sync is noticed).
- **Verified:** uploads are bound to the size and SHA-256 the plan declared (verified by the
  gateway's own upload endpoint and by S3; GCS pins the size), and the commit checks every
  file's size before it deletes anything.
- **Not atomic:** like rsync, visitors may briefly see a mix of old and new files; deletions
  happen last, so a site is never empty mid-deploy.
- **Limits** (`sites.*`): 500 MiB per site, 10,000 files, 100 MiB per file by default;
  `ratelimit.deploys_per_hour` (60) per person.

## Project files

### `.backyard.json`

Written by `backyard init`; the CLI finds it in the current folder or a parent.

```json
{ "site": "lunch-vote", "dir": ".", "method": "auto", "ignore": ["drafts/", "*.psd"] }
```

| Field | Meaning |
|---|---|
| `site` | Site name. |
| `dir` | Folder to deploy, relative to the file — e.g. `"dist"` for build output. Default `"."`. |
| `method` | Preferred deploy method. |
| `ignore` | Extra gitignore-style patterns. |

### `.backyardignore`

Never uploaded, whatever you configure: `.git/`, `.backyard.json`, `.backyardignore`,
`node_modules/`, `.claude/`, `.cursor/`, `.github/`, `CLAUDE.md`, `AGENTS.md`, `.DS_Store` and
every path segment starting with `.` (`.env`, `.backyard-data/`…) except `.well-known/`.

Add more in a `.backyardignore` file at the root of the deployed folder. The syntax is
gitignore's: `#` comments, `!` negation (the last matching pattern wins), a trailing `/` for
directories, a leading or inner `/` anchors to the root, `**` matches any depth, and a path
inside an excluded directory cannot be re-included.

```gitignore
# keys and certificates must never be published
*.pem
*.key
drafts/
/notes.md
!keep.psd
```

`backyard init` writes a starter `.backyardignore`. The same rules are translated into each
direct-sync tool's exclude flags. Symlinks pointing outside the folder are skipped; a file
under `__backyard/` cannot be deployed (that path prefix is reserved).

## Ownership

By default (`sites.ownership: none`) anyone can overwrite any site — the Quick philosophy:
fork, fix and redeploy. The CLI still warns (and asks, on a terminal) before replacing a site
someone else deployed last; `-y` skips the question.

With `sites.ownership: owner`, the first deployer owns the site, and only its owners and
admins may redeploy or delete it (`403 forbidden` otherwise). Owners are managed through the
gateway API:

```sh
curl -X POST -H "Authorization: Bearer $BACKYARD_TOKEN" -H "X-Backyard: 1" \
  -H "Content-Type: application/json" \
  -d '{"add": ["bob@acme.com"], "remove": []}' \
  https://backyard.acme.com/__backyard/api/deploy/sites/lunch-vote/owners
```

Admins (`auth.admins`, `auth.admin_groups`) can always delete or overwrite a site:
`backyard sites delete <site>`.

## CI

Deploy from any pipeline with two environment variables:

- `BACKYARD_SERVER` — the hub URL;
- `BACKYARD_TOKEN` — a [static token](authentication.md#static-tokens) (acts as
  `token:<name>`, never expires, survives people leaving) or a personal API token from
  `backyard token create --name ci --ttl 720h` (acts as you, expires after at most
  `auth.token_ttl`).

```yaml
# .github/workflows/deploy-site.yml
name: Deploy site
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - run: curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh
      - run: backyard deploy ./site --site team-dashboard -y --json
        env:
          BACKYARD_SERVER: https://backyard.acme.com
          BACKYARD_TOKEN: ${{ secrets.BACKYARD_TOKEN }}
          # BACKYARD_GATEWAY: https://deploy.backyard.acme.com   # behind IAP/ALB/Access
```

`--json` prints `{site, url, method, uploaded, unchanged, deleted, bytes, …}`. A runner that
already has cloud credentials can skip Backyard tokens entirely with `--method s3|gcs|azure`.

## Gateways

The deploy gateway is the server side of the `api` method. It runs in three places, all
speaking the same protocol:

| Gateway | URL | Use |
|---|---|---|
| Embedded (default) | `https://<hub>/__backyard/api/deploy` | Every server, `deploy.embedded_gateway: true`. Also accepts the browser session (hub drag-and-drop). |
| `backyard gateway` | its own host/port | A standalone process with the same config: bearer tokens only, no sites. Put it where a proxy-exempt hostname is easy. |
| AWS Lambda | a function URL | For AWS installs behind ALB authentication and for people without cloud accounts. |

Advertise a standalone gateway with `deploy.gateway_url`: `backyard login` reads it from
`/info` and uses it when reachable (or pass `--gateway` / `BACKYARD_GATEWAY`).

### The AWS Lambda gateway

Behind an ALB that authenticates every request, a CLI cannot reach the embedded gateway, and
most employees have no AWS credentials for `aws s3 sync`. The Lambda gateway solves both: a
small function (`cmd/backyard-lambda`) with a public function URL that only accepts Backyard
bearer tokens and hands out presigned S3 upload URLs, so file bytes go straight to the bucket.

The flow for someone without any cloud account:

1. `backyard login --server https://backyard.acme.com` — the browser goes through the ALB and
   Okta, Backyard mints a personal API token, and the CLI adopts the advertised Lambda URL.
2. `backyard deploy` — the CLI talks to the Lambda gateway with that token; uploads go to S3
   with presigned URLs; the commit updates the manifest.

Build and configure:

```sh
make lambda          # dist/backyard-lambda.zip (linux/arm64, provided.al2023 runtime)
```

| Environment | Meaning |
|---|---|
| `BACKYARD_CONFIG_YAML` / `BACKYARD_CONFIG` | The same `backyard.yaml` as the server (storage, auth tokens, OIDC issuer for device-flow tokens, limits). |
| `BACKYARD_SECRET` or `BACKYARD_SECRET_ARN` | The server's secret (must match, so tokens verify), or a Secrets Manager secret read at cold start (plain value, or JSON with `BACKYARD_SECRET`/`secret`). Set only one. |
| `BACKYARD_GATEWAY_URL` | The URL advertised to CLIs (else `deploy.gateway_url`, else derived from the request). |

The [AWS Terraform module](../deploy/terraform/aws/README.md) deploys it
(`enable_lambda_gateway`) and sets `deploy.gateway_url` on the server.

## Troubleshooting

| Problem | Fix |
|---|---|
| `not logged in` / `401` | `backyard login` (tokens expire after `auth.token_ttl`, 30 days by default). In CI set `BACKYARD_TOKEN`. |
| Login works but deploys hang or get HTML back | A proxy in front of the gateway: use the proxy-exempt gateway (`--gateway`). |
| `forbidden` | Ownership mode and you are not an owner, or the gates (`allowed_domains`/`allowed_groups`) exclude you. |
| `invalid site name` | Lowercase letters, digits and `-`, ≤ 63 characters, not reserved. |
| `too_large` | A file or the whole site is over the limits — deploy the build folder, ignore sources. |
| `rate_limited` | More than `deploys_per_hour`; wait. |
| A file is missing on the site | It is ignored (dotfile, default ignore, `.backyardignore`) — check with `--dry-run`. |
| Old content still shows | Reload: sites are served with `Cache-Control: no-cache` and ETags. With several instances, caches expire after `storage.cache_ttl` (5 s). |
