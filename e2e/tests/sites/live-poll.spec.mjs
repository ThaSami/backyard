// examples/live-poll: the host (?host) runs rounds, the audience answers live.
import { expect, openPage, rand, siteURL, test } from "../fixtures.mjs";

const optionButton = (page, text) => page.locator("#a-options button.option", { hasText: new RegExp(`^${text}$`) });
const resultRow = (page, container, text) =>
  page.locator(`${container} .result`, { has: page.locator(".opt", { hasText: new RegExp(`^${text}$`) }) });

test("host starts a round, the audience answers, results update live for everyone", async ({ newUser, state }) => {
  const url = siteURL(state, "live-poll");
  const host = await openPage(await newUser(), url + "?host");
  await expect(host.locator("#host")).toBeVisible();
  await expect(host.locator("#audience")).toBeHidden();
  await expect(host.locator("#h-link")).toHaveText(url);
  await host.context().grantPermissions(["clipboard-read", "clipboard-write"]);
  await host.locator("#h-copy").click();
  await expect(host.locator("#toast")).toHaveText("Link copied");
  expect(await host.evaluate(() => navigator.clipboard.readText())).toBe(url);

  const ann = await openPage(await newUser(), url);
  const ben = await openPage(await newUser(), url);
  await expect(ann.locator("#audience")).toBeVisible();
  await expect(host.locator("#h-people")).toHaveText("3 watching");

  // A new round with three options (a fourth one added and removed again).
  const question = `Where should the offsite be? ${rand()}`;
  await host.locator("#h-question").fill(question);
  await host.locator("#h-add-option").click();
  await expect(host.locator("#h-options .option-row")).toHaveCount(4);
  await host.locator("#h-options .option-row").last().locator("button").click();
  await expect(host.locator("#h-options .option-row")).toHaveCount(3);
  const options = ["Lisbon", "Montreal", "Kyoto"];
  for (const [i, opt] of options.entries()) await host.locator("#h-options input").nth(i).fill(opt);
  await host.locator("#h-form button[type=submit]").click();
  await expect(host.locator("#toast")).toHaveText("New round started");
  await expect(host.locator("#h-status")).toHaveText(/^Round \d+ · voting open$/);
  const round = Number((await host.locator("#h-status").textContent()).match(/Round (\d+)/)[1]);
  await expect(host.locator("#h-question-live")).toHaveText(question);
  await expect(host.locator("#h-results .total")).toHaveText("0 answers");

  // The audience sees the question; results stay hidden until they answer.
  for (const page of [ann, ben]) {
    await expect(page.locator("#a-question")).toHaveText(question);
    await expect(page.locator("#a-live")).toBeVisible();
    await expect(page.locator("#a-options button.option")).toHaveText(options);
    await expect(page.locator("#a-hint")).toHaveText("Tap an answer. You can change it until voting closes.");
    await expect(page.locator("#a-results")).toBeHidden();
  }

  // Ann answers Kyoto: she sees the results, the host sees 1 answer.
  await optionButton(ann, "Kyoto").click();
  await expect(optionButton(ann, "Kyoto")).toHaveAttribute("aria-pressed", "true");
  await expect(ann.locator("#a-results")).toBeVisible();
  await expect(ann.locator("#a-hint")).toHaveText("Thanks! You can still change your answer.");
  await expect(resultRow(host, "#h-results", "Kyoto").locator(".num")).toHaveText("100% · 1");
  await expect(resultRow(host, "#h-results", "Kyoto")).toHaveClass(/top/);
  await expect(host.locator("#h-results .total")).toHaveText("1 answer");
  await expect(ben.locator("#a-results")).toBeHidden(); // Ben has not answered yet

  // Ben answers Lisbon; Ann changes her mind to Lisbon too.
  await optionButton(ben, "Lisbon").click();
  await expect(resultRow(host, "#h-results", "Lisbon").locator(".num")).toHaveText("50% · 1");
  await optionButton(ann, "Lisbon").click();
  await expect(resultRow(host, "#h-results", "Lisbon").locator(".num")).toHaveText("100% · 2");
  await expect(resultRow(host, "#h-results", "Kyoto").locator(".num")).toHaveText("0% · 0");
  await expect(resultRow(ben, "#a-results", "Lisbon").locator(".num")).toHaveText("100% · 2");
  await expect(host.locator("#h-results .total")).toHaveText("2 answers");

  // Closing the vote disables the buttons and shows the results to everyone.
  await host.locator("#h-toggle").click();
  await expect(host.locator("#h-status")).toHaveText(`Round ${round} · voting closed`);
  await expect(host.locator("#h-toggle")).toHaveText("Reopen voting");
  for (const page of [ann, ben]) {
    await expect(page.locator("#a-hint")).toHaveText("Voting is closed. Here is how the room answered.");
    await expect(page.locator("#a-live")).toBeHidden();
    await expect(page.locator("#a-options button.option").first()).toBeDisabled();
    await expect(page.locator("#a-results .total")).toHaveText("2 answers");
  }

  // A latecomer sees the closed round and its results right away.
  const cat = await openPage(await newUser(), url);
  await expect(cat.locator("#a-question")).toHaveText(question);
  await expect(resultRow(cat, "#a-results", "Lisbon").locator(".num")).toHaveText("100% · 2");
  await expect(host.locator("#h-people")).toHaveText("4 watching");

  // The next round starts empty for everyone.
  await host.locator("#h-question").fill(`Second question ${rand()}`);
  await host.locator("#h-form button[type=submit]").click();
  await expect(host.locator("#h-status")).toHaveText(`Round ${round + 1} · voting open`);
  await expect(host.locator("#h-results .total")).toHaveText("0 answers");
  for (const page of [ann, ben, cat]) {
    await expect(page.locator("#a-question")).toHaveText(/^Second question /);
    await expect(page.locator("#a-results")).toBeHidden();
    await expect(page.locator("#a-options button.chosen")).toHaveCount(0);
  }

  // People leaving are counted out.
  await cat.close();
  await ben.close();
  await expect(host.locator("#h-people")).toHaveText("2 watching");
});

test("the host needs two different options", async ({ newUser, state }) => {
  const host = await openPage(await newUser(), siteURL(state, "live-poll") + "?host");
  await host.locator("#h-question").fill("Only one option?");
  await host.locator("#h-options input").nth(0).fill("Same");
  await host.locator("#h-options input").nth(1).fill("Same");
  const before = await host.locator("#h-status").textContent();
  await host.locator("#h-form button[type=submit]").click();
  await expect(host.locator("#toast")).toHaveText("Give at least two different options.");
  await expect(host.locator("#h-status")).toHaveText(before);
});
