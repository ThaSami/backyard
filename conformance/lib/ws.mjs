// Websocket helper (SPEC §6) on Node's built-in WebSocket: queues incoming
// frames so tests can wait for the next frame matching a predicate.
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { rawRequest } from "./http.mjs";

/** The websocket URL of a site. */
export function wsURL(env, site) {
  return env.api(site, "/ws").replace(/^http/, "ws");
}

/** One websocket connection. */
export class Socket {
  #ws;
  #queue = [];
  #waiters = [];
  #closed;
  closeEvent = null;
  hello = null;

  constructor(ws) {
    this.#ws = ws;
    this.#closed = new Promise((resolve) => {
      ws.addEventListener("close", (ev) => {
        this.closeEvent = { code: ev.code, reason: ev.reason };
        resolve(this.closeEvent);
        this.#flush();
      });
    });
    ws.addEventListener("message", (ev) => {
      let frame;
      try {
        frame = typeof ev.data === "string" ? JSON.parse(ev.data) : { binary: true };
      } catch {
        frame = { unparsable: String(ev.data) };
      }
      this.#queue.push(frame);
      this.#flush();
    });
  }

  /**
   * Opens a websocket as client (its cookies and token) with the given
   * Origin (default: the site's own origin) and waits for "hello".
   */
  static async open(env, client, site, { origin } = {}) {
    const url = wsURL(env, site);
    const headers = { Origin: origin === undefined ? env.siteOrigin(site) : origin };
    const cookie = client.jar.header(url.replace(/^ws/, "http"));
    if (cookie) headers.Cookie = cookie;
    if (client.token) headers.Authorization = `Bearer ${client.token}`;
    if (origin === null) delete headers.Origin;
    const ws = new WebSocket(url, { headers });
    const s = new Socket(ws);
    await new Promise((resolve, reject) => {
      const t = setTimeout(() => reject(new Error(`websocket ${url}: no connection within 15s`)), 15_000);
      ws.addEventListener("open", () => {
        clearTimeout(t);
        resolve();
      });
      ws.addEventListener("error", (e) => {
        clearTimeout(t);
        reject(new Error(`websocket ${url}: ${e.message || "connection failed"}`));
      });
    });
    s.hello = await s.next((f) => f.t === "hello", { what: "hello" });
    return s;
  }

  #flush() {
    for (const w of [...this.#waiters]) {
      const i = this.#queue.findIndex(w.pred);
      if (i >= 0) {
        const [f] = this.#queue.splice(i, 1);
        this.#waiters.splice(this.#waiters.indexOf(w), 1);
        clearTimeout(w.timer);
        w.resolve(f);
      } else if (this.closeEvent) {
        this.#waiters.splice(this.#waiters.indexOf(w), 1);
        clearTimeout(w.timer);
        w.reject(new Error(`websocket closed (${this.closeEvent.code} ${this.closeEvent.reason}) while waiting for ${w.what}; queued: ${JSON.stringify(this.#queue).slice(0, 500)}`));
      }
    }
  }

  /** Sends a frame (object → JSON; strings are sent verbatim). */
  send(frame) {
    this.#ws.send(typeof frame === "string" ? frame : JSON.stringify(frame));
  }

  /**
   * Resolves with (and removes) the first queued or future frame matching
   * pred; frames that do not match stay queued.
   */
  next(pred = () => true, { timeout = 10_000, what = "a frame" } = {}) {
    return new Promise((resolve, reject) => {
      const w = { pred, resolve, reject, what };
      w.timer = setTimeout(() => {
        this.#waiters.splice(this.#waiters.indexOf(w), 1);
        reject(new Error(`timed out after ${timeout}ms waiting for ${what}; queued: ${JSON.stringify(this.#queue).slice(0, 800)}`));
      }, timeout);
      this.#waiters.push(w);
      this.#flush();
    });
  }

  /** Sends a frame and waits for the reply matching pred. */
  async request(frame, pred, what) {
    this.send(frame);
    return this.next(pred, { what: what ?? `reply to ${JSON.stringify(frame).slice(0, 100)}` });
  }

  /** Round trip with a ping: every frame sent before it has been processed. */
  async sync() {
    const id = "sync-" + randomBytes(4).toString("hex");
    // A ping carrying an id is not answered by an auto-responder, so the
    // pong proves the server processed everything sent before it.
    this.send({ t: "ping", id });
    await this.next((f) => f.t === "pong", { what: "pong" });
  }

  /** Asserts that no queued frame matches pred (call after sync()). */
  assertNone(pred, what) {
    const hit = this.#queue.find(pred);
    assert.equal(hit, undefined, `unexpected ${what}: ${JSON.stringify(hit)}`);
  }

  /** Drops queued frames matching pred (default: all). */
  drain(pred = () => true) {
    this.#queue = this.#queue.filter((f) => !pred(f));
  }

  get queued() {
    return [...this.#queue];
  }

  get readyState() {
    return this.#ws.readyState;
  }

  close() {
    try {
      this.#ws.close(1000);
    } catch {
      /* already closed */
    }
    return Promise.race([this.#closed, new Promise((r) => setTimeout(r, 2000))]);
  }

  closed() {
    return this.#closed;
  }
}

/**
 * Attempts a websocket handshake with node:http and returns the HTTP status
 * (101 on success) and headers, without keeping the connection.
 */
export async function handshake(env, client, site, { origin, headers = {} } = {}) {
  const url = env.api(site, "/ws");
  const h = {
    Connection: "Upgrade",
    Upgrade: "websocket",
    "Sec-WebSocket-Version": "13",
    "Sec-WebSocket-Key": randomBytes(16).toString("base64"),
    ...headers,
  };
  if (origin) h.Origin = origin;
  const cookie = client.jar.header(url);
  if (cookie) h.Cookie = cookie;
  if (client.token) h.Authorization = `Bearer ${client.token}`;
  return rawRequest(url, { headers: h });
}
