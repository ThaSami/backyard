# Contributing to Backyard

Thanks for helping! Backyard is a small platform with a big surface — a Go server and CLI, a
browser SDK, a Cloudflare Workers edition, agent docs and deployment kits — held together by one
[specification](docs/spec/SPEC.md). This guide covers the development setup, the tests and how
to get a change merged. By participating you agree to the [code of conduct](CODE_OF_CONDUCT.md).
Security problems go to the private channel in [SECURITY.md](SECURITY.md), not to issues.

## Setup

Required:

- **Go 1.26+** — the server, CLI and most tests.
- **Node.js 22+** — SDK and hub tests, the Cloudflare edition, the conformance and end-to-end
  suites.
- **make** and a POSIX shell.

Optional, for the parts you touch:

- **Docker** (with Compose) — integration-test services, container image.
- **golangci-lint v2** — `make lint` runs it when installed.
- **Terraform ≥ 1.7**, **Helm 3.8+** — deployment kits.
- **Chromium** — browser tests. The Playwright suite in `e2e/` uses Playwright's own build
  (`npx playwright install chromium`, or one already in `PLAYWRIGHT_BROWSERS_PATH`); the SDK and
  hub browser tests in `web/test/` load `playwright-core` (`PLAYWRIGHT_CORE`) and skip without
  it.

```sh
git clone https://github.com/thasami/backyard.git && cd backyard
make dev                                   # go run ./cmd/backyard serve --dev → http://localhost:8080
make build && ./bin/backyard version       # a stamped binary in bin/
```

The [architecture overview](docs/architecture.md) maps the packages; [CLAUDE.md](CLAUDE.md)
summarizes the layout and invariants for coding agents (and humans in a hurry).

## Make targets

`make` (or `make help`) lists everything. The ones you will use most:

| Target | What it does |
|---|---|
| `make dev` | Run a local dev server (SQLite + files in `./.backyard-data`, no auth) |
| `make build` / `make install` | Build `bin/backyard` / install into `$GOBIN` |
| `make test` | `go test -race -count=1 ./...`; uses the integration services when they run |
| `make test-short` | Unit tests only (`-short`, no race detector) |
| `make cover` | Coverage report → `coverage.out`, `coverage.html` |
| `make lint` / `make vet` / `make fmt` | gofmt check + `go vet` + golangci-lint / `go vet` / `gofmt -w` |
| `make sdk` | Regenerate `web/sdk/backyard.{js,mjs}` from `web/sdk/src` |
| `make sdk-test` | Check the SDK build is in sync and run the SDK and hub tests (`node --test`) |
| `make scripts-test` | Test the release/CI tooling in `scripts/` (Go and `node --test`) |
| `make services-up` / `make services-down` | Start / stop Postgres, Redis, DynamoDB Local and Azurite |
| `make conformance` | Cross-edition API conformance suite: `conformance/run-go.sh` and `conformance/run-worker.sh` (`CONFORMANCE_TARGET=go`, `worker` or `all`, the default; `CONFORMANCE_ARGS` go to the runners) |
| `make e2e` | Browser end-to-end tests: the Playwright suite in `e2e/` (`cd e2e && npm ci && npm test`; the default `E2E_TARGET=go`), or `web/test/e2e.test.mjs` against the Workers edition (`E2E_TARGET=worker`) |
| `make cloudflare` / `make cloudflare-dev` | Install, typecheck and test the Workers edition / run it locally without a Cloudflare login |
| `make cloudflare-bundle` | Bundle the Worker like a CI deploy (`wrangler deploy --dry-run`) |
| `make cloudflare-setup` | Deploy the Workers edition to your own Cloudflare account (`CF_SETUP_ARGS`) |
| `make validate-configs` | Validate the sample `backyard.yaml` files under `deploy/` (and the Helm chart's) |
| `make tf-validate` / `make helm-lint` / `make compose-config` | Validate the Terraform modules, the Helm chart, the Compose files |
| `make lambda` | Build `dist/backyard-lambda.zip` (Lambda deploy gateway, arm64) |
| `make docker` / `make docker-multiarch` | Container image for this machine / for `PLATFORMS` with buildx (no push) |
| `make release-snapshot` | All release artifacts with GoReleaser (no publish) |
| `make clean` | Remove `bin/`, `dist/` and coverage files |

## Tests

### Unit tests

```sh
make test                                    # everything, with -race
go test ./internal/docstore/...              # one area
go test -run TestQuery ./internal/server/    # one test
```

Tests that need an external service skip themselves when it is not configured, so `make test`
always works on a laptop.

### Integration tests

The document store, pub/sub and blob backends have integration tests against real services.
[`deploy/docker-compose/test-services.yml`](deploy/docker-compose/test-services.yml) runs them
(in memory, bound to 127.0.0.1):

```sh
make services-up        # postgres, redis, dynamodb-local, azurite (docker compose … up -d --wait)
make test               # exports the BACKYARD_TEST_* variables below while the services run
make services-down
```

To run `go test` by hand, export the variables yourself:

```sh
export BACKYARD_TEST_POSTGRES_DSN='postgres://backyard:backyard@127.0.0.1:5432/backyard?sslmode=disable'
export BACKYARD_TEST_REDIS_URL='redis://127.0.0.1:6379/0'
export BACKYARD_TEST_DYNAMODB_ENDPOINT='http://127.0.0.1:8000'
export BACKYARD_TEST_AZURITE='DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;'
go test -race ./internal/docstore/... ./internal/pubsub/... ./internal/blob/...
```

Ports can be moved with `BACKYARD_TEST_{POSTGRES,REDIS,DYNAMODB,AZURITE}_PORT` (then export the
URLs to match). Every document store backend runs the shared conformance cases in
`internal/docstore/conformance` (`querycases.json` is also run by the Cloudflare edition), so a
query-semantics change shows up in every backend at once.

### JavaScript: SDK, hub and Workers edition

The SDK source is `web/sdk/src/core.js`; `web/sdk/backyard.js` and `backyard.mjs` are
generated and committed (the Go binary embeds them, the Worker bundles them). After changing the
source:

```sh
make sdk                # regenerate
make sdk-test           # build is in sync + node --test web/sdk/test web/test
make cloudflare         # npm ci, typecheck and vitest for deploy/cloudflare
```

`web/sdk/backyard.d.ts` is the normative SDK surface; keep it, the source and the skill's API
reference in agreement.

### Conformance tests

The black-box suite in [`conformance/`](conformance/README.md) (SPEC §17) checks the public
contract — the deploy gateway protocol, static serving and the SDK, the site API and the
websocket protocol — and **both editions must pass it**. It runs on Node 22 with `node --test`.
Its runners start a fresh local server for every pass, run the suite and stop the server:

```sh
conformance/run-go.sh        # backyard serve --dev: host mode as a guest (mock AI provider), then path mode with a static token
conformance/run-worker.sh    # wrangler dev --env local: path mode as a guest, then host mode with the DEPLOY_TOKEN
                             # (run `npm ci` in deploy/cloudflare first)
make conformance             # both runners (CONFORMANCE_TARGET=go|worker|all), installing the Worker's dependencies
```

Pick a pass, and pass `node --test` options after `--`:

```sh
conformance/run-go.sh path -- --test-name-pattern=websocket
BACKYARD_BIN=bin/backyard conformance/run-go.sh host       # skip the go build
make conformance CONFORMANCE_TARGET=go CONFORMANCE_ARGS="path -- --test-name-pattern=files"
```

The Go runner adds a warehouse source when `CONFORMANCE_WAREHOUSE_DSN` (or
`BACKYARD_TEST_POSTGRES_DSN`) names a PostgreSQL database; otherwise the warehouse checks
expect `501 unsupported`. To test a server that is already running:
`BACKYARD_URL=https://… BACKYARD_TOKEN=… scripts/conformance.sh external`. The
[suite's README](conformance/README.md) lists every variable and the edition differences it
tolerates. A contract change extends the suite in the same pull request.

### End-to-end tests (Playwright)

The real-browser suite in [`e2e/`](e2e/README.md) drives the hub, the docs, every example, the
`backyard init` templates, the SDK and the sign-in flows in Chromium, against `backyard serve`
processes it builds from the checkout. It runs offline: AI goes to a fake OpenAI-compatible
upstream and sign-in to a mock OpenID Connect provider, both started by the harness.

```sh
cd e2e
npm ci
npx playwright install chromium   # once; skip it when PLAYWRIGHT_BROWSERS_PATH already holds a matching Chromium
npm test                          # everything, headless (make e2e from the root does npm ci && npm test)
```

CI installs the browser together with its system libraries (`npx playwright install
--with-deps chromium`) and skips the build by passing the binary of `make build` as
`BACKYARD_BIN` (from `e2e/`: `BACKYARD_BIN=../bin/backyard npm test`). `BACKYARD_E2E_CHROMIUM`
launches another Chromium binary, and `BACKYARD_TEST_POSTGRES_DSN` adds the
dashboard-on-a-real-warehouse test. The [suite's README](e2e/README.md) lists the ports,
variables and conventions (every test fails on a console error).

`make e2e E2E_TARGET=worker` runs the smaller browser test `web/test/e2e.test.mjs` against the
Workers edition under `wrangler dev --env local`, with `playwright-core` from
`e2e/node_modules` (or `PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs`).

### Deployment kits

```sh
make validate-configs   # sample configs load with Backyard's own strict loader
make tf-validate        # terraform fmt -check, init -backend=false, validate
make helm-lint          # helm lint --strict + template, default and production values
make compose-config     # docker compose config
```

The Terraform modules also have `terraform test` suites with mocked providers
(`deploy/terraform/*/tests`).

## Code style

**Go**

- `gofmt`, `go vet` and `golangci-lint` clean (`make lint`; configuration in
  [`.golangci.yml`](.golangci.yml)).
- Standard library first; new dependencies need a good reason (and `go mod tidy`).
- Wrap errors with context (`fmt.Errorf("sites: list metadata: %w", err)`), use sentinel errors
  for conditions callers branch on, never panic on bad input.
- Pass `context.Context` through every backend call and bound it; log with `log/slog`
  (structured, no secrets, no user content).
- Exported identifiers have doc comments; packages have a package comment that explains their
  role and invariants.
- Every fix comes with a test; table-driven tests are welcome. Integration tests skip cleanly
  without their service.

**JavaScript / TypeScript**

- The SDK and hub have **no runtime dependencies** and no build step at serve time.
- The Workers edition is strict TypeScript (`npm run typecheck`); tests use vitest in workerd.

**Docs**

- User-facing docs live in `docs/`; agent-facing docs in `skills/backyard/` (the single source
  for the skill, `/llms.txt`, `/docs` and the init templates). Document only what exists:
  every command, flag, config key and default must match the code.
- `go test ./docs/` checks the guides (part of `make test`): relative links and `#anchors`
  resolve, every `make <target>` they mention exists, every key of the configuration schema
  has a row in [docs/configuration.md](docs/configuration.md) (and every row is a real key),
  every `§n` names a section of the specification, and statements tied to code match it (key
  derivation labels, site URL examples, limits, quoted configuration errors, AI limits, the MCP
  tools and their parameters, the passive upload types of both editions, the warehouse and
  realtime size limits, the direct-sync command flags, the files no ignore rule can deploy, the
  login state cookie, and the Cloudflare edition's page budget and Access bypass paths).
- Keep the [changelog](CHANGELOG.md) up to date: add user-visible changes to the topmost,
  unreleased version.

## The public contract

The URLs, site API, websocket protocol, deploy gateway protocol, SDK surface and config schema
are a contract shared by the Go server, the Cloudflare edition, the SDK, the CLI, the MCP server
and the docs. A change to any of them must, in the same pull request:

1. update [docs/spec/SPEC.md](docs/spec/SPEC.md) (it is normative);
2. land in **both editions** (`internal/…` and `deploy/cloudflare/…`) — or explain why it
   cannot;
3. extend the conformance suite (and `internal/docstore/conformance` for query semantics);
4. update `web/sdk/backyard.d.ts`, the skill (`skills/backyard/`), `docs/` and sample configs
   where relevant.

## Commits and pull requests

- Branch from `main`; keep pull requests focused (one feature or fix) and reasonably small.
- Commit subjects in the imperative mood, ≤ 72 characters ("Add Redis pub/sub reconnect
  backoff"), with a body explaining *why* when it isn't obvious. Reference issues with
  `Fixes #123`.
- Fill in the [pull request template](.github/PULL_REQUEST_TEMPLATE.md): what and why, how it
  was tested, and the checklist (tests, `make lint`, spec and both editions for contract
  changes, `make sdk` for SDK changes, docs).
- CI must be green: Go (vet, race tests with services), golangci-lint, SDK and hub, Cloudflare
  edition, conformance against both editions, the end-to-end tests in Chromium, Terraform and
  Helm.
- No DCO sign-off or CLA is required. By submitting a contribution you agree to license it
  under the project's [Apache License 2.0](LICENSE).
- Never commit secrets, real tokens or internal hostnames — not in code, tests, fixtures or
  logs.

## Releases

Maintainers tag `vX.Y.Z`; GoReleaser builds the binaries, archives, checksums and the
multi-arch image `ghcr.io/thasami/backyard` (`make release-snapshot` builds the same locally
without publishing). The version is stamped into `internal/version` (`make build
VERSION=1.2.3`).
