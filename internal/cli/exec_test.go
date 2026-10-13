package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBrowserCommand(t *testing.T) {
	const u = "https://backyard.acme.com/s/x/"
	tests := []struct {
		env  string
		want []string
	}{
		{"firefox", []string{"firefox", u}},
		{"firefox --new-tab", []string{"firefox", "--new-tab", u}},
		{"chromium --app=%s", []string{"chromium", "--app=" + u}},
		{"w3m:lynx", []string{"w3m", u}},
	}
	for _, tt := range tests {
		t.Setenv("BROWSER", tt.env)
		if got := browserCommand(u).Argv(); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("BROWSER=%q: argv = %q, want %q", tt.env, got, tt.want)
		}
	}
	t.Setenv("BROWSER", "")
	if got := browserCommand(u).Argv(); got[len(got)-1] != u {
		t.Errorf("default argv = %q", got)
	}
}

// TestOpenInBrowserLeavesBrowserRunning checks that a launcher still running
// after the grace period (a browser started in the foreground) is neither
// waited for nor killed, and that a launcher failing fast is reported.
func TestOpenInBrowserLeavesBrowserRunning(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script")
	}
	old := browserGrace
	browserGrace = 100 * time.Millisecond
	t.Cleanup(func() { browserGrace = old })
	dir := t.TempDir()
	marker := filepath.Join(dir, "opened")
	script := filepath.Join(dir, "browser.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 0.5\necho \"$1\" > "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", script)
	start := time.Now()
	if err := openInBrowser(t.Context(), "http://127.0.0.1:9/callback?x=1"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 400*time.Millisecond {
		t.Fatal("openInBrowser waited for the browser to exit")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(marker)
		if err == nil && strings.TrimSpace(string(b)) == "http://127.0.0.1:9/callback?x=1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the browser process was killed or never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}

	failing := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BROWSER", failing)
	if err := openInBrowser(t.Context(), "https://example.com/"); err == nil {
		t.Fatal("a failing launcher was not reported")
	}
	for _, bad := range []string{"file:///etc/passwd", "javascript:alert(1)", "https://"} {
		if err := openInBrowser(t.Context(), bad); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
}
