// The generated builds are in sync with src/core.js, and the classic and ES
// module builds expose the same SDK (and share one instance per page).
import { test } from "node:test";
import assert from "node:assert/strict";
import vm from "node:vm";
import { readFileSync } from "node:fs";
import { build, validateCore, NAMED_EXPORTS, paths } from "../build.mjs";
import { CLASSIC_PATH, MODULE_PATH, TYPES_PATH, CORE_PATH, createEnv, makeLocation, json } from "./helpers/harness.mjs";

const core = readFileSync(CORE_PATH, "utf8");
const classic = readFileSync(CLASSIC_PATH, "utf8");
const moduleSrc = readFileSync(MODULE_PATH, "utf8");
const types = readFileSync(TYPES_PATH, "utf8");

test("backyard.js and backyard.mjs are in sync with src/core.js (run: node web/sdk/build.mjs)", () => {
  const out = build(core);
  assert.equal(paths.core, CORE_PATH);
  assert.ok(out.classic === classic, "backyard.js is stale: run `node web/sdk/build.mjs`");
  assert.ok(out.module === moduleSrc, "backyard.mjs is stale: run `node web/sdk/build.mjs`");
});

test("core.js is a single factory without module syntax", () => {
  validateCore(core);
  assert.throws(() => validateCore(core + "\nexport default 1;\n"), /import\/export/);
  assert.throws(() => validateCore('import x from "y";\n' + core), /import\/export/);
  assert.throws(() => validateCore("function other() {}\n"), /createBackyard/);
});

test("both builds embed the identical core source", () => {
  assert.ok(classic.includes(core));
  assert.ok(moduleSrc.includes(core));
});

test("the classic build defines only window.backyard (plus the shared-instance symbol)", () => {
  const sandbox = { location: makeLocation("https://foo.backyard.test/", []), URL, TextDecoder };
  vm.createContext(sandbox);
  const before = new Set(vm.runInContext("Object.getOwnPropertyNames(globalThis)", sandbox));
  vm.runInContext(classic, sandbox);
  const added = vm.runInContext("Object.getOwnPropertyNames(globalThis)", sandbox).filter((n) => !before.has(n));
  assert.deepEqual([...added], ["backyard"]);
  const symbols = vm.runInContext("Object.getOwnPropertySymbols(globalThis).map(String)", sandbox);
  assert.deepEqual([...symbols], ["Symbol(backyard.sdk)"]);
  // Loading the script twice keeps the same instance (one websocket per page).
  const first = vm.runInContext("backyard", sandbox);
  vm.runInContext(classic, sandbox);
  assert.equal(vm.runInContext("backyard", sandbox), first);
});

test("the classic build works with the server preamble prepended", () => {
  const preamble = 'globalThis.__BACKYARD__ = {"v":1,"mode":"path","site":"poll","prefix":"/s/","hub":"https://hub.test"};\n';
  const sandbox = { location: makeLocation("https://hub.test/s/poll/", []), URL, TextDecoder };
  vm.createContext(sandbox);
  vm.runInContext(preamble + classic, sandbox);
  assert.equal(vm.runInContext("backyard.site.apiBase", sandbox), "https://hub.test/s/poll/__backyard");
});

test("the d.ts documents every member and named export", () => {
  for (const name of NAMED_EXPORTS) {
    assert.match(types, new RegExp(`export const ${name}\\b`), "ESM export " + name);
  }
  const env = createEnv();
  const by = env.backyard;
  const members = {
    SDK: by,
    IdentityAPI: by.id,
    DatabaseAPI: by.db,
    FilesAPI: by.files,
    AIAPI: by.ai,
    WarehouseAPI: by.warehouse,
    SocketAPI: by.socket,
  };
  for (const [iface, obj] of Object.entries(members)) {
    const block = new RegExp(`interface ${iface}\\b[^{]*\\{([\\s\\S]*?)\\n  \\}`).exec(types);
    assert.ok(block, "interface " + iface);
    const keys = new Set([...Object.keys(obj), ...Object.getOwnPropertyNames(Object.getPrototypeOf(obj)).filter((k) => k !== "constructor")]);
    for (const key of keys) {
      if (key.startsWith("_") || key === "__proto__" || typeof Object.prototype[key] === "function") continue;
      assert.match(block[1], new RegExp(`\\b${key}\\b`), `${iface}.${key} is declared`);
    }
  }
  const col = by.db.collection("x");
  for (const key of Object.getOwnPropertyNames(Object.getPrototypeOf(col))) {
    if (key === "constructor" || key.startsWith("_")) continue;
    assert.match(types, new RegExp(`interface Collection[\\s\\S]*?\\b${key}\\(`), "Collection." + key);
  }
  const q = col.where("a", "==", 1);
  for (const key of Object.getOwnPropertyNames(Object.getPrototypeOf(q))) {
    if (key === "constructor" || key.startsWith("_")) continue;
    assert.match(types, new RegExp(`interface Query[\\s\\S]*?\\b${key}\\(`), "Query." + key);
  }
  const room = by.socket.room("r");
  for (const key of Object.getOwnPropertyNames(Object.getPrototypeOf(room))) {
    if (key === "constructor" || key.startsWith("_")) continue;
    assert.match(types, new RegExp(`interface Room<[\\s\\S]*?\\b${key}\\b`), "Room." + key);
  }
});

// --- ES module build, evaluated in this (real) realm --------------------------

let importCounter = 0;
/** Imports a fresh copy of backyard.mjs with the given page globals. */
async function importModule({ href, preamble, fetch }) {
  const KEY = Symbol.for("backyard.sdk");
  delete globalThis[KEY];
  delete globalThis.backyard;
  globalThis.location = makeLocation(href, []);
  globalThis.__BACKYARD__ = preamble;
  globalThis.fetch = fetch;
  const src = (preamble ? `globalThis.__BACKYARD__ = ${JSON.stringify(preamble)};\n` : "") + moduleSrc;
  const url = "data:text/javascript;base64," + Buffer.from(src + `\n//# sourceURL=backyard.mjs?${++importCounter}`).toString("base64");
  return import(url);
}

test("ESM: default export + named exports bound to the same instance", async () => {
  const realFetch = globalThis.fetch;
  try {
    const mod = await importModule({ href: "https://foo.backyard.test/", preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: "https://backyard.test" } });
    assert.deepEqual(Object.keys(mod).sort(), ["default", ...NAMED_EXPORTS].sort());
    for (const name of NAMED_EXPORTS) assert.equal(mod[name], mod.default[name], name);
    assert.equal(typeof mod.me, "function");
    assert.equal(typeof mod.configure, "function");
    assert.ok(mod.ready instanceof Promise);
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("ESM/classic parity: same context resolution and request shapes", async () => {
  const realFetch = globalThis.fetch;
  const scenarios = [
    { href: "https://backyard.test/s/poll/x.html", preamble: { v: 1, mode: "path", site: null, prefix: "/s/", hub: "https://backyard.test" } },
    { href: "https://foo.backyard.test/", preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: "https://backyard.test" } },
  ];
  try {
    for (const sc of scenarios) {
      const classicEnv = createEnv(sc);
      classicEnv.on("*", /.*/, (req) => json(req.method === "POST" ? 201 : 200, { id: "d1", ok: true }));
      const seen = [];
      const mod = await importModule({
        ...sc,
        fetch: async (url, init) => {
          seen.push({ url, method: init.method, headers: init.headers, body: init.body, credentials: init.credentials });
          return json(init.method === "POST" ? 201 : 200, { id: "d1", ok: true });
        },
      });
      assert.deepEqual({ ...mod.site }, JSON.parse(JSON.stringify(classicEnv.backyard.site)));

      const ops = [
        (by) => by.db.collection("posts").create({ a: 1 }, { id: "d1" }),
        (by) => by.db.collection("posts").where("a", "==", 1).orderBy("a", "desc").get(),
        (by) => by.files.list({ limit: 2 }),
        (by) => by.ai.chat("hi", { maxTokens: 5 }),
        (by) => by.warehouse.named("q", { a: 1 }),
        (by) => by.id.lookup("a@b.c"),
      ];
      for (const op of ops) {
        await op(classicEnv.backyard).catch(() => {});
        await op(mod.default).catch(() => {});
      }
      const fromClassic = classicEnv.requests.map((r) => ({ url: r.url, method: r.method, body: r.body, credentials: r.credentials, csrf: r.headers["x-backyard"] }));
      const fromModule = seen.map((r) => ({ url: r.url, method: r.method, body: r.body, credentials: r.credentials, csrf: r.headers["X-Backyard"] }));
      assert.deepEqual(fromModule, fromClassic);
    }
  } finally {
    globalThis.fetch = realFetch;
  }
});

test("ESM and classic builds loaded on one page share one instance", async () => {
  const realFetch = globalThis.fetch;
  try {
    const mod = await importModule({ href: "https://foo.backyard.test/", preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: "https://backyard.test" } });
    vm.runInThisContext(classic);
    assert.equal(globalThis.backyard, mod.default);
  } finally {
    globalThis.fetch = realFetch;
    delete globalThis.backyard;
  }
});
