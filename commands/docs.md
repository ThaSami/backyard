---
description: Answer a question about Backyard (client API, realtime, AI, deploying, limits) from its documentation
argument-hint: "[question or topic, e.g. how do live queries work?]"
---

Answer this question about Backyard: $ARGUMENTS

Use the **backyard** skill: read `SKILL.md`, then the reference that covers the topic:

- `references/api.md`: the client API (`backyard.db`, `files`, `ai`, `warehouse`, `socket`,
  `id`), query operators and semantics, the HTTP and websocket protocol, errors and limits;
- `references/recipes.md`: complete example apps;
- `references/deploy.md`: the CLI, login per authentication mode, deploy methods,
  `.backyard.json` / `.backyardignore`, host vs path mode, the Cloudflare free-tier edition, CI.

If the skill is not available, call the `backyard_docs` MCP tool with the topic `skill`, `api`,
`recipes` or `deploy`, or read `<hub URL>/llms-full.txt`.

Answer concisely with exact method names and a short, correct code example. With no question,
give a ten-line overview of Backyard and its APIs and point to the references. Never invent API
surface: if the documentation does not cover something, say so.
