// The pages `backyard init` writes, deployed with the CLI: the blank template
// (backyard.me + a live guestbook with a counter) and the app template (live
// item list with presence).
import { URLS } from "../../harness/env.mjs";
import { expect, openPage, rand, siteURL, test } from "../fixtures.mjs";

const note = (page, text) => page.locator("#notes li", { has: page.locator(".text", { hasText: text }) });
const item = (page, text) => page.locator("#list li.item", { has: page.locator(".text", { hasText: text }) });
const countOf = async (page) => Number((await page.locator("#count").textContent()) || 0);

test.describe("blank template", () => {
  test("greets the user and runs a live guestbook with a counter", async ({ newUser, state }) => {
    const url = siteURL(state, "tmpl-blank");
    const alice = await openPage(await newUser(), url);
    const bob = await openPage(await newUser(), url);

    // backyard.me(): guests on this server.
    await expect(alice.locator("#me-name")).toHaveText(/^Hi, Guest [0-9a-f]{4} 👋$/);
    await expect(alice.locator("#me-detail")).toHaveText("You are browsing as a guest (this server runs without sign-in).");
    await expect(alice.locator("#me-avatar")).toHaveText("G" + (await alice.locator("#me-name").textContent()).match(/Guest ([0-9a-f])/)[1].toUpperCase());
    const aliceName = (await alice.locator("#me-name").textContent()).replace(/^Hi, | 👋$/g, "");
    await expect(alice.locator(".eyebrow")).toHaveText("🌱 Backyard · tmpl-blank");
    await expect(alice.locator("footer a")).toHaveAttribute("href", `${URLS.main}/docs`);

    // Notes appear for everyone, newest first, with a live counter.
    const before = await countOf(bob);
    if (before === 0) await expect(bob.locator("#empty")).toHaveText("No notes yet. Be the first!");
    const text = `Hello from the e2e suite ${rand()}`;
    await alice.locator("#note").fill(text);
    await alice.locator("#post").click();
    await expect(alice.locator("#note")).toHaveValue("");
    await expect(note(bob, text)).toBeVisible();
    await expect(note(bob, text).locator(".who")).toHaveText(aliceName);
    await expect(note(bob, text).locator("time")).toHaveText("just now");
    await expect(bob.locator("#count")).toHaveText(String(before + 1));
    await expect(bob.locator("#empty")).toBeHidden();
    await expect(bob.locator("#notes li").first()).toContainText(text);

    // Only the author can delete a note.
    await expect(note(bob, text).locator("button.del")).toHaveCount(0);
    await expect(note(alice, text).locator("button.del")).toBeVisible();

    // User text is shown as text, never as HTML.
    let dialogs = 0;
    for (const p of [alice, bob]) p.on("dialog", (d) => (dialogs++, d.dismiss()));
    const html = `<img src=x onerror=alert(1)> ${rand()}`;
    await bob.locator("#note").fill(html);
    await bob.locator("#note").press("Enter");
    await expect(note(alice, html)).toBeVisible();
    await expect(note(alice, html).locator(".text")).toHaveText(html);
    await expect(alice.locator("#notes img")).toHaveCount(0);
    await expect(alice.locator("#count")).toHaveText(String(before + 2));

    await note(alice, text).locator("button.del").click();
    await note(bob, html).locator("button.del").click();
    await expect(note(bob, text)).toHaveCount(0);
    await expect(note(alice, html)).toHaveCount(0);
    await expect(alice.locator("#count")).toHaveText(before ? String(before) : "");
    expect(dialogs).toBe(0);
  });
});

test.describe("app template", () => {
  test("shares a live item list with presence", async ({ newUser, state }) => {
    const url = siteURL(state, "tmpl-app");
    const alice = await openPage(await newUser(), url);
    const bob = await openPage(await newUser(), url);
    await expect(alice.locator("#user-name")).toHaveText(/^Guest [0-9a-f]{4}$/);
    const aliceName = await alice.locator("#user-name").textContent();
    await expect(alice.locator("#online")).toHaveText("● 2 online");
    await expect(bob.locator("#online")).toHaveText("● 2 online");
    await expect(alice.locator("#offline")).toBeHidden();
    await expect(alice.locator("#loading")).toBeHidden();

    // Alice adds an item; Bob sees it with her name.
    const text = `Book the room ${rand()}`;
    await alice.locator("#add-input").fill(text);
    await alice.locator("#add-input").press("Enter");
    await expect(item(bob, text)).toBeVisible();
    await expect(item(bob, text).locator(".meta")).toHaveText(aliceName);
    await expect(bob.locator("#list li.item").first()).toContainText(text); // newest first

    // Bob completes it; Alice sees it done. Filters: open / mine.
    await item(bob, text).locator('input[type="checkbox"]').check();
    await expect(item(alice, text)).toHaveClass(/done/);
    await expect(item(alice, text).locator('input[type="checkbox"]')).toBeChecked();
    await alice.locator('[data-filter="open"]').click();
    await expect(item(alice, text)).toHaveCount(0);
    await alice.locator('[data-filter="mine"]').click();
    await expect(item(alice, text)).toBeVisible();
    await bob.locator('[data-filter="mine"]').click();
    await expect(item(bob, text)).toHaveCount(0);
    await bob.locator('[data-filter="all"]').click();

    // Deleting removes it for everyone.
    await item(bob, text).locator("button.icon").click();
    await expect(item(alice, text)).toHaveCount(0);

    // Presence: Bob leaves.
    await bob.close();
    await expect(alice.locator("#online")).toHaveText("● 1 online");
  });
});
