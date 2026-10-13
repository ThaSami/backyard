// A small HTTP client for the suite: global fetch with a cookie jar (each
// Client is one browser profile, hence one anonymous identity in auth mode
// none), the CSRF header on state-changing requests, the optional bearer
// token, manual redirects and automatic retries of 429 responses (anonymous
// clients share one per-IP rate-limit bucket).
import assert from "node:assert/strict";
import http from "node:http";
import https from "node:https";
import { token as envToken } from "./env.mjs";

/** A minimal RFC 6265 cookie jar (domain matching; paths are always "/"). */
export class CookieJar {
  #cookies = new Map(); // key: name|domain

  /** Stores the Set-Cookie headers of a response for the request URL. */
  store(url, setCookies) {
    const host = new URL(url).hostname.toLowerCase();
    for (const line of setCookies) {
      const [pair, ...attrs] = line.split(";");
      const eq = pair.indexOf("=");
      if (eq <= 0) continue;
      const name = pair.slice(0, eq).trim();
      const value = pair.slice(eq + 1).trim();
      let domain = host;
      let hostOnly = true;
      let expired = false;
      const attributes = {};
      for (const a of attrs) {
        const i = a.indexOf("=");
        const k = (i < 0 ? a : a.slice(0, i)).trim().toLowerCase();
        const v = i < 0 ? "" : a.slice(i + 1).trim();
        attributes[k] = v;
        if (k === "domain" && v) {
          domain = v.replace(/^\./, "").toLowerCase();
          hostOnly = false;
        } else if (k === "max-age" && Number(v) <= 0) {
          expired = true;
        } else if (k === "expires" && Date.parse(v) < Date.now()) {
          expired = true;
        }
      }
      const key = `${name}|${domain}`;
      if (expired) this.#cookies.delete(key);
      else this.#cookies.set(key, { name, value, domain, hostOnly, attributes, raw: line });
    }
  }

  /** The Cookie header value for url ("" when none). */
  header(url) {
    const host = new URL(url).hostname.toLowerCase();
    const out = [];
    for (const c of this.#cookies.values()) {
      if (c.hostOnly ? host === c.domain : host === c.domain || host.endsWith("." + c.domain)) out.push(`${c.name}=${c.value}`);
    }
    return out.join("; ");
  }

  /** All stored cookies (for assertions). */
  all() {
    return [...this.#cookies.values()];
  }
}

/** A buffered response. */
export class Reply {
  constructor(url, method, res, body, attempts) {
    this.url = url;
    this.method = method;
    this.status = res.status;
    this.headers = res.headers;
    this.body = body;
    this.attempts = attempts;
  }

  get text() {
    return this.body.toString("utf8");
  }

  /** The parsed JSON body (fails the test with context when it is not JSON). */
  get json() {
    try {
      return JSON.parse(this.text);
    } catch {
      assert.fail(`${this.method} ${this.url}: expected a JSON body, got HTTP ${this.status} ${this.headers.get("content-type")}: ${this.text.slice(0, 300)}`);
    }
  }

  header(name) {
    return this.headers.get(name);
  }

  toString() {
    return `${this.method} ${this.url} → HTTP ${this.status} ${this.text.slice(0, 400)}`;
  }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** One browser-like client (its own cookies, hence its own anonymous identity). */
export class Client {
  /**
   * @param {{token?: string|null, jar?: CookieJar}} [opts] token: null = never send a
   *   bearer token; default BACKYARD_TOKEN.
   */
  constructor(opts = {}) {
    this.token = opts.token === undefined ? envToken : opts.token || "";
    this.jar = opts.jar ?? new CookieJar();
  }

  /**
   * Sends a request.
   *   json     body to send as application/json (or a pre-encoded string with raw: true)
   *   body     raw body (string, Buffer, FormData, Blob)
   *   headers  extra headers (a null value removes a default header)
   *   csrf     send "X-Backyard: 1" (default: for every method but GET/HEAD)
   *   retry    retry 429 responses (default true)
   */
  async fetch(url, opts = {}) {
    const method = (opts.method || "GET").toUpperCase();
    const headers = new Headers();
    if (this.token) headers.set("Authorization", `Bearer ${this.token}`);
    const csrf = opts.csrf ?? !(method === "GET" || method === "HEAD");
    if (csrf) headers.set("X-Backyard", "1");
    let body = opts.body;
    if (opts.json !== undefined) {
      body = typeof opts.json === "string" && opts.raw ? opts.json : JSON.stringify(opts.json);
      headers.set("Content-Type", "application/json");
    }
    for (const [k, v] of Object.entries(opts.headers ?? {})) {
      if (v === null || v === undefined) headers.delete(k);
      else headers.set(k, String(v));
    }
    const retry = opts.retry ?? true;
    for (let attempt = 1; ; attempt++) {
      const h = new Headers(headers);
      const cookie = this.jar.header(url);
      if (cookie && !h.has("Cookie")) h.set("Cookie", cookie);
      const res = await fetch(url, { method, headers: h, body, redirect: "manual", signal: AbortSignal.timeout(opts.timeout ?? 60_000) });
      this.jar.store(url, res.headers.getSetCookie());
      const buf = Buffer.from(await res.arrayBuffer());
      if (res.status === 429 && retry && attempt < 8 && !(body instanceof ReadableStream)) {
        const after = Number(res.headers.get("Retry-After"));
        await sleep(Math.min(Number.isFinite(after) && after > 0 ? after * 1000 : 1000, 15_000));
        continue;
      }
      return new Reply(url, method, res, buf, attempt);
    }
  }

  get(url, opts = {}) {
    return this.fetch(url, { ...opts, method: "GET" });
  }
  head(url, opts = {}) {
    return this.fetch(url, { ...opts, method: "HEAD" });
  }
  post(url, json, opts = {}) {
    return this.fetch(url, { ...opts, method: "POST", json });
  }
  put(url, json, opts = {}) {
    return this.fetch(url, { ...opts, method: "PUT", json });
  }
  patch(url, json, opts = {}) {
    return this.fetch(url, { ...opts, method: "PATCH", json });
  }
  delete(url, opts = {}) {
    return this.fetch(url, { ...opts, method: "DELETE" });
  }
}

/**
 * Sends a request with node:http so that the path is sent verbatim (fetch
 * normalizes "/./" and "/../") and any Host header can be set. Resolves to
 * {status, headers, body}.
 */
export function rawRequest(urlStr, { method = "GET", path, headers = {}, body, timeout = 30_000 } = {}) {
  const u = new URL(urlStr);
  const mod = u.protocol === "https:" ? https : http;
  return new Promise((resolve, reject) => {
    const req = mod.request(
      {
        protocol: u.protocol,
        hostname: u.hostname.replace(/^\[|\]$/g, ""),
        port: u.port || (u.protocol === "https:" ? 443 : 80),
        method,
        path: path ?? u.pathname + u.search,
        headers,
        timeout,
      },
      (res) => {
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
        res.on("error", reject);
      },
    );
    req.on("timeout", () => req.destroy(Object.assign(new Error(`${method} ${urlStr}: timeout`), { code: "ETIMEDOUT" })));
    req.on("error", reject);
    req.on("upgrade", (res, socket) => {
      socket.destroy();
      resolve({ status: res.statusCode, headers: res.headers, body: Buffer.alloc(0) });
    });
    if (body !== undefined) req.write(body);
    req.end();
  });
}

/**
 * Asserts a JSON error envelope {"error": {"code", "message"}} with the given
 * status and code.
 */
export function assertError(reply, status, code, what = "") {
  const ctx = `${what ? what + ": " : ""}${reply}`;
  assert.equal(reply.status, status, `expected HTTP ${status} (${code}): ${ctx}`);
  assert.match(reply.header("content-type") || "", /^application\/json\b/, `error content type: ${ctx}`);
  const b = reply.json;
  assert.ok(b && typeof b === "object" && b.error && typeof b.error === "object", `error envelope: ${ctx}`);
  assert.equal(b.error.code, code, `error code: ${ctx}`);
  assert.equal(typeof b.error.message, "string", `error message: ${ctx}`);
  assert.ok(b.error.message.length > 0, `error message is empty: ${ctx}`);
  return b.error;
}

/** Asserts a status, printing the response on failure; returns the reply. */
export function expectStatus(reply, status, what = "") {
  assert.equal(reply.status, status, `${what ? what + ": " : ""}${reply}`);
  return reply;
}
