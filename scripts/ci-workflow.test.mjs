// Checks on .github/workflows/ci.yml that keep tests from silently skipping
// in CI: node --test scripts/*.test.mjs
//
// The Terraform configcheck tests (deploy/terraform/*/configcheck) and the
// Helm chart tests (deploy/helm/charttest) skip unless an environment
// variable enables them, so `go test ./...` in the Go job never runs them.
// The jobs that have terraform or helm installed must.
import { test } from "node:test";
import assert from "node:assert/strict";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const workflow = readFileSync(join(root, ".github", "workflows", "ci.yml"), "utf8");

/** The text of one job under `jobs:` (two-space indented key to the next). */
function job(name) {
  const lines = workflow.split("\n");
  const start = lines.findIndex((l) => l === `  ${name}:`);
  assert.ok(start >= 0, `no job ${name} in ci.yml`);
  let end = start + 1;
  while (end < lines.length && !/^ {2}[A-Za-z0-9_-]+:\s*$/.test(lines[end])) end++;
  return lines.slice(start, end).join("\n");
}

/** The steps of a job, each as its text. */
function steps(jobText) {
  return jobText.split(/\n(?= {6}- )/).filter((s) => /^ {6}- /.test(s));
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
