#!/bin/sh
# Run the cross-edition API conformance suite (conformance/, SPEC §17).
#
#   scripts/conformance.sh [go] [ARGS]     # conformance/run-go.sh: backyard serve --dev
#   scripts/conformance.sh worker [ARGS]   # conformance/run-worker.sh: wrangler dev --env local
#   BACKYARD_URL=https://... BACKYARD_TOKEN=... scripts/conformance.sh external
#
# go and worker hand over to the suite's own runners, which start fresh local
# servers for each pass (host and path mode, a mock AI provider; see
# conformance/README.md); ARGS go to them unchanged, e.g.
# `scripts/conformance.sh go path -- --test-name-pattern=files`. `make
# conformance` (CONFORMANCE_TARGET=go|worker|all) runs the same runners.
#
# external runs the suite against the server at BACKYARD_URL (and
# BACKYARD_TOKEN when there is one), through scripts/with-server.sh: with
# `npm test` when conformance/package.json defines a test script, else with
# `node --test`.
set -eu

target="${1:-go}"
[ $# -gt 0 ] && shift
root="$(cd "$(dirname "$0")/.." && pwd)"
suite="$root/conformance"

[ -d "$suite" ] || {
	echo "error: no conformance suite at $suite" >&2
	exit 1
}

case "$target" in
go) exec "$suite/run-go.sh" "$@" ;;
worker) exec "$suite/run-worker.sh" "$@" ;;
external) ;;
*)
	echo "usage: $0 [go|worker|external] [runner args]" >&2
	exit 2
	;;
esac

# Install the suite's dependencies (if it has any) before the run.
if [ -f "$suite/package.json" ]; then
	if [ -f "$suite/package-lock.json" ]; then
		(cd "$suite" && npm ci --no-audit --no-fund)
	else
		(cd "$suite" && npm install --no-audit --no-fund)
	fi
fi

cd "$suite"
if [ -f package.json ] && node -e 'process.exit(require("./package.json").scripts?.test ? 0 : 1)'; then
	exec "$root/scripts/with-server.sh" external -- npm test
fi
exec "$root/scripts/with-server.sh" external -- node --test
