# Realtime

Backyard pages are live by default: database changes reach every open tab within
milliseconds, and websocket **rooms** carry presence (who is here, with what state) and
fire-and-forget messages (cursors, reactions, game moves). Everything runs over **one
websocket per page** that the SDK opens on first use, reconnects with exponential backoff
(0.5 s → 10 s, with jitter) and restores — subscriptions, rooms and your latest presence state —
after every reconnect.

## Live data

**Live queries** are the easy path: `watch` calls back with the full result now and again after
every change (re-fetched, debounced 50 ms), including after reconnects.

```js
const todos = backyard.db.collection("todos");
const stop = todos.watch(render, todos.where("done", "==", false).orderBy("_meta.created_at", "desc").limit(50));
// later: stop()
```

**Change feeds** deliver individual events, including your own writes. Only live events are
delivered — changes made while a tab was offline are not replayed (use `watch` when you must
not miss any).

```js
const unsubscribe = todos.subscribe({
  onCreate(doc) {}, onUpdate(doc) {}, onDelete(id) {},
  onChange({ op, collection, id, doc }) {},
});
```

Every successful write through the API (`create`, `set`, `update`, `delete`) publishes one
event to the site's subscribers on every instance.

## Rooms and presence

```js
const room = backyard.socket.room("board", { state: { x: 0, y: 0, color: "#f80" } });

room.on("members", (members) => drawCursors(members));      // everyone but you, after any change
room.on("join", (member) => {});                            // also "leave" and "update"
room.on("message", (data, from) => {});                     // from = { cid, user }

room.updateState({ x: 0.4, y: 0.7 });                       // shallow merge (setState replaces)
room.send({ type: "reaction", emoji: "🎉" }, { echo: true }); // broadcast; { to: cid } for one member
room.leave();
```

- A **member** is a connection: `{cid, user: {id, name, email, avatar_url}, state, joined_at}`.
  The same person in two tabs appears twice — group by `member.user.id` for "people here".
- Rooms are per site and exist while someone is in them; messages are **not stored** (late
  joiners don't see them). Persist what matters in the database.
- Keep presence state small (≤ 8 KiB) and throttle updates (≈ 20/s); the server allows
  60 frames/s per connection (burst 120).
- `backyard.socket.connected`, `socket.on("open" | "close" | "error")`, `socket.cid` and
  `socket.user` expose the connection.

[Recipes](../skills/backyard/references/recipes.md) include multiplayer cursors, a chat with
presence and typing indicators, a live poll and a game leaderboard.

## Protocol

`GET /__backyard/api/ws` on the site, authenticated like any API call. JSON text frames; the
first server frame is `{"t":"hello","cid":"c_…","user":{…}}`.

| Client → server | Answer |
|---|---|
| `{"t":"sub","id":"s1","collection":"posts"}` | `{"t":"ack","id":"s1"}`, then `db` frames |
| `{"t":"unsub","id":"s1"}` | `ack` |
| `{"t":"join","id":"r1","room":"lobby","state":{…}}` | `{"t":"joined","id":"r1","room":"lobby","self":Member,"members":[…]}` |
| `{"t":"leave","room":"lobby"}` | — (`ack` when it carries an `id`) |
| `{"t":"state","room":"lobby","state":{…}}` | — (`ack` with an `id`) |
| `{"t":"send","room":"lobby","data":…,"to":"c_…","echo":false}` | — (`ack` with an `id`) |
| `{"t":"ping"}` | `{"t":"pong"}` |

| Server → client | Meaning |
|---|---|
| `{"t":"db","sub":"s1","collection":"posts","op":"create\|update\|delete","doc":{…}\|null,"doc_id":"…"}` | Document change (flattened document; `null` for deletes) |
| `{"t":"presence","room":"lobby","op":"join\|leave\|update","member":Member}` | Presence change |
| `{"t":"msg","room":"lobby","from":{"cid","user"},"data":…}` | Room message |
| `{"t":"error","id":…,"error":{"code","message"}}` | Error for a frame (the connection stays open) |

Details that matter when writing your own client:

- `joined_at` always has three fractional digits (`2026-01-02T03:04:05.120Z`) and member lists
  are ordered by `joined_at`, then `cid`.
- Room names match `^[A-Za-z0-9_\-:.]{1,128}$`; `to` must be a member's `cid`.
- The server pings every 25 s and drops connections that do not answer within 60 s. A client
  that cannot keep up with its outbound queue is closed with status 1008; frames far over the
  size limit with 1009.
- The browser `Origin` must be the site's own origin (path mode: the hub's). Set
  `realtime.allow_cross_site_ws: true` to let other Backyard sites embed a site's realtime.

The normative description is [SPEC §6](spec/SPEC.md#6-realtime-websocket-get-__backyardapiws).

## Limits

| Setting | Default |
|---|---|
| `realtime.max_message_bytes` | 64 KiB per frame |
| `realtime.messages_per_second` / `message_burst` | 60 / 120 per connection |
| `realtime.max_rooms_per_conn` / `max_subs_per_conn` | 50 / 100 |
| `realtime.max_presence_bytes` | 8 KiB |
| `realtime.max_conns_per_user` | 50 (more get HTTP 429 before the upgrade) |
| `realtime.disabled` | `false` (`true` answers `501 unsupported`) |

## Several instances

Each instance holds its own connections. With more than one instance, configure a shared
**pub/sub** (`pubsub.type: postgres` or `redis`, see [storage and databases](storage-and-databases.md#pubsub)):

- every instance subscribes to `rt.<site>.db` while a local client watches that site's
  collections, and to `rt.<site>.room.<room>` while a local client is in the room;
- database changes and room messages are published once and delivered by every instance to its
  local clients;
- presence is applied locally right away and gossiped: join (plus a request for the other
  instances' members), update, leave, and a full member snapshot every 20 s. Members of other
  instances that are not refreshed within 60 s expire. Per-instance sequence numbers stop a
  delayed snapshot from resurrecting someone who left.

Delivery is at-most-once; `watch` re-fetches after reconnects, so live lists heal on their own.
Load balancers need websocket support and generous idle timeouts (≥ 60 s); session affinity is
not required.

On the [Cloudflare edition](cloudflare.md) each site's realtime hub runs in that site's Durable
Object (with hibernation), so no pub/sub is involved. The server sends no keep-alive pings
there; the SDK's own `{"t":"ping"}` frames keep connections alive.
