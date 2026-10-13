/** Realtime websocket protocol (SPEC §6) on the site Durable Object. */
import { describe, expect, it } from "vitest";
import { anonCookie, apiHeaders, call, uniqueSite } from "./helpers";

type Frame = Record<string, unknown> & { t: string };

/** A test client collecting frames. */
class Client {
  readonly frames: Frame[] = [];
  private waiters: { pred: (f: Frame) => boolean; resolve: (f: Frame) => void; reject: (e: Error) => void }[] = [];
  closed = false;

  constructor(readonly ws: WebSocket) {
    ws.accept();
    ws.addEventListener("message", (ev) => {
      const f = JSON.parse(String(ev.data)) as Frame;
      this.frames.push(f);
      this.waiters = this.waiters.filter((w) => {
        if (w.pred(f)) {
          w.resolve(f);
          return false;
        }
        return true;
      });
    });
    ws.addEventListener("close", () => {
      this.closed = true;
    });
  }

  send(f: unknown): void {
    this.ws.send(typeof f === "string" ? f : JSON.stringify(f));
  }

  /** Waits for the next frame (already received or future) matching pred. */
  next(pred: (f: Frame) => boolean, timeoutMs = 3000): Promise<Frame> {
    const idx = this.frames.findIndex(pred);
    if (idx >= 0) return Promise.resolve(this.frames.splice(idx, 1)[0]!);
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`timed out waiting for a frame; got ${JSON.stringify(this.frames)}`)), timeoutMs);
      this.waiters.push({
        pred,
        resolve: (f) => {
          clearTimeout(timer);
          this.frames.splice(this.frames.indexOf(f), 1);
          resolve(f);
        },
        reject,
      });
    });
  }

  t(type: string, extra: (f: Frame) => boolean = () => true): Promise<Frame> {
    return this.next((f) => f.t === type && extra(f));
  }
}

async function guestCookie(): Promise<string> {
  return anonCookie(await call("/__backyard/api/me"));
}

async function connect(site: string, cookie: string, headers: Record<string, string> = {}): Promise<Client> {
  const res = await call(`/s/${site}/__backyard/api/ws`, { headers: { Upgrade: "websocket", Cookie: cookie, ...headers } });
  expect(res.status).toBe(101);
  return new Client(res.webSocket!);
}

describe("websocket protocol", () => {
  it("greets, subscribes and delivers database events", async () => {
    const site = uniqueSite("ws");
    const cookie = await guestCookie();
    const c = await connect(site, cookie);
    const hello = await c.t("hello");
    expect(hello.cid).toMatch(/^c_[0-9a-f]{20}$/);
    expect((hello.user as { id: string }).id).toMatch(/^anon:/);

    c.send({ t: "sub", id: "s1", collection: "posts" });
    expect(await c.t("ack")).toEqual({ t: "ack", id: "s1" });
    c.send({ t: "sub", id: 7, collection: "other" });
    expect(await c.t("ack")).toEqual({ t: "ack", id: 7 });

    const created = await call(`/s/${site}/__backyard/api/db/posts`, { method: "POST", headers: apiHeaders({ Cookie: cookie }), body: JSON.stringify({ id: "p1", title: "x" }) });
    expect(created.status).toBe(201);
    const ev = await c.t("db");
    expect(ev).toMatchObject({ t: "db", sub: "s1", collection: "posts", op: "create", doc_id: "p1" });
    expect((ev.doc as { title: string }).title).toBe("x");
    expect(Object.keys(ev)).toEqual(["t", "sub", "collection", "op", "doc", "doc_id"]);

    await call(`/s/${site}/__backyard/api/db/posts/p1`, { method: "PATCH", headers: apiHeaders(), body: JSON.stringify({ title: "y" }) });
    expect(await c.t("db")).toMatchObject({ op: "update", doc_id: "p1" });
    await call(`/s/${site}/__backyard/api/db/posts/p1`, { method: "DELETE", headers: { "X-Backyard": "1" } });
    expect(await c.t("db")).toEqual({ t: "db", sub: "s1", collection: "posts", op: "delete", doc: null, doc_id: "p1" });

    c.send({ t: "unsub", id: "s1" });
    expect(await c.t("ack")).toEqual({ t: "ack", id: "s1" });
    await call(`/s/${site}/__backyard/api/db/posts`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ id: "p2" }) });
    c.send({ t: "ping", id: 1 });
    expect(await c.t("pong")).toEqual({ t: "pong" });
    expect(c.frames.filter((f) => f.t === "db")).toEqual([]);

    // Errors never close the connection.
    c.send({ t: "sub", collection: "posts" });
    expect(await c.t("error")).toMatchObject({ error: { code: "bad_request" } });
    c.send({ t: "sub", id: "s2", collection: "_files" });
    expect(await c.t("error")).toMatchObject({ id: "s2", error: { code: "forbidden" } });
    c.send({ t: "nope", id: "z" });
    expect(await c.t("error")).toMatchObject({ id: "z", error: { code: "bad_request" } });
    c.send("not json");
    expect(await c.t("error")).toMatchObject({ error: { code: "bad_request" } });
    c.send({ t: "send", room: "lobby", data: 1 });
    expect(await c.t("error")).toMatchObject({ error: { code: "not_found" } });
    c.send(JSON.stringify({ t: "ping", pad: "x".repeat(70 * 1024) }));
    expect(await c.t("error")).toMatchObject({ error: { code: "too_large" } });
    c.send({ t: "ping" });
    expect(await c.t("pong")).toEqual({ t: "pong" });
    expect(c.closed).toBe(false);
    c.ws.close(1000, "bye");
  });

  it("joins rooms with presence and relays messages", async () => {
    const site = uniqueSite("room");
    const a = await connect(site, await guestCookie());
    const b = await connect(site, await guestCookie());
    const helloA = await a.t("hello");
    const helloB = await b.t("hello");

    a.send({ t: "join", id: "r1", room: "lobby", state: { color: "red" } });
    const joinedA = await a.t("joined");
    expect(joinedA).toMatchObject({ t: "joined", id: "r1", room: "lobby", members: [] });
    const selfA = joinedA.self as { cid: string; user: { id: string; name: string }; state: unknown; joined_at: string };
    expect(selfA.cid).toBe(helloA.cid);
    expect(selfA.state).toEqual({ color: "red" });
    expect(selfA.joined_at).toMatch(/^\d{4}-\d\d-\d\dT.*Z$/);
    expect(Object.keys(selfA.user).sort()).toEqual(["id", "name"]);

    b.send({ t: "join", id: "r2", room: "lobby" });
    const joinedB = await b.t("joined");
    expect((joinedB.members as { cid: string }[]).map((m) => m.cid)).toEqual([helloA.cid]);
    expect((joinedB.self as { state: unknown }).state).toEqual({});
    const pj = await a.t("presence");
    expect(pj).toMatchObject({ t: "presence", room: "lobby", op: "join", member: { cid: helloB.cid, state: {} } });

    b.send({ t: "state", id: "st", room: "lobby", state: { color: "blue" } });
    expect(await b.t("ack")).toEqual({ t: "ack", id: "st" });
    expect(await a.t("presence")).toMatchObject({ op: "update", member: { cid: helloB.cid, state: { color: "blue" } } });

    // broadcast (no echo), echo, direct
    a.send({ t: "send", room: "lobby", data: { hi: 1 } });
    const m1 = await b.t("msg");
    expect(m1).toMatchObject({ t: "msg", room: "lobby", from: { cid: helloA.cid }, data: { hi: 1 } });
    a.send({ t: "send", room: "lobby", data: "echo", echo: true, id: "m2" });
    expect(await a.t("msg")).toMatchObject({ data: "echo" });
    expect(await a.t("ack")).toEqual({ t: "ack", id: "m2" });
    expect(await b.t("msg")).toMatchObject({ data: "echo" });
    b.send({ t: "send", room: "lobby", to: helloA.cid, data: "psst" });
    expect(await a.t("msg")).toMatchObject({ data: "psst", from: { cid: helloB.cid } });
    b.send({ t: "send", room: "lobby", to: "c_ffff", data: "x", id: "bad" });
    expect(await b.t("error")).toMatchObject({ id: "bad", error: { code: "not_found" } });
    b.send({ t: "send", room: "lobby", to: "nope", data: "x" });
    expect(await b.t("error")).toMatchObject({ error: { code: "bad_request" } });

    // invalid room, oversized presence
    a.send({ t: "join", id: "j", room: "bad room!" });
    expect(await a.t("error")).toMatchObject({ id: "j", error: { code: "bad_request" } });
    a.send({ t: "join", id: "j2", room: "big", state: { blob: "x".repeat(9000) } });
    expect(await a.t("error")).toMatchObject({ id: "j2", error: { code: "too_large" } });
    a.send({ t: "join", id: "j3", room: "lobby", state: [1] });
    expect(await a.t("error")).toMatchObject({ id: "j3", error: { code: "bad_request" } });

    // leave and disconnect
    b.send({ t: "leave", room: "lobby", id: "l" });
    expect(await b.t("ack")).toEqual({ t: "ack", id: "l" });
    expect(await a.t("presence")).toMatchObject({ op: "leave", member: { cid: helloB.cid } });
    b.send({ t: "join", id: "again", room: "lobby" });
    await b.t("joined");
    await a.t("presence", (f) => f.op === "join");
    b.ws.close(1000, "bye");
    expect(await a.t("presence")).toMatchObject({ op: "leave", member: { cid: helloB.cid } });
    a.ws.close(1000, "bye");
  });

  it("checks the Origin and requires an upgrade", async () => {
    const site = uniqueSite("orig");
    const cookie = await guestCookie();
    let res = await call(`/s/${site}/__backyard/api/ws`, { headers: { Upgrade: "websocket", Cookie: cookie, Origin: "https://evil.example" } });
    expect(res.status).toBe(403);
    res = await call(`/s/${site}/__backyard/api/ws`, { headers: { Upgrade: "websocket", Cookie: cookie, Origin: "https://backyard.test" } });
    expect(res.status).toBe(101);
    res.webSocket!.accept();
    res.webSocket!.close(1000, "bye");
    res = await call(`/s/${site}/__backyard/api/ws`, { headers: { Cookie: cookie } });
    expect(res.status).toBe(426);
  });
});
