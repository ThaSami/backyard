// The SDK's realtime contract in the browser (backyard.d.ts): disconnect() /
// connect(), presence differences reported after a reconnect, and direct
// messages. Runs on a probe site of its own.
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";

import { deploySite } from "../../harness/backyard.mjs";
import { URLS, readState } from "../../harness/env.mjs";
import { expect, openPage, rand, test } from "../fixtures.mjs";

let SITE = "";
test.beforeAll(async ({}, testInfo) => {
  const name = `e2e-sdk-${rand()}`;
  const dir = join(testInfo.outputPath("site"), name);
  mkdirSync(dir, { recursive: true });
  writeFileSync(
    join(dir, "index.html"),
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>SDK probe</title><link rel="icon" href="data:,">
<script src="/__backyard/backyard.js"></script></head><body><h1>SDK probe</h1></body></html>`,
  );
  SITE = deploySite(readState().bin, { dir, site: name, server: URLS.main, home: testInfo.outputPath("cli") }).url;
});

/** Opens the probe site and joins room with state; events are recorded in window.log. */
async function member(newUser, room, state) {
  const page = await openPage(await newUser(), SITE);
  await page.evaluate(
    ([room, state]) => {
      window.log = [];
      window.r = backyard.socket.room(room, { state });
      for (const ev of ["join", "leave", "update"]) r.on(ev, (m) => log.push(`${ev}:${m.state.who}`));
      r.on("message", (data, from) => log.push(`msg:${data}:${from.user.name === backyard.socket.user.name ? "self" : "other"}`));
      backyard.socket.on("open", () => log.push("open"));
      backyard.socket.on("close", () => log.push("close"));
    },
    [room, state],
  );
  await page.waitForFunction(() => window.r.joined);
  return page;
}

const membersOf = (page) => page.evaluate(() => window.r.members.map((m) => m.state.who).sort());

test("disconnect() and connect(): the differences are reported as join/leave/update", async ({ newUser }) => {
  const room = `room-${rand()}`;
  const alice = await member(newUser, room, { who: "alice" });
  const bob = await member(newUser, room, { who: "bob" });
  const carol = await member(newUser, room, { who: "carol" });
  await expect.poll(() => membersOf(alice)).toEqual(["bob", "carol"]);

  await alice.evaluate(() => {
    window.log.length = 0;
    backyard.socket.disconnect();
  });
  await expect.poll(() => alice.evaluate(() => window.log)).toEqual(["close"]);
  expect(await alice.evaluate(() => [backyard.socket.connected, window.r.joined])).toEqual([false, false]);
  await expect.poll(() => membersOf(bob)).toEqual(["carol"]); // alice left for the others

  // While Alice is away: Carol leaves, Dave arrives, Bob changes his state.
  await carol.close();
  const dave = await member(newUser, room, { who: "dave" });
  await bob.evaluate(() => window.r.updateState({ who: "bob", mood: "busy" }));
  await expect.poll(() => membersOf(dave)).toEqual(["bob"]);

  // connect() restores the membership; the differences arrive as events.
  await alice.evaluate(() => backyard.socket.connect());
  await alice.waitForFunction(() => window.r.joined);
  await expect.poll(() => membersOf(alice)).toEqual(["bob", "dave"]);
  const log = await alice.evaluate(() => window.log);
  expect(log[0]).toBe("close");
  expect(log).toContain("open");
  expect(log.filter((l) => !["open", "close"].includes(l)).sort()).toEqual(["join:dave", "leave:carol", "update:bob"]);
  expect(await alice.evaluate(() => window.r.members.find((m) => m.state.who === "bob").state.mood)).toBe("busy");
  await expect.poll(() => membersOf(dave)).toEqual(["alice", "bob"]);
});

test("messages: broadcast, direct (to a cid) and echo", async ({ newUser }) => {
  const room = `room-${rand()}`;
  const alice = await member(newUser, room, { who: "alice" });
  const bob = await member(newUser, room, { who: "bob" });
  const carol = await member(newUser, room, { who: "carol" });
  await expect.poll(() => membersOf(alice)).toEqual(["bob", "carol"]);
  for (const p of [alice, bob, carol]) await p.evaluate(() => (window.log.length = 0));

  const aliceCid = await alice.evaluate(() => backyard.socket.cid);
  await bob.evaluate((cid) => {
    window.r.send("everyone");
    window.r.send("just alice", { to: cid });
    window.r.send("me too", { echo: true });
  }, aliceCid);
  await expect.poll(() => alice.evaluate(() => window.log)).toEqual(["msg:everyone:other", "msg:just alice:other", "msg:me too:other"]);
  await expect.poll(() => carol.evaluate(() => window.log)).toEqual(["msg:everyone:other", "msg:me too:other"]);
  await expect.poll(() => bob.evaluate(() => window.log)).toEqual(["msg:me too:self"]);
});
