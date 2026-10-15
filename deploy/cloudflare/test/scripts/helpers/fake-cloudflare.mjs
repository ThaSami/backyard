// An in-memory (file-backed) Cloudflare for the engine's tests: the REST API
// under /client/v4, DNS-over-HTTPS under /dns-query and the edge under
// /__edge/<host>/<path>, on one loopback HTTP server. Nothing here talks to
// the real Cloudflare.
//
//   const fake = await startFakeCloudflare({ statePath });   // statePath = FAKE_CF_STATE
//   fake.apiBase    → CLOUDFLARE_API_BASE_URL   fake.dohUrl → BACKYARD_CF_DOH_URL
//   fake.origin     → FAKE_CF_ORIGIN (helpers/fetch-hook.mjs rewrites https://<host> to <origin>/__edge/<host>)
//
// STATE (FAKE_CF_STATE, a JSON file shared with helpers/fake-wrangler.mjs, which
// writes the deployed Workers; read and written on every request, so a test can
// change it between runs with fake.update(s => …) or sandbox.updateCf):
//
//   accounts      {<id>: {name, subdomain: "acme" | null}}   (null: no workers.dev subdomain → 10007)
//   takenSubdomains  ["name", …]                 names other accounts hold (→ 10031)
//   loggedIn      false → the fake wrangler reports "not logged in" (also FAKE_LOGGED_OUT=1)
//   tokens        {<token>: {label, permissions: [PERMS…], accounts?: [ids]}}   API tokens and the OAuth token
//   apiKeys       {<key>: {email, label, permissions}}                           X-Auth-Key credentials
//   workers       {<name>: {account, vars, bindings, secrets: {NAME: value}, workersDev, r2Bucket,
//                           deployments, version}}   (written by the fake wrangler's deploy)
//   buckets       ["name", …]                    R2 buckets (fake wrangler r2 bucket create)
//   zones         [{id, name, status: "active"|"pending", account}]
//   customDomains [{id, hostname, zone_id, zone_name, service, environment, enabled?, previews_enabled?}]
//                 (flags other than an origin's make the changeset report it modified, and
//                 the PUT needs override_existing_origin, also for the same Worker)
//   routes        [{id, zone_id, pattern, script}]
//   dnsRecords    [{id, zone_id, zone_name, type, name, content, proxied, ttl, comment}]
//   access        {<account>: {org: {auth_domain, name} | null, apps: [...], policies: [...]}}
//   dns           {<name or *.name>: "resolve" | "nxdomain" | "servfail"}  DoH/edge overrides
//   edge          {omitKid, redirectTeam, certsEmpty, readyz (status), readyzBody (its text when not 200),
//                  tlsFail: [host suffixes;
//                  "*.<host>" = its subdomains only],
//                  down: [hosts → ECONNREFUSED], workerVersion,
//                  preConnector: true → the Worker predates the Claude connector (see EDGE)}
//
// PERMISSIONS: a credential needs, per endpoint (edit implies read):
//   workers subdomain GET → scriptsRead, PUT → scriptsEdit; script settings/subdomain → scriptsRead;
//   zones → zoneRead; custom domains: GET → scriptsRead, POST/PUT/DELETE → scriptsEdit + zoneRead;
//   routes: GET → routesRead, writes → routesEdit; dns_records: GET → dnsRead, writes → dnsEdit;
//   access organizations → accessOrgRead; access apps/policies: GET → accessAppsRead, writes → accessAppsEdit.
// Missing or unknown credentials → 401 code 10000; a known one without the permission → 403
// code 10000 ("Authentication error"). TOKEN_PRESETS has the usual sets (oauth = wrangler's
// OAuth scopes: no DNS, no Access).
//
// FAULTS: fake.addFault({method, path, match, status, code, message, times = 1, retryAfter, body, apply})
// answers matching API requests (path: exact string, "*" glob, or RegExp, without
// /client/v4 and the query; match: an optional predicate on the logged request
// {method, path, query, body, token}) with that error before anything else; times:
// Infinity for always. retryAfter sets Retry-After. apply: true carries the request
// out first (when it would succeed) and only then answers with the error, like a
// Cloudflare that acted but whose answer was lost (an ambiguous POST).
//
// PAGINATION: list endpoints honour page/per_page (default per_page 20, like
// Cloudflare) and report result_info.total_pages; GET …/workers/routes returns
// every route of the zone.
//
// LOGS (in memory): fake.requests — every API request {method, path, query, body,
// token: <label> | "none" | "unknown" (never the secret), status, codes};
// fake.edgeRequests — {method, host, path, status, location}; fake.dohRequests — {name, type, status}.
// fake.writes() — the API requests that are not GETs.
// DELETE …/workers/domains/{id} answers 200 without a body, like the live API.
//
// EDGE: https://<host>/<path> (through the fetch hook) is answered like Cloudflare would:
//   * <team>.cloudflareaccess.com: /cdn-cgi/access/certs → {keys: [...]} (empty with
//     edge.certsEmpty) for a team that is some account's org auth_domain; else ENOTFOUND.
//   * A host that does not resolve (see DNS) → the fetch fails with ENOTFOUND (x-fake-error).
//   * <w>.<sub>.workers.dev → Worker w (when deployed with workers_dev on); a custom domain
//     → its service; a route pattern (*.<base>/*, <host>/*) → its script; another resolving
//     host → a foreign website (200 "some other website").
//   * Access (not on workers.dev): the most specific application destination wins
//     (an exact path "host/p" matches only /p; "host/p/*" matches /p/…; "host" every path;
//     "*.host" subdomains). A bypass policy → the Worker; an allow policy → 302 to
//     https://<team>/cdn-cgi/access/login/<host>?kid=<aud>&redirect_url=… (no kid with
//     edge.omitKid; team = edge.redirectTeam || the account's auth_domain); an app without
//     policies → 403.
//   * The fake Worker reflects the deployed vars and secrets: /__backyard/healthz → 200,
//     /__backyard/readyz → 200 with BACKYARD_SECRET (edge.readyz overrides) else 503,
//     /__backyard/api/deploy/info → {edition: "cloudflare", version, auth: {mode}, …},
//     /__backyard/api/deploy/me → 200 with "Authorization: Bearer <DEPLOY_TOKEN>", the guest
//     ({anonymous: true}) without a token in AUTH_MODE=none, else 401 JSON,
//     /__backyard/api/tokens → 401 JSON; the Claude connector's paths (SPEC §8.4) with
//     MCP_ENABLED on: /__backyard/mcp → 401 JSON (+ WWW-Authenticate), GET
//     /__backyard/oauth/{token,register,revoke} → 405 JSON, the two discovery documents
//     → 200 JSON, and /info carries mcp_url; with it off all four → 404 JSON (a
//     Worker with edge.preConnector answers /__backyard/mcp and /__backyard/oauth/* with
//     404 JSON and the discovery documents like any hub page); with AUTH_MODE=access
//     every other path → 401 (unless a Cf-Access-Jwt-Assertion header is sent), else
//     the hub page (200 HTML).
// DNS (DoH and edge): a name resolves when dns[name] says so, it is a custom domain, it has
// a DNS record (or a "*.<parent>" record covers it), it is a deployed <w>.<sub>.workers.dev,
// or an Access team; dns[name] = "nxdomain" overrides. DoH answers {Status: 0, Answer: [...]},
// {Status: 3} (NXDOMAIN) or {Status: 2} (servfail).

import { randomBytes, randomUUID } from "node:crypto";
import { existsSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { createServer } from "node:http";

export const PERMS = Object.freeze({
  accountRead: "account:read",
  scriptsRead: "workers_scripts:read",
  scriptsEdit: "workers_scripts:edit",
  routesRead: "workers_routes:read",
  routesEdit: "workers_routes:edit",
  zoneRead: "zone:read",
  dnsRead: "dns:read",
  dnsEdit: "dns:edit",
  accessOrgRead: "access_org:read",
  accessAppsRead: "access_apps:read",
  accessAppsEdit: "access_apps:edit",
});

/** Permission sets of typical credentials. */
export const TOKEN_PRESETS = Object.freeze({
  /** wrangler login (OAuth): Workers, routes, zones; no DNS, no Access */
  oauth: [PERMS.accountRead, PERMS.scriptsEdit, PERMS.routesEdit, PERMS.zoneRead],
  /** the "Edit Cloudflare Workers" API token template */
  workers: [PERMS.accountRead, PERMS.scriptsEdit, PERMS.routesEdit, PERMS.zoneRead],
  /** Zone · DNS · Edit */
  dns: [PERMS.dnsEdit, PERMS.zoneRead],
  /** Access: Apps and Policies · Edit + Organizations · Read */
  access: [PERMS.accessAppsEdit, PERMS.accessOrgRead],
  /** everything */
  all: Object.values(PERMS),
});

/**
 * Whether a TLS failure list covers `host`: "example.com" covers it and its
 * subdomains, "*.example.com" only its subdomains (a certificate for the hub
 * that does not cover the sites below it). Shared with helpers/fetch-hook.mjs.
 * @param {string} host
 * @param {string[]} list
 */
export function tlsFails(host, list) {
  const h = String(host).toLowerCase();
  return list.some((raw) => {
    const sfx = String(raw).trim().toLowerCase();
    if (sfx.startsWith("*.")) return h.endsWith(sfx.slice(1)) && h !== sfx.slice(2);
    return h === sfx || h.endsWith(`.${sfx}`);
  });
}

/** The OAuth token the fake wrangler hands out (`wrangler auth token --json`). */
export const OAUTH_TOKEN = "fake-oauth-token-0d6c9a7f3b2e41d8a5c7e9f1";

/**
 * A fresh fake Cloudflare: one account "acc1" ("Acme", subdomain "acme"), the
 * OAuth token, no Workers, zones or Access. `over` replaces top-level keys.
 * @param {Record<string, any>} [over]
 */
export function defaultCfState(over = {}) {
  return {
    accounts: { acc1: { name: "Acme", subdomain: "acme" } },
    takenSubdomains: [],
    tokens: { [OAUTH_TOKEN]: { label: "oauth", permissions: [...TOKEN_PRESETS.oauth] } },
    apiKeys: {},
    workers: {},
    buckets: [],
    zones: [],
    customDomains: [],
    routes: [],
    dnsRecords: [],
    access: {},
    dns: {},
    edge: {},
    ...over,
  };
}

/**
 * Worker bindings like the Cloudflare API lists them for a config: Durable
 * Objects, AI, R2, vars (plain_text, or json for non-strings) and secret names.
 */
export function bindingsOf(config, secrets = {}) {
  const b = [];
  for (const d of config.durable_objects?.bindings ?? []) b.push({ type: "durable_object_namespace", name: d.name, class_name: d.class_name });
  if (config.ai?.binding) b.push({ type: "ai", name: config.ai.binding });
  for (const r of config.r2_buckets ?? []) b.push({ type: "r2_bucket", name: r.binding, bucket_name: r.bucket_name });
  for (const [k, v] of Object.entries(config.vars ?? {})) b.push(typeof v === "string" ? { type: "plain_text", name: k, text: v } : { type: "json", name: k, json: v });
  for (const k of Object.keys(secrets ?? {}).sort()) b.push({ type: "secret_text", name: k });
  return b;
}

/**
 * A deployed Worker for state.workers (what another machine left behind).
 * @param {{account?: string, vars?: Record<string, any>, secrets?: Record<string, string>, r2Bucket?: string, ai?: boolean, workersDev?: boolean, version?: string}} o
 *   ai: false leaves the Workers AI binding out (deployed with setup --no-ai)
 */
export function deployedWorker(o = {}) {
  const config = {
    durable_objects: { bindings: [{ name: "SITE", class_name: "SiteDO" }, { name: "REGISTRY", class_name: "RegistryDO" }] },
    ...(o.ai === false ? {} : { ai: { binding: "AI" } }),
    r2_buckets: o.r2Bucket ? [{ binding: "R2", bucket_name: o.r2Bucket }] : [],
    vars: o.vars ?? {},
  };
  const secrets = o.secrets ?? {};
  return {
    account: o.account ?? "acc1",
    vars: config.vars,
    bindings: bindingsOf(config, secrets),
    secrets,
    workersDev: o.workersDev ?? true,
    r2Bucket: o.r2Bucket ?? null,
    deployments: 1,
    version: o.version ?? "0.1.0-dev",
  };
}

export function readCfState(path) {
  if (!existsSync(path)) return defaultCfState();
  return JSON.parse(readFileSync(path, "utf8"));
}

export function writeCfState(path, s) {
  const tmp = `${path}.${process.pid}.tmp`;
  writeFileSync(tmp, JSON.stringify(s, null, 2));
  renameSync(tmp, path);
}

const hex = (n) => randomBytes(n).toString("hex");
const IMPLIES = { [PERMS.scriptsEdit]: [PERMS.scriptsRead], [PERMS.routesEdit]: [PERMS.routesRead], [PERMS.dnsEdit]: [PERMS.dnsRead], [PERMS.accessAppsEdit]: [PERMS.accessAppsRead] };
function hasPerm(perms, need) {
  return perms.includes(need) || perms.some((p) => (IMPLIES[p] ?? []).includes(need));
}

function globToRe(glob) {
  return new RegExp("^" + glob.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*") + "$");
}

const lower = (s) => String(s ?? "").toLowerCase().replace(/\.$/, "");

/** Whether `name` resolves in the fake DNS. */
export function resolves(s, name) {
  const n = lower(name);
  const ov = s.dns?.[n];
  if (ov === "nxdomain" || ov === "servfail" || ov === false) return false;
  if (ov === "resolve" || ov === true) return true;
  if ((s.customDomains ?? []).some((d) => lower(d.hostname) === n)) return true;
  if ((s.dnsRecords ?? []).some((r) => lower(r.name) === n)) return true;
  // Wildcards: the nearest "*.<parent>" (record or override).
  const labels = n.split(".");
  for (let i = 1; i < labels.length - 1; i++) {
    const wild = `*.${labels.slice(i).join(".")}`;
    const wov = s.dns?.[wild];
    if (wov === "nxdomain" || wov === false) return false;
    if (wov === "resolve" || wov === true) return true;
    if ((s.dnsRecords ?? []).some((r) => lower(r.name) === wild)) return true;
  }
  const wd = workersDevTarget(s, n);
  if (wd) return true;
  if (teamAccount(s, n)) return true;
  return false;
}

function workersDevTarget(s, host) {
  const m = /^([a-z0-9-]+)\.([a-z0-9-]+)\.workers\.dev$/.exec(host);
  if (!m) return null;
  const [, w, sub] = m;
  const acct = Object.entries(s.accounts ?? {}).find(([, a]) => a.subdomain === sub)?.[0];
  if (!acct) return null;
  const worker = s.workers?.[w];
  if (!worker || worker.account !== acct || worker.workersDev === false) return null;
  return w;
}

function teamAccount(s, host) {
  return Object.entries(s.access ?? {}).find(([, a]) => a?.org?.auth_domain && lower(a.org.auth_domain) === host)?.[0] ?? null;
}

/** Whether a route pattern ("*.example.com/*", "example.com/*") covers host + path. */
function routeMatches(pattern, host, path) {
  const p = lower(pattern);
  const slash = p.indexOf("/");
  const hp = slash >= 0 ? p.slice(0, slash) : p;
  const pp = slash >= 0 ? p.slice(slash) : "/*";
  const hostOK = hp.startsWith("*.") ? host.endsWith(hp.slice(1)) && host.length > hp.length - 1 : hp.startsWith("*") ? host.endsWith(hp.slice(1)) : host === hp;
  return hostOK && globToRe(pp).test(path);
}

/** The Worker serving host + path: {worker, via} or null. */
export function edgeTarget(s, host, path) {
  const wd = workersDevTarget(s, host);
  if (wd) return { worker: wd, via: "workers.dev" };
  const cd = (s.customDomains ?? []).find((d) => lower(d.hostname) === host);
  if (cd) return { worker: cd.service, via: "custom-domain" };
  const route = (s.routes ?? []).find((r) => routeMatches(r.pattern, host, path));
  if (route && route.script) return { worker: route.script, via: "route" };
  return null;
}

/**
 * The Access application in front of host + path (most specific destination),
 * with its account: {app, account} or null.
 */
export function accessFor(s, host, path) {
  let best = null;
  for (const [account, a] of Object.entries(s.access ?? {})) {
    for (const app of a?.apps ?? []) {
      const uris = [...(app.destinations ?? []).map((d) => d.uri), app.domain].filter(Boolean);
      for (const raw of new Set(uris)) {
        const uri = lower(raw);
        const slash = uri.indexOf("/");
        const uh = slash >= 0 ? uri.slice(0, slash) : uri;
        const up = slash >= 0 ? uri.slice(slash) : "";
        let hostScore;
        if (uh === host) hostScore = 2;
        else if (uh.startsWith("*.") && host.endsWith(uh.slice(1))) hostScore = 1;
        else continue;
        let pathScore;
        if (!up) pathScore = 0;
        else if (up.endsWith("/*")) {
          const prefix = up.slice(0, -1); // "/p/"
          if (!path.startsWith(prefix)) continue;
          pathScore = prefix.length;
        } else {
          if (path !== up) continue;
          pathScore = up.length + 1;
        }
        const score = pathScore * 10 + hostScore;
        if (!best || score > best.score) best = { app, account, score };
      }
    }
  }
  return best;
}

function policyDecision(s, account, app) {
  const policies = (app.policies ?? []).map((ref) => (s.access?.[account]?.policies ?? []).find((p) => p.id === ref.id) ?? ref);
  if (policies.length === 0) return "deny";
  if (policies.some((p) => p.decision === "bypass")) return "bypass";
  if (policies.some((p) => p.decision === "allow")) return "allow";
  return "deny";
}

/**
 * Starts the fake on 127.0.0.1 (a free port).
 * @param {{statePath: string}} o
 */
export async function startFakeCloudflare(o) {
  const statePath = o.statePath;
  if (!existsSync(statePath)) writeCfState(statePath, defaultCfState());
  const requests = [];
  const edgeRequests = [];
  const dohRequests = [];
  const faults = [];

  const read = () => readCfState(statePath);
  const save = (s) => writeCfState(statePath, s);

  const server = createServer((req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      try {
        route(req, res, body);
      } catch (e) {
        res.writeHead(500, { "content-type": "text/plain" });
        res.end(`fake cloudflare crashed: ${e?.stack ?? e}`);
      }
    });
  });

  function send(res, status, obj, headers = {}) {
    res.writeHead(status, { "content-type": "application/json", ...headers });
    res.end(JSON.stringify(obj));
  }

  function route(req, res, raw) {
    const u = new URL(req.url, "http://fake");
    if (u.pathname.startsWith("/client/v4/") || u.pathname === "/client/v4") return api(req, res, u, raw);
    if (u.pathname === "/dns-query") return doh(req, res, u);
    if (u.pathname.startsWith("/__edge/")) return edge(req, res, u, raw);
    res.writeHead(404, { "content-type": "text/plain" });
    res.end("not found\n");
  }

  // ---- REST API -------------------------------------------------------

  const ok = (result, result_info) => ({ success: true, errors: [], messages: [], result, ...(result_info ? { result_info } : {}) });
  /** A handler's answer without a body (no JSON, no content type). */
  const NO_BODY = Symbol("no body");
  const fail = (code, message) => ({ success: false, errors: [{ code, message }], messages: [], result: null });

  function page(items, q) {
    const per = Math.max(1, Math.min(1000, Number(q.get("per_page")) || 20));
    const pg = Math.max(1, Number(q.get("page")) || 1);
    const total = items.length;
    const slice = items.slice((pg - 1) * per, pg * per);
    return ok(slice, { page: pg, per_page: per, count: slice.length, total_count: total, total_pages: Math.max(1, Math.ceil(total / per)) });
  }

  function identify(req, s) {
    const auth = req.headers.authorization ?? "";
    const m = /^Bearer (.+)$/.exec(auth);
    if (m) {
      const t = s.tokens?.[m[1]];
      return t ? { label: t.label ?? "token", permissions: t.permissions ?? [], accounts: t.accounts } : { label: "unknown" };
    }
    const key = req.headers["x-auth-key"];
    if (key) {
      const k = s.apiKeys?.[key];
      return k && k.email === req.headers["x-auth-email"] ? { label: k.label ?? "api-key", permissions: k.permissions ?? [], accounts: k.accounts } : { label: "unknown" };
    }
    return { label: "none" };
  }

  /** [method, regex, permissions needed (any account-scoped check via :a), handler] */
  const R = [];
  const on = (method, re, need, fn) => R.push({ method, re, need, fn });

  function acct(s, a) {
    return s.accounts?.[a] ?? null;
  }
  const noAccount = () => [404, fail(7003, "Could not route to /accounts, perhaps your object identifier is invalid?")];
  const noWorker = () => [404, fail(10007, "This Worker does not exist on your account.")];

  // workers.dev subdomain
  on("GET", /^\/accounts\/([^/]+)\/workers\/subdomain$/, [PERMS.scriptsRead], (s, m) => {
    const a = acct(s, m[1]);
    if (!a) return noAccount();
    if (!a.subdomain) return [404, fail(10007, "This account does not have a workers.dev subdomain.")];
    return [200, ok({ subdomain: a.subdomain })];
  });
  on("PUT", /^\/accounts\/([^/]+)\/workers\/subdomain$/, [PERMS.scriptsEdit], (s, m, body) => {
    const a = acct(s, m[1]);
    if (!a) return noAccount();
    const name = String(body?.subdomain ?? "");
    if (!/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(name)) return [400, fail(10032, "invalid subdomain")];
    const taken = (s.takenSubdomains ?? []).includes(name) || Object.entries(s.accounts).some(([id, x]) => id !== m[1] && x.subdomain === name);
    if (taken) return [409, fail(10031, "Subdomain unavailable.")];
    a.subdomain = name;
    return [200, ok({ subdomain: name }), true];
  });
  on("GET", /^\/accounts\/([^/]+)\/workers\/subdomains\/([^/]+)$/, [PERMS.scriptsRead], (s, m) => {
    if (!acct(s, m[1])) return noAccount();
    const name = m[2];
    const taken = (s.takenSubdomains ?? []).includes(name) || Object.values(s.accounts).some((x) => x.subdomain === name);
    return taken ? [409, fail(10031, "Subdomain unavailable.")] : [404, fail(10032, "Subdomain available.")];
  });

  // live Worker configuration
  on("GET", /^\/accounts\/([^/]+)\/workers\/scripts\/([^/]+)\/settings$/, [PERMS.scriptsRead], (s, m) => {
    if (!acct(s, m[1])) return noAccount();
    const w = s.workers?.[m[2]];
    if (!w || w.account !== m[1] || !w.deployments) return noWorker();
    return [200, ok({ bindings: w.bindings ?? [], compatibility_date: w.compatibilityDate ?? "2026-10-01", logpush: false, tags: [], usage_model: "standard" })];
  });
  on("GET", /^\/accounts\/([^/]+)\/workers\/scripts\/([^/]+)\/subdomain$/, [PERMS.scriptsRead], (s, m) => {
    if (!acct(s, m[1])) return noAccount();
    const w = s.workers?.[m[2]];
    if (!w || w.account !== m[1] || !w.deployments) return noWorker();
    return [200, ok({ enabled: w.workersDev !== false, previews_enabled: false })];
  });

  // zones
  on("GET", /^\/zones$/, [PERMS.zoneRead], (s, m, body, q, who) => {
    const name = q.get("name");
    const account = q.get("account.id");
    const zones = (s.zones ?? []).filter(
      (z) => (!name || lower(z.name) === lower(name)) && (!account || z.account === account) && (!who.accounts || who.accounts.includes(z.account)),
    );
    return [200, page(zones.map((z) => ({ id: z.id, name: z.name, status: z.status ?? "active", account: { id: z.account } })), q)];
  });

  // custom domains
  const zoneById = (s, id) => (s.zones ?? []).find((z) => z.id === id);
  /** Whether a custom domain's flags (enabled, previews_enabled; API defaults true, false) differ from an origin's. */
  const flagsDiffer = (d, o) => (d.enabled ?? true) !== (o.enabled ?? true) || (d.previews_enabled ?? false) !== (o.previews_enabled ?? false);
  on("POST", /^\/accounts\/([^/]+)\/workers\/scripts\/([^/]+)\/domains\/changeset$/, [PERMS.scriptsEdit, PERMS.zoneRead], (s, m, body) => {
    if (!acct(s, m[1])) return noAccount();
    const w = m[2];
    const origins = Array.isArray(body) ? body : [];
    const out = { added: [], removed: [], updated: [], conflicting: [] };
    for (const o of origins) {
      const host = lower(o.hostname);
      const z = zoneById(s, o.zone_id) ?? (s.zones ?? []).find((x) => lower(x.name) === lower(o.zone_name));
      if (!z) return [400, fail(100109, `zone not found for ${host}`)];
      const existing = (s.customDomains ?? []).find((d) => lower(d.hostname) === host);
      const entry = { hostname: host, zone_id: z.id, zone_name: z.name };
      if (existing) out.updated.push({ ...entry, id: existing.id, service: existing.service, environment: existing.environment, modified: existing.service !== w || flagsDiffer(existing, o) });
      else if ((s.dnsRecords ?? []).some((r) => lower(r.name) === host)) out.conflicting.push(entry);
      else out.added.push(entry);
    }
    const wanted = new Set(origins.map((o) => lower(o.hostname)));
    for (const d of s.customDomains ?? []) {
      if (d.service === w && !wanted.has(lower(d.hostname))) out.removed.push({ id: d.id, hostname: d.hostname, zone_id: d.zone_id, zone_name: d.zone_name, service: d.service, environment: d.environment });
    }
    return [200, ok(out)];
  });
  on("PUT", /^\/accounts\/([^/]+)\/workers\/scripts\/([^/]+)\/domains\/records$/, [PERMS.scriptsEdit, PERMS.zoneRead], (s, m, body) => {
    if (!acct(s, m[1])) return noAccount();
    const w = m[2];
    const origins = Array.isArray(body?.origins) ? body.origins : [];
    s.customDomains ??= [];
    for (const o of origins) {
      const host = lower(o.hostname);
      const z = zoneById(s, o.zone_id);
      if (!z || !(host === lower(z.name) || host.endsWith(`.${lower(z.name)}`))) return [400, fail(100109, `${host} is not in zone ${o.zone_id}`)];
      const existing = s.customDomains.find((d) => lower(d.hostname) === host);
      if (existing && existing.service !== w && !body.override_existing_origin) return [409, fail(100116, `Hostname ${host} already has a Custom Domain for a different service`)];
      // Modifying an origin (even this Worker's own, with other flags) needs override_existing_origin.
      if (existing && flagsDiffer(existing, o) && !body.override_existing_origin) return [409, fail(100116, `Hostname ${host} already has a Custom Domain`)];
      if (!existing && (s.dnsRecords ?? []).some((r) => lower(r.name) === host) && !body.override_existing_dns_record) {
        return [409, fail(100117, `Hostname ${host} has existing DNS records`)];
      }
    }
    const wanted = new Set(origins.map((o) => lower(o.hostname)));
    if (body?.override_scope) s.customDomains = s.customDomains.filter((d) => d.service !== w || wanted.has(lower(d.hostname)));
    for (const o of origins) {
      const host = lower(o.hostname);
      const z = zoneById(s, o.zone_id);
      if (body.override_existing_dns_record) s.dnsRecords = (s.dnsRecords ?? []).filter((r) => lower(r.name) !== host);
      const existing = s.customDomains.find((d) => lower(d.hostname) === host);
      if (existing) {
        Object.assign(existing, { service: w, environment: "production" });
        for (const k of ["enabled", "previews_enabled"]) {
          if (o[k] === undefined) delete existing[k];
          else existing[k] = o[k];
        }
      }
      else s.customDomains.push({ id: hex(16), hostname: host, zone_id: z.id, zone_name: z.name, service: w, environment: "production" });
    }
    return [200, ok(s.customDomains.filter((d) => d.service === w)), true];
  });
  on("GET", /^\/accounts\/([^/]+)\/workers\/domains\/records$/, [PERMS.scriptsRead], (s, m, body, q) => {
    if (!acct(s, m[1])) return noAccount();
    const f = (k, v) => !q.get(k) || lower(v) === lower(q.get(k));
    const list = (s.customDomains ?? []).filter((d) => f("service", d.service) && f("environment", d.environment) && f("hostname", d.hostname) && f("zone_id", d.zone_id) && f("zone_name", d.zone_name));
    return [200, page(list, q)];
  });
  on("GET", /^\/accounts\/([^/]+)\/workers\/domains\/records\/([^/]+)$/, [PERMS.scriptsRead], (s, m) => {
    if (!acct(s, m[1])) return noAccount();
    const d = (s.customDomains ?? []).find((x) => x.id === m[2]);
    return d ? [200, ok(d)] : [404, fail(100114, "Domain not found")];
  });
  on("DELETE", /^\/accounts\/([^/]+)\/workers\/domains\/([^/]+)$/, [PERMS.scriptsEdit, PERMS.zoneRead], (s, m) => {
    if (!acct(s, m[1])) return noAccount();
    const before = (s.customDomains ?? []).length;
    s.customDomains = (s.customDomains ?? []).filter((x) => x.id !== m[2]);
    // Like the live API: 200 without a body or content type (no envelope).
    return before === s.customDomains.length ? [404, fail(100114, "Domain not found")] : [200, NO_BODY, true];
  });

  // routes
  const zoneOr404 = (s, id) => zoneById(s, id);
  on("GET", /^\/zones\/([^/]+)\/workers\/routes$/, [PERMS.routesRead], (s, m, body, q) => {
    if (!zoneOr404(s, m[1])) return [404, fail(7003, "zone not found")];
    // Not paginated (like Cloudflare's): every route of the zone.
    const routes = (s.routes ?? []).filter((r) => r.zone_id === m[1]).map((r) => ({ id: r.id, pattern: r.pattern, script: r.script }));
    return [200, ok(routes, { page: 1, per_page: routes.length, count: routes.length, total_count: routes.length, total_pages: 1 })];
  });
  on("POST", /^\/zones\/([^/]+)\/workers\/routes$/, [PERMS.routesEdit], (s, m, body) => {
    if (!zoneOr404(s, m[1])) return [404, fail(7003, "zone not found")];
    s.routes ??= [];
    if (s.routes.some((r) => r.zone_id === m[1] && lower(r.pattern) === lower(body?.pattern))) return [409, fail(10020, "A route with the same pattern already exists.")];
    const r = { id: hex(16), zone_id: m[1], pattern: String(body?.pattern ?? ""), script: String(body?.script ?? "") };
    s.routes.push(r);
    return [200, ok({ id: r.id, pattern: r.pattern, script: r.script }), true];
  });
  on("PUT", /^\/zones\/([^/]+)\/workers\/routes\/([^/]+)$/, [PERMS.routesEdit], (s, m, body) => {
    const r = (s.routes ?? []).find((x) => x.zone_id === m[1] && x.id === m[2]);
    if (!r) return [404, fail(10019, "route not found")];
    if (body?.pattern !== undefined) r.pattern = String(body.pattern);
    if (body?.script !== undefined) r.script = String(body.script);
    return [200, ok({ id: r.id, pattern: r.pattern, script: r.script }), true];
  });
  on("DELETE", /^\/zones\/([^/]+)\/workers\/routes\/([^/]+)$/, [PERMS.routesEdit], (s, m) => {
    const before = (s.routes ?? []).length;
    s.routes = (s.routes ?? []).filter((x) => !(x.zone_id === m[1] && x.id === m[2]));
    return before === s.routes.length ? [404, fail(10019, "route not found")] : [200, ok({ id: m[2] }), true];
  });

  // DNS records
  on("GET", /^\/zones\/([^/]+)\/dns_records$/, [PERMS.dnsRead], (s, m, body, q) => {
    if (!zoneOr404(s, m[1])) return [404, fail(7003, "zone not found")];
    const list = (s.dnsRecords ?? []).filter(
      (r) => r.zone_id === m[1] && (!q.get("type") || r.type === q.get("type")) && (!q.get("name") || lower(r.name) === lower(q.get("name"))),
    );
    return [200, page(list, q)];
  });
  on("POST", /^\/zones\/([^/]+)\/dns_records$/, [PERMS.dnsEdit], (s, m, body) => {
    const z = zoneOr404(s, m[1]);
    if (!z) return [404, fail(7003, "zone not found")];
    const name = lower(body?.name);
    if (!(name === lower(z.name) || name.endsWith(`.${lower(z.name)}`))) return [400, fail(9005, `${name} is not in zone ${z.name}`)];
    s.dnsRecords ??= [];
    if (s.dnsRecords.some((r) => r.zone_id === z.id && lower(r.name) === name && r.type === body?.type && r.content === body?.content)) {
      return [400, fail(81057, "Record already exists.")];
    }
    const rec = { id: hex(16), zone_id: z.id, zone_name: z.name, type: body?.type, name, content: body?.content, proxied: Boolean(body?.proxied), ttl: body?.ttl ?? 1, comment: body?.comment ?? "" };
    s.dnsRecords.push(rec);
    return [200, ok(rec), true];
  });
  on("DELETE", /^\/zones\/([^/]+)\/dns_records\/([^/]+)$/, [PERMS.dnsEdit], (s, m) => {
    const before = (s.dnsRecords ?? []).length;
    s.dnsRecords = (s.dnsRecords ?? []).filter((x) => !(x.zone_id === m[1] && x.id === m[2]));
    return before === s.dnsRecords.length ? [404, fail(81044, "Record does not exist.")] : [200, ok({ id: m[2] }), true];
  });

  // Access
  const accessOf = (s, a) => {
    s.access ??= {};
    s.access[a] ??= { org: null, apps: [], policies: [] };
    s.access[a].apps ??= [];
    s.access[a].policies ??= [];
    return s.access[a];
  };
  const expandApp = (s, a, app) => ({
    ...app,
    policies: (app.policies ?? []).map((ref) => {
      const p = accessOf(s, a).policies.find((x) => x.id === ref.id);
      return p ? { ...p, precedence: ref.precedence } : ref;
    }),
  });
  on("GET", /^\/accounts\/([^/]+)\/access\/organizations$/, [PERMS.accessOrgRead], (s, m) => {
    if (!acct(s, m[1])) return noAccount();
    const org = s.access?.[m[1]]?.org;
    return org ? [200, ok(org)] : [404, fail(12130, "access.api.error.not_found: organization not found")];
  });
  on("GET", /^\/accounts\/([^/]+)\/access\/apps$/, [PERMS.accessAppsRead], (s, m, body, q) => {
    if (!acct(s, m[1])) return noAccount();
    const ax = accessOf(s, m[1]);
    return [200, page(ax.apps.map((app) => expandApp(s, m[1], app)), q)];
  });
  const appBody = (body) => {
    const out = { ...body };
    out.policies = (body?.policies ?? []).map((p) => (typeof p === "string" ? { id: p } : { id: p.id, precedence: p.precedence }));
    return out;
  };
  on("POST", /^\/accounts\/([^/]+)\/access\/apps$/, [PERMS.accessAppsEdit], (s, m, body) => {
    if (!acct(s, m[1])) return noAccount();
    const ax = accessOf(s, m[1]);
    if (!ax.org) return [400, fail(12130, "Zero Trust is not set up on this account")];
    if (!body?.name || !body?.domain) return [400, fail(12000, "name and domain are required")];
    for (const p of body.policies ?? []) if (!ax.policies.some((x) => x.id === (p.id ?? p))) return [400, fail(12000, `policy ${p.id ?? p} not found`)];
    const now = new Date().toISOString();
    const app = { ...appBody(body), id: randomUUID(), uid: randomUUID(), aud: hex(32), created_at: now, updated_at: now };
    ax.apps.push(app);
    return [200, ok(expandApp(s, m[1], app)), true];
  });
  on("GET", /^\/accounts\/([^/]+)\/access\/apps\/([^/]+)$/, [PERMS.accessAppsRead], (s, m) => {
    const app = accessOf(s, m[1]).apps.find((x) => x.id === m[2]);
    return app ? [200, ok(expandApp(s, m[1], app))] : [404, fail(12130, "application not found")];
  });
  on("PUT", /^\/accounts\/([^/]+)\/access\/apps\/([^/]+)$/, [PERMS.accessAppsEdit], (s, m, body) => {
    const ax = accessOf(s, m[1]);
    const app = ax.apps.find((x) => x.id === m[2]);
    if (!app) return [404, fail(12130, "application not found")];
    for (const p of body?.policies ?? []) if (!ax.policies.some((x) => x.id === (p.id ?? p))) return [400, fail(12000, `policy ${p.id ?? p} not found`)];
    const { id, uid, aud, created_at } = app;
    for (const k of Object.keys(app)) delete app[k];
    Object.assign(app, appBody(body), { id, uid, aud, created_at, updated_at: new Date().toISOString() });
    return [200, ok(expandApp(s, m[1], app)), true];
  });
  on("DELETE", /^\/accounts\/([^/]+)\/access\/apps\/([^/]+)$/, [PERMS.accessAppsEdit], (s, m) => {
    const ax = accessOf(s, m[1]);
    const before = ax.apps.length;
    ax.apps = ax.apps.filter((x) => x.id !== m[2]);
    return before === ax.apps.length ? [404, fail(12130, "application not found")] : [200, ok({ id: m[2] }), true];
  });
  on("GET", /^\/accounts\/([^/]+)\/access\/policies$/, [PERMS.accessAppsRead], (s, m, body, q) => {
    if (!acct(s, m[1])) return noAccount();
    return [200, page(accessOf(s, m[1]).policies, q)];
  });
  on("POST", /^\/accounts\/([^/]+)\/access\/policies$/, [PERMS.accessAppsEdit], (s, m, body) => {
    if (!acct(s, m[1])) return noAccount();
    if (!body?.name || !body?.decision) return [400, fail(12000, "name and decision are required")];
    const now = new Date().toISOString();
    const p = { ...body, id: randomUUID(), created_at: now, updated_at: now, reusable: true };
    accessOf(s, m[1]).policies.push(p);
    return [200, ok(p), true];
  });
  on("GET", /^\/accounts\/([^/]+)\/access\/policies\/([^/]+)$/, [PERMS.accessAppsRead], (s, m) => {
    const p = accessOf(s, m[1]).policies.find((x) => x.id === m[2]);
    return p ? [200, ok(p)] : [404, fail(12130, "policy not found")];
  });
  on("PUT", /^\/accounts\/([^/]+)\/access\/policies\/([^/]+)$/, [PERMS.accessAppsEdit], (s, m, body) => {
    const p = accessOf(s, m[1]).policies.find((x) => x.id === m[2]);
    if (!p) return [404, fail(12130, "policy not found")];
    const { id, created_at } = p;
    for (const k of Object.keys(p)) delete p[k];
    Object.assign(p, body, { id, created_at, updated_at: new Date().toISOString(), reusable: true });
    return [200, ok(p), true];
  });
  on("DELETE", /^\/accounts\/([^/]+)\/access\/policies\/([^/]+)$/, [PERMS.accessAppsEdit], (s, m) => {
    const ax = accessOf(s, m[1]);
    if (ax.apps.some((app) => (app.policies ?? []).some((r) => r.id === m[2]))) return [400, fail(12000, "the policy is used by an application")];
    const before = ax.policies.length;
    ax.policies = ax.policies.filter((x) => x.id !== m[2]);
    return before === ax.policies.length ? [404, fail(12130, "policy not found")] : [200, ok({ id: m[2] }), true];
  });

  function api(req, res, u, raw) {
    const path = u.pathname.slice("/client/v4".length) || "/";
    let body = null;
    if (raw) {
      try {
        body = JSON.parse(raw);
      } catch {
        body = raw;
      }
    }
    const s = read();
    const who = identify(req, s);
    const entry = { method: req.method, path, query: Object.fromEntries(u.searchParams), body, token: who.label, status: 0, codes: [] };
    requests.push(entry);
    const reply = (status, obj, headers) => {
      entry.status = status;
      if (obj === NO_BODY) {
        res.writeHead(status, headers ?? {});
        return res.end();
      }
      entry.codes = (obj?.errors ?? []).map((e) => e.code);
      send(res, status, obj, headers);
    };
    const fi = faults.findIndex(
      (f) =>
        (!f.method || f.method === req.method) &&
        (f.path === undefined ||
          (f.path instanceof RegExp ? f.path.test(path) : typeof f.path === "string" && f.path.includes("*") ? globToRe(f.path).test(path) : f.path === path)) &&
        (!f.match || f.match(entry)),
    );
    let fault = null;
    if (fi >= 0) {
      fault = faults[fi];
      fault.times = (fault.times ?? 1) - 1;
      if (fault.times <= 0) faults.splice(fi, 1);
    }
    const replyFault = (f) => {
      const headers = f.retryAfter !== undefined ? { "retry-after": String(f.retryAfter) } : {};
      return reply(f.status ?? 500, f.body ?? fail(f.code ?? 10013, f.message ?? "injected fault"), headers);
    };
    if (fault && !fault.apply) return replyFault(fault);
    const r = R.find((x) => x.method === req.method && x.re.test(path));
    if (!r) return reply(404, fail(7000, `No route for that URI (fake: ${req.method} ${path})`));
    if (!who.permissions) return reply(401, fail(10000, "Authentication error"));
    const m = r.re.exec(path);
    const account = /^\/accounts\/([^/]+)/.exec(path)?.[1];
    if (account && who.accounts && !who.accounts.includes(account)) return reply(403, fail(10000, "Authentication error"));
    if (!r.need.every((p) => hasPerm(who.permissions, p))) return reply(403, fail(10000, "Authentication error"));
    const [status, obj, changed] = r.fn(s, m, body, u.searchParams, who);
    if (changed) save(s);
    // apply: the change happened, but the answer is the fault (only for a request that succeeded).
    if (fault && status < 400) return replyFault(fault);
    return reply(status, obj);
  }

  // ---- DNS over HTTPS ---------------------------------------------------

  function doh(req, res, u) {
    const s = read();
    const name = lower(u.searchParams.get("name"));
    const type = (u.searchParams.get("type") || "A").toUpperCase();
    const ov = s.dns?.[name];
    let status;
    let answer = [];
    if (ov === "servfail") status = 2;
    else if (resolves(s, name)) {
      status = 0;
      answer = [{ name: `${name}.`, type: type === "AAAA" ? 28 : 1, TTL: 300, data: type === "AAAA" ? "100::" : "192.0.2.1" }];
    } else status = 3;
    dohRequests.push({ name, type, status });
    res.writeHead(200, { "content-type": "application/dns-json" });
    res.end(JSON.stringify({ Status: status, TC: false, RD: true, RA: true, Question: [{ name: `${name}.`, type: type === "AAAA" ? 28 : 1 }], ...(answer.length ? { Answer: answer } : {}) }));
  }

  // ---- edge ---------------------------------------------------------------

  function edge(req, res, u, raw) {
    const rest = u.pathname.slice("/__edge/".length);
    const slash = rest.indexOf("/");
    const host = lower(slash >= 0 ? rest.slice(0, slash) : rest);
    const path = slash >= 0 ? rest.slice(slash) : "/";
    const s = read();
    const entry = { method: req.method, host, path, status: 0, location: "" };
    edgeRequests.push(entry);
    const answer = (status, headers, body) => {
      entry.status = status;
      entry.location = headers.location ?? "";
      res.writeHead(status, headers);
      res.end(body);
    };
    const fetchError = (code) => answer(502, { "x-fake-error": code, "content-type": "text/plain" }, `fake edge: ${code}\n`);
    const json = (status, obj) => answer(status, { "content-type": "application/json" }, JSON.stringify(obj));
    const ed = s.edge ?? {};
    if ((ed.down ?? []).includes(host)) return fetchError("ECONNREFUSED");
    if (tlsFails(host, ed.tlsFail ?? [])) return fetchError("ERR_TLS_CERT_ALTNAME_INVALID");

    if (host.endsWith(".cloudflareaccess.com")) {
      if (!teamAccount(s, host)) return fetchError("ENOTFOUND");
      if (path === "/cdn-cgi/access/certs") {
        const keys = ed.certsEmpty ? [] : [{ kid: "fake-kid", kty: "RSA", alg: "RS256", use: "sig", e: "AQAB", n: "fake" }];
        return json(200, { keys, public_cert: keys[0] ? { kid: "fake-kid", cert: "-----BEGIN CERTIFICATE-----" } : undefined, public_certs: [] });
      }
      if (path.startsWith("/cdn-cgi/access/login")) return answer(200, { "content-type": "text/html" }, "<!doctype html><title>Sign in</title>\n");
      return answer(404, { "content-type": "text/plain" }, "not found\n");
    }
    if (!resolves(s, host)) return fetchError("ENOTFOUND");
    const target = edgeTarget(s, host, path);
    if (!target) return answer(200, { "content-type": "text/html" }, "<!doctype html><title>some other website</title>\n");

    if (target.via !== "workers.dev") {
      const a = accessFor(s, host, path);
      if (a) {
        const decision = policyDecision(s, a.account, a.app);
        if (decision === "deny") return answer(403, { "content-type": "text/html" }, "<!doctype html><title>Access denied</title>\n");
        if (decision === "allow" && !req.headers["cf-access-jwt-assertion"]) {
          const team = ed.redirectTeam || s.access?.[a.account]?.org?.auth_domain || "unknown.cloudflareaccess.com";
          const loc = new URL(`https://${team}/cdn-cgi/access/login/${host}`);
          if (!ed.omitKid) loc.searchParams.set("kid", a.app.aud);
          loc.searchParams.set("redirect_url", path + (u.search || ""));
          loc.searchParams.set("meta", "fake");
          return answer(302, { location: loc.href, "content-type": "text/html" }, "");
        }
      }
    }
    return worker(req, s, target.worker, host, path, answer, json);
  }

  function worker(req, s, name, host, path, answer, json) {
    const w = s.workers?.[name];
    if (!w || !w.deployments) return answer(404, { "content-type": "text/plain" }, "There is nothing here yet\n");
    const vars = w.vars ?? {};
    const secrets = w.secrets ?? {};
    const mode = String(vars.AUTH_MODE ?? "none").trim().toLowerCase() || "none";
    const ed = s.edge ?? {};
    if (path === "/__backyard/healthz" || /^\/s\/[^/]+\/__backyard\/healthz$/.test(path)) return answer(200, { "content-type": "text/plain" }, "ok\n");
    if (path === "/__backyard/readyz" || /^\/s\/[^/]+\/__backyard\/readyz$/.test(path)) {
      const status = secrets.BACKYARD_SECRET ? (ed.readyz ?? 200) : 503;
      const reason = secrets.BACKYARD_SECRET ? (ed.readyzBody ?? "not ready\n") : "not ready: BACKYARD_SECRET is not set\n";
      return answer(status, { "content-type": "text/plain" }, status === 200 ? "ok\n" : reason);
    }
    const base = String(vars.BASE_DOMAIN ?? "") || host;
    const publicUrl = vars.PUBLIC_URL || `https://${base}`;
    const mcp = !ed.preConnector && /^(true|1|yes|on)$/i.test(String(vars.MCP_ENABLED ?? "").trim());
    if (path === "/__backyard/api/deploy/info") {
      return json(200, {
        name: vars.SERVER_NAME || "Backyard",
        version: ed.workerVersion ?? w.version ?? "0.1.0-dev",
        public_url: publicUrl,
        base_domain: base,
        url_mode: String(vars.URL_MODE ?? "path") === "host" ? "host" : "path",
        auth: { mode, anonymous_deploy: false },
        methods: ["api"],
        edition: "cloudflare",
        ...(mcp ? { mcp_url: `${publicUrl}/__backyard/mcp` } : {}),
      });
    }
    // The Claude connector's machine paths: answered before any sign-in, like the real Worker.
    const wellKnown = path === "/.well-known/oauth-authorization-server" || path === "/.well-known/oauth-protected-resource/__backyard/mcp";
    if (path === "/__backyard/mcp" || path.startsWith("/__backyard/oauth/") || (wellKnown && !ed.preConnector)) {
      const off = () => json(404, { error: { code: "not_found", message: ed.preConnector ? `no such endpoint: ${path}` : "the Claude connector is not enabled on this server" } });
      if (!mcp) return off();
      if (path === "/__backyard/mcp") {
        const challenge = `Bearer resource_metadata="${publicUrl}/.well-known/oauth-protected-resource/__backyard/mcp", scope="mcp"`;
        return answer(401, { "content-type": "application/json", "www-authenticate": challenge }, JSON.stringify({ error: { code: "unauthenticated", message: "connect this app to Backyard to sign in" } }));
      }
      if (["/__backyard/oauth/token", "/__backyard/oauth/register", "/__backyard/oauth/revoke"].includes(path)) {
        if (req.method === "POST") return json(400, { error: "invalid_request", error_description: "the fake Worker does not run OAuth" });
        return answer(405, { "content-type": "application/json", allow: "POST" }, JSON.stringify({ error: { code: "bad_request", message: "method not allowed" } }));
      }
      if (path === "/.well-known/oauth-authorization-server") return json(200, { issuer: publicUrl, authorization_endpoint: `${publicUrl}/__backyard/auth/authorize`, token_endpoint: `${publicUrl}/__backyard/oauth/token` });
      if (path === "/.well-known/oauth-protected-resource/__backyard/mcp") return json(200, { resource: `${publicUrl}/__backyard/mcp`, authorization_servers: [publicUrl], scopes_supported: ["mcp"] });
      return json(404, { error: { code: "not_found", message: `no such endpoint: ${path}` } });
    }
    if (path === "/__backyard/api/deploy/me") {
      const bearer = /^Bearer (.+)$/.exec(req.headers.authorization ?? "")?.[1];
      if (bearer && secrets.DEPLOY_TOKEN && bearer === secrets.DEPLOY_TOKEN) return json(200, { user: { id: "deploy-token", kind: "token", name: "deploy token" } });
      // Like the real gateway: AUTH_MODE=none gives a caller without a token the guest identity.
      if (!bearer && mode === "none") return json(200, { id: "anon:6b9d602b5f33deb6", name: "Guest 6b9d", anonymous: true, admin: false });
      return json(401, { error: { code: "unauthenticated", message: "a deploy token is needed" } });
    }
    if (path.startsWith("/__backyard/api/tokens")) return json(401, { error: { code: "unauthenticated", message: "sign in first" } });
    if (mode === "access" && !req.headers["cf-access-jwt-assertion"]) {
      return path.includes("/__backyard/")
        ? json(401, { error: { code: "unauthenticated", message: "Cloudflare Access sign-in required" } })
        : answer(401, { "content-type": "text/html" }, "<!doctype html><title>Sign in with Cloudflare Access</title>\n");
    }
    return answer(200, { "content-type": "text/html" }, `<!doctype html><title>Backyard</title><!-- worker ${name} -->\n`);
  }

  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const origin = `http://127.0.0.1:${server.address().port}`;
  return {
    origin,
    apiBase: `${origin}/client/v4`,
    dohUrl: `${origin}/dns-query`,
    statePath,
    requests,
    edgeRequests,
    dohRequests,
    /** @param {{method?: string, path?: string|RegExp, match?: (req: {method: string, path: string, query: Record<string, string>, body: any, token: string}) => boolean, status?: number, code?: number, message?: string, times?: number, retryAfter?: number, body?: any, apply?: boolean}} f */
    addFault(f) {
      faults.push({ times: 1, ...f });
    },
    clearFaults() {
      faults.length = 0;
    },
    /** The API requests that are not GETs. */
    writes() {
      return requests.filter((r) => r.method !== "GET");
    },
    state: read,
    /** @param {(s: Record<string, any>) => void} fn */
    update(fn) {
      const s = read();
      fn(s);
      save(s);
    },
    close() {
      return new Promise((r) => {
        server.closeAllConnections?.();
        server.close(() => r());
      });
    },
  };
}
