// How the engine is installed, the wrangler configuration it deploys, and the
// one deploy function every command uses.
//
// Layouts:
// * packaged (the npm package): dist/build.json exists and src/index.ts does
//   not. No build step; every wrangler call runs in a private run directory
//   (mkdtemp "backyard-cf-", 0700, removed at the end), where the effective
//   config is written as wrangler.json: dist/wrangler.base.json plus name,
//   main (absolute dist/worker.js), no_bundle and find_additional_modules
//   false. Nothing is written next to the package (read-only installs work).
// * source (a checkout): src/index.ts and wrangler.jsonc exist. sync-assets
//   --strict runs once before the first deploy; wrangler runs in the package
//   directory and the effective config is wrangler.setup.jsonc next to
//   wrangler.jsonc (removed afterwards).
//
// The effective config never contains `routes`: custom domains and routes are
// attached by `domain add` with explicit conflict checks, so a deploy (also
// from CI) never changes them. With AI off (state.ai === false, `setup
// --no-ai`) it has no `ai` binding either. Variables merge as base config <
// state.vars < the variables managed by `domain add` / `access enable` /
// `setup --no-ai` (managedVars).

import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { authGuard, fillAuthFromInfo } from "./live.mjs";
import { CliError, EXIT, bold, dim, ui } from "./ui.mjs";
import { createBucket, findURL, putSecret, runWrangler, secretList } from "./wrangler.mjs";

/** The engine's package directory (deploy/cloudflare in a checkout). */
export const PKG_ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
/** Name of the effective config in the source layout. */
export const GENERATED_CONFIG = "wrangler.setup.jsonc";

/**
 * Parses JSONC: strips // and /* *\/ comments and trailing commas, leaving
 * string contents alone (a value such as "a,}" or "x // y" is kept as is).
 */
export function parseJSONC(text) {
  // Pass 1: comments out (string-aware).
  let noComments = "";
  let inStr = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (inStr) {
      noComments += c;
      if (c === "\\") noComments += text[++i] ?? "";
      else if (c === '"') inStr = false;
    } else if (c === '"') {
      inStr = true;
      noComments += c;
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      noComments += "\n";
    } else if (c === "/" && text[i + 1] === "*") {
      i += 2;
      while (i < text.length && !(text[i] === "*" && text[i + 1] === "/")) i++;
      i++;
      noComments += " ";
    } else {
      noComments += c;
    }
  }
  // Pass 2: trailing commas out (string-aware).
  let out = "";
  inStr = false;
  for (let i = 0; i < noComments.length; i++) {
    const c = noComments[i];
    if (inStr) {
      out += c;
      if (c === "\\") out += noComments[++i] ?? "";
      else if (c === '"') inStr = false;
      continue;
    }
    if (c === '"') inStr = true;
    else if (c === ",") {
      let j = i + 1;
      while (j < noComments.length && /\s/.test(noComments[j])) j++;
      if (noComments[j] === "}" || noComments[j] === "]") continue;
    }
    out += c;
  }
  return JSON.parse(out);
}

/**
 * @typedef {object} Layout
 * @property {"packaged"|"source"} kind
 * @property {string} root            the package directory
 * @property {string} version         "1.2.3" (packaged) or "0.1.0-dev (source checkout)"
 * @property {string} packageVersion  package.json "version"
 * @property {Record<string, any>} build   dist/build.json (packaged), else {}
 */

/**
 * Detects the layout of the engine at `root`. Throws CliError(1) for an
 * incomplete installation.
 * @param {string} [root]
 * @returns {Layout}
 */
export function detectLayout(root = PKG_ROOT) {
  let pkg = {};
  try {
    pkg = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
  } catch {
    pkg = {};
  }
  const packageVersion = String(pkg.version ?? "0.0.0");
  const hasBuild = existsSync(join(root, "dist", "build.json"));
  const hasSrc = existsSync(join(root, "src", "index.ts"));
  if (hasBuild && !hasSrc) {
    let build = {};
    try {
      build = JSON.parse(readFileSync(join(root, "dist", "build.json"), "utf8"));
    } catch {
      throw new CliError(EXIT.FAILED, "incomplete installation (dist/build.json is not valid JSON): reinstall backyard-cloudflare, or run from a complete checkout");
    }
    for (const f of ["worker.js", "wrangler.base.json"]) {
      if (!existsSync(join(root, "dist", f))) throw new CliError(EXIT.FAILED, `incomplete installation (dist/${f} is missing): reinstall backyard-cloudflare, or run from a complete checkout`);
    }
    return { kind: "packaged", root, version: String(build.version || packageVersion), packageVersion, build };
  }
  if (hasSrc && existsSync(join(root, "wrangler.jsonc"))) {
    return { kind: "source", root, version: `${packageVersion}-dev (source checkout)`, packageVersion, build: {} };
  }
  throw new CliError(EXIT.FAILED, "incomplete installation: reinstall backyard-cloudflare, or run from a complete checkout");
}

/**
 * The base wrangler config of a layout (the variables' defaults included).
 * @param {Layout} layout
 */
export function baseConfig(layout) {
  if (layout.kind === "packaged") {
    let base;
    try {
      base = JSON.parse(readFileSync(join(layout.root, "dist", "wrangler.base.json"), "utf8"));
    } catch (e) {
      throw new CliError(EXIT.FAILED, `cannot read dist/wrangler.base.json (${e?.message ?? e}): reinstall backyard-cloudflare`);
    }
    return { ...base, name: base.name || "backyard", main: join(layout.root, "dist", "worker.js"), no_bundle: true, find_additional_modules: false };
  }
  try {
    return parseJSONC(readFileSync(join(layout.root, "wrangler.jsonc"), "utf8"));
  } catch (e) {
    throw new CliError(EXIT.FAILED, `cannot read wrangler.jsonc: ${e?.message ?? e}`);
  }
}

let synced = false;
/**
 * Source layout: bundles the SDK, hub and docs of the checkout
 * (scripts/sync-assets.mjs --strict), once per process. No-op when packaged.
 * @param {Layout} layout
 */
export async function syncAssets(layout) {
  if (layout.kind !== "source" || synced) return;
  const code = await new Promise((res) => {
    const c = spawn(process.execPath, [join(layout.root, "scripts", "sync-assets.mjs"), "--strict"], { stdio: "inherit", cwd: layout.root });
    c.on("error", () => res(1));
    c.on("close", (code) => res(code ?? 1));
  });
  if (code !== 0) throw new CliError(EXIT.FAILED, "syncing assets failed (scripts/sync-assets.mjs --strict; see above)");
  synced = true;
}

/**
 * fn, run at most once: a temporary file removed after its use is not removed
 * again by the end-of-command cleanup (in a checkout another Worker's run may
 * have written the same wrangler.setup.jsonc by then).
 * @param {() => unknown} fn
 */
function once(fn) {
  let done = false;
  return () => {
    if (done) return;
    done = true;
    fn();
  };
}

/** A private run directory (0700) for wrangler in the packaged layout. */
export function createRunDir() {
  const d = mkdtempSync(join(tmpdir(), "backyard-cf-"));
  chmodSync(d, 0o700);
  return d;
}

/**
 * The variables `domain add`, `access enable` and `setup --no-ai` manage,
 * derived from the state at every deploy. AI off sets AI_ENABLED=false, which
 * wins over a --var, so the Worker never runs without the AI binding while
 * its variable says AI is on.
 * @param {Record<string, any>} state
 */
export function managedVars(state) {
  const m = {};
  if (state?.ai === false) m.AI_ENABLED = "false";
  if (state?.domain?.host) {
    m.BASE_DOMAIN = state.domain.host;
    m.URL_MODE = state.domain.mode === "host" && state.domain.status === "active" ? "host" : "path";
  }
  if (state?.access) {
    m.AUTH_MODE = "access";
    m.ACCESS_TEAM_DOMAIN = String(state.access.team ?? "");
    m.ACCESS_AUD = String(state.access.aud ?? "");
  }
  return m;
}

/**
 * The variables of a deploy: base config < state.vars < managed.
 * @param {Record<string, any>} baseVars
 * @param {Record<string, any>} state
 */
export function computeVars(baseVars, state) {
  return { ...(baseVars ?? {}), ...(state?.vars ?? {}), ...managedVars(state) };
}

/**
 * The effective wrangler config: the base plus name, vars, the R2 binding and
 * workers_dev, without routes (warned about in a checkout's wrangler.jsonc),
 * and without the Workers AI binding when o.ai is false.
 * @param {Record<string, any>} base
 * @param {{worker: string, vars: Record<string, any>, bucket?: string, workersDev?: boolean, ai?: boolean, warnRoutes?: boolean}} o
 */
export function effectiveConfig(base, o) {
  const config = { ...base, name: o.worker, vars: o.vars, workers_dev: o.workersDev ?? true };
  if (o.bucket) config.r2_buckets = [{ binding: "R2", bucket_name: o.bucket }];
  if (o.ai === false) delete config.ai;
  if (config.routes !== undefined || config.route !== undefined) {
    if (o.warnRoutes) ui.warn("routes in wrangler.jsonc are ignored; use domain add");
    delete config.routes;
    delete config.route;
  }
  return config;
}

/**
 * Writes the effective config where wrangler expects it and returns its path.
 * @param {Layout} layout
 * @param {Record<string, any>} config
 * @param {string} runDir   the packaged layout's run directory
 */
export function writeConfig(layout, config, runDir) {
  if (layout.kind === "packaged") {
    const p = join(runDir, "wrangler.json");
    writeFileSync(p, JSON.stringify(config, null, 2) + "\n", { mode: 0o600 });
    return p;
  }
  const p = join(layout.root, GENERATED_CONFIG);
  writeFileSync(p, "// Generated by scripts/setup.mjs from wrangler.jsonc — do not edit.\n" + JSON.stringify(config, null, 2) + "\n");
  return p;
}

/**
 * The part of a command context the deploy needs (see Ctx in scripts/cli.mjs).
 * @typedef {object} DeployCtx
 * @property {string} worker
 * @property {Layout} layout
 * @property {Record<string, any>} base
 * @property {string} invokedAs
 * @property {string} statePath
 * @property {() => string} cwd
 * @property {() => Promise<import("./live.mjs").Live>} readLive
 * @property {(state: Record<string, any>) => string} saveState
 * @property {(fn: () => void) => void} [onCleanup]   runs at the end of the command, also on Ctrl-C
 */

/**
 * @typedef {object} DeployOptions
 * @property {import("./live.mjs").Live} [live]   live snapshot for the guard (read when absent)
 * @property {boolean} [guard]               false skips the auth guard (access disable only)
 * @property {boolean} [confirmAuthChange]   the guard reports instead of refusing
 * @property {string} [bucket]               R2 bucket to bind (default state.r2Bucket; "" = none)
 * @property {Record<string, any>} [stateOverrides]  fields replacing the state's for this deploy only, e.g. {access: undefined}
 * @property {boolean} [rotateToken]
 * @property {boolean} [rotateSecret]
 * @property {boolean} [skipDeploy]          set missing secrets with `wrangler secret put` instead of deploying
 * @property {boolean} [quiet]               do not stream wrangler's output
 *
 * @typedef {object} DeployResult
 * @property {Record<string, any>} vars      the deployed variables
 * @property {string} token                  the deploy token ("" when the Worker keeps one this machine does not know)
 * @property {string[]} secretsSet           names of the secrets uploaded
 * @property {string} output                 wrangler's output
 * @property {string} url                    the URL in wrangler's output ("" when none)
 * @property {string} bucket
 * @property {boolean} workersDev
 * @property {boolean} ai                    whether the Workers AI binding was deployed
 */

/**
 * Deploys the Worker with the configuration derived from the state: the auth
 * guard, the secrets (BACKYARD_SECRET only when missing or rotated; the
 * DEPLOY_TOKEN from $DEPLOY_TOKEN, a rotation, the state, a new one when the
 * Worker has none, or kept), `wrangler deploy --secrets-file <0600 tmp>`, and
 * the clean-up of every temporary file. A new deploy token is saved to the
 * state right away (state.deployToken), so it is never lost.
 * @param {DeployCtx} ctx
 * @param {Record<string, any>} state
 * @param {DeployOptions} [o]
 * @returns {Promise<DeployResult>}
 */
export async function deployWorker(ctx, state, o = {}) {
  const worker = ctx.worker;
  const view = { ...state, ...(o.stateOverrides ?? {}) };
  const vars = computeVars(ctx.base.vars ?? {}, view);
  const bucket = o.bucket !== undefined ? o.bucket : state.r2Bucket ?? "";
  const workersDev = view.workersDev ?? true;
  const ai = view.ai !== false;
  const cwd = ctx.cwd();

  let listed;
  if (o.guard !== false) {
    let live = o.live ?? (await ctx.readLive());
    if (live.exists === null) {
      live = await fillAuthFromInfo({ ...live }, [state.url, state.workersDevUrl]);
      if (live.exists === null) {
        // Only wrangler's explicit "Worker not found" makes it a first deploy.
        listed = await secretList(worker, { cwd });
        if (!listed.exists) live = { ...live, exists: false };
      }
    }
    const remedy =
      `  If another machine changed it: delete ${ctx.statePath} to adopt the live configuration, then re-run.\n` +
      `  If this is deliberate: ${ctx.invokedAs} setup --confirm-auth-change (or ${ctx.invokedAs} access disable to turn Access off).`;
    const g = authGuard(live, vars, { allow: Boolean(o.confirmAuthChange), worker, remedy });
    for (const n of g.notes) ui.warn(n);
  }

  listed ??= await secretList(worker, { cwd });
  const have = listed.names;
  if (o.skipDeploy && !listed.exists) throw new CliError(EXIT.FAILED, `Worker ${worker} does not exist yet: run ${ctx.invokedAs} setup without --skip-deploy first`);
  const secrets = {};
  // Generated only when the Worker has none (or on request): a new value
  // revokes every API token. It is never printed.
  if (!have.has("BACKYARD_SECRET") || o.rotateSecret) secrets.BACKYARD_SECRET = randomBytes(32).toString("hex");
  const newToken = () => "byd_" + randomBytes(24).toString("base64url");
  // The deploy token: the one from the environment, a new one on request,
  // the one remembered here, or a new one when the Worker has none. A token
  // the Worker already has is never replaced just because this machine does
  // not know it (another machine, a teammate, CI, a fresh $XDG_CONFIG_HOME):
  // every CLI login and pipeline using it would stop working.
  let token = "";
  if (process.env.DEPLOY_TOKEN) {
    token = process.env.DEPLOY_TOKEN;
    secrets.DEPLOY_TOKEN = token;
  } else if (o.rotateToken) {
    token = newToken();
    secrets.DEPLOY_TOKEN = token;
  } else if (state.deployToken) {
    token = state.deployToken;
    if (!have.has("DEPLOY_TOKEN")) secrets.DEPLOY_TOKEN = token;
  } else if (!have.has("DEPLOY_TOKEN")) {
    token = newToken();
    secrets.DEPLOY_TOKEN = token;
  } else {
    ui.warn(
      `Worker ${worker} already has a DEPLOY_TOKEN that this machine does not know: keeping it. ` +
        `Run with DEPLOY_TOKEN=<token> to remember it here, or with --rotate-token to issue a new one.`,
    );
  }

  // A new token for a Worker that has none is remembered before the upload: if
  // the run is interrupted or fails, the next run uploads it (state.deployToken
  // set, DEPLOY_TOKEN missing). A token that replaces one the Worker has is
  // saved only after the deploy, or the state would name a token the Worker
  // does not have.
  if (token && !have.has("DEPLOY_TOKEN") && state.deployToken !== token) {
    state.deployToken = token;
    ctx.saveState(state);
  }

  let output = "";
  if (o.skipDeploy) {
    for (const name of ["BACKYARD_SECRET", "DEPLOY_TOKEN"]) {
      if (secrets[name] !== undefined) await putSecret(worker, name, secrets[name], { cwd });
      else ui.ok(`Secret ${bold(name)} kept`);
    }
  } else {
    const config = effectiveConfig(ctx.base, { worker, vars, bucket, workersDev, ai, warnRoutes: ctx.layout.kind === "source" });
    const generated = writeConfig(ctx.layout, config, cwd);
    // Removed in the finally below, or by the command's cleanups on Ctrl-C (which exits at once).
    const removeGenerated = once(() => rmSync(generated, { force: true }));
    ctx.onCleanup?.(removeGenerated);
    const args = ["deploy", "--name", worker, "--env=", "--config", generated];
    // A private temporary file (0600), removed right after the deploy.
    const secretDir = Object.keys(secrets).length ? mkdtempSync(join(tmpdir(), "backyard-cf-secrets-")) : "";
    const removeSecrets = once(() => secretDir && rmSync(secretDir, { recursive: true, force: true }));
    if (secretDir) {
      ctx.onCleanup?.(removeSecrets);
      chmodSync(secretDir, 0o700);
      const secretsFile = join(secretDir, "secrets.json");
      writeFileSync(secretsFile, JSON.stringify(secrets), { mode: 0o600 });
      args.push("--secrets-file", secretsFile);
    }
    const overrides = Object.keys(view.vars ?? {}).sort();
    if (overrides.length) ui.note(`Variable overrides: ${overrides.join(", ")}`);
    ui.log(`\nDeploying Worker ${bold(worker)}${bucket ? ` with R2 bucket ${bold(bucket)}` : ""}${ai ? "" : `${bucket ? "," : ""} without AI`}…\n`);
    let r;
    try {
      r = await runWrangler(args, { cwd, quiet: o.quiet });
    } finally {
      removeGenerated();
      removeSecrets();
    }
    output = r.output;
    if (r.code !== 0) {
      if (/workers\.dev subdomain/i.test(r.output)) {
        throw new CliError(
          EXIT.FAILED,
          `register a workers.dev subdomain first: re-run ${ctx.invokedAs} setup --workers-subdomain <name>, or dash.cloudflare.com → Workers & Pages (set up your subdomain)`,
        );
      }
      throw new CliError(EXIT.FAILED, "wrangler deploy failed (see the output above)");
    }
    for (const name of ["BACKYARD_SECRET", "DEPLOY_TOKEN"]) {
      ui.ok(`Secret ${bold(name)} ${secrets[name] !== undefined ? "set" : "kept"}`);
    }
  }
  if (token && state.deployToken !== token) {
    state.deployToken = token;
    ctx.saveState(state);
  }
  return { vars, token, secretsSet: Object.keys(secrets).sort(), output, url: findURL(output, worker), bucket, workersDev, ai };
}

/**
 * `wrangler deploy --dry-run` of the effective config: validates the bundle
 * and the bindings offline (no login, no secrets, no state writes). Prints the
 * effective variables and returns wrangler's exit code.
 * @param {DeployCtx} ctx
 * @param {Record<string, any>} state   (not saved)
 * @param {{bucket?: string}} [o]
 */
export async function dryRunDeploy(ctx, state, o = {}) {
  const vars = computeVars(ctx.base.vars ?? {}, state);
  const bucket = o.bucket ?? state.r2Bucket ?? "";
  const ai = state.ai !== false;
  const config = effectiveConfig(ctx.base, { worker: ctx.worker, vars, bucket, workersDev: state.workersDev ?? true, ai, warnRoutes: ctx.layout.kind === "source" });
  const cwd = ctx.cwd();
  const outBase = ctx.layout.kind === "packaged" ? cwd : mkdtempSync(join(tmpdir(), "backyard-cf-dry-run-"));
  const generated = writeConfig(ctx.layout, config, cwd);
  const removeGenerated = once(() => rmSync(generated, { force: true }));
  const removeOut = once(() => outBase !== cwd && rmSync(outBase, { recursive: true, force: true }));
  ctx.onCleanup?.(removeGenerated);
  ctx.onCleanup?.(removeOut);
  ui.log(`Validating Worker ${bold(ctx.worker)} (wrangler deploy --dry-run: nothing is uploaded)…\n`);
  let r;
  try {
    r = await runWrangler(["deploy", "--dry-run", "--name", ctx.worker, "--env=", "--config", generated, "--outdir", join(outBase, "out")], { cwd });
  } finally {
    removeGenerated();
    removeOut();
  }
  ui.log("\nEffective variables:");
  for (const k of Object.keys(vars).sort()) {
    const v = vars[k];
    ui.log(`  ${k}=${typeof v === "string" ? v : JSON.stringify(v)}`);
  }
  ui.log(dim(`R2: ${bucket ? `bucket ${bucket}` : "none"} · AI: ${ai ? "on" : "off (no Workers AI binding)"} · workers.dev: ${config.workers_dev ? "on" : "off"} · layout: ${ctx.layout.kind}`));
  if (r.code !== 0) ui.error("wrangler deploy --dry-run failed (see the output above)");
  return r.code;
}

/**
 * Creates the R2 bucket (an existing one is fine): {ok, notEnabled} (see
 * createBucket in wrangler.mjs; a failure prints nothing).
 * @param {DeployCtx} ctx
 * @param {string} bucket
 */
export function ensureBucket(ctx, bucket) {
  return createBucket(bucket, { cwd: ctx.cwd() });
}
