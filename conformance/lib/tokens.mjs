// Backyard API tokens (SPEC §8.3) as the suite needs them: the documented key
// derivation and HS256 layout, so that a runner that knows the server's
// secret (BACKYARD_CONFORMANCE_SECRET) can hand the suite a token of a real,
// non-anonymous user — something no auth mode the runners use can mint.
import { createHmac, hkdfSync, randomBytes } from "node:crypto";

/** keys.Derive: HKDF-SHA256(ikm = secret, salt = "backyard", info = label, 32 bytes). */
export function deriveKey(secret, label) {
  return Buffer.from(hkdfSync("sha256", Buffer.from(secret, "utf8"), Buffer.from("backyard"), Buffer.from(label), 32));
}

const b64url = (v) => Buffer.from(typeof v === "string" ? v : JSON.stringify(v)).toString("base64url");

/**
 * An API token for user {id, email?, name?} that expires ttlSeconds from now
 * (exp may also be given in seconds since the epoch).
 */
export function signAPIToken(secret, user, { ttlSeconds = 3600, exp, now = Math.floor(Date.now() / 1000) } = {}) {
  const claims = { sub: user.id };
  if (user.email) claims.email = user.email;
  if (user.name) claims.name = user.name;
  Object.assign(claims, { scope: "api", iss: "backyard", jti: randomBytes(16).toString("hex"), iat: now, exp: exp ?? now + ttlSeconds });
  const input = `${b64url({ alg: "HS256", typ: "JWT" })}.${b64url(claims)}`;
  const sig = createHmac("sha256", deriveKey(secret, "backyard/api-token/v1")).update(input).digest("base64url");
  return `${input}.${sig}`;
}

/** The payload of a JWT (no verification), or null when it is not one. */
export function jwtClaims(token) {
  const parts = String(token || "").split(".");
  if (parts.length !== 3) return null;
  try {
    const c = JSON.parse(Buffer.from(parts[1], "base64url").toString("utf8"));
    return c && typeof c === "object" ? c : null;
  } catch {
    return null;
  }
}
