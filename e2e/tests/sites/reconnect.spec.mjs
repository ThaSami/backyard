// SDK reconnects: a dedicated server is killed (SIGKILL, like a crash) and
// started again; open pages must notice, reconnect on their own, re-subscribe
// their db subscriptions and live queries, and re-join their rooms.
import { join } from "node:path";

import { PORTS, readState } from "../../harness/env.mjs";
import { startScratchServer } from "../../harness/scratch.mjs";
import { expect, httpRequest, openPage, rand, test } from "../fixtures.mjs";

const item = (page, text) => page.locator("#list li.item", { has: page.locator(".text", { hasText: text }) });

/** @type {import("../../harness/backyard.mjs").BackyardServer} */
let server;
let SITE = "";

test.beforeAll(async () => {
  // The `backyard init --template app` page: live list (watch) + presence room + offline pill.
  // The config (and so the secret) is kept across restarts: guest cookies stay valid.
  const started = await startScratchServer({
    name: "reconnect",
    port: PORTS.reconnect,
    sites: { reconnect: join(readState().tmp, "init", "tmpl-app") },
  });
  server = started.server;
  SITE = started.sites.reconnect;
});

test.afterAll(async () => {
  await server?.stop();
});

test("subscriptions, live queries and rooms resume after the server restarts", async ({ newUser, pageProblems }) => {
  // While the server is down the browser logs the failed connection attempts.
  pageProblems.allow(/^WebSocket connection to 'ws:\/\/reconnect\.localhost:\d+\/__backyard\/api\/ws' failed/);
  pageProblems.allow(/net::ERR_CONNECTION_REFUSED/);

  const alice = await openPage(await newUser(), SITE);
  const bob = await openPage(await newUser(), SITE);
  await expect(alice.locator("#online")).toHaveText("● 2 online");
  await expect(bob.locator("#online")).toHaveText("● 2 online");

  // Bob also has a plain db subscription and watches the socket status.
  await bob.evaluate(() => {
    window.events = [];
    window.socketLog = [];
    backyard.db.collection("items").subscribe({ onCreate: (d) => window.events.push(`create:${d.text}`) });
    backyard.socket.on("close", () => window.socketLog.push("close"));
    backyard.socket.on("open", () => window.socketLog.push("open"));
  });
  const meBefore = await bob.evaluate(() => fetch("/__backyard/api/me").then((r) => r.json()));
  const before = `Before the crash ${rand()}`;
  await alice.locator("#add-input").fill(before);
  await alice.locator("#add-input").press("Enter");
  await expect(item(bob, before)).toBeVisible();
  await expect.poll(() => bob.evaluate(() => window.events)).toEqual([`create:${before}`]);

  // Crash: both pages notice.
  await server.kill();
  for (const page of [alice, bob]) await expect(page.locator("#offline")).toBeVisible();
  expect(await bob.evaluate(() => backyard.socket.connected)).toBe(false);

  // Back up. A change written right away (most likely before the pages have
  // reconnected) must still reach them: live queries re-query on reconnect.
  await server.start();
  const during = `Written during the reconnect ${rand()}`;
  const res = await httpRequest("POST", `${SITE}__backyard/api/db/items`, {
    headers: { "X-Backyard": "1" },
    json: { text: during, done: false, author: "a script" },
  });
  expect(res.status, res.text).toBe(201);

  for (const page of [alice, bob]) {
    await expect(page.locator("#offline")).toBeHidden({ timeout: 20_000 });
    await expect(item(page, during)).toBeVisible();
    await expect(item(page, before)).toBeVisible();
  }
  expect(await bob.evaluate(() => backyard.socket.connected)).toBe(true);
  expect(await bob.evaluate(() => window.socketLog.slice(0, 1).concat(window.socketLog.slice(-1)))).toEqual(["close", "open"]);

  // Rooms were re-joined with presence; the same guest identities (cookies) still work.
  await expect(alice.locator("#online")).toHaveText("● 2 online");
  await expect(bob.locator("#online")).toHaveText("● 2 online");
  expect(await bob.evaluate(() => fetch("/__backyard/api/me").then((r) => r.json()))).toMatchObject({ id: meBefore.id, name: meBefore.name });

  // New changes flow live again, through the watch and the plain subscription.
  const after = `After the restart ${rand()}`;
  await alice.locator("#add-input").fill(after);
  await alice.locator("#add-input").press("Enter");
  await expect(item(bob, after)).toBeVisible();
  await expect.poll(() => bob.evaluate(() => window.events)).toContain(`create:${after}`);
  await item(bob, after).locator('input[type="checkbox"]').check();
  await expect(item(alice, after)).toHaveClass(/done/);

  // Presence keeps working: Bob leaves.
  await bob.close();
  await expect(alice.locator("#online")).toHaveText("● 1 online");
});

test("after a graceful restart, presence shows who is still there", async ({ newUser, pageProblems }) => {
  pageProblems.allow(/^WebSocket connection to 'ws:\/\/reconnect\.localhost:\d+\/__backyard\/api\/ws' failed/);
  pageProblems.allow(/net::ERR_CONNECTION_REFUSED/);

  const alice = await openPage(await newUser(), SITE);
  const bob = await openPage(await newUser(), SITE);
  await expect(alice.locator("#online")).toHaveText("● 2 online");

  // A graceful stop (SIGTERM) closes the websockets; Bob leaves while the server is down.
  await server.stop();
  await expect(alice.locator("#offline")).toBeVisible();
  await bob.close();

  await server.start();
  await expect(alice.locator("#offline")).toBeHidden({ timeout: 20_000 });
  // Alice re-joined; Bob never came back, so he is not listed any more.
  await expect(alice.locator("#online")).toHaveText("● 1 online");
  expect(await alice.evaluate(() => backyard.socket.room("presence").members.length)).toBe(0);

  // Someone new arrives after the restart.
  const carol = await openPage(await newUser(), SITE);
  await expect(alice.locator("#online")).toHaveText("● 2 online");
  await expect(carol.locator("#online")).toHaveText("● 2 online");
});
