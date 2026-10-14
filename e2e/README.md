# Backyard end-to-end tests (Playwright)

Real-browser tests of the hub, the docs, every example in [`examples/`](../examples), the pages
`backyard init` writes, the browser SDK and the sign-in flows, against real `backyard serve`
processes built from this checkout. Everything runs offline: AI goes to a fake
OpenAI-compatible upstream and sign-in to a mock OpenID Connect provider, both started by the
harness.

## Run

Needs Go (to build `./cmd/backyard`) and Node 22.

```sh
cd e2e
npm ci
npx playwright install chromium   # once; skip when PLAYWRIGHT_BROWSERS_PATH already has it
npm test                          # everything, headless
```

`make e2e` from the repository root does `npm ci && npm test` here.

Useful variations:

```sh
npx playwright test tests/sites/lunch-vote.spec.mjs   # one file
npx playwright test --project auth                    # only the sign-in tests
npx playwright test -g "reconnect"                    # tests whose title matches
npx playwright test --headed --workers 1              # watch it happen
npx playwright show-trace test-results/<test>/trace.zip   # after a failure
```

| Variable | Effect |
|---|---|
| `BACKYARD_BIN` | Use this `backyard` binary instead of running `go build` (CI passes `bin/backyard`). |
| `BACKYARD_E2E_PORT` | First of the nine ports the servers use (default `18500`, see below). |
| `BACKYARD_E2E_WORKERS` | Parallel workers (default 4, 2 on CI). |
| `BACKYARD_E2E_CHROMIUM` | Launch this Chromium binary instead of Playwright's own build. |
| `BACKYARD_TEST_POSTGRES_DSN` | Also run the KPI dashboard against a real Postgres warehouse (skipped otherwise). |
| `BACKYARD_E2E_PRINT_PROBLEMS` | Print every console error / page error a test saw, allowed or not. |
| `CI` | Retries once, forbids `test.only`, adds the GitHub and HTML reporters (`playwright-report/`). |

## What runs where

[`global-setup.mjs`](global-setup.mjs) builds the binary and starts, for the whole run:

| Port | Server |
|---|---|
| `18500` | **main**: `backyard serve --dev`, host mode on `*.localhost` (Chromium resolves those to loopback), guests (`auth.mode: none`), AI through the fake upstream, no warehouse. Every example and two `backyard init` sites (`tmpl-blank`, `tmpl-app`) are deployed with `backyard deploy --site <name>`. |
| `18501` | **oidc**: `auth.mode: oidc` against the mock IdP, path mode on `127.0.0.1` (`/s/<site>/`), admins by group, only `example.com` addresses, claims enriched from userinfo. The same sites are deployed with a static deploy token. |
| `18503` | **mock IdP** (`http://localhost:18503`): discovery, JWKS, an authorization endpoint (silent when the browser has an IdP session, else "choose an account"), a token endpoint issuing RS256 ID tokens (PKCE, nonce, `at_hash`), userinfo, end-session. |

Spec files that would disturb the others start servers of their own: `18502` (the reconnect test
kills and restarts it), `18504` (phone layouts), `18505` (Postgres warehouse), `18506`
(accessibility checks with data), `18507` and `18508` (an `auth.mode: oidc` server with the
Claude connector enabled, `mcp.enabled`, and a mock IdP of its own). The fake AI, and the
connector test's client callback (its loopback redirect URI), listen on random ports.

Path mode for the OIDC server is deliberate: browsers refuse `Domain=localhost` cookies, so in host
mode on `localhost` the hub's session cookie never reaches `<site>.localhost` (see SPEC §8.3).

| Project | Spec | Covers |
|---|---|---|
| sites | `hub.spec.mjs` | site directory, titles/descriptions, search, list/grid, copy buttons, CLI commands |
| sites | `hub-deploy.spec.mjs` | folder picker, file picker + `.backyard.json`, synthesized and real OS drag and drop, diffed redeploys, ignores, rejected tokens, a `#token=` link that asks before it is used, deleting with a token |
| sites | `lunch-vote`, `live-poll`, `cursors`, `templates` | realtime between two or more browser contexts (= people): data, presence joining and leaving |
| sites | `ai-chat.spec.mjs` | token-by-token streaming, Markdown, history, model/style choice, upstream failures; SDK image, embeddings, JSON |
| sites | `file-drop.spec.mjs` | picker, drop and paste uploads, live gallery, filters, viewer, delete |
| sites | `kpi-dashboard`, `kpi-warehouse` | demo-data fallback without a warehouse; real rows from a named query |
| sites | `reconnect.spec.mjs` | kill -9 / graceful restart: the SDK reconnects, re-subscribes, live queries resync, rooms rejoin |
| sites | `sdk-realtime.spec.mjs` | `socket.disconnect()`/`connect()`, presence differences after a reconnect, direct messages |
| sites | `docs.spec.mjs` | `/docs` renders `/docs/api.md`, table of contents, document switching |
| sites | `security.spec.mjs` | uploaded HTML/SVG sandboxed; a site cannot call another site's API or websocket |
| sites | `responsive`, `a11y` | no horizontal scrolling on a phone, light and dark mode, axe-core WCAG 2.1 AA with real data |
| auth | `oidc.spec.mjs` | site visit → IdP → back with the mapped identity (`backyard.me()`), cookies, 401s, allowed domains, sign-out |
| auth | `sessions.spec.mjs` | hub deploys with the browser session, the CLI's browser sign-in (and its confirmation page when another site links to it), re-sign-in from the hub, docs and SDK |
| auth | `examples.spec.mjs` | the examples in path mode with real names and avatars, AI/warehouse not configured |
| auth | `mcp-connector.spec.mjs` | the Claude connector with a real MCP client (`@modelcontextprotocol/sdk`): registration, sign-in at the IdP, the consent page (axe-core), publishing a site, consent shown again on a second connection, Cancel (`access_denied`), a person outside the allowed domains, the hub card and Disconnect cutting the client off |

## Conventions for tests

- Import `test`/`expect` from [`tests/fixtures.mjs`](tests/fixtures.mjs). Every test fails when a
  page throws or logs a console error — including the browser's own "Failed to load resource: …
  404" lines. A test that provokes one on purpose says so:
  `pageProblems.allow(/the server responded with a status of 409/)`.
- `newUser()` opens a browser context: a separate person (own cookies, so a new guest on the main
  server). `openPage(user, url)` waits for the SDK.
- Specs run in parallel; the tests inside one file run in order. Only one spec file uses a given
  site on the main server, because presence counts ("2 deciding", "● 2 online") would see the
  people of another file. Use unique data (`rand()`) and clean up what you create.
- `httpGet`/`httpRequest` reach `*.localhost` from Node without cookies (handy for 404 checks
  that would otherwise land in a page's console).
- The harness ([`harness/`](harness)) is plain Node: `BackyardServer` (start, stop, kill, logs),
  `backyardCLI`/`deploySite`, `startScratchServer`, the fake AI and the mock IdP.
