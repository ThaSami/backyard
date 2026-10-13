// Offline tests of scripts/setup.mjs (node --test): the script runs against a
// fake wrangler that records every call, so the account choice, the automatic
// R2 bucket, secrets, remembered state and the generated config are checked
// without a Cloudflare account.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { after, before, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const pkgDir = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");

// The fake wrangler: answers from FAKE_* variables, logs each call as a JSON line.
const FAKE_WRANGLER = String.raw`
import { appendFileSync, existsSync, readFileSync, statSync, writeFileSync } from "node:fs";
const args = process.argv.slice(2);
const log = process.env.FAKE_LOG;
const secretsFile = process.env.FAKE_SECRETS;
const readStdin = () => { try { return readFileSync(0, "utf8"); } catch { return ""; } };
const entry = { args, account: process.env.CLOUDFLARE_ACCOUNT_ID ?? null };
const secrets = existsSync(secretsFile) ? JSON.parse(readFileSync(secretsFile, "utf8")) : {};
let code = 0;
switch (args[0]) {
  case "whoami":
    if (process.env.FAKE_LOGGED_OUT) { console.log('{"loggedIn":false}'); code = 1; break; }
    console.log("noise before json");
    console.log(JSON.stringify({ loggedIn: true, email: "me@example.com", accounts: JSON.parse(process.env.FAKE_ACCOUNTS || "[]") }));
    break;
  case "r2":
    if (process.env.FAKE_R2 === "disabled") { console.error("✘ [ERROR] A request to the Cloudflare API failed. Please enable R2 through the Cloudflare Dashboard. [code: 10042]"); code = 1; }
    else console.log("Created bucket " + args[3]);
    break;
  case "deploy": {
    const i = args.indexOf("--config");
    if (i >= 0) entry.config = readFileSync(args[i + 1], "utf8");
    const j = args.indexOf("--secrets-file");
    if (j >= 0) {
      const file = JSON.parse(readFileSync(args[j + 1], "utf8"));
      entry.secretsFile = Object.keys(file).sort();
      entry.secretsMode = (statSync(args[j + 1]).mode & 0o777).toString(8);
      Object.assign(secrets, file);
      writeFileSync(secretsFile, JSON.stringify(secrets));
    }
    writeFileSync(secretsFile + ".deployed", "1");
    console.log("Uploaded backyard\nDeployed backyard triggers\n  https://" + args[args.indexOf("--name") + 1] + ".me.workers.dev");
    break;
  }
  case "secret":
    if (args[1] === "list" && process.env.FAKE_LIST_ERROR) { console.error("✘ [ERROR] Authentication error [code: 10000]"); code = 1; break; }
    if (args[1] === "list" && !existsSync(secretsFile + ".deployed")) { console.error('✘ [ERROR] Worker "' + args[args.indexOf("--name") + 1] + '" not found. [code: 10007]'); code = 1; break; }
    if (args[1] === "list") console.log("▲ [WARNING] noise [x]\n" + JSON.stringify(Object.keys(secrets).map((name) => ({ name, type: "secret_text" }))));
    if (args[1] === "put") { secrets[args[2]] = readStdin().replace(/\n$/, ""); writeFileSync(secretsFile, JSON.stringify(secrets)); entry.secret = args[2]; }
    break;
  default:
    code = 2;
}
appendFileSync(log, JSON.stringify(entry) + "\n");
process.exit(code);
`;

function sandbox() {
  const dir = mkdtempSync(join(tmpdir(), "backyard-setup-"));
  mkdirSync(join(dir, "scripts"));
  cpSync(join(pkgDir, "scripts", "setup.mjs"), join(dir, "scripts", "setup.mjs"));
  writeFileSync(join(dir, "scripts", "sync-assets.mjs"), "");
  cpSync(join(pkgDir, "wrangler.jsonc"), join(dir, "wrangler.jsonc"));
  mkdirSync(join(dir, "node_modules", "wrangler", "bin"), { recursive: true });
  writeFileSync(join(dir, "node_modules", "wrangler", "bin", "wrangler.js"), FAKE_WRANGLER);
  writeFileSync(join(dir, "node_modules", "wrangler", "package.json"), '{"type":"module"}');
  return dir;
}

function run(dir, args, env = {}) {
  return new Promise((resolveRun) => {
    const child = spawn(process.execPath, [join(dir, "scripts", "setup.mjs"), ...args], {
      cwd: dir,
      env: {
        PATH: process.env.PATH,
        HOME: dir,
        XDG_CONFIG_HOME: join(dir, "config"),
        FAKE_LOG: join(dir, "calls.log"),
        FAKE_SECRETS: join(dir, "secrets.json"),
        NO_COLOR: "1",
        ...env,
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let out = "";
    child.stdout.on("data", (c) => (out += c));
    child.stderr.on("data", (c) => (out += c));
    child.on("close", (code) => resolveRun({ code, out }));
  });
}

function calls(dir) {
  const p = join(dir, "calls.log");
  if (!existsSync(p)) return [];
  const all = readFileSync(p, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  rmSync(p);
  return all;
}

const state = (dir, worker = "backyard") => JSON.parse(readFileSync(join(dir, "config", "backyard", "cloudflare", `${worker}.json`), "utf8"));

describe("scripts/setup.mjs", () => {
  let server;
  let hub;
  const dirs = [];
  before(async () => {
    server = createServer((req, res) => {
      res.writeHead(req.url === "/__backyard/readyz" ? 200 : 404);
      res.end("ok\n");
    });
    await new Promise((r) => server.listen(0, "127.0.0.1", r));
    hub = `http://127.0.0.1:${server.address().port}`;
  });
  after(() => {
    server.close();
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
  });
  const newSandbox = () => {
    const d = sandbox();
    dirs.push(d);
    return d;
  };

  it("deploys with an automatic R2 bucket, sets secrets once and remembers its choices", async () => {
    const dir = newSandbox();
    const accounts = JSON.stringify([{ id: "acc1", name: "Only" }]);
    let r = await run(dir, ["--var", `PUBLIC_URL:${hub}`, "--var", 'LIMITS:{"ai_per_day":5}'], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    let c = calls(dir);
    assert.deepEqual(c.map((x) => x.args.slice(0, 2).join(" ")), ["whoami --json", "r2 bucket", "secret list", "deploy --name"]);
    // Every call after the account choice runs for that account.
    assert.ok(c.slice(1).every((x) => x.account === "acc1"));
    const deployCall = c.find((x) => x.args[0] === "deploy");
    // The secrets travel with the first version, through a private file.
    assert.deepEqual(deployCall.secretsFile, ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    assert.equal(deployCall.secretsMode, "600");
    assert.ok(deployCall.args.includes("--env="));
    const generated = JSON.parse(deployCall.config.slice(deployCall.config.indexOf("\n") + 1));
    assert.deepEqual(generated.r2_buckets, [{ binding: "R2", bucket_name: "backyard-files" }]);
    assert.equal(generated.vars.LIMITS, '{"ai_per_day":5}');
    assert.equal(generated.vars.URL_MODE, "path");
    assert.ok(!existsSync(join(dir, "wrangler.setup.jsonc")), "the generated config is removed");
    const secrets = JSON.parse(readFileSync(join(dir, "secrets.json"), "utf8"));
    assert.match(secrets.BACKYARD_SECRET, /^[0-9a-f]{64}$/);
    assert.match(secrets.DEPLOY_TOKEN, /^byd_/);
    const st = state(dir);
    assert.equal(st.url, hub);
    assert.equal(st.deployToken, secrets.DEPLOY_TOKEN);
    assert.equal(st.accountId, "acc1");
    assert.equal(st.r2Bucket, "backyard-files");
    assert.match(r.out, new RegExp(`backyard login --server ${hub.replace(/[.]/g, "\\.")} --token byd_`));

    // A re-run keeps secrets, bucket and overrides.
    r = await run(dir, [], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    c = calls(dir);
    assert.equal(c.filter((x) => x.secret || x.secretsFile).length, 0, "no secret is rewritten");
    assert.equal(JSON.parse(c.find((x) => x.args[0] === "deploy").config.split("\n").slice(1).join("\n")).vars.PUBLIC_URL, hub);
    assert.equal(state(dir).deployToken, secrets.DEPLOY_TOKEN);

    // --no-r2 is remembered; --rotate-token replaces the token.
    r = await run(dir, ["--no-r2", "--rotate-token"], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    c = calls(dir);
    assert.ok(!c.some((x) => x.args[0] === "r2"));
    assert.deepEqual(c.find((x) => x.args[0] === "deploy").secretsFile, ["DEPLOY_TOKEN"]);
    assert.equal(state(dir).r2, false);
    assert.equal(state(dir).r2Bucket, undefined);
    r = await run(dir, [], { FAKE_ACCOUNTS: accounts });
    assert.ok(!calls(dir).some((x) => x.args[0] === "r2"), "--no-r2 sticks");
  });

  it("never replaces secrets when they cannot be listed, and sets them directly with --skip-deploy", async () => {
    const dir = newSandbox();
    const accounts = '[{"id":"a","name":"A"}]';
    let r = await run(dir, ["--var", `PUBLIC_URL:${hub}`], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    const before = readFileSync(join(dir, "secrets.json"), "utf8");
    calls(dir);
    r = await run(dir, [], { FAKE_ACCOUNTS: accounts, FAKE_LIST_ERROR: "1" });
    assert.notEqual(r.code, 0);
    assert.match(r.out, /could not list the secrets/);
    assert.ok(!calls(dir).some((x) => x.args[0] === "deploy"));
    assert.equal(readFileSync(join(dir, "secrets.json"), "utf8"), before);
    r = await run(dir, ["--skip-deploy", "--rotate-token"], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    const c = calls(dir);
    assert.ok(!c.some((x) => x.args[0] === "deploy"));
    assert.deepEqual(c.filter((x) => x.secret).map((x) => x.secret), ["DEPLOY_TOKEN"]);
    assert.equal(state(dir).deployToken, JSON.parse(readFileSync(join(dir, "secrets.json"), "utf8")).DEPLOY_TOKEN);
  });

  it("continues without R2 when the account has not enabled it", async () => {
    const dir = newSandbox();
    const r = await run(dir, ["--var", `PUBLIC_URL:${hub}`], { FAKE_ACCOUNTS: '[{"id":"a","name":"A"}]', FAKE_R2: "disabled" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /R2 is not enabled on this account/);
    const deployCall = calls(dir).find((x) => x.args[0] === "deploy");
    assert.ok(!JSON.parse(deployCall.config.split("\n").slice(1).join("\n")).r2_buckets);
    assert.equal(state(dir).r2Bucket, undefined);
    assert.equal(state(dir).r2, undefined, "a failed attempt is retried next time");
  });

  it("asks for an account only when it must", async () => {
    const two = JSON.stringify([
      { id: "a1", name: "Personal" },
      { id: "a2", name: "Work" },
    ]);
    const dir = newSandbox();
    // Non-interactive with several accounts: a clear error, nothing deployed.
    let r = await run(dir, [], { FAKE_ACCOUNTS: two });
    assert.notEqual(r.code, 0);
    assert.match(r.out, /CLOUDFLARE_ACCOUNT_ID/);
    assert.ok(!calls(dir).some((x) => x.args[0] === "deploy"));
    // CLOUDFLARE_ACCOUNT_ID decides (and is remembered).
    r = await run(dir, ["--var", `PUBLIC_URL:${hub}`], { FAKE_ACCOUNTS: two, CLOUDFLARE_ACCOUNT_ID: "a2" });
    assert.equal(r.code, 0, r.out);
    assert.ok(calls(dir).filter((x) => x.args[0] !== "whoami").every((x) => x.account === "a2"));
    r = await run(dir, [], { FAKE_ACCOUNTS: two });
    assert.equal(r.code, 0, r.out);
    assert.ok(calls(dir).slice(1).every((x) => x.account === "a2"));
  });

  it("uses API tokens non-interactively and fails clearly when logged out", async () => {
    const dir = newSandbox();
    let r = await run(dir, ["--worker-name", "team-yard", "--var", `PUBLIC_URL:${hub}`], { CLOUDFLARE_API_TOKEN: "t", CLOUDFLARE_ACCOUNT_ID: "acc", DEPLOY_TOKEN: "my-token" });
    assert.equal(r.code, 0, r.out);
    const c = calls(dir);
    assert.ok(!c.some((x) => x.args[0] === "whoami"), "no whoami with token and account");
    assert.ok(c.some((x) => x.args.join(" ").startsWith("deploy --name team-yard")));
    assert.equal(JSON.parse(readFileSync(join(dir, "secrets.json"), "utf8")).DEPLOY_TOKEN, "my-token");
    assert.ok(c.find((x) => x.args[0] === "deploy").secretsFile.includes("DEPLOY_TOKEN"));
    assert.equal(state(dir, "team-yard").deployToken, "my-token");

    r = await run(newSandbox(), [], { FAKE_LOGGED_OUT: "1" });
    assert.notEqual(r.code, 0);
    assert.match(r.out, /not logged in to Cloudflare/);
    r = await run(dir, ["--bogus"]);
    assert.notEqual(r.code, 0);
    assert.match(r.out, /unknown option --bogus/);
  });
});
