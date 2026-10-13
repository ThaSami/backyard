/** AI proxy (SPEC §5.4) against a mocked Workers AI binding and mocked HTTP providers. */
import { afterEach, describe, expect, it, vi } from "vitest";
import { AnthropicProvider, acceptsSampling, effortFor, supportsDefaultFallbacks } from "../src/ai/anthropic";
import { stripCodeFences } from "../src/ai/types";
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
