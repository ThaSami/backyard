// Every page fits a phone screen without horizontal scrolling, in light and
// dark mode, is accessible there, and renders without errors.
import { PORTS } from "../../harness/env.mjs";
import { STANDARD_SITE_NAMES, standardSites, startScratchServer } from "../../harness/scratch.mjs";
import { expect, expectAccessible, test } from "../fixtures.mjs";

// A server of its own: these pages join the realtime rooms other specs count people in.
/** @type {import("../../harness/backyard.mjs").BackyardServer} */
let server;
/** @type {Record<string, string>} */
let sites = {};
test.beforeAll(async () => {
  ({ server, sites } = await startScratchServer({ name: "phone", port: PORTS.phone, sites: standardSites() }));
});
test.afterAll(async () => {
  await server?.stop();
});

const PAGES = [
  ["hub", () => `${server.url}/`],
  ["docs", () => `${server.url}/docs`],
  ...STANDARD_SITE_NAMES.map((s) => [s, () => sites[s]]),
  ["live-poll host view", () => sites["live-poll"] + "?host"],
];

for (const colorScheme of ["light", "dark"]) {
  test.describe(`${colorScheme} mode on a phone`, () => {
    test.use({ viewport: { width: 360, height: 740 }, colorScheme, isMobile: true, hasTouch: true });

    for (const [name, url] of PAGES) {
      test(name, async ({ page, pageProblems }) => {
        pageProblems.allow(/the server responded with a status of 501/); // kpi-dashboard: no warehouse
        await page.goto(url());
        await page.waitForLoadState("networkidle");
        const { scrollWidth, clientWidth, offenders } = await page.evaluate(() => {
          const root = document.scrollingElement;
          const offenders = [...document.querySelectorAll("body *")]
            .filter((el) => {
              const r = el.getBoundingClientRect();
              return r.width > 0 && r.right > root.clientWidth + 1 && getComputedStyle(el).position !== "fixed";
            })
            .slice(0, 5)
            .map((el) => `${el.tagName.toLowerCase()}${el.id ? "#" + el.id : ""}${el.className && typeof el.className === "string" ? "." + el.className.trim().split(/\s+/).join(".") : ""} (right ${Math.round(el.getBoundingClientRect().right)})`);
          return { scrollWidth: root.scrollWidth, clientWidth: root.clientWidth, offenders };
        });
        expect(scrollWidth, `horizontal overflow; widest elements: ${offenders.join(", ")}`).toBeLessThanOrEqual(clientWidth);
        // The page background follows the color scheme (light and dark mode are both styled).
        const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
        const [r, g, b] = bg.match(/\d+/g).map(Number);
        const light = (r + g + b) / 3 > 128;
        expect(light, `body background ${bg} in ${colorScheme} mode`).toBe(colorScheme === "light");

        // Accessibility (WCAG 2.1 A/AA rules of axe-core) of the first view on a phone.
        await expectAccessible(page, `${name} (${colorScheme})`);
      });
    }
  });
}
