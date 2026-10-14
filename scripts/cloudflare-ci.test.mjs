// Tests for scripts/cloudflare-ci.mjs: node --test scripts/*.test.mjs
//
// The deploy tests run the real script against a fake wrangler (a small node
// program that records its arguments and plays the Cloudflare API), so no
// Cloudflare account or network access is needed.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, readFileSync, existsSync, mkdirSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer } from "node:http";

import {
  CIError,
  GENERATED_CONFIG,
  deploy,
  effectiveConfig,
  hubURL,
  isMissingWorker,
  parseDeployArgs,
  parseJSONC,
  parseSecretList,
  parseVars,
  planSecrets,
} from "./cloudflare-ci.mjs";

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
  assert.throws(() => parseDeployArgs("--secrets-file x.json"), /secrets-file/);
});

test("effectiveConfig", () => {
  const base = { name: "backyard", vars: { URL_MODE: "path", A: "1" }, r2_buckets: [{ binding: "OTHER", bucket_name: "o" }] };
  assert.equal(effectiveConfig(base, {}), null);
  const got = effectiveConfig(base, { vars: { URL_MODE: "host" }, bucket: "files" });
  assert.deepEqual(got.vars, { URL_MODE: "host", A: "1" });
  assert.deepEqual(got.r2_buckets, [{ binding: "OTHER", bucket_name: "o" }, { binding: "R2", bucket_name: "files" }]);
  assert.deepEqual(base.vars, { URL_MODE: "path", A: "1" }, "input is not modified");
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

function fixture(state = {}) {
  const dir = mkdtempSync(join(tmpdir(), "backyard-cf-ci-test-"));
  const cf = join(dir, "cf");
  mkdirSync(cf);
  writeFileSync(
    join(cf, "wrangler.jsonc"),
    `// test config\n{\n  "name": "backyard",\n  "main": "src/index.ts",\n  "vars": { "URL_MODE": "path", "AUTH_MODE": "none", },\n}\n`,
  );
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
    CLOUDFLARE_API_TOKEN: "cf-token",
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
  };
}

const quiet = () => {};

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
  const f = fixture({ exists: true, secrets: ["BACKYARD_SECRET", "DEPLOY_TOKEN"] });
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
