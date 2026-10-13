/** Provider-neutral AI types (mirrors internal/ai). */
import { HttpError } from "../http";

/** One piece of message content. */
export interface Part {
  type: "text" | "image";
  text?: string;
  /** Image bytes (decoded from base64 "data"). */
  data?: Uint8Array;
  media_type?: string;
  /** Remote image URL. */
  url?: string;
}

export interface Message {
  role: "user" | "assistant";
  content: Part[];
}

/** A provider-level chat request (model is the provider model id). */
export interface ChatRequest {
  model: string;
  system: string;
  messages: Message[];
  maxTokens: number;
  temperature?: number;
  json: boolean;
  effort?: string;
  /** Opaque end-user id for provider abuse monitoring. */
  user: string;
}

export interface Usage {
  input_tokens: number;
  output_tokens: number;
}

/** Normalized stop reasons (SPEC §5.4). */
export type StopReason = "end_turn" | "max_tokens" | "stop_sequence" | "refusal" | "other";

export interface ChatResponse {
  content: string;
  model: string;
  stop_reason: StopReason;
  usage: Usage;
}

export interface ImageRequest {
  model: string;
  prompt: string;
  size: string;
  n: number;
  user: string;
}

export interface GeneratedImage {
  data: Uint8Array;
  media_type: string;
}

export interface EmbedRequest {
  model: string;
  input: string[];
  user: string;
}

export interface EmbedResponse {
  embeddings: number[][];
  usage: Usage;
}

/** An upstream AI API. */
export interface Provider {
  readonly name: string;
  chat(req: ChatRequest): Promise<ChatResponse>;
  /** Streams text deltas to onText and returns the final response. */
  stream(req: ChatRequest, onText: (text: string) => Promise<void> | void): Promise<ChatResponse>;
  image(req: ImageRequest): Promise<GeneratedImage[]>;
  embed(req: EmbedRequest): Promise<EmbedResponse>;
}

/** System instruction appended for JSON mode (internal/ai.JSONInstruction). */
export const JSON_INSTRUCTION =
  "Respond with a single valid JSON object and nothing else: no Markdown code fences, no commentary before or after it.";

export function systemWithJSON(system: string): string {
  const s = system.trim();
  return s ? s + "\n\n" + JSON_INSTRUCTION : JSON_INSTRUCTION;
}

function validJSON(s: string): boolean {
  try {
    JSON.parse(s);
    return true;
  } catch {
    return false;
  }
}

function jsonSpan(s: string): string | null {
  for (const [open, close] of [
    ["{", "}"],
    ["[", "]"],
  ] as const) {
    const start = s.indexOf(open);
    const end = s.lastIndexOf(close);
    if (start >= 0 && end > start) {
      const c = s.slice(start, end + 1);
      if (validJSON(c)) return c;
    }
  }
  return null;
}

/** Extracts a JSON document from a model answer (port of ai.StripCodeFences). */
export function stripCodeFences(s: string): string {
  const t = s.trim();
  if (t === "" || validJSON(t)) return t;
  const i = t.indexOf("```");
  if (i >= 0) {
    let rest = t.slice(i + 3);
    const nl = rest.indexOf("\n");
    rest = nl >= 0 ? rest.slice(nl + 1) : rest.replace(/^[A-Za-z]+/, "");
    const j = rest.indexOf("```");
    if (j >= 0) rest = rest.slice(0, j);
    const inner = rest.trim();
    if (validJSON(inner)) return inner;
    return jsonSpan(inner) ?? inner;
  }
  return jsonSpan(t) ?? t;
}

/** An error returned by a provider API (502 upstream_error). */
export function upstreamError(provider: string, message: string, status = 502): HttpError {
  const s = status === 429 ? 429 : status === 504 ? 504 : 502;
  const code = s === 429 ? "rate_limited" : "upstream_error";
  return new HttpError(s, code, `AI provider error: ${provider}: ${message.slice(0, 500)}`);
}

/** Concatenated text of message parts. */
export function textOf(parts: Part[]): string {
  return parts
    .filter((p) => p.type === "text")
    .map((p) => p.text ?? "")
    .join("");
}

/** Opaque, stable end-user id (internal/ai.OpaqueUserID). */
export async function opaqueUserID(user: string): Promise<string> {
  if (!user) return "";
  const sum = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode("backyard/ai-user\u0000" + user)));
  return "u_" + [...sum.subarray(0, 16)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

/** Base64 data URL of image bytes. */
export function dataURL(data: Uint8Array, mediaType: string): string {
  let s = "";
  for (let i = 0; i < data.length; i += 0x8000) s += String.fromCharCode(...data.subarray(i, i + 0x8000));
  return `data:${mediaType};base64,${btoa(s)}`;
}
