# Backyard API conformance suite

A black-box HTTP + websocket test suite for the public contract of Backyard
(SPEC §17): the deploy gateway protocol (§7), static serving and the SDK
(§2–§3), the site API (§5: identity, CSRF, database, files, AI, warehouse) and
the realtime websocket protocol (§6). Both editions — the Go server and the
Cloudflare Worker in `deploy/cloudflare` — must pass it.

It runs on Node 22 with `node --test` and has no dependencies (global `fetch`
and `WebSocket`).

## Running

Against fresh local servers (what CI does):

```sh
conformance/run-go.sh        # backyard serve --dev on :18400, host mode then path mode
conformance/run-worker.sh    # wrangler dev --env local on :18450, path mode then host mode
                             # (run `npm ci` in deploy/cloudflare first)
```

| Pass | Identity | Features |
|---|---|---|
| Go, host mode (`<site>.localhost`) | anonymous guest | AI (mock provider), warehouse when `CONFORMANCE_WAREHOUSE_DSN` (or `BACKYARD_TEST_POSTGRES_DSN`) is set |
| Go, path mode (`/s/<site>/`) | static bearer token | no AI, no warehouse (the `501 unsupported` answers) |
| Worker, path mode | anonymous guest | AI (mock provider through the `openai` provider) |
| Worker, host mode (`BASE_DOMAIN=localhost`) | `DEPLOY_TOKEN` | no AI; no warehouse |

Pick passes and pass `node --test` options after `--`:

```sh
conformance/run-go.sh path -- --test-name-pattern="websocket"
BACKYARD_BIN=bin/backyard conformance/run-go.sh host      # skip the go build
```

Against any running server:

```sh
cd conformance
BACKYARD_URL=https://backyard.acme.dev BACKYARD_TOKEN=… node --test "tests/*.test.mjs"
```

The suite deploys a handful of uniquely named sites (`cf-<area>-<hex>`), and
deletes the ones it may delete. It needs a token, or a server that allows
anonymous deploys (`info.auth.anonymous_deploy`); without either, the
deploy-dependent tests are skipped.

## Configuration

| Variable | Meaning |
|---|---|
| `BACKYARD_URL` | hub URL of the server under test (required) |
| `BACKYARD_TOKEN` | bearer token sent with every request (optional) |
| `BACKYARD_URL_MODE` | `host` or `path` (default: `url_mode` of `/__backyard/api/info`) |
| `BACKYARD_SITE_URL_TEMPLATE` | site URL template, e.g. `https://{site}.apps.acme.dev/` (default: derived from the hub URL and `base_domain`) |
| `BACKYARD_RESOLVE_IP` | address that site host names resolve to (host mode without wildcard DNS) |
| `BACKYARD_CONFORMANCE_AI` | `mock`: the AI aliases are backed by `mock/openai.mjs` (exact answers are checked); `live`: call the real provider (costs tokens); empty: no provider calls |
| `BACKYARD_CONFORMANCE_SECRET` | the server's `server.secret` (`BACKYARD_SECRET`): lets the suite sign a Backyard API token of a non-anonymous user (SPEC §8.3) to check that tokens minted with it never outlive it; the runners set it. Without it, `BACKYARD_TOKEN` is used when it is a Backyard API token, else those tests are skipped |
| `BACKYARD_CONFORMANCE_PREFIX` | prefix of the test site names (default `cf`) |
| `BACKYARD_SKIP_SDK_BYTES=1` | do not compare the served SDK with `web/sdk` (testing another version) |
| `BACKYARD_QUERYCASES` | path of the shared query cases (default `internal/docstore/conformance/querycases.json`) |

Host mode on `localhost` works without `/etc/hosts` entries: Node's resolver
does not map `*.localhost` to loopback like browsers do, so the suite resolves
site host names that do not resolve to the hub's address (`lib/env.mjs`).

## Layout

| Path | What |
|---|---|
| `lib/env.mjs` | configuration, server discovery, site URLs, name resolution |
| `lib/http.mjs` | client with a cookie jar (one client = one browser = one guest), CSRF header, 429 retries, raw requests, error-envelope assertions |
| `lib/deploy.mjs` | the gateway protocol as a client (plan → upload → commit) |
| `lib/ws.mjs` | websocket client with a frame queue, handshake probe |
| `lib/shapes.mjs` | wire-shape assertions (User, SiteMeta, Doc, FileInfo, timestamps) |
| `lib/tokens.mjs` | Backyard API tokens (SPEC §8.3 key derivation and HS256 layout), JWT claims |
| `tests/*.test.mjs` | the suite, one file per area |
| `mock/openai.mjs` | deterministic OpenAI-compatible provider used by the runners |
| `run-go.sh`, `run-worker.sh` | runners for the two editions |

## Capability detection and edition differences

Tests detect capabilities instead of checking the edition:

* **AI** — `GET /ai/models` answering `501 unsupported` means every AI endpoint
  must answer the same; otherwise the request validation is checked, and with
  `BACKYARD_CONFORMANCE_AI=mock|live` the provider calls too.
* **Warehouse** — `GET /warehouse` answering `501` (always on the Cloudflare
  edition without `WAREHOUSE_URL`) means every warehouse endpoint must too.
  With a warehouse, raw SQL is checked when allowed and a named query
  `conformance_echo(msg)` is run when one exists (`run-go.sh` configures it).
* **Rate limits** — HTTP rate limiting differs by design (per-minute token
  buckets on the Go server, daily caps on the Worker); the suite checks the
  shared websocket limits (message rate, subscriptions, rooms, connections
  per user → `429` before the upgrade) and retries `429` answers elsewhere.
* **Dot segments** — `//` in a path is redirected (or `400` for writes) by both
  editions. `/./` and `/../` are resolved by the Cloudflare runtime before the
  Worker runs, so the request is served as the cleaned path; the Go server
  redirects. Both satisfy the goal of SPEC §2 (no handler sees a non-canonical
  path), and the suite accepts either.
* **Early `413` for oversized uploads** — checked by sending only the headers;
  skipped when the server (or a proxy such as `wrangler dev`) waits for the
  declared body before answering.
* **Browser sign-in** — the CLI login tests (`auth.test.mjs`: the cross-site
  confirmation page, its CSRF-checked `POST`, the direct `302`) need an
  identity the browser itself carries; they run in mode `none` (guest cookie)
  and are skipped when the cross-site probe answers anything but the
  confirmation page (`oidc`: a redirect to the IdP; a proxy mode without its
  identity: `401`). The hub's `login?rd=` redirect is checked outside `oidc`.
  In path mode sites share the hub's origin, so a site's `Referer` does not
  count as cross-site there.
* **Token identities** — with `BACKYARD_TOKEN` every client is the same user,
  so the anonymous-cookie and per-user connection-cap tests are skipped; site
  deletion is checked only for non-anonymous identities (guests get `401`).
