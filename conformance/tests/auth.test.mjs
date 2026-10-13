// Authentication endpoints of the hub (SPEC §8): the login redirect outside
// oidc mode, the CLI login with its cross-site confirmation, and minting API
// tokens with an API token (which never outlive the presented token).
//
// Capability detection: the CLI login needs an identity the browser itself
// carries (the guest cookie in mode none, a session or a proxy identity);
// when the server answers the cross-site probe with anything but the
// confirmation page (oidc: 302 to the IdP; a proxy mode without its
// identity: 401), those tests are skipped. Minting with an API token needs a
// non-anonymous API token: the runners pass the server's secret
// (BACKYARD_CONFORMANCE_SECRET) so the suite can sign one (the token format
// and key derivation are part of SPEC §8.3); otherwise BACKYARD_TOKEN is used
// when it is a Backyard API token, else the test is skipped.
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { before, describe, test } from "node:test";
import { hubURL, setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";
import { jwtClaims, signAPIToken } from "../lib/tokens.mjs";

const env = await setup();
const authMode = String(env.info.auth?.mode || "");
const secret = (process.env.BACKYARD_CONFORMANCE_SECRET || "").trim();
const publicOrigin = new URL(env.info.public_url || hubURL).origin;

/** Decodes the character references an HTML attribute value may hold. */
function decodeHTML(s) {
  const named = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" };
  return s.replace(/&(#x[0-9a-f]+|#[0-9]+|amp|lt|gt|quot|apos);/gi, (_, e) => {
    if (e[0] === "#") return String.fromCodePoint(e[1] === "x" || e[1] === "X" ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10));
    return named[e.toLowerCase()];
  });
}

/** The hidden fields of the first form of an HTML page, and its method and action. */
function parseForm(html) {
  const m = /<form\b([^>]*)>([\s\S]*?)<\/form>/i.exec(html);
  if (!m) return null;
  const attr = (tag, name) => {
    const a = new RegExp(`\\b${name}\\s*=\\s*("([^"]*)"|'([^']*)'|([^\\s>]+))`, "i").exec(tag);
    return a ? decodeHTML(a[2] ?? a[3] ?? a[4] ?? "") : null;
  };
  const fields = {};
  for (const input of m[2].match(/<input\b[^>]*>/gi) ?? []) {
    if ((attr(input, "type") || "").toLowerCase() !== "hidden") continue;
    fields[attr(input, "name")] = attr(input, "value") ?? "";
  }
  return { method: (attr(m[1], "method") || "get").toLowerCase(), action: attr(m[1], "action"), fields };
}

describe("hub sign-in redirect", () => {
  test("GET /__backyard/auth/login?rd=<site URL> answers 302 to rd outside oidc mode", async (t) => {
    if (authMode === "oidc") return t.skip("oidc mode: the login goes to the identity provider");
    const c = new Client({ token: null });
    const login = (rd, opts) => c.get(`${hubURL}/__backyard/auth/login?rd=${encodeURIComponent(rd)}`, opts);
    const target = `${env.canonicalSiteURL(uniqueSite("login"))}some/page?x=1#top`;
    const r = expectStatus(await login(target), 302, "a site URL");
    assert.equal(r.header("location"), target);
    assert.match(r.header("cache-control") || "", /\bno-store\b/, "the redirect is not cached");
    const head = expectStatus(await c.head(`${hubURL}/__backyard/auth/login?rd=${encodeURIComponent(target)}`), 302, "HEAD");
    assert.equal(head.header("location"), target);
    const hubPage = `${String(env.info.public_url).replace(/\/+$/, "")}/docs`;
    assert.equal(expectStatus(await login(hubPage), 302, "a hub URL").header("location"), hubPage);
    const rel = new URL(expectStatus(await login("/docs?a=b"), 302, "a hub-relative path").header("location"), hubURL);
    assert.equal(rel.origin, publicOrigin, "a relative rd stays on the hub");
    assert.equal(rel.pathname + rel.search, "/docs?a=b");
    // Never an open redirect, and never back into the login itself.
    for (const bad of ["https://evil.example/x", "//evil.example/x", "/\\evil.example", "javascript:alert(1)", `${publicOrigin}/__backyard/auth/login`]) {
      const loc = new URL(expectStatus(await login(bad), 302, bad).header("location"), hubURL);
      assert.equal(loc.origin, publicOrigin, `rd ${bad} is replaced by the hub (got ${loc})`);
      assert.notEqual(loc.pathname, "/__backyard/auth/login", `rd ${bad} does not loop`);
    }
    const none = new URL(expectStatus(await c.get(`${hubURL}/__backyard/auth/login`), 302, "no rd").header("location"), hubURL);
    assert.equal(none.origin, publicOrigin, "without rd: the hub");
  });
});

describe("CLI login (/__backyard/auth/cli)", () => {
  const callback = "http://127.0.0.1:53682/callback";
  // Characters that need escaping in HTML, in a query and in JSON.
  const state = `cf+&=/ é<>"'${randomBytes(6).toString("hex")}`;
  const cli = (q) => `${hubURL}/__backyard/auth/cli?${new URLSearchParams(q)}`;
  const crossSite = { "Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document" };
  const fromPage = { "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "navigate", Origin: "null" };
  /** The browser: the mode's own identity (guest cookie), never a bearer token. */
  const browser = new Client({ token: null });
  let skipReason = "";
  let page; // the confirmation page of the cross-site probe
  let me; // the browser's identity

  /** POSTs the confirmation form as the page would. */
  const confirm = (fields, { client = browser, headers = fromPage } = {}) =>
    client.fetch(`${hubURL}/__backyard/auth/cli`, {
      method: "POST",
      body: new URLSearchParams(fields).toString(),
      headers: { "Content-Type": "application/x-www-form-urlencoded", ...headers },
      csrf: false,
    });

  /** Asserts a 302/303 to the CLI callback carrying a token for the browser's user. */
  async function assertTokenRedirect(r, status, what) {
    expectStatus(r, status, what);
    const loc = new URL(r.header("location"));
    assert.equal(`${loc.origin}${loc.pathname}`, callback, `${what}: redirected to the CLI`);
    assert.equal(loc.searchParams.get("state"), state, `${what}: state is passed back`);
    assert.ok(!Number.isNaN(Date.parse(loc.searchParams.get("expires_at"))), `${what}: expires_at`);
    assert.match(r.header("cache-control") || "", /\bno-store\b/, `${what}: not cached`);
    assert.equal(r.header("referrer-policy"), "no-referrer", `${what}: Referrer-Policy`);
    const token = loc.searchParams.get("token");
    assert.ok(token, `${what}: token`);
    const who = expectStatus(await new Client({ token }).get(`${hubURL}/__backyard/api/me`), 200, `${what}: the token authenticates`).json;
    assert.equal(who.id, me.id, `${what}: the token is the browser user's`);
    return token;
  }

  /** Asserts that a response mints nothing: no redirect, no token in the page. */
  function assertNoToken(r, what) {
    assert.equal(r.header("location"), null, `${what}: no redirect`);
    assert.doesNotMatch(r.text, /eyJ[\w-]+\.[\w-]+\.[\w-]+/, `${what}: no token in the page`);
  }

  before(async () => {
    const r = await browser.get(cli({ redirect_uri: callback, state }), { headers: crossSite });
    if (r.status !== 200) {
      skipReason = `no browser identity in auth mode ${authMode} (the cross-site probe answered HTTP ${r.status})`;
      return;
    }
    page = r;
    me = expectStatus(await browser.get(`${hubURL}/__backyard/api/me`), 200, "the browser's identity").json;
  });

  test("a cross-site navigation gets a confirmation page (200) and no token", (t) => {
    if (skipReason) return t.skip(skipReason);
    assert.match(page.header("content-type") || "", /^text\/html\b/);
    assertNoToken(page, "cross-site GET");
    assert.match(page.header("cache-control") || "", /\bno-store\b/);
    assert.equal(page.header("referrer-policy"), "no-referrer");
    const csp = page.header("content-security-policy") || "";
    assert.match(csp, /frame-ancestors 'none'/, "the confirmation cannot be framed (clickjacking)");
    const formAction = /form-action ([^;]*)/.exec(csp)?.[1]?.trim().split(/\s+/) ?? [];
    assert.deepEqual(formAction.sort(), ["'self'", "http://127.0.0.1:53682"].sort(), `CSP form-action: ${csp}`);
    const form = parseForm(page.text);
    assert.ok(form, `a form: ${page.text.slice(0, 500)}`);
    assert.equal(form.method, "post");
    assert.equal(new URL(form.action, hubURL).pathname, "/__backyard/auth/cli");
    assert.equal(form.fields.redirect_uri, callback);
    assert.equal(form.fields.state, state);
    assert.match(form.fields.csrf || "", /^\d+\.[\w-]+$/, "CSRF token <expiry>.<base64url HMAC>");
    const exp = Number(form.fields.csrf.split(".")[0]);
    const now = Date.now() / 1000;
    assert.ok(exp > now + 5 * 60 && exp <= now + 11 * 60, `the confirmation expires after 10 minutes (exp ${exp})`);
  });

  test("other requests that a foreign page may have started need the confirmation too", async (t) => {
    if (skipReason) return t.skip(skipReason);
    const q = { redirect_uri: callback, state };
    const cases = [
      ["Sec-Fetch-Site: same-site (sites are subdomains of the hub)", { "Sec-Fetch-Site": "same-site" }],
      ["no Fetch Metadata, a foreign Referer", { Referer: "https://evil.example/page" }],
    ];
    // In path mode sites share the hub's origin (a documented trade-off: their
    // pages can mint tokens with POST /__backyard/api/tokens anyway).
    const siteRef = env.canonicalSiteURL(uniqueSite("ref"));
    if (new URL(siteRef).origin !== publicOrigin) cases.push(["no Fetch Metadata, a site's Referer", { Referer: siteRef }]);
    for (const [what, headers] of cases) {
      const r = expectStatus(await browser.get(cli(q), { headers }), 200, what);
      assertNoToken(r, what);
      assert.equal(parseForm(r.text)?.fields.state, state, `${what}: confirmation form`);
    }
    const display = expectStatus(await browser.get(cli({ mode: "display" }), { headers: crossSite }), 200, "mode=display");
    assertNoToken(display, "mode=display, cross-site");
    assert.equal(parseForm(display.text)?.fields.mode, "display");
  });

  test("POST with the page's CSRF token answers 303 to the CLI with a token", async (t) => {
    if (skipReason) return t.skip(skipReason);
    const r = await confirm(parseForm(page.text).fields);
    await assertTokenRedirect(r, 303, "confirmed POST");
  });

  test("the confirmation is refused (403) unless it verifies for this user, request and page", async (t) => {
    if (skipReason) return t.skip(skipReason);
    const fields = parseForm(page.text).fields;
    const [exp, mac] = fields.csrf.split(".");
    const flipped = mac.slice(0, -2) + (mac.at(-2) === "A" ? "B" : "A") + mac.at(-1);
    const cases = [
      ["no CSRF token", { ...fields, csrf: undefined }],
      ["a tampered CSRF token", { ...fields, csrf: `${exp}.${flipped}` }],
      ["an expired CSRF token", { ...fields, csrf: `${Math.floor(Date.now() / 1000) - 5}.${mac}` }],
      ["another state", { ...fields, state: state + "x" }],
      ["another loopback port", { ...fields, redirect_uri: "http://127.0.0.1:53683/callback" }],
      ["display instead of the redirect", { ...fields, mode: "display" }],
    ];
    for (const [what, f] of cases) {
      const clean = Object.fromEntries(Object.entries(f).filter(([, v]) => v !== undefined));
      const r = await confirm(clean);
      assert.equal(r.status, 403, `${what}: ${r}`);
      assertNoToken(r, what);
    }
    const signals = [
      ["Sec-Fetch-Site: cross-site", { ...fromPage, "Sec-Fetch-Site": "cross-site" }],
      ["Sec-Fetch-Site: same-site", { ...fromPage, "Sec-Fetch-Site": "same-site" }],
      ["a foreign Origin", { ...fromPage, Origin: "https://evil.example" }],
    ];
    for (const [what, headers] of signals) {
      const r = await confirm(fields, { headers });
      assert.equal(r.status, 403, `${what}: ${r}`);
      assertNoToken(r, what);
    }
    const stranger = await confirm(fields, { client: new Client({ token: null }) });
    assert.equal(stranger.status, 403, `the page of another browser user: ${stranger}`);
    assertNoToken(stranger, "another browser");
  });

  test("mode=display: the confirmed POST shows the token", async (t) => {
    if (skipReason) return t.skip(skipReason);
    const p = expectStatus(await browser.get(cli({ mode: "display" }), { headers: crossSite }), 200);
    const r = expectStatus(await confirm(parseForm(p.text).fields), 200, "confirmed display");
    assert.match(r.header("content-type") || "", /^text\/html\b/);
    assert.match(r.header("cache-control") || "", /\bno-store\b/);
    const token = /--token\s+([\w-]+\.[\w-]+\.[\w-]+)/.exec(r.text)?.[1];
    assert.ok(token, `the page shows "backyard login … --token <token>": ${r.text.slice(0, 600)}`);
    const who = expectStatus(await new Client({ token }).get(`${hubURL}/__backyard/api/me`), 200).json;
    assert.equal(who.id, me.id);
  });

  test("a navigation the user started (or a same-origin one) mints directly with 302", async (t) => {
    if (skipReason) return t.skip(skipReason);
    const q = { redirect_uri: callback, state };
    await assertTokenRedirect(await browser.get(cli(q), { headers: { "Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate" } }), 302, "Sec-Fetch-Site: none");
    await assertTokenRedirect(await browser.get(cli(q), { headers: { "Sec-Fetch-Site": "same-origin" } }), 302, "Sec-Fetch-Site: same-origin");
    await assertTokenRedirect(await browser.get(cli(q)), 302, "no Fetch Metadata, no Referer");
    await assertTokenRedirect(await browser.get(cli(q), { headers: { Referer: `${hubURL}/` } }), 302, "no Fetch Metadata, the hub's Referer");
  });

  test("invalid requests and other methods mint nothing", async (t) => {
    if (skipReason) return t.skip(skipReason);
    for (const [what, q] of [
      ["a non-loopback redirect_uri", { redirect_uri: "http://evil.example/cb", state }],
      ["no state", { redirect_uri: callback }],
    ]) {
      const r = await browser.get(cli(q), { headers: { "Sec-Fetch-Site": "none" } });
      assert.equal(r.status, 400, `${what}: ${r}`);
      assertNoToken(r, what);
    }
    const put = await browser.fetch(cli({ redirect_uri: callback, state }), { method: "PUT", csrf: false });
    assert.equal(put.status, 405, `PUT: ${put}`);
    assert.match(put.header("allow") || "", /\bPOST\b/);
  });
});

describe("API tokens (POST /__backyard/api/tokens)", () => {
  /** A 1h API token of a real (non-anonymous) user, or a reason to skip. */
  async function oneHourToken() {
    if (secret) {
      const id = `${uniqueSite("tokens")}@example.com`;
      return { token: signAPIToken(secret, { id, email: id, name: "Conformance Tokens" }, { ttlSeconds: 3600 }) };
    }
    if (jwtClaims(process.env.BACKYARD_TOKEN)?.iss === "backyard") {
      const r = await new Client().post(`${hubURL}/__backyard/api/tokens`, { name: "conformance 1h", ttl_hours: 1 });
      if (r.status === 200) return { token: r.json.token };
    }
    return { skip: "set BACKYARD_CONFORMANCE_SECRET (the server's secret) or a Backyard API token as BACKYARD_TOKEN" };
  }

  test("a token minted with an API token never outlives it", async (t) => {
    const { token: first, skip } = await oneHourToken();
    if (skip) return t.skip(skip);
    const presented = jwtClaims(first).exp;
    const left = presented - Date.now() / 1000;
    if (left < 20 * 60) return t.skip(`the API token has only ${Math.round(left)}s left`);
    const c = new Client({ token: first });
    const me = expectStatus(await c.get(`${hubURL}/__backyard/api/me`), 200, "the API token authenticates").json;
    assert.equal(me.anonymous, false);
    let chained = first;
    for (const body of [{ name: "default ttl" }, { name: "longer", ttl_hours: 2 }, {}]) {
      const r = expectStatus(await c.post(`${hubURL}/__backyard/api/tokens`, body), 200, JSON.stringify(body)).json;
      const exp = jwtClaims(r.token)?.exp;
      assert.ok(Number.isInteger(exp), `minted exp: ${JSON.stringify(r)}`);
      assert.ok(exp <= presented, `${JSON.stringify(body)}: exp ${exp} is not after the presented token's ${presented}`);
      if (!body.ttl_hours || body.ttl_hours * 3600 > left) {
        assert.ok(exp >= presented - 120, `${JSON.stringify(body)}: capped at the presented token's expiry, not shorter (${exp} vs ${presented})`);
      }
      assert.ok(Date.parse(r.expires_at) <= presented * 1000, `expires_at ${r.expires_at} reports the capped expiry`);
      assert.equal(Math.floor(Date.parse(r.expires_at) / 1000), exp, "expires_at is the token's exp");
      if (body.name) assert.equal(r.name, body.name);
      const who = expectStatus(await new Client({ token: r.token }).get(`${hubURL}/__backyard/api/me`), 200).json;
      assert.equal(who.id, me.id, "the minted token is the same user's");
      chained = r.token;
    }
    // A shorter lifetime than what is left is honoured.
    const short = expectStatus(await c.post(`${hubURL}/__backyard/api/tokens`, { ttl_hours: 0.25 }), 200).json;
    const shortExp = jwtClaims(short.token).exp;
    assert.ok(shortExp <= Date.now() / 1000 + 15 * 60 + 5 && shortExp >= Date.now() / 1000 + 14 * 60, `ttl_hours 0.25: exp ${shortExp}`);
    // Chaining cannot extend the lifetime either.
    const again = expectStatus(await new Client({ token: chained }).post(`${hubURL}/__backyard/api/tokens`, { ttl_hours: 2 }), 200).json;
    assert.ok(jwtClaims(again.token).exp <= jwtClaims(chained).exp, "a token minted from a minted token");
    assert.ok(jwtClaims(again.token).exp <= presented);
  });

  test("an API token with (almost) nothing left cannot mint: 401 unauthenticated", async (t) => {
    if (!secret) return t.skip("set BACKYARD_CONFORMANCE_SECRET (the server's secret)");
    const id = `${uniqueSite("tokens")}@example.com`;
    const now = Math.floor(Date.now() / 1000);
    const dying = signAPIToken(secret, { id, email: id }, { now: now - 60, exp: now + 1 });
    const r = await new Client({ token: dying }).post(`${hubURL}/__backyard/api/tokens`, { name: "too late" });
    assertError(r, 401, "unauthenticated");
  });
});
