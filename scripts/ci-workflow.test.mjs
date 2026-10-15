// Checks on the GitHub workflows and the GoReleaser configuration:
// node --test scripts/*.test.mjs
//
// ci.yml: the Terraform configcheck tests (deploy/terraform/*/configcheck) and
// the Helm chart tests (deploy/helm/charttest) skip unless an environment
// variable enables them, so `go test ./...` in the Go job never runs them. The
// jobs that have terraform or helm installed must. The Cloudflare job also
// installs the npm package backyard-cloudflare the way a user does.
//
// release.yml and .goreleaser.yaml: a read-only job builds and tests the
// backyard-cloudflare package (no npm code runs where a write token is), the
// GoReleaser job checks its SHA-256, embeds it in the CLI and attaches it; npm
// gets the release's own bytes, with provenance (public repositories only),
// from npm-publish.yml, which the release calls and maintainers can run by hand.
//
// cloudflare-deploy.yml: only a manual run can confirm an auth downgrade.
//
// The files are read as text (no YAML dependency): jobs are the two-space
// indented keys under `jobs:`, steps the six-space indented list items.
import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const read = (...parts) => readFileSync(join(root, ...parts), "utf8");
const workflows = {
  "ci.yml": read(".github", "workflows", "ci.yml"),
  "release.yml": read(".github", "workflows", "release.yml"),
  "npm-publish.yml": read(".github", "workflows", "npm-publish.yml"),
  "cloudflare-deploy.yml": read(".github", "workflows", "cloudflare-deploy.yml"),
};
const goreleaser = read(".goreleaser.yaml");

/** The lines of a workflow's `jobs:` section. */
function jobLines(file) {
  const lines = workflows[file].split("\n");
  const start = lines.indexOf("jobs:");
  assert.ok(start >= 0, `no jobs: in ${file}`);
  let end = start + 1;
  while (end < lines.length && !/^[A-Za-z0-9_-]+:/.test(lines[end])) end++;
  return lines.slice(start + 1, end);
}

/** The names of a workflow's jobs. */
function jobNames(file) {
  return jobLines(file)
    .map((l) => /^ {2}([A-Za-z0-9_-]+):\s*$/.exec(l)?.[1])
    .filter(Boolean);
}

/** The text of one job under `jobs:` (two-space indented key to the next). */
function job(name, file = "ci.yml") {
  const lines = jobLines(file);
  const start = lines.findIndex((l) => l === `  ${name}:`);
  assert.ok(start >= 0, `no job ${name} in ${file}`);
  let end = start + 1;
  while (end < lines.length && !/^ {2}[A-Za-z0-9_-]+:\s*$/.test(lines[end])) end++;
  return lines.slice(start, end).join("\n");
}

/** The steps of a job, each as its text. */
function steps(jobText) {
  return jobText.split(/\n(?= {6}- )/).filter((s) => /^ {6}- /.test(s));
}

/** The script of a step's run: (a one-liner or the indented block under `run: |`). */
function runScript(stepText) {
  const lines = stepText.split("\n");
  const i = lines.findIndex((l) => /^ {8}run:/.test(l));
  if (i < 0) return "";
  const first = lines[i].replace(/^ {8}run:\s*/, "");
  if (!/^[|>]/.test(first)) return first;
  const out = [];
  for (const l of lines.slice(i + 1)) {
    if (l.trim() !== "" && !/^ {10}/.test(l)) break;
    out.push(l);
  }
  return out.join("\n");
}

/** The index of the only step that matches pred (a RegExp or a substring). */
function stepIndex(list, pred, what) {
  const matches = (s) => (pred instanceof RegExp ? pred.test(s) : s.includes(pred));
  const found = list.flatMap((s, i) => (matches(s) ? [i] : []));
  assert.equal(found.length, 1, `expected exactly one step ${what}, found ${found.length}`);
  return found[0];
}

/** A top-level section of .goreleaser.yaml (`key:` to the next top-level key). */
function goreleaserSection(key) {
  const lines = goreleaser.split("\n");
  const start = lines.indexOf(`${key}:`);
  assert.ok(start >= 0, `no ${key}: in .goreleaser.yaml`);
  let end = start + 1;
  while (end < lines.length && !/^[A-Za-z0-9_]+:/.test(lines[end])) end++;
  return lines.slice(start, end).join("\n");
}

/** The items of a top-level list in .goreleaser.yaml, by id. */
function goreleaserItems(key) {
  const items = {};
  for (const item of goreleaserSection(key).split(/\n(?= {2}- id: )/).slice(1)) {
    items[/^ {2}- id: (\S+)/.exec(item)[1]] = item;
  }
  return items;
}

test("the terraform job loads the rendered configs with BACKYARD_TEST_TERRAFORM set", () => {
  const tf = job("terraform");
  assert.match(tf, /module: \[aws, gcp, azure, cloudflare\]/);
  const step = steps(tf).find((s) => /go test .*\/configcheck\//.test(s));
  assert.ok(step, "no configcheck step in the terraform job");
  assert.match(step, /BACKYARD_TEST_TERRAFORM: "1"/);
  assert.match(step, /go test .*"\.\/\$\{\{ steps\.module\.outputs\.dir \}\}\/configcheck\/"/);
  assert.ok(steps(tf).some((s) => s.includes("actions/setup-go")), "the terraform job needs Go for configcheck");
  // TF_PLUGIN_CACHE_DIR lets configcheck reuse the providers the job installed.
  assert.match(tf, /TF_PLUGIN_CACHE_DIR:/);
  for (const cloud of ["aws", "gcp", "azure"]) {
    assert.ok(existsSync(join(root, "deploy", "terraform", cloud, "configcheck")), `deploy/terraform/${cloud}/configcheck is missing`);
  }
});

test("the helm job runs the chart tests with helm required", () => {
  const helm = job("helm");
  const step = steps(helm).find((s) => s.includes("./deploy/helm/..."));
  assert.ok(step, "no chart test step in the helm job");
  assert.match(step, /BACKYARD_TEST_HELM: "1"/);
  assert.ok(steps(helm).some((s) => s.includes("actions/setup-go")), "the helm job needs Go");
});

test("the cloudflare job installs the npm package like a user and runs it", () => {
  const list = steps(job("cloudflare"));
  const testStep = stepIndex(list, /^ {6}- name: Test\n/, "named Test");
  const pack = stepIndex(list, "scripts/pack.mjs", "running scripts/pack.mjs");
  assert.match(list[pack], /run: node scripts\/pack\.mjs --version "0\.0\.0-ci\.\$GITHUB_RUN_NUMBER" --out "\$RUNNER_TEMP\/cf-pack"/);
  assert.doesNotMatch(list[pack], /working-directory/, "pack.mjs runs in deploy/cloudflare (the job default)");

  const install = stepIndex(list, /npm install .*cf-pack/, "installing the .tgz");
  assert.match(
    list[install],
    /run: npm install --no-audit --no-fund --prefix "\$RUNNER_TEMP\/cf-user" "\$RUNNER_TEMP"\/cf-pack\/backyard-cloudflare-\*\.tgz$/m,
  );
  assert.doesNotMatch(list[install], /--omit|--ignore-scripts|--no-save/, "a plain user install");
  assert.match(list[install], /working-directory: \$\{\{ runner\.temp \}\}/, "installed outside the checkout");

  const engine = '"$engine"';
  const run = stepIndex(list, "setup --dry-run", "running the installed engine");
  assert.match(list[run], /working-directory: \$\{\{ runner\.temp \}\}/);
  assert.match(list[run], /XDG_CONFIG_HOME: \$\{\{ runner\.temp \}\}\/cfg/, "a private config dir (no state)");
  assert.ok(list[run].includes('engine="$RUNNER_TEMP/cf-user/node_modules/backyard-cloudflare/bin/backyard-cloudflare.mjs"'), list[run]);
  assert.ok(list[run].includes(`node ${engine} --version`), "the engine reports its version");
  assert.ok(list[run].includes('grep -Fq "backyard-cloudflare 0.0.0-ci.$GITHUB_RUN_NUMBER (wrangler "'), "and it is the packed one");
  assert.ok(list[run].includes(`node ${engine} setup --dry-run --non-interactive`), list[run]);

  assert.ok(testStep < pack && pack < install && install < run, "test → pack → install → run");
  // The source checkout is still bundled like a CI deploy.
  stepIndex(list, "node scripts/cloudflare-ci.mjs deploy --dry-run", "bundling with scripts/cloudflare-ci.mjs");
});

test("the release builds and tests the backyard-cloudflare package in a read-only job", () => {
  const pkg = job("cloudflare-package", "release.yml");
  assert.match(pkg, /^ {4}permissions:\n {6}contents: read\n {4}outputs:/m, "contents: read and nothing else");
  assert.doesNotMatch(pkg, /: write\b/);
  assert.match(pkg, /^ {6}sha256: \$\{\{ steps\.pack\.outputs\.sha256 \}\}$/m, "the digest pack.mjs reports is a job output");

  const list = steps(pkg);
  const checkout = stepIndex(list, "actions/checkout", "checking out");
  assert.match(list[checkout], /persist-credentials: false/, "no token left on disk for npm code");
  const cf = stepIndex(list, /run: make cloudflare$/m, "running make cloudflare");
  const pack = stepIndex(list, "make cloudflare-pack", "running make cloudflare-pack");
  assert.match(list[pack], /^ {8}id: pack$/m);
  assert.match(list[pack], /run: make cloudflare-pack CF_PACK_VERSION="\$\{GITHUB_REF_NAME#v\}"$/m, "the package has the tag's version");
  const upload = stepIndex(list, "actions/upload-artifact", "uploading the package");
  assert.match(list[upload], /^ {10}name: dist-npm$/m);
  assert.match(list[upload], /^ {10}path: dist-npm\/$/m);
  assert.match(list[upload], /^ {10}if-no-files-found: error$/m);
  assert.ok(checkout < cf && cf < pack && pack < upload, "checkout → make cloudflare → pack → upload");
});

test("the GoReleaser job runs no npm code, verifies the package and pins its digest", () => {
  const gr = job("goreleaser", "release.yml");
  assert.match(gr, /^ {4}needs: cloudflare-package$/m);
  assert.doesNotMatch(gr, /make cloudflare|npm (ci|install|test|run|exec)\b|npx /, "the npm toolchain stays out of the job with the write token");

  const list = steps(gr);
  const checkout = stepIndex(list, "actions/checkout", "checking out");
  assert.match(list[checkout], /fetch-depth: 0/);
  assert.match(list[checkout], /persist-credentials: false/, "GoReleaser reads GITHUB_TOKEN from env");
  const checks = stepIndex(list, "name: Pre-release checks", "with the pre-release checks");
  const download = stepIndex(list, "actions/download-artifact", "downloading the package");
  assert.match(list[download], /^ {10}name: dist-npm$/m);
  assert.match(list[download], /^ {10}path: dist-npm$/m);
  const digest = stepIndex(list, "BACKYARD_CF_PACKAGE_SHA256=", "exporting the package digest");
  assert.match(list[digest], /^ {10}PACKAGE_SHA256: \$\{\{ needs\.cloudflare-package\.outputs\.sha256 \}\}$/m, "compared with what the package job reported");
  assert.ok(list[digest].includes('f="dist-npm/backyard-cloudflare-${GITHUB_REF_NAME#v}.tgz"'), `this version's package:\n${list[digest]}`);
  assert.ok(list[digest].includes('sha="$(sha256sum "$f" | cut -d\' \' -f1)"'), "the digest is computed from the downloaded bytes");
  assert.match(list[digest], /grep -Eqx '\[0-9a-f\]\{64\}'/, "a missing package fails the release");
  assert.ok(list[digest].includes('[ "$sha" != "$PACKAGE_SHA256" ]'), "a mismatch with the job output fails the release");
  assert.ok(list[digest].includes('[ "$(cut -d\' \' -f1 "$f.sha256")" != "$sha" ]'), "a mismatch with the sidecar fails the release");
  assert.match(list[digest], /echo "BACKYARD_CF_PACKAGE_SHA256=\$sha" >> "\$GITHUB_ENV"/);
  const release = stepIndex(list, "goreleaser/goreleaser-action", "running GoReleaser");
  assert.doesNotMatch(list[release], /BACKYARD_CF_PACKAGE_SHA256/, "GoReleaser gets the digest from $GITHUB_ENV");
  assert.ok(checkout < checks && checks < download && download < digest && digest < release, "checks → download → digest → GoReleaser");
});

test("no release job with a write permission runs npm code or keeps checkout credentials", () => {
  for (const file of ["release.yml", "npm-publish.yml"]) {
    for (const name of jobNames(file)) {
      const text = job(name, file);
      if (!/^ {6}[a-z-]+: write\b/m.test(text)) continue;
      assert.doesNotMatch(text, /make cloudflare|npm (ci|install|test|run|exec)\b|npx /, `${file}: job ${name} has a write permission`);
      for (const s of steps(text).filter((s) => s.includes("actions/checkout"))) {
        assert.match(s, /persist-credentials: false/, `${file}: job ${name} has a write permission`);
      }
    }
  }
});

test("with npm publishing on, a private repository fails the release before GoReleaser", () => {
  const list = steps(job("goreleaser", "release.yml"));
  const guard = stepIndex(list, "github.event.repository.private", "refusing a private repository");
  assert.match(list[guard], /^ {8}if: vars\.BACKYARD_NPM_PUBLISH == 'true' && github\.event\.repository\.private$/m);
  assert.match(list[guard], /exit 1/);
  assert.ok(guard < stepIndex(list, "goreleaser/goreleaser-action", "running GoReleaser"), "before the GitHub release exists");
});

test("the release's npm job calls npm-publish.yml with the tag, only when switched on", () => {
  const npm = job("npm", "release.yml");
  assert.match(npm, /^ {4}needs: goreleaser$/m);
  assert.match(npm, /^ {4}if: vars\.BACKYARD_NPM_PUBLISH == 'true'$/m);
  assert.match(npm, /^ {4}uses: \.\/\.github\/workflows\/npm-publish\.yml$/m);
  assert.match(npm, /^ {4}with:\n {6}tag: \$\{\{ github\.ref_name \}\}$/m);
  // Only NPM_TOKEN; a secret of that name in the npm environment takes precedence.
  assert.match(npm, /^ {4}secrets:\n {6}NPM_TOKEN: \$\{\{ secrets\.NPM_TOKEN \}\}$/m);
  assert.doesNotMatch(npm, /secrets: inherit/);
  // A called workflow gets at most the permissions of the job that calls it.
  assert.match(npm, /^ {4}permissions:\n {6}contents: read\n {6}id-token: write\b/m);
});

test("npm-publish.yml is called by the release or run by hand, with the tag of a release", () => {
  const wf = workflows["npm-publish.yml"];
  const on = wf.slice(wf.indexOf("\non:\n"), wf.indexOf("\npermissions:\n"));
  const call = on.slice(on.indexOf("\n  workflow_call:\n"), on.indexOf("\n  workflow_dispatch:\n"));
  assert.match(call, /^ {4}inputs:\n {6}tag:\n {8}description: .+\n {8}type: string\n {8}required: true$/m);
  assert.match(call, /^ {4}secrets:\n {6}NPM_TOKEN:\n {8}required: false$/m, "the release passes NPM_TOKEN");
  const dispatch = on.slice(on.indexOf("\n  workflow_dispatch:\n"));
  assert.match(dispatch, /^ {4}inputs:\n {6}tag:\n {8}description: .+\n {8}type: string\n {8}required: true$/m);
  assert.deepEqual(
    [...on.matchAll(/^ {2}([a-z_]+):$/gm)].map((m) => m[1]),
    ["workflow_call", "workflow_dispatch"],
    "no other trigger",
  );
  assert.match(wf, /^permissions:\n {2}contents: read$/m);
  assert.deepEqual(jobNames("npm-publish.yml"), ["npm"]);
});

test("npm-publish.yml publishes the release's own package with provenance", () => {
  const npm = job("npm", "npm-publish.yml");
  assert.match(npm, /^ {4}environment:\n {6}name: npm$/m, "NPM_TOKEN can be a secret of the npm environment");
  assert.match(npm, /^ {4}permissions:\n {6}contents: read\n {6}id-token: write\b/m);
  assert.doesNotMatch(npm, /contents: write|actions\/checkout|npm pack|make /, "npm gets the release asset, not a rebuild");
  // The tag reaches the scripts as $TAG, never interpolated into them.
  assert.match(npm, /^ {4}env:\n {6}TAG: \$\{\{ inputs\.tag \}\}$/m);
  assert.doesNotMatch(npm, /GITHUB_REF_NAME#v/, "the tag input names the release, not the ref the run is on");

  const list = steps(npm);
  const check = stepIndex(list, "name: Check the tag", "checking the tag");
  const script = runScript(list[check]);
  assert.ok(script.includes(`grep -Eqx 'v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?'`), `a release tag only:\n${script}`);
  assert.ok(script.includes('[ "$GITHUB_REF_TYPE" = tag ] && [ "$GITHUB_REF_NAME" != "$TAG" ]'), "run on another tag, the provenance would name its commit");
  assert.match(list[check], /^ {10}PRIVATE: \$\{\{ github\.event\.repository\.private \}\}$/m);
  assert.ok(script.includes('[ "$PRIVATE" = true ]'), "a private repository fails before anything is published");

  const node = stepIndex(list, "actions/setup-node", "setting up Node");
  assert.match(list[node], /registry-url: "https:\/\/registry\.npmjs\.org"/);
  const download = stepIndex(list, "gh release download", "downloading the release asset");
  const fetch = runScript(list[download]);
  assert.ok(fetch.includes('gh release view "$TAG" --repo "$GITHUB_REPOSITORY" --json isDraft --jq .isDraft'), "a published release only");
  assert.ok(fetch.includes('[ "$draft" != false ]'));
  assert.ok(
    fetch.includes(`gh release download "$TAG" --repo "$GITHUB_REPOSITORY" --pattern 'backyard-cloudflare-*.tgz' --pattern checksums.txt --dir pkg`),
    fetch,
  );
  const verify = stepIndex(list, "sha256sum", "verifying the download");
  assert.match(list[verify], /sha256sum --ignore-missing -c checksums\.txt/);
  assert.ok(list[verify].includes('backyard-cloudflare-${TAG#v}\\.tgz$" checksums.txt'), "the package must be listed");
  const publish = stepIndex(list, "npm publish", "publishing");
  assert.match(list[publish], /NODE_AUTH_TOKEN: \$\{\{ secrets\.NPM_TOKEN \}\}/);
  assert.ok(runScript(list[publish]).includes('if [ -z "$NODE_AUTH_TOKEN" ]; then'), "a missing token fails the job");
  assert.ok(list[publish].includes('version="${TAG#v}"'));
  assert.ok(
    list[publish].includes('npm publish "./pkg/backyard-cloudflare-${version}.tgz" --provenance --access public --tag "$tag"'),
    list[publish],
  );
  assert.ok(check < node && node < download && download < verify && verify < publish, "check → download → verify → publish");
});

test("npm-publish.yml picks the dist-tag: latest, next for a pre-release, release-X.Y for an older line", () => {
  const list = steps(job("npm", "npm-publish.yml"));
  const script = runScript(list[stepIndex(list, "npm publish", "publishing")]);
  // The script up to npm publish, with a fake npm whose `npm view` prints
  // npm's current latest (and fails, like E404, when there is none).
  const choose = script.slice(script.indexOf('version="${TAG#v}"'), script.indexOf("npm publish "));
  const dir = mkdtempSync(join(tmpdir(), "npm-publish-"));
  writeFileSync(join(dir, "npm"), '#!/bin/sh\n[ -n "$FAKE_LATEST" ] || exit 1\necho "$FAKE_LATEST"\n', { mode: 0o755 });
  const distTag = (version, latest) =>
    execFileSync("bash", ["-ec", `${choose}\nprintf '%s' "$tag"`], {
      env: { PATH: `${dir}:${process.env.PATH}`, TAG: `v${version}`, FAKE_LATEST: latest },
      encoding: "utf8",
    }).replace(/^::notice::.*\n/m, "");
  try {
    assert.equal(distTag("0.2.0", ""), "latest", "a new package");
    assert.equal(distTag("0.2.0", "0.0.0"), "latest");
    assert.equal(distTag("1.0.0", "0.2.0"), "latest");
    assert.equal(distTag("0.10.0", "0.9.3"), "latest", "versions compare numerically");
    assert.equal(distTag("0.2.0", "0.2.0"), "latest", "a republish is npm's to refuse");
    assert.equal(distTag("0.1.1", "0.2.0"), "release-0.1", "an older line keeps latest");
    assert.equal(distTag("0.9.3", "0.10.0"), "release-0.9");
    assert.equal(distTag("1.2.0-rc.1", "1.1.0"), "next");
    assert.equal(distTag("0.1.0-rc.1", "0.2.0"), "next");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("only the publishing job runs in the npm environment, whose NPM_TOKEN no other job may read", () => {
  for (const file of Object.keys(workflows)) {
    for (const name of jobNames(file)) {
      if (file === "npm-publish.yml" && name === "npm") continue;
      assert.doesNotMatch(job(name, file), /^ {4}environment:/m, `${file}: job ${name}`);
    }
  }
});

test("no workflow interpolates inputs or event data into a script", () => {
  // Such values reach scripts through env:, where they cannot inject shell code.
  let seen = 0;
  for (const [file, text] of Object.entries(workflows)) {
    for (const name of jobNames(file)) {
      for (const s of steps(job(name, file))) {
        const script = runScript(s);
        if (script) seen++;
        assert.doesNotMatch(script, /\$\{\{[^}]*\b(inputs|github\.event|github\.head_ref)\b/, `${file}: job ${name}:\n${script}`);
      }
    }
  }
  assert.ok(seen > 0, "no run: scripts found");
});

test("npm publish gets a file path npm cannot mistake for a GitHub repository", () => {
  // npm-package-arg reads "pkg/x.tgz" as the GitHub shorthand user/repo.
  let seen = 0;
  for (const [file, text] of Object.entries(workflows)) {
    for (const m of text.matchAll(/npm publish (\S+)/g)) {
      seen++;
      assert.match(m[1], /^"?(\.\/|\/|\$PWD\/|\$\{?GITHUB_WORKSPACE\}?\/)/, `${file}: npm publish ${m[1]}`);
    }
  }
  assert.ok(seen > 0, "no npm publish found");
});

test("only the npm job can mint an OIDC token", () => {
  // release.yml's npm job grants it to npm-publish.yml, which it calls.
  const allowed = new Set(["release.yml/npm", "npm-publish.yml/npm"]);
  for (const file of Object.keys(workflows)) {
    const head = workflows[file].slice(0, workflows[file].indexOf("\njobs:"));
    assert.doesNotMatch(head, /id-token/, `${file}: workflow-level id-token permission`);
    for (const name of jobNames(file)) {
      if (allowed.has(`${file}/${name}`)) continue;
      assert.doesNotMatch(job(name, file), /id-token/, `${file}: job ${name} must not have id-token`);
    }
  }
  for (const id of allowed) {
    const [file, name] = id.split("/");
    assert.ok(jobNames(file).includes(name), id);
  }
});

test(".goreleaser.yaml embeds the package digest and attaches the package", () => {
  const ldflag = '-X github.com/thasami/backyard/internal/cli.cloudflarePackageSHA256={{ envOrDefault "BACKYARD_CF_PACKAGE_SHA256" "" }}';
  const builds = goreleaserItems("builds");
  assert.ok(builds.backyard?.includes(`- ${ldflag}`), `the backyard build lacks ${ldflag}`);
  assert.ok(!builds["backyard-lambda"]?.includes("cloudflarePackageSHA256"), "only the CLI uses the digest");
  const glob = /^ {2}extra_files:\n {4}- glob: \.\/dist-npm\/backyard-cloudflare-\*\.tgz$/m;
  assert.match(goreleaserSection("release"), glob, "release.extra_files");
  assert.match(goreleaserSection("checksum"), glob, "checksum.extra_files (checksums.txt lists the package)");
  // The release notes name the npm package only when the npm job publishes it
  // (until then npm may hold nothing, or something else, under that name);
  // otherwise they name the release's own asset.
  const npmBranch = /\{\{- if eq \(envOrDefault "BACKYARD_NPM_PUBLISH" ""\) "true" \}\} or\n\s*`npx backyard-cloudflare@\{\{ \.Version \}\} setup`\n\s*\{\{- else \}\}[^{]*`npx --package https:\/\/github\.com\/thasami\/backyard\/releases\/download\/\{\{ \.Tag \}\}\/backyard-cloudflare-\{\{ \.Version \}\}\.tgz backyard-cloudflare setup`\n\s*\{\{- end \}\}/;
  assert.match(goreleaserSection("release"), npmBranch, "the release notes name the package (npm only when it is published)");
  assert.equal(goreleaser.match(/npx backyard-cloudflare@/g)?.length, 1, "no other unconditional npx backyard-cloudflare@<version>");
  const run = steps(job("goreleaser", "release.yml")).find((s) => s.includes("goreleaser/goreleaser-action"));
  assert.match(run, /^ {10}BACKYARD_NPM_PUBLISH: \$\{\{ vars\.BACKYARD_NPM_PUBLISH \}\}$/m, "GoReleaser sees whether the npm job publishes");
});

test("dist-npm/ is ignored, so GoReleaser's clean-tree check passes", () => {
  const ignore = read(".gitignore").split("\n").map((l) => l.trim());
  assert.ok(ignore.includes("/dist-npm/"), ".gitignore lacks /dist-npm/");
});

test("the Cloudflare deploy workflow confirms auth changes per manual run only, and gets no admin token", () => {
  const wf = workflows["cloudflare-deploy.yml"];
  const deploy = job("deploy", "cloudflare-deploy.yml");
  const step = steps(deploy)[stepIndex(steps(deploy), "node scripts/cloudflare-ci.mjs deploy", "deploying")];
  // inputs is empty on push, so a push deploy always runs the guard.
  assert.match(step, /^ {10}BACKYARD_CF_CONFIRM_AUTH_CHANGE: \$\{\{ inputs\.confirm_auth_change && 'true' \|\| '' \}\}$/m);
  const dispatch = wf.slice(wf.indexOf("\n  workflow_dispatch:\n"), wf.indexOf("\n  push:\n"));
  assert.match(dispatch, /^ {6}confirm_auth_change:\n {8}description: .+\n {8}type: boolean\n {8}default: false$/m, "a workflow_dispatch input, off by default");
  assert.doesNotMatch(wf, /vars\.BACKYARD_CF_CONFIRM_AUTH_CHANGE/, "a repository variable would outlive the run and disable the guard for push deploys");
  assert.match(wf.slice(0, wf.indexOf("\non:")), /^# .*confirm_auth_change/m, "documented in the header");
  assert.doesNotMatch(wf, /BACKYARD_CF_API_TOKEN/, "the CI deployer never gets the admin token");
});
