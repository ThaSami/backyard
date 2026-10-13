/** Validation of deployed file paths (SPEC §7.3) and uploaded file names (§5.3). */

/** Longest site-relative path accepted. */
export const MAX_PATH_BYTES = 1024;

const enc = new TextEncoder();

/** Reports why a site-relative path is unacceptable, or "" when it is valid. */
export function validatePath(p: unknown): string {
  if (typeof p !== "string" || p === "") return "empty path";
  if (enc.encode(p).length > MAX_PATH_BYTES) return "path longer than 1024 bytes";
  if (/[\ud800-\udfff]/u.test(p.replace(/[\ud800-\udbff][\udc00-\udfff]/g, ""))) return "path is not valid UTF-8";
  if (p[0] === "/") return "path must be relative (no leading /)";
  if (p.includes("\\")) return 'path may not contain "\\"';
  if (p === "__backyard" || p.startsWith("__backyard/")) return 'path may not start with "__backyard/"';
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(p)) return "path contains a control character";
  for (const seg of p.split("/")) {
    if (seg === "") return "path contains an empty segment";
    if (seg === "." || seg === "..") return 'path may not contain "." or ".." segments';
  }
  return "";
}

const MAX_FILE_NAME = 200;

/**
 * Makes a client-supplied upload name safe for keys, URLs and
 * Content-Disposition (port of the Go sanitizeFileName): base name only, no
 * control or separator characters, bounded length (keeping the extension).
 */
export function sanitizeFileName(name: string): string {
  let n = name.replace(/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/g, "");
  const cut = Math.max(n.lastIndexOf("/"), n.lastIndexOf("\\"));
  if (cut >= 0) n = n.slice(cut + 1);
  let out = "";
  for (const ch of n) {
    const cp = ch.codePointAt(0)!;
    // eslint-disable-next-line no-control-regex
    if (/[\u0000-\u001f\u007f-\u009f"<>|*?: ‎‏‪-‮⁦-⁩؜]/.test(ch) || cp === 0xfffd) {
      out += "_";
    } else if (/\s/.test(ch)) {
      out += " ";
    } else {
      out += ch;
    }
  }
  n = out.trim().replace(/^[. ]+|[. ]+$/g, "");
  if (enc.encode(n).length > MAX_FILE_NAME) {
    const dot = n.lastIndexOf(".");
    let ext = dot > 0 ? n.slice(dot) : "";
    if (enc.encode(ext).length > 20) ext = "";
    let stem = ext ? n.slice(0, n.length - ext.length) : n;
    while (enc.encode(stem).length > MAX_FILE_NAME - enc.encode(ext).length) stem = [...stem].slice(0, -1).join("");
    n = stem.replace(/[. ]+$/, "") + ext;
  }
  if (n === "" || n === "." || n === "..") n = "file";
  return n;
}

/** Owner identifiers (SPEC §7.2): trimmed, lowercased, no spaces/commas. */
export function normalizeOwners(input: unknown, max = 100): string[] {
  if (input === undefined || input === null) return [];
  if (!Array.isArray(input)) throw new Error("owner lists must be arrays of strings");
  if (input.length > max) throw new Error(`too many owners in one request (max ${max})`);
  return input.map((o) => {
    if (typeof o !== "string") throw new Error("owners must be strings");
    const v = o.trim().toLowerCase();
    // eslint-disable-next-line no-control-regex
    if (v === "" || v.length > 254 || /[\u0000- \u007f,]/.test(v)) throw new Error(`invalid owner ${JSON.stringify(v.slice(0, 100))} (want an email or user id)`);
    return v;
  });
}
