#!/usr/bin/env node
// Deploys the Backyard Workers edition (deploy/cloudflare, SPEC §16) to a
// Cloudflare account from CI, the non-interactive counterpart of
// `npm run setup` / `backyard cloudflare setup`. Used by
// .github/workflows/cloudflare-deploy.yml; works in any CI system.
//
//   node scripts/cloudflare-ci.mjs deploy            # deploy (or upgrade) the Worker
//   node scripts/cloudflare-ci.mjs deploy --dry-run  # bundle and validate only
//
// Everything is read from the environment, so secrets never appear on a
// command line:
//
//   CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID   wrangler credentials (not needed with --dry-run)
//   BACKYARD_SECRET    ≥ 32 characters. Optional: when unset, the Worker's
//                      existing secret is kept, or a random one is generated
//                      on the first deploy (it is never printed).
//   DEPLOY_TOKEN       optional static deploy token (kept when unset)
//   ANTHROPIC_API_KEY, OPENAI_API_KEY, WAREHOUSE_TOKEN   optional secrets
//   BACKYARD_CF_WORKER_NAME   Worker name (default: "name" in wrangler.jsonc)
//   BACKYARD_CF_VARS          JSON object of Worker variables merged over
//                             wrangler.jsonc, e.g. {"URL_MODE":"host",
//                             "BASE_DOMAIN":"example.com","LIMITS":{"ai_per_day":50}}
//   BACKYARD_CF_R2_BUCKET     R2 bucket for large files: created if missing and
//                             bound as R2 (needs R2 enabled on the account)
//   BACKYARD_CF_AI            "false" turns AI off, like `setup --no-ai`: the deploy
//                             has no Workers AI binding and AI_ENABLED is "false"
//                             (then BACKYARD_CF_VARS and BACKYARD_CF_DEPLOY_ARGS may
//                             not set AI_ENABLED). Empty or "true": AI on (default)
//   BACKYARD_CF_SITE_LISTING  "owners" lists each site only to its owners and admins, like
//                             `setup --site-listing owners`: SITE_LISTING is "owners", which
//                             implies OWNERSHIP=owner (so OWNERSHIP, when set, must be
//                             "owner"). Empty or "all": every site is listed (default), and
//                             ownership is off again unless OWNERSHIP=owner is set.
//                             BACKYARD_CF_VARS and BACKYARD_CF_DEPLOY_ARGS never set SITE_LISTING,
//                             and a SITE_LISTING in wrangler.jsonc must agree with this switch
//   BACKYARD_CF_DEPLOY_ARGS   extra `wrangler deploy` arguments (split on spaces)
//   BACKYARD_CF_DIR           the Worker project (default: deploy/cloudflare)
//   BACKYARD_CF_WAIT_SECONDS  how long to wait for /__backyard/readyz (default 60, 0 = skip)
//   BACKYARD_CF_CONFIRM_AUTH_CHANGE  "true" lets this deploy turn Cloudflare Access off,
//                             change its team, or go ahead when the live auth mode cannot
//                             be read (the auth guard below); set it for one run only
//                             (cloudflare-deploy.yml: the confirm_auth_change input)
//   CLOUDFLARE_API_BASE_URL   Cloudflare API base for wrangler and the guard's REST calls
//                             (default https://api.cloudflare.com/client/v4)
//   WRANGLER_BIN              wrangler entry point (default: the project's node_modules)
//
// Secrets are uploaded together with the new version (`wrangler deploy
// --secrets-file`), so a first deploy never runs without BACKYARD_SECRET. In
// GitHub Actions the hub URL is written to $GITHUB_OUTPUT (url, worker) and a
// summary to $GITHUB_STEP_SUMMARY; secret values are masked and never logged.
//
// Auth guard (the rules of `backyard cloudflare setup`, from
// deploy/cloudflare/scripts/lib/live.mjs): before a deploy (not a dry run) the
// live Worker's settings are read with CLOUDFLARE_API_TOKEN
// (GET /accounts/$CLOUDFLARE_ACCOUNT_ID/workers/scripts/<worker>/settings). The
// deploy is refused, and nothing is changed, when the live Worker runs
// AUTH_MODE=access and this deploy (wrangler.jsonc + BACKYARD_CF_VARS + --var in
// BACKYARD_CF_DEPLOY_ARGS) would turn it off or switch its team, or when the
// live auth mode cannot be read (the Worker's own /__backyard/api/deploy/info on
// workers.dev is the fallback), unless BACKYARD_CF_CONFIRM_AUTH_CHANGE=true. A
// first deploy (wrangler reports the Worker missing) is not guarded. So keep
// AUTH_MODE, ACCESS_TEAM_DOMAIN and ACCESS_AUD (and BASE_DOMAIN, URL_MODE) in
// BACKYARD_CF_VARS once `backyard cloudflare access enable` / `domain add` set them.
//
// Routes and custom domains are never deployed from here: the generated config
// has no "routes" (wrangler.jsonc's are ignored with a warning), and
// BACKYARD_CF_DEPLOY_ARGS may not set --route(s), --domain(s), --config, --env
// or --name. They belong to `backyard cloudflare domain add` (or Terraform).

import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { appendFileSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { apiBase, createApi } from "../deploy/cloudflare/scripts/lib/cfapi.mjs";
import { authGuard, authModeOf, fillAuthFromInfo, readLive, siteListingOf } from "../deploy/cloudflare/scripts/lib/live.mjs";
import { proxyHint } from "../deploy/cloudflare/scripts/lib/net.mjs";
import { CliError } from "../deploy/cloudflare/scripts/lib/ui.mjs";
import { isMissingWorker, parseSecretList as parseSecretListOutput } from "../deploy/cloudflare/scripts/lib/wrangler.mjs";

export { isMissingWorker };

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

/** Secrets the Worker understands (deploy/cloudflare/src/env.ts). */
export const SECRET_NAMES = ["BACKYARD_SECRET", "DEPLOY_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "WAREHOUSE_TOKEN"];

/** Name of the generated wrangler configuration (next to wrangler.jsonc). */
export const GENERATED_CONFIG = "wrangler.ci.jsonc";

const WORKER_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const BUCKET_RE = /^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/;
const VAR_NAME_RE = /^[A-Z_][A-Z0-9_]*$/;
const MIN_SECRET_LEN = 32;

/** CIError is a failure with a message meant for the person running the deploy. */
export class CIError extends Error {
  constructor(message) {
    super(message);
    this.name = "CIError";
  }
}

/**
 * Parses JSONC (JSON with // and /* *\/ comments and trailing commas), the
 * format of wrangler.jsonc. Comment markers and commas inside strings are
 * left alone.
 */
export function parseJSONC(text) {
  let out = "";
  let inStr = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inStr) {
      out += c;
      if (c === "\\") out += text[++i] ?? "";
      else if (c === '"') inStr = false;
    } else if (c === '"') {
      inStr = true;
      out += c;
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      out += "\n";
    } else if (c === "/" && text[i + 1] === "*") {
      i += 2;
      while (i < text.length && !(text[i] === "*" && text[i + 1] === "/")) i++;
      i++;
      out += " ";
    } else if (c === ",") {
      let j = i + 1;
      while (j < text.length && /\s/.test(text[j])) j++;
      // A comment may sit between a trailing comma and the bracket.
      if (text[j] === "/" && (text[j + 1] === "/" || text[j + 1] === "*")) {
        const rest = parseTrailing(text, j);
        if (rest === "}" || rest === "]") continue;
      } else if (text[j] === "}" || text[j] === "]") {
        continue;
      }
      out += c;
    } else {
      out += c;
    }
  }
  return JSON.parse(out);
}

// parseTrailing returns the first significant character at or after i,
// skipping whitespace and comments.
function parseTrailing(text, i) {
  while (i < text.length) {
    if (/\s/.test(text[i])) {
      i++;
    } else if (text[i] === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++;
    } else if (text[i] === "/" && text[i + 1] === "*") {
      i += 2;
      while (i < text.length && !(text[i] === "*" && text[i + 1] === "/")) i++;
      i += 2;
    } else {
      return text[i];
    }
  }
  return "";
}

/**
 * Parses BACKYARD_CF_AI: true (AI on, the default; empty or unset) or false
 * (no Workers AI binding and AI_ENABLED=false, like `setup --no-ai`).
 */
export function parseAISwitch(raw) {
  const v = String(raw ?? "").trim().toLowerCase();
  if (v === "" || v === "true") return true;
  if (v === "false") return false;
  throw new CIError(`BACKYARD_CF_AI must be "true" or "false", not ${JSON.stringify(String(raw))}`);
}

/**
 * Parses BACKYARD_CF_SITE_LISTING: "all" (every site is listed, the default;
 * empty or unset) or "owners" (SITE_LISTING=owners, like `setup --site-listing
 * owners`).
 * @returns {"all" | "owners"}
 */
export function parseSiteListing(raw) {
  const v = String(raw ?? "").trim().toLowerCase();
  if (v === "" || v === "all") return "all";
  if (v === "owners") return "owners";
  throw new CIError(`BACKYARD_CF_SITE_LISTING must be "all" or "owners", not ${JSON.stringify(String(raw))}`);
}

/**
 * Parses BACKYARD_CF_VARS: a JSON object of Worker variables. Non-string
 * values (numbers, booleans, objects such as LIMITS) are JSON-encoded, since
 * Worker variables are strings. Secret names are refused: variables are
 * stored and shown in plain text.
 */
export function parseVars(raw) {
  if (raw === undefined || raw === null || String(raw).trim() === "") return {};
  let obj;
  try {
    obj = JSON.parse(raw);
  } catch (e) {
    throw new CIError(`BACKYARD_CF_VARS is not valid JSON: ${e.message}`);
  }
  if (obj === null || typeof obj !== "object" || Array.isArray(obj)) {
    throw new CIError('BACKYARD_CF_VARS must be a JSON object, e.g. {"URL_MODE":"host","BASE_DOMAIN":"example.com"}');
  }
  const out = {};
  for (const [k, v] of Object.entries(obj)) {
    if (!VAR_NAME_RE.test(k)) throw new CIError(`BACKYARD_CF_VARS: invalid variable name ${JSON.stringify(k)}`);
    if (SECRET_NAMES.includes(k)) {
      throw new CIError(`BACKYARD_CF_VARS: ${k} is a secret; set it as a repository secret, not as a plain-text variable`);
    }
    if (v === null || v === undefined) continue;
    out[k] = typeof v === "string" ? v : JSON.stringify(v);
  }
  return out;
}

/**
 * Arguments of `wrangler deploy` that BACKYARD_CF_DEPLOY_ARGS may not set: they
 * would attach routes or custom domains, or deploy another config, environment
 * or Worker than the one the auth guard checked.
 */
const RESERVED_DEPLOY_ARGS = [
  [/^--(routes?|domains?)(=|$)/, "routes and custom domains are managed by `backyard cloudflare domain add` (or Terraform); deploys never change them"],
  [/^(--config|-c)(=|$)/, "the deploy uses wrangler.jsonc in BACKYARD_CF_DIR with BACKYARD_CF_VARS"],
  [/^(--env|-e)(=|$)/, "the deploy uses the top-level environment of wrangler.jsonc"],
  [/^--name(=|$)/, "set BACKYARD_CF_WORKER_NAME instead"],
];

/** Splits BACKYARD_CF_DEPLOY_ARGS on whitespace. Secrets and the reserved arguments are refused. */
export function parseDeployArgs(raw) {
  const args = String(raw ?? "").split(/\s+/).filter(Boolean);
  for (const a of args) {
    if (/^--secrets?-file/.test(a)) throw new CIError("BACKYARD_CF_DEPLOY_ARGS may not set --secrets-file (secrets come from the environment)");
    // KEY:VALUE, and KEY=VALUE (a typo wrangler would take as a variable name): never a secret.
    const m = /(?:^|=)([A-Z_][A-Z0-9_]*)[:=]/.exec(a);
    if (m && SECRET_NAMES.includes(m[1])) {
      throw new CIError(`BACKYARD_CF_DEPLOY_ARGS: ${m[1]} is a secret; never pass it with --var`);
    }
    for (const [re, why] of RESERVED_DEPLOY_ARGS) {
      if (re.test(a)) throw new CIError(`BACKYARD_CF_DEPLOY_ARGS may not set ${a.split("=")[0]}: ${why}`);
    }
  }
  return args;
}

/**
 * The variables BACKYARD_CF_DEPLOY_ARGS sets with `--var KEY:VALUE` (or
 * `--var=KEY:VALUE`; wrangler takes several values after one --var). wrangler
 * lets them override the config, so the auth guard sees them too.
 */
export function deployArgVars(args) {
  const out = {};
  for (let i = 0; i < args.length; i++) {
    const values = [];
    if (args[i].startsWith("--var=")) values.push(args[i].slice("--var=".length));
    else if (args[i] === "--var") while (i + 1 < args.length && !args[i + 1].startsWith("-")) values.push(args[++i]);
    for (const v of values) {
      const c = v.indexOf(":");
      if (c > 0) out[v.slice(0, c)] = v.slice(c + 1);
    }
  }
  return out;
}

/** Whether a wrangler config lists routes (or custom domains, which are routes too). */
export function hasRoutes(config) {
  return config?.routes !== undefined || config?.route !== undefined;
}

/**
 * Builds the effective wrangler configuration: wrangler.jsonc with variable
 * overrides and the optional R2 binding, without the Workers AI binding and
 * with AI_ENABLED=false when ai is false, with SITE_LISTING=owners when
 * listing is "owners", and never routes (a deploy must not attach or move
 * routes and custom domains). Returns null when nothing changes.
 */
export function effectiveConfig(config, { vars = {}, bucket = "", ai = true, listing = "all" } = {}) {
  if (!Object.keys(vars).length && !bucket && ai && listing !== "owners" && !hasRoutes(config)) return null;
  const out = { ...config, vars: { ...(config.vars ?? {}), ...vars } };
  delete out.routes;
  delete out.route;
  if (!ai) {
    delete out.ai;
    out.vars.AI_ENABLED = "false";
  }
  if (listing === "owners") out.vars.SITE_LISTING = "owners";
  if (bucket) {
    const others = (config.r2_buckets ?? []).filter((b) => b.binding !== "R2");
    out.r2_buckets = [...others, { binding: "R2", bucket_name: bucket }];
  }
  return out;
}

/**
 * Picks the hub URL: PUBLIC_URL, else https://BASE_DOMAIN, else the first
 * https target wrangler reported (the workers.dev URL).
 */
export function hubURL(vars, outputText) {
  const pub = String(vars.PUBLIC_URL ?? "").trim().replace(/\/+$/, "");
  if (pub) return pub;
  const base = String(vars.BASE_DOMAIN ?? "").trim().replace(/\.$/, "");
  if (base) return `https://${base}`;
  let url = "";
  for (const line of String(outputText ?? "").split("\n")) {
    if (!line.trim()) continue;
    let entry;
    try {
      entry = JSON.parse(line);
    } catch {
      continue;
    }
    if (entry?.type !== "deploy" || !Array.isArray(entry.targets)) continue;
    const t = entry.targets.find((x) => typeof x === "string" && /^https:\/\//.test(x));
    if (t) url = t.replace(/\/+$/, "");
  }
  return url;
}

/**
 * Extracts the secret names from the stdout of `wrangler secret list --format
 * json` (deploy/cloudflare/scripts/lib/wrangler.mjs, shared with setup): only a
 * JSON list of {name} objects counts, a stray "[]" line never hides the real
 * list, and anything else throws a CIError. An empty result would make the
 * deploy generate BACKYARD_SECRET, rotating it (and revoking every API token)
 * on an existing Worker: never guess.
 */
export function parseSecretList(output) {
  try {
    return parseSecretListOutput(output);
  } catch (e) {
    throw new CIError(`unexpected \`wrangler secret list\` output (${e?.message ?? e})`);
  }
}

/**
 * Decides which secrets to upload. env holds the provided values, existing
 * the names already set on the Worker. Returns {secrets, generated, warnings}.
 */
export function planSecrets(env, existing, vars, random = () => randomBytes(32).toString("hex")) {
  const secrets = {};
  const warnings = [];
  let generated = false;
  for (const name of SECRET_NAMES) {
    const v = env[name];
    if (v) secrets[name] = v;
  }
  if (secrets.BACKYARD_SECRET !== undefined && secrets.BACKYARD_SECRET.length < MIN_SECRET_LEN) {
    throw new CIError(`BACKYARD_SECRET must be at least ${MIN_SECRET_LEN} characters (openssl rand -hex 32)`);
  }
  if (!secrets.BACKYARD_SECRET && !existing.has("BACKYARD_SECRET")) {
    secrets.BACKYARD_SECRET = random();
    generated = true;
  }
  if (secrets.DEPLOY_TOKEN !== undefined && secrets.DEPLOY_TOKEN.length < 16) {
    warnings.push("DEPLOY_TOKEN is shorter than 16 characters; use a long random token (e.g. byd_$(openssl rand -hex 24))");
  }
  const authMode = String(vars.AUTH_MODE ?? "none").toLowerCase();
  const anonymous = String(vars.ALLOW_ANONYMOUS_DEPLOY ?? "").toLowerCase() === "true";
  if (!secrets.DEPLOY_TOKEN && !existing.has("DEPLOY_TOKEN") && authMode !== "access" && !anonymous) {
    warnings.push(
      "No DEPLOY_TOKEN: with AUTH_MODE=none nobody can deploy sites yet. Add a DEPLOY_TOKEN secret " +
        "(e.g. byd_$(openssl rand -hex 24)) and re-run, then: backyard login --server <url> --token <token>",
    );
  }
  return { secrets, generated, warnings };
}

// ---------------------------------------------------------------------------
// Auth guard

/** The managed variables a CI deploy must carry to keep what the live Worker has. */
const KEEP_KEYS = ["AUTH_MODE", "ACCESS_TEAM_DOMAIN", "ACCESS_AUD", "BASE_DOMAIN", "URL_MODE"];

/** A live snapshot that says "unknown" (see readLive in deploy/cloudflare/scripts/lib/live.mjs). */
function unknownLive(error) {
  return { exists: null, vars: {}, secretNames: [], r2Bucket: "", workersDev: null, authMode: "", team: "", aud: "", source: "", error };
}

/** What to do about a refusal of the guard (reason: downgrade, team, fail-closed or unknown). */
function guardRemedy(reason, live) {
  const confirm =
    "If this change is deliberate, run this one deploy with BACKYARD_CF_CONFIRM_AUTH_CHANGE=true (in GitHub Actions: Run workflow with confirm_auth_change checked).";
  if (reason === "unknown") {
    return [
      "  Check that CLOUDFLARE_ACCOUNT_ID is set and that CLOUDFLARE_API_TOKEN may read the Worker (Account · Workers Scripts · Read;",
      '  the "Edit Cloudflare Workers" template has it).',
      `  ${confirm}`,
      "  BACKYARD_CF_VARS must then carry AUTH_MODE, ACCESS_TEAM_DOMAIN and ACCESS_AUD if the Worker uses Cloudflare Access.",
    ].join("\n");
  }
  const keep = {};
  for (const k of KEEP_KEYS) if (live.vars?.[k]) keep[k] = live.vars[k];
  if (!keep.AUTH_MODE && live.authMode === "access") Object.assign(keep, { AUTH_MODE: "access", ACCESS_TEAM_DOMAIN: live.team || "<team>", ACCESS_AUD: live.aud || "<aud>" });
  return [
    `  To keep the live configuration, put it into BACKYARD_CF_VARS: ${JSON.stringify(keep)} (with your other variables).`,
    `  ${confirm}`,
  ].join("\n");
}

/**
 * The auth guard of `backyard cloudflare setup` for CI: reads the live Worker
 * (settings with CLOUDFLARE_API_TOKEN; when unreadable, the Worker's own
 * /__backyard/api/deploy/info on workers.dev) and throws a CIError, before
 * anything is changed, when `desired` would turn a live AUTH_MODE=access off,
 * change its team, open a fail-closed AUTH_MODE, or when the live auth mode
 * cannot be read, unless BACKYARD_CF_CONFIRM_AUTH_CHANGE=true. `wrangler` is
 * what `wrangler secret list` said: {exists} (secrets listed) or {missing}
 * (an explicit "Worker not found": a first deploy, not guarded).
 * @param {{env: NodeJS.ProcessEnv, worker: string, desired: Record<string, string>, wrangler: {exists: boolean, missing: boolean}, log: (m: string) => void, warn: (m: string) => void}} o
 * @returns {Promise<import("../deploy/cloudflare/scripts/lib/live.mjs").Live>}
 */
export async function guardAuth(o) {
  const { env, worker, desired, wrangler } = o;
  const allow = String(env.BACKYARD_CF_CONFIRM_AUTH_CHANGE ?? "").trim().toLowerCase() === "true";
  let base;
  try {
    base = apiBase(env);
  } catch (e) {
    throw new CIError(e?.message ?? String(e));
  }
  const api = createApi({ credential: { type: "api_token", token: env.CLOUDFLARE_API_TOKEN ?? "" }, base, label: "CLOUDFLARE_API_TOKEN" });
  const account = String(env.CLOUDFLARE_ACCOUNT_ID ?? "").trim();
  let live = account ? await readLive({ api, accountId: account, worker }) : unknownLive("CLOUDFLARE_ACCOUNT_ID is not set");
  if (live.exists === false && wrangler.exists) {
    // wrangler sees the Worker but the API does not (another account?): never assume a first deploy.
    live = unknownLive(`the Cloudflare API reports no Worker ${worker} in account ${account}, but wrangler lists its secrets`);
  }
  if (live.exists === null) {
    // The Worker's own answer on its workers.dev URL (<worker>.<account subdomain>.workers.dev).
    let workersDev = "";
    if (account) {
      try {
        const sub = await api.get(`/accounts/${account}/workers/subdomain`);
        if (sub?.subdomain) workersDev = `https://${worker}.${sub.subdomain}.workers.dev`;
      } catch {
        /* unknown: no workers.dev candidate */
      }
    }
    if (workersDev) live = await fillAuthFromInfo(live, [workersDev]);
    // The configured hub URL may belong to anything: it may only make the guard stricter.
    if (live.exists === null) {
      const configured = hubURL(desired, "");
      const other = configured ? await fillAuthFromInfo({ ...live }, [configured]) : live;
      if (other.authMode === "access") live = other;
    }
  }
  if (live.exists === null && wrangler.missing) live = { ...live, exists: false };
  try {
    const g = authGuard(live, desired, { allow, worker });
    for (const n of g.notes) o.warn(n);
    if (g.checked) {
      const team = live.team ? ` (team ${live.team})` : "";
      o.log(`Auth guard: the live Worker runs AUTH_MODE=${live.authMode}${team}; this deploy sets AUTH_MODE=${authModeOf(desired.AUTH_MODE)}.`);
    }
  } catch (e) {
    if (e instanceof CliError && e.code === 3) throw new CIError(`${e.message}\n${guardRemedy(e.reason, live)}`);
    throw e;
  }
  return live;
}

// ---------------------------------------------------------------------------
// Process helpers

function wranglerCommand(cfDir, env) {
  const bin = env.WRANGLER_BIN || join(cfDir, "node_modules", "wrangler", "bin", "wrangler.js");
  if (!existsSync(bin)) throw new CIError(`wrangler not found at ${bin}: run npm ci in ${cfDir} first`);
  return /\.(c|m)?js$/.test(bin) ? [process.execPath, [bin]] : [bin, []];
}

/**
 * Runs wrangler. stdout and stderr are captured (output: both, stdout: only
 * stdout) and streamed unless quiet.
 */
function runWrangler(cfDir, env, args, { quiet = false, extraEnv = {} } = {}) {
  const [cmd, pre] = wranglerCommand(cfDir, env);
  return new Promise((resolvePromise) => {
    const child = spawn(cmd, [...pre, ...args], {
      cwd: cfDir,
      env: { ...env, WRANGLER_SEND_METRICS: env.WRANGLER_SEND_METRICS ?? "false", ...extraEnv },
      stdio: ["ignore", "pipe", "pipe"],
    });
    let output = "";
    let stdout = "";
    child.stdout.on("data", (chunk) => {
      output += chunk.toString();
      stdout += chunk.toString();
      if (!quiet) process.stdout.write(chunk);
    });
    child.stderr.on("data", (chunk) => {
      output += chunk.toString();
      if (!quiet) process.stderr.write(chunk);
    });
    child.on("error", (e) => resolvePromise({ code: 1, output: String(e), stdout: "" }));
    child.on("close", (code) => resolvePromise({ code: code ?? 1, output, stdout }));
  });
}

async function waitReady(url, seconds) {
  const deadline = Date.now() + seconds * 1000;
  while (Date.now() < deadline) {
    try {
      // A Cloudflare Access login redirect is not "ready": do not follow it.
      const res = await fetch(`${url}/__backyard/readyz`, { redirect: "manual", signal: AbortSignal.timeout(10_000) });
      if (res.status === 200) return true;
    } catch {
      // not reachable yet
    }
    await new Promise((r) => setTimeout(r, 3000));
  }
  return false;
}

function gha(env, log = console.log) {
  const on = env.GITHUB_ACTIONS === "true";
  return {
    mask: (v) => on && v && process.stdout.write(`::add-mask::${v}\n`),
    warn: (msg) => log(on ? `::warning title=Backyard on Cloudflare::${msg}` : `warning: ${msg}`),
    output: (k, v) => env.GITHUB_OUTPUT && appendFileSync(env.GITHUB_OUTPUT, `${k}=${v}\n`),
    summary: (text) => env.GITHUB_STEP_SUMMARY && appendFileSync(env.GITHUB_STEP_SUMMARY, text),
  };
}

// ---------------------------------------------------------------------------
// deploy

/** deploy runs the whole CI deploy. It returns the hub URL (empty for a dry run). */
export async function deploy({ env = process.env, dryRun = false, log = console.log } = {}) {
  const cfDir = resolve(env.BACKYARD_CF_DIR || join(repoRoot, "deploy", "cloudflare"));
  const configPath = join(cfDir, "wrangler.jsonc");
  if (!existsSync(configPath)) throw new CIError(`no Worker project at ${cfDir} (wrangler.jsonc is missing)`);
  const config = parseJSONC(readFileSync(configPath, "utf8"));
  const worker = (env.BACKYARD_CF_WORKER_NAME || config.name || "backyard").trim();
  if (!WORKER_NAME_RE.test(worker)) throw new CIError(`invalid Worker name ${JSON.stringify(worker)}`);
  const bucket = (env.BACKYARD_CF_R2_BUCKET || "").trim();
  if (bucket && !BUCKET_RE.test(bucket)) throw new CIError(`invalid R2 bucket name ${JSON.stringify(bucket)}`);
  const ai = parseAISwitch(env.BACKYARD_CF_AI);
  const listing = parseSiteListing(env.BACKYARD_CF_SITE_LISTING);
  const vars = parseVars(env.BACKYARD_CF_VARS);
  const extraArgs = parseDeployArgs(env.BACKYARD_CF_DEPLOY_ARGS);
  const argVars = deployArgVars(extraArgs);
  // With AI off the switch owns AI_ENABLED, so the binding and the variable never disagree.
  if (!ai && ("AI_ENABLED" in vars || "AI_ENABLED" in argVars)) {
    throw new CIError("AI_ENABLED is set by BACKYARD_CF_AI=false; remove it from BACKYARD_CF_VARS and BACKYARD_CF_DEPLOY_ARGS");
  }
  // The listing switch owns SITE_LISTING, like `setup --site-listing` does.
  if ("SITE_LISTING" in vars || "SITE_LISTING" in argVars) {
    throw new CIError('SITE_LISTING is set by BACKYARD_CF_SITE_LISTING ("owners", or "all"/empty for every site); remove it from BACKYARD_CF_VARS and BACKYARD_CF_DEPLOY_ARGS');
  }
  // A SITE_LISTING in wrangler.jsonc would outlive BACKYARD_CF_SITE_LISTING=all (only owners is
  // added), so one the Worker reads as another listing is refused instead of deployed.
  const baseListing = config.vars?.SITE_LISTING;
  const baseText = typeof baseListing === "string" ? baseListing : baseListing === undefined || baseListing === null ? "" : JSON.stringify(baseListing);
  if (baseText.trim() !== "" && siteListingOf(baseText) !== listing) {
    throw new CIError(`wrangler.jsonc sets SITE_LISTING=${JSON.stringify(baseText)}, but the site listing of this deploy is ${listing} (BACKYARD_CF_SITE_LISTING; empty means all): remove SITE_LISTING from wrangler.jsonc and choose with BACKYARD_CF_SITE_LISTING`);
  }
  // Owners are recorded only with OWNERSHIP=owner (the Worker's default with SITE_LISTING=owners;
  // any other value makes it refuse every request).
  if (listing === "owners") {
    const [where, own] = "OWNERSHIP" in argVars ? ["BACKYARD_CF_DEPLOY_ARGS", argVars.OWNERSHIP] : "OWNERSHIP" in vars ? ["BACKYARD_CF_VARS", vars.OWNERSHIP] : ["wrangler.jsonc", config.vars?.OWNERSHIP];
    const o = String(own ?? "").trim().toLowerCase();
    if (o !== "" && o !== "owner") {
      throw new CIError(`BACKYARD_CF_SITE_LISTING=owners needs OWNERSHIP=owner (its default then), but ${where} sets OWNERSHIP to ${JSON.stringify(String(own))}: remove it there or set it to "owner"`);
    }
  }
  // What the Worker will run with: wrangler.jsonc < BACKYARD_CF_VARS < --var in BACKYARD_CF_DEPLOY_ARGS.
  const merged = { ...(config.vars ?? {}), ...vars, ...argVars, ...(ai ? {} : { AI_ENABLED: "false" }), ...(listing === "owners" ? { SITE_LISTING: "owners" } : {}) };
  const ci = gha(env, log);
  for (const name of SECRET_NAMES) ci.mask(env[name]);
  // wrangler uses HTTPS_PROXY; the auth guard's API calls and the readiness check only with NODE_USE_ENV_PROXY=1.
  const proxy = proxyHint(env);
  if (proxy) ci.warn(proxy);

  const work = mkdtempSync(join(env.RUNNER_TEMP || tmpdir(), "backyard-cf-"));
  const generatedPath = join(cfDir, GENERATED_CONFIG);
  let wroteConfig = false;
  try {
    if (!dryRun && !env.CLOUDFLARE_API_TOKEN) {
      throw new CIError("set CLOUDFLARE_API_TOKEN (and CLOUDFLARE_ACCOUNT_ID): an API token from the \"Edit Cloudflare Workers\" template");
    }

    // Secrets: keep what the Worker has, generate BACKYARD_SECRET on the first deploy.
    let plan = { secrets: {}, generated: false, warnings: [] };
    if (!dryRun) {
      const listed = await runWrangler(cfDir, env, ["secret", "list", "--name", worker, "--format", "json"], { quiet: true });
      let existing;
      let missing = false;
      if (listed.code === 0) {
        existing = parseSecretList(listed.stdout);
      } else if (isMissingWorker(listed.output)) {
        log(`Worker ${worker} does not exist yet: first deploy.`);
        existing = new Set();
        missing = true;
      } else {
        // Never guess here: a wrong guess would rotate BACKYARD_SECRET and
        // revoke every API token.
        throw new CIError(`could not list the secrets of Worker ${worker}:\n${listed.output.trim()}`);
      }
      // Before anything changes (R2 bucket, secrets, the new version).
      await guardAuth({ env, worker, desired: merged, wrangler: { exists: !missing, missing }, log, warn: ci.warn });
      plan = planSecrets(env, existing, merged);
      ci.mask(plan.secrets.BACKYARD_SECRET);
      for (const w of plan.warnings) ci.warn(w);
      if (plan.generated) log("Generated a new BACKYARD_SECRET (first deploy; stored only on the Worker).");
    }

    if (bucket && !dryRun) {
      const r = await runWrangler(cfDir, env, ["r2", "bucket", "create", bucket], { quiet: true });
      if (r.code !== 0 && !/already exists|already own|\b10004\b/i.test(r.output)) {
        throw new CIError(`could not create the R2 bucket ${bucket} (is R2 enabled on the account, and may the token edit R2?):\n${r.output.trim()}`);
      }
      log(`R2 bucket ${bucket} ready.`);
    }

    if (hasRoutes(config)) {
      ci.warn("routes in wrangler.jsonc are ignored: routes and custom domains are managed by `backyard cloudflare domain add` (or Terraform), and deploys never change them");
    }
    const effective = effectiveConfig(config, { vars, bucket, ai, listing });
    // --env= selects the top-level environment explicitly (wrangler.jsonc also
    // defines "local" for offline development).
    const args = ["deploy", "--name", worker, "--env="];
    if (effective) {
      writeFileSync(generatedPath, `// Generated by scripts/cloudflare-ci.mjs from wrangler.jsonc; do not edit.\n${JSON.stringify(effective, null, 2)}\n`);
      wroteConfig = true;
      args.push("--config", generatedPath);
      if (Object.keys(vars).length) log(`Variable overrides: ${Object.keys(vars).sort().join(", ")}`);
    }
    if (listing === "owners") log("Site listing: owners only (BACKYARD_CF_SITE_LISTING=owners): each site is listed to its owners and admins.");
    if (dryRun) {
      args.push("--dry-run", "--outdir", join(work, "bundle"));
    } else if (Object.keys(plan.secrets).length) {
      const secretsFile = join(work, "secrets.json");
      writeFileSync(secretsFile, JSON.stringify(plan.secrets), { mode: 0o600 });
      args.push("--secrets-file", secretsFile);
      log(`Secrets uploaded with this version: ${Object.keys(plan.secrets).sort().join(", ")}`);
    }
    args.push(...extraArgs);

    const outputFile = join(work, "wrangler-output.ndjson");
    log(`\n${dryRun ? "Bundling" : "Deploying"} Worker ${worker}${bucket ? ` with R2 bucket ${bucket}` : ""}…\n`);
    const res = await runWrangler(cfDir, env, args, { extraEnv: { WRANGLER_OUTPUT_FILE_PATH: outputFile } });
    if (res.code !== 0) {
      if (/workers\.dev subdomain/i.test(res.output)) {
        throw new CIError("register a workers.dev subdomain first: dash.cloudflare.com → Workers & Pages, then re-run");
      }
      throw new CIError("wrangler deploy failed (see the output above)");
    }
    if (dryRun) {
      log("Dry run: the Worker bundles and the configuration is valid.");
      return "";
    }

    const url = hubURL(merged, existsSync(outputFile) ? readFileSync(outputFile, "utf8") : "");
    ci.output("worker", worker);
    ci.output("url", url);
    const waitSeconds = Number(env.BACKYARD_CF_WAIT_SECONDS ?? 60);
    let ready = false;
    if (url && waitSeconds > 0) {
      ready = await waitReady(url, waitSeconds);
      if (!ready) ci.warn(`${url}/__backyard/readyz did not answer 200 yet (a new workers.dev URL can take a minute, or Access blocks it)`);
    }
    log(`\nBackyard ${ready ? "is live" : "was deployed"}${url ? ` at ${url}` : ""}.`);
    const lines = [
      "### Backyard on Cloudflare",
      "",
      `Worker \`${worker}\` deployed${url ? `: ${url}` : ""}.`,
      "",
    ];
    if (url) {
      lines.push("Connect the CLI with your deploy token (the `DEPLOY_TOKEN` secret):", "", "```sh", `backyard login --server ${url} --token <DEPLOY_TOKEN>`, "```", "");
    }
    for (const w of plan.warnings) lines.push(`> **Warning:** ${w}`, "");
    ci.summary(lines.join("\n") + "\n");
    return url;
  } finally {
    if (wroteConfig) rmSync(generatedPath, { force: true });
    rmSync(work, { recursive: true, force: true });
  }
}

const USAGE = `usage: node scripts/cloudflare-ci.mjs deploy [--dry-run]

Deploys deploy/cloudflare to Cloudflare from CI. Configuration comes from the
environment; see the comment at the top of this file.`;

export async function main(argv = process.argv.slice(2)) {
  const [cmd, ...rest] = argv;
  if (cmd === "-h" || cmd === "--help" || cmd === "help") {
    console.log(USAGE);
    return 0;
  }
  if (cmd !== "deploy") {
    console.error(USAGE);
    return 2;
  }
  const unknown = rest.filter((a) => a !== "--dry-run");
  if (unknown.length) {
    console.error(`unknown option ${unknown[0]}\n\n${USAGE}`);
    return 2;
  }
  try {
    await deploy({ dryRun: rest.includes("--dry-run") });
    return 0;
  } catch (e) {
    if (e instanceof CIError) {
      if (process.env.GITHUB_ACTIONS === "true") {
        console.log(`::error title=Backyard on Cloudflare::${e.message.split("\n")[0]}`);
      }
      console.error(`error: ${e.message}`);
      return 1;
    }
    throw e;
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = await main();
}
