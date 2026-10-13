package scaffold

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/thasami/backyard/templates"
)

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		rel     string
		wantErr string
	}{
		{rel: "index.html"},
		{rel: "a/b/c.txt"},
		{rel: "dir/x.txt"},
		{rel: ".claude/skills/backyard/SKILL.md"},
		{rel: "a/./b.txt"},
		{rel: "", wantErr: "empty"},
		{rel: "/etc/passwd", wantErr: "absolute"},
		{rel: "../x", wantErr: "outside"},
		{rel: "a/../../x", wantErr: "outside"},
		{rel: "a/..", wantErr: "outside"},
		{rel: `a\b`, wantErr: `"\"`},
		{rel: "a\x00b", wantErr: "NUL"},
		{rel: "file/x.txt", wantErr: "not a directory"},
		{rel: "dir", wantErr: "is a directory"},
		{rel: ".", wantErr: "outside"},
	}
	for _, tt := range tests {
		got, err := SafeJoin(root, tt.rel)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("SafeJoin(%q) = %q, %v; want error %q", tt.rel, got, err, tt.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("SafeJoin(%q): %v", tt.rel, err)
			continue
		}
		if !strings.HasPrefix(got, root+string(filepath.Separator)) {
			t.Errorf("SafeJoin(%q) = %q outside %q", tt.rel, got, root)
		}
	}
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "f"), filepath.Join(root, "flink")); err != nil {
			t.Fatal(err)
		}
		for _, rel := range []string{"link/x", "link", "flink"} {
			if _, err := SafeJoin(root, rel); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Errorf("SafeJoin(%q) err = %v", rel, err)
			}
		}
	}
}

func TestParseAgents(t *testing.T) {
	got, err := ParseAgents([]string{"Claude, cursor", "copilot,claude", ""})
	if err != nil || !slices.Equal(got, []string{"claude", "cursor", "copilot"}) {
		t.Fatalf("ParseAgents = %v, %v", got, err)
	}
	if got, err := ParseAgents(nil); err != nil || got != nil {
		t.Fatalf("empty = %v %v", got, err)
	}
	if _, err := ParseAgents([]string{"vim"}); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestInitAndWrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new")
	var seen templates.InitOptions
	render := func(o templates.InitOptions) (map[string][]byte, error) {
		seen = o
		files := map[string][]byte{"a.txt": []byte("a"), "sub/b.txt": []byte("b")}
		if !o.SkipIndex {
			files["index.html"] = []byte("i")
		}
		return files, nil
	}
	res, err := Init(Options{Dir: dir, Site: "my-site", Server: "https://b.example.com", Agents: []string{"codex"}, Template: "app", Render: render})
	if err != nil {
		t.Fatal(err)
	}
	if seen.Site != "my-site" || seen.Server != "https://b.example.com" || seen.Template != "app" || !slices.Equal(seen.Agents, []string{"codex"}) || seen.SkipIndex {
		t.Fatalf("render options = %+v", seen)
	}
	if !slices.Equal(res.Created, []string{"a.txt", "index.html", "sub/b.txt"}) || res.Dir != dir || res.Site != "my-site" {
		t.Fatalf("result = %+v", res)
	}
	// Existing project: index.html is neither rendered nor overwritten.
	res, err = Init(Options{Dir: dir, Site: "my-site", Render: render})
	if err != nil {
		t.Fatal(err)
	}
	if !seen.SkipIndex || len(res.Created) != 0 || !slices.Equal(res.Skipped, []string{"a.txt", "index.html", "sub/b.txt"}) {
		t.Fatalf("second init = %+v (skip=%v)", res, seen.SkipIndex)
	}
	res, err = Init(Options{Dir: dir, Site: "my-site", Render: render, Force: true})
	if err != nil || len(res.Overwritten) != 3 {
		t.Fatalf("force = %+v %v", res, err)
	}
	for _, bad := range []string{"", "Bad", "www", "-x"} {
		if _, err := Init(Options{Dir: dir, Site: bad, Render: render}); err == nil {
			t.Errorf("site %q accepted", bad)
		}
	}
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o644)
	if _, err := Init(Options{Dir: f, Site: "ok", Render: render}); err == nil {
		t.Fatal("file as dir accepted")
	}
}
