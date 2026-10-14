/**
 * SiteDO: one Durable Object per site (idFromName(site)) holding everything
 * of that site in its SQLite storage (SPEC §16):
 *
 *   - blobs/chunks: content-addressed file bytes ("s:<sha256>" for deployed
 *     files, "u:<id>" for uploads) in < 2 MB chunks, or a pointer to R2;
 *   - files: the site manifest (path → sha256, size, content type);
 *   - docs: the site database with the query semantics of SPEC §5.2;
 *   - uploads: metadata of files uploaded through the API (SPEC §5.3);
 *   - the realtime websocket hub (SPEC §6) on the Hibernation API, so idle
 *     sites cost nothing; per-connection state lives in the socket attachment.
 *
 * The Worker authenticates, authorizes and validates every request before it
 * reaches this object; JSON operations are RPC methods returning Result
 * values, streaming endpoints (static files, uploads, websockets) use fetch.
 */
import { DurableObject } from "cloudflare:workers";
import { getConfig, type Config } from "./config";
import { randomHex } from "./crypto";
import { buildQuery, DOC_COLUMNS, type Query } from "./db/query";
import { formatMicros, formatMillisFixed, MicroClock, toFixedMillis } from "./db/time";
import { canonicalDoc } from "./db/values";
import { newULID } from "./db/ulid";
import type { Env } from "./env";
import { HttpError, badRequest, conflict, fail, internal, notFound, ok, tooLarge, rateLimited, unwrap, type Result, errorResponse } from "./http";
import { summarize, type User, type UserSummary } from "./identity";
import { contentTypeFor, detectContentType, isActiveContent, normalizeContentType } from "./mime";
import { boundaryOf, MultipartError, MultipartReader } from "./multipart";
import { serveObject, type Servable } from "./object";
import { sanitizeFileName } from "./paths";
import { extractTitle, TITLE_SCAN_BYTES } from "./title";

/** Chunk size of stored bytes (the row limit of DO SQLite is 2 MB). */
export const CHUNK_SIZE = 1_536 * 1024;
/** Part size of uploads streamed to R2 (R2 wants equal parts of at least 5 MiB, except the last). */
export const R2_PART_BYTES = 10 << 20;
/** Room for the multipart framing and small fields around the file of an upload. */
export const MULTIPART_OVERHEAD = 1 << 20;
/** Unreferenced or unfinished blobs older than this are garbage collected. */
const BLOB_GRACE_MS = 60 * 60 * 1000;
/**
 * Connection state whose JSON is longer than this spills to SQLite. The
 * runtime limits the serialized attachment to 16 KiB, which can be up to
 * about 2.25 times the JSON length (two-byte strings, numbers), so a state
 * below this can still be too large: save() falls back to the spill then.
 */
const ATTACHMENT_MAX = 15_000;
/**
 * Stored JSON (in UTF-16 code units) one query page carries at most. A page
 * travels to the Worker as one RPC result, which is limited to 32 MiB of
 * structured-clone data (a parsed document can take about 2.25 times its JSON
 * length: numbers), and the object has 128 MB of memory. A page always holds
 * at least one document; a page cut short says has_more.
 */
export const QUERY_PAGE_BUDGET = 8 << 20;

const ROOM_RE = /^[A-Za-z0-9_\-:.]{1,128}$/;
const CID_RE = /^c_[0-9a-f]{1,64}$/;
const COLLECTION_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

/** A file of a deploy request (validated by the Worker). */
export interface DeployFile {
  path: string;
  size: number;
  sha256: string;
  content_type: string;
}

/** Flattened document wire shape (SPEC §5.2). */
export type DocWire = Record<string, unknown> & {
  id: string;
  _meta: { created_at: string; updated_at: string; created_by: string; updated_by: string; version: number };
};

/** Stored upload metadata (FileInfo without the URL, which the Worker builds). */
export interface UploadRow {
  id: string;
  name: string;
  size: number;
  content_type: string;
  created_at: string;
  created_by: string;
  storage: "inline" | "r2";
  r2_key: string;
}

/** Result of a commit. */
export interface CommitResult {
  deleted: number;
  fileCount: number;
  bytes: number;
  title: string;
  description: string;
  /** index.html lives in R2: the Worker reads its title from this key. */
  titleR2Key: string;
  /** R2 objects no longer referenced (the Worker deletes them). */
  r2Garbage: string[];
}

/** Limits a merge checks on the resulting file set (0 = unlimited). */
export interface MergeLimits {
  maxFiles: number;
  maxSiteBytes: number;
}

/**
 * Outcome of deployMerge (SPEC §14.1, the Claude connector's publish): the
 * site after the switch, or why nothing was changed.
 */
export type MergeOutcome =
  | (CommitResult & {
      ok: true;
      /** The site had files before. */
      existed: boolean;
      /** Listed files whose content was already the path's content. */
      unchanged: number;
      /** Paths of a merge's delete list that did not exist. */
      missingDeletes: string[];
      /** index.html is part of the site afterwards. */
      hasIndex: boolean;
      /** Unix milliseconds of the switch (the manifest's updated_at). */
      committedAt: number;
    })
  /** Contents of listed files are not stored (any more): the caller stores them again. */
  | { ok: false; error: "missing"; paths: string[] }
  | MergeRefusal;

/** Why the result set of a merge breaks the limits; nothing is changed. */
export type MergeRefusal =
  /** The site would have no file. */
  | { ok: false; error: "empty" }
  /** More files than limits.maxFiles. */
  | { ok: false; error: "files"; count: number }
  /** More bytes than limits.maxSiteBytes. */
  | { ok: false; error: "bytes"; bytes: number }
  /** Paths that would be both a file and a folder of another file (sorted). */
  | { ok: false; error: "tree"; paths: string[] };

const enc = new TextEncoder();

/** The paths of a file set that are also a folder of another path in it, sorted. */
export function fileFolderConflicts(paths: Set<string>): string[] {
  const both = new Set<string>();
  for (const p of paths) {
    for (let i = p.lastIndexOf("/"); i > 0; i = p.lastIndexOf("/", i - 1)) {
      if (paths.has(p.slice(0, i))) both.add(p.slice(0, i));
    }
  }
  return [...both].sort(byBytes);
}

/** Orders strings by their UTF-8 bytes (SQLite's BINARY collation, Go's string order). */
function byBytes(a: string, b: string): number {
  if (a === b) return 0;
  const x = enc.encode(a);
  const y = enc.encode(b);
  for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i]! - y[i]!;
  return x.length - y.length;
}

/** Options of a document write (limits are enforced here, atomically). */
export interface WriteOptions {
  actor: string;
  /** Rate-limit key of the writer ("user:<id>" or "ip:<addr>"). */
  writer: string;
}

interface DocRow {
  id: string;
  data: string;
  created_at: number;
  updated_at: number;
  created_by: string;
  updated_by: string;
  version: number;
}

interface BlobRow {
  key: string;
  size: number;
  sha256: string;
  storage: string;
  r2_key: string;
  complete: number;
  created_at: number;
}

/** A file received by POST /files, before its metadata is recorded. */
interface StoredUpload {
  storage: "inline" | "r2";
  r2Key: string;
  size: number;
  filename: string;
  declaredType: string;
  /** Leading bytes, for content sniffing. */
  head: Uint8Array;
}

/** Streams bytes into the chunks of a temporary blob; publish() files them under a key. */
interface ChunkWriter {
  write(bytes: Uint8Array): void;
  publish(key: string, size: number, sha256: string): void;
  abort(): void;
}

interface ConnState {
  cid: string;
  user: User;
  summary: UserSummary;
  /** Subscription id (JSON text) → {id value, collection}. */
  subs: Record<string, { id: string | number; collection: string }>;
  /**
   * Room → presence. joined_at is fixed-width RFC 3339 with milliseconds
   * (formatMillisFixed), so it orders chronologically as a string; joined_us
   * is kept for connection state saved by older versions and is not used.
   * An object without prototype (see dict): room names are client input, and
   * "__proto__", "constructor" or "toString" must be rooms like any other,
   * never members of Object.prototype. Membership is tested with inRoom.
   */
  rooms: Record<string, RoomEntry>;
}

interface RoomEntry {
  state: Record<string, unknown>;
  joined_at: string;
  joined_us?: number;
}

/** A copy of o's own entries in an object without prototype. */
function dict<T>(o: unknown): Record<string, T> {
  const out = Object.create(null) as Record<string, T>;
  if (o && typeof o === "object") for (const [k, v] of Object.entries(o)) out[k] = v as T;
  return out;
}

/** Whether the connection is a member of room (own entries only). */
function inRoom(st: ConnState, room: string): boolean {
  return Object.hasOwn(st.rooms, room);
}

interface Bucket {
  tokens: number;
  at: number;
  lastLimited: number;
}

/** Tables of SCHEMA (an object with none of them has never stored anything). */
const TABLES = ["meta", "blobs", "chunks", "files", "docs", "uploads", "ws_spill"];

const SCHEMA = [
  `CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
  `CREATE TABLE IF NOT EXISTS blobs (
     key TEXT PRIMARY KEY, size INTEGER NOT NULL, sha256 TEXT NOT NULL DEFAULT '',
     storage TEXT NOT NULL DEFAULT 'inline', r2_key TEXT NOT NULL DEFAULT '',
     complete INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
  `CREATE TABLE IF NOT EXISTS chunks (key TEXT NOT NULL, idx INTEGER NOT NULL, data BLOB NOT NULL, PRIMARY KEY (key, idx))`,
  `CREATE TABLE IF NOT EXISTS files (
     path TEXT PRIMARY KEY, sha256 TEXT NOT NULL, size INTEGER NOT NULL,
     content_type TEXT NOT NULL, updated_at INTEGER NOT NULL)`,
  `CREATE TABLE IF NOT EXISTS docs (
     collection TEXT NOT NULL, id TEXT NOT NULL, data TEXT NOT NULL,
     created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
     created_by TEXT NOT NULL DEFAULT '', updated_by TEXT NOT NULL DEFAULT '',
     version INTEGER NOT NULL DEFAULT 1, PRIMARY KEY (collection, id))`,
  `CREATE INDEX IF NOT EXISTS docs_coll_created ON docs (collection, created_at, id)`,
  `CREATE TABLE IF NOT EXISTS uploads (
     id TEXT PRIMARY KEY, name TEXT NOT NULL, size INTEGER NOT NULL, content_type TEXT NOT NULL,
     storage TEXT NOT NULL DEFAULT 'inline', r2_key TEXT NOT NULL DEFAULT '',
     created_at INTEGER NOT NULL, created_by TEXT NOT NULL DEFAULT '')`,
  `CREATE INDEX IF NOT EXISTS uploads_created ON uploads (created_at, id)`,
  `CREATE TABLE IF NOT EXISTS ws_spill (cid TEXT PRIMARY KEY, data TEXT NOT NULL)`,
];

function utcDay(ms = Date.now()): string {
  return new Date(ms).toISOString().slice(0, 10);
}

/** Converts a thrown value into a wire error (unexpected errors are logged). */
function failure<T>(err: unknown, what: string): Result<T> {
  if (err instanceof HttpError) return fail(err);
  const msg = err instanceof Error ? err.message : String(err);
  if (msg.includes("SQLITE_FULL")) return fail(tooLarge("this site has run out of storage"));
  console.error(`backyard: site: ${what} failed: ${msg}`);
  return fail(internal(`${what} failed`));
}

export class SiteDO extends DurableObject<Env> {
  private readonly sql: SqlStorage;
  private readonly clock = new MicroClock();
  private docCount: number | null = null;
  private readonly writes = new Map<string, { day: string; n: number }>();
  private readonly conns = new WeakMap<WebSocket, ConnState>();
  private readonly buckets = new Map<string, Bucket>();
  /**
   * Whether the tables exist. They are created by the object's first write,
   * not when it starts: reads of a site name that was never deployed or used
   * (probes of random names, typos) answer "empty" and leave no storage
   * behind, so they cost none of the free plan's daily rows written or its
   * storage. Every read checks this flag; every write calls ensureSchema().
   */
  private ready = false;

  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    this.sql = ctx.storage.sql;
    const placeholders = TABLES.map(() => "?").join(", ");
    if (this.sql.exec(`SELECT 1 FROM sqlite_master WHERE type = 'table' AND name IN (${placeholders}) LIMIT 1`, ...TABLES).toArray().length > 0) {
      this.migrate();
    }
    // Answered by the runtime without waking the object (keeps hibernation cheap).
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair('{"t":"ping"}', '{"t":"pong"}'));
  }

  private migrate(): void {
    for (const stmt of SCHEMA) this.sql.exec(stmt);
    this.ready = true;
    const row = this.sql.exec("SELECT MAX(updated_at) AS m FROM docs").toArray()[0] as { m: number | null } | undefined;
    if (row?.m) this.clock.observe(row.m);
  }

  /** Creates the tables before the first write. */
  private ensureSchema(): void {
    if (!this.ready) this.migrate();
  }

  private get cfg(): Config {
    return getConfig(this.env);
  }

  // ===========================================================================
  // fetch: streaming endpoints

  override async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url);
    try {
      switch (url.pathname) {
        case "/static":
          return this.serveStatic(req, url);
        case "/upload-file":
          return this.serveUpload(req, url.searchParams.get("id") ?? "");
        case "/raw":
          return this.serveRaw(req, url.searchParams.get("path") ?? "");
        case "/blob":
          return await this.putBlob(req, url);
        case "/upload":
          return await this.uploadMultipart(req, url);
        case "/ws":
          return this.acceptWS(req);
      }
      return errorResponse(notFound("no such internal endpoint"));
    } catch (e) {
      if (e instanceof HttpError) return errorResponse(e);
      console.error(`backyard: site: ${url.pathname} failed: ${(e as Error).message}`);
      return errorResponse(internal());
    }
  }

  // --- blobs -------------------------------------------------------------

  private blob(key: string): BlobRow | undefined {
    if (!this.ready) return undefined;
    return this.sql.exec("SELECT key, size, sha256, storage, r2_key, complete, created_at FROM blobs WHERE key = ?", key).toArray()[0] as
      | BlobRow
      | undefined;
  }

  /** Reads [offset, offset+length) of an inline blob as a stream (or bytes for one chunk). */
  private openBlob(key: string, offset: number, length: number): ReadableStream<Uint8Array> | Uint8Array {
    const first = Math.floor(offset / CHUNK_SIZE);
    const last = Math.floor((offset + length - 1) / CHUNK_SIZE);
    const read = (idx: number): Uint8Array => {
      const row = this.sql.exec("SELECT data FROM chunks WHERE key = ? AND idx = ?", key, idx).toArray()[0] as { data: ArrayBuffer } | undefined;
      if (!row) throw new Error(`missing chunk ${idx} of ${key}`);
      const data = new Uint8Array(row.data);
      const start = idx === first ? offset - idx * CHUNK_SIZE : 0;
      const end = idx === last ? offset + length - idx * CHUNK_SIZE : data.length;
      return data.subarray(start, end);
    };
    if (first === last) return read(first);
    let idx = first;
    return new ReadableStream<Uint8Array>({
      pull(controller) {
        try {
          controller.enqueue(read(idx));
          idx++;
          if (idx > last) controller.close();
        } catch (e) {
          controller.error(e);
        }
      },
    });
  }

  /** Reads up to n leading bytes of an inline blob. */
  private headOfBlob(key: string, n: number): Uint8Array {
    const parts: Uint8Array[] = [];
    let total = 0;
    for (const row of this.sql.exec("SELECT data FROM chunks WHERE key = ? ORDER BY idx", key)) {
      const d = new Uint8Array(row.data as ArrayBuffer);
      parts.push(d);
      total += d.length;
      if (total >= n) break;
    }
    const out = new Uint8Array(Math.min(total, n));
    let off = 0;
    for (const p of parts) {
      const take = Math.min(p.length, out.length - off);
      out.set(p.subarray(0, take), off);
      off += take;
      if (off >= out.length) break;
    }
    return out;
  }

  /**
   * PUT /blob?key=&size=&sha256= streams the body into chunks under a temporary
   * key, verifies size (and sha256 when given) and then atomically publishes
   * it under key. An existing complete blob with that key is kept (content
   * addressing makes the bytes identical).
   */
  private async putBlob(req: Request, url: URL): Promise<Response> {
    const key = url.searchParams.get("key") ?? "";
    const size = Number(url.searchParams.get("size"));
    const want = (url.searchParams.get("sha256") ?? "").toLowerCase();
    if (!/^[su]:[A-Za-z0-9]{1,128}$/.test(key) || !Number.isSafeInteger(size) || size < 0) {
      return errorResponse(new HttpError(400, "bad_request", "invalid blob parameters"));
    }
    this.ensureSchema();
    const tmp = "t:" + randomHex(16);
    const now = Date.now();
    this.sql.exec("INSERT INTO blobs (key, size, sha256, storage, complete, created_at) VALUES (?, ?, '', 'inline', 0, ?)", tmp, size, now);
    const digest = new crypto.DigestStream("SHA-256");
    const dw = digest.getWriter();
    let total = 0;
    let idx = 0;
    let buf = new Uint8Array(CHUNK_SIZE);
    let fill = 0;
    const flush = () => {
      this.sql.exec("INSERT INTO chunks (key, idx, data) VALUES (?, ?, ?)", tmp, idx++, buf.slice(0, fill));
      fill = 0;
    };
    const cleanup = () => this.deleteBlobs([tmp]);
    try {
      if (req.body) {
        const reader = req.body.getReader();
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          total += value.byteLength;
          if (total > size) {
            await reader.cancel().catch(() => {});
            cleanup();
            return errorResponse(tooLarge(`upload is larger than the declared ${size} bytes`));
          }
          await dw.write(value);
          let off = 0;
          while (off < value.length) {
            const take = Math.min(CHUNK_SIZE - fill, value.length - off);
            buf.set(value.subarray(off, off + take), fill);
            fill += take;
            off += take;
            if (fill === CHUNK_SIZE) {
              flush();
              buf = new Uint8Array(CHUNK_SIZE);
            }
          }
        }
      }
      if (fill > 0) flush();
      await dw.close();
    } catch (e) {
      cleanup();
      throw e;
    }
    const sha = [...new Uint8Array(await digest.digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
    if (total !== size || (want && sha !== want)) {
      cleanup();
      return errorResponse(new HttpError(400, "bad_request", "upload does not match the declared size and sha256"));
    }
    // Publish atomically (no await between these statements).
    const existing = this.blob(key);
    if (existing && existing.complete) {
      cleanup();
    } else {
      this.ctx.storage.transactionSync(() => {
        if (existing) this.deleteBlobs([key]);
        this.sql.exec("UPDATE chunks SET key = ? WHERE key = ?", key, tmp);
        this.sql.exec("DELETE FROM blobs WHERE key = ?", tmp);
        this.sql.exec(
          "INSERT INTO blobs (key, size, sha256, storage, r2_key, complete, created_at) VALUES (?, ?, ?, 'inline', '', 1, ?)",
          key,
          size,
          sha,
          now,
        );
      });
    }
    return Response.json({ key, size, sha256: sha });
  }

  /** Starts a temporary inline blob (garbage-collected if never published). */
  private chunkWriter(): ChunkWriter {
    const tmp = "t:" + randomHex(16);
    this.sql.exec("INSERT INTO blobs (key, size, sha256, storage, complete, created_at) VALUES (?, 0, '', 'inline', 0, ?)", tmp, Date.now());
    const buf = new Uint8Array(CHUNK_SIZE);
    let fill = 0;
    let idx = 0;
    const flush = () => {
      if (fill === 0) return;
      this.sql.exec("INSERT INTO chunks (key, idx, data) VALUES (?, ?, ?)", tmp, idx++, buf.slice(0, fill));
      fill = 0;
    };
    return {
      write: (bytes) => {
        for (let off = 0; off < bytes.length; ) {
          const take = Math.min(CHUNK_SIZE - fill, bytes.length - off);
          buf.set(bytes.subarray(off, off + take), fill);
          fill += take;
          off += take;
          if (fill === CHUNK_SIZE) flush();
        }
      },
      publish: (key, size, sha256) => {
        flush();
        this.ctx.storage.transactionSync(() => {
          this.deleteBlobs([key]);
          this.sql.exec("UPDATE chunks SET key = ? WHERE key = ?", key, tmp);
          this.sql.exec("DELETE FROM blobs WHERE key = ?", tmp);
          this.sql.exec(
            "INSERT INTO blobs (key, size, sha256, storage, r2_key, complete, created_at) VALUES (?, ?, ?, 'inline', '', 1, ?)",
            key,
            size,
            sha256,
            Date.now(),
          );
        });
      },
      abort: () => {
        this.deleteBlobs([tmp]);
      },
    };
  }

  /**
   * POST /upload?site=&limit=&inline_max=&r2= — the body of POST /files (SPEC §5.3): exactly
   * one "file" field and an optional "name" field. The multipart body is
   * parsed here as a stream rather than in the Worker (a Durable Object has
   * seconds of CPU per request, a free-plan Worker 10 ms) and never held in
   * memory as a whole: the file goes to SQLite chunks, or — with R2 bound and
   * above FILE_INLINE_MAX — to R2 one part at a time.
   */
  private async uploadMultipart(req: Request, url: URL): Promise<Response> {
    // The Worker decides the limits (its configuration is authoritative).
    const site = url.searchParams.get("site") ?? "";
    const limit = Number(url.searchParams.get("limit"));
    const inlineMax = Number(url.searchParams.get("inline_max"));
    const bucket = url.searchParams.get("r2") === "1" ? this.env.R2 : undefined;
    let createdBy = "";
    try {
      createdBy = decodeURIComponent(req.headers.get("x-backyard-created-by") ?? "");
    } catch {
      /* rejected below */
    }
    if (!site || !Number.isSafeInteger(limit) || limit < 0 || !Number.isSafeInteger(inlineMax) || inlineMax < 0 || !createdBy) {
      return errorResponse(badRequest("invalid upload parameters"));
    }
    let boundary: string;
    try {
      boundary = boundaryOf(req.headers.get("Content-Type") ?? "");
    } catch (e) {
      return errorResponse(badRequest(`invalid multipart body: ${(e as Error).message}`));
    }
    if (!req.body) return errorResponse(badRequest('missing "file" field'));
    this.ensureSchema();
    // The whole body is bounded too (fields other than "file" are skipped, not stored).
    let total = 0;
    const bounded = req.body.pipeThrough(
      new TransformStream<Uint8Array, Uint8Array>({
        transform(chunk, controller) {
          total += chunk.byteLength;
          if (total > limit + MULTIPART_OVERHEAD) controller.error(tooLarge(`file exceeds the maximum upload size of ${limit} bytes`));
          else controller.enqueue(chunk);
        },
      }),
    );
    const mp = new MultipartReader(bounded, boundary);
    const id = newULID();
    let nameField: string | null = null;
    let stored: StoredUpload | null = null;
    try {
      for (let part = await mp.next(); part; part = await mp.next()) {
        if (part.name === "name" && part.filename === null) {
          nameField = await mp.text(4096);
          continue;
        }
        if (part.name !== "file") continue; // other fields are ignored (skipped by next())
        if (part.filename === null) throw badRequest('the "file" field must be a file');
        if (stored) throw badRequest("only one file per request");
        const key = `uploads/${site}/${id}/${sanitizeFileName((nameField ?? "").trim() || part.filename || "file")}`;
        stored = await this.receiveFile(mp, { id, r2Key: key, limit, inlineMax, bucket }, part.filename, part.contentType);
      }
    } catch (e) {
      await mp.cancel();
      if (stored) this.discardUpload(id, stored);
      if (e instanceof HttpError) return errorResponse(e);
      if (e instanceof MultipartError) return errorResponse(badRequest(`invalid multipart body: ${e.message}`));
      throw e;
    }
    if (!stored) return errorResponse(badRequest('missing "file" field'));
    const name = sanitizeFileName((nameField ?? "").trim() || stored.filename || "file");
    let ct = normalizeContentType(stored.declaredType);
    if (!ct || ct === "application/octet-stream") ct = detectContentType(name, stored.head);
    try {
      this.sql.exec(
        "INSERT INTO uploads (id, name, size, content_type, storage, r2_key, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
        id,
        name,
        stored.size,
        ct,
        stored.storage,
        stored.r2Key,
        this.clock.now(),
        createdBy,
      );
    } catch (e) {
      this.discardUpload(id, stored);
      throw e;
    }
    return Response.json(this.uploadRow(id), { status: 201 });
  }

  /** Receives the body of the current "file" part (see uploadMultipart). */
  private async receiveFile(
    mp: MultipartReader,
    o: { id: string; r2Key: string; limit: number; inlineMax: number; bucket: R2Bucket | undefined },
    filename: string,
    declaredType: string,
  ): Promise<StoredUpload> {
    const { id, r2Key, limit, inlineMax, bucket } = o;
    const head = new Uint8Array(512);
    let headLen = 0;
    let size = 0;
    const count = (chunk: Uint8Array) => {
      size += chunk.length;
      if (size > limit) throw tooLarge(`file exceeds the maximum upload size of ${limit} bytes`);
      if (headLen < head.length) {
        const take = Math.min(head.length - headLen, chunk.length);
        head.set(chunk.subarray(0, take), headLen);
        headLen += take;
      }
    };
    const result = (storage: "inline" | "r2"): StoredUpload => ({
      storage,
      r2Key: storage === "r2" ? r2Key : "",
      size,
      filename,
      declaredType,
      head: head.slice(0, headLen),
    });

    if (!bucket) {
      const w = this.chunkWriter();
      try {
        await mp.body((chunk) => {
          count(chunk);
          w.write(chunk);
        });
        w.publish("u:" + id, size, "");
      } catch (e) {
        w.abort();
        throw e;
      }
      return result("inline");
    }

    // With R2: collect one part in memory; a file that ends within it and
    // within FILE_INLINE_MAX is stored inline, a bigger one goes to R2 (one put,
    // or a multipart upload of equal parts).
    const partSize = Math.max(R2_PART_BYTES, inlineMax);
    const parts: R2UploadedPart[] = [];
    const mpu: { upload: R2MultipartUpload | null } = { upload: null };
    let pending = new Uint8Array(Math.min(partSize, Math.max(limit, 1)));
    let fill = 0;
    try {
      await mp.body(async (chunk) => {
        count(chunk);
        for (let off = 0; off < chunk.length; ) {
          const take = Math.min(pending.length - fill, chunk.length - off);
          pending.set(chunk.subarray(off, off + take), fill);
          fill += take;
          off += take;
          if (fill === pending.length && off < chunk.length) {
            // A full part with more to come (the size limit keeps a file of exactly one part out of here).
            mpu.upload ??= await bucket.createMultipartUpload(r2Key);
            parts.push(await mpu.upload.uploadPart(parts.length + 1, pending));
            pending = new Uint8Array(partSize);
            fill = 0;
          }
        }
      });
      if (mpu.upload) {
        if (fill > 0) parts.push(await mpu.upload.uploadPart(parts.length + 1, pending.subarray(0, fill)));
        await mpu.upload.complete(parts);
        return result("r2");
      }
      if (size <= inlineMax) {
        const w = this.chunkWriter();
        w.write(pending.subarray(0, fill));
        w.publish("u:" + id, size, "");
        return result("inline");
      }
      await bucket.put(r2Key, pending.subarray(0, fill));
      return result("r2");
    } catch (e) {
      if (mpu.upload) await mpu.upload.abort().catch(() => {});
      throw e;
    }
  }

  /** Removes the bytes of an upload whose metadata could not be recorded. */
  private discardUpload(id: string, stored: StoredUpload): void {
    if (stored.storage === "inline") this.deleteBlobs(["u:" + id]);
    else if (this.env.R2) this.ctx.waitUntil(this.env.R2.delete(stored.r2Key).catch(() => {}));
  }

  /** Deletes blobs and their chunks; returns R2 keys of R2-backed ones. */
  private deleteBlobs(keys: string[]): string[] {
    const r2: string[] = [];
    for (const k of keys) {
      const b = this.blob(k);
      if (b?.storage === "r2" && b.r2_key) r2.push(b.r2_key);
      this.sql.exec("DELETE FROM chunks WHERE key = ?", k);
      this.sql.exec("DELETE FROM blobs WHERE key = ?", k);
    }
    return r2;
  }

  /**
   * Whether the content-addressed blob key is stored completely with the given
   * size (a deploy upload of content the site already holds under another
   * path, or an upload retried by the CLI, can then skip storing it again).
   */
  async hasBlob(key: string, size: number): Promise<Result<boolean>> {
    try {
      const b = this.blob(key);
      return ok(!!b && b.complete === 1 && b.size === size);
    } catch (e) {
      return failure(e, "reading the blob index");
    }
  }

  /**
   * Records an object the Worker stored in R2 (deploy files). stored is false
   * when the content was already present: the caller then deletes its copy
   * (R2 keys are unique per upload, so garbage collection of one copy can
   * never delete another).
   */
  async registerR2Blob(key: string, size: number, sha256: string, r2Key: string): Promise<Result<{ stored: boolean }>> {
    try {
      this.ensureSchema();
      const existing = this.blob(key);
      if (existing?.complete) return ok({ stored: false });
      let garbage: string[] = [];
      this.ctx.storage.transactionSync(() => {
        if (existing) garbage = this.deleteBlobs([key]);
        this.sql.exec(
          "INSERT INTO blobs (key, size, sha256, storage, r2_key, complete, created_at) VALUES (?, ?, ?, 'r2', ?, 1, ?)",
          key,
          size,
          sha256.toLowerCase(),
          r2Key,
          Date.now(),
        );
      });
      if (garbage.length && this.env.R2) this.ctx.waitUntil(this.env.R2.delete(garbage).catch(() => {}));
      return ok({ stored: true });
    } catch (e) {
      return failure(e, "registering an R2 object");
    }
  }

  // --- static files (SPEC §3) --------------------------------------------

  private fileObject(path: string): Servable | null {
    if (!this.ready) return null;
    const row = this.sql.exec("SELECT path, sha256, size, content_type, updated_at FROM files WHERE path = ?", path).toArray()[0] as
      | { path: string; sha256: string; size: number; content_type: string; updated_at: number }
      | undefined;
    if (!row) return null;
    const key = "s:" + row.sha256;
    const b = this.blob(key);
    if (!b || !b.complete) return null;
    const name = path.slice(path.lastIndexOf("/") + 1);
    const obj: Servable = {
      name,
      size: row.size,
      etag: `"${row.sha256.slice(0, 32)}"`,
      lastModified: row.updated_at,
      contentType: contentTypeFor(name, row.content_type),
    };
    if (b.storage === "r2") obj.r2Key = b.r2_key;
    else obj.open = (offset, length) => this.openBlob(key, offset, length);
    return obj;
  }

  private serveStatic(req: Request, url: URL): Response {
    const p = url.searchParams.get("path") ?? "/";
    const loc = url.searchParams.get("loc") ?? p;
    const query = url.searchParams.get("q") ?? "";
    const rel = p.replace(/^\/+/, "");
    let obj: Servable | null;
    if (p === "/" || p.endsWith("/")) {
      obj = this.fileObject(rel + "index.html");
    } else {
      obj = this.fileObject(rel);
      if (!obj) {
        if (this.fileObject(rel + "/index.html")) {
          return new Response(null, { status: 301, headers: { Location: loc + "/" + query, "Cache-Control": "no-store" } });
        }
        obj = this.fileObject(rel + ".html");
      }
    }
    if (obj) return serveObject(req, obj);
    const spa = this.fileObject("200.html");
    if (spa) return serveObject(req, spa);
    const nf = this.fileObject("404.html");
    if (nf) return serveObject(req, nf, { status: 404, cacheControl: "no-store" });
    const any = this.ready && this.sql.exec("SELECT 1 FROM files LIMIT 1").toArray().length > 0;
    return new Response(null, { status: 404, headers: { "x-backyard-not-found": any ? "page" : "site" } });
  }

  /** GET /raw?path= — a deployed file as-is (gateway downloads). */
  private serveRaw(req: Request, path: string): Response {
    const obj = this.fileObject(path);
    if (!obj) return errorResponse(notFound(`file ${JSON.stringify(path.slice(0, 200))} not found`));
    obj.contentType = this.sql.exec("SELECT content_type FROM files WHERE path = ?", path).one().content_type as string;
    return serveObject(req, obj, {
      cacheControl: "no-store",
      headers: { "Content-Security-Policy": "sandbox; default-src 'none'", "Content-Disposition": "attachment" },
    });
  }

  // --- deploys (SPEC §7.3) -----------------------------------------------

  /** Paths whose current content already has the given sha256 and size. */
  async deployPlan(files: DeployFile[]): Promise<Result<{ unchanged: string[] }>> {
    try {
      const unchanged: string[] = [];
      if (!this.ready) return ok({ unchanged });
      for (const f of files) {
        const row = this.sql.exec("SELECT sha256, size FROM files WHERE path = ?", f.path).toArray()[0] as
          | { sha256: string; size: number }
          | undefined;
        if (!row || row.sha256 !== f.sha256 || row.size !== f.size) continue;
        const b = this.blob("s:" + f.sha256);
        if (b?.complete && b.size === f.size) unchanged.push(f.path);
      }
      return ok({ unchanged });
    } catch (e) {
      return failure(e, "planning the deploy");
    }
  }

  /**
   * Commits a deploy: verifies that every listed file's content is stored,
   * replaces the manifest in one transaction (so a site is never half
   * replaced) and garbage-collects content no longer referenced.
   */
  async deployCommit(files: DeployFile[]): Promise<Result<CommitResult>> {
    try {
      const problems: string[] = [];
      this.ensureSchema();
      for (const f of files) {
        const b = this.blob("s:" + f.sha256);
        if (!b || !b.complete) problems.push(`${f.path.slice(0, 200)} (missing)`);
        else if (b.size !== f.size) problems.push(`${f.path.slice(0, 200)} (${b.size} bytes in storage, expected ${f.size})`);
      }
      if (problems.length) {
        const n = problems.length;
        const list = n > 10 ? [...problems.slice(0, 10), `… and ${n - 10} more`] : problems;
        return fail(conflict(`${n} file(s) were not uploaded correctly: ${list.join(", ")}`));
      }
      const now = Date.now();
      let deleted = 0;
      let r2Garbage: string[] = [];
      this.ctx.storage.transactionSync(() => {
        const listed = new Set(files.map((f) => f.path));
        const old = this.sql.exec("SELECT path, sha256 FROM files").toArray() as { path: string; sha256: string }[];
        const released = new Set<string>();
        for (const o of old) {
          if (!listed.has(o.path)) {
            this.sql.exec("DELETE FROM files WHERE path = ?", o.path);
            deleted++;
            released.add(o.sha256);
          }
        }
        const oldSha = new Map(old.map((o) => [o.path, o.sha256]));
        for (const f of files) {
          const prev = oldSha.get(f.path);
          if (prev === f.sha256) {
            this.sql.exec("UPDATE files SET size = ?, content_type = ? WHERE path = ?", f.size, f.content_type, f.path);
            continue;
          }
          if (prev) released.add(prev);
          this.sql.exec(
            "INSERT OR REPLACE INTO files (path, sha256, size, content_type, updated_at) VALUES (?, ?, ?, ?, ?)",
            f.path,
            f.sha256,
            f.size,
            f.content_type,
            now,
          );
        }
        this.sql.exec("INSERT OR REPLACE INTO meta (k, v) VALUES ('updated_at', ?)", String(now));
        r2Garbage = this.collectGarbage(now, released);
      });
      const stats = this.stats();
      return ok({ deleted, fileCount: stats.fileCount, bytes: stats.bytes, ...this.titleInfo(), r2Garbage });
    } catch (e) {
      return failure(e, "committing the deploy");
    }
  }

  /**
   * The result set of a connector publish (SPEC §14.1) against the current
   * manifest: (current files − deletes − changed paths) ∪ changes, or exactly
   * changes with replace; the paths it drops, the deletes that do not exist,
   * and the refusal of limits when it breaks one. Reads only.
   */
  private mergeSet(changes: DeployFile[], deletes: string[], replace: boolean, limits: MergeLimits) {
    const base = this.ready ? (this.sql.exec("SELECT path, sha256, size FROM files").toArray() as { path: string; sha256: string; size: number }[]) : [];
    const current = new Map(base.map((r) => [r.path, r]));
    const listed = new Set(changes.map((f) => f.path));
    const result = new Map<string, number>();
    const removed: string[] = [];
    const missingDeletes: string[] = [];
    if (replace) {
      for (const r of base) if (!listed.has(r.path)) removed.push(r.path);
    } else {
      const gone = new Set<string>();
      for (const p of deletes) {
        if (gone.has(p)) continue;
        gone.add(p);
        if (!current.has(p)) missingDeletes.push(p);
        else if (!listed.has(p)) removed.push(p);
      }
      for (const r of base) if (!listed.has(r.path) && !gone.has(r.path)) result.set(r.path, r.size);
    }
    for (const f of changes) result.set(f.path, f.size);
    let refusal: MergeRefusal | null = null;
    let total = 0;
    for (const n of result.values()) total += n;
    if (result.size === 0) refusal = { ok: false, error: "empty" };
    else if (limits.maxFiles > 0 && result.size > limits.maxFiles) refusal = { ok: false, error: "files", count: result.size };
    else if (limits.maxSiteBytes > 0 && total > limits.maxSiteBytes) refusal = { ok: false, error: "bytes", bytes: total };
    else {
      const tree = fileFolderConflicts(new Set(result.keys()));
      if (tree.length) refusal = { ok: false, error: "tree", paths: tree };
    }
    return { base, current, result, removed, missingDeletes, refusal };
  }

  /**
   * Plans a connector publish without changing anything: the listed paths
   * whose current content already has the given sha256 and size (they need no
   * upload), and the refusal of the merge's limits as the site is now, so a
   * publish that cannot succeed uploads nothing. deployMerge checks again.
   */
  async mergePlan(changes: DeployFile[], deletes: string[], replace: boolean, limits: MergeLimits): Promise<Result<{ unchanged: string[]; refusal: MergeRefusal | null }>> {
    try {
      const plan = unwrap(await this.deployPlan(changes));
      return ok({ unchanged: plan.unchanged, refusal: this.mergeSet(changes, deletes, replace, limits).refusal });
    } catch (e) {
      return failure(e, "planning the publish");
    }
  }

  /**
   * Merges a publish of the Claude connector into the site (SPEC §14.1) in
   * one transaction, so concurrent merges never lose each other's files: the
   * result set is (current files − deletes − changed paths) ∪ changes, or
   * exactly changes with replace. It verifies that every change's content is
   * stored, checks the result against limits (nothing changes on a refusal),
   * switches the manifest (an unchanged file keeps its updated_at) and
   * garbage-collects content no longer referenced. A path of deletes that
   * does not exist is reported, not an error; the Worker has already refused
   * a path that is both in changes and in deletes.
   */
  async deployMerge(changes: DeployFile[], deletes: string[], replace: boolean, limits: MergeLimits): Promise<Result<MergeOutcome>> {
    try {
      this.ensureSchema();
      const missing: string[] = [];
      for (const f of changes) {
        const b = this.blob("s:" + f.sha256);
        if (!b || !b.complete || b.size !== f.size) missing.push(f.path);
      }
      if (missing.length) return ok({ ok: false, error: "missing", paths: missing });
      const now = Date.now();
      // No await from here on: the merge sees the manifest as it is and no
      // other request of this site runs in between.
      return ok(
        this.ctx.storage.transactionSync((): MergeOutcome => {
          const m = this.mergeSet(changes, deletes, replace, limits);
          if (m.refusal) return m.refusal;
          const released = new Set<string>();
          for (const p of m.removed) {
            this.sql.exec("DELETE FROM files WHERE path = ?", p);
            released.add(m.current.get(p)!.sha256);
          }
          let unchanged = 0;
          for (const f of changes) {
            const prev = m.current.get(f.path);
            if (prev && prev.sha256 === f.sha256) {
              this.sql.exec("UPDATE files SET size = ?, content_type = ? WHERE path = ?", f.size, f.content_type, f.path);
              unchanged++;
              continue;
            }
            if (prev) released.add(prev.sha256);
            this.sql.exec(
              "INSERT OR REPLACE INTO files (path, sha256, size, content_type, updated_at) VALUES (?, ?, ?, ?, ?)",
              f.path,
              f.sha256,
              f.size,
              f.content_type,
              now,
            );
          }
          this.sql.exec("INSERT OR REPLACE INTO meta (k, v) VALUES ('updated_at', ?)", String(now));
          const r2Garbage = this.collectGarbage(now, released);
          const stats = this.stats();
          return {
            ok: true,
            existed: m.base.length > 0,
            deleted: m.removed.length,
            unchanged,
            missingDeletes: m.missingDeletes,
            hasIndex: m.result.has("index.html"),
            committedAt: now,
            fileCount: stats.fileCount,
            bytes: stats.bytes,
            ...this.titleInfo(),
            r2Garbage,
          };
        }),
      );
    } catch (e) {
      return failure(e, "merging the publish");
    }
  }

  /**
   * Deletes site content blobs that no file references: right away for
   * content released by this commit, after BLOB_GRACE_MS for content uploaded
   * by deploys that never committed. Unfinished uploads are dropped too.
   */
  private collectGarbage(now: number, released: Set<string>): string[] {
    const cutoff = now - BLOB_GRACE_MS;
    const rows = this.sql
      .exec(
        `SELECT key, created_at, complete FROM blobs
         WHERE (key LIKE 's:%' AND substr(key, 3) NOT IN (SELECT sha256 FROM files))
            OR (key LIKE 't:%' AND created_at < ?)`,
        cutoff,
      )
      .toArray() as { key: string; created_at: number; complete: number }[];
    const doomed = rows.filter((r) => r.key.startsWith("t:") || released.has(r.key.slice(2)) || r.created_at < cutoff).map((r) => r.key);
    return this.deleteBlobs(doomed);
  }

  private stats(): { fileCount: number; bytes: number } {
    const r = this.sql.exec("SELECT COUNT(*) AS n, COALESCE(SUM(size), 0) AS b FROM files").one() as { n: number; b: number };
    return { fileCount: r.n, bytes: r.b };
  }

  private titleInfo(): { title: string; description: string; titleR2Key: string } {
    const row = this.sql.exec("SELECT sha256 FROM files WHERE path = 'index.html'").toArray()[0] as { sha256: string } | undefined;
    if (!row) return { title: "", description: "", titleR2Key: "" };
    const b = this.blob("s:" + row.sha256);
    if (!b) return { title: "", description: "", titleR2Key: "" };
    if (b.storage === "r2") return { title: "", description: "", titleR2Key: b.r2_key };
    return { ...extractTitle(this.headOfBlob(b.key, TITLE_SCAN_BYTES)), titleR2Key: "" };
  }

  /** File count, bytes, title and description of the current manifest (refresh). */
  async siteInfo(): Promise<Result<{ fileCount: number; bytes: number; title: string; description: string; titleR2Key: string; updatedAt: number }>> {
    try {
      if (!this.ready) return ok({ fileCount: 0, bytes: 0, title: "", description: "", titleR2Key: "", updatedAt: 0 });
      const updated = this.sql.exec("SELECT v FROM meta WHERE k = 'updated_at'").toArray()[0] as { v: string } | undefined;
      return ok({ ...this.stats(), ...this.titleInfo(), updatedAt: updated ? Number(updated.v) : 0 });
    } catch (e) {
      return failure(e, "reading the site");
    }
  }

  /** The manifest: GET G/sites/{site}/files. */
  async listFiles(): Promise<Result<{ path: string; size: number; sha256: string; updated_at: string }[]>> {
    try {
      if (!this.ready) return ok([]);
      const rows = this.sql.exec("SELECT path, size, sha256, updated_at FROM files ORDER BY path").toArray() as {
        path: string;
        size: number;
        sha256: string;
        updated_at: number;
      }[];
      return ok(rows.map((r) => ({ path: r.path, size: r.size, sha256: r.sha256, updated_at: formatMicros(r.updated_at * 1000) })));
    } catch (e) {
      return failure(e, "listing files");
    }
  }

  /** Deletes everything of this site; returns the R2 keys to delete. */
  async destroy(): Promise<Result<{ r2Keys: string[] }>> {
    try {
      const r2Keys = this.ready
        ? [
            ...(this.sql.exec("SELECT r2_key FROM blobs WHERE storage = 'r2' AND r2_key <> ''").toArray() as { r2_key: string }[]).map((r) => r.r2_key),
            ...(this.sql.exec("SELECT r2_key FROM uploads WHERE storage = 'r2' AND r2_key <> ''").toArray() as { r2_key: string }[]).map((r) => r.r2_key),
          ]
        : [];
      for (const ws of this.ctx.getWebSockets()) {
        try {
          ws.close(1001, "site deleted");
        } catch {
          /* already closed */
        }
      }
      await this.ctx.storage.deleteAll();
      this.docCount = null;
      this.writes.clear();
      // The tables are created again by the next write: a deleted site keeps no storage.
      this.ready = false;
      return ok({ r2Keys });
    } catch (e) {
      return failure(e, "deleting the site");
    }
  }

  // ===========================================================================
  // Database (SPEC §5.2)

  private docWire(r: DocRow): DocWire {
    const data = JSON.parse(r.data) as Record<string, unknown>;
    return {
      id: r.id,
      ...data,
      _meta: {
        created_at: formatMicros(r.created_at),
        updated_at: formatMicros(r.updated_at),
        created_by: r.created_by,
        updated_by: r.updated_by,
        version: r.version,
      },
    } as DocWire;
  }

  private docRow(collection: string, id: string): DocRow | undefined {
    if (!this.ready) return undefined;
    return this.sql.exec(`SELECT ${DOC_COLUMNS} FROM docs WHERE collection = ? AND id = ?`, collection, id).toArray()[0] as DocRow | undefined;
  }

  private countDocs(): number {
    if (this.docCount === null) {
      this.docCount = (this.sql.exec("SELECT COUNT(*) AS n FROM docs").one() as { n: number }).n;
    }
    return this.docCount;
  }

  /** Applies the per-writer daily write cap (counted in memory). */
  private chargeWrite(writer: string): void {
    const max = this.cfg.limits.db_writes_per_day;
    if (!max) return;
    const day = utcDay();
    let c = this.writes.get(writer);
    if (!c || c.day !== day) {
      c = { day, n: 0 };
      if (this.writes.size > 10_000) this.writes.clear();
      this.writes.set(writer, c);
    }
    if (c.n >= max) throw rateLimited(`daily database write limit reached (${max} per day); it resets at midnight UTC`, secondsToMidnight());
    c.n++;
  }

  private checkQuota(): void {
    const max = this.cfg.limits.max_docs_per_site;
    if (max > 0 && this.countDocs() >= max) {
      throw tooLarge(`this site has reached its document quota (${max} documents)`);
    }
  }

  /**
   * Validates a document body (canonicalDoc: nesting depth, U+0000, finite
   * numbers; 400 otherwise) and the size limit; returns the stored JSON text.
   */
  private encodeDoc(data: Record<string, unknown>): string {
    const text = JSON.stringify(canonicalDoc(data));
    const max = this.cfg.limits.max_doc_bytes;
    if (max > 0 && new TextEncoder().encode(text).length > max) {
      throw tooLarge(`document exceeds the maximum size of ${max} bytes`);
    }
    return text;
  }

  /** GET /db: non-system collections with documents, sorted. */
  async dbCollections(): Promise<Result<string[]>> {
    try {
      if (!this.ready) return ok([]);
      const rows = this.sql.exec("SELECT DISTINCT collection FROM docs ORDER BY collection").toArray() as { collection: string }[];
      return ok(rows.map((r) => r.collection).filter((c) => !c.startsWith("_")));
    } catch (e) {
      return failure(e, "listing collections");
    }
  }

  /** POST /db/{c}: creates a document (409 if the id exists). */
  async dbCreate(collection: string, id: string, data: Record<string, unknown>, opts: WriteOptions): Promise<Result<DocWire>> {
    try {
      const text = this.encodeDoc(data);
      this.ensureSchema();
      if (id && this.docRow(collection, id)) return fail(conflict("a document with this id already exists"));
      this.checkQuota();
      this.chargeWrite(opts.writer);
      const docID = id || newULID();
      const now = this.clock.now();
      this.sql.exec(
        "INSERT INTO docs (collection, id, data, created_at, updated_at, created_by, updated_by, version) VALUES (?, ?, ?, ?, ?, ?, ?, 1)",
        collection,
        docID,
        text,
        now,
        now,
        opts.actor,
        opts.actor,
      );
      if (this.docCount !== null) this.docCount++;
      const doc = this.docWire(this.docRow(collection, docID)!);
      this.broadcastDB(collection, "create", doc, docID);
      return ok(doc);
    } catch (e) {
      return failure(e, "creating the document");
    }
  }

  /** GET /db/{c}/{id}. */
  async dbGet(collection: string, id: string): Promise<Result<DocWire>> {
    try {
      const r = this.docRow(collection, id);
      return r ? ok(this.docWire(r)) : fail(notFound("document not found"));
    } catch (e) {
      return failure(e, "reading the document");
    }
  }

  /** PUT /db/{c}/{id}: creates or replaces (version + 1, creation kept). */
  async dbPut(collection: string, id: string, data: Record<string, unknown>, opts: WriteOptions): Promise<Result<{ doc: DocWire; created: boolean }>> {
    try {
      const text = this.encodeDoc(data);
      this.ensureSchema();
      const cur = this.docRow(collection, id);
      if (!cur) this.checkQuota();
      this.chargeWrite(opts.writer);
      const now = this.clock.now();
      if (cur) {
        this.sql.exec(
          "UPDATE docs SET data = ?, updated_at = ?, updated_by = ?, version = version + 1 WHERE collection = ? AND id = ?",
          text,
          now,
          opts.actor,
          collection,
          id,
        );
      } else {
        this.sql.exec(
          "INSERT INTO docs (collection, id, data, created_at, updated_at, created_by, updated_by, version) VALUES (?, ?, ?, ?, ?, ?, ?, 1)",
          collection,
          id,
          text,
          now,
          now,
          opts.actor,
          opts.actor,
        );
        if (this.docCount !== null) this.docCount++;
      }
      const doc = this.docWire(this.docRow(collection, id)!);
      this.broadcastDB(collection, cur ? "update" : "create", doc, id);
      return ok({ doc, created: !cur });
    } catch (e) {
      return failure(e, "writing the document");
    }
  }

  /** PATCH /db/{c}/{id}: shallow merge, null deletes a key; 404 if missing. */
  async dbPatch(collection: string, id: string, patch: Record<string, unknown>, opts: WriteOptions): Promise<Result<DocWire>> {
    try {
      const cur = this.docRow(collection, id);
      if (!cur) return fail(notFound("document not found"));
      const merged = JSON.parse(cur.data) as Record<string, unknown>;
      for (const [k, v] of Object.entries(canonicalDoc(patch))) {
        if (v === null) delete merged[k];
        // Defined, not assigned: a "__proto__" key is data, not the prototype.
        else Object.defineProperty(merged, k, { value: v, enumerable: true, writable: true, configurable: true });
      }
      const text = this.encodeDoc(merged);
      this.chargeWrite(opts.writer);
      const now = this.clock.now();
      this.sql.exec(
        "UPDATE docs SET data = ?, updated_at = ?, updated_by = ?, version = version + 1 WHERE collection = ? AND id = ?",
        text,
        now,
        opts.actor,
        collection,
        id,
      );
      const doc = this.docWire(this.docRow(collection, id)!);
      this.broadcastDB(collection, "update", doc, id);
      return ok(doc);
    } catch (e) {
      return failure(e, "updating the document");
    }
  }

  /** DELETE /db/{c}/{id}: reports whether a document was deleted. */
  async dbDelete(collection: string, id: string, opts: WriteOptions): Promise<Result<boolean>> {
    try {
      this.chargeWrite(opts.writer);
      if (!this.ready) return ok(false);
      const res = this.sql.exec("DELETE FROM docs WHERE collection = ? AND id = ?", collection, id);
      const deleted = res.rowsWritten > 0;
      if (deleted) {
        if (this.docCount !== null) this.docCount = Math.max(0, this.docCount - 1);
        this.broadcastDB(collection, "delete", null, id);
      }
      return ok(deleted);
    } catch (e) {
      return failure(e, "deleting the document");
    }
  }

  /** Runs a normalized query (SPEC §5.2). */
  async dbQuery(collection: string, q: Query): Promise<Result<{ docs: DocWire[]; has_more: boolean }>> {
    try {
      const { sql, params } = buildQuery(collection, q);
      const docs: DocWire[] = [];
      if (!this.ready) return ok({ docs, has_more: false });
      // Rows are read one at a time and the page stops at its byte budget
      // (QUERY_PAGE_BUDGET): limit × max_doc_bytes can far exceed what one
      // RPC result and the object's memory can hold.
      let used = 0;
      let hasMore = false;
      for (const row of this.sql.exec(sql, ...params)) {
        const r = row as unknown as DocRow;
        if (docs.length >= q.limit || (docs.length > 0 && used + r.data.length > QUERY_PAGE_BUDGET)) {
          hasMore = true;
          break;
        }
        used += r.data.length;
        docs.push(this.docWire(r));
      }
      return ok({ docs, has_more: hasMore });
    } catch (e) {
      return failure(e, "querying documents");
    }
  }

  // ===========================================================================
  // Uploads (SPEC §5.3)

  private uploadRow(id: string): UploadRow | null {
    if (!this.ready) return null;
    const r = this.sql.exec("SELECT id, name, size, content_type, storage, r2_key, created_at, created_by FROM uploads WHERE id = ?", id).toArray()[0] as
      | (Omit<UploadRow, "created_at"> & { created_at: number })
      | undefined;
    return r ? { ...r, created_at: formatMicros(r.created_at) } : null;
  }

  /**
   * Records an upload whose bytes are stored (inline blob "u:<id>" written
   * through /blob, or an R2 object).
   */
  async recordUpload(u: { id: string; name: string; size: number; content_type: string; created_by: string; r2_key?: string }): Promise<Result<UploadRow>> {
    try {
      this.ensureSchema();
      if (u.r2_key) {
        this.sql.exec(
          "INSERT INTO uploads (id, name, size, content_type, storage, r2_key, created_at, created_by) VALUES (?, ?, ?, ?, 'r2', ?, ?, ?)",
          u.id,
          u.name,
          u.size,
          u.content_type,
          u.r2_key,
          this.clock.now(),
          u.created_by,
        );
      } else {
        const b = this.blob("u:" + u.id);
        if (!b?.complete) return fail(internal("upload content is missing"));
        this.sql.exec(
          "INSERT INTO uploads (id, name, size, content_type, storage, r2_key, created_at, created_by) VALUES (?, ?, ?, ?, 'inline', '', ?, ?)",
          u.id,
          u.name,
          b.size,
          u.content_type,
          this.clock.now(),
          u.created_by,
        );
      }
      return ok(this.uploadRow(u.id)!);
    } catch (e) {
      return failure(e, "recording the upload");
    }
  }

  /** GET /files: newest first. */
  async listUploads(limit: number, offset: number): Promise<Result<{ files: UploadRow[]; has_more: boolean }>> {
    try {
      if (!this.ready) return ok({ files: [], has_more: false });
      const rows = this.sql
        .exec(
          "SELECT id, name, size, content_type, storage, r2_key, created_at, created_by FROM uploads ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?",
          Math.max(0, Math.floor(limit)) + 1,
          Math.max(0, Math.floor(offset)),
        )
        .toArray() as (Omit<UploadRow, "created_at"> & { created_at: number })[];
      const files = rows.slice(0, limit).map((r) => ({ ...r, created_at: formatMicros(r.created_at) }));
      return ok({ files, has_more: rows.length > limit });
    } catch (e) {
      return failure(e, "listing files");
    }
  }

  /** GET /files/{id}. */
  async getUpload(id: string): Promise<Result<UploadRow>> {
    try {
      const r = this.uploadRow(id);
      return r ? ok(r) : fail(notFound("file not found"));
    } catch (e) {
      return failure(e, "reading the file");
    }
  }

  /** DELETE /files/{id}: returns the R2 key to delete ("" when inline or missing). */
  async deleteUpload(id: string): Promise<Result<{ r2Key: string }>> {
    try {
      const r = this.uploadRow(id);
      if (!r) return ok({ r2Key: "" });
      this.ctx.storage.transactionSync(() => {
        this.deleteBlobs(["u:" + id]);
        this.sql.exec("DELETE FROM uploads WHERE id = ?", id);
      });
      return ok({ r2Key: r.storage === "r2" ? r.r2_key : "" });
    } catch (e) {
      return failure(e, "deleting the file");
    }
  }

  /** GET /upload-file?id= — the bytes of an upload (/__backyard/files/{id}/{name}). */
  private serveUpload(req: Request, id: string): Response {
    const r = /^[A-Za-z0-9_\-:.]{1,128}$/.test(id) ? this.uploadRow(id) : null;
    if (!r) return new Response(null, { status: 404, headers: { "x-backyard-not-found": "file" } });
    let ct = r.content_type || "application/octet-stream";
    if (ct.startsWith("text/") && !ct.includes("charset")) ct += "; charset=utf-8";
    const created = Date.parse(r.created_at);
    const obj: Servable = { name: r.name, size: r.size, etag: `"${r.id}"`, lastModified: created, contentType: ct };
    if (r.storage === "r2") obj.r2Key = r.r2_key;
    else {
      const key = "u:" + r.id;
      if (!this.blob(key)?.complete) return new Response(null, { status: 404, headers: { "x-backyard-not-found": "file" } });
      obj.open = (o, l) => this.openBlob(key, o, l);
    }
    const headers: Record<string, string> = { "Content-Disposition": contentDisposition("inline", r.name) };
    if (isActiveContent(ct)) headers["Content-Security-Policy"] = "sandbox";
    return serveObject(req, obj, { cacheControl: "private, no-cache", headers });
  }

  // ===========================================================================
  // Realtime (SPEC §6) — Hibernation API

  private acceptWS(req: Request): Response {
    if ((req.headers.get("Upgrade") ?? "").toLowerCase() !== "websocket") {
      return errorResponse(new HttpError(426, "bad_request", "expected a websocket upgrade"));
    }
    let user: User;
    try {
      user = JSON.parse(decodeURIComponent(req.headers.get("x-backyard-user") ?? "")) as User;
      if (!user || typeof user.id !== "string" || !user.id) throw new Error("no user");
    } catch {
      return errorResponse(new HttpError(401, "unauthenticated", "authentication required"));
    }
    const lim = this.cfg.limits;
    let mine = 0;
    for (const ws of this.ctx.getWebSockets()) if (this.state(ws)?.user.id === user.id) mine++;
    if (mine >= lim.ws_conns_per_user) {
      return errorResponse(rateLimited(`too many open connections for this user (max ${lim.ws_conns_per_user})`, 10));
    }
    const pair = new WebSocketPair();
    const [client, server] = [pair[0], pair[1]];
    this.ctx.acceptWebSocket(server);
    const st: ConnState = { cid: "c_" + randomHex(10), user, summary: summarize(user), subs: dict({}), rooms: dict({}) };
    this.save(server, st);
    server.send(JSON.stringify({ t: "hello", cid: st.cid, user }));
    return new Response(null, { status: 101, webSocket: client });
  }

  /** Per-connection state (rebuilt from the attachment after hibernation). */
  private state(ws: WebSocket): ConnState | null {
    const cached = this.conns.get(ws);
    if (cached) return cached;
    let att: unknown;
    try {
      att = ws.deserializeAttachment();
    } catch {
      return null;
    }
    if (!att || typeof att !== "object") return null;
    let st = att as ConnState & { spill?: boolean };
    if (st.spill) {
      if (!this.ready) return null;
      const row = this.sql.exec("SELECT data FROM ws_spill WHERE cid = ?", st.cid).toArray()[0] as { data: string } | undefined;
      if (!row) return null;
      st = JSON.parse(row.data) as ConnState;
    }
    // Rebuilt state is made of ordinary objects: rooms and subscriptions
    // become objects without prototype again.
    st.rooms = dict<RoomEntry>(st.rooms);
    st.subs = dict(st.subs);
    // State saved by an older version may carry a variable-width joined_at.
    for (const r of Object.values(st.rooms)) {
      if (r && typeof r.joined_at === "string") r.joined_at = toFixedMillis(r.joined_at);
    }
    this.conns.set(ws, st);
    return st;
  }

  /**
   * Persists a connection's state: in the socket attachment when it fits,
   * else in ws_spill. The JSON length only estimates the serialized size
   * (two-byte strings and numbers take more), so an attachment the runtime
   * rejects spills too.
   */
  private save(ws: WebSocket, st: ConnState): void {
    this.conns.set(ws, st);
    const text = JSON.stringify(st);
    if (text.length <= ATTACHMENT_MAX) {
      try {
        ws.serializeAttachment(st);
        if (this.ready) this.sql.exec("DELETE FROM ws_spill WHERE cid = ?", st.cid);
        return;
      } catch {
        /* larger than the runtime's attachment limit once serialized: spill */
      }
    }
    this.ensureSchema();
    this.sql.exec("INSERT OR REPLACE INTO ws_spill (cid, data) VALUES (?, ?)", st.cid, text);
    ws.serializeAttachment({ cid: st.cid, spill: true });
  }

  private send(ws: WebSocket, frame: unknown): void {
    try {
      ws.send(typeof frame === "string" ? frame : JSON.stringify(frame));
    } catch {
      /* closed */
    }
  }

  private sendError(ws: WebSocket, id: string | number | undefined, code: string, message: string): void {
    const f: Record<string, unknown> = { t: "error" };
    if (id !== undefined) f.id = id;
    f.error = { code, message };
    this.send(ws, f);
  }

  /** Token bucket per connection (messages_per_second, burst). */
  private allowFrame(cid: string): { ok: boolean; report: boolean } {
    const lim = this.cfg.limits;
    const now = Date.now();
    let b = this.buckets.get(cid);
    if (!b) {
      b = { tokens: lim.ws_message_burst, at: now, lastLimited: 0 };
      this.buckets.set(cid, b);
    }
    b.tokens = Math.min(lim.ws_message_burst, b.tokens + ((now - b.at) / 1000) * lim.ws_messages_per_second);
    b.at = now;
    if (b.tokens >= 1) {
      b.tokens -= 1;
      return { ok: true, report: false };
    }
    const report = now - b.lastLimited >= 1000;
    if (report) b.lastLimited = now;
    return { ok: false, report };
  }

  private sockets(): { ws: WebSocket; st: ConnState }[] {
    const out: { ws: WebSocket; st: ConnState }[] = [];
    for (const ws of this.ctx.getWebSockets()) {
      const st = this.state(ws);
      if (st) out.push({ ws, st });
    }
    return out;
  }

  private member(st: ConnState, room: string) {
    const r = st.rooms[room]!;
    // Callers check inRoom first.
    return { cid: st.cid, user: st.summary, state: r.state, joined_at: r.joined_at };
  }

  /**
   * The other members of room in the order of the Go edition: by joined_at
   * (fixed-width, so the string order is chronological), then by cid.
   */
  private others(room: string, cid: string) {
    return this.sockets()
      .filter((x) => x.st.cid !== cid && inRoom(x.st, room))
      .sort((a, b) => compareMembers(a.st.rooms[room]!.joined_at, a.st.cid, b.st.rooms[room]!.joined_at, b.st.cid))
      .map((x) => this.member(x.st, room));
  }

  private broadcastPresence(room: string, op: "join" | "leave" | "update", member: unknown, exceptCid: string): void {
    let frame: string | null = null;
    for (const { ws, st } of this.sockets()) {
      if (st.cid === exceptCid || !inRoom(st, room)) continue;
      frame ??= JSON.stringify({ t: "presence", room, op, member });
      this.send(ws, frame);
    }
  }

  /** Delivers a database change to subscribers of the collection. */
  private broadcastDB(collection: string, op: "create" | "update" | "delete", doc: DocWire | null, docID: string): void {
    for (const { ws, st } of this.sockets()) {
      for (const sub of Object.values(st.subs)) {
        if (sub.collection !== collection) continue;
        this.send(ws, { t: "db", sub: sub.id, collection, op, doc, doc_id: docID });
      }
    }
  }

  override async webSocketMessage(ws: WebSocket, message: string | ArrayBuffer): Promise<void> {
    const st = this.state(ws);
    if (!st) {
      try {
        ws.close(1011, "connection state lost; reconnect");
      } catch {
        /* closed */
      }
      return;
    }
    const lim = this.cfg.limits;
    const rl = this.allowFrame(st.cid);
    if (!rl.ok) {
      if (rl.report) {
        this.sendError(ws, undefined, "rate_limited", `too many messages (limit ${lim.ws_messages_per_second}/s, burst ${lim.ws_message_burst}); message dropped`);
      }
      return;
    }
    if (typeof message !== "string") {
      this.sendError(ws, undefined, "bad_request", "binary frames are not supported");
      return;
    }
    if (message.length > lim.ws_max_message_bytes || new TextEncoder().encode(message).length > lim.ws_max_message_bytes) {
      this.sendError(ws, undefined, "too_large", `frame exceeds ${lim.ws_max_message_bytes} bytes`);
      return;
    }
    let f: Record<string, unknown>;
    try {
      const v = JSON.parse(message) as unknown;
      if (!v || typeof v !== "object" || Array.isArray(v)) throw new Error("frame must be a JSON object");
      f = v as Record<string, unknown>;
    } catch (e) {
      this.sendError(ws, undefined, "bad_request", `malformed frame: ${(e as Error).message.slice(0, 200)}`);
      return;
    }
    let id: string | number | undefined;
    if (f.id !== undefined && f.id !== null) {
      if ((typeof f.id !== "string" && typeof f.id !== "number") || JSON.stringify(f.id).length > 128) {
        this.sendError(ws, undefined, "bad_request", "id must be a string or a number of at most 128 bytes");
        return;
      }
      id = f.id;
    }
    try {
      this.handleFrame(ws, st, id, f);
    } catch (e) {
      // Handlers change the connection's state before they save it: drop the
      // cached copy, so that the state is rebuilt from what was last saved
      // and memory never disagrees with what survives hibernation. Errors
      // never close the connection (SPEC §6).
      this.conns.delete(ws);
      console.error(`backyard: site: websocket frame ${JSON.stringify(String(f.t).slice(0, 32))} failed: ${(e as Error)?.message ?? e}`);
      this.sendError(ws, id, "internal", "the frame could not be processed");
    }
  }

  private handleFrame(ws: WebSocket, st: ConnState, id: string | number | undefined, f: Record<string, unknown>): void {
    switch (f.t) {
      case "ping":
        this.send(ws, '{"t":"pong"}');
        return;
      case "sub":
        this.onSub(ws, st, id, f.collection);
        return;
      case "unsub":
        if (id === undefined) return this.sendError(ws, undefined, "bad_request", "unsub requires the id of the subscription");
        delete st.subs[JSON.stringify(id)];
        this.save(ws, st);
        this.send(ws, { t: "ack", id });
        return;
      case "join":
        this.onJoin(ws, st, id, f.room, f.state);
        return;
      case "leave":
        if (typeof f.room !== "string" || !ROOM_RE.test(f.room)) return this.sendError(ws, id, "bad_request", "invalid room name");
        this.leaveRoom(ws, st, f.room);
        if (id !== undefined) this.send(ws, { t: "ack", id });
        return;
      case "state":
        this.onState(ws, st, id, f.room, f.state);
        return;
      case "send":
        this.onSend(ws, st, id, f);
        return;
      case undefined:
      case "":
        this.sendError(ws, id, "bad_request", 'missing frame type "t"');
        return;
      default:
        this.sendError(ws, id, "bad_request", `unknown frame type ${JSON.stringify(String(f.t).slice(0, 64))}`);
    }
  }

  private onSub(ws: WebSocket, st: ConnState, id: string | number | undefined, collection: unknown): void {
    if (id === undefined) return this.sendError(ws, undefined, "bad_request", "sub requires an id");
    if (typeof collection === "string" && collection.startsWith("_")) {
      return this.sendError(ws, id, "forbidden", "system collections cannot be subscribed to");
    }
    if (typeof collection !== "string" || !COLLECTION_RE.test(collection)) {
      return this.sendError(ws, id, "bad_request", `invalid collection name ${JSON.stringify(String(collection ?? "").slice(0, 64))}`);
    }
    const key = JSON.stringify(id);
    const existing = st.subs[key];
    if (!existing && Object.keys(st.subs).length >= this.cfg.limits.ws_subs_per_conn) {
      return this.sendError(ws, id, "rate_limited", `too many subscriptions on this connection (max ${this.cfg.limits.ws_subs_per_conn})`);
    }
    st.subs[key] = { id, collection };
    this.save(ws, st);
    this.send(ws, { t: "ack", id });
  }

  private normalizeState(raw: unknown): Record<string, unknown> {
    if (raw === undefined || raw === null) return {};
    if (typeof raw !== "object" || Array.isArray(raw)) throw new HttpError(400, "bad_request", "state must be a JSON object");
    const text = JSON.stringify(raw);
    const max = this.cfg.limits.ws_max_presence_bytes;
    if (new TextEncoder().encode(text).length > max) throw new HttpError(413, "too_large", `presence state exceeds ${max} bytes`);
    return raw as Record<string, unknown>;
  }

  private onJoin(ws: WebSocket, st: ConnState, id: string | number | undefined, room: unknown, rawState: unknown): void {
    if (typeof room !== "string" || !ROOM_RE.test(room)) {
      return this.sendError(ws, id, "bad_request", "invalid room name (1-128 of A-Z a-z 0-9 _ - : .)");
    }
    let state: Record<string, unknown>;
    try {
      state = this.normalizeState(rawState);
    } catch (e) {
      const he = e as HttpError;
      return this.sendError(ws, id, he.code, he.message);
    }
    const cur = inRoom(st, room) ? st.rooms[room] : undefined;
    if (cur) {
      const changed = JSON.stringify(cur.state) !== JSON.stringify(state);
      cur.state = state;
      this.save(ws, st);
      const me = this.member(st, room);
      this.send(ws, withID({ t: "joined" }, id, { room, self: me, members: this.others(room, st.cid) }));
      if (changed) this.broadcastPresence(room, "update", me, st.cid);
      return;
    }
    if (Object.keys(st.rooms).length >= this.cfg.limits.ws_rooms_per_conn) {
      return this.sendError(ws, id, "rate_limited", `too many rooms on this connection (max ${this.cfg.limits.ws_rooms_per_conn})`);
    }
    st.rooms[room] = { state, joined_at: formatMillisFixed(Date.now()) };
    this.save(ws, st);
    const me = this.member(st, room);
    this.send(ws, withID({ t: "joined" }, id, { room, self: me, members: this.others(room, st.cid) }));
    this.broadcastPresence(room, "join", me, st.cid);
  }

  private leaveRoom(ws: WebSocket, st: ConnState, room: string): void {
    if (!inRoom(st, room)) return;
    const me = this.member(st, room);
    delete st.rooms[room];
    this.save(ws, st);
    this.broadcastPresence(room, "leave", me, st.cid);
  }

  private onState(ws: WebSocket, st: ConnState, id: string | number | undefined, room: unknown, rawState: unknown): void {
    if (typeof room !== "string" || !inRoom(st, room)) {
      return this.sendError(ws, id, "not_found", `not a member of room ${JSON.stringify(String(room ?? "").slice(0, 128))}`);
    }
    let state: Record<string, unknown>;
    try {
      state = this.normalizeState(rawState);
    } catch (e) {
      const he = e as HttpError;
      return this.sendError(ws, id, he.code, he.message);
    }
    const cur = st.rooms[room]!;
    const changed = JSON.stringify(cur.state) !== JSON.stringify(state);
    cur.state = state;
    if (changed) {
      this.save(ws, st);
      this.broadcastPresence(room, "update", this.member(st, room), st.cid);
    }
    if (id !== undefined) this.send(ws, { t: "ack", id });
  }

  private onSend(ws: WebSocket, st: ConnState, id: string | number | undefined, f: Record<string, unknown>): void {
    const room = f.room;
    if (typeof room !== "string" || !inRoom(st, room)) {
      return this.sendError(ws, id, "not_found", `not a member of room ${JSON.stringify(String(room ?? "").slice(0, 128))}`);
    }
    const to = typeof f.to === "string" ? f.to : "";
    const echo = f.echo === true;
    const members = this.sockets().filter((x) => inRoom(x.st, room));
    if (to) {
      if (!CID_RE.test(to)) return this.sendError(ws, id, "bad_request", "invalid recipient connection id");
      if (!members.some((x) => x.st.cid === to)) return this.sendError(ws, id, "not_found", `no member ${JSON.stringify(to)} in room ${JSON.stringify(room)}`);
    }
    const frame = JSON.stringify({ t: "msg", room, from: { cid: st.cid, user: st.summary }, data: f.data === undefined ? null : f.data });
    for (const m of members) {
      if (to) {
        if (m.st.cid === to || (echo && m.st.cid === st.cid && st.cid !== to)) this.send(m.ws, frame);
      } else if (m.st.cid !== st.cid || echo) {
        this.send(m.ws, frame);
      }
    }
    if (id !== undefined) this.send(ws, { t: "ack", id });
  }

  private dropConnection(ws: WebSocket): void {
    const st = this.state(ws);
    if (!st) return;
    for (const room of Object.keys(st.rooms)) {
      const me = this.member(st, room);
      delete st.rooms[room];
      this.broadcastPresence(room, "leave", me, st.cid);
    }
    st.subs = dict({});
    this.conns.delete(ws);
    this.buckets.delete(st.cid);
    if (this.ready) this.sql.exec("DELETE FROM ws_spill WHERE cid = ?", st.cid);
    try {
      ws.serializeAttachment(null);
    } catch {
      /* closed */
    }
  }

  override async webSocketClose(ws: WebSocket): Promise<void> {
    this.dropConnection(ws);
  }

  override async webSocketError(ws: WebSocket): Promise<void> {
    this.dropConnection(ws);
  }
}

/** Orders presence members by joined_at (fixed-width RFC 3339), then cid. */
export function compareMembers(joinedA: string, cidA: string, joinedB: string, cidB: string): number {
  if (joinedA !== joinedB) return joinedA < joinedB ? -1 : 1;
  return cidA < cidB ? -1 : cidA > cidB ? 1 : 0;
}

function withID(base: Record<string, unknown>, id: string | number | undefined, rest: Record<string, unknown>): Record<string, unknown> {
  const out = { ...base };
  if (id !== undefined) out.id = id;
  return Object.assign(out, rest);
}

/** Seconds until the next UTC midnight (Retry-After of daily caps). */
export function secondsToMidnight(now = Date.now()): number {
  const d = new Date(now);
  const next = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 1);
  return Math.max(1, Math.ceil((next - now) / 1000));
}

/** Content-Disposition with an ASCII fallback and an RFC 5987 UTF-8 name. */
export function contentDisposition(kind: string, name: string): string {
  const ascii = /^[\x20-\x7e]*$/.test(name) && !/["\\]/.test(name);
  if (ascii) return `${kind}; filename="${name}"`;
  const fallback = name.replace(/[^\x20-\x7e]|["\\]/g, "_");
  return `${kind}; filename="${fallback}"; filename*=UTF-8''${encodeURIComponent(name)}`;
}
