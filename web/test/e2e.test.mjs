// End-to-end browser test of the hub and the SDK against a REAL server: the
// Go server (`backyard serve --dev`) or the Cloudflare Worker (`wrangler dev`).
// Needs playwright-core and Chromium (see browser.test.mjs).
//
// Against the Go server of this checkout, started and stopped by the test
// (it builds ./cmd/backyard — or uses BACKYARD_E2E_BIN — and runs
// `backyard serve --dev` on localhost:$BACKYARD_E2E_PORT, default 18231, with a
// throwaway data directory, a deploy token and a fake OpenAI-compatible AI
// upstream; with BACKYARD_TEST_POSTGRES_DSN also a Postgres warehouse):
//
//   BACKYARD_E2E_SERVE=go \
//   PLAYWRIGHT_CORE=/opt/node-tools/node_modules/playwright-core/index.mjs \
//   PLAYWRIGHT_BROWSERS_PATH=/opt/pw-browsers node --test web/test/e2e.test.mjs
//
// (BACKYARD_E2E_SERVE takes precedence over BACKYARD_E2E_URL.) Against a
// server that is already running (`make e2e` does this with
// scripts/with-server.sh, for the Go server or the Worker):
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
import { createHash, randomBytes } from "node:crypto";
import { spawn, spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import http from "node:http";

const SERVE = (process.env.BACKYARD_E2E_SERVE || "").trim().toLowerCase();
if (SERVE && SERVE !== "go" && SERVE !== "1") throw new Error(`BACKYARD_E2E_SERVE=${SERVE}: only "go" is supported`);
let BASE = SERVE ? "" : (process.env.BACKYARD_E2E_URL || "").replace(/\/+$/, "");
let TOKEN = process.env.BACKYARD_E2E_TOKEN || "";
/** Set when the test runs its own Go server: AI and the stream keep-alive are then known to work. */
let goServer = null;

let pw = null;
let skipReason = BASE || SERVE ? "" : "set BACKYARD_E2E_SERVE=go (or BACKYARD_E2E_URL)";
if (BASE || SERVE) {
  try {
    pw = await import(process.env.PLAYWRIGHT_CORE || "playwright-core");
  } catch {
    skipReason = "playwright-core is not installed (set PLAYWRIGHT_CORE=/path/to/playwright-core/index.mjs)";
  }
}

// --- the Go server under test (BACKYARD_E2E_SERVE=go) ------------------------

const REPO = fileURLToPath(new URL("../../", import.meta.url));

/** A 1×1 PNG returned by the fake image model. */
const PNG = Buffer.from("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", "base64");

/** Prompts containing this word make the fake model think long enough for the server's ": ping" keep-alive (15 s). */
const SLOW = "slowly";

/**
 * A fake OpenAI-compatible upstream (chat completions with streaming, image
 * generation, embeddings), so the Go server's AI endpoints answer for real.
 */
async function startFakeAI() {
  const server = http.createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      let body = {};
      try {
        body = JSON.parse(Buffer.concat(chunks).toString() || "{}");
      } catch {
        body = {};
      }
      const send = (status, obj) => {
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(obj));
      };
      const path = new URL(req.url, "http://x").pathname;
      if (path.endsWith("/chat/completions")) {
        const last = (body.messages || []).filter((m) => m.role === "user").pop() || {};
        const text = typeof last.content === "string" ? last.content : (last.content || []).map((p) => p.text || "").join("");
        const reply = body.response_format ? '{"ok": true}' : "echo: " + text;
        const usage = { prompt_tokens: 5, completion_tokens: 3, total_tokens: 8 };
        if (!body.stream) {
          return send(200, { id: "c1", object: "chat.completion", model: body.model, choices: [{ index: 0, message: { role: "assistant", content: reply }, finish_reason: "stop" }], usage });
        }
        res.writeHead(200, { "Content-Type": "text/event-stream" });
        const ev = (o) => res.write("data: " + JSON.stringify(o) + "\n\n");
        const timer = setTimeout(
          () => {
            for (const piece of reply.match(/.{1,4}/gs) || []) ev({ id: "c1", object: "chat.completion.chunk", model: body.model, choices: [{ index: 0, delta: { content: piece }, finish_reason: null }] });
            ev({ id: "c1", object: "chat.completion.chunk", model: body.model, choices: [{ index: 0, delta: {}, finish_reason: "stop" }] });
            ev({ id: "c1", object: "chat.completion.chunk", model: body.model, choices: [], usage });
            res.end("data: [DONE]\n\n");
          },
          text.includes(SLOW) ? 16_500 : 0,
        );
        res.on("close", () => clearTimeout(timer));
        return;
      }
      if (path.endsWith("/images/generations")) {
        return send(200, { created: 1, data: Array.from({ length: body.n || 1 }, () => ({ b64_json: PNG.toString("base64") })) });
      }
      if (path.endsWith("/embeddings")) {
        const input = Array.isArray(body.input) ? body.input : [body.input];
        return send(200, { object: "list", model: body.model, data: input.map((t, i) => ({ object: "embedding", index: i, embedding: [String(t).length, 0.5, -1] })), usage: { prompt_tokens: input.length, total_tokens: input.length } });
      }
      send(404, { error: { message: "no such endpoint: " + path } });
    });
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  return { url: `http://127.0.0.1:${server.address().port}/v1`, close: () => new Promise((resolve) => server.close(resolve)) };
}

async function waitHealthy(url, proc, logs, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (proc.exitCode !== null) throw new Error(`backyard serve exited with ${proc.exitCode}:\n${logs.join("")}`);
    try {
      const res = await fetch(url + "/__backyard/healthz");
      if (res.ok) return;
    } catch {
      /* not listening yet */
    }
    if (Date.now() > deadline) throw new Error(`backyard serve did not become healthy:\n${logs.join("")}`);
    await new Promise((r) => setTimeout(r, 200));
  }
}

/** Builds and starts `backyard serve --dev` with a throwaway configuration. */
async function startGoServer() {
  const port = Number(process.env.BACKYARD_E2E_PORT || 18231);
  const dir = mkdtempSync(join(tmpdir(), "backyard-e2e-go-"));
  let bin = process.env.BACKYARD_E2E_BIN || "";
  if (!bin) {
    bin = join(dir, "backyard");
    const built = spawnSync("go", ["build", "-o", bin, "./cmd/backyard"], { cwd: REPO, stdio: ["ignore", "inherit", "inherit"], timeout: 600_000 });
    if (built.status !== 0) throw new Error("go build ./cmd/backyard failed");
  }
  const fakeAI = await startFakeAI();
  const token = "e2e-" + randomBytes(16).toString("hex");
  const pg = process.env.BACKYARD_TEST_POSTGRES_DSN || "";
  const config = {
    server: { listen: `:${port}`, dev: true, name: "Backyard E2E" },
    auth: { mode: "none", tokens: [{ name: "e2e", sha256: createHash("sha256").update(token).digest("hex"), admin: true }] },
    storage: { type: "fs", fs: { root: join(dir, "data") } },
    database: { type: "sqlite", sqlite: { path: join(dir, "backyard.db") } },
    ai: {
      default_model: "smart",
      providers: { fake: { type: "openai", base_url: fakeAI.url, api_key: "test" } },
      models: {
        smart: { provider: "fake", model: "fake-chat", kind: "chat", description: "Fake chat model" },
        image: { provider: "fake", model: "fake-image", kind: "image" },
        embedding: { provider: "fake", model: "fake-embed", kind: "embedding" },
      },
    },
    ...(pg ? { warehouse: { allow_raw_sql: true, sources: { default: { type: "postgres", dsn: pg } } } } : {}),
    telemetry: { log_level: "warn" },
  };
  const cfgPath = join(dir, "backyard.yaml");
  writeFileSync(cfgPath, JSON.stringify(config, null, 2)); // JSON is valid YAML
  const logs = [];
  const proc = spawn(bin, ["serve", "--dev", "--config", cfgPath], { cwd: dir, stdio: ["ignore", "pipe", "pipe"] });
  proc.stdout.on("data", (b) => logs.push(b.toString()));
  proc.stderr.on("data", (b) => logs.push(b.toString()));
  const url = `http://localhost:${port}`;
  try {
    await waitHealthy(url, proc, logs, 60_000);
  } catch (err) {
    proc.kill("SIGKILL");
    await fakeAI.close();
    rmSync(dir, { recursive: true, force: true });
    throw err;
  }
  return {
    url,
    token,
    logs,
    warehouse: !!pg,
    async stop() {
      if (proc.exitCode === null) {
        const exited = new Promise((resolve) => proc.once("exit", resolve));
        proc.kill("SIGTERM");
        const timer = setTimeout(() => proc.kill("SIGKILL"), 10_000);
        await exited;
        clearTimeout(timer);
      }
      await fakeAI.close();
      rmSync(dir, { recursive: true, force: true });
    },
  };
}

let browser = null;
before(
  async () => {
    if (!pw) return;
    if (SERVE) {
      goServer = await startGoServer();
      BASE = goServer.url;
      TOKEN = goServer.token;
    }
    try {
      const chromium = pw.chromium || (pw.default && pw.default.chromium);
      browser = await chromium.launch({ executablePath: process.env.BACKYARD_TEST_CHROMIUM || undefined });
    } catch (err) {
      skipReason = "Chromium is not available: " + String(err.message || err).split("\n")[0];
    }
  },
  { timeout: 900_000 },
);
after(async () => {
  if (browser) await browser.close();
  if (goServer) await goServer.stop();
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

/**
 * Hands the deploy token (if any) to the hub page in the URL fragment: the hub
 * asks whose token it is before using it, so this confirms it (unless this
 * browser already uses that token).
 */
async function useHandedToken(page) {
  if (!TOKEN) return;
  await page.waitForFunction((tok) => document.getElementById("handoff-dialog").open || document.getElementById("deploy-token").value === tok, TOKEN, { timeout: 15_000 });
  if (await page.evaluate(() => document.getElementById("handoff-dialog").open)) {
    assert.match(await page.textContent("#handoff-title"), /^Use the deploy token of /);
    await page.click("#handoff-use");
  }
  await page.waitForFunction((tok) => document.getElementById("deploy-token").value === tok, TOKEN);
}

const SITE_FILES = {
  "index.html": `<!doctype html><html><head><meta charset="utf-8"><title>E2E site</title>
<meta name="description" content="Backyard end-to-end test site">
<meta property="og:image" content="/img/cover.svg?x=1">
<script src="/__backyard/backyard.js"></script><script src="app.js" defer></script></head>
<body><h1>e2e</h1></body></html>`,
  "app.js": "window.appLoaded = true;\n",
  // The preview the hub shows (SiteMeta.preview, from the og:image above).
  "img/cover.svg": '<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720"><rect width="1280" height="720" fill="#23784a"/></svg>\n',
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
    await useHandedToken(page);
    await page.waitForSelector("#site-list:not([aria-busy])", { state: "attached" });
    assert.equal(await page.isHidden("#banner"), true, "server info and site list loaded: " + (await page.textContent("#banner")));
    const info = await page.evaluate(() => fetch("/__backyard/api/info").then((r) => r.json()));
    assert.ok(["host", "path"].includes(info.url_mode), "info.url_mode");
    assert.match(await page.textContent("#cmd-login"), /^backyard login --server /);
    await page.setInputFiles("#folder-input", folder);
    await page.waitForFunction(() => !document.getElementById("deploy-btn").disabled, null, { timeout: 30_000 });
    assert.equal(await page.inputValue("#site-name"), name);
    assert.match(await page.textContent("#selection-summary"), new RegExp("^" + name + " · 4 files · "));
    await page.click("#deploy-btn");
    await page.waitForSelector("#result:not([hidden]), #deploy-error:not([hidden])", { timeout: 60_000 });
    assert.equal(await page.isHidden("#deploy-error"), true, "deploy failed: " + (await page.textContent("#deploy-error")));
    deployed = true;
    const siteURL = await page.textContent("#result-link");
    assert.match(await page.textContent("#progress-text"), /^Deployed · 4 uploaded · 0 unchanged/);
    await page.waitForSelector(`.site[data-site="${name}"]`);
    assert.equal(await page.textContent(`.site[data-site="${name}"] .site-title a`), "E2E site", "title extracted from index.html");
    // The card shows the site's preview (its og:image), loaded from the hub's own origin.
    const preview = `.site[data-site="${name}"] .site-preview img`;
    await page.waitForFunction((sel) => document.querySelector(sel)?.naturalWidth > 0, preview, { timeout: 15_000 });
    const src = await page.getAttribute(preview, "src");
    assert.match(src, new RegExp(`^/__backyard/api/sites/${name}/preview\\?v=.+`));
    const served = await page.evaluate((u) => fetch(u).then((r) => [r.status, r.headers.get("content-type"), r.headers.get("content-security-policy")]), src);
    assert.deepEqual(served, [200, "image/svg+xml", "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'"]);
    // Re-deploying the same folder uploads nothing.
    await page.click("#deploy-btn");
    await page.waitForFunction(() => /^Deployed · 0 uploaded · 4 unchanged/.test(document.getElementById("progress-text").textContent), null, { timeout: 60_000 });

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

    const r = await a.page.evaluate(async ({ slowPrompt, imageN }) => {
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
        out.ai = { default: models.default, kinds: models.models.map((m) => m.kind).sort() };
        // A stream whose model stays silent past the server's keep-alive
        // interval: the headers come at once, then ": ping" comments (read raw
        // here), and the SDK still delivers the answer. Runs in the background.
        let slow = null;
        if (slowPrompt) {
          const started = Date.now();
          const raw = fetch(backyard.site.apiBase + "/api/ai/chat", {
            method: "POST",
            headers: { "Content-Type": "application/json", "X-Backyard": "1", Accept: "text/event-stream" },
            body: JSON.stringify({ messages: [{ role: "user", content: slowPrompt }], stream: true }),
          }).then(async (res) => {
            const headersAfter = Date.now() - started;
            return { status: res.status, type: res.headers.get("content-type"), headersAfter, text: await res.text() };
          });
          const s = backyard.ai.stream(slowPrompt);
          slow = Promise.all([raw, s.done.then((d) => ({ content: d.content, stop: d.stop_reason }))]);
        }
        out.ai.chat = (await backyard.ai.chat("ping")).content;
        const s = backyard.ai.stream("hi", { onText: () => {} });
        const chunks = [];
        for await (const c of s) chunks.push(c.text);
        const final = await s.done;
        out.ai.stream = { joined: chunks.join(""), content: final.content, chunks: chunks.length, stop: final.stop_reason };
        out.ai.json = (await backyard.ai.chat("x", { json: true }).catch((e) => ({ json: e.code }))).json;
        const img = await backyard.ai.image("a dot", { n: imageN }).catch((e) => ({ error: e.code }));
        out.ai.image = img.error
          ? img
          : {
              ok: typeof img.url === "string" && img.images.length >= 1,
              n: img.images.length,
              idOK: typeof img.id === "string" && img.id === img.images[0].id && img.url.includes("/__backyard/files/" + img.id + "/"),
              type: img.content_type,
              model: img.model,
              file: img.id ? ((await backyard.files.get(img.id)) || {}).content_type : undefined,
            };
        if (img.url) out.ai.imageFetch = await fetch(img.url).then((x) => [x.status, x.headers.get("content-type")]);
        const emb = await backyard.ai.embed(["a", "bb"]).catch((e) => ({ error: e.code }));
        out.ai.embed = Array.isArray(emb) ? emb.length : emb;
        out.ai.embedOne = await backyard.ai.embed("abc").catch((e) => ({ error: e.code }));
        out.ai.embedMany = emb;
        const page = await backyard.files.list({ limit: 1 });
        out.ai.filesPage = { n: page.length, hasMore: page.hasMore, keys: Object.keys(page) };
        if (slow) {
          const [raw, sdk] = await slow;
          out.ai.slow = {
            status: raw.status,
            type: raw.type,
            fastHeaders: raw.headersAfter < 5000,
            ping: raw.text.includes(": ping\n\n"),
            done: raw.text.includes('"type":"done"'),
            sdk,
          };
        }
      }

      // Warehouse (optional).
      const wh = await backyard.warehouse.query("SELECT 1 AS n").catch((e) => e);
      out.warehouse = wh instanceof Error ? { code: wh.code, status: wh.status } : { rows: wh.rows, row_count: wh.row_count };
      return out;
    }, { slowPrompt: goServer ? "Answer " + SLOW : "", imageN: goServer ? 2 : 1 });

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
    if (goServer) {
      // Our own server has the fake AI upstream: every AI response shape is checked.
      assert.deepEqual(r.ai.kinds, ["chat", "embedding", "image"]);
      assert.equal(r.ai.default, "smart");
      assert.equal(r.ai.chat, "echo: ping");
      assert.deepEqual(r.ai.json, { ok: true });
      assert.equal(r.ai.stream.content, "echo: hi");
      assert.deepEqual(r.ai.image, { ok: true, n: 2, idOK: true, type: "image/png", model: "image", file: "image/png" });
      assert.deepEqual(r.ai.imageFetch, [200, "image/png"]);
      assert.deepEqual(r.ai.embedOne, [3, 0.5, -1]);
      assert.deepEqual(r.ai.embedMany, [
        [1, 0.5, -1],
        [2, 0.5, -1],
      ]);
      assert.deepEqual(r.ai.filesPage, { n: 1, hasMore: true, keys: ["0"] });
      assert.deepEqual(r.ai.slow, {
        status: 200,
        type: "text/event-stream",
        fastHeaders: true,
        ping: true,
        done: true,
        sdk: { content: "echo: Answer " + SLOW, stop: "end_turn" },
      });
      if (goServer.warehouse) assert.deepEqual(r.warehouse.rows, [{ n: 1 }]);
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
    // The server answers frames that carry an id with an ack or an error with
    // that id: the SDK routes the errors to the room.
    const roomErrors = await b.page.evaluate(async () => {
      const errs = [];
      room.on("error", (e) => errs.push(e.code));
      room.send("psst", { to: "c_0123456789abcdef" }); // no such member
      room.setState({ big: "x".repeat(9000) }); // over the 8 KiB presence limit
      room.send("still fine");
      await new Promise((res) => setTimeout(res, 1000));
      room.setState({ color: "green" });
      return errs;
    });
    assert.deepEqual(roomErrors, ["not_found", "too_large"]);
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
        await useHandedToken(page);
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
