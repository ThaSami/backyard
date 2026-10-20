# Backyard with Docker Compose

Run Backyard on a single machine — a laptop, a VM in your data center, any cloud instance —
without a cloud bucket:

| Service    | Image                            | Role                                                        |
|------------|----------------------------------|-------------------------------------------------------------|
| `backyard` | `ghcr.io/thasami/backyard`       | hub, sites, site APIs, websockets, deploy gateway           |
| `postgres` | `postgres:16-alpine`             | documents (`backyard.db`) and realtime fan-out (LISTEN/NOTIFY) |
| `caddy`    | `caddy:2`                        | TLS for `<domain>` and `*.<domain>`, reverse proxy          |

Site files and uploads live on the `backyard-data` volume (fs storage). Files in this folder:

* `docker-compose.yml` — the stack; `backyard.yaml` — Backyard's configuration (strictly validated:
  unknown keys are errors; `backyard config example` prints every option);
* `Caddyfile` — TLS and proxying; `.env.example` — settings; `init-env.sh` — creates `.env`;
* `test-services.yml` — throwaway databases for contributors running the integration tests
  (`make services-up`), unrelated to the stack above.

## Local quickstart

Needs Docker with Compose v2 and the [Backyard CLI](../../scripts/install.sh).

```sh
cd deploy/docker-compose
./init-env.sh localhost      # .env with fresh secrets; prints your deploy token
docker compose up -d
```

* Hub: <https://localhost> — sites: `https://<site>.localhost` (browsers resolve `*.localhost`
  to your machine; no DNS needed).
* Caddy issues certificates from its own local CA. Trust it once to get rid of the warning:

  ```sh
  docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt ./caddy-root.crt
  # macOS:  sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain caddy-root.crt
  # Debian: sudo cp caddy-root.crt /usr/local/share/ca-certificates/ && sudo update-ca-certificates
  ```

  The CLI uses the system trust store; Firefox has its own (Settings → Certificates → Import).
* Deploy something:

  ```sh
  backyard login --server https://localhost --token <token printed by init-env.sh>
  backyard init hello && cd hello && backyard deploy     # → https://hello.localhost/
  ```

Ports 80/443 taken? Set `HTTP_PORT=8080`, `HTTPS_PORT=8443` and
`BACKYARD_PUBLIC_URL=https://localhost:8443` in `.env`.

To run a build of your checkout instead of the release image, uncomment `build: ../..` in
`docker-compose.yml` and run `docker compose up -d --build`.

## Production

### 1. DNS

Point both the hub and a wildcard for the sites at the machine:

```
backyard.acme.com.     A   203.0.113.10
*.backyard.acme.com.   A   203.0.113.10
```

(Only single-label subdomains are sites. Without wildcard DNS, set `url_mode: path` in
`backyard.yaml` — sites are then served at `https://backyard.acme.com/s/<site>/`, sharing one
origin.)

### 2. Settings

```sh
./init-env.sh backyard.acme.com
$EDITOR .env      # BACKYARD_IMAGE (pin a version), BACKYARD_TLS, ...
```

Keep `.env` private (it holds the server secret and database password; it is git-ignored and
created with mode 0600). Rotating `BACKYARD_SECRET` signs everyone out and revokes all API
tokens.

### 3. TLS — pick one `BACKYARD_TLS` mode

| Mode | Certificates | Needs |
|---|---|---|
| `internal` | Caddy's private CA | devices that trust Caddy's root (push it with your MDM) |
| `files` | your wildcard certificate (`certs/fullchain.pem`, `certs/privkey.pem`) | a cert for `<domain>` + `*.<domain>` from your internal PKI or a CA |
| `dns` | one Let's Encrypt wildcard via the DNS-01 challenge | a Caddy image with your DNS provider plugin, an API token for the zone |
| `on_demand` | one Let's Encrypt certificate per site, issued on first visit | public DNS and inbound 80/443 |

* **dns** (recommended for internet-facing installs): build Caddy with your provider, e.g.
  Cloudflare:

  ```dockerfile
  FROM caddy:2-builder AS builder
  RUN xcaddy build --with github.com/caddy-dns/cloudflare
  FROM caddy:2
  COPY --from=builder /usr/bin/caddy /usr/bin/caddy
  ```

  Use it in `docker-compose.yml` (`build:` on the `caddy` service), uncomment the `(tls_dns)`
  snippet in the `Caddyfile`, pass `CLOUDFLARE_API_TOKEN` to the `caddy` service and set
  `BACKYARD_TLS=dns`. Other providers: `caddy-dns/route53`, `caddy-dns/googleclouddns`,
  `caddy-dns/azure`, … (<https://caddyserver.com/docs/modules/>).
* **on_demand** works with the stock image. Caddy asks its built-in endpoint (bottom of the
  `Caddyfile`) before issuing, which only allows the hub and single-label subdomains of your
  domain. Every new site name costs one certificate on its first visit, so mind the
  [Let's Encrypt rate limits](https://letsencrypt.org/docs/rate-limits/) (50 per registered
  domain per week) if people create many sites.
* Set `CADDY_GLOBAL_OPTIONS=email ops@acme.com` to receive ACME expiry notices.

### 4. Single sign-on

Without SSO (`auth.mode: none`) anyone who can reach the server can use every site. Inside a
VPN that may be fine; otherwise register an OIDC web application in your IdP (Okta, Entra ID,
Google Workspace, Keycloak, Auth0, …) with redirect URL
`https://backyard.acme.com/__backyard/auth/callback`, put `OIDC_ISSUER`, `OIDC_CLIENT_ID` and
`OIDC_CLIENT_SECRET` in `.env`, and switch `auth` in `backyard.yaml` to the commented `oidc`
block (restrict it with `allowed_domains` / `allowed_groups`, and name admins with `admins` or
`admin_groups`). People then run `backyard login --server https://backyard.acme.com` and sign
in through the browser. The static `deploy` token keeps working for CI.

Behind an identity-aware proxy instead (Cloudflare Access, Pomerium, oauth2-proxy, …) use
`auth.mode: header` — see `backyard config example`.

### Claude connector (optional)

To let people publish sites from Claude with their sign-in, add to `backyard.yaml`:

```yaml
mcp:
  enabled: true
```

and add `https://backyard.acme.com/__backyard/mcp` to Claude as a custom connector (see the
[Claude connector guide](../../docs/claude-connector.md)). Caddy needs nothing else: it has no
request body limit. It needs a sign-in (step 4) to publish as a person; with `auth.mode: none`
the connector is a guest and can publish only with `deploy.allow_anonymous`. Hosted Claude
reaches the server from `160.79.104.0/21` and ChatGPT from the ranges OpenAI publishes in
[`chatgpt-connectors.json`](https://openai.com/chatgpt-connectors.json), so the four machine
paths (`/__backyard/mcp`, `/__backyard/oauth/*`, `/.well-known/oauth-authorization-server`,
`/.well-known/oauth-protected-resource/__backyard/mcp`) must be reachable from there; behind an
identity-aware proxy, exempt exactly those from its login and keep
`/__backyard/auth/authorize` behind it. Backyard fetches Claude's client metadata from
`https://claude.ai` and ChatGPT's from `https://chatgpt.com` when someone connects; without
outbound internet access add `clients: []` to the `mcp` section.

### Owners-only site listing (optional)

The hub, `backyard sites list` and `backyard_list_sites` show every site to everyone by
default. To show each person only the sites they own (admins, such as the `deploy` token, see
every site), set `BACKYARD_SITE_LISTING=owners` in `.env` and uncomment the `sites` block of
`backyard.yaml`:

```yaml
sites:
  listing: ${BACKYARD_SITE_LISTING:-all}
```

then `docker compose up -d`. It implies site ownership (the first deployer owns a site; only
its owners and admins may redeploy or delete it, except that a site without owners, such as one
from before, can be replaced and claimed by any signed-in deployer) and hides sites from the
directory without restricting who can open them. It is meant for a sign-in (step 4): with
`auth.mode: none` every browser is a guest and is listed no site. Images older than the option
refuse the key, so move `BACKYARD_IMAGE` to a release that has it first. Details:
[owners-only site listing](../../docs/deploying-sites.md#owners-only-site-listing).

### 5. Start and upgrade

```sh
docker compose up -d
docker compose logs -f backyard
docker compose run --rm backyard config validate /etc/backyard/backyard.yaml   # after edits
```

Upgrade by changing `BACKYARD_IMAGE` (or `docker compose pull` for a moving tag) and
`docker compose up -d`. Database migrations run on start.

Prometheus metrics are served on `backyard:9090/metrics` inside the compose network (not
published); add your Prometheus to the network or publish the port to a private interface.

## Deploying with rsync (no API, no bucket)

Besides the API (`backyard deploy`, the hub's drag & drop), sites can be pushed with plain
rsync over SSH straight into the sites folder — handy for CI systems and for people who
already think in `rsync -a --delete`. `backyard deploy --method rsync` does exactly that and
then asks the server to refresh the site's metadata.

The container writes as uid/gid **65532**, so the SSH user must be that uid too. Keep the data
on a host directory instead of the named volume, and restrict the user to the sites folder with
[`rrsync`](https://download.samba.org/pub/rsync/rrsync.1) (a python3 script that ships with
rsync ≥ 3.2.4; some distributions keep it under `/usr/share/doc/rsync/scripts/`). `-munge`
makes symlinks inert, so a deploy can't read or write through a link out of `sites/` (symlinks
inside rsync-deployed sites therefore don't resolve); `-no-lock` allows concurrent deploys of
different sites. The keys file belongs to root, so the deploy account can't add keys or change
its forced command:

```sh
# 1. Host directory for Backyard's data, owned by the container user.
sudo install -d -o 65532 -g 65532 -m 0750 /srv/backyard/data
#    In .env: BACKYARD_DATA=/srv/backyard/data
#    (moving an existing install: docker compose down, copy the old volume's
#     contents, e.g. with `docker run --rm -v backyard_backyard-data:/from -v /srv/backyard/data:/to alpine cp -a /from/. /to/`)

# 2. A deploy account with the same uid, a shell (needed for the forced command)
#    and no password.
sudo groupadd --system --gid 65532 backyard-deploy
sudo useradd --system --uid 65532 --gid 65532 --home-dir /srv/backyard/deploy \
     --create-home --shell /bin/sh backyard-deploy
sudo usermod -p '*' backyard-deploy

# 3. One line per person or CI system that may deploy: only rsync, only into sites/.
#    The keys file is root's (0644), not the deploy account's.
sudo install -d -o root -g root -m 0755 /srv/backyard/deploy/.ssh
echo 'restrict,command="rrsync -munge -no-lock /srv/backyard/data/sites" ssh-ed25519 AAAA... alice@acme.com' \
  | sudo tee -a /srv/backyard/deploy/.ssh/authorized_keys
sudo chown root:root /srv/backyard/deploy/.ssh/authorized_keys
sudo chmod 0644 /srv/backyard/deploy/.ssh/authorized_keys
sudo install -d -o 65532 -g 65532 -m 0750 /srv/backyard/data/sites
```

If uid 65532 is already taken on the host, use that existing account for the deploy key
instead. Then advertise the target to CLIs in `.env` and restart:

```sh
BACKYARD_RSYNC_TARGET=backyard-deploy@backyard.acme.com:
docker compose up -d
```

rrsync maps paths into the restricted folder, so the CLI's
`rsync -az --delete <dir>/ backyard-deploy@backyard.acme.com:/<site>/` lands in
`/srv/backyard/data/sites/<site>/`. Test it with `backyard deploy --method rsync --dry-run`.
After an rsync the CLI calls the gateway's `refresh` endpoint (it needs a token from
`backyard login`) so the hub shows the new title and file count right away; otherwise the
site is still served immediately.

## Backups

Everything worth keeping is in two places:

```sh
# Database (documents, users, audit log): consistent online dump.
docker compose exec -T postgres pg_dump -U backyard -Fc backyard > backyard-$(date +%F).dump

# Site files, uploads and metadata (named volume; with BACKYARD_DATA as a host path,
# back up that directory instead).
docker run --rm -v backyard_backyard-data:/data:ro -v "$PWD":/backup alpine \
  tar -czf /backup/backyard-data-$(date +%F).tgz -C /data .
```

Restore into a fresh stack:

```sh
docker compose up -d postgres
docker compose exec -T postgres pg_restore -U backyard -d backyard --clean --if-exists < backyard-2026-01-31.dump
docker run --rm -v backyard_backyard-data:/data -v "$PWD":/backup alpine \
  sh -c 'tar -xzf /backup/backyard-data-2026-01-31.tgz -C /data && chown -R 65532:65532 /data'
docker compose up -d
```

Also keep `.env` (in your secret store): without `BACKYARD_SECRET` existing API tokens and
sessions stop working, and without `POSTGRES_PASSWORD` you need to reset the database password.
Caddy's volumes (`caddy-data`) only hold certificates and can be re-issued.

## Troubleshooting

* `required variable BACKYARD_SECRET is missing a value` — run `./init-env.sh` (or fill `.env`).
* The server exits with a configuration error — run
  `docker compose run --rm backyard config validate /etc/backyard/backyard.yaml`; every problem
  is listed.
* `permission denied` under `/var/lib/backyard` — the data directory must be writable by uid
  65532 (`sudo chown -R 65532:65532 /srv/backyard/data`).
* Websocket or streaming problems behind another proxy/load balancer in front of Caddy — make
  sure it forwards `Upgrade`/`Connection` headers and does not buffer `text/event-stream`.
