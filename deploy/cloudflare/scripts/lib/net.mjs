// Edge probes of the engine: plain HTTPS requests to the hub, the workers.dev
// URL, an Access team and DNS-over-HTTPS, all without credentials. Requests
// never follow redirects (a Cloudflare Access login redirect is an answer, not
// "ready"), time out, and identify as backyard-cloudflare/<version>.
//
//   probe(url, opts)            → {ok: true, status, location, contentType, text, json}
//                                 | {ok: false, kind: "certificate"|"dns"|"network", code, error}
//   waitFor(check, opts)        polls check() until it reports done or the time is up
//   dohResolves(name)           whether a name resolves (AAAA, then A) through DoH
//   classifyFetchError(err)     "certificate" | "dns" | "network"
//   parseAccessRedirect(loc)    {team, kid} of a Cloudflare Access login redirect, or null
//   randomProbeLabel()          "byd-probe-<8 hex>", a fresh label for wildcard checks
//   proxyHint(env)              why a set HTTPS_PROXY is not used by fetch ("" when it is, or none is set)
//
// The DoH endpoint is BACKYARD_CF_DOH_URL (JSON API), by default
// https://cloudflare-dns.com/dns-query.

import { randomBytes } from "node:crypto";
import { readFileSync } from "node:fs";

export const DEFAULT_DOH_URL = "https://cloudflare-dns.com/dns-query";

const pkgVersion = (() => {
  try {
    return String(JSON.parse(readFileSync(new URL("../../package.json", import.meta.url), "utf8")).version ?? "0.0.0");
  } catch {
    return "0.0.0";
  }
})();

/** User-Agent of every request the engine makes itself. */
export const USER_AGENT = `backyard-cloudflare/${pkgVersion}`;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Error codes of fetch failures, outermost first (fetch wraps the socket error in `cause`). */
function errorCodes(err) {
  const codes = [];
  for (let e = err, depth = 0; e && depth < 5; e = e.cause, depth++) {
    if (typeof e.code === "string") codes.push(e.code);
    if (typeof e.name === "string" && (e.name === "TimeoutError" || e.name === "AbortError")) codes.push(e.name);
    if (Array.isArray(e.errors)) for (const sub of e.errors) if (typeof sub?.code === "string") codes.push(sub.code);
  }
  return codes;
}

/**
 * Classifies a fetch error: a certificate problem (the TLS certificate does not
 * cover the name yet, or is not trusted), a name that does not resolve, or
 * anything else.
 * @param {unknown} err
 * @returns {"certificate"|"dns"|"network"}
 */
export function classifyFetchError(err) {
  const codes = errorCodes(err);
  if (codes.some((c) => /^(ERR_TLS_|ERR_SSL_|CERT_|UNABLE_TO_)/.test(c) || c === "EPROTO" || c === "DEPTH_ZERO_SELF_SIGNED_CERT" || c === "SELF_SIGNED_CERT_IN_CHAIN")) {
    return "certificate";
  }
  if (codes.some((c) => c === "ENOTFOUND" || c === "EAI_AGAIN" || c === "EAI_NONAME" || c === "EAI_NODATA")) return "dns";
  return "network";
}

/**
 * @typedef {object} ProbeOK
 * @property {true} ok
 * @property {number} status
 * @property {string} location     Location header ("" when absent)
 * @property {string} contentType
 * @property {Headers} headers
 * @property {string} text         body (at most maxBody bytes)
 * @property {any} json            parsed body when it is JSON, else undefined
 *
 * @typedef {object} ProbeFail
 * @property {false} ok
 * @property {"certificate"|"dns"|"network"} kind
 * @property {string} code         first error code ("" when none)
 * @property {string} error        a short description
 */

/**
 * One request without following redirects.
 * @param {string} url
 * @param {{method?: string, headers?: Record<string, string>, timeoutMs?: number, accept?: string, maxBody?: number, body?: string}} [o]
 * @returns {Promise<ProbeOK | ProbeFail>}
 */
export async function probe(url, o = {}) {
  const headers = { "User-Agent": USER_AGENT, ...(o.accept ? { Accept: o.accept } : {}), ...(o.headers ?? {}) };
  try {
    const res = await fetch(url, {
      method: o.method ?? "GET",
      headers,
      body: o.body,
      redirect: "manual",
      signal: AbortSignal.timeout(o.timeoutMs ?? 10_000),
    });
    const max = o.maxBody ?? 256 * 1024;
    let text = "";
    try {
      const buf = Buffer.from(await res.arrayBuffer());
      text = buf.subarray(0, max).toString("utf8");
    } catch {
      /* body cut off: keep what the status says */
    }
    const contentType = res.headers.get("content-type") ?? "";
    let json;
    if (/\bjson\b/i.test(contentType) || /^\s*[{[]/.test(text)) {
      try {
        json = JSON.parse(text);
      } catch {
        json = undefined;
      }
    }
    return { ok: true, status: res.status, location: res.headers.get("location") ?? "", contentType, headers: res.headers, text, json };
  } catch (err) {
    const codes = errorCodes(err);
    const msg = err?.cause?.message || err?.message || String(err);
    return { ok: false, kind: classifyFetchError(err), code: codes[0] ?? "", error: msg };
  }
}

/**
 * Polls `check` until it returns {done: true} or `timeoutSec` has passed (0 =
 * check once). Returns the last result. `onPending(result, elapsedSec)` runs
 * before each wait (progress lines).
 * @template {{done: boolean}} T
 * @param {() => Promise<T>} check
 * @param {{timeoutSec?: number, intervalSec?: number, onPending?: (r: T, elapsedSec: number) => void, sleep?: (ms: number) => Promise<void>}} [o]
 * @returns {Promise<T>}
 */
export async function waitFor(check, o = {}) {
  const timeoutMs = Math.max(0, (o.timeoutSec ?? 0) * 1000);
  const intervalMs = Math.max(0, (o.intervalSec ?? 5) * 1000);
  const wait = o.sleep ?? sleep;
  const start = Date.now();
  for (;;) {
    const r = await check();
    const elapsed = Date.now() - start;
    if (r?.done || elapsed + intervalMs > timeoutMs) return r;
    o.onPending?.(r, Math.round(elapsed / 1000));
    await wait(intervalMs);
  }
}

/**
 * Whether `name` resolves through DNS-over-HTTPS: AAAA first, then A; a
 * response with Status 0 (NOERROR) and a non-empty Answer counts. Errors and
 * anything else count as "does not resolve".
 * @param {string} name
 * @param {{url?: string, types?: string[], timeoutMs?: number}} [o]
 */
export async function dohResolves(name, o = {}) {
  const base = o.url ?? (process.env.BACKYARD_CF_DOH_URL || DEFAULT_DOH_URL);
  for (const type of o.types ?? ["AAAA", "A"]) {
    const u = new URL(base);
    u.searchParams.set("name", name);
    u.searchParams.set("type", type);
    const r = await probe(u.href, { accept: "application/dns-json", timeoutMs: o.timeoutMs ?? 10_000 });
    if (r.ok && r.status === 200 && dohAnswered(r.json)) return true;
  }
  return false;
}

/** Whether a DoH JSON response is NOERROR with at least one answer. */
export function dohAnswered(json) {
  return Boolean(json && typeof json === "object" && json.Status === 0 && Array.isArray(json.Answer) && json.Answer.length > 0);
}

/**
 * Parses the Location of a Cloudflare Access login redirect
 * (https://<team>.cloudflareaccess.com/cdn-cgi/access/login/<host>?kid=<aud>&redirect_url=…).
 * Returns {team: the team host, kid: the application AUD or ""}, or null when
 * the location is not an Access login.
 * @param {string} location
 * @returns {{team: string, kid: string} | null}
 */
export function parseAccessRedirect(location) {
  let u;
  try {
    u = new URL(String(location ?? ""));
  } catch {
    return null;
  }
  if (u.protocol !== "https:") return null;
  const host = u.hostname.toLowerCase();
  if (!host.endsWith(".cloudflareaccess.com") || host === ".cloudflareaccess.com") return null;
  if (!u.pathname.startsWith("/cdn-cgi/access/")) return null;
  return { team: host, kid: (u.searchParams.get("kid") ?? "").toLowerCase() };
}

/** The proxy variables wrangler honours and Node's fetch ignores by default. */
const PROXY_VARS = ["HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"];

/**
 * Why the engine's own requests may miss a configured proxy: a proxy variable
 * is set, but Node's fetch uses it only with NODE_USE_ENV_PROXY=1 (or
 * --use-env-proxy), on Node 22.21 or later. "" when no proxy is set or fetch
 * uses it.
 * @param {NodeJS.ProcessEnv} [env]
 * @param {{node?: string, execArgv?: string[]}} [o]   the Node version and flags (default: this process)
 */
export function proxyHint(env = process.env, o = {}) {
  const name = PROXY_VARS.find((k) => env[k]);
  if (!name) return "";
  const node = o.node ?? process.versions.node;
  const [major, minor] = String(node).split(".").map(Number);
  const supported = (major === 22 && minor >= 21) || major >= 24;
  const flag = [...(o.execArgv ?? process.execArgv), ...String(env.NODE_OPTIONS ?? "").split(/\s+/)].includes("--use-env-proxy");
  if (supported && (env.NODE_USE_ENV_PROXY === "1" || flag)) return "";
  return supported
    ? `${name} is set, but Node's fetch ignores it: set NODE_USE_ENV_PROXY=1 so Backyard's own API calls and checks use the proxy (wrangler uses it already)`
    : `${name} is set, but Node ${node}'s fetch ignores it: Backyard's own API calls and checks reach a proxy only on Node 22.21 or later, with NODE_USE_ENV_PROXY=1`;
}

/** A fresh random label for wildcard checks: byd-probe-<8 hex>. */
export function randomProbeLabel() {
  return `byd-probe-${randomBytes(4).toString("hex")}`;
}
