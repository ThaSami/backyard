// AI endpoints (SPEC §5.4). Capability detection: when GET /ai/models answers
// 501 unsupported, every AI endpoint must do the same. Provider calls run only
// against the deterministic mock provider (BACKYARD_CONFORMANCE_AI=mock, set
// by the runners, which back the aliases with conformance/mock/openai.mjs) or
// when BACKYARD_CONFORMANCE_AI=live (real provider; costs tokens).
import assert from "node:assert/strict";
import { before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";
import { assertFileInfo } from "../lib/shapes.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";
const site = uniqueSite("ai");
const api = (p) => env.api(site, p);
const providerMode = (process.env.BACKYARD_CONFORMANCE_AI || "").trim(); // "", "mock" or "live"

/** Parses a text/event-stream body into its data events (ignoring ":" comments). */
function sseEvents(text) {
  const events = [];
  for (const block of text.split("\n\n")) {
    const data = block
      .split("\n")
      .filter((l) => l.startsWith("data:"))
      .map((l) => l.slice(5).trimStart())
      .join("\n");
    if (data) events.push(JSON.parse(data));
  }
  return events;
}

describe("AI", { skip }, () => {
  let models = null; // null: AI disabled
  let byKind = {};

  before(async () => {
    await deploySite(client, env, site, { "index.html": "ai" });
    const r = await client.get(api("/ai/models"));
    if (r.status === 501) {
      assertError(r, 501, "unsupported");
      return;
    }
    models = expectStatus(r, 200).json;
    byKind = {};
    for (const m of models.models) (byKind[m.kind] ??= []).push(m.name);
  });

  test("GET /ai/models: {default, models: [ModelInfo]} or 501 unsupported", (t) => {
    if (!models) return t.skip("AI is not configured on this server");
    assert.equal(typeof models.default, "string");
    assert.ok(Array.isArray(models.models));
    for (const m of models.models) {
      assert.equal(typeof m.name, "string");
      assert.ok(["chat", "image", "embedding"].includes(m.kind), `kind ${m.kind}`);
      assert.equal(typeof m.provider, "string");
      for (const k of Object.keys(m)) assert.ok(["name", "kind", "provider", "model", "description"].includes(k), `unexpected ModelInfo key ${k}`);
    }
    assert.ok(models.models.some((m) => m.name === models.default && m.kind === "chat"), "the default is a chat alias");
  });

  test("with AI disabled every endpoint is 501 unsupported", async (t) => {
    if (models) return t.skip("AI is configured");
    assertError(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "hi" }] }), 501, "unsupported");
    assertError(await client.post(api("/ai/image"), { prompt: "a cat" }), 501, "unsupported");
    assertError(await client.post(api("/ai/embed"), { input: "x" }), 501, "unsupported");
  });

  test("request validation", async (t) => {
    if (!models) return t.skip("AI is not configured on this server");
    assertError(await client.post(api("/ai/chat"), { messages: [] }), 400, "bad_request", "no messages");
    assertError(await client.post(api("/ai/chat"), { messages: [{ role: "robot", content: "hi" }] }), 400, "bad_request", "bad role");
    assertError(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "hi" }], model: "no-such-alias" }), 404, "not_found", "unknown alias");
    assertError(await client.post(api("/ai/chat"), "{", { raw: true }), 400, "bad_request", "invalid JSON");
    const hi = [{ role: "user", content: "hi" }];
    assertError(await client.post(api("/ai/chat"), { messages: hi, max_tokens: -1 }), 400, "bad_request", "negative max_tokens");
    assertError(await client.post(api("/ai/chat"), { messages: hi, max_tokens: "many" }), 400, "bad_request", "max_tokens not a number");
    const withImage = (url) => ({ messages: [{ role: "user", content: [{ type: "text", text: "see" }, { type: "image", url }] }] });
    for (const url of [`${env.sitePrefix(site)}/__backyard/files/01J0000000000000000000000/x.png`, "x.png", "ftp://example.com/x.png"]) {
      assertError(await client.post(api("/ai/chat"), withImage(url)), 400, "bad_request", `image url ${url}`);
    }
    assertError(await client.post(api("/ai/chat"), withImage(undefined)), 400, "bad_request", "image without data or url");
    if (byKind.image?.length) {
      assertError(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "hi" }], model: byKind.image[0] }), 400, "bad_request", "image alias for chat");
      assertError(await client.post(api("/ai/image"), { prompt: "" }), 400, "bad_request", "empty prompt");
    }
    if (byKind.embedding?.length) {
      assertError(await client.post(api("/ai/embed"), { input: [] }), 400, "bad_request", "empty input");
      assertError(await client.post(api("/ai/embed"), { input: 5 }), 400, "bad_request", "numeric input");
    }
  });

  describe("provider calls", { skip: !providerMode && "set BACKYARD_CONFORMANCE_AI=mock|live to call the provider" }, () => {
    const mock = providerMode === "mock";

    test("chat", async (t) => {
      if (!models) return t.skip("AI is not configured");
      const r = expectStatus(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "hello there" }], max_tokens: 50 }), 200).json;
      assert.deepEqual(Object.keys(r).sort(), ["content", "model", "stop_reason", "usage"]);
      assert.equal(r.model, models.default, "model is the alias");
      assert.ok(["end_turn", "max_tokens", "stop_sequence", "refusal", "other"].includes(r.stop_reason));
      assert.ok(Number.isInteger(r.usage.input_tokens) && Number.isInteger(r.usage.output_tokens));
      if (mock) {
        assert.equal(r.content, "mock: hello there");
        assert.equal(r.stop_reason, "end_turn");
        assert.deepEqual(r.usage, { input_tokens: 7, output_tokens: 5 });
        const sys = expectStatus(await client.post(api("/ai/chat"), { system: "be brief", messages: [{ role: "system", content: "hoisted" }, { role: "user", content: [{ type: "text", text: "parts" }] }] }), 200).json;
        assert.equal(sys.content, "mock: parts");
        const len = expectStatus(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "MOCK_LENGTH" }] }), 200).json;
        assert.equal(len.stop_reason, "max_tokens", "finish_reason length is normalized");
        const js = expectStatus(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "as json" }], json: true }), 200).json;
        assert.deepEqual(JSON.parse(js.content), { echo: "as json" });
        assertError(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "MOCK_FAIL" }] }), 502, "upstream_error", "provider failure");
      } else {
        assert.equal(typeof r.content, "string");
      }
    });

    test("chat streaming (server-sent events)", async (t) => {
      if (!models) return t.skip("AI is not configured");
      const r = expectStatus(await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "stream me" }], stream: true }), 200);
      assert.match(r.header("content-type"), /^text\/event-stream\b/);
      const events = sseEvents(r.text);
      const done = events.at(-1);
      assert.equal(done.type, "done", r.text);
      assert.equal(done.model, models.default);
      assert.ok(done.usage && Number.isInteger(done.usage.input_tokens));
      const text = events.filter((e) => e.type === "text").map((e) => e.text).join("");
      for (const e of events.slice(0, -1)) assert.equal(e.type, "text", JSON.stringify(e));
      if (mock) {
        assert.equal(text, "mock: stream me");
        assert.equal(done.stop_reason, "end_turn");
        const bad = await client.post(api("/ai/chat"), { messages: [{ role: "user", content: "MOCK_FAIL" }], stream: true });
        if (bad.status === 200) {
          const last = sseEvents(bad.text).at(-1);
          assert.equal(last.type, "error", bad.text);
          assert.equal(last.error.code, "upstream_error");
        } else {
          assertError(bad, 502, "upstream_error");
        }
      }
    });

    test("embeddings mirror the input shape", async (t) => {
      if (!models || !byKind.embedding?.length) return t.skip("no embedding alias");
      const one = expectStatus(await client.post(api("/ai/embed"), { input: "abcd" }), 200).json;
      assert.equal(one.embeddings.length, 1);
      assert.ok(byKind.embedding.includes(one.model), "model is an embedding alias");
      assert.ok(Number.isInteger(one.usage.input_tokens));
      assert.equal(one.usage.output_tokens, 0);
      const many = expectStatus(await client.post(api("/ai/embed"), { input: ["a", "bb", "ccc"] }), 200).json;
      assert.equal(many.embeddings.length, 3);
      if (mock) {
        assert.deepEqual(one.embeddings, [[4, 1, 0.5]]);
        assert.deepEqual(many.embeddings.map((v) => v[0]), [1, 2, 3]);
      }
    });

    test("images are stored as site files", async (t) => {
      if (!models || !byKind.image?.length || !mock) return t.skip("image generation runs against the mock provider only");
      const r = expectStatus(await client.post(api("/ai/image"), { prompt: "a dot", n: 2 }), 200).json;
      assert.ok(byKind.image.includes(r.model));
      assert.equal(r.images.length, 2);
      const files = (await client.get(api("/files?limit=1000"))).json.files;
      for (const img of r.images) {
        assert.deepEqual(Object.keys(img).sort(), ["content_type", "id", "url"]);
        assert.equal(img.content_type, "image/png");
        const f = files.find((x) => x.id === img.id);
        assert.ok(f, "generated images appear in GET /files");
        assertFileInfo(f, { prefix: env.sitePrefix(site) });
        assert.equal(f.url, img.url);
        const bytes = expectStatus(await client.get(new URL(img.url, env.siteURL(site)).href), 200);
        assert.equal(bytes.header("content-type"), "image/png");
      }
      assertError(await client.post(api("/ai/image"), { prompt: "x", n: 99 }), 400, "bad_request", "too many images");
    });
  });
});
