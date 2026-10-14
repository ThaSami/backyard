// A JSON-RPC client for the remote MCP endpoint (SPEC §14.1): one POST per
// message, JSON answers only. Every answer is checked for what the endpoint
// must never send: an Mcp-Session-Id, a cookie, or an event stream.
import assert from "node:assert/strict";
import http from "node:http";
import https from "node:https";
import { send } from "./oauth.mjs";

/** The protocol versions of SPEC §14.1, newest first. */
export const PROTOCOL_VERSIONS = ["2025-11-25", "2025-06-18"];

/** Asserts the transport rules every answer of /__backyard/mcp follows. */
export function assertTransport(r, what = "") {
  const ctx = `${what ? what + ": " : ""}${r}`;
  assert.equal(r.header("mcp-session-id"), null, `no Mcp-Session-Id: ${ctx}`);
  assert.deepEqual(r.headers.getSetCookie?.() ?? [], [], `no Set-Cookie: ${ctx}`);
  assert.doesNotMatch(r.header("content-type") || "", /text\/event-stream/, `never SSE: ${ctx}`);
  for (const [k] of r.headers) assert.ok(!k.toLowerCase().startsWith("access-control-"), `no CORS header ${k}: ${ctx}`);
}

/** Asserts a JSON-RPC error answer {jsonrpc, id, error:{code, message}} and returns error. */
export function assertRPCError(r, { status = 200, code, id, message, what = "" }) {
  const ctx = `${what ? what + ": " : ""}${r}`;
  assert.equal(r.status, status, `HTTP ${status}: ${ctx}`);
  assert.match(r.header("content-type") || "", /^application\/json\b/, `content type: ${ctx}`);
  const b = r.json;
  assert.equal(b.jsonrpc, "2.0", `jsonrpc: ${ctx}`);
  if (id !== undefined) assert.deepEqual(b.id, id, `id: ${ctx}`);
  assert.ok(b.error && typeof b.error === "object" && !("result" in b), `an error: ${ctx}`);
  assert.equal(b.error.code, code, `error code: ${ctx}`);
  assert.equal(typeof b.error.message, "string", `error message: ${ctx}`);
  if (message !== undefined) assert.equal(b.error.message, message, `error message: ${ctx}`);
  return b.error;
}

/** One connection's client: an endpoint (R) and an access token. */
export class MCPClient {
  /**
   * @param {{endpoint: string, token?: string, version?: string|null, origin?: string}} opts
   *   version: the MCP-Protocol-Version header (null: none).
   */
  constructor({ endpoint, token = "", version = PROTOCOL_VERSIONS[0], origin } = {}) {
    this.endpoint = endpoint;
    this.token = token;
    this.version = version;
    this.origin = origin;
    this.nextID = 1;
  }

  /**
   * POSTs body (an object, or a string or Buffer sent as is) and returns the
   * response after the transport checks. headers replace the defaults (a null value
   * removes one); token: null sends no Authorization.
   */
  async post(body, { token, headers = {}, method = "POST", what } = {}) {
    const h = { "Content-Type": "application/json", Accept: "application/json, text/event-stream" };
    const tok = token === undefined ? this.token : token;
    if (tok) h.Authorization = `Bearer ${tok}`;
    if (this.version) h["MCP-Protocol-Version"] = this.version;
    if (this.origin) h.Origin = this.origin;
    Object.assign(h, headers);
    for (const [k, v] of Object.entries(h)) if (v === null || v === undefined) delete h[k];
    const payload = method === "GET" || method === "HEAD" ? undefined : typeof body === "string" || Buffer.isBuffer(body) ? body : JSON.stringify(body);
    const r = await send(this.endpoint, { method, headers: h, body: payload });
    assertTransport(r, what ?? `${method} ${JSON.stringify(body)?.slice(0, 80)}`);
    return r;
  }

  /** A JSON-RPC request object with the next id. */
  request(method, params) {
    const msg = { jsonrpc: "2.0", id: this.nextID++, method };
    if (params !== undefined) msg.params = params;
    return msg;
  }

  /**
   * Sends a request and returns its result, asserting a 200 JSON-RPC success
   * that echoes the id.
   */
  async rpc(method, params, opts = {}) {
    const msg = this.request(method, params);
    const r = await this.post(msg, opts);
    const ctx = `${method}: ${r}`;
    assert.equal(r.status, 200, ctx);
    assert.match(r.header("content-type") || "", /^application\/json\b/, `content type: ${ctx}`);
    assert.match(r.header("cache-control") || "", /\bno-store\b/, `Cache-Control: ${ctx}`);
    const b = r.json;
    assert.equal(b.jsonrpc, "2.0", ctx);
    assert.deepEqual(b.id, msg.id, `id echoed: ${ctx}`);
    assert.ok(!("error" in b), `no error: ${ctx}`);
    assert.ok("result" in b, `a result: ${ctx}`);
    return b.result;
  }

  /** initialize with protocolVersion (default this.version or the newest). */
  initialize(protocolVersion = this.version ?? PROTOCOL_VERSIONS[0], opts) {
    return this.rpc(
      "initialize",
      { protocolVersion, capabilities: {}, clientInfo: { name: "backyard-conformance", version: "1" } },
      opts,
    );
  }

  /**
   * tools/call; returns the result ({content, isError?}) after checking its
   * shape: one text content item.
   */
  async call(name, args, opts) {
    const params = { name };
    if (args !== undefined) params.arguments = args;
    const res = await this.rpc("tools/call", params, opts);
    const ctx = `tools/call ${name}: ${JSON.stringify(res).slice(0, 400)}`;
    assert.ok(Array.isArray(res.content) && res.content.length === 1, `one content item: ${ctx}`);
    assert.equal(res.content[0].type, "text", ctx);
    assert.equal(typeof res.content[0].text, "string", ctx);
    assert.ok(!("structuredContent" in res), `no structuredContent: ${ctx}`);
    if ("isError" in res) assert.equal(typeof res.isError, "boolean", ctx);
    return res;
  }

  /** A tool call that must succeed; returns its text. */
  async callText(name, args, opts) {
    const res = await this.call(name, args, opts);
    assert.notEqual(res.isError, true, `${name} failed: ${res.content[0].text}`);
    return res.content[0].text;
  }

  /** A tool call that must succeed with a JSON result; returns it. */
  async callJSON(name, args, opts) {
    const text = await this.callText(name, args, opts);
    try {
      return JSON.parse(text);
    } catch {
      assert.fail(`${name}: the result is not JSON: ${text.slice(0, 300)}`);
    }
  }

  /** A tool call that must fail (isError); returns the message. */
  async callError(name, args, opts) {
    const res = await this.call(name, args, opts);
    assert.equal(res.isError, true, `${name} must fail: ${res.content[0].text.slice(0, 300)}`);
    return res.content[0].text;
  }
}

/**
 * POSTs a body of exactly size bytes (a JSON-RPC ping padded with spaces)
 * with node:http and `Expect: 100-continue`: the body is sent once the server
 * asks for it (or after 1 s, for servers and proxies that ignore the header),
 * so an answer given from the Content-Length alone (413) arrives before any
 * body is written. Resolves to {status, headers, body}.
 */
export function postSized(endpoint, size, headers = {}) {
  const msg = Buffer.from(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "ping" }));
  assert.ok(size >= msg.length, `size ${size} is too small`);
  const u = new URL(endpoint);
  const mod = u.protocol === "https:" ? https : http;
  return new Promise((resolve, reject) => {
    let answered = false;
    let started = false;
    const req = mod.request(
      {
        protocol: u.protocol,
        hostname: u.hostname.replace(/^\[|\]$/g, ""),
        port: u.port || (u.protocol === "https:" ? 443 : 80),
        method: "POST",
        path: u.pathname + u.search,
        headers: { "Content-Type": "application/json", Accept: "application/json", "Content-Length": String(size), Expect: "100-continue", ...headers },
        timeout: 120_000,
      },
      (res) => {
        answered = true;
        clearTimeout(timer);
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => {
          resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) });
          if (!started || left > 0) req.destroy(); // the rest of the body is never sent
        });
        res.on("error", reject);
      },
    );
    req.on("timeout", () => req.destroy(new Error(`POST ${endpoint} (${size} bytes): timeout`)));
    req.on("error", (err) => {
      // The server may close the connection after answering early.
      if (!answered) reject(err);
    });
    const chunk = Buffer.alloc(64 * 1024, 0x20);
    let left = size - msg.length;
    const write = () => {
      while (left > 0 && !answered) {
        const n = Math.min(left, chunk.length);
        left -= n;
        if (!req.write(n === chunk.length ? chunk : chunk.subarray(0, n))) {
          req.once("drain", write);
          return;
        }
      }
      if (!answered) req.end();
    };
    const start = () => {
      if (started || answered) return;
      started = true;
      req.write(msg);
      write();
    };
    const timer = setTimeout(start, 1000);
    req.on("continue", start);
    req.flushHeaders();
  });
}
