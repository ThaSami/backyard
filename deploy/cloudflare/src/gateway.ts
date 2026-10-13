/**
 * The deploy gateway (SPEC §7) mounted at /__backyard/api/deploy: /info, /me,
 * sites, files, deploys (diffed create → self-hosted uploads → commit),
 * refresh, owners and delete. Deploy ids and upload URLs are HMAC-signed with
 * the same keys and encoding as the Go gateway.
 */
import { anonCookieUser, authenticate, authenticateBearer, guestFromIP, hasBearer, isCookieConflict, rateKey, secretMissing } from "./auth";
import { maxFileBytes, MAX_INLINE_FILE } from "./config";
import { KeyLabel, randomHex, signClaims, verifyClaims } from "./crypto";
import { formatMicros } from "./db/time";
import {
  HttpError,
  badRequest,
  bearerToken,
  forbidden,
  isPlainObject,
  json,
  methodNotAllowed,
  notFound,
  readJSON,
  tooLarge,
  unauthenticated,
  unsupported,
  unwrap,
} from "./http";
import { cleanUser, type User } from "./identity";
import { detectContentType } from "./mime";
import { resolveR2Body, R2_KEY_HEADER } from "./object";
import { normalizeOwners, validatePath } from "./paths";
import type { SiteRecord } from "./registry-do";
import { gatewayURL, siteNameError, siteURL, type RequestContext } from "./router";
import type { CommitResult, DeployFile } from "./site-do";
import { fixedLength } from "./site-api";
import { isOwner, siteMetaJSON } from "./sitemeta";
import { chargeOrThrow, registry, siteStub, VERSION } from "./stubs";
import { extractTitle, TITLE_SCAN_BYTES } from "./title";

/** How long a deploy id (and its upload URLs) stays valid. */
const DEPLOY_TTL_SECONDS = 3600;
/** How long a first commit holds its slot of max_sites before the site is recorded. */
const SITE_RESERVATION_MS = 10 * 60 * 1000;
const MAX_OWNERS = 100;

type Access = "read" | "deploy" | "manage";

interface DeployClaims {
  site: string;
  actor: string;
  exp: number;
  nonce: string;
}

interface UploadClaims {
  site: string;
  path: string;
  size: number;
  sha256: string;
  content_type: string;
  deploy_id: string;
  exp: number;
}

/** GET /info (no auth). */
export function infoDocument(ctx: RequestContext): Record<string, unknown> {
  const { cfg } = ctx;
  const auth: Record<string, unknown> = { mode: cfg.authMode };
  if (cfg.authMode === "access") auth.cli_login_url = ctx.publicURL + "/__backyard/auth/cli";
  auth.anonymous_deploy = cfg.authMode === "none" && cfg.allowAnonymousDeploy;
  const base = cfg.baseDomain || new URL(ctx.publicURL).host;
  return {
    name: cfg.serverName,
    version: VERSION,
    public_url: ctx.publicURL,
    base_domain: base,
    url_mode: cfg.urlMode,
    gateway_url: gatewayURL(ctx),
    auth,
    methods: ["api"],
    limits: {
      max_site_bytes: cfg.limits.max_site_bytes,
      max_files: cfg.limits.max_files,
      max_file_bytes: maxFileBytes(cfg, !!ctx.env.R2),
    },
    edition: "cloudflare",
  };
}

/**
 * CSRF rule of the gateway: state-changing requests need "X-Backyard: 1",
 * except upload URLs (their signed token is the capability) and bearer-token
 * requests without browser cross-site signals.
 */
function csrfOK(ctx: RequestContext, sub: string): boolean {
  const m = ctx.req.method;
  if (m === "GET" || m === "HEAD" || m === "OPTIONS") return true;
  if (sub.startsWith("/uploads/") || ctx.req.headers.get("X-Backyard") === "1") return true;
  if (!hasBearer(ctx.req) || ctx.req.headers.get("Origin")) return false;
  const sfs = ctx.req.headers.get("Sec-Fetch-Site") ?? "";
  return sfs === "" || sfs === "none" || sfs === "same-origin";
}

/**
 * Where a CLI user signs in again: in access mode the hub itself (Cloudflare
 * Access intercepts the page load), otherwise there is no browser login.
 */
function gatewayLoginURL(ctx: RequestContext): string {
  return ctx.cfg.authMode === "access" ? ctx.publicURL + "/" : "";
}

function gatewayUnauthenticated(ctx: RequestContext, msg: string): HttpError {
  return unauthenticated(msg, gatewayLoginURL(ctx) || undefined);
}

/**
 * Authenticates a gateway request and checks the access level (the Go
 * gateway's rules): an invalid bearer token is always an error, never a silent
 * guest; a valid token minted for an anonymous guest (AUTH_MODE=none lets
 * guests mint tokens) is that guest and follows the guest rules; without a
 * token, mode none falls back to the hub's cookie identity or a guest keyed by
 * the client address.
 */
async function gatewayUser(ctx: RequestContext, need: Access): Promise<User> {
  const { cfg } = ctx;
  if (!cfg.secret) throw secretMissing();
  const bearer = bearerToken(ctx.req);
  let u: User;
  if (bearer) {
    try {
      u = await authenticateBearer(cfg, bearer);
    } catch (e) {
      if (e instanceof HttpError && e.status === 401) throw gatewayUnauthenticated(ctx, "invalid or expired token (run: backyard login)");
      throw e;
    }
  } else if (cfg.authMode === "access") {
    try {
      u = await authenticate(ctx, { bearer: false });
    } catch (e) {
      if (e instanceof HttpError && e.status === 401 && !isCookieConflict(e)) throw gatewayUnauthenticated(ctx, "authentication required (run: backyard login)");
      throw e;
    }
  } else {
    // Mode none: the hub's browser session (signed cookie) or a guest keyed by address.
    u = (await anonCookieUser(ctx)) ?? (await guestFromIP(ctx));
  }
  ctx.user = u;
  if (u.anonymous) {
    let what = "";
    if (need === "deploy" && !(cfg.authMode === "none" && cfg.allowAnonymousDeploy)) what = "deploy";
    else if (need === "manage") what = "delete sites or change owners";
    if (what) {
      if (bearer) {
        throw gatewayUnauthenticated(
          ctx,
          `this token belongs to an anonymous guest, which cannot ${what}; use a personal or static token (run: backyard login --token <token>)`,
        );
      }
      throw gatewayUnauthenticated(ctx, `a token is required to ${what} (run: backyard login)`);
    }
  }
  return u;
}

function siteParam(ctx: RequestContext, raw: string | undefined, withReserved: boolean): string {
  const name = raw ?? "";
  const err = siteNameError(name, withReserved ? ctx.cfg.reserved : null);
  if (err) throw badRequest(err);
  return name;
}

/** Applies the ownership policy to a write on site; returns the stored record. */
async function authorizeSite(ctx: RequestContext, u: User, site: string): Promise<SiteRecord | null> {
  const meta = unwrap(await registry(ctx.env).getSite(site));
  if (ctx.cfg.ownership !== "owner" || u.admin || !meta || !meta.owners?.length) return meta;
  if (!u.anonymous && isOwner(meta, u.id, u.email ?? "")) return meta;
  throw forbidden(`site ${JSON.stringify(site)} is owned by ${meta.owners.join(", ")}; only owners and admins can change it`);
}

function deployBodyLimit(ctx: RequestContext): number {
  return (1 << 20) + Math.max(ctx.cfg.limits.max_files, 1) * 2048;
}

const SHA_RE = /^[0-9a-fA-F]{64}$/;

/** Validates and normalizes a deploy file list against the limits. */
export function validateFiles(ctx: RequestContext, raw: unknown): { files: DeployFile[]; total: number } {
  if (!Array.isArray(raw) || raw.length === 0) throw badRequest("no files to deploy (to remove a site, delete it)");
  const lim = ctx.cfg.limits;
  if (lim.max_files > 0 && raw.length > lim.max_files) throw tooLarge(`${raw.length} files exceed the limit of ${lim.max_files} files per site`);
  const maxFile = maxFileBytes(ctx.cfg, !!ctx.env.R2);
  const seen = new Set<string>();
  const files: DeployFile[] = [];
  let total = 0;
  for (const f of raw) {
    if (!isPlainObject(f)) throw badRequest("each file must be an object");
    const path = f.path;
    const why = validatePath(path);
    if (why) throw badRequest(`invalid path ${JSON.stringify(String(path ?? "").slice(0, 200))}: ${why}`);
    const p = path as string;
    if (seen.has(p)) throw badRequest(`duplicate path ${JSON.stringify(p.slice(0, 200))}`);
    seen.add(p);
    const size = f.size;
    if (typeof size !== "number" || !Number.isSafeInteger(size) || size < 0) throw badRequest(`invalid size for ${JSON.stringify(p.slice(0, 200))}`);
    if (size > maxFile) {
      const hint = !ctx.env.R2 && size <= ctx.cfg.limits.max_file_bytes ? " (bind an R2 bucket for files above 25 MiB)" : "";
      throw tooLarge(`${JSON.stringify(p.slice(0, 200))} is ${size} bytes; the limit is ${maxFile} bytes per file${hint}`);
    }
    if (typeof f.sha256 !== "string" || !SHA_RE.test(f.sha256)) throw badRequest(`invalid sha256 for ${JSON.stringify(p.slice(0, 200))} (want 64 hex digits)`);
    const rawCT = f.content_type === undefined || f.content_type === null ? "" : f.content_type;
    // eslint-disable-next-line no-control-regex
    if (typeof rawCT !== "string" || rawCT.length > 255 || /[\u0000-\u001f\u007f]/.test(rawCT)) {
      throw badRequest(`invalid content_type for ${JSON.stringify(p.slice(0, 200))}`);
    }
    const ct = rawCT.trim() || detectContentType(p);
    total += size;
    if (lim.max_site_bytes > 0 && total > lim.max_site_bytes) throw tooLarge(`site exceeds the limit of ${lim.max_site_bytes} bytes`);
    files.push({ path: p, size, sha256: f.sha256.toLowerCase(), content_type: ct });
  }
  for (const f of files) {
    for (let i = f.path.lastIndexOf("/"); i > 0; i = f.path.lastIndexOf("/", i - 1)) {
      if (seen.has(f.path.slice(0, i))) {
        throw badRequest(`path ${JSON.stringify(f.path.slice(0, 200))} conflicts with file ${JSON.stringify(f.path.slice(0, i).slice(0, 200))}`);
      }
    }
  }
  return { files, total };
}

function rfc3339(sec: number): string {
  return formatMicros(sec * 1_000_000);
}

// --- handlers -------------------------------------------------------------------

async function handleMe(ctx: RequestContext): Promise<Response> {
  return json(cleanUser(await gatewayUser(ctx, "read")));
}

async function handleListSites(ctx: RequestContext): Promise<Response> {
  await gatewayUser(ctx, "read");
  const list = unwrap(await registry(ctx.env).listSites());
  return json({ sites: list.map((r) => siteMetaJSON(r, siteURL(ctx, r.name))) });
}

async function handleGetSite(ctx: RequestContext, [s]: string[]): Promise<Response> {
  await gatewayUser(ctx, "read");
  const site = siteParam(ctx, s, false);
  const m = unwrap(await registry(ctx.env).getSite(site));
  if (!m) throw notFound(`site ${JSON.stringify(site)} not found`);
  return json(siteMetaJSON(m, siteURL(ctx, site)));
}

async function handleFiles(ctx: RequestContext, [s]: string[]): Promise<Response> {
  await gatewayUser(ctx, "read");
  const site = siteParam(ctx, s, false);
  const files = unwrap(await siteStub(ctx.env, site).listFiles());
  if (files.length === 0 && !unwrap(await registry(ctx.env).getSite(site))) throw notFound(`site ${JSON.stringify(site)} not found`);
  return json({ files });
}

async function handleReadFile(ctx: RequestContext, [s, rawPath]: string[]): Promise<Response> {
  await gatewayUser(ctx, "read");
  const site = siteParam(ctx, s, false);
  const path = rawPath ?? "";
  const why = validatePath(path);
  if (why) throw badRequest(`invalid path ${JSON.stringify(path.slice(0, 200))}: ${why}`);
  const res = await siteStub(ctx.env, site).fetch(`https://site/raw?path=${encodeURIComponent(path)}`, { method: ctx.req.method === "HEAD" ? "HEAD" : "GET" });
  if (res.status === 404 && !res.headers.has(R2_KEY_HEADER)) throw notFound(`file ${JSON.stringify(path.slice(0, 200))} not found in site ${JSON.stringify(site)}`);
  return resolveR2Body(res, ctx.env.R2, ctx.req.method);
}

async function handleCreateDeploy(ctx: RequestContext, [s]: string[]): Promise<Response> {
  const u = await gatewayUser(ctx, "deploy");
  const site = siteParam(ctx, s, true);
  const body = await readJSON(ctx.req, deployBodyLimit(ctx));
  if (!isPlainObject(body)) throw badRequest("request body must be a JSON object");
  const { files, total } = validateFiles(ctx, body.files);
  await authorizeSite(ctx, u, site);
  const reg = registry(ctx.env);
  const cap = unwrap(await reg.canCreateSite(site, ctx.cfg.limits.max_sites));
  if (!cap.allowed) throw tooLarge(`this Backyard has reached its limit of ${ctx.cfg.limits.max_sites} sites`);
  await chargeOrThrow(ctx.env, [{ key: `deploy|${rateKey(ctx, u)}`, limit: ctx.cfg.limits.deploys_per_day }]);
  const { unchanged } = unwrap(await siteStub(ctx.env, site).deployPlan(files));
  const exp = Math.floor(Date.now() / 1000) + DEPLOY_TTL_SECONDS;
  const claims: DeployClaims = { site, actor: u.id, exp, nonce: randomHex(16) };
  const deployID = await signClaims(ctx.cfg.secret, KeyLabel.Deploy, claims);
  const same = new Set(unchanged);
  const base = gatewayURL(ctx);
  const uploads = [];
  for (const f of files) {
    if (same.has(f.path)) continue;
    const up: UploadClaims = {
      site,
      path: f.path,
      size: f.size,
      sha256: f.sha256,
      content_type: f.content_type,
      deploy_id: "d_" + claims.nonce,
      exp,
    };
    const tok = await signClaims(ctx.cfg.secret, KeyLabel.Upload, up);
    uploads.push({ path: f.path, url: `${base}/uploads/${encodeURIComponent(tok)}`, method: "PUT", headers: { "Content-Type": f.content_type } });
  }
  console.log(
    JSON.stringify({ audit: true, action: "deploy.create", actor: u.id, site, files: files.length, bytes: total, uploads: uploads.length, ip: ctx.clientIP }),
  );
  return json({ deploy_id: deployID, uploads, unchanged, expires_at: rfc3339(exp) });
}

async function handleUpload(ctx: RequestContext, [rawTok]: string[]): Promise<Response> {
  if (!ctx.cfg.secret) throw secretMissing();
  const c = await verifyClaims<UploadClaims>(ctx.cfg.secret, KeyLabel.Upload, rawTok ?? "");
  if (!c) throw forbidden("invalid upload token");
  if (typeof c.exp !== "number" || Math.floor(Date.now() / 1000) >= c.exp) throw forbidden("upload token expired; start a new deploy");
  if (siteNameError(String(c.site ?? ""), null)) throw badRequest("invalid upload token");
  const why = validatePath(c.path);
  if (why) throw badRequest(`invalid path ${JSON.stringify(String(c.path).slice(0, 200))}: ${why}`);
  if (typeof c.size !== "number" || !Number.isSafeInteger(c.size) || c.size < 0 || typeof c.sha256 !== "string" || !SHA_RE.test(c.sha256)) {
    throw badRequest("invalid upload token");
  }
  const declared = ctx.req.headers.get("Content-Length");
  if (declared !== null && Number(declared) !== c.size) {
    throw badRequest(`Content-Length ${declared} does not match the declared size ${c.size}`);
  }
  const sha = c.sha256.toLowerCase();
  const key = "s:" + sha;
  const stub = siteStub(ctx.env, c.site);
  if (unwrap(await stub.hasBlob(key, c.size))) {
    // The site already stores this content (same file under another path, or
    // a retried upload): nothing to store.
    await ctx.req.body?.cancel().catch(() => {});
    return json({ path: c.path, size: c.size, sha256: c.sha256 });
  }
  const body = ctx.req.body ?? new Blob([]).stream();
  const bucket = ctx.env.R2;
  if (bucket && c.size > ctx.cfg.fileInlineMax) {
    // Unique per upload: garbage collection of an older copy of the same
    // content can never delete this one.
    const r2Key = `sites/${c.site}/${sha}.${randomHex(8)}`;
    try {
      await bucket.put(r2Key, fixedLength(body, c.size), { sha256: sha, httpMetadata: { contentType: c.content_type || "application/octet-stream" } });
    } catch (e) {
      throw badRequest(`upload of ${JSON.stringify(c.path.slice(0, 200))} does not match the declared size and sha256 (${(e as Error).message.slice(0, 200)})`);
    }
    const reg = await stub.registerR2Blob(key, c.size, sha, r2Key);
    if (!reg.ok || !reg.value.stored) ctx.waitUntil(bucket.delete(r2Key).catch(() => {}));
    unwrap(reg);
  } else {
    if (c.size > MAX_INLINE_FILE) throw tooLarge(`${JSON.stringify(c.path)} is larger than ${MAX_INLINE_FILE} bytes; bind an R2 bucket for larger files`);
    const res = await stub.fetch(`https://site/blob?key=${encodeURIComponent(key)}&size=${c.size}&sha256=${sha}`, { method: "PUT", body });
    if (!res.ok) {
      const e = (await res.json().catch(() => ({}))) as { error?: { code?: string; message?: string } };
      const msg = res.status === 413 ? `upload of ${JSON.stringify(c.path.slice(0, 200))} is larger than the declared ${c.size} bytes` : `upload of ${JSON.stringify(c.path.slice(0, 200))} does not match the declared size and sha256`;
      throw new HttpError(res.status, (e.error?.code as HttpError["code"]) ?? "bad_request", res.status >= 500 ? (e.error?.message ?? "storing the upload failed") : msg);
    }
  }
  return json({ path: c.path, size: c.size, sha256: c.sha256 });
}

async function handleCommit(ctx: RequestContext, [s, id]: string[]): Promise<Response> {
  const u = await gatewayUser(ctx, "deploy");
  const site = siteParam(ctx, s, true);
  const dc = await verifyClaims<DeployClaims>(ctx.cfg.secret, KeyLabel.Deploy, id ?? "");
  if (!dc || typeof dc.exp !== "number" || typeof dc.site !== "string" || typeof dc.actor !== "string") throw badRequest("invalid deploy id");
  if (Math.floor(Date.now() / 1000) >= dc.exp) throw badRequest("deploy id expired; start a new deploy");
  if (dc.site !== site) throw badRequest("deploy id belongs to another site");
  if (!(dc.actor === u.id || (u.anonymous && dc.actor.startsWith("anon:")))) throw forbidden("deploy id belongs to another user");
  const body = await readJSON(ctx.req, deployBodyLimit(ctx));
  if (!isPlainObject(body)) throw badRequest("request body must be a JSON object");
  const { files, total } = validateFiles(ctx, body.files);
  const prev = await authorizeSite(ctx, u, site);
  const reg = registry(ctx.env);
  if (!prev) {
    // A new site: take a slot of the platform cap atomically (other first
    // deploys may have committed since this deploy was created, or be
    // committing right now), before the site's files are switched.
    const cap = unwrap(await reg.reserveSite(site, ctx.cfg.limits.max_sites, SITE_RESERVATION_MS));
    if (!cap.allowed) throw tooLarge(`this Backyard has reached its limit of ${ctx.cfg.limits.max_sites} sites`);
  }
  let r: CommitResult;
  try {
    r = unwrap(await siteStub(ctx.env, site).deployCommit(files));
  } catch (e) {
    if (!prev) ctx.waitUntil(reg.releaseSite(site).catch(() => {}));
    throw e;
  }
  let { title, description } = r;
  if (r.titleR2Key && ctx.env.R2) {
    const obj = await ctx.env.R2.get(r.titleR2Key, { range: { offset: 0, length: TITLE_SCAN_BYTES } });
    if (obj && "body" in obj) ({ title, description } = extractTitle(new Uint8Array(await obj.arrayBuffer())));
  }
  const now = formatMicros(Date.now() * 1000);
  const meta: SiteRecord = {
    name: site,
    created_at: prev?.created_at || now,
    created_by: prev?.created_by || u.id,
    updated_at: now,
    updated_by: u.id,
    deploy_id: "d_" + dc.nonce,
    file_count: files.length,
    bytes: total,
    owners: prev?.owners ? [...prev.owners] : [],
  };
  if (title) meta.title = title;
  if (description) meta.description = description;
  if (ctx.cfg.ownership === "owner" && meta.owners.length === 0 && !u.anonymous) meta.owners = [u.id];
  unwrap(await reg.putSite(meta));
  if (r.r2Garbage.length && ctx.env.R2) ctx.waitUntil(deleteR2(ctx.env.R2, r.r2Garbage));
  console.log(JSON.stringify({ audit: true, action: "deploy.commit", actor: u.id, site, files: files.length, bytes: total, deleted: r.deleted }));
  const url = siteURL(ctx, site);
  return json({ site: siteMetaJSON(meta, url), url, deleted: r.deleted });
}

async function handleRefresh(ctx: RequestContext, [s]: string[]): Promise<Response> {
  const u = await gatewayUser(ctx, "deploy");
  const site = siteParam(ctx, s, true);
  const prev = await authorizeSite(ctx, u, site);
  const info = unwrap(await siteStub(ctx.env, site).siteInfo());
  if (info.fileCount === 0 && !prev) throw notFound(`site ${JSON.stringify(site)} not found`);
  let { title, description } = info;
  if (info.titleR2Key && ctx.env.R2) {
    const obj = await ctx.env.R2.get(info.titleR2Key, { range: { offset: 0, length: TITLE_SCAN_BYTES } });
    if (obj && "body" in obj) ({ title, description } = extractTitle(new Uint8Array(await obj.arrayBuffer())));
  }
  const now = formatMicros(Date.now() * 1000);
  const meta: SiteRecord = {
    name: site,
    created_at: prev?.created_at || now,
    created_by: prev?.created_by || u.id,
    updated_at: info.updatedAt ? formatMicros(info.updatedAt * 1000) : now,
    updated_by: prev?.updated_by || u.id,
    file_count: info.fileCount,
    bytes: info.bytes,
    owners: prev?.owners ? [...prev.owners] : [],
  };
  if (prev?.deploy_id) meta.deploy_id = prev.deploy_id;
  if (title) meta.title = title;
  if (description) meta.description = description;
  unwrap(await registry(ctx.env).putSite(meta));
  return json(siteMetaJSON(meta, siteURL(ctx, site)));
}

async function handleDeleteSite(ctx: RequestContext, [s]: string[]): Promise<Response> {
  const u = await gatewayUser(ctx, "manage");
  const site = siteParam(ctx, s, false);
  const prev = await authorizeSite(ctx, u, site);
  const stub = siteStub(ctx.env, site);
  if (!prev && unwrap(await stub.listFiles()).length === 0) throw notFound(`site ${JSON.stringify(site)} not found`);
  const { r2Keys } = unwrap(await stub.destroy());
  unwrap(await registry(ctx.env).deleteSite(site));
  if (ctx.env.R2) {
    // Before answering, so that a redeploy right after the delete can never
    // lose objects to a cleanup still running in the background. The prefix
    // sweeps also catch objects of deploys that never committed.
    const bucket = ctx.env.R2;
    try {
      await deleteR2(bucket, r2Keys);
      await deleteR2Prefix(bucket, `sites/${site}/`);
      await deleteR2Prefix(bucket, `uploads/${site}/`);
    } catch (e) {
      console.warn(`backyard: deleting the R2 objects of site ${site} failed: ${(e as Error).message}`);
    }
  }
  console.log(JSON.stringify({ audit: true, action: "site.delete", actor: u.id, site, ip: ctx.clientIP }));
  return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
}

async function handleOwners(ctx: RequestContext, [s]: string[]): Promise<Response> {
  const u = await gatewayUser(ctx, "manage");
  if (ctx.cfg.ownership !== "owner") throw unsupported("site ownership is disabled on this server (OWNERSHIP=none)");
  const site = siteParam(ctx, s, false);
  const body = await readJSON(ctx.req, 64 << 10);
  if (!isPlainObject(body)) throw badRequest("request body must be a JSON object");
  let add: string[];
  let remove: string[];
  try {
    add = normalizeOwners(body.add, MAX_OWNERS);
    remove = normalizeOwners(body.remove, MAX_OWNERS);
  } catch (e) {
    throw badRequest((e as Error).message);
  }
  const meta = await authorizeSite(ctx, u, site);
  if (!meta) throw notFound(`site ${JSON.stringify(site)} not found`);
  let owners = [...(meta.owners ?? [])];
  for (const o of add) if (!owners.some((x) => x.toLowerCase() === o)) owners.push(o);
  owners = owners.filter((x) => !remove.includes(x.toLowerCase()));
  if (owners.length > MAX_OWNERS) throw badRequest(`a site can have at most ${MAX_OWNERS} owners`);
  meta.owners = owners;
  unwrap(await registry(ctx.env).putSite(meta));
  console.log(JSON.stringify({ audit: true, action: "site.owners", actor: u.id, site, owners }));
  return json(siteMetaJSON(meta, siteURL(ctx, site)));
}

/** Deletes R2 keys in batches. */
async function deleteR2(bucket: R2Bucket, keys: string[]): Promise<void> {
  for (let i = 0; i < keys.length; i += 1000) await bucket.delete(keys.slice(i, i + 1000));
}

async function deleteR2Prefix(bucket: R2Bucket, prefix: string): Promise<void> {
  let cursor: string | undefined;
  do {
    const list = await bucket.list({ prefix, cursor, limit: 1000 });
    if (list.objects.length) await bucket.delete(list.objects.map((o) => o.key));
    cursor = list.truncated ? list.cursor : undefined;
  } while (cursor);
}

type GHandler = (ctx: RequestContext, params: string[]) => Promise<Response>;

const S = "([^/]+)";
const ROUTES: { path: RegExp; methods: Record<string, GHandler> }[] = [
  { path: /^\/info$/, methods: { GET: async (ctx) => json(infoDocument(ctx), 200, { "Cache-Control": "no-cache" }) } },
  { path: /^\/me$/, methods: { GET: handleMe } },
  { path: /^\/sites$/, methods: { GET: handleListSites } },
  { path: new RegExp(`^/sites/${S}$`), methods: { GET: handleGetSite, DELETE: handleDeleteSite } },
  { path: new RegExp(`^/sites/${S}/files$`), methods: { GET: handleFiles } },
  { path: new RegExp(`^/sites/${S}/files/(.+)$`), methods: { GET: handleReadFile } },
  { path: new RegExp(`^/sites/${S}/owners$`), methods: { POST: handleOwners } },
  { path: new RegExp(`^/sites/${S}/refresh$`), methods: { POST: handleRefresh } },
  { path: new RegExp(`^/sites/${S}/deploys$`), methods: { POST: handleCreateDeploy } },
  { path: new RegExp(`^/sites/${S}/deploys/${S}/commit$`), methods: { POST: handleCommit } },
  { path: /^\/uploads\/([^/]+)$/, methods: { PUT: handleUpload } },
];

/** Serves /__backyard/api/deploy/* (sub is the path below it). */
export async function serveGateway(ctx: RequestContext, sub: string): Promise<Response> {
  if (!csrfOK(ctx, sub)) throw forbidden('missing "X-Backyard: 1" header');
  const method = ctx.req.method === "HEAD" ? "GET" : ctx.req.method;
  for (const r of ROUTES) {
    const m = r.path.exec(sub);
    if (!m) continue;
    const h = r.methods[method];
    if (!h) throw methodNotAllowed(Object.keys(r.methods).flatMap((k) => (k === "GET" ? ["GET", "HEAD"] : [k])));
    return h(ctx, m.slice(1));
  }
  throw notFound(`no such endpoint: ${ctx.req.method} ${sub.slice(0, 200)}`);
}
