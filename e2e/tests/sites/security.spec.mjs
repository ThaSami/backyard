// Browser-enforced security properties of host mode: uploaded documents are
// sandboxed, and one site cannot use another site's API or websocket.
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import { deploySite } from "../../harness/backyard.mjs";
import { URLS, readState } from "../../harness/env.mjs";
import { expect, openPage, rand, test } from "../fixtures.mjs";

const PAGE = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Security probe</title><link rel="icon" href="data:,">
<script src="/__backyard/backyard.js"></script></head><body><h1>probe</h1></body></html>`;

/** @type {{a: string, b: string}} */
const sites = { a: "", b: "" };
test.beforeAll(async ({}, testInfo) => {
  const state = readState();
  for (const key of ["a", "b"]) {
    const name = `e2e-sec-${key}-${rand()}`;
    const dir = join(testInfo.outputPath("sites"), name);
    mkdirSync(dir, { recursive: true });
    writeFileSync(join(dir, "index.html"), PAGE);
    sites[key] = deploySite(state.bin, { dir, site: name, server: URLS.main, home: testInfo.outputPath("cli") }).url;
  }
});

test("uploaded HTML and SVG cannot run script with the site's origin", async ({ newUser, pageProblems }) => {
  // Chromium reports the blocked scripts: that is the point of the test.
  pageProblems.allow(/^Blocked script execution in '.*' because the document's frame is sandboxed/);
  const user = await newUser();
  const page = await openPage(user, sites.a);
  const uploaded = await page.evaluate(async () => {
    const html = `<!doctype html><title>uploaded</title><script>document.title = "script ran"; fetch("/__backyard/api/db/stolen", {method: "POST", headers: {"X-Backyard": "1", "Content-Type": "application/json"}, body: "{}"});</script>`;
    const svg = `<svg xmlns="http://www.w3.org/2000/svg"><script>document.documentElement.setAttribute("data-ran", "1")</script></svg>`;
    const h = await backyard.files.upload(new Blob([html], { type: "text/html" }), { name: "page.html" });
    const s = await backyard.files.upload(new Blob([svg], { type: "image/svg+xml" }), { name: "image.svg" });
    return { html: new URL(h.url, location.href).href, svg: new URL(s.url, location.href).href };
  });

  const doc = await user.newPage();
  const res = await doc.goto(uploaded.html);
  expect(res.headers()["content-security-policy"]).toBe("sandbox");
  expect(res.headers()["content-disposition"]).toMatch(/^inline; filename=/);
  await expect(doc).toHaveTitle("uploaded");
  const svgRes = await doc.goto(uploaded.svg);
  expect(svgRes.headers()["content-security-policy"]).toBe("sandbox");
  expect(await doc.evaluate(() => document.documentElement.getAttribute("data-ran"))).toBeNull();

  // Nothing was written by the uploaded page.
  expect(await page.evaluate(() => backyard.db.collections())).not.toContain("stolen");
});

test("a site cannot call another site's API or open its websocket", async ({ newUser, pageProblems }) => {
  pageProblems.allow(/has been blocked by CORS policy/);
  pageProblems.allow(/^WebSocket connection to 'ws:\/\/e2e-sec-b-.*' failed/);
  pageProblems.allow(/net::ERR_FAILED/);
  pageProblems.allow(/the server responded with a status of 403/);
  const user = await newUser();
  const b = await openPage(user, sites.b);
  await b.evaluate(() => backyard.db.collection("notes").create({ text: "b's own note" }, { id: "own" }));
  const a = await openPage(user, sites.a);

  const attempts = await a.evaluate(async (target) => {
    const out = {};
    // With the CSRF header: the CORS preflight fails.
    out.preflighted = await fetch(`${target}__backyard/api/db/notes`, {
      method: "POST",
      credentials: "include",
      headers: { "X-Backyard": "1", "Content-Type": "application/json" },
      body: JSON.stringify({ text: "from a, with header" }),
    }).then(
      (r) => `status ${r.status}`,
      (e) => e.name,
    );
    // A "simple" cross-site request (no preflight): the server refuses it.
    await fetch(`${target}__backyard/api/db/notes`, {
      method: "POST",
      mode: "no-cors",
      credentials: "include",
      headers: { "Content-Type": "text/plain" },
      body: JSON.stringify({ text: "from a, simple request" }),
    }).catch(() => {});
    // Reading another site's data.
    out.read = await fetch(`${target}__backyard/api/db/notes`, { credentials: "include" }).then(
      (r) => `status ${r.status}`,
      (e) => e.name,
    );
    out.ws = await new Promise((resolve) => {
      const ws = new WebSocket(`${target.replace("http:", "ws:")}__backyard/api/ws`);
      ws.onopen = () => resolve("open");
      ws.onerror = () => resolve("error");
    });
    return out;
  }, sites.b);
  expect(attempts).toEqual({ preflighted: "TypeError", read: "TypeError", ws: "error" });

  // Site b's notes are untouched.
  const notes = await b.evaluate(() => backyard.db.collection("notes").list());
  expect(notes.map((n) => n.text)).toEqual(["b's own note"]);
});
