package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/deploy"
)

// defaultsPlus returns the complete pattern list for the configured lines
// extra, as deploy.IgnorePatterns builds it.
func defaultsPlus(extra ...string) []string { return deploy.WithDefaultIgnore(extra) }

func TestS3Argv(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html": "x", ".git/config": "c", "node_modules/a.js": "x", "sub/node_modules/b.js": "x",
		"app.js.map": "m", "sub/x.map": "m", "keep.map": "k", "build/out.js": "o", "docs/a.md": "d", "docs/sub/b.md": "d",
		".well-known/security.txt": "s", ".well-known/.htaccess": "h", "weird[1]*.txt": "w",
	})
	cmd, err := buildSyncCommand(syncTarget{
		Method: "s3", Dir: dir, Site: "demo",
		Storage:  &blob.Description{Type: "s3", Bucket: "acme", Prefix: "/backyard/", Region: "eu-west-1", Endpoint: "https://minio:9000"},
		Patterns: defaultsPlus("/build", "docs/*.md", "*.map", "/weird[[]1]*.txt"),
	}, "aws")
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"aws", "s3", "sync", dir, "s3://acme/backyard/sites/demo/", "--delete", "--no-follow-symlinks",
		"--region", "eu-west-1", "--endpoint-url", "https://minio:9000",
		// Names whose glob means the same in fnmatch, at the root and below
		// (the any-case project file globs are not: their files, when
		// present, are listed by path).
		"--exclude", "CLAUDE.md", "--exclude", "*/CLAUDE.md",
		"--exclude", "AGENTS.md", "--exclude", "*/AGENTS.md",
		"--exclude", ".DS_Store", "--exclude", "*/.DS_Store",
		"--exclude", "*.map", "--exclude", "*/*.map",
		// Everything else by exact (escaped) path: ".*" would cross "/" in
		// fnmatch and swallow .well-known/security.txt.
		"--exclude", ".git/*", "--exclude", "build/*", "--exclude", "node_modules/*", "--exclude", "sub/node_modules/*",
		"--exclude", ".well-known/.htaccess", "--exclude", "docs/a.md", "--exclude", "weird[[]1][*].txt",
	}
	if !reflect.DeepEqual(cmd.Argv(), wantArgv) {
		t.Fatalf("argv =\n%q\nwant\n%q", cmd.Argv(), wantArgv)
	}
}

func TestGCSArgv(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html": "x", "app.js.map": "m", "sub/x.map": "m", ".git/HEAD": "h", "sub/.git/HEAD": "h",
		"docs/a.md": "d", "docs/b.md": "d",
	})
	target := syncTarget{Method: "gcs", Dir: dir, Site: "demo",
		Storage: &blob.Description{Type: "gcs", Bucket: "acme-sites", Prefix: "p"}, Patterns: []string{".git/", "*.map", "/docs/a.md"}}
	re := `\A(?:(?:.*/)?[^/]*\.map|\.git/.*|sub/\.git/.*|docs/a\.md)\Z`
	cmd, err := buildSyncCommand(target, "gcloud")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gcloud", "storage", "rsync", dir, "gs://acme-sites/p/sites/demo", "--recursive",
		"--delete-unmatched-destination-objects", "--exclude=" + re}
	if !reflect.DeepEqual(cmd.Argv(), want) {
		t.Fatalf("gcloud argv =\n%q\nwant\n%q", cmd.Argv(), want)
	}
	cmd, err = buildSyncCommand(target, "gsutil")
	if err != nil {
		t.Fatal(err)
	}
	// -e: gsutil follows file symlinks otherwise, while the api method
	// skips links leading out of the folder (gcloud skips symlinks).
	want = []string{"gsutil", "-m", "rsync", "-r", "-d", "-e", "-x", re, dir, "gs://acme-sites/p/sites/demo"}
	if !reflect.DeepEqual(cmd.Argv(), want) {
		t.Fatalf("gsutil argv =\n%q\nwant\n%q", cmd.Argv(), want)
	}
	// Nothing excluded → no exclude flag at all.
	clean := t.TempDir()
	writeFiles(t, clean, map[string]string{"index.html": "x"})
	cmd, err = buildSyncCommand(syncTarget{Method: "gcs", Dir: clean, Site: "demo",
		Storage: &blob.Description{Type: "gcs", Bucket: "b"}, Patterns: []string{"*.map"}}, "gcloud")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gcloud", "storage", "rsync", clean, "gs://b/sites/demo", "--recursive",
		"--delete-unmatched-destination-objects", "--exclude=" + `\A(?:(?:.*/)?[^/]*\.map)\Z`}; !reflect.DeepEqual(cmd.Argv(), want) {
		t.Fatalf("argv = %q", cmd.Argv())
	}
	cmd, _ = buildSyncCommand(syncTarget{Method: "gcs", Dir: clean, Site: "demo",
		Storage: &blob.Description{Type: "gcs", Bucket: "b"}, Patterns: []string{"/nothing-here"}}, "gcloud")
	if strings.Contains(strings.Join(cmd.Argv(), " "), "--exclude") {
		t.Fatalf("argv = %q", cmd.Argv())
	}
	if v := gcloudListValue(`a,b`); v != `^;^a,b` {
		t.Fatalf("gcloudListValue = %q", v)
	}
	if v := gcloudListValue(`\A[^/]\Z`); v != `\A[^/]\Z` {
		t.Fatalf("gcloudListValue changed a comma-free value: %q", v)
	}
}

// TestGCSRegexMatchesMatcher checks the generated Python regex against the
// deploy.Matcher (what the api method uploads) with a real Python
// interpreter (skipped when python3 is unavailable).
func TestGCSRegexMatchesMatcher(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	cases := []struct {
		name     string
		patterns []string
	}{
		{"defaults+user", defaultsPlus("*.map", "/drafts/", "docs/**/private", "!keep.map", "build/**", "[Tt]emp?.txt", "a\\*b")},
		{"negated dotfile", defaultsPlus("!.htaccess")},
		{"defaults", deploy.DefaultIgnore()},
		// .git/ and the project files cannot be re-included.
		{"re-include protected", defaultsPlus("!.*", "!.git/", "!.GIT/", "!**/.git/**", "!.backyard.json", "!.backyardignore", "!*")},
	}
	paths := []string{
		".GIT/config", "sub/.Git/HEAD", ".Backyard.json", "sub/.backyardignore", "a/.git",
		"index.html", "app.js", "app.js.map", "keep.map", "sub/keep.map", "sub/x.map",
		".git/config", "sub/.git/HEAD", "node_modules/a/b.js", "x/node_modules/c.js",
		".well-known/security.txt", ".well-known/.hidden", "sub/.well-known/x", ".env", "sub/.env", "a/.cache/b",
		"CLAUDE.md", "sub/CLAUDE.md", "AGENTS.md", ".backyard.json", ".backyardignore", ".DS_Store",
		"drafts/a.html", "sub/drafts/a.html", "docs/private", "docs/a/b/private", "docs/public",
		"build/out.js", "temp1.txt", "Temp2.txt", "temp10.txt", "a*b", "ab", ".claude/skills/x.md",
		".github/workflows/ci.yml", ".cursor/rules/x.mdc", ".htaccess", "sub/.htaccess", "weird (1)+[x].html",
	}
	dir := t.TempDir()
	files := map[string]string{}
	for _, p := range paths {
		files[p] = "x"
	}
	writeFiles(t, dir, files)
	script := `import json,re,sys
d=json.load(sys.stdin)
out=[]
for r in d["res"]:
    rx=re.compile(r) if r else None
    out.append([bool(rx and rx.match(p)) for p in d["paths"]])
print(json.dumps(out))`
	var res []string
	for _, c := range cases {
		ex, err := localExcludes(dir, c.patterns, nil)
		if err != nil {
			t.Fatal(err)
		}
		res = append(res, ex.pythonRegex())
	}
	in, _ := json.Marshal(map[string]any{"res": res, "paths": paths})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-I", "-c", script)
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v (regexes %q)", err, res)
	}
	var got [][]bool
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for ci, c := range cases {
		m := deploy.NewMatcher(c.patterns)
		for i, p := range paths {
			if want := m.Ignored(p, false); got[ci][i] != want {
				t.Errorf("%s: path %q: regex excludes=%v, matcher=%v (regex %s)", c.name, p, got[ci][i], want, res[ci])
			}
			protected := deploy.HasGitSegment(p) || strings.EqualFold(path.Base(p), ".backyard.json") || strings.EqualFold(path.Base(p), ".backyardignore")
			if protected && !got[ci][i] {
				t.Errorf("%s: %q is published", c.name, p)
			}
		}
	}
}

func TestAzureArgv(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"index.html": "x", ".env": "s", "sub/.hidden": "h", ".git/HEAD": "x", "node_modules/a.js": "x",
		"sub/node_modules/b.js": "x", ".well-known/security.txt": "x", "CLAUDE.md": "x", "drafts/a.html": "x",
		"keep/notes.md": "x",
	})
	cmd, err := buildSyncCommand(syncTarget{
		Method: "azure", Dir: dir, Site: "demo",
		Storage:  &blob.Description{Type: "azure", Account: "acmestore", Container: "backyard", Prefix: "prod/"},
		Patterns: defaultsPlus("/drafts/", "keep/notes.md"),
	}, "az")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"az", "storage", "blob", "sync", "--account-name", "acmestore", "-c", "backyard", "-s", dir,
		"-d", "prod/sites/demo", "--delete-destination", "true",
		"--exclude-pattern", "CLAUDE.md;AGENTS.md;.DS_Store;.*",
		"--exclude-path", ".git/;drafts/;keep/notes.md;node_modules/;sub/node_modules/"}
	if !reflect.DeepEqual(cmd.Argv(), want) {
		t.Fatalf("az argv =\n%q\nwant\n%q", cmd.Argv(), want)
	}
	// A negated file pattern disables name patterns: every excluded file is
	// listed explicitly instead.
	ex, err := localExcludes(dir, []string{".*", "!.env"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.names) != 0 || strings.Join(ex.pathPrefixes(), ";") != ".git/;.well-known/;sub/.hidden" {
		t.Fatalf("names=%q paths=%q", ex.names, ex.pathPrefixes())
	}
	if _, err := buildSyncCommand(syncTarget{Method: "azure", Dir: dir, Site: "demo",
		Storage: &blob.Description{Type: "azure", Container: "c"}}, "az"); err == nil {
		t.Fatal("missing account accepted")
	}
}

func TestRsyncArgv(t *testing.T) {
	dir := t.TempDir()
	cmd, err := buildSyncCommand(syncTarget{
		Method: "rsync", Dir: dir + "/", Site: "demo", RsyncTarget: "deploy@host:/var/lib/backyard/sites/",
		Patterns: []string{".git/", "docs/drafts", "/top.txt", ".*", "!.well-known/", "**/cache/"},
	}, "rsync")
	if err != nil {
		t.Fatal(err)
	}
	// --delete-excluded: files the rules exclude are removed from the
	// server too, as the api method's commit removes them; the protect
	// rule keeps the fs store's in-flight files.
	want := []string{"rsync", "-az", "--delete", "--delete-excluded", "--filter=P .backyard-tmp-*",
		"--exclude=cache/", "--include=.well-known/", "--exclude=.*", "--exclude=/top.txt", "--exclude=/docs/drafts", "--exclude=.git/",
		dir + "/", "deploy@host:/var/lib/backyard/sites/demo/"}
	if !reflect.DeepEqual(cmd.Argv(), want) {
		t.Fatalf("rsync argv =\n%q\nwant\n%q", cmd.Argv(), want)
	}
	if _, err := buildSyncCommand(syncTarget{Method: "rsync", Dir: filepath.Join(dir, "missing"), Site: "demo",
		RsyncTarget: "host:/x"}, "rsync"); err == nil {
		t.Fatal("missing folder accepted")
	}
}

// TestSymlinkedDeployFolder: a deploy folder reached through a symlink
// (current -> releases/42, or a symlinked $PWD) is resolved before the
// excludes are computed and before it is handed to the tool. A walk does not
// descend into a symlinked root, which left only the name globs: gsutil
// published .git/ and node_modules/ (and .env with a "!" line), and
// aws s3 sync --no-follow-symlinks would have skipped the root altogether.
func TestSymlinkedDeployFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	base := t.TempDir()
	real := filepath.Join(base, "releases", "42")
	writeFiles(t, real, map[string]string{
		"index.html": "x", ".git/config": "[core]", "node_modules/a/b.js": "x", ".env": "SECRET=1", ".htaccess": "h",
	})
	link := filepath.Join(base, "current")
	if err := os.Symlink(filepath.Join("releases", "42"), link); err != nil {
		t.Fatal(err)
	}
	realDir, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	// .git/ and node_modules/ are only excluded through the walk; with a
	// negated file pattern no name glob is used, so .env is listed too.
	for _, pc := range []struct{ patterns, wants []string }{
		{deploy.DefaultIgnore(), []string{".git", "node_modules"}},
		{defaultsPlus("!.htaccess"), []string{".git", "node_modules", ".env"}},
	} {
		patterns, wants := pc.patterns, pc.wants
		for _, tc := range []struct {
			method, tool string
			st           *blob.Description
		}{
			{"gcs", "gsutil", &blob.Description{Type: "gcs", Bucket: "b"}},
			{"gcs", "gcloud", &blob.Description{Type: "gcs", Bucket: "b"}},
			{"azure", "az", &blob.Description{Type: "azure", Account: "a", Container: "c"}},
			{"s3", "aws", &blob.Description{Type: "s3", Bucket: "b"}},
		} {
			for _, dir := range []string{link, link + "/"} {
				cmd, err := buildSyncCommand(syncTarget{Method: tc.method, Dir: dir, Site: "demo", Storage: tc.st, Patterns: patterns}, tc.tool)
				if err != nil {
					t.Fatal(err)
				}
				argv := strings.Join(cmd.Argv(), "\n")
				if !slices.Contains(cmd.Argv(), realDir) || strings.Contains(argv, link) {
					t.Errorf("%s/%s %v: the resolved folder is not passed: %q", tc.tool, patterns, dir, cmd.Argv())
				}
				for _, want := range wants {
					if !strings.Contains(argv, want) {
						t.Errorf("%s/%s: %s is not excluded: %q", tc.tool, patterns[len(patterns)-1], want, cmd.Argv())
					}
				}
			}
		}
	}
	ex, err := localExcludes(link, defaultsPlus("!.htaccess"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ex.pathPrefixes(), ";"); got != ".env;.git/;node_modules/" {
		t.Fatalf("excludes through a symlink = %q", got)
	}
}

func TestPruneCommands(t *testing.T) {
	paths := []string{"drafts/plan.html", "key.pem", "odd*[1].txt", "a#1.html"}
	s3 := syncTarget{Method: "s3", Site: "demo", Storage: &blob.Description{Type: "s3", Bucket: "acme", Prefix: "p", Region: "eu-west-1"}}
	cmds, skipped := buildPruneCommands(s3, "aws", paths)
	want := []string{"aws", "s3", "rm", "s3://acme/p/sites/demo/", "--recursive", "--region", "eu-west-1",
		"--exclude", "*", "--include", "drafts/plan.html", "--include", "key.pem", "--include", "odd[*][[]1].txt", "--include", "a#1.html"}
	if len(cmds) != 1 || len(skipped) != 0 || !reflect.DeepEqual(cmds[0].Argv(), want) {
		t.Fatalf("s3 = %q %q", cmds, skipped)
	}
	gcs := syncTarget{Method: "gcs", Site: "demo", Storage: &blob.Description{Type: "gcs", Bucket: "acme"}}
	cmds, skipped = buildPruneCommands(gcs, "gcloud", paths)
	if len(cmds) != 1 || !reflect.DeepEqual(cmds[0].Argv(), []string{"gcloud", "storage", "rm",
		"gs://acme/sites/demo/drafts/plan.html", "gs://acme/sites/demo/key.pem"}) || strings.Join(skipped, ",") != "odd*[1].txt,a#1.html" {
		t.Fatalf("gcloud = %q %q", cmds, skipped)
	}
	cmds, _ = buildPruneCommands(gcs, "gsutil", paths[:2])
	if len(cmds) != 1 || !reflect.DeepEqual(cmds[0].Argv()[:3], []string{"gsutil", "-m", "rm"}) {
		t.Fatalf("gsutil = %q", cmds)
	}
	many := make([]string, 250)
	for i := range many {
		many[i] = fmt.Sprintf("f%d.pem", i)
	}
	if cmds, _ = buildPruneCommands(gcs, "gcloud", many); len(cmds) != 3 || len(cmds[2].Args) != 2+50 {
		t.Fatalf("chunks = %d", len(cmds))
	}
	az := syncTarget{Method: "azure", Site: "demo", Storage: &blob.Description{Type: "azure", Account: "acct", Container: "c", Prefix: "prod"}}
	cmds, skipped = buildPruneCommands(az, "az", paths[:2])
	if len(cmds) != 2 || len(skipped) != 0 || !reflect.DeepEqual(cmds[1].Argv(), []string{"az", "storage", "blob", "delete",
		"--account-name", "acct", "-c", "c", "-n", "prod/sites/demo/key.pem"}) {
		t.Fatalf("azure = %q", cmds)
	}
	if cmds, _ := buildPruneCommands(syncTarget{Method: "rsync"}, "rsync", paths); cmds != nil {
		t.Fatalf("rsync = %q", cmds)
	}
}

func TestDirectReady(t *testing.T) {
	tests := []struct {
		method string
		info   *deploy.Info
		ok     bool
	}{
		{"s3", &deploy.Info{Storage: &blob.Description{Type: "s3", Bucket: "b"}}, true},
		{"s3", &deploy.Info{Storage: &blob.Description{Type: "gcs", Bucket: "b"}}, false},
		{"s3", &deploy.Info{}, false},
		{"gcs", &deploy.Info{Storage: &blob.Description{Type: "gcs", Bucket: "b"}}, true},
		{"azure", &deploy.Info{Storage: &blob.Description{Type: "azure", Account: "a", Container: "c"}}, true},
		{"rsync", &deploy.Info{RsyncTarget: "host:/x"}, true},
		{"rsync", &deploy.Info{}, false},
		{"api", &deploy.Info{}, false},
		{"s3", nil, false},
	}
	for _, tt := range tests {
		if got := directReady(tt.method, tt.info) == ""; got != tt.ok {
			t.Errorf("directReady(%s, %+v) ok=%v", tt.method, tt.info, got)
		}
	}
}

func TestGlobToRegex(t *testing.T) {
	tests := map[string]string{
		"*.js":    `[^/]*\.js`,
		"a?c":     `a[^/]c`,
		"[!a-c]x": `[^a-c]x`,
		"[]a]":    `[]a]`,
		"[abc":    `\[abc`,
		`a\*b`:    `a\*b`,
		"x+y(1)":  `x\+y\(1\)`,
	}
	for in, want := range tests {
		if got := globToRegex(in); got != want {
			t.Errorf("globToRegex(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCommandString(t *testing.T) {
	c := Command{Name: "aws", Args: []string{"s3", "sync", "/my dir", "--exclude", "*/x", "it's"}}
	if got := c.String(); got != `aws s3 sync '/my dir' --exclude '*/x' 'it'\''s'` {
		t.Fatalf("String = %s", got)
	}
	if filepath.Base(c.Argv()[0]) != "aws" {
		t.Fatal("Argv")
	}
}

// TestS3SyncMatchesMatcher runs the generated aws s3 sync and aws s3 rm
// filters the way the AWS CLI evaluates them (awscli/customizations/s3/
// filters.py: each pattern is joined to the source root and to the
// destination root, matched with Python's fnmatch — where "*" and "?" cross
// "/" — and the last matching filter wins; sync --delete never deletes a
// destination key its filters exclude) over a local folder and a bucket
// holding an older version of the site. After the sync and the prune of the
// ignored files the gateway lists, the bucket must hold exactly what the api
// method publishes (deploy.Matcher), each path from the local folder.
// Skipped without python3.
func TestS3SyncMatchesMatcher(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	cases := []struct {
		name     string
		patterns []string
	}{
		{"defaults", deploy.DefaultIgnore()},
		{"defaults+user", defaultsPlus("*.map", "/drafts/", "!keep.map", "build/", "[Tt]emp?.txt", "*.pem")},
		{"negated dotfile", defaultsPlus("!.htaccess")},
		// Anchored wildcards must not cross "/" ...
		{"anchored wildcards", defaultsPlus("/*.json", "docs/*.md", "a?b/x", "a*b")},
		// ... a file inside an excluded directory cannot be re-included ...
		{"re-include inside excluded dir", defaultsPlus("drafts/", "!drafts/keep.html")},
		// ... and a negated anchored wildcard must not re-include ignored files.
		{"negated anchored wildcard", defaultsPlus("*.json", "!/*.json")},
		{"literal metacharacters", defaultsPlus(`/odd\*[[]1].txt`, "weird (1)+[x].html")},
		// .git/ and the project files cannot be re-included.
		{"re-include protected", defaultsPlus("!.*", "!.git/", "!.GIT/", "!**/.git/**", "!.backyard.json", "!.backyardignore", "!*")},
	}
	local := []string{
		".GIT/config", "sub/.Git/HEAD", ".Backyard.json", "sub/.backyardignore", "a/.git",
		"index.html", "app.js", "app.js.map", "keep.map", "sub/keep.map", "sub/x.map",
		".git/config", "sub/.git/HEAD", "node_modules/a/b.js", "x/node_modules/c.js", "node_modules/a/package.json",
		".well-known/security.txt", ".well-known/acme-challenge/token", ".well-known/.htaccess",
		".well-known/sub/.hidden", ".well-known/.git/config", ".well-known/node_modules/x.js",
		".well-known/CLAUDE.md", ".well-known/x.map", "sub/.well-known/x", "sub/.well-known/.y",
		".env", "sub/.env", "a/.cache/b", "CLAUDE.md", "sub/CLAUDE.md", "AGENTS.md", ".backyard.json",
		".backyardignore", ".DS_Store", "img/.DS_Store", "drafts/a.html", "drafts/keep.html", "sub/drafts/a.html",
		"build/out.js", "temp1.txt", "Temp2.txt", "temp10.txt", ".claude/skills/x.md", ".claude/settings.json",
		".github/workflows/ci.yml", ".cursor/rules/x.mdc", ".htaccess", "sub/.htaccess", "key.pem",
		"weird (1)+[x].html", "package.json", "tsconfig.json", "data/items.json", "docs/a.md", "docs/sub/b.md",
		"a/b/x", "axb/x", "a/y/b", "x/a/y/b", "ab", "odd*[1].txt", "oddX1.txt", "odd*1.txt",
	}
	// The bucket holds an older deploy: every local path plus files removed
	// locally since (some of them ignored now).
	remote := append(slices.Clone(local), "old.html", "old/.env", "drafts/old.html", "secrets/x.pem", "sub/CLAUDE.md.old")
	dir := t.TempDir()
	files := map[string]string{}
	for _, p := range local {
		files[p] = "x"
	}
	writeFiles(t, dir, files)
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	type filter [2]string
	type evalCase struct {
		Sync  []filter `json:"sync"`
		Prune []filter `json:"prune"`
	}
	var in struct {
		Root   string     `json:"root"`
		Dest   string     `json:"dest"`
		Local  []string   `json:"local"`
		Remote []string   `json:"remote"`
		Cases  []evalCase `json:"cases"`
	}
	in.Root, in.Dest, in.Local, in.Remote = realDir, "bucket/sites/demo", local, remote
	toFilters := func(args []string, from int) []filter {
		var out []filter
		for i := from; i+1 < len(args); i++ {
			if strings.HasPrefix(args[i], "--exclude") || strings.HasPrefix(args[i], "--include") {
				out = append(out, filter{strings.TrimPrefix(args[i], "--"), args[i+1]})
				i++
			}
		}
		return out
	}
	target := func(patterns []string) syncTarget {
		return syncTarget{Method: "s3", Dir: dir, Site: "demo", Storage: &blob.Description{Type: "s3", Bucket: "bucket"}, Patterns: patterns}
	}
	for _, c := range cases {
		cmd, err := buildSyncCommand(target(c.patterns), "aws")
		if err != nil {
			t.Fatal(err)
		}
		// What the gateway lists after the sync: computed from the matcher,
		// as ignoredSiteFiles does (the emulation checks it independently).
		m := deploy.NewMatcher(c.patterns)
		var stale []string
		for _, p := range remote {
			if m.Ignored(p, false) {
				stale = append(stale, p)
			}
		}
		prune, _ := buildPruneCommands(target(c.patterns), "aws", stale)
		ec := evalCase{Sync: toFilters(cmd.Args, 0)}
		if len(stale) > 0 {
			ec.Prune = toFilters(prune[0].Args, 0)
		}
		in.Cases = append(in.Cases, ec)
	}
	script := `import fnmatch,json,os,sys
d=json.load(sys.stdin)
def included(path, filters, roots):
    ok=True
    for typ,pat in filters:
        for r in roots:
            if fnmatch.fnmatch(path, os.path.join(r, pat)):
                ok=(typ=="include")
    return ok
out=[]
for c in d["cases"]:
    roots=[d["root"], d["dest"]]
    bucket={p:"old" for p in d["remote"]}
    local=set(d["local"])
    for p in d["local"]:   # upload (local paths are matched with the local root)
        if included(os.path.join(d["root"], p), c["sync"], roots):
            bucket[p]="new"
    for p in list(bucket): # --delete (keys with the destination root)
        if p not in local and included(d["dest"]+"/"+p, c["sync"], roots):
            del bucket[p]
    if c["prune"]:         # aws s3 rm --recursive (source = destination)
        for p in list(bucket):
            if included(d["dest"]+"/"+p, c["prune"], [d["dest"]+"/", d["dest"]+"/"]):
                del bucket[p]
    out.append(bucket)
print(json.dumps(out))`
	b, _ := json.Marshal(in)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-I", "-c", script)
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("python: %v", err)
	}
	var got []map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for ci, c := range cases {
		m := deploy.NewMatcher(c.patterns)
		want := map[string]string{}
		for _, p := range local {
			if !m.Ignored(p, false) {
				want[p] = "new"
			}
		}
		for _, p := range keys(want) {
			if got[ci][p] != "new" {
				t.Errorf("%s: %q should be published from the folder, bucket has %q", c.name, p, got[ci][p])
			}
		}
		for _, p := range keys(got[ci]) {
			if want[p] == "" {
				t.Errorf("%s: %q is ignored but left in the bucket (%s)", c.name, p, got[ci][p])
			}
		}
	}
}
