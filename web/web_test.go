package web

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSDKFiles(t *testing.T) {
	tests := []struct {
		name string
		got  []byte
		file string
		want []string
	}{
		{"classic", SDKClassic(), "sdk/backyard.js", []string{"function createBackyard(g)", "g.backyard = sdk;", `Symbol.for("backyard.sdk")`}},
		{"module", SDKModule(), "sdk/backyard.mjs", []string{"function createBackyard(g)", "export default backyard;", "export const { db, files, ai, warehouse, socket, id, site, me, configure, ready, BackyardError } = backyard;"}},
		{"types", SDKTypes(), "sdk/backyard.d.ts", []string{"declare var backyard: Backyard.SDK;", `declare module "*/__backyard/backyard.mjs"`, "interface Collection"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) == 0 {
				t.Fatal("empty")
			}
			for _, w := range tc.want {
				if !bytes.Contains(tc.got, []byte(w)) {
					t.Errorf("missing %q", w)
				}
			}
			// The server prepends the per-request preamble: the files must not carry one.
			if tc.name != "types" && !bytes.HasPrefix(tc.got, []byte("/*! Backyard client SDK v")) {
				t.Error("embedded SDK does not start with the build banner")
			}
			disk, err := os.ReadFile(filepath.FromSlash(tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(disk, tc.got) {
				t.Error("embedded bytes differ from the file on disk")
			}
		})
	}
}

// TestSDKBuildsInSync checks that the generated builds embed the current
// source of truth (node web/sdk/build.mjs regenerates them).
func TestSDKBuildsInSync(t *testing.T) {
	core, err := os.ReadFile(filepath.Join("sdk", "src", "core.js"))
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{"backyard.js": SDKClassic(), "backyard.mjs": SDKModule()} {
		if !bytes.Contains(b, core) {
			t.Errorf("%s is stale: run `node web/sdk/build.mjs`", name)
		}
	}
}

func TestHub(t *testing.T) {
	hub := Hub()
	for _, name := range []string{"index.html", "docs.html", "hub.css", "hub.js", "docs.js", "favicon.svg"} {
		b, err := fs.ReadFile(hub, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(b) == 0 {
			t.Errorf("%s is empty", name)
		}
	}
	if _, err := fs.Stat(hub, "hub"); err == nil {
		t.Error("Hub() is not rooted at web/hub")
	}

	// Pages reference only local assets that exist (no CDNs).
	ref := regexp.MustCompile(`(?:src|href)="([^"]+)"`)
	for _, page := range []string{"index.html", "docs.html"} {
		b, _ := fs.ReadFile(hub, page)
		for _, m := range ref.FindAllStringSubmatch(string(b), -1) {
			u := m[1]
			switch {
			case strings.HasPrefix(u, "https://github.com/"), strings.HasPrefix(u, "#"):
				continue
			case strings.Contains(u, "://"), strings.HasPrefix(u, "//"):
				t.Errorf("%s references an external resource %q", page, u)
			case strings.HasPrefix(u, "/") && (strings.HasSuffix(u, ".css") || strings.HasSuffix(u, ".js") || strings.HasSuffix(u, ".svg")):
				if _, err := fs.Stat(hub, strings.TrimPrefix(u, "/")); err != nil {
					t.Errorf("%s references missing asset %q", page, u)
				}
			}
		}
		if bytes.Contains(b, []byte("<script>")) {
			t.Errorf("%s has an inline script (keep pages CSP-friendly)", page)
		}
	}

	// Nothing but the UI is shipped (tests live outside web/hub).
	_ = fs.WalkDir(hub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(p, "test") {
			t.Errorf("unexpected file in the hub: %s", p)
		}
		return nil
	})
}
