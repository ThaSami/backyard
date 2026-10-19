// The live configuration of a deployed Worker, adoption of it by a machine
// without state, and the auth guard.
//
// * readLive({api, accountId, worker}) reads GET …/workers/scripts/<w>/settings
//   (bindings) and GET …/workers/scripts/<w>/subdomain (workers.dev on/off).
//   A 404 or code 10007 means the Worker does not exist; any other error
//   leaves it unknown (exists: null) and the guard refuses to guess.
// * adopt(state, live, baseVars) fills a fresh state from the live Worker:
//   R2 bucket, AI off (no AI binding and AI_ENABLED false), owners-only site
//   listing (SITE_LISTING=owners), variables that differ from the defaults,
//   the domain (BASE_DOMAIN) and Cloudflare Access (AUTH_MODE=access).
// * authGuard(live, desiredVars, o) refuses (CliError 3) a deploy that would
//   turn a live AUTH_MODE=access off, change its team, or replace a Worker
//   whose AUTH_MODE cannot be read, unless o.allow (setup
//   --confirm-auth-change, BACKYARD_CF_CONFIRM_AUTH_CHANGE in CI).
// * normalizeTeam() is the Worker's own normalization (src/config.ts).

import { CfError } from "./cfapi.mjs";
import { parseAccessRedirect, probe } from "./net.mjs";
import { CliError, EXIT } from "./ui.mjs";

/** Variables derived from state.domain (never stored in state.vars while it is set). */
export const MANAGED_DOMAIN_KEYS = ["BASE_DOMAIN", "URL_MODE"];
/** Variables derived from state.access. */
export const MANAGED_ACCESS_KEYS = ["AUTH_MODE", "ACCESS_TEAM_DOMAIN", "ACCESS_AUD"];
/** Variables derived from state.ai === false (`setup --no-ai`; never stored in state.vars then). */
export const MANAGED_AI_KEYS = ["AI_ENABLED"];
/** Variables derived from state.siteListing (`setup --site-listing`; never stored in state.vars). */
export const MANAGED_LISTING_KEYS = ["SITE_LISTING"];

/**
 * Defaults that the wrangler.jsonc of earlier releases wrote out and that are
 * now left unset, with the same meaning: a live value equal to one of them is
 * not an override. (OWNERSHIP "none" until 0.3.0; unset now means none, or
 * owner with SITE_LISTING=owners.)
 */
const LEGACY_DEFAULTS = { OWNERSHIP: "none" };

/**
 * Whether an AI_ENABLED value turns AI on, as the Worker reads it (bool() in
 * src/config.ts with the default true): unset or empty, true, 1, yes or on.
 * @param {unknown} v
 */
export function aiEnabledOf(v) {
  const s = String(v ?? "").trim().toLowerCase();
  return s === "" || s === "true" || s === "1" || s === "yes" || s === "on";
}

/**
 * A SITE_LISTING as the Worker reads it (trimmed, case-insensitive): "all"
 * (also when unset or empty), "owners", or null for any other value (the
 * Worker then refuses every request but the health checks).
 * @param {unknown} v
 * @returns {"all" | "owners" | null}
 */
export function siteListingOf(v) {
  const s = String(v ?? "").trim().toLowerCase();
  return s === "" || s === "all" ? "all" : s === "owners" ? "owners" : null;
}

/**
 * Normalizes a Cloudflare Access team like the Worker does: "Acme" →
 * "acme.cloudflareaccess.com", "https://acme.cloudflareaccess.com/" →
 * "acme.cloudflareaccess.com".
 * @param {unknown} v
 */
export function normalizeTeam(v) {
  let t = String(v ?? "").trim().toLowerCase();
  t = t.replace(/^https?:\/\//, "").replace(/\/.*$/, "");
  if (t === "") return "";
  if (!t.includes(".")) t += ".cloudflareaccess.com";
  return t;
}

/**
 * An AUTH_MODE as the Worker reads it: "none" (also when empty), "access",
 * or the invalid value itself (the Worker then refuses every request).
 * @param {unknown} v
 */
export function authModeOf(v) {
  const m = typeof v === "string" ? v.trim().toLowerCase() : v === undefined || v === null ? "" : `<${typeof v}>`;
  return m === "" ? "none" : m;
}

/** A BASE_DOMAIN as the Worker reads it (no scheme, port, path or trailing dot). */
export function hostOf(v) {
  return String(v ?? "").trim().toLowerCase().replace(/^https?:\/\//, "").replace(/[:/].*$/, "").replace(/\.$/, "");
}

/**
 * @typedef {object} Live
 * @property {boolean|null} exists       false: no such Worker; null: unknown
 * @property {Record<string, string>} vars   plain_text bindings, and json bindings (a JSON string as is, anything else JSON.stringify'd)
 * @property {string[]} secretNames      secret_text bindings
 * @property {string} r2Bucket           the bucket bound as R2 ("" = none)
 * @property {boolean} ai                Workers AI is bound as AI
 * @property {boolean|null} workersDev   null when unreadable
 * @property {string} authMode           see authModeOf ("" when unknown)
 * @property {string} team               normalized ACCESS_TEAM_DOMAIN ("" = unknown or none)
 * @property {string} aud                ACCESS_AUD
 * @property {"settings"|"info"|""} source   where authMode comes from
 * @property {string} error              why the configuration is unknown
 */

/** @returns {Live} */
function emptyLive(exists, error = "") {
  return { exists, vars: {}, secretNames: [], r2Bucket: "", ai: false, workersDev: null, authMode: "", team: "", aud: "", source: "", error };
}

/**
 * Reads the live configuration of a Worker.
 * @param {{api: import("./cfapi.mjs").Api, accountId: string, worker: string}} o
 * @returns {Promise<Live>}
 */
export async function readLive(o) {
  const base = `/accounts/${o.accountId}/workers/scripts/${o.worker}`;
  let settings;
  try {
    settings = await o.api.get(`${base}/settings`);
  } catch (e) {
    if (e instanceof CfError && (e.status === 404 || e.has(10007))) return emptyLive(false);
    return emptyLive(null, e?.message ?? String(e));
  }
  const live = emptyLive(true);
  for (const b of Array.isArray(settings?.bindings) ? settings.bindings : []) {
    if (!b || typeof b.name !== "string") continue;
    if (b.type === "plain_text") live.vars[b.name] = String(b.text ?? "");
    else if (b.type === "json") live.vars[b.name] = typeof b.json === "string" ? b.json : JSON.stringify(b.json);
    else if (b.type === "secret_text") live.secretNames.push(b.name);
    else if (b.type === "r2_bucket" && b.name === "R2") live.r2Bucket = String(b.bucket_name ?? "");
    else if (b.type === "ai" && b.name === "AI") live.ai = true;
  }
  live.authMode = authModeOf(live.vars.AUTH_MODE);
  live.team = normalizeTeam(live.vars.ACCESS_TEAM_DOMAIN);
  live.aud = String(live.vars.ACCESS_AUD ?? "").trim();
  live.source = "settings";
  try {
    const sub = await o.api.get(`${base}/subdomain`);
    live.workersDev = typeof sub?.enabled === "boolean" ? sub.enabled : null;
  } catch {
    live.workersDev = null;
  }
  return live;
}

/**
 * When the settings could not be read, asks the deployed Worker itself
 * (GET <url>/__backyard/api/deploy/info, no auth) for its auth mode. An
 * Access login redirect there counts as AUTH_MODE=access. Updates `live` in
 * place and returns it.
 * @param {Live} live
 * @param {string[]} urls   candidate hub URLs (state.url, workersDevUrl)
 */
export async function fillAuthFromInfo(live, urls) {
  if (live.exists !== null || live.authMode) return live;
  for (const u of urls.filter(Boolean)) {
    const r = await probe(`${String(u).replace(/\/+$/, "")}/__backyard/api/deploy/info`, { accept: "application/json" });
    if (!r.ok) continue;
    const redirect = r.status >= 300 && r.status < 400 ? parseAccessRedirect(r.location) : null;
    if (redirect) {
      Object.assign(live, { exists: true, authMode: "access", team: redirect.team, source: "info" });
      return live;
    }
    if (r.status === 200 && r.json?.edition === "cloudflare" && typeof r.json?.auth?.mode === "string") {
      Object.assign(live, { exists: true, authMode: authModeOf(r.json.auth.mode), source: "info" });
      return live;
    }
  }
  return live;
}

/**
 * Fills a state without setup history from the live Worker. Managed keys go
 * to state.domain / state.access / state.ai when they are in use (BASE_DOMAIN
 * set, AUTH_MODE=access, no AI binding with AI_ENABLED off); otherwise, like
 * every other variable, they are kept in state.vars when they differ from the
 * defaults, so the next deploy changes nothing. SITE_LISTING always goes to
 * state.siteListing ("owners", also for an invalid value, which the Worker
 * never reads as all). Returns the parts that were adopted (for the one-line
 * summary).
 * @param {Record<string, any>} state
 * @param {Live} live
 * @param {Record<string, unknown>} baseVars  the defaults (wrangler config vars)
 * @returns {{parts: string[], warnings: string[]}}
 */
export function adopt(state, live, baseVars = {}) {
  const parts = [];
  const warnings = [];
  if (live.r2Bucket) {
    state.r2 = true;
    state.r2Bucket = live.r2Bucket;
    parts.push(`R2 bucket ${live.r2Bucket}`);
  }
  const v = live.vars;
  // Deployed with --no-ai: no binding and AI_ENABLED off. A Worker without the
  // binding whose AI is on (other providers) keeps AI on: the next deploy binds it.
  const aiOff = !live.ai && !aiEnabledOf(v.AI_ENABLED);
  if (aiOff) {
    state.ai = false;
    parts.push("AI off");
  }
  // Deployed with --site-listing owners (or SITE_LISTING=owners set otherwise).
  const listing = siteListingOf(v.SITE_LISTING);
  if (listing !== "all") {
    state.siteListing = "owners";
    parts.push("owners-only site listing");
    if (listing === null) {
      warnings.push(`SITE_LISTING=${JSON.stringify(v.SITE_LISTING)} is neither all nor owners (the Worker refuses every request): adopted as --site-listing owners`);
    }
  }
  const host = hostOf(v.BASE_DOMAIN);
  const accessOn = authModeOf(v.AUTH_MODE) === "access";
  const managedNow = new Set([
    ...(host ? MANAGED_DOMAIN_KEYS : []),
    ...(accessOn ? MANAGED_ACCESS_KEYS : []),
    ...(aiOff ? MANAGED_AI_KEYS : []),
    ...MANAGED_LISTING_KEYS,
  ]);
  const vars = { ...(state.vars ?? {}) };
  const copied = [];
  for (const [k, val] of Object.entries(v)) {
    if (managedNow.has(k)) continue;
    const def = baseVars[k] === undefined || baseVars[k] === null ? "" : typeof baseVars[k] === "string" ? baseVars[k] : JSON.stringify(baseVars[k]);
    if (def === "" && Object.hasOwn(LEGACY_DEFAULTS, k) && val === LEGACY_DEFAULTS[k]) continue;
    if (val !== def) {
      vars[k] = val;
      copied.push(k);
    }
  }
  if (copied.length) parts.push(`variables ${copied.sort().join(", ")}`);
  state.vars = Object.keys(vars).length ? vars : undefined;
  if (host) {
    const mode = String(v.URL_MODE ?? "").trim().toLowerCase() === "host" ? "host" : "path";
    state.domain = { host, mode, status: "active", source: "adopted" };
    parts.push(`domain ${host} (${mode} mode)`);
  }
  if (accessOn) {
    state.access = { team: normalizeTeam(v.ACCESS_TEAM_DOMAIN), aud: String(v.ACCESS_AUD ?? "").trim(), source: "adopted" };
    parts.push(`Access team ${state.access.team || "(none set)"}`);
  }
  if (live.workersDev === null) {
    warnings.push("could not read whether the Worker serves on workers.dev: assuming it does");
    state.workersDev = true;
  } else {
    state.workersDev = live.workersDev;
    if (!live.workersDev) parts.push("workers.dev off");
  }
  return { parts, warnings };
}

/**
 * The auth guard: compares the live Worker with the variables about to be
 * deployed. Throws CliError(3) (reason "downgrade", "team", "fail-closed" or
 * "unknown") unless `o.allow`; returns notes to print otherwise.
 * @param {Live} live
 * @param {Record<string, unknown>} desired   the variables of the deploy
 * @param {{allow?: boolean, worker?: string, remedy?: string}} [o]
 * @returns {{checked: boolean, notes: string[]}}
 */
export function authGuard(live, desired, o = {}) {
  const w = o.worker ?? "the Worker";
  const remedy = o.remedy ? `\n${o.remedy}` : "";
  const notes = [];
  if (live.exists === false) return { checked: false, notes };
  const desiredMode = authModeOf(desired.AUTH_MODE);
  const refuse = (reason, msg) => {
    if (o.allow) {
      notes.push(`${msg.replace(/\.? Nothing was changed\.$/, "")} — deploying anyway (confirmed)`);
      return { checked: true, notes };
    }
    throw new CliError(EXIT.REFUSED, msg + remedy, { reason });
  };
  if (!live.authMode) {
    return refuse(
      "unknown",
      `could not read the live configuration of Worker ${w}${live.error ? ` (${live.error})` : ""}, so this deploy cannot be checked against it: it might turn Cloudflare Access off. Nothing was changed.`,
    );
  }
  if (live.authMode === "access") {
    if (desiredMode !== "access") {
      return refuse(
        "downgrade",
        `Worker ${w} is live with Cloudflare Access${live.team ? ` (team ${live.team})` : ""}, and this deploy would turn it off (AUTH_MODE=${desiredMode}). Nothing was changed.`,
      );
    }
    const want = normalizeTeam(desired.ACCESS_TEAM_DOMAIN);
    if (live.team && want !== live.team) {
      return refuse(
        "team",
        `Worker ${w} is live with Cloudflare Access team ${live.team}, and this deploy would switch it to ${want || "no team"}. Nothing was changed.`,
      );
    }
    if (!live.team) notes.push(`Worker ${w} is live with Cloudflare Access; its team could not be read, so the team of this deploy (${want}) is not compared`);
    const aud = String(desired.ACCESS_AUD ?? "").trim();
    if (live.aud && aud && live.aud.toLowerCase() !== aud.toLowerCase()) notes.push(`the Access AUD changes from ${live.aud} to ${aud}`);
    return { checked: true, notes };
  }
  if (live.authMode !== "none" && desiredMode === "none") {
    return refuse(
      "fail-closed",
      `Worker ${w} is live with AUTH_MODE=${live.authMode}, which refuses every request (fail closed), and this deploy would open it (AUTH_MODE=none). Nothing was changed.`,
    );
  }
  return { checked: true, notes };
}
