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
};

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
  serverName: string;
  reserved: string[];
  limits: Limits;
  fileInlineMax: number;
  tokenTTLSeconds: number;
  secret: string;
  deployToken: string;
  warehouseURL: string;
  ai: AIConfig;
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
export function parseConfig(env: Env): Config {
  const problems: string[] = [];
  const urlMode = str(env.URL_MODE).toLowerCase() === "host" ? "host" : "path";
  if (str(env.URL_MODE) && !["host", "path"].includes(str(env.URL_MODE).toLowerCase())) {
    problems.push(`URL_MODE must be "path" or "host" (got ${JSON.stringify(env.URL_MODE)}); using path`);
  }
  const baseDomain = str(env.BASE_DOMAIN).toLowerCase().replace(/^https?:\/\//, "").replace(/[:/].*$/, "").replace(/\.$/, "");
  if (urlMode === "host" && !baseDomain) problems.push("URL_MODE=host needs BASE_DOMAIN; sites are served in path mode until it is set");
  let publicURL = str(env.PUBLIC_URL).replace(/\/+$/, "");
  if (publicURL && !/^https?:\/\/[^/]+$/i.test(publicURL)) {
    problems.push(`PUBLIC_URL must be an absolute origin like https://backyard.example.com (got ${JSON.stringify(publicURL)}); ignoring it`);
    publicURL = "";
  }
  const am = str(env.AUTH_MODE).toLowerCase();
  const authMode = am === "access" ? "access" : "none";
  if (am && am !== "access" && am !== "none") problems.push(`AUTH_MODE must be "none" or "access" (got ${JSON.stringify(am)}); using none`);
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
        if (!(k in DEFAULT_LIMITS)) {
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
  if (!secret) problems.push("BACKYARD_SECRET is not set (run npm run setup or wrangler secret put BACKYARD_SECRET)");
  else if (secret.length < 32) problems.push("BACKYARD_SECRET must be at least 32 characters");

  const ownership = str(env.OWNERSHIP).toLowerCase() === "owner" ? "owner" : "none";
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
    serverName: str(env.SERVER_NAME) || "Backyard",
    reserved,
    limits,
    fileInlineMax: Math.min(int(env.FILE_INLINE_MAX, 5 * 1024 * 1024), MAX_INLINE_FILE),
    tokenTTLSeconds: Math.max(60, int(env.TOKEN_TTL_HOURS, 720) * 3600),
    secret: secret.length >= 32 ? secret : "",
    deployToken: typeof env.DEPLOY_TOKEN === "string" ? env.DEPLOY_TOKEN.trim() : "",
    warehouseURL: str(env.WAREHOUSE_URL).replace(/\/+$/, ""),
    ai: parseAI(env, problems),
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
      if (models[explicit]?.kind === kind) return explicit;
      warnings.push(`default ${kind} model ${JSON.stringify(explicit)} is not a ${kind} alias in AI_MODELS`);
    }
    for (const p of prefs) if (models[p]?.kind === kind) return p;
    return Object.keys(models).sort().find((k) => models[k]?.kind === kind) ?? "";
  };
  problems.push(...warnings);
  return {
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
