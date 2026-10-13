// examples/lunch-vote: two people add places and vote; everything syncs live.
import { expect, openPage, rand, siteURL, test } from "../fixtures.mjs";

/** The row of a place. */
const spot = (page, name) => page.locator("li.spot", { has: page.locator(".name", { hasText: new RegExp(`^${name}$`) }) });

test("votes, places and presence sync between two people in realtime", async ({ newUser, state, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 409/); // adding a duplicate place (below)
  const url = siteURL(state, "lunch-vote");
  const alice = await openPage(await newUser(), url);
  const bob = await openPage(await newUser(), url);
  const aliceName = (await alice.locator("#who").textContent()).replace("voting as ", "");
  const bobName = (await bob.locator("#who").textContent()).replace("voting as ", "");
  expect(aliceName).toMatch(/^Guest [0-9a-f]{4}$/);
  expect(bobName).not.toBe(aliceName);

  // Presence: both see two people deciding.
  await expect(alice.locator("#here")).toHaveText("2 deciding");
  await expect(bob.locator("#here")).toHaveText("2 deciding");
  await expect(alice.locator("#stack .avatar")).toHaveCount(2);

  // Alice adds a place: it shows up for Bob without a reload.
  const place = `Pho ${rand()}`;
  await alice.locator("#emoji").selectOption("🍲");
  await alice.locator("#name").fill(place);
  await alice.locator("#add button").click();
  await expect(alice.locator("#name")).toHaveValue("");
  await expect(spot(bob, place)).toBeVisible();
  await expect(spot(bob, place).locator(".emoji")).toHaveText("🍲");
  await expect(spot(bob, place).locator(".count")).toHaveText("0");

  // Adding it again is rejected (the id is the slug of the name).
  await bob.locator("#name").fill(place.toUpperCase());
  await bob.locator("#add button").click();
  await expect(bob.locator("#error")).toHaveText(`${place.toUpperCase()} is already on the list.`);

  // Bob votes: Alice sees the count, the leader and Bob's avatar on the row.
  await spot(bob, place).locator("button.vote").click();
  await expect(spot(bob, place).locator("button.vote")).toHaveAttribute("aria-pressed", "true");
  await expect(spot(bob, place)).toHaveClass(/mine/);
  await expect(spot(alice, place).locator(".count")).toHaveText("1");
  await expect(spot(alice, place).locator("button.vote")).toHaveAttribute("aria-pressed", "false");
  await expect(spot(alice, place).locator(".voters .avatar")).toHaveAttribute("title", bobName);
  await expect(alice.locator("#leader")).toContainText(place);
  // Places with votes cannot be removed.
  await expect(spot(alice, place).locator("button.remove")).toBeDisabled();

  // Alice votes too: two votes, two voters.
  await spot(alice, place).locator("button.vote").click();
  await expect(spot(bob, place).locator(".count")).toHaveText("2");
  await expect(spot(bob, place).locator(".voters .avatar")).toHaveCount(2);

  // A vote can move: Bob picks another place, then takes that vote back.
  const other = `Tacos ${rand()}`;
  await bob.locator("#name").fill(other);
  await bob.locator("#add button").click();
  await expect(spot(alice, other)).toBeVisible();
  await spot(bob, other).locator("button.vote").click();
  await expect(spot(alice, other).locator(".count")).toHaveText("1");
  await expect(spot(alice, place).locator(".count")).toHaveText("1");
  await spot(bob, other).locator("button.vote").click(); // click again = take it back
  await expect(spot(bob, other).locator("button.vote")).toHaveAttribute("aria-pressed", "false");
  await expect(spot(alice, other).locator(".count")).toHaveText("0");

  // Without votes a place can be removed; it disappears for everyone.
  await spot(alice, other).locator("button.remove").click();
  await expect(spot(bob, other)).toHaveCount(0);

  // Presence: when Bob leaves, Alice is alone again.
  await bob.close();
  await expect(alice.locator("#here")).toHaveText("just you so far");

  // Clean up: Alice takes her vote back and removes the place.
  await spot(alice, place).locator("button.vote").click();
  await expect(spot(alice, place).locator(".count")).toHaveText("0");
  await spot(alice, place).locator("button.remove").click();
  await expect(spot(alice, place)).toHaveCount(0);
});

test("a reload restores the live state", async ({ newUser, state }) => {
  const url = siteURL(state, "lunch-vote");
  const user = await newUser();
  const page = await openPage(user, url);
  const place = `Ramen ${rand()}`;
  await page.locator("#name").fill(place);
  await page.locator("#add button").click();
  await spot(page, place).locator("button.vote").click();
  await expect(spot(page, place).locator(".count")).toHaveText("1");

  await page.reload();
  await expect(page.locator("#loading")).toBeHidden();
  await expect(spot(page, place).locator(".count")).toHaveText("1");
  await expect(spot(page, place).locator("button.vote")).toHaveAttribute("aria-pressed", "true"); // same guest identity (cookie)

  await spot(page, place).locator("button.vote").click();
  await spot(page, place).locator("button.remove").click();
  await expect(spot(page, place)).toHaveCount(0);
});
