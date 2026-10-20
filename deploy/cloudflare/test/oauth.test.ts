/**
 * The Claude connector's authorization server (SPEC §8.4): enablement and
 * routing, discovery, registration, the redirect and resource rules, client
 * metadata documents, authorize and consent, the token and revocation
 * endpoints, rotation, access-token verification and the grants API — in
 * mode none (guests) and behind Cloudflare Access.
 */
import { runInDurableObject } from "cloudflare:test";
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetAccessCaches } from "../src/auth";
import { getConfig } from "../src/config";
import { KeyLabel, b64url, deriveKey, hex, signValue } from "../src/crypto";
import type { Env } from "../src/env";
import { issueAPIToken } from "../src/jwt";
import {
  canonicalResource,
  cimdCacheTTL,
  mcpAllowedOrigin,
  mcpChallenge,
  mcpEnabled,
  mcpIssuer,
  mcpResource,
  mcpResourceMetadataURL,
  parseLoopbackURI,
  redirectMatches,
  resetMcpCaches,
  verifyMcpAccessToken,
  type McpGrantRecord,
} from "../src/oauth";
import { route, type RequestContext } from "../src/router";
import { registry } from "../src/stubs";
import {
  DEPLOY_TOKEN,
  LOOPBACK,
  MCP_ENV,
  MCP_RESOURCE,
  ORIGIN,
  SECRET,
  apiHeaders,
  authorizeQuery,
  call,
  hiddenInputs,
  mcpConnect,
  openConsent,
  pkcePair,
  redirectParams,
  registerClient,
  submitConsent,
  testEnv,
  tokenRequest,
} from "./helpers";

const M = ORIGIN + "/.well-known/oauth-protected-resource/__backyard/mcp";
const NOT_ENABLED = { error: { code: "not_found", message: "the Claude connector is not enabled on this server" } };
const CIMD_URL = "https://client.example/oauth/metadata";
const CIMD_ENV: Partial<Env> = { MCP_CLIENTS: CIMD_URL };

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
  resetMcpCaches();
  resetAccessCaches();
});

/** A request context for direct calls (verifyMcpAccessToken, the URL helpers). */
function ctxFor(env: Partial<Env> = {}, url = ORIGIN + "/__backyard/mcp"): RequestContext {
  const e = testEnv({ ...MCP_ENV, ...env });
  const r = route(new Request(url, { method: "POST" }), e, getConfig(e), () => {});
  if (!("ctx" in r)) throw new Error("not routed");
  return r.ctx;
}

const verify = (token: string, env: Partial<Env> = {}) => verifyMcpAccessToken(ctxFor(env), token);

/** The JSON payload of a prefixed signed token. */
function payloadOf(token: string): Record<string, unknown> {
  const p = token.slice(5).split(".")[0]!;
  return JSON.parse(atob(p.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((p.length + 3) % 4))) as Record<string, unknown>;
}

/** s with one character of its MAC (the part after the last ".") changed, away from the padding bits. */
function tamper(s: string): string {
  const i = s.lastIndexOf(".") + 6;
  return s.slice(0, i) + (s[i] === "A" ? "B" : "A") + s.slice(i + 1);
}

/** Basic for a public client with the base64 padding stripped (leading characters %-escaped until there is padding). */
function unpaddedBasic(clientId: string): string {
  for (let k = 0; k < 3; k++) {
    const user = [...clientId.slice(0, k)].map((c) => "%" + c.charCodeAt(0).toString(16).padStart(2, "0")).join("") + clientId.slice(k);
    const b64 = btoa(user + ":");
    if (b64.endsWith("=")) return "Basic " + b64.replace(/=+$/, "");
  }
  throw new Error("unreachable");
}

/** Signs a token like the server does (the tests know the secret). */
async function forge(prefix: string, label: string, payload: unknown): Promise<string> {
  return prefix + (await signValue((await deriveKey(SECRET, label)).hmac, JSON.stringify(payload)));
}

/** The user /me reports for a browser. */
async function whoami(identity: Record<string, string>, env: Partial<Env> = {}): Promise<{ id: string; name: string; email?: string }> {
  const r = await call("/__backyard/api/me", { headers: identity }, { ...MCP_ENV, ...env });
  return (await r.json()) as { id: string; name: string; email?: string };
}

/** Asserts an error page (HTML, never a redirect). */
async function expectPage(r: Response, status: number, what: string): Promise<string> {
  expect(r.status, what).toBe(status);
  expect(r.headers.get("Location"), what).toBeNull();
  expect(r.headers.get("Content-Type") ?? "", what).toMatch(/^text\/html/);
  const html = await r.text();
  expect(html, what).not.toMatch(/byac_/);
  return html;
}

/** Asserts an error redirect to the loopback client. */
function expectErrorRedirect(r: Response, status: number, error: string, state: string | undefined, what: string): Record<string, string> {
  expect(r.status, what).toBe(status);
  const loc = r.headers.get("Location") ?? "";
  expect(loc.startsWith(LOOPBACK), `${what}: ${loc}`).toBe(true);
  const q = redirectParams(r);
  expect(q.error, what).toBe(error);
  expect(q.iss, what).toBe(ORIGIN);
  expect(q.state, what).toBe(state);
  expect(q.code, what).toBeUndefined();
  expect(r.headers.get("Cache-Control") ?? "", what).toContain("no-store");
  expect(r.headers.get("Referrer-Policy"), what).toBe("no-referrer");
  return q;
}

async function expectOAuthError(r: Response, status: number, error: string, what: string): Promise<{ error: string; error_description: string }> {
  expect(r.status, what).toBe(status);
  expect(r.headers.get("Content-Type"), what).toBe("application/json");
  expect(r.headers.get("Cache-Control"), what).toBe("no-store");
  expect(r.headers.get("Pragma"), what).toBe("no-cache");
  const b = (await r.json()) as { error: string; error_description: string };
  expect(b.error, `${what}: ${JSON.stringify(b)}`).toBe(error);
  expect(typeof b.error_description, what).toBe("string");
  return b;
}

/** A fresh guest browser (mode none). */
async function guest(env: Partial<Env> = {}): Promise<Record<string, string>> {
  const r = await call("/__backyard/api/me", {}, { ...MCP_ENV, ...env });
  const m = /backyard_anon=([^;]+)/.exec(r.headers.get("Set-Cookie") ?? "");
  return { Cookie: `backyard_anon=${m![1]}` };
}

/** An authorization code (and its verifier) for a new consent of a browser. */
async function authorize(opts: { clientId?: string; identity?: Record<string, string>; scope?: string; env?: Partial<Env> } = {}) {
  const env = opts.env ?? {};
  const clientId = opts.clientId ?? (await registerClient({ env })).client_id;
  const { verifier, challenge } = await pkcePair();
  const opened = await openConsent(authorizeQuery({ clientId, challenge, state: "st", scope: opts.scope ?? "mcp offline_access" }), opts.identity ?? (await guest(env)), env);
  expect(opened.page.status, opened.html.slice(0, 200)).toBe(200);
  const r = await submitConsent(opened.form, opened.identity, { env });
  expect(r.status).toBe(303);
  return { clientId, verifier, challenge, code: redirectParams(r).code!, identity: opened.identity };
}

const exchange = (a: { clientId: string; code: string; verifier: string }, extra: Record<string, string> = {}, env: Partial<Env> = {}) =>
  tokenRequest({ grant_type: "authorization_code", client_id: a.clientId, code: a.code, code_verifier: a.verifier, ...extra }, { env });

const refresh = (clientId: string, rt: string, extra: Record<string, string> = {}, env: Partial<Env> = {}) =>
  tokenRequest({ grant_type: "refresh_token", client_id: clientId, refresh_token: rt, ...extra }, { env });

describe("keys", () => {
  it("derives the connector's five HKDF labels like internal/keys", async () => {
    // HKDF-SHA256(SECRET, salt "backyard", info <label>), computed independently.
    const want: Record<string, string> = {
      MCPClient: "0d5bb4c07f8b8c550a78a3cf140a1977f834db213df5b2323697f9a2ccd9c5c3",
      MCPConsent: "c3f004507285e1ba61b22517f2da854bbe7d9896c41be2a25906cda01919c0b8",
      MCPCode: "d6881337743ec390478dcfe66b9e1a5e838d7232a008034fd035767bd00971a1",
      MCPRefresh: "3311c2d20c6b172cdab0f20e71aa9e5ecf650b3e2152d551536733c81efac9a6",
      MCPAccess: "7264ce980fe68c32589d89079631a826b820c0c19100f15447594c49373e0bd9",
    };
    for (const [k, v] of Object.entries(want)) {
      expect(KeyLabel[k as keyof typeof KeyLabel], k).toBe(`backyard/mcp-${k.slice(3).toLowerCase()}/v1`);
      expect(hex((await deriveKey(SECRET, KeyLabel[k as keyof typeof KeyLabel])).raw), k).toBe(v);
    }
    expect(new Set(Object.values(KeyLabel)).size).toBe(Object.keys(KeyLabel).length);
  });
});

describe("enablement and routing (SPEC §8.4.1)", () => {
  const machine: [string, RequestInit][] = [
    ["/.well-known/oauth-protected-resource/__backyard/mcp", {}],
    ["/.well-known/oauth-authorization-server", {}],
    ["/__backyard/oauth/register", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }],
    ["/__backyard/oauth/token", { method: "POST" }],
    ["/__backyard/oauth/revoke", { method: "POST" }],
    ["/__backyard/mcp", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" }],
  ];

  it("answers 404 on every path while MCP_ENABLED is unset", async () => {
    for (const env of [{}, { MCP_ENABLED: "false" }, { MCP_ENABLED: "" }]) {
      for (const [path, init] of machine) {
        const r = await call(path, init, env);
        expect(r.status, path).toBe(404);
        expect(await r.json(), path).toEqual(NOT_ENABLED);
        expect(r.headers.getSetCookie(), path).toEqual([]);
      }
      const page = await call("/__backyard/auth/authorize?client_id=x", {}, env);
      expect(await expectPage(page, 404, "authorize")).toContain("not enabled on this server");
      expect(page.headers.getSetCookie()).toEqual([]);
      for (const method of ["GET", "DELETE"]) {
        const r = await call("/__backyard/api/mcp/grants", { method, headers: apiHeaders() }, env);
        expect(r.status, method).toBe(404);
        expect(await r.json()).toEqual(NOT_ENABLED);
      }
      expect((await call("/__backyard/api/mcp/grants/0123", { method: "DELETE", headers: apiHeaders() }, env)).status).toBe(404);
    }
    // Other paths below /.well-known/oauth- are JSON 404s too, without authentication.
    const other = await call("/.well-known/oauth-protected-resource", {}, MCP_ENV);
    expect(other.status).toBe(404);
    expect(((await other.json()) as { error: { code: string } }).error.code).toBe("not_found");
  });

  it("turns MCP off (404, a readiness problem) for invalid MCP variables, and nothing else", async () => {
    const env = { ...MCP_ENV, MCP_CLIENTS: "https://claude.ai" };
    const cfg = getConfig(testEnv(env));
    expect(cfg.mcp.enabled).toBe(false);
    expect(cfg.fatal).toBe("");
    expect(cfg.mcp.problems).toEqual([
      'MCP_CLIENTS: "https://claude.ai" is not a client metadata URL (an https URL with a path, without query, fragment or user info, at most 512 bytes); the Claude connector is off',
    ]);
    expect(cfg.problems).toContain(cfg.mcp.problems[0]);
    for (const [path, init] of machine) expect((await call(path, init, env)).status, path).toBe(404);
    expect((await call("/__backyard/api/me", {}, env)).status).toBe(200);
    expect((await call("/__backyard/healthz", {}, env)).status).toBe(200);
    expect((await call("/__backyard/readyz", {}, env)).status).toBe(200);
    expect(await (await call("/__backyard/readyz", {}, env)).text()).toBe(`ok\nproblem: ${cfg.mcp.problems[0]}\n`);
  });

  it("needs an https (or loopback) hub origin", async () => {
    expect(mcpEnabled(ctxFor())).toBe(true);
    expect(mcpEnabled(ctxFor({}, "http://backyard.test/__backyard/mcp"))).toBe(false);
    for (const host of ["localhost:8787", "127.0.0.1:8787", "hub.localhost", "[::1]:8787"]) {
      expect(mcpEnabled(ctxFor({}, `http://${host}/__backyard/mcp`)), host).toBe(true);
    }
    // PUBLIC_URL decides: an http one on a public host disables it (with a problem).
    const cfg = getConfig(testEnv({ ...MCP_ENV, PUBLIC_URL: "http://backyard.acme.com" }));
    expect(cfg.mcp.enabled).toBe(false);
    expect(cfg.mcp.problems.join()).toContain("MCP_ENABLED needs an https hub URL");
    const r = await call("http://backyard.acme.com/.well-known/oauth-authorization-server", {}, { ...MCP_ENV, PUBLIC_URL: "http://backyard.acme.com" });
    expect(r.status).toBe(404);
    const local = await call("http://127.0.0.1:8787/.well-known/oauth-authorization-server", {}, MCP_ENV);
    expect(local.status).toBe(200);
    expect(((await local.json()) as { issuer: string }).issuer).toBe("http://127.0.0.1:8787");
  });

  it("serves the machine paths on the canonical hub host only", async () => {
    const env = { ...MCP_ENV, PUBLIC_URL: "https://hub.example.com" };
    for (const [path, init] of machine) {
      const r = await call(path, init, env);
      expect(r.status, path).toBe(404);
      expect(((await r.json()) as { error: { code: string } }).error.code, path).toBe("not_found");
    }
    const page = await call("/__backyard/auth/authorize", {}, env);
    expect(await expectPage(page, 404, "authorize on another host")).toContain("Use https://hub.example.com/__backyard/mcp");
    // The canonical host compares case-insensitively and ignores the port.
    const ok = await call("https://HUB.example.com:8443/.well-known/oauth-authorization-server", {}, env);
    expect(ok.status).toBe(200);
    expect(((await ok.json()) as { issuer: string }).issuer).toBe("https://hub.example.com");
    // The grants API is a normal hub API: any hub host.
    expect((await call("/__backyard/api/mcp/grants", {}, env)).status).toBe(200);
  });

  it("leaves site hosts alone", async () => {
    // Path mode: the machine paths are the existing JSON 404 below a site, authorize goes to the hub.
    let r = await call("/s/demo/__backyard/oauth/token", { method: "POST" }, MCP_ENV);
    expect(r.status).toBe(404);
    expect(((await r.json()) as { error: { code: string } }).error.code).toBe("not_found");
    r = await call("/s/demo/__backyard/mcp", { method: "POST" }, MCP_ENV);
    expect(r.status).toBe(404);
    r = await call("/s/demo/__backyard/auth/authorize?client_id=x&state=1", {}, MCP_ENV);
    expect(r.status).toBe(302);
    expect(r.headers.get("Location")).toBe(ORIGIN + "/__backyard/auth/authorize?client_id=x&state=1");
    // Host mode: a site's /.well-known stays site content (here: no such site).
    const host = { ...MCP_ENV, URL_MODE: "host", BASE_DOMAIN: "backyard.test" };
    r = await call("https://demo.backyard.test/.well-known/oauth-authorization-server", { headers: { Accept: "text/html" } }, host);
    expect(r.status).toBe(404);
    expect(r.headers.get("Content-Type")).toContain("text/html");
    expect(await r.text()).toContain("Site not found");
    r = await call("https://demo.backyard.test/__backyard/oauth/register", { method: "POST" }, host);
    expect(r.status).toBe(404);
    expect(r.headers.get("Content-Type")).toBe("application/json");
    r = await call("https://backyard.test/.well-known/oauth-authorization-server", {}, host);
    expect(r.status).toBe(200);
  });

  it("answers wrong methods with 405 and Allow, without CORS or cookies", async () => {
    const notAllowed = { error: { code: "bad_request", message: "method not allowed" } };
    for (const [path, method, allow] of [
      ["/.well-known/oauth-authorization-server", "POST", "GET, HEAD"],
      ["/.well-known/oauth-protected-resource/__backyard/mcp", "OPTIONS", "GET, HEAD"],
      ["/__backyard/oauth/token", "GET", "POST"],
      ["/__backyard/oauth/register", "OPTIONS", "POST"],
      ["/__backyard/oauth/revoke", "HEAD", "POST"],
      ["/__backyard/oauth/token", "PUT", "POST"],
    ] as const) {
      const r = await call(path, { method, headers: { Origin: "https://claude.ai", "Access-Control-Request-Method": "POST" } }, MCP_ENV);
      expect(r.status, `${method} ${path}`).toBe(405);
      expect(r.headers.get("Allow"), `${method} ${path}`).toBe(allow);
      if (method !== "HEAD") expect(await r.json(), `${method} ${path}`).toEqual(notAllowed);
      expect(r.headers.getSetCookie()).toEqual([]);
      for (const [k] of r.headers) expect(k.startsWith("access-control-"), k).toBe(false);
    }
    const page = await call("/__backyard/auth/authorize", { method: "PUT" }, MCP_ENV);
    await expectPage(page, 405, "PUT authorize");
    expect(page.headers.get("Allow")).toBe("GET, POST");
    expect((await call("/__backyard/auth/authorize", { method: "HEAD" }, MCP_ENV)).status).toBe(405);
    for (const p of ["/__backyard/oauth/nope", "/__backyard/oauth/"]) {
      const r = await call(p, { method: "POST" }, MCP_ENV);
      expect(r.status, p).toBe(404);
      expect(((await r.json()) as { error: { code: string } }).error.code).toBe("not_found");
    }
  });

  it("derives P, R and M canonically and accepts the expected origins", () => {
    const ctx = ctxFor({ PUBLIC_URL: "HTTPS://Hub.Example.COM:443/" }, "https://hub.example.com/__backyard/mcp");
    expect(mcpIssuer(ctx)).toBe("https://hub.example.com");
    expect(mcpResource(ctx)).toBe("https://hub.example.com/__backyard/mcp");
    expect(mcpResourceMetadataURL(ctx)).toBe("https://hub.example.com/.well-known/oauth-protected-resource/__backyard/mcp");
    expect(mcpIssuer(ctxFor({ PUBLIC_URL: "https://hub.example.com:8443" }))).toBe("https://hub.example.com:8443");
    const c = ctxFor();
    for (const o of ["https://claude.ai", "https://claude.com", "HTTPS://CLAUDE.AI", ORIGIN, "https://claude.ai:443"]) expect(mcpAllowedOrigin(c, o), o).toBe(true);
    for (const o of ["https://evil.example", "http://claude.ai", "null", "", "https://claude.ai.evil.example"]) expect(mcpAllowedOrigin(c, o), o).toBe(false);
    // Origins come from the effective lists: none without them.
    const bare = ctxFor({ MCP_CLIENTS: "", MCP_REDIRECT_URIS: "" });
    expect(mcpAllowedOrigin(bare, "https://claude.ai")).toBe(false);
    expect(mcpAllowedOrigin(bare, ORIGIN)).toBe(true);
    expect(mcpChallenge(c)).toBe(`Bearer resource_metadata="${M}", scope="mcp"`);
    expect(mcpChallenge(c, "token revoked")).toBe(`Bearer error="invalid_token", error_description="token revoked", resource_metadata="${M}", scope="mcp"`);
  });
});

describe("discovery documents (SPEC §8.4.2)", () => {
  it("serves the protected resource metadata", async () => {
    const r = await call("/.well-known/oauth-protected-resource/__backyard/mcp", {}, { ...MCP_ENV, SERVER_NAME: "Acme Backyard" });
    expect(r.status).toBe(200);
    expect(r.headers.get("Content-Type")).toBe("application/json");
    expect(r.headers.get("Cache-Control")).toBe("public, max-age=300");
    expect(r.headers.getSetCookie()).toEqual([]);
    expect(await r.text()).toBe(
      `{"resource":"${MCP_RESOURCE}","authorization_servers":["${ORIGIN}"],"scopes_supported":["mcp"],"bearer_methods_supported":["header"],"resource_name":"Acme Backyard"}`,
    );
    const head = await call("/.well-known/oauth-protected-resource/__backyard/mcp", { method: "HEAD" }, MCP_ENV);
    expect(head.status).toBe(200);
    expect(head.headers.get("Cache-Control")).toBe("public, max-age=300");
    expect(head.headers.get("Content-Type")).toBe("application/json");
    expect(await head.text()).toBe("");
  });

  it("serves the authorization server metadata, with the CIMD flag only while clients are allowlisted", async () => {
    const want =
      `{"issuer":"${ORIGIN}","authorization_endpoint":"${ORIGIN}/__backyard/auth/authorize","token_endpoint":"${ORIGIN}/__backyard/oauth/token",` +
      `"registration_endpoint":"${ORIGIN}/__backyard/oauth/register","revocation_endpoint":"${ORIGIN}/__backyard/oauth/revoke","scopes_supported":["mcp","offline_access"],` +
      `"response_types_supported":["code"],"response_modes_supported":["query"],"grant_types_supported":["authorization_code","refresh_token"],` +
      `"token_endpoint_auth_methods_supported":["none"],"revocation_endpoint_auth_methods_supported":["none"],"code_challenge_methods_supported":["S256"],` +
      `"authorization_response_iss_parameter_supported":true`;
    let r = await call("/.well-known/oauth-authorization-server", {}, MCP_ENV);
    expect(r.status).toBe(200);
    expect(r.headers.get("Cache-Control")).toBe("public, max-age=300");
    expect(await r.text()).toBe(want + `,"client_id_metadata_document_supported":true}`);
    for (const v of ["", " ", " , "]) {
      r = await call("/.well-known/oauth-authorization-server", {}, { ...MCP_ENV, MCP_CLIENTS: v });
      expect(await r.text(), JSON.stringify(v)).toBe(want + "}");
    }
    const head = await call("/.well-known/oauth-authorization-server", { method: "HEAD" }, MCP_ENV);
    expect(head.status).toBe(200);
    expect(await head.text()).toBe("");
  });
});

describe("registration (SPEC §8.4.3)", () => {
  const register = (body: unknown, headers: Record<string, string> = { "Content-Type": "application/json" }, env: Partial<Env> = {}) =>
    call("/__backyard/oauth/register", { method: "POST", headers, body: typeof body === "string" ? body : JSON.stringify(body) }, { ...MCP_ENV, ...env });

  it("registers statelessly and answers what it registered", async () => {
    const before = Math.floor(Date.now() / 1000);
    const r = await register({
      client_name: "Claude Code",
      redirect_uris: [LOOPBACK, "https://claude.ai/api/mcp/auth_callback"],
      token_endpoint_auth_method: "client_secret_basic",
      grant_types: ["authorization_code", "urn:ietf:params:oauth:grant-type:jwt-bearer"],
      response_types: ["code", "token"],
      scope: "mcp admin",
      logo_uri: "https://evil.example/x.png",
      jwks: { keys: [] },
    });
    expect(r.status).toBe(201);
    expect(r.headers.get("Content-Type")).toBe("application/json");
    expect(r.headers.get("Cache-Control")).toBe("no-store");
    expect(r.headers.getSetCookie()).toEqual([]);
    const c = (await r.json()) as Record<string, unknown>;
    expect(Object.keys(c)).toEqual(["client_id", "client_id_issued_at", "client_name", "redirect_uris", "grant_types", "response_types", "token_endpoint_auth_method"]);
    expect(c.client_id).toMatch(/^bydc_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
    expect(Math.abs((c.client_id_issued_at as number) - before)).toBeLessThan(5);
    expect(c).toMatchObject({
      client_name: "Claude Code",
      redirect_uris: [LOOPBACK, "https://claude.ai/api/mcp/auth_callback"],
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      token_endpoint_auth_method: "none",
    });
    expect(payloadOf(c.client_id as string)).toEqual({ v: 1, n: "Claude Code", r: [LOOPBACK, "https://claude.ai/api/mcp/auth_callback"], t: c.client_id_issued_at });
    // Parameters of the media type are fine.
    expect((await register({ redirect_uris: [LOOPBACK] }, { "Content-Type": "application/json; charset=utf-8" })).status).toBe(201);
  });

  it("accepts the allowed redirect URIs and refuses everything else", async () => {
    for (const u of ["https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback", "http://localhost/callback", "http://127.0.0.1:53682/callback", "http://[::1]:1/cb", "http://localhost:5/cb?a=1"]) {
      const r = await register({ redirect_uris: [u] });
      expect(r.status, u).toBe(201);
    }
    for (const u of [
      "https://evil.example/cb",
      "https://claude.ai/other",
      "https://claude.ai/api/mcp/auth_callback/",
      "https://CLAUDE.ai/api/mcp/auth_callback",
      "http://claude.ai/x",
      "cursor://cb",
      "http://localhost/cb#f",
      "http://u@localhost/cb",
      "http://localhost.evil.example/cb",
      "http://127.0.0.2/cb",
      "https://localhost/cb",
      "http://localhost:0/cb",
      "http://localhost:99999/cb",
    ]) {
      const r = await register({ redirect_uris: [u] });
      const b = await expectOAuthError(r, 400, "invalid_redirect_uri", u);
      expect(b.error_description, u).toBe(`redirect URI ${u} is not allowed on this server (allowed: the URIs in MCP_REDIRECT_URIS and http loopback URIs)`);
    }
    // MCP_REDIRECT_URIS set but empty: loopback only.
    await expectOAuthError(await register({ redirect_uris: ["https://claude.ai/api/mcp/auth_callback"] }, undefined, { MCP_REDIRECT_URIS: "" }), 400, "invalid_redirect_uri", "empty list");
    expect((await register({ redirect_uris: [LOOPBACK] }, undefined, { MCP_REDIRECT_URIS: "" })).status).toBe(201);
    expect((await register({ redirect_uris: ["https://app.example/cb"] }, undefined, { MCP_REDIRECT_URIS: "https://app.example/cb" })).status).toBe(201);
  });

  it("refuses malformed registrations", async () => {
    const long = `http://127.0.0.1/${"a".repeat(500)}`;
    for (const [what, body, headers, error] of [
      ["text/plain", JSON.stringify({ redirect_uris: [LOOPBACK] }), { "Content-Type": "text/plain" }, "invalid_client_metadata"],
      ["no Content-Type", JSON.stringify({ redirect_uris: [LOOPBACK] }), {}, "invalid_client_metadata"],
      ["invalid JSON", "{", undefined, "invalid_client_metadata"],
      ["an array", JSON.stringify([{ redirect_uris: [LOOPBACK] }]), undefined, "invalid_client_metadata"],
      ["null", "null", undefined, "invalid_client_metadata"],
      ["over 16 KiB", JSON.stringify({ redirect_uris: [LOOPBACK], client_name: "x".repeat(17 * 1024) }), undefined, "invalid_client_metadata"],
      ["no redirect_uris", JSON.stringify({ client_name: "x" }), undefined, "invalid_redirect_uri"],
      ["empty redirect_uris", JSON.stringify({ redirect_uris: [] }), undefined, "invalid_redirect_uri"],
      ["a string", JSON.stringify({ redirect_uris: LOOPBACK }), undefined, "invalid_redirect_uri"],
      ["a number in the list", JSON.stringify({ redirect_uris: [LOOPBACK, 5] }), undefined, "invalid_redirect_uri"],
      ["5 URIs", JSON.stringify({ redirect_uris: [1, 2, 3, 4, 5].map((n) => `http://127.0.0.1:${n}/cb`) }), undefined, "invalid_redirect_uri"],
      ["a 513-byte URI", JSON.stringify({ redirect_uris: [long] }), undefined, "invalid_redirect_uri"],
      ["a client_name object", JSON.stringify({ redirect_uris: [LOOPBACK], client_name: { a: 1 } }), undefined, "invalid_client_metadata"],
      ["a client_id over 1024 bytes", JSON.stringify({ redirect_uris: [1, 2, 3, 4].map((n) => `http://127.0.0.1/${n}${"a".repeat(470)}`) }), undefined, "invalid_client_metadata"],
      // The BOM is kept, as on Go, so the body is not JSON.
      ["a UTF-8 BOM", "\ufeff" + JSON.stringify({ redirect_uris: [LOOPBACK] }), undefined, "invalid_client_metadata"],
    ] as const) {
      const r = await register(body, headers ?? { "Content-Type": "application/json" });
      const b = await expectOAuthError(r, 400, error, what);
      if (what === "a client_id over 1024 bytes") expect(b.error_description).toBe("too many or too long redirect_uris");
    }
  });

  it("sanitizes client names", async () => {
    for (const [sent, want] of [
      ["  Claude‮\u0007 Code⁦ ", "Claude Code"],
      ["‎‏‪‫‬‭⁧⁨⁩\n\t", "MCP client"],
      [undefined, "MCP client"],
      [null, "MCP client"],
      ["", "MCP client"],
      ["é".repeat(65), "é".repeat(64)],
      ["😀".repeat(70), "😀".repeat(64)],
      ["<b>Tool</b> & co", "<b>Tool</b> & co"],
    ] as const) {
      const body: Record<string, unknown> = { redirect_uris: [LOOPBACK] };
      if (sent !== undefined) body.client_name = sent;
      const r = await register(body);
      expect(r.status).toBe(201);
      expect(((await r.json()) as { client_name: string }).client_name, JSON.stringify(sent)).toBe(want);
    }
  });

  it("refuses a tampered or foreign client_id at authorize", async () => {
    const c = await registerClient();
    const { challenge } = await pkcePair();
    const [body, mac] = c.client_id.slice(5).split(".") as [string, string];
    const other = await forge("bydc_", KeyLabel.MCPCode, payloadOf(c.client_id)); // signed with another purpose's key
    for (const id of [tamper(c.client_id), `bydc_${body}x.${mac}`, other, "bydc_", "bydc_garbage", c.client_id.slice(5)]) {
      const html = await expectPage(await call(`/__backyard/auth/authorize?${authorizeQuery({ clientId: id, challenge, state: "s" })}`, {}, MCP_ENV), 400, id.slice(0, 30));
      expect(html).toContain("Unknown app");
    }
  });
});

describe("redirect and resource rules (SPEC §8.4.4)", () => {
  it("matches redirect URIs like the shared table", () => {
    for (const [registered, requested, match] of [
      ["https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback", true],
      ["https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback/", false],
      ["https://claude.ai/api/mcp/auth_callback", "https://CLAUDE.ai/api/mcp/auth_callback", false],
      ["http://localhost/callback", "http://localhost:53682/callback", true],
      ["http://127.0.0.1:9/callback", "http://127.0.0.1:41000/callback", true],
      ["http://localhost/callback", "http://127.0.0.1:53682/callback", false],
      ["http://[::1]/cb", "http://[::1]:1/cb", true],
      ["http://localhost/callback", "http://localhost:1/callback?x=1", false],
      ["http://localhost/callback?a=1", "http://localhost:5/callback?a=1", true],
      ["http://localhost/callback", "https://localhost/callback", false],
      // Beyond the table: fragments and userinfo never match.
      ["http://localhost/callback", "http://localhost:5/callback#x", false],
      ["https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback#", false],
      ["http://localhost/callback", "http://u@localhost:5/callback", false],
    ] as const) {
      expect(redirectMatches(registered, requested), `${registered} ~ ${requested}`).toBe(match);
    }
    expect(parseLoopbackURI("http://localhost:53682/callback")).toEqual({ host: "localhost", port: "53682", rest: "/callback" });
    expect(parseLoopbackURI("http://[::1]")).toEqual({ host: "[::1]", port: "", rest: "" });
    for (const u of ["http://LOCALHOST/cb", "http://localhost:/cb", "http://localhost:x/cb", "http://127.1/cb", "http://localhost\\@evil/cb", "http://local host/cb"]) {
      expect(parseLoopbackURI(u), u).toBeNull();
    }
    // As Go's url.Parse and unicode.IsSpace: malformed path escapes, control
    // characters and U+0085 are refused; query escapes and U+FEFF are not.
    for (const u of ["http://localhost/%zz", "http://localhost/a%2", "http://localhost/a\u0001b", "http://localhost/a\u007fb", "http://localhost/a\u0085b", "http://localhost/?a=\u0001"]) {
      expect(parseLoopbackURI(u), JSON.stringify(u)).toBeNull();
    }
    for (const u of ["http://localhost/?a=%zz", "http://localhost?a=%zz", "http://localhost/a%2Fb", "http://localhost/a\ufeffb"]) {
      expect(parseLoopbackURI(u), JSON.stringify(u)).not.toBeNull();
    }
  });

  it("canonicalizes resources like the shared table", () => {
    const R = "https://hub.example/__backyard/mcp";
    for (const [input, valid] of [
      ["https://hub.example/__backyard/mcp", true],
      ["HTTPS://HUB.EXAMPLE:443/__backyard/mcp/", true],
      ["https://hub.example/__backyard/mcp?x=1", false],
      ["https://hub.example/__backyard/mcp#f", false],
      ["http://hub.example/__backyard/mcp", false],
      ["https://other.example/__backyard/mcp", false],
      ["https://hub.example/__backyard/MCP", false],
      ["https://u@hub.example/__backyard/mcp", false],
      ["", false],
      // Beyond the table.
      ["https://hub.example/__backyard/mcp//", false],
      ["https://hub.example:8443/__backyard/mcp", false],
      ["https://hub.example/__backyard/%6Dcp", false],
      ["hub.example/__backyard/mcp", false],
    ] as const) {
      expect(canonicalResource(input) === R, input).toBe(valid);
    }
    expect(canonicalResource("http://LOCALHOST:80/x/")).toBe("http://localhost/x");
    expect(canonicalResource("ftp://hub.example/x")).toBeNull();
  });
});

/** Stubs fetch for the test client metadata URL; other URLs are recorded and refused. */
function stubFetch(handler: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const other: string[] = [];
  vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
    if (url !== CIMD_URL) {
      other.push(url);
      throw new Error("no network in tests");
    }
    calls.push({ url, ...(init ? { init } : {}) });
    return handler(url, init);
  });
  return { calls, other };
}

const cimdDoc = (extra: Record<string, unknown> = {}) => ({
  client_id: CIMD_URL,
  client_name: "Example App",
  redirect_uris: ["http://localhost/callback", "https://evil.example/cb"],
  token_endpoint_auth_method: "none",
  grant_types: ["authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:jwt-bearer"],
  ...extra,
});

const jsonResponse = (doc: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(doc), { status: 200, headers: { "Content-Type": "application/json", ...headers } });

describe("client metadata documents (SPEC §8.4.3)", () => {
  const authorizeCIMD = async (redirectURI = "http://localhost:53682/callback", identity: Record<string, string> = {}, env: Partial<Env> = {}) => {
    const { challenge } = await pkcePair();
    return call(`/__backyard/auth/authorize?${authorizeQuery({ clientId: CIMD_URL, challenge, redirectURI, state: "s" })}`, { headers: identity }, { ...MCP_ENV, ...CIMD_ENV, ...env });
  };

  it("fetches an allowlisted document without redirects and shows it as verified", async () => {
    const { calls } = stubFetch(() => jsonResponse(cimdDoc(), { "Cache-Control": "max-age=600" }));
    const r = await authorizeCIMD();
    const html = await r.text();
    expect(r.status, html.slice(0, 300)).toBe(200);
    expect(calls).toHaveLength(1);
    expect(calls[0]!.init?.redirect).toBe("manual");
    expect(new Headers(calls[0]!.init?.headers).get("Accept")).toBe("application/json");
    expect(calls[0]!.init?.signal).toBeInstanceOf(AbortSignal);
    expect(html).toContain("<strong>Example App</strong> — verified by <strong>client.example</strong>");
    expect(html).toContain("an app on this computer (http://localhost:53682)");
    expect(r.headers.get("Content-Security-Policy")).toMatch(/form-action 'self' http:\/\/localhost:53682$/);
    expect(hiddenInputs(html).client_id).toBe(CIMD_URL);
    // Cached: a second request does not fetch.
    expect((await authorizeCIMD("http://localhost:1/callback")).status).toBe(200);
    expect(calls).toHaveLength(1);
    // Redirect URIs the policy refuses are ignored, never used.
    const evil = await authorizeCIMD("https://evil.example/cb");
    expect(await expectPage(evil, 400, "a document's refused redirect URI")).toContain("Redirect address not allowed");
  });

  it("refuses documents that break the rules, with the reason", async () => {
    let doc: () => Response = () => jsonResponse(cimdDoc());
    stubFetch(() => doc());
    const pad = (n: number) => {
      const base = JSON.stringify(cimdDoc({ pad: "" }));
      return JSON.stringify(cimdDoc({ pad: "x".repeat(n - base.length) }));
    };
    for (const [what, make, reason] of [
      ["another client_id", () => jsonResponse(cimdDoc({ client_id: "https://client.example/other" })), "its client_id is not its own URL"],
      ["a redirect", () => new Response(null, { status: 302, headers: { Location: "https://evil.example/doc" } }), "it answered with a redirect (HTTP 302), which is not followed"],
      ["HTTP 404", () => new Response("nope", { status: 404 }), "it answered HTTP 404"],
      ["text/html", () => new Response(JSON.stringify(cimdDoc()), { status: 200, headers: { "Content-Type": "text/html" } }), "it is not JSON"],
      ["5121 bytes (streamed)", () => new Response(new Blob([pad(5121)]).stream(), { status: 200, headers: { "Content-Type": "application/json" } }), "larger than 5120 bytes"],
      ["a declared length over 5120", () => new Response(pad(6000), { status: 200, headers: { "Content-Type": "application/json", "Content-Length": "6000" } }), "larger than 5120 bytes"],
      ["private_key_jwt", () => jsonResponse(cimdDoc({ token_endpoint_auth_method: "private_key_jwt" })), "client authentication"],
      [
        "a methods list without none",
        () => jsonResponse(cimdDoc({ token_endpoint_auth_method: "none", token_endpoint_auth_methods_supported: ["private_key_jwt"] })),
        "client authentication",
      ],
      ["a methods string", () => jsonResponse(cimdDoc({ token_endpoint_auth_methods_supported: "none" })), "token_endpoint_auth_methods_supported is not an array of strings"],
      ["a methods null", () => jsonResponse(cimdDoc({ token_endpoint_auth_methods_supported: null })), "token_endpoint_auth_methods_supported is not an array of strings"],
      ["a client_secret", () => jsonResponse(cimdDoc({ client_secret: "s" })), "client secret"],
      ["no allowed redirect URI", () => jsonResponse(cimdDoc({ redirect_uris: ["https://evil.example/cb"] })), "none of its redirect_uris is allowed"],
      ["11 redirect URIs", () => jsonResponse(cimdDoc({ redirect_uris: Array.from({ length: 11 }, (_, i) => `http://localhost/${i}`) })), "redirect_uris must be an array of 1 to 10 URIs"],
      ["not an object", () => jsonResponse([cimdDoc()]), "it is not a JSON object"],
      ["invalid JSON", () => new Response("{", { status: 200, headers: { "Content-Type": "application/json" } }), "it is not a JSON object"],
      ["not UTF-8", () => new Response(new Uint8Array([0x7b, 0xff, 0x7d]), { status: 200, headers: { "Content-Type": "application/json" } }), "it is not UTF-8 JSON"],
      ["a client_name number", () => jsonResponse(cimdDoc({ client_name: 5 })), "client_name is not a string"],
    ] as const) {
      resetMcpCaches();
      doc = make;
      const html = await expectPage(await authorizeCIMD(), 502, what);
      expect(html, what).toContain("Could not verify the app");
      expect(html, what).toContain(reason);
      expect(html, what).toContain(CIMD_URL);
    }
    // 5120 bytes exactly is fine; a +json type too; unknown members are ignored.
    resetMcpCaches();
    doc = () => new Response(new Blob([pad(5120)]).stream(), { status: 200, headers: { "Content-Type": "application/oauth-client+json; charset=utf-8" } });
    expect((await authorizeCIMD()).status).toBe(200);
    // Without a client_name the host names the app.
    resetMcpCaches();
    doc = () => jsonResponse(cimdDoc({ client_name: undefined }));
    expect(await (await authorizeCIMD()).text()).toContain("<strong>client.example</strong> — verified by <strong>client.example</strong>");
    // ChatGPT's published document prefers private_key_jwt but lists none
    // (MCP SEP-3149): the list wins.
    resetMcpCaches();
    doc = () =>
      jsonResponse(
        cimdDoc({
          client_name: "ChatGPT",
          token_endpoint_auth_method: "private_key_jwt",
          token_endpoint_auth_methods_supported: ["none", "private_key_jwt"],
          token_endpoint_auth_signing_alg: "RS256",
          jwks_uri: "https://chatgpt.com/oauth/jwks.json",
        }),
      );
    const chatgpt = await authorizeCIMD();
    expect(chatgpt.status).toBe(200);
    expect(await chatgpt.text()).toContain("<strong>ChatGPT</strong> — verified by <strong>client.example</strong>");
  });

  it("never fetches a URL that is not allowlisted", async () => {
    const { calls, other } = stubFetch(() => jsonResponse(cimdDoc()));
    const { challenge } = await pkcePair();
    for (const id of ["https://example.com/x", CIMD_URL + "/", "https://client.example/oauth/METADATA"]) {
      const html = await expectPage(await call(`/__backyard/auth/authorize?${authorizeQuery({ clientId: id, challenge, state: "s" })}`, {}, { ...MCP_ENV, ...CIMD_ENV }), 400, id);
      expect(html).toContain("App not allowed");
      expect(html).toContain("MCP_CLIENTS");
    }
    expect(calls).toHaveLength(0);
    expect(other).toHaveLength(0);
    // The token endpoint never fetches either.
    const r = await tokenRequest({ grant_type: "authorization_code", client_id: CIMD_URL, code: "byac_x.y", code_verifier: "v".repeat(43) }, { env: CIMD_ENV });
    await expectOAuthError(r, 400, "invalid_grant", "an allowlisted client at the token endpoint");
    expect(calls).toHaveLength(0);
  });

  it("caches for max-age clamped to 60–3600 s, never with no-store, and throttles failures for 10 s", async () => {
    expect(cimdCacheTTL("")).toBe(300);
    expect(cimdCacheTTL("max-age=10")).toBe(60);
    expect(cimdCacheTTL("public, max-age=900")).toBe(900);
    expect(cimdCacheTTL("max-age=86400")).toBe(3600);
    expect(cimdCacheTTL('max-age="120"')).toBe(120);
    expect(cimdCacheTTL("max-age=600, no-store")).toBe(0);
    expect(cimdCacheTTL("No-Cache")).toBe(0);
    expect(cimdCacheTTL("max-age=x")).toBe(300);

    vi.useFakeTimers({ toFake: ["Date"] });
    const t0 = Date.now();
    let cc = "max-age=10";
    let fail = false;
    const { calls } = stubFetch(() => (fail ? new Response("down", { status: 503 }) : jsonResponse(cimdDoc(), { "Cache-Control": cc })));
    expect((await authorizeCIMD()).status).toBe(200);
    vi.setSystemTime(t0 + 59_000);
    expect((await authorizeCIMD()).status).toBe(200);
    expect(calls).toHaveLength(1);
    vi.setSystemTime(t0 + 61_000);
    expect((await authorizeCIMD()).status).toBe(200);
    expect(calls).toHaveLength(2);

    resetMcpCaches();
    cc = "no-store";
    expect((await authorizeCIMD()).status).toBe(200);
    expect((await authorizeCIMD()).status).toBe(200);
    expect(calls).toHaveLength(4);

    resetMcpCaches();
    fail = true;
    expect((await authorizeCIMD()).status).toBe(502);
    expect(calls).toHaveLength(5);
    fail = false;
    vi.setSystemTime(t0 + 61_000 + 9_000);
    const throttled = await authorizeCIMD();
    expect(await expectPage(throttled, 502, "within the throttle")).toContain("it answered HTTP 503");
    expect(calls).toHaveLength(5);
    vi.setSystemTime(t0 + 61_000 + 11_000);
    expect((await authorizeCIMD()).status).toBe(200);
    expect(calls).toHaveLength(6);
  });

  it("shares one fetch between concurrent requests", async () => {
    const { calls } = stubFetch(async () => {
      await new Promise((r) => setTimeout(r, 100));
      return jsonResponse(cimdDoc());
    });
    const results = await Promise.all(Array.from({ length: 20 }, (_, i) => authorizeCIMD(`http://localhost:${2000 + i}/callback`)));
    expect(results.map((r) => r.status)).toEqual(Array(20).fill(200));
    expect(calls).toHaveLength(1);
  });

  it("runs the whole flow with a CIMD client", async () => {
    stubFetch(() => jsonResponse(cimdDoc()));
    const c = await mcpConnect({ clientId: CIMD_URL, env: CIMD_ENV, redirectURI: "http://localhost:53682/callback" });
    const v = await verify(c.access_token, CIMD_ENV);
    expect(v.ok && v.grant).toMatchObject({ client_id: CIMD_URL, client_name: "Example App", client_kind: "metadata_document", client_host: "client.example", redirect_uri: "http://localhost:53682/callback" });
    const list = await call("/__backyard/api/mcp/grants", { headers: c.identity }, { ...MCP_ENV, ...CIMD_ENV });
    expect(((await list.json()) as { grants: Record<string, unknown>[] }).grants[0]).toMatchObject({ client_kind: "metadata_document", client_host: "client.example", redirect_host: "localhost:53682" });
    // Taking the URL off the allowlist ends the connection's refreshes.
    await expectOAuthError(await refresh(CIMD_URL, c.refresh_token, {}, { MCP_CLIENTS: "" }), 401, "invalid_client", "a CIMD client no longer allowlisted");
    expect((await refresh(CIMD_URL, c.refresh_token, {}, CIMD_ENV)).status).toBe(200);
  });
});

describe("authorization requests (SPEC §8.4.5)", () => {
  it("renders a page for every failure before the redirect URI is validated, and never redirects", async () => {
    const c = await registerClient();
    const withHTTPS = await registerClient({ redirectURIs: ["https://claude.ai/api/mcp/auth_callback"] });
    const { challenge } = await pkcePair();
    const good = new URLSearchParams(authorizeQuery({ clientId: c.client_id, challenge, state: "s1" }));
    const variant = (change: (q: URLSearchParams) => void) => {
      const q = new URLSearchParams(good);
      change(q);
      return q.toString();
    };
    const cases: [string, string, number, string, Partial<Env>?][] = [
      ["a request URI over 2048 bytes", variant((q) => q.set("prompt", "x".repeat(2100))), 400, "Invalid authorization request"],
      ["a malformed %-escape", variant((q) => q.set("prompt", "x")).replace("prompt=x", "prompt=%zz"), 400, "Invalid authorization request"],
      ["a ; separator", variant((q) => q.set("prompt", "x")).replace("prompt=x", "prompt=a;b"), 400, "Invalid authorization request"],
      ["no client_id", variant((q) => q.delete("client_id")), 400, "Unknown app"],
      ["an unknown kind of client_id", variant((q) => q.set("client_id", "hello")), 400, "Unknown app"],
      ["a CIMD URL not allowlisted", variant((q) => q.set("client_id", "https://example.com/x")), 400, "App not allowed"],
      ["an unregistered redirect URI", variant((q) => q.set("redirect_uri", "http://127.0.0.1:9/other")), 400, "Redirect address not allowed"],
      ["a foreign redirect URI", variant((q) => q.set("redirect_uri", "https://evil.example/cb")), 400, "Redirect address not allowed"],
      ["another loopback host", variant((q) => q.set("redirect_uri", "http://localhost:9/callback")), 400, "Redirect address not allowed"],
      ["no redirect_uri", variant((q) => q.delete("redirect_uri")), 400, "Redirect address not allowed"],
      ["a registered URI the policy no longer allows", authorizeQuery({ clientId: withHTTPS.client_id, challenge, redirectURI: "https://claude.ai/api/mcp/auth_callback" }), 400, "Redirect address not allowed", { MCP_REDIRECT_URIS: "" }],
    ];
    for (const k of ["response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method", "resource", "scope", "state", "response_mode"]) {
      const twice = (q: URLSearchParams) => {
        q.append(k, q.get(k) ?? "query");
        if (q.getAll(k).length < 2) q.append(k, "query");
      };
      cases.push([`a repeated ${k}`, variant(twice), 400, "Invalid authorization request"]);
    }
    for (const [what, query, status, text, env] of cases) {
      const html = await expectPage(await call(`/__backyard/auth/authorize?${query}`, { headers: { "Sec-Fetch-Site": "cross-site" } }, { ...MCP_ENV, ...env }), status, what);
      expect(html, what).toContain(text);
    }
  });

  it("redirects every later failure to the client with error, state and iss", async () => {
    const c = await registerClient();
    const { challenge } = await pkcePair();
    const state = `cf+&=/<>"' ${Math.random()}`;
    const good = new URLSearchParams(authorizeQuery({ clientId: c.client_id, challenge, state }));
    for (const [what, change, error] of [
      ["no code_challenge", (q: URLSearchParams) => q.delete("code_challenge"), "invalid_request"],
      ["plain", (q: URLSearchParams) => q.set("code_challenge_method", "plain"), "invalid_request"],
      ["no code_challenge_method", (q: URLSearchParams) => q.delete("code_challenge_method"), "invalid_request"],
      ["a short code_challenge", (q: URLSearchParams) => q.set("code_challenge", "abc"), "invalid_request"],
      ["a code_challenge with padding", (q: URLSearchParams) => q.set("code_challenge", challenge.slice(0, 42) + "="), "invalid_request"],
      ["a foreign resource", (q: URLSearchParams) => q.set("resource", "https://evil.example/__backyard/mcp"), "invalid_target"],
      ["the resource with a query", (q: URLSearchParams) => q.set("resource", MCP_RESOURCE + "?x=1"), "invalid_target"],
      ["an empty resource", (q: URLSearchParams) => q.set("resource", ""), "invalid_target"],
      ["response_type token", (q: URLSearchParams) => q.set("response_type", "token"), "unsupported_response_type"],
      ["no response_type", (q: URLSearchParams) => q.delete("response_type"), "unsupported_response_type"],
      ["a request object", (q: URLSearchParams) => q.set("request", "e30.e30."), "request_not_supported"],
      ["a request_uri", (q: URLSearchParams) => q.set("request_uri", "urn:x"), "request_uri_not_supported"],
      ["response_mode fragment", (q: URLSearchParams) => q.set("response_mode", "fragment"), "invalid_request"],
    ] as const) {
      const q = new URLSearchParams(good);
      change(q);
      const r = await call(`/__backyard/auth/authorize?${q}`, { headers: { "Sec-Fetch-Site": "cross-site" } }, MCP_ENV);
      const p = expectErrorRedirect(r, 302, error, state, what);
      expect(p.error_description!.length, what).toBeGreaterThan(0);
      expect(r.headers.getSetCookie(), `${what}: no guest is minted before the redirect`).toEqual([]);
    }
    // A state over 512 bytes goes back without state; response_mode=query is fine.
    const long = new URLSearchParams(good);
    long.set("state", "s".repeat(513));
    expectErrorRedirect(await call(`/__backyard/auth/authorize?${long}`, {}, MCP_ENV), 302, "invalid_request", undefined, "a long state");
    const rm = new URLSearchParams(good);
    rm.set("response_mode", "query");
    expect((await call(`/__backyard/auth/authorize?${rm}`, {}, MCP_ENV)).status).toBe(200);
    // The registered URI's query is kept (and "&" joins).
    const withQuery = "http://127.0.0.1:9/callback?a=1";
    const cq = await registerClient({ redirectURIs: [withQuery] });
    const plain = authorizeQuery({ clientId: cq.client_id, challenge, redirectURI: withQuery, state: "s" }).replace("code_challenge_method=S256", "code_challenge_method=plain");
    const r2 = await call(`/__backyard/auth/authorize?${plain}`, {}, MCP_ENV);
    expect(r2.status).toBe(302);
    const loc = r2.headers.get("Location")!;
    expect(loc.startsWith(withQuery + "&error=invalid_request&error_description=")).toBe(true);
    expect(loc.endsWith("&state=s&iss=" + encodeURIComponent(ORIGIN))).toBe(true);
    expect(new URL(loc).searchParams.get("a")).toBe("1");
  });

  it("shows the consent page in mode none: a guest, the app (not verified), where the approval goes, the hub, the expiry", async () => {
    const c = await registerClient({ name: 'Tool <b>&</b> "x"' });
    const { challenge } = await pkcePair();
    const state = `a&b<"c'>`;
    for (const sfs of ["cross-site", "none", "same-origin", "same-site", undefined]) {
      const headers: Record<string, string> = sfs ? { "Sec-Fetch-Site": sfs } : {};
      const r = await call(`/__backyard/auth/authorize?${authorizeQuery({ clientId: c.client_id, challenge, state, scope: "openid mcp offline_access profile" })}`, { headers }, MCP_ENV);
      expect(r.status, String(sfs)).toBe(200);
      const html = await r.text();
      // A guest is minted for a browser without a cookie.
      const cookie = r.headers.getSetCookie().find((x) => x.startsWith("backyard_anon="));
      expect(cookie, "a guest cookie").toBeDefined();
      const me = await whoami({ Cookie: cookie!.split(";")[0]! });
      expect(html).toContain(`<strong>${me.name}</strong>. This Backyard has no sign-in; the app will act as this browser's guest.`);
      expect(html).toContain('<a href="/__backyard/auth/logout">Not you? Sign out</a>');
      expect(html).toContain('<strong>"Tool &lt;b&gt;&amp;&lt;/b&gt; &quot;x&quot;"</strong> (name provided by the app, not verified)');
      expect(html).not.toContain("<b>&</b>");
      expect(html).toContain("<strong>an app on this computer (http://127.0.0.1:9)</strong>. Only continue if you just started the sign-in from an app on this computer.");
      expect(html).toContain("can see the sites on backyard.test and read their files, and publish or change sites as you.");
      expect(html).toContain("Published sites are visible to everyone who can sign in to this Backyard.");
      const until = new Date(Date.now() + 720 * 3600 * 1000).toISOString().slice(0, 10);
      expect(html).toContain(`This connection lasts until ${until} unless you disconnect it on the Backyard hub.`);
      expect(html).toContain("Only allow this if you started connecting Tool &lt;b&gt;&amp;&lt;/b&gt; &quot;x&quot; to Backyard yourself just now.");
      expect(html).toContain('<form id="consent" method="post" action="/__backyard/auth/authorize">');
      expect(html).toContain('<button type="submit" name="decision" value="allow" id="allow">Allow</button>');
      expect(html).toContain('<button type="submit" name="decision" value="deny" id="deny">Cancel</button>');
      expect(html).not.toMatch(/autofocus/i);
      expect(html).not.toMatch(/<script/i);
      expect(html).toMatch(/visibility:\s*hidden/);
      expect(html).toMatch(/animation:\s*by-arm 0s linear 0\.8s forwards/);
      expect(html).toMatch(/@keyframes by-arm\s*\{\s*to\s*\{\s*visibility:\s*visible;?\s*\}\s*\}/);
      const form = hiddenInputs(html);
      expect(Object.keys(form)).toEqual(["response_type", "client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "scope", "resource", "csrf"]);
      expect(form).toMatchObject({ response_type: "code", client_id: c.client_id, redirect_uri: LOOPBACK, state, code_challenge: challenge, code_challenge_method: "S256", scope: "mcp offline_access", resource: MCP_RESOURCE });
      expect(form.csrf).toMatch(/^\d+\.[A-Za-z0-9_-]+$/);
      expect(form.csrf!.length).toBeLessThanOrEqual(128);
      const exp = Number(form.csrf!.split(".")[0]);
      expect(exp - Math.floor(Date.now() / 1000)).toBeGreaterThan(590);
      expect(exp - Math.floor(Date.now() / 1000)).toBeLessThanOrEqual(600);
      // Headers.
      expect(r.headers.get("Content-Type")).toBe("text/html; charset=utf-8");
      expect(r.headers.get("Cache-Control")).toContain("no-store");
      expect(r.headers.get("Referrer-Policy")).toBe("no-referrer");
      expect(r.headers.get("X-Frame-Options")).toBe("DENY");
      expect(r.headers.get("X-Content-Type-Options")).toBe("nosniff");
      expect(r.headers.get("Content-Security-Policy")).toBe(
        "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self' http://127.0.0.1:9",
      );
    }
    // Without state the field is left out; without offline_access the scope is mcp; no resource means R.
    const r = await openConsent(authorizeQuery({ clientId: c.client_id, challenge, resource: null }));
    expect(r.form.state).toBeUndefined();
    expect(r.form.scope).toBe("mcp");
    expect(r.form.resource).toBe(MCP_RESOURCE);
    // Another loopback port is the same registered URI; the CSP names it.
    const other = await openConsent(authorizeQuery({ clientId: c.client_id, challenge, redirectURI: "http://127.0.0.1:53682/callback" }));
    expect(other.page.status).toBe(200);
    expect(other.form.redirect_uri).toBe("http://127.0.0.1:53682/callback");
    expect(other.page.headers.get("Content-Security-Policy")).toMatch(/form-action 'self' http:\/\/127\.0\.0\.1:53682$/);
  });

  it("names the form-action origin for https, localhost and [::1] redirects", async () => {
    const { challenge } = await pkcePair();
    for (const [registered, requested, source, shown] of [
      ["https://claude.ai/api/mcp/auth_callback", "https://claude.ai/api/mcp/auth_callback", "https://claude.ai", "Backyard will send the approval to <strong>https://claude.ai</strong>."],
      ["http://localhost/callback", "http://localhost:53682/callback", "http://localhost:53682", "an app on this computer (http://localhost:53682)"],
      ["http://[::1]/cb", "http://[::1]:5555/cb", "http:", "an app on this computer (http://[::1]:5555)"],
    ] as const) {
      const c = await registerClient({ redirectURIs: [registered] });
      const r = await openConsent(authorizeQuery({ clientId: c.client_id, challenge, redirectURI: requested }));
      expect(r.page.status, registered).toBe(200);
      expect(r.page.headers.get("Content-Security-Policy"), registered).toMatch(new RegExp(`form-action 'self' ${source.replace(/[.[\]]/g, "\\$&")}$`));
      expect(r.html, registered).toContain(shown);
    }
  });
});

describe("the consent decision (SPEC §8.4.5)", () => {
  async function opened() {
    const c = await registerClient();
    const { challenge, verifier } = await pkcePair();
    const o = await openConsent(authorizeQuery({ clientId: c.client_id, challenge, state: "st-1", scope: "mcp offline_access" }));
    expect(o.page.status).toBe(200);
    return { ...o, clientId: c.client_id, challenge, verifier };
  }

  it("refuses a form whose CSRF token or fields do not match, or that another person sends", async () => {
    const o = await opened();
    const other = await registerClient({ name: "Other client" });
    const [exp, mac] = o.form.csrf!.split(".") as [string, string];
    for (const [what, f] of [
      ["no CSRF token", { ...o.form, csrf: "" }],
      ["a tampered CSRF token", { ...o.form, csrf: tamper(`${exp}.${mac}`) }],
      ["a CSRF token of another expiry", { ...o.form, csrf: `${Number(exp) - 1}.${mac}` }],
      ["another state", { ...o.form, state: "st-2" }],
      ["no state", { ...o.form, state: "" }],
      ["another loopback port", { ...o.form, redirect_uri: "http://127.0.0.1:10/callback" }],
      ["another code_challenge", { ...o.form, code_challenge: (await pkcePair()).challenge }],
      ["another scope", { ...o.form, scope: "mcp" }],
      ["another client", { ...o.form, client_id: other.client_id }],
    ] as const) {
      const html = await expectPage(await submitConsent(f as Record<string, string>, o.identity), 403, what);
      expect(html, what).toContain("This approval page expired or was not issued to you.");
    }
    // Another guest cannot use the page.
    await expectPage(await submitConsent(o.form, await guest()), 403, "another person");
    // A browser without its cookie neither (and gets no new guest).
    const bare = await submitConsent(o.form, {});
    await expectPage(bare, 403, "no cookie");
    expect(bare.headers.getSetCookie()).toEqual([]);
    // The page still works afterwards.
    expect((await submitConsent(o.form, o.identity)).status).toBe(303);
  });

  it("accepts an expired CSRF token for 60 s of clock skew only", async () => {
    const o = await opened();
    const me = await whoami(o.identity);
    const now = Math.floor(Date.now() / 1000);
    const token = async (exp: number) => {
      const msg = JSON.stringify(["mcp-consent", exp, me.id, "", o.clientId, LOOPBACK, "st-1", o.challenge, "mcp offline_access", MCP_RESOURCE]);
      const sig = new Uint8Array(await crypto.subtle.sign("HMAC", (await deriveKey(SECRET, KeyLabel.MCPConsent)).hmac, new TextEncoder().encode(msg)));
      return `${exp}.${b64url(sig)}`;
    };
    expect((await submitConsent({ ...o.form, csrf: await token(now - 30) }, o.identity)).status).toBe(303);
    await expectPage(await submitConsent({ ...o.form, csrf: await token(now - 70) }, o.identity), 403, "expired");
    await expectPage(await submitConsent({ ...o.form, csrf: await token(now + 2000) }, o.identity), 403, "too far ahead");
    expect((await submitConsent({ ...o.form, csrf: await token(now + 600) }, o.identity)).status).toBe(303);
  });

  it("refuses an approval the browser says was not sent from the page", async () => {
    const o = await opened();
    for (const [what, headers] of [
      ["Sec-Fetch-Site: cross-site", { "Sec-Fetch-Site": "cross-site" }],
      ["Sec-Fetch-Site: same-site", { "Sec-Fetch-Site": "same-site" }],
      ["Sec-Fetch-Site: none", { "Sec-Fetch-Site": "none" }],
      ["a foreign Origin", { Origin: "https://evil.example" }],
      ["a malformed Origin", { Origin: "nonsense" }],
      ["Sec-Fetch-Mode: cors", { "Sec-Fetch-Mode": "cors" }],
      ["Sec-Fetch-Dest: iframe", { "Sec-Fetch-Dest": "iframe" }],
    ] as const) {
      const html = await expectPage(await submitConsent(o.form, o.identity, { headers }), 403, what);
      expect(html, what).toContain("This approval was not sent from the Backyard page");
    }
    // Without Fetch Metadata (older browsers) and with Origin: null (no-referrer) the token decides.
    const plain = await call("/__backyard/auth/authorize", {
      method: "POST",
      headers: { ...o.identity, "Content-Type": "application/x-www-form-urlencoded", Origin: "null" },
      body: new URLSearchParams({ ...o.form, decision: "allow" }).toString(),
    }, MCP_ENV);
    expect(plain.status).toBe(303);
  });

  it("rejects unreadable forms and unknown decisions with a page", async () => {
    const o = await opened();
    await expectPage(await submitConsent(o.form, o.identity, { decision: "maybe" }), 400, "an unknown decision");
    await expectPage(await submitConsent(o.form, o.identity, { headers: { "Content-Type": "application/json" } }), 400, "not a form");
    const big = await call("/__backyard/auth/authorize", { method: "POST", headers: { ...o.identity, "Content-Type": "application/x-www-form-urlencoded" }, body: "x=" + "a".repeat(17 * 1024) }, MCP_ENV);
    await expectPage(big, 400, "over 16 KiB");
    const repeated = await call("/__backyard/auth/authorize", {
      method: "POST",
      headers: { ...o.identity, "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ ...o.form, decision: "allow" }).toString() + "&state=x",
    }, MCP_ENV);
    expect(await expectPage(repeated, 400, "a repeated field")).toContain("Invalid authorization request");
    const malformed = await call("/__backyard/auth/authorize", {
      method: "POST",
      headers: { ...o.identity, "Content-Type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({ ...o.form, decision: "allow" }).toString() + "&x=%zz",
    }, MCP_ENV);
    expect(await expectPage(malformed, 400, "a malformed %-escape")).toContain("Invalid authorization request");
  });

  it("redirects tampered request fields like the GET, with 303", async () => {
    const o = await opened();
    expectErrorRedirect(await submitConsent({ ...o.form, resource: "https://evil.example/__backyard/mcp" }, o.identity), 303, "invalid_target", "st-1", "a foreign resource");
    expectErrorRedirect(await submitConsent({ ...o.form, code_challenge_method: "plain" }, o.identity), 303, "invalid_request", "st-1", "plain");
    const { resource: _r, ...noResource } = o.form;
    void _r;
    expectErrorRedirect(await submitConsent(noResource, o.identity), 303, "invalid_request", "st-1", "no resource");
    const { response_type: _t, ...noType } = o.form;
    void _t;
    expectErrorRedirect(await submitConsent(noType, o.identity), 303, "unsupported_response_type", "st-1", "no response_type");
    expect(await expectPage(await submitConsent({ ...o.form, client_id: tamper(o.form.client_id!) }, o.identity), 400, "a tampered client_id")).toContain("Unknown app");
  });

  it("Cancel answers access_denied; Allow answers 303 with code, state and iss", async () => {
    const o = await opened();
    expectErrorRedirect(await submitConsent(o.form, o.identity, { decision: "deny" }), 303, "access_denied", "st-1", "Cancel");
    const r = await submitConsent(o.form, o.identity);
    expect(r.status).toBe(303);
    expect(r.headers.get("Cache-Control")).toContain("no-store");
    expect(r.headers.get("Referrer-Policy")).toBe("no-referrer");
    expect(r.headers.getSetCookie()).toEqual([]);
    const loc = r.headers.get("Location")!;
    expect(loc).toMatch(/^http:\/\/127\.0\.0\.1:9\/callback\?code=byac_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+&state=st-1&iss=https%3A%2F%2Fbackyard\.test$/);
    const code = redirectParams(r).code!;
    const p = payloadOf(code);
    expect(Object.keys(p)).toEqual(["g", "x"]);
    expect(p.g).toMatch(/^[0-9a-f]{32}$/);
    expect((p.x as number) - Math.floor(Date.now() / 1000)).toBeGreaterThan(290);
    // The pending grant exists, unredeemed, with the request.
    const g = (await registry(testEnv()).mcpGrantGet(p.g as string)) as { ok: true; value: McpGrantRecord };
    const me = await whoami(o.identity);
    expect(g.value).toMatchObject({
      id: p.g,
      user_id: me.id,
      user: { id: me.id, name: me.name, anonymous: true },
      client_id: o.clientId,
      client_name: "Test client",
      client_kind: "registered",
      client_host: "",
      redirect_uri: LOOPBACK,
      code_challenge: o.challenge,
      scope: "mcp offline_access",
      resource: MCP_RESOURCE,
      code_exp: p.x,
      redeemed_at: null,
    });
    expect(g.value.exp).toBe(g.value.code_exp + 600);
    expect(g.value.code_exp).toBe(g.value.created_at + 300);
  });
});

describe("the token endpoint (SPEC §8.4.6)", () => {
  it("exchanges a code for tokens", async () => {
    const a = await authorize();
    const r = await exchange(a, { redirect_uri: LOOPBACK, resource: MCP_RESOURCE + "/" });
    expect(r.status).toBe(200);
    expect(r.headers.get("Content-Type")).toBe("application/json");
    expect(r.headers.get("Cache-Control")).toBe("no-store");
    expect(r.headers.get("Pragma")).toBe("no-cache");
    expect(r.headers.getSetCookie()).toEqual([]);
    const t = (await r.json()) as Record<string, unknown>;
    expect(Object.keys(t)).toEqual(["access_token", "token_type", "expires_in", "refresh_token", "scope"]);
    expect(t.token_type).toBe("Bearer");
    expect(t.expires_in).toBeGreaterThan(3590);
    expect(t.expires_in).toBeLessThanOrEqual(3600);
    expect(t.scope).toBe("mcp offline_access");
    expect(t.access_token).toMatch(/^byat_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
    expect(t.refresh_token).toMatch(/^byrt_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/);
    const gid = payloadOf(a.code).g;
    expect(payloadOf(t.refresh_token as string)).toEqual({ g: gid, n: 1 });
    const at = payloadOf(t.access_token as string);
    expect(Object.keys(at)).toEqual(["g", "i", "x"]);
    expect(at.g).toBe(gid);
    expect((at.x as number) - (at.i as number)).toBe(3600);
    // The grant is redeemed; its family runs TOKEN_TTL_HOURS from consent.
    const g = ((await registry(testEnv()).mcpGrantGet(gid as string)) as { ok: true; value: McpGrantRecord }).value;
    expect(g.redeemed_at).toBeGreaterThan(0);
    expect(g.exp).toBe(g.created_at + 720 * 3600);
    const v = await verify(t.access_token as string);
    expect(v.ok).toBe(true);
    // Basic with an empty password (and no client_id in the body) is a public client too.
    const b = await authorize();
    const basic = await tokenRequest(
      { grant_type: "authorization_code", code: b.code, code_verifier: b.verifier },
      { headers: { Authorization: "Basic " + btoa(encodeURIComponent(b.clientId) + ":") } },
    );
    expect(basic.status).toBe(200);
  });

  it("caps expires_in at the family's end", async () => {
    const env = { TOKEN_TTL_HOURS: "0" }; // the 60 s minimum
    const c = await mcpConnect({ env });
    expect(c.expires_in).toBeGreaterThan(50);
    expect(c.expires_in).toBeLessThanOrEqual(60);
    expect(payloadOf(c.access_token).x).toBeLessThanOrEqual(Math.floor(Date.now() / 1000) + 60);
  });

  it("keeps the code after a wrong verifier or redirect_uri, and allows leaving redirect_uri out", async () => {
    const a = await authorize();
    for (const [what, extra] of [
      ["another verifier", { code_verifier: (await pkcePair()).verifier }],
      ["a malformed verifier", { code_verifier: "abc" }],
      ["a verifier with a bad character", { code_verifier: a.verifier.slice(0, 42) + "!" }],
      ["another redirect_uri", { redirect_uri: "http://127.0.0.1:10/callback" }],
      ["an empty redirect_uri", { redirect_uri: "" }],
    ] as const) {
      await expectOAuthError(await exchange(a, extra), 400, "invalid_grant", what);
    }
    expect((await exchange(a)).status, "then without redirect_uri").toBe(200);
  });

  it("refuses malformed requests and authenticates public clients only", async () => {
    const a = await authorize();
    const other = await registerClient({ name: "Other client" });
    const base = { grant_type: "authorization_code", client_id: a.clientId, code: a.code, code_verifier: a.verifier, redirect_uri: LOOPBACK };
    const raw = (body: string, headers: Record<string, string>) => call("/__backyard/oauth/token", { method: "POST", headers, body }, MCP_ENV);
    const form = { "Content-Type": "application/x-www-form-urlencoded" };
    const cases: [string, Promise<Response>, number, string][] = [
      ["a foreign resource", tokenRequest({ ...base, resource: "https://evil.example/__backyard/mcp" }), 400, "invalid_target"],
      ["an empty resource", tokenRequest({ ...base, resource: "" }), 400, "invalid_target"],
      ["a JSON body", raw(JSON.stringify(base), { "Content-Type": "application/json" }), 400, "invalid_request"],
      ["a repeated code", raw(new URLSearchParams(base).toString() + "&code=" + a.code, form), 400, "invalid_request"],
      // Go's url.ParseQuery refuses these, so both editions do.
      ["a malformed %-escape", raw(new URLSearchParams(base).toString() + "&pad=%zz", form), 400, "invalid_request"],
      ["a ; separator", raw(new URLSearchParams(base).toString() + "&pad=a;b", form), 400, "invalid_request"],
      ["over 16 KiB", raw(new URLSearchParams({ ...base, pad: "x".repeat(17 * 1024) }).toString(), form), 400, "invalid_request"],
      ["a client_secret", tokenRequest({ ...base, client_secret: "x" }), 401, "invalid_client"],
      ["Basic with a password", tokenRequest(base, { headers: { Authorization: "Basic " + btoa(a.clientId + ":x") } }), 401, "invalid_client"],
      ["Basic without a colon", tokenRequest(base, { headers: { Authorization: "Basic " + btoa(a.clientId) } }), 401, "invalid_client"],
      ["Basic that is not base64", tokenRequest(base, { headers: { Authorization: "Basic !!!" } }), 401, "invalid_client"],
      // As Go's Request.BasicAuth: one space after the scheme, padded base64.
      ["Basic without base64 padding", tokenRequest(base, { headers: { Authorization: unpaddedBasic(a.clientId) } }), 401, "invalid_client"],
      ["Basic followed by two spaces", tokenRequest(base, { headers: { Authorization: "Basic  " + btoa(a.clientId + ":") } }), 401, "invalid_client"],
      ["Basic followed by a tab", tokenRequest(base, { headers: { Authorization: "Basic\t" + btoa(a.clientId + ":") } }), 401, "invalid_client"],
      // A BOM stays part of the first parameter's name (as on Go), so grant_type is missing.
      ["a UTF-8 BOM before the form (no grant_type)", raw("\ufeff" + new URLSearchParams(base).toString(), form), 400, "invalid_request"],
      ["a Bearer Authorization", tokenRequest(base, { headers: { Authorization: "Bearer x" } }), 401, "invalid_client"],
      ["Basic naming another client", tokenRequest(base, { headers: { Authorization: "Basic " + btoa(other.client_id + ":") } }), 400, "invalid_request"],
      ["a tampered client_id", tokenRequest({ ...base, client_id: tamper(a.clientId) }), 401, "invalid_client"],
      ["a CIMD URL that is not allowlisted", tokenRequest({ ...base, client_id: "https://example.com/x" }), 401, "invalid_client"],
      ["no client_id", tokenRequest({ grant_type: "authorization_code", code: a.code, code_verifier: a.verifier }), 401, "invalid_client"],
      ["another client's code", tokenRequest({ ...base, client_id: other.client_id }), 400, "invalid_grant"],
      ["no code_verifier", tokenRequest({ grant_type: "authorization_code", client_id: a.clientId, code: a.code }), 400, "invalid_request"],
      ["no code", tokenRequest({ grant_type: "authorization_code", client_id: a.clientId, code_verifier: a.verifier }), 400, "invalid_request"],
      ["no grant_type", tokenRequest({ client_id: a.clientId, code: a.code }), 400, "invalid_request"],
      ["an unknown grant_type", tokenRequest({ ...base, grant_type: "password" }), 400, "unsupported_grant_type"],
      ["client_credentials", tokenRequest({ ...base, grant_type: "client_credentials" }), 400, "unsupported_grant_type"],
      ["a forged code", tokenRequest({ ...base, code: tamper(a.code) }), 400, "invalid_grant"],
      ["a code signed with another key", tokenRequest({ ...base, code: await forge("byac_", KeyLabel.MCPRefresh, payloadOf(a.code)) }), 400, "invalid_grant"],
      ["an expired code", tokenRequest({ ...base, code: await forge("byac_", KeyLabel.MCPCode, { ...payloadOf(a.code), x: Math.floor(Date.now() / 1000) - 1 }) }), 400, "invalid_grant"],
      ["a code with an extra field", tokenRequest({ ...base, code: await forge("byac_", KeyLabel.MCPCode, { ...payloadOf(a.code), y: 1 }) }), 400, "invalid_grant"],
      ["a code of an unknown grant", tokenRequest({ ...base, code: await forge("byac_", KeyLabel.MCPCode, { g: "0".repeat(32), x: Math.floor(Date.now() / 1000) + 100 }) }), 400, "invalid_grant"],
    ];
    for (const [what, pending, status, error] of cases) {
      const r = await pending;
      await expectOAuthError(r, status, error, what);
      if (status === 401 && /Basic|Bearer|client_secret/.test(what)) expect(r.headers.get("WWW-Authenticate"), what).toBe('Basic realm="Backyard"');
      if (status === 401 && /tampered|not allowlisted|no client_id/.test(what)) expect(r.headers.get("WWW-Authenticate"), what).toBeNull();
    }
    // None of these used the code up.
    expect((await tokenRequest(base)).status).toBe(200);
  });

  it("revokes the connection when a valid code is replayed", async () => {
    const a = await authorize();
    const first = (await (await exchange(a)).json()) as { access_token: string; refresh_token: string };
    expect((await verify(first.access_token)).ok).toBe(true);
    const warn = vi.spyOn(console, "warn");
    await expectOAuthError(await exchange(a), 400, "invalid_grant", "the replay");
    expect(warn.mock.calls.some((c) => String(c[0]).includes('"action":"mcp.code_replay"'))).toBe(true);
    warn.mockRestore();
    expect(await verify(first.access_token)).toEqual({ ok: false, error: "token revoked" });
    await expectOAuthError(await refresh(a.clientId, first.refresh_token), 400, "invalid_grant", "the refresh token after the replay");
    // A replay without the verifier revokes nothing (PKCE first).
    const b = await authorize();
    const t = (await (await exchange(b)).json()) as { access_token: string };
    await expectOAuthError(await exchange({ ...b, verifier: (await pkcePair()).verifier }), 400, "invalid_grant", "a replay without the verifier");
    expect((await verify(t.access_token)).ok).toBe(true);
  });

  it("redeems a code exactly once under concurrency", async () => {
    const a = await authorize();
    const results = await Promise.all(Array.from({ length: 20 }, () => exchange(a)));
    const ok = results.filter((r) => r.status === 200);
    expect(ok).toHaveLength(1);
    for (const r of results.filter((x) => x.status !== 200)) await expectOAuthError(r, 400, "invalid_grant", "a concurrent redemption");
    // The replays revoked the grant.
    const t = (await ok[0]!.json()) as { access_token: string };
    expect(await verify(t.access_token)).toEqual({ ok: false, error: "token revoked" });
  });
});

describe("refresh tokens (SPEC §8.4.6, §8.4.8)", () => {
  it("rotates, answers the grace window with the identical successor and revokes on reuse", async () => {
    const c = await mcpConnect();
    const R1 = c.refresh_token;
    const t2 = (await (await refresh(c.clientId, R1)).json()) as { refresh_token: string; access_token: string; scope: string; expires_in: number };
    const R2 = t2.refresh_token;
    expect(R2).not.toBe(R1);
    expect(payloadOf(R2).n).toBe(2);
    expect(t2.scope).toBe("mcp offline_access");
    expect(t2.expires_in).toBeGreaterThan(3590);
    // R1 again at once: the same R2 (another request may have lost the answer).
    const again = (await (await refresh(c.clientId, R1)).json()) as { refresh_token: string };
    expect(again.refresh_token).toBe(R2);
    const R3 = ((await (await refresh(c.clientId, R2)).json()) as { refresh_token: string }).refresh_token;
    expect(payloadOf(R3).n).toBe(3);
    // R1 once R3 exists is an old generation: the grant is revoked.
    await expectOAuthError(await refresh(c.clientId, R1), 400, "invalid_grant", "R1 after R3");
    await expectOAuthError(await refresh(c.clientId, R3), 400, "invalid_grant", "R3 after the reuse");
    expect(await verify(t2.access_token)).toEqual({ ok: false, error: "token revoked" });
  });

  it("answers two concurrent refreshes of one token with the same successor", async () => {
    const c = await mcpConnect();
    const [a, b] = await Promise.all([refresh(c.clientId, c.refresh_token), refresh(c.clientId, c.refresh_token)]);
    expect([a!.status, b!.status]).toEqual([200, 200]);
    const ra = ((await a!.json()) as { refresh_token: string }).refresh_token;
    const rb = ((await b!.json()) as { refresh_token: string }).refresh_token;
    expect(ra).toBe(rb);
    expect((await refresh(c.clientId, ra)).status).toBe(200);
    // Fifty concurrent refreshes of the newest token: all ok or grace, none a reuse.
    const c2 = await mcpConnect();
    const all = await Promise.all(Array.from({ length: 50 }, () => refresh(c2.clientId, c2.refresh_token)));
    expect(all.map((r) => r.status)).toEqual(Array(50).fill(200));
    const tokens = new Set(await Promise.all(all.map(async (r) => ((await r.json()) as { refresh_token: string }).refresh_token)));
    expect(tokens.size).toBe(1);
  });

  it("keeps one row per grant, which holds its newest generation", async () => {
    const e = testEnv({ ...MCP_ENV });
    const sql = <T>(q: string, ...args: unknown[]) =>
      runInDurableObject(e.REGISTRY.get(e.REGISTRY.idFromName("registry")), (_o, st) => st.storage.sql.exec(q, ...args).toArray() as T[]);
    const a = await authorize();
    const first = (await (await exchange(a)).json()) as { refresh_token: string };
    const gid = payloadOf(first.refresh_token).g as string;
    const rts = ["", first.refresh_token]; // rts[n] = RT(n)
    for (let n = 1; n <= 20; n++) rts.push(((await (await refresh(a.clientId, rts[n]!)).json()) as { refresh_token: string }).refresh_token);
    expect(await sql<{ gen: number }>("SELECT gen FROM mcp_grants WHERE id = ?", gid)).toEqual([{ gen: 21 }]);
    expect(await sql("SELECT name FROM sqlite_master WHERE name LIKE 'mcp_rot%'")).toEqual([]);
    // RT(20) again within the grace window: the same RT(21).
    expect(((await (await refresh(a.clientId, rts[20]!)).json()) as { refresh_token: string }).refresh_token).toBe(rts[21]);
    // The code again: a replay.
    await expectOAuthError(await exchange(a), 400, "invalid_grant", "a replayed code");
    expect(await registry(testEnv()).mcpGrantGet(gid)).toEqual({ ok: true, value: null });
    // An old generation is a reuse.
    const c = await mcpConnect();
    let r = c.refresh_token;
    for (let i = 0; i < 3; i++) r = ((await (await refresh(c.clientId, r)).json()) as { refresh_token: string }).refresh_token;
    await expectOAuthError(await refresh(c.clientId, c.refresh_token), 400, "invalid_grant", "RT(1) after RT(4)");
    await expectOAuthError(await refresh(c.clientId, r), 400, "invalid_grant", "RT(4) after the reuse");
    // Pruning uses the expiry index and is bounded.
    const plan = await sql<{ detail: string }>("EXPLAIN QUERY PLAN SELECT id FROM mcp_grants WHERE exp < ? LIMIT 500", 0);
    expect(plan.map((p) => p.detail).join(" ")).toMatch(/mcp_grants_exp/);
  });

  it("revokes a token re-presented after the 60 s grace window", async () => {
    const c = await mcpConnect();
    const gid = payloadOf(c.refresh_token).g as string;
    expect((await refresh(c.clientId, c.refresh_token)).status).toBe(200);
    const g = ((await registry(testEnv()).mcpGrantGet(gid)) as { ok: true; value: McpGrantRecord }).value;
    const now = Math.floor(Date.now() / 1000);
    expect(await registry(testEnv()).mcpRotate(gid, 1, now + 61, true, g.exp)).toEqual({ ok: true, value: "reuse" });
    expect(((await registry(testEnv()).mcpGrantGet(gid)) as { ok: true; value: unknown }).value).toBeNull();
    await expectOAuthError(await refresh(c.clientId, c.refresh_token), 400, "invalid_grant", "after the revocation");
  });

  it("refuses forged and foreign refresh tokens without revoking anything", async () => {
    const c = await mcpConnect();
    const other = await registerClient({ name: "Other client" });
    for (const [what, rt, clientId] of [
      ["a forged MAC", tamper(c.refresh_token), c.clientId],
      ["an unknown grant", await forge("byrt_", KeyLabel.MCPRefresh, { g: "f".repeat(32), n: 1 }), c.clientId],
      ["generation 0", await forge("byrt_", KeyLabel.MCPRefresh, { ...payloadOf(c.refresh_token), n: 0 }), c.clientId],
      ["an access token", c.access_token, c.clientId],
      ["another key", await forge("byrt_", KeyLabel.MCPAccess, payloadOf(c.refresh_token)), c.clientId],
      ["another client", c.refresh_token, other.client_id],
    ] as const) {
      await expectOAuthError(await refresh(clientId, rt), 400, "invalid_grant", what);
    }
    await expectOAuthError(await tokenRequest({ grant_type: "refresh_token", client_id: c.clientId }), 400, "invalid_request", "no refresh_token");
    expect((await refresh(c.clientId, c.refresh_token)).status).toBe(200);
  });

  it("narrows but never widens the scope", async () => {
    const c = await mcpConnect({ scope: "mcp" });
    expect(c.scope).toBe("mcp");
    await expectOAuthError(await refresh(c.clientId, c.refresh_token, { scope: "mcp offline_access" }), 400, "invalid_scope", "widening");
    const r = await refresh(c.clientId, c.refresh_token, { scope: "mcp" });
    expect(((await r.json()) as { scope: string }).scope).toBe("mcp");
    const d = await mcpConnect();
    const n = await refresh(d.clientId, d.refresh_token, { scope: "offline_access offline_access" });
    expect(((await n.json()) as { scope: string }).scope).toBe("offline_access");
    await expectOAuthError(await refresh(d.clientId, d.refresh_token, { resource: "https://evil.example/__backyard/mcp" }), 400, "invalid_target", "a foreign resource");
  });

  it("re-gates the user: a guest's connection ends once the mode is not none", async () => {
    const c = await mcpConnect();
    const access = { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: "aud" };
    await expectOAuthError(await refresh(c.clientId, c.refresh_token, {}, access), 400, "invalid_grant", "mode access");
    // The grant is gone, also once the mode is none again.
    await expectOAuthError(await refresh(c.clientId, c.refresh_token), 400, "invalid_grant", "mode none again");
    expect(await verify(c.access_token)).toEqual({ ok: false, error: "token revoked" });
    // At the code exchange too.
    const a = await authorize();
    const b = await expectOAuthError(await exchange(a, {}, access), 400, "invalid_grant", "exchange in mode access");
    expect(b.error_description).toBe("access removed by policy");
  });

  it("ends connections whose redirect address the policy no longer allows", async () => {
    const claude = "https://claude.ai/api/mcp/auth_callback";
    const c = await mcpConnect({ redirectURI: claude });
    const b = await expectOAuthError(await refresh(c.clientId, c.refresh_token, {}, { MCP_REDIRECT_URIS: "https://claude.com/api/mcp/auth_callback" }), 400, "invalid_grant", "the address removed");
    expect(b.error_description).toBe("the app's redirect address is no longer allowed on this server");
    expect((await refresh(c.clientId, c.refresh_token)).status).toBe(200);
    // A loopback connection is unaffected.
    const l = await mcpConnect();
    expect((await refresh(l.clientId, l.refresh_token, {}, { MCP_REDIRECT_URIS: "" })).status).toBe(200);
  });
});

describe("revocation (SPEC §8.4.7)", () => {
  const revoke = (fields: Record<string, string>, env: Partial<Env> = {}) => tokenRequest(fields, { path: "/__backyard/oauth/revoke", env });

  it("revokes the grant of a refresh or an access token of the client", async () => {
    const a = await mcpConnect();
    const r = await revoke({ client_id: a.clientId, token: a.refresh_token, token_type_hint: "access_token" });
    expect(r.status).toBe(200);
    expect(await r.text()).toBe("");
    expect(r.headers.get("Cache-Control")).toBe("no-store");
    expect(r.headers.getSetCookie()).toEqual([]);
    await expectOAuthError(await refresh(a.clientId, a.refresh_token), 400, "invalid_grant", "after revoking the refresh token");
    expect(await verify(a.access_token)).toEqual({ ok: false, error: "token revoked" });
    const b = await mcpConnect();
    expect((await revoke({ client_id: b.clientId, token: b.access_token })).status).toBe(200);
    expect(await verify(b.access_token)).toEqual({ ok: false, error: "token revoked" });
  });

  it("answers 200 for anything else and revokes nothing", async () => {
    const c = await mcpConnect();
    const intruder = await registerClient({ name: "Intruder" });
    for (const [what, fields] of [
      ["another client's token", { client_id: intruder.client_id, token: c.refresh_token }],
      ["an unknown token", { client_id: c.clientId, token: await forge("byrt_", KeyLabel.MCPRefresh, { g: "0".repeat(32), n: 1 }) }],
      ["garbage", { client_id: c.clientId, token: "garbage" }],
      ["a code", { client_id: c.clientId, token: "byac_x.y" }],
      ["a forged token", { client_id: c.clientId, token: tamper(c.refresh_token) }],
    ] as const) {
      const r = await revoke(fields);
      expect(r.status, what).toBe(200);
      expect(await r.text(), what).toBe("");
    }
    expect((await refresh(c.clientId, c.refresh_token)).status).toBe(200);
    await expectOAuthError(await revoke({ client_id: c.clientId }), 400, "invalid_request", "no token");
    await expectOAuthError(await revoke({ client_id: c.clientId, token: "x", client_secret: "s" }), 401, "invalid_client", "a client_secret");
    await expectOAuthError(await revoke({ token: "x" }), 401, "invalid_client", "no client");
  });
});

describe("access tokens (SPEC §8.4.8)", () => {
  it("names each reason a token does not authenticate", async () => {
    const c = await mcpConnect();
    const gid = payloadOf(c.access_token).g as string;
    const now = Math.floor(Date.now() / 1000);
    const api = await issueAPIToken(SECRET, { id: "carol@acme.com", email: "carol@acme.com", name: "Carol", anonymous: false, admin: false }, 3600);
    for (const [what, tok] of [
      ["garbage", "garbage"],
      ["empty", ""],
      ["a Backyard API token", api.token],
      ["DEPLOY_TOKEN", DEPLOY_TOKEN],
      ["a refresh token", c.refresh_token],
      ["a forged MAC", tamper(c.access_token)],
      ["over 4096 bytes", await forge("byat_", KeyLabel.MCPAccess, { g: gid, i: now, x: now + 60, pad: "x".repeat(4100) })],
      ["an extra field", await forge("byat_", KeyLabel.MCPAccess, { g: gid, i: now, x: now + 60, y: 1 })],
      ["another key", await forge("byat_", KeyLabel.MCPRefresh, payloadOf(c.access_token))],
      ["no prefix", c.access_token.slice(5)],
    ] as const) {
      expect(await verify(tok), what).toEqual({ ok: false, error: "malformed token" });
    }
    expect(await verify(await forge("byat_", KeyLabel.MCPAccess, { g: gid, i: now - 3600, x: now }))).toEqual({ ok: false, error: "token expired" });
    expect(await verify(await forge("byat_", KeyLabel.MCPAccess, { g: "a".repeat(32), i: now, x: now + 60 }))).toEqual({ ok: false, error: "token revoked" });
    // A pending (unredeemed) grant is not live.
    const a = await authorize();
    expect(await verify(await forge("byat_", KeyLabel.MCPAccess, { g: payloadOf(a.code).g, i: now, x: now + 60 }))).toEqual({ ok: false, error: "token revoked" });
    // Another hub URL means another resource.
    expect(await verify(c.access_token, { PUBLIC_URL: "https://other.example" })).toEqual({ ok: false, error: "token not issued for this server" });
    const ok = await verify(c.access_token);
    expect(ok.ok).toBe(true);
    if (ok.ok) {
      const me = await whoami(c.identity);
      expect(ok.user).toEqual({ id: me.id, name: me.name, anonymous: true, admin: false });
      expect(ok.grant).toMatchObject({ id: gid, user_id: me.id, client_id: c.clientId, client_kind: "registered", scope: "mcp offline_access", resource: MCP_RESOURCE });
      expect(ok.grant).not.toHaveProperty("code_challenge");
    }
    // A policy failure deletes the grant.
    expect(await verify(c.access_token, { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: "aud" })).toEqual({ ok: false, error: "access removed by policy" });
    expect(await verify(c.access_token)).toEqual({ ok: false, error: "token revoked" });
  });

  it("is no API credential", async () => {
    const c = await mcpConnect();
    for (const path of ["/__backyard/api/me", "/__backyard/api/deploy/me", "/__backyard/api/mcp/grants", "/s/demo/__backyard/api/me"]) {
      const r = await call(path, { headers: { Authorization: `Bearer ${c.access_token}` } }, MCP_ENV);
      expect(r.status, path).toBe(401);
    }
  });
});

describe("the grants API (SPEC §8.4.7)", () => {
  const grants = (identity: Record<string, string>, query = "", env: Partial<Env> = {}) => call(`/__backyard/api/mcp/grants${query}`, { headers: identity }, { ...MCP_ENV, ...env });
  const del = (identity: Record<string, string>, path = "", headers: Record<string, string> = { "X-Backyard": "1" }) =>
    call(`/__backyard/api/mcp/grants${path}`, { method: "DELETE", headers: { ...identity, ...headers } }, MCP_ENV);
  const admin = { Authorization: `Bearer ${DEPLOY_TOKEN}` };

  it("lists a person's live connections, newest first", async () => {
    const browser = await guest();
    const me = await whoami(browser);
    const c1 = await mcpConnect({ identity: browser, name: "First" });
    const c2 = await mcpConnect({ identity: browser, name: "Second", scope: "mcp" });
    await authorize({ identity: browser }); // pending: not listed
    const r = await grants(browser);
    expect(r.status).toBe(200);
    const list = ((await r.json()) as { grants: Record<string, unknown>[] }).grants;
    expect(list.map((g) => g.client_name).sort()).toEqual(["First", "Second"]);
    const times = list.map((g) => Date.parse(g.created_at as string));
    expect(times).toEqual([...times].sort((x, y) => y - x));
    const g = list.find((x) => x.client_name === "First")!;
    expect(Object.keys(g)).toEqual(["id", "user", "client_name", "client_kind", "client_host", "redirect_host", "scope", "created_at", "expires_at"]);
    expect(g).toMatchObject({ id: payloadOf(c1.access_token).g, user: { id: me.id, email: "", name: me.name }, client_kind: "registered", client_host: "", redirect_host: "127.0.0.1:9", scope: "mcp offline_access" });
    expect(g.created_at).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
    expect(Date.parse(g.expires_at as string) - Date.parse(g.created_at as string)).toBe(720 * 3600 * 1000);
    expect(list.find((x) => x.client_name === "Second")!.scope).toBe("mcp");
    expect(c2.scope).toBe("mcp");
    // Someone else sees none of them; their own list is theirs.
    const other = await guest();
    expect(((await (await grants(other)).json()) as { grants: unknown[] }).grants).toEqual([]);
    expect((await grants(other, `?user=${encodeURIComponent(me.id)}`)).status).toBe(403);
    // ?user= on GET is for admins only, even naming oneself (SPEC §8.4.7).
    expect((await grants(browser, `?user=${encodeURIComponent(me.id)}`)).status).toBe(403);
    // An admin may list anyone's; a static token owns none.
    const forAdmin = ((await (await grants(admin, `?user=${encodeURIComponent(me.id)}`)).json()) as { grants: { id: string }[] }).grants;
    expect(forAdmin.map((x) => x.id).sort()).toEqual(list.map((x) => x.id as string).sort());
    expect(((await (await grants(admin)).json()) as { grants: unknown[] }).grants).toEqual([]);
    // An MCP access token is no credential here.
    expect((await grants({ Authorization: `Bearer ${c1.access_token}` })).status).toBe(401);
    // A non-admin API token user.
    const api = await issueAPIToken(SECRET, { id: "carol@acme.com", email: "carol@acme.com", name: "Carol", anonymous: false, admin: false }, 3600);
    expect((await grants({ Authorization: `Bearer ${api.token}` }, `?user=${encodeURIComponent(me.id)}`)).status).toBe(403);
    expect((await grants({ Authorization: `Bearer ${api.token}` }, `?user=${encodeURIComponent(me.id)}`, { ADMINS: "carol@acme.com" })).status).toBe(200);
  });

  it("disconnects one connection for its owner or an admin", async () => {
    const browser = await guest();
    const c = await mcpConnect({ identity: browser });
    const id = payloadOf(c.access_token).g as string;
    expect((await del(browser, `/${id}`, {})).status, "without X-Backyard").toBe(403);
    const other = await guest();
    expect((await del(other, `/${id}`)).status, "someone else").toBe(404);
    expect((await verify(c.access_token)).ok).toBe(true);
    const log = vi.spyOn(console, "log");
    const r = await del(browser, `/${id}`);
    expect(r.status).toBe(204);
    expect(await r.text()).toBe("");
    const line = log.mock.calls.map((x) => String(x[0])).find((x) => x.includes('"action":"mcp.revoke"'));
    log.mockRestore();
    expect(JSON.parse(line!)).toMatchObject({ audit: true, action: "mcp.revoke", via: "hub", grant: id.slice(0, 8), client_name: "Test client", redirect_host: "127.0.0.1:9" });
    expect(line).not.toContain(id);
    expect(await verify(c.access_token)).toEqual({ ok: false, error: "token revoked" });
    await expectOAuthError(await refresh(c.clientId, c.refresh_token), 400, "invalid_grant", "the refresh token after Disconnect");
    expect((await del(browser, `/${id}`)).status, "again").toBe(404);
    expect((await del(browser, "/nope")).status).toBe(404);
    expect((await call(`/__backyard/api/mcp/grants/${id}`, { headers: browser }, MCP_ENV)).status).toBe(405);
    // An admin may disconnect anyone's.
    const d = await mcpConnect();
    expect((await del(admin, `/${payloadOf(d.access_token).g}`)).status).toBe(204);
    expect(await verify(d.access_token)).toEqual({ ok: false, error: "token revoked" });
  });

  it("disconnects all of a person's connections", async () => {
    const browser = await guest();
    const me = await whoami(browser);
    const a = await mcpConnect({ identity: browser });
    const b = await mcpConnect({ identity: browser });
    await authorize({ identity: browser }); // pending ones go too
    const other = await guest();
    expect((await del(other, `?user=${encodeURIComponent(me.id)}`)).status).toBe(403);
    expect((await del(browser, "", {})).status).toBe(403);
    const r = await del(browser);
    expect(r.status).toBe(200);
    expect(await r.json()).toEqual({ revoked: 3 });
    for (const c of [a, b]) expect(await verify(c.access_token)).toEqual({ ok: false, error: "token revoked" });
    expect(((await (await grants(browser)).json()) as { grants: unknown[] }).grants).toEqual([]);
    const c = await mcpConnect({ identity: browser });
    const byAdmin = await del(admin, `?user=${encodeURIComponent(me.id)}`);
    expect(await byAdmin.json()).toEqual({ revoked: 1 });
    expect(await verify(c.access_token)).toEqual({ ok: false, error: "token revoked" });
    // Naming oneself is fine on DELETE.
    const d = await mcpConnect({ identity: browser });
    expect(await (await del(browser, `?user=${encodeURIComponent(me.id)}`)).json()).toEqual({ revoked: 1 });
    expect(await verify(d.access_token)).toEqual({ ok: false, error: "token revoked" });
  });

  it("prunes expired grants when a grant is created", async () => {
    const reg = registry(testEnv());
    const now = Math.floor(Date.now() / 1000);
    const old: McpGrantRecord = {
      id: "e".repeat(31) + "1",
      user_id: "anon:0000000000000000",
      user: { id: "anon:0000000000000000", name: "Guest 0000", anonymous: true },
      client_id: "bydc_x",
      client_name: "Old",
      client_kind: "registered",
      client_host: "",
      redirect_uri: LOOPBACK,
      code_challenge: "c".repeat(43),
      scope: "mcp",
      resource: MCP_RESOURCE,
      created_at: now - 2000,
      code_exp: now - 1700,
      redeemed_at: null,
      exp: now - 1100,
    };
    expect(await reg.mcpGrantCreate(old)).toEqual({ ok: true, value: null });
    expect(((await reg.mcpGrantGet(old.id)) as { ok: true; value: unknown }).value).not.toBeNull();
    await authorize();
    expect(await reg.mcpGrantGet(old.id)).toEqual({ ok: true, value: null });
  });
});

describe("Cloudflare Access (AUTH_MODE=access)", () => {
  const TEAM = "acme.cloudflareaccess.com";
  const AUD = "aud-1234567890";
  const accessEnv: Partial<Env> = { ...MCP_ENV, AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: AUD, ADMIN_GROUPS: "platform" };

  async function setup() {
    const pair = (await crypto.subtle.generateKey(
      { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
      true,
      ["sign", "verify"],
    )) as CryptoKeyPair;
    const jwk = (await crypto.subtle.exportKey("jwk", pair.publicKey)) as JsonWebKey;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url === `https://${TEAM}/cdn-cgi/access/certs`) return Response.json({ keys: [{ ...jwk, kid: "k1", alg: "RS256", use: "sig" }] });
      if (url === `https://${TEAM}/cdn-cgi/access/get-identity`) return Response.json({ name: "Alice Smith", email: "alice@acme.com", groups: [{ id: "1", name: "eng" }, { id: "2", name: "platform" }] });
      throw new Error(`unexpected fetch ${url}`);
    });
    const sign = async (claims: Record<string, unknown>) => {
      const h = b64url(new TextEncoder().encode(JSON.stringify({ alg: "RS256", kid: "k1", typ: "JWT" })));
      const p = b64url(new TextEncoder().encode(JSON.stringify(claims)));
      const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", pair.privateKey, new TextEncoder().encode(`${h}.${p}`)));
      return `${h}.${p}.${b64url(sig)}`;
    };
    const now = Math.floor(Date.now() / 1000);
    const claims = { aud: [AUD], email: "Alice@Acme.com", sub: "u-1", iss: `https://${TEAM}`, iat: now, exp: now + 3600, identity_nonce: "n1" };
    return { sign, claims };
  }

  it("needs the Access identity at authorize", async () => {
    const { sign, claims } = await setup();
    const c = await registerClient({ env: accessEnv });
    const { challenge } = await pkcePair();
    const q = authorizeQuery({ clientId: c.client_id, challenge, state: "s" });
    const none = await call(`/__backyard/auth/authorize?${q}`, { headers: { "Sec-Fetch-Site": "cross-site" } }, accessEnv);
    expect(await expectPage(none, 401, "without Access")).toContain("/__backyard/auth/authorize must stay behind it");
    const bad = await call(`/__backyard/auth/authorize?${q}`, { headers: { "Cf-Access-Jwt-Assertion": "a.b.c" } }, accessEnv);
    await expectPage(bad, 401, "an invalid Access JWT");
    const alice = await sign(claims);
    const page = await openConsent(q, { "Cf-Access-Jwt-Assertion": alice }, accessEnv);
    expect(page.page.status).toBe(200);
    expect(page.html).toContain("You are signed in as <strong>alice@acme.com</strong>.");
    expect(page.html).toContain('<a href="/__backyard/auth/logout">Not you? Sign out</a>');
    expect(page.page.headers.getSetCookie()).toEqual([]);
    // Cookies of different users: the conflict page (signing in again would not help).
    const mallory = await sign({ ...claims, email: "mallory@acme.com", sub: "u-2" });
    const conflict = await call(`/__backyard/auth/authorize?${q}`, { headers: { "Cf-Access-Jwt-Assertion": alice, Cookie: `CF_Authorization=${mallory}` } }, accessEnv);
    expect(await expectPage(conflict, 401, "conflicting cookies")).toContain("more than one account");
  });

  it("connects an Access user, recomputes admin rights and binds the consent to the user", async () => {
    const { sign, claims } = await setup();
    const alice = { "Cf-Access-Jwt-Assertion": await sign(claims) };
    const c = await mcpConnect({ identity: alice, env: accessEnv });
    const v = await verify(c.access_token, accessEnv);
    expect(v.ok).toBe(true);
    if (v.ok) {
      expect(v.user).toEqual({ id: "alice@acme.com", email: "alice@acme.com", name: "Alice Smith", groups: ["eng", "platform"], anonymous: false, admin: true });
      expect(v.grant.user).toEqual({ id: "alice@acme.com", email: "alice@acme.com", name: "Alice Smith", groups: ["eng", "platform"], anonymous: false });
    }
    // Admin comes from the configuration, not the snapshot.
    const plain = await verify(c.access_token, { ...accessEnv, ADMIN_GROUPS: "" });
    expect(plain.ok && plain.user.admin).toBe(false);
    // An Access user's connection keeps working in mode none (like API tokens).
    expect((await verify(c.access_token)).ok).toBe(true);
    // Another Access user cannot submit Alice's consent page.
    const cl = await registerClient({ env: accessEnv });
    const { challenge } = await pkcePair();
    const o = await openConsent(authorizeQuery({ clientId: cl.client_id, challenge, state: "s" }), alice, accessEnv);
    const bob = { "Cf-Access-Jwt-Assertion": await sign({ ...claims, email: "bob@acme.com", sub: "u-3" }) };
    await expectPage(await submitConsent(o.form, bob, { env: accessEnv }), 403, "another Access user");
    expect((await submitConsent(o.form, alice, { env: accessEnv })).status).toBe(303);
    // The grants API in mode access.
    const list = await call("/__backyard/api/mcp/grants", { headers: alice }, accessEnv);
    expect(list.status).toBe(200);
    const gs = ((await list.json()) as { grants: { user: { id: string; email: string } }[] }).grants;
    expect(gs.length).toBeGreaterThanOrEqual(1);
    expect(gs[0]!.user).toEqual({ id: "alice@acme.com", email: "alice@acme.com", name: "Alice Smith" });
  });
});
