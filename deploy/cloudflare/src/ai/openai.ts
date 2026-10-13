/**
 * OpenAI and OpenAI-compatible APIs (chat completions, images, embeddings)
 * via fetch. Like the Go provider it adapts to servers that reject a field:
 * a 400 naming max_tokens / max_completion_tokens, temperature,
 * response_format or stream_options is retried once without it.
 */
import { base64Decode } from "../crypto";
import { HttpError } from "../http";
import { sniff } from "../mime";
import { readSSE } from "./sse";
import {
  dataURL,
  opaqueUserID,
  stripCodeFences,
  systemWithJSON,
  upstreamError,
  type ChatRequest,
  type ChatResponse,
  type EmbedRequest,
  type EmbedResponse,
  type GeneratedImage,
  type ImageRequest,
  type Provider,
  type StopReason,
  type Usage,
} from "./types";

type AnyRecord = Record<string, unknown>;

const REQUEST_TIMEOUT_MS = 10 * 60 * 1000;

function stopFrom(r: unknown): StopReason {
  switch (r) {
    case undefined:
    case null:
    case "stop":
      return "end_turn";
    case "length":
    case "max_tokens":
      return "max_tokens";
    case "content_filter":
      return "refusal";
  }
  return "other";
}

function usageFrom(u: unknown): Usage {
  const x = (u ?? {}) as AnyRecord;
  const n = (v: unknown) => (typeof v === "number" && Number.isFinite(v) ? v : 0);
  return { input_tokens: n(x.prompt_tokens), output_tokens: n(x.completion_tokens) };
}

export class OpenAIProvider implements Provider {
  readonly name = "openai";
  private readonly firstParty: boolean;

  constructor(
    private readonly apiKey: string,
    private readonly baseURL: string,
    private readonly fetcher: typeof fetch = fetch,
  ) {
    this.firstParty = /^https:\/\/api\.openai\.com\//.test(baseURL + "/");
  }

  private async post(path: string, body: AnyRecord): Promise<Response> {
    if (!this.apiKey) throw new HttpError(501, "unsupported", "OPENAI_API_KEY is not set on this Worker");
    const send = () =>
      this.fetcher(`${this.baseURL}/${path}`, {
        method: "POST",
        headers: { "content-type": "application/json", authorization: `Bearer ${this.apiKey}` },
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
    for (let attempt = 0; attempt < 3; attempt++) {
      let res: Response;
      try {
        res = await send();
      } catch (e) {
        throw upstreamError(this.name, (e as Error).message || "request failed");
      }
      if (res.ok) return res;
      let msg = `HTTP ${res.status}`;
      try {
        const j = (await res.json()) as { error?: { message?: string } | string };
        const m = typeof j.error === "string" ? j.error : j.error?.message;
        if (m) msg = m;
      } catch {
        /* keep the status */
      }
      if (res.status === 400 && this.adapt(body, msg)) continue;
      if (res.status === 400) throw new HttpError(400, "bad_request", `AI provider rejected the request: ${msg.slice(0, 500)}`);
      throw upstreamError(this.name, msg, res.status === 429 ? 429 : res.status === 504 ? 504 : 502);
    }
    throw upstreamError(this.name, "the provider kept rejecting the request");
  }

  /** Adjusts body after a 400 naming a field; reports whether to retry. */
  private adapt(body: AnyRecord, msg: string): boolean {
    const m = msg.toLowerCase();
    if (m.includes("max_completion_tokens") && "max_completion_tokens" in body) {
      body.max_tokens = body.max_completion_tokens;
      delete body.max_completion_tokens;
      return true;
    }
    if (m.includes("max_tokens") && "max_tokens" in body) {
      body.max_completion_tokens = body.max_tokens;
      delete body.max_tokens;
      return true;
    }
    for (const f of ["stream_options", "response_format", "temperature"]) {
      if (m.includes(f) && f in body) {
        delete body[f];
        return true;
      }
    }
    return false;
  }

  private async chatBody(req: ChatRequest, stream: boolean): Promise<AnyRecord> {
    const messages: AnyRecord[] = [];
    const system = req.json ? systemWithJSON(req.system) : req.system;
    if (system) messages.push({ role: "system", content: system });
    for (const m of req.messages) {
      const hasImage = m.content.some((p) => p.type === "image");
      messages.push({
        role: m.role,
        content: hasImage
          ? m.content.map((p) =>
              p.type === "text"
                ? { type: "text", text: p.text ?? "" }
                : { type: "image_url", image_url: { url: p.data ? dataURL(p.data, p.media_type || "image/png") : (p.url ?? "") } },
            )
          : m.content.map((p) => p.text ?? "").join(""),
      });
    }
    const body: AnyRecord = { model: req.model, messages };
    if (this.firstParty) body.max_completion_tokens = req.maxTokens;
    else body.max_tokens = req.maxTokens;
    if (req.temperature !== undefined) body.temperature = req.temperature;
    if (req.json) body.response_format = { type: "json_object" };
    const uid = await opaqueUserID(req.user);
    if (uid) body.user = uid;
    if (stream) {
      body.stream = true;
      body.stream_options = { include_usage: true };
    }
    return body;
  }

  async chat(req: ChatRequest): Promise<ChatResponse> {
    const res = await this.post("chat/completions", await this.chatBody(req, false));
    const j = (await res.json()) as AnyRecord;
    const choice = (j.choices as AnyRecord[] | undefined)?.[0];
    const msg = choice?.message as AnyRecord | undefined;
    const content = typeof msg?.content === "string" ? msg.content : "";
    let stop = stopFrom(choice?.finish_reason);
    if (msg?.refusal) stop = "refusal";
    return { content: req.json ? stripCodeFences(content) : content, model: req.model, stop_reason: stop, usage: usageFrom(j.usage) };
  }

  async stream(req: ChatRequest, onText: (t: string) => Promise<void> | void): Promise<ChatResponse> {
    const res = await this.post("chat/completions", await this.chatBody(req, true));
    if (!res.body) throw upstreamError(this.name, "empty stream");
    let content = "";
    let stop: unknown;
    let usage: Usage = { input_tokens: 0, output_tokens: 0 };
    await readSSE(res.body, async (ev) => {
      const d = ev.data.trim();
      if (!d || d === "[DONE]") return;
      let j: AnyRecord;
      try {
        j = JSON.parse(d) as AnyRecord;
      } catch {
        return;
      }
      if (j.error) throw upstreamError(this.name, JSON.stringify(j.error).slice(0, 500));
      const choice = (j.choices as AnyRecord[] | undefined)?.[0];
      const delta = choice?.delta as AnyRecord | undefined;
      if (choice?.finish_reason) stop = choice.finish_reason;
      if (j.usage) usage = usageFrom(j.usage);
      if (typeof delta?.content === "string" && delta.content) {
        content += delta.content;
        await onText(delta.content);
      }
    });
    return { content: req.json ? stripCodeFences(content) : content, model: req.model, stop_reason: stopFrom(stop), usage };
  }

  async image(req: ImageRequest): Promise<GeneratedImage[]> {
    const body: AnyRecord = { model: req.model, prompt: req.prompt, n: Math.min(Math.max(req.n, 1), 4) };
    if (req.size) body.size = req.size;
    if (!/gpt-image/i.test(req.model)) body.response_format = "b64_json";
    const uid = await opaqueUserID(req.user);
    if (uid) body.user = uid;
    const res = await this.post("images/generations", body);
    const j = (await res.json()) as { data?: { b64_json?: string; url?: string }[] };
    const out: GeneratedImage[] = [];
    for (const d of j.data ?? []) {
      let bytes: Uint8Array | null = null;
      if (d.b64_json) bytes = base64Decode(d.b64_json);
      else if (d.url) {
        const r = await this.fetcher(d.url);
        if (r.ok) bytes = new Uint8Array(await r.arrayBuffer());
      }
      if (!bytes) throw upstreamError(this.name, "image has neither b64_json nor url");
      const mt = sniff(bytes.subarray(0, 64));
      out.push({ data: bytes, media_type: mt.startsWith("image/") ? mt : "image/png" });
    }
    if (!out.length) throw upstreamError(this.name, "the model returned no image");
    return out;
  }

  async embed(req: EmbedRequest): Promise<EmbedResponse> {
    const uid = await opaqueUserID(req.user);
    const body: AnyRecord = { model: req.model, input: req.input };
    if (uid) body.user = uid;
    const res = await this.post("embeddings", body);
    const j = (await res.json()) as { data?: { index?: number; embedding?: number[] }[]; usage?: AnyRecord };
    const data = j.data ?? [];
    if (data.length !== req.input.length) throw upstreamError(this.name, `got ${data.length} embeddings for ${req.input.length} inputs`);
    const sorted = [...data].sort((a, b) => (a.index ?? 0) - (b.index ?? 0));
    return {
      embeddings: sorted.map((d) => d.embedding ?? []),
      usage: { input_tokens: typeof j.usage?.prompt_tokens === "number" ? j.usage.prompt_tokens : 0, output_tokens: 0 },
    };
  }
}
