// `backyard-cloudflare status` (design §3.6): a read-only live check of the
// Worker, its workers.dev URL, the custom domain (route, wildcard DNS and the
// certificate in host mode) and Cloudflare Access.
//
// It takes no lock, never prompts, never writes the state and never runs
// `wrangler login`: without a login (or with several accounts and no
// CLOUDFLARE_ACCOUNT_ID) the Cloudflare API checks are skipped, and said so;
// the edge checks need no credentials and still run. A machine without state
// shows the live configuration (adopted in memory only).
//
// Each check is ok, pending or failed, with a detail. Check names: worker,
// workers-dev, hub, custom-domain, route, dns, tls, access-redirect,
// access-bypass, access-connector (the Claude connector's four bypass paths:
// failed once /info has mcp_url, pending before), workers-dev-closed,
// deploy-token. --json prints
//
//   {"schema": 1, "worker", "accountId", "statePath",
//    "versions": {"package", "wrangler", "deployed"},
//    "urls": {"public", "workersDev"}, "r2Bucket", "workersDev",
//    "ai": {"enabled", "binding"},   (AI_ENABLED as the Worker reads it; the Workers AI binding)
//    "siteListing": "all"|"owners"|"invalid",   (SITE_LISTING as the Worker reads it; invalid:
//                                    the Worker refuses every request)
//    "domain": {…state.domain…} | null, "access": {"team", "aud", "source"} | null,
//    "checks": [{"name", "status": "ok"|"pending"|"failed", "detail"}]}
//
// Exit codes: 0 when every check is ok, 4 when one is pending or failed, 1
// when the Worker does not exist (or on a hard error).

import { accessEdgeFailures } from "./access.mjs";
import { CfError } from "./lib/cfapi.mjs";
import { adopt, aiEnabledOf, authModeOf, hostOf, normalizeTeam, siteListingOf } from "./lib/live.mjs";
import { dohResolves, parseAccessRedirect, probe, randomProbeLabel } from "./lib/net.mjs";
import { CliError, EXIT, bold, dim, green, maskSecret, red, ui, yellow } from "./lib/ui.mjs";

/** The order of the checks in the output. */
export const CHECK_NAMES = ["worker", "workers-dev", "hub", "custom-domain", "route", "dns", "tls", "access-redirect", "access-bypass", "access-connector", "workers-dev-closed", "deploy-token"];

const lower = (s) => String(s ?? "").trim().toLowerCase();
const trimUrl = (u) => String(u ?? "").trim().replace(/\/+$/, "");

/** A short description of a failed probe. */
function failText(r) {
  const what = r.kind === "certificate" ? "TLS certificate error" : r.kind === "dns" ? "the name does not resolve" : "network error";
  return `${what}${r.error ? `: ${r.error}` : ""}`;
}

/**
 * Logs in only if a login already exists: never the browser, never a prompt
 * (ctx.login with {browser: false}). Returns "" when logged in (ctx.accountId
 * set), else why the live checks are skipped.
 * @param {import("./cli.mjs").Ctx} ctx
 * @param {Record<string, any>} state   not saved: status never writes it
 */
async function quietLogin(ctx, state) {
  try {
    return (await ctx.login(state, { browser: false })) ? "" : "not logged in to Cloudflare";
  } catch (e) {
    if (e instanceof CliError) return e.message;
    throw e;
  }
}

/** The zone of a host: state.domain.zoneId, else the longest suffix that is a zone of the account. */
async function zoneOf(api, accountId, domain) {
  if (domain.zoneId) return domain.zoneId;
  const labels = lower(domain.host).split(".");
  for (let i = 0; i <= labels.length - 2; i++) {
    const name = labels.slice(i).join(".");
    const zones = await api.list("/zones", { name, "account.id": accountId });
    if (zones[0]?.id) return zones[0].id;
  }
  return "";
}

/**
 * `status`.
 * @param {Record<string, any>} opts   parsed flags (lib/args.mjs): json, workerName
 * @param {import("./cli.mjs").Ctx} ctx
 * @returns {Promise<number | void>}
 */
export async function runStatus(opts, ctx) {
  const inv = ctx.invokedAs;
  const w = ctx.worker;
  const state = ctx.loadState();
  /** What this report describes: the state, or (no state on this machine) the live Worker adopted in memory. */
  const view = structuredClone(state);

  const skipped = await quietLogin(ctx, state);
  const loggedIn = skipped === "";
  let live = null;
  let api = null;
  if (loggedIn) {
    api = await ctx.api();
    live = await ctx.readLive();
    if (live.exists && !state.updatedAt) adopt(view, live, ctx.base.vars ?? {});
    if (!view.workersDevUrl && view.workersDev !== false && live.exists !== false) {
      try {
        const sub = (await api.get(`/accounts/${ctx.accountId}/workers/subdomain`))?.subdomain;
        if (sub) view.workersDevUrl = `https://${w}.${sub}.workers.dev`;
      } catch {
        /* unknown: the workers.dev checks are skipped */
      }
    }
  }

  // Externally managed (--var, e.g. the Terraform module): the variables say it.
  const vars = view.vars ?? {};
  let domain = view.domain ?? null;
  if (!domain && hostOf(vars.BASE_DOMAIN)) {
    domain = { host: hostOf(vars.BASE_DOMAIN), mode: lower(vars.URL_MODE) === "host" ? "host" : "path", status: "active", source: "external" };
  }
  let access = view.access ? { team: normalizeTeam(view.access.team), aud: lower(view.access.aud), source: view.access.source ?? "" } : null;
  if (!access && authModeOf(vars.AUTH_MODE) === "access") access = { team: normalizeTeam(vars.ACCESS_TEAM_DOMAIN), aud: lower(vars.ACCESS_AUD), source: "external" };
  if (!access && live?.exists && live.authMode === "access" && state.updatedAt) access = { team: live.team, aud: lower(live.aud), source: "live" };

  const workersDevUrl = trimUrl(view.workersDevUrl);
  const publicUrl = trimUrl(view.url) || (domain ? `https://${domain.host}` : "") || workersDevUrl;
  const workersDevOn = live?.workersDev ?? (view.workersDev !== false);
  const token = process.env.DEPLOY_TOKEN || view.deployToken || "";
  const mode = domain?.mode === "host" ? "host" : "path";

  // The checks run concurrently; each returns {name, status, detail} or null (not applicable).
  const infoP = publicUrl ? probe(`${publicUrl}/__backyard/api/deploy/info`, { accept: "application/json" }) : Promise.resolve(null);
  const accessP = access && domain ? accessEdgeFailures({ host: domain.host, mode, team: access.team, aud: access.aud, worker: w }) : Promise.resolve(null);
  const check = (name, status, detail) => ({ name, status, detail });

  const checks = {
    async worker() {
      const info = await infoP;
      const version = info?.ok && info.status === 200 ? String(info.json?.version ?? "") : "";
      if (!loggedIn) return check("worker", "pending", `Cloudflare checks skipped: ${skipped} (log in with ${inv} setup, or set CLOUDFLARE_API_TOKEN and CLOUDFLARE_ACCOUNT_ID)`);
      if (live.exists === true) return check("worker", "ok", `deployed${version ? ` (version ${version})` : ""}`);
      if (live.exists === false) return check("worker", "failed", `Worker ${w} is not deployed on account ${ctx.accountId}: run ${inv} setup`);
      return check("worker", "failed", `could not read the Worker's settings: ${live.error || "unknown error"}`);
    },
    async "workers-dev"() {
      const want = view.workersDev !== false;
      const actual = live?.workersDev ?? null;
      if (actual !== null && actual !== want) {
        return check("workers-dev", "failed", `workers.dev is ${actual ? "on" : "off"}, but this machine's state says ${want ? "on" : "off"}: re-run ${inv} setup`);
      }
      if (!(actual ?? want)) return check("workers-dev", "ok", "off");
      if (!workersDevUrl) return actual === null ? null : check("workers-dev", "ok", "on (URL not known on this machine)");
      const r = await probe(`${workersDevUrl}/__backyard/healthz`);
      if (r.ok && r.status === 200) return check("workers-dev", "ok", `on: ${workersDevUrl}`);
      if (!r.ok && r.kind === "dns") return check("workers-dev", "pending", `${workersDevUrl} does not resolve yet (a new workers.dev subdomain takes a few minutes)`);
      return check("workers-dev", "failed", `${workersDevUrl}/__backyard/healthz ${r.ok ? `answers HTTP ${r.status}` : `does not answer (${failText(r)})`}`);
    },
    async hub() {
      if (!publicUrl) return check("hub", "pending", `no hub URL known on this machine: run ${inv} setup`);
      const r = await probe(`${publicUrl}/__backyard/readyz`);
      if (r.ok && r.status === 200) return check("hub", "ok", `${publicUrl} is ready`);
      if (!r.ok) {
        const pending = r.kind === "certificate" || r.kind === "dns";
        return check("hub", pending ? "pending" : "failed", `${publicUrl} does not answer (${failText(r)})${pending ? ": certificate or DNS still provisioning?" : ""}`);
      }
      if (r.status >= 300 && r.status < 400 && parseAccessRedirect(r.location)) {
        return check("hub", "failed", `${publicUrl}/__backyard/readyz is behind Cloudflare Access: the bypass application is missing ${new URL(publicUrl).host}/__backyard/readyz`);
      }
      return check("hub", "failed", `${publicUrl}/__backyard/readyz answers HTTP ${r.status}${r.status === 503 ? " (not ready: re-run setup)" : ""}`);
    },
    async "custom-domain"() {
      if (!domain || !loggedIn || live?.exists === false) return null;
      try {
        const list = await api.list(`/accounts/${ctx.accountId}/workers/domains/records`, { service: w, environment: "production" });
        const mine = list.find((d) => lower(d.hostname) === lower(domain.host));
        return mine
          ? check("custom-domain", "ok", `${domain.host} → Worker ${w}`)
          : check("custom-domain", "failed", `${domain.host} is not a custom domain of Worker ${w}: re-run ${inv} domain add ${domain.host}${mode === "host" ? " --host-mode" : ""}`);
      } catch (e) {
        return check("custom-domain", "failed", `could not list the custom domains: ${e instanceof CfError ? e.message : e}`);
      }
    },
    async route() {
      if (!domain || mode !== "host" || !loggedIn || live?.exists === false) return null;
      const pattern = `*.${lower(domain.host)}/*`;
      try {
        const zone = await zoneOf(api, ctx.accountId, domain);
        if (!zone) return check("route", "failed", `no Cloudflare zone of this account holds ${domain.host}`);
        const routes = await api.get(`/zones/${zone}/workers/routes`);
        const r = (Array.isArray(routes) ? routes : []).find((x) => lower(x.pattern) === pattern);
        if (!r) return check("route", "failed", `no route ${pattern}: re-run ${inv} domain add ${domain.host} --host-mode`);
        if (r.script !== w) return check("route", "failed", `the route ${pattern} belongs to Worker ${r.script || "(none)"}, not ${w}`);
        return check("route", "ok", `${pattern} → Worker ${w}`);
      } catch (e) {
        return check("route", "failed", `could not read the routes: ${e instanceof CfError ? e.message : e}`);
      }
    },
    async dns() {
      if (!domain || mode !== "host") return null;
      return (await dohResolves(`${randomProbeLabel()}.${domain.host}`))
        ? check("dns", "ok", `*.${domain.host} resolves`)
        : check("dns", "pending", `*.${domain.host} does not resolve yet (a proxied AAAA record *.${domain.host} → 100::)`);
    },
    async tls() {
      if (!domain) return null;
      const targets = [[domain.host, domain.host]];
      if (mode === "host") targets.push([`${randomProbeLabel()}.${domain.host}`, `*.${domain.host}`]);
      const results = await Promise.all(targets.map(([h]) => probe(`https://${h}/__backyard/healthz`)));
      const problems = [];
      let failed = false;
      for (const [i, r] of results.entries()) {
        const label = targets[i][1];
        if (r.ok) continue;
        if (r.kind === "certificate") problems.push(`the certificate does not cover ${label} yet${label.startsWith("*.") ? " (below the zone apex this needs an Advanced Certificate)" : ""}`);
        else if (r.kind === "dns") problems.push(`${label} does not resolve yet`);
        else {
          failed = true;
          problems.push(`${label}: ${failText(r)}`);
        }
      }
      if (!problems.length) return check("tls", "ok", `HTTPS works for ${targets.map((t) => t[1]).join(" and ")}`);
      return check("tls", failed ? "failed" : "pending", problems.join("; "));
    },
    async "access-redirect"() {
      if (!access) return null;
      if (!domain) return check("access-redirect", "failed", "AUTH_MODE=access, but there is no custom domain for Cloudflare Access to protect");
      const f = await accessP;
      const failures = [...f.certs, ...f.redirect];
      return failures.length ? check("access-redirect", "failed", failures.join("; ")) : check("access-redirect", "ok", `login at ${access.team}, AUD ${access.aud}`);
    },
    async "access-bypass"() {
      if (!access || !domain) return null;
      const f = await accessP;
      return f.bypass.length ? check("access-bypass", "failed", f.bypass.join("; ")) : check("access-bypass", "ok", "the machine paths reach the Worker");
    },
    async "access-connector"() {
      if (!access || !domain) return null;
      const f = await accessP;
      if (!f.connector.length) return check("access-connector", "ok", "the Claude connector's paths reach the Worker");
      // Not a failure until the connector is on (an installation from before it, SPEC §8.4).
      const info = await infoP;
      const on = Boolean(info?.ok && info.status === 200 && info.json?.mcp_url);
      if (on) return check("access-connector", "failed", f.connector.join("; "));
      return check(
        "access-connector",
        "pending",
        `the Claude connector paths are not bypassed yet: run ${inv} access enable (with BACKYARD_CF_API_TOKEN when it made the bypass application, or add the paths in the dashboard) before setting MCP_ENABLED (${f.connector.join("; ")})`,
      );
    },
    async "workers-dev-closed"() {
      if (!access || !workersDevOn || !workersDevUrl) return null;
      // The Worker verifies the Access JWT itself, so workers.dev is no back door.
      const r = await probe(`${workersDevUrl}/`, { accept: "text/html" });
      if (r.ok && r.status === 401) return check("workers-dev-closed", "ok", `${workersDevUrl} answers 401 without a sign-in`);
      return check("workers-dev-closed", "failed", `${workersDevUrl}/ ${r.ok ? `answers HTTP ${r.status} without a sign-in (expected 401)` : `does not answer (${failText(r)})`}`);
    },
    async "deploy-token"() {
      if (!token || !publicUrl) return null;
      const r = await probe(`${publicUrl}/__backyard/api/deploy/me`, { accept: "application/json", headers: { Authorization: `Bearer ${token}` } });
      if (r.ok && r.status === 200) return check("deploy-token", "ok", `accepted (${maskSecret(token)})`);
      if (!r.ok) return check("deploy-token", "failed", `${publicUrl}/__backyard/api/deploy/me does not answer (${failText(r)})`);
      if (r.status >= 300 && r.status < 400 && parseAccessRedirect(r.location)) {
        return check("deploy-token", "failed", `${maskSecret(token)} cannot reach the Worker: the bypass application is missing ${new URL(publicUrl).host}/__backyard/api/deploy/*`);
      }
      return check("deploy-token", "failed", `${maskSecret(token)} is not accepted (HTTP ${r.status}): the Worker has another DEPLOY_TOKEN (DEPLOY_TOKEN=<token> ${inv} setup remembers it)`);
    },
  };
  const results = (await Promise.all(CHECK_NAMES.map((name) => checks[name]()))).filter(Boolean);
  const info = await infoP;
  const deployedVersion = info?.ok && info.status === 200 && info.json?.edition === "cloudflare" ? String(info.json.version ?? "") : "";

  const report = {
    schema: 1,
    worker: w,
    accountId: ctx.accountId || state.accountId || "",
    statePath: ctx.statePath,
    versions: { package: ctx.layout.version, wrangler: ctx.wranglerVersion, deployed: deployedVersion },
    urls: { public: publicUrl, workersDev: workersDevUrl },
    r2Bucket: live?.exists ? live.r2Bucket : view.r2Bucket ?? "",
    workersDev: live?.workersDev ?? (view.workersDev ?? null),
    ai: live?.exists
      ? { enabled: aiEnabledOf(live.vars.AI_ENABLED), binding: live.ai }
      : { enabled: view.ai !== false && aiEnabledOf(vars.AI_ENABLED), binding: view.ai !== false },
    siteListing: (live?.exists ? siteListingOf(live.vars.SITE_LISTING) : view.siteListing === "owners" ? "owners" : siteListingOf(vars.SITE_LISTING)) ?? "invalid",
    domain,
    access,
    checks: results,
  };
  const code = live?.exists === false ? EXIT.FAILED : results.every((c) => c.status === "ok") ? EXIT.OK : EXIT.PENDING;

  if (opts.json) {
    process.stdout.write(JSON.stringify(report, null, 2) + "\n");
    return code;
  }
  printReport(report, { ctx, state, loggedIn, skipped, token });
  return code;
}

/** The human output of status. */
function printReport(report, o) {
  const { ctx, state } = o;
  const row = (label, value) => ui.log(`${label.padEnd(14)}${value}`);
  ui.log(bold(`\nBackyard on Cloudflare — status of ${report.worker}\n`));
  row("State:", `${report.statePath}${state.updatedAt ? "" : dim(" (none on this machine: showing the live configuration)")}`);
  row("Account:", report.accountId ? (ctx.accountName ? `${ctx.accountName} (${report.accountId})` : report.accountId) : "unknown");
  if (!o.loggedIn) ui.log(yellow(`              live Cloudflare checks skipped: ${o.skipped}`));
  row("Versions:", `backyard-cloudflare ${report.versions.package} · wrangler ${report.versions.wrangler || "not installed"} · deployed ${report.versions.deployed || "unknown"}`);
  row("Hub:", report.urls.public || "unknown");
  row("workers.dev:", `${report.workersDev === null ? "unknown" : report.workersDev ? "on" : "off"}${report.urls.workersDev ? ` (${report.urls.workersDev})` : ""}`);
  row("R2:", report.r2Bucket ? `bucket ${report.r2Bucket}` : "none (files in Durable Objects, up to 25 MiB each)");
  const ai = report.ai;
  row(
    "AI:",
    ai.enabled
      ? ai.binding
        ? "on (Workers AI)"
        : "on, without the Workers AI binding (only Anthropic or OpenAI aliases work)"
      : ai.binding
        ? "off (AI_ENABLED=false; the Workers AI binding stays)"
        : `off (${ctx.invokedAs} setup --ai turns it on)`,
  );
  row(
    "Site listing:",
    report.siteListing === "owners"
      ? `owners only: each site is listed to its owners and admins (${ctx.invokedAs} setup --site-listing all lists every site)`
      : report.siteListing === "all"
        ? "every site"
        : `SITE_LISTING is neither all nor owners: the Worker refuses every request (${ctx.invokedAs} setup --site-listing <all|owners>)`,
  );
  const d = report.domain;
  row("Domain:", d ? `${d.host} (${d.mode} mode, ${d.status ?? "unknown"}${d.source ? `, ${d.source}` : ""})` : `none (${ctx.invokedAs} domain add <host>)`);
  const a = report.access;
  row("Access:", a ? `team ${a.team || "unknown"} · AUD ${a.aud || "unknown"}${a.source ? ` (${a.source})` : ""}` : "off");
  row("Deploy token:", o.token ? maskSecret(o.token) : "not known on this machine");
  ui.log("");
  for (const c of report.checks) {
    const mark = c.status === "ok" ? green("ok     ") : c.status === "pending" ? yellow("pending") : red("failed ");
    ui.log(`  ${mark}  ${c.name.padEnd(20)}${c.detail}`);
  }
  const bad = report.checks.filter((c) => c.status !== "ok").length;
  ui.log(bad ? `\n${bad} check${bad === 1 ? "" : "s"} need${bad === 1 ? "s" : ""} attention.\n` : `\n${green("All checks passed.")}\n`);
}
