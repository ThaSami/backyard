// Warehouse (SPEC §5.5). Capability detection: without a warehouse every
// endpoint is 501 unsupported (always on the Cloudflare edition without
// WAREHOUSE_URL). With one, the generic rules are checked; a named query
// "conformance_echo" with the parameter "msg" (configured by run-go.sh) is
// also executed.
import assert from "node:assert/strict";
import { before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";
const site = uniqueSite("wh");
const api = (p) => env.api(site, p);

describe("warehouse", { skip }, () => {
  let described = null;

  before(async () => {
    await deploySite(client, env, site, { "index.html": "wh" });
    const r = await client.get(api("/warehouse"));
    if (r.status === 501) {
      assertError(r, 501, "unsupported");
      return;
    }
    described = expectStatus(r, 200).json;
  });

  test("without a warehouse every endpoint is 501 unsupported", async (t) => {
    if (described) return t.skip("a warehouse is configured");
    assertError(await client.post(api("/warehouse/query"), { sql: "SELECT 1" }), 501, "unsupported");
    assertError(await client.post(api("/warehouse/query"), { query: "anything", params: {} }), 501, "unsupported");
  });

  test("GET /warehouse describes sources and named queries", (t) => {
    if (!described) return t.skip("no warehouse");
    assert.ok(Array.isArray(described.sources) && described.sources.length > 0);
    for (const s of described.sources) {
      assert.equal(typeof s.name, "string");
      assert.equal(typeof s.type, "string");
    }
    assert.ok(Array.isArray(described.queries));
    for (const q of described.queries) {
      assert.equal(typeof q.name, "string");
      assert.ok(Array.isArray(q.params));
      assert.equal(typeof q.source, "string");
    }
  });

  test("raw SQL (when allowed) and the statement guard", async (t) => {
    if (!described) return t.skip("no warehouse");
    const r = await client.post(api("/warehouse/query"), { sql: "SELECT 1 AS n, ? AS s", params: ["x"] });
    if (r.status === 403) {
      assertError(r, 403, "forbidden", "raw SQL disabled");
      return;
    }
    const res = expectStatus(r, 200).json;
    assert.deepEqual(Object.keys(res).sort(), ["columns", "row_count", "rows", "truncated"]);
    assert.deepEqual(res.columns.map((c) => c.name), ["n", "s"]);
    for (const c of res.columns) assert.equal(typeof c.type, "string");
    assert.deepEqual(res.rows, [{ n: 1, s: "x" }]);
    assert.equal(res.row_count, 1);
    assert.equal(res.truncated, false);
    assertError(await client.post(api("/warehouse/query"), { sql: "DELETE FROM conformance_nothing" }), 403, "forbidden", "not a SELECT/WITH");
    assertError(await client.post(api("/warehouse/query"), { sql: "SELECT ? AS a", params: [] }), 400, "bad_request", "placeholder/param mismatch");
    assertError(await client.post(api("/warehouse/query"), { sql: "SELECT 1", source: "no-such-source" }), 404, "not_found", "unknown source");
  });

  test("named queries", async (t) => {
    if (!described) return t.skip("no warehouse");
    assertError(await client.post(api("/warehouse/query"), { query: "no_such_query", params: {} }), 404, "not_found");
    for (const q of described.queries.filter((x) => x.params.length > 0)) {
      assertError(await client.post(api("/warehouse/query"), { query: q.name, params: {} }), 400, "bad_request", `${q.name} without params`);
    }
    const echo = described.queries.find((q) => q.name === "conformance_echo");
    if (echo) {
      const r = expectStatus(await client.post(api("/warehouse/query"), { query: "conformance_echo", params: { msg: "hi" } }), 200).json;
      assert.deepEqual(r.rows, [{ msg: "hi" }]);
      assertError(await client.post(api("/warehouse/query"), { query: "conformance_echo", params: { msg: "hi", extra: 1 } }), 400, "bad_request", "unknown param");
    }
  });
});
