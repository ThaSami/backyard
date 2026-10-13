// Command mkzip writes a zip archive of the given files, each stored at the
// archive root under its base name. It replaces the `zip` tool in `make
// lambda` on machines that do not have it:
//
//	go run ./scripts/mkzip -o dist/backyard-lambda.zip -mode 0755 dist/lambda/bootstrap
//
// The archive is reproducible: entries are written in argument order with a
// fixed modification time (SOURCE_DATE_EPOCH when set, else 1980-01-01) and
// Unix permissions taken from disk or forced with -mode (AWS Lambda requires
// "bootstrap" to be executable, which a build on Windows does not record). The output is written to a temporary file and
// renamed into place, so a failed run never leaves a truncated archive.
package main

import (
	"archive/zip"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// defaultModTime is the earliest time the zip format can represent (DOS epoch).
var defaultModTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mkzip:", err)
		os.Exit(1)
	}
}

// run parses arguments and writes the archive.
func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("mkzip", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "output zip file (required)")
	modeFlag := fs.String("mode", "", "octal Unix permissions for every entry (default: from disk)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: mkzip -o archive.zip [-mode 0755] file...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" || fs.NArg() == 0 {
		fs.Usage()
		return errors.New("an output file (-o) and at least one input file are required")
	}
	mtime, err := modTime(os.Getenv("SOURCE_DATE_EPOCH"))
	if err != nil {
		return err
	}
	perm, err := parseMode(*modeFlag)
	if err != nil {
		return err
	}
	return writeZip(*out, fs.Args(), mtime, perm)
}

// parseMode parses an octal permission string; "" means "keep the file's
// own permissions" and returns 0.
func parseMode(s string) (os.FileMode, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v == 0 || v > 0o777 {
		return 0, fmt.Errorf("invalid -mode %q: want octal permissions such as 0755", s)
	}
	return os.FileMode(v), nil
}

// modTime returns the timestamp for archive entries: SOURCE_DATE_EPOCH
// (seconds since the Unix epoch) when set, else the DOS epoch.
func modTime(epoch string) (time.Time, error) {
	epoch = strings.TrimSpace(epoch)
	if epoch == "" {
		return defaultModTime, nil
	}
	secs, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil || secs < 0 {
		return time.Time{}, fmt.Errorf("invalid SOURCE_DATE_EPOCH %q", epoch)
	}
	t := time.Unix(secs, 0).UTC()
	if t.Before(defaultModTime) {
		t = defaultModTime
	}
	return t, nil
}

// writeZip creates out containing files (stored by base name). A non-zero
// perm overrides the permissions recorded for every entry.
func writeZip(out string, files []string, mtime time.Time, perm os.FileMode) (err error) {
	seen := make(map[string]string, len(files))
	for _, f := range files {
		name := filepath.Base(f)
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("%s and %s would both be stored as %q", prev, f, name)
		}
		seen[name] = f
	}
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".mkzip-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	zw := zip.NewWriter(tmp)
	for _, f := range files {
		if err := addFile(zw, f, mtime, perm); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("finish archive: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("chmod archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return fmt.Errorf("move archive into place: %w", err)
	}
	return nil
}

// addFile appends one regular file to the archive.
func addFile(zw *zip.Writer, path string, mtime time.Time, perm os.FileMode) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	hdr, err := zip.FileInfoHeader(fi)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	hdr.Name = filepath.Base(path)
	hdr.Method = zip.Deflate
	hdr.Modified = mtime
	if perm == 0 {
		perm = fi.Mode().Perm()
	}
	hdr.SetMode(perm)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if _, err := io.Copy(w, f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
