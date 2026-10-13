# Backyard

**Drop a folder of HTML, get an internal URL — with a zero-config backend for every site.**

[![CI](https://github.com/thasami/backyard/actions/workflows/ci.yml/badge.svg)](https://github.com/thasami/backyard/actions/workflows/ci.yml)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Backyard is an open-source, enterprise-ready take on **Quick**, the internal hosting platform
Shopify described on its [engineering blog](https://shopify.engineering/quick): anyone in the
company copies a folder of HTML to a server and gets
`https://lunch-vote.backyard.acme.com/` — reachable only by colleagues, signed in through the
company identity provider. Every site can include one script and get a realtime database, file
uploads, AI, the data warehouse, websockets and the viewer's identity, with no backend code and
no secrets in the page. The free-tier Cloudflare edition is inspired by
[OpenQuick](https://github.com/topmass/openquick).

```html
<script src="/__backyard/backyard.js"></script>
<script type="module">
  const me = await backyard.me();                                   // who is viewing
  const votes = backyard.db.collection("votes");
  await votes.set(me.id.replace(/[^A-Za-z0-9_\-:.]/g, "_"), { choice: "tacos", name: me.name });
  votes.watch((docs) => render(docs));                               // live in every tab
</script>
```

## Why

Most internal tools are a page, a list, a button and a bit of AI. They die in the backlog
because each one needs a repo, a backend, a database, credentials, a deploy pipeline and a
security review. Backyard removes all of that:

- **A site is a folder.** No framework, no build step (a `dist/` folder is fine too), no
  config. Deploying is copying; overwriting is updating.
- **The backend is already there.** Database with realtime, files, AI, warehouse, websockets
  and identity, all scoped per site, all behind your company login.
- **Saying no is a feature.** No server code, no cron, no custom backends — a small, fixed set
  of capabilities that a platform team can secure once.
- **Agents can do it all.** Coding agents get the docs, a skill, an MCP server and templates,
  so "build me a lunch-vote page" ends with a URL.

## Features

- **One binary**: server, deploy CLI, local dev server and MCP server; a container image and
  Terraform/Helm/Compose/systemd kits.
- **Client SDK** (`/__backyard/backyard.js`): `backyard.db` (JSON documents, queries, change
  feeds, live queries), `backyard.files`, `backyard.ai` (chat, streaming, images, embeddings),
  `backyard.warehouse` (read-only SQL), `backyard.socket` (rooms with presence), `backyard.id`.
- **Hub**: a searchable directory of every site, drag-and-drop deploys, CLI setup, API docs at
  `/docs` and `/llms.txt` for agents.
- **Sign-in**: Okta / any OIDC provider, Google IAP, AWS ALB, trusted proxy headers (oauth2-proxy,
  Pomerium, Azure Easy Auth, Cloudflare Access), or none for labs; API tokens, static CI tokens
  and the OAuth device flow for CLIs.
- **Deploy any way**: Backyard's diffed upload API (no cloud account needed), `aws s3 sync`,
  `gcloud storage rsync`, `az storage blob sync` or plain `rsync`.
- **Pluggable everything**: storage (disk, S3, GCS, Azure Blob), database (SQLite, PostgreSQL,
  DynamoDB), realtime fan-out (in-memory, Postgres, Redis), any AI provider, BigQuery /
  Redshift / Postgres / Athena.
- **Enterprise-ready**: admin groups, allowed domains and groups, optional site ownership, rate
  limits and quotas, audit logs, Prometheus metrics, health checks, horizontal scaling.
- **Runs anywhere**: AWS, Google Cloud, Azure, Kubernetes, one VM without a bucket — or
  Cloudflare's free tier with no server at all.

## Quickstart (60 seconds)

```sh
curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh
# or: go install github.com/thasami/backyard/cmd/backyard@latest

backyard serve --dev          # terminal 1: hub at http://localhost:8080, no login, data in ./.backyard-data
```

```sh
backyard init lunch-vote && cd lunch-vote       # terminal 2: index.html, .backyard.json, CLAUDE.md, AGENTS.md, skill
backyard dev                                    # live preview with a local backend: http://lunch-vote.localhost:4321/
backyard login --server http://localhost:8080   # the dev server accepts anonymous deploys
backyard deploy                                 # → http://lunch-vote.localhost:8080/
```

Then open <http://localhost:8080/> to see the hub. Next: the
[getting started guide](docs/getting-started.md).

## The client API at a glance

```js
// Identity — from your IdP, no login forms
const me = await backyard.id.me();     // {id, email, name, title, team, avatar_url, groups, …}

// Database — JSON documents per site, type-strict queries, realtime
const todos = backyard.db.collection("todos");
await todos.create({ text: "Ship it", done: false });
const open = await todos.where("done", "==", false).orderBy("_meta.created_at", "desc").limit(20).get();
todos.subscribe({
  onCreate: (doc) => toast(`${doc._meta.created_by} added “${doc.text}”`),
  onUpdate: (doc) => rerender(doc),
  onDelete: (id) => remove(id),
});

// AI — the server holds the keys; pages pick an alias
const { content } = await backyard.ai.chat("Summarize this thread: " + text, { model: "fast" });
for await (const chunk of backyard.ai.stream("Write release notes for: " + changes)) out.textContent += chunk.text;

// Files and the warehouse
const file = await backyard.files.upload(input.files[0]);            // {id, url, name, size, …}
const { rows } = await backyard.warehouse.named("weekly_active_users", { team: "growth" });

// Websocket rooms — presence and messages
const room = backyard.socket.room("standup", { state: { name: me.name, speaking: false } });
room.on("members", (members) => renderPeople(members));
room.on("message", (data, from) => react(data.emoji, from.user.name));
room.send({ emoji: "🎉" });
```

Full reference: [docs/client-api.md](docs/client-api.md) and
[the API reference](skills/backyard/references/api.md); complete example apps in
[recipes](skills/backyard/references/recipes.md) and [examples/](examples/README.md).

## Architecture

```
                     ┌──────────── company IdP (Okta · Entra ID · Google · …) ────────────┐
                     │                                                                     │
browser ──HTTPS──▶ [ built-in OIDC │ Google IAP │ AWS ALB │ trusted proxy │ none ] ──▶ backyard serve
                                                                                        │
   hub:   https://backyard.acme.com/                      ├─ static sites ─────▶ storage   fs │ s3 │ gcs │ azure
   sites: https://<site>.backyard.acme.com/               ├─ backyard.db ──────▶ database  sqlite │ postgres │ dynamodb
          (or https://backyard.acme.com/s/<site>/)        ├─ backyard.socket ──▶ pub/sub   memory │ postgres │ redis
                                                          ├─ backyard.ai ──────▶ Anthropic · OpenAI-compatible · Azure OpenAI
                                                          │                       · Bedrock · Gemini / Vertex · Ollama
                                                          ├─ backyard.warehouse ▶ BigQuery · Postgres / Redshift · Athena
                                                          └─ deploy gateway (embedded)
CLI · CI · agents ──▶ deploy gateway: embedded │ `backyard gateway` │ AWS Lambda ──presigned PUT──▶ bucket
                └───▶ or direct: aws s3 sync · gcloud storage rsync · az storage blob sync · rsync
```

More in [docs/architecture.md](docs/architecture.md); the normative contract is
[docs/spec/SPEC.md](docs/spec/SPEC.md).

## Deploy it

| Where | What you get | Kit |
|---|---|---|
| **AWS** | ECS Fargate behind an ALB (built-in OIDC or ALB authentication), S3, DynamoDB or RDS PostgreSQL, Valkey pub/sub, Claude on Bedrock, optional Lambda deploy gateway | [deploy/terraform/aws](deploy/terraform/aws/README.md) |
| **Google Cloud** | Cloud Run behind a global load balancer with IAP (like Shopify's Quick), GCS, Cloud SQL, Gemini and Claude on Vertex AI, BigQuery | [deploy/terraform/gcp](deploy/terraform/gcp/README.md) |
| **Azure** | Container Apps behind Front Door, Blob Storage, PostgreSQL Flexible Server, Entra ID, keyless Azure OpenAI | [deploy/terraform/azure](deploy/terraform/azure/README.md) |
| **Kubernetes** | Helm chart: one replica on a volume with SQLite by default; production values with buckets, Postgres/DynamoDB, Redis, OIDC, HPA, PDB, ServiceMonitor | [deploy/helm/backyard](deploy/helm/backyard/README.md) |
| **One VM, no bucket — Docker Compose** | Backyard + PostgreSQL + Caddy (automatic TLS for the wildcard domain), files on a volume, rsync deploys | [deploy/docker-compose](deploy/docker-compose/README.md) |
| **One VM, no bucket — systemd** | One hardened binary, SQLite, files on disk, Caddy or nginx, rsync deploys | [deploy/systemd](deploy/systemd/README.md) |
| **Cloudflare free tier** | One Worker + Durable Objects + Workers AI, optional R2 and Cloudflare Access; no server, no credit card (`backyard cloudflare setup --download`) | [deploy/cloudflare](deploy/cloudflare/README.md), [Terraform](deploy/terraform/cloudflare/README.md) |

The container image is `ghcr.io/thasami/backyard`. Configuration is one `backyard.yaml`
([reference](docs/configuration.md); `backyard config example` prints an annotated one).

## Authentication

| Mode | How | Docs |
|---|---|---|
| `oidc` | Built-in login against Okta, Entra ID, Google Workspace, Auth0, Keycloak… (authorization code + PKCE); one session for the hub and every site | [Okta step by step](docs/authentication.md#okta-step-by-step) |
| `iap` | Google Cloud Identity-Aware Proxy assertions | [IAP](docs/authentication.md#iap-google-cloud-identity-aware-proxy) |
| `alb` | AWS ALB `authenticate-oidc` (e.g. with Okta), signed `X-Amzn-Oidc-Data` | [ALB](docs/authentication.md#alb-aws-alb-with-okta-or-any-oidc-idp) |
| `header` | Trusted proxy headers or a verified JWT: oauth2-proxy, Pomerium, Azure Easy Auth, Cloudflare Access | [header](docs/authentication.md#header-trusted-identity-headers) |
| Cloudflare Access | Native in the Workers edition (`AUTH_MODE=access`) | [Cloudflare](docs/cloudflare.md) |
| `none` | Anonymous guests, for laptops and labs | [none](docs/authentication.md#none) |

CLIs sign in through the browser (`backyard login`), with the OAuth device flow, or with a
token; CI uses static tokens whose SHA-256 is all the server stores.

## Deploy methods

| Method | For |
|---|---|
| `api` (default) | Everyone: diffed uploads through Backyard's deploy gateway — embedded in the server, standalone (`backyard gateway`), or an **AWS Lambda** function URL for people without cloud accounts (they `backyard login` through the browser first, then deploy with their token). |
| `s3` | `aws s3 sync` straight into the bucket |
| `gcs` | `gcloud storage rsync` (or `gsutil rsync`) |
| `azure` | `az storage blob sync` |
| `rsync` | `rsync` to a server with local-disk storage — no bucket at all |

`backyard deploy` picks automatically; see [deploying sites](docs/deploying-sites.md).

## Backends

| Capability | Options |
|---|---|
| Storage (site files, uploads) | Local disk / NFS / mounted buckets (`fs`), Amazon S3 and S3-compatible (MinIO, Cloudflare R2, Ceph, B2), Google Cloud Storage, Azure Blob Storage |
| Database (`backyard.db`) | SQLite (single node), PostgreSQL (RDS, Cloud SQL, Azure, Neon…), Amazon DynamoDB, in-memory |
| Realtime fan-out | In-memory (one instance), PostgreSQL LISTEN/NOTIFY, Redis / Valkey |
| AI | Anthropic (API, Vertex AI, Bedrock), OpenAI and compatible servers (vLLM, LiteLLM, OpenRouter, Workers AI, AI Gateway), Ollama, Azure OpenAI, Amazon Bedrock, Google Gemini (AI Studio, Vertex AI); Workers AI on the Cloudflare edition |
| Warehouse | BigQuery, PostgreSQL-compatible (Redshift, AlloyDB, Aurora…), Amazon Athena |

Guides: [storage and databases](docs/storage-and-databases.md), [AI providers](docs/ai-providers.md),
[warehouse](docs/warehouse.md), [realtime](docs/realtime.md).

## Agent-native

- **Claude skill** — [`skills/backyard`](skills/backyard/SKILL.md): rules, API cheat sheet,
  recipes and deploy guide. `backyard skill install`, and added to every project by `backyard init`.
- **Claude Code plugin** — skill, `/backyard:new`, `/backyard:deploy`, `/backyard:docs` and the MCP
  server:

  ```text
  /plugin marketplace add thasami/backyard
  /plugin install backyard@backyard
  ```

- **MCP server** — `backyard mcp` (e.g. `claude mcp add backyard -- backyard mcp`): tools to read
  the docs, scaffold, deploy, list sites and read other sites' code, with the CLI's login.
- **Init templates** — `backyard init --agents claude,codex,cursor,copilot` writes `CLAUDE.md`,
  `AGENTS.md`, `.cursor/rules/backyard.mdc` and `.github/copilot-instructions.md`.
- **`/llms.txt`, `/llms-full.txt`, `/docs`** on every hub.

See [docs/agents.md](docs/agents.md).

## Security model

Backyard trusts **everyone your IdP signs in** — that is the point — and nobody else:

- every request is authenticated; outsiders see nothing, and `allowed_domains` /
  `allowed_groups` narrow access further;
- inside the bubble there are no per-site permissions: anyone may read and write any site's
  data, so sites must not hold data not every employee may see;
- in host mode each site is its own browser origin, separated from other sites and the hub by
  a CSRF header, Origin checks and websocket origin checks (path mode shares one origin — a
  documented trade-off);
- secrets stay server-side; all keys derive from one `server.secret` with HKDF; tokens are
  short-lived JWTs or hashed static tokens;
- rate limits, quotas, upload limits, read-only warehouse roles, sandboxed uploaded HTML/SVG,
  SSRF-guarded fetching and audit logs bound the damage of a runaway page.

Read [docs/security.md](docs/security.md) before going to production; report vulnerabilities
privately as described in [SECURITY.md](SECURITY.md).

## Documentation

[Getting started](docs/getting-started.md) · [Configuration](docs/configuration.md) ·
[Authentication](docs/authentication.md) · [Deploying sites](docs/deploying-sites.md) ·
[Storage and databases](docs/storage-and-databases.md) · [AI providers](docs/ai-providers.md) ·
[Warehouse](docs/warehouse.md) · [Realtime](docs/realtime.md) · [Client API](docs/client-api.md) ·
[CLI](docs/cli.md) · [Agents](docs/agents.md) · [Security](docs/security.md) ·
[Operations](docs/operations.md) · [Cloudflare](docs/cloudflare.md) ·
[Architecture](docs/architecture.md) · [Specification](docs/spec/SPEC.md)

## Project layout

```
cmd/backyard/            the binary: server, CLI, MCP server
cmd/backyard-lambda/     the deploy gateway as an AWS Lambda function
internal/                server, auth, docstore, blob, pubsub, realtime, ai, warehouse, deploy, cli, …
web/sdk/                 the client SDK (backyard.js / .mjs / .d.ts) served to every site
web/hub/                 the hub UI
skills/backyard/         the Claude skill: the single source of agent-facing docs
templates/               files written by `backyard init`
commands/, .claude-plugin/  the Claude Code plugin
deploy/                  Terraform (aws, gcp, azure, cloudflare), Helm, Docker Compose, systemd,
                         and the Cloudflare Workers edition (deploy/cloudflare)
examples/                complete example sites
docs/                    documentation and the design specification (docs/spec/SPEC.md)
scripts/                 install script and CI tooling
```

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md) for the development setup,
tests (including the integration services and the cross-edition conformance suite) and
guidelines. Please follow the [code of conduct](CODE_OF_CONDUCT.md).

## License

[Apache License 2.0](LICENSE). Backyard is an independent project and is not affiliated with
Shopify or Cloudflare.
