package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/deploy"
)

// defaultsPlus returns the default ignore patterns plus extra lines.
func defaultsPlus(extra ...string) []string { return append(deploy.DefaultIgnore(), extra...) }

func TestS3FiltersAndArgv(t *testing.T) {
	got := s3Filters([]string{"node_modules/", "/build", "docs/*.md", "*.map", "!keep.map", "# comment", "", "**/tmp"})
	want := []string{
		"--exclude", "node_modules", "--exclude", "node_modules/*", "--exclude", "*/node_modules", "--exclude", "*/node_modules/*",
		"--exclude", "build", "--exclude", "build/*",
		"--exclude", "docs/*.md", "--exclude", "docs/*.md/*",
		"--exclude", "*.map", "--exclude", "*.map/*", "--exclude", "*/*.map", "--exclude", "*/*.map/*",
		"--include", "keep.map", "--include", "keep.map/*", "--include", "*/keep.map", "--include", "*/keep.map/*",
		"--exclude", "tmp", "--exclude", "tmp/*", "--exclude", "*/tmp", "--exclude", "*/tmp/*",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("s3Filters =\n%q\nwant\n%q", got, want)
	}

	cmd, err := buildSyncCommand(syncTarget{
		Method: "s3", Dir: "/work/site", Site: "demo",
		Storage:  &blob.Description{Type: "s3", Bucket: "acme", Prefix: "/backyard/", Region: "eu-west-1", Endpoint: "https://minio:9000"},
		Patterns: []string{".git/", "!.well-known/"},
	}, "aws")
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{"aws", "s3", "sync", "/work/site", "s3://acme/backyard/sites/demo/", "--delete",
		"--region", "eu-west-1", "--endpoint-url", "https://minio:9000",
		"--exclude", ".git", "--exclude", ".git/*", "--exclude", "*/.git", "--exclude", "*/.git/*",
		"--include", ".well-known", "--include", ".well-known/*", "--include", "*/.well-known", "--include", "*/.well-known/*",
		// .git/ re-applied below the re-included .well-known/.
		"--exclude", ".well-known/.git", "--exclude", ".well-known/.git/*", "--exclude", ".well-known/*/.git", "--exclude", ".well-known/*/.git/*",
		"--exclude", "*/.well-known/.git", "--exclude", "*/.well-known/.git/*", "--exclude", "*/.well-known/*/.git", "--exclude", "*/.well-known/*/.git/*",
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
	want = []string{"gsutil", "-m", "rsync", "-r", "-d", "-x", re, dir, "gs://acme-sites/p/sites/demo"}
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
	}
	paths := []string{
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
		ex, err := localExcludes(dir, c.patterns)
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
		"--exclude-pattern", ".backyard.json;.backyardignore;CLAUDE.md;AGENTS.md;.DS_Store;.*",
		"--exclude-path", ".git/;drafts/;keep/notes.md;node_modules/;sub/node_modules/"}
	if !reflect.DeepEqual(cmd.Argv(), want) {
		t.Fatalf("az argv =\n%q\nwant\n%q", cmd.Argv(), want)
	}
	// A negated file pattern disables name patterns: every excluded file is
	// listed explicitly instead.
	ex, err := localExcludes(dir, []string{".*", "!.env"})
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
	cmd, err := buildSyncCommand(syncTarget{
		Method: "rsync", Dir: "/work/site/", Site: "demo", RsyncTarget: "deploy@host:/var/lib/backyard/sites/",
		Patterns: []string{".git/", "docs/drafts", "/top.txt", ".*", "!.well-known/", "**/cache/"},
	}, "rsync")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rsync", "-az", "--delete",
		"--exclude=cache/", "--include=.well-known/", "--exclude=.*", "--exclude=/top.txt", "--exclude=/docs/drafts", "--exclude=.git/",
		"/work/site/", "deploy@host:/var/lib/backyard/sites/demo/"}
	if !reflect.DeepEqual(cmd.Argv(), want) {
		t.Fatalf("rsync argv =\n%q\nwant\n%q", cmd.Argv(), want)
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

// TestS3FiltersMatchMatcher evaluates the generated aws s3 sync filters the
// way the AWS CLI does (awscli/customizations/s3/filters.py: each pattern is
// joined to the source directory, matched with Python's fnmatch — where "*"
// crosses "/" — and the last matching filter wins) and compares the result
// with deploy.Matcher, i.e. with what the api method uploads. Skipped
// without python3.
func TestS3FiltersMatchMatcher(t *testing.T) {
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
	}
	paths := []string{
		"index.html", "app.js", "app.js.map", "keep.map", "sub/keep.map", "sub/x.map",
		".git/config", "sub/.git/HEAD", "node_modules/a/b.js", "x/node_modules/c.js",
		".well-known/security.txt", ".well-known/acme-challenge/token", ".well-known/.htaccess",
		".well-known/sub/.hidden", ".well-known/.git/config", ".well-known/node_modules/x.js",
		".well-known/CLAUDE.md", ".well-known/x.map", "sub/.well-known/x", "sub/.well-known/.y",
		".env", "sub/.env", "a/.cache/b", "CLAUDE.md", "sub/CLAUDE.md", "AGENTS.md", ".backyard.json",
		".backyardignore", ".DS_Store", "img/.DS_Store", "drafts/a.html", "sub/drafts/a.html",
		"build/out.js", "temp1.txt", "Temp2.txt", "temp10.txt", ".claude/skills/x.md",
		".github/workflows/ci.yml", ".cursor/rules/x.mdc", ".htaccess", "sub/.htaccess", "key.pem",
		"weird (1)+[x].html",
	}
	root := "/src/site"
	type evalCase struct {
		Filters [][2]string `json:"filters"`
	}
	var in struct {
		Root  string     `json:"root"`
		Paths []string   `json:"paths"`
		Cases []evalCase `json:"cases"`
	}
	in.Root, in.Paths = root, paths
	for _, c := range cases {
		args := s3Filters(c.patterns)
		var ec evalCase
		for i := 0; i+1 < len(args); i += 2 {
			ec.Filters = append(ec.Filters, [2]string{strings.TrimPrefix(args[i], "--"), args[i+1]})
		}
		in.Cases = append(in.Cases, ec)
	}
	script := `import fnmatch,json,os,sys
d=json.load(sys.stdin)
out=[]
for c in d["cases"]:
    res=[]
    for p in d["paths"]:
        full=os.path.join(d["root"],p)
        include=True
        for typ,pat in c["filters"]:
            if fnmatch.fnmatch(full,os.path.join(d["root"],pat)):
                include=(typ=="include")
        res.append(not include)
    out.append(res)
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
	var got [][]bool
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for ci, c := range cases {
		m := deploy.NewMatcher(c.patterns)
		for i, p := range paths {
			if want := m.Ignored(p, false); got[ci][i] != want {
				t.Errorf("%s: %q: aws filters exclude=%v, deploy matcher=%v", c.name, p, got[ci][i], want)
			}
		}
	}
}
