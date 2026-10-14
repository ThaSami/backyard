package skills

import (
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestFSLayout(t *testing.T) {
	fsys := FS()
	for _, p := range []string{"backyard/SKILL.md", "backyard/references/api.md", "backyard/references/recipes.md", "backyard/references/deploy.md"} {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			t.Errorf("%s is empty", p)
		}
	}
	// The CLI installs the skill from fs.Sub(FS(), "backyard").
	sub, err := fs.Sub(fsys, "backyard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(sub, "SKILL.md"); err != nil {
		t.Fatalf("SKILL.md not at the root of the backyard subtree: %v", err)
	}
	// Only markdown documents are shipped (no editor droppings).
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && path.Ext(p) != ".md" {
			t.Errorf("unexpected non-markdown file in the skill: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// frontMatter parses the simple "key: value" YAML front matter of SKILL.md.
func frontMatter(t *testing.T, doc string) (map[string]string, string) {
	t.Helper()
	if !strings.HasPrefix(doc, "---\n") {
		t.Fatal("SKILL.md must start with a YAML front matter block")
	}
	end := strings.Index(doc[4:], "\n---\n")
	if end < 0 {
		t.Fatal("unterminated front matter")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(doc[4:4+end], "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("unexpected front matter line %q", line)
		}
		fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return fields, doc[4+end+5:]
}

func TestSkillFrontMatter(t *testing.T) {
	fields, body := frontMatter(t, SkillMarkdown())
	if fields["name"] != "backyard" {
		t.Errorf("name = %q, want backyard", fields["name"])
	}
	if !regexp.MustCompile(`^[a-z0-9-]{1,64}$`).MatchString(fields["name"]) {
		t.Errorf("name %q violates the Agent Skills naming rules", fields["name"])
	}
	desc := fields["description"]
	switch {
	case desc == "":
		t.Error("description is empty")
	case len(desc) > 1024:
		t.Errorf("description is %d chars, max 1024", len(desc))
	case strings.ContainsAny(desc, "<>"):
		t.Error("description must not contain XML tags")
	}
	for _, trigger := range []string{"dashboard", "prototype", "game", "poll", "backyard.js", "backyard deploy", "share"} {
		if !strings.Contains(desc, trigger) {
			t.Errorf("description does not mention %q", trigger)
		}
	}
	if n := strings.Count(body, "\n"); n > 450 {
		t.Errorf("SKILL.md body has %d lines; keep it under ~400 and move details to references", n)
	}
	for _, rule := range []string{"/__backyard/backyard.js", "Never put secrets", "No server code", "Deploy = overwrite", "backyard init", "backyard dev", "backyard deploy"} {
		if !strings.Contains(body, rule) {
			t.Errorf("SKILL.md body does not mention %q", rule)
		}
	}
}

func TestReference(t *testing.T) {
	api := Reference("api")
	if !strings.HasPrefix(api, "# Backyard API reference") {
		t.Fatalf("unexpected api.md start: %.40q", api)
	}
	tests := []struct {
		name string
		want string // "" = missing; otherwise the canonical name
	}{
		{"api", "api"},
		{"API", "api"},
		{"api.md", "api"},
		{" Deploy.MD ", "deploy"},
		{"recipes", "recipes"},
		{"", ""},
		{"skill", ""},
		{"SKILL", ""},
		{"nope", ""},
		{"../SKILL", ""},
		{"references/api", ""},
		{"api/../api", ""},
		{"./api", ""},
		{"api\x00", ""},
		{strings.Repeat("a", 65), ""},
	}
	for _, tt := range tests {
		got := Reference(tt.name)
		if tt.want == "" {
			if got != "" {
				t.Errorf("Reference(%q) = %.30q…, want empty", tt.name, got)
			}
			continue
		}
		if want := Reference(tt.want); got != want || got == "" {
			t.Errorf("Reference(%q) did not return %s.md", tt.name, tt.want)
		}
	}
}

func TestReferences(t *testing.T) {
	got := References()
	want := []string{"api", "recipes", "deploy"}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Fatalf("References() = %v, want prefix %v", got, want)
	}
	got[0] = "mutated"
	if References()[0] != "api" {
		t.Fatal("References returns a shared slice")
	}
	for _, n := range References() {
		if Reference(n) == "" {
			t.Errorf("listed reference %q cannot be read", n)
		}
	}
}

var mdLinkRe = regexp.MustCompile(`\]\(([^)\s]+)\)`)

// TestDocLinks checks that relative links between the documents resolve.
func TestDocLinks(t *testing.T) {
	docs := map[string]string{"backyard/SKILL.md": SkillMarkdown()}
	for _, n := range References() {
		docs["backyard/references/"+n+".md"] = Reference(n)
	}
	for p, doc := range docs {
		for _, m := range mdLinkRe.FindAllStringSubmatch(doc, -1) {
			target := m[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "#") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			file, frag, _ := strings.Cut(target, "#")
			if strings.HasPrefix(file, "/") || !strings.HasSuffix(file, ".md") {
				continue
			}
			resolved := path.Join(path.Dir(p), file)
			b, err := fs.ReadFile(FS(), resolved)
			if err != nil {
				t.Errorf("%s links to %s, which does not exist (%s)", p, target, resolved)
				continue
			}
			if frag != "" && !headingIDs(string(b))[frag] {
				t.Errorf("%s links to %s, but %s has no heading #%s", p, target, resolved, frag)
			}
		}
	}
}

var headingRe = regexp.MustCompile(`(?m)^#{1,6} (.+)$`)

// slug mirrors the hub docs renderer and GitHub for the headings we use.
func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = regexp.MustCompile(`[^\p{L}\p{N}\s_-]`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, "-")
	return s
}

// headingIDs returns the anchor ids of the headings of doc.
func headingIDs(doc string) map[string]bool {
	ids := map[string]bool{}
	for _, h := range headingRe.FindAllStringSubmatch(stripCodeBlocks(doc), -1) {
		ids[slug(strings.ReplaceAll(h[1], "`", ""))] = true
	}
	return ids
}

// TestAnchors checks in-document "#anchor" links against the headings.
func TestAnchors(t *testing.T) {
	docs := map[string]string{"SKILL.md": SkillMarkdown()}
	for _, n := range References() {
		docs[n+".md"] = Reference(n)
	}
	for name, doc := range docs {
		ids := headingIDs(doc)
		for _, m := range mdLinkRe.FindAllStringSubmatch(stripCodeBlocks(doc), -1) {
			if strings.HasPrefix(m[1], "#") && !ids[m[1][1:]] {
				t.Errorf("%s: anchor %s matches no heading", name, m[1])
			}
		}
	}
}

func stripCodeBlocks(s string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			in = !in
			continue
		}
		if !in {
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}

// TestSDKSurfaceDocumented guards against the API reference drifting from the
// SDK surface of SPEC §5.6.
func TestSDKSurfaceDocumented(t *testing.T) {
	api := Reference("api")
	skill := SkillMarkdown()
	surface := []string{
		"backyard.ready", "backyard.site", "backyard.configure", "autoLogin", "apiBase", "backyard.me()",
		"backyard.id.me()", "backyard.id.lookup(", "backyard.db.collection(", ".create(", ".get(", ".set(",
		".update(", ".delete(", ".list(", ".where(", ".orderBy(", ".limit(", ".offset(", ".first()", ".find(",
		".subscribe(", "onCreate", "onUpdate", "onDelete", "onChange", ".watch(", "backyard.db.collections()",
		"backyard.files.upload(", "backyard.files.list(", "backyard.files.get(", "backyard.files.delete(",
		"backyard.ai.chat(", "maxTokens", "temperature", "json: true", "backyard.ai.stream(", "onText", ".done",
		"backyard.ai.image(", "backyard.ai.embed(", "backyard.ai.models()",
		"backyard.warehouse.query(", "backyard.warehouse.named(", "backyard.warehouse.sources()",
		"backyard.socket.room(", `room.on("message"`, `"members"`, "room.members", "room.self", "room.send(",
		"echo", "room.setState(", "room.updateState(", "room.off(", "room.leave()", "backyard.socket.connected",
		"backyard.socket.on(", "BackyardError", "loginUrl",
		// query operators
		"`==`", "`!=`", "`<`", "`<=`", "`>`", "`>=`", "`in`", "`not-in`", "`contains`",
		// error codes
		"bad_request", "unauthenticated", "forbidden", "not_found", "conflict", "too_large", "rate_limited",
		"unsupported", "upstream_error", "internal",
		// wire protocol
		`"t":"hello"`, `"t":"sub"`, `"t":"unsub"`, `"t":"join"`, `"t":"leave"`, `"t":"state"`, `"t":"send"`,
		`"t":"ping"`, `"t":"db"`, `"t":"presence"`, `"t":"msg"`, `"t":"error"`, "X-Backyard: 1", "_meta",
	}
	for _, s := range surface {
		if !strings.Contains(api, s) {
			t.Errorf("api.md does not document %q", s)
		}
	}
	for _, s := range []string{"backyard.db", "backyard.files", "backyard.ai", "backyard.warehouse", "backyard.socket", "backyard.id", ".watch(", "backyard.ai.stream("} {
		if !strings.Contains(skill, s) {
			t.Errorf("SKILL.md cheat sheet does not mention %q", s)
		}
	}
}

var htmlBlockRe = regexp.MustCompile("(?s)```html\n(.*?)```")

// recipeApps returns the complete HTML apps embedded in recipes.md.
func recipeApps(t *testing.T) []string {
	t.Helper()
	var apps []string
	for _, m := range htmlBlockRe.FindAllStringSubmatch(Reference("recipes"), -1) {
		if strings.HasPrefix(m[1], "<!doctype html>") {
			apps = append(apps, m[1])
		}
	}
	return apps
}

func TestRecipes(t *testing.T) {
	apps := recipeApps(t)
	if len(apps) < 11 {
		t.Fatalf("recipes.md has %d complete apps, want at least 11", len(apps))
	}
	extURL := regexp.MustCompile(`(?:src|href)="(https?:)?//`)
	for i, app := range apps {
		if !strings.Contains(app, `<script src="/__backyard/backyard.js"></script>`) {
			t.Errorf("recipe app %d does not load the SDK with the absolute path", i+1)
		}
		if !strings.Contains(app, "<title>") || !strings.Contains(app, `name="viewport"`) {
			t.Errorf("recipe app %d lacks a <title> or viewport meta", i+1)
		}
		if strings.Count(app, "<script") != strings.Count(app, "</script>") {
			t.Errorf("recipe app %d has unbalanced <script> tags", i+1)
		}
		if extURL.MatchString(app) {
			t.Errorf("recipe app %d loads an external resource", i+1)
		}
		// Only the SPEC §5.6 surface: no SDK extras.
		for _, extra := range []string{".abort(", "retryAfter", "hasMore", "socket.cid", "socket.user"} {
			if strings.Contains(app, extra) {
				t.Errorf("recipe app %d uses %q, which is outside the documented core surface", i+1, extra)
			}
		}
	}
}

var moduleScriptRe = regexp.MustCompile(`(?s)<script type="module">(.*?)</script>`)

// TestRecipeScriptsParse syntax-checks every recipe's module script with
// node, when available.
func TestRecipeScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	n := 0
	for i, app := range recipeApps(t) {
		for j, m := range moduleScriptRe.FindAllStringSubmatch(app, -1) {
			n++
			f := filepath.Join(dir, "recipe"+string(rune('a'+i))+string(rune('0'+j))+".mjs")
			if err := os.WriteFile(f, []byte(m[1]), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
				t.Errorf("recipe app %d script %d does not parse: %v\n%s", i+1, j+1, err, out)
			}
		}
	}
	if n == 0 {
		t.Fatal("no module scripts found in recipes")
	}
}

func TestLLMSTxt(t *testing.T) {
	tests := []struct {
		publicURL string
		base      string
		siteURL   string // host-mode site URL pattern, "" when not derivable
	}{
		{"https://backyard.acme.com", "https://backyard.acme.com", "https://<site>.backyard.acme.com/"},
		{"https://backyard.acme.com/", "https://backyard.acme.com", "https://<site>.backyard.acme.com/"},
		{"  http://localhost:8080/  ", "http://localhost:8080", "http://<site>.localhost:8080/"},
		{"https://acme.com/backyard", "https://acme.com/backyard", ""},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", ""},
		{"http://[::1]:8080/", "http://[::1]:8080", ""},
		{"", "", ""},
	}
	for _, tt := range tests {
		txt := LLMSTxt(tt.publicURL)
		if tt.siteURL == "" && tt.base != "" && strings.Contains(txt, "://<site>.") {
			t.Errorf("%q: llms.txt invents host-mode site URLs for a hub without subdomains", tt.publicURL)
		}
		lines := strings.Split(txt, "\n")
		if lines[0] != "# Backyard" {
			t.Errorf("%q: first line %q, want an H1", tt.publicURL, lines[0])
		}
		if !strings.HasPrefix(lines[2], "> ") {
			t.Errorf("%q: missing blockquote summary", tt.publicURL)
		}
		for _, want := range []string{
			"## Docs", "## Optional",
			"](" + tt.base + "/docs/skill.md)", "](" + tt.base + "/docs/api.md)", "](" + tt.base + "/docs/recipes.md)",
			"](" + tt.base + "/docs/deploy.md)", "](" + tt.base + "/llms-full.txt)", "/__backyard/backyard.js",
		} {
			if !strings.Contains(txt, want) {
				t.Errorf("%q: llms.txt lacks %q", tt.publicURL, want)
			}
		}
		if tt.siteURL != "" && !strings.Contains(txt, tt.siteURL) {
			t.Errorf("%q: llms.txt does not explain host-mode site URLs (%s)", tt.publicURL, tt.siteURL)
		}
		if tt.base != "" && !strings.Contains(txt, tt.base+"/s/<site>/") {
			t.Errorf("%q: llms.txt does not explain path-mode site URLs", tt.publicURL)
		}
		if strings.Contains(txt, HubURLPlaceholder) {
			t.Errorf("%q: llms.txt contains the placeholder", tt.publicURL)
		}
		if strings.Contains(txt, "//docs") {
			t.Errorf("%q: double slash in links", tt.publicURL)
		}
	}
}

func TestLLMSFullTxt(t *testing.T) {
	const pub = "https://hub.acme.dev/"
	full := LLMSFullTxt(pub)
	if strings.Contains(full, "\nname: backyard\n") || strings.HasPrefix(full, "---") {
		t.Error("front matter was not stripped")
	}
	_, body := frontMatter(t, SkillMarkdown())
	firstHeading := strings.SplitN(strings.TrimSpace(body), "\n", 2)[0]
	if !strings.Contains(full, firstHeading) {
		t.Error("SKILL.md body missing")
	}
	for _, n := range References() {
		h := strings.SplitN(Reference(n), "\n", 2)[0]
		if !strings.Contains(full, h) {
			t.Errorf("reference %s missing", n)
		}
		if !strings.Contains(full, "Source: https://hub.acme.dev/docs/"+n+".md") {
			t.Errorf("reference %s has no source marker", n)
		}
	}
	if i, j := strings.Index(full, "# Backyard API reference"), strings.Index(full, "# Backyard recipes"); i < 0 || j < i {
		t.Error("references are not in reading order")
	}
	if strings.Contains(full, HubURLPlaceholder) {
		t.Error("hub placeholder not substituted")
	}
	if !strings.Contains(full, "https://hub.acme.dev/docs\n") && !strings.Contains(full, "https://hub.acme.dev/docs`") {
		t.Error("hub URL not substituted into the docs")
	}
	if strings.Contains(full, "](references/") || strings.Contains(full, "](api.md") || strings.Contains(full, "](deploy.md") {
		t.Error("relative document links were not rewritten")
	}
	if !strings.Contains(full, "](https://hub.acme.dev/docs/api.md)") {
		t.Error("expected rewritten link to the API reference")
	}
	if strings.Contains(full, "backyard.example.com") {
		t.Error("example site hosts were not rewritten to the hub's domain")
	}
	for _, want := range []string{"https://<site>.hub.acme.dev/", "https://hub.acme.dev/s/<site>/", "*.hub.acme.dev"} {
		if !strings.Contains(full, want) {
			t.Errorf("llms-full.txt lacks %q", want)
		}
	}

	// A hub URL with a path cannot have site subdomains: only the hub URL is replaced.
	pathed := LLMSFullTxt("https://acme.com/backyard")
	if strings.Contains(pathed, HubURLPlaceholder+"/") || !strings.Contains(pathed, "https://acme.com/backyard/s/<site>/") {
		t.Error("hub URL with a path not substituted")
	}

	// Without a public URL the placeholder stays and links are server-relative.
	rel := LLMSFullTxt("")
	if !strings.Contains(rel, HubURLPlaceholder) {
		t.Error("placeholder removed without a public URL")
	}
	if !strings.Contains(rel, "](/docs/api.md)") {
		t.Error("expected server-relative links without a public URL")
	}
}

func TestReplaceHubURL(t *testing.T) {
	const base = "https://hub.acme.dev"
	tests := []struct{ in, want string }{
		{"see https://backyard.example.com", "see " + base},
		{"https://backyard.example.com/s/foo/", base + "/s/foo/"},
		{"(https://backyard.example.com)", "(" + base + ")"},
		{"`https://backyard.example.com`.", "`" + base + "`."},
		{"end https://backyard.example.com.", "end " + base + "."},
		{"https://backyard.example.com.evil.org", "https://backyard.example.com.evil.org"},
		{"https://backyard.example.comx", "https://backyard.example.comx"},
		{"https://backyard.example.com-x", "https://backyard.example.com-x"},
		{"https://foo.backyard.example.com/", "https://foo.backyard.example.com/"},
		{"a https://backyard.example.com b https://backyard.example.com/x", "a " + base + " b " + base + "/x"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := replaceHubURL(tt.in, base); got != tt.want {
			t.Errorf("replaceHubURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReplaceSiteHosts(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://<site>.backyard.example.com/", "http://<site>.hub.test:8080/"},
		{"https://lunch-vote.backyard.example.com/__backyard/api", "http://lunch-vote.hub.test:8080/__backyard/api"},
		{"`*.backyard.example.com`", "`*.hub.test:8080`"},
		{"a.b.backyard.example.com.", "a.b.hub.test:8080."},
		{"https://foo.backyard.example.com.evil.org/", "https://foo.backyard.example.com.evil.org/"},
		{"https://backyard.example.com/", "https://backyard.example.com/"},
		{"no hosts here", "no hosts here"},
	}
	for _, tt := range tests {
		if got := replaceSiteHosts(tt.in, "http", "hub.test:8080"); got != tt.want {
			t.Errorf("replaceSiteHosts(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStripFrontMatter(t *testing.T) {
	tests := []struct{ in, want string }{
		{"---\nname: x\n---\nbody", "body"},
		{"---\n---\nbody", "body"},
		{"no front matter", "no front matter"},
		{"---\nunterminated", "---\nunterminated"},
		{"---\nname: x\n---", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripFrontMatter(tt.in); got != tt.want {
			t.Errorf("stripFrontMatter(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestRewriteDocLinks(t *testing.T) {
	known := map[string]bool{"api": true, "skill": true}
	tests := []struct{ in, want string }{
		{"[a](references/api.md)", "[a](B/docs/api.md)"},
		{"[a](api.md#errors)", "[a](B/docs/api.md#errors)"},
		{"[a](./api.md)", "[a](B/docs/api.md)"},
		{"[a](unknown.md)", "[a](unknown.md)"},
		{"[a](https://x/api.md)", "[a](https://x/api.md)"},
		{"[a](#local)", "[a](#local)"},
	}
	for _, tt := range tests {
		if got := rewriteDocLinks(tt.in, "B", known); got != tt.want {
			t.Errorf("rewriteDocLinks(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestDocTitle(t *testing.T) {
	for in, want := range map[string]string{"api": "API reference", "deploy": "Deploying", "data-model": "Data Model", "x_y": "X Y"} {
		if got := docTitle(in); got != want {
			t.Errorf("docTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBaseURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"https://hub.acme.dev", "https://hub.acme.dev"},
		{" https://hub.acme.dev/// ", "https://hub.acme.dev"},
		{"https://hub.acme.dev/?x=1#frag", "https://hub.acme.dev"},
		{"https://hub.acme.dev?", "https://hub.acme.dev"},
		{"https://user:secret@hub.acme.dev/", "https://hub.acme.dev"},
		{"https://acme.com/tools/backyard/", "https://acme.com/tools/backyard"},
		{"http://localhost:8080", "http://localhost:8080"},
		{"/relative/", "/relative"},
	}
	for _, tt := range tests {
		if got := baseURL(tt.in); got != tt.want {
			t.Errorf("baseURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// Credentials in a misconfigured public URL never reach the documents.
	for _, doc := range []string{LLMSTxt("https://u:hunter2@hub.acme.dev"), LLMSFullTxt("https://u:hunter2@hub.acme.dev")} {
		if strings.Contains(doc, "hunter2") {
			t.Fatal("credentials of the public URL leaked into the docs")
		}
	}
}

func TestLocalize(t *testing.T) {
	const md = "Hub https://backyard.example.com/docs, site https://lunch.backyard.example.com/ and " +
		"https://backyard.example.com/s/lunch/; see [api](references/api.md) and `*.backyard.example.com`."
	tests := []struct {
		name, publicURL, want string
	}{
		{"empty keeps the placeholder", "", md},
		{"host mode", "https://hub.acme.dev/", "Hub https://hub.acme.dev/docs, site https://lunch.hub.acme.dev/ and " +
			"https://hub.acme.dev/s/lunch/; see [api](references/api.md) and `*.hub.acme.dev`."},
		{"dev hub with port", "http://localhost:8080", "Hub http://localhost:8080/docs, site http://lunch.localhost:8080/ and " +
			"http://localhost:8080/s/lunch/; see [api](references/api.md) and `*.localhost:8080`."},
		{"IP hub has no site subdomains", "http://127.0.0.1:9000", "Hub http://127.0.0.1:9000/docs, site https://lunch.backyard.example.com/ and " +
			"http://127.0.0.1:9000/s/lunch/; see [api](references/api.md) and `*.backyard.example.com`."},
		{"hub with a path has no site subdomains", "https://acme.com/backyard", "Hub https://acme.com/backyard/docs, site https://lunch.backyard.example.com/ and " +
			"https://acme.com/backyard/s/lunch/; see [api](references/api.md) and `*.backyard.example.com`."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Localize(md, tt.publicURL); got != tt.want {
				t.Errorf("Localize(%q)\n got  %q\n want %q", tt.publicURL, got, tt.want)
			}
		})
	}
	// Every embedded document localizes without leftovers for a host-mode hub.
	docs := append([]string{SkillMarkdown()}, func() []string {
		var out []string
		for _, n := range References() {
			out = append(out, Reference(n))
		}
		return out
	}()...)
	for i, doc := range docs {
		if got := Localize(doc, "https://hub.acme.dev"); strings.Contains(got, "backyard.example.com") {
			t.Errorf("document %d still mentions the placeholder domain after Localize", i)
		}
	}
}

// siteHostRef is the reference (regular expression) definition of an example
// site host; replaceSiteHosts must behave like replacing its matches.
var siteHostRef = regexp.MustCompile(`(https://)?((?:[A-Za-z0-9*_-]|<[a-z]+>)+(?:\.(?:[A-Za-z0-9*_-]|<[a-z]+>)+)*)\.` + regexp.QuoteMeta(placeholderDomain))

func replaceSiteHostsRef(s, scheme, host string) string {
	var b strings.Builder
	last := 0
	for _, m := range siteHostRef.FindAllStringSubmatchIndex(s, -1) {
		if continuesHost(s, m[1]) {
			continue
		}
		b.WriteString(s[last:m[0]])
		if m[2] >= 0 {
			b.WriteString(scheme + "://")
		}
		b.WriteString(s[m[4]:m[5]] + "." + host)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

func TestReplaceSiteHostsMatchesReference(t *testing.T) {
	docs := []string{SkillMarkdown()}
	for _, n := range References() {
		docs = append(docs, Reference(n))
	}
	for i, d := range docs {
		if got, want := replaceSiteHosts(d, "http", "h.test:1"), replaceSiteHostsRef(d, "http", "h.test:1"); got != want {
			t.Errorf("document %d: replaceSiteHosts differs from the reference", i)
		}
	}
	parts := []string{"a", "b", ".", "..", "-", "*", "_", "<site>", "<x", ">", "<>", "<Ab>", "https://", "http://", "/", " ",
		"`", ".backyard.example.com", "backyard.example.com", ".com", "x", ".evil", "é"}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20000 {
		var sb strings.Builder
		for k := r.IntN(12); k >= 0; k-- {
			sb.WriteString(parts[r.IntN(len(parts))])
		}
		s := sb.String()
		if got, want := replaceSiteHosts(s, "http", "h.test:1"), replaceSiteHostsRef(s, "http", "h.test:1"); got != want {
			t.Fatalf("replaceSiteHosts(%q)\n got  %q\n want %q", s, got, want)
		}
	}
}

func FuzzReplaceSiteHosts(f *testing.F) {
	for _, seed := range []string{"https://<site>.backyard.example.com/", "a.backyard.example.com.b.backyard.example.com.evil",
		"`*.backyard.example.com`", "x.backyard.example.com<b>", ".backyard.example.com.backyard.example.com"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if got, want := replaceSiteHosts(s, "http", "h.test:1"), replaceSiteHostsRef(s, "http", "h.test:1"); got != want {
			t.Fatalf("replaceSiteHosts(%q)\n got  %q\n want %q", s, got, want)
		}
	})
}

func TestLLMSFullTxtMemoized(t *testing.T) {
	a := LLMSFullTxt("https://hub.acme.dev/")
	if b := LLMSFullTxt("https://hub.acme.dev"); a != b {
		t.Fatal("equivalent public URLs give different documents")
	}
	if c := LLMSFullTxt("https://other.acme.dev"); c == a || !strings.Contains(c, "https://other.acme.dev/docs") {
		t.Fatal("a cached document was returned for another public URL")
	}
	// Many distinct URLs never grow the cache beyond its bound.
	for i := range 3 * maxFullCache {
		_ = LLMSFullTxt("https://h" + strconv.Itoa(i) + ".acme.dev")
	}
	fullCache.Lock()
	n := len(fullCache.m)
	fullCache.Unlock()
	if n > maxFullCache {
		t.Fatalf("cache holds %d documents, max %d", n, maxFullCache)
	}
}

func BenchmarkLLMSFullTxt(b *testing.B) {
	for b.Loop() {
		_ = buildLLMSFullTxt("https://hub.acme.dev")
	}
}
