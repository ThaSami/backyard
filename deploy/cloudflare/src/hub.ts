/**
 * Hub host requests (SPEC §2): the hub UI, /docs, /llms.txt, the hub API
 * (/me, /sites, /tokens, /info), the deploy gateway and the auth endpoints
 * (/__backyard/auth/cli browser → CLI token handoff, login, logout).
 */
import { hubDirExists, serveHubFile, serveSDK, serveText } from "./assets";
import { ANON_COOKIE, authenticate, cookieString } from "./auth";
import { formatMicros } from "./db/time";
import { infoDocument, serveGateway } from "./gateway";
import { HttpError, badRequest, forbidden, isPlainObject, json, methodNotAllowed, notFound, readBody, unwrap } from "./http";
import { cleanUser, type User } from "./identity";
import { issueAPIToken } from "./jwt";
import { docMarkdown, llmsFullTxt, llmsTxt } from "./llms";
import { authErrorPage, tokenPage } from "./pages";
import { redirect, siteURL, type RequestContext } from "./router";
import { apiGate, dispatch } from "./site-api";
import { siteMetaJSON } from "./sitemeta";
import { registry } from "./stubs";

/** Dispatches a request for the hub (ctx.kind === "hub"). */
export async function serveHub(ctx: RequestContext): Promise<Response> {
  const p = ctx.path;
  if (p === "/__backyard/backyard.js" || p === "/__backyard/backyard.mjs" || p === "/__backyard/backyard.d.ts") {
    if (ctx.req.method !== "GET" && ctx.req.method !== "HEAD") throw methodNotAllowed(["GET", "HEAD"]);
    return serveSDK(ctx);
  }
  if (p === "/__backyard/auth" || p.startsWith("/__backyard/auth/")) return serveAuth(ctx);
  if (p === "/__backyard/api/info") {
    if (ctx.req.method !== "GET" && ctx.req.method !== "HEAD") throw methodNotAllowed(["GET", "HEAD"]);
    return json(infoDocument(ctx), 200, { "Cache-Control": "no-cache" });
  }
  if (p === "/__backyard/api/deploy" || p.startsWith("/__backyard/api/deploy/")) {
    return serveGateway(ctx, p.slice("/__backyard/api/deploy".length) || "/");
  }
  if (p === "/__backyard/api" || p.startsWith("/__backyard/api/")) {
    const user = await apiGate(ctx);
    return dispatch(ctx, user, p.slice("/__backyard/api".length) || "/", HUB_ROUTES);
  }
  if (p === "/__backyard" || p.startsWith("/__backyard/")) throw notFound(`no such endpoint: ${p.slice(0, 200)}`);
  return serveHubPage(ctx);
}

const HUB_ROUTES = [
  { method: "GET", re: /^\/me$/, h: async (_c: RequestContext, u: User) => json(cleanUser(u)) },
  { method: "GET", re: /^\/sites$/, h: listSites },
  { method: "POST", re: /^\/tokens$/, h: mintToken },
];

async function listSites(ctx: RequestContext): Promise<Response> {
  const list = unwrap(await registry(ctx.env).listSites());
  return json({ sites: list.map((r) => siteMetaJSON(r, siteURL(ctx, r.name))) });
}

/** Groups that grant admin rights: kept first when a token must drop groups to fit. */
function adminGroup(ctx: RequestContext): (g: string) => boolean {
  return (g) => ctx.cfg.adminGroups.includes(g.toLowerCase());
}

/** POST /__backyard/api/tokens: mint a personal API token. */
async function mintToken(ctx: RequestContext, u: User): Promise<Response> {
  if (u.anonymous || u.id.startsWith("anon:")) throw forbidden("anonymous users cannot create API tokens");
  if (u.id.startsWith("token:")) throw forbidden("static tokens cannot create API tokens");
  const raw = await readBody(ctx.req, 64 << 10);
  let body: Record<string, unknown> = {};
  const text = new TextDecoder().decode(raw).trim();
  if (text) {
    try {
      const v = JSON.parse(text) as unknown;
      if (!isPlainObject(v)) throw new Error("expected an object");
      body = v;
    } catch (e) {
      throw badRequest(`invalid JSON: ${(e as Error).message}`);
    }
  }
  const name = typeof body.name === "string" ? body.name.trim() : "";
  if (body.name !== undefined && typeof body.name !== "string") throw badRequest('invalid JSON: field "name" must be a string');
  if (name.length > 100) throw badRequest("name must be at most 100 characters");
  const ttlHours = body.ttl_hours === undefined || body.ttl_hours === null ? 0 : body.ttl_hours;
  if (typeof ttlHours !== "number" || !Number.isFinite(ttlHours)) throw badRequest('invalid JSON: field "ttl_hours" must be a number');
  if (ttlHours < 0) throw badRequest("ttl_hours must be positive");
  const maxTTL = ctx.cfg.tokenTTLSeconds;
  let ttl = maxTTL;
  if (ttlHours > 0) {
    if (ttlHours * 3600 > maxTTL) throw badRequest(`ttl_hours must be at most ${maxTTL / 3600}`);
    ttl = Math.max(60, Math.floor(ttlHours * 3600));
  }
  const { token, expiresAt } = await issueAPIToken(ctx.cfg.secret, u, ttl, undefined, adminGroup(ctx));
  console.log(JSON.stringify({ audit: true, action: "token.issue", actor: u.id, name, expires_at: expiresAt }));
  const out: Record<string, unknown> = { token, expires_at: formatMicros(expiresAt * 1_000_000) };
  if (name) out.name = name;
  return json(out);
}

// --- hub pages ---------------------------------------------------------------

async function serveHubPage(ctx: RequestContext): Promise<Response> {
  if (ctx.req.method !== "GET" && ctx.req.method !== "HEAD") throw methodNotAllowed(["GET", "HEAD"]);
  try {
    await authenticate(ctx);
  } catch (e) {
    if (e instanceof HttpError && e.status === 401 && ctx.cfg.authMode === "access") {
      throw new HttpError(401, "unauthenticated", "This Backyard is protected by Cloudflare Access, but this request did not come through it. Open the hub through its protected address.", {
        title: "Sign in required",
      });
    }
    throw e;
  }
  const p = ctx.path;
  if (p === "/llms.txt") return serveText(ctx, "llms.txt", "text/plain; charset=utf-8", llmsTxt(ctx.publicURL));
  if (p === "/llms-full.txt") return serveText(ctx, "llms-full.txt", "text/plain; charset=utf-8", llmsFullTxt(ctx.publicURL));
  if (p === "/docs" || p === "/docs/") {
    const res = await serveHubFile(ctx, "docs.html");
    if (res) return res;
    throw notFound("The page you are looking for does not exist.");
  }
  if (p.startsWith("/docs/") && p.endsWith(".md")) {
    const name = p.slice("/docs/".length, -".md".length);
    const md = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/.test(name) ? docMarkdown(name) : "";
    if (!md) throw notFound(`no such document: ${name.slice(0, 64)}`);
    return serveText(ctx, name + ".md", "text/markdown; charset=utf-8", md);
  }
  let name = p.replace(/^\/+/, "");
  if (name === "" || name.endsWith("/")) name += "index.html";
  const res = await serveHubFile(ctx, name);
  if (res) return res;
  if (hubDirExists(name)) return redirect(ctx.url.pathname + "/" + ctx.url.search, 301);
  throw notFound("The page you are looking for does not exist.");
}

// --- auth endpoints -----------------------------------------------------------

/** Validates a CLI loopback redirect URI (SPEC §8.3). */
export function validateLoopbackRedirect(raw: string): URL {
  const s = raw.trim();
  if (!s) throw new Error("redirect_uri is required");
  // eslint-disable-next-line no-control-regex
  if (s.length > 2048 || /[\u0000-\u001f\u007f\\]/.test(s)) throw new Error("redirect_uri is malformed");
  let u: URL;
  try {
    u = new URL(s);
  } catch {
    throw new Error("redirect_uri is malformed");
  }
  if (u.protocol !== "http:") throw new Error("redirect_uri must use http:// on a loopback address");
  if (u.username || u.password || u.hash || s.includes("#")) throw new Error("redirect_uri must not contain userinfo or a fragment");
  const host = u.hostname.toLowerCase();
  if (host !== "127.0.0.1" && host !== "localhost" && host !== "[::1]") throw new Error("redirect_uri must point at 127.0.0.1, localhost or [::1]");
  const port = Number(u.port);
  if (!u.port || !Number.isInteger(port) || port < 1 || port > 65535) throw new Error("redirect_uri must include a port");
  return u;
}

const MAX_REDIRECT_BYTES = 4096;
const DNS_LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** Whether p is one of the login flow's own endpoints (returning there would loop). */
function isAuthFlowPath(p: string): boolean {
  const i = p.indexOf("/__backyard/auth/");
  const rest = i >= 0 ? p.slice(i) : p;
  return rest === "/__backyard/auth/login" || rest === "/__backyard/auth/logout";
}

function effectivePort(u: URL): string {
  return u.port || (u.protocol === "https:" ? "443" : "80");
}

/**
 * Validates a post-login return URL (the rd parameter; port of the Go
 * sanitizeRedirect): a relative path starting with a single "/" (resolved on
 * the hub) or an absolute URL with the hub's scheme and port whose host is the
 * hub, the base domain or (host mode) a single-label site host. Anything else —
 * other hosts, protocol-relative URLs, backslashes, control characters or
 * whitespace that browsers strip ("/\t/evil.com"), userinfo — becomes the hub
 * URL, so login can never be turned into an open redirect.
 */
export function safeReturnTo(ctx: RequestContext, raw: string): string {
  const hub = ctx.publicURL + "/";
  const rd = raw.trim();
  // eslint-disable-next-line no-control-regex
  if (!rd || rd.length > MAX_REDIRECT_BYTES || rd.includes("\\") || /[\u0000- \u007f-\u009f]/.test(rd)) return hub;
  if (rd.startsWith("/")) {
    if (rd.startsWith("//")) return hub;
    let u: URL;
    try {
      u = new URL(ctx.publicURL + rd);
    } catch {
      return hub;
    }
    if (u.origin !== new URL(ctx.publicURL).origin || isAuthFlowPath(u.pathname)) return hub;
    return u.toString();
  }
  let u: URL;
  try {
    u = new URL(rd);
  } catch {
    return hub;
  }
  const hubURL = new URL(ctx.publicURL);
  if (u.protocol !== hubURL.protocol || u.username || u.password || isAuthFlowPath(u.pathname)) return hub;
  if (effectivePort(u) !== effectivePort(hubURL)) return hub;
  const host = u.hostname.toLowerCase().replace(/\.$/, "");
  const bases = [hubURL.hostname.toLowerCase()];
  if (ctx.cfg.baseDomain && ctx.cfg.baseDomain !== bases[0]) bases.push(ctx.cfg.baseDomain);
  for (const base of bases) {
    if (host === base) return u.toString();
    if (ctx.cfg.urlMode === "host" && host.endsWith("." + base) && DNS_LABEL_RE.test(host.slice(0, -(base.length + 1)))) return u.toString();
  }
  return hub;
}

async function serveAuth(ctx: RequestContext): Promise<Response> {
  const p = ctx.path;
  if (p === "/__backyard/auth/cli") {
    if (ctx.req.method !== "GET") throw methodNotAllowed(["GET"]);
    return serveCLI(ctx);
  }
  if (p === "/__backyard/auth/login") {
    if (ctx.req.method !== "GET" && ctx.req.method !== "HEAD") throw methodNotAllowed(["GET", "HEAD"]);
    // Cloudflare Access (or nothing, in mode none) authenticates page loads
    // itself: going to the target is all a "login" needs.
    return redirect(safeReturnTo(ctx, ctx.url.searchParams.get("rd") ?? ""), 302);
  }
  if (p === "/__backyard/auth/logout") {
    if (ctx.req.method !== "GET" && ctx.req.method !== "POST") throw methodNotAllowed(["GET", "POST"]);
    if (ctx.cfg.authMode === "access") return redirect("/cdn-cgi/access/logout", 302);
    const res = redirect(ctx.publicURL + "/", 302);
    res.headers.append("Set-Cookie", cookieString(ctx, ANON_COOKIE, "", 0).replace("Max-Age=0", "Max-Age=0; Expires=Thu, 01 Jan 1970 00:00:01 GMT"));
    return res;
  }
  throw notFound("no such endpoint");
}

/**
 * /__backyard/auth/cli?redirect_uri=http://127.0.0.1:PORT/callback&state=…
 * → 302 redirect_uri?token=…&expires_at=…&state=…; ?mode=display shows the
 * token. The browser must be authenticated by the mode's own mechanism
 * (bearer tokens are not accepted, so a token cannot renew itself).
 */
async function serveCLI(ctx: RequestContext): Promise<Response> {
  const q = ctx.url.searchParams;
  const display = q.get("mode") === "display";
  const state = q.get("state") ?? "";
  if (state.length > 1024) return authErrorPage(400, "Invalid CLI login request", "The state parameter is too long.");
  let target: URL | null = null;
  if (!display) {
    try {
      target = validateLoopbackRedirect(q.get("redirect_uri") ?? "");
    } catch (e) {
      return authErrorPage(400, "Invalid CLI login request", `This link was not created by the Backyard CLI: ${(e as Error).message}.`);
    }
    if (!state) return authErrorPage(400, "Invalid CLI login request", "The state parameter is missing.");
  }
  let u: User;
  try {
    u = await authenticate(ctx, { bearer: false });
  } catch (e) {
    if (e instanceof HttpError && e.status === 401) {
      return authErrorPage(401, "Not signed in", "Your request was not authenticated by Cloudflare Access. Open this page through the protected address of your Backyard.");
    }
    throw e;
  }
  if (ctx.cfg.authMode !== "none" && (u.anonymous || u.id.startsWith("token:"))) {
    return authErrorPage(403, "Cannot create a CLI token", "Anonymous and static-token identities cannot mint personal API tokens.");
  }
  const { token, expiresAt } = await issueAPIToken(ctx.cfg.secret, u, ctx.cfg.tokenTTLSeconds, undefined, adminGroup(ctx));
  console.log(JSON.stringify({ audit: true, action: "token.issue", actor: u.id, via: "cli", display }));
  const expires = formatMicros(expiresAt * 1_000_000);
  if (display || !target) {
    return tokenPage({
      who: u.email || u.id,
      token,
      expiresAt: new Date(expiresAt * 1000).toUTCString(),
      command: `backyard login --server ${ctx.publicURL} --token ${token}`,
    });
  }
  target.searchParams.set("token", token);
  target.searchParams.set("expires_at", expires);
  target.searchParams.set("state", state);
  const res = redirect(target.toString(), 302);
  res.headers.set("Referrer-Policy", "no-referrer");
  return res;
}
