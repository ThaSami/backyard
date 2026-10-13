#!/bin/sh
# Create .env for deploy/docker-compose with fresh secrets and a deploy token.
#
#   ./init-env.sh [domain]          domain defaults to "localhost"
#   ./init-env.sh --force [domain]  overwrite an existing .env
#
# Writes .env (mode 0600) from .env.example with BACKYARD_DOMAIN,
# BACKYARD_SECRET, POSTGRES_PASSWORD and BACKYARD_DEPLOY_TOKEN_SHA256 filled
# in, and prints the deploy token once (only its SHA-256 is stored).
set -eu

cd "$(dirname "$0")"

force=""
domain=""
for arg in "$@"; do
	case "$arg" in
	-f | --force) force=1 ;;
	-h | --help)
		sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	-*)
		echo "unknown option: $arg" >&2
		exit 2
		;;
	*) domain="$arg" ;;
	esac
done
domain="$(printf '%s' "${domain:-localhost}" | tr '[:upper:]' '[:lower:]')"
case "$domain" in
*[!a-z0-9.-]* | "" | .* | *. | *..*)
	echo "invalid domain: $domain" >&2
	exit 2
	;;
esac

if [ -e .env ] && [ -z "$force" ]; then
	echo ".env already exists (use --force to overwrite it)" >&2
	exit 1
fi
[ -f .env.example ] || {
	echo ".env.example is missing" >&2
	exit 1
}

# rand_hex N prints N random bytes as hex.
rand_hex() {
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -hex "$1"
	else
		od -An -N "$1" -tx1 /dev/urandom | tr -d ' \n'
	fi
}

sha256_hex() {
	if command -v sha256sum >/dev/null 2>&1; then
		printf '%s' "$1" | sha256sum | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		printf '%s' "$1" | shasum -a 256 | awk '{print $1}'
	else
		printf '%s' "$1" | openssl dgst -sha256 | awk '{print $NF}'
	fi
}

secret="$(rand_hex 32)"
db_password="$(rand_hex 16)"
token="byd_$(rand_hex 24)"
token_sha="$(sha256_hex "$token")"

umask 077
tmp=".env.tmp.$$"
trap 'rm -f "$tmp"' EXIT
# All substituted values are [a-z0-9.-] or hex, so they are safe in sed.
sed \
	-e "s|^BACKYARD_DOMAIN=.*|BACKYARD_DOMAIN=$domain|" \
	-e "s|^BACKYARD_SECRET=.*|BACKYARD_SECRET=$secret|" \
	-e "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$db_password|" \
	-e "s|^BACKYARD_DEPLOY_TOKEN_SHA256=.*|BACKYARD_DEPLOY_TOKEN_SHA256=$token_sha|" \
	.env.example >"$tmp"
mv -f "$tmp" .env
chmod 600 .env

url="https://$domain"
cat <<EOF
Wrote $(pwd)/.env for https://$domain

Your deploy token (shown once; only its SHA-256 is in .env):

  $token

Next:
  docker compose up -d
  backyard login --server $url --token $token
  backyard init my-site && cd my-site && backyard deploy
EOF
if [ "$domain" = "localhost" ]; then
	cat <<EOF

Caddy uses its own certificate authority for localhost. Trust it once with
  docker compose cp caddy:/data/caddy/pki/authorities/local/root.crt ./caddy-root.crt
and import caddy-root.crt into your OS / browser trust store (or accept the warning).
EOF
fi
