package templates

import (
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thasami/backyard/skills"
)

const sdkScript = `<script src="/__backyard/backyard.js"></script>`

// skillFiles returns the output paths of the embedded skill.
func skillFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(skills.FS(), "backyard", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, ".claude/skills/"+p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustRender(t *testing.T, o InitOptions) map[string][]byte {
	t.Helper()
	files, err := Render(o)
	if err != nil {
		t.Fatalf("Render(%+v): %v", o, err)
	}
	return files
}

func TestTemplates(t *testing.T) {
	got := Templates()
	if !slices.Equal(got, []string{"blank", "app"}) {
		t.Fatalf("Templates() = %v", got)
	}
	got[0] = "x"
	if Templates()[0] != "blank" {
		t.Fatal("Templates returns a shared slice")
	}
}

func TestRenderFileSets(t *testing.T) {
	skill := skillFiles(t)
	if len(skill) < 4 {
		t.Fatalf("embedded skill has %d files", len(skill))
	}
	base := []string{".backyard.json", ".backyardignore"}
	blank := []string{"index.html"}
	app := []string{"index.html", "app.js", "style.css"}
	cat := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		slices.Sort(out)
		return out
	}
	tests := []struct {
		name string
		opts InitOptions
		want []string
	}{
		{"defaults", InitOptions{Site: "lunch-vote"}, cat(base, blank, []string{"CLAUDE.md", "AGENTS.md"}, skill)},
		{"explicit default agents", InitOptions{Site: "a", Agents: []string{"claude", "codex"}}, cat(base, blank, []string{"CLAUDE.md", "AGENTS.md"}, skill)},
		{"codex only", InitOptions{Site: "a", Agents: []string{"codex"}}, cat(base, blank, []string{"AGENTS.md"})},
		{"claude only", InitOptions{Site: "a", Agents: []string{"claude"}}, cat(base, blank, []string{"CLAUDE.md"}, skill)},
		{"cursor", InitOptions{Site: "a", Agents: []string{"cursor"}}, cat(base, blank, []string{".cursor/rules/backyard.mdc"})},
		{"copilot", InitOptions{Site: "a", Agents: []string{"copilot"}}, cat(base, blank, []string{".github/copilot-instructions.md"})},
		{"all, comma separated and mixed case", InitOptions{Site: "a", Agents: []string{"Claude,codex", " CURSOR ", "copilot", "codex"}},
			cat(base, blank, []string{"CLAUDE.md", "AGENTS.md", ".cursor/rules/backyard.mdc", ".github/copilot-instructions.md"}, skill)},
		{"app template", InitOptions{Site: "a", Template: "app", Agents: []string{"codex"}}, cat(base, app, []string{"AGENTS.md"})},
		{"app template, upper case", InitOptions{Site: "a", Template: " APP ", Agents: []string{"codex"}}, cat(base, app, []string{"AGENTS.md"})},
		{"skip index", InitOptions{Site: "a", SkipIndex: true, Agents: []string{"codex"}}, cat(base, []string{"AGENTS.md"})},
		{"skip index app", InitOptions{Site: "a", Template: "app", SkipIndex: true, Agents: []string{"cursor"}}, cat(base, []string{".cursor/rules/backyard.mdc"})},
		{"empty agent entries mean default", InitOptions{Site: "a", Agents: []string{"", " , "}}, cat(base, blank, []string{"CLAUDE.md", "AGENTS.md"}, skill)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := mustRender(t, tt.opts)
			got := slices.Sorted(maps.Keys(files))
			if !slices.Equal(got, tt.want) {
				t.Fatalf("files = %v\nwant    %v", got, tt.want)
			}
			for p, b := range files {
				if len(b) == 0 {
					t.Errorf("%s is empty", p)
				}
				if strings.HasPrefix(p, "/") || strings.Contains(p, "..") || strings.Contains(p, `\`) {
					t.Errorf("unsafe path %q", p)
				}
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	tests := []struct {
		name string
		opts InitOptions
		want string
	}{
		{"empty site", InitOptions{}, "invalid site name"},
		{"upper case site", InitOptions{Site: "Lunch"}, "invalid site name"},
		{"site with dot", InitOptions{Site: "a.b"}, "invalid site name"},
		{"site with slash", InitOptions{Site: "../x"}, "invalid site name"},
		{"leading dash", InitOptions{Site: "-a"}, "invalid site name"},
		{"too long", InitOptions{Site: strings.Repeat("a", 64)}, "invalid site name"},
		{"unknown agent", InitOptions{Site: "a", Agents: []string{"claude", "vim"}}, `unknown agent "vim"`},
		{"unknown template", InitOptions{Site: "a", Template: "react"}, `unknown template "react"`},
		{"server without scheme", InitOptions{Site: "a", Server: "backyard.acme.com"}, "invalid server URL"},
		{"server ftp", InitOptions{Site: "a", Server: "ftp://backyard.acme.com"}, "invalid server URL"},
		{"server with quote", InitOptions{Site: "a", Server: `https://x.com/"><script>`}, "invalid server URL"},
		{"server with query", InitOptions{Site: "a", Server: "https://x.com/?a=b"}, "invalid server URL"},
		{"server with credentials", InitOptions{Site: "a", Server: "https://u:p@x.com"}, "invalid server URL"},
		{"server javascript", InitOptions{Site: "a", Server: "javascript:alert(1)"}, "invalid server URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := Render(tt.opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if files != nil {
				t.Fatal("files returned with an error")
			}
		})
	}
}

func TestRenderHTML(t *testing.T) {
	for _, tmpl := range Templates() {
		t.Run(tmpl, func(t *testing.T) {
			files := mustRender(t, InitOptions{Site: "lunch-vote", Template: tmpl, Server: "https://backyard.acme.com/"})
			html := string(files["index.html"])
			for _, want := range []string{"<!doctype html>", sdkScript, "<title>Lunch Vote</title>", `name="viewport"`, `name="description"`} {
				if !strings.Contains(html, want) {
					t.Errorf("index.html lacks %q", want)
				}
			}
			if strings.Contains(html, "{{") || strings.Contains(html, "<no value>") {
				t.Error("index.html contains unrendered template syntax")
			}
			// The SDK must come before any app code.
			if i, j := strings.Index(html, sdkScript), strings.Index(html, `<script type="module"`); j >= 0 && j < i {
				t.Error("app script is loaded before the SDK")
			}
			// Own assets are referenced relatively (works in path mode).
			if regexp.MustCompile(`(?:src|href)="/[^_]`).MatchString(html) {
				t.Error("index.html references a root-absolute asset")
			}
			if regexp.MustCompile(`(?:src="|<link[^>]*href=")(?:https?:)?//`).MatchString(html) {
				t.Error("index.html loads an external resource")
			}
		})
	}
	app := mustRender(t, InitOptions{Site: "x", Template: "app"})
	html := string(app["index.html"])
	for _, ref := range []string{`href="style.css"`, `src="app.js"`} {
		if !strings.Contains(html, ref) {
			t.Errorf("app index.html lacks %s", ref)
		}
	}
	if js := string(app["app.js"]); !strings.Contains(js, "backyard.me()") || !strings.Contains(js, ".watch(") {
		t.Error("app.js does not demonstrate me() and watch()")
	}
	blank := string(mustRender(t, InitOptions{Site: "x"})["index.html"])
	for _, want := range []string{"backyard.me()", "backyard.db.collection(", ".watch(", ".create("} {
		if !strings.Contains(blank, want) {
			t.Errorf("blank index.html does not use %s", want)
		}
	}
}

func TestRenderServer(t *testing.T) {
	files := mustRender(t, InitOptions{Site: "lunch-vote", Server: " https://backyard.acme.com/ ", Agents: KnownAgents()})
	claude := string(files["CLAUDE.md"])
	for _, want := range []string{
		"https://backyard.acme.com", "https://lunch-vote.backyard.acme.com/", "https://backyard.acme.com/s/lunch-vote/",
		"backyard login --server https://backyard.acme.com",
	} {
		if !strings.Contains(claude, want) {
			t.Errorf("CLAUDE.md lacks %q", want)
		}
	}
	if strings.Contains(claude, "https://backyard.acme.com//") {
		t.Error("double slash after the server URL")
	}
	if !strings.Contains(string(files["index.html"]), `href="https://backyard.acme.com/docs"`) {
		t.Error("index.html does not link the hub docs")
	}
	// A hub with a path has no host-mode site URL.
	pathed := string(mustRender(t, InitOptions{Site: "x", Server: "https://acme.com/backyard", Agents: []string{"codex"}})["AGENTS.md"])
	if !strings.Contains(pathed, "https://acme.com/backyard/s/x/") || strings.Contains(pathed, "https://x.acme.com") {
		t.Errorf("unexpected site URLs for a hub with a path:\n%s", pathed)
	}
	// Neither has a hub addressed by IP.
	for _, srv := range []string{"http://127.0.0.1:8080", "http://[::1]:8080"} {
		ip := string(mustRender(t, InitOptions{Site: "x", Server: srv, Agents: []string{"claude"}})["CLAUDE.md"])
		host := strings.TrimPrefix(srv, "http://")
		if !strings.Contains(ip, srv+"/s/x/") || strings.Contains(ip, "x."+host) || strings.Contains(ip, "(host mode)") {
			t.Errorf("unexpected site URLs for the hub %s:\n%s", srv, ip)
		}
	}
	// With the server's /info, the exact URL: sites live under base_domain
	// (which may differ from the hub's host), with the hub's scheme and port.
	for _, tc := range []struct {
		opts      InitOptions
		want, not string
	}{
		{InitOptions{Server: "https://hub.acme.com", BaseDomain: "apps.acme.com", URLMode: "host"},
			"This site once deployed: https://x.apps.acme.com/;", "x.hub.acme.com"},
		{InitOptions{Server: "http://hub.acme.test:8080", BaseDomain: "Apps.Acme.Test.", URLMode: "host"},
			"This site once deployed: http://x.apps.acme.test:8080/;", "(path mode)"},
		{InitOptions{Server: "https://hub.acme.com", BaseDomain: "apps.acme.com", URLMode: "path"},
			"This site once deployed: https://hub.acme.com/s/x/;", "x.apps.acme.com"},
		{InitOptions{Server: "https://hub.acme.com", BaseDomain: "apps.acme.com"},
			"https://x.apps.acme.com/ (host mode) or https://hub.acme.com/s/x/ (path mode)", "x.hub.acme.com"},
		{InitOptions{Server: "http://127.0.0.1:8080", BaseDomain: "127.0.0.1", URLMode: "path"},
			"This site once deployed: http://127.0.0.1:8080/s/x/;", "x.127"},
		{InitOptions{Server: "https://hub.acme.com", BaseDomain: "evil.com\"><script>", URLMode: "host"},
			"https://x.hub.acme.com/", "evil.com"},
	} {
		tc.opts.Site, tc.opts.Agents = "x", []string{"codex"}
		doc := string(mustRender(t, tc.opts)["AGENTS.md"])
		if !strings.Contains(doc, tc.want) || strings.Contains(doc, tc.not) {
			t.Errorf("%+v: AGENTS.md lacks %q or has %q:\n%s", tc.opts, tc.want, tc.not, doc)
		}
	}
	// Without a server nothing hub-specific is invented.
	none := mustRender(t, InitOptions{Site: "x", Agents: KnownAgents()})
	for p, b := range none {
		if strings.Contains(string(b), "backyard.acme.com") {
			t.Errorf("%s mentions a server although none was given", p)
		}
	}
	if !strings.Contains(string(none["AGENTS.md"]), "backyard login --server <hub URL>") {
		t.Error("AGENTS.md lacks the generic login hint")
	}
}

func TestProjectFile(t *testing.T) {
	files := mustRender(t, InitOptions{Site: "lunch-vote"})
	var p map[string]any
	if err := json.Unmarshal(files[".backyard.json"], &p); err != nil {
		t.Fatal(err)
	}
	if len(p) != 1 || p["site"] != "lunch-vote" {
		t.Fatalf(".backyard.json = %v", p)
	}
	if !strings.HasSuffix(string(files[".backyard.json"]), "}\n") {
		t.Error(".backyard.json lacks a trailing newline")
	}
	ignore := string(files[".backyardignore"])
	if !strings.Contains(ignore, "*.pem") || !strings.HasPrefix(ignore, "#") {
		t.Errorf(".backyardignore = %q", ignore)
	}
}

func TestAgentDocs(t *testing.T) {
	files := mustRender(t, InitOptions{Site: "lunch-vote", Agents: KnownAgents(), Server: "https://backyard.acme.com"})
	shared := []string{
		"lunch-vote", sdkScript, "Never put secrets", "No server code", "backyard deploy", "backyard dev",
		"http://lunch-vote.localhost:4321/", "Deploy = overwrite", "backyard.me()", "watch(",
	}
	selfContained := []string{
		// the cheat sheet, decision table and limits extracted from SKILL.md
		"backyard.db.collection(", "backyard.files.upload(", "backyard.ai.chat(", "backyard.ai.stream(",
		"backyard.warehouse.named(", "backyard.socket.room(", "room.updateState(", "| Need | Use |", "Document size",
		"https://backyard.acme.com/llms-full.txt",
	}
	for _, p := range []string{"CLAUDE.md", "AGENTS.md", ".cursor/rules/backyard.mdc", ".github/copilot-instructions.md"} {
		doc := string(files[p])
		want := shared
		if p != "CLAUDE.md" {
			want = append(slices.Clone(shared), selfContained...)
		}
		for _, w := range want {
			if !strings.Contains(doc, w) {
				t.Errorf("%s lacks %q", p, w)
			}
		}
		if strings.Contains(doc, "<no value>") || strings.Contains(doc, "{{") {
			t.Errorf("%s has unrendered template syntax", p)
		}
		if strings.Contains(doc, "\n\n\n") || !strings.HasSuffix(doc, "\n") || strings.HasSuffix(doc, "\n\n") {
			t.Errorf("%s is not tidy markdown", p)
		}
		if strings.Count(doc, "```")%2 != 0 {
			t.Errorf("%s has an unterminated code fence", p)
		}
	}
	claude := string(files["CLAUDE.md"])
	if !strings.Contains(claude, ".claude/skills/backyard/SKILL.md") || !strings.Contains(claude, "references/api.md") {
		t.Error("CLAUDE.md does not point at the skill")
	}
	if strings.Count(claude, "\n") > 120 {
		t.Errorf("CLAUDE.md should stay concise (%d lines)", strings.Count(claude, "\n"))
	}
	if !strings.Contains(string(files["AGENTS.md"]), ".claude/skills/backyard/") {
		t.Error("AGENTS.md should mention the installed skill when claude is selected")
	}
	if strings.Contains(string(mustRender(t, InitOptions{Site: "x", Agents: []string{"codex"}})["AGENTS.md"]), ".claude/skills/") {
		t.Error("AGENTS.md mentions the skill although it is not installed")
	}

	// Cursor rule: front matter with alwaysApply.
	mdc := string(files[".cursor/rules/backyard.mdc"])
	if !strings.HasPrefix(mdc, "---\ndescription: \"") || !strings.Contains(mdc, "\nalwaysApply: true\n---\n") {
		t.Errorf("unexpected Cursor rule front matter:\n%.200s", mdc)
	}
	// Copilot instructions are plain Markdown (no front matter).
	if strings.HasPrefix(string(files[".github/copilot-instructions.md"]), "---") {
		t.Error("copilot-instructions.md must not have front matter")
	}
}

func TestSkillCopied(t *testing.T) {
	files := mustRender(t, InitOptions{Site: "a", Agents: []string{"claude"}})
	if got := string(files[".claude/skills/backyard/SKILL.md"]); got != skills.SkillMarkdown() {
		t.Fatal("SKILL.md copy differs from the embedded skill")
	}
	for _, n := range skills.References() {
		if string(files[".claude/skills/backyard/references/"+n+".md"]) != skills.Reference(n) {
			t.Errorf("reference %s not copied", n)
		}
	}
}

func TestRenderIndependentResults(t *testing.T) {
	a := mustRender(t, InitOptions{Site: "a"})
	a["index.html"][0] = 'X'
	a[".backyardignore"][0] = 'X'
	b := mustRender(t, InitOptions{Site: "a"})
	if b["index.html"][0] == 'X' || b[".backyardignore"][0] == 'X' {
		t.Fatal("Render results share memory")
	}
}

func TestExtractors(t *testing.T) {
	md := "# T\n\n## API cheat sheet\n\nintro\n\n```js\na()\n## not a heading\n```\n\n## Which API for what\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\ntext\n\n| x |\n\n## Limits (defaults)\n\n| l |\n"
	if got := firstFence(section(md, "API cheat sheet")); got != "```js\na()\n## not a heading\n```" {
		t.Errorf("cheat sheet = %q", got)
	}
	if got := firstTable(section(md, "Which API for what")); got != "| a | b |\n|---|---|\n| 1 | 2 |" {
		t.Errorf("decision = %q", got)
	}
	if got := firstTable(section(md, "Limits")); got != "| l |" {
		t.Errorf("limits = %q", got)
	}
	if section(md, "Missing") != "" || firstFence("no fence") != "" || firstTable("no table") != "" {
		t.Error("extractors should return empty strings when nothing matches")
	}
	if orFallback("") == "" || orFallback("x") != "x" {
		t.Error("orFallback")
	}
	// The real SKILL.md must yield all three.
	d := newData("x", "", TemplateBlank, DefaultAgents())
	for name, s := range map[string]string{"cheat sheet": d.CheatSheet, "decision": d.Decision, "limits": d.Limits} {
		if strings.HasPrefix(s, "See the Backyard documentation") {
			t.Errorf("%s could not be extracted from SKILL.md", name)
		}
	}
}

func TestTitleFromSite(t *testing.T) {
	for in, want := range map[string]string{"lunch-vote": "Lunch Vote", "kpi": "Kpi", "q3-okrs-2026": "Q3 Okrs 2026", "a": "A"} {
		if got := titleFromSite(in); got != want {
			t.Errorf("titleFromSite(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeServer(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"https://backyard.acme.com", "https://backyard.acme.com"},
		{"https://backyard.acme.com///", "https://backyard.acme.com"},
		{"http://localhost:8080/", "http://localhost:8080"},
		{"https://acme.com/tools/backyard/", "https://acme.com/tools/backyard"},
	}
	for _, tt := range tests {
		got, err := normalizeServer(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("normalizeServer(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

var moduleScriptRe = regexp.MustCompile(`(?s)<script type="module">(.*?)</script>`)

// TestScriptsParse syntax-checks the generated JavaScript with node, when
// available.
func TestScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	check := func(name, src string) {
		f := filepath.Join(dir, name)
		if err := os.WriteFile(f, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Errorf("%s does not parse: %v\n%s", name, err, out)
		}
	}
	blank := string(mustRender(t, InitOptions{Site: "x"})["index.html"])
	m := moduleScriptRe.FindStringSubmatch(blank)
	if m == nil {
		t.Fatal("blank index.html has no module script")
	}
	check("blank.mjs", m[1])
	check("app.mjs", string(mustRender(t, InitOptions{Site: "x", Template: "app"})["app.js"]))
}
