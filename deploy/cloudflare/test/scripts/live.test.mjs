// In-process tests of scripts/lib/live.mjs (node --test): reading the live
// Worker through the fake Cloudflare API, the adoption table, the auth guard
// table, the /info fallback, and normalizeTeam's parity with the Worker's own
// (src/config.ts, the same cases run on both).
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { createServer } from "node:http";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { after, before, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { createApi } from "../../scripts/lib/cfapi.mjs";
import { adopt, aiEnabledOf, authGuard, authModeOf, fillAuthFromInfo, hostOf, normalizeTeam, readLive } from "../../scripts/lib/live.mjs";
import { parseJSONC } from "../../scripts/lib/layout.mjs";
import { CliError } from "../../scripts/lib/ui.mjs";
import { OAUTH_TOKEN, deployedWorker, startFakeCloudflare } from "./helpers/fake-cloudflare.mjs";

const pkgDir = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const BASE_VARS = parseJSONC(readFileSync(join(pkgDir, "wrangler.jsonc"), "utf8")).vars;
const AUD = "b".repeat(64);

/** The Worker's normalizeTeam, taken from src/config.ts (types stripped). */
function workerNormalizeTeam() {
  const src = readFileSync(join(pkgDir, "src", "config.ts"), "utf8");
  const m = /export function normalizeTeam\(v: string\): string \{([\s\S]*?)\n\}/.exec(src);
  assert.ok(m, "normalizeTeam in src/config.ts");
  return new Function("v", m[1]);
}

const refused = (fn, reason) =>
  assert.throws(fn, (e) => {
    assert.ok(e instanceof CliError, String(e));
    assert.equal(e.code, 3);
    assert.equal(e.reason, reason);
    assert.match(e.message, /Nothing was changed\./);
    return true;
  });

/** A live snapshot. */
const live = (o = {}) => ({ exists: true, vars: {}, secretNames: [], r2Bucket: "", ai: true, workersDev: true, authMode: "none", team: "", aud: "", source: "settings", error: "", ...o });

describe("normalizeTeam", () => {
  it("matches the Worker's normalization", () => {
    const worker = workerNormalizeTeam();
    for (const v of ["acme", "Acme", " acme ", "acme.cloudflareaccess.com", "https://Team.cloudflareaccess.com/", "http://acme.cloudflareaccess.com/cdn-cgi/access", "", "  ", "a.b.c"]) {
      assert.equal(normalizeTeam(v), worker(v), JSON.stringify(v));
    }
    assert.equal(normalizeTeam("https://Team.cloudflareaccess.com/"), "team.cloudflareaccess.com");
    assert.equal(normalizeTeam("acme"), "acme.cloudflareaccess.com");
    assert.equal(normalizeTeam(undefined), "");
  });

  it("reads AUTH_MODE and BASE_DOMAIN like the Worker", () => {
    assert.equal(authModeOf(""), "none");
    assert.equal(authModeOf(undefined), "none");
    assert.equal(authModeOf(" Access "), "access");
    assert.equal(authModeOf("acess"), "acess");
    assert.equal(authModeOf(true), "<boolean>");
    assert.equal(hostOf("https://Backyard.Example.com:443/x"), "backyard.example.com");
    assert.equal(hostOf("backyard.example.com."), "backyard.example.com");
  });
});

describe("readLive", () => {
  let dir;
  let fake;
  let api;
  before(async () => {
    dir = mkdtempSync(join(tmpdir(), "backyard-live-"));
    fake = await startFakeCloudflare({ statePath: join(dir, "cf.json") });
    api = createApi({ credential: { type: "oauth", token: OAUTH_TOKEN }, base: fake.apiBase, sleep: async () => {} });
  });
  after(async () => {
    await fake.close();
    rmSync(dir, { recursive: true, force: true });
  });

  it("reports a missing Worker, the bindings of a deployed one, and unknown on errors", async () => {
    assert.equal((await readLive({ api, accountId: "acc1", worker: "backyard" })).exists, false);
    fake.update((s) => {
      s.workers.backyard = deployedWorker({
        vars: { ...BASE_VARS, AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "Acme", ACCESS_AUD: AUD, LIMITS: { ai_per_day: 5 }, NAME_JSON: "quoted" },
        r2Bucket: "files",
        secrets: { BACKYARD_SECRET: "x".repeat(64) },
        workersDev: false,
      });
      // A JSON variable holding a string: the Worker sees the string.
      s.workers.backyard.bindings = s.workers.backyard.bindings.map((b) => (b.name === "NAME_JSON" ? { type: "json", name: "NAME_JSON", json: "quoted" } : b));
    });
    const l = await readLive({ api, accountId: "acc1", worker: "backyard" });
    assert.equal(l.exists, true);
    assert.equal(l.authMode, "access");
    assert.equal(l.team, "acme.cloudflareaccess.com");
    assert.equal(l.aud, AUD);
    assert.equal(l.r2Bucket, "files");
    assert.equal(l.ai, true, "the Workers AI binding");
    assert.equal(l.workersDev, false);
    assert.equal(l.vars.LIMITS, '{"ai_per_day":5}');
    assert.equal(l.vars.NAME_JSON, "quoted");
    assert.deepEqual(l.secretNames, ["BACKYARD_SECRET"]);
    assert.equal(l.source, "settings");
    // An unreadable subdomain leaves workersDev unknown.
    fake.addFault({ method: "GET", path: "/accounts/acc1/workers/scripts/backyard/subdomain", status: 403, code: 10000, times: 2 });
    assert.equal((await readLive({ api, accountId: "acc1", worker: "backyard" })).workersDev, null);
    // Settings errors: unknown, with the reason.
    fake.addFault({ method: "GET", path: "/accounts/acc1/workers/scripts/backyard/settings", status: 403, code: 10000, times: 2 });
    const u = await readLive({ api, accountId: "acc1", worker: "backyard" });
    assert.equal(u.exists, null);
    assert.equal(u.authMode, "");
    assert.match(u.error, /403/);
    // Deployed with --no-ai: no AI binding.
    fake.update((s) => (s.workers.backyard = deployedWorker({ vars: { ...BASE_VARS, AI_ENABLED: "false" }, ai: false })));
    const off = await readLive({ api, accountId: "acc1", worker: "backyard" });
    assert.equal(off.ai, false);
    assert.equal(off.vars.AI_ENABLED, "false");
  });
});

describe("fillAuthFromInfo", () => {
  let server;
  let base;
  let answer = () => ({ status: 200, body: { edition: "cloudflare", auth: { mode: "access" } } });
  before(async () => {
    server = createServer((req, res) => {
      const a = answer(req);
      res.writeHead(a.status, { "content-type": "application/json", ...(a.headers ?? {}) });
      res.end(JSON.stringify(a.body ?? {}));
    });
    await new Promise((r) => server.listen(0, "127.0.0.1", r));
    base = `http://127.0.0.1:${server.address().port}`;
  });
  after(() => new Promise((r) => server.close(r)));

  it("asks the Worker's /info when the settings are unreadable", async () => {
    let l = await fillAuthFromInfo(live({ exists: null, authMode: "", source: "" }), ["", base]);
    assert.equal(l.exists, true);
    assert.equal(l.authMode, "access");
    assert.equal(l.source, "info");
    answer = () => ({ status: 302, headers: { location: "https://acme.cloudflareaccess.com/cdn-cgi/access/login/h?kid=k" } });
    l = await fillAuthFromInfo(live({ exists: null, authMode: "" }), [base]);
    assert.equal(l.authMode, "access");
    assert.equal(l.team, "acme.cloudflareaccess.com");
    // Not a Backyard Worker, or nothing there: still unknown.
    answer = () => ({ status: 200, body: { edition: "server", auth: { mode: "none" } } });
    l = await fillAuthFromInfo(live({ exists: null, authMode: "" }), [base]);
    assert.equal(l.exists, null);
    assert.equal(l.authMode, "");
    // A known snapshot is left alone.
    l = await fillAuthFromInfo(live({ authMode: "none" }), [base]);
    assert.equal(l.source, "settings");
  });
});

describe("adopt", () => {
  it("copies what differs from the defaults and turns BASE_DOMAIN/AUTH_MODE into domain/access", () => {
    const state = {};
    const vars = { ...BASE_VARS, AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: AUD, BASE_DOMAIN: "Sites.Example.com", URL_MODE: "host", LIMITS: '{"ai_per_day":5}', SERVER_NAME: "Backyard" };
    const { parts, warnings } = adopt(state, live({ vars, r2Bucket: "files", workersDev: true }), BASE_VARS);
    assert.deepEqual(state.vars, { LIMITS: '{"ai_per_day":5}' }, "defaults are not copied, managed keys are not variables");
    assert.deepEqual(state.domain, { host: "sites.example.com", mode: "host", status: "active", source: "adopted" });
    assert.deepEqual(state.access, { team: "acme.cloudflareaccess.com", aud: AUD, source: "adopted" });
    assert.equal(state.r2, true);
    assert.equal(state.r2Bucket, "files");
    assert.equal(state.workersDev, true);
    assert.deepEqual(parts, ["R2 bucket files", "variables LIMITS", "domain sites.example.com (host mode)", "Access team acme.cloudflareaccess.com"]);
    assert.deepEqual(warnings, []);
  });

  it("keeps managed keys as variables when nothing uses them, so the deploy changes nothing", () => {
    const state = { vars: { OWNERSHIP: "owner" } };
    const { parts } = adopt(state, live({ vars: { ...BASE_VARS, URL_MODE: "host", AUTH_MODE: "acess", ACCESS_AUD: AUD }, workersDev: null }), BASE_VARS);
    assert.deepEqual(state.vars, { OWNERSHIP: "owner", URL_MODE: "host", AUTH_MODE: "acess", ACCESS_AUD: AUD });
    assert.equal(state.domain, undefined);
    assert.equal(state.access, undefined);
    assert.equal(state.workersDev, true, "unknown: assume on");
    assert.deepEqual(parts, ["variables ACCESS_AUD, AUTH_MODE, URL_MODE"]);
    const off = {};
    assert.deepEqual(adopt(off, live({ vars: { ...BASE_VARS }, workersDev: false }), BASE_VARS).parts, ["workers.dev off"]);
    assert.equal(off.workersDev, false);
    assert.equal(off.vars, undefined);
    // An unreadable workers.dev setting is reported.
    assert.equal(adopt({}, live({ workersDev: null }), BASE_VARS).warnings.length, 1);
  });
});

describe("AI on and off", () => {
  it("reads AI_ENABLED like the Worker's bool() with the default true", () => {
    for (const v of [undefined, null, "", " ", "true", "1", "yes", "on", " TRUE "]) assert.equal(aiEnabledOf(v), true, JSON.stringify(v));
    for (const v of ["false", "0", "no", "off", "ture", false, 0]) assert.equal(aiEnabledOf(v), false, JSON.stringify(v));
  });

  it("adopts AI off only from a Worker without the binding whose AI_ENABLED is off", () => {
    const off = {};
    const r = adopt(off, live({ vars: { ...BASE_VARS, AI_ENABLED: "false" }, ai: false }), BASE_VARS);
    assert.equal(off.ai, false);
    assert.equal(off.vars, undefined, "AI_ENABLED is managed then, not a variable");
    assert.deepEqual(r.parts, ["AI off"]);
    // The binding with AI_ENABLED off (--var AI_ENABLED:false): AI stays on, the variable is kept.
    const runtimeOff = {};
    adopt(runtimeOff, live({ vars: { ...BASE_VARS, AI_ENABLED: "false" }, ai: true }), BASE_VARS);
    assert.equal(runtimeOff.ai, undefined);
    assert.deepEqual(runtimeOff.vars, { AI_ENABLED: "false" });
    // No binding, AI on (other providers): AI stays on, so the next deploy binds Workers AI again.
    const noBinding = {};
    assert.deepEqual(adopt(noBinding, live({ vars: { ...BASE_VARS }, ai: false }), BASE_VARS).parts, []);
    assert.equal(noBinding.ai, undefined);
  });
});

describe("authGuard", () => {
  const access = (team = "acme.cloudflareaccess.com", aud = AUD) => ({ AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: team, ACCESS_AUD: aud });
  it("passes a first deploy and everything that keeps or strengthens auth", () => {
    assert.deepEqual(authGuard(live({ exists: false, authMode: "" }), { AUTH_MODE: "none" }), { checked: false, notes: [] });
    assert.equal(authGuard(live(), { AUTH_MODE: "none" }).checked, true);
    assert.equal(authGuard(live(), access()).notes.length, 0);
    const same = live({ authMode: "access", team: "acme.cloudflareaccess.com", aud: AUD });
    assert.deepEqual(authGuard(same, access("acme")).notes, [], "the same team, written differently");
    assert.match(authGuard(same, access("acme", "c".repeat(64))).notes[0], /the Access AUD changes/);
    assert.match(authGuard(live({ authMode: "access", team: "" }), access()).notes[0], /team could not be read/);
    assert.equal(authGuard(live({ authMode: "acess" }), access()).checked, true, "fixing a broken mode to access");
  });

  it("refuses downgrades, team changes, opening a fail-closed Worker and unknown live state", () => {
    const on = live({ authMode: "access", team: "acme.cloudflareaccess.com", aud: AUD });
    refused(() => authGuard(on, { AUTH_MODE: "none" }, { worker: "backyard" }), "downgrade");
    refused(() => authGuard(on, {}, { worker: "backyard" }), "downgrade");
    refused(() => authGuard(on, access("other")), "team");
    refused(() => authGuard(live({ authMode: "oidc" }), { AUTH_MODE: "none" }), "fail-closed");
    refused(() => authGuard(live({ exists: null, authMode: "", error: "500" }), { AUTH_MODE: "none" }), "unknown");
    refused(() => authGuard(live({ exists: null, authMode: "" }), access()), "unknown");
    assert.throws(() => authGuard(on, { AUTH_MODE: "none" }, { worker: "backyard", remedy: "  do X" }), /Nothing was changed\.\n {2}do X$/);
    // Confirmed: notes instead of a refusal.
    const g = authGuard(on, { AUTH_MODE: "none" }, { allow: true, worker: "backyard" });
    assert.equal(g.checked, true);
    assert.match(g.notes[0], /would turn it off \(AUTH_MODE=none\) — deploying anyway \(confirmed\)/);
  });
});

// The fake Cloudflare of test/scripts/helpers is what every engine test runs
// against: its semantics are pinned here (custom-domain changesets, overrides,
// permissions, Access at the edge, DoH, faults).
describe("helpers/fake-cloudflare.mjs", () => {
  let dir;
  let fake;
  let W;
  let A;
  const ADMIN = "admin-token-0123456789abcdef";
  const edge = async (host, path, headers = {}) => {
    const r = await fetch(`${fake.origin}/__edge/${host}${path}`, { redirect: "manual", headers });
    return { status: r.status, location: r.headers.get("location") ?? "", error: r.headers.get("x-fake-error") ?? "", text: await r.text() };
  };
  before(async () => {
    dir = mkdtempSync(join(tmpdir(), "backyard-fake-"));
    fake = await startFakeCloudflare({ statePath: join(dir, "cf.json") });
    fake.update((s) => {
      s.tokens[ADMIN] = { label: "admin", permissions: ["dns:edit", "zone:read", "access_apps:edit", "access_org:read"] };
      s.zones = [{ id: "z1", name: "example.com", status: "active", account: "acc1" }];
      s.dnsRecords = [{ id: "r1", zone_id: "z1", zone_name: "example.com", type: "A", name: "www.example.com", content: "192.0.2.1", proxied: true }];
      s.workers.backyard = deployedWorker({ vars: { ...BASE_VARS }, secrets: { BACKYARD_SECRET: "x".repeat(64), DEPLOY_TOKEN: "byd_tok-0123456789" } });
      s.workers.other = deployedWorker({ vars: { ...BASE_VARS } });
      s.customDomains = [{ id: "cdo", hostname: "taken.example.com", zone_id: "z1", zone_name: "example.com", service: "other", environment: "production" }];
      s.access = { acc1: { org: { auth_domain: "acme.cloudflareaccess.com", name: "acme" }, apps: [], policies: [] } };
    });
    W = createApi({ credential: { type: "oauth", token: OAUTH_TOKEN }, base: fake.apiBase, sleep: async () => {} });
    A = createApi({ credential: { type: "api_token", token: ADMIN }, base: fake.apiBase, sleep: async () => {}, label: "admin" });
  });
  after(async () => {
    await fake.close();
    rmSync(dir, { recursive: true, force: true });
  });

  it("answers custom-domain changesets and refuses overrides that were not asked for", async () => {
    const cs = await W.post("/accounts/acc1/workers/scripts/backyard/domains/changeset", [
      { hostname: "backyard.example.com", zone_id: "z1" },
      { hostname: "www.example.com", zone_id: "z1" },
      { hostname: "taken.example.com", zone_id: "z1" },
    ]);
    assert.deepEqual(cs.added.map((x) => x.hostname), ["backyard.example.com"]);
    assert.deepEqual(cs.conflicting.map((x) => x.hostname), ["www.example.com"]);
    assert.deepEqual(cs.updated.map((x) => [x.hostname, x.service, x.modified]), [["taken.example.com", "other", true]]);
    const put = (origins, o = {}) =>
      W.put("/accounts/acc1/workers/scripts/backyard/domains/records", { override_scope: true, override_existing_origin: false, override_existing_dns_record: false, ...o, origins });
    await assert.rejects(put([{ hostname: "taken.example.com", zone_id: "z1" }]), (e) => e.status === 409 && e.has(100116));
    await assert.rejects(put([{ hostname: "www.example.com", zone_id: "z1" }]), (e) => e.status === 409 && e.has(100117));
    await put([{ hostname: "backyard.example.com", zone_id: "z1" }]);
    const mine = await W.list("/accounts/acc1/workers/domains/records", { service: "backyard", environment: "production" });
    assert.deepEqual(mine.map((x) => x.hostname), ["backyard.example.com"]);
    // The other Worker's domain is untouched, and a changeset now shows ours as unmodified.
    assert.equal(fake.state().customDomains.find((d) => d.hostname === "taken.example.com").service, "other");
    const again = await W.post("/accounts/acc1/workers/scripts/backyard/domains/changeset", [{ hostname: "backyard.example.com", zone_id: "z1" }]);
    assert.deepEqual(again.updated.map((x) => x.modified), [false]);
    // override_scope: domains of this Worker that are not listed are detached ("removed").
    const removed = await W.post("/accounts/acc1/workers/scripts/backyard/domains/changeset", [{ hostname: "new.example.com", zone_id: "z1" }]);
    assert.deepEqual(removed.removed.map((x) => x.hostname), ["backyard.example.com"]);
  });

  it("enforces token permissions (403 + code 10000) and injects faults", async () => {
    await assert.rejects(W.post("/zones/z1/dns_records", { type: "AAAA", name: "*.backyard.example.com", content: "100::", proxied: true }), (e) => e.status === 403 && e.has(10000));
    await assert.rejects(W.get("/accounts/acc1/access/organizations"), (e) => e.status === 403 && /Access: Organizations/.test(e.message));
    const rec = await A.post("/zones/z1/dns_records", { type: "AAAA", name: "*.backyard.example.com", content: "100::", proxied: true, ttl: 1 });
    assert.equal(rec.name, "*.backyard.example.com");
    await assert.rejects(createApi({ credential: { type: "api_token", token: "unknown-token-0123456789" }, base: fake.apiBase }).get("/zones"), (e) => e.status === 401 && e.has(10000));
    fake.addFault({ method: "GET", path: "/zones", status: 500, code: 7, times: 1, retryAfter: 0 });
    assert.equal((await W.list("/zones", { name: "example.com" })).length, 1, "a GET is retried past one 500");
    const last = fake.requests.slice(-2);
    assert.deepEqual(last.map((x) => x.status), [500, 200]);
    assert.ok(fake.requests.every((x) => !JSON.stringify(x).includes(OAUTH_TOKEN) && !JSON.stringify(x).includes(ADMIN)), "the log holds labels, not tokens");
    assert.deepEqual([...new Set(fake.requests.map((x) => x.token))].sort(), ["admin", "oauth", "unknown"]);
  });

  it("puts Access in front of the edge: the most specific destination wins", async () => {
    const bp = await A.post("/accounts/acc1/access/policies", { name: "bypass", decision: "bypass", include: [{ everyone: {} }] });
    const ap = await A.post("/accounts/acc1/access/policies", { name: "allow", decision: "allow", include: [{ email_domain: { domain: "acme.com" } }] });
    await W.put("/accounts/acc1/workers/scripts/backyard/domains/records", { override_scope: true, origins: [{ hostname: "backyard.example.com", zone_id: "z1" }] });
    await W.post("/zones/z1/workers/routes", { pattern: "*.backyard.example.com/*", script: "backyard" });
    const paths = ["/__backyard/api/deploy", "/__backyard/api/deploy/*", "/__backyard/api/tokens", "/__backyard/healthz", "/__backyard/readyz"];
    await A.post("/accounts/acc1/access/apps", {
      name: "machine",
      type: "self_hosted",
      domain: "backyard.example.com/__backyard/api/deploy",
      destinations: paths.map((p) => ({ type: "public", uri: `backyard.example.com${p}` })),
      policies: [{ id: bp.id, precedence: 1 }],
    });
    const main = await A.post("/accounts/acc1/access/apps", {
      name: "main",
      type: "self_hosted",
      domain: "backyard.example.com",
      destinations: [{ type: "public", uri: "backyard.example.com" }, { type: "public", uri: "*.backyard.example.com" }],
      policies: [{ id: ap.id, precedence: 1 }],
    });
    assert.match(main.aud, /^[0-9a-f]{64}$/);
    let r = await edge("backyard.example.com", "/");
    assert.equal(r.status, 302);
    assert.deepEqual(parseAccessRedirectForTest(r.location), { team: "acme.cloudflareaccess.com", kid: main.aud });
    assert.equal((await edge("backyard.example.com", "/__backyard/healthz")).status, 200);
    assert.equal((await edge("backyard.example.com", "/__backyard/readyz")).status, 200);
    r = await edge("backyard.example.com", "/__backyard/api/deploy/info");
    assert.equal(JSON.parse(r.text).edition, "cloudflare");
    // Without a token: the guest in AUTH_MODE=none (like the real gateway), 401 in access mode or for a wrong token.
    r = await edge("backyard.example.com", "/__backyard/api/deploy/me");
    assert.equal(r.status, 200);
    assert.equal(JSON.parse(r.text).anonymous, true);
    assert.equal((await edge("backyard.example.com", "/__backyard/api/deploy/me", { authorization: "Bearer byd_wrong-0123456789" })).status, 401);
    assert.equal((await edge("backyard.example.com", "/__backyard/api/deploy/me", { authorization: "Bearer byd_tok-0123456789" })).status, 200);
    assert.equal((await edge("backyard.example.com", "/__backyard/api/tokens")).status, 401);
    assert.equal((await edge("backyard.example.com", "/__backyard/api/tokens/x")).status, 302, "an exact path covers only itself");
    assert.equal((await edge("site.backyard.example.com", "/")).status, 302, "*.host");
    assert.equal((await edge("acme.cloudflareaccess.com", "/cdn-cgi/access/certs")).status, 200);
    assert.equal((await edge("other.cloudflareaccess.com", "/cdn-cgi/access/certs")).error, "ENOTFOUND");
    assert.equal((await edge("nope.example.com", "/")).error, "ENOTFOUND");
    assert.match((await edge("www.example.com", "/")).text, /some other website/);
    // workers.dev is not behind Access; AUTH_MODE=access makes the Worker answer 401 there.
    assert.equal((await edge("backyard.acme.workers.dev", "/")).status, 200);
    fake.update((s) => (s.workers.backyard.vars.AUTH_MODE = "access"));
    assert.equal((await edge("backyard.acme.workers.dev", "/")).status, 401);
    assert.equal((await edge("backyard.example.com", "/__backyard/api/deploy/me")).status, 401);
    // Knobs: no kid, another team, empty certs.
    fake.update((s) => (s.edge = { omitKid: true, redirectTeam: "evil.cloudflareaccess.com", certsEmpty: true }));
    assert.deepEqual(parseAccessRedirectForTest((await edge("backyard.example.com", "/")).location), { team: "evil.cloudflareaccess.com", kid: "" });
    assert.deepEqual(JSON.parse((await edge("acme.cloudflareaccess.com", "/cdn-cgi/access/certs")).text).keys, []);
    // DoH: the route's wildcard record resolves every site name.
    const doh = async (name) => (await (await fetch(`${fake.dohUrl}?name=${name}&type=AAAA`)).json()).Status;
    assert.equal(await doh("byd-probe-12345678.backyard.example.com"), 0);
    assert.equal(await doh("nope.example.com"), 3);
    fake.update((s) => (s.dns = { "*.backyard.example.com": "nxdomain" }));
    assert.equal(await doh("byd-probe-12345678.backyard.example.com"), 3);
  });

  it("answers the Claude connector's paths like the Worker (MCP_ENABLED on, off, and before the connector)", async () => {
    const WDH = "backyard.acme.workers.dev";
    const paths = ["/__backyard/mcp", "/__backyard/oauth/token", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"];
    const answers = async () => {
      const out = [];
      for (const p of paths) {
        const r = await fetch(`${fake.origin}/__edge/${WDH}${p}`, { headers: { accept: "application/json" } });
        out.push(`${r.status} ${r.headers.get("content-type")}`);
      }
      return out;
    };
    const info = async () => JSON.parse((await edge(WDH, "/__backyard/api/deploy/info")).text);
    fake.update((s) => {
      s.edge = {};
      s.dns = {};
      s.workers.backyard.vars.AUTH_MODE = "access";
      delete s.workers.backyard.vars.MCP_ENABLED;
    });
    // Off: the JSON 404 of every path, before any sign-in (also in AUTH_MODE=access); /info has no mcp_url.
    assert.deepEqual(await answers(), Array(4).fill("404 application/json"));
    assert.match((await edge(WDH, "/__backyard/oauth/token")).text, /the Claude connector is not enabled on this server/);
    assert.equal((await info()).mcp_url, undefined);
    // On: 401 (with the challenge), 405, 200, 200; /info has mcp_url.
    fake.update((s) => (s.workers.backyard.vars.MCP_ENABLED = "true"));
    assert.deepEqual(await answers(), ["401 application/json", "405 application/json", "200 application/json", "200 application/json"]);
    const r = await fetch(`${fake.origin}/__edge/${WDH}/__backyard/mcp`, { method: "POST" });
    assert.equal(r.headers.get("www-authenticate"), `Bearer resource_metadata="https://${WDH}/.well-known/oauth-protected-resource/__backyard/mcp", scope="mcp"`);
    assert.equal((await info()).mcp_url, `https://${WDH}/__backyard/mcp`);
    // A Worker from before the connector: JSON 404s below /__backyard/, its hub page (401 in AUTH_MODE=access) at the discovery documents.
    fake.update((s) => (s.edge = { preConnector: true }));
    const legacy = await answers();
    assert.deepEqual(legacy.slice(0, 2), ["404 application/json", "404 application/json"]);
    assert.deepEqual(legacy.slice(2).map((x) => x.split(" ")[0]), ["401", "401"]);
    assert.equal((await info()).mcp_url, undefined);
    fake.update((s) => {
      s.edge = {};
      s.workers.backyard.vars.AUTH_MODE = "none";
      delete s.workers.backyard.vars.MCP_ENABLED;
    });
  });
});

function parseAccessRedirectForTest(location) {
  const u = new URL(location);
  return { team: u.hostname, kid: u.searchParams.get("kid") ?? "" };
}
