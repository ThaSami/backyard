/**
 * Authentication: interoperability of keys and API tokens with the Go
 * implementation (vectors produced by internal/keys and internal/auth), the
 * anonymous cookie identity, Cloudflare Access JWTs and the CLI login flow.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetAccessCaches } from "../src/auth";
import { KeyLabel, b64url, deriveKey, hex, signValue } from "../src/crypto";
import { issueAPIToken } from "../src/jwt";
import { anonCookie, call, DEPLOY_TOKEN, SECRET } from "./helpers";

// Produced by the Go implementation with SECRET (see internal/keys, internal/auth/tokens.go).
const GO_API_KEY = "e58c2bef702141f096282bc961cd322f05301e296df59ea5b9065346f1a43902";
const GO_ANON_KEY = "a06f2f691de8c0b6caed952a49e36545c7b8f007c233b329a63dbfff3799e139";
const GO_ANON_SIGNED = "MDEyMzQ1Njc4OWFiY2RlZg.mkgCtfBExPrR9N7htxNoiA5vMDAzHdgktsV6snsAf0w";
const GO_TOKEN =
  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhbGljZUBhY21lLmNvbSIsImVtYWlsIjoiYWxpY2VAYWNtZS5jb20iLCJuYW1lIjoiQWxpY2UgU21pdGgiLCJncm91cHMiOlsiZW5nIiwiYmFja3lhcmQtYWRtaW5zIl0sInNjb3BlIjoiYXBpIiwiaXNzIjoiYmFja3lhcmQiLCJqdGkiOiI0NzMwNzhmZDJlMjEwMGM3OTkxYTliOTI5MWEwMGRmOSIsImlhdCI6MTc5MTg0MjkzMCwiZXhwIjo0MzE0NzIyOTMwfQ.4He0vGvXox6fZtuJwHnSAUrY6x6sNOxvMq1X10fxRug";

const bearer = (t: string) => ({ Authorization: `Bearer ${t}` });

function fromHex(s: string): Uint8Array {
  return new Uint8Array(s.match(/../g)!.map((b) => parseInt(b, 16)));
}

/** Independent HS256 (HMAC-SHA256 with a raw key) for cross-checking. */
async function hs256(keyHex: string, input: string): Promise<string> {
  const key = await crypto.subtle.importKey("raw", fromHex(keyHex), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  return b64url(new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(input))));
}

function decodePart(part: string): Record<string, unknown> {
  const b = atob(part.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((part.length + 3) % 4));
  return JSON.parse(b) as Record<string, unknown>;
}

describe("keys and API tokens (Go interop)", () => {
  it("derives the same HKDF keys as internal/keys", async () => {
    expect(hex((await deriveKey(SECRET, KeyLabel.APIToken)).raw)).toBe(GO_API_KEY);
    expect(hex((await deriveKey(SECRET, KeyLabel.Anon)).raw)).toBe(GO_ANON_KEY);
    expect(await signValue((await deriveKey(SECRET, KeyLabel.Anon)).hmac, "0123456789abcdef")).toBe(GO_ANON_SIGNED);
  });

  it("accepts an API token minted by the Go server", async () => {
    let res = await call("/__backyard/api/me", { headers: bearer(GO_TOKEN) });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({
      id: "alice@acme.com",
      email: "alice@acme.com",
      name: "Alice Smith",
      groups: ["eng", "backyard-admins"],
      anonymous: false,
      admin: false,
    });
    // Admin rights come from the configuration (ADMIN_GROUPS), not the token.
    res = await call("/__backyard/api/me", { headers: bearer(GO_TOKEN) }, { ADMIN_GROUPS: "backyard-admins" });
    expect(((await res.json()) as { admin: boolean }).admin).toBe(true);
    res = await call("/__backyard/api/deploy/me", { headers: bearer(GO_TOKEN) });
    expect(((await res.json()) as { id: string }).id).toBe("alice@acme.com");
  });

  it("mints HS256 tokens with the Go key and claim layout", async () => {
    const res = await call("/__backyard/api/tokens", {
      method: "POST",
      headers: { ...bearer(GO_TOKEN), "X-Backyard": "1", "Content-Type": "application/json" },
      body: JSON.stringify({ name: "laptop", ttl_hours: 2 }),
    });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { token: string; expires_at: string; name: string };
    expect(body.name).toBe("laptop");
    const [h, p, s] = body.token.split(".") as [string, string, string];
    expect(decodePart(h)).toEqual({ alg: "HS256", typ: "JWT" });
    expect(s).toBe(await hs256(GO_API_KEY, `${h}.${p}`));
    const claims = decodePart(p);
    expect(Object.keys(claims)).toEqual(["sub", "email", "name", "groups", "scope", "iss", "jti", "iat", "exp"]);
    expect(claims).toMatchObject({ sub: "alice@acme.com", email: "alice@acme.com", name: "Alice Smith", scope: "api", iss: "backyard" });
    expect((claims.exp as number) - (claims.iat as number)).toBe(7200);
    expect(body.expires_at).toBe(new Date((claims.exp as number) * 1000).toISOString().replace(".000Z", "Z"));
    const me = await call("/__backyard/api/me", { headers: bearer(body.token) });
    expect(((await me.json()) as { id: string }).id).toBe("alice@acme.com");
  });

  it("rejects expired, tampered and foreign tokens", async () => {
    const user = { id: "bob@acme.com", email: "bob@acme.com", name: "Bob", anonymous: false, admin: false };
    const expired = await issueAPIToken(SECRET, user, 60, Math.floor(Date.now() / 1000) - 3600);
    expect((await call("/__backyard/api/me", { headers: bearer(expired.token) })).status).toBe(401);
    const good = await issueAPIToken(SECRET, user, 3600);
    const [h, p, s] = good.token.split(".") as [string, string, string];
    const tampered = `${h}.${b64url(new TextEncoder().encode(JSON.stringify({ ...decodePart(p), sub: "eve@acme.com" })))}.${s}`;
    expect((await call("/__backyard/api/me", { headers: bearer(tampered) })).status).toBe(401);
    const none = `${b64url(new TextEncoder().encode('{"alg":"none","typ":"JWT"}'))}.${p}.`;
    expect((await call("/__backyard/api/me", { headers: bearer(none) })).status).toBe(401);
    const other = await issueAPIToken("another-secret-0123456789-abcdefghijklmnop", user, 3600);
    const res = await call("/__backyard/api/me", { headers: bearer(other.token) });
    expect(res.status).toBe(401);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("unauthenticated");
    expect((await call("/__backyard/api/me", { headers: bearer("garbage") })).status).toBe(401);
  });

  it("restricts who may mint tokens", async () => {
    const headers = { "X-Backyard": "1", "Content-Type": "application/json" };
    let res = await call("/__backyard/api/tokens", { method: "POST", headers, body: "{}" });
    expect(res.status).toBe(403); // anonymous guest
    res = await call("/__backyard/api/tokens", { method: "POST", headers: { ...headers, ...bearer(DEPLOY_TOKEN) }, body: "{}" });
    expect(res.status).toBe(403); // static token
    res = await call("/__backyard/api/tokens", { method: "POST", headers: { ...headers, ...bearer(GO_TOKEN) }, body: JSON.stringify({ ttl_hours: 100000 }) });
    expect(res.status).toBe(400);
    res = await call("/__backyard/api/tokens", { method: "POST", headers: bearer(GO_TOKEN), body: "{}" });
    expect(res.status).toBe(403); // CSRF header missing
  });
});

describe("anonymous identity (AUTH_MODE=none)", () => {
  it("issues a signed guest cookie and keeps the identity", async () => {
    const first = await call("/__backyard/api/me");
    const me = (await first.json()) as { id: string; name: string; anonymous: boolean };
    expect(me.id).toMatch(/^anon:[0-9a-f]{16}$/);
    expect(me.name).toBe("Guest " + me.id.slice(5, 9));
    expect(me.anonymous).toBe(true);
    const sc = first.headers.get("Set-Cookie")!;
    expect(sc).toContain("HttpOnly");
    expect(sc).toContain("SameSite=Lax");
    expect(sc).toContain("Secure");
    expect(sc).toContain("Max-Age=31536000");
    const cookie = anonCookie(first);
    const again = await call("/__backyard/api/me", { headers: { Cookie: cookie } });
    expect(((await again.json()) as { id: string }).id).toBe(me.id);
    expect(again.headers.get("Set-Cookie")).toBeNull();
    // The Go cookie format is accepted as is.
    const go = await call("/__backyard/api/me", { headers: { Cookie: `backyard_anon=${GO_ANON_SIGNED}` } });
    expect(await go.json()).toMatchObject({ id: "anon:0123456789abcdef", name: "Guest 0123" });
    // Forged cookies are replaced.
    const forged = await call("/__backyard/api/me", { headers: { Cookie: "backyard_anon=MDEyMzQ1Njc4OWFiY2RlZg.AAAA" } });
    expect(((await forged.json()) as { id: string }).id).not.toBe("anon:0123456789abcdef");
  });

  it("runs the CLI login flow", async () => {
    let res = await call("/__backyard/auth/cli?redirect_uri=" + encodeURIComponent("http://127.0.0.1:43123/callback") + "&state=st4te");
    expect(res.status).toBe(302);
    const loc = new URL(res.headers.get("Location")!);
    expect(loc.origin + loc.pathname).toBe("http://127.0.0.1:43123/callback");
    expect(loc.searchParams.get("state")).toBe("st4te");
    expect(loc.searchParams.get("expires_at")).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$/);
    const token = loc.searchParams.get("token")!;
    const me = await call("/__backyard/api/me", { headers: bearer(token) });
    expect(((await me.json()) as { anonymous: boolean }).anonymous).toBe(true);
    expect(res.headers.get("Referrer-Policy")).toBe("no-referrer");

    for (const bad of ["https://127.0.0.1:1/x", "http://evil.example:80/x", "http://127.0.0.1/x", "http://127.0.0.1:99999/x", "http://u@localhost:5/x"]) {
      res = await call("/__backyard/auth/cli?state=s&redirect_uri=" + encodeURIComponent(bad));
      expect(res.status).toBe(400);
    }
    res = await call("/__backyard/auth/cli?redirect_uri=" + encodeURIComponent("http://localhost:5000/cb"));
    expect(res.status).toBe(400); // state missing
    res = await call("/__backyard/auth/cli?mode=display");
    expect(res.status).toBe(200);
    const html = await res.text();
    expect(html).toContain("backyard login --server https://backyard.test --token ey");
    expect(res.headers.get("Content-Security-Policy")).toContain("frame-ancestors 'none'");
    expect((await call("/__backyard/auth/cli?mode=display", { method: "POST" })).status).toBe(405);
  });
});

describe("Cloudflare Access (AUTH_MODE=access)", () => {
  const TEAM = "acme.cloudflareaccess.com";
  const AUD = "aud-1234567890";
  const accessEnv = { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: AUD, ADMIN_GROUPS: "platform" };

  afterEach(() => {
    vi.unstubAllGlobals();
    resetAccessCaches();
  });

  async function setup() {
    const pair = (await crypto.subtle.generateKey(
      { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
      true,
      ["sign", "verify"],
    )) as CryptoKeyPair;
    const jwk = (await crypto.subtle.exportKey("jwk", pair.publicKey)) as JsonWebKey;
    const realFetch = globalThis.fetch;
    const seen: string[] = [];
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      seen.push(url);
      if (url === `https://${TEAM}/cdn-cgi/access/certs`) {
        return Response.json({ keys: [{ ...jwk, kid: "k1", alg: "RS256", use: "sig" }] });
      }
      if (url === `https://${TEAM}/cdn-cgi/access/get-identity`) {
        return Response.json({ name: "Alice Smith", email: "alice@acme.com", groups: [{ id: "1", name: "eng" }, { id: "2", name: "platform" }] });
      }
      return realFetch(input, init);
    });
    const sign = async (claims: Record<string, unknown>, kid = "k1") => {
      const h = b64url(new TextEncoder().encode(JSON.stringify({ alg: "RS256", kid, typ: "JWT" })));
      const p = b64url(new TextEncoder().encode(JSON.stringify(claims)));
      const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", pair.privateKey, new TextEncoder().encode(`${h}.${p}`)));
      return `${h}.${p}.${b64url(sig)}`;
    };
    const now = Math.floor(Date.now() / 1000);
    const claims = { aud: [AUD], email: "Alice@Acme.com", sub: "u-1", iss: `https://${TEAM}`, iat: now, exp: now + 3600, identity_nonce: "n1", country: "CA" };
    return { sign, claims, seen };
  }

  it("verifies Cf-Access-Jwt-Assertion and enriches the identity", async () => {
    const { sign, claims, seen } = await setup();
    const jwt = await sign(claims);
    let res = await call("/__backyard/api/me", { headers: { "Cf-Access-Jwt-Assertion": jwt } }, accessEnv);
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({
      id: "alice@acme.com",
      email: "alice@acme.com",
      name: "Alice Smith",
      groups: ["eng", "platform"],
      anonymous: false,
      admin: true,
      extra: { country: "CA" },
    });
    // The CF_Authorization cookie works too (requests to bypassed paths).
    res = await call("/__backyard/api/deploy/me", { headers: { Cookie: `CF_Authorization=${jwt}` } }, accessEnv);
    expect(((await res.json()) as { id: string }).id).toBe("alice@acme.com");
    // Keys and identities are cached.
    expect(seen.filter((u) => u.endsWith("/certs"))).toHaveLength(1);
    expect(seen.filter((u) => u.endsWith("/get-identity"))).toHaveLength(1);
  });

  it("rejects missing, expired and foreign-audience tokens", async () => {
    const { sign, claims } = await setup();
    let res = await call("/s/demo/__backyard/api/me", {}, accessEnv);
    expect(res.status).toBe(401);
    const err = ((await res.json()) as { error: { code: string; login_url?: string } }).error;
    expect(err.code).toBe("unauthenticated");
    expect(err.login_url).toBe("https://backyard.test/s/demo/");
    res = await call("/__backyard/api/me", { headers: { "Cf-Access-Jwt-Assertion": await sign({ ...claims, aud: ["other"] }) } }, accessEnv);
    expect(res.status).toBe(401);
    res = await call("/__backyard/api/me", { headers: { "Cf-Access-Jwt-Assertion": await sign({ ...claims, exp: claims.iat - 120 }) } }, accessEnv);
    expect(res.status).toBe(401);
    res = await call("/__backyard/api/me", { headers: { "Cf-Access-Jwt-Assertion": await sign({ ...claims, iss: "https://evil.cloudflareaccess.com" }) } }, accessEnv);
    expect(res.status).toBe(401);
    res = await call("/__backyard/api/me", { headers: { "Cf-Access-Jwt-Assertion": await sign(claims, "unknown-kid") } }, accessEnv);
    expect(res.status).toBe(401);
    // Pages get an explanation instead of a login loop.
    res = await call("/", { headers: { Accept: "text/html" } }, accessEnv);
    expect(res.status).toBe(401);
    expect(await res.text()).toContain("Cloudflare Access");
    // Anonymous API tokens are not valid in access mode.
    const anon = await issueAPIToken(SECRET, { id: "anon:0123456789abcdef", name: "Guest", anonymous: true, admin: false }, 3600);
    expect((await call("/__backyard/api/me", { headers: bearer(anon.token) }, accessEnv)).status).toBe(401);
  });

  it("advertises the CLI login and mints tokens for Access users", async () => {
    const { sign, claims } = await setup();
    const info = (await (await call("/__backyard/api/deploy/info", {}, accessEnv)).json()) as { auth: Record<string, unknown> };
    expect(info.auth).toEqual({ mode: "access", cli_login_url: "https://backyard.test/__backyard/auth/cli", anonymous_deploy: false });
    const jwt = await sign(claims);
    let res = await call(
      "/__backyard/auth/cli?state=s&redirect_uri=" + encodeURIComponent("http://[::1]:5001/cb"),
      { headers: { "Cf-Access-Jwt-Assertion": jwt } },
      accessEnv,
    );
    expect(res.status).toBe(302);
    const token = new URL(res.headers.get("Location")!).searchParams.get("token")!;
    res = await call("/__backyard/api/deploy/me", { headers: bearer(token) }, accessEnv);
    expect(await res.json()).toMatchObject({ id: "alice@acme.com", admin: true });
    res = await call("/__backyard/auth/cli?mode=display", {}, accessEnv);
    expect(res.status).toBe(401);
    res = await call("/__backyard/auth/logout", { headers: { "Cf-Access-Jwt-Assertion": jwt } }, accessEnv);
    expect(res.headers.get("Location")).toBe("/cdn-cgi/access/logout");
  });
});
