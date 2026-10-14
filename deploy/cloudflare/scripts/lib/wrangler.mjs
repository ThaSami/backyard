// Running wrangler, the engine's only dependency (pinned exactly).
//
// wrangler is resolved through `wrangler/package.json` and its `bin.wrangler`
// (its exports map blocks `wrangler/bin/wrangler.js`), which works nested,
// hoisted (npx, global installs) and in the test sandbox. Every child gets
// WRANGLER_SEND_METRICS=false (unless set) and never BACKYARD_CF_API_TOKEN:
// the admin token is for Backyard's own REST calls only.
//
// The helpers keep the rules of the original setup script: only wrangler's
// explicit "Worker not found" counts as a first deploy, the secret list is
// parsed without guessing, and the REST credential (`wrangler auth token
// --json`, which also refreshes an expired OAuth login) is never printed.

import { spawn } from "node:child_process";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";

import { CliError, EXIT, bold, dim, ui } from "./ui.mjs";

const require = createRequire(import.meta.url);

/** Path of wrangler's bin script; throws CliError(1) when wrangler is not installed. */
export function wranglerBin() {
  let pkgPath;
  try {
    pkgPath = require.resolve("wrangler/package.json");
  } catch {
    throw new CliError(EXIT.FAILED, "wrangler is not installed: run npm ci (in a checkout), or reinstall backyard-cloudflare");
  }
  const pkg = JSON.parse(readFileSync(pkgPath, "utf8"));
  const bin = typeof pkg.bin === "string" ? pkg.bin : pkg.bin?.wrangler;
  if (!bin) throw new CliError(EXIT.FAILED, `${pkgPath} names no wrangler bin`);
  return join(dirname(pkgPath), bin);
}

/** The installed wrangler version ("" when not installed). */
export function wranglerVersion() {
  try {
    return String(JSON.parse(readFileSync(require.resolve("wrangler/package.json"), "utf8")).version ?? "");
  } catch {
    return "";
  }
}

/** The environment of a wrangler child: no admin token, no metrics. */
export function wranglerEnv(env = process.env) {
  const out = { ...env, WRANGLER_SEND_METRICS: env.WRANGLER_SEND_METRICS ?? "false" };
  delete out.BACKYARD_CF_API_TOKEN;
  return out;
}

/**
 * @typedef {{code: number, output: string, stdout: string, stderr: string}} WranglerResult
 */

/**
 * Runs wrangler. Output is streamed (unless quiet) and captured (output:
 * stdout and stderr, stdout/stderr separately); stdin is inherited so
 * wrangler's prompts (the browser login) work, or fed from `input`.
 * @param {string[]} args
 * @param {{cwd: string, quiet?: boolean, input?: string, env?: NodeJS.ProcessEnv}} o
 * @returns {Promise<WranglerResult>}
 */
export function runWrangler(args, o) {
  const bin = wranglerBin();
  return new Promise((resolveRun) => {
    const child = spawn(process.execPath, [bin, ...args], {
      cwd: o.cwd,
      env: wranglerEnv(o.env ?? process.env),
      stdio: [o.input !== undefined ? "pipe" : "inherit", "pipe", "pipe"],
    });
    if (o.input !== undefined) {
      child.stdin.on("error", () => {});
      child.stdin.end(o.input);
    }
    let output = "";
    let stdout = "";
    let stderr = "";
    const onData = (stream, which) => (chunk) => {
      const s = chunk.toString();
      output += s;
      if (which === "out") stdout += s;
      else stderr += s;
      if (!o.quiet) stream.write(chunk);
    };
    child.stdout.on("data", onData(process.stdout, "out"));
    child.stderr.on("data", onData(process.stderr, "err"));
    child.on("error", (e) => resolveRun({ code: 1, output: String(e), stdout: "", stderr: String(e) }));
    child.on("close", (code) => resolveRun({ code: code ?? 1, output, stdout, stderr }));
  });
}

/**
 * Parses `wrangler whoami --json` ({loggedIn, email, accounts: [{id, name}]});
 * null when not logged in.
 * @param {WranglerResult} r
 * @returns {{email: string, accounts: {id: string, name: string}[]} | null}
 */
export function parseWhoami(r) {
  if (r.code !== 0) return null;
  try {
    const out = r.stdout ?? r.output;
    const j = JSON.parse(out.slice(out.indexOf("{"), out.lastIndexOf("}") + 1));
    if (!j.loggedIn) return null;
    const accounts = Array.isArray(j.accounts)
      ? j.accounts.filter((a) => a && typeof a.id === "string").map((a) => ({ id: a.id, name: String(a.name ?? a.id) }))
      : [];
    return { email: typeof j.email === "string" ? j.email : "", accounts };
  } catch {
    return { email: "", accounts: [] };
  }
}

/** `wrangler whoami --json`, quietly. */
export async function whoami(o) {
  return parseWhoami(await runWrangler(["whoami", "--json"], { cwd: o.cwd, quiet: true }));
}

/**
 * Makes sure wrangler can act for us; returns who we are (accounts may be
 * empty for scoped API tokens), or null when not logged in and `browser` is
 * false (status). CLOUDFLARE_API_TOKEN is honoured; the browser login runs only
 * when interactive.
 * @param {{cwd: string, interactive: boolean, browser?: boolean, hint?: string}} o
 */
export async function ensureLogin(o) {
  const fromEnv = Boolean(process.env.CLOUDFLARE_API_TOKEN);
  if (fromEnv && o.browser !== false) ui.note("Using CLOUDFLARE_API_TOKEN from the environment.");
  if (fromEnv && process.env.CLOUDFLARE_ACCOUNT_ID) return { email: "", accounts: [] };
  let who = await whoami(o);
  if (who) {
    if (!fromEnv && o.browser !== false) ui.ok(`Logged in to Cloudflare${who.email ? ` as ${bold(who.email)}` : ""}`);
    return who;
  }
  if (fromEnv) return { email: "", accounts: [] }; // let wrangler report token problems itself
  if (o.browser === false) return null;
  if (!o.interactive) {
    throw new CliError(
      EXIT.USAGE,
      `not logged in to Cloudflare: run ${o.hint ?? "this command"} on an interactive terminal (it opens the Cloudflare login in your browser), or set CLOUDFLARE_API_TOKEN (and CLOUDFLARE_ACCOUNT_ID)`,
    );
  }
  ui.warn("wrangler is not logged in — opening the Cloudflare login in your browser…");
  const login = await runWrangler(["login"], { cwd: o.cwd });
  if (login.code !== 0) throw new CliError(EXIT.FAILED, "wrangler login failed");
  who = await whoami(o);
  if (!who) throw new CliError(EXIT.FAILED, "wrangler login did not complete");
  return who;
}

/**
 * Picks the Cloudflare account: CLOUDFLARE_ACCOUNT_ID, the one used last
 * time, the only one, or a choice (interactive). "" when wrangler reports no
 * accounts (a scoped API token).
 * @param {{id: string, name: string}[]} accounts
 * @param {string | undefined} saved
 * @param {{interactive: boolean, prompt: import("./ui.mjs").Prompt}} o
 */
export async function pickAccount(accounts, saved, o) {
  if (process.env.CLOUDFLARE_ACCOUNT_ID) return process.env.CLOUDFLARE_ACCOUNT_ID;
  if (saved && accounts.some((a) => a.id === saved)) return saved;
  if (accounts.length === 0) return "";
  if (accounts.length === 1) return accounts[0].id;
  if (!o.interactive) {
    throw new CliError(
      EXIT.USAGE,
      `this login can deploy to ${accounts.length} Cloudflare accounts: set CLOUDFLARE_ACCOUNT_ID to one of: ${accounts.map((a) => (a.name && a.name !== a.id ? `${a.name} (${a.id})` : a.id)).join(", ")}`,
    );
  }
  ui.log(`\nYour Cloudflare login can deploy to ${accounts.length} accounts:`);
  return o.prompt.choose(
    "Deploy Backyard to which account?",
    accounts.map((a) => ({ label: `${a.name} ${dim(a.id)}`, value: a.id })),
    { flag: "CLOUDFLARE_ACCOUNT_ID" },
  );
}

/**
 * Parses the output of `wrangler auth token --json`: from the first "{" to
 * the last "}". Throws (without echoing the output, which holds the secret)
 * when it is not a credential.
 * @param {string} stdout
 * @returns {import("./cfapi.mjs").Credential}
 */
export function parseAuthToken(stdout) {
  const s = String(stdout ?? "");
  const end = s.lastIndexOf("}");
  // From the first "{" (or, when a notice line holds one, the first at a line start).
  const starts = [s.indexOf("{")];
  for (let i = s.indexOf("\n{"); i >= 0; i = s.indexOf("\n{", i + 1)) starts.push(i + 1);
  for (const start of starts) {
    if (start < 0 || end < start) continue;
    let j;
    try {
      j = JSON.parse(s.slice(start, end + 1));
    } catch {
      continue;
    }
    if (j && (j.type === "oauth" || j.type === "api_token") && typeof j.token === "string" && j.token) return { type: j.type, token: j.token };
    if (j && j.type === "api_key" && typeof j.key === "string" && j.key) return { type: "api_key", key: j.key, email: String(j.email ?? "") };
  }
  throw new Error("unexpected output of wrangler auth token --json");
}

/**
 * The REST credential wrangler uses (OAuth login or CLOUDFLARE_API_TOKEN),
 * read with `wrangler auth token --json` (quietly; it is never printed).
 * @param {{cwd: string}} o
 */
export async function authToken(o) {
  const r = await runWrangler(["auth", "token", "--json"], { cwd: o.cwd, quiet: true });
  if (r.code === 0) {
    try {
      return parseAuthToken(r.stdout);
    } catch {
      /* reported below */
    }
  }
  // stderr only: stdout would hold the credential.
  const why = r.stderr.trim().split("\n").filter(Boolean).slice(-3).join("\n");
  throw new CliError(EXIT.FAILED, `could not read wrangler's Cloudflare credential (wrangler auth token)${why ? `:\n${why}` : ""}`);
}

/**
 * Extracts the secret names from the stdout of `wrangler secret list
 * --format json`. Wrangler may print other lines first (e.g. "▲ [WARNING]
 * Proxy environment variables detected", or "[…]" notices), so the list is
 * looked for in every line starting with "[" from which a JSON array parses
 * (up to a line-final "]"): the first non-empty array of {name} objects is
 * the list. An empty array ("[]", no secrets) counts only when nothing else
 * parses as a list: a stray "[]" line before the real list must never make
 * the setup believe the Worker has no secrets (it would then replace
 * BACKYARD_SECRET, revoking every API token). Anything else throws: never
 * guess. (Also used by scripts/cloudflare-ci.mjs.)
 */
export function parseSecretList(stdout) {
  const s = String(stdout ?? "");
  const lineFinal = (j) => {
    let k = j + 1;
    while (k < s.length && (s[k] === " " || s[k] === "\t" || s[k] === "\r")) k++;
    return k === s.length || s[k] === "\n";
  };
  const isEntry = (x) => x !== null && typeof x === "object" && !Array.isArray(x) && typeof x.name === "string" && x.name !== "";
  const ends = [];
  for (let j = s.indexOf("]"); j >= 0; j = s.indexOf("]", j + 1)) if (lineFinal(j)) ends.push(j);
  let empty = false;
  let foreign = false;
  for (let i = s.indexOf("["); i >= 0; i = s.indexOf("[", i + 1)) {
    if (i > 0 && s[i - 1] !== "\n") continue;
    for (let k = ends.length - 1; k >= 0 && ends[k] > i; k--) {
      let list;
      try {
        list = JSON.parse(s.slice(i, ends[k] + 1));
      } catch {
        continue;
      }
      if (!Array.isArray(list)) continue;
      if (list.length === 0) empty = true;
      else if (list.every(isEntry)) return new Set(list.map((x) => x.name));
      else foreign = true;
    }
  }
  if (empty && !foreign) return new Set();
  throw new Error(
    foreign
      ? "no JSON list of secrets in the output of wrangler secret list (a JSON array that is not a list of {name} objects)"
      : "no JSON list of secrets in the output of wrangler secret list",
  );
}

/**
 * True when wrangler reports that the Worker does not exist yet (`Worker
 * "<name>" not found`, API error 10007). Authentication, permission and
 * network errors must never be mistaken for a first deploy.
 */
export function isMissingWorker(output) {
  return /Worker "[^"]*"(?: \(env: [^)]*\))? not found|\[code: 10007\]|does not exist on your account/i.test(String(output ?? ""));
}

/**
 * The Worker's secrets: {exists, names}. Only wrangler's explicit "Worker not
 * found" means a first deploy (no secrets); any other failure, or output that
 * is not the JSON list, throws CliError(1), so that a warning line, an expired
 * login or a network hiccup can never make a command replace BACKYARD_SECRET
 * (which would revoke every token).
 * @param {string} worker
 * @param {{cwd: string}} o
 */
export async function secretList(worker, o) {
  const r = await runWrangler(["secret", "list", "--name", worker, "--format", "json"], { cwd: o.cwd, quiet: true });
  if (r.code !== 0) {
    if (isMissingWorker(r.output)) return { exists: false, names: new Set() };
    throw new CliError(EXIT.FAILED, `could not list the secrets of Worker ${worker} (nothing was changed):\n${r.output.trim()}`);
  }
  try {
    return { exists: true, names: parseSecretList(r.stdout) };
  } catch {
    throw new CliError(EXIT.FAILED, `unexpected output of wrangler secret list for Worker ${worker} (nothing was changed):\n${r.output.trim()}`);
  }
}

/**
 * `wrangler secret put <name>`, the value on stdin.
 * @param {string} worker
 * @param {string} name
 * @param {string} value
 * @param {{cwd: string}} o
 */
export async function putSecret(worker, name, value, o) {
  const r = await runWrangler(["secret", "put", name, "--name", worker], { cwd: o.cwd, input: value + "\n", quiet: true });
  if (r.code !== 0) throw new CliError(EXIT.FAILED, `could not set the ${name} secret:\n${r.output.split(value).join("[redacted]")}`);
  ui.ok(`Secret ${bold(name)} set`);
}

/**
 * Creates an R2 bucket (an existing one is fine): {ok: true} after printing
 * that it is ready; else {ok: false, notEnabled} (R2 not enabled on the
 * account, or another failure: a network error, a token without R2
 * permission), printing nothing (the caller decides what it means).
 * @param {string} bucket
 * @param {{cwd: string}} o
 * @returns {Promise<{ok: boolean, notEnabled: boolean}>}
 */
export async function createBucket(bucket, o) {
  const r = await runWrangler(["r2", "bucket", "create", bucket], { cwd: o.cwd, quiet: true });
  if (r.code === 0 || /already exists|already own|10004/i.test(r.output)) {
    ui.ok(`R2 bucket ${bold(bucket)} ready (files above FILE_INLINE_MAX go there, up to 95 MiB each)`);
    return { ok: true, notEnabled: false };
  }
  return { ok: false, notEnabled: /enable R2|10042|not.*(enabled|subscribed)/i.test(r.output) };
}

/**
 * The hub URL in wrangler's deploy output: the Worker's workers.dev URL, else
 * the first custom domain or route.
 */
export function findURL(output, worker) {
  const urls = [...String(output).matchAll(/https:\/\/[a-z0-9.-]+\.workers\.dev/gi)].map((m) => m[0]);
  const exact = urls.find((u) => u.startsWith(`https://${worker}.`));
  if (exact || urls[0]) return exact ?? urls[0];
  const custom = String(output).match(/^\s+([a-z0-9.-]+\.[a-z]{2,})(?:\/\*)? \((?:custom domain|route)/im);
  return custom ? `https://${custom[1].replace(/^\*\./, "")}` : "";
}

