// Offline tests of `backyard-cloudflare domain add` and `domain remove` (node
// --test): the engine runs in a sandbox against the fake wrangler and the fake
// Cloudflare API, DNS-over-HTTPS and edge (test/scripts/helpers), so the zone
// discovery, the fail-closed custom-domain preflight, host mode (route,
// wildcard record, certificate), the refusals and the removal order are checked
// without a Cloudflare account.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { after, afterEach, describe, it } from "node:test";

import { dnsRecordName, parseHost, registrableDomain, zoneCandidates } from "../../scripts/domain.mjs";
import { parseJSONC } from "../../scripts/lib/layout.mjs";
import { TOKEN_PRESETS, deployedWorker } from "./helpers/fake-cloudflare.mjs";
import { assertNoLeaks, createSandbox, ttyAvailable } from "./helpers/sandbox.mjs";

const WD = "https://backyard.acme.workers.dev";
const TOKEN = "byd_domain-test-token-0123456789abcdef";
const SECRET = "s".repeat(64);
const AUD = "c".repeat(64);
/** An admin token with Zone · DNS · Edit (BACKYARD_CF_API_TOKEN). */
const ADMIN = "admin-dns-token-0123456789abcdef";
const ZONE = { id: "zone-example", name: "example.com", status: "active", account: "acc1" };
/** What setup leaves on this machine. */
const BASE_STATE = { version: 2, accountId: "acc1", url: WD, workersDevUrl: WD, workersDev: true, deployToken: TOKEN, updatedAt: "2026-01-01T00:00:00Z" };

const sandboxes = [];
let fresh = [];

/** A Worker as setup deployed it (the defaults of wrangler.jsonc plus `vars`). */
function seedWorker(sb, vars = {}) {
  const base = parseJSONC(readFileSync(join(sb.pkg, "wrangler.jsonc"), "utf8")).vars;
  sb.updateCf((s) => {
    s.workers.backyard = deployedWorker({ vars: { ...base, ...vars }, secrets: { BACKYARD_SECRET: SECRET, DEPLOY_TOKEN: TOKEN } });
  });
}

/**
 * A sandbox after `setup`: the zone example.com, the admin token known to the
 * fake, the deployed Worker and its state (o.state: fields over BASE_STATE;
 * null for a machine without state). Every run's output is kept for the
 * secret checks after each test.
 */
async function newSandbox(o = {}) {
  const sb = await createSandbox({ edge: o.edge });
  sb.updateCf((s) => {
    s.zones.push({ ...ZONE });
    s.tokens[ADMIN] = { label: "admin", permissions: [...TOKEN_PRESETS.dns] };
  });
  seedWorker(sb);
  if (o.state !== null) sb.writeState("backyard", { ...BASE_STATE, ...(o.state ?? {}) });
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
// Every test: no secret in a wrangler argv or env, the state, the result file or the output.
afterEach(() => {
  const check = fresh;
  fresh = [];
  for (const sb of check) assertNoLeaks(sb, { outputs: sb.outputs });
});
after(async () => {
  for (const sb of sandboxes) await sb.close();
});

const deployOf = (calls) => calls.filter((c) => c.args[0] === "deploy").at(-1);
const isChangeset = (x) => x.method === "POST" && x.path.endsWith("/domains/changeset");
/** The API writes of requests (the changeset POST is a check, not a write). */
const writesOf = (reqs) => reqs.filter((x) => x.method !== "GET" && !isChangeset(x));
const line = (x) => `${x.method} ${x.path}${x.path === "/zones" ? ` name=${x.query.name}` : ""}`;
/** A custom domain of the fake (zone example.com). */
const customDomain = (id, hostname, service = "backyard") => ({ id, hostname, zone_id: ZONE.id, zone_name: ZONE.name, service, environment: "production" });
const dnsRecord = (id, type, name, o = {}) => ({ id, zone_id: ZONE.id, zone_name: ZONE.name, type, name, content: type === "AAAA" ? "100::" : "192.0.2.10", proxied: true, ttl: 1, comment: "", ...o });
/** state.domain of a path-mode domain. */
const pathDomain = (host) => ({ host, mode: "path", zoneId: ZONE.id, zoneName: ZONE.name, status: "active", source: "cli" });

/** From now on, every API request records how many deploys the Worker had when it arrived. */
function trackDeploys(sb) {
  const reqs = sb.fake.requests;
  const push = reqs.push.bind(reqs);
  reqs.push = (...entries) => {
    for (const e of entries) e.deploys = sb.cf().workers?.backyard?.deployments ?? 0;
    return push(...entries);
  };
}

/** A host-mode domain sites.example.com as `domain add --host-mode` leaves it, plus foreign objects. */
function seedHostDomain(sb) {
  sb.updateCf((s) => {
    s.customDomains.push(customDomain("cd-sites", "sites.example.com"), customDomain("cd-foreign", "app.example.com", "other"));
    s.routes.push({ id: "route-sites", zone_id: ZONE.id, pattern: "*.sites.example.com/*", script: "backyard" });
    s.routes.push({ id: "route-foreign", zone_id: ZONE.id, pattern: "*.app.example.com/*", script: "other" });
    s.dnsRecords.push(dnsRecord("rec-sites", "AAAA", "*.sites.example.com", { comment: "Backyard sites (Worker backyard)" }), dnsRecord("rec-www", "A", "www.example.com"));
  });
  seedWorker(sb, { BASE_DOMAIN: "sites.example.com", URL_MODE: "host" });
}

describe("backyard-cloudflare domain add", () => {
  it("validates <host> and --zone before anything else", async () => {
    const sb = await newSandbox();
    for (const [args, re] of [
      [["https://backyard.example.com"], /invalid host "https:\/\/backyard\.example\.com": give the host name only, without a scheme/],
      [["backyard.example.com:8443"], /without a port/],
      [["backyard.example.com/hub"], /without a path/],
      [["*.example.com"], /no wildcards \(--host-mode serves the sites at \*\.<host>\)/],
      [["localhost"], /a host name has at least two labels/],
      [["backyard.acme.workers.dev"], /workers\.dev names belong to Cloudflare/],
      [["bad_label.example.com"], /not a valid host name/],
      [["192.0.2.1"], /not a valid host name/],
      [["backyard.example.com", "--zone", "other.com"], /backyard\.example\.com is not in the zone other\.com \(--zone\)/],
      [["backyard.example.com", "--zone", "https://example.com"], /invalid --zone/],
      [["backyard.example.com", "--wait", "x"], /--wait must be a whole number of seconds/],
      [["backyard.example.com", "extra"], /unexpected argument "extra"/],
      [[], /domain add needs <host>/],
    ]) {
      const r = await sb.run(["domain", "add", ...args]);
      assert.equal(r.code, 2, `${args.join(" ")}: ${r.out}`);
      assert.match(r.out, re, args.join(" "));
    }
    assert.deepEqual(sb.calls(), [], "no wrangler call");
    assert.equal(sb.fake.requests.length, 0, "no API request");
  });

  it("finds the zone by its longest suffix (GETs only), and stops at a missing or pending zone", async () => {
    const sb = await newSandbox({ edge: ["example.co.uk"] });
    sb.updateCf((s) => s.zones.push({ id: "zone-uk", name: "example.co.uk", status: "active", account: "acc1" }, { id: "zone-p", name: "pending.dev", status: "pending", account: "acc1" }));
    let r = await sb.run(["domain", "add", "a.b.example.co.uk", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    let zoneReqs = sb.requests().filter((x) => x.path === "/zones");
    assert.deepEqual(zoneReqs.map(line), ["GET /zones name=a.b.example.co.uk", "GET /zones name=b.example.co.uk", "GET /zones name=example.co.uk"]);
    assert.ok(zoneReqs.every((x) => x.query["account.id"] === "acc1"));
    assert.equal(sb.state().domain.zoneId, "zone-uk");
    assert.equal(sb.state().domain.zoneName, "example.co.uk");
    // --zone: one lookup.
    sb.writeState("backyard", BASE_STATE);
    sb.updateCf((s) => (s.customDomains = []));
    r = await sb.run(["domain", "add", "a.b.example.co.uk", "--zone", "Example.co.uk", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(sb.requests().filter((x) => x.path === "/zones").map(line), ["GET /zones name=example.co.uk"]);
    // No zone holds the host: the add-site link, nothing written or deployed.
    sb.writeState("backyard", BASE_STATE);
    sb.calls();
    r = await sb.run(["domain", "add", "hub.other.co.uk", "--wait", "0"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /no zone of this Cloudflare account holds hub\.other\.co\.uk: add other\.co\.uk to Cloudflare first: https:\/\/dash\.cloudflare\.com\/acc1\/add-site/);
    zoneReqs = sb.requests();
    assert.deepEqual(zoneReqs.filter((x) => x.path === "/zones").map(line), ["GET /zones name=hub.other.co.uk", "GET /zones name=other.co.uk", "GET /zones name=co.uk"]);
    assert.deepEqual(zoneReqs.filter((x) => x.method !== "GET"), []);
    // A zone that is not active yet.
    r = await sb.run(["domain", "add", "hub.pending.dev", "--wait", "0"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /zone pending\.dev is pending: finish switching its nameservers/);
    assert.deepEqual(sb.requests().filter((x) => x.method !== "GET"), []);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    assert.equal(sb.state().domain, undefined);
  });

  it("attaches the hub's custom domain in path mode, keeps the Worker's other custom domains, deploys and verifies", async () => {
    const sb = await newSandbox();
    sb.updateCf((s) => s.customDomains.push(customDomain("cd-old", "old.example.com")));
    const env = { BACKYARD_CF_RESULT_FILE: sb.resultPath };
    let r = await sb.run(["domain", "add", "Backyard.Example.com."], env);
    assert.equal(r.code, 0, r.out);
    const reqs = sb.requests();
    assert.deepEqual(reqs.map(line), [
      "GET /accounts/acc1/workers/scripts/backyard/settings",
      "GET /accounts/acc1/workers/scripts/backyard/subdomain",
      "GET /zones name=backyard.example.com",
      "GET /zones name=example.com",
      "POST /accounts/acc1/workers/scripts/backyard/domains/changeset",
      "PUT /accounts/acc1/workers/scripts/backyard/domains/records",
      "GET /accounts/acc1/workers/domains/records",
    ]);
    assert.ok(reqs.every((x) => x.token === "oauth"), "wrangler's own login does it all");
    assert.deepEqual(reqs[4].query, { replace_state: "true" });
    assert.deepEqual(reqs[4].body, [{ hostname: "backyard.example.com", zone_id: ZONE.id }]);
    assert.deepEqual(reqs[5].body, {
      override_scope: true,
      override_existing_origin: false,
      override_existing_dns_record: false,
      origins: [
        { hostname: "backyard.example.com", zone_id: ZONE.id },
        { hostname: "old.example.com", zone_id: ZONE.id },
      ],
    });
    assert.deepEqual(reqs[6].query, { service: "backyard", environment: "production", page: "1", per_page: "50" });
    assert.match(r.out, /Keeping the other custom domains of Worker backyard: old\.example\.com/);
    const cf = sb.cf();
    assert.deepEqual(cf.customDomains.map((x) => `${x.hostname} ${x.service}`).sort(), ["backyard.example.com backyard", "old.example.com backyard"]);
    // Deployed with the managed variables; no secret is touched.
    const d = deployOf(sb.calls());
    assert.equal(d.config.vars.BASE_DOMAIN, "backyard.example.com");
    assert.equal(d.config.vars.URL_MODE, "path");
    assert.equal(d.config.routes, undefined);
    assert.equal(d.secretsFile, undefined);
    // Verified on the edge.
    assert.ok(sb.fake.edgeRequests.some((x) => x.host === "backyard.example.com" && x.path === "/__backyard/readyz" && x.status === 200));
    const st = sb.state();
    assert.deepEqual(st.domain, {
      host: "backyard.example.com",
      mode: "path",
      zoneId: ZONE.id,
      zoneName: "example.com",
      customDomainId: cf.customDomains.find((x) => x.hostname === "backyard.example.com").id,
      status: "active",
      source: "cli",
    });
    assert.equal(st.url, "https://backyard.example.com");
    assert.equal(st.workersDevUrl, WD);
    assert.equal(st.deployToken, TOKEN);
    assert.deepEqual(sb.result(), {
      schema: 1,
      command: "domain add",
      worker: "backyard",
      statePath: sb.statePath(),
      url: "https://backyard.example.com",
      previousUrl: WD,
      workersDevUrl: WD,
      authMode: "none",
      deployToken: "state",
    });
    assert.match(r.out, /Backyard is on https:\/\/backyard\.example\.com/);
    assert.match(r.out, /Sites: https:\/\/backyard\.example\.com\/s\/<site>\//);
    assert.match(r.out, /Company login next: npm run cf -- access enable --allow-email-domain <your-domain>/);

    // A re-run changes nothing on Cloudflare: it redeploys and re-verifies.
    r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"], env);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(deployOf(sb.calls()), "redeployed");
    assert.deepEqual(sb.state().domain, st.domain);
    assert.equal(sb.result().previousUrl, "https://backyard.example.com");
  });

  it("updates its own custom domain whose settings differ (modified, same Worker) without --take-over", async () => {
    const sb = await newSandbox();
    // Attached to this Worker in the dashboard, with Preview URLs on: the changeset reports it modified.
    sb.updateCf((s) => s.customDomains.push({ ...customDomain("cd-hub", "backyard.example.com"), previews_enabled: true }));
    const r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    const put = writesOf(sb.requests()).find((x) => x.method === "PUT" && x.path.endsWith("/domains/records"));
    assert.ok(put, "the record is updated");
    assert.equal(put.body.override_existing_origin, true);
    assert.equal(put.body.override_existing_dns_record, false);
    const cd = sb.cf().customDomains.find((x) => x.hostname === "backyard.example.com");
    assert.equal(cd.service, "backyard");
    assert.equal(cd.previews_enabled, undefined);
    assert.equal(sb.state().domain.status, "active");
  });

  it("refuses another Worker's custom domain and existing DNS records unless told to replace them, and fails closed", async () => {
    // DNS records at the host (a website): refused, then replaced on request.
    let sb = await newSandbox();
    sb.updateCf((s) => s.dnsRecords.push(dnsRecord("rec-www", "A", "www.example.com")));
    let r = await sb.run(["domain", "add", "www.example.com", "--wait", "0"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /www\.example\.com already has DNS records \(a website\?\): nothing was changed\. Re-run with --replace-dns-record/);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    assert.equal(sb.state().domain, undefined);
    r = await sb.run(["domain", "add", "www.example.com", "--replace-dns-record", "--yes", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    let puts = sb.requests().filter((x) => x.method === "PUT");
    assert.deepEqual(puts.map((x) => x.body), [
      { override_scope: true, override_existing_origin: false, override_existing_dns_record: true, origins: [{ hostname: "www.example.com", zone_id: ZONE.id }] },
    ]);
    assert.deepEqual(sb.cf().dnsRecords, []);
    assert.equal(sb.state().domain.host, "www.example.com");

    // Another Worker's custom domain: --take-over moves it (--yes alone does not).
    sb = await newSandbox();
    sb.updateCf((s) => s.customDomains.push(customDomain("cd-taken", "taken.example.com", "other")));
    r = await sb.run(["domain", "add", "taken.example.com", "--yes", "--wait", "0"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /taken\.example\.com is a custom domain of Worker other: nothing was changed\. Re-run with --take-over to move it to Worker backyard/);
    let reqs = sb.requests();
    assert.ok(reqs.some((x) => x.method === "GET" && x.path === "/accounts/acc1/workers/domains/records/cd-taken"));
    assert.deepEqual(writesOf(reqs), []);
    assert.equal(sb.cf().customDomains[0].service, "other");
    r = await sb.run(["domain", "add", "taken.example.com", "--take-over", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    puts = sb.requests().filter((x) => x.method === "PUT");
    assert.deepEqual(puts.map((x) => x.body), [
      { override_scope: true, override_existing_origin: true, override_existing_dns_record: false, origins: [{ hostname: "taken.example.com", zone_id: ZONE.id }] },
    ]);
    assert.equal(sb.cf().customDomains[0].service, "backyard");

    // A failed check attaches nothing.
    sb = await newSandbox();
    sb.fake.addFault({ method: "POST", path: "/accounts/acc1/workers/scripts/backyard/domains/changeset", status: 500, code: 10013 });
    r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /could not check backyard\.example\.com with Cloudflare \(nothing was changed\)/);
    reqs = sb.requests();
    assert.equal(reqs.filter(isChangeset).length, 1, "a POST is never retried blindly");
    assert.deepEqual(writesOf(reqs), []);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    assert.deepEqual(sb.cf().customDomains, []);
    assert.equal(sb.state().domain, undefined);

    // A changeset that does not mention the host: the custom-domain list decides.
    sb = await newSandbox();
    sb.updateCf((s) => s.customDomains.push(customDomain("cd-taken", "taken.example.com", "other"), customDomain("cd-hub", "backyard.example.com")));
    const empty = { success: true, errors: [], messages: [], result: { added: [], removed: [], updated: [], conflicting: [] } };
    sb.fake.addFault({ method: "POST", path: "*/domains/changeset", status: 200, body: empty });
    r = await sb.run(["domain", "add", "taken.example.com", "--wait", "0"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /taken\.example\.com is a custom domain of Worker other: nothing was changed/);
    reqs = sb.requests();
    assert.ok(reqs.some((x) => x.path === "/accounts/acc1/workers/domains/records" && x.query.hostname === "taken.example.com"));
    assert.deepEqual(writesOf(reqs), []);
    sb.fake.addFault({ method: "POST", path: "*/domains/changeset", status: 200, body: empty });
    r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()), [], "already this Worker's: no PUT");
    assert.equal(sb.state().domain.customDomainId, "cd-hub");
  });

  it("host mode with the admin token: route, wildcard record, certificate check and URL_MODE=host; back to path mode removes them", async () => {
    const sb = await newSandbox();
    const env = { BACKYARD_CF_API_TOKEN: ADMIN };
    let r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--yes", "--wait", "5"], env);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Universal SSL covers only example\.com and \*\.example\.com; sites at \*\.sites\.example\.com need an Advanced Certificate/);
    const reqs = sb.requests();
    const routePost = reqs.filter((x) => x.path === "/zones/zone-example/workers/routes" && x.method === "POST");
    assert.deepEqual(routePost.map((x) => [x.body, x.token]), [[{ pattern: "*.sites.example.com/*", script: "backyard" }, "oauth"]]);
    const dns = reqs.filter((x) => x.path.startsWith("/zones/zone-example/dns_records"));
    assert.deepEqual(dns.map((x) => `${x.method} ${x.token}`), ["GET admin", "POST admin"], "only the admin token touches DNS");
    assert.deepEqual(dns[0].query, { type: "AAAA", name: "*.sites.example.com" });
    assert.deepEqual(dns[1].body, { type: "AAAA", name: "*.sites.example.com", content: "100::", proxied: true, ttl: 1, comment: "Backyard sites (Worker backyard)" });
    const cf = sb.cf();
    const st = sb.state();
    assert.equal(st.domain.mode, "host");
    assert.equal(st.domain.status, "active");
    assert.equal(st.domain.routeId, cf.routes[0].id);
    assert.equal(st.domain.dnsRecordId, cf.dnsRecords[0].id);
    // The certificate check reached a fresh site host.
    assert.ok(sb.fake.edgeRequests.some((x) => /^byd-probe-[0-9a-f]{8}\.sites\.example\.com$/.test(x.host) && x.path === "/__backyard/healthz" && x.status === 200));
    let d = deployOf(sb.calls());
    assert.equal(d.config.vars.BASE_DOMAIN, "sites.example.com");
    assert.equal(d.config.vars.URL_MODE, "host");
    assert.match(r.out, /Sites: https:\/\/<site>\.sites\.example\.com\//);

    // Back to path mode on the same host: the deploy switches the links first,
    // then the route and the record this tool created go.
    trackDeploys(sb);
    const before = sb.cf().workers.backyard.deployments;
    r = await sb.run(["domain", "add", "sites.example.com", "--wait", "0"], env);
    assert.equal(r.code, 0, r.out);
    const writes = writesOf(sb.requests());
    assert.deepEqual(writes.map((x) => `${x.method} ${x.path} ${x.token}`), [
      `DELETE /zones/zone-example/workers/routes/${st.domain.routeId} oauth`,
      `DELETE /zones/zone-example/dns_records/${st.domain.dnsRecordId} admin`,
    ]);
    assert.ok(writes.every((x) => x.deploys === before + 1), "deployed before the deletes");
    d = deployOf(sb.calls());
    assert.equal(d.config.vars.URL_MODE, "path");
    assert.equal(d.config.vars.BASE_DOMAIN, "sites.example.com");
    const st2 = sb.state();
    assert.equal(st2.domain.mode, "path");
    assert.equal(st2.domain.status, "active");
    assert.equal(st2.domain.routeId, undefined);
    assert.equal(st2.domain.dnsRecordId, undefined);
    assert.deepEqual(sb.cf().routes, []);
    assert.deepEqual(sb.cf().dnsRecords, []);
  });

  it("asks before host mode at a zone apex or below it, and exits 2 without --yes", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["domain", "add", "example.com", "--host-mode", "--wait", "0"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /the route \*\.example\.com\/\* sends every proxied hostname of example\.com \(www, api, …\) to Backyard; prefer a dedicated zone/);
    assert.match(r.out, /needs confirmation: re-run with --yes \(Use host mode on example\.com\?\)/);
    r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--wait", "0"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /Universal SSL covers only example\.com and \*\.example\.com; sites at \*\.sites\.example\.com need an Advanced Certificate \(paid; dashboard: SSL\/TLS → Edge Certificates → Order, or the Terraform module's advanced_certificate\)/);
    assert.equal(sb.fake.writes().length, 0);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    assert.equal(sb.state().domain, undefined);
  });

  it("refuses another Worker's *.<host>/* route unless --take-over, and writes no record when *.<host> already resolves", async () => {
    const sb = await newSandbox();
    sb.updateCf((s) => {
      s.routes.push({ id: "route-other", zone_id: ZONE.id, pattern: "*.sites.example.com/*", script: "other" });
      s.dns["*.sites.example.com"] = "resolve";
    });
    let r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--yes", "--wait", "0"], { BACKYARD_CF_API_TOKEN: ADMIN });
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /the route \*\.sites\.example\.com\/\* runs Worker other: nothing was changed\. Re-run with --take-over to move it to Worker backyard/);
    assert.deepEqual(writesOf(sb.requests()), [], "refused before the custom domain is attached");
    assert.deepEqual(sb.cf().customDomains, []);
    assert.equal(sb.state().domain, undefined);
    r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--yes", "--take-over", "--wait", "0"], { BACKYARD_CF_API_TOKEN: ADMIN });
    assert.equal(r.code, 0, r.out);
    const reqs = sb.requests();
    const writes = writesOf(reqs);
    assert.deepEqual(writes.map((x) => `${x.method} ${x.path}`), ["PUT /accounts/acc1/workers/scripts/backyard/domains/records", "PUT /zones/zone-example/workers/routes/route-other"]);
    assert.equal(writes[0].body.override_existing_origin, true);
    assert.deepEqual(writes[1].body, { pattern: "*.sites.example.com/*", script: "backyard" });
    assert.ok(!reqs.some((x) => x.path.includes("/dns_records")), "DNS already resolves: no record is read or written");
    assert.equal(sb.cf().routes[0].script, "backyard");
    const st = sb.state();
    assert.equal(st.domain.routeId, "route-other");
    assert.equal(st.domain.dnsRecordId, undefined);
    assert.equal(st.domain.status, "active");
    assert.equal(deployOf(sb.calls()).config.vars.URL_MODE, "host");
  });

  it("without a token prints the wildcard record, checks once, keeps path URLs (exit 4), and finishes on a re-run", async () => {
    const sb = await newSandbox();
    const env = { BACKYARD_CF_RESULT_FILE: sb.resultPath };
    // The default --wait (300 s): a non-interactive run must not poll for the record.
    let r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--yes"], env);
    assert.equal(r.code, 4, r.out);
    assert.match(r.out, /Your wrangler login cannot create DNS records\. Add this record in the dashboard \(https:\/\/dash\.cloudflare\.com\/acc1\/example\.com\/dns\/records\):/);
    assert.match(r.out, /Type AAAA · Name \*\.sites · IPv6 address 100:: · Proxy status Proxied/);
    assert.match(r.out, /BACKYARD_CF_API_TOKEN=<token with Zone · DNS · Edit> npm run cf -- domain add sites\.example\.com --host-mode/);
    assert.match(
      r.out,
      /\*\.sites\.example\.com does not resolve yet: the hub works on https:\/\/sites\.example\.com\/; sites switch to https:\/\/<site>\.sites\.example\.com\/ once DNS and the certificate are ready: re-run `npm run cf -- domain add sites\.example\.com --host-mode`/,
    );
    assert.match(r.out, /Already applied: custom domain sites\.example\.com attached to Worker backyard; route \*\.sites\.example\.com\/\* created/);
    // A single check: one fresh name, AAAA then A.
    assert.equal(new Set(sb.fake.dohRequests.map((x) => x.name)).size, 1);
    assert.match(sb.fake.dohRequests[0].name, /^byd-probe-[0-9a-f]{8}\.sites\.example\.com$/);
    assert.deepEqual(sb.fake.dohRequests.map((x) => x.type), ["AAAA", "A"]);
    assert.ok(!sb.requests().some((x) => x.path.includes("/dns_records")), "wrangler's login has no DNS permission: nothing is tried");
    let d = deployOf(sb.calls());
    assert.equal(d.config.vars.BASE_DOMAIN, "sites.example.com");
    assert.equal(d.config.vars.URL_MODE, "path");
    let st = sb.state();
    assert.equal(st.domain.status, "pending-dns");
    assert.equal(st.domain.mode, "host");
    assert.ok(st.domain.routeId);
    assert.equal(st.url, "https://sites.example.com");
    assert.equal(sb.result().url, "https://sites.example.com");
    assert.equal(sb.result().previousUrl, WD);
    assert.match(r.out, /Sites: https:\/\/sites\.example\.com\/s\/<site>\//);

    // The person adds the record in the dashboard; the re-run (no --yes needed now) finishes.
    sb.updateCf((s) => s.dnsRecords.push(dnsRecord("rec-by-hand", "AAAA", "*.sites.example.com")));
    r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--wait", "0"], env);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()), [], "an idempotent re-run writes nothing");
    d = deployOf(sb.calls());
    assert.equal(d.config.vars.URL_MODE, "host");
    st = sb.state();
    assert.equal(st.domain.status, "active");
    assert.equal(st.domain.dnsRecordId, undefined, "a record this tool did not create is not its own");
    assert.match(r.out, /Sites: https:\/\/<site>\.sites\.example\.com\//);
  });

  it("keeps path URLs while the wildcard certificate is missing, and refuses an unproxied wildcard record", async () => {
    let sb = await newSandbox();
    sb.updateCf((s) => (s.dns["*.sites.example.com"] = "resolve"));
    // The hub's own certificate works (Workers custom domain); only *.sites.example.com is not covered.
    let r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--yes", "--wait", "0"], { FAKE_TLS_FAIL: "*.sites.example.com", BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 4, r.out);
    assert.match(r.out, /the certificate does not cover \*\.sites\.example\.com yet \(see the warning above\)/);
    assert.match(r.out, /https:\/\/sites\.example\.com is ready/, "the hub answers while the sites' certificate is pending");
    assert.doesNotMatch(r.out, /does not answer yet/);
    assert.equal(sb.state().domain.status, "pending-certificate");
    assert.equal(deployOf(sb.calls()).config.vars.URL_MODE, "path");
    assert.equal(sb.result().command, "domain add", "the result file is written for exit 4 too");

    sb = await newSandbox();
    sb.updateCf((s) => {
      s.dnsRecords.push(dnsRecord("rec-dns-only", "AAAA", "*.sites.example.com", { proxied: false }));
      s.dns["*.sites.example.com"] = "nxdomain";
    });
    r = await sb.run(["domain", "add", "sites.example.com", "--host-mode", "--yes", "--wait", "0"], { BACKYARD_CF_API_TOKEN: ADMIN });
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /\*\.sites\.example\.com exists but is not proxied/);
    assert.ok(!sb.requests().some((x) => x.method === "POST" && x.path.includes("/dns_records")));
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
  });

  it("refuses another host, a domain change while Access is on and externally managed variables; needs a deployed Worker", async () => {
    const sb = await newSandbox({ state: { url: "https://old.example.com", domain: pathDomain("old.example.com") } });
    let r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /Backyard is on old\.example\.com: run `npm run cf -- domain remove` first \(nothing was changed\)/);

    // Cloudflare Access is on: a mode change is refused, the same host and mode re-verifies.
    seedWorker(sb, { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme.cloudflareaccess.com", ACCESS_AUD: AUD, BASE_DOMAIN: "backyard.example.com" });
    sb.updateCf((s) => s.customDomains.push(customDomain("cd-hub", "backyard.example.com")));
    sb.writeState("backyard", { ...BASE_STATE, url: "https://backyard.example.com", domain: pathDomain("backyard.example.com"), access: { team: "acme.cloudflareaccess.com", aud: AUD, source: "manual" } });
    r = await sb.run(["domain", "add", "backyard.example.com", "--host-mode", "--yes", "--wait", "0"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /Cloudflare Access is on: run `npm run cf -- access disable` first/);
    r = await sb.run(["domain", "add", "other.example.com", "--wait", "0"]);
    assert.equal(r.code, 3, r.out);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    const d = deployOf(sb.calls());
    assert.equal(d.config.vars.AUTH_MODE, "access");
    assert.equal(d.config.vars.ACCESS_AUD, AUD);
    assert.equal(sb.state().access.aud, AUD);

    // Set with --var (the Terraform module's setup_args): exit 2.
    sb.writeState("backyard", { ...BASE_STATE, vars: { BASE_DOMAIN: "backyard.example.com", URL_MODE: "path" } });
    r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 2, r.out);
    assert.match(
      r.out,
      /BASE_DOMAIN and URL_MODE are set with --var \(e\.g\. by the Terraform module\): drop them with `npm run cf -- setup --var BASE_DOMAIN: --var URL_MODE:`, or keep managing them with Terraform/,
    );

    // No Worker yet.
    sb.writeState("backyard", BASE_STATE);
    sb.updateCf((s) => delete s.workers.backyard);
    r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /Worker backyard does not exist yet: run npm run cf -- setup first/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    assert.deepEqual(writesOf(sb.fake.requests), []);
  });

  it("names what was already applied when the deploy fails, and a re-run resumes", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"], { FAKE_DEPLOY_FAIL: "1" });
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /wrangler deploy failed/);
    assert.match(r.out, /Already applied: custom domain backyard\.example\.com attached to Worker backyard\. Re-running the same command resumes\./);
    assert.equal(sb.state().domain.host, "backyard.example.com");
    assert.equal(sb.state().url, WD, "the hub URL moves only after a deploy");
    sb.requests();
    r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()), [], "already attached");
    assert.equal(deployOf(sb.calls()).config.vars.BASE_DOMAIN, "backyard.example.com");
    assert.equal(sb.state().url, "https://backyard.example.com");
  });

  it("refuses to attach anything when the deploy would turn a live Cloudflare Access off", async () => {
    const sb = await newSandbox();
    seedWorker(sb, { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme.cloudflareaccess.com", ACCESS_AUD: AUD });
    const r = await sb.run(["domain", "add", "backyard.example.com", "--wait", "0"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /Worker backyard is live with Cloudflare Access \(team acme\.cloudflareaccess\.com\), and this deploy would turn it off/);
    assert.ok(r.out.includes(`delete ${sb.statePath()} to adopt the live configuration`), r.out);
    assert.deepEqual(sb.fake.requests.filter((x) => x.method !== "GET"), [], "not even the changeset");
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
  });

  it("polls DNS on a terminal until the record shows up, without Enter", { skip: !ttyAvailable() && "needs util-linux script" }, async () => {
    const sb = await newSandbox();
    // The person adds the record in the dashboard once the first check has failed.
    const timer = setInterval(() => {
      if (sb.fake.dohRequests.length < 2) return;
      clearInterval(timer);
      sb.updateCf((s) => s.dnsRecords.push(dnsRecord("rec-by-hand", "AAAA", "*.sites.example.com")));
    }, 50);
    const r = await sb.runTTY(["domain", "add", "sites.example.com", "--host-mode", "--wait", "30"], {}, {
      answers: [
        [/Use host mode on sites\.example\.com\? \[y\/N\]/, "y\r"],
        [/How do you want to continue\? \[1-2\]/, "1\r"],
      ],
    });
    clearInterval(timer);
    sb.outputs.push(r.out);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /This step needs an API token with Zone · DNS · Edit/);
    assert.match(r.out, /Your wrangler login cannot create DNS records/);
    assert.match(r.out, /no need to press Enter/);
    assert.ok(new Set(sb.fake.dohRequests.map((x) => x.name)).size >= 2, "a fresh name for every check");
    assert.equal(sb.state().domain.status, "active");
    assert.equal(deployOf(sb.calls()).config.vars.URL_MODE, "host");
  });
  it("changes nothing when the host-mode confirmation is answered No (exit 3)", { skip: !ttyAvailable() && "needs util-linux script" }, async () => {
    const sb = await newSandbox();
    sb.requests();
    const r = await sb.runTTY(["domain", "add", "sites.example.com", "--host-mode", "--wait", "0"], {}, {
      answers: [[/Use host mode on sites\.example\.com\? \[y\/N\]/, "n\r"]],
    });
    sb.outputs.push(r.out);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /not confirmed: nothing was changed/);
    assert.deepEqual(writesOf(sb.requests()), []);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
  });
});

describe("backyard-cloudflare domain remove", () => {
  it("deploys without the domain first, then deletes the route, the record it created and the custom domain", async () => {
    const sb = await newSandbox({
      state: {
        url: "https://sites.example.com",
        domain: { host: "sites.example.com", mode: "host", zoneId: ZONE.id, zoneName: ZONE.name, customDomainId: "cd-sites", routeId: "route-sites", dnsRecordId: "rec-sites", status: "active", source: "cli" },
      },
    });
    seedHostDomain(sb);
    trackDeploys(sb);
    const before = sb.cf().workers.backyard.deployments;
    const r = await sb.run(["domain", "remove", "--yes"], { BACKYARD_CF_API_TOKEN: ADMIN, BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Remove sites\.example\.com from Worker backyard\? The hub moves back to https:\/\/backyard\.acme\.workers\.dev yes \(--yes\)/);
    const d = deployOf(sb.calls());
    assert.equal(d.config.vars.BASE_DOMAIN, "");
    assert.equal(d.config.vars.URL_MODE, "path");
    const writes = writesOf(sb.requests());
    assert.deepEqual(writes.map((x) => `${x.method} ${x.path} ${x.token}`), [
      "DELETE /zones/zone-example/workers/routes/route-sites oauth",
      "DELETE /zones/zone-example/dns_records/rec-sites admin",
      "DELETE /accounts/acc1/workers/domains/cd-sites oauth",
    ]);
    assert.ok(writes.every((x) => x.deploys === before + 1), "the deploy without the domain comes first");
    // The detach answers 200 without a body (like the live API): a success, no manual step.
    assert.equal(writes[2].status, 200);
    assert.match(r.out, /Custom domain sites\.example\.com detached from Worker backyard/);
    assert.doesNotMatch(r.out, /could not detach/);
    // Foreign objects stay.
    const cf = sb.cf();
    assert.deepEqual(cf.customDomains.map((x) => x.id), ["cd-foreign"]);
    assert.deepEqual(cf.routes.map((x) => x.id), ["route-foreign"]);
    assert.deepEqual(cf.dnsRecords.map((x) => x.id), ["rec-www"]);
    const st = sb.state();
    assert.equal(st.domain, undefined);
    assert.equal(st.url, WD);
    assert.equal(st.deployToken, TOKEN);
    assert.deepEqual(sb.result(), {
      schema: 1,
      command: "domain remove",
      worker: "backyard",
      statePath: sb.statePath(),
      url: WD,
      previousUrl: "https://sites.example.com",
      workersDevUrl: WD,
      authMode: "none",
      deployToken: "state",
    });
    assert.match(r.out, /sites\.example\.com removed: the hub is on https:\/\/backyard\.acme\.workers\.dev again/);
  });

  it("deletes the wildcard record only with the admin token, and prints it otherwise", async () => {
    const sb = await newSandbox({
      state: {
        url: "https://sites.example.com",
        domain: { host: "sites.example.com", mode: "host", zoneId: ZONE.id, zoneName: ZONE.name, routeId: "route-sites", dnsRecordId: "rec-sites", status: "active", source: "cli" },
      },
    });
    seedHostDomain(sb);
    const r = await sb.run(["domain", "remove", "--yes"]);
    assert.equal(r.code, 0, r.out);
    assert.deepEqual(writesOf(sb.requests()).map((x) => `${x.method} ${x.path}`), [
      "DELETE /zones/zone-example/workers/routes/route-sites",
      "DELETE /accounts/acc1/workers/domains/cd-sites",
    ]);
    assert.match(
      r.out,
      /Delete the DNS record AAAA \*\.sites\.example\.com in the dashboard \(https:\/\/dash\.cloudflare\.com\/acc1\/example\.com\/dns\/records\) \(this tool created it; with BACKYARD_CF_API_TOKEN=<token with Zone · DNS · Edit> it deletes it itself\)/,
    );
    assert.deepEqual(sb.cf().dnsRecords.map((x) => x.id).sort(), ["rec-sites", "rec-www"]);
    assert.equal(sb.state().domain, undefined);
  });

  it("leaves a route alone that now runs another Worker", async () => {
    const sb = await newSandbox({
      state: { url: "https://sites.example.com", domain: { host: "sites.example.com", mode: "host", zoneId: ZONE.id, zoneName: ZONE.name, routeId: "route-sites", status: "active", source: "cli" } },
    });
    seedHostDomain(sb);
    sb.updateCf((s) => (s.routes.find((x) => x.id === "route-sites").script = "other"));
    const r = await sb.run(["domain", "remove", "--yes"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /The route \*\.sites\.example\.com\/\* now runs Worker other: left alone/);
    assert.deepEqual(writesOf(sb.requests()).map((x) => `${x.method} ${x.path}`), ["DELETE /accounts/acc1/workers/domains/cd-sites"]);
    assert.equal(sb.cf().routes.find((x) => x.id === "route-sites").script, "other");
  });

  it("refuses while Access is on or workers.dev is off, needs --yes without a terminal, and has nothing to do without a domain", async () => {
    const sb = await newSandbox();
    let r = await sb.run(["domain", "remove", "--yes"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /No domain is managed by this tool for Worker backyard: nothing to do/);
    sb.writeState("backyard", { ...BASE_STATE, vars: { BASE_DOMAIN: "backyard.example.com" } });
    r = await sb.run(["domain", "remove", "--yes"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /BASE_DOMAIN is set with --var \(e\.g\. by the Terraform module\): drop it with `npm run cf -- setup --var BASE_DOMAIN:`, or keep managing it with Terraform/);
    const domain = pathDomain("backyard.example.com");
    sb.writeState("backyard", { ...BASE_STATE, domain, access: { team: "acme.cloudflareaccess.com", aud: AUD, source: "manual" } });
    r = await sb.run(["domain", "remove", "--yes"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /Cloudflare Access protects backyard\.example\.com: run `npm run cf -- access disable` first \(nothing was changed\)/);
    sb.writeState("backyard", { ...BASE_STATE, domain, workersDev: false });
    r = await sb.run(["domain", "remove", "--yes"]);
    assert.equal(r.code, 3, r.out);
    assert.match(r.out, /turn workers\.dev back on first: `npm run cf -- setup --workers-dev`/);
    sb.writeState("backyard", { ...BASE_STATE, domain });
    r = await sb.run(["domain", "remove"]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.out, /needs confirmation: re-run with --yes \(Remove backyard\.example\.com from Worker backyard\? The hub moves back to https:\/\/backyard\.acme\.workers\.dev\)/);
    assert.ok(!sb.calls().some((x) => x.args[0] === "deploy"));
    assert.deepEqual(sb.fake.writes(), []);
    assert.deepEqual(sb.state().domain, domain, "unchanged");
  });

  it("removes an adopted domain through lookups on a machine without state", async () => {
    const sb = await newSandbox({ state: null });
    sb.updateCf((s) => {
      s.customDomains.push(customDomain("cd-sites", "sites.example.com"));
      s.routes.push({ id: "route-sites", zone_id: ZONE.id, pattern: "*.sites.example.com/*", script: "backyard" });
    });
    seedWorker(sb, { BASE_DOMAIN: "sites.example.com", URL_MODE: "host" });
    const r = await sb.run(["domain", "remove", "--yes"], { BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /Adopted the deployed configuration of backyard \(no setup state on this machine\): domain sites\.example\.com \(host mode\)/);
    const reqs = sb.requests();
    assert.ok(reqs.some((x) => x.method === "GET" && x.path === "/accounts/acc1/workers/domains/records" && x.query.service === "backyard"));
    assert.ok(!reqs.some((x) => x.path === "/zones"), "the zone comes from the custom domain");
    assert.deepEqual(writesOf(reqs).map((x) => `${x.method} ${x.path}`), [
      "DELETE /zones/zone-example/workers/routes/route-sites",
      "DELETE /accounts/acc1/workers/domains/cd-sites",
    ]);
    assert.match(r.out, /if you added it for Backyard \(this tool did not create it, so it leaves it alone\)/);
    assert.equal(deployOf(sb.calls()).config.vars.BASE_DOMAIN, "");
    const st = sb.state();
    assert.equal(st.domain, undefined);
    assert.equal(st.url, WD);
    assert.equal(st.workersDevUrl, WD);
    assert.equal(sb.result().previousUrl, "");
    assert.equal(sb.result().url, WD);
  });

  it("ends with exit 4 and the dashboard step when the custom domain cannot be detached", async () => {
    const sb = await newSandbox({ state: { url: "https://backyard.example.com", domain: pathDomain("backyard.example.com") } });
    sb.updateCf((s) => s.customDomains.push(customDomain("cd-hub", "backyard.example.com")));
    seedWorker(sb, { BASE_DOMAIN: "backyard.example.com" });
    sb.fake.addFault({ method: "DELETE", path: "/accounts/acc1/workers/domains/*", status: 400, code: 100100, message: "fake detach failure" });
    const r = await sb.run(["domain", "remove", "--yes"], { BACKYARD_CF_RESULT_FILE: sb.resultPath });
    assert.equal(r.code, 4, r.out);
    assert.match(r.out, /could not detach backyard\.example\.com .*fake detach failure.*: remove it in the dashboard \(Workers & Pages → backyard → Settings → Domains & Routes → remove backyard\.example\.com\)/);
    assert.equal(deployOf(sb.calls()).config.vars.BASE_DOMAIN, "", "the Worker no longer uses it");
    assert.equal(sb.state().domain, undefined);
    assert.equal(sb.state().url, WD);
    assert.equal(sb.result().command, "domain remove");
  });
});

describe("domain helpers", () => {
  it("normalizes host names, lists zone candidates and names the wildcard record", () => {
    assert.equal(parseHost("Backyard.Example.COM."), "backyard.example.com");
    assert.equal(parseHost("xn--bcher-kva.example"), "xn--bcher-kva.example");
    for (const bad of ["", "http://a.example.com", "a.example.com:443", "a.example.com/x", "*.a.example.com", "example", "a..example.com", "-a.example.com", "1.2.3.4", "x.workers.dev", "workers.dev"]) {
      assert.throws(() => parseHost(bad), (e) => e.code === 2, bad);
    }
    assert.deepEqual(zoneCandidates("a.b.example.co.uk"), ["a.b.example.co.uk", "b.example.co.uk", "example.co.uk", "co.uk"]);
    assert.deepEqual(zoneCandidates("example.com"), ["example.com"]);
    assert.equal(registrableDomain("hub.other.co.uk"), "other.co.uk");
    assert.equal(registrableDomain("backyard.acme.com"), "acme.com");
    assert.equal(registrableDomain("a.b.acme.dev"), "acme.dev");
    assert.equal(dnsRecordName("example.com", "example.com"), "*");
    assert.equal(dnsRecordName("sites.example.com", "example.com"), "*.sites");
    assert.equal(dnsRecordName("a.b.example.com", "example.com"), "*.a.b");
  });
});
