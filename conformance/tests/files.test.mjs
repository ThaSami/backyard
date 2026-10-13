// File uploads (SPEC §5.3) and serving of uploaded files (§3).
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus, rawRequest } from "../lib/http.mjs";
import { assertFileInfo } from "../lib/shapes.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";
const site = uniqueSite("files");
const api = (p) => env.api(site, p);
const prefix = env.sitePrefix(site);
/** Absolute URL of a site-relative FileInfo.url. */
const abs = (u) => new URL(u, env.siteURL(site)).href;

function form(content, filename, type, name) {
  const fd = new FormData();
  if (name !== undefined) fd.append("name", name);
  fd.append("file", new Blob([content], type ? { type } : {}), filename);
  return fd;
}

const upload = (fd, opts = {}) => client.fetch(api("/files"), { method: "POST", body: fd, ...opts });

describe("files", { skip }, () => {
  let me;
  let hello;

  before(async () => {
    await deploySite(client, env, site, { "index.html": "files" });
    me = (await client.get(api("/me"))).json;
  });

  test("POST /files stores a file and returns FileInfo (201)", async () => {
    const r = expectStatus(await upload(form("hello world", "hello.txt", "text/plain")), 201);
    hello = assertFileInfo(r.json, { name: "hello.txt", size: 11, prefix, by: me.id });
    assert.match(hello.content_type, /^text\/plain\b/);
  });

  test("the file is served at its url with the upload headers", async () => {
    const r = expectStatus(await client.get(abs(hello.url)), 200);
    assert.equal(r.text, "hello world");
    assert.equal(r.header("content-type"), "text/plain; charset=utf-8");
    assert.match(r.header("content-disposition") || "", /^inline;\s*filename="?hello\.txt"?/);
    assert.equal(r.header("cache-control"), "private, no-cache");
    assert.equal(r.header("x-content-type-options"), "nosniff");
    assert.equal(r.header("content-security-policy"), null, "plain text is not sandboxed");
    const etag = r.header("etag");
    assert.ok(etag, "ETag");
    expectStatus(await client.get(abs(hello.url), { headers: { "If-None-Match": etag } }), 304);
    const part = expectStatus(await client.get(abs(hello.url), { headers: { Range: "bytes=0-4" } }), 206);
    assert.equal(part.text, "hello");
    assert.equal(part.header("content-range"), "bytes 0-4/11");
    const head = expectStatus(await client.head(abs(hello.url)), 200);
    assert.equal(head.body.length, 0);
    assert.equal(head.header("content-length"), "11");
    const post = await client.post(abs(hello.url), {});
    assert.equal(post.status, 405, `${post}`);
  });

  test("GET /files/{id} and GET /files", async () => {
    const one = expectStatus(await client.get(api(`/files/${hello.id}`)), 200).json;
    assert.deepEqual(one, hello);
    const second = expectStatus(await upload(form(randomBytes(300), "data.bin")), 201).json;
    const list = expectStatus(await client.get(api("/files")), 200).json;
    assert.equal(typeof list.has_more, "boolean");
    const ids = list.files.map((f) => f.id);
    assert.ok(ids.indexOf(second.id) >= 0 && ids.indexOf(second.id) < ids.indexOf(hello.id), "newest first");
    for (const f of list.files) assertFileInfo(f, { prefix });
    const page = expectStatus(await client.get(api("/files?limit=1")), 200).json;
    assert.equal(page.files.length, 1);
    assert.equal(page.has_more, true);
    assert.equal(page.files[0].id, ids[0]);
    const page2 = expectStatus(await client.get(api("/files?limit=1&offset=1")), 200).json;
    assert.equal(page2.files[0].id, ids[1]);
    assertError(await client.get(api("/files?limit=-1")), 400, "bad_request");
    assertError(await client.get(api("/files/NOSUCHFILE")), 404, "not_found");
    const d = expectStatus(await client.get(api("/db")), 200).json;
    assert.ok(!d.collections.some((c) => c.startsWith("_")), "upload metadata is not a user collection");
  });

  test('the "name" field overrides the file name; names are sanitized', async () => {
    const named = expectStatus(await upload(form("x", "orig.txt", "text/plain", "report final.txt")), 201).json;
    assertFileInfo(named, { name: "report final.txt", prefix });
    expectStatus(await client.get(abs(named.url)), 200, "an escaped name is served");
    const evil = expectStatus(await upload(form("x", "../../etc/passwd", "text/plain")), 201).json;
    assert.equal(evil.name, "passwd", "base name only");
    const weird = expectStatus(await upload(form("x", "a&b=c+d #1.txt", "text/plain")), 201).json;
    assertFileInfo(weird, { prefix });
    assert.equal(expectStatus(await client.get(abs(weird.url)), 200).text, "x");
  });

  test("active content is sandboxed", async () => {
    // Everything that is not known to be passive gets the sandbox (an
    // allowlist, SPEC §3): markup, XSLT, any +xml and unknown types too.
    for (const [file, type] of [
      ["page.html", "text/html"],
      ["pic.svg", "image/svg+xml"],
      ["doc.xml", "application/xml"],
      ["style.xsl", "text/xsl"],
      ["clip.vid", "video/x+xml"],
      ["blob.unknown", "application/x-unknown"],
    ]) {
      const f = expectStatus(await upload(form("<b>x</b>", file, type)), 201, file).json;
      const r = expectStatus(await client.get(abs(f.url)), 200, file);
      assert.match(r.header("content-security-policy") || "", /\bsandbox\b/, `${file} (${type}, served as ${r.header("content-type")}) gets CSP sandbox`);
    }
    const txt = expectStatus(await upload(form("<b>x</b>", "note.txt", "text/plain")), 201).json;
    const t = expectStatus(await client.get(abs(txt.url)), 200);
    assert.match(t.header("content-type"), /^text\/plain\b/);
    assert.equal(t.header("content-security-policy"), null, "plain text is not sandboxed");
    const png = Buffer.from("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8ffff3f0005fe02fea7d2a5b20000000049454e44ae426082", "hex");
    const p = expectStatus(await upload(form(png, "dot.png", "image/png")), 201).json;
    assert.equal(p.content_type, "image/png");
    const r = expectStatus(await client.get(abs(p.url)), 200);
    assert.equal(r.header("content-type"), "image/png");
    assert.equal(r.header("content-security-policy"), null, "images are not sandboxed");
    assert.deepEqual(r.body, png);
  });

  test("an empty file", async () => {
    const f = assertFileInfo(expectStatus(await upload(form("", "empty.txt", "text/plain")), 201).json, { name: "empty.txt", size: 0, prefix });
    const r = expectStatus(await client.get(abs(f.url)), 200);
    assert.equal(r.body.length, 0);
  });

  test("the content type is detected when the client sends none", async () => {
    const f = expectStatus(await upload(form("{\"a\":1}", "data.json")), 201).json;
    assert.match(f.content_type, /^application\/json\b/);
  });

  test("DELETE /files/{id} is 204 (also when missing) and removes the bytes", async () => {
    const f = expectStatus(await upload(form("bye", "bye.txt", "text/plain")), 201).json;
    expectStatus(await client.delete(api(`/files/${f.id}`)), 204);
    expectStatus(await client.delete(api(`/files/${f.id}`)), 204);
    assertError(await client.get(api(`/files/${f.id}`)), 404, "not_found");
    const gone = await client.get(abs(f.url));
    assert.equal(gone.status, 404, `${gone}`);
    const list = (await client.get(api("/files?limit=1000"))).json;
    assert.ok(!list.files.some((x) => x.id === f.id));
    assert.equal((await client.get(abs(f.url.replace(/\/[^/]*$/, "/other-name.txt")))).status, 404);
  });

  test("files are per site", async () => {
    const o = uniqueSite("files2");
    await deploySite(client, env, o, { "index.html": "x" });
    assertError(await client.get(env.api(o, `/files/${hello.id}`)), 404, "not_found");
    const r = await client.get(new URL(`${env.sitePrefix(o)}/__backyard/files/${hello.id}/hello.txt`, env.siteURL(o)).href);
    assert.equal(r.status, 404, `another site cannot serve the file: ${r}`);
  });

  test("invalid uploads are 400", async () => {
    assertError(await client.post(api("/files"), { file: "x" }), 400, "bad_request", "JSON body");
    const noFile = new FormData();
    noFile.append("name", "x.txt");
    assertError(await upload(noFile), 400, "bad_request", "no file field");
    const two = new FormData();
    two.append("file", new Blob(["a"]), "a.txt");
    two.append("file", new Blob(["b"]), "b.txt");
    assertError(await upload(two), 400, "bad_request", "two files");
  });

  test("an upload declared above files.max_upload_bytes is 413 too_large", async (t) => {
    // Only the headers and a few bytes are sent: the server refuses the
    // declared length before reading the body. Servers behind a proxy that
    // waits for the whole body (wrangler dev) cannot answer early: skipped.
    const target = api("/files");
    const headers = {
      Host: new URL(target).host,
      "Content-Type": "multipart/form-data; boundary=conformance",
      "Content-Length": String(200 * 1024 * 1024),
      "X-Backyard": "1",
    };
    const cookie = client.jar.header(target);
    if (cookie) headers.Cookie = cookie;
    if (client.token) headers.Authorization = `Bearer ${client.token}`;
    let r;
    try {
      r = await rawRequest(target, { method: "POST", headers, body: "--conformance\r\n", timeout: 5000 });
    } catch (e) {
      if (e.code === "ETIMEDOUT") return t.skip("the server does not answer before the declared body arrived");
      throw e;
    }
    assert.equal(r.status, 413, `HTTP ${r.status} ${r.body}`);
    assert.equal(JSON.parse(r.body.toString()).error.code, "too_large");
  });

  test("uploads need the CSRF header", async () => {
    assertError(await upload(form("x", "x.txt"), { csrf: false }), 403, "forbidden");
  });
});
