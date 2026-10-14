/** Provider-neutral AI types (mirrors internal/ai). */
import { base64Encode } from "../crypto";
import { HttpError } from "../http";

/** One piece of message content. */
export interface Part {
  type: "text" | "image";
  text?: string;
  /**
   * Inline image as standard padded base64: the client's own text, passed
   * on as is (every provider wants base64), so the Worker never decodes and
   * re-encodes megabytes of image within its CPU limit.
   */
  b64?: string;
  /** Inline image bytes (images loaded by the Worker itself). */
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

/** Upper bound of a provider's retry delay passed on to clients (seconds; Go's maxUpstreamRetryAfter). */
const MAX_UPSTREAM_RETRY_AFTER = 3600;

/** Upper bound of provider messages relayed to clients (UTF-8 bytes; Go's maxUpstreamMessage). */
const MAX_UPSTREAM_MESSAGE = 300;

/**
 * The retry delay providers put into rate-limit messages ("Please try again
 * in 1.5s" (OpenAI), "Please retry after 6 seconds" (Azure OpenAI)); Go's
 * retryHintRe (RE2's \s is [\t\n\f\r ]).
 */
const RETRY_HINT_RE = /\b(?:retry after|try again in)[\t\n\f\r ]+(\d{1,6}(?:\.\d{1,3})?)[\t\n\f\r ]*(ms|milliseconds?|s|secs?|seconds?|m|mins?|minutes?)?\b/i;

/** The retry delay (seconds) of a provider message, 0 if none (Go's retryHint). */
export function retryHint(msg: string): number {
  const m = RETRY_HINT_RE.exec(msg);
  if (!m) return 0;
  const v = Number(m[1]);
  if (!Number.isFinite(v)) return 0;
  const unit = (m[2] ?? "").toLowerCase();
  if (unit.startsWith("ms") || unit.startsWith("milli")) return v / 1000;
  if (unit.startsWith("m")) return v * 60;
  return v;
}

/**
 * The delay of a provider response's Retry-After header in seconds (delay
 * seconds or an HTTP date), 0 when absent or unusable.
 */
export function retryAfterHeader(res: Response, now = Date.now()): number {
  const v = (res.headers.get("Retry-After") ?? "").trim();
  if (!v) return 0;
  if (/^\d{1,9}$/.test(v)) return Number(v);
  const at = Date.parse(v);
  return Number.isFinite(at) ? Math.max(0, (at - now) / 1000) : 0;
}

/** URLs in provider messages: endpoint URLs can reveal internal hosts, Azure resource names or Vertex project ids. */
const UPSTREAM_URL_RE = /\b[a-z][a-z0-9+.-]{1,15}:\/\/[^\t\n\f\r "'<>()]+/gi;
/** Account ids and key-like tokens providers echo ("organization org-…", "sk-…****abcd", AWS key ids, bearer tokens). */
const UPSTREAM_SECRET_RE = /\b(?:(?:sk|pk|rk|org|proj|key|sess)[-_][A-Za-z0-9*_-]{6,}|A(?:KIA|SIA)[A-Z0-9]{12,}|[Bb][Ee][Aa][Rr][Ee][Rr][\t\n\f\r ]+[A-Za-z0-9._~+/=-]{8,})/g;
/** Lone UTF-16 surrogates: U+FFFD, as Go's JSON decoding of a provider body yields for them. */
const LONE_SURROGATE_RE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/g;
/** Control (Cc) and bidirectional formatting characters, shown as spaces. */
// eslint-disable-next-line no-control-regex
const CONTROL_RE = /[\u0000-\u001f\u007f-\u009f\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/g;
/** Runs of white space (Go's unicode.IsSpace, as used by strings.Fields). */
const SPACES_RE = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+/;
const TRAILING_SPACES_RE = /[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/;

/** Go's http.StatusText for the statuses providers answer with. */
const STATUS_TEXT: Record<number, string> = {
  400: "Bad Request", 401: "Unauthorized", 402: "Payment Required", 403: "Forbidden", 404: "Not Found",
  405: "Method Not Allowed", 406: "Not Acceptable", 407: "Proxy Authentication Required", 408: "Request Timeout",
  409: "Conflict", 410: "Gone", 411: "Length Required", 412: "Precondition Failed", 413: "Request Entity Too Large",
  414: "Request URI Too Long", 415: "Unsupported Media Type", 416: "Requested Range Not Satisfiable",
  417: "Expectation Failed", 418: "I'm a teapot", 421: "Misdirected Request", 422: "Unprocessable Entity", 423: "Locked",
  424: "Failed Dependency", 425: "Too Early", 426: "Upgrade Required", 428: "Precondition Required",
  429: "Too Many Requests", 431: "Request Header Fields Too Large", 451: "Unavailable For Legal Reasons",
  500: "Internal Server Error", 501: "Not Implemented", 502: "Bad Gateway", 503: "Service Unavailable",
  504: "Gateway Timeout", 505: "HTTP Version Not Supported", 506: "Variant Also Negotiates", 507: "Insufficient Storage",
  508: "Loop Detected", 510: "Not Extended", 511: "Network Authentication Required",
};

/**
 * Prepares a provider error message for clients (port of Go's
 * safeUpstreamMessage): one line of printable text without URLs, account ids
 * or key-like tokens, at most 300 UTF-8 bytes (cut at a character boundary,
 * then "…"). An empty result falls back to the lowercased HTTP status text,
 * else "request failed".
 */
export function safeUpstreamMessage(msg: string, status: number): string {
  let s = msg.replace(LONE_SURROGATE_RE, "\uFFFD").replace(CONTROL_RE, " ");
  s = s.replace(UPSTREAM_URL_RE, "[url]").replace(UPSTREAM_SECRET_RE, "[redacted]");
  s = s
    .split(SPACES_RE)
    .filter((f) => f !== "")
    .join(" ");
  const bytes = new TextEncoder().encode(s);
  if (bytes.length > MAX_UPSTREAM_MESSAGE) {
    let cut = MAX_UPSTREAM_MESSAGE;
    while (cut > 0 && (bytes[cut]! & 0xc0) === 0x80) cut--;
    // ignoreBOM keeps a leading U+FEFF, and Go's TrimSpace (unlike trim()) keeps it too.
    s = new TextDecoder("utf-8", { fatal: false, ignoreBOM: true }).decode(bytes.subarray(0, cut)).replace(TRAILING_SPACES_RE, "") + "…";
  }
  if (!s) s = (STATUS_TEXT[status] ?? "").toLowerCase();
  return s || "request failed";
}

/**
 * Maps a provider failure to an API error by the provider's HTTP status
 * (status 0: none, e.g. a network failure), like the Go server's
 * aiUpstreamError (SPEC §5.4):
 *
 *   - 504 (a timeout) and 408 → 504 upstream_error;
 *   - 429 (the provider throttles this deployment's account, which all users
 *     share) → 429 rate_limited with Retry-After when the provider said when
 *     to retry (retryAfter seconds, e.g. its Retry-After header, else a hint
 *     in its message; at most an hour), else 503 upstream_error: a 429
 *     without a retry time would read as the caller's own rate limit;
 *   - 400 and 422 (the provider rejected the request) → 400 bad_request, and
 *     413 → 413 too_large;
 *   - anything else (bad credentials, outages, malformed responses) → 502.
 *
 * Provider messages are sanitized (safeUpstreamMessage); the raw failure of
 * a 5xx is logged with the provider name.
 */
export function upstreamError(provider: string, message: string, status = 0, retryAfter = 0): HttpError {
  let e: HttpError;
  switch (status) {
    case 504:
    case 408:
      e = new HttpError(504, "upstream_error", "the AI provider did not respond in time");
      break;
    case 429: {
      const after = Math.min(retryAfter > 0 ? retryAfter : retryHint(message), MAX_UPSTREAM_RETRY_AFTER);
      e =
        after > 0
          ? new HttpError(429, "rate_limited", "the AI provider is rate limiting requests; retry later", { retryAfter: after })
          : new HttpError(503, "upstream_error", "the AI provider is rate limiting requests; retry later");
      break;
    }
    case 400:
    case 422:
      e = new HttpError(400, "bad_request", `the AI provider rejected the request: ${safeUpstreamMessage(message, status)}`);
      break;
    case 413:
      e = new HttpError(413, "too_large", `the AI provider rejected the request as too large: ${safeUpstreamMessage(message, status)}`);
      break;
    default:
      e = new HttpError(502, "upstream_error", `AI provider error: ${safeUpstreamMessage(message, status)}`);
  }
  if (e.status >= 500) console.warn(`backyard: AI provider ${provider} failed (HTTP ${status || "-"}): ${message.slice(0, 1000)}`);
  return e;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/**
 * A human-readable message from a provider's error body (port of Go's
 * httpx.ErrorMessage): {"error": {"message"}} or {"error": "…"} (also as the
 * first element of an array), then {"message"}, {"detail": "…"} and
 * Cloudflare's {"errors": [{"message"}]}; otherwise the body text itself.
 */
export function errorBodyMessage(body: string): string {
  const t = body.trim();
  let v: unknown;
  try {
    v = JSON.parse(t);
  } catch {
    return t;
  }
  const first = Array.isArray(v) && v.length > 0 ? v[0] : v;
  if (isRecord(first)) {
    const e = first.error;
    if (isRecord(e) && typeof e.message === "string" && e.message) return e.message;
    if (typeof e === "string" && e) return e;
  }
  if (isRecord(v)) {
    if (typeof v.message === "string" && v.message) return v.message;
    if (typeof v.detail === "string" && v.detail) return v.detail;
    if (Array.isArray(v.errors)) {
      const msgs = v.errors.map((x) => (isRecord(x) && typeof x.message === "string" ? x.message : "")).filter(Boolean);
      if (msgs.length) return msgs.join("; ");
    }
  }
  return t;
}

/** The API error for a provider's non-2xx response (its message, status and Retry-After). */
export async function providerFailure(provider: string, res: Response): Promise<HttpError> {
  let text = "";
  try {
    text = await res.text();
  } catch {
    /* keep the status */
  }
  return upstreamError(provider, errorBodyMessage(text.slice(0, 64 << 10)), res.status, retryAfterHeader(res));
}

/** Whether e is the abort of a fetch whose AbortSignal.timeout expired. */
export function isTimeout(e: unknown): boolean {
  return (e as { name?: unknown } | null)?.name === "TimeoutError";
}

/**
 * Converts a provider call's failure into an API error like Go's aiError:
 * HttpErrors pass through, a request timeout is 504 ("did not respond in
 * time"), anything else (a network failure, a malformed body) is 502 "AI
 * request failed" with the cause logged.
 */
export function aiFailure(e: unknown, provider = "ai"): HttpError {
  if (e instanceof HttpError) return e;
  if (isTimeout(e)) return upstreamError(provider, (e as Error).message || "timeout", 504);
  console.warn(`backyard: AI request failed (${provider}): ${(e as Error)?.message ?? String(e)}`);
  return new HttpError(502, "upstream_error", "AI request failed");
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
  return `data:${mediaType};base64,${base64Encode(data)}`;
}

/** Standard base64 of an inline image part ("" when it has none). */
export function partBase64(p: Part): string {
  if (p.b64) return p.b64;
  return p.data ? base64Encode(p.data) : "";
}

/** Whether a part carries an inline image (base64 or bytes). */
export function hasInlineImage(p: Part): boolean {
  return !!p.b64 || !!p.data;
}

/** The URL a provider gets for an image part: a data: URL for inline images, else the remote URL. */
export function imageURL(p: Part): string {
  if (hasInlineImage(p)) return `data:${p.media_type || "image/png"};base64,${partBase64(p)}`;
  return p.url ?? "";
}
