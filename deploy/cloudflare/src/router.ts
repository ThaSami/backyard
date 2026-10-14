/**
 * Request classification (SPEC §2): hub vs site, host mode (<site>.<BASE_DOMAIN>)
 * vs path mode (/s/<site>/), canonical paths and the public URLs derived from
 * the configuration and the request.
 */
import type { Config } from "./config";
import type { Env } from "./env";
import { HttpError, badRequest } from "./http";
import type { User } from "./identity";

/** Site name pattern (SPEC §9). */
export const SITE_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** Validates a site name; returns an error message or "" when valid. */
export function siteNameError(name: string, reserved: string[] | null): string {
  if (!SITE_NAME_RE.test(name)) {
    return `invalid site name ${JSON.stringify(name)} (lowercase letters, digits and '-', 1-63 chars, no leading/trailing '-')`;
  }
  if (reserved && reserved.some((r) => r.toLowerCase() === name)) return `site name ${JSON.stringify(name)} is reserved`;
  return "";
}

/** Per-request state shared by every handler. */
export interface RequestContext {
  env: Env;
  cfg: Config;
  req: Request;
  url: URL;
  /** Lowercased hostname without port. */
  host: string;
  /** Normalized origin of the request (scheme://host[:port]). */
  origin: string;
  /** Hub URL without trailing slash. */
  publicURL: string;
  /** Origin of the hub. */
  hubOrigin: string;
  /** Effective URL mode of this request ("path" for /s/<site>/ requests). */
  mode: "host" | "path";
  kind: "hub" | "site";
  /** Site name ("" on the hub). */
  site: string;
  /** "/s/<site>" for path-mode site requests, else "". */
  prefix: string;
  /** Decoded, canonical path relative to the site (or the hub). */
  path: string;
  /** Raw (still percent-encoded) path relative to the site (or hub). */
  rawPath: string;
  clientIP: string;
  /** Authenticated user (set by the auth step). */
  user?: User;
  /**
   * Expiry (Unix seconds) of the Backyard API token that authenticated the
   * request as its bearer credential (set by the auth step; undefined for any
   * other credential). A token minted with it must not outlive it.
   */
  bearerExp?: number;
  /** Set-Cookie values to add to the response. */
  setCookies: string[];
  waitUntil: (p: Promise<unknown>) => void;
}

/** Result of routing: a context, or an immediate response (redirects). */
export type RouteResult = { ctx: RequestContext } | { response: Response };

/** Decodes a percent-encoded path; throws 400 on malformed escapes. */
export function decodePath(raw: string): string {
  try {
    return raw
      .split("/")
      .map((seg) => decodeURIComponent(seg))
      .join("/");
  } catch {
    throw badRequest("malformed percent-encoding in the request path");
  }
}

/** Cleans a path like path.Clean but keeps a trailing slash (Go cleanURLPath). */
export function cleanPath(p: string): string {
  if (p === "") return "/";
  if (p[0] !== "/") p = "/" + p;
  if (!p.includes("//") && !p.includes("/./") && !p.includes("/../") && !p.endsWith("/.") && !p.endsWith("/..")) return p;
  const trailing = p.endsWith("/");
  const out: string[] = [];
  for (const seg of p.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") out.pop();
    else out.push(seg);
  }
  let np = "/" + out.join("/");
  if (trailing && np !== "/") np += "/";
  return np;
}

function originOf(u: URL): string {
  return `${u.protocol}//${u.host}`.toLowerCase();
}

/** Classifies a request. */
export function route(req: Request, env: Env, cfg: Config, waitUntil: (p: Promise<unknown>) => void): RouteResult {
  const url = new URL(req.url);
  const host = url.hostname.toLowerCase().replace(/\.$/, "");
  const origin = originOf(url);
  const base = cfg.baseDomain;
  const underBase = base !== "" && (host === base || host.endsWith("." + base));
  let publicURL = cfg.publicURL;
  if (!publicURL) {
    if (base && underBase) publicURL = `${url.protocol}//${base}${url.port ? ":" + url.port : ""}`;
    else if (base) publicURL = `https://${base}`;
    else publicURL = origin;
  }
  publicURL = publicURL.replace(/\/+$/, "");
  const hubOrigin = originOf(new URL(publicURL));

  // Canonical path check on the decoded path (SPEC §3: no "..", no "//").
  const decoded = decodePath(url.pathname);
  const clean = cleanPath(decoded);
  if (clean !== decoded) {
    if (req.method === "GET" || req.method === "HEAD") {
      const loc = clean.split("/").map(encodeURIComponent).join("/") + url.search;
      return { response: redirect(loc, 301) };
    }
    throw badRequest("non-canonical request path");
  }

  const ctx: RequestContext = {
    env,
    cfg,
    req,
    url,
    host,
    origin,
    publicURL,
    hubOrigin,
    mode: "path",
    kind: "hub",
    site: "",
    prefix: "",
    path: decoded,
    rawPath: url.pathname,
    clientIP: req.headers.get("CF-Connecting-IP") || "unknown",
    setCookies: [],
    waitUntil,
  };

  if (cfg.urlMode === "host" && base) {
    if (host === base) {
      ctx.mode = "host";
      return { ctx };
    }
    if (host.endsWith("." + base)) {
      const label = host.slice(0, -(base.length + 1));
      if (label && !label.includes(".")) {
        ctx.mode = "host";
        ctx.kind = "site";
        ctx.site = label;
        return { ctx };
      }
      throw new HttpError(404, "not_found", `${JSON.stringify(host)} is not a Backyard host.`, { title: "Unknown host" });
    }
    // Any other host reaching the Worker (workers.dev, a path-mode custom
    // domain) serves the hub with path-mode sites.
  }

  if (decoded.startsWith("/s/")) {
    const rest = decoded.slice(3);
    const slash = rest.indexOf("/");
    const name = slash < 0 ? rest : rest.slice(0, slash);
    if (name === "") throw new HttpError(404, "not_found", "No site name in the URL.");
    if (slash < 0) {
      if (req.method === "GET" || req.method === "HEAD") {
        return { response: redirect(url.pathname + "/" + url.search, 301) };
      }
      throw new HttpError(404, "not_found", "No such endpoint.");
    }
    const rawRest = url.pathname.slice(3);
    const rawSlash = rawRest.indexOf("/");
    ctx.kind = "site";
    ctx.site = name;
    ctx.prefix = "/s/" + name;
    ctx.path = rest.slice(slash);
    ctx.rawPath = rawSlash < 0 ? "/" : rawRest.slice(rawSlash);
    return { ctx };
  }
  return { ctx };
}

/** A redirect with Cache-Control: no-store. */
export function redirect(location: string, status = 302): Response {
  return new Response(null, { status, headers: { Location: location, "Cache-Control": "no-store" } });
}

/** Public URL of a site (with trailing slash) in the configured URL mode. */
export function siteURL(ctx: RequestContext, site: string): string {
  if (ctx.cfg.urlMode === "host" && ctx.cfg.baseDomain) {
    const u = new URL(ctx.publicURL);
    return `${u.protocol}//${site}.${ctx.cfg.baseDomain}${u.port ? ":" + u.port : ""}/`;
  }
  return `${ctx.publicURL}/s/${site}/`;
}

/** Absolute URL of the current site's root (or the hub's). */
export function siteRoot(ctx: RequestContext): string {
  return ctx.origin + ctx.prefix + "/";
}

/** Gateway base URL (SPEC §7). */
export function gatewayURL(ctx: RequestContext): string {
  return ctx.origin + "/__backyard/api/deploy";
}

/**
 * Whether the client prefers JSON errors: API paths, the OAuth discovery
 * documents of the hub (/.well-known/oauth-*, SPEC §8.4.2) or Accept:
 * application/json.
 */
export function wantsJSON(ctx: RequestContext): boolean {
  const p = ctx.path;
  if (p.startsWith("/__backyard/") && !p.startsWith("/__backyard/files/") && !p.startsWith("/__backyard/auth/")) return true;
  if (ctx.kind === "hub" && p.startsWith("/.well-known/oauth-")) return true;
  const accept = ctx.req.headers.get("Accept") ?? "";
  return accept.includes("application/json") && !accept.includes("text/html");
}

/** Normalizes an Origin header value to scheme://host[:port], or "" when unusable. */
export function parseOrigin(v: string): string {
  try {
    const u = new URL(v.trim());
    if (u.protocol !== "http:" && u.protocol !== "https:") return "";
    if (u.username || u.password) return "";
    return originOf(u);
  } catch {
    return "";
  }
}
