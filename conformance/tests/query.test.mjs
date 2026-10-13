// The shared query-semantics cases (internal/docstore/conformance/querycases.json,
// SPEC §5.2) over HTTP: create the fixtures in order, run every query.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { queryCasesPath, setup, uniqueSite } from "../lib/env.mjs";
import { Client, expectStatus } from "../lib/http.mjs";
import { assertDoc } from "../lib/shapes.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";
const site = uniqueSite("query");

// Numbers keep their source text (9223372036854775808, -0.0, 1e+300) so the
// server parses exactly what the case file says.
const keepNumbers = (_key, value, ctx) => (typeof value === "number" && ctx?.source !== undefined ? JSON.rawJSON(ctx.source) : value);
const cases = JSON.parse(readFileSync(queryCasesPath(), "utf8"), keepNumbers);
const plain = JSON.parse(readFileSync(queryCasesPath(), "utf8"));

describe("shared query cases", { skip }, () => {
  before(async () => {
    await deploySite(client, env, site, { "index.html": "query cases" });
    // Sequentially and in the listed order: created_at must increase.
    for (const name of Object.keys(cases.collections).sort()) {
      const docs = cases.collections[name];
      for (let i = 0; i < docs.length; i++) {
        const { id, data } = docs[i];
        const r = await client.fetch(env.api(site, `/db/${name}`), { method: "POST", json: JSON.stringify({ ...data, id }), raw: true });
        assert.equal(r.status, 201, `create ${name}/${id}: ${r}`);
        assertDoc(r.json, { id, version: 1, data: plain.collections[name][i].data, what: `${name}/${id}` });
      }
    }
  });

  test("the case file is loaded", () => {
    assert.ok(cases.cases.length > 100, `${cases.cases.length} cases`);
  });

  for (const c of cases.cases) {
    test(`${c.collection}: ${c.name}`, async () => {
      const r = await client.fetch(env.api(site, `/db/${c.collection}/query`), { method: "POST", json: JSON.stringify(c.query), raw: true });
      expectStatus(r, 200, c.name);
      const got = r.json;
      assert.deepEqual(got.docs.map((d) => d.id), c.want ?? [], `${c.name}: ids`);
      assert.equal(got.has_more, c.has_more, `${c.name}: has_more`);
    });
  }
});
