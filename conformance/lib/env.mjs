// Suite configuration (environment variables) and server discovery.
//
//   BACKYARD_URL                 hub URL of the server under test (required)
//   BACKYARD_TOKEN               optional bearer token sent with every request
//   BACKYARD_URL_MODE            host | path (default: url_mode of /__backyard/api/info)
//   BACKYARD_SITE_URL_TEMPLATE   optional site URL template, e.g. "https://{site}.apps.example.com/"
//   BACKYARD_RESOLVE_IP          optional address that site host names resolve to (host mode
//                                without wildcard DNS); *.localhost always falls back to the hub
//   BACKYARD_CONFORMANCE_PREFIX  prefix of the test site names (default "cf")
//   BACKYARD_SKIP_SDK_BYTES=1    do not compare the served SDK with web/sdk (testing another version)
import { randomBytes } from "node:crypto";
import dns from "node:dns";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

/** Repository root (the suite lives in <repo>/conformance). */
export const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

const rawURL = (process.env.BACKYARD_URL || "").trim();
if (!rawURL) {
  throw new Error("BACKYARD_URL is not set: point it at the hub of the server under test (see conformance/README.md)");
}
/** Hub URL without trailing slash. */
export const hubURL = rawURL.replace(/\/+$/, "");
const hub = new URL(hubURL);
/** Optional bearer token. */
export const token = (process.env.BACKYARD_TOKEN || "").trim();

// ---------------------------------------------------------------------------
// Name resolution: Node's resolver (getaddrinfo) does not map *.localhost to
// loopback the way browsers do. Site host names that do not resolve fall back
// to the hub's address (or BACKYARD_RESOLVE_IP), so host-mode servers on
// localhost work without /etc/hosts entries. fetch, WebSocket and node:http
// all resolve through dns.lookup.

const hubHost = hub.hostname.replace(/^\[|\]$/g, "").toLowerCase();
const resolveIP = (process.env.BACKYARD_RESOLVE_IP || "").trim();
let siteSuffixes = [".localhost"];
const origLookup = dns.lookup;

function patchedLookup(hostname, options, callback) {
  if (typeof options === "function") {
    callback = options;
    options = {};
  } else if (typeof options === "number") {
    options = { family: options };
  } else if (!options) {
    options = {};
  }
  const h = String(hostname).toLowerCase().replace(/\.$/, "");
  const candidate = h !== hubHost && siteSuffixes.some((s) => h.endsWith(s));
  if (!candidate) return origLookup.call(dns, hostname, options, callback);
  if (resolveIP) return origLookup.call(dns, resolveIP, options, callback);
  return origLookup.call(dns, hostname, options, (err, ...rest) => {
    if (!err) return callback(null, ...rest);
    origLookup.call(dns, hubHost, options, callback);
  });
}
dns.lookup = patchedLookup;

// ---------------------------------------------------------------------------
// Server info

let infoPromise;

/** GET /__backyard/api/info (cached). */
export function serverInfo() {
  infoPromise ??= (async () => {
    const res = await fetch(hubURL + "/__backyard/api/info", { redirect: "manual" });
    if (res.status !== 200) throw new Error(`GET ${hubURL}/__backyard/api/info: HTTP ${res.status}`);
    const info = await res.json();
    const base = String(info.base_domain || hubHost).toLowerCase();
    siteSuffixes = [...new Set([".localhost", "." + base])];
    return info;
  })();
  return infoPromise;
}

/**
 * The environment of one test file: server info, URL mode, gateway and site
 * URL builders.
 */
export async function setup() {
  const info = await serverInfo();
  const envMode = (process.env.BACKYARD_URL_MODE || "").trim();
  const mode = envMode || info.url_mode || "host";
  if (mode !== "host" && mode !== "path") throw new Error(`BACKYARD_URL_MODE must be host or path, got ${JSON.stringify(mode)}`);
  const baseDomain = String(info.base_domain || hubHost).toLowerCase();
  const template = (process.env.BACKYARD_SITE_URL_TEMPLATE || "").trim();
  // The embedded gateway is always reachable at the hub; a standalone one is
  // advertised in gateway_url.
  const gateway = String(info.gateway_url || hubURL + "/__backyard/api/deploy").replace(/\/+$/, "");

  /** URL (with trailing slash) at which this suite reaches a site. */
  function siteURL(site) {
    if (template) return template.replaceAll("{site}", site).replace(/\/*$/, "/");
    if (mode === "path") return `${hubURL}/s/${site}/`;
    return `${hub.protocol}//${site}.${baseDomain}${hub.port ? ":" + hub.port : ""}/`;
  }

  /** The site URL the server itself advertises (SiteMeta.url, commit url). */
  function canonicalSiteURL(site) {
    const pub = new URL(String(info.public_url || hubURL));
    if (info.url_mode === "path") return `${String(info.public_url).replace(/\/+$/, "")}/s/${site}/`;
    return `${pub.protocol}//${site}.${baseDomain}${pub.port ? ":" + pub.port : ""}/`;
  }

  return {
    info,
    mode,
    baseDomain,
    hub: hubURL,
    hubOrigin: hub.origin,
    gateway,
    siteURL,
    canonicalSiteURL,
    /** Origin of a site (host mode: its own; path mode: the hub's). */
    siteOrigin: (site) => new URL(siteURL(site)).origin,
    /** Site path prefix ("" in host mode, "/s/<site>" in path mode). */
    sitePrefix: (site) => new URL(siteURL(site)).pathname.replace(/\/$/, ""),
    /** Absolute URL of a site API endpoint (path starts with "/"). */
    api: (site, path) => siteURL(site) + "__backyard/api" + path,
  };
}

/** A fresh, valid site name unique to this run. */
export function uniqueSite(tag) {
  const prefix = (process.env.BACKYARD_CONFORMANCE_PREFIX || "cf").toLowerCase().replace(/[^a-z0-9-]/g, "");
  return `${prefix}-${tag}-${randomBytes(4).toString("hex")}`.slice(0, 63).replace(/-+$/, "");
}

/** The SDK files of this repository (null when absent or disabled). */
export function repoSDK(name) {
  if (process.env.BACKYARD_SKIP_SDK_BYTES === "1") return null;
  const p = join(repoRoot, "web", "sdk", name);
  return existsSync(p) ? readFileSync(p) : null;
}

/** Shared query cases (internal/docstore/conformance/querycases.json). */
export function queryCasesPath() {
  return process.env.BACKYARD_QUERYCASES || join(repoRoot, "internal", "docstore", "conformance", "querycases.json");
}
