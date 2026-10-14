#!/usr/bin/env node
// Builds the npm package backyard-cloudflare: a prebuilt Worker (the output of
// `wrangler deploy --dry-run`), the engine (bin/, scripts/{cli,setup,domain,
// access,status}.mjs, scripts/lib/) and one runtime dependency, wrangler,
// pinned exactly, with an npm-shrinkwrap.json derived from package-lock.json
// (the tree CI tests). Users run it with `npx backyard-cloudflare setup`, and
// the Go CLI installs the same .tgz (its SHA-256 is embedded in a release).
//
//   node scripts/pack.mjs [--version <semver>] [--out <dir>]
//   npm run package -- --version 1.2.3
//   make cloudflare-pack CF_PACK_VERSION=1.2.3
//
//   --version  the package version, e.g. 1.2.3 or 1.2.3-rc.1 (default: package.json's)
//   --out      where the .tgz goes (default: <repo>/dist-npm; relative to the current directory)
//
// Output: <out>/backyard-cloudflare-<version>.tgz and its digest
// <out>/backyard-cloudflare-<version>.tgz.sha256 ("<hex>  <name>\n"). It prints
// "sha256 <hex>" and, when GITHUB_OUTPUT is set, appends package=<abs path> and
// sha256=<hex> there.
//
// Steps:
//   1. sync-assets --strict and wrangler's build step both run with
//      BACKYARD_VERSION=<version>, so the bundled Worker reports that version
//      (checked in the bundle).
//   2. wrangler deploy --dry-run --outdir bundles the Worker → dist/worker.js
//      (without the source map and its reference); dist/wrangler.base.json is wrangler.jsonc without $schema, build, env,
//      main and routes; dist/build.json = {version, commit, assetsHash,
//      wrangler, workerSha256}.
//   3. The package is staged in a private temp directory; npm-shrinkwrap.json is
//      package-lock.json reduced to the production tree (`npm install
//      --package-lock-only --offline`); it must pin the same wrangler and have
//      no dev entries.
//   4. npm pack; the file list must be exactly the package's files (no src/,
//      test/, node_modules/, source maps, pack.mjs or sync-assets.mjs) and the
//      .tgz under 2 MB.
//
// Works offline after `npm ci`. Nothing is written to the tree except what
// sync-assets always writes (src/generated/, test/generated/; gitignored), what
// wrangler keeps in .wrangler/ (gitignored), and <out>. Exit codes: 0 done,
// 1 failed, 2 usage.

import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { appendFileSync, chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, renameSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
/** deploy/cloudflare (the package being packed). */
export const PKG_DIR = resolve(here, "..");
/** The repository root (LICENSE, the default --out); BACKYARD_REPO_ROOT overrides it, as for sync-assets. */
const REPO_ROOT = resolve(process.env.BACKYARD_REPO_ROOT || join(PKG_DIR, "..", ".."));

export const PACKAGE_NAME = "backyard-cloudflare";
/** A release version: 1.2.3, or 1.2.3-<pre-release> (the same rule as scripts/sync-assets.mjs). */
export const VERSION_RE = /^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/;
/** The engine's command modules (scripts/lib/*.mjs and scripts/lib/commands.json come too). */
export const ENGINE_SCRIPTS = ["cli.mjs", "setup.mjs", "domain.mjs", "access.mjs", "status.mjs"];
/** Keys of wrangler.jsonc that dist/wrangler.base.json leaves out (the engine adds name, main, vars, …). */
export const BASE_CONFIG_DROP = ["$schema", "build", "env", "main", "routes", "route"];
/** Never in the package. */
export const DENY = [/^src\//, /^test\//, /(^|\/)node_modules\//, /^scripts\/pack\.mjs$/, /^scripts\/sync-assets\.mjs$/, /(^|\/)\.npmrc$/, /(^|\/)\.dev\.vars/, /^wrangler\.jsonc$/, /\.map$/];
export const MAX_TGZ_BYTES = 2 * 1024 * 1024;

const USAGE = `usage: node scripts/pack.mjs [--version <semver>] [--out <dir>]

Builds ${PACKAGE_NAME}-<version>.tgz (prebuilt Worker + engine + pinned wrangler)
and its .sha256 into --out (default: <repo>/dist-npm). Run npm ci first.`;

/** A failure meant for the person running the build; code is the exit code. */
export class PackError extends Error {
  constructor(code, message) {
    super(message);
    this.name = "PackError";
    this.code = code;
  }
}

/**
 * Parses JSONC (// and /* *\/ comments, trailing commas), leaving string
 * contents alone.
 * @param {string} text
 */
export function parseJSONC(text) {
  let noComments = "";
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === '"') {
      let j = i + 1;
      while (j < text.length && text[j] !== '"') j += text[j] === "\\" ? 2 : 1;
      noComments += text.slice(i, j + 1);
      i = j;
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      noComments += "\n";
    } else if (c === "/" && text[i + 1] === "*") {
      const end = text.indexOf("*/", i + 2);
      i = end < 0 ? text.length : end + 1;
      noComments += " ";
    } else noComments += c;
  }
  let out = "";
  for (let i = 0; i < noComments.length; i++) {
    const c = noComments[i];
    if (c === '"') {
      let j = i + 1;
      while (j < noComments.length && noComments[j] !== '"') j += noComments[j] === "\\" ? 2 : 1;
      out += noComments.slice(i, j + 1);
      i = j;
      continue;
    }
    if (c === ",") {
      let j = i + 1;
      while (j < noComments.length && /\s/.test(noComments[j])) j++;
      if (noComments[j] === "}" || noComments[j] === "]") continue;
    }
    out += c;
  }
  return JSON.parse(out);
}

/**
 * The package.json of the published package (exact keys).
 * @param {string} version
 * @param {string} wrangler  the exact wrangler version
 */
export function packageJSON(version, wrangler) {
  return {
    name: PACKAGE_NAME,
    version,
    description: "Backyard on your own Cloudflare account (Workers free plan): setup, custom domain and Cloudflare Access without Terraform.",
    license: "Apache-2.0",
    repository: { type: "git", url: "git+https://github.com/ThaSami/backyard.git", directory: "deploy/cloudflare" },
    homepage: "https://github.com/thasami/backyard/tree/main/deploy/cloudflare#readme",
    type: "module",
    bin: { [PACKAGE_NAME]: "bin/backyard-cloudflare.mjs" },
    files: ["bin/", "scripts/", "dist/", "npm-shrinkwrap.json"],
    engines: { node: ">=22" },
    dependencies: { wrangler },
  };
}

/**
 * dist/wrangler.base.json: wrangler.jsonc without the keys the packaged engine
 * sets itself (or never deploys: routes).
 * @param {Record<string, any>} config
 */
export function baseConfig(config) {
  const out = { ...config };
  for (const k of BASE_CONFIG_DROP) delete out[k];
  return out;
}

/** The engine's library files (scripts/lib/*.mjs and commands.json), sorted. */
export function libFiles(pkgDir = PKG_DIR) {
  return readdirSync(join(pkgDir, "scripts", "lib"))
    .filter((f) => f.endsWith(".mjs") || f === "commands.json")
    .sort();
}

/** Every file of the package, as npm pack lists them (sorted). */
export function expectedFiles(pkgDir = PKG_DIR) {
  return [
    "LICENSE",
    "README.md",
    "bin/backyard-cloudflare.mjs",
    "dist/build.json",
    "dist/worker.js",
    "dist/wrangler.base.json",
    "npm-shrinkwrap.json",
    "package.json",
    ...ENGINE_SCRIPTS.map((f) => `scripts/${f}`),
    ...libFiles(pkgDir).map((f) => `scripts/lib/${f}`),
  ].sort();
}

/** Parses the arguments; throws PackError(2) for a usage error. */
export function parseArgs(argv) {
  const o = { help: false, version: undefined, out: undefined };
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i];
    if (a === "-h" || a === "--help") {
      o.help = true;
      continue;
    }
    const m = /^--(version|out)(?:=(.*))?$/.exec(a);
    if (!m) throw new PackError(2, `unknown argument ${JSON.stringify(a)}\n\n${USAGE}`);
    const value = m[2] !== undefined ? m[2] : argv[++i];
    if (value === undefined || value === "" || (m[2] === undefined && value.startsWith("--"))) throw new PackError(2, `--${m[1]} needs a value\n\n${USAGE}`);
    o[m[1]] = value;
  }
  return o;
}

const sha256 = (buf) => createHash("sha256").update(buf).digest("hex");
const readJSON = (p) => JSON.parse(readFileSync(p, "utf8"));

/** The environment of the child processes: no npm lifecycle leftovers that could point npm elsewhere. */
function childEnv(env, extra = {}) {
  const out = { ...env, ...extra };
  for (const k of Object.keys(out)) if (/^npm_(config_local_prefix|package_|lifecycle_)/i.test(k) || k === "npm_command") delete out[k];
  return out;
}

/** Runs a command; stdout is captured with `capture`, everything else is shown. */
function run(cmd, args, o) {
  const r = spawnSync(cmd, args, {
    cwd: o.cwd,
    env: o.env,
    stdio: ["ignore", o.capture ? "pipe" : "inherit", "inherit"],
    encoding: "utf8",
    shell: o.shell ?? false,
    maxBuffer: 64 * 1024 * 1024,
  });
  if (r.error) return { status: 1, stdout: "", error: r.error.message };
  return { status: r.status ?? 1, stdout: r.stdout ?? "", error: "" };
}

/** How to run npm: the npm that runs this script (npm run package), else npm on PATH. */
function npm(env) {
  const execPath = env.npm_execpath ?? "";
  if (/(^|[\\/])npm-cli\.js$/.test(execPath)) return { cmd: process.execPath, pre: [execPath], shell: false };
  return { cmd: process.platform === "win32" ? "npm.cmd" : "npm", pre: [], shell: process.platform === "win32" };
}

/** The installed wrangler: {bin, version}. */
function installedWrangler(pkgDir) {
  const require = createRequire(join(pkgDir, "package.json"));
  let pkgPath;
  try {
    pkgPath = require.resolve("wrangler/package.json");
  } catch {
    throw new PackError(1, "wrangler is not installed: run npm ci first");
  }
  const pkg = readJSON(pkgPath);
  const bin = typeof pkg.bin === "string" ? pkg.bin : pkg.bin?.wrangler;
  if (!bin) throw new PackError(1, `${pkgPath} names no wrangler bin`);
  return { bin: join(dirname(pkgPath), bin), version: String(pkg.version ?? "") };
}

/** The commit the package is built from: GITHUB_SHA, else git rev-parse HEAD, else "unknown". */
function commitOf(env, cwd) {
  if (env.GITHUB_SHA) return env.GITHUB_SHA;
  const r = spawnSync("git", ["rev-parse", "HEAD"], { cwd, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
  const sha = r.status === 0 ? String(r.stdout).trim() : "";
  return /^[0-9a-f]{7,64}$/.test(sha) ? sha : "unknown";
}

/**
 * Checks the derived shrinkwrap: wrangler pinned to `pin`, no dev entries,
 * every entry in the lockfile with the same integrity. Returns the problems.
 * @param {Record<string, any>} shrinkwrap
 * @param {Record<string, any>} lock   package-lock.json
 * @param {string} pin
 */
export function shrinkwrapProblems(shrinkwrap, lock, pin) {
  const problems = [];
  const packages = shrinkwrap?.packages ?? {};
  const w = packages["node_modules/wrangler"];
  if (!w) problems.push("node_modules/wrangler is missing");
  else if (w.version !== pin) problems.push(`node_modules/wrangler is ${w.version}, package.json pins ${pin}`);
  for (const [k, v] of Object.entries(packages)) {
    if (k === "") continue;
    if (v?.dev) problems.push(`${k} is a dev dependency`);
    const src = lock?.packages?.[k];
    if (!src) problems.push(`${k} is not in package-lock.json`);
    else if (src.integrity !== v.integrity || src.version !== v.version) problems.push(`${k} differs from package-lock.json`);
  }
  return problems;
}

/**
 * Whether BACKYARD_VERSION reached wrangler's build step: the regenerated
 * version.ts and the bundle declare BUILD_VERSION = "<version>", and the
 * bundle holds no development version. (A bare "<version>" string proves
 * nothing: the embedded SDK carries its own version, "0.1.0" too.) Returns
 * the problem, "" when there is none.
 * @param {{worker: string | Buffer, versionTs: string, version: string, devVersion: string}} o
 */
export function bundleVersionProblem(o) {
  const decl = `BUILD_VERSION = ${JSON.stringify(o.version)}`;
  if (!o.versionTs.includes(decl)) return `src/generated/version.ts does not declare ${decl}`;
  if (!o.worker.includes(decl)) return `the bundle does not declare ${decl}`;
  if (o.version !== o.devVersion && o.worker.includes(JSON.stringify(o.devVersion))) return `the bundle still holds the development version ${o.devVersion}`;
  return "";
}

/**
 * Builds the package. Returns {path, sha256, size, files, build} (build: dist/build.json).
 * @param {{version?: string, out?: string, env?: NodeJS.ProcessEnv, log?: (...a: unknown[]) => void, pkgDir?: string}} [o]
 */
export function pack(o = {}) {
  const env = o.env ?? process.env;
  const log = o.log ?? console.log;
  const pkgDir = o.pkgDir ?? PKG_DIR;
  const pkg = readJSON(join(pkgDir, "package.json"));
  const version = o.version ?? String(pkg.version ?? "");
  if (!VERSION_RE.test(version)) throw new PackError(2, `--version must be a semantic version such as 1.2.3 or 1.2.3-rc.1 (got ${JSON.stringify(version)})`);
  const pin = String(pkg.devDependencies?.wrangler ?? "");
  if (!/^\d+\.\d+\.\d+$/.test(pin)) throw new PackError(1, `package.json must pin wrangler exactly in devDependencies (got ${JSON.stringify(pin)})`);
  const wrangler = installedWrangler(pkgDir);
  if (wrangler.version !== pin) throw new PackError(1, `node_modules has wrangler ${wrangler.version || "(unknown)"}, but package.json pins ${pin}: run npm ci first`);
  const outDir = resolve(o.out ?? join(REPO_ROOT, "dist-npm"));
  const tgzName = `${PACKAGE_NAME}-${version}.tgz`;
  const buildEnv = childEnv(env, { BACKYARD_VERSION: version, WRANGLER_SEND_METRICS: env.WRANGLER_SEND_METRICS ?? "false" });

  const tmp = mkdtempSync(join(tmpdir(), "backyard-cf-pack-"));
  try {
    // 1. The bundled assets, with the release version.
    log(`→ sync-assets --strict (BACKYARD_VERSION=${version})`);
    if (run(process.execPath, [join(pkgDir, "scripts", "sync-assets.mjs"), "--strict"], { cwd: pkgDir, env: buildEnv }).status !== 0) {
      throw new PackError(1, "sync-assets --strict failed (see above)");
    }

    // 2. The Worker bundle: wrangler's own build (it re-runs sync-assets, with the same env).
    const bundleDir = join(tmp, "bundle");
    log(`→ wrangler ${wrangler.version} deploy --dry-run (bundling the Worker; nothing is uploaded)`);
    const dry = run(process.execPath, [wrangler.bin, "deploy", "--dry-run", "--env=", "--config", join(pkgDir, "wrangler.jsonc"), "--outdir", bundleDir], {
      cwd: pkgDir,
      env: buildEnv,
    });
    if (dry.status !== 0) throw new PackError(1, `wrangler deploy --dry-run failed${dry.error ? `: ${dry.error}` : " (see above)"}`);
    const bundleFiles = existsSync(bundleDir) ? readdirSync(bundleDir) : [];
    if (!bundleFiles.includes("index.js")) throw new PackError(1, `wrangler wrote no index.js to ${bundleDir}`);
    const extra = bundleFiles.filter((f) => f !== "index.js" && f !== "index.js.map" && f !== "README.md");
    if (extra.length) throw new PackError(1, `the bundle has additional modules (${extra.join(", ")}); the package ships only dist/worker.js`);
    // The source map is not shipped, so its reference goes too.
    const worker = Buffer.from(readFileSync(join(bundleDir, "index.js"), "utf8").replace(/\n\/\/# sourceMappingURL=[^\n]*\s*$/, "\n"));
    const versionProblem = bundleVersionProblem({
      worker,
      versionTs: readFileSync(join(pkgDir, "src", "generated", "version.ts"), "utf8"),
      version,
      devVersion: `${String(pkg.version ?? "0.0.0")}-dev`,
    });
    if (versionProblem) {
      throw new PackError(1, `the bundled Worker does not report version ${version} (${versionProblem}): BACKYARD_VERSION did not reach wrangler's build step`);
    }
    const generatedIndex = readFileSync(join(pkgDir, "src", "generated", "index.ts"), "utf8");
    const assetsHash = /export const ASSETS_HASH = "([0-9a-f]+)";/.exec(generatedIndex)?.[1];
    if (!assetsHash) throw new PackError(1, "no ASSETS_HASH in src/generated/index.ts");

    // 3. The package.
    const stage = join(tmp, "package");
    for (const d of ["bin", join("scripts", "lib"), "dist"]) mkdirSync(join(stage, d), { recursive: true });
    copyFileSync(join(pkgDir, "bin", "backyard-cloudflare.mjs"), join(stage, "bin", "backyard-cloudflare.mjs"));
    chmodSync(join(stage, "bin", "backyard-cloudflare.mjs"), 0o755);
    for (const f of ENGINE_SCRIPTS) copyFileSync(join(pkgDir, "scripts", f), join(stage, "scripts", f));
    for (const f of libFiles(pkgDir)) copyFileSync(join(pkgDir, "scripts", "lib", f), join(stage, "scripts", "lib", f));
    writeFileSync(join(stage, "dist", "worker.js"), worker);
    const config = parseJSONC(readFileSync(join(pkgDir, "wrangler.jsonc"), "utf8"));
    writeFileSync(join(stage, "dist", "wrangler.base.json"), JSON.stringify(baseConfig(config), null, 2) + "\n");
    const build = { version, commit: commitOf(env, pkgDir), assetsHash, wrangler: wrangler.version, workerSha256: sha256(worker) };
    writeFileSync(join(stage, "dist", "build.json"), JSON.stringify(build, null, 2) + "\n");
    copyFileSync(join(pkgDir, "npm-readme.md"), join(stage, "README.md"));
    copyFileSync(join(REPO_ROOT, "LICENSE"), join(stage, "LICENSE"));
    writeFileSync(join(stage, "package.json"), JSON.stringify(packageJSON(version, pin), null, 2) + "\n");

    // The shrinkwrap: the tested lockfile reduced to the production tree.
    log("→ npm-shrinkwrap.json (package-lock.json, production dependencies only)");
    const lockPath = join(pkgDir, "package-lock.json");
    if (!existsSync(lockPath)) throw new PackError(1, "package-lock.json is missing: run npm ci first");
    copyFileSync(lockPath, join(stage, "package-lock.json"));
    const n = npm(env);
    const npmEnv = childEnv(env);
    // --legacy-peer-deps=false: the tree must not depend on how pack.mjs runs (`npm run
    // package` exports the checkout's .npmrc legacy-peer-deps=true, which drops optional peers).
    const lockOnly = run(n.cmd, [...n.pre, "install", "--package-lock-only", "--legacy-peer-deps=false", "--ignore-scripts", "--no-audit", "--no-fund", "--offline"], {
      cwd: stage,
      env: npmEnv,
      shell: n.shell,
    });
    if (lockOnly.status !== 0 || !existsSync(join(stage, "package-lock.json"))) {
      throw new PackError(1, `could not derive npm-shrinkwrap.json from package-lock.json offline${lockOnly.error ? ` (${lockOnly.error})` : ""}: run npm ci first`);
    }
    renameSync(join(stage, "package-lock.json"), join(stage, "npm-shrinkwrap.json"));
    if (existsSync(join(stage, "node_modules"))) rmSync(join(stage, "node_modules"), { recursive: true, force: true });
    const problems = shrinkwrapProblems(readJSON(join(stage, "npm-shrinkwrap.json")), readJSON(lockPath), pin);
    if (problems.length) throw new PackError(1, `npm-shrinkwrap.json does not match package-lock.json: ${problems.slice(0, 5).join("; ")}: run npm ci first`);

    // 4. npm pack, then the checks.
    log(`→ npm pack (${tgzName})`);
    mkdirSync(outDir, { recursive: true });
    const tgzPath = join(outDir, tgzName);
    const discard = () => {
      rmSync(tgzPath, { force: true });
      rmSync(`${tgzPath}.sha256`, { force: true });
    };
    discard(); // a .tgz or digest of an earlier build never survives a failed one
    const packed = run(n.cmd, [...n.pre, "pack", stage, "--pack-destination", outDir, "--json"], { cwd: tmp, env: npmEnv, shell: n.shell, capture: true });
    if (packed.status !== 0) throw new PackError(1, `npm pack failed${packed.error ? `: ${packed.error}` : " (see above)"}`);
    let info;
    try {
      const out = packed.stdout;
      info = JSON.parse(out.slice(out.indexOf("["), out.lastIndexOf("]") + 1))[0];
    } catch {
      throw new PackError(1, "unexpected output of npm pack --json");
    }
    if (info?.filename !== tgzName) {
      discard();
      throw new PackError(1, `npm pack wrote ${info?.filename}, expected ${tgzName}`);
    }
    const files = (info.files ?? []).map((f) => String(f.path)).sort();
    const want = expectedFiles(pkgDir);
    const wrong = [
      ["not allowed", files.filter((f) => DENY.some((re) => re.test(f)))],
      ["missing", want.filter((f) => !files.includes(f))],
      ["unexpected", files.filter((f) => !want.includes(f))],
    ].filter(([, list]) => list.length);
    if (wrong.length) {
      discard();
      throw new PackError(1, `the package has the wrong files (nothing was kept): ${wrong.map(([what, list]) => `${what}: ${list.join(", ")}`).join("; ")}`);
    }
    const tgz = readFileSync(tgzPath);
    if (tgz.length >= MAX_TGZ_BYTES) {
      discard();
      throw new PackError(1, `${tgzName} is ${tgz.length} bytes; the limit is ${MAX_TGZ_BYTES} (2 MB)`);
    }

    const digest = sha256(tgz);
    writeFileSync(`${tgzPath}.sha256`, `${digest}  ${tgzName}\n`);
    log(`\nBuilt ${tgzPath} (${(tgz.length / 1024).toFixed(0)} KiB, ${files.length} files, Worker ${(worker.length / 1024).toFixed(0)} KiB, wrangler ${wrangler.version})`);
    log(`sha256 ${digest}`);
    if (env.GITHUB_OUTPUT) appendFileSync(env.GITHUB_OUTPUT, `package=${tgzPath}\nsha256=${digest}\n`);
    return { path: tgzPath, sha256: digest, size: tgz.length, files, build };
  } finally {
    rmSync(tmp, { recursive: true, force: true });
  }
}

/** Runs pack.mjs with argv; returns the exit code. */
export function main(argv = process.argv.slice(2)) {
  try {
    const o = parseArgs(argv);
    if (o.help) {
      console.log(USAGE);
      return 0;
    }
    pack({ version: o.version, out: o.out === undefined ? undefined : resolve(process.cwd(), o.out) });
    return 0;
  } catch (e) {
    if (e instanceof PackError) {
      console.error(`pack: ${e.message}`);
      return e.code;
    }
    throw e;
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  process.exitCode = main();
}
