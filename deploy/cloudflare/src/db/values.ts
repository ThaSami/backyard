/**
 * Canonical document values (port of internal/docstore/internal/dsutil
 * Canonical and docstore.CleanString / CleanNumber), so that the Cloudflare
 * edition stores, returns and rejects exactly what every Go backend does:
 *
 *   - strings and object keys containing U+0000 are rejected (PostgreSQL
 *     cannot store them, so no backend accepts them);
 *   - lone UTF-16 surrogates (JSON escapes such as "\ud800") become U+FFFD,
 *     like encoding/json decodes them;
 *   - numbers must be finite (JSON.parse turns 1e400 into Infinity, which
 *     JSON cannot represent) and -0 becomes 0;
 *   - bodies nested deeper than MAX_NESTING_DEPTH levels are rejected (the
 *     body is level 1), which keeps every document inside what Durable
 *     Object SQLite's JSON functions can evaluate.
 *
 * Violations are 400 bad_request errors (docstore.ErrInvalid in Go).
 */
import { HttpError } from "../http";

/** Maximum nesting depth of a stored document (docstore.MaxNestingDepth). */
export const MAX_NESTING_DEPTH = 100;

/** The message of a string with U+0000 (as in docstore.CleanString). */
export const NUL_MESSAGE = "strings must not contain the NUL character (U+0000)";

/** A value-level violation, wrapped with the offending field by the callers. */
class ValueError extends Error {}

function invalid(msg: string): HttpError {
  return new HttpError(400, "bad_request", msg);
}

/** Canonical form of a string (docstore.CleanString). */
function cleanString(s: string): string {
  if (s.includes("\u0000")) throw new ValueError(NUL_MESSAGE);
  return s.isWellFormed() ? s : s.toWellFormed();
}

/** Canonical form of a number (docstore.CleanNumber). */
function cleanNumber(n: number): number {
  if (!Number.isFinite(n)) throw new ValueError(`number ${n} is not valid JSON`);
  return n === 0 ? 0 : n; // also turns -0 into 0
}

/** Defines (not assigns) a key, so "__proto__" is data rather than the prototype. */
function define(obj: Record<string, unknown>, key: string, value: unknown): void {
  Object.defineProperty(obj, key, { value, enumerable: true, writable: true, configurable: true });
}

/**
 * Canonicalizes v found at nesting level depth (the document body is level
 * 1). Values JSON cannot carry follow JSON.stringify: undefined object
 * members are dropped and undefined array elements become null.
 */
function canonicalValue(v: unknown, depth: number): unknown {
  if (v === null || typeof v === "boolean") return v;
  if (typeof v === "string") return cleanString(v);
  if (typeof v === "number") return cleanNumber(v);
  if (typeof v !== "object") throw new ValueError(`a ${typeof v} is not a JSON value`);
  if (depth > MAX_NESTING_DEPTH) throw new ValueError(`nested deeper than ${MAX_NESTING_DEPTH} levels`);
  if (Array.isArray(v)) {
    const out: unknown[] = new Array<unknown>(v.length);
    for (let i = 0; i < v.length; i++) {
      const e: unknown = v[i];
      out[i] = e === undefined ? null : canonicalValue(e, depth + 1);
    }
    return out;
  }
  const out: Record<string, unknown> = {};
  for (const [k, e] of Object.entries(v as Record<string, unknown>)) {
    if (e === undefined) continue;
    define(out, cleanString(k), canonicalValue(e, depth + 1));
  }
  return out;
}

/**
 * Returns a canonical deep copy of a document write body (or of a PATCH
 * body, whose null values stay deletion markers): the reserved keys "id" and
 * "_meta" are dropped, values are checked and normalized as described above.
 * Throws a 400 HttpError naming the offending field.
 */
export function canonicalDoc(data: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(data)) {
    if (k === "id" || k === "_meta" || v === undefined) continue;
    let key: string;
    try {
      key = cleanString(k);
    } catch (e) {
      throw invalid(`field name ${JSON.stringify(k)}: ${(e as Error).message}`);
    }
    try {
      define(out, key, canonicalValue(v, 2));
    } catch (e) {
      if (e instanceof ValueError) throw invalid(`field ${JSON.stringify(key)}: ${e.message}`);
      throw e;
    }
  }
  return out;
}

/**
 * Canonicalizes a scalar filter operand like stored data (the cleanOperand
 * step of docstore.PrepareQuery): strings with U+0000 and non-finite numbers
 * are rejected, lone surrogates become U+FFFD and -0 becomes 0. Throws a 400
 * HttpError naming the field.
 */
export function cleanOperand(field: string, v: unknown): unknown {
  try {
    if (typeof v === "string") return cleanString(v);
    if (typeof v === "number") return cleanNumber(v);
    return v;
  } catch (e) {
    if (e instanceof ValueError) throw invalid(`filter on ${JSON.stringify(field)}: ${e.message}`);
    throw e;
  }
}
