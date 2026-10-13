// End-to-end browser test of the hub and the SDK against a REAL server: the
// Go server (`backyard serve --dev`) or the Cloudflare Worker (`wrangler dev`).
// Skipped unless BACKYARD_E2E_URL is set and playwright-core + Chromium are
// available (see browser.test.mjs):
//
//   BACKYARD_E2E_URL=http://127.0.0.1:8080 [BACKYARD_E2E_TOKEN=<deploy token>] \
//   PLAYWRIGHT_CORE=/tmp/pw/node_modules/playwright-core/index.mjs \
//   PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers node --test web/test/e2e.test.mjs
//
// The server must run with auth.mode none (guests) and either accept
// anonymous deploys or be given its deploy token. The test deploys a site
// named e2e-<random> through the hub's folder picker, exercises every SDK
// area in that site (AI and the warehouse are optional: "unsupported" and
// friends are accepted), checks realtime between two tabs, and deletes the
// site again when it can (token, or a signed-in user).
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { randomBytes } from "node:crypto";

const BASE = (process.env.BACKYARD_E2E_URL || "").replace(/\/+$/, "");
const TOKEN = process.env.BACKYARD_E2E_TOKEN || "";

let pw = null;
let skipReason = BASE ? "" : "BACKYARD_E2E_URL is not set";
if (BASE) {
  try {
    pw = await import(process.env.PLAYWRIGHT_CORE || "playwright-core");
  } catch {
    skipReason = "playwright-core is not installed (set PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs)";
  }
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

/** Opens a page and collects uncaught exceptions and script console errors. */
async function open(context, url) {
  const page = await context.newPage();
  const problems = [];
  page.on("pageerror", (e) => problems.push("pageerror: " + e.message));
  page.on("console", (m) => {
    // Responses the test provokes on purpose (404, 409, 401, 501…) are logged by the browser itself.
    if (m.type() === "error" && !/^Failed to load resource: the server responded with a status of \d+/.test(m.text())) problems.push("console: " + m.text());
  });
  await page.goto(url);
  return { page, problems };
}

const SITE_FILES = {
  "index.html": `<!doctype html><html><head><meta charset="utf-8"><title>E2E site</title>
<meta name="description" content="Backyard end-to-end test site">
<script src="/__backyard/backyard.js"></script><script src="app.js" defer></script></head>
<body><h1>e2e</h1></body></html>`,
  "app.js": "window.appLoaded = true;\n",
  "sub/page.html": "<!doctype html><title>sub</title><p>sub page</p>",
  ".git/HEAD": "ref: refs/heads/main\n",
  "node_modules/x/index.js": "ignored",
  "CLAUDE.md": "agents",
};

test("hub deploy + SDK end to end against a real server", { timeout: 180_000 }, async (t) => {
  if (!browser) return t.skip(skipReason);
  const name = "e2e-" + randomBytes(4).toString("hex");
  const dir = mkdtempSync(join(tmpdir(), "backyard-e2e-"));
  const folder = join(dir, name);
  for (const [p, text] of Object.entries(SITE_FILES)) {
    mkdirSync(join(folder, p, ".."), { recursive: true });
    writeFileSync(join(folder, p), text);
  }
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  let deployed = false;
  let hub = null;
  try {
    // --- Hub: info, directory, drag-and-drop style folder deploy.
    hub = await open(context, BASE + "/" + (TOKEN ? "#token=" + encodeURIComponent(TOKEN) : ""));
    const { page } = hub;
    await page.waitForSelector("#site-list:not([aria-busy])", { state: "attached" });
    assert.equal(await page.isHidden("#banner"), true, "server info and site list loaded: " + (await page.textContent("#banner")));
    const info = await page.evaluate(() => fetch("/__backyard/api/info").then((r) => r.json()));
    assert.ok(["host", "path"].includes(info.url_mode), "info.url_mode");
    assert.match(await page.textContent("#cmd-login"), /^backyard login --server /);
    await page.setInputFiles("#folder-input", folder);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled, null, { timeout: 30_000 });
    assert.equal(await page.inputValue("#site-name"), name);
    assert.match(await page.textContent("#selection-summary"), new RegExp("^" + name + " · 3 files · "));
    await page.click("#deploy-btn");
    await page.waitForSelector("#result:not([hidden]), #deploy-error:not([hidden])", { timeout: 60_000 });
    assert.equal(await page.isHidden("#deploy-error"), true, "deploy failed: " + (await page.textContent("#deploy-error")));
    deployed = true;
    const siteURL = await page.textContent("#result-link");
    assert.match(await page.textContent("#progress-text"), /^Deployed · 3 uploaded · 0 unchanged/);
    await page.waitForSelector(`.site[data-site="${name}"]`);
    assert.equal(await page.textContent(`.site[data-site="${name}"] .site-title a`), "E2E site", "title extracted from index.html");
    // Re-deploying the same folder uploads nothing.
    await page.click("#deploy-btn");
    await page.waitForFunction(() => /^Deployed · 0 uploaded · 3 unchanged/.test(document.getElementById("progress-text").textContent), null, { timeout: 60_000 });

    // --- The deployed site with the SDK.
    const a = await open(context, siteURL);
    await a.page.waitForFunction(() => window.appLoaded === true && !!window.backyard);
    const sub = await a.page.evaluate(() => fetch("sub/page").then((r) => [r.status, r.headers.get("content-type")]));
    assert.deepEqual(sub, [200, "text/html; charset=utf-8"], "clean URL serves sub/page.html");
    const ignored = await a.page.evaluate(() => fetch("CLAUDE.md").then((r) => r.status));
    assert.equal(ignored, 404, "ignored files are not deployed");

    const site = await a.page.evaluate(() => ({ ...backyard.site }));
    assert.equal(site.name, name);
    assert.equal(site.mode, info.url_mode);
    assert.equal(site.url, siteURL);
    assert.equal(site.apiBase, siteURL + "__backyard");

    const r = await a.page.evaluate(async () => {
      const out = {};
      const errOf = (p) => p.then(
        () => null,
        (e) => ({ name: e.name, code: e.code, status: e.status, isBE: e instanceof backyard.BackyardError }),
      );
      const me = await backyard.me();
      out.me = { hasId: typeof me.id === "string" && me.id.length > 0, cached: (await backyard.id.me()) === me };

      // Database + realtime.
      const posts = backyard.db.collection("posts");
      const events = [];
      const unsub = posts.subscribe({ onCreate: (d) => events.push("create:" + d.id), onUpdate: (d) => events.push("update:" + d.id + ":" + d.votes), onDelete: (id) => events.push("delete:" + id) });
      const snapshots = [];
      const stop = posts.watch((docs) => snapshots.push(docs.map((d) => d.id + "=" + d.votes).join(",")), posts.orderBy("votes", "desc"));
      if (!backyard.socket.connected) await new Promise((resolve) => backyard.socket.on("open", resolve));
      await new Promise((res) => setTimeout(res, 300)); // let the subscription be acknowledged
      const created = await posts.create({ title: "A", votes: 1, tags: ["x"] }, { id: "a" });
      out.created = { id: created.id, version: created._meta.version, by: typeof created._meta.created_by };
      const gen = await posts.create({ title: "B", votes: 5 });
      out.gen = gen.id;
      out.genID = /^[0-9A-HJKMNP-TV-Z]{26}$/.test(gen.id);
      out.conflict = await errOf(posts.create({ title: "dup" }, { id: "a" }));
      const updated = await posts.update("a", { votes: 9, title: null });
      out.updated = { votes: updated.votes, hasTitle: "title" in updated, version: updated._meta.version };
      const replaced = await posts.set("c", { votes: 3 });
      out.replaced = replaced.votes;
      out.writeBack = (await posts.set("c", Object.assign({}, replaced, { votes: 4 })))._meta.version; // _meta/id in a write body are ignored
      out.missing = await posts.get("nope");
      out.list = (await posts.list()).map((d) => d.id);
      const page1 = await posts.list({ limit: 2, orderBy: "votes", order: "desc" });
      out.page1 = { ids: page1.map((d) => d.id), hasMore: page1.hasMore };
      out.query = (await posts.where("votes", ">=", 4).orderBy("votes", "desc").get()).map((d) => d.id);
      out.contains = (await posts.where("tags", "contains", "x").get()).map((d) => d.id);
      out.inOp = (await posts.find({ where: [["votes", "in", [3, 4, 9]]], orderBy: "votes" })).map((d) => d.id);
      out.first = (await posts.orderBy("votes").first()).id;
      out.none = await posts.where("votes", ">", 100).first();
      out.typeStrict = (await posts.where("votes", "==", "9").get()).length;
      await posts.delete(gen.id);
      await posts.delete("never-existed");
      out.collections = await backyard.db.collections();
      out.badCollection = await errOf(backyard.db.collection("_system").list());
      await new Promise((res) => setTimeout(res, 600));
      stop();
      unsub();
      out.events = events;
      out.lastSnapshot = snapshots[snapshots.length - 1];
      out.snapshotCount = snapshots.length;

      // Files.
      const f = await backyard.files.upload(new Blob(["hello e2e"], { type: "text/plain" }), { name: "hello e2e.txt" });
      out.file = { name: f.name, size: f.size, type: f.content_type, urlOK: f.url.includes("/__backyard/files/" + f.id + "/") };
      out.fileBody = await fetch(f.url).then((x) => x.text());
      out.fileGet = (await backyard.files.get(f.id)).id === f.id;
      out.fileList = (await backyard.files.list()).some((x) => x.id === f.id);
      await backyard.files.delete(f.id);
      out.fileGone = await backyard.files.get(f.id);

      // Identity lookups.
      out.lookup = await backyard.id.lookup("nobody-" + Date.now() + "@example.com");

      // AI (optional on the server).
      const models = await backyard.ai.models().catch((e) => e);
      if (models instanceof Error) {
        out.ai = { unavailable: models.code };
      } else {
        out.ai = { default: models.default };
        out.ai.chat = (await backyard.ai.chat("ping")).content;
        const s = backyard.ai.stream("hi", { onText: () => {} });
        const chunks = [];
        for await (const c of s) chunks.push(c.text);
        const final = await s.done;
        out.ai.stream = { joined: chunks.join(""), content: final.content, chunks: chunks.length, stop: final.stop_reason };
        out.ai.json = (await backyard.ai.chat("x", { json: true }).catch((e) => ({ json: e.code }))).json;
        const img = await backyard.ai.image("a dot").catch((e) => ({ error: e.code }));
        out.ai.image = img.error ? img : { ok: typeof img.url === "string" && img.images.length >= 1 };
        if (img.url) out.ai.imageFetch = await fetch(img.url).then((x) => [x.status, x.headers.get("content-type")]);
        const emb = await backyard.ai.embed(["a", "bb"]).catch((e) => ({ error: e.code }));
        out.ai.embed = Array.isArray(emb) ? emb.length : emb;
      }

      // Warehouse (optional).
      const wh = await backyard.warehouse.query("SELECT 1 AS n").catch((e) => e);
      out.warehouse = wh instanceof Error ? { code: wh.code, status: wh.status } : { rows: wh.rows, row_count: wh.row_count };
      return out;
    });

    assert.deepEqual(r.me, { hasId: true, cached: true });
    assert.deepEqual(r.created, { id: "a", version: 1, by: "string" });
    assert.equal(r.genID, true, "generated ids are ULIDs");
    assert.deepEqual(r.conflict, { name: "BackyardError", code: "conflict", status: 409, isBE: true });
    assert.deepEqual(r.updated, { votes: 9, hasTitle: false, version: 2 });
    assert.equal(r.replaced, 3);
    assert.equal(r.writeBack, 2);
    assert.equal(r.missing, null);
    assert.deepEqual(r.list.slice(0, 1), ["a"], "default order: creation time");
    assert.deepEqual(r.page1, { ids: ["a", r.gen], hasMore: true });
    assert.deepEqual(r.query, ["a", r.gen, "c"]);
    assert.deepEqual(r.contains, ["a"]);
    assert.deepEqual(r.inOp, ["c", "a"]);
    assert.equal(r.first, "c");
    assert.equal(r.none, null);
    assert.equal(r.typeStrict, 0, '"9" never equals 9');
    assert.deepEqual(r.collections, ["posts"]);
    assert.equal(r.badCollection && r.badCollection.isBE, true);
    assert.ok(["bad_request", "forbidden", "not_found"].includes(r.badCollection.code), JSON.stringify(r.badCollection));
    assert.deepEqual(r.events.slice(0, 2), ["create:a", r.events[1]]);
    assert.ok(r.events.includes("update:a:9") && r.events.some((e) => e.startsWith("delete:")), JSON.stringify(r.events));
    assert.equal(r.lastSnapshot, "a=9,c=4", "the live query converged: " + r.snapshotCount + " snapshots");
    assert.deepEqual(r.file, { name: "hello e2e.txt", size: 9, type: r.file.type, urlOK: true });
    assert.match(r.file.type, /^text\/plain/);
    assert.equal(r.fileBody, "hello e2e");
    assert.equal(r.fileGet, true);
    assert.equal(r.fileList, true);
    assert.equal(r.fileGone, null);
    assert.equal(r.lookup, null);
    if (r.ai.unavailable) {
      t.diagnostic("AI unavailable on this server: " + r.ai.unavailable);
    } else {
      assert.equal(typeof r.ai.chat, "string");
      assert.equal(r.ai.stream.joined, r.ai.stream.content);
      assert.ok(r.ai.stream.chunks >= 1);
      assert.equal(r.ai.stream.stop, "end_turn");
      t.diagnostic("AI: " + JSON.stringify(r.ai));
    }
    t.diagnostic("warehouse: " + JSON.stringify(r.warehouse));
    if (r.warehouse.rows) assert.deepEqual(r.warehouse.rows, [{ n: 1 }]);
    else assert.ok(["unsupported", "forbidden", "not_found", "bad_request"].includes(r.warehouse.code), JSON.stringify(r.warehouse));

    // --- Realtime rooms between two tabs.
    const b = await open(context, siteURL);
    await b.page.waitForFunction(() => !!window.backyard);
    await a.page.evaluate(() => {
      window.room = backyard.socket.room("lobby", { state: { color: "red" } });
      window.log = [];
      room.on("members", (ms) => log.push("members:" + ms.length));
      room.on("message", (data, from) => log.push("msg:" + data.text + ":" + (from.cid !== backyard.socket.cid)));
      room.on("update", (m) => log.push("update:" + m.state.color));
    });
    await a.page.waitForFunction(() => window.room.self !== null, null, { timeout: 15_000 });
    await b.page.evaluate(async () => {
      window.room = backyard.socket.room("lobby", { state: { color: "blue" } });
      await new Promise((resolve) => room.on("members", resolve));
      window.peers = room.members.map((m) => m.state.color);
      room.send({ text: "hi" });
      room.updateState({ color: "green" });
    });
    // Messages and presence travel separately (no ordering between them).
    await a.page.waitForFunction(() => window.log.includes("update:green") && window.log.includes("msg:hi:true"), null, { timeout: 15_000 });
    assert.deepEqual(await b.page.evaluate(() => window.peers), ["red"]);
    const log = await a.page.evaluate(() => window.log);
    assert.deepEqual(log.filter((l) => !l.startsWith("members:")).sort(), ["msg:hi:true", "update:green"]);
    assert.equal(await a.page.evaluate(() => room.members.length), 1);
    await b.page.evaluate(() => room.leave());
    await a.page.waitForFunction(() => window.room.members.length === 0, null, { timeout: 15_000 });
    assert.deepEqual(hub.problems.concat(a.problems, b.problems), [], "no script errors on the hub or the site");
    await b.page.close();
    await a.page.close();
  } finally {
    if (deployed && hub) {
      // Clean up through the hub when allowed (token or signed-in user).
      const { page } = hub;
      try {
        await page.goto(BASE + "/" + (TOKEN ? "#token=" + encodeURIComponent(TOKEN) : ""));
        await page.waitForSelector(`.site[data-site="${name}"]`, { timeout: 10_000 });
        const del = await page.$(`button[aria-label="Delete ${name}"]`);
        if (del) {
          await del.click();
          await page.fill("#delete-confirm", name);
          await page.click("#delete-submit");
          await page.waitForFunction((n) => !document.querySelector(`.site[data-site="${n}"]`), name, { timeout: 15_000 });
        } else {
          t.diagnostic(`left site ${name} behind (deleting needs a token or a signed-in user)`);
        }
      } catch (err) {
        t.diagnostic(`cleanup of ${name} failed: ${err.message}`);
      }
      const hubProblems = hub.problems.filter((p) => !/status of 401/.test(p));
      if (hubProblems.length) t.diagnostic("hub page problems: " + JSON.stringify(hubProblems));
    }
    await context.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
