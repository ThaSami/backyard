// HTTP behaviour shared by every method: headers, CSRF header, errors,
// autoLogin redirects, Retry-After, network failures.
import { test } from "node:test";
import assert from "node:assert/strict";
import vm from "node:vm";
import { createEnv, json, apiError, rejects, plain } from "./helpers/harness.mjs";

test("GET sends no CSRF header; writes send X-Backyard: 1 and JSON; credentials same-origin", async () => {
  const env = createEnv();
  const posts = env.backyard.db.collection("posts");
  env.on("*", /.*/, (req) => (req.method === "DELETE" ? json(204) : json(200, { id: "p1" })));

  await posts.get("p1");
  let r = env.last();
  assert.equal(r.method, "GET");
  assert.equal(r.headers["x-backyard"], undefined);
  assert.equal(r.headers["accept"], "application/json");
  assert.equal(r.credentials, "same-origin");
  assert.equal(r.body, undefined);

  for (const [method, call] of [
    ["POST", () => posts.create({ a: 1 })],
    ["PUT", () => posts.set("p1", { a: 1 })],
    ["PATCH", () => posts.update("p1", { a: 1 })],
    ["DELETE", () => posts.delete("p1")],
  ]) {
    await call();
    r = env.last();
    assert.equal(r.method, method);
    assert.equal(r.headers["x-backyard"], "1", method + " carries the CSRF header");
    assert.equal(r.credentials, "same-origin");
    if (method !== "DELETE") {
      assert.equal(r.headers["content-type"], "application/json");
      assert.deepEqual(r.json, { a: 1 });
    }
  }
});

test("API errors become BackyardError {code, status, message}", async () => {
  const env = createEnv();
  env.on("GET", "/__backyard/api/db/posts/x", apiError(403, "forbidden", "nope"));
  const err = await rejects(env.backyard.db.collection("posts").get("x"));
  assert.ok(err instanceof env.backyard.BackyardError);
  assert.ok(err instanceof vm.runInContext("Error", env.sandbox), "a real Error of the page realm");
  assert.ok(typeof err.stack === "string");
  assert.equal(err.name, "BackyardError");
  assert.equal(err.code, "forbidden");
  assert.equal(err.status, 403);
  assert.equal(err.message, "nope");
  assert.equal(err.loginUrl, undefined);
});

const statusCases = [
  [400, "bad_request"],
  [401, "unauthenticated"],
  [403, "forbidden"],
  [404, "not_found"],
  [409, "conflict"],
  [413, "too_large"],
  [429, "rate_limited"],
  [500, "internal"],
  [501, "unsupported"],
  [502, "upstream_error"],
  [503, "upstream_error"],
  [504, "upstream_error"],
];
for (const [status, code] of statusCases) {
  test(`non-JSON ${status} maps to ${code}`, async () => {
    const env = createEnv();
    env.on("GET", "/__backyard/api/ai/models", new Response("<html>proxy error</html>", { status, statusText: "Oops" }));
    const err = await rejects(env.backyard.ai.models());
    assert.equal(err.code, code);
    assert.equal(err.status, status);
    assert.match(err.message, new RegExp(String(status)));
  });
}

test("401 with login_url redirects the top-level page once and still rejects", async () => {
  const env = createEnv({ top: "https://portal.example/" });
  const login = "https://backyard.test/__backyard/auth/login?rd=https%3A%2F%2Ffoo.backyard.test%2F";
  env.on("GET", /\/api\//, apiError(401, "unauthenticated", "Authentication required.", { login_url: login }));
  const e1 = await rejects(env.backyard.id.me());
  const e2 = await rejects(env.backyard.db.collections());
  assert.equal(e1.code, "unauthenticated");
  assert.equal(e1.loginUrl, login);
  assert.equal(e2.loginUrl, login);
  assert.deepEqual(env.topRedirects, [login], "top-level page navigated exactly once");
  assert.deepEqual(env.redirects, []);
});

test("401 redirect falls back to the page itself when there is no top", async () => {
  const env = createEnv();
  env.on("GET", /.*/, apiError(401, "unauthenticated", "x", { login_url: "/__backyard/auth/login" }));
  await rejects(env.backyard.id.me());
  assert.deepEqual(env.redirects, ["/__backyard/auth/login"]);
});

test("autoLogin:false (configure or BACKYARD_CONFIG) disables the redirect", async () => {
  for (const make of [
    () => {
      const env = createEnv();
      env.backyard.configure({ autoLogin: false });
      return env;
    },
    () => createEnv({ config: { autoLogin: false } }),
  ]) {
    const env = make();
    env.on("GET", /.*/, apiError(401, "unauthenticated", "x", { login_url: "https://login.example/" }));
    const err = await rejects(env.backyard.id.me());
    assert.equal(err.loginUrl, "https://login.example/");
    assert.deepEqual(env.redirects, []);
  }
});

test("401 without login_url and unsafe login URLs never redirect", async () => {
  const env = createEnv();
  env.once("GET", /.*/, apiError(401, "unauthenticated", "token required"));
  await rejects(env.backyard.id.me());
  env.once("GET", /.*/, apiError(401, "unauthenticated", "x", { login_url: "javascript:alert(1)" }));
  await rejects(env.backyard.id.me());
  // Protocol-relative URLs would leave the origin: never followed.
  for (const bad of ["//evil.example/login", "/\\evil.example/login", "data:text/html,x"]) {
    env.once("GET", /.*/, apiError(401, "unauthenticated", "x", { login_url: bad }));
    const err = await rejects(env.backyard.id.me());
    assert.equal(err.loginUrl, bad);
  }
  assert.deepEqual(env.redirects, []);
});

test("429 surfaces Retry-After (seconds or HTTP date)", async () => {
  const env = createEnv();
  env.once("POST", "/__backyard/api/ai/chat", apiError(429, "rate_limited", "slow down", {}, { "Retry-After": "30" }));
  let err = await rejects(env.backyard.ai.chat("hi"));
  assert.equal(err.code, "rate_limited");
  assert.equal(err.status, 429);
  assert.equal(err.retryAfter, 30);

  const at = new Date(env.clock.now + 90_000).toUTCString();
  env.once("POST", "/__backyard/api/ai/chat", apiError(429, "rate_limited", "slow down", {}, { "Retry-After": at }));
  err = await rejects(env.backyard.ai.chat("hi"));
  assert.ok(err.retryAfter >= 89 && err.retryAfter <= 90, String(err.retryAfter));

  env.once("POST", "/__backyard/api/ai/chat", apiError(429, "rate_limited", "slow down"));
  err = await rejects(env.backyard.ai.chat("hi"));
  assert.equal(err.retryAfter, undefined);
});

test("network failures and aborts", async () => {
  const env = createEnv();
  env.once("GET", /.*/, new TypeError("Failed to fetch"));
  let err = await rejects(env.backyard.ai.models());
  assert.equal(err.code, "network_error");
  assert.equal(err.status, 0);
  assert.equal(err.cause.message, "Failed to fetch");

  env.once("GET", /.*/, new DOMException("The operation was aborted.", "AbortError"));
  err = await rejects(env.backyard.ai.models());
  assert.equal(err.code, "aborted");
});

test("a 200 that is not JSON (e.g. a proxy login page) is a bad_response", async () => {
  const env = createEnv();
  env.on("GET", /.*/, new Response("<!doctype html><title>Sign in</title>", { status: 200, headers: { "Content-Type": "text/html" } }));
  const err = await rejects(env.backyard.ai.models());
  assert.equal(err.code, "bad_response");
  assert.equal(err.status, 200);
});

test("missing fetch is reported as unsupported", async () => {
  const env = createEnv();
  env.sandbox.fetch = undefined;
  const err = await rejects(env.backyard.ai.models());
  assert.equal(err.code, "unsupported");
});

test("BackyardError is constructible by apps", () => {
  const env = createEnv();
  const e = new env.backyard.BackyardError("conflict", "taken", { status: 409 });
  assert.equal(e.code, "conflict");
  assert.equal(e.status, 409);
  assert.equal(e.message, "taken");
  assert.deepEqual(plain({ name: e.name }), { name: "BackyardError" });
});

test("quota and size limits: 413 too_large keeps the server's message", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/db/posts", apiError(413, "too_large", "this site has reached its document quota (10000 documents)"));
  env.on("POST", "/__backyard/api/files", apiError(413, "too_large", "file exceeds the maximum upload size of 104857600 bytes"));
  let err = await rejects(env.backyard.db.collection("posts").create({ a: 1 }));
  assert.deepEqual([err.code, err.status, err.message], ["too_large", 413, "this site has reached its document quota (10000 documents)"]);
  err = await rejects(env.backyard.files.upload(new Blob(["x"]), { name: "x.bin" }));
  assert.deepEqual([err.code, err.status], ["too_large", 413]);
  assert.match(err.message, /maximum upload size/);
});
