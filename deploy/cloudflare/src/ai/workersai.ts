/**
 * Workers AI provider (the AI binding): chat (legacy {response} and
 * OpenAI-style {choices} result shapes, SSE streaming), image generation
 * (base64 JSON, raw image streams and the multipart FLUX.2 family) and
 * embeddings (BGE and friends).
 */
import { base64Decode } from "../crypto";
import { HttpError } from "../http";
import { sniff } from "../mime";
import { readSSE } from "./sse";
import {
  errorBodyMessage,
  imageURL,
  isTimeout,
  stripCodeFences,
  systemWithJSON,
  upstreamError,
  type ChatRequest,
  type ChatResponse,
  type EmbedRequest,
  type EmbedResponse,
  type GeneratedImage,
  type ImageRequest,
  type Message,
  type Provider,
  type StopReason,
  type Usage,
} from "./types";

type AnyRecord = Record<string, unknown>;

/** Minimal view of the AI binding (easy to mock in tests). */
export interface AIBinding {
  run(model: string, input: AnyRecord, options?: AnyRecord): Promise<unknown>;
}

function contentOf(m: Message): string | AnyRecord[] {
  const hasImage = m.content.some((p) => p.type === "image");
  if (!hasImage) return m.content.map((p) => p.text ?? "").join("");
  return m.content.map((p) =>
    p.type === "text"
      ? { type: "text", text: p.text ?? "" }
      : { type: "image_url", image_url: { url: imageURL(p) } },
  );
}

function stopFrom(reason: unknown): StopReason {
  switch (reason) {
    case undefined:
    case null:
    case "stop":
    case "end_turn":
    case "eos":
      return "end_turn";
    case "length":
    case "max_tokens":
      return "max_tokens";
    case "content_filter":
      return "refusal";
    case "stop_sequence":
      return "stop_sequence";
  }
  return "other";
}

function usageFrom(u: unknown): Usage {
  const x = (u ?? {}) as AnyRecord;
  const n = (v: unknown) => (typeof v === "number" && Number.isFinite(v) ? Math.max(0, Math.floor(v)) : 0);
  return { input_tokens: n(x.prompt_tokens ?? x.input_tokens), output_tokens: n(x.completion_tokens ?? x.output_tokens) };
}

/** Text of a Workers AI chat result (shapes vary by model family). */
export function extractText(result: unknown): string {
  const r = (result ?? {}) as AnyRecord;
  if (typeof r.response === "string") return r.response;
  if (r.response && typeof r.response === "object") return JSON.stringify(r.response);
  const choices = r.choices as AnyRecord[] | undefined;
  const msg = choices?.[0]?.message as AnyRecord | undefined;
  if (typeof msg?.content === "string") return msg.content;
  if (Array.isArray(msg?.content)) {
    return (msg.content as AnyRecord[])
      .filter((c) => c?.type === "text" || c?.type === "output_text")
      .map((c) => String(c.text ?? ""))
      .join("");
  }
  if (typeof r.output_text === "string") return r.output_text;
  if (Array.isArray(r.output)) {
    const parts: string[] = [];
    for (const item of r.output as AnyRecord[]) {
      if (item?.type === "message" && Array.isArray(item.content)) {
        for (const c of item.content as AnyRecord[]) {
          if (typeof c?.text === "string" && c.type !== "reasoning_text") parts.push(c.text);
        }
      }
    }
    return parts.join("");
  }
  return "";
}

export class WorkersAIProvider implements Provider {
  readonly name = "workers-ai";

  constructor(
    private readonly ai: AIBinding | undefined,
    private readonly gatewayID = "",
  ) {}

  private binding(): AIBinding {
    if (!this.ai) throw new HttpError(501, "unsupported", "Workers AI is not bound to this Worker (add \"ai\": {\"binding\": \"AI\"})");
    return this.ai;
  }

  private options(): AnyRecord | undefined {
    return this.gatewayID ? { gateway: { id: this.gatewayID } } : undefined;
  }

  private input(req: ChatRequest, stream: boolean): AnyRecord {
    const system = req.json ? systemWithJSON(req.system) : req.system;
    const messages: AnyRecord[] = [];
    if (system) messages.push({ role: "system", content: system });
    for (const m of req.messages) messages.push({ role: m.role, content: contentOf(m) });
    const input: AnyRecord = { messages, max_tokens: req.maxTokens };
    if (req.temperature !== undefined) input.temperature = req.temperature;
    if (stream) input.stream = true;
    return input;
  }

  private async run(model: string, input: AnyRecord): Promise<unknown> {
    try {
      return await this.binding().run(model, input, this.options());
    } catch (e) {
      if (e instanceof HttpError) throw e;
      const msg = e instanceof Error ? e.message : String(e);
      // The binding throws plain errors ("3040: Capacity temporarily exceeded…"):
      // throttling becomes 429 (→ 429 with a retry hint, else 503), the rest 502.
      throw upstreamError(this.name, msg, /rate|limit|capacity|429/i.test(msg) ? 429 : isTimeout(e) ? 504 : 0);
    }
  }

  async chat(req: ChatRequest): Promise<ChatResponse> {
    const result = (await this.run(req.model, this.input(req, false))) as AnyRecord;
    let content = extractText(result);
    if (req.json) content = stripCodeFences(content);
    const choice = (result?.choices as AnyRecord[] | undefined)?.[0];
    return { content, model: req.model, stop_reason: stopFrom(choice?.finish_reason), usage: usageFrom(result?.usage) };
  }

  async stream(req: ChatRequest, onText: (t: string) => Promise<void> | void): Promise<ChatResponse> {
    const result = await this.run(req.model, this.input(req, true));
    if (!(result instanceof ReadableStream)) {
      // Some models ignore stream: true and answer at once.
      const content = extractText(result);
      if (content) await onText(content);
      const r = (result ?? {}) as AnyRecord;
      return { content, model: req.model, stop_reason: stopFrom((r.choices as AnyRecord[] | undefined)?.[0]?.finish_reason), usage: usageFrom(r.usage) };
    }
    let content = "";
    let stop: unknown;
    let usage: Usage = { input_tokens: 0, output_tokens: 0 };
    await readSSE(result as ReadableStream<Uint8Array>, async (ev) => {
      const d = ev.data.trim();
      if (!d || d === "[DONE]") return;
      let j: AnyRecord;
      try {
        j = JSON.parse(d) as AnyRecord;
      } catch {
        return;
      }
      if (j.error) throw upstreamError(this.name, errorBodyMessage(JSON.stringify({ error: j.error })));
      let text = "";
      if (typeof j.response === "string") text = j.response;
      const choice = (j.choices as AnyRecord[] | undefined)?.[0];
      const delta = choice?.delta as AnyRecord | undefined;
      if (typeof delta?.content === "string") text = delta.content;
      if (choice?.finish_reason) stop = choice.finish_reason;
      if (j.usage) usage = usageFrom(j.usage);
      if (text) {
        content += text;
        await onText(text);
      }
    });
    return { content: req.json ? stripCodeFences(content) : content, model: req.model, stop_reason: stopFrom(stop), usage };
  }

  async image(req: ImageRequest): Promise<GeneratedImage[]> {
    const [w, h] = /^\d+x\d+$/.test(req.size) ? req.size.split("x").map(Number) : [1024, 1024];
    const clamp = (v: number | undefined) => Math.min(2048, Math.max(256, Math.round((v || 1024) / 32) * 32));
    const out: GeneratedImage[] = [];
    for (let i = 0; i < Math.min(Math.max(req.n, 1), 4); i++) {
      let input: AnyRecord;
      if (/flux-2/i.test(req.model)) {
        // The FLUX.2 family takes multipart form input.
        const form = new FormData();
        form.append("prompt", req.prompt);
        form.append("width", String(clamp(w)));
        form.append("height", String(clamp(h)));
        const fr = new Response(form);
        input = { multipart: { body: fr.body, contentType: fr.headers.get("content-type") } };
      } else if (/flux-1-schnell/i.test(req.model)) {
        input = { prompt: req.prompt, steps: 4 };
      } else {
        input = { prompt: req.prompt, width: clamp(w), height: clamp(h) };
      }
      const result = await this.run(req.model, input);
      out.push(await toImage(result, this.name));
    }
    return out;
  }

  async embed(req: EmbedRequest): Promise<EmbedResponse> {
    const result = (await this.run(req.model, { text: req.input })) as AnyRecord;
    const data = result?.data;
    if (!Array.isArray(data) || data.length !== req.input.length) {
      throw upstreamError(this.name, `expected ${req.input.length} embeddings`);
    }
    return { embeddings: data as number[][], usage: usageFrom(result.usage) };
  }
}

async function toImage(result: unknown, provider: string): Promise<GeneratedImage> {
  let bytes: Uint8Array | null = null;
  if (result instanceof ReadableStream) {
    bytes = new Uint8Array(await new Response(result).arrayBuffer());
  } else if (result instanceof ArrayBuffer) {
    bytes = new Uint8Array(result);
  } else if (result instanceof Uint8Array) {
    bytes = result;
  } else if (result && typeof result === "object" && typeof (result as AnyRecord).image === "string") {
    try {
      bytes = base64Decode((result as AnyRecord).image as string);
    } catch {
      throw upstreamError(provider, "the model returned an invalid image");
    }
  }
  if (!bytes || bytes.length === 0) throw upstreamError(provider, "the model returned no image");
  const mt = sniff(bytes.subarray(0, 64));
  return { data: bytes, media_type: mt.startsWith("image/") ? mt : "image/png" };
}
