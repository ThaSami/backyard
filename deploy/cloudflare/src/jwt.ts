/**
 * JSON Web Tokens: Backyard API tokens (HS256, SPEC §8.3, interoperable with
 * internal/auth/tokens.go) and verification of Cloudflare Access tokens
 * (RS256 against the team's JWKS).
 */
import { KeyLabel, b64url, b64urlDecode, deriveKey, randomHex } from "./crypto";
import type { User } from "./identity";

const enc = new TextEncoder();
const dec = new TextDecoder();

/** "iss" of Backyard API tokens. */
export const TOKEN_ISSUER = "backyard";
/** "scope" of Backyard API tokens. */
export const TOKEN_SCOPE = "api";
/** Clock skew tolerated for exp/iat/nbf. */
const LEEWAY_SECONDS = 30;
/** Largest token accepted. */
const MAX_TOKEN_BYTES = 16 << 10;

/** Claims of a Backyard API token (field order matches the Go encoder). */
export interface APITokenClaims {
  sub: string;
  email?: string;
  name?: string;
  groups?: string[];
  admin?: boolean;
  scope: string;
  iss: string;
  jti: string;
  iat: number;
  exp: number;
}

interface JWTParts {
  header: Record<string, unknown>;
  payload: Record<string, unknown>;
  signingInput: Uint8Array;
  signature: Uint8Array;
}

/** Splits and decodes a compact JWS; null when malformed. */
export function parseJWT(token: string): JWTParts | null {
  if (!token || token.length > MAX_TOKEN_BYTES) return null;
  const parts = token.split(".");
  if (parts.length !== 3) return null;
  const [h, p, s] = parts as [string, string, string];
  const hb = b64urlDecode(h);
  const pb = b64urlDecode(p);
  const sb = b64urlDecode(s);
  if (!hb || !pb || !sb) return null;
  try {
    const header = JSON.parse(dec.decode(hb)) as unknown;
    const payload = JSON.parse(dec.decode(pb)) as unknown;
    if (!header || typeof header !== "object" || !payload || typeof payload !== "object" || Array.isArray(payload)) return null;
    return {
      header: header as Record<string, unknown>,
      payload: payload as Record<string, unknown>,
      signingInput: enc.encode(h + "." + p),
      signature: sb,
    };
  } catch {
    return null;
  }
}

/** Returns the "iss" claim without verifying the token ("" when unreadable). */
export function peekIssuer(token: string): string {
  const parts = token.split(".");
  if (parts.length !== 3) return "";
  const pb = b64urlDecode(parts[1]!);
  if (!pb) return "";
  try {
    const p = JSON.parse(dec.decode(pb)) as { iss?: unknown };
    return typeof p.iss === "string" ? p.iss : "";
  } catch {
    return "";
  }
}

/** Longest display name carried by a token (in code points, like Go's runes). */
const MAX_TOKEN_NAME = 256;

/**
 * Mints an API token for u valid for ttlSeconds.
 *
 * Like the Go issuer, the token always fits the size verifiers accept: an
 * overlong name is shortened and, if needed, trailing groups are dropped.
 * important (optional) moves the groups that must survive (admin groups) to
 * the front first.
 */
export async function issueAPIToken(
  secret: string,
  u: User,
  ttlSeconds: number,
  nowSeconds = Math.floor(Date.now() / 1000),
  important?: (group: string) => boolean,
): Promise<{ token: string; expiresAt: number }> {
  if (!u.id || !u.id.trim()) throw new Error("cannot issue a token for a user without id");
  let groups = [...(u.groups ?? [])];
  if (important) groups = [...groups.filter((g) => important(g)), ...groups.filter((g) => !important(g))];
  const name = [...(u.name ?? "").trim()].slice(0, MAX_TOKEN_NAME).join("");
  const jti = randomHex(16);
  const exp = nowSeconds + Math.max(1, Math.floor(ttlSeconds));
  const { hmac } = await deriveKey(secret, KeyLabel.APIToken);
  const header = b64url(enc.encode(JSON.stringify({ alg: "HS256", typ: "JWT" })));
  const sign = async (gs: string[]): Promise<string> => {
    // Go's field order: sub, email, name, groups, admin, scope, iss, jti, iat, exp.
    const body: Record<string, unknown> = { sub: u.id };
    if (u.email) body.email = u.email;
    if (name) body.name = name;
    if (gs.length) body.groups = gs;
    if (u.admin) body.admin = true;
    body.scope = TOKEN_SCOPE;
    body.iss = TOKEN_ISSUER;
    body.jti = jti;
    body.iat = nowSeconds;
    body.exp = exp;
    const payload = b64url(enc.encode(JSON.stringify(body)));
    const sig = new Uint8Array(await crypto.subtle.sign("HMAC", hmac, enc.encode(header + "." + payload)));
    return `${header}.${payload}.${b64url(sig)}`;
  };
  let token = await sign(groups);
  if (token.length > MAX_TOKEN_BYTES) {
    // Keep the largest prefix of groups that fits (binary search).
    let lo = 0;
    let hi = groups.length;
    while (lo < hi) {
      const mid = Math.floor((lo + hi + 1) / 2);
      if ((await sign(groups.slice(0, mid))).length <= MAX_TOKEN_BYTES) lo = mid;
      else hi = mid - 1;
    }
    token = await sign(groups.slice(0, lo));
    if (token.length > MAX_TOKEN_BYTES) throw new Error("user profile too large for an api token");
  }
  return { token, expiresAt: exp };
}

/** Why a token was rejected (never shown verbatim to clients). */
export class TokenError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "TokenError";
  }
}

function numericClaim(p: Record<string, unknown>, k: string): number | undefined {
  const v = p[k];
  return typeof v === "number" && Number.isFinite(v) ? v : undefined;
}

/**
 * Verifies a Backyard API token and returns the user it represents (admin as
 * recorded in the token; "anon:" subjects are anonymous). Only HS256 with the
 * HKDF-derived key is accepted; iss must be "backyard" and scope "api".
 */
export async function verifyAPIToken(secret: string, token: string, nowSeconds = Math.floor(Date.now() / 1000)): Promise<User> {
  const t = parseJWT(token);
  if (!t) throw new TokenError("malformed api token");
  if (t.header.alg !== "HS256") throw new TokenError("unexpected signing algorithm");
  const { hmac } = await deriveKey(secret, KeyLabel.APIToken);
  if (!(await crypto.subtle.verify("HMAC", hmac, t.signature, t.signingInput))) throw new TokenError("bad signature");
  const p = t.payload;
  if (p.iss !== TOKEN_ISSUER) throw new TokenError("wrong issuer");
  const exp = numericClaim(p, "exp");
  const iat = numericClaim(p, "iat");
  if (exp === undefined) throw new TokenError("token without exp");
  if (iat === undefined) throw new TokenError("token without iat");
  if (nowSeconds > exp + LEEWAY_SECONDS) throw new TokenError("token expired");
  if (iat > nowSeconds + LEEWAY_SECONDS) throw new TokenError("token used before issued");
  const nbf = numericClaim(p, "nbf");
  if (nbf !== undefined && nbf > nowSeconds + LEEWAY_SECONDS) throw new TokenError("token not valid yet");
  if (p.scope !== TOKEN_SCOPE) throw new TokenError("wrong scope");
  const sub = typeof p.sub === "string" ? p.sub.trim() : "";
  if (!sub) throw new TokenError("token without subject");
  const email = typeof p.email === "string" ? p.email.trim().toLowerCase() : "";
  const name = typeof p.name === "string" ? p.name.trim() : "";
  const groups = Array.isArray(p.groups) ? p.groups.filter((g): g is string => typeof g === "string" && g.trim() !== "").map((g) => g.trim()) : [];
  const u: User = { id: sub, name: name || sub, anonymous: sub.startsWith("anon:"), admin: p.admin === true };
  if (email) u.email = email;
  if (groups.length) u.groups = groups;
  return u;
}

// --- RS256 (Cloudflare Access) ----------------------------------------------

/** A JSON Web Key as served by .../cdn-cgi/access/certs. */
export interface JWK {
  kid?: string;
  kty?: string;
  alg?: string;
  n?: string;
  e?: string;
  use?: string;
}

const importedKeys = new Map<string, Promise<CryptoKey>>();

function importRSAKey(jwk: JWK): Promise<CryptoKey> {
  const id = `${jwk.kid ?? ""}:${jwk.n ?? ""}`;
  let p = importedKeys.get(id);
  if (!p) {
    p = crypto.subtle.importKey(
      "jwk",
      { kty: "RSA", n: jwk.n, e: jwk.e, alg: "RS256", ext: true } as JsonWebKey,
      { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
      false,
      ["verify"],
    );
    if (importedKeys.size > 64) importedKeys.clear();
    importedKeys.set(id, p);
    p.catch(() => importedKeys.delete(id));
  }
  return p;
}

/** Options of verifyRS256. */
export interface RS256Options {
  /** Returns the candidate keys for a kid (may refetch the JWKS). */
  keys: (kid: string | undefined) => Promise<JWK[]>;
  /** Accepted audiences (any match). */
  audiences: string[];
  /** Required issuer ("" = not checked). */
  issuer: string;
  nowSeconds?: number;
}

/** Verifies an RS256 JWT and returns its claims. Throws TokenError. */
export async function verifyRS256(token: string, opts: RS256Options): Promise<Record<string, unknown>> {
  const t = parseJWT(token);
  if (!t) throw new TokenError("malformed token");
  if (t.header.alg !== "RS256") throw new TokenError("unexpected signing algorithm");
  const kid = typeof t.header.kid === "string" ? t.header.kid : undefined;
  const candidates = (await opts.keys(kid)).filter((k) => k.kty === "RSA" && k.n && k.e && (!kid || !k.kid || k.kid === kid));
  if (candidates.length === 0) throw new TokenError("no matching signing key");
  let verified = false;
  for (const k of candidates) {
    try {
      const key = await importRSAKey(k);
      if (await crypto.subtle.verify("RSASSA-PKCS1-v1_5", key, t.signature, t.signingInput)) {
        verified = true;
        break;
      }
    } catch {
      /* try the next key */
    }
  }
  if (!verified) throw new TokenError("bad signature");
  const p = t.payload;
  const now = opts.nowSeconds ?? Math.floor(Date.now() / 1000);
  const exp = numericClaim(p, "exp");
  if (exp === undefined || now > exp + LEEWAY_SECONDS) throw new TokenError("token expired");
  const nbf = numericClaim(p, "nbf");
  if (nbf !== undefined && nbf > now + LEEWAY_SECONDS) throw new TokenError("token not valid yet");
  const iat = numericClaim(p, "iat");
  if (iat !== undefined && iat > now + LEEWAY_SECONDS) throw new TokenError("token used before issued");
  if (opts.issuer && p.iss !== opts.issuer) throw new TokenError("wrong issuer");
  const aud = Array.isArray(p.aud) ? p.aud : [p.aud];
  if (!aud.some((a) => typeof a === "string" && opts.audiences.includes(a))) throw new TokenError("wrong audience");
  return p;
}
