/**
 * The authorization server of the remote MCP connector (SPEC §8.4): the
 * Worker issues its own OAuth 2.1 tokens to MCP clients such as Claude and
 * signs the browser in through its auth mode (Access or guests) at
 * /__backyard/auth/authorize.
 *
 * - Discovery: the protected resource and authorization server metadata.
 * - Clients: allowlisted Client ID Metadata Documents (CIMD, fetched with
 *   redirect:"manual", a size cap, a per-isolate cache, a failure throttle
 *   and single-flight) and stateless, HMAC-signed dynamic registration.
 * - Codes only ever go to an exact entry of MCP_REDIRECT_URIS or to an http
 *   loopback URI; consent is shown on every authorization.
 * - Tokens are opaque values signed with signValue under five HKDF labels;
 *   every use reads the grant in RegistryDO, so revocation is immediate.
 *
 * src/hub.ts routes the connector's paths here; src/mcp.ts and
 * src/mcp-tools.ts use mcpPathGate, mcpAllowedOrigin, verifyMcpAccessToken,
 * mcpChallenge and the URL helpers (mcpIssuer, mcpResource,
 * mcpResourceMetadataURL).
 */
import { authenticate, isCookieConflict, secretMissing } from "./auth";
import { isAdminIdentity, isLoopbackHost, type Config } from "./config";
import { KeyLabel, b64url, b64urlDecode, deriveKey, randomHex, signValue, verifyValue } from "./crypto";
import { HttpError, errorResponse, forbidden, isPlainObject, json, notFound, readBody, requestID, truncate, unwrap } from "./http";
import type { User } from "./identity";
import { authErrorPage, consentPage, formActionSource } from "./pages";
import { parseOrigin, redirect, type RequestContext } from "./router";
import { registry } from "./stubs";

/** The authorize endpoint (GET, POST), served by serveAuth. */
export const AUTHORIZE_PATH = "/__backyard/auth/authorize";
/** The MCP endpoint (src/mcp.ts). */
export const MCP_PATH = "/__backyard/mcp";
/** Prefix of the register, token and revoke endpoints. */
export const OAUTH_PREFIX = "/__backyard/oauth/";
export const OAUTH_REGISTER_PATH = OAUTH_PREFIX + "register";
export const OAUTH_TOKEN_PATH = OAUTH_PREFIX + "token";
export const OAUTH_REVOKE_PATH = OAUTH_PREFIX + "revoke";
/** RFC 8414 authorization server metadata. */
export const AS_METADATA_PATH = "/.well-known/oauth-authorization-server";
/** RFC 9728 protected resource metadata of /__backyard/mcp. */
export const RESOURCE_METADATA_PATH = "/.well-known/oauth-protected-resource" + MCP_PATH;
/** The hub API of a person's connections (SPEC §8.4.7). */
export const GRANTS_API_PATH = "/__backyard/api/mcp/grants";

/** The 404 message of every connector path while the connector is off. */
export const MCP_NOT_ENABLED = "the Claude connector is not enabled on this server";

// Fixed lifetimes (seconds) and limits (SPEC §11).
const CODE_TTL = 300;
const CONSENT_TTL = 600;
const CONSENT_SKEW = 60;
const ACCESS_TTL = 3600;
const PENDING_RETENTION = 600;
const MAX_AUTHORIZE_URI = 2048;
const MAX_STATE = 512;
const MAX_DCR_CLIENT_ID = 1024;
const MAX_DCR_REDIRECTS = 4;
const MAX_REDIRECT_URI_BYTES = 512;
const MAX_CLIENT_NAME = 64;
const MAX_OAUTH_BODY = 16 << 10;
const MAX_BEARER = 4096;
const MAX_GRANT_LIST = 200;
const MAX_FORM_TOKEN = 128;

// CIMD fetch (SPEC §8.4.3).
const CIMD_TIMEOUT_MS = 5000;
const CIMD_MAX_BYTES = 5120;
const CIMD_MIN_CACHE = 60;
const CIMD_MAX_CACHE = 3600;
const CIMD_DEFAULT_CACHE = 300;
const CIMD_FAIL_THROTTLE_MS = 10_000;

// Token prefixes (SPEC §8.4.8).
const PREFIX_CLIENT = "bydc_";
const PREFIX_CODE = "byac_";
const PREFIX_REFRESH = "byrt_";
const PREFIX_ACCESS = "byat_";

const KIND_CIMD = "metadata_document";
const KIND_DCR = "registered";
const SCOPE_MCP = "mcp";
const SCOPE_OFFLINE = "offline_access";

/** The parameters of an authorization request that must not be repeated. */
const AUTHORIZE_PARAMS = ["response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method", "resource", "scope", "state", "response_mode"];

const CHALLENGE_RE = /^[A-Za-z0-9_-]{43}$/;
const VERIFIER_RE = /^[A-Za-z0-9._~-]{43,128}$/;
const GID_RE = /^[0-9a-f]{32}$/;

/** At token time the redirect policy applies too: removing an address ends its connections. */
const REDIRECT_GONE = "the app's redirect address is no longer allowed on this server";

/** One connection: the consent of a user to one MCP client (SPEC §8.4.8). */
export interface McpGrant {
  id: string;
  user_id: string;
  /** Snapshot taken at consent. */
  user: { id: string; email?: string; name: string; groups?: string[]; anonymous: boolean };
  client_id: string;
  client_name: string;
  client_kind: "metadata_document" | "registered";
  /** Host of a client metadata URL; "" for registered clients. */
  client_host: string;
  redirect_uri: string;
  scope: string;
  resource: string;
  /** Unix seconds. */
  created_at: number;
  code_exp: number;
  redeemed_at?: number | null;
  exp: number;
}

/** A grant as RegistryDO stores it: McpGrant plus the PKCE challenge. */
export interface McpGrantRecord extends McpGrant {
  code_challenge: string;
}

/** The fixed error_description values of an invalid access token (SPEC §14.1). */
export type McpTokenError =
  | "malformed token"
  | "token expired"
  | "token revoked"
  | "token not issued for this server"
  | "access removed by policy";

export type McpTokenResult = { ok: true; user: User; grant: McpGrant } | { ok: false; error: McpTokenError };

const enc = new TextEncoder();
const utf8 = new TextDecoder("utf-8", { fatal: true, ignoreBOM: false });

function nowSec(): number {
  return Math.floor(Date.now() / 1000);
}

function byteLength(s: string): number {
  return enc.encode(s).length;
}

// --- enablement, issuer and the canonical host ------------------------------

/**
 * Whether the connector is on for this request: MCP_ENABLED, a valid MCP
 * configuration and an https (or loopback) hub origin.
 */
export function mcpEnabled(ctx: RequestContext): boolean {
  if (!ctx.cfg.mcp.enabled) return false;
  let u: URL;
  try {
    u = new URL(ctx.hubOrigin);
  } catch {
    return false;
  }
  return u.protocol === "https:" || (u.protocol === "http:" && isLoopbackHost(u.hostname));
}

/** P: the canonical hub origin. */
export function mcpIssuer(ctx: RequestContext): string {
  // router.ts derives it from the public URL as scheme://host[:port],
  // lowercased, without the default port.
  return ctx.hubOrigin;
}

/** R: P + "/__backyard/mcp". */
export function mcpResource(ctx: RequestContext): string {
  return mcpIssuer(ctx) + MCP_PATH;
}

/** M: the protected resource metadata URL. */
export function mcpResourceMetadataURL(ctx: RequestContext): string {
  return mcpIssuer(ctx) + RESOURCE_METADATA_PATH;
}

/** H: the hostname of P. */
function hubHost(ctx: RequestContext): string {
  try {
    return new URL(ctx.hubOrigin).hostname.toLowerCase();
  } catch {
    return "";
  }
}

/** Whether the request is addressed to H (case-insensitive, port ignored). */
export function onCanonicalHost(ctx: RequestContext): boolean {
  return ctx.host === hubHost(ctx);
}

/** The normalized origin (scheme://host[:port], lowercase, no default port) of an absolute http(s) URL, or "". */
function originOf(raw: string): string {
  try {
    const u = new URL(raw);
    if ((u.protocol !== "http:" && u.protocol !== "https:") || u.username || u.password) return "";
    return `${u.protocol}//${u.host}`.toLowerCase();
  } catch {
    return "";
  }
}

/** Whether a present Origin header is accepted at /__backyard/mcp (SPEC §14.1 gate step 3). */
export function mcpAllowedOrigin(ctx: RequestContext, origin: string): boolean {
  const o = parseOrigin(origin);
  if (!o) return false;
  if (o === ctx.hubOrigin || o === ctx.origin) return true;
  const { clients, redirectURIs } = ctx.cfg.mcp;
  return redirectURIs.some((r) => r.startsWith("https://") && originOf(r) === o) || clients.some((c) => originOf(c) === o);
}

/**
 * The answer of a connector path that is off or addressed to another host
 * (SPEC §8.4.1), or null when the request may go on.
 */
export function mcpPathGate(ctx: RequestContext): Response | null {
  if (!mcpEnabled(ctx)) return errorResponse(notFound(MCP_NOT_ENABLED));
  if (!onCanonicalHost(ctx)) return errorResponse(notFound("not found"));
  return null;
}

/** Whether p is the grants API (GET/DELETE /__backyard/api/mcp/grants[/{id}]). */
export function isGrantsPath(p: string): boolean {
  return p === GRANTS_API_PATH || p.startsWith(GRANTS_API_PATH + "/");
}

// --- responses -----------------------------------------------------------------

/** An OAuth endpoint answer: JSON, never cached. */
function oauthJSON(body: unknown, status = 200, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store", Pragma: "no-cache", ...headers },
  });
}

/** An RFC 6749 §5.2 error. */
function oauthError(status: number, error: string, description: string, headers: Record<string, string> = {}): Response {
  return oauthJSON({ error, error_description: description }, status, headers);
}

/** 405 on a JSON path of the connector (SPEC §8.4.1). */
function methodNotAllowedJSON(allow: string[]): Response {
  const e = new HttpError(405, "bad_request", "method not allowed");
  e.allow = allow;
  return errorResponse(e);
}

/** Logs an audit line of the connector (SPEC §8.4; never tokens, codes or full grant ids). */
function audit(ctx: RequestContext, level: "log" | "warn", action: string, user: string, g?: Partial<McpGrantRecord> | null, fields: Record<string, unknown> = {}): void {
  const rec: Record<string, unknown> = { audit: true, action, user };
  if (g) {
    rec.client_name = g.client_name ?? "";
    rec.client_kind = g.client_kind ?? "";
    rec.client_host = g.client_host ?? "";
    rec.redirect_host = redirectHost(g.redirect_uri ?? "");
    if (g.id) rec.grant = g.id.slice(0, 8);
  }
  Object.assign(rec, fields);
  if (ctx.clientIP && ctx.clientIP !== "unknown") rec.ip = ctx.clientIP;
  console[level](JSON.stringify(rec));
}

/** The host (lowercase, with port) of a redirect URI as written. */
function redirectHost(uri: string): string {
  const m = /^[A-Za-z][A-Za-z0-9+.-]*:\/\/([^/?#]*)/.exec(uri);
  if (!m) return "";
  const authority = m[1]!;
  return authority.slice(authority.lastIndexOf("@") + 1).toLowerCase();
}

// --- discovery (SPEC §8.4.2) -----------------------------------------------------

/** Serves the two discovery documents (and 404 for other /.well-known/oauth-* paths of the hub). */
export function serveOAuthMetadata(ctx: RequestContext): Response {
  if (!mcpEnabled(ctx)) return errorResponse(notFound(MCP_NOT_ENABLED));
  const p = ctx.path;
  let doc: Record<string, unknown>;
  const P = mcpIssuer(ctx);
  if (p === RESOURCE_METADATA_PATH) {
    doc = { resource: mcpResource(ctx), authorization_servers: [P], scopes_supported: [SCOPE_MCP], bearer_methods_supported: ["header"], resource_name: ctx.cfg.serverName };
  } else if (p === AS_METADATA_PATH) {
    doc = {
      issuer: P,
      authorization_endpoint: P + AUTHORIZE_PATH,
      token_endpoint: P + OAUTH_TOKEN_PATH,
      registration_endpoint: P + OAUTH_REGISTER_PATH,
      revocation_endpoint: P + OAUTH_REVOKE_PATH,
      scopes_supported: [SCOPE_MCP, SCOPE_OFFLINE],
      response_types_supported: ["code"],
      response_modes_supported: ["query"],
      grant_types_supported: ["authorization_code", "refresh_token"],
      token_endpoint_auth_methods_supported: ["none"],
      revocation_endpoint_auth_methods_supported: ["none"],
      code_challenge_methods_supported: ["S256"],
      authorization_response_iss_parameter_supported: true,
    };
    // Without allowlisted documents, clients fall back to registration.
    if (ctx.cfg.mcp.clients.length > 0) doc.client_id_metadata_document_supported = true;
  } else {
    return errorResponse(notFound("not found"));
  }
  if (!onCanonicalHost(ctx)) return errorResponse(notFound("not found"));
  const m = ctx.req.method;
  if (m !== "GET" && m !== "HEAD") return methodNotAllowedJSON(["GET", "HEAD"]);
  return new Response(m === "HEAD" ? null : JSON.stringify(doc), {
    status: 200,
    headers: { "Content-Type": "application/json", "Cache-Control": "public, max-age=300" },
  });
}

// --- register, token, revoke ------------------------------------------------------

/** Serves /__backyard/oauth/{register,token,revoke} (POST; no cookies, no X-Backyard). */
export async function serveOAuth(ctx: RequestContext): Promise<Response> {
  if (!mcpEnabled(ctx)) return errorResponse(notFound(MCP_NOT_ENABLED));
  const p = ctx.path;
  const h = p === OAUTH_REGISTER_PATH ? serveRegister : p === OAUTH_TOKEN_PATH ? serveToken : p === OAUTH_REVOKE_PATH ? serveRevoke : null;
  if (!h) return errorResponse(notFound("not found"));
  if (!onCanonicalHost(ctx)) return errorResponse(notFound("not found"));
  if (ctx.req.method !== "POST") return methodNotAllowedJSON(["POST"]);
  if (!ctx.cfg.secret) throw secretMissing();
  return h(ctx);
}

/** The media type of a Content-Type header (lowercase, without parameters). */
function mediaType(ctx: RequestContext): string {
  return (ctx.req.headers.get("Content-Type") ?? "").split(";")[0]!.trim().toLowerCase();
}

/** Reads a UTF-8 body of at most 16 KiB; null when it is larger or not UTF-8. */
async function readSmallBody(ctx: RequestContext): Promise<string | null> {
  try {
    return utf8.decode(await readBody(ctx.req, MAX_OAUTH_BODY));
  } catch {
    return null;
  }
}

/** POST /__backyard/oauth/register (RFC 7591, stateless; SPEC §8.4.3). */
async function serveRegister(ctx: RequestContext): Promise<Response> {
  const bad = (error: string, desc: string) => oauthError(400, error, desc);
  if (mediaType(ctx) !== "application/json") return bad("invalid_client_metadata", "the request body must be application/json");
  const text = await readSmallBody(ctx);
  let meta: unknown = null;
  try {
    meta = text === null ? null : (JSON.parse(text) as unknown);
  } catch {
    meta = null;
  }
  if (!isPlainObject(meta)) return bad("invalid_client_metadata", "the request body must be one JSON object of at most 16 KiB");
  const uris = meta.redirect_uris;
  if (!Array.isArray(uris) || uris.length === 0 || uris.length > MAX_DCR_REDIRECTS || !uris.every((u) => typeof u === "string")) {
    return bad("invalid_redirect_uri", "redirect_uris must be an array of 1 to 4 redirect URIs");
  }
  for (const u of uris as string[]) {
    if (byteLength(u) > MAX_REDIRECT_URI_BYTES) return bad("invalid_redirect_uri", "redirect URIs are limited to 512 bytes");
    if (!redirectAllowed(ctx.cfg, u)) {
      return bad("invalid_redirect_uri", `redirect URI ${u} is not allowed on this server (allowed: the URIs in MCP_REDIRECT_URIS and http loopback URIs)`);
    }
  }
  let name = "";
  if (meta.client_name !== undefined && meta.client_name !== null) {
    if (typeof meta.client_name !== "string") return bad("invalid_client_metadata", "client_name must be a string");
    name = meta.client_name;
  }
  name = sanitizeClientName(name);
  const t = nowSec();
  const clientId = PREFIX_CLIENT + (await signValue(await keyOf(ctx, KeyLabel.MCPClient), JSON.stringify({ v: 1, n: name, r: uris, t })));
  if (clientId.length > MAX_DCR_CLIENT_ID) return bad("invalid_client_metadata", "too many or too long redirect_uris");
  console.debug(JSON.stringify({ event: "mcp.register", client_name: name, redirect_uris: uris.length }));
  return oauthJSON(
    {
      client_id: clientId,
      client_id_issued_at: t,
      client_name: name,
      redirect_uris: uris,
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      token_endpoint_auth_method: "none",
    },
    201,
  );
}

/**
 * Parses a query string or form body as Go's url.ParseQuery does: a ";"
 * separator or a malformed %-escape makes it invalid (null), so both editions
 * refuse the same requests.
 */
export function parseQueryStrict(raw: string): URLSearchParams | null {
  for (const pair of raw.split("&")) {
    if (pair.includes(";") || /%(?![0-9A-Fa-f]{2})/.test(pair)) return null;
  }
  return new URLSearchParams(raw);
}

/** Reads an application/x-www-form-urlencoded body (≤ 16 KiB, no repeated parameter); a string on error. */
async function readOAuthForm(ctx: RequestContext): Promise<URLSearchParams | string> {
  if (mediaType(ctx) !== "application/x-www-form-urlencoded") return "the request body must be application/x-www-form-urlencoded";
  const text = await readSmallBody(ctx);
  if (text === null) return "the request body is limited to 16 KiB of UTF-8";
  const form = parseQueryStrict(text);
  if (!form) return "the request body is not a valid form";
  for (const k of new Set(form.keys())) {
    if (form.getAll(k).length > 1) return `parameter ${truncate(k, 64)} is repeated`;
  }
  return form;
}

/** Decodes a form-urlencoded component (Go's url.QueryUnescape); null when malformed. */
function queryUnescape(s: string): string | null {
  try {
    return decodeURIComponent(s.replace(/\+/g, " "));
  } catch {
    return null;
  }
}

/**
 * Authenticates the public client of a token or revocation request (SPEC
 * §8.4.6): client_id in the body and/or as HTTP Basic with an empty
 * password, no secret, and a known client (nothing is fetched). Returns the
 * client_id or the error response.
 */
async function tokenClient(ctx: RequestContext, form: URLSearchParams): Promise<string | Response> {
  const invalidClient = (challenge: boolean, desc: string) =>
    oauthError(401, "invalid_client", desc, challenge ? { "WWW-Authenticate": 'Basic realm="Backyard"' } : {});
  const publicOnly = "only public clients are supported: send client_id without a secret";
  let id = form.get("client_id") ?? "";
  const auth = ctx.req.headers.get("Authorization");
  if (auth !== null) {
    const m = /^basic\s+([A-Za-z0-9+/=]+)\s*$/i.exec(auth.trim());
    let user: string | null = null;
    let pass: string | null = null;
    if (m) {
      try {
        const raw = utf8.decode(Uint8Array.from(atob(m[1]!), (c) => c.charCodeAt(0)));
        const colon = raw.indexOf(":");
        if (colon >= 0) {
          user = queryUnescape(raw.slice(0, colon));
          pass = queryUnescape(raw.slice(colon + 1));
        }
      } catch {
        user = null;
      }
    }
    if (user === null || pass === null || user === "" || pass !== "") return invalidClient(true, publicOnly);
    if (id !== "" && id !== user) return oauthError(400, "invalid_request", "client_id differs between the body and the Authorization header");
    id = user;
  }
  if ((form.get("client_secret") ?? "") !== "") return invalidClient(true, publicOnly);
  if (id === "") return invalidClient(false, "client_id is required");
  if (!(await knownClient(ctx, id))) return invalidClient(false, "unknown client");
  return id;
}

/** 500 server_error naming the request id. */
function serverError(ctx: RequestContext, what: string, e: unknown): Response {
  console.error(`backyard: mcp: ${what} failed: ${(e as Error)?.message ?? e}`);
  return oauthError(500, "server_error", `request ${requestID(ctx.req)}`);
}

/** POST /__backyard/oauth/token (SPEC §8.4.6). */
async function serveToken(ctx: RequestContext): Promise<Response> {
  const form = await readOAuthForm(ctx);
  if (typeof form === "string") return oauthError(400, "invalid_request", form);
  const clientID = await tokenClient(ctx, form);
  if (clientID instanceof Response) return clientID;
  if (form.has("resource") && !resourceOK(ctx, form.get("resource")!)) {
    return oauthError(400, "invalid_target", "resource must be " + mcpResource(ctx));
  }
  const gt = form.get("grant_type") ?? "";
  try {
    if (gt === "authorization_code") return await exchangeCode(ctx, form, clientID);
    if (gt === "refresh_token") return await refreshToken(ctx, form, clientID);
  } catch (e) {
    return serverError(ctx, gt === "refresh_token" ? "refresh" : "code exchange", e);
  }
  if (gt === "") return oauthError(400, "invalid_request", "grant_type is required");
  return oauthError(400, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token");
}

/** grant_type=authorization_code. */
async function exchangeCode(ctx: RequestContext, form: URLSearchParams, clientID: string): Promise<Response> {
  const invalid = (desc: string) => oauthError(400, "invalid_grant", desc);
  const code = form.get("code") ?? "";
  const verifier = form.get("code_verifier") ?? "";
  if (code === "" || verifier === "") return oauthError(400, "invalid_request", "code and code_verifier are required");
  const now = nowSec();
  const p = await parseCode(ctx, code);
  if (!p || p.x < now) return invalid("the authorization code is invalid or expired");
  const g = unwrap(await registry(ctx.env).mcpGrantGet(p.g));
  // A redeemed grant is not rejected here: a replay is only acted on once
  // PKCE has verified (rotate detects it).
  if (!g || g.code_exp < now) return invalid("the authorization code is invalid or expired");
  if (g.client_id !== clientID) return invalid("the authorization code was issued to another client");
  if (!redirectAllowed(ctx.cfg, g.redirect_uri)) return invalid(REDIRECT_GONE);
  if (form.has("redirect_uri") && form.get("redirect_uri") !== g.redirect_uri) return invalid("redirect_uri does not match the authorization request");
  if (!VERIFIER_RE.test(verifier) || !(await pkceMatches(verifier, g.code_challenge))) return invalid("PKCE verification failed");
  const familyExp = g.created_at + ctx.cfg.tokenTTLSeconds;
  const outcome = unwrap(await registry(ctx.env).mcpRotate(g.id, 0, now, false, familyExp));
  if (outcome === "reuse") {
    audit(ctx, "warn", "mcp.code_replay", g.user_id, g);
    audit(ctx, "log", "mcp.revoke", g.user_id, g, { via: "replay" });
    return invalid("the authorization code was already used; the connection has been revoked");
  }
  if (outcome !== "ok") return invalid("the authorization code is invalid or expired");
  g.redeemed_at = now;
  g.exp = familyExp;
  const denied = await regateForToken(ctx, g);
  if (denied) return denied;
  const access = await issueAccess(ctx, g, now);
  if (!access) return invalid("the connection has expired");
  audit(ctx, "log", "mcp.token", g.user_id, g);
  return oauthJSON({ access_token: access.token, token_type: "Bearer", expires_in: access.expiresIn, refresh_token: await signRefresh(ctx, g.id, 1), scope: g.scope });
}

/** grant_type=refresh_token. */
async function refreshToken(ctx: RequestContext, form: URLSearchParams, clientID: string): Promise<Response> {
  const invalid = (desc: string) => oauthError(400, "invalid_grant", desc);
  const raw = form.get("refresh_token") ?? "";
  if (raw === "") return oauthError(400, "invalid_request", "refresh_token is required");
  const p = await parseRefresh(ctx, raw);
  if (!p) return invalid("the refresh token is invalid");
  const now = nowSec();
  const g = unwrap(await registry(ctx.env).mcpGrantGet(p.g));
  if (!g || !live(g, now)) return invalid("the connection was revoked or has expired");
  if (g.client_id !== clientID) return invalid("the refresh token was issued to another client");
  if (!redirectAllowed(ctx.cfg, g.redirect_uri)) return invalid(REDIRECT_GONE);
  let scope = g.scope;
  const asked = (form.get("scope") ?? "").split(/\s+/).filter(Boolean);
  if (asked.length > 0) {
    const granted = g.scope.split(" ");
    const out: string[] = [];
    for (const s of asked) {
      if (!granted.includes(s)) return oauthError(400, "invalid_scope", `the requested scope exceeds the granted scope (${g.scope})`);
      if (!out.includes(s)) out.push(s);
    }
    scope = out.join(" ");
  }
  const outcome = unwrap(await registry(ctx.env).mcpRotate(g.id, p.n, now, true, g.exp));
  if (outcome === "reuse") {
    audit(ctx, "warn", "mcp.refresh_reuse", g.user_id, g);
    audit(ctx, "log", "mcp.revoke", g.user_id, g, { via: "replay" });
    return invalid("the refresh token was already used; the connection has been revoked");
  }
  if (outcome !== "ok" && outcome !== "grace") return invalid("the connection was revoked or has expired");
  const denied = await regateForToken(ctx, g);
  if (denied) return denied;
  const access = await issueAccess(ctx, g, now);
  if (!access) return invalid("the connection has expired");
  return oauthJSON({ access_token: access.token, token_type: "Bearer", expires_in: access.expiresIn, refresh_token: await signRefresh(ctx, g.id, p.n + 1), scope });
}

/** Re-gates a grant's user at the token endpoint; on a policy failure revokes it and answers invalid_grant. */
async function regateForToken(ctx: RequestContext, g: McpGrantRecord): Promise<Response | null> {
  if (regate(ctx.cfg, g)) return null;
  await revokeGrant(ctx, g.id, null, "policy");
  return oauthError(400, "invalid_grant", "access removed by policy");
}

/** POST /__backyard/oauth/revoke (RFC 7009; 200 for anything, so it is no oracle). */
async function serveRevoke(ctx: RequestContext): Promise<Response> {
  const form = await readOAuthForm(ctx);
  if (typeof form === "string") return oauthError(400, "invalid_request", form);
  const clientID = await tokenClient(ctx, form);
  if (clientID instanceof Response) return clientID;
  const tok = form.get("token") ?? "";
  if (tok === "") return oauthError(400, "invalid_request", "token is required");
  const gid = (await parseRefresh(ctx, tok))?.g ?? (await parseAccess(ctx, tok))?.g ?? "";
  if (gid) {
    try {
      const g = unwrap(await registry(ctx.env).mcpGrantGet(gid));
      if (g && g.client_id === clientID) await revokeGrant(ctx, gid, null, "rfc7009");
    } catch (e) {
      return serverError(ctx, "revocation", e);
    }
  }
  return new Response(null, { status: 200, headers: { "Cache-Control": "no-store", Pragma: "no-cache" } });
}

// --- clients (SPEC §8.4.3) ---------------------------------------------------------

/** A resolved client_id. */
interface McpClient {
  id: string;
  kind: "metadata_document" | "registered";
  /** Sanitized display name. */
  name: string;
  /** CIMD: the client_id URL's host; DCR: "". */
  host: string;
  redirects: string[];
}

/** A client_id that cannot be used: the error page of the authorize endpoint. */
interface ClientError {
  status: number;
  title: string;
  message: string;
  detail?: string;
}

const UNKNOWN_APP: ClientError = {
  status: 400,
  title: "Unknown app",
  message: "This sign-in link names an app that Backyard does not know. Go back to the app and connect again.",
};

/** Resolves a client_id for the authorize endpoint (it may fetch a client metadata document). */
async function resolveClient(ctx: RequestContext, id: string): Promise<McpClient | ClientError> {
  if (id.startsWith("https://")) {
    if (!ctx.cfg.mcp.clients.includes(id)) {
      return {
        status: 400,
        title: "App not allowed",
        message: `The app ${id} is not allowed to connect to this Backyard.`,
        detail: "An administrator can allow it by adding its client metadata URL to MCP_CLIENTS.",
      };
    }
    try {
      return await cimdDocument(ctx.cfg, id);
    } catch (e) {
      return {
        status: 502,
        title: "Could not verify the app",
        message: `Backyard could not load the app's client metadata document ${id}: ${(e as Error).message}.`,
        detail: 'Backyard needs outbound HTTPS to that address. Without it, an administrator can set MCP_CLIENTS to an empty value and the app can be added with "Register automatically" instead.',
      };
    }
  }
  if (id.startsWith(PREFIX_CLIENT)) return (await verifyDCRClient(ctx, id)) ?? UNKNOWN_APP;
  return UNKNOWN_APP;
}

/** Whether id is an allowlisted CIMD URL or a DCR id with a valid MAC (token and revocation endpoints; never fetches). */
async function knownClient(ctx: RequestContext, id: string): Promise<boolean> {
  if (id.startsWith("https://")) return ctx.cfg.mcp.clients.includes(id);
  return (await verifyDCRClient(ctx, id)) !== null;
}

/** Checks a DCR client_id and returns its client. */
async function verifyDCRClient(ctx: RequestContext, id: string): Promise<McpClient | null> {
  if (!id.startsWith(PREFIX_CLIENT) || id.length > MAX_DCR_CLIENT_ID) return null;
  const p = await parseSigned(ctx, id, PREFIX_CLIENT, KeyLabel.MCPClient);
  if (!p || p.v !== 1 || typeof p.n !== "string" || typeof p.t !== "number" || !Array.isArray(p.r)) return null;
  const r = p.r as unknown[];
  if (r.length === 0 || r.length > MAX_DCR_REDIRECTS || !r.every((x) => typeof x === "string")) return null;
  return { id, kind: KIND_DCR, name: sanitizeClientName(p.n), host: "", redirects: r as string[] };
}

/** The bidi controls removed from client names (besides Cc). */
const NAME_STRIP_RE = /[\p{Cc}‎‏‪-‮⁦-⁩]/gu;

/**
 * Strips control characters (Cc) and bidi controls (U+200E, U+200F,
 * U+202A–U+202E, U+2066–U+2069) from a client-provided name, trims it and
 * cuts it to 64 code points.
 */
function cleanClientName(s: string): string {
  s = s.toWellFormed().replace(NAME_STRIP_RE, "").trim();
  const cps = [...s];
  return cps.length > MAX_CLIENT_NAME ? cps.slice(0, MAX_CLIENT_NAME).join("").trim() : s;
}

/** cleanClientName, or "MCP client" when nothing is left. */
function sanitizeClientName(s: string): string {
  return cleanClientName(s) || "MCP client";
}

// --- redirect policy and matching (SPEC §8.4.4) -------------------------------------

/** A parsed http loopback redirect URI. */
interface LoopbackURI {
  /** "localhost", "127.0.0.1" or "[::1]". */
  host: string;
  /** "" when absent. */
  port: string;
  /** Path and query, as sent. */
  rest: string;
}

/**
 * Parses an http loopback URI: scheme http, host exactly localhost,
 * 127.0.0.1 or [::1], an optional port, no userinfo or fragment, any path
 * and query; null otherwise.
 */
export function parseLoopbackURI(raw: string): LoopbackURI | null {
  if (!raw.startsWith("http://") || /[#\\\s]/.test(raw)) return null;
  try {
    new URL(raw);
  } catch {
    return null;
  }
  const s = raw.slice("http://".length);
  const cut = s.search(/[/?]/);
  const authority = cut >= 0 ? s.slice(0, cut) : s;
  const rest = cut >= 0 ? s.slice(cut) : "";
  if (authority.includes("@")) return null;
  let host: string;
  let tail: string;
  if (authority.startsWith("[::1]")) {
    host = "[::1]";
    tail = authority.slice("[::1]".length);
  } else {
    const colon = authority.indexOf(":");
    host = colon >= 0 ? authority.slice(0, colon) : authority;
    tail = colon >= 0 ? authority.slice(colon) : "";
  }
  if (host !== "localhost" && host !== "127.0.0.1" && host !== "[::1]") return null;
  let port = "";
  if (tail !== "") {
    const m = /^:(\d+)$/.exec(tail);
    const n = m ? Number(m[1]) : 0;
    if (!m || n < 1 || n > 65535) return null;
    port = m[1]!;
  }
  return { host, port, rest };
}

/** The redirect policy: an exact entry of MCP_REDIRECT_URIS, or an http loopback URI. */
export function redirectAllowed(cfg: Config, uri: string): boolean {
  return cfg.mcp.redirectURIs.includes(uri) || parseLoopbackURI(uri) !== null;
}

/**
 * Whether a requested redirect_uri matches a registered one: byte equality,
 * except that for loopback URIs the port is ignored on both sides (scheme,
 * host string, path and query must be equal).
 */
export function redirectMatches(registered: string, requested: string): boolean {
  if (requested.includes("#")) return false;
  const authority = /^[A-Za-z][A-Za-z0-9+.-]*:\/\/([^/?#]*)/.exec(requested)?.[1] ?? "";
  if (authority.includes("@")) return false;
  const reg = parseLoopbackURI(registered);
  const req = parseLoopbackURI(requested);
  if (reg && req) return reg.host === req.host && reg.rest === req.rest;
  return registered === requested;
}

/** Whether requested matches one of the client's URIs and passes the current redirect policy. */
function clientRedirect(cfg: Config, c: McpClient, requested: string): boolean {
  if (requested === "" || !redirectAllowed(cfg, requested)) return false;
  return c.redirects.some((reg) => redirectMatches(reg, requested));
}

/**
 * Canonicalizes a resource parameter (SPEC §8.4.1): an absolute http(s) URI
 * without userinfo, query or fragment; scheme and host lowercased, the
 * default port dropped, one trailing "/" removed from a non-empty path,
 * percent-encoding kept as is. null when it is not such a URI.
 */
export function canonicalResource(v: string): string | null {
  if (/[?#]/.test(v)) return null;
  const m = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/]*)(.*)$/s.exec(v);
  if (!m) return null;
  const scheme = m[1]!.toLowerCase();
  let host = m[2]!.toLowerCase();
  let path = m[3]!;
  if ((scheme !== "http" && scheme !== "https") || host === "" || host.includes("@")) return null;
  try {
    new URL(v);
  } catch {
    return null;
  }
  const def = scheme === "https" ? ":443" : ":80";
  if (host.endsWith(def)) host = host.slice(0, -def.length);
  if (path !== "" && path.endsWith("/")) path = path.slice(0, -1);
  return `${scheme}://${host}${path}`;
}

/** Whether a present resource parameter names R. */
function resourceOK(ctx: RequestContext, v: string): boolean {
  return canonicalResource(v) === mcpResource(ctx);
}

// --- client metadata documents (SPEC §8.4.3) ----------------------------------------

/** The result of one fetch: a client and how long it may be cached (s; 0 = not at all), or why it failed. */
type CimdFetch = { doc: McpClient; ttl: number } | { err: string };

/** Cache state of one allowlisted client metadata URL (per isolate). */
interface CimdEntry {
  doc?: McpClient;
  /** ms */
  expires: number;
  failErr?: string;
  /** ms: no new fetch before this after a failure. */
  failUntil: number;
  /** The in-flight fetch shared by concurrent requests. */
  inflight?: Promise<CimdFetch>;
}

const cimdCache = new Map<string, CimdEntry>();

/** Clears the client metadata cache (tests). */
export function resetMcpCaches(): void {
  cimdCache.clear();
}

/**
 * The client of an allowlisted metadata URL from the cache or a fetch;
 * throws an Error whose message is the reason it cannot be used. Concurrent
 * requests share one fetch; a failure is not cached, but the URL is not
 * fetched again for 10 s (requests in that window get the same failure).
 */
async function cimdDocument(cfg: Config, id: string): Promise<McpClient> {
  let e = cimdCache.get(id);
  if (!e) {
    e = { expires: 0, failUntil: 0 };
    cimdCache.set(id, e);
  }
  const now = Date.now();
  if (e.doc && now < e.expires) return e.doc;
  if (e.failErr && now < e.failUntil) throw new Error(e.failErr);
  let pending = e.inflight;
  if (!pending) {
    const entry = e;
    pending = entry.inflight = fetchCIMD(cfg, id)
      .catch((err: unknown): CimdFetch => ({ err: `the request failed (${truncate(String((err as Error)?.message ?? err), 120)})` }))
      .then((r) => {
        entry.inflight = undefined;
        if ("err" in r) {
          entry.doc = undefined;
          entry.failErr = r.err;
          entry.failUntil = Date.now() + CIMD_FAIL_THROTTLE_MS;
          console.warn(`backyard: mcp: the client metadata document ${id} could not be loaded: ${r.err}`);
        } else if (r.ttl > 0) {
          entry.doc = r.doc;
          entry.expires = Date.now() + r.ttl * 1000;
          entry.failErr = undefined;
        } else {
          // no-store / no-cache: used for this request only.
          entry.doc = undefined;
          entry.failErr = undefined;
        }
        return r;
      });
  }
  const r = await pending;
  if ("err" in r) throw new Error(r.err);
  return r.doc;
}

/** Reads at most max bytes of a body stream; null when it is longer. */
async function readCapped(body: ReadableStream<Uint8Array> | null, max: number): Promise<Uint8Array | null> {
  if (!body) return new Uint8Array(0);
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > max) {
      await reader.cancel().catch(() => {});
      return null;
    }
    chunks.push(value);
  }
  const out = new Uint8Array(total);
  let off = 0;
  for (const c of chunks) {
    out.set(c, off);
    off += c.byteLength;
  }
  return out;
}

/** Fetches and validates one client metadata document: no redirects, 5 s, ≤ 5120 bytes. */
async function fetchCIMD(cfg: Config, id: string): Promise<CimdFetch> {
  let res: Response;
  try {
    res = await fetch(id, { method: "GET", redirect: "manual", signal: AbortSignal.timeout(CIMD_TIMEOUT_MS), headers: { Accept: "application/json" } });
  } catch (e) {
    const name = (e as Error)?.name ?? "";
    if (name === "TimeoutError" || name === "AbortError") return { err: "the request timed out after 5 s" };
    return { err: `the request failed (${truncate(String((e as Error)?.message ?? e), 120)})` };
  }
  const discard = () => res.body?.cancel().catch(() => {});
  if (res.status >= 300 && res.status < 400) {
    discard();
    return { err: `it answered with a redirect (HTTP ${res.status}), which is not followed` };
  }
  if (res.status !== 200) {
    discard();
    return { err: `it answered HTTP ${res.status}` };
  }
  const ct = res.headers.get("Content-Type") ?? "";
  const mt = ct.split(";")[0]!.trim().toLowerCase();
  if (mt !== "application/json" && !mt.endsWith("+json")) {
    discard();
    return { err: `it is not JSON (Content-Type ${JSON.stringify(truncate(ct, 64))})` };
  }
  const declared = Number(res.headers.get("Content-Length") ?? "");
  if (Number.isFinite(declared) && declared > CIMD_MAX_BYTES) {
    discard();
    return { err: `it is larger than ${CIMD_MAX_BYTES} bytes` };
  }
  let body: Uint8Array | null;
  try {
    body = await readCapped(res.body, CIMD_MAX_BYTES);
  } catch (e) {
    const name = (e as Error)?.name ?? "";
    return { err: name === "TimeoutError" || name === "AbortError" ? "the request timed out after 5 s" : "reading it failed" };
  }
  if (!body) return { err: `it is larger than ${CIMD_MAX_BYTES} bytes` };
  const doc = parseCIMD(cfg, id, body);
  if (typeof doc === "string") return { err: doc };
  return { doc, ttl: cimdCacheTTL(res.headers.get("Cache-Control") ?? "") };
}

/** Validates a client metadata document; a string is the reason it is refused. */
function parseCIMD(cfg: Config, id: string, body: Uint8Array): McpClient | string {
  let text: string;
  try {
    text = utf8.decode(body);
  } catch {
    return "it is not UTF-8 JSON";
  }
  let doc: unknown;
  try {
    doc = JSON.parse(text) as unknown;
  } catch {
    doc = null;
  }
  if (!isPlainObject(doc)) return "it is not a JSON object";
  if (doc.client_id !== id) return "its client_id is not its own URL";
  const uris = doc.redirect_uris;
  if (!Array.isArray(uris) || uris.length === 0 || uris.length > 10 || !uris.every((u) => typeof u === "string")) {
    return "redirect_uris must be an array of 1 to 10 URIs";
  }
  const allowed = (uris as string[]).filter((u) => redirectAllowed(cfg, u));
  if (allowed.length === 0) return "none of its redirect_uris is allowed on this server (MCP_REDIRECT_URIS and http loopback URIs)";
  if (Object.hasOwn(doc, "token_endpoint_auth_method") && doc.token_endpoint_auth_method !== "none") {
    return "it asks for client authentication (token_endpoint_auth_method), which Backyard does not support";
  }
  if (Object.hasOwn(doc, "client_secret") || Object.hasOwn(doc, "client_secret_expires_at")) return "it contains a client secret";
  const host = new URL(id).host.toLowerCase();
  let name = host;
  if (doc.client_name !== undefined && doc.client_name !== null) {
    if (typeof doc.client_name !== "string") return "client_name is not a string";
    name = cleanClientName(doc.client_name) || host;
  }
  return { id, kind: KIND_CIMD, name, host, redirects: allowed };
}

/**
 * How long a document may be cached according to its Cache-Control (s):
 * max-age clamped to 60–3600, 300 without one, 0 for no-store or no-cache.
 */
export function cimdCacheTTL(cacheControl: string): number {
  let ttl = CIMD_DEFAULT_CACHE;
  for (const d of cacheControl.split(",")) {
    const [rawName, rawVal] = d.split("=", 2) as [string, string | undefined];
    const name = rawName.trim().toLowerCase();
    if (name === "no-store" || name === "no-cache") return 0;
    if (name === "max-age" && rawVal !== undefined) {
      const v = rawVal.trim().replace(/^"|"$/g, "");
      if (/^\d+$/.test(v)) ttl = Math.min(CIMD_MAX_CACHE, Math.max(CIMD_MIN_CACHE, Number(v)));
    }
  }
  return ttl;
}

// --- token formats (SPEC §8.4.8) ------------------------------------------------------

async function keyOf(ctx: RequestContext, label: string): Promise<CryptoKey> {
  return (await deriveKey(ctx.cfg.secret, label)).hmac;
}

/** prefix + signValue(key(label), compact JSON of payload). */
async function signToken(ctx: RequestContext, prefix: string, label: string, payload: unknown): Promise<string> {
  return prefix + (await signValue(await keyOf(ctx, label), JSON.stringify(payload)));
}

/** Verifies prefix and MAC of raw and returns its JSON object payload, or null. */
async function parseSigned(ctx: RequestContext, raw: string, prefix: string, label: string): Promise<Record<string, unknown> | null> {
  if (!raw.startsWith(prefix)) return null;
  const payload = await verifyValue(await keyOf(ctx, label), raw.slice(prefix.length));
  if (!payload) return null;
  try {
    const v = JSON.parse(utf8.decode(payload)) as unknown;
    return isPlainObject(v) ? v : null;
  } catch {
    return null;
  }
}

/** Whether v has exactly the given keys. */
function exactKeys(v: Record<string, unknown>, keys: string[]): boolean {
  const k = Object.keys(v);
  return k.length === keys.length && keys.every((x) => Object.hasOwn(v, x));
}

function isInt(v: unknown): v is number {
  return typeof v === "number" && Number.isSafeInteger(v);
}

function isGID(v: unknown): v is string {
  return typeof v === "string" && GID_RE.test(v);
}

/** An authorization code's payload {g, x}, or null. */
async function parseCode(ctx: RequestContext, raw: string): Promise<{ g: string; x: number } | null> {
  const p = await parseSigned(ctx, raw, PREFIX_CODE, KeyLabel.MCPCode);
  return p && exactKeys(p, ["g", "x"]) && isGID(p.g) && isInt(p.x) ? { g: p.g, x: p.x } : null;
}

/** RT(gid, n): deterministic, so a grace answer returns the identical string. */
function signRefresh(ctx: RequestContext, gid: string, n: number): Promise<string> {
  return signToken(ctx, PREFIX_REFRESH, KeyLabel.MCPRefresh, { g: gid, n });
}

/** A refresh token's payload {g, n ≥ 1}, or null. */
async function parseRefresh(ctx: RequestContext, raw: string): Promise<{ g: string; n: number } | null> {
  const p = await parseSigned(ctx, raw, PREFIX_REFRESH, KeyLabel.MCPRefresh);
  return p && exactKeys(p, ["g", "n"]) && isGID(p.g) && isInt(p.n) && p.n >= 1 ? { g: p.g, n: p.n } : null;
}

/** An access token's payload {g, i, x}, or null (also for tokens over 4096 bytes). */
async function parseAccess(ctx: RequestContext, raw: string): Promise<{ g: string; i: number; x: number } | null> {
  if (raw.length > MAX_BEARER) return null;
  const p = await parseSigned(ctx, raw, PREFIX_ACCESS, KeyLabel.MCPAccess);
  return p && exactKeys(p, ["g", "i", "x"]) && isGID(p.g) && isInt(p.i) && isInt(p.x) ? { g: p.g, i: p.i, x: p.x } : null;
}

/** An access token for g: 1 h, never past the grant's expiry; null when the grant has expired. */
async function issueAccess(ctx: RequestContext, g: McpGrantRecord, now: number): Promise<{ token: string; expiresIn: number } | null> {
  const exp = Math.min(now + ACCESS_TTL, g.exp);
  if (exp <= now) return null;
  return { token: await signToken(ctx, PREFIX_ACCESS, KeyLabel.MCPAccess, { g: g.id, i: now, x: exp }), expiresIn: exp - now };
}

/** BASE64URL(SHA-256(verifier)) equals the challenge, compared in constant time. */
async function pkceMatches(verifier: string, challenge: string): Promise<boolean> {
  const sum = b64url(new Uint8Array(await crypto.subtle.digest("SHA-256", enc.encode(verifier))));
  if (sum.length !== challenge.length) return false;
  let diff = 0;
  for (let i = 0; i < sum.length; i++) diff |= sum.charCodeAt(i) ^ challenge.charCodeAt(i);
  return diff === 0;
}

// --- grants, verification and re-gating (SPEC §8.4.8) ---------------------------------

/** Whether a grant is redeemed and unexpired at now. */
function live(g: McpGrant, now: number): boolean {
  return g.redeemed_at !== null && g.redeemed_at !== undefined && g.exp > now;
}

/** The exported view of a stored grant (without the PKCE challenge). */
function publicGrant(g: McpGrantRecord): McpGrant {
  const { code_challenge: _challenge, ...rest } = g;
  void _challenge;
  return rest;
}

/**
 * Rebuilds the user of a grant's snapshot and applies the current policy
 * (SPEC §8.4.8): a guest only while AUTH_MODE is none, never a static-token
 * principal; admin is recomputed from ADMINS / ADMIN_GROUPS. null when the
 * policy refuses it.
 */
function regate(cfg: Config, g: McpGrant): User | null {
  const s = g.user;
  if (!s || typeof s.id !== "string" || s.id === "") return null;
  if (s.id.startsWith("anon:")) {
    if (cfg.authMode !== "none") return null;
    return { id: s.id, name: s.name || "Guest " + s.id.slice(5, 9), anonymous: true, admin: false };
  }
  if (s.id.startsWith("token:") || s.anonymous) return null;
  const u: User = { id: s.id, name: s.name || s.id, anonymous: false, admin: false };
  if (s.email) u.email = s.email;
  if (Array.isArray(s.groups) && s.groups.length) u.groups = [...s.groups];
  u.admin = isAdminIdentity(cfg, u.id, u.email ?? "", u.groups ?? []);
  return u;
}

/** Deletes one grant and audits it (actor null: the grant's own user). Returns the deleted grant. */
async function revokeGrant(ctx: RequestContext, gid: string, actor: User | null, via: string): Promise<McpGrantRecord | null> {
  const g = unwrap(await registry(ctx.env).mcpGrantRevoke(gid));
  if (g) audit(ctx, "log", "mcp.revoke", actor?.id ?? g.user_id, g, { via });
  return g;
}

/**
 * Verifies an MCP access token and re-gates its user (SPEC §8.4.8). Throws on
 * infrastructure failures (answer 503).
 */
export async function verifyMcpAccessToken(ctx: RequestContext, raw: string): Promise<McpTokenResult> {
  if (!ctx.cfg.secret) throw secretMissing();
  const p = await parseAccess(ctx, raw);
  if (!p) return { ok: false, error: "malformed token" };
  const now = nowSec();
  if (p.x <= now) return { ok: false, error: "token expired" };
  // No cache: every request reads the grant, so a revocation takes effect at once.
  const g = unwrap(await registry(ctx.env).mcpGrantGet(p.g));
  if (!g || !live(g, now)) return { ok: false, error: "token revoked" };
  if (g.resource !== mcpResource(ctx)) return { ok: false, error: "token not issued for this server" };
  const user = regate(ctx.cfg, g);
  if (!user) {
    await revokeGrant(ctx, g.id, null, "policy");
    return { ok: false, error: "access removed by policy" };
  }
  return { ok: true, user, grant: publicGrant(g) };
}

/** The exact WWW-Authenticate value (SPEC §14.1); no error gives the missing-token form. */
export function mcpChallenge(ctx: RequestContext, error?: McpTokenError): string {
  const m = mcpResourceMetadataURL(ctx);
  if (!error) return `Bearer resource_metadata="${m}", scope="${SCOPE_MCP}"`;
  return `Bearer error="invalid_token", error_description="${error}", resource_metadata="${m}", scope="${SCOPE_MCP}"`;
}

// --- the authorize endpoint (SPEC §8.4.5) -----------------------------------------------

/** A validated authorization request. */
interface AuthzRequest {
  client: McpClient;
  redirectURI: string;
  /** "" when absent. */
  state: string;
  challenge: string;
  /** The granted scope string. */
  scope: string;
}

function badAuthorizeRequest(detail: string): Response {
  return authErrorPage(400, "Invalid authorization request", "This sign-in link is malformed. Go back to the app and connect again.", { detail });
}

/** Go's url.QueryEscape: everything but A-Z a-z 0-9 - _ . ~ is escaped, space as "+". */
function queryEscape(s: string): string {
  return encodeURIComponent(s.toWellFormed())
    .replace(/[!'()*]/g, (c) => "%" + c.charCodeAt(0).toString(16).toUpperCase())
    .replace(/%20/g, "+");
}

/**
 * Redirects to redirectURI with the given parameters (empty values are
 * skipped) and iss appended to its query (? or &).
 */
function redirectWith(ctx: RequestContext, redirectURI: string, status: number, params: [string, string][]): Response {
  let loc = redirectURI;
  let sep = "?";
  if (redirectURI.includes("?")) sep = redirectURI.endsWith("?") || redirectURI.endsWith("&") ? "" : "&";
  for (const [k, v] of [...params, ["iss", mcpIssuer(ctx)] as [string, string]]) {
    if (v === "") continue;
    loc += `${sep}${k}=${queryEscape(v)}`;
    sep = "&";
  }
  // A Location header holds bytes: escape what a registered URI may hold beyond ASCII.
  loc = loc.toWellFormed().replace(/[^\x21-\x7e]/g, (c) => encodeURIComponent(c));
  const res = redirect(loc, status);
  res.headers.set("Cache-Control", "no-store");
  res.headers.set("Pragma", "no-cache");
  res.headers.set("Referrer-Policy", "no-referrer");
  return res;
}

function redirectError(ctx: RequestContext, redirectURI: string, state: string, code: string, desc: string, status: number): Response {
  return redirectWith(ctx, redirectURI, status, [
    ["error", code],
    ["error_description", desc],
    ["state", state],
  ]);
}

/** Serves /__backyard/auth/authorize (GET: the consent page; POST: the decision). */
export async function serveAuthorize(ctx: RequestContext): Promise<Response> {
  if (!mcpEnabled(ctx)) return authErrorPage(404, "Claude connector not enabled", "The Claude connector is not enabled on this server.");
  if (!onCanonicalHost(ctx)) return authErrorPage(404, "Wrong address", `Use ${mcpResource(ctx)} to connect an app to this Backyard.`);
  if (ctx.req.method !== "GET" && ctx.req.method !== "POST") return authErrorPage(405, "Method not allowed", "Use GET or POST.", { allow: "GET, POST" });
  if (!ctx.cfg.secret) return authErrorPage(503, "Unavailable", secretMissing().message);
  return ctx.req.method === "GET" ? authorizeGET(ctx) : authorizePOST(ctx);
}

/** GET: validates the request, signs the browser in and renders the consent page. */
async function authorizeGET(ctx: RequestContext): Promise<Response> {
  const uri = ctx.url.pathname + ctx.url.search;
  if (byteLength(uri) > MAX_AUTHORIZE_URI) return badAuthorizeRequest("The request is longer than 2048 bytes.");
  const query = parseQueryStrict(ctx.url.search.slice(1));
  if (!query) return badAuthorizeRequest("The query string cannot be parsed.");
  const req = await parseAuthorize(ctx, query, 302, false);
  if (req instanceof Response) return req;
  const u = await consentUser(ctx, false);
  if (u instanceof Response) return u;
  return renderConsent(ctx, u, req);
}

/** POST: the consent decision; only an explicit Allow creates a grant and a code. */
async function authorizePOST(ctx: RequestContext): Promise<Response> {
  const text = mediaType(ctx) === "application/x-www-form-urlencoded" ? await readSmallBody(ctx) : null;
  const form = text === null ? null : parseQueryStrict(text);
  if (!form) return badAuthorizeRequest("The approval form could not be read.");
  if (!consentSignalsOK(ctx)) {
    audit(ctx, "warn", "mcp.consent_rejected", "unknown", null, { reason: "browser signals", sec_fetch_site: truncate(ctx.req.headers.get("Sec-Fetch-Site") ?? "", 32) });
    return authErrorPage(403, "This approval was not sent from the Backyard page", "Backyard only accepts an approval submitted from its own consent page. Go back to the app and connect again.");
  }
  const req = await parseAuthorize(ctx, form, 303, true);
  if (req instanceof Response) return req;
  const u = await consentUser(ctx, true);
  if (u instanceof Response) return u;
  const pseudo = { client_id: req.client.id, client_name: req.client.name, client_kind: req.client.kind, client_host: req.client.host, redirect_uri: req.redirectURI };
  if (!(await verifyConsentCSRF(ctx, u, req, form.get("csrf") ?? ""))) {
    audit(ctx, "warn", "mcp.consent_rejected", u.id, pseudo, { reason: "csrf" });
    return authErrorPage(403, "Approval expired", "This approval page expired or was not issued to you. Go back to the app and connect again.");
  }
  const decision = form.get("decision");
  if (decision === "deny") {
    audit(ctx, "log", "mcp.deny", u.id, pseudo);
    return redirectError(ctx, req.redirectURI, req.state, "access_denied", "the user did not allow the connection", 303);
  }
  if (decision !== "allow") return badAuthorizeRequest("The approval form has no valid decision.");
  return allow(ctx, u, req);
}

/**
 * The browser's own signals on a consent POST (defense in depth; the CSRF
 * token is the actual check).
 */
function consentSignalsOK(ctx: RequestContext): boolean {
  const h = ctx.req.headers;
  const sfs = (h.get("Sec-Fetch-Site") ?? "").trim().toLowerCase();
  if (sfs !== "" && sfs !== "same-origin") return false;
  const mode = (h.get("Sec-Fetch-Mode") ?? "").trim().toLowerCase();
  if (mode !== "" && mode !== "navigate") return false;
  const dest = (h.get("Sec-Fetch-Dest") ?? "").trim().toLowerCase();
  if (dest !== "" && dest !== "document") return false;
  const o = (h.get("Origin") ?? "").trim();
  if (o === "" || o === "null") return true;
  const po = parseOrigin(o);
  return po !== "" && (po === ctx.hubOrigin || po === ctx.origin);
}

/**
 * Validates an authorization request (SPEC §8.4.5 steps 2–5) from the query
 * (GET) or the consent form (POST). Errors before the redirect URI is
 * validated render a page and never redirect; later ones redirect with
 * status redirectStatus.
 */
async function parseAuthorize(ctx: RequestContext, q: URLSearchParams, redirectStatus: number, post: boolean): Promise<AuthzRequest | Response> {
  for (const k of AUTHORIZE_PARAMS) {
    if (q.getAll(k).length > 1) return badAuthorizeRequest(`The parameter ${k} appears more than once.`);
  }
  const client = await resolveClient(ctx, q.get("client_id") ?? "");
  if ("status" in client) return authErrorPage(client.status, client.title, client.message, client.detail ? { detail: client.detail } : {});
  const redirectURI = q.get("redirect_uri") ?? "";
  if (!clientRedirect(ctx.cfg, client, redirectURI)) {
    return authErrorPage(
      400,
      "Redirect address not allowed",
      "The app asked Backyard to send the approval to an address that is not registered for it or not allowed on this Backyard.",
      { detail: "Allowed are the addresses in MCP_REDIRECT_URIS and http loopback addresses on this computer." },
    );
  }
  // From here on, errors go back to the app.
  const state = q.get("state") ?? "";
  const stateOK = byteLength(state) <= MAX_STATE;
  const fail = (code: string, desc: string) => redirectError(ctx, redirectURI, stateOK ? state : "", code, desc, redirectStatus);
  if (q.get("response_type") !== "code") return fail("unsupported_response_type", "response_type must be code");
  if (q.has("request")) return fail("request_not_supported", "request objects are not supported");
  if (q.has("request_uri")) return fail("request_uri_not_supported", "request_uri is not supported");
  if (q.has("response_mode") && q.get("response_mode") !== "query") return fail("invalid_request", "response_mode must be query");
  if (q.get("code_challenge_method") !== "S256") return fail("invalid_request", "PKCE with S256 is required");
  const challenge = q.get("code_challenge") ?? "";
  if (!CHALLENGE_RE.test(challenge)) return fail("invalid_request", "code_challenge must be a base64url SHA-256 value (43 characters)");
  if (q.has("resource")) {
    if (!resourceOK(ctx, q.get("resource")!)) return fail("invalid_target", "resource must be " + mcpResource(ctx));
  } else if (post) {
    return fail("invalid_request", "resource is required");
  }
  if (!stateOK) return fail("invalid_request", "state is limited to 512 bytes");
  const scope = (q.get("scope") ?? "").split(" ").includes(SCOPE_OFFLINE) ? `${SCOPE_MCP} ${SCOPE_OFFLINE}` : SCOPE_MCP;
  return { client, redirectURI, state, challenge, scope };
}

/** Whether u may consent: never a static-token principal, and a guest only in mode none. */
function canConnectApps(mode: Config["authMode"], u: User): boolean {
  return !u.id.startsWith("token:") && (mode === "none" || (!u.anonymous && !u.id.startsWith("anon:")));
}

/**
 * The browser identity at the authorize endpoint (SPEC §8.4.5 step 6, §8.4.9):
 * the mode's own credentials (Access, or the guest cookie), never a bearer
 * token. On failure, the error page.
 */
async function consentUser(ctx: RequestContext, post: boolean): Promise<User | Response> {
  let u: User;
  try {
    // A POST never mints a guest: one without the cookie cannot match the CSRF token anyway.
    u = await authenticate(ctx, { bearer: false, mintAnon: !post });
  } catch (e) {
    if (isCookieConflict(e)) return authErrorPage(401, e.title ?? "Conflicting sign-in cookies", e.message);
    if (e instanceof HttpError && e.status === 401) {
      return authErrorPage(401, "Sign-in required", `Cloudflare Access did not authenticate this request; ${AUTHORIZE_PATH} must stay behind it.`);
    }
    if (e instanceof HttpError && e.status === 403) {
      return authErrorPage(403, "Access denied", "Your account is not allowed to use Backyard.", { linkURL: "/__backyard/auth/logout", linkText: "Sign out" });
    }
    console.error(`backyard: mcp: authorize could not authenticate the browser: ${(e as Error)?.message ?? e}`);
    return authErrorPage(503, "Sign-in unavailable", "Backyard could not verify your identity right now. Please try again in a moment.");
  }
  if (!canConnectApps(ctx.cfg.authMode, u)) {
    return authErrorPage(403, "This sign-in cannot connect apps", "Anonymous and static-token identities cannot connect apps to Backyard. Sign in with your own account.");
  }
  return u;
}

/**
 * The consent CSRF message for user, request and expiry (SPEC §8.4.5). There
 * is no Backyard session in this edition: Cloudflare Access (or the guest
 * cookie, which is the user id) identifies the browser on every request, so
 * the binding is "" and the user id binds the token.
 */
function consentMessage(ctx: RequestContext, u: User, req: AuthzRequest, exp: number): Uint8Array {
  return enc.encode(JSON.stringify(["mcp-consent", exp, u.id, "", req.client.id, req.redirectURI, req.state, req.challenge, req.scope, mcpResource(ctx)]));
}

/** The CSRF token of a consent form: "<unix expiry>.<base64url HMAC>". */
async function consentToken(ctx: RequestContext, u: User, req: AuthzRequest, exp: number): Promise<string> {
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", await keyOf(ctx, KeyLabel.MCPConsent), consentMessage(ctx, u, req, exp)));
  return `${exp}.${b64url(mac)}`;
}

/** Checks a consent form's CSRF token (an expired token is accepted for 60 s of clock skew). */
async function verifyConsentCSRF(ctx: RequestContext, u: User, req: AuthzRequest, token: string): Promise<boolean> {
  const t = token.trim();
  const m = /^(\d{1,15})\.([A-Za-z0-9_-]+)$/.exec(t);
  if (!m || token.length > MAX_FORM_TOKEN) return false;
  const exp = Number(m[1]);
  const now = nowSec();
  if (exp < now - CONSENT_SKEW || exp > now + CONSENT_TTL + CONSENT_SKEW) return false;
  const mac = b64urlDecode(m[2]!);
  if (!mac) return false;
  return crypto.subtle.verify("HMAC", await keyOf(ctx, KeyLabel.MCPConsent), mac, consentMessage(ctx, u, req, exp));
}

/** The name the consent page shows for u: email, else name, else id (the guest's name in mode none). */
function consentWho(u: User): string {
  if (u.anonymous && u.name) return u.name;
  return u.email || u.name || u.id;
}

/** Renders the consent page. It is shown on every authorization, whatever Sec-Fetch-Site says. */
async function renderConsent(ctx: RequestContext, u: User, req: AuthzRequest): Promise<Response> {
  const target = new URL(req.redirectURI);
  const now = nowSec();
  const exp = now + CONSENT_TTL;
  const fields: [string, string][] = [
    ["response_type", "code"],
    ["client_id", req.client.id],
    ["redirect_uri", req.redirectURI],
  ];
  if (req.state) fields.push(["state", req.state]);
  fields.push(["code_challenge", req.challenge], ["code_challenge_method", "S256"], ["scope", req.scope], ["resource", mcpResource(ctx)]);
  fields.push(["csrf", await consentToken(ctx, u, req, exp)]);
  return consentPage({
    who: consentWho(u),
    guest: ctx.cfg.authMode === "none" && u.anonymous,
    // Both modes have a logout: the guest cookie is cleared, or Access signs out.
    signOutURL: "/__backyard/auth/logout",
    clientName: req.client.name,
    verified: req.client.kind === KIND_CIMD,
    clientHost: req.client.host,
    origin: `${target.protocol}//${target.host}`.toLowerCase(),
    loopback: parseLoopbackURI(req.redirectURI) !== null,
    hubHost: hubHost(ctx),
    until: new Date((now + ctx.cfg.tokenTTLSeconds) * 1000).toISOString().slice(0, 10),
    fields,
    formAction: formActionSource(target),
  });
}

/** Allow: creates the pending grant and redirects with the code. */
async function allow(ctx: RequestContext, u: User, req: AuthzRequest): Promise<Response> {
  const now = nowSec();
  const user: McpGrant["user"] = { id: u.id, name: u.name || u.id, anonymous: !!u.anonymous };
  if (u.email) user.email = u.email;
  if (u.groups?.length) user.groups = [...u.groups];
  const g: McpGrantRecord = {
    id: randomHex(16),
    user_id: u.id,
    user,
    client_id: req.client.id,
    client_name: req.client.name,
    client_kind: req.client.kind,
    client_host: req.client.host,
    redirect_uri: req.redirectURI,
    code_challenge: req.challenge,
    scope: req.scope,
    resource: mcpResource(ctx),
    created_at: now,
    code_exp: now + CODE_TTL,
    redeemed_at: null,
    exp: now + CODE_TTL + PENDING_RETENTION,
  };
  try {
    unwrap(await registry(ctx.env).mcpGrantCreate(g));
  } catch (e) {
    console.error(`backyard: mcp: the connection of ${u.id} could not be stored: ${(e as Error)?.message ?? e}`);
    return authErrorPage(503, "Connection unavailable", "Backyard could not save this connection right now. Please try again in a moment.");
  }
  const code = await signToken(ctx, PREFIX_CODE, KeyLabel.MCPCode, { g: g.id, x: g.code_exp });
  audit(ctx, "log", "mcp.authorize", u.id, g, { scope: g.scope });
  return redirectWith(ctx, req.redirectURI, 303, [
    ["code", code],
    ["state", req.state],
  ]);
}

// --- the grants API (SPEC §8.4.7) ----------------------------------------------------------

/** RFC 3339 (UTC, whole seconds) of a unix time. */
function rfc3339(sec: number): string {
  return new Date(sec * 1000).toISOString().replace(/\.\d{3}Z$/, "Z");
}

/** A Grant of the grants API. */
function grantJSON(g: McpGrant): Record<string, unknown> {
  return {
    id: g.id,
    user: { id: g.user.id, email: g.user.email ?? "", name: g.user.name },
    client_name: g.client_name,
    client_kind: g.client_kind,
    client_host: g.client_host,
    redirect_host: redirectHost(g.redirect_uri),
    scope: g.scope,
    created_at: rfc3339(g.created_at),
    expires_at: rfc3339(g.exp),
  };
}

/**
 * The person ?user= names: the caller when absent; otherwise admins only (403),
 * except that selfOK (DELETE) lets anyone name themselves.
 */
function grantsSubject(ctx: RequestContext, u: User, selfOK: boolean): string {
  const who = (ctx.url.searchParams.get("user") ?? "").trim();
  if (who === "" || (selfOK && who === u.id)) return u.id;
  if (!u.admin) throw forbidden("only admins can see or disconnect another person's connections");
  return who;
}

function requireEnabled(ctx: RequestContext): void {
  if (!mcpEnabled(ctx)) throw notFound(MCP_NOT_ENABLED);
}

/** GET /__backyard/api/mcp/grants[?user=<id>]: live grants, newest first, at most 200. */
export async function listMcpGrants(ctx: RequestContext, u: User): Promise<Response> {
  requireEnabled(ctx);
  const subject = grantsSubject(ctx, u, false);
  const list = unwrap(await registry(ctx.env).mcpGrantsByUser(subject, MAX_GRANT_LIST));
  return json({ grants: list.map(grantJSON) });
}

/** DELETE /__backyard/api/mcp/grants/{id}: for the grant's user or an admin (404 otherwise). */
export async function revokeMcpGrant(ctx: RequestContext, u: User, params: string[]): Promise<Response> {
  requireEnabled(ctx);
  const id = params[0] ?? "";
  const missing = () => notFound("no such connection");
  if (!GID_RE.test(id)) throw missing();
  const g = unwrap(await registry(ctx.env).mcpGrantGet(id));
  if (!g || (g.user_id !== u.id && !u.admin)) throw missing();
  if (!(await revokeGrant(ctx, id, u, "hub"))) throw missing();
  return new Response(null, { status: 204, headers: { "Cache-Control": "no-store" } });
}

/** DELETE /__backyard/api/mcp/grants[?user=<id>]: every connection of a person. */
export async function revokeMcpGrants(ctx: RequestContext, u: User): Promise<Response> {
  requireEnabled(ctx);
  const subject = grantsSubject(ctx, u, true);
  const n = unwrap(await registry(ctx.env).mcpRevokeUser(subject));
  audit(ctx, "log", "mcp.revoke", u.id, null, { via: "hub", count: n, subject });
  return json({ revoked: n });
}
