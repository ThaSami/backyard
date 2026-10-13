/**
 * Static assets bundled into the Worker: the client SDK with its per-request
 * preamble (SPEC §2.1), the hub UI and the docs.
 */
import { HUB_FILES } from "./generated/hub";
import { SDK_DTS, SDK_JS, SDK_MJS } from "./generated/sdk";
import { contentTypeFor } from "./mime";
import { contentETag, serveObject } from "./object";
import type { RequestContext } from "./router";

const enc = new TextEncoder();

/** Bytes and ETags of generated documents, cached per content. */
const etagCache = new Map<string, Promise<string>>();

async function etagOf(key: string, data: Uint8Array): Promise<string> {
  let p = etagCache.get(key);
  if (!p) {
    p = contentETag(data);
    if (etagCache.size > 256) etagCache.clear();
    etagCache.set(key, p);
  }
  return p;
}

/** The preamble line prepended to backyard.js / backyard.mjs. */
export function sdkPreamble(ctx: RequestContext): string {
  const pre = {
    v: 1,
    mode: ctx.mode,
    site: ctx.kind === "site" ? ctx.site : null,
    prefix: "/s/",
    hub: ctx.publicURL,
  };
  return `globalThis.__BACKYARD__ = ${JSON.stringify(pre)};\n`;
}

/** Serves /__backyard/backyard.{js,mjs,d.ts} (no authentication: no secrets). */
export async function serveSDK(ctx: RequestContext): Promise<Response> {
  const p = ctx.path;
  let text: string;
  let ct = "text/javascript; charset=utf-8";
  let key = p;
  if (p.endsWith(".d.ts")) {
    text = SDK_DTS;
    ct = "text/plain; charset=utf-8";
  } else {
    const pre = sdkPreamble(ctx);
    key += "\n" + pre;
    text = pre + (p.endsWith(".mjs") ? SDK_MJS : SDK_JS);
  }
  const data = enc.encode(text);
  const etag = await etagOf(key, data);
  return serveObject(ctx.req, { name: p, size: data.length, etag, lastModified: 0, contentType: ct, data }, { cacheControl: "no-cache" });
}

/** Serves a generated text document (llms.txt, markdown docs). */
export async function serveText(ctx: RequestContext, name: string, contentType: string, body: string): Promise<Response> {
  const data = enc.encode(body);
  const etag = await contentETag(data);
  return serveObject(ctx.req, { name, size: data.length, etag, lastModified: 0, contentType, data }, { cacheControl: "no-cache" });
}

const hubBytes = new Map<string, Uint8Array>();

/** Bytes of a hub UI file, or null. */
export function hubFile(name: string): Uint8Array | null {
  const cached = hubBytes.get(name);
  if (cached) return cached;
  const f = HUB_FILES[name];
  if (!f) return null;
  let data: Uint8Array;
  if (f.text !== undefined) data = enc.encode(f.text);
  else {
    const bin = atob(f.base64 ?? "");
    data = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) data[i] = bin.charCodeAt(i);
  }
  hubBytes.set(name, data);
  return data;
}

/** Serves a hub UI file; null when it does not exist. */
export async function serveHubFile(ctx: RequestContext, name: string): Promise<Response | null> {
  const data = hubFile(name);
  if (!data) return null;
  const etag = await etagOf("hub:" + name, data);
  return serveObject(ctx.req, { name, size: data.length, etag, lastModified: 0, contentType: contentTypeFor(name), data }, { cacheControl: "no-cache" });
}

/** Whether name is a directory of the hub UI (has files below it). */
export function hubDirExists(name: string): boolean {
  const prefix = name.replace(/\/+$/, "") + "/";
  return Object.keys(HUB_FILES).some((k) => k.startsWith(prefix));
}
