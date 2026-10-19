/**
 * Owners-only site listing (SITE_LISTING, SPEC §7.2 "Listing", §16): the
 * variable and its fail-closed rules (also with OWNERSHIP, which it implies),
 * the shared predicate listedTo (the cases of internal/sites/listing_test.go,
 * so both editions decide alike), and every surface it filters — the hub's
 * GET /__backyard/api/sites and its preview route, the gateway's GET /sites
 * and the connector's backyard_list_sites (counted after filtering) — plus
 * /info's site_listing, for API tokens, the DEPLOY_TOKEN admin, guests and
 * Cloudflare Access identities. Lookups by name are not filtered.
 */
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { resetAccessCaches } from "../src/auth";
import { parseConfig } from "../src/config";
import { b64url } from "../src/crypto";
import type { Env } from "../src/env";
import type { User } from "../src/identity";
import { issueAPIToken } from "../src/jwt";
import { resetMcpCaches } from "../src/oauth";
import type { SiteRecord } from "../src/registry-do";
import { filterListed, listedTo } from "../src/sitemeta";
import { DEPLOY_TOKEN, MCP_ENV, SECRET, anonCookie, apiHeaders, call, deploy, mcpConnect, tokenHeaders, uniqueSite } from "./helpers";

const base = { BACKYARD_SECRET: "x".repeat(32) } as unknown as Env;
const cfgOf = (vars: Record<string, unknown>) => parseConfig({ ...base, ...vars } as unknown as Env);

afterEach(() => {
  vi.unstubAllGlobals();
  resetAccessCaches();
  resetMcpCaches();
});

describe("SITE_LISTING (SPEC §16)", () => {
  it("lists every site by default, and leaves OWNERSHIP as it was without owners", () => {
    const c = parseConfig(base);
    expect(c.siteListing).toBe("all");
    expect(c.ownership).toBe("none");
    expect(c.fatal).toBe("");
    expect(c.problems).toEqual([]);
    for (const v of [undefined, null, "", "all", " ALL ", "All"]) {
      const d = cfgOf({ SITE_LISTING: v });
      expect(d.siteListing, String(v)).toBe("all");
      expect(d.fatal, String(v)).toBe("");
      expect(d.problems, String(v)).toEqual([]);
    }
    // OWNERSHIP is parsed as before the option: "owner" (any case) or none.
    for (const [v, want] of [
      ["owner", "owner"],
      [" OWNER ", "owner"],
      ["none", "none"],
      ["", "none"],
      ["owners", "none"],
      ["bogus", "none"],
    ] as const) {
      for (const listing of [undefined, "all"]) {
        const d = cfgOf({ SITE_LISTING: listing, OWNERSHIP: v });
        expect(d.ownership, `${listing} ${v}`).toBe(want);
        expect(d.fatal, `${listing} ${v}`).toBe("");
      }
    }
  });

  it("owners implies OWNERSHIP=owner (unset, empty or owner, trimmed, any case)", () => {
    for (const listing of ["owners", " Owners ", "OWNERS"]) {
      for (const own of [undefined, null, "", "owner", " Owner "]) {
        const c = cfgOf({ SITE_LISTING: listing, OWNERSHIP: own });
        expect(c.siteListing, `${listing} ${own}`).toBe("owners");
        expect(c.ownership, `${listing} ${own}`).toBe("owner");
        expect(c.fatal, `${listing} ${own}`).toBe("");
        expect(c.problems, `${listing} ${own}`).toEqual([]);
      }
    }
  });

  it("fails closed for a SITE_LISTING other than all or owners (never lists every site)", () => {
    // Includes the Go values of other keys and JSON values, which arrive parsed.
    for (const v of ["owner", "none", "private", "mine", "admins", "yes", "true", "own", "owners,all", true, 1, false]) {
      const c = cfgOf({ SITE_LISTING: v });
      expect(c.fatal, String(v)).toBe('SITE_LISTING must be "all" or "owners"');
      expect(c.problems.join("\n"), String(v)).toContain(`SITE_LISTING must be "all" or "owners" (got ${JSON.stringify(String(v).toLowerCase())})`);
      expect(c.problems.join("\n"), String(v)).toContain("503");
      expect(c.siteListing, String(v)).toBe("owners");
    }
    for (const v of [{ listing: "owners" }, ["owners"]]) {
      const c = cfgOf({ SITE_LISTING: v });
      expect(c.fatal, JSON.stringify(v)).toBe('SITE_LISTING must be "all" or "owners"');
      expect(c.problems.join("\n")).toContain("(got a value that is not a string)");
    }
  });

  it("fails closed for SITE_LISTING=owners with an OWNERSHIP other than owner, naming both", () => {
    for (const own of ["none", " None ", "owners", "bogus", true, 0]) {
      const c = cfgOf({ SITE_LISTING: "owners", OWNERSHIP: own });
      expect(c.fatal, String(own)).toBe("SITE_LISTING=owners requires OWNERSHIP=owner (its default with this option)");
      const p = c.problems.join("\n");
      expect(p, String(own)).toContain(`(got OWNERSHIP=${JSON.stringify(String(own).trim().toLowerCase())}; leave OWNERSHIP unset or set it to owner)`);
      expect(p, String(own)).toContain("503");
    }
    for (const own of [{ mode: "owner" }, ["owner"]]) {
      const c = cfgOf({ SITE_LISTING: "owners", OWNERSHIP: own });
      expect(c.fatal, JSON.stringify(own)).toContain("SITE_LISTING=owners requires OWNERSHIP=owner");
      expect(c.problems.join("\n")).toContain("got OWNERSHIP=a value that is not a string");
    }
    // Several fatal problems are all named.
    const both = cfgOf({ AUTH_MODE: "oidc", SITE_LISTING: "owners", OWNERSHIP: "none" });
    expect(both.fatal).toBe('AUTH_MODE must be "none" or "access"; SITE_LISTING=owners requires OWNERSHIP=owner (its default with this option)');
    expect(cfgOf({ AUTH_MODE: "oidc", SITE_LISTING: "x" }).fatal).toBe('AUTH_MODE must be "none" or "access"; SITE_LISTING must be "all" or "owners"');
  });

  it("refuses every request but the health checks while it is invalid (503), and readyz names the variables", async () => {
    const site = uniqueSite("lst-closed");
    await deploy(site, { "index.html": "secret page" });
    for (const env of [{ SITE_LISTING: "private" }, { SITE_LISTING: "owners", OWNERSHIP: "none" }] as Partial<Env>[]) {
      const what = JSON.stringify(env);
      for (const [path, init] of [
        [`/s/${site}/`, {}],
        ["/", {}],
        ["/__backyard/api/sites", {}],
        [`/__backyard/api/sites/${site}/preview`, {}],
        ["/__backyard/api/info", {}],
        ["/__backyard/api/deploy/sites", { headers: tokenHeaders() }],
        [`/__backyard/api/deploy/sites/${site}`, { headers: tokenHeaders() }],
        [`/s/${site}/__backyard/api/db/posts`, { method: "POST", headers: apiHeaders(), body: '{"x":1}' }],
      ] as const) {
        const res = await call(path, init as RequestInit, env);
        expect(res.status, `${what} ${path}`).toBe(503);
        expect(await res.text(), `${what} ${path}`).not.toContain("secret page");
      }
      expect((await call("/__backyard/healthz", {}, env)).status, what).toBe(200);
      const ready = await call("/__backyard/readyz", {}, env);
      expect(ready.status, what).toBe(503);
      const text = await ready.text();
      expect(text, what).toContain("SITE_LISTING");
      if (env.OWNERSHIP) expect(text, what).toContain("OWNERSHIP");
    }
    expect((await call(`/s/${site}/`, {}, { SITE_LISTING: " Owners " })).status).toBe(200);
    expect((await call(`/s/${site}/`, {}, { SITE_LISTING: "", OWNERSHIP: "none" })).status).toBe(200);
  });
});

describe("listedTo (the Go server's sites.Listed)", () => {
  const vars = { ADMINS: "Root@X.com, anon:00000000000000aa", ADMIN_GROUPS: "ops" };
  const owners = cfgOf({ ...vars, SITE_LISTING: "owners" });
  const every = cfgOf(vars);
  const user = (id: string, more: Partial<User> = {}): User => ({ id, name: id, anonymous: false, admin: false, ...more });
  const rec = (name: string, owners: string[], createdBy?: string): SiteRecord => {
    const r: SiteRecord = { name, created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", file_count: 1, bytes: 1, owners };
    if (createdBy !== undefined) r.created_by = createdBy;
    return r;
  };
  const bobs = rec("bobs", ["bob@x.com"], "bob@x.com");
  const bobsBySub = rec("bobs-sub", ["sub:bob123"], "carol@x.com");
  const shared = rec("shared", ["carol@x.com", "BOB@X.COM"]);
  const cis = rec("cis", ["token:ci"], "token:ci");
  const legacy = rec("legacy", [], "bob@x.com");
  const legacySub = rec("legacy-sub", [], "SUB:ABC");
  const claimed = rec("claimed", ["carol@x.com"], "bob@x.com");
  const synced = rec("synced", []); // no creator
  const byGuest = rec("by-guest", [], "anon:0123456789abcdef");
  const guestSite = rec("guest-owned", ["anon:0123456789abcdef"]);
  const all = [bobs, bobsBySub, shared, cis, legacy, legacySub, claimed, synced, byGuest, guestSite];

  const cases: [string, User | null, SiteRecord[]][] = [
    ["owner by id and email", user("bob@x.com", { email: "bob@x.com" }), [bobs, shared, legacy]],
    ["owner by id, email in another case", user("sub:Bob123", { email: "Bob@X.com" }), [bobs, bobsBySub, shared, legacy]],
    ["other owner", user("carol@x.com", { email: "carol@x.com" }), [shared, claimed]],
    ["creator of an ownerless site without email", user("sub:abc"), [legacySub]],
    ["admin from ADMINS (recomputed)", user("root@x.com", { email: "root@x.com" }), all],
    ["admin from the identity", user("dave@x.com", { email: "dave@x.com", admin: true }), all],
    ["admin from ADMIN_GROUPS", user("erin@x.com", { email: "erin@x.com", groups: ["OPS"] }), all],
    ["the DEPLOY_TOKEN user", user("token:deploy", { admin: true }), all],
    ["a token user that is no admin", user("token:ci"), [cis]],
    ["guest", user("anon:0123456789abcdef", { anonymous: true }), []],
    // A guest is never an admin, even with its id in ADMINS or flagged.
    ["guest flagged admin", user("anon:00000000000000aa", { anonymous: true, admin: true }), []],
    ["no user", null, []],
  ];
  for (const [name, u, want] of cases) {
    it(name, () => {
      for (const r of all) {
        expect(listedTo(owners, u, r), `owners: ${r.name}`).toBe(want.includes(r));
        expect(listedTo(every, u, r), `all: ${r.name}`).toBe(true);
      }
      expect(filterListed(owners, u, all)).toEqual(want);
      expect(filterListed(every, u, all)).toBe(all);
    });
  }

  it("lists no missing site, and no ownerless site without creator to a user without id", () => {
    expect(listedTo(owners, user("bob@x.com"), null)).toBe(false);
    expect(listedTo(owners, user(""), synced)).toBe(false);
    expect(listedTo(owners, user("", { email: "" }), rec("e", [], ""))).toBe(false);
    expect(listedTo(every, null, null)).toBe(true);
  });
});

// --- the surfaces ----------------------------------------------------------

const LISTING: Partial<Env> = { SITE_LISTING: "owners", ADMINS: "root@acme.com", ADMIN_GROUPS: "platform" };
const SVG = '<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="720"><rect width="1280" height="720"/></svg>\n';

/** A Backyard API token of a signed-in person (mode none accepts it like a CLI login). */
async function personToken(email: string, groups: string[] = []): Promise<string> {
  const u: User = { id: email, email, name: email.split("@")[0]!, anonymous: false, admin: false };
  if (groups.length) u.groups = groups;
  return (await issueAPIToken(SECRET, u, 3600)).token;
}

/** A Backyard API token of a guest (AUTH_MODE=none lets guests mint them). */
async function guestToken(): Promise<string> {
  return (await issueAPIToken(SECRET, { id: "anon:0123456789abcdef", name: "Guest 0123", anonymous: true, admin: false }, 3600)).token;
}

/** Names of the sites with the prefix tag in a {sites:[{name}]} answer, sorted. */
async function tagged(res: Response, tag: string): Promise<string[]> {
  expect(res.status, await res.clone().text()).toBe(200);
  const body = (await res.json()) as { sites: { name: string }[] };
  return body.sites.map((s) => s.name).filter((n) => n.startsWith(tag + "-")).sort();
}

const hubList = (headers: Record<string, string>, env: Partial<Env>) => call("/__backyard/api/sites", { headers }, env);
const gatewayList = (headers: Record<string, string>, env: Partial<Env>) => call("/__backyard/api/deploy/sites", { headers }, env);
const preview = (site: string, headers: Record<string, string>, env: Partial<Env>) => call(`/__backyard/api/sites/${site}/preview`, { headers }, env);
const bearer = (token: string) => ({ Authorization: `Bearer ${token}` });

interface Fixture {
  tag: string;
  /** Every site of the fixture, sorted. */
  sites: string[];
  site: (suffix: string) => string;
  /** A site of the fixture without a preview (owned by alice). */
  plain: string;
}

/**
 * Deploys, with the tag as prefix:
 * - alice: by alice@acme.com with the option (owner alice);
 * - bob: by bob@acme.com with the option (owner bob);
 * - shared: by bob, who then adds alice as an owner;
 * - legacy: by alice before the option (OWNERSHIP unset: no owners, created_by alice);
 * - claimed: by alice before the option, then redeployed by bob with it (owner bob);
 * - ci: with the DEPLOY_TOKEN and the option (owner token:deploy);
 * - guest: by a guest with anonymous deploys (no owners, created_by anon:…);
 * - plain: by alice with the option, without a preview.
 */
async function fixture(): Promise<Fixture> {
  const tag = uniqueSite("lst");
  const site = (s: string) => `${tag}-${s}`;
  const files = { "index.html": "<!doctype html><title>Listing</title><p>hello</p>", "preview.svg": SVG };
  const alice = await personToken("alice@acme.com");
  const bob = await personToken("bob@acme.com");
  await deploy(site("alice"), files, { token: alice, envOverrides: LISTING });
  await deploy(site("bob"), files, { token: bob, envOverrides: LISTING });
  await deploy(site("shared"), files, { token: bob, envOverrides: LISTING });
  const add = await call(`/__backyard/api/deploy/sites/${site("shared")}/owners`, { method: "POST", headers: tokenHeaders(bob), body: JSON.stringify({ add: ["Alice@Acme.com"] }) }, LISTING);
  expect(add.status, await add.clone().text()).toBe(200);
  await deploy(site("legacy"), files, { token: alice });
  await deploy(site("claimed"), files, { token: alice });
  await deploy(site("claimed"), files, { token: bob, envOverrides: LISTING });
  await deploy(site("ci"), files, { envOverrides: LISTING });
  await deploy(site("guest"), files, { token: await guestToken(), envOverrides: { ...LISTING, ALLOW_ANONYMOUS_DEPLOY: "true" } });
  await deploy(site("plain"), { "index.html": "<p>no preview</p>" }, { token: alice, envOverrides: LISTING });
  const sites = ["alice", "bob", "shared", "legacy", "claimed", "ci", "guest", "plain"].map(site).sort();
  return { tag, sites, site, plain: site("plain") };
}

describe("the site lists with SITE_LISTING=owners (mode none)", () => {
  let f: Fixture;
  beforeAll(async () => {
    f = await fixture();
  });

  it("records the owners and creators the listing decides by", async () => {
    const meta = async (s: string) => (await (await call(`/__backyard/api/deploy/sites/${f.site(s)}`, { headers: tokenHeaders() })).json()) as Record<string, unknown>;
    expect(await meta("alice")).toMatchObject({ owners: ["alice@acme.com"], created_by: "alice@acme.com" });
    expect(await meta("shared")).toMatchObject({ owners: ["bob@acme.com", "alice@acme.com"], created_by: "bob@acme.com" });
    expect(await meta("legacy")).toMatchObject({ owners: [], created_by: "alice@acme.com" });
    expect(await meta("claimed")).toMatchObject({ owners: ["bob@acme.com"], created_by: "alice@acme.com" });
    expect(await meta("ci")).toMatchObject({ owners: ["token:deploy"], created_by: "token:deploy" });
    expect(await meta("guest")).toMatchObject({ owners: [], created_by: "anon:0123456789abcdef" });
  });

  it("lists each site to its owners, to its creator while it has none, and to admins, on the hub and the gateway alike", async () => {
    const s = (...names: string[]) => names.map(f.site).sort();
    const who: [string, Record<string, string>, string[]][] = [
      ["alice (API token)", bearer(await personToken("Alice@Acme.com")), s("alice", "shared", "legacy", "plain")],
      ["bob (API token)", bearer(await personToken("bob@acme.com")), s("bob", "shared", "claimed")],
      ["dana, who owns nothing", bearer(await personToken("dana@acme.com")), []],
      ["root, in ADMINS", bearer(await personToken("root@acme.com")), f.sites],
      ["erin, in an ADMIN_GROUPS group", bearer(await personToken("erin@acme.com", ["eng", "Platform"])), f.sites],
      ["the DEPLOY_TOKEN (admin)", bearer(DEPLOY_TOKEN), f.sites],
      ["a guest's API token", bearer(await guestToken()), []],
    ];
    for (const [name, headers, want] of who) {
      expect(await tagged(await hubList(headers, LISTING), f.tag), `hub: ${name}`).toEqual(want);
      expect(await tagged(await gatewayList(headers, LISTING), f.tag), `gateway: ${name}`).toEqual(want);
      // Without the option every site is listed to everyone.
      expect(await tagged(await hubList(headers, {}), f.tag), `hub, all: ${name}`).toEqual(f.sites);
      expect(await tagged(await gatewayList(headers, { SITE_LISTING: "all" }), f.tag), `gateway, all: ${name}`).toEqual(f.sites);
    }
    // Admin rights are recomputed on every request: without ADMINS root is no admin.
    expect(await tagged(await hubList(bearer(await personToken("root@acme.com")), { ...LISTING, ADMINS: "" }), f.tag)).toEqual([]);
  });

  it("lists no site to a guest of the browser (cookie) or the gateway (address)", async () => {
    const first = await hubList({}, LISTING);
    const cookie = anonCookie(first);
    expect(cookie).not.toBe("");
    expect(await tagged(first, f.tag)).toEqual([]);
    expect(await tagged(await hubList({ Cookie: cookie }, LISTING), f.tag)).toEqual([]);
    expect(await tagged(await gatewayList({ Cookie: cookie }, LISTING), f.tag)).toEqual([]);
    expect(await tagged(await gatewayList({}, LISTING), f.tag)).toEqual([]);
    // The same guest sees every site without the option.
    expect(await tagged(await hubList({ Cookie: cookie }, {}), f.tag)).toEqual(f.sites);
    expect(await tagged(await gatewayList({}, {}), f.tag)).toEqual(f.sites);
  });

  it("changes nothing else in the answers: same SiteMeta, same order", async () => {
    const alice = bearer(await personToken("alice@acme.com"));
    const all = ((await (await hubList(bearer(DEPLOY_TOKEN), LISTING)).json()) as { sites: Record<string, unknown>[] }).sites;
    const hers = ((await (await hubList(alice, LISTING)).json()) as { sites: Record<string, unknown>[] }).sites;
    const names = new Set(hers.map((x) => x.name));
    expect(hers).toEqual(all.filter((x) => names.has(x.name)));
    expect(Object.keys((await (await gatewayList(alice, LISTING)).json()) as object)).toEqual(["sites"]);
    const gw = ((await (await gatewayList(alice, LISTING)).json()) as { sites: Record<string, unknown>[] }).sites;
    expect(gw).toEqual(hers);
  });

  it("answers the preview of a site that is not listed to the viewer like one without a preview (404)", async () => {
    const alice = bearer(await personToken("alice@acme.com"));
    const noPreview = await preview(f.plain, alice, LISTING);
    expect(noPreview.status).toBe(404);
    expect(await noPreview.json()).toEqual({ error: { code: "not_found", message: `site "${f.plain}" has no preview` } });
    for (const s of f.sites.filter((x) => x !== f.plain)) {
      const listed = ["alice", "shared", "legacy"].map(f.site).includes(s);
      const res = await preview(s, alice, LISTING);
      expect(res.status, s).toBe(listed ? 200 : 404);
      if (listed) expect(await res.text(), s).toBe(SVG);
      else expect(await res.json(), s).toEqual({ error: { code: "not_found", message: `site "${s}" has no preview` } });
      // Admins and the DEPLOY_TOKEN see every preview; guests none; all of them without the option.
      expect((await preview(s, bearer(DEPLOY_TOKEN), LISTING)).status, s).toBe(200);
      expect((await preview(s, bearer(await personToken("erin@acme.com", ["platform"])), LISTING)).status, s).toBe(200);
      expect((await preview(s, {}, LISTING)).status, s).toBe(404);
      expect((await preview(s, alice, {})).status, s).toBe(200);
      expect((await preview(s, {}, {})).status, s).toBe(200);
    }
  });

  it("does not filter lookups by name, files, the site API or the site itself (not access control)", async () => {
    const dana = await personToken("dana@acme.com");
    const cookie = anonCookie(await call("/__backyard/api/me"));
    for (const s of f.sites) {
      for (const headers of [bearer(dana), { Cookie: cookie }]) {
        const meta = await call(`/__backyard/api/deploy/sites/${s}`, { headers }, LISTING);
        expect(meta.status, s).toBe(200);
        expect(((await meta.json()) as { name: string }).name).toBe(s);
        expect((await call(`/__backyard/api/deploy/sites/${s}/files`, { headers }, LISTING)).status, s).toBe(200);
        expect((await call(`/__backyard/api/deploy/sites/${s}/files/index.html`, { headers }, LISTING)).status, s).toBe(200);
        expect((await call(`/s/${s}/`, { headers }, LISTING)).status, s).toBe(200);
        expect((await call(`/s/${s}/__backyard/api/site`, { headers }, LISTING)).status, s).toBe(200);
      }
    }
    // The ownership policy still guards writes by name: dana cannot take alice's site.
    await expect(deploy(f.site("alice"), { "index.html": "dana" }, { token: dana, envOverrides: LISTING })).rejects.toThrow(/403.*owned by alice@acme\.com/);
  });
});

describe("/info site_listing (SPEC §7.1)", () => {
  it("is always present, on the hub and the gateway: all or owners", async () => {
    for (const [env, want] of [
      [{}, "all"],
      [{ SITE_LISTING: "" }, "all"],
      [{ SITE_LISTING: " ALL " }, "all"],
      [{ SITE_LISTING: "owners" }, "owners"],
      [{ SITE_LISTING: " Owners ", OWNERSHIP: "owner" }, "owners"],
      [{ ...MCP_ENV, SITE_LISTING: "owners" }, "owners"],
    ] as [Partial<Env>, string][]) {
      for (const path of ["/__backyard/api/info", "/__backyard/api/deploy/info"]) {
        const info = (await (await call(path, {}, env)).json()) as Record<string, unknown>;
        expect(info.site_listing, `${path} ${JSON.stringify(env)}`).toBe(want);
        // After limits and before mcp_url and edition, like the Go server's Info.
        const keys = Object.keys(info);
        expect(keys.indexOf("site_listing"), path).toBe(keys.indexOf("limits") + 1);
        expect(keys.at(-1)).toBe("edition");
      }
    }
  });
});

// --- Cloudflare Access identities and the Claude connector ---------------------

describe("SITE_LISTING=owners with Cloudflare Access (AUTH_MODE=access)", () => {
  const TEAM = "acme.cloudflareaccess.com";
  const AUD = "aud-listing";
  const accessEnv: Partial<Env> = { ...MCP_ENV, AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: AUD, ...LISTING };
  const groups: Record<string, string[]> = { "alice@acme.com": ["eng"], "bob@acme.com": ["eng"], "olga@acme.com": ["Platform"], "dana@acme.com": [] };

  /** Stubs the team's keys and identity endpoint; returns the Access header of a person. */
  async function access(): Promise<(email: string) => Promise<Record<string, string>>> {
    const pair = (await crypto.subtle.generateKey(
      { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
      true,
      ["sign", "verify"],
    )) as CryptoKeyPair;
    const jwk = (await crypto.subtle.exportKey("jwk", pair.publicKey)) as JsonWebKey;
    let current = "";
    const realFetch = globalThis.fetch;
    vi.stubGlobal("fetch", async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
      if (url === `https://${TEAM}/cdn-cgi/access/certs`) return Response.json({ keys: [{ ...jwk, kid: "k1", alg: "RS256", use: "sig" }] });
      if (url === `https://${TEAM}/cdn-cgi/access/get-identity`) return Response.json({ name: current, email: current, groups: (groups[current] ?? []).map((g, i) => ({ id: String(i), name: g })) });
      return realFetch(input, init);
    });
    return async (email: string) => {
      current = email;
      const now = Math.floor(Date.now() / 1000);
      const claims = { aud: [AUD], email, sub: email, iss: `https://${TEAM}`, iat: now, exp: now + 3600, identity_nonce: "n-" + email };
      const h = b64url(new TextEncoder().encode(JSON.stringify({ alg: "RS256", kid: "k1", typ: "JWT" })));
      const p = b64url(new TextEncoder().encode(JSON.stringify(claims)));
      const sig = new Uint8Array(await crypto.subtle.sign("RSASSA-PKCS1-v1_5", pair.privateKey, new TextEncoder().encode(`${h}.${p}`)));
      return { "Cf-Access-Jwt-Assertion": `${h}.${p}.${b64url(sig)}` };
    };
  }

  /** Calls a tool of the connector and returns its parsed JSON result. */
  async function tool(token: string, env: Partial<Env>, name: string, args: Record<string, unknown>): Promise<Record<string, unknown>> {
    const res = await call(
      "/__backyard/mcp",
      {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json, text/event-stream", "MCP-Protocol-Version": "2025-11-25", Authorization: `Bearer ${token}` },
        body: JSON.stringify({ jsonrpc: "2.0", id: 1, method: "tools/call", params: { name, arguments: args } }),
      },
      env,
    );
    const text = await res.text();
    expect(res.status, text.slice(0, 300)).toBe(200);
    const result = (JSON.parse(text) as { result: { content: { text: string }[]; isError?: boolean } }).result;
    expect(result.isError, result.content[0]!.text).toBeUndefined();
    return JSON.parse(result.content[0]!.text) as Record<string, unknown>;
  }

  it("lists the sites of the signed-in person on the hub, the gateway and the preview route; admins see all", async () => {
    const f = await fixture();
    const identity = await access();
    const s = (...names: string[]) => names.map(f.site).sort();
    for (const [email, want] of [
      ["alice@acme.com", s("alice", "shared", "legacy", "plain")],
      ["bob@acme.com", s("bob", "shared", "claimed")],
      ["dana@acme.com", []],
      ["olga@acme.com", f.sites], // ADMIN_GROUPS
      ["root@acme.com", f.sites], // ADMINS
    ] as [string, string[]][]) {
      const headers = await identity(email);
      expect(await tagged(await hubList(headers, accessEnv), f.tag), `hub: ${email}`).toEqual(want);
      expect(await tagged(await gatewayList(headers, accessEnv), f.tag), `gateway: ${email}`).toEqual(want);
      for (const site of f.sites.filter((x) => x !== f.plain)) {
        expect((await preview(site, headers, accessEnv)).status, `${email} ${site}`).toBe(want.includes(site) ? 200 : 404);
      }
      // Without the option every site is listed.
      const all = { ...accessEnv, SITE_LISTING: "" };
      expect(await tagged(await hubList(headers, all), f.tag), `hub, all: ${email}`).toEqual(f.sites);
    }
    // The DEPLOY_TOKEN is an admin in mode access too.
    expect(await tagged(await gatewayList(bearer(DEPLOY_TOKEN), accessEnv), f.tag)).toEqual(f.sites);
    expect(await tagged(await hubList(bearer(DEPLOY_TOKEN), accessEnv), f.tag)).toEqual(f.sites);
  });

  it("backyard_list_sites lists and counts only the sites the person can see", async () => {
    const f = await fixture();
    const identity = await access();
    const connect = async (email: string) => (await mcpConnect({ identity: await identity(email), env: accessEnv })).access_token;
    const names = (r: Record<string, unknown>) => (r.sites as { name: string }[]).map((x) => x.name).sort();
    const alice = await connect("alice@acme.com");
    let r = await tool(alice, accessEnv, "backyard_list_sites", { query: f.tag });
    expect(names(r)).toEqual(["alice", "shared", "legacy", "plain"].map(f.site).sort());
    expect(r.total).toBe(4);
    expect(r.truncated).toBe(false);
    r = await tool(alice, accessEnv, "backyard_list_sites", { query: f.site("bob") });
    expect(r).toEqual({ sites: [], total: 0, truncated: false });
    const dana = await connect("dana@acme.com");
    expect(await tool(dana, accessEnv, "backyard_list_sites", { query: f.tag })).toEqual({ sites: [], total: 0, truncated: false });
    const olga = await connect("olga@acme.com");
    r = await tool(olga, accessEnv, "backyard_list_sites", { query: f.tag });
    expect(names(r)).toEqual(f.sites);
    expect(r.total).toBe(f.sites.length);
    // Owners keep appearing (ownership is owner with the option).
    expect((r.sites as { name: string; owners?: string[] }[]).find((x) => x.name === f.site("shared"))?.owners).toEqual(["bob@acme.com", "alice@acme.com"]);
    // Without the option alice sees every site again.
    const all = { ...accessEnv, SITE_LISTING: "all" };
    r = await tool(alice, all, "backyard_list_sites", { query: f.tag });
    expect(names(r)).toEqual(f.sites);
    expect(r.total).toBe(f.sites.length);
    // The read tools are not filtered.
    expect(await tool(dana, accessEnv, "backyard_site_files", { site: f.site("alice") })).toMatchObject({ site: f.site("alice"), total: 2 });
  });

  it("lists no site to a guest's connection in mode none", async () => {
    const f = await fixture();
    const env = { ...MCP_ENV, ...LISTING };
    const conn = await mcpConnect({ env });
    expect(await tool(conn.access_token, env, "backyard_list_sites", { query: f.tag })).toEqual({ sites: [], total: 0, truncated: false });
    const r = await tool(conn.access_token, { ...MCP_ENV }, "backyard_list_sites", { query: f.tag });
    expect(r.total).toBe(f.sites.length);
  });
});
