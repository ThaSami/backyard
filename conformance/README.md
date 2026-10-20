# Backyard API conformance suite

A black-box HTTP + websocket test suite for the public contract of Backyard
(SPEC §17): the deploy gateway protocol (§7), static serving and the SDK
(§2–§3), the site API (§5: identity, CSRF, database, files, AI, warehouse),
the realtime websocket protocol (§6) and the Claude connector (§8.4, §14.1:
Backyard's OAuth endpoints and the remote MCP endpoint). Both editions — the
Go server and the Cloudflare Worker in `deploy/cloudflare` — must pass it.

It runs on Node 22 with `node --test` and has no dependencies (global `fetch`
and `WebSocket`).

## Running

Against fresh local servers (what CI does):

```sh
conformance/run-go.sh        # backyard serve on :18400: host mode, path mode, oidc, then listing
conformance/run-worker.sh    # wrangler dev --env local on :18450: path mode, host mode, then listing
                             # (run `npm ci` in deploy/cloudflare first)
```

Every pass enables the Claude connector (`mcp.enabled: true`, `--var MCP_ENABLED:true`) and
runs with `BACKYARD_CONFORMANCE_MCP=1`. The connector tests sign in through their own browser
(`lib/oauth.mjs`), not with the pass's token.

| Pass | Identity | Features |
|---|---|---|
| Go, host mode (`<site>.localhost`) | anonymous guest | AI (mock provider), warehouse when `CONFORMANCE_WAREHOUSE_DSN` (or `BACKYARD_TEST_POSTGRES_DSN`) is set; connector as guests |
| Go, path mode (`/s/<site>/`) | static bearer token | no AI, no warehouse (the `501 unsupported` answers); connector as guests |
| Go, `oidc` (path mode on `127.0.0.1`, without `--dev`) | the mock IdP's `alice` and `bob` (`mock/idp.mjs` on `CONFORMANCE_IDP_PORT`, default 18480) | only `tests/mcp.test.mjs`: the connector's sign-in through `auth.mode: oidc` |
| Go, `listing` (path mode) | API tokens the suite signs for its people; the static token, here with `admin: true` | only `tests/listing.test.mjs`: `sites.listing: owners` (ownership by its default, `owner`) and the admin `listing-admin@conformance.test` in `auth.admins`; connector as guests |
| Worker, path mode | anonymous guest | AI (mock provider through the `openai` provider); connector as guests |
| Worker, host mode (`BASE_DOMAIN=localhost`) | `DEPLOY_TOKEN` | AI turned off (`AI_ENABLED=false`, with the mock aliases configured, so only the switch answers `501`); no warehouse; connector as guests |
| Worker, `listing` (path mode) | API tokens the suite signs for its people; `DEPLOY_TOKEN` (an admin) | only `tests/listing.test.mjs`: `SITE_LISTING=owners` (`OWNERSHIP` unset, so `owner`) and `ADMINS=listing-admin@conformance.test`; connector as guests |

The `listing` passes run with `BACKYARD_CONFORMANCE_LISTING=owners`, every other pass with
`all`.

Pick passes and pass `node --test` options after `--`:

```sh
conformance/run-go.sh path -- --test-name-pattern="websocket"
conformance/run-go.sh oidc                                # only the connector's sign-in pass
conformance/run-worker.sh listing                         # only the owners-only site listing
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
| `BACKYARD_CONFORMANCE_AI` | `mock`: the AI aliases are backed by `mock/openai.mjs` (exact answers are checked); `live`: call the real provider (costs tokens); `off`: AI is turned off on a server with aliases (`ai.enabled: false`, `AI_ENABLED=false`), so every AI endpoint must answer `501 unsupported`; empty: no provider calls |
| `BACKYARD_CONFORMANCE_SECRET` | the server's `server.secret` (`BACKYARD_SECRET`): lets the suite sign Backyard API tokens of non-anonymous users (SPEC §8.3) to check that tokens minted with one never outlive it, and to be several people in `tests/listing.test.mjs`; the runners set it. Without it, `BACKYARD_TOKEN` is used for the former when it is a Backyard API token, else those tests are skipped |
| `BACKYARD_CONFORMANCE_LISTING` | `all` or `owners`: the `site_listing` the server must report in `/__backyard/api/info` (`sites.listing`, `SITE_LISTING`; SPEC §7.1, §7.2), else `tests/listing.test.mjs` fails; unset: it tests the mode the server reports. The runners set `owners` in their `listing` pass and `all` in the others |
| `BACKYARD_CONFORMANCE_ADMIN` | an email (or user id) the server counts as an admin (`auth.admins`, `ADMINS`): with `site_listing: owners`, `tests/listing.test.mjs` signs that admin's API token to check that admins are listed every site; unset: those checks are skipped (`BACKYARD_TOKEN` is still checked against what `/me` says it is) |
| `BACKYARD_CONFORMANCE_MCP` | `1`: the Claude connector must be enabled (`mcp_url` in `/__backyard/api/info`), else `tests/mcp.test.mjs` fails; unset: it is skipped when the connector is off. With `1` the suite also expects the default client and redirect allowlists (`mcp.clients`, `mcp.redirect_uris`) |
| `BACKYARD_CONFORMANCE_IDP` | issuer URL of the mock OpenID Connect provider (`mock/idp.mjs`) of an `auth.mode: oidc` server: the connector tests sign in there with the cookie `idp_session=<user>` |
| `BACKYARD_CONFORMANCE_IDP_USERS` | the IdP's people the connector tests sign in as (default `alice,bob`; the second one is "someone else") |
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
| `lib/oauth.mjs` | the connector's OAuth flows: a `Browser` (cookie jar, manual redirects across origins, Fetch Metadata), PKCE, registration, authorize + consent, token, refresh, revoke, a strict `WWW-Authenticate` parser |
| `lib/mcp.mjs` | JSON-RPC client of `/__backyard/mcp` that checks every answer (no `Mcp-Session-Id`, no cookies, no event stream) |
| `tests/*.test.mjs` | the suite, one file per area (`mcp.test.mjs`: the Claude connector; `listing.test.mjs`: who is listed which site) |
| `mock/openai.mjs` | deterministic OpenAI-compatible provider used by the runners |
| `mock/idp.mjs` | the e2e harness's mock OpenID Connect provider (`e2e/harness/mock-idp.mjs`) as a process, for the `oidc` pass |
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
  Every client of the suite comes from `127.0.0.1` and the Go server limits
  guests (and the Claude connector's OAuth requests that identify no one) per IP, so `run-go.sh`
  raises `ratelimit.api_per_minute` for the host and path passes: a websocket
  handshake cannot be retried. It raises `ratelimit.deploys_per_hour` too: the
  suite creates more deploys than the default 60 an hour.
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
* **Claude connector** — `tests/mcp.test.mjs` runs when `/info` has `mcp_url`. The tests that
  need a signed-in browser (consent, tokens, tools, grants) run in mode `none` (every
  `Browser` is a new guest) and in `oidc` with `BACKYARD_CONFORMANCE_IDP`; with a proxy in
  front (`iap`, `alb`, `header`, Access) only discovery, registration and the authorization
  request checks run. The tool catalog is compared with `internal/mcpremote/tools.json` of this
  checkout. No test needs outbound HTTPS: the clients register themselves, and the unlisted
  client metadata document tests use a `client_id` on an `.example` host that Backyard must
  accept with a loopback redirect URI without fetching it (SPEC §8.4.3).
* **Site listing** — `tests/listing.test.mjs` follows `site_listing` in `/info`. With `all` a
  site one person deployed must be listed to another person and to a guest (in mode `none`).
  With `owners` the suite deploys sites as its people (one shared through the owners endpoint by
  id, one by email in another case, one left without owners) and checks, in both the hub's list
  and `GET {gateway}/sites`, that each person is listed exactly the sites they own or created
  while ownerless, that the admin is listed every one and a guest none, that the preview route
  answers `404 not_found` for a site not listed to the caller, that a guest's connector
  `backyard_list_sites` counts nothing, and that the lookups by name (`GET {gateway}/sites/{site}`,
  its files, the site API's `GET /site`, the site itself, `backyard_site_files`) still answer.
  The other test files assume `all`: they expect a site their identity deployed to be listed to
  it, which a guest's is not with `owners`.
* **Token identities** — with `BACKYARD_TOKEN` every client is the same user,
  so the anonymous-cookie and per-user connection-cap tests are skipped; site
  deletion is checked only for non-anonymous identities (guests get `401`).
