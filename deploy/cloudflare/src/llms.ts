/**
 * /llms.txt and /llms-full.txt built from the embedded skill documents
 * (port of skills/docs.go): the hub URL placeholder and example site hosts are
 * rewritten for this deployment, links between documents point at /docs/*.md.
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

/** SKILL.md, or a reference document by name ("" when unknown). */
export function docMarkdown(name: string): string {
  const n = name.trim().toLowerCase().replace(/\.md$/, "");
  if (n === "skill") return SKILL_MD;
  if (!/^[a-z0-9][a-z0-9_-]{0,63}$/.test(n)) return "";
  return REFERENCES[n] ?? "";
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

function hubHost(base: string): { scheme: string; host: string } | null {
  try {
    const u = new URL(base);
    if ((u.protocol !== "http:" && u.protocol !== "https:") || (u.pathname !== "" && u.pathname !== "/")) return null;
    return { scheme: u.protocol.slice(0, -1), host: u.host };
  } catch {
    return null;
  }
}

function link(title: string, href: string, desc: string): string {
  return `- [${title}](${href})` + (desc ? `: ${desc}` : "") + "\n";
}

/** /llms.txt for a hub at publicURL. */
export function llmsTxt(publicURL: string): string {
  const base = publicURL.trim().replace(/\/+$/, "");
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
  const base = publicURL.trim().replace(/\/+$/, "");
  const known = new Set(["skill", ...referenceNames()]);
  let b = `<!-- Backyard documentation for agents: the skill overview followed by every reference. Source: ${docURL(base, "skill")} -->\n\n`;
  b += stripFrontMatter(SKILL_MD).trim() + "\n";
  for (const name of referenceNames()) {
    b += `\n---\n\n<!-- Source: ${docURL(base, name)} -->\n\n`;
    b += (REFERENCES[name] ?? "").trim() + "\n";
  }
  let out = b.replace(DOC_LINK_RE, (m: string, n: string, frag: string | undefined) => {
    const name = n.toLowerCase();
    return known.has(name) ? `](${docURL(base, name)}${frag ?? ""})` : m;
  });
  if (base) {
    out = replaceHubURL(out, base);
    const hh = hubHost(base);
    if (hh) out = replaceSiteHosts(out, hh.scheme, hh.host);
  }
  return out;
}
