// examples/kpi-dashboard against a real warehouse: a Postgres source with the
// named query kpi_daily. Needs BACKYARD_TEST_POSTGRES_DSN (skipped without it).
import { join } from "node:path";

import { REPO } from "../../harness/backyard.mjs";
import { PORTS } from "../../harness/env.mjs";
import { startScratchServer } from "../../harness/scratch.mjs";
import { expect, openPage, test } from "../fixtures.mjs";

const DSN = process.env.BACKYARD_TEST_POSTGRES_DSN || "";
test.skip(!DSN, "set BACKYARD_TEST_POSTGRES_DSN to run the dashboard against a Postgres warehouse");

/** One row per day (newest = today): orders = 100 + day of month, returns = day of month % 7. */
const KPI_SQL = `SELECT d::date AS day,
       (100 + EXTRACT(DAY FROM d))::int AS orders,
       (EXTRACT(DAY FROM d)::int % 7) AS returns
FROM generate_series(CURRENT_DATE - (CAST(@days AS integer) - 1), CURRENT_DATE, interval '1 day') AS d
ORDER BY day`;

/** @type {import("../../harness/backyard.mjs").BackyardServer} */
let server;
let url = "";
test.beforeAll(async () => {
  const started = await startScratchServer({
    name: "warehouse",
    port: PORTS.warehouse,
    sites: { "kpi-dashboard": join(REPO, "examples", "kpi-dashboard") },
    config: (c) => ({
      ...c,
      warehouse: {
        sources: { default: { type: "postgres", dsn: DSN, description: "e2e Postgres" } },
        queries: { kpi_daily: { description: "Orders and returns per day", params: ["days"], sql: KPI_SQL } },
      },
    }),
  });
  server = started.server;
  url = started.sites["kpi-dashboard"];
});
test.afterAll(async () => {
  await server?.stop();
});

test("renders the named query's rows: tiles, chart and table", async ({ newUser }) => {
  const page = await openPage(await newUser(), url);
  await expect(page.locator("#source")).toHaveText("Warehouse · kpi_daily");
  await expect(page.locator("#source")).not.toHaveClass(/demo/);
  await expect(page.locator("#notice")).toBeHidden();
  await expect(page.locator("#tiles .tile .label")).toHaveText(["Orders · last 30 days", "Returns · last 30 days"]);
  await expect(page.locator("#svg path.line")).toHaveCount(2);
  await expect(page.locator("#table thead th")).toHaveText(["Day", "Orders", "Returns"]);
  await expect(page.locator("#table tbody tr")).toHaveCount(30);

  // The rows are the database's: compare with what the SQL computes for the server's today.
  const rows = await page.locator("#table tbody tr").evaluateAll((trs) => trs.map((tr) => [...tr.cells].map((td) => td.textContent)));
  for (const [day, orders, returns] of rows) {
    expect(day).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    const dom = Number(day.slice(8, 10));
    expect([orders, returns]).toEqual([String(100 + dom), String(dom % 7)]);
  }
  const days = rows.map((r) => r[0]);
  expect([...days].sort()).toEqual(days); // oldest first
  const total = rows.reduce((s, r) => s + Number(r[1]), 0);
  await expect(page.locator("#tiles .tile.hero .value")).toHaveText(new Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(total));

  // The range is a query parameter.
  await page.locator('[data-days="7"]').click();
  await expect(page.locator("#table tbody tr")).toHaveCount(7);
  await expect(page.locator("#table tbody tr").last().locator("td").first()).toHaveText(days.at(-1));
});
