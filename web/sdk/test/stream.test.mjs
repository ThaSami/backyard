// backyard.ai.stream: SSE parsing over fetch ReadableStreams (SPEC §5.4).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createEnv, sseResponse, json, apiError, rejects, plain, flush } from "./helpers/harness.mjs";

const ev = (o) => `data: ${JSON.stringify(o)}\n\n`;
const DONE = { type: "done", model: "smart", stop_reason: "end_turn", usage: { input_tokens: 3, output_tokens: 4 } };

async function collect(stream) {
  const out = [];
  for await (const chunk of stream) out.push(chunk.text);
  return out;
}

test("stream posts stream:true and yields text chunks; done resolves with the final result", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const seen = [];
  const s = env.backyard.ai.stream("Write a haiku", { model: "smart", maxTokens: 50, onText: (t) => seen.push(t) });
  await flush();
  assert.deepEqual(env.last().json, { messages: [{ role: "user", content: "Write a haiku" }], model: "smart", max_tokens: 50, stream: true });
  assert.equal(env.last().headers["x-backyard"], "1");
  assert.match(env.last().headers.accept, /^text\/event-stream\b/);
  sse.push(ev({ type: "text", text: "Old pond" }));
  sse.push(": ping\n\n");
  sse.push(ev({ type: "text", text: ", frog" }));
  sse.push(ev(DONE));
  sse.close();
  const chunks = await collect(s);
  assert.deepEqual(chunks, ["Old pond", ", frog"]);
  assert.deepEqual(seen, ["Old pond", ", frog"]);
  const final = await s.done;
  assert.deepEqual(plain(final), { content: "Old pond, frog", model: "smart", stop_reason: "end_turn", usage: { input_tokens: 3, output_tokens: 4 } });
  assert.equal(s.text, "Old pond, frog");
});

test("SSE parsing survives arbitrary chunk boundaries, CRLF and split UTF-8", async () => {
  const body =
    ev({ type: "text", text: "héllo " }) +
    ": keep-alive comment\r\n\r\n" +
    `data: {"type":"text",\r\ndata: "text":"wörld 🌍"}\r\n\r\n` + // multi-line data joined with "\n"
    "event: ignored\n" +
    ev({ type: "text", text: "!" }) +
    ev(DONE);
  const bytes = new TextEncoder().encode(body);
  // Every split size, including splitting inside multi-byte characters and "\r\n".
  for (const size of [1, 2, 3, 5, 7, 13, 64, bytes.length]) {
    const env = createEnv();
    const sse = sseResponse();
    env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
    const s = env.backyard.ai.stream([{ role: "user", content: "x" }]);
    await flush();
    for (let i = 0; i < bytes.length; i += size) sse.push(bytes.slice(i, i + size));
    sse.close();
    const chunks = await collect(s);
    assert.deepEqual(chunks, ["héllo ", "wörld 🌍", "!"], "split size " + size);
    assert.equal((await s.done).content, "héllo wörld 🌍!");
  }
});

test("await done without iterating; several iterators replay all chunks", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const s = env.backyard.ai.stream("x");
  await flush();
  sse.push(ev({ type: "text", text: "a" }) + ev({ type: "text", text: "b" }) + ev(DONE));
  sse.close();
  assert.equal((await s.done).content, "ab");
  assert.deepEqual(await collect(s), ["a", "b"]);
  assert.deepEqual(await collect(s), ["a", "b"]);
});

test("breaking out of the loop does not cancel the stream", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const s = env.backyard.ai.stream("x");
  await flush();
  sse.push(ev({ type: "text", text: "a" }));
  for await (const c of s) {
    assert.equal(c.text, "a");
    break;
  }
  sse.push(ev({ type: "text", text: "b" }) + ev(DONE));
  sse.close();
  assert.equal((await s.done).content, "ab");
});

test("an error event rejects done and throws from the iterator", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const s = env.backyard.ai.stream("x");
  await flush();
  sse.push(ev({ type: "text", text: "partial" }));
  sse.push(ev({ type: "error", error: { code: "upstream_error", message: "provider overloaded" } }));
  sse.close();
  const got = [];
  const err = await rejects(
    (async () => {
      for await (const c of s) got.push(c.text);
    })(),
  );
  assert.deepEqual(got, ["partial"]);
  assert.equal(err.name, "BackyardError");
  assert.equal(err.code, "upstream_error");
  assert.equal(err.message, "provider overloaded");
  assert.equal(await rejects(s.done), err);
});

test("a stream that ends without done is an upstream_error", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const s = env.backyard.ai.stream("x");
  await flush();
  sse.push(ev({ type: "text", text: "cut" }));
  sse.close();
  const err = await rejects(s.done);
  assert.equal(err.code, "upstream_error");
  assert.equal(s.text, "cut");
});

test("HTTP errors before the stream starts reject (with autoLogin handling)", async () => {
  const env = createEnv();
  env.once("POST", "/__backyard/api/ai/chat", apiError(404, "not_found", "unknown model alias"));
  let s = env.backyard.ai.stream("x", { model: "nope" });
  let err = await rejects(collect(s));
  assert.equal(err.code, "not_found");
  assert.equal(await rejects(s.done), err);

  env.once("POST", "/__backyard/api/ai/chat", apiError(401, "unauthenticated", "login", { login_url: "/__backyard/auth/login" }));
  s = env.backyard.ai.stream("x");
  err = await rejects(s.done);
  assert.equal(err.loginUrl, "/__backyard/auth/login");
  assert.deepEqual(env.redirects, ["/__backyard/auth/login"]);
});

test("abort() and opts.signal cancel the stream with code aborted", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const s = env.backyard.ai.stream("x");
  await flush();
  sse.push(ev({ type: "text", text: "a" }));
  await flush();
  s.abort();
  sse.error(new DOMException("The operation was aborted.", "AbortError"));
  const err = await rejects(s.done);
  assert.equal(err.code, "aborted");

  const ctrl = new AbortController();
  ctrl.abort();
  const s2 = env.backyard.ai.stream("x", { signal: ctrl.signal });
  assert.equal((await rejects(s2.done)).code, "aborted");
  assert.equal(env.last().signal.aborted, true);
});

test("a non-streaming JSON answer is delivered as a single chunk", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/ai/chat", json(200, { content: '{"n":1}', model: "smart", stop_reason: "end_turn", usage: {} }));
  const s = env.backyard.ai.stream("x", { json: true });
  assert.deepEqual(await collect(s), ['{"n":1}']);
  const final = await s.done;
  assert.deepEqual(plain(final.json), { n: 1 });
});

test("a buffered event-stream body (no ReadableStream reader) still parses", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/ai/chat", () => {
    const res = new Response(ev({ type: "text", text: "x" }) + ev(DONE), { headers: { "Content-Type": "text/event-stream" } });
    Object.defineProperty(res, "body", { value: null });
    return res;
  });
  const s = env.backyard.ai.stream("x");
  assert.equal((await s.done).content, "x");
  assert.deepEqual(await collect(s), ["x"]);
});

test("stream validates its input synchronously", () => {
  const env = createEnv();
  assert.throws(() => env.backyard.ai.stream(null), { code: "bad_request" });
});

test("the Go server's stream shape: flushed headers, ': ping' keep-alives while the model is silent, then events", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const s = env.backyard.ai.stream("think hard");
  const iter = s[Symbol.asyncIterator]();
  const first = iter.next();
  await flush();
  // Headers arrived (fetch resolved) but nothing else yet; keep-alive comments follow.
  sse.push(": ping\n\n");
  sse.push(":ping\n\n");
  sse.push("\n\n");
  await flush();
  assert.equal(s.text, "");
  let settled = false;
  first.then(() => (settled = true));
  await flush();
  assert.equal(settled, false, "comments and blank lines yield no chunks");
  sse.push(ev({ type: "text", text: "Done" }) + ": ping\n\n" + ev(DONE));
  assert.deepEqual(plain(await first), { value: { text: "Done" }, done: false });
  // The server may keep the connection open briefly after "done": the result does not wait for EOF.
  const final = await s.done;
  assert.deepEqual(plain(final), { content: "Done", model: "smart", stop_reason: "end_turn", usage: { input_tokens: 3, output_tokens: 4 } });
  assert.deepEqual(plain(await iter.next()), { done: true });
});

test("an error event after keep-alives (the model failed mid-stream) rejects with the server's code", async () => {
  const env = createEnv();
  const sse = sseResponse();
  env.on("POST", "/__backyard/api/ai/chat", () => sse.response);
  const s = env.backyard.ai.stream("x");
  await flush();
  sse.push(": ping\n\n");
  sse.push(ev({ type: "error", error: { code: "upstream_error", message: "AI provider error: overloaded" } }));
  const err = await rejects(s.done);
  assert.equal(err.code, "upstream_error");
  assert.equal(err.message, "AI provider error: overloaded");
  assert.equal(err.status, 200);
});
