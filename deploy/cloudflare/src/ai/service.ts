/**
 * Alias resolution for the AI proxy (SPEC §5.4): clients name aliases from
 * AI_MODELS (or "provider/model-id" when AI_ALLOW_RAW_MODELS is set), never
 * raw provider ids by default.
 */
import type { Config, ModelConfig } from "../config";
import type { Env } from "../env";
import { HttpError } from "../http";
import { AnthropicProvider } from "./anthropic";
import { OpenAIProvider } from "./openai";
import type { Provider } from "./types";
import { WorkersAIProvider, type AIBinding } from "./workersai";

const DEFAULT_OPENAI_BASE = "https://api.openai.com/v1";

/** A client-visible alias (GET /ai/models). */
export interface ModelInfo {
  name: string;
  kind: string;
  provider: string;
  model?: string;
  description?: string;
}

/** A resolved alias. */
export interface Resolved {
  alias: string;
  provider: Provider;
  model: string;
  config: ModelConfig | null;
}

export class AIService {
  private readonly providers = new Map<string, Provider>();

  constructor(
    private readonly cfg: Config,
    private readonly env: Env,
    private readonly fetcher: typeof fetch = (input, init) => fetch(input, init),
  ) {}

  /**
   * Whether a provider can serve requests here: Workers AI needs the AI binding,
   * Anthropic an API key, OpenAI a key (or a custom OpenAI-compatible base URL).
   */
  available(provider: string): boolean {
    switch (provider) {
      case "workers-ai":
        return this.env.AI != null;
      case "anthropic":
        return !!this.env.ANTHROPIC_API_KEY;
      case "openai":
        return !!this.env.OPENAI_API_KEY || this.cfg.ai.openaiBaseURL !== DEFAULT_OPENAI_BASE;
      default:
        return false;
    }
  }

  /** Whether any configured model is usable on this deployment. */
  enabled(): boolean {
    return Object.values(this.cfg.ai.models).some((m) => this.available(m.provider));
  }

  provider(name: string): Provider {
    let p = this.providers.get(name);
    if (!p) {
      switch (name) {
        case "workers-ai":
          p = new WorkersAIProvider(this.env.AI as unknown as AIBinding | undefined, this.cfg.ai.gatewayID);
          break;
        case "anthropic":
          p = new AnthropicProvider(this.env.ANTHROPIC_API_KEY ?? "", this.cfg.ai.anthropicBaseURL, this.fetcher);
          break;
        case "openai":
          p = new OpenAIProvider(this.env.OPENAI_API_KEY ?? "", this.cfg.ai.openaiBaseURL, this.fetcher);
          break;
        default:
          throw new HttpError(404, "not_found", `unknown AI provider ${JSON.stringify(name)}`);
      }
      this.providers.set(name, p);
    }
    return p;
  }

  /** Whether alias names a configured model (or an allowed raw model). */
  known(alias: string): boolean {
    if (!alias) return true;
    if (alias in this.cfg.ai.models) return true;
    return this.cfg.ai.allowRawModels && (alias.includes("/") || alias.startsWith("@"));
  }

  /**
   * The default alias of kind on this deployment: the configured default when
   * its provider is usable here, else the first usable alias of that kind (so
   * an AI_MODELS list whose default needs a missing binding or key still
   * serves requests that name no model). "" when there is none.
   */
  defaultAlias(kind: ModelConfig["kind"]): string {
    const ai = this.cfg.ai;
    const configured = kind === "chat" ? ai.defaultChat : kind === "image" ? ai.defaultImage : ai.defaultEmbedding;
    const usable = (name: string) => {
      const m = ai.models[name];
      return !!m && m.kind === kind && this.available(m.provider);
    };
    if (configured && usable(configured)) return configured;
    return Object.keys(ai.models).sort().find(usable) ?? "";
  }

  /** Resolves alias ("" = the default of kind) to a usable provider and model id. */
  resolve(alias: string, kind: ModelConfig["kind"]): Resolved {
    const ai = this.cfg.ai;
    const name = alias || this.defaultAlias(kind);
    if (!name) throw new HttpError(501, "unsupported", `no ${kind} model is available on this server`);
    const mc = ai.models[name];
    if (mc) {
      if (mc.kind !== kind) throw new HttpError(400, "bad_request", `model ${JSON.stringify(name)} is a ${mc.kind} model, not a ${kind} model`);
      this.requireAvailable(name, mc.provider);
      return { alias: name, provider: this.provider(mc.provider), model: mc.model, config: mc };
    }
    if (ai.allowRawModels) {
      let prov = "";
      let model = "";
      if (name.startsWith("@")) {
        prov = "workers-ai";
        model = name;
      } else {
        const slash = name.indexOf("/");
        if (slash > 0) {
          prov = name.slice(0, slash);
          model = name.slice(slash + 1);
        }
      }
      if ((prov === "workers-ai" || prov === "anthropic" || prov === "openai") && model) {
        this.requireAvailable(name, prov);
        return { alias: name, provider: this.provider(prov), model, config: null };
      }
    }
    throw new HttpError(404, "not_found", `unknown model ${JSON.stringify(name)}`);
  }

  /** 501 when the provider of a model cannot be used on this deployment. */
  private requireAvailable(name: string, provider: string): void {
    if (this.available(provider)) return;
    const why =
      provider === "workers-ai"
        ? "the Workers AI binding (AI) is not configured"
        : provider === "anthropic"
          ? "ANTHROPIC_API_KEY is not set"
          : "OPENAI_API_KEY is not set";
    throw new HttpError(501, "unsupported", `model ${JSON.stringify(name)} is not available on this server: ${why}`);
  }

  /** GET /ai/models: usable aliases sorted by name and the default chat alias. */
  models(): { default: string; models: ModelInfo[] } {
    const ai = this.cfg.ai;
    const models = Object.keys(ai.models)
      .filter((name) => this.available(ai.models[name]!.provider))
      .sort()
      .map((name) => {
        const m = ai.models[name]!;
        const info: ModelInfo = { name, kind: m.kind, provider: m.provider };
        if (ai.exposeModelIDs) info.model = m.model;
        if (m.description) info.description = m.description;
        return info;
      });
    return { default: this.defaultAlias("chat"), models };
  }
}
