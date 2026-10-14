// Offline tests of scripts/sync-assets.mjs (node --test): the script runs in a
// sandbox copy of the package against a fake repository, so a real deploy or
// build from a partial checkout fails with a clear message instead of shipping
// placeholders, while development and tests keep working.
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { cpSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { after, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const pkgDir = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
const dirs = [];

/**
 * A sandbox package (scripts/sync-assets.mjs, and package.json with `version`
 * when given) and an empty repository root.
 */
function sandbox({ version } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "backyard-sync-"));
  dirs.push(dir);
  mkdirSync(join(dir, "pkg", "scripts"), { recursive: true });
  cpSync(join(pkgDir, "scripts", "sync-assets.mjs"), join(dir, "pkg", "scripts", "sync-assets.mjs"));
  if (version !== undefined) writeFileSync(join(dir, "pkg", "package.json"), JSON.stringify({ name: "backyard-cloudflare", version }) + "\n");
  mkdirSync(join(dir, "repo"));
  return { pkg: join(dir, "pkg"), repo: join(dir, "repo") };
}

/** The version src/generated/version.ts exports. */
function builtVersion(sb) {
  const ts = readFileSync(join(sb.pkg, "src", "generated", "version.ts"), "utf8");
  return /^export const BUILD_VERSION = "([^"]*)";$/m.exec(ts)?.[1];
}

/** Fills the repository with the sources the Worker bundles. */
function fillRepo(repo) {
  const files = {
    "web/sdk/backyard.js": "/* sdk */\n",
    "web/sdk/backyard.mjs": "export {};\n",
    "web/sdk/backyard.d.ts": "export {};\n",
    "web/hub/index.html": "<!doctype html><title>hub</title>\n",
    "web/hub/docs.html": "<!doctype html><title>docs</title>\n",
    "skills/backyard/SKILL.md": "---\nname: backyard\n---\n\n# Backyard\n",
    "skills/backyard/references/api.md": "# API\n",
    "internal/docstore/conformance/querycases.json": '{"version":1,"collections":{"c":[]},"cases":[]}\n',
    "internal/mcpremote/tools.json": JSON.stringify({ instructions: "Use the tools.", tools: [{ name: "t1", inputSchema: { type: "object" } }] }, null, 2) + "\n",
  };
  for (const [rel, content] of Object.entries(files)) {
    mkdirSync(dirname(join(repo, rel)), { recursive: true });
    writeFileSync(join(repo, rel), content);
  }
}

function sync({ pkg, repo }, args = [], env = {}) {
  const r = spawnSync(process.execPath, [join(pkg, "scripts", "sync-assets.mjs"), ...args], {
    env: { PATH: process.env.PATH, BACKYARD_REPO_ROOT: repo, ...env },
    encoding: "utf8",
  });
  return { code: r.status, out: (r.stdout ?? "") + (r.stderr ?? "") };
}

describe("scripts/sync-assets.mjs", () => {
  after(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
  });

  it("refuses to build or deploy from a checkout without web/, skills/ and the connector's tool catalog", () => {
    const sb = sandbox();
    const runs = [
      ["--strict", {}],
      // wrangler's build step (wrangler.jsonc build.command) of a deploy or an upload.
      ["", { WRANGLER_COMMAND: "deploy" }],
      ["", { WRANGLER_COMMAND: "versions upload" }],
    ];
    for (const [arg, env] of runs) {
      const r = sync(sb, arg ? [arg] : [], env);
      const what = `${arg} ${JSON.stringify(env)}`;
      assert.equal(r.code, 1, what);
      assert.match(r.out, /cannot build the Worker/, what);
      for (const missing of ["web/sdk/backyard.js", "web/hub", "skills/backyard/SKILL.md", "skills/backyard/references", "internal/mcpremote/tools.json"]) {
        assert.ok(r.out.includes(`  - ${missing}`), `${what}: ${missing} in\n${r.out}`);
      }
      assert.match(r.out, /complete checkout \(git clone https:\/\/github\.com\/thasami\/backyard\.git/, what);
      assert.match(r.out, /BACKYARD_REPO_ROOT/, what);
      // People without a checkout get the prebuilt package instead.
      assert.match(r.out, /npx backyard-cloudflare setup/, what);
      assert.ok(!existsSync(join(sb.pkg, "src", "generated")), `${what}: nothing is written`);
    }
  });

  it("uses placeholders for development and tests", () => {
    const sb = sandbox();
    for (const env of [{}, { WRANGLER_COMMAND: "dev" }, { WRANGLER_COMMAND: "types" }]) {
      const r = sync(sb, [], env);
      assert.equal(r.code, 0, r.out);
      assert.match(r.out, /warning: missing web\/sdk\/backyard\.js: using a placeholder/);
    }
    assert.match(readFileSync(join(sb.pkg, "src", "generated", "sdk.ts"), "utf8"), /Backyard SDK placeholder/);
    // An empty tool catalog: the connector lists no tools.
    assert.match(readFileSync(join(sb.pkg, "src", "generated", "mcp.ts"), "utf8"), /export const MCP_CATALOG: \{ instructions: string; tools: McpTool\[\] \} = \{\n  "instructions": "The Claude connector's tool catalog was not bundled[^"]*",\n  "tools": \[\]\n\};\n$/);
    // The shared query cases are marked missing, which their test reports.
    const qc = JSON.parse(readFileSync(join(sb.pkg, "test", "generated", "querycases.json"), "utf8"));
    assert.equal(qc.missing, "internal/docstore/conformance/querycases.json");
    assert.deepEqual(qc.cases, []);
  });

  it("bundles a complete checkout and copies the shared query cases", () => {
    const sb = sandbox();
    fillRepo(sb.repo);
    let r = sync(sb, ["--strict"]);
    assert.equal(r.code, 0, r.out);
    assert.ok(!/warning/.test(r.out), r.out);
    const skills = readFileSync(join(sb.pkg, "src", "generated", "skills.ts"), "utf8");
    assert.match(skills, /"api": "# API\\n"/);
    // The connector's catalog, parsed and re-emitted as MCP_CATALOG.
    const mcp = readFileSync(join(sb.pkg, "src", "generated", "mcp.ts"), "utf8");
    const literal = /export const MCP_CATALOG: [^=]+= (\{[\s\S]*\});\n$/.exec(mcp)?.[1];
    assert.deepEqual(JSON.parse(literal ?? "null"), JSON.parse(readFileSync(join(sb.repo, "internal/mcpremote/tools.json"), "utf8")));
    assert.equal(
      readFileSync(join(sb.pkg, "test", "generated", "querycases.json"), "utf8"),
      readFileSync(join(sb.repo, "internal/docstore/conformance/querycases.json"), "utf8"),
    );
    r = sync(sb, ["--check"], { WRANGLER_COMMAND: "deploy" });
    assert.equal(r.code, 0, r.out);
    // A changed catalog changes ASSETS_HASH and makes --check fail.
    const index = readFileSync(join(sb.pkg, "src", "generated", "index.ts"), "utf8");
    const catalog = join(sb.repo, "internal/mcpremote/tools.json");
    const original = readFileSync(catalog, "utf8");
    writeFileSync(catalog, JSON.stringify({ instructions: "Changed.", tools: [] }));
    r = sync(sb, ["--check"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /out of date/);
    r = sync(sb, ["--strict"]);
    assert.equal(r.code, 0, r.out);
    assert.notEqual(readFileSync(join(sb.pkg, "src", "generated", "index.ts"), "utf8"), index, "ASSETS_HASH covers the catalog");
    // An unparsable catalog is refused by a build.
    writeFileSync(catalog, "{");
    r = sync(sb, ["--strict"]);
    assert.equal(r.code, 1, r.out);
    assert.ok(r.out.includes("  - invalid internal/mcpremote/tools.json ("), r.out);
    writeFileSync(catalog, original);
    r = sync(sb, ["--strict"]);
    assert.equal(r.code, 0, r.out);
    // A changed source (the query cases included) makes --check fail.
    writeFileSync(join(sb.repo, "internal/docstore/conformance/querycases.json"), '{"version":2,"collections":{},"cases":[]}\n');
    r = sync(sb, ["--check"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /out of date/);
    // The query cases are test data: a deploy never needs them.
    rmSync(join(sb.repo, "internal", "docstore"), { recursive: true });
    r = sync(sb, ["--strict"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /warning: cannot read internal\/docstore\/conformance\/querycases\.json/);
  });

  it("is strict for npm run build and wrangler's build step", () => {
    const { scripts } = JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8"));
    assert.match(scripts.build, /^node scripts\/sync-assets\.mjs --strict && wrangler deploy --dry-run/);
    // wrangler's own build step (also run by a plain `npx wrangler deploy`).
    const wrangler = readFileSync(join(pkgDir, "wrangler.jsonc"), "utf8");
    assert.match(wrangler, /"command": "node scripts\/sync-assets\.mjs"/);
    // Deploys go through setup (the engine) or scripts/cloudflare-ci.mjs, which keep the
    // state and the auth guard: no npm script deploys the defaults past them.
    for (const name of ["deploy", "predeploy", "deploy:ci"]) assert.equal(scripts[name], undefined, `npm run ${name}`);
  });

  it("writes the version the Worker reports from BACKYARD_VERSION", () => {
    const sb = sandbox({ version: "9.8.7" });
    fillRepo(sb.repo);
    for (const v of ["1.2.3", "1.2.3-rc.1", "0.0.0-ci.42", "10.20.30-test"]) {
      const r = sync(sb, ["--strict"], { BACKYARD_VERSION: v });
      assert.equal(r.code, 0, r.out);
      assert.equal(builtVersion(sb), v);
      assert.match(r.out, new RegExp(`version ${v.replace(/\./g, "\\.")}`));
    }
    const ts = readFileSync(join(sb.pkg, "src", "generated", "version.ts"), "utf8");
    assert.equal(
      ts,
      '// GENERATED by scripts/sync-assets.mjs — do not edit.\n' +
        '/** Version reported by /__backyard/api/deploy/info: BACKYARD_VERSION at build time, else "<package.json version>-dev". */\n' +
        'export const BUILD_VERSION = "10.20.30-test";\n',
    );
    // --check covers version.ts; the version is not part of ASSETS_HASH.
    const index = readFileSync(join(sb.pkg, "src", "generated", "index.ts"), "utf8");
    let r = sync(sb, ["--check"], { BACKYARD_VERSION: "10.20.30-test" });
    assert.equal(r.code, 0, r.out);
    r = sync(sb, ["--check"], { BACKYARD_VERSION: "10.20.31" });
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /out of date/);
    assert.equal(builtVersion(sb), "10.20.30-test", "--check writes nothing");
    r = sync(sb, [], { BACKYARD_VERSION: "10.20.31" });
    assert.equal(r.code, 0, r.out);
    assert.equal(readFileSync(join(sb.pkg, "src", "generated", "index.ts"), "utf8"), index, "ASSETS_HASH does not depend on the version");
  });

  it("defaults to the package.json version with -dev", () => {
    const sb = sandbox({ version: "9.8.7" });
    let r = sync(sb);
    assert.equal(r.code, 0, r.out);
    assert.equal(builtVersion(sb), "9.8.7-dev");
    // An empty BACKYARD_VERSION (an unset CI variable) means "not set".
    r = sync(sb, [], { BACKYARD_VERSION: "" });
    assert.equal(r.code, 0, r.out);
    assert.equal(builtVersion(sb), "9.8.7-dev");
    // A partial copy without package.json still builds.
    const bare = sandbox();
    r = sync(bare);
    assert.equal(r.code, 0, r.out);
    assert.equal(builtVersion(bare), "0.0.0-dev");
    // The real package.
    const { version } = JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8"));
    const real = sandbox({ version });
    sync(real);
    assert.equal(builtVersion(real), `${version}-dev`);
  });

  it("refuses an invalid BACKYARD_VERSION before writing anything", () => {
    for (const v of ["v1.2.3", "1.2", "1.2.3.4", "1.2.3-", "latest", "1.2.3+build", "1.2.3 rc"]) {
      const sb = sandbox({ version: "9.8.7" });
      fillRepo(sb.repo);
      const r = sync(sb, ["--strict"], { BACKYARD_VERSION: v });
      assert.equal(r.code, 1, `${v}: ${r.out}`);
      assert.match(r.out, /BACKYARD_VERSION must be a semantic version/, v);
      assert.ok(!existsSync(join(sb.pkg, "src", "generated")), `${v}: nothing is written`);
      assert.ok(!existsSync(join(sb.pkg, "test", "generated")), `${v}: nothing is written`);
    }
  });
});
