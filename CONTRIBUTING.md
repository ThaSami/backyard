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
| `make cloudflare-pack` | Build the npm package `backyard-cloudflare` (prebuilt Worker and setup CLI) into `dist-npm/` (`CF_PACK_VERSION`, default: `deploy/cloudflare/package.json`'s version; `CF_PACK_OUT`) |
| `make validate-configs` | Validate the sample `backyard.yaml` files under `deploy/` (and the Helm chart's) |
| `make tf-validate` / `make helm-lint` / `make compose-config` | Validate the Terraform modules, the Helm chart, the Compose files |
| `make lambda` | Build `dist/backyard-lambda.zip` (Lambda deploy gateway, arm64) |
| `make docker` / `make docker-multiarch` | Container image for this machine / for `PLATFORMS` with buildx (no push) |
| `make release-snapshot` | All release artifacts with GoReleaser (no publish); builds the Cloudflare package first |
| `make clean` | Remove `bin/`, `dist/`, `dist-npm/` and coverage files |

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

Maintainers tag `vX.Y.Z`. In the [release workflow](.github/workflows/release.yml), the
read-only `cloudflare-package` job runs `make cloudflare` and builds the Cloudflare edition's npm
package with `make cloudflare-pack CF_PACK_VERSION=X.Y.Z` (`dist-npm/backyard-cloudflare-X.Y.Z.tgz`:
the prebuilt Worker, the setup CLI and an `npm-shrinkwrap.json` that pins wrangler), so the npm
toolchain never runs next to the release's write token. The `goreleaser` job runs the
pre-release checks, downloads only the package, checks it against the digest the first job
reported and embeds that SHA-256 in the CLI. GoReleaser then builds the binaries, archives,
checksums and the multi-arch image `ghcr.io/thasami/backyard`, and attaches the package, which
`checksums.txt` lists. By default, `backyard cloudflare setup` of a release installs exactly
that package: it checks the download against the embedded digest. `make release-snapshot`
builds the same locally without publishing (snapshot binaries embed no package digest). The
version is stamped into `internal/version` (`make build VERSION=1.2.3`).

To try the packaged Cloudflare flow before a release, run `make cloudflare-pack`, then
`npx ./dist-npm/backyard-cloudflare-0.3.0.tgz setup` or
`backyard cloudflare setup --package ./dist-npm/backyard-cloudflare-0.3.0.tgz`.

### npm

When the repository variable `BACKYARD_NPM_PUBLISH` is `true`, the release's `npm` job runs the
[npm workflow](.github/workflows/npm-publish.yml), which downloads the package from the GitHub
release, verifies it against `checksums.txt` and publishes exactly those bytes to npm as
`backyard-cloudflare`, with provenance. The dist-tag is `latest`, `next` for a pre-release tag
such as `v1.2.0-rc.1`, or `release-X.Y` for a version older than npm's `latest` (`v1.4.1` after
1.5.0, or `v1.5.0` published after 1.5.1), which leaves `latest` alone (`npm dist-tag` moves or
removes such tags). If npm cannot say what its `latest` is, other than for a package it does not
have yet, the job fails rather than guess. Publishes run one at a time; a run that arrives while
another one waits replaces the waiting one, which then has to be run again. Its job is the only
one with an OIDC token (`id-token: write`, which the release's `npm` job passes on), and the
only one that runs in the `npm` environment (Settings → Environments → New environment, named
`npm`):

- **`NPM_TOKEN`**: a secret of the `npm` environment, so no other job can read it (a repository
  secret works too). The environment can also require a reviewer's approval before each publish;
  if you limit its deployment branches and tags, allow the tags `v*` (and the branch you run the
  npm workflow from by hand, below).
- **`BACKYARD_NPM_PUBLISH`**: a **repository** variable (Settings → Secrets and variables →
  Actions → Variables → Repository variables), not a variable of the `npm` environment. The
  `npm` job's `if:` and the `goreleaser` job, which names the package in the release notes,
  cannot read environment variables, so a switch set only in the environment leaves the job
  skipped.

To publish a release whose npm job was skipped or failed, run the npm workflow by hand: Actions →
**Publish to npm** → Run workflow, with the release's tag (`vX.Y.Z`) as input. Under "Use
workflow from", pick that tag when it contains `npm-publish.yml` (releases after 0.2.0), so the
provenance names the release's commit; otherwise pick `main`, and the provenance names `main`'s
commit, while the bytes are still the release's own. GitHub re-runs a skipped job only by
re-running the whole release, which would build and upload everything again, so do not.

The guides show `npx backyard-cloudflare@latest setup` for people with only Node.js (saying it
works once a release has published the package), so the canonical repository should publish:

1. **Public repository.** npm accepts `--provenance` only from a public source repository
   (private repositories are rejected even for public packages). Make the repository public
   before setting `BACKYARD_NPM_PUBLISH` to `true`: while it is private, the `goreleaser` job
   fails before anything is published when the variable is on. If an `npm` job failed for this
   reason, make the repository public and re-run that job (or run the npm workflow by hand).
2. **First release (name registration).** The first real release registers `backyard-cloudflare`
   on npm under the maintainer account. The docs name the package, so register it before the
   repository becomes public, where anyone could read the name and publish it first: with the
   manual first publish below, or with a placeholder `backyard-cloudflare@0.0.0` published by
   hand, after which `NPM_TOKEN` can be limited to the package right away (step 3). Registering
   `backyard-cf` as well is optional, against typosquatting. Otherwise, a token limited to
   selected packages can name only packages that already exist, so the first `NPM_TOKEN` secret is a short-lived granular token (a few days) with
   "Read and write (publish and stage)" access to "All packages" and "Bypass two-factor
   authentication" checked: npm requires two-factor authentication or such a token for every
   publish, and CI cannot answer a 2FA prompt (a "stage only" token cannot publish). Add it as the `NPM_TOKEN`
   secret of the `npm` environment, set the repository variable `BACKYARD_NPM_PUBLISH` to `true`
   and tag the release. The job fails if the variable is on and the secret is missing. Check that this
   first `npm publish --provenance` succeeds: provenance compares the package's `repository.url`
   (spelled `ThaSami/backyard`, as in the git remote) with the repository.

   Alternatively, leave `BACKYARD_NPM_PUBLISH` off for the first tag and publish the release
   asset by hand with `npm publish ./backyard-cloudflare-X.Y.Z.tgz --access public`, without
   `--provenance` (npm generates provenance only in CI); the provenance check then moves to the
   next release.
3. **Right after that.** Replace `NPM_TOKEN` with a granular token that can publish only
   `backyard-cloudflare` (selectable now that the package exists), again with "Read and write
   (publish and stage)" and "Bypass two-factor authentication", and revoke the bootstrap token.
   The package's Publishing access must still allow tokens (not "Require two-factor
   authentication and disallow tokens").
4. **Before January 2027.** npm removes publishing directly with a granular access token in
   January 2027 ([About access tokens](https://docs.npmjs.com/about-access-tokens)), so by then
   the workflows must use [trusted publishing](https://docs.npmjs.com/trusted-publishers) (OIDC,
   no token). npm checks the file name of the workflow that was triggered, not of the one that
   runs `npm publish`, so the package needs two trusted publishers (package Settings → Trusted
   publishing): `release.yml` for tag releases and `npm-publish.yml` for runs by hand, both with
   the environment `npm` and with `npm publish` allowed (new trusted publishers allow only staged
   publishing by default). `npm-publish.yml` then needs npm 11.5.1 or later (and Node.js 22.14.0
   or later) and no `NPM_TOKEN` check; both workflows already grant `id-token: write`.

Without npm publishing (forks, for example), every GitHub release still carries the package:
`npx --package https://github.com/thasami/backyard/releases/download/vX.Y.Z/backyard-cloudflare-X.Y.Z.tgz backyard-cloudflare setup`.

### Real-account checklist (Cloudflare)

CI tests the Cloudflare edition only against a fake wrangler and a fake Cloudflare API. Before
the first tag, and whenever wrangler is bumped, run these by hand on a free Cloudflare account
with the release candidate's package (`make cloudflare-pack`, then
`npx ./dist-npm/backyard-cloudflare-<version>.tgz <command>`):

1. A fresh account without a workers.dev subdomain: `setup` registers one and the hub answers.
2. `domain add backyard.<zone>` with the browser (OAuth) login: the custom domain is attached
   with OAuth; another custom domain of the Worker survives; an existing A record at the host is
   reported as a conflict; `domain remove` detaches the domain with OAuth.
3. Host mode at the apex of a dedicated zone (`domain add <zone> --host-mode`): the route, the
   guided wildcard DNS record and its detection, the certificate probe, and sites served with
   `URL_MODE=host`.
4. `access enable`: in API mode (`BACKYARD_CF_API_TOKEN`) Cloudflare accepts the applications and
   policies, all nine bypass paths (the five machine endpoints and the Claude connector's four)
   bypass Access in one application (check the per-application destination limit; including
   `/__backyard/api/deploy/*`, and `/__backyard/oauth/*` matching `/__backyard/oauth/token`) and
   the default one-time PIN login works; in guided mode the Access redirect carries the
   application's AUD.
5. `access disable --delete-apps` deletes what `access enable` created.
6. A second machine (an empty `XDG_CONFIG_HOME`): `setup` adopts the deployed configuration, and
   a deploy that would turn Access off is refused.
7. The CI deployer (`scripts/cloudflare-ci.mjs`) refuses the same downgrade unless
   `BACKYARD_CF_CONFIRM_AUTH_CHANGE=true` (in `cloudflare-deploy.yml`: a manual run with the
   `confirm_auth_change` input checked; a push deploy never skips the guard).
8. The release's `npm publish --provenance` succeeds.
9. The Claude connector (`setup --var MCP_ENABLED:true`, docs/claude-connector.md): with Access
   on again (`access enable`; the check needs `AUTH_MODE=access` and a custom domain), `status`
   reports `access-connector` ok both before and after `MCP_ENABLED`; with the four connector
   destinations removed from the bypass application (as in one created before this release), it
   is pending while `MCP_ENABLED` is off and failed once it is on, and `access enable` turns it
   ok again; behind Access (with
   Okta as identity provider) authorize → Access login → consent → claude.ai works and the
   consent `POST` passes Access; on Workers Free, `backyard_publish` calls of 256 KiB, 1 MiB and
   2 MiB (text and base64) stay within the CPU limit (no error 1102, else lower
   `LIMITS.mcp_publish_bytes`); `/__backyard/readyz` names an invalid `MCP_CLIENTS`.
