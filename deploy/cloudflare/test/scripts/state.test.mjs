// In-process tests of scripts/lib/state.mjs (node --test): the state file
// (v1 → v2, mode 0600, invalid JSON stops, unknown fields kept), the lock and
// its staleness rules, and the result file.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { homedir, hostname, tmpdir } from "node:os";
import { join } from "node:path";
import { after, describe, it } from "node:test";

import { LOCK_STALE_MS, acquireLock, buildResult, loadState, saveState, stateDir, stateFile, writeResult } from "../../scripts/lib/state.mjs";
import { CliError } from "../../scripts/lib/ui.mjs";

const dirs = [];
function env() {
  const d = mkdtempSync(join(tmpdir(), "backyard-state-"));
  dirs.push(d);
  return { XDG_CONFIG_HOME: d };
}
after(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});
const mode = (p) => (statSync(p).mode & 0o777).toString(8);
const failsWith = (fn, re) => assert.throws(fn, (e) => e instanceof CliError && e.code === 1 && re.test(e.message));

describe("state file", () => {
  it("lives under $XDG_CONFIG_HOME, else ~/.config", () => {
    assert.equal(stateFile("team", { XDG_CONFIG_HOME: "/x" }), "/x/backyard/cloudflare/team.json");
    assert.equal(stateDir({}), join(homedir(), ".config", "backyard", "cloudflare"));
  });

  it("is empty when missing, reads version 1 as version 2 and keeps unknown fields", () => {
    const e = env();
    assert.deepEqual(loadState("backyard", e), {});
    const v1 = { accountId: "a", deployToken: "byd_x", url: "https://u", r2Bucket: "b", vars: { LIMITS: "{}" }, updatedAt: "2026-01-01T00:00:00Z", future: [1] };
    mkdirSync(stateDir(e), { recursive: true });
    writeFileSync(stateFile("backyard", e), JSON.stringify(v1));
    const st = loadState("backyard", e);
    assert.deepEqual(st, v1);
    const p = saveState("backyard", st, e);
    const saved = JSON.parse(readFileSync(p, "utf8"));
    assert.equal(Object.keys(saved)[0], "version");
    assert.equal(saved.version, 2);
    assert.deepEqual(saved.future, [1]);
    assert.equal(saved.deployToken, "byd_x");
    assert.notEqual(saved.updatedAt, v1.updatedAt);
    // undefined fields are dropped.
    st.domain = undefined;
    st.r2Bucket = undefined;
    saveState("backyard", st, e);
    assert.ok(!("r2Bucket" in JSON.parse(readFileSync(p, "utf8"))));
  });

  it("is private (0600 in a 0700 directory) and written atomically", () => {
    const e = env();
    const p = saveState("backyard", { deployToken: "byd_secret" }, e);
    if (process.platform !== "win32") {
      assert.equal(mode(p), "600");
      assert.equal(mode(stateDir(e)), "700");
      // An older version may have left it readable by others: fixed on save.
      spawnSync("chmod", ["644", p]);
      saveState("backyard", loadState("backyard", e), e);
      assert.equal(mode(p), "600");
    }
    assert.deepEqual(readdirSync(stateDir(e)), ["backyard.json"], "no temporary file is left");
  });

  it("stops on a file that is not a JSON object, or of an unknown version", () => {
    const e = env();
    mkdirSync(stateDir(e), { recursive: true });
    for (const bad of ["{ not json", "[]", "null", '"x"', "42"]) {
      writeFileSync(stateFile("backyard", e), bad);
      failsWith(() => loadState("backyard", e), /is not a valid state file \(a JSON object\): repair it, or delete it/);
    }
    writeFileSync(stateFile("backyard", e), '{"version": 3}');
    failsWith(() => loadState("backyard", e), /state version 3/);
  });
});

describe("lock", () => {
  it("is exclusive while its holder lives, and released by its holder only", () => {
    const e = env();
    const l = acquireLock("backyard", { env: e });
    assert.ok(existsSync(l.path));
    const held = JSON.parse(readFileSync(l.path, "utf8"));
    assert.equal(held.pid, process.pid);
    assert.equal(held.host, hostname());
    if (process.platform !== "win32") assert.equal(mode(l.path), "600");
    failsWith(() => acquireLock("backyard", { env: e }), /another backyard-cloudflare run for backyard is in progress \(pid \d+ on .+ since .+\); if not, delete .+backyard\.lock/);
    // Other Workers have their own lock.
    acquireLock("team", { env: e }).release();
    l.release();
    assert.ok(!existsSync(l.path));
    // A lock taken over by someone else is not removed by the old holder.
    const a = acquireLock("backyard", { env: e });
    writeFileSync(a.path, JSON.stringify({ pid: 1, host: "other", startedAt: new Date().toISOString() }));
    a.release();
    assert.ok(existsSync(a.path));
  });

  it("is stale when its process is gone (same host) or it is older than 2 hours (other host)", () => {
    const e = env();
    const p = join(stateDir(e), "backyard.lock");
    mkdirSync(stateDir(e), { recursive: true });
    const dead = spawnSync(process.execPath, ["-e", ""]).pid;
    writeFileSync(p, JSON.stringify({ pid: dead, host: hostname(), startedAt: new Date().toISOString() }));
    acquireLock("backyard", { env: e }).release();
    const now = Date.now();
    writeFileSync(p, JSON.stringify({ pid: 4242, host: "elsewhere", startedAt: new Date(now - LOCK_STALE_MS + 60_000).toISOString() }));
    failsWith(() => acquireLock("backyard", { env: e }), /in progress \(pid 4242 on elsewhere/);
    writeFileSync(p, JSON.stringify({ pid: 4242, host: "elsewhere", startedAt: new Date(now - LOCK_STALE_MS - 60_000).toISOString() }));
    acquireLock("backyard", { env: e }).release();
    // Unreadable: stale only once it is not brand new.
    writeFileSync(p, "garbage");
    failsWith(() => acquireLock("backyard", { env: e, now: () => Date.now() }), /in progress \(unknown process\)/);
    acquireLock("backyard", { env: e, now: () => Date.now() + 60_000 }).release();
  });
});

describe("result file", () => {
  it("is written only when BACKYARD_CF_RESULT_FILE is set, privately, without secrets", () => {
    const e = env();
    const state = { url: "https://backyard.acme.com", workersDevUrl: "https://backyard.acme.workers.dev", deployToken: "byd_secret-value-0123456789" };
    const r = buildResult({ command: "domain add", worker: "backyard", statePath: "/s.json", state, previousUrl: "https://backyard.acme.workers.dev", authMode: "none", env: {} });
    assert.deepEqual(r, {
      schema: 1,
      command: "domain add",
      worker: "backyard",
      statePath: "/s.json",
      url: "https://backyard.acme.com",
      previousUrl: "https://backyard.acme.workers.dev",
      workersDevUrl: "https://backyard.acme.workers.dev",
      authMode: "none",
      deployToken: "state",
    });
    assert.equal(buildResult({ command: "setup", worker: "w", statePath: "", state, env: { DEPLOY_TOKEN: "x" } }).deployToken, "env");
    assert.equal(buildResult({ command: "setup", worker: "w", statePath: "", state: {}, env: {} }).deployToken, "unknown");
    assert.equal(buildResult({ command: "access enable", worker: "w", statePath: "", state: {}, authMode: "access", env: {} }).authMode, "access");
    assert.equal(buildResult({ command: "setup", worker: "w", statePath: "", state: {}, env: {} }).url, "");
    assert.equal(writeResult(r, {}), "");
    const p = join(e.XDG_CONFIG_HOME, "out", "result.json");
    assert.equal(writeResult(r, { BACKYARD_CF_RESULT_FILE: p }), p);
    const text = readFileSync(p, "utf8");
    assert.deepEqual(JSON.parse(text), r);
    assert.ok(!text.includes("byd_secret"));
    if (process.platform !== "win32") assert.equal(mode(p), "600");
  });
});
