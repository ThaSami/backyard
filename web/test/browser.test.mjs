// Real-browser smoke tests (Chromium via playwright-core) for the SDK and the
// hub UI against web/test/stubserver.mjs. Skipped unless playwright-core and
// a Chromium build are available:
//
//   npm i --prefix /tmp/pw playwright-core@1.56.1
//   PLAYWRIGHT_CORE=/tmp/pw/node_modules/playwright-core/index.mjs \
//   PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers node --test web/test/browser.test.mjs
//
// (or set BACKYARD_TEST_CHROMIUM=/path/to/chrome).
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createHash } from "node:crypto";
import { inflateSync } from "node:zlib";
import { startStub } from "./stubserver.mjs";

let pw = null;
let skipReason = "";
try {
  pw = await import(process.env.PLAYWRIGHT_CORE || "playwright-core");
} catch {
  skipReason = "playwright-core is not installed (set PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs)";
}

let browser = null;
before(async () => {
  if (!pw) return;
  try {
    const chromium = pw.chromium || (pw.default && pw.default.chromium);
    browser = await chromium.launch({ executablePath: process.env.BACKYARD_TEST_CHROMIUM || undefined });
  } catch (err) {
    skipReason = "Chromium is not available: " + String(err.message || err).split("\n")[0];
  }
});
after(async () => {
  if (browser) await browser.close();
});

const SITE_PAGE = `<!doctype html><html><head><meta charset="utf-8"><title>demo</title>
<script src="/__backyard/backyard.js"></script></head><body><h1>demo</h1></body></html>`;

/** Opens a page and collects console errors and uncaught exceptions. */
async function open(context, url) {
  const page = await context.newPage();
  const problems = [];
  page.on("pageerror", (e) => problems.push("pageerror: " + e.message));
  page.on("console", (m) => {
    // Network errors of requests a test makes on purpose (404, 501) are logged by
    // the browser itself; only script errors count.
    if (m.type() === "error" && !/^Failed to load resource: the server responded with a status of (404|501)\b/.test(m.text())) {
      problems.push("console: " + m.text());
    }
  });
  await page.goto(url);
  return { page, problems };
}

test("SDK in a real browser: context, db, realtime, files, AI streaming, rooms, reconnect, ESM", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ pages: { "index.html": SITE_PAGE, "": SITE_PAGE } });
  const context = await browser.newContext();
  try {
    const { page, problems } = await open(context, stub.url + "/s/demo/index.html");

    const site = await page.evaluate(() => ({ ...backyard.site }));
    assert.deepEqual(site, { name: "demo", url: stub.url + "/s/demo/", apiBase: stub.url + "/s/demo/__backyard", mode: "path" });

    const r = await page.evaluate(async () => {
      const out = {};
      out.me = (await backyard.me()).id;
      const posts = backyard.db.collection("posts");
      const events = [];
      posts.subscribe({ onCreate: (d) => events.push("create:" + d.id), onUpdate: (d) => events.push("update:" + d.id + ":" + d.votes), onDelete: (id) => events.push("delete:" + id) });
      const snapshots = [];
      const stop = posts.watch((docs) => snapshots.push(docs.map((d) => d.id + "=" + d.votes).join(",")), posts.orderBy("votes", "desc"));
      await new Promise((resolve) => backyard.socket.on("open", resolve));
      await new Promise((r) => setTimeout(r, 100));
      const a = await posts.create({ title: "A", votes: 1 }, { id: "a" });
      await posts.create({ title: "B", votes: 5 }, { id: "b" });
      await posts.update("a", { votes: 9 });
      out.got = (await posts.get("a")).votes;
      out.missing = await posts.get("nope");
      out.top = (await posts.where("votes", ">=", 5).orderBy("votes", "desc").get()).map((d) => d.id);
      out.first = (await posts.orderBy("votes").first()).id;
      await posts.delete("b");
      await new Promise((r) => setTimeout(r, 300));
      stop();
      out.events = events;
      out.snapshots = snapshots;
      out.createdMeta = a._meta.version;
      out.collections = await backyard.db.collections();
      const f = await backyard.files.upload(new Blob(["hello file"], { type: "text/plain" }), { name: "hello.txt" });
      out.file = f.name;
      const err = await backyard.warehouse.query("SELECT 1").catch((e) => e);
      out.warehouse = [err.name, err.code, err.status, err instanceof backyard.BackyardError];
      const chunks = [];
      const s = backyard.ai.stream("hi", { onText: (t) => chunks.push(t) });
      const iterated = [];
      for await (const c of s) iterated.push(c.text);
      const final = await s.done;
      out.ai = { chunks, iterated, content: final.content, usage: final.usage.output_tokens };
      out.chat = (await backyard.ai.chat("ping")).content;
      const img = await backyard.ai.image("a dot", { n: 2 });
      out.image = { id: img.id, url: img.url, type: img.content_type, model: img.model, n: img.images.length, ids: img.images.map((i) => i.id) };
      out.embed = [await backyard.ai.embed("abc"), await backyard.ai.embed(["a", "bb"])];
      out.kinds = (await backyard.ai.models()).models.map((m) => m.kind);
      const files = await backyard.files.list({ limit: 2 });
      out.files = { n: files.length, hasMore: files.hasMore, keys: Object.keys(files) };
      const mod = await import("/__backyard/backyard.mjs");
      out.esmShared = mod.default === window.backyard && mod.db === backyard.db;
      return out;
    });
    assert.equal(r.me, "alice@acme.com");
    assert.equal(r.got, 9);
    assert.equal(r.missing, null);
    assert.deepEqual(r.top, ["a", "b"]);
    assert.equal(r.first, "b");
    assert.deepEqual(r.events, ["create:a", "create:b", "update:a:9", "delete:b"]);
    assert.equal(r.snapshots[r.snapshots.length - 1], "a=9", "live query converged: " + JSON.stringify(r.snapshots));
    assert.equal(r.createdMeta, 1);
    assert.deepEqual(r.collections, ["posts"]);
    assert.equal(r.file, "hello.txt");
    assert.deepEqual(r.warehouse, ["BackyardError", "unsupported", 501, true]);
    assert.deepEqual(r.ai, { chunks: ["Hello", " from", " the", " stub", " ✓"], iterated: ["Hello", " from", " the", " stub", " ✓"], content: "Hello from the stub ✓", usage: 5 });
    assert.equal(r.chat, "echo: ping");
    assert.match(r.image.id, /^img\d+$/);
    assert.equal(r.image.url, `/s/demo/__backyard/files/${r.image.id}/image-1.png`);
    assert.deepEqual([r.image.type, r.image.model, r.image.n, r.image.ids[0]], ["image/png", "image", 2, r.image.id]);
    assert.deepEqual(r.embed, [[3, 0.5], [[1, 0.5], [2, 0.5]]]);
    assert.deepEqual(r.kinds, ["chat", "image", "embedding"]);
    assert.deepEqual(r.files, { n: 2, hasMore: true, keys: ["0", "1"] });
    assert.equal(r.esmShared, true);

    // CSRF header on writes, multipart upload.
    const create = stub.state.requests.find((q) => q.method === "POST" && q.path === "/db/posts");
    assert.equal(create.headers["x-backyard"], "1");
    const upload = stub.state.requests.find((q) => q.path === "/files");
    assert.match(upload.headers["content-type"], /^multipart\/form-data; boundary=/);

    // Rooms with presence between two users, then a network drop.
    await context.addCookies([{ name: "user", value: "bob@acme.com", url: stub.url }]);
    const second = await open(context, stub.url + "/s/demo/");
    await page.evaluate(() => {
      window.room = backyard.socket.room("lobby", { state: { color: "red" } });
      window.log = [];
      room.on("members", (ms) => log.push("members:" + ms.map((m) => m.user.id).join(",")));
      room.on("message", (data, from) => log.push("msg:" + data.text + ":" + from.user.id));
      room.on("join", (m) => log.push("join:" + m.user.id));
      room.on("update", (m) => log.push("update:" + m.user.id + ":" + m.state.color));
    });
    await page.waitForFunction(() => window.room && window.room.self !== null);
    await second.page.evaluate(async () => {
      window.room = backyard.socket.room("lobby", { state: { color: "blue" } });
      await new Promise((resolve) => room.on("members", resolve));
      room.send({ text: "hi alice" });
      room.updateState({ color: "green" });
    });
    await page.waitForFunction(() => window.log.some((l) => l.startsWith("update:")));
    const log = await page.evaluate(() => window.log);
    assert.deepEqual(log, ["members:", "join:bob@acme.com", "members:bob@acme.com", "msg:hi alice:bob@acme.com", "update:bob@acme.com:green", "members:bob@acme.com"]);
    const bobView = await second.page.evaluate(() => ({ self: room.self.user.id, members: room.members.map((m) => m.user.id + ":" + m.state.color) }));
    assert.deepEqual(bobView, { self: "bob@acme.com", members: ["alice@acme.com:red"] });
    // Errors about this room's frames come back with the frame id and reach room.on("error").
    const roomErrors = await second.page.evaluate(async () => {
      const errs = [];
      room.on("error", (e) => errs.push(e.code + ":" + e.message));
      room.send("hello?", { to: "c_ffffffffff" });
      room.setState({ blob: "x".repeat(2000) });
      room.send({ big: "y".repeat(5000) });
      await new Promise((r) => setTimeout(r, 300));
      return errs;
    });
    assert.deepEqual(roomErrors, ['not_found:no member "c_ffffffffff" in room "lobby"', "too_large:presence state exceeds 1024 bytes", "too_large:message too large"]);
    await second.page.evaluate(() => room.setState({ color: "green" }));

    // Network drop: both pages reconnect, resubscribe and rejoin with their latest state.
    await page.evaluate(() => {
      window.sockLog = [];
      backyard.socket.on("close", () => sockLog.push("close"));
      backyard.socket.on("open", () => sockLog.push("open"));
      room.setState({ color: "purple" });
    });
    stub.killSockets();
    await page.waitForFunction(() => window.sockLog.join() === "close,open", null, { timeout: 15000 });
    await second.page.waitForFunction(() => backyard.socket.connected && room.members.some((m) => m.state.color === "purple"), null, { timeout: 15000 });
    const after = await page.evaluate(async () => {
      const seen = [];
      backyard.db.collection("posts").subscribe({ onCreate: (d) => seen.push(d.id) });
      await new Promise((r) => setTimeout(r, 100));
      await backyard.db.collection("posts").create({ votes: 0 }, { id: "after-reconnect" });
      await new Promise((r) => setTimeout(r, 200));
      return { seen, connected: backyard.socket.connected, members: room.members.map((m) => m.user.id) };
    });
    assert.deepEqual(after, { seen: ["after-reconnect"], connected: true, members: ["bob@acme.com"] });
    const rejoins = stub.state.requests.filter((q) => q.ws && q.ws.t === "join" && q.ws.state && q.ws.state.color === "purple");
    assert.equal(rejoins.length, 1, "rejoined with the latest presence state");
    assert.deepEqual(problems.concat(second.problems), []);
  } finally {
    await context.close();
    await stub.close();
  }
});

test("SDK in a real browser: db events without their document are re-read, in order", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ pages: { "index.html": SITE_PAGE, "": SITE_PAGE }, maxEventDocBytes: 1024 });
  const context = await browser.newContext();
  try {
    const { page, problems } = await open(context, stub.url + "/s/demo/index.html");
    const r = await page.evaluate(async () => {
      const posts = backyard.db.collection("posts");
      const events = [];
      const changes = [];
      posts.subscribe({
        onCreate: (d) => events.push("create:" + d.id + ":" + d.body.length),
        onUpdate: (d) => events.push("update:" + d.id + ":" + d.body.length),
        onDelete: (id) => events.push("delete:" + id),
        onChange: (e) => changes.push(e.op + ":" + e.id + ":" + (e.doc ? "doc" : "null")),
        onError: (e) => events.push("error:" + e.code),
      });
      await new Promise((resolve) => backyard.socket.on("open", resolve));
      await new Promise((r) => setTimeout(r, 100));
      const until = async (n) => {
        for (let i = 0; i < 100 && events.length < n; i++) await new Promise((r) => setTimeout(r, 20));
      };
      await posts.create({ body: "small" }, { id: "s" });
      await until(1);
      await posts.create({ body: "x".repeat(2000) }, { id: "big" });
      await until(2);
      await posts.update("big", { body: "y".repeat(3000) });
      await until(3);
      await posts.update("s", { body: "smaller" });
      await posts.delete("big");
      await until(5);
      return { events, changes };
    });
    assert.deepEqual(r.events, ["create:s:5", "create:big:2000", "update:big:3000", "update:s:7", "delete:big"]);
    assert.deepEqual(r.changes, ["create:s:doc", "create:big:doc", "update:big:doc", "update:s:doc", "delete:big:null"]);
    const reads = stub.state.requests.filter((q) => q.method === "GET" && q.path.startsWith("/db/posts/")).map((q) => q.path);
    assert.deepEqual(reads, ["/db/posts/big", "/db/posts/big"], "only the documents sent without content are read");
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
  }
});

test("hub: header, CLI commands, site directory, folder deploy with a deploy token, docs", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({
    requireToken: "t0k",
    info: { gateway_url: "https://deploy.example/gw" },
    sites: [
      { name: "lunch-vote", url: "http://x/s/lunch-vote/", title: "Lunch vote", description: "Where do we eat?", updated_at: new Date(Date.now() - 3 * 3600e3).toISOString(), updated_by: "carol@acme.com", owners: [] },
      { name: "okr", url: "http://x/s/okr/", title: "OKRs", updated_at: new Date(Date.now() - 86400e3 * 3).toISOString(), updated_by: "dan@acme.com", owners: [] },
    ],
  });
  const dir = mkdtempSync(join(tmpdir(), "backyard-hub-"));
  const site = join(dir, "My Cool Site");
  mkdirSync(join(site, "css"), { recursive: true });
  mkdirSync(join(site, "node_modules", "x"), { recursive: true });
  mkdirSync(join(site, ".git"), { recursive: true });
  writeFileSync(join(site, "index.html"), "<!doctype html><title>Cool</title><h1>cool</h1>");
  writeFileSync(join(site, "css", "site.css"), "body{color:red}");
  writeFileSync(join(site, "node_modules", "x", "index.js"), "ignored");
  writeFileSync(join(site, ".git", "HEAD"), "ref");
  writeFileSync(join(site, "CLAUDE.md"), "agents");
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  try {
    // The deploy token is handed over in the URL fragment and removed from the URL.
    const { page, problems } = await open(context, stub.url + "/#token=t0k");
    await page.waitForSelector(".site");
    assert.equal(page.url(), stub.url + "/");
    assert.equal(await page.textContent(".brand-name"), "Stub Backyard");
    assert.equal(await page.title(), "Stub Backyard");
    assert.equal(await page.textContent("#user-name"), "Alice");
    assert.equal(await page.isVisible("#logout"), false, "no logout link outside OIDC");
    assert.equal(await page.textContent("#cmd-login"), `backyard login --server ${stub.url} --gateway https://deploy.example/gw`);
    assert.match(await page.textContent("#login-note"), /deploy token/);
    assert.equal(await page.textContent("#cmd-deploy"), "backyard init my-site && cd my-site && backyard deploy");
    // It is used only once the visitor confirms whose token it is.
    await page.waitForSelector("#handoff-dialog[open]");
    assert.equal(await page.textContent("#handoff-title"), "Use the deploy token of deploy (token:deploy)?");
    assert.equal(await page.inputValue("#deploy-token"), "");
    await page.click("#handoff-use");
    assert.equal(await page.textContent("#toast"), "Deploy token set for this visit");
    assert.equal(await page.isVisible("#token-box"), true);
    assert.equal(await page.inputValue("#deploy-token"), "t0k");
    // Path mode: sites share the hub's origin, so the token is not persisted unless asked.
    assert.equal(await page.evaluate(() => localStorage.getItem("backyard.deployToken")), null);
    await page.click("#token-box summary");
    assert.equal(await page.isVisible("#remember-hint"), true);
    assert.equal(await page.isChecked("#remember-token"), false);
    await page.check("#remember-token");
    assert.equal(await page.evaluate(() => localStorage.getItem("backyard.deployToken")), "t0k");

    // Directory: newest first, search, list view.
    assert.deepEqual(await page.$$eval(".site .site-name", (els) => els.map((e) => e.textContent)), ["lunch-vote", "okr"]);
    assert.equal(await page.textContent(".site time"), "3 hours ago");
    assert.equal(await page.textContent('.site[data-site="lunch-vote"] .site-url'), "x/s/lunch-vote");
    await page.fill("#search", "eat");
    assert.deepEqual(await page.$$eval(".site .site-name", (els) => els.map((e) => e.textContent)), ["lunch-vote"]);
    assert.equal(await page.textContent("#site-count"), "(1 of 2)");
    await page.fill("#search", "");
    await page.click("#view-list");
    assert.equal(await page.getAttribute("#site-list", "class"), "site-list as-list");

    // Deploy a folder via the directory picker.
    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    assert.equal(await page.inputValue("#site-name"), "my-cool-site");
    assert.match(await page.textContent("#selection-summary"), /^My Cool Site · 2 files · /);
    assert.equal(await page.textContent("#site-url"), stub.url + "/s/my-cool-site/");
    await page.click("#deploy-btn");
    await page.waitForSelector("#result:not([hidden])");
    assert.equal(await page.textContent("#result-link"), stub.url + "/s/my-cool-site/");
    const create = stub.state.requests.find((q) => q.path === "/deploy/sites/my-cool-site/deploys");
    assert.equal(create.headers["x-backyard"], "1");
    assert.equal(create.headers.authorization, "Bearer t0k");
    const files = JSON.parse(create.body.toString()).files;
    assert.deepEqual(
      files,
      [
        ["css/site.css", "body{color:red}"],
        ["index.html", "<!doctype html><title>Cool</title><h1>cool</h1>"],
      ].map(([path, text]) => ({ path, size: Buffer.byteLength(text), sha256: createHash("sha256").update(text).digest("hex") })),
    );
    assert.equal(stub.state.stored.get("my-cool-site/css/site.css").toString(), "body{color:red}");
    assert.equal(stub.state.deploys[0].committed, true);
    await page.waitForSelector('.site[data-site="my-cool-site"].fresh');

    // Re-deploying unchanged files uploads nothing.
    await page.click("#deploy-btn");
    await page.waitForFunction(() => /0 uploaded · 2 unchanged/.test(document.getElementById("progress-text").textContent));

    // Delete through the confirmation dialog.
    await page.click('button[aria-label="Delete okr"]');
    assert.equal(await page.isDisabled("#delete-submit"), true);
    await page.fill("#delete-confirm", "okr");
    await page.click("#delete-submit");
    await page.waitForFunction(() => !document.querySelector('.site[data-site="okr"]'));

    // Docs: rendered markdown, TOC, client-side navigation between documents.
    const docs = await open(context, stub.url + "/docs");
    await docs.page.waitForSelector("#doc h1");
    assert.equal(await docs.page.textContent("#doc h1"), "Backyard API");
    assert.deepEqual(await docs.page.$$eval("#toc a", (as) => as.map((a) => a.textContent)), ["Database", "collection(name)", "Files"]);
    assert.equal(await docs.page.title(), "Backyard API · Stub Backyard");
    await docs.page.click("text=recipes");
    await docs.page.waitForFunction(() => document.querySelector("#doc h1")?.textContent === "Recipes");
    assert.equal(new URL(docs.page.url()).search, "?doc=recipes");
    assert.equal(await docs.page.getAttribute('#doc-list a[data-doc="recipes"]', "aria-current"), "page");
    assert.equal(await docs.page.isVisible(".code-block .copy, #doc h2"), true);
    assert.deepEqual(problems.concat(docs.problems), []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub: a deploy token from a link is used only after the visitor confirms whose it is (no token fixation)", async (t) => {
  if (!browser) return t.skip(skipReason);
  // Host mode: a confirmed token is remembered by default, so a silently
  // adopted one would stick to this browser.
  const stub = await startStub({
    info: { url_mode: "host" },
    tokens: { m4llory: { id: "token:mallory", name: "mallory", groups: [], anonymous: false, admin: false } },
  });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>V</title>victim" });
  const context = await browser.newContext();
  const stored = (page) => page.evaluate(() => localStorage.getItem("backyard.deployToken"));
  const meChecks = () => stub.state.requests.filter((q) => q.path === "/deploy/me").map((q) => q.headers.authorization);
  const deployAs = async (page) => {
    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    const before = stub.state.deploys.length;
    await page.click("#deploy-btn");
    await page.waitForSelector("#result:not([hidden])");
    const create = stub.state.requests.filter((q) => q.path === "/deploy/sites/guest-site/deploys")[before];
    return create.headers.authorization;
  };
  let visits = 0;
  /** Loads the hub afresh (a hash-only navigation would not reload it). */
  const reopen = async (page, hash) => {
    await page.goto(`${stub.url}/?visit=${++visits}${hash}`);
    assert.equal(new URL(page.url()).hash, "");
  };
  try {
    // Mallory's link: the hub asks the gateway whose token it is and asks the
    // visitor; nothing changes until then (not even in host mode).
    const { page, problems } = await open(context, stub.url + "/#token=m4llory");
    await page.waitForSelector("#handoff-dialog[open]");
    assert.equal(page.url(), stub.url + "/", "the fragment is removed at once");
    assert.equal(await page.textContent("#handoff-title"), "Use the deploy token of mallory (token:mallory)?");
    assert.match(await page.textContent("#handoff-body"), /run as mallory \(token:mallory\), not as you \(Alice \(alice@acme\.com\)\)/);
    assert.deepEqual(meChecks(), ["Bearer m4llory"]);
    assert.equal(await page.evaluate(() => document.activeElement.id), "handoff-ignore", "not using it is the default");
    assert.equal(await page.inputValue("#deploy-token"), "");
    assert.equal(await stored(page), null);

    // Declining keeps the visitor's own identity.
    await page.click("#handoff-ignore");
    assert.equal(await page.isVisible("#handoff-dialog"), false);
    assert.equal(await page.textContent("#toast"), "The deploy token from the link was not used");
    assert.equal(await page.inputValue("#deploy-token"), "");
    assert.equal(await stored(page), null);
    assert.equal(await deployAs(page), undefined, "the deploy runs as the signed-in visitor");
    assert.equal(stub.state.sites.find((s) => s.name === "guest-site").updated_by, "alice@acme.com");

    // Escape declines too.
    await reopen(page, "#token=m4llory");
    await page.waitForSelector("#handoff-dialog[open]");
    await page.keyboard.press("Escape");
    await page.waitForSelector("#handoff-dialog:not([open])", { state: "attached" });
    assert.equal(await page.inputValue("#deploy-token"), "");
    assert.equal(await stored(page), null);

    // An explicit "use" adopts it (and, in host mode, remembers it).
    await reopen(page, "#token=m4llory");
    await page.waitForSelector("#handoff-dialog[open]");
    await page.click("#handoff-use");
    assert.equal(await page.textContent("#toast"), "Deploy token saved in this browser");
    assert.equal(await stored(page), "m4llory");
    assert.equal(await deployAs(page), "Bearer m4llory");

    // The token already in use is not asked about again.
    const checks = meChecks().length;
    await reopen(page, "#token=m4llory");
    await page.waitForSelector(".site");
    await page.waitForLoadState("networkidle");
    assert.equal(await page.isVisible("#handoff-dialog"), false);
    assert.equal(meChecks().length, checks);

    // A token the server rejects is never offered, and the one in use stays.
    await reopen(page, "#token=bogus");
    await page.waitForSelector("#banner:not([hidden])");
    assert.equal(await page.textContent("#banner"), "The deploy token in the link you opened was rejected by the server, so it is not used.");
    assert.equal(await page.isVisible("#handoff-dialog"), false);
    assert.deepEqual(meChecks().slice(-1), ["Bearer bogus"]);
    assert.equal(await stored(page), "m4llory");
    assert.deepEqual(problems.filter((p) => !/status of 401/.test(p)), []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

/** Creates a deployable folder; returns its path (removed by the caller). */
function makeSiteDir(files) {
  const dir = mkdtempSync(join(tmpdir(), "backyard-hub-"));
  const site = join(dir, "guest-site");
  for (const [p, text] of Object.entries(files)) {
    mkdirSync(join(site, p, ".."), { recursive: true });
    writeFileSync(join(site, p), text);
  }
  return { dir, site };
}

test("hub as a guest with a required deploy token (Cloudflare edition, AUTH_MODE=none + DEPLOY_TOKEN)", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({
    guest: true,
    requireToken: "t0k",
    sites: [{ name: "okr", url: "http://x/s/okr/", title: "OKRs", updated_at: new Date().toISOString(), owners: [] }],
  });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>G</title>hi" });
  const context = await browser.newContext();
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector(".site");
    assert.equal(await page.isHidden("#banner"), true, "no error banner when the server info loaded");
    assert.equal(await page.textContent("#user-name"), "Guest 0123");
    assert.equal(await page.isVisible("#logout"), false);
    // A token is needed to deploy: the box is shown and open; guests cannot delete.
    assert.equal(await page.isVisible("#deploy-token"), true);
    assert.match(await page.textContent("#token-hint"), /needs a deploy token for publishing/);
    assert.equal(await page.$('button[aria-label="Delete okr"]'), null);

    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    await page.click("#deploy-btn");
    await page.waitForSelector("#deploy-error:not([hidden])");
    assert.match(await page.textContent("#deploy-error"), /requires a deploy token/);
    assert.equal(await page.evaluate(() => document.activeElement.id), "deploy-token");

    await page.fill("#deploy-token", "t0k");
    await page.waitForSelector('button[aria-label="Delete okr"]');
    await page.click("#deploy-btn");
    await page.waitForSelector("#result:not([hidden])");
    assert.equal(await page.textContent("#result-link"), stub.url + "/s/guest-site/");
    assert.equal(await page.evaluate(() => localStorage.getItem("backyard.deployToken")), null, "path mode: not persisted unless asked");

    await page.click('button[aria-label="Delete okr"]');
    await page.fill("#delete-confirm", "okr");
    await page.click("#delete-submit");
    await page.waitForFunction(() => !document.querySelector('.site[data-site="okr"]'));
    const del = stub.state.requests.find((q) => q.method === "DELETE");
    assert.equal(del.headers.authorization, "Bearer t0k");
    assert.deepEqual(problems.filter((p) => !/status of 401/.test(p)), []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub in playground mode: guests deploy without a token, deleting needs one", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({
    guest: true,
    sites: [{ name: "okr", url: "http://x/s/okr/", title: "OKRs", updated_at: new Date().toISOString(), owners: [] }],
  });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>P</title>hi", "a/b.txt": "b" });
  const context = await browser.newContext();
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector(".site");
    assert.equal(await page.isHidden("#banner"), true);
    // The token box is offered (collapsed) for deletes only.
    assert.equal(await page.isVisible("#token-box"), true);
    assert.equal(await page.isVisible("#deploy-token"), false, "collapsed");
    assert.match(await page.textContent("#token-hint"), /only needed to delete sites/);
    assert.equal(await page.$('button[aria-label="Delete okr"]'), null);
    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    await page.click("#deploy-btn");
    await page.waitForSelector("#result:not([hidden])");
    const create = stub.state.requests.find((q) => q.path === "/deploy/sites/guest-site/deploys");
    assert.equal(create.headers.authorization, undefined);
    assert.equal(stub.state.stored.get("guest-site/a/b.txt").toString(), "b");
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

/** A connector connection as GET /__backyard/api/mcp/grants returns it (SPEC §8.4.7). */
function grant(id, userID, over = {}) {
  return {
    id,
    user: { id: userID, email: userID, name: userID.split("@")[0] },
    client_name: "Claude",
    client_kind: "metadata_document",
    client_host: "claude.ai",
    redirect_host: "claude.ai",
    scope: "mcp offline_access",
    created_at: "2026-10-14T09:30:00Z",
    expires_at: "2026-11-13T09:30:00Z",
    ...over,
  };
}

test("hub: no Claude connector card while /info has no mcp_url", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub();
  const context = await browser.newContext();
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector("#site-list:not([aria-busy])", { state: "attached" });
    await page.waitForLoadState("networkidle");
    assert.equal(await page.isHidden("#connector"), true);
    assert.equal(stub.state.requests.filter((q) => q.path.startsWith("/mcp/")).length, 0, "the grants API is not asked");
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
  }
});

test("hub: the Claude connector card shows the URL, the setup steps and the connections; Disconnect sends DELETE with X-Backyard: 1", async (t) => {
  if (!browser) return t.skip(skipReason);
  const mine = grant("a0000000000000000000000000000001", "alice@acme.com");
  const tool = grant("a0000000000000000000000000000002", "alice@acme.com", { client_name: "My tool", client_kind: "registered", client_host: "", redirect_host: "localhost" });
  const bobs = grant("b0000000000000000000000000000001", "bob@acme.com");
  const stub = await startStub({ requireToken: "t0k", mcp: { grants: [mine, tool, bobs] } });
  const context = await browser.newContext({ permissions: ["clipboard-read", "clipboard-write"] });
  try {
    // A deploy token in use must not be sent to the grants API (it owns no connections).
    await context.addInitScript(() => localStorage.setItem("backyard.deployToken", "t0k"));
    const { page, problems } = await open(context, stub.url + "/");
    // The card starts collapsed: its title and summary only.
    await page.waitForSelector("#connector");
    assert.equal(await page.isVisible("#connector-copy"), false);
    await page.click("#connector summary");
    await page.waitForSelector("#connector-grants li");
    const url = stub.url + "/__backyard/mcp";
    assert.equal(await page.isVisible("#connector"), true);
    assert.equal(await page.textContent("#connector h2"), "Use Backyard in Claude");
    assert.equal(await page.textContent("#connector-url"), url);
    assert.equal(await page.textContent("#connector-cmd"), "claude mcp add --transport http backyard " + url);
    assert.match(
      await page.textContent("#connector .steps"),
      /Customize → Connectors → Add custom connector[\s\S]*Organization settings[\s\S]*claude mcp add --transport http backyard/,
    );
    assert.equal(await page.isHidden("#connector-revoke-user"), true, "the admin form is for admins only");

    await page.click("#connector-copy");
    await page.waitForFunction(() => document.getElementById("toast").textContent === "Copied to clipboard");
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), url);

    // Only the visitor's own connections, each with its trust and return host.
    const items = await page.$$eval("#connector-grants li", (lis) => lis.map((li) => li.innerText.replace(/\s+/g, " ").trim()));
    assert.equal(items.length, 2);
    assert.match(items[0], /^Claude verified by claude\.ai Returns to claude\.ai · connected .+2026 · expires .+2026 Disconnect$/);
    assert.match(items[1], /^My tool unverified Returns to an app on this computer · /);
    assert.deepEqual(
      await page.$$eval("button.connector-revoke", (bs) => bs.map((b) => [b.dataset.id, b.textContent, b.getAttribute("aria-label")])),
      [
        [mine.id, "Disconnect", "Disconnect Claude"],
        [tool.id, "Disconnect", "Disconnect My tool"],
      ],
    );
    const list = stub.state.requests.find((q) => q.path === "/mcp/grants" && q.method === "GET");
    assert.equal(list.headers.authorization, undefined);

    // Disconnect: DELETE with the CSRF header and the visitor's cookies only.
    await page.click(`button.connector-revoke[data-id="${tool.id}"]`);
    await page.waitForFunction(() => document.querySelectorAll("#connector-grants li").length === 1);
    const del = stub.state.requests.filter((q) => q.method === "DELETE");
    assert.equal(del.length, 1);
    assert.equal(del[0].path, "/mcp/grants/" + tool.id);
    assert.equal(del[0].headers["x-backyard"], "1");
    assert.equal(del[0].headers.authorization, undefined);
    assert.deepEqual(
      stub.state.grants.map((g) => g.id),
      [mine.id, bobs.id],
    );
    assert.match(await page.textContent("#connector-status"), /^Disconnected My tool\./);
    assert.equal(await page.evaluate(() => document.activeElement.dataset.id), mine.id, "the focus stays in the list");

    // A connection that is already gone (404) disappears as well; the last one leaves the empty note.
    stub.state.grants = stub.state.grants.filter((g) => g.id !== mine.id);
    await page.click(`button.connector-revoke[data-id="${mine.id}"]`);
    await page.waitForSelector("#connector-grants-empty:not([hidden])");
    assert.equal(await page.isHidden("#connector-grants"), true);
    assert.equal(await page.evaluate(() => document.activeElement.id), "connector-grants-title");

    // A failure keeps the connection and says why (the card stays open across the reload).
    stub.state.grants.push(mine);
    await page.reload();
    await page.waitForSelector("#connector-grants li");
    const unavailable = JSON.stringify({ error: { code: "unavailable", message: "storage is down" } });
    await page.route("**/__backyard/api/mcp/grants/*", (r) => r.fulfill({ status: 503, contentType: "application/json", body: unavailable }));
    await page.click(`button.connector-revoke[data-id="${mine.id}"]`);
    await page.waitForFunction(() => document.getElementById("connector-status").textContent === "storage is down");
    assert.equal(await page.getAttribute("#connector-status", "class"), "hint error");
    assert.equal(await page.isEnabled(`button.connector-revoke[data-id="${mine.id}"]`), true);
    assert.deepEqual(problems.filter((p) => !/status of 503/.test(p)), []);
  } finally {
    await context.close();
    await stub.close();
  }
});

test("hub: only admins get the form to disconnect a person; it asks first and reports the count", async (t) => {
  if (!browser) return t.skip(skipReason);
  const own = grant("c0000000000000000000000000000001", "admin@acme.com");
  const stub = await startStub({
    mcp: {
      grants: [
        own,
        grant("b0000000000000000000000000000001", "bob@acme.com"),
        grant("b0000000000000000000000000000002", "bob@acme.com", { client_kind: "registered", client_host: "" }),
      ],
    },
  });
  const context = await browser.newContext();
  try {
    await context.addCookies([{ name: "user", value: "admin@acme.com", url: stub.url }]);
    const { page, problems } = await open(context, stub.url + "/");
    await page.click("#connector summary");
    await page.waitForSelector("#connector-grants li");
    assert.equal(await page.isVisible("#user-admin"), true);
    assert.equal(await page.isVisible("#connector-revoke-user"), true);
    const deletes = () => stub.state.requests.filter((q) => q.method === "DELETE");

    // Nothing named: no request.
    await page.click("#connector-user-submit");
    assert.equal(await page.textContent("#connector-user-status"), "Enter the person's email or user ID.");
    assert.equal(await page.getAttribute("#connector-user", "aria-invalid"), "true");
    assert.equal(deletes().length, 0);

    // Cancelled confirmation: no request.
    await page.fill("#connector-user", " Bob@ACME.com ");
    const asked = [];
    page.once("dialog", (d) => {
      asked.push(d.type() + ": " + d.message());
      d.dismiss();
    });
    await page.click("#connector-user-submit");
    assert.deepEqual(asked, ["confirm: Disconnect every Claude connection of bob@acme.com? Their apps lose access to Backyard at once."]);
    assert.equal(deletes().length, 0);

    page.once("dialog", (d) => d.accept());
    await page.click("#connector-user-submit");
    await page.waitForFunction(() => document.getElementById("connector-user-status").textContent === "Disconnected 2 connections of bob@acme.com.");
    assert.deepEqual(
      deletes().map((q) => [q.path, q.query, q.headers["x-backyard"]]),
      [["/mcp/grants", "?user=bob%40acme.com", "1"]],
    );
    assert.deepEqual(
      stub.state.grants.map((g) => g.id),
      [own.id],
    );
    assert.equal(await page.inputValue("#connector-user"), "");
    assert.equal(await page.$$eval("#connector-grants li", (lis) => lis.length), 1, "the admin's own connection stays");

    page.once("dialog", (d) => d.accept());
    await page.fill("#connector-user", "bob@acme.com");
    await page.click("#connector-user-submit");
    await page.waitForFunction(() => document.getElementById("connector-user-status").textContent === "bob@acme.com had no connections.");
    assert.deepEqual(problems, []);

    // Someone who is not an admin never sees the form.
    await context.clearCookies();
    await context.addCookies([{ name: "user", value: "bob@acme.com", url: stub.url }]);
    await page.reload();
    await page.waitForSelector("#connector-grants-empty:not([hidden])");
    assert.equal(await page.isVisible("#connector"), true);
    assert.equal(await page.isHidden("#connector-revoke-user"), true);
  } finally {
    await context.close();
    await stub.close();
  }
});

test("hub on a phone-sized screen has no horizontal scroll", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({
    sites: [
      { name: "a-very-long-site-name-that-should-wrap-nicely", url: "http://x/", updated_at: new Date().toISOString(), owners: [] },
      { name: "pic", url: "http://x/s/pic/", title: "With a preview", preview: "preview.svg", updated_at: new Date().toISOString(), owners: [] },
    ],
    stored: { "pic/preview.svg": PREVIEW_SVG },
    mcp: { grants: [grant("a0000000000000000000000000000001", "admin@acme.com", { client_name: "A-connection-name-without-any-spaces-that-must-wrap" })] },
  });
  const context = await browser.newContext({ viewport: { width: 360, height: 740 } });
  await context.addCookies([{ name: "user", value: "admin@acme.com", url: stub.url }]);
  const overflow = (page) => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  try {
    for (const path of ["/", "/docs"]) {
      const { page, problems } = await open(context, stub.url + path);
      await page.waitForLoadState("networkidle");
      assert.ok((await overflow(page)) <= 0, `${path} overflows horizontally by ${await overflow(page)}px`);
      if (path === "/") {
        assert.equal(await page.isHidden("#banner"), true, "no error banner");
        await page.waitForFunction(() => document.querySelector(".site-preview img")?.naturalWidth > 0);
        const boxes = await page.$$eval(".site-preview", (els) => els.map((e) => Math.round(e.getBoundingClientRect().right)));
        assert.equal(boxes.length, 2);
        for (const right of boxes) assert.ok(right <= 360, "the preview boxes fit: " + boxes);
        // Every side card opened, with the connector's admin form.
        for (const summary of await page.$$(".side details > summary")) await summary.click();
        assert.equal(await page.isVisible("#connector-revoke-user"), true, "the connector card with its admin form");
        assert.equal(await page.isVisible("#cmd-mcp"), true);
        assert.ok((await overflow(page)) <= 0, `/ with the side cards open overflows horizontally by ${await overflow(page)}px`);
      }
      assert.deepEqual(problems, []);
    }
  } finally {
    await context.close();
    await stub.close();
  }
});

/** A preview image as sites ship them (16:9). */
const PREVIEW_SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720" viewBox="0 0 1280 720"><rect width="1280" height="720" fill="#e0672b"/><rect x="64" y="64" width="640" height="80" rx="12" fill="#fff"/></svg>';

/**
 * A preview of another shape: a w×h SVG in BODY with a band of BAND across its
 * top (band units high). sized: false leaves out width and height (a
 * viewBox-only SVG, as docs/deploying-sites.md suggests).
 */
const BAND = [224, 103, 43]; // #e0672b
const BODY = [43, 103, 224]; // #2b67e0
const bandedSVG = (w, h, band, sized = true) =>
  `<svg xmlns="http://www.w3.org/2000/svg"${sized ? ` width="${w}" height="${h}"` : ""} viewBox="0 0 ${w} ${h}"><rect width="${w}" height="${h}" fill="#2b67e0"/><rect width="${w}" height="${band}" fill="#e0672b"/></svg>`;

/** Decodes an 8-bit, non-interlaced RGB or RGBA PNG (a Playwright screenshot) into { width, height, at(x, y) → [r, g, b] }. */
function decodePNG(buf) {
  assert.equal(buf.toString("latin1", 1, 4), "PNG");
  let width = 0;
  let height = 0;
  let channels = 0;
  const idat = [];
  for (let off = 8; off < buf.length; ) {
    const len = buf.readUInt32BE(off);
    const type = buf.toString("latin1", off + 4, off + 8);
    const data = buf.subarray(off + 8, off + 8 + len);
    if (type === "IHDR") {
      [width, height, channels] = [data.readUInt32BE(0), data.readUInt32BE(4), { 2: 3, 6: 4 }[data[9]]];
      assert.ok(data[8] === 8 && data[12] === 0 && channels, "an 8-bit, non-interlaced RGB(A) PNG");
    } else if (type === "IDAT") idat.push(data);
    else if (type === "IEND") break;
    off += 12 + len;
  }
  const raw = inflateSync(Buffer.concat(idat));
  const stride = width * channels;
  const px = Buffer.alloc(height * stride);
  for (let y = 0; y < height; y++) {
    const filter = raw[y * (stride + 1)];
    for (let i = 0; i < stride; i++) {
      const a = i >= channels ? px[y * stride + i - channels] : 0;
      const b = y > 0 ? px[(y - 1) * stride + i] : 0;
      const c = i >= channels && y > 0 ? px[(y - 1) * stride + i - channels] : 0;
      const p = a + b - c;
      const paeth = Math.abs(p - a) <= Math.abs(p - b) && Math.abs(p - a) <= Math.abs(p - c) ? a : Math.abs(p - b) <= Math.abs(p - c) ? b : c;
      px[y * stride + i] = raw[y * (stride + 1) + 1 + i] + [0, a, b, (a + b) >> 1, paeth][filter];
    }
  }
  return { width, height, at: (x, y) => [...px.subarray(y * stride + x * channels, y * stride + x * channels + 3)] };
}
const sameColor = (rgb, want) => rgb.every((v, i) => Math.abs(v - want[i]) <= 8);

const ago = (hours) => new Date(Date.now() - hours * 3600e3).toISOString();

test("hub: the side cards start collapsed (Sites in the first screen), open by click or keyboard and stay as left", async (t) => {
  if (!browser) return t.skip(skipReason);
  const sites = ["one", "two", "three", "four", "five", "six"].map((name, i) => ({ name, url: `http://x/s/${name}/`, title: name, updated_at: ago(i + 1), owners: [] }));
  const stub = await startStub({ sites, mcp: { grants: [grant("a0000000000000000000000000000001", "alice@acme.com")] } });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const stored = (page) => page.evaluate(() => localStorage.getItem("backyard.hubPanels"));
  const opened = (page) => page.$$eval("details[data-panel]", (ds) => ds.filter((d) => d.open).map((d) => d.dataset.panel));
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector("#connector");
    await page.waitForSelector(".site");
    assert.deepEqual(await opened(page), []);
    assert.deepEqual(
      await page.$$eval(".side details > summary", (ss) => ss.map((s) => s.innerText.replace(/\s+/g, " ").trim())),
      ["Use Backyard in Claude Let Claude publish sites for you.", "Use the CLI Deploy from your terminal or CI.", "For agents Docs and tools for coding agents."],
    );
    for (const sel of ["#connector-url", "#connector-copy", "#cmd-login", ".links a"]) assert.equal(await page.isVisible(sel), false, sel + " is collapsed");
    const sitesTitle = await page.$eval("#sites-title", (h) => h.getBoundingClientRect().bottom);
    assert.ok(sitesTitle <= 800, `the Sites heading is in the first screen (bottom at ${sitesTitle}px)`);
    assert.equal(await stored(page), null);

    // A click opens one; its summary line gives way to the full introduction.
    await page.click('details[data-panel="cli"] > summary');
    assert.equal(await page.isVisible("#cmd-login"), true);
    assert.equal(await page.isHidden('details[data-panel="cli"] .panel-hint'), true);
    // <details> fires "toggle" as a task after the click, so the state is saved a moment later.
    await page.waitForFunction(() => localStorage.getItem("backyard.hubPanels") === '{"cli":true}');
    // The keyboard too (Enter and Space on the focused summary).
    await page.focus('details[data-panel="agents"] > summary');
    await page.keyboard.press("Enter");
    await page.waitForFunction(() => localStorage.getItem("backyard.hubPanels") === '{"agents":true,"cli":true}');
    assert.equal(await page.isVisible('.links a[href="/llms.txt"]'), true);

    // Remembered per card.
    await page.reload();
    await page.waitForSelector("#cmd-login");
    assert.deepEqual(await opened(page), ["cli", "agents"]);
    assert.equal(await page.isVisible("#connector-copy"), false);
    await page.click('details[data-panel="cli"] > summary');
    await page.waitForFunction(() => localStorage.getItem("backyard.hubPanels") === '{"agents":true}');
    await page.focus('details[data-panel="agents"] > summary');
    await page.keyboard.press(" ");
    await page.waitForFunction(() => localStorage.getItem("backyard.hubPanels") === null);
    assert.deepEqual(await opened(page), []);

    // Junk in storage reads as "all collapsed".
    await page.evaluate(() => localStorage.setItem("backyard.hubPanels", "{not json"));
    await page.reload();
    await page.waitForSelector(".site");
    assert.deepEqual(await opened(page), []);
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
  }
});

test("hub: grid cards show the site's preview image, or a placeholder of the same size (also when the image fails)", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({
    sites: [
      { name: "pic", url: "http://x/s/pic/", title: "With a preview", preview: "img/cover.svg", updated_at: "2026-10-18T09:30:00.123456Z", owners: [] },
      { name: "plain", url: "http://x/s/plain/", title: "Without", description: "No image here.", updated_at: "2026-10-17T09:30:00Z", owners: [] },
      { name: "gone", url: "http://x/s/gone/", title: "Missing file", preview: "preview.png", updated_at: "2026-10-16T09:30:00Z", owners: [] },
    ],
    stored: { "pic/img/cover.svg": PREVIEW_SVG },
  });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>N</title>new", "preview.svg": PREVIEW_SVG });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const previewGets = (name) => stub.state.requests.filter((q) => q.path === `/sites/${name}/preview`);
  const box = (page, name) => page.$eval(`.site[data-site="${name}"] .site-preview`, (b) => JSON.parse(JSON.stringify(b.getBoundingClientRect())));
  try {
    // The placeholders while the list loads have the same box.
    let release;
    const held = new Promise((r) => (release = r));
    const page = await context.newPage();
    const problems = [];
    page.on("pageerror", (e) => problems.push("pageerror: " + e.message));
    page.on("console", (m) => m.type() === "error" && !/status of 404\b/.test(m.text()) && problems.push("console: " + m.text()));
    await page.route("**/__backyard/api/sites", async (r) => {
      await held;
      await r.continue();
    });
    await page.goto(stub.url + "/");
    await page.waitForSelector("#site-list.skeleton .site-preview");
    const skeleton = await page.$eval("#site-list .site-preview", (b) => JSON.parse(JSON.stringify(b.getBoundingClientRect())));
    release();
    await page.waitForSelector('.site[data-site="pic"]');

    // With SiteMeta.preview: the image from the hub's own origin, versioned by updated_at.
    const img = '.site[data-site="pic"] .site-preview img';
    await page.waitForFunction((sel) => document.querySelector(sel)?.complete && document.querySelector(sel).naturalWidth > 0, img);
    assert.deepEqual(
      await page.$eval(img, (i) => ["src", "alt", "loading", "decoding", "width", "height"].map((a) => i.getAttribute(a))),
      ["/__backyard/api/sites/pic/preview?v=2026-10-18T09%3A30%3A00.123456Z", "", "lazy", "async", "320", "180"],
    );
    assert.equal(await page.getAttribute('.site[data-site="pic"] .site-preview', "aria-hidden"), "true", "decorative");
    // Without one: the placeholder, and no request at all.
    assert.equal(await page.textContent('.site[data-site="plain"] .site-preview'), "No preview available");
    assert.equal(await page.$('.site[data-site="plain"] .site-preview img'), null);
    assert.equal(previewGets("plain").length, 0);
    // A preview that fails to load (404 here; an expired session elsewhere) becomes the placeholder.
    await page.waitForSelector('.site[data-site="gone"] .site-preview.none');
    assert.equal(await page.textContent('.site[data-site="gone"] .site-preview'), "No preview available");
    assert.equal(await page.$('.site[data-site="gone"] .site-preview img'), null);

    // Every box has the same 16:9 size, as the loading placeholders had.
    const [a, b, c] = [await box(page, "pic"), await box(page, "plain"), await box(page, "gone")];
    for (const other of [b, c, skeleton]) assert.deepEqual([other.width, other.height], [a.width, a.height]);
    assert.deepEqual([skeleton.x, skeleton.y], [a.x, a.y], "nothing moves when the list arrives");
    assert.ok(Math.abs(a.width / a.height - 16 / 9) < 0.01, `16:9: ${a.width}×${a.height}`);
    assert.ok(a.width > 250, "full card width");

    // The whole card, preview included, is the link; the actions stay clickable.
    const hit = await page.evaluate(() => {
      document.querySelector('.site[data-site="pic"]').scrollIntoView({ block: "center" });
      const r = document.querySelector('.site[data-site="pic"] .site-preview').getBoundingClientRect();
      const at = document.elementFromPoint(r.x + r.width / 2, r.y + r.height / 2);
      const copy = document.querySelector('button[aria-label="Copy link to pic"]').getBoundingClientRect();
      return [at.closest("a")?.getAttribute("href"), document.elementFromPoint(copy.x + copy.width / 2, copy.y + copy.height / 2).closest("button")?.getAttribute("aria-label")];
    });
    assert.deepEqual(hit, ["http://x/s/pic/", "Copy link to pic"]);

    // Searching re-renders the cards without loading the images again.
    await page.fill("#search", "pic");
    await page.fill("#search", "");
    await page.waitForSelector('.site[data-site="gone"] .site-preview.none');
    assert.equal(previewGets("pic").length, 1);
    assert.equal(previewGets("gone").length, 1, "a failed preview is not asked for again");

    // A deploy with a preview.svg shows it at once.
    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    await page.click("#deploy-btn");
    await page.waitForSelector('.site[data-site="guest-site"].fresh');
    const fresh = '.site[data-site="guest-site"] .site-preview img';
    await page.waitForFunction((sel) => document.querySelector(sel)?.naturalWidth > 0, fresh);
    assert.match(await page.getAttribute(fresh, "src"), /^\/__backyard\/api\/sites\/guest-site\/preview\?v=\d{4}-/);
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub: a preview of any shape covers its 16:9 box from the top (a tall page, a 1200×630 og:image); the card stays one link", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({
    sites: [
      { name: "tall", url: "http://x/s/tall/", title: "Tall page", preview: "preview.svg", updated_at: ago(1), owners: [] },
      { name: "og", url: "http://x/s/og/", title: "Open Graph image", preview: "img/og.svg", updated_at: ago(2), owners: [] },
      { name: "plain", url: "http://x/s/plain/", title: "Without", updated_at: ago(3), owners: [] },
    ],
    // A full-page screenshot's shape (viewBox only) and og:image's usual 1200×630.
    stored: { "tall/preview.svg": bandedSVG(400, 2000, 100, false), "og/img/og.svg": bandedSVG(1200, 630, 60) },
  });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector('.site[data-site="plain"] .site-preview.none');
    for (const viewport of [{ width: 1280, height: 800 }, { width: 360, height: 740 }]) {
      await page.setViewportSize(viewport);
      for (const name of ["tall", "og"]) {
        const sel = `.site[data-site="${name}"] .site-preview`;
        const at = `${name} at ${viewport.width}px`;
        await page.$eval(sel, (b) => b.scrollIntoView({ block: "center" }));
        await page.waitForFunction((s) => document.querySelector(s + " img")?.complete && document.querySelector(s + " img").naturalWidth > 0, sel);
        // The image's box is the preview box (inside its bottom border), whatever the image's own ratio.
        const g = await page.$eval(sel, (b) => {
          const r = b.getBoundingClientRect();
          const i = b.querySelector("img").getBoundingClientRect();
          return { box: [r.left, r.top, r.width, r.height], img: [i.left, i.top, i.width, i.height], border: parseFloat(getComputedStyle(b).borderBottomWidth) };
        });
        assert.ok(Math.abs(g.box[2] / g.box[3] - 16 / 9) < 0.01, `${at}: the box is 16:9 (${g.box[2]}×${g.box[3]})`);
        assert.equal(g.border, 1);
        for (const [k, want, got] of [["left", g.box[0], g.img[0]], ["top offset", g.box[1], g.img[1]], ["width", g.box[2], g.img[2]], ["height", g.box[3] - g.border, g.img[3]]]) {
          assert.ok(Math.abs(want - got) < 0.01, `${at}: img ${k} ${got} ≠ box ${want} (${JSON.stringify(g)})`);
        }
        // What shows is the image's top (its band at the top centre), cropped, not letterboxed.
        const png = decodePNG(await page.locator(sel).screenshot());
        const cx = Math.floor(png.width / 2);
        const cy = Math.floor(png.height / 2);
        const samples = { "top centre": png.at(cx, 2), "bottom centre": png.at(cx, png.height - 4), "left middle": png.at(1, cy), "right middle": png.at(png.width - 2, cy) };
        const want = { "top centre": BAND, "bottom centre": BODY, "left middle": BODY, "right middle": BODY };
        for (const [k, rgb] of Object.entries(samples)) assert.ok(sameColor(rgb, want[k]), `${at}: the ${k} pixel is rgb(${rgb}), not rgb(${want[k]})`);
      }
      // The placeholder keeps its layout: the icon and the text centred in the box.
      const ph = await page.$eval('.site[data-site="plain"] .site-preview', (b) => {
        const r = b.getBoundingClientRect();
        const i = b.querySelector(".icon").getBoundingClientRect();
        const s = b.querySelector("span").getBoundingClientRect();
        return { cx: r.left + r.width / 2, cy: r.top + (r.height - 1) / 2, icon: i.left + i.width / 2, text: s.left + s.width / 2, mid: (i.top + s.bottom) / 2, below: s.top > i.bottom };
      });
      for (const k of ["icon", "text"]) assert.ok(Math.abs(ph[k] - ph.cx) < 1, `${viewport.width}px: placeholder ${k} centred (${JSON.stringify(ph)})`);
      assert.ok(Math.abs(ph.mid - ph.cy) < 1 && ph.below, `${viewport.width}px: placeholder centred vertically (${JSON.stringify(ph)})`);
      // The whole card, preview included, is still its title link.
      for (const name of ["tall", "og", "plain"]) {
        const hit = await page.evaluate((n) => {
          const card = document.querySelector(`.site[data-site="${n}"]`);
          card.scrollIntoView({ block: "center" });
          const r = card.querySelector(".site-preview").getBoundingClientRect();
          return document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2) === card.querySelector(".site-title a");
        }, name);
        assert.equal(hit, true, `${name} at ${viewport.width}px: the preview's centre hits the title link`);
      }
    }
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
  }
});

test("hub: the list view shows no previews and requests none", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({
    sites: [
      { name: "pic", url: "http://x/s/pic/", title: "With a preview", preview: "preview.svg", updated_at: ago(1), owners: [] },
      { name: "plain", url: "http://x/s/plain/", title: "Without", updated_at: ago(2), owners: [] },
    ],
    stored: { "pic/preview.svg": PREVIEW_SVG },
  });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const previewGets = () => stub.state.requests.filter((q) => /^\/sites\/[^/]+\/preview$/.test(q.path));
  try {
    await context.addInitScript(() => localStorage.setItem("backyard.hubView", "list"));
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector(".site");
    await page.waitForLoadState("networkidle");
    assert.equal(await page.getAttribute("#site-list", "class"), "site-list as-list");
    assert.equal(await page.$$eval(".site-preview", (els) => els.length), 0);
    assert.equal(previewGets().length, 0);
    const rows = await page.$$eval(".site", (els) => els.map((e) => Math.round(e.getBoundingClientRect().height)));
    for (const h of rows) assert.ok(h < 60, "compact rows: " + rows);

    await page.click("#view-grid");
    await page.waitForFunction(() => document.querySelector(".site-preview img")?.naturalWidth > 0);
    assert.equal(await page.$$eval(".site-preview", (els) => els.length), 2);
    await page.click("#view-list");
    assert.equal(await page.$$eval(".site-preview", (els) => els.length), 0);
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
  }
});

// --- owners-only site listing (sites.listing: owners, SPEC §7.1 site_listing, §7.2 "Listing")

/** Alice's site, an ownerless one she created before the upgrade, Bob's and Carol's. */
const listingSites = () => [
  { name: "mine", url: "http://x/s/mine/", title: "My site", updated_at: ago(1), updated_by: "alice@acme.com", created_by: "alice@acme.com", owners: ["alice@acme.com"] },
  { name: "legacy", url: "http://x/s/legacy/", title: "From before the upgrade", updated_at: ago(2), updated_by: "Alice@Acme.com", created_by: "Alice@Acme.com", owners: [] },
  { name: "theirs", url: "http://x/s/theirs/", title: "Bob's site", updated_at: ago(3), updated_by: "bob@acme.com", created_by: "bob@acme.com", owners: ["bob@acme.com"] },
  { name: "orphan", url: "http://x/s/orphan/", title: "Carol's old site", updated_at: ago(4), updated_by: "carol@acme.com", created_by: "carol@acme.com", owners: [] },
];
const TAKEN_HINT = "This name is taken by a site you don't own; only its owners or an admin can update it.";
const NEW_HINT = "A new site will be created.";
/** The hint for a site without owners the list leaves out: a deploy replaces it and claims it (SPEC §7.2, §7.3). */
const OWNERLESS_HINT = (creator) => `A site of this name exists without an owner${creator ? " (created by " + creator + ")" : ""}: deploying replaces it and makes you its owner.`;
const OWN_EMPTY = "You don't own any sites yet. Drop a folder above to create one.";
const GUEST_NOTE = "Sites are listed only to their signed-in owners and admins.";
const GUEST_EMPTY = "Sites are listed only to signed-in people.";
/** Waits until the deploy form's name hint reads want. */
const hintIs = (page, want) => page.waitForFunction((w) => document.getElementById("site-hint").textContent === w, want);
/** The site names the hub looked up with GET G/sites/{name}, in order. */
const lookups = (stub) => stub.state.requests.filter((q) => q.method === "GET" && /^\/deploy\/sites\/[^/]+$/.test(q.path)).map((q) => q.path.slice("/deploy/sites/".length));
const siteNames = (page) => page.$$eval(".site .site-name", (els) => els.map((e) => e.textContent));
/** Longer than the hub's lookup debounce (300 ms): a lookup would have been sent by then. */
const settle = () => new Promise((r) => setTimeout(r, 900));

test("hub with an owners-only listing: a person is listed their sites with a note; a name taken by a site they don't own is looked up", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ listing: "owners", sites: listingSites() });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>T</title>takeover" });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addCookies([{ name: "user", value: "alice@acme.com", url: stub.url }]);
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector(".site");
    // Her own site and the ownerless one she created; the count keeps its form.
    assert.deepEqual(await siteNames(page), ["mine", "legacy"]);
    assert.equal(await page.textContent("#site-count"), "(2)");
    assert.equal(await page.textContent("#sites-note"), "Only your sites are listed.");
    assert.equal(await page.isVisible("#sites-note"), true);
    const [title, note] = await page.$$eval(["#sites-title", "#sites-note"].join(","), (els) => els.map((e) => JSON.parse(JSON.stringify(e.getBoundingClientRect()))));
    assert.ok(note.top >= title.bottom && note.top - title.bottom < 12 && Math.abs(note.left - title.left) < 1, `the note is right under the heading: ${JSON.stringify({ title, note })}`);
    assert.equal(await page.isHidden("#sites-empty"), true);
    await settle();
    assert.deepEqual(lookups(stub), [], "nothing is looked up before a name is typed");

    // A name the list leaves out is looked up (debounced) with the visitor's own session.
    await page.click("#site-name");
    await page.keyboard.type("theirs");
    await page.waitForFunction((want) => document.getElementById("site-hint").textContent === want, TAKEN_HINT);
    const asked = lookups(stub);
    assert.equal(asked[asked.length - 1], "theirs");
    assert.ok(asked.length < "theirs".length, "debounced, not one lookup per key: " + asked);
    const req = stub.state.requests.find((q) => q.path === "/deploy/sites/theirs");
    assert.deepEqual([req.headers.accept, req.headers["x-backyard"], req.headers.authorization], ["application/json", "1", undefined]);
    assert.match(req.headers.cookie, /(^|; )user=alice@acme\.com($|;)/);
    // A free name (404) and a listed one (no lookup) keep today's texts.
    const free = page.waitForResponse((r) => r.url().endsWith("/__backyard/api/deploy/sites/fresh-name"));
    await page.fill("#site-name", "fresh-name");
    assert.equal((await free).status(), 404);
    await settle();
    assert.equal(await page.textContent("#site-hint"), NEW_HINT);
    await page.fill("#site-name", "mine");
    assert.equal(await page.textContent("#site-hint"), "Updates the existing site (last deployed by alice@acme.com, 1 hour ago).");
    await settle();
    assert.equal(lookups(stub).includes("mine"), false, "a listed site is not looked up");

    // A slower, older lookup is aborted and never overrides a newer name's hint.
    let held;
    const reached = new Promise((r) => (held = r));
    let release;
    const released = new Promise((r) => (release = r));
    const failed = [];
    page.on("requestfailed", (r) => failed.push(new URL(r.url()).pathname + " " + r.failure().errorText));
    await page.route(
      "**/__backyard/api/deploy/sites/theirs",
      async (route) => {
        held();
        await released;
        await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(listingSites()[2]) }).catch(() => {});
      },
      { times: 1 },
    );
    await page.fill("#site-name", "theirs");
    await reached;
    const other = page.waitForResponse((r) => r.url().endsWith("/__backyard/api/deploy/sites/other-name"));
    await page.fill("#site-name", "other-name");
    await other;
    release();
    await settle();
    assert.equal(await page.textContent("#site-hint"), NEW_HINT);
    assert.deepEqual(failed, ["/__backyard/api/deploy/sites/theirs net::ERR_ABORTED"]);

    // Deploying over the taken name is refused by the gateway (the ownership policy): nothing is overwritten.
    await page.fill("#site-name", "theirs");
    await page.waitForFunction((want) => document.getElementById("site-hint").textContent === want, TAKEN_HINT);
    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    assert.equal(await page.inputValue("#site-name"), "theirs", "a typed name is kept");
    await page.click("#deploy-btn");
    await page.waitForSelector("#deploy-error:not([hidden])");
    assert.equal(await page.textContent("#deploy-error"), "site theirs is owned by bob@acme.com; only owners and admins can change it");
    assert.equal(stub.state.sites.find((s) => s.name === "theirs").updated_by, "bob@acme.com");
    assert.deepEqual(problems.filter((p) => !/status of 403\b/.test(p)), []);

    // An admin is listed every site, is told so, and nothing is looked up.
    await context.addCookies([{ name: "user", value: "admin@acme.com", url: stub.url }]);
    const before = lookups(stub).length;
    const adm = await open(context, stub.url + "/");
    await adm.page.waitForSelector(".site");
    assert.deepEqual(await siteNames(adm.page), ["mine", "legacy", "theirs", "orphan"]);
    assert.equal(await adm.page.textContent("#site-count"), "(4)");
    assert.equal(await adm.page.textContent("#sites-note"), "You are an admin: every site is listed.");
    await adm.page.fill("#site-name", "brand-new");
    await settle();
    assert.equal(await adm.page.textContent("#site-hint"), NEW_HINT);
    assert.equal(lookups(stub).length, before, "an admin is listed every site: no lookup");
    assert.deepEqual(adm.problems, []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub with an owners-only listing: a name taken by a site without owners says a deploy replaces it and claims it", async (t) => {
  if (!browser) return t.skip(skipReason);
  // "synced" has neither owners nor a creator (a direct sync without meta).
  const synced = { name: "synced", url: "http://x/s/synced/", title: "Synced", updated_at: ago(5), file_count: 1, bytes: 1, owners: [] };
  const stub = await startStub({ listing: "owners", sites: listingSites().concat([synced]) });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>C</title>claimed" });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addCookies([{ name: "user", value: "alice@acme.com", url: stub.url }]);
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector(".site");
    assert.deepEqual(await siteNames(page), ["mine", "legacy"], "Carol's ownerless site is not listed to Alice");
    // Carol's ownerless site is not said to be protected; Bob's owned one is.
    await page.fill("#site-name", "orphan");
    await hintIs(page, OWNERLESS_HINT("carol@acme.com"));
    assert.equal(lookups(stub).includes("orphan"), true);
    await page.fill("#site-name", "synced");
    await hintIs(page, OWNERLESS_HINT(""));
    await page.fill("#site-name", "theirs");
    await hintIs(page, TAKEN_HINT);

    // The hint is right: the deploy replaces Carol's site and makes Alice its owner.
    await page.fill("#site-name", "orphan");
    await hintIs(page, OWNERLESS_HINT("carol@acme.com"));
    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    assert.equal(await page.inputValue("#site-name"), "orphan", "a typed name is kept");
    await page.click("#deploy-btn");
    await page.waitForSelector('.site[data-site="orphan"].fresh');
    const claimed = stub.state.sites.find((s) => s.name === "orphan");
    assert.deepEqual([claimed.owners, claimed.created_by, claimed.updated_by], [["alice@acme.com"], "carol@acme.com", "alice@acme.com"]);
    assert.equal(stub.state.stored.get("orphan/index.html").toString(), "<!doctype html><title>C</title>claimed");
    assert.deepEqual(await siteNames(page), ["orphan", "mine", "legacy"]);
    assert.equal(await page.textContent("#site-count"), "(3)");
    assert.match(await page.textContent("#site-hint"), /^Updates the existing site \(last deployed by alice@acme\.com, just now\)\.$/);
    assert.deepEqual(problems, []);

    // Carol is no longer listed it, and the hint now says it has an owner.
    await context.addCookies([{ name: "user", value: "carol@acme.com", url: stub.url }]);
    const carol = await open(context, stub.url + "/");
    await carol.page.waitForSelector("#sites-empty:not([hidden])");
    assert.equal(await carol.page.textContent("#sites-empty"), OWN_EMPTY);
    await carol.page.fill("#site-name", "orphan");
    await hintIs(carol.page, TAKEN_HINT);
    assert.deepEqual(carol.problems, []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub with an owners-only listing seen by a guest: no site is listed or promised; the name hint says what holds with and without a token", async (t) => {
  if (!browser) return t.skip(skipReason);
  // An admin token like the Cloudflare DEPLOY_TOKEN (the guest never owns a site).
  const tokens = { d3pl0y: { id: "token:deploy", name: "deploy", groups: [], anonymous: false, admin: true } };
  const stub = await startStub({ guest: true, listing: "owners", sites: listingSites(), tokens });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>G</title>guest" });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector("#sites-empty:not([hidden])");
    assert.equal(await page.textContent("#sites-note"), GUEST_NOTE);
    assert.equal(await page.isVisible("#sites-note"), true);
    assert.equal(await page.textContent("#sites-empty"), GUEST_EMPTY);
    assert.equal(await page.textContent("#site-count"), "");
    assert.equal(await page.isHidden("#site-list"), true);
    await page.fill("#search", "theirs");
    assert.equal(await page.textContent("#sites-empty"), GUEST_EMPTY);
    await page.fill("#search", "");

    // Names are still looked up. Without a token a guest is refused a site that has owners
    // (true for it: a guest never owns one), and told only that a site without owners exists.
    await page.fill("#site-name", "theirs");
    await hintIs(page, TAKEN_HINT);
    await page.fill("#site-name", "orphan");
    await hintIs(page, "Updates the existing site (last deployed by carol@acme.com, 4 hours ago).");
    assert.deepEqual(lookups(stub).filter((n) => n === "theirs" || n === "orphan"), ["theirs", "orphan"]);
    const free = page.waitForResponse((r) => r.url().endsWith("/__backyard/api/deploy/sites/fresh-name"));
    await page.fill("#site-name", "fresh-name");
    assert.equal((await free).status(), 404);
    await hintIs(page, NEW_HINT);

    // A guest's deploy replaces an ownerless site but never owns it, so nothing gets listed.
    await page.fill("#site-name", "orphan");
    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    assert.equal(await page.inputValue("#site-name"), "orphan");
    await page.click("#deploy-btn");
    await page.waitForSelector("#result:not([hidden])");
    assert.equal(await page.textContent("#result-title"), "Deployed orphan!");
    const replaced = stub.state.sites.find((s) => s.name === "orphan");
    assert.deepEqual([replaced.owners, replaced.updated_by], [[], "anon:0123456789abcdef"]);
    await hintIs(page, "Updates the existing site (last deployed by anon:0123456789abcdef, just now).");
    assert.equal(await page.textContent("#sites-empty"), GUEST_EMPTY);
    assert.equal(await page.textContent("#sites-note"), GUEST_NOTE);
    for (const text of [await page.textContent("#site-hint"), await page.textContent("#sites-note"), await page.textContent("#sites-empty")]) {
      assert.doesNotMatch(text, /don't own|Drop a folder/, text);
    }

    // With a token pasted the deploy runs as the token's account (here an admin): the hint
    // follows the token box as it is filled and emptied, and says only that the site exists.
    await page.fill("#site-name", "theirs");
    await hintIs(page, TAKEN_HINT);
    await page.click("#token-box summary");
    await page.fill("#deploy-token", "d3pl0y");
    await hintIs(page, "Updates the existing site (last deployed by bob@acme.com, 3 hours ago).");
    await page.fill("#deploy-token", "");
    await hintIs(page, TAKEN_HINT);
    await page.fill("#deploy-token", "d3pl0y");
    await hintIs(page, "Updates the existing site (last deployed by bob@acme.com, 3 hours ago).");
    await page.fill("#site-name", "fresh-name");
    await hintIs(page, NEW_HINT);
    await page.fill("#site-name", "theirs");
    await hintIs(page, "Updates the existing site (last deployed by bob@acme.com, 3 hours ago).");
    // The hint was right: the admin token updates Bob's site, which keeps its owner.
    await page.click("#deploy-btn");
    await page.waitForFunction(() => document.getElementById("result-title").textContent === "Deployed theirs!");
    const updated = stub.state.sites.find((s) => s.name === "theirs");
    assert.deepEqual([updated.owners, updated.updated_by], [["bob@acme.com"], "token:deploy"]);
    assert.equal(stub.state.stored.get("theirs/index.html").toString(), "<!doctype html><title>G</title>guest");
    await hintIs(page, "Updates the existing site (last deployed by token:deploy, just now).");
    assert.equal(await page.textContent("#sites-empty"), GUEST_EMPTY, "a guest's list stays empty: the token does not sign it in");
    // Forget drops the token: the guest is refused the owned site again.
    await page.click("#forget-token");
    await hintIs(page, TAKEN_HINT);
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub with an owners-only listing and none of the person's sites: the empty state; their first deploy is listed", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ listing: "owners", sites: listingSites() });
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>D</title>dave" });
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addCookies([{ name: "user", value: "dave@acme.com", url: stub.url }]);
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector("#sites-empty:not([hidden])");
    assert.equal(await page.textContent("#sites-empty"), OWN_EMPTY);
    assert.equal(await page.textContent("#sites-note"), "Only your sites are listed.");
    assert.equal(await page.textContent("#site-count"), "");
    assert.equal(await page.isHidden("#site-list"), true);
    // Searching an empty directory says nothing about other people's sites.
    await page.fill("#search", "theirs");
    assert.equal(await page.textContent("#sites-empty"), OWN_EMPTY);
    await page.fill("#search", "");

    await page.setInputFiles("#folder-input", site);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
    assert.equal(await page.inputValue("#site-name"), "guest-site");
    await page.click("#deploy-btn");
    await page.waitForSelector('.site[data-site="guest-site"].fresh');
    assert.deepEqual(stub.state.sites.find((s) => s.name === "guest-site").owners, ["dave@acme.com"], "the deployer became the owner");
    assert.deepEqual(await siteNames(page), ["guest-site"]);
    assert.equal(await page.textContent("#site-count"), "(1)");
    assert.equal(await page.textContent("#sites-note"), "Only your sites are listed.");
    assert.equal(await page.isHidden("#sites-empty"), true);
    assert.match(await page.textContent("#site-hint"), /^Updates the existing site \(last deployed by dave@acme\.com, just now\)\.$/);
    assert.deepEqual(problems, []);
  } finally {
    await context.close();
    await stub.close();
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub listing every site (site_listing all, or no field): no note, today's texts, no name lookups", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ sites: listingSites() });
  const older = await startStub({ info: { site_listing: undefined } }); // a server from before the option
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addCookies([{ name: "user", value: "alice@acme.com", url: stub.url }]);
  try {
    const { page, problems } = await open(context, stub.url + "/");
    await page.waitForSelector(".site");
    assert.equal((await page.evaluate(() => fetch("/__backyard/api/info").then((r) => r.json()))).site_listing, "all");
    assert.deepEqual(await siteNames(page), ["mine", "legacy", "theirs", "orphan"]);
    assert.equal(await page.textContent("#site-count"), "(4)");
    assert.equal(await page.isHidden("#sites-note"), true);
    assert.equal(await page.textContent("#sites-note"), "");
    await page.fill("#site-name", "theirs");
    assert.equal(await page.textContent("#site-hint"), "Updates the existing site (last deployed by bob@acme.com, 3 hours ago).");
    await page.click("#site-name", { clickCount: 3 });
    await page.keyboard.type("brand-new");
    await settle();
    assert.equal(await page.textContent("#site-hint"), NEW_HINT);
    assert.deepEqual(lookups(stub), [], "no lookup when every site is listed");

    const old = await open(context, older.url + "/");
    await old.page.waitForSelector("#sites-empty:not([hidden])");
    assert.equal(await old.page.textContent("#sites-empty"), "No sites yet. Drop a folder above to create the first one.");
    assert.equal(await old.page.isHidden("#sites-note"), true);
    await old.page.fill("#site-name", "brand-new");
    await settle();
    assert.equal(await old.page.textContent("#site-hint"), NEW_HINT);
    assert.deepEqual(lookups(older), []);
    assert.deepEqual(problems.concat(old.problems), []);
  } finally {
    await context.close();
    await Promise.all([stub.close(), older.close()]);
  }
});

test("hub deploy to presigned storage URLs on another origin: headers verbatim; a CORS failure is explained", async (t) => {
  if (!browser) return t.skip(skipReason);
  const { dir, site } = makeSiteDir({ "index.html": "<!doctype html><title>B</title>bucket", "img/a.svg": "<svg/>" });
  try {
    for (const cors of [true, false]) {
      const stub = await startStub({ presign: { cors } });
      const context = await browser.newContext();
      try {
        const { page, problems } = await open(context, stub.url + "/");
        await page.waitForSelector("#site-list:not([aria-busy])", { state: "attached" });
        await page.setInputFiles("#folder-input", site);
        await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled);
        await page.click("#deploy-btn");
        await page.waitForSelector("#result:not([hidden]), #deploy-error:not([hidden])", { timeout: 30_000 });
        const puts = stub.state.bucket.filter((r) => r.method === "PUT");
        if (cors) {
          assert.equal(await page.isHidden("#deploy-error"), true, await page.textContent("#deploy-error"));
          assert.equal(puts.length, 2);
          for (const r of puts) {
            assert.equal(r.headers["x-ms-blob-type"], "BlockBlob");
            assert.match(r.headers["x-amz-meta-sha256"], /^[0-9a-f]{64}$/);
            assert.equal(r.headers["content-type"], "application/octet-stream", "the signed Content-Type, not the file's");
            assert.equal(r.headers.cookie, undefined, "no cookies to storage");
          }
          assert.equal(stub.state.stored.get("guest-site/img/a.svg").toString(), "<svg/>");
          assert.equal(stub.state.deploys[0].committed, true);
          assert.deepEqual(problems, []);
        } else {
          assert.equal(puts.length, 0, "the preflight was refused");
          const msg = await page.textContent("#deploy-error");
          assert.match(msg, /failed: network or CORS error/);
          assert.ok(msg.includes(`the bucket CORS must allow PUT from ${stub.url} with the headers Content-Type, x-ms-blob-type, x-amz-meta-sha256`), msg);
          assert.equal(stub.state.deploys[0].committed, false);
          assert.equal(stub.state.bucket.filter((r) => r.method === "OPTIONS").length >= 1, true);
          assert.deepEqual(problems.filter((p) => !/CORS policy|net::ERR_FAILED/.test(p)), []);
        }
      } finally {
        await context.close();
        await stub.close();
      }
    }
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("hub with an expired session (API calls redirected by a proxy) goes to the login page and back", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ expiredSession: true });
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    await page.goto(stub.url + "/?q=poll");
    await page.waitForURL((u) => u.pathname === "/__backyard/auth/login", { timeout: 10_000 });
    assert.equal(new URL(page.url()).searchParams.get("rd"), stub.url + "/?q=poll");
    assert.deepEqual(stub.state.logins, [stub.url + "/?q=poll"], "one navigation, although several API calls failed");
  } finally {
    await context.close();
    await stub.close();
  }
});

test("docs page with an expired session goes to the login page and comes back to the same document", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ expiredSession: true });
  const context = await browser.newContext();
  try {
    const page = await context.newPage();
    const docsURL = stub.url + "/docs?doc=recipes#live-lists";
    await page.goto(docsURL);
    await page.waitForURL((u) => u.pathname === "/__backyard/auth/login", { timeout: 10_000 });
    assert.equal(new URL(page.url()).searchParams.get("rd"), docsURL);
    assert.deepEqual(stub.state.logins, [docsURL]);
    assert.equal(await page.textContent("h1"), "Sign in");

    // Coming back still expired (a broken login) does not loop: the page offers a link instead.
    await page.goto(docsURL);
    await page.waitForSelector("#doc .docs-status a");
    assert.equal(await page.textContent("#doc .docs-status"), "Your session has expired. Sign in again");
    assert.equal(await page.getAttribute("#doc .docs-status a", "href"), "/__backyard/auth/login?rd=" + encodeURIComponent(docsURL));
    assert.equal(stub.state.logins.length, 1);
  } finally {
    await context.close();
    await stub.close();
  }
});
