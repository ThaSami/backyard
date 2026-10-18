/**
 * Reads what a site's root index.html says about the site (SPEC §7.2): the
 * <title>, the <meta name="description"> and the og:image reference. A port
 * of internal/sites/title.go with the same results: the document is read as
 * the bytes Go reads, comments and raw-text elements are skipped, character
 * references are decoded like Go's html.UnescapeString, whitespace is
 * collapsed and lengths are capped.
 */
import { ENTITY_DATA } from "./entities";

/** Only the first 64 KiB of index.html are scanned. */
export const TITLE_SCAN_BYTES = 64 * 1024;

const MAX_TITLE = 256;
const MAX_DESCRIPTION = 1024;
const RAW_TEXT = ["script", "style", "svg", "template", "textarea", "noscript", "math"];

/** What a site's root index.html says about the site. */
export interface Page {
  /** The <title> text. */
  title: string;
  /** The <meta name="description"> content. */
  description: string;
  /** The og:image reference, entity-decoded and trimmed but not resolved (resolvePreviewRef, choosePreview). */
  image: string;
}

// --- bytes as Go sees them -------------------------------------------------

const fatalUTF8 = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
const utf8 = new TextEncoder();

/** The length of the valid UTF-8 sequence at b[i] (Go's utf8.DecodeRune), or 0 when it is invalid there. */
export function utf8SeqLen(b: Uint8Array, i: number): number {
  const c = b[i]!;
  if (c < 0x80) return 1;
  const cont = (j: number, lo = 0x80, hi = 0xbf) => i + j < b.length && b[i + j]! >= lo && b[i + j]! <= hi;
  if (c >= 0xc2 && c <= 0xdf) return cont(1) ? 2 : 0;
  if (c === 0xe0) return cont(1, 0xa0) && cont(2) ? 3 : 0;
  if ((c >= 0xe1 && c <= 0xec) || c === 0xee || c === 0xef) return cont(1) && cont(2) ? 3 : 0;
  if (c === 0xed) return cont(1, 0x80, 0x9f) && cont(2) ? 3 : 0;
  if (c === 0xf0) return cont(1, 0x90) && cont(2) && cont(3) ? 4 : 0;
  if (c >= 0xf1 && c <= 0xf3) return cont(1) && cont(2) && cont(3) ? 4 : 0;
  if (c === 0xf4) return cont(1, 0x80, 0x8f) && cont(2) && cont(3) ? 4 : 0;
  return 0;
}

/**
 * Text of bytes as Go's string functions see them: valid UTF-8 becomes its
 * characters, and every other byte a lone surrogate U+DC80–U+DCFF (a
 * "surrogate escape"), so that it survives for the checks Go makes on the
 * bytes (strings.ToValidUTF8, utf8.ValidString). byteString turns it back.
 */
export function decodeBytes(b: Uint8Array): string {
  try {
    return fatalUTF8.decode(b);
  } catch {
    /* escaped below */
  }
  let out = "";
  let start = 0;
  for (let i = 0; i < b.length; ) {
    const n = utf8SeqLen(b, i);
    if (n > 0) {
      i += n;
      continue;
    }
    out += fatalUTF8.decode(b.subarray(start, i)) + String.fromCharCode(0xdc00 + b[i]!);
    start = ++i;
  }
  return out + fatalUTF8.decode(b.subarray(start));
}

/**
 * The bytes of s as Go would hold them, one character U+0000–U+00FF per
 * byte: UTF-8, with the surrogate escapes of decodeBytes back as their bytes
 * (any other lone surrogate encodes as U+FFFD, like TextEncoder does).
 */
export function byteString(s: string): string {
  let out = "";
  for (const ch of s) {
    const c = ch.codePointAt(0)!;
    if (c < 0x80) out += ch;
    else if (c >= 0xdc80 && c <= 0xdcff) out += String.fromCharCode(c - 0xdc00);
    else for (const b of utf8.encode(ch)) out += String.fromCharCode(b);
  }
  return out;
}

// --- character references (Go's html.UnescapeString) ------------------------

/** The longest name of a reference HTML accepts without its ";". */
const LONGEST_LEGACY = 6;

/** Windows-1252 characters of the numeric references &#x80; to &#x9F; (Go's replacementTable). */
const C1_REPLACEMENTS = [
  0x20ac, 0x81, 0x201a, 0x192, 0x201e, 0x2026, 0x2020, 0x2021, 0x2c6, 0x2030, 0x160, 0x2039, 0x152, 0x8d, 0x17d, 0x8f, 0x90, 0x2018, 0x2019, 0x201c, 0x201d,
  0x2022, 0x2013, 0x2014, 0x2dc, 0x2122, 0x161, 0x203a, 0x153, 0x9d, 0x17e, 0x178,
];

let entityTable: Map<string, string> | null = null;

/** Reference name ("amp;", or "amp" for a legacy one) → its text (ENTITY_DATA). */
function entities(): Map<string, string> {
  if (entityTable) return entityTable;
  const t = new Map<string, string>();
  for (const e of ENTITY_DATA.trim().split(/\s+/)) {
    const [name, hex] = e.split(":") as [string, string];
    t.set(name, String.fromCodePoint(...hex.split(",").map((h) => parseInt(h, 16))));
  }
  entityTable = t;
  return t;
}

const isAlnum = (c: number) => (c >= 0x30 && c <= 0x39) || (c >= 0x41 && c <= 0x5a) || (c >= 0x61 && c <= 0x7a);

function hexValue(c: number): number {
  if (c >= 0x30 && c <= 0x39) return c - 0x30;
  if (c >= 0x41 && c <= 0x46) return c - 0x41 + 10;
  if (c >= 0x61 && c <= 0x66) return c - 0x61 + 10;
  return -1;
}

/** Decodes the reference at s[i] === "&" (Go's unescapeEntity): its text and the index after it. */
function unescapeEntity(s: string, i: number): [string, number] {
  // Go measures the rest in bytes; the decisions below come out the same.
  const rest = s.length - i;
  if (rest <= 1) return ["&", i + 1];
  let j = i + 1;
  if (s[j] === "#") {
    if (rest <= 3) return ["&", i + 1];
    j++;
    const hex = s[j] === "x" || s[j] === "X";
    if (hex) j++;
    let x = 0;
    while (j < s.length) {
      const c = s.charCodeAt(j++);
      const d = hex ? hexValue(c) : c >= 0x30 && c <= 0x39 ? c - 0x30 : -1;
      if (d >= 0) {
        x = (Math.imul(x, hex ? 16 : 10) + d) | 0; // a Go rune overflows the same way
        continue;
      }
      if (c !== 0x3b) j--;
      break;
    }
    if (j - i <= 3) return ["&", i + 1]; // nothing matched
    if (x >= 0x80 && x <= 0x9f) x = C1_REPLACEMENTS[x - 0x80]!;
    else if (x <= 0 || (x >= 0xd800 && x <= 0xdfff) || x > 0x10ffff) x = 0xfffd;
    return [String.fromCodePoint(x), j];
  }
  // The longest run of letters and digits, and a ";" after it.
  while (j < s.length) {
    const c = s.charCodeAt(j++);
    if (isAlnum(c)) continue;
    if (c !== 0x3b) j--;
    break;
  }
  const name = s.slice(i + 1, j);
  if (name) {
    const table = entities();
    const text = table.get(name);
    if (text !== undefined) return [text, j];
    // A legacy name without its ";" may run into more letters: "&notit;" is "¬it;".
    for (let k = Math.min(name.length - 1, LONGEST_LEGACY); k > 1; k--) {
      const p = table.get(name.slice(0, k));
      if (p !== undefined) return [p, i + 1 + k];
    }
  }
  return [s.slice(i, j), j];
}

/** Decodes character references like Go's html.UnescapeString (also those HTML accepts without a ";"). */
export function unescapeHTML(s: string): string {
  let i = s.indexOf("&");
  if (i < 0) return s;
  let out = s.slice(0, i);
  for (;;) {
    const [text, next] = unescapeEntity(s, i);
    out += text;
    i = s.indexOf("&", next);
    if (i < 0) return out + s.slice(next);
    out += s.slice(next, i);
  }
}

// --- the scan ---------------------------------------------------------------

/** Go's unicode.IsSpace: JavaScript's \s without U+FEFF, with U+0085. */
const SPACE = "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const SPACES_RE = new RegExp(`[${SPACE}]+`, "u");
const TRIM_RE = new RegExp(`^[${SPACE}]+|[${SPACE}]+$`, "gu");

/** HTML's ASCII whitespace (Go's trimASCIISpace). */
const trimASCIISpace = (s: string) => s.replace(/^[ \t\n\r\f]+|[ \t\n\r\f]+$/g, "");

function asciiLower(s: string): string {
  return s.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) + 32));
}

/**
 * Whether s equals the lowercase ASCII word under Go's strings.EqualFold:
 * ASCII letters in either case, and the two other characters that fold to
 * ASCII letters (U+017F to "s", U+212A to "k").
 */
function equalFoldASCII(s: string, word: string): boolean {
  const chars = [...s];
  if (chars.length !== word.length) return false;
  return chars.every((ch, i) => {
    const w = word[i]!;
    return asciiLower(ch) === w || (ch === "\u017f" && w === "s") || (ch === "\u212a" && w === "k");
  });
}

function tagIs(s: string, i: number, name: string): boolean {
  if (s.substr(i + 1, name.length) !== name) return false;
  const c = s[i + 1 + name.length];
  return c === undefined || c === ">" || c === "/" || c === " " || c === "\t" || c === "\n" || c === "\r" || c === "\f";
}

function tagEnd(s: string, i: number): number {
  let quote = "";
  for (let j = i + 1; j < s.length; j++) {
    const c = s[j];
    if (quote) {
      if (c === quote) quote = "";
    } else if (c === '"' || c === "'") {
      quote = c;
    } else if (c === ">") {
      return j + 1;
    }
  }
  return -1;
}

/** The attributes of a start tag: names lowercased, values raw (entity-encoded); the first of a name wins. */
function parseAttrs(s: string): Map<string, string> {
  const attrs = new Map<string, string>();
  const isSpace = (c: string | undefined) => c === " " || c === "\t" || c === "\n" || c === "\r" || c === "\f" || c === "/";
  const isWS = (c: string | undefined) => c === " " || c === "\t" || c === "\n" || c === "\r";
  const isWhitespace = (c: string | undefined) => isWS(c) || c === "\f";
  let i = 0;
  while (i < s.length) {
    while (i < s.length && isSpace(s[i])) i++;
    const start = i;
    while (i < s.length && !isSpace(s[i]) && s[i] !== "=") i++;
    const name = asciiLower(s.slice(start, i));
    while (i < s.length && isWS(s[i])) i++;
    let val = "";
    if (s[i] === "=") {
      i++;
      while (i < s.length && isWS(s[i])) i++;
      if (s[i] === '"' || s[i] === "'") {
        const q = s[i];
        i++;
        const vs = i;
        while (i < s.length && s[i] !== q) i++;
        val = s.slice(vs, i);
        if (i < s.length) i++;
      } else {
        // Unquoted values end at whitespace only: "/" is part of the value (content=https://example.com/x).
        const vs = i;
        while (i < s.length && !isWhitespace(s[i])) i++;
        val = s.slice(vs, i);
      }
    }
    if (name === "") {
      if (i === start) i++;
      continue;
    }
    if (!attrs.has(name)) attrs.set(name, val);
  }
  return attrs;
}

/** Go's cleanText: invalid bytes become U+FFFD (a run of them one), references are decoded, whitespace collapsed, the length capped. */
function cleanText(s: string, max: number): string {
  let t = unescapeHTML(s.replace(/[\udc80-\udcff]+/gu, "\ufffd"));
  t = t.split(SPACES_RE).filter(Boolean).join(" ");
  const chars = [...t];
  if (chars.length > max) t = chars.slice(0, max - 1).join("").replace(TRIM_RE, "") + "…";
  return t;
}

/**
 * Returns the title, the description and the og:image reference of an HTML
 * document (best effort; only the first 64 KiB count). The og:image is the
 * content of the first <meta> whose property attribute, or without one its
 * name attribute, is "og:image" (ASCII case-insensitive, trimmed). The scan
 * goes on until all three are found.
 */
export function extractPage(doc: Uint8Array | string): Page {
  const bytes = typeof doc === "string" ? utf8.encode(doc) : doc;
  const src = decodeBytes(bytes.subarray(0, TITLE_SCAN_BYTES));
  const lower = asciiLower(src);
  const page: Page = { title: "", description: "", image: "" };
  let haveTitle = false;
  let haveDesc = false;
  let haveImage = false;
  let i = 0;
  while (i < src.length && !(haveTitle && haveDesc && haveImage)) {
    const lt = lower.indexOf("<", i);
    if (lt < 0) break;
    i = lt;
    if (lower.startsWith("<!--", i)) {
      const end = lower.indexOf("-->", i + 4);
      if (end < 0) break;
      i = end + 3;
      continue;
    }
    if (tagIs(lower, i, "title")) {
      const start = tagEnd(lower, i);
      if (start < 0) break;
      let end = lower.indexOf("</title", start);
      if (end < 0) end = src.length;
      if (!haveTitle) {
        page.title = cleanText(src.slice(start, end), MAX_TITLE);
        haveTitle = true;
      }
      i = end;
      continue;
    }
    if (tagIs(lower, i, "meta")) {
      const end = tagEnd(lower, i);
      if (end < 0) break;
      if (!haveDesc || !haveImage) {
        const attrs = parseAttrs(src.slice(i + 5, end - 1));
        if (!haveDesc && equalFoldASCII((attrs.get("name") ?? "").replace(TRIM_RE, ""), "description")) {
          page.description = cleanText(attrs.get("content") ?? "", MAX_DESCRIPTION);
          haveDesc = true;
        }
        const key = attrs.has("property") ? attrs.get("property")! : (attrs.get("name") ?? "");
        if (!haveImage && asciiLower(trimASCIISpace(key)) === "og:image") {
          page.image = trimASCIISpace(unescapeHTML(attrs.get("content") ?? ""));
          haveImage = true;
        }
      }
      i = end;
      continue;
    }
    let skipped = false;
    let stop = false;
    for (const el of RAW_TEXT) {
      if (!tagIs(lower, i, el)) continue;
      const start = tagEnd(lower, i);
      if (start < 0) {
        stop = true;
        break;
      }
      if ((el === "svg" || el === "math") && start >= 2 && lower[start - 2] === "/") {
        i = start;
        skipped = true;
        break;
      }
      const end = lower.indexOf("</" + el, start);
      if (end < 0) {
        stop = true;
        break;
      }
      i = end + 2 + el.length;
      skipped = true;
      break;
    }
    if (stop) break;
    if (!skipped) i++;
  }
  return page;
}
