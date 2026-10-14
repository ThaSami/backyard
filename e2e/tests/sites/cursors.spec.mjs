// examples/cursors: presence (members appear and disappear), live cursor
// positions, cursor chat and click reactions over one realtime room.
import { expect, openPage, siteURL, test } from "../fixtures.mjs";

const peopleNames = (page) => page.locator("#people li").allTextContents();

test("cursors, chat and reactions travel between people; members appear and disappear", async ({ newUser, state }) => {
  const url = siteURL(state, "cursors");
  const alice = await openPage(await newUser(), url);
  await expect(alice.locator("#status")).toHaveText("live");
  await expect(alice.locator("#people li")).toHaveCount(1);
  const aliceName = (await alice.locator("#people li").textContent()).replace(" (you)", "");
  expect(aliceName).toMatch(/^Guest [0-9a-f]{4}$/);

  // Bob joins: Alice gets a toast and sees two people.
  const bob = await openPage(await newUser(), url);
  await expect(bob.locator("#status")).toHaveText("live");
  await expect(bob.locator("#people li")).toHaveCount(2);
  const bobName = (await bob.locator("#people li", { hasText: "(you)" }).textContent()).replace(" (you)", "");
  await expect(alice.locator("#toasts .toast")).toContainText([`${bobName} joined`]);
  await expect.poll(() => peopleNames(alice)).toEqual([`${aliceName} (you)`, bobName]);
  await expect.poll(() => peopleNames(bob)).toEqual(expect.arrayContaining([`${bobName} (you)`, aliceName]));

  // Nobody has a cursor on the board until they move over it.
  await expect(alice.locator("#board .cursor")).toHaveCount(0);

  // Bob moves his pointer: Alice sees his named cursor at the same relative spot.
  const box = await bob.locator("#board").boundingBox();
  await bob.mouse.move(box.x + box.width * 0.25, box.y + box.height * 0.5);
  await bob.mouse.move(box.x + box.width * 0.5, box.y + box.height * 0.5, { steps: 5 });
  const cursor = alice.locator("#board .cursor");
  await expect(cursor).toHaveCount(1);
  await expect(cursor.locator(".who")).toHaveText(bobName);
  const aliceBoard = await alice.locator("#board").boundingBox();
  await expect
    .poll(async () => {
      const m = /translate\(([\d.]+)px, ([\d.]+)px\)/.exec(await cursor.evaluate((el) => el.style.transform));
      return m && [Math.round((Number(m[1]) / aliceBoard.width) * 100), Math.round((Number(m[2]) / aliceBoard.height) * 100)];
    })
    .toEqual([50, 50]);

  // Cursor chat: "/" opens the input; what Bob types shows next to his cursor.
  await bob.keyboard.press("/");
  await expect(bob.locator("#chat")).toBeVisible();
  await bob.keyboard.type("hello alice");
  await expect(cursor).toHaveClass(/talking/);
  await expect(cursor.locator(".say")).toHaveText("hello alice");
  await bob.keyboard.press("Escape");
  await expect(bob.locator("#chat")).toBeHidden();
  await expect(cursor.locator(".say")).toHaveText("");
  await expect(cursor).not.toHaveClass(/talking/);

  // Reactions: Bob picks 🎉 and clicks; it floats up on both screens.
  await bob.locator("#emojis button", { hasText: "🎉" }).click();
  await expect(bob.locator("#emojis button.active")).toHaveText("🎉");
  await bob.mouse.click(box.x + box.width * 0.6, box.y + box.height * 0.4);
  await expect(alice.locator("#board .reaction", { hasText: "🎉" })).toHaveCount(1);
  await expect(bob.locator("#board .reaction", { hasText: "🎉" })).toHaveCount(1); // echo: true

  // Leaving the board hides the cursor (state x/y = null) without leaving the room.
  await bob.mouse.move(box.x + box.width + 400, box.y + 5);
  await bob.locator("#board").dispatchEvent("pointerleave");
  await expect(cursor).toHaveCount(0);
  await expect(alice.locator("#people li")).toHaveCount(2);

  // Bob closes the tab: he disappears from Alice's list with a toast.
  await bob.mouse.move(box.x + 50, box.y + 50);
  await expect(alice.locator("#board .cursor")).toHaveCount(1);
  await bob.close();
  await expect(alice.locator("#board .cursor")).toHaveCount(0);
  await expect(alice.locator("#toasts .toast", { hasText: `${bobName} left` })).toHaveCount(1);
  await expect.poll(() => peopleNames(alice)).toEqual([`${aliceName} (you)`]);
});

test("one person with two tabs is listed once with a tab count", async ({ newUser, state }) => {
  const url = siteURL(state, "cursors");
  const carol = await newUser();
  const first = await openPage(carol, url);
  const second = await openPage(carol, url);
  await expect(second.locator("#status")).toHaveText("live");
  await expect(first.locator("#people li")).toHaveCount(1);
  await expect(first.locator("#people li small")).toHaveText(" ×2");
  await second.close();
  await expect(first.locator("#people li small")).toHaveCount(0);
});
