// Tests of the engine's command line (node --test): the parser and help that
// come from scripts/lib/commands.json, the dispatch of bin/backyard-cloudflare.mjs,
// --version, exit codes, the invoked-as hints, the Node.js version check and
// how wrangler is resolved.
import assert from "node:assert/strict";
import { existsSync, readFileSync, rmSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join, resolve } from "node:path";
import { after, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { detectInvokedAs, versionLine } from "../../scripts/cli.mjs";
import { COMMANDS, camel, formatHelp, parseArgs } from "../../scripts/lib/args.mjs";
import { CliError } from "../../scripts/lib/ui.mjs";
import { findURL, parseAuthToken, parseWhoami, wranglerBin, wranglerEnv, wranglerVersion } from "../../scripts/lib/wrangler.mjs";
import { createSandbox, ttyAvailable } from "./helpers/sandbox.mjs";

const pkgDir = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const sandboxes = [];
async function newSandbox(o) {
  const sb = await createSandbox(o);
  sandboxes.push(sb);
  return sb;
}
after(async () => {
  for (const sb of sandboxes) await sb.close();
});

/** The binding content of design §3.8: every command and its flags. */
const EXPECTED = {
  global: ["worker-name", "yes", "non-interactive", "help"],
  setup: ["r2", "no-r2", "r2-bucket", "ai", "no-ai", "site-listing", "var", "rotate-token", "rotate-secret", "skip-deploy", "workers-subdomain", "workers-dev", "no-workers-dev", "confirm-auth-change", "dry-run"],
  "domain add": ["host-mode", "zone", "take-over", "replace-dns-record", "wait"],
  "domain remove": [],
  "access enable": ["allow-email", "allow-email-domain", "team", "aud", "wait"],
  "access disable": ["delete-apps"],
  status: ["json"],
};

const usageError = (fn, re) =>
  assert.throws(fn, (e) => {
    assert.ok(e instanceof CliError, String(e));
    assert.equal(e.code, 2);
    if (re) assert.match(e.message, re);
    return true;
  });

describe("lib/commands.json", () => {
  it("has exactly the commands and flags of the design", () => {
    assert.equal(COMMANDS.schema, 1);
    assert.deepEqual(COMMANDS.global.map((f) => f.name), EXPECTED.global);
    assert.deepEqual(Object.fromEntries(COMMANDS.commands.map((c) => [c.name, c.flags.map((f) => f.name)])), Object.fromEntries(Object.entries(EXPECTED).filter(([k]) => k !== "global")));
    assert.deepEqual(COMMANDS.commands.find((c) => c.name === "domain add").args, ["host"]);
    const g = Object.fromEntries(COMMANDS.global.map((f) => [f.name, f]));
    assert.deepEqual(g["worker-name"].alias, ["name"]);
    assert.equal(g.yes.short, "y");
    assert.equal(g.help.short, "h");
    for (const c of COMMANDS.commands) {
      assert.ok(c.summary, c.name);
      for (const f of [...c.flags, ...COMMANDS.global]) {
        assert.ok(f.type === "bool" || f.type === "string", `${c.name} --${f.name}`);
        assert.ok(f.help, `${c.name} --${f.name}`);
      }
    }
    const repeatable = COMMANDS.commands.flatMap((c) => c.flags.filter((f) => f.repeatable).map((f) => `${c.name} --${f.name}`));
    assert.deepEqual(repeatable, ["setup --var", "access enable --allow-email", "access enable --allow-email-domain"]);
  });
});

describe("lib/args.mjs", () => {
  it("parses every flag of every command, as --f v and --f=v", () => {
    for (const c of COMMANDS.commands) {
      for (const style of ["space", "equals"]) {
        const argv = c.name.split(" ");
        const expected = {};
        for (const [i, a] of c.args.entries()) {
          argv.push(`value-${i}.example.com`);
          expected[camel(a)] = `value-${i}.example.com`;
        }
        for (const f of [...c.flags, ...COMMANDS.global.filter((g) => g.name !== "help")]) {
          const key = camel(f.name);
          if (f.type === "bool") {
            argv.push(`--${f.name}`);
            expected[key] = true;
            continue;
          }
          const v = f.name === "wait" ? "42" : f.name === "var" ? "KEY:a=b" : `v-${f.name}`;
          const values = f.repeatable ? [v, `${v}2`] : [v];
          for (const x of values) argv.push(...(style === "space" ? [`--${f.name}`, x] : [`--${f.name}=${x}`]));
          expected[key] = f.name === "wait" ? 42 : f.repeatable ? values : v;
        }
        const p = parseArgs(argv);
        assert.equal(p.kind, "run", c.name);
        assert.equal(p.command, c.name);
        for (const [k, v] of Object.entries(expected)) assert.deepEqual(p.opts[k], v, `${c.name} ${style} ${k}`);
      }
    }
    // Defaults: bools false, repeatables empty, strings undefined.
    const p = parseArgs(["setup"]);
    assert.equal(p.opts.noR2, false);
    assert.deepEqual(p.opts.var, []);
    assert.equal(p.opts.workerName, undefined);
    // The alias and the short flags.
    assert.equal(parseArgs(["status", "--name", "team"]).opts.workerName, "team");
    assert.equal(parseArgs(["access", "disable", "-y"]).opts.yes, true);
    assert.equal(parseArgs(["domain", "add", "x.example.com", "--wait", "0"]).opts.wait, 0);
    assert.deepEqual(parseArgs(["setup", "--var", "A:--looks-like-a-flag"]).opts.var, ["A:--looks-like-a-flag"]);
  });

  it("rejects unknown flags, missing values, extra or missing positionals and bad --wait (exit 2, with usage)", () => {
    usageError(() => parseArgs(["setup", "--bogus"]), /unknown option --bogus[\s\S]*Usage: backyard-cloudflare setup \[options\]/);
    usageError(() => parseArgs(["setup", "--host-mode"]), /unknown option --host-mode/);
    usageError(() => parseArgs(["setup", "--worker-name"]), /--worker-name needs a value/);
    usageError(() => parseArgs(["setup", "--dry-run=yes"]), /--dry-run takes no value/);
    usageError(() => parseArgs(["setup", "extra"]), /unexpected argument "extra"/);
    usageError(() => parseArgs(["domain", "add"]), /domain add needs <host>[\s\S]*Usage: backyard-cloudflare domain add <host>/);
    usageError(() => parseArgs(["domain", "add", "a.example.com", "b.example.com"]), /unexpected argument "b\.example\.com"/);
    usageError(() => parseArgs(["domain", "remove", "a.example.com"]), /unexpected argument/);
    for (const bad of ["-1", "1.5", "abc", "3601", ""]) usageError(() => parseArgs(["domain", "add", "a.example.com", "--wait", bad]), /--wait must be a whole number of seconds from 0 to 3600/);
    assert.equal(parseArgs(["access", "enable", "--wait", "3600"]).opts.wait, 3600);
    usageError(() => parseArgs(["deploy"]), /unknown command "deploy"/);
    usageError(() => parseArgs(["domain"]), /domain needs a subcommand: domain add, domain remove/);
    usageError(() => parseArgs(["access", "on"]), /unknown command "access on"/);
    usageError(() => parseArgs(["help", "bogus"]), /unknown command "bogus"/);
    usageError(() => parseArgs(["--version", "x"]), /--version takes no arguments/);
    // --help wins over everything else on the line.
    assert.deepEqual(
      (({ kind, command, exitCode }) => ({ kind, command, exitCode }))(parseArgs(["setup", "--bogus", "--help"])),
      { kind: "help", command: "setup", exitCode: 0 },
    );
    assert.equal(parseArgs(["domain", "add", "-h"]).command, "domain add");
    assert.equal(parseArgs(["help", "access", "enable"]).command, "access enable");
    assert.equal(parseArgs(["domain", "--help"]).group, "domain");
    assert.equal(parseArgs([]).exitCode, 2);
    assert.equal(parseArgs(["--help"]).exitCode, 0);
  });

  it("lists every flag in the help", () => {
    for (const c of COMMANDS.commands) {
      const h = formatHelp(c.name, { invokedAs: "npx backyard-cloudflare" });
      assert.ok(h.startsWith(`Usage: npx backyard-cloudflare ${c.name}${c.args.map((a) => ` <${a}>`).join("")} [options]`), h);
      for (const f of [...c.flags, ...COMMANDS.global]) {
        assert.ok(h.includes(`--${f.name}`), `${c.name}: --${f.name}`);
        assert.ok(h.includes(f.help), `${c.name}: help of --${f.name}`);
      }
      assert.ok(h.includes("--name"), "the alias");
      assert.match(h, /Exit codes: 0 done · 1 failed .* 2 usage.* 3 refused for safety .* 4 applied, verification pending .* 130 interrupted/);
    }
    const general = formatHelp("", { invokedAs: "backyard cloudflare" });
    for (const c of COMMANDS.commands) assert.ok(general.includes(c.name), c.name);
    assert.match(general, /--version/);
    assert.match(general, /Run `backyard cloudflare <command> --help`/);
    assert.match(formatHelp("", { group: "access" }), /access enable[\s\S]*access disable/);
    assert.doesNotMatch(formatHelp("", { group: "access" }), /domain add/);
  });
});

describe("bin/backyard-cloudflare.mjs", () => {
  it("prints the help and exits 2 without a command, 0 with --help", async () => {
    const sb = await newSandbox();
    let r = await sb.run([]);
    assert.equal(r.code, 2, r.out);
    assert.match(r.stdout, /^Usage: npm run cf -- <command> \[options\]/);
    r = await sb.run(["--help"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.stdout, /Commands:/);
    r = await sb.run(["help", "setup"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.stdout, /Usage: npm run cf -- setup \[options\]/);
    r = await sb.run(["domain", "add", "--help"], { BACKYARD_CF_INVOKED_AS: "backyard cloudflare" });
    assert.equal(r.code, 0, r.out);
    assert.match(r.stdout, /^Usage: backyard cloudflare domain add <host> \[options\]/);
    assert.equal(sb.allCalls().length, 0, "help runs no wrangler");
    assert.equal(sb.fake.requests.length, 0);
  });

  it("exits 2 for usage errors and an invalid worker name, before anything runs", async () => {
    const sb = await newSandbox();
    for (const argv of [["setup", "--bogus"], ["frobnicate"], ["domain", "add"], ["status", "--wait", "5"], ["setup", "--worker-name", "Bad_Name"]]) {
      const r = await sb.run(argv);
      assert.equal(r.code, 2, `${argv.join(" ")}: ${r.out}`);
      assert.match(r.stderr, /✗ /);
    }
    assert.equal(sb.allCalls().length, 0);
  });

  it("prints its version and wrangler's", async () => {
    let sb = await newSandbox();
    let r = await sb.run(["--version"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(r.stdout, "backyard-cloudflare 0.5.0-dev (source checkout) (wrangler 4.149.0)\n");
    sb = await newSandbox({ layout: "packaged", version: "1.2.3-test" });
    r = await sb.run(["--version"]);
    assert.equal(r.code, 0, r.out);
    assert.equal(r.stdout, "backyard-cloudflare 1.2.3-test (wrangler 4.149.0)\n");
    assert.match(versionLine({ version: "9.9.9" }), /^backyard-cloudflare 9\.9\.9 \(wrangler \d+\.\d+\.\d+\)$/);
  });

  it("refuses Node.js older than 22 (exit 2)", async () => {
    const sb = await newSandbox();
    const stub = 'data:text/javascript,Object.defineProperty(process.versions,"node",{value:"20.11.0"})';
    const r = await sb.run(["--version"], {}, { nodeArgs: ["--import", stub] });
    assert.equal(r.code, 2, r.out);
    assert.match(r.stderr, /backyard-cloudflare needs Node\.js 22 or later \(found v20\.11\.0\); https:\/\/nodejs\.org\//);
  });

  it("exits 130 on Ctrl-C at a prompt, releasing the lock", { skip: !ttyAvailable() && "needs util-linux script" }, async () => {
    const sb = await newSandbox({ cf: { accounts: { acc1: { name: "Acme", subdomain: null } } } });
    const r = await sb.runTTY(["setup"], {}, { answers: [[/workers\.dev subdomain for this account \[acme\]/, "\u0003"]] });
    assert.equal(r.code, 130, r.out);
    assert.match(r.out, /interrupted/);
    assert.ok(!existsSync(join(sb.root, "config", "backyard", "cloudflare", "backyard.lock")));
    assert.ok(!sb.allCalls().some((c) => c.args[0] === "deploy"));
  });

  it("reports an incomplete installation (exit 1)", async () => {
    const sb = await newSandbox();
    rmSync(join(sb.pkg, "src"), { recursive: true });
    const r = await sb.run(["setup", "--dry-run"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.stderr, /incomplete installation: reinstall backyard-cloudflare, or run from a complete checkout/);
    assert.equal((await sb.run(["--version"])).code, 0, "--version still works");
  });
});

describe("hints and wrangler", () => {
  it("names the right command form", () => {
    assert.equal(detectInvokedAs({ kind: "source" }, { BACKYARD_CF_INVOKED_AS: "backyard cloudflare" }), "backyard cloudflare");
    assert.equal(detectInvokedAs({ kind: "source" }, {}), "npm run cf --");
    assert.equal(detectInvokedAs({ kind: "packaged" }, { npm_command: "exec" }), "npx backyard-cloudflare");
    assert.equal(detectInvokedAs({ kind: "packaged" }, { npm_command: "run-script" }), "backyard-cloudflare");
    assert.equal(detectInvokedAs(null, {}), "backyard-cloudflare");
  });

  it("resolves wrangler through wrangler/package.json (its exports map blocks bin/wrangler.js)", () => {
    const require = createRequire(join(pkgDir, "scripts", "lib", "wrangler.mjs"));
    assert.throws(() => require.resolve("wrangler/bin/wrangler.js"), (e) => e.code === "ERR_PACKAGE_PATH_NOT_EXPORTED");
    const bin = wranglerBin();
    assert.equal(bin, join(pkgDir, "node_modules", "wrangler", "bin", "wrangler.js"));
    assert.equal(wranglerVersion(), JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8")).devDependencies.wrangler);
  });

  it("reads wrangler's credential without echoing it", () => {
    assert.deepEqual(parseAuthToken('{\n  "type": "oauth",\n  "token": "t0k"\n}\n'), { type: "oauth", token: "t0k" });
    assert.deepEqual(parseAuthToken('notice {with braces}\n{"type":"api_token","token":"abc"}\n'), { type: "api_token", token: "abc" });
    assert.deepEqual(parseAuthToken('{"type":"api_key","key":"k","email":"e@x"}'), { type: "api_key", key: "k", email: "e@x" });
    for (const bad of ["", "Not logged in", '{"type":"oauth"}', '{"type":"weird","token":"x"}']) {
      assert.throws(() => parseAuthToken(bad), (e) => !e.message.includes("x\"") && /unexpected output/.test(e.message), bad);
    }
    // The admin token never reaches a wrangler child; metrics are off unless chosen.
    const env = wranglerEnv({ BACKYARD_CF_API_TOKEN: "a", PATH: "/bin" });
    assert.equal(env.BACKYARD_CF_API_TOKEN, undefined);
    assert.equal(env.WRANGLER_SEND_METRICS, "false");
    assert.equal(wranglerEnv({ WRANGLER_SEND_METRICS: "true" }).WRANGLER_SEND_METRICS, "true");
    assert.deepEqual(parseWhoami({ code: 0, stdout: 'x\n{"loggedIn":true,"email":"me@x","accounts":[{"id":"a","name":"A"},{"bad":1}]}' }), { email: "me@x", accounts: [{ id: "a", name: "A" }] });
    assert.equal(parseWhoami({ code: 1, stdout: '{"loggedIn":false}' }), null);
    assert.equal(findURL("Deployed\n  https://other.me.workers.dev\n  https://backyard.me.workers.dev\n", "backyard"), "https://backyard.me.workers.dev");
    assert.equal(findURL("  backyard.example.com (custom domain)\n", "backyard"), "https://backyard.example.com");
    assert.equal(findURL("nothing", "backyard"), "");
  });
});
