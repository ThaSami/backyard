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
//   BACKYARD_CF_DEPLOY_ARGS   extra `wrangler deploy` arguments (split on spaces)
//   BACKYARD_CF_DIR           the Worker project (default: deploy/cloudflare)
//   BACKYARD_CF_WAIT_SECONDS  how long to wait for /__backyard/readyz (default 60, 0 = skip)
//   WRANGLER_BIN              wrangler entry point (default: the project's node_modules)
//
// Secrets are uploaded together with the new version (`wrangler deploy
// --secrets-file`), so a first deploy never runs without BACKYARD_SECRET. In
// GitHub Actions the hub URL is written to $GITHUB_OUTPUT (url, worker) and a
// summary to $GITHUB_STEP_SUMMARY; secret values are masked and never logged.

import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { appendFileSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

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

/** Splits BACKYARD_CF_DEPLOY_ARGS on whitespace. Secrets are refused. */
export function parseDeployArgs(raw) {
  const args = String(raw ?? "").split(/\s+/).filter(Boolean);
  for (const a of args) {
    if (/^--secrets?-file/.test(a)) throw new CIError("BACKYARD_CF_DEPLOY_ARGS may not set --secrets-file (secrets come from the environment)");
    const m = /(?:^|=)([A-Z_][A-Z0-9_]*):/.exec(a);
    if (m && SECRET_NAMES.includes(m[1])) {
      throw new CIError(`BACKYARD_CF_DEPLOY_ARGS: ${m[1]} is a secret; never pass it with --var`);
    }
  }
  return args;
}

/**
 * Builds the effective wrangler configuration: wrangler.jsonc with variable
 * overrides and the optional R2 binding. Returns null when nothing changes.
 */
export function effectiveConfig(config, { vars = {}, bucket = "" } = {}) {
  if (!Object.keys(vars).length && !bucket) return null;
  const out = { ...config, vars: { ...(config.vars ?? {}), ...vars } };
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
 * Extracts the secret names from the stdout of `wrangler secret list
 * --format json`. Wrangler may print other lines around the array (e.g.
 * "▲ [WARNING] Proxy environment variables detected", or "[…]" notices), so
 * every line-initial "[" is tried with every line-final "]" after it, and only
 * a JSON array of {name: "<non-empty string>"} objects counts (the same rules
 * as deploy/cloudflare/scripts/setup.mjs).
 *
 * An empty result makes the deploy generate BACKYARD_SECRET, which would
 * rotate it (and revoke every API token) on an existing Worker, so an empty
 * list must never win by accident: a non-empty list anywhere in the output
 * beats a stray "[]" line, and "[]" next to some other non-empty JSON array
 * (a list in a format this parser does not know) is ambiguous and throws.
 * Anything else throws too: never guess.
 */
export function parseSecretList(output) {
  const s = String(output ?? "");
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
  throw new CIError(
    foreign
      ? "unexpected `wrangler secret list` output (a JSON array that is not a list of {name} objects)"
      : "unexpected `wrangler secret list` output (no JSON list of secrets)",
  );
}

/**
 * True when wrangler reports that the Worker does not exist yet (`Worker
 * "<name>" not found.`, API error 10007). Anything else (authentication,
 * network) must not be mistaken for a first deploy.
 */
export function isMissingWorker(output) {
  return /Worker "[^"]*"(?: \(env: [^)]*\))? not found|\[code: 10007\]|does not exist on your account/i.test(String(output ?? ""));
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

function gha(env) {
  const on = env.GITHUB_ACTIONS === "true";
  return {
    mask: (v) => on && v && process.stdout.write(`::add-mask::${v}\n`),
    warn: (msg) => console.log(on ? `::warning title=Backyard on Cloudflare::${msg}` : `warning: ${msg}`),
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
  const vars = parseVars(env.BACKYARD_CF_VARS);
  const extraArgs = parseDeployArgs(env.BACKYARD_CF_DEPLOY_ARGS);
  const merged = { ...(config.vars ?? {}), ...vars };
  const ci = gha(env);
  for (const name of SECRET_NAMES) ci.mask(env[name]);

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
      if (listed.code === 0) {
        existing = parseSecretList(listed.stdout);
      } else if (isMissingWorker(listed.output)) {
        log(`Worker ${worker} does not exist yet: first deploy.`);
        existing = new Set();
      } else {
        // Never guess here: a wrong guess would rotate BACKYARD_SECRET and
        // revoke every API token.
        throw new CIError(`could not list the secrets of Worker ${worker}:\n${listed.output.trim()}`);
      }
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

    const effective = effectiveConfig(config, { vars, bucket });
    // --env= selects the top-level environment explicitly (wrangler.jsonc also
    // defines "local" for offline development).
    const args = ["deploy", "--name", worker, "--env="];
    if (effective) {
      writeFileSync(generatedPath, `// Generated by scripts/cloudflare-ci.mjs from wrangler.jsonc; do not edit.\n${JSON.stringify(effective, null, 2)}\n`);
      wroteConfig = true;
      args.push("--config", generatedPath);
      if (Object.keys(vars).length) log(`Variable overrides: ${Object.keys(vars).sort().join(", ")}`);
    }
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
