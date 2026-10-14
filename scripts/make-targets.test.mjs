// Tests for the Makefile's conformance, e2e, tf-configcheck and helm-lint
// targets and for scripts/conformance.sh: node --test scripts/*.test.mjs
//
// The Makefile is checked with `make -n` (nothing runs: make prints the
// recipe it would run, with the target selection already resolved), and
// scripts/conformance.sh against stand-in conformance runners.
import { test } from "node:test";
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, copyFileSync, mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const hasMake = spawnSync("make", ["--version"], { encoding: "utf8" }).status === 0;
const hasSh = process.platform !== "win32" && spawnSync("sh", ["-c", "true"]).status === 0;

// The caller's make (make scripts-test) and environment must not leak into
// the target selection.
function cleanEnv() {
  const env = { ...process.env };
  for (const k of ["MAKEFLAGS", "MFLAGS", "MAKELEVEL", "MAKEOVERRIDES", "CONFORMANCE_TARGET", "CONFORMANCE_ARGS", "E2E_TARGET"]) delete env[k];
  return env;
}

/** make -n TARGET VARS... in the repository root: {status, out, err}. */
function dryRun(target, ...vars) {
  const r = spawnSync("make", ["--no-print-directory", "-n", "-C", root, target, "NODE=node", "NPM=npm", ...vars], {
    encoding: "utf8",
    env: cleanEnv(),
  });
  return { status: r.status, out: r.stdout, err: r.stderr };
}

/** The runners the conformance recipe loops over, in order. */
function runners(out) {
  const m = /for runner in ([^;]*);/.exec(out);
  assert.ok(m, `no runner loop in:\n${out}`);
  return m[1].trim().split(/\s+/);
}

test("make conformance runs both editions' runners by default", { skip: !hasMake && "make not found" }, () => {
  const r = dryRun("conformance");
  assert.equal(r.status, 0, r.err);
  assert.deepEqual(runners(r.out), ["conformance/run-go.sh", "conformance/run-worker.sh"]);
  assert.match(r.out, /deploy\/cloudflare\/node_modules\/wrangler/, "the Worker's dependencies are installed when missing");
  assert.match(r.out, /\|\| status=1/, "a failing edition does not stop the other");
  assert.doesNotMatch(r.out, /scripts\/conformance\.sh|with-server\.sh/);
});

test("make conformance CONFORMANCE_TARGET selects the runner", { skip: !hasMake && "make not found" }, () => {
  const go = dryRun("conformance", "CONFORMANCE_TARGET=go");
  assert.equal(go.status, 0, go.err);
  assert.deepEqual(runners(go.out), ["conformance/run-go.sh"]);
  assert.doesNotMatch(go.out, /run-worker\.sh|node_modules\/wrangler/);

  const worker = dryRun("conformance", "CONFORMANCE_TARGET=worker");
  assert.equal(worker.status, 0, worker.err);
  assert.deepEqual(runners(worker.out), ["conformance/run-worker.sh"]);
  assert.doesNotMatch(worker.out, /run-go\.sh/);

  const all = dryRun("conformance", "CONFORMANCE_TARGET=all");
  assert.deepEqual(runners(all.out), ["conformance/run-go.sh", "conformance/run-worker.sh"]);

  const args = dryRun("conformance", "CONFORMANCE_TARGET=go", "CONFORMANCE_ARGS=path -- --test-name-pattern=files");
  assert.match(args.out, /\.\/\$runner path -- --test-name-pattern=files /);

  for (const bad of ["bogus", "", "go worker", "external"]) {
    const r = dryRun("conformance", `CONFORMANCE_TARGET=${bad}`);
    assert.notEqual(r.status, 0, bad);
    assert.match(r.err, /CONFORMANCE_TARGET must be go, worker or all/, bad);
  }
});

test("make e2e runs the Playwright suite by default", { skip: !hasMake && "make not found" }, () => {
  for (const vars of [[], ["E2E_TARGET=go"]]) {
    const r = dryRun("e2e", ...vars);
    assert.equal(r.status, 0, r.err);
    assert.match(r.out, /^cd e2e && npm ci --no-audit --no-fund && npm test$/m);
    assert.doesNotMatch(r.out, /with-server\.sh|web\/test\/e2e\.test\.mjs/);
  }
});

test("make e2e E2E_TARGET=worker tests the Worker with web/test/e2e.test.mjs", { skip: !hasMake && "make not found" }, () => {
  const r = dryRun("e2e", "E2E_TARGET=worker");
  assert.equal(r.status, 0, r.err);
  assert.match(r.out, /\.\/scripts\/with-server\.sh worker -- node --test web\/test\/e2e\.test\.mjs$/m);
  assert.match(r.out, /PLAYWRIGHT_CORE="\$\{PLAYWRIGHT_CORE:-[^"]*\/e2e\/node_modules\/playwright-core\/index\.mjs\}"/, "playwright-core defaults to e2e/'s");
  assert.match(r.out, /deploy\/cloudflare\/node_modules\/wrangler/);
  assert.doesNotMatch(r.out, /npm test/, "not the Playwright suite");

  const bad = dryRun("e2e", "E2E_TARGET=external");
  assert.notEqual(bad.status, 0);
  assert.match(bad.err, /E2E_TARGET must be go or worker/);
});

test("scripts/conformance.sh hands go and worker over to the conformance runners", { skip: !hasSh && "no POSIX sh" }, () => {
  const dir = mkdtempSync(join(tmpdir(), "backyard-conformance-sh-"));
  mkdirSync(join(dir, "scripts"));
  mkdirSync(join(dir, "conformance"));
  copyFileSync(join(root, "scripts", "conformance.sh"), join(dir, "scripts", "conformance.sh"));
  for (const name of ["run-go.sh", "run-worker.sh"]) {
    const p = join(dir, "conformance", name);
    writeFileSync(p, `#!/bin/sh\necho "${name}:$#:$*"\n`);
    chmodSync(p, 0o755);
  }
  const run = (...args) => spawnSync("sh", [join(dir, "scripts", "conformance.sh"), ...args], { encoding: "utf8", env: cleanEnv() });

  let r = run();
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout.trim(), "run-go.sh:0:");
  r = run("go", "path", "--", "--test-name-pattern=files");
  assert.equal(r.stdout.trim(), "run-go.sh:3:path -- --test-name-pattern=files");
  r = run("worker", "host");
  assert.equal(r.status, 0, r.stderr);
  assert.equal(r.stdout.trim(), "run-worker.sh:1:host");
  r = run("bogus");
  assert.equal(r.status, 2);
  assert.match(r.stderr, /usage/);
});

test("make tf-configcheck loads every Terraform module's rendered config with terraform enabled", { skip: !hasMake && "make not found" }, () => {
  const r = dryRun("tf-configcheck", "GO=go");
  assert.equal(r.status, 0, r.err);
  const line = r.out.split("\n").find((l) => l.includes("go test"));
  assert.ok(line, `no go test in:\n${r.out}`);
  assert.match(line, /^BACKYARD_TEST_TERRAFORM=1 go test /, "without BACKYARD_TEST_TERRAFORM the tests only skip");
  assert.match(line, / -p 1 /, "the modules share Terraform's plugin cache, which is not safe for concurrent inits");
  for (const cloud of ["aws", "azure", "gcp"]) {
    assert.ok(line.includes(`./deploy/terraform/${cloud}/configcheck/`), `${cloud} is missing from: ${line}`);
  }
  assert.doesNotMatch(line, /cloudflare/, "the Cloudflare module has no configcheck package");
});

test("make helm-lint runs the chart's guard tests with helm required", { skip: !hasMake && "make not found" }, () => {
  const r = dryRun("helm-lint", "GO=go", "HELM=helm");
  assert.equal(r.status, 0, r.err);
  assert.match(r.out, /^BACKYARD_TEST_HELM=1 go test -count=1 \.\/deploy\/helm\/\.\.\.$/m);
});
