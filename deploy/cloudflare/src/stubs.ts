/** Durable Object stubs and the guardrail helpers built on them. */
import type { Env } from "./env";
import { HttpError, unwrap } from "./http";
import type { Charge, RegistryDO } from "./registry-do";
import type { SiteDO } from "./site-do";

/** Version reported by /info. */
export const VERSION = "0.1.0";

/** The public methods of T (RPC surface of a Durable Object, with its plain types). */
type RPC<T> = { [K in keyof T as T[K] extends (...args: never[]) => Promise<unknown> ? K : never]: T[K] };

/** A SiteDO stub: typed RPC methods plus fetch for the streaming endpoints. */
export type SiteStub = Omit<RPC<SiteDO>, "fetch" | "webSocketMessage" | "webSocketClose" | "webSocketError" | "alarm"> & {
  fetch(input: RequestInfo | URL, init?: RequestInit): Promise<Response>;
};

/** A RegistryDO stub. */
export type RegistryStub = Omit<RPC<RegistryDO>, "fetch" | "alarm">;

/** The SiteDO of a site. */
export function siteStub(env: Env, site: string): SiteStub {
  return env.SITE.get(env.SITE.idFromName(site)) as unknown as SiteStub;
}

/** The single RegistryDO. */
export function registry(env: Env): RegistryStub {
  return env.REGISTRY.get(env.REGISTRY.idFromName("registry")) as unknown as RegistryStub;
}

const LABELS: Record<string, string> = {
  ai: "daily AI request limit",
  ai_platform: "daily AI budget of this Backyard",
  ai_image: "daily image generation limit",
  ai_image_platform: "daily image budget of this Backyard",
  deploy: "daily deploy limit",
  upload: "daily upload limit",
};

/**
 * Charges daily counters in the RegistryDO; throws 429 (with Retry-After until
 * UTC midnight) when one of them is exhausted. Limits of 0 are skipped.
 */
export async function chargeOrThrow(env: Env, charges: Charge[]): Promise<void> {
  const active = charges.filter((c) => c.limit > 0);
  if (active.length === 0) return;
  const r = unwrap(await registry(env).charge(active));
  if (r.allowed) return;
  const kind = (r.exhausted ?? "").split("|")[0] ?? "";
  const label = LABELS[kind] ?? "daily limit";
  const limit = active.find((c) => c.key === r.exhausted)?.limit;
  throw new HttpError(429, "rate_limited", `${label} reached${limit ? ` (${limit} per day)` : ""}; it resets at midnight UTC`, {
    retryAfter: r.retryAfter ?? 3600,
  });
}
