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

/** Runs the full gateway protocol (create → PUT uploads → commit). */
export async function deploy(site: string, files: SiteFiles, opts: { token?: string; envOverrides?: Partial<Env> } = {}): Promise<DeployOutcome> {
  const token = opts.token ?? DEPLOY_TOKEN;
  const entries = await Promise.all(
    Object.entries(files).map(async ([path, content]) => {
      const bytes = typeof content === "string" ? new TextEncoder().encode(content) : content;
      return { path, bytes, size: bytes.length, sha256: await sha256Hex(bytes) };
    }),
  );
  const list = entries.map(({ path, size, sha256 }) => ({ path, size, sha256 }));
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
