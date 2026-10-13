/** AI proxy (SPEC §5.4) against a mocked Workers AI binding and mocked HTTP providers. */
import { afterEach, describe, expect, it, vi } from "vitest";
import { AnthropicProvider, acceptsSampling, effortFor, supportsDefaultFallbacks } from "../src/ai/anthropic";
import { stripCodeFences } from "../src/ai/types";
import { parseChat } from "../src/site-api";
import { apiHeaders, call, uniqueSite } from "./helpers";

const PNG = new Uint8Array([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 13, 73, 72, 68, 82, 1, 2, 3, 4]);

function sseStream(events: unknown[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  return new ReadableStream({
    start(c) {
      for (const e of events) c.enqueue(enc.encode(`data: ${typeof e === "string" ? e : JSON.stringify(e)}\n\n`));
      c.close();
    },
  });
}

interface Call {
  model: string;
  input: Record<string, unknown>;
}

function mockAI() {
  const calls: Call[] = [];
  const ai = {
    async run(model: string, input: Record<string, unknown>) {
      calls.push({ model, input });
      if (model.includes("flux")) return { image: btoa(String.fromCharCode(...PNG)) };
      if (model.includes("bge")) return { shape: [(input.text as string[]).length, 3], data: (input.text as string[]).map((_, i) => [i, 0.5, -1]) };
      if (input.stream) {
        if (model.includes("llama")) {
          return sseStream([{ response: "Hel" }, { response: "lo" }, { response: "", usage: { prompt_tokens: 7, completion_tokens: 2 } }, "[DONE]"]);
        }
        return sseStream([
          { choices: [{ delta: { content: "Hi" } }] },
          { choices: [{ delta: { content: " there" }, finish_reason: "length" }] },
          { choices: [], usage: { prompt_tokens: 3, completion_tokens: 4 } },
          "[DONE]",
        ]);
      }
      if (model.includes("llama")) return { response: '```json\n{"ok":true}\n```', usage: { prompt_tokens: 5, completion_tokens: 6 } };
      return {
        choices: [{ message: { role: "assistant", content: "Paris" }, finish_reason: "stop" }],
        usage: { prompt_tokens: 12, completion_tokens: 40 },
      };
    },
  };
  return { ai: ai as unknown as Ai, calls };
}

async function readSSE(res: Response): Promise<Record<string, unknown>[]> {
  const text = await res.text();
  return text
    .split("\n\n")
    .filter((b) => b.startsWith("data: "))
    .map((b) => JSON.parse(b.slice(6)) as Record<string, unknown>);
}

describe("AI API", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("lists the default aliases", async () => {
    const { ai } = mockAI();
    const res = await call(`/s/${uniqueSite("ai")}/__backyard/api/ai/models`, {}, { AI: ai });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { default: string; models: { name: string; kind: string; provider: string; model?: string }[] };
    expect(body.default).toBe("smart");
    expect(body.models.map((m) => [m.name, m.kind, m.provider])).toEqual([
      ["embed", "embedding", "workers-ai"],
      ["fast", "chat", "workers-ai"],
      ["image", "image", "workers-ai"],
      ["smart", "chat", "workers-ai"],
    ]);
    expect(body.models[0]!.model).toBeUndefined();
    const exposed = (await (await call(`/s/x1/__backyard/api/ai/models`, {}, { AI: ai, AI_EXPOSE_MODEL_IDS: "true" })).json()) as { models: { model?: string }[] };
    expect(exposed.models[0]!.model).toBe("@cf/baai/bge-base-en-v1.5");

    // Only aliases whose provider is usable here are listed.
    const models = JSON.stringify({
      claude: { provider: "anthropic", model: "claude-opus-5-5", kind: "chat" },
      gpt: { provider: "openai", model: "gpt-x", kind: "chat" },
      local: { provider: "workers-ai", model: "@cf/meta/llama-3.1-8b-instruct-fp8", kind: "chat" },
    });
    const names = async (env: Parameters<typeof call>[2]) =>
      ((await (await call(`/s/x1/__backyard/api/ai/models`, {}, { AI_MODELS: models, ...env })).json()) as { default: string; models: { name: string }[] });
    expect(await names({ AI: undefined, ANTHROPIC_API_KEY: "k" })).toEqual({ default: "claude", models: [{ name: "claude", kind: "chat", provider: "anthropic" }] });
    expect((await names({ AI: ai, OPENAI_API_KEY: "k" })).models.map((m) => m.name)).toEqual(["gpt", "local"]);
    // Without any usable provider (no AI binding, no keys) AI is off.
    expect((await call(`/s/x1/__backyard/api/ai/models`, {}, { AI: undefined })).status).toBe(501);
  });

  it("chats (OpenAI-style and legacy result shapes)", async () => {
    const { ai, calls } = mockAI();
    const site = uniqueSite("chat");
    let res = await call(
      `/s/${site}/__backyard/api/ai/chat`,
      {
        method: "POST",
        headers: apiHeaders(),
        body: JSON.stringify({
          messages: [
            { role: "system", content: "Be brief." },
            { role: "user", content: "Capital of France?" },
          ],
          system: "You are a geographer.",
          max_tokens: 100000,
          temperature: 0.2,
        }),
      },
      { AI: ai },
    );
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ content: "Paris", model: "smart", stop_reason: "end_turn", usage: { input_tokens: 12, output_tokens: 40 } });
    expect(calls[0]!.model).toBe("@cf/google/gemma-4-26b-a4b-it");
    expect(calls[0]!.input).toMatchObject({
      messages: [
        { role: "system", content: "You are a geographer.\n\nBe brief." },
        { role: "user", content: "Capital of France?" },
      ],
      max_tokens: 4096,
      temperature: 0.2,
    });

    res = await call(
      `/s/${site}/__backyard/api/ai/chat`,
      { method: "POST", headers: apiHeaders(), body: JSON.stringify({ messages: [{ role: "user", content: "json please" }], model: "fast", json: true }) },
      { AI: ai },
    );
    const fast = (await res.json()) as { content: string; model: string };
    expect(fast).toMatchObject({ content: '{"ok":true}', model: "fast" });
    expect(String((calls[1]!.input.messages as { content: string }[])[0]!.content)).toContain("single valid JSON object");
  });

  it("streams server-sent events in Backyard's format", async () => {
    const { ai } = mockAI();
    const site = uniqueSite("stream");
    let res = await call(
      `/s/${site}/__backyard/api/ai/chat`,
      { method: "POST", headers: apiHeaders(), body: JSON.stringify({ messages: [{ role: "user", content: "hi" }], stream: true }) },
      { AI: ai },
    );
    expect(res.headers.get("Content-Type")).toBe("text/event-stream");
    expect(await readSSE(res)).toEqual([
      { type: "text", text: "Hi" },
      { type: "text", text: " there" },
      { type: "done", model: "smart", stop_reason: "max_tokens", usage: { input_tokens: 3, output_tokens: 4 } },
    ]);
    res = await call(
      `/s/${site}/__backyard/api/ai/chat`,
      { method: "POST", headers: apiHeaders(), body: JSON.stringify({ messages: [{ role: "user", content: "hi" }], stream: true, model: "fast" }) },
      { AI: ai },
    );
    expect(await readSSE(res)).toEqual([
      { type: "text", text: "Hel" },
      { type: "text", text: "lo" },
      { type: "done", model: "fast", stop_reason: "end_turn", usage: { input_tokens: 7, output_tokens: 2 } },
    ]);
    // Upstream failures become an error event.
    const failing = { run: async () => { throw new Error("model exploded"); } } as unknown as Ai;
    res = await call(
      `/s/${site}/__backyard/api/ai/chat`,
      { method: "POST", headers: apiHeaders(), body: JSON.stringify({ messages: [{ role: "user", content: "hi" }], stream: true }) },
      { AI: failing },
    );
    const events = await readSSE(res);
    expect(events).toHaveLength(1);
    expect(events[0]).toMatchObject({ type: "error", error: { code: "upstream_error" } });
  });

  it("generates images stored as site files", async () => {
    const { ai, calls } = mockAI();
    const site = uniqueSite("img");
    const res = await call(`/s/${site}/__backyard/api/ai/image`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ prompt: "a fox" }) }, { AI: ai });
    expect(res.status).toBe(200);
    const body = (await res.json()) as { model: string; images: { id: string; url: string; content_type: string }[] };
    expect(body.model).toBe("image");
    expect(body.images).toHaveLength(1);
    expect(body.images[0]!.content_type).toBe("image/png");
    expect(body.images[0]!.url).toMatch(new RegExp(`^/s/${site}/__backyard/files/[0-9A-Z]{26}/image-1\\.png$`));
    expect(calls[0]).toEqual({ model: "@cf/black-forest-labs/flux-1-schnell", input: { prompt: "a fox", steps: 4 } });
    const file = await call(body.images[0]!.url);
    expect(file.status).toBe(200);
    expect(new Uint8Array(await file.arrayBuffer())).toEqual(PNG);
    const list = (await (await call(`/s/${site}/__backyard/api/files`)).json()) as { files: { name: string }[] };
    expect(list.files.map((f) => f.name)).toEqual(["image-1.png"]);
    expect((await call(`/s/${site}/__backyard/api/ai/image`, { method: "POST", headers: apiHeaders(), body: "{}" }, { AI: ai })).status).toBe(400);
  });

  it("computes embeddings mirroring the input shape", async () => {
    const { ai } = mockAI();
    const site = uniqueSite("emb");
    const res = await call(`/s/${site}/__backyard/api/ai/embed`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ input: ["a", "b"] }) }, { AI: ai });
    expect(await res.json()).toMatchObject({ model: "embed", embeddings: [[0, 0.5, -1], [1, 0.5, -1]] });
    const bad = await call(`/s/${site}/__backyard/api/ai/embed`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ input: 5 }) }, { AI: ai });
    expect(bad.status).toBe(400);
  });

  it("validates requests and models", async () => {
    const { ai } = mockAI();
    const site = uniqueSite("val");
    const chat = (body: unknown, env = {}) =>
      call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers: apiHeaders(), body: JSON.stringify(body) }, { AI: ai, ...env });
    expect((await chat({ messages: [] })).status).toBe(400);
    expect((await chat({ messages: [{ role: "tool", content: "x" }] })).status).toBe(400);
    expect((await chat({ messages: [{ role: "system", content: "only system" }] })).status).toBe(400);
    expect((await chat({ messages: [{ role: "user", content: "x" }], model: "nope" })).status).toBe(404);
    expect((await chat({ messages: [{ role: "user", content: "x" }], model: "image" })).status).toBe(400);
    expect((await chat({ messages: [{ role: "user", content: "x" }], model: "@cf/meta/llama-x" })).status).toBe(404);
    expect((await chat({ messages: [{ role: "user", content: "x" }], model: "@cf/meta/llama-x" }, { AI_ALLOW_RAW_MODELS: "true" })).status).toBe(200);
    expect((await chat({ messages: [{ role: "user", content: [{ type: "image", data: "!!!" }] }] })).status).toBe(400);
    expect((await chat({ messages: [{ role: "user", content: "x" }], max_tokens: -1 })).status).toBe(400);
    const noAI = await call(`/s/${site}/__backyard/api/ai/models`, {}, { AI_MODELS: "{}" });
    expect(noAI.status).toBe(501);
    const ware = await call(`/s/${site}/__backyard/api/warehouse/query`, { method: "POST", headers: apiHeaders(), body: '{"sql":"select 1"}' });
    expect(ware.status).toBe(501);
    expect(((await ware.json()) as { error: { code: string } }).error.code).toBe("unsupported");
  });

  it("enforces the daily AI caps per user and platform-wide", async () => {
    const { ai } = mockAI();
    const site = uniqueSite("cap");
    const env = { AI: ai, LIMITS: JSON.stringify({ ai_per_day: 2, ai_platform_per_day: 100000 }) };
    const headers = { ...apiHeaders(), "CF-Connecting-IP": `203.0.113.${Math.floor(Math.random() * 250)}` };
    const body = JSON.stringify({ messages: [{ role: "user", content: "x" }] });
    // Anonymous users are keyed by IP; send no cookie so every call is a fresh guest of the same IP.
    expect((await call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers, body }, env)).status).toBe(200);
    expect((await call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers, body }, env)).status).toBe(200);
    const res = await call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers, body }, env);
    expect(res.status).toBe(429);
    expect(Number(res.headers.get("Retry-After"))).toBeGreaterThan(0);
    expect(((await res.json()) as { error: { code: string } }).error.code).toBe("rate_limited");
  });
});

describe("AI image caps and inline images", () => {
  const ip = () => `198.51.100.${1 + Math.floor(Math.random() * 250)}`;

  it("counts generated images, not requests, against the daily image caps", async () => {
    const { ai, calls } = mockAI();
    const site = uniqueSite("imgcap");
    const image = (n: number, env: Record<string, unknown>, who: string) =>
      call(`/s/${site}/__backyard/api/ai/image`, { method: "POST", headers: { ...apiHeaders(), "CF-Connecting-IP": who }, body: JSON.stringify({ prompt: "a fox", n }) }, { AI: ai, ...env });
    const four = { LIMITS: JSON.stringify({ ai_images_per_day: 4, ai_images_platform_per_day: 100000, ai_per_day: 100, ai_platform_per_day: 100000 }) };
    const who = ip();
    let res = await image(4, four, who);
    expect(res.status).toBe(200);
    expect(((await res.json()) as { images: unknown[] }).images).toHaveLength(4);
    expect(calls).toHaveLength(4);
    res = await image(1, four, who);
    expect(res.status).toBe(429);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("image generation limit");
    expect(calls).toHaveLength(4);
    // A request for more images than are left runs no model at all.
    const two = { LIMITS: JSON.stringify({ ai_images_per_day: 2, ai_images_platform_per_day: 100000, ai_per_day: 100, ai_platform_per_day: 100000 }) };
    res = await image(4, two, ip());
    expect(res.status).toBe(429);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("this request needs 4");
    expect(calls).toHaveLength(4);
    // The platform-wide image budget counts images too.
    const platform = { LIMITS: JSON.stringify({ ai_images_per_day: 100, ai_images_platform_per_day: 3, ai_per_day: 100, ai_platform_per_day: 100000 }) };
    res = await image(4, platform, ip());
    expect(res.status).toBe(429);
    expect(((await res.json()) as { error: { message: string } }).error.message).toContain("image budget");
  });

  it("passes the client's base64 on to the provider without decoding or re-encoding it", async () => {
    const { ai, calls } = mockAI();
    const site = uniqueSite("b64");
    const b64 = btoa(String.fromCharCode(...PNG));
    const chat = (part: Record<string, unknown>) =>
      call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ messages: [{ role: "user", content: [{ type: "text", text: "what is it?" }, part] }] }) }, { AI: ai });
    for (const part of [{ type: "image", data: b64 }, { type: "image", data: `data:image/png;base64,${b64}` }, { type: "image_url", image_url: { url: `data:image/png;base64,${b64}` } }]) {
      const res = await chat(part);
      expect(res.status, JSON.stringify(part).slice(0, 40)).toBe(200);
      const content = (calls[calls.length - 1]!.input.messages as { content: { type: string; image_url?: { url: string } }[] }[])[0]!.content;
      expect(content[1]).toEqual({ type: "image_url", image_url: { url: `data:image/png;base64,${b64}` } });
    }
    // Proof that nothing was decoded and encoded again: base64 whose unused
    // trailing bits are set decodes to the same bytes, but only a pass-through
    // keeps its exact text.
    const A = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    const last = b64.length - 2; // "…XYZ=": Z carries 2 unused bits
    const odd = b64.slice(0, last) + A[A.indexOf(b64[last]!) ^ 1] + b64.slice(last + 1);
    expect(odd).not.toBe(b64);
    expect((await chat({ type: "image", data: odd, media_type: "image/png" })).status).toBe(200);
    const sent = (calls[calls.length - 1]!.input.messages as { content: { image_url?: { url: string } }[] }[])[0]!.content[1]!;
    expect(sent.image_url!.url).toBe(`data:image/png;base64,${odd}`);
    // URL-safe or unpadded base64 is normalized, not refused.
    const urlSafe = btoa(String.fromCharCode(0xfb, 0xff, 0xfe, ...PNG.subarray(3))).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
    expect((await chat({ type: "image", data: urlSafe, media_type: "image/png" })).status).toBe(200);
    expect((await chat({ type: "image", data: "!!!" })).status).toBe(400);
    expect((await chat({ type: "image", data: "====" })).status).toBe(400);
  });

  it("refuses inline images above 20 MiB (SPEC §5.4)", () => {
    const big = "A".repeat(((20 << 20) / 3) * 4 + 8); // 20 MiB + 6 bytes
    const parse = (data: string) => parseChat({ messages: [{ role: "user", content: [{ type: "image", data, media_type: "image/png" }] }] }, "");
    expect(() => parse(big)).toThrow(/image exceeds 20 MiB/);
    expect(() => parse(`data:image/png;base64,${big}`)).toThrow(/image exceeds 20 MiB/);
    const ok = "A".repeat(((20 << 20) / 3) * 4 - 4); // just under
    expect(parse(ok).req.messages[0]!.content[0]).toMatchObject({ type: "image", media_type: "image/png" });
  });

  it("answers 404 for models named like members of Object.prototype", async () => {
    const { ai } = mockAI();
    const site = uniqueSite("protomodel");
    for (const model of ["constructor", "toString", "__proto__", "hasOwnProperty"]) {
      const res = await call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ model, messages: [{ role: "user", content: "x" }] }) }, { AI: ai });
      expect(res.status, model).toBe(404);
      expect(((await res.json()) as { error: { code: string } }).error.code).toBe("not_found");
    }
  });
});

describe("chat images from the site's own uploads (SPEC §5.4, Go parity)", () => {
  async function uploadFile(site: string, bytes: Uint8Array, name: string, type: string): Promise<{ id: string; url: string }> {
    const f = new FormData();
    f.append("file", new Blob([bytes], { type }), name);
    const res = await call(`/s/${site}/__backyard/api/files`, { method: "POST", headers: { "X-Backyard": "1" }, body: f });
    expect(res.status).toBe(201);
    return (await res.json()) as { id: string; url: string };
  }

  it("reads uploads named by their site-relative or same-origin URL, and refuses other site files", async () => {
    const { ai, calls } = mockAI();
    const site = uniqueSite("upimg");
    const other = uniqueSite("upimg2");
    const png = await uploadFile(site, PNG, "cat.png", "application/octet-stream");
    const text = await uploadFile(site, new TextEncoder().encode("not an image"), "x.png", "image/png");
    const foreign = await uploadFile(other, PNG, "dog.png", "image/png");
    const chat = (url: string) =>
      call(`/s/${site}/__backyard/api/ai/chat`, { method: "POST", headers: apiHeaders(), body: JSON.stringify({ messages: [{ role: "user", content: [{ type: "image", url }, { type: "text", text: "?" }] }] }) }, { AI: ai });
    const b64 = btoa(String.fromCharCode(...PNG));
    const relative = png.url;
    expect(relative).toBe(`/s/${site}/__backyard/files/${png.id}/cat.png`);
    for (const url of [relative, `/__backyard/files/${png.id}/cat.png`, `https://backyard.test${relative}`, `https://backyard.test:443${relative}?x=1`]) {
      const res = await chat(url);
      expect(res.status, url).toBe(200);
      const content = (calls[calls.length - 1]!.input.messages as { content: { image_url?: { url: string } }[] }[])[0]!.content;
      // Read by the Worker (the type detected from the bytes), never handed to the provider as a URL.
      expect(content[0], url).toEqual({ type: "image_url", image_url: { url: `data:image/png;base64,${b64}` } });
    }
    const n = calls.length;
    for (const [url, msg] of [
      [`/s/${site}/__backyard/files/01J0000000000000000000000/x.png`, "image file not found"],
      [`/s/${site}/__backyard/files/no*such/x.png`, "image file not found"],
      [foreign.url, "image is not a file of this site"],
      [`https://backyard.test${foreign.url}`, "image is not a file of this site"],
      [`/s/${site}/other/cat.png`, "an upload of this site"],
      [text.url, "not a png, jpeg, gif or webp image"],
    ] as const) {
      const res = await chat(url);
      expect(res.status, url).toBe(400);
      const err = ((await res.json()) as { error: { code: string; message: string } }).error;
      expect(err.code, url).toBe("bad_request");
      expect(err.message, url).toContain("message 0 part 0");
      expect(err.message, url).toContain(msg);
    }
    // An upload stored in R2 (above FILE_INLINE_MAX) is read the same way.
    const big = new Uint8Array(70_000);
    big.set(PNG);
    const inR2 = await uploadFile(site, big, "big.png", "image/png");
    expect((await chat(inR2.url)).status).toBe(200);
    const sentBig = (calls[calls.length - 1]!.input.messages as { content: { image_url?: { url: string } }[] }[])[0]!.content[0]!.image_url!.url;
    expect(sentBig.length).toBe("data:image/png;base64,".length + Math.ceil(70_000 / 3) * 4);
    expect(sentBig.startsWith(`data:image/png;base64,${b64.slice(0, 24)}`)).toBe(true);
    // Other URLs are not refused here: absolute URLs elsewhere go to the provider as they are.
    expect((await chat("https://images.example/cat.png")).status).toBe(200);
    const content = (calls[calls.length - 1]!.input.messages as { content: { image_url?: { url: string } }[] }[])[0]!.content;
    expect(content[0]).toEqual({ type: "image_url", image_url: { url: "https://images.example/cat.png" } });
    expect(calls.length).toBe(n + 2);
    expect((await chat("/relative.png")).status).toBe(400);
  });
});

describe("anthropic provider", () => {
  it("follows the current Messages API rules", () => {
    expect(acceptsSampling("claude-opus-5-5")).toBe(false);
    expect(acceptsSampling("claude-sonnet-4-5-20250929")).toBe(true);
    expect(acceptsSampling("claude-3-5-haiku-20241022")).toBe(true);
    expect(acceptsSampling("claude-opus-4-7")).toBe(false);
    expect(acceptsSampling("claude-fable-5-1")).toBe(false);
    expect(effortFor("claude-opus-5-5", "high")).toBe("high");
    expect(effortFor("claude-opus-4-6", "xhigh")).toBe("high");
    expect(effortFor("claude-opus-4-5", "max")).toBe("high");
    expect(effortFor("claude-sonnet-4-5", "high")).toBe("");
    expect(effortFor("claude-3-7-sonnet", "high")).toBe("");
    expect(supportsDefaultFallbacks("claude-opus-5-5")).toBe(true);
    expect(supportsDefaultFallbacks("claude-haiku-4-5")).toBe(false);
  });

  it("builds requests and parses streams", async () => {
    const seen: { url: string; headers: Record<string, string>; body: Record<string, unknown> }[] = [];
    const fetcher = (async (url: string, init: RequestInit) => {
      seen.push({ url, headers: init.headers as Record<string, string>, body: JSON.parse(String(init.body)) as Record<string, unknown> });
      const body = JSON.parse(String(init.body)) as { stream?: boolean };
      if (body.stream) {
        const events = [
          { type: "message_start", message: { usage: { input_tokens: 10, cache_read_input_tokens: 5 } } },
          { type: "content_block_delta", delta: { type: "thinking_delta", thinking: "" } },
          { type: "content_block_delta", delta: { type: "text_delta", text: "Hel" } },
          { type: "content_block_delta", delta: { type: "text_delta", text: "lo" } },
          { type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 3 } },
          { type: "message_stop" },
        ];
        return new Response(events.map((e) => `event: ${e.type}\ndata: ${JSON.stringify(e)}\n\n`).join(""), { headers: { "content-type": "text/event-stream" } });
      }
      return Response.json({ content: [{ type: "text", text: "Hi" }], stop_reason: "refusal", usage: { input_tokens: 1, output_tokens: 2 } });
    }) as unknown as typeof fetch;
    const p = new AnthropicProvider("sk-test", "https://api.anthropic.com", fetcher);
    const req = {
      model: "claude-opus-5-5",
      system: "sys",
      messages: [{ role: "user" as const, content: [{ type: "text" as const, text: "hello" }, { type: "image" as const, data: new Uint8Array([1, 2, 3]), media_type: "image/png" }] }],
      maxTokens: 1000,
      temperature: 0.5,
      json: false,
      user: "alice@acme.com",
    };
    const r = await p.chat(req);
    expect(r).toEqual({ content: "Hi", model: "claude-opus-5-5", stop_reason: "refusal", usage: { input_tokens: 1, output_tokens: 2 } });
    expect(seen[0]!.url).toBe("https://api.anthropic.com/v1/messages");
    expect(seen[0]!.headers).toMatchObject({ "x-api-key": "sk-test", "anthropic-version": "2023-06-01", "anthropic-beta": "server-side-fallback-2026-07-01" });
    expect(seen[0]!.body.temperature).toBeUndefined();
    expect(seen[0]!.body.thinking).toBeUndefined();
    expect(seen[0]!.body.fallbacks).toBe("default");
    expect(seen[0]!.body.system).toBe("sys");
    expect((seen[0]!.body.metadata as { user_id: string }).user_id).toMatch(/^u_[0-9a-f]{32}$/);
    expect(seen[0]!.body.messages).toEqual([
      { role: "user", content: [{ type: "text", text: "hello" }, { type: "image", source: { type: "base64", media_type: "image/png", data: "AQID" } }] },
    ]);
    const texts: string[] = [];
    const s = await p.stream({ ...req, model: "claude-sonnet-4-5" }, (t) => {
      texts.push(t);
    });
    expect(texts).toEqual(["Hel", "lo"]);
    expect(s).toMatchObject({ content: "Hello", stop_reason: "end_turn", usage: { input_tokens: 15, output_tokens: 3 } });
    expect(seen[1]!.body.temperature).toBe(0.5);
    expect(seen[1]!.body.fallbacks).toBeUndefined();
  });

  it("strips code fences in JSON mode", () => {
    expect(stripCodeFences('```json\n{"a":1}\n```')).toBe('{"a":1}');
    expect(stripCodeFences('Sure! {"a":1} done')).toBe('{"a":1}');
    expect(stripCodeFences("not json")).toBe("not json");
  });
});

/**
 * Provider failures map like the Go server's aiUpstreamError (SPEC §5.4); the
 * table mirrors internal/server TestAIUpstreamStatusMapping.
 */
describe("AI upstream errors (Go parity)", () => {
  afterEach(() => vi.unstubAllGlobals());

  const models = JSON.stringify({
    gpt: { provider: "openai", model: "gpt-x", kind: "chat" },
    pic: { provider: "openai", model: "gpt-image-x", kind: "image" },
    vec: { provider: "openai", model: "emb-x", kind: "embedding" },
  });
  const env = { AI_MODELS: models, OPENAI_API_KEY: "sk-test", LIMITS: JSON.stringify({ ai_per_day: 100000, ai_platform_per_day: 10000000 }) };
  type Reply = { status: number; body?: string; headers?: Record<string, string> } | Error;
  type Err = { error: { code: string; message: string } };

  /** Stubs fetch so the OpenAI provider gets reply. */
  function provider(reply: Reply): void {
    vi.stubGlobal("fetch", async () => {
      if (reply instanceof Error) throw reply;
      return new Response(reply.body ?? "", { status: reply.status, headers: reply.headers });
    });
  }
  const errBody = (message: string) => JSON.stringify({ error: { message, type: "x" } });
  const site = uniqueSite("aierr");
  const post = (path: string, body: unknown) =>
    call(
      `/s/${site}/__backyard/api/ai/${path}`,
      { method: "POST", headers: { ...apiHeaders(), "CF-Connecting-IP": "198.51.100.7" }, body: JSON.stringify(body) },
      env,
    );
  const chat = { messages: [{ role: "user", content: "hi" }] };

  const cases: { name: string; reply: Reply; status: number; code: string; retryAfter: string | null; contains?: string[]; excludes?: string[] }[] = [
    { name: "provider timeout", reply: { status: 504, body: errBody("no response") }, status: 504, code: "upstream_error", retryAfter: null, contains: ["did not respond in time"] },
    { name: "request timeout", reply: { status: 408, body: errBody("timeout") }, status: 504, code: "upstream_error", retryAfter: null },
    { name: "throttled with Retry-After", reply: { status: 429, body: errBody("slow down"), headers: { "Retry-After": "20" } }, status: 429, code: "rate_limited", retryAfter: "20" },
    { name: "throttled hint milliseconds", reply: { status: 429, body: errBody("Please try again in 350ms.") }, status: 429, code: "rate_limited", retryAfter: "1" },
    {
      name: "throttled hint seconds",
      reply: { status: 429, body: errBody("Rate limit reached for gpt-4o in organization org-AbCdEf123456 on tokens per min. Please try again in 1.5s.") },
      status: 429,
      code: "rate_limited",
      retryAfter: "2",
      excludes: ["org-AbCdEf123456"],
    },
    { name: "throttled azure hint", reply: { status: 429, body: errBody("Requests have exceeded token rate limit. Please retry after 6 seconds.") }, status: 429, code: "rate_limited", retryAfter: "6" },
    { name: "throttled hint minutes", reply: { status: 429, body: errBody("Quota exceeded; try again in 2 minutes") }, status: 429, code: "rate_limited", retryAfter: "120" },
    { name: "throttled unknown", reply: { status: 429, body: errBody("Too many requests, please wait before trying again.") }, status: 503, code: "upstream_error", retryAfter: null, contains: ["rate limiting"] },
    { name: "throttled absurd hint", reply: { status: 429, body: errBody("retry after 999999 seconds") }, status: 429, code: "rate_limited", retryAfter: "3600" },
    {
      name: "prefill rejected",
      reply: { status: 400, body: errBody("This model does not support assistant message prefill. The conversation must end with a user message.") },
      status: 400,
      code: "bad_request",
      retryAfter: null,
      contains: ["rejected the request", "does not support assistant message prefill"],
    },
    { name: "unprocessable", reply: { status: 422, body: errBody("invalid image") }, status: 400, code: "bad_request", retryAfter: null, contains: ["invalid image"] },
    { name: "too large", reply: { status: 413, body: errBody("request too large") }, status: 413, code: "too_large", retryAfter: null, contains: ["too large"] },
    {
      name: "bad request leaks",
      reply: { status: 400, body: errBody("see https://internal.example:8443/v1/deploy/secret-deployment?api-version=1 for key sk-proj-abcdefghijklmnop\u0000\nnext") },
      status: 400,
      code: "bad_request",
      retryAfter: null,
      contains: ["[url]", "[redacted]", "next"],
      excludes: ["internal.example", "secret-deployment", "abcdefghijklmnop", "\u0000", "\n"],
    },
    { name: "empty 400 message", reply: { status: 400, body: "" }, status: 400, code: "bad_request", retryAfter: null, contains: ["bad request"] },
    { name: "credentials", reply: { status: 401, body: errBody("invalid x-api-key") }, status: 502, code: "upstream_error", retryAfter: null, contains: ["invalid x-api-key"] },
    { name: "overloaded", reply: { status: 529, body: errBody("Overloaded") }, status: 502, code: "upstream_error", retryAfter: null, contains: ["Overloaded"] },
    { name: "plain-text body", reply: { status: 503, body: "upstream connect error" }, status: 502, code: "upstream_error", retryAfter: null, contains: ["upstream connect error"] },
    { name: "long message", reply: { status: 500, body: errBody("é".repeat(400)) }, status: 502, code: "upstream_error", retryAfter: null, contains: ["…"] },
    { name: "network failure", reply: new TypeError("Network connection lost"), status: 502, code: "upstream_error", retryAfter: null },
    { name: "client timeout", reply: new DOMException("The operation was aborted due to timeout", "TimeoutError"), status: 504, code: "upstream_error", retryAfter: null, contains: ["did not respond in time"] },
  ];

  for (const tc of cases) {
    it(`maps ${tc.name}`, async () => {
      provider(tc.reply);
      const res = await post("chat", chat);
      expect(res.status).toBe(tc.status);
      expect(res.headers.get("Retry-After")).toBe(tc.retryAfter);
      const e = ((await res.json()) as Err).error;
      expect(e.code).toBe(tc.code);
      for (const s of tc.contains ?? []) expect(e.message).toContain(s);
      for (const s of tc.excludes ?? []) expect(e.message).not.toContain(s);
      expect(new TextEncoder().encode(e.message).length).toBeLessThanOrEqual(400);
    });
  }

  it("reads an HTTP-date Retry-After", async () => {
    provider({ status: 429, body: errBody("slow down"), headers: { "Retry-After": new Date(Date.now() + 30_000).toUTCString() } });
    const res = await post("chat", chat);
    expect(res.status).toBe(429);
    const after = Number(res.headers.get("Retry-After"));
    expect(after).toBeGreaterThan(25);
    expect(after).toBeLessThanOrEqual(31);
  });

  it("maps images, embeddings and malformed responses the same way", async () => {
    provider({ status: 400, body: errBody("prompt rejected by safety system") });
    let res = await post("image", { prompt: "x", model: "pic" });
    expect(res.status).toBe(400);
    expect(((await res.json()) as Err).error.code).toBe("bad_request");
    provider({ status: 504, body: errBody("no response within 1m0s") });
    res = await post("embed", { input: "x", model: "vec" });
    expect(res.status).toBe(504);
    // A 200 with a body that is not JSON is a provider failure (Go: 502 "AI request failed"), not a 500.
    provider({ status: 200, body: "<html>proxy login</html>" });
    res = await post("chat", chat);
    expect(res.status).toBe(502);
    expect(((await res.json()) as Err).error).toEqual({ code: "upstream_error", message: "AI request failed" });
  });

  it("reports the mapped error in a stream (TestAIUpstreamStatusInStream)", async () => {
    for (const [reply, code, msg] of [
      [{ status: 400, body: errBody("prefill not supported") }, "bad_request", "prefill not supported"],
      [{ status: 429, body: errBody("slow down") }, "upstream_error", "rate limiting"],
      [{ status: 504, body: errBody("no response within 10m0s") }, "upstream_error", "did not respond in time"],
      [new DOMException("timed out", "TimeoutError"), "upstream_error", "did not respond in time"],
    ] as [Reply, string, string][]) {
      provider(reply);
      const res = await post("chat", { ...chat, stream: true });
      const events = await readSSE(res);
      expect(events).toHaveLength(1);
      expect(events[0]).toMatchObject({ type: "error", error: { code } });
      expect((events[0]!.error as { message: string }).message).toContain(msg);
    }
  });

  it("maps Anthropic error events by type like the Go provider", async () => {
    const run = async (type: string, message: string) => {
      const fetcher = (async () =>
        new Response(`event: error\ndata: ${JSON.stringify({ type: "error", error: { type, message } })}\n\n`, { headers: { "content-type": "text/event-stream" } })) as unknown as typeof fetch;
      const p = new AnthropicProvider("sk-test", "https://api.anthropic.com", fetcher);
      const req = { model: "claude-sonnet-4-5", system: "", messages: [{ role: "user" as const, content: [{ type: "text" as const, text: "hi" }] }], maxTokens: 10, json: false, user: "" };
      return p.stream(req, () => {}).then(
        () => null,
        (e: { status: number; code: string; message: string; retryAfter?: number }) => e,
      );
    };
    // Overloaded is a provider outage (502), not the caller's rate limit.
    expect(await run("overloaded_error", "Overloaded")).toMatchObject({ status: 502, code: "upstream_error" });
    expect(await run("rate_limit_error", "slow down")).toMatchObject({ status: 503, code: "upstream_error" });
    expect(await run("rate_limit_error", "try again in 7s")).toMatchObject({ status: 429, code: "rate_limited", retryAfter: 7 });
    expect(await run("invalid_request_error", "bad image")).toMatchObject({ status: 400, code: "bad_request" });
    expect(await run("request_too_large", "too big")).toMatchObject({ status: 413, code: "too_large" });
    expect(await run("timeout_error", "slow")).toMatchObject({ status: 504, code: "upstream_error" });
    // HTTP errors too: 529 → 502 (it used to be reported as rate_limited).
    const fetcher529 = (async () => new Response(errBody("Overloaded"), { status: 529 })) as unknown as typeof fetch;
    const p = new AnthropicProvider("sk-test", "https://api.anthropic.com", fetcher529);
    const err = await p.chat({ model: "claude-sonnet-4-5", system: "", messages: [{ role: "user", content: [{ type: "text", text: "hi" }] }], maxTokens: 10, json: false, user: "" }).catch((e: unknown) => e);
    expect(err).toMatchObject({ status: 502, code: "upstream_error", message: "AI provider error: Overloaded" });
  });
});

/**
 * The warehouse proxy (WAREHOUSE_URL) answers like the Go server's
 * warehouseError (SPEC §5.5): query errors pass through, server-side
 * failures get a generic message naming the request id.
 */
describe("warehouse proxy (Go parity)", () => {
  afterEach(() => vi.unstubAllGlobals());

  const env = { WAREHOUSE_URL: "https://backyard.internal.example/__backyard/api/", WAREHOUSE_TOKEN: "wh-token-123" };
  type Reply = { status: number; body?: string; headers?: Record<string, string> } | Error;
  type Err = { error: { code: string; message: string; login_url?: string } };
  const seen: Request[] = [];

  function upstream(reply: Reply): void {
    seen.length = 0;
    vi.stubGlobal("fetch", async (input: RequestInfo, init?: RequestInit) => {
      seen.push(new Request(input, init));
      if (reply instanceof Error) throw reply;
      return new Response(reply.body ?? "", { status: reply.status, headers: reply.headers });
    });
  }
  const site = uniqueSite("wh");
  const query = (headers: Record<string, string> = {}) =>
    call(`/s/${site}/__backyard/api/warehouse/query`, { method: "POST", headers: { ...apiHeaders(), ...headers }, body: '{"sql":"select 1"}' }, env);
  const jsonErr = (status: number, code: string, message: string, headers: Record<string, string> = {}) => ({
    status,
    body: JSON.stringify({ error: { code, message } }),
    headers: { "Content-Type": "application/json", ...headers },
  });
  const generic = (rid: string) => `the warehouse could not run the query; the details are in the server log (request id ${rid})`;

  it("relays results and sends the token and the request id upstream", async () => {
    const result = { columns: [{ name: "n", type: "INT" }], rows: [{ n: 1 }], row_count: 1, truncated: false };
    upstream({ status: 200, body: JSON.stringify(result), headers: { "Content-Type": "application/json; charset=utf-8" } });
    const res = await query({ "X-Request-Id": "req-0123456789" });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual(result);
    expect(seen[0]!.url).toBe("https://backyard.internal.example/__backyard/api/warehouse/query");
    expect(seen[0]!.headers.get("Authorization")).toBe("Bearer wh-token-123");
    expect(seen[0]!.headers.get("X-Backyard")).toBe("1");
    expect(seen[0]!.headers.get("X-Request-Id")).toBe("req-0123456789");
    expect(seen[0]!.redirect).toBe("manual");
    expect(res.headers.get("X-Request-Id")).toBe("req-0123456789");
  });

  it("passes on the caller's query errors and rate limits", async () => {
    for (const [status, code, message] of [
      [400, "bad_request", 'syntax error at or near "SELEC"'],
      [403, "forbidden", "raw SQL is disabled on this server"],
      [404, "not_found", "unknown named query weekly"],
      [501, "unsupported", "the data warehouse is not configured on this server"],
    ] as [number, string, string][]) {
      upstream(jsonErr(status, code, message));
      const res = await query();
      expect(res.status, code).toBe(status);
      expect(((await res.json()) as Err).error).toEqual({ code, message });
    }
    upstream(jsonErr(429, "rate_limited", "rate limit exceeded", { "Retry-After": "12" }));
    const res = await query();
    expect(res.status).toBe(429);
    expect(res.headers.get("Retry-After")).toBe("12");
    expect(((await res.json()) as Err).error.code).toBe("rate_limited");
  });

  it("hides server-side failures behind a generic message naming the request id", async () => {
    const leaky = "pq: password authentication failed for user admin at db.internal:5432";
    for (const reply of [
      new TypeError(`connect ECONNREFUSED 10.1.2.3:443 (${leaky})`),
      jsonErr(500, "internal", leaky),
      jsonErr(502, "upstream_error", leaky),
      { status: 502, body: `<html>Bad gateway: ${leaky}</html>`, headers: { "Content-Type": "text/html" } },
      // WAREHOUSE_TOKEN rejected: not the user's sign-in (no login loop, no login_url).
      { status: 401, body: JSON.stringify({ error: { code: "unauthenticated", message: leaky, login_url: "https://idp.example/login" } }), headers: { "Content-Type": "application/json" } },
      { status: 302, headers: { Location: "https://idp.example/login" } },
      { status: 200, body: `<html>${leaky}</html>`, headers: { "Content-Type": "text/html" } },
      jsonErr(418, "teapot", leaky),
    ] as Reply[]) {
      upstream(reply);
      const res = await query();
      const what = reply instanceof Error ? reply.message : String(reply.status);
      expect(res.status, what).toBe(502);
      expect(res.headers.get("Content-Type")).toBe("application/json");
      const e = ((await res.json()) as Err).error;
      expect(e, what).toEqual({ code: "upstream_error", message: generic(res.headers.get("X-Request-Id")!) });
    }
  });

  it("reports timeouts as 504", async () => {
    for (const reply of [new DOMException("The operation was aborted due to timeout", "TimeoutError"), jsonErr(504, "upstream_error", "warehouse query timed out")] as Reply[]) {
      upstream(reply);
      const res = await query();
      expect(res.status).toBe(504);
      expect(((await res.json()) as Err).error).toEqual({ code: "upstream_error", message: "warehouse query timed out" });
    }
  });
});
