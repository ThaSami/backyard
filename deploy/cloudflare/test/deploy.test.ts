import { describe, expect, it } from "vitest";
import { issueAPIToken } from "../src/jwt";
import { call, deploy, DEPLOY_TOKEN, SECRET, sha256Hex, tokenHeaders, uniqueSite } from "./helpers";

const INDEX = `<!doctype html><html><head><title>Lunch &amp; Vote</title>
<meta name="description" content="Pick   lunch together"></head><body>hi</body></html>`;

describe("deploy gateway protocol", () => {
  it("advertises /info without auth", async () => {
    const res = await call("/__backyard/api/deploy/info");
    expect(res.status).toBe(200);
    const info = (await res.json()) as Record<string, unknown>;
    expect(info).toMatchObject({
      name: "Backyard",
      public_url: "https://backyard.test",
      url_mode: "path",
      gateway_url: "https://backyard.test/__backyard/api/deploy",
      methods: ["api"],
      edition: "cloudflare",
      auth: { mode: "none", anonymous_deploy: false },
    });
    expect((info.limits as Record<string, number>).max_files).toBeGreaterThan(0);
    // The hub alias of /info is the same document.
    const hub = await call("/__backyard/api/info");
    expect(((await hub.json()) as Record<string, unknown>).gateway_url).toBe(info.gateway_url);
  });

  it("deploys a site and serves it with the static rules of SPEC §3", async () => {
    const site = uniqueSite("deploy");
    const out = await deploy(site, {
      "index.html": INDEX,
      "about.html": "<p>about</p>",
      "docs/index.html": "<p>docs</p>",
      "app.js": "console.log(1)",
      "data/x.json": '{"a":1}',
      "404.html": "<p>custom 404</p>",
    });
    expect(out.create.uploads).toHaveLength(6);
    expect(out.create.unchanged).toEqual([]);
    expect(out.commit.url).toBe(`https://backyard.test/s/${site}/`);
    expect(out.commit.deleted).toBe(0);
    expect(out.commit.site).toMatchObject({
      name: site,
      url: `https://backyard.test/s/${site}/`,
      title: "Lunch & Vote",
      description: "Pick lunch together",
      file_count: 6,
      created_by: "token:deploy",
      owners: [],
    });

    // index
    let res = await call(`/s/${site}/`);
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("text/html; charset=utf-8");
    expect(res.headers.get("Cache-Control")).toBe("no-cache");
    expect(res.headers.get("X-Content-Type-Options")).toBe("nosniff");
    expect(res.headers.get("Referrer-Policy")).toBe("strict-origin-when-cross-origin");
    expect(res.headers.get("X-Frame-Options")).toBeNull();
    expect(await res.text()).toBe(INDEX);
    const etag = res.headers.get("ETag")!;
    expect(etag).toMatch(/^"[0-9a-f]{32}"$/);
    expect(res.headers.get("Last-Modified")).toMatch(/GMT$/);

    // conditional GET
    res = await call(`/s/${site}/`, { headers: { "If-None-Match": etag } });
    expect(res.status).toBe(304);
    expect(await res.text()).toBe("");
    res = await call(`/s/${site}/index.html`, { headers: { "If-None-Match": `W/${etag}` } });
    expect(res.status).toBe(304);

    // clean URLs and directory redirects
    res = await call(`/s/${site}/about`);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("<p>about</p>");
    res = await call(`/s/${site}/docs?x=1`);
    expect(res.status).toBe(301);
    expect(res.headers.get("Location")).toBe(`/s/${site}/docs/?x=1`);
    res = await call(`/s/${site}/docs/`);
    expect(await res.text()).toBe("<p>docs</p>");

    // content types
    res = await call(`/s/${site}/app.js`);
    expect(res.headers.get("Content-Type")).toBe("text/javascript; charset=utf-8");
    res = await call(`/s/${site}/data/x.json`);
    expect(res.headers.get("Content-Type")).toBe("application/json");

    // HEAD and Range
    res = await call(`/s/${site}/app.js`, { method: "HEAD" });
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Length")).toBe("14");
    expect(await res.text()).toBe("");
    res = await call(`/s/${site}/app.js`, { headers: { Range: "bytes=0-6" } });
    expect(res.status).toBe(206);
    expect(res.headers.get("Content-Range")).toBe("bytes 0-6/14");
    expect(await res.text()).toBe("console");
    res = await call(`/s/${site}/app.js`, { headers: { Range: "bytes=-3" } });
    expect(await res.text()).toBe("(1)");
    res = await call(`/s/${site}/app.js`, { headers: { Range: "bytes=99-" } });
    expect(res.status).toBe(416);
    expect(res.headers.get("Content-Range")).toBe("bytes */14");

    // custom 404 page
    res = await call(`/s/${site}/nope`);
    expect(res.status).toBe(404);
    expect(await res.text()).toBe("<p>custom 404</p>");
    expect(res.headers.get("Cache-Control")).toBe("no-store");

    // methods
    res = await call(`/s/${site}/`, { method: "POST" });
    expect(res.status).toBe(405);
    expect(res.headers.get("Allow")).toBe("GET, HEAD");

    // the reserved prefix is never shadowed by site files
    res = await call(`/s/${site}/__backyard/nothing`);
    expect(res.status).toBe(404);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("not_found");
  });

  it("serves the SPA fallback and the built-in pages", async () => {
    const site = uniqueSite("spa");
    await deploy(site, { "index.html": "<p>home</p>", "200.html": "<p>spa</p>" });
    let res = await call(`/s/${site}/some/client/route`);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("<p>spa</p>");

    const plain = uniqueSite("plain");
    await deploy(plain, { "index.html": "<p>home</p>" });
    res = await call(`/s/${plain}/missing.png`, { headers: { Accept: "text/html" } });
    expect(res.status).toBe(404);
    expect(await res.text()).toContain("Page not found");
    res = await call(`/s/${plain}/missing.png`, { headers: { Accept: "application/json" } });
    expect(res.status).toBe(404);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("not_found");

    res = await call(`/s/${uniqueSite("nosite")}/`);
    expect(res.status).toBe(404);
    const html = await res.text();
    expect(html).toContain("Site not found");
    expect(html).toContain('href="https://backyard.test/"');

    res = await call(`/s/${plain}`);
    expect(res.status).toBe(301);
    expect(res.headers.get("Location")).toBe(`/s/${plain}/`);
  });

  it("diffs unchanged files and deletes stale ones on commit", async () => {
    const site = uniqueSite("diff");
    await deploy(site, { "index.html": "<p>v1</p>", "a.js": "a", "b.css": "b", "old.txt": "old" });
    const second = await deploy(site, { "index.html": "<p>v2</p>", "a.js": "a", "b.css": "b", "new.txt": "new" });
    expect(second.create.unchanged.sort()).toEqual(["a.js", "b.css"]);
    expect(second.create.uploads.map((u) => u.path).sort()).toEqual(["index.html", "new.txt"]);
    expect(second.commit.deleted).toBe(1);
    expect(await (await call(`/s/${site}/`)).text()).toBe("<p>v2</p>");
    expect((await call(`/s/${site}/old.txt`)).status).toBe(404);
    expect(await (await call(`/s/${site}/new.txt`)).text()).toBe("new");

    const files = await call(`/__backyard/api/deploy/sites/${site}/files`, { headers: tokenHeaders() });
    const list = ((await files.json()) as { files: { path: string; size: number; sha256: string; updated_at: string }[] }).files;
    expect(list.map((f) => f.path)).toEqual(["a.js", "b.css", "index.html", "new.txt"]);
    expect(list[0]!.sha256).toBe(await sha256Hex("a"));
    expect(list[0]!.updated_at).toMatch(/^\d{4}-\d\d-\d\dT/);

    const raw = await call(`/__backyard/api/deploy/sites/${site}/files/index.html`, { headers: tokenHeaders() });
    expect(raw.status).toBe(200);
    expect(raw.headers.get("Content-Disposition")).toBe("attachment");
    expect(raw.headers.get("Content-Security-Policy")).toContain("sandbox");
    expect(await raw.text()).toBe("<p>v2</p>");
    expect((await call(`/__backyard/api/deploy/sites/${site}/files/missing.txt`, { headers: tokenHeaders() })).status).toBe(404);

    // A third deploy of identical content uploads nothing.
    const third = await deploy(site, { "index.html": "<p>v2</p>", "a.js": "a", "b.css": "b", "new.txt": "new" });
    expect(third.create.uploads).toEqual([]);
    expect(third.commit.deleted).toBe(0);
  });

  it("lists, describes, refreshes and deletes sites", async () => {
    const site = uniqueSite("meta");
    await deploy(site, { "index.html": "<title>Meta</title>" });
    let res = await call(`/__backyard/api/deploy/sites`, { headers: tokenHeaders() });
    const sites = ((await res.json()) as { sites: { name: string }[] }).sites;
    expect(sites.some((s) => s.name === site)).toBe(true);

    res = await call(`/__backyard/api/deploy/sites/${site}`, { headers: tokenHeaders() });
    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ name: site, title: "Meta", file_count: 1, owners: [] });

    res = await call(`/__backyard/api/deploy/sites/${site}/refresh`, { method: "POST", headers: tokenHeaders() });
    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ name: site, title: "Meta", file_count: 1 });

    // hub API and site API views
    res = await call(`/__backyard/api/sites`);
    expect(((await res.json()) as { sites: { name: string }[] }).sites.some((s) => s.name === site)).toBe(true);
    res = await call(`/s/${site}/__backyard/api/site`);
    expect(await res.json()).toMatchObject({ name: site, url: `https://backyard.test/s/${site}/` });

    // owners are unsupported unless OWNERSHIP=owner
    res = await call(`/__backyard/api/deploy/sites/${site}/owners`, { method: "POST", headers: tokenHeaders(), body: "{}" });
    expect(res.status).toBe(501);

    res = await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: tokenHeaders() });
    expect(res.status).toBe(204);
    res = await call(`/__backyard/api/deploy/sites/${site}`, { headers: tokenHeaders() });
    expect(res.status).toBe(404);
    res = await call(`/s/${site}/`);
    expect(res.status).toBe(404);
    expect(await res.text()).toContain("Site not found");
    res = await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: tokenHeaders() });
    expect(res.status).toBe(404);
  });

  it("requires a token to deploy unless anonymous deploys are allowed", async () => {
    const site = uniqueSite("anon");
    const body = JSON.stringify({ files: [{ path: "index.html", size: 1, sha256: await sha256Hex("x") }] });
    let res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: { "X-Backyard": "1" }, body });
    expect(res.status).toBe(401);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("unauthenticated");
    res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: tokenHeaders("wrong-token"), body });
    expect(res.status).toBe(401);
    res = await call(`/__backyard/api/deploy/me`, { headers: tokenHeaders() });
    expect(await res.json()).toMatchObject({ id: "token:deploy", name: "deploy", admin: true, anonymous: false });

    // Playground mode.
    const out = await deploy(site, { "index.html": "x" }, { token: "", envOverrides: { ALLOW_ANONYMOUS_DEPLOY: "true" } });
    expect(out.commit.site).toMatchObject({ name: site });
    expect(String(out.commit.site.created_by)).toMatch(/^anon:[0-9a-f]{16}$/);
    const info = (await (await call("/__backyard/api/deploy/info", {}, { ALLOW_ANONYMOUS_DEPLOY: "true" })).json()) as { auth: { anonymous_deploy: boolean } };
    expect(info.auth.anonymous_deploy).toBe(true);
    const ok = await call(
      `/__backyard/api/deploy/sites/${site}/deploys`,
      { method: "POST", headers: { "X-Backyard": "1", "Content-Type": "application/json" }, body },
      { ALLOW_ANONYMOUS_DEPLOY: "true" },
    );
    expect(ok.status).toBe(200);
    // Deleting still needs a real identity.
    res = await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: { "X-Backyard": "1" } }, { ALLOW_ANONYMOUS_DEPLOY: "true" });
    expect(res.status).toBe(401);
  });

  it("treats a token minted for a guest as that guest, like the Go gateway", async () => {
    // AUTH_MODE=none lets guests mint API tokens (the CLI browser login).
    const guest = { id: "anon:0123456789abcdef", name: "Guest 0123", anonymous: true, admin: false };
    const { token } = await issueAPIToken(SECRET, guest, 3600);
    const site = uniqueSite("guest");
    const body = JSON.stringify({ files: [{ path: "index.html", size: 1, sha256: await sha256Hex("x") }] });
    const anonymous = { ALLOW_ANONYMOUS_DEPLOY: "true" };
    const errorOf = async (res: Response) => ((await res.json()) as { error: { code: string; message: string } }).error;

    // It reads as that guest.
    let res = await call("/__backyard/api/deploy/me", { headers: tokenHeaders(token) });
    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ id: guest.id, anonymous: true, admin: false });
    expect((await call("/__backyard/api/deploy/sites", { headers: tokenHeaders(token) })).status).toBe(200);

    // Without ALLOW_ANONYMOUS_DEPLOY it cannot deploy, and is told why.
    res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: tokenHeaders(token), body });
    expect(res.status).toBe(401);
    let e = await errorOf(res);
    expect(e.code).toBe("unauthenticated");
    expect(e.message).toContain("anonymous guest, which cannot deploy");

    // With it, the guest deploys and refreshes as itself (never as an owner)...
    const out = await deploy(site, { "index.html": "guest" }, { token, envOverrides: { ...anonymous, OWNERSHIP: "owner" } });
    expect(out.commit.site).toMatchObject({ name: site, created_by: guest.id, updated_by: guest.id, owners: [] });
    res = await call(`/__backyard/api/deploy/sites/${site}/refresh`, { method: "POST", headers: tokenHeaders(token) }, anonymous);
    expect(res.status).toBe(200);

    // ...but never deletes sites or changes owners.
    res = await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: tokenHeaders(token) }, anonymous);
    expect(res.status).toBe(401);
    e = await errorOf(res);
    expect(e.message).toContain("anonymous guest, which cannot delete sites or change owners");
    res = await call(
      `/__backyard/api/deploy/sites/${site}/owners`,
      { method: "POST", headers: tokenHeaders(token), body: JSON.stringify({ add: ["me@example.com"] }) },
      { ...anonymous, OWNERSHIP: "owner" },
    );
    expect(res.status).toBe(401);

    // Guest tokens are only valid in AUTH_MODE=none; an invalid token is never a guest.
    const access = { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: "aud" };
    res = await call("/__backyard/api/deploy/me", { headers: tokenHeaders(token) }, access);
    expect(res.status).toBe(401);
    res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: tokenHeaders(token.slice(0, -2) + "xx"), body }, anonymous);
    expect(res.status).toBe(401);
    expect((await errorOf(res)).message).toContain("invalid or expired token");

    // A real identity can still delete the site.
    res = await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: tokenHeaders() });
    expect(res.status).toBe(204);
  });

  it("validates deploy requests", async () => {
    const site = uniqueSite("val");
    const sha = await sha256Hex("x");
    const post = (s: string, files: unknown) =>
      call(`/__backyard/api/deploy/sites/${s}/deploys`, { method: "POST", headers: tokenHeaders(), body: JSON.stringify({ files }) });
    expect((await post("api", [{ path: "index.html", size: 1, sha256: sha }])).status).toBe(400); // reserved
    expect((await post("Bad_Name", [{ path: "index.html", size: 1, sha256: sha }])).status).toBe(400);
    expect((await post(site, [])).status).toBe(400);
    expect((await post(site, [{ path: "../x", size: 1, sha256: sha }])).status).toBe(400);
    expect((await post(site, [{ path: "/abs", size: 1, sha256: sha }])).status).toBe(400);
    expect((await post(site, [{ path: "__backyard/x", size: 1, sha256: sha }])).status).toBe(400);
    expect((await post(site, [{ path: "a", size: 1, sha256: "zz" }])).status).toBe(400);
    expect((await post(site, [{ path: "a", size: -1, sha256: sha }])).status).toBe(400);
    expect(
      (await post(site, [
        { path: "a", size: 1, sha256: sha },
        { path: "a", size: 1, sha256: sha },
      ])).status,
    ).toBe(400);
    expect(
      (await post(site, [
        { path: "a", size: 1, sha256: sha },
        { path: "a/b", size: 1, sha256: sha },
      ])).status,
    ).toBe(400);
    expect((await post(site, [{ path: "big.bin", size: 200 * 1024 * 1024, sha256: sha }])).status).toBe(413);

    // No ".git" segment, in any case, anywhere in a path (deploy and read).
    for (const path of [".git/config", "a/.GIT/HEAD", "x/.Git"]) {
      const r = await post(site, [
        { path: "index.html", size: 1, sha256: sha },
        { path, size: 1, sha256: sha },
      ]);
      expect(r.status, path).toBe(400);
      expect(await r.json(), path).toEqual({
        error: { code: "bad_request", message: `invalid path ${JSON.stringify(path)}: path may not contain a ".git" segment` },
      });
    }
    for (const path of [".git/config", "sub/.GIT/HEAD"]) {
      const r = await call(`/__backyard/api/deploy/sites/${site}/files/${path}`, { headers: tokenHeaders() });
      expect(r.status, path).toBe(400);
      expect(((await r.json()) as { error: { code: string } }).error.code).toBe("bad_request");
    }

    // CSRF: no header and no bearer → 403; bearer without browser signals is fine.
    let res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", body: "{}" });
    expect(res.status).toBe(403);
    res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, {
      method: "POST",
      headers: { Authorization: `Bearer ${DEPLOY_TOKEN}`, Origin: "https://evil.example" },
      body: "{}",
    });
    expect(res.status).toBe(403);

    res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "GET", headers: tokenHeaders() });
    expect(res.status).toBe(405);
    expect(res.headers.get("Allow")).toBe("POST");
    res = await call(`/__backyard/api/deploy/nope`, { headers: tokenHeaders() });
    expect(res.status).toBe(404);
  });

  it("verifies uploads and commits", async () => {
    const site = uniqueSite("verify");
    const content = "hello world";
    const sha = await sha256Hex(content);
    const files = [{ path: "index.html", size: content.length, sha256: sha }];
    let res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: tokenHeaders(), body: JSON.stringify({ files }) });
    const plan = (await res.json()) as { deploy_id: string; uploads: { url: string; headers: Record<string, string> }[] };
    const up = plan.uploads[0]!;
    expect(up.url).toMatch(/^https:\/\/backyard\.test\/__backyard\/api\/deploy\/uploads\//);
    expect(up.headers["Content-Type"]).toBe("text/html; charset=utf-8");

    // commit before upload → 409
    const commit = () =>
      call(`/__backyard/api/deploy/sites/${site}/deploys/${encodeURIComponent(plan.deploy_id)}/commit`, {
        method: "POST",
        headers: tokenHeaders(),
        body: JSON.stringify({ files }),
      });
    res = await commit();
    expect(res.status).toBe(409);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("index.html (missing)");

    // wrong content, wrong length, forged token
    res = await call(up.url, { method: "PUT", body: "hello WORLD" });
    expect(res.status).toBe(400);
    res = await call(up.url, { method: "PUT", body: "hello world!!" });
    expect([400, 413]).toContain(res.status);
    const forged = up.url.replace(/uploads\/(.)/, (_m, c: string) => `uploads/${c === "e" ? "f" : "e"}`);
    res = await call(forged, { method: "PUT", body: content });
    expect(res.status).toBe(403);

    res = await call(up.url, { method: "PUT", body: content });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ path: "index.html", size: content.length, sha256: sha });
    res = await commit();
    expect(res.status).toBe(200);

    // a deploy id is bound to its site
    res = await call(`/__backyard/api/deploy/sites/${uniqueSite("other")}/deploys/${encodeURIComponent(plan.deploy_id)}/commit`, {
      method: "POST",
      headers: tokenHeaders(),
      body: JSON.stringify({ files }),
    });
    expect(res.status).toBe(400);
    res = await call(`/__backyard/api/deploy/sites/${site}/deploys/garbage/commit`, { method: "POST", headers: tokenHeaders(), body: JSON.stringify({ files }) });
    expect(res.status).toBe(400);
  });

  it("stores large files in R2 when bound and inline (chunked) otherwise", async () => {
    // FILE_INLINE_MAX is 64 KiB in the test config.
    const big = new Uint8Array(200_000).map((_, i) => i % 251);
    const r2site = uniqueSite("r2");
    await deploy(r2site, { "index.html": "x", "big.bin": big });
    let res = await call(`/s/${r2site}/big.bin`);
    expect(res.status).toBe(200);
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(big);
    res = await call(`/s/${r2site}/big.bin`, { headers: { Range: "bytes=100000-100009" } });
    expect(res.status).toBe(206);
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(big.subarray(100000, 100010));

    // Without R2 the bytes are chunked into the Durable Object (1.5 MiB chunks).
    const huge = new Uint8Array(3_500_000).map((_, i) => (i * 7) % 256);
    const inline = uniqueSite("inline");
    await deploy(inline, { "index.html": "x", "huge.bin": huge }, { envOverrides: { R2: undefined } });
    res = await call(`/s/${inline}/huge.bin`, {}, { R2: undefined });
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Length")).toBe("3500000");
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(huge);
    const start = 1_572_860; // across the first chunk boundary (1,572,864)
    res = await call(`/s/${inline}/huge.bin`, { headers: { Range: `bytes=${start}-${start + 9}` } }, { R2: undefined });
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(huge.subarray(start, start + 10));
  });

  it("serves sites in host mode", async () => {
    const site = uniqueSite("host");
    const env = { URL_MODE: "host", BASE_DOMAIN: "backyard.test" };
    const out = await deploy(site, { "index.html": "<p>host</p>" }, { envOverrides: env });
    expect(out.commit.url).toBe(`https://${site}.backyard.test/`);
    let res = await call(`https://${site}.backyard.test/`, {}, env);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("<p>host</p>");
    const sdk = await call(`https://${site}.backyard.test/__backyard/backyard.js`, {}, env);
    expect((await sdk.text()).split("\n")[0]).toBe(
      `globalThis.__BACKYARD__ = {"v":1,"mode":"host","site":"${site}","prefix":"/s/","hub":"https://backyard.test"};`,
    );
    res = await call(`https://a.b.backyard.test/`, {}, env);
    expect(res.status).toBe(404);
    // Site hosts redirect auth endpoints to the hub.
    res = await call(`https://${site}.backyard.test/__backyard/auth/login`, {}, env);
    expect(res.status).toBe(302);
    expect(res.headers.get("Location")).toBe(`https://backyard.test/__backyard/auth/login?rd=${encodeURIComponent(`https://${site}.backyard.test/`)}`);
  });
});
