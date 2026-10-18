# Backyard documentation

Backyard turns a folder of HTML into an internal URL with a zero-config backend. Start with the
[getting started guide](getting-started.md); the [design specification](spec/SPEC.md) is the
normative reference for every URL, wire format and default.

## Use Backyard

| Guide | What it covers |
|---|---|
| [Getting started](getting-started.md) | Install, run a local server, build and deploy a first site |
| [Client API](client-api.md) | `backyard.db`, `files`, `ai`, `warehouse`, `socket`, `id` at a glance |
| [Realtime](realtime.md) | Live queries, change feeds, rooms and presence, the websocket protocol |
| [Deploying sites](deploying-sites.md) | Site previews on the hub, deploy methods, `.backyardignore`, ownership, CI, the Lambda gateway |
| [CLI reference](cli.md) | Every `backyard` command and flag |
| [Agents](agents.md) | Claude skill, Claude Code plugin, MCP server, init templates, `/llms.txt` |
| [Claude connector](claude-connector.md) | Publish from Claude with the company sign-in: turning on the remote MCP connector, adding it in Claude, proxies, limits, disconnecting |

## Run Backyard

| Guide | What it covers |
|---|---|
| [Configuration](configuration.md) | Every `backyard.yaml` key with type, default and description |
| [Authentication](authentication.md) | OIDC (Okta, Entra ID, Google…), IAP, ALB, header, Cloudflare Access, tokens, CLI login |
| [Storage and databases](storage-and-databases.md) | fs / S3 / GCS / Azure, SQLite / Postgres / DynamoDB, pub/sub |
| [AI providers](ai-providers.md) | Anthropic, OpenAI-compatible, Ollama, Azure OpenAI, Bedrock, Gemini / Vertex, aliases |
| [Warehouse](warehouse.md) | BigQuery, Postgres / Redshift, Athena, named queries, read-only roles |
| [Security](security.md) | Threat model and hardening |
| [Operations](operations.md) | Metrics, health checks, logs and audit, backups, scaling, upgrades |
| [Cloudflare edition](cloudflare.md) | Backyard on Cloudflare's free tier |
| [Architecture](architecture.md) | Components, request flow, data layout |

Deployment kits live next to the code: [AWS](../deploy/terraform/aws/README.md),
[Google Cloud](../deploy/terraform/gcp/README.md), [Azure](../deploy/terraform/azure/README.md),
[Kubernetes (Helm)](../deploy/helm/backyard/README.md),
[Docker Compose](../deploy/docker-compose/README.md), [systemd](../deploy/systemd/README.md) and
[Cloudflare](../deploy/cloudflare/README.md).
