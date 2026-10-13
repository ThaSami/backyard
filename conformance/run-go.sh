#!/bin/sh
# Run the conformance suite against the Go server (`backyard serve --dev`),
# each pass against a fresh server with a temporary data directory:
#
#   host  host mode (sites at <site>.localhost) as an anonymous guest, with AI
#         (aliases backed by mock/openai.mjs) and, given a DSN, a warehouse
#   path  path mode (sites at /s/<site>/) with a static bearer token, without
#         AI and warehouse (their 501 unsupported answers)
#
#   conformance/run-go.sh               # both passes
#   conformance/run-go.sh host          # one pass (host | path)
#   conformance/run-go.sh path -- --test-name-pattern=files   # extra node --test args
#
# Environment:
#   BACKYARD_BIN                prebuilt backyard binary (default: go build into a temp dir)
#   CONFORMANCE_PORT            server port (default 18400)
#   CONFORMANCE_AI_PORT         port of the mock AI provider (default 18490; "off" disables AI)
#   CONFORMANCE_WAREHOUSE_DSN   PostgreSQL DSN for a warehouse source (default
#                               $BACKYARD_TEST_POSTGRES_DSN; empty: no warehouse → 501 checks)
#   CONFORMANCE_TIMEOUT         seconds to wait for the server (default 60)
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"
port="${CONFORMANCE_PORT:-18400}"
ai_port="${CONFORMANCE_AI_PORT:-18490}"
timeout="${CONFORMANCE_TIMEOUT:-60}"
warehouse_dsn="${CONFORMANCE_WAREHOUSE_DSN-${BACKYARD_TEST_POSTGRES_DSN:-}}"

modes=""
while [ $# -gt 0 ]; do
	case "$1" in
	host | path) modes="$modes $1" ;;
	--)
		shift
		break
		;;
	*)
		echo "usage: $0 [host|path]... [-- node --test args]" >&2
		exit 2
		;;
	esac
	shift
done
[ -n "$modes" ] || modes="host path"

log() { printf '>> %s\n' "$*" >&2; }
fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

command -v node >/dev/null 2>&1 || fail "node 22+ is required"

work="$(mktemp -d 2>/dev/null || mktemp -d -t backyard-conformance)"
server_pid=""
mock_pid=""

stop_pid() {
	[ -n "$1" ] || return 0
	kill "$1" 2>/dev/null || true
	i=0
	while kill -0 "$1" 2>/dev/null && [ "$i" -lt 50 ]; do
		sleep 0.2
		i=$((i + 1))
	done
	kill -9 "$1" 2>/dev/null || true
	wait "$1" 2>/dev/null || true
}

# Invoked by the EXIT trap (shellcheck cannot tell when the script ends with exit).
# shellcheck disable=SC2317
cleanup() {
	status=$?
	stop_pid "$server_pid"
	stop_pid "$mock_pid"
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

rand_hex() {
	node -e 'process.stdout.write(require("crypto").randomBytes(Number(process.argv[1])).toString("hex"))' "$1"
}

sha256_hex() {
	node -e 'process.stdout.write(require("crypto").createHash("sha256").update(process.argv[1]).digest("hex"))' "$1"
}

# wait_http URL: polls until URL answers 200.
wait_http() {
	tries=0
	while [ "$tries" -lt "$((timeout * 4))" ]; do
		if node -e 'fetch(process.argv[1], { signal: AbortSignal.timeout(2000) }).then(r => process.exit(r.ok ? 0 : 1), () => process.exit(1))' "$1"; then
			return 0
		fi
		if [ -n "$server_pid" ] && ! kill -0 "$server_pid" 2>/dev/null; then
			return 1
		fi
		sleep 0.25
		tries=$((tries + 1))
	done
	return 1
}

bin="${BACKYARD_BIN:-}"
if [ -z "$bin" ]; then
	command -v go >/dev/null 2>&1 || fail "go is required to build backyard (or set BACKYARD_BIN)"
	log "building backyard"
	(cd "$root" && CGO_ENABLED=0 go build -o "$work/backyard" ./cmd/backyard)
	bin="$work/backyard"
fi
case "$bin" in /*) ;; *) bin="$(pwd)/$bin" ;; esac
[ -x "$bin" ] || fail "BACKYARD_BIN $bin is not executable"

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

# write_config MODE TOKEN_SHA256 DATA_DIR FEATURES: prints the server
# configuration; FEATURES "full" adds AI and the warehouse.
write_config() {
	cat <<EOF
server:
  listen: "127.0.0.1:$port"
  url_mode: $1
  secret: "$(rand_hex 32)"
storage:
  type: fs
  fs:
    root: "$3/blobs"
database:
  type: sqlite
  sqlite:
    path: "$3/backyard.db"
auth:
  mode: none
  tokens:
    - name: conformance
      sha256: "$2"
deploy:
  allow_anonymous: true
telemetry:
  log_format: text
EOF
	[ "$4" = "full" ] || return 0
	if [ -n "$ai_mode" ]; then
		cat <<EOF
ai:
  default_model: smart
  default_image_model: image
  default_embedding_model: embed
  providers:
    mock:
      type: openai
      api_key: mock-key
      base_url: "http://127.0.0.1:$ai_port/v1"
  models:
    smart:
      provider: mock
      model: mock-chat
      description: Mock chat model
    image:
      provider: mock
      model: mock-image
      kind: image
    embed:
      provider: mock
      model: mock-embed
      kind: embedding
EOF
	fi
	if [ -n "$warehouse_dsn" ]; then
		cat <<EOF
warehouse:
  allow_raw_sql: true
  sources:
    default:
      type: postgres
      dsn: "$warehouse_dsn"
      description: Conformance PostgreSQL
  queries:
    conformance_echo:
      source: default
      sql: "SELECT CAST(@msg AS text) AS msg"
      params: [msg]
      description: Echoes msg
EOF
	fi
}

status=0
for mode in $modes; do
	data="$work/$mode"
	mkdir -p "$data"
	token="byd_conformance_$(rand_hex 16)"
	features="full"
	[ "$mode" = "path" ] && features="none"
	write_config "$mode" "$(sha256_hex "$token")" "$data" "$features" >"$data/backyard.yaml"
	log "starting backyard serve --dev ($mode mode) on 127.0.0.1:$port"
	(
		cd "$data"
		unset BACKYARD_CONFIG BACKYARD_CONFIG_YAML BACKYARD_SECRET BACKYARD_BASE_DOMAIN BACKYARD_PUBLIC_URL DATABASE_URL PORT
		exec "$bin" serve --dev --config "$data/backyard.yaml"
	) >"$data/server.log" 2>&1 &
	server_pid=$!
	if ! wait_http "http://127.0.0.1:$port/__backyard/healthz"; then
		tail -n 100 "$data/server.log" >&2 || true
		fail "the server did not become healthy within ${timeout}s"
	fi

	suite_token=""
	suite_ai="$ai_mode"
	who="an anonymous guest"
	if [ "$mode" = "path" ]; then
		suite_token="$token"
		suite_ai=""
		who="the static token, without AI and warehouse"
	fi
	log "running the suite ($mode mode, as $who)"
	set +e
	(
		cd "$here"
		BACKYARD_URL="http://localhost:$port" BACKYARD_TOKEN="$suite_token" BACKYARD_CONFORMANCE_AI="$suite_ai" \
			node --test --test-reporter=spec "$@" "tests/*.test.mjs"
	)
	rc=$?
	set -e
	if [ "$rc" -ne 0 ]; then
		status=$rc
		printf '\n---- server log (%s mode, last 150 lines) ----\n' "$mode" >&2
		tail -n 150 "$data/server.log" >&2 || true
	fi
	stop_pid "$server_pid"
	server_pid=""
done
exit "$status"
