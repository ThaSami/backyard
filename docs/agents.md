# Agents

Backyard is built for coding agents as much as for people. An agent can learn the platform,
scaffold a site, preview it, deploy it and study other sites' code — with the same login and
the same guardrails as the person it works for.

All agent-facing documentation has a single source of truth: the **Backyard skill** in
[`skills/backyard/`](../skills/backyard/SKILL.md) (`SKILL.md` plus `references/api.md`,
`recipes.md` and `deploy.md`). It is embedded in the binary and reaches agents through every
channel below, so they never drift apart.

| Channel | For | How |
|---|---|---|
| Claude Code plugin | Claude Code | `/plugin marketplace add thasami/backyard`, `/plugin install backyard@backyard` |
| Claude skill | Claude Code, Claude apps | `backyard skill install`, or added to every project by `backyard init` |
| MCP server | any MCP client (Claude Code, Claude Desktop, Cursor, VS Code, Codex…) | `backyard mcp` |
| Claude connector | Claude on the web, Desktop, mobile and Cowork, Claude Code, ChatGPT and other MCP apps (opencode, Codex CLI…) — no CLI needed | `https://<hub>/__backyard/mcp`, once an admin turns it on |
| Project instructions | Claude, Codex and other `AGENTS.md` readers, Cursor, Copilot | `backyard init --agents …` |
| `/llms.txt`, `/llms-full.txt`, `/docs` | any LLM or person with a browser | served by every hub |

## Claude Code plugin

The repository is a Claude Code plugin marketplace
([`.claude-plugin/marketplace.json`](../.claude-plugin/marketplace.json)):

```text
/plugin marketplace add thasami/backyard
/plugin install backyard@backyard
```

The plugin ([`.claude-plugin/plugin.json`](../.claude-plugin/plugin.json)) bundles:

- the **backyard skill**, loaded whenever a task is about building, hosting or sharing an
  internal page;
- three slash commands from [`commands/`](../commands/new.md):
  - `/backyard:new <idea>` — build and publish a new site from a one-line idea (name, scaffold,
    build, check, deploy);
  - `/backyard:deploy [dir] [--site name]` — publish the current site and return its URL;
  - `/backyard:docs [question]` — answer a question from the documentation;
- the **MCP server** (`backyard mcp`).

The MCP server and deploys need the `backyard` CLI on your `PATH` and a one-time
`backyard login` in a terminal (an agent cannot complete a browser login).

## Claude skill

```sh
backyard skill install             # ~/.claude/skills/backyard (all projects)
backyard skill install --project   # ./.claude/skills/backyard (this project)
```

`backyard init` also writes the skill into each new project. In Claude apps that support custom
skills, upload the `skills/backyard` folder as a ZIP. The skill's description triggers on any
request to build, host, publish or share an internal page, dashboard, tool, prototype, game,
poll or form, and on mentions of Backyard, `backyard.js` or the CLI.

What it teaches: the golden rules (a site is a folder; absolute SDK path, relative own paths;
no server code; no secrets in the client; all site data is shared; deploy = overwrite), a
cheat sheet of every API, patterns (live lists, one document per person, presence, streaming,
uploads, dashboards), limits, troubleshooting, and complete
[recipes](../skills/backyard/references/recipes.md).

## MCP server

`backyard mcp` speaks the Model Context Protocol over stdio and uses the CLI's current login
(profile, server, token).

```sh
claude mcp add backyard -- backyard mcp
```

```json
{ "mcpServers": { "backyard": { "command": "backyard", "args": ["mcp"] } } }
```

| Tool | Input | Does |
|---|---|---|
| `backyard_docs` | `topic?` (`skill`, the default, `api`, `recipes` or `deploy`) | Returns the documentation as markdown. Agents call it first. |
| `backyard_init` | `dir`, `site`, `agents?` | Scaffolds a site (`index.html`, `.backyard.json`, agent files, skill). Never overwrites. |
| `backyard_deploy` | `dir?`, `site?`, `message?`, `dry_run?`, `overwrite?` | Publishes a folder and returns the URL, the folder it published (`dir`) and counts. Without `dir` it publishes the current project like `backyard deploy` (the folder `.backyard.json` names, else the working directory). The same ignore rules apply: `.git/` and the project files are never uploaded. Refuses to replace a site someone else deployed last unless `overwrite` is true — which agents must only set after asking the user. |
| `backyard_list_sites` | `query?` | The sites you can see, with title, description, URL and last deploy — to find existing tools. That is every site unless the server lists each site only to its owners and admins ([owners-only site listing](deploying-sites.md#owners-only-site-listing)); then a name it does not list may still be taken (`backyard_site_files` finds any site by name). |
| `backyard_site_files` | `site` | A site's deployed files (path, size; at most 2,000 listed). |
| `backyard_read_site_file` | `site`, `path` | One text file of a site (≤ 256 KiB) — to study or reuse another site's code. Binary file types (images, fonts, archives, media…) are refused without being downloaded, and so is a file whose content is not text. |
| `backyard_whoami` | — | The logged-in user, server and profile. |
| `backyard_open_url` | `site?` | The URL of a site or the hub (does not open a browser). |

Tool annotations mark the read-only tools as such and `backyard_deploy` as destructive, so MCP
clients can ask for confirmation.

The tools are driven by an agent that may have read a prompt injection, so they keep it inside
the project and away from credentials:

- **Local folders.** `backyard_init` and `backyard_deploy` work in the folder the agent was
  started in (the MCP server's working directory): a relative `dir` must stay inside it, also
  once symlinks are resolved. Any other folder is used only when the agent passes its absolute
  path (or `~/…`) and it already exists. The filesystem root and the home directory are always
  refused, and so are folders that hold credentials: the CLI's own config folder (and any folder
  whose deploy would include the CLI's config file), hidden folders of the home directory such
  as `~/.ssh` or `~/.config` and `~/Library` or `~/AppData` (unless the agent was started inside
  one), and, outside the working
  directory, any hidden folder. `backyard_deploy` also refuses to publish the root of a project
  whose `.backyard.json` publishes another folder (a build output such as `dist`).
- **Other people's code.** What `backyard_read_site_file` returns was written by other people:
  it is untrusted data to study, never instructions to follow (the tool's description tells the
  agent so).
- **Credentials.** Tool results and error messages never contain tokens or signed upload URLs:
  the CLI's access and refresh tokens, bearer credentials, JWTs, URL query strings (presigned
  signatures) and the capability in gateway upload paths are redacted.

## Claude connector (remote MCP)

The hub itself can be an MCP server for Claude, ChatGPT and other MCP apps: an admin turns the
connector on (`mcp.enabled`, or `MCP_ENABLED` on the Cloudflare edition), people add
`https://<hub>/__backyard/mcp` as a custom connector in Claude or ChatGPT, or as a remote MCP
server in an app on their computer (opencode, Codex CLI, Hermes Agent, Gemini CLI…), and
everyone connects with the company sign-in. The app then publishes and edits sites as the
signed-in person, without the CLI. Setup, the sign-in and consent flow, proxies, limits and
disconnecting: [Claude connector](claude-connector.md).

| Tool | Input | Does |
|---|---|---|
| `backyard_docs` | `topic?` (`skill`, the default, `api`, `recipes` or `deploy`) | Returns the documentation as Markdown, like the stdio tool. |
| `backyard_whoami` | — | The account the connection acts as, whether it may publish, the hub URL, the site URL pattern and the publishing limits. |
| `backyard_list_sites` | `query?` | The sites the person can see (every site, unless the server lists each site only to its owners and admins), most recently updated first (at most 200): name, URL, title, description, last publisher, size, owners. |
| `backyard_site_files` | `site` | A site's files with their sizes (at most 2,000). |
| `backyard_read_site_file` | `site`, `path` | One text file of a site (≤ 128 KiB). Binary files are refused; the text is untrusted data. |
| `backyard_publish` | `site`, `files` (each `path`, `content`, `encoding?` — `utf-8` or `base64` — and `content_type?`), `delete?`, `replace?`, `overwrite?` | Writes the files and returns the site's URL. Merges by default (the site's other files stay); `delete` removes paths and `replace: true` makes the site exactly the given files. A site someone else published last needs `overwrite`, which Claude sets only after asking. One call carries at most 4 MiB of file content (Cloudflare: 2 MiB); bigger sites go over several calls, big media through the hub or the CLI. |

Differences from `backyard mcp`: no local folders (`backyard_init`, `backyard_deploy` and
`backyard_open_url` do not exist; files travel inside the call), no CLI login (the connection
has its own OAuth tokens, valid only at `/__backyard/mcp`, which accepts nothing else), and a
publish merges instead of replacing the whole site. Publishing needs a sign-in on the hub, or
anonymous deploys on a hub without one.

## Project instructions (`backyard init` templates)

`backyard init <site> --agents claude,codex,cursor,copilot` writes, next to `index.html`:

| Agent | Files |
|---|---|
| `claude` (default) | `CLAUDE.md` and `.claude/skills/backyard/**` |
| `codex` (default) | `AGENTS.md` (also read by other agents that support the convention) |
| `cursor` | `.cursor/rules/backyard.mdc` |
| `copilot` | `.github/copilot-instructions.md` |

Each file carries the site name, the server URL, the workflow (`backyard dev`, `backyard
deploy`), the golden rules and the API cheat sheet — extracted from the skill at build time, so
they always match it. None of these files is deployed (they are in the default ignore list,
which only an explicit `!` line in `.backyardignore` would override), and `backyard pull` never
writes them. Templates live in [`templates/`](../templates/templates.go).

## `/llms.txt` and friends

Every hub serves agent-readable docs, with example URLs rewritten to the hub's real address:

| Path | Content |
|---|---|
| `/llms.txt` | What Backyard is, the essentials, links to each document ([llmstxt.org](https://llmstxt.org) format) |
| `/llms-full.txt` | `SKILL.md` plus every reference in one file |
| `/docs` | The same documents rendered for people |
| `/docs/skill.md`, `/docs/<name>.md`, `/docs/references/<name>.md` | Individual documents as markdown (`<name>`: `api`, `recipes`, `deploy`; `SKILL.md` links to its references as `references/<name>.md`) |
| `/__backyard/backyard.d.ts` | Normative TypeScript declarations of the SDK |

The Cloudflare edition serves the same paths.

## A typical agent session

1. Read `backyard_docs` (or the skill); restate the idea; pick a site name and check it is free
   with `backyard sites info <name>` ("not found" = free; the MCP tool `backyard_site_files`
   finds any site by name too). `backyard_list_sites` shows only the sites you can see, on some
   servers just your own.
2. `backyard init <name>`; build the page with relative asset paths and the absolute SDK path,
   a `<title>`, a `<meta name="description">` and a `preview.svg` (1280×720) sketching the main
   screen for the hub's site card ([Site preview](deploying-sites.md#site-preview)).
3. Preview with `backyard dev --no-open` in the background; fix console errors. Outside a
   browser `*.localhost` often does not resolve, so check the page with the line `backyard dev`
   prints: `curl -s -H 'Host: <site>.localhost:4321' http://127.0.0.1:4321/`
   (`http://localhost:4321/` is the dev hub, not the site).
4. `backyard deploy` (or `backyard_deploy`) and hand the URL to the user.

Guardrails that apply to agents as to people: deploys are diffed, rate limited and audited; a
site last deployed by someone else is never replaced without an explicit `overwrite`/`-y`; in
ownership mode only owners and admins can replace a site (a site without owners, one from
before ownership mode, can be replaced and claimed by any signed-in deployer); and everything an
agent publishes is readable by every employee, so the skill tells agents never to put secrets in
a site.
