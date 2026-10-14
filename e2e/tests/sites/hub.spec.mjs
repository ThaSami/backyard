// The hub: site directory, search, header, CLI instructions.
import { expect, test } from "../fixtures.mjs";

const EXAMPLE_TITLES = {
  "lunch-vote": "Lunch Vote",
  "live-poll": "Live Poll",
  cursors: "Cursors",
  "ai-chat": "AI Chat",
  "file-drop": "File Drop",
  "kpi-dashboard": "KPI Dashboard",
};

test.describe("hub", () => {
  test.beforeEach(async ({ page }) => {
    await page.goto("/");
    await expect(page.locator("#site-list")).not.toHaveAttribute("aria-busy", /.*/);
  });

  test("loads and lists every deployed site with its title and description", async ({ page, request, state }) => {
    await expect(page).toHaveTitle("Backyard E2E");
    await expect(page.locator(".brand-name")).toHaveText("Backyard E2E");
    await expect(page.locator("#banner")).toBeHidden();

    const { sites } = await (await request.get("/__backyard/api/sites")).json();
    for (const name of Object.keys(state.sites)) {
      const meta = sites.find((s) => s.name === name);
      expect(meta, `site ${name} in /api/sites`).toBeTruthy();
      const item = page.locator(`.site[data-site="${name}"]`);
      await expect(item.locator(".site-title a")).toHaveText(meta.title || name);
      await expect(item.locator(".site-title a")).toHaveAttribute("href", state.sites[name]);
      await expect(item.locator(".site-url")).toHaveText(state.sites[name].replace(/^http:\/\//, "").replace(/\/$/, ""));
      if (meta.description) await expect(item.locator(".site-desc")).toHaveText(meta.description);
    }
    for (const [name, title] of Object.entries(EXAMPLE_TITLES)) {
      await expect(page.locator(`.site[data-site="${name}"] .site-title a`)).toHaveText(title);
    }
    // Other spec files may deploy more sites meanwhile: at least ours are counted.
    const count = Number((await page.locator("#site-count").textContent()).replace(/\D/g, ""));
    expect(count).toBeGreaterThanOrEqual(Object.keys(state.sites).length);
  });

  test("shows the guest user, server-specific CLI commands and agent links", async ({ page }) => {
    const me = await page.evaluate(() => fetch("/__backyard/api/me").then((r) => r.json()));
    expect(me.anonymous).toBe(true);
    await expect(page.locator("#user")).toBeVisible();
    await expect(page.locator("#user-name")).toHaveText(me.name);
    await expect(page.locator("#user-admin")).toBeHidden();
    await expect(page.locator("#logout")).toBeHidden();
    await expect(page.locator("#cmd-login")).toContainText(`--server ${new URL(page.url()).origin}`);
    await expect(page.locator("#footer-version")).toContainText("Backyard v");
    for (const href of ["/llms.txt", "/llms-full.txt", "/__backyard/backyard.d.ts", "/docs"]) {
      await expect(page.locator(`.links a[href="${href}"]`)).toBeVisible();
    }
    const llms = await page.request.get("/llms.txt");
    expect(llms.ok()).toBe(true);
    expect(await llms.text()).toContain("backyard");
  });

  test("search filters by name, title and description", async ({ page }) => {
    const search = page.locator("#search");
    const visibleSites = () => page.locator("#site-list .site").evaluateAll((els) => els.map((e) => e.dataset.site));

    // "/" focuses the search box from anywhere on the page.
    await page.locator("body").click({ position: { x: 5, y: 5 } });
    await page.keyboard.press("/");
    await expect(search).toBeFocused();

    await page.keyboard.type("poll");
    await expect.poll(visibleSites).toEqual(["live-poll"]);
    await expect(page.locator("#site-count")).toHaveText(/^\(1 of \d+\)$/);
    await expect(page.locator("#search-status")).toHaveText("1 site found");

    await search.fill("lunch vote"); // every term must match
    await expect.poll(visibleSites).toEqual(["lunch-vote"]);

    await search.fill("WAREHOUSE"); // description, case-insensitive
    await expect.poll(visibleSites).toEqual(["kpi-dashboard"]);

    await search.fill("no-such-site-anywhere");
    await expect(page.locator("#site-list")).toBeHidden();
    await expect(page.locator("#sites-empty")).toHaveText("No sites match “no-such-site-anywhere”.");

    await search.press("Escape");
    await expect(search).toHaveValue("");
    await expect(page.locator(`.site[data-site="lunch-vote"]`)).toBeVisible();
    await expect(page.locator("#sites-empty")).toBeHidden();
  });

  test("the list/grid choice is remembered", async ({ page }) => {
    await page.locator("#view-list").click();
    await expect(page.locator("#site-list")).toHaveClass(/as-list/);
    await expect(page.locator("#view-list")).toHaveAttribute("aria-pressed", "true");
    await page.reload();
    await expect(page.locator("#site-list")).toHaveClass(/as-list/);
    await page.locator("#view-grid").click();
    await expect(page.locator("#site-list")).not.toHaveClass(/as-list/);
  });

  test("copy buttons put the site link and CLI commands on the clipboard", async ({ page, context, state }) => {
    await context.grantPermissions(["clipboard-read", "clipboard-write"]);
    await page.locator('button[aria-label="Copy link to tmpl-blank"]').click();
    await expect(page.locator("#toast")).toHaveText("Copied to clipboard");
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(state.sites["tmpl-blank"]);
    await page.locator("#cmd-login").locator("xpath=ancestor::div[contains(@class,'cmd')]").locator("button.copy").click();
    expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(await page.locator("#cmd-login").textContent());
  });

  test("a copy that cannot reach the clipboard says so", async ({ page }) => {
    // Neither the async clipboard nor the legacy execCommand fallback works here.
    await page.evaluate(() => {
      Object.defineProperty(navigator, "clipboard", { value: { writeText: () => Promise.reject(new DOMException("denied", "NotAllowedError")) } });
      document.execCommand = () => false;
    });
    await page.locator('button[aria-label="Copy link to tmpl-blank"]').click();
    await expect(page.locator("#toast")).toHaveText("Could not copy: select the text and copy it yourself");
  });

  test("a site card links to the live site", async ({ page, state }) => {
    // (Not a realtime example: their specs count the people on the page.)
    await page.locator(`.site[data-site="tmpl-blank"] .site-title a`).click();
    await expect(page).toHaveURL(state.sites["tmpl-blank"]);
    await expect(page.locator("#me-name")).toHaveText(/^Hi, Guest /);
  });
});
