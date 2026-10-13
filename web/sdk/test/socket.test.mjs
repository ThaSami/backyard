// Realtime: db subscriptions, live queries, rooms/presence, queueing,
// reconnect with resubscribe/rejoin, keepalive (SPEC §5.6, §6).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createEnv, plain, flush, apiError } from "./helpers/harness.mjs";

const user = (id) => ({ id, name: id.split("@")[0], email: id, avatar_url: "" });
const member = (cid, id, state = {}, joined_at = "2026-01-01T00:00:0" + cid.slice(-1) + "Z") => ({ cid, user: user(id), state, joined_at });
const doc = (id, data = {}) => ({ id, ...data, _meta: { version: 1 } });
/** Room frames carry ids "<room key>.<n>"; returns the frames without them. */
const noID = (frames) => frames.map(({ id, ...f }) => f);
const keyOf = (id) => String(id).slice(0, String(id).indexOf("."));

test("no socket until first use; one shared socket per page", () => {
  const env = createEnv();
  const by = env.backyard;
  by.db.collection("posts");
  by.socket.on("open", () => {});
  assert.equal(env.sockets.length, 0);
  assert.equal(by.socket.connected, false);
  by.db.collection("posts").subscribe({});
  by.db.collection("votes").subscribe({});
  by.socket.room("lobby");
  assert.equal(env.sockets.length, 1);
  assert.equal(env.ws().url, "wss://foo.backyard.test/__backyard/api/ws");
});

test("hello: connected, cid/user, open event; frames wait for hello", () => {
  const env = createEnv();
  const by = env.backyard;
  const opened = [];
  by.socket.on("open", (info) => opened.push(plain(info)));
  by.db.collection("posts").subscribe({});
  const ws = env.ws();
  ws.open();
  assert.equal(by.socket.connected, false, "connected only after hello");
  assert.deepEqual(ws.sent, []);
  ws.hello("c_abc", user("alice@acme.com"));
  assert.equal(by.socket.connected, true);
  assert.equal(by.socket.cid, "c_abc");
  assert.equal(by.socket.user.id, "alice@acme.com");
  assert.deepEqual(opened, [{ cid: "c_abc", user: user("alice@acme.com") }]);
  assert.deepEqual(ws.frames("sub"), [{ t: "sub", id: "s1", collection: "posts" }]);
});

test("subscribe dispatches create/update/delete; subscribers share one server subscription", () => {
  const env = createEnv();
  const posts = env.backyard.db.collection("posts");
  const a = [];
  const b = [];
  const unsubA = posts.subscribe({
    onCreate: (d) => a.push(["create", d.id]),
    onUpdate: (d) => a.push(["update", d.id, d.title]),
    onDelete: (id) => a.push(["delete", id]),
    onChange: (e) => a.push(["change", e.op, e.id, e.collection]),
  });
  const unsubB = posts.subscribe((e) => b.push(e.op));
  const ws = env.ws();
  ws.hello();
  assert.equal(ws.frames("sub").length, 1, "multiplexed");
  const sid = ws.frames("sub")[0].id;
  ws.push({ t: "ack", id: sid });
  ws.push({ t: "db", sub: sid, collection: "posts", op: "create", doc: doc("p1"), doc_id: "p1" });
  ws.push({ t: "db", sub: sid, collection: "posts", op: "update", doc: doc("p1", { title: "T" }), doc_id: "p1" });
  ws.push({ t: "db", sub: sid, collection: "posts", op: "delete", doc: null, doc_id: "p1" });
  ws.push({ t: "db", sub: "s999", collection: "posts", op: "create", doc: doc("x"), doc_id: "x" }); // unknown sub
  assert.deepEqual(a, [
    ["create", "p1"],
    ["change", "create", "p1", "posts"],
    ["update", "p1", "T"],
    ["change", "update", "p1", "posts"],
    ["delete", "p1"],
    ["change", "delete", "p1", "posts"],
  ]);
  assert.deepEqual(b, ["create", "update", "delete"]);

  unsubA();
  unsubA(); // idempotent
  assert.equal(ws.frames("unsub").length, 0, "still one subscriber");
  unsubB();
  assert.deepEqual(ws.frames("unsub"), [{ t: "unsub", id: sid }]);
  ws.push({ t: "db", sub: sid, collection: "posts", op: "create", doc: doc("late"), doc_id: "late" });
  assert.deepEqual(b, ["create", "update", "delete"]);
});

test("a throwing handler is reported and does not break dispatch", () => {
  const env = createEnv();
  const posts = env.backyard.db.collection("posts");
  const got = [];
  posts.subscribe({
    onCreate: () => {
      throw new Error("app bug");
    },
  });
  posts.subscribe({ onCreate: (d) => got.push(d.id) });
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: doc("p1"), doc_id: "p1" });
  assert.deepEqual(got, ["p1"]);
  assert.equal(env.errors.length, 1);
  assert.equal(env.errors[0][0].message, "app bug");
});

test("subscription errors go to onError", () => {
  const env = createEnv();
  const errs = [];
  env.backyard.db.collection("bad name").subscribe({ onError: (e) => errs.push([e.code, e.message]) });
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "error", id: "s1", error: { code: "bad_request", message: "invalid collection name" } });
  assert.deepEqual(errs, [["bad_request", "invalid collection name"]]);
});

// --- db events without their document (SPEC §6: the Go server sends
// "doc": null with doc_id when an event exceeds the pub/sub payload limit) ---

/** Records every callback of a subscriber as compact tuples. */
function recorder(out) {
  return {
    onCreate: (d) => out.push(["create", d.id, d.title]),
    onUpdate: (d) => out.push(["update", d.id, d.title]),
    onDelete: (id) => out.push(["delete", id]),
    onChange: (e) => out.push(["change", e.op, e.id, e.doc === null ? null : e.doc.title]),
    onError: (err) => out.push(["error", err.code]),
  };
}

/** GET requests for documents of a collection. */
const docReads = (env, c) => env.requests.filter((r) => r.method === "GET" && r.path.startsWith("/__backyard/api/db/" + c + "/")).map((r) => r.path);

test("a create/update without its document is re-read before onCreate/onUpdate; later events wait, in order", async () => {
  const env = createEnv();
  const pending = [];
  env.on("GET", /^\/__backyard\/api\/db\/posts\/[^/]+$/, (req) => new Promise((resolve) => pending.push({ req, resolve })));
  const posts = env.backyard.db.collection("posts");
  const a = [];
  const b = [];
  posts.subscribe(recorder(a));
  posts.subscribe((e) => b.push([e.op, e.id, e.doc && e.doc.title]));
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: null, doc_id: "a:1" });
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "update", doc: doc("a:1", { title: "v2" }), doc_id: "a:1" });
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: doc("p2", { title: "p2" }), doc_id: "p2" });
  assert.deepEqual(a, [], "nothing is delivered ahead of the pending re-read");
  assert.deepEqual(b, []);
  await flush();
  assert.equal(pending.length, 1, "the subscribers of one event share one GET");
  assert.equal(pending[0].req.path, "/__backyard/api/db/posts/a%3A1");
  assert.equal(pending[0].req.credentials, "same-origin");
  pending[0].resolve(doc("a:1", { title: "v1" }));
  await flush(5);
  assert.deepEqual(a, [
    ["create", "a:1", "v1"],
    ["change", "create", "a:1", "v1"],
    ["update", "a:1", "v2"],
    ["change", "update", "a:1", "v2"],
    ["create", "p2", "p2"],
    ["change", "create", "p2", "p2"],
  ]);
  assert.deepEqual(b, [
    ["create", "a:1", "v1"],
    ["update", "a:1", "v2"],
    ["create", "p2", "p2"],
  ]);
  // Back to synchronous dispatch once nothing is pending.
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "delete", doc: null, doc_id: "p2" });
  assert.deepEqual(a.slice(-2), [
    ["delete", "p2"],
    ["change", "delete", "p2", null],
  ]);
  assert.deepEqual(docReads(env, "posts"), ["/__backyard/api/db/posts/a%3A1"], "deletes and full events are never re-read");
});

test("a re-read that finds the document gone is delivered as a delete, once", async () => {
  const env = createEnv();
  const pending = [];
  env.on("GET", /^\/__backyard\/api\/db\/posts\/[^/]+$/, (req) => new Promise((resolve) => pending.push({ req, resolve })));
  const a = [];
  env.backyard.db.collection("posts").subscribe(recorder(a));
  const ws = env.ws();
  ws.hello();
  // Gone, and no delete is queued: reported as a delete.
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "update", doc: null, doc_id: "p1" });
  await flush();
  pending[0].resolve(apiError(404, "not_found", "document not found"));
  await flush(5);
  assert.deepEqual(a, [
    ["delete", "p1"],
    ["change", "delete", "p1", null],
  ]);
  // Gone, and its delete event is already queued: only that one is delivered.
  a.length = 0;
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: null, doc_id: "p2" });
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "delete", doc: null, doc_id: "p2" });
  await flush();
  assert.equal(pending.length, 2);
  pending[1].resolve(apiError(404, "not_found", "document not found"));
  await flush(5);
  assert.deepEqual(a, [
    ["delete", "p2"],
    ["change", "delete", "p2", null],
  ]);
  assert.deepEqual(env.errors, []);
});

test("a failed re-read goes to onError (or a warning); onChange still sees the event and later events flow", async () => {
  const env = createEnv();
  env.on("GET", "/__backyard/api/db/posts/p1", apiError(500, "internal", "boom"));
  env.on("GET", "/__backyard/api/db/posts/p2", () => new Promise(() => {})); // hangs
  const a = [];
  const posts = env.backyard.db.collection("posts");
  posts.subscribe(recorder(a));
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: null, doc_id: "p1" });
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "update", doc: doc("p0", { title: "x" }), doc_id: "p0" });
  await flush(5);
  assert.deepEqual(a, [
    ["error", "internal"],
    ["change", "create", "p1", null],
    ["update", "p0", "x"],
    ["change", "update", "p0", "x"],
  ]);

  // A re-read that never answers times out (30 s) instead of stalling the subscription.
  a.length = 0;
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "update", doc: null, doc_id: "p2" });
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "delete", doc: null, doc_id: "p0" });
  await flush();
  const read = env.requests.find((r) => r.path === "/__backyard/api/db/posts/p2");
  assert.ok(read && read.signal, "the re-read can be aborted");
  env.clock.tick(29_999);
  await flush();
  assert.deepEqual(a, []);
  env.clock.tick(1);
  await flush(5);
  assert.equal(read.signal.aborted, true, "the timed-out request is aborted");
  assert.deepEqual(a, [
    ["error", "network_error"],
    ["change", "update", "p2", null],
    ["delete", "p0"],
    ["change", "delete", "p0", null],
  ]);

  // Without onError: a console warning; onCreate is still not called with null.
  const created = [];
  env.backyard.db.collection("posts").subscribe({ onCreate: (d) => created.push(d) });
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: null, doc_id: "p1" });
  await flush(5);
  assert.deepEqual(created, []);
  assert.match(env.warnings.join("\n"), /could not read document "p1" of a change to "posts": boom/);
});

test("stop() while a re-read is pending delivers nothing more", async () => {
  const env = createEnv();
  const pending = [];
  env.on("GET", "/__backyard/api/db/posts/p1", () => new Promise((resolve) => pending.push(resolve)));
  const a = [];
  const stop = env.backyard.db.collection("posts").subscribe(recorder(a));
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: null, doc_id: "p1" });
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "delete", doc: null, doc_id: "p9" });
  await flush();
  stop();
  stop(); // idempotent
  assert.deepEqual(ws.frames("unsub"), [{ t: "unsub", id: "s1" }]);
  pending[0](doc("p1", { title: "t" }));
  await flush(5);
  assert.deepEqual(a, []);
});

test("watch() refetches on an event without its document and never reads the document itself", async () => {
  const env = createEnv();
  let lists = 0;
  env.on("GET", "/__backyard/api/db/posts", () => {
    lists++;
    return { docs: [doc("p1", { title: "big" })], has_more: false };
  });
  const results = [];
  const stop = env.backyard.db.collection("posts").watch((docs) => results.push(plain(docs).map((d) => d.title)));
  await flush();
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "db", sub: "s1", collection: "posts", op: "create", doc: null, doc_id: "p1" });
  env.clock.tick(50);
  await flush();
  assert.equal(lists, 2);
  assert.deepEqual(results, [["big"]]);
  assert.deepEqual(docReads(env, "posts"), []);
  stop();
});

test("watch: initial result, debounced refetch on changes, dedupe, stop()", async () => {
  const env = createEnv();
  let version = 0;
  let fetches = 0;
  env.on("GET", "/__backyard/api/db/todos", () => {
    fetches++;
    return { docs: [doc("t1", { v: version })], has_more: false };
  });
  const results = [];
  const stop = env.backyard.db.collection("todos").watch((docs) => results.push(plain(docs)));
  await flush();
  assert.equal(fetches, 1);
  assert.deepEqual(results, [[doc("t1", { v: 0 })]]);

  const ws = env.ws();
  ws.hello();
  ws.push({ t: "ack", id: "s1" }); // subscription confirmed → catch-up refetch
  env.clock.tick(50);
  await flush();
  assert.equal(fetches, 2);
  assert.equal(results.length, 1, "identical result is not delivered twice");

  version = 1;
  for (let i = 0; i < 5; i++) ws.push({ t: "db", sub: "s1", collection: "todos", op: "update", doc: doc("t1"), doc_id: "t1" });
  env.clock.tick(49);
  await flush();
  assert.equal(fetches, 2, "debounced");
  env.clock.tick(1);
  await flush();
  assert.equal(fetches, 3, "a burst of events → one refetch");
  assert.deepEqual(results[1], [doc("t1", { v: 1 })]);

  stop();
  stop();
  assert.deepEqual(ws.frames("unsub"), [{ t: "unsub", id: "s1" }]);
  version = 2;
  ws.push({ t: "db", sub: "s1", collection: "todos", op: "update", doc: doc("t1"), doc_id: "t1" });
  env.clock.tick(100);
  await flush();
  assert.equal(fetches, 3);
});

test("watch with a query uses POST /query; stale responses never overwrite newer ones", async () => {
  const env = createEnv();
  const pending = [];
  env.on("POST", "/__backyard/api/db/todos/query", (req) => new Promise((resolve) => pending.push({ req, resolve })));
  const results = [];
  const todos = env.backyard.db.collection("todos");
  const stop = todos.where("done", "==", false).orderBy("due").watch((docs) => results.push(plain(docs).map((d) => d.id)));
  await flush();
  assert.equal(pending.length, 1);
  assert.deepEqual(pending[0].req.json, { where: [{ field: "done", op: "==", value: false }], order_by: [{ field: "due", dir: "asc" }] });

  const ws = env.ws();
  ws.hello();
  ws.push({ t: "db", sub: "s1", collection: "todos", op: "create", doc: doc("b"), doc_id: "b" });
  env.clock.tick(50);
  await flush();
  assert.equal(pending.length, 2);
  // The newer fetch answers first; the older one must be ignored.
  pending[1].resolve({ docs: [doc("a"), doc("b")], has_more: false });
  await flush();
  pending[0].resolve({ docs: [doc("a")], has_more: false });
  await flush();
  assert.deepEqual(results, [["a", "b"]]);
  stop();
});

test("watch reports fetch errors to onError (or a console warning)", async () => {
  const env = createEnv();
  env.on("GET", "/__backyard/api/db/todos", { error: { code: "forbidden", message: "no" } }); // 200 but irrelevant
  env.on("GET", "/__backyard/api/db/todos", () => new Response('{"error":{"code":"forbidden","message":"no"}}', { status: 403 }));
  const errs = [];
  const stop = env.backyard.db.collection("todos").watch(() => assert.fail("no data expected"), undefined, { onError: (e) => errs.push(e.code) });
  await flush();
  assert.deepEqual(errs, ["forbidden"]);
  stop();
  const stop2 = env.backyard.db.collection("todos").watch(() => {});
  await flush();
  assert.match(env.warnings.join("\n"), /watch\("todos"\) failed/);
  stop2();
});

test("rooms: join with state, joined → self/members (excluding self), presence events", () => {
  const env = createEnv();
  const by = env.backyard;
  const room = by.socket.room("lobby", { state: { color: "red" } });
  assert.equal(by.socket.room("lobby"), room, "one Room per name");
  const events = [];
  room.on("join", (m) => events.push(["join", m.cid]));
  room.on("leave", (m) => events.push(["leave", m.cid]));
  room.on("update", (m) => events.push(["update", m.cid, plain(m.state)]));
  room.on("members", (ms) => events.push(["members", plain(ms).map((m) => m.cid)]));
  assert.equal(room.self, null);
  assert.deepEqual(plain(room.members), []);

  const ws = env.ws();
  ws.hello("c_1", user("alice@acme.com"));
  const join = ws.frames("join")[0];
  assert.deepEqual(join, { t: "join", id: join.id, room: "lobby", state: { color: "red" } });
  ws.push({
    t: "joined",
    id: join.id,
    room: "lobby",
    self: member("c_1", "alice@acme.com", { color: "red" }),
    members: [member("c_3", "carol@acme.com"), member("c_2", "bob@acme.com")],
  });
  assert.equal(room.self.cid, "c_1");
  assert.equal(room.joined, true);
  assert.deepEqual(plain(room.members).map((m) => m.cid), ["c_2", "c_3"], "sorted by joined_at");
  assert.deepEqual(events, [["members", ["c_2", "c_3"]]], "initial membership is reported via members only");

  events.length = 0;
  ws.push({ t: "presence", room: "lobby", op: "join", member: member("c_4", "dan@acme.com") });
  ws.push({ t: "presence", room: "lobby", op: "update", member: member("c_2", "bob@acme.com", { typing: true }) });
  ws.push({ t: "presence", room: "lobby", op: "leave", member: member("c_3", "carol@acme.com") });
  ws.push({ t: "presence", room: "lobby", op: "leave", member: member("c_9", "nobody@acme.com") }); // unknown: ignored
  ws.push({ t: "presence", room: "lobby", op: "update", member: member("c_1", "alice@acme.com", { x: 1 }) }); // self: ignored
  ws.push({ t: "presence", room: "other", op: "join", member: member("c_5", "eve@acme.com") }); // other room
  assert.deepEqual(events, [
    ["join", "c_4"],
    ["members", ["c_2", "c_3", "c_4"]],
    ["update", "c_2", { typing: true }],
    ["members", ["c_2", "c_3", "c_4"]],
    ["leave", "c_3"],
    ["members", ["c_2", "c_4"]],
  ]);
  assert.deepEqual(plain(room.members.find((m) => m.cid === "c_2").state), { typing: true });
});

test("rooms: an update about a member we did not know is reported as a join", () => {
  const env = createEnv();
  const room = env.backyard.socket.room("r");
  const ws = env.ws();
  ws.hello("c_1");
  ws.push({ t: "joined", id: ws.frames("join")[0].id, room: "r", self: member("c_1", "a@x"), members: [] });
  const ev = [];
  room.on("join", (m) => ev.push("join:" + m.cid));
  room.on("update", (m) => ev.push("update:" + m.cid));
  ws.push({ t: "presence", room: "r", op: "update", member: member("c_2", "b@x", { v: 1 }) });
  ws.push({ t: "presence", room: "r", op: "update", member: member("c_2", "b@x", { v: 2 }) });
  ws.push({ t: "presence", room: "r", op: "join", member: member("c_2", "b@x", { v: 3 }) });
  assert.deepEqual(ev, ["join:c_2", "update:c_2", "update:c_2"]);
  assert.deepEqual(plain(room.members).map((m) => m.state.v), [3]);
});

test("rooms: messages, send options, state updates, off, leave", () => {
  const env = createEnv();
  const room = env.backyard.socket.room("game");
  const ws = env.ws();
  ws.hello("c_1");
  const joinID = ws.frames("join")[0].id;
  ws.push({ t: "joined", id: joinID, room: "game", self: member("c_1", "alice@acme.com"), members: [] });

  const msgs = [];
  const handler = (data, from) => msgs.push([plain(data), from.cid, from.user.id]);
  room.on("message", handler);
  ws.push({ t: "msg", room: "game", from: { cid: "c_2", user: user("bob@acme.com") }, data: { move: "e4" } });
  assert.deepEqual(msgs, [[{ move: "e4" }, "c_2", "bob@acme.com"]]);

  room.send({ move: "e5" });
  room.send("hi", { to: "c_2" });
  room.send(null, { echo: true });
  assert.deepEqual(noID(ws.frames("send")), [
    { t: "send", room: "game", data: { move: "e5" } },
    { t: "send", room: "game", data: "hi", to: "c_2" },
    { t: "send", room: "game", data: null, echo: true },
  ]);

  room.setState({ cursor: [1, 2] });
  room.updateState({ name: "A" });
  assert.deepEqual(noID(ws.frames("state")), [
    { t: "state", room: "game", state: { cursor: [1, 2] } },
    { t: "state", room: "game", state: { cursor: [1, 2], name: "A" } },
  ]);
  // Every frame of the room carries a unique id with the room's key (server
  // errors about it reach room.on("error"); the server acks it).
  const ids = ws.frames("send").concat(ws.frames("state")).map((f) => f.id);
  assert.equal(new Set(ids).size, 5);
  for (const id of ids) assert.equal(keyOf(id), keyOf(joinID), id);
  for (const id of ids) ws.push({ t: "ack", id }); // acks are ignored
  assert.deepEqual(env.warnings, []);
  assert.deepEqual(plain(room.self.state), { cursor: [1, 2], name: "A" });
  assert.deepEqual(plain(room.state), { cursor: [1, 2], name: "A" });
  assert.throws(() => room.setState("nope"), { code: "bad_request" });

  room.off("message", handler);
  ws.push({ t: "msg", room: "game", from: { cid: "c_2", user: user("bob@acme.com") }, data: 2 });
  assert.equal(msgs.length, 1);

  const offAll = room.on("message", handler);
  offAll();
  ws.push({ t: "msg", room: "game", from: { cid: "c_2", user: user("bob@acme.com") }, data: 3 });
  assert.equal(msgs.length, 1);

  room.leave();
  room.leave();
  assert.deepEqual(ws.frames("leave"), [{ t: "leave", room: "game" }]);
  // A late answer about a frame of the left room is reported generically.
  ws.push({ t: "error", id: ids[0], error: { code: "not_found", message: "not a member of room \"game\"" } });
  assert.match(env.warnings.join("\n"), /realtime error: not_found/);
  assert.equal(room.self, null);
  assert.deepEqual(plain(room.members), []);
  room.send("ignored");
  assert.equal(ws.frames("send").length, 3);
  assert.notEqual(env.backyard.socket.room("game"), room, "a new Room after leave");
});

test("room names are validated", () => {
  const env = createEnv();
  for (const bad of ["", "has space", "x".repeat(129), 5, null]) {
    assert.throws(() => env.backyard.socket.room(bad), { code: "bad_request" });
  }
  env.backyard.socket.room("ok:room-1_.x");
});

test("frames sent while connecting are queued and flushed after the join, in order", () => {
  const env = createEnv();
  const room = env.backyard.socket.room("q", { state: { a: 1 } });
  room.send(1);
  room.send(2);
  room.setState({ a: 2 }); // not sent separately: carried by the join
  const left = env.backyard.socket.room("gone");
  left.send("dropped");
  left.leave();
  const ws = env.ws();
  ws.hello();
  assert.deepEqual(
    ws.sent.map((f) => [f.t, f.room, f.data ?? f.state]),
    [
      ["join", "q", { a: 2 }],
      ["send", "q", 1],
      ["send", "q", 2],
    ],
  );
});

test("reconnect: close event, exponential backoff with jitter, resubscribe and rejoin with latest state", async () => {
  const env = createEnv();
  const by = env.backyard;
  const status = [];
  by.socket.on("open", () => status.push("open"));
  by.socket.on("close", (e) => status.push("close:" + e.code));
  env.on("GET", "/__backyard/api/db/posts", { docs: [], has_more: false });
  const changes = [];
  by.db.collection("posts").subscribe({ onChange: (e) => changes.push(e.op) });
  const stopWatch = by.db.collection("posts").watch(() => {});
  const room = by.socket.room("lobby", { state: { v: 1 } });
  const ev = [];
  room.on("join", (m) => ev.push("join:" + m.cid));
  room.on("leave", (m) => ev.push("leave:" + m.cid));
  room.on("update", (m) => ev.push("update:" + m.cid));

  let ws = env.ws();
  ws.hello("c_1");
  const subID = ws.frames("sub")[0].id;
  ws.push({ t: "joined", id: ws.frames("join")[0].id, room: "lobby", self: member("c_1", "a@x"), members: [member("c_2", "b@x"), member("c_3", "c@x", { s: 1 })] });
  assert.equal(by.socket.connected, true);

  ws.serverClose(1006);
  assert.equal(by.socket.connected, false);
  assert.equal(by.socket.cid, null);
  assert.deepEqual(status, ["open", "close:1006"]);
  assert.equal(room.joined, false);
  room.setState({ v: 2 }); // while disconnected
  room.send("queued while offline");

  // Backoff: 0.5s ±20%, then doubling, capped at 10s.
  const delays = [];
  for (let attempt = 0; attempt < 8; attempt++) {
    const timer = env.clock.pending().find((t) => !t.every);
    assert.ok(timer, "a reconnect is scheduled");
    delays.push(timer.delay);
    const before = env.sockets.length;
    env.clock.tick(timer.delay);
    assert.equal(env.sockets.length, before + 1, "reconnect attempt " + attempt);
    if (attempt < 7) env.ws().serverClose(1006); // the attempt fails
  }
  const bases = [500, 1000, 2000, 4000, 8000, 10000, 10000, 10000];
  delays.forEach((d, i) => {
    assert.ok(d >= Math.min(10000, bases[i] * 0.8) - 1 && d <= Math.min(10000, bases[i] * 1.2) + 1, `delay ${i} = ${d}`);
  });

  // The 8th attempt succeeds.
  const fetchesBefore = env.requests.length;
  ws = env.ws();
  ws.hello("c_9");
  assert.deepEqual(ws.frames("sub"), [{ t: "sub", id: subID, collection: "posts" }], "same subscription id is restored");
  const rejoin = ws.frames("join")[0];
  assert.deepEqual(rejoin.state, { v: 2 }, "rejoined with the latest presence state");
  assert.deepEqual(noID(ws.frames("send")), [{ t: "send", room: "lobby", data: "queued while offline" }]);
  assert.equal(keyOf(ws.frames("send")[0].id), keyOf(rejoin.id));
  assert.ok(ws.sent.indexOf(rejoin) < ws.sent.findIndex((f) => f.t === "send"), "join precedes queued sends");
  assert.deepEqual(status, ["open", "close:1006", "open"]);

  // The server confirms; the live query catches up on what was missed.
  ws.push({ t: "ack", id: subID });
  env.clock.tick(50);
  await flush();
  assert.equal(env.requests.length, fetchesBefore + 1, "watch refetched after resubscribing");

  // Our own old connection (c_1) lingering on another instance is hidden.
  ws.push({
    t: "joined",
    id: rejoin.id,
    room: "lobby",
    self: member("c_9", "a@x"),
    members: [member("c_1", "a@x"), member("c_3", "c@x", { s: 2 }), member("c_4", "d@x")],
  });
  assert.deepEqual(ev, ["leave:c_2", "update:c_3", "join:c_4"]);
  assert.deepEqual(plain(room.members).map((m) => m.cid), ["c_3", "c_4"]);
  assert.equal(room.self.cid, "c_9");
  ws.push({ t: "db", sub: subID, collection: "posts", op: "create", doc: doc("p"), doc_id: "p" });
  assert.deepEqual(changes, ["create"]);
  stopWatch();
});

test("a stale joined frame (old join id) is ignored", () => {
  const env = createEnv();
  const room = env.backyard.socket.room("r");
  const ws = env.ws();
  ws.hello("c_1");
  ws.push({ t: "joined", id: "j-old", room: "r", self: member("c_1", "a@x"), members: [member("c_2", "b@x")] });
  assert.equal(room.self, null);
  assert.deepEqual(plain(room.members), []);
});

test("join errors are emitted on the room; unmatched errors on socket 'error'", () => {
  const env = createEnv();
  const room = env.backyard.socket.room("full");
  const roomErrs = [];
  room.on("error", (e) => roomErrs.push(e.code));
  const sockErrs = [];
  env.backyard.socket.on("error", (e) => sockErrs.push(e.code));
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "error", id: ws.frames("join")[0].id, error: { code: "rate_limited", message: "too many rooms" } });
  ws.push({ t: "error", error: { code: "rate_limited", message: "too many messages" } });
  ws.push("not json");
  ws.push({ t: "pong" });
  assert.deepEqual(roomErrs, ["rate_limited"]);
  assert.deepEqual(sockErrs, ["rate_limited"]);
});

test("keepalive: ping every 25s; an unanswered ping drops the connection and it reconnects", () => {
  const env = createEnv();
  env.backyard.socket.room("r");
  const ws = env.ws();
  ws.hello();
  env.clock.tick(25_000);
  assert.deepEqual(ws.frames("ping"), [{ t: "ping" }]);
  ws.push({ t: "pong" });
  env.clock.tick(25_000);
  assert.equal(ws.frames("ping").length, 2);
  // No pong (nor any other frame) by the next check: the path is dead.
  env.clock.tick(25_000);
  assert.equal(ws.closedWith && ws.closedWith.code, 4000);
  assert.equal(ws.closedWith.reason, "keepalive timeout");
  assert.equal(env.backyard.socket.connected, false);
  env.clock.tick(700);
  assert.equal(env.sockets.length, 2, "reconnected");
});

test("keepalive: any frame answers a ping; throttled background timers cause no spurious reconnects", () => {
  const env = createEnv();
  const closes = [];
  env.backyard.socket.on("close", (e) => closes.push(e.code));
  env.backyard.socket.room("r");
  const ws = env.ws();
  ws.hello();
  // Background tabs may wake timers only once a minute: emulate a 65 s interval.
  const ping = env.clock.pending().find((t) => t.every);
  const timer = env.clock.timers.get(ping.id);
  timer.every = 65_000;
  timer.at = env.clock.now + 65_000;
  for (let i = 1; i <= 5; i++) {
    env.clock.tick(65_000);
    assert.equal(ws.frames("ping").length, i, "ping " + i);
    if (i % 2) ws.push({ t: "pong" });
    else ws.push({ t: "presence", room: "x", op: "join", member: member("c_7", "z@x") }); // any frame counts
  }
  assert.equal(ws.closedWith, null);
  assert.deepEqual(closes, []);
  assert.equal(env.sockets.length, 1);
});

test("keepalive: a connection that never says hello is dropped and retried", () => {
  const env = createEnv();
  env.backyard.socket.room("r");
  const ws = env.ws();
  env.clock.tick(25_000); // never opened
  assert.equal(ws.closedWith && ws.closedWith.reason, "connect timeout");
  env.clock.tick(700);
  assert.equal(env.sockets.length, 2, "retried");
  const ws2 = env.ws();
  ws2.open(); // opened, but the server never sends hello
  env.clock.tick(25_000);
  assert.equal(ws2.closedWith && ws2.closedWith.reason, "connect timeout");
  assert.deepEqual(ws2.frames("ping"), [], "no pings before hello");
});

test("online and visibilitychange: reconnect at once, or probe a live connection", () => {
  const docListeners = {};
  const document = {
    visibilityState: "hidden",
    addEventListener: (type, fn) => (docListeners[type] ||= []).push(fn),
  };
  const env = createEnv({ document });
  const by = env.backyard;
  by.socket.room("r");
  let ws = env.ws();
  ws.hello();
  // Probe: a ping right away; no answer by the next check drops the connection.
  env.fire("online");
  assert.deepEqual(ws.frames("ping"), [{ t: "ping" }]);
  env.clock.tick(25_000);
  assert.equal(ws.closedWith && ws.closedWith.reason, "keepalive timeout");
  // Backing off: becoming visible reconnects without waiting.
  assert.equal(env.sockets.length, 1);
  document.visibilityState = "visible";
  for (const fn of docListeners.visibilitychange) fn({});
  assert.equal(env.sockets.length, 2, "reconnected on visibilitychange");
  ws = env.ws();
  ws.hello("c_2");
  // Visible again with a healthy connection: a probe ping that is answered.
  for (const fn of docListeners.visibilitychange) fn({});
  assert.equal(ws.frames("ping").length, 1);
  ws.push({ t: "pong" });
  env.clock.tick(25_000);
  assert.equal(ws.closedWith, null);
  assert.equal(by.socket.connected, true);
});

test("disconnect() stops reconnecting until the socket is used again; online reconnects at once", () => {
  const env = createEnv();
  const by = env.backyard;
  const room = by.socket.room("r");
  env.ws().hello();
  by.socket.disconnect();
  assert.equal(by.socket.connected, false);
  env.clock.tick(60_000);
  assert.equal(env.sockets.length, 1);
  by.socket.room("r2");
  assert.equal(env.sockets.length, 2);
  env.ws().hello("c_2");
  assert.deepEqual(env.ws().frames("join").map((f) => f.room).sort(), ["r", "r2"]);
  assert.equal(room.name, "r");

  env.ws().serverClose(1006);
  assert.equal(env.sockets.length, 2);
  env.fire("online");
  assert.equal(env.sockets.length, 3, "the online event reconnects immediately");
});

test("socket.connect() keeps a connection without rooms; on/off for status listeners", () => {
  const env = createEnv();
  const seen = [];
  const fn = () => seen.push("open");
  const off = env.backyard.socket.on("open", fn);
  env.backyard.socket.connect();
  env.ws().hello();
  off();
  env.ws().serverClose(1006);
  env.clock.tick(700);
  env.ws().hello();
  assert.deepEqual(seen, ["open"]);
  assert.throws(() => env.backyard.socket.on("message", () => {}), { code: "bad_request" });
});

test("without WebSocket support the SDK warns instead of throwing", () => {
  const env = createEnv({ noWebSocket: true });
  env.backyard.socket.room("r");
  assert.match(env.warnings.join("\n"), /WebSocket is not available/);
});

test("errors answering a room's send/state/join reach room.on('error'), else socket 'error', else the console", () => {
  const env = createEnv();
  const a = env.backyard.socket.room("a");
  const b = env.backyard.socket.room("b");
  const ws = env.ws();
  ws.hello("c_1");
  for (const j of ws.frames("join")) ws.push({ t: "joined", id: j.id, room: j.room, self: member("c_1", "a@x"), members: [] });
  const aErrs = [];
  a.on("error", (e) => aErrs.push([e.name, e.code, e.message, e instanceof env.backyard.BackyardError]));
  const sockErrs = [];
  const offSock = env.backyard.socket.on("error", (e) => sockErrs.push(e.code));

  // The Go server and the Worker answer frames that carry an id with an ack
  // on success (ignored) or an error frame with the same id.
  a.send("x".repeat(10), { to: "c_404" });
  const send = ws.frames("send")[0];
  ws.push({ t: "error", id: send.id, error: { code: "not_found", message: 'no member "c_404" in room "a"' } });
  a.setState({ big: true });
  const state = ws.frames("state")[0];
  ws.push({ t: "error", id: state.id, error: { code: "too_large", message: "presence state exceeds 8192 bytes" } });
  a.send("ok");
  ws.push({ t: "ack", id: ws.frames("send")[1].id });
  assert.deepEqual(aErrs, [
    ["BackyardError", "not_found", 'no member "c_404" in room "a"', true],
    ["BackyardError", "too_large", "presence state exceeds 8192 bytes", true],
  ]);

  // Room b has no error handler: socket listeners get it, then the console.
  b.send("hi");
  const bID = ws.frames("send").find((f) => f.room === "b").id;
  ws.push({ t: "error", id: bID, error: { code: "too_large", message: "message too large" } });
  assert.deepEqual(sockErrs, ["too_large"]);
  offSock();
  ws.push({ t: "error", id: bID, error: { code: "internal", message: "realtime backend unavailable" } });
  assert.match(env.warnings.join("\n"), /realtime error: internal realtime backend unavailable/);
  // Errors without an id (e.g. rate limiting) never reach a room.
  ws.push({ t: "error", error: { code: "rate_limited", message: "too many messages" } });
  assert.equal(aErrs.length, 2);
  assert.match(env.warnings.join("\n"), /realtime error: rate_limited/);
});

test("join errors of a room without an error handler are not swallowed", () => {
  const env = createEnv();
  env.backyard.socket.room("full");
  const sockErrs = [];
  env.backyard.socket.on("error", (e) => sockErrs.push(e.code));
  const ws = env.ws();
  ws.hello();
  ws.push({ t: "error", id: ws.frames("join")[0].id, error: { code: "rate_limited", message: "too many rooms on this connection (max 50)" } });
  assert.deepEqual(sockErrs, ["rate_limited"]);
});

test("members are ordered by join time whatever the timestamp precision, then by cid", () => {
  const env = createEnv();
  const room = env.backyard.socket.room("t");
  const ws = env.ws();
  ws.hello("c_self");
  const at = (cid, joined_at) => ({ cid, user: user(cid + "@x"), state: {}, joined_at });
  ws.push({
    t: "joined",
    id: ws.frames("join")[0].id,
    room: "t",
    self: at("c_self", "2026-01-01T00:00:00.000Z"),
    members: [
      at("c_nofrac", "2026-01-01T00:00:01Z"), // 1.000 s: before 1.5 s (a string compare gets this wrong)
      at("c_half", "2026-01-01T00:00:01.5Z"),
      at("c_go", "2026-01-01T00:00:01.120Z"), // Go: fixed 3 digits
      at("c_short", "2026-01-01T00:00:01.12Z"), // same instant as c_go: tie broken by cid
      at("c_micro2", "2026-01-01T00:00:01.123457Z"), // Cloudflare edition: 6 digits
      at("c_micro1", "2026-01-01T00:00:01.123456Z"),
      at("c_nano", "2026-01-01T00:00:01.1234565Z"),
      at("c_offset", "2026-01-01T01:00:00.900+01:00"), // = 00:00:00.900Z
      at("c_lower", "2026-01-01t00:00:00.950z"),
      at("c_bad", "yesterday"),
      at("c_missing", undefined),
    ],
  });
  assert.deepEqual(
    plain(room.members).map((m) => m.cid),
    ["c_offset", "c_lower", "c_nofrac", "c_go", "c_short", "c_micro1", "c_nano", "c_micro2", "c_half", "c_bad", "c_missing"],
  );
  // Presence updates keep the order (a late joiner goes last, ties by cid).
  ws.push({ t: "presence", room: "t", op: "join", member: at("c_aaa", "2026-01-01T00:00:01.5Z") });
  assert.deepEqual(plain(room.members).map((m) => m.cid).slice(7, 10), ["c_micro2", "c_aaa", "c_half"]);
});
