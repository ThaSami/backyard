/**
 * The remote MCP endpoint POST /__backyard/mcp (SPEC §14.1): Streamable HTTP,
 * stateless, JSON responses only (no sessions, no SSE, no batches), protocol
 * versions 2025-11-25 and 2025-06-18. It is a hand-written JSON-RPC handler,
 * the twin of the Go server's internal/mcpremote, over the shared tool catalog
 * (src/generated/mcp.ts from internal/mcpremote/tools.json).
 *
 * Only the MCP access tokens of the connector's authorization server
 * (src/oauth.ts) are accepted: never cookies, API tokens, DEPLOY_TOKEN or
 * Access JWTs, and authenticate() is never called, so no guest cookie is
 * minted. Every request re-reads the grant, so a revoked connection stops at
 * once. The tools are in src/mcp-tools.ts.
 *
 * src/hub.ts calls serveMcp for /__backyard/mcp after mcpPathGate (connector
 * on, canonical hub host).
 */
import { MCP_CATALOG, type McpTool } from "./generated/mcp";
import { truncateBytes } from "./gateway";
import { HttpError, errorResponse, isPlainObject, readBody, requestID, type ErrorCode } from "./http";
import { TOOLS, type ToolCall, type ToolResult } from "./mcp-tools";
import { mcpAllowedOrigin, mcpChallenge, mcpPathGate, mcpResourceMetadataURL, verifyMcpAccessToken } from "./oauth";
import type { RequestContext } from "./router";
import { chargeOrThrow, VERSION } from "./stubs";

/** Protocol versions (SPEC §14.1): the latest, and the one of requests without the header. */
const VERSION_LATEST = "2025-11-25";
const VERSION_FALLBACK = "2025-06-18";
export const PROTOCOL_VERSIONS = [VERSION_LATEST, VERSION_FALLBACK];

/** JSON-RPC error codes. */
const PARSE_ERROR = -32700;
const INVALID_REQUEST = -32600;
const METHOD_NOT_FOUND = -32601;
const INVALID_PARAMS = -32602;
const INTERNAL_ERROR = -32603;
const UNSUPPORTED_VERSION = -32022;

const MSG_UNAUTHORIZED = "connect this app to Backyard to sign in";

/** Bytes of a validation detail quoted in "Invalid arguments: …". */
const MAX_DETAIL = 500;

/** A JSON-RPC error member. */
interface RpcError {
  code: number;
  message: string;
  data?: unknown;
}

/** An HTTP-level error of the gate (the SPEC §5 envelope). */
function gateError(status: number, code: ErrorCode, message: string, headers?: HeadersInit): Response {
  return errorResponse(new HttpError(status, code, message), headers);
}

/** One JSON-RPC response (compact, id echoed as sent). */
function rpcResponse(status: number, idText: string, body: { result: unknown } | { error: RpcError }): Response {
  const member = "error" in body ? `"error":${JSON.stringify(body.error)}` : `"result":${JSON.stringify(body.result)}`;
  return new Response(`{"jsonrpc":"2.0","id":${idText},${member}}`, {
    status,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

/** The answer to a message whose id is unknown (HTTP 400, id null). */
function rpcFailure(code: number, message: string, data?: unknown): Response {
  const error: RpcError = { code, message };
  if (data !== undefined) error.data = data;
  return rpcResponse(400, "null", { error });
}

/** Serves /__backyard/mcp on the canonical hub host: the gate of SPEC §14.1, in its order. */
export async function serveMcp(ctx: RequestContext): Promise<Response> {
  const req = ctx.req;
  // 1. Enabled, on the canonical hub host (hub.ts checks it too).
  const off = mcpPathGate(ctx);
  if (off) return off;
  // 3. Origin: absent, or the hub's, the request's own, or Claude's.
  const origin = req.headers.get("Origin") ?? "";
  if (origin !== "" && !(plainOrigin(origin) && mcpAllowedOrigin(ctx, origin))) {
    return gateError(403, "forbidden", "requests from this origin are not accepted");
  }
  // 4. Credentials: only Backyard MCP access tokens.
  const authz = req.headers.get("Authorization");
  if (authz !== null && severalCredentials(authz)) {
    return gateError(400, "bad_request", "send exactly one Authorization header", {
      "WWW-Authenticate": `Bearer error="invalid_request", resource_metadata="${mcpResourceMetadataURL(ctx)}"`,
    });
  }
  const token = bearerOf(authz ?? "");
  if (token === "") return gateError(401, "unauthenticated", MSG_UNAUTHORIZED, { "WWW-Authenticate": mcpChallenge(ctx) });
  let verified: Awaited<ReturnType<typeof verifyMcpAccessToken>>;
  try {
    verified = await verifyMcpAccessToken(ctx, token);
  } catch (e) {
    if (e instanceof HttpError && e.status === 503) return errorResponse(e);
    const id = requestID(req);
    console.error(`backyard: mcp: token check failed (request ${id}): ${(e as Error)?.message ?? e}`);
    return gateError(503, "internal", `Backyard could not check this connection right now (request ${id}); try again`);
  }
  if (!verified.ok) return gateError(401, "unauthenticated", MSG_UNAUTHORIZED, { "WWW-Authenticate": mcpChallenge(ctx, verified.error) });
  // 5 (rate limit per user) is the Go server's; the Worker charges tool calls (12).
  // 6. Method.
  if (req.method !== "POST") {
    const e = new HttpError(405, "bad_request", "method not allowed");
    e.allow = ["POST"];
    return errorResponse(e);
  }
  // 7. Media types.
  const contentType = (req.headers.get("Content-Type") ?? "").split(";")[0]!.trim().toLowerCase();
  if (contentType !== "application/json") return gateError(415, "bad_request", "send the JSON-RPC message as application/json");
  const accept = req.headers.get("Accept") ?? "";
  if (accept.trim() !== "" && !acceptsJSON(accept)) {
    return gateError(400, "bad_request", "the Accept header must admit application/json (this server never streams)");
  }
  // 8. Protocol version.
  const protocol = (req.headers.get("MCP-Protocol-Version") ?? "").trim() || VERSION_FALLBACK;
  if (!PROTOCOL_VERSIONS.includes(protocol)) {
    return rpcFailure(UNSUPPORTED_VERSION, "Unsupported protocol version", { supported: PROTOCOL_VERSIONS, requested: truncateBytes(protocol, 100) });
  }
  // 9. Body.
  const limit = maxBodyBytes(ctx);
  let body: Uint8Array;
  try {
    body = await readBody(req, limit);
  } catch (e) {
    if (e instanceof HttpError && e.status === 413) return gateError(413, "too_large", `MCP requests are limited to ${limit} bytes; publish over several calls`);
    return gateError(400, "bad_request", "could not read the request body");
  }
  // 10. JSON-RPC parse.
  const msg = parseMessage(body);
  if ("error" in msg) return rpcFailure(msg.error.code, msg.error.message);
  // 11. Dispatch.
  if (msg.kind !== "request") return new Response(null, { status: 202, headers: { "Cache-Control": "no-store" } });
  const call: ToolCall = { ctx, user: verified.user, grant: verified.grant, requestID: requestID(req) };
  try {
    const out = await dispatch(call, msg.method, msg.params);
    if (out instanceof Response) return out;
    return rpcResponse(200, msg.id, out);
  } catch (e) {
    console.error(`backyard: mcp: ${msg.method} failed (request ${call.requestID}): ${(e as Error)?.stack ?? e}`);
    return rpcResponse(200, msg.id, { error: internalError(call) });
  }
}

/** The request body limit: 2 × LIMITS.mcp_publish_bytes + 1 MiB (SPEC §11). */
export function maxBodyBytes(ctx: RequestContext): number {
  return 2 * ctx.cfg.limits.mcp_publish_bytes + (1 << 20);
}

/**
 * Whether an Origin value is scheme://host[:port] (a trailing "/" allowed):
 * like the Go server, an origin with a path, query, fragment or user info is
 * not accepted.
 */
function plainOrigin(v: string): boolean {
  return /^[A-Za-z][A-Za-z0-9+.-]*:\/\/[^/?#\s@]+\/?$/.test(v.trim());
}

/**
 * Whether an Authorization value holds more than one credential. The runtime
 * joins repeated headers with ", ", so two or more comma-separated parts that
 * each start like "<scheme> <credentials>" were separate headers (a single
 * credential's auth-params, "a=1, b=2", never look like that).
 */
function severalCredentials(v: string): boolean {
  if (!v.includes(",")) return false;
  return v.split(",").filter((p) => /^\s*[!#$%&'*+.^_`|~0-9A-Za-z-]+\s+\S/.test(p)).length >= 2;
}

/** The token of "Bearer <token>" (the scheme case-insensitive), or "": other schemes count as absent. */
function bearerOf(v: string): string {
  const h = v.trim();
  const sp = h.indexOf(" ");
  if (sp < 0 || h.slice(0, sp).toLowerCase() !== "bearer") return "";
  return h.slice(sp + 1).trim();
}

/** Whether an Accept value admits a JSON answer. */
function acceptsJSON(accept: string): boolean {
  return accept.split(",").some((part) => ["application/json", "application/*", "*/*"].includes(part.split(";")[0]!.trim().toLowerCase()));
}

// --- JSON-RPC framing ---------------------------------------------------------------

type Message = { kind: "request"; id: string; method: string; params: unknown } | { kind: "notification" | "response" } | { error: RpcError };

/** A JSON number without fraction or exponent. */
const INTEGER_RE = /^-?(0|[1-9][0-9]*)$/;

/** The JSON-RPC message of a body (one message per POST; SPEC §14.1 step 10). */
function parseMessage(body: Uint8Array): Message {
  const invalid: Message = { error: { code: INVALID_REQUEST, message: "Invalid Request" } };
  // The source text of numeric "id" members (JSON.parse source text access):
  // an id must be an integer as written, and is echoed as written.
  const idSources = new Map<object, string>();
  let v: unknown;
  try {
    // ignoreBOM: a byte order mark is no JSON, as on the Go server.
    const text = new TextDecoder("utf-8", { fatal: false, ignoreBOM: true }).decode(body);
    v = JSON.parse(text, function (this: unknown, key: string, value: unknown, context?: { source?: string }) {
      if (key === "id" && typeof value === "number" && typeof context?.source === "string") idSources.set(this as object, context.source);
      return value;
    } as (this: unknown, key: string, value: unknown) => unknown);
  } catch {
    return { error: { code: PARSE_ERROR, message: "Parse error" } };
  }
  if (Array.isArray(v)) return { error: { code: INVALID_REQUEST, message: "Invalid Request: JSON-RPC batches are not supported" } };
  if (!isPlainObject(v) || v.jsonrpc !== "2.0") return invalid;
  const hasMethod = Object.hasOwn(v, "method");
  if (hasMethod && typeof v.method !== "string") return invalid;
  let id = "";
  if (Object.hasOwn(v, "id")) {
    if (typeof v.id === "string") {
      id = JSON.stringify(v.id);
    } else if (typeof v.id === "number") {
      const src = idSources.get(v);
      if (src !== undefined ? !INTEGER_RE.test(src) : !Number.isSafeInteger(v.id)) return invalid;
      id = src ?? String(v.id);
    } else {
      return invalid; // null, objects, arrays, booleans
    }
  }
  if (hasMethod && id !== "") return { kind: "request", id, method: v.method as string, params: v.params };
  if (hasMethod) return { kind: "notification" };
  if (Object.hasOwn(v, "result") || Object.hasOwn(v, "error")) return { kind: "response" };
  return invalid;
}

function internalError(c: ToolCall): RpcError {
  return { code: INTERNAL_ERROR, message: `Internal error (request ${c.requestID})` };
}

const invalidParams = (): { error: RpcError } => ({ error: { code: INVALID_PARAMS, message: "Invalid params" } });

/** Answers a request: its result or error member, or an HTTP response (the tool-call limit). */
async function dispatch(c: ToolCall, method: string, params: unknown): Promise<{ result: unknown } | { error: RpcError } | Response> {
  switch (method) {
    case "initialize": {
      if (!isPlainObject(params) || typeof params.protocolVersion !== "string") return invalidParams();
      const asked = params.protocolVersion;
      return {
        result: {
          protocolVersion: PROTOCOL_VERSIONS.includes(asked) ? asked : VERSION_LATEST,
          capabilities: { tools: {} },
          serverInfo: { name: "backyard", title: "Backyard", version: VERSION },
          instructions: MCP_CATALOG.instructions,
        },
      };
    }
    case "ping":
      return { result: {} };
    case "tools/list":
      // The catalog verbatim; params.cursor is ignored, there is no nextCursor.
      return { result: { tools: MCP_CATALOG.tools } };
    case "tools/call":
      return callTool(c, params);
    default:
      return { error: { code: METHOD_NOT_FOUND, message: "Method not found" } };
  }
}

/** The tools of the catalog by name. */
const CATALOG = new Map<string, McpTool>(MCP_CATALOG.tools.map((t) => [t.name, t]));

/** tools/call (SPEC §14.1). */
async function callTool(c: ToolCall, params: unknown): Promise<{ result: unknown } | { error: RpcError } | Response> {
  if (!isPlainObject(params) || typeof params.name !== "string") return invalidParams();
  let args: unknown = {};
  if (Object.hasOwn(params, "arguments")) {
    args = params.arguments;
    if (!isPlainObject(args)) return invalidParams();
  }
  const name = params.name;
  const tool = CATALOG.get(name);
  const run = Object.hasOwn(TOOLS, name) ? TOOLS[name] : undefined;
  if (!tool || !run) return { error: { code: INVALID_PARAMS, message: `Unknown tool: ${scrub(c, truncateBytes(name, 100))}` } };
  // 12. The daily tool-call limit of the person (429 with Retry-After).
  try {
    await chargeOrThrow(c.ctx.env, [{ key: `mcp|user:${c.user.id}`, limit: c.ctx.cfg.limits.mcp_calls_per_day }]);
  } catch (e) {
    if (e instanceof HttpError && e.status === 429) return errorResponse(e);
    throw e;
  }
  let res: ToolResult;
  const detail = validateSchema(tool.inputSchema, args);
  if (detail !== null) res = { text: "Invalid arguments: " + truncateBytes(detail, MAX_DETAIL), isError: true };
  else res = await run(c, args as Record<string, unknown>);
  const out: Record<string, unknown> = { content: [{ type: "text", text: scrub(c, res.text) }] };
  if (res.isError) out.isError = true;
  return { result: out };
}

/** The connector's tokens, codes and registered client ids (SPEC §8.4 token formats). */
const CREDENTIAL_RE = /by(?:at|rt|ac|dc)_[A-Za-z0-9_.-]{16,}/g;
const REDACTED = "[REDACTED]";

/**
 * Keeps credentials out of a tool result: the connector's tokens, codes and
 * client ids, BACKYARD_SECRET and DEPLOY_TOKEN (SPEC §14.1). File text and
 * site metadata are written by other people and may quote anything.
 */
export function scrub(c: ToolCall, s: string): string {
  let out = s.replace(CREDENTIAL_RE, REDACTED);
  for (const secret of [c.ctx.cfg.secret, c.ctx.cfg.deployToken]) {
    if (secret && secret.length >= 8) out = out.split(secret).join(REDACTED);
  }
  return out;
}

// --- input schema validation --------------------------------------------------------

/**
 * The JSON Schema keywords validateSchema implements: exactly those the
 * catalog uses (test/mcp.test.ts checks that it uses no others), plus the
 * annotations it ignores.
 */
export const SCHEMA_KEYWORDS = [
  "type",
  "properties",
  "required",
  "additionalProperties",
  "enum",
  "minLength",
  "maxLength",
  "pattern",
  "maxItems",
  "items",
  "description",
  "title",
];

const patterns = new Map<string, RegExp>();

function patternOf(p: string): RegExp {
  let re = patterns.get(p);
  if (!re) {
    re = new RegExp(p, "u");
    patterns.set(p, re);
  }
  return re;
}

function hasType(v: unknown, type: string): boolean {
  switch (type) {
    case "object":
      return isPlainObject(v);
    case "array":
      return Array.isArray(v);
    case "string":
      return typeof v === "string";
    case "boolean":
      return typeof v === "boolean";
    case "number":
      return typeof v === "number";
    case "integer":
      return typeof v === "number" && Number.isInteger(v);
    case "null":
      return v === null;
  }
  return false;
}

/** Length of a string in code points (JSON Schema counts characters, not UTF-16 units). */
function codePoints(s: string): number {
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
      const d = s.charCodeAt(i + 1);
      if (d >= 0xdc00 && d <= 0xdfff) i++;
    }
    n++;
  }
  return n;
}

function where(at: string): string {
  return at === "" ? "" : `${at}: `;
}

/**
 * Validates v against a schema of the catalog (JSON Schema 2020-12, the
 * keywords of SCHEMA_KEYWORDS); returns the first problem, or null.
 */
export function validateSchema(schema: Record<string, unknown>, v: unknown, at = ""): string | null {
  const type = schema.type;
  if (typeof type === "string" && !hasType(v, type)) return `${where(at)}must be ${type === "array" || type === "object" || type === "integer" ? "an" : "a"} ${type}`;
  if (Array.isArray(schema.enum) && !schema.enum.some((e) => (typeof e === "object" && e !== null ? JSON.stringify(e) === JSON.stringify(v) : e === v))) {
    return `${where(at)}must be one of ${schema.enum.map((e) => JSON.stringify(e)).join(", ")}`;
  }
  if (typeof v === "string") {
    const n = typeof schema.minLength === "number" || typeof schema.maxLength === "number" ? codePoints(v) : 0;
    if (typeof schema.minLength === "number" && n < schema.minLength) return `${where(at)}must be at least ${schema.minLength} characters long`;
    if (typeof schema.maxLength === "number" && n > schema.maxLength) return `${where(at)}must be at most ${schema.maxLength} characters long`;
    if (typeof schema.pattern === "string" && !patternOf(schema.pattern).test(v)) return `${where(at)}must match the pattern ${schema.pattern}`;
  }
  if (Array.isArray(v)) {
    if (typeof schema.maxItems === "number" && v.length > schema.maxItems) return `${where(at)}must have at most ${schema.maxItems} items`;
    if (isPlainObject(schema.items)) {
      for (let i = 0; i < v.length; i++) {
        const e = validateSchema(schema.items, v[i], `${at || "arguments"}[${i}]`);
        if (e) return e;
      }
    }
  }
  if (isPlainObject(v)) {
    const props = isPlainObject(schema.properties) ? schema.properties : {};
    if (Array.isArray(schema.required)) {
      for (const k of schema.required) {
        if (typeof k === "string" && !Object.hasOwn(v, k)) return `${where(at)}missing required property ${JSON.stringify(k)}`;
      }
    }
    for (const [k, val] of Object.entries(v)) {
      const sub = Object.hasOwn(props, k) ? props[k] : undefined;
      if (isPlainObject(sub)) {
        const e = validateSchema(sub, val, at === "" ? k : `${at}.${k}`);
        if (e) return e;
      } else if (schema.additionalProperties === false) {
        return `${where(at)}unexpected property ${JSON.stringify(truncateBytes(k, 100))}`;
      }
    }
  }
  return null;
}
