// Offline tests of scripts/setup.mjs (node --test): the script runs against a
// fake wrangler that records every call, so the account choice, the automatic
// R2 bucket, secrets, remembered state and the generated config are checked
// without a Cloudflare account.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmodSync, cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { after, before, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { isCI, isMissingWorker, maskSecret, parseSecretList, revealSecrets } from "../../scripts/setup.mjs";

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
  case "secret": {
    // FAKE_LIST_MODE plays the ways "secret list" can go wrong.
    const mode = process.env.FAKE_LIST_MODE || (process.env.FAKE_LIST_ERROR ? "auth" : "");
    if (args[1] === "list" && mode === "auth") { console.error("✘ [ERROR] Authentication error [code: 10000]"); code = 1; break; }
    if (args[1] === "list" && mode === "network") { console.error("✘ [ERROR] fetch failed: getaddrinfo ENOTFOUND api.cloudflare.com (route not found)"); code = 1; break; }
    if (args[1] === "list" && !existsSync(secretsFile + ".deployed")) { console.error('✘ [ERROR] Worker "' + args[args.indexOf("--name") + 1] + '" not found. [code: 10007]'); code = 1; break; }
    // Exit 0 without the list on stdout (an empty "[]" only on stderr).
    if (args[1] === "list" && mode === "stderr-json") { console.log("Listing secrets…"); console.error("[]"); break; }
    if (args[1] === "list" && mode === "garbage") { console.log("Something unexpected happened"); break; }
    // A warning on stderr that starts with "[" (what a proxy warning can look like without colors).
    if (args[1] === "list" && mode === "stderr-warning") console.error("[WARNING] Proxy environment variables detected. We'll use your proxy for fetch requests.");
    if (args[1] === "list") console.log("▲ [WARNING] noise [x]\n" + JSON.stringify(Object.keys(secrets).map((name) => ({ name, type: "secret_text" }))));
    if (args[1] === "put") { secrets[args[2]] = readStdin().replace(/\n$/, ""); writeFileSync(secretsFile, JSON.stringify(secrets)); entry.secret = args[2]; }
    break;
  }
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

const statePath = (dir, worker = "backyard") => join(dir, "config", "backyard", "cloudflare", `${worker}.json`);
const state = (dir, worker = "backyard") => JSON.parse(readFileSync(statePath(dir, worker), "utf8"));
const fileMode = (p) => (statSync(p).mode & 0o777).toString(8);

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
    if (process.platform !== "win32") assert.equal(fileMode(statePath(dir)), "600");
    // Not a terminal: the generated secrets are never printed, the token is
    // masked and lives in the state file.
    assert.ok(!r.out.includes(secrets.DEPLOY_TOKEN), "the deploy token is not printed");
    assert.ok(!r.out.includes(secrets.BACKYARD_SECRET), "BACKYARD_SECRET is not printed");
    assert.ok(r.out.includes(`backyard login --server ${hub} --token <"deployToken" in ${statePath(dir)}>`), r.out);
    assert.ok(r.out.includes(maskSecret(secrets.DEPLOY_TOKEN)), r.out);

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
    // Authentication or network errors, exit 0 without the list on stdout (an
    // empty list only on stderr), or garbage: stop, change nothing.
    for (const [mode, msg] of [
      ["auth", /could not list the secrets/],
      ["network", /could not list the secrets/],
      ["stderr-json", /unexpected output of wrangler secret list/],
      ["garbage", /unexpected output of wrangler secret list/],
    ]) {
      r = await run(dir, [], { FAKE_ACCOUNTS: accounts, FAKE_LIST_MODE: mode });
      assert.notEqual(r.code, 0, mode);
      assert.match(r.out, msg, mode);
      assert.ok(!calls(dir).some((x) => x.args[0] === "deploy" || x.secret), mode);
      assert.equal(readFileSync(join(dir, "secrets.json"), "utf8"), before, mode);
    }
    // A "[…]" warning on stderr is not the list: the secrets are kept.
    r = await run(dir, [], { FAKE_ACCOUNTS: accounts, FAKE_LIST_MODE: "stderr-warning", CI: "true" });
    assert.equal(r.code, 0, r.out);
    let c = calls(dir);
    assert.ok(c.some((x) => x.args[0] === "deploy"));
    assert.equal(c.filter((x) => x.secret || x.secretsFile).length, 0, "no secret is rewritten");
    assert.equal(readFileSync(join(dir, "secrets.json"), "utf8"), before);
    assert.match(r.out, /not printed: CI/);
    assert.ok(!r.out.includes(JSON.parse(before).DEPLOY_TOKEN));
    r = await run(dir, ["--skip-deploy", "--rotate-token"], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    c = calls(dir);
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
    // A token from the environment is referred to, never echoed.
    assert.ok(!r.out.includes("my-token"), r.out);
    assert.match(r.out, /--token \$DEPLOY_TOKEN/);

    r = await run(newSandbox(), [], { FAKE_LOGGED_OUT: "1" });
    assert.notEqual(r.code, 0);
    assert.match(r.out, /not logged in to Cloudflare/);
    r = await run(dir, ["--bogus"]);
    assert.notEqual(r.code, 0);
    assert.match(r.out, /unknown option --bogus/);
  });
  it("keeps a broken state file and an unknown Worker from rotating anything", async () => {
    const dir = newSandbox();
    const accounts = '[{"id":"a","name":"A"}]';
    // --skip-deploy cannot set secrets on a Worker that does not exist yet.
    let r = await run(dir, ["--skip-deploy"], { FAKE_ACCOUNTS: accounts });
    assert.notEqual(r.code, 0);
    assert.match(r.out, /does not exist yet/);
    assert.ok(!calls(dir).some((x) => x.secret || x.args[0] === "deploy"));

    r = await run(dir, ["--var", `PUBLIC_URL:${hub}`], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    calls(dir);
    const token = state(dir).deployToken;
    // An older version may have left the state readable by others: fixed on save.
    chmodSync(statePath(dir), 0o644);
    r = await run(dir, [], { FAKE_ACCOUNTS: accounts });
    assert.equal(r.code, 0, r.out);
    calls(dir);
    if (process.platform !== "win32") assert.equal(fileMode(statePath(dir)), "600");
    assert.equal(state(dir).deployToken, token);

    // A corrupt state file stops the setup instead of issuing a new token.
    writeFileSync(statePath(dir), "{ not json");
    r = await run(dir, [], { FAKE_ACCOUNTS: accounts });
    assert.notEqual(r.code, 0);
    assert.match(r.out, /is not a valid state file/);
    writeFileSync(statePath(dir), "[]");
    r = await run(dir, [], { FAKE_ACCOUNTS: accounts });
    assert.notEqual(r.code, 0);
    assert.match(r.out, /is not a valid state file/);
    assert.ok(!calls(dir).some((x) => x.args[0] === "deploy" || x.secret));
  });
});

describe("scripts/setup.mjs helpers", () => {
  it("parses the secret list from stdout only, never guessing", () => {
    const list = JSON.stringify([{ name: "BACKYARD_SECRET", type: "secret_text" }, { name: "DEPLOY_TOKEN", type: "secret_text" }]);
    const names = (s) => [...parseSecretList(s)].sort();
    assert.deepEqual(names(list), ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    assert.deepEqual(names("[]"), []);
    assert.deepEqual(names(`▲ [WARNING] Proxy environment variables detected\n${list}\n`), ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    assert.deepEqual(names(`[WARNING] a notice [x]\n${list}\n`), ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    assert.deepEqual(names(`${list}\n▲ [WARNING] after the list [x]\n`), ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    assert.deepEqual(names('[\n  {\n    "name": "A",\n    "type": "secret_text"\n  }\n]\n'), ["A"]);
    for (const bad of ["", "garbage", "Listing secrets…", "x []", "[1, 2]", '[{"type":"secret_text"}]', "[] trailing", "{}"]) {
      assert.throws(() => parseSecretList(bad), /no JSON list/, JSON.stringify(bad));
    }
  });

  it("recognizes only wrangler's explicit missing-Worker errors", () => {
    assert.ok(isMissingWorker('✘ [ERROR] Worker "backyard" not found. [code: 10007]'));
    assert.ok(isMissingWorker('Worker "backyard" (env: production) not found'));
    assert.ok(isMissingWorker("This Worker does not exist on your account. [code: 10007]"));
    for (const other of [
      "✘ [ERROR] Authentication error [code: 10000]",
      "fetch failed: getaddrinfo ENOTFOUND api.cloudflare.com",
      "404 Not Found",
      "",
    ]) {
      assert.ok(!isMissingWorker(other), other);
    }
  });

  it("reveals secrets only on an interactive terminal outside CI", () => {
    assert.equal(revealSecrets({}, true), true);
    assert.equal(revealSecrets({}, false), false);
    assert.equal(revealSecrets({ CI: "true" }, true), false);
    assert.equal(revealSecrets({ CI: "1" }, true), false);
    assert.equal(revealSecrets({ CI: "false" }, true), true);
    assert.equal(isCI({ CI: "0" }), false);
    assert.equal(isCI({ CI: "" }), false);
    assert.equal(isCI({ CI: "woodpecker" }), true);
    const tok = "byd_abcdefghijklmnopqrstuvwxyz012345";
    assert.equal(maskSecret(tok), "byd_…2345");
    assert.ok(!maskSecret(tok).includes("abcdefgh"));
    assert.equal(maskSecret("short"), "…");
    assert.equal(maskSecret("0123456789abcdef0123456789abcdef"), "…cdef");
  });
});
