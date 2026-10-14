// The docs page renders the skill's Markdown documents (/docs/<name>.md).
import { readFileSync } from "node:fs";
import { join } from "node:path";

import { REPO } from "../../harness/backyard.mjs";
import { expect, test } from "../fixtures.mjs";

const API_MD = readFileSync(join(REPO, "skills/backyard/references/api.md"), "utf8");
/** The level-2 headings of api.md (outside code fences). */
const API_H2 = API_MD.replace(/^```[\s\S]*?^```/gm, "")
  .split("\n")
  .filter((l) => l.startsWith("## "))
  .map((l) => l.slice(3).trim());

test("the API reference renders from /docs/api.md with a table of contents", async ({ page }) => {
  const md = await page.request.get("/docs/api.md");
  expect(md.ok()).toBe(true);
  expect(md.headers()["content-type"]).toMatch(/^text\/markdown/);
  // The served copy is the skill's document with this server's URLs in place of the examples'.
  const served = await md.text();
  const origin = new URL(page.url() === "about:blank" ? md.url() : page.url()).origin;
  expect(served).toContain(`\`${origin}\`; a site \`foo\` lives at`);
  expect(served).not.toContain("https://backyard.example.com");
  expect(served.split("\n").length).toBe(API_MD.split("\n").length);

  await page.goto("/docs");
  const doc = page.locator("#doc");
  await expect(doc.locator("h1")).toHaveText("Backyard API reference");
  await expect(page).toHaveTitle("Backyard API reference · Backyard E2E");
  await expect(doc.locator(".docs-status")).toHaveCount(0);
  // (Each heading ends with its "#" anchor link.)
  await expect(doc.locator("h2")).toHaveText(API_H2.map((h) => new RegExp(`^${h.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}\\s*#?$`)));
  await expect(page.locator('#doc-list a[data-doc="api"]')).toHaveAttribute("aria-current", "page");

  // Code blocks are highlighted text (never live HTML) with copy buttons.
  const firstBlock = doc.locator(".code-block").first();
  await expect(firstBlock.locator("pre")).toContainText('<script src="/__backyard/backyard.js"></script>');
  await expect(doc.locator("script")).toHaveCount(0);
  await expect(firstBlock.locator(".code-lang")).toHaveText("html");
  await expect(firstBlock.locator("button.copy")).toHaveText("Copy");
  // Code blocks scroll sideways: keyboard users can focus them to scroll.
  await expect(doc.locator(".code-block pre:not([tabindex='0'])")).toHaveCount(0);
  await firstBlock.locator("pre").focus();
  await expect(firstBlock.locator("pre")).toBeFocused();

  // The table of contents links to the headings.
  const toc = page.locator("#toc a");
  expect(await toc.count()).toBeGreaterThanOrEqual(API_H2.length);
  await page.locator('#toc a[href="#database"]').click();
  await expect(page).toHaveURL(/\/docs#database$/);
  await expect(doc.locator("h2#database")).toBeInViewport();
  // So do the document's own in-page links ("Contents: … [Errors](#errors) …").
  await doc.locator('p a[href="#errors"]').first().click();
  await expect(page).toHaveURL(/\/docs#errors$/);
  await expect(doc.locator("h2#errors")).toBeInViewport();
});

test("other documents load in place and keep their own URL", async ({ page, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 404/); // /docs?doc=nope (below)
  await page.goto("/docs");
  await expect(page.locator("#doc h1")).toHaveText("Backyard API reference");
  await page.locator('#doc-list a[data-doc="recipes"]').click();
  await expect(page).toHaveURL(/\/docs\?doc=recipes$/);
  await expect(page.locator('#doc-list a[data-doc="recipes"]')).toHaveAttribute("aria-current", "page");
  const recipes = readFileSync(join(REPO, "skills/backyard/references/recipes.md"), "utf8");
  await expect(page.locator("#doc h1")).toHaveText(/^#\s*(.+)$/m.exec(recipes)[1].trim());

  await page.goBack();
  await expect(page.locator("#doc h1")).toHaveText("Backyard API reference");

  // A deep link to a heading of another document.
  await page.goto("/docs?doc=deploy");
  await expect(page.locator('#doc-list a[data-doc="deploy"]')).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#doc h1")).toBeVisible();

  // An unknown document is reported, not rendered.
  await page.goto("/docs?doc=nope");
  await expect(page.locator("#doc .docs-status")).toHaveText("This document does not exist.");
});
