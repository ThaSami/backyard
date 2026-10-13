/** Uploaded files (SPEC §5.3) and guardrails (SPEC §16). */
import { describe, expect, it } from "vitest";
import { apiHeaders, call, deploy, sha256Hex, tokenHeaders, uniqueSite } from "./helpers";

function form(file: Blob, name: string, extra: Record<string, string> = {}): FormData {
  const f = new FormData();
  for (const [k, v] of Object.entries(extra)) f.append(k, v);
  f.append("file", file, name);
  return f;
}

async function upload(site: string, body: FormData, env = {}, headers: Record<string, string> = {}): Promise<Response> {
  return call(`/s/${site}/__backyard/api/files`, { method: "POST", headers: { "X-Backyard": "1", ...headers }, body }, env);
}

describe("files API", () => {
  it("uploads, lists, serves and deletes files", async () => {
    const site = uniqueSite("files");
    let res = await upload(site, form(new Blob(["hello, world"], { type: "text/plain" }), "notes/../hello world.txt"));
    expect(res.status).toBe(201);
    const info = (await res.json()) as Record<string, unknown>;
    expect(Object.keys(info)).toEqual(["id", "name", "url", "size", "content_type", "created_at", "created_by"]);
    expect(info).toMatchObject({ name: "hello world.txt", size: 12, content_type: "text/plain" });
    expect(info.url).toBe(`/s/${site}/__backyard/files/${info.id}/hello%20world.txt`);
    expect(String(info.created_by)).toMatch(/^anon:/);

    res = await upload(site, form(new Blob([new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1])]), "blob"), {}, {});
    const png = (await res.json()) as Record<string, unknown>;
    expect(png.content_type).toBe("image/png");
    res = await upload(site, form(new Blob(["<svg xmlns='http://www.w3.org/2000/svg'/>"], { type: "image/svg+xml" }), "x.svg"), {}, {});
    const svg = (await res.json()) as Record<string, string>;
    res = await upload(site, form(new Blob(["renamed"]), "orig.txt", { name: "custom.txt" }));
    expect(((await res.json()) as { name: string }).name).toBe("custom.txt");

    res = await call(String(info.url));
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("hello, world");
    expect(res.headers.get("Content-Type")).toBe("text/plain; charset=utf-8");
    expect(res.headers.get("Content-Disposition")).toBe('inline; filename="hello world.txt"');
    expect(res.headers.get("Cache-Control")).toBe("private, no-cache");
    res = await call(String(info.url), { headers: { Range: "bytes=7-" } });
    expect(res.status).toBe(206);
    expect(await res.text()).toBe("world");
    res = await call(svg.url!);
    expect(res.headers.get("Content-Security-Policy")).toBe("sandbox");

    res = await call(`/s/${site}/__backyard/api/files?limit=2`);
    const page = (await res.json()) as { files: { name: string }[]; has_more: boolean };
    expect(page.files.map((f) => f.name)).toEqual(["custom.txt", "x.svg"]);
    expect(page.has_more).toBe(true);
    res = await call(`/s/${site}/__backyard/api/files/${info.id}`);
    expect(((await res.json()) as { name: string }).name).toBe("hello world.txt");
    expect((await call(`/s/${site}/__backyard/api/files/nope`)).status).toBe(404);

    res = await call(`/s/${site}/__backyard/api/files/${info.id}`, { method: "DELETE", headers: { "X-Backyard": "1" } });
    expect(res.status).toBe(204);
    expect((await call(String(info.url))).status).toBe(404);
    expect((await call(`/s/${site}/__backyard/api/files/${info.id}`, { method: "DELETE", headers: { "X-Backyard": "1" } })).status).toBe(204);
  });

  it("rejects malformed uploads", async () => {
    const site = uniqueSite("badup");
    let res = await call(`/s/${site}/__backyard/api/files`, { method: "POST", headers: apiHeaders(), body: "{}" });
    expect(res.status).toBe(400);
    const f = new FormData();
    f.append("other", "x");
    res = await upload(site, f);
    expect(res.status).toBe(400);
    const two = new FormData();
    two.append("file", new Blob(["a"]), "a");
    two.append("file", new Blob(["b"]), "b");
    expect((await upload(site, two)).status).toBe(400);
    res = await call(`/s/${site}/__backyard/api/files`, { method: "POST", body: form(new Blob(["x"]), "x") });
    expect(res.status).toBe(403);
  });

  it("stores large uploads in R2 when bound", async () => {
    const site = uniqueSite("bigup");
    const big = new Uint8Array(100_000).map((_, i) => i % 199);
    const res = await upload(site, form(new Blob([big], { type: "application/octet-stream" }), "big.bin"));
    expect(res.status).toBe(201);
    const info = (await res.json()) as { url: string; size: number };
    expect(info.size).toBe(100_000);
    const got = await call(info.url);
    expect(new Uint8Array(await got.arrayBuffer())).toEqual(big);
    const part = await call(info.url, { headers: { Range: "bytes=99990-" } });
    expect(new Uint8Array(await part.arrayBuffer())).toEqual(big.subarray(99990));
  });
});

describe("guardrails", () => {
  it("caps uploads per user/IP per day", async () => {
    const site = uniqueSite("upcap");
    const env = { LIMITS: JSON.stringify({ uploads_per_day: 1 }) };
    const ip = { "CF-Connecting-IP": `198.51.100.${Math.floor(Math.random() * 250)}` };
    expect((await upload(site, form(new Blob(["1"]), "1.txt"), env, ip)).status).toBe(201);
    const res = await upload(site, form(new Blob(["2"]), "2.txt"), env, ip);
    expect(res.status).toBe(429);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("daily upload limit");
  });

  it("caps deploys per day and the number of sites", async () => {
    const files = [{ path: "index.html", size: 1, sha256: await sha256Hex("x") }];
    const post = (site: string, env: Record<string, string>) =>
      call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: tokenHeaders(), body: JSON.stringify({ files }) }, env);

    // Count the existing sites to place the platform-wide cap right at the limit.
    const sites = ((await (await call("/__backyard/api/deploy/sites", { headers: tokenHeaders() })).json()) as { sites: unknown[] }).sites.length;
    const existing = uniqueSite("cap-existing");
    await deploy(existing, { "index.html": "x" });
    const capEnv = { LIMITS: JSON.stringify({ max_sites: sites + 1, deploys_per_day: 100000 }) };
    const res = await post(uniqueSite("cap-new"), capEnv);
    expect(res.status).toBe(413);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("limit of");
    // Redeploying an existing site is still allowed.
    expect((await post(existing, capEnv)).status).toBe(200);

    const daily = { ALLOW_ANONYMOUS_DEPLOY: "true", LIMITS: JSON.stringify({ deploys_per_day: 2, max_sites: 100000 }) };
    const ip = `192.0.2.${Math.floor(Math.random() * 250)}`;
    const anonPost = (site: string) =>
      call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: { "X-Backyard": "1", "CF-Connecting-IP": ip }, body: JSON.stringify({ files }) }, daily);
    const s = uniqueSite("daily");
    expect((await anonPost(s)).status).toBe(200);
    expect((await anonPost(s)).status).toBe(200);
    const third = await anonPost(s);
    expect(third.status).toBe(429);
    expect(third.headers.get("Retry-After")).toMatch(/^\d+$/);
  });
});
