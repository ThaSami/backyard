// What `access enable` creates in Cloudflare Access (or prints for the
// dashboard): the names of the applications and policies of a Worker and the
// paths the Worker authenticates itself, which the bypass application lets
// through (CLI deploys and tokens use a deploy token, the health checks need
// none, and the last four are the Claude connector's machine paths: its MCP
// endpoint and OAuth endpoints check their own tokens, SPEC §8.4). The same
// nine paths as the Terraform module's bypass_paths default and SPEC §16
// (docs/contract_hardening_test.go and docs/contract_test.go compare them).
//
//   accessNames(w)            names of the four objects ("Backyard (<w>)…")
//   policyBodies(o)           request bodies of the allow and bypass policies
//   appBodies(o)              request bodies of the hub and the bypass applications
//   guidedSteps(o)            the dashboard steps, for people without an API token
//   describeAllow(e, d)       "Emails ending in @acme.com, Emails a@acme.com"
//   CONSUMER_MAIL_DOMAINS     mail domains anyone can get an address at

export const ACCESS_BYPASS_PATHS = ["/__backyard/api/deploy", "/__backyard/api/deploy/*", "/__backyard/api/tokens", "/__backyard/healthz", "/__backyard/readyz", "/__backyard/mcp", "/__backyard/oauth/*", "/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource/__backyard/mcp"];

/**
 * Names of the Access objects of a Worker. They intentionally differ from the
 * Terraform module's names ("Backyard…"), so the tool never adopts objects
 * Terraform owns.
 * @param {string} worker
 */
export function accessNames(worker) {
  const app = `Backyard (${worker})`;
  return {
    app,
    bypassApp: `${app} machine endpoints`,
    allowPolicy: `${app} users`,
    bypassPolicy: `${app} machine endpoints`,
  };
}

/**
 * Mail domains anyone can get an address at: allowing one lets the whole
 * world sign in, so `access enable` asks first.
 */
export const CONSUMER_MAIL_DOMAINS = [
  "gmail.com",
  "googlemail.com",
  "outlook.com",
  "hotmail.com",
  "live.com",
  "yahoo.com",
  "icloud.com",
  "me.com",
  "aol.com",
  "proton.me",
  "protonmail.com",
  "gmx.com",
  "gmx.de",
];

/** Whether anyone can get an address at this mail domain (CONSUMER_MAIL_DOMAINS). */
export function isConsumerMailDomain(domain) {
  return CONSUMER_MAIL_DOMAINS.includes(String(domain ?? "").trim().toLowerCase().replace(/^@/, ""));
}

/**
 * The include rules of the allow policy: every email, then every email domain.
 * @param {string[]} [emails]
 * @param {string[]} [domains]
 */
export function allowInclude(emails = [], domains = []) {
  return [...emails.map((email) => ({ email: { email } })), ...domains.map((domain) => ({ email_domain: { domain } }))];
}

/**
 * The bodies of POST/PUT /accounts/{a}/access/policies[/{id}]: who may sign in
 * (allow) and the machine endpoints everyone reaches (bypass; the Worker
 * checks the deploy token there itself).
 * @param {{worker: string, emails?: string[], domains?: string[]}} o
 */
export function policyBodies(o) {
  const n = accessNames(o.worker);
  return {
    allow: { name: n.allowPolicy, decision: "allow", include: allowInclude(o.emails, o.domains), session_duration: "24h" },
    bypass: { name: n.bypassPolicy, decision: "bypass", include: [{ everyone: {} }] },
  };
}

/**
 * The bodies of POST/PUT /accounts/{a}/access/apps[/{id}]: the hub
 * application (the hostname, and *.<host> in host mode) and the bypass
 * application for ACCESS_BYPASS_PATHS, which is created first so CLI deploys
 * are never blocked.
 * @param {{worker: string, host: string, mode?: "path"|"host", allowPolicyId: string, bypassPolicyId: string}} o
 */
export function appBodies(o) {
  const n = accessNames(o.worker);
  const destinations = [{ type: "public", uri: o.host }];
  if (o.mode === "host") destinations.push({ type: "public", uri: `*.${o.host}` });
  return {
    bypass: {
      name: n.bypassApp,
      type: "self_hosted",
      domain: `${o.host}/__backyard/api/deploy`,
      destinations: ACCESS_BYPASS_PATHS.map((p) => ({ type: "public", uri: o.host + p })),
      app_launcher_visible: false,
      policies: [{ id: o.bypassPolicyId, precedence: 1 }],
    },
    main: {
      name: n.app,
      type: "self_hosted",
      domain: o.host,
      destinations,
      session_duration: "24h",
      app_launcher_visible: true,
      http_only_cookie_attribute: true,
      same_site_cookie_attribute: "lax",
      policies: [{ id: o.allowPolicyId, precedence: 1 }],
    },
  };
}

/**
 * Who may sign in, in the words of the dashboard's policy editor:
 * "Emails ending in @acme.com, Emails a@acme.com" ("" when nobody is listed).
 * @param {string[]} [emails]
 * @param {string[]} [domains]
 */
export function describeAllow(emails = [], domains = []) {
  const parts = [];
  if (domains.length) parts.push(`Emails ending in ${domains.map((d) => `@${d}`).join(", ")}`);
  if (emails.length) parts.push(`Emails ${emails.join(", ")}`);
  return parts.join(", ");
}

/** The account permissions an API token needs for `access enable` (in words). */
export const ACCESS_TOKEN_PERMISSIONS = "Account · Access: Apps and Policies · Edit and Account · Access: Organizations, Identity Providers, and Groups · Read";

/**
 * The dashboard steps for the two applications, with the exact values (the
 * guided mode of `access enable`).
 * @param {{worker: string, host: string, mode?: "path"|"host", emails?: string[], domains?: string[], invokedAs?: string, rerun?: string}} o
 *   rerun: the arguments of the automatic alternative (default: the allow flags)
 * @returns {string}
 */
export function guidedSteps(o) {
  const n = accessNames(o.worker);
  const inv = o.invokedAs ?? "backyard-cloudflare";
  const pad = (label) => label.padEnd(19);
  const cont = " ".repeat(19);
  const who = describeAllow(o.emails, o.domains) || "<who may sign in>";
  const allowFlags = [...(o.emails ?? []).map((e) => `--allow-email ${e}`), ...(o.domains ?? []).map((d) => `--allow-email-domain ${d}`)].join(" ");
  const rerun = o.rerun ?? (allowFlags || "--allow-email-domain <your-domain>");
  const hub =
    o.mode === "host"
      ? [`     ${pad("Public hostnames:")}${o.host}`, `     ${cont}*.${o.host}`]
      : [`     ${pad("Public hostname:")}${o.host}`];
  const machine = ACCESS_BYPASS_PATHS.map((p, i) => `     ${i === 0 ? pad("Public hostnames:") : cont}${o.host}${p}`);
  return [
    "Cloudflare Access needs two self-hosted applications",
    "(https://one.dash.cloudflare.com/ → Access → Applications → Add an application → Self-hosted):",
    "",
    `  1. ${n.app}`,
    ...hub,
    `     ${pad("Policy:")}Allow · Include · ${who}`,
    `  2. ${n.bypassApp}   (paths the Worker authenticates itself)`,
    ...machine,
    `     ${pad("Policy:")}Bypass · Include · Everyone`,
    "",
    "  First time? Open https://one.dash.cloudflare.com/ once, pick a team name and the Free plan.",
    "  Automatic instead: create an API token with Account · Access: Apps and Policies · Edit and",
    "  Account · Access: Organizations, Identity Providers, and Groups · Read, then run",
    `  BACKYARD_CF_API_TOKEN=<token> ${inv} access enable ${rerun}`,
    "",
  ].join("\n");
}

/**
 * Prints guidedSteps(o).
 * @param {Parameters<typeof guidedSteps>[0]} o
 * @param {(s: string) => void} [log]
 */
export function printGuidedSteps(o, log = (s) => console.log(s)) {
  log(guidedSteps(o));
}
