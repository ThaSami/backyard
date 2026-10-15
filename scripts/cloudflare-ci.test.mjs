// Tests for scripts/cloudflare-ci.mjs: node --test scripts/*.test.mjs
//
// The deploy tests run the real script against a fake wrangler (a small node
// program that records its arguments) and, for the auth guard's REST calls, the
// engine's fake Cloudflare API (deploy/cloudflare/test/scripts/helpers/
// fake-cloudflare.mjs) on 127.0.0.1. The engine's test fetch hook is loaded
// in-process, so the Worker's own workers.dev answers come from the fake edge
// and any other non-loopback request fails like an unknown host: no Cloudflare
// account or network access is needed, and nothing reaches the real API.
import { after, test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, readFileSync, existsSync, mkdirSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:http";

import {
  CIError,
  GENERATED_CONFIG,
  deploy,
  deployArgVars,
  effectiveConfig,
  hubURL,
  parseAISwitch,
  isMissingWorker,
  parseDeployArgs,
  parseJSONC,
  parseSecretList,
  parseVars,
  planSecrets,
} from "./cloudflare-ci.mjs";
import { TOKEN_PRESETS, defaultCfState, deployedWorker, startFakeCloudflare } from "../deploy/cloudflare/test/scripts/helpers/fake-cloudflare.mjs";

const scratch = mkdtempSync(join(tmpdir(), "backyard-cf-ci-test-"));
const fake = await startFakeCloudflare({ statePath: join(scratch, "cf-state.json") });
process.env.FAKE_CF_ORIGIN = fake.origin;
process.env.FAKE_EDGE = JSON.stringify(["workers.dev", "example.com"]);
process.env.FAKE_TLS_FAIL = "";
await import("../deploy/cloudflare/test/scripts/helpers/fetch-hook.mjs");

after(async () => {
  await fake.close();
  rmSync(scratch, { recursive: true, force: true });
});

/** The CI token (label "ci" in the fake's request log; the "Edit Cloudflare Workers" template). */
const CI_TOKEN = "cf-token";
const AUD = "a".repeat(64);
const ACCESS_VARS = { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme.cloudflareaccess.com", ACCESS_AUD: AUD, BASE_DOMAIN: "backyard.example.com" };

test("parseJSONC handles comments, trailing commas and comment markers in strings", () => {
  const text = `// header
{
  "name": "backyard", // trailing comment
  /* block
     comment */
  "url": "https://example.com/a//b", "glob": "/* not a comment */",
  "list": [1, 2, /* c */ ],
  "vars": { "A": "x,}", "B": "y", },
}`;
  assert.deepEqual(parseJSONC(text), {
    name: "backyard",
    url: "https://example.com/a//b",
    glob: "/* not a comment */",
    list: [1, 2],
    vars: { A: "x,}", B: "y" },
  });
  assert.throws(() => parseJSONC("{ bad }"));
});

test("parseJSONC reads the Worker's wrangler.jsonc", () => {
  const p = new URL("../deploy/cloudflare/wrangler.jsonc", import.meta.url);
  if (!existsSync(p)) return;
  const cfg = parseJSONC(readFileSync(p, "utf8"));
  assert.equal(typeof cfg.name, "string");
  assert.ok(cfg.durable_objects?.bindings?.length >= 2);
  // The free plan needs SQLite-backed Durable Objects.
  assert.ok(cfg.migrations?.some((m) => (m.new_sqlite_classes ?? []).length > 0), "migrations must use new_sqlite_classes");
});

test("parseVars", async (t) => {
  const cases = [
    { name: "empty", raw: "", want: {} },
    { name: "undefined", raw: undefined, want: {} },
    { name: "strings", raw: '{"URL_MODE":"host","BASE_DOMAIN":"example.com"}', want: { URL_MODE: "host", BASE_DOMAIN: "example.com" } },
    { name: "non-strings are JSON-encoded", raw: '{"LIMITS":{"ai_per_day":5},"FILE_INLINE_MAX":1024,"X":true}', want: { LIMITS: '{"ai_per_day":5}', FILE_INLINE_MAX: "1024", X: "true" } },
    { name: "null drops", raw: '{"A":null}', want: {} },
    { name: "bad json", raw: "{", err: /not valid JSON/ },
    { name: "array", raw: "[]", err: /JSON object/ },
    { name: "bad name", raw: '{"lower":"x"}', err: /invalid variable name/ },
    { name: "secret as var", raw: '{"DEPLOY_TOKEN":"x"}', err: /is a secret/ },
    { name: "backyard secret as var", raw: '{"BACKYARD_SECRET":"x"}', err: /is a secret/ },
  ];
  for (const c of cases) {
    await t.test(c.name, () => {
      if (c.err) assert.throws(() => parseVars(c.raw), (e) => e instanceof CIError && c.err.test(e.message));
      else assert.deepEqual(parseVars(c.raw), c.want);
    });
  }
});

test("parseDeployArgs", () => {
  assert.deepEqual(parseDeployArgs(""), []);
  assert.deepEqual(parseDeployArgs("  --var AUTH_MODE:access   --minify "), ["--var", "AUTH_MODE:access", "--minify"]);
  assert.throws(() => parseDeployArgs("--var DEPLOY_TOKEN:abc"), /is a secret/);
  assert.throws(() => parseDeployArgs("--var=BACKYARD_SECRET:abc"), /is a secret/);
  // Written with "=": refused too, and the value is not in the message.
  assert.throws(() => parseDeployArgs("--var DEPLOY_TOKEN=byd_value-0123456789"), (e) => /DEPLOY_TOKEN is a secret/.test(e.message) && !e.message.includes("byd_value"));
  assert.throws(() => parseDeployArgs("--var=WAREHOUSE_TOKEN=abc"), /is a secret/);
  assert.deepEqual(parseDeployArgs("--var SERVER_NAME=Team"), ["--var", "SERVER_NAME=Team"]);
  assert.throws(() => parseDeployArgs("--secrets-file x.json"), /secrets-file/);
  // Routes, custom domains and another config, environment or Worker would get past the guard.
  for (const [raw, re] of [
    ["--route example.com/*", /may not set --route: routes and custom domains are managed by `backyard cloudflare domain add`/],
    ["--routes=example.com/*", /may not set --routes/],
    ["--domain backyard.example.com", /may not set --domain/],
    ["--domains x", /may not set --domains/],
    ["--config other.jsonc", /may not set --config/],
    ["-c other.jsonc", /may not set -c/],
    ["--env production", /may not set --env/],
    ["-e=production", /may not set -e/],
    ["--name other", /BACKYARD_CF_WORKER_NAME/],
  ]) {
    assert.throws(() => parseDeployArgs(raw), (e) => e instanceof CIError && re.test(e.message), raw);
  }
  assert.deepEqual(parseDeployArgs("--minify --keep-vars"), ["--minify", "--keep-vars"]);
});

test("deployArgVars", () => {
  assert.deepEqual(deployArgVars([]), {});
  assert.deepEqual(deployArgVars(["--minify", "--var", "AUTH_MODE:none", "URL:https://x:1/", "--var=LIMITS:{}", "--dry"]), {
    AUTH_MODE: "none",
    URL: "https://x:1/",
    LIMITS: "{}",
  });
  assert.deepEqual(deployArgVars(["--var", "--minify"]), {});
});

test("effectiveConfig", () => {
  const base = { name: "backyard", vars: { URL_MODE: "path", A: "1" }, r2_buckets: [{ binding: "OTHER", bucket_name: "o" }] };
  assert.equal(effectiveConfig(base, {}), null);
  const got = effectiveConfig(base, { vars: { URL_MODE: "host" }, bucket: "files" });
  assert.deepEqual(got.vars, { URL_MODE: "host", A: "1" });
  assert.deepEqual(got.r2_buckets, [{ binding: "OTHER", bucket_name: "o" }, { binding: "R2", bucket_name: "files" }]);
  assert.deepEqual(base.vars, { URL_MODE: "path", A: "1" }, "input is not modified");
  // Never routes: a config with routes is always rewritten without them.
  const routed = { ...base, routes: [{ pattern: "backyard.example.com", custom_domain: true }], route: "x.example.com/*" };
  const clean = effectiveConfig(routed, {});
  assert.ok(clean, "a config with routes is rewritten");
  assert.equal("routes" in clean, false);
  assert.equal("route" in clean, false);
  assert.deepEqual(clean.vars, base.vars);
  assert.equal(effectiveConfig(routed, { vars: { A: "2" } }).routes, undefined);
  assert.ok(routed.routes, "input is not modified");
});

test("parseAISwitch", () => {
  for (const v of [undefined, "", " ", "true", "TRUE"]) assert.equal(parseAISwitch(v), true, `${JSON.stringify(v)} is on`);
  for (const v of ["false", " False "]) assert.equal(parseAISwitch(v), false, `${JSON.stringify(v)} is off`);
  for (const v of ["no", "0", "off", "yes"]) assert.throws(() => parseAISwitch(v), /BACKYARD_CF_AI must be "true" or "false"/);
});

test("effectiveConfig without AI drops the binding and sets AI_ENABLED", () => {
  const base = { name: "backyard", ai: { binding: "AI", remote: true }, vars: { URL_MODE: "path" } };
  assert.equal(effectiveConfig(base, { ai: true }), null, "AI on changes nothing");
  const got = effectiveConfig(base, { ai: false });
  assert.equal("ai" in got, false, "no Workers AI binding");
  assert.deepEqual(got.vars, { URL_MODE: "path", AI_ENABLED: "false" });
  assert.deepEqual(base.ai, { binding: "AI", remote: true }, "input is not modified");
  assert.deepEqual(base.vars, { URL_MODE: "path" }, "input vars are not modified");
});

test("hubURL", async (t) => {
  const out = [
    JSON.stringify({ type: "wrangler-session", version: 1 }),
    "not json",
    JSON.stringify({ type: "deploy", targets: ["example.com/*", "https://backyard.acme.workers.dev", "backyard.example.com (custom domain)"] }),
  ].join("\n");
  const cases = [
    { name: "public url wins", vars: { PUBLIC_URL: "https://by.example.com/", BASE_DOMAIN: "x.com" }, want: "https://by.example.com" },
    { name: "base domain", vars: { BASE_DOMAIN: "by.example.com." }, want: "https://by.example.com" },
    { name: "workers.dev target", vars: {}, want: "https://backyard.acme.workers.dev" },
    { name: "nothing", vars: {}, out: "", want: "" },
  ];
  for (const c of cases) {
    await t.test(c.name, () => assert.equal(hubURL(c.vars, c.out ?? out), c.want));
  }
});

test("parseSecretList and isMissingWorker", () => {
  const names = parseSecretList('▲ [WARNING] proxy\n[\n  {"name": "BACKYARD_SECRET", "type": "secret_text"},\n  {"name": "DEPLOY_TOKEN", "type": "secret_text"}\n]\n');
  assert.deepEqual([...names].sort(), ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
  assert.deepEqual([...parseSecretList("[]")], []);
  assert.throws(() => parseSecretList("Authentication error"), CIError);
  assert.equal(isMissingWorker('✘ [ERROR] Worker "backyard" not found.\n\nIf this is a new Worker, run `wrangler deploy` first'), true);
  assert.equal(isMissingWorker("This Worker does not exist on your account. [code: 10007]"), true);
  assert.equal(isMissingWorker("Authentication error [code: 10000]"), false);
  assert.equal(isMissingWorker("fetch failed: getaddrinfo ENOTFOUND api.cloudflare.com"), false);
});

test("parseSecretList accepts only a JSON list of {name} objects", () => {
  const list = JSON.stringify([{ name: "BACKYARD_SECRET", type: "secret_text" }, { name: "DEPLOY_TOKEN", type: "secret_text" }], null, 2);
  const names = (out) => [...parseSecretList(out)].sort();
  const both = ["BACKYARD_SECRET", "DEPLOY_TOKEN"];
  assert.deepEqual(names(list), both);
  assert.deepEqual(names(`${list}\r\n`), both);
  assert.deepEqual(names(`[WARNING] a notice [x]\n${list}\n`), both);
  assert.deepEqual(names(`${list}\n▲ [WARNING] after the list [x]\n`), both);
  assert.deepEqual(names("▲ [WARNING] Proxy environment variables detected\n[]\n"), []);
  // A stray "[]" line never hides the real list (an empty list would
  // regenerate, i.e. rotate, BACKYARD_SECRET).
  assert.deepEqual(names(`[]\n${list}\n`), both);
  assert.deepEqual(names(`${list}\n[]\n`), both);
  assert.deepEqual(names(`[]\n▲ [WARNING] notice\n${list}`), both);
  for (const bad of [
    "",
    "garbage",
    "{}",
    "x []",
    "[] trailing",
    "[1, 2]",
    '["BACKYARD_SECRET"]',
    '[{"type":"secret_text"}]',
    '[{"name":""}]',
    '[{"name":7}]',
    "[null]",
    "[[]]",
    // "[]" next to a list in an unknown format is ambiguous: never "no secrets".
    '[]\n["BACKYARD_SECRET"]\n',
    '[{"Name":"BACKYARD_SECRET"}]\n[]\n',
  ]) {
    assert.throws(() => parseSecretList(bad), CIError, JSON.stringify(bad));
  }
});

test("planSecrets", async (t) => {
  const random = () => "r".repeat(64);
  const long = "s".repeat(40);
  await t.test("first deploy generates BACKYARD_SECRET and warns without a deploy token", () => {
    const p = planSecrets({}, new Set(), {}, random);
    assert.deepEqual(p.secrets, { BACKYARD_SECRET: "r".repeat(64) });
    assert.equal(p.generated, true);
    assert.equal(p.warnings.length, 1);
    assert.match(p.warnings[0], /No DEPLOY_TOKEN/);
  });
  await t.test("existing secrets are kept", () => {
    const p = planSecrets({}, new Set(["BACKYARD_SECRET", "DEPLOY_TOKEN"]), {}, random);
    assert.deepEqual(p.secrets, {});
    assert.equal(p.generated, false);
    assert.deepEqual(p.warnings, []);
  });
  await t.test("provided secrets are uploaded", () => {
    const env = { BACKYARD_SECRET: long, DEPLOY_TOKEN: "byd_" + "t".repeat(30), OPENAI_API_KEY: "sk", UNRELATED: "x" };
    const p = planSecrets(env, new Set(), {}, random);
    assert.deepEqual(Object.keys(p.secrets).sort(), ["BACKYARD_SECRET", "DEPLOY_TOKEN", "OPENAI_API_KEY"]);
    assert.equal(p.generated, false);
  });
  await t.test("short BACKYARD_SECRET is refused", () => {
    assert.throws(() => planSecrets({ BACKYARD_SECRET: "short" }, new Set(), {}, random), /at least 32/);
  });
  await t.test("short DEPLOY_TOKEN warns", () => {
    const p = planSecrets({ DEPLOY_TOKEN: "abc" }, new Set(["BACKYARD_SECRET"]), {}, random);
    assert.match(p.warnings.join("\n"), /shorter than 16/);
  });
  await t.test("no token warning with Access or anonymous deploys", () => {
    assert.deepEqual(planSecrets({}, new Set(["BACKYARD_SECRET"]), { AUTH_MODE: "access" }, random).warnings, []);
    assert.deepEqual(planSecrets({}, new Set(["BACKYARD_SECRET"]), { ALLOW_ANONYMOUS_DEPLOY: "true" }, random).warnings, []);
  });
});

// ---------------------------------------------------------------------------
// deploy() against a fake wrangler

const FAKE_WRANGLER = `
const fs = require("fs");
const args = process.argv.slice(2);
const state = JSON.parse(fs.readFileSync(process.env.FAKE_STATE, "utf8"));
state.calls.push({ args, cwd: process.cwd(), token: process.env.CLOUDFLARE_API_TOKEN || "" });
const save = () => fs.writeFileSync(process.env.FAKE_STATE, JSON.stringify(state));
if (args[0] === "secret" && args[1] === "list") {
  save();
  if (state.listError) { console.error(state.listError); process.exit(1); }
  if (!state.exists) { console.error('✘ [ERROR] Worker "' + args[args.indexOf("--name") + 1] + '" not found.'); process.exit(1); }
  const list = JSON.stringify(state.secrets.map((name) => ({ name, type: "secret_text" })), null, 2);
  if (state.listStdout !== undefined) { process.stdout.write(state.listStdout.replace("%LIST%", list)); process.exit(0); }
  console.log(list);
  process.exit(0);
}
if (args[0] === "r2") { save(); if (state.r2Error) { console.error(state.r2Error); process.exit(1); } process.exit(0); }
if (args[0] === "deploy") {
  const i = args.indexOf("--secrets-file");
  if (i >= 0) {
    const file = args[i + 1];
    state.secretsFileMode = fs.statSync(file).mode & 0o777;
    state.uploaded = JSON.parse(fs.readFileSync(file, "utf8"));
  }
  const c = args.indexOf("--config");
  if (c >= 0) state.config = fs.readFileSync(args[c + 1], "utf8");
  save();
  if (state.deployError) { console.error(state.deployError); process.exit(1); }
  if (process.env.WRANGLER_OUTPUT_FILE_PATH && !args.includes("--dry-run")) {
    fs.appendFileSync(process.env.WRANGLER_OUTPUT_FILE_PATH, JSON.stringify({ type: "deploy", targets: [state.url] }) + "\\n");
  }
  console.log("Deployed");
  process.exit(0);
}
save();
console.error("unexpected wrangler call: " + args.join(" "));
process.exit(1);
`;

const DEFAULT_CONFIG = `// test config\n{\n  "name": "backyard",\n  "main": "src/index.ts",\n  "vars": { "URL_MODE": "path", "AUTH_MODE": "none", },\n}\n`;

/**
 * A Worker project, a fake wrangler (state: what it reports) and a fresh fake
 * Cloudflare API (cf: top-level keys of its state, e.g. workers; by default the
 * Worker "backyard" with AUTH_MODE=none when the fake wrangler says it exists).
 * The env has the CI token and CLOUDFLARE_ACCOUNT_ID=acc1.
 */
function fixture(state = {}, { cf: cfState = {}, config = DEFAULT_CONFIG } = {}) {
  const dir = mkdtempSync(join(scratch, "fixture-"));
  const cf = join(dir, "cf");
  mkdirSync(cf);
  writeFileSync(join(cf, "wrangler.jsonc"), config);
  fake.clearFaults();
  const workers = cfState.workers ?? (state.exists ? { backyard: deployedWorker({ vars: { AUTH_MODE: "none" } }) } : {});
  fake.update((s) => {
    for (const k of Object.keys(s)) delete s[k];
    Object.assign(s, defaultCfState({ ...cfState, workers }));
    s.tokens[CI_TOKEN] = { label: "ci", permissions: [...TOKEN_PRESETS.workers] };
  });
  const firstRequest = fake.requests.length;
  const wrangler = join(dir, "wrangler.cjs");
  writeFileSync(wrangler, FAKE_WRANGLER);
  const statePath = join(dir, "state.json");
  writeFileSync(statePath, JSON.stringify({ calls: [], exists: false, secrets: [], url: "https://backyard.acme.workers.dev", ...state }));
  const outputs = join(dir, "github-output");
  const summary = join(dir, "summary.md");
  const env = {
    PATH: process.env.PATH,
    BACKYARD_CF_DIR: cf,
    WRANGLER_BIN: wrangler,
    FAKE_STATE: statePath,
    CLOUDFLARE_API_TOKEN: CI_TOKEN,
    CLOUDFLARE_ACCOUNT_ID: "acc1",
    CLOUDFLARE_API_BASE_URL: fake.apiBase,
    BACKYARD_CF_WAIT_SECONDS: "0",
    GITHUB_OUTPUT: outputs,
    GITHUB_STEP_SUMMARY: summary,
    RUNNER_TEMP: dir,
  };
  return {
    dir,
    cf,
    env,
    state: () => JSON.parse(readFileSync(statePath, "utf8")),
    outputs: () => (existsSync(outputs) ? readFileSync(outputs, "utf8") : ""),
    summary: () => (existsSync(summary) ? readFileSync(summary, "utf8") : ""),
    /** The fake API requests of this fixture. */
    requests: () => fake.requests.slice(firstRequest),
    /** The wrangler commands this fixture ran ("secret list", "r2 bucket create", "deploy"). */
    commands() {
      return this.state().calls.map((c) => (c.args[0] === "deploy" ? "deploy" : c.args.slice(0, c.args[0] === "r2" ? 3 : 2).join(" ")));
    },
  };
}

const quiet = () => {};

/** A log that keeps its lines. */
function recorder() {
  const lines = [];
  const log = (...a) => lines.push(a.join(" "));
  log.text = () => lines.join("\n");
  return log;
}

test("deploy: first deploy generates BACKYARD_SECRET and uploads it with the version", async () => {
  const f = fixture();
  const url = await deploy({ env: f.env, log: quiet });
  assert.equal(url, "https://backyard.acme.workers.dev");
  const s = f.state();
  const deployCall = s.calls.find((c) => c.args[0] === "deploy");
  assert.ok(deployCall, "wrangler deploy ran");
  assert.deepEqual(deployCall.args.slice(0, 3), ["deploy", "--name", "backyard"]);
  assert.ok(!deployCall.args.includes("--config"), "no generated config without overrides");
  assert.deepEqual(Object.keys(s.uploaded), ["BACKYARD_SECRET"]);
  assert.match(s.uploaded.BACKYARD_SECRET, /^[0-9a-f]{64}$/);
  assert.equal(s.secretsFileMode, 0o600);
  assert.match(f.outputs(), /^worker=backyard$/m);
  assert.match(f.outputs(), /^url=https:\/\/backyard\.acme\.workers\.dev$/m);
  assert.match(f.summary(), /No DEPLOY_TOKEN/);
  assert.ok(!f.summary().includes(s.uploaded.BACKYARD_SECRET), "the secret is not in the summary");
  // Temporary files are removed.
  assert.ok(!existsSync(join(f.cf, GENERATED_CONFIG)));
});

test("deploy: upgrade keeps existing secrets and applies overrides", async () => {
  const f = fixture({ exists: true, secrets: ["BACKYARD_SECRET", "DEPLOY_TOKEN"] }, { cf: { workers: { team: deployedWorker({ vars: { AUTH_MODE: "none" } }) } } });
  f.env.BACKYARD_CF_WORKER_NAME = "team";
  f.env.BACKYARD_CF_VARS = '{"URL_MODE":"host","BASE_DOMAIN":"by.example.com","LIMITS":{"ai_per_day":5}}';
  f.env.BACKYARD_CF_R2_BUCKET = "team-files";
  f.env.BACKYARD_CF_DEPLOY_ARGS = "--minify";
  f.env.ANTHROPIC_API_KEY = "sk-ant-test";
  const url = await deploy({ env: f.env, log: quiet });
  assert.equal(url, "https://by.example.com");
  const s = f.state();
  assert.deepEqual(s.uploaded, { ANTHROPIC_API_KEY: "sk-ant-test" });
  const r2 = s.calls.find((c) => c.args[0] === "r2");
  assert.deepEqual(r2.args, ["r2", "bucket", "create", "team-files"]);
  const d = s.calls.find((c) => c.args[0] === "deploy");
  assert.deepEqual(d.args.slice(0, 3), ["deploy", "--name", "team"]);
  assert.equal(d.args.at(-1), "--minify");
  const cfg = parseJSONC(s.config);
  assert.deepEqual(cfg.vars, { URL_MODE: "host", AUTH_MODE: "none", BASE_DOMAIN: "by.example.com", LIMITS: '{"ai_per_day":5}' });
  assert.deepEqual(cfg.r2_buckets, [{ binding: "R2", bucket_name: "team-files" }]);
  assert.ok(!existsSync(join(f.cf, GENERATED_CONFIG)), "generated config is removed");
  assert.equal(f.summary().includes("Warning"), false);
});

test("deploy: BACKYARD_CF_AI=false deploys without the Workers AI binding", async () => {
  const config = `{\n  "name": "backyard",\n  "main": "src/index.ts",\n  "ai": { "binding": "AI", "remote": true },\n  "vars": { "URL_MODE": "path", "AUTH_MODE": "none" },\n}\n`;
  const off = fixture({ exists: true, secrets: ["BACKYARD_SECRET", "DEPLOY_TOKEN"] }, { config });
  off.env.BACKYARD_CF_AI = "false";
  await deploy({ env: off.env, log: quiet });
  const cfg = parseJSONC(off.state().config);
  assert.equal("ai" in cfg, false, "the deployed config has no ai binding");
  assert.equal(cfg.vars.AI_ENABLED, "false");

  const on = fixture({ exists: true, secrets: ["BACKYARD_SECRET", "DEPLOY_TOKEN"] }, { config });
  await deploy({ env: on.env, log: quiet });
  const d = on.state().calls.find((c) => c.args[0] === "deploy");
  assert.ok(!d.args.includes("--config"), "AI on (the default) deploys wrangler.jsonc as is, binding included");

  for (const [key, value] of [["BACKYARD_CF_VARS", '{"AI_ENABLED":"true"}'], ["BACKYARD_CF_DEPLOY_ARGS", "--var AI_ENABLED:true"]]) {
    const clash = fixture({ exists: true, secrets: ["BACKYARD_SECRET"] }, { config });
    clash.env.BACKYARD_CF_AI = "false";
    clash.env[key] = value;
    await assert.rejects(deploy({ env: clash.env, log: quiet }), /AI_ENABLED is set by BACKYARD_CF_AI=false/, key);
    assert.deepEqual(clash.state().calls, [], `${key}: refused before wrangler runs`);
  }
  const bad = fixture({}, { config });
  bad.env.BACKYARD_CF_AI = "off";
  await assert.rejects(deploy({ env: bad.env, log: quiet }), /BACKYARD_CF_AI must be "true" or "false"/);
});

test("deploy: an existing R2 bucket is fine, other R2 errors fail", async () => {
  const ok = fixture({ exists: true, secrets: ["BACKYARD_SECRET", "DEPLOY_TOKEN"], r2Error: "The bucket you tried to create already exists, and you own it. [code: 10004]" });
  ok.env.BACKYARD_CF_R2_BUCKET = "files";
  await deploy({ env: ok.env, log: quiet });
  const bad = fixture({ exists: true, secrets: ["BACKYARD_SECRET"], r2Error: "Please enable R2 through the Cloudflare Dashboard. [code: 10042]" });
  bad.env.BACKYARD_CF_R2_BUCKET = "files";
  await assert.rejects(deploy({ env: bad.env, log: quiet }), /could not create the R2 bucket/);
  assert.equal(bad.state().calls.some((c) => c.args[0] === "deploy"), false, "no deploy after a failed bucket");
});

test("deploy: a failing secret listing never rotates BACKYARD_SECRET", async () => {
  const f = fixture({ exists: true, listError: "✘ [ERROR] Authentication error [code: 10000]" });
  await assert.rejects(deploy({ env: f.env, log: quiet }), /could not list the secrets/);
  assert.equal(f.state().calls.some((c) => c.args[0] === "deploy"), false);
});

test("deploy: a stray [] in the secret listing never rotates BACKYARD_SECRET", async () => {
  const f = fixture({ exists: true, secrets: ["BACKYARD_SECRET", "DEPLOY_TOKEN"], listStdout: "[]\n▲ [WARNING] Proxy environment variables detected\n%LIST%\n" });
  await deploy({ env: f.env, log: quiet });
  const s = f.state();
  assert.ok(s.calls.some((c) => c.args[0] === "deploy"), "wrangler deploy ran");
  assert.equal(s.uploaded, undefined, "no secret is uploaded (BACKYARD_SECRET is kept)");
});

test("deploy: a secret listing in an unknown format fails without deploying", async () => {
  for (const listStdout of ['["BACKYARD_SECRET","DEPLOY_TOKEN"]\n', '[]\n[{"secret":"BACKYARD_SECRET"}]\n', "[] (no secrets)\n"]) {
    const f = fixture({ exists: true, secrets: ["BACKYARD_SECRET"], listStdout });
    await assert.rejects(deploy({ env: f.env, log: quiet }), CIError, listStdout);
    assert.equal(f.state().calls.some((c) => c.args[0] === "deploy"), false, listStdout);
  }
});

test("deploy: requires CLOUDFLARE_API_TOKEN unless dry run", async () => {
  const f = fixture();
  delete f.env.CLOUDFLARE_API_TOKEN;
  await assert.rejects(deploy({ env: f.env, log: quiet }), /CLOUDFLARE_API_TOKEN/);
  assert.deepEqual(f.state().calls, []);
});

test("deploy: dry run bundles without credentials or secrets", async () => {
  const f = fixture();
  delete f.env.CLOUDFLARE_API_TOKEN;
  f.env.BACKYARD_SECRET = "s".repeat(40);
  const url = await deploy({ env: f.env, dryRun: true, log: quiet });
  assert.equal(url, "");
  const s = f.state();
  assert.equal(s.calls.length, 1);
  assert.ok(s.calls[0].args.includes("--dry-run"));
  assert.ok(!s.calls[0].args.includes("--secrets-file"));
  assert.equal(s.uploaded, undefined);
  assert.equal(f.outputs(), "");
});

test("deploy: wrangler failure is reported", async () => {
  const f = fixture({ deployError: "✘ [ERROR] You need to register a workers.dev subdomain before publishing" });
  await assert.rejects(deploy({ env: f.env, log: quiet }), /register a workers\.dev subdomain/);
  const g = fixture({ deployError: "boom" });
  await assert.rejects(deploy({ env: g.env, log: quiet }), /wrangler deploy failed/);
});

test("deploy: rejects bad names before calling wrangler", async () => {
  for (const [k, v, re] of [
    ["BACKYARD_CF_WORKER_NAME", "Bad_Name", /invalid Worker name/],
    ["BACKYARD_CF_R2_BUCKET", "UPPER", /invalid R2 bucket/],
    ["BACKYARD_CF_VARS", '{"OPENAI_API_KEY":"x"}', /is a secret/],
  ]) {
    const f = fixture();
    f.env[k] = v;
    await assert.rejects(deploy({ env: f.env, log: quiet }), re);
    assert.deepEqual(f.state().calls, []);
  }
});

test("deploy: waits for readyz", async () => {
  let hits = 0;
  const server = createServer((req, res) => {
    hits++;
    res.writeHead(req.url === "/__backyard/readyz" ? 200 : 404).end("ok");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  try {
    const f = fixture({ exists: true, secrets: ["BACKYARD_SECRET", "DEPLOY_TOKEN"] });
    f.env.BACKYARD_CF_VARS = JSON.stringify({ PUBLIC_URL: `http://127.0.0.1:${server.address().port}` });
    f.env.BACKYARD_CF_WAIT_SECONDS = "5";
    await deploy({ env: f.env, log: quiet });
    assert.ok(hits >= 1);
  } finally {
    server.close();
  }
});

test("the script is executable as a CLI", async () => {
  const { spawnSync } = await import("node:child_process");
  const script = new URL("./cloudflare-ci.mjs", import.meta.url).pathname;
  const help = spawnSync(process.execPath, [script, "--help"], { encoding: "utf8" });
  assert.equal(help.status, 0);
  assert.match(help.stdout, /usage:/);
  const bad = spawnSync(process.execPath, [script, "nope"], { encoding: "utf8" });
  assert.equal(bad.status, 2);
  const f = fixture();
  delete f.env.CLOUDFLARE_API_TOKEN;
  const run = spawnSync(process.execPath, [script, "deploy"], { encoding: "utf8", env: { ...f.env, GITHUB_ACTIONS: "true" } });
  assert.equal(run.status, 1);
  assert.match(run.stdout, /::error title=Backyard on Cloudflare::set CLOUDFLARE_API_TOKEN/);
  assert.ok(statSync(script).isFile());
});

// ---------------------------------------------------------------------------
// The auth guard (live settings from the fake API)

const SECRETS = ["BACKYARD_SECRET", "DEPLOY_TOKEN"];
const liveAccess = (vars = ACCESS_VARS) => ({ workers: { backyard: deployedWorker({ vars, secrets: { BACKYARD_SECRET: "s".repeat(64), DEPLOY_TOKEN: "byd_live" } }) } });
const settingsPath = "/accounts/acc1/workers/scripts/backyard/settings";

test("guard: refuses a deploy that would turn the live Cloudflare Access off; nothing is changed", async () => {
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  f.env.BACKYARD_CF_R2_BUCKET = "files";
  await assert.rejects(deploy({ env: f.env, log: quiet }), (e) => {
    assert.ok(e instanceof CIError, String(e));
    assert.match(e.message, /Worker backyard is live with Cloudflare Access \(team acme\.cloudflareaccess\.com\), and this deploy would turn it off \(AUTH_MODE=none\)\. Nothing was changed\./);
    assert.ok(
      e.message.includes(`put it into BACKYARD_CF_VARS: ${JSON.stringify(ACCESS_VARS)}`),
      `the remedy shows the live values to keep:\n${e.message}`,
    );
    assert.match(e.message, /run this one deploy with BACKYARD_CF_CONFIRM_AUTH_CHANGE=true \(in GitHub Actions: Run workflow with confirm_auth_change checked\)/);
    assert.doesNotMatch(e.message, /repository variable/, "a variable would outlive the run");
    assert.ok(!e.message.includes(CI_TOKEN), "the token is never in a message");
    return true;
  });
  assert.deepEqual(f.commands(), ["secret list"], "no R2 bucket, no secrets, no deploy");
  const reads = f.requests();
  assert.ok(reads.some((r) => r.method === "GET" && r.path === settingsPath && r.token === "ci"), "the live settings were read with CLOUDFLARE_API_TOKEN");
  assert.deepEqual(reads.filter((r) => r.method !== "GET"), [], "the guard only reads");
});

test("guard: BACKYARD_CF_CONFIRM_AUTH_CHANGE=true lets the change through, with a warning", async () => {
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  f.env.BACKYARD_CF_CONFIRM_AUTH_CHANGE = "true";
  const log = recorder();
  await deploy({ env: f.env, log });
  assert.deepEqual(f.commands(), ["secret list", "deploy"]);
  assert.match(log.text(), /warning: Worker backyard is live with Cloudflare Access .* deploying anyway \(confirmed\)/);
  // Any other value is no confirmation.
  for (const v of ["1", "yes", "TRUE-ish", ""]) {
    const g = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
    g.env.BACKYARD_CF_CONFIRM_AUTH_CHANGE = v;
    await assert.rejects(deploy({ env: g.env, log: quiet }), /would turn it off/, v);
  }
});

test("guard: a deploy that keeps the live Access (BACKYARD_CF_VARS) passes; another team is refused", async () => {
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  // The short team form normalizes like the Worker does.
  f.env.BACKYARD_CF_VARS = JSON.stringify({ ...ACCESS_VARS, ACCESS_TEAM_DOMAIN: "Acme" });
  const log = recorder();
  await deploy({ env: f.env, log });
  assert.deepEqual(f.commands(), ["secret list", "deploy"]);
  assert.match(log.text(), /Auth guard: the live Worker runs AUTH_MODE=access \(team acme\.cloudflareaccess\.com\); this deploy sets AUTH_MODE=access\./);

  const g = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  g.env.BACKYARD_CF_VARS = JSON.stringify({ ...ACCESS_VARS, ACCESS_TEAM_DOMAIN: "other" });
  await assert.rejects(deploy({ env: g.env, log: quiet }), /team acme\.cloudflareaccess\.com, and this deploy would switch it to other\.cloudflareaccess\.com\. Nothing was changed/);
  assert.deepEqual(g.commands(), ["secret list"]);

  // A new AUD alone is not refused (a wrong AUD fails closed), but it is reported.
  const h = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  h.env.BACKYARD_CF_VARS = JSON.stringify({ ...ACCESS_VARS, ACCESS_AUD: "b".repeat(64) });
  const hlog = recorder();
  await deploy({ env: h.env, log: hlog });
  assert.match(hlog.text(), /warning: the Access AUD changes from a{64} to b{64}/);
});

test("guard: --var in BACKYARD_CF_DEPLOY_ARGS counts", async () => {
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  f.env.BACKYARD_CF_VARS = JSON.stringify(ACCESS_VARS);
  f.env.BACKYARD_CF_DEPLOY_ARGS = "--minify --var AUTH_MODE:none";
  await assert.rejects(deploy({ env: f.env, log: quiet }), /would turn it off \(AUTH_MODE=none\)/);
  assert.deepEqual(f.commands(), ["secret list"]);
});

test("guard: a first deploy is not guarded", async () => {
  const f = fixture();
  const log = recorder();
  await deploy({ env: f.env, log });
  assert.deepEqual(f.commands(), ["secret list", "deploy"]);
  assert.ok(f.requests().some((r) => r.path === settingsPath && r.status === 404), "the API was asked and knows no such Worker");
  assert.doesNotMatch(log.text(), /Auth guard/);
});

test("guard: an unreadable live configuration is refused (fail closed) unless confirmed", async () => {
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  // The token may not read the Worker: neither its settings nor the account's workers.dev subdomain.
  fake.addFault({ method: "GET", path: "/accounts/acc1/workers/*", status: 403, code: 10000, message: "Authentication error", times: Infinity });
  await assert.rejects(deploy({ env: f.env, log: quiet }), (e) => {
    assert.ok(e instanceof CIError);
    assert.match(e.message, /could not read the live configuration of Worker backyard .*it might turn Cloudflare Access off\. Nothing was changed\./s);
    assert.match(e.message, /Account · Workers Scripts · Read/, "the permission is named");
    assert.match(e.message, /CLOUDFLARE_ACCOUNT_ID/);
    assert.match(e.message, /BACKYARD_CF_CONFIRM_AUTH_CHANGE=true/);
    assert.match(e.message, /BACKYARD_CF_VARS must then carry AUTH_MODE, ACCESS_TEAM_DOMAIN and ACCESS_AUD/);
    return true;
  });
  assert.deepEqual(f.commands(), ["secret list"]);

  const g = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  fake.addFault({ method: "GET", path: "/accounts/acc1/workers/*", status: 403, code: 10000, message: "Authentication error", times: Infinity });
  g.env.BACKYARD_CF_CONFIRM_AUTH_CHANGE = "true";
  await deploy({ env: g.env, log: quiet });
  assert.deepEqual(g.commands(), ["secret list", "deploy"]);
});

test("guard: unreadable settings fall back to the Worker's own /info on workers.dev", async () => {
  // Live Access, settings unreadable: the Worker itself says AUTH_MODE=access.
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess() });
  fake.addFault({ method: "GET", path: settingsPath, status: 500, code: 10013, times: Infinity });
  const edge = fake.edgeRequests.length;
  await assert.rejects(deploy({ env: f.env, log: quiet }), /Worker backyard is live with Cloudflare Access, and this deploy would turn it off/);
  assert.ok(
    fake.edgeRequests.slice(edge).some((r) => r.host === "backyard.acme.workers.dev" && r.path === "/__backyard/api/deploy/info"),
    "the Worker's workers.dev URL was asked",
  );
  assert.deepEqual(f.commands(), ["secret list"]);

  // Live AUTH_MODE=none, settings unreadable: the Worker says none, so the deploy may go ahead.
  const g = fixture({ exists: true, secrets: SECRETS });
  fake.addFault({ method: "GET", path: settingsPath, status: 500, code: 10013, times: Infinity });
  const log = recorder();
  await deploy({ env: g.env, log });
  assert.deepEqual(g.commands(), ["secret list", "deploy"]);
  assert.match(log.text(), /Auth guard: the live Worker runs AUTH_MODE=none/);
});

test("guard: the configured hub URL can only make the guard stricter", async () => {
  // Settings unreadable, workers.dev off (no answer there): a hub URL that says
  // "none" is not trusted (it might be another deployment), one that says
  // "access" is.
  let mode = "none";
  const server = createServer((req, res) => {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify({ edition: "cloudflare", version: "1", auth: { mode } }));
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  try {
    const hub = `http://127.0.0.1:${server.address().port}`;
    for (const [m, re] of [
      ["none", /could not read the live configuration/],
      ["access", /is live with Cloudflare Access, and this deploy would turn it off/],
    ]) {
      mode = m;
      const f = fixture({ exists: true, secrets: SECRETS }, { cf: { workers: { backyard: { ...deployedWorker({ vars: { AUTH_MODE: "none" } }), workersDev: false } } } });
      fake.addFault({ method: "GET", path: settingsPath, status: 403, code: 10000, times: Infinity });
      f.env.BACKYARD_CF_VARS = JSON.stringify({ PUBLIC_URL: hub });
      await assert.rejects(deploy({ env: f.env, log: quiet }), re, m);
      assert.deepEqual(f.commands(), ["secret list"], m);
    }
  } finally {
    server.close();
  }
});

test("guard: without CLOUDFLARE_ACCOUNT_ID an existing Worker is refused, a first deploy is not", async () => {
  const f = fixture({ exists: true, secrets: SECRETS });
  delete f.env.CLOUDFLARE_ACCOUNT_ID;
  await assert.rejects(deploy({ env: f.env, log: quiet }), /could not read the live configuration of Worker backyard \(CLOUDFLARE_ACCOUNT_ID is not set\)/);
  assert.deepEqual(f.commands(), ["secret list"]);
  assert.deepEqual(f.requests(), [], "no API request without an account");

  const g = fixture();
  delete g.env.CLOUDFLARE_ACCOUNT_ID;
  await deploy({ env: g.env, log: quiet });
  assert.deepEqual(g.commands(), ["secret list", "deploy"]);
});

test("guard: wrangler sees the Worker but the API does not: refused", async () => {
  // e.g. CLOUDFLARE_ACCOUNT_ID names another account than the one wrangler deploys to.
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: { workers: {} } });
  await assert.rejects(deploy({ env: f.env, log: quiet }), /reports no Worker backyard in account acc1, but wrangler lists its secrets/);
  assert.deepEqual(f.commands(), ["secret list"]);
});

test("guard: a live AUTH_MODE that fails closed is not opened by accident", async () => {
  const f = fixture({ exists: true, secrets: SECRETS }, { cf: liveAccess({ AUTH_MODE: "acess" }) });
  await assert.rejects(deploy({ env: f.env, log: quiet }), /live with AUTH_MODE=acess, which refuses every request \(fail closed\), and this deploy would open it/);
  assert.deepEqual(f.commands(), ["secret list"]);
});

test("guard: CLOUDFLARE_API_BASE_URL must not send the token in clear text", async () => {
  const f = fixture({ exists: true, secrets: SECRETS });
  f.env.CLOUDFLARE_API_BASE_URL = "http://api.example.com/client/v4";
  await assert.rejects(deploy({ env: f.env, log: quiet }), (e) => e instanceof CIError && /must use https/.test(e.message));
  assert.deepEqual(f.commands(), ["secret list"]);
});

test("guard: a dry run reads nothing live", async () => {
  const f = fixture({}, { cf: liveAccess() });
  delete f.env.CLOUDFLARE_API_TOKEN;
  await deploy({ env: f.env, dryRun: true, log: quiet });
  assert.deepEqual(f.requests(), []);
});

test("deploy: routes in wrangler.jsonc are never deployed", async () => {
  const config = `{\n  "name": "backyard",\n  "main": "src/index.ts",\n  "routes": [{ "pattern": "backyard.example.com", "custom_domain": true }],\n  "vars": { "AUTH_MODE": "none" },\n}\n`;
  for (const dryRun of [true, false]) {
    const f = fixture({ exists: true, secrets: SECRETS }, { config });
    const log = recorder();
    await deploy({ env: f.env, dryRun, log });
    const d = f.state().calls.find((c) => c.args[0] === "deploy");
    assert.ok(d.args.includes("--config"), "a config without routes is generated even without overrides");
    const cfg = parseJSONC(f.state().config);
    assert.equal(cfg.routes, undefined);
    assert.equal(cfg.route, undefined);
    assert.deepEqual(cfg.vars, { AUTH_MODE: "none" });
    assert.match(log.text(), /warning: routes in wrangler\.jsonc are ignored: routes and custom domains are managed by `backyard cloudflare domain add`/);
    assert.ok(!existsSync(join(f.cf, GENERATED_CONFIG)), "the generated config is removed");
  }
});
