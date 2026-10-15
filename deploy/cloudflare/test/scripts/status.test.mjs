// Offline tests of `backyard-cloudflare status` (node --test): a read-only live
// check against the fake wrangler and the fake Cloudflare API and edge
// (test/scripts/helpers). It takes no lock, writes nothing, never opens the
// browser login, and exits 0 when every check is ok, 4 when one is pending or
// failed, 1 when the Worker does not exist.
import assert from "node:assert/strict";
import { existsSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { hostname } from "node:os";
import { join } from "node:path";
import { after, afterEach, describe, it } from "node:test";

import { CHECK_NAMES } from "../../scripts/status.mjs";
import { parseJSONC } from "../../scripts/setup.mjs";
import { TOKEN_PRESETS, deployedWorker } from "./helpers/fake-cloudflare.mjs";
import { assertNoLeaks, createSandbox } from "./helpers/sandbox.mjs";

const sandboxes = [];
let fresh = [];
async function newSandbox(o) {
  const sb = await createSandbox(o);
  const run = sb.run.bind(sb);
  sb.outputs = [];
  sb.run = async (...a) => {
    const r = await run(...a);
    sb.outputs.push(r.out);
    return r;
  };
  sandboxes.push(sb);
  fresh.push(sb);
  return sb;
}
afterEach(() => {
  const check = fresh;
  fresh = [];
  for (const sb of check) assertNoLeaks(sb, { outputs: sb.outputs });
});
after(async () => {
  for (const sb of sandboxes) await sb.close();
});

const HOST = "backyard.example.com";
const TEAM = "acme.cloudflareaccess.com";
const WD = "https://backyard.acme.workers.dev";
const ADMIN = "admin-token-access-0123456789abcdef";
const TOKEN = "byd_existing-token-0123456789abcdef";
const stateDir = (sb) => join(sb.root, "config", "backyard", "cloudflare");
const checksOf = (j) => Object.fromEntries(j.checks.map((c) => [c.name, c]));

/** A Worker with a custom domain (path or host mode), as setup and domain add leave it. */
async function domainSandbox(mode = "path") {
  const sb = await newSandbox();
  const base = parseJSONC(readFileSync(join(sb.pkg, "wrangler.jsonc"), "utf8")).vars;
  sb.updateCf((s) => {
    s.zones.push({ id: "zone-example", name: "example.com", status: "active", account: "acc1" });
    s.customDomains.push({ id: "cd-hub", hostname: HOST, zone_id: "zone-example", zone_name: "example.com", service: "backyard", environment: "production" });
    if (mode === "host") {
      s.routes.push({ id: "route-1", zone_id: "zone-example", pattern: `*.${HOST}/*`, script: "backyard" });
      s.dnsRecords.push({ id: "dns-1", zone_id: "zone-example", zone_name: "example.com", type: "AAAA", name: `*.${HOST}`, content: "100::", proxied: true, ttl: 1, comment: "" });
    }
    s.workers.backyard = deployedWorker({ vars: { ...base, BASE_DOMAIN: HOST, URL_MODE: mode }, secrets: { BACKYARD_SECRET: "s".repeat(64), DEPLOY_TOKEN: TOKEN }, r2Bucket: "backyard-files" });
    s.tokens[ADMIN] = { label: "admin", permissions: [...TOKEN_PRESETS.access] };
    s.access.acc1 = { org: { auth_domain: TEAM, name: "acme" }, apps: [], policies: [] };
  });
  sb.writeState("backyard", {
    version: 2,
    accountId: "acc1",
    url: `https://${HOST}`,
    deployToken: TOKEN,
    workersDevUrl: WD,
    workersDev: true,
    r2: true,
    r2Bucket: "backyard-files",
    domain: { host: HOST, mode, zoneId: "zone-example", zoneName: "example.com", customDomainId: "cd-hub", status: "active", source: "cli" },
    updatedAt: "2026-10-01T00:00:00Z",
  });
  return sb;
}

/** domainSandbox plus Cloudflare Access turned on by `access enable` (API mode). */
async function accessSandbox() {
  const sb = await domainSandbox();
  const r = await sb.run(["access", "enable", "--allow-email-domain", "acme.com", "--wait", "0"], { BACKYARD_CF_API_TOKEN: ADMIN });
  assert.equal(r.code, 0, r.out);
  sb.calls();
  sb.requests();
  return sb;
}

describe("backyard-cloudflare status", () => {
  it("reports every check (human output and --json) without a lock, a write or a prompt", async () => {
    const sb = await accessSandbox();
    const before = readFileSync(sb.statePath(), "utf8");
    // Another run holds the lock: status takes none, so it still runs.
    const lock = join(stateDir(sb), "backyard.lock");
    writeFileSync(lock, JSON.stringify({ pid: process.pid, host: hostname(), startedAt: new Date().toISOString() }));
    let r = await sb.run(["status"]);
    assert.equal(r.code, 0, r.out);
    assert.ok(r.out.includes(`State:        ${sb.statePath()}`), r.out);
    assert.match(r.out, /Account: {6}Acme \(acc1\)/);
    assert.match(r.out, /Versions: {5}backyard-cloudflare 0\.2\.0-dev \(source checkout\) · wrangler 4\.149\.0 · deployed 0\.1\.0-dev/);
    assert.match(r.out, /Hub: {10}https:\/\/backyard\.example\.com/);
    assert.match(r.out, /R2: {11}bucket backyard-files/);
    assert.match(r.out, /AI: {11}on \(Workers AI\)/);
    assert.match(r.out, /Domain: {7}backyard\.example\.com \(path mode, active, cli\)/);
    assert.match(r.out, /Access: {7}team acme\.cloudflareaccess\.com · AUD [0-9a-f]{64} \(api\)/);
    assert.match(r.out, /Deploy token: byd_…cdef/);
    for (const name of ["worker", "workers-dev", "hub", "custom-domain", "tls", "access-redirect", "access-bypass", "access-connector", "workers-dev-closed", "deploy-token"]) {
      assert.match(r.out, new RegExp(`^  ok {7}${name} `, "m"), name);
    }
    assert.match(r.out, /All checks passed\./);

    r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 0, r.out);
    const j = JSON.parse(r.stdout);
    assert.deepEqual(Object.keys(j), ["schema", "worker", "accountId", "statePath", "versions", "urls", "r2Bucket", "workersDev", "ai", "domain", "access", "checks"]);
    assert.equal(j.schema, 1);
    assert.equal(j.worker, "backyard");
    assert.equal(j.accountId, "acc1");
    assert.equal(j.statePath, sb.statePath());
    assert.deepEqual(j.versions, { package: "0.2.0-dev (source checkout)", wrangler: "4.149.0", deployed: "0.1.0-dev" });
    assert.deepEqual(j.urls, { public: `https://${HOST}`, workersDev: WD });
    assert.equal(j.r2Bucket, "backyard-files");
    assert.equal(j.workersDev, true);
    assert.deepEqual(j.ai, { enabled: true, binding: true });
    assert.deepEqual(j.domain, JSON.parse(before).domain);
    assert.deepEqual(j.access, { team: TEAM, aud: JSON.parse(before).access.aud, source: "api" });
    assert.deepEqual(
      j.checks.map((c) => c.name),
      ["worker", "workers-dev", "hub", "custom-domain", "tls", "access-redirect", "access-bypass", "access-connector", "workers-dev-closed", "deploy-token"],
    );
    for (const c of j.checks) {
      assert.deepEqual(Object.keys(c), ["name", "status", "detail"]);
      assert.equal(c.status, "ok", `${c.name}: ${c.detail}`);
      assert.ok(CHECK_NAMES.includes(c.name));
    }
    // Read-only: no lock taken (the foreign one is untouched), no state write, no API write, no login.
    assert.equal(readFileSync(lock, "utf8").includes(String(process.pid)), true);
    assert.equal(readFileSync(sb.statePath(), "utf8"), before);
    assert.deepEqual(sb.requests().filter((x) => x.method !== "GET"), []);
    assert.ok(!sb.calls().some((c) => c.args[0] === "login" || c.args[0] === "deploy" || c.args[0] === "secret"));
    rmSync(lock);
  });

  it("exits 4 when a check fails, naming what is wrong", async () => {
    const sb = await accessSandbox();
    // The bypass application loses /__backyard/api/tokens in the dashboard.
    sb.updateCf((s) => {
      const app = s.access.acc1.apps.find((a) => a.name === "Backyard (backyard) machine endpoints");
      app.destinations = app.destinations.filter((d) => !d.uri.endsWith("/__backyard/api/tokens"));
    });
    const r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 4, r.out);
    const c = checksOf(JSON.parse(r.stdout));
    assert.equal(c["access-bypass"].status, "failed");
    assert.equal(c["access-bypass"].detail, `the bypass application is missing ${HOST}/__backyard/api/tokens`);
    assert.equal(c["access-redirect"].status, "ok");
    assert.equal(c.hub.status, "ok");
    const human = await sb.run(["status"]);
    assert.equal(human.code, 4);
    assert.match(human.out, /^ {2}failed {3}access-bypass {7}the bypass application is missing/m);
    assert.match(human.out, /1 check needs attention\./);
  });

  it("reports the Claude connector's bypass paths: pending before MCP_ENABLED, failed after", async () => {
    const sb = await accessSandbox();
    // A bypass application from before the connector: the five machine paths only.
    const connector = ["/__backyard/mcp", "/__backyard/oauth/*", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"];
    sb.updateCf((s) => {
      const app = s.access.acc1.apps.find((a) => a.name === "Backyard (backyard) machine endpoints");
      app.destinations = app.destinations.filter((d) => !connector.some((p) => d.uri.endsWith(p)));
    });
    const missing = connector.map((p) => `the bypass application is missing ${HOST}${p}`).join("; ");
    let r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 4, r.out);
    let c = checksOf(JSON.parse(r.stdout));
    assert.equal(c["access-connector"].status, "pending");
    assert.equal(c["access-connector"].detail, `the Claude connector paths are not bypassed yet: run npm run cf -- access enable (with BACKYARD_CF_API_TOKEN when it made the bypass application, or add the paths in the dashboard) before setting MCP_ENABLED (${missing})`);
    assert.equal(c["access-bypass"].status, "ok", "the machine endpoints are fine");
    const human = await sb.run(["status"]);
    assert.match(human.out, /^ {2}pending {2}access-connector {4}the Claude connector paths are not bypassed yet/m);
    assert.match(human.out, /1 check needs attention\./);
    // MCP_ENABLED is on (/info has mcp_url): now a failure.
    sb.updateCf((s) => (s.workers.backyard.vars.MCP_ENABLED = "true"));
    r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 4, r.out);
    c = checksOf(JSON.parse(r.stdout));
    assert.equal(c["access-connector"].status, "failed");
    assert.equal(c["access-connector"].detail, missing);
    // access enable puts them back: ok, with the connector on.
    r = await sb.run(["access", "enable", "--wait", "0"], { BACKYARD_CF_API_TOKEN: ADMIN });
    assert.equal(r.code, 0, r.out);
    r = await sb.run(["status", "--json"]);
    c = checksOf(JSON.parse(r.stdout));
    assert.equal(c["access-connector"].status, "ok", c["access-connector"].detail);
    assert.equal(c["access-connector"].detail, "the Claude connector's paths reach the Worker");
  });

  it("skips the Cloudflare checks when not logged in, without opening the browser (exit 4)", async () => {
    const sb = await domainSandbox();
    sb.requests();
    const r = await sb.run(["status", "--json"], { FAKE_LOGGED_OUT: "1" });
    assert.equal(r.code, 4, r.out);
    const j = JSON.parse(r.stdout);
    const c = checksOf(j);
    assert.equal(c.worker.status, "pending");
    assert.match(c.worker.detail, /^Cloudflare checks skipped: not logged in to Cloudflare \(log in with npm run cf -- setup, or set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID\)/);
    assert.equal(c["custom-domain"], undefined, "API checks are skipped");
    assert.equal(c.hub.status, "ok", "edge checks still run");
    assert.equal(c["deploy-token"].status, "ok");
    assert.deepEqual(sb.requests(), [], "no Cloudflare API request");
    const calls = sb.calls();
    assert.ok(!calls.some((x) => x.args[0] === "login"), "never the browser login");
    assert.deepEqual(calls.map((x) => x.args.slice(0, 2).join(" ")), ["whoami --json"]);
    const human = await sb.run(["status"], { FAKE_LOGGED_OUT: "1" });
    assert.equal(human.code, 4);
    assert.match(human.out, /live Cloudflare checks skipped: not logged in to Cloudflare/);
  });

  it("exits 1 when the Worker does not exist (packaged layout: nothing written next to the package)", async () => {
    const sb = await newSandbox({ layout: "packaged" });
    const before = sb.snapshot();
    const r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 1, r.out);
    const j = JSON.parse(r.stdout);
    const c = checksOf(j);
    assert.equal(c.worker.status, "failed");
    assert.equal(c.worker.detail, "Worker backyard is not deployed on account acc1: run backyard-cloudflare setup");
    assert.equal(j.versions.package, "1.2.3-test");
    assert.ok(!sb.hasState(), "status writes no state");
    assert.deepEqual(sb.snapshot(), before);
    // Every wrangler call ran in a private run directory, removed afterwards.
    const cwds = new Set(sb.calls().map((x) => x.cwd));
    assert.equal(cwds.size, 1);
    for (const d of cwds) {
      assert.match(d, /backyard-cf-/);
      assert.ok(!existsSync(d));
    }
  });

  it("checks the route, the wildcard DNS and the certificates in host mode", async () => {
    const sb = await domainSandbox("host");
    let r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 0, r.out);
    let c = checksOf(JSON.parse(r.stdout));
    assert.deepEqual(
      JSON.parse(r.stdout).checks.map((x) => x.name),
      ["worker", "workers-dev", "hub", "custom-domain", "route", "dns", "tls", "deploy-token"],
    );
    assert.equal(c.route.detail, `*.${HOST}/* → Worker backyard`);
    assert.equal(c.dns.detail, `*.${HOST} resolves`);
    assert.equal(c.tls.detail, `HTTPS works for ${HOST} and *.${HOST}`);
    // The wildcard record is gone and the route belongs to another Worker.
    sb.updateCf((s) => {
      s.dnsRecords = [];
      s.routes[0].script = "other";
    });
    r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 4, r.out);
    c = checksOf(JSON.parse(r.stdout));
    assert.equal(c.route.status, "failed");
    assert.equal(c.route.detail, `the route *.${HOST}/* belongs to Worker other, not backyard`);
    assert.equal(c.dns.status, "pending");
    assert.equal(c.tls.status, "pending");
    assert.match(c.tls.detail, /\*\.backyard\.example\.com does not resolve yet/);
    // A certificate that does not cover the host yet: pending.
    sb.updateCf((s) => (s.dnsRecords = [{ id: "dns-1", zone_id: "zone-example", zone_name: "example.com", type: "AAAA", name: `*.${HOST}`, content: "100::", proxied: true }]));
    r = await sb.run(["status", "--json"], { FAKE_TLS_FAIL: HOST });
    assert.equal(r.code, 4, r.out);
    c = checksOf(JSON.parse(r.stdout));
    assert.equal(c.tls.status, "pending");
    assert.match(c.tls.detail, /the certificate does not cover backyard\.example\.com yet; the certificate does not cover \*\.backyard\.example\.com yet/);
    assert.equal(c.hub.status, "pending");
  });

  it("reports AI off (setup --no-ai) without a failing check, from the live Worker and from the state", async () => {
    const sb = await domainSandbox();
    const redeploy = (vars, ai) =>
      sb.updateCf((s) => {
        const w = s.workers.backyard;
        s.workers.backyard = deployedWorker({ vars: { ...w.vars, ...vars }, secrets: w.secrets, r2Bucket: w.r2Bucket, ai });
      });
    redeploy({ AI_ENABLED: "false" }, false);
    sb.writeState("backyard", { ...sb.state(), ai: false });
    let r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 0, r.out);
    let j = JSON.parse(r.stdout);
    assert.deepEqual(j.ai, { enabled: false, binding: false });
    for (const c of j.checks) assert.equal(c.status, "ok", `${c.name}: ${c.detail}`);
    assert.match((await sb.run(["status"])).out, /AI: {11}off \(npm run cf -- setup --ai turns it on\)/);
    // Not logged in: from this machine's state.
    r = await sb.run(["status", "--json"], { FAKE_LOGGED_OUT: "1" });
    assert.deepEqual(JSON.parse(r.stdout).ai, { enabled: false, binding: false });
    // --var AI_ENABLED:false with AI on: the API is off, the binding stays.
    redeploy({ AI_ENABLED: "false" }, true);
    sb.writeState("backyard", { ...sb.state(), ai: undefined, vars: { AI_ENABLED: "false" } });
    j = JSON.parse((await sb.run(["status", "--json"])).stdout);
    assert.deepEqual(j.ai, { enabled: false, binding: true });
    assert.match((await sb.run(["status"])).out, /AI: {11}off \(AI_ENABLED=false; the Workers AI binding stays\)/);
    // Without the binding but with AI on (Anthropic or OpenAI keys).
    redeploy({ AI_ENABLED: "" }, false);
    j = JSON.parse((await sb.run(["status", "--json"])).stdout);
    assert.deepEqual(j.ai, { enabled: true, binding: false });
    assert.match((await sb.run(["status"])).out, /AI: {11}on, without the Workers AI binding \(only Anthropic or OpenAI aliases work\)/);
  });

  it("shows the live configuration on a machine without state, and writes none", async () => {
    const sb = await accessSandbox();
    const aud = sb.state().access.aud;
    rmSync(join(sb.root, "config"), { recursive: true, force: true });
    const r = await sb.run(["status", "--json"]);
    assert.equal(r.code, 0, r.out);
    const j = JSON.parse(r.stdout);
    assert.deepEqual(j.domain, { host: HOST, mode: "path", status: "active", source: "adopted" });
    assert.deepEqual(j.access, { team: TEAM, aud, source: "adopted" });
    assert.deepEqual(j.urls, { public: `https://${HOST}`, workersDev: WD });
    assert.equal(checksOf(j)["deploy-token"], undefined, "the token is not known here");
    assert.ok(!existsSync(join(sb.root, "config", "backyard", "cloudflare", "backyard.json")));
    const human = await sb.run(["status"]);
    assert.match(human.out, /none on this machine: showing the live configuration/);
    assert.match(human.out, /Deploy token: not known on this machine/);
  });
});
