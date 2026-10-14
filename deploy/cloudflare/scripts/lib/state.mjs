// The engine's per-machine state, its lock and the result file.
//
// State: $XDG_CONFIG_HOME/backyard/cloudflare/<worker>.json (else ~/.config),
// mode 0600 in a 0700 directory, written atomically. It holds the deploy token,
// so a file that exists but cannot be read stops every command: carrying on
// would silently replace the remembered token. Schema version 2 (a version-1
// file, without "version", is read as version 2 with the same fields); unknown
// fields are kept. The admin token (BACKYARD_CF_API_TOKEN) is never stored.
//
//   { "version": 2, "accountId", "url", "deployToken", "workersDevUrl", "workersDev",
//     "r2", "r2Bucket", "vars": {…user --var overrides…},
//     "domain": {host, mode: "path"|"host", zoneId, zoneName, customDomainId, routeId,
//                dnsRecordId, status: "active"|"pending-dns"|"pending-certificate",
//                source: "cli"|"adopted"},
//     "access": {team, aud, source: "api"|"manual"|"adopted", appId, bypassAppId,
//                allowPolicyId, bypassPolicyId, allow: {emails, emailDomains}},
//     "accessCreated": {host, appId, bypassAppId, allowPolicyId, bypassPolicyId},
//                ids of Access objects this tool created that "access" does not use
//                (yet: an enable that stopped before its deploy; or no longer: after
//                `access disable` without --delete-apps), so a re-run reuses them and
//                `access disable --delete-apps` can delete them
//     "updatedAt" }
//
// Lock: <state dir>/<worker>.lock, created with O_EXCL, holding {pid, host,
// startedAt}. Stale when it is on this host and its process is gone, or on
// another host and older than 2 hours.
//
// Result file (for wrappers such as the Go CLI): when BACKYARD_CF_RESULT_FILE
// is set, a command that saved state writes there (0600, atomically, also when
// it exits 4):
//
//   {"schema": 1, "command", "worker", "statePath", "url", "previousUrl",
//    "workersDevUrl", "authMode": "none"|"access", "deployToken": "state"|"env"|"unknown"}
//
// It never contains a secret.

import { chmodSync, closeSync, existsSync, mkdirSync, openSync, readFileSync, renameSync, rmSync, statSync, unlinkSync, writeFileSync, writeSync } from "node:fs";
import { homedir, hostname } from "node:os";
import { dirname, join } from "node:path";

import { CliError, EXIT } from "./ui.mjs";

export const STATE_VERSION = 2;
export const RESULT_SCHEMA = 1;
/** A lock of another host is stale after this long. */
export const LOCK_STALE_MS = 2 * 60 * 60 * 1000;

/** The directory of the state files. */
export function stateDir(env = process.env) {
  return join(env.XDG_CONFIG_HOME || join(homedir(), ".config"), "backyard", "cloudflare");
}

/** The state file of a Worker. */
export function stateFile(worker, env = process.env) {
  return join(stateDir(env), `${worker}.json`);
}

/**
 * Reads the saved state ({} when there is none). A file that exists but is not
 * a JSON object stops the run (CliError 1).
 * @param {string} worker
 * @param {NodeJS.ProcessEnv} [env]
 * @returns {Record<string, any>}
 */
export function loadState(worker, env = process.env) {
  const p = stateFile(worker, env);
  let text;
  try {
    text = readFileSync(p, "utf8");
  } catch (e) {
    if (e?.code === "ENOENT") return {};
    throw new CliError(EXIT.FAILED, `cannot read ${p}: ${e?.message ?? e}`);
  }
  let state;
  try {
    state = JSON.parse(text);
  } catch {
    state = undefined;
  }
  if (!state || typeof state !== "object" || Array.isArray(state)) {
    throw new CliError(
      EXIT.FAILED,
      `${p} is not a valid state file (a JSON object): repair it, or delete it to start over (the deployed configuration is then adopted; a new DEPLOY_TOKEN is issued only if the Worker has none)`,
    );
  }
  // Version 1 (no "version") has the same fields.
  if (state.version !== undefined && state.version !== 1 && state.version !== STATE_VERSION) {
    throw new CliError(EXIT.FAILED, `${p} has state version ${JSON.stringify(state.version)}, which this backyard-cloudflare does not know: upgrade it`);
  }
  return state;
}

/**
 * Writes a file atomically as a private file (0600, also when an older
 * version left it readable by others), creating its directory (0700).
 */
export function writePrivateFile(p, text) {
  mkdirSync(dirname(p), { recursive: true, mode: 0o700 });
  const tmp = `${p}.${process.pid}.${Date.now()}.tmp`;
  try {
    writeFileSync(tmp, text, { mode: 0o600 });
    chmodSync(tmp, 0o600);
    renameSync(tmp, p);
  } finally {
    rmSync(tmp, { force: true });
  }
  return p;
}

/**
 * Saves the state (version 2, updatedAt now) and returns its path. Fields
 * that are undefined are dropped, unknown fields are kept.
 * @param {string} worker
 * @param {Record<string, any>} state  updated in place (version, updatedAt)
 * @param {NodeJS.ProcessEnv} [env]
 */
export function saveState(worker, state, env = process.env) {
  state.version = STATE_VERSION;
  state.updatedAt = new Date().toISOString();
  const { version, ...rest } = state;
  return writePrivateFile(stateFile(worker, env), JSON.stringify({ version, ...rest }, null, 2) + "\n");
}

function processAlive(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    return e?.code === "EPERM"; // exists, but belongs to someone else
  }
}

/**
 * Takes the lock of a Worker (every mutating command; status and --dry-run
 * take none). Throws CliError(1) while another live run holds it.
 * @param {string} worker
 * @param {{env?: NodeJS.ProcessEnv, now?: () => number, host?: string, pid?: number}} [o]
 * @returns {{path: string, release: () => void}}
 */
export function acquireLock(worker, o = {}) {
  const env = o.env ?? process.env;
  const now = o.now ?? Date.now;
  const host = o.host ?? hostname();
  const pid = o.pid ?? process.pid;
  const p = join(stateDir(env), `${worker}.lock`);
  mkdirSync(dirname(p), { recursive: true, mode: 0o700 });
  const mine = { pid, host, startedAt: new Date(now()).toISOString() };
  for (let attempt = 0; attempt < 2; attempt++) {
    let fd;
    try {
      fd = openSync(p, "wx", 0o600);
    } catch (e) {
      if (e?.code !== "EEXIST") throw new CliError(EXIT.FAILED, `cannot create the lock ${p}: ${e?.message ?? e}`);
      let held;
      try {
        held = JSON.parse(readFileSync(p, "utf8"));
      } catch {
        held = null;
      }
      let stale;
      if (held && typeof held === "object" && Number.isInteger(held.pid)) {
        const age = now() - Date.parse(held.startedAt ?? "");
        stale = held.host === host ? !processAlive(held.pid) : !(age < LOCK_STALE_MS);
      } else {
        // Unreadable: stale unless it was just created (being written right now).
        let mtime = now();
        try {
          mtime = statSync(p).mtimeMs;
        } catch {
          /* gone meanwhile */
        }
        stale = now() - mtime > 10_000;
      }
      if (stale && attempt === 0) {
        rmSync(p, { force: true });
        continue;
      }
      const who = held ? `pid ${held.pid} on ${held.host} since ${held.startedAt}` : "unknown process";
      throw new CliError(EXIT.FAILED, `another backyard-cloudflare run for ${worker} is in progress (${who}); if not, delete ${p}`);
    }
    try {
      writeSync(fd, JSON.stringify(mine) + "\n");
    } finally {
      closeSync(fd);
    }
    return {
      path: p,
      release() {
        try {
          const held = JSON.parse(readFileSync(p, "utf8"));
          if (held.pid === mine.pid && held.startedAt === mine.startedAt && held.host === mine.host) unlinkSync(p);
        } catch {
          /* already gone */
        }
      },
    };
  }
  throw new CliError(EXIT.FAILED, `cannot take the lock ${p}`);
}

/** Releases a lock taken with acquireLock (only if this run still holds it). */
export function releaseLock(lock) {
  lock?.release();
}

/**
 * Builds the result file content of a command (no secrets).
 * @param {{command: string, worker: string, statePath: string, state: Record<string, any>, previousUrl?: string, authMode?: string, env?: NodeJS.ProcessEnv}} o
 */
export function buildResult(o) {
  const env = o.env ?? process.env;
  const deployToken = env.DEPLOY_TOKEN ? "env" : o.state.deployToken ? "state" : "unknown";
  return {
    schema: RESULT_SCHEMA,
    command: o.command,
    worker: o.worker,
    statePath: o.statePath,
    url: o.state.url ?? "",
    previousUrl: o.previousUrl ?? "",
    workersDevUrl: o.state.workersDevUrl ?? "",
    authMode: o.authMode === "access" ? "access" : "none",
    deployToken,
  };
}

/**
 * Writes the result to BACKYARD_CF_RESULT_FILE (when set); returns its path or "".
 * @param {ReturnType<typeof buildResult>} result
 * @param {NodeJS.ProcessEnv} [env]
 */
export function writeResult(result, env = process.env) {
  const p = env.BACKYARD_CF_RESULT_FILE;
  if (!p) return "";
  writePrivateFile(p, JSON.stringify(result, null, 2) + "\n");
  return p;
}

/** Whether a state file exists for the Worker. */
export function hasState(worker, env = process.env) {
  return existsSync(stateFile(worker, env));
}
