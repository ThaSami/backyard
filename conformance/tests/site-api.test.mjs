// Site API common rules (SPEC §5): identity, CSRF, error envelopes, routing.
import assert from "node:assert/strict";
import { before, describe, test } from "node:test";
import { canDeploy, deploySite } from "../lib/deploy.mjs";
import { hubURL, setup, uniqueSite } from "../lib/env.mjs";
import { assertError, Client, expectStatus } from "../lib/http.mjs";
import { assertSiteMeta, assertUser } from "../lib/shapes.mjs";

const env = await setup();
const client = new Client();
const skip = !canDeploy(env, client) && "cannot deploy (no BACKYARD_TOKEN, anonymous deploys off)";
const site = uniqueSite("api");
const other = uniqueSite("api2");
const api = (p) => env.api(site, p);

describe("site API", { skip }, () => {
  let me;

  before(async () => {
    await deploySite(client, env, site, { "index.html": "<title>API test</title>" });
    await deploySite(client, env, other, { "index.html": "other" });
    me = assertUser(expectStatus(await client.get(api("/me")), 200).json, "/me");
  });

  describe("identity", () => {
    test("GET /me is stable for one client", async () => {
      const again = expectStatus(await client.get(api("/me")), 200).json;
      assert.deepEqual(again, me);
    });

    test("anonymous identities come from a signed cookie", { skip: Boolean(client.token) && "BACKYARD_TOKEN is set" }, async () => {
      if (!me.anonymous) return; // a server with real authentication
      const c = new Client({ token: null });
      const first = await c.get(api("/me"));
      const u = assertUser(expectStatus(first, 200).json);
      assert.equal(u.anonymous, true);
      const cookie = c.jar.all().find((x) => x.name === "backyard_anon");
      assert.ok(cookie, `backyard_anon cookie set: ${first.headers.getSetCookie()}`);
      const attrs = cookie.raw.toLowerCase();
      assert.match(attrs, /;\s*httponly/);
      assert.match(attrs, /;\s*samesite=lax/);
      assert.match(attrs, /;\s*path=\//);
      assert.deepEqual(expectStatus(await c.get(api("/me")), 200).json, u, "the cookie keeps the identity");
      const stranger = expectStatus(await new Client({ token: null }).get(api("/me")), 200).json;
      assert.notEqual(stranger.id, u.id, "another browser is another guest");
      const forged = expectStatus(await new Client({ token: null }).get(api("/me"), { headers: { Cookie: "backyard_anon=forged" } }), 200).json;
      assert.notEqual(forged.id, u.id);
    });

    test("an invalid bearer token is 401 unauthenticated, never a guest", async () => {
      const r = await new Client({ token: "garbage-token" }).get(api("/me"));
      const e = assertError(r, 401, "unauthenticated");
      if (env.info.auth.mode === "none") assert.equal(e.login_url, undefined, "no login_url when no browser login is possible");
      const r2 = await new Client({ token: "a.b.c" }).post(api("/db/things"), { a: 1 });
      assertError(r2, 401, "unauthenticated");
    });

    test("a bearer token is a real (non-anonymous) identity", { skip: !client.token && "no BACKYARD_TOKEN" }, () => {
      assert.equal(me.anonymous, false);
    });

    test("/__backyard/auth/* on a site redirects to the hub", async () => {
      const r = expectStatus(await new Client({ token: null }).get(env.siteURL(site) + "__backyard/auth/login"), 302);
      const loc = new URL(r.header("location"));
      assert.equal(loc.origin + loc.pathname, env.info.public_url + "/__backyard/auth/login");
      assert.equal(loc.searchParams.get("rd"), env.siteURL(site), "rd is the site root");
      const keep = expectStatus(await client.get(env.siteURL(site) + "__backyard/auth/login?rd=" + encodeURIComponent(env.siteURL(site) + "page")), 302);
      assert.equal(new URL(keep.header("location")).searchParams.get("rd"), env.siteURL(site) + "page", "an explicit rd is kept");
      const out = expectStatus(await client.get(env.siteURL(site) + "__backyard/auth/logout"), 302);
      assert.equal(out.header("location"), env.info.public_url + "/__backyard/auth/logout");
    });

    test("GET /site is the SiteMeta of the current site", async () => {
      const m = expectStatus(await client.get(api("/site")), 200).json;
      assertSiteMeta(m, { name: site, url: env.canonicalSiteURL(site) });
      assert.equal(m.title, "API test");
      const m2 = expectStatus(await client.get(env.api(other, "/site")), 200).json;
      assert.equal(m2.name, other);
    });

    test("GET /users/{email}", async () => {
      assertError(await client.get(api("/users/not-an-email")), 400, "bad_request");
      assertError(await client.get(api("/users/nobody-" + Date.now() + "@example.invalid")), 404, "not_found");
    });

    test("the hub API answers /me and /sites", async () => {
      assertUser(expectStatus(await client.get(hubURL + "/__backyard/api/me"), 200).json, "hub /me");
      const s = expectStatus(await client.get(hubURL + "/__backyard/api/sites"), 200).json;
      assert.ok(s.sites.some((x) => x.name === site));
    });

    test("POST /__backyard/api/tokens is refused to guests and static tokens", async () => {
      const r = await client.post(hubURL + "/__backyard/api/tokens", { name: "conformance" });
      if (me.anonymous || me.id.startsWith("token:")) {
        assert.ok([403, 501].includes(r.status), `${r}`);
        assertError(r, r.status, r.status === 403 ? "forbidden" : "unsupported");
        return;
      }
      const t = expectStatus(r, 200).json;
      assert.equal(typeof t.token, "string");
    });
  });

  describe("errors and routing", () => {
    test("unknown endpoints are 404 not_found", async () => {
      for (const p of ["/nope", "/db/c/id/extra/more", "/files/a/b"]) {
        assertError(await client.get(api(p)), 404, "not_found", p);
      }
    });

    test("known paths with the wrong method are 405 bad_request with Allow", async () => {
      const r = await client.delete(api("/me"));
      assertError(r, 405, "bad_request");
      assert.match(r.header("allow") || "", /\bGET\b/);
      const r2 = await client.put(api("/db/things"), {});
      assertError(r2, 405, "bad_request");
      const allow = (r2.header("allow") || "").split(/,\s*/).sort();
      assert.ok(allow.includes("GET") && allow.includes("POST"), `Allow: ${allow}`);
    });

    test("JSON responses are not cached", async () => {
      const r = expectStatus(await client.get(api("/me")), 200);
      assert.match(r.header("content-type"), /^application\/json\b/);
      assert.equal(r.header("cache-control"), "no-store");
      assert.equal(r.header("x-content-type-options"), "nosniff");
    });

    test("invalid JSON bodies are 400", async () => {
      assertError(await client.post(api("/db/things"), "{oops", { raw: true }), 400, "bad_request", "malformed");
      assertError(await client.post(api("/db/things"), "[1,2]", { raw: true }), 400, "bad_request", "not an object");
      assertError(await client.post(api("/db/things"), '{"a":1} {"b":2}', { raw: true }), 400, "bad_request", "two values");
      assertError(await client.fetch(api("/db/things"), { method: "POST", body: "" }), 400, "bad_request", "empty");
    });
  });

  describe("CSRF", () => {
    const write = (opts) => client.fetch(api("/db/csrf"), { method: "POST", body: '{"a":1}', headers: { "Content-Type": "application/json", ...opts.headers }, csrf: opts.csrf });

    test('state-changing requests need "X-Backyard: 1"', async () => {
      assertError(await write({ csrf: false }), 403, "forbidden", "no header");
      assertError(await write({ csrf: false, headers: { "X-Backyard": "0" } }), 403, "forbidden", "wrong value");
      for (const m of ["PUT", "PATCH", "DELETE"]) {
        const r = await client.fetch(api("/db/csrf/x"), { method: m, body: m === "DELETE" ? undefined : "{}", csrf: false });
        assertError(r, 403, "forbidden", m);
      }
      expectStatus(await write({}), 201, "with the header");
    });

    test("the Origin, when present, must be the site's own (path mode: the hub's)", async () => {
      expectStatus(await write({ headers: { Origin: env.siteOrigin(site) } }), 201, "own origin");
      assertError(await write({ headers: { Origin: "https://evil.example" } }), 403, "forbidden", "foreign origin");
      assertError(await write({ headers: { Origin: "null" } }), 403, "forbidden", "opaque origin");
      if (env.mode === "host") {
        assertError(await write({ headers: { Origin: env.siteOrigin(other) } }), 403, "forbidden", "another site's origin");
        assertError(await write({ headers: { Origin: env.hubOrigin } }), 403, "forbidden", "the hub origin in host mode");
      } else {
        expectStatus(await write({ headers: { Origin: env.hubOrigin } }), 201, "the hub origin in path mode");
      }
    });

    test("GET and HEAD need no header", async () => {
      expectStatus(await client.get(api("/db/csrf"), { csrf: false }), 200);
      expectStatus(await client.head(api("/me"), { csrf: false }), 200);
    });
  });
});
