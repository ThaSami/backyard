# Getting started

This guide takes you from nothing to a deployed site with a realtime database, on your own
machine, in a few minutes. Afterwards, pick a [deployment kit](#run-it-for-your-company) to run
Backyard for your company.

## 1. Install the CLI

The `backyard` binary is the server, the CLI and the MCP server in one.

```sh
# Linux and macOS (verifies the release checksum; installs to /usr/local/bin or ~/.local/bin)
curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh

# With Go 1.26+
go install github.com/thasami/backyard/cmd/backyard@latest

# Container image (server)
docker run --rm -p 8080:8080 ghcr.io/thasami/backyard serve --dev
```

On Windows, download `backyard_<version>_windows_<arch>.zip` from the GitHub releases page.
Check the install with `backyard version`.

## 2. Run a local server

```sh
mkdir backyard-server && cd backyard-server
backyard serve --dev
```

`--dev` needs no configuration file: the hub runs at <http://localhost:8080/>, sites at
`http://<site>.localhost:8080/` (browsers resolve every `*.localhost` name to your machine),
there is no login (everyone is an anonymous guest), deploys need no token, and files plus an
SQLite database live in `./.backyard-data`. Set `PORT=9000` to use another port.

Open <http://localhost:8080/> — the hub lists sites, shows CLI setup and accepts drag-and-drop
deploys of a folder.

## 3. Create a site

In another terminal:

```sh
backyard init lunch-vote
cd lunch-vote
```

`backyard init` writes `index.html`, `.backyard.json` (the site name), `.backyardignore`, and
instructions for coding agents: `CLAUDE.md`, `AGENTS.md` and the Backyard skill in
`.claude/skills/backyard/`. Add `--agents claude,codex,cursor,copilot` for Cursor and Copilot
rules too, or `--template app` for a starter app with a realtime list.

## 4. Preview it with a local backend

```sh
backyard dev
```

This serves the folder live at <http://lunch-vote.localhost:4321/> with a complete local
backend (database, realtime, files) and opens the browser; edit a file and reload. Data is kept
in `./.backyard-data` inside the site folder (a dotfolder, so deploys skip it). AI and the
warehouse need providers: `backyard dev --config backyard.yaml` reads them from a server config
(see [AI providers](ai-providers.md)).

## 5. Use the client API

Every page can load the SDK from the absolute path `/__backyard/backyard.js`. Replace
`index.html` with a tiny realtime voting app:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Lunch vote</title>
</head>
<body>
  <h1>Where do we eat?</h1>
  <p id="who"></p>
  <button data-choice="tacos">Tacos</button> <button data-choice="ramen">Ramen</button>
  <ul id="votes"></ul>

  <script src="/__backyard/backyard.js"></script>
  <script type="module">
    const me = await backyard.me();                      // who is viewing (from your IdP)
    document.querySelector("#who").textContent = `Hi ${me.name}!`;

    const votes = backyard.db.collection("votes");
    const myId = me.id.replace(/[^A-Za-z0-9_\-:.]/g, "_"); // one vote per person

    for (const b of document.querySelectorAll("button")) {
      b.onclick = () => votes.set(myId, { choice: b.dataset.choice, name: me.name });
    }

    votes.watch((docs) => {                               // live: every open tab updates
      document.querySelector("#votes").replaceChildren(...docs.map((d) =>
        Object.assign(document.createElement("li"), { textContent: `${d.name}: ${d.choice}` })));
    });
  </script>
</body>
</html>
```

Open it in two browser windows and vote: both update instantly. The [client API](client-api.md)
covers files, AI, the warehouse and websocket rooms; [recipes](../skills/backyard/references/recipes.md)
has complete apps to copy.

## 6. Deploy

Point the CLI at your server once, then deploy the folder:

```sh
backyard login --server http://localhost:8080
backyard deploy
```

The dev server allows anonymous deploys, so `login` only stores the server URL. `deploy`
uploads what changed and prints the URL — <http://lunch-vote.localhost:8080/>. Deploying again
overwrites the site in place (files you deleted locally are removed); the data in its database
survives. `backyard deploy --dry-run` shows what would change.

Useful next commands: `backyard sites list`, `backyard open lunch-vote`,
`backyard pull lunch-vote` (download a site to study or fork it) and `backyard whoami`.

## Run it for your company

| Where | Kit |
|---|---|
| AWS (ECS Fargate, ALB, S3, DynamoDB or RDS, Bedrock, optional Lambda deploy gateway) | [deploy/terraform/aws](../deploy/terraform/aws/README.md) |
| Google Cloud (Cloud Run, IAP, GCS, Cloud SQL, Vertex AI, BigQuery) | [deploy/terraform/gcp](../deploy/terraform/gcp/README.md) |
| Azure (Container Apps, Front Door, Blob Storage, PostgreSQL, Entra ID, Azure OpenAI) | [deploy/terraform/azure](../deploy/terraform/azure/README.md) |
| Kubernetes | [deploy/helm/backyard](../deploy/helm/backyard/README.md) |
| One VM without a bucket (Docker Compose + Caddy) | [deploy/docker-compose](../deploy/docker-compose/README.md) |
| One VM without a bucket (systemd) | [deploy/systemd](../deploy/systemd/README.md) |
| Cloudflare free tier (no server at all) | [deploy/cloudflare](../deploy/cloudflare/README.md) — or just `backyard cloudflare setup --download` |

Every kit renders a `backyard.yaml`; [configuration](configuration.md) documents every key and
[authentication](authentication.md) walks through Okta, Entra ID, Google IAP, AWS ALB and the
other sign-in options. Before going live, read [security](security.md).

With a real server, people sign in once with `backyard login --server https://backyard.acme.com`
(the browser opens your company login) and deploy exactly as above.

## Let an agent build it

Coding agents can do all of the above: `backyard init` drops the instructions they need, and
`backyard mcp` gives any MCP client tools to read the docs, scaffold, deploy and browse other
sites. In Claude Code:

```text
/plugin marketplace add thasami/backyard
/plugin install backyard@backyard
/backyard:new a realtime lunch-spot voting page for my team
```

See [agents](agents.md).
