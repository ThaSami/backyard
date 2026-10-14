package docs

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/thasami/backyard/internal/config"
)

// repoRoot is the repository root relative to this package.
const repoRoot = ".."

// topLevelGuides are the repository's top-level Markdown files that are
// checked along with everything under docs/.
var topLevelGuides = []string{"README.md", "CONTRIBUTING.md", "CLAUDE.md", "CHANGELOG.md", "SECURITY.md"}

// markdown is a parsed Markdown file.
type markdown struct {
	path string // relative to repoRoot, slash-separated
	// prose is the text outside fenced code blocks with inline code spans
	// blanked out (same length and line structure as the file), so that
	// link syntax inside code is not mistaken for a link.
	prose string
	// code holds the contents of the inline code spans and the lines of
	// fenced code blocks, with their line numbers.
	code []codeText
	// anchors are the fragment identifiers the rendered page has: GitHub
	// heading slugs and explicit <a id/name> anchors.
	anchors map[string]bool
	// headings are the ATX headings' texts, in order.
	headings []string
}

type codeText struct {
	line   int
	text   string
	fenced bool
}

var (
	fenceRe      = regexp.MustCompile("^ {0,3}(```+|~~~+)")
	headingRe    = regexp.MustCompile(`^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$`)
	htmlAnchorRe = regexp.MustCompile(`<a\s+(?:id|name)="([^"]+)"`)
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
	inlineLinkRe = regexp.MustCompile(`\[((?:[^\[\]]|\[[^\[\]]*\])*)\]\(([^()]*)\)`)
	refDefRe     = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:[ \t]*<?([^\s>]+)>?`)
	schemeRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
)

// parseMarkdown reads and parses a file below repoRoot.
func parseMarkdown(t *testing.T, rel string) *markdown {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	md := &markdown{path: rel, anchors: map[string]bool{}}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	prose := make([]string, len(lines))
	slugs := map[string]int{}
	fence := ""
	for i, line := range lines {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			marker := m[1]
			switch {
			case fence == "":
				fence = marker
				continue
			case marker[0] == fence[0] && len(marker) >= len(fence) && strings.TrimSpace(line) == marker:
				fence = ""
				continue
			}
		}
		if fence != "" {
			md.code = append(md.code, codeText{line: i + 1, text: line, fenced: true})
			continue
		}
		stripped, spans := stripCodeSpans(line)
		prose[i] = stripped
		for _, s := range spans {
			md.code = append(md.code, codeText{line: i + 1, text: s})
		}
		for _, m := range htmlAnchorRe.FindAllStringSubmatch(line, -1) {
			md.anchors[m[1]] = true
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			md.headings = append(md.headings, m[2])
			s := slug(m[2])
			if n := slugs[s]; n > 0 {
				md.anchors[fmt.Sprintf("%s-%d", s, n)] = true
			} else {
				md.anchors[s] = true
			}
			slugs[s]++
		}
	}
	md.prose = strings.Join(prose, "\n")
	return md
}

// stripCodeSpans blanks out the inline code spans of a line (keeping its
// length) and returns their contents. An unmatched backtick run is literal.
func stripCodeSpans(line string) (string, []string) {
	b := []byte(line)
	var spans []string
	for i := 0; i < len(b); {
		if b[i] != '`' {
			i++
			continue
		}
		n := 0
		for i+n < len(b) && b[i+n] == '`' {
			n++
		}
		closeAt := -1
		for j := i + n; j < len(b); {
			if b[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(b) && b[j+m] == '`' {
				m++
			}
			if m == n {
				closeAt = j
				break
			}
			j += m
		}
		if closeAt < 0 {
			i += n
			continue
		}
		spans = append(spans, strings.TrimSpace(string(b[i+n:closeAt])))
		for k := i; k < closeAt+n; k++ {
			b[k] = ' '
		}
		i = closeAt + n
	}
	return string(b), spans
}

// slug returns GitHub's anchor for a heading: the rendered text lowercased,
// with everything but letters, digits, "_", "-" and spaces removed and the
// spaces replaced by "-".
func slug(heading string) string {
	s := inlineLinkRe.ReplaceAllString(heading, "$1")
	s = htmlTagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "`", "")
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-', r == '_', unicode.IsLetter(r), unicode.IsDigit(r), unicode.Is(unicode.Mn, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// lineOf returns the 1-based line of offset in s.
func lineOf(s string, offset int) int { return strings.Count(s[:offset], "\n") + 1 }

// checkedFiles returns the checked Markdown files, relative to repoRoot.
func checkedFiles(t *testing.T) []string {
	t.Helper()
	files := slices.Clone(topLevelGuides)
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			files = append(files, path.Join("docs", filepath.ToSlash(p)))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// link is a link found in a Markdown file.
type link struct {
	line   int
	target string
}

// links returns the inline links, images and reference definitions of md.
func (md *markdown) links() []link {
	var out []link
	for _, m := range inlineLinkRe.FindAllStringSubmatchIndex(md.prose, -1) {
		dest := strings.TrimSpace(md.prose[m[4]:m[5]])
		if strings.HasPrefix(dest, "<") {
			dest = strings.TrimPrefix(strings.SplitN(dest, ">", 2)[0], "<")
		} else if i := strings.IndexAny(dest, " \t\n"); i >= 0 {
			dest = dest[:i] // drop a "title"
		}
		out = append(out, link{line: lineOf(md.prose, m[0]), target: dest})
	}
	for _, m := range refDefRe.FindAllStringSubmatchIndex(md.prose, -1) {
		out = append(out, link{line: lineOf(md.prose, m[0]), target: md.prose[m[2]:m[3]]})
	}
	return out
}

// TestRelativeLinks checks that every relative link of the guides points at
// a file or directory of the repository and, for Markdown targets, that its
// #fragment is a heading (or explicit anchor) of the target.
func TestRelativeLinks(t *testing.T) {
	cache := map[string]*markdown{}
	load := func(rel string) *markdown {
		if md, ok := cache[rel]; ok {
			return md
		}
		md := parseMarkdown(t, rel)
		cache[rel] = md
		return md
	}
	checked := 0
	for _, file := range checkedFiles(t) {
		md := load(file)
		for _, l := range md.links() {
			if l.target == "" || schemeRe.MatchString(l.target) || strings.HasPrefix(l.target, "/") {
				continue // external, or a URL path of a running server
			}
			checked++
			target, frag, _ := strings.Cut(l.target, "#")
			if decoded, err := url.PathUnescape(target); err == nil {
				target = decoded
			}
			rel := file
			if target != "" {
				rel = path.Join(path.Dir(file), target)
				if rel == ".." || strings.HasPrefix(rel, "../") {
					t.Errorf("%s:%d: link %q leaves the repository", file, l.line, l.target)
					continue
				}
				if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(rel))); err != nil {
					t.Errorf("%s:%d: broken link %q: %s does not exist", file, l.line, l.target, rel)
					continue
				}
			}
			if frag == "" || !strings.HasSuffix(rel, ".md") {
				continue
			}
			if decoded, err := url.PathUnescape(frag); err == nil {
				frag = decoded
			}
			if !load(rel).anchors[frag] {
				t.Errorf("%s:%d: broken link %q: %s has no heading with the anchor #%s", file, l.line, l.target, rel, frag)
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d relative links found: is the link parser broken?", checked)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"5.2 Database":                             "52-database",
		"`fs` — no bucket at all":                  "fs--no-bucket-at-all",
		"8.1 `oidc` (Okta, Entra ID, Google…)":     "81-oidc-okta-entra-id-google",
		"ALB: AWS ALB with Okta (or any OIDC IdP)": "alb-aws-alb-with-okta-or-any-oidc-idp",
		"`auth.oidc`":                              "authoidc",
		"[Linked](x.md) `max_conns`":               "linked-max_conns",
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripCodeSpans(t *testing.T) {
	got, spans := stripCodeSpans("a `[x](y)` b ``c ` d`` e `unclosed")
	if want := "a          b           e `unclosed"; got != want {
		t.Errorf("stripped = %q, want %q", got, want)
	}
	if want := []string{"[x](y)", "c ` d"}; !slices.Equal(spans, want) {
		t.Errorf("spans = %q, want %q", spans, want)
	}
}

// makefileTargets returns the explicit targets of the repository Makefile.
func makefileTargets(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	targetRe := regexp.MustCompile(`^([A-Za-z0-9_.-]+(?:[ \t]+[A-Za-z0-9_.-]+)*)[ \t]*:([^=]|$)`)
	targets := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if m := targetRe.FindStringSubmatch(line); m != nil {
			for _, name := range strings.Fields(m[1]) {
				if !strings.HasPrefix(name, ".") {
					targets[name] = true
				}
			}
		}
	}
	return targets
}

var (
	// makeCmdRe finds a make invocation at the start of a shell command.
	makeCmdRe   = regexp.MustCompile(`(?:^|&&|;|\|\||\$ )\s*make((?:[ \t]+\S+)*)`)
	makeTargRe  = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)
	makeVarRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	makeStopRe  = regexp.MustCompile(`^(?:#|&&|\|\||;|\||>|<|\(|→)`)
	makeFlagArg = map[string]bool{"-C": true, "-f": true, "-j": true}
)

// makeTargets returns the targets named by the make commands in a code
// span or line: `make fmt lint`, `make e2e E2E_TARGET=worker`, `cd x &&
// make test`.
func makeTargets(code string) []string {
	var out []string
	for _, m := range makeCmdRe.FindAllStringSubmatch(code, -1) {
		args := strings.Fields(m[1])
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case makeStopRe.MatchString(a):
				i = len(args)
			case makeFlagArg[a]:
				i++
			case strings.HasPrefix(a, "-"), makeVarRe.MatchString(a):
			case makeTargRe.MatchString(a):
				out = append(out, a)
			default:
				i = len(args)
			}
		}
	}
	return out
}

func TestMakeTargetsParser(t *testing.T) {
	for in, want := range map[string][]string{
		"make fmt lint":                         {"fmt", "lint"},
		"make test               # exports …":   {"test"},
		"make e2e E2E_TARGET=worker":            {"e2e"},
		"cd e2e && make build VERSION=1.2.3":    {"build"},
		"make -C .. services-up test":           {"services-up", "test"},
		"make":                                  nil,
		"go run . # make it so":                 nil,
		"make build && ./bin/backyard version":  {"build"},
		"make conformance CONFORMANCE_ARGS=x y": {"conformance", "y"},
	} {
		if got := makeTargets(in); !slices.Equal(got, want) {
			t.Errorf("makeTargets(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMakeTargetsExist checks that every `make <target>` in the guides is a
// target of the Makefile.
func TestMakeTargetsExist(t *testing.T) {
	targets := makefileTargets(t)
	if !targets["test"] || !targets["help"] {
		t.Fatalf("Makefile targets not found (parser broken?): %v", targets)
	}
	seen := 0
	for _, file := range checkedFiles(t) {
		md := parseMarkdown(t, file)
		for _, c := range md.code {
			text := c.text
			if !c.fenced && !strings.HasPrefix(text, "make") {
				continue // only code spans that are make commands
			}
			for _, target := range makeTargets(text) {
				seen++
				if !targets[target] {
					t.Errorf("%s:%d: `make %s`: the Makefile has no target %q", file, c.line, target, target)
				}
			}
		}
	}
	if seen < 20 {
		t.Fatalf("only %d make targets found in the guides: is the parser broken?", seen)
	}
}

// specSections returns the section numbers of SPEC.md ("0", "2.1", …).
func specSections(t *testing.T) map[string]bool {
	t.Helper()
	numRe := regexp.MustCompile(`^(\d+(?:\.\d+)*)\.?\s`)
	out := map[string]bool{}
	for _, h := range parseMarkdown(t, "docs/spec/SPEC.md").headings {
		if m := numRe.FindStringSubmatch(h); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// TestSpecSectionReferences checks that every "§n" the guides (and the
// specification itself) reference is a section of SPEC.md. References to
// other documents' sections ("RFC 9110 §15.3.7") are skipped.
func TestSpecSectionReferences(t *testing.T) {
	sections := specSections(t)
	if !sections["5.2"] || !sections["17"] {
		t.Fatalf("SPEC.md sections not found (parser broken?): %v", sections)
	}
	refRe := regexp.MustCompile(`(RFC[ \t]*\d+[ \t]*)?§[ \t]*(\d+(?:\.\d+)*)`)
	for _, file := range checkedFiles(t) {
		raw, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, m := range refRe.FindAllStringSubmatchIndex(text, -1) {
			if m[2] >= 0 {
				continue
			}
			if n := text[m[4]:m[5]]; !sections[n] {
				t.Errorf("%s:%d: §%s is not a section of docs/spec/SPEC.md", file, lineOf(text, m[0]), n)
			}
		}
	}
}

// configKeys returns the dotted paths of every configuration key of the
// schema: maps of sections appear as "<>" ("ai.models.<>.kind"), lists of
// sections as "[]" ("auth.tokens[].name"). Struct-valued keys are listed
// too (they are documented as sections or rows); leaves records which
// keys hold values.
func configKeys() (all, leaves map[string]bool) {
	all, leaves = map[string]bool{}, map[string]bool{}
	durationType := reflect.TypeFor[config.Duration]()
	var walk func(prefix string, t reflect.Type)
	walk = func(prefix string, t reflect.Type) {
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" || !f.IsExported() {
				continue
			}
			key := name
			if prefix != "" {
				key = prefix + "." + name
			}
			all[key] = true
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			switch {
			case ft == durationType:
				leaves[key] = true
			case ft.Kind() == reflect.Struct:
				walk(key, ft)
			case ft.Kind() == reflect.Map && ft.Elem().Kind() == reflect.Struct:
				all[key+".<>"] = true
				walk(key+".<>", ft.Elem())
			case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
				all[key+"[]"] = true
				walk(key+"[]", ft.Elem())
			default:
				leaves[key] = true
			}
		}
	}
	walk("", reflect.TypeFor[config.Config]())
	return all, leaves
}

// documentedConfigKeys returns the keys of the tables of configuration.md,
// normalized like configKeys, with the line of their row. Rows of the
// tables that describe one entry of a map ("`ai.providers.<name>` (…):"
// followed by a table of bare keys) are prefixed with that map's path.
func documentedConfigKeys(t *testing.T) map[string]int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "docs", "configuration.md"))
	if err != nil {
		t.Fatal(err)
	}
	placeholderRe := regexp.MustCompile(`<[^<>]*>`)
	norm := func(k string) string { return placeholderRe.ReplaceAllString(k, "<>") }
	rowKeyRe := regexp.MustCompile("^\\| *((?:`[^`]+`(?: */ *)?)+) *\\|")
	codeRe := regexp.MustCompile("`([^`]+)`")
	out := map[string]int{}
	prefix := ""    // map entry prefix of the current table
	paragraph := "" // the first line of the paragraph before the current table
	prevBlank, inTable := true, false
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			inTable = false
			if trimmed != "" && prevBlank {
				paragraph = trimmed
			}
			prevBlank = trimmed == ""
			continue
		}
		prevBlank = false
		if !inTable {
			inTable = true
			prefix = ""
			// "`ai.models.<alias>`:" or "`ai.providers.<name>` (the type…"
			if m := codeRe.FindStringSubmatch(paragraph); m != nil && strings.HasPrefix(paragraph, "`") &&
				strings.Contains(m[1], ".") && strings.HasSuffix(m[1], ">") {
				prefix = norm(m[1])
			}
			continue // the header row
		}
		m := rowKeyRe.FindStringSubmatch(line)
		if m == nil {
			continue // the delimiter row, or a row without a key
		}
		cells := codeRe.FindAllStringSubmatch(m[1], -1)
		var first string
		for j, c := range cells {
			k := c[1]
			switch {
			case j > 0 && !strings.Contains(k, "."):
				// "`storage.s3.access_key_id` / `secret_access_key`"
				k = first[:strings.LastIndex(first, ".")+1] + k
			case prefix != "" && !strings.Contains(k, "."):
				k = prefix + "." + k
			}
			if j == 0 {
				first = k
			}
			out[norm(k)] = i + 1
		}
	}
	return out
}

// TestConfigurationReference checks docs/configuration.md against the
// schema: every key it documents exists, and every key that holds a value
// has a row (CLAUDE.md, invariant 6).
func TestConfigurationReference(t *testing.T) {
	all, leaves := configKeys()
	documented := documentedConfigKeys(t)
	if len(documented) < 100 {
		t.Fatalf("only %d keys found in configuration.md: is the table parser broken?", len(documented))
	}
	sections := map[string]bool{}
	for k := range all {
		sections[strings.SplitN(k, ".", 2)[0]] = true
	}
	var unknown, missing []string
	for k, line := range documented {
		top := strings.SplitN(strings.SplitN(k, ".", 2)[0], "[", 2)[0]
		if !sections[top] {
			continue // not a configuration key (an environment variable table, …)
		}
		if !all[k] {
			unknown = append(unknown, fmt.Sprintf("configuration.md:%d: %s", line, k))
		}
	}
	for k := range leaves {
		if _, ok := documented[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(unknown)
	sort.Strings(missing)
	for _, u := range unknown {
		t.Errorf("%s is not a key of the configuration schema (internal/config)", u)
	}
	for _, m := range missing {
		t.Errorf("configuration key %s has no row in docs/configuration.md", m)
	}
}
