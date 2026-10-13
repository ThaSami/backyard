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
