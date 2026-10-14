// Offline tests of `backyard-cloudflare access enable` and `access disable`
// (node --test): the engine runs in a sandbox against the fake wrangler and the
// fake Cloudflare API and edge (test/scripts/helpers), which simulate the Zero
// Trust organization, Access applications and policies, the login redirect
// (with kid) and the Worker behind them. Every test also checks that no secret
// (deploy token, BACKYARD_SECRET, OAuth or admin token) leaks into a wrangler
// argv or env, the state, the result file or the output.
import assert from "node:assert/strict";
import { readFileSync, rmSync } from "node:fs";
import { join } from "node:path";
import { after, afterEach, describe, it } from "node:test";

import { appCovers, changedFields, normalizeAllow, parseWho } from "../../scripts/access.mjs";
import { ACCESS_BYPASS_PATHS, accessNames, appBodies, describeAllow, guidedSteps, isConsumerMailDomain, policyBodies } from "../../scripts/lib/access-plan.mjs";
import { parseJSONC } from "../../scripts/setup.mjs";
import { TOKEN_PRESETS, deployedWorker } from "./helpers/fake-cloudflare.mjs";
import { assertNoLeaks, createSandbox, ttyAvailable } from "./helpers/sandbox.mjs";

const sandboxes = [];
let fresh = [];
/** A sandbox whose every run's output is kept for the secret checks after each test. */
async function newSandbox(o) {
  const sb = await createSandbox(o);
  const run = sb.run.bind(sb);
  sb.outputs = [];
  sb.allowInEnv = [];
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
  for (const sb of check) assertNoLeaks(sb, { outputs: sb.outputs, allowInEnv: sb.allowInEnv });
});
after(async () => {
  for (const sb of sandboxes) await sb.close();
});

const HOST = "backyard.example.com";
const TEAM = "acme.cloudflareaccess.com";
const WD = "https://backyard.acme.workers.dev";
const ADMIN = "admin-token-access-0123456789abcdef";
const DNS_ONLY = "admin-token-dns-only-0123456789abcd";
const CI_TOKEN = "ci-workers-token-0123456789abcdef";
const TOKEN = "byd_existing-token-0123456789abcdef";
const DASH_AUD = "d".repeat(63) + "1";
const N = accessNames("backyard");
const ENABLE = ["access", "enable", "--allow-email-domain", "acme.com", "--wait", "0"];
const WITH_ADMIN = { BACKYARD_CF_API_TOKEN: ADMIN };

/** The paths the Claude connector probes and what a Worker answers there with MCP_ENABLED off (SPEC §8.4). */
const CONNECTOR_OFF = {
  "/__backyard/mcp": 404,
  "/__backyard/oauth/token": 404,
  "/.well-known/oauth-authorization-server": 404,
  "/.well-known/oauth-protected-resource/__backyard/mcp": 404,
};
const CONNECTOR_RULES = ["/__backyard/mcp", "/__backyard/oauth/*", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"];

const deployOf = (calls) => calls.find((c) => c.args[0] === "deploy");
const writesOf = (reqs) => reqs.filter((x) => x.method !== "GET");
const accessReqs = (reqs) => reqs.filter((x) => x.path.includes("/access/"));
const liveVars = (sb) => sb.cf().workers.backyard.vars;
const forgetState = (sb) => rmSync(join(sb.root, "config"), { recursive: true, force: true });

/**
 * A Worker that `setup` and `domain add` left behind: live on workers.dev and
 * on the custom domain HOST (path or host mode), Zero Trust set up on the
 * account (team acme), and this machine's state.
 */
async function accessSandbox(o = {}) {
  const sb = await newSandbox();
  const mode = o.mode ?? "path";
  const base = parseJSONC(readFileSync(join(sb.pkg, "wrangler.jsonc"), "utf8")).vars;
  sb.updateCf((s) => {
    s.zones.push({ id: "zone-example", name: "example.com", status: "active", account: "acc1" });
    s.customDomains.push({ id: "cd-hub", hostname: HOST, zone_id: "zone-example", zone_name: "example.com", service: "backyard", environment: "production" });
    if (mode === "host") {
      s.routes.push({ id: "route-1", zone_id: "zone-example", pattern: `*.${HOST}/*`, script: "backyard" });
      s.dnsRecords.push({ id: "dns-1", zone_id: "zone-example", zone_name: "example.com", type: "AAAA", name: `*.${HOST}`, content: "100::", proxied: true, ttl: 1, comment: "" });
    }
    s.workers.backyard = deployedWorker({ vars: { ...base, BASE_DOMAIN: HOST, URL_MODE: mode }, secrets: { BACKYARD_SECRET: "s".repeat(64), DEPLOY_TOKEN: TOKEN } });
    s.tokens[ADMIN] = { label: "admin", permissions: [...TOKEN_PRESETS.access] };
    s.tokens[DNS_ONLY] = { label: "dns-only", permissions: [...TOKEN_PRESETS.dns] };
    s.tokens[CI_TOKEN] = { label: "ci", permissions: [...TOKEN_PRESETS.workers] };
    s.access.acc1 = { org: o.noOrg ? null : { auth_domain: TEAM, name: "acme" }, apps: [], policies: [] };
  });
  sb.writeState("backyard", {
    version: 2,
    accountId: "acc1",
    url: `https://${HOST}`,
    deployToken: TOKEN,
    workersDevUrl: WD,
    workersDev: true,
    r2: false,
    domain: { host: HOST, mode, zoneId: "zone-example", zoneName: "example.com", customDomainId: "cd-hub", status: "active", source: "cli" },
    updatedAt: "2026-10-01T00:00:00Z",
    ...(o.state ?? {}),
  });
  return sb;
}

/** Access applications someone made in the dashboard (other names, not this tool's). */
function seedDashboardApps(sb, o = {}) {
  sb.updateCf((s) => {
    const ax = s.access.acc1;
    ax.policies.push(
      { id: "pol-allow", name: "Staff", decision: "allow", include: [{ email_domain: { domain: "acme.com" } }] },
      { id: "pol-bypass", name: "Machines", decision: "bypass", include: [{ everyone: {} }] },
    );
    ax.apps.push({
      id: "app-bypass",
      name: "Backyard machines",
      type: "self_hosted",
      domain: `${HOST}/__backyard/api/deploy`,
      destinations: ACCESS_BYPASS_PATHS.filter((p) => !(o.omit ?? []).includes(p)).map((p) => ({ type: "public", uri: HOST + p })),
      policies: [{ id: "pol-bypass", precedence: 1 }],
      aud: "b".repeat(64),
    });
    ax.apps.push({ id: "app-main", name: "Backyard hub", type: "self_hosted", domain: HOST, destinations: [{ type: "public", uri: HOST }], policies: [{ id: "pol-allow", precedence: 1 }], aud: DASH_AUD });
  });
}

describe("backyard-cloudflare access enable (API mode)", () => {
  it("creates the policies and the applications in order, verifies the edge, then turns Access on", async () => {
    const sb = await accessSandbox();
    const r = await sb.run(["access", "enable", "--allow-email-domain", "@Acme.com", "--allow-email", "A@acme.com", "--wait", "0"], { ...WITH_ADMIN, BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 0, r.out);
    const reqs = sb.requests();
    const writes = writesOf(reqs);
    assert.deepEqual(
      writes.map((x) => `${x.method} ${x.path} ${x.token}`),
      [
        "POST /accounts/acc1/access/policies admin",
        "POST /accounts/acc1/access/policies admin",
        "POST /accounts/acc1/access/apps admin",
        "POST /accounts/acc1/access/apps admin",
      ],
    );
    const orgAt = reqs.findIndex((x) => x.path === "/accounts/acc1/access/organizations");
    assert.ok(orgAt >= 0 && orgAt < reqs.indexOf(writes[0]), "the organization is read first");
    // The admin token is used for Access only; everything else uses wrangler's own login.
    assert.ok(accessReqs(reqs).every((x) => x.token === "admin"));
    assert.ok(reqs.filter((x) => !x.path.includes("/access/")).every((x) => x.token === "oauth"));
    const ax = sb.cf().access.acc1;
    const allowP = ax.policies.find((p) => p.name === N.allowPolicy);
    const bypassP = ax.policies.find((p) => p.name === N.bypassPolicy);
    const main = ax.apps.find((a) => a.name === N.app);
    const bypassApp = ax.apps.find((a) => a.name === N.bypassApp);
    assert.deepEqual(writes[0].body, {
      name: "Backyard (backyard) users",
      decision: "allow",
      include: [{ email: { email: "a@acme.com" } }, { email_domain: { domain: "acme.com" } }],
      session_duration: "24h",
    });
    assert.deepEqual(writes[1].body, { name: "Backyard (backyard) machine endpoints", decision: "bypass", include: [{ everyone: {} }] });
    assert.deepEqual(writes[2].body, {
      name: "Backyard (backyard) machine endpoints",
      type: "self_hosted",
      domain: `${HOST}/__backyard/api/deploy`,
      destinations: ACCESS_BYPASS_PATHS.map((p) => ({ type: "public", uri: `${HOST}${p}` })),
      app_launcher_visible: false,
      policies: [{ id: bypassP.id, precedence: 1 }],
    });
    assert.deepEqual(writes[3].body, {
      name: "Backyard (backyard)",
      type: "self_hosted",
      domain: HOST,
      destinations: [{ type: "public", uri: HOST }],
      session_duration: "24h",
      app_launcher_visible: true,
      http_only_cookie_attribute: true,
      same_site_cookie_attribute: "lax",
      policies: [{ id: allowP.id, precedence: 1 }],
    });
    // Deployed with the team and the AUD from the API; no secret was rewritten.
    const d = deployOf(sb.calls());
    assert.equal(d.config.vars.AUTH_MODE, "access");
    assert.equal(d.config.vars.ACCESS_TEAM_DOMAIN, TEAM);
    assert.equal(d.config.vars.ACCESS_AUD, main.aud);
    assert.equal(d.config.vars.BASE_DOMAIN, HOST);
    assert.equal(d.config.vars.URL_MODE, "path");
    assert.equal(d.secretsFile, undefined);
    assert.deepEqual(sb.state().access, {
      team: TEAM,
      aud: main.aud,
      source: "api",
      appId: main.id,
      bypassAppId: bypassApp.id,
      allowPolicyId: allowP.id,
      bypassPolicyId: bypassP.id,
      allow: { emails: ["a@acme.com"], emailDomains: ["acme.com"] },
    });
    assert.equal(sb.state().accessCreated, undefined);
    if (process.platform !== "win32") assert.equal(sb.stateMode(), "600");
    // The edge was checked (the machine paths and the Claude connector's four), and the post-checks ran.
    const edge = sb.fake.edgeRequests;
    for (const p of ["/__backyard/healthz", "/__backyard/readyz", "/__backyard/api/deploy/info", "/__backyard/api/deploy/me", "/__backyard/api/tokens"]) {
      assert.ok(edge.some((x) => x.host === HOST && x.path === p), p);
    }
    for (const [p, status] of Object.entries(CONNECTOR_OFF)) assert.ok(edge.some((x) => x.host === HOST && x.path === p && x.status === status), p);
    assert.match(r.out, /Cloudflare Access verified: login redirect to acme\.cloudflareaccess\.com, AUD [0-9a-f]{64}, 9 machine paths reach the Worker/);
    assert.ok(edge.some((x) => x.host === TEAM && x.path === "/cdn-cgi/access/certs" && x.status === 200));
    assert.ok(edge.some((x) => x.host === "backyard.acme.workers.dev" && x.path === "/" && x.status === 401), "workers.dev is closed");
    assert.ok(edge.some((x) => x.host === HOST && x.path === "/__backyard/api/deploy/me" && x.status === 200), "the deploy token still works");
    assert.match(r.out, /Company login is on\. Teammates: backyard login --server https:\/\/backyard\.example\.com/);
    assert.match(r.out, /Make someone admin: npm run cf -- setup --var ADMINS:<email>/);
    assert.match(r.out, /Optional: npm run cf -- setup --no-workers-dev/);
    assert.deepEqual(sb.result(), {
      schema: 1,
      command: "access enable",
      worker: "backyard",
      statePath: sb.statePath(),
      url: `https://${HOST}`,
      previousUrl: `https://${HOST}`,
      workersDevUrl: WD,
      authMode: "access",
      deployToken: "state",
    });
  });

  it("re-runs without writes, PUTs the policy once for new allow flags, and recreates a deleted application", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const first = sb.state().access;
    sb.requests();
    sb.calls();
    // Same flags: no Access write, a redeploy and a re-verification.
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(deployOf(sb.calls()));
    assert.deepEqual(sb.state().access, first);
    // A new allow list: exactly one policy PUT.
    r = await sb.run(["access", "enable", "--allow-email", "b@acme.com", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const w = writesOf(sb.requests());
    assert.deepEqual(w.map((x) => `${x.method} ${x.path}`), [`PUT /accounts/acc1/access/policies/${first.allowPolicyId}`]);
    assert.deepEqual(w[0].body.include, [{ email: { email: "b@acme.com" } }]);
    assert.deepEqual(sb.state().access.allow, { emails: ["b@acme.com"], emailDomains: [] });
    // The hub application deleted in the dashboard: created again (a new AUD), nothing else written.
    sb.updateCf((s) => (s.access.acc1.apps = s.access.acc1.apps.filter((a) => a.id !== first.appId)));
    sb.calls();
    r = await sb.run(["access", "enable", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()).map((x) => `${x.method} ${x.path}`), ["POST /accounts/acc1/access/apps"]);
    assert.match(r.out, /Access application "Backyard \(backyard\)" \(.*\) is gone: creating it again/);
    const now = sb.state().access;
    assert.notEqual(now.appId, first.appId);
    assert.notEqual(now.aud, first.aud);
    assert.equal(deployOf(sb.calls()).config.vars.ACCESS_AUD, now.aud);
    assert.match(r.out, /the Access AUD changes from/);
    assert.equal(sb.cf().access.acc1.policies.length, 2);
    assert.equal(sb.cf().access.acc1.apps.length, 2);
  });

  it("adds the Claude connector's paths to a bypass application of an earlier version with one PUT", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const ids = sb.state().access;
    // What an earlier version created: the five machine paths only.
    const old = ACCESS_BYPASS_PATHS.filter((p) => !CONNECTOR_RULES.includes(p));
    assert.equal(old.length, 5);
    sb.updateCf((s) => (s.access.acc1.apps.find((a) => a.id === ids.bypassAppId).destinations = old.map((p) => ({ type: "public", uri: HOST + p }))));
    sb.requests();
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const w = writesOf(sb.requests());
    assert.deepEqual(w.map((x) => `${x.method} ${x.path}`), [`PUT /accounts/acc1/access/apps/${ids.bypassAppId}`]);
    assert.deepEqual(w[0].body.destinations.map((d) => d.uri), ACCESS_BYPASS_PATHS.map((p) => HOST + p));
    assert.equal(w[0].body.domain, `${HOST}/__backyard/api/deploy`, "element 0 stays the domain");
    assert.match(r.out, /Access application "Backyard \(backyard\) machine endpoints" updated \(destinations\)/);
    assert.deepEqual(sb.state().access, ids, "the same objects, the same AUD");
    // With the connector on, its paths answer through the bypass application too.
    sb.updateCf((s) => (s.workers.backyard.vars.MCP_ENABLED = "true"));
    sb.fake.edgeRequests.length = 0;
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const on = { "/__backyard/mcp": 401, "/__backyard/oauth/token": 405, "/.well-known/oauth-authorization-server": 200, "/.well-known/oauth-protected-resource/__backyard/mcp": 200 };
    for (const [p, status] of Object.entries(on)) assert.ok(sb.fake.edgeRequests.some((x) => x.host === HOST && x.path === p && x.status === status), p);
  });

  it("keeps what an admin set in the dashboard when it updates a policy or an application", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const ids = sb.state().access;
    const require = [{ login_method: { id: "okta" } }];
    const exclude = [{ email: { email: "contractor@acme.com" } }];
    // Hardening in the dashboard: MFA-like rules on our policy, one IdP on the hub app, and a Deny policy on top.
    sb.updateCf((s) => {
      const ax = s.access.acc1;
      Object.assign(ax.policies.find((p) => p.id === ids.allowPolicyId), { require, exclude });
      ax.policies.push({ id: "pol-deny", name: "No contractors", decision: "deny", include: [{ email_domain: { domain: "contractors.acme.com" } }], reusable: true });
      const app = ax.apps.find((a) => a.id === ids.appId);
      Object.assign(app, { allowed_idps: ["okta"], auto_redirect_to_identity: true });
      app.policies = [{ id: "pol-deny", precedence: 1 }, { id: ids.allowPolicyId, precedence: 2 }];
    });
    sb.requests();
    // A plain re-run: nothing to write (the Deny policy does not make the app look changed).
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()), []);
    // A managed field changed in the dashboard and a new allow list: both PUTs keep the rest.
    sb.updateCf((s) => (s.access.acc1.apps.find((a) => a.id === ids.appId).session_duration = "8h"));
    r = await sb.run(["access", "enable", "--allow-email", "new@acme.com", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const w = writesOf(sb.requests());
    assert.deepEqual(w.map((x) => `${x.method} ${x.path}`), [`PUT /accounts/acc1/access/policies/${ids.allowPolicyId}`, `PUT /accounts/acc1/access/apps/${ids.appId}`]);
    assert.deepEqual(w[0].body.include, [{ email: { email: "new@acme.com" } }]);
    assert.deepEqual(w[0].body.require, require);
    assert.deepEqual(w[0].body.exclude, exclude);
    for (const k of ["id", "created_at", "updated_at", "reusable"]) assert.ok(!(k in w[0].body), `${k} is not sent back`);
    assert.equal(w[1].body.session_duration, "24h");
    assert.deepEqual(w[1].body.allowed_idps, ["okta"]);
    assert.equal(w[1].body.auto_redirect_to_identity, true);
    assert.deepEqual(w[1].body.policies, [
      { id: "pol-deny", precedence: 1 },
      { id: ids.allowPolicyId, precedence: 2 },
    ]);
    for (const k of ["id", "uid", "aud", "created_at", "updated_at"]) assert.ok(!(k in w[1].body), `${k} is not sent back`);
    assert.match(r.out, /Access application "Backyard \(backyard\)" updated \(session_duration\)/);
    const ax = sb.cf().access.acc1;
    const policy = ax.policies.find((p) => p.id === ids.allowPolicyId);
    assert.deepEqual([policy.require, policy.exclude], [require, exclude]);
    const app = ax.apps.find((a) => a.id === ids.appId);
    assert.deepEqual(app.allowed_idps, ["okta"]);
    assert.deepEqual(app.policies.map((p) => p.id), ["pol-deny", ids.allowPolicyId]);
    assert.equal(app.aud, ids.aud, "the AUD stays");
  });

  it("attaches its allow policy again without dropping the policies an admin added", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const ids = sb.state().access;
    sb.updateCf((s) => {
      const ax = s.access.acc1;
      ax.policies.push({ id: "pol-deny", name: "No contractors", decision: "deny", include: [{ everyone: {} }], reusable: true });
      // Our policy detached in the dashboard, a Deny policy attached instead.
      ax.apps.find((a) => a.id === ids.appId).policies = [{ id: "pol-deny", precedence: 1 }];
    });
    sb.requests();
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const w = writesOf(sb.requests());
    assert.deepEqual(w.map((x) => `${x.method} ${x.path}`), [`PUT /accounts/acc1/access/apps/${ids.appId}`]);
    assert.deepEqual(w[0].body.policies, [
      { id: "pol-deny", precedence: 1 },
      { id: ids.allowPolicyId, precedence: 2 },
    ]);
  });

  it("refuses new allow flags without an API token instead of reporting a change it did not make", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const before = sb.state().access;
    sb.requests();
    sb.calls();
    // Narrowing the list from a shell without BACKYARD_CF_API_TOKEN (wrangler's OAuth login cannot do it).
    r = await sb.run(["access", "enable", "--allow-email", "b@acme.com", "--wait", "0"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /changing who may sign in needs an API token with Account · Access: Apps and Policies · Edit/);
    assert.match(r.out, /set BACKYARD_CF_API_TOKEN \(it is never stored\) and re-run, or edit the Allow policy in the dashboard \(Zero Trust → Access → Policies → "Backyard \(backyard\) users"\)/);
    assert.match(r.out, /Nothing was changed/);
    assert.doesNotMatch(r.out, /Company login is on/);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(!deployOf(sb.calls()), "no deploy");
    assert.deepEqual(sb.state().access, before, "the remembered list is unchanged");
    assert.deepEqual(sb.cf().access.acc1.policies.find((p) => p.id === before.allowPolicyId).include, [{ email_domain: { domain: "acme.com" } }]);
    // The same list as before needs no token: it only verifies again.
    r = await sb.run(ENABLE);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Company login is on/);
    assert.deepEqual(sb.state().access, before);
  });

  it("saves every new id at once: a failed hub application POST resumes without duplicates", async () => {
    const sb = await accessSandbox();
    // Fail the second application POST (the hub application) once, ambiguously (500), without applying it.
    sb.fake.addFault({ method: "POST", path: "/accounts/acc1/access/apps", match: (req) => req.body?.name === N.app, status: 500, code: 10013 });
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /Already applied: Access policy "Backyard \(backyard\) users" created; Access policy "Backyard \(backyard\) machine endpoints" created; Access application "Backyard \(backyard\) machine endpoints" created/);
    assert.ok(!deployOf(sb.calls()), "the Worker is not changed");
    assert.equal(liveVars(sb).AUTH_MODE, "none");
    const st = sb.state();
    assert.equal(st.access, undefined, "Access is not on before the edge checks pass");
    assert.deepEqual(Object.keys(st.accessCreated).sort(), ["allowPolicyId", "bypassAppId", "bypassPolicyId", "host"]);
    sb.requests();
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()).map((x) => `${x.method} ${x.path}`), ["POST /accounts/acc1/access/apps"]);
    const ax = sb.cf().access.acc1;
    assert.equal(ax.policies.length, 2, "no duplicate policy");
    assert.deepEqual(ax.apps.map((a) => a.name).sort(), [N.app, N.bypassApp]);
    assert.equal(sb.state().accessCreated, undefined);
    assert.equal(sb.state().access.bypassAppId, st.accessCreated.bypassAppId);
  });

  it("recovers from a POST that Cloudflare carried out but whose answer was lost (no duplicates)", async () => {
    const sb = await accessSandbox();
    // The allow policy and the hub application are created, but the API answers 502 (ambiguous).
    sb.fake.addFault({ method: "POST", path: "/accounts/acc1/access/policies", match: (req) => req.body?.name === N.allowPolicy, status: 502, code: 10013, apply: true });
    sb.fake.addFault({ method: "POST", path: "/accounts/acc1/access/apps", match: (req) => req.body?.name === N.app, status: 502, code: 10013, apply: true });
    const r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const posts = writesOf(sb.requests()).filter((x) => x.method === "POST").map((x) => `${x.path} ${x.body?.name} ${x.status}`);
    assert.deepEqual(posts, [
      `/accounts/acc1/access/policies ${N.allowPolicy} 502`,
      `/accounts/acc1/access/policies ${N.bypassPolicy} 200`,
      `/accounts/acc1/access/apps ${N.bypassApp} 200`,
      `/accounts/acc1/access/apps ${N.app} 502`,
    ]);
    const ax = sb.cf().access.acc1;
    assert.deepEqual(ax.policies.map((p) => p.name).sort(), [N.bypassPolicy, N.allowPolicy].sort(), "the lost POSTs were found, not repeated");
    assert.deepEqual(ax.apps.map((a) => a.name).sort(), [N.app, N.bypassApp].sort());
    const st = sb.state().access;
    assert.equal(st.allowPolicyId, ax.policies.find((p) => p.name === N.allowPolicy).id);
    assert.equal(st.appId, ax.apps.find((a) => a.name === N.app).id);
    assert.equal(deployOf(sb.calls()).config.vars.ACCESS_AUD, st.aud);
  });

  it("adopts its objects by name on a second machine, without duplicates", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const first = sb.state().access;
    forgetState(sb);
    sb.requests();
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Adopted the deployed configuration of backyard/);
    assert.match(r.out, /Found the Access applications of backyard \(made on another machine\): reusing them/);
    assert.deepEqual(writesOf(sb.requests()), []);
    const st = sb.state().access;
    for (const k of ["team", "aud", "appId", "bypassAppId", "allowPolicyId", "bypassPolicyId"]) assert.equal(st[k], first[k], k);
    assert.equal(st.source, "api");
    assert.equal(sb.cf().access.acc1.apps.length, 2);
  });

  it("refuses an application of anyone else that covers the host, showing its AUD", async () => {
    const sb = await accessSandbox();
    const tf = "f".repeat(64);
    sb.updateCf((s) => s.access.acc1.apps.push({ id: "tf-app", name: "Backyard", type: "self_hosted", domain: HOST, destinations: [{ type: "public", uri: HOST }], policies: [], aud: tf }));
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 3, r.out);
    assert.ok(
      r.out.includes(`Access application "Backyard" (aud ${tf}) already covers ${HOST}. Use it with \`npm run cf -- access enable --team ${TEAM} --aud ${tf}\`, or delete it`),
      r.out,
    );
    assert.match(r.out, /Nothing was changed/);
    // A wildcard above the host covers it too.
    sb.updateCf((s) => (s.access.acc1.apps = [{ id: "wild", name: "Everything", type: "self_hosted", domain: "*.example.com", policies: [], aud: "e".repeat(64) }]));
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /Access application "Everything" \(aud e{64}\) already covers backyard\.example\.com/);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(!deployOf(sb.calls()));
    assert.equal(sb.state().access, undefined);
  });

  it("does not count an App Launcher bookmark of the host as an application covering it", async () => {
    const sb = await accessSandbox();
    sb.updateCf((s) => s.access.acc1.apps.push({ id: "tile", name: "Backyard", type: "bookmark", domain: `https://${HOST}`, app_launcher_visible: true, aud: "a".repeat(64) }));
    const r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.doesNotMatch(r.out, /already covers/);
    assert.equal(sb.cf().access.acc1.apps.length, 3, "the bookmark stays");
    assert.notEqual(sb.state().access.aud, "a".repeat(64));
  });

  it("needs Zero Trust on the account and a token with the Access permissions", async () => {
    const sb = await accessSandbox({ noOrg: true });
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /Zero Trust is not set up on this account: open https:\/\/one\.dash\.cloudflare\.com\/ once, pick a team name and the Free plan, then re-run/);
    sb.updateCf((s) => (s.access.acc1.org = { auth_domain: TEAM, name: "acme" }));
    // A dedicated token without the Access permissions: the permission message.
    r = await sb.run(ENABLE, { BACKYARD_CF_API_TOKEN: DNS_ONLY });
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /cannot manage Cloudflare Access: an API token needs Account · Access: Apps and Policies · Edit and Account · Access: Organizations, Identity Providers, and Groups · Read/);
    // wrangler's own CLOUDFLARE_API_TOKEN without them: the same message, then the dashboard steps.
    sb.allowInEnv = [CI_TOKEN];
    r = await sb.run(ENABLE, { CLOUDFLARE_API_TOKEN: CI_TOKEN, CLOUDFLARE_ACCOUNT_ID: "acc1" });
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /\(CLOUDFLARE_API_TOKEN\) cannot manage Cloudflare Access: an API token needs Account · Access: Apps and Policies · Edit/);
    assert.match(r.out, /Cloudflare Access needs two self-hosted applications/);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(!deployOf(sb.calls()));
  });

  it("never reuses the ids it recorded for another host", async () => {
    const old = { host: "old.example.com", appId: "old-app", bypassAppId: "old-bypass", allowPolicyId: "old-allow", bypassPolicyId: "old-bypass-policy" };
    const sb = await accessSandbox({ state: { accessCreated: old } });
    const r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const reqs = sb.requests();
    assert.ok(!reqs.some((x) => /\/old-/.test(x.path)), "the other host's objects are not touched");
    assert.equal(writesOf(reqs).filter((x) => x.method === "POST").length, 4);
    assert.match(r.out, /The Access objects this tool created for old\.example\.com \(Backyard \(backyard\), Backyard \(backyard\) machine endpoints\) are no longer tracked/);
    assert.equal(sb.state().accessCreated, undefined);
    assert.equal(sb.state().access.source, "api");
  });

  it("adds *.<host> to the hub application in host mode and checks it at a random label", async () => {
    const sb = await accessSandbox({ mode: "host" });
    const r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const main = writesOf(sb.requests()).find((x) => x.method === "POST" && x.body?.name === N.app);
    assert.deepEqual(main.body.destinations, [
      { type: "public", uri: HOST },
      { type: "public", uri: `*.${HOST}` },
    ]);
    assert.ok(sb.fake.edgeRequests.some((x) => /^byd-probe-[0-9a-f]{8}\.backyard\.example\.com$/.test(x.host) && x.path === "/" && x.status === 302));
    assert.equal(deployOf(sb.calls()).config.vars.URL_MODE, "host");
  });
});

describe("backyard-cloudflare access enable (guided and --team/--aud)", () => {
  it("prints the dashboard steps without a token (non-interactive: exit 2, nothing changed)", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /Cloudflare Access needs two self-hosted applications\n\(https:\/\/one\.dash\.cloudflare\.com\/ → Access → Applications → Add an application → Self-hosted\):/);
    assert.ok(r.out.includes(`  1. Backyard (backyard)\n     Public hostname:   ${HOST}\n     Policy:            Allow · Include · Emails ending in @acme.com\n`), r.out);
    assert.ok(r.out.includes("  2. Backyard (backyard) machine endpoints   (paths the Worker authenticates itself)\n"), r.out);
    for (const p of ACCESS_BYPASS_PATHS) assert.ok(r.out.includes(`${HOST}${p}\n`), p);
    assert.ok(r.out.includes("     Policy:            Bypass · Include · Everyone\n"), r.out);
    assert.match(r.out, /BACKYARD_CF_API_TOKEN=<token> npm run cf -- access enable --allow-email-domain acme\.com/);
    assert.match(r.out, /create the two applications above in the dashboard, then re-run on an interactive terminal \(it detects them\) or with --team <team> --aud <aud>/);
    assert.deepEqual(accessReqs(sb.requests()), [], "no Access API call without a token");
    assert.ok(!deployOf(sb.calls()));
    assert.ok(!sb.hasState() || sb.state().access === undefined);
    // Once the applications exist, the hint names their team and AUD.
    seedDashboardApps(sb);
    r = await sb.run(["access", "enable", "--wait", "0"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /Policy: {12}Allow · Include · <who may sign in>/);
    assert.ok(r.out.includes(`Found Cloudflare Access in front of https://${HOST}: team ${TEAM}, application AUD ${DASH_AUD}. To use it: npm run cf -- access enable --team ${TEAM} --aud ${DASH_AUD}`), r.out);
  });

  it("verifies applications made in the dashboard with --team/--aud (source manual), and re-verifies them later", async () => {
    const sb = await accessSandbox();
    seedDashboardApps(sb);
    let r = await sb.run(["access", "enable", "--team", "acme", "--aud", DASH_AUD.toUpperCase(), "--wait", "0"], { BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 0, r.out);
    const d = deployOf(sb.calls());
    assert.equal(d.config.vars.AUTH_MODE, "access");
    assert.equal(d.config.vars.ACCESS_TEAM_DOMAIN, TEAM);
    assert.equal(d.config.vars.ACCESS_AUD, DASH_AUD);
    assert.deepEqual(sb.state().access, { team: TEAM, aud: DASH_AUD, source: "manual" });
    assert.deepEqual(accessReqs(sb.requests()), []);
    assert.equal(sb.result().authMode, "access");
    // No flags, no token: the remembered team and AUD are verified again.
    r = await sb.run(["access", "enable", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Using team acme\.cloudflareaccess\.com and AUD d+1 \(remembered/);
    assert.deepEqual(sb.state().access, { team: TEAM, aud: DASH_AUD, source: "manual" });
    // Even with an admin token, applications made in the dashboard are not taken over.
    sb.requests();
    r = await sb.run(["access", "enable", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(accessReqs(sb.requests()), []);
    assert.equal(sb.state().access.source, "manual");
  });

  it("exits 4 and leaves the Worker unchanged when the edge does not show the expected Access", async () => {
    const sb = await accessSandbox();
    seedDashboardApps(sb, { omit: ["/__backyard/api/tokens"] });
    const run = (aud = DASH_AUD) => sb.run(["access", "enable", "--team", TEAM, "--aud", aud, "--wait", "0"]);
    const unchanged = () => {
      assert.ok(!deployOf(sb.calls()), "no deploy");
      assert.equal(liveVars(sb).AUTH_MODE, "none");
      assert.equal(sb.cf().workers.backyard.deployments, 1);
      assert.equal(sb.state().access, undefined);
    };
    // A machine path the bypass application does not cover.
    let r = await run();
    assert.equal(r.code, 4, r.out);
    assert.ok(r.out.includes(`the bypass application is missing ${HOST}/__backyard/api/tokens`), r.out);
    assert.match(r.out, /so the Worker was not changed/);
    unchanged();
    sb.updateCf((s) => s.access.acc1.apps.find((a) => a.id === "app-bypass").destinations.push({ type: "public", uri: `${HOST}/__backyard/api/tokens` }));
    // Another application's AUD in the login redirect (kid).
    r = await run("c".repeat(64));
    assert.equal(r.code, 4, r.out);
    assert.ok(r.out.includes(`Access in front of ${HOST} is the application with AUD ${DASH_AUD}, not ${"c".repeat(64)}`), r.out);
    unchanged();
    // Another team's login.
    sb.updateCf((s) => (s.edge = { redirectTeam: "evil.cloudflareaccess.com" }));
    r = await run();
    assert.equal(r.code, 4, r.out);
    assert.ok(r.out.includes(`Access in front of ${HOST} belongs to team evil.cloudflareaccess.com, not ${TEAM}`), r.out);
    unchanged();
    // A team without signing keys.
    sb.updateCf((s) => (s.edge = { certsEmpty: true }));
    r = await run();
    assert.equal(r.code, 4, r.out);
    assert.ok(r.out.includes(`the Access team ${TEAM} has no signing keys yet`), r.out);
    unchanged();
    // Everything in place: it works.
    sb.updateCf((s) => (s.edge = {}));
    r = await run();
    assert.equal(r.code, 0, r.out);
    // Usage errors: --team without --aud, a malformed AUD.
    r = await sb.run(["access", "enable", "--team", TEAM]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /--team and --aud go together/);
    r = await sb.run(["access", "enable", "--team", TEAM, "--aud", "not-an-aud"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /--aud expects the 64-character Application Audience \(AUD\) tag/);
  });

  it("fails the edge check for each Claude connector path the bypass application lacks, before changing the Worker", async () => {
    const sb = await accessSandbox();
    // An application made in the dashboard from an earlier version's steps.
    seedDashboardApps(sb, { omit: CONNECTOR_RULES });
    let r = await sb.run(["access", "enable", "--team", TEAM, "--aud", DASH_AUD, "--wait", "0"]);
    assert.equal(r.code, 4, r.out);
    for (const p of CONNECTOR_RULES) assert.ok(r.out.includes(`  - the bypass application is missing ${HOST}${p}\n`), `${p}\n${r.out}`);
    assert.ok(!r.out.includes(`missing ${HOST}/__backyard/healthz`), r.out);
    assert.match(r.out, /so the Worker was not changed/);
    assert.ok(!deployOf(sb.calls()), "no deploy");
    assert.equal(liveVars(sb).AUTH_MODE, "none");
    // Only /__backyard/oauth/* missing: named alone.
    sb.updateCf((s) => {
      const app = s.access.acc1.apps.find((a) => a.id === "app-bypass");
      for (const p of CONNECTOR_RULES.filter((x) => x !== "/__backyard/oauth/*")) app.destinations.push({ type: "public", uri: HOST + p });
    });
    r = await sb.run(["access", "enable", "--team", TEAM, "--aud", DASH_AUD, "--wait", "0"]);
    assert.equal(r.code, 4, r.out);
    assert.ok(r.out.includes(`the bypass application is missing ${HOST}/__backyard/oauth/*`), r.out);
    assert.ok(!r.out.includes(`missing ${HOST}/__backyard/mcp`), r.out);
    // All nine in place, and a Worker from before the connector (its hub page at the
    // discovery documents, JSON 404s below /__backyard/): it passes.
    sb.updateCf((s) => {
      s.access.acc1.apps.find((a) => a.id === "app-bypass").destinations.push({ type: "public", uri: `${HOST}/__backyard/oauth/*` });
      s.edge = { preConnector: true };
    });
    r = await sb.run(["access", "enable", "--team", TEAM, "--aud", DASH_AUD, "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(liveVars(sb).AUTH_MODE, "access");
    // Re-verified on the Worker in AUTH_MODE=access (its hub page answers 401 there).
    sb.fake.edgeRequests.length = 0;
    r = await sb.run(["access", "enable", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    assert.ok(sb.fake.edgeRequests.some((x) => x.path === "/.well-known/oauth-authorization-server" && x.status === 401));
    assert.ok(sb.fake.edgeRequests.some((x) => x.path === "/__backyard/oauth/token" && x.status === 404));
  });

  it("asks who may sign in when it must, and confirms consumer mail domains", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(["access", "enable", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /who may sign in: pass --allow-email <email> or --allow-email-domain <domain>/);
    assert.deepEqual(writesOf(sb.requests()), []);
    r = await sb.run(["access", "enable", "--allow-email-domain", "gmail.com", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /needs confirmation: re-run with --yes \(Anyone with a gmail\.com address could sign in\. Allow gmail\.com anyway\?\)/);
    assert.deepEqual(accessReqs(sb.requests()), [], "asked before any Access call");
    r = await sb.run(["access", "enable", "--allow-email", "not-an-email", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /--allow-email expects an email address/);
    r = await sb.run(["access", "enable", "--allow-email-domain", "gmail.com", "--yes", "--wait", "0"], WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(sb.state().access.allow, { emails: [], emailDomains: ["gmail.com"] });
  });

  it("needs an active domain from domain add, a deployed Worker, and no --var AUTH_MODE", async () => {
    const sb = await accessSandbox({ state: { domain: undefined, url: WD } });
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /Cloudflare Access protects your own hostname: run npm run cf -- domain add <host> first/);
    sb.writeState("backyard", { ...sb.state(), domain: { host: HOST, mode: "host", status: "pending-dns", source: "cli" } });
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /backyard\.example\.com is pending-dns: re-run npm run cf -- domain add backyard\.example\.com --host-mode/);
    sb.writeState("backyard", {
      ...sb.state(),
      domain: { host: HOST, mode: "path", status: "active", source: "cli" },
      vars: { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: DASH_AUD },
    });
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /AUTH_MODE, ACCESS_TEAM_DOMAIN, ACCESS_AUD are set with --var \(e\.g\. by the Terraform module\): drop them with --var AUTH_MODE: --var ACCESS_TEAM_DOMAIN: --var ACCESS_AUD: or keep managing them with Terraform/);
    sb.updateCf((s) => delete s.workers.backyard);
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /Worker backyard is not deployed on this account: run npm run cf -- setup first/);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(!deployOf(sb.calls()));
  });

  it("finds the applications from the terminal and asks before using them", { skip: !ttyAvailable() && "needs util-linux script" }, async () => {
    const sb = await accessSandbox();
    seedDashboardApps(sb);
    const r = await sb.runTTY(["access", "enable", "--allow-email-domain", "acme.com", "--yes", "--wait", "20"], {}, {
      answers: [
        [/How do you want to continue\? \S*\[1-2\]/, "1\r"],
        [/Use it\? \S*\[y\/N\]/, "y\r"],
      ],
    });
    sb.outputs.push(r.out);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Cloudflare Access needs two self-hosted applications/);
    assert.match(r.out, /Found Cloudflare Access in front of https:\/\/backyard\.example\.com: team acme\.cloudflareaccess\.com, application AUD d+1\. Use it\? \S*\[y\/N\]/);
    assert.doesNotMatch(r.out, /Use it\? \S*yes \(--yes\)/, "--yes does not answer it");
    assert.match(r.out, /Who may sign in|Allow · Include · Emails ending in @acme\.com/);
    assert.deepEqual(sb.state().access, { team: TEAM, aud: DASH_AUD, source: "manual", allow: { emails: [], emailDomains: ["acme.com"] } });
    assert.equal(deployOf(sb.calls()).config.vars.ACCESS_AUD, DASH_AUD);
  });

  it("takes a pasted API token without echoing or storing it", { skip: !ttyAvailable() && "needs util-linux script" }, async () => {
    const sb = await accessSandbox();
    const r = await sb.runTTY(ENABLE, {}, {
      answers: [
        [/How do you want to continue\? \S*\[1-2\]/, "2\r"],
        [/API token \(not stored\):/, `${ADMIN}\r`],
      ],
    });
    sb.outputs.push(r.out);
    assert.equal(r.code, 0, r.out);
    assert.ok(!r.out.includes(ADMIN), "the token is not echoed");
    assert.equal(sb.state().access.source, "api");
    assert.ok(accessReqs(sb.requests()).every((x) => x.token === "admin"));
  });
});

describe("backyard-cloudflare access disable", () => {
  it("deploys AUTH_MODE=none first, then deletes only what it created, in order", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const ids = sb.state().access;
    sb.updateCf((s) => s.access.acc1.apps.push({ id: "other-app", name: "Other", type: "self_hosted", domain: "other.example.com", policies: [], aud: "9".repeat(64) }));
    sb.calls();
    sb.requests();
    // --delete-apps without credential A, or without --yes: refused before anything changes.
    r = await sb.run(["access", "disable", "--yes", "--delete-apps"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /--delete-apps needs an API token with Account · Access: Apps and Policies · Edit/);
    r = await sb.run(["access", "disable", "--delete-apps"], WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /needs confirmation: re-run with --yes \(Turn company login off\? Sites become viewable by anyone with a link: on https:\/\/backyard\.acme\.workers\.dev right away, and on https:\/\/backyard\.example\.com/);
    assert.ok(!deployOf(sb.calls()));
    // The deploy comes first: when it fails, nothing is deleted.
    r = await sb.run(["access", "disable", "--yes", "--delete-apps"], { ...WITH_ADMIN, FAKE_DEPLOY_FAIL: "1" });
    assert.equal(r.code, 1, r.out);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.deepEqual(sb.state().access, ids);
    // The first delete fails: the Worker is already open (deployed past the guard), the ids are kept.
    sb.fake.addFault({ method: "DELETE", path: `/accounts/acc1/access/apps/${ids.appId}`, status: 400, code: 12000, message: "try again" });
    r = await sb.run(["access", "disable", "--yes", "--delete-apps"], { ...WITH_ADMIN, BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /could not delete Access application "Backyard \(backyard\)"/);
    assert.match(r.out, /Already applied: the Worker deployed with AUTH_MODE=none/);
    const d = deployOf(sb.calls());
    assert.equal(d.config.vars.AUTH_MODE, "none");
    assert.equal(d.config.vars.ACCESS_TEAM_DOMAIN, "");
    assert.equal(d.config.vars.ACCESS_AUD, "");
    assert.equal(liveVars(sb).AUTH_MODE, "none");
    assert.equal(sb.state().access, undefined);
    assert.deepEqual(sb.state().accessCreated, { host: HOST, appId: ids.appId, bypassAppId: ids.bypassAppId, allowPolicyId: ids.allowPolicyId, bypassPolicyId: ids.bypassPolicyId });
    assert.equal(sb.result().authMode, "none");
    sb.requests();
    // A re-run resumes the deletions (no deploy needed any more).
    r = await sb.run(["access", "disable", "--yes", "--delete-apps"], WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.ok(!deployOf(sb.calls()));
    assert.deepEqual(
      writesOf(sb.requests()).map((x) => `${x.method} ${x.path} ${x.token}`),
      [
        `DELETE /accounts/acc1/access/apps/${ids.appId} admin`,
        `DELETE /accounts/acc1/access/apps/${ids.bypassAppId} admin`,
        `DELETE /accounts/acc1/access/policies/${ids.allowPolicyId} admin`,
        `DELETE /accounts/acc1/access/policies/${ids.bypassPolicyId} admin`,
      ],
    );
    assert.deepEqual(sb.cf().access.acc1.apps.map((a) => a.id), ["other-app"], "a foreign application is never deleted");
    assert.deepEqual(sb.cf().access.acc1.policies, []);
    assert.equal(sb.state().accessCreated, undefined);
  });

  it("keeps the applications without --delete-apps, and the guard does not stand in the way", async () => {
    const sb = await accessSandbox();
    let r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    const ids = sb.state().access;
    sb.requests();
    sb.calls();
    r = await sb.run(["access", "disable", "--yes"], { BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 0, r.out);
    assert.equal(deployOf(sb.calls()).config.vars.AUTH_MODE, "none");
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.match(r.out, /The Access applications of https:\/\/backyard\.example\.com remain, so it still asks for a sign-in until they are deleted: Backyard \(backyard\), Backyard \(backyard\) machine endpoints \(npm run cf -- access disable --delete-apps/);
    assert.equal(sb.state().access, undefined);
    assert.equal(sb.state().accessCreated.appId, ids.appId);
    assert.equal(sb.result().command, "access disable");
    assert.equal(sb.result().authMode, "none");
    // Nothing more to turn off.
    r = await sb.run(["access", "disable", "--yes"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Cloudflare Access is not managed by this tool: nothing to do/);
    assert.match(r.out, /Access objects this tool created remain: delete them with npm run cf -- access disable --delete-apps/);
    assert.ok(!deployOf(sb.calls()));
    // Turning it on again reuses them.
    r = await sb.run(ENABLE, WITH_ADMIN);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.equal(sb.state().access.appId, ids.appId);
    assert.equal(sb.state().accessCreated, undefined);
  });

  it("cannot delete applications made in the dashboard", async () => {
    const sb = await accessSandbox();
    seedDashboardApps(sb);
    let r = await sb.run(["access", "enable", "--team", TEAM, "--aud", DASH_AUD, "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    r = await sb.run(["access", "disable", "--yes", "--delete-apps"], WITH_ADMIN);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /the applications were created in the dashboard: delete Backyard \(backyard\) and Backyard \(backyard\) machine endpoints there/);
    assert.equal(sb.state().access.source, "manual");
    r = await sb.run(["access", "disable", "--yes"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /delete them in the dashboard/);
    assert.equal(sb.cf().access.acc1.apps.length, 2);
  });
});

describe("scripts/lib/access-plan.mjs and the access helpers", () => {
  it("builds the exact request bodies", () => {
    const p = policyBodies({ worker: "team", emails: ["a@acme.com"], domains: ["acme.com"] });
    assert.deepEqual(p.allow, { name: "Backyard (team) users", decision: "allow", include: [{ email: { email: "a@acme.com" } }, { email_domain: { domain: "acme.com" } }], session_duration: "24h" });
    assert.deepEqual(p.bypass, { name: "Backyard (team) machine endpoints", decision: "bypass", include: [{ everyone: {} }] });
    const a = appBodies({ worker: "team", host: "h.acme.dev", mode: "host", allowPolicyId: "pa", bypassPolicyId: "pb" });
    assert.deepEqual(a.main.destinations, [
      { type: "public", uri: "h.acme.dev" },
      { type: "public", uri: "*.h.acme.dev" },
    ]);
    assert.deepEqual(a.main.policies, [{ id: "pa", precedence: 1 }]);
    assert.equal(a.bypass.domain, "h.acme.dev/__backyard/api/deploy");
    assert.deepEqual(a.bypass.destinations.map((d) => d.uri), ACCESS_BYPASS_PATHS.map((x) => `h.acme.dev${x}`));
    assert.deepEqual(a.bypass.policies, [{ id: "pb", precedence: 1 }]);
    assert.equal(appBodies({ worker: "team", host: "h", allowPolicyId: "a", bypassPolicyId: "b" }).main.destinations.length, 1);
  });

  it("keeps ACCESS_BYPASS_PATHS one statement, as the docs test parses it", () => {
    const text = readFileSync(new URL("../../scripts/lib/access-plan.mjs", import.meta.url), "utf8");
    const m = /export const ACCESS_BYPASS_PATHS = \[(.*?)\];/s.exec(text);
    assert.ok(m);
    assert.deepEqual(JSON.parse(`[${m[1]}]`), [
      "/__backyard/api/deploy",
      "/__backyard/api/deploy/*",
      "/__backyard/api/tokens",
      "/__backyard/healthz",
      "/__backyard/readyz",
      "/__backyard/mcp",
      "/__backyard/oauth/*",
      "/.well-known/oauth-authorization-server",
      "/.well-known/oauth-protected-resource/__backyard/mcp",
    ]);
    assert.deepEqual(ACCESS_BYPASS_PATHS.slice(-4), CONNECTOR_RULES, "the connector's paths come last");
  });

  it("prints the guided steps with the exact values (host mode lists *.<host>)", () => {
    const s = guidedSteps({ worker: "backyard", host: "sites.acme.dev", mode: "host", emails: ["a@acme.com"], domains: ["acme.com"], invokedAs: "npx backyard-cloudflare" });
    assert.ok(s.includes("     Public hostnames:  sites.acme.dev\n                        *.sites.acme.dev\n"), s);
    assert.ok(s.includes("     Policy:            Allow · Include · Emails ending in @acme.com, Emails a@acme.com\n"), s);
    assert.ok(s.includes("     Public hostnames:  sites.acme.dev/__backyard/api/deploy\n                        sites.acme.dev/__backyard/api/deploy/*\n"), s);
    assert.ok(
      s.includes(
        "                        sites.acme.dev/__backyard/readyz\n" +
          "                        sites.acme.dev/__backyard/mcp\n" +
          "                        sites.acme.dev/__backyard/oauth/*\n" +
          "                        sites.acme.dev/.well-known/oauth-authorization-server\n" +
          "                        sites.acme.dev/.well-known/oauth-protected-resource/__backyard/mcp\n" +
          "     Policy:            Bypass · Include · Everyone\n",
      ),
      s,
    );
    assert.ok(s.includes("BACKYARD_CF_API_TOKEN=<token> npx backyard-cloudflare access enable --allow-email a@acme.com --allow-email-domain acme.com"), s);
    assert.equal(describeAllow([], []), "");
  });

  it("normalizes allow lists and recognizes consumer mail domains", () => {
    assert.deepEqual(normalizeAllow(["A@Acme.com", "a@acme.com"], ["@Acme.COM", "acme.com."]), { emails: ["a@acme.com"], domains: ["acme.com"] });
    assert.deepEqual(parseWho("a@acme.com, @acme.com beta.dev"), { emails: ["a@acme.com"], domains: ["acme.com", "beta.dev"] });
    assert.throws(() => normalizeAllow(["nope"], []), /--allow-email expects an email address/);
    assert.throws(() => normalizeAllow([], ["not a domain"]), /--allow-email-domain expects a mail domain/);
    assert.ok(isConsumerMailDomain("@GMail.com"));
    assert.ok(!isConsumerMailDomain("acme.com"));
  });

  it("tells foreign coverage and changed fields apart", () => {
    const app = (domain, uris = []) => ({ domain, destinations: uris.map((uri) => ({ type: "public", uri })) });
    assert.ok(appCovers(app("backyard.example.com"), "backyard.example.com"));
    assert.ok(appCovers(app("backyard.example.com/admin"), "backyard.example.com"));
    assert.ok(appCovers(app("x.example.com", ["*.backyard.example.com"]), "backyard.example.com"));
    assert.ok(appCovers(app("*.example.com"), "backyard.example.com"));
    assert.ok(!appCovers(app("other.example.com"), "backyard.example.com"));
    assert.ok(!appCovers(app("example.com"), "backyard.example.com"));
    // A bookmark (App Launcher tile) gates nothing; a self-hosted app with the same domain does.
    assert.ok(!appCovers({ type: "bookmark", domain: "https://backyard.example.com" }, "backyard.example.com"));
    assert.ok(appCovers({ type: "self_hosted", domain: "backyard.example.com" }, "backyard.example.com"));
    const desired = { name: "n", destinations: [{ type: "public", uri: "a" }, { type: "public", uri: "b" }], policies: [{ id: "p", precedence: 1 }], include: [{ email: { email: "x@y.z" } }] };
    const live = { ...desired, id: "1", aud: "z", destinations: [{ uri: "B", type: "public" }, { type: "public", uri: "a" }], policies: [{ id: "p", name: "n", decision: "allow", precedence: 1 }] };
    assert.deepEqual(changedFields(desired, live), []);
    assert.deepEqual(changedFields({ ...desired, include: [] }, live), ["include"]);
  });
});
