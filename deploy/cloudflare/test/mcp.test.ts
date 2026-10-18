/**
 * The Claude connector's MCP endpoint POST /__backyard/mcp (SPEC §14.1): the
 * gate in the Go server's order (same statuses, headers and JSON-RPC codes as
 * internal/mcpremote), JSON-RPC framing, the catalog, the tools and their
 * error texts, publish (merge, delete, replace, the overwrite guard, quotas,
 * base64, the per-call cap, files in R2, deployMerge atomicity), the daily
 * tool-call limit and credential isolation. Connections are made through the
 * real authorization flow (DCR, PKCE, consent, code exchange).
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetAccessCaches } from "../src/auth";
import { KeyLabel, b64url, deriveKey, signValue } from "../src/crypto";
import { formatMicros } from "../src/db/time";
import type { Env } from "../src/env";
import { formatBytes, truncateBytes } from "../src/gateway";
import { MCP_CATALOG } from "../src/generated/mcp";
import { unwrap } from "../src/http";
import { issueAPIToken } from "../src/jwt";
import { SCHEMA_KEYWORDS, validateSchema } from "../src/mcp";
import { TOOLS, decodeBase64, toValidUTF8 } from "../src/mcp-tools";
import { resetMcpCaches } from "../src/oauth";
import { fileFolderConflicts, type DeployFile } from "../src/site-do";
import { registry, siteStub } from "../src/stubs";
import { DEPLOY_TOKEN, MCP_ENV, ORIGIN, SECRET, call, deploy, mcpConnect, sha256Hex, testEnv, uniqueSite, wrapSiteNamespace, type MCPConnection } from "./helpers";

/** Mode none with anonymous deploys: guests may publish. */
const PUB: Partial<Env> = { ...MCP_ENV, ALLOW_ANONYMOUS_DEPLOY: "true" };
const M = ORIGIN + "/.well-known/oauth-protected-resource/__backyard/mcp";
const CHALLENGE = `Bearer resource_metadata="${M}", scope="mcp"`;
const invalidToken = (desc: string, m = M) => `Bearer error="invalid_token", error_description="${desc}", resource_metadata="${m}", scope="mcp"`;
const UNAUTHENTICATED = { error: { code: "unauthenticated", message: "connect this app to Backyard to sign in" } };
const LIMIT = 2 * 2097152 + (1 << 20);
const TOOL_NAMES = ["backyard_docs", "backyard_whoami", "backyard_list_sites", "backyard_site_files", "backyard_read_site_file", "backyard_publish"];

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  resetMcpCaches();
  resetAccessCaches();
});

/** LIMITS of the suite (vitest.config.ts) with overrides. */
function limits(extra: Record<string, number>): Partial<Env> {
  return { LIMITS: JSON.stringify({ max_sites: 100000, deploys_per_day: 100000, uploads_per_day: 100000, ...extra }) };
}

interface Sent {
  res: Response;
  status: number;
  text: string;
  /** The parsed body (undefined when empty or no JSON). */
  json: any; // eslint-disable-line @typescript-eslint/no-explicit-any
  header(name: string): string | null;
}

interface PostOptions {
  /** The access token (null: no Authorization header). */
  token?: string | null;
  /** Extra headers; null removes a default one. */
  headers?: Record<string, string | null>;
  /** Raw header list (repeated names), replacing Authorization. */
  headerList?: [string, string][];
  method?: string;
  env?: Partial<Env>;
  url?: string;
}

/**
 * Sends one message to /__backyard/mcp (body: a value to encode, or a raw
 * string or bytes). Every answer must carry no Mcp-Session-Id and set no cookie.
 */
async function post(body: unknown, o: PostOptions = {}): Promise<Sent> {
  const h = new Headers({ "Content-Type": "application/json", Accept: "application/json, text/event-stream", "MCP-Protocol-Version": "2025-11-25" });
  if (o.token) h.set("Authorization", `Bearer ${o.token}`);
  for (const [k, v] of Object.entries(o.headers ?? {})) {
    if (v === null) h.delete(k);
    else h.set(k, v);
  }
  for (const [k, v] of o.headerList ?? []) h.append(k, v);
  const method = o.method ?? "POST";
  const payload =
    method === "GET" || method === "HEAD" || body === undefined ? undefined : typeof body === "string" || body instanceof Uint8Array ? body : JSON.stringify(body);
  const res = await call(o.url ?? "/__backyard/mcp", { method, headers: h, body: payload }, o.env ?? PUB);
  const text = await res.text();
  let json: unknown;
  try {
    json = text ? JSON.parse(text) : undefined;
  } catch {
    json = undefined;
  }
  expect(res.headers.get("Mcp-Session-Id"), "no session").toBeNull();
  expect(res.headers.getSetCookie(), "no cookie").toEqual([]);
  return { res, status: res.status, text, json, header: (n) => res.headers.get(n) };
}

let nextID = 1;
const request = (method: string, params?: unknown) => (params === undefined ? { jsonrpc: "2.0", id: nextID++, method } : { jsonrpc: "2.0", id: nextID++, method, params });

/** A connection's JSON-RPC client. */
function client(conn: { access_token: string }, env: Partial<Env> = PUB) {
  const c = {
    post: (body: unknown, o: PostOptions = {}) => post(body, { token: conn.access_token, env, ...o }),
    async rpc(method: string, params?: unknown): Promise<any> { // eslint-disable-line @typescript-eslint/no-explicit-any
      const r = await c.post(request(method, params));
      expect(r.status, `${method}: ${r.text.slice(0, 300)}`).toBe(200);
      expect(r.header("Content-Type")).toBe("application/json");
      expect(r.header("Cache-Control")).toBe("no-store");
      expect(r.json.error, `${method}: ${r.text.slice(0, 300)}`).toBeUndefined();
      return r.json.result;
    },
    async tool(name: string, args?: Record<string, unknown>): Promise<{ content: { type: string; text: string }[]; isError?: boolean }> {
      const result = await c.rpc("tools/call", args === undefined ? { name } : { name, arguments: args });
      expect(result.content).toHaveLength(1);
      expect(result.content[0].type).toBe("text");
      expect(Object.keys(result).sort()).toEqual(result.isError ? ["content", "isError"] : ["content"]);
      return result;
    },
    async text(name: string, args?: Record<string, unknown>): Promise<string> {
      const r = await c.tool(name, args);
      expect(r.isError, `${name}: ${r.content[0]!.text.slice(0, 300)}`).toBeUndefined();
      return r.content[0]!.text;
    },
    async json(name: string, args?: Record<string, unknown>): Promise<any> { // eslint-disable-line @typescript-eslint/no-explicit-any
      return JSON.parse(await c.text(name, args));
    },
    async error(name: string, args?: Record<string, unknown>): Promise<string> {
      const r = await c.tool(name, args);
      expect(r.isError, `${name} should fail: ${r.content[0]!.text.slice(0, 300)}`).toBe(true);
      return r.content[0]!.text;
    },
  };
  return c;
}

/** A new guest's connection (mode none) and its client. */
async function guest(env: Partial<Env> = PUB): Promise<{ conn: MCPConnection; mcp: ReturnType<typeof client> }> {
  const conn = await mcpConnect({ env });
  return { conn, mcp: client(conn, env) };
}

/** The guest user id of a connection. */
async function userOf(mcp: ReturnType<typeof client>): Promise<string> {
  return (await mcp.json("backyard_whoami")).user.id as string;
}

/** The gid of an access token. */
function gidOf(token: string): string {
  const p = token.slice(5).split(".")[0]!;
  return (JSON.parse(atob(p.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((p.length + 3) % 4))) as { g: string }).g;
}

/** Fetches a published file of a site (path mode). */
async function siteFile(site: string, path: string, env: Partial<Env> = PUB): Promise<Response> {
  return call(`/s/${site}/${path}`, {}, env);
}

/** The site record of the registry. */
async function siteRecord(site: string) {
  return (await registry(testEnv()).getSite(site)) as { ok: true; value: Record<string, unknown> | null };
}

// =============================================================================

describe("the shared catalog (SPEC §14.1)", () => {
  it("is the generated copy of internal/mcpremote/tools.json, with a tool for every entry", () => {
    expect(MCP_CATALOG.tools.map((t) => t.name)).toEqual(TOOL_NAMES);
    expect(Object.keys(TOOLS).sort()).toEqual([...TOOL_NAMES].sort());
    expect(MCP_CATALOG.instructions).toMatch(/^Backyard hosts internal websites/);
    for (const t of MCP_CATALOG.tools) {
      expect(t.title, t.name).toBeTruthy();
      expect(Object.keys(t.annotations ?? {}).sort(), t.name).toEqual(["destructiveHint", "idempotentHint", "openWorldHint", "readOnlyHint", "title"]);
    }
  });

  it("uses no JSON Schema keyword the validator does not implement", () => {
    const seen = new Set<string>();
    const walk = (s: Record<string, unknown>) => {
      for (const [k, v] of Object.entries(s)) {
        seen.add(k);
        if (k === "properties") for (const p of Object.values(v as Record<string, Record<string, unknown>>)) walk(p);
        else if (k === "items") walk(v as Record<string, unknown>);
      }
    };
    for (const t of MCP_CATALOG.tools) {
      expect(t.inputSchema.type, t.name).toBe("object");
      walk(t.inputSchema);
    }
    for (const k of seen) expect(SCHEMA_KEYWORDS, `keyword ${k}`).toContain(k);
    // Every pattern compiles as a Unicode regular expression.
    const publish = MCP_CATALOG.tools.find((t) => t.name === "backyard_publish")!;
    expect(validateSchema(publish.inputSchema, { site: "ok-name", files: [] })).toBeNull();
  });
});

describe("validateSchema (the catalog's keywords)", () => {
  const schema = {
    type: "object",
    properties: {
      s: { type: "string", minLength: 1, maxLength: 3, pattern: "^[a-z😀]+$" },
      e: { type: "string", enum: ["a", "b"] },
      b: { type: "boolean" },
      list: { type: "array", maxItems: 2, items: { type: "object", properties: { p: { type: "string" } }, required: ["p"], additionalProperties: false } },
    },
    required: ["s"],
    additionalProperties: false,
  };
  it("accepts valid input and counts characters as code points", () => {
    expect(validateSchema(schema, { s: "abc" })).toBeNull();
    expect(validateSchema(schema, { s: "😀😀😀", e: "b", b: false, list: [{ p: "x" }, { p: "" }] })).toBeNull();
  });
  it("reports the first problem with its location", () => {
    const cases: [unknown, RegExp][] = [
      [[], /must be an object/],
      [{}, /missing required property "s"/],
      [{ s: "" }, /^s: must be at least 1/],
      [{ s: "abcd" }, /^s: must be at most 3/],
      [{ s: "😀😀😀😀" }, /^s: must be at most 3/],
      [{ s: "A" }, /^s: must match the pattern/],
      [{ s: 5 }, /^s: must be a string/],
      [{ s: "a", e: "c" }, /^e: must be one of "a", "b"/],
      [{ s: "a", b: "true" }, /^b: must be a boolean/],
      [{ s: "a", x: 1 }, /unexpected property "x"/],
      [{ s: "a", list: {} }, /^list: must be an array/],
      [{ s: "a", list: [{ p: "1" }, { p: "2" }, { p: "3" }] }, /^list: must have at most 2 items/],
      [{ s: "a", list: [{ p: "1" }, {}] }, /^list\[1\]: missing required property "p"/],
      [{ s: "a", list: [{ p: 1 }] }, /^list\[0\]\.p: must be a string/],
      [{ s: "a", list: [{ p: "1", q: 2 }] }, /^list\[0\]: unexpected property "q"/],
    ];
    for (const [v, want] of cases) expect(validateSchema(schema, v), JSON.stringify(v)).toMatch(want);
  });
});

describe("helpers", () => {
  it("decodes base64 like the Go server (both alphabets, padding optional, no whitespace)", () => {
    const raw = new Uint8Array([0xfb, 0xff, 0xfe, 0x3e, 0x3f, 0x00]);
    expect(decodeBase64("+//+Pj8A")).toEqual(raw);
    expect(decodeBase64("-__-Pj8A")).toEqual(raw);
    expect(decodeBase64("aGk=")).toEqual(new TextEncoder().encode("hi"));
    expect(decodeBase64("aGk")).toEqual(new TextEncoder().encode("hi"));
    expect(decodeBase64("")).toEqual(new Uint8Array(0));
    for (const bad of ["aGVs bG8=", "aGVs\nbG8=", "!!!!", "a", "aGk==", "aG=k", "aGk===", "+_8A", "aGVsbG8=="]) expect(decodeBase64(bad), bad).toBeNull();
  });

  it("formats sizes like the Go server's FormatBytes", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(812)).toBe("812 B");
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(1280)).toBe("1.2 KiB"); // half to even, as Go's %.1f
    expect(formatBytes(1331)).toBe("1.3 KiB");
    expect(formatBytes(3584)).toBe("3.5 KiB");
    expect(formatBytes(2097152)).toBe("2.0 MiB");
    expect(formatBytes(2097153)).toBe("2.0 MiB");
    expect(formatBytes(5 * 1024 ** 3)).toBe("5.0 GiB");
  });

  it("repairs invalid UTF-8 like strings.ToValidUTF8 and truncates by bytes", () => {
    const b = (...x: number[]) => new Uint8Array(x);
    expect(toValidUTF8(new TextEncoder().encode("héllo"))).toBe("héllo");
    expect(toValidUTF8(b(0x61, 0xff, 0xfe, 0x62))).toBe("a\ufffdb"); // one replacement per run
    expect(toValidUTF8(b(0xe2, 0x82, 0x41))).toBe("\ufffdA");
    expect(toValidUTF8(b(0xed, 0xa0, 0x80))).toBe("\ufffd"); // a surrogate
    expect(toValidUTF8(b(0xf0, 0x9f, 0x98, 0x80, 0xc0))).toBe("😀\ufffd");
    expect(truncateBytes("abc", 3)).toBe("abc");
    expect(truncateBytes("abcd", 3)).toBe("abc…");
    expect(truncateBytes("aé", 2)).toBe("a…");
  });

  it("finds paths that are both a file and a folder", () => {
    expect(fileFolderConflicts(new Set(["a", "a/b", "c/d", "c/d/e/f", "x"]))).toEqual(["a", "c/d"]);
    expect(fileFolderConflicts(new Set(["a/b", "ab"]))).toEqual([]);
  });
});

// =============================================================================

describe("the gate (SPEC §14.1 steps 1–9, in order)", () => {
  it("answers 404 while the connector is off, and on another host", async () => {
    const off = { error: { code: "not_found", message: "the Claude connector is not enabled on this server" } };
    for (const env of [{}, { MCP_ENABLED: "false" }, { ...MCP_ENV, PUBLIC_URL: "http://backyard.test" }]) {
      const r = await post(request("ping"), { env });
      expect(r.status, JSON.stringify(env)).toBe(404);
      expect(r.json).toEqual(off);
    }
    const other = await post(request("ping"), { url: "https://other.test/__backyard/mcp", env: { ...PUB, PUBLIC_URL: ORIGIN } });
    expect(other.status).toBe(404);
    expect(other.json.error.code).toBe("not_found");
    // A site host: the existing JSON 404.
    const site = await post(request("ping"), { url: ORIGIN + "/s/some-site/__backyard/mcp" });
    expect(site.status).toBe(404);
  });

  it("checks Origin first: absent, the hub's and Claude's are accepted", async () => {
    const { conn } = await guest();
    const forbidden = { error: { code: "forbidden", message: "requests from this origin are not accepted" } };
    for (const origin of ["https://evil.example", "null", "https://claude.ai.evil.example", "https://backyard.test.evil", "https://claude.ai/path", "https://user@claude.ai", "http://claude.ai"]) {
      for (const token of [conn.access_token, null]) {
        const r = await post(request("ping"), { token, headers: { Origin: origin } });
        expect(r.status, `${origin} ${token ? "with" : "without"} a token`).toBe(403);
        expect(r.json).toEqual(forbidden);
      }
    }
    for (const origin of [null, ORIGIN, "https://BACKYARD.test", "HTTPS://BACKYARD.TEST:443", "https://claude.ai", "HTTPS://CLAUDE.AI", "https://claude.com", "https://claude.ai/", ""]) {
      const r = await post(request("ping"), { token: conn.access_token, headers: { Origin: origin } });
      expect(r.status, `Origin ${origin}: ${r.text}`).toBe(200);
    }
    // The origins come from the configured allowlists.
    const custom = { ...PUB, MCP_CLIENTS: "", MCP_REDIRECT_URIS: "https://app.example/cb" };
    expect((await post(request("ping"), { token: conn.access_token, env: custom, headers: { Origin: "https://claude.ai" } })).status).toBe(403);
    expect((await post(request("ping"), { token: conn.access_token, env: custom, headers: { Origin: "https://app.example" } })).status).toBe(200);
  });

  it("needs exactly one Backyard MCP access token, with the exact challenges", async () => {
    const r = await post(request("ping"));
    expect(r.status).toBe(401);
    expect(r.header("WWW-Authenticate")).toBe(CHALLENGE);
    expect(r.json).toEqual(UNAUTHENTICATED);
    // Other schemes count as absent; the scheme is case-insensitive.
    for (const authz of ["Basic dXNlcjpwYXNz", "Bearer", "Bearer   ", "Token abc", "bearerabc"]) {
      const o = await post(request("ping"), { headers: { Authorization: authz } });
      expect(o.status, authz).toBe(401);
      expect(o.header("WWW-Authenticate"), authz).toBe(CHALLENGE);
    }
    const { conn } = await guest();
    expect((await post(request("ping"), { headers: { Authorization: `bEaReR ${conn.access_token}` } })).status).toBe(200);
    // Two Authorization headers.
    const two = await post(request("ping"), { headerList: [["Authorization", `Bearer ${conn.access_token}`], ["Authorization", `Bearer ${conn.access_token}`]] });
    expect(two.status).toBe(400);
    expect(two.header("WWW-Authenticate")).toBe(`Bearer error="invalid_request", resource_metadata="${M}"`);
    expect(two.json.error.code).toBe("bad_request");
    // Garbage, other credentials and other Backyard tokens are "malformed token".
    const api = await issueAPIToken(SECRET, { id: "alice@acme.com", email: "alice@acme.com", name: "Alice", anonymous: false, admin: false }, 3600);
    const forged = "byat_" + (await signValue((await deriveKey(SECRET, KeyLabel.APIToken)).hmac, JSON.stringify({ g: gidOf(conn.access_token), i: 1, x: 9999999999 })));
    for (const [what, tok] of [
      ["garbage", "nope"],
      ["DEPLOY_TOKEN", DEPLOY_TOKEN],
      ["an API token", api.token],
      ["a refresh token", conn.refresh_token],
      ["a token signed with another label", forged],
      ["a tampered token", conn.access_token.slice(0, -2) + (conn.access_token.endsWith("AA") ? "BB" : "AA")],
      ["a token over 4096 bytes", "byat_" + "a".repeat(4092)],
    ] as const) {
      const bad = await post(request("ping"), { token: tok });
      expect(bad.status, what).toBe(401);
      expect(bad.header("WWW-Authenticate"), what).toBe(invalidToken("malformed token"));
      expect(bad.json, what).toEqual(UNAUTHENTICATED);
    }
  });

  it("reports expired, revoked, foreign and re-gated tokens", async () => {
    const { conn } = await guest();
    const gid = gidOf(conn.access_token);
    const now = Math.floor(Date.now() / 1000);
    const expired = "byat_" + (await signValue((await deriveKey(SECRET, KeyLabel.MCPAccess)).hmac, JSON.stringify({ g: gid, i: now - 7200, x: now - 3600 })));
    const e = await post(request("ping"), { token: expired });
    expect(e.status).toBe(401);
    expect(e.header("WWW-Authenticate")).toBe(invalidToken("token expired"));
    // The hub's public URL changed: R is another resource.
    const moved = "https://backyard.test:8443";
    const f = await post(request("ping"), { token: conn.access_token, env: { ...PUB, PUBLIC_URL: moved } });
    expect(f.status).toBe(401);
    expect(f.header("WWW-Authenticate")).toBe(invalidToken("token not issued for this server", moved + "/.well-known/oauth-protected-resource/__backyard/mcp"));
    // Disconnected on the hub: revoked at once.
    const del = await call(`/__backyard/api/mcp/grants/${gid}`, { method: "DELETE", headers: { ...conn.identity, "X-Backyard": "1" } }, MCP_ENV);
    expect(del.status).toBe(204);
    const r = await post(request("ping"), { token: conn.access_token });
    expect(r.status).toBe(401);
    expect(r.header("WWW-Authenticate")).toBe(invalidToken("token revoked"));
    // A guest's connection in another auth mode: removed by policy, and the grant is gone.
    const g2 = await guest();
    const access = { ...PUB, AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: "aud" };
    const p = await post(request("ping"), { token: g2.conn.access_token, env: access });
    expect(p.status).toBe(401);
    expect(p.header("WWW-Authenticate")).toBe(invalidToken("access removed by policy"));
    expect((await post(request("ping"), { token: g2.conn.access_token })).header("WWW-Authenticate")).toBe(invalidToken("token revoked"));
  });

  it("answers 503 when the connection cannot be checked, and -32603 for internal failures", async () => {
    const { conn } = await guest();
    const real = testEnv().REGISTRY;
    /** The registry, with some RPC methods failing. */
    const failing = (methods: string[]) =>
      ({
        idFromName: (n: string) => real.idFromName(n),
        get: (id: DurableObjectId) => {
          const stub = real.get(id);
          return new Proxy(stub, {
            get(t, p) {
              if (typeof p === "string" && methods.includes(p)) return async () => Promise.reject(new Error("registry down"));
              // RPC methods of a stub are called on the stub itself.
              return (...a: unknown[]) => (t as unknown as Record<string | symbol, (...x: unknown[]) => unknown>)[p]!(...a);
            },
          });
        },
      }) as unknown as Env["REGISTRY"];
    const down = await post(request("ping"), { token: conn.access_token, env: { ...PUB, REGISTRY: failing(["mcpGrantGet"]) } });
    expect(down.status).toBe(503);
    expect(down.json.error.code).toBe("internal");
    expect(down.json.error.message).toMatch(/^Backyard could not check this connection right now \(request [^)]+\); try again$/);
    expect(down.text).not.toContain("registry down");
    expect(down.header("WWW-Authenticate")).toBeNull();
    const broken = await post(request("tools/call", { name: "backyard_whoami" }), { token: conn.access_token, env: { ...PUB, REGISTRY: failing(["charge"]) } });
    expect(broken.status).toBe(200);
    expect(broken.json.error).toEqual({ code: -32603, message: `Internal error (request ${broken.header("X-Request-Id")})` });
    // A read tool's own failure is a tool error naming the request.
    const list = await post(request("tools/call", { name: "backyard_list_sites" }), { token: conn.access_token, env: { ...PUB, REGISTRY: failing(["listSites"]) } });
    expect(list.json.result).toEqual({ content: [{ type: "text", text: `Internal error (request ${list.header("X-Request-Id")})` }], isError: true });
  });

  it("checks the method after authentication: 405 with Allow: POST", async () => {
    const { conn } = await guest();
    for (const method of ["GET", "DELETE", "PUT", "PATCH", "OPTIONS", "HEAD"]) {
      const r = await post(request("ping"), { token: conn.access_token, method });
      expect(r.status, method).toBe(405);
      expect(r.header("Allow"), method).toBe("POST");
      if (method !== "HEAD") expect(r.json, method).toEqual({ error: { code: "bad_request", message: "method not allowed" } });
      for (const [k] of r.res.headers) expect(k.startsWith("access-control-"), k).toBe(false);
    }
    const anon = await post(undefined, { method: "GET", headers: { Accept: "text/event-stream" } });
    expect(anon.status).toBe(401);
    expect(anon.header("WWW-Authenticate")).toBe(CHALLENGE);
  });

  it("checks the media types, then the protocol version", async () => {
    const { conn } = await guest();
    const t = conn.access_token;
    for (const ct of ["text/plain", null, "application/jsonx", "multipart/form-data"]) {
      const r = await post(request("ping"), { token: t, headers: { "Content-Type": ct } });
      expect(r.status, String(ct)).toBe(415);
      expect(r.json.error.code).toBe("bad_request");
    }
    for (const accept of ["text/html", "text/event-stream", "image/*"]) {
      const r = await post(request("ping"), { token: t, headers: { Accept: accept } });
      expect(r.status, accept).toBe(400);
      expect(r.json.error.code).toBe("bad_request");
    }
    const variants: Record<string, string | null>[] = [
      { "Content-Type": "application/json; charset=utf-8" },
      { "Content-Type": "Application/JSON" },
      { Accept: "*/*" },
      { Accept: "application/*" },
      { Accept: "text/html;q=0.9, application/json;q=0.1" },
      { Accept: null },
      { Accept: "" },
      { "MCP-Protocol-Version": null },
      { "MCP-Protocol-Version": "2025-06-18" },
      { "Mcp-Session-Id": "ignored", "Last-Event-ID": "1" },
    ];
    for (const headers of variants) {
      const r = await post(request("ping"), { token: t, headers });
      expect(r.status, `${JSON.stringify(headers)}: ${r.text}`).toBe(200);
    }
    for (const v of ["2025-03-26", "2026-07-28", "2024-11-05", "x".repeat(150)]) {
      const r = await post(request("ping"), { token: t, headers: { "MCP-Protocol-Version": v } });
      expect(r.status, v).toBe(400);
      expect(r.header("Content-Type")).toBe("application/json");
      expect(r.json).toEqual({
        jsonrpc: "2.0",
        id: null,
        error: { code: -32022, message: "Unsupported protocol version", data: { supported: ["2025-11-25", "2025-06-18"], requested: truncateBytes(v, 100) } },
      });
    }
    // The order: the media type before the version, the version before the body.
    expect((await post(request("ping"), { token: t, headers: { "Content-Type": "text/plain", "MCP-Protocol-Version": "1999-01-01" } })).status).toBe(415);
    const big = await post("x".repeat(LIMIT + 1), { token: t, headers: { "MCP-Protocol-Version": "1999-01-01" } });
    expect(big.status).toBe(400);
    expect(big.json.error.code).toBe(-32022);
  });

  it("limits the body to 2 × LIMITS.mcp_publish_bytes + 1 MiB (by Content-Length and while reading)", async () => {
    const { conn } = await guest();
    const msg = { error: { code: "too_large", message: `MCP requests are limited to ${LIMIT} bytes; publish over several calls` } };
    /** A ping of exactly n bytes. */
    const sized = (n: number) => {
      const head = '{"jsonrpc":"2.0","id":1,"method":"ping","pad":"';
      return head + "a".repeat(n - head.length - 2) + '"}';
    };
    const at = await post(sized(LIMIT), { token: conn.access_token });
    expect(at.status, at.text.slice(0, 200)).toBe(200);
    expect(at.json).toEqual({ jsonrpc: "2.0", id: 1, result: {} });
    const over = await post(sized(LIMIT + 1), { token: conn.access_token });
    expect(over.status).toBe(413);
    expect(over.json).toEqual(msg);
    // A streamed body of unknown length.
    const bytes = new TextEncoder().encode(sized(LIMIT + 1));
    const stream = new ReadableStream<Uint8Array>({
      start(c) {
        for (let i = 0; i < bytes.length; i += 1 << 20) c.enqueue(bytes.subarray(i, i + (1 << 20)));
        c.close();
      },
    });
    const h = { "Content-Type": "application/json", Authorization: `Bearer ${conn.access_token}` };
    const streamed = await call("/__backyard/mcp", { method: "POST", headers: h, body: stream }, PUB);
    expect(streamed.status).toBe(413);
    expect(await streamed.json()).toEqual(msg);
    // A smaller publish cap lowers the limit.
    const small = { ...PUB, ...limits({ mcp_publish_bytes: 65536 }) };
    const r = await post(sized(2 * 65536 + (1 << 20) + 1), { token: conn.access_token, env: small });
    expect(r.status).toBe(413);
    expect(r.json.error.message).toBe(`MCP requests are limited to ${2 * 65536 + (1 << 20)} bytes; publish over several calls`);
  });
});

describe("JSON-RPC (SPEC §14.1 steps 10–11)", () => {
  it("answers parse errors, batches and invalid requests with 400 and id null", async () => {
    const { mcp } = await guest();
    const parse = await mcp.post("{");
    expect(parse.status).toBe(400);
    expect(parse.json).toEqual({ jsonrpc: "2.0", id: null, error: { code: -32700, message: "Parse error" } });
    for (const raw of ["", "  ", "\ufeff{}", '{"jsonrpc":"2.0",}']) expect((await mcp.post(raw)).json.error.code, JSON.stringify(raw)).toBe(-32700);
    for (const batch of [[], [request("ping")]]) {
      const r = await mcp.post(batch);
      expect(r.status).toBe(400);
      expect(r.json).toEqual({ jsonrpc: "2.0", id: null, error: { code: -32600, message: "Invalid Request: JSON-RPC batches are not supported" } });
    }
    for (const body of [
      JSON.stringify("x"),
      "5",
      "null",
      { jsonrpc: "1.0", id: 1, method: "ping" },
      { id: 1, method: "ping" },
      { jsonrpc: 2, id: 1, method: "ping" },
      { jsonrpc: "2.0", id: null, method: "ping" },
      '{"jsonrpc":"2.0","id":1.5,"method":"ping"}',
      '{"jsonrpc":"2.0","id":1.0,"method":"ping"}',
      '{"jsonrpc":"2.0","id":1e3,"method":"ping"}',
      { jsonrpc: "2.0", id: {}, method: "ping" },
      { jsonrpc: "2.0", id: [1], method: "ping" },
      { jsonrpc: "2.0", id: true, method: "ping" },
      { jsonrpc: "2.0", id: 1, method: 5 },
      { jsonrpc: "2.0", id: 1, method: null },
      { jsonrpc: "2.0", id: 1 },
      { jsonrpc: "2.0" },
    ]) {
      const r = await mcp.post(body);
      expect(r.status, JSON.stringify(body)).toBe(400);
      expect(r.json, JSON.stringify(body)).toEqual({ jsonrpc: "2.0", id: null, error: { code: -32600, message: "Invalid Request" } });
    }
  });

  it("accepts notifications and client responses with 202 and an empty body", async () => {
    const { mcp } = await guest();
    for (const body of [
      { jsonrpc: "2.0", method: "notifications/initialized" },
      { jsonrpc: "2.0", method: "notifications/cancelled", params: { requestId: 1 } },
      { jsonrpc: "2.0", id: 7, result: {} },
      { jsonrpc: "2.0", id: "x", error: { code: 1, message: "x" } },
      { jsonrpc: "2.0", result: null },
    ]) {
      const r = await mcp.post(body);
      expect(r.status, JSON.stringify(body)).toBe(202);
      expect(r.text).toBe("");
    }
  });

  it("negotiates the protocol version in initialize", async () => {
    const { mcp } = await guest();
    for (const [asked, want] of [
      ["2025-06-18", "2025-06-18"],
      ["2025-11-25", "2025-11-25"],
      ["2024-11-05", "2025-11-25"],
      ["2026-07-28", "2025-11-25"],
    ]) {
      const res = await mcp.rpc("initialize", { protocolVersion: asked, capabilities: {}, clientInfo: { name: "test", version: "1" } });
      expect(res).toEqual({
        protocolVersion: want,
        capabilities: { tools: {} },
        serverInfo: { name: "backyard", title: "Backyard", version: expect.any(String) },
        instructions: MCP_CATALOG.instructions,
      });
    }
    // The member name is exact, as on the Go server.
    for (const params of [{}, { protocolVersion: 20250618 }, { protocolVersion: null }, undefined, null, [], { ProtocolVersion: "2025-06-18" }]) {
      const r = await mcp.post(request("initialize", params));
      expect(r.status).toBe(200);
      expect(r.json.error, JSON.stringify(params)).toEqual({ code: -32602, message: "Invalid params" });
    }
  });

  it("echoes ids as sent and needs no initialize", async () => {
    const { mcp } = await guest();
    const s = await mcp.post({ jsonrpc: "2.0", id: "abc", method: "ping" });
    expect(s.text).toBe('{"jsonrpc":"2.0","id":"abc","result":{}}');
    const big = await mcp.post('{"jsonrpc":"2.0","id":12345678901234567890,"method":"ping"}');
    expect(big.text).toBe('{"jsonrpc":"2.0","id":12345678901234567890,"result":{}}');
    // An integer as written, also -0 (the Go server's rule).
    const neg = await mcp.post('{"jsonrpc":"2.0","id":-0,"method":"ping"}');
    expect(neg.text).toBe('{"jsonrpc":"2.0","id":-0,"result":{}}');
    const zero = await mcp.post('{"jsonrpc":"2.0","id":0,"method":"ping"}');
    expect(zero.text).toBe('{"jsonrpc":"2.0","id":0,"result":{}}');
  });

  it("refuses a body that is not UTF-8 as invalid JSON, as the Go server does", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("utf8");
    const enc = new TextEncoder();
    const parts = JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      method: "tools/call",
      params: { name: "backyard_publish", arguments: { site, files: [{ path: "index.html", content: "x" }, { path: "aBAD.html", content: "y" }] } },
    }).split("BAD");
    // A truncated four-byte sequence (F0 9F 98) inside a path, and a stray byte in an id.
    const truncated = new Uint8Array([...enc.encode(parts[0]!), 0xf0, 0x9f, 0x98, ...enc.encode(parts[1]!)]);
    const stray = new Uint8Array([...enc.encode('{"jsonrpc":"2.0","id":"'), 0xff, ...enc.encode('","method":"ping"}')]);
    for (const body of [truncated, stray]) {
      const r = await mcp.post(body);
      expect(r.status).toBe(400);
      expect(r.text).toBe('{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}');
    }
    expect((await mcp.rpc("tools/call", { name: "backyard_site_files", arguments: { site } })).isError).toBe(true);
  });

  it("decodes lone surrogate escapes as U+FFFD, like encoding/json, but echoes the id as sent", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("surr");
    const body = JSON.stringify({
      jsonrpc: "2.0",
      id: "idSURR",
      method: "tools/call",
      params: { name: "backyard_publish", arguments: { site, files: [{ path: "index.html", content: "x" }, { path: "aSURR.html", content: "y" }] } },
    }).replaceAll("SURR", "\\ud800");
    const r = await mcp.post(body);
    expect(r.status).toBe(200);
    expect(r.text.startsWith('{"jsonrpc":"2.0","id":"id\\ud800","result":')).toBe(true);
    expect(r.json.result.isError, r.text).toBeUndefined();
    expect(JSON.parse(r.json.result.content[0].text)).toMatchObject({ site, files: 2 });
    expect(await mcp.text("backyard_read_site_file", { site, path: "a\ufffd.html" })).toBe("y");
  });

  it("lists the catalog verbatim and refuses other methods", async () => {
    const { mcp } = await guest();
    for (const params of [undefined, { cursor: "x" }]) {
      const res = await mcp.rpc("tools/list", params);
      expect(Object.keys(res)).toEqual(["tools"]);
      expect(res.tools).toEqual(MCP_CATALOG.tools);
      expect(JSON.stringify(res.tools)).toBe(JSON.stringify(MCP_CATALOG.tools));
    }
    for (const method of ["resources/list", "prompts/list", "logging/setLevel", "completion/complete", "server/discover", "nope"]) {
      const r = await mcp.post(request(method, {}));
      expect(r.status, method).toBe(200);
      expect(r.json.error, method).toEqual({ code: -32601, message: "Method not found" });
    }
    const unknown = await mcp.post(request("tools/call", { name: "nope", arguments: {} }));
    expect(unknown.json.error).toEqual({ code: -32602, message: "Unknown tool: nope" });
    for (const params of [{}, { name: 5 }, { name: "backyard_whoami", arguments: [1] }, { name: "backyard_whoami", arguments: null }, undefined, "x"]) {
      const r = await mcp.post(request("tools/call", params));
      expect(r.json.error, JSON.stringify(params)).toEqual({ code: -32602, message: "Invalid params" });
    }
    // An unknown tool's name never echoes a credential.
    const sneaky = await mcp.post(request("tools/call", { name: "byat_" + "x".repeat(30) }));
    expect(sneaky.json.error.message).toBe("Unknown tool: [REDACTED]");
  });

  it("validates the arguments against the input schema (a tool error)", async () => {
    const { mcp } = await guest();
    for (const [name, args] of [
      ["backyard_docs", { topic: "nope" }],
      ["backyard_whoami", { extra: 1 }],
      ["backyard_list_sites", { query: "q".repeat(201) }],
      ["backyard_site_files", {}],
      ["backyard_site_files", { site: "Not_A_Name" }],
      ["backyard_site_files", { site: "-x" }],
      ["backyard_read_site_file", { site: "a", path: "" }],
      ["backyard_publish", { site: "x", files: [{ path: "a.txt" }] }],
      ["backyard_publish", { site: "x" }],
      ["backyard_publish", { site: "x", files: [{ path: "a.txt", content: "x", encoding: "hex" }] }],
      ["backyard_publish", { site: "x", files: [], replace: "yes" }],
      ["backyard_publish", { site: "x", files: [], delete: [""] }],
      ["backyard_publish", { site: "x", files: Array.from({ length: 1001 }, (_, i) => ({ path: `f${i}`, content: "" })) }],
    ] as const) {
      const msg = await mcp.error(name, args as Record<string, unknown>);
      expect(msg, `${name} ${JSON.stringify(args).slice(0, 100)}`).toMatch(/^Invalid arguments: /);
    }
    // Absent arguments are {}.
    expect(typeof (await mcp.json("backyard_whoami")).hub_url).toBe("string");
  });
});

// =============================================================================

describe("the tools (SPEC §14.1)", () => {
  it("backyard_docs returns the skill and its references", async () => {
    const { mcp } = await guest();
    const skill = await mcp.text("backyard_docs");
    expect(skill).toMatch(/^---\nname: backyard/);
    expect(await mcp.text("backyard_docs", { topic: "skill" })).toBe(skill);
    expect(await mcp.text("backyard_docs", { topic: "api" })).toMatch(/backyard\.db/);
    for (const topic of ["recipes", "deploy"]) expect((await mcp.text("backyard_docs", { topic })).length).toBeGreaterThan(200);
  });

  it("backyard_whoami describes the connection", async () => {
    const { conn, mcp } = await guest();
    const who = await mcp.json("backyard_whoami", {});
    const me = (await (await call("/__backyard/api/me", { headers: conn.identity }, PUB)).json()) as { id: string; name: string };
    expect(Object.keys(who)).toEqual(["user", "hub_url", "site_url_template", "can_publish", "limits", "connection"]);
    expect(who.user).toEqual({ id: me.id, name: me.name, admin: false, anonymous: true });
    expect(who.hub_url).toBe(ORIGIN);
    expect(who.site_url_template).toBe(`${ORIGIN}/s/{site}/`);
    expect(who.can_publish).toBe(true);
    const info = (await (await call("/__backyard/api/info", {}, PUB)).json()) as { limits: Record<string, number> };
    expect(who.limits).toEqual({ max_publish_bytes: 2097152, max_files: info.limits.max_files, max_file_bytes: info.limits.max_file_bytes, max_site_bytes: info.limits.max_site_bytes, max_read_bytes: 131072 });
    expect(who.connection.client).toBe("Test client");
    expect(who.connection.expires_at).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/);
    expect(Date.parse(who.connection.expires_at)).toBeGreaterThan(Date.now() + 29 * 86400_000);
    // Guests can only read without anonymous deploys; host mode has its URL pattern.
    const ro = client(conn, MCP_ENV);
    expect((await ro.json("backyard_whoami")).can_publish).toBe(false);
    const host = client(conn, { ...PUB, BASE_DOMAIN: "backyard.test", URL_MODE: "host" });
    expect((await host.json("backyard_whoami")).site_url_template).toBe("https://{site}.backyard.test/");
  });

  it("publishes a new site that serves the exact bytes, and reads it back", async () => {
    const { conn, mcp } = await guest();
    const site = uniqueSite("mcp");
    const index = `<!doctype html><html><head><title>MCP ${site}</title><meta name="description" content="from the connector"></head><body>héllo</body></html>\n`;
    const hexPNG = "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8ffff3f0005fe02fea7d2a5b20000000049454e44ae426082";
    const png = Uint8Array.from(hexPNG.match(/../g)!, (x) => parseInt(x, 16));
    const files = [
      { path: "index.html", content: index },
      { path: "logo.png", content: btoa(String.fromCharCode(...png)), encoding: "base64" },
    ];
    const log = vi.spyOn(console, "log");
    const res = await mcp.json("backyard_publish", { site, files });
    const bytes = new TextEncoder().encode(index).length + png.length;
    expect(res).toEqual({ site, url: `${ORIGIN}/s/${site}/`, created: true, files: 2, uploaded: 2, unchanged: 0, deleted: 0, bytes, warnings: [] });
    // The audit line names the connector, the client and the grant (never a token).
    const audit = log.mock.calls.map((c) => String(c[0])).find((l) => l.includes('"deploy.commit"') && l.includes(site));
    expect(audit).toBeDefined();
    const a = JSON.parse(audit!) as Record<string, unknown>;
    expect(a).toMatchObject({ audit: true, action: "deploy.commit", site, via: "mcp", client: "Test client", grant: gidOf(conn.access_token).slice(0, 8), files: 2, uploaded: 2 });
    expect(audit).not.toMatch(/by(at|rt|ac|dc)_/);
    const page = await siteFile(site, "");
    expect(page.status).toBe(200);
    expect(page.headers.get("Content-Type")).toMatch(/^text\/html/);
    expect(await page.text()).toBe(index);
    const logo = await siteFile(site, "logo.png");
    expect(logo.headers.get("Content-Type")).toBe("image/png");
    expect(new Uint8Array(await logo.arrayBuffer())).toEqual(png);

    const list = await mcp.json("backyard_list_sites", { query: site.toUpperCase() });
    expect(Object.keys(list)).toEqual(["sites", "total", "truncated"]);
    expect(list.total).toBe(1);
    expect(list.truncated).toBe(false);
    const me = await userOf(mcp);
    expect(list.sites[0]).toEqual({
      name: site,
      url: `${ORIGIN}/s/${site}/`,
      title: `MCP ${site}`,
      description: "from the connector",
      updated_at: expect.stringMatching(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/),
      updated_by: me,
      file_count: 2,
      bytes,
    });
    expect(await mcp.json("backyard_site_files", { site })).toEqual({
      site,
      url: `${ORIGIN}/s/${site}/`,
      files: [
        { path: "index.html", size: new TextEncoder().encode(index).length },
        { path: "logo.png", size: png.length },
      ],
      total: 2,
      truncated: false,
    });
    expect(await mcp.text("backyard_read_site_file", { site, path: "index.html" })).toBe(index);
    // The registry record matches the files.
    expect((await siteRecord(site)).value).toMatchObject({ file_count: 2, bytes, updated_by: me, owners: [] });
  });

  it("merges by default, deletes, replaces and skips unchanged files", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("merge");
    const index = "<!doctype html><title>merge</title>\n";
    await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: index }, { path: "a.txt", content: "a" }] });
    const again = await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: index }, { path: "a.txt", content: "a" }] });
    expect(again).toMatchObject({ created: false, files: 2, uploaded: 0, unchanged: 2, deleted: 0 });
    const add = await mcp.json("backyard_publish", { site, files: [{ path: "js/app.js", content: "console.log(1);\n" }] });
    expect(add).toMatchObject({ files: 3, uploaded: 1, unchanged: 0 });
    expect(await (await siteFile(site, "a.txt")).text()).toBe("a");
    // Both alphabets, padding optional.
    const raw = new Uint8Array([0xfb, 0xff, 0xfe, 0x3e, 0x3f, 0x00]);
    const b64 = await mcp.json("backyard_publish", {
      site,
      files: [
        { path: "std.bin", content: "+//+Pj8A", encoding: "base64" },
        { path: "url.bin", content: "-__-Pj8A", encoding: "base64" },
        { path: "short.bin", content: "aGk", encoding: "base64" },
      ],
    });
    expect(b64.files).toBe(6);
    expect(new Uint8Array(await (await siteFile(site, "std.bin")).arrayBuffer())).toEqual(raw);
    expect(new Uint8Array(await (await siteFile(site, "url.bin")).arrayBuffer())).toEqual(raw);
    expect(await (await siteFile(site, "short.bin")).text()).toBe("hi");
    const del = await mcp.json("backyard_publish", { site, files: [], delete: ["std.bin", "url.bin", "short.bin", "nope.txt", "nope.txt"] });
    expect(del).toMatchObject({ files: 3, deleted: 3, uploaded: 0 });
    expect(del.warnings).toEqual(['"nope.txt" in delete does not exist.']);
    expect((await siteFile(site, "std.bin")).status).toBe(404);
    // A changed file is written; a content type follows the request.
    const ch = await mcp.json("backyard_publish", { site, files: [{ path: "a.txt", content: "b", content_type: "text/markdown" }] });
    expect(ch).toMatchObject({ files: 3, uploaded: 1, unchanged: 0 });
    expect(await (await siteFile(site, "a.txt")).text()).toBe("b");
    const rep = await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: index }], replace: true });
    expect(rep).toMatchObject({ files: 1, deleted: 2, unchanged: 1, uploaded: 0, warnings: [] });
    expect((await mcp.json("backyard_site_files", { site })).files).toEqual([{ path: "index.html", size: index.length }]);
    expect((await siteFile(site, "js/app.js")).status).toBe(404);
    expect((await siteRecord(site)).value).toMatchObject({ file_count: 1, bytes: index.length, title: "merge" });
  });

  it("warns about a missing index.html and binary files sent as text", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("warn");
    const res = await mcp.json("backyard_publish", {
      site,
      files: [
        { path: "about.html", content: "<p>about</p>" },
        { path: "photo.png", content: "not really a png" },
        { path: "font.woff2", content: "x" },
      ],
      delete: ["gone.txt"],
    });
    expect(res.created).toBe(true);
    expect(res.warnings).toEqual([
      `No index.html at the site root: ${ORIGIN}/s/${site}/ answers 404 until one is published.`,
      'photo.png was sent as text but looks binary; binary files need "encoding": "base64".',
      'font.woff2 was sent as text but looks binary; binary files need "encoding": "base64".',
      '"gone.txt" in delete does not exist.',
    ]);
    expect((await siteFile(site, "photo.png")).headers.get("Content-Type")).toBe("image/png");
  });

  it("refuses invalid publishes with the catalog's texts, changing nothing", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("bad");
    await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: "<p>x</p>" }] });
    const one = (path: string, content = "x") => [{ path, content }];
    const half = Math.floor(2097152 / 2) + 1;
    const cases: [string, Record<string, unknown>, string | RegExp][] = [
      ["E3 a reserved name", { site: "api", files: one("index.html") }, '"api" is reserved on this Backyard; choose another name.'],
      ["E4 nothing", { site, files: [] }, "Nothing to publish: give files, delete, or both."],
      ["E5 replace and delete", { site, files: one("index.html"), delete: ["a"], replace: true }, 'Use either "replace" or "delete", not both.'],
      // replace without files: E4 (no delete) or E5 (delete) come first.
      ["E4 replace with no files", { site, files: [], replace: true }, "Nothing to publish: give files, delete, or both."],
      ["E6 deleting every file", { site, files: [], delete: ["index.html"] }, "A site needs at least one file."],
      ["E7 invalid base64", { site, files: [{ path: "a.bin", content: "!!!!", encoding: "base64" }] }, 'files[0] (a.bin): "content" is not valid base64.'],
      ["E7 whitespace", { site, files: [...one("ok.txt"), { path: "b.bin", content: "aGVs bG8=", encoding: "base64" }] }, 'files[1] (b.bin): "content" is not valid base64.'],
      ["E7 mixed alphabets", { site, files: [{ path: "c.bin", content: "+_8A", encoding: "base64" }] }, /not valid base64/],
      ["E8 leaving the site", { site, files: one("../x") }, /^"\.\.\/x" is not an allowed path: .+\.$/],
      ["E8 absolute", { site, files: one("/x.html") }, /^"\/x\.html" is not an allowed path: /],
      ["E8 reserved path", { site, files: one("__backyard/x.js") }, /^"__backyard\/x\.js" is not an allowed path: /],
      ["E8 repository metadata", { site, files: one(".git/config") }, /^"\.git\/config" is not an allowed path: /],
      ["E8 an empty segment", { site, files: one("a//b") }, /is not an allowed path/],
      ["E8 a delete path", { site, files: one("ok.txt"), delete: ["../y"] }, /^"\.\.\/y" is not an allowed path/],
      ["E9 a duplicate", { site, files: [...one("a.txt"), ...one("a.txt")] }, '"a.txt" appears more than once.'],
      ["E10 in files and delete", { site, files: one("a.txt"), delete: ["a.txt"] }, '"a.txt" is both in files and in delete.'],
      [
        "E11 over the per-call limit",
        { site, files: [{ path: "a.txt", content: "a".repeat(half) }, { path: "b.txt", content: "b".repeat(half) }] },
        "This call carries 2.0 MiB of file content; the limit is 2.0 MiB per call. Send the files over several calls (files you do not list are kept).",
      ],
      ["E1 a control character", { site, files: [{ path: "a.txt", content: "x", content_type: "text/plain\u0001" }] }, /^Invalid arguments: files\[0\]\.content_type/],
      ["E14 a file and a folder", { site, files: one("index.html/x.html", "tree conflict content") }, "These paths would be both a file and a folder: index.html."],
      ["E14 within the call", { site, files: [...one("d"), ...one("d/e")] }, "These paths would be both a file and a folder: d."],
    ];
    for (const [what, args, want] of cases) {
      const msg = await mcp.error("backyard_publish", args);
      if (typeof want === "string") expect(msg, what).toBe(want);
      else expect(msg, what).toMatch(want);
      expect(msg, what).not.toMatch(/by(at|rt|ac|dc)_/);
    }
    expect((await mcp.json("backyard_site_files", { site })).files.map((f: { path: string }) => f.path)).toEqual(["index.html"]);
    // A refused merge uploaded nothing.
    const content = new TextEncoder().encode("tree conflict content");
    expect(await siteStub(testEnv(), site).hasBlob("s:" + (await sha256Hex(content)), content.length)).toEqual({ ok: true, value: false });
    // RESERVED_SITES adds names.
    const { conn } = await guest();
    const reserved = client(conn, { ...PUB, RESERVED_SITES: "intranet, Wiki" });
    expect(await reserved.error("backyard_publish", { site: "wiki", files: one("index.html") })).toBe('"wiki" is reserved on this Backyard; choose another name.');
  });

  it("applies the site limits to the resulting site (E12, E13) and the per-call cap at its boundary (E11)", async () => {
    const { conn } = await guest();
    const site = uniqueSite("lim");
    const small = client(conn, { ...PUB, ...limits({ max_files: 2, max_site_bytes: 100, max_file_bytes: 60, mcp_publish_bytes: 65536 }) });
    await small.json("backyard_publish", { site, files: [{ path: "a.txt", content: "a".repeat(40) }] });
    expect(await small.error("backyard_publish", { site, files: [{ path: "b.txt", content: "b".repeat(61) }] })).toBe("b.txt is 61 B; files may be at most 60 B.");
    expect(await small.error("backyard_publish", { site, files: [{ path: "b.txt", content: "b" }, { path: "c.txt", content: "c" }] })).toBe(
      "The site would have 3 files; this Backyard allows 2 per site.",
    );
    expect(await small.error("backyard_publish", { site, files: [{ path: "b.txt", content: "b".repeat(60) + "😀" }] })).toMatch(/files may be at most 60 B/);
    expect(await small.error("backyard_publish", { site, files: [{ path: "b.txt", content: "b".repeat(59) }, { path: "a.txt", content: "a".repeat(42) }] })).toBe(
      "The site would be 101 B; this Backyard allows 100 B per site.",
    );
    // Replacing a.txt keeps the site within both limits.
    expect(await small.json("backyard_publish", { site, files: [{ path: "b.txt", content: "b".repeat(50) }, { path: "a.txt", content: "a" }] })).toMatchObject({ files: 2, bytes: 51 });
    // The per-call cap counts decoded bytes: exactly the cap passes.
    const capped = client(conn, { ...PUB, ...limits({ mcp_publish_bytes: 65536 }) });
    const site2 = uniqueSite("cap");
    const b64 = btoa("x".repeat(30000));
    expect(await capped.json("backyard_publish", { site: site2, files: [{ path: "a.bin", content: b64, encoding: "base64" }, { path: "b.txt", content: "y".repeat(65536 - 30000) }] })).toMatchObject({ bytes: 65536 });
    expect(await capped.error("backyard_publish", { site: site2, files: [{ path: "c.txt", content: "z".repeat(65537) }] })).toMatch(/^This call carries 64\.0 KiB of file content; the limit is 64\.0 KiB per call\./);
  });

  it("stores large files in R2 (FILE_INLINE_MAX) at the default 2 MiB cap", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("r2");
    const big = "<!doctype html><title>Big page</title>\n" + "<p>" + "b".repeat(100_000) + "</p>\n";
    const huge = "h".repeat(2097152 - big.length);
    const res = await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: big }, { path: "huge.txt", content: huge }] });
    expect(res).toMatchObject({ created: true, files: 2, uploaded: 2, bytes: 2097152 });
    expect(await (await siteFile(site, "")).text()).toBe(big);
    expect((await (await siteFile(site, "huge.txt")).text()).length).toBe(huge.length);
    // The record's title comes from the sent index.html; later merges read it from R2.
    expect((await siteRecord(site)).value).toMatchObject({ title: "Big page", file_count: 2 });
    await mcp.json("backyard_publish", { site, files: [{ path: "small.txt", content: "s" }] });
    expect((await siteRecord(site)).value).toMatchObject({ title: "Big page", file_count: 3 });
    // read_site_file reads R2 files up to 128 KiB and refuses larger ones.
    expect(await mcp.text("backyard_read_site_file", { site, path: "index.html" })).toBe(big);
    expect(await mcp.error("backyard_read_site_file", { site, path: "huge.txt" })).toBe(`huge.txt is ${formatBytes(huge.length)}; files up to 128 KiB can be read.`);
    // One byte over the cap.
    expect(await mcp.error("backyard_publish", { site, files: [{ path: "huge.txt", content: huge + "!" }, { path: "index.html", content: big }] })).toMatch(/per call/);
    // Unchanged R2 content is not stored again.
    expect(await mcp.json("backyard_publish", { site, files: [{ path: "huge.txt", content: huge }] })).toMatchObject({ uploaded: 0, unchanged: 1 });
  });

  it("derives the preview like the title, from the files the site has after each publish (SPEC §7.2)", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("pubprev");
    const index = '<!doctype html><title>P</title><meta property="og:image" content="./img/cover.svg?v=1">';
    await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: index }] });
    expect((await siteRecord(site)).value).not.toHaveProperty("preview");
    // A merge adds the file the kept index.html names.
    await mcp.json("backyard_publish", { site, files: [{ path: "img/cover.svg", content: "<svg/>" }] });
    expect((await siteRecord(site)).value).toMatchObject({ preview: "img/cover.svg" });
    const res = await call(`/__backyard/api/sites/${site}/preview`, {}, PUB);
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("image/svg+xml");
    expect(await res.text()).toBe("<svg/>");
    // Deleting it falls back to preview.*; replacing the site without one removes it.
    await mcp.json("backyard_publish", { site, files: [{ path: "preview.svg", content: "<svg></svg>" }], delete: ["img/cover.svg"] });
    expect((await siteRecord(site)).value).toMatchObject({ preview: "preview.svg" });
    await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: index }], replace: true });
    expect((await siteRecord(site)).value).not.toHaveProperty("preview");
    // index.html in R2 (FILE_INLINE_MAX): from the sent bytes, then read back from R2 by a later merge.
    const big = index + "<p>" + "b".repeat(100_000) + "</p>";
    await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: big }, { path: "img/cover.svg", content: "<svg/>" }] });
    expect((await siteRecord(site)).value).toMatchObject({ title: "P", preview: "img/cover.svg" });
    expect(unwrap(await siteStub(testEnv(), site).siteInfo()).titleR2Key).not.toBe("");
    await mcp.json("backyard_publish", { site, files: [{ path: "preview.png", content: "png" }] });
    expect((await siteRecord(site)).value).toMatchObject({ title: "P", preview: "img/cover.svg", file_count: 3 });
    // backyard_list_sites keeps its own fields.
    const list = await mcp.json("backyard_list_sites", { query: site });
    expect(list.sites[0].name).toBe(site);
    expect(list.sites[0]).not.toHaveProperty("preview");
  });

  it("keeps the preview in step with concurrent publishes (the record settles on the files)", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("raceprev");
    await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: '<meta property="og:image" content="cover.svg">' }] });
    await Promise.all([
      mcp.json("backyard_publish", { site, files: [{ path: "cover.svg", content: "<svg/>" }] }),
      ...Array.from({ length: 5 }, (_, i) => mcp.json("backyard_publish", { site, files: [{ path: `part${i}.js`, content: `// ${i}\n` }] })),
    ]);
    expect((await siteRecord(site)).value).toMatchObject({ file_count: 7, preview: "cover.svg" });
  });

  it("settles the title, preview and updated_at on the files also when a concurrent publish keeps the file count and size", async () => {
    const { conn, mcp } = await guest();
    const site = uniqueSite("settle");
    const index = (img: string, title: string) => `<title>${title}</title><meta property="og:image" content="${img}">`;
    await mcp.json("backyard_publish", {
      site,
      files: [
        { path: "index.html", content: index("img/a.svg", "Old") },
        { path: "app.js", content: "aaaa" },
        { path: "img/a.svg", content: "<svg>A</svg>" },
        { path: "img/b.svg", content: "<svg>B</svg>" },
      ],
    });
    expect((await siteRecord(site)).value).toMatchObject({ title: "Old", preview: "img/a.svg" });
    // Publish A (app.js, same size) merges; before it writes its record, publish B
    // (index.html of the same size naming img/b.svg) runs to its end. A then writes
    // its older record over B's, and its settle has to put the site's values back.
    let latest = null as Record<string, unknown> | null;
    let merges = 0;
    const SITE = wrapSiteNamespace({
      async deployMerge(real, ...args) {
        const r = await real(...args);
        if (merges++ === 0) {
          await new Promise((done) => setTimeout(done, 5)); // B switches at a later millisecond
          await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: index("img/b.svg", "New") }] });
          latest = (await siteRecord(site)).value;
        }
        return r;
      },
    });
    await client(conn, { ...PUB, SITE }).json("backyard_publish", { site, files: [{ path: "app.js", content: "bbbb" }] });
    expect(merges).toBe(1);
    expect(latest).toMatchObject({ title: "New", preview: "img/b.svg" });
    const info = unwrap(await siteStub(testEnv(), site).siteInfo());
    expect(info).toMatchObject({ title: "New", preview: "img/b.svg", fileCount: 4 });
    expect((await siteRecord(site)).value).toMatchObject({
      title: "New",
      preview: "img/b.svg",
      file_count: 4,
      bytes: info.bytes,
      updated_at: formatMicros(info.updatedAt * 1000),
    });
    expect(latest!.updated_at).toBe(formatMicros(info.updatedAt * 1000));
  });

  it("merges concurrent publishes of one site atomically: no file is lost", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("race");
    await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: "<p>i</p>" }] });
    const n = 6;
    const results = await Promise.all(Array.from({ length: n }, (_, i) => mcp.json("backyard_publish", { site, files: [{ path: `part${i}.js`, content: `// ${i}\n` }] })));
    for (const r of results) expect(r.uploaded).toBe(1);
    const tree = await mcp.json("backyard_site_files", { site });
    expect(tree.files.map((f: { path: string }) => f.path)).toEqual(["index.html", ...Array.from({ length: n }, (_, i) => `part${i}.js`)]);
    // The last publish to commit reports every file; the record catches up with the files.
    expect(Math.max(...results.map((r) => r.files))).toBe(n + 1);
    expect((await siteRecord(site)).value).toMatchObject({ file_count: n + 1 });
    // Parallel deletes and additions.
    await Promise.all([
      mcp.json("backyard_publish", { site, files: [], delete: ["part0.js"] }),
      mcp.json("backyard_publish", { site, files: [{ path: "late.js", content: "1" }] }),
      mcp.json("backyard_publish", { site, files: [], delete: ["part1.js"] }),
    ]);
    const after = (await mcp.json("backyard_site_files", { site })).files.map((f: { path: string }) => f.path);
    expect(after).toEqual(["index.html", "late.js", ...Array.from({ length: n - 2 }, (_, i) => `part${i + 2}.js`)]);
    expect((await siteRecord(site)).value).toMatchObject({ file_count: after.length });
  });

  it("guards a site someone else published last (E16) and the owners (E15)", async () => {
    const site = uniqueSite("guard");
    await deploy(site, { "index.html": "<p>by the deploy token</p>" });
    const { mcp } = await guest();
    const msg = await mcp.error("backyard_publish", { site, files: [{ path: "index.html", content: "<p>mine</p>" }] });
    expect(msg).toMatch(
      new RegExp(
        `^${site} was last published by token:deploy on \\d{4}-\\d\\d-\\d\\d \\d\\d:\\d\\d UTC\\. Nothing was changed\\. Ask the user whether to replace it; if so, call again with "overwrite": true, otherwise choose another name\\.$`,
      ),
    );
    expect(await (await siteFile(site, "")).text()).toBe("<p>by the deploy token</p>");
    const ok = await mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: "<p>mine</p>" }], overwrite: true });
    expect(ok).toMatchObject({ created: false, uploaded: 1 });
    const me = await userOf(mcp);
    expect((await siteRecord(site)).value).toMatchObject({ updated_by: me });
    // Now the guest published last: no overwrite needed for them, but for another guest.
    expect(await mcp.json("backyard_publish", { site, files: [{ path: "x.txt", content: "x" }] })).toMatchObject({ files: 2 });
    const other = await guest();
    expect(await other.mcp.error("backyard_publish", { site, files: [{ path: "y.txt", content: "y" }] })).toMatch(new RegExp(`^${site} was last published by ${me.replace(/[:]/g, "\\$&")} on `));
    // OWNERSHIP=owner: the deploy token owns its new site; guests are never owners.
    const owned = uniqueSite("owned");
    const ownerEnv = { ...PUB, OWNERSHIP: "owner" };
    await deploy(owned, { "index.html": "<p>o</p>" }, { envOverrides: ownerEnv });
    const g = client(other.conn, ownerEnv);
    expect(await g.error("backyard_publish", { site: owned, files: [{ path: "index.html", content: "x" }], overwrite: true })).toBe(
      `${owned} belongs to token:deploy; you are not an owner. Ask an owner to add you, or publish under another name.`,
    );
    // A guest's first publish of a new site makes no owner.
    const fresh = uniqueSite("noowner");
    await g.json("backyard_publish", { site: fresh, files: [{ path: "index.html", content: "x" }] });
    expect((await siteRecord(fresh)).value).toMatchObject({ owners: [] });
  });

  it("follows the anonymous rules, the deploy quota and the site cap (E17, E19, E27)", async () => {
    const ro = await guest(MCP_ENV);
    expect(await ro.mcp.error("backyard_publish", { site: uniqueSite("ro"), files: [{ path: "index.html", content: "x" }] })).toBe(
      "This Backyard has no sign-in and does not accept anonymous publishing, so this connection can only read. Ask the Backyard admin to set up sign-in or allow anonymous deploys.",
    );
    // deploys_per_day is keyed by the user (user:<id>, guests included).
    const quota = { ...PUB, ...limits({ deploys_per_day: 1 }) };
    const a = await guest(quota);
    const site = uniqueSite("quota");
    await a.mcp.json("backyard_publish", { site, files: [{ path: "index.html", content: "1" }] });
    expect(await a.mcp.error("backyard_publish", { site, files: [{ path: "index.html", content: "2" }] })).toMatch(/^Publishing limit reached \(1 per day\)\. Try again in \d+ minutes\.$/);
    const me = await userOf(a.mcp);
    const counted = await registry(testEnv()).counters([`deploy|user:${me}`]);
    expect(counted).toEqual({ ok: true, value: { [`deploy|user:${me}`]: 1 } });
    // The overwrite guard charges nothing.
    const b = await guest(quota);
    expect(await b.mcp.error("backyard_publish", { site, files: [{ path: "index.html", content: "3" }] })).toMatch(/overwrite/);
    expect(await b.mcp.json("backyard_publish", { site: uniqueSite("quota2"), files: [{ path: "index.html", content: "4" }] })).toMatchObject({ created: true });
    // The platform's site cap.
    const capped = client(b.conn, { ...PUB, ...limits({ max_sites: 1 }) });
    expect(await capped.error("backyard_publish", { site: uniqueSite("cap"), files: [{ path: "index.html", content: "x" }] })).toBe(
      "This Backyard already has its maximum number of sites (1).",
    );
  });

  it("reads site files with the catalog's checks (E22–E25) and scrubs credentials", async () => {
    const { mcp } = await guest();
    const site = uniqueSite("read");
    const secretish = `token byat_${"Q".repeat(24)} code byac_${"z".repeat(20)} secret ${SECRET} deploy ${DEPLOY_TOKEN}`;
    await mcp.json("backyard_publish", {
      site,
      files: [
        { path: "index.html", content: "<p>hi</p>" },
        { path: "notes.txt", content: secretish },
        { path: "bad.txt", content: btoa("a\xff\xfeb"), encoding: "base64" },
        { path: "nul.txt", content: btoa("a\x00b"), encoding: "base64" },
        { path: "img.jpg", content: btoa("jpg"), encoding: "base64" },
        { path: "big.txt", content: "x".repeat(131073) },
        { path: "edge.txt", content: "y".repeat(131072) },
      ],
    });
    const none = uniqueSite("none");
    expect(await mcp.error("backyard_read_site_file", { site: none, path: "index.html" })).toBe(`There is no site named "${none}" on this Backyard.`);
    expect(await mcp.error("backyard_site_files", { site: none })).toBe(`There is no site named "${none}" on this Backyard.`);
    expect(await mcp.error("backyard_read_site_file", { site, path: "../x" })).toMatch(/^"\.\.\/x" is not an allowed path: .+\.$/);
    expect(await mcp.error("backyard_read_site_file", { site, path: "img.jpg" })).toBe("img.jpg is a binary file and cannot be read as text; link to it by URL instead.");
    expect(await mcp.error("backyard_read_site_file", { site, path: "missing.png" })).toMatch(/is a binary file/);
    expect(await mcp.error("backyard_read_site_file", { site, path: "missing.html" })).toBe(`${site} has no file "missing.html".`);
    expect(await mcp.error("backyard_read_site_file", { site, path: "big.txt" })).toBe("big.txt is 128.0 KiB; files up to 128 KiB can be read.");
    expect((await mcp.text("backyard_read_site_file", { site, path: "edge.txt" })).length).toBe(131072);
    expect(await mcp.error("backyard_read_site_file", { site, path: "nul.txt" })).toBe("nul.txt is a binary file and cannot be read as text; link to it by URL instead.");
    expect(await mcp.text("backyard_read_site_file", { site, path: "bad.txt" })).toBe("a\ufffdb");
    const notes = await mcp.text("backyard_read_site_file", { site, path: "notes.txt" });
    expect(notes).toBe("token [REDACTED] code [REDACTED] secret [REDACTED] deploy [REDACTED]");
    // The size check of a file stored in the SiteDO (not in R2).
    const inline = client((await guest()).conn, { ...PUB, FILE_INLINE_MAX: String(1 << 20) });
    const site2 = uniqueSite("read-inline");
    await inline.json("backyard_publish", { site: site2, files: [{ path: "big.txt", content: "x".repeat(131073) }, { path: "edge.txt", content: "é".repeat(65536) }] });
    expect(await inline.error("backyard_read_site_file", { site: site2, path: "big.txt" })).toBe("big.txt is 128.0 KiB; files up to 128 KiB can be read.");
    expect(await inline.text("backyard_read_site_file", { site: site2, path: "edge.txt" })).toBe("é".repeat(65536));
  });

  it("lists sites most recently updated first, filtered by name, title or description", async () => {
    const { conn, mcp } = await guest();
    const tag = uniqueSite("lst");
    const first = `${tag}-a`;
    const second = `${tag}-b`;
    await mcp.json("backyard_publish", { site: first, files: [{ path: "index.html", content: "<title>Alpha</title>" }] });
    await new Promise((r) => setTimeout(r, 5));
    await mcp.json("backyard_publish", { site: second, files: [{ path: "index.html", content: `<title>Beta ${tag}</title>` }] });
    const list = await mcp.json("backyard_list_sites", { query: tag });
    expect(list.sites.map((s: { name: string }) => s.name)).toEqual([second, first]);
    expect(list.total).toBe(2);
    expect((await mcp.json("backyard_list_sites", { query: `BETA ${tag}` })).sites.map((s: { name: string }) => s.name)).toEqual([second]);
    expect((await mcp.json("backyard_list_sites", { query: `${tag}-nothing` })).sites).toEqual([]);
    // Owners appear with OWNERSHIP=owner only.
    const site = uniqueSite("lst-own");
    await deploy(site, { "index.html": "<p>o</p>" }, { envOverrides: { OWNERSHIP: "owner" } });
    const owned = client(conn, { ...PUB, OWNERSHIP: "owner" });
    expect((await owned.json("backyard_list_sites", { query: site })).sites[0].owners).toEqual(["token:deploy"]);
    expect((await mcp.json("backyard_list_sites", { query: site })).sites[0].owners).toBeUndefined();
  });

  it("charges LIMITS.mcp_calls_per_day per user for tools/call only (429)", async () => {
    const env = { ...PUB, ...limits({ mcp_calls_per_day: 2 }) };
    const { mcp } = await guest(env);
    await mcp.json("backyard_whoami");
    await mcp.text("backyard_docs", { topic: "api" });
    // Unknown tools and other methods are not tool calls.
    expect((await mcp.post(request("tools/call", { name: "nope" }))).json.error.code).toBe(-32602);
    expect(await mcp.rpc("ping")).toEqual({});
    expect((await mcp.rpc("tools/list")).tools).toHaveLength(6);
    const r = await mcp.post(request("tools/call", { name: "backyard_whoami", arguments: {} }));
    expect(r.status).toBe(429);
    expect(Number(r.header("Retry-After"))).toBeGreaterThan(0);
    expect(r.json).toEqual({ error: { code: "rate_limited", message: "daily connector tool-call limit reached (2 per day); it resets at midnight UTC" } });
    // Per user: another guest still has its calls.
    const other = await guest(env);
    expect((await other.mcp.json("backyard_whoami")).user.anonymous).toBe(true);
  });
});

describe("credential isolation (SPEC §8, §14.1)", () => {
  it("an MCP access token is no API or gateway credential", async () => {
    const { conn, mcp } = await guest();
    for (const path of ["/__backyard/api/me", "/__backyard/api/deploy/me", "/__backyard/api/mcp/grants", "/__backyard/api/sites"]) {
      const r = await call(path, { headers: { Authorization: `Bearer ${conn.access_token}` } }, PUB);
      expect(r.status, path).toBe(401);
    }
    expect(await mcp.rpc("ping")).toEqual({});
  });

  it("never sets a cookie, even for a browser that sends one", async () => {
    const { conn, mcp } = await guest();
    const r = await mcp.post(request("ping"), { headers: { Cookie: conn.identity.Cookie ?? "" } });
    expect(r.status).toBe(200);
    const anon = await post(request("ping"), { headers: { Cookie: "backyard_anon=forged" } });
    expect(anon.status).toBe(401);
  });

  it("publishes info.mcp_url only while the connector is on", async () => {
    for (const path of ["/__backyard/api/info", "/__backyard/api/deploy/info"]) {
      const on = (await (await call(path, {}, MCP_ENV)).json()) as Record<string, unknown>;
      expect(on.mcp_url, path).toBe(ORIGIN + "/__backyard/mcp");
      expect(Object.keys(on).slice(-2)).toEqual(["mcp_url", "edition"]);
      const off = (await (await call(path)).json()) as Record<string, unknown>;
      expect("mcp_url" in off, path).toBe(false);
      const http = (await (await call(path, {}, { ...MCP_ENV, PUBLIC_URL: "http://backyard.test" })).json()) as Record<string, unknown>;
      expect("mcp_url" in http, path).toBe(false);
    }
  });
});

describe("Cloudflare Access users (AUTH_MODE=access)", () => {
  const TEAM = "acme.cloudflareaccess.com";
  const AUD = "aud-mcp-tools";
  const accessEnv: Partial<Env> = { ...MCP_ENV, AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: AUD, ADMIN_GROUPS: "platform" };

  async function setup() {
    const pair = (await crypto.subtle.generateKey(
      { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
      true,
      ["sign", "verify"],
    )) as CryptoKeyPair;
    const jwk = (await crypto.subtle.exportKey("jwk", pair.publicKey)) as JsonWebKey;
    const people: Record<string, { name: string; groups: string[] }> = {
      "alice@acme.com": { name: "Alice Smith", groups: ["eng", "platform"] },
      "bob@acme.com": { name: "Bob Jones", groups: ["eng"] },
    };
    let current = "alice@acme.com";
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url === `https://${TEAM}/cdn-cgi/access/certs`) return Response.json({ keys: [{ ...jwk, kid: "k1", alg: "RS256", use: "sig" }] });
      if (url === `https://${TEAM}/cdn-cgi/access/get-identity`) {
        const p = people[current]!;
        return Response.json({ name: p.name, email: current, groups: p.groups.map((g, i) => ({ id: String(i), name: g })) });
      }
      throw new Error(`unexpected fetch ${url}`);
    });
    const identity = async (email: string) => {
      current = email;
      const now = Math.floor(Date.now() / 1000);
      const claims = { aud: [AUD], email, sub: email, iss: `https://${TEAM}`, iat: now, exp: now + 3600, identity_nonce: "n-" + email };
      const h = b64url(new TextEncoder().encode(JSON.stringify({ alg: "RS256", kid: "k1", typ: "JWT" })));
      const p = b64url(new TextEncoder().encode(JSON.stringify(claims)));
      const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", pair.privateKey, new TextEncoder().encode(`${h}.${p}`)));
      return { "Cf-Access-Jwt-Assertion": `${h}.${p}.${b64url(sig)}` };
    };
    return { identity };
  }

  it("publishes as the signed-in person: owners, the overwrite guard and admin rights", async () => {
    const { identity } = await setup();
    const ownerEnv = { ...accessEnv, OWNERSHIP: "owner" };
    const aliceConn = await mcpConnect({ identity: await identity("alice@acme.com"), env: accessEnv });
    const alice = client(aliceConn, ownerEnv);
    const bob = client(await mcpConnect({ identity: await identity("bob@acme.com"), env: accessEnv }), ownerEnv);
    expect((await alice.json("backyard_whoami")).user).toEqual({ id: "alice@acme.com", email: "alice@acme.com", name: "Alice Smith", admin: true, anonymous: false });
    expect((await bob.json("backyard_whoami")).user).toEqual({ id: "bob@acme.com", email: "bob@acme.com", name: "Bob Jones", admin: false, anonymous: false });
    expect((await bob.json("backyard_whoami")).can_publish).toBe(true);
    // Bob's new site is his.
    const site = uniqueSite("acc");
    expect(await bob.json("backyard_publish", { site, files: [{ path: "index.html", content: "<title>Bob</title>" }] })).toMatchObject({ created: true });
    expect((await siteRecord(site)).value).toMatchObject({ owners: ["bob@acme.com"], created_by: "bob@acme.com", updated_by: "bob@acme.com" });
    // Alice is an admin: the owner rule lets her in, the overwrite guard still asks.
    expect(await alice.error("backyard_publish", { site, files: [{ path: "index.html", content: "<title>Alice</title>" }] })).toMatch(/^.+ was last published by bob@acme\.com on .+"overwrite": true/);
    expect(await alice.json("backyard_publish", { site, files: [{ path: "index.html", content: "<title>Alice</title>" }], overwrite: true })).toMatchObject({ uploaded: 1 });
    expect((await siteRecord(site)).value).toMatchObject({ owners: ["bob@acme.com"], updated_by: "alice@acme.com", title: "Alice" });
    // Bob still owns it (overwrite needed: Alice published last).
    expect(await bob.json("backyard_publish", { site, files: [{ path: "b.txt", content: "b" }], overwrite: true })).toMatchObject({ files: 2 });
    // Alice's own site: Bob is no owner.
    const hers = uniqueSite("acc-a");
    await alice.json("backyard_publish", { site: hers, files: [{ path: "index.html", content: "a" }] });
    expect(await bob.error("backyard_publish", { site: hers, files: [{ path: "index.html", content: "b" }], overwrite: true })).toBe(
      `${hers} belongs to alice@acme.com; you are not an owner. Ask an owner to add you, or publish under another name.`,
    );
    // Without admin groups Alice is a plain user: admin is recomputed on every request.
    const plain = client(aliceConn, { ...ownerEnv, ADMIN_GROUPS: "" });
    expect((await plain.json("backyard_whoami")).user.admin).toBe(false);
    expect(await plain.error("backyard_publish", { site, files: [{ path: "a.txt", content: "a" }], overwrite: true })).toMatch(/^.+ belongs to bob@acme\.com; you are not an owner\./);
  });
});

describe("SiteDO.mergePlan and deployMerge", () => {
  const file = async (path: string, content: string): Promise<DeployFile> => {
    const bytes = new TextEncoder().encode(content);
    return { path, size: bytes.length, sha256: await sha256Hex(bytes), content_type: "text/plain; charset=utf-8" };
  };

  it("plans a merge without changing anything", async () => {
    const site = uniqueSite("plan");
    await deploy(site, { "index.html": "<p>i</p>", "a.txt": "a" });
    const stub = siteStub(testEnv(), site);
    const lim = { maxFiles: 0, maxSiteBytes: 0 };
    const same = await file("a.txt", "a");
    const other = await file("b.txt", "never stored");
    expect(await stub.mergePlan([same, other], [], false, lim)).toEqual({ ok: true, value: { unchanged: ["a.txt"], refusal: null } });
    expect(await stub.mergePlan([other], [], false, { maxFiles: 2, maxSiteBytes: 0 })).toEqual({ ok: true, value: { unchanged: [], refusal: { ok: false, error: "files", count: 3 } } });
    expect(await stub.mergePlan([other], [], true, { maxFiles: 2, maxSiteBytes: 0 })).toEqual({ ok: true, value: { unchanged: [], refusal: null } });
    expect(await stub.mergePlan([], ["a.txt", "index.html"], false, lim)).toEqual({ ok: true, value: { unchanged: [], refusal: { ok: false, error: "empty" } } });
    // A site that never stored anything stays without storage.
    const fresh = siteStub(testEnv(), uniqueSite("plan-new"));
    expect(await fresh.mergePlan([], ["x"], false, lim)).toEqual({ ok: true, value: { unchanged: [], refusal: { ok: false, error: "empty" } } });
    expect(await fresh.listFiles()).toEqual({ ok: true, value: [] });
  });

  it("refuses contents that are not stored and changes nothing on a refusal", async () => {
    const site = uniqueSite("dm");
    await deploy(site, { "index.html": "<p>i</p>", "a.txt": "a" });
    const stub = siteStub(testEnv(), site);
    const lim = { maxFiles: 0, maxSiteBytes: 0 };
    const missing = await stub.deployMerge([await file("new.txt", "never uploaded")], [], false, lim);
    expect(missing).toEqual({ ok: true, value: { ok: false, error: "missing", paths: ["new.txt"] } });
    const same = await file("b.txt", "a"); // content the site already holds
    const r = await stub.deployMerge([same], [], false, { maxFiles: 2, maxSiteBytes: 0 });
    expect(r).toEqual({ ok: true, value: { ok: false, error: "files", count: 3 } });
    expect(await stub.deployMerge([same], [], false, { maxFiles: 0, maxSiteBytes: 9 })).toEqual({ ok: true, value: { ok: false, error: "bytes", bytes: 10 } });
    expect(await stub.deployMerge([], ["index.html", "a.txt"], false, lim)).toEqual({ ok: true, value: { ok: false, error: "empty" } });
    const tree = await file("a.txt/x", "a");
    expect(await stub.deployMerge([tree], [], false, lim)).toEqual({ ok: true, value: { ok: false, error: "tree", paths: ["a.txt"] } });
    expect(((await stub.listFiles()) as { ok: true; value: { path: string }[] }).value.map((f) => f.path)).toEqual(["a.txt", "index.html"]);
    // A merge with content the site holds under another path needs no upload.
    const m = await stub.deployMerge([same], ["a.txt"], false, lim);
    expect(m.ok && m.value.ok && { deleted: m.value.deleted, fileCount: m.value.fileCount, unchanged: m.value.unchanged, existed: m.value.existed, hasIndex: m.value.hasIndex }).toEqual({
      deleted: 1,
      fileCount: 2,
      unchanged: 0,
      existed: true,
      hasIndex: true,
    });
    expect(await (await call(`/s/${site}/b.txt`)).text()).toBe("a");
    expect((await call(`/s/${site}/a.txt`)).status).toBe(404);
  });
});
