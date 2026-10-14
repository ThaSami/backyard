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

/** A sandbox package (scripts/sync-assets.mjs only) and an empty repository root. */
function sandbox() {
  const dir = mkdtempSync(join(tmpdir(), "backyard-sync-"));
  dirs.push(dir);
  mkdirSync(join(dir, "pkg", "scripts"), { recursive: true });
  cpSync(join(pkgDir, "scripts", "sync-assets.mjs"), join(dir, "pkg", "scripts", "sync-assets.mjs"));
  mkdirSync(join(dir, "repo"));
  return { pkg: join(dir, "pkg"), repo: join(dir, "repo") };
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

  it("refuses to build or deploy from a checkout without web/ and skills/", () => {
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
      for (const missing of ["web/sdk/backyard.js", "web/hub", "skills/backyard/SKILL.md", "skills/backyard/references"]) {
        assert.ok(r.out.includes(`  - ${missing}`), `${what}: ${missing} in\n${r.out}`);
      }
      assert.match(r.out, /complete checkout \(git clone https:\/\/github\.com\/thasami\/backyard\.git/, what);
      assert.match(r.out, /BACKYARD_REPO_ROOT/, what);
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
    assert.equal(
      readFileSync(join(sb.pkg, "test", "generated", "querycases.json"), "utf8"),
      readFileSync(join(sb.repo, "internal/docstore/conformance/querycases.json"), "utf8"),
    );
    r = sync(sb, ["--check"], { WRANGLER_COMMAND: "deploy" });
    assert.equal(r.code, 0, r.out);
    // A changed source (the query cases included) makes --check fail.
    writeFileSync(join(sb.repo, "internal/docstore/conformance/querycases.json"), '{"version":2,"collections":{},"cases":[]}\n');
    r = sync(sb, ["--check"]);
    assert.equal(r.code, 1, r.out);
    assert.match(r.out, /out of date/);
    // The query cases are test data: a deploy never needs them.
    rmSync(join(sb.repo, "internal"), { recursive: true });
    r = sync(sb, ["--strict"]);
    assert.equal(r.code, 0, r.out);
    assert.match(r.out, /warning: cannot read internal\/docstore\/conformance\/querycases\.json/);
  });

  it("is strict for npm run build and npm run deploy", () => {
    const { scripts } = JSON.parse(readFileSync(join(pkgDir, "package.json"), "utf8"));
    assert.match(scripts.build, /^node scripts\/sync-assets\.mjs --strict && wrangler deploy --dry-run/);
    assert.match(scripts.predeploy, /sync-assets\.mjs --strict/);
    assert.match(scripts["deploy:ci"], /sync-assets\.mjs --strict && wrangler deploy/);
    // wrangler's own build step (also run by a plain `npx wrangler deploy`).
    const wrangler = readFileSync(join(pkgDir, "wrangler.jsonc"), "utf8");
    assert.match(wrangler, /"command": "node scripts\/sync-assets\.mjs"/);
  });
});
