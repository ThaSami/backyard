# TLS certificates (BACKYARD_TLS=files)

Put a certificate valid for both `<domain>` and `*.<domain>` here:

* `fullchain.pem` — the certificate followed by its intermediates
* `privkey.pem` — the private key (readable by the Caddy container)

Then set `BACKYARD_TLS=files` in `.env` and run `docker compose up -d`. Caddy does not renew
these certificates; replace the files and run `docker compose exec caddy caddy reload
--config /etc/caddy/Caddyfile` when you rotate them. Files in this folder are git-ignored.
