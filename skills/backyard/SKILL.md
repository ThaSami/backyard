---
name: backyard
description: Build, host and share internal web pages on Backyard, the company's zero-config hosting platform where a folder of HTML becomes an internal URL with a built-in backend. Use whenever the user wants to build, host, publish, deploy or share an internal web page, site, dashboard, tool, prototype, game, poll, form or app, or mentions Backyard, backyard.js, /__backyard/, backyard init, backyard dev or backyard deploy. Covers the browser API (realtime database, file uploads, AI chat/streaming/images/embeddings, data warehouse queries, websocket rooms with presence, identity), the CLI and the deploy workflow.
---

# Backyard

Backyard turns a **folder of static files** into an internal URL that only people in the
company can open (`https://<site>.backyard.example.com/`, or
`https://backyard.example.com/s/<site>/` on path-mode servers). Every site gets a zero-config
backend through one script tag — `backyard.db` (JSON documents + realtime), `backyard.files`
(uploads), `backyard.ai` (LLMs, images, embeddings), `backyard.warehouse` (read-only SQL),
`backyard.socket` (rooms with presence) and `backyard.id` (who is viewing). Sign-in is handled
by the company identity provider before the page loads. There is nothing else: no server code,
no cron, no config files — saying no is a feature.

The hub (`https://backyard.example.com`) lists every site and serves these docs. Throughout these
docs, `https://backyard.example.com` stands for the company's Backyard server; `backyard whoami`
(or the project's `CLAUDE.md` / `AGENTS.md`) shows its real URL — use that one with people.

## Golden rules

1. **A site is a folder of static files.** `index.html` is the entry point; any HTML, CSS, JS,
   images, fonts, JSON or WASM next to it is served as-is. A build step is fine — deploy its
   output folder (`backyard deploy dist`). Clean URLs work (`/about` serves `about.html`);
   `200.html` is an SPA fallback, `404.html` a custom not-found page.
2. **Include the SDK with an absolute path, before your code:**
   `<script src="/__backyard/backyard.js"></script>` (classic, defines `window.backyard`) or
   `import backyard from "/__backyard/backyard.mjs"`. Reference **your own** files with
   **relative** URLs (`app.js`, `img/logo.png`), never `/app.js` — root-absolute paths break
   on path-mode servers.
3. **No server code.** No Node/Python/PHP backends, API routes, cron jobs or databases of your
   own. Use the built-in APIs; if something truly needs a backend, tell the user it is out of
   scope for Backyard.
4. **Never put secrets in the client.** No API keys, tokens or passwords in any file: everything
   you deploy is readable by every employee. AI and warehouse credentials live on the server;
   the page never needs a key.
5. **All data of a site is shared by all its users.** Anyone who can open the site can read and
   write its collections, files and rooms; there are no per-document permissions. Use
   `_meta.created_by` to show "mine" or to hide edit buttons, but treat it as UX, not security.
   Do not store confidential data unless the whole company may see it.
6. **Identity is free.** `await backyard.me()` returns the signed-in person (name, email,
   avatar…). Never build login forms or ask for names.
7. **Deploy = overwrite.** `backyard deploy` replaces the whole site with the folder (files
   missing locally are deleted). Data (db, uploads) survives deploys. Pick a distinctive site
   name and confirm with the user before deploying over a site you did not create.
8. **Prefer realtime.** Render lists with `collection.watch(...)` so every open tab updates
   live; use rooms for presence and high-frequency signals (cursors, typing).
9. **Self-contained pages.** Vanilla HTML/CSS/JS works best. Vendor libraries into the folder
   instead of loading them from public CDNs (corporate networks may block them). Handle loading,
   empty and error states, and make it look good on laptop and phone.

## Quickstart

```sh
backyard init lunch-vote      # ./lunch-vote: index.html, .backyard.json, CLAUDE.md, AGENTS.md, skill
cd lunch-vote
backyard dev                  # http://lunch-vote.localhost:4321/ with a local backend (edits show on reload)
backyard deploy               # publishes the folder and prints the URL
```

- `backyard deploy` needs a login once: `backyard login --server https://backyard.example.com`
  opens the browser (company SSO). An agent cannot complete it — ask the user to run it.
- No Backyard server yet? `backyard cloudflare setup` deploys one to the user's own Cloudflare
  account on the **free plan** (needs a Cloudflare account and Node.js 20+; the user completes
  a browser login) and logs the CLI in, so `backyard deploy` works right after. See
  [references/deploy.md](references/deploy.md#cloudflare-edition-free-tier).
- The site name comes from `--site`, `.backyard.json` (`{"site": "lunch-vote"}`) or the folder
  name. Names: lowercase letters, digits and `-`, ≤ 63 chars, not reserved (`www`, `api`,
  `docs`, `admin`…).
- Useful: `backyard sites list [filter]`, `backyard sites info <site>`, `backyard open [site]`,
  `backyard pull <site> [dir]` (download a site to study or fork it), `backyard whoami`,
  `backyard deploy --dry-run`.
- With the MCP server (`backyard mcp`) the same is available as tools: `backyard_docs`,
  `backyard_init`, `backyard_deploy`, `backyard_list_sites`, `backyard_site_files`,
  `backyard_read_site_file`, `backyard_whoami`, `backyard_open_url`.
- `backyard dev` runs until stopped — agents start it in the background with `--no-open`
  (it opens a browser otherwise). AI and warehouse need
  `backyard dev --config backyard.yaml` with providers configured; otherwise those calls fail
  with `unsupported`, so code must degrade gracefully.

### Agent workflow

1. Restate the idea in one line; pick a site name; check it is free with
   `backyard sites info <name>` ("not found" = free) or `backyard_list_sites`.
2. `backyard init <name>` (or create the folder with `index.html` + `.backyard.json`).
3. Build the page(s) following the rules above; keep data models simple (see Patterns).
4. Preview with `backyard dev` when possible; fix console errors.
5. `backyard deploy` from the folder; give the user the printed URL. Redeploy after changes.

## Page skeleton

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Lunch Vote</title>                         <!-- shown in the hub directory -->
  <meta name="description" content="Vote where the team eats today">
  <link rel="stylesheet" href="style.css">           <!-- relative! -->
</head>
<body>
  <main id="app">Loading…</main>
  <script src="/__backyard/backyard.js"></script>    <!-- absolute! -->
  <script src="app.js"></script>
</body>
</html>
```

## API cheat sheet

All methods return Promises unless marked *sync*. Errors reject with a `BackyardError`
(`err.name === "BackyardError"`, `err.code`, `err.status`, `err.message`, `err.loginUrl?`).
On `401` the SDK sends the page to the login screen (`backyard.configure({autoLogin:false})`
to handle it yourself). Full details: [references/api.md](references/api.md).

```js
// Context & identity
backyard.site            // sync {name, url, apiBase, mode: "host"|"path"}
await backyard.me()      // {id, email, name, title?, team?, slack?, avatar_url?, groups, anonymous, admin, extra?}
await backyard.id.lookup("bob@acme.com")   // User | null

// Database: JSON documents per site; collections appear on first write
const todos = backyard.db.collection("todos")             // sync
const doc = await todos.create({ text: "Hi", done: false })  // {id, text, done, _meta:{created_at, updated_at, created_by, updated_by, version}}
await todos.create({ text: "x" }, { id: "custom-id" })     // rejects "conflict" if the id exists
await todos.get(id)                       // Doc | null
await todos.set(id, data)                 // create or replace
await todos.update(id, { done: true, note: null })  // shallow merge; null deletes a key; "not_found" if missing
await todos.delete(id)                    // ok even if missing
await todos.list({ limit: 50, offset: 0, orderBy: "_meta.created_at", order: "desc" })  // Doc[]
await todos.where("done", "==", false).where("tags", "contains", "ops")
           .orderBy("due", "asc").limit(20).offset(0).get()   // Doc[]; .first() → Doc|null
await todos.find({ where: [{ field: "done", op: "==", value: false }], order_by: [{ field: "due", dir: "asc" }], limit: 20 })
const stop = todos.watch(docs => render(docs), todos.where("done", "==", false))  // live query; stop()
const unsub = todos.subscribe({ onCreate(doc){}, onUpdate(doc){}, onDelete(id){}, onChange(ev){} })
await backyard.db.collections()           // string[]
// ops: == != < <= > >= in not-in (array ≤ 30) contains (array field has value)
// type-strict: "3" never matches 3, a missing field never matches, `!= null` matches nothing

// Files
const f = await backyard.files.upload(fileOrBlob, { name: "photo.png" })  // {id, name, url, size, content_type, created_at, created_by}
img.src = f.url                           // site-relative URL, works in <img>/<a>/<video>
await backyard.files.list({ limit: 50 })  // FileInfo[] newest first (.hasMore: more exist)
await backyard.files.get(id)              // FileInfo | null
await backyard.files.delete(id)

// AI (model = alias configured by admins; default when omitted)
const r = await backyard.ai.chat("Summarize: " + text, { system: "Be brief", model: "smart", maxTokens: 800, temperature: 0.2 })
r.content; r.model; r.stop_reason; r.usage   // stop_reason: end_turn|max_tokens|stop_sequence|refusal|other
const { json } = await backyard.ai.chat("Extract {name, date} as JSON from: " + text, { json: true })
const dataUrl = await new Promise((ok) => { const r = new FileReader(); r.onload = () => ok(r.result); r.readAsDataURL(fileOrBlob); })
await backyard.ai.chat([{ role: "user", content: [{ type: "text", text: "What is this?" }, { type: "image", url: dataUrl }] }])  // data: URL or public https URL, ≤ 20 MiB
const s = backyard.ai.stream(messages, { onText: t => (out.textContent += t) })
for await (const chunk of s) { /* chunk.text */ }   // either iterate or use onText
const final = await s.done                // {content, model, stop_reason, usage}
await backyard.ai.image("a watercolor fox", { size: "1024x1024", n: 1 })  // {id, url, content_type, model, images:[{id, url, content_type}]} — stored as site files
await backyard.ai.embed("text")           // number[]   (string[] → number[][])
await backyard.ai.models()                // {default, models:[{name, kind: "chat"|"image"|"embedding", provider, description}]}

// Warehouse (read-only analytics; may be unavailable → "unsupported")
await backyard.warehouse.named("weekly_active_users", { team: "growth" })  // {columns, rows, row_count, truncated}
await backyard.warehouse.query("SELECT day, n FROM dau WHERE team = ?", ["growth"], { source: "default" })
await backyard.warehouse.sources()        // {sources:[{name,type,description}], queries:[{name,description,params,source}]}

// Realtime rooms (one websocket per page, auto-reconnect, auto-rejoin)
const room = backyard.socket.room("lobby", { state: { status: "here" } })  // sync, joins automatically
room.on("message", (data, from) => {})    // from = {cid, user:{id, name, email, avatar_url}}
room.on("join" | "leave" | "update", member => {})   // member = {cid, user, state, joined_at}
room.on("members", members => {})         // everyone except you, after any presence change
room.members; room.self                   // current list (excluding self); own Member | null
room.send({ type: "wave" }, { to: cid, echo: false })  // sync; any JSON ≤ 64 KiB
room.setState({ x: 1 }); room.updateState({ y: 2 })    // sync; presence ≤ 8 KiB
room.off("message", handler); room.leave()
backyard.socket.connected; backyard.socket.on("open" | "close", handler)
```

## Which API for what

| Need | Use |
|---|---|
| Records people add/edit (votes, posts, tasks, scores) | `db.collection(...)` + `watch` to render |
| One record per person (RSVP, vote, settings) | doc id derived from `me.id` + `set()` |
| Who is here now, cursors, typing, "someone is editing" | room presence: `socket.room(name, {state})`, `members` |
| Fire-and-forget events (reactions, game moves, pings) | `room.send` (not stored) |
| Persistent chat | `db` messages + `watch`; room for presence/typing |
| Images, attachments, CSV exports | `files.upload` → store `url` in a db doc |
| Summaries, rewriting, classification, extraction | `ai.chat` (`json: true` for structured output) |
| Chat UI with typing effect | `ai.stream` |
| Pictures from a prompt | `ai.image` (stored as site files) |
| Similarity / semantic search on small sets | `ai.embed` + cosine similarity in JS |
| Company metrics, KPIs | `warehouse.named` (pre-approved query) or `warehouse.query` |
| Profile of a colleague | `id.lookup(email)` |
| Reuse UI/code from another site | `<script src>` / `<link>` with that site's absolute URL |

## Patterns

**Live list.** Render from `watch`; write with `create/update/delete`; never patch the DOM
by hand after writes — the watcher re-renders (debounced 50 ms) for every tab.

```js
const items = backyard.db.collection("items");
const stop = items.watch(render, items.orderBy("_meta.created_at", "desc").limit(100));
form.onsubmit = async (e) => { e.preventDefault(); await items.create({ text: input.value.trim() }); input.value = ""; };
function render(docs) { list.replaceChildren(...docs.map(d => Object.assign(document.createElement("li"), { textContent: d.text }))); }
```

**One doc per person.** Ids allow `A-Z a-z 0-9 _ - : .` only, so sanitize emails:
`const key = me.id.replace(/[^A-Za-z0-9_\-:.]/g, "_");` then `votes.set(key, { choice, name: me.name })`.

**Counters.** There is no atomic increment: store one doc per vote/like and count them
(`watch` the collection) instead of updating a shared counter.

**Presence / cursors.** Keep presence state tiny; throttle updates (≈ 20/s max):

```js
const room = backyard.socket.room("board", { state: { x: 0, y: 0 } });
let last = 0;
addEventListener("pointermove", (e) => {
  const now = performance.now(); if (now - last < 50) return; last = now;
  room.updateState({ x: e.clientX / innerWidth, y: e.clientY / innerHeight });
});
room.on("members", (members) => drawCursors(members)); // m.user.name, m.state.x, m.state.y
```

**AI streaming.**

```js
const out = document.querySelector("#answer"); out.textContent = "";
const stream = backyard.ai.stream([{ role: "user", content: question }], { system: "Answer concisely.", onText: (t) => (out.textContent += t) });
try { await stream.done; } catch (err) { out.textContent = err.code === "unsupported" ? "AI is not enabled here." : err.message; }
```

**Uploads.** `const f = await backyard.files.upload(input.files[0]); await photos.create({ url: f.url, file_id: f.id, name: f.name });`
Render `<img src="${f.url}">`. Delete with `files.delete(f.id)` and remove the doc.

**Warehouse dashboard.** Prefer named queries (admins define them in the server config); show a
clear message or demo data when the warehouse answers `unsupported`/`forbidden`/`not_found`.
Draw charts with inline SVG or `<canvas>` — no chart CDN needed.

**Sharing code between sites.** Any site can load another site's files by absolute URL
(`<script src="https://ui-kit.backyard.example.com/kit.js">`, or
`https://backyard.example.com/s/ui-kit/kit.js` in path mode) and embed other sites in
`<iframe>`s. Data APIs always act on the site the **page** runs on, so a shared library that
calls `backyard.db` reads and writes the including site's data. To study or fork a site:
`backyard pull <site>` (or the `backyard_site_files` / `backyard_read_site_file` MCP tools).

**Escaping.** Insert user content with `textContent` or escape it before using `innerHTML`.

## Limits (server defaults; admins may change them)

| What | Limit |
|---|---|
| Document size | 256 KiB of JSON |
| `list`/query `limit` | default 100, max 1000 (`hasMore`/`has_more` tells if more exist) |
| Documents per site | 100,000 |
| Upload size | 100 MiB per file |
| Site size | 500 MiB, 10,000 files, 100 MiB per file |
| Websocket | 64 KiB per message, 60 msgs/s (burst 120), 50 rooms + 100 subscriptions per tab, presence state 8 KiB |
| Rate limits per user | 600 API calls/min, 300 db writes/min, 20 AI calls/min, 2M AI tokens/day, 30 warehouse queries/min, 60 uploads/min, 60 deploys/h |
| Query `in`/`not-in` | ≤ 30 values; field paths ≤ 8 levels |

The Cloudflare free-tier edition has no warehouse (`unsupported`), uses Workers AI models, path-mode
URLs by default, smaller limits (25 MiB per file without R2, 200 MiB and 2,000 files per site)
and daily caps per user (e.g. 200 AI calls) that answer `rate_limited`. With its default
`AUTH_MODE=none`, sites are open to **anyone with the link** and every visitor is an anonymous
guest (`me.anonymous === true`): never rely on names or emails being real there (see
[references/deploy.md](references/deploy.md#cloudflare-edition-free-tier)).

## Troubleshooting

| Symptom | Cause → fix |
|---|---|
| Page redirects to a login screen / `401 unauthenticated` | Session expired or not signed in; the SDK redirects automatically. CLI: `backyard login`. |
| `403 forbidden` from your own `fetch()` | Every non-GET API call needs header `X-Backyard: 1` and a same-origin request — use the SDK instead of raw fetch. |
| `forbidden` on deploy | Ownership mode: only the site's owners/admins may overwrite it — choose another name. |
| `backyard is not defined` | The SDK `<script>` is missing, comes after your code, or uses a relative path. |
| Own assets 404 on `/s/<site>/` URLs | Root-absolute links (`/app.js`) — use relative ones (`app.js`). |
| `unsupported` (501) | Feature not configured on this server (AI, warehouse, realtime) or Cloudflare edition (warehouse). Degrade gracefully. |
| `rate_limited` (429) | Too many calls; wait `err.retryAfter` seconds. Debounce writes; use rooms for high-frequency data. |
| `conflict` on `create` | That id exists — use `set()` (replace) or `update()` (merge). |
| `too_large` | Doc > 256 KiB, upload > limit or websocket message > 64 KiB — split data or use `files`. |
| Query matches nothing | Types must match exactly (`"3"` ≠ `3`, also for `!=` and `not-in`); missing fields never match; `!= null` matches nothing; check field paths. |
| Other tabs don't update | Use `watch` (or `subscribe`) — `list`/`get` are one-shot reads. |
| AI/warehouse fail in `backyard dev` | Start with `backyard dev --config backyard.yaml` containing providers/sources. |

## References

- [references/api.md](references/api.md) — complete SDK reference, HTTP & websocket protocol,
  query semantics, errors and limits. Read before using an API in a non-trivial way.
- [references/recipes.md](references/recipes.md) — complete, copy-paste apps: realtime voting,
  live poll, multiplayer cursors, chat with presence, AI summarizer, image generator, file
  gallery, kanban, KPI dashboard, guestbook, game leaderboard.
- [references/deploy.md](references/deploy.md) — CLI, login per auth mode, deploy methods,
  `.backyard.json` / `.backyardignore`, host vs path mode, Cloudflare free-tier edition, CI.
- Hub docs: `https://backyard.example.com/docs`, `https://backyard.example.com/llms-full.txt`;
  TypeScript types at `/__backyard/backyard.d.ts` on any site.
