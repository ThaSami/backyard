// In-process tests of the engine's network helpers (node --test):
// scripts/lib/net.mjs (Access redirects, fetch error classes, DoH, probes,
// polling) and scripts/lib/cfapi.mjs (the REST client: envelope, pagination,
// retries, credentials, error messages) against local servers.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { after, before, describe, it } from "node:test";

import { CfError, apiBase, createApi, credentialHeaders, permissionFor } from "../../scripts/lib/cfapi.mjs";
import { classifyFetchError, dohAnswered, dohResolves, parseAccessRedirect, probe, proxyHint, randomProbeLabel, waitFor } from "../../scripts/lib/net.mjs";
import { CliError } from "../../scripts/lib/ui.mjs";

/** A local server whose handler a test sets; records every request. */
function testServer() {
  const seen = [];
  let handler = (req, res) => res.end();
  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      seen.push({ method: req.method, url: req.url, headers: req.headers, body });
      handler(req, res, body);
    });
  });
  return {
    seen,
    set: (h) => {
      handler = h;
      seen.length = 0;
    },
    start: () => new Promise((r) => server.listen(0, "127.0.0.1", () => r(`http://127.0.0.1:${server.address().port}`))),
    stop: () => new Promise((r) => server.close(() => r())),
  };
}

const json = (res, status, obj, headers = {}) => {
  res.writeHead(status, { "content-type": "application/json", ...headers });
  res.end(JSON.stringify(obj));
};
const envelope = (result, extra = {}) => ({ success: true, errors: [], messages: [], result, ...extra });
const failure = (code, message) => ({ success: false, errors: [{ code, message }], messages: [], result: null });
const fetchError = (code) => new TypeError("fetch failed", { cause: Object.assign(new Error(code), { code }) });

describe("lib/net.mjs", () => {
  const srv = testServer();
  let base;
  before(async () => {
    base = await srv.start();
  });
  after(() => srv.stop());

  it("parses Cloudflare Access login redirects", () => {
    const aud = "0123456789abcdef".repeat(4);
    assert.deepEqual(parseAccessRedirect(`https://acme.cloudflareaccess.com/cdn-cgi/access/login/backyard.acme.com?kid=${aud}&redirect_url=%2F&meta=x`), { team: "acme.cloudflareaccess.com", kid: aud });
    assert.deepEqual(parseAccessRedirect("https://Acme.CloudflareAccess.com/cdn-cgi/access/login/h?redirect_url=%2F"), { team: "acme.cloudflareaccess.com", kid: "" }, "no kid");
    // The kid of the redirect itself, not one inside the encoded redirect_url.
    assert.deepEqual(
      parseAccessRedirect(`https://acme.cloudflareaccess.com/cdn-cgi/access/login/h?redirect_url=${encodeURIComponent("/x?kid=evil&y=1")}&kid=${aud.toUpperCase()}`),
      { team: "acme.cloudflareaccess.com", kid: aud },
    );
    for (const other of [
      "https://login.example.com/cdn-cgi/access/login/h?kid=x",
      "https://evil.com/?.cloudflareaccess.com/cdn-cgi/access/login",
      "http://acme.cloudflareaccess.com/cdn-cgi/access/login/h?kid=x",
      "https://acme.cloudflareaccess.com/somewhere",
      "/cdn-cgi/access/login/h",
      "",
      "not a url",
    ]) {
      assert.equal(parseAccessRedirect(other), null, other);
    }
  });

  it("classifies fetch errors", () => {
    assert.equal(classifyFetchError(fetchError("ERR_TLS_CERT_ALTNAME_INVALID")), "certificate");
    assert.equal(classifyFetchError(fetchError("EPROTO")), "certificate");
    assert.equal(classifyFetchError(fetchError("UNABLE_TO_VERIFY_LEAF_SIGNATURE")), "certificate");
    assert.equal(classifyFetchError(fetchError("CERT_HAS_EXPIRED")), "certificate");
    assert.equal(classifyFetchError(fetchError("ERR_SSL_WRONG_VERSION_NUMBER")), "certificate");
    assert.equal(classifyFetchError(fetchError("ENOTFOUND")), "dns");
    assert.equal(classifyFetchError(fetchError("EAI_AGAIN")), "dns");
    assert.equal(classifyFetchError(fetchError("ECONNREFUSED")), "network");
    assert.equal(classifyFetchError(new Error("boom")), "network");
    // A nested cause (undici wraps the socket error).
    const nested = new TypeError("fetch failed", { cause: new Error("x", { cause: Object.assign(new Error("y"), { code: "ERR_TLS_CERT_ALTNAME_INVALID" }) }) });
    assert.equal(classifyFetchError(nested), "certificate");
  });

  it("parses DNS-over-HTTPS answers", async () => {
    assert.equal(dohAnswered({ Status: 0, Answer: [{ data: "100::" }] }), true);
    assert.equal(dohAnswered({ Status: 0, Answer: [] }), false);
    assert.equal(dohAnswered({ Status: 0 }), false);
    assert.equal(dohAnswered({ Status: 3 }), false);
    assert.equal(dohAnswered(null), false);
    const answers = { AAAA: { Status: 0 }, A: { Status: 0, Answer: [{ name: "x.", type: 1, data: "192.0.2.1" }] } };
    srv.set((req, res) => {
      const u = new URL(req.url, base);
      json(res, 200, answers[u.searchParams.get("type")] ?? { Status: 3 }, { "content-type": "application/dns-json" });
    });
    assert.equal(await dohResolves("byd-probe-1.example.com", { url: `${base}/dns-query` }), true, "AAAA empty, A answers");
    assert.deepEqual(
      srv.seen.map((x) => [new URL(x.url, base).searchParams.get("name"), new URL(x.url, base).searchParams.get("type"), x.headers.accept]),
      [
        ["byd-probe-1.example.com", "AAAA", "application/dns-json"],
        ["byd-probe-1.example.com", "A", "application/dns-json"],
      ],
    );
    answers.A = { Status: 3 };
    assert.equal(await dohResolves("nope.example.com", { url: `${base}/dns-query` }), false);
    srv.set((req, res) => json(res, 500, {}));
    assert.equal(await dohResolves("x.example.com", { url: `${base}/dns-query` }), false, "errors do not resolve");
  });

  it("probes without following redirects and reports failures by kind", async () => {
    srv.set((req, res) => {
      if (req.url === "/redirect") {
        res.writeHead(302, { location: "https://acme.cloudflareaccess.com/cdn-cgi/access/login/h?kid=k" });
        return res.end();
      }
      json(res, 200, { ok: true, ua: req.headers["user-agent"] });
    });
    const r = await probe(`${base}/redirect`);
    assert.equal(r.ok, true);
    assert.equal(r.status, 302);
    assert.match(r.location, /cloudflareaccess/);
    const j = await probe(`${base}/info`, { accept: "application/json" });
    assert.equal(j.status, 200);
    assert.match(j.json.ua, /^backyard-cloudflare\//);
    assert.equal(srv.seen.at(-1).headers.accept, "application/json");
    // A closed port: a network failure, not an exception.
    const closed = testServer();
    const dead = await closed.start();
    await closed.stop();
    const f = await probe(`${dead}/x`, { timeoutMs: 2000 });
    assert.equal(f.ok, false);
    assert.equal(f.kind, "network");
    assert.equal(f.code, "ECONNREFUSED");
  });

  it("polls until done or the time is up (0 = once)", async () => {
    let n = 0;
    const slept = [];
    const sleep = async (ms) => slept.push(ms);
    let r = await waitFor(async () => ({ done: false, n: ++n }), { timeoutSec: 0, sleep });
    assert.equal(r.n, 1);
    assert.equal(slept.length, 0);
    n = 0;
    r = await waitFor(async () => ({ done: ++n === 3, n }), { timeoutSec: 60, intervalSec: 0, sleep });
    assert.equal(r.n, 3);
    assert.equal(r.done, true);
    const pending = [];
    n = 0;
    await waitFor(async () => ({ done: ++n === 2 }), { timeoutSec: 60, intervalSec: 0, sleep, onPending: (x, s) => pending.push(s) });
    assert.equal(pending.length, 1);
  });

  it("makes fresh probe labels", () => {
    const a = randomProbeLabel();
    assert.match(a, /^byd-probe-[0-9a-f]{8}$/);
    assert.notEqual(a, randomProbeLabel());
  });

  it("says when a proxy is set that Node's fetch ignores", () => {
    const node = "22.22.0";
    assert.equal(proxyHint({}, { node, execArgv: [] }), "");
    assert.match(proxyHint({ HTTPS_PROXY: "http://proxy:3128" }, { node, execArgv: [] }), /^HTTPS_PROXY is set, but Node's fetch ignores it: set NODE_USE_ENV_PROXY=1/);
    assert.match(proxyHint({ http_proxy: "http://proxy:3128" }, { node, execArgv: [] }), /^http_proxy is set/);
    assert.equal(proxyHint({ HTTPS_PROXY: "http://proxy:3128", NODE_USE_ENV_PROXY: "1" }, { node, execArgv: [] }), "");
    assert.equal(proxyHint({ HTTPS_PROXY: "http://proxy:3128", NODE_OPTIONS: "--max-old-space-size=99 --use-env-proxy" }, { node: "24.1.0", execArgv: [] }), "");
    assert.equal(proxyHint({ HTTPS_PROXY: "http://proxy:3128" }, { node, execArgv: ["--use-env-proxy"] }), "");
    // Older Node 22 has no such switch: the hint names the version needed.
    assert.match(proxyHint({ HTTPS_PROXY: "http://proxy:3128", NODE_USE_ENV_PROXY: "1" }, { node: "22.20.0", execArgv: [] }), /Node 22\.20\.0's fetch ignores it: .*Node 22\.21 or later, with NODE_USE_ENV_PROXY=1/);
  });
});

describe("lib/cfapi.mjs", () => {
  const srv = testServer();
  let base;
  const TOKEN = "secret-token-0123456789abcdef";
  before(async () => {
    base = await srv.start();
  });
  after(() => srv.stop());
  const api = (o = {}) => createApi({ credential: { type: "api_token", token: TOKEN }, base, sleep: async () => {}, ...o });

  it("accepts plain http only for loopback API bases", () => {
    assert.equal(apiBase({}), "https://api.cloudflare.com/client/v4");
    assert.equal(apiBase({ CLOUDFLARE_API_BASE_URL: "http://127.0.0.1:9/client/v4/" }), "http://127.0.0.1:9/client/v4");
    assert.equal(apiBase({ CLOUDFLARE_API_BASE_URL: "http://localhost:9/client/v4" }), "http://localhost:9/client/v4");
    assert.equal(apiBase({ CLOUDFLARE_API_BASE_URL: "https://proxy.example.com/client/v4" }), "https://proxy.example.com/client/v4");
    for (const bad of ["http://api.example.com/client/v4", "ftp://x/y", "not a url"]) {
      assert.throws(() => apiBase({ CLOUDFLARE_API_BASE_URL: bad }), (e) => e instanceof CliError && e.code === 2, bad);
    }
  });

  it("sends the credential and unwraps the envelope", async () => {
    srv.set((req, res) => json(res, 200, envelope({ id: "x" })));
    assert.deepEqual(await api().get("/accounts/a/thing", { q: "1", empty: "" }), { id: "x" });
    const s = srv.seen[0];
    assert.equal(s.url, "/accounts/a/thing?q=1");
    assert.equal(s.headers.authorization, `Bearer ${TOKEN}`);
    assert.match(s.headers["user-agent"], /^backyard-cloudflare\//);
    assert.deepEqual(credentialHeaders({ type: "api_key", key: "k", email: "e@x" }), { "X-Auth-Key": "k", "X-Auth-Email": "e@x" });
    await api().post("/p", { a: 1 });
    assert.equal(srv.seen.at(-1).headers["content-type"], "application/json");
    assert.deepEqual(JSON.parse(srv.seen.at(-1).body), { a: 1 });
  });

  it("follows pagination", async () => {
    const items = Array.from({ length: 7 }, (_, i) => ({ i }));
    srv.set((req, res) => {
      const u = new URL(req.url, base);
      const page = Number(u.searchParams.get("page"));
      const per = Number(u.searchParams.get("per_page"));
      json(res, 200, envelope(items.slice((page - 1) * per, page * per), { result_info: { page, per_page: per, total_pages: Math.ceil(items.length / per), total_count: items.length } }));
    });
    assert.deepEqual(await api().list("/apps", {}, { perPage: 3 }), items);
    assert.equal(srv.seen.length, 3);
    // Without result_info: until a short page.
    srv.set((req, res) => {
      const page = Number(new URL(req.url, base).searchParams.get("page"));
      json(res, 200, envelope(page === 1 ? [1, 2] : [3]));
    });
    assert.deepEqual(await api().list("/x", {}, { perPage: 2 }), [1, 2, 3]);
  });

  it("retries GET on 429 (Retry-After) and 5xx, but never a POST blindly", async () => {
    const slept = [];
    const sleep = async (ms) => slept.push(ms);
    let n = 0;
    srv.set((req, res) => (++n === 1 ? json(res, 429, failure(971, "slow down"), { "retry-after": "7" }) : json(res, 200, envelope("ok"))));
    assert.equal(await api({ sleep }).get("/x"), "ok");
    assert.deepEqual(slept, [7000]);
    // 5xx: up to 3 retries, then the error; without Retry-After the waits back off (never 0 ms).
    slept.length = 0;
    srv.set((req, res) => json(res, 503, failure(10013, "unavailable")));
    await assert.rejects(api({ sleep }).get("/x"), (e) => e instanceof CfError && e.status === 503 && e.has(10013));
    assert.equal(srv.seen.length, 4);
    assert.deepEqual(slept, [500, 1000, 2000]);
    // POST + 500: one request, an ambiguous error.
    srv.set((req, res) => json(res, 500, failure(10013, "internal")));
    await assert.rejects(api().post("/x", {}), (e) => e instanceof CfError && e.ambiguous === true);
    assert.equal(srv.seen.length, 1);
    // POST + 429: nothing was done, retried.
    n = 0;
    srv.set((req, res) => (++n === 1 ? json(res, 429, failure(971, "slow down"), { "retry-after": "0" }) : json(res, 200, envelope({ id: 1 }))));
    assert.deepEqual(await api().post("/x", {}), { id: 1 });
    // postOrFind: after an ambiguous failure, the object is looked up.
    srv.set((req, res) => json(res, 502, failure(0, "bad gateway")));
    assert.deepEqual(await api().postOrFind("/x", {}, async () => ({ found: true })), { found: true });
    await assert.rejects(api().postOrFind("/x", {}, async () => null), CfError);
    // A definite error is not looked up.
    srv.set((req, res) => json(res, 400, failure(1, "bad")));
    let looked = false;
    await assert.rejects(api().postOrFind("/x", {}, async () => (looked = true)), (e) => e.status === 400 && !e.ambiguous);
    assert.equal(looked, false);
  });

  it("re-reads the credential once after a 401 or code 10000", async () => {
    let reads = 0;
    const getCredential = async () => ({ type: "oauth", token: `oauth-token-${++reads}-abcdefgh` });
    srv.set((req, res) => (req.headers.authorization === "Bearer oauth-token-2-abcdefgh" ? json(res, 200, envelope("fresh")) : json(res, 401, failure(10000, "Authentication error"))));
    const a = createApi({ getCredential, base, sleep: async () => {} });
    assert.equal(await a.get("/x"), "fresh");
    assert.equal(reads, 2);
    // refresh() makes the next request read it again.
    a.refresh();
    await a.get("/x").catch(() => {});
    assert.equal(reads, 4, "refresh, then once more after the 401");
    assert.equal(await a.credentialType(), "oauth");
  });

  it("names the missing permission and never puts a token in an error", async () => {
    srv.set((req, res) => json(res, 403, failure(10000, `Authentication error for ${TOKEN}`)));
    await assert.rejects(api({ label: "BACKYARD_CF_API_TOKEN" }).post("/zones/z/dns_records", {}), (e) => {
      assert.ok(e instanceof CfError);
      assert.equal(e.forbidden, true);
      assert.equal(e.permission, "Zone · DNS · Edit (that zone only)");
      assert.match(e.message, /POST \/zones\/z\/dns_records failed \(403: .*\[code: 10000\]\): the credential \(BACKYARD_CF_API_TOKEN\) lacks a permission; an API token needs Zone · DNS · Edit/);
      assert.ok(!e.message.includes(TOKEN), e.message);
      assert.ok(!e.stack.includes(TOKEN));
      return true;
    });
    // A network failure does not echo the token either.
    const closed = testServer();
    const dead = await closed.start();
    await closed.stop();
    await assert.rejects(createApi({ credential: { type: "api_token", token: TOKEN }, base: dead, sleep: async () => {} }).get("/x"), (e) => e.status === 0 && !e.message.includes(TOKEN));
    // Not an API response at all.
    srv.set((req, res) => {
      res.writeHead(502, { "content-type": "text/html" });
      res.end("<html>bad gateway</html>");
    });
    await assert.rejects(api({ retries: 0 }).get("/x"), /not a Cloudflare API response/);
  });

  it("names the proxy setting when the API is unreachable and a proxy is set", async () => {
    const saved = { HTTPS_PROXY: process.env.HTTPS_PROXY, NODE_USE_ENV_PROXY: process.env.NODE_USE_ENV_PROXY, NODE_OPTIONS: process.env.NODE_OPTIONS };
    process.env.HTTPS_PROXY = "http://proxy.example:3128";
    delete process.env.NODE_USE_ENV_PROXY;
    delete process.env.NODE_OPTIONS;
    try {
      const unreachable = async () => {
        throw new TypeError("fetch failed", { cause: Object.assign(new Error("getaddrinfo ENOTFOUND api.cloudflare.com"), { code: "ENOTFOUND" }) });
      };
      const a = createApi({ credential: { type: "api_token", token: TOKEN }, base: "https://api.cloudflare.com/client/v4", fetch: unreachable, sleep: async () => {} });
      await assert.rejects(a.get("/accounts/a/workers/subdomain"), (e) => {
        assert.equal(e.status, 0);
        assert.match(e.message, /GET \/accounts\/a\/workers\/subdomain: ENOTFOUND \(HTTPS_PROXY is set, but Node.*fetch ignores it.*NODE_USE_ENV_PROXY=1/);
        return true;
      });
      // A loopback base (tests, a local proxy of the API) never goes through a proxy: no hint.
      const closed = testServer();
      const dead = await closed.start();
      await closed.stop();
      await assert.rejects(createApi({ credential: { type: "api_token", token: TOKEN }, base: dead, sleep: async () => {} }).get("/x"), (e) => e.status === 0 && !/HTTPS_PROXY/.test(e.message));
    } finally {
      for (const [k, v] of Object.entries(saved)) {
        if (v === undefined) delete process.env[k];
        else process.env[k] = v;
      }
    }
  });

  it("takes an answer without a body to a DELETE as success, but never to a GET", async () => {
    for (const status of [200, 204]) {
      srv.set((req, res) => {
        res.writeHead(status);
        res.end();
      });
      assert.equal(await api().delete("/accounts/a/workers/domains/abc"), null, `DELETE ${status}`);
      assert.equal(srv.seen.length, 1, "not retried");
    }
    srv.set((req, res) => {
      res.writeHead(200);
      res.end();
    });
    await assert.rejects(api({ retries: 0 }).get("/x"), /GET \/x failed \(200, not a Cloudflare API response\)/);
    // An error answer to a DELETE is still an error.
    srv.set((req, res) => json(res, 404, failure(100114, "Domain not found")));
    await assert.rejects(api().delete("/accounts/a/workers/domains/abc"), (e) => e instanceof CfError && e.notFound);
  });

  it("maps every endpoint of the design to its permission", () => {
    const cases = {
      "/accounts/a/workers/subdomain": "Account · Workers Scripts · Edit",
      "/accounts/a/workers/subdomains/acme": "Account · Workers Scripts · Edit",
      "/accounts/a/workers/scripts/w/settings": "Account · Workers Scripts · Read",
      "/accounts/a/workers/scripts/w/subdomain": "Account · Workers Scripts · Read",
      "/zones?name=x": "Zone · Zone · Read",
      "/accounts/a/workers/scripts/w/domains/changeset": "Edit Cloudflare Workers",
      "/accounts/a/workers/scripts/w/domains/records": "Edit Cloudflare Workers",
      "/accounts/a/workers/domains/records/id": "Edit Cloudflare Workers",
      "/accounts/a/workers/domains/id": "Edit Cloudflare Workers",
      "/zones/z/workers/routes/id": "Zone · Workers Routes · Edit",
      "/zones/z/dns_records": "Zone · DNS · Edit",
      "/accounts/a/access/organizations": "Access: Organizations, Identity Providers, and Groups · Read",
      "/accounts/a/access/apps/id": "Access: Apps and Policies · Edit",
      "/accounts/a/access/policies": "Access: Apps and Policies · Edit",
    };
    for (const [path, perm] of Object.entries(cases)) assert.ok(permissionFor(path).includes(perm), `${path}: ${permissionFor(path)}`);
  });
});
