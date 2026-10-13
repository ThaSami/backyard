// The deploy gateway protocol (SPEC §7.3) as a client: plan, upload, commit.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";

const TYPES = {
  html: "text/html; charset=utf-8",
  htm: "text/html; charset=utf-8",
  css: "text/css; charset=utf-8",
  js: "text/javascript; charset=utf-8",
  mjs: "text/javascript; charset=utf-8",
  json: "application/json",
  txt: "text/plain; charset=utf-8",
  svg: "image/svg+xml",
  png: "image/png",
};

/** sha256 hex of bytes. */
export function sha256(buf) {
  return createHash("sha256").update(buf).digest("hex");
}

/**
 * Builds the deploy file list of {path: content}. Content types are set for
 * well-known extensions and left to the server otherwise.
 */
export function fileEntries(files) {
  return Object.entries(files).map(([path, content]) => {
    const buf = Buffer.isBuffer(content) ? content : Buffer.from(content);
    const e = { path, size: buf.length, sha256: sha256(buf) };
    const ext = path.includes(".") ? path.slice(path.lastIndexOf(".") + 1).toLowerCase() : "";
    if (TYPES[ext]) e.content_type = TYPES[ext];
    return { entry: e, buf };
  });
}

/** POST {G}/sites/{site}/deploys. */
export function createDeploy(client, env, site, entries, opts = {}) {
  return client.post(`${env.gateway}/sites/${site}/deploys`, { files: entries, message: opts.message ?? "conformance" }, opts.fetch);
}

/** PUT one upload as instructed by the plan. */
export function putUpload(client, upload, buf) {
  assert.equal(upload.method, "PUT", `upload method of ${upload.path}`);
  return client.fetch(upload.url, { method: "PUT", body: buf, headers: { ...(upload.headers ?? {}) }, csrf: false });
}

/** POST {G}/sites/{site}/deploys/{id}/commit. */
export function commitDeploy(client, env, site, deployID, entries, opts = {}) {
  return client.post(`${env.gateway}/sites/${site}/deploys/${encodeURIComponent(deployID)}/commit`, { files: entries }, opts.fetch);
}

/**
 * Deploys files ({path: string|Buffer}) to site with the full protocol and
 * asserts every step. Returns {plan, commit, entries}.
 */
export async function deploySite(client, env, site, files, opts = {}) {
  const list = fileEntries(files);
  const entries = list.map((f) => f.entry);
  const plan = await createDeploy(client, env, site, entries, opts);
  assert.equal(plan.status, 200, `create deploy: ${plan}`);
  const p = plan.json;
  const byPath = new Map(list.map((f) => [f.entry.path, f.buf]));
  for (const up of p.uploads) {
    const buf = byPath.get(up.path);
    assert.ok(buf, `upload for an unknown path ${up.path}`);
    const r = await putUpload(client, up, buf);
    assert.ok(r.status >= 200 && r.status < 300, `upload ${up.path}: ${r}`);
  }
  const commit = await commitDeploy(client, env, site, p.deploy_id, entries, opts);
  assert.equal(commit.status, 200, `commit: ${commit}`);
  return { plan: p, commit: commit.json, entries };
}

/** Deletes a site when the identity may (best effort, for cleanup). */
export async function deleteSiteQuietly(client, env, site) {
  try {
    await client.delete(`${env.gateway}/sites/${site}`);
  } catch {
    /* cleanup only */
  }
}

/**
 * Whether the suite can deploy at all: with a token, or when the server
 * allows anonymous deploys.
 */
export function canDeploy(env, client) {
  return Boolean(client.token) || env.info?.auth?.anonymous_deploy === true;
}
