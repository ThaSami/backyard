// backyard.files, backyard.id, backyard.warehouse and non-streaming backyard.ai.
import { test } from "node:test";
import assert from "node:assert/strict";
import { createEnv, json, apiError, rejects, plain } from "./helpers/harness.mjs";

const fileInfo = (id, name) => ({
  id,
  name,
  url: `/__backyard/files/${id}/${encodeURIComponent(name)}`,
  size: 5,
  content_type: "text/plain",
  created_at: "2026-01-01T00:00:00Z",
  created_by: "alice@acme.com",
});

test("files.upload sends multipart FormData with the file (and optional name)", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/files", json(201, fileInfo("f1", "notes.txt")));
  const file = new File(["hello"], "notes.txt", { type: "text/plain" });

  const info = await env.backyard.files.upload(file);
  let r = env.last();
  assert.equal(r.method, "POST");
  assert.equal(r.headers["x-backyard"], "1");
  assert.equal(r.headers["content-type"], undefined, "the browser sets the multipart boundary");
  assert.ok(r.body instanceof FormData);
  assert.equal(r.body.get("file").name, "notes.txt");
  assert.equal(await r.body.get("file").text(), "hello");
  assert.equal(r.body.get("name"), null);
  assert.equal(info.id, "f1");
  assert.equal(info.url, "/__backyard/files/f1/notes.txt");

  await env.backyard.files.upload(new Blob(["png"], { type: "image/png" }), { name: "avatar.png" });
  r = env.last();
  assert.equal(r.body.get("name"), "avatar.png");
  assert.equal(r.body.get("file").name, "avatar.png");
  assert.deepEqual([...r.body.keys()], ["name", "file"]);

  assert.equal((await rejects(env.backyard.files.upload("nope"))).code, "bad_request");
  assert.equal((await rejects(env.backyard.files.upload(null))).code, "bad_request");
});

test("files.list/get/delete", async () => {
  const env = createEnv();
  env.on("GET", "/__backyard/api/files", { files: [fileInfo("f2", "b"), fileInfo("f1", "a")] });
  env.on("GET", "/__backyard/api/files/f1", fileInfo("f1", "a"));
  env.on("GET", "/__backyard/api/files/nope", apiError(404, "not_found", "no such file"));
  env.on("DELETE", "/__backyard/api/files/f1", json(204));

  const list = await env.backyard.files.list({ limit: 10, offset: 5 });
  assert.deepEqual(env.last().query, { limit: "10", offset: "5" });
  assert.deepEqual(plain(list.map((f) => f.id)), ["f2", "f1"]);
  await env.backyard.files.list();
  assert.equal(env.last().search, "");

  assert.equal((await env.backyard.files.get("f1")).name, "a");
  assert.equal(await env.backyard.files.get("nope"), null);
  assert.equal(await env.backyard.files.delete("f1"), undefined);
  assert.equal(env.last().method, "DELETE");
  assert.equal(env.last().headers["x-backyard"], "1");
});

test("id.me is cached, retried after failure, and aliased as backyard.me", async () => {
  const env = createEnv();
  let calls = 0;
  env.on("GET", "/__backyard/api/me", () => {
    calls++;
    return calls === 1 ? apiError(500, "internal", "flaky") : json(200, { id: "alice@acme.com", name: "Alice", anonymous: false });
  });
  assert.equal((await rejects(env.backyard.id.me())).code, "internal");
  const [a, b] = await Promise.all([env.backyard.id.me(), env.backyard.me()]);
  assert.equal(a.name, "Alice");
  assert.equal(a, b);
  await env.backyard.me();
  assert.equal(calls, 2, "one failed + one successful request");
});

test("id.lookup encodes the email and returns null on 404", async () => {
  const env = createEnv();
  env.on("GET", /\/users\//, apiError(404, "not_found", "unknown user"));
  env.on("GET", "/__backyard/api/users/bob%2Bx%40acme.com", { id: "bob+x@acme.com", name: "Bob" });
  assert.equal((await env.backyard.id.lookup(" bob+x@acme.com ")).name, "Bob");
  assert.equal(await env.backyard.id.lookup("ghost@acme.com"), null);
  assert.equal((await rejects(env.backyard.id.lookup(""))).code, "bad_request");
});

test("warehouse.query/named/sources", async () => {
  const env = createEnv();
  const result = { columns: [{ name: "day", type: "DATE" }], rows: [{ day: "2026-01-01" }], row_count: 1, truncated: false };
  env.on("POST", "/__backyard/api/warehouse/query", result);
  env.on("GET", "/__backyard/api/warehouse", { sources: [{ name: "default", type: "bigquery" }], queries: [] });

  const r = await env.backyard.warehouse.query("SELECT * FROM t WHERE team = ?", ["growth"], { source: "bq" });
  assert.deepEqual(env.last().json, { sql: "SELECT * FROM t WHERE team = ?", params: ["growth"], source: "bq" });
  assert.equal(env.last().headers["x-backyard"], "1");
  assert.equal(r.row_count, 1);

  await env.backyard.warehouse.query("SELECT 1");
  assert.deepEqual(env.last().json, { sql: "SELECT 1", params: [] });

  await env.backyard.warehouse.named("weekly_active_users", { team: "growth" });
  assert.deepEqual(env.last().json, { query: "weekly_active_users", params: { team: "growth" } });
  await env.backyard.warehouse.named("totals");
  assert.deepEqual(env.last().json, { query: "totals", params: {} });

  assert.equal((await env.backyard.warehouse.sources()).sources[0].type, "bigquery");
  assert.equal(env.last().method, "GET");

  assert.equal((await rejects(env.backyard.warehouse.query("SELECT ?", { a: 1 }))).code, "bad_request");
  assert.equal((await rejects(env.backyard.warehouse.named("x", [1]))).code, "bad_request");
  assert.equal((await rejects(env.backyard.warehouse.query(""))).code, "bad_request");
});

test("warehouse on the Cloudflare edition surfaces 501 unsupported", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/warehouse/query", apiError(501, "unsupported", "the warehouse is not available"));
  const err = await rejects(env.backyard.warehouse.query("SELECT 1"));
  assert.equal(err.code, "unsupported");
  assert.equal(err.status, 501);
});

test("ai.chat maps input and options to the wire body", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/ai/chat", (req) =>
    json(200, { content: req.json.json ? '```json\n{"ok": true}\n```' : "hi", model: "smart", stop_reason: "end_turn", usage: { input_tokens: 1, output_tokens: 2 } }),
  );

  let r = await env.backyard.ai.chat("Summarize");
  assert.deepEqual(env.last().json, { messages: [{ role: "user", content: "Summarize" }] });
  assert.equal(r.content, "hi");
  assert.equal(r.json, undefined);

  const messages = [
    { role: "system", content: "Be brief" },
    { role: "user", content: [{ type: "text", text: "what is this" }, { type: "image", url: "https://x/y.png" }] },
  ];
  r = await env.backyard.ai.chat(messages, { model: "fast", system: "You are terse", maxTokens: 100, temperature: 0, json: true });
  assert.deepEqual(env.last().json, { messages, model: "fast", system: "You are terse", max_tokens: 100, temperature: 0, json: true });
  assert.deepEqual(plain(r.json), { ok: true });
  assert.equal(r.stop_reason, "end_turn");

  await env.backyard.ai.chat({ role: "user", content: "single" });
  assert.deepEqual(env.last().json, { messages: [{ role: "user", content: "single" }] });

  assert.equal((await rejects(env.backyard.ai.chat(42))).code, "bad_request");
});

test("ai.chat json parsing tolerates prose and returns undefined for non-JSON", async () => {
  const env = createEnv();
  const replies = ['Sure! Here it is: {"a": [1, 2]} hope that helps', "[1,2,3]", "not json at all"];
  let i = 0;
  env.on("POST", "/__backyard/api/ai/chat", () => json(200, { content: replies[i++], model: "smart", stop_reason: "end_turn", usage: {} }));
  assert.deepEqual(plain((await env.backyard.ai.chat("x", { json: true })).json), { a: [1, 2] });
  assert.deepEqual(plain((await env.backyard.ai.chat("x", { json: true })).json), [1, 2, 3]);
  assert.equal((await env.backyard.ai.chat("x", { json: true })).json, undefined);
});

test("ai.image returns the first image plus the full list", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/ai/image", {
    images: [
      { url: "/__backyard/files/i1/image.png", content_type: "image/png" },
      { url: "/__backyard/files/i2/image.png", content_type: "image/png" },
    ],
  });
  const r = await env.backyard.ai.image("a watercolor fox", { model: "img", size: "512x512", n: 2 });
  assert.deepEqual(env.last().json, { prompt: "a watercolor fox", model: "img", size: "512x512", n: 2 });
  assert.equal(r.url, "/__backyard/files/i1/image.png");
  assert.equal(r.content_type, "image/png");
  assert.equal(r.images.length, 2);
  await env.backyard.ai.image("x");
  assert.deepEqual(env.last().json, { prompt: "x" });
  assert.equal((await rejects(env.backyard.ai.image(""))).code, "bad_request");
});

test("ai.embed mirrors the input shape", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/ai/embed", (req) =>
    json(200, { model: "embed", embeddings: (Array.isArray(req.json.input) ? req.json.input : [req.json.input]).map((_, i) => [i, 0.5]) }),
  );
  assert.deepEqual(plain(await env.backyard.ai.embed("one")), [0, 0.5]);
  assert.deepEqual(env.last().json, { input: "one" });
  assert.deepEqual(plain(await env.backyard.ai.embed(["a", "b"], { model: "embed-large" })), [
    [0, 0.5],
    [1, 0.5],
  ]);
  assert.deepEqual(env.last().json, { input: ["a", "b"], model: "embed-large" });
  assert.equal((await rejects(env.backyard.ai.embed(3))).code, "bad_request");
});

test("ai.models", async () => {
  const env = createEnv();
  env.on("GET", "/__backyard/api/ai/models", { default: "smart", models: [{ name: "smart", kind: "chat", provider: "anthropic" }] });
  const m = await env.backyard.ai.models();
  assert.equal(m.default, "smart");
  assert.equal(env.last().method, "GET");
});
