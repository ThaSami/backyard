// Tests of scripts/pack.mjs (node --test): it builds the real npm package from
// a copy of deploy/cloudflare with the dev install's wrangler (offline: no
// login, no upload), and the test checks the .tgz against the package layout
// (file list, package.json, shrinkwrap, dist/) and runs the packaged engine
// with the real wrangler, the way `npx backyard-cloudflare` would.
//
// The copy keeps the real tree untouched (sync-assets writes src/generated of
// the copy, with BACKYARD_VERSION=1.2.3-test), while BACKYARD_REPO_ROOT points
// sync-assets and the LICENSE at the real repository.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { after, before, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

import { PackError, baseConfig, bundleVersionProblem, expectedFiles, packageJSON, parseArgs, parseJSONC, shrinkwrapProblems } from "../../scripts/pack.mjs";

const PKG_DIR = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const REPO_ROOT = resolve(PKG_DIR, "..", "..");
const VERSION = "1.2.3-test";
const TGZ = `backyard-cloudflare-${VERSION}.tgz`;
const SHA = "0123456789abcdef0123456789abcdef01234567";
const pkgJSON = JSON.parse(readFileSync(join(PKG_DIR, "package.json"), "utf8"));
const lock = JSON.parse(readFileSync(join(PKG_DIR, "package-lock.json"), "utf8"));
const sha256 = (buf) => createHash("sha256").update(buf).digest("hex");
const hasWrangler = existsSync(join(PKG_DIR, "node_modules", "wrangler", "package.json"));
const hasTar = spawnSync("tar", ["--version"], { stdio: "ignore" }).status === 0;
const skip = !hasWrangler ? "wrangler is not installed (npm ci)" : !hasTar ? "tar is not available" : false;

/** Files under dir (relative, "/"-separated, sorted), skipping `skipRe`. */
function listFiles(dir, skipRe) {
  const out = [];
  const visit = (d) => {
    for (const name of readdirSync(d).sort()) {
      const p = join(d, name);
      const rel = relative(dir, p).split("\\").join("/");
      if (skipRe?.test(rel)) continue;
      const st = statSync(p);
      if (st.isDirectory()) visit(p);
      else out.push(rel);
    }
  };
  visit(dir);
  return out.sort();
}

/** A listing with sizes and mtimes, to prove nothing else was written. */
function snapshot(dir, skipRe) {
  return listFiles(dir, skipRe).map((f) => {
    const st = statSync(join(dir, f));
    return `${f} ${st.size} ${st.mtimeMs}`;
  });
}

/** The files the package must hold, written out independently of pack.mjs. */
function allowList() {
  const lib = readdirSync(join(PKG_DIR, "scripts", "lib")).filter((f) => f.endsWith(".mjs") || f === "commands.json");
  return [
    "LICENSE",
    "README.md",
    "bin/backyard-cloudflare.mjs",
    "dist/build.json",
    "dist/worker.js",
    "dist/wrangler.base.json",
    "npm-shrinkwrap.json",
    "package.json",
    "scripts/access.mjs",
    "scripts/cli.mjs",
    "scripts/domain.mjs",
    "scripts/setup.mjs",
    "scripts/status.mjs",
    ...lib.map((f) => `scripts/lib/${f}`),
  ].sort();
}

// What the copy's own build leaves behind (gitignored in the real tree).
const GENERATED = /^(node_modules|\.wrangler|src\/generated|test\/generated)(\/|$)/;

describe("scripts/pack.mjs", { skip }, () => {
  const tmp = mkdtempSync(join(tmpdir(), "backyard-pack-test-"));
  const pkg = join(tmp, "pkg");
  const out = join(tmp, "out");
  const ghOutput = join(tmp, "github-output");
  const extracted = join(tmp, "extracted");
  /** @type {{status: number|null, stdout: string, stderr: string}} */
  let build;
  let before0 = [];

  before(() => {
    cpSync(PKG_DIR, pkg, {
      recursive: true,
      filter: (src) => {
        const rel = relative(PKG_DIR, src).split("\\").join("/");
        return !GENERATED.test(rel) && !/^wrangler\.setup\.jsonc$|(^|\/)\.dev\.vars/.test(rel);
      },
    });
    symlinkSync(join(PKG_DIR, "node_modules"), join(pkg, "node_modules"), "dir");
    before0 = snapshot(pkg, GENERATED);
    // npm_config_legacy_peer_deps: what `npm run package` exports from the checkout's .npmrc;
    // the package must be the same as from `node scripts/pack.mjs` (make cloudflare-pack).
    const env = {
      ...process.env,
      BACKYARD_REPO_ROOT: REPO_ROOT,
      GITHUB_SHA: SHA,
      GITHUB_OUTPUT: ghOutput,
      XDG_CONFIG_HOME: join(tmp, "xdg"),
      NO_COLOR: "1",
      npm_config_legacy_peer_deps: "true",
    };
    delete env.BACKYARD_VERSION;
    const r = spawnSync(process.execPath, [join(pkg, "scripts", "pack.mjs"), "--version", VERSION, "--out", "out"], { cwd: tmp, env, encoding: "utf8", timeout: 240_000 });
    build = { status: r.status, stdout: r.stdout ?? "", stderr: r.stderr ?? "" };
    if (r.status === 0) {
      mkdirSync(extracted, { recursive: true });
      const x = spawnSync("tar", ["-xzf", join(out, TGZ), "-C", extracted], { encoding: "utf8" });
      assert.equal(x.status, 0, x.stderr);
    }
  });

  after(() => {
    rmSync(tmp, { recursive: true, force: true });
  });

  const packaged = (...p) => join(extracted, "package", ...p);
  const readPackaged = (...p) => readFileSync(packaged(...p), "utf8");

  it("builds the .tgz, its .sha256 and the GitHub outputs", () => {
    assert.equal(build.status, 0, `${build.stdout}\n${build.stderr}`);
    const tgz = readFileSync(join(out, TGZ));
    assert.ok(tgz.length < 2 * 1024 * 1024, `${tgz.length} bytes`);
    const digest = sha256(tgz);
    assert.equal(readFileSync(join(out, `${TGZ}.sha256`), "utf8"), `${digest}  ${TGZ}\n`);
    assert.match(build.stdout, new RegExp(`^sha256 ${digest}$`, "m"));
    assert.equal(readFileSync(ghOutput, "utf8"), `package=${join(out, TGZ)}\nsha256=${digest}\n`);
    assert.deepEqual(readdirSync(out).sort(), [TGZ, `${TGZ}.sha256`]);
  });

  it("holds exactly the package's files (no sources, tests, source maps or build scripts)", () => {
    assert.equal(build.status, 0);
    const files = listFiles(join(extracted, "package"));
    assert.deepEqual(files, allowList());
    assert.deepEqual(expectedFiles(PKG_DIR), allowList(), "pack.mjs checks the same list");
    for (const f of files) {
      assert.ok(!/^(src|test|node_modules)\//.test(f) && !/\.map$/.test(f) && f !== "wrangler.jsonc", f);
    }
    assert.equal(readPackaged("README.md"), readFileSync(join(PKG_DIR, "npm-readme.md"), "utf8"));
    assert.equal(readPackaged("LICENSE"), readFileSync(join(REPO_ROOT, "LICENSE"), "utf8"));
    for (const f of ["bin/backyard-cloudflare.mjs", "scripts/cli.mjs", "scripts/lib/commands.json", "scripts/lib/wrangler.mjs"]) {
      assert.equal(readPackaged(f), readFileSync(join(PKG_DIR, f), "utf8"), f);
    }
    assert.ok((statSync(packaged("bin", "backyard-cloudflare.mjs")).mode & 0o111) !== 0, "the bin is executable");
  });

  it("has the pinned package.json", () => {
    assert.equal(build.status, 0);
    const got = JSON.parse(readPackaged("package.json"));
    const want = {
      name: "backyard-cloudflare",
      version: VERSION,
      description: "Backyard on your own Cloudflare account (Workers free plan): setup, custom domain and Cloudflare Access without Terraform.",
      license: "Apache-2.0",
      repository: { type: "git", url: "git+https://github.com/ThaSami/backyard.git", directory: "deploy/cloudflare" },
      homepage: "https://github.com/thasami/backyard/tree/main/deploy/cloudflare#readme",
      type: "module",
      bin: { "backyard-cloudflare": "bin/backyard-cloudflare.mjs" },
      files: ["bin/", "scripts/", "dist/", "npm-shrinkwrap.json"],
      engines: { node: ">=22" },
      dependencies: { wrangler: pkgJSON.devDependencies.wrangler },
    };
    assert.deepEqual(got, want);
    assert.deepEqual(Object.keys(got), Object.keys(want), "key order");
    assert.match(got.dependencies.wrangler, /^\d+\.\d+\.\d+$/, "wrangler is pinned exactly");
    assert.deepEqual(packageJSON(VERSION, pkgJSON.devDependencies.wrangler), want);
  });

  it("ships the tested lockfile's production tree as npm-shrinkwrap.json", () => {
    assert.equal(build.status, 0);
    const sw = JSON.parse(readPackaged("npm-shrinkwrap.json"));
    const w = sw.packages["node_modules/wrangler"];
    assert.equal(w.version, pkgJSON.devDependencies.wrangler);
    assert.equal(w.version, lock.packages["node_modules/wrangler"].version);
    assert.equal(w.integrity, lock.packages["node_modules/wrangler"].integrity);
    assert.deepEqual(sw.packages[""].dependencies, { wrangler: pkgJSON.devDependencies.wrangler });
    assert.equal(sw.packages[""].devDependencies, undefined);
    const entries = Object.entries(sw.packages).filter(([k]) => k !== "");
    assert.ok(entries.length > 10, `${entries.length} entries`);
    for (const [k, v] of entries) {
      assert.notEqual(v.dev, true, `${k} is a dev entry`);
      assert.ok(lock.packages[k], `${k} is in package-lock.json`);
      assert.equal(v.integrity, lock.packages[k].integrity, `${k} integrity`);
      assert.equal(v.version, lock.packages[k].version, `${k} version`);
    }
    for (const dev of ["node_modules/vitest", "node_modules/typescript", "node_modules/@cloudflare/vitest-plugin"]) {
      assert.equal(sw.packages[dev], undefined, `${dev} is not shipped`);
    }
    // wrangler's optional peers are in the tree although npm_config_legacy_peer_deps=true was set.
    const peers = Object.entries(JSON.parse(readFileSync(join(PKG_DIR, "node_modules", "wrangler", "package.json"), "utf8")).peerDependenciesMeta ?? {});
    for (const [name, meta] of peers) {
      if (meta?.optional && lock.packages[`node_modules/${name}`]) assert.ok(sw.packages[`node_modules/${name}`], `the optional peer ${name} is in the shrinkwrap`);
    }
  });

  it("ships the prebuilt Worker with its build record", () => {
    assert.equal(build.status, 0);
    const worker = readFileSync(packaged("dist", "worker.js"));
    assert.ok(worker.includes(JSON.stringify(VERSION)), "BACKYARD_VERSION reached wrangler's build step");
    assert.ok(!worker.includes("0.1.0-dev"), "no development version in the bundle");
    assert.ok(!worker.includes("sourceMappingURL"), "no reference to the source map that is not shipped");
    assert.match(worker.toString("utf8"), /export \{[^}]*\bRegistryDO\b[^}]*\bSiteDO\b[^}]*\}/, "the Durable Object classes are exported");
    const b = JSON.parse(readPackaged("dist", "build.json"));
    assert.deepEqual(Object.keys(b), ["version", "commit", "assetsHash", "wrangler", "workerSha256"]);
    assert.equal(b.version, VERSION);
    assert.equal(b.commit, SHA);
    const hash = /export const ASSETS_HASH = "([0-9a-f]+)";/.exec(readFileSync(join(pkg, "src", "generated", "index.ts"), "utf8"))?.[1];
    assert.ok(hash);
    assert.equal(b.assetsHash, hash);
    assert.equal(b.wrangler, JSON.parse(readFileSync(join(PKG_DIR, "node_modules", "wrangler", "package.json"), "utf8")).version);
    assert.equal(b.workerSha256, sha256(worker));
    assert.equal(readFileSync(join(pkg, "src", "generated", "version.ts"), "utf8").includes(`BUILD_VERSION = "${VERSION}"`), true);
  });

  it("ships wrangler.jsonc as the base config, without the parts the engine sets", () => {
    assert.equal(build.status, 0);
    const got = JSON.parse(readPackaged("dist", "wrangler.base.json"));
    const src = parseJSONC(readFileSync(join(PKG_DIR, "wrangler.jsonc"), "utf8"));
    const want = { ...src };
    for (const k of ["$schema", "build", "env", "main", "routes", "route"]) delete want[k];
    assert.deepEqual(got, want);
    for (const k of ["$schema", "build", "env", "main", "routes"]) assert.equal(k in got, false, k);
    assert.equal(got.name, "backyard");
    assert.deepEqual(got.durable_objects.bindings.map((x) => x.name), ["SITE", "REGISTRY"]);
    assert.equal(got.ai.binding, "AI");
    assert.equal(got.vars.AUTH_MODE, "none");
  });

  it("writes nothing to the package directory but what sync-assets and wrangler always write", () => {
    assert.equal(build.status, 0);
    assert.deepEqual(snapshot(pkg, GENERATED), before0);
    assert.ok(!existsSync(join(pkg, "dist-npm")));
  });

  it("runs as an installed package with the real wrangler (--version, setup --dry-run)", () => {
    assert.equal(build.status, 0);
    // The dependency as npm would install it (its own dependencies resolve from the dev install).
    mkdirSync(packaged("node_modules"), { recursive: true });
    symlinkSync(join(PKG_DIR, "node_modules", "wrangler"), packaged("node_modules", "wrangler"), "dir");
    const listing = snapshot(join(extracted, "package"), /^node_modules(\/|$)/);
    const cwd = join(tmp, "user-cwd");
    mkdirSync(cwd, { recursive: true });
    const env = { ...process.env, XDG_CONFIG_HOME: join(tmp, "cfg"), NO_COLOR: "1", CI: "true", WRANGLER_SEND_METRICS: "false" };
    for (const k of ["BACKYARD_CF_INVOKED_AS", "BACKYARD_CF_RESULT_FILE", "CLOUDFLARE_API_TOKEN", "CLOUDFLARE_ACCOUNT_ID", "DEPLOY_TOKEN", "BACKYARD_VERSION"]) delete env[k];
    const bin = packaged("bin", "backyard-cloudflare.mjs");

    const v = spawnSync(process.execPath, [bin, "--version"], { cwd, env, encoding: "utf8" });
    assert.equal(v.status, 0, v.stderr);
    assert.equal(v.stdout.trim(), `backyard-cloudflare ${VERSION} (wrangler ${pkgJSON.devDependencies.wrangler})`);

    const r = spawnSync(process.execPath, [bin, "setup", "--dry-run", "--non-interactive"], { cwd, env, encoding: "utf8", timeout: 180_000 });
    const text = `${r.stdout}\n${r.stderr}`;
    assert.equal(r.status, 0, text);
    for (const binding of ["env.SITE (SiteDO)", "env.REGISTRY (RegistryDO)", "env.AI"]) assert.ok(text.includes(binding), `${binding} in\n${text}`);
    assert.match(text, /layout: packaged/);
    assert.match(text, /AUTH_MODE=none/);
    assert.deepEqual(snapshot(join(extracted, "package"), /^node_modules(\/|$)/), listing, "nothing is written next to the package");
    assert.ok(!existsSync(join(tmp, "cfg", "backyard")), "a dry run saves no state");
    assert.deepEqual(readdirSync(cwd), [], "nothing is written to the user's directory");
  });
});

describe("scripts/pack.mjs helpers", () => {
  it("parses its arguments", () => {
    assert.deepEqual(parseArgs([]), { help: false, version: undefined, out: undefined });
    assert.deepEqual(parseArgs(["--version", "1.2.3", "--out=x"]), { help: false, version: "1.2.3", out: "x" });
    assert.equal(parseArgs(["-h"]).help, true);
    for (const bad of [["--bogus"], ["--version"], ["--out", "--version"], ["--version="], ["extra"]]) {
      assert.throws(() => parseArgs(bad), (e) => e instanceof PackError && e.code === 2, JSON.stringify(bad));
    }
  });

  it("refuses an invalid version or unknown arguments before building anything", () => {
    const out = mkdtempSync(join(tmpdir(), "backyard-pack-usage-"));
    try {
      for (const [args, code, re] of [
        [["--version", "v1.2.3"], 2, /semantic version/],
        [["--version", "1.2"], 2, /semantic version/],
        [["--nope"], 2, /unknown argument/],
      ]) {
        const r = spawnSync(process.execPath, [join(PKG_DIR, "scripts", "pack.mjs"), ...args, "--out", out], { encoding: "utf8", env: { ...process.env, GITHUB_OUTPUT: "" } });
        assert.equal(r.status, code, `${args}: ${r.stderr}`);
        assert.match(r.stderr, re);
      }
      assert.deepEqual(readdirSync(out), []);
      const help = spawnSync(process.execPath, [join(PKG_DIR, "scripts", "pack.mjs"), "--help"], { encoding: "utf8" });
      assert.equal(help.status, 0);
      assert.match(help.stdout, /usage: node scripts\/pack\.mjs \[--version <semver>\] \[--out <dir>\]/);
    } finally {
      rmSync(out, { recursive: true, force: true });
    }
  });

  it("fails closed when the installed wrangler is not the pinned one", { skip: hasWrangler ? false : "wrangler is not installed (npm ci)" }, () => {
    const dir = mkdtempSync(join(tmpdir(), "backyard-pack-pin-"));
    try {
      mkdirSync(join(dir, "scripts"), { recursive: true });
      cpSync(join(PKG_DIR, "scripts", "pack.mjs"), join(dir, "scripts", "pack.mjs"));
      symlinkSync(join(PKG_DIR, "node_modules"), join(dir, "node_modules"), "dir");
      for (const [pin, re] of [
        ["4.0.0", /node_modules has wrangler \d+\.\d+\.\d+, but package\.json pins 4\.0\.0: run npm ci first/],
        ["^4.149.0", /must pin wrangler exactly/],
      ]) {
        const pkg = { ...pkgJSON, devDependencies: { ...pkgJSON.devDependencies, wrangler: pin } };
        writeFileSync(join(dir, "package.json"), JSON.stringify(pkg));
        const r = spawnSync(process.execPath, [join(dir, "scripts", "pack.mjs"), "--out", join(dir, "out")], { encoding: "utf8" });
        assert.equal(r.status, 1, r.stderr);
        assert.match(r.stderr, re);
        assert.ok(!existsSync(join(dir, "out")), "nothing is built");
        assert.ok(!existsSync(join(dir, "src")), "sync-assets did not run");
      }
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it("checks that BACKYARD_VERSION reached the bundle by its declaration, not a bare version string", () => {
    const sdk = 'const VERSION = "0.1.0";\n';
    const ok = { worker: `${sdk}var BUILD_VERSION = "0.1.0";\n`, versionTs: 'export const BUILD_VERSION = "0.1.0";\n', version: "0.1.0", devVersion: "0.1.0-dev" };
    assert.equal(bundleVersionProblem(ok), "");
    assert.equal(bundleVersionProblem({ ...ok, worker: Buffer.from(ok.worker) }), "");
    // The first release, 0.1.0, built without BACKYARD_VERSION: the SDK's "0.1.0" does not count.
    const dev = { ...ok, worker: `${sdk}var BUILD_VERSION = "0.1.0-dev";\n`, versionTs: 'export const BUILD_VERSION = "0.1.0-dev";\n' };
    assert.match(bundleVersionProblem(dev), /version\.ts does not declare BUILD_VERSION = "0\.1\.0"/);
    assert.match(bundleVersionProblem({ ...dev, versionTs: ok.versionTs }), /the bundle does not declare BUILD_VERSION = "0\.1\.0"/);
    assert.match(bundleVersionProblem({ ...ok, worker: `${ok.worker}const fallback = "0.1.0-dev";\n` }), /still holds the development version 0\.1\.0-dev/);
    assert.equal(bundleVersionProblem({ ...dev, version: "0.1.0-dev" }), "", "a development build may say so");
  });

  it("checks the shrinkwrap against the lockfile", () => {
    const lockfile = {
      packages: {
        "": { devDependencies: { vitest: "1" } },
        "node_modules/wrangler": { version: "4.149.0", integrity: "sha512-w" },
        "node_modules/esbuild": { version: "0.1.0", integrity: "sha512-e" },
        "node_modules/vitest": { version: "1.0.0", integrity: "sha512-v", dev: true },
      },
    };
    const good = { packages: { "": {}, "node_modules/wrangler": { version: "4.149.0", integrity: "sha512-w" }, "node_modules/esbuild": { version: "0.1.0", integrity: "sha512-e" } } };
    assert.deepEqual(shrinkwrapProblems(good, lockfile, "4.149.0"), []);
    const cases = [
      [{ packages: { "": {} } }, /node_modules\/wrangler is missing/],
      [{ packages: { "node_modules/wrangler": { version: "4.150.0", integrity: "sha512-w" } } }, /pins 4\.149\.0/],
      [{ packages: { ...good.packages, "node_modules/vitest": { version: "1.0.0", integrity: "sha512-v", dev: true } } }, /vitest is a dev dependency/],
      [{ packages: { ...good.packages, "node_modules/left-pad": { version: "1.0.0", integrity: "sha512-l" } } }, /left-pad is not in package-lock\.json/],
      [{ packages: { ...good.packages, "node_modules/esbuild": { version: "0.1.0", integrity: "sha512-other" } } }, /esbuild differs/],
    ];
    for (const [sw, re] of cases) assert.match(shrinkwrapProblems(sw, lockfile, "4.149.0").join("; "), re);
  });

  it("derives the base config and parses JSONC like wrangler", () => {
    const cfg = parseJSONC(`// c\n{\n  "$schema": "x", "name": "backyard", "main": "src/index.ts", /* b */\n  "routes": [{"pattern": "a/*"},],\n  "vars": { "A": "x,}", "B": "//not a comment", },\n  "env": {}, "build": {},\n}\n`);
    assert.deepEqual(cfg.vars, { A: "x,}", B: "//not a comment" });
    assert.deepEqual(baseConfig(cfg), { name: "backyard", vars: { A: "x,}", B: "//not a comment" } });
  });
});
