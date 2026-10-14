#!/usr/bin/env node
// backyard-cloudflare: Backyard on your own Cloudflare account (Workers free
// plan) — setup, custom domain and Cloudflare Access without Terraform.
//
//   npx backyard-cloudflare setup
//   npx backyard-cloudflare domain add backyard.example.com
//   npx backyard-cloudflare access enable --allow-email-domain example.com
//   npx backyard-cloudflare status
//
// In a checkout: `npm run setup` or `npm run cf -- <command>`. The commands
// live in ../scripts (cli.mjs dispatches); `--help` lists them.

const major = Number(String(process.versions.node).split(".")[0]);
if (!(major >= 22)) {
  console.error(`backyard-cloudflare needs Node.js 22 or later (found v${process.versions.node}); https://nodejs.org/`);
  process.exit(2);
}

// wrangler uses HTTPS_PROXY; the engine's own fetch calls only with NODE_USE_ENV_PROXY=1.
const { proxyHint } = await import("../scripts/lib/net.mjs");
const hint = proxyHint();
if (hint) console.error(`! ${hint}`);

const { main } = await import("../scripts/cli.mjs");
process.exitCode = await main(process.argv.slice(2));
