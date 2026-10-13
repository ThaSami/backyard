/**
 * Authentication (SPEC §8, §16): bearer tokens (DEPLOY_TOKEN, Backyard API
 * tokens) in every mode, anonymous signed-cookie identities in AUTH_MODE=none,
 * and Cloudflare Access JWTs (Cf-Access-Jwt-Assertion header or the
 * CF_Authorization cookie, RS256 against the team JWKS) in AUTH_MODE=access.
 */
import { isAdminIdentity, type Config } from "./config";
import { KeyLabel, deriveKey, randomHex, sha256Hex, signValue, timingSafeEqual, verifyValue } from "./crypto";
import { HttpError, bearerToken, getCookie, unauthenticated } from "./http";
import type { User } from "./identity";
import { TokenError, peekIssuer, verifyAPIToken, verifyRS256, type JWK } from "./jwt";
import { siteRoot, type RequestContext } from "./router";

/** Cookie holding the signed anonymous id (auth mode none). */
export const ANON_COOKIE = "backyard_anon";
/** Cookie set by Cloudflare Access on the application domain. */
export const ACCESS_COOKIE = "CF_Authorization";
/** Header Cloudflare Access adds to proxied requests. */
export const ACCESS_HEADER = "Cf-Access-Jwt-Assertion";
/** Name and id prefix of the DEPLOY_TOKEN principal. */
export const DEPLOY_TOKEN_NAME = "deploy";

const ANON_TTL_SECONDS = 365 * 24 * 3600;

/** Options of authenticate. */
export interface AuthOptions {
  /** Accept Authorization: Bearer (default true). */
  bearer?: boolean;
  /** Mint (and set) an anonymous cookie when missing (default true). */
  mintAnon?: boolean;
}

/** The static principal of DEPLOY_TOKEN (admin). */
export function deployTokenUser(): User {
  return { id: "token:" + DEPLOY_TOKEN_NAME, name: DEPLOY_TOKEN_NAME, anonymous: false, admin: true };
}

/** Anonymous guest for a 16-hex id (SPEC §4). */
export function anonUser(id: string): User {
  return { id: "anon:" + id, name: "Guest " + id.slice(0, 4), anonymous: true, admin: false };
}

/** Whether the request carries Authorization: Bearer <token>. */
export function hasBearer(req: Request): boolean {
  return bearerToken(req) !== "";
}

/** Message for requests that need BACKYARD_SECRET. */
export function secretMissing(): HttpError {
  return new HttpError(503, "internal", "this Backyard is not configured yet: BACKYARD_SECRET is missing (run npm run setup)");
}

/**
 * Resolves the user of ctx.req (and stores it in ctx.user). Throws 401 for
 * missing/invalid credentials and 403 for rejected identities. In mode none
 * it never fails without a bearer token: guests get a signed cookie.
 */
export async function authenticate(ctx: RequestContext, opts: AuthOptions = {}): Promise<User> {
  const { cfg, req } = ctx;
  if (!cfg.secret) throw secretMissing();
  const tok = opts.bearer === false ? "" : bearerToken(req);
  if (tok) {
    const u = await authenticateBearer(cfg, tok);
    ctx.user = u;
    return u;
  }
  let u: User;
  if (cfg.authMode === "access") {
    u = await authenticateAccess(ctx);
  } else {
    u = await anonFromCookie(ctx, opts.mintAnon !== false);
  }
  ctx.user = u;
  return u;
}

/** Verifies a bearer token: DEPLOY_TOKEN or a Backyard API token. */
export async function authenticateBearer(cfg: Config, tok: string): Promise<User> {
  if (tok.length > 16 << 10) throw unauthenticated("bearer token too large");
  if (cfg.deployToken && (await timingSafeEqual(tok, cfg.deployToken))) return deployTokenUser();
  if (tok.split(".").length !== 3 || peekIssuer(tok) !== "backyard") {
    throw unauthenticated("invalid or expired token (run: backyard login)");
  }
  let u: User;
  try {
    u = await verifyAPIToken(cfg.secret, tok);
  } catch (e) {
    if (e instanceof TokenError) throw unauthenticated("invalid or expired token (run: backyard login)");
    throw e;
  }
  if (u.id.startsWith("anon:")) {
    if (cfg.authMode !== "none") throw unauthenticated("anonymous tokens are only valid with AUTH_MODE=none");
    u.anonymous = true;
    u.admin = false;
    return u;
  }
  if (u.id.startsWith("token:")) {
    if (u.id === "token:" + DEPLOY_TOKEN_NAME && cfg.deployToken) return deployTokenUser();
    throw unauthenticated("static token principal no longer configured");
  }
  u.anonymous = false;
  u.admin = isAdminIdentity(cfg, u.id, u.email ?? "", u.groups ?? []);
  return u;
}

// --- anonymous cookie ------------------------------------------------------

/** Returns the guest of the backyard_anon cookie, minting one when missing. */
async function anonFromCookie(ctx: RequestContext, mint: boolean): Promise<User> {
  const { hmac } = await deriveKey(ctx.cfg.secret, KeyLabel.Anon);
  const raw = getCookie(ctx.req, ANON_COOKIE);
  if (raw) {
    const payload = await verifyValue(hmac, raw);
    if (payload && payload.length === 16) {
      const id = new TextDecoder().decode(payload);
      if (/^[0-9a-f]{16}$/.test(id)) return anonUser(id);
    }
  }
  const id = randomHex(8);
  if (mint) {
    const value = await signValue(hmac, id);
    ctx.setCookies.push(cookieString(ctx, ANON_COOKIE, value, ANON_TTL_SECONDS));
  }
  return anonUser(id);
}

/** Validates the anon cookie without minting (null when absent/invalid). */
export async function anonCookieUser(ctx: RequestContext): Promise<User | null> {
  const raw = getCookie(ctx.req, ANON_COOKIE);
  if (!raw || !ctx.cfg.secret) return null;
  const { hmac } = await deriveKey(ctx.cfg.secret, KeyLabel.Anon);
  const payload = await verifyValue(hmac, raw);
  if (!payload || payload.length !== 16) return null;
  const id = new TextDecoder().decode(payload);
  return /^[0-9a-f]{16}$/.test(id) ? anonUser(id) : null;
}

/** Synthetic guest keyed by the client address (gateway calls without cookies). */
export async function guestFromIP(ctx: RequestContext): Promise<User> {
  const id = (await sha256Hex("backyard/guest\u0000" + ctx.clientIP)).slice(0, 16);
  return anonUser(id);
}

/** Builds a Set-Cookie value with Backyard's attributes (Domain=.<base> in host mode). */
export function cookieString(ctx: RequestContext, name: string, value: string, maxAge: number): string {
  const parts = [`${name}=${value}`, "Path=/", `Max-Age=${maxAge}`, "HttpOnly", "SameSite=Lax"];
  if (ctx.url.protocol === "https:") parts.push("Secure");
  const base = ctx.cfg.baseDomain;
  if (ctx.cfg.urlMode === "host" && base && (ctx.host === base || ctx.host.endsWith("." + base)) && base.includes(".")) {
    parts.push(`Domain=.${base}`);
  }
  return parts.join("; ");
}

// --- Cloudflare Access -------------------------------------------------------

interface JWKSCache {
  keys: JWK[];
  fetchedAt: number;
}

const jwksCache = new Map<string, JWKSCache>();
const JWKS_TTL_MS = 10 * 60 * 1000;
const JWKS_REFRESH_MIN_MS = 30 * 1000;

/** Fetches (with caching) the signing keys of an Access team. */
export async function accessKeys(team: string, kid: string | undefined, fetcher: typeof fetch = fetch): Promise<JWK[]> {
  const now = Date.now();
  const c = jwksCache.get(team);
  const known = c && (!kid || c.keys.some((k) => k.kid === kid));
  if (c && known && now - c.fetchedAt < JWKS_TTL_MS) return c.keys;
  if (c && !known && now - c.fetchedAt < JWKS_REFRESH_MIN_MS) return c.keys;
  try {
    const res = await fetcher(`https://${team}/cdn-cgi/access/certs`, { headers: { Accept: "application/json" } });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const body = (await res.json()) as { keys?: JWK[] };
    const keys = Array.isArray(body.keys) ? body.keys : [];
    jwksCache.set(team, { keys, fetchedAt: now });
    return keys;
  } catch (e) {
    console.warn(`backyard: auth: fetching Access certs for ${team} failed: ${(e as Error).message}`);
    if (c) return c.keys;
    throw new HttpError(503, "upstream_error", "cannot verify the Cloudflare Access token right now");
  }
}

/** Clears the JWKS and identity caches (tests). */
export function resetAccessCaches(): void {
  jwksCache.clear();
  identityCache.clear();
}

interface AccessIdentity {
  name?: string;
  email?: string;
  groups?: string[];
}

const identityCache = new Map<string, { at: number; id: AccessIdentity | null }>();
const IDENTITY_TTL_MS = 10 * 60 * 1000;

/** Calls https://<team>/cdn-cgi/access/get-identity for name and groups (cached). */
async function accessIdentity(team: string, token: string, cacheKey: string, fetcher: typeof fetch = fetch): Promise<AccessIdentity | null> {
  const now = Date.now();
  const c = identityCache.get(cacheKey);
  if (c && now - c.at < IDENTITY_TTL_MS) return c.id;
  let id: AccessIdentity | null = null;
  try {
    const res = await fetcher(`https://${team}/cdn-cgi/access/get-identity`, {
      headers: { Cookie: `${ACCESS_COOKIE}=${token}`, Accept: "application/json" },
    });
    if (res.ok) {
      const body = (await res.json()) as Record<string, unknown>;
      id = {};
      if (typeof body.name === "string") id.name = body.name;
      if (typeof body.email === "string") id.email = body.email;
      if (Array.isArray(body.groups)) {
        id.groups = body.groups
          .map((g) => (typeof g === "string" ? g : g && typeof g === "object" ? ((g as { name?: unknown }).name as string) : ""))
          .filter((g): g is string => typeof g === "string" && g !== "");
      }
    }
  } catch (e) {
    console.warn(`backyard: auth: Access get-identity failed: ${(e as Error).message}`);
  }
  if (identityCache.size > 1000) identityCache.clear();
  identityCache.set(cacheKey, { at: now, id });
  return id;
}

/** Maps verified Access claims (+ optional identity) to a User. */
export function accessUser(cfg: Config, claims: Record<string, unknown>, ident: AccessIdentity | null): User {
  const email = typeof claims.email === "string" ? claims.email.trim().toLowerCase() : "";
  const sub = typeof claims.sub === "string" ? claims.sub.trim() : "";
  const commonName = typeof claims.common_name === "string" ? claims.common_name.trim() : "";
  let u: User;
  if (email) {
    u = { id: email, email, name: ident?.name?.trim() || (typeof claims.name === "string" && claims.name) || email, anonymous: false, admin: false };
  } else if (commonName) {
    // Access service token (no user): identified by its client id.
    u = { id: "service:" + commonName, name: commonName, anonymous: false, admin: false };
  } else if (sub) {
    u = { id: "sub:" + sub, name: sub, anonymous: false, admin: false };
  } else {
    throw unauthenticated("the Cloudflare Access token has neither an email nor a subject");
  }
  const groups = ident?.groups ?? (Array.isArray(claims.groups) ? claims.groups.filter((g): g is string => typeof g === "string") : []);
  if (groups.length) u.groups = groups;
  const country = claims.country;
  if (typeof country === "string" && country) u.extra = { country };
  u.admin = isAdminIdentity(cfg, u.id, u.email ?? "", u.groups ?? []);
  return u;
}

/** Verifies the Access JWT of a request (header or cookie). */
async function authenticateAccess(ctx: RequestContext): Promise<User> {
  const { cfg, req } = ctx;
  const token = req.headers.get(ACCESS_HEADER) || getCookie(req, ACCESS_COOKIE) || "";
  const login = accessLoginURL(ctx);
  if (!cfg.accessTeam || cfg.accessAud.length === 0) {
    throw new HttpError(503, "internal", "AUTH_MODE=access needs ACCESS_TEAM_DOMAIN and ACCESS_AUD");
  }
  if (!token) throw unauthenticated("Authentication required: sign in through Cloudflare Access.", login);
  let claims: Record<string, unknown>;
  try {
    claims = await verifyRS256(token, {
      keys: (kid) => accessKeys(cfg.accessTeam, kid),
      audiences: cfg.accessAud,
      issuer: `https://${cfg.accessTeam}`,
    });
  } catch (e) {
    if (e instanceof TokenError) throw unauthenticated(`Your Cloudflare Access session is not valid (${e.message}); sign in again.`, login);
    throw e;
  }
  let ident: AccessIdentity | null = null;
  if (cfg.accessGetIdentity && typeof claims.email === "string" && claims.email) {
    const key = `${String(claims.sub ?? "")}:${String(claims.iat ?? "")}:${String(claims.identity_nonce ?? "")}`;
    ident = await accessIdentity(cfg.accessTeam, token, key);
  }
  return accessUser(cfg, claims, ident);
}

/** Where a browser goes to (re-)authenticate: Access intercepts the page itself. */
export function accessLoginURL(ctx: RequestContext): string {
  if (ctx.cfg.authMode !== "access") return "";
  const ref = ctx.req.headers.get("Referer");
  if (ref) {
    try {
      const u = new URL(ref);
      if (`${u.protocol}//${u.host}`.toLowerCase() === ctx.origin) return ref;
    } catch {
      /* fall through */
    }
  }
  return siteRoot(ctx);
}

/** Rate-limit / counter key of a user ("ip:<addr>" for anonymous users). */
export function rateKey(ctx: RequestContext, u: User | undefined): string {
  if (!u || u.anonymous) return "ip:" + ctx.clientIP;
  return "user:" + u.id;
}
