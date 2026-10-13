// examples/file-drop: upload (picker, drop, paste) → live gallery for everyone
// → filters, search, viewer → delete.
import { PNG_1X1 } from "../../harness/fake-ai.mjs";
import { expect, httpGet, openPage, rand, siteURL, test } from "../fixtures.mjs";

const card = (page, name) => page.locator("#grid .card", { has: page.locator(".name", { hasText: new RegExp(`^${name}$`) }) });

test("uploads appear in everyone's gallery, can be filtered, viewed and deleted", async ({ newUser, state }) => {
  const url = siteURL(state, "file-drop");
  const alice = await openPage(await newUser(), url);
  const bob = await openPage(await newUser(), url);
  await expect(alice.locator("#loading")).toBeHidden();
  await expect(bob.locator("#loading")).toBeHidden();

  const tag = rand();
  const png = `dot-${tag}.png`;
  const txt = `notes-${tag}.txt`;
  await alice.locator("#picker").setInputFiles([
    { name: png, mimeType: "image/png", buffer: PNG_1X1 },
    { name: txt, mimeType: "text/plain", buffer: Buffer.from("meeting notes\n") },
  ]);
  // The queue shows progress, then clears.
  await expect(alice.locator("#queue li.done")).toHaveCount(2);
  await expect(alice.locator("#queue li")).toHaveCount(0, { timeout: 15_000 });

  // Both galleries update (Bob through the change signal).
  for (const page of [alice, bob]) {
    await expect(card(page, png)).toBeVisible();
    await expect(card(page, txt)).toBeVisible();
  }
  await expect(card(alice, png).locator(".meta")).toContainText(" · you · ");
  await expect(card(bob, png).locator(".meta")).toContainText(" · a guest · ");
  await expect(card(bob, txt).locator(".meta")).toHaveText(/^14 B · /);
  await expect(card(bob, png).locator(".thumb img")).toHaveJSProperty("naturalWidth", 1);

  // The download link serves the bytes.
  const href = await card(bob, txt).locator("a.name").getAttribute("href");
  expect(href).toMatch(/^\/__backyard\/files\/[0-9A-Z]{26}\//);
  const file = await bob.evaluate((h) => fetch(h).then(async (r) => ({ status: r.status, type: r.headers.get("content-type"), text: await r.text() })), href);
  expect(file).toEqual({ status: 200, type: "text/plain; charset=utf-8", text: "meeting notes\n" });

  // Filters and search.
  await bob.locator('[data-kind="image"]').click();
  await expect(card(bob, png)).toBeVisible();
  await expect(card(bob, txt)).toHaveCount(0);
  await bob.locator('[data-kind="doc"]').click();
  await expect(card(bob, txt)).toBeVisible();
  await expect(card(bob, png)).toHaveCount(0);
  await bob.locator('[data-kind="all"]').click();
  await bob.locator("#search").fill(`notes-${tag}`);
  await expect(bob.locator("#grid .card")).toHaveCount(1);
  await bob.locator("#search").fill(`nothing-${tag}`);
  await expect(bob.locator("#empty")).toHaveText("Nothing matches this filter.");
  await bob.locator("#search").fill("");

  // The image viewer.
  await card(bob, png).locator(".thumb").click();
  await expect(bob.locator("#viewer")).toBeVisible();
  await expect(bob.locator("#viewer-caption")).toHaveText(png);
  await bob.locator("#viewer .close").click();
  await expect(bob.locator("#viewer")).toBeHidden();

  // Deleting asks first; cancelling keeps the file.
  bob.once("dialog", (d) => d.dismiss());
  await card(bob, png).locator("button.delete").click();
  await expect(card(bob, png)).toBeVisible();
  bob.once("dialog", (d) => {
    expect(d.message()).toBe(`Delete “${png}” for everyone?`);
    d.accept();
  });
  await card(bob, png).locator("button.delete").click();
  await expect(card(bob, png)).toHaveCount(0);
  await expect(card(alice, png)).toHaveCount(0);
  expect((await httpGet(new URL(await card(alice, txt).locator("a.name").getAttribute("href"), url).href)).status).toBe(200);

  alice.once("dialog", (d) => d.accept());
  await card(alice, txt).locator("button.delete").click();
  await expect(card(bob, txt)).toHaveCount(0);
});

test("files can be dropped onto the page or pasted", async ({ newUser, state }) => {
  const page = await openPage(await newUser(), siteURL(state, "file-drop"));
  await expect(page.locator("#loading")).toBeHidden();
  const tag = rand();

  const dropped = await page.evaluateHandle((name) => {
    const dt = new DataTransfer();
    dt.items.add(new File(["a,b\n1,2\n"], name, { type: "text/csv" }));
    return dt;
  }, `table-${tag}.csv`);
  await page.locator("#zone").dispatchEvent("dragover", { dataTransfer: dropped });
  await expect(page.locator("#zone")).toHaveClass(/over/);
  await page.locator("#zone").dispatchEvent("drop", { dataTransfer: dropped });
  await expect(page.locator("#zone")).not.toHaveClass(/over/);
  await expect(card(page, `table-${tag}.csv`)).toBeVisible();

  // The drop zone is a keyboard-operable file picker.
  const [chooser] = await Promise.all([page.waitForEvent("filechooser"), page.locator("#zone").press("Enter")]);
  expect(chooser.isMultiple()).toBe(true);
  await chooser.setFiles([{ name: `picked-${tag}.txt`, mimeType: "text/plain", buffer: Buffer.from("picked") }]);
  await expect(card(page, `picked-${tag}.txt`)).toBeVisible();

  // A pasted screenshot has no name: one is generated from its type.
  await page.evaluate(
    ([b64]) => {
      const bytes = Uint8Array.from(atob(b64), (c) => c.charCodeAt(0));
      const dt = new DataTransfer();
      dt.items.add(new File([bytes], "", { type: "image/png" }));
      document.dispatchEvent(new ClipboardEvent("paste", { clipboardData: dt, bubbles: true }));
    },
    [PNG_1X1.toString("base64")],
  );
  const pasted = page.locator("#grid .card .name", { hasText: /^pasted-\d+\.png$/ });
  await expect(pasted).toHaveCount(1);

  // Clean up.
  for (const name of [`table-${tag}.csv`, `picked-${tag}.txt`, await pasted.textContent()]) {
    page.once("dialog", (d) => d.accept());
    await card(page, name).locator("button.delete").click();
    await expect(card(page, name)).toHaveCount(0);
  }
});
