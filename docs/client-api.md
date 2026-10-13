# Client API

Every site gets the same zero-config backend through one script tag. This page is a tour; the
complete reference — every method, option, wire format, error and limit — is
[`skills/backyard/references/api.md`](../skills/backyard/references/api.md) (also served by
every hub at `/docs` and in `/llms-full.txt`). The TypeScript declarations in
[`web/sdk/backyard.d.ts`](../web/sdk/backyard.d.ts) are normative and served at
`/__backyard/backyard.d.ts`.

## Loading the SDK

```html
<script src="/__backyard/backyard.js"></script>        <!-- defines window.backyard -->
<script type="module">
  import backyard, { db, ai } from "/__backyard/backyard.mjs";   // or as an ES module
</script>
```

Always use the **absolute** path `/__backyard/…` for the SDK and **relative** paths for your own
files, so the page works in host mode (`https://<site>.backyard.acme.com/`) and path mode
(`https://backyard.acme.com/s/<site>/`). There is no npm package, no build step and no key: the
server prepends a one-line context preamble (site, mode, hub URL) and the SDK derives its API
base from it.

| Namespace | What it does |
|---|---|
| `backyard.id` / `backyard.me()` | The signed-in person (`{id, email, name, title, team, slack, avatar_url, groups, anonymous, admin, extra}`) and colleague lookup. |
| `backyard.db` | Per-site JSON documents with queries, change feeds and live queries. |
| `backyard.files` | Uploads (100 MiB by default) served from the site with Range support. |
| `backyard.ai` | Chat, streaming, image generation and embeddings through server-side providers. |
| `backyard.warehouse` | Read-only SQL against the company warehouse (named queries or raw SQL). |
| `backyard.socket` | Websocket rooms with presence and messages. |

Everything is scoped to the site the page runs on and shared by everyone who can open it.

## A tour

```js
// Identity
const me = await backyard.me();                       // cached after the first call
const bob = await backyard.id.lookup("bob@acme.com");   // User | null

// Database
const posts = backyard.db.collection("posts");        // created by the first write
const post = await posts.create({ title: "Hello", votes: 0 });   // {id, title, votes, _meta:{…}}
await posts.update(post.id, { votes: 1, draft: null });           // shallow merge; null deletes
const top = await posts.where("votes", ">=", 1).orderBy("votes", "desc").limit(10).get();
const stop = posts.watch((docs) => render(docs));                 // live query
posts.subscribe({ onCreate: (doc) => toast(`New: ${doc.title}`) }); // change feed

// Files
const f = await backyard.files.upload(input.files[0]);  // {id, name, url, size, content_type, …}
img.src = f.url;

// AI (model = an alias configured by the admins)
const { content } = await backyard.ai.chat("Summarize: " + text, { model: "fast" });
const s = backyard.ai.stream(question, { onText: (t) => (out.textContent += t) });
await s.done;
const { json } = await backyard.ai.chat(`Extract {name, date} from: ${text}`, { json: true });
const image = await backyard.ai.image("a watercolor fox");          // {id, url, content_type, images}
const vector = await backyard.ai.embed("vacation policy");          // number[]

// Warehouse
const { rows } = await backyard.warehouse.named("weekly_active_users", { team: "growth" });

// Realtime rooms
const room = backyard.socket.room("lobby", { state: { status: "here" } });
room.on("members", (members) => renderPeople(members));
room.send({ type: "wave" });
```

## Query semantics in one paragraph

Filters (`==`, `!=`, `<`, `<=`, `>`, `>=`, `in`, `not-in`, `contains`) only match values of the
**same JSON type** — `"3"` never matches `3`, `!= 2` skips `"2"`, `!= null` matches nothing — and
a **missing field never matches**. Sorting ranks types (missing/null < booleans < numbers <
strings < arrays < objects) and breaks ties by `id`; the default order is creation time.
Document timestamps have microsecond precision. Every backend (SQLite, Postgres, DynamoDB,
memory, the Cloudflare edition) passes the same conformance cases. See
[SPEC §5.2](spec/SPEC.md#52-database).

## Errors

Every call rejects with a `BackyardError` (`err.code`, `err.status`, `err.message`, and
`err.loginUrl` on `401`, `err.retryAfter` on `429`). On `401` the SDK sends the page to the
login screen unless `backyard.configure({ autoLogin: false })`.

| `code` | HTTP | Typical cause |
|---|---|---|
| `bad_request` | 400 / 405 | Invalid name, operator or body |
| `unauthenticated` | 401 | Session or token expired |
| `forbidden` | 403 | Missing `X-Backyard` header, cross-origin write, gated user |
| `not_found` | 404 | Unknown document, file, alias or named query (`get` returns `null`) |
| `conflict` | 409 | `create` with an existing id |
| `too_large` | 413 | Document, upload or quota over the limit |
| `rate_limited` | 429 | Per-user limits, or the AI provider throttling the server (with `Retry-After`) |
| `unsupported` | 501 | Feature not configured (AI, warehouse, realtime) |
| `upstream_error` | 502 / 503 / 504 | AI provider or warehouse failed, throttled without a retry time (503) or timed out (504) |
| `internal` | 500 | Server bug |

AI provider answers map onto these codes the same way on both editions (a rejected request is
`bad_request`, an oversized one `too_large`; see
[SPEC §5.4](spec/SPEC.md#54-ai)). Warehouse failures on the server's side carry a generic
message naming the request id (`X-Request-Id`, present on every response) under which the
server logged the details — quote it when you ask the platform team.

## The HTTP API underneath

The SDK is a thin client over JSON endpoints under `/__backyard/api/` on each site; scripts and
other languages can use them directly with a bearer token:

```sh
TOKEN=$(backyard token create --name script --ttl 24h)
curl -s -H "Authorization: Bearer $TOKEN" \
  https://lunch-vote.backyard.acme.com/__backyard/api/db/votes
curl -s -X POST -H "Authorization: Bearer $TOKEN" -H "X-Backyard: 1" \
  -H "Content-Type: application/json" -d '{"choice":"tacos"}' \
  https://lunch-vote.backyard.acme.com/__backyard/api/db/votes
```

Every state-changing request must carry `X-Backyard: 1` (the SDK always does) — that header is
Backyard's CSRF defence. Endpoints, bodies and responses are listed in
[SPEC §5](spec/SPEC.md#5-site-api-__backyardapi-site-scoped) and the
[API reference](../skills/backyard/references/api.md#http-api); the websocket protocol in
[realtime](realtime.md#protocol).
