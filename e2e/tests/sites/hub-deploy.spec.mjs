// Deploying from the hub: the folder and file pickers, synthesized and real
// (OS-level) drag and drop, .backyard.json, diffed redeploys, deploy-token
// errors, and deleting a site with a deploy token.
import { mkdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";

import { deploySite } from "../../harness/backyard.mjs";
import { URLS } from "../../harness/env.mjs";
import { expect, httpGet, mainSiteURL, rand, test } from "../fixtures.mjs";

/** Writes files ({relative path: content}) below dir. */
function writeTree(dir, files) {
  for (const [p, content] of Object.entries(files)) {
    mkdirSync(dirname(join(dir, p)), { recursive: true });
    writeFileSync(join(dir, p), content);
  }
}

/** A small site whose script proves the SDK works: it shows backyard.me() and a db round trip. */
function sitePage(title) {
  return `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>${title}</title>
<meta name="description" content="Deployed by the e2e suite">
<link rel="icon" href="data:,">
<link rel="stylesheet" href="style.css">
<script src="/__backyard/backyard.js"></script>
<script src="app.js" defer></script></head>
<body><h1>${title}</h1><p id="who">…</p><p id="db">…</p></body></html>`;
}

const APP_JS = `backyard.me().then((me) => { document.querySelector("#who").textContent = "hello " + me.name; });
backyard.db.collection("checks").create({ ok: true }).then((d) => backyard.db.collection("checks").get(d.id))
  .then((d) => { document.querySelector("#db").textContent = "db ok " + d.ok; });
`;

/** Waits for the hub to finish loading. */
async function openHub(page, hash = "") {
  await page.goto("/" + hash);
  await expect(page.locator("#site-list")).not.toHaveAttribute("aria-busy", /.*/);
}

/** Clicks Deploy and waits for the result (fails with the hub's error message). */
async function deploy(page) {
  await expect(page.locator("#deploy-btn")).toBeEnabled();
  await page.locator("#deploy-btn").click();
  await expect(page.locator("#result:not([hidden]), #deploy-error:not([hidden])")).toBeVisible({ timeout: 30_000 });
  await expect(page.locator("#deploy-error"), "the hub reported a deploy error").toBeHidden();
}

/** Checks the deployed site in a new page: the SDK works and the page title is right. */
async function checkSite(context, url, title) {
  const site = await context.newPage();
  await site.goto(url);
  await expect(site).toHaveTitle(title);
  await expect(site.locator("#who")).toHaveText(/^hello Guest [0-9a-f]{4}$/);
  await expect(site.locator("#db")).toHaveText("db ok true");
  return site;
}

test.describe("hub deploy", () => {
  test("a folder from the folder picker becomes a site; redeploys are diffed", async ({ page, context }, testInfo) => {
    const name = `e2e-folder-${rand()}`;
    const dir = join(testInfo.outputPath("upload"), name);
    writeTree(dir, {
      "index.html": sitePage("Folder Deploy"),
      "app.js": APP_JS,
      "style.css": "body { font-family: system-ui; }\n",
      "about.html": "<!doctype html><title>About</title><p id=about>about page</p>",
      "assets/logo.svg": '<svg xmlns="http://www.w3.org/2000/svg" width="1" height="1"/>',
      ".backyardignore": "secret.txt\n",
      "secret.txt": "not for the web",
      ".git/HEAD": "ref: refs/heads/main\n",
      "node_modules/pkg/index.js": "module.exports = 1;\n",
      "CLAUDE.md": "agent notes",
    });

    await openHub(page);
    await expect(page.locator("#deploy-btn")).toBeDisabled();
    await expect(page.locator("#deploy-note")).toHaveText("Choose a folder to deploy.");
    await page.locator("#folder-input").setInputFiles(dir);

    // The name comes from the folder; ignored files are listed separately.
    await expect(page.locator("#site-name")).toHaveValue(name);
    await expect(page.locator("#site-url")).toHaveText(mainSiteURL(name));
    await expect(page.locator("#site-hint")).toHaveText("A new site will be created.");
    await expect(page.locator("#selection-summary")).toHaveText(new RegExp(`^${name} · 5 files · `));
    const ignored = await page.locator("#ignored-list li").allTextContents();
    expect(ignored).toEqual(expect.arrayContaining([".backyardignore", ".git/", "CLAUDE.md", "node_modules/", "secret.txt"]));
    await expect(page.locator("#file-list li")).toHaveCount(5);

    await deploy(page);
    const url = mainSiteURL(name);
    await expect(page.locator("#result-title")).toHaveText(`Deployed ${name}!`);
    await expect(page.locator("#result-link")).toHaveText(url);
    await expect(page.locator("#progress-text")).toHaveText("Deployed · 5 uploaded · 0 unchanged");
    await expect(page.locator(`.site[data-site="${name}"] .site-title a`)).toHaveText("Folder Deploy");
    await expect(page.locator(`.site[data-site="${name}"]`)).toHaveClass(/fresh/);

    // "Open site" opens the live site in a new tab.
    const [site] = await Promise.all([context.waitForEvent("page"), page.locator("#result-open").click()]);
    await site.waitForLoadState();
    expect(site.url()).toBe(url);
    await expect(site.locator("#db")).toHaveText("db ok true");
    await expect(site.locator("#who")).toHaveText(/^hello Guest /);
    expect((await httpGet(url + "about")).status).toBe(200); // clean URL → about.html
    expect((await httpGet(url + "assets/logo.svg")).headers["content-type"]).toBe("image/svg+xml");
    expect((await httpGet(url + "secret.txt")).status).toBe(404); // .backyardignore
    expect((await httpGet(url + "CLAUDE.md")).status).toBe(404); // default ignore

    // Change one file and remove another: only the change is uploaded, the
    // removed file disappears from the site.
    writeFileSync(join(dir, "style.css"), "body { font-family: serif; }\n");
    rmSync(join(dir, "about.html"));
    await page.locator("#folder-input").setInputFiles(dir);
    await expect(page.locator("#selection-summary")).toHaveText(new RegExp(`^${name} · 4 files · `));
    await expect(page.locator("#site-hint")).toHaveText(/^Updates the existing site/);
    await deploy(page);
    await expect(page.locator("#progress-text")).toHaveText("Deployed · 1 uploaded · 3 unchanged · 1 removed");
    expect((await httpGet(url + "about")).status).toBe(404);
    expect((await httpGet(url + "style.css")).text).toContain("serif");
  });

  test("the Choose folder… and Choose files… buttons open the right pickers", async ({ page }, testInfo) => {
    const dir = join(testInfo.outputPath("pick"), `e2e-pick-${rand()}`);
    writeTree(dir, { "index.html": sitePage("Picked"), "app.js": APP_JS, "style.css": "", "sub/page.html": "<p>sub</p>" });
    await openHub(page);

    const [folder] = await Promise.all([page.waitForEvent("filechooser"), page.locator("#pick-folder").click()]);
    expect(folder.isMultiple()).toBe(true);
    expect(await folder.element().evaluate((el) => el.webkitdirectory)).toBe(true);
    await folder.setFiles(dir);
    await expect(page.locator("#selection-summary")).toHaveText(/ · 4 files · /);

    const [files] = await Promise.all([page.waitForEvent("filechooser"), page.locator("#pick-files").click()]);
    expect(await files.element().evaluate((el) => el.webkitdirectory)).toBe(false);
    await files.setFiles([join(dir, "index.html"), join(dir, "app.js")]);
    await expect(page.locator("#selection-summary")).toHaveText(/^2 files · /);

    await page.locator("#clear-selection").click();
    await expect(page.locator("#selection")).toBeHidden();
    await expect(page.locator("#deploy-btn")).toBeDisabled();
  });

  test("dropping files on the drop zone deploys them", async ({ page, context }) => {
    const name = `e2e-drop-${rand()}`;
    await openHub(page);
    await page.locator("#site-name").fill(name);

    const dataTransfer = await page.evaluateHandle(
      ({ html, js }) => {
        const dt = new DataTransfer();
        dt.items.add(new File([html], "index.html", { type: "text/html" }));
        dt.items.add(new File([js], "app.js", { type: "text/javascript" }));
        dt.items.add(new File(["body{}"], "style.css", { type: "text/css" }));
        return dt;
      },
      { html: sitePage("Dropped Files"), js: APP_JS },
    );
    await page.locator("#dropzone").dispatchEvent("dragenter", { dataTransfer });
    await expect(page.locator("#dropzone")).toHaveClass(/dragging/);
    await page.locator("#dropzone").dispatchEvent("drop", { dataTransfer });
    await expect(page.locator("#dropzone")).not.toHaveClass(/dragging/);
    await expect(page.locator("#selection-summary")).toHaveText(/^3 files · /);
    await expect(page.locator("#site-name")).toHaveValue(name); // a typed name is kept

    await deploy(page);
    await expect(page.locator("#progress-text")).toHaveText("Deployed · 3 uploaded · 0 unchanged");
    const url = await page.locator("#result-link").textContent();
    await checkSite(context, url, "Dropped Files");
  });

  test("a folder dragged in from the desktop deploys its .backyard.json dir", async ({ page, context }, testInfo) => {
    // A real OS-level drop (CDP Input.dispatchDragEvent with a path): the hub
    // walks it with webkitGetAsEntry()/readEntries(), like a folder from Finder.
    const name = `e2e-osdrop-${rand()}`;
    const project = join(testInfo.outputPath("os-drop"), "My Project");
    writeTree(project, {
      ".backyard.json": JSON.stringify({ site: name, dir: "dist", ignore: ["*.map"] }),
      "README.md": "not deployed: outside dist/",
      "src/app.ts": "not deployed",
      "dist/index.html": sitePage("Dragged Folder"),
      "dist/app.js": APP_JS,
      "dist/app.js.map": "{}",
      "dist/style.css": "body{}",
      "dist/img/deep/pixel.svg": '<svg xmlns="http://www.w3.org/2000/svg"/>',
      "dist/.well-known/security.txt": "Contact: mailto:security@example.com\n",
      "dist/.env": "SECRET=1",
    });
    await openHub(page);
    const cdp = await context.newCDPSession(page);
    const box = await page.locator("#dropzone").boundingBox();
    const at = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
    const data = { items: [], files: [project], dragOperationsMask: 1 };
    await cdp.send("Input.dispatchDragEvent", { type: "dragEnter", ...at, data });
    await cdp.send("Input.dispatchDragEvent", { type: "dragOver", ...at, data });
    await expect(page.locator("#dropzone")).toHaveClass(/dragging/);
    await cdp.send("Input.dispatchDragEvent", { type: "drop", ...at, data });

    await expect(page.locator("#site-name")).toHaveValue(name); // from .backyard.json
    await expect(page.locator("#selection-summary")).toHaveText(/^My Project\/dist · 5 files · /);
    await expect(page.locator("#file-list li")).toHaveText([
      /^\.well-known\/security\.txt /,
      /^app\.js /,
      /^img\/deep\/pixel\.svg /,
      /^index\.html /,
      /^style\.css /,
    ]);
    expect(await page.locator("#ignored-list li").allTextContents()).toEqual([".env", "app.js.map"]);
    await deploy(page);
    await expect(page.locator("#progress-text")).toHaveText("Deployed · 5 uploaded · 0 unchanged");
    const url = mainSiteURL(name);
    await checkSite(context, url, "Dragged Folder");
    expect((await httpGet(url + ".well-known/security.txt")).status).toBe(200);
    expect((await httpGet(url + "img/deep/pixel.svg")).status).toBe(200);
    expect((await httpGet(url + "README.md")).status).toBe(404);
  });

  test("a rejected deploy token is reported and can be forgotten", async ({ page, pageProblems }) => {
    pageProblems.allow(/the server responded with a status of 401/); // the bogus token, on purpose
    await openHub(page, "#token=not-a-valid-token");
    await expect(page.locator("#token-box")).toBeVisible();
    await page.locator("#site-name").fill(`e2e-token-${rand()}`);
    await page.locator("#files-input").setInputFiles([{ name: "index.html", mimeType: "text/html", buffer: Buffer.from("<!doctype html><title>t</title>") }]);
    await expect(page.locator("#deploy-btn")).toBeEnabled();
    await page.locator("#deploy-btn").click();
    await expect(page.locator("#deploy-error")).toHaveText("The deploy token was rejected. Check it and try again.");
    await expect(page.locator("#token-box")).toHaveAttribute("open", "");
    await expect(page.locator("#deploy-token")).toBeFocused();

    // Without the token, anonymous deploys work on this server.
    await page.locator("#forget-token").click();
    await expect(page.locator("#toast")).toHaveText("Token removed from this browser");
    await expect(page.locator("#deploy-token")).toHaveValue("");
    await page.locator("#deploy-btn").click();
    await expect(page.locator("#result")).toBeVisible();
    await expect(page.locator("#deploy-error")).toBeHidden();
  });

  test("the file picker honors .backyard.json and validates the site name", async ({ page, context }) => {
    const name = `e2e-config-${rand()}`;
    await openHub(page);

    // Typing normalizes and validates the name.
    const input = page.locator("#site-name");
    await input.pressSequentially("My Site");
    await expect(input).toHaveValue("my-site");
    await input.fill("bad_name");
    await expect(page.locator("#site-hint")).toHaveText("Use lowercase letters, digits and dashes only.");
    await input.fill("admin");
    await expect(page.locator("#site-hint")).toHaveText('"admin" is reserved; pick another name.');
    await input.fill("");

    await page.locator("#files-input").setInputFiles([
      { name: ".backyard.json", mimeType: "application/json", buffer: Buffer.from(JSON.stringify({ site: name })) },
      { name: "index.html", mimeType: "text/html", buffer: Buffer.from(sitePage("Configured Site")) },
      { name: "app.js", mimeType: "text/javascript", buffer: Buffer.from(APP_JS) },
      { name: "style.css", mimeType: "text/css", buffer: Buffer.from("body{}") },
    ]);
    await expect(input).toHaveValue(name);
    await expect(page.locator("#selection-summary")).toHaveText(/^3 files · /);
    await deploy(page);
    await checkSite(context, await page.locator("#result-link").textContent(), "Configured Site");
  });

  test("a site can be deleted from the hub with a deploy token", async ({ page, context, state }, testInfo) => {
    const name = `e2e-delete-${rand()}`;
    const dir = testInfo.outputPath(name);
    writeTree(dir, { "index.html": sitePage("To Be Deleted"), "app.js": APP_JS, "style.css": "" });
    const { url } = deploySite(state.bin, { dir, site: name, server: URLS.main, home: testInfo.outputPath("cli") });

    // Guests cannot delete without a token.
    await openHub(page);
    await expect(page.locator(`.site[data-site="${name}"]`)).toBeVisible();
    await expect(page.locator(`button[aria-label="Delete ${name}"]`)).toHaveCount(0);

    // The token handed over in the URL fragment unlocks deleting.
    page = await context.newPage();
    await openHub(page, `#token=${encodeURIComponent(state.adminToken)}`);
    await expect(page.locator("#toast")).toHaveText(/^Deploy token (saved in this browser|set for this visit)$/);
    expect(new URL(page.url()).hash).toBe(""); // the fragment is removed at once
    await page.locator(`button[aria-label="Delete ${name}"]`).click();
    const dialog = page.locator("#delete-dialog");
    await expect(dialog).toBeVisible();
    await expect(page.locator("#delete-submit")).toBeDisabled();
    await page.locator("#delete-confirm").fill(name);
    await page.locator("#delete-submit").click();
    await expect(dialog).toBeHidden();
    await expect(page.locator(`.site[data-site="${name}"]`)).toHaveCount(0);
    await expect(page.locator("#toast")).toHaveText(`Deleted ${name}`);

    const gone = await httpGet(url);
    expect(gone.status).toBe(404);
    expect(gone.text).toContain("not found");
  });
});
