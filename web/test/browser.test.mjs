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

test("hub on a phone-sized screen has no horizontal scroll", async (t) => {
  if (!browser) return t.skip(skipReason);
  const stub = await startStub({ sites: [{ name: "a-very-long-site-name-that-should-wrap-nicely", url: "http://x/", updated_at: new Date().toISOString(), owners: [] }] });
  const context = await browser.newContext({ viewport: { width: 360, height: 740 } });
  try {
    for (const path of ["/", "/docs"]) {
      const { page, problems } = await open(context, stub.url + path);
      await page.waitForLoadState("networkidle");
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
      assert.ok(overflow <= 0, `${path} overflows horizontally by ${overflow}px`);
      if (path === "/") assert.equal(await page.isHidden("#banner"), true, "no error banner");
      assert.deepEqual(problems, []);
    }
  } finally {
    await context.close();
    await stub.close();
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
