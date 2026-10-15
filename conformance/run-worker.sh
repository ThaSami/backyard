#!/bin/sh
# Run the conformance suite against the Cloudflare Workers edition under
# `wrangler dev --env local` (npm run dev:local in deploy/cloudflare: no
# Cloudflare login, Durable Objects and R2 simulated, temporary state):
# once in path mode as an anonymous guest and once in host mode with the
# DEPLOY_TOKEN, each against a fresh Worker with the Claude connector enabled
# (MCP_ENABLED, SPEC §8.4; the suite runs with BACKYARD_CONFORMANCE_MCP=1):
#
#   path  path mode (the default) as an anonymous guest, with AI aliases
#         backed by mock/openai.mjs through the "openai" provider
#   host  host mode (BASE_DOMAIN=localhost) with the DEPLOY_TOKEN, with the same mock
#         aliases but AI turned off (AI_ENABLED=false: its 501 unsupported answers, as the
#         Go server's ai.enabled: false)
#
#   conformance/run-worker.sh            # both passes
#   conformance/run-worker.sh path       # one pass (path | host)
#   conformance/run-worker.sh path -- --test-name-pattern=files
#
# Run `npm ci` in deploy/cloudflare first.
#
# Environment:
#   CONFORMANCE_WORKER_PORT   Worker port (default 18450)
#   CONFORMANCE_AI_PORT       port of the mock AI provider (default 18491; "off" disables AI)
#   CONFORMANCE_TIMEOUT       seconds to wait for the Worker (default 180)
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"
cf="$root/deploy/cloudflare"
port="${CONFORMANCE_WORKER_PORT:-18450}"
ai_port="${CONFORMANCE_AI_PORT:-18491}"
timeout="${CONFORMANCE_TIMEOUT:-180}"

modes=""
while [ $# -gt 0 ]; do
	case "$1" in
	host | path) modes="$modes $1" ;;
	--)
		shift
		break
		;;
	*)
		echo "usage: $0 [path|host]... [-- node --test args]" >&2
		exit 2
		;;
	esac
	shift
done
[ -n "$modes" ] || modes="path host"

log() { printf '>> %s\n' "$*" >&2; }
fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

command -v node >/dev/null 2>&1 || fail "node 22+ is required"
[ -f "$cf/package.json" ] || fail "no Cloudflare edition at $cf"
[ -d "$cf/node_modules/wrangler" ] || fail "run npm ci in $cf first"

work="$(mktemp -d 2>/dev/null || mktemp -d -t backyard-conformance)"
server_pid=""
mock_pid=""
# setsid (when available) gives wrangler and its workerd children their own
# process group, so that stopping the Worker stops all of them.
runner=""
command -v setsid >/dev/null 2>&1 && runner="setsid"

stop_group() {
	[ -n "$1" ] || return 0
	target="$1"
	[ -n "$runner" ] && target="-$1"
	kill -TERM "$target" 2>/dev/null || true
	i=0
	while kill -0 "$target" 2>/dev/null && [ "$i" -lt 50 ]; do
		sleep 0.2
		i=$((i + 1))
	done
	kill -KILL "$target" 2>/dev/null || true
	wait "$1" 2>/dev/null || true
}

# Invoked by the EXIT trap (shellcheck cannot tell when the script ends with exit).
# shellcheck disable=SC2317,SC2329
cleanup() {
	status=$?
	stop_group "$server_pid"
	if [ -n "$mock_pid" ]; then
		kill "$mock_pid" 2>/dev/null || true
		wait "$mock_pid" 2>/dev/null || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

rand_hex() {
	node -e 'process.stdout.write(require("crypto").randomBytes(Number(process.argv[1])).toString("hex"))' "$1"
}

# wait_http URL: polls until URL answers 200.
wait_http() {
	tries=0
	while [ "$tries" -lt "$((timeout * 2))" ]; do
		if node -e 'fetch(process.argv[1], { signal: AbortSignal.timeout(2000) }).then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))' "$1"; then
			return 0
		fi
		if [ -n "$server_pid" ] && ! kill -0 "$server_pid" 2>/dev/null; then
			return 1
		fi
		sleep 0.5
		tries=$((tries + 1))
	done
	return 1
}

ai_mode=""
if [ "$ai_port" != "off" ]; then
	log "starting the mock AI provider on 127.0.0.1:$ai_port"
	node "$here/mock/openai.mjs" "$ai_port" >"$work/mock.log" 2>&1 &
	mock_pid=$!
	wait_http "http://127.0.0.1:$ai_port/v1/models" || {
		cat "$work/mock.log" >&2
		fail "the mock AI provider did not start"
	}
	ai_mode="mock"
fi
ai_models='{"smart":{"provider":"openai","model":"mock-chat","kind":"chat","description":"Mock chat model"},"image":{"provider":"openai","model":"mock-image","kind":"image"},"embed":{"provider":"openai","model":"mock-embed","kind":"embedding"}}'

status=0
for mode in $modes; do
	data="$work/$mode"
	mkdir -p "$data"
	token="byd_conformance_$(rand_hex 16)"
	secret="$(rand_hex 32)"
	log "starting wrangler dev --env local ($mode mode) on 127.0.0.1:$port"
	(
		cd "$cf"
		WRANGLER_SEND_METRICS=false
		export WRANGLER_SEND_METRICS
		# The wrangler arguments (this subshell's own positional parameters).
		set -- --port "$port" --ip 127.0.0.1 --persist-to "$data/state" --show-interactive-dev-session=false \
			--var AUTH_MODE:none --var ALLOW_ANONYMOUS_DEPLOY:true --var MCP_ENABLED:true \
			--var "BACKYARD_SECRET:$secret" --var "DEPLOY_TOKEN:$token"
		if [ "$mode" = "host" ]; then
			set -- "$@" --var URL_MODE:host --var BASE_DOMAIN:localhost --var AI_ENABLED:false
		else
			set -- "$@" --var URL_MODE:path
		fi
		# Both passes get the mock aliases: in host mode they are usable too, so
		# its 501 answers can only come from AI_ENABLED=false.
		if [ -n "$ai_mode" ]; then
			set -- "$@" --var "AI_MODELS:$ai_models" --var "OPENAI_BASE_URL:http://127.0.0.1:$ai_port/v1" --var OPENAI_API_KEY:mock-key
		fi
		# $runner is empty or "setsid" (word splitting intended).
		# shellcheck disable=SC2086
		exec $runner npm run --silent dev:local -- "$@"
	) >"$data/worker.log" 2>&1 </dev/null &
	server_pid=$!
	if ! wait_http "http://127.0.0.1:$port/__backyard/healthz"; then
		tail -n 100 "$data/worker.log" >&2 || true
		fail "the Worker did not become healthy within ${timeout}s"
	fi

	suite_token=""
	suite_ai="$ai_mode"
	who="an anonymous guest"
	url="http://127.0.0.1:$port"
	if [ "$mode" = "host" ]; then
		suite_token="$token"
		# "off" (with the mock aliases): the suite fails unless AI_ENABLED=false turns AI off.
		suite_ai="${ai_mode:+off}"
		who="the DEPLOY_TOKEN, with AI_ENABLED=false"
		url="http://localhost:$port"
	fi
	log "running the suite ($mode mode, as $who)"
	set +e
	(
		cd "$here"
		BACKYARD_URL="$url" BACKYARD_TOKEN="$suite_token" BACKYARD_CONFORMANCE_AI="$suite_ai" \
			BACKYARD_CONFORMANCE_SECRET="$secret" BACKYARD_CONFORMANCE_MCP=1 \
			node --test --test-reporter=spec "$@" "tests/*.test.mjs"
	)
	rc=$?
	set -e
	if [ "$rc" -ne 0 ]; then
		status=$rc
		printf '\n---- Worker log (%s mode, last 150 lines) ----\n' "$mode" >&2
		tail -n 150 "$data/worker.log" >&2 || true
	fi
	stop_group "$server_pid"
	server_pid=""
done
exit "$status"
