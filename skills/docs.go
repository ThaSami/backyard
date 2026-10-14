package skills

import (
	"io/fs"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// HubURLPlaceholder is the hub URL the embedded documents use in examples
// (sites appear as https://<site>.backyard.example.com/ or
// https://backyard.example.com/s/<site>/). LLMSFullTxt replaces it with the
// server's public URL; other renderers (e.g. the Cloudflare edition) should
// do the same.
const HubURLPlaceholder = "https://backyard.example.com"

const (
	skillDir      = "backyard"
	skillPath     = skillDir + "/SKILL.md"
	referencesDir = skillDir + "/references"
)

// refOrder is the reading order of the well-known references; others follow
// alphabetically.
var refOrder = []string{"api", "recipes", "deploy"}

// refInfo describes the well-known documents for /llms.txt.
var refInfo = map[string]struct{ title, desc string }{
	"skill": {
		"Overview and rules",
		"what Backyard is, the golden rules, quickstart, a cheat sheet of every API, patterns, limits and troubleshooting",
	},
	"api": {
		"API reference",
		"the complete JavaScript SDK (backyard.db, files, ai, warehouse, socket, id) plus the HTTP and websocket API, query semantics, errors and limits",
	},
	"recipes": {
		"Recipes",
		"complete copy-paste apps: realtime voting, live poll, multiplayer cursors, chat with presence, AI summarizer, image generator, file drop, kanban, KPI dashboard, guestbook, game leaderboard",
	},
	"deploy": {
		"Deploying",
		"the CLI, login per auth mode, deploy methods, .backyard.json and .backyardignore, host vs path mode, the Cloudflare free-tier edition and CI",
	},
}

var refNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// normalizeRefName lowercases name and strips a ".md" suffix, reporting
// whether the result is a valid document name.
func normalizeRefName(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.TrimSuffix(name, ".md")
	return name, refNameRe.MatchString(name)
}

// References returns the names of the reference documents (for Reference),
// in reading order: api, recipes, deploy, then any others alphabetically.
func References() []string { return slices.Clone(referenceNames()) }

var referenceNames = sync.OnceValue(func() []string {
	entries, err := fs.ReadDir(content, referencesDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.Type().IsRegular() || path.Ext(e.Name()) != ".md" {
			continue
		}
		if n, ok := normalizeRefName(e.Name()); ok && n+".md" == e.Name() {
			names = append(names, n)
		}
	}
	rank := func(n string) int {
		if i := slices.Index(refOrder, n); i >= 0 {
			return i
		}
		return len(refOrder)
	}
	slices.SortStableFunc(names, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	return names
})

// baseURL normalizes a public URL: surrounding space, a query, a fragment,
// user info and trailing slashes are dropped ("" stays "", so links become
// server-relative).
func baseURL(publicURL string) string {
	s := strings.TrimSpace(publicURL)
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		u.User = nil
		u.RawQuery, u.ForceQuery = "", false
		u.Fragment, u.RawFragment = "", ""
		s = u.String()
	}
	return strings.TrimRight(s, "/")
}

// Localize adapts an embedded document (SKILL.md or a reference) to the hub
// at publicURL: HubURLPlaceholder becomes publicURL and, when publicURL has
// no path, example site hosts under the placeholder domain
// ("https://<site>.backyard.example.com/") become subdomains of the hub host.
// Links between the documents are left as they are. With an empty
// publicURL, md is returned unchanged. Serve documents through Localize
// (e.g. /docs/<name>.md) so they match /llms-full.txt.
func Localize(md, publicURL string) string {
	base := baseURL(publicURL)
	if base == "" {
		return md
	}
	out := replaceHubURL(md, base)
	if scheme, host, ok := hubHost(base); ok {
		out = replaceSiteHosts(out, scheme, host)
	}
	return out
}

// docURL is the hub URL of a document (/docs/<name>.md).
func docURL(base, name string) string { return base + "/docs/" + name + ".md" }

// docTitle returns the human title of a document.
func docTitle(name string) string {
	if info, ok := refInfo[name]; ok {
		return info.title
	}
	words := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

// buildLLMSTxt renders /llms.txt following the llms.txt convention: an H1,
// a blockquote summary, a few lines of essentials and H2 sections of links.
func buildLLMSTxt(publicURL string) string {
	base := baseURL(publicURL)
	var b strings.Builder
	b.WriteString("# Backyard\n\n")
	b.WriteString("> Backyard turns a folder of static HTML/CSS/JS into an internal URL that only people in the company can open, ")
	b.WriteString("and gives every site a zero-config backend in the browser: `backyard.db` (JSON documents with realtime queries), ")
	b.WriteString("`backyard.files` (uploads), `backyard.ai` (chat, streaming, images, embeddings), `backyard.warehouse` (read-only SQL), ")
	b.WriteString("`backyard.socket` (websocket rooms with presence) and `backyard.id` (the signed-in user).\n\n")

	if scheme, host, ok := hubHost(base); ok {
		b.WriteString("Hub: " + base + "/ — sites live at `" + scheme + "://<site>." + host + "/` (host mode) or `" + base + "/s/<site>/` (path mode).\n\n")
	} else if base != "" {
		b.WriteString("Hub: " + base + "/ — sites live at `" + base + "/s/<site>/` (path mode) or on subdomains of the hub host (host mode).\n\n")
	} else {
		b.WriteString("Sites live at `https://<site>.<hub host>/` (host mode) or `<hub>/s/<site>/` (path mode).\n\n")
	}
	b.WriteString("Essentials:\n\n")
	b.WriteString("- A site is a folder of static files with an `index.html`. Include the SDK with `<script src=\"/__backyard/backyard.js\"></script>` (absolute path) and reference your own files with relative URLs.\n")
	b.WriteString("- No server code and never any secrets in the page: AI, warehouse and sign-in are handled by the server. All data of a site is shared by everyone who can open it.\n")
	b.WriteString("- `backyard init <site>` scaffolds a project, `backyard dev` previews it with a local backend, `backyard deploy` publishes it (overwriting the whole site). `backyard mcp` exposes the same as MCP tools.\n\n")

	b.WriteString("## Docs\n\n")
	writeLink(&b, docTitle("skill"), docURL(base, "skill"), refInfo["skill"].desc)
	for _, name := range referenceNames() {
		desc := ""
		if info, ok := refInfo[name]; ok {
			desc = info.desc
		}
		writeLink(&b, docTitle(name), docURL(base, name), desc)
	}
	b.WriteString("\n## Optional\n\n")
	writeLink(&b, "Full documentation", base+"/llms-full.txt", "the overview and every reference in one file")
	writeLink(&b, "TypeScript declarations", base+"/__backyard/backyard.d.ts", "the normative types of the browser SDK")
	writeLink(&b, "Human-readable docs", base+"/docs", "the same documentation rendered for people")
	return b.String()
}

func writeLink(b *strings.Builder, title, href, desc string) {
	b.WriteString("- [" + title + "](" + href + ")")
	if desc != "" {
		b.WriteString(": " + desc)
	}
	b.WriteString("\n")
}

// hubHost splits an absolute hub URL without a path into scheme and
// host[:port]; ok is false when site subdomains cannot be derived from it
// (a path, or an IP address as host).
func hubHost(base string) (scheme, host string, ok bool) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || (u.Path != "" && u.Path != "/") ||
		net.ParseIP(u.Hostname()) != nil {
		return "", "", false
	}
	return u.Scheme, u.Host, true
}

// buildLLMSFullTxt renders /llms-full.txt: SKILL.md (without its YAML front
// matter) followed by every reference, with the hub placeholder and
// cross-document links resolved against publicURL.
func buildLLMSFullTxt(publicURL string) string {
	base := baseURL(publicURL)
	known := map[string]bool{"skill": true}
	for _, n := range referenceNames() {
		known[n] = true
	}
	var b strings.Builder
	b.WriteString("<!-- Backyard documentation for agents: the skill overview followed by every reference. Source: " + docURL(base, "skill") + " -->\n\n")
	b.WriteString(strings.TrimSpace(stripFrontMatter(SkillMarkdown())))
	b.WriteString("\n")
	for _, name := range referenceNames() {
		b.WriteString("\n---\n\n<!-- Source: " + docURL(base, name) + " -->\n\n")
		b.WriteString(strings.TrimSpace(Reference(name)))
		b.WriteString("\n")
	}
	return Localize(rewriteDocLinks(b.String(), base, known), base)
}

// stripFrontMatter removes a leading YAML front matter block ("---" lines).
func stripFrontMatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[len("---\n"):]
	if strings.HasPrefix(rest, "---\n") {
		return rest[len("---\n"):]
	}
	if i := strings.Index(rest, "\n---\n"); i >= 0 {
		return rest[i+len("\n---\n"):]
	}
	if strings.HasSuffix(rest, "\n---") {
		return ""
	}
	return s
}

// docLinkRe matches markdown link targets to sibling documents:
// "](references/api.md#errors)", "](api.md)", "](./deploy.md)".
var docLinkRe = regexp.MustCompile(`\]\((?:\./)?(?:references/)?([A-Za-z0-9_-]+)\.md(#[^)\s]*)?\)`)

// rewriteDocLinks points links between the embedded documents at their hub
// URLs so they resolve once concatenated.
func rewriteDocLinks(s, base string, known map[string]bool) string {
	return docLinkRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := docLinkRe.FindStringSubmatch(m)
		name := strings.ToLower(sub[1])
		if !known[name] {
			return m
		}
		return "](" + docURL(base, name) + sub[2] + ")"
	})
}

// replaceHubURL replaces HubURLPlaceholder with base wherever the
// placeholder is not followed by more host characters (so a longer host
// such as "https://backyard.example.com.evil" is left alone).
func replaceHubURL(s, base string) string {
	var b strings.Builder
	b.Grow(len(s))
	for {
		i := strings.Index(s, HubURLPlaceholder)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(HubURLPlaceholder)
		b.WriteString(s[:i])
		if continuesHost(s, end) {
			b.WriteString(HubURLPlaceholder)
		} else {
			b.WriteString(base)
		}
		s = s[end:]
	}
}

// placeholderDomain is the host of HubURLPlaceholder; example sites are its
// subdomains ("https://<site>.backyard.example.com/", "*.backyard.example.com").
var placeholderDomain = strings.TrimPrefix(HubURLPlaceholder, "https://")

// siteSuffix ends every example site host: "<labels>.backyard.example.com".
var siteSuffix = "." + placeholderDomain

// replaceSiteHosts rewrites example site hosts under the placeholder domain
// to subdomains of host ("https://<site>.backyard.example.com/" →
// "<scheme>://<site>.<host>/"), mirroring host-mode URLs of the real hub.
// Labels are letters, digits, '*', '_', '-' and "<word>" placeholders; an
// "https://" right before them is replaced by scheme. Hosts that continue
// past the placeholder domain ("….backyard.example.com.evil.org") are left
// alone. It runs in linear time (no regular expression).
func replaceSiteHosts(s, scheme, host string) string {
	var b strings.Builder
	last := 0
	for from := 0; ; {
		k := strings.Index(s[from:], siteSuffix)
		if k < 0 {
			break
		}
		dot := from + k
		end := dot + len(siteSuffix)
		from = end
		start := subdomainStart(s, dot, last)
		if start == dot {
			continue
		}
		// Like a greedy match: the host ends at the last placeholder domain
		// within the run of labels ("a.backyard.example.com.b.backyard.example.com").
		run := runEnd(s, end)
		dot += strings.LastIndex(s[dot:run], siteSuffix)
		end = dot + len(siteSuffix)
		from = end
		if continuesHost(s, end) {
			continue
		}
		if b.Len() == 0 {
			b.Grow(len(s) + 64)
		}
		const https = "https://"
		if start-len(https) >= last && s[start-len(https):start] == https {
			b.WriteString(s[last : start-len(https)])
			b.WriteString(scheme + "://")
		} else {
			b.WriteString(s[last:start])
		}
		b.WriteString(s[start:dot])
		b.WriteString(".")
		b.WriteString(host)
		last = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// subdomainStart returns where the dot-separated labels ending at s[end]
// (exclusive) begin, never before floor; it returns end when there is none.
func subdomainStart(s string, end, floor int) int {
	start := end
	for {
		j := labelStart(s, start, floor)
		if j == start {
			return start
		}
		start = j
		// Continue over "label." only when a label precedes the dot.
		if start-1 <= floor || s[start-1] != '.' || labelStart(s, start-1, floor) == start-1 {
			return start
		}
		start--
	}
}

// labelStart returns where the host label ending at s[end] (exclusive)
// begins, never before floor; it returns end when there is none.
func labelStart(s string, end, floor int) int {
	i := end
	for i > floor {
		c := s[i-1]
		switch {
		case isAlnum(c) || c == '*' || c == '_' || c == '-':
			i--
		case c == '>':
			j := i - 2
			for j >= floor && s[j] >= 'a' && s[j] <= 'z' {
				j--
			}
			if j < floor || j == i-2 || s[j] != '<' {
				return i
			}
			i = j
		default:
			return i
		}
	}
	return i
}

// runEnd returns where the run of host labels and dots continuing at
// s[start] ends ("x.y<z>" in "x.y<z>/"), or start when it does not continue.
func runEnd(s string, start int) int {
	pos := start
	for pos < len(s) {
		if e := labelEnd(s, pos); e > pos {
			pos = e
			continue
		}
		if s[pos] == '.' {
			if e := labelEnd(s, pos+1); e > pos+1 {
				pos = e
				continue
			}
		}
		break
	}
	return pos
}

// labelEnd returns where the host label starting at s[start] ends; it
// returns start when there is none.
func labelEnd(s string, start int) int {
	i := start
	for i < len(s) {
		c := s[i]
		switch {
		case isAlnum(c) || c == '*' || c == '_' || c == '-':
			i++
		case c == '<':
			j := i + 1
			for j < len(s) && s[j] >= 'a' && s[j] <= 'z' {
				j++
			}
			if j == i+1 || j >= len(s) || s[j] != '>' {
				return i
			}
			i = j + 1
		default:
			return i
		}
	}
	return i
}

// continuesHost reports whether s[i:] continues a hostname.
func continuesHost(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	switch c := s[i]; {
	case isAlnum(c) || c == '-' || c == '_':
		return true
	case c == '.':
		return i+1 < len(s) && isAlnum(s[i+1])
	}
	return false
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
