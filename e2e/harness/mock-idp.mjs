// A small mock OpenID Connect provider for the auth tests: discovery, JWKS,
// an authorization endpoint (auto-approves when the browser has an IdP
// session, else shows a "choose an account" page), a token endpoint that
// issues RS256 ID tokens (with PKCE, nonce and at_hash), userinfo and an
// end-session endpoint.
//
// The IdP session is the cookie "idp_session" (= user key) on the IdP's own
// origin; tests set it with context.addCookies to get a silent sign-in, and
// /logout clears it. GET /__log returns what happened (authorize, token,
// logout events) for assertions.
import http from "node:http";
import { createHash, generateKeyPairSync, randomBytes, sign, timingSafeEqual } from "node:crypto";

import { PNG_1X1 } from "./fake-ai.mjs";

/** The people the IdP knows. Claim names are the ones Backyard maps by default. */
export const IDP_USERS = {
  alice: {
    sub: "u-alice",
    email: "Alice@Example.com",
    email_verified: true,
    name: "Alice Smith",
    title: "Staff Engineer",
    department: "Platform",
    slack: "@alice",
    groups: ["eng", "backyard-admins"],
    location: "Ottawa",
    avatar: true,
  },
  bob: {
    sub: "u-bob",
    email: "bob@example.com",
    email_verified: true,
    name: "Bob Jones",
    title: "Product Designer",
    department: "Design",
    groups: ["design"],
  },
  mallory: {
    sub: "u-mallory",
    email: "mallory@evil.test",
    email_verified: true,
    name: "Mallory",
    groups: [],
  },
};

const b64url = (buf) => Buffer.from(buf).toString("base64url");
const sha256 = (s) => createHash("sha256").update(s).digest();

function safeEqual(a, b) {
  const x = Buffer.from(String(a));
  const y = Buffer.from(String(b));
  return x.length === y.length && timingSafeEqual(x, y);
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
}

function parseCookies(header) {
  const out = {};
  for (const part of String(header ?? "").split(";")) {
    const i = part.indexOf("=");
    if (i > 0) out[part.slice(0, i).trim()] = decodeURIComponent(part.slice(i + 1).trim());
  }
  return out;
}

/**
 * Starts the IdP on 127.0.0.1 (and ::1 when available) at port; the issuer is
 * http://localhost:<port> so its cookies never mix with the 127.0.0.1 server.
 *
 * @param {{port: number, clientId: string, clientSecret: string, redirectURIs: string[], postLogoutRedirectURIs: string[]}} opts
 */
export async function startMockIdP({ port, clientId, clientSecret, redirectURIs, postLogoutRedirectURIs }) {
  const issuer = `http://localhost:${port}`;
  const kid = "e2e-" + randomBytes(4).toString("hex");
  const { publicKey, privateKey } = generateKeyPairSync("rsa", { modulusLength: 2048 });
  const jwk = { ...publicKey.export({ format: "jwk" }), kid, alg: "RS256", use: "sig" };
  const codes = new Map(); // code → {user, clientId, redirectURI, challenge, nonce, scope, exp}
  const accessTokens = new Map(); // token → user key
  const log = [];

  function signJWT(payload) {
    const head = b64url(JSON.stringify({ alg: "RS256", typ: "JWT", kid }));
    const body = b64url(JSON.stringify(payload));
    const sig = sign("RSA-SHA256", Buffer.from(`${head}.${body}`), privateKey);
    return `${head}.${body}.${b64url(sig)}`;
  }

  function claimsOf(key) {
    const u = IDP_USERS[key];
    const claims = { sub: u.sub, email: u.email, email_verified: u.email_verified, name: u.name, groups: u.groups };
    for (const k of ["title", "department", "slack", "location"]) if (u[k]) claims[k] = u[k];
    if (u.avatar) claims.picture = `${issuer}/avatars/${key}.png`;
    return claims;
  }

  function codeRedirect(res, user, params) {
    const code = b64url(randomBytes(24));
    codes.set(code, {
      user,
      clientId: params.client_id,
      redirectURI: params.redirect_uri,
      challenge: params.code_challenge,
      nonce: params.nonce,
      exp: Date.now() + 60_000,
    });
    const to = new URL(params.redirect_uri);
    to.searchParams.set("code", code);
    to.searchParams.set("state", params.state);
    res.writeHead(302, { Location: to.toString(), "Cache-Control": "no-store" });
    res.end();
  }

  function page(res, status, title, body, headers = {}) {
    res.writeHead(status, { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", ...headers });
    res.end(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${escapeHTML(title)}</title>
<link rel="icon" href="data:,"></head><body><h1>${escapeHTML(title)}</h1>${body}</body></html>`);
  }

  /** Validates an authorization request; returns an error message or "". */
  function checkAuthorize(p) {
    if (p.client_id !== clientId) return "unknown client_id";
    if (!redirectURIs.includes(p.redirect_uri)) return "redirect_uri is not registered";
    if (p.response_type !== "code") return "response_type must be code";
    if (!String(p.scope ?? "").split(" ").includes("openid")) return "scope must include openid";
    if (!p.state) return "state is required";
    if (!p.nonce) return "nonce is required";
    if (p.code_challenge_method !== "S256" || !/^[A-Za-z0-9_-]{43}$/.test(p.code_challenge ?? "")) return "PKCE S256 is required";
    return "";
  }

  function json(res, status, obj, headers = {}) {
    res.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store", ...headers });
    res.end(JSON.stringify(obj));
  }

  function handle(req, res, raw) {
    const url = new URL(req.url, issuer);
    const path = url.pathname;
    const cookies = parseCookies(req.headers.cookie);

    if (path === "/__log") return json(res, 200, log);

    if (path === "/.well-known/openid-configuration") {
      return json(res, 200, {
        issuer,
        authorization_endpoint: `${issuer}/authorize`,
        token_endpoint: `${issuer}/token`,
        userinfo_endpoint: `${issuer}/userinfo`,
        jwks_uri: `${issuer}/jwks`,
        end_session_endpoint: `${issuer}/logout`,
        response_types_supported: ["code"],
        subject_types_supported: ["public"],
        id_token_signing_alg_values_supported: ["RS256"],
        code_challenge_methods_supported: ["S256"],
        token_endpoint_auth_methods_supported: ["client_secret_basic", "client_secret_post"],
        scopes_supported: ["openid", "profile", "email", "groups"],
      });
    }

    if (path === "/jwks") return json(res, 200, { keys: [jwk] });

    if (path.startsWith("/avatars/") && path.endsWith(".png")) {
      res.writeHead(200, { "Content-Type": "image/png", "Cache-Control": "no-cache" });
      return res.end(PNG_1X1);
    }

    if (path === "/authorize") {
      const params = req.method === "POST" ? Object.fromEntries(new URLSearchParams(raw)) : Object.fromEntries(url.searchParams);
      const problem = checkAuthorize(params);
      log.push({ type: "authorize", method: req.method, params, problem, session: cookies.idp_session ?? null });
      if (problem) return page(res, 400, "Invalid authorization request", `<p id="error">${escapeHTML(problem)}</p>`);
      if (req.method === "POST") {
        const user = params.user;
        if (!IDP_USERS[user]) return page(res, 400, "Unknown user", `<p id="error">No such user.</p>`);
        res.setHeader("Set-Cookie", `idp_session=${user}; Path=/; HttpOnly; SameSite=Lax`);
        return codeRedirect(res, user, params);
      }
      if (IDP_USERS[cookies.idp_session]) return codeRedirect(res, cookies.idp_session, params); // auto-approve
      const hidden = Object.entries(params)
        .map(([k, v]) => `<input type="hidden" name="${escapeHTML(k)}" value="${escapeHTML(v)}">`)
        .join("");
      const buttons = Object.entries(IDP_USERS)
        .map(([key, u]) => `<button type="submit" name="user" value="${key}">Sign in as ${escapeHTML(u.name)}</button>`)
        .join(" ");
      return page(res, 200, "Mock IdP: choose an account", `<form method="post" action="/authorize">${hidden}${buttons}</form>`);
    }

    if (path === "/token" && req.method === "POST") {
      const form = new URLSearchParams(raw);
      let id = form.get("client_id") ?? "";
      let secret = form.get("client_secret") ?? "";
      const basic = /^Basic (.+)$/i.exec(req.headers.authorization ?? "");
      if (basic) {
        const [u, p] = Buffer.from(basic[1], "base64").toString().split(":");
        id = decodeURIComponent(u ?? "");
        secret = decodeURIComponent(p ?? "");
      }
      const fail = (error, description) => {
        log.push({ type: "token", error, description });
        json(res, 400, { error, error_description: description });
      };
      if (id !== clientId || !safeEqual(secret, clientSecret)) return fail("invalid_client", "bad client credentials");
      if (form.get("grant_type") !== "authorization_code") return fail("unsupported_grant_type", "only authorization_code");
      const code = form.get("code") ?? "";
      const grant = codes.get(code);
      codes.delete(code); // single use
      if (!grant || grant.exp < Date.now()) return fail("invalid_grant", "unknown or expired code");
      if (grant.clientId !== id || grant.redirectURI !== form.get("redirect_uri")) return fail("invalid_grant", "redirect_uri mismatch");
      if (b64url(sha256(form.get("code_verifier") ?? "")) !== grant.challenge) return fail("invalid_grant", "PKCE verification failed");
      const accessToken = b64url(randomBytes(24));
      accessTokens.set(accessToken, grant.user);
      const now = Math.floor(Date.now() / 1000);
      const idToken = signJWT({
        iss: issuer,
        aud: clientId,
        iat: now,
        exp: now + 300,
        nonce: grant.nonce,
        at_hash: b64url(sha256(accessToken).subarray(0, 16)),
        ...claimsOf(grant.user),
      });
      log.push({ type: "token", user: grant.user, pkce: true });
      return json(res, 200, { access_token: accessToken, token_type: "Bearer", expires_in: 300, id_token: idToken, scope: "openid profile email" });
    }

    if (path === "/userinfo") {
      const m = /^Bearer (.+)$/i.exec(req.headers.authorization ?? "");
      const user = m && accessTokens.get(m[1]);
      if (!user) return json(res, 401, { error: "invalid_token" }, { "WWW-Authenticate": 'Bearer error="invalid_token"' });
      return json(res, 200, claimsOf(user));
    }

    if (path === "/logout") {
      const to = url.searchParams.get("post_logout_redirect_uri") ?? "";
      log.push({ type: "logout", session: cookies.idp_session ?? null, post_logout_redirect_uri: to, client_id: url.searchParams.get("client_id") });
      const clear = { "Set-Cookie": "idp_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Lax" };
      if (postLogoutRedirectURIs.includes(to)) {
        res.writeHead(302, { Location: to, "Cache-Control": "no-store", ...clear });
        return res.end();
      }
      return page(res, 200, "Signed out", "<p>You are signed out of the mock IdP.</p>", clear);
    }

    page(res, 404, "Not found", "<p>No such page.</p>");
  }

  const handler = (req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      try {
        handle(req, res, Buffer.concat(chunks).toString());
      } catch (err) {
        res.writeHead(500, { "Content-Type": "text/plain" });
        res.end(String(err?.stack ?? err));
      }
    });
  };

  const servers = [];
  const sockets = new Set();
  async function listen(host, required) {
    const server = http.createServer(handler);
    server.on("connection", (s) => {
      sockets.add(s);
      s.on("close", () => sockets.delete(s));
    });
    try {
      await new Promise((resolve, reject) => {
        server.once("error", reject);
        server.listen(port, host, resolve);
      });
      servers.push(server);
    } catch (err) {
      if (required) throw err;
    }
  }
  await listen("127.0.0.1", true);
  await listen("::1", false); // browsers may try ::1 first for "localhost"

  return {
    issuer,
    close: async () => {
      for (const s of sockets) s.destroy();
      await Promise.all(servers.map((s) => new Promise((resolve) => s.close(() => resolve()))));
    },
  };
}
