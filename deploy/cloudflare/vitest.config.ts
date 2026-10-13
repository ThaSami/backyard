import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// Tests run inside workerd (Miniflare) with the bindings of wrangler.jsonc.
// Remote bindings are disabled: Workers AI is replaced by a mock in the tests
// that need it, and R2 is a local Miniflare bucket.
export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.jsonc" },
      remoteBindings: false,
      miniflare: {
        r2Buckets: ["R2"],
        bindings: {
          BACKYARD_SECRET: "test-secret-0123456789-abcdefghijklmnopqrstuvwxyz",
          DEPLOY_TOKEN: "test-deploy-token",
          FILE_INLINE_MAX: "65536",
          // Generous caps for the suite; guardrail tests pass their own LIMITS.
          LIMITS: JSON.stringify({ max_sites: 100000, deploys_per_day: 100000, uploads_per_day: 100000, ai_per_day: 100000, ai_platform_per_day: 100000 }),
        },
      },
    }),
  ],
  test: {
    include: ["test/**/*.test.ts"],
    testTimeout: 30_000,
  },
});
