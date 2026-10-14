package docs

// Contract checks for the statements the specification and the guides make
// about the hardened behaviour of both editions: which uploads are served
// without a sandbox, the warehouse and realtime size limits, the direct-sync
// commands, the ignore rules no configuration can override, the login state
// cookie, the Cloudflare edition's query page budget and its Access bypass
// list.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/warehouse"
)

// parseGoFile parses a Go file below repoRoot.
func parseGoFile(t *testing.T, rel string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(repoRoot, filepath.FromSlash(rel)), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// stringLit returns the value of a string literal expression.
func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// goPassiveUploadTypes returns the keys of the Go server's
// passiveUploadTypes map (internal/server/files.go).
func goPassiveUploadTypes(t *testing.T) []string {
	t.Helper()
	var types []string
	ast.Inspect(parseGoFile(t, "internal/server/files.go"), func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "passiveUploadTypes" || len(vs.Values) != 1 {
			return true
		}
		cl, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatal("passiveUploadTypes is not a composite literal")
		}
		for _, el := range cl.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if s, ok := stringLit(kv.Key); ok {
					types = append(types, s)
				}
			}
		}
		return false
	})
	sort.Strings(types)
	return types
}

// workerPassiveUploadTypes returns PASSIVE_UPLOAD_TYPES of the Cloudflare
// edition (deploy/cloudflare/src/mime.ts).
func workerPassiveUploadTypes(t *testing.T) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)const PASSIVE_UPLOAD_TYPES = new Set\(\[(.*?)\]\)`).FindStringSubmatch(readRepoFile(t, "deploy/cloudflare/src/mime.ts"))
	if m == nil {
		t.Fatal("PASSIVE_UPLOAD_TYPES not found in deploy/cloudflare/src/mime.ts")
	}
	var types []string
	for _, s := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		types = append(types, s[1])
	}
	sort.Strings(types)
	return types
}

// TestUploadSandboxAllowlistDocumented checks that SPEC §3 names exactly the
// upload types both editions serve without "Content-Security-Policy:
// sandbox", and that the two editions agree on them.
func TestUploadSandboxAllowlistDocumented(t *testing.T) {
	goTypes, workerTypes := goPassiveUploadTypes(t), workerPassiveUploadTypes(t)
	if len(goTypes) < 10 || !slices.Contains(goTypes, "image/png") {
		t.Fatalf("passive upload types not found (parser broken?): %q", goTypes)
	}
	if !slices.Equal(goTypes, workerTypes) {
		t.Errorf("the editions serve different upload types without a sandbox:\nGo:     %q\nWorker: %q", goTypes, workerTypes)
	}
	spec := flatten(specSection(t, "3"))
	start, end := strings.Index(spec, "only **passive**"), strings.Index(spec, "Every other type")
	if start < 0 || end < start {
		t.Fatal("SPEC §3 no longer has the sentence listing the passive upload types")
	}
	mediaType := regexp.MustCompile(`^[a-z]+/[a-z0-9.+*-]+$`)
	var documented []string
	for _, s := range codeSpans(spec[start:end]) {
		if mediaType.MatchString(s) {
			documented = append(documented, s)
		}
	}
	want := append(slices.Clone(goTypes), "audio/*", "video/*")
	sort.Strings(want)
	sort.Strings(documented)
	if !slices.Equal(documented, want) {
		t.Errorf("SPEC §3 lists the passive upload types %q, the code %q", documented, want)
	}
	for _, s := range []string{"`text/xsl`", "`+xml`", "`Content-Security-Policy: sandbox`"} {
		if !strings.Contains(spec, s) {
			t.Errorf("SPEC §3 does not mention %s", s)
		}
	}
}

// TestWarehouseResultCapDocumented checks the warehouse result-size cap that
// SPEC §5.5 and the guides state against the code's default.
func TestWarehouseResultCapDocumented(t *testing.T) {
	mib := warehouse.DefaultMaxResultBytes >> 20
	if mib<<20 != warehouse.DefaultMaxResultBytes {
		t.Fatalf("DefaultMaxResultBytes %d is not a whole number of MiB", warehouse.DefaultMaxResultBytes)
	}
	want := strconv.Itoa(mib) + " MiB"
	for where, text := range map[string]string{
		"SPEC §5.5":                    flatten(specSection(t, "5.5")),
		"docs/warehouse.md":            flatten(readRepoFile(t, "docs/warehouse.md")),
		"docs/configuration.md":        flatten(readRepoFile(t, "docs/configuration.md")),
		"skills/…/references/api.md":   flatten(readRepoFile(t, "skills/backyard/references/api.md")),
		"docs/security.md (warehouse)": flatten(markdownSection(t, readRepoFile(t, "docs/security.md"), "Warehouse")),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not state the %s warehouse result cap", where, want)
		}
	}
}

// TestRealtimePayloadLimitDocumented checks that SPEC §6 names the pub/sub
// payload limit above which db events travel without their document.
func TestRealtimePayloadLimitDocumented(t *testing.T) {
	mib := pubsub.MaxPayloadBytes >> 20
	if mib<<20 != pubsub.MaxPayloadBytes {
		t.Fatalf("MaxPayloadBytes %d is not a whole number of MiB", pubsub.MaxPayloadBytes)
	}
	spec := flatten(specSection(t, "6"))
	for _, want := range []string{
		"exceed its " + strconv.Itoa(mib) + " MiB pub/sub payload limit",
		`"doc": null`,
		"`GET /db/{c}/{doc_id}`",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("SPEC §6 does not say %q", want)
		}
	}
}

// directSyncFlags returns, per deploy method, the flags (normalized by
// flagName) that the CLI's command builder for it passes as string
// literals (internal/cli/direct.go).
func directSyncFlags(t *testing.T) map[string][]string {
	t.Helper()
	builders := map[string]string{
		"buildS3Command": "s3", "buildGCSCommand": "gcs", "buildAzureCommand": "azure", "buildRsyncCommand": "rsync",
	}
	flags := map[string][]string{}
	for _, decl := range parseGoFile(t, "internal/cli/direct.go").Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || builders[fn.Name.Name] == "" || fn.Body == nil {
			continue
		}
		method := builders[fn.Name.Name]
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			if s, ok := stringLit(e); ok && strings.HasPrefix(s, "-") {
				if f := flagName(s); !slices.Contains(flags[method], f) {
					flags[method] = append(flags[method], f)
				}
			}
			return true
		})
	}
	if len(flags) != len(builders) {
		t.Fatalf("found the flags of %d of the %d direct-sync command builders (renamed?): %v", len(flags), len(builders), flags)
	}
	return flags
}

// flagName reduces a command-line token to its flag: "--exclude=<regex>" →
// "--exclude", "[--region" → "--region", "--filter='P" → "--filter".
func flagName(tok string) string {
	tok = strings.Trim(tok, "[]()'\"")
	if i := strings.IndexByte(tok, '='); i > 0 {
		tok = tok[:i]
	}
	return tok
}

// commandFlags returns the flag names of the command lines in the code spans
// of text.
func commandFlags(text string) map[string]bool {
	set := map[string]bool{}
	for _, s := range codeSpans(text) {
		for tok := range strings.FieldsSeq(s) {
			if f := flagName(tok); strings.HasPrefix(f, "-") {
				set[f] = true
			}
		}
	}
	return set
}

// specMethodBullet returns the text of SPEC §13's "* `<method>` — …" bullet,
// including its continuation lines.
func specMethodBullet(t *testing.T, method string) string {
	t.Helper()
	lines := strings.Split(specSection(t, "13"), "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "* `"+method+"` — ") {
			continue
		}
		var b strings.Builder
		b.WriteString(line)
		for _, l := range lines[i+1:] {
			if !strings.HasPrefix(l, "  ") {
				break
			}
			b.WriteString("\n" + l)
		}
		return b.String()
	}
	t.Fatalf("SPEC §13 has no bullet for the deploy method %s", method)
	return ""
}

// TestDirectSyncCommandsDocumented checks that the direct-sync command lines
// in SPEC §13, docs/deploying-sites.md and the skill's deploy reference name
// every flag the CLI passes to the storage tools.
func TestDirectSyncCommandsDocumented(t *testing.T) {
	flags := directSyncFlags(t)
	if !slices.Contains(flags["s3"], "--no-follow-symlinks") || !slices.Contains(flags["rsync"], "--delete-excluded") {
		t.Fatalf("direct-sync flags not found (parser broken?): %v", flags)
	}
	guides := map[string]string{
		"docs/deploying-sites.md":       markdownSection(t, readRepoFile(t, "docs/deploying-sites.md"), "Deploy methods"),
		"skills/…/references/deploy.md": markdownSection(t, readRepoFile(t, "skills/backyard/references/deploy.md"), "Deploy methods"),
	}
	for method, want := range flags {
		docs := map[string]map[string]bool{"SPEC §13": commandFlags(specMethodBullet(t, method))}
		for where, section := range guides {
			found := false
			for _, row := range tableRows(t, section, "| Method |") {
				if len(row) > 1 && row[0] == "`"+method+"`" {
					docs[where], found = commandFlags(row[1]), true
				}
			}
			if !found {
				t.Errorf("%s: the deploy methods table has no row for %s", where, method)
			}
		}
		for where, got := range docs {
			for _, f := range want {
				if !got[f] {
					t.Errorf("%s: the %s command line lacks %s, which the CLI passes", where, method, f)
				}
			}
		}
	}
}

// TestNeverUploadedDocumented checks the two tiers of deploy ignore rules:
// the files the guides call never uploaded are excluded even when the
// configuration re-includes them, while a default such as dotfiles can be
// overridden with a "!" line, as the guides say.
func TestNeverUploadedDocumented(t *testing.T) {
	dir := t.TempDir()
	files := map[string]bool{ // path → deployed
		"index.html": true, ".htaccess": true, "CLAUDE.md": true,
		".env": false, ".git/config": false, "sub/.git/HEAD": false, ".GIT/config": false,
		".backyard.json": false, ".backyardignore": false, "sub/.backyard.json": false,
	}
	reinclude := "!.git/\n!.GIT/\n!.backyard.json\n!.backyardignore\n!.htaccess\n!CLAUDE.md\n"
	for p := range files {
		content := "x"
		if p == ".backyardignore" {
			content = reinclude
		}
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	collected, err := deploy.CollectFiles(dir, []string{"!.backyard.json", "!.git/"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, f := range collected {
		got[f.Path] = true
	}
	for p, deployed := range files {
		if got[p] != deployed {
			t.Errorf("deploying a folder whose ignore configuration re-includes everything: %s deployed = %v, want %v", p, got[p], deployed)
		}
	}

	never := []string{".backyard.json", ".backyardignore", ".git/"}
	for where, text := range map[string]string{
		"docs/deploying-sites.md": readRepoFile(t, "docs/deploying-sites.md"),
		"SPEC §13":                specSection(t, "13"),
	} {
		flat := flatten(text)
		i := strings.Index(flat, "**Never uploaded**")
		if i < 0 {
			t.Errorf("%s no longer says what is never uploaded", where)
			continue
		}
		sentence, _, _ := strings.Cut(flat[i:], ". ")
		spans := codeSpans(sentence)
		sort.Strings(spans)
		if !slices.Equal(spans, never) {
			t.Errorf("%s: never uploaded = %q, want %q", where, spans, never)
		}
		if !strings.Contains(flat, "`!.htaccess`") {
			t.Errorf("%s no longer gives the example of re-including a default (`!.htaccess`)", where)
		}
	}
}

// TestLoginStateCookieDocumented checks the login state cookie names and the
// http cookie path that SPEC §8.1 and the security guide state.
func TestLoginStateCookieDocumented(t *testing.T) {
	if auth.OAuthHostCookie != "__Host-"+auth.OAuthCookie {
		t.Fatalf("OAuthHostCookie = %q", auth.OAuthHostCookie)
	}
	for where, text := range map[string]string{
		"SPEC §8.1":        flatten(specSection(t, "8.1")),
		"docs/security.md": flatten(markdownSection(t, readRepoFile(t, "docs/security.md"), "Cookies and sessions")),
	} {
		for _, want := range []string{"`" + auth.OAuthHostCookie + "`", "`" + auth.OAuthCookie + "`", "Path=" + auth.PathPrefix} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not mention %s", where, want)
			}
		}
	}
}

// TestCloudflareQueryPageBudgetDocumented checks the query page budget of the
// Cloudflare edition (QUERY_PAGE_BUDGET in deploy/cloudflare/src/site-do.ts)
// against SPEC §16 and the guides that tell clients how to page.
func TestCloudflareQueryPageBudgetDocumented(t *testing.T) {
	m := regexp.MustCompile(`export const QUERY_PAGE_BUDGET = (\d+) << 20;`).FindStringSubmatch(readRepoFile(t, "deploy/cloudflare/src/site-do.ts"))
	if m == nil {
		t.Fatal("QUERY_PAGE_BUDGET (n << 20) not found in deploy/cloudflare/src/site-do.ts")
	}
	want := "about " + m[1] + " MiB"
	for where, text := range map[string]string{
		"SPEC §16":                   flatten(specSection(t, "16")),
		"docs/cloudflare.md":         flatten(readRepoFile(t, "docs/cloudflare.md")),
		"skills/…/references/api.md": flatten(readRepoFile(t, "skills/backyard/references/api.md")),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not state the query page budget (%s)", where, want)
		}
	}
	spec := flatten(specSection(t, "5.2"))
	if !strings.Contains(spec, "advancing `offset` by the number of documents received") {
		t.Error("SPEC §5.2 no longer tells clients to advance offset by the number of documents received")
	}
}

// TestAccessBypassPathsDocumented checks that SPEC §16 and the Cloudflare
// guide list every path the Cloudflare Terraform module lets bypass Access.
func TestAccessBypassPathsDocumented(t *testing.T) {
	tf := readRepoFile(t, "deploy/terraform/cloudflare/variables.tf")
	m := regexp.MustCompile(`(?s)variable "bypass_paths" \{.*?default = \[(.*?)\]`).FindStringSubmatch(tf)
	if m == nil {
		t.Fatal("the default of bypass_paths not found in deploy/terraform/cloudflare/variables.tf")
	}
	var paths []string
	for _, s := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		paths = append(paths, s[1])
	}
	if !slices.Contains(paths, "/__backyard/api/tokens") {
		t.Fatalf("bypass_paths = %q (parser broken?)", paths)
	}
	for where, text := range map[string]string{
		"SPEC §16":           specSection(t, "16"),
		"docs/cloudflare.md": readRepoFile(t, "docs/cloudflare.md"),
	} {
		for _, p := range paths {
			if !strings.Contains(text, "`"+p+"`") {
				t.Errorf("%s does not list the Access bypass path %s", where, p)
			}
		}
	}
}
