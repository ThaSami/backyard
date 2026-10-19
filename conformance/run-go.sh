#!/bin/sh
# Run the conformance suite against the Go server, each pass against a fresh
# server with a temporary data directory and the Claude connector enabled
# (mcp.enabled, SPEC §8.4; the suite runs with BACKYARD_CONFORMANCE_MCP=1):
#
#   host  `backyard serve --dev`, host mode (sites at <site>.localhost) as an
#         anonymous guest, with AI (aliases backed by mock/openai.mjs) and,
#         given a DSN, a warehouse
#   path  `backyard serve --dev`, path mode (sites at /s/<site>/) with a static
#         bearer token, without AI and warehouse (their 501 unsupported answers)
#   oidc  `backyard serve`, auth.mode oidc against the mock OpenID Connect
#         provider (mock/idp.mjs), path mode on 127.0.0.1; runs only
#         tests/mcp.test.mjs, signed in as the IdP's alice and bob
#   listing `backyard serve --dev`, path mode with sites.listing: owners (so
#         sites.ownership defaults to owner) and an admin in auth.admins; runs
#         only tests/listing.test.mjs, with API tokens the suite signs for its
#         people and the static token, an admin here
#
#   conformance/run-go.sh               # all four passes
#   conformance/run-go.sh host          # one pass (host | path | oidc | listing)
#   conformance/run-go.sh path -- --test-name-pattern=files   # extra node --test args
#
# Environment:
#   BACKYARD_BIN                prebuilt backyard binary (default: go build into a temp dir)
#   CONFORMANCE_PORT            server port (default 18400)
#   CONFORMANCE_AI_PORT         port of the mock AI provider (default 18490; "off" disables AI)
#   CONFORMANCE_IDP_PORT        port of the mock OpenID Connect provider (default 18480)
#   CONFORMANCE_WAREHOUSE_DSN   PostgreSQL DSN for a warehouse source (default
#                               $BACKYARD_TEST_POSTGRES_DSN; empty: no warehouse → 501 checks)
#   CONFORMANCE_TIMEOUT         seconds to wait for the server (default 60)
set -eu

here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/.." && pwd)"
port="${CONFORMANCE_PORT:-18400}"
ai_port="${CONFORMANCE_AI_PORT:-18490}"
idp_port="${CONFORMANCE_IDP_PORT:-18480}"
timeout="${CONFORMANCE_TIMEOUT:-60}"
warehouse_dsn="${CONFORMANCE_WAREHOUSE_DSN-${BACKYARD_TEST_POSTGRES_DSN:-}}"

modes=""
while [ $# -gt 0 ]; do
	case "$1" in
	host | path | oidc | listing) modes="$modes $1" ;;
	--)
		shift
		break
		;;
	*)
		echo "usage: $0 [host|path|oidc|listing]... [-- node --test args]" >&2
		exit 2
		;;
	esac
	shift
done
[ -n "$modes" ] || modes="host path oidc listing"
# The admin of the listing pass (auth.admins); the suite signs an API token for it.
listing_admin="listing-admin@conformance.test"

log() { printf '>> %s\n' "$*" >&2; }
fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

command -v node >/dev/null 2>&1 || fail "node 22+ is required"

work="$(mktemp -d 2>/dev/null || mktemp -d -t backyard-conformance)"
server_pid=""
mock_pid=""
idp_pid=""

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
# shellcheck disable=SC2317,SC2329
cleanup() {
	status=$?
	stop_pid "$server_pid"
	stop_pid "$mock_pid"
	stop_pid "$idp_pid"
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

# write_config URL_MODE TOKEN_SHA256 DATA_DIR FEATURES SECRET [LISTING]: prints
# the server configuration; FEATURES "full" adds AI and the warehouse; LISTING
# "owners" adds sites.listing: owners, the listing pass's admin and admin
# rights for the static token.
write_config() {
	admins="[]"
	token_admin="false"
	if [ "${6:-}" = "owners" ]; then
		admins="[\"$listing_admin\"]"
		token_admin="true"
	fi
	cat <<EOF
server:
  listen: "127.0.0.1:$port"
  url_mode: $1
  secret: "$5"
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
  admins: $admins
  tokens:
    - name: conformance
      sha256: "$2"
      admin: $token_admin
deploy:
  allow_anonymous: true
mcp:
  enabled: true
ratelimit:
  # Every client of the suite comes from 127.0.0.1 and guests are limited per
  # IP, like the connector's OAuth endpoints: one bucket for all the files
  # running in parallel. The suite retries HTTP 429s but cannot retry a
  # websocket handshake.
  api_per_minute: 6000
  # Guests' deploys share that address's bucket too (the static token's are
  # one user's), and the suite creates more deploys than the default 60 an
  # hour (tests/preview.test.mjs alone about 40).
  deploys_per_hour: 1000
telemetry:
  log_format: text
EOF
	if [ "${6:-}" = "owners" ]; then
		cat <<EOF
sites:
  # No sites.ownership: owner is its default with this listing.
  listing: owners
EOF
	fi
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

# write_oidc_config DATA_DIR SECRET IDP_SECRET: prints the configuration of the
# oidc pass (no --dev: path mode on 127.0.0.1, the mock IdP's issuer).
write_oidc_config() {
	cat <<EOF
server:
  listen: "127.0.0.1:$port"
  base_domain: 127.0.0.1
  public_url: "http://127.0.0.1:$port"
  url_mode: path
  secret: "$2"
storage:
  type: fs
  fs:
    root: "$1/blobs"
database:
  type: sqlite
  sqlite:
    path: "$1/backyard.db"
auth:
  mode: oidc
  oidc:
    issuer: "http://localhost:$idp_port"
    client_id: conformance
    client_secret: "$3"
    scopes: [openid, profile, email, groups]
mcp:
  enabled: true
telemetry:
  log_format: text
EOF
}

# start_idp CLIENT_SECRET: starts mock/idp.mjs for the oidc pass. Its issuer is
# http://localhost:<port>, another cookie host than the server's 127.0.0.1.
start_idp() {
	log "starting the mock OpenID Connect provider on 127.0.0.1:$idp_port"
	node "$here/mock/idp.mjs" "$idp_port" conformance "$1" "http://127.0.0.1:$port/__backyard/auth/callback" >"$work/idp.log" 2>&1 &
	idp_pid=$!
	wait_http "http://127.0.0.1:$idp_port/.well-known/openid-configuration" || {
		cat "$work/idp.log" >&2
		fail "the mock OpenID Connect provider did not start"
	}
}

status=0
for mode in $modes; do
	data="$work/$mode"
	mkdir -p "$data"
	token="byd_conformance_$(rand_hex 16)"
	secret="$(rand_hex 32)"
	dev="--dev"
	if [ "$mode" = "oidc" ]; then
		idp_secret="$(rand_hex 16)"
		stop_pid "$idp_pid"
		start_idp "$idp_secret"
		write_oidc_config "$data" "$secret" "$idp_secret" >"$data/backyard.yaml"
		dev=""
	else
		url_mode="$mode"
		features="full"
		listing=""
		case "$mode" in
		path) features="none" ;;
		listing)
			url_mode="path"
			features="none"
			listing="owners"
			;;
		esac
		write_config "$url_mode" "$(sha256_hex "$token")" "$data" "$features" "$secret" "$listing" >"$data/backyard.yaml"
	fi
	log "starting backyard serve $dev ($mode mode) on 127.0.0.1:$port"
	(
		cd "$data"
		unset BACKYARD_CONFIG BACKYARD_CONFIG_YAML BACKYARD_SECRET BACKYARD_BASE_DOMAIN BACKYARD_PUBLIC_URL DATABASE_URL PORT
		# $dev is empty or "--dev" (word splitting intended).
		# shellcheck disable=SC2086
		exec "$bin" serve $dev --config "$data/backyard.yaml"
	) >"$data/server.log" 2>&1 &
	server_pid=$!
	if ! wait_http "http://127.0.0.1:$port/__backyard/healthz"; then
		tail -n 100 "$data/server.log" >&2 || true
		fail "the server did not become healthy within ${timeout}s"
	fi

	suite_token=""
	suite_ai="$ai_mode"
	suite_url="http://localhost:$port"
	suite_idp=""
	suite_listing="all"
	suite_admin=""
	tests="tests/*.test.mjs"
	who="an anonymous guest"
	if [ "$mode" = "path" ]; then
		suite_token="$token"
		suite_ai=""
		who="the static token, without AI and warehouse"
	elif [ "$mode" = "listing" ]; then
		suite_token="$token"
		suite_ai=""
		suite_listing="owners"
		suite_admin="$listing_admin"
		tests="tests/listing.test.mjs"
		who="API tokens of the suite's people and the static token (an admin), only the site listing"
	elif [ "$mode" = "oidc" ]; then
		suite_ai=""
		suite_url="http://127.0.0.1:$port"
		suite_idp="http://localhost:$idp_port"
		tests="tests/mcp.test.mjs"
		who="the mock IdP's alice and bob, only the Claude connector"
	fi
	log "running the suite ($mode mode, as $who)"
	set +e
	(
		cd "$here"
		BACKYARD_URL="$suite_url" BACKYARD_TOKEN="$suite_token" BACKYARD_CONFORMANCE_AI="$suite_ai" \
			BACKYARD_CONFORMANCE_SECRET="$secret" BACKYARD_CONFORMANCE_MCP=1 \
			BACKYARD_CONFORMANCE_IDP="$suite_idp" BACKYARD_CONFORMANCE_IDP_USERS=alice,bob \
			BACKYARD_CONFORMANCE_LISTING="$suite_listing" BACKYARD_CONFORMANCE_ADMIN="$suite_admin" \
			node --test --test-reporter=spec "$@" "$tests"
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
