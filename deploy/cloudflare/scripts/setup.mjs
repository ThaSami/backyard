#!/usr/bin/env node
// Deploys (or upgrades) Backyard on your Cloudflare account, free plan included.
// Safe to re-run: existing secrets are kept, the deploy token is remembered.
//
//   npm run setup                         log in (browser) if needed, deploy, set secrets
//                                         (and an R2 bucket for big files when R2 is enabled)
//   npm run setup -- --no-r2              never use R2 (files capped at 25 MiB)
//   npm run setup -- --worker-name team   deploy under another Worker name
//   npm run setup -- --var AUTH_MODE:access --var ACCESS_TEAM_DOMAIN:acme --var ACCESS_AUD:<aud>
//   npm run setup -- --rotate-token       issue a new DEPLOY_TOKEN
//
// Non-interactive use (CI): set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID.
// State (the deploy token, URL, account, bucket, --var overrides) is kept in
// $XDG_CONFIG_HOME/backyard/cloudflare/<worker>.json (mode 0600), so a plain
// re-run keeps every earlier choice.

import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { createInterface } from "node:readline";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const pkgDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const GENERATED_CONFIG = "wrangler.setup.jsonc";
const color = process.stdout.isTTY && !process.env.NO_COLOR;
const paint = (code) => (s) => (color ? `\x1b[${code}m${s}\x1b[0m` : String(s));
const bold = paint("1");
const dim = paint("2");
const green = paint("32");
const yellow = paint("33");
const cyan = paint("36");
const red = paint("31");

function fail(msg) {
  console.error(`${red("✗")} ${msg}`);
  process.exit(1);
}

const HELP = `Usage: npm run setup -- [options]

Deploys Backyard to your Cloudflare account (Workers free plan) and prints the
"backyard login" command. Re-running upgrades the Worker and keeps its secrets.

Options:
  --worker-name <name>   Worker name (default: "name" in wrangler.jsonc)
  --r2                   use an R2 bucket for big files (the default when R2 is
                         enabled on the account; re-enables after --no-r2)
  --r2-bucket <name>     bucket name (default: <worker>-files)
  --no-r2                never use R2 (remembered; files are capped at 25 MiB)
  --var KEY:VALUE        override a variable of wrangler.jsonc (repeatable), e.g.
                         --var URL_MODE:host --var BASE_DOMAIN:backyard.acme.com
                         Remembered for later runs; --var KEY: (empty) drops it.
  --rotate-token         replace DEPLOY_TOKEN with a new random token
  --rotate-secret        replace BACKYARD_SECRET (revokes every API token)
  --skip-deploy          only (re)set secrets
  -h, --help             show this help
`;

function parseArgs(argv) {
  const o = { vars: {}, r2: undefined, r2Bucket: "", workerName: "", rotateToken: false, rotateSecret: false, skipDeploy: false };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    const next = () => {
      const v = argv[++i];
      if (v === undefined) fail(`${a} needs a value`);
      return v;
    };
    const [flag, inline] = a.includes("=") && a.startsWith("--") ? [a.slice(0, a.indexOf("=")), a.slice(a.indexOf("=") + 1)] : [a, undefined];
    const val = () => (inline !== undefined ? inline : next());
    switch (flag) {
      case "-h":
      case "--help":
        console.log(HELP);
        process.exit(0);
        break;
      case "--worker-name":
      case "--name":
        o.workerName = val();
        break;
      case "--r2":
        o.r2 = true;
        break;
      case "--no-r2":
        o.r2 = false;
        break;
      case "--r2-bucket":
        o.r2Bucket = val();
        o.r2 = true;
        break;
      case "--var": {
        const v = val();
        const m = /^([A-Z_][A-Z0-9_]*):([\s\S]*)$/.exec(v);
        if (!m) fail(`--var expects KEY:VALUE, got ${JSON.stringify(v)}`);
        o.vars[m[1]] = m[2];
        break;
      }
      case "--rotate-token":
        o.rotateToken = true;
        break;
      case "--rotate-secret":
        o.rotateSecret = true;
        break;
      case "--skip-deploy":
        o.skipDeploy = true;
        break;
      default:
        fail(`unknown option ${a}\n\n${HELP}`);
    }
  }
  if (o.workerName && !/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(o.workerName)) fail(`invalid worker name ${JSON.stringify(o.workerName)}`);
  if (o.r2Bucket && !/^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$/.test(o.r2Bucket)) fail(`invalid bucket name ${JSON.stringify(o.r2Bucket)}`);
  return o;
}

/** Strips // and /* *\/ comments and trailing commas from JSONC (string-aware). */
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
    } else {
      out += c;
    }
  }
  return JSON.parse(out.replace(/,(\s*[}\]])/g, "$1"));
}

function wranglerBin() {
  const p = join(pkgDir, "node_modules", "wrangler", "bin", "wrangler.js");
  if (!existsSync(p)) fail(`wrangler is not installed: run ${bold("npm ci")} in ${pkgDir} first`);
  return p;
}

/**
 * Runs wrangler. Output is streamed (unless quiet) and captured; stdin is
 * inherited so wrangler's prompts work, or fed from opts.input.
 */
function wrangler(args, opts = {}) {
  return new Promise((resolvePromise) => {
    const child = spawn(process.execPath, [wranglerBin(), ...args], {
      cwd: pkgDir,
      env: { ...process.env, WRANGLER_SEND_METRICS: process.env.WRANGLER_SEND_METRICS ?? "false" },
      stdio: [opts.input !== undefined ? "pipe" : "inherit", "pipe", "pipe"],
    });
    if (opts.input !== undefined) {
      child.stdin.write(opts.input);
      child.stdin.end();
    }
    let output = "";
    const onData = (stream) => (chunk) => {
      output += chunk.toString();
      if (!opts.quiet) stream.write(chunk);
    };
    child.stdout.on("data", onData(process.stdout));
    child.stderr.on("data", onData(process.stderr));
    child.on("error", (e) => resolvePromise({ code: 1, output: String(e) }));
    child.on("close", (code) => resolvePromise({ code: code ?? 1, output }));
  });
}

function stateFile(worker) {
  const base = process.env.XDG_CONFIG_HOME || join(homedir(), ".config");
  return join(base, "backyard", "cloudflare", `${worker}.json`);
}

function loadState(worker) {
  try {
    return JSON.parse(readFileSync(stateFile(worker), "utf8"));
  } catch {
    return {};
  }
}

function saveState(worker, state) {
  const p = stateFile(worker);
  mkdirSync(dirname(p), { recursive: true, mode: 0o700 });
  writeFileSync(p, JSON.stringify(state, null, 2) + "\n", { mode: 0o600 });
  return p;
}

/** Parses `wrangler whoami --json` ({loggedIn, email, accounts: [{id, name}]}); null when not logged in. */
function parseWhoami(r) {
  if (r.code !== 0) return null;
  try {
    const j = JSON.parse(r.output.slice(r.output.indexOf("{"), r.output.lastIndexOf("}") + 1));
    if (!j.loggedIn) return null;
    const accounts = Array.isArray(j.accounts)
      ? j.accounts.filter((a) => a && typeof a.id === "string").map((a) => ({ id: a.id, name: String(a.name ?? a.id) }))
      : [];
    return { email: typeof j.email === "string" ? j.email : "", accounts };
  } catch {
    return { email: "", accounts: [] };
  }
}

/** Makes sure wrangler can act for us; returns who we are (accounts may be empty for scoped API tokens). */
async function ensureLogin() {
  const fromEnv = Boolean(process.env.CLOUDFLARE_API_TOKEN);
  if (fromEnv) console.log(dim("Using CLOUDFLARE_API_TOKEN from the environment."));
  if (fromEnv && process.env.CLOUDFLARE_ACCOUNT_ID) return { email: "", accounts: [] };
  let who = parseWhoami(await wrangler(["whoami", "--json"], { quiet: true }));
  if (who) {
    if (!fromEnv) console.log(`${green("✓")} Logged in to Cloudflare${who.email ? ` as ${bold(who.email)}` : ""}`);
    return who;
  }
  if (fromEnv) return { email: "", accounts: [] }; // let wrangler report token problems itself
  if (!process.stdin.isTTY) fail("not logged in to Cloudflare: run `npx wrangler login`, or set CLOUDFLARE_API_TOKEN (and CLOUDFLARE_ACCOUNT_ID)");
  console.log(`${yellow("!")} wrangler is not logged in — opening the Cloudflare login in your browser…`);
  const login = await wrangler(["login"]);
  if (login.code !== 0) fail("wrangler login failed");
  who = parseWhoami(await wrangler(["whoami", "--json"], { quiet: true }));
  if (!who) fail("wrangler login did not complete");
  return who;
}

function ask(question) {
  return new Promise((resolveAnswer) => {
    const rl = createInterface({ input: process.stdin, output: process.stdout });
    rl.question(question, (answer) => {
      rl.close();
      resolveAnswer(answer.trim());
    });
  });
}

/**
 * Picks the Cloudflare account to deploy to (like OpenQuick): CLOUDFLARE_ACCOUNT_ID,
 * the one used last time, the only one, or a choice. Every later wrangler call
 * gets it through CLOUDFLARE_ACCOUNT_ID, so none of them stops to ask (a quiet
 * call would wait for an answer to a question nobody sees).
 */
async function pickAccount(accounts, saved) {
  if (process.env.CLOUDFLARE_ACCOUNT_ID) return process.env.CLOUDFLARE_ACCOUNT_ID;
  if (saved && accounts.some((a) => a.id === saved)) return saved;
  if (accounts.length === 0) return "";
  if (accounts.length === 1) return accounts[0].id;
  if (!process.stdin.isTTY) {
    fail(`this login can deploy to ${accounts.length} Cloudflare accounts: set CLOUDFLARE_ACCOUNT_ID to one of ${accounts.map((a) => a.id).join(", ")}`);
  }
  console.log(`\nYour Cloudflare login can deploy to ${accounts.length} accounts:`);
  accounts.forEach((a, i) => console.log(`  ${bold(String(i + 1))}. ${a.name} ${dim(a.id)}`));
  for (;;) {
    const answer = await ask(`Deploy Backyard to which account? ${dim(`[1-${accounts.length}]`)} `);
    const i = Number(answer) - 1;
    if (Number.isInteger(i) && accounts[i]) return accounts[i].id;
    console.log(yellow("  Please enter a number from the list."));
  }
}

/**
 * Names of the Worker's secrets. A Worker that does not exist yet has none;
 * any other failure stops the setup, so that a hiccup can never make it
 * replace BACKYARD_SECRET (which would revoke every token).
 */
async function existingSecrets(worker) {
  const r = await wrangler(["secret", "list", "--name", worker, "--format", "json"], { quiet: true });
  if (r.code !== 0) {
    if (/Worker "[^"]*"(?: \(env: [^)]*\))? not found|\[code: 10007\]|does not exist on your account/i.test(r.output)) return new Set();
    fail(`could not list the secrets of Worker ${worker}:\n${r.output.trim()}`);
  }
  const end = r.output.lastIndexOf("]");
  for (let i = r.output.indexOf("["); i >= 0 && i < end; i = r.output.indexOf("[", i + 1)) {
    if (i > 0 && r.output[i - 1] !== "\n") continue;
    try {
      const list = JSON.parse(r.output.slice(i, end + 1));
      if (Array.isArray(list)) return new Set(list.map((x) => x?.name).filter(Boolean));
    } catch {
      /* try the next "[" */
    }
  }
  fail(`unexpected output of wrangler secret list:\n${r.output.trim()}`);
}

async function putSecret(worker, name, value) {
  const r = await wrangler(["secret", "put", name, "--name", worker], { input: value + "\n", quiet: true });
  if (r.code !== 0) fail(`could not set the ${name} secret:\n${r.output}`);
  console.log(`${green("✓")} Secret ${bold(name)} set`);
}

async function ensureBucket(bucket) {
  const r = await wrangler(["r2", "bucket", "create", bucket], { quiet: true });
  if (r.code === 0 || /already exists|already own|10004/i.test(r.output)) {
    console.log(`${green("✓")} R2 bucket ${bold(bucket)} ready (files above FILE_INLINE_MAX go there, up to 95 MiB each)`);
    return true;
  }
  const notEnabled = /enable R2|10042|not.*(enabled|subscribed)/i.test(r.output);
  console.log(
    `${yellow("!")} ${notEnabled ? "R2 is not enabled on this account" : `Could not create the R2 bucket ${bucket}`}` +
      ` (enable it once at dash.cloudflare.com → R2 — the free tier includes 10 GB — then re-run npm run setup).`,
  );
  console.log(dim("  Continuing without R2: everything works, files are just capped at 25 MiB each."));
  return false;
}

function findURL(output, worker) {
  const urls = [...output.matchAll(/https:\/\/[a-z0-9.-]+\.workers\.dev/gi)].map((m) => m[0]);
  const exact = urls.find((u) => u.startsWith(`https://${worker}.`));
  if (exact || urls[0]) return exact ?? urls[0];
  const custom = output.match(/^\s+([a-z0-9.-]+\.[a-z]{2,})(?:\/\*)? \((?:custom domain|route)/im);
  return custom ? `https://${custom[1].replace(/^\*\./, "")}` : "";
}

async function waitReady(url) {
  for (let i = 0; i < 15; i++) {
    try {
      // manual: a Cloudflare Access login redirect is not "ready".
      const res = await fetch(`${url}/__backyard/readyz`, { redirect: "manual", signal: AbortSignal.timeout(10_000) });
      if (res.status === 200) return true;
    } catch {
      /* not yet reachable */
    }
    await new Promise((r) => setTimeout(r, 3000));
  }
  return false;
}

async function main() {
  const opts = parseArgs(process.argv.slice(2));
  console.log(bold("\nBackyard on Cloudflare — setup\n"));

  // Bundle the SDK, hub and docs from the repository.
  await new Promise((res) => {
    const c = spawn(process.execPath, [join(pkgDir, "scripts", "sync-assets.mjs"), "--strict"], { stdio: "inherit" });
    c.on("close", (code) => (code === 0 ? res() : fail("syncing assets failed")));
  });

  const config = parseJSONC(readFileSync(join(pkgDir, "wrangler.jsonc"), "utf8"));
  const worker = opts.workerName || config.name || "backyard";
  const state = loadState(worker);

  const who = await ensureLogin();
  const accountId = await pickAccount(who.accounts, state.accountId);
  if (accountId) {
    process.env.CLOUDFLARE_ACCOUNT_ID = accountId;
    const name = who.accounts.find((a) => a.id === accountId)?.name;
    if (who.accounts.length > 1 || name) console.log(dim(`Account: ${name ? `${name} (${accountId})` : accountId}`));
  }
  state.accountId = accountId || state.accountId || undefined;

  // R2 is used automatically when the account has it (like OpenQuick); --no-r2
  // opts out for good, --r2 opts back in. A failed attempt is retried next run.
  if (opts.r2 !== undefined) state.r2 = opts.r2;
  let bucket = "";
  if (state.r2 !== false) {
    const name = opts.r2Bucket || state.r2Bucket || `${worker}-files`;
    if (await ensureBucket(name)) bucket = name;
  }
  state.r2Bucket = bucket || undefined;

  // Variable overrides: remembered across runs, an empty value drops one.
  const vars = { ...(state.vars ?? {}) };
  for (const [k, v] of Object.entries(opts.vars)) {
    if (v === "") delete vars[k];
    else vars[k] = v;
  }
  state.vars = Object.keys(vars).length ? vars : undefined;
  const merged = { ...(config.vars ?? {}), ...vars };

  // Secrets: keep what exists, generate what is missing. They are uploaded
  // with the new version (wrangler deploy --secrets-file), so a first deploy
  // never runs without BACKYARD_SECRET.
  const have = await existingSecrets(worker);
  const secrets = {};
  if (!have.has("BACKYARD_SECRET") || opts.rotateSecret) secrets.BACKYARD_SECRET = randomBytes(32).toString("hex");
  const newToken = () => "byd_" + randomBytes(24).toString("base64url");
  let token;
  if (process.env.DEPLOY_TOKEN) {
    token = process.env.DEPLOY_TOKEN; // explicit choice (e.g. CI)
    secrets.DEPLOY_TOKEN = token;
  } else if (opts.rotateToken) {
    token = newToken();
    secrets.DEPLOY_TOKEN = token;
  } else if (state.deployToken) {
    token = state.deployToken;
    if (!have.has("DEPLOY_TOKEN")) secrets.DEPLOY_TOKEN = token;
  } else {
    token = newToken();
    secrets.DEPLOY_TOKEN = token;
    if (have.has("DEPLOY_TOKEN")) console.log(`${yellow("!")} No saved deploy token for ${worker}: issuing a new DEPLOY_TOKEN (the old one stops working).`);
  }

  let url = state.url || "";
  if (!opts.skipDeploy) {
    // --env= targets the top-level environment explicitly (wrangler.jsonc also defines "local").
    const args = ["deploy", "--name", worker, "--env="];
    let generated = "";
    if (bucket || state.vars) {
      // wrangler.jsonc plus the R2 binding and the overrides, written next to it
      // so relative paths resolve (vars as data: JSON values need no quoting).
      generated = join(pkgDir, GENERATED_CONFIG);
      const effective = { ...config, name: worker, vars: merged };
      if (bucket) effective.r2_buckets = [{ binding: "R2", bucket_name: bucket }];
      writeFileSync(generated, "// Generated by scripts/setup.mjs from wrangler.jsonc — do not edit.\n" + JSON.stringify(effective, null, 2) + "\n");
      args.push("--config", generated);
    }
    // A private temporary file (0600), removed right after the deploy.
    const secretDir = Object.keys(secrets).length ? mkdtempSync(join(tmpdir(), "backyard-setup-")) : "";
    if (secretDir) {
      const secretsFile = join(secretDir, "secrets.json");
      writeFileSync(secretsFile, JSON.stringify(secrets), { mode: 0o600 });
      args.push("--secrets-file", secretsFile);
    }
    if (state.vars) console.log(dim(`Variable overrides: ${Object.keys(vars).sort().join(", ")}`));
    console.log(`\nDeploying Worker ${bold(worker)}${bucket ? ` with R2 bucket ${bold(bucket)}` : ""}…\n`);
    let r;
    try {
      r = await wrangler(args);
    } finally {
      if (generated) rmSync(generated, { force: true });
      if (secretDir) rmSync(secretDir, { recursive: true, force: true });
    }
    if (r.code !== 0) {
      if (/workers\.dev subdomain/i.test(r.output)) {
        fail("register a workers.dev subdomain first: dash.cloudflare.com → Workers & Pages → (set up your subdomain), then re-run npm run setup");
      }
      fail("wrangler deploy failed (see the output above)");
    }
    url = findURL(r.output, worker) || url;
    for (const name of ["BACKYARD_SECRET", "DEPLOY_TOKEN"]) {
      console.log(`${green("✓")} Secret ${bold(name)} ${secrets[name] !== undefined ? "set" : "kept"}`);
    }
  } else {
    for (const name of ["BACKYARD_SECRET", "DEPLOY_TOKEN"]) {
      if (secrets[name] !== undefined) await putSecret(worker, name, secrets[name]);
      else console.log(`${green("✓")} Secret ${bold(name)} kept`);
    }
  }
  // A custom domain (BASE_DOMAIN / PUBLIC_URL) is the hub URL to hand out.
  const publicURL = String(merged.PUBLIC_URL ?? "").replace(/\/+$/, "");
  if (publicURL) url = publicURL;
  else if (merged.BASE_DOMAIN) url = `https://${merged.BASE_DOMAIN}`;

  state.deployToken = token;
  state.url = url || undefined;
  state.updatedAt = new Date().toISOString();
  const saved = saveState(worker, state);

  if (url) {
    process.stdout.write(`\nWaiting for ${url} to answer…`);
    const ready = await waitReady(url);
    console.log(ready ? ` ${green("ready")}` : ` ${yellow("not yet (workers.dev can take a minute on a first deploy)")}`);
  }

  const hub = url || "https://<your-worker>.<your-subdomain>.workers.dev";
  console.log(`\n${green("✔")} Backyard is live at ${bold(cyan(hub))}\n`);
  console.log("Connect the CLI (keep the token secret — it can deploy and delete sites):\n");
  console.log(`  ${bold(`backyard login --server ${hub} --token ${token}`)}\n`);
  console.log(`Then: ${bold("backyard init my-site && cd my-site && backyard deploy")}`);
  console.log(dim(`\nThe deploy token is saved in ${saved}.`));
  console.log(dim(`AI runs on Workers AI (free daily allowance); caps: LIMITS in wrangler.jsonc.`));
  const noR2 = state.r2 === false ? "R2 is off (--r2 turns it on)" : "enable R2 on the account and re-run setup for files up to 95 MiB";
  console.log(dim(`Files: ${bucket ? `R2 bucket ${bucket} for files above FILE_INLINE_MAX` : `stored in Durable Objects (25 MiB per file); ${noR2}`}.`));
  console.log(dim("Custom domain, host mode and Cloudflare Access: see README.md and deploy/terraform/cloudflare.\n"));
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((e) => fail(e?.stack || String(e)));
}
