/**
 * Extracts <title> and <meta name="description"> from an HTML document
 * (port of internal/sites/title.go): comments and raw-text elements are
 * skipped, entities decoded, whitespace collapsed, lengths capped.
 */

/** Only the first 64 KiB of index.html are scanned. */
export const TITLE_SCAN_BYTES = 64 * 1024;

const MAX_TITLE = 256;
const MAX_DESCRIPTION = 1024;
const RAW_TEXT = ["script", "style", "svg", "template", "textarea", "noscript", "math"];

function asciiLower(s: string): string {
  return s.replace(/[A-Z]/g, (c) => String.fromCharCode(c.charCodeAt(0) + 32));
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

function parseAttrs(s: string): Record<string, string> {
  const attrs: Record<string, string> = {};
  const isSpace = (c: string | undefined) => c === " " || c === "\t" || c === "\n" || c === "\r" || c === "\f" || c === "/";
  const isWS = (c: string | undefined) => c === " " || c === "\t" || c === "\n" || c === "\r";
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
        const vs = i;
        while (i < s.length && !isSpace(s[i])) i++;
        val = s.slice(vs, i);
      }
    }
    if (name === "") {
      if (i === start) i++;
      continue;
    }
    if (!(name in attrs)) attrs[name] = val;
  }
  return attrs;
}

const ENTITIES: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: " ", copy: "©", reg: "®", mdash: "—", ndash: "–", hellip: "…" };

function unescapeHTML(s: string): string {
  return s.replace(/&(#x[0-9a-fA-F]+|#[0-9]+|[A-Za-z]+);/g, (m, e: string) => {
    if (e[0] === "#") {
      const cp = e[1] === "x" || e[1] === "X" ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10);
      return Number.isFinite(cp) && cp > 0 && cp <= 0x10ffff ? String.fromCodePoint(cp) : m;
    }
    return ENTITIES[e] ?? m;
  });
}

function cleanText(s: string, max: number): string {
  let t = unescapeHTML(s).split(/\s+/).filter(Boolean).join(" ");
  const chars = [...t];
  if (chars.length > max) t = chars.slice(0, max - 1).join("").trim() + "…";
  return t;
}

/** Returns the title and description of an HTML document (best effort). */
export function extractTitle(doc: Uint8Array | string): { title: string; description: string } {
  let src = typeof doc === "string" ? doc : new TextDecoder().decode(doc.subarray(0, TITLE_SCAN_BYTES));
  if (src.length > TITLE_SCAN_BYTES) src = src.slice(0, TITLE_SCAN_BYTES);
  const lower = asciiLower(src);
  let title = "";
  let description = "";
  let haveTitle = false;
  let haveDesc = false;
  let i = 0;
  while (i < src.length && !(haveTitle && haveDesc)) {
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
        title = cleanText(src.slice(start, end), MAX_TITLE);
        haveTitle = true;
      }
      i = end;
      continue;
    }
    if (tagIs(lower, i, "meta")) {
      const end = tagEnd(lower, i);
      if (end < 0) break;
      if (!haveDesc) {
        const attrs = parseAttrs(src.slice(i + 5, end - 1));
        if ((attrs.name ?? "").trim().toLowerCase() === "description") {
          description = cleanText(attrs.content ?? "", MAX_DESCRIPTION);
          haveDesc = true;
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
  return { title, description };
}
