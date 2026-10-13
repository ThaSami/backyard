package deploy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestMatcher(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		path     string
		isDir    bool
		want     bool
	}{
		// Basics.
		{"no patterns", nil, "a.txt", false, false},
		{"exact name", []string{"a.txt"}, "a.txt", false, true},
		{"name at depth", []string{"a.txt"}, "x/y/a.txt", false, true},
		{"glob star", []string{"*.log"}, "logs/debug.log", false, true},
		{"glob no match", []string{"*.log"}, "debug.txt", false, false},
		{"question mark", []string{"?.js"}, "a.js", false, true},
		{"question mark long", []string{"?.js"}, "ab.js", false, false},
		{"character class", []string{"[ab].txt"}, "b.txt", false, true},
		{"negated class", []string{"[!ab].txt"}, "c.txt", false, true},
		{"negated class excludes", []string{"[!ab].txt"}, "a.txt", false, false},
		{"caret class", []string{"[^ab].txt"}, "c.txt", false, true},
		{"escaped bracket bang", []string{`\[!a].txt`}, "[!a].txt", false, true},
		{"star does not cross /", []string{"a*b"}, "a/b", false, false},
		// Comments and blank lines.
		{"comment", []string{"# a.txt"}, "# a.txt", false, false},
		{"escaped hash", []string{`\#a.txt`}, "#a.txt", false, true},
		{"blank", []string{"", "   "}, "a", false, false},
		{"trailing spaces trimmed", []string{"a.txt   "}, "a.txt", false, true},
		{"escaped trailing space", []string{`a\ `}, "a ", false, true},
		{"crlf", []string{"a.txt\r"}, "a.txt", false, true},
		// Negation.
		{"negation", []string{"*.log", "!keep.log"}, "keep.log", false, false},
		{"negation others", []string{"*.log", "!keep.log"}, "drop.log", false, true},
		{"last match wins", []string{"!a", "a"}, "a", false, true},
		{"escaped bang", []string{`\!a`}, "!a", false, true},
		{"cannot reinclude under excluded dir", []string{"dir/", "!dir/keep"}, "dir/keep", false, true},
		{"reinclude with dir/**", []string{"dir/**", "!dir/keep"}, "dir/keep", false, false},
		{"dir/** excludes contents", []string{"dir/**"}, "dir/x/y", false, true},
		{"dir/** keeps dir itself", []string{"dir/**"}, "dir", true, false},
		// Directory-only patterns.
		{"dir only matches dir", []string{"build/"}, "build", true, true},
		{"dir only not file", []string{"build/"}, "build", false, false},
		{"dir only contents", []string{"build/"}, "build/out.js", false, true},
		{"dir only nested", []string{"build/"}, "src/build/x", false, true},
		{"dir pattern without slash", []string{"node_modules"}, "a/node_modules/x/y.js", false, true},
		// Anchoring.
		{"leading slash anchors", []string{"/a.txt"}, "a.txt", false, true},
		{"leading slash not deep", []string{"/a.txt"}, "x/a.txt", false, false},
		{"inner slash anchors", []string{"doc/*.txt"}, "doc/a.txt", false, true},
		{"inner slash not nested", []string{"doc/*.txt"}, "doc/sub/a.txt", false, false},
		{"inner slash not deep", []string{"doc/*.txt"}, "x/doc/a.txt", false, false},
		// Double star.
		{"leading **", []string{"**/foo"}, "foo", false, true},
		{"leading ** deep", []string{"**/foo"}, "a/b/foo", false, true},
		{"leading ** with path", []string{"**/foo/bar"}, "x/foo/bar", false, true},
		{"middle **", []string{"a/**/b"}, "a/b", false, true},
		{"middle ** deep", []string{"a/**/b"}, "a/x/y/b", false, true},
		{"middle ** wrong root", []string{"a/**/b"}, "z/a/x/b", false, false},
		{"trailing ** matches files", []string{"a/**"}, "a/b.txt", false, true},
		{"lone **", []string{"**"}, "x/y", false, true},
		{"embedded ** acts like *", []string{"a**b"}, "axxb", false, true},
		// Invalid patterns are ignored.
		{"bad glob", []string{"[", "a"}, "[", false, false},
		{"bad glob keeps others", []string{"[", "a"}, "a", false, true},
		{"only slash", []string{"/"}, "a", false, false},
		// Paths are normalized.
		{"leading slash in path", []string{"a"}, "/a", false, true},
		{"empty path", []string{"*"}, "", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Ignored(tc.path, tc.isDir, tc.patterns); got != tc.want {
				t.Errorf("Ignored(%q, %v, %q) = %v, want %v", tc.path, tc.isDir, tc.patterns, got, tc.want)
			}
		})
	}
}

func TestDefaultIgnore(t *testing.T) {
	d := DefaultIgnore()
	tests := []struct {
		path  string
		isDir bool
		want  bool
	}{
		{".git", true, true},
		{".git/config", false, true},
		{".GIT/config", false, true},
		{"sub/.Git", true, true},
		{"mod/.git", false, true},
		{".backyard.json", false, true},
		{".Backyard.JSON", false, true},
		{".backyardignore", false, true},
		{"sub/.BACKYARDIGNORE", false, true},
		{".well-known/.git/config", false, true},
		{".well-known/.backyard.json", false, true},
		{"node_modules", true, true},
		{"web/node_modules/x.js", false, true},
		{".claude/settings.json", false, true},
		{".cursor", true, true},
		{".github/workflows/ci.yml", false, true},
		{"CLAUDE.md", false, true},
		{"docs/AGENTS.md", false, true},
		{".DS_Store", false, true},
		{"img/.DS_Store", false, true},
		{".env", false, true},
		{"a/.hidden/b.txt", false, true},
		{".well-known", true, false},
		{".well-known/security.txt", false, false},
		{".well-known/.secret", false, true},
		{"index.html", false, false},
		{"README.md", false, false},
		{"claude.md", false, false},
		{"a.b/c.d", false, false},
	}
	for _, tc := range tests {
		if got := Ignored(tc.path, tc.isDir, d); got != tc.want {
			t.Errorf("default Ignored(%q, %v) = %v, want %v", tc.path, tc.isDir, got, tc.want)
		}
	}
	d[0] = "mutated"
	if DefaultIgnore()[0] == "mutated" {
		t.Error("DefaultIgnore returns a shared slice")
	}
}

func TestValidatePath(t *testing.T) {
	good := []string{"index.html", "a/b/c.txt", "with space.txt", "ü/日本.html", ".well-known/x", "a..b", "...", strings.Repeat("a", 1024), "__backyardx/a", "x/__backyard/y"}
	for _, p := range good {
		if why := ValidatePath(p); why != "" {
			t.Errorf("ValidatePath(%q) = %q, want valid", p, why)
		}
	}
	bad := []string{"", "/a", "a/", "a//b", "./a", "a/.", "../a", "a/../b", "..", "__backyard", "__backyard/x", `a\b`, "a\x00b", "a\nb", "a\x7fb", "a\xffb", strings.Repeat("a", 1025)}
	for _, p := range bad {
		if why := ValidatePath(p); why == "" {
			t.Errorf("ValidatePath(%q) = valid, want error", p)
		}
		if why := ValidatePathSyntax(p); why == "" {
			t.Errorf("ValidatePathSyntax(%q) = valid, want error", p)
		}
	}
	// No ".git" segment in any letter case (defense in depth: deploys never
	// publish one and gateway reads never serve one); the syntax alone
	// accepts them, and names merely containing ".git" stay valid.
	for _, p := range []string{".git", ".git/config", ".GIT/HEAD", "a/.Git/objects/x", "sub/.gIT", ".well-known/.git/config"} {
		if why := ValidatePath(p); why != `path may not contain a ".git" segment` {
			t.Errorf("ValidatePath(%q) = %q, want the .git rule", p, why)
		}
		if why := ValidatePathSyntax(p); why != "" {
			t.Errorf("ValidatePathSyntax(%q) = %q, want valid", p, why)
		}
		if !HasGitSegment(p) {
			t.Errorf("HasGitSegment(%q) = false", p)
		}
	}
	for _, p := range []string{".gitignore", ".github/x", "a.git", ".git.html", "git/x", "x/.gitkeep", "..git"} {
		if why := ValidatePath(p); why != "" {
			t.Errorf("ValidatePath(%q) = %q, want valid", p, why)
		}
		if HasGitSegment(p) {
			t.Errorf("HasGitSegment(%q) = true", p)
		}
	}
}

// TestProtectedIgnoreCannotBeReincluded: "!" lines in .backyardignore or the
// .backyard.json "ignore" list may re-include the overridable defaults
// (dotfiles, node_modules/, agent files…) but never .git/ or the project
// files, in any letter case and at any depth.
func TestProtectedIgnoreCannotBeReincluded(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":               "x",
		".git/config":              "[core]",
		".git/HEAD":                "ref",
		".GIT/config":              "[core]",
		"sub/.Git/HEAD":            "ref",
		"mod/.git":                 "gitdir: ../.git/modules/mod",
		"mod/index.html":           "x",
		".backyard.json":           `{"site":"x"}`,
		".Backyard.json":           `{"site":"x"}`,
		"sub/.backyard.json":       `{"site":"x"}`,
		"sub/.backyardignore":      "!.*",
		".env":                     "SECRET=1",
		".htaccess":                "x",
		"node_modules/a.js":        "x",
		"CLAUDE.md":                "x",
		".github/workflows/ci.yml": "x",
		".well-known/.git/config":  "[core]",
		".well-known/security.txt": "x",
		IgnoreFile: "!.*\n!.git/\n!.git\n!.git/**\n!**/.git/**\n!/.git/config\n!.GIT/\n!.backyard.json\n!.Backyard.json\n" +
			"!.backyardignore\n!*\n!**\n!node_modules/\n!CLAUDE.md\n!.github/\n",
	})
	files, err := CollectFiles(dir, []string{"!.git/", "!sub/.Git/HEAD", "!**/.backyard.json"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{".env", ".github/workflows/ci.yml", ".htaccess", ".well-known/security.txt", "CLAUDE.md", "index.html", "mod/index.html", "node_modules/a.js"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %q\nwant    %q", paths, want)
	}
	pats, err := IgnorePatterns(dir, []string{"extra"})
	if err != nil {
		t.Fatal(err)
	}
	if prot := ProtectedIgnore(); !slices.Equal(pats[len(pats)-len(prot):], prot) || pats[len(pats)-len(prot)-1] != "extra" {
		t.Errorf("IgnorePatterns = %q, want the configured patterns followed by ProtectedIgnore", pats)
	}
	if got := ProtectedIgnore(); !slices.Equal(got, []string{".[gG][iI][tT]", ".[bB][aA][cC][kK][yY][aA][rR][dD].[jJ][sS][oO][nN]",
		".[bB][aA][cC][kK][yY][aA][rR][dD][iI][gG][nN][oO][rR][eE]"}) {
		t.Errorf("ProtectedIgnore = %q", got)
	}
	// Overridable defaults stay overridable one by one.
	for _, tc := range []struct {
		neg, path string
	}{{"!.env", ".env"}, {"!node_modules/", "node_modules/x.js"}, {"!CLAUDE.md", "CLAUDE.md"}, {"!AGENTS.md", "docs/AGENTS.md"},
		{"!.claude/", ".claude/skills/x.md"}, {"!.DS_Store", ".DS_Store"}} {
		if NewMatcher(WithDefaultIgnore(nil)).Ignored(tc.path, false) != true {
			t.Errorf("%s is not ignored by default", tc.path)
		}
		if NewMatcher(WithDefaultIgnore([]string{tc.neg})).Ignored(tc.path, false) {
			t.Errorf("%q does not re-include %s", tc.neg, tc.path)
		}
	}
	if !slices.Equal(DefaultIgnore(), WithDefaultIgnore(nil)) {
		t.Error("DefaultIgnore differs from WithDefaultIgnore(nil)")
	}
}

func TestCollectFiles(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":           "<html></html>",
		"b/z.css":              "body{}",
		"b/a.js":               "1",
		"noext":                "\x89PNG\r\n\x1a\n rest",
		".hidden":              "x",
		"node_modules/m/i.js":  "x",
		"CLAUDE.md":            "x",
		".well-known/ai.txt":   "x",
		"skip/me.txt":          "x",
		IgnoreFile:             "skip/\n",
		"empty.txt":            "",
		"deep/er/still/ok.txt": "ok",
	})
	files, err := CollectFiles(dir, []string{"b/z.css"})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
		if !filepath.IsAbs(f.Abs) {
			t.Errorf("%s: Abs %q is not absolute", f.Path, f.Abs)
		}
		data, err := os.ReadFile(f.Abs)
		if err != nil {
			t.Fatal(err)
		}
		if f.Size != int64(len(data)) || f.SHA256 != sha256Hex(data) {
			t.Errorf("%s: size/sha = %d/%s", f.Path, f.Size, f.SHA256)
		}
	}
	want := []string{".well-known/ai.txt", "b/a.js", "deep/er/still/ok.txt", "empty.txt", "index.html", "noext"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
	types := map[string]string{}
	for _, f := range files {
		types[f.Path] = f.ContentType
	}
	if types["index.html"] != "text/html; charset=utf-8" || types["b/a.js"] != "text/javascript; charset=utf-8" || types["noext"] != "image/png" {
		t.Errorf("content types = %v", types)
	}

	// IgnorePatterns exposes the same list.
	pats, err := IgnorePatterns(dir, []string{"extra"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pats, "skip/") || pats[len(pats)-len(ProtectedIgnore())-1] != "extra" || !slices.Contains(pats, ".*") {
		t.Errorf("IgnorePatterns = %v", pats)
	}
}

// A .backyardignore saved with a UTF-8 byte order mark (PowerShell 5's
// Out-File -Encoding utf8, older Notepad) keeps its first rule, as in git.
func TestIgnoreFileWithBOM(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"index.html":      "<html></html>",
		"secrets/key.txt": "hunter2",
		"notes.md":        "x",
		IgnoreFile:        "\uFEFFsecrets/\r\nnotes.md\r\n",
	})
	files, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"index.html"}) {
		t.Errorf("paths = %v, want only index.html", paths)
	}
	pats, err := IgnorePatterns(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pats, "secrets/") {
		t.Errorf("IgnorePatterns = %q, want the BOM stripped from the first rule", pats)
	}
	// Only a leading BOM is a byte order mark.
	writeTree(t, dir, map[string]string{IgnoreFile: "a\n\uFEFFb\n"})
	if pats, _ := IgnorePatterns(dir, nil); !slices.Contains(pats, "\uFEFFb") {
		t.Errorf("IgnorePatterns = %q, want a later U+FEFF kept", pats)
	}
}

func TestCollectFilesErrors(t *testing.T) {
	if _, err := CollectFiles(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Error("missing dir accepted")
	}
	f := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CollectFiles(f, nil); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file as dir: %v", err)
	}
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"__backyard/x.js": "x"})
	if _, err := CollectFiles(dir, nil); err == nil || !strings.Contains(err.Error(), "__backyard") {
		t.Errorf("reserved path: %v", err)
	}
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{`back\slash.txt`: "x"})
		if _, err := CollectFiles(dir, nil); err == nil {
			t.Error("backslash file name accepted")
		}
		// …unless ignored.
		if _, err := CollectFiles(dir, []string{`back\\slash.txt`}); err != nil {
			t.Errorf("ignored backslash name: %v", err)
		}
	}
}

func TestCollectFilesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	outside := t.TempDir()
	writeTree(t, outside, map[string]string{"secret.txt": "top secret", "dir/x.txt": "x"})
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"real/a.txt": "a", "index.html": "i"})
	links := map[string]string{
		"link-to-file.txt": filepath.Join(dir, "real", "a.txt"),  // followed
		"relative.txt":     "real/a.txt",                         // followed
		"escape.txt":       filepath.Join(outside, "secret.txt"), // skipped: outside
		"escape-rel.txt":   "../" + filepath.Base(outside) + "/secret.txt",
		"linked-dir":       filepath.Join(dir, "real"),           // skipped: directory
		"outside-dir":      filepath.Join(outside, "dir"),        // skipped
		"broken.txt":       filepath.Join(dir, "does-not-exist"), // skipped
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	files, err := CollectFiles(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
		if f.Path == "link-to-file.txt" && (f.Size != 1 || f.SHA256 != sha256Hex([]byte("a"))) {
			t.Errorf("symlinked file not followed: %+v", f)
		}
	}
	want := []string{"index.html", "link-to-file.txt", "real/a.txt", "relative.txt"}
	if !slices.Equal(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}

	// The root itself may be a symlink.
	root := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(dir, root); err != nil {
		t.Fatal(err)
	}
	files2, err := CollectFiles(root, nil)
	if err != nil || len(files2) != len(files) {
		t.Errorf("CollectFiles(symlinked root) = %d files, %v", len(files2), err)
	}
}

// onlyReader hides io.WriterTo and friends.
type onlyReader struct{ r io.Reader }

func (o onlyReader) Read(p []byte) (int, error) { return o.r.Read(p) }

func TestVerifyingReader(t *testing.T) {
	data := []byte("hello, verifying reader")
	sum := sha256Hex(data)
	tests := []struct {
		name    string
		body    io.Reader
		size    int64
		sha     string
		wantErr error
		readN   int64 // -1: read to EOF; else io.CopyN like a store with a known size
	}{
		{"exact", bytes.NewReader(data), int64(len(data)), sum, nil, -1},
		{"exact known size", bytes.NewReader(data), int64(len(data)), sum, nil, int64(len(data))},
		{"one byte at a time", iotest.OneByteReader(bytes.NewReader(data)), int64(len(data)), sum, nil, -1},
		{"data with EOF", iotest.DataErrReader(bytes.NewReader(data)), int64(len(data)), sum, nil, -1},
		{"wrong digest", bytes.NewReader(bytes.ToUpper(data)), int64(len(data)), sum, errUploadMismatch, -1},
		{"wrong digest known size", bytes.NewReader(bytes.ToUpper(data)), int64(len(data)), sum, errUploadMismatch, int64(len(data))},
		{"short", bytes.NewReader(data[:5]), int64(len(data)), sum, errUploadMismatch, -1},
		{"long", bytes.NewReader(append(data, '!')), int64(len(data)), sum, errUploadTooLarge, -1},
		{"long tiny reads", iotest.OneByteReader(bytes.NewReader(append(data, '!'))), int64(len(data)), sum, errUploadTooLarge, -1},
		{"empty", bytes.NewReader(nil), 0, sha256Hex(nil), nil, -1},
		{"empty but data", bytes.NewReader([]byte("x")), 0, sha256Hex(nil), errUploadTooLarge, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := newVerifyingReader(tc.body, tc.size, tc.sha)
			var err error
			if tc.readN >= 0 {
				_, err = io.CopyN(io.Discard, onlyReader{v}, tc.readN)
			} else {
				_, err = io.Copy(io.Discard, onlyReader{v})
			}
			if tc.wantErr == nil {
				if err != nil || !v.ok {
					t.Errorf("err = %v, ok = %v", err, v.ok)
				}
				return
			}
			// io.CopyN drops a reader error that arrives with the last
			// byte; the verifier's own state must still record it (the
			// upload handler relies on that to delete such objects).
			if !errors.Is(v.err, tc.wantErr) || v.ok {
				t.Errorf("verifier err = %v, ok = %v; want %v", v.err, v.ok, tc.wantErr)
			}
			if tc.readN < 0 && !errors.Is(err, tc.wantErr) {
				t.Errorf("read err = %v, want %v", err, tc.wantErr)
			}
		})
	}
	// finish verifies a body the store never read.
	v := newVerifyingReader(bytes.NewReader(nil), 0, sha256Hex(nil))
	if err := v.finish(); err != nil || !v.ok {
		t.Errorf("finish on empty body: %v, ok=%v", err, v.ok)
	}
}
