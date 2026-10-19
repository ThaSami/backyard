/**
 * The deploy gateway (SPEC §7) mounted at /__backyard/api/deploy: /info, /me,
 * sites, files, deploys (diffed create → self-hosted uploads → commit),
 * refresh, owners and delete. Deploy ids and upload URLs are HMAC-signed with
 * the same keys and encoding as the Go gateway.
 *
 * It also publishes from memory for the Claude connector (publishFiles,
 * SPEC §14.1): the files arrive inside a backyard_publish tool call, are
 * stored like uploads and merged into the site in one SiteDO transaction.
 */
import { anonCookieUser, authenticate, authenticateBearer, guestFromIP, hasBearer, isCookieConflict, rateKey, secretMissing } from "./auth";
import { maxFileBytes, MAX_INLINE_FILE, type Config } from "./config";
import { KeyLabel, randomHex, sha256Hex, signClaims, verifyClaims } from "./crypto";
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
import { mcpEnabled, mcpResource } from "./oauth";
import { normalizeOwners, validatePath } from "./paths";
import type { SiteRecord } from "./registry-do";
import { gatewayURL, siteNameError, siteURL, type RequestContext } from "./router";
import type { CommitResult, DeployFile, MergeOutcome, MergeRefusal, PageInfo } from "./site-do";
import { fixedLength } from "./site-api";
import { filterListed, isOwner, siteMetaJSON } from "./sitemeta";
import { chargeOrThrow, registry, siteStub, VERSION } from "./stubs";
import { extractPage, TITLE_SCAN_BYTES, type Page } from "./title";

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
  const doc: Record<string, unknown> = {
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
    // Who the site lists show a site to (SPEC §7.1, §7.2 "Listing"): always present.
    site_listing: cfg.siteListing,
  };
  // The Claude connector's endpoint (SPEC §7.1, §14.1), while it is on.
  if (mcpEnabled(ctx)) doc.mcp_url = mcpResource(ctx);
  doc.edition = "cloudflare";
  return doc;
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

/** GET /sites: the sites listed to the caller (SITE_LISTING, listedTo); lookups by name are not filtered. */
async function handleListSites(ctx: RequestContext): Promise<Response> {
  const u = await gatewayUser(ctx, "read");
  const list = filterListed(ctx.cfg, u, unwrap(await registry(ctx.env).listSites()));
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
  await storeSiteBlob(ctx, c.site, { path: c.path, size: c.size, sha256: c.sha256, content_type: c.content_type }, ctx.req.body ?? new Blob([]).stream());
  return json({ path: c.path, size: c.size, sha256: c.sha256 });
}

/**
 * Stores the content of one deploy file in a site (an upload's request
 * stream, or the decoded bytes of a connector publish): nothing when the site
 * already holds that content, R2 above FILE_INLINE_MAX when a bucket is
 * bound, else the SiteDO's chunks. The size and sha256 are verified on the
 * way in; a mismatch is a 400.
 */
export async function storeSiteBlob(ctx: RequestContext, site: string, f: DeployFile, body: ReadableStream<Uint8Array> | Uint8Array): Promise<void> {
  const sha = f.sha256.toLowerCase();
  const key = "s:" + sha;
  const name = JSON.stringify(f.path.slice(0, 200));
  const stub = siteStub(ctx.env, site);
  if (unwrap(await stub.hasBlob(key, f.size))) {
    // The site already stores this content (same file under another path, or
    // a retried upload): nothing to store.
    if (body instanceof ReadableStream) await body.cancel().catch(() => {});
    return;
  }
  const bucket = ctx.env.R2;
  if (bucket && f.size > ctx.cfg.fileInlineMax) {
    // Unique per upload: garbage collection of an older copy of the same
    // content can never delete this one.
    const r2Key = `sites/${site}/${sha}.${randomHex(8)}`;
    try {
      await bucket.put(r2Key, body instanceof ReadableStream ? fixedLength(body, f.size) : body, {
        sha256: sha,
        httpMetadata: { contentType: f.content_type || "application/octet-stream" },
      });
    } catch (e) {
      throw badRequest(`upload of ${name} does not match the declared size and sha256 (${(e as Error).message.slice(0, 200)})`);
    }
    const reg = await stub.registerR2Blob(key, f.size, sha, r2Key);
    if (!reg.ok || !reg.value.stored) ctx.waitUntil(bucket.delete(r2Key).catch(() => {}));
    unwrap(reg);
    return;
  }
  if (f.size > MAX_INLINE_FILE) throw tooLarge(`${JSON.stringify(f.path)} is larger than ${MAX_INLINE_FILE} bytes; bind an R2 bucket for larger files`);
  const res = await stub.fetch(`https://site/blob?key=${encodeURIComponent(key)}&size=${f.size}&sha256=${sha}`, { method: "PUT", body });
  if (!res.ok) {
    const e = (await res.json().catch(() => ({}))) as { error?: { code?: string; message?: string } };
    const msg = res.status === 413 ? `upload of ${name} is larger than the declared ${f.size} bytes` : `upload of ${name} does not match the declared size and sha256`;
    throw new HttpError(res.status, (e.error?.code as HttpError["code"]) ?? "bad_request", res.status >= 500 ? (e.error?.message ?? "storing the upload failed") : msg);
  }
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
  const page = await sitePage(ctx, site, r);
  const meta = nextSiteRecord(ctx, u, site, prev, {
    // The SiteDO's time of the switch, which a refresh reports too.
    updatedAt: formatMicros(r.committedAt * 1000),
    deployID: "d_" + dc.nonce,
    fileCount: files.length,
    bytes: total,
    ...page,
  });
  unwrap(await reg.putSite(meta));
  dropR2Garbage(ctx, r.r2Garbage);
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
  // Strict: a refresh that cannot read index.html fails and keeps the record (as on the Go server).
  const { title, description, preview } = await sitePage(ctx, site, info, undefined, true);
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
  if (preview) meta.preview = preview;
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

// --- shared by commits, refreshes and connector publishes ------------------------

/** What a site's files say about it after a write (SPEC §7.2). */
interface SitePage {
  title: string;
  description: string;
  preview: string;
}

/**
 * Title, description and preview of a site: as the SiteDO found them, or,
 * when its index.html lives in R2, from the head of that file (head when
 * the caller has the bytes, else read from R2) with the SiteDO choosing the
 * preview for its og:image. Best effort for a commit or a publish, like the
 * Go server's readPage: the files are already switched, so a failed read
 * leaves what the SiteDO found, and a failed choice of the preview keeps the
 * title and description read and the SiteDO's preview. strict (a refresh,
 * which switches nothing, and the settling of a record) throws instead, like
 * the Go server's Refresh, so that the record is left as it is.
 */
async function sitePage(ctx: RequestContext, site: string, r: PageInfo, head?: Uint8Array, strict = false): Promise<SitePage> {
  const stored = { title: r.title, description: r.description, preview: r.preview };
  if (!r.titleR2Key) return stored;
  let page: Page;
  try {
    if (!head) {
      if (!ctx.env.R2) return stored;
      const obj = await ctx.env.R2.get(r.titleR2Key, { range: { offset: 0, length: TITLE_SCAN_BYTES } });
      if (!obj || !("body" in obj)) return stored;
      head = new Uint8Array(await obj.arrayBuffer());
    }
    page = extractPage(head);
  } catch (e) {
    if (strict) throw e;
    console.warn(`backyard: reading index.html of ${site} for its title failed: ${(e as Error)?.message ?? e}`);
    return stored;
  }
  let preview = r.preview;
  if (page.image) {
    try {
      preview = unwrap(await siteStub(ctx.env, site).previewFor(page.image));
    } catch (e) {
      if (strict) throw e;
      console.warn(`backyard: choosing the preview of ${site} failed: ${(e as Error)?.message ?? e}`);
    }
  }
  return { title: page.title, description: page.description, preview };
}

/**
 * The SiteMeta record after a deploy by u: creation fields and owners from
 * prev; on a new site with OWNERSHIP=owner its first (signed-in) deployer
 * becomes the owner.
 */
function nextSiteRecord(
  ctx: RequestContext,
  u: User,
  site: string,
  prev: SiteRecord | null,
  f: { updatedAt: string; deployID: string; fileCount: number; bytes: number } & SitePage,
): SiteRecord {
  const meta: SiteRecord = {
    name: site,
    created_at: prev?.created_at || f.updatedAt,
    created_by: prev?.created_by || u.id,
    updated_at: f.updatedAt,
    updated_by: u.id,
    deploy_id: f.deployID,
    file_count: f.fileCount,
    bytes: f.bytes,
    owners: prev?.owners ? [...prev.owners] : [],
  };
  if (f.title) meta.title = f.title;
  if (f.description) meta.description = f.description;
  if (f.preview) meta.preview = f.preview;
  if (ctx.cfg.ownership === "owner" && meta.owners.length === 0 && !u.anonymous) meta.owners = [u.id];
  return meta;
}

/** Deletes R2 objects a commit released, after the response. */
function dropR2Garbage(ctx: RequestContext, keys: string[]): void {
  if (keys.length && ctx.env.R2) ctx.waitUntil(deleteR2(ctx.env.R2, keys).catch(() => {}));
}

/** A byte count for messages, like the Go server's FormatBytes ("812 B", "1.2 MiB"). */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  let div = 1024;
  let exp = 0;
  for (let m = Math.floor(n / 1024); m >= 1024; m = Math.floor(m / 1024)) {
    div *= 1024;
    exp++;
  }
  // One decimal, rounded half to even (Go's %.1f): n*10/div is exact here.
  const t = (n * 10) / div;
  let r = Math.floor(t);
  const frac = t - r;
  if (frac > 0.5 || (frac === 0.5 && r % 2 === 1)) r++;
  return `${Math.floor(r / 10)}.${r % 10} ${"KMGTPE"[exp]}iB`;
}

// --- the Claude connector's publish (SPEC §14.1) -------------------------------------

/** Paths of one publish call at most (files, and delete). */
export const MAX_PUBLISH_LIST = 1000;
/** Paths named in one error message at most. */
const MAX_LISTED_PATHS = 10;
/** Concurrent blob writes of one publish. */
const STORE_PARALLELISM = 4;

/** One file of a publish, already decoded. */
export interface PublishFile {
  path: string;
  data: Uint8Array;
  /** "" = by extension (or sniffed); set by checkPublish. */
  contentType: string;
}

/** One backyard_publish call. */
export interface PublishRequest {
  user: User;
  site: string;
  files: PublishFile[];
  delete: string[];
  replace: boolean;
  overwrite: boolean;
  /** For the deploy.commit audit line. */
  clientName: string;
  grantID: string;
}

/** The site after a publish (the tool's result object, without the text-as-binary warnings). */
export interface PublishResult {
  site: string;
  url: string;
  created: boolean;
  files: number;
  uploaded: number;
  unchanged: number;
  deleted: number;
  bytes: number;
  warnings: string[];
}

/**
 * A refusal or failure with its id of the connector's error catalog ("E2" …
 * "E27", SPEC §14.1) and the tool error text. E21 (storage) has no request
 * id: the connector adds it.
 */
export interface PublishError {
  id: string;
  message: string;
  /** Seconds (E19). */
  retryAfter?: number;
}

/** E21 without the request id. */
export const PUBLISH_STORE_FAILED = "Publishing failed: Backyard could not store the files. Publishing the same files again completes it.";

const pubErr = (id: string, message: string): PublishError => ({ id, message });

/** Truncates s to at most n UTF-8 bytes (on a character boundary) with "…", as the Go server does. */
export function truncateBytes(s: string, n: number): string {
  const b = utf8.encode(s);
  if (b.length <= n) return s;
  let i = n;
  while (i > 0 && (b[i]! & 0xc0) === 0x80) i--;
  return new TextDecoder().decode(b.subarray(0, i)) + "…";
}

const utf8 = new TextEncoder();

/** Whether anonymous guests may deploy (AUTH_MODE=none with ALLOW_ANONYMOUS_DEPLOY). */
export function anonymousDeployAllowed(cfg: Config): boolean {
  return cfg.authMode === "none" && cfg.allowAnonymousDeploy;
}

/** E8 for a path that cannot be published, or null. */
function pathError(p: string): PublishError | null {
  const why = validatePath(p);
  return why ? pubErr("E8", `"${truncateBytes(p, 200)}" is not an allowed path: ${why}.`) : null;
}

/**
 * The checks of a publish that need no storage, in their normative order
 * (SPEC §14.1 steps 1–4), filling in default content types. decode, when
 * given, sets files[i].data once that file's path checks passed, so an
 * encoding error (E7) comes in its place in the order.
 */
export function checkPublish(ctx: RequestContext, req: PublishRequest, decode?: (i: number) => Uint8Array | PublishError): PublishError | null {
  const { cfg } = ctx;
  const { user: u, site } = req;
  if (u.anonymous && !anonymousDeployAllowed(cfg)) {
    return pubErr(
      "E17",
      "This Backyard has no sign-in and does not accept anonymous publishing, so this connection can only read. Ask the Backyard admin to set up sign-in or allow anonymous deploys.",
    );
  }
  if (siteNameError(site, null)) {
    return pubErr("E2", `"${truncateBytes(site, 100)}" is not a valid site name: use lowercase letters, digits and '-' (at most 63 characters), e.g. "lunch-poll".`);
  }
  if (cfg.reserved.some((r) => r.toLowerCase() === site)) return pubErr("E3", `"${site}" is reserved on this Backyard; choose another name.`);
  if (req.files.length === 0 && req.delete.length === 0) return pubErr("E4", "Nothing to publish: give files, delete, or both.");
  if (req.replace && req.delete.length > 0) return pubErr("E5", 'Use either "replace" or "delete", not both.');
  if (req.replace && req.files.length === 0) return pubErr("E6", "A site needs at least one file.");
  if (req.files.length > MAX_PUBLISH_LIST) return pubErr("E1", `Invalid arguments: files has ${req.files.length} entries; at most ${MAX_PUBLISH_LIST} are allowed.`);
  if (req.delete.length > MAX_PUBLISH_LIST) return pubErr("E1", `Invalid arguments: delete has ${req.delete.length} entries; at most ${MAX_PUBLISH_LIST} are allowed.`);
  const deletes = new Set(req.delete);
  const seen = new Set<string>();
  const maxFile = maxFileBytes(cfg, !!ctx.env.R2);
  let total = 0;
  for (let i = 0; i < req.files.length; i++) {
    const f = req.files[i]!;
    const bad = pathError(f.path);
    if (bad) return bad;
    if (seen.has(f.path)) return pubErr("E9", `"${truncateBytes(f.path, 200)}" appears more than once.`);
    seen.add(f.path);
    if (deletes.has(f.path)) return pubErr("E10", `"${truncateBytes(f.path, 200)}" is both in files and in delete.`);
    if (decode) {
      const d = decode(i);
      if (!(d instanceof Uint8Array)) return d;
      f.data = d;
    }
    const size = f.data.byteLength;
    if (maxFile > 0 && size > maxFile) return pubErr("E12", `${truncateBytes(f.path, 200)} is ${formatBytes(size)}; files may be at most ${formatBytes(maxFile)}.`);
    // eslint-disable-next-line no-control-regex
    if (f.contentType.length > 255 || /[\u0000-\u001f\u007f]/.test(f.contentType)) {
      return pubErr("E1", `Invalid arguments: files[${i}].content_type is not a valid content type (at most 255 characters, no control characters).`);
    }
    f.contentType = f.contentType.trim();
    total += size;
  }
  const cap = cfg.limits.mcp_publish_bytes;
  if (cap > 0 && total > cap) {
    return pubErr(
      "E11",
      `This call carries ${formatBytes(total)} of file content; the limit is ${formatBytes(cap)} per call. Send the files over several calls (files you do not list are kept).`,
    );
  }
  for (const p of req.delete) {
    const bad = pathError(p);
    if (bad) return bad;
  }
  for (const f of req.files) if (!f.contentType) f.contentType = detectContentType(f.path, f.data.subarray(0, 512));
  return null;
}

/** Whether updatedBy names someone other than u (case-insensitive id or email). */
function foreignPublisher(updatedBy: string | undefined, u: User): boolean {
  if (!updatedBy) return false;
  const who = updatedBy.toLowerCase();
  return ![u.id, u.email ?? ""].some((x) => x !== "" && x.toLowerCase() === who);
}

/** "YYYY-MM-DD HH:MM" (UTC) of an RFC 3339 time, or "". */
function minuteUTC(rfc3339: string): string {
  const t = Date.parse(rfc3339);
  return Number.isFinite(t) ? new Date(t).toISOString().slice(0, 16).replace("T", " ") : "";
}

/** Names at most MAX_LISTED_PATHS paths for a message. */
function listPaths(paths: string[]): string {
  const named = paths.slice(0, MAX_LISTED_PATHS).map((p) => truncateBytes(p, 200));
  if (paths.length > MAX_LISTED_PATHS) named.push(`and ${paths.length - MAX_LISTED_PATHS} more`);
  return named.join(", ");
}

/** Runs fn over items with at most n in flight. */
async function eachLimit<T>(items: T[], n: number, fn: (item: T) => Promise<void>): Promise<void> {
  let next = 0;
  const worker = async () => {
    while (next < items.length) await fn(items[next++]!);
  };
  await Promise.all(Array.from({ length: Math.min(n, items.length) }, worker));
}

/**
 * Publishes req for the Claude connector (SPEC §14.1 steps 6–10): the
 * ownership policy, the overwrite guard, the site cap and deploys_per_day
 * (keyed user:<id>, guests included), then the changed contents are stored
 * and SiteDO.deployMerge switches the file list in one transaction, so
 * concurrent publishes of a site never lose each other's files. Merge by
 * default: listed files are written, delete removes paths, the others are
 * kept; replace makes the site exactly the files. Never throws: storage
 * failures are E21.
 */
export async function publishFiles(ctx: RequestContext, req: PublishRequest): Promise<{ ok: true; result: PublishResult } | { ok: false; error: PublishError }> {
  const refused = checkPublish(ctx, req);
  if (refused) return { ok: false, error: refused };
  const { cfg, env } = ctx;
  const { user: u, site } = req;
  const reg = registry(env);
  const stub = siteStub(env, site);
  let reserved = false;
  try {
    const prev = unwrap(await reg.getSite(site));
    // Ownership (anonymous users are never owners).
    if (cfg.ownership === "owner" && !u.admin && prev?.owners?.length && (u.anonymous || !isOwner(prev, u.id, u.email ?? ""))) {
      return {
        ok: false,
        error: pubErr("E15", `${site} belongs to ${prev.owners.join(", ")}; you are not an owner. Ask an owner to add you, or publish under another name.`),
      };
    }
    // The overwrite guard: nothing changes and nothing is charged.
    if (prev && !req.overwrite && foreignPublisher(prev.updated_by, u)) {
      const when = minuteUTC(prev.updated_at);
      return {
        ok: false,
        error: pubErr(
          "E16",
          `${site} was last published by ${prev.updated_by}${when ? ` on ${when} UTC` : ""}. Nothing was changed. Ask the user whether to replace it; if so, call again with "overwrite": true, otherwise choose another name.`,
        ),
      };
    }
    const siteCap = () => pubErr("E27", `This Backyard already has its maximum number of sites (${cfg.limits.max_sites}).`);
    if (!prev && !unwrap(await reg.canCreateSite(site, cfg.limits.max_sites)).allowed) return { ok: false, error: siteCap() };
    try {
      await chargeOrThrow(env, [{ key: `deploy|user:${u.id}`, limit: cfg.limits.deploys_per_day }]);
    } catch (e) {
      if (!(e instanceof HttpError) || e.status !== 429) throw e;
      const wait = e.retryAfter ?? 3600;
      return {
        ok: false,
        error: { id: "E19", message: `Publishing limit reached (${cfg.limits.deploys_per_day} per day). Try again in ${Math.max(1, Math.ceil(wait / 60))} minutes.`, retryAfter: wait },
      };
    }

    const files: DeployFile[] = await Promise.all(
      req.files.map(async (f) => ({ path: f.path, size: f.data.byteLength, sha256: await sha256Hex(f.data), content_type: f.contentType })),
    );
    const bytesOf = new Map(req.files.map((f) => [f.path, f.data]));
    const store = (list: DeployFile[]) => eachLimit(list, STORE_PARALLELISM, (f) => storeSiteBlob(ctx, site, f, bytesOf.get(f.path)!));
    const limits = { maxFiles: cfg.limits.max_files, maxSiteBytes: cfg.limits.max_site_bytes };
    const refusal = (r: MergeRefusal): PublishError => {
      switch (r.error) {
        case "empty":
          return pubErr("E6", "A site needs at least one file.");
        case "files":
          return pubErr("E13", `The site would have ${r.count} files; this Backyard allows ${cfg.limits.max_files} per site.`);
        case "bytes":
          return pubErr("E13", `The site would be ${formatBytes(r.bytes)}; this Backyard allows ${formatBytes(cfg.limits.max_site_bytes)} per site.`);
        case "tree":
          return pubErr("E14", `These paths would be both a file and a folder: ${listPaths(r.paths)}.`);
      }
    };
    // The plan: contents the site already has, and limits the site as it is
    // now would break (then nothing is uploaded; the merge checks again).
    const plan = unwrap(await stub.mergePlan(files, req.delete, req.replace, limits));
    if (plan.refusal) return { ok: false, error: refusal(plan.refusal) };
    const same = new Set(plan.unchanged);
    await store(files.filter((f) => !same.has(f.path)));
    if (!prev) {
      // A slot of the platform's site cap, taken atomically until putSite.
      if (!unwrap(await reg.reserveSite(site, cfg.limits.max_sites, SITE_RESERVATION_MS)).allowed) return { ok: false, error: siteCap() };
      reserved = true;
    }
    let m: MergeOutcome = unwrap(await stub.deployMerge(files, req.delete, req.replace, limits));
    if (!m.ok && m.error === "missing") {
      // Content released by a concurrent publish in between: store it again, once.
      const again = new Set(m.paths);
      await store(files.filter((f) => again.has(f.path)));
      m = unwrap(await stub.deployMerge(files, req.delete, req.replace, limits));
    }
    if (!m.ok) {
      if (reserved) ctx.waitUntil(reg.releaseSite(site).catch(() => {}));
      if (m.error !== "missing") return { ok: false, error: refusal(m) };
      console.error(`backyard: mcp: publish of ${site}: the contents of ${m.paths.length} file(s) were not stored`);
      return { ok: false, error: pubErr("E21", PUBLISH_STORE_FAILED) };
    }

    // Title, description and preview of the files the site has now (a sent
    // index.html is the stored one: its bytes spare the read from R2).
    const page = await sitePage(ctx, site, m, bytesOf.get("index.html"));
    const deployID = "d_" + randomHex(16);
    // The freshest record: a concurrent first publish may have recorded the site (and its owner) by now.
    const latest = unwrap(await reg.getSite(site)) ?? prev;
    const meta = nextSiteRecord(ctx, u, site, latest, {
      updatedAt: formatMicros(m.committedAt * 1000),
      deployID,
      fileCount: m.fileCount,
      bytes: m.bytes,
      ...page,
    });
    unwrap(await reg.putSite(meta));
    reserved = false;
    dropR2Garbage(ctx, m.r2Garbage);
    await settleSiteRecord(ctx, site, m.committedAt);

    const url = siteURL(ctx, site);
    const uploaded = files.length - m.unchanged;
    console.log(
      JSON.stringify({
        audit: true,
        action: "deploy.commit",
        actor: u.id,
        site,
        deploy_id: deployID,
        files: m.fileCount,
        bytes: m.bytes,
        deleted: m.deleted,
        uploaded,
        unchanged: m.unchanged,
        replace: req.replace,
        via: "mcp",
        client: req.clientName,
        grant: req.grantID.slice(0, 8),
        ip: ctx.clientIP,
      }),
    );
    const warnings: string[] = [];
    if (!m.hasIndex) warnings.push(`No index.html at the site root: ${url} answers 404 until one is published.`);
    for (const p of m.missingDeletes) warnings.push(`"${truncateBytes(p, 200)}" in delete does not exist.`);
    return {
      ok: true,
      result: {
        site,
        url,
        created: !prev && !m.existed,
        files: m.fileCount,
        uploaded,
        unchanged: m.unchanged,
        deleted: m.deleted,
        bytes: m.bytes,
        warnings,
      },
    };
  } catch (e) {
    if (reserved) ctx.waitUntil(reg.releaseSite(site).catch(() => {}));
    console.error(`backyard: mcp: publish of ${site} failed: ${(e as Error)?.message ?? e}`);
    return { ok: false, error: pubErr("E21", PUBLISH_STORE_FAILED) };
  }
}

/**
 * Keeps the site record in step with the files when another commit switched
 * the site after this publish (concurrent publishes write their records in
 * any order): when its file count, size, title, description or preview is
 * not what the site has now, the record gets those of the site as it is now
 * and the time of its latest switch (so the hub's preview URL changes too).
 * They are derived strictly: what cannot be read leaves the record alone.
 */
async function settleSiteRecord(ctx: RequestContext, site: string, committedAt: number): Promise<void> {
  try {
    const info = unwrap(await siteStub(ctx.env, site).siteInfo());
    if (info.updatedAt === committedAt) return;
    const reg = registry(ctx.env);
    const rec = unwrap(await reg.getSite(site));
    if (!rec) return;
    const { title, description, preview } = await sitePage(ctx, site, info, undefined, true);
    if (
      rec.file_count === info.fileCount &&
      rec.bytes === info.bytes &&
      (rec.title ?? "") === title &&
      (rec.description ?? "") === description &&
      (rec.preview ?? "") === preview
    ) {
      return;
    }
    const next: SiteRecord = { ...rec, file_count: info.fileCount, bytes: info.bytes };
    if (info.updatedAt) next.updated_at = formatMicros(info.updatedAt * 1000);
    delete next.title;
    delete next.description;
    delete next.preview;
    if (title) next.title = title;
    if (description) next.description = description;
    if (preview) next.preview = preview;
    unwrap(await reg.putSite(next));
  } catch (e) {
    console.warn(`backyard: mcp: updating the record of ${site} failed: ${(e as Error)?.message ?? e}`);
  }
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
