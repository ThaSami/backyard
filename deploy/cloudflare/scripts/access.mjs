// `backyard-cloudflare access enable` and `access disable` (design §3.4 and
// §3.5): company login with Cloudflare Access in front of the Worker's own
// hostname (a custom domain from `domain add`; Access cannot protect
// workers.dev, where the Worker itself answers 401 once AUTH_MODE=access).
//
// access enable:
//   1. Preconditions: an active domain managed by `domain add`, no --var
//      AUTH_MODE/ACCESS_* (externally managed), the hub answering.
//   2. Who may sign in: --allow-email / --allow-email-domain, else remembered,
//      else asked (consumer mail domains need a confirmation).
//   3. One of three modes:
//      * API (credential A: BACKYARD_CF_API_TOKEN, CLOUDFLARE_API_TOKEN, or a
//        pasted token): creates or updates the allow and bypass policies, the
//        bypass application and the hub application (in that order, saving
//        each new id at once), named "Backyard (<w>)…". An application of
//        anyone else that covers the host is refused, never touched.
//      * guided: prints the dashboard steps, then (interactive) watches the
//        edge until Access shows up and asks before using the team and AUD it
//        found there.
//      * --team/--aud: verifies applications made elsewhere.
//   4. Edge verification BEFORE the Worker changes: the team's signing keys,
//      the login redirect (team and AUD), and the nine bypass paths reaching
//      the Worker (the machine endpoints and the Claude connector's four
//      paths). A failure exits 4 with the Worker unchanged.
//   5. Deploy with AUTH_MODE=access (the auth guard passes: it strengthens
//      auth), then post-checks: /info says access, workers.dev answers 401,
//      the deploy token still works.
//
// access disable: deploys AUTH_MODE=none first (the guard is bypassed on
// purpose), then, with --delete-apps, deletes what this tool created.
//
// State: state.access = {team, aud, source: "api"|"manual"|"adopted",
// appId, bypassAppId, allowPolicyId, bypassPolicyId, allow: {emails,
// emailDomains}} while Access is on; every deploy derives AUTH_MODE=access
// from it, so it is written only after the edge checks pass.
// state.accessCreated = {host, appId?, bypassAppId?, allowPolicyId?,
// bypassPolicyId?} holds the ids of Access objects this tool created that
// state.access does not use: not yet (an enable that stopped before the
// deploy; a re-run reuses them instead of creating duplicates) or no longer
// (after `access disable` without --delete-apps; `--delete-apps` later
// deletes them). The admin token is never stored, printed or given to wrangler.

import { CfError } from "./lib/cfapi.mjs";
import { ACCESS_BYPASS_PATHS, ACCESS_TOKEN_PERMISSIONS, accessNames, appBodies, isConsumerMailDomain, policyBodies, printGuidedSteps } from "./lib/access-plan.mjs";
import { MANAGED_ACCESS_KEYS, normalizeTeam } from "./lib/live.mjs";
import { parseAccessRedirect, probe, randomProbeLabel, waitFor } from "./lib/net.mjs";
import { CliError, EXIT, bold, dim, ui } from "./lib/ui.mjs";

const AUD_RE = /^[0-9a-f]{64}$/i;
const EMAIL_RE = /^[^@\s]+@[^@\s]+\.[^@\s]+$/;
const DOMAIN_RE = /^(?=.{1,253}$)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/;
const TEAM_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/;
/** The ids of the Access objects this tool creates, in deletion order. */
const ID_KEYS = ["appId", "bypassAppId", "allowPolicyId", "bypassPolicyId"];

/** Default --wait of the edge checks, and while waiting for dashboard steps. */
const VERIFY_WAIT = 120;
const DISCOVERY_WAIT = 900;
const POST_CHECK_WAIT = 60;

// ---------------------------------------------------------------------------
// Inputs

/** --team as the Worker reads it ("acme" → "acme.cloudflareaccess.com"); exit 2 when it is not a host. */
function checkTeam(v) {
  const t = normalizeTeam(v);
  if (!TEAM_RE.test(t)) throw new CliError(EXIT.USAGE, `--team expects a Zero Trust team ("acme" or "acme.cloudflareaccess.com"), got ${JSON.stringify(String(v))}`);
  return t;
}

/** --aud: 64 hex characters (stored lowercase, as Access puts it in the JWT). */
function checkAud(v, what = "--aud") {
  const a = String(v ?? "").trim();
  if (!AUD_RE.test(a)) throw new CliError(EXIT.USAGE, `${what} expects the 64-character Application Audience (AUD) tag, got ${JSON.stringify(a)}`);
  return a.toLowerCase();
}

/**
 * Validates and normalizes an allow list (lowercased, domains without "@",
 * duplicates dropped). Throws CliError(2) naming the first bad entry.
 * @param {string[]} emails
 * @param {string[]} domains
 * @returns {{emails: string[], domains: string[]}}
 */
export function normalizeAllow(emails = [], domains = []) {
  const e = [];
  const d = [];
  for (const raw of emails) {
    const v = String(raw ?? "").trim().toLowerCase();
    if (!EMAIL_RE.test(v)) throw new CliError(EXIT.USAGE, `--allow-email expects an email address, got ${JSON.stringify(String(raw))}`);
    if (!e.includes(v)) e.push(v);
  }
  for (const raw of domains) {
    const v = String(raw ?? "").trim().toLowerCase().replace(/^@/, "").replace(/\.$/, "");
    if (!DOMAIN_RE.test(v)) throw new CliError(EXIT.USAGE, `--allow-email-domain expects a mail domain (acme.com or @acme.com), got ${JSON.stringify(String(raw))}`);
    if (!d.includes(v)) d.push(v);
  }
  return { emails: e, domains: d };
}

/**
 * Parses an answer to "Who may sign in?": emails and domains (with or
 * without "@"), separated by commas or spaces.
 * @param {string} answer
 */
export function parseWho(answer) {
  const emails = [];
  const domains = [];
  for (const item of String(answer ?? "").split(/[\s,;]+/).filter(Boolean)) {
    if (item.startsWith("@") || !item.includes("@")) domains.push(item);
    else emails.push(item);
  }
  return normalizeAllow(emails, domains);
}

const isEmpty = (a) => !a || (a.emails.length === 0 && a.domains.length === 0);

/** Whether two lists hold the same items (in any order). */
function sameItems(a, b) {
  const x = [...a].sort();
  const y = [...b].sort();
  return x.length === y.length && x.every((v, i) => v === y[i]);
}

/** The keys of `keys` that state.vars sets (externally managed). */
function externalKeys(state, keys) {
  return keys.filter((k) => state.vars && Object.hasOwn(state.vars, k));
}

/** A short description of a failed probe. */
function failText(r) {
  const what = r.kind === "certificate" ? "TLS certificate error" : r.kind === "dns" ? "the name does not resolve" : "network error";
  return `${what}${r.error ? `: ${r.error}` : ""}`;
}

const isJSON = (r) => /\bjson\b/i.test(r.contentType ?? "");

/** Whether a response is Cloudflare Access stopping the request (login redirect or its deny page). */
function accessBlocked(r) {
  if (!r.ok) return false;
  if (r.status >= 300 && r.status < 400 && parseAccessRedirect(r.location)) return true;
  return r.status === 403 && !isJSON(r);
}

/** Prints a progress line now and then while waiting (every 30 s at most). */
function progress(label) {
  let last = -Infinity;
  return (r, elapsed) => {
    if (elapsed - last < 30) return;
    last = elapsed;
    const why = r?.failures?.[0] ?? r?.why ?? "";
    ui.note(`  … ${label} (${elapsed} s)${why ? `: ${why}` : ""}`);
  };
}

// ---------------------------------------------------------------------------
// Edge checks (shared with status)

/**
 * The edge checks of Cloudflare Access in front of the hub (design §3.4 step
 * 6), without credentials: (a) the team's signing keys, (b) the login redirect
 * at https://<host>/ (team, and AUD when the redirect carries it), (c) in host
 * mode the same at a random https://byd-probe-….<host>/, (d) the machine
 * endpoints of the bypass application reaching the Worker, (e) the Claude
 * connector's four paths of it reaching the Worker (SPEC §8.4; they pass
 * whether or not MCP_ENABLED is on, and on a Worker that predates the
 * connector). Returns the failures of each group (empty = ok), and the
 * destinations of (d) and (e) that Access blocks (missing from the bypass
 * application).
 * @param {{host: string, mode?: string, team: string, aud: string, worker: string}} o
 * @returns {Promise<{certs: string[], redirect: string[], wildcard: string[], bypass: string[], connector: string[], missing: string[]}>}
 */
export async function accessEdgeFailures(o) {
  const { host, team } = o;
  const aud = String(o.aud ?? "").toLowerCase();
  const n = accessNames(o.worker);

  const certs = async () => {
    const url = `https://${team}/cdn-cgi/access/certs`;
    const r = await probe(url, { accept: "application/json" });
    if (!r.ok) return [`the Access team ${team} does not answer (${failText(r)}): check the team name`];
    if (r.status !== 200) return [`the Access team ${team} answers HTTP ${r.status} at ${url}: check the team name`];
    if (!Array.isArray(r.json?.keys) || r.json.keys.length === 0) return [`the Access team ${team} has no signing keys yet (${url})`];
    return [];
  };

  const login = async (h, label) => {
    const r = await probe(`https://${h}/`, { accept: "text/html" });
    if (!r.ok) return [`https://${label}/ does not answer (${failText(r)})`];
    if (r.status >= 300 && r.status < 400) {
      const p = parseAccessRedirect(r.location);
      if (!p) return [`https://${label}/ redirects elsewhere (${safeOrigin(r.location)}), not to a Cloudflare Access login`];
      if (p.team !== team) return [`Access in front of ${label} belongs to team ${p.team}, not ${team}`];
      if (p.kid && p.kid !== aud) return [`Access in front of ${label} is the application with AUD ${p.kid}, not ${aud} (pass --aud ${p.kid} if that is the right one)`];
      return [];
    }
    if (r.status === 403 && !isJSON(r)) return [`Cloudflare Access denies everyone at https://${label}/: the application ${n.app} needs an Allow policy`];
    return [`https://${label}/ is not behind Cloudflare Access yet (HTTP ${r.status}): the application ${n.app} for ${label} is missing`];
  };

  const BYPASS = [
    { path: "/__backyard/healthz", rule: "/__backyard/healthz", want: "200", ok: (r) => r.status === 200 },
    { path: "/__backyard/readyz", rule: "/__backyard/readyz", want: "200 or 503", ok: (r) => r.status === 200 || r.status === 503 },
    { path: "/__backyard/api/deploy/info", rule: "/__backyard/api/deploy/*", want: "the Worker's JSON", ok: (r) => r.status === 200 && r.json?.edition === "cloudflare" },
    // Without a token the Worker answers 401 in AUTH_MODE=access, and the guest in
    // AUTH_MODE=none (which it still runs before the first enable).
    {
      path: "/__backyard/api/deploy/me",
      rule: "/__backyard/api/deploy/*",
      want: "the Worker's JSON answer (401, or the guest in AUTH_MODE=none)",
      ok: (r) => isJSON(r) && (r.status === 401 || (r.status === 200 && r.json?.anonymous === true)),
    },
    { path: "/__backyard/api/tokens", rule: "/__backyard/api/tokens", want: "a JSON answer of the Worker", ok: (r) => r.status >= 400 && r.status < 500 && isJSON(r) },
  ];
  // The Claude connector's paths (SPEC §8.4). The Worker answers them itself:
  // 401/405/200 with MCP_ENABLED, its JSON 404 without; a Worker older than
  // the connector answers its hub page (401 in AUTH_MODE=access, else 404) at
  // the discovery documents.
  const discovery = (r) => [200, 401, 404].includes(r.status);
  const CONNECTOR = [
    { path: "/__backyard/mcp", rule: "/__backyard/mcp", want: "the Worker's JSON 401, 404 or 405", ok: (r) => isJSON(r) && [401, 404, 405].includes(r.status) },
    { path: "/__backyard/oauth/token", rule: "/__backyard/oauth/*", want: "the Worker's JSON 404 or 405", ok: (r) => isJSON(r) && [404, 405].includes(r.status) },
    { path: "/.well-known/oauth-authorization-server", rule: "/.well-known/oauth-authorization-server", want: "200, 401 or 404", ok: discovery },
    { path: "/.well-known/oauth-protected-resource/__backyard/mcp", rule: "/.well-known/oauth-protected-resource/__backyard/mcp", want: "200, 401 or 404", ok: discovery },
  ];
  const missing = [];
  const probeAll = async (list) => {
    const results = await Promise.all(list.map((b) => probe(`https://${host}${b.path}`, { accept: "application/json" })));
    const out = [];
    for (const [i, r] of results.entries()) {
      const b = list[i];
      let f = "";
      if (!r.ok) f = `https://${host}${b.path} does not answer (${failText(r)})`;
      else if (accessBlocked(r)) {
        f = `the bypass application is missing ${host}${b.rule}`;
        if (!missing.includes(host + b.rule)) missing.push(host + b.rule);
      } else if (!b.ok(r)) f = `https://${host}${b.path} answers HTTP ${r.status}, not ${b.want}`;
      if (f && !out.includes(f)) out.push(f);
    }
    return out;
  };

  const [c, red, wild, by, conn] = await Promise.all([
    certs(),
    login(host, host),
    o.mode === "host" ? login(`${randomProbeLabel()}.${host}`, `*.${host}`) : Promise.resolve([]),
    probeAll(BYPASS),
    probeAll(CONNECTOR),
  ]);
  return { certs: c, redirect: red, wildcard: wild, bypass: by, connector: conn, missing };
}

/** The origin of a URL, for messages ("" when it is not one). */
function safeOrigin(u) {
  try {
    return new URL(u).origin;
  } catch {
    return "an invalid location";
  }
}

/**
 * The checks after AUTH_MODE=access is deployed (design §3.4 step 8): /info
 * reports access, workers.dev answers 401 without a sign-in, the deploy token
 * still works (it reaches the Worker through the bypass application).
 * @param {{host: string, workersDevUrl?: string, token?: string}} o
 * @returns {Promise<{info: string[], workersDev: string[], token: string[]}>}
 */
export async function accessPostFailures(o) {
  const info = async () => {
    const r = await probe(`https://${o.host}/__backyard/api/deploy/info`, { accept: "application/json" });
    if (!r.ok) return [`https://${o.host}/__backyard/api/deploy/info does not answer (${failText(r)})`];
    const mode = r.json?.auth?.mode;
    return mode === "access" ? [] : [`https://${o.host}/__backyard/api/deploy/info reports auth mode ${mode ? JSON.stringify(mode) : `(HTTP ${r.status})`}, not "access"`];
  };
  const workersDev = async () => {
    if (!o.workersDevUrl) return [];
    const r = await probe(`${o.workersDevUrl}/`, { accept: "text/html" });
    if (!r.ok) return [`${o.workersDevUrl}/ does not answer (${failText(r)})`];
    return r.status === 401 ? [] : [`${o.workersDevUrl}/ answers HTTP ${r.status} without a sign-in (expected 401)`];
  };
  const token = async () => {
    if (!o.token) return [];
    const r = await probe(`https://${o.host}/__backyard/api/deploy/me`, { accept: "application/json", headers: { Authorization: `Bearer ${o.token}` } });
    if (!r.ok) return [`https://${o.host}/__backyard/api/deploy/me does not answer (${failText(r)})`];
    if (accessBlocked(r)) return [`the deploy token cannot reach the Worker: the bypass application is missing ${o.host}/__backyard/api/deploy/*`];
    return r.status === 200 ? [] : [`the deploy token is not accepted at https://${o.host}/__backyard/api/deploy/me (HTTP ${r.status})`];
  };
  const [i, w, t] = await Promise.all([info(), workersDev(), token()]);
  return { info: i, workersDev: w, token: t };
}

// ---------------------------------------------------------------------------
// API mode

/** JSON with sorted keys (for comparisons). */
function stable(v) {
  if (Array.isArray(v)) return `[${v.map(stable).join(",")}]`;
  if (v && typeof v === "object") {
    return `{${Object.keys(v)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${stable(v[k])}`)
      .join(",")}}`;
  }
  return JSON.stringify(v ?? null);
}

/** A field of an Access object in a comparable form. */
function canon(key, v) {
  if (key === "policies") {
    return stable(
      (Array.isArray(v) ? v : [])
        .map((p, i) => [typeof p === "string" ? p : String(p?.id ?? ""), Number(p?.precedence ?? i + 1)])
        .sort((a, b) => a[1] - b[1] || String(a[0]).localeCompare(String(b[0]))),
    );
  }
  if (key === "destinations") {
    return stable((Array.isArray(v) ? v : []).map((d) => `${d?.type ?? "public"}|${String(d?.uri ?? "").toLowerCase()}`).sort());
  }
  if (key === "include") return stable((Array.isArray(v) ? v : []).map(stable).sort());
  if (key === "domain") return String(v ?? "").toLowerCase();
  return stable(v);
}

/** The fields of `desired` that `live` does not have the same way. */
export function changedFields(desired, live) {
  return Object.keys(desired).filter((k) => canon(k, desired[k]) !== canon(k, live?.[k]));
}

/** Fields of a live Access object that Cloudflare sets itself: never sent back. */
const READ_ONLY_FIELDS = new Set(["id", "uid", "aud", "created_at", "updated_at", "reusable", "app_count", "self_hosted_domains"]);

/**
 * The body of a PUT that changes only what this tool manages. Cloudflare's PUT
 * replaces the whole object, so the body starts from the live object (what an
 * admin set in the dashboard stays: require/exclude rules, allowed_idps,
 * custom pages, …) and overlays `desired` (with the live include list when
 * keepInclude). An application's `policies` are sent as references: the live
 * ones (a policy attached in the dashboard stays attached) without the `stale`
 * ids of this tool, plus ours (desired.policies[0]) when it is missing.
 * @param {Record<string, any>} live
 * @param {Record<string, any>} desired
 * @param {{kind: "apps"|"policies", keepInclude?: boolean, stale?: string[]}} o
 */
export function putBody(live, desired, o) {
  const body = Object.fromEntries(Object.entries(live ?? {}).filter(([k]) => !READ_ONLY_FIELDS.has(k) && k !== "policies"));
  Object.assign(body, desired);
  if (o.keepInclude) body.include = live?.include ?? [];
  if (o.kind === "apps") {
    const ours = String(desired.policies?.[0]?.id ?? "");
    const stale = new Set((o.stale ?? []).filter((id) => id && id !== ours));
    const refs = (Array.isArray(live?.policies) ? live.policies : [])
      .map((p, i) => {
        const precedence = Number(p?.precedence);
        // References only: an object with more fields would be read as an inline policy.
        return { id: String(typeof p === "string" ? p : (p?.id ?? "")), precedence: Number.isFinite(precedence) ? precedence : i + 1 };
      })
      .filter((r) => r.id && !stale.has(r.id));
    if (ours && !refs.some((r) => r.id === ours)) refs.push({ id: ours, precedence: Math.max(0, ...refs.map((r) => r.precedence)) + 1 });
    body.policies = refs;
  }
  return body;
}

const lower = (s) => String(s ?? "").trim().toLowerCase();

/** The host part and the path part of an Access destination ("host/p" → ["host", "/p"]). */
function splitUri(uri) {
  const u = lower(uri).replace(/^https?:\/\//, "");
  const i = u.indexOf("/");
  return i >= 0 ? [u.slice(0, i), u.slice(i)] : [u, ""];
}

/**
 * Whether an Access application covers `host`: its domain or a destination is
 * the host (any path), *.<host>, or a wildcard *.<s> for a suffix s of host.
 * A bookmark (an App Launcher tile) gates nothing, so it never covers a host.
 */
export function appCovers(app, host) {
  if (lower(app?.type) === "bookmark") return false;
  const h = lower(host);
  const uris = [app?.domain, ...(Array.isArray(app?.destinations) ? app.destinations.map((d) => d?.uri) : [])].filter(Boolean);
  return uris.some((uri) => {
    const [uh] = splitUri(uri);
    if (uh === h || uh === `*.${h}`) return true;
    return uh.startsWith("*.") && h.endsWith(uh.slice(1));
  });
}

/**
 * Creates or updates the four Access objects of the Worker (API mode).
 * @param {import("./cli.mjs").Ctx} ctx
 * @param {Record<string, any>} state
 * @param {{admin: import("./lib/cfapi.mjs").Api, host: string, mode: string, allow: {emails: string[], domains: string[]}, explicit: boolean, onCreated: () => void}} o
 * @returns {Promise<{team: string, aud: string, ids: Record<string, string>, applied: string[]} | null>}
 *   null: the credential lacks the Access permissions and is wrangler's own CLOUDFLARE_API_TOKEN (guided instead)
 */
async function apiMode(ctx, state, o) {
  const { admin, host } = o;
  const a = ctx.accountId;
  const w = ctx.worker;
  const inv = ctx.invokedAs;
  const n = accessNames(w);
  const derived = admin === (await ctx.api()); // CLOUDFLARE_API_TOKEN, not a dedicated admin token
  const applied = [];

  const permissionError = (e) => {
    const msg = `the credential (${admin.label}) cannot manage Cloudflare Access: an API token needs ${ACCESS_TOKEN_PERMISSIONS}`;
    if (derived) {
      ui.warn(`${msg}. Continuing with the dashboard steps (or set BACKYARD_CF_API_TOKEN to a token that has them).`);
      return null;
    }
    throw new CliError(EXIT.FAILED, `${msg} (${e.method} ${e.path} answered ${e.status}). Nothing was changed.`);
  };

  // 1. The Zero Trust organization: its auth_domain is the team.
  let org;
  try {
    org = await admin.get(`/accounts/${a}/access/organizations`);
  } catch (e) {
    if (e instanceof CfError && e.forbidden) return permissionError(e);
    if (e instanceof CfError && (e.notFound || e.has(12130))) org = null;
    else throw new CliError(EXIT.FAILED, `could not read the Zero Trust organization of the account (nothing was changed): ${e?.message ?? e}`);
  }
  const team = normalizeTeam(org?.auth_domain);
  if (!team) {
    throw new CliError(EXIT.USAGE, "Zero Trust is not set up on this account: open https://one.dash.cloudflare.com/ once, pick a team name and the Free plan, then re-run");
  }

  // 2. Discovery: ours (an id this machine knows, or our name on our host) and anyone else's.
  let apps;
  try {
    apps = await admin.list(`/accounts/${a}/access/apps`, {}, { perPage: 100 });
  } catch (e) {
    if (e instanceof CfError && e.forbidden) return permissionError(e);
    throw new CliError(EXIT.FAILED, `could not list the Access applications (nothing was changed): ${e?.message ?? e}`);
  }
  const known = {};
  const created = createdFor(state, host);
  for (const k of ID_KEYS) known[k] = state.access?.[k] || created[k] || "";
  const bypassDomain = `${host}/__backyard/api/deploy`;
  const isMain = (app) => (known.appId && app.id === known.appId) || (app.name === n.app && lower(app.domain) === lower(host));
  const isBypass = (app) => (known.bypassAppId && app.id === known.bypassAppId) || (app.name === n.bypassApp && lower(app.domain) === lower(bypassDomain));
  const foreign = apps.filter((app) => !isMain(app) && !isBypass(app) && appCovers(app, host));
  if (foreign.length) {
    const lines = foreign.map(
      (f) => `Access application "${f.name}" (aud ${f.aud ?? "unknown"}) already covers ${host}. Use it with \`${inv} access enable --team ${team} --aud ${f.aud ?? "<aud>"}\`, or delete it`,
    );
    throw new CliError(EXIT.REFUSED, `${lines.join("\n")}\nNothing was changed.`, { reason: "foreign" });
  }
  const mainFound = apps.find(isMain);
  const bypassFound = apps.find(isBypass);
  if ((mainFound && !known.appId) || (bypassFound && !known.bypassAppId)) ui.note(`Found the Access applications of ${w} (made on another machine): reusing them.`);
  known.appId ||= mainFound?.id ?? "";
  known.bypassAppId ||= bypassFound?.id ?? "";
  // Policies: by id, else by name on our applications, else by name in the account.
  const refOf = (app, name) => (Array.isArray(app?.policies) ? app.policies.find((p) => p && typeof p === "object" && p.name === name)?.id : undefined) ?? "";
  known.allowPolicyId ||= refOf(mainFound, n.allowPolicy);
  known.bypassPolicyId ||= refOf(bypassFound, n.bypassPolicy);
  let policyList;
  const findPolicy = async (name, fo = {}) => {
    if (fo.fresh) policyList = undefined;
    policyList ??= await admin.list(`/accounts/${a}/access/policies`, {}, { perPage: 100 });
    return policyList.find((p) => p?.name === name) ?? null;
  };
  if (!known.allowPolicyId) known.allowPolicyId = (await findPolicy(n.allowPolicy))?.id ?? "";
  if (!known.bypassPolicyId) known.bypassPolicyId = (await findPolicy(n.bypassPolicy))?.id ?? "";

  // 3. Writes: allow policy, bypass policy, bypass application, hub application.
  // A new id is saved at once (in state.access while Access is on, else in
  // state.accessCreated), so a failure later never leads to a duplicate.
  const record = (key, id) => {
    if (state.access) state.access[key] = id;
    else trackCreated(state, host, { ...createdFor(state, host), [key]: id }, w);
    ctx.saveState(state);
    o.onCreated();
  };
  const ensure = async ({ key, kind, label, id, desired, keepInclude, beforeCreate, find }) => {
    const path = `/accounts/${a}/access/${kind}`;
    let live = null;
    if (id) {
      try {
        live = await admin.get(`${path}/${id}`);
      } catch (e) {
        if (!(e instanceof CfError && e.notFound)) throw e;
        ui.warn(`${label} (${id}) is gone: creating it again`);
      }
    }
    if (live) {
      // Only the managed fields change: settings made in the dashboard are kept.
      const body = putBody(live, desired, { kind, keepInclude, stale: Object.values(known) });
      const changed = changedFields(body, live);
      if (changed.length === 0) {
        ui.note(`${label}: unchanged`);
        return live;
      }
      const obj = await admin.put(`${path}/${live.id ?? id}`, body);
      ui.ok(`${label} updated (${changed.join(", ")})`);
      applied.push(`${label} updated`);
      return { ...live, ...(obj ?? {}) };
    }
    beforeCreate?.();
    const obj = await admin.postOrFind(path, desired, find);
    if (!obj?.id) throw new CliError(EXIT.FAILED, `creating ${label} returned no id`);
    record(key, obj.id);
    ui.ok(`${label} created`);
    applied.push(`${label} created`);
    return obj;
  };

  try {
    return await writeAll();
  } catch (e) {
    if (e instanceof CfError) throw new CliError(EXIT.FAILED, e.message, { applied, cause: e });
    throw e;
  }

  async function writeAll() {
    const p = policyBodies({ worker: w, emails: o.allow.emails, domains: o.allow.domains });
    const allowPolicy = await ensure({
      key: "allowPolicyId",
      kind: "policies",
      label: `Access policy "${n.allowPolicy}"`,
      id: known.allowPolicyId,
      desired: p.allow,
      keepInclude: !o.explicit,
      beforeCreate: () => {
        if (isEmpty(o.allow)) {
          throw new CliError(EXIT.USAGE, "who may sign in: pass --allow-email <email> or --allow-email-domain <domain> (both repeatable), or run on an interactive terminal");
        }
      },
      find: () => findPolicy(n.allowPolicy, { fresh: true }).catch(() => null),
    });
    const bypassPolicy = await ensure({
      key: "bypassPolicyId",
      kind: "policies",
      label: `Access policy "${n.bypassPolicy}"`,
      id: known.bypassPolicyId,
      desired: p.bypass,
      find: () => findPolicy(n.bypassPolicy, { fresh: true }).catch(() => null),
    });
    const bodies = appBodies({ worker: w, host, mode: o.mode, allowPolicyId: allowPolicy.id, bypassPolicyId: bypassPolicy.id });
    const findApp = (name, domain) => async () => {
      const list = await admin.list(`/accounts/${a}/access/apps`, {}, { perPage: 100 }).catch(() => []);
      return list.find((x) => x?.name === name && lower(x.domain) === lower(domain)) ?? null;
    };
    const bypassApp = await ensure({
      key: "bypassAppId",
      kind: "apps",
      label: `Access application "${n.bypassApp}"`,
      id: known.bypassAppId,
      desired: bodies.bypass,
      find: findApp(n.bypassApp, bypassDomain),
    });
    const mainApp = await ensure({
      key: "appId",
      kind: "apps",
      label: `Access application "${n.app}"`,
      id: known.appId,
      desired: bodies.main,
      find: findApp(n.app, host),
    });
    const aud = String(mainApp.aud ?? "").toLowerCase();
    if (!AUD_RE.test(aud)) throw new CliError(EXIT.FAILED, `the Access application "${n.app}" has no valid AUD tag (got ${JSON.stringify(mainApp.aud ?? "")})`, { applied });
    return {
      team,
      aud,
      ids: { appId: mainApp.id, bypassAppId: bypassApp.id, allowPolicyId: allowPolicy.id, bypassPolicyId: bypassPolicy.id },
      applied,
    };
  }
}

// ---------------------------------------------------------------------------
// Guided mode

/**
 * Prints the dashboard steps and, on a terminal, waits for Access to show up
 * at the edge (asking before it uses what it found).
 * @param {import("./cli.mjs").Ctx} ctx
 * @param {{host: string, mode: string, allow: {emails: string[], domains: string[]}, wait?: number}} o
 * @returns {Promise<{team: string, aud: string}>}
 */
async function guidedMode(ctx, o) {
  const { host } = o;
  const inv = ctx.invokedAs;
  ui.log("");
  printGuidedSteps({ worker: ctx.worker, host, mode: o.mode, emails: o.allow.emails, domains: o.allow.domains, invokedAs: inv });
  const look = async () => {
    const r = await probe(`https://${host}/`, { accept: "text/html" });
    const found = r.ok && r.status >= 300 && r.status < 400 ? parseAccessRedirect(r.location) : null;
    return { done: Boolean(found), found, why: r.ok ? `HTTP ${r.status}` : failText(r) };
  };
  if (!ctx.interactive) {
    const { found } = await look();
    if (found && AUD_RE.test(found.kid)) {
      throw new CliError(
        EXIT.USAGE,
        `Found Cloudflare Access in front of https://${host}: team ${found.team}, application AUD ${found.kid}. To use it: ${inv} access enable --team ${found.team} --aud ${found.kid}`,
      );
    }
    throw new CliError(
      EXIT.USAGE,
      `create the two applications above in the dashboard, then re-run on an interactive terminal (it detects them) or with --team <team> --aud <aud>${found ? ` (Access in front of https://${host} belongs to team ${found.team})` : ""}`,
    );
  }
  const timeoutSec = o.wait ?? DISCOVERY_WAIT;
  ui.log(`Waiting for Cloudflare Access in front of https://${host} (checking every 5 s for up to ${timeoutSec} s; Ctrl-C stops)…`);
  const r = await waitFor(look, { timeoutSec, intervalSec: 5, onPending: progress("still no Cloudflare Access login") });
  if (!r.found) {
    throw new CliError(EXIT.PENDING, `no Cloudflare Access login in front of https://${host} yet (${r.why}): finish the dashboard steps above, then re-run ${inv} access enable`);
  }
  const { team, kid } = r.found;
  const use = await ctx.prompt.confirm(`Found Cloudflare Access in front of https://${host}: team ${team}, application AUD ${kid || "(not shown)"}. Use it?`, { honorYes: false });
  if (!use) throw new CliError(EXIT.REFUSED, `not using it: nothing was changed (pass --team and --aud of the right application)`, { reason: "declined" });
  let aud = AUD_RE.test(kid) ? kid : "";
  while (!aud) {
    const answer = await ctx.prompt.ask("Application Audience (AUD) Tag (Access → Applications → Backyard → Overview):", { flag: "--aud" });
    if (AUD_RE.test(answer.trim())) aud = answer.trim().toLowerCase();
    else ui.warn("the AUD tag is 64 hexadecimal characters");
  }
  return { team, aud };
}

// ---------------------------------------------------------------------------
// access enable

/**
 * `access enable`.
 * @param {Record<string, any>} opts   parsed flags (lib/args.mjs)
 * @param {import("./cli.mjs").Ctx} ctx
 * @returns {Promise<number | void>}
 */
export async function runAccessEnable(opts, ctx) {
  const inv = ctx.invokedAs;
  const w = ctx.worker;
  // Usage errors first: nothing is touched before the input is known to be valid.
  if (Boolean(opts.team) !== Boolean(opts.aud)) {
    throw new CliError(EXIT.USAGE, "--team and --aud go together: the Zero Trust team and the AUD tag of the hub's Access application");
  }
  const given = opts.team ? { team: checkTeam(opts.team), aud: checkAud(opts.aud) } : null;
  const flagAllow = normalizeAllow(opts.allowEmail ?? [], opts.allowEmailDomain ?? []);

  ui.log(bold("\nBackyard on Cloudflare — company login (Cloudflare Access)\n"));
  const state = ctx.loadState();
  const previousUrl = state.url ?? "";
  await ctx.login(state);
  await ctx.api();
  const live = await ctx.readLive();
  if (live.exists === false) throw new CliError(EXIT.USAGE, `Worker ${w} is not deployed on this account: run ${inv} setup first`);
  ctx.adopt(state, live);

  // 1. Preconditions.
  const ext = externalKeys(state, MANAGED_ACCESS_KEYS);
  if (ext.length) {
    throw new CliError(
      EXIT.USAGE,
      `${ext.join(", ")} ${ext.length === 1 ? "is" : "are"} set with --var (e.g. by the Terraform module): drop ${ext.length === 1 ? "it" : "them"} with ${ext.map((k) => `--var ${k}:`).join(" ")} or keep managing ${ext.length === 1 ? "it" : "them"} with Terraform`,
    );
  }
  const domain = state.domain;
  if (!domain?.host || domain.status !== "active") {
    let why = "";
    if (domain?.host) why = ` (${domain.host} is ${domain.status ?? "not verified"}: re-run ${inv} domain add ${domain.host}${domain.mode === "host" ? " --host-mode" : ""})`;
    else if (state.vars?.BASE_DOMAIN) why = ` (BASE_DOMAIN is set with --var; access enable needs a domain added with ${inv} domain add)`;
    throw new CliError(EXIT.USAGE, `Cloudflare Access protects your own hostname: run ${inv} domain add <host> first${why}`);
  }
  const host = domain.host;
  const mode = domain.mode === "host" ? "host" : "path";
  const ready = await probe(`https://${host}/__backyard/readyz`, { accept: "application/json" });
  if (!ready.ok) throw new CliError(EXIT.FAILED, `https://${host} does not answer (${failText(ready)}): check it with ${inv} status`);

  // 2. Who may sign in.
  let allow = flagAllow;
  let explicit = !isEmpty(flagAllow);
  if (!explicit && state.access?.allow) {
    allow = normalizeAllow(state.access.allow.emails ?? [], state.access.allow.emailDomains ?? []);
  }
  if (isEmpty(allow) && !given && ctx.interactive) {
    for (;;) {
      try {
        allow = parseWho(await ctx.prompt.ask("Who may sign in? (emails or @domains, comma-separated)", { flag: "--allow-email-domain" }));
        break;
      } catch (e) {
        if (!(e instanceof CliError) || e.code !== EXIT.USAGE || /input needed|no answer/.test(e.message)) throw e;
        ui.warn(e.message.replace(/^--allow-email(-domain)? expects/, "expected"));
      }
    }
    explicit = !isEmpty(allow);
  }
  if (explicit) {
    for (const d of allow.domains.filter(isConsumerMailDomain)) {
      if (!(await ctx.prompt.confirm(`Anyone with a ${d} address could sign in. Allow ${d} anyway?`))) {
        throw new CliError(EXIT.REFUSED, `${d} not allowed: nothing was changed (list your company's domain or single emails instead)`, { reason: "declined" });
      }
    }
  }

  // 3. The mode: --team/--aud, API (credential A), or the dashboard (guided).
  let plan = null;
  let saved = false;
  let upgradeWithoutToken = false;
  const onCreated = () => {
    saved = true;
  };
  const prev = state.access;
  const remembered = prev?.team && AUD_RE.test(String(prev.aud ?? "")) ? { team: normalizeTeam(prev.team), aud: lower(prev.aud), source: prev.source ?? "manual", ids: pickIds(prev), applied: [] } : null;
  const useRemembered = () => {
    ui.note(`Using team ${remembered.team} and AUD ${remembered.aud} (remembered; --team and --aud change them).`);
    return remembered;
  };
  // Whether the allow flags change who may sign in (compared with the list remembered here).
  let prevAllow = null;
  try {
    if (prev?.allow) prevAllow = normalizeAllow(prev.allow.emails ?? [], prev.allow.emailDomains ?? []);
  } catch {
    prevAllow = null; // a hand-edited list: treated as unknown
  }
  const allowChanged = explicit && (!prevAllow || !sameItems(allow.emails, prevAllow.emails) || !sameItems(allow.domains, prevAllow.domains));
  if (given) {
    const same = remembered && remembered.team === given.team && remembered.aud === given.aud;
    plan = same ? remembered : { ...given, source: "manual", ids: {}, applied: [] };
  } else if (remembered?.source === "manual" && !explicit) {
    // Made in the dashboard: verified again, never taken over by API mode.
    plan = useRemembered();
  } else {
    // A new allow list needs the API: offer to paste a token even on a re-run.
    let ask = !remembered || allowChanged;
    // So does a bypass application this tool made that lacks paths (one of an
    // earlier version, before the Claude connector's four): found at the edge.
    // Without a token the verification below still waits for them (added in
    // the dashboard meanwhile, or just now and not yet at the edge).
    let missing = [];
    if (!ask && remembered?.source === "api") {
      missing = (await accessEdgeFailures({ host, mode, team: remembered.team, aud: remembered.aud, worker: w })).missing;
      ask = missing.length > 0;
    }
    const admin = await ctx.admin("access", { prompt: ask });
    if (admin) {
      const r = await apiMode(ctx, state, { admin, host, mode, allow, explicit, onCreated });
      if (r) plan = { ...r, source: "api" };
    }
    upgradeWithoutToken = !plan && missing.length > 0;
    if (!plan && remembered && allowChanged) {
      // The remembered team and AUD would verify fine, but the policy would keep the old list.
      const where = remembered.source === "api" ? `Policies → "${accessNames(w).allowPolicy}"` : "Applications → the hub's application → Policies";
      throw new CliError(
        EXIT.USAGE,
        `changing who may sign in needs an API token with ${ACCESS_TOKEN_PERMISSIONS}: set BACKYARD_CF_API_TOKEN (it is never stored) and re-run, ` +
          `or edit the Allow policy in the dashboard (Zero Trust → Access → ${where}) and re-run without allow flags. Nothing was changed.`,
      );
    }
    if (!plan && remembered) plan = useRemembered();
    if (!plan) plan = { ...(await guidedMode(ctx, { host, mode, allow, wait: opts.wait })), source: "manual", ids: {}, applied: [] };
  }
  const { team, aud } = plan;

  // 4. Edge verification, before anything changes on the Worker.
  ui.log(`\nChecking Cloudflare Access at the edge (team ${team}, AUD ${aud})…`);
  const verify = async () => {
    const f = await accessEdgeFailures({ host, mode, team, aud, worker: w });
    const failures = [...f.certs, ...f.redirect, ...f.wildcard, ...f.bypass, ...f.connector];
    return { done: failures.length === 0, failures, missing: f.missing };
  };
  const v = await waitFor(verify, { timeoutSec: opts.wait ?? VERIFY_WAIT, intervalSec: 5, onPending: progress("waiting for Cloudflare Access") });
  if (v.failures.length) {
    if (saved) ctx.writeResult(state, { previousUrl });
    if (upgradeWithoutToken && v.missing.length) {
      // The bypass application this tool made still lacks paths, and there was no token to update it.
      const others = v.failures.filter((f) => !f.startsWith("the bypass application is missing "));
      throw new CliError(
        EXIT.PENDING,
        `the Access application "${accessNames(w).bypassApp}" lacks paths this version needs (${v.missing.join(", ")}), so the Worker was not changed. ` +
          `Updating it needs an API token with ${ACCESS_TOKEN_PERMISSIONS}: set BACKYARD_CF_API_TOKEN (it is never stored) and re-run, ` +
          `or add these destinations to it in the dashboard (Zero Trust → Access → Applications → "${accessNames(w).bypassApp}" → Destinations) and re-run (if you just did, give it a minute).` +
          (others.length ? `\nAlso not ready:\n${others.map((f) => `  - ${f}`).join("\n")}` : ""),
        { applied: plan.applied },
      );
    }
    throw new CliError(
      EXIT.PENDING,
      `Cloudflare Access in front of https://${host} is not ready, so the Worker was not changed:\n${v.failures.map((f) => `  - ${f}`).join("\n")}\n` +
        `Fix it (or give it a minute), then re-run the same command.`,
      { applied: plan.applied },
    );
  }
  ui.ok(`Cloudflare Access verified: login redirect to ${team}, AUD ${aud}, ${ACCESS_BYPASS_PATHS.length} machine paths reach the Worker`);

  // 5. Turn it on: AUTH_MODE=access and the team and AUD (the guard passes: auth gets stronger).
  // A new list is remembered only when it was applied (API mode), or when nothing was remembered before.
  const allowSaved = !isEmpty(allow) && (plan.source === "api" || !prev?.allow) ? { emails: allow.emails, emailDomains: allow.domains } : prev?.allow;
  const access = { team, aud, source: plan.source, ...plan.ids, ...(allowSaved ? { allow: allowSaved } : {}) };
  const d = await ctx.deploy(state, { live, stateOverrides: { access } });
  state.access = access;
  const leftover = leftoverIds(createdFor(state, host), plan.ids);
  if (plan.source !== "api" && prev?.source === "api") Object.assign(leftover, leftoverIds(pickIds(prev), plan.ids)); // replaced by --team/--aud
  trackCreated(state, host, leftover, w);
  ctx.saveState(state);

  // Post-checks: the Worker enforces it, workers.dev is no back door, the deploy token still works.
  const workersDevUrl = state.workersDev !== false ? state.workersDevUrl ?? "" : "";
  const post = await waitFor(
    async () => {
      const f = await accessPostFailures({ host, workersDevUrl, token: d.token });
      const failures = [...f.info, ...f.workersDev, ...f.token];
      return { done: failures.length === 0, failures };
    },
    { timeoutSec: Math.min(POST_CHECK_WAIT, opts.wait ?? POST_CHECK_WAIT), intervalSec: 5, onPending: progress("waiting for the new version") },
  );
  ctx.writeResult(state, { previousUrl, authMode: "access" });
  if (post.failures.length) {
    throw new CliError(EXIT.PENDING, `applied, but ${post.failures.join("; ")}. Check again with ${inv} status, or re-run ${inv} access enable.`, {
      applied: [...plan.applied, "the Worker deployed with AUTH_MODE=access"],
    });
  }

  ui.log("");
  ui.ok(`Company login is on. Teammates: ${bold(`backyard login --server https://${host}`)}`);
  ui.log(`  Make someone admin: ${inv} setup --var ADMINS:<email>`);
  if (state.workersDev !== false) ui.log(`  Optional: ${inv} setup --no-workers-dev ${dim("(workers.dev already answers 401 without a sign-in)")}`);
  ui.log("");
  return EXIT.OK;
}

/** The object ids of an access section. */
function pickIds(a) {
  const out = {};
  for (const k of ID_KEYS) if (a?.[k]) out[k] = a[k];
  return out;
}

/** The ids in `created` that `used` does not hold. */
function leftoverIds(created, used = {}) {
  const out = {};
  for (const k of ID_KEYS) if (created?.[k] && created[k] !== used[k]) out[k] = created[k];
  return out;
}

/** The ids of state.accessCreated when it is about `host` ({} for another host). */
function createdFor(state, host) {
  const c = state.accessCreated;
  if (!c || (c.host && host && lower(c.host) !== lower(host))) return {};
  return pickIds(c);
}

/**
 * Sets state.accessCreated to the ids of objects this tool created for
 * `host` (undefined when there are none). A record about another host is left
 * alone, unless it must be replaced: its objects are then reported as no
 * longer tracked.
 */
function trackCreated(state, host, ids, worker) {
  const cur = state.accessCreated;
  const has = ID_KEYS.some((k) => ids[k]);
  const other = cur && cur.host && host && lower(cur.host) !== lower(host) && ID_KEYS.some((k) => cur[k]);
  if (other && !has) return;
  if (other) {
    const n = accessNames(worker);
    ui.warn(`The Access objects this tool created for ${cur.host} (${n.app}, ${n.bypassApp}) are no longer tracked: delete them in the dashboard if they are unused`);
  }
  state.accessCreated = has ? { host: host || undefined, ...pickIds(ids) } : undefined;
}

// ---------------------------------------------------------------------------
// access disable

/**
 * Deletes Access objects this tool created, in order (hub app, bypass app,
 * allow policy, bypass policy), saving the state after each; 404 counts as
 * gone. Throws CliError(1) on the first failure (a re-run resumes).
 * @param {import("./cli.mjs").Ctx} ctx
 * @param {Record<string, any>} state   state.accessCreated holds the ids
 * @param {import("./lib/cfapi.mjs").Api} admin
 * @param {{applied: string[], previousUrl: string}} o
 */
async function deleteCreated(ctx, state, admin, o) {
  const n = accessNames(ctx.worker);
  const labels = { appId: `Access application "${n.app}"`, bypassAppId: `Access application "${n.bypassApp}"`, allowPolicyId: `Access policy "${n.allowPolicy}"`, bypassPolicyId: `Access policy "${n.bypassPolicy}"` };
  for (const key of ID_KEYS) {
    const id = state.accessCreated?.[key];
    if (!id) continue;
    const kind = key.endsWith("AppId") || key === "appId" ? "apps" : "policies";
    try {
      await admin.delete(`/accounts/${ctx.accountId}/access/${kind}/${id}`);
      ui.ok(`${labels[key]} deleted`);
    } catch (e) {
      if (!(e instanceof CfError && e.notFound)) {
        ctx.writeResult(state, { previousUrl: o.previousUrl, authMode: "none" });
        throw new CliError(EXIT.FAILED, `could not delete ${labels[key]} (${id}): ${e?.message ?? e}`, { applied: o.applied });
      }
      ui.note(`${labels[key]} was already gone`);
    }
    o.applied.push(`${labels[key]} deleted`);
    const rest = { ...state.accessCreated };
    delete rest[key];
    state.accessCreated = ID_KEYS.some((k) => rest[k]) ? rest : undefined;
    ctx.saveState(state);
  }
}

/**
 * `access disable`.
 * @param {Record<string, any>} opts
 * @param {import("./cli.mjs").Ctx} ctx
 * @returns {Promise<number | void>}
 */
export async function runAccessDisable(opts, ctx) {
  const inv = ctx.invokedAs;
  const w = ctx.worker;
  const n = accessNames(w);
  ui.log(bold("\nBackyard on Cloudflare — turn company login (Cloudflare Access) off\n"));
  const state = ctx.loadState();
  const previousUrl = state.url ?? "";
  let loggedIn = false;
  if (!state.updatedAt) {
    // A machine without setup state adopts the live Worker first.
    await ctx.login(state);
    loggedIn = true;
    ctx.adopt(state, await ctx.readLive());
  }
  const host = state.domain?.host ?? "";
  const leftover = pickIds(state.accessCreated); // of any host

  if (!state.access) {
    if (opts.deleteApps && Object.keys(leftover).length) {
      // Objects of an earlier enable or disable: delete them (the Worker is not behind Access).
      if (!loggedIn) await ctx.login(state);
      const admin = await adminFor(ctx, n);
      const what = Object.keys(leftover).length;
      if (!(await ctx.prompt.confirm(`Delete the ${what} Access object${what === 1 ? "" : "s"} this tool created${state.accessCreated?.host ? ` for ${state.accessCreated.host}` : ""}? Anyone with a link can then open the sites there.`))) {
        throw new CliError(EXIT.REFUSED, "nothing was deleted", { reason: "declined" });
      }
      const applied = [];
      await deleteCreated(ctx, state, admin, { applied, previousUrl });
      ctx.saveState(state);
      ctx.writeResult(state, { previousUrl, authMode: "none" });
      return EXIT.OK;
    }
    ui.ok("Cloudflare Access is not managed by this tool: nothing to do");
    const ext = externalKeys(state, MANAGED_ACCESS_KEYS);
    if (ext.length) ui.note(`${ext.join(", ")} ${ext.length === 1 ? "is" : "are"} set with --var: turn Access off with ${inv} setup --var AUTH_MODE:none --confirm-auth-change`);
    else if (state.updatedAt) ui.note(`If another machine turned it on: delete ${ctx.statePath} to adopt the live configuration, then re-run.`);
    if (Object.keys(leftover).length) ui.note(`Access objects this tool created remain: delete them with ${inv} access disable --delete-apps`);
    return EXIT.OK;
  }

  // --delete-apps needs the ids of objects this tool created, and credential A: checked before anything changes.
  const ids = { ...createdFor(state, host), ...(state.access.source === "api" ? pickIds(state.access) : {}) };
  let admin = null;
  if (!loggedIn) await ctx.login(state);
  if (opts.deleteApps) {
    if (!Object.keys(ids).length) {
      const why =
        state.access.source === "adopted"
          ? `this machine does not know which Access applications this tool created (run ${inv} access enable once to find them, or delete ${n.app} and ${n.bypassApp} in the dashboard)`
          : `the applications were created in the dashboard: delete ${n.app} and ${n.bypassApp} there`;
      throw new CliError(EXIT.USAGE, why);
    }
    admin = await adminFor(ctx, n);
  }

  const wd = state.workersDev !== false ? state.workersDevUrl ?? "" : "";
  const where = [wd ? `on ${wd} right away` : "", host ? `on https://${host} ${opts.deleteApps ? "as soon as the Access applications are deleted (now)" : "once the Access applications are deleted"}` : ""].filter(Boolean);
  const q = `Turn company login off? Sites become viewable by anyone with a link${where.length ? `: ${where.join(", and ")}` : ""}.`;
  if (!(await ctx.prompt.confirm(q))) throw new CliError(EXIT.REFUSED, "company login stays on: nothing was changed", { reason: "declined" });

  // AUTH_MODE=none first, so nothing is left half-protected; the guard would refuse it on purpose.
  await ctx.deploy(state, { guard: false, stateOverrides: { access: undefined } });
  const applied = ["the Worker deployed with AUTH_MODE=none"];
  state.access = undefined;
  trackCreated(state, host, ids, w);
  ctx.saveState(state);

  if (opts.deleteApps) {
    await deleteCreated(ctx, state, admin, { applied, previousUrl });
    ctx.saveState(state);
  }
  ctx.writeResult(state, { previousUrl, authMode: "none" });

  ui.log("");
  ui.ok("Company login is off (AUTH_MODE=none).");
  if (!opts.deleteApps && host) {
    ui.log(
      `  The Access applications of https://${host} remain, so it still asks for a sign-in until they are deleted: ` +
        (Object.keys(ids).length ? `${n.app}, ${n.bypassApp} (${inv} access disable --delete-apps, with BACKYARD_CF_API_TOKEN)` : "delete them in the dashboard (Zero Trust → Access → Applications)"),
    );
  }
  ui.log("");
  return EXIT.OK;
}

/** Credential A for deleting Access objects; exit 2 without one. */
async function adminFor(ctx, n) {
  const admin = await ctx.admin("access");
  if (!admin) {
    throw new CliError(
      EXIT.USAGE,
      `--delete-apps needs an API token with ${ACCESS_TOKEN_PERMISSIONS}: set BACKYARD_CF_API_TOKEN (it is never stored), or delete ${n.app} and ${n.bypassApp} in the dashboard`,
    );
  }
  return admin;
}
