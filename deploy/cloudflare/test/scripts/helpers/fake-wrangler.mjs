// A fake `wrangler` for the engine's tests (installed by helpers/sandbox.mjs as
// node_modules/wrangler with the real exports map, so the engine resolves it
// exactly like the real one). It never talks to Cloudflare: it reads and writes
// the fake Cloudflare state (FAKE_CF_STATE, shared with helpers/fake-cloudflare.mjs)
// and appends one JSON line per call to FAKE_LOG:
//
//   {args, cwd, account: $CLOUDFLARE_ACCOUNT_ID | null, env: {…the whole environment…},
//    code, config?: <parsed --config>, configPath?, secretsFile?: [names], secretsMode?: "600",
//    secret?: <name put>, stdinBytes?}
//
// Commands:
//   whoami --json            {loggedIn, email, accounts}: accounts from FAKE_ACCOUNTS (JSON list)
//                            or the state's accounts; logged out with FAKE_LOGGED_OUT=1 or state.loggedIn === false
//   login                    sets state.loggedIn = true (FAKE_LOGIN_FAIL=1 fails)
//   auth token --json        {"type":"api_token","token":$CLOUDFLARE_API_TOKEN} when set; else
//                            FAKE_AUTH_TYPE=api_key → {"type":"api_key","key":$FAKE_API_KEY,"email":$FAKE_API_EMAIL};
//                            else {"type":"oauth","token": $FAKE_OAUTH_TOKEN or OAUTH_TOKEN}; fails when logged out
//   r2 bucket create <name>  adds to state.buckets ("already exists" when present); FAKE_R2=disabled → code 10042
//   secret list --name <w> --format json
//                            the names of state.workers[w].secrets; "Worker not found [code: 10007]" when
//                            not deployed. FAKE_LIST_MODE = auth | network | stderr-json | garbage |
//                            stderr-warning plays the ways it can go wrong (FAKE_LIST_ERROR=1 = auth)
//   secret put <NAME> --name <w>   the value from stdin
//   deploy --name <w> --config <file> [--secrets-file <f>] [--dry-run --outdir <d>]
//                            FAILS (exit 1) when the config has "routes" or "route". --dry-run copies
//                            `main` into --outdir and prints the bindings, changing nothing. Otherwise
//                            records the deployed {vars, bindings, secrets (merged), workersDev, r2Bucket}
//                            in state.workers[w] and prints https://<w>.<subdomain>.workers.dev (when
//                            workers_dev is on). An account without a workers.dev subdomain and
//                            workers_dev on → the "register a workers.dev subdomain" error.
//                            FAKE_DEPLOY_FAIL=1 fails it; FAKE_DEPLOY_DELAY_MS=<ms> prints "Uploading (fake:
//                            slow)…" and blocks that long first (Ctrl-C tests). FAKE_AI=disabled fails a
//                            config with the Workers AI binding (an account where Workers AI is not allowed).
//   anything else            exit 2

import { appendFileSync, copyFileSync, existsSync, mkdirSync, readFileSync, statSync } from "node:fs";
import { basename, dirname, join, resolve } from "node:path";

import { OAUTH_TOKEN, bindingsOf, defaultCfState, readCfState, writeCfState } from "./fake-cloudflare.mjs";

const args = process.argv.slice(2);
const env = process.env;
const statePath = env.FAKE_CF_STATE;
const s = statePath ? readCfState(statePath) : defaultCfState();
const entry = { args, cwd: process.cwd(), account: env.CLOUDFLARE_ACCOUNT_ID ?? null, env: { ...env }, code: 0 };
const flag = (name) => {
  const i = args.indexOf(name);
  if (i >= 0) return args[i + 1];
  const pre = args.find((a) => a.startsWith(`${name}=`));
  return pre !== undefined ? pre.slice(name.length + 1) : undefined;
};
const readStdin = () => {
  try {
    return readFileSync(0, "utf8");
  } catch {
    return "";
  }
};
const loggedIn = () => !env.FAKE_LOGGED_OUT && s.loggedIn !== false;
const accountList = () => (env.FAKE_ACCOUNTS ? JSON.parse(env.FAKE_ACCOUNTS) : Object.entries(s.accounts ?? {}).map(([id, a]) => ({ id, name: a.name ?? id })));
const save = () => statePath && writeCfState(statePath, s);

/** JSONC as wrangler reads it: comments and trailing commas out, strings untouched. */
function parseJSONC(text) {
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
    } else noComments += c;
  }
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

function run() {
  switch (args[0]) {
    case "whoami":
      if (!loggedIn()) {
        console.log('{"loggedIn":false}');
        return 1;
      }
      console.log("noise before json");
      console.log(JSON.stringify({ loggedIn: true, email: "me@example.com", accounts: accountList() }));
      return 0;
    case "login":
      if (env.FAKE_LOGIN_FAIL) return 1;
      s.loggedIn = true;
      save();
      console.log("Successfully logged in.");
      return 0;
    case "auth": {
      if (args[1] !== "token") return 2;
      let out;
      if (env.CLOUDFLARE_API_TOKEN) out = { type: "api_token", token: env.CLOUDFLARE_API_TOKEN };
      else if (env.FAKE_AUTH_TYPE === "api_key") out = { type: "api_key", key: env.FAKE_API_KEY ?? "", email: env.FAKE_API_EMAIL ?? "" };
      else if (!loggedIn()) {
        console.error("✘ [ERROR] Not logged in. Please run `wrangler login` to authenticate.");
        return 1;
      } else out = { type: "oauth", token: env.FAKE_OAUTH_TOKEN || OAUTH_TOKEN };
      console.log(JSON.stringify(out, null, 2));
      return 0;
    }
    case "r2": {
      const name = args[3];
      if (env.FAKE_R2 === "disabled") {
        console.error("✘ [ERROR] A request to the Cloudflare API failed. Please enable R2 through the Cloudflare Dashboard. [code: 10042]");
        return 1;
      }
      s.buckets ??= [];
      if (s.buckets.includes(name)) {
        console.error(`✘ [ERROR] The bucket you tried to create already exists, and you own it. [code: 10004]`);
        return 1;
      }
      s.buckets.push(name);
      save();
      console.log(`Created bucket ${name}`);
      return 0;
    }
    case "secret": {
      const w = flag("--name");
      const worker = s.workers?.[w];
      const mode = env.FAKE_LIST_MODE || (env.FAKE_LIST_ERROR ? "auth" : "");
      if (args[1] === "list") {
        if (mode === "auth") {
          console.error("✘ [ERROR] Authentication error [code: 10000]");
          return 1;
        }
        if (mode === "network") {
          console.error("✘ [ERROR] fetch failed: getaddrinfo ENOTFOUND api.cloudflare.com (route not found)");
          return 1;
        }
        if (!worker?.deployments) {
          console.error(`✘ [ERROR] Worker "${w}" not found. [code: 10007]`);
          return 1;
        }
        // Exit 0 without the list on stdout (an empty "[]" only on stderr).
        if (mode === "stderr-json") {
          console.log("Listing secrets…");
          console.error("[]");
          return 0;
        }
        if (mode === "garbage") {
          console.log("Something unexpected happened");
          return 0;
        }
        // A warning on stderr that starts with "[" (what a proxy warning can look like without colors).
        if (mode === "stderr-warning") console.error("[WARNING] Proxy environment variables detected. We'll use your proxy for fetch requests.");
        console.log("▲ [WARNING] noise [x]\n" + JSON.stringify(Object.keys(worker.secrets ?? {}).map((name) => ({ name, type: "secret_text" }))));
        return 0;
      }
      if (args[1] === "put") {
        const value = readStdin().replace(/\n$/, "");
        entry.secret = args[2];
        entry.stdinBytes = value.length;
        s.workers ??= {};
        s.workers[w] ??= { account: env.CLOUDFLARE_ACCOUNT_ID ?? null, secrets: {}, deployments: 0 };
        s.workers[w].secrets ??= {};
        s.workers[w].secrets[args[2]] = value;
        const others = (s.workers[w].bindings ?? []).filter((b) => b.type !== "secret_text");
        s.workers[w].bindings = [...others, ...Object.keys(s.workers[w].secrets).sort().map((name) => ({ type: "secret_text", name }))];
        save();
        console.log(`✨ Success! Uploaded secret ${args[2]}`);
        return 0;
      }
      return 2;
    }
    case "deploy": {
      const w = flag("--name");
      const configPath = flag("--config");
      if (!configPath) {
        console.error("✘ [ERROR] fake wrangler: deploy needs --config");
        return 1;
      }
      const text = readFileSync(configPath, "utf8");
      const config = parseJSONC(text);
      entry.config = config;
      entry.configPath = configPath;
      if (config.routes !== undefined || config.route !== undefined) {
        console.error("✘ [ERROR] fake wrangler: the deployed config must never contain routes (they are managed by domain add)");
        return 1;
      }
      const name = w || config.name;
      if (args.includes("--dry-run")) {
        const outdir = flag("--outdir");
        const main = resolve(dirname(configPath), config.main ?? "");
        if (outdir) {
          mkdirSync(outdir, { recursive: true });
          if (existsSync(main)) copyFileSync(main, join(outdir, basename(main).replace(/\.ts$/, ".js")));
        }
        entry.outdir = outdir;
        console.log("Total Upload: 1.00 KiB / gzip: 0.50 KiB");
        console.log("Your Worker has access to the following bindings:");
        for (const b of bindingsOf(config, {})) console.log(`env.${b.name}${b.type === "plain_text" ? ` ("${b.text}")` : ""}  ${b.type}`);
        console.log("--dry-run: exiting now.");
        return 0;
      }
      if (env.FAKE_DEPLOY_FAIL) {
        console.error("✘ [ERROR] fake deploy failure");
        return 1;
      }
      if (env.FAKE_AI === "disabled" && config.ai) {
        console.error("✘ [ERROR] fake: Workers AI is not available on this account (binding AI)");
        return 1;
      }
      if (env.FAKE_DEPLOY_DELAY_MS) {
        // A slow upload (Ctrl-C tests): blocks before anything is recorded.
        console.log("Uploading (fake: slow)…");
        Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, Number(env.FAKE_DEPLOY_DELAY_MS));
      }
      const accounts = accountList();
      const account = env.CLOUDFLARE_ACCOUNT_ID || (accounts.length === 1 ? accounts[0].id : "");
      if (!account) {
        console.error("✘ [ERROR] More than one account available but unable to select one in non-interactive mode.");
        return 1;
      }
      const sub = s.accounts?.[account]?.subdomain;
      const workersDev = config.workers_dev !== false;
      if (workersDev && !sub) {
        console.error(
          "▲ [WARNING] You need to register a workers.dev subdomain before publishing to workers.dev\n" +
            `✘ [ERROR] You can either deploy your worker to one or more routes by specifying them in your wrangler.json file, or register a workers.dev subdomain here:\nhttps://dash.cloudflare.com/${account}/workers/onboarding`,
        );
        return 1;
      }
      s.workers ??= {};
      const prev = s.workers[name] ?? {};
      const secrets = { ...(prev.secrets ?? {}) };
      const sf = flag("--secrets-file");
      if (sf) {
        const file = JSON.parse(readFileSync(sf, "utf8"));
        entry.secretsFile = Object.keys(file).sort();
        entry.secretsMode = (statSync(sf).mode & 0o777).toString(8);
        Object.assign(secrets, file);
      }
      const r2 = (config.r2_buckets ?? []).find((b) => b.binding === "R2");
      s.workers[name] = {
        ...prev,
        account,
        vars: config.vars ?? {},
        bindings: bindingsOf(config, secrets),
        secrets,
        workersDev,
        r2Bucket: r2?.bucket_name ?? null,
        compatibilityDate: config.compatibility_date,
        deployments: (prev.deployments ?? 0) + 1,
        version: env.FAKE_WORKER_VERSION || prev.version || "0.1.0-dev",
      };
      save();
      console.log(`Uploaded ${name} (0.42 sec)`);
      console.log(`Deployed ${name} triggers (0.21 sec)`);
      if (workersDev) console.log(`  https://${name}.${sub}.workers.dev`);
      console.log(`Current Version ID: ${String(s.workers[name].deployments).padStart(8, "0")}-fake`);
      return 0;
    }
    default:
      return 2;
  }
}

let code;
try {
  code = run();
} catch (e) {
  console.error(`✘ [ERROR] fake wrangler crashed: ${e?.stack ?? e}`);
  code = 70;
}
entry.code = code;
if (env.FAKE_LOG) appendFileSync(env.FAKE_LOG, JSON.stringify(entry) + "\n");
process.exit(code);
