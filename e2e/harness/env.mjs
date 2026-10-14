// Ports and the state global-setup hands to the tests.

const base = Number(process.env.BACKYARD_E2E_PORT || 18500);
if (!Number.isInteger(base) || base < 1024 || base > 65000) throw new Error(`BACKYARD_E2E_PORT=${process.env.BACKYARD_E2E_PORT} is not a usable port`);

/** Fixed ports (browsers need stable origins; BACKYARD_E2E_PORT moves them all). */
export const PORTS = {
  /** `backyard serve --dev`, host mode on *.localhost, auth none, fake AI. */
  main: base,
  /** auth.mode oidc against the mock IdP, path mode on 127.0.0.1. */
  oidc: base + 1,
  /** A server the reconnect test kills and restarts. */
  reconnect: base + 2,
  /** The mock OpenID Connect provider (issuer http://localhost:<port>). */
  idp: base + 3,
  /** A copy of the sites for the phone-layout checks (they would disturb presence counts on main). */
  phone: base + 4,
  /** A server with a Postgres warehouse (only with BACKYARD_TEST_POSTGRES_DSN). */
  warehouse: base + 5,
  /** A copy of the sites for the accessibility checks (they fill the sites with data). */
  a11y: base + 6,
  /** An auth.mode oidc server with the Claude connector on (mcp.enabled), path mode on 127.0.0.1. */
  mcp: base + 7,
  /** The mock IdP of the connector server (issuer http://localhost:<port>). */
  mcpIdP: base + 8,
};

/** Hub URLs. */
export const URLS = {
  main: `http://localhost:${PORTS.main}`,
  oidc: `http://127.0.0.1:${PORTS.oidc}`,
  reconnect: `http://localhost:${PORTS.reconnect}`,
};

const STATE_VAR = "BACKYARD_E2E_STATE";

/** Publishes the setup state to the test workers (they inherit the runner's environment). */
export function publishState(state) {
  process.env[STATE_VAR] = JSON.stringify(state);
}

/**
 * The state written by global-setup.mjs:
 * {tmp, bin, cliHome, adminToken, fakeAI: {origin}, idp: {issuer, clientId},
 *  oidcToken, sites: {name: url}, oidcSites: {name: url}, logs}
 */
export function readState() {
  const raw = process.env[STATE_VAR];
  if (!raw) throw new Error(`${STATE_VAR} is not set: run the tests with "npm test" (playwright test) so global-setup.mjs runs first`);
  return JSON.parse(raw);
}
