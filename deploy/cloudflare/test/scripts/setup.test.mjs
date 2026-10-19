// Offline tests of `backyard-cloudflare setup` (node --test): the engine runs
// in a sandbox against the fake wrangler and the fake Cloudflare API/edge
// (test/scripts/helpers), so the account choice, the workers.dev subdomain,
// adoption, the auth guard, the automatic R2 bucket, secrets, remembered state
// and the generated config are checked without a Cloudflare account.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import { hostname } from "node:os";
import { join } from "node:path";
import { after, afterEach, describe, it } from "node:test";

import { isCI, isMissingWorker, maskSecret, parseJSONC, parseSecretList, revealSecrets, suggestSubdomain } from "../../scripts/setup.mjs";
import { OAUTH_TOKEN, TOKEN_PRESETS, deployedWorker } from "./helpers/fake-cloudflare.mjs";
import { assertNoLeaks, createSandbox, makeReadOnly, makeWritable, ttyAvailable } from "./helpers/sandbox.mjs";

const sandboxes = [];
let fresh = [];
/** A sandbox whose every run's output is kept for the secret checks after each test. */
async function newSandbox(o) {
  const sb = await createSandbox(o);
  const run = sb.run.bind(sb);
  sb.outputs = [];
  sb.allowInEnv = [];
  sb.extraSecrets = [];
  sb.run = async (...a) => {
    const r = await run(...a);
    sb.outputs.push(r.out);
    return r;
  };
  sandboxes.push(sb);
  fresh.push(sb);
  return sb;
}
// Every test: no secret in a wrangler argv or env, the state, the result file or the output.
afterEach(() => {
  const check = fresh;
  fresh = [];
  for (const sb of check) assertNoLeaks(sb, { outputs: sb.outputs, allowInEnv: sb.allowInEnv, extra: sb.extraSecrets });
});
after(async () => {
  for (const sb of sandboxes) await sb.close();
});

const deployOf = (calls) => calls.find((c) => c.args[0] === "deploy");
const names = (calls) => calls.map((c) => c.args.slice(0, 2).join(" "));
const secretsOf = (sb, w = "backyard") => sb.cf().workers?.[w]?.secrets ?? {};
const AUD = "a".repeat(64);
const fileMode = (p) => (statSync(p).mode & 0o777).toString(8);
/** "Another machine" or a fresh $XDG_CONFIG_HOME: no state at all. */
const forgetState = (sb) => rmSync(join(sb.root, "config"), { recursive: true, force: true });

/** A Worker another machine deployed (vars merged over the defaults of wrangler.jsonc). */
function seedWorker(sb, o = {}) {
  const base = parseJSONC(readFileSync(join(sb.pkg, "wrangler.jsonc"), "utf8")).vars;
  sb.updateCf((s) => {
    s.workers[o.name ?? "backyard"] = deployedWorker({
      vars: { ...base, ...(o.vars ?? {}) },
      secrets: o.secrets ?? { BACKYARD_SECRET: "s".repeat(64), DEPLOY_TOKEN: "byd_existing-token-0123456789abcdef" },
      r2Bucket: o.r2Bucket,
      ai: o.ai,
      workersDev: o.workersDev,
    });
    for (const host of o.customDomains ?? []) {
      s.zones = [...(s.zones ?? []), { id: "zone-example", name: "example.com", status: "active", account: "acc1" }];
      s.customDomains.push({ id: `cd-${host}`, hostname: host, zone_id: "zone-example", zone_name: "example.com", service: o.name ?? "backyard", environment: "production" });
    }
  });
}

describe("backyard-cloudflare setup", () => {
  it("deploys with an automatic R2 bucket, sets secrets once and remembers its choices", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup", "--var", 'LIMITS:{"ai_per_day":5}']);
    assert.equal(r.code, 0, r.out);
    let c = sb.calls();
    assert.deepEqual(names(c), ["whoami --json", "auth token", "r2 bucket", "secret list", "deploy --name"]);
    // Every call after the account choice runs for that account.
    assert.ok(c.slice(1).every((x) => x.account === "acc1"));
    assert.equal(sb.syncRuns(), 1, "the checkout's assets are synced once");
    const d = deployOf(c);
    // The secrets travel with the first version, through a private file.
    assert.deepEqual(d.secretsFile, ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    assert.equal(d.secretsMode, "600");
    assert.ok(d.args.includes("--env="));
    assert.deepEqual(d.config.r2_buckets, [{ binding: "R2", bucket_name: "backyard-files" }]);
    assert.equal(d.config.vars.LIMITS, '{"ai_per_day":5}');
    assert.equal(d.config.vars.URL_MODE, "path");
    assert.equal(d.config.workers_dev, true);
    assert.equal(d.config.routes, undefined, "a deploy never carries routes");
    assert.equal(d.config.main, "src/index.ts");
    assert.ok(!existsSync(join(sb.pkg, "wrangler.setup.jsonc")), "the generated config is removed");
    const secrets = secretsOf(sb);
    assert.match(secrets.BACKYARD_SECRET, /^[0-9a-f]{64}$/);
    assert.match(secrets.DEPLOY_TOKEN, /^byd_/);
    // The REST calls: the live settings (none yet), then the account's workers.dev subdomain.
    assert.deepEqual(
      sb.requests().map((x) => `${x.method} ${x.path} ${x.status} ${x.token}`),
      ["GET /accounts/acc1/workers/scripts/backyard/settings 404 oauth", "GET /accounts/acc1/workers/subdomain 200 oauth"],
    );
    const st = sb.state();
    assert.equal(st.version, 2);
    assert.equal(st.url, "https://backyard.acme.workers.dev");
    assert.equal(st.workersDevUrl, "https://backyard.acme.workers.dev");
    assert.equal(st.workersDev, true);
    assert.equal(st.deployToken, secrets.DEPLOY_TOKEN);
    assert.equal(st.accountId, "acc1");
    assert.equal(st.r2Bucket, "backyard-files");
    assert.deepEqual(st.vars, { LIMITS: '{"ai_per_day":5}' });
    if (process.platform !== "win32") assert.equal(sb.stateMode(), "600");
    // Not a terminal: the generated secrets are never printed, the token is
    // masked and lives in the state file.
    assert.ok(r.out.includes(`backyard login --server https://backyard.acme.workers.dev --token <"deployToken" in ${sb.statePath()}>`), r.out);
    assert.ok(r.out.includes(maskSecret(secrets.DEPLOY_TOKEN)), r.out);
    assert.match(r.out, /Waiting for https:\/\/backyard\.acme\.workers\.dev to answer… ready/);
    assert.match(r.out, /Your own domain: npm run cf -- domain add <host>/);
    assert.doesNotMatch(r.out, /Terraform/);

    // A re-run keeps secrets, bucket and overrides.
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    c = sb.calls();
    assert.equal(c.filter((x) => x.secret || x.secretsFile).length, 0, "no secret is rewritten");
    assert.equal(deployOf(c).config.vars.LIMITS, '{"ai_per_day":5}');
    assert.equal(sb.state().deployToken, secrets.DEPLOY_TOKEN);
    assert.deepEqual(secretsOf(sb), secrets);

    // --no-r2 is remembered; --rotate-token replaces the token.
    r = await sb.run(["setup", "--no-r2", "--rotate-token"]);
    assert.equal(r.code, 0, r.out);
    c = sb.calls();
    assert.ok(!c.some((x) => x.args[0] === "r2"));
    assert.deepEqual(deployOf(c).secretsFile, ["DEPLOY_TOKEN"]);
    assert.equal(deployOf(c).config.r2_buckets, undefined);
    assert.equal(sb.state().r2, false);
    assert.equal(sb.state().r2Bucket, undefined);
    assert.notEqual(secretsOf(sb).DEPLOY_TOKEN, secrets.DEPLOY_TOKEN);
    assert.equal(sb.state().deployToken, secretsOf(sb).DEPLOY_TOKEN);
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.ok(!sb.calls().some((x) => x.args[0] === "r2"), "--no-r2 sticks");
    assertNoLeaks(sb, { outputs: [r.out] });
  });

  it("never replaces secrets when they cannot be listed, and sets them directly with --skip-deploy", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    const before = secretsOf(sb);
    sb.calls();
    // Authentication or network errors, exit 0 without the list on stdout (an
    // empty list only on stderr), or garbage: stop, change nothing.
    for (const [mode, msg] of [
      ["auth", /could not list the secrets/],
      ["network", /could not list the secrets/],
      ["stderr-json", /unexpected output of wrangler secret list/],
      ["garbage", /unexpected output of wrangler secret list/],
    ]) {
      r = await sb.run(["setup"], { FAKE_LIST_MODE: mode });
      assert.equal(r.code, 1, `${mode}: ${r.out}`);
      assert.match(r.out, msg, mode);
      assert.ok(!sb.calls().some((x) => x.args[0] === "deploy" || x.secret), mode);
      assert.deepEqual(secretsOf(sb), before, mode);
    }
    // A "[…]" warning on stderr is not the list: the secrets are kept.
    r = await sb.run(["setup"], { FAKE_LIST_MODE: "stderr-warning", CI: "true" });
    assert.equal(r.code, 0, r.out);
    let c = sb.calls();
    assert.ok(c.some((x) => x.args[0] === "deploy"));
    assert.equal(c.filter((x) => x.secret || x.secretsFile).length, 0, "no secret is rewritten");
    assert.deepEqual(secretsOf(sb), before);
    assert.match(r.out, /not printed: CI/);
    assert.ok(!r.out.includes(before.DEPLOY_TOKEN));
    r = await sb.run(["setup", "--skip-deploy", "--rotate-token"]);
    assert.equal(r.code, 0, r.out);
    c = sb.calls();
    assert.ok(!c.some((x) => x.args[0] === "deploy"));
    assert.deepEqual(c.filter((x) => x.secret).map((x) => x.secret), ["DEPLOY_TOKEN"]);
    assert.equal(sb.state().deployToken, secretsOf(sb).DEPLOY_TOKEN);
    assertNoLeaks(sb, { outputs: [r.out] });
  });

  it("continues without R2 when the account has not enabled it", async () => {
    const sb = await newSandbox();
    const r = await sb.run(["setup"], { FAKE_R2: "disabled" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /R2 is not enabled on this account/);
    assert.match(r.out, /re-run npm run cf -- setup/);
    assert.equal(deployOf(sb.calls()).config.r2_buckets, undefined);
    assert.equal(sb.state().r2Bucket, undefined);
    assert.equal(sb.state().r2, undefined, "a failed attempt is retried next time");
  });

  it("never unbinds or forgets the R2 bucket in use when creating it fails", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup", "--r2-bucket", "team-files"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(sb.cf().workers.backyard.r2Bucket, "team-files");
    // The create fails (a transient error, a token without R2 permission): the bound bucket stays bound.
    r = await sb.run(["setup"], { FAKE_R2: "disabled" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /could not confirm the R2 bucket team-files: keeping it bound \(the Worker already uses it\)/);
    assert.doesNotMatch(r.out, /Continuing without R2/);
    assert.deepEqual(deployOf(sb.calls()).config.r2_buckets, [{ binding: "R2", bucket_name: "team-files" }]);
    assert.equal(sb.state().r2Bucket, "team-files");
    assert.equal(sb.cf().workers.backyard.r2Bucket, "team-files");
    // A machine without state adopts the live bucket and keeps it bound too.
    forgetState(sb);
    r = await sb.run(["setup"], { FAKE_R2: "disabled" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /R2 bucket team-files/);
    assert.deepEqual(deployOf(sb.calls()).config.r2_buckets, [{ binding: "R2", bucket_name: "team-files" }]);
    assert.equal(sb.state().r2Bucket, "team-files");
  });

  it("remembers a bucket name it could not create yet, and uses it on the next run", async () => {
    const sb = await newSandbox();
    sb.writeState("backyard", { version: 2, accountId: "acc1", r2Bucket: "team-files", updatedAt: "2026-01-01T00:00:00Z" });
    let r = await sb.run(["setup"], { FAKE_R2: "disabled" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /R2 is not enabled on this account/);
    assert.match(r.out, /Continuing without R2/);
    assert.equal(deployOf(sb.calls()).config.r2_buckets, undefined);
    assert.equal(sb.state().r2Bucket, "team-files", "the name is kept");
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(deployOf(sb.calls()).config.r2_buckets, [{ binding: "R2", bucket_name: "team-files" }]);
  });

  it("turns AI off with --no-ai (no Workers AI binding, AI_ENABLED managed), remembers it, and --ai turns it back on", async () => {
    const sb = await newSandbox();
    // An account where Workers AI is not allowed: a deploy with the binding fails.
    const noWorkersAI = { FAKE_AI: "disabled" };
    let r = await sb.run(["setup", "--no-r2"], noWorkersAI);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /Workers AI is not available on this account/);
    assert.deepEqual(deployOf(sb.calls()).config.ai, { binding: "AI", remote: true }, "on by default");
    r = await sb.run(["setup", "--no-r2", "--no-ai"], noWorkersAI);
    assert.equal(r.code, 0, r.out);
    let d = deployOf(sb.calls());
    assert.equal(d.config.ai, undefined, "no Workers AI binding");
    assert.equal(d.config.vars.AI_ENABLED, "false");
    assert.ok(!sb.cf().workers.backyard.bindings.some((b) => b.type === "ai"));
    assert.equal(sb.state().ai, false);
    assert.equal(sb.state().vars, undefined, "AI_ENABLED is managed, not a remembered override");
    assert.match(r.out, /Deploying Worker backyard without AI…/);
    assert.match(r.out, /AI is off \(--ai turns it on\): the AI endpoints answer 501 unsupported\./);
    assert.doesNotMatch(r.out, /AI runs on Workers AI/);
    // Remembered by the next run.
    r = await sb.run(["setup"], noWorkersAI);
    assert.equal(r.code, 0, r.out);
    d = deployOf(sb.calls());
    assert.equal(d.config.ai, undefined);
    assert.equal(d.config.vars.AI_ENABLED, "false");
    // While AI is off, AI_ENABLED is managed: --var for it is refused before anything happens.
    for (const argv of [
      ["setup", "--var", "AI_ENABLED:true"],
      ["setup", "--var", "AI_ENABLED:"],
      ["setup", "--no-ai", "--var", "AI_ENABLED:false"],
      ["setup", "--dry-run", "--var", "AI_ENABLED:true"],
    ]) {
      r = await sb.run(argv);
      assert.equal(r.code, 2, `${argv.join(" ")}: ${r.out}`);
      assert.match(r.out, /AI_ENABLED is managed by `npm run cf -- setup --no-ai` while AI is off \(`npm run cf -- setup --ai` turns AI back on\)/);
      assert.deepEqual(sb.calls(), [], argv.join(" "));
    }
    r = await sb.run(["setup", "--ai", "--no-ai"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /--ai and --no-ai contradict each other/);
    assert.deepEqual(sb.calls(), []);
    // --dry-run shows the remembered choice, and what --ai would deploy, changing nothing.
    const saved = readFileSync(sb.statePath(), "utf8");
    r = await sb.run(["setup", "--dry-run"]);
    assert.equal(r.code, 0, r.out);
    d = sb.calls()[0];
    assert.equal(d.config.ai, undefined);
    assert.equal(d.config.vars.AI_ENABLED, "false");
    assert.match(r.out, /AI_ENABLED=false/);
    assert.match(r.out, /AI: off \(no Workers AI binding\)/);
    r = await sb.run(["setup", "--dry-run", "--ai"]);
    assert.equal(r.code, 0, r.out);
    d = sb.calls()[0];
    assert.deepEqual(d.config.ai, { binding: "AI", remote: true });
    assert.equal(d.config.vars.AI_ENABLED, undefined);
    assert.match(r.out, /AI: on/);
    assert.equal(readFileSync(sb.statePath(), "utf8"), saved, "--dry-run writes no state");
    // --ai: the binding is back and AI_ENABLED is no longer set.
    r = await sb.run(["setup", "--ai"]);
    assert.equal(r.code, 0, r.out);
    d = deployOf(sb.calls());
    assert.deepEqual(d.config.ai, { binding: "AI", remote: true });
    assert.equal(d.config.vars.AI_ENABLED, undefined);
    assert.ok(sb.cf().workers.backyard.bindings.some((b) => b.type === "ai" && b.name === "AI"));
    assert.equal(sb.state().ai, undefined);
    assert.match(r.out, /AI runs on Workers AI/);
    // With AI on, --var AI_ENABLED:false turns only the API off and keeps the binding (remembered).
    r = await sb.run(["setup", "--var", "AI_ENABLED:false"]);
    assert.equal(r.code, 0, r.out);
    d = deployOf(sb.calls());
    assert.deepEqual(d.config.ai, { binding: "AI", remote: true });
    assert.equal(d.config.vars.AI_ENABLED, "false");
    assert.deepEqual(sb.state().vars, { AI_ENABLED: "false" });
    assert.match(r.out, /AI is off: AI_ENABLED is false \(--var AI_ENABLED: drops it\); the Workers AI binding stays\./);
    // --no-ai takes AI_ENABLED over: the override is dropped, the variable is managed.
    r = await sb.run(["setup", "--no-ai"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Dropped the AI_ENABLED override: with AI off \(--no-ai\) setup sets AI_ENABLED=false itself\./);
    d = deployOf(sb.calls());
    assert.equal(d.config.ai, undefined);
    assert.equal(d.config.vars.AI_ENABLED, "false");
    assert.equal(sb.state().vars, undefined);
    assert.equal(sb.state().ai, false);
    // --ai then turns AI fully on again: nothing remembered keeps the API off.
    r = await sb.run(["setup", "--ai"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.AI_ENABLED, undefined);
  });

  it("adopts AI off from a Worker deployed with --no-ai, and keeps AI on for a Worker merely without the binding", async () => {
    const sb = await newSandbox();
    seedWorker(sb, { vars: { AI_ENABLED: "false" }, ai: false });
    let r = await sb.run(["setup", "--no-r2"], { FAKE_AI: "disabled" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Adopted the deployed configuration of backyard \(no setup state on this machine\): AI off/);
    let d = deployOf(sb.calls());
    assert.equal(d.config.ai, undefined);
    assert.equal(d.config.vars.AI_ENABLED, "false");
    assert.equal(sb.state().ai, false);
    assert.equal(sb.state().vars, undefined, "AI_ENABLED is not copied as an override");
    // On a machine without state, a --var AI_ENABLED for a Worker adopted as AI off is
    // refused right after the live read: no workers.dev subdomain, no R2 bucket.
    forgetState(sb);
    sb.updateCf((s) => (s.buckets = []));
    sb.requests();
    r = await sb.run(["setup", "--var", "AI_ENABLED:true"], { FAKE_AI: "disabled" });
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /AI_ENABLED is managed by `npm run cf -- setup --no-ai` while AI is off \(`npm run cf -- setup --ai` turns AI back on\)/);
    assert.deepEqual(names(sb.calls()), ["whoami --json", "auth token"]);
    assert.deepEqual(sb.cf().buckets, [], "no R2 bucket was created");
    assert.ok(!sb.requests().some((x) => x.path.endsWith("/workers/subdomain")), "the workers.dev subdomain is not touched");
    assert.doesNotMatch(r.out, /R2 bucket/);
    // Without the binding but with AI on (another provider's keys): AI stays on, the binding comes back.
    forgetState(sb);
    seedWorker(sb, { ai: false });
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    assert.doesNotMatch(r.out, /AI off/);
    d = deployOf(sb.calls());
    assert.deepEqual(d.config.ai, { binding: "AI", remote: true });
    assert.equal(sb.state().ai, undefined);
  });

  it("lists sites only to their owners with --site-listing owners (SITE_LISTING managed), remembers it, and --site-listing all lists every site again", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup", "--no-r2", "--site-listing", "owners"]);
    assert.equal(r.code, 0, r.out);
    let d = deployOf(sb.calls());
    assert.equal(d.config.vars.SITE_LISTING, "owners");
    assert.equal(d.config.vars.OWNERSHIP, undefined, "the Worker implies OWNERSHIP=owner with SITE_LISTING=owners");
    assert.equal(sb.cf().workers.backyard.bindings.find((b) => b.name === "SITE_LISTING")?.text, "owners");
    assert.equal(sb.state().siteListing, "owners");
    assert.equal(sb.state().vars, undefined, "SITE_LISTING is managed, not a remembered override");
    assert.match(r.out, /Site listing: owners only \(--site-listing all lists every site\)\. Without company login visitors are anonymous guests and see no sites\./);
    // Remembered by the next run.
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "owners");
    assert.match(r.out, /Site listing: owners only/);
    // A --var that contradicts the listing, an OWNERSHIP other than owner, or a bad flag value is refused before anything happens.
    for (const [argv, re] of [
      [["setup", "--var", "SITE_LISTING:all"], /SITE_LISTING is managed by `npm run cf -- setup --site-listing`: the site listing is owners \(`npm run cf -- setup --site-listing all` changes it\)/],
      [["setup", "--var", "SITE_LISTING:"], /SITE_LISTING is managed by `npm run cf -- setup --site-listing`: the site listing is owners \(`npm run cf -- setup --site-listing all` changes it\)/],
      [["setup", "--site-listing", "all", "--var", "SITE_LISTING:owners"], /SITE_LISTING is managed by `npm run cf -- setup --site-listing`: the site listing is all \(`npm run cf -- setup --site-listing owners` changes it\)/],
      [["setup", "--dry-run", "--var", "SITE_LISTING:mine"], /SITE_LISTING is managed by `npm run cf -- setup --site-listing`: the site listing is owners \(`npm run cf -- setup --site-listing all` changes it\)/],
      [["setup", "--var", "OWNERSHIP:none"], /--var OWNERSHIP:none contradicts the owners-only site listing, which needs OWNERSHIP=owner \(its default with --site-listing owners; owners are recorded only then\): leave OWNERSHIP out, or pass --var OWNERSHIP:owner/],
      [["setup", "--dry-run", "--var", "OWNERSHIP:none"], /--var OWNERSHIP:none contradicts the owners-only site listing/],
      [["setup", "--site-listing", "mine"], /--site-listing must be all or owners \(got "mine"\)/],
      [["setup", "--site-listing", "Owners"], /--site-listing must be all or owners \(got "Owners"\)/],
      [["setup", "--site-listing"], /--site-listing needs a value/],
    ]) {
      r = await sb.run(argv);
      assert.equal(r.code, 2, `${argv.join(" ")}: ${r.out}`);
      assert.match(r.out, re, argv.join(" "));
      assert.deepEqual(sb.calls(), [], argv.join(" "));
    }
    // A --var that agrees is not kept: the flag sets the variable. OWNERSHIP:owner is a plain override.
    r = await sb.run(["setup", "--no-r2", "--var", "SITE_LISTING:owners", "--var", "OWNERSHIP:owner"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Dropped the SITE_LISTING override: setup --site-listing sets SITE_LISTING itself \(the site listing is owners\)\./);
    d = deployOf(sb.calls());
    assert.equal(d.config.vars.SITE_LISTING, "owners");
    assert.equal(d.config.vars.OWNERSHIP, "owner");
    assert.deepEqual(sb.state().vars, { OWNERSHIP: "owner" });
    // --dry-run shows the remembered choice, and what --site-listing all would deploy, changing nothing.
    const saved = readFileSync(sb.statePath(), "utf8");
    r = await sb.run(["setup", "--dry-run"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(sb.calls()[0].config.vars.SITE_LISTING, "owners");
    assert.match(r.out, /^ {2}SITE_LISTING=owners$/m);
    assert.match(r.out, /site listing: owners only/);
    r = await sb.run(["setup", "--dry-run", "--site-listing", "all"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(sb.calls()[0].config.vars.SITE_LISTING, undefined);
    assert.match(r.out, /site listing: every site/);
    assert.equal(readFileSync(sb.statePath(), "utf8"), saved, "--dry-run writes no state");
    // --site-listing all: SITE_LISTING is no longer set, and OWNERSHIP is a plain variable again.
    r = await sb.run(["setup", "--no-r2", "--site-listing", "all", "--var", "OWNERSHIP:none"]);
    assert.equal(r.code, 0, r.out);
    d = deployOf(sb.calls());
    assert.equal(d.config.vars.SITE_LISTING, undefined);
    assert.equal(d.config.vars.OWNERSHIP, "none");
    assert.equal(sb.state().siteListing, undefined);
    assert.deepEqual(sb.state().vars, { OWNERSHIP: "none" });
    assert.doesNotMatch(r.out, /Site listing:/);
    // Back to owners: the remembered OWNERSHIP:none is refused until it is dropped.
    r = await sb.run(["setup", "--site-listing", "owners"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /the remembered --var OWNERSHIP:none contradicts the owners-only site listing, which needs OWNERSHIP=owner .*: drop it with --var OWNERSHIP:/);
    assert.deepEqual(sb.calls(), []);
    r = await sb.run(["setup", "--no-r2", "--site-listing", "owners", "--var", "OWNERSHIP:"]);
    assert.equal(r.code, 0, r.out);
    d = deployOf(sb.calls());
    assert.equal(d.config.vars.SITE_LISTING, "owners");
    assert.equal(d.config.vars.OWNERSHIP, undefined);
    assert.equal(sb.state().vars, undefined);
    assert.equal(sb.state().siteListing, "owners");
    // An OWNERSHIP written into the base configuration needs an override.
    const config = join(sb.pkg, "wrangler.jsonc");
    writeFileSync(config, readFileSync(config, "utf8").replace(/"vars": \{/, '"vars": {\n    "OWNERSHIP": "none",'));
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /the base configuration \(wrangler\.jsonc\) sets OWNERSHIP=none, which contradicts the owners-only site listing.*: pass --var OWNERSHIP:owner/);
    assert.deepEqual(sb.calls(), []);
    r = await sb.run(["setup", "--no-r2", "--var", "OWNERSHIP:owner"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.OWNERSHIP, "owner");
  });

  it("adopts owners-only listing from the live Worker, and an older release's OWNERSHIP default is no override", async () => {
    const sb = await newSandbox();
    seedWorker(sb, { vars: { SITE_LISTING: "owners", ADMINS: "alice@example.com" } });
    let r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Adopted the deployed configuration of backyard \(no setup state on this machine\): owners-only site listing; variables ADMINS/);
    let d = deployOf(sb.calls());
    assert.equal(d.config.vars.SITE_LISTING, "owners");
    assert.equal(sb.state().siteListing, "owners");
    assert.deepEqual(sb.state().vars, { ADMINS: "alice@example.com" }, "SITE_LISTING is not copied as an override");
    // On a machine without state, an OWNERSHIP that contradicts the adopted listing is
    // refused right after the live read: no workers.dev subdomain, no R2 bucket.
    forgetState(sb);
    sb.updateCf((s) => (s.buckets = []));
    sb.requests();
    r = await sb.run(["setup", "--var", "OWNERSHIP:none"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /--var OWNERSHIP:none contradicts the owners-only site listing/);
    assert.deepEqual(names(sb.calls()), ["whoami --json", "auth token"]);
    assert.deepEqual(sb.cf().buckets, [], "no R2 bucket was created");
    assert.ok(!sb.requests().some((x) => x.path.endsWith("/workers/subdomain")), "the workers.dev subdomain is not touched");
    // A Worker an earlier release deployed (OWNERSHIP "none" written out): adopting it keeps
    // nothing, so --site-listing owners works right away.
    forgetState(sb);
    seedWorker(sb, { vars: { OWNERSHIP: "none" } });
    r = await sb.run(["setup", "--no-r2", "--site-listing", "owners"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Adopted the deployed configuration of backyard \(no setup state on this machine\): the defaults \(nothing to keep\)/);
    d = deployOf(sb.calls());
    assert.equal(d.config.vars.SITE_LISTING, "owners");
    assert.equal(d.config.vars.OWNERSHIP, undefined);
    assert.equal(sb.state().vars, undefined);
    // An invalid live value (the Worker fails closed) is adopted as owners-only, never as every site.
    forgetState(sb);
    seedWorker(sb, { vars: { SITE_LISTING: "mine" } });
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /SITE_LISTING="mine" is neither all nor owners \(the Worker refuses every request\): adopted as --site-listing owners/);
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "owners");
    // A SITE_LISTING override an earlier version remembered becomes the choice, and is dropped.
    sb.writeState("backyard", { ...sb.state(), siteListing: undefined, vars: { SITE_LISTING: "owners" } });
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Dropped the SITE_LISTING override: setup --site-listing sets SITE_LISTING itself \(the site listing is owners\)\./);
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "owners");
    assert.equal(sb.state().siteListing, "owners");
    assert.equal(sb.state().vars, undefined);
  });

  it("refuses a SITE_LISTING in wrangler.jsonc that contradicts the site listing (dry-run, real and adopted runs)", async () => {
    const sb = await newSandbox();
    const config = join(sb.pkg, "wrangler.jsonc");
    const setBase = (v) =>
      writeFileSync(config, readFileSync(config, "utf8").replace(/^ *(\/\/ )?"SITE_LISTING": "[^"]*",$/m, v === undefined ? '    // "SITE_LISTING": "owners",' : `    "SITE_LISTING": ${JSON.stringify(v)},`));
    const refusal = (base, listing) =>
      `the base configuration (wrangler.jsonc) sets SITE_LISTING=${base}, but the site listing is ${listing}: remove SITE_LISTING from wrangler.jsonc (\`npm run cf -- setup --site-listing <all|owners>\` sets it)`;
    setBase("owners");
    assert.equal(parseJSONC(readFileSync(config, "utf8")).vars.SITE_LISTING, "owners");
    // Without the flag or a remembered choice the listing is all: the base value would deploy
    // owners while setup says every site (and skip the OWNERSHIP check), so it is refused. On a
    // machine without state that happens after the live read (adoption may still pick owners),
    // before the workers.dev subdomain or an R2 bucket is touched.
    sb.requests();
    let r = await sb.run(["setup", "--var", "OWNERSHIP:none"]);
    assert.equal(r.code, 2, r.out);
    assert.ok(r.out.includes(refusal("owners", "all")), r.out);
    assert.deepEqual(names(sb.calls()), ["whoami --json", "auth token"]);
    assert.deepEqual(sb.cf().buckets ?? [], [], "no R2 bucket was created");
    assert.ok(!sb.requests().some((x) => x.path.endsWith("/workers/subdomain")), "the workers.dev subdomain is not touched");
    for (const argv of [
      ["setup", "--dry-run"],
      ["setup", "--site-listing", "all"],
      ["setup", "--dry-run", "--site-listing", "all"],
    ]) {
      r = await sb.run(argv);
      assert.equal(r.code, 2, `${argv.join(" ")}: ${r.out}`);
      assert.ok(r.out.includes(refusal("owners", "all")), `${argv.join(" ")}: ${r.out}`);
      assert.deepEqual(sb.calls(), [], argv.join(" "));
    }
    // A base value that agrees deploys; the flag (remembered) is what setup manages.
    r = await sb.run(["setup", "--no-r2", "--site-listing", "owners"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "owners");
    assert.equal(sb.state().siteListing, "owners");
    r = await sb.run(["setup", "--dry-run"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(sb.calls()[0].config.vars.SITE_LISTING, "owners");
    // With state, --site-listing all is refused before anything happens, real or dry run.
    for (const argv of [
      ["setup", "--no-r2", "--site-listing", "all"],
      ["setup", "--dry-run", "--site-listing", "all"],
    ]) {
      r = await sb.run(argv);
      assert.equal(r.code, 2, `${argv.join(" ")}: ${r.out}`);
      assert.ok(r.out.includes(refusal("owners", "all")), `${argv.join(" ")}: ${r.out}`);
      assert.deepEqual(sb.calls(), [], argv.join(" "));
    }
    assert.equal(sb.state().siteListing, "owners", "a refused run changes nothing");
    // Adopted runs: a live owners-only listing agrees with the base value, a live "every site" does not.
    forgetState(sb);
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Adopted the deployed configuration of backyard .*owners-only site listing/);
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "owners");
    forgetState(sb);
    seedWorker(sb, { vars: { SITE_LISTING: "all" } });
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 2, r.out);
    assert.ok(r.out.includes(refusal("owners", "all")), r.out);
    assert.deepEqual(names(sb.calls()), ["whoami --json", "auth token"]);
    // Read like the Worker (trimmed, case-insensitive): "all" contradicts owners, an invalid value both.
    setBase(" Owners ");
    r = await sb.run(["setup", "--no-r2", "--site-listing", "owners"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "owners");
    setBase("all");
    r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 2, r.out);
    assert.ok(r.out.includes(refusal("all", "owners")), r.out);
    assert.deepEqual(sb.calls(), []);
    setBase("mine");
    for (const listing of ["owners", "all"]) {
      r = await sb.run(["setup", "--dry-run", "--site-listing", listing]);
      assert.equal(r.code, 2, r.out);
      assert.ok(r.out.includes(refusal("mine", listing)), r.out);
    }
    setBase("all");
    r = await sb.run(["setup", "--no-r2", "--site-listing", "all"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "all");
    assert.equal(sb.state().siteListing, undefined);
    setBase("");
    r = await sb.run(["setup", "--no-r2", "--site-listing", "owners"]);
    assert.equal(r.code, 0, r.out, "an empty base value is unset");
    assert.equal(deployOf(sb.calls()).config.vars.SITE_LISTING, "owners");
  });

  it("exits 4 with the reason when the hub does not become ready, instead of saying it is live", async () => {
    const sb = await newSandbox();
    const fast = { FAKE_FAST_SLEEP: "1", BACKYARD_CF_RESULT_FILE: sb.resultPath };
    // The Worker fails closed (e.g. AUTH_MODE=oidc from the Go server): its own reason is shown.
    sb.updateCf((s) => (s.edge = { readyz: 503, readyzBody: 'not ready: AUTH_MODE must be "none" or "access" (got "oidc")\n' }));
    let r = await sb.run(["setup", "--no-r2"], fast);
    assert.equal(r.code, 4, r.out);
    assert.match(r.out, /Waiting for https:\/\/backyard\.acme\.workers\.dev to answer… not yet/);
    assert.ok(
      r.out.includes('Deployed to https://backyard.acme.workers.dev, but it is not ready: /__backyard/readyz answers HTTP 503: not ready: AUTH_MODE must be "none" or "access" (got "oidc").'),
      r.out,
    );
    assert.match(r.out, /Check with npm run cf -- status; re-run npm run cf -- setup once it is fixed/);
    assert.doesNotMatch(r.out, /Backyard is live|workers\.dev can take a minute/);
    assert.match(r.out, /backyard login --server https:\/\/backyard\.acme\.workers\.dev --token/, "the login line is still there");
    assert.equal(sb.fake.edgeRequests.filter((x) => x.path === "/__backyard/readyz").length, 15);
    assert.equal(sb.result().command, "setup", "the result file is written");
    // A name that does not resolve yet: the DNS hint.
    sb.updateCf((s) => {
      s.edge = {};
      s.dns = { "backyard.acme.workers.dev": "nxdomain" };
    });
    r = await sb.run(["setup", "--no-r2"], fast);
    assert.equal(r.code, 4, r.out);
    assert.match(r.out, /but it is not ready: DNS or the certificate is still provisioning/);
    // Ready: exit 0.
    sb.updateCf((s) => (s.dns = {}));
    r = await sb.run(["setup", "--no-r2"], fast);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Backyard is live at https:\/\/backyard\.acme\.workers\.dev/);
  });

  it("leaves the login and the next step to the Go CLI when it logs in itself, and says where the CLI comes from otherwise", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Connect the CLI with the deploy token/);
    assert.ok(r.out.includes("No backyard CLI yet? curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh"), r.out);
    assert.match(r.out, /Then: backyard init my-site && cd my-site && backyard deploy/);
    // Run by `backyard cloudflare`, which logs in with the deploy token and prints the next step.
    r = await sb.run(["setup", "--no-r2"], { BACKYARD_CF_INVOKED_AS: "backyard cloudflare", BACKYARD_CF_CLI_LOGIN: "1" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Backyard is live at/);
    assert.doesNotMatch(r.out, /Connect the CLI|backyard login --server|Then: |No backyard CLI yet/);
    assert.match(r.out, /The deploy token is saved in/);
    // `backyard cloudflare --no-login`: the CLI is installed, the login line stays.
    r = await sb.run(["setup", "--no-r2"], { BACKYARD_CF_INVOKED_AS: "backyard cloudflare" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /backyard login --server https:\/\/backyard\.acme\.workers\.dev --token/);
    assert.match(r.out, /Then: backyard init my-site/);
    assert.doesNotMatch(r.out, /No backyard CLI yet/);
  });

  it("asks for an account only when it must", async () => {
    const sb = await newSandbox({ cf: { accounts: { a1: { name: "Personal", subdomain: "personal" }, a2: { name: "Work", subdomain: "work" } } } });
    // Non-interactive with several accounts: a clear error, nothing deployed.
    let r = await sb.run(["setup"]);
    assert.equal(r.code, 2, r.out);
    // The names tell the accounts apart (real ids are 32 hex characters).
    assert.match(r.out, /this login can deploy to 2 Cloudflare accounts: set CLOUDFLARE_ACCOUNT_ID to one of: Personal \(a1\), Work \(a2\)/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    // status reports the same answer (its live checks are pending).
    r = await sb.run(["status"]);
    assert.equal(r.code, 4, r.out);
    assert.match(r.out, /set CLOUDFLARE_ACCOUNT_ID to one of: Personal \(a1\), Work \(a2\)/);
    // CLOUDFLARE_ACCOUNT_ID decides (and is remembered).
    r = await sb.run(["setup"], { CLOUDFLARE_ACCOUNT_ID: "a2" });
    assert.equal(r.code, 0, r.out);
    assert.ok(sb.calls().filter((x) => x.args[0] !== "whoami").every((x) => x.account === "a2"));
    assert.equal(sb.state().url, "https://backyard.work.workers.dev");
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.ok(sb.calls().slice(1).every((x) => x.account === "a2"));
    assert.ok(sb.requests().every((x) => x.path.startsWith("/accounts/a2/")));
  });

  it("uses API tokens non-interactively and fails clearly when logged out", async () => {
    const apiToken = "ci-api-token-0123456789abcdef";
    const sb = await newSandbox({ cf: { tokens: { [apiToken]: { label: "ci", permissions: [...TOKEN_PRESETS.workers] } } } });
    // The admin token is for Backyard's own REST calls only: never given to wrangler, never stored.
    const adminToken = "admin-token-for-dns-and-access-0123456789";
    const env = { CLOUDFLARE_API_TOKEN: apiToken, CLOUDFLARE_ACCOUNT_ID: "acc1", DEPLOY_TOKEN: "byd_my-token-0123456789", BACKYARD_CF_API_TOKEN: adminToken };
    let r = await sb.run(["setup", "--worker-name", "team-yard"], env);
    assert.equal(r.code, 0, r.out);
    const c = sb.calls();
    assert.ok(!c.some((x) => x.args[0] === "whoami"), "no whoami with token and account");
    assert.ok(c.some((x) => x.args.join(" ").startsWith("deploy --name team-yard")));
    assert.equal(secretsOf(sb, "team-yard").DEPLOY_TOKEN, "byd_my-token-0123456789");
    assert.ok(deployOf(c).secretsFile.includes("DEPLOY_TOKEN"));
    assert.equal(sb.state("team-yard").deployToken, "byd_my-token-0123456789");
    assert.ok(sb.requests().every((x) => x.token === "ci"), "the REST calls use wrangler's credential");
    // A token from the environment is referred to, never echoed.
    assert.ok(!r.out.includes("byd_my-token-0123456789"), r.out);
    assert.match(r.out, /--token \$DEPLOY_TOKEN/);
    sb.allowInEnv = [apiToken, "byd_my-token-0123456789"];
    sb.extraSecrets = [adminToken];
    assertNoLeaks(sb, { outputs: [r.out], allowInEnv: sb.allowInEnv, extra: [adminToken] });
    assert.ok(sb.allCalls().every((x) => !("BACKYARD_CF_API_TOKEN" in x.env)));

    r = await sb.run(["setup"], { FAKE_LOGGED_OUT: "1" });
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /not logged in to Cloudflare/);
    r = await sb.run(["setup", "--bogus"]);
    assert.equal(r.code, 2);
    assert.match(r.out, /unknown option --bogus/);
    assert.match(r.out, /Usage: npm run cf -- setup/);
  });

  it("keeps a DEPLOY_TOKEN the Worker has when this machine does not know it (second machine, CI)", async () => {
    const sb = await newSandbox();
    // Set up from "laptop A".
    let r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    sb.calls();
    const before = secretsOf(sb);
    const token = before.DEPLOY_TOKEN;
    // "Laptop B" or a CI runner: no state file.
    forgetState(sb);
    r = await sb.run(["setup"], { CI: "true" });
    assert.equal(r.code, 0, r.out);
    const c = sb.calls();
    const d = deployOf(c);
    assert.ok(d, "it still deploys");
    assert.ok(!(d.secretsFile ?? []).includes("DEPLOY_TOKEN"), "DEPLOY_TOKEN is not uploaded");
    assert.ok(!c.some((x) => x.secret), "no secret is put");
    assert.deepEqual(secretsOf(sb), before, "the Worker's secrets are unchanged");
    assert.match(r.out, /already has a DEPLOY_TOKEN that this machine does not know: keeping it/);
    assert.match(r.out, /--token <DEPLOY_TOKEN>/);
    assert.match(r.out, /Adopted the deployed configuration of backyard \(no setup state on this machine\): R2 bucket backyard-files/);
    assert.equal(sb.state().deployToken, undefined, "no token is remembered that is not the Worker's");
    assert.ok(!r.out.includes(token));
    // Telling it the token remembers it again, without rotating anything.
    sb.allowInEnv = [token];
    r = await sb.run(["setup"], { DEPLOY_TOKEN: token });
    assert.equal(r.code, 0, r.out);
    sb.calls();
    assert.equal(secretsOf(sb).DEPLOY_TOKEN, token);
    assert.equal(sb.state().deployToken, token);
    // --rotate-token still issues a new one on purpose.
    forgetState(sb);
    r = await sb.run(["setup", "--rotate-token"]);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(deployOf(sb.calls()).secretsFile, ["DEPLOY_TOKEN"]);
    assert.notEqual(secretsOf(sb).DEPLOY_TOKEN, token);
    assertNoLeaks(sb, { outputs: [r.out] });
  });

  it("keeps a broken state file and an unknown Worker from rotating anything", async () => {
    const sb = await newSandbox();
    // --skip-deploy cannot set secrets on a Worker that does not exist yet.
    let r = await sb.run(["setup", "--skip-deploy"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /does not exist yet/);
    assert.ok(!sb.calls().some((x) => x.secret || x.args[0] === "deploy"));

    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    sb.calls();
    const token = sb.state().deployToken;
    // An older version may have left the state readable by others: fixed on save.
    chmodSync(sb.statePath(), 0o644);
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    sb.calls();
    if (process.platform !== "win32") assert.equal(sb.stateMode(), "600");
    assert.equal(sb.state().deployToken, token);

    // A corrupt state file stops the setup instead of issuing a new token.
    for (const bad of ["{ not json", "[]"]) {
      sb.writeState("backyard", bad);
      r = await sb.run(["setup"]);
      assert.equal(r.code, 1, r.out);
      assert.match(r.out, /is not a valid state file/);
      assert.ok(!sb.calls().some((x) => x.args[0] === "deploy" || x.secret));
    }
  });

  it("leaves no secrets file behind on Ctrl-C during the deploy, and remembers a new deploy token first", async () => {
    for (const layout of ["packaged", "source"]) {
      const sb = await newSandbox({ layout });
      const tmp = join(sb.root, "tmp");
      mkdirSync(tmp);
      // Ctrl-C while wrangler uploads (the secrets file with BACKYARD_SECRET exists then).
      let r = await sb.run(["setup", "--no-r2"], { TMPDIR: tmp, FAKE_DEPLOY_DELAY_MS: "20000" }, { interrupt: /Uploading \(fake: slow\)/ });
      assert.equal(r.code, 130, `${layout}: ${r.out}`);
      assert.match(r.out, /Interrupted\./);
      assert.deepEqual(readdirSync(tmp), [], `${layout}: nothing is left in TMPDIR`);
      assert.ok(!existsSync(join(sb.pkg, "wrangler.setup.jsonc")), `${layout}: the generated config is gone`);
      // The token was saved before the upload, so the next run uploads that one.
      const token = sb.state().deployToken;
      assert.match(token, /^byd_/, layout);
      r = await sb.run(["setup", "--no-r2"], { TMPDIR: tmp });
      assert.equal(r.code, 0, `${layout}: ${r.out}`);
      assert.equal(secretsOf(sb).DEPLOY_TOKEN, token, layout);
      assert.equal(sb.state().deployToken, token, layout);
      assert.deepEqual(readdirSync(tmp), [], layout);
    }
  });

  it("saves a rotated deploy token only once the deploy worked", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup", "--no-r2"]);
    assert.equal(r.code, 0, r.out);
    const token = sb.state().deployToken;
    r = await sb.run(["setup", "--no-r2", "--rotate-token"], { FAKE_DEPLOY_FAIL: "1" });
    assert.equal(r.code, 1, r.out);
    assert.equal(sb.state().deployToken, token, "the state still names the token the Worker has");
    assert.equal(secretsOf(sb).DEPLOY_TOKEN, token);
  });

  it("deploys the prebuilt Worker of the packaged layout from a private run directory", async () => {
    for (const wrangler of ["nested", "hoisted"]) {
      const sb = await newSandbox({ layout: "packaged", wrangler });
      const before = sb.snapshot();
      makeReadOnly(sb.pkg);
      try {
        const r = await sb.run(["setup"]);
        assert.equal(r.code, 0, `${wrangler}: ${r.out}`);
        assert.equal(sb.syncRuns(), 0, "no build step in the package");
        const c = sb.calls();
        assert.deepEqual(names(c), ["whoami --json", "auth token", "r2 bucket", "secret list", "deploy --name"]);
        const d = deployOf(c);
        assert.equal(d.config.main, join(sb.pkg, "dist", "worker.js"));
        assert.equal(d.config.no_bundle, true);
        assert.equal(d.config.find_additional_modules, false);
        for (const k of ["build", "env", "$schema", "routes"]) assert.equal(d.config[k], undefined, k);
        assert.equal(d.config.name, "backyard");
        assert.deepEqual(
          d.config.durable_objects.bindings.map((b) => b.name),
          ["SITE", "REGISTRY"],
        );
        assert.deepEqual(d.config.ai, { binding: "AI", remote: true }, "dist/wrangler.base.json binds Workers AI");
        // Every wrangler call ran in a private temporary directory, gone afterwards.
        const cwds = new Set(c.map((x) => x.cwd));
        assert.equal(cwds.size, 1);
        for (const cwd of cwds) {
          assert.ok(!cwd.startsWith(sb.pkg), cwd);
          assert.match(cwd, /backyard-cf-/);
          assert.ok(!existsSync(cwd), `${cwd} is removed`);
        }
        assert.equal(d.configPath, join([...cwds][0], "wrangler.json"));
        assert.ok(r.out.includes("Your own domain: backyard-cloudflare domain add <host>"), r.out);
        assertNoLeaks(sb, { outputs: [r.out] });
      } finally {
        makeWritable(sb.pkg);
      }
      assert.deepEqual(sb.snapshot(), before, "nothing is written into the package");
      // npx sets npm_command=exec: the hints name npx.
      let r = await sb.run(["setup", "--dry-run"], { npm_command: "exec" });
      assert.equal(r.code, 0, r.out);
      // --no-ai leaves the binding of dist/wrangler.base.json out.
      sb.calls();
      r = await sb.run(["setup", "--no-ai"], { FAKE_AI: "disabled" });
      assert.equal(r.code, 0, `${wrangler}: ${r.out}`);
      const d = deployOf(sb.calls());
      assert.equal(d.config.ai, undefined);
      assert.equal(d.config.vars.AI_ENABLED, "false");
    }
  });

  it("registers the account's workers.dev subdomain from the terminal, and never renames one", async () => {
    // No subdomain, non-interactive, no flag: exit 2 with the onboarding link, nothing deployed.
    const sb = await newSandbox({ cf: { accounts: { acc1: { name: "Acme Inc.", subdomain: null } }, takenSubdomains: ["taken"] } });
    let r = await sb.run(["setup"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /this account has no workers\.dev subdomain: pass --workers-subdomain <name>, or register one at https:\/\/dash\.cloudflare\.com\/acc1\/workers\/onboarding/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy" || x.args[0] === "r2"));
    assert.equal(sb.fake.writes().length, 0);
    // A taken name: exit 1, nothing registered.
    r = await sb.run(["setup", "--workers-subdomain", "taken"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /the workers\.dev subdomain taken is taken/);
    assert.equal(sb.fake.writes().length, 0);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    // An invalid name: exit 2.
    r = await sb.run(["setup", "--workers-subdomain", "-bad-"]);
    assert.equal(r.code, 2, r.out);
    // --workers-subdomain acme: availability check, then the PUT.
    sb.requests();
    r = await sb.run(["setup", "--workers-subdomain", "acme"]);
    assert.equal(r.code, 0, r.out);
    // (After the live settings: the subdomain is resolved once the adopted workers.dev setting is known.)
    const reqs = sb.requests().filter((x) => /\/workers\/subdomains?(\/|$)/.test(x.path));
    assert.deepEqual(
      reqs.slice(0, 3).map((x) => `${x.method} ${x.path} ${x.status}`),
      ["GET /accounts/acc1/workers/subdomain 404", "GET /accounts/acc1/workers/subdomains/acme 404", "PUT /accounts/acc1/workers/subdomain 200"],
    );
    assert.deepEqual(reqs[2].body, { subdomain: "acme" });
    assert.match(r.out, /workers\.dev subdomain acme registered \(DNS can take a few minutes\)/);
    assert.equal(sb.state().workersDevUrl, "https://backyard.acme.workers.dev");
    assert.equal(sb.state().url, "https://backyard.acme.workers.dev");
    // An existing (different) subdomain is never renamed.
    r = await sb.run(["setup", "--workers-subdomain", "other"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /account already uses acme; --workers-subdomain ignored/);
    assert.ok(!sb.requests().some((x) => x.method === "PUT"));
    assert.equal(suggestSubdomain("Acme Inc."), "acme-inc");
    assert.equal(suggestSubdomain("--Ünïcode__Corp--"), "n-code-corp");
    assert.equal(suggestSubdomain("x".repeat(80)).length, 63);
  });

  it("asks on a terminal: the account, the workers.dev subdomain (again when taken), and shows the hub link", { skip: !ttyAvailable() && "needs util-linux script" }, async () => {
    const sb = await newSandbox({ cf: { accounts: { acc1: { name: "Acme Inc.", subdomain: null }, acc2: { name: "Other", subdomain: "other" } }, takenSubdomains: ["taken"] } });
    const r = await sb.runTTY(["setup"], {}, {
      answers: [
        [/Deploy Backyard to which account\? \[1-2\]/, "1\r"],
        [/workers\.dev subdomain for this account \[acme-inc\]/, "taken\r"],
        [/taken\.workers\.dev is taken; please choose another\s+workers\.dev subdomain for this account \[acme-inc\]/, "\r"],
      ],
    });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /workers\.dev subdomain acme-inc registered/);
    const token = sb.state().deployToken;
    assert.equal(sb.state().url, "https://backyard.acme-inc.workers.dev");
    // On a terminal (outside CI) the token is shown once: the hub link and the login line.
    assert.ok(r.out.includes(`https://backyard.acme-inc.workers.dev/#token=${token}`), r.out);
    assert.ok(r.out.includes(`backyard login --server https://backyard.acme-inc.workers.dev --token ${token}`), r.out);
    assert.deepEqual(sb.requests().filter((x) => x.method === "PUT").map((x) => x.body), [{ subdomain: "acme-inc" }]);
    // CI=true is never interactive, even on a terminal.
    const ci = await sb.runTTY(["setup"], { CI: "true" });
    assert.equal(ci.code, 0, ci.out);
    assert.ok(!ci.out.includes(token), "masked in CI");
    sb.outputs = [ci.out];
  });

  it("falls back to the deploy output when the credential cannot read the subdomain (code 10000)", async () => {
    const sb = await newSandbox();
    sb.fake.addFault({ method: "GET", path: "/accounts/acc1/workers/subdomain", status: 403, code: 10000, message: "Authentication error", times: 2 });
    const r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /cannot read the account's workers\.dev subdomain/);
    assert.equal(sb.state().url, "https://backyard.acme.workers.dev");
    assert.equal(sb.state().workersDevUrl, "https://backyard.acme.workers.dev");
    // The credential was re-read once after the 10000 (an expired OAuth token gets refreshed).
    assert.equal(sb.calls().filter((x) => x.args[0] === "auth").length, 2);
  });

  it("adopts the deployed configuration on a machine without state", async () => {
    const sb = await newSandbox();
    seedWorker(sb, {
      vars: { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: AUD, BASE_DOMAIN: "backyard.example.com", LIMITS: '{"ai_per_day":5}' },
      r2Bucket: "custom",
      customDomains: ["backyard.example.com"],
    });
    const r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.match(
      r.out,
      /Adopted the deployed configuration of backyard \(no setup state on this machine\): R2 bucket custom; variables LIMITS; domain backyard\.example\.com \(path mode\); Access team acme\.cloudflareaccess\.com/,
    );
    const d = deployOf(sb.calls());
    assert.equal(d.config.vars.AUTH_MODE, "access");
    assert.equal(d.config.vars.ACCESS_TEAM_DOMAIN, "acme.cloudflareaccess.com");
    assert.equal(d.config.vars.ACCESS_AUD, AUD);
    assert.equal(d.config.vars.BASE_DOMAIN, "backyard.example.com");
    assert.equal(d.config.vars.URL_MODE, "path");
    assert.equal(d.config.vars.LIMITS, '{"ai_per_day":5}');
    assert.deepEqual(d.config.r2_buckets, [{ binding: "R2", bucket_name: "custom" }]);
    assert.equal(d.secretsFile, undefined, "no secret changes");
    const st = sb.state();
    assert.deepEqual(st.domain, { host: "backyard.example.com", mode: "path", status: "active", source: "adopted" });
    assert.deepEqual(st.access, { team: "acme.cloudflareaccess.com", aud: AUD, source: "adopted" });
    assert.deepEqual(st.vars, { LIMITS: '{"ai_per_day":5}' });
    assert.equal(st.r2Bucket, "custom");
    assert.equal(st.url, "https://backyard.example.com");
    assert.match(r.out, /--token <DEPLOY_TOKEN>/);
    // Managed now: --var for them is refused.
    for (const [v, by] of [
      ["BASE_DOMAIN:x.example.com", /BASE_DOMAIN is managed by `npm run cf -- domain add` \(use `npm run cf -- domain remove`\)/],
      ["AUTH_MODE:none", /AUTH_MODE is managed by `npm run cf -- access enable` \(use `npm run cf -- access disable`\)/],
    ]) {
      const rr = await sb.run(["setup", "--var", v]);
      assert.equal(rr.code, 2, rr.out);
      assert.match(rr.out, by);
    }
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    assertNoLeaks(sb, { outputs: [r.out] });
  });

  it("keeps --var for managed keys working when nothing manages them (the Terraform hand-off)", async () => {
    const sb = await newSandbox();
    seedWorker(sb, { customDomains: ["backyard.example.com"] });
    sb.writeState("backyard", { accountId: "acc1", updatedAt: "2026-01-01T00:00:00Z", deployToken: "byd_existing-token-0123456789abcdef" });
    let r = await sb.run(["setup", "--var", "BASE_DOMAIN:backyard.example.com", "--var", "URL_MODE:path"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.BASE_DOMAIN, "backyard.example.com");
    assert.deepEqual(sb.state().vars, { BASE_DOMAIN: "backyard.example.com", URL_MODE: "path" });
    assert.equal(sb.state().domain, undefined);
    assert.equal(sb.state().url, "https://backyard.example.com");
    // A second machine running the module's setup_args: the adopted domain stays external.
    forgetState(sb);
    r = await sb.run(["setup", "--var", "BASE_DOMAIN:backyard.example.com", "--var", "URL_MODE:path"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Adopted the deployed configuration/);
    assert.equal(sb.state().domain, undefined);
    assert.deepEqual(sb.state().vars, { BASE_DOMAIN: "backyard.example.com", URL_MODE: "path" });
    sb.calls();
    // --var KEY: drops it again.
    r = await sb.run(["setup", "--var", "BASE_DOMAIN:", "--var", "URL_MODE:"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(sb.state().vars, undefined);
    assert.equal(deployOf(sb.calls()).config.vars.BASE_DOMAIN, "");
  });

  it("refuses a secret as --var before anything happens (not a plain-text variable)", async () => {
    const sb = await newSandbox();
    for (const [name, hint] of [
      ["DEPLOY_TOKEN", /DEPLOY_TOKEN=<token>/],
      ["BACKYARD_SECRET", /--rotate-secret/],
      ["ANTHROPIC_API_KEY", /npx wrangler secret put ANTHROPIC_API_KEY --name backyard/],
    ]) {
      const r = await sb.run(["setup", "--var", `${name}:not-a-variable-value`]);
      assert.equal(r.code, 2, r.out);
      assert.match(r.out, new RegExp(`${name} is a secret, not a variable: never pass it with --var`));
      assert.match(r.out, hint);
      assert.doesNotMatch(r.out, /not-a-variable-value/, "the value is not echoed");
    }
    // Written with "=" (the habit of other tools): refused the same way, the value never echoed.
    for (const v of ["DEPLOY_TOKEN=not-a-variable-value", "deploy_token=not-a-variable-value"]) {
      const r = await sb.run(["setup", "--var", v]);
      assert.equal(r.code, 2, r.out);
      assert.match(r.out, /DEPLOY_TOKEN is a secret, not a variable: never pass it with --var/);
      assert.doesNotMatch(r.out, /not-a-variable-value/, "the value is not echoed");
    }
    // Any other malformed --var names only its key.
    let r = await sb.run(["setup", "--var", "SERVER_NAME=not-a-variable-value"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /--var expects KEY:VALUE \(with ":"\), got "SERVER_NAME" followed by a value/);
    assert.doesNotMatch(r.out, /not-a-variable-value/);
    // Also with --dry-run, which prints the variables it would deploy.
    r = await sb.run(["setup", "--dry-run", "--var", "WAREHOUSE_TOKEN:not-a-variable-value"]);
    assert.equal(r.code, 2, r.out);
    assert.deepEqual(sb.calls(), [], "no wrangler call");
    assert.equal(sb.hasState("backyard"), false);
  });

  it("refuses to turn a live Cloudflare Access off, or to change its team, unless confirmed", async () => {
    const sb = await newSandbox();
    seedWorker(sb, { vars: { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme.cloudflareaccess.com", ACCESS_AUD: AUD, BASE_DOMAIN: "backyard.example.com" }, customDomains: ["backyard.example.com"] });
    // This machine set it up before another machine turned Access on.
    const old = { accountId: "acc1", updatedAt: "2026-01-01T00:00:00Z", deployToken: "byd_existing-token-0123456789abcdef", url: "https://backyard.acme.workers.dev" };
    sb.writeState("backyard", old);
    let r = await sb.run(["setup"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /Worker backyard is live with Cloudflare Access \(team acme\.cloudflareaccess\.com\), and this deploy would turn it off \(AUTH_MODE=none\)\. Nothing was changed\./);
    assert.ok(r.out.includes(`delete ${sb.statePath()} to adopt the live configuration`), r.out);
    assert.match(r.out, /npm run cf -- setup --confirm-auth-change/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy" || x.secret));
    assert.equal(sb.cf().workers.backyard.deployments, 1);
    // A team change is refused too.
    sb.writeState("backyard", { ...old, domain: { host: "backyard.example.com", mode: "path", status: "active", source: "cli" }, access: { team: "other", aud: AUD, source: "manual" } });
    r = await sb.run(["setup"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /live with Cloudflare Access team acme\.cloudflareaccess\.com, and this deploy would switch it to other\.cloudflareaccess\.com/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    // --confirm-auth-change deploys anyway.
    sb.writeState("backyard", old);
    r = await sb.run(["setup", "--confirm-auth-change"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /deploying anyway \(confirmed\)/);
    assert.equal(deployOf(sb.calls()).config.vars.AUTH_MODE, "none");
  });

  it("refuses to deploy over a Worker whose live auth mode cannot be read", async () => {
    const sb = await newSandbox();
    seedWorker(sb);
    sb.writeState("backyard", { accountId: "acc1", updatedAt: "2026-01-01T00:00:00Z", deployToken: "byd_existing-token-0123456789abcdef", url: "https://backyard.acme.workers.dev", workersDevUrl: "https://backyard.acme.workers.dev" });
    sb.fake.addFault({ method: "GET", path: "*/workers/scripts/backyard/settings", status: 500, code: 10013, times: Infinity, retryAfter: 0 });
    // The Worker's own /info answers: AUTH_MODE=none, nothing to protect.
    let r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.ok(sb.fake.edgeRequests.some((x) => x.path === "/__backyard/api/deploy/info"));
    // Settings 500 and /info unreachable on an existing Worker: refused.
    sb.updateCf((s) => (s.edge = { down: ["backyard.acme.workers.dev"] }));
    sb.calls();
    r = await sb.run(["setup"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /could not read the live configuration of Worker backyard .*Nothing was changed\./s);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    // A first deploy (wrangler: Worker not found) needs no guard even then.
    sb.updateCf((s) => {
      delete s.workers.backyard;
      s.edge = {};
    });
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    sb.fake.clearFaults();
  });

  it("turns workers.dev off only with a working custom domain, and remembers it", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup", "--no-workers-dev"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /turning workers\.dev off needs a working custom domain \(npm run cf -- domain add <host>\)/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    seedWorker(sb, { customDomains: ["backyard.example.com"] });
    sb.writeState("backyard", {
      accountId: "acc1",
      updatedAt: "2026-01-01T00:00:00Z",
      deployToken: "byd_existing-token-0123456789abcdef",
      domain: { host: "backyard.example.com", mode: "path", zoneId: "zone-example", zoneName: "example.com", status: "active", source: "cli" },
    });
    r = await sb.run(["setup", "--no-workers-dev"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.workers_dev, false);
    assert.equal(sb.state().workersDev, false);
    assert.equal(sb.state().url, "https://backyard.example.com");
    assert.equal(sb.state().workersDevUrl, "https://backyard.acme.workers.dev", "kept for later");
    sb.requests();
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.workers_dev, false, "remembered");
    assert.ok(!sb.requests().some((x) => x.path.endsWith("/workers/subdomain")), "no subdomain lookup while off");
    r = await sb.run(["setup", "--workers-dev"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.workers_dev, true);
    assert.equal(sb.state().workersDev, true);
    r = await sb.run(["setup", "--workers-dev", "--no-workers-dev"]);
    assert.equal(r.code, 2, r.out);
  });

  it("never registers a workers.dev subdomain for a Worker that does not serve workers.dev", async () => {
    // The account has no workers.dev subdomain (deleted after moving to the custom domain).
    const sb = await newSandbox({ cf: { accounts: { acc1: { name: "Acme", subdomain: null } } } });
    seedWorker(sb, { customDomains: ["backyard.example.com"], vars: { BASE_DOMAIN: "backyard.example.com" } });
    sb.writeState("backyard", {
      accountId: "acc1",
      updatedAt: "2026-01-01T00:00:00Z",
      deployToken: "byd_existing-token-0123456789abcdef",
      domain: { host: "backyard.example.com", mode: "path", zoneId: "zone-example", zoneName: "example.com", status: "active", source: "cli" },
    });
    let r = await sb.run(["setup", "--no-workers-dev"]);
    assert.equal(r.code, 0, r.out);
    assert.doesNotMatch(r.out, /no workers\.dev subdomain/);
    assert.equal(deployOf(sb.calls()).config.workers_dev, false);
    assert.ok(!sb.requests().some((x) => x.method === "PUT" && x.path.endsWith("/workers/subdomain")), "nothing registered");
    assert.equal(sb.cf().accounts.acc1.subdomain, null);
    // A second machine adopts a Worker that is off workers.dev: a plain setup works too.
    forgetState(sb);
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /workers\.dev off/);
    assert.equal(deployOf(sb.calls()).config.workers_dev, false);
    assert.ok(!sb.requests().some((x) => x.method === "PUT" && x.path.endsWith("/workers/subdomain")));
    // --workers-dev on that machine is applied after the adoption (and needs the subdomain again).
    r = await sb.run(["setup", "--workers-dev", "--non-interactive"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /this account has no workers\.dev subdomain: pass --workers-subdomain <name>/);
  });

  it("validates offline with --dry-run: no lock, no login, no API call, no state write", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["setup", "--dry-run", "--non-interactive", "--var", "SERVER_NAME:Team"]);
    assert.equal(r.code, 0, r.out);
    const c = sb.calls();
    assert.deepEqual(names(c), ["deploy --dry-run"]);
    assert.ok(c[0].args.includes("--outdir"));
    assert.equal(c[0].config.vars.SERVER_NAME, "Team");
    assert.equal(c[0].config.routes, undefined);
    assert.equal(sb.fake.requests.length, 0);
    assert.ok(!sb.hasState());
    assert.ok(!existsSync(join(sb.root, "config", "backyard", "cloudflare", "backyard.lock")));
    assert.match(r.out, /Effective variables:/);
    assert.match(r.out, /SERVER_NAME=Team/);
    // It reads the state when there is one, and returns wrangler's exit code.
    sb.writeState("backyard", { vars: { LIMITS: '{"max_sites":3}' }, r2Bucket: "kept", updatedAt: "2026-01-01T00:00:00Z" });
    r = await sb.run(["setup", "--dry-run"]);
    assert.equal(r.code, 0, r.out);
    const d = sb.calls()[0];
    assert.equal(d.config.vars.LIMITS, '{"max_sites":3}');
    assert.deepEqual(d.config.r2_buckets, [{ binding: "R2", bucket_name: "kept" }]);
    assert.deepEqual(sb.state(), { vars: { LIMITS: '{"max_sites":3}' }, r2Bucket: "kept", updatedAt: "2026-01-01T00:00:00Z" }, "unchanged");
    r = await sb.run(["setup", "--dry-run", "--rotate-token"]);
    assert.equal(r.code, 2, r.out);
    // Routes in a checkout's wrangler.jsonc are ignored (the fake wrangler fails on any).
    const cfg = join(sb.pkg, "wrangler.jsonc");
    writeFileSync(cfg, readFileSync(cfg, "utf8").replace(/\n}\s*$/, ',\n  "routes": [{ "pattern": "backyard.example.com", "custom_domain": true }]\n}\n'));
    assert.ok(parseJSONC(readFileSync(cfg, "utf8")).routes);
    r = await sb.run(["setup", "--dry-run"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /routes in wrangler\.jsonc are ignored; use domain add/);
    assert.equal(sb.calls()[0].config.routes, undefined);
  });

  it("upgrades a version-1 state without losing the token or unknown fields", async () => {
    const sb = await newSandbox();
    seedWorker(sb, { r2Bucket: "backyard-files" });
    sb.writeState("backyard", {
      accountId: "acc1",
      deployToken: "byd_existing-token-0123456789abcdef",
      url: "https://backyard.acme.workers.dev",
      r2Bucket: "backyard-files",
      updatedAt: "2026-01-01T00:00:00Z",
      futureField: { keep: true },
    });
    const r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    const st = sb.state();
    assert.equal(st.version, 2);
    assert.equal(st.deployToken, "byd_existing-token-0123456789abcdef");
    assert.deepEqual(st.futureField, { keep: true });
    assert.equal(deployOf(sb.calls()).secretsFile, undefined);
    assert.doesNotMatch(r.out, /Adopted/);
  });

  it("takes the Worker's lock, and takes over a stale one", async () => {
    const sb = await newSandbox();
    const lockPath = join(sb.root, "config", "backyard", "cloudflare", "backyard.lock");
    const write = (o) => {
      mkdirSync(join(sb.root, "config", "backyard", "cloudflare"), { recursive: true });
      writeFileSync(lockPath, JSON.stringify(o));
    };
    // Held by a live process on this host (the test runner itself).
    write({ pid: process.pid, host: hostname(), startedAt: new Date().toISOString() });
    let r = await sb.run(["setup"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /another backyard-cloudflare run for backyard is in progress \(pid \d+ on .* since .*\); if not, delete .*backyard\.lock/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    // A dead process: stale, taken over, and released afterwards.
    const dead = spawnSync(process.execPath, ["-e", ""]).pid;
    write({ pid: dead, host: hostname(), startedAt: new Date().toISOString() });
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
    assert.ok(!existsSync(lockPath), "the lock is released");
    // Another host's lock counts until it is 2 hours old.
    write({ pid: 1, host: "elsewhere.example", startedAt: new Date().toISOString() });
    r = await sb.run(["setup"]);
    assert.equal(r.code, 1, r.out);
    write({ pid: 1, host: "elsewhere.example", startedAt: new Date(Date.now() - 3 * 3600_000).toISOString() });
    r = await sb.run(["setup"]);
    assert.equal(r.code, 0, r.out);
  });

  it("writes the result file for wrappers, without secrets", async () => {
    const sb = await newSandbox();
    const env = { BACKYARD_CF_RESULT_FILE: sb.resultPath, BACKYARD_CF_INVOKED_AS: "backyard cloudflare" };
    let r = await sb.run(["setup"], env);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(sb.result(), {
      schema: 1,
      command: "setup",
      worker: "backyard",
      statePath: sb.statePath(),
      url: "https://backyard.acme.workers.dev",
      previousUrl: "",
      workersDevUrl: "https://backyard.acme.workers.dev",
      authMode: "none",
      deployToken: "state",
    });
    if (process.platform !== "win32") assert.equal(fileMode(sb.resultPath), "600");
    // BACKYARD_CF_INVOKED_AS names the right command form in the hints.
    assert.match(r.out, /Your own domain: backyard cloudflare domain add <host>/);
    sb.allowInEnv = [secretsOf(sb).DEPLOY_TOKEN];
    r = await sb.run(["setup"], { ...env, DEPLOY_TOKEN: secretsOf(sb).DEPLOY_TOKEN });
    assert.equal(r.code, 0, r.out);
    assert.equal(sb.result().previousUrl, "https://backyard.acme.workers.dev");
    assert.equal(sb.result().deployToken, "env");
    assertNoLeaks(sb, { outputs: [r.out], allowInEnv: [secretsOf(sb).DEPLOY_TOKEN] });
    // No result file without a state save (--help, --dry-run).
    rmSync(sb.resultPath, { force: true });
    r = await sb.run(["setup", "--dry-run"], env);
    assert.equal(r.code, 0, r.out);
    assert.equal(sb.result(), null);
  });

  it("runs the same through `node scripts/setup.mjs` (setup's old entry point)", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["--help"], {}, { script: "scripts/setup.mjs" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Usage: npm run cf -- setup \[options\]/);
    assert.match(r.out, /domain add <host>/);
    assert.match(r.out, /access enable --allow-email-domain/);
    r = await sb.run(["--var", "SERVER_NAME:Direct"], {}, { script: "scripts/setup.mjs" });
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.SERVER_NAME, "Direct");
    assert.ok(!r.out.includes(OAUTH_TOKEN));
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
    // A stray "[]" line before the real list is not the list (it would make
    // setup believe the Worker has no BACKYARD_SECRET and replace it).
    assert.deepEqual(names(`[]\n${list}\n`), ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    assert.deepEqual(names(`[]\n[]\n`), []);
    assert.deepEqual(names(`${list}\n[]\n`), ["BACKYARD_SECRET", "DEPLOY_TOKEN"]);
    // An empty list next to a foreign array is not trusted either.
    assert.throws(() => parseSecretList('[]\n[{"type":"secret_text"}]'), /no JSON list/);
    assert.throws(() => parseSecretList("[]\n[1, 2]\n"), /no JSON list/);
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

  it("reads wrangler.jsonc (comments, trailing commas, strings with //)", () => {
    assert.deepEqual(parseJSONC('// c\n{ "a": "x // y", /* z */ "b": [1, 2,], }\n'), { a: "x // y", b: [1, 2] });
    // A trailing-comma lookalike inside a string is a value, not syntax.
    assert.deepEqual(parseJSONC('{ "a": "x,}", "b": "y, ]", "c": "\\",}", "d": [1, /* e */ ], }'), { a: "x,}", b: "y, ]", c: '",}', d: [1] });
  });
});
