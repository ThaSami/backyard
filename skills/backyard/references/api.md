# Backyard API reference

Complete reference of the browser SDK (`window.backyard`) and of the HTTP and websocket API
underneath it. Everything is **scoped to the site the page runs on**: collections, files and
rooms of one site are invisible to other sites. The hub in these examples is
`https://backyard.example.com`; a site `foo` lives at `https://foo.backyard.example.com/`
(host mode) or `https://backyard.example.com/s/foo/` (path mode).

Contents: [Loading the SDK](#loading-the-sdk) · [Conventions](#conventions) ·
[Top level](#top-level) · [Identity](#identity) · [Database](#database) · [Files](#files) ·
[AI](#ai) · [Warehouse](#warehouse) · [Realtime rooms](#realtime-rooms) · [Errors](#errors) ·
[Limits](#limits) · [HTTP API](#http-api) · [Websocket protocol](#websocket-protocol) ·
[Hub endpoints](#hub-endpoints)

## Loading the SDK

```html
<!-- classic script: defines window.backyard -->
<script src="/__backyard/backyard.js"></script>

<!-- or as an ES module -->
<script type="module">
  import backyard from "/__backyard/backyard.mjs";
  const me = await backyard.me();
</script>
```

- Always use the **absolute** path `/__backyard/backyard.js`; it works in host and path mode.
  Reference your own files with relative paths (`app.js`), never `/app.js`.
- TypeScript declarations: `/__backyard/backyard.d.ts` (download it next to your code and add
  `/// <reference path="backyard.d.ts" />` for editor completion).
- No build step, no npm package, no keys. The SDK holds no secrets.
- The server prepends a context preamble to the SDK
  (`globalThis.__BACKYARD__ = {"v":1,"mode":"host","site":"foo","prefix":"/s/","hub":"https://backyard.example.com"}`).
  The API base is resolved as: `window.BACKYARD_CONFIG.apiBase` if set before the script loads
  (escape hatch) → host mode: `<page origin>/__backyard` → path mode:
  `<origin>/s/<site>/__backyard` (site from the preamble or from `location.pathname`), or
  `<origin>/__backyard` on the hub.

## Conventions

- Every method returns a **Promise** unless marked *sync*.
- Failures reject with a **`BackyardError`**: `{ name: "BackyardError", code, status, message,
  loginUrl? }` (also `retryAfter` seconds on `429`). `code` is one of the
  [error codes](#errors); `status` is the HTTP status (`0` for client-side failures). The class
  is available as `backyard.BackyardError` for `instanceof` checks.
- On `401` with a login URL, the SDK **redirects the top-level page** to the login page and
  comes back afterwards. Disable with `backyard.configure({ autoLogin: false })` and handle
  `err.code === "unauthenticated"` / `err.loginUrl` yourself.
- The SDK sends the CSRF header `X-Backyard: 1` on every request and uses the browser session
  cookie (or the identity-aware proxy in front of Backyard).
- Timestamps are RFC 3339 strings in UTC. Document timestamps (`_meta.created_at`,
  `_meta.updated_at`, `FileInfo.created_at`) have microsecond precision with trailing zeros
  dropped (`"2026-03-02T17:04:05.1234Z"`), so compare them with `new Date(…)` or let queries
  order them — not with string comparison. Presence `joined_at` always has exactly three
  fractional digits (`"2026-03-02T17:04:05.120Z"`).

```js
try {
  await backyard.db.collection("slugs").create({ title }, { id: slug });
} catch (err) {
  if (err.name === "BackyardError" && err.code === "conflict") showError("That slug is taken");
  else throw err;
}
```

## Top level

| Member | Description |
|---|---|
| `backyard.ready` | `Promise<void>`, resolves when the context is resolved (immediately). |
| `backyard.site` | *sync* `{ name: string \| null, url: string, apiBase: string, mode: "host" \| "path" }` — `name` is `null` on the hub; `url` is the absolute site root. |
| `backyard.configure(opts)` | *sync* `opts: { apiBase?: string, autoLogin?: boolean }`. `apiBase` overrides the API base (`null`/`""` restores the default); `autoLogin` (default `true`) controls the 401 redirect. |
| `backyard.me()` | Shorthand for `backyard.id.me()`. |
| `backyard.version` | SDK version string. |
| `backyard.BackyardError` | The error class. |
| `backyard.id`, `.db`, `.files`, `.ai`, `.warehouse`, `.socket` | The namespaces below. |

The ES module also exports the namespaces by name:
`import backyard, { db, files, ai, warehouse, socket, id, site, me } from "/__backyard/backyard.mjs"`.

## Identity

```ts
interface User {
  id: string;            // stable: lowercased email, else "sub:<issuer-sub>", else "anon:<hex>" (guests)
  email?: string;
  name: string;          // "Guest 1a2b" for anonymous users
  title?: string;        // job title (IdP claim or company directory)
  team?: string;
  slack?: string;        // "@alice"
  avatar_url?: string;
  groups?: string[];
  anonymous: boolean;    // true when the server runs without login (auth.mode: none)
  admin: boolean;        // Backyard administrator
  extra?: Record<string, any>;  // extra mapped claims, e.g. { location: "Ottawa" }
}
interface UserSummary { id: string; name: string; email?: string; avatar_url?: string }
```

| Method | Returns |
|---|---|
| `backyard.id.me()` | `User` — the signed-in person; cached after the first successful call. |
| `backyard.id.lookup(email)` | `User \| null` — a person who has used Backyard (or is in the company directory); `null` when unknown. |

Use `avatar_url` when present, else render initials. Ids contain `@`, which is not allowed in
document ids — sanitize before using them as ids (see [Database](#database)).

## Database

A per-site JSON document store with queries and realtime change feeds. Collections are created
implicitly by the first write.

- **Collection names:** `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` (names starting with `_` are reserved).
- **Document ids:** `^[A-Za-z0-9_\-:.]{1,128}$`. Generated ids are 26-character ULIDs, which
  sort by creation time. Derive ids from emails with
  `email.replace(/[^A-Za-z0-9_\-:.]/g, "_")`.
- **Document shape (flattened):** your data plus `id` and read-only `_meta`:

```json
{ "id": "01JNV6Q3W7Y9XG1Q5S7T2K4M8P", "title": "Hello", "status": "draft",
  "_meta": { "created_at": "2026-03-02T17:04:05.123Z", "updated_at": "2026-03-02T17:04:05.123Z",
             "created_by": "alice@acme.com", "updated_by": "alice@acme.com", "version": 1 } }
```

- On writes, `id` and `_meta` keys in the data are ignored (not stored). Data must be a JSON
  object of at most 256 KiB (`too_large` otherwise, also when the site reached its document
  quota). `_meta.version` starts at 1 and increments on every write.

### Collection

```js
const posts = backyard.db.collection("posts");   // sync; nothing is created until the first write
```

| Method | Returns | Notes |
|---|---|---|
| `posts.create(data, { id? })` | `Doc` | Generated ULID unless `id` is given; rejects `conflict` if the id exists. |
| `posts.get(id)` | `Doc \| null` | |
| `posts.set(id, data)` | `Doc` | Create or **replace** (PUT); version + 1. |
| `posts.update(id, patch)` | `Doc` | **Shallow** merge; a `null` value **deletes** that key; rejects `not_found` if missing. Nested objects are replaced, not merged. |
| `posts.delete(id)` | `void` | Succeeds when the document does not exist. |
| `posts.list({ limit?, offset?, orderBy?, order? })` | `Doc[]` | Default: oldest first, 100 per page. `orderBy` is a field (or array of fields), `order` `"asc"` (default) or `"desc"`; `{ order: "desc" }` alone = newest first. |
| `posts.where(field, op, value)` | `Query` | Starts a chainable query (also `posts.orderBy(...)`, `.limit(n)`, `.offset(n)`). |
| `posts.find(queryObject)` | `Doc[]` | Runs a query given as a wire object (below) or a `Query`. |
| `posts.subscribe(handlers)` | `unsubscribe()` | *sync*. Realtime change events (below). |
| `posts.watch(callback, query?)` | `stop()` | *sync*. Live query (below). |
| `backyard.db.collections()` | `string[]` | Names of this site's collections. |

Arrays returned by `list`, `find` and `Query.get` carry a non-enumerable `hasMore` boolean:
`true` when more documents match after this page. A page can hold **fewer** than `limit`
documents and still have `hasMore` (the Cloudflare edition ends a page at about 8 MiB of
documents), so advance `offset` by the number of documents received, not by `limit`:

```js
const all = [];
for (let page; !page || page.hasMore; ) {
  page = await posts.list({ limit: 500, offset: all.length });
  all.push(...page);
}
```

### Queries

```js
const open = await posts
  .where("status", "==", "open")            // all filters must match (AND)
  .where("votes", ">=", 3)
  .orderBy("votes", "desc")                 // several orderBy calls = several sort keys
  .limit(20).offset(0)
  .get();                                   // Doc[]
const top = await posts.orderBy("votes", "desc").first();   // Doc | null
```

Queries are immutable: every builder call returns a new `Query`, so a base query can be
reused. The same query as a wire object (for `find`, and the HTTP API):

```json
{ "where": [ {"field": "status", "op": "==", "value": "open"},
             {"field": "votes",  "op": ">=", "value": 3} ],
  "order_by": [ {"field": "votes", "dir": "desc"} ],
  "limit": 20, "offset": 0 }
```

**Fields** are dot paths into your data (`"author.name"`); each segment matches
`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`, at most 8 levels. System fields: `id`, `_meta.created_at`,
`_meta.updated_at` (compared as times: pass RFC 3339 strings), `_meta.created_by`,
`_meta.updated_by` and `_meta.version` (a number).

**Operators**

| Op | Matches when |
|---|---|
| `==` | field equals value (`== null` matches an explicit `null` only) |
| `!=` | field exists, has the **same type** as value and differs from it — `!= null` matches nothing, `!= 2` skips `"2"` |
| `<` `<=` `>` `>=` | field and value have the same type and compare accordingly |
| `in` | field equals one of the values (value = array of ≤ 30 scalars) |
| `not-in` | field exists and is `!=` every value (so it has their type; ≤ 30 scalars) |
| `contains` | field is an array that contains the scalar value |

**Semantics**

- Values are JSON scalars: string, number, boolean or null.
- Comparisons only match values of the **same JSON type**, for every operator: `"3"` never
  equals `3`, `true` never equals `1`, `<` on a string field with a number value matches
  nothing, and `!=` / `not-in` skip values of another type.
- A **missing field never matches** any operator (including `!=` and `not-in`). To find
  documents without a field, store an explicit `null` (or `false`) and query `== null`.
- Ordering within a type: numbers numerically, strings by byte order (case-sensitive),
  `false < true`. Across types: missing/`null` < booleans < numbers < strings < arrays <
  objects, reversed for descending order — so missing and `null` values sort **first
  ascending, last descending**. Ties are broken by `id` ascending.
- Default order: `_meta.created_at` ascending, then `id`.
- `limit` defaults to 100, max 1000. Page with `offset` while `hasMore` is true, advancing it
  by the number of documents each page returned (a page may hold fewer than `limit`).
- There are no aggregates, joins or full-text search — compute them in JavaScript.

### Realtime: watch and subscribe

**`collection.watch(callback, query?)`** — a live query. Calls `callback(docs)` with the full
result immediately and again after every change to the collection (re-fetched, debounced
50 ms), including after reconnects. Without `query` it watches `list()` (first 100 docs,
oldest first). Returns `stop()`.

```js
const stop = posts.watch(render, posts.where("status", "==", "open").orderBy("votes", "desc").limit(50));
// later: stop();
```

**`collection.subscribe(handlers)`** — raw change events from anyone (including yourself),
delivered over the page's websocket, in order. Only live events are delivered — after a
reconnect, changes that happened while offline are not replayed (use `watch` when you must not
miss any). `onCreate`/`onUpdate` always get a full document: when a document is too large to
travel in the event (about 1 MiB, only possible if the admins raised the document size limit),
the SDK re-reads it first. Treat `onDelete` ids you do not know as harmless (remove by id).

```js
const unsubscribe = posts.subscribe({
  onCreate(doc) {},          // the new document
  onUpdate(doc) {},          // the document after the change
  onDelete(id) {},           // id of the deleted document
  onChange(event) {},        // every event: { op: "create"|"update"|"delete", collection, id, doc|null }
});
```

Passing a function instead of an object is shorthand for `{ onChange }`. Errors: `subscribe`
accepts `onError(err)` (e.g. an invalid collection name), and `watch(callback, query, { onError })`
reports failed re-fetches (default: a console warning). A `Query` has its own
`query.watch(callback)`.

### Recipes for common data shapes

- **One document per person** (vote, RSVP, preferences): `set(sanitize(me.id), data)`.
- **Counters / likes:** one document per like (id = `<item>:<user>`) and count them; there is
  no atomic increment, and read-modify-write `update`s from several people can lose writes.
- **Ordering by time:** generated ids and `_meta.created_at` are time-ordered.
- **"Mine":** `where("_meta.created_by", "==", me.id)`.
- **Big blobs:** store files with `backyard.files` and keep only the `url` in documents.

## Files

| Method | Returns |
|---|---|
| `backyard.files.upload(file, { name? })` | `FileInfo` — `file` is a `File` or `Blob`; `name` overrides the file name (give Blobs a name). Max 100 MiB by default. |
| `backyard.files.list({ limit?, offset? })` | `FileInfo[]`, newest first (100 per page by default); the array's non-enumerable `hasMore` is `true` when more exist. |
| `backyard.files.get(id)` | `FileInfo \| null` |
| `backyard.files.delete(id)` | `void` |

```ts
interface FileInfo {
  id: string;            // ULID
  name: string;          // "team-photo.png"
  url: string;           // "/__backyard/files/01JNV…/team-photo.png" (path mode: "/s/<site>/__backyard/files/…")
  size: number;          // bytes
  content_type: string;  // "image/png"
  created_at: string;
  created_by: string;    // user id
}
```

`url` is site-relative and works directly in `<img src>`, `<video src>` (Range requests are
supported) and `<a href download>`. Uploaded files are visible to everyone who can open the
site. Only passive types (images, audio, video, plain text, CSV, PDF, JSON) are served as
they are; anything else — HTML, SVG, XML, scripts, unknown types — gets
`Content-Security-Policy: sandbox`, so it displays but never runs scripts as your site. Host
pages as site files instead. Files have
no realtime feed: to show other people's uploads live, also create a db document per upload
and `watch` that collection.

```js
input.onchange = async () => {
  const info = await backyard.files.upload(input.files[0]);
  preview.src = info.url;
};
// From a canvas: canvas.toBlob(b => backyard.files.upload(b, { name: "drawing.png" }));
```

## AI

Model names are **aliases** configured by Backyard admins (e.g. `smart`, `fast`, `image`,
`embed`); omit `model` to use the server default. List them with `backyard.ai.models()`.
Requests are proxied by the server with its own credentials, rate limited per user
(default 20 calls/min, 2M tokens/day). When AI is not configured, calls reject with
`unsupported`.

### chat

```js
const res = await backyard.ai.chat(input, {
  model: "smart",        // optional alias
  system: "You are a helpful assistant for the Platform team.",
  maxTokens: 1024,       // optional, capped by the server
  temperature: 0.2,      // optional; ignored by models that reject it
  json: false,           // true → ask for a JSON object and parse it into res.json
});
// res = { content: "…", model: "smart", stop_reason: "end_turn", usage: { input_tokens: 12, output_tokens: 40 }, json?: any }
```

- `input` is a prompt string or an array of messages:
  `{ role: "user" | "assistant" | "system", content: string | ContentBlock[] }`. System
  messages are hoisted into the system prompt.
- Content blocks: `{ type: "text", text }`, `{ type: "image", data: "<base64 without data: prefix>", media_type: "image/png" }`,
  `{ type: "image", url: f.url }` (an upload of this site),
  `{ type: "image", url: "data:image/png;base64,…" }` or `{ type: "image", url: "https://…" }`.
  The OpenAI shape `{ type: "image_url", image_url: { url } }` is accepted too.
  - **Uploads of this site work directly:** pass the `url` that `backyard.files.upload` (or
    `backyard.ai.image`) returned. The server reads the file from storage — nothing is
    fetched — and detects its type from the bytes (png, jpeg, gif or webp). Another site's
    file, an unknown file or any other site-relative URL is rejected (`bad_request`).
  - Other URLs must be absolute and **publicly reachable** (the provider or Backyard's guarded
    fetcher downloads them; private and internal addresses are refused).
  - Images you only have in the browser (a picked `File` you did not upload, a canvas) go as a
    data URL.
  - Images are limited to 20 MiB each, and images loaded from URLs to 64 MiB per request.

```js
const f = await backyard.files.upload(input.files[0]);     // or a file uploaded earlier
const { content } = await backyard.ai.chat([{ role: "user", content: [
  { type: "text", text: "Describe this photo in one sentence." },
  { type: "image", url: f.url },
] }]);

// Not uploaded: send it as a data URL.
const toDataURL = (blob) => new Promise((ok, fail) => {
  const r = new FileReader(); r.onload = () => ok(r.result); r.onerror = () => fail(r.error); r.readAsDataURL(blob);
});
canvas.toBlob(async (png) => {
  const { content } = await backyard.ai.chat([{ role: "user", content: [
    { type: "text", text: "What did I draw?" }, { type: "image", url: await toDataURL(png) },
  ] }]);
});
```

- `stop_reason` is normalized to `end_turn`, `max_tokens`, `stop_sequence`, `refusal` or `other`.
- With `json: true`, `res.json` is the parsed object (`undefined` if the model did not return
  valid JSON — validate it). Describe the exact shape you want in the prompt.

```js
const { json } = await backyard.ai.chat(
  `Classify this feedback. Reply with JSON {"sentiment": "positive"|"neutral"|"negative", "topics": string[]}.\n\n${text}`,
  { json: true, temperature: 0 });
```

Keep conversations in memory (or in the db) and send the whole history each turn:

```js
const history = [];
async function ask(q) {
  history.push({ role: "user", content: q });
  const { content } = await backyard.ai.chat(history, { system: "Be concise." });
  history.push({ role: "assistant", content });
  return content;
}
```

### stream

`backyard.ai.stream(input, opts)` starts a streaming completion immediately (same options as
`chat`, plus `onText(text)`). The returned object is an async iterable of `{ text }` chunks and
has a `done` promise resolving to the final result (`{ content, model, stop_reason, usage }`),
a `text` property with everything received so far and `abort()` (then `done` rejects with
`aborted`; breaking out of a `for await` loop does not cancel the request). Every AI method also
accepts an `AbortSignal` as `signal`.

```js
const s = backyard.ai.stream(history, { system: "Be concise.", onText: (t) => (out.textContent += t) });
const final = await s.done;          // rejects with BackyardError on failure

// or iterate:
let text = "";
for await (const chunk of backyard.ai.stream("Write a haiku about Mondays")) text += chunk.text;
```

### image

```js
const img = await backyard.ai.image("isometric illustration of a tiny office garden", { size: "1024x1024", n: 1, model: "image" });
// img = { id: "01JNV…", url: "/__backyard/files/01JNV…/image-1.png", content_type: "image/png",
//         model: "image", images: [{ id, url, content_type }] }   // id/url/content_type = first image
```

Generated images are stored as site files: they appear in `files.list()`, and `id` works with
`backyard.files.get(id)` / `backyard.files.delete(id)`.

### embed

```js
const v = await backyard.ai.embed("vacation policy");             // number[]
const vs = await backyard.ai.embed(["doc one", "doc two"]);        // number[][] (same order)
const cosine = (a, b) => { let d = 0, na = 0, nb = 0; for (let i = 0; i < a.length; i++) { d += a[i] * b[i]; na += a[i] ** 2; nb += b[i] ** 2; } return d / Math.sqrt(na * nb); };
```

### models

```js
const { default: def, models } = await backyard.ai.models();
// models = [{ name: "smart", kind: "chat", provider: "anthropic", description: "Best quality" }, { name: "image", kind: "image", … }]
const chatModels = models.filter((m) => m.kind === "chat");
```

`kind` is `chat` (for `chat`/`stream`), `image` (for `image`) or `embedding` (for `embed`).
Provider model ids (`model`) are only included when the admin enables it.

## Warehouse

Read-only analytics queries against the company warehouse (BigQuery, Postgres/Redshift,
Athena). Not available on the Cloudflare edition or when not configured (`unsupported`).

```js
// Named query (pre-approved by admins, parameters use @name in the SQL)
const r = await backyard.warehouse.named("weekly_active_users", { team: "growth" });

// Raw SQL (only if the server allows it; else "forbidden"). Positional ? placeholders.
const r2 = await backyard.warehouse.query(
  "SELECT day, COUNT(*) AS orders FROM orders WHERE region = ? GROUP BY day ORDER BY day", ["emea"], { source: "default" });

// r = { columns: [{ name: "day", type: "DATE" }, { name: "orders", type: "INT64" }],
//       rows: [{ day: "2026-01-01", orders: 120 }, …], row_count: 7, truncated: false }

const { sources, queries } = await backyard.warehouse.sources();
// sources = [{ name: "default", type: "bigquery", description }], queries = [{ name, description, params: ["team"], source }]
```

- Raw SQL must start with `SELECT` or `WITH` (after comments) unless the admin allows any
  statement; Postgres sources run in a read-only transaction.
- Results are capped (default 10,000 rows, and 32 MiB of row data → `truncated: true`) and time
  out after 60 s by default. Aggregate in SQL instead of fetching wide raw rows.
- Column `type` is the engine's type name. Values arrive as JSON: dates/timestamps as strings,
  big integers/decimals may be strings — convert with `Number()` when charting.
- Rate limited per user; cache results in memory instead of re-querying on every render.

## Realtime rooms

Rooms broadcast messages and track **presence** (who is connected, with a small state
object each). The SDK keeps **one websocket per page**, created on first use (room, subscribe
or watch), reconnects with exponential backoff (0.5 s → 10 s, jitter) and transparently
re-joins rooms with the latest presence state.

```js
const room = backyard.socket.room("lobby", { state: { status: "online" } });  // sync; joins automatically
```

| Member | Description |
|---|---|
| `room.on("message", (data, from) => …)` | A message from another member (or yourself with `echo`). `from = { cid, user: UserSummary }`. |
| `room.on("join" \| "leave" \| "update", (member) => …)` | Someone joined, left or changed their state. |
| `room.on("members", (members) => …)` | Full member list (excluding yourself) after any presence change. |
| `room.members` | *sync* current `Member[]` (excluding yourself). |
| `room.self` | *sync* your own `Member` once joined, else `null`. |
| `room.state`, `room.joined` | *sync* the presence state you publish; whether the server confirmed the membership on the current connection. |
| `room.send(data, { to?, echo? })` | *sync* broadcast any JSON (≤ 64 KiB); `to` = a member's `cid` for a direct message; `echo: true` also delivers it to yourself (default `false`). Queued while reconnecting. |
| `room.setState(state)` | *sync* replace your presence state (JSON object ≤ 8 KiB). |
| `room.updateState(partial)` | *sync* shallow-merge into your presence state. |
| `room.on("error", (err) => …)` | The server rejected joining (e.g. too many rooms), a message (too large, unknown `to`) or a state update (over 8 KiB). Without a handler these go to `socket.on("error")`. |
| `room.off(event, handler)` | Remove a handler (`on` also returns an unsubscribe function). |
| `room.leave()` | Leave the room. |
| `backyard.socket.connected` | *sync* whether the websocket is connected. |
| `backyard.socket.cid`, `backyard.socket.user` | *sync* this tab's connection id and the user the server identified, once connected (else `null`). |
| `backyard.socket.on("open" \| "close" \| "error", handler)` | Connection status changes (show an "offline" badge) and realtime errors such as `rate_limited`. |
| `backyard.socket.connect()` / `.disconnect()` | Open the websocket without rooms or subscriptions / close it until it is used again. |

```ts
interface Member { cid: string; user: UserSummary; state: object; joined_at: string }
```

- `cid` identifies a **connection** (one per browser tab); the same person in two tabs appears
  twice — group by `member.user.id` for "people here".
- Room names: `^[A-Za-z0-9_\-:.]{1,128}$`. Rooms are per site and exist while someone is in them.
- Messages are **not stored**: late joiners do not see earlier messages. Persist what matters
  in the db.
- Calling `socket.room(name)` again with the same name returns the same room.
- Keep presence small and throttle `updateState` (≤ 20/s); the server allows 60 messages/s
  (burst 120) per connection.

```js
const room = backyard.socket.room("standup", { state: { speaking: false } });
room.on("members", (members) => renderPeople([room.self, ...members].filter(Boolean)));
room.on("message", (data, from) => { if (data.type === "reaction") floatEmoji(data.emoji, from.user.name); });
reactBtn.onclick = () => room.send({ type: "reaction", emoji: "🎉" }, { echo: true });
backyard.socket.on("close", () => badge.hidden = false);
backyard.socket.on("open", () => badge.hidden = true);
```

## Errors

HTTP errors have the body `{"error": {"code": "not_found", "message": "…", "login_url"?: "…"}}`.
The SDK turns them into `BackyardError` with the same `code`.

| `code` | HTTP | Meaning / what to do |
|---|---|---|
| `bad_request` | 400 (405) | Invalid input: collection/id/field name, query operator, JSON body, a model of the wrong kind; `405` when the method is not allowed for the path. Fix the call. |
| `unauthenticated` | 401 | Not signed in or session/token expired. The SDK redirects to `loginUrl`. |
| `forbidden` | 403 | Missing `X-Backyard` header or cross-origin write, user not allowed (domain/group restrictions), raw SQL disabled, site owned by someone else (deploys). |
| `not_found` | 404 | Unknown document/file/user/named query/model alias. (`get` methods return `null` instead.) |
| `conflict` | 409 | `create` with an id that exists. Use `set` or `update`. |
| `too_large` | 413 | Document > 256 KiB, upload or deploy file over the limit, too many documents in the site. |
| `rate_limited` | 429 | Slow down; `err.retryAfter` / `Retry-After` gives seconds to wait. |
| `unsupported` | 501 | Feature not configured on this server (AI, warehouse, realtime, tokens). |
| `upstream_error` | 502/504 | The AI provider or warehouse failed or timed out. Retry later. |
| `internal` | 500 | Server bug; retry, report if persistent. |
| `network_error` | 0 | SDK only: offline, DNS or connection failure. |
| `aborted` | 0 | SDK only: request cancelled. |
| `bad_response` | 0 | SDK only: a non-JSON answer (often a proxy login page — reload the page). |

## Limits

Defaults of the Go server; administrators can change them (ask `GET /__backyard/api/info` on the
hub for the deploy limits). The Cloudflare free-tier edition has smaller limits and daily caps —
see [deploy.md](deploy.md#cloudflare-edition-free-tier).

| Area | Limit |
|---|---|
| Document | 256 KiB JSON; 100,000 documents per site |
| Query | `limit` default 100, max 1000; `in`/`not-in` ≤ 30 values; field depth ≤ 8 |
| Upload | 100 MiB per file (`files.upload`) |
| Site (deploy) | 500 MiB total, 10,000 files, 100 MiB per file |
| Websocket | 64 KiB per frame; 60 msgs/s sustained, burst 120; 50 rooms and 100 subscriptions per connection; presence state ≤ 8 KiB; 50 connections per user |
| Rate limits (per user) | API 600/min · db writes 300/min · AI 20 calls/min and 2,000,000 tokens/day · uploads 60/min · warehouse 30 queries/min · deploys 60/h |
| AI | `max_tokens` capped by the server (default 8192); chat images ≤ 20 MiB each, ≤ 64 MiB per request for images loaded from URLs (uploads included) |
| Warehouse | 10,000 rows and 32 MiB per result, 60 s timeout |

## HTTP API

The SDK is a thin client over this API; use it directly from scripts, other languages or
`fetch`. Base: `https://<site>.backyard.example.com/__backyard/api` (host mode) or
`https://backyard.example.com/s/<site>/__backyard/api` (path mode).

**Rules**

- JSON in and out (`Content-Type: application/json`).
- **CSRF:** every non-GET/HEAD request must send `X-Backyard: 1`; if an `Origin` header is
  present it must be the site's own origin (host mode) or the hub origin (path mode).
  Otherwise `403 forbidden`.
- **Auth:** the browser session (cookie, or the identity-aware proxy) — or
  `Authorization: Bearer <token>` with a personal API token (`backyard token create`) or a
  static CI token.
- Errors: status + `{"error": {"code", "message", "login_url"?}}`; `429` carries `Retry-After`.

```sh
TOKEN=$(backyard token create --name script --ttl 24h)   # prints a personal API token
curl -s -H "Authorization: Bearer $TOKEN" https://lunch-vote.backyard.example.com/__backyard/api/db/spots
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-Backyard: 1" -H "Content-Type: application/json" \
  -d '{"name":"Tacos"}' https://lunch-vote.backyard.example.com/__backyard/api/db/spots
```

### Identity

| Method & path | Result |
|---|---|
| `GET /me` | `User` |
| `GET /users/{email}` | `User` \| 404 |
| `GET /site` | `SiteMeta` of the current site |

`SiteMeta = {name, url, title, description, preview, created_at, created_by, updated_at,
updated_by, deploy_id, file_count, bytes, owners: []}` — `title`/`description` come from the
root `index.html`'s `<title>` and `<meta name="description">`; `preview` (omitted when there is
none) is the site path of the image the hub shows on the site's card: the `og:image` of
`index.html` when it names one of the site's own files (relative or `/`-rooted; png, jpg, jpeg,
gif, webp, avif or svg; ≤ 1 MiB), else the first of `preview.png`, `preview.jpg`,
`preview.jpeg`, `preview.webp`, `preview.avif`, `preview.gif`, `preview.svg` at the site root
that is at most 1 MiB. All three are recomputed whenever the site is deployed or refreshed.

### Database

| Method & path | Body | Result |
|---|---|---|
| `GET /db` | | `{"collections": ["posts", …]}` |
| `POST /db/{c}` | data (optional `id`) | `201` doc; `409 conflict` if `id` exists |
| `GET /db/{c}?limit=&offset=&order=field:asc,field2:desc` | | `{"docs": […], "has_more": bool}` |
| `POST /db/{c}/query` | query object | `{"docs": […], "has_more": bool}` |
| `GET /db/{c}/{id}` | | doc \| `404` |
| `PUT /db/{c}/{id}` | data | doc (create or replace; version + 1) |
| `PATCH /db/{c}/{id}` | partial data | doc (shallow merge; `null` deletes a key); `404` if missing |
| `DELETE /db/{c}/{id}` | | `204` (also when missing) |

Every successful write publishes a realtime event to subscribers of the collection.

### Files

| Method & path | Body | Result |
|---|---|---|
| `POST /files` | `multipart/form-data`: field `file` (+ optional `name`) | `201` FileInfo |
| `GET /files?limit=&offset=` | | `{"files": [FileInfo], "has_more": bool}` newest first |
| `GET /files/{id}` | | FileInfo \| `404` |
| `DELETE /files/{id}` | | `204` (also when missing) |

Bytes are served (not under `/api`) at `/__backyard/files/{id}/{name}` with Range support
(every type except passive ones — raster images, audio/video, plain text, CSV, PDF, JSON,
octet-stream — with `Content-Security-Policy: sandbox`).

### AI

`POST /ai/chat`

```jsonc
{ "messages": [ {"role": "user", "content": "Summarize my tasks"} ],
  // content may also be [{"type":"text","text":"…"}, {"type":"image","data":"<base64>","media_type":"image/png"}, {"type":"image","url":"https://…"}]
  "model": "smart",        // alias; default = server default
  "system": "You are…",    // optional; role:"system" messages are hoisted
  "max_tokens": 4096,      // optional, capped by the server
  "temperature": 0.2,      // optional
  "json": false,           // optional: ask for a JSON object
  "stream": false }
```

- Non-streaming → `{"content": "…", "model": "smart", "stop_reason": "end_turn", "usage": {"input_tokens": 12, "output_tokens": 40}}`
- `"stream": true` → `text/event-stream`, one `data: <json>` line per event:
  `{"type":"text","text":"…"}` (repeated), then
  `{"type":"done","model":"smart","stop_reason":"end_turn","usage":{…}}`, or
  `{"type":"error","error":{"code":"upstream_error","message":"…"}}`. Lines starting with `:`
  (`: ping` keep-alives while the model is silent) are comments — ignore them.

| Method & path | Body | Result |
|---|---|---|
| `POST /ai/image` | `{"prompt", "model"?, "size"?: "1024x1024", "n"?: 1}` | `{"model": "image", "images": [{"id", "url": "/__backyard/files/…", "content_type": "image/png"}]}` |
| `POST /ai/embed` | `{"input": string \| string[], "model"?}` | `{"model": "embed", "embeddings": [[…], …], "usage": {"input_tokens", "output_tokens"}}` (one vector per input string) |
| `GET /ai/models` | | `{"default": "smart", "models": [{"name", "kind", "provider", "model"?, "description"?}]}` |

Clients pass aliases; raw `"provider/model-id"` names work only when the admin enables
`ai.allow_raw_models`.

### Warehouse

| Method & path | Body | Result |
|---|---|---|
| `POST /warehouse/query` | `{"sql": "SELECT … WHERE team = ?", "params": ["growth"], "source": "default"}` or `{"query": "weekly_active_users", "params": {"team": "growth"}}` | `{"columns": [{"name","type"}], "rows": [{…}], "row_count": 7, "truncated": false}` |
| `GET /warehouse` | | `{"sources": [{name, type, description}], "queries": [{name, description, params, source}]}` |

## Websocket protocol

`GET /__backyard/api/ws` (same auth as the API). JSON text frames, max 64 KiB. The first frame
from the server is `{"t":"hello","cid":"c_8f3a…","user":{User}}`.

Client → server (`id` is an optional correlation id echoed in `ack`/`joined`/`error`;
`leave`, `state` and `send` frames with an `id` are also answered with `{"t":"ack","id"}` on
success):

| Frame | Meaning |
|---|---|
| `{"t":"sub","id":"s1","collection":"posts"}` | Subscribe to db changes → `{"t":"ack","id":"s1"}` |
| `{"t":"unsub","id":"s1"}` | Unsubscribe → ack |
| `{"t":"join","id":"r1","room":"lobby","state":{…}}` | Join with presence state → `{"t":"joined","id":"r1","room":"lobby","self":Member,"members":[Member…]}` (members excludes self) |
| `{"t":"leave","room":"lobby"}` | Leave a room |
| `{"t":"state","room":"lobby","state":{…}}` | Replace own presence state |
| `{"t":"send","room":"lobby","data":any,"to":"c_…","echo":false}` | Broadcast (or direct message with `to`) |
| `{"t":"ping"}` | → `{"t":"pong"}` |

Server → client:

| Frame | Meaning |
|---|---|
| `{"t":"db","sub":"s1","collection":"posts","op":"create"\|"update"\|"delete","doc":{…}\|null,"doc_id":"…"}` | Database change (`doc` is the flattened document; `null` for deletes, and for a create or update too large to broadcast: read it with `GET /db/{c}/{doc_id}`) |
| `{"t":"presence","room":"lobby","op":"join"\|"leave"\|"update","member":Member}` | Presence change |
| `{"t":"msg","room":"lobby","from":{"cid":"…","user":UserSummary},"data":any}` | Room message |
| `{"t":"error","id":…,"error":{"code","message"}}` | Error (e.g. `rate_limited`, `bad_request`) |

`Member = {"cid", "user": UserSummary, "state": {…}, "joined_at": RFC3339}` (`joined_at` with
exactly three fractional digits; `members` sorted by `joined_at`, then `cid`),
`UserSummary = {id, name, email, avatar_url}`. Errors never close the connection. The server
pings every 25 s and closes connections that do not answer within 60 s. Limits: 50 rooms and 100
subscriptions per connection, 60 msgs/s (burst 120), presence state ≤ 8 KiB, 50 connections per
user (more are refused with HTTP `429`). The `Origin` must be the site's own origin (host mode)
or the hub's (path mode).

## Hub endpoints

On the hub host (`https://backyard.example.com`):

| Path | Purpose |
|---|---|
| `GET /__backyard/api/me` | Current user |
| `GET /__backyard/api/sites` | `{"sites": [SiteMeta]}` — the site directory |
| `GET /__backyard/api/sites/{site}/preview` | The image `SiteMeta.preview` names (`404 not_found` without one, or when the stored file is over 1 MiB), typed by its extension and sandboxed (`Content-Security-Policy: sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'`); a non-empty `?v=` makes it cacheable for a year (`Cache-Control: private, max-age=31536000, immutable`; without one `private, no-cache`), the hub passes the site's last update |
| `POST /__backyard/api/tokens` | `{"name"?, "ttl_hours"?}` → `{"token", "expires_at", "name"?}` (personal API token; `ttl_hours` at most the server's token TTL, default 720; needs `X-Backyard: 1`; not for guests or static tokens) |
| `GET /__backyard/api/info` | Server info: URL mode, auth mode, deploy methods, limits |
| `/__backyard/api/deploy/…` | Deploy gateway (see [deploy.md](deploy.md)) |
| `/docs`, `/llms.txt`, `/llms-full.txt` | Documentation for people and agents |
| `/__backyard/healthz`, `/__backyard/readyz` | Health checks (no auth) |

Sites embed in each other freely (`<iframe src="https://other.backyard.example.com/">`); there
is no `X-Frame-Options`.
