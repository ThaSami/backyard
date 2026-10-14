// Sandboxes for the engine's tests: a copy of the engine (source layout without
// the real src/, or the packaged layout with dist/), the fake wrangler as
// node_modules/wrangler, a fake Cloudflare (helpers/fake-cloudflare.mjs) and a
// private $XDG_CONFIG_HOME. Commands run as a user would run them:
//
//   node --import helpers/fetch-hook.mjs <pkg>/bin/backyard-cloudflare.mjs <args…>
//
// non-TTY (so never interactive), NO_COLOR=1, with
//   CLOUDFLARE_API_BASE_URL / BACKYARD_CF_DOH_URL / FAKE_CF_ORIGIN → the fake,
//   FAKE_EDGE (["workers.dev", "example.com", …o.edge]), FAKE_CF_STATE, FAKE_LOG,
//   XDG_CONFIG_HOME=<root>/config, HOME=<root>/home, SYNC_LOG (the sync-assets stub).
//
//   const sb = await createSandbox({ layout: "source" | "packaged", cf: {...fake state...},
//                                    edge: ["acme.dev"], wrangler: "nested" | "hoisted" });
//   const r = await sb.run(["setup"], { CI: "true" });     // {code, out, stdout, stderr}
//   sb.calls()        wrangler calls since the last calls() (see fake-wrangler.mjs for the fields)
//   sb.allCalls()     every wrangler call
//   sb.requests()     API requests since the last requests(); sb.fake.requests: all of them
//   sb.state(w)       the engine's state file (JSON); sb.statePath(w); sb.stateMode(w) → "600";
//                     sb.hasState(w); sb.writeState(w, objectOrText) (e.g. a state "another run" left)
//   sb.result()       the BACKYARD_CF_RESULT_FILE JSON (null when absent); pass
//                     {BACKYARD_CF_RESULT_FILE: sb.resultPath} to a run to get one
//   sb.cf()           the fake Cloudflare state; sb.updateCf(s => …) (see fake-cloudflare.mjs);
//                     sb.fake: the server (addFault, requests, edgeRequests, dohRequests, writes())
//   sb.syncRuns()     how often the sync-assets stub ran (FAKE_SYNC_FAIL=1 makes it fail)
//   sb.snapshot()     a listing of the package directory (to prove nothing was written there);
//                     makeReadOnly(dir) / makeWritable(dir) for chmod -R a-w tests
//   sb.close()        stops the fake and removes the sandbox
//   sb.runTTY(args, env, {answers: [[/prompt/, "answer\n"], …]})   the same on a pseudo
//                     terminal (interactive: prompts, the hub link with the token); only
//                     when ttyAvailable() (Linux with util-linux `script`)
//
// run(args, env, {input, script, cwd, nodeArgs, timeoutMs, interrupt}): env entries set to
// undefined are removed; `script` runs another entry point (e.g. "scripts/setup.mjs");
// `interrupt: /pattern/` sends SIGINT to the run's process group (like Ctrl-C in a
// terminal: the engine and its wrangler child) once the output matches;
// the cwd is <root>/cwd (the user's directory, outside the package). Runs are
// killed after 60 s. Every run of a mutating command waits for the hub's
// readiness through the fake edge, so a test must make the hub reachable (a
// deployed Worker on workers.dev, or a custom domain in the fake) — otherwise
// setup spends 45 s waiting.
//
// Typical fake-state seeds (sb.updateCf): s.zones.push({id, name, status: "active",
// account: "acc1"}); s.workers.backyard = deployedWorker({vars, secrets, r2Bucket});
// s.tokens[t] = {label: "admin", permissions: [...TOKEN_PRESETS.dns, ...TOKEN_PRESETS.access]};
// s.access.acc1 = {org: {auth_domain: "acme.cloudflareaccess.com"}, apps: [], policies: []}.
//
// assertNoLeaks(sb, {outputs, allowInEnv}) checks the secret rules of every run:
// no secret value (deploy token, BACKYARD_SECRET, the OAuth token, API tokens of
// the fake, extra values) in any wrangler argv, in a wrangler child's env (except
// the values listed in allowInEnv, which the test put there itself), in the state
// file (tokens of the fake), or in the outputs; BACKYARD_CF_API_TOKEN never in a
// wrangler child's env.

import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { chmodSync, cpSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

import { parseJSONC } from "../../../scripts/lib/layout.mjs";
import { OAUTH_TOKEN, defaultCfState, readCfState, startFakeCloudflare, writeCfState } from "./fake-cloudflare.mjs";

const here = dirname(fileURLToPath(import.meta.url));
/** deploy/cloudflare */
export const PKG_DIR = resolve(here, "..", "..", "..");
export const FETCH_HOOK = join(here, "fetch-hook.mjs");
export const FAKE_WRANGLER = join(here, "fake-wrangler.mjs");
export const WRANGLER_VERSION = "4.149.0";

/** The engine's own files (both layouts). */
const ENGINE_SCRIPTS = ["cli.mjs", "setup.mjs", "domain.mjs", "access.mjs", "status.mjs"];


/** Copies the engine (bin/, the command scripts, scripts/lib/) into pkg. */
function copyEngine(pkg) {
  cpSync(join(PKG_DIR, "bin"), join(pkg, "bin"), { recursive: true });
  mkdirSync(join(pkg, "scripts"), { recursive: true });
  for (const f of ENGINE_SCRIPTS) cpSync(join(PKG_DIR, "scripts", f), join(pkg, "scripts", f));
  cpSync(join(PKG_DIR, "scripts", "lib"), join(pkg, "scripts", "lib"), { recursive: true });
}

/** A checkout-like package: wrangler.jsonc, package.json, a stub src/index.ts and a sync-assets stub. */
function buildSource(pkg) {
  copyEngine(pkg);
  cpSync(join(PKG_DIR, "wrangler.jsonc"), join(pkg, "wrangler.jsonc"));
  cpSync(join(PKG_DIR, "package.json"), join(pkg, "package.json"));
  mkdirSync(join(pkg, "src"), { recursive: true });
  writeFileSync(join(pkg, "src", "index.ts"), "export default { fetch: () => new Response('ok') };\n");
  writeFileSync(
    join(pkg, "scripts", "sync-assets.mjs"),
    'import { appendFileSync } from "node:fs";\n' +
      'if (process.env.SYNC_LOG) appendFileSync(process.env.SYNC_LOG, JSON.stringify(process.argv.slice(2)) + "\\n");\n' +
      'if (process.env.FAKE_SYNC_FAIL) { console.error("sync-assets: cannot build the Worker (fake)"); process.exit(1); }\n',
  );
}

/** The layout of the npm package: dist/{worker.js, wrangler.base.json, build.json}, no src/. */
function buildPackaged(pkg, version) {
  copyEngine(pkg);
  const worker = "export default { async fetch() { return new Response('ok'); } };\n";
  mkdirSync(join(pkg, "dist"), { recursive: true });
  writeFileSync(join(pkg, "dist", "worker.js"), worker);
  const base = parseJSONC(readFileSync(join(PKG_DIR, "wrangler.jsonc"), "utf8"));
  for (const k of ["$schema", "build", "env", "main", "routes"]) delete base[k];
  writeFileSync(join(pkg, "dist", "wrangler.base.json"), JSON.stringify(base, null, 2) + "\n");
  writeFileSync(
    join(pkg, "dist", "build.json"),
    JSON.stringify({ version, commit: "0000000", assetsHash: "test", wrangler: WRANGLER_VERSION, workerSha256: createHash("sha256").update(worker).digest("hex") }, null, 2) + "\n",
  );
  writeFileSync(
    join(pkg, "package.json"),
    JSON.stringify(
      {
        name: "backyard-cloudflare",
        version,
        type: "module",
        bin: { "backyard-cloudflare": "bin/backyard-cloudflare.mjs" },
        engines: { node: ">=22" },
        dependencies: { wrangler: WRANGLER_VERSION },
      },
      null,
      2,
    ) + "\n",
  );
}

/** node_modules/wrangler with the real exports map and bin, running helpers/fake-wrangler.mjs. */
function installFakeWrangler(modulesDir) {
  const d = join(modulesDir, "wrangler");
  mkdirSync(join(d, "bin"), { recursive: true });
  mkdirSync(join(d, "wrangler-dist"), { recursive: true });
  writeFileSync(
    join(d, "package.json"),
    JSON.stringify(
      {
        name: "wrangler",
        version: WRANGLER_VERSION,
        exports: {
          ".": { types: "./wrangler-dist/cli.d.ts", default: "./wrangler-dist/cli.js" },
          "./experimental-config": { types: "./wrangler-dist/experimental-config.d.mts", import: "./wrangler-dist/experimental-config.mjs" },
          "./package.json": "./package.json",
        },
        bin: { "cf-wrangler": "./bin/cf-wrangler.js", wrangler: "./bin/wrangler.js", wrangler2: "./bin/wrangler.js" },
      },
      null,
      2,
    ),
  );
  writeFileSync(join(d, "wrangler-dist", "cli.js"), "module.exports = {};\n");
  // CommonJS, like the real bin: it loads the fake (an ES module) by URL.
  writeFileSync(
    join(d, "bin", "wrangler.js"),
    `#!/usr/bin/env node\nimport(${JSON.stringify(pathToFileURL(FAKE_WRANGLER).href)}).catch((e) => { console.error(e); process.exit(70); });\n`,
  );
}

function listFiles(dir) {
  const out = [];
  const visit = (d) => {
    for (const name of readdirSync(d).sort()) {
      const p = join(d, name);
      const st = statSync(p);
      if (st.isDirectory()) visit(p);
      else out.push(`${relative(dir, p)} ${st.size} ${st.mtimeMs}`);
    }
  };
  visit(dir);
  return out;
}

function readLines(p) {
  if (!existsSync(p)) return [];
  return readFileSync(p, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
}

/**
 * @typedef {object} RunResult
 * @property {number} code
 * @property {string} out      stdout and stderr, interleaved
 * @property {string} stdout
 * @property {string} stderr
 */

/**
 * @param {{layout?: "source"|"packaged", cf?: Record<string, any>, edge?: string[], wrangler?: "nested"|"hoisted", version?: string}} [o]
 */
export async function createSandbox(o = {}) {
  const root = mkdtempSync(join(tmpdir(), "backyard-cf-test-"));
  const layout = o.layout ?? "source";
  const hoisted = o.wrangler === "hoisted";
  // Hoisted: <root>/node_modules/{backyard-cloudflare,wrangler}, like npx or a global install.
  const pkg = hoisted ? join(root, "node_modules", "backyard-cloudflare") : join(root, "pkg");
  mkdirSync(pkg, { recursive: true });
  if (layout === "packaged") buildPackaged(pkg, o.version ?? "1.2.3-test");
  else buildSource(pkg);
  installFakeWrangler(hoisted ? join(root, "node_modules") : join(pkg, "node_modules"));
  for (const d of ["config", "home", "cwd"]) mkdirSync(join(root, d), { recursive: true });

  const cfPath = join(root, "cf.json");
  writeCfState(cfPath, defaultCfState(o.cf ?? {}));
  const fake = await startFakeCloudflare({ statePath: cfPath });
  const logPath = join(root, "calls.log");
  const syncLog = join(root, "sync.log");
  const resultPath = join(root, "result.json");
  const env = {
    PATH: process.env.PATH,
    HOME: join(root, "home"),
    XDG_CONFIG_HOME: join(root, "config"),
    TMPDIR: process.env.TMPDIR ?? tmpdir(),
    NO_COLOR: "1",
    CLOUDFLARE_API_BASE_URL: fake.apiBase,
    BACKYARD_CF_DOH_URL: fake.dohUrl,
    FAKE_CF_ORIGIN: fake.origin,
    FAKE_EDGE: JSON.stringify(["workers.dev", "example.com", ...(o.edge ?? [])]),
    FAKE_CF_STATE: cfPath,
    FAKE_LOG: logPath,
    SYNC_LOG: syncLog,
  };
  let callsSeen = 0;
  let requestsSeen = 0;

  const statePath = (w = "backyard") => join(root, "config", "backyard", "cloudflare", `${w}.json`);

  const sb = {
    root,
    pkg,
    layout,
    fake,
    env,
    resultPath,
    statePath,
    state: (w = "backyard") => JSON.parse(readFileSync(statePath(w), "utf8")),
    hasState: (w = "backyard") => existsSync(statePath(w)),
    stateMode: (w = "backyard") => (statSync(statePath(w)).mode & 0o777).toString(8),
    writeState(w, obj) {
      mkdirSync(dirname(statePath(w)), { recursive: true });
      writeFileSync(statePath(w), typeof obj === "string" ? obj : JSON.stringify(obj, null, 2));
    },
    result: () => (existsSync(resultPath) ? JSON.parse(readFileSync(resultPath, "utf8")) : null),
    cf: () => readCfState(cfPath),
    /** @param {(s: Record<string, any>) => void} fn */
    updateCf(fn) {
      const s = readCfState(cfPath);
      fn(s);
      writeCfState(cfPath, s);
    },
    calls() {
      const all = readLines(logPath);
      const fresh = all.slice(callsSeen);
      callsSeen = all.length;
      return fresh;
    },
    allCalls: () => readLines(logPath),
    requests() {
      const fresh = fake.requests.slice(requestsSeen);
      requestsSeen = fake.requests.length;
      return fresh;
    },
    syncRuns: () => readLines(syncLog).length,
    snapshot: () => listFiles(pkg),
    /**
     * Runs the engine (bin/backyard-cloudflare.mjs) like a user.
     * @param {string[]} args
     * @param {Record<string, string | undefined>} [extraEnv]  undefined deletes a variable
     * @param {{input?: string, script?: string, cwd?: string, nodeArgs?: string[], timeoutMs?: number, interrupt?: RegExp}} [ro]
     * @returns {Promise<RunResult>}
     */
    run(args, extraEnv = {}, ro = {}) {
      const childEnv = { ...env, ...extraEnv };
      for (const [k, v] of Object.entries(childEnv)) if (v === undefined) delete childEnv[k];
      const entry = ro.script ? join(pkg, ro.script) : join(pkg, "bin", "backyard-cloudflare.mjs");
      return new Promise((resolveRun) => {
        const child = spawn(process.execPath, [...(ro.nodeArgs ?? []), "--import", pathToFileURL(FETCH_HOOK).href, entry, ...args], {
          cwd: ro.cwd ?? join(root, "cwd"),
          env: childEnv,
          stdio: [ro.input !== undefined ? "pipe" : "ignore", "pipe", "pipe"],
          // A process group of its own, so an interrupt reaches wrangler too (as Ctrl-C does).
          detached: Boolean(ro.interrupt),
        });
        if (ro.input !== undefined) child.stdin.end(ro.input);
        let out = "";
        let stdout = "";
        let stderr = "";
        let interrupted = false;
        const maybeInterrupt = () => {
          if (!ro.interrupt || interrupted || !ro.interrupt.test(out)) return;
          interrupted = true;
          try {
            process.kill(-child.pid, "SIGINT");
          } catch {
            /* already gone */
          }
        };
        child.stdout.on("data", (c) => {
          out += c;
          stdout += c;
          maybeInterrupt();
        });
        child.stderr.on("data", (c) => {
          out += c;
          stderr += c;
          maybeInterrupt();
        });
        const timer = setTimeout(() => child.kill("SIGKILL"), ro.timeoutMs ?? 60_000);
        child.on("close", (code) => {
          clearTimeout(timer);
          resolveRun({ code: code ?? -1, out, stdout, stderr });
        });
      });
    },
    /**
     * Runs the engine on a pseudo terminal (util-linux `script`), so it is
     * interactive: each answer is typed once the output matches its pattern.
     * Use only when ttyAvailable(). Output lines end in "\r\n".
     * @param {string[]} args
     * @param {Record<string, string | undefined>} [extraEnv]
     * @param {{answers?: [RegExp, string][], timeoutMs?: number}} [ro]
     * @returns {Promise<RunResult>}
     */
    runTTY(args, extraEnv = {}, ro = {}) {
      const childEnv = { ...env, SHELL: "/bin/sh", TERM: "dumb", ...extraEnv };
      for (const [k, v] of Object.entries(childEnv)) if (v === undefined) delete childEnv[k];
      const q = (a) => `'${String(a).replace(/'/g, "'\\''")}'`;
      const cmd = [process.execPath, "--import", pathToFileURL(FETCH_HOOK).href, join(pkg, "bin", "backyard-cloudflare.mjs"), ...args].map(q).join(" ");
      const answers = [...(ro.answers ?? [])];
      return new Promise((resolveRun) => {
        const child = spawn("script", ["-qefc", cmd, "/dev/null"], { cwd: join(root, "cwd"), env: childEnv, stdio: ["pipe", "pipe", "pipe"] });
        let out = "";
        let seen = 0;
        const onData = (c) => {
          out += c;
          for (let m; answers.length && (m = answers[0][0].exec(out.slice(seen))); ) {
            seen += m.index + m[0].length;
            child.stdin.write(answers.shift()[1]);
          }
        };
        child.stdout.on("data", onData);
        child.stderr.on("data", onData);
        const timer = setTimeout(() => child.kill("SIGKILL"), ro.timeoutMs ?? 60_000);
        child.on("close", (code) => {
          clearTimeout(timer);
          resolveRun({ code: code ?? -1, out, stdout: out, stderr: "" });
        });
      });
    },
    async close() {
      await fake.close();
      makeWritable(root);
      rmSync(root, { recursive: true, force: true });
    },
  };
  return sb;
}

/** Whether runTTY works here (Linux with util-linux `script`). */
export function ttyAvailable() {
  if (process.platform !== "linux") return false;
  const r = spawnSync("script", ["--version"], { encoding: "utf8" });
  return r.status === 0 && /util-linux/.test(r.stdout + r.stderr);
}

/** Makes a tree writable again (a test may have made the package read-only). */
export function makeWritable(dir) {
  const visit = (p) => {
    let st;
    try {
      st = lstatSync(p);
    } catch {
      return;
    }
    if (st.isSymbolicLink()) return;
    if (st.isDirectory()) {
      chmodSync(p, 0o755);
      for (const name of readdirSync(p)) visit(join(p, name));
    } else chmodSync(p, st.mode | 0o200);
  };
  visit(dir);
}

/** Makes a tree read-only (chmod -R a-w). */
export function makeReadOnly(dir) {
  const visit = (p) => {
    const st = lstatSync(p);
    if (st.isSymbolicLink()) return;
    if (st.isDirectory()) for (const name of readdirSync(p)) visit(join(p, name));
    chmodSync(p, st.mode & ~0o222);
  };
  visit(dir);
}

/**
 * Every secret value a sandbox knows: the Workers' secrets, the OAuth token
 * and every API token / key of the fake, plus `extra`.
 * @param {Awaited<ReturnType<typeof createSandbox>>} sb
 * @param {string[]} [extra]
 */
export function knownSecrets(sb, extra = []) {
  const s = sb.cf();
  const values = new Set([OAUTH_TOKEN, ...extra]);
  for (const w of Object.values(s.workers ?? {})) for (const v of Object.values(w.secrets ?? {})) values.add(String(v));
  for (const t of Object.keys(s.tokens ?? {})) values.add(t);
  for (const k of Object.keys(s.apiKeys ?? {})) values.add(k);
  return [...values].filter((v) => v.length >= 8);
}

/**
 * Asserts the secret rules (see the header).
 * @param {Awaited<ReturnType<typeof createSandbox>>} sb
 * @param {{outputs?: string[], allowInEnv?: string[], extra?: string[]}} [o]
 */
export function assertNoLeaks(sb, o = {}) {
  const secrets = knownSecrets(sb, o.extra ?? []);
  const allowed = new Set(o.allowInEnv ?? []);
  for (const c of sb.allCalls()) {
    assert.ok(!("BACKYARD_CF_API_TOKEN" in (c.env ?? {})), `BACKYARD_CF_API_TOKEN reached wrangler ${c.args.join(" ")}`);
    for (const v of secrets) {
      assert.ok(!c.args.some((a) => String(a).includes(v)), `a secret is on the argv of wrangler ${c.args[0]}`);
      if (allowed.has(v)) continue;
      for (const [k, ev] of Object.entries(c.env ?? {})) assert.ok(!String(ev).includes(v), `a secret is in the env (${k}) of wrangler ${c.args[0]}`);
    }
  }
  const stateDir = join(sb.root, "config", "backyard", "cloudflare");
  if (existsSync(stateDir)) {
    const tokens = new Set([OAUTH_TOKEN, ...Object.keys(sb.cf().tokens ?? {}), ...(o.extra ?? [])].filter((v) => v.length >= 8));
    for (const f of readdirSync(stateDir)) {
      const text = readFileSync(join(stateDir, f), "utf8");
      for (const v of tokens) assert.ok(!text.includes(v), `an API token is in the state file ${f}`);
    }
  }
  if (existsSync(sb.resultPath)) {
    const text = readFileSync(sb.resultPath, "utf8");
    for (const v of secrets) assert.ok(!text.includes(v), "a secret is in the result file");
  }
  for (const out of o.outputs ?? []) for (const v of secrets) assert.ok(!out.includes(v), `a secret is in the output:\n${out}`);
}
