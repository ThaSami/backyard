/** Hub host: SDK with preamble (SPEC §2.1), hub UI, docs, llms.txt, health. */
import { describe, expect, it } from "vitest";
import { HUB_FILES } from "../src/generated/hub";
import { SDK_DTS, SDK_JS, SDK_MJS } from "../src/generated/sdk";
import { REFERENCES, SKILL_MD } from "../src/generated/skills";
import { HUB_URL_PLACEHOLDER, localize } from "../src/llms";
import { issueAPIToken } from "../src/jwt";
import { apiHeaders, call, deploy, ORIGIN, SECRET, tokenHeaders, uniqueSite } from "./helpers";

describe("hub", () => {
  it("serves the SDK with the per-request preamble, byte-identical otherwise", async () => {
    let res = await call("/__backyard/backyard.js");
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("text/javascript; charset=utf-8");
    expect(res.headers.get("Cache-Control")).toBe("no-cache");
    const pre = 'globalThis.__BACKYARD__ = {"v":1,"mode":"path","site":null,"prefix":"/s/","hub":"https://backyard.test"};\n';
    expect(await res.text()).toBe(pre + SDK_JS);
    const etag = res.headers.get("ETag")!;
    expect((await call("/__backyard/backyard.js", { headers: { "If-None-Match": etag } })).status).toBe(304);

    res = await call("/s/demo/__backyard/backyard.mjs");
    expect(await res.text()).toBe(pre.replace('"site":null', '"site":"demo"') + SDK_MJS);
    res = await call("/s/demo/__backyard/backyard.d.ts");
    expect(res.headers.get("Content-Type")).toBe("text/plain; charset=utf-8");
    expect(await res.text()).toBe(SDK_DTS);
    expect((await call("/__backyard/backyard.js", { method: "POST" })).status).toBe(405);

    // PUBLIC_URL and BASE_DOMAIN shape the hub URL.
    res = await call("/__backyard/backyard.js", {}, { PUBLIC_URL: "https://hub.example.com/" });
    expect((await res.text()).startsWith('globalThis.__BACKYARD__ = {"v":1,"mode":"path","site":null,"prefix":"/s/","hub":"https://hub.example.com"};')).toBe(true);
  });

  it("serves the hub UI and docs", async () => {
    let res = await call("/");
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("text/html; charset=utf-8");
    expect(await res.text()).toBe(HUB_FILES["index.html"]!.text);
    for (const name of Object.keys(HUB_FILES)) {
      res = await call("/" + name);
      expect(res.status, name).toBe(200);
    }
    res = await call("/docs");
    expect(await res.text()).toBe(HUB_FILES["docs.html"]!.text);
    // Documents are localized for this hub, like /llms-full.txt (and the Go hub).
    res = await call("/docs/skill.md");
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("text/markdown; charset=utf-8");
    const skill = await res.text();
    expect(skill).toBe(localize(SKILL_MD, ORIGIN));
    expect(skill).not.toContain(HUB_URL_PLACEHOLDER + "/");
    if (SKILL_MD.includes(HUB_URL_PLACEHOLDER)) expect(skill).toContain(ORIGIN);
    for (const name of Object.keys(REFERENCES)) {
      res = await call(`/docs/${name}.md`);
      expect(res.status, name).toBe(200);
      const body = await res.text();
      expect(body).toBe(localize(REFERENCES[name]!, ORIGIN));
      expect(body).not.toContain(HUB_URL_PLACEHOLDER + "/");
      // SKILL.md links to "references/<name>.md", relative to /docs/skill.md.
      res = await call(`/docs/references/${name}.md`);
      expect(res.status, `references/${name}`).toBe(200);
      expect(res.headers.get("Content-Type")).toBe("text/markdown; charset=utf-8");
      expect(await res.text()).toBe(body);
    }
    expect((await call("/docs/references/skill.md")).status).toBe(404);
    expect((await call("/docs/references/nope.md")).status).toBe(404);
    expect((await call("/docs/references/references/api.md")).status).toBe(404);
    expect((await call("/docs/constructor.md")).status).toBe(404);
    expect((await call("/docs/../etc.md")).status).toBe(404);
    expect((await call("/docs/nope.md")).status).toBe(404);
    // %2F decodes to "/" like Go's r.URL.Path, so it names the same document.
    res = await call("/docs/references%2Fapi.md");
    expect(res.status).toBe(200);
    expect(await res.text()).toBe(localize(REFERENCES.api!, ORIGIN));
    // Like the Go hub: "no such document: <rel>" truncated to 80 bytes + "…".
    const long = "x".repeat(100);
    res = await call(`/docs/${long}.md`, { headers: { Accept: "application/json" } });
    expect(res.status).toBe(404);
    expect(((await res.json()) as { error: { message: string } }).error.message).toBe(`no such document: ${"x".repeat(80)}…`);
    res = await call("/docs/references/nope.md", { headers: { Accept: "application/json" } });
    expect(((await res.json()) as { error: { message: string } }).error.message).toBe("no such document: references/nope");
    expect((await call("/nope.html")).status).toBe(404);
    expect((await call("/__backyard/nothing")).status).toBe(404);
    expect((await call("/__backyard/api/nothing")).status).toBe(404);
  });

  it("serves llms.txt and llms-full.txt for this deployment", async () => {
    let res = await call("/llms.txt");
    expect(res.status).toBe(200);
    const txt = await res.text();
    expect(txt.startsWith("# Backyard\n")).toBe(true);
    expect(txt).toContain("Hub: https://backyard.test/");
    expect(txt).toContain("- [Overview and rules](https://backyard.test/docs/skill.md)");
    expect(txt).toContain("(https://backyard.test/llms-full.txt)");
    res = await call("/llms-full.txt");
    const full = await res.text();
    expect(full).toContain("<!-- Backyard documentation for agents");
    expect(full).not.toContain("https://backyard.example.com/");
    expect(full).not.toMatch(/^---\nname:/);
  });

  it("localizes docs for PUBLIC_URL as the Go server would (lowercased, port kept)", async () => {
    const env = { PUBLIC_URL: "HTTPS://Hub.Example.COM:8443/" };
    const hub = "https://hub.example.com:8443";
    for (const path of ["/docs/skill.md", "/docs/references/deploy.md"]) {
      const res = await call(path, {}, env);
      expect(res.status, path).toBe(200);
      const md = await res.text();
      const src = path.endsWith("skill.md") ? SKILL_MD : REFERENCES.deploy!;
      expect(md).toBe(localize(src, hub));
      if (src.includes(HUB_URL_PLACEHOLDER + "/")) expect(md).toContain(hub + "/");
      // Example site hosts become subdomains of the hub host, with its port.
      if (src.includes("://demo.backyard.example.com")) expect(md).toContain("://demo.hub.example.com:8443");
      expect(md).not.toContain("Hub.Example.COM");
    }
    const txt = await (await call("/llms.txt", {}, env)).text();
    expect(txt).toContain(`- [Overview and rules](${hub}/docs/skill.md)`);
    expect(txt).toContain("`https://<site>.hub.example.com:8443/` (host mode)");
  });

  it("answers health checks on every host without auth", async () => {
    expect(await (await call("/__backyard/healthz")).text()).toBe("ok\n");
    expect((await call("/s/demo/__backyard/healthz")).status).toBe(200);
    const ready = await call("/__backyard/readyz");
    expect(ready.status).toBe(200);
    const notReady = await call("/__backyard/readyz", {}, { BACKYARD_SECRET: "" });
    expect(notReady.status).toBe(503);
    // Without a secret the platform refuses to run (but stays healthy).
    const res = await call("/__backyard/api/me", {}, { BACKYARD_SECRET: "" });
    expect(res.status).toBe(503);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("BACKYARD_SECRET");
  });

  it("refuses every request but the health checks when AUTH_MODE is not none or access (fail closed)", async () => {
    const site = uniqueSite("failclosed");
    await deploy(site, { "index.html": "secret page" });
    for (const mode of ["oidc", "okta", "cf-access", "cloudflare-access", "acess", "true"]) {
      // The Claude connector's paths fail closed too, also while it is enabled.
      const env = { AUTH_MODE: mode, ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: "aud", MCP_ENABLED: "true" };
      for (const [path, init] of [
        [`/s/${site}/`, {}],
        ["/", {}],
        [`/s/${site}/__backyard/api/me`, {}],
        [`/s/${site}/__backyard/api/db/posts`, { method: "POST", headers: apiHeaders(), body: '{"x":1}' }],
        ["/__backyard/api/deploy/sites", { headers: tokenHeaders() }],
        ["/__backyard/api/info", {}],
        ["/__backyard/auth/cli?mode=display", {}],
        ["/__backyard/mcp", { method: "POST", headers: { "Content-Type": "application/json", Authorization: "Bearer byat_x.y" }, body: '{"jsonrpc":"2.0","id":1,"method":"ping"}' }],
        ["/__backyard/oauth/register", { method: "POST", headers: { "Content-Type": "application/json" }, body: '{"redirect_uris":["http://127.0.0.1:9/cb"]}' }],
        ["/__backyard/oauth/token", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "grant_type=refresh_token" }],
        ["/__backyard/oauth/revoke", { method: "POST", headers: { "Content-Type": "application/x-www-form-urlencoded" }, body: "token=x" }],
        ["/.well-known/oauth-authorization-server", {}],
        ["/.well-known/oauth-protected-resource/__backyard/mcp", {}],
        ["/__backyard/auth/authorize?client_id=x", {}],
        ["/__backyard/api/mcp/grants", {}],
      ] as const) {
        const res = await call(path, init as RequestInit, env);
        expect(res.status, `${mode} ${path}`).toBe(503);
        expect(await res.text(), `${mode} ${path}`).not.toContain("secret page");
      }
      expect((await call("/__backyard/healthz", {}, env)).status, mode).toBe(200);
      const ready = await call("/__backyard/readyz", {}, env);
      expect(ready.status, mode).toBe(503);
      expect(await ready.text()).toContain("AUTH_MODE");
    }
    // Case and blanks are forgiven; "none" and "" stay the documented default.
    expect((await call(`/s/${site}/`, {}, { AUTH_MODE: " None " })).status).toBe(200);
    expect((await call(`/s/${site}/`, {}, { AUTH_MODE: "" })).status).toBe(200);
    expect((await call(`/s/${site}/`, {}, { AUTH_MODE: " Access ", ACCESS_TEAM_DOMAIN: "acme", ACCESS_AUD: "aud" })).status).toBe(401);
  });

  it("serves no hub file for names of Object.prototype", async () => {
    for (const name of ["constructor", "toString", "__proto__", "hasOwnProperty", "valueOf"]) {
      expect((await call(`/${name}`)).status, name).toBe(404);
    }
  });

  it("canonicalizes paths", async () => {
    let res = await call("/s/demo//x");
    expect(res.status).toBe(301);
    expect(res.headers.get("Location")).toBe("/s/demo/x");
    res = await call("/s/demo/a%2F..%2Fb");
    expect(res.status).toBe(301);
    expect(res.headers.get("Location")).toBe("/s/demo/b");
    res = await call("/s//", { method: "GET" });
    expect(res.status).toBe(301);
    res = await call("/s/demo//x", { method: "POST" });
    expect(res.status).toBe(400);
    res = await call("/s/%zz/");
    expect(res.status).toBe(400);
    res = await call("/s/Not_A_Site/");
    expect(res.status).toBe(404);
    expect(await res.text()).toContain("Site not found");
  });
});

describe("POST /__backyard/api/tokens", () => {
  const user = { id: "carol@acme.com", email: "carol@acme.com", name: "Carol", anonymous: false, admin: false };
  const mint = (bearer: string, body: unknown = {}) =>
    call("/__backyard/api/tokens", { method: "POST", headers: { Authorization: `Bearer ${bearer}`, "X-Backyard": "1", "Content-Type": "application/json" }, body: JSON.stringify(body) });
  const claimsOf = (tok: string) => JSON.parse(atob(tok.split(".")[1]!.replace(/-/g, "+").replace(/_/g, "/"))) as { exp: number; iat: number; sub: string };

  it("never mints a token that outlives the API token it was asked with (a token cannot renew itself)", async () => {
    const short = await issueAPIToken(SECRET, user, 3600);
    let res = await mint(short.token);
    expect(res.status).toBe(200);
    const first = (await res.json()) as { token: string; expires_at: string };
    const c1 = claimsOf(first.token);
    expect(c1.sub).toBe("carol@acme.com");
    expect(c1.exp).toBeLessThanOrEqual(short.expiresAt);
    expect(c1.exp).toBeGreaterThan(short.expiresAt - 10);
    expect(first.expires_at).toBe(new Date(c1.exp * 1000).toISOString().replace(".000Z", "Z"));
    // Asking for longer, or chaining minted tokens, extends nothing.
    for (const tok of [short.token, first.token]) {
      res = await mint(tok, { ttl_hours: 24 });
      expect(res.status).toBe(200);
      expect(claimsOf(((await res.json()) as { token: string }).token).exp).toBeLessThanOrEqual(short.expiresAt);
    }
    // A shorter lifetime is still honoured.
    res = await mint(short.token, { ttl_hours: 0.1 });
    const c3 = claimsOf(((await res.json()) as { token: string }).token);
    expect(c3.exp - c3.iat).toBe(360);
    // A token that is (about to be) expired, within the verification leeway, cannot mint at all.
    for (const ending of [await issueAPIToken(SECRET, user, 1), await issueAPIToken(SECRET, user, 60, Math.floor(Date.now() / 1000) - 70)]) {
      res = await mint(ending.token);
      expect(res.status).toBe(401);
      expect(((await res.json()) as { error: { code: string } }).error.code).toBe("unauthenticated");
    }
  });
});
