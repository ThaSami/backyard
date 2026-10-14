// `backyard-cloudflare domain add <host>` and `domain remove` (design §3.2, §3.3).
//
//   domain add backyard.acme.com              the hub (and sites at /s/<site>/) on your own domain
//   domain add acme-sites.dev --host-mode     sites at https://<site>.acme-sites.dev/
//   domain remove                             back to workers.dev
//
// domain add attaches <host> to the Worker as a Workers Custom Domain
// (Cloudflare adds its DNS record and certificate) with Backyard's own REST
// calls, after a preflight that fails closed: another Worker's custom domain
// needs --take-over, existing DNS records at <host> need --replace-dns-record,
// and any failed check stops before anything is attached. Deploys never carry
// routes, so this is the only place domains change. --host-mode also adds the
// route *.<host>/* and a proxied wildcard record AAAA *.<host> 100:: (created
// with the admin token BACKYARD_CF_API_TOKEN, or added by hand from printed
// steps and detected through DNS-over-HTTPS), and switches URL_MODE to host
// once a site host answers over TLS. Then it deploys with BASE_DOMAIN=<host>
// and waits for https://<host>/__backyard/readyz. Re-running the same command
// changes nothing on Cloudflare: it redeploys and re-verifies (exit 4 means
// "applied, verification pending": re-run it later).
//
// domain remove deploys without the domain first (links switch back to
// workers.dev), then deletes the route, the wildcard record this tool created
// (only with the admin token) and detaches the custom domain. Objects of other
// Workers and DNS records this tool did not create are never touched.
//
// Both run with the context of scripts/cli.mjs (Ctx): the Worker's lock is
// already taken; the admin token is never stored, printed or given to wrangler.

import { CfError } from "./lib/cfapi.mjs";
import { MANAGED_DOMAIN_KEYS, authGuard } from "./lib/live.mjs";
import { dohResolves, parseAccessRedirect, probe, randomProbeLabel, waitFor } from "./lib/net.mjs";
import { CliError, EXIT, bold, cyan, dim, green, ui } from "./lib/ui.mjs";

/** --wait default (seconds) for DNS, the certificate and readiness. */
export const DEFAULT_WAIT_SEC = 300;
/** Seconds between two checks while waiting. */
const POLL_SEC = 5;
/** The API-token permission that lets this tool create the wildcard record. */
const DNS_PERMISSION = "Zone · DNS · Edit";

const LABEL_RE = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;
const TLD_RE = /^(?:[a-z]{2,63}|xn--[a-z0-9-]{1,59})$/;

const lower = (s) => String(s ?? "").trim().toLowerCase().replace(/\.$/, "");
const errText = (e) => e?.message ?? String(e);

/**
 * Normalizes and validates a host name: lowercased, without a trailing dot,
 * at least two labels, no wildcard, scheme, port or path, and not a
 * workers.dev name. Throws CliError(2) otherwise.
 * @param {unknown} raw
 * @param {string} [what]   how the value is named in errors ("host", "--zone")
 * @returns {string}
 */
export function parseHost(raw, what = "host") {
  const shown = JSON.stringify(String(raw ?? ""));
  const s = lower(raw);
  const bad = (why) => new CliError(EXIT.USAGE, `invalid ${what} ${shown}: ${why}`);
  if (s === "") throw bad("it is empty");
  if (/^[a-z][a-z0-9+.-]*:\/\//.test(s)) throw bad("give the host name only, without a scheme (e.g. backyard.acme.com)");
  if (s.includes("/")) throw bad("give the host name only, without a path");
  if (s.includes(":")) throw bad("give the host name only, without a port");
  if (s.includes("*")) throw bad(what === "host" ? "no wildcards (--host-mode serves the sites at *.<host>)" : "no wildcards");
  const labels = s.split(".");
  if (labels.length < 2) throw bad("a host name has at least two labels (e.g. backyard.acme.com)");
  if (s.length > 253 || !labels.every((l) => LABEL_RE.test(l)) || !TLD_RE.test(labels[labels.length - 1])) throw bad("not a valid host name");
  if (s === "workers.dev" || s.endsWith(".workers.dev")) {
    throw bad("workers.dev names belong to Cloudflare: Backyard already serves there, domain add is for a domain of your own");
  }
  return s;
}

/** The zones that may hold `host`, longest first, with at least two labels. */
export function zoneCandidates(host) {
  const labels = host.split(".");
  const out = [];
  for (let i = 0; i <= labels.length - 2; i++) out.push(labels.slice(i).join("."));
  return out;
}

/**
 * The domain to add to Cloudflare for `host` (the hint when no zone holds
 * it): the last two labels, three for second-level suffixes such as co.uk.
 */
export function registrableDomain(host) {
  const l = host.split(".");
  const n = l.length >= 3 && /^(?:co|com|net|org|gov|edu|ac|ne|or|go)$/.test(l[l.length - 2]) && l[l.length - 1].length === 2 ? 3 : 2;
  return l.slice(-n).join(".");
}

/** The wildcard record's name as the dashboard asks for it: relative to the zone, "*" at the apex. */
export function dnsRecordName(host, zoneName) {
  if (host === zoneName) return "*";
  if (zoneName && host.endsWith(`.${zoneName}`)) return `*.${host.slice(0, host.length - zoneName.length - 1)}`;
  return `*.${host}`;
}

/** A Cloudflare failure as CliError(1), naming what was already applied. */
function failed(what, e, applied = []) {
  if (e instanceof CliError) return e;
  return new CliError(EXIT.FAILED, `${what}: ${errText(e)}`, { applied: [...applied], cause: e });
}

/** A confirmation answered No: exit 3 like every refusal that changed nothing (as in access enable/disable). */
const cancelled = () => new CliError(EXIT.REFUSED, "not confirmed: nothing was changed", { reason: "declined" });

/** The auth guard's remedy (as for every deploy). */
function guardRemedy(ctx) {
  return (
    `  If another machine changed it: delete ${ctx.statePath} to adopt the live configuration, then re-run.\n` +
    `  If this is deliberate: ${ctx.invokedAs} setup --confirm-auth-change (or ${ctx.invokedAs} access disable to turn Access off).`
  );
}

/** BASE_DOMAIN / URL_MODE set with --var (externally managed, e.g. by the Terraform module). */
function externalDomainKeys(state) {
  return MANAGED_DOMAIN_KEYS.filter((k) => state.vars && Object.hasOwn(state.vars, k));
}

function externalHint(keys, inv) {
  const them = keys.length > 1 ? "them" : "it";
  return (
    `${keys.join(" and ")} ${keys.length > 1 ? "are" : "is"} set with --var (e.g. by the Terraform module): ` +
    `drop ${them} with \`${inv} setup ${keys.map((k) => `--var ${k}:`).join(" ")}\`, or keep managing ${them} with Terraform`
  );
}

/** Fills state.workersDevUrl from the account's subdomain when it is unknown (best effort). */
async function fillWorkersDevUrl(ctx, api, state) {
  if (state.workersDev === false || state.workersDevUrl) return;
  try {
    const sub = (await api.get(`/accounts/${ctx.accountId}/workers/subdomain`))?.subdomain;
    if (sub) state.workersDevUrl = `https://${ctx.worker}.${sub}.workers.dev`;
  } catch {
    /* unknown: the result file then has no workers.dev URL */
  }
}

/** Progress lines while waiting, at most one every 30 seconds. */
function progress(what) {
  let shown = -1;
  return (_r, elapsed) => {
    const step = Math.floor(elapsed / 30);
    if (step === shown) return;
    shown = step;
    ui.note(`  waiting for ${what}… (${elapsed} s)`);
  };
}

/**
 * The zone of `host` in the account: `--zone`, else the longest suffix that is
 * a zone (GETs only). Exits 1 when there is none or it is not active yet.
 * @returns {Promise<{id: string, name: string}>}
 */
async function findZone(ctx, api, host, zoneFlag) {
  const a = ctx.accountId;
  let zone = null;
  for (const name of zoneFlag ? [zoneFlag] : zoneCandidates(host)) {
    let list;
    try {
      list = await api.get("/zones", { name, "account.id": a });
    } catch (e) {
      throw failed(`could not look up the Cloudflare zone of ${host} (nothing was changed)`, e);
    }
    zone = (Array.isArray(list) ? list : []).find((z) => lower(z?.name) === name) ?? null;
    if (zone) break;
  }
  if (!zone) {
    const what = zoneFlag ? `the zone ${zoneFlag} is not in this Cloudflare account` : `no zone of this Cloudflare account holds ${host}`;
    throw new CliError(EXIT.FAILED, `${what}: add ${zoneFlag ?? registrableDomain(host)} to Cloudflare first: https://dash.cloudflare.com/${a}/add-site`);
  }
  const name = lower(zone.name);
  const status = String(zone.status ?? "active").toLowerCase();
  if (status !== "active") {
    throw new CliError(EXIT.FAILED, `zone ${name} is ${status}: finish switching its nameservers to Cloudflare (https://dash.cloudflare.com/${a}/${name}), then re-run`);
  }
  return { id: String(zone.id), name };
}

/** The route with `pattern` (this Worker's first when there are several), or null. */
function findRoute(routes, pattern, worker) {
  const same = (Array.isArray(routes) ? routes : []).filter((r) => lower(r?.pattern) === pattern);
  return same.find((r) => r.script === worker) ?? same[0] ?? null;
}

/**
 * Host mode: the route *.<host>/* to this Worker (found, taken over with
 * --take-over, or created). Returns its id.
 */
async function ensureRoute(ctx, api, zone, pattern, route, applied) {
  const w = ctx.worker;
  const path = `/zones/${zone.id}/workers/routes`;
  if (route && route.script === w) {
    ui.ok(`Route ${bold(pattern)} runs Worker ${w}`);
    return String(route.id ?? "");
  }
  if (route) {
    try {
      await api.put(`${path}/${route.id}`, { pattern, script: w });
    } catch (e) {
      throw failed(`could not move the route ${pattern} to Worker ${w}`, e, applied);
    }
    applied.push(`route ${pattern} moved to Worker ${w}`);
    ui.ok(`Route ${bold(pattern)} moved from ${route.script ? `Worker ${route.script}` : "no Worker"} to ${w} (--take-over)`);
    return String(route.id ?? "");
  }
  let created;
  try {
    created = await api.postOrFind(path, { pattern, script: w }, async () => {
      const r = findRoute(await api.get(path), pattern, w);
      return r?.script === w ? r : null;
    });
  } catch (e) {
    throw failed(`could not create the route ${pattern}`, e, applied);
  }
  applied.push(`route ${pattern} created`);
  ui.ok(`Route ${bold(pattern)} → Worker ${w} created`);
  return String(created?.id ?? "");
}

/**
 * Host mode: makes *.<host> resolve. A DoH probe of a fresh random name
 * decides; when it does not resolve, the record is created with the admin
 * token (a 403 falls through), else the dashboard steps are printed and, on a
 * terminal, DoH is polled until --wait (no Enter needed). Returns whether it
 * resolves.
 */
async function ensureWildcardDns(ctx, state, zone, host, wait, applied) {
  const wild = `*.${host}`;
  const resolves = () => dohResolves(`${randomProbeLabel()}.${host}`);
  const poll = () => waitFor(async () => ({ done: await resolves() }), { timeoutSec: wait, intervalSec: POLL_SEC, onPending: progress(`${wild} to resolve`) });
  const records = `https://dash.cloudflare.com/${ctx.accountId}/${zone.name}/dns/records`;
  if (await resolves()) {
    ui.ok(`DNS: ${bold(wild)} resolves`);
    return true;
  }
  const admin = await ctx.admin("dns");
  let denied = false;
  if (admin) {
    const path = `/zones/${zone.id}/dns_records`;
    const lookup = async () => {
      const list = await admin.get(path, { type: "AAAA", name: wild });
      return (Array.isArray(list) ? list : []).find((r) => lower(r?.name) === wild) ?? null;
    };
    try {
      const existing = await lookup();
      if (existing && !existing.proxied) {
        throw new CliError(EXIT.FAILED, `${wild} exists but is not proxied: turn its proxy on or delete it (${records}), then re-run`, { applied: [...applied] });
      }
      if (existing) ui.ok(`DNS: ${bold(`AAAA ${wild}`)} exists (proxied)`);
      else {
        const body = { type: "AAAA", name: wild, content: "100::", proxied: true, ttl: 1, comment: `Backyard sites (Worker ${ctx.worker})` };
        const rec = await admin.postOrFind(path, body, async () => {
          const r = await lookup();
          return r && r.content === "100::" ? r : null;
        });
        state.domain.dnsRecordId = String(rec?.id ?? "") || undefined;
        ctx.saveState(state);
        applied.push(`DNS record AAAA ${wild} created`);
        ui.ok(`DNS record ${bold(`AAAA ${wild} 100::`)} (proxied) created`);
      }
    } catch (e) {
      if (e instanceof CfError && e.forbidden) denied = true;
      else throw failed(`could not create the DNS record ${wild}`, e, applied);
    }
    if (!denied) {
      ui.note(`Waiting for ${wild} to resolve…`);
      return (await poll()).done;
    }
  }
  const intro = denied
    ? `The API token (${admin.label}) cannot create DNS records in ${zone.name} (it needs ${DNS_PERMISSION}). Add this record in the dashboard`
    : "Your wrangler login cannot create DNS records. Add this record in the dashboard";
  ui.log(`\n${intro} (${records}):`);
  ui.log(`  ${bold(`Type AAAA · Name ${dnsRecordName(host, zone.name)} · IPv6 address 100:: · Proxy status Proxied`)}`);
  ui.log(`Or let Backyard create it: BACKYARD_CF_API_TOKEN=<token with ${DNS_PERMISSION}> ${ctx.invokedAs} domain add ${host} --host-mode`);
  if (!ctx.interactive) return false; // checked once above
  ui.note(`Waiting for ${wild} to resolve (a check every ${POLL_SEC} s for up to ${wait} s; no need to press Enter, Ctrl-C stops)…`);
  return (await poll()).done;
}

/**
 * Host mode: whether a site host answers over TLS (GET
 * https://byd-probe-<hex>.<host>/__backyard/healthz, polled until --wait).
 * A Cloudflare Access login redirect counts too: TLS works then. Returns ""
 * when it does, else why not.
 */
async function checkWildcardTLS(ctx, host, wait) {
  let last;
  const r = await waitFor(
    async () => {
      last = await probe(`https://${randomProbeLabel()}.${host}/__backyard/healthz`);
      const access = last.ok && last.status >= 300 && last.status < 400 && Boolean(parseAccessRedirect(last.location));
      return { done: last.ok && (last.status === 200 || access) };
    },
    { timeoutSec: wait, intervalSec: POLL_SEC, onPending: progress(`a certificate for *.${host}`) },
  );
  if (r.done) {
    ui.ok(`TLS: sites at ${bold(`https://<site>.${host}/`)} answer`);
    return "";
  }
  if (!last.ok && last.kind === "certificate") return `the certificate does not cover *.${host} yet (see the warning above)`;
  if (!last.ok && last.kind === "dns") return `*.${host} does not resolve on this machine yet (your resolver may cache the old answer for a few minutes)`;
  if (!last.ok) return `sites at *.${host} cannot be reached yet (${last.error})`;
  return `sites at *.${host} answer ${last.status} instead of Worker ${ctx.worker} (check the route *.${host}/*)`;
}

/** Waits for <hub>/__backyard/readyz == 200; "" when ready, else why not. */
async function waitReady(hub, wait) {
  ui.log(`\nWaiting for ${hub} to answer…`);
  let last;
  const r = await waitFor(
    async () => {
      last = await probe(`${hub}/__backyard/readyz`);
      return { done: last.ok && last.status === 200 };
    },
    { timeoutSec: wait, intervalSec: POLL_SEC, onPending: progress(hub) },
  );
  if (r.done) {
    ui.ok(`${hub} is ${green("ready")}`);
    return "";
  }
  if (!last.ok && (last.kind === "certificate" || last.kind === "dns")) return "certificate or DNS still provisioning";
  if (!last.ok) return last.error;
  return `/__backyard/readyz answers ${last.status}`;
}

/**
 * Deletes what host mode added for this Worker: the route *.<host>/* (only
 * while it runs this Worker; by its id, else by pattern) and the wildcard
 * record this tool created (only with the admin token; otherwise the record
 * to delete is printed). Updates `d` (routeId, dnsRecordId) and returns the
 * steps left for the person (the route could not be deleted).
 * @param {import("./cli.mjs").Ctx} ctx
 * @param {import("./lib/cfapi.mjs").Api} api
 * @param {Record<string, any>} d   the domain state
 * @param {{id: string, name: string} | null} zone
 * @returns {Promise<string[]>}
 */
async function removeHostMode(ctx, api, d, zone) {
  const w = ctx.worker;
  const pattern = `*.${d.host}/*`;
  const wild = `*.${d.host}`;
  const dash = `Workers & Pages → ${w} → Settings → Domains & Routes`;
  const manual = [];
  if (!zone?.id) {
    manual.push(`delete the route ${pattern} in the dashboard (${dash}): the zone of ${d.host} could not be found`);
  } else {
    let list = null;
    try {
      const routes = await api.get(`/zones/${zone.id}/workers/routes`);
      list = Array.isArray(routes) ? routes : [];
    } catch (e) {
      manual.push(`delete the route ${pattern} in the dashboard (${dash}): ${errText(e)}`);
    }
    if (list) {
      const byId = d.routeId ? list.find((r) => String(r?.id) === String(d.routeId)) : undefined;
      const route = byId?.script === w ? byId : list.find((r) => lower(r?.pattern) === pattern && r?.script === w);
      if (route) {
        try {
          await api.delete(`/zones/${zone.id}/workers/routes/${route.id}`);
          ui.ok(`Route ${bold(route.pattern)} deleted`);
          delete d.routeId;
        } catch (e) {
          if (e instanceof CfError && e.notFound) delete d.routeId;
          else manual.push(`delete the route ${pattern} in the dashboard (${dash}): ${errText(e)}`);
        }
      } else {
        if (byId) ui.note(`The route ${byId.pattern} now runs ${byId.script ? `Worker ${byId.script}` : "no Worker"}: left alone`);
        delete d.routeId;
      }
    }
  }
  const zoneName = zone?.name || d.zoneName || "";
  const records = zoneName ? `https://dash.cloudflare.com/${ctx.accountId}/${zoneName}/dns/records` : `https://dash.cloudflare.com/${ctx.accountId}`;
  const byHand = (why) => ui.warn(`Delete the DNS record AAAA ${wild} in the dashboard (${records}) ${why}`);
  if (d.dnsRecordId && zone?.id) {
    const admin = await ctx.admin("dns");
    if (admin) {
      try {
        await admin.delete(`/zones/${zone.id}/dns_records/${d.dnsRecordId}`);
        ui.ok(`DNS record ${bold(`AAAA ${wild}`)} deleted`);
        delete d.dnsRecordId;
      } catch (e) {
        if (e instanceof CfError && e.notFound) delete d.dnsRecordId;
        else byHand(`(this tool created it; deleting it failed: ${errText(e)})`);
      }
    } else {
      byHand(`(this tool created it; with BACKYARD_CF_API_TOKEN=<token with ${DNS_PERMISSION}> it deletes it itself)`);
    }
  } else {
    byHand("if you added it for Backyard (this tool did not create it, so it leaves it alone)");
  }
  return manual;
}

/**
 * `domain add <host>`.
 * @param {Record<string, any>} opts   parsed flags (lib/args.mjs): host, hostMode, zone, takeOver, replaceDnsRecord, wait
 * @param {import("./cli.mjs").Ctx} ctx
 * @returns {Promise<number>}
 */
export async function runDomainAdd(opts, ctx) {
  const inv = ctx.invokedAs;
  const w = ctx.worker;
  // 1. Arguments.
  const host = parseHost(opts.host);
  const zoneFlag = opts.zone !== undefined ? parseHost(opts.zone, "--zone") : undefined;
  if (zoneFlag && host !== zoneFlag && !host.endsWith(`.${zoneFlag}`)) throw new CliError(EXIT.USAGE, `${host} is not in the zone ${zoneFlag} (--zone)`);
  const mode = opts.hostMode ? "host" : "path";
  const wait = opts.wait ?? DEFAULT_WAIT_SEC;

  ui.log(bold(`\nBackyard on Cloudflare — domain add ${host}${mode === "host" ? " --host-mode" : ""}\n`));
  // 2–3. State, login, the live Worker (adopted by a machine without state).
  const state = ctx.loadState();
  const previousUrl = state.url ?? "";
  await ctx.login(state);
  const api = await ctx.api();
  const live = await ctx.readLive();
  if (live.exists === false) throw new CliError(EXIT.USAGE, `Worker ${w} does not exist yet: run ${inv} setup first`);
  if (live.exists === null) throw new CliError(EXIT.FAILED, `could not read the live configuration of Worker ${w} (nothing was changed): ${live.error || "unknown error"}`);
  ctx.adopt(state, live);

  // 4. Refusals: nothing has changed yet.
  const external = externalDomainKeys(state);
  if (external.length) throw new CliError(EXIT.USAGE, externalHint(external, inv));
  const prev = state.domain?.host ? state.domain : undefined;
  const prevMode = prev?.mode === "host" ? "host" : "path";
  if (prev && prev.host !== host) throw new CliError(EXIT.REFUSED, `Backyard is on ${prev.host}: run \`${inv} domain remove\` first (nothing was changed)`);
  if (state.access && (!prev || prevMode !== mode)) {
    throw new CliError(EXIT.REFUSED, `Cloudflare Access is on: run \`${inv} access disable\` first, then domain add and access enable again (nothing was changed)`);
  }
  // A deploy the auth guard refuses must not get as far as attaching anything.
  authGuard(live, ctx.vars(state), { worker: w, remedy: guardRemedy(ctx) });
  await fillWorkersDevUrl(ctx, api, state);

  // 5. The zone.
  const zone = await findZone(ctx, api, host, zoneFlag);
  ui.ok(`Zone ${bold(zone.name)}`);

  // 6. Host-mode warnings (confirmed once per host: re-runs only repeat them).
  if (mode === "host") {
    ui.warn(
      host === zone.name
        ? `the route *.${zone.name}/* sends every proxied hostname of ${zone.name} (www, api, …) to Backyard; prefer a dedicated zone`
        : `Universal SSL covers only ${zone.name} and *.${zone.name}; sites at *.${host} need an Advanced Certificate (paid; dashboard: SSL/TLS → Edge Certificates → Order, or the Terraform module's advanced_certificate)`,
    );
    if (!(prev && prevMode === "host") && !(await ctx.prompt.confirm(`Use host mode on ${host}?`))) throw cancelled();
  }

  // 7. Preflight: it fails closed, nothing is attached when a check fails.
  const a = ctx.accountId;
  const origin = { hostname: host, zone_id: zone.id };
  let cs;
  try {
    cs = await api.post(`/accounts/${a}/workers/scripts/${w}/domains/changeset`, [origin], { replace_state: "true" });
  } catch (e) {
    throw failed(`could not check ${host} with Cloudflare (nothing was changed)`, e);
  }
  const entry = (list) => (Array.isArray(list) ? list : []).find((x) => lower(x?.hostname) === host);
  let updated = entry(cs?.updated);
  const conflicting = entry(cs?.conflicting);
  if (!updated && !conflicting && !entry(cs?.added)) {
    // Not in the changeset at all: the custom-domain list tells whose it is.
    let found;
    try {
      found = entry(await api.list(`/accounts/${a}/workers/domains/records`, { hostname: host }));
    } catch (e) {
      throw failed(`could not check ${host} with Cloudflare (nothing was changed)`, e);
    }
    if (found) updated = { id: found.id, hostname: host, modified: String(found.service ?? "") !== w };
  }
  const attached = Boolean(updated && !updated.modified);
  if (updated?.modified) {
    let service;
    try {
      service = String((await api.get(`/accounts/${a}/workers/domains/records/${updated.id}`))?.service ?? "");
    } catch (e) {
      throw failed(`could not check which Worker ${host} belongs to (nothing was changed)`, e);
    }
    if (service !== w && !opts.takeOver) {
      throw new CliError(EXIT.REFUSED, `${host} is a custom domain of Worker ${service || "(unknown)"}: nothing was changed. Re-run with --take-over to move it to Worker ${w}`);
    }
  }
  if (conflicting && !opts.replaceDnsRecord) {
    throw new CliError(EXIT.REFUSED, `${host} already has DNS records (a website?): nothing was changed. Re-run with --replace-dns-record to replace them with the Worker's custom domain`);
  }
  const others = (Array.isArray(cs?.removed) ? cs.removed : []).filter((x) => x?.hostname && lower(x.hostname) !== host);
  const pattern = `*.${host}/*`;
  let route = null;
  if (mode === "host") {
    try {
      route = findRoute(await api.get(`/zones/${zone.id}/workers/routes`), pattern, w);
    } catch (e) {
      throw failed(`could not read the routes of zone ${zone.name} (nothing was changed)`, e);
    }
    if (route && route.script !== w && !opts.takeOver) {
      throw new CliError(EXIT.REFUSED, `the route ${pattern} runs ${route.script ? `Worker ${route.script}` : "no Worker"}: nothing was changed. Re-run with --take-over to move it to Worker ${w}`);
    }
  }

  // 8. Attach (unless it already belongs to this Worker); the Worker's other custom domains stay.
  const applied = [];
  let customDomainId = attached ? String(updated.id ?? "") : "";
  if (!attached) {
    if (others.length) ui.note(`Keeping the other custom domains of Worker ${w}: ${others.map((x) => x.hostname).join(", ")}`);
    const body = {
      override_scope: true,
      // A modified record that passed the ownership check is this Worker's own (other
      // flags), or --take-over was given: either way the origin must be overridden.
      override_existing_origin: Boolean(opts.takeOver || updated?.modified),
      override_existing_dns_record: Boolean(opts.replaceDnsRecord),
      origins: [origin, ...others.map((r) => ({ hostname: r.hostname, zone_id: r.zone_id }))],
    };
    try {
      await api.put(`/accounts/${a}/workers/scripts/${w}/domains/records`, body);
    } catch (e) {
      throw failed(`could not attach ${host} to Worker ${w}`, e);
    }
    applied.push(`custom domain ${host} attached to Worker ${w}`);
    try {
      customDomainId = String(entry(await api.list(`/accounts/${a}/workers/domains/records`, { service: w, environment: "production" }))?.id ?? "");
    } catch (e) {
      throw failed(`could not read the custom domains of Worker ${w}`, e, applied);
    }
    ui.ok(`Custom domain ${bold(host)} attached to Worker ${bold(w)} (Cloudflare adds its DNS record and certificate)`);
  } else {
    ui.ok(`${bold(host)} is a custom domain of Worker ${bold(w)}`);
  }
  const switchToPath = Boolean(prev) && prevMode === "host" && mode === "path";
  state.domain = {
    ...(prev ?? {}),
    host,
    mode,
    zoneId: zone.id,
    zoneName: zone.name,
    customDomainId: customDomainId || prev?.customDomainId || undefined,
    // Host mode stays as it was until the checks below decide.
    status: mode === "path" ? "active" : prev && prevMode === "host" ? (prev.status ?? "pending-dns") : "pending-dns",
    source: prev && attached ? (prev.source ?? "cli") : "cli",
  };
  ctx.saveState(state);

  // 9. Host mode: the route, the wildcard record, the certificate.
  let pending = "";
  if (mode === "host") {
    state.domain.routeId = (await ensureRoute(ctx, api, zone, pattern, route, applied)) || undefined;
    ctx.saveState(state);
    if (!(await ensureWildcardDns(ctx, state, zone, host, wait, applied))) {
      state.domain.status = "pending-dns";
      pending = `*.${host} does not resolve yet`;
    } else {
      pending = await checkWildcardTLS(ctx, host, wait);
      state.domain.status = pending ? "pending-certificate" : "active";
    }
    ctx.saveState(state);
  }

  // 11. Deploy: BASE_DOMAIN=<host>, URL_MODE=host only once host mode is active.
  let d;
  try {
    d = await ctx.deploy(state, { live });
  } catch (e) {
    if (e instanceof CliError && e.code === EXIT.FAILED && !e.applied.length) e.applied.push(...applied);
    throw e;
  }
  ui.note(`Deployed with BASE_DOMAIN=${d.vars.BASE_DOMAIN} URL_MODE=${d.vars.URL_MODE}`);

  // 10. host → path: the route and the wildcard record go, after links have switched.
  if (switchToPath) {
    const manual = await removeHostMode(ctx, await ctx.api({ fresh: true }), state.domain, zone);
    ctx.saveState(state);
    for (const m of manual) ui.warn(m);
  }

  // 12–13. The hub's new URL, then readiness.
  const hub = `https://${host}`;
  state.url = hub;
  ctx.saveState(state);
  ctx.writeResult(state, { previousUrl });
  const notReady = await waitReady(hub, wait);

  // 14. Where things are.
  const hostActive = mode === "host" && state.domain.status === "active";
  ui.log(`\n${green("✔")} Backyard is on ${bold(cyan(hub))}`);
  ui.log(`  Sites: ${bold(hostActive ? `https://<site>.${host}/` : `${hub}/s/<site>/`)}`);
  if (notReady || pending) {
    const parts = [];
    if (notReady) parts.push(`${hub} does not answer yet (${notReady}): check later with \`${inv} status\`; your resolver may cache the old answer for a few minutes`);
    if (pending) {
      parts.push(
        `${pending}: the hub works on ${hub}/; sites switch to https://<site>.${host}/ once DNS and the certificate are ready: re-run \`${inv} domain add ${host} --host-mode\``,
      );
    }
    throw new CliError(EXIT.PENDING, parts.join("\n  "), { applied });
  }
  if (!state.access) ui.note(`\nCompany login next: ${inv} access enable --allow-email-domain <your-domain>`);
  ui.log("");
  return EXIT.OK;
}

/**
 * `domain remove`.
 * @param {Record<string, any>} opts
 * @param {import("./cli.mjs").Ctx} ctx
 * @returns {Promise<number>}
 */
export async function runDomainRemove(opts, ctx) {
  void opts;
  const inv = ctx.invokedAs;
  const w = ctx.worker;
  ui.log(bold("\nBackyard on Cloudflare — domain remove\n"));
  // 1. State and login; a machine without state adopts the live Worker first.
  const state = ctx.loadState();
  const previousUrl = state.url ?? "";
  await ctx.login(state);
  let api = await ctx.api();
  let live;
  if (!state.updatedAt) {
    live = await ctx.readLive();
    ctx.adopt(state, live);
  }
  const d = state.domain?.host ? state.domain : undefined;
  if (!d) {
    ui.ok(`No domain is managed by this tool for Worker ${w}: nothing to do`);
    const external = externalDomainKeys(state);
    if (external.length) ui.note(externalHint(external, inv));
    return EXIT.OK;
  }
  // 2. Refusals.
  if (state.access) throw new CliError(EXIT.REFUSED, `Cloudflare Access protects ${d.host}: run \`${inv} access disable\` first (nothing was changed)`);
  if (state.workersDev === false) {
    throw new CliError(EXIT.REFUSED, `workers.dev is off, so the hub would have no address left: turn workers.dev back on first: \`${inv} setup --workers-dev\` (nothing was changed)`);
  }
  live ??= await ctx.readLive();
  if (live.exists === null) throw new CliError(EXIT.FAILED, `could not read the live configuration of Worker ${w} (nothing was changed): ${live.error || "unknown error"}`);
  await fillWorkersDevUrl(ctx, api, state);

  // 3. Confirm.
  const back = state.workersDevUrl || "workers.dev";
  if (!(await ctx.prompt.confirm(`Remove ${d.host} from Worker ${w}? The hub moves back to ${back}`))) throw cancelled();

  // 4. Deploy without BASE_DOMAIN and URL_MODE first, so links switch first (the guard runs).
  if (live.exists) await ctx.deploy(state, { live, stateOverrides: { domain: undefined } });
  else ui.warn(`Worker ${w} does not exist: nothing to deploy`);

  // 5–6. The route and the wildcard record (host mode), then the custom domain.
  api = await ctx.api({ fresh: true });
  const a = ctx.accountId;
  const dash = `Workers & Pages → ${w} → Settings → Domains & Routes → remove ${d.host}`;
  const manual = [];
  let records = null;
  try {
    records = await api.list(`/accounts/${a}/workers/domains/records`, { service: w, environment: "production" });
  } catch (e) {
    manual.push(`could not look up the custom domain ${d.host} (${errText(e)}): remove it in the dashboard (${dash})`);
  }
  const cd = (records ?? []).find((r) => lower(r?.hostname) === d.host && (r.service ?? w) === w);
  if (d.mode === "host" || d.routeId || d.dnsRecordId) {
    let zone = d.zoneId ? { id: String(d.zoneId), name: lower(d.zoneName) } : cd?.zone_id ? { id: String(cd.zone_id), name: lower(cd.zone_name) } : null;
    if (!zone) zone = await findZone(ctx, api, d.host).catch(() => null);
    manual.push(...(await removeHostMode(ctx, api, d, zone)));
  }
  if (cd) {
    try {
      await api.delete(`/accounts/${a}/workers/domains/${cd.id}`);
      ui.ok(`Custom domain ${bold(d.host)} detached from Worker ${bold(w)}`);
    } catch (e) {
      if (!(e instanceof CfError && e.notFound)) manual.push(`could not detach ${d.host} (${errText(e)}): remove it in the dashboard (${dash})`);
    }
  } else if (records) {
    ui.note(`${d.host} is not a custom domain of Worker ${w} (any more)`);
  }

  // 7. Forget the domain.
  state.domain = undefined;
  state.url = state.workersDevUrl || undefined;
  ctx.saveState(state);
  ctx.writeResult(state, { previousUrl });
  ui.log(`\n${green("✔")} ${d.host} removed: the hub is on ${bold(cyan(state.workersDevUrl || "workers.dev"))} again`);
  if (manual.length) throw new CliError(EXIT.PENDING, `the Worker no longer uses ${d.host}, but some of it is left for you:\n${manual.map((m) => `  - ${m}`).join("\n")}`);
  ui.log("");
  return EXIT.OK;
}
