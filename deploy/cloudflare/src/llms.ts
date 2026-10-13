/**
 * /llms.txt, /llms-full.txt and /docs/<name>.md built from the embedded skill
 * documents (port of skills/docs.go and the Go hub's /docs handler): the hub
 * URL placeholder and example site hosts are rewritten for this deployment
 * (localize), and in /llms-full.txt links between documents point at
 * /docs/*.md.
 */
import { REFERENCES, SKILL_MD } from "./generated/skills";

/** Hub URL used by the documents in examples. */
export const HUB_URL_PLACEHOLDER = "https://backyard.example.com";
const PLACEHOLDER_DOMAIN = HUB_URL_PLACEHOLDER.slice("https://".length);

const REF_ORDER = ["api", "recipes", "deploy"];

const REF_INFO: Record<string, { title: string; desc: string }> = {
  skill: {
    title: "Overview and rules",
    desc: "what Backyard is, the golden rules, quickstart, a cheat sheet of every API, patterns, limits and troubleshooting",
  },
  api: {
    title: "API reference",
    desc: "the complete JavaScript SDK (backyard.db, files, ai, warehouse, socket, id) plus the HTTP and websocket API, query semantics, errors and limits",
  },
  recipes: {
    title: "Recipes",
    desc: "complete copy-paste apps: realtime voting, live poll, multiplayer cursors, chat with presence, AI summarizer, image generator, file drop, kanban, KPI dashboard, guestbook, game leaderboard",
  },
  deploy: {
    title: "Deploying",
    desc: "the CLI, login per auth mode, deploy methods, .backyard.json and .backyardignore, host vs path mode, the Cloudflare free-tier edition and CI",
  },
};

/** Reference names in reading order (api, recipes, deploy, then the rest). */
export function referenceNames(): string[] {
  const names = Object.keys(REFERENCES).filter((n) => /^[a-z0-9][a-z0-9_-]{0,63}$/.test(n));
  const rank = (n: string) => {
    const i = REF_ORDER.indexOf(n);
    return i >= 0 ? i : REF_ORDER.length;
  };
  return names.sort((a, b) => rank(a) - rank(b) || (a < b ? -1 : a > b ? 1 : 0));
}

/** Name of a document below /docs/ (one path segment, so no "/", "." or ".."). */
const DOC_NAME_RE = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;

/** Optional directory of /docs/references/<name>.md (how SKILL.md links to its references). */
const REFERENCES_DIR = "references/";

/**
 * The document served at /docs/<rel>.md, localized for the hub at publicURL
 * (like /llms-full.txt), or "" when there is none. rel is the path below
 * /docs/ without ".md": "skill" (SKILL.md, front matter included), a
 * reference name ("api"), or the same with a "references/" prefix
 * ("references/api"), which is where SKILL.md's relative links
 * ("references/api.md") resolve against /docs/skill.md. Names are
 * case-insensitive; "references/skill" does not exist.
 */
export function docMarkdown(rel: string, publicURL = ""): string {
  const isRef = rel.startsWith(REFERENCES_DIR);
  const name = isRef ? rel.slice(REFERENCES_DIR.length) : rel;
  if (!DOC_NAME_RE.test(name)) return "";
  const n = name.toLowerCase();
  let md: string;
  if (n === "skill") md = isRef ? "" : SKILL_MD;
  else md = Object.hasOwn(REFERENCES, n) ? (REFERENCES[n] ?? "") : "";
  return md ? localize(md, publicURL) : "";
}

/** scheme, authority and path of an absolute URL ("scheme://authority/path?query#fragment"). */
const ABS_URL_RE = /^([A-Za-z][A-Za-z0-9+.-]*):\/\/([^/?#]*)([^?#]*)/;

/**
 * Splits an absolute URL like Go's url.Parse does for the parts the
 * documents need: the scheme (lowercased), the host[:port] as written (user
 * info dropped; unlike WHATWG URL, neither lowercased nor stripped of a
 * default port) and the path. null when s is not "scheme://host…".
 */
function splitURL(s: string): { scheme: string; host: string; path: string } | null {
  const m = ABS_URL_RE.exec(s);
  if (!m) return null;
  const authority = m[2]!;
  const host = authority.slice(authority.lastIndexOf("@") + 1);
  if (!host) return null;
  return { scheme: m[1]!.toLowerCase(), host, path: m[3]! };
}

/**
 * Normalizes a public URL like skills.baseURL: surrounding space, user info,
 * a query, a fragment and trailing slashes are dropped ("" stays "", so links
 * become server-relative). The host keeps its spelling, as in Go.
 */
export function baseURL(publicURL: string): string {
  let s = publicURL.trim();
  const u = splitURL(s);
  if (u) s = `${u.scheme}://${u.host}${u.path}`;
  return s.replace(/\/+$/, "");
}

/**
 * Adapts an embedded document to the hub at publicURL (skills.Localize): the
 * placeholder hub URL becomes publicURL and, when publicURL has no path,
 * example site hosts under the placeholder domain become subdomains of the hub
 * host. Links between documents are left alone. An empty publicURL returns md
 * unchanged.
 */
export function localize(md: string, publicURL: string): string {
  const base = baseURL(publicURL);
  if (!base) return md;
  let out = replaceHubURL(md, base);
  const hh = hubHost(base);
  if (hh) out = replaceSiteHosts(out, hh.scheme, hh.host);
  return out;
}

function docURL(base: string, name: string): string {
  return `${base}/docs/${name}.md`;
}

function docTitle(name: string): string {
  const info = REF_INFO[name];
  if (info) return info.title;
  return name
    .split(/[-_]/)
    .filter(Boolean)
    .map((w) => w[0]!.toUpperCase() + w.slice(1))
    .join(" ");
}

/**
 * Reports whether the host of host[:port] is an IP address (like Go's
 * net.ParseIP(u.Hostname()): a bracketed or bare IPv6 address, or a dotted
 * IPv4 address without leading zeros).
 */
function isIPHost(hostport: string): boolean {
  if (hostport.startsWith("[")) return true;
  const colon = hostport.lastIndexOf(":");
  const host = colon >= 0 && /^:\d*$/.test(hostport.slice(colon)) ? hostport.slice(0, colon) : hostport;
  if (host.includes(":")) return true;
  const parts = host.split(".");
  return parts.length === 4 && parts.every((p) => /^(?:0|[1-9]\d{0,2})$/.test(p) && Number(p) <= 255);
}

/**
 * Splits an absolute hub URL without a path into scheme and host[:port]; null
 * when site subdomains cannot be derived from it (a path, or an IP address).
 */
function hubHost(base: string): { scheme: string; host: string } | null {
  const u = splitURL(base);
  if (!u || (u.scheme !== "http" && u.scheme !== "https") || (u.path !== "" && u.path !== "/") || isIPHost(u.host)) return null;
  return { scheme: u.scheme, host: u.host };
}

function link(title: string, href: string, desc: string): string {
  return `- [${title}](${href})` + (desc ? `: ${desc}` : "") + "\n";
}

/** /llms.txt for a hub at publicURL. */
export function llmsTxt(publicURL: string): string {
  const base = baseURL(publicURL);
  let b = "# Backyard\n\n";
  b +=
    "> Backyard turns a folder of static HTML/CSS/JS into an internal URL that only people in the company can open, " +
    "and gives every site a zero-config backend in the browser: `backyard.db` (JSON documents with realtime queries), " +
    "`backyard.files` (uploads), `backyard.ai` (chat, streaming, images, embeddings), `backyard.warehouse` (read-only SQL), " +
    "`backyard.socket` (websocket rooms with presence) and `backyard.id` (the signed-in user).\n\n";
  const hh = hubHost(base);
  if (hh) {
    b += `Hub: ${base}/ — sites live at \`${hh.scheme}://<site>.${hh.host}/\` (host mode) or \`${base}/s/<site>/\` (path mode).\n\n`;
  } else if (base) {
    b += `Hub: ${base}/ — sites live at \`${base}/s/<site>/\` (path mode) or on subdomains of the hub host (host mode).\n\n`;
  } else {
    b += "Sites live at `https://<site>.<hub host>/` (host mode) or `<hub>/s/<site>/` (path mode).\n\n";
  }
  b += "Essentials:\n\n";
  b +=
    '- A site is a folder of static files with an `index.html`. Include the SDK with `<script src="/__backyard/backyard.js"></script>` (absolute path) and reference your own files with relative URLs.\n';
  b +=
    "- No server code and never any secrets in the page: AI, warehouse and sign-in are handled by the server. All data of a site is shared by everyone who can open it.\n";
  b +=
    "- `backyard init <site>` scaffolds a project, `backyard dev` previews it with a local backend, `backyard deploy` publishes it (overwriting the whole site). `backyard mcp` exposes the same as MCP tools.\n\n";
  b += "## Docs\n\n";
  b += link(docTitle("skill"), docURL(base, "skill"), REF_INFO.skill!.desc);
  for (const name of referenceNames()) b += link(docTitle(name), docURL(base, name), REF_INFO[name]?.desc ?? "");
  b += "\n## Optional\n\n";
  b += link("Full documentation", base + "/llms-full.txt", "the overview and every reference in one file");
  b += link("TypeScript declarations", base + "/__backyard/backyard.d.ts", "the normative types of the browser SDK");
  b += link("Human-readable docs", base + "/docs", "the same documentation rendered for people");
  return b;
}

function stripFrontMatter(s: string): string {
  if (!s.startsWith("---\n")) return s;
  const rest = s.slice(4);
  if (rest.startsWith("---\n")) return rest.slice(4);
  const i = rest.indexOf("\n---\n");
  if (i >= 0) return rest.slice(i + 5);
  if (rest.endsWith("\n---")) return "";
  return s;
}

const DOC_LINK_RE = /\]\((?:\.\/)?(?:references\/)?([A-Za-z0-9_-]+)\.md(#[^)\s]*)?\)/g;

function isAlnum(c: string | undefined): boolean {
  return c !== undefined && /[A-Za-z0-9]/.test(c);
}

function continuesHost(s: string, i: number): boolean {
  if (i >= s.length) return false;
  const c = s[i];
  if (isAlnum(c) || c === "-" || c === "_") return true;
  return c === "." && isAlnum(s[i + 1]);
}

function replaceHubURL(s: string, base: string): string {
  let out = "";
  for (;;) {
    const i = s.indexOf(HUB_URL_PLACEHOLDER);
    if (i < 0) return out + s;
    const end = i + HUB_URL_PLACEHOLDER.length;
    out += s.slice(0, i) + (continuesHost(s, end) ? HUB_URL_PLACEHOLDER : base);
    s = s.slice(end);
  }
}

const SITE_HOST_RE = new RegExp(
  "(https://)?((?:[A-Za-z0-9*_-]|<[a-z]+>)+(?:\\.(?:[A-Za-z0-9*_-]|<[a-z]+>)+)*)\\." + PLACEHOLDER_DOMAIN.replace(/\./g, "\\."),
  "g",
);

function replaceSiteHosts(s: string, scheme: string, host: string): string {
  return s.replace(SITE_HOST_RE, (m: string, sch: string | undefined, label: string, offset: number) =>
    continuesHost(s, offset + m.length) ? m : (sch ? scheme + "://" : "") + label + "." + host,
  );
}

/** /llms-full.txt: SKILL.md followed by every reference, links resolved. */
export function llmsFullTxt(publicURL: string): string {
  const base = baseURL(publicURL);
  const known = new Set(["skill", ...referenceNames()]);
  let b = `<!-- Backyard documentation for agents: the skill overview followed by every reference. Source: ${docURL(base, "skill")} -->\n\n`;
  b += stripFrontMatter(SKILL_MD).trim() + "\n";
  for (const name of referenceNames()) {
    b += `\n---\n\n<!-- Source: ${docURL(base, name)} -->\n\n`;
    b += (REFERENCES[name] ?? "").trim() + "\n";
  }
  const out = b.replace(DOC_LINK_RE, (m: string, n: string, frag: string | undefined) => {
    const name = n.toLowerCase();
    return known.has(name) ? `](${docURL(base, name)}${frag ?? ""})` : m;
  });
  return localize(out, base);
}
