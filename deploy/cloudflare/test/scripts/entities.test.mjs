// src/entities.ts against Go's own table (node --test): the Worker decodes
// named character references in titles and og:image references exactly as
// the Go server's html.UnescapeString does. Skipped where Go is not installed.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const pkgDir = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const go = spawnSync("go", ["env", "GOROOT"], { encoding: "utf8" });
const entityGo = go.status === 0 ? join(go.stdout.trim(), "src", "html", "entity.go") : "";
const skip = !entityGo || !existsSync(entityGo) ? "Go is not installed" : false;

/** name → "hex[,hex]" of src/entities.ts. */
function workerTable() {
  const ts = readFileSync(join(pkgDir, "src", "entities.ts"), "utf8");
  const data = /export const ENTITY_DATA = `([^`]*)`;/.exec(ts)?.[1];
  assert.ok(data, "ENTITY_DATA in src/entities.ts");
  const out = new Map();
  for (const e of data.trim().split(/\s+/)) {
    const i = e.lastIndexOf(":");
    assert.ok(!out.has(e.slice(0, i)), `duplicate ${e}`);
    out.set(e.slice(0, i), e.slice(i + 1));
  }
  return out;
}

/** name → "hex[,hex]" of Go's html/entity.go (both maps; commented-out entries are not Go's). */
function goTable() {
  const src = readFileSync(entityGo, "utf8");
  const out = new Map();
  for (const m of src.matchAll(/^\s*"([^"]+)":\s+'\\U([0-9A-Fa-f]{8})',?\s*$/gm)) out.set(m[1], parseInt(m[2], 16).toString(16));
  for (const m of src.matchAll(/^\s*"([^"]+)":\s+\{'\\u([0-9A-Fa-f]{4})', '\\u([0-9A-Fa-f]{4})'\},?\s*$/gm)) {
    out.set(m[1], `${parseInt(m[2], 16).toString(16)},${parseInt(m[3], 16).toString(16)}`);
  }
  return out;
}

describe("src/entities.ts", () => {
  it("is Go's html entity table", { skip }, () => {
    const want = goTable();
    assert.ok(want.size > 2200, `parsed ${want.size} entries of ${entityGo}`);
    assert.deepEqual(new Map([...workerTable()].sort()), new Map([...want].sort()));
  });

  it("names legacy references without a ';' of at most 6 characters (title.ts LONGEST_LEGACY)", () => {
    const legacy = [...workerTable().keys()].filter((k) => !k.endsWith(";"));
    assert.ok(legacy.includes("amp") && legacy.includes("nbsp"));
    assert.equal(Math.max(...legacy.map((k) => k.length)), 6);
  });
});
