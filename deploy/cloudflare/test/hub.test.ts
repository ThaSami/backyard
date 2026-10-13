/** Hub host: SDK with preamble (SPEC §2.1), hub UI, docs, llms.txt, health. */
import { describe, expect, it } from "vitest";
import { HUB_FILES } from "../src/generated/hub";
import { SDK_DTS, SDK_JS, SDK_MJS } from "../src/generated/sdk";
import { REFERENCES } from "../src/generated/skills";
import { call } from "./helpers";

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
    res = await call("/docs/skill.md");
    expect(res.status).toBe(200);
    expect(res.headers.get("Content-Type")).toBe("text/markdown; charset=utf-8");
    for (const name of Object.keys(REFERENCES)) {
      res = await call(`/docs/${name}.md`);
      expect(res.status, name).toBe(200);
      expect(await res.text()).toBe(REFERENCES[name]);
    }
    expect((await call("/docs/../etc.md")).status).toBe(404);
    expect((await call("/docs/nope.md")).status).toBe(404);
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
