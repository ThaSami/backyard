#!/bin/sh
# Install (or upgrade) the Backyard server on a Linux VM as a systemd service:
# fs storage + SQLite in /var/lib/backyard, no cloud bucket needed.
#
#   curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/deploy/systemd/install.sh \
#     | sudo sh -s -- --domain backyard.acme.com
#
# Options:
#   --domain D        base domain (hub at https://D, sites at https://<site>.D); required on first install
#   --version V       release to install (default: latest)
#   --binary PATH     install this backyard binary instead of downloading one (air-gapped hosts)
#   --repo O/R        GitHub repository (default: thasami/backyard)
#   --enable-rsync    let the "backyard" account receive rsync deploys over SSH (rrsync)
#   --no-start        install but do not (re)start the service
#
# What it does: installs /usr/local/bin/backyard (checksum-verified), creates the
# "backyard" system user, writes /etc/backyard/backyard.yaml and
# /etc/backyard/backyard.env (fresh BACKYARD_SECRET and a deploy token, printed
# once; only its SHA-256 is stored) unless they exist, installs
# /etc/systemd/system/backyard.service, validates the configuration and starts
# the service. Re-running it upgrades the binary and unit and keeps your config.
set -eu

REPO="${BACKYARD_REPO:-thasami/backyard}"
VERSION="${BACKYARD_VERSION:-latest}"
GITHUB="${BACKYARD_GITHUB_URL:-https://github.com}"
GITHUB_API="${BACKYARD_GITHUB_API_URL:-https://api.github.com}"
RAW="${BACKYARD_RAW_URL:-https://raw.githubusercontent.com}"
DOMAIN=""
BINARY=""
ENABLE_RSYNC=""
NO_START=""

PREFIX="${BACKYARD_PREFIX:-}" # testing: install below this root, skip systemctl/useradd
BIN_PATH="$PREFIX/usr/local/bin/backyard"
ETC_DIR="$PREFIX/etc/backyard"
UNIT_PATH="$PREFIX/etc/systemd/system/backyard.service"
DATA_DIR="$PREFIX/var/lib/backyard"
SERVICE_USER="backyard"

log() { printf '>> %s\n' "$*" >&2; }
fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
	cat >&2 <<EOF
Install or upgrade the Backyard server as a systemd service.

usage: install.sh --domain backyard.acme.com [--version V] [--binary PATH]
                  [--repo owner/name] [--enable-rsync] [--no-start]
EOF
	exit "${1:-0}"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--domain) [ $# -ge 2 ] || fail "--domain needs a value"; DOMAIN="$2"; shift 2 ;;
	--domain=*) DOMAIN="${1#*=}"; shift ;;
	--version) [ $# -ge 2 ] || fail "--version needs a value"; VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--binary) [ $# -ge 2 ] || fail "--binary needs a value"; BINARY="$2"; shift 2 ;;
	--binary=*) BINARY="${1#*=}"; shift ;;
	--repo) [ $# -ge 2 ] || fail "--repo needs a value"; REPO="$2"; shift 2 ;;
	--repo=*) REPO="${1#*=}"; shift ;;
	--enable-rsync) ENABLE_RSYNC=1; shift ;;
	--no-start) NO_START=1; shift ;;
	-h | --help) usage 0 ;;
	*) printf 'unknown option: %s\n' "$1" >&2; usage 2 ;;
	esac
done

if [ -z "$PREFIX" ]; then
	[ "$(id -u)" = 0 ] || fail "run as root (sudo sh install.sh ...)"
	have systemctl || fail "systemd (systemctl) is required"
fi
[ "$(uname -s)" = Linux ] || fail "this installer supports Linux only"

DOMAIN="$(printf '%s' "$DOMAIN" | tr '[:upper:]' '[:lower:]')"
if [ -n "$DOMAIN" ]; then
	case "$DOMAIN" in
	*[!a-z0-9.-]* | .* | *. | *..*) fail "invalid --domain: $DOMAIN" ;;
	esac
fi
if [ ! -f "$ETC_DIR/backyard.yaml" ] && [ -z "$DOMAIN" ]; then
	fail "--domain is required on the first install (e.g. --domain backyard.acme.com)"
fi

# rsync deploys need rrsync (rsync >= 3.2.4 installs it; older packages ship it
# as documentation). Check before changing anything.
rrsync=""
rrsync_src=""
if [ -n "$ENABLE_RSYNC" ]; then
	have rsync || fail "--enable-rsync needs rsync (apt install rsync / dnf install rsync)"
	rrsync="$(command -v rrsync || true)"
	if [ -z "$rrsync" ]; then
		for f in /usr/share/doc/rsync/scripts/rrsync /usr/share/rsync/scripts/rrsync; do
			if [ -f "$f" ]; then
				rrsync_src="$f"
				rrsync=/usr/local/bin/rrsync
				break
			fi
		done
	fi
	[ -n "$rrsync" ] || fail "--enable-rsync needs rrsync (part of rsync >= 3.2.4)"
	# The forced command uses -munge (neutralise symlinks, so a deploy can never
	# write or read through a link out of the sites folder) and -no-lock
	# (concurrent deploys of different sites); both need rsync >= 3.2.4's rrsync.
	if ! grep -q -- "-no-lock" "${rrsync_src:-$rrsync}" 2>/dev/null; then
		fail "--enable-rsync needs the rrsync of rsync >= 3.2.4 (options -munge and -no-lock); ${rrsync_src:-$rrsync} is older"
	fi
	have python3 || fail "--enable-rsync needs python3 (rrsync is a python script)"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

download() {
	if have curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif have wget; then
		wget -q -O "$2" "$1"
	else
		fail "curl or wget is required"
	fi
}

sha256() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	else
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	fi
}

# sha256_str STRING prints the hex SHA-256 of STRING.
sha256_str() {
	if have sha256sum; then
		printf '%s' "$1" | sha256sum | awk '{print $1}'
	else
		printf '%s' "$1" | openssl dgst -sha256 | awk '{print $NF}'
	fi
}

rand_hex() {
	if have openssl; then
		openssl rand -hex "$1"
	else
		od -An -N "$1" -tx1 /dev/urandom | tr -d ' \n'
	fi
}

# --- 1. binary -----------------------------------------------------------------

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "unsupported architecture $(uname -m) (release builds exist for amd64 and arm64; use --binary)" ;;
esac

if [ -n "$BINARY" ]; then
	[ -f "$BINARY" ] || fail "--binary $BINARY does not exist"
	cp "$BINARY" "$tmp/backyard"
	# Sample files come from the matching tag, or main for "latest".
	if [ "$VERSION" = latest ]; then tag="main"; else tag="v${VERSION#v}"; fi
else
	if [ "$VERSION" = latest ]; then
		download "$GITHUB_API/repos/$REPO/releases/latest" "$tmp/latest.json" ||
			fail "could not query the latest release of $REPO (use --version)"
		tag="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)"
		[ -n "$tag" ] || fail "no published release found for $REPO"
	else
		tag="v${VERSION#v}"
	fi
	version="${tag#v}"
	archive="backyard_${version}_linux_${arch}.tar.gz"
	base="$GITHUB/$REPO/releases/download/$tag"
	log "downloading backyard $version (linux/$arch)"
	download "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
	download "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"
	want="$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1}' "$tmp/checksums.txt" | head -n 1)"
	[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
	got="$(sha256 "$tmp/$archive")"
	[ "$want" = "$got" ] || fail "checksum mismatch for $archive"
	tar -xzf "$tmp/$archive" -C "$tmp" backyard
fi
chmod 0755 "$tmp/backyard"
"$tmp/backyard" version >/dev/null 2>&1 || fail "the backyard binary does not run on this machine"

mkdir -p "$(dirname "$BIN_PATH")"
cp "$tmp/backyard" "$BIN_PATH.new"
chmod 0755 "$BIN_PATH.new"
mv -f "$BIN_PATH.new" "$BIN_PATH"
log "installed $BIN_PATH ($("$BIN_PATH" version 2>/dev/null || echo unknown))"

# --- 2. user -------------------------------------------------------------------

nologin="$(command -v nologin || echo /usr/sbin/nologin)"
if [ -z "$PREFIX" ]; then
	if ! id "$SERVICE_USER" >/dev/null 2>&1; then
		if have useradd; then
			useradd --system --user-group --home-dir /var/lib/backyard --no-create-home \
				--shell "$nologin" --comment "Backyard server" "$SERVICE_USER"
		elif have adduser; then # Alpine / BusyBox
			adduser -S -D -H -h /var/lib/backyard -s "$nologin" -g "Backyard server" "$SERVICE_USER"
		else
			fail "cannot create the $SERVICE_USER user (no useradd/adduser)"
		fi
		log "created system user $SERVICE_USER"
	fi
fi

# --- 3. configuration ------------------------------------------------------------

# fetch_file NAME DEST: deploy/systemd/NAME from next to this script (checkout)
# or from GitHub at the installed tag.
fetch_file() {
	here=""
	case "$0" in
	*install.sh) here="$(dirname "$0")" ;; # run from a checkout, not piped into sh
	esac
	if [ -n "$here" ] && [ -f "$here/$1" ]; then
		cp "$here/$1" "$2"
	else
		ref="$tag"
		download "$RAW/$REPO/$ref/deploy/systemd/$1" "$2" ||
			download "$RAW/$REPO/main/deploy/systemd/$1" "$2" ||
			fail "could not download deploy/systemd/$1"
	fi
}

mkdir -p "$ETC_DIR"
chmod 0755 "$ETC_DIR"
group_of_service() { if [ -z "$PREFIX" ]; then echo "$SERVICE_USER"; else id -gn; fi; }

new_yaml=""
if [ ! -f "$ETC_DIR/backyard.yaml" ]; then
	fetch_file backyard.yaml "$tmp/backyard.yaml"
	sed "s/backyard\.example\.com/$DOMAIN/g" "$tmp/backyard.yaml" >"$ETC_DIR/backyard.yaml"
	chmod 0644 "$ETC_DIR/backyard.yaml"
	new_yaml=1
	log "wrote $ETC_DIR/backyard.yaml for $DOMAIN"
else
	log "keeping existing $ETC_DIR/backyard.yaml"
fi

# The deploy token is shown once at the end; only its SHA-256 is stored.
deploy_token=""
if [ ! -f "$ETC_DIR/backyard.env" ]; then
	deploy_token="byd_$(rand_hex 24)"
	(
		umask 077
		cat >"$ETC_DIR/backyard.env" <<EOF
# Secrets for backyard.service (KEY=value, one per line; no quotes needed).
# Rotating BACKYARD_SECRET signs everyone out and revokes all API tokens.
BACKYARD_SECRET=$(rand_hex 32)
# SHA-256 of the deploy token (auth.tokens in backyard.yaml). A new one:
#   TOKEN=byd_\$(openssl rand -hex 24); printf %s "\$TOKEN" | sha256sum
BACKYARD_DEPLOY_TOKEN_SHA256=$(sha256_str "$deploy_token")
# OIDC_CLIENT_SECRET=
# ANTHROPIC_API_KEY=
EOF
	)
	chgrp "$(group_of_service)" "$ETC_DIR/backyard.env"
	chmod 0640 "$ETC_DIR/backyard.env"
	log "wrote $ETC_DIR/backyard.env with a fresh BACKYARD_SECRET and deploy token"
else
	log "keeping existing $ETC_DIR/backyard.env"
	# A freshly written backyard.yaml references the deploy token: add one to
	# an older backyard.env that lacks it.
	if [ -n "$new_yaml" ] && ! grep -q '^BACKYARD_DEPLOY_TOKEN_SHA256=' "$ETC_DIR/backyard.env"; then
		deploy_token="byd_$(rand_hex 24)"
		printf '\n# SHA-256 of the deploy token (auth.tokens in backyard.yaml).\nBACKYARD_DEPLOY_TOKEN_SHA256=%s\n' \
			"$(sha256_str "$deploy_token")" >>"$ETC_DIR/backyard.env"
		log "added a deploy token to $ETC_DIR/backyard.env"
	fi
fi

log "validating the configuration"
(
	set -a
	# shellcheck disable=SC1091
	. "$ETC_DIR/backyard.env"
	set +a
	"$BIN_PATH" config validate "$ETC_DIR/backyard.yaml"
) || fail "fix $ETC_DIR/backyard.yaml (see the errors above), then re-run this script"

# --- 4. rsync deploys (optional) -------------------------------------------------

if [ -n "$ENABLE_RSYNC" ]; then
	if [ -n "$rrsync_src" ] && [ -z "$PREFIX" ]; then
		install -m 0755 "$rrsync_src" "$rrsync"
	fi
	mkdir -p "$DATA_DIR/sites" "$DATA_DIR/.ssh"
	[ -f "$DATA_DIR/.ssh/authorized_keys" ] || : >"$DATA_DIR/.ssh/authorized_keys"
	chmod 0750 "$DATA_DIR"
	if [ -z "$PREFIX" ]; then
		chown -R "$SERVICE_USER:$SERVICE_USER" "$DATA_DIR"
		# The keys belong to root: deployers log in as the service account and
		# must not be able to add keys or change their forced command.
		chown -R root:root "$DATA_DIR/.ssh"
		# A shell is needed to run the forced command; '*' = no password (key-only).
		usermod -s /bin/sh "$SERVICE_USER"
		usermod -p '*' "$SERVICE_USER"
	fi
	chmod 0755 "$DATA_DIR/.ssh"
	chmod 0644 "$DATA_DIR/.ssh/authorized_keys"
	log "rsync deploys: add one line per deployer to $DATA_DIR/.ssh/authorized_keys (as root):"
	log "  restrict,command=\"$rrsync -munge -no-lock $DATA_DIR/sites\" ssh-ed25519 AAAA... alice@acme.com"
fi

# --- 5. service ------------------------------------------------------------------

mkdir -p "$(dirname "$UNIT_PATH")"
fetch_file backyard.service "$tmp/backyard.service"
cp "$tmp/backyard.service" "$UNIT_PATH"
chmod 0644 "$UNIT_PATH"
log "installed $UNIT_PATH"

if [ -z "$PREFIX" ]; then
	systemctl daemon-reload
	if [ -z "$NO_START" ]; then
		systemctl enable backyard.service >/dev/null 2>&1 || true
		systemctl restart backyard.service
		sleep 2
		if systemctl is-active --quiet backyard.service; then
			log "backyard is running: journalctl -u backyard -f"
		else
			systemctl --no-pager status backyard.service || true
			fail "backyard did not start; see journalctl -u backyard"
		fi
	fi
fi

listen="$(sed -n 's/^[[:space:]]*listen:[[:space:]]*"\{0,1\}\([^"#[:space:]]*\).*/\1/p' "$ETC_DIR/backyard.yaml" | head -n 1)"
cat >&2 <<EOF

Done. Next steps:
  1. DNS: point ${DOMAIN:-<domain>} and *.${DOMAIN:-<domain>} at this machine.
  2. TLS + reverse proxy to ${listen:-127.0.0.1:8080} (Caddy or nginx examples:
     https://github.com/$REPO/tree/main/deploy/systemd#reverse-proxy-and-tls).
  3. Single sign-on, more tokens, AI: edit $ETC_DIR/backyard.yaml, then
     systemctl restart backyard
EOF
if [ -n "$deploy_token" ]; then
	cat >&2 <<EOF

Your deploy token (shown once; only its SHA-256 is stored):

  $deploy_token

  backyard login --server https://${DOMAIN:-<domain>} --token $deploy_token
EOF
fi
