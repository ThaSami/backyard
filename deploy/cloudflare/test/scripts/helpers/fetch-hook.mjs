// Test-only fetch hook for the engine: `node --import <abs path of this file>
// bin/backyard-cloudflare.mjs …` (helpers/sandbox.mjs does this). It wraps
// globalThis.fetch so that the engine's edge probes reach the fake Cloudflare
// (helpers/fake-cloudflare.mjs) instead of the Internet. Production code has
// no such hook.
//
//   https://<host>/<path>  → <FAKE_CF_ORIGIN>/__edge/<host>/<path>   for hosts in FAKE_EDGE
//                            (a JSON list of host suffixes: "workers.dev" covers
//                            backyard.acme.workers.dev) and every *.cloudflareaccess.com
//   https://cloudflare-dns.com/dns-query?… → <FAKE_CF_ORIGIN>/dns-query?…
//   loopback URLs (the fake API, CLOUDFLARE_API_BASE_URL) → unchanged
//   anything else          → fails like an unknown host (ENOTFOUND): tests never reach the network
//
// FAKE_CF_ORIGIN defaults to the origin of CLOUDFLARE_API_BASE_URL.
// FAKE_TLS_FAIL=<suffix>[,<suffix>…] makes requests to matching hosts fail like a
// certificate that does not cover them (cause.code ERR_TLS_CERT_ALTNAME_INVALID);
// "*.<host>" matches only the subdomains of <host> (the hub's certificate works,
// the sites' wildcard certificate is missing).
// An edge answer with an "x-fake-error: <CODE>" header (the fake's way to say
// "this name does not resolve", ENOTFOUND, or a TLS error) becomes a fetch error
// with that cause code.
// FAKE_FAST_SLEEP=1 makes timers of a second or more fire after 1 ms (the
// engine's readiness polling and retries), so "never ready" takes no 45 s.

import { tlsFails } from "./fake-cloudflare.mjs";

if (process.env.FAKE_FAST_SLEEP === "1") {
  const realSetTimeout = globalThis.setTimeout;
  globalThis.setTimeout = (fn, ms, ...args) => realSetTimeout(fn, Number(ms) >= 1000 ? 1 : ms, ...args);
}

const realFetch = globalThis.fetch;
const origin = process.env.FAKE_CF_ORIGIN || (process.env.CLOUDFLARE_API_BASE_URL ? new URL(process.env.CLOUDFLARE_API_BASE_URL).origin : "");
let suffixes = [];
try {
  suffixes = JSON.parse(process.env.FAKE_EDGE || "[]").map((x) => String(x).toLowerCase());
} catch {
  suffixes = [];
}
const tlsFail = String(process.env.FAKE_TLS_FAIL ?? "")
  .split(",")
  .map((x) => x.trim().toLowerCase())
  .filter(Boolean);

const matches = (host, list) => list.some((sfx) => host === sfx || host.endsWith(`.${sfx}`));
const LOOPBACK = new Set(["127.0.0.1", "localhost", "[::1]", "::1"]);

function fetchError(code, message) {
  const cause = Object.assign(new Error(message), { code });
  return new TypeError("fetch failed", { cause });
}

globalThis.fetch = async function hookedFetch(input, init = {}) {
  const isRequest = typeof Request !== "undefined" && input instanceof Request;
  const url = new URL(isRequest ? input.url : String(input));
  const host = url.hostname.toLowerCase();
  if (LOOPBACK.has(host)) return realFetch(input, init);
  if (!origin) throw fetchError("ENOTFOUND", `getaddrinfo ENOTFOUND ${host} (fetch hook: no FAKE_CF_ORIGIN)`);
  const opts = isRequest ? { method: input.method, headers: input.headers, redirect: input.redirect, signal: input.signal, ...init } : init;
  if (host === "cloudflare-dns.com" && url.pathname === "/dns-query") return realFetch(`${origin}/dns-query${url.search}`, opts);
  if (url.protocol === "https:" && (host.endsWith(".cloudflareaccess.com") || matches(host, suffixes))) {
    if (tlsFails(host, tlsFail)) throw fetchError("ERR_TLS_CERT_ALTNAME_INVALID", `Hostname/IP does not match certificate's altnames: Host: ${host}`);
    const res = await realFetch(`${origin}/__edge/${host}${url.pathname}${url.search}`, opts);
    const fe = res.headers.get("x-fake-error");
    if (fe) {
      await res.arrayBuffer().catch(() => {});
      throw fetchError(fe, fe === "ENOTFOUND" ? `getaddrinfo ENOTFOUND ${host}` : `${fe} (${host})`);
    }
    return res;
  }
  throw fetchError("ENOTFOUND", `getaddrinfo ENOTFOUND ${host} (blocked by the test fetch hook)`);
};
