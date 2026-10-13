// Package examples_test checks the example sites in this folder: each one
// must be a deployable Backyard site that loads the SDK correctly, references
// its own files relatively, loads nothing from the internet, uses only the
// SPEC §5.6 SDK surface, and has scripts that parse.
package examples_test

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const sdkScript = `<script src="/__backyard/backyard.js"></script>`

// want lists the example sites documented in README.md.
var want = []string{"ai-chat", "cursors", "file-drop", "kpi-dashboard", "live-poll", "lunch-vote"}

// exampleDirs returns the folders of this directory that contain an index.html.
func exampleDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		if _, err := os.Stat(filepath.Join(e.Name(), "index.html")); err == nil {
			dirs = append(dirs, e.Name())
		}
	}
	return dirs
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExamplesListed(t *testing.T) {
	dirs := exampleDirs(t)
	if !slices.Equal(dirs, want) {
		t.Fatalf("example folders = %v, want %v", dirs, want)
	}
	readme := read(t, "README.md")
	for _, d := range dirs {
		if !strings.Contains(readme, "("+d+"/)") {
			t.Errorf("README.md does not link %s/", d)
		}
	}
}

var (
	attrRe      = regexp.MustCompile(`(?i)\b(src|href)\s*=\s*"([^"]*)"`)
	scriptTagRe = regexp.MustCompile(`(?i)<script\b[^>]*>`)
	importRe    = regexp.MustCompile(`(?m)^\s*import\b|\bimport\s*\(|@import\b|url\(\s*['"]?(?:https?:)?//`)
	// SDK members outside the SPEC §5.6 surface (they exist in the SDK, but
	// the examples must run on any edition implementing §5.6).
	extraSurface = []string{".abort(", "retryAfter", "hasMore", "socket.cid", "socket.user", "socket.connect(", "socket.disconnect(", ".toJSON(", "backyard.version"}
	// Things that must never ship in a site.
	secretRe = regexp.MustCompile(`(?i)\b(sk-[a-z0-9]{16,}|AKIA[0-9A-Z]{16}|api[_-]?key\s*[:=]\s*["'][^"']+["']|-----BEGIN [A-Z ]*PRIVATE KEY-----)`)
)

func TestExampleSites(t *testing.T) {
	for _, dir := range exampleDirs(t) {
		t.Run(dir, func(t *testing.T) {
			html := read(t, filepath.Join(dir, "index.html"))
			for _, w := range []string{"<!doctype html>", `<html lang="`, `<meta charset="utf-8">`, `name="viewport"`, "<title>", `name="description"`, sdkScript} {
				if !strings.Contains(html, w) {
					t.Errorf("index.html lacks %q", w)
				}
			}
			// The SDK is the first script, so window.backyard exists when the app runs.
			if tags := scriptTagRe.FindAllString(html, -1); len(tags) == 0 || !strings.Contains(sdkScript, tags[0]) {
				t.Errorf("the SDK must be the first <script>; got %q", tags)
			}
			for _, m := range attrRe.FindAllStringSubmatch(html, -1) {
				ref := m[2]
				switch {
				case strings.HasPrefix(ref, "data:"), strings.HasPrefix(ref, "#"):
				case strings.HasPrefix(ref, "/__backyard/"):
				case strings.HasPrefix(ref, "/"), strings.Contains(ref, "://"):
					t.Errorf("index.html references %q: use a relative path to the site's own files and nothing external", ref)
				default:
					p := path.Clean(strings.SplitN(strings.SplitN(ref, "#", 2)[0], "?", 2)[0])
					if strings.HasPrefix(p, "..") {
						t.Errorf("index.html references %q outside the site folder", ref)
					} else if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
						t.Errorf("index.html references %q, which does not exist", ref)
					}
				}
			}
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				name := f.Name()
				if strings.HasPrefix(name, ".") {
					t.Errorf("hidden file %s would not be deployed", name)
					continue
				}
				src := read(t, filepath.Join(dir, name))
				if secretRe.MatchString(src) {
					t.Errorf("%s contains something that looks like a secret", name)
				}
				if ext := path.Ext(name); ext == ".js" || ext == ".css" {
					if importRe.MatchString(src) {
						t.Errorf("%s imports code or styles; examples are self-contained", name)
					}
				}
				if path.Ext(name) != ".js" {
					continue
				}
				for _, extra := range extraSurface {
					if strings.Contains(src, extra) {
						t.Errorf("%s uses %q, outside the SPEC §5.6 SDK surface", name, extra)
					}
				}
				if !strings.Contains(src, "window.backyard") {
					t.Errorf("%s does not handle a page opened without Backyard (no window.backyard check)", name)
				}
			}
		})
	}
}

// TestExampleScriptsParse syntax-checks every example script with node (as
// an ES module, like the pages load them), when node is installed.
func TestExampleScriptsParse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	tmp := t.TempDir()
	n := 0
	for _, dir := range exampleDirs(t) {
		matches, err := filepath.Glob(filepath.Join(dir, "*.js"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range matches {
			n++
			dst := filepath.Join(tmp, strings.ReplaceAll(f, string(filepath.Separator), "_")+".mjs")
			if err := os.WriteFile(dst, []byte(read(t, f)), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(node, "--check", dst).CombinedOutput(); err != nil {
				t.Errorf("%s does not parse as a module: %v\n%s", f, err, out)
			}
		}
	}
	if n == 0 {
		t.Fatal("no example scripts found")
	}
}
