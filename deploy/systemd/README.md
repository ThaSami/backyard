# Backyard on a single VM (systemd)

The smallest production setup — and the closest to Shopify's original Quick ("one VM and a
folder"): one `backyard` binary as a hardened systemd service, site files and uploads on the
local disk, documents in an embedded SQLite database. No cloud bucket, no database server, no
containers. A reverse proxy on the same machine (Caddy or nginx) terminates TLS for the hub and
the wildcard site domain.

| Path | What |
|---|---|
| `/usr/local/bin/backyard` | the binary (server + CLI) |
| `/etc/backyard/backyard.yaml` | configuration ([`backyard.yaml`](backyard.yaml); strictly validated) |
| `/etc/backyard/backyard.env` | secrets as `KEY=value` lines (`BACKYARD_SECRET`, the deploy token's SHA-256, API keys); `0640 root:backyard` |
| `/etc/systemd/system/backyard.service` | the unit ([`backyard.service`](backyard.service)) |
| `/var/lib/backyard/` | all data: `sites/`, `uploads/`, `_meta/`, `backyard.db` (SQLite) |

A 1-2 vCPU / 2 GB VM comfortably serves a few thousand internal users; SQLite and the in-memory
realtime hub mean **one instance** (for several replicas use Postgres + Redis, e.g. the Helm
chart or Terraform modules).

## Install

On Debian/Ubuntu, RHEL/Fedora, SUSE, Alpine-with-systemd, amd64 or arm64:

```sh
curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/deploy/systemd/install.sh \
  | sudo sh -s -- --domain backyard.acme.com
```

or from a checkout: `sudo deploy/systemd/install.sh --domain backyard.acme.com`. The script

1. downloads the release for your architecture and verifies it against `checksums.txt`
   (`--version 0.2.0` pins a release; `--binary ./backyard` installs a binary you copied over,
   for air-gapped hosts);
2. creates the `backyard` system user;
3. writes `/etc/backyard/backyard.yaml` (domain filled in) and `/etc/backyard/backyard.env` with a
   fresh `BACKYARD_SECRET` and a **deploy token** — **only if they do not exist yet**. The token is
   printed once at the end (only its SHA-256 is stored); use it with
   `backyard login --server https://<domain> --token <token>`;
4. validates the configuration, installs the unit and (re)starts the service.

Re-run it any time to upgrade; your configuration is kept. `--enable-rsync` additionally
prepares rsync deploys (below), `--no-start` skips starting the service.

Then:

```sh
systemctl status backyard
journalctl -u backyard -f
curl -s http://127.0.0.1:8080/__backyard/healthz      # ok
```

### Manual install

```sh
sudo install -m 0755 backyard /usr/local/bin/backyard
sudo useradd --system --user-group --home-dir /var/lib/backyard --no-create-home \
     --shell /usr/sbin/nologin backyard
sudo install -d -m 0755 /etc/backyard
sudo install -m 0644 backyard.yaml /etc/backyard/backyard.yaml      # edit base_domain / public_url
TOKEN="byd_$(openssl rand -hex 24)"; echo "deploy token: $TOKEN"      # keep it; only the hash is stored
printf 'BACKYARD_SECRET=%s\nBACKYARD_DEPLOY_TOKEN_SHA256=%s\n' "$(openssl rand -hex 32)" \
     "$(printf %s "$TOKEN" | sha256sum | cut -d' ' -f1)" | sudo tee /etc/backyard/backyard.env >/dev/null
sudo chown root:backyard /etc/backyard/backyard.env && sudo chmod 0640 /etc/backyard/backyard.env
sudo install -m 0644 backyard.service /etc/systemd/system/backyard.service
sudo systemctl daemon-reload && sudo systemctl enable --now backyard
```

## Configure

Edit `/etc/backyard/backyard.yaml`, check it, restart:

```sh
sudo sh -c 'set -a; . /etc/backyard/backyard.env; backyard config validate /etc/backyard/backyard.yaml'
sudo systemctl restart backyard
```

`${VAR}` references in the YAML are expanded from `backyard.env`, so secrets such as
`client_secret: "${OIDC_CLIENT_SECRET}"` stay out of the YAML. `backyard config example`
prints every option. Highlights:

* **Single sign-on** — `auth.mode: oidc` with your IdP's issuer and client
  (redirect URL `https://<domain>/__backyard/auth/callback`); or `auth.mode: header` behind an
  identity-aware proxy.
* **Deploy tokens** — `auth.tokens` holds SHA-256 hashes of static bearer tokens. The installer
  creates `deploy` (its hash is `BACKYARD_DEPLOY_TOKEN_SHA256` in `backyard.env`); add more for CI
  with `backyard token hash <token>`. Remove the entry to revoke a token.
* **AI** — the commented `ai` block; put the API key in `backyard.env`.
* **Owners-only site listing** — `sites.listing: owners`, see
  [below](#owners-only-site-listing).
* **Path mode** — without wildcard DNS, `server.url_mode: path` serves sites at
  `https://<domain>/s/<site>/`.

The unit's sandbox (`ProtectSystem=strict`, no capabilities, private `/tmp`, syscall filter, …)
only lets the server write to `/var/lib/backyard`. Adjust it with `systemctl edit backyard`,
e.g. to read a people directory (`identity.directory`) from elsewhere add
`ReadOnlyPaths=/srv/directory.csv`, or to bind `:443` directly see the comment in the unit.

## Reverse proxy and TLS

Backyard listens on `127.0.0.1:8080` (`server.listen`) and trusts `X-Forwarded-*` from
localhost (`server.trusted_proxies`). The proxy needs a certificate for **both** `<domain>`
and `*.<domain>` and must pass websockets (`/__backyard/api/ws`) and unbuffered streams
(`text/event-stream`). DNS: an `A`/`AAAA` record for `<domain>` and one for `*.<domain>`.

### Caddy

Install Caddy from your distribution or <https://caddyserver.com/docs/install>. A wildcard
certificate from Let's Encrypt needs the DNS-01 challenge, i.e. a Caddy build with your DNS
provider's plugin (`xcaddy build --with github.com/caddy-dns/cloudflare`, or download a build
with the plugin from <https://caddyserver.com/download>). `/etc/caddy/Caddyfile`:

```caddyfile
backyard.acme.com, *.backyard.acme.com {
	tls {
		dns cloudflare {env.CLOUDFLARE_API_TOKEN}
	}
	encode zstd gzip
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
```

Alternatives in the `tls` block: `tls /etc/ssl/backyard/fullchain.pem /etc/ssl/backyard/privkey.pem`
(a wildcard certificate from your internal PKI), `tls internal` (Caddy's own CA; devices must
trust it), or on-demand certificates per site with the stock Caddy build — see
[`../docker-compose/Caddyfile`](../docker-compose/Caddyfile) for a complete on-demand setup with
an `ask` endpoint that only allows single-label subdomains of your domain.

### nginx

Get a wildcard certificate, e.g. with certbot and a DNS plugin:

```sh
sudo certbot certonly --dns-route53 -d backyard.acme.com -d '*.backyard.acme.com'
```

`/etc/nginx/conf.d/backyard.conf`:

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 80;
    listen [::]:80;
    server_name backyard.acme.com *.backyard.acme.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name backyard.acme.com *.backyard.acme.com;

    ssl_certificate     /etc/letsencrypt/live/backyard.acme.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/backyard.acme.com/privkey.pem;

    # Deploy uploads and backyard.files (match sites.max_file_bytes / files.max_upload_bytes).
    client_max_body_size 100m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $host;
        # Websockets (realtime, rooms).
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 1h;
        # Streaming AI responses.
        proxy_buffering off;
    }
}
```

## Claude connector

To let people publish sites from Claude with their sign-in, add to
`/etc/backyard/backyard.yaml` and restart:

```yaml
mcp:
  enabled: true
```

The connector URL is `https://<domain>/__backyard/mcp` (see the
[Claude connector guide](../../docs/claude-connector.md)). The Caddy example has no body limit
and the nginx example's `client_max_body_size 100m` is above the 10 MiB a publish call may need;
a smaller limit must stay at `10m` or more. Publishing as a person needs single sign-on
(`auth.mode: oidc`, or `header` behind a proxy); hosted Claude reaches the server from
`160.79.104.0/21`. Behind an identity-aware proxy, exempt the four machine paths
(`/__backyard/mcp`, `/__backyard/oauth/*`, `/.well-known/oauth-authorization-server`,
`/.well-known/oauth-protected-resource/__backyard/mcp`) from its login and keep
`/__backyard/auth/authorize` behind it.

## Owners-only site listing

By default the hub, `backyard sites list` and `backyard_list_sites` show every site to everyone.
To show each person only the sites they own (admins, such as the `deploy` token, see every
site), set `sites.listing: owners` in `/etc/backyard/backyard.yaml`, then validate and restart
as in [Configure](#configure). New installs have the block below, commented out: uncomment
both lines. If your file already has a `sites:` section, add `listing: owners` under it
instead (a second `sites:` key is an error that `backyard config validate` reports):

```yaml
sites:
  listing: owners
```

It implies site ownership (the first deployer owns a site; only its owners and admins may
redeploy or delete it once it has owners) and hides sites from the directory without
restricting who can open them. Sites from before have no owners (ownership was off): each
stays listed to its creator, and until it has an owner any signed-in deployer can replace it
and becomes its owner, and any signed-in user may delete it or set its owners. So give the
sites that matter owners right after turning it on (owners can be set only once ownership is
on): their creator redeploys them, or adds owners with the
[owners call](../../docs/deploying-sites.md#ownership).
Removing the key lists every site again and turns ownership off, unless
`sites.ownership: owner` is set. It is meant for single sign-on: with `auth.mode: none` every
browser is a guest and is listed no site. Upgrade first (`install.sh` keeps your
configuration): servers older than the option refuse the key. Details:
[owners-only site listing](../../docs/deploying-sites.md#owners-only-site-listing).

## Deploying with rsync

Next to the API (`backyard deploy`, hub drag & drop), people and CI can push sites with plain
rsync over SSH — Quick's original workflow. `backyard deploy --method rsync` runs
`rsync -az --delete <dir>/ <rsync_target>/<site>/` and then asks the server to refresh the
site's metadata.

Deploys log in as the **`backyard` service account itself**, so every file is owned by the
server's user (no group or ACL juggling), and
[`rrsync`](https://download.samba.org/pub/rsync/rrsync.1) confines each key to
`/var/lib/backyard/sites`: no shell, no port forwarding, no access to the database or other
files. Two details keep that confinement tight:

* `rrsync -munge` stores symlinks in "munged" (inert) form, so a deploy can never read or write
  through a link that points out of `sites/` (to `backyard.db`, say). Symlinks inside sites
  deployed this way therefore don't resolve — deploy real files (the API method does).
  `-no-lock` lets people deploy different sites at the same time.
* `.ssh/authorized_keys` is owned by **root** (`0644`): the deploy account can use its key but
  can't add keys or change its forced command. Edit the file as root.

```sh
# Requires rsync >= 3.2.4 (ships /usr/bin/rrsync, a python3 script; some distributions keep it in
# /usr/share/doc/rsync/scripts/rrsync and install.sh copies it to /usr/local/bin).
sudo deploy/systemd/install.sh --enable-rsync      # or the manual steps below
```

Manually:

```sh
sudo usermod -s /bin/sh backyard        # sshd runs forced commands through the login shell
sudo usermod -p '*' backyard            # no password: key-based logins only
sudo install -d -o backyard -g backyard -m 0750 /var/lib/backyard/sites
sudo install -d -o root -g root -m 0755 /var/lib/backyard/.ssh
sudo install -o root -g root -m 0644 /dev/null /var/lib/backyard/.ssh/authorized_keys
```

Add one line per person or CI system (their public key):

```
restrict,command="/usr/bin/rrsync -munge -no-lock /var/lib/backyard/sites" ssh-ed25519 AAAAC3Nza... alice@acme.com
restrict,command="/usr/bin/rrsync -munge -no-lock /var/lib/backyard/sites" ssh-ed25519 AAAAC3Nza... ci@github-actions
```

Advertise the target to CLIs in `/etc/backyard/backyard.yaml` (quoted — the trailing `:` is YAML
syntax otherwise) and restart:

```yaml
deploy:
  methods: [api, rsync]
  rsync_target: "backyard@backyard.acme.com:"
```

rrsync maps the paths into the restricted folder, so `backyard@backyard.acme.com:/lunch-vote/`
is `/var/lib/backyard/sites/lunch-vote/`. Check with
`backyard deploy --method rsync --dry-run`. Hardening notes: keep `PasswordAuthentication no`
in sshd, consider `AllowUsers backyard <admins>`, and remove a person's line to revoke access.
The `refresh` call after an rsync needs a Backyard token (`backyard login`); without it the
site is still served immediately, the hub just shows the new title/size a little later.

## Backups

All state is in `/var/lib/backyard` plus `/etc/backyard`:

```sh
# Consistent SQLite snapshot while running (sqlite3 CLI), then the files.
sudo -u backyard sqlite3 /var/lib/backyard/backyard.db ".backup '/var/lib/backyard/backup.db'"
sudo tar -czf backyard-$(date +%F).tgz -C /var/lib/backyard --exclude=./backyard.db \
     --exclude=./backyard.db-wal --exclude=./backyard.db-shm . -C /etc backyard
sudo rm /var/lib/backyard/backup.db
```

(Without the `sqlite3` CLI: `systemctl stop backyard`, archive the directory, start it again.)
Restic, Borg or VM snapshots work the same way — just snapshot the SQLite file with `.backup`
first. To restore: stop the service, unpack into `/var/lib/backyard` (rename `backup.db` to
`backyard.db`), `chown -R backyard:backyard /var/lib/backyard`, and start it.

## Upgrade and uninstall

```sh
curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/deploy/systemd/install.sh | sudo sh   # latest
curl -fsSL .../install.sh | sudo sh -s -- --version 0.3.0                                               # specific
```

Uninstall (keeps the data unless you delete it):

```sh
sudo systemctl disable --now backyard
sudo rm /etc/systemd/system/backyard.service /usr/local/bin/backyard && sudo systemctl daemon-reload
# sudo rm -rf /etc/backyard /var/lib/backyard && sudo userdel backyard
```
