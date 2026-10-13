// Static serving (SPEC §3) and the SDK (§2.1).
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { hubURL, repoSDK, setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus, rawRequest } from "../lib/http.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";

const site = uniqueSite("static");
const spa = uniqueSite("spa");
const bare = uniqueSite("bare");
const big = randomBytes(70_000);
const files = {
  "index.html": "<!doctype html><title>Home</title><h1>home</h1>",
  "about.html": "<h1>about</h1>",
  "docs/index.html": "<h1>docs</h1>",
  "docs/page.html": "<h1>docs page</h1>",
  "assets/app.js": "export const x = 1;\n",
  "assets/style.css": "body{}\n",
  "data.json": '{"ok":true}',
  "notes.txt": "plain text\n",
  "blob.bin": big,
  "404.html": "<h1>custom not found</h1>",
  ".well-known/security.txt": "Contact: mailto:security@example.com\n",
  "names/hello world.txt": "space",
  "names/ü.txt": "umlaut",
  "names/a+b&c=d.txt": "reserved characters",
  "names/100%.txt": "percent",
};

const url = (s, p = "") => env.siteURL(s) + p;

describe("static serving", { skip }, () => {
  before(async () => {
    await deploySite(client, env, site, files);
    await deploySite(client, env, spa, { "index.html": "<h1>spa index</h1>", "200.html": "<h1>spa shell</h1>", "app.js": "1" });
    await deploySite(client, env, bare, { "index.html": "<h1>bare</h1>" });
  });

  test("/ serves index.html with the standard headers", async () => {
    const r = expectStatus(await client.get(url(site)), 200);
    assert.equal(r.text, files["index.html"]);
    assert.equal(r.header("content-type"), "text/html; charset=utf-8");
    assert.ok(r.header("etag"), "ETag");
    assert.match(r.header("etag"), /^(W\/)?"[^"]+"$/, "quoted ETag");
    assert.ok(!Number.isNaN(Date.parse(r.header("last-modified") || "")), `Last-Modified: ${r.header("last-modified")}`);
    assert.equal(r.header("cache-control"), "no-cache");
    assert.equal(r.header("x-content-type-options"), "nosniff");
    assert.equal(r.header("referrer-policy"), "strict-origin-when-cross-origin");
    assert.ok(r.header("x-request-id"), "X-Request-Id");
    assert.equal(r.header("x-frame-options"), null, "no X-Frame-Options: embedding sites is a feature");
  });

  test("explicit paths, clean URLs and directory indexes", async () => {
    assert.equal(expectStatus(await client.get(url(site, "index.html")), 200).text, files["index.html"]);
    assert.equal(expectStatus(await client.get(url(site, "about")), 200).text, files["about.html"], "clean URL");
    assert.equal(expectStatus(await client.get(url(site, "about.html")), 200).text, files["about.html"]);
    assert.equal(expectStatus(await client.get(url(site, "docs/")), 200).text, files["docs/index.html"]);
    assert.equal(expectStatus(await client.get(url(site, "docs/page")), 200).text, files["docs/page.html"]);
    assert.equal(expectStatus(await client.get(url(site, ".well-known/security.txt")), 200).text, files[".well-known/security.txt"]);
  });

  test("file names are percent-decoded", async () => {
    for (const name of ["hello world.txt", "ü.txt", "a+b&c=d.txt", "100%.txt"]) {
      const r = expectStatus(await client.get(url(site, "names/" + encodeURIComponent(name))), 200, name);
      assert.equal(r.text, files["names/" + name], name);
    }
    expectStatus(await client.get(url(site, "names/a+b%26c%3Dd.txt")), 200, "partially encoded");
    const bad = await client.get(url(site, "names/%E0%A4%A"));
    assert.equal(bad.status, 400, `malformed percent-encoding: ${bad}`);
  });

  test("path mode: the site root without a slash redirects", { skip: env.mode !== "path" && "host mode" }, async () => {
    const r = expectStatus(await client.get(`${hubURL}/s/${site}?x=1`), 301);
    assert.equal(new URL(r.header("location"), hubURL).href, `${hubURL}/s/${site}/?x=1`);
    expectStatus(await client.get(`${hubURL}/s/`), 404);
  });

  test("a directory without a trailing slash redirects (301), keeping the query", async () => {
    const r = expectStatus(await client.get(url(site, "docs?a=1&b=2")), 301);
    const loc = new URL(r.header("location"), url(site, "docs"));
    assert.equal(loc.href, url(site, "docs/?a=1&b=2"));
  });

  test("content types by extension", async () => {
    const cases = {
      "assets/app.js": /^(text|application)\/javascript; charset=utf-8$/,
      "assets/style.css": /^text\/css; charset=utf-8$/,
      "data.json": /^application\/json\b/,
      "notes.txt": /^text\/plain; charset=utf-8$/,
      "blob.bin": /^application\/octet-stream$/,
    };
    for (const [p, re] of Object.entries(cases)) {
      const r = expectStatus(await client.get(url(site, p)), 200, p);
      assert.match(r.header("content-type"), re, p);
    }
  });

  test("404.html is served with status 404", async () => {
    const r = expectStatus(await client.get(url(site, "nope/missing")), 404);
    assert.equal(r.text, files["404.html"]);
    assert.match(r.header("content-type"), /^text\/html/);
  });

  test("200.html is the SPA fallback", async () => {
    const r = expectStatus(await client.get(url(spa, "deep/client/route")), 200);
    assert.equal(r.text, "<h1>spa shell</h1>");
    assert.equal(expectStatus(await client.get(url(spa)), 200).text, "<h1>spa index</h1>", "index.html still wins at /");
    assert.equal(expectStatus(await client.get(url(spa, "app.js")), 200).text, "1", "files still win");
  });

  test("a missing file without 404.html gets the built-in 404 page (JSON when asked)", async () => {
    const r = expectStatus(await client.get(url(bare, "missing")), 404);
    assert.match(r.header("content-type"), /^text\/html/);
    const j = await client.get(url(bare, "missing"), { headers: { Accept: "application/json" } });
    assertError(j, 404, "not_found");
  });

  test("an unknown site is a 404 page", async () => {
    const ghost = uniqueSite("ghost");
    const r = expectStatus(await client.get(url(ghost)), 404);
    assert.match(r.header("content-type"), /^text\/html/);
    assert.ok(r.text.includes(env.info.public_url), "the site-not-found page links to the hub");
    assertError(await client.get(url(ghost, "x"), { headers: { Accept: "application/json" } }), 404, "not_found");
  });

  test("conditional GET: If-None-Match and If-Modified-Since give 304", async () => {
    const r = expectStatus(await client.get(url(site, "about.html")), 200);
    const etag = r.header("etag");
    const nm = expectStatus(await client.get(url(site, "about.html"), { headers: { "If-None-Match": etag } }), 304);
    assert.equal(nm.body.length, 0, "a 304 has no body");
    assert.equal(nm.header("etag"), etag);
    const lm = r.header("last-modified");
    expectStatus(await client.get(url(site, "about.html"), { headers: { "If-Modified-Since": lm } }), 304);
    expectStatus(await client.get(url(site, "about.html"), { headers: { "If-None-Match": '"other"' } }), 200);
    expectStatus(await client.get(url(site, "about.html"), { headers: { "If-None-Match": `"other", ${etag}` } }), 304, "a list of ETags");
    expectStatus(await client.get(url(site, "about.html"), { headers: { "If-None-Match": "*" } }), 304, "If-None-Match: *");
    const weak = etag.startsWith("W/") ? etag : "W/" + etag;
    expectStatus(await client.get(url(site, "about.html"), { headers: { "If-None-Match": weak } }), 304, "weak comparison");
    const head = expectStatus(await client.head(url(site, "about.html"), { headers: { "If-None-Match": etag } }), 304, "HEAD");
    assert.equal(head.body.length, 0);
  });

  test("If-Range", async () => {
    const etag = expectStatus(await client.get(url(site, "blob.bin")), 200).header("etag");
    const hit = await client.get(url(site, "blob.bin"), { headers: { Range: "bytes=0-9", "If-Range": etag } });
    expectStatus(hit, 206, "If-Range with the current ETag");
    assert.deepEqual(hit.body, big.subarray(0, 10));
    const miss = await client.get(url(site, "blob.bin"), { headers: { Range: "bytes=0-9", "If-Range": '"stale"' } });
    expectStatus(miss, 200, "If-Range with another ETag serves the whole file");
    assert.equal(miss.body.length, big.length);
  });

  test("Range requests", async () => {
    const r = expectStatus(await client.get(url(site, "blob.bin"), { headers: { Range: "bytes=10-109" } }), 206);
    assert.equal(r.header("content-range"), `bytes 10-109/${big.length}`);
    assert.equal(r.header("content-length"), "100");
    assert.deepEqual(r.body, big.subarray(10, 110));
    const suffix = expectStatus(await client.get(url(site, "blob.bin"), { headers: { Range: "bytes=-16" } }), 206);
    assert.deepEqual(suffix.body, big.subarray(big.length - 16));
    const open = expectStatus(await client.get(url(site, "blob.bin"), { headers: { Range: `bytes=${big.length - 5}-` } }), 206);
    assert.deepEqual(open.body, big.subarray(big.length - 5));
    const bad = expectStatus(await client.get(url(site, "blob.bin"), { headers: { Range: `bytes=${big.length + 10}-` } }), 416);
    assert.equal(bad.header("content-range"), `bytes */${big.length}`);
    const full = expectStatus(await client.get(url(site, "blob.bin")), 200);
    assert.equal(full.header("accept-ranges"), "bytes");
    assert.deepEqual(full.body, big);
  });

  test("HEAD has the GET headers and no body", async () => {
    const g = expectStatus(await client.get(url(site, "blob.bin")), 200);
    const h = expectStatus(await client.head(url(site, "blob.bin")), 200);
    assert.equal(h.body.length, 0);
    assert.equal(h.header("etag"), g.header("etag"));
    assert.equal(h.header("content-type"), g.header("content-type"));
    assert.equal(h.header("content-length"), String(big.length));
  });

  test("methods other than GET/HEAD are 405", async () => {
    for (const m of ["POST", "PUT", "DELETE"]) {
      const r = await client.fetch(url(site, "index.html"), { method: m, body: "" });
      assert.equal(r.status, 405, `${m}: ${r}`);
      assert.match(r.header("allow") || "", /GET/);
    }
  });

  test("the reserved prefix is never a site file", async () => {
    assertError(await client.get(url(site, "__backyard/nope")), 404, "not_found");
    assertError(await client.get(url(site, "__backyard/api/nope")), 404, "not_found");
  });

  test("non-canonical paths are redirected (GET) or rejected", async () => {
    const base = new URL(url(site));
    const prefix = base.pathname.replace(/\/$/, "");
    const raw = (path, opts = {}) => rawRequest(url(site), { path, ...opts, headers: { Host: base.host, ...opts.headers } });
    const write = { method: "POST", headers: { "X-Backyard": "1", "Content-Type": "application/json" }, body: "{}" };
    // Empty segments reach every server as sent: 301 to the cleaned path, or 400.
    for (const [path, want] of [
      [`${prefix}//about.html`, `${prefix}/about.html`],
      [`${prefix}/docs//page.html?q=1`, `${prefix}/docs/page.html`],
    ]) {
      const r = await raw(path);
      assert.equal(r.status, 301, `${path}: HTTP ${r.status}`);
      const loc = new URL(r.headers.location, base);
      assert.equal(loc.pathname, want, `${path} → ${r.headers.location}`);
      if (path.includes("?")) assert.equal(loc.search, "?q=1", "the query is kept");
    }
    const post = await raw(`${prefix}/x//__backyard/api/db/c`, write);
    assert.equal(post.status, 400, `POST to a non-canonical path: HTTP ${post.status} ${post.body}`);
    // Dot segments: a server redirects them like "//". Some runtimes resolve
    // them before the application sees the request (the Cloudflare Workers
    // runtime and edge do); the request is then served as the cleaned path.
    // Either way no handler ever sees a non-canonical path.
    for (const [path, want] of [
      [`${prefix}/docs/./page.html`, `${prefix}/docs/page.html`],
      [`${prefix}/docs/../about.html`, `${prefix}/about.html`],
      [`${prefix}/docs/%2e%2e/about.html`, `${prefix}/about.html`],
    ]) {
      const r = await raw(path);
      if (r.status === 301) {
        assert.equal(new URL(r.headers.location, base).pathname, want, `${path} → ${r.headers.location}`);
        continue;
      }
      const clean = await raw(want);
      assert.equal(r.status, 200, `${path}: HTTP ${r.status} (want 301, or the cleaned path's response)`);
      assert.deepEqual(r.body, clean.body, `${path} serves ${want}`);
    }
    const dots = await raw(`${prefix}/x/../__backyard/api/db/c`, write);
    assert.ok(dots.status === 400 || dots.status === 201, `POST with dot segments: 400, or served as the cleaned path: HTTP ${dots.status} ${dots.body}`);
  });
});

describe("SDK", { skip }, () => {
  before(async () => {
    const r = await client.get(url(site));
    if (r.status !== 200) await deploySite(client, env, site, files);
  });

  const preamble = (text) => {
    const nl = text.indexOf("\n");
    const line = text.slice(0, nl);
    const m = /^globalThis\.__BACKYARD__ = (\{.*\});$/.exec(line);
    assert.ok(m, `SDK preamble line: ${line.slice(0, 200)}`);
    return { ctx: JSON.parse(m[1]), rest: text.slice(nl + 1) };
  };

  test("backyard.js and backyard.mjs start with the per-request preamble", async () => {
    for (const name of ["backyard.js", "backyard.mjs"]) {
      const r = expectStatus(await new Client({ token: null }).get(url(site, `__backyard/${name}`)), 200, name);
      assert.match(r.header("content-type"), /^(text|application)\/javascript\b/, name);
      const { ctx, rest } = preamble(r.text);
      assert.deepEqual(ctx, { v: 1, mode: env.info.url_mode, site, prefix: "/s/", hub: env.info.public_url }, `${name} preamble`);
      const want = repoSDK(name);
      if (want) assert.ok(rest === want.toString("utf8"), `${name} is served byte-identical to web/sdk/${name} (set BACKYARD_SKIP_SDK_BYTES=1 for another version)`);
      const hub = expectStatus(await client.get(`${hubURL}/__backyard/${name}`), 200);
      const h = preamble(hub.text);
      assert.equal(h.ctx.site, null, "no site on the hub");
      assert.equal(h.rest, rest, "hub and site serve the same SDK");
    }
  });

  test("backyard.d.ts is served", async () => {
    const r = expectStatus(await client.get(url(site, "__backyard/backyard.d.ts")), 200);
    assert.match(r.text, /declare/);
    const want = repoSDK("backyard.d.ts");
    if (want) assert.ok(r.text === want.toString("utf8"), "backyard.d.ts is byte-identical to web/sdk");
  });

  test("the SDK is cacheable with validators", async () => {
    const r = expectStatus(await client.get(url(site, "__backyard/backyard.js")), 200);
    assert.ok(r.header("etag"));
    expectStatus(await client.get(url(site, "__backyard/backyard.js"), { headers: { "If-None-Match": r.header("etag") } }), 304);
    expectStatus(await client.head(url(site, "__backyard/backyard.js")), 200);
    const p = await client.post(url(site, "__backyard/backyard.js"), {});
    assert.equal(p.status, 405, `${p}`);
  });
});

describe("hub", () => {
  test("health endpoints answer on the hub and on sites without authentication", async () => {
    const anon = new Client({ token: null });
    const h = expectStatus(await anon.get(hubURL + "/__backyard/healthz"), 200);
    assert.match(h.text, /^ok\b/);
    expectStatus(await anon.get(hubURL + "/__backyard/readyz"), 200);
    const s = expectStatus(await anon.get(url(site, "__backyard/healthz")), 200);
    assert.match(s.text, /^ok\b/);
  });

  test("hub pages and agent docs", async () => {
    const page = expectStatus(await client.get(hubURL + "/"), 200);
    assert.match(page.header("content-type"), /^text\/html/);
    const llms = expectStatus(await client.get(hubURL + "/llms.txt"), 200);
    assert.match(llms.header("content-type"), /^text\/plain/);
    assert.match(llms.text, /backyard/i);
    expectStatus(await client.get(hubURL + "/llms-full.txt"), 200);
    expectStatus(await client.get(hubURL + "/docs"), 200);
    const md = expectStatus(await client.get(hubURL + "/docs/skill.md"), 200);
    assert.match(md.header("content-type"), /^text\/markdown/);
  });

  test("unknown reserved hub paths are JSON 404", async () => {
    assertError(await client.get(hubURL + "/__backyard/nope"), 404, "not_found");
    assertError(await client.get(hubURL + "/__backyard/api/nope"), 404, "not_found");
  });
});
