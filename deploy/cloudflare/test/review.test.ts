/**
 * Regression tests for the review pass: gateway auth parity with the Go
 * gateway, token size bounds, R2 object lifecycle, the platform site cap at
 * commit, "__proto__" data keys, AI provider availability, streamed multipart
 * uploads and post-login redirects.
 */
import { listDurableObjectIds } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { describe, expect, it } from "vitest";
import type { Env } from "../src/env";
import { getConfig } from "../src/config";
import { safeReturnTo } from "../src/hub";
import { issueAPIToken, verifyAPIToken } from "../src/jwt";
import { boundaryOf, indexOfBytes, MultipartReader, parseDisposition } from "../src/multipart";
import type { RequestContext } from "../src/router";
import { apiHeaders, call, deploy, SECRET, sha256Hex, tokenHeaders, uniqueSite } from "./helpers";

const bucket = (env as unknown as Env).R2!;

async function r2Keys(prefix: string): Promise<string[]> {
  const out: string[] = [];
  let cursor: string | undefined;
  do {
    const l = await bucket.list({ prefix, cursor });
    out.push(...l.objects.map((o) => o.key));
    cursor = l.truncated ? l.cursor : undefined;
  } while (cursor);
  return out;
}

type ErrorBody = { error: { code: string; message: string; login_url?: string } };

describe("gateway authentication (Go parity)", () => {
  it("treats a valid guest token as that guest: reads yes, deploys only in playground mode", async () => {
    const guest = await issueAPIToken(SECRET, { id: "anon:00112233445566ff", name: "Guest 0011", anonymous: true, admin: false }, 3600);
    const site = uniqueSite("guesttok");
    let res = await call("/__backyard/api/deploy/me", { headers: tokenHeaders(guest.token) });
    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ id: "anon:00112233445566ff", anonymous: true, admin: false });
    expect((await call("/__backyard/api/deploy/sites", { headers: tokenHeaders(guest.token) })).status).toBe(200);

    const body = JSON.stringify({ files: [{ path: "index.html", size: 1, sha256: await sha256Hex("x") }] });
    res = await call(`/__backyard/api/deploy/sites/${site}/deploys`, { method: "POST", headers: tokenHeaders(guest.token), body });
    expect(res.status).toBe(401);
    expect(((await res.json()) as ErrorBody).error.message).toContain("anonymous guest");

    const out = await deploy(site, { "index.html": "x" }, { token: guest.token, envOverrides: { ALLOW_ANONYMOUS_DEPLOY: "true" } });
    expect(out.commit.site.created_by).toBe("anon:00112233445566ff");
    // Deleting always needs a real identity.
    res = await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: tokenHeaders(guest.token) }, { ALLOW_ANONYMOUS_DEPLOY: "true" });
    expect(res.status).toBe(401);
    // An invalid token is an error, never a silent guest.
    res = await call("/__backyard/api/deploy/me", { headers: tokenHeaders("not-a-token") });
    expect(res.status).toBe(401);
  });

  it("adds a login_url to gateway 401s in access mode", async () => {
    const accessEnv = { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: "aud" };
    const res = await call("/__backyard/api/deploy/sites", { headers: tokenHeaders("bogus") }, accessEnv);
    expect(res.status).toBe(401);
    expect(((await res.json()) as ErrorBody).error.login_url).toBe("https://backyard.test/");
    const none = await call("/__backyard/api/deploy/sites", { headers: tokenHeaders("bogus") });
    expect(((await none.json()) as ErrorBody).error.login_url).toBeUndefined();
  });
});

describe("API token size", () => {
  it("stays verifiable: long names are cut, trailing groups dropped, important groups kept", async () => {
    const groups = Array.from({ length: 2000 }, (_, i) => `group-${i}-${"x".repeat(20)}`);
    groups.push("backyard-admins");
    const u = { id: "big@acme.com", email: "big@acme.com", name: "N".repeat(1000), groups, anonymous: false, admin: true };
    const { token } = await issueAPIToken(SECRET, u, 3600, undefined, (g) => g === "backyard-admins");
    expect(token.length).toBeLessThanOrEqual(16 << 10);
    const back = await verifyAPIToken(SECRET, token);
    expect(back.name).toHaveLength(256);
    expect(back.groups![0]).toBe("backyard-admins");
    expect(back.groups!.length).toBeGreaterThan(10);
    expect(back.groups!.length).toBeLessThan(groups.length);
    // Small profiles are unchanged.
    const small = await verifyAPIToken(SECRET, (await issueAPIToken(SECRET, { ...u, name: "Bob", groups: ["a", "b"] }, 60)).token);
    expect(small).toMatchObject({ name: "Bob", groups: ["a", "b"] });
  });
});

describe("R2 object lifecycle", () => {
  const r2Env = { FILE_INLINE_MAX: "16" };

  it("stores one R2 object per content, skips re-uploads and deletes everything with the site", async () => {
    const site = uniqueSite("r2life");
    const big = "B".repeat(1000);
    const out = await deploy(site, { "a.txt": big, "copy/a.txt": big, "index.html": "<title>x</title>" + "i".repeat(100) }, { envOverrides: r2Env });
    // Both paths with the same content were offered for upload; one copy is stored.
    expect(out.create.uploads.map((u) => u.path).sort()).toEqual(["a.txt", "copy/a.txt", "index.html"]);
    const keys = await r2Keys(`sites/${site}/`);
    expect(keys).toHaveLength(2);
    expect(keys.every((k) => /^sites\/[^/]+\/[0-9a-f]{64}\.[0-9a-f]{16}$/.test(k))).toBe(true);
    expect(await (await call(`/s/${site}/copy/a.txt`, {}, r2Env)).text()).toBe(big);
    expect(out.commit.site.title).toBe("x");

    // Moving content to a new path re-uses the stored object.
    const out2 = await deploy(site, { "moved.txt": big, "index.html": "<title>x</title>" + "i".repeat(100) }, { envOverrides: r2Env });
    expect(out2.create.uploads.map((u) => u.path)).toEqual(["moved.txt"]);
    expect(await r2Keys(`sites/${site}/`)).toHaveLength(2);
    expect(await (await call(`/s/${site}/moved.txt`, {}, r2Env)).text()).toBe(big);

    // Uploads (POST /files) live under uploads/<site>/.
    const f = new FormData();
    f.append("file", new Blob(["U".repeat(500)]), "u.txt");
    expect((await call(`/s/${site}/__backyard/api/files`, { method: "POST", headers: { "X-Backyard": "1" }, body: f }, r2Env)).status).toBe(201);
    expect(await r2Keys(`uploads/${site}/`)).toHaveLength(1);

    // DELETE answers after the R2 objects are gone.
    const res = await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: tokenHeaders() }, r2Env);
    expect(res.status).toBe(204);
    expect(await r2Keys(`sites/${site}/`)).toEqual([]);
    expect(await r2Keys(`uploads/${site}/`)).toEqual([]);
  });

  it("deletes R2 objects released by a commit", async () => {
    const site = uniqueSite("r2dup");
    const content = "D".repeat(300);
    await deploy(site, { "one.txt": content }, { envOverrides: r2Env });
    await deploy(site, { "two.txt": "other content".repeat(10) }, { envOverrides: r2Env });
    // one.txt's object was released by the second commit.
    await new Promise((r) => setTimeout(r, 50));
    const keys = await r2Keys(`sites/${site}/`);
    expect(keys).toHaveLength(1);
    expect(keys[0]).toContain(await sha256Hex("other content".repeat(10)));
  });
});

describe("platform site cap", () => {
  it("is checked again when a new site commits", async () => {
    const files = [{ path: "index.html", size: 1, sha256: await sha256Hex("x") }];
    const sites = ((await (await call("/__backyard/api/deploy/sites", { headers: tokenHeaders() })).json()) as { sites: unknown[] }).sites.length;
    const capEnv = { LIMITS: JSON.stringify({ max_sites: sites + 1 }) };
    const late = uniqueSite("cap-late");
    const res = await call(`/__backyard/api/deploy/sites/${late}/deploys`, { method: "POST", headers: tokenHeaders(), body: JSON.stringify({ files }) }, capEnv);
    expect(res.status).toBe(200);
    const created = (await res.json()) as { deploy_id: string; uploads: { url: string }[] };
    // Another site takes the last slot meanwhile.
    await deploy(uniqueSite("cap-first"), { "index.html": "x" }, { envOverrides: capEnv });
    const commit = await call(
      `/__backyard/api/deploy/sites/${late}/deploys/${encodeURIComponent(created.deploy_id)}/commit`,
      { method: "POST", headers: tokenHeaders(), body: JSON.stringify({ files }) },
      capEnv,
    );
    expect(commit.status).toBe(413);
    expect((await call(`/s/${late}/`)).status).toBe(404);
  });
});

describe("static probes of unknown sites", () => {
  it("answer 'site not found' without creating the site's Durable Object", async () => {
    const e = env as unknown as Env;
    const site = uniqueSite("probe");
    const id = e.SITE.idFromName(site).toString();
    for (const path of [`/s/${site}/`, `/s/${site}/index.html`, `/s/${site}/x/y`]) {
      const res = await call(path);
      expect(res.status).toBe(404);
      expect(await res.text()).toContain("Site not found");
    }
    expect((await listDurableObjectIds(e.SITE)).map((x) => x.toString())).not.toContain(id);
    // Once deployed it is served (and a deleted site is "not found" again).
    await deploy(site, { "index.html": "hello" });
    expect(await (await call(`/s/${site}/`)).text()).toBe("hello");
    expect((await call(`/__backyard/api/deploy/sites/${site}`, { method: "DELETE", headers: tokenHeaders() })).status).toBe(204);
    const gone = await call(`/s/${site}/`);
    expect(gone.status).toBe(404);
    expect(await gone.text()).toContain("Site not found");
  });
});

describe("document data keys", () => {
  it("stores a __proto__ key as data on create, put and patch", async () => {
    const site = uniqueSite("proto");
    const base = `/s/${site}/__backyard/api/db/things`;
    let res = await call(base, { method: "POST", headers: apiHeaders(), body: '{"id":"p1","__proto__":{"polluted":true},"a":1}' });
    expect(res.status).toBe(201);
    const text = await res.text();
    expect(Object.prototype.hasOwnProperty.call(JSON.parse(text), "__proto__")).toBe(true);
    expect(text).toContain('"__proto__":{"polluted":true}');
    res = await call(`${base}/p1`, { method: "PATCH", headers: apiHeaders(), body: '{"__proto__":{"v":2},"b":2}' });
    expect(await res.text()).toContain('"__proto__":{"v":2}');
    res = await call(`${base}/p1`, { method: "PATCH", headers: apiHeaders(), body: '{"__proto__":null}' });
    expect(await res.text()).not.toContain("__proto__");
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
  });
});

describe("AI provider availability", () => {
  const models = JSON.stringify({
    claude: { provider: "anthropic", model: "claude-opus-5-5", kind: "chat" },
    gpt: { provider: "openai", model: "gpt-x", kind: "chat" },
  });

  it("answers 501 for an alias whose provider is not configured and falls back for the default", async () => {
    const fake = {
      async run() {
        return { response: "hi" };
      },
    } as unknown as Ai;
    const site = uniqueSite("aiavail");
    const chat = (body: Record<string, unknown>, e: Record<string, unknown>) =>
      call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ messages: [{ role: "user", content: "x" }], ...body }) }, e);
    // gpt is configured but OPENAI_API_KEY is missing: a clear 501, not an upstream 502.
    let res = await chat({ model: "gpt" }, { AI_MODELS: models, ANTHROPIC_API_KEY: "k" });
    expect(res.status).toBe(501);
    expect(((await res.json()) as ErrorBody).error.message).toContain("OPENAI_API_KEY");
    // The default (smart) needs the AI binding; without it the first usable chat alias serves.
    const withLocal = JSON.stringify({ ...JSON.parse(models), smart: { provider: "workers-ai", model: "@cf/x", kind: "chat" } });
    const m = (await (await call(`/s/${site}/__backyard/api/ai/models`, {}, { AI: undefined, AI_MODELS: withLocal, OPENAI_API_KEY: "k" })).json()) as { default: string };
    expect(m.default).toBe("gpt");
    res = await chat({}, { AI: fake, AI_MODELS: withLocal });
    expect(((await res.json()) as { model: string }).model).toBe("smart");
  });
});

describe("streamed multipart uploads (R2)", () => {
  const r2Env = { FILE_INLINE_MAX: "1024" };

  function multipart(parts: { name: string; filename?: string; type?: string; data: string | Uint8Array }[], boundary = "XyZb0undary"): Uint8Array {
    const enc = new TextEncoder();
    const chunks: Uint8Array[] = [];
    for (const p of parts) {
      let h = `--${boundary}\r\nContent-Disposition: form-data; name="${p.name}"`;
      if (p.filename !== undefined) h += `; filename="${p.filename}"`;
      h += "\r\n";
      if (p.type) h += `Content-Type: ${p.type}\r\n`;
      chunks.push(enc.encode(h + "\r\n"), typeof p.data === "string" ? enc.encode(p.data) : p.data, enc.encode("\r\n"));
    }
    chunks.push(enc.encode(`--${boundary}--\r\n`));
    const total = chunks.reduce((n, c) => n + c.length, 0);
    const out = new Uint8Array(total);
    let off = 0;
    for (const c of chunks) {
      out.set(c, off);
      off += c.length;
    }
    return out;
  }

  /** A body stream delivered in small chunks (delimiters straddle chunk edges). */
  function chunked(bytes: Uint8Array, size: number): ReadableStream<Uint8Array> {
    let off = 0;
    return new ReadableStream({
      pull(c) {
        if (off >= bytes.length) return c.close();
        c.enqueue(bytes.slice(off, off + size));
        off += size;
      },
    });
  }

  const post = (site: string, body: BodyInit, e: Record<string, string> = r2Env, boundary = "XyZb0undary") =>
    call(`/s/${site}/__backyard/api/files`, { method: "POST", headers: { "X-Backyard": "1", "Content-Type": `multipart/form-data; boundary=${boundary}` }, body }, e);

  it("streams a file larger than one R2 part and keeps it byte-exact", async () => {
    const site = uniqueSite("mpbig");
    const size = 11 * 1024 * 1024 + 123;
    const data = new Uint8Array(size);
    for (let i = 0; i < size; i++) data[i] = (i * 31 + (i >> 8)) & 0xff;
    // A fake delimiter prefix inside the content must not end the part.
    data.set(new TextEncoder().encode("\r\n--XyZb0undar"), 5000);
    const res = await post(site, multipart([{ name: "name", data: "video.bin" }, { name: "file", filename: "orig.bin", type: "application/octet-stream", data }]));
    expect(res.status).toBe(201);
    const info = (await res.json()) as { url: string; size: number; name: string; content_type: string };
    expect(info).toMatchObject({ size, name: "video.bin", content_type: "application/octet-stream" });
    const got = new Uint8Array(await (await call(info.url, {}, r2Env)).arrayBuffer());
    expect(got.length).toBe(size);
    expect(await sha256Hex(got)).toBe(await sha256Hex(data));
  });

  it("parses fields in any order, sniffs types and ignores unknown fields", async () => {
    const site = uniqueSite("mporder");
    const png = new Uint8Array(4000);
    png.set([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]);
    const body = multipart([
      { name: "other", data: "ignored" },
      { name: "file", filename: "C:\\Users\\me\\pic", data: png },
      { name: "name", data: "  pic.png " },
    ]);
    const res = await post(site, chunked(body, 7));
    expect(res.status).toBe(201);
    const info = (await res.json()) as { name: string; content_type: string; size: number };
    expect(info).toMatchObject({ name: "pic.png", content_type: "image/png", size: 4000 });
  });

  it("rejects bad bodies without leaving objects behind", async () => {
    const site = uniqueSite("mpbad");
    const two = multipart([
      { name: "file", filename: "a.txt", data: "a".repeat(2000) },
      { name: "file", filename: "b.txt", data: "b".repeat(2000) },
    ]);
    let res = await post(site, chunked(two, 512));
    expect(res.status).toBe(400);
    expect(((await res.json()) as ErrorBody).error.message).toContain("only one file");
    res = await post(site, chunked(multipart([{ name: "file", data: "x".repeat(2000) }]), 512));
    expect(res.status).toBe(400);
    res = await post(site, chunked(multipart([{ name: "other", data: "x".repeat(2000) }]), 512));
    expect(((await res.json()) as ErrorBody).error.message).toContain('missing "file"');
    const truncated = multipart([{ name: "file", filename: "t.txt", data: "t".repeat(3000) }]).subarray(0, 2500);
    res = await post(site, chunked(truncated, 256));
    expect(res.status).toBe(400);
    res = await post(site, chunked(multipart([{ name: "file", filename: "big", data: "z".repeat(5000) }]), 1000), {
      ...r2Env,
      LIMITS: JSON.stringify({ max_upload_bytes: 4096 }),
    });
    expect(res.status).toBe(413);
    res = await call(
      `/s/${site}/__backyard/api/files`,
      { method: "POST", headers: { "X-Backyard": "1", "Content-Type": "multipart/form-data" }, body: chunked(new Uint8Array(2000), 500) },
      r2Env,
    );
    expect(res.status).toBe(400);
    await new Promise((r) => setTimeout(r, 50));
    expect(await r2Keys(`uploads/${site}/`)).toEqual([]);
  });

  it("limits buffered (inline) uploads without a Content-Length", async () => {
    const site = uniqueSite("mplimit");
    // Past the limit plus the framing allowance: the body is cut off while it is read.
    const body = multipart([{ name: "file", filename: "x.bin", data: new Uint8Array(1_200_000) }]);
    const noR2 = { R2: undefined, LIMITS: JSON.stringify({ max_upload_bytes: 1000 }) } as unknown as Record<string, string>;
    let res = await post(site, chunked(body, 64 * 1024), noR2);
    expect(res.status).toBe(413);
    // Within the allowance the file size itself is checked.
    res = await post(site, chunked(multipart([{ name: "file", filename: "y.bin", data: new Uint8Array(3000) }]), 256), noR2);
    expect(res.status).toBe(413);
  });
});

describe("multipart reader", () => {
  it("parses boundaries, dispositions and finds byte patterns", () => {
    expect(boundaryOf('multipart/form-data; boundary="a b:c"')).toBe("a b:c");
    expect(boundaryOf("Multipart/Form-Data; charset=utf-8; boundary=----WebKitFormBoundary7MA4YWxk")).toBe("----WebKitFormBoundary7MA4YWxk");
    expect(() => boundaryOf("multipart/form-data")).toThrow();
    expect(() => boundaryOf("text/plain; boundary=x")).toThrow();
    expect(parseDisposition('form-data; name="file"; filename="a\\"b.txt"')).toEqual({ name: "file", filename: 'a"b.txt' });
    expect(parseDisposition("form-data; name=f; filename*=UTF-8''%E2%82%AC.txt; filename=\"e.txt\"")).toEqual({ name: "f", filename: "€.txt" });
    expect(parseDisposition('form-data; name="x"')).toEqual({ name: "x", filename: null });
    expect(() => parseDisposition("attachment; name=x")).toThrow();
    const hay = new TextEncoder().encode("aab\r\n--b\r\n--bound");
    expect(indexOfBytes(hay, new TextEncoder().encode("\r\n--bound"))).toBe(8);
    expect(indexOfBytes(hay, new TextEncoder().encode("zz"))).toBe(-1);
  });

  it("reads parts with every chunking", async () => {
    const body = new TextEncoder().encode("preamble\r\n--bb\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nhello\r\n--bb  \r\nContent-Disposition: form-data; name=\"b\"; filename=\"f\"\r\nContent-Type: text/plain\r\n\r\n\r\n--b\r\n--bb--\r\nepilogue");
    for (const size of [1, 2, 3, 5, 8, 13, body.length]) {
      let off = 0;
      const stream = new ReadableStream<Uint8Array>({
        pull(c) {
          if (off >= body.length) return c.close();
          c.enqueue(body.slice(off, off + size));
          off += size;
        },
      });
      const r = new MultipartReader(stream, "bb");
      const a = await r.next();
      expect(a).toEqual({ name: "a", filename: null, contentType: "" });
      expect(await r.text(100)).toBe("hello");
      const b = await r.next();
      expect(b).toEqual({ name: "b", filename: "f", contentType: "text/plain" });
      const chunks: string[] = [];
      await r.body((c) => void chunks.push(new TextDecoder().decode(c)));
      expect(chunks.join("")).toBe("\r\n--b");
      expect(await r.next()).toBeNull();
    }
  });
});

describe("multipart reader (randomized)", () => {
  it("round-trips binary parts full of CR, LF and dashes under random chunking", async () => {
    let seed = 12345;
    const rnd = () => ((seed = (seed * 1103515245 + 12345) >>> 0) / 2 ** 32);
    const alphabet = [0x0d, 0x0a, 0x2d, 0x62, 0x00, 0xff];
    const enc = new TextEncoder();
    for (let round = 0; round < 40; round++) {
      const boundary = "b-" + Math.floor(rnd() * 1e9).toString(36);
      const parts = Array.from({ length: 1 + Math.floor(rnd() * 3) }, (_, i) => {
        const len = Math.floor(rnd() * 3000);
        const data = new Uint8Array(len).map(() => alphabet[Math.floor(rnd() * alphabet.length)]!);
        return { name: `f${i}`, data };
      });
      const chunks: Uint8Array[] = [];
      for (const p of parts) chunks.push(enc.encode(`--${boundary}\r\nContent-Disposition: form-data; name="${p.name}"; filename="x"\r\n\r\n`), p.data, enc.encode("\r\n"));
      chunks.push(enc.encode(`--${boundary}--\r\n`));
      const body = new Uint8Array(chunks.reduce((n, c) => n + c.length, 0));
      let off = 0;
      for (const c of chunks) {
        body.set(c, off);
        off += c.length;
      }
      let pos = 0;
      const stream = new ReadableStream<Uint8Array>({
        pull(c) {
          if (pos >= body.length) return c.close();
          const n = 1 + Math.floor(rnd() * 700);
          c.enqueue(body.slice(pos, pos + n));
          pos += n;
        },
      });
      const r = new MultipartReader(stream, boundary);
      for (const p of parts) {
        const info = await r.next();
        expect(info?.name).toBe(p.name);
        const got: number[] = [];
        await r.body((c) => void got.push(...c));
        expect(new Uint8Array(got)).toEqual(p.data);
      }
      expect(await r.next()).toBeNull();
    }
  });
});

describe("post-login redirects", () => {
  function ctxFor(e: Partial<Env>, host = "backyard.test"): RequestContext {
    const full = { ...(env as unknown as Env), ...e } as Env;
    const cfg = getConfig(full);
    return { cfg, host, publicURL: cfg.publicURL || `https://${host}` } as unknown as RequestContext;
  }

  it("never redirects off the platform", () => {
    const ctx = ctxFor({});
    const hub = "https://backyard.test/";
    for (const bad of [
      "//evil.com",
      "/\t/evil.com",
      "/\n/evil.com",
      "/\\evil.com",
      "https://evil.com/",
      "https://backyard.test.evil.com/",
      "https://user@backyard.test/",
      "http://backyard.test/",
      "https://backyard.test:8443/",
      "javascript:alert(1)",
      "/__backyard/auth/login?rd=x",
      "/s/foo/__backyard/auth/logout",
      "https://foo.backyard.test/",
    ]) {
      expect(safeReturnTo(ctx, bad), bad).toBe(hub);
    }
    expect(safeReturnTo(ctx, "/s/foo/page?x=1#h")).toBe("https://backyard.test/s/foo/page?x=1#h");
    expect(safeReturnTo(ctx, "https://backyard.test/s/foo/")).toBe("https://backyard.test/s/foo/");
    expect(safeReturnTo(ctx, "/__backyard/auth/cli?state=1")).toBe("https://backyard.test/__backyard/auth/cli?state=1");

    const host = ctxFor({ URL_MODE: "host", BASE_DOMAIN: "backyard.test" });
    expect(safeReturnTo(host, "https://foo.backyard.test/x")).toBe("https://foo.backyard.test/x");
    expect(safeReturnTo(host, "https://a.b.backyard.test/")).toBe(hub);
  });
});
