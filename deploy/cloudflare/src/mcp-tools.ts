/**
 * The tools of the Claude connector (SPEC §14.1), run by src/mcp.ts on
 * arguments that passed their input schema: backyard_docs, backyard_whoami,
 * backyard_list_sites, backyard_site_files, backyard_read_site_file and
 * backyard_publish (gateway.ts publishFiles). Texts and error messages follow
 * the catalog of SPEC §14.1, identical to the Go server's internal/mcpremote.
 */
import { maxFileBytes } from "./config";
import { anonymousDeployAllowed, checkPublish, formatBytes, publishFiles, truncateBytes, type PublishError, type PublishRequest } from "./gateway";
import { REFERENCES, SKILL_MD } from "./generated/skills";
import { unwrap } from "./http";
import type { User } from "./identity";
import { extOf } from "./mime";
import { mcpIssuer, type McpGrant } from "./oauth";
import { R2_KEY_HEADER, R2_RANGE_HEADER } from "./object";
import { validatePath } from "./paths";
import type { SiteRecord } from "./registry-do";
import { siteURL, type RequestContext } from "./router";
import { filterListed } from "./sitemeta";
import { registry, siteStub } from "./stubs";
import { utf8SeqLen } from "./title";

/** One tools/call: the request, the re-gated user and the connection. */
export interface ToolCall {
  ctx: RequestContext;
  user: User;
  grant: McpGrant;
  /** X-Request-Id, named in internal errors. */
  requestID: string;
}

/** A tool's text and whether it is an error (isError). */
export interface ToolResult {
  text: string;
  isError?: boolean;
}

type ToolFunc = (c: ToolCall, args: Record<string, unknown>) => Promise<ToolResult>;

/** backyard_read_site_file reads at most this much (128 KiB). */
export const MAX_READ_BYTES = 128 << 10;
/** backyard_list_sites lists at most this many sites. */
const MAX_LISTED_SITES = 200;
/** backyard_site_files lists at most this many files. */
const MAX_LISTED_FILES = 2000;
/** How much of a file is checked for NUL bytes. */
const BINARY_SNIFF_BYTES = 8 << 10;

/**
 * Extensions of binary formats: backyard_read_site_file refuses them, and a
 * publish warns when one is sent as text (the Go server's mcpserver.BinaryExt).
 */
const BINARY_EXTS = new Set([
  ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".ico", ".bmp", ".tif", ".tiff", ".heic", ".psd",
  ".pdf", ".zip", ".gz", ".tgz", ".bz2", ".xz", ".zst", ".br", ".7z", ".rar", ".tar", ".jar",
  ".wasm", ".woff", ".woff2", ".ttf", ".otf", ".eot",
  ".mp3", ".mp4", ".m4a", ".m4v", ".wav", ".ogg", ".oga", ".webm", ".mov", ".avi", ".mkv", ".flac", ".aac",
  ".glb", ".bin", ".exe", ".dll", ".so", ".dylib", ".class", ".parquet", ".sqlite", ".db",
  ".xls", ".xlsx", ".doc", ".docx", ".ppt", ".pptx",
]);

/** Whether p has the extension of a binary format. */
export function binaryExt(p: string): boolean {
  return BINARY_EXTS.has(extOf(p));
}

const ok = (text: string): ToolResult => ({ text });
const failure = (text: string): ToolResult => ({ text, isError: true });
const okJSON = (v: unknown): ToolResult => ok(JSON.stringify(v));
const internalFailure = (c: ToolCall): ToolResult => failure(`Internal error (request ${c.requestID})`);

/** RFC 3339 in UTC with whole seconds. */
function rfc3339(ms: number): string {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** Microseconds since the epoch of an RFC 3339 time (NaN when it is none). */
function micros(s: string | undefined): number {
  const m = /^(.+?)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$/.exec(s ?? "");
  if (!m) return NaN;
  const ms = Date.parse(m[1]! + m[3]!);
  return ms * 1000 + Number(((m[2] ?? "") + "000000").slice(0, 6));
}

/**
 * Runs a read tool: an infrastructure failure becomes
 * "Internal error (request <id>)" (details in the log).
 */
function readTool(name: string, fn: ToolFunc): ToolFunc {
  return async (c, args) => {
    try {
      return await fn(c, args);
    } catch (e) {
      console.error(`backyard: mcp: ${name} failed (request ${c.requestID}): ${(e as Error)?.message ?? e}`);
      return internalFailure(c);
    }
  };
}

// --- backyard_docs and backyard_whoami ------------------------------------------------

async function toolDocs(c: ToolCall, a: Record<string, unknown>): Promise<ToolResult> {
  const topic = typeof a.topic === "string" && a.topic !== "" ? a.topic : "skill";
  const text = topic === "skill" ? SKILL_MD : Object.hasOwn(REFERENCES, topic) ? REFERENCES[topic]! : "";
  if (text.trim() === "") {
    console.error(`backyard: mcp: no documentation for topic ${JSON.stringify(topic)} (request ${c.requestID})`);
    return internalFailure(c);
  }
  return ok(text);
}

async function toolWhoami(c: ToolCall): Promise<ToolResult> {
  const { ctx, user: u, grant: g } = c;
  const { cfg } = ctx;
  const user: Record<string, unknown> = { id: u.id };
  if (u.email) user.email = u.email;
  user.name = u.name;
  user.admin = !u.anonymous && !!u.admin;
  user.anonymous = !!u.anonymous;
  return okJSON({
    user,
    hub_url: mcpIssuer(ctx),
    site_url_template: siteURL(ctx, "{site}"),
    can_publish: !u.anonymous || anonymousDeployAllowed(cfg),
    limits: {
      max_publish_bytes: cfg.limits.mcp_publish_bytes,
      max_files: cfg.limits.max_files,
      max_file_bytes: maxFileBytes(cfg, !!ctx.env.R2),
      max_site_bytes: cfg.limits.max_site_bytes,
      max_read_bytes: MAX_READ_BYTES,
    },
    connection: { client: g.client_name, expires_at: rfc3339(g.exp * 1000) },
  });
}

// --- the read tools ---------------------------------------------------------------------

async function toolListSites(c: ToolCall, a: Record<string, unknown>): Promise<ToolResult> {
  const { ctx } = c;
  const q = (typeof a.query === "string" ? a.query : "").trim().toLowerCase();
  // Only the sites listed to the user (SITE_LISTING) are matched and counted,
  // so total and truncated say nothing about the others.
  const all = filterListed(ctx.cfg, c.user, unwrap(await registry(ctx.env).listSites()));
  const keyed = all.map((r) => ({ r, t: micros(r.updated_at) }));
  // Most recently updated first, then by name.
  keyed.sort((x, y) => (y.t || 0) - (x.t || 0) || (x.r.name < y.r.name ? -1 : x.r.name > y.r.name ? 1 : 0));
  const owners = ctx.cfg.ownership === "owner";
  const sites: Record<string, unknown>[] = [];
  let total = 0;
  for (const { r, t } of keyed) {
    if (q && ![r.name, r.title ?? "", r.description ?? ""].some((s) => s.toLowerCase().includes(q))) continue;
    total++;
    if (sites.length === MAX_LISTED_SITES) continue;
    sites.push(siteEntry(ctx, r, t, owners));
  }
  return okJSON({ sites, total, truncated: total > sites.length });
}

/** One site of backyard_list_sites (empty fields omitted). */
function siteEntry(ctx: RequestContext, r: SiteRecord, t: number, owners: boolean): Record<string, unknown> {
  const s: Record<string, unknown> = { name: r.name, url: siteURL(ctx, r.name) };
  if (r.title) s.title = r.title;
  if (r.description) s.description = r.description;
  if (Number.isFinite(t) && t > 0) s.updated_at = rfc3339(Math.floor(t / 1000));
  if (r.updated_by) s.updated_by = r.updated_by;
  s.file_count = r.file_count;
  s.bytes = r.bytes;
  if (owners && r.owners?.length) s.owners = [...r.owners];
  return s;
}

const noSite = (site: string): ToolResult => failure(`There is no site named "${site}" on this Backyard.`);

/** The served files of site (path order), or null when there is no such site. */
async function siteFiles(ctx: RequestContext, site: string): Promise<{ path: string; size: number }[] | null> {
  const files = unwrap(await siteStub(ctx.env, site).listFiles());
  if (files.length === 0 && !unwrap(await registry(ctx.env).getSite(site))) return null;
  return files.map((f) => ({ path: f.path, size: f.size }));
}

async function toolSiteFiles(c: ToolCall, a: Record<string, unknown>): Promise<ToolResult> {
  const site = String(a.site);
  const files = await siteFiles(c.ctx, site);
  if (!files) return noSite(site);
  return okJSON({ site, url: siteURL(c.ctx, site), files: files.slice(0, MAX_LISTED_FILES), total: files.length, truncated: files.length > MAX_LISTED_FILES });
}

async function toolReadSiteFile(c: ToolCall, a: Record<string, unknown>): Promise<ToolResult> {
  const { ctx } = c;
  const site = String(a.site);
  const p = String(a.path);
  // 1. The site exists.
  if (!unwrap(await registry(ctx.env).getSite(site)) && unwrap(await siteStub(ctx.env, site).listFiles()).length === 0) return noSite(site);
  // 2. A valid path.
  const why = validatePath(p);
  if (why) return failure(`"${truncateBytes(p, 200)}" is not an allowed path: ${why}.`);
  // 3. Not a binary format.
  const binary = failure(`${p} is a binary file and cannot be read as text; link to it by URL instead.`);
  if (binaryExt(p)) return binary;
  // 4. The file exists; 5. at most 128 KiB.
  const res = await siteStub(ctx.env, site).fetch(`https://site/raw?path=${encodeURIComponent(p)}`);
  const r2Key = res.headers.get(R2_KEY_HEADER);
  if (res.status === 404 && !r2Key) return failure(`${site} has no file "${p}".`);
  if (!res.ok) throw new Error(`reading ${p}: HTTP ${res.status}`);
  const tooBig = (n: number) => failure(`${p} is ${formatBytes(n)}; files up to 128 KiB can be read.`);
  let bytes: Uint8Array;
  if (r2Key) {
    const size = Number((res.headers.get(R2_RANGE_HEADER) ?? "").split("-")[1]);
    if (!Number.isSafeInteger(size)) throw new Error(`reading ${p}: no size`);
    if (size > MAX_READ_BYTES) return tooBig(size);
    if (!ctx.env.R2) throw new Error("this file is stored in R2 but no R2 bucket is bound");
    const obj = size > 0 ? await ctx.env.R2.get(r2Key) : null;
    if (size > 0 && !obj) return failure(`${site} has no file "${p}".`);
    bytes = obj ? new Uint8Array(await obj.arrayBuffer()) : new Uint8Array(0);
  } else {
    const declared = Number(res.headers.get("Content-Length") ?? "");
    if (Number.isSafeInteger(declared) && declared > MAX_READ_BYTES) {
      await res.body?.cancel().catch(() => {});
      return tooBig(declared);
    }
    bytes = new Uint8Array(await res.arrayBuffer());
  }
  if (bytes.byteLength > MAX_READ_BYTES) return tooBig(bytes.byteLength);
  // 6. No NUL byte in the first 8 KiB.
  if (bytes.subarray(0, BINARY_SNIFF_BYTES).includes(0)) return binary;
  return ok(toValidUTF8(bytes));
}

const fatalUTF8 = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });

/** Text of bytes with each run of invalid UTF-8 replaced by one U+FFFD (Go's strings.ToValidUTF8). */
export function toValidUTF8(b: Uint8Array): string {
  try {
    return fatalUTF8.decode(b);
  } catch {
    /* repaired below */
  }
  const parts: string[] = [];
  let start = 0;
  let i = 0;
  while (i < b.length) {
    const n = utf8SeqLen(b, i);
    if (n > 0) {
      i += n;
      continue;
    }
    if (i > start) parts.push(fatalUTF8.decode(b.subarray(start, i)));
    parts.push("\ufffd");
    while (i < b.length && utf8SeqLen(b, i) === 0) i++;
    start = i;
  }
  if (start < b.length) parts.push(fatalUTF8.decode(b.subarray(start)));
  return parts.join("");
}

// --- backyard_publish -------------------------------------------------------------------------

interface PublishArgFile {
  path: string;
  content: string;
  encoding?: string;
  content_type?: string;
}

const enc = new TextEncoder();

type Base64Ctor = { fromBase64?: (s: string, o?: { alphabet?: string; lastChunkHandling?: string }) => Uint8Array };

/**
 * Decodes base64 in the standard or the URL alphabet (not both), padding
 * optional, no whitespace; null when it is not that (E7).
 */
export function decodeBase64(s: string): Uint8Array | null {
  if (/[\s]/.test(s)) return null;
  const body = s.replace(/=+$/, "");
  const pad = s.length - body.length;
  if (pad > 2 || (pad > 0 && s.length % 4 !== 0) || body.length % 4 === 1) return null;
  const std = /[+/]/.test(body);
  const url = /[-_]/.test(body);
  if ((std && url) || !/^[A-Za-z0-9+/_-]*$/.test(body)) return null;
  const native = (Uint8Array as unknown as Base64Ctor).fromBase64;
  try {
    if (typeof native === "function") return native(body, { alphabet: url ? "base64url" : "base64", lastChunkHandling: "loose" });
    const t = url ? body.replace(/-/g, "+").replace(/_/g, "/") : body;
    const bin = atob(t + "===".slice((t.length + 3) % 4));
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out;
  } catch {
    return null;
  }
}

/** The tool error of a publish refusal; E21 names the request. */
function publishFailure(c: ToolCall, e: PublishError): ToolResult {
  if (e.id === "E21") return failure(`Publishing failed: Backyard could not store the files (request ${c.requestID}). Publishing the same files again completes it.`);
  return failure(e.message);
}

async function toolPublish(c: ToolCall, a: Record<string, unknown>): Promise<ToolResult> {
  const { ctx, user, grant } = c;
  const files = (Array.isArray(a.files) ? a.files : []) as PublishArgFile[];
  const req: PublishRequest = {
    user,
    site: String(a.site),
    files: files.map((f) => ({ path: f.path, data: new Uint8Array(0), contentType: f.content_type ?? "" })),
    delete: (Array.isArray(a.delete) ? a.delete : []) as string[],
    replace: a.replace === true,
    overwrite: a.overwrite === true,
    clientName: grant.client_name,
    grantID: grant.id,
  };
  // W2, in file order.
  const textBinary: string[] = [];
  const decode = (i: number): Uint8Array | PublishError => {
    const f = files[i]!;
    if (f.encoding === "base64") {
      const data = decodeBase64(f.content);
      return data ?? { id: "E7", message: `files[${i}] (${truncateBytes(f.path, 200)}): "content" is not valid base64.` };
    }
    if (binaryExt(f.path)) textBinary.push(`${truncateBytes(f.path, 200)} was sent as text but looks binary; binary files need "encoding": "base64".`);
    return enc.encode(f.content);
  };
  const refused = checkPublish(ctx, req, decode);
  if (refused) return publishFailure(c, refused);
  // The decoded copies are the ones kept.
  for (const f of files) f.content = "";
  const out = await publishFiles(ctx, req);
  if (!out.ok) return publishFailure(c, out.error);
  const r = out.result;
  // W1 first, then W2, then the gateway's other warnings.
  const rest = [...r.warnings];
  const warnings = rest[0]?.startsWith("No index.html") ? [rest.shift()!] : [];
  warnings.push(...textBinary, ...rest);
  return okJSON({
    site: r.site,
    url: r.url,
    created: r.created,
    files: r.files,
    uploaded: r.uploaded,
    unchanged: r.unchanged,
    deleted: r.deleted,
    bytes: r.bytes,
    warnings,
  });
}

/** The tools by name; tools.json lists exactly these (test/mcp.test.ts). */
export const TOOLS: Record<string, ToolFunc> = {
  backyard_docs: readTool("backyard_docs", toolDocs),
  backyard_whoami: readTool("backyard_whoami", toolWhoami),
  backyard_list_sites: readTool("backyard_list_sites", toolListSites),
  backyard_site_files: readTool("backyard_site_files", toolSiteFiles),
  backyard_read_site_file: readTool("backyard_read_site_file", toolReadSiteFile),
  backyard_publish: async (c, a) => {
    try {
      return await toolPublish(c, a);
    } catch (e) {
      console.error(`backyard: mcp: backyard_publish failed (request ${c.requestID}): ${(e as Error)?.message ?? e}`);
      return publishFailure(c, { id: "E21", message: "" });
    }
  },
};
