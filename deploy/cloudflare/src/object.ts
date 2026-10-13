/**
 * Serving of stored objects with HTTP semantics (SPEC §3): strong ETags,
 * Last-Modified, conditional requests (304/412), HEAD and single byte ranges
 * (206/416). A port of internal/server/object.go.
 */
import { HttpError } from "./http";

/** Something servable: a site file, an upload, a hub asset. */
export interface Servable {
  /** File name (for logs). */
  name: string;
  size: number;
  /** Quoted strong ETag. */
  etag: string;
  /** Modification time in milliseconds since the epoch (0 = unknown). */
  lastModified: number;
  contentType: string;
  /** Full content when held in memory. */
  data?: Uint8Array;
  /** Returns the bytes [offset, offset+length) when data is not set. */
  open?: (offset: number, length: number) => ReadableStream<Uint8Array> | Uint8Array;
  /**
   * The object lives in R2 under this key: the response carries no body and
   * the internal headers below so that the Worker streams the range from R2.
   */
  r2Key?: string;
}

/** Internal headers of a response whose body must be read from R2 by the Worker. */
export const R2_KEY_HEADER = "x-backyard-r2-key";
export const R2_RANGE_HEADER = "x-backyard-r2-range";

export interface ServeOptions {
  /** 200 (default) enables conditional and range handling. */
  status?: number;
  /** Default "no-cache". */
  cacheControl?: string;
  /** Extra headers (Content-Disposition, CSP…). */
  headers?: Record<string, string>;
}

/** Quoted content ETag: hex of the first 12 bytes of the SHA-256. */
export async function contentETag(data: Uint8Array): Promise<string> {
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", data));
  let s = "";
  for (const b of sum.subarray(0, 12)) s += b.toString(16).padStart(2, "0");
  return `"${s}"`;
}

/** Formats an HTTP date (IMF-fixdate). */
export function httpDate(ms: number): string {
  return new Date(Math.floor(ms / 1000) * 1000).toUTCString();
}

function parseHTTPDate(s: string): number | null {
  const t = Date.parse(s);
  return Number.isFinite(t) ? t : null;
}

function etagListMatch(list: string, etag: string, strong: boolean): boolean {
  if (!etag) return false;
  for (let item of list.split(",")) {
    item = item.trim();
    if (item === "*") return true;
    const weak = item.startsWith("W/");
    if (weak && strong) continue;
    if ((weak ? item.slice(2) : item) === etag) return true;
  }
  return false;
}

/** Evaluates preconditions (RFC 9110 §13.2.2): 0 to proceed, 304 or 412. */
export function checkPreconditions(req: Request, obj: Servable): 0 | 304 | 412 {
  const modSec = obj.lastModified > 0 ? Math.floor(obj.lastModified / 1000) * 1000 : 0;
  const im = req.headers.get("If-Match");
  if (im) {
    if (!etagListMatch(im, obj.etag, true)) return 412;
  } else {
    const ius = req.headers.get("If-Unmodified-Since");
    if (ius && modSec) {
      const t = parseHTTPDate(ius);
      if (t !== null && modSec > t) return 412;
    }
  }
  const get = req.method === "GET" || req.method === "HEAD";
  const inm = req.headers.get("If-None-Match");
  if (inm) {
    if (etagListMatch(inm, obj.etag, false)) return get ? 304 : 412;
    return 0;
  }
  const ims = req.headers.get("If-Modified-Since");
  if (ims && get && modSec) {
    const t = parseHTTPDate(ims);
    if (t !== null && !(modSec > t)) return 304;
  }
  return 0;
}

function ifRangeMatches(req: Request, obj: Servable): boolean {
  const ir = (req.headers.get("If-Range") ?? "").trim();
  if (!ir) return true;
  if (ir.startsWith('"')) return ir === obj.etag;
  if (ir.startsWith("W/")) return false;
  const t = parseHTTPDate(ir);
  return t !== null && obj.lastModified > 0 && Math.floor(obj.lastModified / 1000) * 1000 === t;
}

export type RangeResult = { kind: "ignore" } | { kind: "ok"; start: number; length: number } | { kind: "unsatisfiable" };

/** Parses a single "bytes=" range against size (multiple ranges are ignored). */
export function parseRange(h: string, size: number): RangeResult {
  const eq = h.trim().indexOf("=");
  if (eq < 0 || h.trim().slice(0, eq).trim().toLowerCase() !== "bytes") return { kind: "ignore" };
  const spec = h.trim().slice(eq + 1).trim();
  if (spec === "" || spec.includes(",")) return { kind: "ignore" };
  const dash = spec.indexOf("-");
  if (dash < 0) return { kind: "ignore" };
  const first = spec.slice(0, dash).trim();
  const last = spec.slice(dash + 1).trim();
  const isInt = (s: string) => /^\d+$/.test(s);
  if (first === "") {
    if (!isInt(last)) return { kind: "ignore" };
    let n = Number(last);
    if (n === 0 || size === 0) return { kind: "unsatisfiable" };
    n = Math.min(n, size);
    return { kind: "ok", start: size - n, length: n };
  }
  if (!isInt(first)) return { kind: "ignore" };
  const a = Number(first);
  let end = size - 1;
  if (last !== "") {
    if (!isInt(last)) return { kind: "ignore" };
    const b = Number(last);
    if (b < a) return { kind: "ignore" };
    end = Math.min(b, size - 1);
  }
  if (a >= size) return { kind: "unsatisfiable" };
  return { kind: "ok", start: a, length: end - a + 1 };
}

/**
 * Builds the response for obj: validators, conditional GET, HEAD and single
 * byte-range support. Throws HttpError(412) for failed preconditions.
 */
export function serveObject(req: Request, obj: Servable, opts: ServeOptions = {}): Response {
  let status = opts.status ?? 200;
  const h = new Headers(opts.headers);
  h.set("Cache-Control", opts.cacheControl ?? "no-cache");
  let offset = 0;
  let length = obj.size;
  if (status === 200) {
    if (obj.etag) h.set("ETag", obj.etag);
    if (obj.lastModified > 0) h.set("Last-Modified", httpDate(obj.lastModified));
    h.set("Accept-Ranges", "bytes");
    const pre = checkPreconditions(req, obj);
    if (pre === 304) return new Response(null, { status: 304, headers: h });
    if (pre === 412) throw new HttpError(412, "bad_request", "precondition failed");
    const rh = req.headers.get("Range");
    if (rh && (req.method === "GET" || req.method === "HEAD") && ifRangeMatches(req, obj)) {
      const r = parseRange(rh, obj.size);
      if (r.kind === "unsatisfiable") {
        const msg = "requested range not satisfiable\n";
        const eh = new Headers({
          "Content-Range": `bytes */${obj.size}`,
          "Content-Type": "text/plain; charset=utf-8",
          "Cache-Control": "no-store",
          "Accept-Ranges": "bytes",
        });
        return new Response(req.method === "HEAD" ? null : msg, { status: 416, headers: eh });
      }
      if (r.kind === "ok") {
        offset = r.start;
        length = r.length;
        status = 206;
        h.set("Content-Range", `bytes ${r.start}-${r.start + r.length - 1}/${obj.size}`);
      }
    }
  }
  h.set("Content-Type", obj.contentType);
  h.set("Content-Length", String(length));
  if (obj.r2Key) {
    h.set(R2_KEY_HEADER, obj.r2Key);
    h.set(R2_RANGE_HEADER, `${offset}-${length}`);
    return new Response(null, { status, headers: h });
  }
  if (req.method === "HEAD" || length === 0) {
    return new Response(null, { status, headers: h });
  }
  let body: ReadableStream<Uint8Array> | Uint8Array;
  if (obj.data) body = obj.data.subarray(offset, offset + length);
  else if (obj.open) body = obj.open(offset, length);
  else throw new HttpError(500, "internal", "object has no content");
  if (body instanceof ReadableStream) {
    // A known-length stream keeps Content-Length on the wire.
    const fixed = new FixedLengthStream(length);
    void body.pipeTo(fixed.writable).catch(() => {});
    body = fixed.readable;
  }
  return new Response(body, { status, headers: h });
}

/** Reads the R2-backed body of a response produced by serveObject (Worker side). */
export async function resolveR2Body(res: Response, bucket: R2Bucket | undefined, method: string): Promise<Response> {
  const key = res.headers.get(R2_KEY_HEADER);
  if (!key) return res;
  const headers = new Headers(res.headers);
  headers.delete(R2_KEY_HEADER);
  const range = headers.get(R2_RANGE_HEADER) ?? "";
  headers.delete(R2_RANGE_HEADER);
  if (method === "HEAD" || res.status === 304) return new Response(null, { status: res.status, headers });
  if (!bucket) throw new HttpError(500, "internal", "this file is stored in R2 but no R2 bucket is bound");
  const [o, l] = range.split("-").map(Number);
  const offset = Number.isFinite(o) ? o! : 0;
  const length = Number.isFinite(l) ? l! : 0;
  if (length === 0) return new Response(null, { status: res.status, headers });
  const obj = await bucket.get(key, { range: { offset, length } });
  if (!obj || !("body" in obj)) throw new HttpError(404, "not_found", "file not found");
  return new Response(obj.body, { status: res.status, headers });
}
