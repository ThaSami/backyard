// Deploy gateway protocol (SPEC §7): info, me, diffed deploys, sites, files,
// refresh, delete and the validation errors.
import assert from "node:assert/strict";
import { after, before, describe, test } from "node:test";
import { commitDeploy, createDeploy, deploySite, fileEntries, putUpload, sha256 } from "../lib/deploy.mjs";
import { hubURL, setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";
import { assertSiteMeta, assertUser, RFC3339_UTC } from "../lib/shapes.mjs";

const env = await setup();
const client = new Client();
const deployable = Boolean(client.token) || env.info.auth?.anonymous_deploy === true;

describe("info", () => {
  test("GET /__backyard/api/info needs no authentication and has the SPEC §7.1 shape", async () => {
    const r = expectStatus(await new Client({ token: null }).get(hubURL + "/__backyard/api/info"), 200);
    assert.match(r.header("content-type"), /^application\/json\b/);
    const i = r.json;
    for (const k of ["name", "version", "public_url", "base_domain", "url_mode", "gateway_url"]) {
      assert.equal(typeof i[k], "string", `info.${k}: ${r.text}`);
    }
    assert.ok(["host", "path"].includes(i.url_mode), `url_mode ${i.url_mode}`);
    assert.match(i.public_url, /^https?:\/\/[^/]+$/, "public_url is an origin without a trailing slash");
    assert.match(i.gateway_url, /^https?:\/\//, "gateway_url is absolute");
    assert.equal(typeof i.auth?.mode, "string");
    assert.equal(typeof i.auth?.anonymous_deploy, "boolean");
    assert.ok(Array.isArray(i.methods) && i.methods.includes("api"), `methods: ${JSON.stringify(i.methods)}`);
    for (const k of ["max_site_bytes", "max_files", "max_file_bytes"]) {
      assert.ok(Number.isInteger(i.limits?.[k]), `limits.${k}`);
    }
    if ("rsync_target" in i) assert.notEqual(i.rsync_target, "", "rsync_target is omitted when empty");
    if (i.storage !== undefined) assert.equal(typeof i.storage.type, "string");
  });

  test("GET {gateway}/info is the same document", async () => {
    const a = (await client.get(hubURL + "/__backyard/api/info")).json;
    const b = expectStatus(await client.get(env.gateway + "/info"), 200).json;
    assert.deepEqual(b, a);
  });

  test("POST /info is 405 with Allow", async () => {
    const r = await client.post(env.gateway + "/info", {});
    assertError(r, 405, "bad_request");
    assert.match(r.header("allow") || "", /\bGET\b/);
  });
});

describe("gateway identity and site list", () => {
  test("GET {gateway}/me returns the caller", async () => {
    const r = await client.get(env.gateway + "/me");
    if (!deployable && r.status === 401) {
      assertError(r, 401, "unauthenticated");
      return;
    }
    assertUser(expectStatus(r, 200).json, "gateway /me");
  });

  test("an invalid bearer token is 401 unauthenticated", async () => {
    const r = await new Client({ token: "not-a-valid-token" }).get(env.gateway + "/me");
    assertError(r, 401, "unauthenticated");
  });

  test("unknown endpoints are 404 not_found", async () => {
    assertError(await client.get(env.gateway + "/nope"), 404, "not_found");
    assertError(await client.get(env.gateway + "/sites/x/nope"), 404, "not_found");
  });
});

describe("deploys", { skip: !deployable && "no BACKYARD_TOKEN and anonymous deploys are off" }, () => {
  const site = uniqueSite("gw");
  const v1 = {
    "index.html": "<!doctype html><html><head><title>Conformance  Site</title><meta name=\"description\" content=\"A test site\"></head><body>v1</body></html>",
    "app.js": "console.log('v1');\n",
    "style.css": "body { color: red; }\n",
    "docs/guide.html": "<p>guide</p>",
    "data/empty.txt": "",
  };
  let first;
  let me;

  before(async () => {
    me = (await client.get(env.gateway + "/me")).json;
  });

  after(async () => {
    if (!me?.anonymous) await client.delete(`${env.gateway}/sites/${site}`);
  });

  test("create → upload → commit", async () => {
    const list = fileEntries(v1);
    const entries = list.map((f) => f.entry);
    const plan = expectStatus(await createDeploy(client, env, site, entries), 200, "create").json;
    assert.equal(typeof plan.deploy_id, "string");
    assert.ok(plan.deploy_id.length > 10);
    assert.deepEqual(plan.unchanged, [], "nothing is unchanged on a new site");
    assert.deepEqual(plan.uploads.map((u) => u.path).sort(), Object.keys(v1).sort());
    for (const u of plan.uploads) {
      assert.equal(u.method, "PUT");
      assert.match(u.url, /^https?:\/\//);
      if (u.headers !== undefined) assert.equal(typeof u.headers, "object");
    }
    assert.match(plan.expires_at, RFC3339_UTC);
    const ttl = Date.parse(plan.expires_at) - Date.now();
    assert.ok(ttl > 50 * 60_000 && ttl <= 61 * 60_000, `deploy ids expire after about 1h, got ${ttl}ms`);

    const bufs = new Map(list.map((f) => [f.entry.path, f.buf]));
    for (const u of plan.uploads) {
      const r = await putUpload(client, u, bufs.get(u.path));
      assert.ok(r.status >= 200 && r.status < 300, `upload ${u.path}: ${r}`);
    }
    const c = expectStatus(await commitDeploy(client, env, site, plan.deploy_id, entries), 200, "commit").json;
    assert.equal(c.url, env.canonicalSiteURL(site), "commit url");
    assert.equal(c.deleted, 0);
    assertSiteMeta(c.site, { name: site, url: env.canonicalSiteURL(site) });
    assert.equal(c.site.title, "Conformance Site", "title from <title> with whitespace collapsed");
    assert.equal(c.site.description, "A test site", "description from <meta name=description>");
    assert.equal(c.site.file_count, Object.keys(v1).length);
    assert.equal(c.site.bytes, entries.reduce((n, e) => n + e.size, 0));
    assert.equal(c.site.created_by, me.id);
    assert.equal(c.site.updated_by, me.id);
    assert.match(c.site.deploy_id, /^d_[0-9a-f]+$/);
    first = c.site;
  });

  test("the site is listed and readable", async () => {
    const list = expectStatus(await client.get(env.gateway + "/sites"), 200).json;
    assert.ok(Array.isArray(list.sites));
    const m = list.sites.find((s) => s.name === site);
    assert.ok(m, `${site} is listed`);
    assertSiteMeta(m, { name: site, url: env.canonicalSiteURL(site) });

    const one = expectStatus(await client.get(`${env.gateway}/sites/${site}`), 200).json;
    assertSiteMeta(one, { name: site, url: env.canonicalSiteURL(site) });
    assert.equal(one.deploy_id, first.deploy_id);

    const hub = expectStatus(await client.get(hubURL + "/__backyard/api/sites"), 200).json;
    assert.ok(hub.sites.some((s) => s.name === site), "the hub API lists the site");
  });

  test("files list and raw file reads", async () => {
    const r = expectStatus(await client.get(`${env.gateway}/sites/${site}/files`), 200).json;
    assert.ok(Array.isArray(r.files));
    const byPath = new Map(r.files.map((f) => [f.path, f]));
    assert.deepEqual([...byPath.keys()].sort(), Object.keys(v1).sort());
    for (const [p, content] of Object.entries(v1)) {
      const f = byPath.get(p);
      assert.equal(f.size, Buffer.byteLength(content), `size of ${p}`);
      assert.equal(f.sha256, sha256(Buffer.from(content)), `sha256 of ${p}`);
      assert.match(f.updated_at, RFC3339_UTC, `updated_at of ${p}`);
    }
    for (const p of ["docs/guide.html", "app.js", "data/empty.txt"]) {
      const raw = expectStatus(await client.get(`${env.gateway}/sites/${site}/files/${p}`), 200, p);
      assert.equal(raw.text, v1[p], `content of ${p}`);
    }
    assertError(await client.get(`${env.gateway}/sites/${site}/files/missing.txt`), 404, "not_found");
    assertError(await client.get(`${env.gateway}/sites/${site}/files/__backyard/x.js`), 400, "bad_request", "reserved path");
    assertError(await client.get(`${env.gateway}/sites/Bad_Name/files`), 400, "bad_request", "invalid site name");
  });

  test("raw file reads never run as content of the gateway's origin", async () => {
    const r = expectStatus(await client.get(`${env.gateway}/sites/${site}/files/index.html`), 200);
    assert.match(r.header("content-security-policy") || "", /\bsandbox\b/, "CSP sandbox");
    assert.match(r.header("content-disposition") || "", /^attachment\b/, "served as an attachment");
    assert.equal(r.header("cache-control"), "no-store");
    assert.equal(r.header("x-content-type-options"), "nosniff");
    const h = expectStatus(await client.head(`${env.gateway}/sites/${site}/files/index.html`), 200);
    assert.equal(h.body.length, 0);
    assert.equal(h.header("content-length"), String(Buffer.byteLength(v1["index.html"])));
  });

  test("a redeploy of the same files uploads nothing", async () => {
    const entries = fileEntries(v1).map((f) => f.entry);
    const plan = expectStatus(await createDeploy(client, env, site, entries), 200).json;
    assert.deepEqual(plan.uploads, []);
    assert.deepEqual([...plan.unchanged].sort(), Object.keys(v1).sort());
    const c = expectStatus(await commitDeploy(client, env, site, plan.deploy_id, entries), 200).json;
    assert.equal(c.deleted, 0);
    assert.equal(c.site.created_at, first.created_at, "created_at is kept");
    assert.notEqual(c.site.deploy_id, first.deploy_id, "every deploy gets a new deploy_id");
  });

  test("a diffed redeploy uploads changes and deletes removed files last", async () => {
    const v2 = { ...v1, "app.js": "console.log('v2');\n", "new.txt": "new" };
    delete v2["style.css"];
    delete v2["docs/guide.html"];
    const list = fileEntries(v2);
    const entries = list.map((f) => f.entry);
    const plan = expectStatus(await createDeploy(client, env, site, entries), 200).json;
    assert.deepEqual(plan.uploads.map((u) => u.path).sort(), ["app.js", "new.txt"]);
    assert.deepEqual([...plan.unchanged].sort(), ["data/empty.txt", "index.html"]);
    // Before the commit, the removed files are still served (deletions go last).
    const before = await client.get(`${env.gateway}/sites/${site}/files/style.css`);
    expectStatus(before, 200, "removed files stay until the commit");
    const bufs = new Map(list.map((f) => [f.entry.path, f.buf]));
    for (const u of plan.uploads) expectStatus(await putUpload(client, u, bufs.get(u.path)), 200, `upload ${u.path}`);
    const c = expectStatus(await commitDeploy(client, env, site, plan.deploy_id, entries), 200).json;
    assert.equal(c.deleted, 2);
    assert.equal(c.site.file_count, 4);
    const files = (await client.get(`${env.gateway}/sites/${site}/files`)).json.files.map((f) => f.path).sort();
    assert.deepEqual(files, ["app.js", "data/empty.txt", "index.html", "new.txt"]);
    assert.equal((await client.get(`${env.gateway}/sites/${site}/files/app.js`)).text, v2["app.js"]);
    assertError(await client.get(`${env.gateway}/sites/${site}/files/style.css`), 404, "not_found");
  });

  test("owners: 501 unsupported without sites.ownership, guests cannot change them", async () => {
    const r = await client.post(`${env.gateway}/sites/${site}/owners`, { add: ["someone@example.com"] });
    if (me.anonymous) {
      assertError(r, 401, "unauthenticated");
      return;
    }
    if (r.status === 501) {
      assertError(r, 501, "unsupported");
      return;
    }
    const m = assertSiteMeta(expectStatus(r, 200).json, { name: site });
    assert.ok(m.owners.includes("someone@example.com"));
    const back = expectStatus(await client.post(`${env.gateway}/sites/${site}/owners`, { remove: ["someone@example.com"] }), 200).json;
    assert.ok(!back.owners.includes("someone@example.com"));
  });

  test("refresh rebuilds the metadata from storage", async () => {
    const r = expectStatus(await client.post(`${env.gateway}/sites/${site}/refresh`, {}), 200).json;
    assertSiteMeta(r, { name: site, url: env.canonicalSiteURL(site) });
    assert.equal(r.file_count, 4);
    assert.equal(r.title, "Conformance Site");
  });

  test("a commit with a missing upload is 409 conflict naming the path", async () => {
    const files = { ...v1, "missing.bin": "never uploaded" };
    const entries = fileEntries(files).map((f) => f.entry);
    const plan = expectStatus(await createDeploy(client, env, site, entries), 200).json;
    const e = assertError(await commitDeploy(client, env, site, plan.deploy_id, entries), 409, "conflict");
    assert.match(e.message, /missing\.bin/);
  });

  test("uploads verify the declared size and sha256", async () => {
    const entries = fileEntries({ "check.txt": "expected body" }).map((f) => f.entry);
    const plan = expectStatus(await createDeploy(client, env, site, [...fileEntries(v1).map((f) => f.entry), ...entries]), 200).json;
    const up = plan.uploads.find((u) => u.path === "check.txt");
    assert.ok(up);
    if (!new URL(up.url).pathname.includes("/uploads/")) return; // presigned storage URL: storage verifies
    const wrong = await putUpload(client, up, Buffer.from("tampered body"));
    assertError(wrong, 400, "bad_request", "same size, other sha256");
    const longer = await putUpload(client, up, Buffer.from("expected body, but longer"));
    assert.ok([400, 413].includes(longer.status), `a longer body is rejected: ${longer}`);
    const bad = await client.fetch(up.url.replace(/\/uploads\/.*/, "/uploads/not-a-token"), { method: "PUT", body: "x", csrf: false });
    assertError(bad, 403, "forbidden", "invalid upload token");
  });

  test("deploy ids are bound to their site", async () => {
    const other = uniqueSite("gw2");
    const entries = fileEntries({ "index.html": "x" }).map((f) => f.entry);
    const plan = expectStatus(await createDeploy(client, env, site, entries), 200).json;
    assertError(await commitDeploy(client, env, other, plan.deploy_id, entries), 400, "bad_request", "deploy id of another site");
    assertError(await commitDeploy(client, env, site, "garbage", entries), 400, "bad_request", "invalid deploy id");
  });

  test("site name validation", async () => {
    const entries = fileEntries({ "index.html": "x" }).map((f) => f.entry);
    for (const bad of ["Bad", "-x", "x-", "a_b", "x".repeat(64)]) {
      assertError(await createDeploy(client, env, bad, entries), 400, "bad_request", `site name ${bad}`);
    }
    assertError(await createDeploy(client, env, "www", entries), 400, "bad_request", "reserved name www");
    assertError(await createDeploy(client, env, "s", entries), 400, "bad_request", "reserved name s");
  });

  test("file list validation", async () => {
    const good = { path: "index.html", size: 1, sha256: sha256(Buffer.from("x")) };
    const cases = [
      ["empty list", []],
      ["absolute path", [{ ...good, path: "/index.html" }]],
      ["dot-dot segment", [{ ...good, path: "a/../index.html" }]],
      ["dot segment", [{ ...good, path: "./index.html" }]],
      ["empty segment", [{ ...good, path: "a//index.html" }]],
      ["backslash", [{ ...good, path: "a\\b.html" }]],
      ["reserved prefix", [{ ...good, path: "__backyard/x.js" }]],
      ["control character", [{ ...good, path: "a\u0001.html" }]],
      ["path too long", [{ ...good, path: "a".repeat(1025) }]],
      ["duplicate path", [good, good]],
      ["bad sha256", [{ ...good, sha256: "abc" }]],
      ["negative size", [{ ...good, size: -1 }]],
      ["file vs directory", [good, { ...good, path: "index.html/x" }]],
    ];
    for (const [what, files] of cases) {
      assertError(await createDeploy(client, env, site, files), 400, "bad_request", what);
    }
    const r = await client.post(`${env.gateway}/sites/${site}/deploys`, "{not json", { raw: true });
    assertError(r, 400, "bad_request", "invalid JSON");
  });

  test("limits from /info are enforced with 413 too_large", async () => {
    const max = env.info.limits.max_file_bytes;
    const f = { path: "huge.bin", size: max + 1, sha256: sha256(Buffer.from("x")) };
    assertError(await createDeploy(client, env, site, [f]), 413, "too_large", "file above max_file_bytes");
    const n = env.info.limits.max_files;
    if (n > 0 && n <= 20_000) {
      const many = Array.from({ length: n + 1 }, (_, i) => ({ path: `f${i}.txt`, size: 1, sha256: sha256(Buffer.from("x")) }));
      assertError(await createDeploy(client, env, site, many), 413, "too_large", "more than max_files");
    }
  });

  test("wrong methods are 405 with Allow", async () => {
    const r = await client.get(`${env.gateway}/sites/${site}/deploys`);
    assertError(r, 405, "bad_request");
    assert.match(r.header("allow") || "", /\bPOST\b/);
    const r2 = await client.post(`${env.gateway}/sites/${site}/files`, {});
    assertError(r2, 405, "bad_request");
  });

  test("an unknown site is 404 not_found", async () => {
    const ghost = uniqueSite("ghost");
    assertError(await client.get(`${env.gateway}/sites/${ghost}`), 404, "not_found");
    assertError(await client.get(`${env.gateway}/sites/${ghost}/files`), 404, "not_found");
  });

  test("delete (needs a non-anonymous identity) removes the site with its data", async () => {
    const victim = uniqueSite("del");
    await deploySite(client, env, victim, { "index.html": "<p>bye</p>" });
    expectStatus(await client.put(env.api(victim, "/db/notes/n1"), { text: "keep?" }), 200);
    const fd = new FormData();
    fd.append("file", new Blob(["bytes"]), "f.txt");
    const up = expectStatus(await client.fetch(env.api(victim, "/files"), { method: "POST", body: fd }), 201).json;
    const r = await client.delete(`${env.gateway}/sites/${victim}`);
    if (me.anonymous) {
      assertError(r, 401, "unauthenticated", "guests cannot delete sites");
      return;
    }
    expectStatus(r, 204);
    assertError(await client.get(`${env.gateway}/sites/${victim}`), 404, "not_found");
    const page = await client.get(env.siteURL(victim));
    assert.equal(page.status, 404, `a deleted site is gone: ${page}`);
    assertError(await client.delete(`${env.gateway}/sites/${victim}`), 404, "not_found", "deleting twice");
    assert.ok(!(await client.get(env.gateway + "/sites")).json.sites.some((s) => s.name === victim), "no longer listed");
    // A new site of the same name starts empty.
    await deploySite(client, env, victim, { "index.html": "<p>again</p>" });
    assertError(await client.get(env.api(victim, "/db/notes/n1")), 404, "not_found", "documents are deleted with the site");
    assertError(await client.get(env.api(victim, `/files/${up.id}`)), 404, "not_found", "uploads are deleted with the site");
    assert.equal((await client.get(new URL(up.url, env.siteURL(victim)).href)).status, 404, "upload bytes are gone");
    await client.delete(`${env.gateway}/sites/${victim}`);
  });
});

describe("gateway CSRF", { skip: !deployable && "no BACKYARD_TOKEN and anonymous deploys are off" }, () => {
  const site = uniqueSite("gwcsrf");
  const entries = fileEntries({ "index.html": "x" }).map((f) => f.entry);

  test("state-changing requests without X-Backyard and without a bearer token are 403", async () => {
    const anon = new Client({ token: null });
    const r = await anon.post(`${env.gateway}/sites/${site}/deploys`, { files: entries }, { csrf: false });
    assertError(r, 403, "forbidden");
  });

  test("a bearer token without browser signals needs no X-Backyard", { skip: !client.token && "no BACKYARD_TOKEN" }, async () => {
    const r = await client.post(`${env.gateway}/sites/${site}/deploys`, { files: entries }, { csrf: false });
    expectStatus(r, 200);
    const cross = await client.post(`${env.gateway}/sites/${site}/deploys`, { files: entries }, { csrf: false, headers: { Origin: "https://evil.example" } });
    assertError(cross, 403, "forbidden", "a bearer token with a cross-site Origin");
  });
});
