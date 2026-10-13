// Realtime websocket protocol (SPEC §6) with several clients.
import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { setup, uniqueSite } from "../lib/env.mjs";
import { Client, expectStatus } from "../lib/http.mjs";
import { assertDoc, assertSummary, assertUser, JOINED_AT } from "../lib/shapes.mjs";
import { handshake, Socket } from "../lib/ws.mjs";

const env = await setup();
const alice = new Client();
const bob = new Client();
const carol = new Client();
const skip = !canDeploy(env, alice) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";
const site = uniqueSite("ws");
const db = (p) => env.api(site, "/db" + p);

const isErr = (id, code) => (f) => f.t === "error" && (id === undefined || f.id === id) && (code === undefined || f.error?.code === code);

/** Asserts a Member (SPEC §6). */
function assertMember(m, { cid, user, state, what = "member" } = {}) {
  assert.ok(m && typeof m === "object", `${what}: ${JSON.stringify(m)}`);
  assert.deepEqual(Object.keys(m).sort(), ["cid", "joined_at", "state", "user"], `${what} keys`);
  assert.match(m.cid, /^c_/, `${what}.cid`);
  if (cid !== undefined) assert.equal(m.cid, cid, `${what}.cid`);
  if (user) assertSummary(m.user, user, `${what}.user`);
  if (state !== undefined) assert.deepEqual(m.state, state, `${what}.state`);
  assert.match(m.joined_at, JOINED_AT, `${what}.joined_at`);
}

describe("websocket", { skip }, () => {
  let users;
  const open = [];
  const connect = async (client, opts) => {
    const s = await Socket.open(env, client, site, opts);
    open.push(s);
    return s;
  };

  before(async () => {
    await deploySite(alice, env, site, { "index.html": "ws" });
    users = {};
    for (const [name, c] of Object.entries({ alice, bob, carol })) {
      users[name] = assertUser(expectStatus(await c.get(env.api(site, "/me")), 200).json);
    }
  });

  after(async () => {
    await Promise.all(open.map((s) => s.close()));
  });

  test("hello is the first frame; ping → pong", async () => {
    const a = await connect(alice);
    assert.equal(a.hello.t, "hello");
    assert.match(a.hello.cid, /^c_[0-9a-zA-Z]+$/);
    assert.deepEqual(a.hello.user, users.alice, "hello carries the User of /me");
    const b = await connect(alice);
    assert.notEqual(b.hello.cid, a.hello.cid, "every connection has its own cid");
    a.send({ t: "ping" });
    assert.deepEqual(await a.next((f) => f.t === "pong"), { t: "pong" });
  });

  test("db subscriptions receive the events of HTTP writes", async () => {
    const a = await connect(alice);
    assert.deepEqual(await a.request({ t: "sub", id: "s1", collection: "posts" }, (f) => f.t === "ack"), { t: "ack", id: "s1" });
    // Another collection is not delivered to s1.
    await a.request({ t: "sub", id: "s2", collection: "other" }, (f) => f.t === "ack" && f.id === "s2");

    const created = expectStatus(await bob.post(db("/posts"), { title: "hi" }), 201).json;
    const ev = await a.next((f) => f.t === "db" && f.op === "create", { what: "create event" });
    assert.deepEqual(Object.keys(ev).sort(), ["collection", "doc", "doc_id", "op", "sub", "t"]);
    assert.equal(ev.sub, "s1");
    assert.equal(ev.collection, "posts");
    assert.equal(ev.doc_id, created.id);
    assertDoc(ev.doc, { id: created.id, data: { title: "hi" }, version: 1, by: users.bob.id });
    assert.deepEqual(ev.doc, created, "the event carries the flattened document");

    const patched = expectStatus(await bob.patch(db(`/posts/${created.id}`), { title: "hello" }), 200).json;
    const up = await a.next((f) => f.t === "db" && f.op === "update", { what: "update event" });
    assert.deepEqual(up.doc, patched);

    const put = expectStatus(await bob.put(db(`/posts/${created.id}`), { title: "replaced" }), 200).json;
    assert.deepEqual((await a.next((f) => f.t === "db" && f.op === "update", { what: "update event (PUT)" })).doc, put);

    const fresh = expectStatus(await bob.put(db("/posts/by-put"), { a: 1 }), 200).json;
    const c2 = await a.next((f) => f.t === "db" && f.op === "create", { what: "create event (PUT)" });
    assert.equal(c2.doc_id, "by-put");
    assert.deepEqual(c2.doc, fresh);

    expectStatus(await bob.delete(db(`/posts/${created.id}`)), 204);
    const del = await a.next((f) => f.t === "db" && f.op === "delete", { what: "delete event" });
    assert.equal(del.doc, null);
    assert.equal(del.doc_id, created.id);
    assert.equal(del.sub, "s1");

    expectStatus(await bob.delete(db("/posts/never-existed")), 204);
    expectStatus(await bob.post(db("/other"), { x: 1 }), 201);
    const o = await a.next((f) => f.t === "db" && f.collection === "other", { what: "event of s2" });
    assert.equal(o.sub, "s2");
    await a.sync();
    a.assertNone((f) => f.t === "db", "event (a missing document's delete publishes nothing; s1 gets only posts)");

    // Re-using an id for another collection replaces the subscription.
    await a.request({ t: "sub", id: "s3", collection: "first" }, (f) => f.t === "ack" && f.id === "s3");
    await a.request({ t: "sub", id: "s3", collection: "second" }, (f) => f.t === "ack" && f.id === "s3");
    expectStatus(await bob.post(db("/first"), { n: 1 }), 201);
    expectStatus(await bob.post(db("/second"), { n: 2 }), 201);
    const moved = await a.next((f) => f.t === "db" && f.sub === "s3", { what: "event of the replaced subscription" });
    assert.equal(moved.collection, "second");
    await a.sync();
    a.assertNone((f) => f.t === "db" && f.collection === "first", "event of the replaced collection");
    await a.request({ t: "unsub", id: "s3" }, (f) => f.t === "ack" && f.id === "s3");

    assert.deepEqual(await a.request({ t: "unsub", id: "s1" }, (f) => f.t === "ack" && f.id === "s1"), { t: "ack", id: "s1" });
    expectStatus(await bob.post(db("/posts"), { after: "unsub" }), 201);
    expectStatus(await bob.post(db("/other"), { y: 1 }), 201);
    await a.next((f) => f.t === "db" && f.collection === "other", { what: "event of s2 after unsub of s1" });
    await a.sync();
    a.assertNone((f) => f.t === "db" && f.sub === "s1", "event after unsub");
  });

  test("subscription errors", async () => {
    const a = await connect(alice);
    const sys = await a.request({ t: "sub", id: "x1", collection: "_files" }, isErr("x1"));
    assert.ok(["forbidden", "bad_request"].includes(sys.error.code), JSON.stringify(sys));
    assert.equal((await a.request({ t: "sub", id: "x2", collection: "bad name" }, isErr("x2"))).error.code, "bad_request");
    assert.equal((await a.request({ t: "sub", collection: "posts" }, isErr())).error.code, "bad_request", "sub needs an id");
    await a.sync();
  });

  test("rooms: join, presence, state, messages, direct messages, leave", async () => {
    const room = "lobby";
    const a = await connect(alice);
    const b = await connect(bob);
    const c = await connect(carol);

    const ja = await a.request({ t: "join", id: "j1", room, state: { color: "red" } }, (f) => f.t === "joined");
    assert.equal(ja.id, "j1");
    assert.equal(ja.room, room);
    assertMember(ja.self, { cid: a.hello.cid, user: users.alice, state: { color: "red" } });
    assert.deepEqual(ja.members, [], "members excludes self");

    const jb = await b.request({ t: "join", id: "j2", room }, (f) => f.t === "joined");
    assertMember(jb.self, { cid: b.hello.cid, user: users.bob, state: {} });
    assert.equal(jb.members.length, 1);
    assertMember(jb.members[0], { cid: a.hello.cid, user: users.alice, state: { color: "red" } });
    const pj = await a.next((f) => f.t === "presence" && f.op === "join", { what: "presence join" });
    assert.deepEqual(Object.keys(pj).sort(), ["member", "op", "room", "t"]);
    assert.equal(pj.room, room);
    assert.deepEqual(pj.member, jb.self);

    const jc = await c.request({ t: "join", id: "j3", room, state: { n: 3 } }, (f) => f.t === "joined");
    assert.deepEqual(jc.members.map((m) => m.cid), [a.hello.cid, b.hello.cid], "members sorted by joined_at, then cid");
    const sorted = [...jc.members].sort((x, y) => (x.joined_at < y.joined_at ? -1 : x.joined_at > y.joined_at ? 1 : x.cid < y.cid ? -1 : 1));
    assert.deepEqual(jc.members, sorted);
    await a.next((f) => f.t === "presence" && f.op === "join" && f.member.cid === c.hello.cid);
    await b.next((f) => f.t === "presence" && f.op === "join" && f.member.cid === c.hello.cid);

    // state replaces the presence state; with an id it is acknowledged.
    assert.deepEqual(await b.request({ t: "state", id: "st1", room, state: { color: "blue" } }, (f) => f.t === "ack"), { t: "ack", id: "st1" });
    const pu = await a.next((f) => f.t === "presence" && f.op === "update", { what: "presence update" });
    assertMember(pu.member, { cid: b.hello.cid, state: { color: "blue" } });
    await c.next((f) => f.t === "presence" && f.op === "update");

    // Broadcast: everyone but the sender.
    a.send({ t: "send", room, data: { text: "hi all", n: [1, 2] } });
    for (const s of [b, c]) {
      const m = await s.next((f) => f.t === "msg", { what: "broadcast" });
      assert.deepEqual(Object.keys(m).sort(), ["data", "from", "room", "t"]);
      assert.equal(m.room, room);
      assert.deepEqual(m.data, { text: "hi all", n: [1, 2] });
      assert.equal(m.from.cid, a.hello.cid);
      assertSummary(m.from.user, users.alice, "msg.from.user");
    }
    await a.sync();
    a.assertNone((f) => f.t === "msg", "echo of an own message without echo:true");

    // echo:true includes the sender; an id acknowledges the send.
    assert.deepEqual(await a.request({ t: "send", id: "m1", room, data: "again", echo: true }, (f) => f.t === "ack"), { t: "ack", id: "m1" });
    assert.equal((await a.next((f) => f.t === "msg", { what: "echo" })).data, "again");
    for (const s of [b, c]) assert.equal((await s.next((f) => f.t === "msg")).data, "again");

    // Direct message: only the recipient (plus the sender with echo).
    a.send({ t: "send", room, to: b.hello.cid, data: { secret: 1 } });
    const dm = await b.next((f) => f.t === "msg", { what: "direct message" });
    assert.deepEqual(dm.data, { secret: 1 });
    a.send({ t: "send", room, to: b.hello.cid, data: "dm-echo", echo: true });
    assert.equal((await a.next((f) => f.t === "msg", { what: "direct echo" })).data, "dm-echo");
    assert.equal((await b.next((f) => f.t === "msg")).data, "dm-echo");
    await c.sync();
    c.assertNone((f) => f.t === "msg", "direct message to someone else");

    // Messages without data carry null.
    b.send({ t: "send", room });
    assert.equal((await a.next((f) => f.t === "msg" && f.from.cid === b.hello.cid)).data, null);
    c.drain((f) => f.t === "msg");

    // Errors about recipients and rooms.
    assert.equal((await a.request({ t: "send", id: "e1", room, to: "c_00000000000000000000", data: 1 }, isErr("e1"))).error.code, "not_found");
    assert.equal((await a.request({ t: "send", id: "e2", room, to: "nobody", data: 1 }, isErr("e2"))).error.code, "bad_request");
    assert.equal((await a.request({ t: "send", id: "e3", room: "elsewhere", data: 1 }, isErr("e3"))).error.code, "not_found");
    assert.equal((await a.request({ t: "state", id: "e4", room: "elsewhere", state: {} }, isErr("e4"))).error.code, "not_found");
    assert.equal((await a.request({ t: "join", id: "e5", room: "bad room!" }, isErr("e5"))).error.code, "bad_request");

    // Re-joining replaces the state and answers with joined again.
    const again = await a.request({ t: "join", id: "j4", room, state: { color: "green" } }, (f) => f.t === "joined" && f.id === "j4");
    assertMember(again.self, { cid: a.hello.cid, state: { color: "green" } });
    assert.equal(again.self.joined_at, ja.self.joined_at, "re-joining keeps joined_at");
    assert.equal(again.members.length, 2);
    await b.next((f) => f.t === "presence" && f.op === "update" && f.member.cid === a.hello.cid);
    await c.next((f) => f.t === "presence" && f.op === "update" && f.member.cid === a.hello.cid);

    // leave (acknowledged with an id) → presence leave for the others.
    assert.deepEqual(await a.request({ t: "leave", id: "l1", room }, (f) => f.t === "ack"), { t: "ack", id: "l1" });
    for (const s of [b, c]) {
      const pl = await s.next((f) => f.t === "presence" && f.op === "leave", { what: "presence leave" });
      assert.equal(pl.member.cid, a.hello.cid);
    }
    assert.equal((await a.request({ t: "send", id: "e6", room, data: 1 }, isErr("e6"))).error.code, "not_found", "send after leave");

    // A closed connection leaves its rooms.
    await c.close();
    const gone = await b.next((f) => f.t === "presence" && f.op === "leave" && f.member.cid === c.hello.cid, { what: "leave on disconnect" });
    assertMember(gone.member, { cid: c.hello.cid, user: users.carol });
  });

  test("invalid frames are answered with errors and never close the connection", async () => {
    const a = await connect(alice);
    const bad = [
      ["not json", "bad_request"],
      ["[1,2]", "bad_request"],
      [JSON.stringify({ id: "n1" }), "bad_request"],
      [JSON.stringify({ t: "bogus", id: "n2" }), "bad_request"],
      [JSON.stringify({ t: "join", id: "n3", room: "" }), "bad_request"],
      [JSON.stringify({ t: "join", id: "n4", room: "x".repeat(129) }), "bad_request"],
      [JSON.stringify({ t: "join", id: "n5", room: "ok", state: [1] }), "bad_request"],
    ];
    for (const [frame, code] of bad) {
      a.send(frame);
      const e = await a.next((f) => f.t === "error", { what: `error for ${frame}` });
      assert.equal(e.error.code, code, `${frame}: ${JSON.stringify(e)}`);
      assert.equal(typeof e.error.message, "string");
    }
    assert.equal((await a.request({ t: "sub", id: { nested: 1 }, collection: "posts" }, isErr())).error.code, "bad_request", "object id");
    assert.deepEqual(await a.request({ t: "sub", id: 42, collection: "posts" }, (f) => f.t === "ack"), { t: "ack", id: 42 }, "numeric ids are echoed as numbers");
    const nul = await a.request({ t: "join", id: "n8", room: "nullstate", state: null }, (f) => f.id === "n8");
    assert.equal(nul.t, "joined", JSON.stringify(nul));
    assert.deepEqual(nul.self.state, {}, "a null state is the empty state");
    const big = await a.request({ t: "join", id: "n6", room: "big", state: { blob: "x".repeat(9 * 1024) } }, isErr("n6"));
    assert.equal(big.error.code, "too_large", "presence state above 8 KiB");
    a.send({ t: "send", id: "n7", room: "nowhere", data: "x".repeat(66 * 1024) });
    assert.equal((await a.next(isErr(undefined, "too_large"), { what: "too_large for a frame above 64 KiB" })).error.code, "too_large");
    await a.sync();
    assert.equal(a.readyState, WebSocket.OPEN);
  });

  test("per-connection limits: 100 subscriptions and 50 rooms", async () => {
    const a = await connect(alice);
    for (let i = 0; i < 100; i++) a.send({ t: "sub", id: `cap${i}`, collection: "capped" });
    for (let i = 0; i < 100; i++) await a.next((f) => f.t === "ack" && f.id === `cap${i}`, { what: `ack cap${i}` });
    assert.equal((await a.request({ t: "sub", id: "cap100", collection: "capped" }, isErr("cap100"))).error.code, "rate_limited");
    assert.deepEqual(await a.request({ t: "sub", id: "cap7", collection: "capped" }, (f) => f.t === "ack" && f.id === "cap7"), { t: "ack", id: "cap7" }, "re-subscribing an id is not a new subscription");
    await a.request({ t: "unsub", id: "cap3" }, (f) => f.t === "ack" && f.id === "cap3");
    await a.request({ t: "sub", id: "cap100", collection: "capped" }, (f) => f.t === "ack" && f.id === "cap100");

    const b = await connect(alice);
    for (let i = 0; i < 50; i++) b.send({ t: "join", id: `room${i}`, room: `cap-room-${i}` });
    for (let i = 0; i < 50; i++) await b.next((f) => f.t === "joined" && f.id === `room${i}`, { what: `joined room${i}` });
    assert.equal((await b.request({ t: "join", id: "room50", room: "cap-room-50" }, isErr("room50"))).error.code, "rate_limited");
    await b.request({ t: "leave", id: "l0", room: "cap-room-0" }, (f) => f.t === "ack" && f.id === "l0");
    await b.request({ t: "join", id: "room51", room: "cap-room-50" }, (f) => f.t === "joined" && f.id === "room51");
  });

  test("message rate limit (60/s, burst 120) answers rate_limited", async () => {
    const a = await connect(alice);
    for (let i = 0; i < 300; i++) a.send({ t: "unsub", id: `r${i}` });
    const e = await a.next(isErr(undefined, "rate_limited"), { what: "rate_limited", timeout: 15_000 });
    assert.match(e.error.message, /./);
    await new Promise((r) => setTimeout(r, 2500));
    a.drain();
    await a.sync();
    assert.equal(a.readyState, WebSocket.OPEN, "rate limiting drops frames, it does not close the connection");
  });

  test("Origin check on the handshake", async () => {
    const ok = await handshake(env, alice, site, { origin: env.siteOrigin(site) });
    assert.equal(ok.status, 101, "own origin");
    const none = await handshake(env, alice, site, {});
    assert.equal(none.status, 101, "no Origin (non-browser client)");
    const evil = await handshake(env, alice, site, { origin: "https://evil.example" });
    assert.equal(evil.status, 403, "foreign origin");
    if (env.mode === "host") {
      const other = await handshake(env, alice, site, { origin: env.siteOrigin(uniqueSite("other")) });
      assert.equal(other.status, 403, "another site's origin");
    } else {
      const hub = await handshake(env, alice, site, { origin: env.hubOrigin });
      assert.equal(hub.status, 101, "path mode: the hub origin");
    }
  });

  test("connections per user are capped (HTTP 429 before the upgrade)", { skip: Boolean(alice.token) && "BACKYARD_TOKEN: every client is the same user" }, async () => {
    const guest = new Client({ token: null });
    const me = (await guest.get(env.api(site, "/me"))).json;
    if (!me.anonymous) return; // not a throwaway identity
    const socks = [];
    try {
      let refused = null;
      for (let i = 0; i < 60 && !refused; i++) {
        try {
          socks.push(await Socket.open(env, guest, site));
        } catch {
          refused = await handshake(env, guest, site, { origin: env.siteOrigin(site) });
        }
      }
      assert.ok(socks.length >= 1 && socks.length <= 50, `the cap allowed ${socks.length} connections`);
      refused ??= await handshake(env, guest, site, { origin: env.siteOrigin(site) });
      assert.equal(refused.status, 429, `connection ${socks.length + 1}`);
      assert.ok(refused.headers["retry-after"], "Retry-After");
      assert.equal(JSON.parse(refused.body.toString()).error.code, "rate_limited");
    } finally {
      await Promise.all(socks.map((s) => s.close()));
    }
  });
});
