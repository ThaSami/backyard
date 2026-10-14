// Database API (SPEC §5.2): CRUD, the flattened wire shape, versions,
// listing, validation and limits.
import assert from "node:assert/strict";
import { before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";
import { assertDoc, ULID } from "../lib/shapes.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";
const site = uniqueSite("db");
const db = (p) => env.api(site, "/db" + p);

describe("database", { skip }, () => {
  let me;
  before(async () => {
    await deploySite(client, env, site, { "index.html": "db" });
    me = (await client.get(env.api(site, "/me"))).json;
  });

  test("create returns 201 and the flattened document", async () => {
    const r = expectStatus(await client.post(db("/posts"), { title: "Hello", status: "draft", tags: ["a"], n: 1.5, nested: { k: "v" } }), 201);
    const d = assertDoc(r.json, { data: { title: "Hello", status: "draft", tags: ["a"], n: 1.5, nested: { k: "v" } }, version: 1, by: me.id });
    assert.match(d.id, ULID, "generated ids are ULIDs");
    assert.equal(d._meta.created_by, me.id);
    assert.equal(d._meta.created_at, d._meta.updated_at);
    assert.match(r.header("content-type"), /^application\/json\b/);
    assert.deepEqual(expectStatus(await client.get(db(`/posts/${d.id}`)), 200).json, d, "GET returns the same document");
  });

  test("generated ids are time-sortable", async () => {
    const a = (await client.post(db("/sorted"), { i: 1 })).json.id;
    const b = (await client.post(db("/sorted"), { i: 2 })).json.id;
    assert.ok(a < b, `${a} < ${b}`);
  });

  test("create with an id; a duplicate id is 409 conflict", async () => {
    const d = expectStatus(await client.post(db("/posts"), { id: "my-id:1.x_y", a: 1 }), 201).json;
    assertDoc(d, { id: "my-id:1.x_y", data: { a: 1 }, version: 1 });
    assertError(await client.post(db("/posts"), { id: "my-id:1.x_y", a: 2 }), 409, "conflict");
    expectStatus(await client.post(db("/posts"), { id: "", a: 3 }), 201, "an empty id generates one");
    expectStatus(await client.post(db("/posts"), { id: null, a: 4 }), 201, "a null id generates one");
  });

  test('"id" and "_meta" in a write body are not stored as data', async () => {
    const d = expectStatus(await client.post(db("/posts"), { id: "meta-test", _meta: { version: 99, created_by: "mallory" }, x: 1 }), 201).json;
    assertDoc(d, { id: "meta-test", data: { x: 1 }, version: 1, by: me.id });
    const p = expectStatus(await client.put(db("/posts/meta-test"), { _meta: { version: 50 }, y: 2 }), 200).json;
    assertDoc(p, { id: "meta-test", data: { y: 2 }, version: 2 });
  });

  test("PUT creates (version 1) or replaces (version + 1)", async () => {
    const c = expectStatus(await client.put(db("/things/t1"), { a: 1, b: 2 }), 200).json;
    assertDoc(c, { id: "t1", data: { a: 1, b: 2 }, version: 1, by: me.id });
    const r = expectStatus(await client.put(db("/things/t1"), { c: 3 }), 200).json;
    assertDoc(r, { id: "t1", data: { c: 3 }, version: 2 });
    assert.equal(r._meta.created_at, c._meta.created_at, "created_at is kept");
    assert.ok(Date.parse(r._meta.updated_at) >= Date.parse(c._meta.updated_at));
    assertError(await client.put(db("/things/t1"), { id: "other", c: 4 }), 400, "bad_request", "body id differs from the URL");
    expectStatus(await client.put(db("/things/t1"), { id: "t1", c: 5 }), 200, "a matching body id is fine");
  });

  test("PATCH merges shallowly and null deletes a key", async () => {
    await client.put(db("/things/p1"), { a: 1, b: { x: 1, y: 2 }, c: "keep" });
    const d = expectStatus(await client.patch(db("/things/p1"), { a: 2, b: { x: 9 }, c: null, d: [1] }), 200).json;
    assertDoc(d, { id: "p1", data: { a: 2, b: { x: 9 }, d: [1] }, version: 2, by: me.id });
    const e = expectStatus(await client.patch(db("/things/p1"), {}), 200).json;
    assert.equal(e._meta.version, 3, "every write increments the version");
    assertError(await client.patch(db("/things/missing-doc"), { a: 1 }), 404, "not_found");
  });

  test("DELETE is 204, also when missing", async () => {
    await client.put(db("/things/del1"), { a: 1 });
    const r = expectStatus(await client.delete(db("/things/del1")), 204);
    assert.equal(r.body.length, 0);
    expectStatus(await client.delete(db("/things/del1")), 204);
    assertError(await client.get(db("/things/del1")), 404, "not_found");
  });

  test("GET /db lists the site's collections (not system ones)", async () => {
    const r = expectStatus(await client.get(db("")), 200).json;
    assert.ok(Array.isArray(r.collections));
    for (const c of ["posts", "things"]) assert.ok(r.collections.includes(c), `${c} in ${r.collections}`);
    assert.ok(!r.collections.some((c) => c.startsWith("_")), `no system collections: ${r.collections}`);
    const o = uniqueSite("dbother");
    await deploySite(client, env, o, { "index.html": "x" });
    const r2 = expectStatus(await client.get(env.api(o, "/db")), 200).json;
    assert.deepEqual(r2.collections, [], "collections are per site");
    assertError(await client.get(env.api(o, "/db/posts/meta-test")), 404, "not_found", "documents are per site");
  });

  describe("listing", () => {
    before(async () => {
      for (const [id, n, s] of [
        ["l1", 3, "c"],
        ["l2", 1, "a"],
        ["l3", 2, "b"],
        ["l4", 2, "a"],
      ]) {
        expectStatus(await client.put(db(`/list/${id}`), { n, s }), 200);
      }
    });
    const ids = (r) => r.json.docs.map((d) => d.id);

    test("default order is creation order", async () => {
      const r = expectStatus(await client.get(db("/list")), 200);
      assert.deepEqual(ids(r), ["l1", "l2", "l3", "l4"]);
      assert.equal(r.json.has_more, false);
      for (const d of r.json.docs) assertDoc(d);
    });

    test("limit, offset and has_more", async () => {
      const a = expectStatus(await client.get(db("/list?limit=2")), 200).json;
      assert.deepEqual(a.docs.map((d) => d.id), ["l1", "l2"]);
      assert.equal(a.has_more, true);
      const b = expectStatus(await client.get(db("/list?limit=2&offset=2")), 200).json;
      assert.deepEqual(b.docs.map((d) => d.id), ["l3", "l4"]);
      assert.equal(b.has_more, false);
      const c = expectStatus(await client.get(db("/list?offset=10")), 200).json;
      assert.deepEqual(c, { docs: [], has_more: false });
      const d = expectStatus(await client.get(db("/list?limit=0")), 200).json;
      assert.equal(d.docs.length, 4, "limit=0 means the default limit");
      const e = expectStatus(await client.post(db("/list/query"), { limit: 0, offset: 3 }), 200).json;
      assert.deepEqual(e.docs.map((x) => x.id), ["l4"]);
    });

    test("order=field:dir,field2:dir and orderBy=&order=", async () => {
      assert.deepEqual(ids(await client.get(db("/list?order=n:desc,s:asc"))), ["l1", "l4", "l3", "l2"]);
      assert.deepEqual(ids(await client.get(db("/list?order=s,n:desc"))), ["l4", "l2", "l3", "l1"]);
      assert.deepEqual(ids(await client.get(db("/list?orderBy=n&order=desc"))), ["l1", "l3", "l4", "l2"], "ties broken by id");
      assert.deepEqual(ids(await client.get(db("/list?orderBy=s,n&order=asc"))), ["l2", "l4", "l3", "l1"]);
    });

    test("POST /db/{c}/query", async () => {
      const r = expectStatus(
        await client.post(db("/list/query"), { where: [{ field: "n", op: ">=", value: 2 }], order_by: [{ field: "s", dir: "desc" }], limit: 2 }),
        200,
      ).json;
      assert.deepEqual(r.docs.map((d) => d.id), ["l1", "l3"]);
      assert.equal(r.has_more, true);
      const empty = expectStatus(await client.post(db("/never-written/query"), {}), 200).json;
      assert.deepEqual(empty, { docs: [], has_more: false });
    });

    test("system fields are queryable", async () => {
      const all = (await client.get(db("/list"))).json.docs;
      const r = expectStatus(await client.post(db("/list/query"), { where: [{ field: "_meta.version", op: "==", value: 1 }, { field: "_meta.created_by", op: "==", value: me.id }] }), 200).json;
      assert.deepEqual(r.docs.map((d) => d.id), ["l1", "l2", "l3", "l4"]);
      const t = all[1]._meta.created_at;
      const r2 = expectStatus(await client.post(db("/list/query"), { where: [{ field: "_meta.created_at", op: ">", value: t }] }), 200).json;
      assert.deepEqual(r2.docs.map((d) => d.id), ["l3", "l4"]);
      const r3 = expectStatus(await client.post(db("/list/query"), { where: [{ field: "id", op: "in", value: ["l4", "l2", "zz"] }] }), 200).json;
      assert.deepEqual(r3.docs.map((d) => d.id), ["l2", "l4"]);
    });

    test("invalid list parameters and queries are 400", async () => {
      for (const q of ["limit=-1", "limit=x", "offset=-2", "order=n:sideways", "orderBy=n&order=up"]) {
        assertError(await client.get(db(`/list?${q}`)), 400, "bad_request", q);
      }
      const bad = [
        ["unknown operator", { where: [{ field: "n", op: "~=", value: 1 }] }],
        ["non-scalar value", { where: [{ field: "n", op: "==", value: { a: 1 } }] }],
        ["in without an array", { where: [{ field: "n", op: "in", value: 1 }] }],
        ["in with 31 values", { where: [{ field: "n", op: "in", value: Array.from({ length: 31 }, (_, i) => i) }] }],
        ["in with an empty array", { where: [{ field: "n", op: "in", value: [] }] }],
        ["bad field", { where: [{ field: "a..b", op: "==", value: 1 }] }],
        ["field too deep", { where: [{ field: "a.b.c.d.e.f.g.h.i", op: "==", value: 1 }] }],
        ["bad order direction", { order_by: [{ field: "n", dir: "up" }] }],
        ["negative limit", { limit: -1 }],
        ["unknown query key", { wher: [] }],
        ["21 filters", { where: Array.from({ length: 21 }, () => ({ field: "n", op: "==", value: 1 })) }],
      ];
      for (const [what, q] of bad) assertError(await client.post(db("/list/query"), q), 400, "bad_request", what);
    });

    test("limit is capped by database.max_list", async () => {
      const r = expectStatus(await client.get(db("/list?limit=100000")), 200).json;
      assert.equal(r.docs.length, 4);
    });
  });

  describe("validation and limits", () => {
    test("collection names", async () => {
      for (const c of ["_files", "_private", "-x", "a b", "a".repeat(65), "x.y"]) {
        const r = await client.post(db("/" + encodeURIComponent(c)), { a: 1 });
        assertError(r, 400, "bad_request", `collection ${c}`);
      }
      assertError(await client.get(db("/_files")), 400, "bad_request", "system collections are not readable");
      expectStatus(await client.post(db("/A-z_0-9"), { a: 1 }), 201);
      expectStatus(await client.post(db("/" + "c".repeat(64)), { a: 1 }), 201);
    });

    test("document ids", async () => {
      for (const id of ["a b", "a/b", "x".repeat(129), "é"]) {
        assertError(await client.put(db("/things/" + encodeURIComponent(id)), { a: 1 }), 400, "bad_request", `id ${id}`);
        assertError(await client.post(db("/things"), { id, a: 1 }), 400, "bad_request", `body id ${id}`);
      }
      assertError(await client.post(db("/things"), { id: 5 }), 400, "bad_request", "numeric id");
      expectStatus(await client.put(db("/things/" + "x".repeat(128)), { a: 1 }), 200, "128-char id");
    });

    test("write bodies must be JSON objects", async () => {
      for (const body of ["[]", "1", '"s"', "null"]) {
        assertError(await client.put(db("/things/w1"), body, { raw: true }), 400, "bad_request", body);
      }
    });

    test("documents are at most 100 levels deep and contain no U+0000", async () => {
      const nest = (n) => (n === 0 ? 1 : { a: nest(n - 1) });
      expectStatus(await client.post(db("/deep"), { v: nest(99) }), 201, "100 levels");
      assertError(await client.post(db("/deep"), { v: nest(100) }), 400, "bad_request", "101 levels");
      const arr = (n) => (n === 0 ? 1 : [arr(n - 1)]);
      assertError(await client.post(db("/deep"), { v: arr(100) }), 400, "bad_request", "101 levels of arrays");
      assertError(await client.post(db("/deep"), { v: "nul\u0000byte" }), 400, "bad_request", "U+0000 in a string");
      assertError(await client.post(db("/deep"), { "k\u0000": 1 }), 400, "bad_request", "U+0000 in a key");
      assertError(await client.patch(db("/things/p1"), { v: "x\u0000" }), 400, "bad_request", "U+0000 in a patch");
    });

    test("a document above database.max_doc_bytes is 413 too_large", async () => {
      const r = await client.post(db("/big"), { blob: "x".repeat(300 * 1024) });
      assertError(r, 413, "too_large");
      expectStatus(await client.post(db("/big"), { blob: "x".repeat(200 * 1024) }), 201, "200 KiB fits the default 256 KiB");
    });

    test("values round-trip exactly", async () => {
      const data = {
        s: "text with \"quotes\", \\ and unicode é😀",
        int: 42,
        neg: -7,
        float: 0.1,
        big: 1e21,
        small: 5e-324,
        t: true,
        f: false,
        nul: null,
        arr: [1, "two", null, [3], { four: 4 }],
        obj: { a: { b: { c: [] } }, empty: {} },
        "key with spaces": 1,
        "": "empty key",
      };
      const d = expectStatus(await client.post(db("/roundtrip"), data), 201).json;
      assertDoc(d, { data });
      assertDoc(expectStatus(await client.get(db(`/roundtrip/${d.id}`)), 200).json, { data });
    });
  });
});
