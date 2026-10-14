/**
 * Database API (SPEC §5.2): CRUD over HTTP and the query semantics table
 * ported from the Go conformance suite (internal/docstore/conformance).
 */
import { env } from "cloudflare:workers";
import { beforeAll, describe, expect, it } from "vitest";
import { normalizeQuery, type RawQuery } from "../src/db/query";
import type { Env } from "../src/env";
import { unwrap } from "../src/http";
import { siteStub } from "../src/stubs";
import { apiHeaders, call, uniqueSite } from "./helpers";

type Doc = Record<string, unknown> & { id: string; _meta: Record<string, unknown> };

const E = env as unknown as Env;

function api(site: string, path: string): string {
  return `/s/${site}/__backyard/api${path}`;
}

async function create(site: string, coll: string, body: unknown): Promise<Response> {
  return call(api(site, `/db/${coll}`), { method: "POST", headers: apiHeaders(), body: JSON.stringify(body) });
}

describe("database HTTP API", () => {
  it("creates, reads, replaces, patches and deletes documents", async () => {
    const site = uniqueSite("db");
    let res = await create(site, "posts", { title: "Hello", status: "draft", id: "p1", _meta: { version: 99 } });
    expect(res.status).toBe(201);
    const d = (await res.json()) as Doc;
    expect(d.id).toBe("p1");
    expect(d.title).toBe("Hello");
    expect(d._meta.version).toBe(1);
    expect(String(d._meta.created_by)).toMatch(/^anon:[0-9a-f]{16}$/);
    expect(d._meta.created_at).toBe(d._meta.updated_at);
    expect(String(d._meta.created_at)).toMatch(/^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d{1,6})?Z$/);
    expect(Object.keys(d)).toEqual(["id", "title", "status", "_meta"]);

    res = await create(site, "posts", { id: "p1", title: "dup" });
    expect(res.status).toBe(409);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("conflict");

    res = await create(site, "posts", { title: "generated" });
    const g = (await res.json()) as Doc;
    expect(g.id).toMatch(/^[0-9A-HJKMNP-TV-Z]{26}$/);

    res = await call(api(site, "/db/posts/p1"));
    expect(res.status).toBe(200);
    expect(((await res.json()) as Doc).title).toBe("Hello");
    expect((await call(api(site, "/db/posts/nope"))).status).toBe(404);

    res = await call(api(site, "/db/posts/p1"), { method: "PUT", headers: apiHeaders(), body: JSON.stringify({ title: "Replaced" }) });
    const put = (await res.json()) as Doc;
    expect(put._meta.version).toBe(2);
    expect(put.status).toBeUndefined();
    expect(put._meta.created_at).toBe(d._meta.created_at);

    res = await call(api(site, "/db/posts/p1"), { method: "PUT", headers: apiHeaders(), body: JSON.stringify({ id: "other" }) });
    expect(res.status).toBe(400);

    res = await call(api(site, "/db/posts/p1"), {
      method: "PATCH",
      headers: apiHeaders(),
      body: JSON.stringify({ votes: 3, title: null, nested: { a: 1 } }),
    });
    const patched = (await res.json()) as Doc;
    expect(patched.votes).toBe(3);
    expect("title" in patched).toBe(false);
    expect(patched.nested).toEqual({ a: 1 });
    expect(patched._meta.version).toBe(3);
    expect((await call(api(site, "/db/posts/missing"), { method: "PATCH", headers: apiHeaders(), body: "{}" })).status).toBe(404);

    // PUT creates when missing.
    res = await call(api(site, "/db/posts/new1"), { method: "PUT", headers: apiHeaders(), body: JSON.stringify({ x: 1 }) });
    expect(((await res.json()) as Doc)._meta.version).toBe(1);

    res = await call(api(site, "/db"));
    expect(await res.json()).toEqual({ collections: ["posts"] });

    res = await call(api(site, "/db/posts/p1"), { method: "DELETE", headers: { "X-Backyard": "1" } });
    expect(res.status).toBe(204);
    res = await call(api(site, "/db/posts/p1"), { method: "DELETE", headers: { "X-Backyard": "1" } });
    expect(res.status).toBe(204);
    expect((await call(api(site, "/db/posts/p1"))).status).toBe(404);
  });

  it("lists with limit, offset, order and has_more", async () => {
    const site = uniqueSite("list");
    for (let i = 0; i < 5; i++) await create(site, "items", { id: `i${i}`, n: i % 3 });
    let res = await call(api(site, "/db/items?limit=2"));
    let page = (await res.json()) as { docs: Doc[]; has_more: boolean };
    expect(page.docs.map((x) => x.id)).toEqual(["i0", "i1"]);
    expect(page.has_more).toBe(true);
    res = await call(api(site, "/db/items?limit=2&offset=4"));
    page = (await res.json()) as { docs: Doc[]; has_more: boolean };
    expect(page.docs.map((x) => x.id)).toEqual(["i4"]);
    expect(page.has_more).toBe(false);
    res = await call(api(site, "/db/items?order=n:desc,id:asc"));
    page = (await res.json()) as { docs: Doc[]; has_more: boolean };
    expect(page.docs.map((x) => x.id)).toEqual(["i2", "i1", "i4", "i0", "i3"]);
    res = await call(api(site, "/db/items?orderBy=n&order=desc"));
    page = (await res.json()) as { docs: Doc[]; has_more: boolean };
    expect(page.docs.map((x) => x.id)).toEqual(["i2", "i1", "i4", "i0", "i3"]);
    expect((await call(api(site, "/db/items?order=n:sideways"))).status).toBe(400);
    expect((await call(api(site, "/db/items?limit=-1"))).status).toBe(400);
    expect((await call(api(site, "/db/items?order=_meta.nope"))).status).toBe(400);
  });

  it("queries with POST /db/{c}/query and rejects malformed queries", async () => {
    const site = uniqueSite("q");
    for (const [id, votes, status] of [
      ["a", 5, "draft"],
      ["b", 1, "draft"],
      ["c", 3, "published"],
    ] as const) {
      await create(site, "posts", { id, votes, status });
    }
    const q = (body: unknown) => call(api(site, "/db/posts/query"), { method: "POST", headers: apiHeaders(), body: JSON.stringify(body) });
    let res = await q({ where: [{ field: "status", op: "==", value: "draft" }], order_by: [{ field: "votes", dir: "desc" }] });
    expect(((await res.json()) as { docs: Doc[] }).docs.map((d) => d.id)).toEqual(["a", "b"]);
    res = await q({ where: [{ field: "votes", op: ">=", value: 3 }], limit: 1 });
    const page = (await res.json()) as { docs: Doc[]; has_more: boolean };
    expect(page.docs.map((d) => d.id)).toEqual(["a"]);
    expect(page.has_more).toBe(true);
    expect((await q({ where: [{ field: "x", op: "~", value: 1 }] })).status).toBe(400);
    expect((await q({ where: [{ field: "x", op: "in", value: [] }] })).status).toBe(400);
    expect((await q({ where: [{ field: "x", op: "==", value: { a: 1 } }] })).status).toBe(400);
    expect((await q({ where: [{ field: "a..b", op: "==", value: 1 }] })).status).toBe(400);
    expect((await q({ where: [{ field: "_meta.x", op: "==", value: 1 }] })).status).toBe(400);
    expect((await q({ bogus: true })).status).toBe(400);
    expect((await q({ limit: -1 })).status).toBe(400);
    expect((await q({ offset: 1_000_000 })).status).toBe(400);
    expect((await q({ order_by: [{ field: "votes", dir: "up" }] })).status).toBe(400);
    expect((await q({ where: Array.from({ length: 21 }, () => ({ field: "a", op: "==", value: 1 })) })).status).toBe(400);
  });

  it("validates collections, ids, bodies and CSRF", async () => {
    const site = uniqueSite("val");
    expect((await create(site, "_files", { a: 1 })).status).toBe(400);
    expect((await create(site, "bad name", { a: 1 })).status).toBe(400);
    expect((await create(site, "ok", { id: "bad id!" })).status).toBe(400);
    expect((await create(site, "ok", { id: 5 })).status).toBe(400);
    expect((await create(site, "ok", [1, 2])).status).toBe(400);
    let res = await call(api(site, "/db/ok"), { method: "POST", headers: apiHeaders(), body: "{nope" });
    expect(res.status).toBe(400);
    res = await call(api(site, "/db/ok"), { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
    expect(res.status).toBe(403);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("X-Backyard");
    res = await call(api(site, "/db/ok"), { method: "POST", headers: apiHeaders({ Origin: "https://evil.example" }), body: "{}" });
    expect(res.status).toBe(403);
    res = await call(api(site, "/db/ok"), { method: "POST", headers: apiHeaders({ Origin: "https://backyard.test" }), body: "{}" });
    expect(res.status).toBe(201);
    res = await create(site, "ok", { big: "x".repeat(300 * 1024) });
    expect(res.status).toBe(413);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("too_large");
    res = await call(api(site, "/db/ok/x"), { method: "POST", headers: apiHeaders(), body: "{}" });
    expect(res.status).toBe(405);
    expect(res.headers.get("Allow")).toBe("GET, HEAD, PUT, PATCH, DELETE");
    expect((await call(api(site, "/nothing"))).status).toBe(404);
  });
});

describe("document validation (docstore.ErrInvalid parity)", () => {
  /** levels nested containers (alternating arrays and objects) around "leaf". */
  const nest = (levels: number): unknown => {
    let v: unknown = "leaf";
    for (let i = 0; i < levels; i++) v = i % 2 === 0 ? [v] : { k: v };
    return v;
  };
  const errorOf = async (res: Response) => ((await res.json()) as { error: { code: string; message: string } }).error;
  const put = (site: string, id: string, body: string, method = "PUT") =>
    call(api(site, `/db/deep/${id}`), { method, headers: apiHeaders(), body });

  it("rejects bodies nested more than 100 levels on every write", async () => {
    const site = uniqueSite("deep");
    let res = await create(site, "deep", { id: "ok", d: nest(99), n: 1 });
    expect(res.status).toBe(201);
    expect(((await res.json()) as Doc).d).toEqual(nest(99));
    res = await create(site, "deep", { d: nest(100), n: 1 });
    expect(res.status).toBe(400);
    expect(await errorOf(res)).toEqual({ code: "bad_request", message: 'field "d": nested deeper than 100 levels' });
    expect((await put(site, "ok", JSON.stringify({ d: nest(100) }))).status).toBe(400);
    expect((await put(site, "ok", JSON.stringify({ x: nest(100) }), "PATCH")).status).toBe(400);
    // Far deeper than any engine can evaluate: still a clean 400.
    expect((await put(site, "ok", `{"d":${"[".repeat(5000)}${"]".repeat(5000)}}`)).status).toBe(400);
    // A string of brackets is not nesting.
    expect((await create(site, "deep", { id: "brackets", x: "[".repeat(1500), n: 1 })).status).toBe(201);
    // The stored documents keep the collection queryable.
    res = await call(api(site, "/db/deep/query"), {
      method: "POST",
      headers: apiHeaders(),
      body: JSON.stringify({ where: [{ field: "n", op: "==", value: 1 }], order_by: [{ field: "d", dir: "desc" }] }),
    });
    expect(res.status).toBe(200);
    expect(((await res.json()) as { docs: Doc[] }).docs.map((d) => d.id)).toEqual(["ok", "brackets"]);
    expect(((await (await call(api(site, "/db/deep/ok"))).json()) as Doc)._meta.version).toBe(1);
  });

  it("rejects U+0000 in strings and keys, and numbers JSON cannot represent", async () => {
    const site = uniqueSite("nul");
    for (const body of ['{"a":"x\\u0000y"}', '{"a":{"b":["ok","\\u0000"]}}', '{"a\\u0000":1}', '{"a":{"k\\u0000":1}}', '{"n":1e400}', '{"n":[-1e999]}']) {
      const res = await call(api(site, "/db/c"), { method: "POST", headers: apiHeaders(), body });
      expect(res.status, body).toBe(400);
      expect((await errorOf(res)).message, body).toMatch(/NUL character \(U\+0000\)|is not valid JSON/);
    }
    expect((await create(site, "c", { id: "d1", a: "fine" })).status).toBe(201);
    expect((await put(site, "x", '{"a":"\\u0000"}')).status).toBe(400);
    let res = await call(api(site, "/db/c/d1"), { method: "PATCH", headers: apiHeaders(), body: '{"a":"\\u0000"}' });
    expect(res.status).toBe(400);
    expect(((await (await call(api(site, "/db/c/d1"))).json()) as Doc).a).toBe("fine");
    expect(((await (await call(api(site, "/db"))).json()) as { collections: string[] }).collections).toEqual(["c"]);

    // Lone surrogates become U+FFFD (as encoding/json decodes them), -0 is 0.
    res = await call(api(site, "/db/c"), { method: "POST", headers: apiHeaders(), body: '{"id":"d2","s":"a\\ud800b","z":-0}' });
    expect(res.status).toBe(201);
    const d2 = (await res.json()) as Doc;
    expect(d2.s).toBe("a\ufffdb");
    expect(Object.is(d2.z, 0)).toBe(true);

    // Filter operands are checked like stored values.
    const q = (body: string) => call(api(site, "/db/c/query"), { method: "POST", headers: apiHeaders(), body });
    res = await q('{"where":[{"field":"a","op":"==","value":"\\u0000"}]}');
    expect(res.status).toBe(400);
    expect((await errorOf(res)).message).toBe('filter on "a": strings must not contain the NUL character (U+0000)');
    expect((await q('{"where":[{"field":"a","op":"in","value":["x","\\u0000"]}]}')).status).toBe(400);
    expect((await q('{"where":[{"field":"_meta.created_by","op":"==","value":"\\u0000"}]}')).status).toBe(400);
    res = await q('{"where":[{"field":"z","op":"==","value":1e400}]}');
    expect(res.status).toBe(400);
    expect((await errorOf(res)).message).toBe('filter on "z": number Infinity is not valid JSON');
    res = await q('{"where":[{"field":"s","op":"==","value":"a\\udfffb"}]}');
    expect(((await res.json()) as { docs: Doc[] }).docs.map((d) => d.id)).toEqual(["d2"]);
    res = await q('{"where":[{"field":"z","op":"==","value":-0}]}');
    expect(((await res.json()) as { docs: Doc[] }).docs.map((d) => d.id)).toEqual(["d2"]);
  });

  it("validates inside the site object too", async () => {
    const stub = siteStub(E, uniqueSite("dov"));
    const r = await stub.dbCreate("c", "x", { a: "\u0000" }, { actor: "t", writer: "t" });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error).toMatchObject({ status: 400, code: "bad_request" });
    const deep = await stub.dbPut("c", "x", { d: nest(100) }, { actor: "t", writer: "t" });
    expect(deep.ok).toBe(false);
    const ok = unwrap(await stub.dbCreate("c", "y", { z: -0, s: "\udc00" }, { actor: "t", writer: "t" }));
    expect(ok.s).toBe("\ufffd");
  });
});

// --- semantics table (SPEC §5.2), via the site object -----------------------

const FIXTURE: [string, string, Record<string, unknown>][] = [
  ["f01", "alice", { n: 1, s: "a", b: true, tags: ["x", "y"], nested: { k: "v1", num: 10 }, "my-field": "dash", _under: 1 }],
  ["f02", "bob", { n: 2, s: "b", b: false, tags: ["y"], nested: { k: "v2" } }],
  ["f03", "alice", { n: "3", s: "c", b: 1, tags: "x" }],
  ["f04", "bob", { n: null, s: null, b: null, tags: null, nested: null }],
  ["f05", "alice", {}],
  ["f06", "bob", { n: 3.5, s: "B", b: "true", tags: [1, "1", true, null, ["x"], { x: 1 }], nested: "notobj" }],
  ["f07", "alice", { n: -10, s: "", tags: [], nested: { k: null } }],
  ["f08", "bob", { n: 2.0, s: "é", b: false, tags: ["X"], nested: { k: "v1", deep: { a: { b: { c: { d: { e: { f: "bottom" } } } } } } } }],
  ["f09", "alice", { n: 1e300, s: "ab", b: [true], tags: { "0": "x" } }],
  ["f10", "bob", { n: [1], s: ["a"], b: { b: true } }],
];

type W = [string, string, unknown];
const F = (...ids: string[]) => ids;

async function ids(site: string, coll: string, q: RawQuery): Promise<{ ids: string[]; hasMore: boolean }> {
  const page = unwrap(await siteStub(E, site).dbQuery(coll, normalizeQuery(q, 1000)));
  return { ids: page.docs.map((d) => d.id), hasMore: page.has_more };
}

function where(...ws: W[]): RawQuery {
  return { where: ws.map(([field, op, value]) => ({ field, op, value })), limit: 1000 };
}

describe("query semantics (conformance table)", () => {
  const site = uniqueSite("sem");
  const created: Record<string, Doc> = {};

  beforeAll(async () => {
    const stub = siteStub(E, site);
    for (const [id, actor, data] of FIXTURE) {
      created[id] = unwrap(await stub.dbCreate("fx", id, data, { actor, writer: "test" })) as Doc;
    }
  });

  const cases: [string, W[], string[]][] = [
    ["n == 1", [["n", "==", 1]], F("f01")],
    ["n == 2 (int and 2.0)", [["n", "==", 2]], F("f02", "f08")],
    ['n == 3 (not "3")', [["n", "==", 3]], F()],
    ['n == "3"', [["n", "==", "3"]], F("f03")],
    ["n == 3.5", [["n", "==", 3.5]], F("f06")],
    ["n == 1e300", [["n", "==", 1e300]], F("f09")],
    ["n == null", [["n", "==", null]], F("f04")],
    ["n != 1", [["n", "!=", 1]], F("f02", "f06", "f07", "f08", "f09")],
    ["n != null", [["n", "!=", null]], F()],
    ['n != "3"', [["n", "!=", "3"]], F()],
    ['n != "x"', [["n", "!=", "x"]], F("f03")],
    ["n < 2", [["n", "<", 2]], F("f01", "f07")],
    ["n <= 2", [["n", "<=", 2]], F("f01", "f02", "f07", "f08")],
    ["n > 2", [["n", ">", 2]], F("f06", "f09")],
    ["n >= 3.5", [["n", ">=", 3.5]], F("f06", "f09")],
    ['n < "4"', [["n", "<", "4"]], F("f03")],
    ['n > "3"', [["n", ">", "3"]], F()],
    ["n < true", [["n", "<", true]], F()],
    ["n <= null", [["n", "<=", null]], F("f04")],
    ["n >= null", [["n", ">=", null]], F("f04")],
    ["n < null", [["n", "<", null]], F()],
    ["n > null", [["n", ">", null]], F()],
    ['n in [1,2,"3"]', [["n", "in", [1, 2, "3"]]], F("f01", "f02", "f03", "f08")],
    ["n in [null]", [["n", "in", [null]]], F("f04")],
    ['n in ["1"]', [["n", "in", ["1"]]], F()],
    ["n in [true, 1]", [["n", "in", [true, 1]]], F("f01")],
    ["n not-in [1,2]", [["n", "not-in", [1, 2]]], F("f06", "f07", "f09")],
    ['n not-in ["3"]', [["n", "not-in", ["3"]]], F()],
    ['n not-in ["x"]', [["n", "not-in", ["x"]]], F("f03")],
    ['n not-in [1,"x"] (mixed)', [["n", "not-in", [1, "x"]]], F()],
    ["n not-in [null]", [["n", "not-in", [null]]], F()],
    ["n contains 1", [["n", "contains", 1]], F("f10")],
    ['s == "a"', [["s", "==", "a"]], F("f01")],
    ['s == ""', [["s", "==", ""]], F("f07")],
    ['s == "A"', [["s", "==", "A"]], F()],
    ['s > "a"', [["s", ">", "a"]], F("f02", "f03", "f08", "f09")],
    ['s < "a"', [["s", "<", "a"]], F("f06", "f07")],
    ['s >= ""', [["s", ">=", ""]], F("f01", "f02", "f03", "f06", "f07", "f08", "f09")],
    ['s < "b"', [["s", "<", "b"]], F("f01", "f06", "f07", "f09")],
    ['s > "z" (multibyte)', [["s", ">", "z"]], F("f08")],
    ["s < 1", [["s", "<", 1]], F()],
    ['s contains "a"', [["s", "contains", "a"]], F("f10")],
    ['s in ["a","é","zz"]', [["s", "in", ["a", "é", "zz"]]], F("f01", "f08")],
    ['s not-in ["a","b"]', [["s", "not-in", ["a", "b"]]], F("f03", "f06", "f07", "f08", "f09")],
    ["b == true", [["b", "==", true]], F("f01")],
    ["b == 1", [["b", "==", 1]], F("f03")],
    ['b == "true"', [["b", "==", "true"]], F("f06")],
    ["b == false", [["b", "==", false]], F("f02", "f08")],
    ["b == 0", [["b", "==", 0]], F()],
    ["b != true", [["b", "!=", true]], F("f02", "f08")],
    ["b < true", [["b", "<", true]], F("f02", "f08")],
    ["b > false", [["b", ">", false]], F("f01")],
    ["b <= true", [["b", "<=", true]], F("f01", "f02", "f08")],
    ["b >= false", [["b", ">=", false]], F("f01", "f02", "f08")],
    ["b in [true, 1]", [["b", "in", [true, 1]]], F("f01", "f03")],
    ["b not-in [false]", [["b", "not-in", [false]]], F("f01")],
    ["b == null", [["b", "==", null]], F("f04")],
    ["b contains true", [["b", "contains", true]], F("f09")],
    ['tags contains "x"', [["tags", "contains", "x"]], F("f01")],
    ['tags contains "y"', [["tags", "contains", "y"]], F("f01", "f02")],
    ["tags contains 1", [["tags", "contains", 1]], F("f06")],
    ['tags contains "1"', [["tags", "contains", "1"]], F("f06")],
    ["tags contains true", [["tags", "contains", true]], F("f06")],
    ["tags contains null", [["tags", "contains", null]], F("f06")],
    ["tags contains false", [["tags", "contains", false]], F()],
    ['tags contains "X"', [["tags", "contains", "X"]], F("f08")],
    ['tags == "x"', [["tags", "==", "x"]], F("f03")],
    ["tags == null", [["tags", "==", null]], F("f04")],
    ['tags != "x" (arrays never compare)', [["tags", "!=", "x"]], F()],
    ['tags in ["x"]', [["tags", "in", ["x"]]], F("f03")],
    ['nested.k == "v1"', [["nested.k", "==", "v1"]], F("f01", "f08")],
    ['nested.k != "v1"', [["nested.k", "!=", "v1"]], F("f02")],
    ["nested.k == null", [["nested.k", "==", null]], F("f07")],
    ['nested.k < "v2"', [["nested.k", "<", "v2"]], F("f01", "f08")],
    ["nested.num >= 10", [["nested.num", ">=", 10]], F("f01")],
    ["nested == null", [["nested", "==", null]], F("f04")],
    ['nested == "notobj"', [["nested", "==", "notobj"]], F("f06")],
    ["nested.x == null (missing)", [["nested.x", "==", null]], F()],
    ["depth 8", [["nested.deep.a.b.c.d.e.f", "==", "bottom"]], F("f08")],
    ["through non-object", [["s.length", ">=", 0]], F()],
    ["no array indexing", [["tags.x", "==", 1]], F()],
    ["my-field", [["my-field", "==", "dash"]], F("f01")],
    ["_under", [["_under", "==", 1]], F("f01")],
    ["missing field !=", [["nope", "!=", 1]], F()],
    ["missing field not-in", [["nope", "not-in", [1]]], F()],
    ["missing field == null", [["nope", "==", null]], F()],
    ["n >= 1 && b == true", [["n", ">=", 1], ["b", "==", true]], F("f01")],
    ["n >= 1 && b == false", [["n", ">=", 1], ["b", "==", false]], F("f02", "f08")],
    ["s > a && n == 2", [["s", ">", "a"], ["n", "==", 2]], F("f02", "f08")],
    ["n in [1,2] && tags contains y", [["n", "in", [1, 2]], ["tags", "contains", "y"]], F("f01", "f02")],
    ["range 1 <= n < 3", [["n", ">=", 1], ["n", "<", 3]], F("f01", "f02", "f08")],
    ["contradiction", [["n", "==", 1], ["n", "==", 2]], F()],
    ["id ==", [["id", "==", "f03"]], F("f03")],
    ["id in", [["id", "in", ["f01", "f10", "nope"]]], F("f01", "f10")],
    ["id >", [["id", ">", "f08"]], F("f09", "f10")],
    ["id == 3", [["id", "==", 3]], F()],
    ["id == null", [["id", "==", null]], F()],
    ["id contains", [["id", "contains", "f"]], F()],
    ["version == 1", [["_meta.version", "==", 1]], FIXTURE.map((f) => f[0])],
    ["version > 1", [["_meta.version", ">", 1]], F()],
    ['version == "1"', [["_meta.version", "==", "1"]], F()],
    ["created_by ==", [["_meta.created_by", "==", "alice"]], F("f01", "f03", "f05", "f07", "f09")],
    ["created_by in", [["_meta.created_by", "in", ["bob"]]], F("f02", "f04", "f06", "f08", "f10")],
    ["updated_by !=", [["_meta.updated_by", "!=", "alice"]], F("f02", "f04", "f06", "f08", "f10")],
    ["created_at == garbage", [["_meta.created_at", "==", "garbage"]], F()],
    ["created_at != garbage", [["_meta.created_at", "!=", "garbage"]], F()],
    ["created_at == 123", [["_meta.created_at", "==", 123]], F()],
    ["created_at > far past", [["_meta.created_at", ">", "1000-01-01T00:00:00Z"]], FIXTURE.map((f) => f[0])],
    ["created_at < far future", [["_meta.created_at", "<", "9999-12-31T23:59:59.999999999Z"]], FIXTURE.map((f) => f[0])],
    ["mixed system + data", [["_meta.created_by", "==", "bob"], ["n", "==", 2]], F("f02", "f08")],
  ];

  it.each(cases)("%s", async (_name, ws, want) => {
    expect((await ids(site, "fx", where(...ws))).ids).toEqual(want);
  });

  it("compares timestamps chronologically, across offsets", async () => {
    const t5 = String(created.f05!._meta.created_at);
    const every = FIXTURE.map((f) => f[0]);
    expect((await ids(site, "fx", where(["_meta.created_at", ">=", t5]))).ids).toEqual(every.slice(4));
    expect((await ids(site, "fx", where(["_meta.created_at", ">", t5]))).ids).toEqual(every.slice(5));
    expect((await ids(site, "fx", where(["_meta.created_at", "==", t5]))).ids).toEqual(["f05"]);
    expect((await ids(site, "fx", where(["_meta.created_at", "<", t5]))).ids).toEqual(every.slice(0, 4));
    expect((await ids(site, "fx", where(["_meta.created_at", "!=", t5]))).ids).toEqual(every.filter((x) => x !== "f05"));
    // The same instant written in another zone (+05:30).
    const ms = Date.parse(t5);
    const frac = t5.includes(".") ? t5.slice(t5.indexOf("."), -1) : "";
    const shifted = new Date(ms + 5.5 * 3600_000).toISOString().slice(0, 19) + frac + "+05:30";
    expect((await ids(site, "fx", where(["_meta.created_at", "==", shifted]))).ids).toEqual(["f05"]);
    const t2 = String(created.f02!._meta.created_at);
    const t9 = String(created.f09!._meta.created_at);
    expect((await ids(site, "fx", where(["_meta.created_at", "in", [t2, t9, "garbage"]]))).ids).toEqual(["f02", "f09"]);
    expect((await ids(site, "fx", where(["_meta.created_at", "not-in", [t2]]))).ids).toEqual(every.filter((x) => x !== "f02"));
  });
});

describe("ordering and pagination (conformance table)", () => {
  async function seed(coll: string, docs: [string, Record<string, unknown>][]): Promise<string> {
    const site = uniqueSite("ord");
    const stub = siteStub(E, site);
    for (const [id, data] of docs) unwrap(await stub.dbCreate(coll, id, data, { actor: "", writer: "test" }));
    return site;
  }
  const order = (...o: [string, "asc" | "desc"][]): RawQuery => ({ orderBy: o.map(([field, dir]) => ({ field, dir })), limit: 1000 });

  it("orders numbers with missing/null first ascending and last descending", async () => {
    const site = await seed("ord", [
      ["o01", { v: 3 }], ["o02", { v: 1 }], ["o03", {}], ["o04", { v: null }], ["o05", { v: 2 }],
      ["o06", { v: 1 }], ["o07", { v: 10 }], ["o08", { v: -1.5 }], ["o09", { v: 1e21 }], ["o10", { v: 0.5 }],
    ]);
    expect((await ids(site, "ord", order(["v", "asc"]))).ids).toEqual(["o03", "o04", "o08", "o10", "o02", "o06", "o05", "o01", "o07", "o09"]);
    expect((await ids(site, "ord", order(["v", "desc"]))).ids).toEqual(["o09", "o07", "o01", "o05", "o02", "o06", "o10", "o08", "o03", "o04"]);
    expect((await ids(site, "ord", { ...order(["v", "asc"]), where: [{ field: "v", op: ">", value: 0 }] })).ids).toEqual([
      "o10", "o02", "o06", "o05", "o01", "o07", "o09",
    ]);
    expect((await ids(site, "ord", order(["v", "desc"], ["id", "desc"]))).ids).toEqual(["o09", "o07", "o01", "o05", "o06", "o02", "o10", "o08", "o04", "o03"]);
  });

  it("handles number edge cases", async () => {
    const sum = 0.1 + 0.2;
    const site = await seed("nums", [
      ["e1", { v: 1e20 }], ["e2", { v: 2 ** 63 }], ["e3", { v: 2 ** 53 }], ["e4", { v: -0 }],
      ["e5", { v: 1e-300 }], ["e6", { v: -1e20 }], ["e7", { v: 0 }], ["e8", { v: sum }],
    ]);
    const q = (v: W) => ids(site, "nums", where(v));
    expect((await q(["v", "==", 1e20])).ids).toEqual(["e1"]);
    expect((await q(["v", ">", 9e18])).ids).toEqual(["e1", "e2"]);
    expect((await q(["v", "==", 2 ** 63])).ids).toEqual(["e2"]);
    expect((await q(["v", "==", 2 ** 53])).ids).toEqual(["e3"]);
    expect((await q(["v", "==", 0])).ids).toEqual(["e4", "e7"]);
    expect((await q(["v", ">", 0])).ids).toEqual(["e1", "e2", "e3", "e5", "e8"]);
    expect((await q(["v", "<", 0])).ids).toEqual(["e6"]);
    expect((await q(["v", "==", sum])).ids).toEqual(["e8"]);
    expect((await q(["v", "==", 0.3])).ids).toEqual([]);
    expect((await ids(site, "nums", order(["v", "asc"]))).ids).toEqual(["e6", "e4", "e7", "e5", "e8", "e3", "e2", "e1"]);
    const e8 = unwrap(await siteStub(E, site).dbGet("nums", "e8"));
    expect(e8.v).toBe(sum);
  });

  it("compares integers beyond 2^53 as doubles, like Go's float64 (raw JSON bodies)", async () => {
    // Stored as the shortest text of the double (1760000000123456800), which
    // SQLite reads as an INTEGER and compares exactly with the REAL operand
    // 1760000000123456768 unless it is CAST first.
    const site = uniqueSite("bigint");
    const raw = (path: string, body: string) => call(api(site, path), { method: "POST", headers: apiHeaders(), body });
    for (const body of [
      '{"id":"i1","v":1760000000123456789}',
      '{"id":"i2","v":1760000000123457024}',
      '{"id":"i3","v":1760000000123456512}',
      '{"id":"i4","v":"1760000000123456789"}',
      '{"id":"i8","arr":[1760000000123456789,1]}',
      '{"id":"i9","arr":[1760000000123457024]}',
    ]) {
      expect((await raw("/db/big", body)).status, body).toBe(201);
    }
    const q = async (filter: string) => {
      const res = await raw("/db/big/query", `{"where":[${filter}],"limit":1000}`);
      expect(res.status, filter).toBe(200);
      return ((await res.json()) as { docs: Doc[] }).docs.map((d) => d.id);
    };
    expect(await q('{"field":"v","op":"==","value":1760000000123456789}')).toEqual(["i1"]);
    expect(await q('{"field":"v","op":"==","value":1760000000123456768}')).toEqual(["i1"]);
    expect(await q('{"field":"v","op":"!=","value":1760000000123456789}')).toEqual(["i2", "i3"]);
    expect(await q('{"field":"v","op":"<=","value":1760000000123456789}')).toEqual(["i1", "i3"]);
    expect(await q('{"field":"v","op":">=","value":1760000000123456789}')).toEqual(["i1", "i2"]);
    expect(await q('{"field":"v","op":">","value":1760000000123456789}')).toEqual(["i2"]);
    expect(await q('{"field":"v","op":"<","value":1760000000123456789}')).toEqual(["i3"]);
    expect(await q('{"field":"v","op":"in","value":[1760000000123456789,1]}')).toEqual(["i1"]);
    expect(await q('{"field":"v","op":"not-in","value":[1760000000123456789]}')).toEqual(["i2", "i3"]);
    expect(await q('{"field":"arr","op":"contains","value":1760000000123456789}')).toEqual(["i8"]);
    expect(await q('{"field":"arr","op":"contains","value":1760000000123457024}')).toEqual(["i9"]);
  });

  it("orders strings bytewise, booleans false < true, and mixed types by rank", async () => {
    const s = await seed("ords", [
      ["s01", { v: "b" }], ["s02", { v: "B" }], ["s03", { v: "a" }], ["s04", { v: "" }], ["s05", { v: "é" }],
      ["s06", { v: "aa" }], ["s07", { v: "Z" }], ["s08", { v: "ä" }], ["s09", { v: "~" }], ["s10", { v: "😀" }],
      ["s11", {}], ["s12", { v: "a" }], ["s13", { v: "10" }], ["s14", { v: "9" }],
    ]);
    expect((await ids(s, "ords", order(["v", "asc"]))).ids).toEqual(["s11", "s04", "s13", "s14", "s02", "s07", "s03", "s12", "s06", "s01", "s09", "s08", "s05", "s10"]);
    expect((await ids(s, "ords", order(["v", "desc"]))).ids).toEqual(["s10", "s05", "s08", "s09", "s01", "s06", "s03", "s12", "s07", "s02", "s14", "s13", "s04", "s11"]);

    const b = await seed("ordb", [["b1", { v: true }], ["b2", { v: false }], ["b3", {}], ["b4", { v: true }], ["b5", { v: false }], ["b6", { v: null }]]);
    expect((await ids(b, "ordb", order(["v", "asc"]))).ids).toEqual(["b3", "b6", "b2", "b5", "b1", "b4"]);
    expect((await ids(b, "ordb", order(["v", "desc"]))).ids).toEqual(["b1", "b4", "b2", "b5", "b3", "b6"]);

    const x = await seed("ordx", [
      ["x1", { v: "str" }], ["x2", { v: 5 }], ["x3", { v: true }], ["x4", { v: null }], ["x5", { v: [9] }], ["x6", { v: { a: 1 } }],
      ["x7", {}], ["x8", { v: [1] }], ["x9", { v: {} }], ["xa", { v: false }], ["xb", { v: "a" }], ["xc", { v: -1 }],
    ]);
    expect((await ids(x, "ordx", order(["v", "asc"]))).ids).toEqual(["x4", "x7", "xa", "x3", "xc", "x2", "xb", "x1", "x5", "x8", "x6", "x9"]);
    expect((await ids(x, "ordx", order(["v", "desc"]))).ids).toEqual(["x6", "x9", "x5", "x8", "x1", "xb", "x2", "xc", "x3", "xa", "x4", "x7"]);
  });

  it("orders by several keys and nested paths", async () => {
    const m = await seed("multi", [
      ["m1", { g: "a", score: 5 }], ["m2", { g: "b", score: 7 }], ["m3", { g: "a", score: 9 }],
      ["m4", { g: "b", score: 7 }], ["m5", { score: 1 }], ["m6", { g: "a" }],
    ]);
    expect((await ids(m, "multi", order(["g", "asc"], ["score", "desc"]))).ids).toEqual(["m5", "m3", "m1", "m6", "m2", "m4"]);
    expect((await ids(m, "multi", order(["g", "asc"], ["id", "desc"]))).ids).toEqual(["m5", "m6", "m3", "m1", "m4", "m2"]);
    expect((await ids(m, "multi", order(["score", "asc"], ["g", "desc"]))).ids).toEqual(["m6", "m5", "m1", "m2", "m4", "m3"]);
    expect((await ids(m, "multi", order(["g", "desc"], ["score", "asc"]))).ids).toEqual(["m2", "m4", "m6", "m1", "m3", "m5"]);

    const n = await seed("nest", [
      ["n1", { p: { q: 2 } }], ["n2", { p: { q: 1 } }], ["n3", { p: "flat" }],
      ["n4", { p: { q: null } }], ["n5", { p: { q: 3, r: { s: "z" } } }], ["n6", { p: { r: { s: "a" } } }],
    ]);
    expect((await ids(n, "nest", order(["p.q", "asc"]))).ids).toEqual(["n3", "n4", "n6", "n2", "n1", "n5"]);
    expect((await ids(n, "nest", order(["p.q", "desc"]))).ids).toEqual(["n5", "n1", "n2", "n3", "n4", "n6"]);
    expect((await ids(n, "nest", order(["p.r.s", "desc"]))).ids).toEqual(["n5", "n6", "n1", "n2", "n3", "n4"]);
  });

  it("orders by system fields", async () => {
    const site = uniqueSite("sys");
    const stub = siteStub(E, site);
    unwrap(await stub.dbCreate("sys", "c", {}, { actor: "zed", writer: "t" }));
    unwrap(await stub.dbCreate("sys", "a", {}, { actor: "amy", writer: "t" }));
    unwrap(await stub.dbCreate("sys", "b", {}, { actor: "mia", writer: "t" }));
    for (const id of ["c", "c", "b"]) unwrap(await stub.dbPatch("sys", id, { touched: true }, { actor: "", writer: "t" }));
    const q = (o: RawQuery) => ids(site, "sys", o);
    expect((await q({})).ids).toEqual(["c", "a", "b"]);
    expect((await q(order(["_meta.created_at", "desc"]))).ids).toEqual(["b", "a", "c"]);
    expect((await q(order(["id", "asc"]))).ids).toEqual(["a", "b", "c"]);
    expect((await q(order(["id", "desc"]))).ids).toEqual(["c", "b", "a"]);
    expect((await q(order(["_meta.updated_at", "asc"]))).ids).toEqual(["a", "c", "b"]);
    expect((await q(order(["_meta.updated_at", "desc"]))).ids).toEqual(["b", "c", "a"]);
    expect((await q(order(["_meta.version", "desc"]))).ids).toEqual(["c", "b", "a"]);
    expect((await q(order(["_meta.created_by", "asc"]))).ids).toEqual(["a", "b", "c"]);
  });

  it("paginates with has_more", async () => {
    const docs: [string, Record<string, unknown>][] = Array.from({ length: 25 }, (_, i) => [`p${String(i).padStart(2, "0")}`, { n: i % 5 }]);
    const site = await seed("pg", docs);
    const all = docs.map((d) => d[0]);
    const cases: [string, RawQuery, string[], boolean][] = [
      ["first page", { limit: 10 }, all.slice(0, 10), true],
      ["second page", { limit: 10, offset: 10 }, all.slice(10, 20), true],
      ["last partial", { limit: 10, offset: 20 }, all.slice(20), false],
      ["exact last", { limit: 5, offset: 20 }, all.slice(20), false],
      ["boundary has more", { limit: 5, offset: 15 }, all.slice(15, 20), true],
      ["offset at end", { limit: 10, offset: 25 }, [], false],
      ["offset beyond", { limit: 10, offset: 100 }, [], false],
      ["default limit", {}, all, false],
      ["filtered page", { where: [{ field: "n", op: "==", value: 0 }], limit: 2, offset: 2 }, ["p10", "p15"], true],
      ["filtered last", { where: [{ field: "n", op: "==", value: 0 }], limit: 2, offset: 4 }, ["p20"], false],
      ["ordered page", { orderBy: [{ field: "n", dir: "desc" }], limit: 3 }, ["p04", "p09", "p14"], true],
      ["ordered page 2", { orderBy: [{ field: "n", dir: "desc" }], limit: 3, offset: 3 }, ["p19", "p24", "p03"], true],
    ];
    for (const [name, q, want, more] of cases) {
      const got = await ids(site, "pg", q);
      expect({ name, ids: got.ids, more: got.hasMore }).toEqual({ name, ids: want, more });
    }
  });
});

describe("data fields named like members of Object.prototype", () => {
  it("filters and orders constructor, toString, valueOf and __proto__ as data, like the Go edition", async () => {
    const site = uniqueSite("protofields");
    const q = async (query: Record<string, unknown>) => {
      const res = await call(api(site, "/db/f1/query"), { method: "POST", headers: apiHeaders(), body: JSON.stringify(query) });
      expect(res.status, JSON.stringify(query)).toBe(200);
      return ((await res.json()) as { docs: Doc[] }).docs.map((d) => d.id);
    };
    expect((await call(api(site, "/db/f1"), { method: "POST", headers: apiHeaders(), body: '{"id":"a","constructor":"x","toString":2,"valueOf":true,"__proto__":3}' })).status).toBe(201);
    expect((await create(site, "f1", { id: "b", constructor: "y", toString: 1, valueOf: false })).status).toBe(201);
    expect((await create(site, "f1", { id: "c" })).status).toBe(201);
    expect(await q({ where: [{ field: "constructor", op: "==", value: "x" }] })).toEqual(["a"]);
    expect(await q({ where: [{ field: "constructor", op: "in", value: ["x", "y"] }], order_by: [{ field: "constructor", dir: "desc" }] })).toEqual(["b", "a"]);
    expect(await q({ where: [{ field: "toString", op: ">", value: 0 }], order_by: [{ field: "toString" }] })).toEqual(["b", "a"]);
    expect(await q({ where: [{ field: "valueOf", op: "==", value: true }] })).toEqual(["a"]);
    expect(await q({ where: [{ field: "__proto__", op: "==", value: 3 }] })).toEqual(["a"]);
    expect(await q({ where: [{ field: "hasOwnProperty", op: "!=", value: 1 }] })).toEqual([]);
    expect(await q({ order_by: [{ field: "__proto__", dir: "desc" }] })).toEqual(["a", "b", "c"]);
    for (const order of ["valueOf:asc", "toString:desc", "constructor:asc", "isPrototypeOf:asc"]) {
      const res = await call(api(site, `/db/f1?order=${order}`));
      expect(res.status, order).toBe(200);
    }
  });
});

describe("query pages and the Durable Object RPC limit", () => {
  it("cuts a page at its byte budget with has_more instead of failing, and offsets reach every document", async () => {
    const site = uniqueSite("bigpage");
    // 64 documents of ~250 KB of numbers: ~16 MB of JSON, but over 32 MiB once
    // parsed and serialized for RPC (9 bytes per number): a page of them all
    // used to fail with 500 even at the default limit.
    const body = JSON.stringify({ v: new Array(64000).fill(1.5) });
    for (let i = 0; i < 64; i++) {
      const res = await call(api(site, `/db/nums/d${String(i).padStart(2, "0")}`), { method: "PUT", headers: apiHeaders(), body });
      expect(res.status, `put ${i}`).toBe(200);
    }
    for (const first of [() => call(api(site, "/db/nums")), () => call(api(site, "/db/nums/query"), { method: "POST", headers: apiHeaders(), body: '{"limit":1000}' })]) {
      const res = await first();
      expect(res.status).toBe(200);
      const page = (await res.json()) as { docs: Doc[]; has_more: boolean };
      expect(page.docs.length).toBeGreaterThan(0);
      expect(page.docs.length).toBeLessThan(64);
      expect(page.has_more).toBe(true);
      expect((page.docs[0]!.v as number[]).length).toBe(64000);
    }
    const ids: string[] = [];
    for (let offset = 0; ; ) {
      const res = await call(api(site, `/db/nums?limit=1000&offset=${offset}`));
      expect(res.status).toBe(200);
      const page = (await res.json()) as { docs: Doc[]; has_more: boolean };
      ids.push(...page.docs.map((d) => d.id));
      offset += page.docs.length;
      if (!page.has_more) break;
    }
    expect(ids).toEqual(Array.from({ length: 64 }, (_, i) => `d${String(i).padStart(2, "0")}`));
    // Small documents still fill the page up to the limit.
    for (let i = 0; i < 5; i++) await create(site, "small", { id: `s${i}`, n: i });
    const small = (await (await call(api(site, "/db/small?limit=3"))).json()) as { docs: Doc[]; has_more: boolean };
    expect(small.docs.map((d) => d.id)).toEqual(["s0", "s1", "s2"]);
    expect(small.has_more).toBe(true);
  });
});
