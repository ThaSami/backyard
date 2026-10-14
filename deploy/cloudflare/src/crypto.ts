/**
 * Key derivation and signing, byte-compatible with the Go implementation in
 * internal/keys: purpose keys are HKDF-SHA256(secret, salt "backyard", info
 * <label>, 32 bytes) and signed values are base64url(payload) + "." +
 * base64url(HMAC-SHA256(key, payload)).
 */

/** Labels for the derived keys (internal/keys). */
export const KeyLabel = {
  Session: "backyard/session/v1",
  OAuthState: "backyard/oauth-state/v1",
  APIToken: "backyard/api-token/v1",
  Deploy: "backyard/deploy/v1",
  Upload: "backyard/upload/v1",
  Anon: "backyard/anon/v1",
  /** Signs the confirmation form of a cross-site CLI login (like Go's keys.CLIConfirm). */
  CLIConfirm: "backyard/cli-confirm/v1",
} as const;

const enc = new TextEncoder();
const keyCache = new Map<string, Promise<{ raw: Uint8Array; hmac: CryptoKey }>>();

/** Returns the 32-byte key for label derived from secret (cached). */
export function deriveKey(secret: string, label: string): Promise<{ raw: Uint8Array; hmac: CryptoKey }> {
  const id = label + "\u0000" + secret;
  let p = keyCache.get(id);
  if (!p) {
    p = (async () => {
      const ikm = await crypto.subtle.importKey("raw", enc.encode(secret), "HKDF", false, ["deriveBits"]);
      const bits = await crypto.subtle.deriveBits(
        { name: "HKDF", hash: "SHA-256", salt: enc.encode("backyard"), info: enc.encode(label) },
        ikm,
        256,
      );
      const raw = new Uint8Array(bits);
      const hmac = await crypto.subtle.importKey("raw", raw, { name: "HMAC", hash: "SHA-256" }, false, ["sign", "verify"]);
      return { raw, hmac };
    })();
    if (keyCache.size > 64) keyCache.clear();
    keyCache.set(id, p);
  }
  return p;
}

/** Signs payload: base64url(payload) + "." + base64url(HMAC). */
export async function signValue(key: CryptoKey, payload: Uint8Array | string): Promise<string> {
  const bytes = typeof payload === "string" ? enc.encode(payload) : payload;
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, bytes));
  return b64url(bytes) + "." + b64url(mac);
}

/** Verifies a value produced by signValue and returns its payload, or null. */
export async function verifyValue(key: CryptoKey, signed: string): Promise<Uint8Array | null> {
  const dot = signed.indexOf(".");
  if (dot < 0) return null;
  const payload = b64urlDecode(signed.slice(0, dot));
  const mac = b64urlDecode(signed.slice(dot + 1));
  if (!payload || !mac) return null;
  const ok = await crypto.subtle.verify("HMAC", key, mac, payload);
  return ok ? payload : null;
}

/** Signs a JSON value with the key for label. */
export async function signClaims(secret: string, label: string, claims: unknown): Promise<string> {
  const { hmac } = await deriveKey(secret, label);
  return signValue(hmac, JSON.stringify(claims));
}

/** Verifies a signed JSON value; returns the decoded object or null. */
export async function verifyClaims<T>(secret: string, label: string, signed: string, maxLen = 8192): Promise<T | null> {
  if (!signed || signed.length > maxLen || /[\s/]/.test(signed)) return null;
  const { hmac } = await deriveKey(secret, label);
  const payload = await verifyValue(hmac, signed);
  if (!payload) return null;
  try {
    const v = JSON.parse(new TextDecoder().decode(payload)) as unknown;
    return v && typeof v === "object" ? (v as T) : null;
  } catch {
    return null;
  }
}

// --- encodings -------------------------------------------------------------

/** base64url without padding. */
export function b64url(bytes: Uint8Array): string {
  let s = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    s += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** Decodes unpadded (or padded) base64url; null when malformed. */
export function b64urlDecode(s: string): Uint8Array | null {
  if (!/^[A-Za-z0-9_-]*={0,2}$/.test(s)) return null;
  const t = s.replace(/=+$/, "").replace(/-/g, "+").replace(/_/g, "/");
  if (t.length % 4 === 1) return null;
  try {
    return base64Decode(t + "===".slice((t.length + 3) % 4));
  } catch {
    return null;
  }
}

/** Uint8Array.prototype.toBase64 (TC39 base64 proposal) where the runtime has it. */
type WithToBase64 = Uint8Array & { toBase64?: () => string };

/**
 * Standard base64 encoding of bytes. Uses the runtime's native encoder when
 * there is one; otherwise builds the binary string a chunk at a time with
 * String.fromCharCode.apply (spreading a typed array into the call goes
 * through the iterator protocol and is several times slower: CPU time is
 * scarce in a Worker).
 */
export function base64Encode(bytes: Uint8Array): string {
  const native = (bytes as WithToBase64).toBase64;
  if (typeof native === "function") return native.call(bytes);
  const parts: string[] = [];
  for (let i = 0; i < bytes.length; i += 0x8000) {
    parts.push(String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000) as unknown as number[]));
  }
  return btoa(parts.join(""));
}

const STD_BASE64_RE = /^[A-Za-z0-9+/]*={0,2}$/;
const BASE64_BODY_RE = /^[A-Za-z0-9+/]*$/;

/**
 * Standard, padded base64 of a base64 text in the standard or URL-safe
 * alphabet, padded or not, possibly with white space; null when it is not
 * base64. Text that already is standard padded base64 (what clients send) is
 * returned as is, without a copy or decoding it: it can be passed on.
 */
export function normalizeBase64(s: string): string | null {
  if (s.length % 4 === 0 && STD_BASE64_RE.test(s)) return s;
  const t = s.replace(/\s+/g, "").replace(/-/g, "+").replace(/_/g, "/").replace(/=+$/, "");
  if (!BASE64_BODY_RE.test(t) || t.length % 4 === 1) return null;
  return t + "===".slice((t.length + 3) % 4);
}

/** Decoded size in bytes of standard padded base64 (see normalizeBase64). */
export function base64Size(b64: string): number {
  const pad = b64.endsWith("==") ? 2 : b64.endsWith("=") ? 1 : 0;
  return (b64.length / 4) * 3 - pad;
}

/** The first n (at most) decoded bytes of standard padded base64, without decoding the rest. */
export function base64Head(b64: string, n: number): Uint8Array {
  const chars = Math.min(b64.length, Math.ceil(n / 3) * 4);
  try {
    return base64Decode(b64.slice(0, chars)).subarray(0, n);
  } catch {
    return new Uint8Array(0);
  }
}

/** Decodes standard base64 (throws on invalid input). */
export function base64Decode(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** Decodes base64 in the standard or URL-safe alphabet, padded or not; null when invalid. */
export function decodeAnyBase64(s: string): Uint8Array | null {
  const t = s.replace(/[\s]/g, "").replace(/-/g, "+").replace(/_/g, "/").replace(/=+$/, "");
  if (!/^[A-Za-z0-9+/]*$/.test(t) || t.length % 4 === 1) return null;
  try {
    return base64Decode(t + "===".slice((t.length + 3) % 4));
  } catch {
    return null;
  }
}

/** Lowercase hex of bytes. */
export function hex(bytes: Uint8Array): string {
  let s = "";
  for (const b of bytes) s += b.toString(16).padStart(2, "0");
  return s;
}

/** Random lowercase hex string of n bytes. */
export function randomHex(n: number): string {
  return hex(crypto.getRandomValues(new Uint8Array(n)));
}

/** SHA-256 hex digest. */
export async function sha256Hex(data: Uint8Array | string): Promise<string> {
  const bytes = typeof data === "string" ? enc.encode(data) : data;
  return hex(new Uint8Array(await crypto.subtle.digest("SHA-256", bytes)));
}

/** Constant-time string comparison (via HMAC of both values). */
export async function timingSafeEqual(a: string, b: string): Promise<boolean> {
  const key = await crypto.subtle.importKey("raw", crypto.getRandomValues(new Uint8Array(32)), { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
  ]);
  const [ma, mb] = await Promise.all([crypto.subtle.sign("HMAC", key, enc.encode(a)), crypto.subtle.sign("HMAC", key, enc.encode(b))]);
  const x = new Uint8Array(ma);
  const y = new Uint8Array(mb);
  let diff = 0;
  for (let i = 0; i < x.length; i++) diff |= x[i]! ^ y[i]!;
  return diff === 0;
}
