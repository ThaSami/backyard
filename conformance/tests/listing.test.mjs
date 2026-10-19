// Site listing (SPEC §7.2 "Listing", with §2, §3, §7.1 and §14.1): which
// sites the site directories — the hub's GET /__backyard/api/sites and its
// preview route, GET {gateway}/sites and the connector's backyard_list_sites —
// return to whom.
//
// The file tests the mode /__backyard/api/info reports as site_listing;
// BACKYARD_CONFORMANCE_LISTING (the runners: "owners" in their listing pass,
// "all" in every other) makes any other value a failure:
//
// * "all" (the default): a site is listed to everyone, whoever deployed it.
// * "owners" (sites.listing: owners, SITE_LISTING=owners): a site is listed
//   only to its owners, to its creator while it has none, and to admins —
//   never to a guest; the lookups by name still answer for everyone (listing
//   is not access control).
//
// Identities: Backyard API tokens of the suite's own people, signed with the
// server's secret (BACKYARD_CONFORMANCE_SECRET, which the runners set; without
// it these tests are skipped); a client without credentials, which is a guest
// in auth mode none; and with "owners" the admin BACKYARD_CONFORMANCE_ADMIN
// names (an email or id the server lists in auth.admins / ADMINS) and
// BACKYARD_TOKEN, whichever /me says it is.
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { after, before, describe, test } from "node:test";
import { createDeploy, deleteSiteQuietly, deploySite, fileEntries } from "../lib/deploy.mjs";
import { token as envToken, hubURL, setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";
import { MCPClient } from "../lib/mcp.mjs";
import * as oauth from "../lib/oauth.mjs";
import { assertSiteMeta, assertUser } from "../lib/shapes.mjs";
import { signAPIToken } from "../lib/tokens.mjs";

const env = await setup();
const listing = env.info.site_listing;
const expected = (process.env.BACKYARD_CONFORMANCE_LISTING || "").trim();
const secret = (process.env.BACKYARD_CONFORMANCE_SECRET || "").trim();
const adminName = (process.env.BACKYARD_CONFORMANCE_ADMIN || "").trim();
const authMode = String(env.info.auth?.mode || "");
const noSecret = !secret && "no BACKYARD_CONFORMANCE_SECRET to sign API tokens of the suite's people";
// A client without credentials is a guest only in auth mode none.
const guestSkip = authMode !== "none" && `a client without credentials is no guest in auth mode ${authMode}`;
const tag = randomBytes(3).toString("hex");

const svg = Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" width="64" height="36"><rect width="64" height="36" fill="#16a34a"/></svg>\n');
const page = (site) => `<!doctype html><html><head><title>Listing ${site}</title></head><body>${site}</body></html>\n`;
/** The files of every test site: a page and a preview (SPEC §7.2 rule 2). */
const files = (site) => ({ "index.html": page(site), "preview.svg": svg });

const previewURL = (site) => `${hubURL}/__backyard/api/sites/${site}/preview`;
const LISTS = [
  ["GET {gateway}/sites", () => `${env.gateway}/sites`],
  ["GET /__backyard/api/sites", () => `${hubURL}/__backyard/api/sites`],
];

/** A person of the suite: who (for messages), {id, email, name} and a client with their API token (SPEC §8.3). */
function person(who, { id = `sub:cf-${who}-${tag}`, email = `${who}-${tag}@conformance.test` } = {}) {
  const user = { id, email, name: `Conformance ${who}` };
  return { who, ...user, client: new Client({ token: signAPIToken(secret, user) }) };
}

const same = (a, b) => typeof a === "string" && typeof b === "string" && a !== "" && a.toLowerCase() === b.toLowerCase();

/**
 * Whether sites.listing: owners lists m to the signed-in non-admin p (SPEC
 * §7.2): an owner by id or email, or, while the site has no owners, its
 * creator — both case-insensitively.
 */
function listedTo(m, p) {
  if (m.owners.length) return m.owners.some((o) => same(o, p.id) || same(o, p.email));
  return same(m.created_by, p.id) || same(m.created_by, p.email);
}

/** Both site lists of client, asserted to be {"sites": [SiteMeta…]}: {where: sites}. */
async function siteLists(client, what) {
  const out = {};
  for (const [where, url] of LISTS) {
    const r = expectStatus(await client.get(url()), 200, `${what}: ${where}`);
    const b = r.json;
    assert.deepEqual(Object.keys(b), ["sites"], `${what}: ${where} has the shape {"sites": [SiteMeta]}: ${r}`);
    assert.ok(Array.isArray(b.sites), `${what}: ${where}: sites is an array: ${r}`);
    for (const m of b.sites) assert.ok(typeof m.name === "string" && Array.isArray(m.owners), `${what}: ${where}: a SiteMeta: ${JSON.stringify(m)}`);
    out[where] = b.sites;
  }
  return out;
}

/** The names of mine (sorted) that sites holds. */
const among = (sites, mine) => sites.map((m) => m.name).filter((n) => mine.includes(n)).sort();

/**
 * A Claude connector connection (SPEC §8.4) of a new browser — a guest in auth
 * mode none —: its MCP client (§14.1).
 */
async function connectGuest() {
  const { P, R } = oauth.connectorURLs(env.info.mcp_url);
  const client = await oauth.registerClient(P, { name: `Conformance listing ${tag}` });
  const redirectURI = client.redirect_uris[0];
  const { verifier, challenge } = oauth.pkce();
  const params = oauth.authorizeParams({ client, redirectURI, challenge, state: oauth.newState(), resource: R, scope: "mcp" });
  const a = await oauth.authorizeAndConsent(new oauth.Browser(), P, params);
  assert.ok(a.code, `a code: ${a.location}`);
  const t = oauth.assertTokens(await oauth.exchangeCode(P, { clientId: client.client_id, code: a.code, verifier, redirectURI, resource: R }), {
    scope: "mcp",
    what: "code exchange",
  });
  return new MCPClient({ endpoint: R, token: t.access_token });
}

test("GET /__backyard/api/info reports site_listing (SPEC §7.1)", () => {
  assert.ok(["all", "owners"].includes(listing), `site_listing is always present, "all" or "owners": ${JSON.stringify(env.info)}`);
  if (expected) assert.equal(listing, expected, `BACKYARD_CONFORMANCE_LISTING=${expected}: the server's site_listing`);
});

describe("site listing: all (SPEC §7.2)", { skip: (listing !== "all" && `site_listing is ${JSON.stringify(listing)}`) || noSecret }, () => {
  const site = uniqueSite("lstall");
  let alice;
  let bob;

  before(async () => {
    alice = person("alice");
    bob = person("bob");
    await deploySite(alice.client, env, site, files(site));
  });

  after(async () => {
    if (alice) await deleteSiteQuietly(alice.client, env, site);
  });

  test("someone else is listed the site and gets its preview", async () => {
    const l = await siteLists(bob.client, "bob");
    for (const [where, sites] of Object.entries(l)) assert.deepEqual(among(sites, [site]), [site], `${where} lists alice's site to bob`);
    const img = expectStatus(await bob.client.get(previewURL(site)), 200, "bob: the preview");
    assert.deepEqual(img.body, svg);
  });

  test("a guest is listed the site and gets its preview", async (t) => {
    if (guestSkip) return t.skip(guestSkip);
    const guest = new Client({ token: null });
    const l = await siteLists(guest, "a guest");
    for (const [where, sites] of Object.entries(l)) assert.deepEqual(among(sites, [site]), [site], `${where} lists alice's site to a guest`);
    expectStatus(await guest.get(previewURL(site)), 200, "a guest: the preview");
  });
});

describe("site listing: owners (SPEC §7.2)", { skip: (listing !== "owners" && `site_listing is ${JSON.stringify(listing)}`) || noSecret }, () => {
  // own: alice's. bobs: bob's, carol added by email (in another case). shared:
  // alice's, carol added by id (stored lowercased; her id is mixed case).
  // orphan: alice's, then without owners (created_by stays alice). guest: a
  // guest's (never owned), when guests may deploy.
  const site = {
    own: uniqueSite("lst-own"),
    bobs: uniqueSite("lst-bob"),
    shared: uniqueSite("lst-shr"),
    orphan: uniqueSite("lst-orph"),
    guest: uniqueSite("lst-gst"),
  };
  const guestsDeploy = !guestSkip && env.info.auth?.anonymous_deploy === true;
  const ours = () => Object.values(site).filter((s) => s !== site.guest || guestsDeploy);
  const guest = new Client({ token: null });
  const meta = {};
  let alice;
  let bob;
  let carol;
  let admin;
  let visible;

  before(async () => {
    alice = person("alice");
    bob = person("bob");
    carol = person("carol", { id: `sub:CF-Carol-${tag}` });
    if (adminName) admin = person("admin", adminName.includes("@") ? { email: adminName } : { id: adminName, email: "" });
    for (const p of [alice, bob, carol, admin].filter(Boolean)) {
      const me = assertUser(expectStatus(await p.client.get(`${env.gateway}/me`), 200, `${p.who}: GET {gateway}/me`).json, `${p.who}`);
      assert.equal(me.id, p.id, `${p.who}: the API token's user`);
      assert.equal(me.anonymous, false, `${p.who} is signed in`);
      assert.equal(me.admin, p === admin, p === admin ? `BACKYARD_CONFORMANCE_ADMIN=${adminName} must be an admin of the server (auth.admins, ADMINS)` : `${p.who} is no admin`);
    }

    await deploySite(alice.client, env, site.own, files(site.own));
    await deploySite(bob.client, env, site.bobs, files(site.bobs));
    await deploySite(alice.client, env, site.shared, files(site.shared));
    await deploySite(alice.client, env, site.orphan, files(site.orphan));
    if (guestsDeploy) await deploySite(guest, env, site.guest, files(site.guest));
    const owners = async (client, s, body) => expectStatus(await client.post(`${env.gateway}/sites/${s}/owners`, body), 200, `owners of ${s}: ${JSON.stringify(body)}`);
    await owners(alice.client, site.shared, { add: [carol.id.toLowerCase()] });
    await owners(bob.client, site.bobs, { add: [carol.email.toUpperCase()] });
    await owners(alice.client, site.orphan, { remove: [alice.id] });
    for (const s of ours()) meta[s] = assertSiteMeta(expectStatus(await alice.client.get(`${env.gateway}/sites/${s}`), 200, s).json, { name: s });
    visible = {
      [alice.who]: [site.own, site.shared, site.orphan],
      [bob.who]: [site.bobs],
      [carol.who]: [site.bobs, site.shared],
    };
  });

  after(async () => {
    for (const s of Object.values(site)) await deleteSiteQuietly(admin ? admin.client : s === site.bobs ? bob.client : alice.client, env, s);
  });

  test("the option implies ownership: the first signed-in deployer owns a site", async () => {
    assert.deepEqual(meta[site.own].owners, [alice.id], `alice owns her site: ${JSON.stringify(meta[site.own])}`);
    assert.equal(meta[site.own].created_by, alice.id);
    assert.deepEqual(meta[site.bobs].owners, [bob.id, carol.email.toLowerCase()], "bob's site: bob, and carol by email (lowercased)");
    assert.deepEqual(meta[site.shared].owners, [alice.id, carol.id.toLowerCase()], "the shared site: alice, and carol by id (lowercased)");
    assert.deepEqual(meta[site.orphan].owners, [], "the orphan has no owners left");
    assert.equal(meta[site.orphan].created_by, alice.id, "the orphan's creator is kept");
    if (guestsDeploy) {
      assert.deepEqual(meta[site.guest].owners, [], "guests never own a site");
      assert.match(meta[site.guest].created_by ?? "", /^anon:/, "a guest created it");
    }
    const entries = fileEntries({ "index.html": "taken over" }).map((f) => f.entry);
    assertError(await createDeploy(bob.client, env, site.own, entries), 403, "forbidden", "bob cannot redeploy alice's site");
  });

  test("a person is listed the sites they own and the ownerless ones they created, nothing else", async () => {
    for (const p of [alice, bob, carol]) {
      const l = await siteLists(p.client, p.who);
      for (const [where, sites] of Object.entries(l)) {
        assert.deepEqual(among(sites, ours()), [...visible[p.who]].sort(), `${where}: the test sites listed to ${p.who}`);
        for (const m of sites) {
          assert.ok(listedTo(m, p), `${where} lists ${m.name} to ${p.who} (${p.id}, ${p.email}), but its owners are ${JSON.stringify(m.owners)} and created_by ${JSON.stringify(m.created_by)}`);
        }
      }
    }
  });

  test("an admin is listed every site", async (t) => {
    if (!admin) return t.skip("no BACKYARD_CONFORMANCE_ADMIN (an admin of the server)");
    const l = await siteLists(admin.client, "the admin");
    for (const [where, sites] of Object.entries(l)) assert.deepEqual(among(sites, ours()), ours().sort(), `${where}: every test site is listed to the admin`);
  });

  test("BACKYARD_TOKEN is listed every site when it is an admin, else what the rules allow", async (t) => {
    if (!envToken) return t.skip("no BACKYARD_TOKEN");
    const c = new Client();
    const me = assertUser(expectStatus(await c.get(`${env.gateway}/me`), 200, "BACKYARD_TOKEN: GET {gateway}/me").json, "BACKYARD_TOKEN");
    const l = await siteLists(c, "BACKYARD_TOKEN");
    for (const [where, sites] of Object.entries(l)) {
      if (me.admin) assert.deepEqual(among(sites, ours()), ours().sort(), `${where}: every test site is listed to BACKYARD_TOKEN (an admin)`);
      else for (const m of sites) assert.ok(!me.anonymous && listedTo(m, me), `${where} lists ${m.name} to ${me.id}: ${JSON.stringify(m.owners)}`);
    }
  });

  test("a guest is listed no site, not even one it deployed", async (t) => {
    if (guestSkip) return t.skip(guestSkip);
    for (const [where, url] of LISTS) {
      const r = expectStatus(await guest.get(url()), 200, `a guest: ${where}`);
      assert.deepEqual(r.json, { sites: [] }, `${where} lists nothing to a guest: ${r}`);
    }
  });

  test("the preview route answers only for a site listed to the caller (else 404 not_found)", async () => {
    assert.equal(meta[site.own].preview, "preview.svg");
    for (const [who, c] of [["alice", alice.client], ["the admin", admin?.client]]) {
      if (!c) continue;
      assert.deepEqual(expectStatus(await c.get(previewURL(site.own)), 200, `${who}: the preview`).body, svg, `${who}: the preview bytes`);
    }
    assert.deepEqual(expectStatus(await carol.client.get(previewURL(site.shared)), 200, "carol: the shared site's preview").body, svg);
    const hidden = [["bob", bob.client], ["carol", carol.client]];
    if (!guestSkip) hidden.push(["a guest", guest]);
    for (const [who, c] of hidden) {
      assertError(await c.get(previewURL(site.own)), 404, "not_found", `${who}: the preview of a site not listed to them`);
      expectStatus(await c.head(previewURL(site.own)), 404, `${who}: HEAD of that preview`);
    }
    assertError(await bob.client.get(previewURL(site.orphan)), 404, "not_found", "bob: the orphan's preview");
  });

  test("listing is not access control: the lookups by name answer for every site", async () => {
    const callers = [["bob", bob.client]];
    if (!guestSkip) callers.push(["a guest", guest]);
    for (const [who, c] of callers) {
      const one = assertSiteMeta(expectStatus(await c.get(`${env.gateway}/sites/${site.own}`), 200, `${who}: GET {gateway}/sites/{site}`).json, {
        name: site.own,
        url: env.canonicalSiteURL(site.own),
        what: `${who}: GET {gateway}/sites/{site}`,
      });
      assert.deepEqual(one.owners, [alice.id], `${who}: the owners are shown`);
      const list = expectStatus(await c.get(`${env.gateway}/sites/${site.own}/files`), 200, `${who}: GET {gateway}/sites/{site}/files`).json;
      assert.deepEqual(list.files.map((f) => f.path).sort(), ["index.html", "preview.svg"], `${who}: the files`);
      assert.equal(expectStatus(await c.get(`${env.gateway}/sites/${site.own}/files/index.html`), 200, `${who}: a raw file`).text, page(site.own));
      assertSiteMeta(expectStatus(await c.get(env.api(site.own, "/site")), 200, `${who}: the site API's GET /site`).json, { name: site.own });
      assert.equal(expectStatus(await c.get(env.siteURL(site.own)), 200, `${who}: the site itself`).text, page(site.own));
    }
  });

  test("the connector's backyard_list_sites counts only the sites listed to the user", async (t) => {
    if (!env.info.mcp_url) return t.skip("the Claude connector is not enabled (/__backyard/api/info has no mcp_url)");
    if (authMode !== "none") return t.skip(`a connection needs a browser identity; the suite carries one only in auth mode none (here ${authMode})`);
    const mcp = await connectGuest();
    const none = { sites: [], total: 0, truncated: false };
    assert.deepEqual(await mcp.callJSON("backyard_list_sites", {}), none, "a guest's connection: nothing listed, and nothing counted");
    assert.deepEqual(await mcp.callJSON("backyard_list_sites", { query: site.own }), none, "also when the query names a site");
    const tree = await mcp.callJSON("backyard_site_files", { site: site.own });
    assert.deepEqual(
      tree.files.map((f) => f.path),
      ["index.html", "preview.svg"],
      "backyard_site_files still answers for a site that is not listed",
    );
  });

  test("a new owner is listed the site at once; a removed one no longer", async () => {
    const has = async (p, s) => {
      const l = await siteLists(p.client, p.who);
      const found = Object.values(l).map((sites) => sites.some((m) => m.name === s));
      assert.equal(found[0], found[1], `both lists agree about ${s} for ${p.who}`);
      return found[0];
    };
    assert.equal(await has(bob, site.own), false);
    expectStatus(await alice.client.post(`${env.gateway}/sites/${site.own}/owners`, { add: [bob.email] }), 200, "add bob");
    assert.equal(await has(bob, site.own), true, "bob, now an owner, is listed alice's site");
    expectStatus(await alice.client.post(`${env.gateway}/sites/${site.own}/owners`, { remove: [bob.email] }), 200, "remove bob");
    assert.equal(await has(bob, site.own), false, "bob, no longer an owner, is not");
  });

  test("an ownerless site's creator loses the listing once someone else's deploy owns it", async () => {
    await deploySite(bob.client, env, site.orphan, files(site.orphan));
    const m = expectStatus(await bob.client.get(`${env.gateway}/sites/${site.orphan}`), 200).json;
    assert.deepEqual(m.owners, [bob.id], "bob's deploy made him the owner (SPEC §7.3)");
    assert.equal(m.created_by, alice.id, "created_by is kept");
    for (const [p, want] of [
      [bob, true],
      [alice, false],
    ]) {
      const l = await siteLists(p.client, p.who);
      for (const [where, sites] of Object.entries(l)) {
        assert.equal(
          sites.some((s) => s.name === site.orphan),
          want,
          `${where}: ${p.who} ${want ? "is" : "is not"} listed the site (created_by counts only while it has no owners)`,
        );
      }
    }
  });
});
