// The Claude connector (SPEC §8.4, §14.1) end to end: a real MCP client
// (@modelcontextprotocol/sdk, Streamable HTTP with an OAuthClientProvider)
// registers itself, the browser signs in at the mock IdP and approves on
// Backyard's consent page, and the client publishes a site. Then a second
// connection, Cancel, a sign-out before Allow, a person outside the allowed
// domains, and the hub card's Disconnect, after which the client is cut off
// at once.
//
// Runs on an oidc server of its own (mcp.enabled, path mode on 127.0.0.1)
// with a mock IdP of its own, so the shared oidc server keeps its
// configuration. The client's loopback redirect URI is a listener of this
// file on an ephemeral port, as a local MCP client has (Playwright's
// page.route does not see the targets of redirects, and the code arrives
// through the consent POST's 303).
import { randomBytes } from "node:crypto";
import { createServer } from "node:http";
import { join } from "node:path";

import { UnauthorizedError } from "@modelcontextprotocol/sdk/client/auth.js";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

import { BackyardServer } from "../../harness/backyard.mjs";
import { oidcServerConfig } from "../../harness/configs.mjs";
import { PORTS, readState } from "../../harness/env.mjs";
import { IDP_USERS, startMockIdP } from "../../harness/mock-idp.mjs";
import { expect, expectAccessible, rand, test } from "../fixtures.mjs";

/** This file's server and IdP (PORTS.mcp and PORTS.mcpIdP of harness/env.mjs). */
const SERVER_PORT = PORTS.mcp;
const IDP_PORT = PORTS.mcpIdP;
const HUB = `http://127.0.0.1:${SERVER_PORT}`;
const MCP_URL = `${HUB}/__backyard/mcp`;
/** The client's loopback redirect URI: http://127.0.0.1:<ephemeral port>/callback (set by beforeAll). */
let CALLBACK = "";
/** The consent page's URL (the authorization endpoint with a query). */
const AUTHORIZE_URL = new RegExp(`^${HUB.replace(/[.]/g, "\\.")}/__backyard/auth/authorize\\?`);
const TOOLS = ["backyard_docs", "backyard_whoami", "backyard_list_sites", "backyard_site_files", "backyard_read_site_file", "backyard_publish"];

test.describe.configure({ mode: "serial" });

/** @type {BackyardServer} */
let server;
/** @type {{issuer: string, close: () => Promise<void>}} */
let idp;
/** The listener of CALLBACK. */
let callbackServer;
/** Resolvers waiting for the next request to CALLBACK's path. */
let callbackWaiters = [];

test.beforeAll(async () => {
  callbackServer = createServer((req, res) => {
    const url = new URL(req.url ?? "/", CALLBACK);
    if (url.pathname === new URL(CALLBACK).pathname) {
      const waiters = callbackWaiters;
      callbackWaiters = [];
      for (const resolve of waiters) resolve(url);
    }
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" });
    res.end('<!doctype html><html lang="en"><title>Connected</title><p>You can close this window.</p></html>');
  });
  await new Promise((resolve, reject) => {
    callbackServer.once("error", reject);
    callbackServer.listen(0, "127.0.0.1", resolve);
  });
  CALLBACK = `http://127.0.0.1:${callbackServer.address().port}/callback`;
  const state = readState();
  const clientId = "backyard-e2e-mcp";
  const clientSecret = "idp-secret-" + randomBytes(8).toString("hex");
  idp = await startMockIdP({
    port: IDP_PORT,
    clientId,
    clientSecret,
    redirectURIs: [`${HUB}/__backyard/auth/callback`],
    postLogoutRedirectURIs: [HUB, `${HUB}/`],
  });
  const dir = join(state.tmp, `mcp-${Date.now().toString(36)}`);
  server = new BackyardServer({
    bin: state.bin,
    name: "mcp",
    port: SERVER_PORT,
    dir,
    url: HUB,
    dev: false,
    config: oidcServerConfig({
      dir,
      port: SERVER_PORT,
      publicURL: HUB,
      issuer: idp.issuer,
      clientId,
      clientSecret,
      deployToken: "e2e-deploy-" + randomBytes(16).toString("hex"),
      mcp: true,
    }),
  });
  try {
    await server.start();
  } catch (err) {
    await idp.close();
    throw err;
  }
});

test.afterAll(async () => {
  await server?.stop();
  await idp?.close();
  await new Promise((resolve) => (callbackServer ? callbackServer.close(() => resolve()) : resolve()));
});

/**
 * An OAuthClientProvider for a client that registers itself (no client
 * metadata URL, so the SDK uses dynamic registration) and hands the
 * authorization URL to the test instead of opening a browser.
 */
class TestProvider {
  constructor(name) {
    this.name = name;
    this.client = undefined;
    this.saved = undefined;
    this.verifier = "";
    this.lastState = "";
    /** @type {URL[]} */
    this.authorizationURLs = [];
  }

  get redirectUrl() {
    return CALLBACK;
  }

  get clientMetadata() {
    return {
      client_name: this.name,
      redirect_uris: [CALLBACK],
      grant_types: ["authorization_code", "refresh_token"],
      response_types: ["code"],
      token_endpoint_auth_method: "none",
    };
  }

  state() {
    this.lastState = randomBytes(16).toString("hex");
    return this.lastState;
  }

  clientInformation() {
    return this.client;
  }

  saveClientInformation(info) {
    this.client = info;
  }

  tokens() {
    return this.saved;
  }

  saveTokens(tokens) {
    this.saved = tokens;
  }

  redirectToAuthorization(url) {
    this.authorizationURLs.push(new URL(url));
  }

  saveCodeVerifier(v) {
    this.verifier = v;
  }

  codeVerifier() {
    return this.verifier;
  }

  invalidateCredentials(scope) {
    if (scope === "all" || scope === "tokens") this.saved = undefined;
    if (scope === "all" || scope === "client") this.client = undefined;
    if (scope === "all" || scope === "verifier") this.verifier = "";
  }
}

/** A fetch that records {method, url, status} of every request the SDK makes. */
function recordingFetch(log) {
  return async (url, init) => {
    const res = await fetch(url, init);
    log.push({ method: (init?.method ?? "GET").toUpperCase(), url: String(url), status: res.status });
    return res;
  };
}

/** A Streamable HTTP transport and a client for provider (not connected). */
function mcpClient(provider, log) {
  const transport = new StreamableHTTPClientTransport(new URL(MCP_URL), { authProvider: provider, fetch: recordingFetch(log) });
  const client = new Client({ name: "backyard-e2e", version: "1.0.0" });
  return { client, transport };
}

/**
 * The first connection attempt: the server answers 401, the SDK discovers the
 * authorization server, registers and asks the provider to open the
 * authorization URL. Resolves to {transport, url}.
 */
async function startConnecting(provider, log) {
  const { client, transport } = mcpClient(provider, log);
  await expect(client.connect(transport)).rejects.toThrow(UnauthorizedError);
  expect(provider.authorizationURLs.length, "the SDK asked to open the authorization URL").toBeGreaterThan(0);
  return { transport, url: provider.authorizationURLs.at(-1) };
}

/** {hit}: a promise of the URL of the next request to the client's redirect URI. */
function interceptCallback() {
  const hit = new Promise((resolve) => callbackWaiters.push(resolve));
  return { hit };
}

/** A browser context (a person); with idpUser it already has a session at this file's IdP. */
async function person(newUser, idpUser) {
  const ctx = await newUser({ baseURL: HUB });
  if (idpUser) await ctx.addCookies([{ name: "idp_session", value: idpUser, url: idp.issuer, httpOnly: true, sameSite: "Lax" }]);
  return ctx;
}

/** Records the URLs of the main frame's navigation requests (redirect hops included). */
function recordNavigations(page) {
  const urls = [];
  page.on("request", (req) => {
    if (req.isNavigationRequest() && req.frame() === page.mainFrame()) urls.push(new URL(req.url()));
  });
  return urls;
}

/** State the tests of this file share (they run in order). */
const first = { provider: null, log: [], client: null, site: "" };

test("a client registers, the person signs in at the IdP and approves, and the client publishes", async ({ newUser }) => {
  first.provider = new TestProvider(`E2E connector ${rand()}`);
  const { provider, log } = first;
  const { transport, url } = await startConnecting(provider, log);

  // Discovery and registration happened without any browser.
  expect(log[0]).toMatchObject({ method: "POST", url: MCP_URL, status: 401 });
  expect(log).toContainEqual({ method: "GET", url: `${HUB}/.well-known/oauth-protected-resource/__backyard/mcp`, status: 200 });
  expect(log).toContainEqual({ method: "GET", url: `${HUB}/.well-known/oauth-authorization-server`, status: 200 });
  expect(log).toContainEqual({ method: "POST", url: `${HUB}/__backyard/oauth/register`, status: 201 });
  expect(provider.client.client_id).toMatch(/^bydc_/);
  expect(`${url.origin}${url.pathname}`).toBe(`${HUB}/__backyard/auth/authorize`);
  expect(url.searchParams.get("resource")).toBe(MCP_URL);
  expect(url.searchParams.get("code_challenge_method")).toBe("S256");
  expect(url.searchParams.get("redirect_uri")).toBe(CALLBACK);

  // The browser: no IdP session yet, so the IdP asks who is signing in.
  const page = await (await person(newUser)).newPage();
  const { hit: callback } = interceptCallback();
  await page.goto(url.href);
  await expect(page.locator("h1")).toHaveText("Mock IdP: choose an account");
  await page.getByRole("button", { name: `Sign in as ${IDP_USERS.alice.name}` }).click();

  // Backyard's consent page, after the sign-in.
  await expect(page).toHaveURL(AUTHORIZE_URL);
  await expect(page.locator("html")).toContainText("Connect an app to Backyard");
  const body = page.locator("body");
  await expect(body).toContainText("alice@example.com");
  await expect(body).toContainText(provider.name);
  await expect(body).toContainText(/not verified/);
  await expect(body).toContainText(/an app on this computer/);
  await expect(body).toContainText(new URL(CALLBACK).origin);
  await expect(page.locator("#allow")).toBeVisible(); // armed after a short delay
  await expectAccessible(page, "consent page");
  await page.locator("#allow").click();

  const back = await callback;
  expect(back.searchParams.get("code")).toMatch(/^byac_/);
  expect(back.searchParams.get("state")).toBe(provider.lastState);
  expect(back.searchParams.get("iss")).toBe(HUB);
  await transport.finishAuth(back.searchParams.get("code"));
  expect(provider.tokens().access_token).toMatch(/^byat_/);
  expect(provider.tokens().refresh_token).toMatch(/^byrt_/);

  // Connected: a fresh transport uses the tokens.
  const { client, transport: connected } = mcpClient(provider, log);
  await client.connect(connected);
  first.client = client;
  expect(client.getServerVersion()?.name).toBe("backyard");
  expect(connected.protocolVersion).toBe("2025-11-25");
  expect(client.getInstructions()).toContain("backyard_publish");
  expect((await client.listTools()).tools.map((t) => t.name)).toEqual(TOOLS);

  const who = JSON.parse((await client.callTool({ name: "backyard_whoami", arguments: {} })).content[0].text);
  expect(who.user).toMatchObject({ id: "alice@example.com", email: "alice@example.com", name: IDP_USERS.alice.name, anonymous: false });
  expect(who.can_publish).toBe(true);

  first.site = `mcp-${rand()}`;
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>From Claude</title></head><body><h1>Published by the connector ${first.site}</h1></body></html>\n`;
  const res = await client.callTool({ name: "backyard_publish", arguments: { site: first.site, files: [{ path: "index.html", content: html }] } });
  expect(res.isError ?? false, res.content[0].text).toBe(false);
  const published = JSON.parse(res.content[0].text);
  expect(published).toMatchObject({ site: first.site, url: `${HUB}/s/${first.site}/`, created: true, files: 1, uploaded: 1, warnings: [] });

  // The person sees the site (signed in, path mode).
  await page.goto(published.url);
  await expect(page.locator("h1")).toHaveText(`Published by the connector ${first.site}`);
});

test("connecting again while signed in shows the consent page again; Cancel answers access_denied", async ({ newUser }) => {
  const page = await (await person(newUser, "alice")).newPage();
  await page.goto("/");
  await expect(page.locator("#user-name")).toHaveText(IDP_USERS.alice.name); // signed in to Backyard

  const provider = new TestProvider(`E2E second ${rand()}`);
  const log = [];
  const { url } = await startConnecting(provider, log);
  const { hit: callback } = interceptCallback();
  const navigations = recordNavigations(page);
  await page.goto(url.href);
  await expect(page).toHaveURL(AUTHORIZE_URL);
  expect(navigations.some((u) => u.origin === idp.issuer), "no new sign-in at the IdP").toBe(false);
  await expect(page.locator("#allow")).toBeVisible(); // shown again: consent is never remembered
  await expect(page.locator("body")).toContainText(provider.name);
  await page.locator("#deny").click();

  const back = await callback;
  expect(back.searchParams.get("error")).toBe("access_denied");
  expect(back.searchParams.get("state")).toBe(provider.lastState);
  expect(back.searchParams.get("iss")).toBe(HUB);
  expect(back.searchParams.has("code")).toBe(false);
});

test("signed out before Allow: the approval offers a sign-in link that leads back to the consent page", async ({ newUser, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 401/); // the "sign-in ended" page itself
  const ctx = await person(newUser, "alice");
  const page = await ctx.newPage();
  const provider = new TestProvider(`E2E signed out ${rand()}`);
  const { url } = await startConnecting(provider, []);
  await page.goto(url.href);
  await expect(page).toHaveURL(AUTHORIZE_URL);
  await expect(page.locator("#allow")).toBeVisible();
  // The Backyard session ends (session_ttl, or a sign-out in another tab).
  await ctx.clearCookies({ name: "backyard_session" });
  await page.locator("#allow").click();
  // No redirect: the consent page's form-action would block the hop to the IdP.
  await expect(page.locator("h1")).toHaveText("Your Backyard sign-in ended");
  const { hit: callback } = interceptCallback();
  await page.getByRole("link", { name: "Sign in again" }).click();
  await expect(page).toHaveURL(AUTHORIZE_URL); // signed in again at the IdP, consent asked again
  await expect(page.locator("body")).toContainText(provider.name);
  await expect(page.locator("#allow")).toBeVisible();
  await page.locator("#allow").click();
  const back = await callback;
  expect(back.searchParams.get("code")).toMatch(/^byac_/);
  expect(back.searchParams.get("state")).toBe(provider.lastState);
});

test("a person outside the allowed domains gets Access denied and the client nothing", async ({ newUser, pageProblems }) => {
  pageProblems.allow(/the server responded with a status of 403/); // the "Access denied" page itself
  const page = await (await person(newUser, "mallory")).newPage();
  let reached = false;
  interceptCallback().hit.then(() => (reached = true));
  const provider = new TestProvider(`E2E mallory ${rand()}`);
  const { url } = await startConnecting(provider, []);
  await page.goto(url.href);
  await expect(page.locator("h1")).toHaveText("Access denied");
  await expect(page.locator("body")).toContainText(IDP_USERS.mallory.email);
  await expect(page.locator("#allow")).toHaveCount(0);
  expect(new URL(page.url()).origin).toBe(HUB);
  expect(reached, "nothing was sent to the client").toBe(false);
});

test("the hub card shows the connector URL and the connection; Disconnect cuts the client off", async ({ newUser }) => {
  test.skip(!first.client, "needs the connection of the first test");
  const page = await (await person(newUser, "alice")).newPage();
  page.on("dialog", (d) => d.accept());
  await page.goto("/");
  const card = page.locator("#connector");
  await expect(card).toBeVisible();
  await expect(card.locator("#connector-url")).toHaveText(MCP_URL);
  await expect(card.locator("#connector-copy")).toBeVisible();
  await expect(card).toContainText(`claude mcp add --transport http backyard ${MCP_URL}`);
  await expect(card.locator("#connector-revoke-user")).toBeVisible(); // alice is an admin (backyard-admins)

  const grants = (await (await page.request.get(`${HUB}/__backyard/api/mcp/grants`)).json()).grants;
  const grant = grants.find((g) => g.client_name === first.provider.name);
  expect(grant, `the first connection is listed: ${JSON.stringify(grants)}`).toBeTruthy();
  const item = page.locator("#connector-grants li", { has: page.locator(`button.connector-revoke[data-id="${grant.id}"]`) });
  await expect(item).toContainText(first.provider.name);
  await expect(item).toContainText("unverified");
  await expectAccessible(page, "hub with the connector card");

  // The connection works until Disconnect.
  expect((await first.client.callTool({ name: "backyard_whoami", arguments: {} })).isError ?? false).toBe(false);
  await item.locator("button.connector-revoke").click();
  await expect(item).toHaveCount(0);

  const mark = first.log.length;
  const before = first.provider.authorizationURLs.length;
  await expect(first.client.callTool({ name: "backyard_whoami", arguments: {} })).rejects.toThrow(UnauthorizedError);
  const after = first.log.slice(mark);
  expect(after[0], "the next call is refused").toMatchObject({ method: "POST", url: MCP_URL, status: 401 });
  expect(after, "the refresh token is dead too").toContainEqual({ method: "POST", url: `${HUB}/__backyard/oauth/token`, status: 400 });
  expect(first.provider.authorizationURLs.length, "the client has to ask the person again").toBe(before + 1);
  await first.client.close();
});
