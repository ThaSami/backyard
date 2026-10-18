/**
 * Site previews (SPEC §7.2): an image the site ships itself, chosen at every
 * write like the title — the og:image of the root index.html when it names a
 * usable file of the site, else the first of PREVIEW_NAMES — and served on
 * the hub by GET /__backyard/api/sites/{site}/preview (hub.ts). A port of
 * internal/sites/preview.go: the same values, rules and results.
 */
import { byteString } from "./title";

/** The root files tried, in order, when the og:image of index.html is missing or unusable. */
export const PREVIEW_NAMES = ["preview.png", "preview.jpg", "preview.jpeg", "preview.webp", "preview.avif", "preview.gif", "preview.svg"];

/** The largest file that can be a preview. */
export const MAX_PREVIEW_BYTES = 1 << 20;

/**
 * The Content-Security-Policy of a served preview: an SVG on the hub's
 * origin must never run script or load anything, and a navigation to it
 * stays sandboxed.
 */
export const PREVIEW_CSP = "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'";

/** The (lowercased) extensions a preview may have, without the dot, and the Content-Type it is served with. */
export const PREVIEW_TYPES: Readonly<Record<string, string>> = {
  png: "image/png",
  jpg: "image/jpeg",
  jpeg: "image/jpeg",
  gif: "image/gif",
  webp: "image/webp",
  avif: "image/avif",
  svg: "image/svg+xml",
};

/** Cache-Control of a preview requested with a version (?v=…): the hub changes the URL when the site changes. */
export const PREVIEW_CACHE_VERSIONED = "private, max-age=31536000, immutable";
/** Cache-Control of a preview requested without a version: revalidated on every use. */
export const PREVIEW_CACHE_UNVERSIONED = "private, no-cache";

/** The longest og:image reference, in bytes. */
const MAX_PREVIEW_REF = 1024;

const SCHEME_RE = /^[A-Za-z][A-Za-z0-9+.-]*:/;
const validUTF8 = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });

/**
 * The Content-Type of a preview path by its extension, or "" when the
 * extension is not one of PREVIEW_TYPES. The extension is lowercased like
 * Go's strings.ToLower, which also maps U+0130 to "i" and U+212A to "k".
 */
export function previewType(p: string): string {
  const name = p.slice(p.lastIndexOf("/") + 1);
  const dot = name.lastIndexOf(".");
  if (dot < 0) return "";
  const ext = name
    .slice(dot + 1)
    .replace(/[A-Z]/g, (c) => c.toLowerCase())
    .replace(/\u0130/g, "i")
    .replace(/\u212a/g, "k");
  return Object.hasOwn(PREVIEW_TYPES, ext) ? PREVIEW_TYPES[ext]! : "";
}

/**
 * Turns the og:image reference of a site's root index.html into a site
 * path, or null. It accepts only references to the site's own files:
 * relative ("img/a.png", "./a.png") or site-root relative ("/a.png"),
 * without a query or fragment. It refuses references that are empty or
 * longer than 1024 bytes, have a URL scheme, start with "//" or contain
 * "\", have an invalid percent escape, or decode to something with a
 * control character, invalid UTF-8, or an empty, ".", ".." or dot-prefixed
 * segment. The checks run on the bytes Go would see (byteString), so that
 * extractPage's surrogate escapes count as the invalid bytes they stand for.
 */
export function resolvePreviewRef(ref: string): string | null {
  if (ref === "" || ref.length > MAX_PREVIEW_REF) return null; // never fewer bytes than UTF-16 units
  let s = byteString(ref);
  if (s.length > MAX_PREVIEW_REF || SCHEME_RE.test(s) || s.startsWith("//") || s.includes("\\")) return null;
  const cut = s.search(/[?#]/);
  if (cut >= 0) s = s.slice(0, cut);
  if (s.startsWith("/")) s = s.slice(1);
  while (s.startsWith("./")) s = s.slice(2);
  if (/%(?![0-9A-Fa-f]{2})/.test(s)) return null;
  s = s.replace(/%([0-9A-Fa-f]{2})/g, (_, h: string) => String.fromCharCode(parseInt(h, 16)));
  // eslint-disable-next-line no-control-regex
  if (s === "" || /[\x00-\x1f\x7f]/.test(s) || s.split("/").some((seg) => seg === "" || seg.startsWith("."))) return null;
  try {
    return validUTF8.decode(Uint8Array.from(s, (c) => c.charCodeAt(0)));
  } catch {
    return null;
  }
}

/**
 * The preview of a site after a write: ogImage, the og:image reference of
 * its root index.html ("" when none), when it resolves to one of its files
 * with a PREVIEW_TYPES extension and at most MAX_PREVIEW_BYTES; else the
 * first of PREVIEW_NAMES among its files within that size; else "". sizeOf
 * gives the size of a file of the site, undefined for no such file.
 */
export function choosePreview(ogImage: string, sizeOf: (path: string) => number | undefined): string {
  const usable = (p: string) => {
    const n = sizeOf(p);
    return n !== undefined && n <= MAX_PREVIEW_BYTES;
  };
  const p = resolvePreviewRef(ogImage);
  if (p !== null && previewType(p) && usable(p)) return p;
  return PREVIEW_NAMES.find(usable) ?? "";
}
