// Dedicated `backyard serve --dev` instances for spec files that must not
// share a server with the others (they restart it, change its config, or
// would disturb the presence counts other specs assert on).
import { existsSync, readdirSync } from "node:fs";
import { join } from "node:path";

import { BackyardServer, REPO, deploySite } from "./backyard.mjs";
import { devServerConfig } from "./configs.mjs";
import { readState } from "./env.mjs";

/** examples/<name>/ folders with an index.html, as {site: dir}. */
export function exampleSites() {
  const sites = {};
  const root = join(REPO, "examples");
  for (const d of readdirSync(root, { withFileTypes: true })) {
    if (d.isDirectory() && existsSync(join(root, d.name, "index.html"))) sites[d.name] = join(root, d.name);
  }
  return sites;
}

/** The sites global-setup deploys to the main server: the examples plus two `backyard init` sites. */
export const STANDARD_SITE_NAMES = [...Object.keys(exampleSites()), "tmpl-blank", "tmpl-app"];

/** {site: dir} for STANDARD_SITE_NAMES (needs the setup state, so call it in hooks or tests). */
export function standardSites(state = readState()) {
  return {
    ...exampleSites(),
    "tmpl-blank": join(state.tmp, "init", "tmpl-blank"),
    "tmpl-app": join(state.tmp, "init", "tmpl-app"),
  };
}

/**
 * Starts a dev server on 127.0.0.1:port (host mode on *.localhost) with its
 * own data directory below the run's temp directory and deploys sites
 * ({name: dir}) to it. With ai, its AI goes to the run's fake upstream;
 * config(base) may adjust the configuration.
 * @returns {Promise<{server: BackyardServer, sites: Record<string, string>}>} sites maps names to URLs
 */
export async function startScratchServer({ name, port, sites = {}, ai = false, config = (c) => c }) {
  const state = readState();
  const fakeAI = ai ? { url: `${state.fakeAI.origin}/v1` } : null;
  const dir = join(state.tmp, `${name}-${Date.now().toString(36)}`);
  const server = new BackyardServer({
    bin: state.bin,
    name,
    port,
    dir,
    url: `http://localhost:${port}`,
    config: config(devServerConfig({ dir, port, name: `Backyard ${name}`, adminToken: state.adminToken, fakeAI })),
  });
  await server.start();
  const urls = {};
  try {
    for (const [site, siteDir] of Object.entries(sites)) {
      urls[site] = deploySite(state.bin, { dir: siteDir, site, server: server.url, home: join(dir, "cli") }).url;
    }
  } catch (err) {
    await server.stop();
    throw err;
  }
  return { server, sites: urls };
}
