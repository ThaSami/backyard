/**
 * Site previews (SPEC §3, §7.2): the rules of preview.ts (the cases of
 * internal/sites/preview_test.go and title_test.go, so both editions decide
 * alike), SiteMeta.preview on every write path of the gateway (deploy commit,
 * also with index.html in R2, and refresh), and the hub route
 * GET /__backyard/api/sites/{site}/preview with its headers, caching,
 * conditional requests, HEAD, R2-backed files, 404s and gate.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { resetAccessCaches } from "../src/auth";
import { b64url } from "../src/crypto";
import type { Env } from "../src/env";
import { unwrap } from "../src/http";
import { choosePreview, MAX_PREVIEW_BYTES, PREVIEW_CSP, PREVIEW_NAMES, PREVIEW_TYPES, previewType, resolvePreviewRef } from "../src/preview";
import type { SiteRecord } from "../src/registry-do";
import { registry, siteStub } from "../src/stubs";
import { decodeBytes, extractPage } from "../src/title";
import { apiHeaders, call, deploy, DEPLOY_TOKEN, testEnv, tokenHeaders, uniqueSite, wrapSiteNamespace } from "./helpers";

const CSP = "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'";
const IMMUTABLE = "private, max-age=31536000, immutable";
const REVALIDATE = "private, no-cache";
const MAX = 1 << 20;
const cp = (n: number) => String.fromCodePoint(n);

/** A 1×1 PNG. */
const PNG = Uint8Array.from(
  "89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8ffff3f0005fe02fea7d2a5b20000000049454e44ae426082".match(/../g)!,
  (h) => parseInt(h, 16),
);
const svg = (label: string) => `<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720"><rect width="1280" height="720" fill="#2563eb"/><!-- ${label} --></svg>\n`;
const page = (head: string, body = "") => `<!doctype html><html><head><title>Preview test</title><meta name="description" content="Rules">${head}</head><body>${body}</body></html>\n`;
const og = (content: string) => `<meta property="og:image" content="${content}">`;
const previewURL = (site: string, query = "") => `/__backyard/api/sites/${site}/preview${query}`;

async function commitMeta(site: string, files: Record<string, string | Uint8Array>, contentTypes?: Record<string, string>): Promise<Record<string, unknown>> {
  return (await deploy(site, files, contentTypes ? { contentTypes } : {})).commit.site;
}

async function getJSON(path: string, env: Partial<Env> = {}): Promise<Record<string, unknown>> {
  const res = await call(path, { headers: tokenHeaders() }, env);
  expect(res.status, path).toBe(200);
  return (await res.json()) as Record<string, unknown>;
}

async function errorOf(res: Response): Promise<{ code: string; message: string }> {
  return ((await res.json()) as { error: { code: string; message: string } }).error;
}

describe("preview rules (as internal/sites/preview.go)", () => {
  it("has the values of the Go server and SPEC §7.2", () => {
    expect(PREVIEW_NAMES).toEqual(["preview.png", "preview.jpg", "preview.jpeg", "preview.webp", "preview.avif", "preview.gif", "preview.svg"]);
    expect(MAX_PREVIEW_BYTES).toBe(1048576);
    expect(PREVIEW_CSP).toBe(CSP);
    expect(PREVIEW_TYPES).toEqual({ png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp", avif: "image/avif", svg: "image/svg+xml" });
    for (const n of PREVIEW_NAMES) expect(previewType(n), n).not.toBe("");
  });

  it.each([
    ["preview.png", "image/png"],
    ["img/Cover.JPG", "image/jpeg"],
    ["a.jpeg", "image/jpeg"],
    ["a.Svg", "image/svg+xml"],
    ["a.webp", "image/webp"],
    ["a.avif", "image/avif"],
    ["a.gif", "image/gif"],
    // Go's strings.ToLower maps U+0130 to "i".
    [`a.G${cp(0x130)}F`, "image/gif"],
    [`a.AV${cp(0x130)}F`, "image/avif"],
    ["a.svgz", ""],
    ["a.html", ""],
    ["a.ico", ""],
    ["a.bmp", ""],
    ["png", ""],
    ["a.png/b", ""],
    ["a.png.html", ""],
    ["dir.png/cover", ""],
    ["a.", ""],
    ["a.constructor", ""],
    ["a.__proto__", ""],
    ["a.toString", ""],
  ])("previewType(%j) = %j", (p, want) => expect(previewType(p)).toBe(want));

  it.each([
    ["preview.png", "preview.png"],
    ["img/cover.png", "img/cover.png"],
    ["/img/cover.png", "img/cover.png"],
    ["./img/cover.png", "img/cover.png"],
    ["././a.png", "a.png"],
    ["/./a.png", "a.png"],
    ["/././a.png", "a.png"],
    ["a.png?v=2", "a.png"],
    ["a.png#frag", "a.png"],
    ["a.png#x?y", "a.png"],
    ["a.png?x#y", "a.png"],
    ["img/my%20cover.png", "img/my cover.png"],
    ["%69mg/a.png", "img/a.png"],
    ["img%2Fa.png", "img/a.png"],
    ["caf%C3%A9.png", "caf\xe9.png"],
    ["caf\xe9.png", "caf\xe9.png"],
    ["a b.png", "a b.png"],
    ["a+b.png", "a+b.png"],
    ["img/a:b.png", "img/a:b.png"],
    ["1a:b.png", "1a:b.png"],
    ["a_b:c.png", "a_b:c.png"],
    ["-a:b.png", "-a:b.png"],
    ["a.png%3Fv=1", "a.png?v=1"],
    ["x..png", "x..png"],
    ["a/b./c.png", "a/b./c.png"],
    ["__backyard/x.png", "__backyard/x.png"],
    ["a".repeat(1020) + ".png", "a".repeat(1020) + ".png"],
    ["Preview.PNG", "Preview.PNG"],
    ["preview", "preview"],
    // The bound is on bytes: 512 two-byte characters are 1024 bytes.
    ["\xe9".repeat(512), "\xe9".repeat(512)],
    // U+FEFF is part of the name (no BOM is stripped).
    [cp(0xfeff) + "a.png", cp(0xfeff) + "a.png"],
  ])("resolvePreviewRef(%j) = %j", (ref, want) => expect(resolvePreviewRef(ref)).toBe(want));

  it.each([
    ["empty", ""],
    ["longer than 1024 bytes", "a".repeat(1021) + ".png"],
    ["longer than 1024 bytes, in fewer characters", "\xe9".repeat(513)],
    ["scheme https", "https://example.com/a.png"],
    ["scheme http upper", "HTTP://example.com/a.png"],
    ["scheme data", "data:image/png;base64,AAAA"],
    ["scheme javascript", "javascript:alert(1)"],
    ["scheme with digits and +.-", "a1+.-b:x.png"],
    ["scheme drive letter", "c:/a.png"],
    ["scheme single letter", "x:a.png"],
    ["scheme without slashes", "blob:a.png"],
    ["scheme-like file name", "a:b.png"],
    ["scheme-like file name with dot", "cover.png:x"],
    ["protocol-relative", "//cdn.example.com/a.png"],
    ["protocol-relative after nothing else", "///a.png"],
    ["backslash", "img\\a.png"],
    ["backslash leading", "\\a.png"],
    ["backslash after query", "a.png?x\\y"],
    ["query only", "?v=1"],
    ["fragment only", "#x"],
    ["root only", "/"],
    ["dot slash only", "./"],
    ["dot slash then query", "./?x"],
    ["invalid escape", "a%2.png"],
    ["invalid escape at end", "a.png%"],
    ["invalid escape non-hex", "a%zz.png"],
    ["escaped NUL", "a%00.png"],
    ["escaped newline", "a%0A.png"],
    ["escaped DEL", "a%7F.png"],
    ["raw tab", "a\t.png"],
    ["raw control", "a\x01.png"],
    ["raw DEL", "a\x7f.png"],
    ["escaped invalid UTF-8", "a%FF.png"],
    ["empty segment", "img//a.png"],
    ["empty segment after decoding", "img%2F%2Fa.png"],
    ["trailing slash", "img/"],
    ["leading slash after one removed", "/.//a.png"],
    ["dot segment", "img/./a.png"],
    ["dotdot segment", "../a.png"],
    ["dotdot inside", "img/../a.png"],
    ["dotdot after dot slash", "./../a.png"],
    ["escaped dotdot", "%2E%2E/a.png"],
    ["escaped slash dotdot", "img%2F..%2Fa.png"],
    ["dotfile", ".hidden.png"],
    ["dot directory", ".well-known/a.png"],
    ["dot directory nested", "img/.cache/a.png"],
    ["escaped leading dot", "%2Ehidden.png"],
    ["lone dot", "."],
    ["lone dotdot", ".."],
    ["two leading slashes after root removal", "/%2Fa.png"],
  ])("resolvePreviewRef refuses %s", (_why, ref) => expect(resolvePreviewRef(ref)).toBeNull());

  it("checks the bytes Go sees: extractPage's escapes of invalid UTF-8 count as those bytes", () => {
    const bytes = (...parts: (string | number)[]) => decodeBytes(new Uint8Array(parts.flatMap((p) => (typeof p === "number" ? [p] : [...new TextEncoder().encode(p)]))));
    expect(resolvePreviewRef(bytes("a", 0xff, ".png"))).toBeNull();
    expect(resolvePreviewRef(bytes("a.png?", 0xff))).toBe("a.png");
    // A raw lead byte and an escaped continuation byte make one valid character, as in Go.
    expect(resolvePreviewRef(bytes(0xc3, "%A9.png"))).toBe("\xe9.png");
    expect(extractPage(new Uint8Array([...new TextEncoder().encode("<meta property=og:image content='"), 0xc3, ...new TextEncoder().encode("%A9.png'>")])).image).toBe(
      cp(0xdcc3) + "%A9.png",
    );
  });

  it.each([
    ["nothing", "", { "index.html": 10 }, ""],
    ["no files", "a.png", {}, ""],
    ["og relative", "img/cover.png", { "index.html": 10, "img/cover.png": 5 }, "img/cover.png"],
    ["og root relative", "/img/cover.png", { "img/cover.png": 5 }, "img/cover.png"],
    ["og dot slash", "./cover.webp", { "cover.webp": 5 }, "cover.webp"],
    ["og percent-decoded", "my%20cover.svg", { "my cover.svg": 5 }, "my cover.svg"],
    ["og with query", "cover.png?v=3", { "cover.png": 5 }, "cover.png"],
    ["og beats preview.png", "cover.jpg", { "cover.jpg": 5, "preview.png": 5 }, "cover.jpg"],
    ["og case of extension", "Cover.JPEG", { "Cover.JPEG": 5, "preview.png": 5 }, "Cover.JPEG"],
    ["og path is case-sensitive", "cover.png", { "Cover.png": 5, "preview.svg": 5 }, "preview.svg"],
    ["og at the size limit", "cover.png", { "cover.png": MAX }, "cover.png"],
    ["og too large", "cover.png", { "cover.png": MAX + 1, "preview.png": 5 }, "preview.png"],
    ["og missing file", "cover.png", { "preview.gif": 5 }, "preview.gif"],
    ["og unsupported extension", "cover.bmp", { "cover.bmp": 5, "preview.png": 5 }, "preview.png"],
    ["og no extension", "cover", { cover: 5 }, ""],
    ["og absolute URL", "https://example.com/preview.png", { "preview.png": 5 }, "preview.png"],
    ["og absolute URL to own path", "https://site.example.com/cover.png", { "cover.png": 5 }, ""],
    ["og protocol-relative", "//example.com/cover.png", { "cover.png": 5 }, ""],
    ["og dotdot", "../cover.png", { "cover.png": 5 }, ""],
    ["og dot segment", "img/./cover.png", { "img/cover.png": 5 }, ""],
    ["og dotfile", ".cover.png", { ".cover.png": 5, "preview.png": 1 }, "preview.png"],
    ["og directory", "img", { "img/a.png": 5 }, ""],
    ["fallback order png", "", { "preview.svg": 1, "preview.gif": 1, "preview.avif": 1, "preview.webp": 1, "preview.jpeg": 1, "preview.jpg": 1, "preview.png": 1 }, "preview.png"],
    ["fallback order jpg", "", { "preview.svg": 1, "preview.gif": 1, "preview.avif": 1, "preview.webp": 1, "preview.jpeg": 1, "preview.jpg": 1 }, "preview.jpg"],
    ["fallback order jpeg", "", { "preview.svg": 1, "preview.gif": 1, "preview.avif": 1, "preview.webp": 1, "preview.jpeg": 1 }, "preview.jpeg"],
    ["fallback order webp", "", { "preview.svg": 1, "preview.gif": 1, "preview.avif": 1, "preview.webp": 1 }, "preview.webp"],
    ["fallback order avif", "", { "preview.svg": 1, "preview.gif": 1, "preview.avif": 1 }, "preview.avif"],
    ["fallback order gif", "", { "preview.svg": 1, "preview.gif": 1 }, "preview.gif"],
    ["fallback svg", "", { "preview.svg": 1 }, "preview.svg"],
    ["fallback skips too large", "", { "preview.png": MAX + 1, "preview.svg": MAX }, "preview.svg"],
    ["fallback all too large", "", { "preview.png": MAX + 1, "preview.svg": MAX + 1 }, ""],
    ["fallback empty file", "", { "preview.png": 0 }, "preview.png"],
    ["fallback exact lowercase", "", { "Preview.png": 1, "PREVIEW.SVG": 1 }, ""],
    ["fallback only at the root", "", { "img/preview.png": 1 }, ""],
    ["fallback other names ignored", "", { "preview.ico": 1, "preview.bmp": 1, "thumbnail.png": 1 }, ""],
    ["no prototype names", "constructor.png", { "preview.svg": 1 }, "preview.svg"],
  ] as [string, string, Record<string, number>, string][])("choosePreview: %s", (_what, ogImage, files, want) => {
    const m = new Map(Object.entries(files));
    expect(choosePreview(ogImage, (p) => m.get(p))).toBe(want);
  });
});

describe("the og:image of index.html (as internal/sites/title.go)", () => {
  const head = '<title>T</title><meta name="description" content="D">';
  it.each([
    ["property", '<meta property="og:image" content="preview.png">', "preview.png"],
    ["name", '<meta name="og:image" content="img/cover.jpg">', "img/cover.jpg"],
    ["after title and description", head + '<meta property="og:image" content="a.png">', "a.png"],
    ["before title", '<meta property="og:image" content="a.png">' + head, "a.png"],
    ["far after title and description", head + "<p>filler</p>".repeat(2000) + '<meta property="og:image" content="late.png">', "late.png"],
    ["case-insensitive", '<META PROPERTY="OG:Image" CONTENT="Up.PNG">', "Up.PNG"],
    ["key trimmed", "<meta property=' og:image\n' content='a.png'>", "a.png"],
    ["content trimmed and decoded", "<meta property='og:image' content=' \timg/a&amp;b.png\n\f '>", "img/a&b.png"],
    ["only ASCII whitespace trimmed", "<meta property='og:image' content='&nbsp;a.png'>", "\xa0a.png"],
    ["numeric entity", '<meta property="og:image" content="&#x2F;a.png">', "/a.png"],
    ["unquoted", "<meta property=og:image content=/img/a.png>", "/img/a.png"],
    ["absolute URL kept raw", '<meta property="og:image" content="https://cdn.example.com/a.png">', "https://cdn.example.com/a.png"],
    ["first wins", '<meta property="og:image" content="one.png"><meta property="og:image" content="two.png">', "one.png"],
    ["first wins even if empty", '<meta property="og:image" content=""><meta property="og:image" content="two.png">', ""],
    ["property beats name", '<meta property="og:title" name="og:image" content="x.png"><meta name="og:image" content="y.png">', "y.png"],
    ["empty property beats name", '<meta property="" name="og:image" content="x.png">', ""],
    ["same tag as description", '<meta name="description" property="og:image" content="d.png">', "d.png"],
    ["other og tags ignored", '<meta property="og:image:width" content="1200"><meta property="og:image:url" content="u.png"><meta property="twitter:image" content="t.png">', ""],
    ["image_src is not og:image", '<link rel="image_src" href="a.png"><meta itemprop="image" content="b.png">', ""],
    ["in comment", '<!-- <meta property="og:image" content="hidden.png"> --><meta property="og:image" content="shown.png">', "shown.png"],
    ["in script", `<script>var m = '<meta property="og:image" content="s.png">'</script><meta property="og:image" content="real.png">`, "real.png"],
    ["in noscript", '<noscript><meta property="og:image" content="n.png"></noscript>', ""],
    ["in template", '<template><meta property="og:image" content="t.png"></template><meta name="og:image" content="ok.png">', "ok.png"],
    ["in svg", '<svg><meta property="og:image" content="s.png"></svg>', ""],
    ["in textarea", '<textarea><meta property="og:image" content="x.png"></textarea>', ""],
    ["none", head, ""],
  ])("%s", (_what, html, want) => expect(extractPage(html).image).toBe(want));

  it("is found with the title and description wherever it is, within the first 64 KiB", () => {
    expect(extractPage('<meta property="og:image" content="a.png">' + head)).toEqual({ title: "T", description: "D", image: "a.png" });
    const tag = '<meta property="og:image" content="a.png">';
    expect(extractPage(" ".repeat(64 * 1024 - tag.length) + tag).image).toBe("a.png");
    expect(extractPage(" ".repeat(64 * 1024 - tag.length + 1) + tag).image).toBe("");
  });
});

describe("SiteMeta.preview on the gateway's write paths (SPEC §7.2)", () => {
  it("is set by a deploy commit, in field order, everywhere SiteMeta appears", async () => {
    const site = uniqueSite("prev");
    const m = await commitMeta(site, { "index.html": page(og("./img/cover.png?v=2")), "img/cover.png": PNG, "preview.svg": svg("fallback") });
    expect(m.preview).toBe("img/cover.png");
    expect(Object.keys(m).slice(0, 6)).toEqual(["name", "url", "title", "description", "preview", "created_at"]);
    const hub = (await getJSON("/__backyard/api/sites")).sites as Record<string, unknown>[];
    const gw = (await getJSON("/__backyard/api/deploy/sites")).sites as Record<string, unknown>[];
    for (const [what, meta] of [
      ["hub list", hub.find((s) => s.name === site)],
      ["gateway list", gw.find((s) => s.name === site)],
      ["gateway get", await getJSON(`/__backyard/api/deploy/sites/${site}`)],
      ["site API", await getJSON(`/s/${site}/__backyard/api/site`)],
    ] as const) {
      expect(meta?.preview, what).toBe("img/cover.png");
      expect(Object.keys(meta ?? {}).indexOf("preview"), what).toBe(Object.keys(meta ?? {}).indexOf("description") + 1);
    }
  });

  it("falls back to preview.*, follows each redeploy and is omitted without one", async () => {
    const site = uniqueSite("prevfall");
    let m = await commitMeta(site, { "index.html": page(og("https://example.com/x.png")), "preview.gif": PNG, "preview.svg": svg("s") });
    expect(m.preview).toBe("preview.gif");
    m = await commitMeta(site, { "index.html": page(og("img/missing.png")), "preview.svg": svg("s") });
    expect(m.preview).toBe("preview.svg");
    // Without index.html only preview.* counts.
    m = await commitMeta(site, { "other.html": page(og("x.png")), "x.png": PNG, "preview.webp": PNG });
    expect(m.preview).toBe("preview.webp");
    m = await commitMeta(site, { "index.html": page(og("img/cover.png")) });
    expect(m).not.toHaveProperty("preview");
    expect(await getJSON(`/__backyard/api/deploy/sites/${site}`)).not.toHaveProperty("preview");
    // A file over 1 MiB is no preview.
    m = await commitMeta(site, { "index.html": page(og("big.png")), "big.png": new Uint8Array(MAX + 1), "preview.png": new Uint8Array(MAX) });
    expect(m.preview).toBe("preview.png");
  });

  it("reads the og:image from R2 when index.html is stored there", async () => {
    const site = uniqueSite("prevr2");
    // FILE_INLINE_MAX is 64 KiB in the suite: this index.html goes to R2.
    const big = page(og("/img/cover.png"), "<p>" + "x".repeat(80_000) + "</p>");
    let m = await commitMeta(site, { "index.html": big, "img/cover.png": PNG, "preview.svg": svg("r2") });
    expect(m).toMatchObject({ title: "Preview test", description: "Rules", preview: "img/cover.png" });
    // The SiteDO's own answer: the R2 key, and what preview.* gives.
    const info = unwrap(await siteStub(testEnv(), site).siteInfo());
    expect(info.titleR2Key).not.toBe("");
    expect(info).toMatchObject({ title: "", preview: "preview.svg" });
    // An unusable og:image there leaves the SiteDO's choice of preview.*.
    m = await commitMeta(site, { "index.html": page(og("img/nope.png"), "<p>" + "y".repeat(80_000) + "</p>"), "img/cover.png": PNG, "preview.svg": svg("r2") });
    expect(m.preview).toBe("preview.svg");
    m = await commitMeta(site, { "index.html": page("", "<p>" + "z".repeat(80_000) + "</p>"), "img/cover.png": PNG });
    expect(m).not.toHaveProperty("preview");
    expect(m.title).toBe("Preview test");
  });

  it("is derived again by a refresh", async () => {
    const site = uniqueSite("prevref");
    await commitMeta(site, { "index.html": page(og("img/a.svg")), "img/a.svg": svg("a") });
    // A record written before previews existed has none: a refresh adds it.
    const rec = (await registry(testEnv()).getSite(site)) as { ok: true; value: SiteRecord };
    const old = { ...rec.value };
    delete old.preview;
    await registry(testEnv()).putSite(old);
    expect(await getJSON(`/__backyard/api/deploy/sites/${site}`)).not.toHaveProperty("preview");
    const res = await call(`/__backyard/api/deploy/sites/${site}/refresh`, { method: "POST", headers: tokenHeaders(), body: "{}" });
    expect(res.status).toBe(200);
    const m = (await res.json()) as Record<string, unknown>;
    expect(m.preview).toBe("img/a.svg");
    expect(Object.keys(m).indexOf("preview")).toBe(Object.keys(m).indexOf("description") + 1);
    expect((await getJSON(`/__backyard/api/deploy/sites/${site}`)).preview).toBe("img/a.svg");
  });

  it("keeps the title and description read from R2 when choosing the preview fails at a commit", async () => {
    const site = uniqueSite("prevforerr");
    const SITE = wrapSiteNamespace({
      previewFor: async () => {
        throw new Error("simulated SiteDO error");
      },
    });
    const big = page(og("img/c.svg"), "<p>" + "x".repeat(80_000) + "</p>");
    const m = (await deploy(site, { "index.html": big, "img/c.svg": svg("c"), "preview.svg": svg("p") }, { envOverrides: { SITE } })).commit.site;
    // Best effort: the files are switched; the preview is the SiteDO's choice of preview.*.
    expect(m).toMatchObject({ title: "Preview test", description: "Rules", preview: "preview.svg" });
    expect(await getJSON(`/__backyard/api/deploy/sites/${site}`)).toMatchObject({ title: "Preview test", description: "Rules", preview: "preview.svg" });
  });

  it("fails a refresh that cannot read index.html from R2 or choose its preview, and keeps the record (as on the Go server)", async () => {
    const site = uniqueSite("prevreferr");
    const big = page(og("img/c.svg"), "<p>" + "x".repeat(80_000) + "</p>");
    await commitMeta(site, { "index.html": big, "img/c.svg": svg("c"), "preview.svg": svg("p") });
    expect(unwrap(await siteStub(testEnv(), site).siteInfo()).titleR2Key).not.toBe("");
    const before = await getJSON(`/__backyard/api/deploy/sites/${site}`);
    expect(before).toMatchObject({ title: "Preview test", description: "Rules", preview: "img/c.svg" });
    const refresh = (env: Partial<Env>) => call(`/__backyard/api/deploy/sites/${site}/refresh`, { method: "POST", headers: tokenHeaders(), body: "{}" }, env);
    const bucket = testEnv().R2!;
    const R2 = new Proxy(bucket, {
      get(t, p) {
        if (p === "get") {
          return async () => {
            throw new Error("simulated R2 error");
          };
        }
        const v = Reflect.get(t, p);
        return typeof v === "function" ? v.bind(t) : v;
      },
    });
    const SITE = wrapSiteNamespace({
      previewFor: async () => ({ ok: false, error: { status: 500, code: "internal", message: "choosing the preview failed" } }),
    });
    for (const [what, env] of [
      ["R2 read", { R2 }],
      ["previewFor", { SITE }],
    ] as const) {
      const res = await refresh(env);
      expect(res.status, what).toBe(500);
      expect((await errorOf(res)).code, what).toBe("internal");
      expect(await getJSON(`/__backyard/api/deploy/sites/${site}`), what).toEqual(before);
    }
    const res = await refresh({});
    expect(res.status).toBe(200);
    expect(await res.json()).toMatchObject({ title: "Preview test", description: "Rules", preview: "img/c.svg" });
  });
});

describe("GET /__backyard/api/sites/{site}/preview (SPEC §3)", () => {
  /** Asserts the headers of an image answer. */
  function assertImage(res: Response, type: string, cache: string, what: string) {
    expect(res.headers.get("Content-Type"), what).toBe(type);
    expect(res.headers.get("Content-Security-Policy"), what).toBe(CSP);
    expect(res.headers.get("X-Content-Type-Options"), what).toBe("nosniff");
    expect(res.headers.get("Content-Disposition"), what).toBeNull();
    expect(res.headers.get("Cache-Control"), what).toBe(cache);
    expect(res.headers.get("ETag") ?? "", what).toMatch(/^"[0-9a-f]+"$/);
  }

  it("serves the file by its extension, sandboxed, with both caching rules, 304, HEAD and ranges", async () => {
    const site = uniqueSite("prevapi");
    const cover = svg("cover");
    // The declared type is never served.
    const m = await commitMeta(site, { "index.html": page(og("img/Shot.SVG")), "img/Shot.SVG": cover }, { "img/Shot.SVG": "text/html; charset=utf-8" });
    expect(m.preview).toBe("img/Shot.SVG");
    let res = await call(previewURL(site));
    expect(res.status).toBe(200);
    assertImage(res, "image/svg+xml", REVALIDATE, "no v");
    expect(await res.text()).toBe(cover);
    const etag = res.headers.get("ETag")!;
    for (const q of ["?v=", "?other=1", "?v", "?v=1;x", "?v=%zz", "?v=&v=2", "?V=1"]) {
      res = await call(previewURL(site, q));
      expect(res.status, q).toBe(200);
      assertImage(res, "image/svg+xml", REVALIDATE, q);
      await res.arrayBuffer();
    }
    for (const q of [`?v=${encodeURIComponent(String(m.updated_at))}`, "?x=1&v=2", "?%76=1", "?v=%FF", "?v=a+b&v="]) {
      res = await call(previewURL(site, q));
      expect(res.status, q).toBe(200);
      assertImage(res, "image/svg+xml", IMMUTABLE, q);
      expect(res.headers.get("ETag"), "the ETag does not depend on v").toBe(etag);
      await res.arrayBuffer();
    }
    res = await call(previewURL(site, "?v=1"), { headers: { "If-None-Match": etag } });
    expect(res.status).toBe(304);
    expect(res.headers.get("ETag")).toBe(etag);
    expect(res.headers.get("Cache-Control")).toBe(IMMUTABLE);
    expect(res.headers.get("Content-Security-Policy")).toBe(CSP);
    expect(await res.text()).toBe("");
    res = await call(previewURL(site), { headers: { "If-None-Match": '"other"' } });
    expect(res.status).toBe(200);
    expect(await res.text()).toBe(cover);
    res = await call(previewURL(site, "?v=1"), { method: "HEAD" });
    expect(res.status).toBe(200);
    assertImage(res, "image/svg+xml", IMMUTABLE, "HEAD");
    expect(res.headers.get("Content-Length")).toBe(String(cover.length));
    expect(await res.text()).toBe("");
    res = await call(previewURL(site), { headers: { Range: "bytes=0-3" } });
    expect(res.status).toBe(206);
    expect(res.headers.get("Content-Range")).toBe(`bytes 0-3/${cover.length}`);
    expect(await res.text()).toBe("<svg");

    // A redeploy serves the new file under a new ETag.
    const m2 = await commitMeta(site, { "index.html": page(""), "preview.png": PNG });
    expect(m2.preview).toBe("preview.png");
    res = await call(previewURL(site, `?v=${encodeURIComponent(String(m2.updated_at))}`), { headers: { "If-None-Match": etag } });
    expect(res.status).toBe(200);
    assertImage(res, "image/png", IMMUTABLE, "PNG");
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(PNG);
    expect(res.headers.get("ETag")).not.toBe(etag);
    // Without a preview: 404, also for HEAD.
    await commitMeta(site, { "index.html": page(og("img/Shot.SVG")) });
    res = await call(previewURL(site));
    expect(res.status).toBe(404);
    expect(await errorOf(res)).toEqual({ code: "not_found", message: `site "${site}" has no preview` });
    expect((await call(previewURL(site), { method: "HEAD" })).status).toBe(404);
  });

  it("streams a preview stored in R2", async () => {
    const site = uniqueSite("prevr2api");
    const img = new Uint8Array(200_000).map((_, i) => i % 251);
    const m = await commitMeta(site, { "index.html": page(""), "preview.webp": img });
    expect(m.preview).toBe("preview.webp");
    let res = await call(previewURL(site, "?v=1"));
    expect(res.status).toBe(200);
    assertImage(res, "image/webp", IMMUTABLE, "R2");
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(img);
    const etag = res.headers.get("ETag")!;
    res = await call(previewURL(site), { headers: { "If-None-Match": etag } });
    expect(res.status).toBe(304);
    res = await call(previewURL(site), { method: "HEAD" });
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Length")).toBe(String(img.length));
    expect(await res.text()).toBe("");
    res = await call(previewURL(site), { headers: { Range: "bytes=100000-100009" } });
    expect(res.status).toBe(206);
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(img.subarray(100_000, 100_010));
  });

  it("answers 404 not_found for unknown sites, invalid names, missing files and non-image records, 405 for other methods", async () => {
    const site = uniqueSite("prev404");
    await commitMeta(site, { "index.html": page(""), "preview.svg": svg("x"), "page.html": "<script>alert(1)</script>" });
    for (const path of [previewURL(uniqueSite("ghost")), previewURL("Bad_Name"), previewURL("constructor"), previewURL(site) + "/x", `/__backyard/api/sites/${site}/preview/`]) {
      const res = await call(path);
      expect(res.status, path).toBe(404);
      expect((await errorOf(res)).code, path).toBe("not_found");
    }
    // On a site the path is the site API's (hub host only).
    expect((await call(`/s/${site}/__backyard/api/sites/${site}/preview`)).status).toBe(404);
    // A record naming a file that is gone, or a file of another type (defence in depth: never served).
    const rec = ((await registry(testEnv()).getSite(site)) as { ok: true; value: SiteRecord }).value;
    for (const preview of ["gone.png", "page.html", "index.html"]) {
      await registry(testEnv()).putSite({ ...rec, preview });
      const res = await call(previewURL(site));
      expect(res.status, preview).toBe(404);
      expect(await errorOf(res), preview).toEqual({ code: "not_found", message: `site "${site}" has no preview` });
    }
    await registry(testEnv()).putSite(rec);
    expect((await call(previewURL(site))).status).toBe(200);
    for (const method of ["POST", "PUT", "DELETE", "PATCH"]) {
      const res = await call(previewURL(site), { method, headers: apiHeaders(), body: "{}" });
      expect(res.status, method).toBe(405);
      expect(res.headers.get("Allow"), method).toBe("GET, HEAD");
      expect((await errorOf(res)).code).toBe("bad_request");
    }
  });

  it("answers 404 for a file over 1 MiB, whatever the record names", async () => {
    const site = uniqueSite("prevbig");
    const m = await commitMeta(site, { "index.html": page(og("big.png")), "big.png": new Uint8Array(MAX + 1), "edge.png": new Uint8Array(MAX) });
    expect(m).not.toHaveProperty("preview");
    const rec = ((await registry(testEnv()).getSite(site)) as { ok: true; value: SiteRecord }).value;
    await registry(testEnv()).putSite({ ...rec, preview: "big.png" });
    for (const q of ["", "?v=1"]) {
      const res = await call(previewURL(site, q));
      expect(res.status, q).toBe(404);
      expect(await errorOf(res), q).toEqual({ code: "not_found", message: `site "${site}" has no preview` });
      expect((await call(previewURL(site, q), { method: "HEAD" })).status, q).toBe(404);
    }
    // At most 1 MiB is served.
    await registry(testEnv()).putSite({ ...rec, preview: "edge.png" });
    const res = await call(previewURL(site, "?v=1"));
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("image/png");
    expect((await res.arrayBuffer()).byteLength).toBe(MAX);
  });

  it("is served on the hub host in host mode, and is not a site path there", async () => {
    const env = { URL_MODE: "host", BASE_DOMAIN: "backyard.test" };
    const site = uniqueSite("prevhost");
    await deploy(site, { "index.html": page(""), "preview.svg": svg("host") }, { envOverrides: env });
    const res = await call(`https://backyard.test${previewURL(site)}`, {}, env);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe(svg("host"));
    expect((await call(`https://${site}.backyard.test${previewURL(site)}`, {}, env)).status).toBe(404);
  });

  it("passes the gate of the site list: guests in mode none, bearer tokens", async () => {
    const site = uniqueSite("prevgate");
    await commitMeta(site, { "index.html": page(""), "preview.svg": svg("gate") });
    // Mode none: a browser without a cookie is a guest, like for /__backyard/api/sites.
    expect((await call("/__backyard/api/sites")).status).toBe(200);
    let res = await call(previewURL(site));
    expect(res.status).toBe(200);
    expect(res.headers.get("Set-Cookie") ?? "").toMatch(/^backyard_anon=/);
    res = await call(previewURL(site), { headers: { Authorization: `Bearer ${DEPLOY_TOKEN}` } });
    expect(res.status).toBe(200);
    const bad = await call(previewURL(site), { headers: { Authorization: "Bearer not-a-valid-token" } });
    const list = await call("/__backyard/api/sites", { headers: { Authorization: "Bearer not-a-valid-token" } });
    expect(bad.status).toBe(list.status);
    expect((await errorOf(bad)).code).toBe((await errorOf(list)).code);
  });
});

describe("the preview route with Cloudflare Access (AUTH_MODE=access)", () => {
  const TEAM = "acme.cloudflareaccess.com";
  const AUD = "aud-preview";
  const accessEnv = { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: AUD };

  afterEach(() => {
    vi.unstubAllGlobals();
    resetAccessCaches();
  });

  /** Stubs the team's keys and identity endpoint; returns a JWT signer. */
  async function access() {
    const pair = (await crypto.subtle.generateKey(
      { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
      true,
      ["sign", "verify"],
    )) as CryptoKeyPair;
    const jwk = (await crypto.subtle.exportKey("jwk", pair.publicKey)) as JsonWebKey;
    const realFetch = globalThis.fetch;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url === `https://${TEAM}/cdn-cgi/access/certs`) return Response.json({ keys: [{ ...jwk, kid: "k1", alg: "RS256", use: "sig" }] });
      if (url === `https://${TEAM}/cdn-cgi/access/get-identity`) return Response.json({ name: "Alice", email: "alice@acme.com", groups: [] });
      return realFetch(input, init);
    });
    const now = Math.floor(Date.now() / 1000);
    return async (email: string) => {
      const claims = { aud: [AUD], email, sub: email, iss: `https://${TEAM}`, iat: now, exp: now + 3600, identity_nonce: email };
      const h = b64url(new TextEncoder().encode(JSON.stringify({ alg: "RS256", kid: "k1", typ: "JWT" })));
      const p = b64url(new TextEncoder().encode(JSON.stringify(claims)));
      const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", pair.privateKey, new TextEncoder().encode(`${h}.${p}`)));
      return `${h}.${p}.${b64url(sig)}`;
    };
  }

  it("loads with the CF_Authorization cookie or the Access header, and refuses what the site list refuses", async () => {
    const sign = await access();
    const site = uniqueSite("prevaccess");
    await commitMeta(site, { "index.html": page(""), "preview.svg": svg("access") });
    const alice = await sign("alice@acme.com");
    for (const headers of [{ Cookie: `CF_Authorization=${alice}` }, { "Cf-Access-Jwt-Assertion": alice }] as Record<string, string>[]) {
      const res = await call(previewURL(site, "?v=1"), { headers }, accessEnv);
      expect(res.status, Object.keys(headers)[0]).toBe(200);
      expect(res.headers.get("Content-Security-Policy")).toBe(CSP);
      expect(await res.text()).toBe(svg("access"));
    }
    const mallory = await sign("mallory@acme.com");
    for (const headers of [{}, { Cookie: "CF_Authorization=garbage" }, { Cookie: `CF_Authorization=${mallory}; CF_Authorization=${alice}` }] as Record<string, string>[]) {
      const list = await call("/__backyard/api/sites", { headers }, accessEnv);
      const res = await call(previewURL(site), { headers }, accessEnv);
      expect(list.status).toBe(401);
      expect(res.status, JSON.stringify(headers)).toBe(401);
      expect(await errorOf(res)).toEqual(await errorOf(list));
    }
  });
});
