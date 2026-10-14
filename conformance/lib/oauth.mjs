// The Claude connector's OAuth 2.1 flows (SPEC §8.4) as the suite needs them,
// without a real browser or any dependency:
//
//   Browser            a cookie jar that navigates like a browser: manual
//                      redirects across origins, Fetch Metadata headers, and
//                      the consent form parsed out of the page
//   pkce()             a code_verifier and its S256 code_challenge
//   register(), openConsent(), submitConsent(), authorizeAndConsent()
//   exchangeCode(), refresh(), revoke(), tokenRequest()
//   parseChallenge()   a strict WWW-Authenticate parser
//
// A flow stops at the redirect to the client's loopback redirect URI and reads
// code, state and iss from its Location: nothing needs to listen there.
import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";
import http from "node:http";
import https from "node:https";
import { CookieJar } from "./http.mjs";

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** The path of the MCP endpoint below the hub URL (P). */
export const MCP_PATH = "/__backyard/mcp";
/** The path of the authorization endpoint. */
export const AUTHORIZE_PATH = "/__backyard/auth/authorize";

/** P, R and M of SPEC §8.4.1 for the connector URL R (info.mcp_url). */
export function connectorURLs(mcpURL) {
  const R = String(mcpURL);
  assert.ok(R.endsWith(MCP_PATH), `mcp_url ${R} must end in ${MCP_PATH}`);
  const P = R.slice(0, -MCP_PATH.length);
  return { P, R, M: `${P}/.well-known/oauth-protected-resource${MCP_PATH}`, H: new URL(P).hostname };
}

/** A buffered response (the shape of lib/http.mjs's Reply). */
export class Response {
  constructor(url, method, res, body) {
    this.url = url;
    this.method = method;
    this.status = res.status;
    this.headers = res.headers;
    this.body = body;
  }

  get text() {
    return this.body.toString("utf8");
  }

  get json() {
    try {
      return JSON.parse(this.text);
    } catch {
      assert.fail(`${this.method} ${this.url}: expected a JSON body, got HTTP ${this.status} ${this.headers.get("content-type")}: ${this.text.slice(0, 300)}`);
    }
  }

  header(name) {
    return this.headers.get(name);
  }

  toString() {
    return `${this.method} ${this.url} → HTTP ${this.status} ${this.text.slice(0, 400)}`;
  }
}

/**
 * One HTTP request with node:http, which sends exactly the given headers:
 * global fetch always sends its own Sec-Fetch-Mode (cors) and Accept, so it
 * cannot act like a browser navigation, nor send no Accept at all.
 */
function request1(url, { method, headers, body, timeout }) {
  const u = new URL(url);
  const mod = u.protocol === "https:" ? https : http;
  const payload = body === undefined || body === null ? null : Buffer.isBuffer(body) ? body : Buffer.from(String(body));
  const h = { ...headers };
  if (payload && !Object.keys(h).some((k) => k.toLowerCase() === "content-length")) h["Content-Length"] = String(payload.length);
  return new Promise((resolve, reject) => {
    const req = mod.request(
      { protocol: u.protocol, hostname: u.hostname.replace(/^\[|\]$/g, ""), port: u.port || (u.protocol === "https:" ? 443 : 80), method, path: u.pathname + u.search, headers: h, timeout },
      (res) => {
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => {
          const hs = new Headers();
          for (let i = 0; i < res.rawHeaders.length; i += 2) hs.append(res.rawHeaders[i], res.rawHeaders[i + 1]);
          resolve({ status: res.statusCode, headers: hs, body: Buffer.concat(chunks) });
        });
        res.on("error", reject);
      },
    );
    req.on("timeout", () => req.destroy(new Error(`${method} ${url}: timeout`)));
    req.on("error", reject);
    req.end(payload ?? undefined);
  });
}

/**
 * A request with manual redirects, a buffered body and retries of 429 answers
 * (the Go server rate-limits OAuth requests that identify no one per client
 * IP, and the whole suite shares one address). Sends no cookies unless headers carry them, and
 * no header that was not given.
 */
export async function send(url, { method = "GET", headers = {}, body, retry = true, timeout = 60_000 } = {}) {
  for (let attempt = 1; ; attempt++) {
    const res = await request1(url, { method, headers, body, timeout });
    if (res.status === 429 && retry && attempt < 8) {
      const after = Number(res.headers.get("Retry-After"));
      await sleep(Math.min(Number.isFinite(after) && after > 0 ? after * 1000 : 1000, 15_000));
      continue;
    }
    return new Response(url, method, res, res.body);
  }
}

/** Decodes the character references an HTML attribute value may hold. */
export function decodeHTML(s) {
  const named = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" };
  return String(s).replace(/&(#x[0-9a-f]+|#[0-9]+|amp|lt|gt|quot|apos);/gi, (_, e) => {
    if (e[0] === "#") return String.fromCodePoint(e[1] === "x" || e[1] === "X" ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10));
    return named[e.toLowerCase()];
  });
}

/**
 * The hidden inputs of the consent page (SPEC §8.4.5 renders each exactly as
 * <input type="hidden" name="…" value="…">), HTML-unescaped. A repeated
 * name fails the test.
 */
export function hiddenInputs(html) {
  const out = {};
  for (const m of String(html).matchAll(/<input type="hidden" name="([^"]+)" value="([^"]*)">/g)) {
    const name = decodeHTML(m[1]);
    assert.ok(!(name in out), `the consent form has the hidden input ${name} twice`);
    out[name] = decodeHTML(m[2]);
  }
  return out;
}

/** A PKCE pair: a 43-character verifier and its S256 challenge. */
export function pkce() {
  const verifier = randomBytes(32).toString("base64url");
  return { verifier, challenge: s256(verifier) };
}

/** BASE64URL(SHA-256(ascii(verifier))). */
export function s256(verifier) {
  return createHash("sha256").update(verifier, "ascii").digest("base64url");
}

/** A random OAuth state with characters that need escaping in a query and in HTML. */
export function newState() {
  return `cf+&=/<>"' ${randomBytes(8).toString("hex")}`;
}

/** Fetch Metadata of a top-level navigation that another site started. */
export const CROSS_SITE_NAVIGATION = { "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-User": "?1" };

/**
 * One browser profile: a cookie jar shared by every origin it visits (RFC 6265
 * host matching, as browsers do), manual redirects, Fetch Metadata. It never
 * sends a bearer token.
 */
export class Browser {
  constructor({ jar } = {}) {
    this.jar = jar ?? new CookieJar();
  }

  /** Stores a cookie for url's host, as context.addCookies would. */
  setCookie(url, name, value) {
    this.jar.store(url, [`${name}=${value}; Path=/`]);
  }

  /** One request with this browser's cookies (no redirects followed). */
  async request(url, { method = "GET", headers = {}, body, retry = true } = {}) {
    const h = { ...headers };
    const cookie = this.jar.header(url);
    if (cookie && !Object.keys(h).some((k) => k.toLowerCase() === "cookie")) h.Cookie = cookie;
    const r = await send(url, { method, headers: h, body, retry });
    this.jar.store(url, r.headers.getSetCookie());
    return r;
  }

  /**
   * Navigates to url and follows redirects (GET after 301/302/303) like a
   * browser, until a response that is not a redirect, or a redirect whose
   * target stop(url) accepts (the client's redirect URI). site is the
   * Sec-Fetch-Site of the first request; a cross-site hop makes the rest
   * cross-site too. Resolves to {response, hops, stoppedAt} where hops lists
   * every response and stoppedAt is the URL that was not followed (or null).
   */
  async navigate(url, { method = "GET", headers = {}, body, site = "none", stop = () => false, maxHops = 12 } = {}) {
    const hops = [];
    let current = new URL(url);
    let fetchSite = site;
    let opts = { method, headers, body };
    for (let i = 0; i < maxHops; i++) {
      const fm = { "Sec-Fetch-Site": fetchSite, "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document" };
      const r = await this.request(current.href, { ...opts, headers: { ...fm, ...opts.headers } });
      hops.push(r);
      const loc = r.header("location");
      if (![301, 302, 303, 307, 308].includes(r.status) || !loc) return { response: r, hops, stoppedAt: null };
      const next = new URL(loc, current);
      if (stop(next)) return { response: r, hops, stoppedAt: next };
      if (fetchSite !== "cross-site" && next.origin !== current.origin) fetchSite = "cross-site";
      const keepBody = r.status === 307 || r.status === 308;
      opts = keepBody ? opts : { method: "GET", headers: {}, body: undefined };
      current = next;
    }
    assert.fail(`too many redirects from ${url}: ${hops.map((h) => `${h.status} ${h.header("location")}`).join(" → ")}`);
  }
}

/** POST <P>/__backyard/oauth/register with a JSON body (or raw: a string body). */
export function register(P, metadata, { headers = {}, raw } = {}) {
  return send(`${P}/__backyard/oauth/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", ...headers },
    body: raw ?? JSON.stringify(metadata),
  });
}

/** Registers a client and returns its registration (asserting 201). */
export async function registerClient(P, { redirectURIs = ["http://127.0.0.1:9/callback"], name = "Conformance", extra = {} } = {}) {
  const r = await register(P, { client_name: name, redirect_uris: redirectURIs, ...extra });
  assert.equal(r.status, 201, `register: ${r}`);
  return r.json;
}

/**
 * The authorization request URL for params (in this order; undefined values
 * are left out, arrays repeat the parameter).
 */
export function authorizeURL(P, params) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === null) continue;
    for (const one of Array.isArray(v) ? v : [v]) q.append(k, String(one));
  }
  return `${P}${AUTHORIZE_PATH}?${q}`;
}

/** The standard parameters of an authorization request. */
export function authorizeParams({ client, redirectURI, challenge, state, resource, scope = "mcp offline_access", extra = {} }) {
  return {
    response_type: "code",
    client_id: client.client_id ?? client,
    redirect_uri: redirectURI ?? client.redirect_uris?.[0],
    code_challenge: challenge,
    code_challenge_method: "S256",
    state,
    resource,
    scope,
    ...extra,
  };
}

/** Whether u is on the origin of the redirect URI (where a flow stops). */
const atRedirect = (redirectURI) => (u) => u.origin === new URL(redirectURI).origin && u.pathname === new URL(redirectURI).pathname;

/**
 * Opens the authorization URL the way a client does (a cross-site
 * navigation) and follows any sign-in until the consent page or a redirect
 * to the client. Resolves to {page, form, stoppedAt, hops}: page is the final
 * response (200 consent page, or an error page), form its hidden inputs.
 */
export async function openConsent(browser, P, params, { site = "cross-site" } = {}) {
  const redirectURI = params.redirect_uri;
  const { response, hops, stoppedAt } = await browser.navigate(authorizeURL(P, params), {
    site,
    stop: redirectURI ? atRedirect(redirectURI) : () => false,
  });
  return { page: response, form: response.status === 200 ? hiddenInputs(response.text) : null, stoppedAt, hops };
}

/** Fetch Metadata and Origin of the consent page's own form submission. */
export function consentHeaders(P) {
  return { "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Fetch-User": "?1", Origin: new URL(P).origin };
}

/**
 * POSTs the consent form fields (plus decision) as the page would; headers
 * replace the defaults (a null value removes one). The redirect is not
 * followed.
 */
export function submitConsent(browser, P, fields, { decision = "allow", headers } = {}) {
  const h = { ...consentHeaders(P), "Content-Type": "application/x-www-form-urlencoded", ...(headers ?? {}) };
  for (const [k, v] of Object.entries(h)) if (v === null || v === undefined) delete h[k];
  const form = new URLSearchParams();
  for (const [k, v] of Object.entries(fields)) if (v !== undefined && v !== null) form.append(k, v);
  if (decision !== null) form.append("decision", decision);
  return browser.request(`${P}${AUTHORIZE_PATH}`, { method: "POST", headers: h, body: form.toString() });
}

/**
 * The parameters an authorization response carries in its Location (after
 * asserting that it goes to redirectURI: the same URI with the query added).
 */
export function redirectParams(location, redirectURI, what = "redirect") {
  const loc = new URL(location);
  const want = new URL(redirectURI);
  assert.equal(`${loc.origin}${loc.pathname}`, `${want.origin}${want.pathname}`, `${what}: goes to the redirect URI (${location})`);
  for (const [k, v] of want.searchParams) assert.equal(loc.searchParams.get(k), v, `${what}: keeps the redirect URI's query ${k}`);
  return Object.fromEntries(loc.searchParams);
}

/**
 * A complete authorization: consent page → POST allow → 303 to the redirect
 * URI. Resolves to {code, state, iss, location, consent} (consent: the page).
 */
export async function authorizeAndConsent(browser, P, params, { decision = "allow" } = {}) {
  const { page, form } = await openConsent(browser, P, params);
  assert.equal(page.status, 200, `the consent page: ${page}`);
  const r = await submitConsent(browser, P, form, { decision });
  assert.equal(r.status, 303, `consent ${decision}: ${r}`);
  const q = redirectParams(r.header("location"), params.redirect_uri, `consent ${decision}`);
  return { code: q.code, state: q.state, iss: q.iss, error: q.error, location: r.header("location"), consent: page, response: r };
}

/**
 * POST <P>/__backyard/oauth/<endpoint> with a form body (a plain object;
 * arrays repeat a parameter). headers may carry Authorization or another
 * Content-Type; raw replaces the body.
 */
export function oauthPost(P, endpoint, form, { headers = {}, raw } = {}) {
  const body = new URLSearchParams();
  for (const [k, v] of Object.entries(form ?? {})) {
    if (v === undefined || v === null) continue;
    for (const one of Array.isArray(v) ? v : [v]) body.append(k, String(one));
  }
  return send(`${P}/__backyard/oauth/${endpoint}`, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded", Accept: "application/json", ...headers },
    body: raw ?? body.toString(),
  });
}

/** The token endpoint. */
export const tokenRequest = (P, form, opts) => oauthPost(P, "token", form, opts);

/** grant_type=authorization_code (redirect_uri and resource only when given). */
export function exchangeCode(P, { clientId, code, verifier, redirectURI, resource, headers }) {
  return tokenRequest(
    P,
    { grant_type: "authorization_code", client_id: clientId, code, code_verifier: verifier, redirect_uri: redirectURI, resource },
    { headers },
  );
}

/** grant_type=refresh_token. */
export function refresh(P, { clientId, refreshToken, scope, resource, headers }) {
  return tokenRequest(P, { grant_type: "refresh_token", client_id: clientId, refresh_token: refreshToken, scope, resource }, { headers });
}

/** The revocation endpoint (RFC 7009). */
export function revoke(P, { clientId, token, hint, headers }) {
  return oauthPost(P, "revoke", { token, token_type_hint: hint, client_id: clientId }, { headers });
}

/** Authorization: Basic for a public client (empty password unless given). */
export function basicAuth(clientId, password = "") {
  return `Basic ${Buffer.from(`${encodeURIComponent(clientId)}:${encodeURIComponent(password)}`).toString("base64")}`;
}

/** Asserts an OAuth error answer {"error","error_description"?} and returns its body. */
export function assertOAuthError(r, status, code, what = "") {
  const ctx = `${what ? what + ": " : ""}${r}`;
  assert.equal(r.status, status, `expected HTTP ${status} ${code}: ${ctx}`);
  assert.match(r.header("content-type") || "", /^application\/json\b/, `content type: ${ctx}`);
  const b = r.json;
  assert.equal(b.error, code, `error: ${ctx}`);
  if ("error_description" in b) assert.equal(typeof b.error_description, "string", `error_description: ${ctx}`);
  return b;
}

/** Asserts the no-cache headers of every token endpoint answer. */
export function assertNoStore(r, what = "") {
  assert.match(r.header("cache-control") || "", /\bno-store\b/, `${what}: Cache-Control: ${r}`);
  assert.match(r.header("pragma") || "", /\bno-cache\b/, `${what}: Pragma: ${r}`);
}

/** Asserts a successful token answer and returns it. */
export function assertTokens(r, { scope, what = "token" } = {}) {
  assert.equal(r.status, 200, `${what}: ${r}`);
  assert.match(r.header("content-type") || "", /^application\/json\b/, `${what}: content type`);
  assertNoStore(r, what);
  const t = r.json;
  assert.deepEqual(Object.keys(t).sort(), ["access_token", "expires_in", "refresh_token", "scope", "token_type"], `${what}: members ${JSON.stringify(Object.keys(t))}`);
  assert.equal(t.token_type, "Bearer", `${what}: token_type`);
  assert.ok(Number.isInteger(t.expires_in) && t.expires_in > 3000 && t.expires_in <= 3600, `${what}: expires_in ${t.expires_in}`);
  assert.match(t.access_token, /^byat_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/, `${what}: access token format`);
  assert.ok(t.access_token.length <= 4096, `${what}: access token ≤ 4096 bytes`);
  assert.match(t.refresh_token, /^byrt_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/, `${what}: refresh token format`);
  if (scope !== undefined) assert.equal(t.scope, scope, `${what}: scope`);
  return t;
}

/**
 * A strict parser of one WWW-Authenticate challenge (RFC 9110 §11.6.1):
 * `scheme param="value", param=token, …`. Throws on anything else, including
 * repeated parameters. Returns {scheme, params}.
 */
export function parseChallenge(header) {
  const s = String(header ?? "");
  const m = /^([!#$%&'*+.^_`|~0-9A-Za-z-]+)(?: (.*))?$/.exec(s);
  if (!m) throw new Error(`malformed WWW-Authenticate: ${JSON.stringify(s)}`);
  const params = {};
  let rest = m[2] ?? "";
  while (rest.length) {
    const p = /^([!#$%&'*+.^_`|~0-9A-Za-z-]+)=(?:"((?:[^"\\]|\\.)*)"|([!#$%&'*+.^_`|~0-9A-Za-z-]+))/.exec(rest);
    if (!p) throw new Error(`malformed WWW-Authenticate parameter in ${JSON.stringify(s)} at ${JSON.stringify(rest)}`);
    const name = p[1].toLowerCase();
    if (name in params) throw new Error(`WWW-Authenticate repeats ${name}: ${JSON.stringify(s)}`);
    params[name] = p[2] !== undefined ? p[2].replace(/\\(.)/g, "$1") : p[3];
    rest = rest.slice(p[0].length);
    if (!rest.length) break;
    if (!rest.startsWith(", ")) throw new Error(`malformed WWW-Authenticate separator in ${JSON.stringify(s)} at ${JSON.stringify(rest)}`);
    rest = rest.slice(2);
  }
  return { scheme: m[1], params };
}
