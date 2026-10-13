package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	// WriteFile applies the umask; force the exact permissions.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func readZip(t *testing.T, path string) map[string]*zip.File {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { zr.Close() })
	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}
	return files
}

func contents(t *testing.T, f *zip.File) string {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunWritesExecutableEntries(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "build", "bootstrap")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, bin, "#!/bin/sh\necho hi\n", 0o644)
	readme := filepath.Join(dir, "README.txt")
	writeFile(t, readme, "hello", 0o600)
	out := filepath.Join(dir, "dist", "lambda.zip")

	t.Setenv("SOURCE_DATE_EPOCH", "")
	if err := run([]string{"-o", out, "-mode", "0755", bin}, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	files := readZip(t, out)
	f, ok := files["bootstrap"]
	if !ok || len(files) != 1 {
		t.Fatalf("archive entries = %v, want only bootstrap at the root", files)
	}
	if got := f.Mode().Perm(); got != 0o755 {
		t.Errorf("bootstrap mode = %o, want 755", got)
	}
	if !f.Mode().IsRegular() {
		t.Errorf("bootstrap is not recorded as a regular file: %v", f.Mode())
	}
	if !f.Modified.Equal(defaultModTime) {
		t.Errorf("modified = %v, want %v", f.Modified, defaultModTime)
	}
	if got := contents(t, f); got != "#!/bin/sh\necho hi\n" {
		t.Errorf("contents = %q", got)
	}

	// Without -mode, permissions come from disk.
	out2 := filepath.Join(dir, "dist", "two.zip")
	if err := run([]string{"-o", out2, bin, readme}, io.Discard); err != nil {
		t.Fatalf("run: %v", err)
	}
	files = readZip(t, out2)
	if len(files) != 2 {
		t.Fatalf("entries = %d, want 2", len(files))
	}
	if runtime.GOOS != "windows" {
		if got := files["README.txt"].Mode().Perm(); got != 0o600 {
			t.Errorf("README.txt mode = %o, want 600", got)
		}
	}
}

func TestRunIsReproducible(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "bootstrap")
	writeFile(t, in, strings.Repeat("backyard ", 1000), 0o755)
	t.Setenv("SOURCE_DATE_EPOCH", "1700000000")
	a, b := filepath.Join(dir, "a.zip"), filepath.Join(dir, "b.zip")
	for _, out := range []string{a, b} {
		if err := run([]string{"-o", out, in}, io.Discard); err != nil {
			t.Fatalf("run: %v", err)
		}
		// Touch the input between runs: its mtime must not leak into the archive.
		now := time.Now().Add(time.Hour)
		if err := os.Chtimes(in, now, now); err != nil {
			t.Fatal(err)
		}
	}
	ab, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bb, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ab, bb) {
		t.Error("two runs over the same input produced different archives")
	}
	f := readZip(t, a)["bootstrap"]
	if want := time.Unix(1700000000, 0).UTC(); !f.Modified.Equal(want) {
		t.Errorf("modified = %v, want %v", f.Modified, want)
	}
}

func TestRunErrors(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "bootstrap")
	writeFile(t, in, "x", 0o755)
	other := filepath.Join(dir, "sub")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	dup := filepath.Join(other, "bootstrap")
	writeFile(t, dup, "y", 0o755)
	out := filepath.Join(dir, "out.zip")

	tests := []struct {
		name    string
		args    []string
		epoch   string
		wantErr string
	}{
		{"no output", []string{in}, "", "required"},
		{"no inputs", []string{"-o", out}, "", "required"},
		{"missing input", []string{"-o", out, filepath.Join(dir, "nope")}, "", "no such file"},
		{"directory input", []string{"-o", out, other}, "", "not a regular file"},
		{"duplicate names", []string{"-o", out, in, dup}, "", "both be stored"},
		{"bad mode", []string{"-o", out, "-mode", "999", in}, "", "invalid -mode"},
		{"zero mode", []string{"-o", out, "-mode", "0", in}, "", "invalid -mode"},
		{"bad epoch", []string{"-o", out, in}, "yesterday", "SOURCE_DATE_EPOCH"},
		{"negative epoch", []string{"-o", out, in}, "-5", "SOURCE_DATE_EPOCH"},
		{"unknown flag", []string{"-x", "-o", out, in}, "", "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SOURCE_DATE_EPOCH", tt.epoch)
			err := run(tt.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Errorf("a failed run left %s behind", out)
			}
			leftovers, _ := filepath.Glob(filepath.Join(dir, ".mkzip-*"))
			if len(leftovers) > 0 {
				t.Errorf("temporary files left behind: %v", leftovers)
			}
		})
	}
}

func TestModTime(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
		ok   bool
	}{
		{"", defaultModTime, true},
		{"  ", defaultModTime, true},
		{"0", defaultModTime, true}, // before the DOS epoch: clamped
		{"1700000000", time.Unix(1700000000, 0).UTC(), true},
		{"1.5", time.Time{}, false},
		{"abc", time.Time{}, false},
	}
	for _, tt := range tests {
		got, err := modTime(tt.in)
		if (err == nil) != tt.ok {
			t.Errorf("modTime(%q) err = %v, want ok=%v", tt.in, err, tt.ok)
			continue
		}
		if tt.ok && !got.Equal(tt.want) {
			t.Errorf("modTime(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
