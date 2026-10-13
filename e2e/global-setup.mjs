// Global setup for the Backyard end-to-end tests:
//
//  1. builds ./cmd/backyard (or uses $BACKYARD_BIN);
//  2. starts a fake OpenAI-compatible upstream and `backyard serve --dev` on
//     PORTS.main (host mode on *.localhost, auth none, AI through the fake,
//     no warehouse) with a throwaway data directory;
//  3. deploys every example in examples/ and two `backyard init` sites
//     (templates blank and app) with the CLI: `backyard deploy --site <name>`;
//  4. starts a mock OpenID Connect provider and a second server in
//     auth.mode oidc (path mode on 127.0.0.1, no AI) and deploys the same
//     sites there with a static deploy token.
//
// The returned function stops everything and deletes the temporary directory.
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

import { BackyardServer, backyardCLI, buildBackyard, deploySite } from "./harness/backyard.mjs";
import { devServerConfig, oidcServerConfig } from "./harness/configs.mjs";
import { PORTS, URLS, publishState } from "./harness/env.mjs";
import { startFakeAI } from "./harness/fake-ai.mjs";
import { startMockIdP } from "./harness/mock-idp.mjs";
import { exampleSites } from "./harness/scratch.mjs";

export default async function globalSetup() {
  const tmp = mkdtempSync(join(tmpdir(), "backyard-e2e-"));
  const cleanups = [];
  const cleanup = async () => {
    for (const fn of cleanups.reverse()) {
      try {
        await fn();
      } catch (err) {
        console.error("e2e teardown:", err);
      }
    }
    cleanups.length = 0;
    rmSync(tmp, { recursive: true, force: true });
  };

  try {
    const bin = buildBackyard(tmp);
    const cliHome = join(tmp, "cli-home");

    // --- main server: dev mode, guests, fake AI --------------------------------
    const fakeAI = await startFakeAI();
    cleanups.push(() => fakeAI.close());
    const adminToken = "e2e-admin-" + randomBytes(16).toString("hex");
    const mainDir = join(tmp, "main");
    const main = new BackyardServer({
      bin,
      name: "main",
      port: PORTS.main,
      dir: mainDir,
      url: URLS.main,
      config: devServerConfig({ dir: mainDir, port: PORTS.main, name: "Backyard E2E", adminToken, fakeAI }),
    });
    await main.start();
    cleanups.push(() => main.stop());

    const sites = {};
    for (const [site, dir] of Object.entries(exampleSites())) {
      sites[site] = deploySite(bin, { dir, site, server: main.url, home: cliHome }).url;
    }
    for (const [site, template] of [
      ["tmpl-blank", "blank"],
      ["tmpl-app", "app"],
    ]) {
      const dir = join(tmp, "init", site);
      backyardCLI(bin, ["init", site, "--dir", dir, "--template", template, "--server", main.url], { home: cliHome });
      sites[site] = deploySite(bin, { dir, site, server: main.url, home: cliHome }).url;
    }

    // --- OIDC server: mock IdP, path mode on 127.0.0.1 -------------------------
    const clientId = "backyard-e2e";
    const clientSecret = "idp-secret-" + randomBytes(8).toString("hex");
    const idp = await startMockIdP({
      port: PORTS.idp,
      clientId,
      clientSecret,
      redirectURIs: [`${URLS.oidc}/__backyard/auth/callback`],
      postLogoutRedirectURIs: [URLS.oidc, `${URLS.oidc}/`],
    });
    cleanups.push(() => idp.close());
    const oidcToken = "e2e-deploy-" + randomBytes(16).toString("hex");
    const oidcDir = join(tmp, "oidc");
    const oidc = new BackyardServer({
      bin,
      name: "oidc",
      port: PORTS.oidc,
      dir: oidcDir,
      url: URLS.oidc,
      dev: false,
      config: oidcServerConfig({
        dir: oidcDir,
        port: PORTS.oidc,
        publicURL: URLS.oidc,
        issuer: idp.issuer,
        clientId,
        clientSecret,
        deployToken: oidcToken,
      }),
    });
    await oidc.start();
    cleanups.push(() => oidc.stop());
    // The init sites under other names, and every example (path mode, signed-in people).
    const oidcSites = {};
    for (const [site, dir] of Object.entries({
      hello: join(tmp, "init", "tmpl-blank"),
      "team-app": join(tmp, "init", "tmpl-app"),
      ...exampleSites(),
    })) {
      oidcSites[site] = deploySite(bin, { dir, site, server: oidc.url, token: oidcToken, home: cliHome }).url;
    }

    publishState({
      tmp,
      bin,
      cliHome,
      adminToken,
      fakeAI: { origin: fakeAI.origin },
      idp: { issuer: idp.issuer, clientId },
      oidcToken,
      sites,
      oidcSites,
      logs: { main: main.logFile, oidc: oidc.logFile },
    });
  } catch (err) {
    await cleanup();
    throw err;
  }
  return cleanup;
}
