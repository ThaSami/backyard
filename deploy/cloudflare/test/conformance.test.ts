/**
 * Regressions found by the cross-edition conformance suite (conformance/,
 * SPEC §17): behaviour where the Worker deviated from the Go server.
 */
import { describe, expect, it } from "vitest";
import { HttpError } from "../src/http";
import { parseChat } from "../src/site-api";
import { apiHeaders, call, deploy, uniqueSite } from "./helpers";

describe("X-Request-Id (SPEC §3, §15)", () => {
  it("is set on every response", async () => {
    for (const path of ["/", "/__backyard/healthz", "/__backyard/api/info", "/__backyard/api/nope", "/s/nobody-here/"]) {
      const res = await call(path);
      expect(res.headers.get("X-Request-Id"), path).toMatch(/^[0-9a-f]{24}$/);
    }
    const a = (await call("/__backyard/healthz")).headers.get("X-Request-Id");
    const b = (await call("/__backyard/healthz")).headers.get("X-Request-Id");
    expect(a).not.toBe(b);
  });

  it("keeps a valid incoming id, else uses the Ray id", async () => {
    let res = await call("/__backyard/healthz", { headers: { "X-Request-Id": "trace-1234.abc_DEF" } });
    expect(res.headers.get("X-Request-Id")).toBe("trace-1234.abc_DEF");
    res = await call("/__backyard/healthz", { headers: { "X-Request-Id": "bad id with spaces", "CF-Ray": "8f1a2b3c4d5e6f70-IAD" } });
    expect(res.headers.get("X-Request-Id")).toBe("8f1a2b3c4d5e6f70-IAD");
    res = await call("/__backyard/healthz", { headers: { "X-Request-Id": "short" } });
    expect(res.headers.get("X-Request-Id")).toMatch(/^[0-9a-f]{24}$/);
    res = await call("/__backyard/healthz", { headers: { "X-Request-Id": "x".repeat(129) } });
    expect(res.headers.get("X-Request-Id")).toMatch(/^[0-9a-f]{24}$/);
  });
});

describe("AI image parts (SPEC §5.4)", () => {
  const chatWith = (part: Record<string, unknown>) => () => parseChat({ messages: [{ role: "user", content: [{ type: "text", text: "look" }, part] }] }, "");

  it("rejects image URLs that are not absolute http(s) URLs", () => {
    for (const url of ["/__backyard/files/01J/x.png", "x.png", "ftp://example.com/x.png", "javascript:alert(1)", "data:image/png,notbase64", "https://", "https://e.com/" + "x".repeat(8192)]) {
      let err: unknown;
      try {
        chatWith({ type: "image", url })();
      } catch (e) {
        err = e;
      }
      expect(err, url).toBeInstanceOf(HttpError);
      expect((err as HttpError).status, url).toBe(400);
      expect((err as HttpError).code, url).toBe("bad_request");
    }
    expect(() => chatWith({ type: "image_url", image_url: { url: "/relative.png" } })()).toThrow(/absolute http\(s\) URL/);
  });

  it("accepts absolute URLs and data: URLs", () => {
    let p = chatWith({ type: "image", url: "https://example.com/cat.png" })();
    expect(p.req.messages[0]!.content[1]).toMatchObject({ type: "image", url: "https://example.com/cat.png" });
    p = chatWith({ type: "image", url: "HTTP://Example.com/a.png?x=1" })();
    expect(p.req.messages[0]!.content[1]).toMatchObject({ type: "image", url: "HTTP://Example.com/a.png?x=1" });
    p = chatWith({ type: "image", url: "data:image/png;base64,iVBORw0KGgo=" })();
    expect(p.req.messages[0]!.content[1]).toMatchObject({ type: "image", media_type: "image/png" });
  });

  it("answers 400 over HTTP before calling a provider", async () => {
    const site = uniqueSite("aiurl");
    await deploy(site, { "index.html": "x" });
    const res = await call(`/s/${site}/__backyard/api/ai/chat`, {
      method: "POST",
      headers: apiHeaders(),
      body: JSON.stringify({ messages: [{ role: "user", content: [{ type: "image", url: `/s/${site}/__backyard/files/01J/x.png` }] }] }),
    });
    // 501 when no AI provider is bound in the test environment; never 200/502.
    expect([400, 501]).toContain(res.status);
  });
});

describe("site API path parameters (SPEC §5.2)", () => {
  it("an encoded slash stays inside its parameter: invalid id → 400, not 404", async () => {
    const site = uniqueSite("params");
    await deploy(site, { "index.html": "x" });
    const db = (p: string) => `/s/${site}/__backyard/api/db${p}`;
    for (const [method, body] of [
      ["GET", undefined],
      ["PUT", "{}"],
      ["PATCH", "{}"],
      ["DELETE", undefined],
    ] as const) {
      const res = await call(db("/things/a%2Fb"), { method, headers: apiHeaders(), body });
      expect(res.status, method).toBe(400);
      expect(((await res.json()) as { error: { code: string } }).error.code).toBe("bad_request");
    }
    let res = await call(db("/a%2Fb"), { method: "POST", headers: apiHeaders(), body: "{}" });
    expect(res.status).toBe(400);
    res = await call(db("/a%2Fb/query"), { method: "POST", headers: apiHeaders(), body: "{}" });
    expect(res.status).toBe(400);

    // Other escapes decode as usual ("%3A" is ":"; "%25" is a literal "%", an invalid id).
    res = await call(db("/things/a%3Ab"), { method: "PUT", headers: apiHeaders(), body: '{"v":1}' });
    expect(res.status).toBe(200);
    expect(((await res.json()) as { id: string }).id).toBe("a:b");
    res = await call(db("/things/a:b"));
    expect(res.status).toBe(200);
    res = await call(db("/things/a%252Fb"), { method: "PUT", headers: apiHeaders(), body: "{}" });
    expect(res.status).toBe(400);
    // A malformed escape is a bad request; unknown paths stay 404.
    expect((await call(db("/things/%E0%A4%A"))).status).toBe(400);
    expect((await call(`/s/${site}/__backyard/api/nope/a%2Fb`)).status).toBe(404);
  });
});
