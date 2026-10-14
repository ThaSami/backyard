/**
 * Backyard — Cloudflare Workers edition (SPEC §16).
 *
 * One Worker implements the public contract of the Go server: hub and site
 * URLs (host or path mode), static serving, identity, the site API with
 * realtime websockets, AI, the deploy gateway and the agent docs. Each site's
 * data lives in its own SQLite Durable Object (SiteDO); RegistryDO keeps the
 * site list and the free-tier guardrail counters.
 */
import { getConfig } from "./config";
import type { Env } from "./env";
import { HttpError, errorResponse, requestID, withSecurityHeaders } from "./http";
import { serveHub } from "./hub";
import { errorPage } from "./pages";
import { route, wantsJSON, type RequestContext } from "./router";
import { serveSite } from "./site-api";
import { registry } from "./stubs";

export { RegistryDO } from "./registry-do";
export { SiteDO } from "./site-do";

const HEALTHZ = "/__backyard/healthz";
const READYZ = "/__backyard/readyz";

function text(body: string, status = 200): Response {
  return new Response(body, { status, headers: { "Content-Type": "text/plain; charset=utf-8", "Cache-Control": "no-store" } });
}

async function readyz(env: Env): Promise<Response> {
  const cfg = getConfig(env);
  if (cfg.fatal) return text(`not ready: ${cfg.fatal}\n`, 503);
  if (!cfg.secret) return text("not ready: BACKYARD_SECRET is not set\n", 503);
  try {
    const r = await registry(env).ping();
    if (!r.ok) return text("not ready: registry unavailable\n", 503);
  } catch {
    return text("not ready: registry unavailable\n", 503);
  }
  return text("ok\n");
}

/** Renders an error as JSON (API clients) or as an HTML page (browsers). */
function renderError(err: unknown, req: Request, ctx: RequestContext | undefined, publicURL: string): Response {
  let he: HttpError;
  if (err instanceof HttpError) {
    he = err;
  } else {
    const msg = err instanceof Error ? err.message : String(err);
    if (/overloaded|Durable Object.*(reset|exceeded)/i.test(msg)) {
      he = new HttpError(503, "internal", "the site is busy; retry shortly", { retryAfter: 2 });
    } else {
      he = new HttpError(500, "internal", "internal error");
    }
    console.error(`backyard: ${req.method} ${new URL(req.url).pathname}: ${msg}`);
  }
  const path = ctx ? ctx.path : new URL(req.url).pathname;
  const isJSON = ctx
    ? wantsJSON(ctx)
    : path.includes("/__backyard/") && !path.includes("/__backyard/files/") && !path.includes("/__backyard/auth/");
  const res = isJSON ? errorResponse(he) : errorPage(he, publicURL, req.method);
  if (he.allow?.length) res.headers.set("Allow", he.allow.join(", "));
  return res;
}

async function handle(req: Request, env: Env, ectx: ExecutionContext): Promise<Response> {
  const url = new URL(req.url);
  // Liveness and readiness on any host, without authentication (SPEC §2).
  if (url.pathname === HEALTHZ || /^\/s\/[^/]+\/__backyard\/healthz$/.test(url.pathname)) return text("ok\n");
  if (url.pathname === READYZ || /^\/s\/[^/]+\/__backyard\/readyz$/.test(url.pathname)) return readyz(env);
  const cfg = getConfig(env);
  if (cfg.fatal) {
    // Fail closed: never serve with a configuration that may have been meant
    // to require sign-in (the details are logged: npx wrangler tail).
    const err = new HttpError(503, "internal", `this Backyard is not configured correctly (${cfg.fatal}); its operator can see the details with npx wrangler tail`);
    return renderError(err, req, undefined, cfg.publicURL || url.origin);
  }
  let ctx: RequestContext | undefined;
  let res: Response;
  try {
    const r = route(req, env, cfg, (p) => ectx.waitUntil(p));
    if ("response" in r) return r.response;
    ctx = r.ctx;
    res = ctx.kind === "site" ? await serveSite(ctx) : await serveHub(ctx);
  } catch (e) {
    res = renderError(e, req, ctx, ctx?.publicURL || cfg.publicURL || url.origin);
  }
  if (ctx?.setCookies.length) {
    if (res.status === 101) return res;
    res = new Response(res.body, res);
    for (const c of ctx.setCookies) res.headers.append("Set-Cookie", c);
  }
  return res;
}

export default {
  async fetch(req: Request, env: Env, ectx: ExecutionContext): Promise<Response> {
    const res = await handle(req, env, ectx);
    return res.status === 101 ? res : withSecurityHeaders(res, requestID(req));
  },
} satisfies ExportedHandler<Env>;
