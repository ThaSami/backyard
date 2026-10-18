/** Unit tests of the pure modules. */
import { describe, expect, it } from "vitest";
import { readSSE } from "../src/ai/sse";
import { retryHint, safeUpstreamMessage } from "../src/ai/types";
import { extractText } from "../src/ai/workersai";
import {
  DEFAULT_LIMITS,
  DEFAULT_MCP_CLIENTS,
  DEFAULT_MCP_REDIRECT_URIS,
  isLoopbackHost,
  mcpRedirectURIHost,
  normalizeTeam,
  parseConfig,
  validMCPClientURL,
} from "../src/config";
import { cookiePathPrefixes, secretMissing } from "../src/auth";
import { b64urlDecode, base64Decode, base64Encode, base64Head, base64Size, decodeAnyBase64, normalizeBase64 } from "../src/crypto";
import { buildQuery, normalizeQuery, parseListParams } from "../src/db/query";
import { formatMicros, formatMillisFixed, MicroClock, parseRFC3339Micros, toFixedMillis } from "../src/db/time";
import { canonicalDoc, MAX_NESTING_DEPTH } from "../src/db/values";
import { newULID } from "../src/db/ulid";
import type { Env } from "../src/env";
import { REFERENCES, SKILL_MD } from "../src/generated/skills";
import { HttpError } from "../src/http";
import { baseURL, docMarkdown, HUB_URL_PLACEHOLDER, localize } from "../src/llms";
import { validateLoopbackRedirect } from "../src/hub";
import { contentTypeFor, detectContentType, isActiveContent, normalizeContentType } from "../src/mime";
import { checkPreconditions, parseRange, serveObject, type Servable } from "../src/object";
import { sanitizeFileName, validatePath } from "../src/paths";
import { cleanPath, siteNameError } from "../src/router";
import { compareMembers, contentDisposition, secondsToMidnight } from "../src/site-do";
import { extractPage, unescapeHTML } from "../src/title";

describe("paths and names", () => {
  it.each([
    ["/a/b", "/a/b"],
    ["/a//b", "/a/b"],
    ["/a/./b/", "/a/b/"],
    ["/a/../b", "/b"],
    ["/../x", "/x"],
    ["/", "/"],
    ["", "/"],
    ["/a/..", "/"],
  ])("cleanPath(%s) = %s", (p, want) => expect(cleanPath(p)).toBe(want));

  it("validates site names", () => {
    expect(siteNameError("lunch-vote", [])).toBe("");
    expect(siteNameError("a", [])).toBe("");
    expect(siteNameError("-a", [])).not.toBe("");
    expect(siteNameError("a-", [])).not.toBe("");
    expect(siteNameError("A", [])).not.toBe("");
    expect(siteNameError("a".repeat(64), [])).not.toBe("");
    expect(siteNameError("api", ["api"])).toContain("reserved");
  });

  it.each([
    ["index.html", ""],
    ["a/b/c.js", ""],
    ["", "empty path"],
    ["/abs", "path must be relative (no leading /)"],
    ["a//b", "path contains an empty segment"],
    ["a/../b", 'path may not contain "." or ".." segments'],
    ["./a", 'path may not contain "." or ".." segments'],
    ["__backyard/x", 'path may not start with "__backyard/"'],
    ["__backyard", 'path may not start with "__backyard/"'],
    ["a\\b", 'path may not contain "\\"'],
    ["a\nb", "path contains a control character"],
    ["x".repeat(1025), "path longer than 1024 bytes"],
    [".git", 'path may not contain a ".git" segment'],
    [".git/config", 'path may not contain a ".git" segment'],
    ["a/.GIT/HEAD", 'path may not contain a ".git" segment'],
    ["vendor/lib/.Git", 'path may not contain a ".git" segment'],
    [".github/workflows/ci.yml", ""],
    [".gitignore", ""],
    ["a/.git.bak/x", ""],
    ["a/x.git", ""],
    ["git/HEAD", ""],
  ])("validatePath(%j)", (p, want) => expect(validatePath(p)).toBe(want));

  it("sanitizes upload names", () => {
    expect(sanitizeFileName("../../etc/passwd")).toBe("passwd");
    expect(sanitizeFileName("C:\\Users\\me\\photo.jpg")).toBe("photo.jpg");
    expect(sanitizeFileName('a"b<c>d|e*f?g:h.txt')).toBe("a_b_c_d_e_f_g_h.txt");
    expect(sanitizeFileName("  .hidden. ")).toBe("hidden");
    expect(sanitizeFileName("")).toBe("file");
    expect(sanitizeFileName("..")).toBe("file");
    expect(sanitizeFileName("tab\there")).toBe("tab_here");
    const long = sanitizeFileName("x".repeat(300) + ".png");
    expect(long.endsWith(".png")).toBe(true);
    expect(new TextEncoder().encode(long).length).toBeLessThanOrEqual(200);
    expect(sanitizeFileName("résumé.pdf")).toBe("résumé.pdf");
  });

  it("validates CLI loopback redirects", () => {
    expect(validateLoopbackRedirect("http://127.0.0.1:8080/cb").port).toBe("8080");
    expect(validateLoopbackRedirect("http://localhost:1/").hostname).toBe("localhost");
    expect(() => validateLoopbackRedirect("http://[::1]:9/x")).not.toThrow();
    for (const bad of ["", "https://127.0.0.1:1/", "http://127.0.0.1/", "http://example.com:80/", "http://127.0.0.1:1/#x", "http://a@127.0.0.1:1/", "javascript:alert(1)"]) {
      expect(() => validateLoopbackRedirect(bad), bad).toThrow();
    }
  });
});

describe("timestamps and ids", () => {
  it("formats microseconds like Go's RFC3339Nano", () => {
    expect(formatMicros(0)).toBe("1970-01-01T00:00:00Z");
    expect(formatMicros(1_500_000)).toBe("1970-01-01T00:00:01.5Z");
    expect(formatMicros(1_791_842_930_123_456)).toBe("2026-10-12T22:08:50.123456Z");
    expect(formatMicros(1_791_842_930_120_000)).toBe("2026-10-12T22:08:50.12Z");
  });

  it("formats presence joined_at with exactly three fractional digits (Go joinedAtLayout)", () => {
    expect(formatMillisFixed(0)).toBe("1970-01-01T00:00:00.000Z");
    expect(formatMillisFixed(1_791_842_930_120)).toBe("2026-10-12T22:08:50.120Z");
    expect(formatMillisFixed(1_791_842_930_100)).toBe("2026-10-12T22:08:50.100Z");
    expect(formatMillisFixed(1_791_842_930_123.9)).toBe("2026-10-12T22:08:50.123Z");
    // Fixed width: the string order is the chronological order.
    const times = [0, 5, 50, 120, 999, 1000, 1001, 59_999, 1_791_842_930_000, 1_791_842_930_001, 1_791_842_930_120, 1_791_842_931_000];
    const shuffled = [...times].reverse().map(formatMillisFixed);
    expect(shuffled.sort()).toEqual(times.map(formatMillisFixed));
    expect(new Set(times.map((t) => formatMillisFixed(t).length))).toEqual(new Set([24]));
    // State saved by older versions (variable width, offsets) is converted.
    expect(toFixedMillis("2026-10-12T22:08:50.12Z")).toBe("2026-10-12T22:08:50.120Z");
    expect(toFixedMillis("2026-10-12T22:08:50Z")).toBe("2026-10-12T22:08:50.000Z");
    expect(toFixedMillis("2026-10-13T03:38:50.123456+05:30")).toBe("2026-10-12T22:08:50.123Z");
    expect(toFixedMillis("2026-10-12T22:08:50.120Z")).toBe("2026-10-12T22:08:50.120Z");
    expect(toFixedMillis("garbage")).toBe("garbage");
    // Members order by joined_at, then cid (internal/realtime sortMembers).
    const t1 = formatMillisFixed(1000);
    const t2 = formatMillisFixed(1001);
    expect(compareMembers(t1, "c_b", t2, "c_a")).toBeLessThan(0);
    expect(compareMembers(t1, "c_b", t1, "c_a")).toBeGreaterThan(0);
    expect(compareMembers(t1, "c_a", t1, "c_a")).toBe(0);
  });

  it("orders and filters _meta timestamps as integers, not as their variable-width text", () => {
    // "…50.12Z" sorts after "…50.123Z" as text although it is earlier.
    expect(formatMicros(1_791_842_930_120_000) > formatMicros(1_791_842_930_123_000)).toBe(true);
    const q = normalizeQuery(
      { where: [{ field: "_meta.created_at", op: ">", value: "2026-10-12T22:08:50.12Z" }], orderBy: [{ field: "_meta.updated_at", dir: "desc" }] },
      1000,
    );
    const { sql, params } = buildQuery("c", q);
    expect(sql).toContain("created_at > ?");
    expect(sql).toContain("ORDER BY updated_at DESC, id ASC");
    expect(params).toContain(1_791_842_930_120_000);
  });

  it("parses RFC 3339 with offsets and fractions", () => {
    expect(parseRFC3339Micros("1970-01-01T00:00:01.5Z")).toBe(1_500_000);
    expect(parseRFC3339Micros("1970-01-01T05:30:00+05:30")).toBe(0);
    expect(parseRFC3339Micros("2026-10-12T22:08:50.1234567891Z")).toBe(1_791_842_930_123_456);
    expect(parseRFC3339Micros("1000-01-01T00:00:00Z")).toBeLessThan(0);
    expect(parseRFC3339Micros("9999-12-31T23:59:59.999999999Z")).toBeGreaterThan(0);
    for (const bad of ["garbage", "2026-02-30T00:00:00Z", "2026-13-01T00:00:00Z", "2026-01-01 00:00:00Z", "2026-01-01T24:00:00Z", 123, null]) {
      expect(parseRFC3339Micros(bad), String(bad)).toBeNull();
    }
    expect(parseRFC3339Micros("2024-02-29T00:00:00Z")).not.toBeNull();
    expect(parseRFC3339Micros("2023-02-29T00:00:00Z")).toBeNull();
  });

  it("keeps a strictly increasing clock", () => {
    const c = new MicroClock();
    c.observe(Date.now() * 1000 + 5_000_000);
    const a = c.now();
    const b = c.now();
    expect(b).toBeGreaterThan(a);
  });

  it("generates sortable ULIDs", () => {
    const ids = Array.from({ length: 50 }, () => newULID(1_700_000_000_000));
    expect(ids.every((id) => /^[0-9A-HJKMNP-TV-Z]{26}$/.test(id))).toBe(true);
    expect([...ids].sort()).toEqual(ids);
    expect(new Set(ids).size).toBe(50);
    expect(newULID(1_700_000_000_001) > ids[49]!).toBe(true);
    // A clock going backwards keeps ids increasing.
    expect(newULID(0) > ids[49]!).toBe(true);
  });
});

describe("queries", () => {
  it("normalizes defaults and the id tiebreaker", () => {
    expect(normalizeQuery({}, 1000)).toEqual({ where: [], orderBy: [{ field: "_meta.created_at", desc: false }, { field: "id", desc: false }], limit: 100, offset: 0 });
    expect(normalizeQuery({ limit: 5000 }, 1000).limit).toBe(1000);
    expect(normalizeQuery({ orderBy: [{ field: "id", dir: "DESC" }] }, 1000).orderBy).toEqual([{ field: "id", desc: true }]);
    expect(() => normalizeQuery({ orderBy: Array.from({ length: 6 }, () => ({ field: "a" })) }, 10)).toThrow();
  });

  it("binds every value as a parameter (≤ 100 per query)", () => {
    const where = Array.from({ length: 20 }, (_, i) => ({ field: "f" + i, op: "in", value: [...Array.from({ length: 15 }, (_, j) => j), ...Array.from({ length: 15 }, (_, j) => "s" + j)] }));
    const { sql, params } = buildQuery("c", normalizeQuery({ where }, 1000));
    expect(params.length).toBeLessThanOrEqual(100);
    expect(sql).not.toContain("s14");
    const evil = buildQuery("c", normalizeQuery({ where: [{ field: "a", op: "==", value: "'; DROP TABLE docs; --" }] }, 10));
    expect(evil.sql).not.toContain("DROP");
    expect(evil.params).toContain("'; DROP TABLE docs; --");
  });

  it("parses list parameters", () => {
    expect(parseListParams(new URLSearchParams("limit=5&offset=2&order=a:desc,b"))).toEqual({ limit: 5, offset: 2, orderBy: [{ field: "a", dir: "desc" }, { field: "b", dir: "" }] });
    expect(parseListParams(new URLSearchParams("orderBy=a,b&order=desc"))).toEqual({ orderBy: [{ field: "a", dir: "desc" }, { field: "b", dir: "desc" }] });
    expect(() => parseListParams(new URLSearchParams("limit=x"))).toThrow();
    expect(() => parseListParams(new URLSearchParams("order=a:up"))).toThrow();
  });
});

describe("HTTP objects", () => {
  const obj = (): Servable => ({ name: "a.txt", size: 10, etag: '"abc"', lastModified: Date.UTC(2026, 0, 1, 12, 0, 0, 500), contentType: "text/plain", data: new TextEncoder().encode("0123456789") });

  it.each([
    ["bytes=0-3", { kind: "ok", start: 0, length: 4 }],
    ["bytes=5-", { kind: "ok", start: 5, length: 5 }],
    ["bytes=-3", { kind: "ok", start: 7, length: 3 }],
    ["bytes=-30", { kind: "ok", start: 0, length: 10 }],
    ["bytes=8-100", { kind: "ok", start: 8, length: 2 }],
    ["bytes=10-", { kind: "unsatisfiable" }],
    ["bytes=-0", { kind: "unsatisfiable" }],
    ["bytes=3-1", { kind: "ignore" }],
    ["bytes=0-1,3-4", { kind: "ignore" }],
    ["items=0-1", { kind: "ignore" }],
    ["bytes=x-", { kind: "ignore" }],
  ])("parseRange(%s)", (h, want) => expect(parseRange(h, 10)).toEqual(want));

  it("evaluates preconditions", () => {
    const o = obj();
    const req = (h: Record<string, string>, method = "GET") => new Request("https://x/", { method, headers: h });
    expect(checkPreconditions(req({ "If-None-Match": '"abc"' }), o)).toBe(304);
    expect(checkPreconditions(req({ "If-None-Match": '"zzz", W/"abc"' }), o)).toBe(304);
    expect(checkPreconditions(req({ "If-None-Match": "*" }), o)).toBe(304);
    expect(checkPreconditions(req({ "If-None-Match": '"zzz"' }), o)).toBe(0);
    expect(checkPreconditions(req({ "If-Match": '"zzz"' }), o)).toBe(412);
    expect(checkPreconditions(req({ "If-Match": 'W/"abc"' }), o)).toBe(412);
    expect(checkPreconditions(req({ "If-Modified-Since": "Thu, 01 Jan 2026 12:00:00 GMT" }), o)).toBe(304);
    expect(checkPreconditions(req({ "If-Modified-Since": "Thu, 01 Jan 2026 11:59:59 GMT" }), o)).toBe(0);
    expect(checkPreconditions(req({ "If-Unmodified-Since": "Thu, 01 Jan 2026 11:00:00 GMT" }), o)).toBe(412);
    expect(checkPreconditions(req({ "If-None-Match": '"abc"' }, "POST"), o)).toBe(412);
  });

  it("serves ranges only when If-Range matches", async () => {
    let res = serveObject(new Request("https://x/", { headers: { Range: "bytes=0-1", "If-Range": '"abc"' } }), obj());
    expect(res.status).toBe(206);
    res = serveObject(new Request("https://x/", { headers: { Range: "bytes=0-1", "If-Range": '"old"' } }), obj());
    expect(res.status).toBe(200);
    expect(await res.text()).toBe("0123456789");
    expect(() => serveObject(new Request("https://x/", { headers: { "If-Match": '"no"' } }), obj())).toThrow();
  });

  it("picks content types", () => {
    expect(contentTypeFor("x.HTML")).toBe("text/html; charset=utf-8");
    expect(contentTypeFor("x.ts")).toBe("text/plain; charset=utf-8");
    expect(contentTypeFor("x.unknown")).toBe("application/octet-stream");
    expect(contentTypeFor("x", "text/csv")).toBe("text/csv; charset=utf-8");
    expect(contentTypeFor(".htaccess")).toBe("application/octet-stream");
    expect(detectContentType("blob", new TextEncoder().encode("<!DOCTYPE html><p>"))).toBe("text/html; charset=utf-8");
    expect(detectContentType("blob", new Uint8Array([0xff, 0xd8, 0xff, 0]))).toBe("image/jpeg");
    expect(detectContentType("blob", new Uint8Array([0, 1, 2]))).toBe("application/octet-stream");
    expect(normalizeContentType("Text/Plain; Charset=UTF-8")).toBe("text/plain; charset=UTF-8");
    expect(normalizeContentType("garbage")).toBe("");
    expect(isActiveContent("image/svg+xml")).toBe(true);
    expect(isActiveContent("image/png")).toBe(false);
    expect(contentDisposition("inline", "a b.txt")).toBe('inline; filename="a b.txt"');
    expect(contentDisposition("inline", "naïve.txt")).toBe(`inline; filename="na_ve.txt"; filename*=UTF-8''na%C3%AFve.txt`);
  });
});

describe("upload sandboxing (an allowlist of passive types, like Go's activeContent)", () => {
  it.each([
    "text/html",
    "text/html; charset=utf-8",
    "application/xhtml+xml",
    "image/svg+xml",
    "text/xml",
    "application/xml",
    "text/xsl",
    "application/xslt+xml",
    "application/rss+xml",
    "text/markdown",
    "text/javascript",
    "application/x-unknown",
    "video/x+xml",
    "audio/svg+xml",
    "multipart/x-mixed-replace",
    "garbage",
    "",
    "text/",
  ])("sandboxes %j", (ct) => expect(isActiveContent(ct)).toBe(true));
  it.each([
    "image/png",
    "image/jpeg",
    "image/gif",
    "image/webp",
    "image/avif",
    "image/x-icon",
    "text/plain; charset=utf-8",
    "text/csv",
    "application/pdf",
    "application/json",
    "application/octet-stream",
    "audio/mpeg",
    "video/mp4",
    "Video/WebM",
  ])("serves %j as is", (ct) => expect(isActiveContent(ct)).toBe(false));
});

describe("titles (as internal/sites/title.go)", () => {
  it.each([
    ["<title>Hello</title>", "Hello", ""],
    ["<TITLE>  A &amp;\n B </TITLE>", "A & B", ""],
    ['<meta name="description" content="Desc &lt;3"><title>T</title>', "T", "Desc <3"],
    ["<!-- <title>No</title> --><title>Yes</title>", "Yes", ""],
    ["<script>var s='<title>No</title>'</script><title>Yes</title>", "Yes", ""],
    ["<svg><title>icon</title></svg><title>Page</title>", "Page", ""],
    ["<titlex>no</titlex>", "", ""],
    ["<title>unterminated", "unterminated", ""],
    ["<meta name=Description content=unquoted>", "", "unquoted"],
    ["<title>&constructor; &toString; &amp;</title>", "&constructor; &toString; &", ""],
    ['<meta constructor="x" name="description" content="d">', "", "d"],
    // The cases of TestExtractTitle.
    ["", "", ""],
    ['<HTML><HEAD><TITLE>Shout</TITLE><META NAME="Description" CONTENT="Loud"></HEAD>', "Shout", "Loud"],
    ["<title>Tom &amp; Jerry &#8212; &lt;3 &quot;x&quot;</title>", 'Tom & Jerry \u2014 <3 "x"', ""],
    ["<title>\n   Multi\n\tline   title \n</title>", "Multi line title", ""],
    [`<title id="t" data-x='>'>Attr</title>`, "Attr", ""],
    [`<meta content='Single &amp; quoted' name='description' />`, "", "Single & quoted"],
    ["<meta name=description content=https://example.com/a/b>", "", "https://example.com/a/b"],
    ['<meta name="description" content="Closed"/>', "", "Closed"],
    ['<meta charset="utf-8"><meta name="viewport" content="width=device-width"><meta name="description" content="Real">', "", "Real"],
    ["<title>One</title><title>Two</title><meta name=description content=a><meta name=description content=b>", "One", "a"],
    ["<svg/><title>After svg</title>", "After svg", ""],
    ["<!-- <title>x</title>", "", ""],
    ["<title></title><meta name=description content=''>", "", ""],
  ])("extractPage(%j)", (html, title, description) => expect(extractPage(html)).toEqual({ title, description, image: "" }));

  it("reads the bytes as Go does: invalid UTF-8 becomes one U+FFFD per run, also at the 64 KiB cut", () => {
    const enc = (s: string) => new TextEncoder().encode(s);
    const join = (...parts: (string | number[])[]) => new Uint8Array(parts.flatMap((p) => (typeof p === "string" ? [...enc(p)] : p)));
    expect(extractPage(join("<title>bad", [0xff], "byte</title>")).title).toBe("bad\ufffdbyte");
    expect(extractPage(join("<title>a", [0xff, 0xfe], "b", [0xc3], "c", [0xe2, 0x82], "</title>")).title).toBe("a\ufffdb\ufffdc\ufffd");
    // A character cut by the 64 KiB limit is an invalid byte there.
    const cut = join("<title>", " ".repeat(64 * 1024 - 8), "\u00e9\u00e9</title>");
    expect(extractPage(cut).title).toBe("\ufffd");
    expect(extractPage(" ".repeat(64 * 1024) + "<title>Too late</title>").title).toBe("");
  });

  it("collapses Go's white space (with U+0085, without U+FEFF) and caps the length", () => {
    expect(extractPage("<title>a\u0085b\u3000c\ufeffd</title>").title).toBe("a b c\ufeffd");
    const long = extractPage(`<title>${"a".repeat(5000)}</title><meta name=description content=${"b".repeat(5000)}>`);
    expect([...long.title]).toHaveLength(256);
    expect(long.title.endsWith("a\u2026")).toBe(true);
    expect([...long.description]).toHaveLength(1024);
    // U+FEFF is no white space: the cut keeps it.
    expect(extractPage(`<title>${"a".repeat(254)}\ufeffb c</title>`).title).toBe("a".repeat(254) + "\ufeff\u2026");
    expect(extractPage(`<title>${"\u00e9".repeat(300)}</title>`).title).toBe("\u00e9".repeat(255) + "\u2026");
  });

  it("matches the description like strings.EqualFold(strings.TrimSpace(name))", () => {
    expect(extractPage('<meta name="de\u017fcription" content="long s">').description).toBe("long s");
    expect(extractPage('<meta name=" description\u0085" content="x">').description).toBe("x");
    expect(extractPage('<meta name="description\ufeff" content="x">').description).toBe("");
    expect(extractPage('<meta name="DESCR\u0130PTION" content="x">').description).toBe("");
  });

  it("trims attribute values in linear time, with Go's two sets of white space", () => {
    // A long run of white space inside a value made the old trimming regexes (/^[S]+|[S]+$/) quadratic: seconds per page.
    const L = 65400;
    for (const [what, html, want] of [
      ["name", `<meta name="x${" ".repeat(L)}x" content="d">`, { title: "", description: "", image: "" }],
      ["property", `<meta property="x${"\t".repeat(L)}x" content="c">`, { title: "", description: "", image: "" }],
      ["content", `<meta property="og:image" content="x${" ".repeat(L)}x">`, { title: "", description: "", image: `x${" ".repeat(L)}x` }],
      ["name with Go's spaces", `<meta name="d${"\u3000\u0085\v".repeat(L / 3)}d">`, { title: "", description: "", image: "" }],
      ["title cut", `<title>${"a".repeat(254)}${" \u2028".repeat(16000)}b c</title>`, { title: "a".repeat(254) + "\u2026", description: "", image: "" }],
    ] as const) {
      const start = performance.now();
      const page = extractPage(html);
      const ms = performance.now() - start;
      expect(page, what).toEqual(want);
      expect(ms, what).toBeLessThan(50);
    }
    // strings.TrimSpace (unicode.IsSpace) for the name, HTML's ASCII white space for the og:image key and content.
    expect(extractPage(`<meta name="${" ".repeat(L)}description\u3000\v\u00a0" content="d">`).description).toBe("d");
    expect(extractPage('<meta name="\u1680\u2000\u200a\u2028\u2029\u202f\u205fdescription\t\n\r\f" content="d">').description).toBe("d");
    expect(extractPage('<meta name="\u200bdescription" content="d">').description).toBe("");
    expect(extractPage('<meta property=" \t\n\r\fog:image \t\n\r\f" content=" \t\n\r\fa.png \t\n\r\f">').image).toBe("a.png");
    expect(extractPage('<meta property="\vog:image" content="a.png">').image).toBe("");
    expect(extractPage('<meta property="og:image\u00a0" content="a.png">').image).toBe("");
    expect(extractPage('<meta property="og:image" content="&nbsp;a.png\v\u3000 ">').image).toBe("\u00a0a.png\v\u3000");
  });

  it.each([
    ["&amp; &amp &AMP; &ampx &lt3 &notit; &not;", "& & & &x <3 \u00acit; \u00ac"],
    ["&#47; &#x2F; &#X2f; &#47 &#x2F", "/ / / / /"],
    ["&#128; &#x9F; &#150;", "\u20ac \u0178 \u2013"],
    ["&#0; &#xD800; &#x110000; &#99999999999; &#x;", "\ufffd \ufffd \ufffd \ufffd \ufffd"],
    ["&#; &# &#x &#xg; &; & x &", "&#; &# &#x &#xg; &; & x &"],
    ["&eacute;&Eacute&rsquo;&hellip;&euro;&sol;&period;&lowbar;&nbsp;&apos;", "\u00e9\u00c9\u2019\u2026\u20ac/._\u00a0'"],
    ["&lang;&rang;&constructor;&hasOwnProperty;&__proto__;", "\u27e8\u27e9&constructor;&hasOwnProperty;&__proto__;"],
    // HTML 5 names, two code points, and the two Go leaves out.
    ["&check; &Ascr; &NotEqualTilde; &fjlig; &nLt; &notinva; &notinvax &ThickSpace;", "\u2713 \u{1d49c} \u2242\u0338 fj &nLt; \u2209 \u00acinvax \u205f\u200a"],
  ])("unescapeHTML(%j) as Go's html.UnescapeString", (s, want) => expect(unescapeHTML(s)).toBe(want));
});

describe("config", () => {
  const base = { BACKYARD_SECRET: "x".repeat(32) } as unknown as Env;

  it("applies defaults", () => {
    const c = parseConfig(base);
    expect(c.urlMode).toBe("path");
    expect(c.authMode).toBe("none");
    expect(c.limits).toEqual(DEFAULT_LIMITS);
    expect(c.fileInlineMax).toBe(5 * 1024 * 1024);
    expect(c.tokenTTLSeconds).toBe(720 * 3600);
    expect(c.ai.defaultChat).toBe("smart");
    expect(c.ai.defaultImage).toBe("image");
    expect(c.ai.defaultEmbedding).toBe("embed");
    expect(c.problems).toEqual([]);
  });

  it("parses vars and reports problems", () => {
    const c = parseConfig({
      ...base,
      URL_MODE: "host",
      BASE_DOMAIN: "https://Backyard.Acme.com/",
      AUTH_MODE: "access",
      ACCESS_TEAM_DOMAIN: "acme",
      ACCESS_AUD: "a1, a2",
      ADMINS: "Root@Acme.com",
      LIMITS: '{"ai_per_day": 5, "bogus": 1, "max_sites": -1}',
      AI_MODELS: JSON.stringify({ claude: { provider: "anthropic", model: "claude-opus-5-5", effort: "high" }, bad: { provider: "x", model: "y" } }),
    } as unknown as Env);
    expect(c.urlMode).toBe("host");
    expect(c.baseDomain).toBe("backyard.acme.com");
    expect(c.accessTeam).toBe("acme.cloudflareaccess.com");
    expect(c.accessAud).toEqual(["a1", "a2"]);
    expect(c.admins).toEqual(["root@acme.com"]);
    expect(c.limits.ai_per_day).toBe(5);
    expect(c.limits.max_sites).toBe(DEFAULT_LIMITS.max_sites);
    expect(c.ai.models).toEqual({ claude: { provider: "anthropic", model: "claude-opus-5-5", kind: "chat", effort: "high" } });
    expect(c.ai.defaultChat).toBe("claude");
    expect(c.problems.join("\n")).toContain("bogus");
    expect(c.problems.join("\n")).toContain("AI_MODELS.bad");
    expect(parseConfig({ ...base, URL_MODE: "host" } as unknown as Env).urlMode).toBe("path");
    expect(parseConfig({} as Env).problems.join()).toContain("BACKYARD_SECRET");
    // The remedy works without a checkout (npm run setup does not).
    const missing = [parseConfig({} as Env).problems.join(), secretMissing().message].join("\n");
    expect(missing).not.toContain("npm run setup");
    expect(missing).toContain("backyard cloudflare setup or npx backyard-cloudflare setup");
    expect(normalizeTeam("https://Team.cloudflareaccess.com/")).toBe("team.cloudflareaccess.com");
  });

  it("fails closed for an AUTH_MODE other than none or access", () => {
    for (const mode of ["oidc", "okta", "cf-access", "cloudflare-access", "acess", "iap", "true"]) {
      const c = parseConfig({ ...base, AUTH_MODE: mode } as unknown as Env);
      expect(c.fatal, mode).toContain('AUTH_MODE must be "none" or "access"');
      expect(c.problems.join(), mode).toContain("503");
    }
    // Not a string at all (a JSON variable) is no mode either.
    for (const v of [true, 1, { mode: "access" }, ["access"]]) {
      expect(parseConfig({ ...base, AUTH_MODE: v } as unknown as Env).fatal, JSON.stringify(v)).not.toBe("");
    }
    for (const mode of ["", " none ", "NONE", "access", " Access "]) {
      expect(parseConfig({ ...base, AUTH_MODE: mode, ACCESS_TEAM_DOMAIN: "a", ACCESS_AUD: "b" } as unknown as Env).fatal, mode).toBe("");
    }
    expect(parseConfig(base).fatal).toBe("");
  });

  it("takes LIMITS and AI_MODELS given as JSON variables (objects), and reports other value types", () => {
    const c = parseConfig({
      ...base,
      LIMITS: { ai_per_day: 5, max_sites: 3 },
      AI_MODELS: { mini: { provider: "workers-ai", model: "@cf/x/y", kind: "chat" } },
      FILE_INLINE_MAX: 1000,
      ALLOW_ANONYMOUS_DEPLOY: true,
      TOKEN_TTL_HOURS: 2,
    } as unknown as Env);
    expect(c.limits.ai_per_day).toBe(5);
    expect(c.limits.max_sites).toBe(3);
    expect(Object.keys(c.ai.models)).toEqual(["mini"]);
    expect(c.ai.defaultChat).toBe("mini");
    expect(c.fileInlineMax).toBe(1000);
    expect(c.allowAnonymousDeploy).toBe(true);
    expect(c.tokenTTLSeconds).toBe(7200);
    expect(c.problems).toEqual([]);
    const bad = parseConfig({ ...base, LIMITS: 5, AI_MODELS: ["x"], SERVER_NAME: { a: 1 } } as unknown as Env);
    expect(bad.limits).toEqual(DEFAULT_LIMITS);
    const problems = bad.problems.join("\n");
    expect(problems).toContain("LIMITS");
    expect(problems).toContain("AI_MODELS must be a string or a JSON object (got an array)");
    expect(problems).toContain("SERVER_NAME must be a string (got a object)");
    expect(bad.serverName).toBe("Backyard");
    // LIMITS keys are own keys of the defaults, never members of Object.prototype.
    const proto = parseConfig({ ...base, LIMITS: '{"constructor": 5, "toString": 1}' } as unknown as Env);
    expect(proto.problems.join()).toContain('unknown key "constructor"');
    expect(Object.hasOwn(proto.limits, "constructor")).toBe(false);
  });

  it("normalizes PUBLIC_URL like the Go server's public URL", () => {
    const pub = (v: string) => parseConfig({ ...base, PUBLIC_URL: v } as unknown as Env);
    // scheme://host[:port], lowercased; an explicit port is kept.
    expect(pub(" HTTPS://Hub.Acme.COM:443/ ").publicURL).toBe("https://hub.acme.com:443");
    expect(pub("http://hub.local:8787").publicURL).toBe("http://hub.local:8787");
    for (const bad of ["https://user@hub.acme.com", "https://hub.acme.com?x=1", "https://hub.acme.com#f", "https://hub.acme.com/backyard", "hub.acme.com", "ftp://hub"]) {
      const c = pub(bad);
      expect(c.publicURL, bad).toBe("");
      expect(c.problems.join(), bad).toContain("PUBLIC_URL must be an absolute origin");
    }
  });
});

describe("AI_ENABLED (SPEC §16)", () => {
  const base = { BACKYARD_SECRET: "x".repeat(32) } as unknown as Env;
  const cfg = (vars: Record<string, unknown>) => parseConfig({ ...base, ...vars } as unknown as Env);

  it("is on by default and parsed like the other booleans", () => {
    expect(parseConfig(base).ai.enabled).toBe(true);
    for (const v of [undefined, "", "true", "1", "yes", "on", " TRUE ", true, 1]) expect(cfg({ AI_ENABLED: v }).ai.enabled, String(v)).toBe(true);
    // A JSON boolean in wrangler.jsonc or the dashboard arrives parsed.
    for (const v of ["false", "0", "no", "off", "ture", false, 0]) expect(cfg({ AI_ENABLED: v }).ai.enabled, String(v)).toBe(false);
    expect(cfg({ AI_ENABLED: "false" }).problems).toEqual([]);
  });
});

describe("Claude connector variables (MCP_*, SPEC §16)", () => {
  const base = { BACKYARD_SECRET: "x".repeat(32) } as unknown as Env;
  const cfg = (vars: Record<string, unknown>) => parseConfig({ ...base, ...vars } as unknown as Env);

  it("is off by default, with Claude's clients and callbacks", () => {
    const c = parseConfig(base);
    expect(c.mcp).toEqual({ enabled: false, clients: DEFAULT_MCP_CLIENTS, redirectURIs: DEFAULT_MCP_REDIRECT_URIS, problems: [] });
    expect(DEFAULT_MCP_CLIENTS).toEqual(["https://claude.ai/oauth/mcp-oauth-client-metadata", "https://claude.ai/oauth/claude-code-client-metadata"]);
    expect(DEFAULT_MCP_REDIRECT_URIS).toEqual(["https://claude.ai/api/mcp/auth_callback", "https://claude.com/api/mcp/auth_callback"]);
    expect(c.limits.mcp_publish_bytes).toBe(2097152);
    expect(c.limits.mcp_calls_per_day).toBe(5000);
    expect(cfg({ LIMITS: '{"mcp_publish_bytes": 1048576, "mcp_calls_per_day": 0}' }).limits).toMatchObject({ mcp_publish_bytes: 1048576, mcp_calls_per_day: 0 });
  });

  it("parses MCP_ENABLED like the other booleans", () => {
    for (const v of ["true", "1", "yes", "on", " TRUE ", true, 1]) expect(cfg({ MCP_ENABLED: v }).mcp.enabled, String(v)).toBe(true);
    for (const v of ["", "false", "0", "no", "off", "ture", false, undefined]) expect(cfg({ MCP_ENABLED: v }).mcp.enabled, String(v)).toBe(false);
  });

  it("splits the lists on commas and white space; set but empty means none", () => {
    const c = cfg({
      MCP_ENABLED: "true",
      MCP_CLIENTS: " https://a.example/meta,https://b.example/m\nhttps://a.example/meta ",
      MCP_REDIRECT_URIS: "https://a.example/cb  https://b.example/cb,",
    });
    expect(c.mcp.clients).toEqual(["https://a.example/meta", "https://b.example/m"]);
    expect(c.mcp.redirectURIs).toEqual(["https://a.example/cb", "https://b.example/cb"]);
    expect(c.mcp.enabled).toBe(true);
    for (const v of ["", " ", ",", " , "]) {
      const e = cfg({ MCP_ENABLED: "true", MCP_CLIENTS: v, MCP_REDIRECT_URIS: v });
      expect(e.mcp, JSON.stringify(v)).toEqual({ enabled: true, clients: [], redirectURIs: [], problems: [] });
    }
    // A JSON array of strings (a wrangler.jsonc array) works too; any other value turns the connector off.
    expect(cfg({ MCP_CLIENTS: ["https://a.example/meta", "https://b.example/m"] }).mcp.clients).toEqual(["https://a.example/meta", "https://b.example/m"]);
    const bad = cfg({ MCP_ENABLED: "true", MCP_CLIENTS: { a: 1 } });
    expect(bad.mcp.enabled).toBe(false);
    expect(bad.mcp.clients).toEqual([]);
    expect(bad.problems.join("\n")).toContain("MCP_CLIENTS must be a string or an array of strings (got a object)");
    expect(bad.mcp.problems).toEqual(["MCP_CLIENTS is not a string; the Claude connector is off"]);
    expect(bad.fatal).toBe("");
  });

  it("turns the connector off for invalid entries, without failing closed", () => {
    const off = "; the Claude connector is off";
    const clientMsg = (e: string) => `MCP_CLIENTS: ${JSON.stringify(e)} is not a client metadata URL (an https URL with a path, without query, fragment or user info, at most 512 bytes)${off}`;
    for (const e of [
      "https://claude.ai",
      "https://claude.ai/",
      "http://claude.ai/x",
      "https://1.2.3.4/x",
      "https://[::1]/x",
      "https://claude.ai/a/../b",
      "https://claude.ai/./b",
      "https://claude.ai/%2E%2e/b",
      "https://claude.ai/x?y=1",
      "https://claude.ai/x#f",
      "https://u@claude.ai/x",
      "https://claude.ai\\x/y",
      `https://claude.ai/${"x".repeat(500)}`,
      "claude.ai/x",
    ]) {
      const c = cfg({ MCP_ENABLED: "true", MCP_CLIENTS: e });
      expect(c.mcp.enabled, e).toBe(false);
      expect(c.mcp.problems, e).toEqual([clientMsg(e)]);
      expect(c.problems, e).toContain(clientMsg(e));
      expect(c.fatal, e).toBe("");
    }
    const redirectMsg = (e: string) =>
      `MCP_REDIRECT_URIS: ${JSON.stringify(e)} is not an https redirect URI without fragment or user info (at most 512 bytes; loopback redirect URIs are always allowed and are not listed)${off}`;
    for (const e of ["http://localhost/x", "https://claude.ai/cb#f", "https://u@claude.ai/cb", "cursor://cb", `https://claude.ai/${"x".repeat(500)}`, "https://"]) {
      const c = cfg({ MCP_ENABLED: "true", MCP_REDIRECT_URIS: e });
      expect(c.mcp.enabled, e).toBe(false);
      expect(c.mcp.problems, e).toEqual([redirectMsg(e)]);
    }
    // Rule 3: not this Backyard or one of its sites.
    for (const [vars, e] of [
      [{ BASE_DOMAIN: "backyard.acme.com" }, "https://backyard.acme.com/cb"],
      [{ BASE_DOMAIN: "backyard.acme.com" }, "https://lunch.backyard.acme.com/cb"],
      [{ PUBLIC_URL: "https://hub.acme.com" }, "https://HUB.acme.com/cb"],
    ] as const) {
      const c = cfg({ MCP_ENABLED: "true", MCP_REDIRECT_URIS: e, ...vars });
      expect(c.mcp.problems, e).toEqual([`MCP_REDIRECT_URIS: ${JSON.stringify(e)} points at this Backyard or one of its sites${off}`]);
    }
    expect(cfg({ MCP_ENABLED: "true", BASE_DOMAIN: "backyard.acme.com", MCP_REDIRECT_URIS: "https://acme.com/cb" }).mcp.enabled).toBe(true);
    // Problems are reported even while the connector is off.
    expect(cfg({ MCP_CLIENTS: "https://claude.ai" }).mcp.problems).toHaveLength(1);
    // Valid entries (the defaults included) are fine.
    for (const e of [...DEFAULT_MCP_CLIENTS, "https://Example.COM/a/b.json", "https://example.com:8443/m"]) expect(validMCPClientURL(e), e).toBe(true);
    expect(mcpRedirectURIHost("https://CLAUDE.ai:443/api/mcp/auth_callback")).toBe("claude.ai");
  });

  it("needs an https public URL (http only on loopback hosts)", () => {
    const c = cfg({ MCP_ENABLED: "true", PUBLIC_URL: "http://backyard.acme.com" });
    expect(c.mcp.enabled).toBe(false);
    expect(c.mcp.problems).toEqual([
      "MCP_ENABLED needs an https hub URL (PUBLIC_URL is http://backyard.acme.com; plain http only on localhost, *.localhost or loopback addresses); the Claude connector is off",
    ]);
    for (const u of ["http://127.0.0.1:8787", "http://localhost:8787", "http://hub.localhost", "http://[::1]:8787", "https://backyard.acme.com"]) {
      expect(cfg({ MCP_ENABLED: "true", PUBLIC_URL: u }).mcp.enabled, u).toBe(true);
    }
  });

  it("knows loopback hosts", () => {
    for (const h of ["localhost", "LOCALHOST", "a.localhost", "127.0.0.1", "127.1.2.3", "[::1]", "::1", "localhost."]) expect(isLoopbackHost(h), h).toBe(true);
    for (const h of ["localhost.example", "128.0.0.1", "127.0.0.256", "[::2]", "example.com", "", "0.0.0.0"]) expect(isLoopbackHost(h), h).toBe(false);
  });
});

describe("misc", () => {
  it("normalizes base64 without decoding it", () => {
    const std = btoa("hello world!?");
    expect(normalizeBase64(std)).toBe(std); // already canonical: the same string
    expect(normalizeBase64("aGVsbG8")).toBe("aGVsbG8=");
    expect(normalizeBase64("aGVs bG8=\n")).toBe("aGVsbG8=");
    expect(normalizeBase64("-_8")).toBe("+/8=");
    expect(normalizeBase64("")).toBe("");
    for (const bad of ["!!!", "a", "aGVsbG8=x", "data:image/png;base64,AAAA", "a=b="]) expect(normalizeBase64(bad), bad).toBeNull();
    for (const n of [0, 1, 2, 3, 4, 5, 100]) {
      const b = base64Encode(new Uint8Array(n).map((_, i) => i * 7));
      expect(base64Size(b), String(n)).toBe(n);
      expect(base64Decode(b)).toEqual(new Uint8Array(n).map((_, i) => i * 7));
    }
    expect(base64Head(base64Encode(new Uint8Array(200).map((_, i) => i)), 64)).toEqual(new Uint8Array(64).map((_, i) => i));
    // Large inputs (chunked encoding) round-trip.
    const big = new Uint8Array(100_000).map((_, i) => (i * 31 + (i >> 8)) & 0xff);
    expect(base64Decode(base64Encode(big))).toEqual(big);
  });

  it("lists the cookie paths that match a request path (RFC 6265 path-match)", () => {
    expect(cookiePathPrefixes("/")).toEqual(["/"]);
    expect(cookiePathPrefixes("/__backyard/api/deploy/me")).toEqual([
      "/",
      "/__backyard",
      "/__backyard/",
      "/__backyard/api",
      "/__backyard/api/",
      "/__backyard/api/deploy",
      "/__backyard/api/deploy/",
      "/__backyard/api/deploy/me",
    ]);
    expect(cookiePathPrefixes("/a/").length).toBe(3);
    expect(cookiePathPrefixes("/" + "x/".repeat(20)).length).toBe(17);
  });

  it("decodes base64 variants", () => {
    expect(b64urlDecode("AQID")).toEqual(new Uint8Array([1, 2, 3]));
    expect(b64urlDecode("A+B")).toBeNull();
    expect(decodeAnyBase64("AQID")).toEqual(new Uint8Array([1, 2, 3]));
    expect(decodeAnyBase64("AQ-_")).toEqual(new Uint8Array([1, 15, 191]));
    expect(decodeAnyBase64("!!")).toBeNull();
  });

  it("parses server-sent events", async () => {
    const enc = new TextEncoder();
    const chunks = ["data: a\n", "\ndata: b\r\n", "data: c\r\n\r\n: comment\n\nevent: x\ndata: {\"y\":1}", "\n\n"];
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        for (const ch of chunks) c.enqueue(enc.encode(ch));
        c.close();
      },
    });
    const events: { event: string; data: string }[] = [];
    await readSSE(body, (e) => {
      events.push(e);
    });
    expect(events).toEqual([
      { event: "message", data: "a" },
      { event: "message", data: "b\nc" },
      { event: "x", data: '{"y":1}' },
    ]);
  });

  it("extracts text from Workers AI result shapes", () => {
    expect(extractText({ response: "a" })).toBe("a");
    expect(extractText({ choices: [{ message: { content: "b" } }] })).toBe("b");
    expect(extractText({ output: [{ type: "reasoning" }, { type: "message", content: [{ type: "output_text", text: "c" }] }] })).toBe("c");
    expect(extractText(null)).toBe("");
  });

  it("computes the time to the UTC midnight reset", () => {
    expect(secondsToMidnight(Date.UTC(2026, 0, 1, 23, 59, 0))).toBe(60);
  });
});

describe("document values (docstore canonical form)", () => {
  /** levels nested containers (alternating arrays and objects) around "leaf". */
  const nest = (levels: number): unknown => {
    let v: unknown = "leaf";
    for (let i = 0; i < levels; i++) v = i % 2 === 0 ? [v] : { k: v };
    return v;
  };
  const err = (f: () => unknown): HttpError => {
    try {
      f();
    } catch (e) {
      expect(e).toBeInstanceOf(HttpError);
      return e as HttpError;
    }
    throw new Error("expected a 400");
  };

  it("allows exactly MAX_NESTING_DEPTH levels (the body is level 1)", () => {
    expect(MAX_NESTING_DEPTH).toBe(100);
    const ok = { d: nest(MAX_NESTING_DEPTH - 1), n: 1 };
    expect(canonicalDoc(ok)).toEqual(ok);
    const e = err(() => canonicalDoc({ d: nest(MAX_NESTING_DEPTH), n: 1 }));
    expect(e.status).toBe(400);
    expect(e.code).toBe("bad_request");
    expect(e.message).toBe('field "d": nested deeper than 100 levels');
    expect(canonicalDoc({ x: "[".repeat(1500) })).toEqual({ x: "[".repeat(1500) });
  });

  it("rejects U+0000 in strings and keys at any depth", () => {
    expect(err(() => canonicalDoc({ a: "x\u0000y" })).message).toBe('field "a": strings must not contain the NUL character (U+0000)');
    expect(err(() => canonicalDoc({ a: { b: ["ok", "\u0000"] } })).message).toContain('field "a"');
    expect(err(() => canonicalDoc({ a: { "k\u0000": 1 } })).message).toContain("NUL");
    expect(err(() => canonicalDoc({ "k\u0000": 1 })).message).toBe('field name "k\\u0000": strings must not contain the NUL character (U+0000)');
  });

  it("normalizes numbers and strings like the Go backends", () => {
    expect(err(() => canonicalDoc({ n: Infinity })).message).toBe('field "n": number Infinity is not valid JSON');
    expect(err(() => canonicalDoc({ n: [NaN] })).message).toContain("not valid JSON");
    const out = canonicalDoc({ z: -0, s: "a\ud800b", "\udc00": [undefined, 1], u: undefined, id: "x", _meta: { version: 9 } });
    expect(Object.is(out.z, 0)).toBe(true);
    expect(out.s).toBe("a\ufffdb");
    expect(out["\ufffd"]).toEqual([null, 1]);
    expect(Object.keys(out)).toEqual(["z", "s", "\ufffd"]);
    // "__proto__" is data, not the prototype.
    const proto = canonicalDoc(JSON.parse('{"__proto__": {"x": 1}}') as Record<string, unknown>);
    expect(Object.keys(proto)).toEqual(["__proto__"]);
    expect(Object.getPrototypeOf(proto)).toBe(Object.prototype);
  });

  it("canonicalizes filter operands (PrepareQuery)", () => {
    const where = (value: unknown, op = "==") => normalizeQuery({ where: [{ field: "s", op, value }] }, 1000).where[0]!.value;
    expect(err(() => where("a\u0000")).message).toBe('filter on "s": strings must not contain the NUL character (U+0000)');
    expect(err(() => where(["ok", "\u0000"], "in")).message).toContain("NUL");
    expect(err(() => where(Infinity)).message).toBe('filter on "s": number Infinity is not valid JSON');
    expect(err(() => where([1, -Infinity], "not-in")).message).toContain("not valid JSON");
    expect(Object.is(where(-0), 0)).toBe(true);
    expect(where("a\udfff")).toBe("a\ufffd");
    expect(where([-0, "x"], "in")).toEqual([0, "x"]);
    // Normalizing never changes the caller's query.
    const raw = { where: [{ field: "s", op: "in", value: [-0] }] };
    normalizeQuery(raw, 1000);
    expect(Object.is((raw.where[0]!.value as number[])[0], -0)).toBe(true);
  });
});

describe("agent docs", () => {
  const HUB = "https://hub.acme.com";

  it("localizes documents like skills.Localize", () => {
    const md = [
      `Hub: ${HUB_URL_PLACEHOLDER}/docs and ${HUB_URL_PLACEHOLDER}.`,
      "Site: https://demo.backyard.example.com/ and https://<site>.backyard.example.com/x, wildcard *.backyard.example.com.",
      "Kept: https://backyard.example.com.evil.org/ and https://x.backyard.example.com-y/.",
    ].join("\n");
    expect(localize(md, HUB + "/")).toBe(
      [
        `Hub: ${HUB}/docs and ${HUB}.`,
        "Site: https://demo.hub.acme.com/ and https://<site>.hub.acme.com/x, wildcard *.hub.acme.com.",
        "Kept: https://backyard.example.com.evil.org/ and https://x.backyard.example.com-y/.",
      ].join("\n"),
    );
    expect(localize(md, "")).toBe(md);
    // No site subdomains for a hub with a path or an IP address.
    expect(localize("https://demo.backyard.example.com/", "https://acme.com/backyard")).toBe("https://demo.backyard.example.com/");
    expect(localize(`${HUB_URL_PLACEHOLDER}/ https://demo.backyard.example.com/`, "http://127.0.0.1:8787")).toBe(
      "http://127.0.0.1:8787/ https://demo.backyard.example.com/",
    );
    expect(localize("https://demo.backyard.example.com/", "http://hub.local:8787")).toBe("http://demo.hub.local:8787/");
    expect(baseURL("  https://user:pw@hub.acme.com///?q=1#frag  ")).toBe(HUB);
    expect(baseURL("")).toBe("");
  });

  it("matches skills.Localize byte for byte, including the hub URL's spelling", () => {
    // [document, public URL, skills.Localize output] computed by the Go
    // implementation. Go keeps the host's case and an explicit default port
    // (WHATWG URL would lowercase and drop them), drops user info, a query
    // and a fragment, and derives site hosts only for a non-IP host.
    const cases: [string, string, string][] = [
      [
        "See https://backyard.example.com/s/demo/ or https://demo.backyard.example.com/ (*.backyard.example.com).",
        "HTTPS://Hub.Acme.COM:443/",
        "See https://Hub.Acme.COM:443/s/demo/ or https://demo.Hub.Acme.COM:443/ (*.Hub.Acme.COM:443).",
      ],
      [
        "https://<site>.backyard.example.com/x and https://backyard.example.com",
        "https://u:p@hub.acme.com:8443/?q=1#f",
        "https://<site>.hub.acme.com:8443/x and https://hub.acme.com:8443",
      ],
      [
        "https://demo.backyard.example.com/ https://backyard.example.com/docs",
        "https://10.0.0.1:8443",
        "https://demo.backyard.example.com/ https://10.0.0.1:8443/docs",
      ],
      [
        "https://demo.backyard.example.com/ https://backyard.example.com/docs",
        "https://01.2.3.4",
        "https://demo.01.2.3.4/ https://01.2.3.4/docs",
      ],
      [
        "https://demo.backyard.example.com/ https://backyard.example.com/docs",
        "http://[::1]:8787",
        "https://demo.backyard.example.com/ http://[::1]:8787/docs",
      ],
      [
        "a.backyard.example.com.b.backyard.example.com c.backyard.example.com.evil.org https://backyard.example.com.evil",
        "https://hub.acme.com",
        "a.backyard.example.com.b.hub.acme.com c.backyard.example.com.evil.org https://backyard.example.com.evil",
      ],
      [
        "x.backyard.example.com-y.backyard.example.com <a<b>.backyard.example.com <Site>.backyard.example.com",
        "http://hub.local:8787",
        "x.backyard.example.com-y.hub.local:8787 <a<b>.hub.local:8787 <Site>.backyard.example.com",
      ],
    ];
    for (const [md, url, want] of cases) expect(localize(md, url), `${md} @ ${url}`).toBe(want);
    expect(baseURL("HTTPS://Hub.Acme.COM:443/")).toBe("https://Hub.Acme.COM:443");
  });

  it("serves SKILL.md and references by name, with an optional references/ prefix", () => {
    expect(docMarkdown("skill", HUB)).toBe(localize(SKILL_MD, HUB));
    expect(docMarkdown("SKILL", HUB)).toBe(localize(SKILL_MD, HUB));
    expect(docMarkdown("references/skill", HUB)).toBe("");
    for (const name of Object.keys(REFERENCES)) {
      const want = localize(REFERENCES[name]!, HUB);
      expect(docMarkdown(name, HUB), name).toBe(want);
      expect(docMarkdown(`references/${name}`, HUB), name).toBe(want);
      expect(docMarkdown(name.toUpperCase(), HUB), name).toBe(want);
      expect(docMarkdown(name, ""), name).toBe(REFERENCES[name]);
      if (REFERENCES[name]!.includes(HUB_URL_PLACEHOLDER + "/")) expect(want).not.toContain(HUB_URL_PLACEHOLDER + "/");
    }
    for (const bad of ["", "nope", "../api", "references/../api", "references/references/api", "api.md", "a/b", "constructor", "__proto__", "toString"]) {
      expect(docMarkdown(bad, HUB), bad).toBe("");
    }
  });
});

describe("AI provider messages (Go's safeUpstreamMessage and retryHint)", () => {
  it("sanitizes and bounds provider messages", () => {
    expect(safeUpstreamMessage("see https://x.internal:8443/v1?k=1 key sk-proj-abcdefghijklmnop Bearer abcdefgh12345678", 400)).toBe(
      "see [url] key [redacted] [redacted]",
    );
    expect(safeUpstreamMessage("a\u0000b‮c\n\n d e", 0)).toBe("a b c d e");
    // A leading BOM is not white space for Go (unicode.IsSpace) and survives a cut.
    const long = "﻿" + "é".repeat(400);
    const out = safeUpstreamMessage(long, 500);
    expect(out.startsWith("﻿é")).toBe(true);
    expect(out.endsWith("…")).toBe(true);
    expect(new TextEncoder().encode(out).length).toBe(299 + 3);
    expect(safeUpstreamMessage("\udc00x", 0)).toBe("�x");
    expect(safeUpstreamMessage("", 400)).toBe("bad request");
    expect(safeUpstreamMessage("  ", 529)).toBe("request failed");
  });

  it("parses retry hints", () => {
    expect(retryHint("Please try again in 1.5s.")).toBe(1.5);
    expect(retryHint("Please try again in 350ms.")).toBe(0.35);
    expect(retryHint("Please retry after 6 seconds.")).toBe(6);
    expect(retryHint("try again in 2 minutes")).toBe(120);
    expect(retryHint("retry after 3")).toBe(3);
    expect(retryHint("please wait")).toBe(0);
  });
});
