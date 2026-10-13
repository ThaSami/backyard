// Test harness for the browser SDK: loads a build into a fresh vm context with
// minimal fakes for location, fetch, WebSocket, timers and the console.
// No dependencies beyond Node 22 built-ins.

import vm from "node:vm";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

export const SDK_DIR = fileURLToPath(new URL("../../", import.meta.url));
export const CLASSIC_PATH = SDK_DIR + "backyard.js";
export const MODULE_PATH = SDK_DIR + "backyard.mjs";
export const CORE_PATH = SDK_DIR + "src/core.js";
export const TYPES_PATH = SDK_DIR + "backyard.d.ts";

const classicSource = readFileSync(CLASSIC_PATH, "utf8");

/** Converts a value from the sandbox realm into plain outer-realm data. */
export function plain(v) {
  return v === undefined ? undefined : JSON.parse(JSON.stringify(v));
}

/** Lets pending promise callbacks (in any realm) run. */
export function flush(times = 3) {
  let p = Promise.resolve();
  for (let i = 0; i < times; i++) p = p.then(() => new Promise((r) => setImmediate(r)));
  return p;
}

/** A deterministic clock implementing setTimeout/setInterval. */
export class FakeClock {
  constructor(start = 1_700_000_000_000) {
    this.now = start;
    this.timers = new Map();
    this.nextID = 1;
  }
  setTimeout(fn, ms) {
    const id = this.nextID++;
    this.timers.set(id, { fn, at: this.now + Math.max(0, Number(ms) || 0), every: 0 });
    return id;
  }
  setInterval(fn, ms) {
    const id = this.nextID++;
    const every = Math.max(1, Number(ms) || 0);
    this.timers.set(id, { fn, at: this.now + every, every });
    return id;
  }
  clear(id) {
    this.timers.delete(id);
  }
  /** Pending timers as {id, delay} (delay relative to now), soonest first. */
  pending() {
    return [...this.timers.entries()].map(([id, t]) => ({ id, delay: t.at - this.now, every: t.every })).sort((a, b) => a.delay - b.delay);
  }
  /** Advances time by ms, running due timers in order. */
  tick(ms) {
    const end = this.now + ms;
    for (;;) {
      let next = null;
      for (const [id, t] of this.timers) if (t.at <= end && (!next || t.at < next[1].at)) next = [id, t];
      if (!next) break;
      const [id, t] = next;
      this.now = t.at;
      if (t.every) t.at += t.every;
      else this.timers.delete(id);
      t.fn();
    }
    this.now = end;
  }
}

/** Builds a location object; assigning href records a redirect instead of navigating. */
export function makeLocation(href, redirects) {
  const u = new URL(href);
  return {
    get href() {
      return u.href;
    },
    set href(v) {
      redirects.push(String(v));
    },
    assign(v) {
      redirects.push(String(v));
    },
    origin: u.origin,
    protocol: u.protocol,
    host: u.host,
    hostname: u.hostname,
    port: u.port,
    pathname: u.pathname,
    search: u.search,
    hash: u.hash,
  };
}

/** A scriptable WebSocket double. Tests drive the server side. */
export class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.sent = [];
    this.closedWith = null;
    this.onopen = this.onmessage = this.onclose = this.onerror = null;
  }
  send(data) {
    if (this.readyState !== 1) throw new Error("InvalidStateError: not open");
    this.sent.push(JSON.parse(data));
  }
  close(code, reason) {
    this.closedWith = { code, reason };
    this.readyState = 3;
  }
  // --- server side
  open() {
    this.readyState = 1;
    this.onopen && this.onopen({});
  }
  hello(cid = "c_1", user = { id: "alice@acme.com", name: "Alice" }) {
    if (this.readyState === 0) this.open();
    this.push({ t: "hello", cid, user });
  }
  push(frame) {
    this.onmessage && this.onmessage({ data: typeof frame === "string" ? frame : JSON.stringify(frame) });
  }
  serverClose(code = 1006, reason = "") {
    this.readyState = 3;
    this.onclose && this.onclose({ code, reason, wasClean: code === 1000 });
  }
  /** Frames sent of type t (or all). */
  frames(t) {
    return t ? this.sent.filter((f) => f.t === t) : this.sent.slice();
  }
}

/** Creates a JSON Response. */
export function json(status, body, headers = {}) {
  if (status === 204) return new Response(null, { status, headers });
  return new Response(body === undefined ? "" : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", ...headers },
  });
}

/** Creates an API error Response ({"error":{code,message,login_url?}}). */
export function apiError(status, code, message, extra = {}, headers = {}) {
  return json(status, { error: { code, message, ...extra } }, headers);
}

/** A text/event-stream Response whose chunks the test pushes manually. */
export function sseResponse() {
  let controller;
  const stream = new ReadableStream({
    start(c) {
      controller = c;
    },
  });
  const enc = new TextEncoder();
  return {
    response: new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } }),
    push(text) {
      controller.enqueue(typeof text === "string" ? enc.encode(text) : text);
    },
    close() {
      controller.close();
    },
    error(err) {
      controller.error(err);
    },
  };
}

/**
 * Creates a sandbox with fakes and loads backyard.js into it.
 *
 * opts: href, preamble (object for globalThis.__BACKYARD__), config
 * (BACKYARD_CONFIG), top (separate top-level location for redirect tests),
 * document (a fake document, e.g. for visibilitychange), noWebSocket.
 */
export function createEnv(opts = {}) {
  const clock = new FakeClock();
  const redirects = [];
  const topRedirects = [];
  const requests = [];
  const routes = [];
  const sockets = [];
  const warnings = [];
  const errors = [];
  const windowListeners = {};
  const location = makeLocation(opts.href || "https://foo.backyard.test/index.html", redirects);

  class WS extends FakeWebSocket {
    constructor(url) {
      super(url);
      sockets.push(this);
    }
  }

  async function fakeFetch(url, init = {}) {
    const headers = {};
    for (const [k, v] of Object.entries(init.headers || {})) headers[k.toLowerCase()] = v;
    const u = new URL(url);
    const req = {
      url: String(url),
      path: u.pathname,
      search: u.search,
      query: Object.fromEntries(u.searchParams),
      method: init.method || "GET",
      headers,
      credentials: init.credentials,
      body: init.body,
      signal: init.signal,
      json: typeof init.body === "string" ? JSON.parse(init.body) : undefined,
    };
    requests.push(req);
    if (init.signal && init.signal.aborted) throw new DOMException("aborted", "AbortError");
    for (const r of routes) {
      if (r.method !== "*" && r.method !== req.method) continue;
      const m = typeof r.path === "string" ? (r.path === req.path ? [r.path] : null) : r.path.exec(req.path);
      if (!m) continue;
      if (r.once) routes.splice(routes.indexOf(r), 1);
      const out = await r.handler(req, m);
      if (out instanceof Error) throw out;
      return out instanceof Response ? out : json(200, out);
    }
    return apiError(404, "not_found", "no route for " + req.method + " " + req.path);
  }

  const sandbox = {
    location,
    fetch: fakeFetch,
    WebSocket: opts.noWebSocket ? undefined : WS,
    setTimeout: (fn, ms) => clock.setTimeout(fn, ms),
    clearTimeout: (id) => clock.clear(id),
    setInterval: (fn, ms) => clock.setInterval(fn, ms),
    clearInterval: (id) => clock.clear(id),
    console: {
      log() {},
      info() {},
      warn: (...a) => warnings.push(a.map(String).join(" ")),
      error: (...a) => errors.push(a),
    },
    reportError: (e) => errors.push([e]),
    addEventListener: (type, fn) => {
      (windowListeners[type] ||= []).push(fn);
    },
    FormData,
    Blob,
    File,
    URL,
    TextDecoder,
    TextEncoder,
    AbortController,
    DOMException,
    Response,
    Headers,
  };
  if (opts.preamble !== undefined) sandbox.__BACKYARD__ = opts.preamble;
  if (opts.config !== undefined) sandbox.BACKYARD_CONFIG = opts.config;
  if (opts.top) sandbox.top = { location: makeLocation(opts.top, topRedirects) };
  if (opts.document) sandbox.document = opts.document;
  sandbox.__clockNow = () => clock.now;
  vm.createContext(sandbox);
  vm.runInContext("Date.now = function () { return __clockNow(); };", sandbox);
  vm.runInContext(classicSource, sandbox, { filename: "backyard.js" });

  const env = {
    sandbox,
    backyard: sandbox.backyard,
    clock,
    requests,
    sockets,
    redirects,
    topRedirects,
    warnings,
    errors,
    /**
     * Registers a route: method ("*" any), path (exact string or RegExp on the
     * pathname). Routes registered later take precedence.
     */
    on(method, path, handler, { once = false } = {}) {
      // Static Responses are cloned per request (a body can only be read once).
      const fn = typeof handler === "function" ? handler : handler instanceof Response ? () => handler.clone() : () => handler;
      routes.unshift({ method, path, handler: fn, once });
      return env;
    },
    once(method, path, handler) {
      return env.on(method, path, handler, { once: true });
    },
    /** The most recent request. */
    last() {
      return requests[requests.length - 1];
    },
    /** The current (latest) socket. */
    ws() {
      return sockets[sockets.length - 1];
    },
    fire(type, ev = {}) {
      for (const fn of windowListeners[type] || []) fn(ev);
    },
  };
  return env;
}

/** Awaits a promise expected to reject; returns the error. */
export async function rejects(promise) {
  try {
    await promise;
  } catch (err) {
    return err;
  }
  throw new Error("expected the promise to reject");
}
