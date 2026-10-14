// A small Cloudflare REST client (API v4) for the steps wrangler has no
// command for: the workers.dev subdomain, the live Worker settings, zones,
// custom domains, routes, DNS records and Cloudflare Access.
//
// * Base URL: CLOUDFLARE_API_BASE_URL (wrangler honours it too), default
//   https://api.cloudflare.com/client/v4. Plain http: only for loopback hosts.
// * Credentials: {type: "oauth"|"api_token", token} → Authorization: Bearer;
//   {type: "api_key", key, email} → X-Auth-Key/X-Auth-Email. They come from a
//   provider (wrangler auth token --json for wrangler's own login, or the admin
//   token), are cached, re-read on refresh() and once after a 401 or code 10000,
//   and never appear in an error message.
// * Unwraps {success, errors: [{code, message}], result, result_info}; list()
//   follows page/per_page pagination. A DELETE answered 2xx without a body is a
//   success (result null).
// * GET, PUT and DELETE retry 429 (honouring Retry-After) and 5xx up to 3 times.
//   POST is retried only when Cloudflare cannot have acted on it (401/10000
//   after a fresh credential, 429): after any other failure the error is
//   marked `ambiguous` and the caller looks the object up (postOrFind).
// * 403 and code 10000 errors name the API-token permission the request needs.

import { CliError, EXIT } from "./ui.mjs";
import { USER_AGENT, proxyHint } from "./net.mjs";

export const DEFAULT_API_BASE = "https://api.cloudflare.com/client/v4";

/** Loopback hosts that may be reached over plain http (tests, local proxies). */
const LOOPBACK = new Set(["127.0.0.1", "localhost", "[::1]", "::1"]);

/**
 * The API base URL (without a trailing slash). Throws CliError(2) for a
 * plain-http base on a non-loopback host: the credentials would travel in
 * clear text.
 * @param {NodeJS.ProcessEnv} [env]
 */
export function apiBase(env = process.env) {
  const raw = String(env.CLOUDFLARE_API_BASE_URL || DEFAULT_API_BASE).trim().replace(/\/+$/, "");
  let u;
  try {
    u = new URL(raw);
  } catch {
    throw new CliError(EXIT.USAGE, `CLOUDFLARE_API_BASE_URL is not a URL: ${JSON.stringify(raw)}`);
  }
  if (u.protocol === "http:" && !LOOPBACK.has(u.hostname)) {
    throw new CliError(EXIT.USAGE, `CLOUDFLARE_API_BASE_URL must use https (plain http only for localhost): ${JSON.stringify(raw)}`);
  }
  if (u.protocol !== "https:" && u.protocol !== "http:") throw new CliError(EXIT.USAGE, `CLOUDFLARE_API_BASE_URL must be an https URL`);
  return raw;
}

/**
 * The API-token permissions of each request (for 403 messages and the docs).
 * First match wins; paths are without the base and the query.
 */
export const PERMISSION_HINTS = [
  [/^\/accounts\/[^/]+\/workers\/subdomains?(\/|$)/, "Account · Workers Scripts · Edit"],
  [/^\/accounts\/[^/]+\/workers\/scripts\/[^/]+\/(settings|subdomain)$/, "Account · Workers Scripts · Read"],
  [/^\/zones$/, "Zone · Zone · Read"],
  [/^\/accounts\/[^/]+\/workers\/(scripts\/[^/]+\/)?domains(\/|$)/, 'the "Edit Cloudflare Workers" template (Account · Workers Scripts · Edit, Zone · Workers Routes · Edit, Zone · Zone · Read)'],
  [/^\/zones\/[^/]+\/workers\/routes(\/|$)/, "Zone · Workers Routes · Edit"],
  [/^\/zones\/[^/]+\/dns_records(\/|$)/, "Zone · DNS · Edit (that zone only)"],
  [/^\/accounts\/[^/]+\/access\/organizations(\/|$)/, "Account · Access: Organizations, Identity Providers, and Groups · Read"],
  [/^\/accounts\/[^/]+\/access\/(apps|policies)(\/|$)/, "Account · Access: Apps and Policies · Edit"],
];

/** The permission a request needs ("" when unknown). */
export function permissionFor(path) {
  const p = String(path).split("?")[0];
  for (const [re, perm] of PERMISSION_HINTS) if (re.test(p)) return perm;
  return "";
}

/**
 * A failed Cloudflare API request.
 */
export class CfError extends Error {
  /**
   * @param {{status: number, codes?: number[], message: string, method?: string, path?: string, permission?: string, ambiguous?: boolean}} o
   */
  constructor(o) {
    super(o.message);
    this.name = "CfError";
    /** HTTP status (0 for a network failure) */
    this.status = o.status;
    /** @type {number[]} Cloudflare error codes */
    this.codes = o.codes ?? [];
    this.method = o.method ?? "";
    this.path = o.path ?? "";
    /** the API-token permission the request needs, set for 403 / code 10000 */
    this.permission = o.permission ?? "";
    /** true when a POST may or may not have been applied */
    this.ambiguous = Boolean(o.ambiguous);
  }

  /** Whether one of the Cloudflare error codes is `code`. */
  has(code) {
    return this.codes.includes(code);
  }

  /** 403 or code 10000: the credential lacks a permission. */
  get forbidden() {
    return this.status === 403 || this.codes.includes(10000);
  }

  /** 404 (or Cloudflare's "not found" codes). */
  get notFound() {
    return this.status === 404;
  }
}

/**
 * HTTP headers for a credential.
 * @param {{type: string, token?: string, key?: string, email?: string}} cred
 */
export function credentialHeaders(cred) {
  if (!cred) return {};
  if (cred.type === "api_key") return { "X-Auth-Key": String(cred.key ?? ""), "X-Auth-Email": String(cred.email ?? "") };
  return { Authorization: `Bearer ${cred.token ?? ""}` };
}

/** Removes every occurrence of the credential's secret parts from a text. */
function redact(text, cred) {
  let s = String(text ?? "");
  for (const secret of [cred?.token, cred?.key]) {
    if (secret && String(secret).length >= 4) s = s.split(String(secret)).join("[redacted]");
  }
  return s;
}

const defaultSleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * @typedef {object} Credential
 * @property {"oauth"|"api_token"|"api_key"} type
 * @property {string} [token]
 * @property {string} [key]
 * @property {string} [email]
 *
 * @typedef {object} Api
 * @property {(path: string, query?: Record<string, any>) => Promise<any>} get       the unwrapped `result`
 * @property {(path: string, query?: Record<string, any>, o?: {perPage?: number, maxPages?: number}) => Promise<any[]>} list   every page
 * @property {(path: string, body?: any, query?: Record<string, any>) => Promise<any>} post
 * @property {(path: string, body?: any, query?: Record<string, any>) => Promise<any>} put
 * @property {(path: string, body?: any, query?: Record<string, any>) => Promise<any>} patch
 * @property {(path: string, query?: Record<string, any>) => Promise<any>} delete
 * @property {(method: string, path: string, o?: {query?: Record<string, any>, body?: any}) => Promise<{result: any, result_info: any, status: number}>} request   the whole envelope
 * @property {(path: string, body: any, find: () => Promise<any>) => Promise<any>} postOrFind
 *   POST; after an ambiguous failure, `find()` looks for the object (returned when found, else the error is thrown)
 * @property {() => void} refresh            re-read the credential before the next request (phase boundaries)
 * @property {() => Promise<string>} credentialType   "oauth" | "api_token" | "api_key"
 * @property {string} base
 * @property {string} label                  what this client is ("wrangler login", "admin token") for messages
 */

/**
 * Creates a client.
 * @param {{credential?: Credential, getCredential?: () => Promise<Credential>, base?: string, fetch?: typeof fetch, retries?: number, sleep?: (ms: number) => Promise<void>, timeoutMs?: number, label?: string}} o
 * @returns {Api}
 */
export function createApi(o) {
  const base = o.base ?? apiBase();
  const doFetch = o.fetch ?? ((...a) => globalThis.fetch(...a));
  const retries = o.retries ?? 3;
  const sleep = o.sleep ?? defaultSleep;
  const getCredential = o.getCredential ?? (async () => o.credential);
  /** @type {Credential | undefined} */
  let cred;
  let stale = true;
  const credential = async () => {
    if (stale || !cred) {
      cred = await getCredential();
      stale = false;
    }
    return cred;
  };

  const url = (path, query) => {
    const u = new URL(base + (path.startsWith("/") ? path : `/${path}`));
    for (const [k, v] of Object.entries(query ?? {})) if (v !== undefined && v !== null && v !== "") u.searchParams.set(k, String(v));
    return u;
  };

  async function once(method, path, query, body, c) {
    const u = url(path, query);
    const headers = { Accept: "application/json", "User-Agent": USER_AGENT, ...credentialHeaders(c) };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    let res;
    try {
      res = await doFetch(u.href, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        redirect: "manual",
        signal: AbortSignal.timeout(o.timeoutMs ?? 30_000),
      });
    } catch (err) {
      const why = err?.cause?.code || err?.cause?.message || err?.message || String(err);
      // Behind a proxy that Node's fetch ignores, say so (wrangler uses it, so everything else works).
      const hint = LOOPBACK.has(u.hostname) ? "" : proxyHint();
      const message = `${method} ${path}: ${why}${hint ? ` (${hint})` : ""}`;
      return { network: true, error: new CfError({ status: 0, message: redact(message, c), method, path, ambiguous: method === "POST" || method === "PATCH" }) };
    }
    let envelope;
    const text = await res.text().catch(() => "");
    // Detach Worker Domain (DELETE /accounts/{a}/workers/domains/{id}) answers 200
    // without a body (or 204): that is a success, though not an envelope.
    if (res.ok && method === "DELETE" && (res.status === 204 || text.trim() === "")) {
      return { ok: true, status: res.status, envelope: { success: true, result: null } };
    }
    try {
      envelope = JSON.parse(text);
    } catch {
      envelope = undefined;
    }
    // Number(null) and Number("") are 0: an absent header must mean "back off", not "retry now".
    const retryAfterHeader = res.headers.get("retry-after");
    const retryAfter = retryAfterHeader === null || retryAfterHeader.trim() === "" ? NaN : Number(retryAfterHeader);
    if (res.ok && envelope && envelope.success !== false) {
      return { ok: true, status: res.status, envelope };
    }
    const errors = Array.isArray(envelope?.errors) ? envelope.errors : [];
    const codes = errors.map((e) => Number(e?.code)).filter((n) => Number.isFinite(n));
    const detail = errors.map((e) => `${e?.message ?? "error"}${e?.code !== undefined ? ` [code: ${e.code}]` : ""}`).join("; ");
    const forbidden = res.status === 403 || codes.includes(10000);
    const permission = forbidden ? permissionFor(path) : "";
    let message = `${method} ${path} failed (${res.status}${detail ? `: ${detail}` : envelope ? "" : `, not a Cloudflare API response`})`;
    if (forbidden) {
      message += permission
        ? `: the credential (${o.label ?? "wrangler login"}) lacks a permission; an API token needs ${permission}`
        : `: the credential (${o.label ?? "wrangler login"}) is not allowed to do this`;
    }
    return {
      status: res.status,
      retryAfter: Number.isFinite(retryAfter) ? retryAfter : undefined,
      authFailed: res.status === 401 || codes.includes(10000),
      error: new CfError({
        status: res.status,
        codes,
        message: redact(message, c),
        method,
        path,
        permission,
        ambiguous: (method === "POST" || method === "PATCH") && res.status >= 500,
      }),
    };
  }

  async function request(method, path, { query, body } = {}) {
    const idempotent = method === "GET" || method === "PUT" || method === "DELETE";
    let refreshed = false;
    for (let attempt = 0; ; attempt++) {
      const c = await credential();
      const r = await once(method, path, query, body, c);
      if (r.ok) return { result: r.envelope.result, result_info: r.envelope.result_info, status: r.status };
      if (r.authFailed && !refreshed) {
        // An expired OAuth token: wrangler auth token refreshes it.
        refreshed = true;
        stale = true;
        continue;
      }
      const retryable = r.network ? idempotent : r.status === 429 || (idempotent && r.status >= 500);
      if (retryable && attempt < retries) {
        const wait = r.retryAfter !== undefined ? Math.min(60, Math.max(0, r.retryAfter)) * 1000 : 500 * 2 ** attempt;
        await sleep(wait);
        continue;
      }
      throw r.error;
    }
  }

  /** @type {Api} */
  const api = {
    base,
    label: o.label ?? "wrangler login",
    request,
    get: async (path, query) => (await request("GET", path, { query })).result,
    post: async (path, body, query) => (await request("POST", path, { query, body })).result,
    put: async (path, body, query) => (await request("PUT", path, { query, body })).result,
    patch: async (path, body, query) => (await request("PATCH", path, { query, body })).result,
    delete: async (path, query) => (await request("DELETE", path, { query })).result,
    async list(path, query = {}, lo = {}) {
      const perPage = lo.perPage ?? 50;
      const maxPages = lo.maxPages ?? 200;
      const out = [];
      for (let page = 1; page <= maxPages; page++) {
        const r = await request("GET", path, { query: { ...query, page, per_page: perPage } });
        const items = Array.isArray(r.result) ? r.result : [];
        out.push(...items);
        const info = r.result_info;
        if (info && Number.isFinite(Number(info.total_pages)) && Number(info.total_pages) > 0) {
          if (page >= Number(info.total_pages)) break;
        } else if (items.length < perPage) break;
        if (items.length === 0) break;
      }
      return out;
    },
    async postOrFind(path, body, find) {
      try {
        return await api.post(path, body);
      } catch (e) {
        if (!(e instanceof CfError) || !e.ambiguous) throw e;
        const found = await find();
        if (found) return found;
        throw e;
      }
    },
    refresh() {
      stale = true;
    },
    async credentialType() {
      return (await credential())?.type ?? "";
    },
  };
  return api;
}
