// The examples on the OIDC server: path mode (/s/<site>/, one origin for all
// sites), signed-in people with names and avatars, and no AI or warehouse.
import { URLS } from "../../harness/env.mjs";
import { expect, rand, test } from "../fixtures.mjs";
import { signedInPage } from "./helpers.mjs";

const ALICE_AVATAR = (state) => `${state.idp.issuer}/avatars/alice.png`;

test("file-drop names uploaders with backyard.id.lookup and serves path-mode file URLs", async ({ newUser, state }) => {
  const url = state.oidcSites["file-drop"];
  const alice = await signedInPage(newUser, state, "alice", url);
  const bob = await signedInPage(newUser, state, "bob", url);
  await expect(bob.locator("#loading")).toBeHidden();

  const name = `report-${rand()}.txt`;
  await alice.locator("#picker").setInputFiles([{ name, mimeType: "text/plain", buffer: Buffer.from("quarterly numbers") }]);
  const card = (page) => page.locator("#grid .card", { has: page.locator(".name", { hasText: name }) });
  await expect(card(bob).locator(".meta")).toContainText(" · Alice Smith · ");
  await expect(card(alice).locator(".meta")).toContainText(" · you · ");

  const href = await card(bob).locator("a.name").getAttribute("href");
  expect(href).toMatch(new RegExp(`^/s/file-drop/__backyard/files/[0-9A-Z]{26}/${name.replace(".", "\\.")}$`));
  expect(await bob.evaluate((h) => fetch(h).then((r) => r.text()), href)).toBe("quarterly numbers");

  bob.once("dialog", (d) => d.accept());
  await card(bob).locator("button.delete").click();
  await expect(card(alice)).toHaveCount(0);
});

test("lunch-vote shows who voted with their avatars", async ({ newUser, state }) => {
  const url = state.oidcSites["lunch-vote"];
  const alice = await signedInPage(newUser, state, "alice", url);
  const bob = await signedInPage(newUser, state, "bob", url);
  await expect(alice.locator("#who")).toHaveText("voting as Alice Smith");
  await expect(bob.locator("#here")).toHaveText("2 deciding");
  // Alice has a picture; Bob gets initials.
  await expect(bob.locator(`#stack img.avatar[title="Alice Smith"]`)).toHaveAttribute("src", ALICE_AVATAR(state));
  await expect(alice.locator(`#stack span.avatar[title="Bob Jones"]`)).toHaveText("BJ");

  const place = `Sushi ${rand()}`;
  await alice.locator("#name").fill(place);
  await alice.locator("#add button").click();
  const row = (page) => page.locator("li.spot", { has: page.locator(".name", { hasText: place }) });
  await row(bob).locator("button.vote").click();
  await row(alice).locator("button.vote").click();
  await expect(row(bob).locator(".voters .avatar")).toHaveCount(2);
  await expect(row(bob).locator(`.voters img.avatar[title="Alice Smith"]`)).toHaveJSProperty("naturalWidth", 1);
  await expect(row(alice).locator(`.voters span.avatar[title="Bob Jones"]`)).toHaveText("BJ");

  await row(bob).locator("button.vote").click();
  await row(alice).locator("button.vote").click();
  await row(alice).locator("button.remove").click();
  await expect(row(bob)).toHaveCount(0);
});

test("live-poll and cursors work in path mode with real names", async ({ newUser, state }) => {
  const poll = state.oidcSites["live-poll"];
  const host = await signedInPage(newUser, state, "alice", poll + "?host");
  await expect(host.locator("#h-link")).toHaveText(poll);
  const question = `Signed-in poll ${rand()}`;
  await host.locator("#h-question").fill(question);
  await host.locator("#h-options input").nth(0).fill("Yes");
  await host.locator("#h-options input").nth(1).fill("No");
  await host.locator("#h-form button[type=submit]").click();
  const voter = await signedInPage(newUser, state, "bob", poll);
  await expect(voter.locator("#a-question")).toHaveText(question);
  await voter.locator("#a-options button", { hasText: "Yes" }).click();
  await expect(host.locator("#h-results .total")).toHaveText("1 answer");
  const [answer] = await host.evaluate(() => backyard.db.collection("answers").where("choice", "==", 0).orderBy("_meta.created_at", "desc").limit(1).get());
  expect(answer).toMatchObject({ name: "Bob Jones", _meta: { created_by: "bob@example.com" } });

  const cursors = state.oidcSites.cursors;
  const a = await signedInPage(newUser, state, "alice", cursors);
  const b = await signedInPage(newUser, state, "bob", cursors);
  await expect(a.locator("#people li")).toHaveText(["Alice Smith (you)", "Bob Jones"]);
  const box = await b.locator("#board").boundingBox();
  await b.mouse.move(box.x + 100, box.y + 100);
  await b.mouse.move(box.x + 120, box.y + 110);
  await expect(a.locator("#board .cursor .who")).toHaveText("Bob Jones");
});

test("ai-chat and kpi-dashboard explain what is not configured on this server", async ({ newUser, state, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 501/); // AI and warehouse are off here
  const chat = await signedInPage(newUser, state, "alice", state.oidcSites["ai-chat"]);
  await expect(chat.locator("#banner")).toHaveText("AI is not enabled on this Backyard server. Ask an admin to configure a provider.");
  await expect(chat.locator("#input")).toBeDisabled();
  await expect(chat.locator("#send")).toBeDisabled();
  await expect(chat.locator("#model option")).toHaveText(["unavailable"]);
  await expect(chat.locator("#hello")).toHaveText("What can I help with, Alice?");

  const kpi = await signedInPage(newUser, state, "bob", state.oidcSites["kpi-dashboard"]);
  await expect(kpi.locator("#notice-text")).toHaveText("The data warehouse is not configured on this Backyard server, so this page shows demo data.");
  await expect(kpi.locator("#table tbody tr")).toHaveCount(30);
});

test("sites share the hub's origin in path mode, but not their data", async ({ newUser, state }) => {
  const a = await signedInPage(newUser, state, "alice", state.oidcSites.hello);
  expect(new URL(a.url()).origin).toBe(URLS.oidc);
  const text = `Only on hello ${rand()}`;
  await a.locator("#note").fill(text);
  await a.locator("#post").click();
  await expect(a.locator("#notes li", { hasText: text })).toBeVisible();
  // The same collection name on another site is a different collection.
  const other = await a.evaluate(async (t) => {
    const res = await fetch("/s/team-app/__backyard/api/db/guestbook/query", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Backyard": "1" },
      body: JSON.stringify({ where: [{ field: "text", op: "==", value: t }] }),
    });
    return (await res.json()).docs.length;
  }, text);
  expect(other).toBe(0);
  await a.locator("#notes li", { hasText: text }).locator("button.del").click();
  await expect(a.locator("#notes li", { hasText: text })).toHaveCount(0);
});
