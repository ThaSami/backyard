#!/bin/sh
# Start a throwaway local Backyard server, run a command against it, stop it.
# Used by the conformance suite (scripts/conformance.sh external), the browser
# end-to-end test (make e2e) and CI, for both editions.
#
#   scripts/with-server.sh go     -- node --test web/test/e2e.test.mjs
#   scripts/with-server.sh worker -- npm --prefix conformance test
#   BACKYARD_URL=https://backyard.acme.dev BACKYARD_TOKEN=... scripts/with-server.sh external -- CMD
#
# Targets:
#   go       `backyard serve --dev` (auth none, anonymous deploys, sqlite + fs
#            storage in a temporary directory)
#   worker   the Cloudflare Workers edition under `wrangler dev --env local`
#            (deploy/cloudflare; no Cloudflare login needed: Workers AI is
#            off, R2 and Durable Objects are simulated, state is temporary)
#   external an already running server at BACKYARD_URL
#
# The command gets BACKYARD_URL and BACKYARD_E2E_URL (the hub URL),
# BACKYARD_TOKEN and BACKYARD_E2E_TOKEN (a deploy token, when there is one)
# and BACKYARD_TARGET. The exit status is the command's; the server log is
# printed when it fails.
#
# Environment:
#   PORT                  port for the local server (default 18080 go, 18787 worker)
#   BACKYARD_BIN          prebuilt backyard binary (default: go build into a temp dir)
#   BACKYARD_CONFIG       config file for the Go server (still run with --dev)
#   BACKYARD_URL_MODE     host (default) or path, for the Go server without BACKYARD_CONFIG
#                         (the Worker always uses path mode)
#   BACKYARD_SERVER_TIMEOUT  seconds to wait for /__backyard/healthz (default 120)
set -eu

usage() {
	echo "usage: $0 go|worker|external [--] command [arg...]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
target="$1"
shift
[ "${1:-}" = "--" ] && shift
[ $# -ge 1 ] || usage

root="$(cd "$(dirname "$0")/.." && pwd)"
timeout="${BACKYARD_SERVER_TIMEOUT:-120}"

log() { printf '>> %s\n' "$*" >&2; }
fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

command -v node >/dev/null 2>&1 || fail "node (22+) is required"

work="$(mktemp -d 2>/dev/null || mktemp -d -t backyard-server)"
server_pid=""
server_log="$work/server.log"

stop_server() {
	[ -n "$server_pid" ] || return 0
	# With setsid the server leads its own process group: signal all of it
	# (wrangler/npx spawn workerd children). Note: dash's kill takes no "--".
	group="$server_pid"
	[ -n "$runner" ] && group="-$server_pid"
	kill -TERM "$group" 2>/dev/null || true
	(sleep 10 && kill -KILL "$group") >/dev/null 2>&1 &
	watchdog=$!
	wait "$server_pid" 2>/dev/null || true
	# The leader is gone; give the rest of its group a moment, then force it.
	i=0
	while [ -n "$runner" ] && kill -0 "$group" 2>/dev/null && [ "$i" -lt 10 ]; do
		sleep 0.5
		i=$((i + 1))
	done
	kill -KILL "$group" 2>/dev/null || true
	kill "$watchdog" 2>/dev/null || true
	wait "$watchdog" 2>/dev/null || true
	server_pid=""
}

cleanup() {
	status=$?
	stop_server
	if [ "$status" -ne 0 ] && [ -s "$server_log" ]; then
		printf '\n---- server log (last 200 lines) ----\n' >&2
		tail -n 200 "$server_log" >&2 || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# rand_hex N prints N random bytes as hex.
rand_hex() {
	node -e 'process.stdout.write(require("crypto").randomBytes(Number(process.argv[1])).toString("hex"))' "$1"
}

# wait_healthy URL polls URL/__backyard/healthz (sh has no local variables:
# keep the names distinct from the callers').
wait_healthy() {
	health_url="$1/__backyard/healthz"
	tries=0
	while [ "$tries" -lt "$timeout" ]; do
		if node -e 'fetch(process.argv[1], { signal: AbortSignal.timeout(5000) }).then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))' "$health_url"; then
			return 0
		fi
		if [ -n "$server_pid" ] && ! kill -0 "$server_pid" 2>/dev/null; then
			fail "the server exited before becoming healthy"
		fi
		tries=$((tries + 1))
		sleep 1
	done
	fail "the server did not become healthy at $health_url within ${timeout}s"
}

# setsid (when available) gives the server its own process group for cleanup.
runner=""
command -v setsid >/dev/null 2>&1 && runner="setsid"

token="${BACKYARD_TOKEN:-}"
case "$target" in
go)
	port="${PORT:-18080}"
	bin="${BACKYARD_BIN:-}"
	if [ -z "$bin" ]; then
		command -v go >/dev/null 2>&1 || fail "go is required to build backyard (or set BACKYARD_BIN)"
		log "building backyard"
		(cd "$root" && CGO_ENABLED=0 go build -o "$work/backyard" ./cmd/backyard)
		bin="$work/backyard"
	fi
	case "$bin" in /*) ;; *) bin="$(pwd)/$bin" ;; esac
	[ -x "$bin" ] || fail "BACKYARD_BIN $bin is not an executable"
	config_yaml=""
	config_arg=""
	if [ -n "${BACKYARD_CONFIG:-}" ]; then
		config_arg="$(cd "$(dirname "$BACKYARD_CONFIG")" && pwd)/$(basename "$BACKYARD_CONFIG")"
	elif [ "${BACKYARD_URL_MODE:-host}" = "path" ]; then
		config_yaml="server:
  url_mode: path"
	fi
	log "starting backyard serve --dev on :$port"
	mkdir -p "$work/data"
	# The server reads these from the environment; keep the caller's out.
	(
		cd "$work/data"
		unset BACKYARD_CONFIG BACKYARD_CONFIG_YAML BACKYARD_SECRET BACKYARD_BASE_DOMAIN BACKYARD_PUBLIC_URL DATABASE_URL
		if [ -n "$config_yaml" ]; then
			BACKYARD_CONFIG_YAML="$config_yaml"
			export BACKYARD_CONFIG_YAML
		fi
		# $runner is empty or "setsid" (word splitting intended).
		if [ -n "$config_arg" ]; then
			# shellcheck disable=SC2086
			exec $runner env PORT="$port" "$bin" serve --dev --config "$config_arg"
		fi
		# shellcheck disable=SC2086
		exec $runner env PORT="$port" "$bin" serve --dev
	) >"$server_log" 2>&1 &
	server_pid=$!
	url="http://localhost:$port"
	;;
worker)
	port="${PORT:-18787}"
	cf="$root/deploy/cloudflare"
	[ -f "$cf/package.json" ] || fail "no Cloudflare edition at $cf"
	[ -d "$cf/node_modules/wrangler" ] || fail "run npm ci in $cf first"
	secret="$(rand_hex 32)"
	[ -n "$token" ] || token="byd_$(rand_hex 24)"
	log "starting wrangler dev --env local on :$port"
	(
		cd "$cf"
		# shellcheck disable=SC2086
		exec $runner npx --no-install wrangler dev --env local --port "$port" --ip 127.0.0.1 \
			--persist-to "$work/wrangler-state" --show-interactive-dev-session=false \
			--var "AUTH_MODE:none" --var "ALLOW_ANONYMOUS_DEPLOY:true" \
			--var "BACKYARD_SECRET:$secret" --var "DEPLOY_TOKEN:$token"
	) >"$server_log" 2>&1 </dev/null &
	server_pid=$!
	url="http://127.0.0.1:$port"
	;;
external)
	url="${BACKYARD_URL:-}"
	[ -n "$url" ] || fail "set BACKYARD_URL for the external target"
	;;
*)
	usage
	;;
esac

url="${url%/}"
log "waiting for $url"
wait_healthy "$url"

BACKYARD_URL="$url"
BACKYARD_E2E_URL="$url"
BACKYARD_TARGET="$target"
export BACKYARD_URL BACKYARD_E2E_URL BACKYARD_TARGET
if [ -n "$token" ]; then
	BACKYARD_E2E_TOKEN="${BACKYARD_E2E_TOKEN:-$token}"
	BACKYARD_TOKEN="$token"
	export BACKYARD_TOKEN BACKYARD_E2E_TOKEN
fi

log "running: $* (against $url)"
# Last command: with set -e its exit status becomes the script's.
"$@"
