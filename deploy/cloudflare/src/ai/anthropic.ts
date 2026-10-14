/**
 * Claude via the Anthropic Messages API (raw fetch; no SDK in the Worker).
 * Request rules follow the current API, as the Go provider does: temperature
 * only for models that accept sampling parameters (Claude 3.x and 4.0–4.6),
 * thinking left at the model default (adaptive on current models),
 * output_config.effort only when configured, and server-side refusal
 * fallbacks ("fallbacks": "default") for the models that support them.
 */
import { HttpError } from "../http";
import { readSSE } from "./sse";
import {
  hasInlineImage,
  isTimeout,
  opaqueUserID,
  partBase64,
  providerFailure,
  stripCodeFences,
  systemWithJSON,
  upstreamError,
  type ChatRequest,
  type ChatResponse,
  type EmbedResponse,
  type GeneratedImage,
  type Provider,
  type StopReason,
  type Usage,
} from "./types";

type AnyRecord = Record<string, unknown>;

const API_VERSION = "2023-06-01";
const FALLBACK_BETA = "server-side-fallback-2026-07-01";
const REQUEST_TIMEOUT_MS = 10 * 60 * 1000;

/**
 * HTTP statuses of the error types delivered inside an otherwise successful
 * event stream (the Go provider's errorTypeStatus).
 */
const STREAM_ERROR_STATUS: Record<string, number> = {
  invalid_request_error: 400,
  authentication_error: 401,
  billing_error: 402,
  permission_error: 403,
  not_found_error: 404,
  request_too_large: 413,
  rate_limit_error: 429,
  api_error: 500,
  timeout_error: 504,
  overloaded_error: 529,
};

interface ClaudeVersion {
  family: string;
  major: number;
  minor: number;
}

/** Canonical Claude id without platform prefixes or date suffixes ("" if not Claude). */
export function claudeBase(model: string): string {
  let m = model.trim().toLowerCase();
  const i = m.indexOf("claude");
  if (i < 0) return "";
  m = m.slice(i);
  const j = m.search(/[@:]/);
  if (j >= 0) m = m.slice(0, j);
  const out: string[] = [];
  for (const p of m.split("-")) {
    if (!p) continue;
    if (/^\d{8}$/.test(p) || /^v\d+$/.test(p)) break;
    out.push(p);
  }
  return out.join("-");
}

function parseClaude(model: string): ClaudeVersion | null {
  const b = claudeBase(model);
  if (!b) return null;
  const v: ClaudeVersion = { family: "", major: 0, minor: 0 };
  const nums: number[] = [];
  for (const p of b.split("-").slice(1)) {
    if (/^\d+$/.test(p)) nums.push(Number(p));
    else if (!v.family) v.family = p;
  }
  if (!nums.length) return null;
  v.major = nums[0]!;
  v.minor = nums[1] ?? 0;
  return v;
}

/** Whether the model accepts temperature (Claude 3.x, 4.0–4.6). */
export function acceptsSampling(model: string): boolean {
  const v = parseClaude(model);
  if (!v || v.family === "fable" || v.family === "mythos") return false;
  if (v.major < 4) return true;
  return v.major === 4 && v.minor <= 6;
}

/** Effort level the model accepts ("" when it has no effort parameter). */
export function effortFor(model: string, effort: string): string {
  if (!effort) return "";
  const v = parseClaude(model);
  if (!v || v.family === "fable" || v.family === "mythos") return effort;
  if (v.major < 4) return "";
  if (v.major === 4) {
    if (v.minor < 5) return "";
    if (v.minor === 5) {
      if (v.family !== "opus") return "";
      return effort === "xhigh" || effort === "max" ? "high" : effort;
    }
    if (v.minor === 6 && effort === "xhigh") return "high";
  }
  return effort;
}

const FALLBACK_MODELS = new Set(["claude-opus-5-5", "claude-opus-5", "claude-fable-5-1", "claude-sonnet-5-5"]);

/** Whether the model supports "fallbacks": "default". */
export function supportsDefaultFallbacks(model: string): boolean {
  return FALLBACK_MODELS.has(claudeBase(model));
}

function stopFrom(r: unknown): StopReason {
  switch (r) {
    case "end_turn":
      return "end_turn";
    case "max_tokens":
    case "model_context_window_exceeded":
      return "max_tokens";
    case "stop_sequence":
      return "stop_sequence";
    case "refusal":
      return "refusal";
  }
  return "other";
}

function n(v: unknown): number {
  return typeof v === "number" && Number.isFinite(v) ? v : 0;
}

/** Totals usage (with fallbacks, usage.iterations lists every attempt). */
export function usageFrom(u: unknown): Usage {
  const x = (u ?? {}) as AnyRecord;
  if (Array.isArray(x.iterations) && x.iterations.length) {
    const t: Usage = { input_tokens: 0, output_tokens: 0 };
    for (const it of x.iterations as AnyRecord[]) {
      t.input_tokens += n(it.input_tokens) + n(it.cache_creation_input_tokens) + n(it.cache_read_input_tokens);
      t.output_tokens += n(it.output_tokens);
    }
    return t;
  }
  return {
    input_tokens: n(x.input_tokens) + n(x.cache_creation_input_tokens) + n(x.cache_read_input_tokens),
    output_tokens: n(x.output_tokens),
  };
}

const IMAGE_TYPES = new Set(["image/png", "image/jpeg", "image/gif", "image/webp"]);

export class AnthropicProvider implements Provider {
  readonly name = "anthropic";

  constructor(
    private readonly apiKey: string,
    private readonly baseURL: string,
    private readonly fetcher: typeof fetch = fetch,
  ) {}

  /** Builds the Messages API body for req. */
  async body(req: ChatRequest, stream: boolean): Promise<AnyRecord> {
    const messages = req.messages.map((m) => ({
      role: m.role,
      content: m.content
        .filter((p) => p.type !== "text" || (p.text ?? "") !== "")
        .map((p) => {
          if (p.type === "text") return { type: "text", text: p.text };
          if (hasInlineImage(p)) {
            const mt = (p.media_type || "image/png").toLowerCase().replace("image/jpg", "image/jpeg");
            if (!IMAGE_TYPES.has(mt)) throw new HttpError(400, "bad_request", `unsupported image type ${JSON.stringify(mt)} (png, jpeg, gif or webp)`);
            return { type: "image", source: { type: "base64", media_type: mt, data: partBase64(p) } };
          }
          return { type: "image", source: { type: "url", url: p.url } };
        }),
    }));
    const body: AnyRecord = { model: req.model, max_tokens: req.maxTokens, messages };
    const system = req.json ? systemWithJSON(req.system) : req.system;
    if (system) body.system = system;
    if (req.temperature !== undefined && acceptsSampling(req.model)) body.temperature = Math.min(Math.max(req.temperature, 0), 1);
    const effort = effortFor(req.model, req.effort ?? "");
    if (effort) body.output_config = { effort };
    if (supportsDefaultFallbacks(req.model)) body.fallbacks = "default";
    const uid = await opaqueUserID(req.user);
    if (uid) body.metadata = { user_id: uid };
    if (stream) body.stream = true;
    return body;
  }

  private async post(body: AnyRecord): Promise<Response> {
    if (!this.apiKey) throw new HttpError(501, "unsupported", "ANTHROPIC_API_KEY is not set on this Worker");
    const headers: Record<string, string> = {
      "content-type": "application/json",
      "x-api-key": this.apiKey,
      "anthropic-version": API_VERSION,
    };
    if (body.fallbacks) headers["anthropic-beta"] = FALLBACK_BETA;
    let res: Response;
    try {
      res = await this.fetcher(this.baseURL + "/v1/messages", {
        method: "POST",
        headers,
        body: JSON.stringify(body),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
    } catch (e) {
      throw upstreamError(this.name, (e as Error).message || "request failed", isTimeout(e) ? 504 : 0);
    }
    // Mapped by the provider's status like the Go server (529 overloaded → 502).
    if (!res.ok) throw await providerFailure(this.name, res);
    return res;
  }

  async chat(req: ChatRequest): Promise<ChatResponse> {
    const res = await this.post(await this.body(req, false));
    const msg = (await res.json()) as AnyRecord;
    const content = ((msg.content as AnyRecord[] | undefined) ?? [])
      .filter((b) => b.type === "text")
      .map((b) => String(b.text ?? ""))
      .join("");
    return {
      content: req.json ? stripCodeFences(content) : content,
      model: req.model,
      stop_reason: stopFrom(msg.stop_reason),
      usage: usageFrom(msg.usage),
    };
  }

  async stream(req: ChatRequest, onText: (t: string) => Promise<void> | void): Promise<ChatResponse> {
    const res = await this.post(await this.body(req, true));
    if (!res.body) throw upstreamError(this.name, "empty stream");
    let content = "";
    let stop: unknown;
    let usage: AnyRecord = {};
    await readSSE(res.body, async (ev) => {
      let j: AnyRecord;
      try {
        j = JSON.parse(ev.data) as AnyRecord;
      } catch {
        return;
      }
      switch (j.type ?? ev.event) {
        case "message_start": {
          const m = j.message as AnyRecord | undefined;
          usage = { ...((m?.usage as AnyRecord) ?? {}) };
          break;
        }
        case "content_block_delta": {
          const d = j.delta as AnyRecord | undefined;
          if (d?.type === "text_delta" && typeof d.text === "string" && d.text) {
            content += d.text;
            await onText(d.text);
          }
          break;
        }
        case "message_delta": {
          const d = j.delta as AnyRecord | undefined;
          if (d?.stop_reason) stop = d.stop_reason;
          if (j.usage && typeof j.usage === "object") usage = { ...usage, ...(j.usage as AnyRecord) };
          break;
        }
        case "error": {
          // An error inside a 200 stream carries only its type (Go's errorTypeStatus).
          const e = j.error as AnyRecord | undefined;
          const status = STREAM_ERROR_STATUS[String(e?.type ?? "")] ?? 0;
          throw upstreamError(this.name, typeof e?.message === "string" && e.message ? e.message : "stream error", status);
        }
      }
    });
    return { content: req.json ? stripCodeFences(content) : content, model: req.model, stop_reason: stopFrom(stop), usage: usageFrom(usage) };
  }

  async image(): Promise<GeneratedImage[]> {
    throw new HttpError(501, "unsupported", "anthropic (Claude) does not generate images; use an image alias");
  }

  async embed(): Promise<EmbedResponse> {
    throw new HttpError(501, "unsupported", "anthropic (Claude) has no embeddings API; use an embedding alias");
  }
}
