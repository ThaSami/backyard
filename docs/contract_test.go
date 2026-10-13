package docs

// Checks that tie statements of the specification and the guides to the
// code they describe, so that a contract change cannot leave them stale.

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thasami/backyard/internal/ai/router"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/mcpserver"
)

// readRepoFile returns a file below repoRoot as a string.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// markdownSection returns the lines of the section of a Markdown text whose
// ATX heading text starts with title (a SPEC section number such as "8.3",
// or a heading's full text), up to the next heading of the same or a higher
// level. Fenced code blocks are not looked into for headings.
func markdownSection(t *testing.T, text, title string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	start, level, fence := -1, 0, false
	for i, line := range lines {
		if fenceRe.MatchString(line) {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if start >= 0 && len(m[1]) <= level {
			return strings.Join(lines[start:i], "\n")
		}
		h := m[2]
		if start < 0 && (h == title || strings.HasPrefix(h, title+" ") || strings.HasPrefix(h, title+". ")) {
			start, level = i, len(m[1])
		}
	}
	if start < 0 {
		t.Fatalf("no section %q", title)
	}
	return strings.Join(lines[start:], "\n")
}

// specSection returns section num of the specification ("8.3").
func specSection(t *testing.T, num string) string {
	t.Helper()
	return markdownSection(t, readRepoFile(t, "docs/spec/SPEC.md"), num)
}

// flatten collapses all whitespace runs (line breaks of wrapped prose,
// list indentation) into single spaces.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

var codeSpanRe = regexp.MustCompile("`([^`]+)`")

// codeSpans returns the inline code spans of a line of Markdown.
func codeSpans(s string) []string {
	var out []string
	for _, m := range codeSpanRe.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	return out
}

// tableRows returns the cells of the rows of the first Markdown table in
// text whose header row starts with header (the header and delimiter rows
// excluded).
func tableRows(t *testing.T, text, header string) [][]string {
	t.Helper()
	var rows [][]string
	in := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case !in && strings.HasPrefix(trimmed, header):
			in = true
		case in && !strings.HasPrefix(trimmed, "|"):
			return rows
		case in && strings.HasPrefix(trimmed, "|---"):
		case in:
			cells := strings.Split(strings.Trim(trimmed, "|"), "|")
			for i := range cells {
				cells[i] = strings.TrimSpace(cells[i])
			}
			rows = append(rows, cells)
		}
	}
	if !in {
		t.Fatalf("no table with the header %q", header)
	}
	return rows
}

// keyLabels returns the HKDF labels of the server.secret-derived keys that
// the Go server (internal/keys) and the Cloudflare edition (KeyLabel in
// deploy/cloudflare/src/crypto.ts) use.
func keyLabels(t *testing.T) []string {
	t.Helper()
	labelRe := regexp.MustCompile(`^backyard/[a-z0-9-]+/v[0-9]+$`)
	set := map[string]bool{}
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, "internal", "keys", "keys.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			for _, v := range spec.(*ast.ValueSpec).Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil && labelRe.MatchString(s) {
						set[s] = true
					}
				}
			}
		}
		return false
	})
	ts := readRepoFile(t, "deploy/cloudflare/src/crypto.ts")
	block := regexp.MustCompile(`(?s)export const KeyLabel = \{(.*?)\}`).FindStringSubmatch(ts)
	if block == nil {
		t.Fatal("KeyLabel not found in deploy/cloudflare/src/crypto.ts")
	}
	for _, m := range regexp.MustCompile(`"(backyard/[^"]+)"`).FindAllStringSubmatch(block[1], -1) {
		set[m[1]] = true
	}
	labels := make([]string, 0, len(set))
	for l := range set {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	return labels
}

// TestKeyLabelsDocumented checks that every key derivation label of either
// edition is listed in SPEC §8.3 and in the security guide.
func TestKeyLabelsDocumented(t *testing.T) {
	labels := keyLabels(t)
	if len(labels) < 7 || !slices.Contains(labels, "backyard/session/v1") {
		t.Fatalf("key labels not found (parser broken?): %q", labels)
	}
	docs := map[string]string{
		"docs/spec/SPEC.md §8.3": specSection(t, "8.3"),
		"docs/security.md":       readRepoFile(t, "docs/security.md"),
	}
	for where, text := range docs {
		for _, l := range labels {
			if !strings.Contains(text, "`"+l+"`") {
				t.Errorf("%s does not list the key label `%s`", where, l)
			}
		}
	}
}

// TestSiteURLExamples checks the site URL examples of SPEC §2 against the
// server's (config.SiteURL) and the CLI's (clientconfig.SiteURL, from the
// server's /info) derivation.
func TestSiteURLExamples(t *testing.T) {
	rows := tableRows(t, specSection(t, "2"), "| `public_url` | `base_domain` | `url_mode` |")
	if len(rows) < 3 {
		t.Fatalf("only %d site URL examples in SPEC §2", len(rows))
	}
	differs := false
	for _, row := range rows {
		var vals []string
		for _, c := range row {
			s := codeSpans(c)
			if len(s) != 1 {
				t.Fatalf("SPEC §2 site URL example %q: every cell must be one code span", row)
			}
			vals = append(vals, s[0])
		}
		if len(vals) != 4 {
			t.Fatalf("SPEC §2 site URL example %q: want 4 cells", row)
		}
		public, base, mode, want := vals[0], vals[1], vals[2], vals[3]
		if host := strings.Split(strings.TrimPrefix(strings.TrimPrefix(public, "https://"), "http://"), ":")[0]; host != base {
			differs = true
		}
		cfg := &config.Config{}
		cfg.Server.PublicURL, cfg.Server.BaseDomain, cfg.Server.URLMode = public, base, mode
		if got := cfg.SiteURL("foo"); got != want {
			t.Errorf("SPEC §2: public_url %s, base_domain %s, url_mode %s: config.SiteURL = %q, the spec says %q",
				public, base, mode, got, want)
		}
		info := &deploy.Info{PublicURL: public, BaseDomain: base, URLMode: mode}
		if got := clientconfig.SiteURL(info, public, "foo"); got != want {
			t.Errorf("SPEC §2: public_url %s, base_domain %s, url_mode %s: clientconfig.SiteURL = %q, the spec says %q",
				public, base, mode, got, want)
		}
	}
	if !differs {
		t.Error("SPEC §2 has no site URL example whose public_url host differs from base_domain")
	}
}

// TestUnlimitedLimitsDocumented checks that every limit for which a negative
// value means unlimited says so in its configuration.md row: the ratelimit
// limits and database.max_docs_per_site.
func TestUnlimitedLimitsDocumented(t *testing.T) {
	keys := []string{"database.max_docs_per_site"}
	rt := reflect.TypeFor[config.RateLimitConfig]()
	for i := range rt.NumField() {
		f := rt.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		switch f.Type.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64:
			keys = append(keys, "ratelimit."+name)
		}
	}
	if len(keys) < 7 {
		t.Fatalf("only %d limits found: %q", len(keys), keys)
	}
	lines := strings.Split(readRepoFile(t, "docs/configuration.md"), "\n")
	for _, k := range keys {
		found := false
		for i, line := range lines {
			if !strings.HasPrefix(line, "| `"+k+"` |") {
				continue
			}
			found = true
			if !strings.Contains(line, "negative = unlimited") {
				t.Errorf("configuration.md:%d: the row of %s does not say that a negative value means unlimited", i+1, k)
			}
		}
		if !found {
			t.Errorf("configuration.md has no row for %s", k)
		}
	}
}

// TestOIDCHostModeErrorsDocumented checks that the configuration errors
// configuration.md quotes for auth.mode oidc in host mode are the errors
// validation reports, and that the combination it calls fine passes.
func TestOIDCHostModeErrorsDocumented(t *testing.T) {
	validate := func(base, public string) error {
		cfg := &config.Config{}
		cfg.Server.BaseDomain, cfg.Server.PublicURL = base, public
		cfg.Server.Secret = strings.Repeat("s", 40)
		cfg.Auth.Mode = "oidc"
		cfg.Auth.OIDC.Issuer, cfg.Auth.OIDC.ClientID = "https://idp.example.com", "backyard"
		cfg.ApplyDefaults()
		return cfg.Validate()
	}
	section := markdownSection(t, readRepoFile(t, "docs/configuration.md"), "`auth.oidc`")
	var quoted []string
	for _, s := range codeSpans(section) {
		if q, ok := strings.CutSuffix(s, " …"); ok && strings.HasPrefix(q, "server.") {
			quoted = append(quoted, q)
		}
	}
	for _, tc := range []struct{ base, public, want string }{
		{"localhost", "http://localhost:8080", `server.base_domain "localhost" cannot carry a cookie Domain`},
		{"apps.acme.com", "https://hub.acme.com", `server.public_url host "hub.acme.com" must be server.base_domain "apps.acme.com" or one of its subdomains`},
	} {
		err := validate(tc.base, tc.public)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("base_domain %s, public_url %s: validation error = %v, want it to contain %q", tc.base, tc.public, err, tc.want)
		}
		if !slices.Contains(quoted, tc.want) {
			t.Errorf("configuration.md (auth.oidc) does not quote the error `%s …` (quoted: %q)", tc.want, quoted)
		}
	}
	if len(quoted) != 2 {
		t.Errorf("configuration.md (auth.oidc) quotes %d errors, want the 2 checked here: %q", len(quoted), quoted)
	}
	if !strings.Contains(flatten(section), "`public_url: https://hub.apps.acme.com`) is fine") {
		t.Error("configuration.md (auth.oidc) no longer gives the example of a hub on a subdomain of base_domain")
	}
	if err := validate("apps.acme.com", "https://hub.apps.acme.com"); err != nil {
		t.Errorf("a hub on a subdomain of base_domain: %v", err)
	}
}

// TestAILimitsDocumented checks the AI request limits stated by SPEC §5.4
// and the guides against the Go server's constants, and that the
// specification describes how chat images that are the site's own uploads
// are read.
func TestAILimitsDocumented(t *testing.T) {
	spec := flatten(specSection(t, "5.4"))
	mib := func(n int) int { return n >> 20 }
	for _, want := range []string{
		fmt.Sprintf("Inline images ≤ %d MiB each", mib(router.MaxImageBytes)),
		fmt.Sprintf("images loaded from URLs (uploads and downloads together) ≤ %d MiB per request, and ≤ %d MiB each",
			mib(router.MaxRemoteImageBytes), mib(router.MaxImageBytes)),
		fmt.Sprintf("`n` is at most %d on the Go server", router.MaxImageCount),
		fmt.Sprintf("At most %d strings (%d MiB in total) on the Go server", router.MaxEmbedInputs, mib(router.MaxEmbedBytes)),
		// The URLs the server reads from the site's own uploads.
		"the site-relative `/__backyard/files/{id}/{name}` that `files.upload` returns (path mode: with `/s/{site}` in front)",
		"the same path as an absolute URL on the site's own origin",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("SPEC §5.4 does not say %q", want)
		}
	}
	for file, wants := range map[string][]string{
		"docs/ai-providers.md": {
			fmt.Sprintf("Images are limited to %d MiB each", mib(router.MaxImageBytes)),
			fmt.Sprintf("(uploads and downloads together) to %d MiB per request", mib(router.MaxRemoteImageBytes)),
		},
		"docs/security.md": {
			fmt.Sprintf("caps each image at %d MiB and each request at %d MiB", mib(router.MaxImageBytes), mib(router.MaxRemoteImageBytes)),
		},
	} {
		text := flatten(readRepoFile(t, file))
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", file, want)
			}
		}
	}
}

// mcpToolSchema is the part of a tool's input schema the docs describe.
type mcpToolSchema struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

// TestMCPToolsDocumented checks the MCP tool table of docs/agents.md against
// the tools `backyard mcp` serves — every tool has a row and every row a
// tool, and the Input column names each parameter, with "?" exactly for
// the optional ones — and that SPEC §14 and docs/cli.md list every tool.
func TestMCPToolsDocumented(t *testing.T) {
	dir := t.TempDir()
	srv := mcpserver.New(mcpserver.Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Getwd:      func() (string, error) { return dir, nil },
	})
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "docs-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) < 8 {
		t.Fatalf("only %d MCP tools", len(res.Tools))
	}

	section := markdownSection(t, readRepoFile(t, "docs/agents.md"), "MCP server")
	documented := map[string]string{} // tool → Input cell
	for _, row := range tableRows(t, section, "| Tool | Input | Does |") {
		names := codeSpans(row[0])
		if len(names) != 1 || len(row) != 3 {
			t.Fatalf("docs/agents.md: malformed MCP tool row %q", row)
		}
		documented[names[0]] = row[1]
	}
	parenRe := regexp.MustCompile(`\([^()]*\)`)
	spec := flatten(specSection(t, "14"))
	cli := flatten(markdownSection(t, readRepoFile(t, "docs/cli.md"), "`backyard mcp`"))
	served := map[string]bool{}
	for _, tool := range res.Tools {
		served[tool.Name] = true
		for where, text := range map[string]string{"SPEC §14": spec, "docs/cli.md (backyard mcp)": cli} {
			if !strings.Contains(text, "`"+tool.Name+"`") {
				t.Errorf("%s does not list the MCP tool %s", where, tool.Name)
			}
		}
		cell, ok := documented[tool.Name]
		if !ok {
			t.Errorf("docs/agents.md: the MCP tool %s has no row", tool.Name)
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema mcpToolSchema
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: input schema: %v", tool.Name, err)
		}
		var want []string
		for p := range schema.Properties {
			if slices.Contains(schema.Required, p) {
				want = append(want, p)
			} else {
				want = append(want, p+"?")
			}
		}
		// The parameters are the code spans outside parentheses (which
		// hold explanations such as a parameter's values).
		got := codeSpans(parenRe.ReplaceAllString(cell, ""))
		sort.Strings(want)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("docs/agents.md: the Input of %s is %q, want the parameters %q (\"?\" = optional)", tool.Name, got, want)
		}
	}
	for name := range documented {
		if !served[name] {
			t.Errorf("docs/agents.md documents the MCP tool %s, which backyard mcp does not serve", name)
		}
	}
}
