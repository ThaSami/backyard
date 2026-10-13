/**
 * Document queries (SPEC §5.2): validation/normalisation (a port of
 * docstore.Query.Normalize) and translation into Durable Object SQLite with
 * exactly the semantics of the reference evaluator (internal/docstore/eval.go):
 *
 *   - comparisons only match values of the same JSON type ("3" never equals 3);
 *     json_type() guards every comparison;
 *   - a missing field never matches; "== null" matches an explicit null only;
 *     "<="/">=" null match null, "<"/">"/"!=" null match nothing;
 *   - "in" = any ==, "not-in" = all != (so all values must share the field's type);
 *   - "contains" = the field is an array with an element of the same type equal to the value;
 *   - ordering ranks missing/null < boolean < number < string < array < object, then
 *     numbers numerically, strings by bytes, false < true; descending reverses
 *     everything; ties are broken by id ascending.
 *
 * User input never becomes SQL text: field paths are validated against
 * ^[A-Za-z_][A-Za-z0-9_-]*$ segments before they are inlined, every value is a
 * bound parameter (lists are bound as one JSON array and expanded with
 * json_each, which keeps a query under the 100-parameter limit of Durable
 * Object SQLite).
 */
import { HttpError } from "../http";
import { parseRFC3339Micros } from "./time";
import { cleanOperand } from "./values";

export const COLLECTION_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;
export const SYSTEM_COLLECTION_RE = /^_[A-Za-z0-9][A-Za-z0-9_-]{0,62}$/;
export const DOC_ID_RE = /^[A-Za-z0-9_\-:.]{1,128}$/;
const SEGMENT_RE = /^[A-Za-z_][A-Za-z0-9_-]{0,63}$/;

/** Addressable non-data fields and their SQL columns. */
export const SYSTEM_COLUMNS: Record<string, string> = {
  id: "id",
  "_meta.created_at": "created_at",
  "_meta.updated_at": "updated_at",
  "_meta.created_by": "created_by",
  "_meta.updated_by": "updated_by",
  "_meta.version": "version",
};

type Kind = "null" | "bool" | "number" | "string" | "time" | "invalid";

const SYSTEM_KINDS: Record<string, Kind> = {
  id: "string",
  "_meta.created_by": "string",
  "_meta.updated_by": "string",
  "_meta.created_at": "time",
  "_meta.updated_at": "time",
  "_meta.version": "number",
};

export type Op = "==" | "!=" | "<" | "<=" | ">" | ">=" | "in" | "not-in" | "contains";
const OPS = new Set<string>(["==", "!=", "<", "<=", ">", ">=", "in", "not-in", "contains"]);
const SQL_OPS: Record<string, string> = { "==": "=", "!=": "<>", "<": "<", "<=": "<=", ">": ">", ">=": ">=" };

export interface Filter {
  field: string;
  op: Op;
  value: unknown;
}

export interface Order {
  field: string;
  desc: boolean;
}

/** A validated query with defaults applied and the id tiebreaker appended. */
export interface Query {
  where: Filter[];
  orderBy: Order[];
  limit: number;
  offset: number;
}

function invalid(msg: string): HttpError {
  return new HttpError(400, "bad_request", msg);
}

/** Validates a user collection name (names starting with "_" are reserved). */
export function collectionError(name: string): string {
  if (name.startsWith("_")) return "collection names starting with '_' are reserved for the system";
  if (!COLLECTION_RE.test(name)) {
    return `collection name ${JSON.stringify(name)} (letters, digits, '_' and '-', max 64, must not start with '_')`;
  }
  return "";
}

/** Validates a document id. */
export function docIDError(id: string): string {
  return DOC_ID_RE.test(id) ? "" : `document id ${JSON.stringify(id)}`;
}

/** Validates a dot-path field reference (user data path or system field). */
export function fieldError(field: unknown): string {
  if (typeof field !== "string") return "field must be a string";
  if (field in SYSTEM_COLUMNS) return "";
  const segs = field.split(".");
  if (segs.length > 8) return `field path ${JSON.stringify(field)} too deep`;
  for (const s of segs) if (!SEGMENT_RE.test(s)) return `field path ${JSON.stringify(field)}`;
  if (segs[0] === "_meta" || segs[0] === "id") return `field path ${JSON.stringify(field)}`;
  return "";
}

/** JSON scalars (non-finite numbers pass here and are rejected by cleanOperand, as in Go). */
function isScalar(v: unknown): boolean {
  return v === null || typeof v === "string" || typeof v === "boolean" || typeof v === "number";
}

function isPlain(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function intField(v: unknown, name: string): number {
  if (v === undefined || v === null) return 0;
  if (typeof v !== "number" || !Number.isInteger(v)) throw invalid(`invalid JSON: field "${name}" must be an integer`);
  return v;
}

/** Raw query input before normalisation. */
export interface RawQuery {
  where?: { field: string; op: string; value: unknown }[];
  orderBy?: { field: string; dir?: string }[];
  limit?: number;
  offset?: number;
}

/**
 * Decodes a POST /db/{c}/query body strictly (unknown fields are rejected,
 * like the Go server's DisallowUnknownFields).
 */
export function decodeQueryBody(body: unknown): RawQuery {
  if (!isPlain(body)) throw invalid("request body must be a JSON object");
  for (const k of Object.keys(body)) {
    if (!["where", "order_by", "limit", "offset"].includes(k)) throw invalid(`invalid JSON: unknown field ${JSON.stringify(k)}`);
  }
  const out: RawQuery = {};
  if (body.where !== undefined && body.where !== null) {
    if (!Array.isArray(body.where)) throw invalid('invalid JSON: field "where" must be an array');
    out.where = body.where.map((f) => {
      if (!isPlain(f)) throw invalid("invalid JSON: each where entry must be an object");
      for (const k of Object.keys(f)) {
        if (!["field", "op", "value"].includes(k)) throw invalid(`invalid JSON: unknown field ${JSON.stringify(k)}`);
      }
      if (f.field !== undefined && typeof f.field !== "string") throw invalid('invalid JSON: field "field" must be a string');
      if (f.op !== undefined && typeof f.op !== "string") throw invalid('invalid JSON: field "op" must be a string');
      return { field: (f.field as string) ?? "", op: (f.op as string) ?? "", value: f.value ?? null };
    });
  }
  if (body.order_by !== undefined && body.order_by !== null) {
    if (!Array.isArray(body.order_by)) throw invalid('invalid JSON: field "order_by" must be an array');
    out.orderBy = body.order_by.map((o) => {
      if (!isPlain(o)) throw invalid("invalid JSON: each order_by entry must be an object");
      for (const k of Object.keys(o)) {
        if (!["field", "dir"].includes(k)) throw invalid(`invalid JSON: unknown field ${JSON.stringify(k)}`);
      }
      if (o.field !== undefined && typeof o.field !== "string") throw invalid('invalid JSON: field "field" must be a string');
      if (o.dir !== undefined && o.dir !== null && typeof o.dir !== "string") throw invalid('invalid JSON: field "dir" must be a string');
      return { field: (o.field as string) ?? "", dir: (o.dir as string | undefined) ?? "" };
    });
  }
  out.limit = intField(body.limit, "limit");
  out.offset = intField(body.offset, "offset");
  return out;
}

/**
 * Validates q and applies defaults (limit, default order, id tiebreaker), like
 * docstore.Query.Normalize followed by PrepareQuery: filter operands are
 * canonicalized like stored data (cleanOperand), so a string with U+0000 or a
 * non-finite number is a 400 rather than a filter that silently never matches.
 */
export function normalizeQuery(q: RawQuery, maxList: number): Query {
  if (maxList <= 0) maxList = 1000;
  const where: Filter[] = [];
  for (const f of q.where ?? []) {
    const fe = fieldError(f.field);
    if (fe) throw invalid(fe);
    if (!OPS.has(f.op)) throw invalid(`unknown operator ${JSON.stringify(f.op)}`);
    const op = f.op as Op;
    if (op === "in" || op === "not-in") {
      const arr = f.value;
      if (!Array.isArray(arr) || arr.length === 0 || arr.length > 30) throw invalid(`value for "${op}" must be an array of 1-30 scalars`);
      for (const v of arr) if (!isScalar(v)) throw invalid(`value for "${op}" must be an array of scalars`);
    } else if (!isScalar(f.value)) {
      throw invalid(`value for "${op}" must be a scalar`);
    }
    where.push({ field: f.field, op, value: f.value });
  }
  if (where.length > 20) throw invalid("at most 20 filters");
  const orderBy: Order[] = [];
  for (const o of q.orderBy ?? []) {
    const fe = fieldError(o.field);
    if (fe) throw invalid(fe);
    const dir = (o.dir ?? "").toLowerCase();
    if (dir !== "" && dir !== "asc" && dir !== "desc") throw invalid("order dir must be asc or desc");
    orderBy.push({ field: o.field, desc: dir === "desc" });
  }
  if (orderBy.length > 5) throw invalid("at most 5 order_by fields");
  if (orderBy.length === 0) orderBy.push({ field: "_meta.created_at", desc: false });
  if (orderBy[orderBy.length - 1]!.field !== "id") orderBy.push({ field: "id", desc: false });
  let limit = q.limit ?? 0;
  if (limit <= 0) limit = 100;
  if (limit > maxList) limit = maxList;
  const offset = q.offset ?? 0;
  if (offset < 0) throw invalid("offset must be >= 0");
  if (offset > 100_000) throw invalid("offset too large");
  for (const f of where) {
    f.value = Array.isArray(f.value) ? f.value.map((v) => cleanOperand(f.field, v)) : cleanOperand(f.field, f.value);
  }
  return { where, orderBy, limit, offset };
}

// --- SQL translation ---------------------------------------------------------

/** The kind of a filter operand for field (timestamps parse RFC 3339). */
function operand(field: string, v: unknown): { kind: Kind; value: unknown } {
  if (field === "_meta.created_at" || field === "_meta.updated_at") {
    const us = parseRFC3339Micros(v);
    return us === null ? { kind: "invalid", value: null } : { kind: "time", value: us };
  }
  if (v === null) return { kind: "null", value: null };
  switch (typeof v) {
    case "boolean":
      return { kind: "bool", value: v };
    case "number":
      return Number.isFinite(v) ? { kind: "number", value: v } : { kind: "invalid", value: null };
    case "string":
      return { kind: "string", value: v };
  }
  return { kind: "invalid", value: null };
}

/** SQLite JSON path literal for a validated dot path: '$."a"."b-c"'. */
export function jsonPath(field: string): string {
  return "'$" + field.split(".").map((s) => `."${s}"`).join("") + "'";
}

class Builder {
  sql = "";
  params: unknown[] = [];
  w(s: string): void {
    this.sql += s;
  }
  p(v: unknown): string {
    this.params.push(v);
    return "?";
  }
}

const FALSE = "0";
const NUM_TYPES = "('integer', 'real')";

function dataCompare(b: Builder, path: string, field: string, op: Op, v: unknown): void {
  const { kind, value } = operand(field, v);
  const sqlOp = SQL_OPS[op]!;
  const jt = `json_type(data, ${path})`;
  const jx = `json_extract(data, ${path})`;
  switch (kind) {
    case "null":
      b.w(op === "==" || op === "<=" || op === ">=" ? `${jt} = 'null'` : FALSE);
      return;
    case "bool": {
      const bv = value as boolean;
      if (op === "==") b.w(`${jt} = '${bv ? "true" : "false"}'`);
      else if (op === "!=") b.w(`${jt} = '${bv ? "false" : "true"}'`);
      else b.w(`(${jt} IN ('true', 'false') AND ${jx} ${sqlOp} ${bv ? 1 : 0})`);
      return;
    }
    case "number":
      b.w(`(${jt} IN ${NUM_TYPES} AND ${jx} ${sqlOp} ${b.p(value)})`);
      return;
    case "string":
      b.w(`(${jt} = 'text' AND ${jx} ${sqlOp} ${b.p(value)})`);
      return;
  }
  b.w(FALSE);
}

/** Groups list values by kind (invalid kinds are kept as a flag). */
function groupValues(field: string, values: unknown[]) {
  const g = { nulls: false, bools: new Set<boolean>(), numbers: [] as number[], strings: [] as string[], times: [] as number[], invalid: false };
  for (const v of values) {
    const { kind, value } = operand(field, v);
    switch (kind) {
      case "null":
        g.nulls = true;
        break;
      case "bool":
        g.bools.add(value as boolean);
        break;
      case "number":
        g.numbers.push(value as number);
        break;
      case "string":
        g.strings.push(value as string);
        break;
      case "time":
        g.times.push(value as number);
        break;
      default:
        g.invalid = true;
    }
  }
  return g;
}

function dataIn(b: Builder, path: string, field: string, values: unknown[]): void {
  const g = groupValues(field, values);
  const jt = `json_type(data, ${path})`;
  const jx = `json_extract(data, ${path})`;
  const terms: string[] = [];
  if (g.nulls) terms.push(`${jt} = 'null'`);
  if (g.bools.size) terms.push(`${jt} IN (${[...g.bools].map((x) => `'${x ? "true" : "false"}'`).join(", ")})`);
  if (g.numbers.length) terms.push(`(${jt} IN ${NUM_TYPES} AND ${jx} IN (SELECT value FROM json_each(${b.p(JSON.stringify(g.numbers))})))`);
  if (g.strings.length) terms.push(`(${jt} = 'text' AND ${jx} IN (SELECT value FROM json_each(${b.p(JSON.stringify(g.strings))})))`);
  b.w(terms.length ? `(${terms.join(" OR ")})` : FALSE);
}

function dataNotIn(b: Builder, path: string, field: string, values: unknown[]): void {
  const g = groupValues(field, values);
  const kinds = (g.nulls ? 1 : 0) + (g.bools.size ? 1 : 0) + (g.numbers.length ? 1 : 0) + (g.strings.length ? 1 : 0) + (g.times.length ? 1 : 0);
  // != only holds between values of the same type, so a mixed list (or null,
  // which is never != null) matches nothing.
  if (g.invalid || kinds !== 1 || g.nulls) {
    b.w(FALSE);
    return;
  }
  const jt = `json_type(data, ${path})`;
  const jx = `json_extract(data, ${path})`;
  if (g.bools.size) {
    const allowed = ["true", "false"].filter((t) => !g.bools.has(t === "true"));
    b.w(allowed.length ? `${jt} IN (${allowed.map((t) => `'${t}'`).join(", ")})` : FALSE);
  } else if (g.numbers.length) {
    b.w(`(${jt} IN ${NUM_TYPES} AND ${jx} NOT IN (SELECT value FROM json_each(${b.p(JSON.stringify(g.numbers))})))`);
  } else if (g.strings.length) {
    b.w(`(${jt} = 'text' AND ${jx} NOT IN (SELECT value FROM json_each(${b.p(JSON.stringify(g.strings))})))`);
  } else {
    b.w(FALSE);
  }
}

function dataContains(b: Builder, path: string, field: string, v: unknown): void {
  const { kind, value } = operand(field, v);
  let cond: string;
  switch (kind) {
    case "null":
      cond = "e.type = 'null'";
      break;
    case "bool":
      cond = `e.type = '${value ? "true" : "false"}'`;
      break;
    case "number":
      cond = `e.type IN ${NUM_TYPES} AND e.value = ${b.p(value)}`;
      break;
    case "string":
      cond = `e.type = 'text' AND e.value = ${b.p(value)}`;
      break;
    default:
      b.w(FALSE);
      return;
  }
  b.w(`(json_type(data, ${path}) = 'array' AND EXISTS (SELECT 1 FROM json_each(docs.data, ${path}) AS e WHERE ${cond}))`);
}

function systemFilter(b: Builder, col: string, f: Filter): void {
  const colKind = SYSTEM_KINDS[f.field]!;
  const cmp = (op: Op, v: unknown) => {
    const { kind, value } = operand(f.field, v);
    if (kind !== colKind) {
      b.w(FALSE);
      return;
    }
    b.w(`${col} ${SQL_OPS[op]} ${b.p(value)}`);
  };
  switch (f.op) {
    case "==":
    case "!=":
    case "<":
    case "<=":
    case ">":
    case ">=":
      cmp(f.op, f.value);
      return;
    case "in":
    case "not-in": {
      const vals: unknown[] = [];
      let mismatch = false;
      for (const v of f.value as unknown[]) {
        const { kind, value } = operand(f.field, v);
        if (kind === colKind) vals.push(value);
        else mismatch = true;
      }
      if (f.op === "in") {
        b.w(vals.length ? `${col} IN (SELECT value FROM json_each(${b.p(JSON.stringify(vals))}))` : FALSE);
      } else {
        b.w(!mismatch && vals.length ? `${col} NOT IN (SELECT value FROM json_each(${b.p(JSON.stringify(vals))}))` : FALSE);
      }
      return;
    }
    default:
      // contains: system fields are never arrays.
      b.w(FALSE);
  }
}

function filterSQL(b: Builder, f: Filter): void {
  const col = SYSTEM_COLUMNS[f.field];
  if (col) {
    systemFilter(b, col, f);
    return;
  }
  const path = jsonPath(f.field);
  switch (f.op) {
    case "in":
      dataIn(b, path, f.field, f.value as unknown[]);
      return;
    case "not-in":
      dataNotIn(b, path, f.field, f.value as unknown[]);
      return;
    case "contains":
      dataContains(b, path, f.field, f.value);
      return;
    default:
      dataCompare(b, path, f.field, f.op, f.value);
  }
}

function orderSQL(o: Order): string {
  const dir = o.desc ? " DESC" : " ASC";
  const col = SYSTEM_COLUMNS[o.field];
  if (col) return col + dir;
  const path = jsonPath(o.field);
  const jt = `json_type(data, ${path})`;
  return (
    `CASE ${jt} WHEN 'true' THEN 1 WHEN 'false' THEN 1 WHEN 'integer' THEN 2 WHEN 'real' THEN 2 ` +
    `WHEN 'text' THEN 3 WHEN 'array' THEN 4 WHEN 'object' THEN 5 ELSE 0 END${dir}, ` +
    `CASE WHEN ${jt} IN ('true', 'false', 'integer', 'real', 'text') THEN json_extract(data, ${path}) END${dir}`
  );
}

/** Columns selected for a document row. */
export const DOC_COLUMNS = "id, data, created_at, updated_at, created_by, updated_by, version";

/**
 * Returns the SELECT for q on collection. It fetches limit+1 rows so the
 * caller can report has_more.
 */
export function buildQuery(collection: string, q: Query): { sql: string; params: unknown[] } {
  const b = new Builder();
  b.w(`SELECT ${DOC_COLUMNS} FROM docs WHERE collection = ${b.p(collection)}`);
  for (const f of q.where) {
    b.w(" AND ");
    filterSQL(b, f);
  }
  b.w(" ORDER BY " + q.orderBy.map(orderSQL).join(", "));
  b.w(` LIMIT ${Math.floor(q.limit) + 1} OFFSET ${Math.floor(q.offset)}`);
  return { sql: b.sql, params: b.params };
}

/** Parses GET /db/{c}?limit=&offset=&order=field:asc,field2:desc (also orderBy=&order=asc|desc). */
export function parseListParams(sp: URLSearchParams): RawQuery {
  const q: RawQuery = {};
  const num = (name: string): number | undefined => {
    const s = sp.get(name);
    if (s === null || s === "") return undefined;
    if (!/^\d+$/.test(s)) throw invalid(`${name} must be a non-negative integer`);
    const n = Number(s);
    if (!Number.isSafeInteger(n)) throw invalid(`${name} must be a non-negative integer`);
    return n;
  };
  const limit = num("limit");
  const offset = num("offset");
  if (limit !== undefined) q.limit = limit;
  if (offset !== undefined) q.offset = offset;
  let spec = (sp.get("order") ?? "").trim();
  let defDir = "";
  const ob = (sp.get("orderBy") || sp.get("order_by") || "").trim();
  if (ob) {
    defDir = (sp.get("dir") || spec).trim().toLowerCase();
    spec = ob;
  }
  if (defDir !== "" && defDir !== "asc" && defDir !== "desc") throw invalid(`order direction must be asc or desc, got ${JSON.stringify(defDir)}`);
  if (spec) {
    q.orderBy = [];
    for (let item of spec.split(",")) {
      item = item.trim();
      if (!item) continue;
      const colon = item.indexOf(":");
      const field = (colon < 0 ? item : item.slice(0, colon)).trim();
      let dir = (colon < 0 ? "" : item.slice(colon + 1)).trim().toLowerCase();
      if (!dir) dir = defDir;
      if (dir !== "" && dir !== "asc" && dir !== "desc") throw invalid(`order direction must be asc or desc, got ${JSON.stringify(dir)}`);
      if (!field) throw invalid("order field is empty");
      q.orderBy.push({ field, dir });
    }
  }
  return q;
}
