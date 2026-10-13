/**
 * HTTP helpers shared by the Worker and the Durable Objects: the JSON error
 * envelope of SPEC §5 ({"error":{"code","message","login_url"?}}), JSON
 * responses and bounded body readers.
 */

/** Error codes of the JSON API (SPEC §5). */
export type ErrorCode =
  | "bad_request"
  | "unauthenticated"
  | "forbidden"
  | "not_found"
  | "conflict"
  | "too_large"
  | "rate_limited"
  | "unsupported"
  | "upstream_error"
  | "internal";

/** An HTTP error rendered as the JSON envelope (or an HTML page for browsers). */
export class HttpError extends Error {
  readonly status: number;
  readonly code: ErrorCode;
  loginURL?: string;
  /** Seconds for the Retry-After header. */
  retryAfter?: number;
  /** Heading of the HTML error page. */
  title?: string;
  /** Allowed methods (405 responses carry an Allow header). */
  allow?: string[];

  constructor(status: number, code: ErrorCode, message: string, opts: { loginURL?: string; retryAfter?: number; title?: string } = {}) {
    super(message);
    this.name = "HttpError";
    this.status = status;
    this.code = code;
    if (opts.loginURL) this.loginURL = opts.loginURL;
    if (opts.retryAfter !== undefined) this.retryAfter = opts.retryAfter;
    if (opts.title) this.title = opts.title;
  }
}

export const badRequest = (msg: string) => new HttpError(400, "bad_request", msg);
export const unauthenticated = (msg: string, loginURL?: string) =>
  new HttpError(401, "unauthenticated", msg, loginURL ? { loginURL } : {});
export const forbidden = (msg: string) => new HttpError(403, "forbidden", msg);
export const notFound = (msg: string) => new HttpError(404, "not_found", msg);
export const conflict = (msg: string) => new HttpError(409, "conflict", msg);
export const tooLarge = (msg: string) => new HttpError(413, "too_large", msg);
export const rateLimited = (msg: string, retryAfter?: number) =>
  new HttpError(429, "rate_limited", msg, retryAfter !== undefined ? { retryAfter } : {});
export const unsupported = (msg: string) => new HttpError(501, "unsupported", msg);
export const upstream = (msg: string, status = 502) => new HttpError(status, "upstream_error", msg);
export const internal = (msg = "internal error") => new HttpError(500, "internal", msg);
export const methodNotAllowed = (allow: string[]) =>
  Object.assign(new HttpError(405, "bad_request", `method not allowed (allowed: ${allow.join(", ")})`), { allow });

/** Plain error description that crosses the Durable Object RPC boundary. */
export interface WireError {
  status: number;
  code: ErrorCode;
  message: string;
}

/** Result of a Durable Object RPC call (errors are values, not exceptions). */
export type Result<T> = { ok: true; value: T } | { ok: false; error: WireError };

export function ok<T>(value: T): Result<T> {
  return { ok: true, value };
}

export function fail<T = never>(e: HttpError | WireError): Result<T> {
  return { ok: false, error: { status: e.status, code: e.code, message: e.message } };
}

/** Returns the value of an RPC result or throws its error as an HttpError. */
export function unwrap<T>(r: Result<T>): T {
  if (r.ok) return r.value;
  throw new HttpError(r.error.status, r.error.code, r.error.message);
}

/** Converts any thrown value into an HttpError (unknown errors become 500). */
export function toHttpError(err: unknown): HttpError {
  if (err instanceof HttpError) return err;
  return internal();
}

/** JSON response with Cache-Control: no-store (unless overridden). */
export function json(data: unknown, status = 200, headers?: HeadersInit): Response {
  const h = new Headers(headers);
  h.set("Content-Type", "application/json");
  if (!h.has("Cache-Control")) h.set("Cache-Control", "no-store");
  return new Response(JSON.stringify(data) + "\n", { status, headers: h });
}

/** Renders err as the SPEC §5 JSON envelope. */
export function errorResponse(err: HttpError, headers?: HeadersInit): Response {
  const h = new Headers(headers);
  if (err.retryAfter !== undefined && err.retryAfter > 0) h.set("Retry-After", String(Math.ceil(err.retryAfter)));
  if (err.allow?.length) h.set("Allow", err.allow.join(", "));
  const detail: Record<string, string> = { code: err.code, message: err.message };
  if (err.loginURL) detail.login_url = err.loginURL;
  return json({ error: detail }, err.status, h);
}

/** Copies a response so that its headers become mutable (keeps websockets). */
export function mutable(res: Response): Response {
  const init: ResponseInit & { webSocket?: WebSocket | null } = {
    status: res.status,
    statusText: res.statusText,
    headers: new Headers(res.headers),
  };
  if (res.webSocket) init.webSocket = res.webSocket;
  return new Response(res.status === 101 ? null : res.body, init);
}

/** Reads at most limit bytes of the request body (413 above). */
export async function readBody(req: Request, limit: number): Promise<Uint8Array> {
  const declared = req.headers.get("Content-Length");
  if (declared !== null && Number(declared) > limit) throw tooLarge(`request body exceeds ${limit} bytes`);
  if (!req.body) return new Uint8Array(0);
  const reader = req.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > limit) {
      await reader.cancel().catch(() => {});
      throw tooLarge(`request body exceeds ${limit} bytes`);
    }
    chunks.push(value);
  }
  return concat(chunks, total);
}

/** Concatenates byte arrays. */
export function concat(chunks: Uint8Array[], total?: number): Uint8Array {
  const n = total ?? chunks.reduce((a, c) => a + c.byteLength, 0);
  if (chunks.length === 1 && chunks[0]!.byteLength === n) return chunks[0]!;
  const out = new Uint8Array(n);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.byteLength;
  }
  return out;
}

const decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });

/** Reads and parses a JSON body of at most limit bytes. */
export async function readJSON(req: Request, limit: number): Promise<unknown> {
  const bytes = await readBody(req, limit);
  if (bytes.byteLength === 0) throw badRequest("request body is empty; expected JSON");
  let text: string;
  try {
    text = decoder.decode(bytes);
  } catch {
    throw badRequest("request body is not valid UTF-8");
  }
  try {
    return JSON.parse(text) as unknown;
  } catch (e) {
    throw badRequest(`invalid JSON: ${(e as Error).message}`);
  }
}

/** Reads a JSON object body (400 for anything else). */
export async function readObject(req: Request, limit: number): Promise<Record<string, unknown>> {
  const v = await readJSON(req, limit);
  if (!isPlainObject(v)) throw badRequest("request body must be a JSON object");
  return v;
}

/** Reports whether v is a JSON object (not null, not an array). */
export function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** Truncates s to at most n characters for messages. */
export function truncate(s: string, n: number): string {
  return s.length <= n ? s : s.slice(0, n) + "…";
}

/** Header value of a single cookie, or null. */
export function getCookie(req: Request, name: string): string | null {
  const header = req.headers.get("Cookie");
  if (!header) return null;
  for (const part of header.split(";")) {
    const i = part.indexOf("=");
    if (i < 0) continue;
    if (part.slice(0, i).trim() === name) return part.slice(i + 1).trim();
  }
  return null;
}

/** Bearer token of the Authorization header, or "" when absent. */
export function bearerToken(req: Request): string {
  const h = (req.headers.get("Authorization") ?? "").trim();
  const sp = h.indexOf(" ");
  if (sp < 0) return "";
  if (h.slice(0, sp).toLowerCase() !== "bearer") return "";
  return h.slice(sp + 1).trim();
}

/** Escapes text for HTML. */
export function escapeHTML(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]!);
}

const REQUEST_ID_RE = /^[A-Za-z0-9._-]{8,128}$/;

/**
 * The id of a request (SPEC §3, §15: returned as X-Request-Id): a valid
 * incoming X-Request-Id (8–128 of A-Z a-z 0-9 . _ -, like the Go server),
 * else Cloudflare's Ray id (it names the request in Cloudflare's logs), else
 * a random one.
 */
export function requestID(req: Request): string {
  for (const v of [req.headers.get("X-Request-Id"), req.headers.get("CF-Ray")]) {
    if (v && REQUEST_ID_RE.test(v)) return v;
  }
  const b = crypto.getRandomValues(new Uint8Array(12));
  return [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
}

/**
 * Wraps a response with the headers every Backyard response carries: the
 * security headers and, when given, X-Request-Id.
 */
export function withSecurityHeaders(res: Response, requestId?: string): Response {
  const out = mutable(res);
  out.headers.set("X-Content-Type-Options", "nosniff");
  if (!out.headers.has("Referrer-Policy")) out.headers.set("Referrer-Policy", "strict-origin-when-cross-origin");
  if (requestId) out.headers.set("X-Request-Id", requestId);
  return out;
}
