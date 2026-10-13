// Playwright configuration for Backyard's real-browser end-to-end tests.
// global-setup.mjs builds the binary and starts the servers; see README.md.
import { defineConfig, devices } from "@playwright/test";

import { URLS } from "./harness/env.mjs";

const CI = !!process.env.CI;

export default defineConfig({
  testDir: "./tests",
  testMatch: "**/*.spec.mjs",
  globalSetup: "./global-setup.mjs",
  outputDir: "./test-results",
  timeout: 60_000,
  expect: { timeout: 10_000 },
  forbidOnly: CI,
  retries: CI ? 1 : 0,
  // Spec files run in parallel; the tests inside one file share a site and run in order.
  workers: Number(process.env.BACKYARD_E2E_WORKERS) || (CI ? 2 : 4),
  reporter: CI ? [["list"], ["github"], ["html", { open: "never", outputFolder: "playwright-report" }]] : [["list"]],
  use: {
    ...devices["Desktop Chrome"],
    headless: true,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    // The browsers in PLAYWRIGHT_BROWSERS_PATH match this Playwright version; an
    // explicit binary (another Chromium build) can be given with BACKYARD_E2E_CHROMIUM.
    launchOptions: process.env.BACKYARD_E2E_CHROMIUM ? { executablePath: process.env.BACKYARD_E2E_CHROMIUM } : {},
  },
  projects: [
    {
      // `backyard serve --dev`: host mode on *.localhost, guests, fake AI.
      name: "sites",
      testDir: "./tests/sites",
      use: { baseURL: URLS.main },
    },
    {
      // auth.mode oidc against the mock IdP, path mode on 127.0.0.1.
      name: "auth",
      testDir: "./tests/auth",
      use: { baseURL: URLS.oidc },
    },
  ],
});
