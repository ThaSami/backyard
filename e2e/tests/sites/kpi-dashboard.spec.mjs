// examples/kpi-dashboard on a server without a warehouse: it explains why and
// renders clearly labeled demo data (tiles, SVG chart with tooltip, table).
import { expect, openPage, siteURL, test } from "../fixtures.mjs";

test.beforeEach(({ pageProblems }) => {
  // backyard.warehouse.sources() answers 501 unsupported on this server, on purpose.
  pageProblems.allow(/the server responded with a status of 501/);
});

test("falls back to labeled demo data when the warehouse is not configured", async ({ newUser, state }) => {
  const page = await openPage(await newUser(), siteURL(state, "kpi-dashboard"));

  await expect(page.locator("#notice")).toBeVisible();
  await expect(page.locator("#notice-text")).toHaveText(
    "The data warehouse is not configured on this Backyard server, so this page shows demo data.",
  );
  await expect(page.locator("#source")).toHaveText("Demo data");
  await expect(page.locator("#source")).toHaveClass(/demo/);

  // Two numeric series (orders, returns): two tiles, two lines, a legend, 30 rows.
  await expect(page.locator("#tiles .tile")).toHaveCount(2);
  await expect(page.locator("#tiles .tile .label")).toHaveText(["Orders · last 30 days", "Returns · last 30 days"]);
  await expect(page.locator("#tiles .tile.hero .delta")).toHaveText(/^[▲▼] \d+\.\d% · last 7 days vs the 7 before$/);
  await expect(page.locator("#svg path.line")).toHaveCount(2);
  await expect(page.locator("#svg path.area")).toHaveCount(0); // only drawn for a single series
  await expect(page.locator("#legend .legend-item")).toHaveText(["Orders", "Returns"]);
  await expect(page.locator("#chart-title")).toHaveText("Daily trend");
  await expect(page.locator("#empty")).toBeHidden();
  await expect(page.locator("#table thead th")).toHaveText(["Day", "Orders", "Returns"]);
  await expect(page.locator("#table tbody tr")).toHaveCount(30);

  // The demo data ends today (the browser's calendar day) and is deterministic.
  const today = await page.evaluate(() => new Date().toLocaleDateString("en-CA"));
  await page.locator(".table-card summary").click();
  await expect(page.locator("#table tbody tr").last().locator("td").first()).toHaveText(today);
  const firstOrders = await page.locator("#table tbody tr").first().locator("td").nth(1).textContent();
  await page.reload();
  await expect(page.locator("#table tbody tr").first().locator("td").nth(1)).toHaveText(firstOrders);

  // Range buttons reload the data.
  await page.locator('[data-days="7"]').click();
  await expect(page.locator('[data-days="7"]')).toHaveClass(/active/);
  await expect(page.locator("#table tbody tr")).toHaveCount(7);
  await expect(page.locator("#tiles .tile .label").first()).toHaveText("Orders · last 7 days");
  await page.locator('[data-days="90"]').click();
  await expect(page.locator("#table tbody tr")).toHaveCount(90);
  await expect(page.locator("#main")).not.toHaveClass(/loading/);
});

test("the chart has a crosshair tooltip for the pointer and the keyboard", async ({ newUser, state }) => {
  const page = await openPage(await newUser(), siteURL(state, "kpi-dashboard"));
  await expect(page.locator("#svg path.line")).toHaveCount(2);

  const box = await page.locator("#svg").boundingBox();
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  const tip = page.locator("#tip");
  await expect(tip).toBeVisible();
  await expect(tip.locator(".tip-row")).toHaveCount(2);
  await expect(tip.locator(".tip-row span:last-child")).toHaveText(["Orders", "Returns"]);
  await expect(tip.locator(".tip-head")).toHaveText(/^\w{3}, \w{3} \d{1,2}, \d{4}$/);
  await expect(page.locator("#svg line.cross")).toHaveAttribute("visibility", "visible");

  await page.mouse.move(box.x + box.width / 2, box.y + box.height + 200); // leave the chart
  await expect(tip).toBeHidden();

  // Keyboard: focus the chart, arrows move the crosshair, Escape hides it.
  await page.locator("#svg").focus();
  await page.keyboard.press("ArrowLeft");
  await expect(tip).toBeVisible();
  const last = await page.locator("#table tbody tr").last().locator("td").first().textContent();
  const secondToLast = await page.locator("#table tbody tr").nth(-2).locator("td").first().textContent();
  const longDate = (s) => page.evaluate((d) => {
    const [y, m, day] = d.split("-").map(Number);
    return new Date(y, m - 1, day).toLocaleDateString(undefined, { weekday: "short", month: "short", day: "numeric", year: "numeric" });
  }, s);
  await expect(tip.locator(".tip-head")).toHaveText(await longDate(secondToLast));
  await page.keyboard.press("ArrowRight");
  await expect(tip.locator(".tip-head")).toHaveText(await longDate(last));
  await page.keyboard.press("Escape");
  await expect(tip).toBeHidden();
});
