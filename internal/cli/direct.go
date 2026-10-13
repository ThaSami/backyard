package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
)

// syncTarget describes a direct-sync deploy (SPEC §13): copying a folder
// straight into the server's storage with the storage provider's own tool.
type syncTarget struct {
	Method      string
	Dir         string // absolute local folder
	Site        string
	Storage     *blob.Description
	RsyncTarget string
	// Patterns are the complete ignore patterns (deploy.IgnorePatterns).
	Patterns []string
}

// directTools lists the executables usable for each direct method, in
// preference order.
var directTools = map[string][]string{
	clientconfig.MethodS3:    {"aws"},
	clientconfig.MethodGCS:   {"gcloud", "gsutil"},
	clientconfig.MethodAzure: {"az"},
	clientconfig.MethodRsync: {"rsync"},
}

// toolInstallHints tell users how to get a missing tool.
var toolInstallHints = map[string]string{
	clientconfig.MethodS3:    "install the AWS CLI: https://aws.amazon.com/cli/",
	clientconfig.MethodGCS:   "install the Google Cloud CLI: https://cloud.google.com/sdk/docs/install",
	clientconfig.MethodAzure: "install the Azure CLI: https://learn.microsoft.com/cli/azure/install-azure-cli",
	clientconfig.MethodRsync: "install rsync with your package manager",
}

// directReady reports why method cannot be used with info ("" when it can).
func directReady(method string, info *deploy.Info) string {
	if info == nil {
		return "the server's storage details are unknown (the gateway is unreachable and no cached info exists)"
	}
	st := info.Storage
	switch method {
	case clientconfig.MethodS3, clientconfig.MethodGCS:
		if st == nil || st.Type != method || st.Bucket == "" {
			return "the server does not expose " + method + " bucket details (deploy.expose_storage)"
		}
	case clientconfig.MethodAzure:
		if st == nil || st.Type != "azure" || st.Container == "" || st.Account == "" {
			return "the server does not expose its Azure storage account/container (deploy.expose_storage)"
		}
	case clientconfig.MethodRsync:
		if strings.TrimSpace(info.RsyncTarget) == "" {
			return "the server does not advertise an rsync target (deploy.rsync_target)"
		}
	default:
		return "not a direct-sync method"
	}
	return ""
}

// storagePrefix normalizes a storage prefix to "" or "p/".
func storagePrefix(st *blob.Description) string {
	if st == nil {
		return ""
	}
	p := strings.Trim(strings.TrimSpace(st.Prefix), "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// buildSyncCommand returns the command for t. tool selects the executable
// for methods with alternatives (gcs: gcloud or gsutil).
//
// The folder is resolved through symlinks first, as deploy.CollectFiles
// does: the excludes are computed by walking it (which does not descend
// into a symlinked root), and aws s3 sync --no-follow-symlinks would skip a
// symlinked root entirely — and then delete the whole site.
func buildSyncCommand(t syncTarget, tool string) (Command, error) {
	if why := directReady(t.Method, &deploy.Info{Storage: t.Storage, RsyncTarget: t.RsyncTarget}); why != "" {
		return Command{}, errors.New(why)
	}
	dir, err := resolveSyncDir(t.Dir)
	if err != nil {
		return Command{}, err
	}
	t.Dir = dir
	switch t.Method {
	case clientconfig.MethodS3:
		return buildS3Command(t)
	case clientconfig.MethodGCS:
		return buildGCSCommand(t, tool == "gsutil")
	case clientconfig.MethodAzure:
		return buildAzureCommand(t)
	case clientconfig.MethodRsync:
		return buildRsyncCommand(t), nil
	}
	return Command{}, fmt.Errorf("unknown direct method %q", t.Method)
}

// resolveSyncDir returns the absolute deploy folder with symlinks resolved.
func resolveSyncDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("deploy folder %s: %w", dir, err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("deploy folder %s: %w", dir, err)
	}
	return real, nil
}

// s3Dest returns s3://<bucket>/<prefix>sites/<site>/.
func s3Dest(t syncTarget) string {
	return "s3://" + t.Storage.Bucket + "/" + storagePrefix(t.Storage) + "sites/" + t.Site + "/"
}

// s3Location returns the --region and --endpoint-url flags.
func s3Location(st *blob.Description) []string {
	var args []string
	if st.Region != "" {
		args = append(args, "--region", st.Region)
	}
	if st.Endpoint != "" {
		args = append(args, "--endpoint-url", st.Endpoint)
	}
	return args
}

// buildS3Command: aws s3 sync <dir> s3://<bucket>/<prefix>sites/<site>/
// --delete --no-follow-symlinks [--region r] [--endpoint-url e] --exclude ….
//
// The AWS CLI matches filters with Python's fnmatch against the whole path,
// where "*" and "?" cross "/", and lets a later --include re-include a file
// inside an excluded directory: it cannot express gitignore semantics. So
// the excludes are derived from walking the folder with deploy.Matcher (the
// rules the api method applies): every excluded directory ("dir/*") and
// file by its exact, escaped path, plus the single-segment names whose glob
// means the same in fnmatch (literal names and "*suffix", at any depth).
//
// --no-follow-symlinks: CollectFiles never follows a symlinked directory or
// a link leading outside the folder; the AWS CLI follows both by default.
func buildS3Command(t syncTarget) (Command, error) {
	ex, err := localExcludes(t.Dir, t.Patterns, fnmatchSafeName)
	if err != nil {
		return Command{}, err
	}
	args := []string{"s3", "sync", t.Dir, s3Dest(t), "--delete", "--no-follow-symlinks"}
	args = append(args, s3Location(t.Storage)...)
	for _, g := range ex.fnmatchGlobs() {
		args = append(args, "--exclude", g)
	}
	return Command{Name: "aws", Args: args}, nil
}

// buildGCSCommand: gcloud storage rsync <dir> gs://<bucket>/<prefix>sites/<site>
// --recursive --delete-unmatched-destination-objects --exclude=<regex>, or
// gsutil -m rsync -r -d -e -x <regex> <dir> gs://… when gcloud is missing.
// gcloud skips symlinks by default; gsutil follows file symlinks unless -e.
func buildGCSCommand(t syncTarget, gsutil bool) (Command, error) {
	dest := gcsDest(t)
	ex, err := localExcludes(t.Dir, t.Patterns, nil)
	if err != nil {
		return Command{}, err
	}
	re := ex.pythonRegex()
	if gsutil {
		args := []string{"-m", "rsync", "-r", "-d", "-e"}
		if re != "" {
			args = append(args, "-x", re)
		}
		return Command{Name: "gsutil", Args: append(args, t.Dir, dest)}, nil
	}
	args := []string{"storage", "rsync", t.Dir, dest, "--recursive", "--delete-unmatched-destination-objects"}
	if re != "" {
		args = append(args, "--exclude="+gcloudListValue(re))
	}
	return Command{Name: "gcloud", Args: args}, nil
}

// buildAzureCommand: az storage blob sync --account-name <a> -c <container>
// -s <dir> -d <prefix>sites/<site> --delete-destination true
// [--exclude-pattern "p1;p2"] [--exclude-path "d1/;f2"].
func buildAzureCommand(t syncTarget) (Command, error) {
	args := []string{"storage", "blob", "sync",
		"--account-name", t.Storage.Account,
		"-c", t.Storage.Container,
		"-s", t.Dir,
		"-d", storagePrefix(t.Storage) + "sites/" + t.Site,
		"--delete-destination", "true",
	}
	ex, err := localExcludes(t.Dir, t.Patterns, nil)
	if err != nil {
		return Command{}, err
	}
	if len(ex.names) > 0 {
		args = append(args, "--exclude-pattern", strings.Join(ex.names, ";"))
	}
	if paths := ex.pathPrefixes(); len(paths) > 0 {
		args = append(args, "--exclude-path", strings.Join(paths, ";"))
	}
	return Command{Name: "az", Args: args}, nil
}

// gcsDest returns gs://<bucket>/<prefix>sites/<site>.
func gcsDest(t syncTarget) string {
	return "gs://" + t.Storage.Bucket + "/" + storagePrefix(t.Storage) + "sites/" + t.Site
}

// fsTempPattern matches the fs store's in-flight files (internal/blob/fs).
const fsTempPattern = ".backyard-tmp-*"

// buildRsyncCommand: rsync -az --delete --delete-excluded <filters> <dir>/
// <rsync_target>/<site>/. --delete-excluded removes files the ignore rules
// exclude from the server too (as the api method's commit does); the
// receiver-side protect rule keeps the fs store's in-flight
// ".backyard-tmp-*" files, which the default ".*" rule matches.
func buildRsyncCommand(t syncTarget) Command {
	args := []string{"-az", "--delete", "--delete-excluded", "--filter=P " + fsTempPattern}
	args = append(args, rsyncFilters(t.Patterns)...)
	target := strings.TrimRight(strings.TrimSpace(t.RsyncTarget), "/")
	src := strings.TrimRight(t.Dir, "/") + "/"
	return Command{Name: "rsync", Args: append(args, src, target+"/"+t.Site+"/")}
}

// buildPruneCommands returns the commands deleting site files (paths
// relative to the site) from the storage. The direct-sync tools never delete
// destination files their excludes match, so files published earlier that
// the ignore rules now exclude are removed this way, as the api method's
// commit removes them. Paths the tool cannot name literally are returned in
// skipped. (rsync needs none of this: it runs with --delete-excluded.)
func buildPruneCommands(t syncTarget, tool string, paths []string) (cmds []Command, skipped []string) {
	switch t.Method {
	case clientconfig.MethodS3:
		// One listing of the site, deleting exactly the escaped paths.
		args := []string{"s3", "rm", s3Dest(t), "--recursive"}
		args = append(args, s3Location(t.Storage)...)
		args = append(args, "--exclude", "*")
		for _, p := range paths {
			args = append(args, "--include", fnmatchEscape(p))
		}
		return []Command{{Name: "aws", Args: args}}, nil
	case clientconfig.MethodGCS:
		// gcloud and gsutil expand wildcards in URLs and read "#<n>" as a
		// generation; such names cannot be addressed literally.
		var urls []string
		for _, p := range paths {
			if strings.ContainsAny(p, "*?[]#") {
				skipped = append(skipped, p)
				continue
			}
			urls = append(urls, gcsDest(t)+"/"+p)
		}
		for chunk := range slices.Chunk(urls, 100) {
			if tool == "gsutil" {
				cmds = append(cmds, Command{Name: "gsutil", Args: append([]string{"-m", "rm"}, chunk...)})
			} else {
				cmds = append(cmds, Command{Name: "gcloud", Args: append([]string{"storage", "rm"}, chunk...)})
			}
		}
		return cmds, skipped
	case clientconfig.MethodAzure:
		for _, p := range paths {
			cmds = append(cmds, Command{Name: "az", Args: []string{"storage", "blob", "delete",
				"--account-name", t.Storage.Account, "-c", t.Storage.Container,
				"-n", storagePrefix(t.Storage) + "sites/" + t.Site + "/" + p}})
		}
		return cmds, nil
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Ignore pattern translation

// ignoreRule is one parsed gitignore line (mirrors deploy.Matcher's syntax).
type ignoreRule struct {
	negate   bool
	dirOnly  bool
	anchored bool
	segs     []string // glob segments; "**" for any depth
}

// body returns the "/"-joined segments.
func (r ignoreRule) body() string { return strings.Join(r.segs, "/") }

// parseIgnoreRules parses gitignore lines, dropping blanks and comments.
func parseIgnoreRules(patterns []string) []ignoreRule {
	var rules []ignoreRule
	for _, line := range patterns {
		line = strings.TrimSuffix(line, "\r")
		for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
			line = line[:len(line)-1]
		}
		if line == "" || line[0] == '#' {
			continue
		}
		var r ignoreRule
		if line[0] == '!' {
			r.negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.dirOnly = true
			line = strings.TrimRight(line, "/")
		}
		if line == "" {
			continue
		}
		r.anchored = strings.Contains(line, "/")
		for s := range strings.SplitSeq(strings.TrimPrefix(line, "/"), "/") {
			if s == "" {
				continue
			}
			if s != "**" && strings.Contains(s, "**") {
				for strings.Contains(s, "**") {
					s = strings.ReplaceAll(s, "**", "*")
				}
			}
			r.segs = append(r.segs, s)
		}
		if len(r.segs) == 0 {
			continue
		}
		// A leading "**/" makes an anchored pattern match at any depth.
		if r.segs[0] == "**" {
			r.anchored = false
			r.segs = r.segs[1:]
			if len(r.segs) == 0 {
				continue
			}
		}
		rules = append(rules, r)
	}
	return rules
}

// globToRegex translates one path-segment glob ("*", "?", "[…]", "\x") to a
// regex fragment that never crosses "/".
func globToRegex(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '\\':
			if i+1 < len(g) {
				i++
				b.WriteString(regexp.QuoteMeta(string(g[i])))
			} else {
				b.WriteString(`\\`)
			}
		case '[':
			j := i + 1
			if j < len(g) && (g[j] == '!' || g[j] == '^') {
				j++
			}
			if j < len(g) && g[j] == ']' { // a literal "]" first in the class
				j++
			}
			k := strings.IndexByte(g[j:], ']')
			if k < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : j+k]
			if class[0] == '!' {
				class = "^" + class[1:]
			}
			class = strings.ReplaceAll(class, "[", `\[`)
			b.WriteString("[" + class + "]")
			i = j + k
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

// gcloudListValue protects a value passed to a gcloud list flag: commas
// would split it, so an alternate delimiter (^D^) is declared when needed.
func gcloudListValue(v string) string {
	if !strings.Contains(v, ",") {
		return v
	}
	for _, d := range []string{";", "~", "@", "#", "%", "&", "!", "="} {
		if !strings.Contains(v, d) {
			return "^" + d + "^" + v
		}
	}
	return v
}

// rsyncFilters translates patterns to rsync --exclude/--include flags.
// rsync uses the first matching rule while gitignore uses the last, so the
// rules are emitted in reverse order. Patterns with an inner "/" are
// anchored to the transfer root with a leading "/", as in gitignore.
func rsyncFilters(patterns []string) []string {
	rules := parseIgnoreRules(patterns)
	var args []string
	for i := len(rules) - 1; i >= 0; i-- {
		r := rules[i]
		p := r.body()
		if r.anchored {
			p = "/" + p
		}
		if r.dirOnly {
			p += "/"
		}
		if r.negate {
			args = append(args, "--include="+p)
		} else {
			args = append(args, "--exclude="+p)
		}
	}
	return args
}

// excludes describes what the ignore patterns exclude in a local folder,
// in a form storage sync tools can express exactly (they cannot evaluate
// gitignore semantics such as re-included directories themselves).
type excludes struct {
	// names are single-segment file globs (".*", "CLAUDE.md", "*.map"),
	// matched against file names at any depth. They are only used when no
	// negated file pattern could re-include such a file.
	names []string
	// dirs are the top-most excluded directories (relative, "/"-separated).
	dirs []string
	// files are the other excluded files outside excluded directories.
	files []string
}

// localExcludes walks dir with the deploy matcher (the same rules the api
// method applies) and summarizes the exclusions. nameOK (nil: any) limits
// which single-segment name globs may stand for the files they match; the
// files of the other names are listed one by one. A symlinked dir is
// resolved first: the walk would not descend into it.
func localExcludes(dir string, patterns []string, nameOK func(string) bool) (*excludes, error) {
	ex := &excludes{}
	useNames := true
	rules := parseIgnoreRules(patterns)
	for _, r := range rules {
		if r.negate && !r.dirOnly {
			useNames = false
		}
	}
	if useNames {
		for _, r := range rules {
			if !r.negate && !r.dirOnly && !r.anchored && len(r.segs) == 1 && !strings.ContainsAny(r.segs[0], `\[`) &&
				(nameOK == nil || nameOK(r.segs[0])) && !slices.Contains(ex.names, r.segs[0]) {
				ex.names = append(ex.names, r.segs[0])
			}
		}
	}
	m := deploy.NewMatcher(patterns)
	root, err := resolveSyncDir(dir)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if m.Ignored(rel, true) {
				ex.dirs = append(ex.dirs, rel)
				return filepath.SkipDir
			}
			return nil
		}
		if !m.Ignored(rel, false) {
			return nil
		}
		for _, n := range ex.names {
			if ok, _ := path.Match(n, path.Base(rel)); ok {
				return nil // covered by a name pattern
			}
		}
		ex.files = append(ex.files, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", dir, err)
	}
	return ex, nil
}

// fnmatchSafeName reports whether the name glob n matches the same paths in
// Python's fnmatch (used by the AWS CLI's filters, where "*" and "?" also
// match "/") as a gitignore name at any depth does, when emitted as "n" and
// "*/n": a literal name, or "*" followed by a literal suffix.
func fnmatchSafeName(n string) bool {
	return n != "" && !strings.ContainsAny(strings.TrimPrefix(n, "*"), `*?[]\`)
}

// fnmatchEscape quotes the fnmatch metacharacters of a literal path.
func fnmatchEscape(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*', '?', '[':
			b.WriteString("[" + string(r) + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fnmatchGlobs returns the exclusions as fnmatch globs relative to the
// folder (aws s3 --exclude): each name at the root and at any depth, each
// excluded directory's contents and each excluded file.
func (e *excludes) fnmatchGlobs() []string {
	var out []string
	for _, n := range e.names {
		out = append(out, n, "*/"+n)
	}
	for _, d := range e.dirs {
		out = append(out, fnmatchEscape(d)+"/*")
	}
	for _, f := range e.files {
		out = append(out, fnmatchEscape(f))
	}
	return out
}

// pathPrefixes returns the excluded directories (with a trailing "/") and
// files as relative path prefixes, sorted (az --exclude-path).
func (e *excludes) pathPrefixes() []string {
	out := make([]string, 0, len(e.dirs)+len(e.files))
	for _, d := range e.dirs {
		out = append(out, d+"/")
	}
	out = append(out, e.files...)
	slices.Sort(out)
	return out
}

// pythonRegex returns one Python regular expression matching every
// excluded relative path ("" when nothing is excluded), for gcloud storage
// rsync --exclude / gsutil rsync -x. \A and \Z anchors are used so the
// expression never starts with "^", which gcloud would read as a
// list-delimiter override.
func (e *excludes) pythonRegex() string {
	var alts []string
	for _, n := range e.names {
		alts = append(alts, "(?:.*/)?"+globToRegex(n))
	}
	for _, d := range e.dirs {
		alts = append(alts, regexp.QuoteMeta(d)+"/.*")
	}
	for _, f := range e.files {
		alts = append(alts, regexp.QuoteMeta(f))
	}
	if len(alts) == 0 {
		return ""
	}
	return `\A(?:` + strings.Join(alts, "|") + `)\Z`
}
