package templates

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"text/template"

	"github.com/thasami/backyard/skills"
)

// Template names accepted by Render.
const (
	// TemplateBlank is a single index.html starter (the default).
	TemplateBlank = "blank"
	// TemplateApp is an app skeleton: index.html, app.js and style.css.
	TemplateApp = "app"
	// DefaultTemplate is used when InitOptions.Template is empty.
	DefaultTemplate = TemplateBlank
)

// Agent integrations accepted in InitOptions.Agents.
const (
	AgentClaude  = "claude"  // CLAUDE.md + .claude/skills/backyard/**
	AgentCodex   = "codex"   // AGENTS.md
	AgentCursor  = "cursor"  // .cursor/rules/backyard.mdc
	AgentCopilot = "copilot" // .github/copilot-instructions.md
)

// KnownAgents returns every agent integration Render supports.
func KnownAgents() []string { return []string{AgentClaude, AgentCodex, AgentCursor, AgentCopilot} }

// DefaultAgents returns the agents used when InitOptions.Agents is empty.
func DefaultAgents() []string { return []string{AgentClaude, AgentCodex} }

// Output paths of the generated files.
const (
	ProjectFile    = ".backyard.json"
	IgnoreFile     = ".backyardignore"
	ClaudeFile     = "CLAUDE.md"
	ClaudeSkillDir = ".claude/skills/backyard"
	AgentsFile     = "AGENTS.md"
	CursorRuleFile = ".cursor/rules/backyard.mdc"
	CopilotFile    = ".github/copilot-instructions.md"
)

// sources holds the template sources (templates/files/**).
//
//go:embed all:files
var sources embed.FS

// pageFiles maps each template to its output files and sources.
var pageFiles = map[string][]struct{ out, src string }{
	TemplateBlank: {
		{"index.html", "files/blank/index.html.tmpl"},
	},
	TemplateApp: {
		{"index.html", "files/app/index.html.tmpl"},
		{"app.js", "files/app/app.js"},
		{"style.css", "files/app/style.css"},
	},
}

// agentFiles maps each agent to its instruction file.
var agentFiles = map[string]struct{ out, src string }{
	AgentClaude:  {ClaudeFile, "files/agents/CLAUDE.md.tmpl"},
	AgentCodex:   {AgentsFile, "files/agents/AGENTS.md.tmpl"},
	AgentCursor:  {CursorRuleFile, "files/agents/backyard.mdc.tmpl"},
	AgentCopilot: {CopilotFile, "files/agents/copilot-instructions.md.tmpl"},
}

const ignoreSource = "files/project/backyardignore"

// tmplData is the data every template is executed with.
type tmplData struct {
	Site       string // "lunch-vote"
	Title      string // "Lunch Vote"
	Server     string // hub URL without trailing slash, or ""
	HostURL    string // host-mode site URL guess ("" if unknown)
	PathURL    string // path-mode site URL guess ("" if unknown)
	SiteURL    string // the site URL when the server's URL mode is known
	Template   string // blank | app
	IndexFiles string // markdown list of the page files
	Claude     bool   // the Claude skill is installed
	Codex      bool   // AGENTS.md is written
	CheatSheet string // API cheat sheet (fenced code) from SKILL.md
	Decision   string // "which API for what" table from SKILL.md
	Limits     string // limits table from SKILL.md
}

var siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// parsed parses every .tmpl source once into one template set, so the
// partials defined in files/agents/partials.tmpl are shared.
var parsed = sync.OnceValues(func() (*template.Template, error) {
	root := template.New("").Option("missingkey=error")
	err := fs.WalkDir(sources, "files", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".tmpl" {
			return err
		}
		b, err := sources.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = root.New(p).Parse(string(b))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("templates: parse: %w", err)
	}
	return root, nil
})

func render(opts InitOptions) (map[string][]byte, error) {
	site := strings.TrimSpace(opts.Site)
	if !siteNameRe.MatchString(site) {
		return nil, fmt.Errorf("templates: invalid site name %q (lowercase letters, digits and '-', at most 63 characters)", opts.Site)
	}
	tmplName, err := normalizeTemplate(opts.Template)
	if err != nil {
		return nil, err
	}
	agents, err := normalizeAgents(opts.Agents)
	if err != nil {
		return nil, err
	}
	server, err := normalizeServer(opts.Server)
	if err != nil {
		return nil, err
	}
	set, err := parsed()
	if err != nil {
		return nil, err
	}

	data := newData(site, server, tmplName, agents)
	data.setSiteURLs(site, server, opts.BaseDomain, opts.URLMode)
	out := map[string][]byte{}

	project, err := json.MarshalIndent(map[string]string{"site": site}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("templates: encode %s: %w", ProjectFile, err)
	}
	out[ProjectFile] = append(project, '\n')

	ignore, err := sources.ReadFile(ignoreSource)
	if err != nil {
		return nil, fmt.Errorf("templates: read %s: %w", ignoreSource, err)
	}
	out[IgnoreFile] = ignore

	if !opts.SkipIndex {
		for _, f := range pageFiles[tmplName] {
			b, err := produce(set, f.src, data)
			if err != nil {
				return nil, err
			}
			out[f.out] = b
		}
	}

	for _, agent := range agents {
		f := agentFiles[agent]
		b, err := produce(set, f.src, data)
		if err != nil {
			return nil, err
		}
		out[f.out] = tidyMarkdown(b)
		if agent == AgentClaude {
			if err := addSkill(out); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// produce executes a .tmpl source, or returns any other source verbatim.
func produce(set *template.Template, src string, data *tmplData) ([]byte, error) {
	if path.Ext(src) != ".tmpl" {
		b, err := sources.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("templates: read %s: %w", src, err)
		}
		return b, nil
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, src, data); err != nil {
		return nil, fmt.Errorf("templates: render %s: %w", src, err)
	}
	return buf.Bytes(), nil
}

// addSkill copies the embedded Backyard skill to .claude/skills/backyard/.
func addSkill(out map[string][]byte) error {
	skill, err := fs.Sub(skills.FS(), "backyard")
	if err != nil {
		return fmt.Errorf("templates: skill: %w", err)
	}
	n := 0
	err = fs.WalkDir(skill, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := fs.ReadFile(skill, p)
		if err != nil {
			return err
		}
		out[ClaudeSkillDir+"/"+p] = b
		n++
		return nil
	})
	if err != nil {
		return fmt.Errorf("templates: copy skill: %w", err)
	}
	if n == 0 {
		return errors.New("templates: the embedded skill is empty")
	}
	return nil
}

func normalizeTemplate(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return DefaultTemplate, nil
	}
	if _, ok := pageFiles[name]; !ok {
		return "", fmt.Errorf("templates: unknown template %q (choose from %s)", name, strings.Join(Templates(), ", "))
	}
	return name, nil
}

// normalizeAgents lowercases, de-duplicates and validates the agent list
// (entries may also be comma-separated). Empty means DefaultAgents.
func normalizeAgents(list []string) ([]string, error) {
	var agents []string
	for _, item := range list {
		for a := range strings.SplitSeq(item, ",") {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" {
				continue
			}
			if _, ok := agentFiles[a]; !ok {
				return nil, fmt.Errorf("templates: unknown agent %q (choose from %s)", a, strings.Join(KnownAgents(), ", "))
			}
			if !slices.Contains(agents, a) {
				agents = append(agents, a)
			}
		}
	}
	if len(agents) == 0 {
		return DefaultAgents(), nil
	}
	return agents, nil
}

// normalizeServer validates the hub URL and strips a trailing slash. Only
// plain http(s) URLs are accepted, so the value is safe in HTML attributes
// and Markdown.
func normalizeServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	bad := func() error {
		return fmt.Errorf("templates: invalid server URL %q (want http(s)://host[:port][/path])", raw)
	}
	if strings.ContainsAny(raw, " \t\r\n\"'<>`\\{}|^") {
		return "", bad()
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", bad()
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/"), nil
}

func newData(site, server, tmplName string, agents []string) *tmplData {
	d := &tmplData{
		Site:     site,
		Title:    titleFromSite(site),
		Server:   server,
		Template: tmplName,
		Claude:   slices.Contains(agents, AgentClaude),
		Codex:    slices.Contains(agents, AgentCodex),
	}
	names := make([]string, 0, len(pageFiles[tmplName]))
	for _, f := range pageFiles[tmplName] {
		names = append(names, "`"+f.out+"`")
	}
	d.IndexFiles = strings.Join(names, ", ")
	md := skills.SkillMarkdown()
	d.CheatSheet = orFallback(firstFence(section(md, "API cheat sheet")))
	d.Decision = orFallback(firstTable(section(md, "Which API for what")))
	d.Limits = orFallback(firstTable(section(md, "Limits")))
	return d
}

// hostnameRe accepts plain DNS names (safe in Markdown and HTML).
var hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// setSiteURLs fills the site URLs. In path mode a site lives at
// <hub>/s/<site>/; in host mode at <scheme>://<site>.<base_domain>[:port]/
// with the hub's scheme and port — base_domain may differ from the hub's
// host. Without the server's base_domain, the hub's host is assumed (no
// host-mode URL for a hub with a path or an IP address); without its URL
// mode, both forms are given.
func (d *tmplData) setSiteURLs(site, server, baseDomain, urlMode string) {
	if server == "" {
		return
	}
	d.PathURL = server + "/s/" + site + "/"
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return
	}
	base := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(baseDomain)), ".")
	if h, _, err := net.SplitHostPort(base); err == nil {
		base = h
	}
	switch {
	case urlMode == "path":
	case base != "" && hostnameRe.MatchString(base) && net.ParseIP(base) == nil:
		host := site + "." + base
		if p := u.Port(); p != "" {
			host = net.JoinHostPort(host, p)
		}
		d.HostURL = u.Scheme + "://" + host + "/"
	case u.Path == "" && net.ParseIP(u.Hostname()) == nil:
		d.HostURL = u.Scheme + "://" + site + "." + u.Host + "/"
	}
	switch urlMode {
	case "path":
		d.SiteURL = d.PathURL
	case "host":
		d.SiteURL = d.HostURL
	}
}

// orFallback keeps the agent files useful even if SKILL.md is reorganized
// (the package tests make sure this never happens unnoticed).
func orFallback(s string) string {
	if strings.TrimSpace(s) == "" {
		return "See the Backyard documentation on your hub (`/docs`, `/llms-full.txt`)."
	}
	return s
}

// titleFromSite turns "lunch-vote" into "Lunch Vote".
func titleFromSite(site string) string {
	words := strings.FieldsFunc(site, func(r rune) bool { return r == '-' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	if len(words) == 0 {
		return site
	}
	return strings.Join(words, " ")
}

// section returns the body of the "## <heading>…" section of md (up to the
// next level-2 heading), or "".
func section(md, heading string) string {
	lines := strings.Split(md, "\n")
	inFence := false
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(line, "## ") {
			continue
		}
		if start >= 0 {
			return strings.Join(lines[start:i], "\n")
		}
		if strings.HasPrefix(strings.TrimPrefix(line, "## "), heading) {
			start = i + 1
		}
	}
	if start >= 0 {
		return strings.Join(lines[start:], "\n")
	}
	return ""
}

// firstFence returns the first fenced code block of s, fences included.
func firstFence(s string) string {
	lines := strings.Split(s, "\n")
	start := -1
	for i, line := range lines {
		if !strings.HasPrefix(line, "```") {
			continue
		}
		if start < 0 {
			start = i
			continue
		}
		return strings.Join(lines[start:i+1], "\n")
	}
	return ""
}

// firstTable returns the first Markdown table (consecutive "|" lines) of s.
func firstTable(s string) string {
	var rows []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "|") {
			rows = append(rows, line)
			continue
		}
		if len(rows) > 0 {
			break
		}
	}
	return strings.Join(rows, "\n")
}

var blankRunRe = regexp.MustCompile(`\n{3,}`)

// tidyMarkdown collapses runs of blank lines left by template actions and
// ends the document with exactly one newline.
func tidyMarkdown(b []byte) []byte {
	s := blankRunRe.ReplaceAllString(string(b), "\n\n")
	return []byte(strings.TrimSpace(s) + "\n")
}
