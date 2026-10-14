// The Claude connector (SPEC §8.4, §14.1): Backyard's own OAuth 2.1
// authorization server and the remote MCP endpoint, driven without a browser.
//
// The suite runs only when /__backyard/api/info advertises mcp_url; with
// BACKYARD_CONFORMANCE_MCP=1 (the runners) a missing mcp_url is a failure.
//
// Identities: in auth mode none every lib/oauth.mjs Browser is a new guest. In
// the Go runner's oidc pass BACKYARD_CONFORMANCE_IDP names the mock IdP
// (conformance/mock/idp.mjs) and BACKYARD_CONFORMANCE_IDP_USERS its people
// (default alice,bob): a Browser with the cookie idp_session=<user> on the
// IdP's origin signs in without a page. Other modes (a proxy in front) have no
// identity the suite can carry; the tests that need a signed-in browser are
// skipped there.
//
// Every flow registers its own client (stateless DCR) with the loopback
// redirect URI http://127.0.0.1:9/callback and stops at the redirect to it.
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { after, before, describe, test } from "node:test";
import { deleteSiteQuietly } from "../lib/deploy.mjs";
import { token as envToken, repoRoot, setup, uniqueSite } from "../lib/env.mjs";
import { Client, rawRequest } from "../lib/http.mjs";
import { MCPClient, PROTOCOL_VERSIONS, assertRPCError, postSized } from "../lib/mcp.mjs";
import * as oauth from "../lib/oauth.mjs";
import { RFC3339_UTC } from "../lib/shapes.mjs";
import { signAPIToken } from "../lib/tokens.mjs";

const env = await setup();
const required = process.env.BACKYARD_CONFORMANCE_MCP === "1";

if (!env.info.mcp_url) {
  test("the Claude connector is enabled (SPEC §8.4)", (t) => {
    if (required) assert.fail("BACKYARD_CONFORMANCE_MCP=1, but /__backyard/api/info has no mcp_url: the connector is not enabled");
    t.skip("the Claude connector is not enabled on this server (/__backyard/api/info has no mcp_url)");
  });
} else {
  connectorSuite();
}

function connectorSuite() {
  const { P, R, M, H } = oauth.connectorURLs(env.info.mcp_url);
  const authMode = String(env.info.auth?.mode || "");
  const cloudflare = env.info.edition === "cloudflare";
  const secret = (process.env.BACKYARD_CONFORMANCE_SECRET || "").trim();
  const idpURL = (process.env.BACKYARD_CONFORMANCE_IDP || "").trim().replace(/\/+$/, "");
  const idpUsers = (process.env.BACKYARD_CONFORMANCE_IDP_USERS || "alice,bob").split(/[\s,]+/).filter(Boolean);
  const oidc = authMode === "oidc" && Boolean(idpURL);
  const signInSkip = authMode === "none" || oidc ? "" : `no browser identity the suite can carry in auth mode ${authMode} (set BACKYARD_CONFORMANCE_IDP for oidc)`;
  const LOOPBACK = "http://127.0.0.1:9/callback";
  const CHALLENGE_MISSING = `Bearer resource_metadata="${M}", scope="mcp"`;
  const challengeInvalid = (desc) => `Bearer error="invalid_token", error_description="${desc}", resource_metadata="${M}", scope="mcp"`;
  const UNAUTHENTICATED = { error: { code: "unauthenticated", message: "connect this app to Backyard to sign in" } };
  const catalogPath = join(repoRoot, "internal", "mcpremote", "tools.json");
  const catalog = existsSync(catalogPath) ? JSON.parse(readFileSync(catalogPath, "utf8")) : null;
  const tag = () => randomBytes(3).toString("hex");

  /** A new person: a guest (mode none) or, with the mock IdP, people[i]. */
  function person(i = 0) {
    const b = new oauth.Browser();
    if (oidc) b.setCookie(idpURL, "idp_session", idpUsers[i % idpUsers.length]);
    return b;
  }

  /**
   * The identity a browser has on the hub (GET /__backyard/api/me). A guest
   * gets its cookie on the way; with the IdP a browser without a session
   * signs in through /__backyard/auth/login first.
   */
  async function signIn(browser) {
    const me = `${P}/__backyard/api/me`;
    let r = await browser.request(me);
    if (r.status === 401 && oidc) r = (await browser.navigate(`${P}/__backyard/auth/login?rd=${encodeURIComponent(me)}`, { site: "same-origin" })).response;
    assert.equal(r.status, 200, `the browser's identity: ${r}`);
    return r.json;
  }

  /** Asserts that a machine path answer neither sets cookies nor allows CORS. */
  function assertMachine(r, what) {
    assert.deepEqual(r.headers.getSetCookie(), [], `${what}: no Set-Cookie: ${r}`);
    for (const [k] of r.headers) assert.ok(!k.startsWith("access-control-"), `${what}: no CORS header ${k}`);
  }

  /** Asserts the exact JSON body of an answer. */
  function assertBody(r, want, what) {
    assert.match(r.header("content-type") || "", /^application\/json\b/, `${what}: content type: ${r}`);
    assert.deepEqual(r.json, want, `${what}: ${r}`);
  }

  /** Asserts the answer of a request with an invalid MCP access token. */
  function assertInvalidToken(r, desc, what) {
    assert.equal(r.status, 401, `${what}: ${r}`);
    assert.equal(r.header("www-authenticate"), challengeInvalid(desc), `${what}: WWW-Authenticate`);
    const c = oauth.parseChallenge(r.header("www-authenticate"));
    assert.deepEqual(c, { scheme: "Bearer", params: { error: "invalid_token", error_description: desc, resource_metadata: M, scope: "mcp" } });
    assertBody(r, UNAUTHENTICATED, what);
  }

  /** Asserts an error page answer: HTML with the status, never a redirect. */
  function assertPage(r, status, what) {
    assert.equal(r.status, status, `${what}: ${r}`);
    assert.equal(r.header("location"), null, `${what}: no redirect`);
    assert.match(r.header("content-type") || "", /^text\/html\b/, `${what}: an HTML page`);
    assert.doesNotMatch(r.text, /byac_/, `${what}: no code in the page`);
  }

  /** Asserts the 302/303 error redirect to the client of SPEC §8.4.5. */
  function assertErrorRedirect(r, { status = 302, error, state, redirectURI = LOOPBACK, what }) {
    assert.equal(r.status, status, `${what}: ${r}`);
    const q = oauth.redirectParams(r.header("location"), redirectURI, what);
    assert.equal(q.error, error, `${what}: error (${r.header("location")})`);
    assert.equal(q.iss, P, `${what}: iss`);
    assert.equal(q.state, state, `${what}: state`);
    assert.ok(!("code" in q), `${what}: no code`);
    if ("error_description" in q) assert.ok(q.error_description.length > 0, `${what}: error_description`);
    assert.match(r.header("cache-control") || "", /\bno-store\b/, `${what}: Cache-Control`);
    assert.equal(r.header("referrer-policy"), "no-referrer", `${what}: Referrer-Policy`);
    return q;
  }

  /** The client registered for a flow (unique name per call). */
  const newClient = (opts = {}) => oauth.registerClient(P, { name: `Conformance ${tag()}`, ...opts });

  /**
   * Authorizes client for browser up to the code. Resolves to {code, verifier,
   * state, consent, params}.
   */
  async function authorize(browser, client, { redirectURI = client.redirect_uris[0], scope = "mcp offline_access", resource = R } = {}) {
    const { verifier, challenge } = oauth.pkce();
    const state = oauth.newState();
    const params = oauth.authorizeParams({ client, redirectURI, challenge, state, resource, scope });
    const a = await oauth.authorizeAndConsent(browser, P, params);
    assert.match(a.code ?? "", /^byac_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/, `a code: ${a.location}`);
    assert.equal(a.state, state, "the state comes back");
    assert.equal(a.iss, P, "iss is the issuer");
    return { code: a.code, verifier, state, consent: a.consent, params, redirectURI };
  }

  /**
   * A complete connection: register, authorize, exchange. Resolves to the
   * token answer plus {client, mcp, consent}.
   */
  async function connect(browser, { client, scope = "mcp offline_access" } = {}) {
    client ??= await newClient();
    const a = await authorize(browser, client, { scope });
    const t = oauth.assertTokens(
      await oauth.exchangeCode(P, { clientId: client.client_id, code: a.code, verifier: a.verifier, redirectURI: a.redirectURI, resource: R }),
      { scope, what: "code exchange" },
    );
    return { ...t, client, consent: a.consent, mcp: new MCPClient({ endpoint: R, token: t.access_token }) };
  }

  /** Flips one character of the signature part of a signed value. */
  function tamper(value) {
    const i = value.length - 3;
    return value.slice(0, i) + (value[i] === "A" ? "B" : "A") + value.slice(i + 1);
  }

  /**
   * Basic for a public client with the base64 padding stripped: the first
   * k characters of the client_id are %-escaped (servers unescape them) until
   * the padded form has padding to strip.
   */
  function unpaddedBasic(clientId) {
    for (let k = 0; k < 3; k++) {
      const user = [...clientId.slice(0, k)].map((c) => "%" + c.charCodeAt(0).toString(16).padStart(2, "0")).join("") + clientId.slice(k);
      const b64 = Buffer.from(`${user}:`).toString("base64");
      if (b64.endsWith("=")) return `Basic ${b64.replace(/=+$/, "")}`;
    }
    throw new Error("unreachable");
  }

  // ---------------------------------------------------------------------------

  describe("connector discovery (SPEC §8.4.2, §14.1)", () => {
    const anon = new MCPClient({ endpoint: R, token: "" });
    const initialize = { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: { name: "c", version: "1" } } };

    test("POST /__backyard/mcp without a token: 401 with the challenge that names the metadata", async () => {
      for (const [what, headers] of [
        ["no Authorization", {}],
        ["another scheme", { Authorization: "Basic YTpi" }],
      ]) {
        const r = await anon.post(initialize, { token: null, headers, what });
        assert.equal(r.status, 401, `${what}: ${r}`);
        assert.equal(r.header("www-authenticate"), CHALLENGE_MISSING, `${what}: WWW-Authenticate`);
        assert.deepEqual(oauth.parseChallenge(r.header("www-authenticate")), { scheme: "Bearer", params: { resource_metadata: M, scope: "mcp" } });
        assertBody(r, UNAUTHENTICATED, what);
      }
    });

    test("a token Backyard did not issue: 401 invalid_token (malformed token)", async () => {
      for (const [what, tok] of [
        ["garbage", "nonsense"],
        ["a forged access token", `byat_${Buffer.from('{"g":"00000000000000000000000000000000","i":1,"x":9999999999}').toString("base64url")}.AAAA`],
        ["over 4096 bytes", `byat_${"a".repeat(5000)}.b`],
        ["a refresh token", "byrt_eyJnIjoiMCIsIm4iOjF9.AAAA"],
      ]) {
        assertInvalidToken(await anon.post(initialize, { token: tok, what }), "malformed token", what);
      }
      const lower = await anon.post(initialize, { token: null, headers: { Authorization: "bearer nonsense" } });
      assertInvalidToken(lower, "malformed token", "the scheme is case-insensitive");
    });

    test("two Authorization headers: 400 invalid_request", async () => {
      const r = await rawRequest(R, {
        method: "POST",
        headers: { "content-type": "application/json", authorization: ["Bearer a", "Bearer b"] },
        body: JSON.stringify(initialize),
      });
      assert.equal(r.status, 400, `two Authorization headers: HTTP ${r.status} ${r.body}`);
      assert.equal(r.headers["www-authenticate"], `Bearer error="invalid_request", resource_metadata="${M}"`);
      assert.equal(JSON.parse(r.body.toString()).error.code, "bad_request");
    });

    test("the protected resource metadata (RFC 9728)", async () => {
      const r = await oauth.send(M);
      assert.equal(r.status, 200, `GET ${M}: ${r}`);
      assert.match(r.header("content-type") || "", /^application\/json\b/);
      assert.equal(r.header("cache-control"), "public, max-age=300");
      assertMachine(r, "PRM");
      const want = { resource: R, authorization_servers: [P], scopes_supported: ["mcp"], bearer_methods_supported: ["header"], resource_name: String(env.info.name || "Backyard") };
      assert.deepEqual(Object.keys(r.json), Object.keys(want), "the members, in order");
      assert.deepEqual(r.json, want);
      const head = await oauth.send(M, { method: "HEAD" });
      assert.equal(head.status, 200, `HEAD: ${head}`);
      assert.equal(head.body.length, 0, "HEAD has no body");
      assert.equal(head.header("cache-control"), "public, max-age=300");
      assert.match(head.header("content-type") || "", /^application\/json\b/);
    });

    test("the authorization server metadata (RFC 8414)", async () => {
      const url = `${P}/.well-known/oauth-authorization-server`;
      const r = await oauth.send(url);
      assert.equal(r.status, 200, `GET ${url}: ${r}`);
      assert.match(r.header("content-type") || "", /^application\/json\b/);
      assert.equal(r.header("cache-control"), "public, max-age=300");
      assertMachine(r, "AS metadata");
      const want = {
        issuer: P,
        authorization_endpoint: `${P}/__backyard/auth/authorize`,
        token_endpoint: `${P}/__backyard/oauth/token`,
        registration_endpoint: `${P}/__backyard/oauth/register`,
        revocation_endpoint: `${P}/__backyard/oauth/revoke`,
        scopes_supported: ["mcp", "offline_access"],
        response_types_supported: ["code"],
        response_modes_supported: ["query"],
        grant_types_supported: ["authorization_code", "refresh_token"],
        token_endpoint_auth_methods_supported: ["none"],
        revocation_endpoint_auth_methods_supported: ["none"],
        code_challenge_methods_supported: ["S256"],
        authorization_response_iss_parameter_supported: true,
      };
      // CIMD is advertised while the client allowlist is non-empty (the
      // runners use the default list).
      if (required || "client_id_metadata_document_supported" in r.json) want.client_id_metadata_document_supported = true;
      assert.deepEqual(Object.keys(r.json), Object.keys(want), "the members, in order");
      assert.deepEqual(r.json, want);
      const prm = (await oauth.send(M)).json;
      assert.equal(r.json.issuer, prm.authorization_servers[0], "the issuer is the protected resource's authorization server");
      const head = await oauth.send(url, { method: "HEAD" });
      assert.equal(head.status, 200, `HEAD: ${head}`);
      assert.equal(head.body.length, 0, "HEAD has no body");
    });

    test("wrong methods, unknown OAuth paths and no CORS", async () => {
      const notAllowed = { error: { code: "bad_request", message: "method not allowed" } };
      for (const [path, method, allow] of [
        ["/.well-known/oauth-authorization-server", "POST", "GET, HEAD"],
        ["/.well-known/oauth-protected-resource/__backyard/mcp", "DELETE", "GET, HEAD"],
        ["/.well-known/oauth-authorization-server", "OPTIONS", "GET, HEAD"],
        ["/__backyard/oauth/register", "GET", "POST"],
        ["/__backyard/oauth/token", "GET", "POST"],
        ["/__backyard/oauth/revoke", "GET", "POST"],
        ["/__backyard/oauth/token", "OPTIONS", "POST"],
      ]) {
        const r = await oauth.send(P + path, { method, headers: { Origin: "https://claude.ai", "Access-Control-Request-Method": "POST" } });
        const what = `${method} ${path}`;
        assert.equal(r.status, 405, `${what}: ${r}`);
        assert.equal(r.header("allow"), allow, `${what}: Allow`);
        assertMachine(r, what);
        if (method !== "HEAD") assertBody(r, notAllowed, what);
      }
      for (const method of ["GET", "POST"]) {
        const r = await oauth.send(`${P}/__backyard/oauth/nope`, { method });
        assert.equal(r.status, 404, `${method} /__backyard/oauth/nope: ${r}`);
        assert.match(r.header("content-type") || "", /^application\/json\b/);
        assert.equal(r.json.error.code, "not_found");
      }
      for (const path of ["/.well-known/oauth-protected-resource", "/.well-known/openid-configuration"]) {
        const r = await oauth.send(P + path);
        assert.notEqual(r.status, 200, `${path} is not served: ${r}`);
      }
    });
  });

  // ---------------------------------------------------------------------------

  describe("client registration (SPEC §8.4.3)", () => {
    test("a loopback redirect URI registers a public client (201)", async () => {
      const name = `Conformance ${tag()}`;
      const before = Math.floor(Date.now() / 1000);
      const r = await oauth.register(P, { client_name: name, redirect_uris: [LOOPBACK] });
      assert.equal(r.status, 201, `${r}`);
      assert.match(r.header("content-type") || "", /^application\/json\b/);
      assert.match(r.header("cache-control") || "", /\bno-store\b/);
      assertMachine(r, "register");
      const c = r.json;
      assert.deepEqual(
        Object.keys(c).sort(),
        ["client_id", "client_id_issued_at", "client_name", "grant_types", "redirect_uris", "response_types", "token_endpoint_auth_method"],
        JSON.stringify(c),
      );
      assert.match(c.client_id, /^bydc_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
      assert.ok(c.client_id.length <= 1024, "client_id ≤ 1024 bytes");
      assert.ok(Number.isInteger(c.client_id_issued_at) && Math.abs(c.client_id_issued_at - before) < 300, `client_id_issued_at ${c.client_id_issued_at}`);
      assert.equal(c.client_name, name);
      assert.deepEqual(c.redirect_uris, [LOOPBACK]);
      assert.deepEqual(c.grant_types, ["authorization_code", "refresh_token"]);
      assert.deepEqual(c.response_types, ["code"]);
      assert.equal(c.token_endpoint_auth_method, "none");
    });

    test("loopback hosts with or without a port, and the default Claude callback", async () => {
      const uris = ["http://localhost/callback", "http://127.0.0.1:53682/callback", "http://[::1]:1/cb", "http://localhost:8080/cb?x=1"];
      const c = await newClient({ redirectURIs: uris });
      assert.deepEqual(c.redirect_uris, uris);
      if (required) {
        // The runners use the default mcp.redirect_uris / MCP_REDIRECT_URIS.
        for (const u of ["https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"]) {
          assert.deepEqual((await newClient({ redirectURIs: [u] })).redirect_uris, [u]);
        }
      }
    });

    test("other client metadata is accepted and replaced by what Backyard registers", async () => {
      const r = await oauth.register(P, {
        client_name: "Metadata",
        redirect_uris: [LOOPBACK],
        token_endpoint_auth_method: "client_secret_basic",
        grant_types: ["authorization_code", "urn:ietf:params:oauth:grant-type:jwt-bearer"],
        response_types: ["code", "token"],
        scope: "mcp profile",
        application_type: "native",
        logo_uri: "https://evil.example/logo.png",
        client_uri: "https://evil.example/",
        contacts: ["x@evil.example"],
        software_statement: "e30.e30.sig",
        jwks_uri: "https://evil.example/jwks",
        something_else: { nested: true },
      });
      assert.equal(r.status, 201, `${r}`);
      assert.equal(r.json.token_endpoint_auth_method, "none");
      assert.deepEqual(r.json.grant_types, ["authorization_code", "refresh_token"]);
      assert.deepEqual(r.json.response_types, ["code"]);
    });

    test("client_name is sanitized (controls, bidi, 64 code points)", async () => {
      const cases = [
        ["  Evil‮⁦ App\u0007\u0000‏ ", "Evil App"],
        ["😀".repeat(70), "😀".repeat(64)],
        ["‮\u0001  ", "MCP client"],
        [undefined, "MCP client"],
      ];
      for (const [sent, want] of cases) {
        const r = await oauth.register(P, { client_name: sent, redirect_uris: [LOOPBACK] });
        assert.equal(r.status, 201, `${JSON.stringify(sent)}: ${r}`);
        assert.equal(r.json.client_name, want, `client_name ${JSON.stringify(sent)}`);
      }
    });

    test("redirect URIs outside the policy are refused (400 invalid_redirect_uri)", async () => {
      for (const uri of [
        "https://evil.example/cb",
        "https://claude.ai/other",
        "https://claude.ai/api/mcp/auth_callback/",
        "http://claude.ai/api/mcp/auth_callback",
        "cursor://callback",
        "http://127.0.0.1:9/callback#frag",
        "http://user@127.0.0.1:9/callback",
        "http://localhost.evil.example/callback",
        "http://127.0.0.2/callback",
        "https://localhost/callback",
        // Not an RFC 3986 URI: a malformed escape, a control character.
        "http://127.0.0.1:9/%zz",
        "http://127.0.0.1:9/a\u0001b",
      ]) {
        const r = await oauth.register(P, { client_name: "Refused", redirect_uris: [LOOPBACK, uri] });
        const b = oauth.assertOAuthError(r, 400, "invalid_redirect_uri", uri);
        assert.ok(String(b.error_description).includes(uri), `the description names ${uri}: ${b.error_description}`);
        assertMachine(r, uri);
      }
    });

    test("malformed registrations are refused (400)", async () => {
      const either = ["invalid_client_metadata", "invalid_redirect_uri"];
      const long = `http://127.0.0.1/${"a".repeat(500)}`;
      for (const [what, opts, codes] of [
        ["text/plain", { raw: JSON.stringify({ redirect_uris: [LOOPBACK] }), headers: { "Content-Type": "text/plain" } }, ["invalid_client_metadata"]],
        ["invalid JSON", { raw: "{" }, ["invalid_client_metadata"]],
        ["an array", { raw: JSON.stringify([{ redirect_uris: [LOOPBACK] }]) }, ["invalid_client_metadata"]],
        ["a body over 16 KiB", { raw: JSON.stringify({ redirect_uris: [LOOPBACK], client_name: "x".repeat(17 * 1024) }) }, ["invalid_client_metadata"]],
        ["no redirect_uris", { raw: JSON.stringify({ client_name: "x" }) }, either],
        ["redirect_uris empty", { raw: JSON.stringify({ redirect_uris: [] }) }, either],
        ["redirect_uris not an array", { raw: JSON.stringify({ redirect_uris: LOOPBACK }) }, either],
        ["5 redirect URIs", { raw: JSON.stringify({ redirect_uris: [1, 2, 3, 4, 5].map((n) => `http://127.0.0.1:${n}/cb`) }) }, either],
        ["a 513-byte redirect URI", { raw: JSON.stringify({ redirect_uris: [long] }) }, either],
        ["a client_id over 1024 bytes", { raw: JSON.stringify({ redirect_uris: [1, 2, 3, 4].map((n) => `http://127.0.0.1/${n}${"a".repeat(470)}`) }) }, ["invalid_client_metadata"]],
        ["a UTF-8 BOM before the JSON", { raw: "\ufeff" + JSON.stringify({ redirect_uris: [LOOPBACK] }) }, ["invalid_client_metadata"]],
      ]) {
        const r = await oauth.register(P, null, opts);
        assert.equal(r.status, 400, `${what}: ${r}`);
        assert.ok(codes.includes(r.json.error), `${what}: error ${r.json.error}`);
      }
    });
  });

  // ---------------------------------------------------------------------------

  describe("authorization requests (SPEC §8.4.5)", () => {
    let client;
    let challenge;
    const state = oauth.newState();
    const good = () => oauth.authorizeParams({ client, redirectURI: LOOPBACK, challenge, state, resource: R });

    before(async () => {
      client = await newClient();
      challenge = oauth.pkce().challenge;
    });

    test("an invalid client or redirect URI gets an error page and is never redirected", async () => {
      const browser = new oauth.Browser();
      const cases = [
        ["a tampered client_id", { ...good(), client_id: tamper(client.client_id) }, 400],
        ["no client_id", { ...good(), client_id: undefined }, 400],
        ["an unknown kind of client_id", { ...good(), client_id: "hello" }, 400],
        ["a client metadata URL that is not allowlisted", { ...good(), client_id: "https://example.com/x" }, 400],
        ["an unregistered redirect URI", { ...good(), redirect_uri: "http://127.0.0.1:9/other" }, 400],
        ["a foreign redirect URI", { ...good(), redirect_uri: "https://evil.example/cb" }, 400],
        ["another loopback host", { ...good(), redirect_uri: "http://localhost:9/callback" }, 400],
        ["no redirect_uri", { ...good(), redirect_uri: undefined }, 400],
        ["a repeated state", { ...good(), state: ["a", "b"] }, 400],
        ["a repeated client_id", { ...good(), client_id: [client.client_id, client.client_id] }, 400],
        ["a request URI over 2048 bytes", { ...good(), prompt: "x".repeat(2100) }, 400],
      ];
      for (const [what, params, status] of cases) {
        const r = await browser.request(oauth.authorizeURL(P, params), { headers: oauth.CROSS_SITE_NAVIGATION });
        assertPage(r, status, what);
      }
    });

    test("other invalid requests are redirected to the client with error, state and iss", async () => {
      const browser = new oauth.Browser();
      const cases = [
        ["no code_challenge", { code_challenge: undefined }, "invalid_request"],
        ["code_challenge_method plain", { code_challenge_method: "plain" }, "invalid_request"],
        ["no code_challenge_method", { code_challenge_method: undefined }, "invalid_request"],
        ["a short code_challenge", { code_challenge: "abc" }, "invalid_request"],
        ["a foreign resource", { resource: "https://evil.example/__backyard/mcp" }, "invalid_target"],
        ["the resource with a query", { resource: `${R}?x=1` }, "invalid_target"],
        ["an empty resource", { resource: "" }, "invalid_target"],
        ["response_type token", { response_type: "token" }, "unsupported_response_type"],
        ["a request object", { request: "e30.e30." }, "request_not_supported"],
        ["a request_uri", { request_uri: "urn:x" }, "request_uri_not_supported"],
        ["response_mode fragment", { response_mode: "fragment" }, "invalid_request"],
      ];
      for (const [what, change, error] of cases) {
        const r = await browser.request(oauth.authorizeURL(P, { ...good(), ...change }), { headers: oauth.CROSS_SITE_NAVIGATION });
        assertErrorRedirect(r, { error, state, what });
      }
      const long = await browser.request(oauth.authorizeURL(P, { ...good(), state: "s".repeat(513) }), { headers: oauth.CROSS_SITE_NAVIGATION });
      assertErrorRedirect(long, { error: "invalid_request", state: undefined, what: "a state over 512 bytes (sent back without state)" });
    });

    test("an error redirect keeps the registered URI's query", async () => {
      const withQuery = "http://127.0.0.1:9/callback?a=1";
      const c = await newClient({ redirectURIs: [withQuery] });
      const params = oauth.authorizeParams({ client: c, redirectURI: withQuery, challenge, state, resource: R, extra: { response_type: "token" } });
      const r = await new oauth.Browser().request(oauth.authorizeURL(P, params));
      const q = assertErrorRedirect(r, { error: "unsupported_response_type", state, redirectURI: withQuery, what: "a redirect URI with a query" });
      assert.equal(q.a, "1");
    });

    test("oidc: a browser without a session is sent to the sign-in first", async (t) => {
      if (!oidc) return t.skip("only in auth mode oidc with the mock IdP");
      const url = oauth.authorizeURL(P, good());
      const r = await new oauth.Browser().request(url, { headers: oauth.CROSS_SITE_NAVIGATION });
      assert.equal(r.status, 302, `${r}`);
      const loc = new URL(r.header("location"), P);
      assert.equal(loc.pathname, "/__backyard/auth/login", `to the sign-in: ${loc}`);
      const rd = new URL(loc.searchParams.get("rd"), P);
      assert.equal(`${rd.origin}${rd.pathname}`, `${P}${oauth.AUTHORIZE_PATH}`, `rd is the authorize URL: ${rd}`);
      assert.deepEqual([...rd.searchParams].sort(), [...new URL(url).searchParams].sort(), "rd keeps the request");
    });
  });

  // ---------------------------------------------------------------------------

  describe("consent (SPEC §8.4.5)", () => {
    let browser;
    let me;
    let client;
    let page;
    let form;
    let pkcePair;
    const state = oauth.newState();
    const name = `Conformance <b>&</b> ${tag()}`;
    const escapedName = name.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");

    before(async () => {
      if (signInSkip) return;
      browser = person(0);
      client = await oauth.registerClient(P, { name, redirectURIs: [LOOPBACK] });
      pkcePair = oauth.pkce();
      const opened = await oauth.openConsent(browser, P, oauth.authorizeParams({ client, challenge: pkcePair.challenge, state, resource: R }));
      page = opened.page;
      form = opened.form;
      me = await signIn(browser);
    });

    test("a cross-site navigation gets the consent page with its security headers", (t) => {
      if (signInSkip) return t.skip(signInSkip);
      assert.equal(page.status, 200, `${page}`);
      assert.match(page.header("content-type") || "", /^text\/html\b/);
      assert.match(page.header("cache-control") || "", /\bno-store\b/);
      assert.equal(page.header("referrer-policy"), "no-referrer");
      assert.equal(page.header("x-frame-options"), "DENY");
      assert.equal(page.header("x-content-type-options"), "nosniff");
      const csp = page.header("content-security-policy") || "";
      for (const d of ["default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"]) assert.ok(csp.includes(d), `CSP has ${d}: ${csp}`);
      const formAction = /form-action ([^;]*)/.exec(csp)?.[1]?.trim().split(/\s+/) ?? [];
      assert.deepEqual(formAction.sort(), ["'self'", "http://127.0.0.1:9"].sort(), `CSP form-action: ${csp}`);
    });

    test("the page names the user, the app (not verified), where the approval goes and the hub", (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const text = page.text;
      if (me.anonymous) assert.ok(text.includes(me.name), `the guest ${me.name} is named`);
      else assert.ok(text.toLowerCase().includes(String(me.email || me.name).toLowerCase()), `the user ${me.email} is named`);
      assert.ok(text.includes(escapedName), `the client name, HTML-escaped (${escapedName})`);
      assert.ok(!text.includes(name), "the client name is never raw HTML");
      assert.match(text, /not verified/i, "a registered client's name is marked as not verified");
      assert.match(text, /this computer/i, "the loopback warning");
      assert.ok(text.includes("http://127.0.0.1:9"), "the redirect origin");
      assert.ok(text.includes(H), `the hub host ${H}`);
      const today = new Date().toISOString().slice(0, 10);
      const dates = [...text.matchAll(/\b(\d{4}-\d{2}-\d{2})\b/g)].map((m) => m[1]);
      assert.ok(dates.some((d) => d > today), `the expiry date (YYYY-MM-DD) is shown: ${dates}`);
      const logout = text.includes("/__backyard/auth/logout");
      if (oidc || cloudflare) assert.ok(logout, "a sign-out link where a logout exists");
      else if (authMode === "none") assert.ok(!logout, "no sign-out link without a logout");
    });

    test("the form posts the request back with a CSRF token; the buttons are armed after a delay", (t) => {
      if (signInSkip) return t.skip(signInSkip);
      assert.ok(page.text.includes('<form id="consent" method="post" action="/__backyard/auth/authorize">'), "the consent form");
      const keys = ["client_id", "code_challenge", "code_challenge_method", "csrf", "redirect_uri", "resource", "response_type", "scope", "state"];
      assert.deepEqual(Object.keys(form).sort(), keys, `hidden inputs: ${JSON.stringify(form)}`);
      assert.equal(form.response_type, "code");
      assert.equal(form.client_id, client.client_id);
      assert.equal(form.redirect_uri, LOOPBACK);
      assert.equal(form.state, state);
      assert.equal(form.code_challenge, pkcePair.challenge);
      assert.equal(form.code_challenge_method, "S256");
      assert.equal(form.scope, "mcp offline_access");
      assert.equal(form.resource, R);
      assert.match(form.csrf, /^\d+\.[A-Za-z0-9_-]+$/, "CSRF token <exp>.<base64url MAC>");
      assert.ok(form.csrf.length <= 128, "CSRF token ≤ 128 characters");
      const exp = Number(form.csrf.split(".")[0]);
      const now = Date.now() / 1000;
      assert.ok(exp > now + 5 * 60 && exp <= now + 11 * 60, `the CSRF token expires after 10 minutes (exp ${exp})`);
      assert.ok(page.text.includes('<button type="submit" name="decision" value="allow" id="allow">Allow</button>'), "the Allow button");
      assert.ok(page.text.includes('<button type="submit" name="decision" value="deny" id="deny">Cancel</button>'), "the Cancel button");
      assert.doesNotMatch(page.text, /autofocus/i, "no autofocus");
      assert.match(page.text, /visibility:\s*hidden/, "the buttons start hidden");
      assert.match(page.text, /animation:\s*by-arm 0s linear 0\.8s forwards/, "the arming animation");
      assert.match(page.text, /@keyframes by-arm\s*\{\s*to\s*\{\s*visibility:\s*visible;?\s*\}\s*\}/, "the arming keyframes");
    });

    test("consent is shown on every authorization; resource and scope default", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const params = oauth.authorizeParams({ client, challenge: oauth.pkce().challenge, state, resource: undefined, scope: "mcp profile email" });
      for (const site of ["cross-site", "none", "same-origin"]) {
        const again = await oauth.openConsent(browser, P, params, { site });
        assert.equal(again.page.status, 200, `Sec-Fetch-Site ${site}: the consent page again: ${again.page}`);
        assert.equal(again.stoppedAt, null, `Sec-Fetch-Site ${site}: never approved without the page`);
        assert.equal(again.form.resource, R, "no resource means R");
        assert.equal(again.form.scope, "mcp", "unknown scopes are ignored; offline_access only when asked");
      }
    });

    test("a redirect URI on another loopback port is accepted", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const other = "http://127.0.0.1:53682/callback";
      const opened = await oauth.openConsent(browser, P, oauth.authorizeParams({ client, redirectURI: other, challenge: oauth.pkce().challenge, state, resource: R }));
      assert.equal(opened.page.status, 200, `${opened.page}`);
      assert.equal(opened.form.redirect_uri, other);
      const csp = opened.page.header("content-security-policy") || "";
      assert.ok(/form-action [^;]*http:\/\/127\.0\.0\.1:53682/.test(csp), `form-action names the port: ${csp}`);
    });

    test("an approval the page did not send is refused (403) and mints nothing", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const [exp, mac] = form.csrf.split(".");
      const other = await newClient();
      const fields = [
        ["no CSRF token", { ...form, csrf: undefined }],
        ["a tampered CSRF token", { ...form, csrf: `${exp}.${tamper(mac)}` }],
        ["an expired CSRF token", { ...form, csrf: `${Number(exp) - 700}.${mac}` }],
        ["another state", { ...form, state: state + "x" }],
        ["another loopback port", { ...form, redirect_uri: "http://127.0.0.1:10/callback" }],
        ["another code_challenge", { ...form, code_challenge: oauth.pkce().challenge }],
        ["another scope", { ...form, scope: "mcp" }],
        ["another client", { ...form, client_id: other.client_id }],
      ];
      for (const [what, f] of fields) assertPage(await oauth.submitConsent(browser, P, f), 403, what);
      const signals = [
        ["Sec-Fetch-Site: cross-site", { "Sec-Fetch-Site": "cross-site" }],
        ["Sec-Fetch-Site: same-site", { "Sec-Fetch-Site": "same-site" }],
        ["a foreign Origin", { Origin: "https://evil.example" }],
        ["Sec-Fetch-Mode: cors", { "Sec-Fetch-Mode": "cors" }],
        ["Sec-Fetch-Dest: iframe", { "Sec-Fetch-Dest": "iframe" }],
      ];
      for (const [what, headers] of signals) assertPage(await oauth.submitConsent(browser, P, form, { headers }), 403, what);
      // Another person (another guest; with the IdP the second user) cannot use the page.
      const stranger = person(1);
      await signIn(stranger);
      assertPage(await oauth.submitConsent(stranger, P, form), 403, "another person");
      if (oidc) {
        // The Go server binds the token to the session that rendered the page.
        const again = person(0);
        const opened = await oauth.openConsent(again, P, oauth.authorizeParams({ client, challenge: oauth.pkce().challenge, state, resource: R }));
        assert.equal(opened.page.status, 200);
        assertPage(await oauth.submitConsent(again, P, form), 403, "the same user in another session");
      }
      const bad = await oauth.submitConsent(browser, P, form, { decision: "maybe" });
      assertPage(bad, 400, "an unknown decision");
      const notForm = await oauth.submitConsent(browser, P, form, { headers: { "Content-Type": "application/json" } });
      assertPage(notForm, 400, "not a form body");
    });

    test("tampered request fields are redirected like the GET (303)", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      assertErrorRedirect(await oauth.submitConsent(browser, P, { ...form, resource: "https://evil.example/__backyard/mcp" }), {
        status: 303,
        error: "invalid_target",
        state,
        what: "a foreign resource",
      });
      assertErrorRedirect(await oauth.submitConsent(browser, P, { ...form, code_challenge_method: "plain" }), { status: 303, error: "invalid_request", state, what: "plain" });
      assertPage(await oauth.submitConsent(browser, P, { ...form, client_id: tamper(form.client_id) }), 400, "a tampered client_id");
    });

    test("Cancel answers access_denied to the client (303)", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const opened = await oauth.openConsent(browser, P, oauth.authorizeParams({ client, challenge: oauth.pkce().challenge, state, resource: R }));
      const r = await oauth.submitConsent(browser, P, opened.form, { decision: "deny", headers: { Origin: "null" } });
      assertErrorRedirect(r, { status: 303, error: "access_denied", state, what: "Cancel" });
    });

    test("Allow answers 303 to the client with code, state and iss", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const r = await oauth.submitConsent(browser, P, form);
      assert.equal(r.status, 303, `${r}`);
      assert.match(r.header("cache-control") || "", /\bno-store\b/);
      assert.equal(r.header("referrer-policy"), "no-referrer");
      const q = oauth.redirectParams(r.header("location"), LOOPBACK, "Allow");
      assert.match(q.code ?? "", /^byac_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/, `code: ${r.header("location")}`);
      assert.equal(q.state, state);
      assert.equal(q.iss, P);
      assert.ok(!("error" in q));
      const t2 = await oauth.exchangeCode(P, { clientId: client.client_id, code: q.code, verifier: pkcePair.verifier, redirectURI: LOOPBACK });
      oauth.assertTokens(t2, { scope: "mcp offline_access" });
    });
  });

  // ---------------------------------------------------------------------------

  describe("token endpoint (SPEC §8.4.6)", () => {
    let browser;
    let client;
    before(async () => {
      if (signInSkip) return;
      browser = person(0);
      client = await newClient();
    });

    test("a wrong code_verifier fails without using up the code", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const a = await authorize(browser, client);
      for (const [what, verifier] of [
        ["another verifier", oauth.pkce().verifier],
        ["a malformed verifier", "abc"],
      ]) {
        const r = await oauth.exchangeCode(P, { clientId: client.client_id, code: a.code, verifier, redirectURI: LOOPBACK });
        oauth.assertOAuthError(r, 400, "invalid_grant", what);
        oauth.assertNoStore(r, what);
        assertMachine(r, what);
      }
      oauth.assertTokens(await oauth.exchangeCode(P, { clientId: client.client_id, code: a.code, verifier: a.verifier, redirectURI: LOOPBACK }), { scope: "mcp offline_access" });
    });

    test("redirect_uri may be left out; a different one fails without using up the code", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const a = await authorize(browser, client, { scope: "mcp" });
      oauth.assertTokens(await oauth.exchangeCode(P, { clientId: client.client_id, code: a.code, verifier: a.verifier }), { scope: "mcp", what: "without redirect_uri" });
      const b = await authorize(browser, client);
      const wrong = await oauth.exchangeCode(P, { clientId: client.client_id, code: b.code, verifier: b.verifier, redirectURI: "http://127.0.0.1:10/callback" });
      oauth.assertOAuthError(wrong, 400, "invalid_grant", "another redirect_uri");
      oauth.assertTokens(await oauth.exchangeCode(P, { clientId: client.client_id, code: b.code, verifier: b.verifier, redirectURI: LOOPBACK }), { what: "then the right one" });
    });

    test("malformed requests and client authentication", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const a = await authorize(browser, client);
      const other = await newClient();
      const base = { grant_type: "authorization_code", client_id: client.client_id, code: a.code, code_verifier: a.verifier, redirect_uri: LOOPBACK };
      const cases = [
        ["a foreign resource", { form: { ...base, resource: "https://evil.example/__backyard/mcp" } }, 400, ["invalid_target"]],
        ["a JSON body", { raw: JSON.stringify(base), headers: { "Content-Type": "application/json" } }, 400, ["invalid_request"]],
        ["a repeated code", { form: { ...base, code: [a.code, a.code] } }, 400, ["invalid_request"]],
        ["a client_secret", { form: { ...base, client_secret: "x" } }, 401, ["invalid_client"]],
        ["Basic with a password", { form: base, headers: { Authorization: oauth.basicAuth(client.client_id, "x") } }, 401, ["invalid_client"]],
        ["a Bearer Authorization", { form: base, headers: { Authorization: "Bearer x" } }, 401, ["invalid_client"]],
        // Basic is the scheme, one space and padded standard base64 (as Go's Request.BasicAuth).
        ["Basic without base64 padding", { form: base, headers: { Authorization: unpaddedBasic(client.client_id) } }, 401, ["invalid_client"]],
        ["Basic followed by two spaces", { form: base, headers: { Authorization: oauth.basicAuth(client.client_id).replace(" ", "  ") } }, 401, ["invalid_client"]],
        ["Basic followed by a tab", { form: base, headers: { Authorization: oauth.basicAuth(client.client_id).replace(" ", "\t") } }, 401, ["invalid_client"]],
        // A BOM is part of the first parameter's name, so grant_type is missing.
        ["a UTF-8 BOM before the form", { raw: "\ufeff" + new URLSearchParams(base).toString() }, 400, ["invalid_request"]],
        ["Basic naming another client", { form: base, headers: { Authorization: oauth.basicAuth(other.client_id) } }, 400, ["invalid_request"]],
        ["a tampered client_id", { form: { ...base, client_id: tamper(client.client_id) } }, 401, ["invalid_client"]],
        ["a client metadata URL that is not allowlisted", { form: { ...base, client_id: "https://example.com/x" } }, 401, ["invalid_client"]],
        ["no client_id", { form: { ...base, client_id: undefined } }, null, ["invalid_request", "invalid_client"]],
        ["another client's code", { form: { ...base, client_id: other.client_id } }, 400, ["invalid_grant"]],
        ["no code_verifier", { form: { ...base, code_verifier: undefined } }, 400, ["invalid_request", "invalid_grant"]],
        ["an unknown grant_type", { form: { ...base, grant_type: "password" } }, 400, ["unsupported_grant_type"]],
        ["a forged code", { form: { ...base, code: tamper(a.code) } }, 400, ["invalid_grant"]],
      ];
      for (const [what, req, status, codes] of cases) {
        const r = await oauth.tokenRequest(P, req.form, { raw: req.raw, headers: req.headers });
        if (status !== null) assert.equal(r.status, status, `${what}: ${r}`);
        else assert.ok([400, 401].includes(r.status), `${what}: ${r}`);
        assert.ok(codes.includes(r.json.error), `${what}: error ${r.json.error} (${r})`);
        oauth.assertNoStore(r, what);
        if (r.status === 401 && req.headers?.Authorization) {
          assert.equal(r.header("www-authenticate"), 'Basic realm="Backyard"', `${what}: WWW-Authenticate`);
        }
      }
      // Basic with an empty password (and no client_id in the body) is a public client.
      const ok = await oauth.tokenRequest(P, { ...base, client_id: undefined }, { headers: { Authorization: oauth.basicAuth(client.client_id) } });
      oauth.assertTokens(ok, { what: "Basic with an empty password" });
    });

    test("a code is redeemed once: a replay revokes the connection", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const a = await authorize(browser, client);
      const ex = { clientId: client.client_id, code: a.code, verifier: a.verifier, redirectURI: LOOPBACK, resource: R };
      const first = oauth.assertTokens(await oauth.exchangeCode(P, ex), { what: "the first exchange" });
      const mcp = new MCPClient({ endpoint: R, token: first.access_token });
      assert.deepEqual(await mcp.rpc("ping"), {}, "the access token works");
      oauth.assertOAuthError(await oauth.exchangeCode(P, ex), 400, "invalid_grant", "the replay");
      assertInvalidToken(await mcp.post(mcp.request("ping")), "token revoked", "the first access token after the replay");
      oauth.assertOAuthError(await oauth.refresh(P, { clientId: client.client_id, refreshToken: first.refresh_token }), 400, "invalid_grant", "the first refresh token after the replay");
    });
  });

  // ---------------------------------------------------------------------------

  describe("refresh tokens (SPEC §8.4.6, §8.4.8)", () => {
    let browser;
    before(() => {
      if (!signInSkip) browser = person(0);
    });

    test("rotation, the 60 s grace answer and reuse detection", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const c = await connect(browser);
      const id = c.client.client_id;
      const R1 = c.refresh_token;
      const t2 = oauth.assertTokens(await oauth.refresh(P, { clientId: id, refreshToken: R1 }), { scope: "mcp offline_access", what: "R1 → R2" });
      const R2 = t2.refresh_token;
      assert.notEqual(R2, R1, "a new refresh token");
      assert.deepEqual(await new MCPClient({ endpoint: R, token: t2.access_token }).rpc("ping"), {}, "the new access token works");
      const grace = oauth.assertTokens(await oauth.refresh(P, { clientId: id, refreshToken: R1 }), { what: "R1 again at once" });
      assert.equal(grace.refresh_token, R2, "the grace answer is the same successor");
      const t3 = oauth.assertTokens(await oauth.refresh(P, { clientId: id, refreshToken: R2, resource: R }), { what: "R2 → R3" });
      const R3 = t3.refresh_token;
      assert.ok(R3 !== R2 && R3 !== R1, "a third generation");
      oauth.assertOAuthError(await oauth.refresh(P, { clientId: id, refreshToken: R1 }), 400, "invalid_grant", "R1 after R3 exists (reuse)");
      oauth.assertOAuthError(await oauth.refresh(P, { clientId: id, refreshToken: R3 }), 400, "invalid_grant", "R3 after the reuse (the connection is revoked)");
      assertInvalidToken(await new MCPClient({ endpoint: R, token: t3.access_token }).post({ jsonrpc: "2.0", id: 1, method: "ping" }), "token revoked", "the access token after the reuse");
    });

    test("a refresh needs the same client and cannot widen the scope; failures revoke nothing", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const c = await connect(browser, { scope: "mcp" });
      const id = c.client.client_id;
      const other = await newClient();
      for (const [what, req, code] of [
        ["a wider scope", { clientId: id, refreshToken: c.refresh_token, scope: "mcp offline_access" }, "invalid_scope"],
        ["another client", { clientId: other.client_id, refreshToken: c.refresh_token }, "invalid_grant"],
        ["a forged refresh token", { clientId: id, refreshToken: tamper(c.refresh_token) }, "invalid_grant"],
        ["not a refresh token", { clientId: id, refreshToken: c.access_token }, "invalid_grant"],
        ["a foreign resource", { clientId: id, refreshToken: c.refresh_token, resource: "https://evil.example/__backyard/mcp" }, "invalid_target"],
      ]) {
        const r = await oauth.refresh(P, req);
        oauth.assertOAuthError(r, 400, code, what);
        oauth.assertNoStore(r, what);
      }
      const ok = oauth.assertTokens(await oauth.refresh(P, { clientId: id, refreshToken: c.refresh_token, scope: "mcp" }), { scope: "mcp", what: "the refresh still works" });
      const wide = await connect(browser);
      const narrowed = oauth.assertTokens(await oauth.refresh(P, { clientId: wide.client.client_id, refreshToken: wide.refresh_token, scope: "mcp" }), { what: "a narrower scope" });
      assert.equal(narrowed.scope, "mcp");
      assert.ok(ok.refresh_token);
    });
  });

  // ---------------------------------------------------------------------------

  describe("MCP endpoint transport (SPEC §14.1)", () => {
    let conn;
    let mcp;
    before(async () => {
      if (signInSkip) return;
      conn = await connect(person(0));
      mcp = conn.mcp;
    });
    const ping = () => ({ jsonrpc: "2.0", id: 1, method: "ping" });

    test("Origin: the hub's and Claude's are accepted, others refused before authentication", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const forbidden = { error: { code: "forbidden", message: "requests from this origin are not accepted" } };
      for (const [what, token] of [
        ["with a token", undefined],
        ["without a token", null],
      ]) {
        const r = await mcp.post(ping(), { token, headers: { Origin: "https://evil.example" }, what });
        assert.equal(r.status, 403, `${what}: ${r}`);
        assertBody(r, forbidden, what);
      }
      const ok = [new URL(P).origin, "https://claude.ai", "HTTPS://CLAUDE.AI"];
      if (required) ok.push("https://claude.com");
      for (const origin of ok) {
        const r = await mcp.post(ping(), { headers: { Origin: origin } });
        assert.equal(r.status, 200, `Origin ${origin}: ${r}`);
      }
    });

    test("methods other than POST: 405 with Allow: POST, after authentication", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      for (const method of ["GET", "DELETE", "PUT", "PATCH", "OPTIONS", "HEAD"]) {
        const r = await mcp.post(ping(), { method });
        assert.equal(r.status, 405, `${method}: ${r}`);
        assert.equal(r.header("allow"), "POST", `${method}: Allow`);
      }
      const r = await mcp.post(null, { method: "GET", token: null, headers: { Accept: "text/event-stream" } });
      assert.equal(r.status, 401, `GET without a token: ${r}`);
      assert.equal(r.header("www-authenticate"), CHALLENGE_MISSING);
    });

    test("content type, Accept and the protocol version header", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const r415 = await mcp.post(ping(), { headers: { "Content-Type": "text/plain" } });
      assert.equal(r415.status, 415, `text/plain: ${r415}`);
      assert.equal(r415.json.error.code, "bad_request");
      const accept = await mcp.post(ping(), { headers: { Accept: "text/html" } });
      assert.equal(accept.status, 400, `Accept: text/html: ${accept}`);
      assert.equal(accept.json.error.code, "bad_request");
      for (const headers of [
        { "Content-Type": "application/json; charset=utf-8" },
        { Accept: "*/*" },
        { Accept: "application/*" },
        { Accept: "application/json" },
        { Accept: null },
        { "MCP-Protocol-Version": null },
        { "MCP-Protocol-Version": "2025-06-18" },
        { "Mcp-Session-Id": "ignored", "Last-Event-ID": "1" },
      ]) {
        const r = await mcp.post(ping(), { headers });
        assert.equal(r.status, 200, `${JSON.stringify(headers)}: ${r}`);
      }
      for (const v of ["2026-07-28", "2025-03-26", "2024-11-05"]) {
        const r = await mcp.post(ping(), { headers: { "MCP-Protocol-Version": v } });
        assert.equal(r.status, 400, `MCP-Protocol-Version ${v}: ${r}`);
        assertBody(
          r,
          { jsonrpc: "2.0", id: null, error: { code: -32022, message: "Unsupported protocol version", data: { supported: PROTOCOL_VERSIONS, requested: v } } },
          `MCP-Protocol-Version ${v}`,
        );
      }
    });

    test("request bodies are limited (413 by Content-Length)", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const who = await mcp.callJSON("backyard_whoami", {});
      const limit = 2 * who.limits.max_publish_bytes + 1024 * 1024;
      const headers = { Authorization: `Bearer ${conn.access_token}`, "MCP-Protocol-Version": "2025-11-25" };
      const big = await postSized(R, limit + 1, headers);
      assert.equal(big.status, 413, `limit + 1 bytes: HTTP ${big.status} ${big.body.toString().slice(0, 200)}`);
      const err = JSON.parse(big.body.toString()).error;
      assert.equal(err.code, "too_large");
      assert.equal(err.message, `MCP requests are limited to ${limit} bytes; publish over several calls`);
      assert.equal(big.headers["mcp-session-id"], undefined);
      assert.equal(big.headers["set-cookie"], undefined);
      const atLimit = await postSized(R, limit, headers);
      assert.equal(atLimit.status, 200, `exactly the limit: HTTP ${atLimit.status} ${atLimit.body.toString().slice(0, 200)}`);
    });

    test("JSON-RPC framing: parse errors, batches and invalid requests", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const parse = await mcp.post("{");
      assert.equal(parse.status, 400, `${parse}`);
      assertBody(parse, { jsonrpc: "2.0", id: null, error: { code: -32700, message: "Parse error" } }, "invalid JSON");
      for (const batch of [[], [ping()]]) {
        const r = await mcp.post(batch);
        assertRPCError(r, { status: 400, code: -32600, id: null, message: "Invalid Request: JSON-RPC batches are not supported", what: "a batch" });
      }
      for (const [what, body] of [
        ["a string", JSON.stringify("x")],
        ["a number", "5"],
        ["jsonrpc 1.0", { jsonrpc: "1.0", id: 1, method: "ping" }],
        ["no jsonrpc", { id: 1, method: "ping" }],
        ["a null id", { jsonrpc: "2.0", id: null, method: "ping" }],
        ["a fractional id", { jsonrpc: "2.0", id: 1.5, method: "ping" }],
        ["an object id", { jsonrpc: "2.0", id: {}, method: "ping" }],
        ["a method that is not a string", { jsonrpc: "2.0", id: 1, method: 5 }],
      ]) {
        assertRPCError(await mcp.post(body), { status: 400, code: -32600, id: null, message: "Invalid Request", what });
      }
    });

    test("notifications and client responses: 202 with an empty body", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      for (const body of [
        { jsonrpc: "2.0", method: "notifications/initialized" },
        { jsonrpc: "2.0", method: "notifications/cancelled", params: { requestId: 1 } },
        { jsonrpc: "2.0", id: 7, result: {} },
        { jsonrpc: "2.0", id: 8, error: { code: 1, message: "x" } },
      ]) {
        const r = await mcp.post(body);
        assert.equal(r.status, 202, `${JSON.stringify(body)}: ${r}`);
        assert.equal(r.body.length, 0, "an empty body");
      }
    });

    test("initialize negotiates the protocol version", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      for (const [asked, want] of [
        ["2025-06-18", "2025-06-18"],
        ["2025-11-25", "2025-11-25"],
        ["2024-11-05", "2025-11-25"],
        ["2026-07-28", "2025-11-25"],
      ]) {
        const res = await mcp.initialize(asked);
        assert.equal(res.protocolVersion, want, `asked ${asked}`);
        assert.deepEqual(Object.keys(res).sort(), ["capabilities", "instructions", "protocolVersion", "serverInfo"]);
        assert.deepEqual(res.capabilities, { tools: {} });
        assert.equal(res.serverInfo.name, "backyard");
        assert.equal(res.serverInfo.title, "Backyard");
        assert.equal(typeof res.serverInfo.version, "string");
        if (catalog) assert.equal(res.instructions, catalog.instructions, "the catalog's instructions");
      }
      // The member name is exact: no case-insensitive match.
      for (const params of [{}, { protocolVersion: 20250618 }, undefined, { ProtocolVersion: "2025-06-18" }]) {
        const r = await mcp.post(mcp.request("initialize", params));
        assertRPCError(r, { code: -32602, message: "Invalid params", what: `initialize ${JSON.stringify(params)}` });
      }
    });

    test("ping, string ids, and no initialize needed", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const r = await mcp.post({ jsonrpc: "2.0", id: "abc", method: "ping" });
      assert.equal(r.status, 200);
      assertBody(r, { jsonrpc: "2.0", id: "abc", result: {} }, "ping");
      const fresh = new MCPClient({ endpoint: R, token: conn.access_token, version: null });
      assert.ok(Array.isArray((await fresh.rpc("tools/list")).tools), "tools/list without initialize and without the version header");
    });

    test("tools/list returns the catalog verbatim", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      for (const params of [undefined, { cursor: "x" }]) {
        const res = await mcp.rpc("tools/list", params);
        assert.deepEqual(Object.keys(res), ["tools"], "no nextCursor");
        if (catalog) assert.deepEqual(res.tools, catalog.tools, "internal/mcpremote/tools.json");
        assert.deepEqual(
          res.tools.map((x) => x.name),
          ["backyard_docs", "backyard_whoami", "backyard_list_sites", "backyard_site_files", "backyard_read_site_file", "backyard_publish"],
        );
      }
    });

    test("unknown methods, unknown tools and invalid arguments", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      for (const method of ["resources/list", "prompts/list", "logging/setLevel", "completion/complete", "server/discover", "nope"]) {
        assertRPCError(await mcp.post(mcp.request(method, {})), { code: -32601, message: "Method not found", what: method });
      }
      assertRPCError(await mcp.post(mcp.request("tools/call", { name: "nope", arguments: {} })), { code: -32602, message: "Unknown tool: nope" });
      for (const params of [{}, { name: 5 }, { name: "backyard_whoami", arguments: [1] }, undefined]) {
        assertRPCError(await mcp.post(mcp.request("tools/call", params)), { code: -32602, what: `tools/call ${JSON.stringify(params)}` });
      }
      for (const [name, args] of [
        ["backyard_docs", { topic: "nope" }],
        ["backyard_whoami", { extra: 1 }],
        ["backyard_site_files", {}],
        ["backyard_site_files", { site: "Not_A_Name" }],
        ["backyard_publish", { site: "x", files: [{ path: "a.txt" }] }],
      ]) {
        const msg = await mcp.callError(name, args);
        assert.match(msg, /^Invalid arguments: /, `${name} ${JSON.stringify(args)}: ${msg}`);
      }
      // Absent arguments are {}.
      assert.equal(typeof (await mcp.callJSON("backyard_whoami")).hub_url, "string");
    });
  });

  // ---------------------------------------------------------------------------

  describe("connector tools (SPEC §14.1)", () => {
    let browser;
    let conn;
    let mcp;
    let who;
    const site = uniqueSite("mcp");
    const site2 = uniqueSite("mcp2");
    const site3 = uniqueSite("mcp3");
    const index = `<!doctype html><html><head><title>MCP ${site}</title><meta name="description" content="from the connector"></head><body>héllo ${site}</body></html>\n`;
    const png = Buffer.from("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8ffff3f0005fe02fea7d2a5b20000000049454e44ae426082", "hex");
    const files = () => [
      { path: "index.html", content: index },
      { path: "logo.png", content: png.toString("base64"), encoding: "base64" },
    ];

    before(async () => {
      if (signInSkip) return;
      browser = person(0);
      conn = await connect(browser);
      mcp = conn.mcp;
      who = await mcp.callJSON("backyard_whoami", {});
    });

    after(async () => {
      const cleaner = new Client();
      for (const s of [site, site2, site3]) await deleteSiteQuietly(cleaner, env, s);
    });

    /** GET of a site file with the person's browser. */
    const getFile = (path) => browser.request(env.siteURL(site) + path);

    test("backyard_whoami describes the connection", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      assert.deepEqual(Object.keys(who).sort(), ["can_publish", "connection", "hub_url", "limits", "site_url_template", "user"]);
      const me = await signIn(browser);
      assert.equal(who.user.id, me.id);
      assert.equal(who.user.name, me.name);
      assert.equal(who.user.anonymous, authMode === "none", "anonymous exactly in mode none");
      assert.equal(typeof who.user.admin, "boolean");
      if (me.email) assert.equal(who.user.email, me.email);
      else assert.ok(!("email" in who.user), "no empty email");
      assert.equal(who.hub_url, P);
      assert.ok(who.site_url_template.includes("{site}"), who.site_url_template);
      assert.equal(who.site_url_template.replace("{site}", site), env.canonicalSiteURL(site));
      assert.equal(who.can_publish, true, "the runners allow publishing (anonymous deploys or a signed-in user)");
      const l = who.limits;
      assert.deepEqual(Object.keys(l).sort(), ["max_file_bytes", "max_files", "max_publish_bytes", "max_read_bytes", "max_site_bytes"]);
      for (const k of Object.keys(l)) assert.ok(Number.isInteger(l[k]) && l[k] > 0, `limits.${k}`);
      assert.equal(l.max_read_bytes, 131072);
      if (env.info.limits) {
        assert.equal(l.max_files, env.info.limits.max_files);
        assert.equal(l.max_site_bytes, env.info.limits.max_site_bytes);
        assert.equal(l.max_file_bytes, env.info.limits.max_file_bytes);
      }
      assert.equal(who.connection.client, conn.client.client_name);
      assert.match(who.connection.expires_at, RFC3339_UTC);
      assert.ok(Date.parse(who.connection.expires_at) > Date.now() + 3600_000, "the connection outlives the access token");
    });

    test("backyard_publish creates a site that serves the exact bytes", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const res = await mcp.callJSON("backyard_publish", { site, files: files() });
      const bytes = Buffer.byteLength(index) + png.length;
      assert.deepEqual(res, { site, url: env.canonicalSiteURL(site), created: true, files: 2, uploaded: 2, unchanged: 0, deleted: 0, bytes, warnings: [] });
      const page = await getFile("");
      assert.equal(page.status, 200, `${page}`);
      assert.match(page.header("content-type") || "", /^text\/html\b/);
      assert.deepEqual(page.body, Buffer.from(index), "index.html bytes");
      const logo = await getFile("logo.png");
      assert.equal(logo.status, 200, `${logo}`);
      assert.match(logo.header("content-type") || "", /^image\/png\b/);
      assert.deepEqual(logo.body, png, "logo.png bytes");
    });

    test("a lone surrogate escape in a path reads as U+FFFD (as encoding/json decodes it)", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const msg = mcp.request("tools/call", {
        name: "backyard_publish",
        arguments: { site: site3, files: [{ path: "index.html", content: "x" }, { path: "aSURR.html", content: "y" }] },
      });
      const r = await mcp.post(JSON.stringify(msg).replace("SURR", "\\ud800"));
      assert.equal(r.status, 200, `${r}`);
      assert.ok(!r.json.result.isError, `published: ${r}`);
      assert.equal(JSON.parse(r.json.result.content[0].text).files, 2, `${r}`);
      assert.equal(await mcp.callText("backyard_read_site_file", { site: site3, path: "a\ufffd.html" }), "y");
    });

    test("the read tools: list_sites, site_files, read_site_file and docs", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const list = await mcp.callJSON("backyard_list_sites", { query: site.toUpperCase() });
      assert.deepEqual(Object.keys(list).sort(), ["sites", "total", "truncated"]);
      const meta = list.sites.find((s) => s.name === site);
      assert.ok(meta, `list_sites finds ${site}: ${JSON.stringify(list).slice(0, 400)}`);
      assert.equal(meta.url, env.canonicalSiteURL(site));
      assert.equal(meta.title, `MCP ${site}`);
      assert.equal(meta.description, "from the connector");
      assert.equal(meta.file_count, 2);
      assert.equal(meta.bytes, Buffer.byteLength(index) + png.length);
      assert.ok(list.total >= 1 && list.truncated === false);

      const tree = await mcp.callJSON("backyard_site_files", { site });
      assert.deepEqual(tree, {
        site,
        url: env.canonicalSiteURL(site),
        files: [
          { path: "index.html", size: Buffer.byteLength(index) },
          { path: "logo.png", size: png.length },
        ],
        total: 2,
        truncated: false,
      });

      assert.equal(await mcp.callText("backyard_read_site_file", { site, path: "index.html" }), index);
      assert.match(await mcp.callError("backyard_read_site_file", { site, path: "logo.png" }), /binary/);
      await mcp.callError("backyard_read_site_file", { site, path: "missing.html" });
      await mcp.callError("backyard_read_site_file", { site, path: "../x" });
      await mcp.callError("backyard_read_site_file", { site: uniqueSite("none"), path: "index.html" });
      await mcp.callError("backyard_site_files", { site: uniqueSite("none") });

      assert.match(await mcp.callText("backyard_docs", { topic: "api" }), /backyard\.db/);
      for (const args of [{}, { topic: "skill" }, { topic: "recipes" }, { topic: "deploy" }]) {
        const doc = await mcp.callText("backyard_docs", args);
        assert.ok(doc.length > 200, `backyard_docs ${JSON.stringify(args)}`);
      }
    });

    test("publishing the same files again changes nothing", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const res = await mcp.callJSON("backyard_publish", { site, files: files() });
      assert.equal(res.created, false);
      assert.equal(res.files, 2);
      assert.equal(res.uploaded, 0);
      assert.equal(res.unchanged, 2);
      assert.equal(res.deleted, 0);
    });

    test("merge keeps unlisted files; delete removes; replace makes the site exactly the files", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const add = await mcp.callJSON("backyard_publish", { site, files: [{ path: "js/app.js", content: "console.log(1);\n" }] });
      assert.equal(add.files, 3, JSON.stringify(add));
      assert.equal(add.uploaded, 1);
      assert.equal(add.unchanged, 0);
      assert.equal((await getFile("js/app.js")).text, "console.log(1);\n");
      assert.deepEqual((await getFile("logo.png")).body, png, "unlisted files are kept");

      // Both base64 alphabets, padding optional.
      const raw = Buffer.from([0xfb, 0xff, 0xfe, 0x3e, 0x3f, 0x00]);
      const b64 = await mcp.callJSON("backyard_publish", {
        site,
        files: [
          { path: "std.bin", content: raw.toString("base64"), encoding: "base64" },
          { path: "url.bin", content: raw.toString("base64url"), encoding: "base64" },
        ],
      });
      assert.equal(b64.files, 5, JSON.stringify(b64));
      assert.deepEqual((await getFile("std.bin")).body, raw, "standard base64");
      assert.deepEqual((await getFile("url.bin")).body, raw, "URL-safe base64 without padding");

      const del = await mcp.callJSON("backyard_publish", { site, files: [], delete: ["logo.png", "std.bin", "url.bin", "nope.txt"] });
      assert.equal(del.files, 2, JSON.stringify(del));
      assert.equal(del.deleted, 3);
      assert.ok(del.warnings.includes('"nope.txt" in delete does not exist.'), `the warning for a missing path: ${JSON.stringify(del.warnings)}`);
      assert.equal((await getFile("logo.png")).status, 404, "logo.png was deleted");

      const rep = await mcp.callJSON("backyard_publish", { site, files: [{ path: "index.html", content: index }], replace: true });
      assert.equal(rep.files, 1, JSON.stringify(rep));
      assert.equal(rep.deleted, 1);
      assert.equal(rep.unchanged, 1);
      assert.equal((await getFile("js/app.js")).status, 404, "replace removed js/app.js");
      assert.deepEqual((await mcp.callJSON("backyard_site_files", { site })).files.map((f) => f.path), ["index.html"]);
    });

    test("warnings: no index.html, and binary content sent as text", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const res = await mcp.callJSON("backyard_publish", {
        site: site2,
        files: [
          { path: "about.html", content: "<p>about</p>" },
          { path: "photo.png", content: "not really a png" },
        ],
      });
      assert.equal(res.created, true);
      assert.ok(res.warnings.includes(`No index.html at the site root: ${env.canonicalSiteURL(site2)} answers 404 until one is published.`), JSON.stringify(res.warnings));
      assert.ok(res.warnings.includes('photo.png was sent as text but looks binary; binary files need "encoding": "base64".'), JSON.stringify(res.warnings));
    });

    test("invalid publishes are refused and change nothing", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const one = (path, content = "x") => [{ path, content }];
      const half = Math.floor(who.limits.max_publish_bytes / 2) + 1;
      const cases = [
        ["a reserved name", { site: "api", files: one("index.html") }, /reserved/],
        ["a path that leaves the site", { site, files: one("../x") }, null],
        ["an absolute path", { site, files: one("/x.html") }, null],
        ["a reserved path", { site, files: one("__backyard/x.js") }, null],
        ["repository metadata", { site, files: one(".git/config") }, null],
        ["invalid base64", { site, files: [{ path: "a.bin", content: "!!!!", encoding: "base64" }] }, /base64/],
        ["base64 with whitespace", { site, files: [{ path: "a.bin", content: "aGVs bG8=", encoding: "base64" }] }, /base64/],
        ["over the per-call limit", { site, files: [{ path: "a.txt", content: "a".repeat(half) }, { path: "b.txt", content: "b".repeat(half) }] }, /per call/],
        ["nothing to publish", { site, files: [] }, null],
        ["replace and delete", { site, files: one("index.html"), delete: ["a"], replace: true }, null],
        ["a duplicate path", { site, files: [...one("a.txt"), ...one("a.txt")] }, null],
        ["a path in files and delete", { site, files: one("a.txt"), delete: ["a.txt"] }, null],
        ["a content type with a control character", { site, files: [{ path: "a.txt", content: "x", content_type: "text/plain\u0001" }] }, null],
        ["deleting every file", { site, files: [], delete: ["index.html"] }, null],
        ["a file and a folder at once", { site, files: [...one("index.html/x.html")] }, null],
      ];
      for (const [what, args, fragment] of cases) {
        const msg = await mcp.callError("backyard_publish", args);
        if (fragment) assert.match(msg, fragment, `${what}: ${msg}`);
        for (const secretish of ["byat_", "byrt_", "byac_", "bydc_"]) assert.ok(!msg.includes(secretish), `${what}: no token in the error`);
      }
      assert.deepEqual((await mcp.callJSON("backyard_site_files", { site })).files.map((f) => f.path), ["index.html"], "nothing changed");
    });

    test("a site someone else published last needs overwrite: true", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const otherBrowser = person(1);
      const other = await connect(otherBrowser);
      const changed = [{ path: "index.html", content: "<!doctype html><title>taken over</title>\n" }];
      const msg = await other.mcp.callError("backyard_publish", { site, files: changed });
      assert.match(msg, /overwrite/, msg);
      assert.equal(await mcp.callText("backyard_read_site_file", { site, path: "index.html" }), index, "nothing was changed");
      const res = await other.mcp.callJSON("backyard_publish", { site, files: changed, overwrite: true });
      assert.equal(res.created, false);
      assert.equal(res.uploaded, 1);
      assert.equal((await getFile("")).text, changed[0].content);
      const meta = (await mcp.callJSON("backyard_list_sites", { query: site })).sites.find((s) => s.name === site);
      const otherMe = await signIn(otherBrowser);
      assert.ok([otherMe.id, otherMe.email, otherMe.name].includes(meta.updated_by), `updated_by ${meta.updated_by} names the other person`);
    });
  });

  // ---------------------------------------------------------------------------

  describe("credential isolation (SPEC §8, §14.1)", () => {
    test("an MCP access token is no API credential", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const c = await connect(person(0));
      const api = new Client({ token: c.access_token });
      for (const path of ["/__backyard/api/me", "/__backyard/api/deploy/me", "/__backyard/api/mcp/grants"]) {
        const r = await api.get(P + path);
        assert.equal(r.status, 401, `${path} with an MCP access token: ${r}`);
      }
      assert.deepEqual(await c.mcp.rpc("ping"), {}, "the token still works at /__backyard/mcp");
    });

    test("API tokens, static tokens and DEPLOY_TOKEN are no MCP tokens", async (t) => {
      const tokens = [];
      if (envToken) tokens.push(["BACKYARD_TOKEN", envToken]);
      if (secret) tokens.push(["a Backyard API token", signAPIToken(secret, { id: "conformance@example.com", email: "conformance@example.com", name: "Conformance" })]);
      if (!tokens.length) return t.skip("no BACKYARD_TOKEN and no BACKYARD_CONFORMANCE_SECRET");
      const mcp = new MCPClient({ endpoint: R });
      for (const [what, tok] of tokens) {
        assertInvalidToken(await mcp.post({ jsonrpc: "2.0", id: 1, method: "ping" }, { token: tok, what }), "malformed token", what);
      }
    });
  });

  // ---------------------------------------------------------------------------

  describe("revocation and the grants API (SPEC §8.4.7)", () => {
    let browser;
    let me;
    before(async () => {
      if (signInSkip) return;
      browser = person(0);
      me = await signIn(browser);
    });
    const grantsURL = `${P}/__backyard/api/mcp/grants`;
    const listGrants = async (b = browser, query = "") => {
      const r = await b.request(grantsURL + query);
      assert.equal(r.status, 200, `GET grants: ${r}`);
      return r.json.grants;
    };
    const csrf = { "X-Backyard": "1" };

    test("the hub lists the person's connections", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const c = await connect(browser);
      const grants = await listGrants();
      const g = grants.find((x) => x.client_name === c.client.client_name);
      assert.ok(g, `the connection is listed: ${JSON.stringify(grants).slice(0, 400)}`);
      assert.deepEqual(Object.keys(g).sort(), ["client_host", "client_kind", "client_name", "created_at", "expires_at", "id", "redirect_host", "scope", "user"]);
      assert.match(g.id, /^[0-9a-f]{32}$/);
      assert.equal(g.user.id, me.id);
      assert.equal(g.client_kind, "registered");
      assert.equal(g.client_host, "");
      assert.ok(["127.0.0.1", "127.0.0.1:9"].includes(g.redirect_host), `redirect_host ${g.redirect_host}`);
      assert.equal(g.scope, "mcp offline_access");
      assert.match(g.created_at, RFC3339_UTC);
      assert.match(g.expires_at, RFC3339_UTC);
      assert.ok(Date.parse(g.expires_at) > Date.parse(g.created_at));
      assert.ok(c.consent.text.includes(g.expires_at.slice(0, 10)), `the consent page showed the expiry date ${g.expires_at.slice(0, 10)}`);
      for (const x of grants) assert.equal(x.user.id, me.id, "only the caller's own grants");
      const times = grants.map((x) => Date.parse(x.created_at));
      assert.deepEqual(times, [...times].sort((a, b) => b - a), "newest first");
      const stranger = await browser.request(`${grantsURL}?user=${encodeURIComponent("someone-else@example.com")}`);
      assert.equal(stranger.status, 403, `another person's grants need an admin: ${stranger}`);
      const them = person(1);
      await signIn(them);
      const theirs = await listGrants(them);
      assert.ok(!theirs.some((x) => x.id === g.id), "another person does not see the grant");
    });

    test("Disconnect on the hub revokes the connection at once", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const c = await connect(browser);
      const g = (await listGrants()).find((x) => x.client_name === c.client.client_name);
      const one = `${grantsURL}/${g.id}`;
      const noCSRF = await browser.request(one, { method: "DELETE" });
      assert.equal(noCSRF.status, 403, `DELETE without X-Backyard: ${noCSRF}`);
      const other = person(1);
      await signIn(other);
      const foreign = await other.request(one, { method: "DELETE", headers: csrf });
      assert.equal(foreign.status, 404, `another person's DELETE: ${foreign}`);
      assert.deepEqual(await c.mcp.rpc("ping"), {}, "the connection still works");
      const del = await browser.request(one, { method: "DELETE", headers: csrf });
      assert.equal(del.status, 204, `DELETE: ${del}`);
      assertInvalidToken(await c.mcp.post({ jsonrpc: "2.0", id: 1, method: "ping" }), "token revoked", "the access token after Disconnect");
      oauth.assertOAuthError(await oauth.refresh(P, { clientId: c.client.client_id, refreshToken: c.refresh_token }), 400, "invalid_grant", "the refresh token after Disconnect");
      assert.ok(!(await listGrants()).some((x) => x.id === g.id), "no longer listed");
      const again = await browser.request(one, { method: "DELETE", headers: csrf });
      assert.equal(again.status, 404, `DELETE again: ${again}`);
    });

    test("RFC 7009 revocation: 200 for everything, and only the client's own tokens count", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const a = await connect(browser);
      const r1 = await oauth.revoke(P, { clientId: a.client.client_id, token: a.refresh_token, hint: "refresh_token" });
      assert.equal(r1.status, 200, `${r1}`);
      assert.equal(r1.body.length, 0, "an empty body");
      assertMachine(r1, "revoke");
      oauth.assertOAuthError(await oauth.refresh(P, { clientId: a.client.client_id, refreshToken: a.refresh_token }), 400, "invalid_grant", "after revoking the refresh token");
      assertInvalidToken(await a.mcp.post({ jsonrpc: "2.0", id: 1, method: "ping" }), "token revoked", "the access token of a revoked refresh token");

      const b = await connect(browser);
      assert.equal((await oauth.revoke(P, { clientId: b.client.client_id, token: b.access_token })).status, 200);
      assertInvalidToken(await b.mcp.post({ jsonrpc: "2.0", id: 1, method: "ping" }), "token revoked", "after revoking the access token");

      const c = await connect(browser);
      const intruder = await newClient();
      for (const [what, req] of [
        ["another client's token", { clientId: intruder.client_id, token: c.refresh_token }],
        ["an unknown token", { clientId: c.client.client_id, token: "byrt_eyJnIjoiMDAiLCJuIjoxfQ.AAAA" }],
        ["garbage", { clientId: c.client.client_id, token: "garbage" }],
      ]) {
        const r = await oauth.revoke(P, req);
        assert.equal(r.status, 200, `${what}: ${r}`);
        assert.equal(r.body.length, 0, `${what}: an empty body`);
      }
      oauth.assertTokens(await oauth.refresh(P, { clientId: c.client.client_id, refreshToken: c.refresh_token }), { what: "the token another client tried to revoke still works" });
      oauth.assertOAuthError(await oauth.revoke(P, { clientId: c.client.client_id }), 400, "invalid_request", "no token");
      oauth.assertOAuthError(await oauth.oauthPost(P, "revoke", { token: "x", client_id: c.client.client_id, client_secret: "s" }), 401, "invalid_client", "a client_secret");
    });

    test("disconnecting all of a person's connections", async (t) => {
      if (signInSkip) return t.skip(signInSkip);
      const own = person(0);
      const a = await connect(own);
      const b = await connect(own);
      const r = await own.request(grantsURL, { method: "DELETE", headers: csrf });
      assert.equal(r.status, 200, `DELETE all: ${r}`);
      assert.ok(Number.isInteger(r.json.revoked) && r.json.revoked >= 2, `revoked ${r.json.revoked}`);
      for (const c of [a, b]) assertInvalidToken(await c.mcp.post({ jsonrpc: "2.0", id: 1, method: "ping" }), "token revoked", "after disconnecting all");
      assert.deepEqual(await listGrants(own), []);
      const foreign = await own.request(`${grantsURL}?user=${encodeURIComponent("someone-else@example.com")}`, { method: "DELETE", headers: csrf });
      assert.equal(foreign.status, 403, `another person's connections need an admin: ${foreign}`);
    });
  });

}
