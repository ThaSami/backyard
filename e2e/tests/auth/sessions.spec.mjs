// Signed-in flows beyond the first sign-in: deploying from the hub with the
// browser session (path mode), the CLI's browser sign-in, and signing in
// again when the session is gone (hub, docs page, SDK).
import http from "node:http";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import { URLS } from "../../harness/env.mjs";
import { expect, rand, test } from "../fixtures.mjs";
import { person, signedInPage } from "./helpers.mjs";

const APP_JS = `backyard.me().then((me) => { document.querySelector("#who").textContent = "hello " + me.name; });`;
const PAGE = (title) => `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>${title}</title><link rel="icon" href="data:,">
<script src="/__backyard/backyard.js"></script><script src="app.js" defer></script></head><body><h1>${title}</h1><p id="who">…</p></body></html>`;

/** Resolves on the next top-level navigation request to the login endpoint (a redirect hop, never "committed"). */
const loginNavigation = (page) =>
  page.waitForRequest((req) => req.isNavigationRequest() && req.url().startsWith(`${URLS.oidc}/__backyard/auth/login?`));

/** Forgets the Backyard session (as if it expired) but keeps the IdP session. */
async function dropBackyardSession(ctx) {
  await ctx.clearCookies({ domain: "127.0.0.1" });
}

test("a signed-in person deploys a folder from the hub and deletes the site again", async ({ newUser, state }, testInfo) => {
  const bob = await person(newUser, state, "bob");
  const page = await bob.newPage();
  await page.goto("/");
  await expect(page.locator("#user-name")).toHaveText("Bob Jones");
  await expect(page.locator("#user-admin")).toBeHidden();
  await expect(page.locator("#site-list")).not.toHaveAttribute("aria-busy", /.*/);
  await expect(page.locator("#token-box")).toBeHidden();

  const name = `e2e-bob-${rand()}`;
  const dir = join(testInfo.outputPath("upload"), name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, "index.html"), PAGE("Bob's Site"));
  writeFileSync(join(dir, "app.js"), APP_JS);
  await page.locator("#folder-input").setInputFiles(dir);
  const url = `${URLS.oidc}/s/${name}/`;
  await expect(page.locator("#site-url")).toHaveText(url);
  await page.locator("#deploy-btn").click();
  await expect(page.locator("#result")).toBeVisible();
  await expect(page.locator("#deploy-error")).toBeHidden();
  await expect(page.locator("#result-link")).toHaveText(url);
  await expect(page.locator(`.site[data-site="${name}"] .who`)).toHaveText("bob@example.com");

  const site = await bob.newPage();
  await site.goto(url);
  await expect(site.locator("#who")).toHaveText("hello Bob Jones");
  await site.close();

  // Sites without owners can be deleted by any signed-in person.
  await page.locator(`button[aria-label="Delete ${name}"]`).click();
  await page.locator("#delete-confirm").fill(name);
  await page.locator("#delete-submit").click();
  await expect(page.locator("#delete-dialog")).toBeHidden();
  await expect(page.locator(`.site[data-site="${name}"]`)).toHaveCount(0);
});

test("the CLI's browser sign-in hands a personal API token to the loopback server", async ({ newUser, state, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 400/); // the bad redirect_uri below
  // Stand-in for `backyard login`'s loopback listener.
  const received = [];
  const loopback = http.createServer((req, res) => {
    received.push(new URL(req.url, "http://127.0.0.1"));
    res.writeHead(200, { "Content-Type": "text/html" });
    res.end("<!doctype html><title>CLI</title><link rel=icon href='data:,'><p id=done>You can close this tab.</p>");
  });
  await new Promise((resolve) => loopback.listen(0, "127.0.0.1", resolve));
  const callback = `http://127.0.0.1:${loopback.address().port}/callback`;
  try {
    const alice = await person(newUser, state, "alice");
    const page = await alice.newPage();
    const st = rand();
    await page.goto(`/__backyard/auth/cli?redirect_uri=${encodeURIComponent(callback)}&state=${st}`);
    await expect(page.locator("#done")).toHaveText("You can close this tab.");
    expect(received).toHaveLength(1);
    const q = received[0].searchParams;
    expect(received[0].pathname).toBe("/callback");
    expect(q.get("state")).toBe(st);
    expect(Date.parse(q.get("expires_at"))).toBeGreaterThan(Date.now());
    const token = q.get("token");

    // The token works for the API and the deploy gateway (no cookies).
    const me = await fetch(`${URLS.oidc}/__backyard/api/deploy/me`, { headers: { Authorization: `Bearer ${token}` } });
    expect(me.status).toBe(200);
    expect(await me.json()).toMatchObject({ id: "alice@example.com", name: "Alice Smith", admin: true });

    // Headless machines: the token is shown for copy and paste.
    await page.goto("/__backyard/auth/cli?mode=display");
    await expect(page.locator("body")).toContainText("Your Backyard CLI token");
    await expect(page.locator("body")).toContainText(`backyard login --server ${URLS.oidc} --token eyJ`);

    // Links that would send the token anywhere but a loopback address are refused.
    await page.goto(`/__backyard/auth/cli?redirect_uri=${encodeURIComponent("http://evil.example/cb")}&state=x`);
    await expect(page.locator("h1")).toHaveText("Invalid CLI login request");
    expect(received).toHaveLength(1);
  } finally {
    await new Promise((resolve) => loopback.close(resolve));
  }
});

test("the docs page signs in again when the session is gone", async ({ newUser, state }) => {
  const alice = await person(newUser, state, "alice");
  const page = await alice.newPage();
  await page.goto("/docs");
  await expect(page.locator("#doc h1")).toHaveText("Backyard API reference");
  await dropBackyardSession(alice);
  // Client-side navigation fetches /docs/recipes.md: the login redirect is
  // noticed and the page goes through the IdP and comes back to the document.
  await page.locator('#doc-list a[data-doc="recipes"]').click();
  await expect(page).toHaveURL(`${URLS.oidc}/docs?doc=recipes`);
  await expect(page.locator('#doc-list a[data-doc="recipes"]')).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#doc h1")).not.toHaveText("Backyard API reference");
  expect((await alice.cookies(URLS.oidc)).map((c) => c.name)).toContain("backyard_session");
});

test("the SDK sends the page through sign-in on a 401 and it comes back", async ({ newUser, state, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 401/); // the call that finds the session gone
  const page = await signedInPage(newUser, state, "alice", state.oidcSites.hello);
  await expect(page.locator("#me-name")).toHaveText("Hi, Alice 👋");
  await dropBackyardSession(page.context());
  // A write without a session: the SDK redirects the top-level page to the
  // login URL, which returns to this page (the API call's Referer).
  await page.locator("#note").fill(`after expiry ${rand()}`);
  const [login] = await Promise.all([loginNavigation(page), page.locator("#post").click()]);
  expect(new URL(login.url()).searchParams.get("rd")).toBe(state.oidcSites.hello);
  await page.waitForURL(state.oidcSites.hello);
  await expect(page.locator("#me-name")).toHaveText("Hi, Alice 👋");
});

test("the hub signs in again when a deploy finds the session gone", async ({ newUser, state, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 401/);
  const alice = await person(newUser, state, "alice");
  const page = await alice.newPage();
  await page.goto("/");
  await expect(page.locator("#user-name")).toHaveText("Alice Smith");
  await page.locator("#site-name").fill(`e2e-expired-${rand()}`);
  await page.locator("#files-input").setInputFiles([{ name: "index.html", mimeType: "text/html", buffer: Buffer.from(PAGE("Expired")) }]);
  await dropBackyardSession(alice);
  const [login] = await Promise.all([loginNavigation(page), page.locator("#deploy-btn").click()]);
  expect(new URL(login.url()).searchParams.get("rd")).toBe(`${URLS.oidc}/`);
  await page.waitForURL(`${URLS.oidc}/`);
  await expect(page.locator("#user-name")).toHaveText("Alice Smith");
});
