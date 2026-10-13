/**
 * Content types by extension (the fixed table of internal/blob/contenttype.go,
 * so both editions serve identical headers) plus a small content sniffer.
 */

const CONTENT_TYPES: Record<string, string> = {
  ".html": "text/html; charset=utf-8",
  ".htm": "text/html; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".js": "text/javascript; charset=utf-8",
  ".mjs": "text/javascript; charset=utf-8",
  ".cjs": "text/javascript; charset=utf-8",
  ".json": "application/json",
  ".map": "application/json",
  ".webmanifest": "application/manifest+json",
  ".txt": "text/plain; charset=utf-8",
  ".md": "text/markdown; charset=utf-8",
  ".csv": "text/csv; charset=utf-8",
  ".tsv": "text/tab-separated-values; charset=utf-8",
  ".xml": "application/xml",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".jpg": "image/jpeg",
  ".jpeg": "image/jpeg",
  ".gif": "image/gif",
  ".webp": "image/webp",
  ".avif": "image/avif",
  ".ico": "image/x-icon",
  ".bmp": "image/bmp",
  ".woff": "font/woff",
  ".woff2": "font/woff2",
  ".ttf": "font/ttf",
  ".otf": "font/otf",
  ".mp4": "video/mp4",
  ".webm": "video/webm",
  ".mov": "video/quicktime",
  ".mp3": "audio/mpeg",
  ".wav": "audio/wav",
  ".ogg": "audio/ogg",
  ".m4a": "audio/mp4",
  ".pdf": "application/pdf",
  ".zip": "application/zip",
  ".gz": "application/gzip",
  ".wasm": "application/wasm",
  ".glb": "model/gltf-binary",
  ".gltf": "model/gltf+json",
  ".ts": "text/plain; charset=utf-8",
  ".tsx": "text/plain; charset=utf-8",
  ".jsx": "text/javascript; charset=utf-8",
  ".yaml": "text/yaml; charset=utf-8",
  ".yml": "text/yaml; charset=utf-8",
  ".ics": "text/calendar; charset=utf-8",
};

/** Extension of the last path segment, lowercased (".html"), or "". */
export function extOf(name: string): string {
  const base = name.slice(name.lastIndexOf("/") + 1);
  const dot = base.lastIndexOf(".");
  return dot < 0 ? "" : base.slice(dot).toLowerCase();
}

/** Content type for a file name's extension, or "". */
export function contentTypeByExt(name: string): string {
  return CONTENT_TYPES[extOf(name)] ?? "";
}

/**
 * Picks the served Content-Type (SPEC §3): by extension, else the stored type,
 * else application/octet-stream; text types get "; charset=utf-8".
 */
export function contentTypeFor(name: string, stored = ""): string {
  let ct = contentTypeByExt(name) || stored.trim() || "application/octet-stream";
  if (ct.startsWith("text/") && !ct.toLowerCase().includes("charset")) ct += "; charset=utf-8";
  return ct;
}

function startsWith(b: Uint8Array, sig: number[], at = 0): boolean {
  if (b.length < at + sig.length) return false;
  for (let i = 0; i < sig.length; i++) if (b[at + i] !== sig[i]) return false;
  return true;
}

/** Sniffs common binary formats; returns "" when unknown. */
export function sniff(head: Uint8Array): string {
  if (startsWith(head, [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) return "image/png";
  if (startsWith(head, [0xff, 0xd8, 0xff])) return "image/jpeg";
  if (startsWith(head, [0x47, 0x49, 0x46, 0x38])) return "image/gif";
  if (startsWith(head, [0x52, 0x49, 0x46, 0x46]) && startsWith(head, [0x57, 0x45, 0x42, 0x50], 8)) return "image/webp";
  if (startsWith(head, [0x25, 0x50, 0x44, 0x46, 0x2d])) return "application/pdf";
  if (startsWith(head, [0x50, 0x4b, 0x03, 0x04])) return "application/zip";
  if (startsWith(head, [0x1f, 0x8b])) return "application/gzip";
  if (startsWith(head, [0x00, 0x61, 0x73, 0x6d])) return "application/wasm";
  if (startsWith(head, [0x66, 0x74, 0x79, 0x70], 4)) return "video/mp4";
  const text = new TextDecoder().decode(head.subarray(0, 512)).trimStart().toLowerCase();
  if (text.startsWith("<!doctype html") || text.startsWith("<html")) return "text/html; charset=utf-8";
  if (text.startsWith("<svg") || (text.startsWith("<?xml") && text.includes("<svg"))) return "image/svg+xml";
  if (head.length > 0 && isProbablyText(head)) return "text/plain; charset=utf-8";
  return "";
}

function isProbablyText(b: Uint8Array): boolean {
  const n = Math.min(b.length, 512);
  for (let i = 0; i < n; i++) {
    const c = b[i]!;
    if (c === 0) return false;
    if (c < 0x09 || (c > 0x0d && c < 0x20 && c !== 0x1b)) return false;
  }
  try {
    new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(b.subarray(0, n));
  } catch {
    // A multi-byte character cut at the boundary is fine; anything else is binary.
    return n === b.length ? false : true;
  }
  return true;
}

/** Content type for stored bytes: by extension, else sniffed, else octet-stream. */
export function detectContentType(name: string, head?: Uint8Array): string {
  return contentTypeByExt(name) || (head ? sniff(head) : "") || "application/octet-stream";
}

/** Validates a client-supplied media type; returns a normalized value or "". */
export function normalizeContentType(ct: string | null | undefined): string {
  if (!ct) return "";
  const [rawType, ...params] = ct.split(";");
  const mt = (rawType ?? "").trim().toLowerCase();
  if (!/^[a-z0-9][a-z0-9!#$&^_.+-]*\/[a-z0-9][a-z0-9!#$&^_.+-]*$/.test(mt)) return "";
  for (const p of params) {
    const [k, v] = p.split("=");
    if ((k ?? "").trim().toLowerCase() === "charset" && v) {
      const cs = v.trim().replace(/^"|"$/g, "");
      if (/^[A-Za-z0-9._-]{1,40}$/.test(cs)) return `${mt}; charset=${cs}`;
    }
  }
  return mt;
}

/** Content types a browser may execute (served with CSP sandbox when uploaded). */
export function isActiveContent(ct: string): boolean {
  const mt = ct.split(";")[0]!.trim().toLowerCase();
  if (!mt.includes("/")) return true;
  return (
    mt === "text/html" ||
    mt === "application/xhtml+xml" ||
    mt === "image/svg+xml" ||
    mt === "text/xml" ||
    mt === "application/xml" ||
    mt.endsWith("+xml")
  );
}
