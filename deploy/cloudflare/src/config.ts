/**
 * Configuration of the Worker, parsed from wrangler vars and secrets with the
 * defaults of the Cloudflare edition (SPEC §16). Parsing is cached per env
 * object, so it costs nothing per request.
 */
import type { Env } from "./env";

/** Site names that can never be deployed (SPEC §9 defaults). */
export const DEFAULT_RESERVED = ["www", "api", "admin", "hub", "auth", "static", "assets", "cdn", "docs", "mail", "backyard", "s"];

/** Hard per-file cap without R2: files are chunked into Durable Object SQLite. */
export const MAX_INLINE_FILE = 25 * 1024 * 1024;
/** Hard per-file cap with R2: the request body limit of the free plan (100 MB) minus overhead. */
export const MAX_R2_FILE = 95 * 1024 * 1024;

/** Guardrails and limits (LIMITS JSON var, merged over these defaults). */
export interface Limits {
  /** Platform-wide number of sites. */
  max_sites: number;
  /** Total bytes of one site's files. */
  max_site_bytes: number;
  /** Files per site. */
  max_files: number;
  /** Bytes per deployed file (capped at 25 MiB without R2, 95 MiB with). */
  max_file_bytes: number;
  /** Bytes per uploaded file (POST /files; same caps). */
  max_upload_bytes: number;
  /** JSON bytes per document. */
  max_doc_bytes: number;
  /** Documents per site. */
  max_docs_per_site: number;
  /** Maximum query limit. */
  max_list: number;
  /** AI calls per user (or IP when anonymous) per UTC day. 0 = unlimited. */
  ai_per_day: number;
  /** Generated images per user/IP per day. */
  ai_images_per_day: number;
  /** AI calls per day across the whole platform (the drain guard). */
  ai_platform_per_day: number;
  /** Generated images per day across the platform. */
  ai_images_platform_per_day: number;
  /** Cap on max_tokens of a chat request. */
  ai_max_tokens: number;
  /** Deploys (created) per user/IP per day. */
  deploys_per_day: number;
  /** File uploads (POST /files) per user/IP per day. */
  uploads_per_day: number;
  /** Database writes per user/IP per site per day (counted in memory by the site). */
  db_writes_per_day: number;
  /** Websocket limits (SPEC §6). */
  ws_conns_per_user: number;
  ws_rooms_per_conn: number;
  ws_subs_per_conn: number;
  ws_messages_per_second: number;
  ws_message_burst: number;
  ws_max_message_bytes: number;
  ws_max_presence_bytes: number;
  /** Decoded file bytes of one backyard_publish call of the Claude connector (SPEC §14.1). */
  mcp_publish_bytes: number;
  /** Connector tool calls (tools/call) per user per UTC day. 0 = unlimited. */
  mcp_calls_per_day: number;
}

export const DEFAULT_LIMITS: Limits = {
  max_sites: 500,
  max_site_bytes: 200 * 1024 * 1024,
  max_files: 2000,
  max_file_bytes: MAX_R2_FILE,
  max_upload_bytes: MAX_R2_FILE,
  max_doc_bytes: 256 * 1024,
  max_docs_per_site: 100_000,
  max_list: 1000,
  ai_per_day: 200,
  ai_images_per_day: 30,
  ai_platform_per_day: 2000,
  ai_images_platform_per_day: 300,
  ai_max_tokens: 4096,
  deploys_per_day: 200,
  uploads_per_day: 500,
  db_writes_per_day: 20_000,
  ws_conns_per_user: 50,
  ws_rooms_per_conn: 50,
  ws_subs_per_conn: 100,
  ws_messages_per_second: 60,
  ws_message_burst: 120,
  ws_max_message_bytes: 64 * 1024,
  ws_max_presence_bytes: 8 * 1024,
  mcp_publish_bytes: 2 * 1024 * 1024,
  mcp_calls_per_day: 5000,
};

/** The client metadata documents of Claude (claude.ai, Desktop, mobile and Cowork; Claude Code) and ChatGPT: the default MCP_CLIENTS. */
export const DEFAULT_MCP_CLIENTS = [
  "https://claude.ai/oauth/mcp-oauth-client-metadata",
  "https://claude.ai/oauth/claude-code-client-metadata",
  "https://chatgpt.com/oauth/client.json",
];

/** The OAuth callbacks of Claude and ChatGPT: the default MCP_REDIRECT_URIS. */
export const DEFAULT_MCP_REDIRECT_URIS = [
  "https://claude.ai/api/mcp/auth_callback",
  "https://claude.com/api/mcp/auth_callback",
  "https://chatgpt.com/connector_platform_oauth_redirect",
];

/** The Claude connector (remote MCP, SPEC §8.4, §14.1, §16). */
export interface MCPConfig {
  /**
   * MCP_ENABLED and valid MCP variables. Requests also need an https (or
   * loopback) hub origin: see mcpEnabled in oauth.ts.
   */
  enabled: boolean;
  /**
   * Effective MCP_CLIENTS: allowlisted Client ID Metadata Document URLs
   * (exact), the only ones fetched and the only URL client ids that may use
   * an https redirect URI (SPEC §8.4.3).
   */
  clients: string[];
  /** Effective MCP_REDIRECT_URIS: exact https redirect URIs (loopback ones are always allowed). */
  redirectURIs: string[];
  /**
   * Problems of the MCP variables: each one turns the connector off without
   * failing the Worker closed; /__backyard/readyz lists them.
   */
  problems: string[];
}

/** An AI model alias (AI_MODELS JSON var). */
export interface ModelConfig {
  provider: "workers-ai" | "anthropic" | "openai";
  model: string;
  kind: "chat" | "image" | "embedding";
  description?: string;
  /** Per-alias cap on max_tokens. */
  max_tokens?: number;
  /** Anthropic: output_config.effort (low|medium|high|xhigh|max). */
  effort?: string;
}

/** Default aliases: free Workers AI models (SPEC §16). */
export const DEFAULT_MODELS: Record<string, ModelConfig> = {
  smart: {
    provider: "workers-ai",
    model: "@cf/google/gemma-4-26b-a4b-it",
    kind: "chat",
    description: "Balanced default chat model (Gemma 4 26B on Workers AI)",
  },
  fast: {
    provider: "workers-ai",
    model: "@cf/meta/llama-3.1-8b-instruct-fp8",
    kind: "chat",
    description: "Small, fast chat model (Llama 3.1 8B on Workers AI)",
  },
  image: {
    provider: "workers-ai",
    model: "@cf/black-forest-labs/flux-1-schnell",
    kind: "image",
    description: "Image generation (FLUX.1 schnell on Workers AI)",
  },
  embed: {
    provider: "workers-ai",
    model: "@cf/baai/bge-base-en-v1.5",
    kind: "embedding",
    description: "Text embeddings, 768 dimensions (BGE base on Workers AI)",
  },
};

export interface AIConfig {
  /**
   * AI_ENABLED (default true). False: every AI endpoint answers 501
   * unsupported before anything else (Go's ai.enabled: false), and no
   * provider or binding is used.
   */
  enabled: boolean;
  models: Record<string, ModelConfig>;
  defaultChat: string;
  defaultImage: string;
  defaultEmbedding: string;
  allowRawModels: boolean;
  exposeModelIDs: boolean;
  systemPrompt: string;
  gatewayID: string;
  anthropicBaseURL: string;
  openaiBaseURL: string;
  /** Problems found while parsing AI_MODELS (logged once). */
  warnings: string[];
}

export interface Config {
  urlMode: "path" | "host";
  /** Lowercased hub host without port ("" = the request host). */
  baseDomain: string;
  /** Configured public URL without trailing slash ("" = derived per request). */
  publicURL: string;
  authMode: "none" | "access";
  /** Access team host, e.g. "acme.cloudflareaccess.com" ("" when unset). */
  accessTeam: string;
  accessAud: string[];
  accessGetIdentity: boolean;
  allowAnonymousDeploy: boolean;
  admins: string[];
  adminGroups: string[];
  ownership: "none" | "owner";
  /**
   * SITE_LISTING (SPEC §7.2 "Listing"): "all" lists every site; "owners" lists
   * a site only to whom listedTo (sitemeta.ts) admits, and implies ownership
   * "owner".
   */
  siteListing: "all" | "owners";
  serverName: string;
  reserved: string[];
  limits: Limits;
  fileInlineMax: number;
  tokenTTLSeconds: number;
  secret: string;
  deployToken: string;
  warehouseURL: string;
  ai: AIConfig;
  mcp: MCPConfig;
  /**
   * A misconfiguration that makes the Worker answer every request except the
   * health checks with 503 (fail closed), "" when there is none: an
   * AUTH_MODE other than "none" or "access" (serving without authentication
   * instead would make public a Worker meant to sit behind Cloudflare Access;
   * workers.dev is not behind Access), a SITE_LISTING other than "all" or
   * "owners", or SITE_LISTING=owners with an OWNERSHIP other than "owner"
   * (listing every site instead would show what was meant to be hidden). The
   * Go server refuses to start then. Several are joined with "; ".
   */
  fatal: string;
  /** Configuration problems (logged once per isolate: npx wrangler tail). */
  problems: string[];
}

const cache = new WeakMap<object, Config>();

/** Returns the parsed configuration for env (cached per env object). */
export function getConfig(env: Env): Config {
  let c = cache.get(env);
  if (!c) {
    c = parseConfig(env);
    cache.set(env, c);
    for (const p of c.problems) console.warn(`backyard: config: ${p}`);
  }
  return c;
}

function str(v: unknown): string {
  return typeof v === "string" ? v.trim() : "";
}

/** The variables of Env (bindings and secrets are not variables). */
const VAR_KEYS = [
  "BASE_DOMAIN",
  "PUBLIC_URL",
  "URL_MODE",
  "AUTH_MODE",
  "ACCESS_TEAM_DOMAIN",
  "ACCESS_AUD",
  "ACCESS_GET_IDENTITY",
  "ALLOW_ANONYMOUS_DEPLOY",
  "ADMINS",
  "ADMIN_GROUPS",
  "OWNERSHIP",
  "SITE_LISTING",
  "SERVER_NAME",
  "RESERVED_SITES",
  "AI_ENABLED",
  "AI_MODELS",
  "AI_DEFAULT_MODEL",
  "AI_DEFAULT_IMAGE_MODEL",
  "AI_DEFAULT_EMBEDDING_MODEL",
  "AI_MAX_TOKENS",
  "AI_ALLOW_RAW_MODELS",
  "AI_EXPOSE_MODEL_IDS",
  "AI_SYSTEM_PROMPT",
  "AI_GATEWAY_ID",
  "ANTHROPIC_BASE_URL",
  "OPENAI_BASE_URL",
  "LIMITS",
  "FILE_INLINE_MAX",
  "TOKEN_TTL_HOURS",
  "WAREHOUSE_URL",
  "MCP_ENABLED",
  "MCP_CLIENTS",
  "MCP_REDIRECT_URIS",
] as const satisfies readonly (keyof Env)[];

/** Variables that hold a JSON object: wrangler and the dashboard may deliver them parsed. */
const JSON_VARS = new Set<string>(["LIMITS", "AI_MODELS"]);

/** List variables that may also be given as a JSON array of strings. */
const LIST_VARS = new Set<string>(["MCP_CLIENTS", "MCP_REDIRECT_URIS"]);

/**
 * The variables of env as strings. Wrangler turns a variable written as a
 * JSON value in wrangler.jsonc (or a dashboard variable of type JSON) into a
 * parsed value: numbers and booleans are taken as their text, objects in
 * LIMITS and AI_MODELS as their JSON, arrays of strings in MCP_CLIENTS and
 * MCP_REDIRECT_URIS as a comma-separated list. Any other value is reported
 * and ignored (in AUTH_MODE and SITE_LISTING, and in OWNERSHIP alongside
 * SITE_LISTING=owners, it makes the configuration fatal; in the MCP lists it
 * turns the Claude connector off).
 */
function stringVars(env: Env, problems: string[], invalid: Set<string>): Env {
  const out: Record<string, unknown> = { ...env };
  for (const k of VAR_KEYS) {
    const v = (env as unknown as Record<string, unknown>)[k];
    if (v === undefined || v === null || typeof v === "string") continue;
    if (typeof v === "number" || typeof v === "boolean") {
      out[k] = String(v);
    } else if (JSON_VARS.has(k) && typeof v === "object" && !Array.isArray(v)) {
      out[k] = JSON.stringify(v);
    } else if (LIST_VARS.has(k) && Array.isArray(v) && v.every((x) => typeof x === "string")) {
      out[k] = v.join(",");
    } else {
      const also = JSON_VARS.has(k) ? " or a JSON object" : LIST_VARS.has(k) ? " or an array of strings" : "";
      problems.push(`${k} must be a string${also} (got ${Array.isArray(v) ? "an array" : `a ${typeof v}`}); ignoring it`);
      invalid.add(k);
      delete out[k];
    }
  }
  return out as unknown as Env;
}

function bool(v: unknown, def = false): boolean {
  const s = str(v).toLowerCase();
  if (s === "") return def;
  return s === "true" || s === "1" || s === "yes" || s === "on";
}

function list(v: unknown): string[] {
  return str(v)
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);
}

function int(v: unknown, def: number): number {
  const s = str(v);
  if (s === "") return def;
  const n = Number(s);
  return Number.isFinite(n) && n >= 0 ? Math.floor(n) : def;
}

/** Normalizes ACCESS_TEAM_DOMAIN ("acme", "acme.cloudflareaccess.com", "https://acme.cloudflareaccess.com/"). */
export function normalizeTeam(v: string): string {
  let t = v.trim().toLowerCase();
  t = t.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
  if (t === "") return "";
  if (!t.includes(".")) t += ".cloudflareaccess.com";
  return t;
}

/** Parses env into a Config (no caching). Exported for tests. */
export function parseConfig(rawEnv: Env): Config {
  const problems: string[] = [];
  const invalid = new Set<string>();
  const env = stringVars(rawEnv, problems, invalid);
  const urlMode = str(env.URL_MODE).toLowerCase() === "host" ? "host" : "path";
  if (str(env.URL_MODE) && !["host", "path"].includes(str(env.URL_MODE).toLowerCase())) {
    problems.push(`URL_MODE must be "path" or "host" (got ${JSON.stringify(env.URL_MODE)}); using path`);
  }
  const baseDomain = str(env.BASE_DOMAIN).toLowerCase().replace(/^https?:\/\//, "").replace(/[:/].*$/, "").replace(/\.$/, "");
  if (urlMode === "host" && !baseDomain) problems.push("URL_MODE=host needs BASE_DOMAIN; sites are served in path mode until it is set");
  // Like the Go server's public URL: scheme://host[:port], lowercased (an
  // explicit port is kept), so links, the SDK's hub and /docs read the same.
  let publicURL = str(env.PUBLIC_URL).replace(/\/+$/, "").toLowerCase();
  if (publicURL && !/^https?:\/\/[^/?#@\s]+$/.test(publicURL)) {
    problems.push(`PUBLIC_URL must be an absolute origin like https://backyard.example.com (got ${JSON.stringify(str(env.PUBLIC_URL))}); ignoring it`);
    publicURL = "";
  }
  const am = str(env.AUTH_MODE).toLowerCase();
  const authMode = am === "access" ? "access" : "none";
  // Fail closed: a mistyped or Go-style mode ("oidc", "okta", "cloudflare-access")
  // was meant to require sign-in, so it never silently means "none".
  let fatal = "";
  if (invalid.has("AUTH_MODE") || (am && am !== "access" && am !== "none")) {
    fatal = 'AUTH_MODE must be "none" or "access"';
    problems.push(
      `${fatal} (got ${invalid.has("AUTH_MODE") ? "a value that is not a string" : JSON.stringify(am.slice(0, 64))}); ${FAIL_CLOSED}`,
    );
  }
  const accessTeam = normalizeTeam(str(env.ACCESS_TEAM_DOMAIN));
  const accessAud = list(env.ACCESS_AUD);
  if (authMode === "access" && (!accessTeam || accessAud.length === 0)) {
    problems.push("AUTH_MODE=access needs ACCESS_TEAM_DOMAIN and ACCESS_AUD; browser requests will be rejected");
  }

  const limits: Limits = { ...DEFAULT_LIMITS };
  const rawLimits = str(env.LIMITS);
  if (rawLimits) {
    try {
      const parsed = JSON.parse(rawLimits) as unknown;
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("not an object");
      for (const [k, v] of Object.entries(parsed as Record<string, unknown>)) {
        if (!Object.hasOwn(DEFAULT_LIMITS, k)) {
          problems.push(`LIMITS: unknown key ${JSON.stringify(k)}`);
          continue;
        }
        if (typeof v !== "number" || !Number.isFinite(v) || v < 0) {
          problems.push(`LIMITS.${k} must be a non-negative number`);
          continue;
        }
        (limits as unknown as Record<string, number>)[k] = Math.floor(v);
      }
    } catch (e) {
      problems.push(`LIMITS is not valid JSON (${(e as Error).message}); using defaults`);
    }
  }

  const secret = typeof env.BACKYARD_SECRET === "string" ? env.BACKYARD_SECRET : "";
  if (!secret) problems.push("BACKYARD_SECRET is not set (re-run setup: backyard cloudflare setup or npx backyard-cloudflare setup; or npx wrangler secret put BACKYARD_SECRET, at least 32 characters)");
  else if (secret.length < 32) problems.push("BACKYARD_SECRET must be at least 32 characters");

  const { siteListing, ownership } = parseListing(env, invalid, problems, (f) => (fatal = fatal ? `${fatal}; ${f}` : f));
  const reserved = [...DEFAULT_RESERVED, ...list(env.RESERVED_SITES).map((s) => s.toLowerCase())];

  return {
    urlMode: urlMode === "host" && baseDomain ? "host" : "path",
    baseDomain,
    publicURL,
    authMode,
    accessTeam,
    accessAud,
    accessGetIdentity: bool(env.ACCESS_GET_IDENTITY, true),
    allowAnonymousDeploy: bool(env.ALLOW_ANONYMOUS_DEPLOY),
    admins: list(env.ADMINS).map((s) => s.toLowerCase()),
    adminGroups: list(env.ADMIN_GROUPS).map((s) => s.toLowerCase()),
    ownership,
    siteListing,
    serverName: str(env.SERVER_NAME) || "Backyard",
    reserved,
    limits,
    fileInlineMax: Math.min(int(env.FILE_INLINE_MAX, 5 * 1024 * 1024), MAX_INLINE_FILE),
    tokenTTLSeconds: Math.max(60, int(env.TOKEN_TTL_HOURS, 720) * 3600),
    secret: secret.length >= 32 ? secret : "",
    deployToken: typeof env.DEPLOY_TOKEN === "string" ? env.DEPLOY_TOKEN.trim() : "",
    warehouseURL: str(env.WAREHOUSE_URL).replace(/\/+$/, ""),
    ai: parseAI(env, problems),
    mcp: parseMCP(env, invalid, baseDomain, publicURL, problems),
    fatal,
    problems,
  };
}

const PROVIDERS = new Set(["workers-ai", "anthropic", "openai"]);
const KINDS = new Set(["chat", "image", "embedding"]);
const ALIAS_RE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;

function parseAI(env: Env, problems: string[]): AIConfig {
  const warnings: string[] = [];
  let models: Record<string, ModelConfig> = { ...DEFAULT_MODELS };
  const raw = str(env.AI_MODELS);
  if (raw) {
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("not an object");
      models = {};
      for (const [alias, v] of Object.entries(parsed as Record<string, unknown>)) {
        const m = v as Partial<ModelConfig> | null;
        if (!ALIAS_RE.test(alias)) {
          warnings.push(`AI_MODELS: invalid alias ${JSON.stringify(alias)}`);
          continue;
        }
        if (!m || typeof m !== "object" || typeof m.model !== "string" || !m.model) {
          warnings.push(`AI_MODELS.${alias}: "model" is required`);
          continue;
        }
        const provider = (m.provider ?? "workers-ai") as ModelConfig["provider"];
        const kind = (m.kind ?? "chat") as ModelConfig["kind"];
        if (!PROVIDERS.has(provider)) {
          warnings.push(`AI_MODELS.${alias}: provider must be workers-ai, anthropic or openai`);
          continue;
        }
        if (!KINDS.has(kind)) {
          warnings.push(`AI_MODELS.${alias}: kind must be chat, image or embedding`);
          continue;
        }
        const mc: ModelConfig = { provider, model: m.model, kind };
        if (typeof m.description === "string") mc.description = m.description;
        if (typeof m.max_tokens === "number" && m.max_tokens > 0) mc.max_tokens = Math.floor(m.max_tokens);
        if (typeof m.effort === "string" && m.effort) mc.effort = m.effort;
        models[alias] = mc;
      }
    } catch (e) {
      warnings.push(`AI_MODELS is not valid JSON (${(e as Error).message}); using the default models`);
      models = { ...DEFAULT_MODELS };
    }
  }
  const pick = (explicit: string, kind: ModelConfig["kind"], prefs: string[]): string => {
    if (explicit) {
      if (Object.hasOwn(models, explicit) && models[explicit]!.kind === kind) return explicit;
      warnings.push(`default ${kind} model ${JSON.stringify(explicit)} is not a ${kind} alias in AI_MODELS`);
    }
    for (const p of prefs) if (models[p]?.kind === kind) return p;
    return Object.keys(models).sort().find((k) => models[k]?.kind === kind) ?? "";
  };
  problems.push(...warnings);
  return {
    enabled: bool(env.AI_ENABLED, true),
    models,
    defaultChat: pick(str(env.AI_DEFAULT_MODEL), "chat", ["default", "smart", "chat", "fast"]),
    defaultImage: pick(str(env.AI_DEFAULT_IMAGE_MODEL), "image", ["image", "images"]),
    defaultEmbedding: pick(str(env.AI_DEFAULT_EMBEDDING_MODEL), "embedding", ["embed", "embedding", "embeddings"]),
    allowRawModels: bool(env.AI_ALLOW_RAW_MODELS),
    exposeModelIDs: bool(env.AI_EXPOSE_MODEL_IDS),
    systemPrompt: str(env.AI_SYSTEM_PROMPT),
    gatewayID: str(env.AI_GATEWAY_ID),
    anthropicBaseURL: (str(env.ANTHROPIC_BASE_URL) || "https://api.anthropic.com").replace(/\/+$/, ""),
    openaiBaseURL: (str(env.OPENAI_BASE_URL) || "https://api.openai.com/v1").replace(/\/+$/, ""),
    warnings,
  };
}

// --- Claude connector (SPEC §8.4, §12 entry rules, §16) ----------------------

/** Longest entry of MCP_CLIENTS and MCP_REDIRECT_URIS (bytes). */
const MAX_MCP_URL_BYTES = 512;

/** A comma or whitespace separated list. */
function urlList(v: unknown): string[] {
  return str(v)
    .split(/[\s,]+/)
    .filter(Boolean);
}

/** UTF-8 length of s. */
function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

/**
 * Splits an absolute "scheme://authority/path…" URL as written (WHATWG URL
 * would normalize dot segments and case away); null when it is none.
 */
function splitURL(e: string): { scheme: string; authority: string; rest: string } | null {
  const m = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)(.*)$/s.exec(e);
  if (!m) return null;
  try {
    new URL(e);
  } catch {
    return null;
  }
  return { scheme: m[1]!.toLowerCase(), authority: m[2]!, rest: m[3]! };
}

/** The host of a URL authority (lowercase, no port, no trailing dot; IPv6 in brackets). */
function authorityHost(authority: string): string {
  const at = authority.lastIndexOf("@");
  let h = at >= 0 ? authority.slice(at + 1) : authority;
  if (h.startsWith("[")) h = h.slice(0, h.indexOf("]") + 1);
  else if (h.includes(":")) h = h.slice(0, h.indexOf(":"));
  return h.toLowerCase().replace(/\.$/, "");
}

/** Whether h is an IP literal (dotted IPv4 or bracketed IPv6). */
function isIPLiteral(h: string): boolean {
  return h.startsWith("[") || /^\d{1,3}(\.\d{1,3}){3}$/.test(h);
}

/**
 * Whether hostname h (lowercase, without port; IPv6 with or without
 * brackets) is a loopback host: localhost, *.localhost, 127.0.0.0/8 or ::1.
 */
export function isLoopbackHost(h: string): boolean {
  h = h.toLowerCase().replace(/\.$/, "");
  if (h === "localhost" || h.endsWith(".localhost")) return true;
  if (h === "[::1]" || h === "::1") return true;
  const m = /^127\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(h);
  return !!m && m.slice(1).every((x) => Number(x) <= 255);
}

/**
 * Whether e is a client metadata URL (rule 1 of SPEC §12): an https URL whose
 * host is a name (not an IP literal), with a path other than "/" and without
 * dot segments, query, fragment or user info, at most 512 bytes. It decides
 * what MCP_CLIENTS may list; an https client_id outside the list must pass it
 * too (oauth.ts unlistedClientURL adds a stricter form, SPEC §8.4.3).
 */
export function validMCPClientURL(e: string): boolean {
  // eslint-disable-next-line no-control-regex
  if (!e || byteLength(e) > MAX_MCP_URL_BYTES || /[?#\\\s\u0000-\u001f\u007f]/.test(e)) return false;
  const u = splitURL(e);
  if (!u || u.scheme !== "https" || u.authority.includes("@")) return false;
  const host = authorityHost(u.authority);
  if (!host || isIPLiteral(host)) return false;
  if (u.rest === "" || u.rest === "/" || !u.rest.startsWith("/")) return false;
  // Dot segments, also percent-encoded ("%2e%2e", as Go decodes the path).
  return !u.rest.split("/").some((seg) => {
    const d = seg.replace(/%2e/gi, ".");
    return d === "." || d === "..";
  });
}

/**
 * The host of an MCP_REDIRECT_URIS entry when it is valid (rule 2 of SPEC
 * §12: an https URL with a host and without fragment or user info, at most
 * 512 bytes), else "".
 */
export function mcpRedirectURIHost(e: string): string {
  // eslint-disable-next-line no-control-regex
  if (!e || byteLength(e) > MAX_MCP_URL_BYTES || /[#\\\s\u0000-\u001f\u007f]/.test(e)) return "";
  const u = splitURL(e);
  if (!u || u.scheme !== "https" || u.authority.includes("@")) return "";
  return authorityHost(u.authority);
}

/** How a fatal problem is reported: the problem line names the remedy. */
const FAIL_CLOSED = "every request but the health checks is refused with 503 until it is fixed";

/**
 * Parses SITE_LISTING and OWNERSHIP (SPEC §7.2 "Listing", §16), like the Go
 * server's sites.listing and sites.ownership. SITE_LISTING (trimmed,
 * case-insensitive) unset, empty or "all" lists every site, and OWNERSHIP
 * "owner" means ownership owner, anything else none (as before the option).
 * "owners" implies ownership: OWNERSHIP unset, empty or "owner" means owner.
 * Fails closed (fail) for any other SITE_LISTING, and for any other
 * OWNERSHIP alongside "owners": listing every site, or owners-only listing
 * without recorded owners, is not what was asked for.
 */
function parseListing(
  env: Env,
  invalid: Set<string>,
  problems: string[],
  fail: (fatal: string) => void,
): { siteListing: "all" | "owners"; ownership: "none" | "owner" } {
  const sl = str(env.SITE_LISTING).toLowerCase();
  const own = str(env.OWNERSHIP).toLowerCase();
  if (invalid.has("SITE_LISTING") || (sl !== "" && sl !== "all" && sl !== "owners")) {
    const f = 'SITE_LISTING must be "all" or "owners"';
    fail(f);
    problems.push(`${f} (got ${invalid.has("SITE_LISTING") ? "a value that is not a string" : JSON.stringify(sl.slice(0, 64))}); ${FAIL_CLOSED}`);
    // Never all (the configuration is fatal anyway).
    return { siteListing: "owners", ownership: "owner" };
  }
  if (sl !== "owners") return { siteListing: "all", ownership: own === "owner" ? "owner" : "none" };
  if (invalid.has("OWNERSHIP") || (own !== "" && own !== "owner")) {
    const f = "SITE_LISTING=owners requires OWNERSHIP=owner (its default with this option)";
    fail(f);
    const got = invalid.has("OWNERSHIP") ? "a value that is not a string" : JSON.stringify(own.slice(0, 64));
    problems.push(`${f} (got OWNERSHIP=${got}; leave OWNERSHIP unset or set it to owner); ${FAIL_CLOSED}`);
  }
  return { siteListing: "owners", ownership: "owner" };
}

/** Parses MCP_ENABLED, MCP_CLIENTS and MCP_REDIRECT_URIS (SPEC §16). */
function parseMCP(env: Env, invalid: Set<string>, baseDomain: string, publicURL: string, problems: string[]): MCPConfig {
  const own: string[] = [];
  const off = "; the Claude connector is off";
  const listed = (k: "MCP_CLIENTS" | "MCP_REDIRECT_URIS", def: string[]): string[] => {
    if (invalid.has(k)) {
      own.push(`${k} is not a string${off}`);
      return [];
    }
    const v = (env as unknown as Record<string, unknown>)[k];
    if (v === undefined || v === null) return [...def];
    return [...new Set(urlList(v))];
  };
  const clients = listed("MCP_CLIENTS", DEFAULT_MCP_CLIENTS);
  const redirectURIs = listed("MCP_REDIRECT_URIS", DEFAULT_MCP_REDIRECT_URIS);
  for (const e of clients) {
    if (!validMCPClientURL(e)) {
      own.push(`MCP_CLIENTS: ${JSON.stringify(e)} is not a client metadata URL (an https URL with a path, without query, fragment or user info, at most 512 bytes)${off}`);
    }
  }
  let hub = "";
  if (publicURL) {
    try {
      hub = new URL(publicURL).hostname.toLowerCase();
    } catch {
      /* publicURL is validated above */
    }
  }
  for (const e of redirectURIs) {
    const h = mcpRedirectURIHost(e);
    if (!h) {
      own.push(
        `MCP_REDIRECT_URIS: ${JSON.stringify(e)} is not an https redirect URI without fragment or user info (at most 512 bytes; loopback redirect URIs are always allowed and are not listed)${off}`,
      );
    } else if (h === hub || (baseDomain !== "" && (h === baseDomain || h.endsWith("." + baseDomain)))) {
      own.push(`MCP_REDIRECT_URIS: ${JSON.stringify(e)} points at this Backyard or one of its sites${off}`);
    }
  }
  const wanted = bool(env.MCP_ENABLED);
  if (wanted && publicURL && publicURL.startsWith("http://") && !isLoopbackHost(hub)) {
    own.push(`MCP_ENABLED needs an https hub URL (PUBLIC_URL is ${publicURL}; plain http only on localhost, *.localhost or loopback addresses)${off}`);
  }
  problems.push(...own);
  return { enabled: wanted && own.length === 0, clients, redirectURIs, problems: own };
}

/** Effective per-file cap for deploys, given whether R2 is bound. */
export function maxFileBytes(cfg: Config, hasR2: boolean): number {
  return Math.min(cfg.limits.max_file_bytes, hasR2 ? MAX_R2_FILE : MAX_INLINE_FILE);
}

/** Effective per-upload cap for POST /files. */
export function maxUploadBytes(cfg: Config, hasR2: boolean): number {
  return Math.min(cfg.limits.max_upload_bytes, hasR2 ? MAX_R2_FILE : MAX_INLINE_FILE);
}

/** Admin rule shared by every authentication source (ADMINS ∪ ADMIN_GROUPS). */
export function isAdminIdentity(cfg: Config, id: string, email: string, groups: string[]): boolean {
  const lid = id.toLowerCase();
  const le = email.toLowerCase();
  if (cfg.admins.some((a) => a === lid || (le !== "" && a === le))) return true;
  return groups.some((g) => cfg.adminGroups.includes(g.toLowerCase()));
}
