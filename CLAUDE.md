# CLAUDE.md — working on the Backyard repository

Backyard turns a folder of HTML into an internal URL with a zero-config backend (db + realtime,
files, AI, warehouse, websockets, identity). This file is for contributors working **on this
repository** with Claude Code. (The `CLAUDE.md` that `backyard init` writes into user projects
comes from `templates/files/agents/CLAUDE.md.tmpl` — a different thing.)

## Layout

| Path | What |
|---|---|
| `docs/spec/SPEC.md` | **The normative design spec.** URLs, wire formats, defaults, semantics. |
| `cmd/backyard` | The single binary (cobra): `serve`, `gateway`, `dev`, `init`, `deploy`, `login`, `mcp`, … |
| `cmd/backyard-lambda` | The deploy gateway as an AWS Lambda function |
| `internal/app` | Wiring config → components → `http.Handler` |
| `internal/config` | Config schema, `${ENV}` expansion, defaults, validation (`example.yaml` = `backyard config example`) |
| `internal/server` | HTTP layer: routing, static, site API, hub, health, metrics |
| `internal/{auth,keys,identity}` | Authenticators, sessions, tokens, HKDF key derivation, users |
| `internal/{docstore,blob,pubsub}` (+ backends) | Storage abstractions; `docstore/eval.go` is the reference query semantics |
| `internal/{realtime,ai,warehouse,deploy,sites,ratelimit}` | The capabilities |
| `internal/{cli,clientconfig,mcpserver}` | CLI commands, local profiles, MCP tools |
| `web/sdk` | Browser SDK: edit `src/core.js`; `backyard.{js,mjs}` are **generated** (`make sdk`); `backyard.d.ts` is normative |
| `web/hub` | Hub UI (embedded) |
| `skills/backyard` | The Claude skill — single source for agent docs, `/llms.txt`, `/docs` and init templates |
| `templates/` | `backyard init` templates (agent files extract parts of `SKILL.md`) |
| `deploy/cloudflare` | The Cloudflare Workers edition (TypeScript); `src/generated/` is generated, don't edit |
| `deploy/{terraform,helm,docker-compose,systemd}` | Deployment kits |
| `conformance/`, `e2e/` | Cross-edition black-box API suite (`node --test`; `run-go.sh`, `run-worker.sh`) and the Playwright browser suite |
| `docs/` | User and operator documentation; `docs_test.go` checks links, make targets and config keys, `contract_test.go` statements tied to code (key labels, site URLs, limits, MCP tools) |

## Invariants

1. **SPEC.md is normative.** If code and spec disagree, fix one deliberately and in the same
   change — never silently. Contract changes (URLs, site API, websocket, gateway protocol, SDK
   surface, config schema) update the spec first.
2. **Both editions stay in sync.** The Go server and `deploy/cloudflare` implement the same
   public contract; a contract change lands in both, plus `conformance/` (and
   `internal/docstore/conformance/querycases.json` for query semantics, which both run).
3. **Query semantics are type-strict and identical on every backend** (sqlite, postgres,
   dynamodb, memory, Worker): `!=`/`not-in` never match another type, missing fields never
   match, fixed cross-type sort rank, microsecond UTC timestamps. `internal/docstore/eval.go` is
   the reference.
4. **The SDK is dependency-free and served byte-identical** by both editions (plus the
   per-request preamble). Run `make sdk` after editing `web/sdk/src` and commit the outputs.
5. **Agent docs have one source:** `skills/backyard/**`. `skills/skills_test.go` checks links,
   anchors, the SDK surface and recipe scripts; `templates/` extracts the cheat sheet from
   `SKILL.md`.
6. **Config is strict:** unknown keys are errors. New keys need a default in `ApplyDefaults`,
   validation, an entry in `example.yaml` when user-facing, and a row in
   `docs/configuration.md`.
7. **Keys derive from `server.secret` per purpose** (`internal/keys`, HKDF-SHA256, salt
   `backyard`, info `backyard/<purpose>/v1`) — never reuse a key across purposes.
8. **Security defaults stay on:** CSRF header + Origin checks, `trusted_proxies` gating,
   sandboxed uploaded HTML/SVG, SSRF-guarded fetches, rate limits. Don't weaken them for
   convenience.
9. **Docs describe only what exists.** Every command, flag, key, endpoint and default in
   `docs/` and `skills/` must match the code (check with `go run ./cmd/backyard <cmd> --help`
   and `go run ./cmd/backyard config example`).

## Before committing

```sh
make fmt lint                 # gofmt, go vet, golangci-lint
make test                     # go test -race ./... (integration tests skip without services)
make sdk-test                 # if web/ changed
make cloudflare               # if deploy/cloudflare or the shared SDK/skill changed
go test ./skills/... ./templates/...   # if skills/ or templates/ changed
go test ./docs/               # if docs/, a top-level guide, a config key or a documented contract changed
make validate-configs tf-validate helm-lint   # if deploy/ changed
make conformance              # if the public contract changed (runs both editions)
make e2e                      # if the hub, SDK, examples, templates or sign-in flows changed
```

With the integration services (`make services-up`, see `CONTRIBUTING.md`), `make test` also
runs the Postgres, Redis, DynamoDB and Azurite tests. Record user-visible changes in
`CHANGELOG.md` under the topmost (unreleased) version.

## Handy commands

```sh
go run ./cmd/backyard serve --dev                 # hub on :8080 (PORT=… to move it)
go run ./cmd/backyard dev --no-open examples/live-poll   # http://live-poll.localhost:4321/
go run ./cmd/backyard config validate deploy/systemd/backyard.yaml
go run ./cmd/backyard --help                      # source of truth for docs/cli.md
```

Local test data lands in `./.backyard-data` (ignored by git). When running servers in the
background, use a free port and stop them by PID.
