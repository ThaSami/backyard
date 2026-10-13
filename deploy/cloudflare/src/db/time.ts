/**
 * Timestamps on the wire, in the formats of the Go edition:
 *
 *   - document `_meta.created_at` / `_meta.updated_at` (and the other API
 *     timestamps) are integer microseconds since the epoch (the precision of
 *     every docstore backend) rendered like Go's time.Time JSON: RFC 3339 in
 *     UTC with the trailing zeros of the fraction removed
 *     ("2026-01-02T03:04:05.12Z"). Such strings do not sort chronologically
 *     as text, so the server compares and orders them as integers
 *     (db/query.ts) and filter values are parsed (parseRFC3339Micros);
 *   - realtime presence `joined_at` has a fixed width of exactly three
 *     fractional digits ("2026-01-02T03:04:05.120Z", internal/realtime's
 *     joinedAtLayout), so clients may order members by comparing strings.
 */

const pad = (n: number, w = 2) => String(n).padStart(w, "0");

/** "YYYY-MM-DDTHH:MM:SS" of a Date in UTC (years outside 0..9999 unpadded, like Go). */
function utcSeconds(d: Date): string {
  const y = d.getUTCFullYear();
  const year = y >= 0 && y <= 9999 ? pad(y, 4) : String(y);
  return `${year}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}`;
}

/** Formats microseconds since the epoch as RFC 3339 in UTC (Go RFC3339Nano style). */
export function formatMicros(us: number): string {
  const ms = Math.floor(us / 1000);
  const frac = us - ms * 1000; // 0..999 microseconds beyond the millisecond
  const d = new Date(ms);
  let s = utcSeconds(d);
  const micros = d.getUTCMilliseconds() * 1000 + frac;
  if (micros > 0) s += "." + pad(micros, 6).replace(/0+$/, "");
  return s + "Z";
}

/**
 * Formats milliseconds since the epoch as RFC 3339 in UTC with exactly three
 * fractional digits ("2026-01-02T03:04:05.120Z"): the fixed-width format of
 * presence joined_at, which orders chronologically as a string.
 */
export function formatMillisFixed(ms: number): string {
  const d = new Date(Math.floor(ms));
  return `${utcSeconds(d)}.${pad(d.getUTCMilliseconds(), 3)}Z`;
}

const FIXED_MILLIS_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;

/**
 * Returns an RFC 3339 timestamp in the fixed-width presence format, converting
 * other RFC 3339 forms (e.g. from connection state saved by an older version);
 * values that do not parse are returned unchanged.
 */
export function toFixedMillis(v: string): string {
  if (FIXED_MILLIS_RE.test(v)) return v;
  const us = parseRFC3339Micros(v);
  return us === null ? v : formatMillisFixed(Math.floor(us / 1000));
}

const RFC3339_RE = /^(\d{4})-(\d{2})-(\d{2})[Tt](\d{2}):(\d{2}):(\d{2})(?:[.,](\d+))?([Zz]|[+-]\d{2}:\d{2})$/;

const MONTH_DAYS = [31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];

function daysIn(year: number, month: number): number {
  if (month === 2) return (year % 4 === 0 && year % 100 !== 0) || year % 400 === 0 ? 29 : 28;
  return MONTH_DAYS[month - 1]!;
}

/**
 * Parses an RFC 3339 timestamp (optional fraction, Z or ±hh:mm offset) into
 * microseconds since the epoch, truncated to microseconds. Returns null for
 * anything else (such a filter value matches nothing).
 */
export function parseRFC3339Micros(v: unknown): number | null {
  if (typeof v !== "string") return null;
  const m = RFC3339_RE.exec(v);
  if (!m) return null;
  const year = Number(m[1]);
  const month = Number(m[2]);
  const day = Number(m[3]);
  const hour = Number(m[4]);
  const minute = Number(m[5]);
  const second = Number(m[6]);
  if (month < 1 || month > 12 || day < 1 || day > daysIn(year, month)) return null;
  if (hour > 23 || minute > 59 || second > 59) return null;
  let offsetMin = 0;
  const tz = m[8]!;
  if (tz !== "Z" && tz !== "z") {
    const oh = Number(tz.slice(1, 3));
    const om = Number(tz.slice(4, 6));
    if (oh > 23 || om > 59) return null;
    offsetMin = (oh * 60 + om) * (tz[0] === "-" ? -1 : 1);
  }
  const d = new Date(0);
  d.setUTCFullYear(year, month - 1, day);
  d.setUTCHours(hour, minute, second, 0);
  const ms = d.getTime() - offsetMin * 60_000;
  const fracDigits = (m[7] ?? "").slice(0, 6).padEnd(6, "0");
  return ms * 1000 + Number(fracDigits);
}

/** A strictly increasing microsecond clock (per Durable Object instance). */
export class MicroClock {
  private last = 0;

  /** Raises the floor (e.g. to the newest stored timestamp after a restart). */
  observe(us: number): void {
    if (us > this.last) this.last = us;
  }

  /** Current time in microseconds, strictly greater than every previous value. */
  now(): number {
    let t = Date.now() * 1000;
    if (t <= this.last) t = this.last + 1;
    this.last = t;
    return t;
  }
}
