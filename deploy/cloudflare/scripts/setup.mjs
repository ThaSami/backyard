#!/usr/bin/env node
// `backyard-cloudflare setup`: deploys (or upgrades) Backyard on your
// Cloudflare account, free plan included. Safe to re-run: every choice is kept
// (secrets, the deploy token, R2, AI, the site listing, the domain, Access, --var
// overrides), and a machine without state adopts the deployed configuration first.
//
//   setup                          log in (browser) if needed, register the account's
//                                  workers.dev subdomain if it has none, deploy, set secrets
//                                  (and an R2 bucket for big files when R2 is enabled)
//   setup --no-r2                  never use R2 (files capped at 25 MiB)
//   setup --no-ai                  turn AI off: no Workers AI binding, AI_ENABLED=false (--ai: on again)
//   setup --site-listing owners    list each site only to its owners and admins: SITE_LISTING=owners,
//                                  which implies OWNERSHIP=owner (--site-listing all: every site again)
//   setup --worker-name team       deploy under another Worker name
//   setup --var LIMITS:'{"ai_per_day":50}'   override a variable (remembered; KEY: drops it)
//   setup --rotate-token           issue a new DEPLOY_TOKEN
//   setup --dry-run                bundle and validate only (offline, no login, no changes)
//
// A custom domain and company login come afterwards: `domain add <host>` and
// `access enable` (scripts/domain.mjs, scripts/access.mjs).
//
// Non-interactive use (CI): set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID
// (and --workers-subdomain for an account without one). State is kept in
// $XDG_CONFIG_HOME/backyard/cloudflare/<worker>.json (mode 0600).
//
// Secrets are never guessed: BACKYARD_SECRET is generated only when wrangler
// clearly reports that the Worker has none (or does not exist yet), since a new
// one revokes every API token; any other failure to list the secrets stops the
// setup. Generated secrets are printed only to an interactive terminal; in CI
// (CI=true) or when the output is redirected, the deploy token is masked and
// only written to the state file. BACKYARD_SECRET is never printed. A deploy
// that would turn a live Cloudflare Access off is refused unless
// --confirm-auth-change.
//
// `node scripts/setup.mjs <args>` is the same as `backyard-cloudflare setup <args>`.

import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { GO_CLI_INVOCATION } from "./cli.mjs";
import { CfError } from "./lib/cfapi.mjs";
import { MANAGED_ACCESS_KEYS, MANAGED_AI_KEYS, MANAGED_DOMAIN_KEYS, MANAGED_LISTING_KEYS, aiEnabledOf, authModeOf, hostOf, refuseListingVars, rememberedListing } from "./lib/live.mjs";
import { probe } from "./lib/net.mjs";
import { CliError, EXIT, bold, cyan, dim, green, isCI, maskSecret, revealSecrets, ui, yellow } from "./lib/ui.mjs";

export { parseJSONC } from "./lib/layout.mjs";
export { isCI, maskSecret, revealSecrets } from "./lib/ui.mjs";
export { isMissingWorker, parseSecretList } from "./lib/wrangler.mjs";

const BUCKET_RE = /^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/;
const VAR_RE = /^([A-Z_][A-Z0-9_]*):([\s\S]*)$/;
/** A workers.dev subdomain (wrangler's rule). */
export const SUBDOMAIN_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/**
 * The default workers.dev subdomain for an account name: lowercased, runs of
 * anything but a-z and 0-9 replaced by "-", trimmed, at most 63 characters.
 */
export function suggestSubdomain(accountName) {
  return String(accountName ?? "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+/, "")
    .slice(0, 63)
    .replace(/-+$/, "");
}

/**
 * Secrets of the Worker (src/env.ts): never a --var, which would put the value
 * on the command line, into a plain-text variable everyone with dashboard
 * access sees, and into the state file (scripts/cloudflare-ci.mjs refuses them too).
 */
export const SECRET_NAMES = ["BACKYARD_SECRET", "DEPLOY_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "WAREHOUSE_TOKEN"];

/** The refusal of a secret given with --var (the value is never echoed). */
function secretVarError(name, worker) {
  const how =
    name === "DEPLOY_TOKEN"
      ? "pass it as the environment variable DEPLOY_TOKEN=<token> (or --rotate-token for a new one)"
      : name === "BACKYARD_SECRET"
        ? "setup generates it once (--rotate-secret replaces it)"
        : `set it with \`npx wrangler secret put ${name} --name ${worker}\` (setup keeps it)`;
  return new CliError(EXIT.USAGE, `${name} is a secret, not a variable: never pass it with --var; ${how}`);
}

/** Parses the --var flags ({KEY: value}; "" drops a key). */
function parseVarFlags(list, worker) {
  const out = {};
  for (const v of list ?? []) {
    const m = VAR_RE.exec(v);
    if (!m) {
      // Never echo the value: it may be a secret typed as KEY=VALUE.
      const key = /^([A-Za-z_][A-Za-z0-9_]*)[=:]/.exec(v)?.[1] ?? "";
      if (SECRET_NAMES.includes(key.toUpperCase())) throw secretVarError(key.toUpperCase(), worker);
      throw new CliError(EXIT.USAGE, key ? `--var expects KEY:VALUE (with ":"), got ${JSON.stringify(key)} followed by a value` : "--var expects KEY:VALUE");
    }
    if (SECRET_NAMES.includes(m[1]) && m[2] !== "") throw secretVarError(m[1], worker);
    out[m[1]] = m[2];
  }
  return out;
}

/** Refuses --var for a key that `domain add` / `access enable` manage right now. */
function refuseManagedVars(overrides, state, inv) {
  for (const k of Object.keys(overrides)) {
    if (state.domain && MANAGED_DOMAIN_KEYS.includes(k)) {
      throw new CliError(EXIT.USAGE, `${k} is managed by \`${inv} domain add\` (use \`${inv} domain remove\`)`);
    }
    if (state.access && MANAGED_ACCESS_KEYS.includes(k)) {
      throw new CliError(EXIT.USAGE, `${k} is managed by \`${inv} access enable\` (use \`${inv} access disable\`)`);
    }
  }
}

/**
 * Refuses --var AI_ENABLED while AI is off (`setup --no-ai`, remembered):
 * AI_ENABLED is managed then ("false", with no AI binding), so the variable
 * can never say AI is on while the Worker has no binding.
 */
function refuseAIVar(overrides, aiOff, inv) {
  for (const k of MANAGED_AI_KEYS) {
    if (aiOff && Object.hasOwn(overrides, k)) {
      throw new CliError(EXIT.USAGE, `${k} is managed by \`${inv} setup --no-ai\` while AI is off (\`${inv} setup --ai\` turns AI back on)`);
    }
  }
}

/**
 * A domain or Access adopted in this very run whose keys are also given with
 * --var stays "externally managed" (e.g. the Terraform module's setup_args on
 * a second machine): its live values go to state.vars, where --var applies.
 */
function externalizeAdopted(state, overrides, baseVars) {
  const keep = (k, v) => {
    const def = String(baseVars[k] ?? "");
    if (v !== def) state.vars = { ...(state.vars ?? {}), [k]: v };
  };
  const given = Object.keys(overrides);
  if (state.domain?.source === "adopted" && given.some((k) => MANAGED_DOMAIN_KEYS.includes(k))) {
    keep("BASE_DOMAIN", state.domain.host);
    keep("URL_MODE", state.domain.mode === "host" ? "host" : "path");
    delete state.domain;
  }
  if (state.access?.source === "adopted" && given.some((k) => MANAGED_ACCESS_KEYS.includes(k))) {
    keep("AUTH_MODE", "access");
    keep("ACCESS_TEAM_DOMAIN", state.access.team ?? "");
    keep("ACCESS_AUD", state.access.aud ?? "");
    delete state.access;
  }
}

/**
 * Resolves the workers.dev URL of the Worker: the account's subdomain, which
 * is registered when there is none (never renamed). "" when the credential
 * cannot read it (a scoped API token): the deploy output tells then. With
 * {register: false} (workers.dev is off or being turned off) it only looks
 * the URL up, best effort: no registration, no prompt, no failure.
 * @param {import("./cli.mjs").Ctx} ctx
 * @param {import("./lib/cfapi.mjs").Api} api
 * @param {Record<string, any>} opts
 * @param {{register?: boolean}} [ro]
 */
async function resolveWorkersDev(ctx, api, opts, ro = {}) {
  const register = ro.register !== false;
  let sub;
  try {
    sub = (await api.get(`/accounts/${ctx.accountId}/workers/subdomain`))?.subdomain;
  } catch (e) {
    if (!register) return "";
    if (e instanceof CfError && e.has(10007)) {
      sub = await registerSubdomain(ctx, api, opts);
    } else if (e instanceof CfError && e.forbidden) {
      ui.note("The credential cannot read the account's workers.dev subdomain: the URL comes from the deploy output.");
      return "";
    } else {
      throw new CliError(EXIT.FAILED, `could not read the workers.dev subdomain of the account (nothing was changed): ${e?.message ?? e}`);
    }
  }
  if (!sub) return "";
  if (opts.workersSubdomain && opts.workersSubdomain !== sub) ui.warn(`account already uses ${sub}; --workers-subdomain ignored`);
  return `https://${ctx.worker}.${sub}.workers.dev`;
}

/**
 * Registers the account's workers.dev subdomain (the endpoints and error
 * codes wrangler uses: 10032 available, 10031 taken).
 * @param {import("./cli.mjs").Ctx} ctx
 * @param {import("./lib/cfapi.mjs").Api} api
 * @param {Record<string, any>} opts
 */
async function registerSubdomain(ctx, api, opts) {
  const a = ctx.accountId;
  const onboarding = `https://dash.cloudflare.com/${a}/workers/onboarding`;
  let name = opts.workersSubdomain ?? "";
  if (!name && !ctx.interactive) {
    throw new CliError(EXIT.USAGE, `this account has no workers.dev subdomain: pass --workers-subdomain <name>, or register one at ${onboarding}`);
  }
  const suggestion = suggestSubdomain(ctx.accountName);
  if (!name) ui.log("This Cloudflare account has no workers.dev subdomain yet (Workers are served at https://<worker>.<subdomain>.workers.dev).");
  for (;;) {
    if (!name) {
      name = (await ctx.prompt.ask("workers.dev subdomain for this account", { default: suggestion, flag: "--workers-subdomain" })).toLowerCase();
    }
    if (!SUBDOMAIN_RE.test(name)) {
      if (!ctx.interactive) throw new CliError(EXIT.USAGE, `invalid workers.dev subdomain ${JSON.stringify(name)} (a-z, 0-9 and -, at most 63 characters)`);
      ui.warn(`${JSON.stringify(name)} is not a valid subdomain (a-z, 0-9 and -, at most 63 characters)`);
      name = "";
      continue;
    }
    let available = true;
    try {
      await api.get(`/accounts/${a}/workers/subdomains/${name}`);
    } catch (e) {
      if (e instanceof CfError && e.has(10032)) available = true;
      else if (e instanceof CfError && e.has(10031)) available = false;
      else throw new CliError(EXIT.FAILED, `could not check whether the workers.dev subdomain ${name} is available: ${e?.message ?? e}`);
    }
    if (available) {
      try {
        const r = await api.put(`/accounts/${a}/workers/subdomain`, { subdomain: name });
        const got = String(r?.subdomain || name);
        ui.ok(`workers.dev subdomain ${bold(got)} registered (DNS can take a few minutes)`);
        return got;
      } catch (e) {
        if (!(e instanceof CfError && e.has(10031))) throw new CliError(EXIT.FAILED, `could not register the workers.dev subdomain ${name}: ${e?.message ?? e}`);
      }
    }
    if (!ctx.interactive) throw new CliError(EXIT.FAILED, `the workers.dev subdomain ${name} is taken: pick another with --workers-subdomain <name> (or register one at ${onboarding})`);
    ui.warn(`${name}.workers.dev is taken; please choose another`);
    name = "";
  }
}

/** How to get the `backyard` CLI (printed when the Go CLI did not run the engine). */
export const CLI_INSTALL = "curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh";

/**
 * Polls <url>/__backyard/readyz (15 × 3 s): "" when ready, else why not,
 * with the Worker's own reason when it answers (e.g. "not ready: AUTH_MODE
 * must be …").
 * @param {string} url
 */
async function waitReady(url) {
  let last;
  for (let i = 0; i < 15; i++) {
    // manual: a Cloudflare Access login redirect is not "ready".
    last = await probe(`${url}/__backyard/readyz`, { timeoutMs: 10_000 });
    if (last.ok && last.status === 200) return "";
    if (i < 14) await new Promise((res) => setTimeout(res, 3000));
  }
  if (!last.ok) {
    return last.kind === "dns" || last.kind === "certificate"
      ? "DNS or the certificate is still provisioning (a new workers.dev subdomain can take a few minutes)"
      : `it does not answer (${last.error})`;
  }
  if (last.status >= 300 && last.status < 400) return `/__backyard/readyz redirects (${last.location || `HTTP ${last.status}`})`;
  const reason = String(last.text ?? "").split("\n")[0].replace(/[\x00-\x1f\x7f]/g, "").trim().slice(0, 300);
  return `/__backyard/readyz answers HTTP ${last.status}${reason ? `: ${reason}` : ""}`;
}

/**
 * `setup`: from nothing to a live Worker, and every later upgrade.
 * @param {Record<string, any>} opts   parsed flags (lib/args.mjs)
 * @param {import("./cli.mjs").Ctx} ctx
 * @returns {Promise<number>}
 */
export async function runSetup(opts, ctx) {
  const inv = ctx.invokedAs;
  const w = ctx.worker;
  const overrides = parseVarFlags(opts.var, w);
  if (opts.r2 && opts.noR2) throw new CliError(EXIT.USAGE, "--r2 and --no-r2 contradict each other");
  if (opts.ai && opts.noAi) throw new CliError(EXIT.USAGE, "--ai and --no-ai contradict each other");
  if (opts.noR2 && opts.r2Bucket) throw new CliError(EXIT.USAGE, "--r2-bucket and --no-r2 contradict each other");
  if (opts.workersDev && opts.noWorkersDev) throw new CliError(EXIT.USAGE, "--workers-dev and --no-workers-dev contradict each other");
  if (opts.r2Bucket && !BUCKET_RE.test(opts.r2Bucket)) throw new CliError(EXIT.USAGE, `invalid bucket name ${JSON.stringify(opts.r2Bucket)}`);
  if (opts.workersSubdomain !== undefined && !SUBDOMAIN_RE.test(opts.workersSubdomain)) {
    throw new CliError(EXIT.USAGE, `invalid workers.dev subdomain ${JSON.stringify(opts.workersSubdomain)} (a-z, 0-9 and -, at most 63 characters)`);
  }
  if (opts.siteListing !== undefined && opts.siteListing !== "all" && opts.siteListing !== "owners") {
    throw new CliError(EXIT.USAGE, `--site-listing must be all or owners (got ${JSON.stringify(opts.siteListing)})`);
  }
  if (opts.dryRun && (opts.skipDeploy || opts.rotateToken || opts.rotateSecret)) {
    throw new CliError(EXIT.USAGE, "--dry-run changes nothing: it cannot be combined with --skip-deploy, --rotate-token or --rotate-secret");
  }
  /** @type {boolean | undefined} */
  const r2Choice = opts.noR2 ? false : opts.r2 || opts.r2Bucket ? true : undefined;
  /** @type {boolean | undefined} */
  const aiChoice = opts.noAi ? false : opts.ai ? true : undefined;
  /** Whether AI is off for this run: --no-ai, or remembered and no --ai. */
  const aiOff = (state) => aiChoice === false || (aiChoice === undefined && state.ai === false);
  /** @type {"all" | "owners" | undefined} */
  const listingChoice = opts.siteListing;
  /** The site listing of this run: --site-listing, else the remembered one. */
  const listingOf = (state) => listingChoice ?? rememberedListing(state);
  const baseVars = ctx.base.vars ?? {};

  if (opts.dryRun) {
    await ctx.syncAssets();
    const state = ctx.loadState();
    refuseManagedVars(overrides, state, inv);
    refuseAIVar(overrides, aiOff(state), inv);
    refuseListingVars(overrides, listingOf(state), state, baseVars, inv);
    const vars = { ...(state.vars ?? {}) };
    for (const [k, v] of Object.entries(overrides)) {
      if (v === "") delete vars[k];
      else vars[k] = v;
    }
    for (const k of MANAGED_LISTING_KEYS) delete vars[k];
    const view = { ...state, vars, ai: aiOff(state) ? false : undefined, siteListing: listingOf(state) === "owners" ? "owners" : undefined };
    if (opts.workersDev) view.workersDev = true;
    if (opts.noWorkersDev) view.workersDev = false;
    const bucket = r2Choice === false ? "" : opts.r2Bucket || (state.r2 !== false ? state.r2Bucket : "") || (r2Choice ? `${w}-files` : "");
    return ctx.dryRun(view, { bucket });
  }

  ui.log(bold("\nBackyard on Cloudflare — setup\n"));
  // Source layout: bundle the SDK, hub and docs from the checkout.
  await ctx.syncAssets();
  const state = ctx.loadState();
  const previousUrl = state.url ?? "";
  refuseManagedVars(overrides, state, inv);
  refuseAIVar(overrides, aiOff(state), inv);
  // Without the flag a machine without state may still adopt the listing from
  // the live Worker: the base SITE_LISTING is checked against it below.
  refuseListingVars(overrides, listingOf(state), state, baseVars, inv, { checkBase: listingChoice !== undefined || !!state.updatedAt });

  await ctx.login(state);
  const api = await ctx.api();

  // The live Worker: adopted by a machine without state, checked by the guard.
  const live = await ctx.readLive();
  if (ctx.adopt(state, live)) externalizeAdopted(state, overrides, baseVars);
  // An adopted --no-ai makes AI_ENABLED managed, and an adopted owners-only
  // listing (or OWNERSHIP) is checked like a remembered one: refuse a --var
  // that contradicts them now, before the workers.dev subdomain or the R2
  // bucket is touched.
  refuseAIVar(overrides, aiOff(state), inv);
  refuseListingVars(overrides, listingOf(state), state, baseVars, inv);

  // The account's workers.dev subdomain (registered when there is none), once
  // the adopted setting and the flags are known: a Worker that does not serve
  // workers.dev never needs one, its URL is only looked up for later.
  if (opts.workersDev) state.workersDev = true;
  const servesWorkersDev = !opts.noWorkersDev && state.workersDev !== false;
  if (servesWorkersDev || !state.workersDevUrl) {
    const workersDevUrl = await resolveWorkersDev(ctx, api, opts, { register: servesWorkersDev });
    if (workersDevUrl) state.workersDevUrl = workersDevUrl;
  }

  // R2 is used automatically when the account has it (like OpenQuick); --no-r2
  // opts out for good, --r2 opts back in. A failed attempt is retried next run,
  // and never drops or forgets a bucket in use: files above FILE_INLINE_MAX live there.
  if (r2Choice !== undefined) state.r2 = r2Choice;
  let bucket = "";
  if (state.r2 !== false) {
    const known = opts.r2Bucket || state.r2Bucket || live.r2Bucket || "";
    const name = known || `${w}-files`;
    const created = await ctx.ensureBucket(name);
    if (created.ok) bucket = name;
    else if (live.r2Bucket && live.r2Bucket === name) {
      // Bound to the live Worker, so it exists: a failed create (a transient
      // error, a token without R2 permission) must not unbind it.
      ui.warn(`could not confirm the R2 bucket ${name}: keeping it bound (the Worker already uses it)`);
      bucket = name;
    } else {
      ui.warn(
        created.notEnabled
          ? `R2 is not enabled on this account (enable it once at dash.cloudflare.com → R2 — the free tier includes 10 GB — then re-run ${inv} setup).`
          : `Could not create the R2 bucket ${name} (re-run ${inv} setup to try again).`,
      );
      ui.note("  Continuing without R2: everything works, files are just capped at 25 MiB each.");
    }
    // Only --no-r2 forgets the bucket: a failed attempt keeps the known name for the next run.
    state.r2Bucket = bucket || known || undefined;
  } else {
    state.r2Bucket = undefined;
  }

  // AI is on unless --no-ai (remembered as "ai": false); --ai turns it back
  // on. While it is off the deploy has no Workers AI binding and AI_ENABLED is
  // managed ("false", see managedVars), so no override of it is kept (a --var
  // for it was refused above, right after adoption).
  state.ai = aiOff(state) ? false : undefined;

  // Owners-only site listing (--site-listing owners, remembered as "siteListing":
  // "owners"; --site-listing all removes it). SITE_LISTING is managed (see
  // managedVars), so no override of it is kept (a contradicting --var was
  // refused above).
  const listing = listingOf(state);
  state.siteListing = listing === "owners" ? "owners" : undefined;

  // Variable overrides: remembered across runs, an empty value drops one.
  refuseManagedVars(overrides, state, inv);
  const vars = { ...(state.vars ?? {}) };
  for (const [k, v] of Object.entries(overrides)) {
    if (v === "") delete vars[k];
    else vars[k] = v;
  }
  if (state.ai === false) {
    for (const k of MANAGED_AI_KEYS) {
      if (vars[k] === undefined) continue;
      delete vars[k];
      ui.note(`Dropped the ${k} override: with AI off (--no-ai) setup sets ${k}=false itself.`);
    }
  }
  for (const k of MANAGED_LISTING_KEYS) {
    if (vars[k] === undefined) continue;
    delete vars[k];
    ui.note(`Dropped the ${k} override: setup --site-listing sets ${k} itself (the site listing is ${listing}).`);
  }
  state.vars = Object.keys(vars).length ? vars : undefined;
  if (opts.noWorkersDev) {
    if (state.domain?.status !== "active") throw new CliError(EXIT.USAGE, `turning workers.dev off needs a working custom domain (${inv} domain add <host>)`);
    state.workersDev = false;
  }
  state.workersDev = state.workersDev ?? true;

  const d = await ctx.deploy(state, {
    live,
    bucket,
    confirmAuthChange: opts.confirmAuthChange,
    rotateToken: opts.rotateToken,
    rotateSecret: opts.rotateSecret,
    skipDeploy: opts.skipDeploy,
  });

  // The hub URL to hand out: PUBLIC_URL, the custom domain, workers.dev.
  const publicURL = String(d.vars.PUBLIC_URL ?? "").trim().replace(/\/+$/, "");
  const baseDomain = hostOf(d.vars.BASE_DOMAIN);
  if (!state.workersDevUrl && /^https:\/\/[^/]+\.workers\.dev$/.test(d.url)) state.workersDevUrl = d.url;
  const url =
    publicURL || (baseDomain ? `https://${baseDomain}` : "") || (state.workersDev !== false ? state.workersDevUrl ?? "" : "") || d.url || state.url || "";
  state.url = url || undefined;
  const saved = ctx.saveState(state);
  ctx.writeResult(state, { previousUrl });

  let notReady = "";
  if (url) {
    process.stdout.write(`\nWaiting for ${url} to answer…`);
    notReady = await waitReady(url);
    ui.log(notReady ? ` ${yellow("not yet")}` : ` ${green("ready")}`);
  }

  const hub = url || "https://<your-worker>.<your-subdomain>.workers.dev";
  const token = d.token;
  // The Go CLI (BACKYARD_CF_CLI_LOGIN=1) logs in with the deploy token itself and prints the next step.
  const wrapperLogsIn = process.env.BACKYARD_CF_CLI_LOGIN === "1" && Boolean(token);
  if (notReady) {
    ui.log(`\n${yellow("!")} Deployed to ${bold(cyan(hub))}, but it is not ready: ${notReady}.`);
    ui.log(`  Check with ${inv} status; re-run ${inv} setup once it is fixed.\n`);
  } else {
    ui.log(`\n${green("✔")} Backyard is live at ${bold(cyan(hub))}\n`);
  }
  if (!token) {
    // The Worker keeps a deploy token this machine does not know.
    ui.log("Connect the CLI with the Worker's existing deploy token (not known on this machine):\n");
    ui.log(`  ${bold(`backyard login --server ${hub} --token <DEPLOY_TOKEN>`)}\n`);
  } else if (revealSecrets()) {
    ui.log(`Publish in the browser: ${bold(`${hub}/#token=${token}`)}   ${dim("(the hub asks before using it)")}\n`);
    if (!wrapperLogsIn) {
      ui.log("Or connect the CLI (keep the token secret — it can deploy and delete sites):\n");
      ui.log(`  ${bold(`backyard login --server ${hub} --token ${token}`)}\n`);
    }
  } else if (!wrapperLogsIn) {
    // CI logs are kept and shared: the token stays in the state file (0600).
    const where = process.env.DEPLOY_TOKEN ? "$DEPLOY_TOKEN" : `<"deployToken" in ${saved}>`;
    const why = isCI() ? "CI" : "the output is not a terminal";
    ui.log(`Connect the CLI with the deploy token ${maskSecret(token)} (not printed: ${why}):\n`);
    ui.log(`  ${bold(`backyard login --server ${hub} --token ${where}`)}\n`);
  }
  if (!wrapperLogsIn) {
    if (inv !== GO_CLI_INVOCATION) ui.note(`No backyard CLI yet? ${CLI_INSTALL}`);
    ui.log(`Then: ${bold("backyard init my-site && cd my-site && backyard deploy")}`);
  }
  if (token) ui.note(`\nThe deploy token is saved in ${saved} (mode 0600).`);
  if (state.ai === false) ui.note("AI is off (--ai turns it on): the AI endpoints answer 501 unsupported.");
  else if (!aiEnabledOf(d.vars.AI_ENABLED)) ui.note("AI is off: AI_ENABLED is false (--var AI_ENABLED: drops it); the Workers AI binding stays.");
  else ui.note("AI runs on Workers AI (free daily allowance); caps: --var LIMITS:'{\"ai_per_day\":50}'.");
  if (state.siteListing === "owners") {
    const guests = authModeOf(d.vars.AUTH_MODE) === "access" ? "" : " Without company login visitors are anonymous guests and see no sites.";
    ui.note(`Site listing: owners only (--site-listing all lists every site).${guests}`);
  }
  const noR2 = state.r2 === false ? "R2 is off (--r2 turns it on)" : `enable R2 on the account and re-run ${inv} setup for files up to 95 MiB`;
  ui.note(`Files: ${bucket ? `R2 bucket ${bucket} for files above FILE_INLINE_MAX` : `stored in Durable Objects (25 MiB per file); ${noR2}`}.`);
  if (!state.domain) ui.note(`Your own domain: ${inv} domain add <host>`);
  else if (!state.access) ui.note(`Company login: ${inv} access enable --allow-email-domain <your-domain>`);
  ui.log("");
  // Applied, but not verified (design: exit 4); the result file and the login lines are there.
  return notReady ? EXIT.PENDING : EXIT.OK;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  // No top-level await: cli.mjs imports this module to run the command.
  import("./cli.mjs")
    .then(({ main }) => main(["setup", ...process.argv.slice(2)]))
    .then((code) => {
      process.exitCode = code;
    });
}
