// SDK context resolution (SPEC §2.1) and configure().
import { test } from "node:test";
import assert from "node:assert/strict";
import { createEnv, plain, json } from "./helpers/harness.mjs";

const HUB = "https://backyard.test";

const cases = [
  {
    name: "host mode, site from preamble",
    href: "https://foo.backyard.test/app/index.html",
    preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: HUB },
    want: { name: "foo", url: "https://foo.backyard.test/", apiBase: "https://foo.backyard.test/__backyard", mode: "host" },
  },
  {
    name: "host mode on the hub",
    href: "https://backyard.test/",
    preamble: { v: 1, mode: "host", site: null, prefix: "/s/", hub: HUB },
    want: { name: null, url: "https://backyard.test/", apiBase: "https://backyard.test/__backyard", mode: "host" },
  },
  {
    name: "host mode keeps the port of the page origin",
    href: "http://foo.localhost:8080/x",
    preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: "http://localhost:8080" },
    want: { name: "foo", url: "http://foo.localhost:8080/", apiBase: "http://foo.localhost:8080/__backyard", mode: "host" },
  },
  {
    name: "host mode without site in preamble derives it from the hub host",
    href: "https://bar.backyard.test/",
    preamble: { v: 1, mode: "host", prefix: "/s/", hub: HUB },
    want: { name: "bar", url: "https://bar.backyard.test/", apiBase: "https://bar.backyard.test/__backyard", mode: "host" },
  },
  {
    name: "host mode ignores deeper subdomains",
    href: "https://a.b.backyard.test/",
    preamble: { v: 1, mode: "host", prefix: "/s/", hub: HUB },
    want: { name: null, url: "https://a.b.backyard.test/", apiBase: "https://a.b.backyard.test/__backyard", mode: "host" },
  },
  {
    name: "no preamble defaults to host mode on the page origin",
    href: "https://intranet.example/page",
    preamble: undefined,
    want: { name: null, url: "https://intranet.example/", apiBase: "https://intranet.example/__backyard", mode: "host" },
  },
  {
    name: "path mode, site from preamble wins over the path",
    href: "https://backyard.test/s/other/index.html",
    preamble: { v: 1, mode: "path", site: "foo", prefix: "/s/", hub: HUB },
    want: { name: "foo", url: "https://backyard.test/s/foo/", apiBase: "https://backyard.test/s/foo/__backyard", mode: "path" },
  },
  {
    name: "path mode, site from location.pathname",
    href: "https://backyard.test/s/lunch-vote/deep/page.html?x=1",
    preamble: { v: 1, mode: "path", site: null, prefix: "/s/", hub: HUB },
    want: {
      name: "lunch-vote",
      url: "https://backyard.test/s/lunch-vote/",
      apiBase: "https://backyard.test/s/lunch-vote/__backyard",
      mode: "path",
    },
  },
  {
    name: "path mode with a custom prefix",
    href: "https://quick.example/sites/demo/",
    preamble: { v: 1, mode: "path", site: null, prefix: "/sites/", hub: "https://quick.example" },
    want: { name: "demo", url: "https://quick.example/sites/demo/", apiBase: "https://quick.example/sites/demo/__backyard", mode: "path" },
  },
  {
    name: "path mode on the hub root",
    href: "https://backyard.test/",
    preamble: { v: 1, mode: "path", site: null, prefix: "/s/", hub: HUB },
    want: { name: null, url: "https://backyard.test/", apiBase: "https://backyard.test/__backyard", mode: "path" },
  },
  {
    name: "path mode requires the trailing slash after the site",
    href: "https://backyard.test/s/foo",
    preamble: { v: 1, mode: "path", site: null, prefix: "/s/", hub: HUB },
    want: { name: null, url: "https://backyard.test/", apiBase: "https://backyard.test/__backyard", mode: "path" },
  },
  {
    name: "workers.dev path mode (Cloudflare edition)",
    href: "https://backyard.acme.workers.dev/s/poll/",
    preamble: { v: 1, mode: "path", site: null, prefix: "/s/", hub: "https://backyard.acme.workers.dev" },
    want: {
      name: "poll",
      url: "https://backyard.acme.workers.dev/s/poll/",
      apiBase: "https://backyard.acme.workers.dev/s/poll/__backyard",
      mode: "path",
    },
  },
  {
    name: "BACKYARD_CONFIG.apiBase overrides everything (absolute)",
    href: "https://foo.backyard.test/",
    preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: HUB },
    config: { apiBase: "https://api.example/s/foo/__backyard/" },
    want: { name: "foo", url: "https://api.example/s/foo/", apiBase: "https://api.example/s/foo/__backyard", mode: "host" },
  },
  {
    name: "BACKYARD_CONFIG.apiBase relative to the page",
    href: "https://dev.example/proxy/page.html",
    preamble: undefined,
    config: { apiBase: "/proxy/__backyard" },
    want: { name: null, url: "https://dev.example/proxy/", apiBase: "https://dev.example/proxy/__backyard", mode: "host" },
  },
];

for (const c of cases) {
  test("context: " + c.name, () => {
    const env = createEnv({ href: c.href, preamble: c.preamble, config: c.config });
    assert.deepEqual(plain(env.backyard.site), c.want);
  });
}

test("requests go to <apiBase>/api and the websocket to ws(s)://<apiBase>/api/ws", async () => {
  const env = createEnv({
    href: "https://backyard.test/s/foo/",
    preamble: { v: 1, mode: "path", site: null, prefix: "/s/", hub: HUB },
  });
  env.on("GET", "/s/foo/__backyard/api/me", { id: "a" });
  await env.backyard.id.me();
  assert.equal(env.last().url, "https://backyard.test/s/foo/__backyard/api/me");
  env.backyard.socket.room("lobby");
  assert.equal(env.ws().url, "wss://backyard.test/s/foo/__backyard/api/ws");

  const local = createEnv({ href: "http://foo.localhost:8080/", preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: "http://localhost:8080" } });
  local.backyard.db.collection("x").subscribe(() => {});
  assert.equal(local.ws().url, "ws://foo.localhost:8080/__backyard/api/ws");
});

test("ready resolves immediately; version is exposed", async () => {
  const env = createEnv();
  assert.equal(await env.backyard.ready, undefined);
  assert.match(env.backyard.version, /^\d+\.\d+\.\d+/);
});

test("configure({apiBase}) re-targets requests, site info and the socket; null resets", async () => {
  const env = createEnv({ href: "https://foo.backyard.test/", preamble: { v: 1, mode: "host", site: "foo", prefix: "/s/", hub: HUB } });
  const by = env.backyard;
  const siteRef = by.site;
  by.socket.room("r");
  const first = env.ws();
  first.hello();
  by.configure({ apiBase: "https://other.example/__backyard/" });
  assert.equal(by.site, siteRef, "site object is updated in place");
  assert.equal(by.site.apiBase, "https://other.example/__backyard");
  assert.equal(by.site.url, "https://other.example/");
  assert.equal(first.closedWith.code, 1000);
  assert.equal(env.ws().url, "wss://other.example/__backyard/api/ws");
  env.on("GET", /.*/, (req) => json(200, { path: req.url }));
  const res = await by.ai.models();
  assert.equal(res.path, "https://other.example/__backyard/api/ai/models");
  by.configure({ apiBase: null });
  assert.equal(by.site.apiBase, "https://foo.backyard.test/__backyard");
});

test("configure ignores junk", () => {
  const env = createEnv();
  env.backyard.configure();
  env.backyard.configure("x");
  env.backyard.configure({ unknown: 1 });
  assert.equal(env.backyard.site.apiBase, "https://foo.backyard.test/__backyard");
});
