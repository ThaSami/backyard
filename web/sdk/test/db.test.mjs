// backyard.db request shapes (SPEC §5.2, §5.6).
import { test } from "node:test";
import assert from "node:assert/strict";
import { createEnv, json, apiError, rejects, plain } from "./helpers/harness.mjs";

const doc = (id, data = {}) => ({
  id,
  ...data,
  _meta: {
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    created_by: "alice@acme.com",
    updated_by: "alice@acme.com",
    version: 1,
  },
});

test("collections() lists collection names", async () => {
  const env = createEnv();
  env.on("GET", "/__backyard/api/db", { collections: ["posts", "votes"] });
  assert.deepEqual(plain(await env.backyard.db.collections()), ["posts", "votes"]);
});

test("collection() is synchronous, cached, and validates its argument", () => {
  const env = createEnv();
  const a = env.backyard.db.collection("posts");
  assert.equal(a, env.backyard.db.collection("posts"));
  assert.equal(a.name, "posts");
  assert.throws(() => env.backyard.db.collection(""), { name: "BackyardError", code: "bad_request" });
});

test("create posts the data (optional id) and returns the flattened doc", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/db/posts", (req) => json(201, doc(req.json.id || "01JGENERATED", req.json)));
  const posts = env.backyard.db.collection("posts");

  const d = await posts.create({ title: "Hello", tags: ["a"] });
  assert.deepEqual(env.last().json, { title: "Hello", tags: ["a"] });
  assert.equal(d.id, "01JGENERATED");
  assert.equal(d.title, "Hello");
  assert.equal(d._meta.version, 1);

  await posts.create({ title: "x" }, { id: "custom-1" });
  assert.deepEqual(env.last().json, { title: "x", id: "custom-1" });

  // _meta from a previously read document is never sent back.
  await posts.create({ ...doc("y", { title: "copy" }) });
  assert.deepEqual(env.last().json, { id: "y", title: "copy" });

  for (const bad of [null, "str", [1], 3]) {
    const err = await rejects(posts.create(bad));
    assert.equal(err.code, "bad_request");
  }
});

test("create conflict surfaces 409", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/db/posts", apiError(409, "conflict", "document exists"));
  const err = await rejects(env.backyard.db.collection("posts").create({}, { id: "dup" }));
  assert.equal(err.code, "conflict");
  assert.equal(err.status, 409);
});

test("get returns the doc, null on 404, and throws other errors", async () => {
  const env = createEnv();
  const posts = env.backyard.db.collection("posts");
  env.on("GET", "/__backyard/api/db/posts/p1", doc("p1", { title: "t" }));
  env.on("GET", "/__backyard/api/db/posts/missing", apiError(404, "not_found", "no such document"));
  env.on("GET", "/__backyard/api/db/posts/boom", apiError(500, "internal", "boom"));
  assert.equal((await posts.get("p1")).title, "t");
  assert.equal(await posts.get("missing"), null);
  assert.equal((await rejects(posts.get("boom"))).code, "internal");
});

test("ids and collection names are URL-encoded; invalid ids are rejected", async () => {
  const env = createEnv();
  env.on("GET", /.*/, (req) => json(200, { path: req.url }));
  const c = env.backyard.db.collection("my posts");
  await c.get("a:b/c");
  assert.equal(env.last().url, "https://foo.backyard.test/__backyard/api/db/my%20posts/a%3Ab%2Fc");
  await c.get(42);
  assert.equal(env.last().path, "/__backyard/api/db/my%20posts/42");
  for (const bad of [undefined, null, "", ".", "..", {}]) {
    assert.equal((await rejects(c.get(bad))).code, "bad_request");
  }
});

test("set PUTs, update PATCHes (null deletes), delete resolves to undefined", async () => {
  const env = createEnv();
  const posts = env.backyard.db.collection("posts");
  env.on("PUT", "/__backyard/api/db/posts/p1", (req) => doc("p1", req.json));
  env.on("PATCH", "/__backyard/api/db/posts/p1", (req) => doc("p1", req.json));
  env.on("DELETE", "/__backyard/api/db/posts/p1", json(204));

  const s = await posts.set("p1", { ...doc("p1", { title: "A" }), title: "B" });
  assert.deepEqual(env.last().json, { id: "p1", title: "B" });
  assert.equal(s.title, "B");

  await posts.update("p1", { status: "done", draft: null });
  assert.equal(env.last().method, "PATCH");
  assert.deepEqual(env.last().json, { status: "done", draft: null });

  assert.equal(await posts.delete("p1"), undefined);
  assert.equal(env.last().method, "DELETE");
  assert.equal(env.last().body, undefined);
});

test("list maps options to the query string and returns docs with hasMore", async () => {
  const env = createEnv();
  env.on("GET", "/__backyard/api/db/posts", { docs: [doc("a"), doc("b")], has_more: true });
  const posts = env.backyard.db.collection("posts");

  const cases = [
    [undefined, {}],
    [{}, {}],
    [{ limit: 20, offset: 40 }, { limit: "20", offset: "40" }],
    [{ orderBy: "votes", order: "desc" }, { order: "votes:desc" }],
    [{ orderBy: "votes" }, { order: "votes:asc" }],
    [{ orderBy: ["votes", "title"], order: "desc" }, { order: "votes:desc,title:desc" }],
    [{ orderBy: "votes:desc" }, { order: "votes:desc" }],
    [{ order: "desc" }, { order: "_meta.created_at:desc,id:desc" }],
    [{ order: "asc" }, { order: "_meta.created_at:asc,id:asc" }],
    [{ limit: 0 }, { limit: "0" }],
  ];
  for (const [opts, query] of cases) {
    const docs = await posts.list(opts);
    assert.deepEqual(env.last().query, query, JSON.stringify(opts));
    assert.equal(docs.length, 2);
    assert.equal(docs.hasMore, true);
    assert.deepEqual(Object.keys(docs), ["0", "1"], "hasMore is not enumerable");
  }
  assert.equal((await rejects(posts.list({ orderBy: "x", order: "sideways" }))).code, "bad_request");
});

test("where/orderBy/limit/offset chain → POST /db/{c}/query; queries are immutable", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/db/posts/query", { docs: [doc("a", { votes: 5 })], has_more: false });
  const posts = env.backyard.db.collection("posts");

  const base = posts.where("status", "==", "draft");
  const q = base.where("votes", ">=", 3).orderBy("votes", "desc").orderBy("id").limit(20).offset(0);
  const docs = await q.get();
  assert.equal(env.last().method, "POST");
  assert.equal(env.last().headers["x-backyard"], "1");
  assert.deepEqual(env.last().json, {
    where: [
      { field: "status", op: "==", value: "draft" },
      { field: "votes", op: ">=", value: 3 },
    ],
    order_by: [
      { field: "votes", dir: "desc" },
      { field: "id", dir: "asc" },
    ],
    limit: 20,
    offset: 0,
  });
  assert.equal(docs[0].votes, 5);
  assert.equal(docs.hasMore, false);

  await base.get();
  assert.deepEqual(env.last().json, { where: [{ field: "status", op: "==", value: "draft" }] }, "base query unchanged");

  await posts.orderBy("title").get();
  assert.deepEqual(env.last().json, { order_by: [{ field: "title", dir: "asc" }] });
  await posts.limit(5).offset(10).get();
  assert.deepEqual(env.last().json, { limit: 5, offset: 10 });
  assert.deepEqual(plain(q.toJSON()).limit, 20);
});

test("every operator is accepted; aliases normalize; unknown ops throw synchronously", () => {
  const env = createEnv();
  const posts = env.backyard.db.collection("posts");
  for (const op of ["==", "!=", "<", "<=", ">", ">=", "in", "not-in", "contains"]) {
    assert.equal(posts.where("f", op, 1).toJSON().where[0].op, op);
  }
  assert.equal(posts.where("f", "=", 1).toJSON().where[0].op, "==");
  assert.equal(posts.where("f", "array-contains", 1).toJSON().where[0].op, "contains");
  assert.throws(() => posts.where("f", "like", 1), { code: "bad_request" });
  assert.throws(() => posts.where("", "==", 1), { code: "bad_request" });
  assert.throws(() => posts.orderBy("f", "up"), { code: "bad_request" });
  assert.throws(() => posts.limit(-1), { code: "bad_request" });
  assert.throws(() => posts.offset("3"), { code: "bad_request" });
});

test("first() asks for one doc and returns null when nothing matches", async () => {
  const env = createEnv();
  const posts = env.backyard.db.collection("posts");
  env.once("POST", "/__backyard/api/db/posts/query", { docs: [doc("a")], has_more: true });
  assert.equal((await posts.where("x", "==", 1).first()).id, "a");
  assert.equal(env.last().json.limit, 1);
  env.once("POST", "/__backyard/api/db/posts/query", { docs: [], has_more: false });
  assert.equal(await posts.where("x", "==", 2).first(), null);
});

test("find() accepts Query instances and plain query objects", async () => {
  const env = createEnv();
  env.on("POST", "/__backyard/api/db/posts/query", { docs: [], has_more: false });
  const posts = env.backyard.db.collection("posts");

  await posts.find({ where: [{ field: "a", op: "in", value: [1, 2] }], order_by: [{ field: "a", dir: "desc" }], limit: 3 });
  assert.deepEqual(env.last().json, { where: [{ field: "a", op: "in", value: [1, 2] }], order_by: [{ field: "a", dir: "desc" }], limit: 3 });

  await posts.find({ where: [["tags", "contains", "x"]], orderBy: ["votes:desc", "title"] });
  assert.deepEqual(env.last().json, {
    where: [{ field: "tags", op: "contains", value: "x" }],
    order_by: [
      { field: "votes", dir: "desc" },
      { field: "title", dir: "asc" },
    ],
  });

  await posts.find(posts.where("a", "!=", null));
  assert.deepEqual(env.last().json, { where: [{ field: "a", op: "!=", value: null }] });

  await posts.find();
  assert.deepEqual(env.last().json, {});

  assert.equal((await rejects(posts.find("status = 1"))).code, "bad_request");
  assert.equal((await rejects(posts.find({ where: "x" }))).code, "bad_request");
});
