// Accessibility (axe-core, WCAG 2.1 A/AA) of the hub, the docs and every
// example in use — with data, people, answers, uploads and open dialogs, in
// light and dark mode. Runs on a server of its own.
import { PORTS } from "../../harness/env.mjs";
import { PNG_1X1 } from "../../harness/fake-ai.mjs";
import { standardSites, startScratchServer } from "../../harness/scratch.mjs";
import { expect, expectAccessible, openPage, rand, test } from "../fixtures.mjs";

/** @type {import("../../harness/backyard.mjs").BackyardServer} */
let server;
/** @type {Record<string, string>} */
let sites = {};
test.beforeAll(async () => {
  ({ server, sites } = await startScratchServer({ name: "a11y", port: PORTS.a11y, sites: standardSites(), ai: true }));
});
test.afterAll(async () => {
  await server?.stop();
});

for (const colorScheme of ["light", "dark"]) {
  test.describe(`${colorScheme} mode`, () => {
    test.use({ colorScheme });

    test("hub with a selection, the token box and the delete dialog", async ({ page, state }) => {
      await page.goto(`${server.url}/#token=${encodeURIComponent(state.adminToken)}`);
      await expect(page.locator("#site-list")).not.toHaveAttribute("aria-busy", /.*/);
      await expect(page.locator("#handoff-dialog")).toBeVisible();
      await expectAccessible(page, "hub token dialog");
      await page.locator("#handoff-use").click();
      await page.locator("#files-input").setInputFiles([
        { name: "index.html", mimeType: "text/html", buffer: Buffer.from("<!doctype html><title>x</title>") },
        { name: ".env", mimeType: "text/plain", buffer: Buffer.from("x") },
      ]);
      await page.locator("#selection-files summary").click();
      await page.locator("#selection-ignored summary").click();
      await page.locator("#token-box summary").click();
      await expectAccessible(page, "hub");
      await page.locator('button[aria-label="Delete tmpl-blank"]').click();
      await expect(page.locator("#delete-dialog")).toBeVisible();
      await expectAccessible(page, "hub delete dialog");
    });

    test("docs", async ({ page }) => {
      await page.goto(`${server.url}/docs`);
      await expect(page.locator("#doc h1")).toBeVisible();
      await expectAccessible(page, "docs");
    });

    test("lunch-vote with places, votes and people", async ({ newUser, pageProblems }) => {
      pageProblems.allow(/the server responded with a status of 409/); // the duplicate below
      const alice = await openPage(await newUser(), sites["lunch-vote"]);
      const bob = await openPage(await newUser(), sites["lunch-vote"]);
      const place = `Dumplings ${rand()}`;
      await alice.locator("#name").fill(place);
      await alice.locator("#add button").click();
      const row = (p) => p.locator("li.spot", { has: p.locator(".name", { hasText: place }) });
      await row(alice).locator("button.vote").click();
      await row(bob).locator("button.vote").click();
      await expect(row(alice).locator(".count")).toHaveText("2");
      await alice.locator("#name").fill(place);
      await alice.locator("#add button").click();
      await expect(alice.locator("#error")).toBeVisible();
      await expectAccessible(alice, "lunch-vote");
    });

    test("live-poll: host and audience during a round", async ({ newUser }) => {
      const host = await openPage(await newUser(), sites["live-poll"] + "?host");
      await host.locator("#h-question").fill(`Accessible? ${rand()}`);
      await host.locator("#h-options input").nth(0).fill("Yes");
      await host.locator("#h-options input").nth(1).fill("Also yes");
      await host.locator("#h-form button[type=submit]").click();
      const voter = await openPage(await newUser(), sites["live-poll"]);
      await voter.locator("#a-options button", { hasText: /^Yes$/ }).click();
      await expect(voter.locator("#a-results")).toBeVisible();
      await expect(host.locator("#h-results .total")).toHaveText("1 answer");
      await expectAccessible(host, "live-poll host");
      await expectAccessible(voter, "live-poll audience");
      await host.locator("#h-toggle").click();
      await expect(voter.locator("#a-hint")).toHaveText(/closed/);
      await expectAccessible(voter, "live-poll audience, voting closed");
    });

    test("cursors with someone else's cursor, chat and a reaction", async ({ newUser }) => {
      const alice = await openPage(await newUser(), sites.cursors);
      const bob = await openPage(await newUser(), sites.cursors);
      const box = await bob.locator("#board").boundingBox();
      await bob.mouse.move(box.x + 200, box.y + 200);
      await bob.mouse.move(box.x + 220, box.y + 210);
      await bob.keyboard.press("/");
      await bob.keyboard.type("hi there");
      await expect(alice.locator("#board .cursor.talking")).toHaveCount(1);
      await expect(alice.locator("#toasts .toast")).not.toHaveCount(0);
      await expectAccessible(alice, "cursors (viewer)");
      await expectAccessible(bob, "cursors (chatting)");
    });

    test("ai-chat with a conversation", async ({ newUser }) => {
      const page = await openPage(await newUser(), sites["ai-chat"]);
      await expect(page.locator("#model")).toBeEnabled();
      await expectAccessible(page, "ai-chat welcome");
      await page.locator("#input").fill("Show me some code");
      await page.locator("#input").press("Enter");
      await expect(page.locator(".msg.assistant:not(.streaming) .meta")).toBeVisible();
      await page.locator("#input").fill("[fail] once more");
      await page.locator("#input").press("Enter");
      await expect(page.locator("#banner")).toBeVisible();
      await expectAccessible(page, "ai-chat conversation with an error banner");
    });

    test("file-drop with files, a filter and the viewer", async ({ newUser }) => {
      const page = await openPage(await newUser(), sites["file-drop"]);
      const tag = rand();
      await page.locator("#picker").setInputFiles([
        { name: `pixel-${tag}.png`, mimeType: "image/png", buffer: PNG_1X1 },
        { name: `notes-${tag}.txt`, mimeType: "text/plain", buffer: Buffer.from("notes") },
      ]);
      await expect(page.locator("#queue li.done")).toHaveCount(2);
      await expectAccessible(page, "file-drop uploading");
      await expect(page.locator("#grid .card")).not.toHaveCount(0);
      await page.locator('[data-kind="image"]').click();
      await expect(page.locator('[data-kind="image"]')).toHaveAttribute("aria-pressed", "true");
      await expect(page.locator('[data-kind="all"]')).toHaveAttribute("aria-pressed", "false");
      await expectAccessible(page, "file-drop gallery");
      await page.locator("#grid .card .thumb").first().click();
      await expect(page.locator("#viewer")).toBeVisible();
      await expectAccessible(page, "file-drop viewer");
    });

    test("kpi-dashboard with the tooltip and the table open", async ({ newUser, pageProblems }) => {
      pageProblems.allow(/the server responded with a status of 501/); // no warehouse here
      const page = await openPage(await newUser(), sites["kpi-dashboard"]);
      await expect(page.locator("#svg path.line")).toHaveCount(2);
      await page.locator(".table-card summary").click();
      await page.locator("#notice details summary").click();
      await page.locator("#svg").hover(); // (scrolls the chart into view first)
      await expect(page.locator("#tip")).toBeVisible();
      await expectAccessible(page, "kpi-dashboard");
    });

    test("init templates with content", async ({ newUser }) => {
      const blank = await openPage(await newUser(), sites["tmpl-blank"]);
      await blank.locator("#note").fill(`An accessible note ${rand()}`);
      await blank.locator("#post").click();
      await expect(blank.locator("#notes li")).not.toHaveCount(0);
      await expectAccessible(blank, "blank template");

      const user = await newUser();
      const app = await openPage(user, sites["tmpl-app"]);
      await openPage(await newUser(), sites["tmpl-app"]); // someone else online
      await app.locator("#add-input").fill(`Item ${rand()}`);
      await app.locator("#add-input").press("Enter");
      await app.locator("#add-input").fill(`Done item ${rand()}`);
      await app.locator("#add-input").press("Enter");
      await app.locator("#list li.item").first().locator('input[type="checkbox"]').check();
      await expect(app.locator("#list li.item.done")).not.toHaveCount(0);
      await expect(app.locator("#online")).toHaveText(/online/);
      await app.locator('[data-filter="mine"]').click();
      await expect(app.locator('[data-filter="mine"]')).toHaveAttribute("aria-pressed", "true");
      await expectAccessible(app, "app template");
    });
  });
}
