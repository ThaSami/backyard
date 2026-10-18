/** Test helpers: call the Worker in-process (inside workerd) and deploy sites. */
import { createExecutionContext, waitOnExecutionContext } from "cloudflare:test";
import { env } from "cloudflare:workers";
import worker from "../src/index";
import type { Env } from "../src/env";

export const ORIGIN = "https://backyard.test";
export const DEPLOY_TOKEN = "test-deploy-token";
export const SECRET = "test-secret-0123456789-abcdefghijklmnopqrstuvwxyz";

let counter = 0;

/** A unique, valid site name. */
export function uniqueSite(prefix = "t"): string {
  counter++;
  return `${prefix}-${Date.now().toString(36)}-${counter}-${Math.random().toString(36).slice(2, 7)}`;
}

/** The bindings of the test Worker (from wrangler.jsonc + vitest.config.ts). */
export function testEnv(overrides: Partial<Env> = {}): Env {
  return { ...(env as unknown as Env), ...overrides };
}

/** Calls the Worker with a request (path relative to ORIGIN or absolute URL). */
export async function call(path: string, init: RequestInit = {}, envOverrides: Partial<Env> = {}): Promise<Response> {
  const url = /^https?:\/\//.test(path) ? path : ORIGIN + path;
  const req = new Request(url, { redirect: "manual", ...init });
  const ctx = createExecutionContext();
  const res = await worker.fetch(req, testEnv(envOverrides), ctx);
  // Streaming responses are produced by waitUntil tasks that need a reader.
  if ((res.headers.get("Content-Type") ?? "").startsWith("text/event-stream")) return res;
  await waitOnExecutionContext(ctx);
  return res;
}

/** Headers for a JSON API write. */
export function apiHeaders(extra: Record<string, string> = {}): Record<string, string> {
  return { "Content-Type": "application/json", "X-Backyard": "1", ...extra };
}

/** Headers for gateway calls with the deploy token. */
export function tokenHeaders(token = DEPLOY_TOKEN, extra: Record<string, string> = {}): Record<string, string> {
  return { Authorization: `Bearer ${token}`, "X-Backyard": "1", "Content-Type": "application/json", ...extra };
}

export async function sha256Hex(data: Uint8Array | string): Promise<string> {
  const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
  const d = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
  return [...d].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export type SiteFiles = Record<string, string | Uint8Array>;

export interface DeployOutcome {
  create: { deploy_id: string; uploads: { path: string; url: string; method: string; headers: Record<string, string> }[]; unchanged: string[]; expires_at: string };
  commit: { site: Record<string, unknown>; url: string; deleted: number };
}

/** Runs the full gateway protocol (create → PUT uploads → commit); contentTypes declares types ({path: type}). */
export async function deploy(
  site: string,
  files: SiteFiles,
  opts: { token?: string; envOverrides?: Partial<Env>; contentTypes?: Record<string, string> } = {},
): Promise<DeployOutcome> {
  const token = opts.token ?? DEPLOY_TOKEN;
  const entries = await Promise.all(
    Object.entries(files).map(async ([path, content]) => {
      const bytes = typeof content === "string" ? new TextEncoder().encode(content) : content;
      return { path, bytes, size: bytes.length, sha256: await sha256Hex(bytes) };
    }),
  );
  const types = opts.contentTypes ?? {};
  const list = entries.map(({ path, size, sha256 }) => (Object.hasOwn(types, path) ? { path, size, sha256, content_type: types[path] } : { path, size, sha256 }));
  const res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: tokenHeaders(token), body: JSON.stringify({ files: list }) }, opts.envOverrides);
  if (res.status !== 200) throw new Error(`create deploy: ${res.status} ${await res.text()}`);
  const create = (await res.json()) as DeployOutcome["create"];
  for (const up of create.uploads) {
    const e = entries.find((x) => x.path === up.path)!;
    const r = await call(up.url, { method: up.method, headers: { ...up.headers, "Content-Length": String(e.size) }, body: e.bytes }, opts.envOverrides);
    if (r.status !== 200) throw new Error(`upload ${up.path}: ${r.status} ${await r.text()}`);
  }
  const c = await call(
    `/__backyard/api/deploy/sites/${site}/deploys/${encodeURIComponent(create.deploy_id)}/commit`,
    { method: "POST", headers: tokenHeaders(token), body: JSON.stringify({ files: list }) },
    opts.envOverrides,
  );
  if (c.status !== 200) throw new Error(`commit: ${c.status} ${await c.text()}`);
  return { create, commit: (await c.json()) as DeployOutcome["commit"] };
}

/** Returns the anon cookie pair ("backyard_anon=...") set by a response. */
export function anonCookie(res: Response): string {
  const sc = res.headers.get("Set-Cookie") ?? "";
  const m = /backyard_anon=([^;]+)/.exec(sc);
  return m ? `backyard_anon=${m[1]}` : "";
}

// --- Claude connector (SPEC §8.4) ---------------------------------------------

/** Variables that turn the Claude connector on (mode none, the default hub https://backyard.test). */
export const MCP_ENV: Partial<Env> = { MCP_ENABLED: "true" };

/** R, the MCP resource of the test hub. */
export const MCP_RESOURCE = ORIGIN + "/__backyard/mcp";

/** The loopback redirect URI the tests register (no listener is needed: the flow stops at its Location). */
export const LOOPBACK = "http://127.0.0.1:9/callback";

/** A PKCE pair: a 43-character verifier and its S256 challenge. */
export async function pkcePair(): Promise<{ verifier: string; challenge: string }> {
  const b64url = (b: Uint8Array) => btoa(String.fromCharCode(...b)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  const verifier = b64url(crypto.getRandomValues(new Uint8Array(32)));
  const challenge = b64url(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier))));
  return { verifier, challenge };
}

/** Decodes the character references of an HTML attribute value. */
export function decodeHTML(s: string): string {
  const named: Record<string, string> = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" };
  return s.replace(/&(#x[0-9a-f]+|#[0-9]+|amp|lt|gt|quot|apos);/gi, (_, e: string) =>
    e[0] === "#" ? String.fromCodePoint(e[1] === "x" || e[1] === "X" ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10)) : named[e.toLowerCase()]!,
  );
}

/** The hidden inputs of a consent page, HTML-unescaped. */
export function hiddenInputs(html: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const m of html.matchAll(/<input type="hidden" name="([^"]+)" value="([^"]*)">/g)) out[decodeHTML(m[1]!)] = decodeHTML(m[2]!);
  return out;
}

/** Registers a client (stateless DCR) and returns the registration. */
export async function registerClient(
  opts: { name?: string; redirectURIs?: string[]; env?: Partial<Env> } = {},
): Promise<{ client_id: string; client_name: string; redirect_uris: string[] }> {
  const res = await call(
    "/__backyard/oauth/register",
    { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ client_name: opts.name ?? "Test client", redirect_uris: opts.redirectURIs ?? [LOOPBACK] }) },
    { ...MCP_ENV, ...opts.env },
  );
  if (res.status !== 201) throw new Error(`register: ${res.status} ${await res.text()}`);
  return (await res.json()) as { client_id: string; client_name: string; redirect_uris: string[] };
}

/** The query of an authorization request. */
export function authorizeQuery(p: { clientId: string; challenge: string; redirectURI?: string; state?: string; scope?: string; resource?: string | null }): string {
  const q = new URLSearchParams({ response_type: "code", client_id: p.clientId, redirect_uri: p.redirectURI ?? LOOPBACK, code_challenge: p.challenge, code_challenge_method: "S256" });
  if (p.state !== undefined) q.set("state", p.state);
  if (p.scope !== undefined) q.set("scope", p.scope);
  if (p.resource !== null) q.set("resource", p.resource ?? MCP_RESOURCE);
  return q.toString();
}

/** The headers of a consent form POST from the consent page itself. */
export function consentHeaders(identity: Record<string, string>): Record<string, string> {
  return { ...identity, "Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", Origin: ORIGIN };
}

/**
 * Opens the consent page for a browser (identity: its Cookie, or the
 * Cf-Access-Jwt-Assertion header in mode access). In mode none a browser
 * without a cookie gets a new guest, whose cookie is returned in identity.
 */
export async function openConsent(
  query: string,
  identity: Record<string, string> = {},
  env: Partial<Env> = {},
): Promise<{ page: Response; html: string; form: Record<string, string>; identity: Record<string, string> }> {
  const page = await call(`/__backyard/auth/authorize?${query}`, { headers: { "Sec-Fetch-Site": "cross-site", ...identity } }, { ...MCP_ENV, ...env });
  const html = await page.text();
  const minted = anonCookie(page);
  const who = minted && !identity.Cookie ? { ...identity, Cookie: minted } : identity;
  return { page, html, form: page.status === 200 ? hiddenInputs(html) : {}, identity: who };
}

/** Posts a consent decision. */
export function submitConsent(
  form: Record<string, string>,
  identity: Record<string, string>,
  opts: { decision?: string; headers?: Record<string, string>; env?: Partial<Env> } = {},
): Promise<Response> {
  const body = new URLSearchParams({ ...form, decision: opts.decision ?? "allow" });
  return call("/__backyard/auth/authorize", { method: "POST", headers: { ...consentHeaders(identity), ...opts.headers }, body: body.toString() }, { ...MCP_ENV, ...opts.env });
}

/** The query parameters of a redirect to the loopback client. */
export function redirectParams(res: Response): Record<string, string> {
  const loc = res.headers.get("Location") ?? "";
  return Object.fromEntries(new URL(loc).searchParams);
}

/** A token endpoint request (form body). */
export function tokenRequest(fields: Record<string, string>, opts: { headers?: Record<string, string>; env?: Partial<Env>; path?: string } = {}): Promise<Response> {
  return call(
    opts.path ?? "/__backyard/oauth/token",
    { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded", ...opts.headers }, body: new URLSearchParams(fields).toString() },
    { ...MCP_ENV, ...opts.env },
  );
}

/** A connection made through the whole flow: register, authorize, consent, code exchange. */
export interface MCPConnection {
  clientId: string;
  /** The browser that consented (its Cookie, or Access header). */
  identity: Record<string, string>;
  access_token: string;
  refresh_token: string;
  expires_in: number;
  scope: string;
}

/**
 * Connects a client the way Claude does (DCR, PKCE, consent, code
 * exchange). Without identity the browser is a new guest (mode none).
 */
export async function mcpConnect(
  opts: { identity?: Record<string, string>; env?: Partial<Env>; scope?: string; name?: string; clientId?: string; redirectURI?: string } = {},
): Promise<MCPConnection> {
  const env = opts.env ?? {};
  const redirectURI = opts.redirectURI ?? LOOPBACK;
  const clientId = opts.clientId ?? (await registerClient({ name: opts.name ?? "Test client", redirectURIs: [redirectURI], env })).client_id;
  const { verifier, challenge } = await pkcePair();
  const opened = await openConsent(authorizeQuery({ clientId, challenge, redirectURI, state: "st", scope: opts.scope ?? "mcp offline_access" }), opts.identity ?? {}, env);
  if (opened.page.status !== 200) throw new Error(`authorize: ${opened.page.status} ${opened.html.slice(0, 300)}`);
  const allowed = await submitConsent(opened.form, opened.identity, { env });
  if (allowed.status !== 303) throw new Error(`consent: ${allowed.status} ${await allowed.text()}`);
  const code = redirectParams(allowed).code ?? "";
  const res = await tokenRequest({ grant_type: "authorization_code", client_id: clientId, code, code_verifier: verifier, redirect_uri: redirectURI }, { env });
  if (res.status !== 200) throw new Error(`token: ${res.status} ${await res.text()}`);
  const t = (await res.json()) as { access_token: string; refresh_token: string; expires_in: number; scope: string };
  return { clientId, identity: opened.identity, ...t };
}
