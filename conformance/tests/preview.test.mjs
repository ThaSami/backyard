// Site previews (SPEC §7.2, §3): SiteMeta.preview, derived at every write
// from the og:image of the root index.html or a root preview.* file, and the
// hub route GET /__backyard/api/sites/{site}/preview that serves it.
import assert from "node:assert/strict";
import { describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { hubURL, setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";
import { assertSiteMeta } from "../lib/shapes.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";

const MAX = 1 << 20;
const CSP = "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'";
const IMMUTABLE = "private, max-age=31536000, immutable";
const REVALIDATE = "private, no-cache";

const png = Buffer.from("89504e470d0a1a0a0000000d49484452000000010000000108060000001f15c4890000000d4944415478da63f8ffff3f0005fe02fea7d2a5b20000000049454e44ae426082", "hex");
const svg = (label) => Buffer.from(`<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720" viewBox="0 0 1280 720"><rect width="1280" height="720" fill="#2563eb"/><!-- ${label} --></svg>\n`);
/** Bytes standing in for an image (the server never decodes previews). */
const fake = (label) => Buffer.from(`not really an image: ${label}\n`);

/** The root index.html with extra head (and body) markup after the title and the description. */
const page = (head, body = "") =>
  `<!doctype html><html><head><meta charset="utf-8"><title>Preview test</title><meta name="description" content="Preview rules">${head}</head><body>${body}</body></html>\n`;
/** An og:image meta tag. */
const og = (content) => `<meta property="og:image" content="${content}">`;

const previewURL = (site, query = "") => `${hubURL}/__backyard/api/sites/${site}/preview${query}`;

/** Deploys files and returns the commit's SiteMeta (asserted). */
async function deploy(site, files, opts) {
  const { commit } = await deploySite(client, env, site, files, opts);
  return assertSiteMeta(commit.site, { name: site, url: env.canonicalSiteURL(site), what: "commit SiteMeta" });
}

/** The site's SiteMeta from every place that returns one (SPEC §2, §5.1, §7.2); they agree on preview. */
async function metaEverywhere(site, want) {
  const where = {
    "GET G/sites/{site}": expectStatus(await client.get(`${env.gateway}/sites/${site}`), 200).json,
    "GET G/sites": expectStatus(await client.get(`${env.gateway}/sites`), 200).json.sites.find((s) => s.name === site),
    "GET /__backyard/api/sites": expectStatus(await client.get(`${hubURL}/__backyard/api/sites`), 200).json.sites.find((s) => s.name === site),
    "GET /__backyard/api/site": expectStatus(await client.get(env.api(site, "/site")), 200).json,
  };
  for (const [what, m] of Object.entries(where)) {
    assertSiteMeta(m, { name: site, what });
    assert.equal(m.preview, want, `${what}: preview`);
  }
}

describe("SiteMeta.preview (SPEC §7.2)", { skip }, () => {
  const site = uniqueSite("prev");
  const assets = {
    "img/cover.png": png,
    "img/hero shot.webp": fake("webp"),
    "img/a&b.JPG": fake("jpg"),
    "img/.hidden.png": png,
    "img/100%.png": png,
    "img/cover.bmp": fake("bmp"),
    "img/big.png": Buffer.alloc(MAX + 1, 0x61),
    "img/edge.gif": Buffer.alloc(MAX, 0x62),
    "preview.svg": svg("fallback"),
  };
  const slash = "/" + "./".repeat(505) + "img/cover.png"; // 1024 bytes
  assert.equal(Buffer.byteLength(slash), 1024);

  // [what, the index.html, the expected preview]
  const cases = [
    ["a relative og:image wins over preview.*, also after the title and the description", page(og("img/cover.png")), "img/cover.png"],
    ["leading ./ segments, a query and a fragment", page(og("././img/cover.png?v=2#top")), "img/cover.png"],
    ["a site-root relative og:image (/ then ./)", page(og("/./img/cover.png")), "img/cover.png"],
    ["percent escapes are decoded", page(og("img/hero%20shot.webp")), "img/hero shot.webp"],
    [
      "name= without property=, in any case and trimmed; entities decoded; an upper-case extension",
      page(`<meta name=" OG:Image " content=" img/a&amp;b&#46;JPG ">`),
      "img/a&b.JPG",
    ],
    ["upper-case markup; a file of exactly 1 MiB", page(`<META PROPERTY="OG:IMAGE" CONTENT="img/edge.gif">`), "img/edge.gif"],
    ["a reference of exactly 1024 bytes", page(og(slash)), "img/cover.png"],
    // Every og:image below fails a check and falls through to preview.svg.
    ["an absolute URL", page(og("https://example.com/img/cover.png")), "preview.svg"],
    ["a scheme-relative URL", page(og("//example.com/img/cover.png")), "preview.svg"],
    ["a .. segment", page(og("img/../img/cover.png")), "preview.svg"],
    ["a .. segment after percent-decoding", page(og("%2e%2e/img/cover.png")), "preview.svg"],
    ["a segment that starts with a dot", page(og("img/.hidden.png")), "preview.svg"],
    ["a backslash", page(og("img\\cover.png")), "preview.svg"],
    ["an invalid percent escape", page(og("img/100%.png")), "preview.svg"],
    ["an extension that is no preview type", page(og("img/cover.bmp")), "preview.svg"],
    ["a file the site does not have", page(og("img/missing.png")), "preview.svg"],
    ["a file larger than 1 MiB", page(og("img/big.png")), "preview.svg"],
    ["a reference longer than 1024 bytes", page(og("./".repeat(506) + "img/cover.png")), "preview.svg"],
    ["no content attribute", page(`<meta property="og:image">`), "preview.svg"],
    ["only the first og:image counts", page(og("https://example.com/x.png") + og("img/cover.png")), "preview.svg"],
    ["a property attribute other than og:image hides the name attribute", page(`<meta property="og:title" name="og:image" content="img/cover.png">`), "preview.svg"],
    ["only ASCII white space is trimmed", page(og("&#xA0;img/cover.png")), "preview.svg"],
    ["an og:image inside a comment", page(`<!-- ${og("img/cover.png")} -->`), "preview.svg"],
    ["an og:image inside a script", page(`<script>const t = '${og("img/cover.png")}';</script>`), "preview.svg"],
    ["an og:image after the first 64 KiB", page("", `<p>${"x".repeat(65 * 1024)}</p>${og("img/cover.png")}`), "preview.svg"],
  ];

  for (const [what, index, want] of cases) {
    test(`og:image: ${what} → ${want}`, async () => {
      const m = await deploy(site, { "index.html": index, ...assets });
      assert.equal(m.preview, want, `preview of ${index.length > 400 ? index.slice(0, 400) + "…" : index}`);
    });
  }

  test("an unusable og:image without a preview.* file means no preview", async () => {
    const { "preview.svg": _, ...rest } = assets;
    const m = await deploy(site, { "index.html": page(og("https://example.com/cover.png")), ...rest });
    assert.ok(!("preview" in m), `no preview: ${JSON.stringify(m)}`);
  });
});

describe("preview.* fallback order (SPEC §7.2)", { skip }, () => {
  const site = uniqueSite("prevord");
  const index = page("");
  const all = {
    "preview.png": Buffer.alloc(MAX + 1, 0x63), // too large: skipped
    "preview.jpg": Buffer.alloc(MAX, 0x64), // exactly 1 MiB
    "preview.jpeg": fake("jpeg"),
    "preview.webp": fake("webp"),
    "preview.avif": fake("avif"),
    "preview.gif": fake("gif"),
    "preview.svg": svg("order"),
  };
  const without = (...names) => Object.fromEntries(Object.entries(all).filter(([k]) => !names.includes(k)));
  const steps = [
    ["preview.png over 1 MiB is skipped for preview.jpg of exactly 1 MiB", all, "preview.jpg"],
    ["then preview.jpeg", without("preview.png", "preview.jpg"), "preview.jpeg"],
    ["then preview.webp", without("preview.png", "preview.jpg", "preview.jpeg"), "preview.webp"],
    ["then preview.avif", without("preview.png", "preview.jpg", "preview.jpeg", "preview.webp"), "preview.avif"],
    ["then preview.gif", without("preview.png", "preview.jpg", "preview.jpeg", "preview.webp", "preview.avif"), "preview.gif"],
    [
      "then preview.svg; other letter cases and other folders do not count",
      { "preview.svg": all["preview.svg"], "PREVIEW.PNG": png, "Preview.jpg": png, "img/preview.png": png, "preview.png.txt": "x" },
      "preview.svg",
    ],
  ];
  for (const [what, files, want] of steps) {
    test(`${what} → ${want}`, async () => {
      const m = await deploy(site, { "index.html": index, ...files });
      assert.equal(m.preview, want);
    });
  }

  test("no preview.* file within 1 MiB means no preview", async () => {
    const m = await deploy(site, { "index.html": index, "preview.svg": Buffer.alloc(MAX + 1, 0x65), "PREVIEW.PNG": png });
    assert.ok(!("preview" in m), `no preview: ${JSON.stringify(m)}`);
  });
});

describe("preview lifecycle and GET /__backyard/api/sites/{site}/preview (SPEC §3)", { skip }, () => {
  const site = uniqueSite("prevapi");
  const cover = svg("cover");
  const png2 = Buffer.concat([png, Buffer.from("second version")]);
  const v1 = {
    "index.html": page(og("./img/Shot.SVG?v=1")),
    // The declared type is never served: the route answers by extension.
    "img/Shot.SVG": cover,
    "preview.png": png,
  };
  let first;
  let etag;

  /** Asserts the headers of a 200 or HEAD answer of the route. */
  function assertImage(r, type, what) {
    assert.equal(r.header("content-type"), type, `${what}: Content-Type`);
    assert.equal(r.header("content-security-policy"), CSP, `${what}: Content-Security-Policy`);
    assert.equal(r.header("x-content-type-options"), "nosniff", `${what}: nosniff`);
    assert.equal(r.header("content-disposition"), null, `${what}: no Content-Disposition`);
    assert.match(r.header("etag") || "", /^"[^"]+"$/, `${what}: a strong ETag`);
  }

  test("a deploy sets the preview everywhere SiteMeta appears", async () => {
    first = await deploy(site, v1, { contentTypes: { "img/Shot.SVG": "text/html; charset=utf-8" } });
    assert.equal(first.preview, "img/Shot.SVG");
    await metaEverywhere(site, "img/Shot.SVG");
  });

  test("refresh derives the same preview from storage", async () => {
    const r = expectStatus(await client.post(`${env.gateway}/sites/${site}/refresh`, {}), 200).json;
    assertSiteMeta(r, { name: site, url: env.canonicalSiteURL(site), what: "refresh" });
    assert.equal(r.preview, "img/Shot.SVG");
  });

  test("GET serves the bytes by the path's extension, sandboxed, revalidated without v", async () => {
    const r = expectStatus(await client.get(previewURL(site)), 200, "GET");
    assert.deepEqual(r.body, cover, "the file's bytes");
    assertImage(r, "image/svg+xml", "GET");
    assert.equal(r.header("cache-control"), REVALIDATE, "Cache-Control without v");
    etag = r.header("etag");
    for (const q of ["?v=", "?other=1"]) {
      const o = expectStatus(await client.get(previewURL(site, q)), 200, q);
      assert.equal(o.header("cache-control"), REVALIDATE, `Cache-Control with ${q}`);
    }
  });

  test("a non-empty v makes it immutable (the hub's ?v=<updated_at>)", async () => {
    for (const q of [`?v=${encodeURIComponent(first.updated_at)}`, "?x=1&v=2"]) {
      const r = expectStatus(await client.get(previewURL(site, q)), 200, q);
      assert.deepEqual(r.body, cover);
      assertImage(r, "image/svg+xml", q);
      assert.equal(r.header("cache-control"), IMMUTABLE, `Cache-Control with ${q}`);
      assert.equal(r.header("etag"), etag, "the ETag does not depend on v");
    }
  });

  test("conditional GET and HEAD", async () => {
    const nm = await client.get(previewURL(site), { headers: { "If-None-Match": etag } });
    assert.equal(nm.status, 304, `If-None-Match: ${nm}`);
    assert.equal(nm.body.length, 0);
    assert.equal(nm.header("etag"), etag);
    const other = expectStatus(await client.get(previewURL(site), { headers: { "If-None-Match": '"something-else"' } }), 200, "another ETag");
    assert.deepEqual(other.body, cover);
    const h = expectStatus(await client.head(previewURL(site, "?v=1")), 200, "HEAD");
    assert.equal(h.body.length, 0, "HEAD has no body");
    assert.equal(h.header("content-length"), String(cover.length));
    assertImage(h, "image/svg+xml", "HEAD");
    assert.equal(h.header("cache-control"), IMMUTABLE);
    assert.equal(h.header("etag"), etag);
  });

  test("other methods are 405 with Allow", async () => {
    for (const method of ["POST", "PUT", "DELETE"]) {
      const r = await client.fetch(previewURL(site), { method, json: {} });
      assertError(r, 405, "bad_request", method);
      assert.match(r.header("allow") || "", /\bGET\b/, `${method}: Allow`);
    }
  });

  test("the route passes the gate of the site list", async () => {
    for (const [what, c] of [
      ["no credentials", new Client({ token: null })],
      ["an invalid bearer token", new Client({ token: "not-a-valid-token" })],
    ]) {
      const list = await c.get(`${hubURL}/__backyard/api/sites`);
      const img = await c.get(previewURL(site));
      if (list.status === 200) {
        expectStatus(img, 200, what);
        assert.deepEqual(img.body, cover, what);
      } else {
        assert.equal(img.status, list.status, `${what}: the same answer as the site list (${list}): ${img}`);
        assertError(img, list.status, list.json.error.code, what);
      }
    }
  });

  test("unknown sites, invalid names and other hosts are 404 not_found", async () => {
    assertError(await client.get(previewURL(uniqueSite("ghost"))), 404, "not_found", "an unknown site");
    assertError(await client.get(previewURL("Bad_Name")), 404, "not_found", "an invalid site name");
    assertError(await client.get(previewURL(site) + "/x"), 404, "not_found", "a longer path");
    // Hub host only: on a site (its host, or its /s/<site> prefix) the path is the site API's.
    assertError(await client.get(env.api(site, `/sites/${site}/preview`)), 404, "not_found", "on the site");
  });

  test("a redeploy changes the preview and the route serves the new file", async () => {
    const m = await deploy(site, { "index.html": page(""), "preview.png": png2 });
    assert.equal(m.preview, "preview.png");
    await metaEverywhere(site, "preview.png");
    const r = expectStatus(await client.get(previewURL(site, `?v=${encodeURIComponent(m.updated_at)}`)), 200);
    assert.deepEqual(r.body, png2, "the new bytes, not a cached copy");
    assertImage(r, "image/png", "PNG");
    assert.notEqual(r.header("etag"), etag, "a new ETag");
    const stale = expectStatus(await client.get(previewURL(site), { headers: { "If-None-Match": etag } }), 200, "the old ETag");
    assert.deepEqual(stale.body, png2);
  });

  test("a redeploy without a preview removes it; the route answers 404", async () => {
    const m = await deploy(site, { "index.html": page(og("img/Shot.SVG")) });
    assert.ok(!("preview" in m), `no preview: ${JSON.stringify(m)}`);
    await metaEverywhere(site, undefined);
    assertError(await client.get(previewURL(site)), 404, "not_found", "a site without a preview");
    expectStatus(await client.head(previewURL(site)), 404, "HEAD of a site without a preview");
    const r = expectStatus(await client.post(`${env.gateway}/sites/${site}/refresh`, {}), 200).json;
    assert.ok(!("preview" in assertSiteMeta(r, { name: site, what: "refresh" })), "refresh finds none either");
  });
});
