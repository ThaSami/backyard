/**
 * Site host requests (SPEC §2): the SDK, static files (§3), uploaded files,
 * the site API (§5: identity, database, files, AI, warehouse) and the
 * websocket (§6). Work on site data is forwarded to the site's Durable Object.
 */
import { AIService } from "./ai/service";
import type { ChatRequest, Message, Part } from "./ai/types";
import { textOf } from "./ai/types";
import { serveSDK } from "./assets";
import { accessLoginURL, authenticate, rateKey } from "./auth";
import { maxUploadBytes } from "./config";
import { decodeAnyBase64 } from "./crypto";
import { collectionError, decodeQueryBody, docIDError, DOC_ID_RE, normalizeQuery, parseListParams, type RawQuery } from "./db/query";
import { newULID } from "./db/ulid";
import {
  HttpError,
  badRequest,
  forbidden,
  isPlainObject,
  json,
  methodNotAllowed,
  notFound,
  readJSON,
  readObject,
  tooLarge,
  unsupported,
  unwrap,
} from "./http";
import { cleanUser, normalizeEmail, type User } from "./identity";
import { sniff } from "./mime";
import { R2_KEY_HEADER, resolveR2Body } from "./object";
import { pageNotFound, siteNotFound } from "./pages";
import { parseOrigin, redirect, siteNameError, siteRoot, siteURL, type RequestContext } from "./router";
import { MULTIPART_OVERHEAD, type UploadRow } from "./site-do";
import { chargeOrThrow, registry, siteStub } from "./stubs";
import { siteMetaJSON } from "./sitemeta";

/** Upper bound of JSON bodies of small requests. */
const SMALL_BODY = 1 << 20;
/** Upper bound of /ai/chat bodies (inline base64 images). */
const CHAT_BODY = 32 << 20;
/** Interval of keep-alive comments on silent AI streams. */
const SSE_HEARTBEAT_MS = 15_000;

/** Request headers forwarded to the site object for static/upload serving. */
const FORWARD_HEADERS = ["Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"];

function forwardInit(ctx: RequestContext): RequestInit {
  const h = new Headers();
  for (const k of FORWARD_HEADERS) {
    const v = ctx.req.headers.get(k);
    if (v !== null) h.set(k, v);
  }
  // Redirects (directory → slash) are for the client, never followed here.
  return { method: ctx.req.method === "HEAD" ? "HEAD" : "GET", headers: h, redirect: "manual" };
}

/** Rejects methods other than GET/HEAD with 405. */
function requireGet(ctx: RequestContext): void {
  if (ctx.req.method !== "GET" && ctx.req.method !== "HEAD") throw methodNotAllowed(["GET", "HEAD"]);
}

/**
 * Authenticates a browser request for a page or file. Unlike API calls no
 * login redirect is possible in access mode (Cloudflare Access itself
 * intercepts unauthenticated page loads), so failures render a page.
 */
async function authenticatePage(ctx: RequestContext): Promise<User> {
  try {
    return await authenticate(ctx);
  } catch (e) {
    if (e instanceof HttpError && e.status === 401 && ctx.cfg.authMode === "access") {
      throw new HttpError(401, "unauthenticated", "This Backyard is protected by Cloudflare Access, but this request did not come through it. Open the site through its protected address.", {
        title: "Sign in required",
      });
    }
    throw e;
  }
}

/** Dispatches a request for a site (ctx.kind === "site"). */
export async function serveSite(ctx: RequestContext): Promise<Response> {
  const p = ctx.path;
  if (siteNameError(ctx.site, ctx.cfg.reserved)) throw siteNotFound(ctx.site);
  if (p === "/__backyard/backyard.js" || p === "/__backyard/backyard.mjs" || p === "/__backyard/backyard.d.ts") {
    requireGet(ctx);
    return serveSDK(ctx);
  }
  if (p === "/__backyard/auth" || p.startsWith("/__backyard/auth/")) {
    let q = ctx.url.search;
    if (p === "/__backyard/auth/login" && !ctx.url.searchParams.get("rd")) {
      q += (q ? "&" : "?") + "rd=" + encodeURIComponent(siteRoot(ctx));
    }
    return redirect(ctx.publicURL + ctx.rawPath + q, 302);
  }
  if (p === "/__backyard/api" || p.startsWith("/__backyard/api/")) return siteAPI(ctx, p.slice("/__backyard/api".length) || "/");
  if (p.startsWith("/__backyard/files/")) return serveUploadedFile(ctx);
  if (p === "/__backyard" || p.startsWith("/__backyard/")) throw notFound(`no such endpoint: ${p.slice(0, 200)}`);
  return serveStatic(ctx);
}

/** Sites known to be deployed (per isolate): site → expiry (ms). */
const deployedSites = new Map<string, number>();
const DEPLOYED_SITE_TTL_MS = 60_000;

/**
 * Whether a site has been deployed (it is in the registry; commits write it).
 * Static requests for other names are answered without waking — and thereby
 * creating — a Durable Object, so probing random site names costs no storage
 * writes of the free plan's daily allowance.
 */
async function siteDeployed(ctx: RequestContext): Promise<boolean> {
  const now = Date.now();
  if ((deployedSites.get(ctx.site) ?? 0) > now) return true;
  const meta = unwrap(await registry(ctx.env).getSite(ctx.site));
  if (!meta) {
    deployedSites.delete(ctx.site);
    return false;
  }
  if (deployedSites.size > 10_000) deployedSites.clear();
  deployedSites.set(ctx.site, now + DEPLOYED_SITE_TTL_MS);
  return true;
}

/** Static files (SPEC §3), resolved by the site object. */
async function serveStatic(ctx: RequestContext): Promise<Response> {
  requireGet(ctx);
  await authenticatePage(ctx);
  if (!(await siteDeployed(ctx))) throw siteNotFound(ctx.site);
  const qs = new URLSearchParams({ path: ctx.path, loc: ctx.url.pathname, q: ctx.url.search });
  const res = await siteStub(ctx.env, ctx.site).fetch(`https://site/static?${qs}`, forwardInit(ctx));
  const nf = res.headers.get("x-backyard-not-found");
  if (nf) throw nf === "site" ? siteNotFound(ctx.site) : pageNotFound();
  return finishObject(ctx, res);
}

/** Converts an object response of the site (inline or R2-backed) for the client. */
async function finishObject(ctx: RequestContext, res: Response): Promise<Response> {
  if (res.status >= 400 && !res.headers.get("ETag") && (res.headers.get("Content-Type") ?? "").includes("application/json")) {
    const body = (await res.json()) as { error?: { code?: string; message?: string } };
    throw new HttpError(res.status, (body.error?.code as HttpError["code"]) ?? "internal", body.error?.message ?? "error");
  }
  if (res.headers.has(R2_KEY_HEADER)) return resolveR2Body(res, ctx.env.R2, ctx.req.method);
  return res;
}

/** /__backyard/files/{id}/{name}: uploaded bytes (Range supported). */
async function serveUploadedFile(ctx: RequestContext): Promise<Response> {
  requireGet(ctx);
  await authenticatePage(ctx);
  const id = ctx.path.slice("/__backyard/files/".length).split("/")[0] ?? "";
  if (!DOC_ID_RE.test(id)) throw notFound("file not found");
  const res = await siteStub(ctx.env, ctx.site).fetch(`https://site/upload-file?id=${encodeURIComponent(id)}`, forwardInit(ctx));
  if (res.headers.get("x-backyard-not-found")) throw notFound("file not found");
  return finishObject(ctx, res);
}

// --- API gate -----------------------------------------------------------------

const touched = new Map<string, number>();
const TOUCH_EVERY_MS = 10 * 60 * 1000;

/** Records an identified user in the user cache (throttled per isolate). */
function touchUser(ctx: RequestContext, u: User): void {
  if (u.anonymous || !u.email || u.id.startsWith("anon:") || u.id.startsWith("token:")) return;
  const now = Date.now();
  const last = touched.get(u.id);
  if (last && now - last < TOUCH_EVERY_MS) return;
  if (touched.size > 10_000) touched.clear();
  touched.set(u.id, now);
  ctx.waitUntil(
    registry(ctx.env)
      .touchUser(cleanUser(u))
      .catch((e: unknown) => console.warn(`backyard: user cache update failed: ${(e as Error).message}`)),
  );
}

/**
 * CSRF (SPEC §5): non-GET/HEAD requests need "X-Backyard: 1" and an Origin,
 * when present, equal to the request's origin (or the hub's in path mode).
 */
export function checkCSRF(ctx: RequestContext): void {
  const m = ctx.req.method;
  if (m === "GET" || m === "HEAD") return;
  if (ctx.req.headers.get("X-Backyard") !== "1") throw forbidden('missing "X-Backyard: 1" header');
  const o = ctx.req.headers.get("Origin");
  if (o !== null && o !== "") {
    const po = parseOrigin(o);
    if (!po || (po !== ctx.origin && !(ctx.mode === "path" && po === ctx.hubOrigin))) throw forbidden("cross-origin request rejected");
  }
}

/** Authenticates an API request (401 JSON with login_url), enforces CSRF. */
export async function apiGate(ctx: RequestContext): Promise<User> {
  let u: User;
  try {
    u = await authenticate(ctx);
  } catch (e) {
    if (e instanceof HttpError && e.status === 401 && !e.loginURL) {
      const login = accessLoginURL(ctx);
      if (login) e.loginURL = login;
    }
    throw e;
  }
  checkCSRF(ctx);
  touchUser(ctx, u);
  return u;
}

type Handler = (ctx: RequestContext, user: User, params: string[]) => Promise<Response>;

interface Route {
  method: string;
  re: RegExp;
  h: Handler;
}

const SEG = "([^/]+)";

/** Matches routes; 404 for unknown paths, 405 (with Allow) for wrong methods. */
export async function dispatch(ctx: RequestContext, user: User, sub: string, routes: Route[]): Promise<Response> {
  const method = ctx.req.method === "HEAD" ? "GET" : ctx.req.method;
  const allowed: string[] = [];
  for (const r of routes) {
    const m = r.re.exec(sub);
    if (!m) continue;
    if (r.method === method) return r.h(ctx, user, m.slice(1));
    allowed.push(r.method);
    if (r.method === "GET") allowed.push("HEAD");
  }
  if (allowed.length) throw methodNotAllowed(allowed);
  throw notFound(`no such endpoint: ${ctx.req.method} ${ctx.url.pathname.slice(0, 200)}`);
}

const SITE_ROUTES: Route[] = [
  { method: "GET", re: /^\/me$/, h: async (_c, u) => json(cleanUser(u)) },
  { method: "GET", re: new RegExp(`^/users/${SEG}$`), h: lookupUser },
  { method: "GET", re: /^\/site$/, h: getSiteMeta },
  { method: "GET", re: /^\/db$/, h: dbCollections },
  { method: "POST", re: new RegExp(`^/db/${SEG}$`), h: dbCreate },
  { method: "GET", re: new RegExp(`^/db/${SEG}$`), h: dbList },
  { method: "POST", re: new RegExp(`^/db/${SEG}/query$`), h: dbQuery },
  { method: "GET", re: new RegExp(`^/db/${SEG}/${SEG}$`), h: dbGet },
  { method: "PUT", re: new RegExp(`^/db/${SEG}/${SEG}$`), h: dbPut },
  { method: "PATCH", re: new RegExp(`^/db/${SEG}/${SEG}$`), h: dbPatch },
  { method: "DELETE", re: new RegExp(`^/db/${SEG}/${SEG}$`), h: dbDelete },
  { method: "POST", re: /^\/files$/, h: filesUpload },
  { method: "GET", re: /^\/files$/, h: filesList },
  { method: "GET", re: new RegExp(`^/files/${SEG}$`), h: filesGet },
  { method: "DELETE", re: new RegExp(`^/files/${SEG}$`), h: filesDelete },
  { method: "POST", re: /^\/ai\/chat$/, h: aiChat },
  { method: "POST", re: /^\/ai\/image$/, h: aiImage },
  { method: "POST", re: /^\/ai\/embed$/, h: aiEmbed },
  { method: "GET", re: /^\/ai\/models$/, h: aiModels },
  { method: "POST", re: /^\/warehouse\/query$/, h: warehouse },
  { method: "GET", re: /^\/warehouse$/, h: warehouse },
  { method: "GET", re: /^\/ws$/, h: websocket },
];

async function siteAPI(ctx: RequestContext, sub: string): Promise<Response> {
  const user = await apiGate(ctx);
  return dispatch(ctx, user, sub, SITE_ROUTES);
}

// --- identity -----------------------------------------------------------------

async function lookupUser(ctx: RequestContext, _u: User, [raw]: string[]): Promise<Response> {
  const email = normalizeEmail(raw ?? "");
  if (!email || !email.includes("@")) throw badRequest("invalid email address");
  const found = unwrap(await registry(ctx.env).lookupUser(email));
  if (!found) throw notFound("unknown user");
  return json(cleanUser(found));
}

async function getSiteMeta(ctx: RequestContext): Promise<Response> {
  const m = unwrap(await registry(ctx.env).getSite(ctx.site));
  if (!m) throw notFound(`site ${JSON.stringify(ctx.site)} has not been deployed`);
  return json(siteMetaJSON(m, siteURL(ctx, ctx.site)));
}

// --- database -----------------------------------------------------------------

function collectionParam(c: string | undefined): string {
  const name = c ?? "";
  const err = collectionError(name);
  if (err) throw badRequest(err);
  return name;
}

function idParam(id: string | undefined): string {
  const v = id ?? "";
  const err = docIDError(v);
  if (err) throw badRequest(err);
  return v;
}

function docBodyLimit(ctx: RequestContext): number {
  const n = ctx.cfg.limits.max_doc_bytes;
  return n > 0 ? n * 2 + (64 << 10) : 8 << 20;
}

/**
 * Strips the reserved keys "id" and "_meta" from a write body. Keys are
 * defined, not assigned, so that a data key named "__proto__" is stored like
 * any other key (as the Go server does) instead of changing the prototype.
 */
function prepareData(body: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(body)) {
    if (k !== "id" && k !== "_meta") Object.defineProperty(out, k, { value: v, enumerable: true, writable: true, configurable: true });
  }
  return out;
}

function bodyID(body: Record<string, unknown>): string {
  const v = body.id;
  if (v === undefined || v === null) return "";
  if (typeof v !== "string") throw badRequest('"id" must be a string');
  if (v === "") return "";
  const err = docIDError(v);
  if (err) throw badRequest(err);
  return v;
}

function writeOpts(ctx: RequestContext, u: User) {
  return { actor: u.id || "unknown", writer: rateKey(ctx, u) };
}

async function dbCollections(ctx: RequestContext): Promise<Response> {
  return json({ collections: unwrap(await siteStub(ctx.env, ctx.site).dbCollections()) });
}

async function dbCreate(ctx: RequestContext, u: User, [c]: string[]): Promise<Response> {
  const coll = collectionParam(c);
  const body = await readObject(ctx.req, docBodyLimit(ctx));
  const id = bodyID(body);
  const doc = unwrap(await siteStub(ctx.env, ctx.site).dbCreate(coll, id, prepareData(body), writeOpts(ctx, u)));
  return json(doc, 201);
}

async function runQuery(ctx: RequestContext, coll: string, raw: RawQuery): Promise<Response> {
  if ((raw.limit ?? 0) < 0) throw badRequest("limit must be >= 0");
  const q = normalizeQuery(raw, ctx.cfg.limits.max_list);
  const page = unwrap(await siteStub(ctx.env, ctx.site).dbQuery(coll, q));
  return json(page);
}

async function dbList(ctx: RequestContext, _u: User, [c]: string[]): Promise<Response> {
  const coll = collectionParam(c);
  return runQuery(ctx, coll, parseListParams(ctx.url.searchParams));
}

async function dbQuery(ctx: RequestContext, _u: User, [c]: string[]): Promise<Response> {
  const coll = collectionParam(c);
  const body = await readJSON(ctx.req, SMALL_BODY);
  return runQuery(ctx, coll, decodeQueryBody(body));
}

async function dbGet(ctx: RequestContext, _u: User, [c, id]: string[]): Promise<Response> {
  const coll = collectionParam(c);
  const docID = idParam(id);
  return json(unwrap(await siteStub(ctx.env, ctx.site).dbGet(coll, docID)));
}

async function dbPut(ctx: RequestContext, u: User, [c, id]: string[]): Promise<Response> {
  const coll = collectionParam(c);
  const docID = idParam(id);
  const body = await readObject(ctx.req, docBodyLimit(ctx));
  const bid = bodyID(body);
  if (bid && bid !== docID) throw badRequest(`"id" in the body (${JSON.stringify(bid)}) does not match the URL (${JSON.stringify(docID)})`);
  const r = unwrap(await siteStub(ctx.env, ctx.site).dbPut(coll, docID, prepareData(body), writeOpts(ctx, u)));
  return json(r.doc);
}

async function dbPatch(ctx: RequestContext, u: User, [c, id]: string[]): Promise<Response> {
  const coll = collectionParam(c);
  const docID = idParam(id);
  const body = await readObject(ctx.req, docBodyLimit(ctx));
  return json(unwrap(await siteStub(ctx.env, ctx.site).dbPatch(coll, docID, prepareData(body), writeOpts(ctx, u))));
}

async function dbDelete(ctx: RequestContext, u: User, [c, id]: string[]): Promise<Response> {
  const coll = collectionParam(c);
  const docID = idParam(id);
  unwrap(await siteStub(ctx.env, ctx.site).dbDelete(coll, docID, writeOpts(ctx, u)));
  return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
}

// --- files ------------------------------------------------------------------------

/** FileInfo wire shape (SPEC §5.3). */
function fileInfo(ctx: RequestContext, r: UploadRow) {
  return {
    id: r.id,
    name: r.name,
    url: `${ctx.prefix}/__backyard/files/${r.id}/${encodeURIComponent(r.name)}`,
    size: r.size,
    content_type: r.content_type,
    created_at: r.created_at,
    created_by: r.created_by,
  };
}

/**
 * Stores bytes as an upload of the current site: inline in the site object,
 * or in R2 above FILE_INLINE_MAX when R2 is bound.
 */
export async function storeUpload(
  ctx: RequestContext,
  u: User,
  name: string,
  contentType: string,
  size: number,
  body: ReadableStream<Uint8Array> | Uint8Array | Blob,
): Promise<UploadRow> {
  const stub = siteStub(ctx.env, ctx.site);
  const id = newULID();
  const bucket = ctx.env.R2;
  if (bucket && size > ctx.cfg.fileInlineMax) {
    const key = `uploads/${ctx.site}/${id}/${name}`;
    await bucket.put(key, body instanceof ReadableStream ? fixedLength(body, size) : body, { httpMetadata: { contentType } });
    const r = await stub.recordUpload({ id, name, size, content_type: contentType, created_by: u.id, r2_key: key });
    if (!r.ok) ctx.waitUntil(bucket.delete(key));
    return unwrap(r);
  }
  const stream = body instanceof ReadableStream ? body : body instanceof Blob ? body.stream() : new Blob([body]).stream();
  const res = await stub.fetch(`https://site/blob?key=${encodeURIComponent("u:" + id)}&size=${size}`, { method: "PUT", body: stream });
  if (!res.ok) {
    const e = (await res.json().catch(() => ({}))) as { error?: { code?: string; message?: string } };
    throw new HttpError(res.status, (e.error?.code as HttpError["code"]) ?? "internal", e.error?.message ?? "storing the upload failed");
  }
  return unwrap(await stub.recordUpload({ id, name, size, content_type: contentType, created_by: u.id }));
}

/** Wraps a stream with a known length (required by R2 for streamed bodies). */
export function fixedLength(body: ReadableStream<Uint8Array>, size: number): ReadableStream<Uint8Array> {
  const f = new FixedLengthStream(size);
  void body.pipeTo(f.writable).catch(() => {});
  return f.readable;
}

/**
 * POST /files (SPEC §5.3). The Worker checks the request and the daily cap,
 * then streams the multipart body to the site object, which parses it and
 * stores the file (SQLite chunks, or R2 above FILE_INLINE_MAX): the Worker
 * spends no CPU on the body and nothing holds the whole upload in memory.
 */
async function filesUpload(ctx: RequestContext, u: User): Promise<Response> {
  const ct = ctx.req.headers.get("Content-Type") ?? "";
  if (!ct.toLowerCase().startsWith("multipart/form-data")) throw badRequest('expected a multipart/form-data body with a "file" field');
  const limit = maxUploadBytes(ctx.cfg, !!ctx.env.R2);
  const declared = Number(ctx.req.headers.get("Content-Length") ?? "0");
  if (declared > limit + MULTIPART_OVERHEAD) throw tooLarge(`file exceeds the maximum upload size of ${limit} bytes`);
  await chargeOrThrow(ctx.env, [{ key: `upload|${rateKey(ctx, u)}`, limit: ctx.cfg.limits.uploads_per_day }]);
  const qs = new URLSearchParams({ site: ctx.site, limit: String(limit), inline_max: String(ctx.cfg.fileInlineMax), r2: ctx.env.R2 ? "1" : "0" });
  const res = await siteStub(ctx.env, ctx.site).fetch(`https://site/upload?${qs}`, {
    method: "POST",
    headers: { "Content-Type": ct, "x-backyard-created-by": encodeURIComponent(u.id) },
    body: ctx.req.body,
  });
  if (!res.ok) {
    const e = (await res.json().catch(() => ({}))) as { error?: { code?: string; message?: string } };
    throw new HttpError(res.status, (e.error?.code as HttpError["code"]) ?? "internal", e.error?.message ?? "storing the upload failed");
  }
  return json(fileInfo(ctx, (await res.json()) as UploadRow), 201);
}

function intParam(sp: URLSearchParams, name: string): number | undefined {
  const s = sp.get(name);
  if (s === null || s === "") return undefined;
  if (!/^\d+$/.test(s)) throw badRequest(`${name} must be a non-negative integer`);
  return Number(s);
}

async function filesList(ctx: RequestContext): Promise<Response> {
  let limit = intParam(ctx.url.searchParams, "limit") ?? 0;
  const offset = intParam(ctx.url.searchParams, "offset") ?? 0;
  if (offset > 100_000) throw badRequest("offset too large");
  if (limit <= 0) limit = 100;
  limit = Math.min(limit, ctx.cfg.limits.max_list);
  const page = unwrap(await siteStub(ctx.env, ctx.site).listUploads(limit, offset));
  return json({ files: page.files.map((f) => fileInfo(ctx, f)), has_more: page.has_more });
}

async function filesGet(ctx: RequestContext, _u: User, [id]: string[]): Promise<Response> {
  if (!DOC_ID_RE.test(id ?? "")) throw notFound("file not found");
  return json(fileInfo(ctx, unwrap(await siteStub(ctx.env, ctx.site).getUpload(id!))));
}

async function filesDelete(ctx: RequestContext, _u: User, [id]: string[]): Promise<Response> {
  if (DOC_ID_RE.test(id ?? "")) {
    const { r2Key } = unwrap(await siteStub(ctx.env, ctx.site).deleteUpload(id!));
    if (r2Key && ctx.env.R2) await ctx.env.R2.delete(r2Key);
  }
  return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
}

// --- AI (SPEC §5.4) --------------------------------------------------------------

function aiService(ctx: RequestContext): AIService {
  const ai = new AIService(ctx.cfg, ctx.env);
  if (!ai.enabled()) throw unsupported("AI is not configured on this server");
  return ai;
}

/** Charges the daily AI caps (per user/IP and platform-wide). */
async function chargeAI(ctx: RequestContext, u: User, image: boolean): Promise<void> {
  const who = rateKey(ctx, u);
  const lim = ctx.cfg.limits;
  const charges = [
    { key: `ai|${who}`, limit: lim.ai_per_day },
    { key: "ai_platform|all", limit: lim.ai_platform_per_day },
  ];
  if (image) {
    charges.push({ key: `ai_image|${who}`, limit: lim.ai_images_per_day }, { key: "ai_image_platform|all", limit: lim.ai_images_platform_per_day });
  }
  await chargeOrThrow(ctx.env, charges);
}

const MEDIA_EXT: Record<string, string> = { "image/png": ".png", "image/jpeg": ".jpg", "image/jpg": ".jpg", "image/webp": ".webp", "image/gif": ".gif" };

/** Parses a data: URL with base64 payload. */
function parseDataURL(s: string): { data: Uint8Array; mediaType: string } | null {
  if (!s.startsWith("data:")) return null;
  const comma = s.indexOf(",");
  if (comma < 0) return null;
  const meta = s.slice(5, comma).split(";");
  if (meta.length < 2 || meta[meta.length - 1]!.toLowerCase() !== "base64") return null;
  const data = decodeAnyBase64(s.slice(comma + 1).trim());
  return data ? { data, mediaType: (meta[0] ?? "").trim().toLowerCase() } : null;
}

function parseContent(i: number, raw: unknown): Part[] {
  if (raw === undefined || raw === null) throw badRequest(`message ${i}: content is required`);
  if (typeof raw === "string") return [{ type: "text", text: raw }];
  if (!Array.isArray(raw)) throw badRequest(`message ${i}: content must be a string or an array of parts`);
  if (raw.length === 0) throw badRequest(`message ${i}: content is empty`);
  return raw.map((p, j) => {
    if (!isPlainObject(p)) throw badRequest(`message ${i}: content must be a string or an array of parts`);
    const type = String(p.type ?? "").toLowerCase();
    if (type === "text") return { type: "text", text: typeof p.text === "string" ? p.text : "" } as Part;
    if (type === "image" || type === "image_url") {
      let mt = typeof p.media_type === "string" ? p.media_type.trim().toLowerCase() : "";
      const dataStr = typeof p.data === "string" ? p.data : "";
      let url = typeof p.url === "string" ? p.url : "";
      if (!url && isPlainObject(p.image_url) && typeof p.image_url.url === "string") url = p.image_url.url;
      if (dataStr) {
        const du = parseDataURL(dataStr);
        const data = du ? du.data : decodeAnyBase64(dataStr);
        if (!data) throw badRequest(`message ${i} part ${j}: image data must be base64`);
        if (!mt) mt = du?.mediaType || sniff(data.subarray(0, 64)) || "application/octet-stream";
        return { type: "image", data, media_type: mt } as Part;
      }
      if (url) {
        const du = parseDataURL(url);
        if (du) return { type: "image", data: du.data, media_type: mt || du.mediaType } as Part;
        return { type: "image", url, media_type: mt } as Part;
      }
      throw badRequest(`message ${i} part ${j}: image needs data or url`);
    }
    throw badRequest(`message ${i} part ${j}: unknown type ${JSON.stringify(type)} (text or image)`);
  });
}

interface ParsedChat {
  req: Omit<ChatRequest, "model" | "maxTokens">;
  maxTokens: number;
  alias: string;
  stream: boolean;
}

/** Converts the wire request (SPEC §5.4) into a provider-neutral request. */
export function parseChat(body: Record<string, unknown>, systemPrompt: string): ParsedChat {
  const maxTokens = body.max_tokens === undefined || body.max_tokens === null ? 0 : body.max_tokens;
  if (typeof maxTokens !== "number" || !Number.isInteger(maxTokens)) throw badRequest('invalid JSON: field "max_tokens" must be a number');
  if (maxTokens < 0) throw badRequest("max_tokens must be >= 0");
  if (!Array.isArray(body.messages) || body.messages.length === 0) throw badRequest("messages must not be empty");
  const system: string[] = [];
  if (systemPrompt.trim()) system.push(systemPrompt.trim());
  if (typeof body.system === "string" && body.system.trim()) system.push(body.system.trim());
  const messages: Message[] = [];
  body.messages.forEach((m, i) => {
    if (!isPlainObject(m)) throw badRequest(`message ${i}: must be an object`);
    const role = String(m.role ?? "").trim().toLowerCase();
    const parts = parseContent(i, m.content);
    if (role === "system") {
      if (parts.some((p) => p.type !== "text")) throw badRequest(`message ${i}: system messages may only contain text`);
      const t = textOf(parts).trim();
      if (t) system.push(t);
    } else if (role === "user" || role === "assistant") {
      messages.push({ role, content: parts });
    } else {
      throw badRequest(`message ${i}: role must be user, assistant or system`);
    }
  });
  if (!messages.length) throw badRequest("messages must contain at least one user message");
  const temperature = typeof body.temperature === "number" && Number.isFinite(body.temperature) ? body.temperature : undefined;
  const req: ParsedChat["req"] = { system: system.join("\n\n"), messages, json: body.json === true, user: "" };
  if (temperature !== undefined) req.temperature = temperature;
  return { req, maxTokens, alias: typeof body.model === "string" ? body.model : "", stream: body.stream === true };
}

async function aiChat(ctx: RequestContext, u: User): Promise<Response> {
  const ai = aiService(ctx);
  const body = await readObject(ctx.req, CHAT_BODY);
  const parsed = parseChat(body, ctx.cfg.ai.systemPrompt);
  if (!ai.known(parsed.alias)) throw notFound(`unknown model ${JSON.stringify(parsed.alias)}`);
  const r = ai.resolve(parsed.alias, "chat");
  await chargeAI(ctx, u, false);
  const cap = Math.min(ctx.cfg.limits.ai_max_tokens || 4096, r.config?.max_tokens || Infinity);
  const req: ChatRequest = { ...parsed.req, model: r.model, maxTokens: parsed.maxTokens > 0 ? Math.min(parsed.maxTokens, cap) : cap, user: u.id };
  if (r.config?.effort) req.effort = r.config.effort;
  if (!parsed.stream) {
    const resp = await r.provider.chat(req);
    return json({ content: resp.content, model: r.alias, stop_reason: resp.stop_reason, usage: resp.usage });
  }
  return streamChat(ctx, r.alias, (onText) => r.provider.stream(req, onText));
}

/** Runs a streaming completion as text/event-stream (SPEC §5.4). */
function streamChat(
  ctx: RequestContext,
  alias: string,
  run: (onText: (t: string) => Promise<void>) => Promise<{ stop_reason: string; usage: unknown }>,
): Response {
  const ts = new TransformStream<Uint8Array, Uint8Array>();
  const writer = ts.writable.getWriter();
  const enc = new TextEncoder();
  let closed = false;
  let last = Date.now();
  const write = async (s: string) => {
    if (closed) return;
    last = Date.now();
    await writer.write(enc.encode(s));
  };
  const event = (v: unknown) => write(`data: ${JSON.stringify(v)}\n\n`);
  const timer = setInterval(() => {
    if (Date.now() - last >= SSE_HEARTBEAT_MS) void write(": ping\n\n").catch(() => {});
  }, SSE_HEARTBEAT_MS);
  const pump = (async () => {
    try {
      const resp = await run((text) => (text ? event({ type: "text", text }) : Promise.resolve()));
      await event({ type: "done", model: alias, stop_reason: resp.stop_reason, usage: resp.usage });
    } catch (e) {
      const he = e instanceof HttpError ? e : new HttpError(502, "upstream_error", "AI request failed");
      if (!(e instanceof HttpError)) console.warn(`backyard: AI stream failed: ${(e as Error)?.message ?? e}`);
      await event({ type: "error", error: { code: he.code, message: he.message } }).catch(() => {});
    } finally {
      clearInterval(timer);
      closed = true;
      await writer.close().catch(() => {});
    }
  })();
  ctx.waitUntil(pump);
  return new Response(ts.readable, {
    headers: { "Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no" },
  });
}

async function aiImage(ctx: RequestContext, u: User): Promise<Response> {
  const ai = aiService(ctx);
  const body = await readObject(ctx.req, SMALL_BODY);
  const prompt = typeof body.prompt === "string" ? body.prompt : "";
  if (!prompt.trim()) throw badRequest("prompt is required");
  const rawN = body.n === undefined || body.n === null ? 0 : body.n;
  if (typeof rawN !== "number" || !Number.isInteger(rawN)) throw badRequest('invalid JSON: field "n" must be a number');
  let n = rawN;
  if (n < 0) throw badRequest("n must be >= 0");
  if (n === 0) n = 1;
  if (n > 4) throw badRequest("n must be at most 4");
  const alias = typeof body.model === "string" ? body.model : "";
  if (!ai.known(alias)) throw notFound(`unknown model ${JSON.stringify(alias)}`);
  const r = ai.resolve(alias, "image");
  await chargeAI(ctx, u, true);
  const images = await r.provider.image({ model: r.model, prompt, size: typeof body.size === "string" ? body.size : "", n, user: u.id });
  const out = [];
  for (let i = 0; i < images.length; i++) {
    const img = images[i]!;
    const mt = img.media_type || "image/png";
    const row = await storeUpload(ctx, u, `image-${i + 1}${MEDIA_EXT[mt] ?? ".bin"}`, mt, img.data.length, img.data);
    out.push({ id: row.id, url: fileInfo(ctx, row).url, content_type: row.content_type });
  }
  return json({ model: r.alias, images: out });
}

async function aiEmbed(ctx: RequestContext, u: User): Promise<Response> {
  const ai = aiService(ctx);
  const body = await readObject(ctx.req, 16 << 20);
  let inputs: string[];
  if (typeof body.input === "string") inputs = [body.input];
  else if (Array.isArray(body.input) && body.input.every((x) => typeof x === "string")) {
    if (body.input.length === 0) throw badRequest("input must not be empty");
    inputs = body.input as string[];
  } else if (body.input === undefined || body.input === null) {
    throw badRequest("input is required");
  } else {
    throw badRequest("input must be a string or an array of strings");
  }
  if (inputs.length > 100) throw badRequest("at most 100 inputs per request");
  const alias = typeof body.model === "string" ? body.model : "";
  if (!ai.known(alias)) throw notFound(`unknown model ${JSON.stringify(alias)}`);
  const r = ai.resolve(alias, "embedding");
  await chargeAI(ctx, u, false);
  const resp = await r.provider.embed({ model: r.model, input: inputs, user: u.id });
  return json({ model: r.alias, embeddings: resp.embeddings, usage: resp.usage });
}

async function aiModels(ctx: RequestContext): Promise<Response> {
  return json(aiService(ctx).models());
}

// --- warehouse (SPEC §5.5) -----------------------------------------------------

async function warehouse(ctx: RequestContext, _u: User): Promise<Response> {
  const base = ctx.cfg.warehouseURL;
  if (!base) {
    throw unsupported("the data warehouse is not available on the Cloudflare edition (set WAREHOUSE_URL to proxy a Backyard server)");
  }
  const sub = ctx.path.slice("/__backyard/api".length);
  const init: RequestInit = { method: ctx.req.method, headers: { Accept: "application/json", "X-Backyard": "1" } };
  if (ctx.env.WAREHOUSE_TOKEN) (init.headers as Record<string, string>).Authorization = `Bearer ${ctx.env.WAREHOUSE_TOKEN}`;
  if (ctx.req.method === "POST") {
    init.body = JSON.stringify(await readJSON(ctx.req, SMALL_BODY));
    (init.headers as Record<string, string>)["Content-Type"] = "application/json";
  }
  let res: Response;
  try {
    res = await fetch(base + sub, { ...init, signal: AbortSignal.timeout(90_000) });
  } catch (e) {
    throw new HttpError(502, "upstream_error", `warehouse proxy failed: ${(e as Error).message}`);
  }
  return new Response(res.body, { status: res.status, headers: { "Content-Type": res.headers.get("Content-Type") ?? "application/json", "Cache-Control": "no-store" } });
}

// --- websocket (SPEC §6) -------------------------------------------------------

/** Origins allowed to open this site's websocket. */
function wsOriginAllowed(ctx: RequestContext): boolean {
  const o = ctx.req.headers.get("Origin");
  if (!o) return true;
  const po = parseOrigin(o);
  if (!po) return false;
  const allowed = new Set([parseOrigin(siteURL(ctx, ctx.site)), ctx.origin]);
  if (ctx.mode === "path") allowed.add(ctx.hubOrigin);
  return allowed.has(po);
}

async function websocket(ctx: RequestContext, u: User): Promise<Response> {
  if ((ctx.req.headers.get("Upgrade") ?? "").toLowerCase() !== "websocket") {
    throw new HttpError(426, "bad_request", "expected a websocket upgrade");
  }
  if (!wsOriginAllowed(ctx)) throw forbidden("origin not allowed");
  return siteStub(ctx.env, ctx.site).fetch("https://site/ws", {
    headers: { Upgrade: "websocket", "x-backyard-user": encodeURIComponent(JSON.stringify(cleanUser(u))) },
  });
}
