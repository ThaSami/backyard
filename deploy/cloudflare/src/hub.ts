/**
 * Hub host requests (SPEC §2): the hub UI, /docs, /llms.txt, the hub API
 * (/me, /sites, /tokens, /info), the deploy gateway and the auth endpoints
 * (/__backyard/auth/cli browser → CLI token handoff, login, logout).
 */
import { hubDirExists, serveHubFile, serveSDK, serveText } from "./assets";
import { ANON_COOKIE, authenticate, cookieString, isCookieConflict } from "./auth";
import { KeyLabel, b64url, b64urlDecode, deriveKey } from "./crypto";
import { formatMicros } from "./db/time";
import { infoDocument, serveGateway } from "./gateway";
import { HttpError, badRequest, forbidden, isPlainObject, json, methodNotAllowed, notFound, readBody, truncate, unauthenticated, unwrap } from "./http";
import { cleanUser, type User } from "./identity";
import { issueAPIToken } from "./jwt";
import { docMarkdown, llmsFullTxt, llmsTxt } from "./llms";
import { authErrorPage, cliConfirmPage, tokenPage } from "./pages";
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

/**
 * POST /__backyard/api/tokens: mint a personal API token.
 *
 * A request authenticated with a Backyard API token gets a token that expires
 * no later than the one it presented: a token cannot renew itself. Otherwise
 * chaining tokens would keep access (and a snapshot of the user's groups)
 * forever after the user left Cloudflare Access or the IdP, and only rotating
 * BACKYARD_SECRET would end it. A Cloudflare Access identity (or the guest of
 * mode none) is a fresh authentication and gets the full lifetime.
 */
async function mintToken(ctx: RequestContext, u: User): Promise<Response> {
  if (u.anonymous || u.id.startsWith("anon:")) throw forbidden("anonymous users cannot create API tokens");
  if (u.id.startsWith("token:")) throw forbidden("static tokens cannot create API tokens");
  // Taken before the body is read: how long it takes to arrive must not matter.
  const limit = ctx.bearerExp;
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
  const now = Math.floor(Date.now() / 1000);
  if (limit !== undefined) {
    // One second of margin: token times are whole seconds.
    const left = Math.floor(limit) - now - 1;
    if (left <= 0) throw unauthenticated("the API token used for this request has expired; sign in again (run: backyard login)");
    ttl = Math.min(ttl, left);
  }
  const { token, expiresAt } = await issueAPIToken(ctx.cfg.secret, u, ttl, now, adminGroup(ctx));
  if (limit !== undefined && expiresAt > limit) throw new Error("minted token would outlive the presented token");
  const audit: Record<string, unknown> = { audit: true, action: "token.issue", actor: u.id, name, expires_at: expiresAt };
  if (limit !== undefined) audit.from_api_token = true;
  console.log(JSON.stringify(audit));
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
    if (e instanceof HttpError && e.status === 401 && ctx.cfg.authMode === "access" && !isCookieConflict(e)) {
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
    // /docs/<name>.md and /docs/references/<name>.md, localized like /llms-full.txt.
    const rel = p.slice("/docs/".length, -".md".length);
    const md = docMarkdown(rel, ctx.publicURL);
    if (!md) throw notFound(`no such document: ${truncate(rel, 80)}`);
    return serveText(ctx, rel.slice(rel.lastIndexOf("/") + 1) + ".md", "text/markdown; charset=utf-8", md);
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

/** The login flow's own endpoints (Go's auth.LoginPath, CallbackPath and LogoutPath). */
const AUTH_PREFIX = "/__backyard/auth/";
const AUTH_FLOW_PATHS = new Set([AUTH_PREFIX + "login", AUTH_PREFIX + "callback", AUTH_PREFIX + "logout"]);

/** Go's path.Clean of an absolute path: no empty, "." or ".." segments, no trailing slash. */
function cleanAbsPath(p: string): string {
  const out: string[] = [];
  for (const seg of p.split("/")) {
    if (seg === "" || seg === ".") continue;
    if (seg === "..") out.pop();
    else out.push(seg);
  }
  return "/" + out.join("/");
}

/**
 * Whether the decoded path p is one of the login flow's own endpoints, also
 * below a site prefix ("/s/<site>/__backyard/auth/login"): returning there
 * after a login would loop or sign the user out (Go's isAuthFlowPath; /cli
 * stays allowed, the CLI handoff resumes after login).
 */
function isAuthFlowPath(p: string): boolean {
  p = cleanAbsPath("/" + p);
  const i = p.indexOf(AUTH_PREFIX);
  if (i > 0) p = p.slice(i);
  return AUTH_FLOW_PATHS.has(p);
}

/** A "%" not followed by two hex digits (Go's url.Parse rejects it in paths and fragments). */
const BAD_ESCAPE_RE = /%(?![0-9A-Fa-f]{2})/;

/**
 * The decoded path of a URL's raw path and fragment, like Go's url.Parse
 * (bytes become latin-1 characters: only the comparison with ASCII paths
 * matters), or null when either has an invalid escape.
 */
function decodedPath(rawPath: string, rawFragment: string): string | null {
  if (BAD_ESCAPE_RE.test(rawPath) || BAD_ESCAPE_RE.test(rawFragment)) return null;
  return rawPath.replace(/%([0-9A-Fa-f]{2})/g, (_, h: string) => String.fromCharCode(parseInt(h, 16)));
}

/** Splits "path?query#fragment" into the raw path and fragment. */
function pathAndFragment(s: string): { path: string; fragment: string } {
  const hash = s.indexOf("#");
  const fragment = hash >= 0 ? s.slice(hash + 1) : "";
  const beforeHash = hash >= 0 ? s.slice(0, hash) : s;
  const q = beforeHash.indexOf("?");
  return { path: q >= 0 ? beforeHash.slice(0, q) : beforeHash, fragment };
}

/** Matches "scheme://authority" + the rest of an absolute URL. */
const ABS_URL_RE = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)(.*)$/s;

/**
 * Validates a post-login return URL (the rd parameter; port of the Go
 * sanitizeRedirect): a relative path starting with a single "/" (resolved on
 * the hub) or an absolute "scheme://host" URL with the hub's scheme and port
 * whose host is the hub, the base domain or (host mode) a single-label site
 * host. Anything else — other hosts, protocol-relative or opaque URLs
 * ("https:host"), backslashes, control characters ("/\t/evil.com"), user info
 * (even empty), invalid escapes, the login flow's own endpoints — becomes the
 * hub URL, so login can never be turned into an open redirect. The result is
 * serialized by URL (percent-encoded, so it is a valid Location header).
 */
export function safeReturnTo(ctx: RequestContext, raw: string): string {
  const hub = ctx.publicURL + "/";
  const rd = raw.trim();
  // eslint-disable-next-line no-control-regex
  if (!rd || rd.length > MAX_REDIRECT_BYTES || rd.includes("\\") || /[\u0000-\u001f\u007f-\u009f]/.test(rd)) return hub;
  const hubURL = new URL(ctx.publicURL);
  if (rd.startsWith("/")) {
    if (rd.startsWith("//")) return hub;
    const pf = pathAndFragment(rd);
    const p = decodedPath(pf.path, pf.fragment);
    if (p === null || isAuthFlowPath(p)) return hub;
    let u: URL;
    try {
      u = new URL(ctx.publicURL + rd);
    } catch {
      return hub;
    }
    return u.origin === hubURL.origin ? u.toString() : hub;
  }
  const m = ABS_URL_RE.exec(rd);
  if (!m) return hub;
  const [, scheme, authority, rest] = m as unknown as [string, string, string, string];
  // Go: user info (even "@host") is refused; so are escapes in the host (Go
  // allows them only for non-ASCII bytes, which never match a hub host).
  if (authority.includes("@") || authority.includes("%")) return hub;
  let hostname = authority;
  let port = "";
  if (authority.startsWith("[")) {
    const close = authority.indexOf("]");
    if (close < 0) return hub;
    hostname = authority.slice(0, close + 1);
    const after = authority.slice(close + 1);
    if (after && !/^:\d*$/.test(after)) return hub;
    port = after.slice(1);
  } else {
    const colon = authority.lastIndexOf(":");
    if (colon >= 0) {
      if (!/^\d*$/.test(authority.slice(colon + 1))) return hub;
      hostname = authority.slice(0, colon);
      port = authority.slice(colon + 1);
    }
  }
  const pf = pathAndFragment(rest);
  const p = decodedPath(pf.path, pf.fragment);
  if (!hostname || p === null || isAuthFlowPath(p)) return hub;
  const hubScheme = hubURL.protocol.slice(0, -1);
  if (scheme.toLowerCase() !== hubScheme) return hub;
  const defaultPort = hubScheme === "https" ? "443" : "80";
  if ((port || defaultPort) !== (hubURL.port || defaultPort)) return hub;
  const host = hostname.toLowerCase().replace(/\.$/, "");
  const bases = [hubURL.hostname.toLowerCase()];
  if (ctx.cfg.baseDomain && ctx.cfg.baseDomain !== bases[0]) bases.push(ctx.cfg.baseDomain);
  const own = bases.some(
    (base) => host === base || (ctx.cfg.urlMode === "host" && host.endsWith("." + base) && DNS_LABEL_RE.test(host.slice(0, -(base.length + 1)))),
  );
  if (!own) return hub;
  try {
    return new URL(rd).toString();
  } catch {
    return hub;
  }
}

async function serveAuth(ctx: RequestContext): Promise<Response> {
  const p = ctx.path;
  if (p === "/__backyard/auth/cli") {
    // GET, or POST from the confirmation page. Not HEAD: minting a token on
    // HEAD would be a pointless side effect.
    if (ctx.req.method !== "GET" && ctx.req.method !== "POST") throw methodNotAllowed(["GET", "POST"]);
    return serveCLI(ctx);
  }
  if (p === "/__backyard/auth/login") {
    if (ctx.req.method !== "GET" && ctx.req.method !== "HEAD") throw methodNotAllowed(["GET", "HEAD"]);
    // Cloudflare Access (or nothing, in mode none) authenticates page loads
    // itself: going to the target is all a "login" needs (like the Go
    // server's /login in its proxy modes, uncacheable like its noStore).
    const res = redirect(safeReturnTo(ctx, ctx.url.searchParams.get("rd") ?? ""), 302);
    res.headers.set("Cache-Control", "no-store, max-age=0");
    res.headers.set("Pragma", "no-cache");
    res.headers.set("Expires", "0");
    return res;
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

/** Path of the CLI login (SPEC §8.3). */
const CLI_PATH = "/__backyard/auth/cli";
/** How long a CLI login confirmation page stays valid (seconds). */
const CLI_CONFIRM_TTL = 600;
/** Bound of the confirmation form body (redirect_uri and state are at most 2 KiB and 1 KiB). */
const MAX_CLI_FORM_BYTES = 16 << 10;
/** Longest state parameter. */
const MAX_STATE_BYTES = 1024;

/** A validated CLI login request. */
interface CLIRequest {
  /** Raw redirect_uri ("" in display mode). */
  redirectURI: string;
  /** Parsed redirect_uri (null in display mode). */
  target: URL | null;
  state: string;
  display: boolean;
}

/**
 * Validates the parameters of a CLI login (from the query string, or from the
 * confirmation form); an error page when they are invalid.
 */
function parseCLIRequest(q: URLSearchParams): CLIRequest | Response {
  const display = q.get("mode") === "display";
  const state = q.get("state") ?? "";
  if (state.length > MAX_STATE_BYTES) return authErrorPage(400, "Invalid CLI login request", "The state parameter is too long.");
  if (display) return { redirectURI: "", target: null, state, display };
  const redirectURI = q.get("redirect_uri") ?? "";
  let target: URL;
  try {
    target = validateLoopbackRedirect(redirectURI);
  } catch (e) {
    return authErrorPage(400, "Invalid CLI login request", `This link was not created by the Backyard CLI: ${(e as Error).message}.`);
  }
  if (!state) return authErrorPage(400, "Invalid CLI login request", "The state parameter is missing.");
  return { redirectURI, target, state, display };
}

/**
 * Whether raw (a Referer or Origin) has the hub's origin or the origin of the
 * request itself (the hub may be reached under another host, e.g. workers.dev).
 */
function isOwnOrigin(ctx: RequestContext, raw: string): boolean {
  let u: URL;
  try {
    u = new URL(raw.trim());
  } catch {
    return false;
  }
  if (!u.host || u.username || u.password) return false;
  const origin = `${u.protocol}//${u.host}`.toLowerCase();
  return origin === ctx.hubOrigin || origin === ctx.origin;
}

/**
 * Whether a CLI login request may have been started by another site, so that
 * the user must confirm it (port of Go's crossSiteRequest). Per Fetch
 * Metadata, only a same-origin request or a navigation the user started
 * (Sec-Fetch-Site: none, e.g. the CLI opening the URL in the browser) goes
 * ahead; "same-site" counts as cross-site, because sites are subdomains of
 * the hub in host mode and their pages are not trusted. Browsers without
 * Fetch Metadata are judged by the Referer: one from another origin needs
 * confirmation.
 */
export function crossSiteRequest(ctx: RequestContext): boolean {
  const sfs = (ctx.req.headers.get("Sec-Fetch-Site") ?? "").trim().toLowerCase();
  if (sfs === "none" || sfs === "same-origin") return false;
  if (sfs !== "") return true;
  const ref = (ctx.req.headers.get("Referer") ?? "").trim();
  return ref !== "" && !isOwnOrigin(ctx, ref);
}

/**
 * The browser's own signals on a POSTed confirmation (defense in depth; the
 * CSRF token is the actual check): when present, Sec-Fetch-Site must be
 * same-origin and Origin the hub's (or "null", which the confirmation page's
 * no-referrer policy produces).
 */
function confirmationRequestOK(ctx: RequestContext): boolean {
  const sfs = (ctx.req.headers.get("Sec-Fetch-Site") ?? "").trim().toLowerCase();
  if (sfs !== "" && sfs !== "same-origin") return false;
  const o = (ctx.req.headers.get("Origin") ?? "").trim();
  return o === "" || o === "null" || isOwnOrigin(ctx, o);
}

/**
 * HMAC of a confirmation for user, request and expiry. There is no Backyard
 * session to bind to in this edition: Cloudflare Access (or, in mode none,
 * the guest cookie, which is the user id) identifies the browser on every
 * request, so the user id binds the confirmation (like Go's proxy modes).
 */
async function cliConfirmMAC(ctx: RequestContext, u: User, req: CLIRequest, exp: number): Promise<{ key: CryptoKey; msg: Uint8Array }> {
  const { hmac } = await deriveKey(ctx.cfg.secret, KeyLabel.CLIConfirm);
  // A JSON array encodes the fields unambiguously (Go's layout, with an empty session binding).
  const msg = new TextEncoder().encode(JSON.stringify(["cli-confirm", String(exp), u.id, "", req.redirectURI, req.state, req.display ? "display" : ""]));
  return { key: hmac, msg };
}

/** The CSRF token of a confirmation form: "<unix expiry>.<base64url HMAC>". */
async function cliCSRFToken(ctx: RequestContext, u: User, req: CLIRequest, now: number): Promise<string> {
  const exp = now + CLI_CONFIRM_TTL;
  const { key, msg } = await cliConfirmMAC(ctx, u, req, exp);
  return `${exp}.${b64url(new Uint8Array(await crypto.subtle.sign("HMAC", key, msg)))}`;
}

/** Checks a posted CSRF token against the current user and request. */
async function verifyCLIConfirmation(ctx: RequestContext, u: User, req: CLIRequest, token: string): Promise<boolean> {
  const t = token.trim();
  const dot = t.indexOf(".");
  if (dot < 0 || t.length > 128 || !/^\d{1,12}$/.test(t.slice(0, dot))) return false;
  const exp = Number(t.slice(0, dot));
  const now = Math.floor(Date.now() / 1000);
  if (exp <= now || exp > now + CLI_CONFIRM_TTL + 60) return false;
  const mac = b64urlDecode(t.slice(dot + 1));
  if (!mac) return false;
  const { key, msg } = await cliConfirmMAC(ctx, u, req, exp);
  return crypto.subtle.verify("HMAC", key, mac, msg);
}

/** The CSP form-action source of a loopback redirect (CSP cannot name IPv6 hosts: [::1] needs "http:"). */
function formActionSource(u: URL | null): string {
  if (!u) return "";
  return u.hostname.includes(":") ? "http:" : `http://${u.host.toLowerCase()}`;
}

/** Asks the user whether the CLI may sign in as them (a request another site may have started). */
async function renderCLIConfirmation(ctx: RequestContext, u: User, req: CLIRequest): Promise<Response> {
  const who = u.email || u.id;
  const now = Math.floor(Date.now() / 1000);
  const until = new Date((now + ctx.cfg.tokenTTLSeconds) * 1000).toUTCString();
  const form = {
    action: CLI_PATH,
    redirectURI: req.redirectURI,
    state: req.state,
    mode: req.display ? "display" : "",
    csrf: await cliCSRFToken(ctx, u, req, now),
    cancelURL: ctx.publicURL + "/",
    formAction: formActionSource(req.target),
  };
  if (req.display) {
    return cliConfirmPage({
      ...form,
      title: `Create a Backyard CLI token for ${who}?`,
      message: "A page asked Backyard to create a personal API token for your account and show it on the next page.",
      detail: `Only continue if you are signing in the Backyard CLI yourself. The token can act as you until ${until}.`,
    });
  }
  const origin = req.target ? `${req.target.protocol}//${req.target.host}` : "the CLI";
  return cliConfirmPage({
    ...form,
    title: `Allow the Backyard CLI on this computer to sign in as ${who}?`,
    message: `A page asked Backyard to hand a personal API token for your account to ${origin} on this computer.`,
    detail: `Only allow this if you just ran backyard login in a terminal on this computer. The token can act as you until ${until}.`,
  });
}

/**
 * /__backyard/auth/cli?redirect_uri=http://127.0.0.1:PORT/callback&state=…
 * → 302 redirect_uri?token=…&expires_at=…&state=…; ?mode=display shows the
 * token. The browser must be authenticated by the mode's own mechanism
 * (bearer tokens are not accepted, so a token cannot renew itself).
 *
 * A request that another site may have started (see crossSiteRequest) does
 * not mint anything: it gets a confirmation page whose form POSTs back here
 * with a CSRF token bound to the user (SPEC §8.3). Otherwise any web page
 * could navigate a signed-in browser here and have a token for its user
 * delivered to a loopback port — to whatever listens there, which need not be
 * the CLI the user started — or minted for display.
 */
async function serveCLI(ctx: RequestContext): Promise<Response> {
  const confirmed = ctx.req.method === "POST";
  let q = ctx.url.searchParams;
  if (confirmed) {
    const ct = (ctx.req.headers.get("Content-Type") ?? "").split(";")[0]!.trim().toLowerCase();
    let text = "";
    try {
      text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false }).decode(await readBody(ctx.req, MAX_CLI_FORM_BYTES));
    } catch {
      return authErrorPage(400, "Invalid CLI login request", "The confirmation form could not be read.");
    }
    q = new URLSearchParams(ct === "application/x-www-form-urlencoded" ? text : "");
  }
  const req = parseCLIRequest(q);
  if (req instanceof Response) return req;
  if (confirmed && !confirmationRequestOK(ctx)) {
    return authErrorPage(403, "Cannot create a CLI token", "The confirmation was not submitted from Backyard's own page.");
  }
  let u: User;
  try {
    u = await authenticate(ctx, { bearer: false });
  } catch (e) {
    // Signing in again would not tell the accounts apart.
    if (isCookieConflict(e)) return authErrorPage(401, e.title ?? "Not signed in", e.message);
    if (e instanceof HttpError && e.status === 401) {
      return authErrorPage(401, "Not signed in", "Your request was not authenticated by Cloudflare Access. Open this page through the protected address of your Backyard.");
    }
    throw e;
  }
  if (ctx.cfg.authMode !== "none" && (u.anonymous || u.id.startsWith("token:"))) {
    return authErrorPage(403, "Cannot create a CLI token", "Anonymous and static-token identities cannot mint personal API tokens.");
  }
  if (confirmed) {
    if (!(await verifyCLIConfirmation(ctx, u, req, q.get("csrf") ?? ""))) {
      console.warn(JSON.stringify({ audit: true, action: "token.cli_confirm_rejected", actor: u.id }));
      return authErrorPage(403, "Confirmation expired", "This confirmation is no longer valid. Run backyard login again to get a new one.");
    }
  } else if (crossSiteRequest(ctx)) {
    console.log(
      JSON.stringify({ event: "cli_login_confirmation", actor: u.id, sec_fetch_site: truncate(ctx.req.headers.get("Sec-Fetch-Site") ?? "", 32) }),
    );
    return renderCLIConfirmation(ctx, u, req);
  }
  const { token, expiresAt } = await issueAPIToken(ctx.cfg.secret, u, ctx.cfg.tokenTTLSeconds, undefined, adminGroup(ctx));
  console.log(JSON.stringify({ audit: true, action: "token.issue", actor: u.id, via: "cli", display: req.display, confirmed }));
  const expires = formatMicros(expiresAt * 1_000_000);
  if (req.display || !req.target) {
    return tokenPage({
      who: u.email || u.id,
      token,
      expiresAt: new Date(expiresAt * 1000).toUTCString(),
      command: `backyard login --server ${ctx.publicURL} --token ${token}`,
    });
  }
  const target = new URL(req.target.toString());
  target.searchParams.set("token", token);
  target.searchParams.set("expires_at", expires);
  target.searchParams.set("state", req.state);
  // A 303 turns the confirmation POST into a GET of the loopback URL.
  const res = redirect(target.toString(), confirmed ? 303 : 302);
  res.headers.set("Cache-Control", "no-store, max-age=0");
  res.headers.set("Pragma", "no-cache");
  res.headers.set("Expires", "0");
  res.headers.set("Referrer-Policy", "no-referrer");
  return res;
}
