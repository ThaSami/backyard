// auth.mode oidc against the mock IdP (path mode on 127.0.0.1): browser
// sign-in through the IdP, claim mapping into backyard.me(), the hub's user
// header, API 401s with a login URL, allowed domains, realtime with real
// identities, and sign-out.
import { URLS } from "../../harness/env.mjs";
import { IDP_USERS } from "../../harness/mock-idp.mjs";
import { expect, expectAccessible, rand, test } from "../fixtures.mjs";
import { idpLog, person } from "./helpers.mjs";

/** Records the URLs of top-level navigation requests (redirect hops included). */
function recordNavigations(page) {
  const urls = [];
  page.on("request", (req) => {
    if (req.isNavigationRequest() && req.frame() === page.mainFrame()) urls.push(req.url());
  });
  return urls;
}

const backyardCookies = async (ctx) => (await ctx.cookies(URLS.oidc)).filter((c) => c.name.startsWith("backyard_"));

test("visiting a site signs in through the IdP and comes back with the mapped identity", async ({ newUser, state }) => {
  const alice = await person(newUser, state, "alice");
  const page = await alice.newPage();
  const navigations = recordNavigations(page);
  const site = `${state.oidcSites.hello}?from=e2e`;
  const logBefore = (await idpLog(state)).length;

  await page.goto(site);
  await expect(page).toHaveURL(site); // the original URL (with its query) survives the round trip
  const hops = navigations.map((u) => new URL(u));
  expect(hops[0].href).toBe(site);
  expect(hops.some((u) => u.origin === URLS.oidc && u.pathname === "/__backyard/auth/login" && u.searchParams.get("rd") === site)).toBe(true);
  expect(hops.some((u) => u.origin === state.idp.issuer && u.pathname === "/authorize")).toBe(true);
  expect(hops.some((u) => u.origin === URLS.oidc && u.pathname === "/__backyard/auth/callback" && u.searchParams.has("code"))).toBe(true);

  // The authorization request used PKCE (S256), a nonce and the registered callback.
  const events = (await idpLog(state)).slice(logBefore);
  const authorize = events.find((e) => e.type === "authorize");
  expect(authorize.problem).toBe("");
  expect(authorize.params).toMatchObject({
    client_id: state.idp.clientId,
    redirect_uri: `${URLS.oidc}/__backyard/auth/callback`,
    response_type: "code",
    code_challenge_method: "S256",
  });
  expect(authorize.params.scope.split(" ")).toEqual(expect.arrayContaining(["openid", "profile", "email", "groups"]));
  expect(events).toContainEqual({ type: "token", user: "alice", pkce: true });

  // The page: backyard.me() is the mapped IdP identity.
  await expect(page.locator("#me-name")).toHaveText("Hi, Alice 👋");
  await expect(page.locator("#me-detail")).toHaveText("Staff Engineer · Platform · alice@example.com");
  await expect(page.locator("img#me-avatar")).toHaveJSProperty("naturalWidth", 1);
  const me = await page.evaluate(() => backyard.me());
  me.groups.sort(); // (the session keeps admin/allowed groups first)
  expect(me).toEqual({
    id: "alice@example.com",
    email: "alice@example.com",
    name: "Alice Smith",
    title: "Staff Engineer",
    team: "Platform",
    slack: "@alice",
    avatar_url: `${state.idp.issuer}/avatars/alice.png`,
    groups: ["backyard-admins", "eng"],
    anonymous: false,
    admin: true, // auth.admin_groups: [backyard-admins]
    extra: { location: "Ottawa" }, // identity.extra_claims
  });
  expect(await page.evaluate(() => ({ ...backyard.site }))).toEqual({
    name: "hello",
    url: state.oidcSites.hello,
    apiBase: `${state.oidcSites.hello}__backyard`,
    mode: "path",
  });

  // The session cookie: host-only (path mode), HttpOnly, SameSite=Lax; the login state cookie is gone.
  const cookies = await backyardCookies(alice);
  expect(cookies.map((c) => c.name)).toEqual(["backyard_session"]);
  expect(cookies[0]).toMatchObject({ domain: "127.0.0.1", path: "/", httpOnly: true, secure: false, sameSite: "Lax" });
});

test("the hub shows the signed-in user, admin badge and sign-out link", async ({ newUser, state }) => {
  const page = await (await person(newUser, state, "alice")).newPage();
  await page.goto("/");
  await expect(page).toHaveURL(`${URLS.oidc}/`);
  await expect(page.locator("#user-name")).toHaveText("Alice Smith");
  await expect(page.locator("#user")).toHaveAttribute("title", "Alice Smith · alice@example.com");
  await expect(page.locator("#user-admin")).toBeVisible();
  await expect(page.locator("#logout")).toBeVisible();
  await expect(page.locator("#token-box")).toBeHidden(); // signed-in users need no deploy token
  for (const name of Object.keys(state.oidcSites)) {
    await expect(page.locator(`.site[data-site="${name}"] .site-title a`)).toHaveAttribute("href", state.oidcSites[name]);
    await expect(page.locator(`button[aria-label="Delete ${name}"]`)).toBeVisible(); // admins may delete
  }
  await expect(page.locator("#cmd-login")).toHaveText(`backyard login --server ${URLS.oidc}`);

  // The signed-in header (avatar, admin badge, sign-out link) is accessible too.
  await expectAccessible(page, "hub, signed in");
});

test("API calls without a session get 401 with a login URL; pages redirect", async ({ request, state }) => {
  const api = await request.get(`${state.oidcSites.hello}__backyard/api/me`);
  expect(api.status()).toBe(401);
  const body = await api.json();
  expect(body.error.code).toBe("unauthenticated");
  expect(body.error.login_url).toMatch(new RegExp(`^${URLS.oidc}/__backyard/auth/login\\?rd=`));

  const html = await request.get(state.oidcSites.hello, { headers: { Accept: "text/html" }, maxRedirects: 0 });
  expect(html.status()).toBe(302);
  const to = new URL(html.headers().location, URLS.oidc);
  expect(to.pathname).toBe("/__backyard/auth/login");
  expect(to.searchParams.get("rd")).toBe(state.oidcSites.hello);

  // The SDK file and server info are public; the site's files are not.
  expect((await request.get(`${state.oidcSites.hello}__backyard/backyard.js`)).status()).toBe(200);
  expect((await request.get(`${URLS.oidc}/__backyard/api/info`)).status()).toBe(200);
  expect((await request.get(`${state.oidcSites.hello}index.html`, { maxRedirects: 0 })).status()).not.toBe(200);
});

test("without an IdP session the IdP asks who you are; Bob signs in", async ({ newUser, state }) => {
  const page = await (await person(newUser, state, null)).newPage();
  await page.goto(state.oidcSites.hello);
  await expect(page).toHaveURL(new RegExp(`^${state.idp.issuer}/authorize\\?`));
  await expect(page.locator("h1")).toHaveText("Mock IdP: choose an account");
  await page.getByRole("button", { name: "Sign in as Bob Jones" }).click();
  await expect(page).toHaveURL(state.oidcSites.hello);
  await expect(page.locator("#me-name")).toHaveText("Hi, Bob 👋");
  await expect(page.locator("#me-detail")).toHaveText("Product Designer · Design · bob@example.com");
  const me = await page.evaluate(() => backyard.me());
  expect(me).toMatchObject({ id: "bob@example.com", name: "Bob Jones", groups: ["design"], admin: false, anonymous: false });
  expect(me.avatar_url).toBeUndefined();
});

test("realtime between signed-in people carries their real names", async ({ newUser, state }) => {
  const alice = await (await person(newUser, state, "alice")).newPage();
  const bob = await (await person(newUser, state, "bob")).newPage();

  // The guestbook: Bob sees Alice's note under her name and cannot delete it.
  await alice.goto(state.oidcSites.hello);
  await bob.goto(state.oidcSites.hello);
  const text = `Signed note ${rand()}`;
  await alice.locator("#note").fill(text);
  await alice.locator("#post").click();
  const note = bob.locator("#notes li", { has: bob.locator(".text", { hasText: text }) });
  await expect(note.locator(".who")).toHaveText("Alice Smith");
  await expect(note.locator("img.avatar")).toHaveAttribute("src", `${state.idp.issuer}/avatars/alice.png`);
  await expect(note.locator("button.del")).toHaveCount(0);
  await alice.locator("#notes li", { has: alice.locator(".text", { hasText: text }) }).locator("button.del").click();
  await expect(note).toHaveCount(0);

  // The app template: presence counts people; items carry the author's name.
  await alice.goto(state.oidcSites["team-app"]);
  await bob.goto(state.oidcSites["team-app"]);
  await expect(alice.locator("#online")).toHaveText("● 2 online");
  await expect(bob.locator("#user-name")).toHaveText("Bob Jones");
  const task = `Signed task ${rand()}`;
  await bob.locator("#add-input").fill(task);
  await bob.locator("#add-input").press("Enter");
  const row = alice.locator("#list li.item", { has: alice.locator(".text", { hasText: task }) });
  await expect(row.locator(".meta")).toHaveText("Bob Jones");
  await row.locator("button.icon").click();
  await expect(bob.locator("#list li.item", { has: bob.locator(".text", { hasText: task }) })).toHaveCount(0);
});

test("people outside the allowed domains are turned away", async ({ newUser, state, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 403/); // the "Access denied" page itself
  const mallory = await person(newUser, state, "mallory");
  const page = await mallory.newPage();
  const callback = page.waitForResponse((r) => r.url().startsWith(`${URLS.oidc}/__backyard/auth/callback`));
  await page.goto(state.oidcSites.hello);
  expect((await callback).status()).toBe(403);
  await expect(page.locator("h1")).toHaveText("Access denied");
  await expect(page.locator("body")).toContainText(`You signed in as ${IDP_USERS.mallory.email}, but this account is not allowed to use Backyard.`);
  expect(await backyardCookies(mallory)).toEqual([]);
});

test("signing out ends the Backyard session and the IdP session", async ({ newUser, state }) => {
  const alice = await person(newUser, state, "alice");
  const page = await alice.newPage();
  await page.goto("/");
  await expect(page.locator("#user-name")).toHaveText("Alice Smith");
  expect((await backyardCookies(alice)).map((c) => c.name)).toEqual(["backyard_session"]);
  const logBefore = (await idpLog(state)).length;

  await page.locator("#logout").click();
  // Backyard → IdP end_session → back to the hub → not signed in → IdP, which now asks again.
  await expect(page).toHaveURL(new RegExp(`^${state.idp.issuer}/authorize\\?`));
  await expect(page.locator("h1")).toHaveText("Mock IdP: choose an account");
  expect(await backyardCookies(alice)).toEqual([]);
  expect((await alice.cookies(state.idp.issuer)).filter((c) => c.name === "idp_session")).toEqual([]);

  const logout = (await idpLog(state)).slice(logBefore).find((e) => e.type === "logout");
  expect(logout).toMatchObject({ session: "alice", client_id: state.idp.clientId });
  expect([URLS.oidc, `${URLS.oidc}/`]).toContain(logout.post_logout_redirect_uri);

  const api = await alice.request.get(`${state.oidcSites.hello}__backyard/api/me`);
  expect(api.status()).toBe(401);
});
