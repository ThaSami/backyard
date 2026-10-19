package docs

// Contract checks for the statements the specification and the guides make
// about the hardened behaviour of both editions: which uploads are served
// without a sandbox, how site previews are chosen and served, the warehouse
// and realtime size limits, the direct-sync commands, the ignore rules no
// configuration can override, the login state cookie, the Cloudflare
// edition's query page budget, its Access bypass list, the commands and flags
// of its engine (backyard-cloudflare), what the entry points promise about
// `backyard cloudflare setup`, what a CI deploy must carry over from it, and
// how the owners-only site listing is turned on.

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/sites"
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

// workerPreview holds the site preview values of the Cloudflare edition
// (deploy/cloudflare/src/preview.ts).
type workerPreview struct {
	names            []string
	maxBytes         int
	csp              string
	types            map[string]string
	cacheVersioned   string // Cache-Control with a non-empty ?v=
	cacheUnversioned string // Cache-Control without one
}

// readWorkerPreview reads PREVIEW_NAMES, MAX_PREVIEW_BYTES, PREVIEW_CSP,
// PREVIEW_TYPES, PREVIEW_CACHE_VERSIONED and PREVIEW_CACHE_UNVERSIONED from
// deploy/cloudflare/src/preview.ts.
func readWorkerPreview(t *testing.T) workerPreview {
	t.Helper()
	const rel = "deploy/cloudflare/src/preview.ts"
	src := readRepoFile(t, rel)
	find := func(name, re string) []string {
		t.Helper()
		m := regexp.MustCompile(re).FindStringSubmatch(src)
		if m == nil {
			t.Fatalf("%s not found in %s (update this test)", name, rel)
		}
		return m
	}
	var w workerPreview
	w.names = quotedStrings(find("PREVIEW_NAMES", `(?s)export const PREVIEW_NAMES\b[^=]*= \[(.*?)\]`)[1])
	m := find("MAX_PREVIEW_BYTES", `export const MAX_PREVIEW_BYTES\b[^=]*= (\d+)(?: << (\d+))?;`)
	w.maxBytes, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		shift, _ := strconv.Atoi(m[2])
		w.maxBytes <<= shift
	}
	w.csp = find("PREVIEW_CSP", `export const PREVIEW_CSP\b[^=]*= "([^"]*)";`)[1]
	w.types = map[string]string{}
	for _, kv := range regexp.MustCompile(`"?([a-z0-9]+)"?: "([^"]+)"`).FindAllStringSubmatch(find("PREVIEW_TYPES", `(?s)export const PREVIEW_TYPES\b[^=]*= \{(.*?)\}`)[1], -1) {
		w.types[kv[1]] = kv[2]
	}
	w.cacheVersioned = find("PREVIEW_CACHE_VERSIONED", `export const PREVIEW_CACHE_VERSIONED\b[^=]*= "([^"]*)";`)[1]
	w.cacheUnversioned = find("PREVIEW_CACHE_UNVERSIONED", `export const PREVIEW_CACHE_UNVERSIONED\b[^=]*= "([^"]*)";`)[1]
	return w
}

// goStringConsts returns the values of the named string constants of a Go
// file below repoRoot and how often the file uses each name besides
// declaring it.
func goStringConsts(t *testing.T, rel string, names ...string) (values map[string]string, uses map[string]int) {
	t.Helper()
	f := parseGoFile(t, rel)
	values, uses = map[string]string{}, map[string]int{}
	declared := map[*ast.Ident]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, id := range vs.Names {
				if !slices.Contains(names, id.Name) || i >= len(vs.Values) {
					continue
				}
				if s, ok := stringLit(vs.Values[i]); ok {
					values[id.Name] = s
					declared[id] = true
				}
			}
		}
		return false
	})
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && !declared[id] && slices.Contains(names, id.Name) {
			uses[id.Name]++
		}
		return true
	})
	for _, name := range names {
		if _, ok := values[name]; !ok {
			t.Fatalf("string constant %s not found in %s (update this test)", name, rel)
		}
	}
	return values, uses
}

// tsImportRe matches a TypeScript import statement.
var tsImportRe = regexp.MustCompile(`(?s)\bimport\s+(?:type\s+)?\{[^}]*\}\s*from\s*"[^"]*";`)

// listItem returns the Markdown list item of text that contains needle: the
// line that starts the item and its indented continuation lines.
func listItem(t *testing.T, where, text, needle string) string {
	t.Helper()
	lines := strings.Split(text, "\n")
	at := slices.IndexFunc(lines, func(l string) bool { return strings.Contains(l, needle) })
	if at < 0 {
		t.Fatalf("%s no longer says %q (update this test)", where, needle)
	}
	itemStart := regexp.MustCompile(`^\s*(?:\d+\.|[-*])\s`)
	start := at
	for start > 0 && !itemStart.MatchString(lines[start]) {
		start--
	}
	if !itemStart.MatchString(lines[start]) {
		t.Fatalf("%s: %q is no longer in a list item (update this test)", where, needle)
	}
	end := start + 1
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" && !itemStart.MatchString(lines[end]) &&
		strings.TrimLeft(lines[end], " \t") != lines[end] {
		end++
	}
	if end <= at {
		t.Fatalf("%s: %q is no longer in a list item (update this test)", where, needle)
	}
	return strings.Join(lines[start:end], "\n")
}

// mibRe matches a size in MiB ("1 MiB").
var mibRe = regexp.MustCompile(`\b\d+(?:\.\d+)? MiB\b`)

// previewNameList returns the names of the longest list of preview file
// names ("`preview.png`, `preview.jpg`, … or `preview.svg`") in text.
func previewNameList(text string) []string {
	longest := ""
	for _, m := range regexp.MustCompile("`preview\\.[a-z]+`(?:,? (?:or |and )?`preview\\.[a-z]+`)+").FindAllString(text, -1) {
		if len(m) > len(longest) {
			longest = m
		}
	}
	return codeSpans(longest)
}

// between returns the part of text from the first start to the next end
// after it.
func between(t *testing.T, where, text, start, end string) string {
	t.Helper()
	i := strings.Index(text, start)
	if i < 0 {
		t.Fatalf("%s no longer says %q (update this test)", where, start)
	}
	j := strings.Index(text[i:], end)
	if j < 0 {
		t.Fatalf("%s: %q is no longer followed by %q (update this test)", where, start, end)
	}
	return text[i : i+j]
}

// TestSitePreviewContractDocumented checks how site previews are chosen
// (SPEC §7.2) and served (SPEC §3) against both editions: the preview.* file
// names in their order, the og:image extensions, the image types and the
// Content-Type each is served with, the 1 MiB cap (of both rules, and when
// serving), the Content-Security-Policy and which of the two Cache-Control
// values goes with a non-empty ?v=. The Go server (internal/sites/preview.go,
// internal/server/preview.go) and the Cloudflare edition
// (deploy/cloudflare/src/preview.ts) must agree with the specification, and
// their preview handlers must use the Cache-Control constants this test
// reads. The guides that tell people and agents how to ship a preview must
// state the same names, extensions, cap and policy, and the guide and the
// API reference the same Cache-Control values.
func TestSitePreviewContractDocumented(t *testing.T) {
	if len(sites.PreviewNames) < 2 || len(sites.PreviewTypes) < 2 || sites.PreviewCSP == "" {
		t.Fatalf("Go preview contract not found: names %q, types %v, CSP %q", sites.PreviewNames, sites.PreviewTypes, sites.PreviewCSP)
	}
	mib := sites.MaxPreviewBytes >> 20
	if mib<<20 != sites.MaxPreviewBytes {
		t.Fatalf("MaxPreviewBytes %d is not a whole number of MiB", sites.MaxPreviewBytes)
	}
	limit := strconv.Itoa(mib) + " MiB"
	exts := slices.Sorted(maps.Keys(sites.PreviewTypes))
	// checkCap reports a span that states no size in MiB, or another one
	// than the cap.
	checkCap := func(where, span string) {
		t.Helper()
		figures := mibRe.FindAllString(span, -1)
		if len(figures) == 0 {
			t.Errorf("%s does not state the preview cap %s", where, limit)
		}
		for _, f := range figures {
			if f != limit {
				t.Errorf("%s states the preview cap %s, the code %s", where, f, limit)
			}
		}
	}

	// The editions agree.
	w := readWorkerPreview(t)
	if !slices.Equal(w.names, sites.PreviewNames) {
		t.Errorf("preview file names: Go %q, Worker %q", sites.PreviewNames, w.names)
	}
	if w.maxBytes != sites.MaxPreviewBytes {
		t.Errorf("largest preview: Go %d bytes, Worker %d", sites.MaxPreviewBytes, w.maxBytes)
	}
	if w.csp != sites.PreviewCSP {
		t.Errorf("preview Content-Security-Policy: Go %q, Worker %q", sites.PreviewCSP, w.csp)
	}
	if !maps.Equal(w.types, sites.PreviewTypes) {
		t.Errorf("preview types: Go %v, Worker %v", sites.PreviewTypes, w.types)
	}
	for _, name := range sites.PreviewNames {
		if ext := strings.TrimPrefix(path.Ext(name), "."); sites.PreviewTypes[ext] == "" {
			t.Errorf("the preview file name %s has no preview type", name)
		}
	}
	const goHandler = "internal/server/preview.go"
	goCache, goUses := goStringConsts(t, goHandler, "previewCacheVersioned", "previewCacheUnversioned")
	versioned, unversioned := goCache["previewCacheVersioned"], goCache["previewCacheUnversioned"]
	if w.cacheVersioned != versioned {
		t.Errorf("preview Cache-Control with ?v=: Go %q, Worker %q", versioned, w.cacheVersioned)
	}
	if w.cacheUnversioned != unversioned {
		t.Errorf("preview Cache-Control without ?v=: Go %q, Worker %q", unversioned, w.cacheUnversioned)
	}
	workerServe := tsImportRe.ReplaceAllString(readRepoFile(t, "deploy/cloudflare/src/site-do.ts")+readRepoFile(t, "deploy/cloudflare/src/hub.ts"), "")
	for goName, tsName := range map[string]string{"previewCacheVersioned": "PREVIEW_CACHE_VERSIONED", "previewCacheUnversioned": "PREVIEW_CACHE_UNVERSIONED"} {
		if goUses[goName] == 0 {
			t.Errorf("%s defines %s but does not use it", goHandler, goName)
		}
		if !regexp.MustCompile(`\b` + tsName + `\b`).MatchString(workerServe) {
			t.Errorf("deploy/cloudflare/src (site-do.ts, hub.ts) does not use %s", tsName)
		}
	}

	// SPEC §7.2: how a preview is chosen.
	choose := flatten(specSection(t, "7.2"))
	if got := previewNameList(choose); !slices.Equal(got, sites.PreviewNames) {
		t.Errorf("SPEC §7.2 lists the preview files %q, the code %q (in this order)", got, sites.PreviewNames)
	}
	documented := codeSpans(between(t, "SPEC §7.2", choose, "its extension (lowercased) is one of", "MiB"))
	sort.Strings(documented)
	if !slices.Equal(documented, exts) {
		t.Errorf("SPEC §7.2 names the og:image extensions %q, the code %q", documented, exts)
	}
	if want := fmt.Sprintf("%s (%d bytes)", limit, sites.MaxPreviewBytes); !strings.Contains(choose, want) {
		t.Errorf("SPEC §7.2 does not state the preview cap %q", want)
	}
	checkCap("SPEC §7.2 (rule 2, preview.*)", between(t, "SPEC §7.2", choose, "**`preview.*`.**", "Otherwise the site has no preview"))

	// SPEC §3: how it is served.
	serve := flatten(specSection(t, "3"))
	if want := "`Content-Security-Policy: " + sites.PreviewCSP + "`"; !strings.Contains(serve, want) {
		t.Errorf("SPEC §3 does not give the preview policy %s", want)
	}
	served := map[string]string{}
	for _, m := range regexp.MustCompile("`([a-z0-9]+)`(?: and `([a-z0-9]+)`)? → `([a-z]+/[a-z0-9.+-]+)`").FindAllStringSubmatch(
		between(t, "SPEC §3", serve, "`Content-Type` by the extension", "Content-Security-Policy"), -1) {
		served[m[1]] = m[3]
		if m[2] != "" {
			served[m[2]] = m[3]
		}
	}
	if !maps.Equal(served, sites.PreviewTypes) {
		t.Errorf("SPEC §3 serves previews as %v, the code as %v", served, sites.PreviewTypes)
	}
	previews := between(t, "SPEC §3", serve, "**Preview images.**", "The hub UI requests")
	checkCap("SPEC §3 (the 404 answers of the preview route)", between(t, "SPEC §3", previews, "`404 not_found`", "`405`"))
	var cacheControl []string
	for _, s := range codeSpans(previews) {
		if v, ok := strings.CutPrefix(s, "Cache-Control: "); ok {
			cacheControl = append(cacheControl, v)
		}
	}
	if want := []string{versioned, unversioned}; !slices.Equal(cacheControl, want) {
		t.Errorf("SPEC §3 gives the preview Cache-Control values %q, the code %q (with a non-empty ?v=, without)", cacheControl, want)
	}
	for _, want := range []string{
		"`Cache-Control: " + versioned + "` when the request has a non-empty `v`",
		"otherwise `Cache-Control: " + unversioned + "`",
	} {
		if !strings.Contains(previews, want) {
			t.Errorf("SPEC §3 does not say %q (%s: previewCacheVersioned with a non-empty ?v=, previewCacheUnversioned without)", want, goHandler)
		}
	}
	if !strings.Contains(flatten(specSection(t, "2")), "`/__backyard/api/sites/{site}/preview`") {
		t.Error("SPEC §2 does not list the hub route /__backyard/api/sites/{site}/preview")
	}

	// The guides say the same.
	guide := flatten(markdownSection(t, readRepoFile(t, "docs/deploying-sites.md"), "Site preview"))
	apiRaw := readRepoFile(t, "skills/backyard/references/api.md")
	api := flatten(apiRaw)
	// An extension as a word of its own, not the end of a file name such
	// as img/cover.png.
	extWord := func(ext string) *regexp.Regexp {
		return regexp.MustCompile(`(?:^|[^.\w])` + regexp.QuoteMeta(ext) + `\b`)
	}
	for where, text := range map[string]string{"docs/deploying-sites.md (Site preview)": guide, "skills/…/references/api.md": api} {
		if got := previewNameList(text); !slices.Equal(got, sites.PreviewNames) {
			t.Errorf("%s lists the preview files %q, the code %q (in this order)", where, got, sites.PreviewNames)
		}
		for _, want := range []string{limit, sites.PreviewCSP, "/__backyard/api/sites/", versioned, unversioned} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not mention %q", where, want)
			}
		}
		// The og:image rule: its extensions and its cap.
		rule := between(t, where, text, "og:image", "MiB") + "MiB"
		for _, ext := range exts {
			if !extWord(ext).MatchString(rule) {
				t.Errorf("%s does not name the og:image extension %s (between \"og:image\" and its cap)", where, ext)
			}
		}
		checkCap(where+" (the og:image rule)", rule)
	}
	checkCap("docs/deploying-sites.md (Site preview)", guide)
	routeRow := between(t, "skills/…/references/api.md", apiRaw, "| `GET /__backyard/api/sites/{site}/preview` |", "\n")
	if !strings.Contains(routeRow, "non-empty") || !strings.Contains(routeRow, versioned) {
		t.Errorf("skills/…/references/api.md (the preview route row) does not say that a non-empty ?v= gets Cache-Control %q", versioned)
	}
	security := flatten(markdownSection(t, readRepoFile(t, "docs/security.md"), "Uploads and deployed files"))
	for _, s := range []string{sites.PreviewCSP, "/__backyard/api/sites/<site>/preview"} {
		if !strings.Contains(security, s) {
			t.Errorf("docs/security.md (Uploads and deployed files) does not mention %q", s)
		}
	}
	// The agent instructions: the list item that tells agents to ship one.
	for _, rel := range []string{"skills/backyard/SKILL.md", "templates/files/agents/partials.tmpl"} {
		item := flatten(listItem(t, rel, readRepoFile(t, rel), "1280×720"))
		if !strings.Contains(item, "`preview.svg`") {
			t.Errorf("%s does not mention `preview.svg` next to 1280×720", rel)
		}
		checkCap(rel+" (the preview item)", item)
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

// quotedStrings returns the double-quoted strings of a list literal.
func quotedStrings(list string) []string {
	var out []string
	for _, s := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(list, -1) {
		out = append(out, s[1])
	}
	return out
}

// TestAccessBypassPathsDocumented checks that the Cloudflare Terraform module
// and `backyard cloudflare access enable` (ACCESS_BYPASS_PATHS in
// deploy/cloudflare/scripts/lib/access-plan.mjs) let the same paths bypass
// Access, in the same order, and that SPEC §16 and the Cloudflare guides list
// every one of them.
func TestAccessBypassPathsDocumented(t *testing.T) {
	tf := readRepoFile(t, "deploy/terraform/cloudflare/variables.tf")
	m := regexp.MustCompile(`(?s)variable "bypass_paths" \{.*?default = \[(.*?)\]`).FindStringSubmatch(tf)
	if m == nil {
		t.Fatal("the default of bypass_paths not found in deploy/terraform/cloudflare/variables.tf")
	}
	paths := quotedStrings(m[1])
	if !slices.Contains(paths, "/__backyard/api/tokens") {
		t.Fatalf("bypass_paths = %q (parser broken?)", paths)
	}
	m = regexp.MustCompile(`(?s)export const ACCESS_BYPASS_PATHS = \[(.*?)\];`).FindStringSubmatch(readRepoFile(t, "deploy/cloudflare/scripts/lib/access-plan.mjs"))
	if m == nil {
		t.Fatal("ACCESS_BYPASS_PATHS not found in deploy/cloudflare/scripts/lib/access-plan.mjs")
	}
	if engine := quotedStrings(m[1]); !slices.Equal(engine, paths) {
		t.Errorf("access enable bypasses %q (ACCESS_BYPASS_PATHS), the Terraform module %q (bypass_paths)", engine, paths)
	}
	for where, text := range map[string]string{
		"SPEC §16":           specSection(t, "16"),
		"docs/cloudflare.md": readRepoFile(t, "docs/cloudflare.md"),
		"deploy/cloudflare/README.md (Company login)": markdownSection(t, readRepoFile(t, "deploy/cloudflare/README.md"), "Company login (Cloudflare Access)"),
	} {
		for _, p := range paths {
			if !strings.Contains(text, "`"+p+"`") {
				t.Errorf("%s does not list the Access bypass path %s", where, p)
			}
		}
	}
}

// engineCommands is deploy/cloudflare/scripts/lib/commands.json, the single
// source of the commands and flags of backyard-cloudflare.
type engineCommands struct {
	Schema   int          `json:"schema"`
	Global   []engineFlag `json:"global"`
	Commands []struct {
		Name  string       `json:"name"`
		Flags []engineFlag `json:"flags"`
	} `json:"commands"`
}

type engineFlag struct {
	Name  string   `json:"name"`
	Alias []string `json:"alias"`
	Short string   `json:"short"`
}

// TestCloudflareEngineFlagsDocumented checks the "Command reference" of
// deploy/cloudflare/README.md against commands.json: every command and every
// flag (with its short form) is documented there, and every --flag it
// mentions exists (a flag, an alias, or --version, which the engine handles
// before parsing).
func TestCloudflareEngineFlagsDocumented(t *testing.T) {
	var cmds engineCommands
	if err := json.Unmarshal([]byte(readRepoFile(t, "deploy/cloudflare/scripts/lib/commands.json")), &cmds); err != nil {
		t.Fatalf("deploy/cloudflare/scripts/lib/commands.json: %v", err)
	}
	if cmds.Schema != 1 || len(cmds.Global) == 0 || len(cmds.Commands) < 6 {
		t.Fatalf("unexpected commands.json (schema %d, %d global flags, %d commands): parser broken?", cmds.Schema, len(cmds.Global), len(cmds.Commands))
	}
	if !strings.Contains(readRepoFile(t, "deploy/cloudflare/scripts/lib/args.mjs"), `a0 === "--version"`) {
		t.Fatal("deploy/cloudflare/scripts/lib/args.mjs no longer handles --version (update this test)")
	}
	section := markdownSection(t, readRepoFile(t, "deploy/cloudflare/README.md"), "Command reference")

	known := map[string]bool{"version": true}
	var flags []engineFlag
	flags = append(flags, cmds.Global...)
	for _, c := range cmds.Commands {
		if !strings.Contains(section, "`"+c.Name) {
			t.Errorf("deploy/cloudflare/README.md (Command reference) does not document the command %q", c.Name)
		}
		flags = append(flags, c.Flags...)
	}
	for _, f := range flags {
		known[f.Name] = true
		for _, a := range f.Alias {
			known[a] = true
		}
		if !regexp.MustCompile("`--" + regexp.QuoteMeta(f.Name) + "[` ]").MatchString(section) {
			t.Errorf("deploy/cloudflare/README.md (Command reference) does not document --%s", f.Name)
		}
		if f.Short != "" && !strings.Contains(section, "`-"+f.Short+"`") {
			t.Errorf("deploy/cloudflare/README.md (Command reference) does not document -%s (--%s)", f.Short, f.Name)
		}
	}
	mentioned := regexp.MustCompile(`(?:^|[^A-Za-z0-9-])--([a-z][a-z0-9-]*)`).FindAllStringSubmatch(section, -1)
	if len(mentioned) < len(flags) {
		t.Fatalf("only %d flags found in the Command reference: parser broken?", len(mentioned))
	}
	for _, m := range mentioned {
		if !known[m[1]] {
			t.Errorf("deploy/cloudflare/README.md (Command reference) mentions --%s, which is not in commands.json", m[1])
		}
	}
}

// cloudflareEntryPoints are the docs that offer `backyard cloudflare setup`
// to people who have no Backyard server yet: the pages read first, and what
// agents read (the skill behind /llms.txt, the agent files of `backyard init`,
// the plugin commands).
var cloudflareEntryPoints = []string{
	"README.md",
	"docs/getting-started.md",
	"examples/README.md",
	"skills/backyard/SKILL.md",
	"templates/files/agents/partials.tmpl",
	"commands/new.md",
	"commands/deploy.md",
}

// cloudflareNpxGuides are the guides that show `npx backyard-cloudflare`.
var cloudflareNpxGuides = []string{
	"docs/cloudflare.md",
	"deploy/cloudflare/README.md",
	"deploy/terraform/cloudflare/README.md",
	"skills/backyard/references/deploy.md",
}

// TestCloudflareSetupEntryPoints checks what the docs promise about getting
// the engine. Only release builds pin an engine package
// (cloudflarePackageSHA256 is set by the release's ldflags), so a CLI from
// `go install` or a source build runs `backyard cloudflare setup` only in a
// checkout or with --package: every entry point that offers the command says
// so right there. And the npm name backyard-cloudflare is Backyard's only once
// a release has published it (CONTRIBUTING.md, npm): until then `npx` would
// run whatever someone else published under it, so the entry points do not
// send anyone there, and the guides that show it say when it works.
func TestCloudflareSetupEntryPoints(t *testing.T) {
	if !regexp.MustCompile(`(?m)^var cloudflarePackageSHA256 string$`).MatchString(readRepoFile(t, "internal/cli/cloudflare_engine.go")) {
		t.Fatal("internal/cli/cloudflare_engine.go: cloudflarePackageSHA256 is no longer a variable only release builds set (update this test and the docs it checks)")
	}
	for _, rel := range cloudflareEntryPoints {
		text := flatten(readRepoFile(t, rel))
		if strings.Contains(text, "npx backyard-cloudflare") {
			t.Errorf("%s sends people to `npx backyard-cloudflare`; only the guides that say it needs a Backyard release on npm may show it", rel)
		}
		i := strings.Index(text, "`backyard cloudflare setup`")
		if i < 0 {
			t.Errorf("%s no longer offers `backyard cloudflare setup` (update cloudflareEntryPoints)", rel)
			continue
		}
		near := text[i:min(len(text), i+600)]
		if !strings.Contains(near, "`go install`") || !strings.Contains(near, "`--package`") {
			t.Errorf("%s offers `backyard cloudflare setup` without saying that a CLI from `go install` needs a checkout or `--package`:\n%s", rel, near)
		}
	}
	for _, rel := range cloudflareNpxGuides {
		text := flatten(readRepoFile(t, rel))
		if !strings.Contains(text, "npx backyard-cloudflare") {
			t.Errorf("%s no longer shows `npx backyard-cloudflare` (update cloudflareNpxGuides)", rel)
		} else if !strings.Contains(text, "release has published it to npm") {
			t.Errorf("%s shows `npx backyard-cloudflare` without saying that it works only once a Backyard release has published it to npm", rel)
		}
	}
}

// TestCloudflareCIDeployCarriesSetup checks the "Deploying from GitHub
// Actions" section of deploy/cloudflare/README.md against
// scripts/cloudflare-ci.mjs. A CI deploy names the Worker, its variables, its
// R2 bucket, its AI switch and its site listing from its environment only,
// and wrangler removes a binding (and every deploy resets a variable) the
// configuration leaves out, so the section's workflow passes all five and
// says what to carry over from a Worker that `setup` made: the bucket (or the
// files stored in R2 fail), AI off, the owners-only site listing (or every
// site is listed again), and that workers.dev stays on.
func TestCloudflareCIDeployCarriesSetup(t *testing.T) {
	ci := readRepoFile(t, "scripts/cloudflare-ci.mjs")
	section := markdownSection(t, readRepoFile(t, "deploy/cloudflare/README.md"), "Deploying from GitHub Actions")
	for _, name := range []string{"BACKYARD_CF_WORKER_NAME", "BACKYARD_CF_VARS", "BACKYARD_CF_R2_BUCKET", "BACKYARD_CF_AI", "BACKYARD_CF_SITE_LISTING"} {
		if !strings.Contains(ci, "env."+name) {
			t.Errorf("scripts/cloudflare-ci.mjs no longer reads %s (update this test and the README)", name)
		}
		if !strings.Contains(section, name+": ${{ vars."+name+" }}") {
			t.Errorf("the workflow in deploy/cloudflare/README.md (Deploying from GitHub Actions) does not pass %s", name)
		}
	}
	if !strings.Contains(ci, `const bucket = (env.BACKYARD_CF_R2_BUCKET || "").trim();`) {
		t.Error("scripts/cloudflare-ci.mjs picks the R2 bucket differently now: check what deploy/cloudflare/README.md (Deploying from GitHub Actions) says about BACKYARD_CF_R2_BUCKET, then update this test")
	}
	flat := flatten(section)
	for _, want := range []string{
		"`backyard cloudflare status`", "`r2Bucket`", "set `BACKYARD_CF_R2_BUCKET` to it",
		"A deploy without it removes the R2 binding", "`workers_dev`", "`setup --no-workers-dev`",
		"set `BACKYARD_CF_AI` to `false`", "set `BACKYARD_CF_SITE_LISTING` to `owners`",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("deploy/cloudflare/README.md (Deploying from GitHub Actions) lacks %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^\s*"workers_dev": true,`).MatchString(readRepoFile(t, "deploy/cloudflare/wrangler.jsonc")) {
		t.Error("deploy/cloudflare/wrangler.jsonc no longer sets workers_dev to true: update what deploy/cloudflare/README.md says CI deploys serve on, then this test")
	}
}

// TestCloudflareJSONFlagDocumented checks the skill's global-flags sentence
// against commands.json: `backyard cloudflare` passes --json to the engine,
// which accepts it only on the commands that declare it (status), so the
// skill names them.
func TestCloudflareJSONFlagDocumented(t *testing.T) {
	var cmds engineCommands
	if err := json.Unmarshal([]byte(readRepoFile(t, "deploy/cloudflare/scripts/lib/commands.json")), &cmds); err != nil {
		t.Fatalf("deploy/cloudflare/scripts/lib/commands.json: %v", err)
	}
	for _, f := range cmds.Global {
		if f.Name == "json" {
			t.Skip("--json is a global engine flag now: every backyard cloudflare command accepts it")
		}
	}
	var with []string
	for _, c := range cmds.Commands {
		for _, f := range c.Flags {
			if f.Name == "json" {
				with = append(with, "`"+c.Name+"`")
			}
		}
	}
	if len(with) == 0 || len(with) == len(cmds.Commands) {
		t.Fatalf("--json on %d of %d engine commands: update this test", len(with), len(cmds.Commands))
	}
	want := "`--json` only for " + strings.Join(with, ", ")
	if text := flatten(readRepoFile(t, "skills/backyard/references/deploy.md")); !strings.Contains(text, want) {
		t.Errorf("skills/backyard/references/deploy.md lists --json as a global flag without %q", want)
	}
}

// TestSiteListingDocumented checks the "Owners-only site listing" section of
// docs/deploying-sites.md against the code: the configuration error it quotes
// is the one the Go server reports for sites.listing: owners with
// sites.ownership: none, the defaults it states hold (all; ownership owner
// with owners, none without), /info reports site_listing, and every switch
// its table names is one the kit reads — the Helm values, the Compose
// variable, the engine flag, the Cloudflare Terraform variable, the CI
// deployer's variable and the Worker variable.
func TestSiteListingDocumented(t *testing.T) {
	load := func(listing, ownership string) (*config.Config, error) {
		cfg := &config.Config{}
		cfg.Server.BaseDomain = "backyard.acme.com"
		cfg.Server.Secret = strings.Repeat("s", 40)
		cfg.Sites.Listing, cfg.Sites.Ownership = listing, ownership
		cfg.ApplyDefaults()
		return cfg, cfg.Validate()
	}
	section := flatten(markdownSection(t, readRepoFile(t, "docs/deploying-sites.md"), "Owners-only site listing"))

	for _, tc := range []struct{ listing, ownership, wantListing, wantOwnership string }{
		{"", "", sites.ListingAll, "none"},
		{sites.ListingOwners, "", sites.ListingOwners, "owner"},
		{sites.ListingOwners, "owner", sites.ListingOwners, "owner"},
	} {
		cfg, err := load(tc.listing, tc.ownership)
		if err != nil {
			t.Errorf("sites.listing %q, ownership %q: %v", tc.listing, tc.ownership, err)
			continue
		}
		if cfg.Sites.Listing != tc.wantListing || cfg.Sites.Ownership != tc.wantOwnership {
			t.Errorf("sites.listing %q, ownership %q: loaded as %q/%q, want %q/%q (as docs/deploying-sites.md says)",
				tc.listing, tc.ownership, cfg.Sites.Listing, cfg.Sites.Ownership, tc.wantListing, tc.wantOwnership)
		}
	}
	_, err := load(sites.ListingOwners, "none")
	var quoted string
	if err != nil {
		for _, line := range strings.Split(err.Error(), "\n") {
			if strings.HasPrefix(line, "sites.listing") {
				quoted = line
			}
		}
	}
	if quoted == "" {
		t.Errorf("sites.listing: owners with sites.ownership: none: validation error = %v, want one about sites.listing", err)
	} else if !strings.Contains(section, "`"+quoted+"`") {
		t.Errorf("docs/deploying-sites.md (Owners-only site listing) does not quote the error `%s`", quoted)
	}

	info, ok := reflect.TypeFor[deploy.Info]().FieldByName("SiteListing")
	if !ok || strings.Split(info.Tag.Get("json"), ",")[0] != "site_listing" {
		t.Error("deploy.Info has no field site_listing (update docs/deploying-sites.md and this test)")
	}
	for _, want := range []string{"`site_listing`", "`sites.listing: owners`", "`SITE_LISTING=owners`", "`backyard sites info <name>`"} {
		if !strings.Contains(section, want) {
			t.Errorf("docs/deploying-sites.md (Owners-only site listing) does not mention %s", want)
		}
	}

	// Each switch of the table, and where the kit reads it.
	for _, sw := range []struct{ doc, file, code string }{
		{"`config.sites.listing: owners`", "deploy/helm/backyard/values.yaml", "#   listing: owners"},
		{"`BACKYARD_SITE_LISTING=owners`", "deploy/docker-compose/backyard.yaml", "listing: ${BACKYARD_SITE_LISTING:-all}"},
		{"`BACKYARD_SITE_LISTING=owners`", "deploy/docker-compose/docker-compose.yml", "BACKYARD_SITE_LISTING: ${BACKYARD_SITE_LISTING:-all}"},
		{"`backyard cloudflare setup --site-listing owners`", "deploy/cloudflare/scripts/lib/commands.json", `"name": "site-listing"`},
		{"`site_listing = \"owners\"`", "deploy/terraform/cloudflare/variables.tf", `variable "site_listing"`},
		{"`BACKYARD_CF_SITE_LISTING=owners`", "scripts/cloudflare-ci.mjs", "env.BACKYARD_CF_SITE_LISTING"},
		{"`SITE_LISTING=owners`", "deploy/cloudflare/src/env.ts", "SITE_LISTING?: string"},
	} {
		if !strings.Contains(section, sw.doc) {
			t.Errorf("docs/deploying-sites.md (Owners-only site listing) does not name %s", sw.doc)
		}
		if !strings.Contains(readRepoFile(t, sw.file), sw.code) {
			t.Errorf("%s no longer has %q, which docs/deploying-sites.md (Owners-only site listing) documents as %s", sw.file, sw.code, sw.doc)
		}
	}
}
