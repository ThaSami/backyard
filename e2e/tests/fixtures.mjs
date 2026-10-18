// Shared fixtures: setup state, "users" (one browser context each, so each
// has its own cookies and therefore its own identity), and a guard that fails
// every test whose pages throw uncaught errors or log console errors.
import http from "node:http";

import AxeBuilder from "@axe-core/playwright";
import { test as base, expect } from "@playwright/test";

import { PORTS, readState } from "../harness/env.mjs";

export { expect };

/** Context options for every user (the project's `use` options do not apply to browser.newContext). */
const CONTEXT_DEFAULTS = { viewport: { width: 1280, height: 800 }, locale: "en-US", timezoneId: "UTC" };

export const test = base.extend({
  /** What global-setup.mjs started: URLs, tokens, deployed sites. */
  state: [
    async ({}, use) => {
      await use(readState());
    },
    { scope: "worker" },
  ],

  /** The context of Playwright's default `page` fixture gets the same defaults. */
  contextOptions: async ({ contextOptions }, use) => {
    await use({ ...CONTEXT_DEFAULTS, ...contextOptions });
  },

  /**
   * Collects uncaught exceptions (`pageerror`) and console errors from every
   * page of the test; after the test they must all be explained by
   * `pageProblems.allow(regexp)`. Network failures logged by the browser
   * ("Failed to load resource: … 501") count as console errors too, so tests
   * that provoke one on purpose allow it explicitly.
   */
  pageProblems: [
    async ({ context }, use, testInfo) => {
      const problems = [];
      const allowed = [];
      const watchPage = (page) => {
        const where = () => {
          try {
            return page.url();
          } catch {
            return "(closed)";
          }
        };
        page.on("pageerror", (err) => problems.push({ kind: "pageerror", text: `${err.name}: ${err.message}`, page: where() }));
        page.on("console", (msg) => {
          if (msg.type() !== "error") return;
          const loc = msg.location();
          problems.push({ kind: "console.error", text: msg.text(), page: where(), source: loc?.url ? `${loc.url}:${loc.lineNumber}` : "" });
        });
      };
      const watch = (ctx) => {
        for (const p of ctx.pages()) watchPage(p);
        ctx.on("page", watchPage);
      };
      watch(context);
      await use({
        watch,
        /** Accepts problems whose text matches re. */
        allow: (re) => allowed.push(re),
      });
      const unexpected = problems.filter((p) => !allowed.some((re) => re.test(p.text)));
      if (process.env.BACKYARD_E2E_PRINT_PROBLEMS && problems.length) console.log(`${testInfo.title}: ${JSON.stringify(problems, null, 1)}`);
      if (problems.length) await testInfo.attach("page-problems.json", { body: JSON.stringify(problems, null, 2), contentType: "application/json" });
      expect(unexpected, "uncaught page errors or console errors").toEqual([]);
    },
    { auto: true },
  ],

  /**
   * newUser() opens a new browser context: a new person with their own
   * cookies (a new guest identity on a server without sign-in). Closed after
   * the test.
   */
  newUser: async ({ browser, baseURL, pageProblems }, use) => {
    const contexts = [];
    await use(async (options = {}) => {
      const ctx = await browser.newContext({ ...CONTEXT_DEFAULTS, baseURL, ...options });
      pageProblems.watch(ctx);
      contexts.push(ctx);
      return ctx;
    });
    await Promise.all(contexts.map((c) => c.close()));
  },
});

/** The URL of a site deployed by global-setup (on the main server). */
export function siteURL(state, name) {
  const url = state.sites[name];
  if (!url) throw new Error(`site ${name} was not deployed by global-setup (have: ${Object.keys(state.sites).join(", ")})`);
  return url;
}

/** The URL site name has (or would have) on the main server (host mode on *.localhost). */
export function mainSiteURL(name) {
  return `http://${name}.localhost:${PORTS.main}/`;
}

/** Opens a page for user (a context) at url and waits for the SDK. */
export async function openPage(user, url) {
  const page = await user.newPage();
  await page.goto(url);
  await page.waitForFunction(() => typeof window.backyard === "object" && window.backyard !== null);
  return page;
}

/**
 * An HTTP request from Node (no cookies), resolving *.localhost to 127.0.0.1
 * like browsers do (Node's resolver does not). Requests from Node keep
 * expected errors (404 …) out of the pages' consoles.
 * @param {string} method
 * @param {string} url
 * @param {{headers?: Record<string, string>, json?: unknown}} [opts]
 * @returns {Promise<{status: number, headers: Record<string, string>, text: string}>}
 */
export function httpRequest(method, url, { headers = {}, json } = {}) {
  const u = new URL(url);
  const loopback = u.hostname === "localhost" || u.hostname.endsWith(".localhost");
  const body = json === undefined ? null : Buffer.from(JSON.stringify(json));
  return new Promise((resolve, reject) => {
    const req = http.request(
      {
        method,
        host: loopback ? "127.0.0.1" : u.hostname,
        port: u.port || 80,
        path: u.pathname + u.search,
        headers: { Host: u.host, ...(body ? { "Content-Type": "application/json", "Content-Length": body.length } : {}), ...headers },
      },
      (res) => {
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, text: Buffer.concat(chunks).toString() }));
      },
    );
    req.on("error", reject);
    req.setTimeout(10_000, () => req.destroy(new Error(`${method} ${url}: timeout`)));
    req.end(body);
  });
}

/** GET url from Node (see httpRequest). */
export const httpGet = (url, headers = {}) => httpRequest("GET", url, { headers });

/** Fails with a readable list of the WCAG 2.1 A/AA violations axe-core finds on page. */
export async function expectAccessible(page, what) {
  // axe measures the colors on screen: an element still fading in (an
  // entrance animation or a transition) reads as low contrast. Let the finite
  // ones finish; endless ones (a blinking cursor) are left alone.
  const settle = () =>
    page.evaluate(() =>
      Promise.all(
        document
          .getAnimations()
          .filter((a) => a.effect?.getComputedTiming().endTime !== Infinity)
          .map((a) => a.finished.catch(() => {})),
      ),
    );
  const analyze = async () => {
    await settle();
    return (await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"]).analyze()).violations;
  };
  // Realtime pages can start a new entrance animation while axe runs (another
  // test's note arrives): measure once more before reporting. A real
  // violation is still there the second time.
  let violations = await analyze();
  if (violations.length) violations = await analyze();
  const found = violations.flatMap((v) =>
    v.nodes.slice(0, 5).map((n) => `${v.id} (${v.impact}) at ${n.target.join(" ")}: ${n.failureSummary.split("\n").slice(1).join(" ").trim()}`),
  );
  expect(found, `accessibility violations: ${what}`).toEqual([]);
}

/** A short random suffix for test data. */
export const rand = () => Math.random().toString(36).slice(2, 8);
