/**
 * Bindings, variables and secrets of the Backyard Worker (wrangler.jsonc).
 * Every variable is optional; config.ts applies the defaults.
 */
import type { RegistryDO } from "./registry-do";
import type { SiteDO } from "./site-do";

export interface Env {
  /** One SiteDO per site (idFromName(site)). */
  SITE: DurableObjectNamespace<SiteDO>;
  /** The single RegistryDO (site list, user cache, guardrail counters). */
  REGISTRY: DurableObjectNamespace<RegistryDO>;
  /** Workers AI binding. */
  AI?: Ai;
  /** Optional R2 bucket for large files. */
  R2?: R2Bucket;

  // --- vars
  BASE_DOMAIN?: string;
  PUBLIC_URL?: string;
  URL_MODE?: string;
  AUTH_MODE?: string;
  ACCESS_TEAM_DOMAIN?: string;
  ACCESS_AUD?: string;
  /** "false" disables the Access get-identity call (name/groups enrichment). */
  ACCESS_GET_IDENTITY?: string;
  ALLOW_ANONYMOUS_DEPLOY?: string;
  ADMINS?: string;
  ADMIN_GROUPS?: string;
  /** "none" or "owner"; unset = none, or owner with SITE_LISTING=owners. */
  OWNERSHIP?: string;
  /**
   * "owners" lists a site only to its owners (its creator while it has none)
   * and to admins, like the Go server's sites.listing: owners (SPEC §7.2
   * "Listing"); unset, empty or "all" lists every site.
   */
  SITE_LISTING?: string;
  SERVER_NAME?: string;
  /** Extra reserved site names (comma separated), added to the defaults. */
  RESERVED_SITES?: string;
  /**
   * "false" turns AI off: every /__backyard/api/ai/* endpoint answers 501
   * unsupported, like the Go server's ai.enabled: false (unset = on).
   */
  AI_ENABLED?: string;
  AI_MODELS?: string;
  AI_DEFAULT_MODEL?: string;
  AI_DEFAULT_IMAGE_MODEL?: string;
  AI_DEFAULT_EMBEDDING_MODEL?: string;
  AI_MAX_TOKENS?: string;
  AI_ALLOW_RAW_MODELS?: string;
  AI_EXPOSE_MODEL_IDS?: string;
  AI_SYSTEM_PROMPT?: string;
  /** Cloudflare AI Gateway id used for Workers AI calls (optional). */
  AI_GATEWAY_ID?: string;
  /** Base URL of the Anthropic API (e.g. an AI Gateway URL). */
  ANTHROPIC_BASE_URL?: string;
  /** Base URL of an OpenAI-compatible API (default https://api.openai.com/v1). */
  OPENAI_BASE_URL?: string;
  LIMITS?: string;
  FILE_INLINE_MAX?: string;
  TOKEN_TTL_HOURS?: string;
  /** Site API base of a Backyard Go server whose warehouse is proxied (optional). */
  WAREHOUSE_URL?: string;
  /** "true" serves the Claude connector (remote MCP, SPEC §8.4, §14.1); unset = off. */
  MCP_ENABLED?: string;
  /**
   * Client ID Metadata Document URLs that are fetched and shown as verified,
   * and that may connect from anywhere (comma or whitespace separated).
   * Unset: Claude's two documents and ChatGPT's; set but empty: none, so
   * clients register themselves. Another valid client metadata URL connects
   * without a fetch, unverified, with a loopback redirect address only.
   */
  MCP_CLIENTS?: string;
  /**
   * Exact https addresses that may receive sign-in codes (comma or whitespace
   * separated). Unset: Claude's two callbacks and ChatGPT's; set but empty:
   * http loopback addresses only (always allowed).
   */
  MCP_REDIRECT_URIS?: string;

  // --- secrets
  BACKYARD_SECRET?: string;
  DEPLOY_TOKEN?: string;
  ANTHROPIC_API_KEY?: string;
  OPENAI_API_KEY?: string;
  WAREHOUSE_TOKEN?: string;
}
