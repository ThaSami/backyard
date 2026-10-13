#!/bin/sh
# Run the cross-edition API conformance suite (conformance/, SPEC §17) against
# a freshly started local server (scripts/with-server.sh), then stop it.
#
#   scripts/conformance.sh            # Go server: backyard serve --dev
#   scripts/conformance.sh worker     # Cloudflare Worker: wrangler dev --env local
#   BACKYARD_URL=https://... BACKYARD_TOKEN=... scripts/conformance.sh external
#
# The suite receives BACKYARD_URL (and BACKYARD_TOKEN when there is one). It
# runs with `npm test` when conformance/package.json defines a test script,
# else with `node --test`. See scripts/with-server.sh for PORT, BACKYARD_BIN,
# BACKYARD_CONFIG and BACKYARD_URL_MODE.
set -eu

target="${1:-go}"
root="$(cd "$(dirname "$0")/.." && pwd)"
suite="$root/conformance"

[ -d "$suite" ] || {
	echo "error: no conformance suite at $suite" >&2
	exit 1
}

# Install the suite's dependencies before the server starts.
if [ -f "$suite/package.json" ]; then
	if [ -f "$suite/package-lock.json" ]; then
		(cd "$suite" && npm ci --no-audit --no-fund)
	else
		(cd "$suite" && npm install --no-audit --no-fund)
	fi
fi

cd "$suite"
if [ -f package.json ] && node -e 'process.exit(require("./package.json").scripts?.test ? 0 : 1)'; then
	exec "$root/scripts/with-server.sh" "$target" -- npm test
fi
exec "$root/scripts/with-server.sh" "$target" -- node --test
