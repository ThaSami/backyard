// The backyard-cloudflare command line: parses argv (lib/commands.json),
// builds the context every command runs with, takes the Worker's lock for
// mutating commands, and turns a CliError into the exit code.
//
//   backyard-cloudflare setup | domain add <host> | domain remove |
//                       access enable | access disable | status | help [cmd] | --version
//
// Commands are `async (opts, ctx) => number | void` in scripts/setup.mjs,
// domain.mjs, access.mjs and status.mjs; `opts` are the parsed flags
// (lib/args.mjs: camelCased, repeatables as arrays, --wait a number,
// positionals such as opts.host) and `ctx` is described by Ctx below.

import { rmSync } from "node:fs";

import { formatHelp, parseArgs } from "./lib/args.mjs";
import { CfError, createApi } from "./lib/cfapi.mjs";
import { baseConfig, computeVars, createRunDir, deployWorker, detectLayout, dryRunDeploy, ensureBucket, syncAssets } from "./lib/layout.mjs";
import { adopt, authModeOf, readLive } from "./lib/live.mjs";
import { acquireLock, buildResult, loadState, saveState, stateFile, writeResult } from "./lib/state.mjs";
import { CliError, EXIT, createPrompt, dim, interactive, ui } from "./lib/ui.mjs";
import { authToken, ensureLogin, pickAccount, wranglerVersion } from "./lib/wrangler.mjs";

/** Commands that change something take the Worker's lock (setup --dry-run does not). */
const MUTATING = new Set(["setup", "domain add", "domain remove", "access enable", "access disable"]);

/** Where each command lives (imported on demand). */
const RUNNERS = {
  setup: async () => (await import("./setup.mjs")).runSetup,
  "domain add": async () => (await import("./domain.mjs")).runDomainAdd,
  "domain remove": async () => (await import("./domain.mjs")).runDomainRemove,
  "access enable": async () => (await import("./access.mjs")).runAccessEnable,
  "access disable": async () => (await import("./access.mjs")).runAccessDisable,
  status: async () => (await import("./status.mjs")).runStatus,
};

const WORKER_NAME_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** The permissions the admin token needs, by step (printed in words). */
export const ADMIN_PERMISSIONS = {
  dns: "Zone · DNS · Edit (that zone only)",
  access: "Account · Access: Apps and Policies · Edit and Account · Access: Organizations, Identity Providers, and Groups · Read",
};

/** How the Go CLI names the engine (BACKYARD_CF_INVOKED_AS): its users have the `backyard` CLI. */
export const GO_CLI_INVOCATION = "backyard cloudflare";

/**
 * How hints should name the engine: BACKYARD_CF_INVOKED_AS (the Go CLI sets
 * GO_CLI_INVOCATION), else "npm run cf --" in a checkout, "npx
 * backyard-cloudflare" under npx, or "backyard-cloudflare".
 * @param {{kind: string} | null} layout
 * @param {NodeJS.ProcessEnv} [env]
 */
export function detectInvokedAs(layout, env = process.env) {
  if (env.BACKYARD_CF_INVOKED_AS) return env.BACKYARD_CF_INVOKED_AS;
  if (layout?.kind === "source") return "npm run cf --";
  if (env.npm_command === "exec") return "npx backyard-cloudflare";
  return "backyard-cloudflare";
}

/** `backyard-cloudflare <version> (wrangler <v>)` */
export function versionLine(layout) {
  return `backyard-cloudflare ${layout?.version ?? "(incomplete installation)"} (wrangler ${wranglerVersion() || "not installed"})`;
}

/**
 * @typedef {object} Login
 * @property {string} accountId
 * @property {string} accountName   "" when unknown
 * @property {string} email         "" for API tokens
 * @property {{id: string, name: string}[]} accounts
 */

/**
 * The context of one command run. Everything a command needs from the
 * engine; commands add nothing to it.
 *
 * @typedef {object} Ctx
 * @property {string} command          "setup" | "domain add" | "domain remove" | "access enable" | "access disable" | "status"
 * @property {Record<string, any>} opts   the parsed flags (also passed as the first argument)
 * @property {string} worker           the Worker name (--worker-name, else the config's name, else "backyard"; validated)
 * @property {import("./lib/layout.mjs").Layout} layout   {kind: "packaged"|"source", root, version, packageVersion, build}
 * @property {string} wranglerVersion  the installed wrangler's version
 * @property {Record<string, any>} base   the base wrangler config (base.vars are the variables' defaults)
 * @property {string} invokedAs        how hints name the engine, e.g. "npx backyard-cloudflare", "backyard cloudflare", "npm run cf --"
 * @property {boolean} interactive     prompts allowed (TTY, not CI, no --non-interactive)
 * @property {boolean} yes             --yes was given
 * @property {import("./lib/ui.mjs").Prompt} prompt   ask / confirm / choose / askHidden (rules of §3.0)
 * @property {typeof ui} ui            log / ok / warn / note / error
 * @property {string} statePath        the Worker's state file
 * @property {string} accountId        set by login() ("" before)
 * @property {string} accountName      set by login() ("" when unknown)
 * @property {() => string} cwd        the directory wrangler runs in (packaged: a private run dir, created on first use and removed at the end)
 * @property {() => Record<string, any>} loadState      {} when none; CliError(1) for an unreadable file
 * @property {(state: Record<string, any>) => string} saveState   atomically, 0600, sets version 2 and updatedAt; returns the path
 * @property {() => Promise<void>} syncAssets   source layout: sync-assets --strict once (no-op when packaged)
 * @property {(state?: Record<string, any>, o?: {browser?: boolean}) => Promise<Login | null>} login
 *   wrangler login (the browser only when interactive) and the account choice;
 *   sets CLOUDFLARE_ACCOUNT_ID for every child, ctx.accountId and state.accountId.
 *   With {browser: false} (status) it never prompts: it returns null instead of
 *   logging in, and several accounts without CLOUDFLARE_ACCOUNT_ID are a CliError(2).
 * @property {(o?: {fresh?: boolean}) => Promise<import("./lib/cfapi.mjs").Api>} api
 *   credential W (wrangler's own login, via `wrangler auth token --json`); one
 *   client per run, {fresh: true} re-reads the credential (phase boundaries)
 * @property {(need: "dns"|"access", o?: {prompt?: boolean}) => Promise<import("./lib/cfapi.mjs").Api | null>} admin
 *   credential A: BACKYARD_CF_API_TOKEN, else W when it is an API token, else
 *   (interactive, prompt !== false) a choice between the guided dashboard
 *   steps and pasting a token (hidden). null means "guided". The decision is
 *   remembered for the run. The token is never stored, printed or given to wrangler.
 * @property {() => Promise<import("./lib/live.mjs").Live>} readLive   live settings of the Worker (needs login())
 * @property {(state: Record<string, any>, live: import("./lib/live.mjs").Live) => boolean} adopt
 *   when the Worker exists and the state has no setup history (!state.updatedAt):
 *   fills the state from the live Worker and prints the one-line summary; true when it adopted
 * @property {(state: Record<string, any>, o?: import("./lib/layout.mjs").DeployOptions) => Promise<import("./lib/layout.mjs").DeployResult>} deploy
 *   the shared deploy (managed vars from the state, guard, secrets, cleanup); saves a new deploy token to the state itself
 * @property {(state: Record<string, any>, o?: {bucket?: string}) => Promise<number>} dryRun   wrangler deploy --dry-run of the state's config
 * @property {(bucket: string) => Promise<{ok: boolean, notEnabled: boolean}>} ensureBucket
 *   creates the R2 bucket (an existing one is fine); a failure prints nothing
 * @property {(state: Record<string, any>) => Record<string, any>} vars   the variables a deploy of this state would have
 * @property {(state: Record<string, any>, o?: {previousUrl?: string, authMode?: "none"|"access"}) => string} writeResult
 *   writes BACKYARD_CF_RESULT_FILE (when set) for this command; call it after the
 *   last saveState, also before throwing CliError(4). authMode defaults to the state's.
 * @property {(fn: () => void) => void} onCleanup   runs at the end of the command (also on Ctrl-C)
 */

/**
 * Builds the context of a command run.
 * @param {{command: string, opts: Record<string, any>, layout: import("./lib/layout.mjs").Layout, invokedAs: string, adminToken?: string}} o
 * @returns {Ctx}
 */
export function createContext(o) {
  const { command, opts, layout, invokedAs } = o;
  const base = baseConfig(layout);
  const worker = opts.workerName || base.name || "backyard";
  if (!WORKER_NAME_RE.test(worker)) throw new CliError(EXIT.USAGE, `invalid worker name ${JSON.stringify(worker)} (a-z, 0-9 and -, at most 63 characters)`);
  const isInteractive = interactive({ nonInteractive: opts.nonInteractive });
  const prompt = createPrompt({ interactive: isInteractive, yes: opts.yes });
  const adminToken = o.adminToken ?? "";
  const cleanups = [];
  let runDir = "";
  /** @type {import("./lib/cfapi.mjs").Api | undefined} */
  let wApi;
  /** @type {import("./lib/cfapi.mjs").Api | null | undefined} */
  let aApi;

  /** @type {Ctx} */
  const ctx = {
    command,
    opts,
    worker,
    layout,
    wranglerVersion: wranglerVersion(),
    base,
    invokedAs,
    interactive: isInteractive,
    yes: Boolean(opts.yes),
    prompt,
    ui,
    statePath: stateFile(worker),
    accountId: "",
    accountName: "",
    cwd() {
      if (layout.kind === "source") return layout.root;
      if (!runDir) {
        runDir = createRunDir();
        const d = runDir;
        cleanups.push(() => rmSync(d, { recursive: true, force: true }));
      }
      return runDir;
    },
    loadState: () => loadState(worker),
    saveState: (state) => saveState(worker, state),
    syncAssets: () => syncAssets(layout),
    async login(state, lo = {}) {
      // {browser: false} (status) never prompts: no browser login, no account choice.
      const mayPrompt = isInteractive && lo.browser !== false;
      const who = await ensureLogin({ cwd: ctx.cwd(), interactive: mayPrompt, browser: lo.browser, hint: `${invokedAs} setup` });
      if (!who) return null;
      const accountId = await pickAccount(who.accounts, state?.accountId, { interactive: mayPrompt, prompt });
      if (!accountId) throw new CliError(EXIT.USAGE, "could not tell which Cloudflare account to use: set CLOUDFLARE_ACCOUNT_ID");
      process.env.CLOUDFLARE_ACCOUNT_ID = accountId;
      const accountName = who.accounts.find((a) => a.id === accountId)?.name ?? "";
      ctx.accountId = accountId;
      ctx.accountName = accountName;
      if (state) state.accountId = accountId;
      if (lo.browser !== false && (who.accounts.length > 1 || accountName)) ui.note(`Account: ${accountName ? `${accountName} (${accountId})` : accountId}`);
      return { accountId, accountName, email: who.email, accounts: who.accounts };
    },
    async api(ao = {}) {
      if (!wApi) {
        const label = process.env.CLOUDFLARE_API_TOKEN ? "CLOUDFLARE_API_TOKEN" : "wrangler login";
        wApi = createApi({ getCredential: () => authToken({ cwd: ctx.cwd() }), label });
      } else if (ao.fresh) wApi.refresh();
      return wApi;
    },
    async admin(need, ao = {}) {
      if (aApi !== undefined) return aApi;
      if (adminToken) {
        aApi = createApi({ credential: { type: "api_token", token: adminToken }, label: "BACKYARD_CF_API_TOKEN" });
        return aApi;
      }
      const w = await ctx.api();
      if ((await w.credentialType()) === "api_token") {
        aApi = w; // CLOUDFLARE_API_TOKEN
        return aApi;
      }
      if (!isInteractive || ao.prompt === false) return null;
      ui.log(`\nThis step needs an API token with ${ADMIN_PERMISSIONS[need] ?? need}: your wrangler login cannot do it.`);
      const choice = await prompt.choose("How do you want to continue?", [
        { label: "I'll do it in the dashboard (guided)", value: "guided" },
        { label: "Paste an API token now (hidden input)", value: "paste" },
      ]);
      if (choice !== "paste") {
        aApi = null;
        return null;
      }
      const token = await prompt.askHidden("API token (not stored):");
      aApi = token ? createApi({ credential: { type: "api_token", token }, label: "the pasted API token" }) : null;
      return aApi;
    },
    async readLive() {
      if (!ctx.accountId) throw new CliError(EXIT.FAILED, "internal: readLive before login");
      return readLive({ api: await ctx.api(), accountId: ctx.accountId, worker });
    },
    adopt(state, live) {
      if (!live?.exists || state.updatedAt) return false;
      const { parts, warnings } = adopt(state, live, base.vars ?? {});
      for (const w of warnings) ui.warn(w);
      ui.ok(`Adopted the deployed configuration of ${worker} (no setup state on this machine): ${parts.length ? parts.join("; ") : "the defaults (nothing to keep)"}`);
      return true;
    },
    deploy: (state, d) => deployWorker(ctx, state, d),
    dryRun: (state, d) => dryRunDeploy(ctx, state, d),
    ensureBucket: (bucket) => ensureBucket(ctx, bucket),
    vars: (state) => computeVars(base.vars ?? {}, state),
    writeResult(state, r = {}) {
      const authMode = r.authMode ?? (authModeOf(ctx.vars(state).AUTH_MODE) === "access" ? "access" : "none");
      return writeResult(buildResult({ command, worker, statePath: ctx.statePath, state, previousUrl: r.previousUrl, authMode }));
    },
    onCleanup(fn) {
      cleanups.push(fn);
    },
  };
  Object.defineProperty(ctx, "runCleanups", {
    enumerable: false,
    value() {
      while (cleanups.length) {
        try {
          cleanups.pop()();
        } catch {
          /* best effort */
        }
      }
    },
  });
  return ctx;
}

/** Prints an error and returns its exit code. */
function report(e) {
  if (e instanceof CliError) {
    if (e.message) ui.error(e.message);
    if (e.applied.length) console.error(`  Already applied: ${e.applied.join("; ")}. Re-running the same command resumes.`);
    return e.code;
  }
  if (e instanceof CfError) {
    ui.error(e.message);
    return EXIT.FAILED;
  }
  ui.error(e?.stack || String(e));
  return EXIT.FAILED;
}

/**
 * Runs the engine with argv (without node and the script); resolves to the
 * exit code.
 * @param {string[]} argv
 * @param {{root?: string}} [o]  the package directory (tests)
 * @returns {Promise<number>}
 */
export async function main(argv, o = {}) {
  let layout = null;
  let layoutError = null;
  try {
    layout = detectLayout(o.root);
  } catch (e) {
    layoutError = e;
  }
  const invokedAs = detectInvokedAs(layout);
  let parsed;
  try {
    parsed = parseArgs(argv, { invokedAs });
  } catch (e) {
    return report(e);
  }
  if (parsed.kind === "version") {
    console.log(versionLine(layout));
    return EXIT.OK;
  }
  if (parsed.kind === "help") {
    process.stdout.write(formatHelp(parsed.command, { invokedAs, group: parsed.group }));
    return parsed.exitCode;
  }
  if (layoutError) return report(layoutError);

  // The admin token is kept in memory only: no child process ever sees it.
  const adminToken = process.env.BACKYARD_CF_API_TOKEN ?? "";
  delete process.env.BACKYARD_CF_API_TOKEN;

  let ctx;
  let lock;
  const onSigint = () => {
    try {
      ctx?.runCleanups();
      lock?.release();
    } finally {
      console.error(`\n${dim("Interrupted.")}`);
      process.exit(EXIT.INTERRUPTED);
    }
  };
  process.once("SIGINT", onSigint);
  try {
    ctx = createContext({ command: parsed.command, opts: parsed.opts, layout, invokedAs, adminToken });
    if (MUTATING.has(parsed.command) && !(parsed.command === "setup" && parsed.opts.dryRun)) lock = acquireLock(ctx.worker);
    const run = await RUNNERS[parsed.command]();
    const code = await run(parsed.opts, ctx);
    return typeof code === "number" ? code : EXIT.OK;
  } catch (e) {
    return report(e);
  } finally {
    process.removeListener("SIGINT", onSigint);
    ctx?.runCleanups();
    lock?.release();
  }
}
