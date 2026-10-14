// Hub helpers: site names, ignore rules (parity with internal/deploy), path
// validation, SHA-256, file collection, the deploy protocol and CLI commands.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash, webcrypto } from "node:crypto";
import { loadHubScript, plain } from "./helpers.mjs";

const hub = loadHubScript("hub.js", { crypto: webcrypto }).BackyardHub;
const hubNoCrypto = loadHubScript("hub.js").BackyardHub;

test("validateSiteName", () => {
  const good = ["a", "lunch-vote", "a1", "x".repeat(63), "0-9"];
  for (const n of good) assert.equal(hub.validateSiteName(n), "", n);
  const bad = ["", "A", "Lunch", "-a", "a-", "a_b", "a.b", "x".repeat(64), "www", "s", "docs", "über"];
  for (const n of bad) assert.notEqual(hub.validateSiteName(n), "", n);
  assert.equal(hub.validateSiteName("www", []), "", "custom reserved list");
});

test("suggestSiteName", () => {
  const cases = [
    ["Lunch Vote", "lunch-vote"],
    ["my_site.v2", "my-site-v2"],
    ["Café Menü", "cafe-menu"],
    ["--x--", "x"],
    ["docs", "docs-site"],
    ["日本", ""],
    ["", ""],
    ["a".repeat(80), "a".repeat(63)],
  ];
  for (const [input, want] of cases) assert.equal(hub.suggestSiteName(input), want, input);
});

// Same vectors as internal/deploy TestMatcher (keep in sync).
const matcherCases = [
  ["no patterns", [], "a.txt", false, false],
  ["exact name", ["a.txt"], "a.txt", false, true],
  ["name at depth", ["a.txt"], "x/y/a.txt", false, true],
  ["glob star", ["*.log"], "logs/debug.log", false, true],
  ["glob no match", ["*.log"], "debug.txt", false, false],
  ["question mark", ["?.js"], "a.js", false, true],
  ["question mark long", ["?.js"], "ab.js", false, false],
  ["character class", ["[ab].txt"], "b.txt", false, true],
  ["negated class", ["[!ab].txt"], "c.txt", false, true],
  ["negated class excludes", ["[!ab].txt"], "a.txt", false, false],
  ["caret class", ["[^ab].txt"], "c.txt", false, true],
  ["escaped bracket bang", ["\\[!a].txt"], "[!a].txt", false, true],
  ["star does not cross /", ["a*b"], "a/b", false, false],
  ["comment", ["# a.txt"], "# a.txt", false, false],
  ["escaped hash", ["\\#a.txt"], "#a.txt", false, true],
  ["blank", ["", "   "], "a", false, false],
  ["trailing spaces trimmed", ["a.txt   "], "a.txt", false, true],
  ["escaped trailing space", ["a\\ "], "a ", false, true],
  ["crlf", ["a.txt\r"], "a.txt", false, true],
  ["negation", ["*.log", "!keep.log"], "keep.log", false, false],
  ["negation others", ["*.log", "!keep.log"], "drop.log", false, true],
  ["last match wins", ["!a", "a"], "a", false, true],
  ["escaped bang", ["\\!a"], "!a", false, true],
  ["cannot reinclude under excluded dir", ["dir/", "!dir/keep"], "dir/keep", false, true],
  ["reinclude with dir/**", ["dir/**", "!dir/keep"], "dir/keep", false, false],
  ["dir/** excludes contents", ["dir/**"], "dir/x/y", false, true],
  ["dir/** keeps dir itself", ["dir/**"], "dir", true, false],
  ["dir only matches dir", ["build/"], "build", true, true],
  ["dir only not file", ["build/"], "build", false, false],
  ["dir only contents", ["build/"], "build/out.js", false, true],
  ["dir only nested", ["build/"], "src/build/x", false, true],
  ["dir pattern without slash", ["node_modules"], "a/node_modules/x/y.js", false, true],
  ["leading slash anchors", ["/a.txt"], "a.txt", false, true],
  ["leading slash not deep", ["/a.txt"], "x/a.txt", false, false],
  ["inner slash anchors", ["doc/*.txt"], "doc/a.txt", false, true],
  ["inner slash not nested", ["doc/*.txt"], "doc/sub/a.txt", false, false],
  ["inner slash not deep", ["doc/*.txt"], "x/doc/a.txt", false, false],
  ["leading **", ["**/foo"], "foo", false, true],
  ["leading ** deep", ["**/foo"], "a/b/foo", false, true],
  ["leading ** with path", ["**/foo/bar"], "x/foo/bar", false, true],
  ["middle **", ["a/**/b"], "a/b", false, true],
  ["middle ** deep", ["a/**/b"], "a/x/y/b", false, true],
  ["middle ** wrong root", ["a/**/b"], "z/a/x/b", false, false],
  ["trailing ** matches files", ["a/**"], "a/b.txt", false, true],
  ["lone **", ["**"], "x/y", false, true],
  ["embedded ** acts like *", ["a**b"], "axxb", false, true],
  ["bad glob", ["[", "a"], "[", false, false],
  ["bad glob keeps others", ["[", "a"], "a", false, true],
  ["only slash", ["/"], "a", false, false],
  ["leading slash in path", ["a"], "/a", false, true],
  ["empty path", ["*"], "", true, false],
  // JS-specific: characters that are special in RegExps.
  ["dash in name", ["my-file.txt"], "x/my-file.txt", false, true],
  ["regexp specials are literal", ["a+(b).txt"], "a+(b).txt", false, true],
  ["range class", ["[a-c]x"], "bx", false, true],
  ["range class excludes", ["[a-c]x"], "dx", false, false],
  ["unicode ?", ["?.txt"], "ü.txt", false, true],
];
for (const [name, patterns, path, isDir, want] of matcherCases) {
  test("ignore matcher: " + name, () => {
    assert.equal(hub.compileIgnore(patterns).ignored(path, isDir), want);
  });
}

test("default ignore rules (same vectors as internal/deploy TestDefaultIgnore)", () => {
  const m = hub.compileIgnore(hub.DEFAULT_IGNORE);
  const cases = [
    [".git", true, true],
    [".git/config", false, true],
    [".backyard.json", false, true],
    [".backyardignore", false, true],
    ["node_modules", true, true],
    ["web/node_modules/x.js", false, true],
    [".claude/settings.json", false, true],
    [".cursor", true, true],
    [".github/workflows/ci.yml", false, true],
    ["CLAUDE.md", false, true],
    ["docs/AGENTS.md", false, true],
    [".DS_Store", false, true],
    ["img/.DS_Store", false, true],
    [".env", false, true],
    ["a/.hidden/b.txt", false, true],
    [".well-known", true, false],
    [".well-known/security.txt", false, false],
    [".well-known/.secret", false, true],
    ["index.html", false, false],
    ["README.md", false, false],
    ["claude.md", false, false],
    ["a.b/c.d", false, false],
  ];
  for (const [path, isDir, want] of cases) assert.equal(m.ignored(path, isDir), want, path);
});

test("validatePath (same vectors as internal/deploy TestValidatePath)", () => {
  const good = ["index.html", "a/b/c.txt", "with space.txt", "ü/日本.html", ".well-known/x", "a..b", "...", "a".repeat(1024), "__backyardx/a", "x/__backyard/y"];
  for (const p of good) assert.equal(hub.validatePath(p), "", p);
  const bad = ["", "/a", "a/", "a//b", "./a", "a/.", "../a", "a/../b", "..", "__backyard", "__backyard/x", "a\\b", "a\x00b", "a\nb", "a\x7fb", "a\uD800b", "a".repeat(1025), "é".repeat(513)];
  for (const p of bad) assert.notEqual(hub.validatePath(p), "", JSON.stringify(p));
});

test("sha256Hex matches node:crypto across block boundaries", () => {
  const lengths = [0, 1, 3, 55, 56, 57, 63, 64, 65, 119, 120, 127, 128, 1000, 100_003];
  for (const n of lengths) {
    const buf = new Uint8Array(n);
    for (let i = 0; i < n; i++) buf[i] = (i * 31 + 7) & 0xff;
    assert.equal(hub.sha256Hex(buf), createHash("sha256").update(buf).digest("hex"), "length " + n);
  }
  assert.equal(hub.sha256Hex(new TextEncoder().encode("abc").buffer), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
});

test("hashBlob uses Web Crypto when available and pure JS otherwise", async () => {
  const blob = new Blob(["hello backyard"]);
  const want = createHash("sha256").update("hello backyard").digest("hex");
  assert.equal(await hub.hashBlob(blob), want);
  assert.equal(await hubNoCrypto.hashBlob(blob), want);
});

test("formatBytes", () => {
  const cases = [
    [0, "0 B"],
    [999, "999 B"],
    [1000, "1 KB"],
    [1536, "1.54 KB"],
    [12_345, "12.3 KB"],
    [123_456, "123 KB"],
    [5_000_000, "5 MB"],
    [104_857_600, "105 MB"],
    [2.5e12, "2.5 TB"],
  ];
  for (const [n, want] of cases) assert.equal(hub.formatBytes(n), want, String(n));
  assert.equal(hub.formatBytes(-1), "");
});

test("relativeTime", () => {
  const now = Date.parse("2026-06-01T12:00:00Z");
  const cases = [
    ["2026-06-01T11:59:30Z", "just now"],
    ["2026-06-01T11:58:00Z", "2 minutes ago"],
    ["2026-06-01T09:00:00Z", "3 hours ago"],
    ["2026-05-31T12:00:00Z", "yesterday"],
    ["2026-05-25T12:00:00Z", "last week"],
    ["2026-03-01T12:00:00Z", "3 months ago"],
    ["2024-06-01T12:00:00Z", "2 years ago"],
    ["2026-06-01T15:00:00Z", "in 3 hours"],
    ["not a date", ""],
  ];
  for (const [t, want] of cases) assert.equal(hub.relativeTime(t, now, "en"), want, t);
});

test("siteURLFor host and path mode", () => {
  const host = { public_url: "https://backyard.acme.com", base_domain: "backyard.acme.com", url_mode: "host" };
  assert.equal(hub.siteURLFor(host, "poll"), "https://poll.backyard.acme.com/");
  assert.equal(hub.siteURLFor({ public_url: "http://localhost:8080", base_domain: "localhost", url_mode: "host" }, "x"), "http://x.localhost:8080/");
  assert.equal(hub.siteURLFor({ public_url: "https://by.acme.workers.dev/", url_mode: "path" }, "poll"), "https://by.acme.workers.dev/s/poll/");
  assert.equal(hub.siteURLFor(null, "poll", "https://hub.example"), "https://poll.hub.example/");
  assert.equal(hub.siteURLFor(host, ""), "");
});

test("cliCommands: --gateway only when the gateway differs; auth-mode hints", () => {
  const base = { public_url: "https://backyard.acme.com", gateway_url: "https://backyard.acme.com/__backyard/api/deploy", auth: { mode: "oidc" } };
  let c = hub.cliCommands(base);
  assert.equal(c.login, "backyard login --server https://backyard.acme.com");
  assert.match(c.loginNote, /browser/);
  assert.equal(c.install, "curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh");
  assert.equal(c.installGo, "go install github.com/thasami/backyard/cmd/backyard@latest");
  assert.equal(c.deploy, "backyard init my-site && cd my-site && backyard deploy");
  assert.equal(c.skill, "backyard skill install --global");

  c = hub.cliCommands({ ...base, gateway_url: "https://abc.lambda-url.us-east-1.on.aws/", auth: { mode: "alb" } });
  assert.equal(c.login, "backyard login --server https://backyard.acme.com --gateway https://abc.lambda-url.us-east-1.on.aws");

  c = hub.cliCommands({ ...base, auth: { mode: "oidc", device: { issuer: "x", client_id: "y", scopes: [] } } });
  assert.match(c.loginNote, /--device/);

  c = hub.cliCommands({ public_url: "https://by.acme.workers.dev", gateway_url: "https://by.acme.workers.dev/__backyard/api/deploy", auth: { mode: "none", anonymous_deploy: false } });
  assert.equal(c.login, "backyard login --server https://by.acme.workers.dev");
  assert.match(c.loginNote, /deploy token/);

  c = hub.cliCommands({ public_url: "http://localhost:8080", auth: { mode: "none", anonymous_deploy: true } });
  assert.match(c.loginNote, /without credentials/);

  c = hub.cliCommands(null, "https://fallback.example");
  assert.equal(c.login, "backyard login --server https://fallback.example");
});

test("tokenFromHash", () => {
  assert.equal(hub.tokenFromHash("#token=abc.def"), "abc.def");
  assert.equal(hub.tokenFromHash("#x=1&token=a%2Bb"), "a+b");
  assert.equal(hub.tokenFromHash("#tokens=nope"), "");
  assert.equal(hub.tokenFromHash(""), "");
  assert.equal(hub.tokenFromHash("#token=%E0%A4%A"), "");
});

test("userLabel", () => {
  assert.equal(hub.userLabel({ id: "alice@acme.com", name: "Alice Smith" }), "Alice Smith (alice@acme.com)");
  assert.equal(hub.userLabel({ id: "token:ci", name: "ci" }), "ci (token:ci)");
  assert.equal(hub.userLabel({ id: "bob@acme.com", name: "bob@acme.com" }), "bob@acme.com");
  assert.equal(hub.userLabel({ id: "anon:0123", name: "Guest 0123", anonymous: true }), "an anonymous guest (anon:0123)");
  assert.equal(hub.userLabel({ name: 42 }), "an unknown account");
  assert.equal(hub.userLabel(null), "an unknown account");
});

test("handedTokenPrompt names whose token a link carries and whom it replaces", () => {
  const alice = { id: "alice@acme.com", name: "Alice", anonymous: false };
  const mallory = { id: "token:mallory", name: "mallory", anonymous: false };
  let p = hub.handedTokenPrompt(mallory, alice);
  assert.equal(p.title, "Use the deploy token of mallory (token:mallory)?");
  assert.equal(p.body, "The link you opened carries the deploy token of mallory (token:mallory). If you use it, deploys and deletes from this page run as mallory (token:mallory), not as you (Alice (alice@acme.com)).");
  p = hub.handedTokenPrompt({ ...alice }, alice);
  assert.match(p.body, /a token of your own account/);
  for (const guest of [null, { id: "anon:1", name: "Guest 1", anonymous: true }]) {
    p = hub.handedTokenPrompt(mallory, guest);
    assert.equal(p.body, "The link you opened carries the deploy token of mallory (token:mallory). If you use it, deploys and deletes from this page run as mallory (token:mallory).");
  }
  p = hub.handedTokenPrompt(null, alice, "network error");
  assert.equal(p.title, "Use a deploy token from a link?");
  assert.equal(p.body, "The link you opened carries a deploy token, but Backyard could not check whose it is (network error). If you use it, deploys and deletes from this page use that token instead of your own sign-in (Alice (alice@acme.com)).");
});

test("tokenOwner asks the gateway whose token it is (GET G/me with the token)", async () => {
  const calls = [];
  const answer = (status, body) => async (url, init) => {
    calls.push({ url, ...init });
    return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
  };
  const u = await hub.tokenOwner({ gateway: "/__backyard/api/deploy/", token: "m4l", fetch: answer(200, { id: "token:mallory", name: "mallory" }) });
  assert.deepEqual(plain(u), { id: "token:mallory", name: "mallory" });
  assert.deepEqual(plain(calls[0]), {
    url: "/__backyard/api/deploy/me",
    method: "GET",
    headers: { Accept: "application/json", "X-Backyard": "1", Authorization: "Bearer m4l" },
    credentials: "same-origin",
  });
  const rejected = answer(401, { error: { code: "unauthenticated", message: "invalid or expired token" } });
  await assert.rejects(hub.tokenOwner({ gateway: "/g", token: "bogus", fetch: rejected }), (e) => e.status === 401 && /invalid or expired/.test(e.message));
  await assert.rejects(hub.tokenOwner({ gateway: "/g", token: "x", fetch: answer(200, { name: "no id" }) }), (e) => e.code === "bad_response");
  const offline = async () => {
    throw new TypeError("Failed to fetch");
  };
  await assert.rejects(hub.tokenOwner({ gateway: "/g", token: "x", fetch: offline }), (e) => e.code === "network_error");
  // Something that cannot be a bearer token is rejected without asking.
  const n = calls.length;
  for (const bad of ["", "a b", "tök", "a\nb"]) await assert.rejects(hub.tokenOwner({ gateway: "/g", token: bad, fetch: rejected }), (e) => e.status === 401, JSON.stringify(bad));
  assert.equal(calls.length, n);
});

test("safeHref only lets http(s) and same-origin paths through", () => {
  const cases = [
    ["https://a.b/", "https://a.b/"],
    ["http://x/s/y/", "http://x/s/y/"],
    ["/s/poll/", "/s/poll/"],
    ["javascript:alert(1)", "#"],
    [" JAVASCRIPT:alert(1)", "#"],
    ["data:text/html,x", "#"],
    ["//evil.example/", "#"],
    ["/\\evil.example/", "#"],
    ["", "#"],
    [undefined, "#"],
  ];
  for (const [input, want] of cases) assert.equal(hub.safeHref(input), want, String(input));
  assert.equal(hub.isSafeRedirect("/__backyard/auth/login?rd=x"), true);
  assert.equal(hub.isSafeRedirect("https://idp.example/authorize"), true);
  for (const bad of ["", undefined, "//evil.example", "javascript:alert(1)", "login"]) assert.equal(hub.isSafeRedirect(bad), false, String(bad));
});

test("deploy tokens are remembered by default only in host mode", () => {
  assert.equal(hub.rememberByDefault({ url_mode: "host" }), true);
  assert.equal(hub.rememberByDefault({ url_mode: "path" }), false, "path mode: sites share the hub origin");
  assert.equal(hub.rememberByDefault(null), false, "unknown URL mode: do not persist");
});

test("initialsOf", () => {
  assert.equal(hub.initialsOf("Alice Smith"), "AS");
  assert.equal(hub.initialsOf("bob@acme.com"), "BO");
  assert.equal(hub.initialsOf("Guest 1a2b"), "G1");
  assert.equal(hub.initialsOf(""), "?");
});

test("filterSites: search terms across fields, newest first", () => {
  const sites = [
    { name: "old", title: "Old thing", updated_at: "2026-01-01T00:00:00Z", updated_by: "alice@acme.com" },
    { name: "new", title: "Lunch vote", description: "Where do we eat", updated_at: "2026-03-01T00:00:00Z", updated_by: "bob@acme.com" },
    { name: "mid", updated_at: "2026-02-01T00:00:00Z", owners: ["carol@acme.com"] },
  ];
  assert.deepEqual(plain(hub.filterSites(sites, "").map((s) => s.name)), ["new", "mid", "old"]);
  assert.deepEqual(plain(hub.filterSites(sites, "LUNCH eat").map((s) => s.name)), ["new"]);
  assert.deepEqual(plain(hub.filterSites(sites, "alice").map((s) => s.name)), ["old"]);
  assert.deepEqual(plain(hub.filterSites(sites, "carol").map((s) => s.name)), ["mid"]);
  assert.deepEqual(plain(hub.filterSites(sites, "zzz")), []);
});

// --- file collection --------------------------------------------------------

const file = (text, name = "f") => new File([text], name);

/** Emulates the browser FileSystemEntry API, returning directory listings in small batches. */
function entryTree(spec, name = "", log = []) {
  if (typeof spec === "string" || spec instanceof File) {
    const f = spec instanceof File ? spec : file(spec, name);
    return { isFile: true, isDirectory: false, name, file: (ok) => setTimeout(() => ok(f)) };
  }
  if (spec === null) {
    return {
      isFile: false,
      isDirectory: true,
      name,
      createReader() {
        throw new Error("ignored directory " + name + " must not be read");
      },
    };
  }
  const children = Object.entries(spec).map(([n, s]) => entryTree(s, n, log));
  return {
    isFile: false,
    isDirectory: true,
    name,
    createReader() {
      let pos = 0;
      return {
        readEntries(ok) {
          log.push(name);
          const batch = children.slice(pos, pos + 2); // browsers return entries in batches
          pos += batch.length;
          setTimeout(() => ok(batch));
        },
      };
    },
  };
}

test("collectTree applies default rules, .backyardignore and never reads ignored directories", async () => {
  const root = entryTree({
    "index.html": "<title>x</title>",
    "app.js": "js",
    ".backyardignore": "# comment\n*.log\nsecret/\n!keep.log\n",
    ".git": null,
    node_modules: null,
    secret: null,
    "debug.log": "x",
    "keep.log": "x",
    ".env": "SECRET=1",
    ".well-known": { "security.txt": "x" },
    "CLAUDE.md": "agent",
    assets: { "a.png": "png", deep: { "b.css": "css", ".DS_Store": "x" } },
  });
  const res = await hub.collectTree(root);
  assert.deepEqual(plain(res.files.map((f) => f.path)), [".well-known/security.txt", "app.js", "assets/a.png", "assets/deep/b.css", "index.html", "keep.log"]);
  assert.deepEqual(plain(res.ignored), [".backyardignore", ".env", ".git/", "CLAUDE.md", "assets/deep/.DS_Store", "debug.log", "node_modules/", "secret/"]);
  assert.equal(await res.files[4].file.text(), "<title>x</title>");
  assert.deepEqual(plain(res.invalid), []);
});

test("collectTree honours .backyard.json dir/ignore and reports undeployable paths", async () => {
  const root = entryTree({
    ".backyard.json": JSON.stringify({ site: "dash", dir: "dist", ignore: ["*.map"] }),
    src: { "main.ts": "ts" },
    dist: {
      "index.html": "x",
      "app.js.map": "map",
      ".backyardignore": "drafts/",
      drafts: null,
      __backyard: { "x.js": "nope" },
    },
  });
  const res = await hub.collectTree(root);
  assert.equal(res.config.site, "dash");
  assert.equal(res.dir, "dist");
  assert.deepEqual(plain(res.files.map((f) => f.path)), ["index.html"]);
  assert.deepEqual(plain(res.ignored), [".backyardignore", "app.js.map", "drafts/"]);
  assert.deepEqual(plain(res.invalid.map((f) => f.path)), ["__backyard/x.js"]);

  await assert.rejects(hub.collectTree(entryTree({ ".backyard.json": '{"dir":"missing"}' })), /no such directory/);
  const bad = await hub.collectTree(entryTree({ ".backyard.json": "{nope", "index.html": "x" }));
  assert.match(bad.warnings[0], /not valid JSON/);
});

test("treeFromPaths builds a tree from <input webkitdirectory> style paths", async () => {
  const root = hub.treeFromPaths([
    { path: "index.html", file: file("i") },
    { path: "css/site.css", file: file("c") },
    { path: "css/vendor/x.css", file: file("v") },
    { path: ".git/HEAD", file: file("h") },
    { path: "node_modules/a/index.js", file: file("n") },
  ]);
  const res = await hub.collectTree(root);
  assert.deepEqual(plain(res.files.map((f) => f.path)), ["css/site.css", "css/vendor/x.css", "index.html"]);
  assert.deepEqual(plain(res.ignored), [".git/", "node_modules/"]);
});

// --- deploy protocol --------------------------------------------------------

function fakeGateway({ unchanged = [], failUploads = {}, createError, commitError } = {}) {
  const calls = [];
  const uploads = [];
  const fetch = async (url, init) => {
    const body = init.body ? JSON.parse(init.body) : undefined;
    calls.push({ url, method: init.method, headers: init.headers, body, credentials: init.credentials });
    if (/\/deploys$/.test(url)) {
      if (createError) return new Response(JSON.stringify({ error: createError.error }), { status: createError.status, headers: { "Content-Type": "application/json" } });
      return Response.json({
        deploy_id: "dep 1",
        uploads: body.files
          .filter((f) => unchanged.indexOf(f.path) < 0)
          .map((f) => ({ path: f.path, url: "/__backyard/api/deploy/uploads/tok-" + f.path, method: "PUT", headers: { "Content-Type": "text/plain", "x-amz-meta-sha256": f.sha256 } })),
        unchanged,
        expires_at: "2026-01-01T00:00:00Z",
      });
    }
    if (/\/commit$/.test(url)) {
      if (commitError) return new Response(JSON.stringify({ error: { code: "bad_request", message: commitError } }), { status: 400 });
      return Response.json({ site: { name: "poll", url: "https://poll.backyard.test/" }, url: "https://poll.backyard.test/", deleted: 2 });
    }
    if (init.method === "DELETE") return new Response(null, { status: 204 });
    return new Response("{}", { status: 404 });
  };
  const upload = async ({ url, method, headers, file: f, onProgress }) => {
    const n = (failUploads[url] = (failUploads[url] || 0) - 1);
    if (n >= 0) {
      const err = new Error("boom");
      err.status = 503;
      throw err;
    }
    onProgress(Math.floor(f.size / 2));
    onProgress(f.size);
    uploads.push({ url, method, headers: { ...headers }, text: await f.text() });
  };
  return { fetch, upload, calls, uploads };
}

async function hashed(files) {
  return Promise.all(files.map(async ([path, text]) => ({ path, size: new Blob([text]).size, sha256: createHash("sha256").update(text).digest("hex"), file: file(text, path) })));
}

test("deploySite: create → uploads with given headers → commit", async () => {
  const gw = fakeGateway({ unchanged: ["app.js"] });
  const files = await hashed([
    ["index.html", "<h1>hi</h1>"],
    ["app.js", "console.log(1)"],
    ["img/a.png", "PNGDATA"],
  ]);
  const events = [];
  const res = await hub.deploySite({ gateway: "/__backyard/api/deploy/", site: "poll", files, token: "", fetch: gw.fetch, upload: gw.upload, onProgress: (p) => events.push(plain(p)) });

  const [create, commit] = gw.calls;
  assert.equal(create.url, "/__backyard/api/deploy/sites/poll/deploys");
  assert.equal(create.method, "POST");
  assert.equal(create.credentials, "same-origin");
  assert.equal(create.headers["X-Backyard"], "1");
  assert.equal(create.headers["Content-Type"], "application/json");
  assert.equal(create.headers.Authorization, undefined);
  assert.deepEqual(create.body.files, files.map((f) => ({ path: f.path, size: f.size, sha256: f.sha256 })));
  assert.equal(create.body.message, "Deployed from the hub");

  assert.equal(commit.url, "/__backyard/api/deploy/sites/poll/deploys/dep%201/commit");
  assert.deepEqual(commit.body, { files: create.body.files });
  assert.equal(commit.headers["X-Backyard"], "1");

  assert.equal(gw.uploads.length, 2);
  const up = gw.uploads.find((u) => u.url.endsWith("tok-index.html"));
  assert.equal(up.method, "PUT");
  assert.deepEqual(up.headers, { "Content-Type": "text/plain", "x-amz-meta-sha256": files[0].sha256 }, "exactly the given headers");
  assert.equal(up.text, "<h1>hi</h1>");

  assert.deepEqual(plain(res), { site: { name: "poll", url: "https://poll.backyard.test/" }, url: "https://poll.backyard.test/", uploaded: 2, unchanged: 1, deleted: 2, bytes: 11 + 7 });
  assert.equal(events[0].phase, "plan");
  const last = events.filter((e) => e.phase === "upload").pop();
  assert.deepEqual(last, { phase: "upload", loaded: 18, total: 18, files: 2, fileCount: 2 });
  assert.deepEqual(events.slice(-2).map((e) => e.phase), ["commit", "done"]);
});

test("deploySite sends the deploy token as a bearer token (Cloudflare edition / static tokens)", async () => {
  const gw = fakeGateway();
  await hub.deploySite({ gateway: "/g", site: "x", files: await hashed([["index.html", "x"]]), token: "s3cret", fetch: gw.fetch, upload: gw.upload });
  for (const c of gw.calls) assert.equal(c.headers.Authorization, "Bearer s3cret");
});

test("deploySite retries failed uploads (5xx) and surfaces API errors", async () => {
  const gw = fakeGateway({ failUploads: { "/__backyard/api/deploy/uploads/tok-index.html": 2 } });
  const files = await hashed([["index.html", "x"]]);
  const res = await hub.deploySite({ gateway: "/g", site: "x", files, fetch: gw.fetch, upload: gw.upload, retryDelay: 1 });
  assert.equal(res.uploaded, 1);

  const forever = fakeGateway({ failUploads: { "/__backyard/api/deploy/uploads/tok-index.html": 10 } });
  await assert.rejects(hub.deploySite({ gateway: "/g", site: "x", files, fetch: forever.fetch, upload: forever.upload, retryDelay: 1 }), (e) => e.status === 503);

  const unauth = fakeGateway({ createError: { status: 401, error: { code: "unauthenticated", message: "Authentication required.", login_url: "/__backyard/auth/login" } } });
  const err = await hub.deploySite({ gateway: "/g", site: "x", files, fetch: unauth.fetch, upload: unauth.upload }).catch((e) => e);
  assert.equal(err.status, 401);
  assert.equal(err.code, "unauthenticated");
  assert.equal(err.loginUrl, "/__backyard/auth/login");
  assert.equal(err.message, "Authentication required.");

  const quota = fakeGateway({ createError: { status: 413, error: { code: "too_large", message: "site exceeds 500 MB" } } });
  await assert.rejects(hub.deploySite({ gateway: "/g", site: "x", files, fetch: quota.fetch, upload: quota.upload }), /site exceeds 500 MB/);

  const commitFail = fakeGateway({ commitError: "missing file index.html" });
  await assert.rejects(hub.deploySite({ gateway: "/g", site: "x", files, fetch: commitFail.fetch, upload: commitFail.upload }), /missing file index.html/);
});

test("hashFiles hashes in input order within a memory budget", async () => {
  const sizes = [10, 60, 5, 70, 20, 20, 1];
  const files = sizes.map((n, i) => ({ path: "f" + i, file: file("x".repeat(n), "f" + i) }));
  let inFlight = 0;
  let peak = 0;
  const progress = [];
  const out = await hub.hashFiles(files, {
    budget: 64,
    concurrency: 4,
    onProgress: (d, n) => progress.push(d + "/" + n),
    hash: async (blob) => {
      inFlight += Math.min(blob.size, 64);
      peak = Math.max(peak, inFlight);
      await new Promise((r) => setTimeout(r, 2));
      inFlight -= Math.min(blob.size, 64);
      return "h" + blob.size;
    },
  });
  assert.deepEqual(plain(out.map((f) => [f.path, f.size, f.sha256])), sizes.map((n, i) => ["f" + i, n, "h" + n]));
  assert.ok(peak <= 64, "peak bytes in flight " + peak);
  assert.equal(progress.length, sizes.length);
  assert.equal(progress[progress.length - 1], "7/7");
  // The default hash is the real SHA-256.
  const [real] = await hub.hashFiles([{ path: "a", file: file("abc", "a") }]);
  assert.equal(real.sha256, createHash("sha256").update("abc").digest("hex"));
});

test("deploySite retries create and commit on availability errors, not on 500", async () => {
  const files = await hashed([["index.html", "x"]]);
  const flaky = (failures) => {
    const gw = fakeGateway();
    let n = 0;
    return {
      gw,
      fetch: async (url, init) => {
        const f = failures[n++];
        if (f === "offline") throw new TypeError("Failed to fetch");
        if (f) return new Response(JSON.stringify({ error: { code: "upstream_error", message: "busy" } }), { status: f });
        return gw.fetch(url, init);
      },
    };
  };
  const a = flaky(["offline", 503]); // create fails twice, then succeeds
  const res = await hub.deploySite({ gateway: "/g", site: "x", files, fetch: a.fetch, upload: a.gw.upload, retryDelay: 1 });
  assert.equal(res.uploaded, 1);
  const b = flaky([undefined, 502]); // commit fails once
  assert.equal((await hub.deploySite({ gateway: "/g", site: "x", files, fetch: b.fetch, upload: b.gw.upload, retryDelay: 1 })).uploaded, 1);
  const c = flaky([500, undefined]);
  await assert.rejects(hub.deploySite({ gateway: "/g", site: "x", files, fetch: c.fetch, upload: c.gw.upload, retryDelay: 1 }), (e) => e.status === 500);
});

test("deploySite rejects unexpected server answers", async () => {
  const files = await hashed([["index.html", "x"]]);
  const html = async () => new Response("<html>login</html>", { status: 200 });
  await assert.rejects(hub.deploySite({ gateway: "/g", site: "x", files, fetch: html, upload: async () => {} }), (e) => e.code === "bad_response");
  const unknown = async () => Response.json({ deploy_id: "d", uploads: [{ path: "other.txt", url: "/u", method: "PUT" }] });
  await assert.rejects(hub.deploySite({ gateway: "/g", site: "x", files, fetch: unknown, upload: async () => {} }), /unknown file/);
  const offline = async () => {
    throw new TypeError("Failed to fetch");
  };
  await assert.rejects(hub.deploySite({ gateway: "/g", site: "x", files, fetch: offline, upload: async () => {}, retryDelay: 1 }), (e) => e.code === "network_error");
});

test("deleteSite", async () => {
  const gw = fakeGateway();
  await hub.deleteSite({ gateway: "/__backyard/api/deploy", site: "old-site", token: "t", fetch: gw.fetch });
  assert.deepEqual(plain(gw.calls[0]), {
    url: "/__backyard/api/deploy/sites/old-site",
    method: "DELETE",
    headers: { Accept: "application/json", "X-Backyard": "1", Authorization: "Bearer t" },
    credentials: "same-origin",
  });
});

// --- presigned uploads (XMLHttpRequest PUT) ---------------------------------

/** A scriptable XMLHttpRequest double; the test plays the storage service. */
function fakeXHR() {
  const made = [];
  class XHR {
    constructor() {
      this.headers = [];
      this.upload = {};
      this.status = 0;
      this.responseText = "";
      this.responseHeaders = {};
      made.push(this);
    }
    open(method, url) {
      this.method = method;
      this.url = url;
    }
    setRequestHeader(k, v) {
      if (/^content-length$/i.test(k)) throw new Error("Refused to set unsafe header");
      this.headers.push([k, v]);
    }
    getResponseHeader(k) {
      return this.responseHeaders[k.toLowerCase()] ?? null;
    }
    send(body) {
      this.body = body;
    }
    abort() {
      this.aborted = true;
    }
    // --- storage side
    respond(status, text = "", headers = {}) {
      this.status = status;
      this.responseText = text;
      this.responseHeaders = headers;
      this.upload.onprogress && this.upload.onprogress({ loaded: this.body.size });
      this.onload && this.onload();
    }
    fail() {
      this.onerror && this.onerror();
    }
  }
  return { XHR, made };
}

test("uploads PUT exactly the presigned headers and the raw bytes (no browser-added Content-Type)", async () => {
  const { XHR, made } = fakeXHR();
  const upload = hub.createUploader({ XMLHttpRequest: XHR, origin: "https://backyard.acme.com" });
  const f = new File(["<h1>hi</h1>"], "index.html", { type: "text/html" });
  const cases = [
    // Azure SAS: x-ms-blob-type is required and must reach the service verbatim.
    { "x-ms-blob-type": "BlockBlob", "Content-Type": "text/html; charset=utf-8" },
    // S3 SigV4: signed x-amz-* headers (values untouched), Host/Content-Length left to the browser.
    { "Content-Type": "text/html", "X-Amz-Checksum-Sha256": "q1w2e3==", "x-amz-meta-sha256": "ab12", Host: "bucket.s3.amazonaws.com", "Content-Length": "11" },
    // GCS V4 and the gateway's own upload endpoint.
    { "Content-Type": "text/html" },
    {},
  ];
  for (const headers of cases) {
    const progress = [];
    const p = upload({ url: "https://storage.example.com/b/index.html?sig=1", method: "PUT", headers, file: f, onProgress: (n) => progress.push(n) });
    const x = made[made.length - 1];
    assert.equal(x.method, "PUT");
    assert.equal(x.url, "https://storage.example.com/b/index.html?sig=1");
    const expected = Object.entries(headers).filter(([k]) => !/^(host|content-length)$/i.test(k));
    assert.deepEqual(x.headers, expected, JSON.stringify(headers));
    assert.equal(x.withCredentials, undefined, "no cookies to storage");
    assert.equal(x.body.type, "", "a type-less body: the browser adds no Content-Type of its own");
    assert.equal(await x.body.text(), "<h1>hi</h1>");
    x.respond(201);
    await p;
    assert.deepEqual(progress, [11]);
  }
  assert.deepEqual(plain(hub.uploadHeaders({ "content-type": "a", "Content-Type": "b", "x-n": 5, "x-arr": ["a", "b"], "x-null": null, "bad name": "x", "Sec-Fetch-Mode": "cors" })), [
    ["Content-Type", "b"],
    ["x-n", "5"],
    ["x-arr", "a,b"],
  ]);
});

test("a presigned PUT blocked by CORS (no HTTP response) explains the bucket CORS fix", async () => {
  const { XHR, made } = fakeXHR();
  const upload = hub.createUploader({ XMLHttpRequest: XHR, origin: "https://backyard.acme.com" });
  const f = new File(["x"], "app.js");
  let p = upload({ url: "https://acme-sites.s3.eu-west-1.amazonaws.com/sites/x/app.js?X-Amz-Signature=1", method: "PUT", headers: { "Content-Type": "text/javascript", "x-amz-checksum-sha256": "abc" }, file: f });
  made[made.length - 1].fail();
  let err = await p.catch((e) => e);
  assert.equal(err.name, "HubError");
  assert.equal(err.code, "cors");
  assert.equal(err.retryable, true, "a real network blip is retried");
  assert.match(err.message, /^Uploading app\.js to acme-sites\.s3\.eu-west-1\.amazonaws\.com failed: network or CORS error\./);
  assert.match(err.message, /bucket CORS must allow PUT from https:\/\/backyard\.acme\.com with the headers Content-Type, x-amz-checksum-sha256/);

  // Some browsers report a blocked response as a load with status 0.
  p = upload({ url: "https://acct.blob.core.windows.net/c/app.js?sv=1", method: "PUT", headers: { "x-ms-blob-type": "BlockBlob" }, file: f });
  made[made.length - 1].respond(0);
  err = await p.catch((e) => e);
  assert.equal(err.code, "cors");
  assert.match(err.message, /bucket CORS must allow PUT from https:\/\/backyard\.acme\.com with the headers x-ms-blob-type/);

  // Same-origin uploads (the gateway's own endpoint) are plain network errors.
  p = upload({ url: "/__backyard/api/deploy/uploads/tok", method: "PUT", headers: {}, file: f });
  made[made.length - 1].fail();
  err = await p.catch((e) => e);
  assert.equal(err.code, "network_error");
  assert.equal(err.message, "Network error while uploading app.js");
});

test("upload HTTP errors carry the storage service's reason (JSON or S3/GCS/Azure XML)", async () => {
  const { XHR, made } = fakeXHR();
  const upload = hub.createUploader({ XMLHttpRequest: XHR, origin: "https://backyard.acme.com" });
  const f = new File(["x"], "a.css");
  const run = async (status, text, headers) => {
    const p = upload({ url: "https://s.example/a.css", method: "PUT", headers: {}, file: f });
    made[made.length - 1].respond(status, text, headers);
    return p.catch((e) => e);
  };
  let err = await run(403, '<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>');
  assert.equal(err.message, "Uploading a.css failed: Request has expired (AccessDenied) (HTTP 403)");
  assert.equal(err.status, 403);
  err = await run(400, JSON.stringify({ error: { code: "bad_request", message: "size or sha256 mismatch" } }));
  assert.equal(err.message, "Uploading a.css failed: size or sha256 mismatch (HTTP 400)");
  err = await run(503, "", { "retry-after": "7" });
  assert.equal(err.message, "Uploading a.css failed (HTTP 503)");
  assert.equal(err.retryAfter, 7);
  assert.equal(hub.uploadErrorDetail("<Error><Code>AuthenticationFailed</Code></Error>"), "AuthenticationFailed");
});

test("deploySite + createUploader: a CORS failure surfaces after the retries", async () => {
  const { XHR, made } = fakeXHR();
  const upload = hub.createUploader({ XMLHttpRequest: XHR, origin: "https://backyard.acme.com" });
  const gw = fakeGateway();
  const files = await hashed([["index.html", "x"]]);
  const fetchPresigned = async (url, init) => {
    const res = await gw.fetch(url, init);
    if (!/\/deploys$/.test(url)) return res;
    const plan = await res.json();
    for (const u of plan.uploads) u.url = "https://bucket.example.com/" + u.path;
    return Response.json(plan);
  };
  const p = hub.deploySite({ gateway: "/g", site: "x", files, fetch: fetchPresigned, upload, retryDelay: 1 });
  for (let i = 0; i < 3; i++) {
    while (made.length <= i) await new Promise((r) => setTimeout(r, 1));
    made[i].fail();
  }
  const err = await p.catch((e) => e);
  assert.equal(made.length, 3, "retried");
  assert.equal(err.code, "cors");
  assert.match(err.message, /bucket CORS must allow PUT from https:\/\/backyard\.acme\.com/);
});

test("expired sessions: login responses are recognised and the login URL returns to the page", () => {
  const h = (ct) => ({ get: (k) => (k.toLowerCase() === "content-type" ? ct : null) });
  assert.equal(hub.isLoginResponse({ type: "opaqueredirect", status: 0, ok: false, headers: h("") }), true);
  assert.equal(hub.isLoginResponse({ type: "basic", status: 200, ok: true, headers: h("text/html; charset=utf-8") }), true);
  assert.equal(hub.isLoginResponse({ type: "basic", status: 200, ok: true, headers: h("application/json") }), false);
  assert.equal(hub.isLoginResponse({ type: "basic", status: 401, ok: false, headers: h("application/json") }), false, "JSON 401s carry login_url");
  assert.equal(hub.loginURLFor("https://backyard.acme.com/?q=a b#x"), "/__backyard/auth/login?rd=https%3A%2F%2Fbackyard.acme.com%2F%3Fq%3Da%20b%23x");
});
