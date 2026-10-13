package fs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/blobtest"
)

func newStore(t *testing.T, opts ...Option) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "root")
	s, err := New(dir, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, dir
}

func TestConformance(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s, _ := newStore(t)
		return s
	})
}

func TestConformanceFsync(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s, _ := newStore(t, WithFsync(true))
		return s
	})
}

func TestNewErrors(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("New(\"\") succeeded")
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(f); err == nil {
		t.Error("New(regular file) succeeded")
	}
}

func TestDescribeAndClose(t *testing.T) {
	s, dir := newStore(t)
	if d := s.Describe(); d != (blob.Description{Type: "fs"}) {
		t.Errorf("Describe = %+v", d)
	}
	if s.Dir() != dir {
		t.Errorf("Dir = %q, want %q", s.Dir(), dir)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := s.Stat(context.Background(), "x"); err == nil {
		t.Error("Stat after Close succeeded")
	}
}

func TestPresignNotSupported(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.PresignPut(ctx, "a.txt", blob.PresignPutOptions{ContentType: "text/plain", Size: 1}, time.Minute); !errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("PresignPut err = %v", err)
	}
	if _, err := s.PresignGet(ctx, "a.txt", time.Minute); !errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("PresignGet err = %v", err)
	}
}

// Files written by other tools (rsync) are visible, and their ETag tracks
// size+mtime.
func TestExternalWritesAndETag(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	p := filepath.Join(dir, "sites", "foo", "index.html")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("<h1>v1</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 6789, time.UTC)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	st1, err := s.Stat(ctx, "sites/foo/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if st1.ContentType != "text/html; charset=utf-8" || st1.Size != 11 || !st1.ModTime.Equal(mtime) {
		t.Errorf("Stat = %+v", st1)
	}
	// Same size+mtime → same ETag (stable across calls and processes).
	st2, _ := s.Stat(ctx, "sites/foo/index.html")
	if st1.ETag != st2.ETag {
		t.Errorf("ETag not stable: %q vs %q", st1.ETag, st2.ETag)
	}
	s2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if st, _ := s2.Stat(ctx, "sites/foo/index.html"); st.ETag != st1.ETag {
		t.Errorf("ETag differs across stores: %q vs %q", st.ETag, st1.ETag)
	}
	// rsync replaces the file: same size, new mtime → new ETag.
	if err := os.WriteFile(p, []byte("<h1>v2</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime2 := mtime.Add(time.Nanosecond)
	if err := os.Chtimes(p, mtime2, mtime2); err != nil {
		t.Fatal(err)
	}
	st3, _ := s.Stat(ctx, "sites/foo/index.html")
	if st3.ETag == st1.ETag {
		t.Error("ETag did not change with mtime")
	}
	res, err := s.List(ctx, "sites/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Objects[0].ETag != st3.ETag {
		t.Errorf("List = %+v", res.Objects)
	}
}

func TestPutDoesNotLeaveTempFiles(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "a/b.txt", strings.NewReader("hello"), 5, ""); err != nil {
		t.Fatal(err)
	}
	// Failed put (size mismatch) cleans up its temp file.
	if _, err := s.Put(ctx, "a/c.txt", strings.NewReader("hel"), 5, ""); err == nil {
		t.Fatal("short put succeeded")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"b.txt"}) {
		t.Errorf("directory contains %q", names)
	}
}

func TestTempFilesHiddenAndStaleOnesDeleted(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, "p", "q"), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "p", "q", tmpPrefix+"fresh")
	stale := filepath.Join(dir, "p", "q", tmpPrefix+"stale")
	for _, f := range []string{fresh, stale} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTempAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "p/q/real.txt", strings.NewReader("r"), 1, ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.List(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(res); !slices.Equal(got, []string{"p/q/real.txt"}) {
		t.Errorf("List = %q", got)
	}
	if err := s.DeletePrefix(ctx, "p/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale temp file not removed: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("in-flight temp file removed: %v", err)
	}
	// Only directories holding objects are reported as prefixes.
	res, err = s.List(ctx, "", "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prefixes) != 0 || len(res.Objects) != 0 {
		t.Errorf("List after DeletePrefix = %+v", res)
	}
}

func keys(res *blob.ListResult) []string {
	out := []string{}
	for _, o := range res.Objects {
		out = append(out, o.Key)
	}
	return out
}

func TestDeletePrunesEmptyDirectories(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	for _, k := range []string{"x/y/z/1.txt", "x/keep.txt"} {
		if _, err := s.Put(ctx, k, strings.NewReader("1"), 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(ctx, "x/y/z/1.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x", "y")); !os.IsNotExist(err) {
		t.Errorf("empty directories not pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x", "keep.txt")); err != nil {
		t.Errorf("sibling removed: %v", err)
	}
	if err := s.DeletePrefix(ctx, "x/"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Errorf("x not pruned: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("root removed: %v", err)
	}
	// Empty directories created out-of-band are not listed as prefixes.
	if err := os.MkdirAll(filepath.Join(dir, "empty", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := s.List(ctx, "", "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Prefixes) != 0 {
		t.Errorf("empty directory listed: %q", res.Prefixes)
	}
}

func TestPutConflictsWithDirectory(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.Put(ctx, "c/d/e.txt", strings.NewReader("1"), 1, ""); err != nil {
		t.Fatal(err)
	}
	// "c/d" is a directory: cannot become a file.
	if _, err := s.Put(ctx, "c/d", strings.NewReader("1"), 1, ""); err == nil {
		t.Error("Put over a directory succeeded")
	}
	// "c/d/e.txt" is a file: cannot become a directory.
	if _, err := s.Put(ctx, "c/d/e.txt/f", strings.NewReader("1"), 1, ""); err == nil {
		t.Error("Put below a file succeeded")
	}
	if got, err := readAll(s, "c/d/e.txt"); err != nil || got != "1" {
		t.Errorf("original object = %q, %v", got, err)
	}
}

func readAll(s blob.Store, key string) (string, error) {
	rc, _, err := s.Get(context.Background(), key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	return string(b), err
}

func TestPingReadOnlyRoot(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root/windows")
	}
	s, dir := newStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if err := s.Ping(context.Background()); err == nil {
		t.Error("Ping on read-only root succeeded")
	}
}

func TestPingNoLeftovers(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Errorf("Ping left %d entries", len(entries))
	}
}

func TestLinkTarget(t *testing.T) {
	for _, tc := range []struct {
		dir, target string
		want        string
		ok          bool
	}{
		{"sites/foo", "v2.html", "sites/foo/v2.html", true},
		{"sites/foo", "./v2.html", "sites/foo/v2.html", true},
		{"sites/foo", "assets/app.js", "sites/foo/assets/app.js", true},
		{".", "a.txt", "a.txt", true},
		{"sites/foo", "../bar/x", "", false},
		{"sites/foo", "a/../b", "", false},
		{"sites/foo", "/etc/passwd", "", false},
		{"sites/foo", "", "", false},
		{"sites/foo", ".", "", false},
		{"sites/foo", "a\nb", "", false},
	} {
		got, ok := linkTarget(tc.dir, tc.target)
		if got != tc.want || ok != tc.ok {
			t.Errorf("linkTarget(%q, %q) = %q, %v; want %q, %v", tc.dir, tc.target, got, ok, tc.want, tc.ok)
		}
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported: %v", err)
	}
}

// rsync -a preserves symlinks; links that resolve outside the root (or
// outside their own directory subtree) must never be followed.
func TestSymlinkEscapeProtection(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(filepath.Dir(dir), "secretdir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "x.txt"), []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file at the root (like the default sqlite database) must not be
	// reachable from a site directory either.
	if err := os.WriteFile(filepath.Join(dir, "backyard.db"), []byte("DATABASE"), 0o644); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "sites", "evil")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(site, "index.html"), []byte("<p>hi</p>"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(site, "abs.txt"))                       // absolute, outside
	symlinkOrSkip(t, "../../../secret.txt", filepath.Join(site, "rel.txt"))         // relative, outside root
	symlinkOrSkip(t, "../../backyard.db", filepath.Join(site, "db.txt"))            // relative, inside root but "up"
	symlinkOrSkip(t, outsideDir, filepath.Join(site, "absdir"))                     // directory outside
	symlinkOrSkip(t, "../../../secretdir", filepath.Join(site, "reldir"))           // directory outside (relative)
	symlinkOrSkip(t, "loop.txt", filepath.Join(site, "loop.txt"))                   // self loop
	symlinkOrSkip(t, "missing.txt", filepath.Join(site, "dangling.txt"))            // dangling
	symlinkOrSkip(t, "index.html", filepath.Join(site, "alias.html"))               // safe
	symlinkOrSkip(t, "alias.html", filepath.Join(site, "alias2.html"))              // safe chain
	symlinkOrSkip(t, "../../../secret.txt", filepath.Join(site, "sub", "deep.txt")) // escape from subdir
	symlinkOrSkip(t, "sub", filepath.Join(site, "subdir-link"))                     // dir link: not followed

	blocked := []string{
		"sites/evil/abs.txt",
		"sites/evil/rel.txt",
		"sites/evil/db.txt",
		"sites/evil/absdir/x.txt",
		"sites/evil/reldir/x.txt",
		"sites/evil/loop.txt",
		"sites/evil/dangling.txt",
		"sites/evil/sub/deep.txt",
		"sites/evil/subdir-link/deep.txt",
		"sites/evil/absdir",
	}
	for _, k := range blocked {
		if _, _, err := s.Get(ctx, k); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Get(%q) err = %v, want ErrNotFound", k, err)
		}
		if _, err := s.Stat(ctx, k); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Stat(%q) err = %v, want ErrNotFound", k, err)
		}
		if _, _, err := s.GetRange(ctx, k, 0, 3); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("GetRange(%q) err = %v, want ErrNotFound", k, err)
		}
	}
	// Safe links work and report the link's key.
	for _, k := range []string{"sites/evil/alias.html", "sites/evil/alias2.html"} {
		got, err := readAll(s, k)
		if err != nil || got != "<p>hi</p>" {
			t.Errorf("Get(%q) = %q, %v", k, got, err)
		}
		st, err := s.Stat(ctx, k)
		if err != nil || st.Key != k || st.ContentType != "text/html; charset=utf-8" {
			t.Errorf("Stat(%q) = %+v, %v", k, st, err)
		}
	}

	res, err := s.List(ctx, "sites/", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sites/evil/alias.html", "sites/evil/alias2.html", "sites/evil/index.html"}
	if got := keys(res); !slices.Equal(got, want) {
		t.Errorf("List = %q, want %q", got, want)
	}
	for _, o := range res.Objects {
		if strings.Contains(o.Key, "secret") || o.Size == int64(len("TOP SECRET")) {
			t.Errorf("listed escaped object %+v", o)
		}
	}
	res, err = s.List(ctx, "sites/evil/", "/")
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(res); !slices.Equal(got, want) || len(res.Prefixes) != 0 {
		t.Errorf("List delimiter = %q %q", got, res.Prefixes)
	}
	// Listing through a symlinked directory yields nothing.
	for _, p := range []string{"sites/evil/absdir/", "sites/evil/reldir/", "sites/evil/subdir-link/"} {
		res, err := s.List(ctx, p, "")
		if err != nil || len(res.Objects) != 0 {
			t.Errorf("List(%q) = %+v, %v", p, res, err)
		}
	}

	// Writes never go through symlinked directories.
	if _, err := s.Put(ctx, "sites/evil/absdir/new.txt", strings.NewReader("x"), 1, ""); err == nil {
		t.Error("Put through symlinked directory succeeded")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); !os.IsNotExist(err) {
		t.Errorf("file created outside root: %v", err)
	}
	// Overwriting a symlink key replaces the link, not its target.
	if _, err := s.Put(ctx, "sites/evil/abs.txt", strings.NewReader("replaced"), 8, ""); err != nil {
		t.Fatalf("Put over symlink: %v", err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "TOP SECRET" {
		t.Errorf("symlink target modified: %q", b)
	}
	if got, _ := readAll(s, "sites/evil/abs.txt"); got != "replaced" {
		t.Errorf("Get after replacing link = %q", got)
	}

	// Deleting through or of symlinks never touches the targets.
	if err := s.Delete(ctx, "sites/evil/absdir/x.txt"); err != nil {
		t.Errorf("Delete through symlinked dir: %v", err)
	}
	if err := s.Delete(ctx, "sites/evil/rel.txt"); err != nil {
		t.Errorf("Delete symlink: %v", err)
	}
	if err := s.DeletePrefix(ctx, "sites/evil/reldir/"); err != nil {
		t.Errorf("DeletePrefix through symlinked dir: %v", err)
	}
	if err := s.DeletePrefix(ctx, "sites/"); err != nil {
		t.Errorf("DeletePrefix(sites/): %v", err)
	}
	for _, p := range []string{outside, filepath.Join(outsideDir, "x.txt"), filepath.Join(dir, "backyard.db")} {
		if b, err := os.ReadFile(p); err != nil || (string(b) != "TOP SECRET" && string(b) != "DATABASE") {
			t.Errorf("target %s damaged: %q, %v", p, b, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "sites")); !os.IsNotExist(err) {
		t.Errorf("sites/ not removed: %v", err)
	}
}

func TestSpecialFilesIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	s, dir := newStore(t)
	if err := os.MkdirAll(filepath.Join(dir, "sp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(dir, "sp", "pipe")); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := s.Get(ctx, "sp/pipe"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get(fifo) err = %v", err)
	}
	if _, err := s.Stat(ctx, "sp/pipe"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat(fifo) err = %v", err)
	}
	res, err := s.List(ctx, "", "")
	if err != nil || len(res.Objects) != 0 {
		t.Errorf("List = %+v, %v", res, err)
	}
	symlinkOrSkip(t, "pipe", filepath.Join(dir, "sp", "pipe-link"))
	if _, _, err := s.Get(ctx, "sp/pipe-link"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get(link to fifo) err = %v", err)
	}
}

func TestInvalidFileNamesSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file names are restricted on windows")
	}
	s, dir := newStore(t)
	for _, name := range []string{"back\\slash.txt", "new\nline.txt", "ok.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.List(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(res); !slices.Equal(got, []string{"ok.txt"}) {
		t.Errorf("List = %q", got)
	}
}

func TestDistinctMTimesForRapidOverwrites(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	seen := map[string]bool{}
	for i := range 50 {
		data := bytes.Repeat([]byte{byte('a' + i%26)}, 32)
		info, err := s.Put(ctx, "rapid.txt", bytes.NewReader(data), int64(len(data)), "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[info.ETag] {
			t.Fatalf("ETag %q repeated on overwrite %d", info.ETag, i)
		}
		seen[info.ETag] = true
	}
}

func TestCancelledPutLeavesNoObject(t *testing.T) {
	s, _ := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelAfter{data: bytes.Repeat([]byte("x"), 1<<20), cancel: cancel}
	if _, err := s.Put(ctx, "cancel.bin", r, -1, ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Put err = %v, want context.Canceled", err)
	}
	if _, err := s.Stat(context.Background(), "cancel.bin"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat err = %v", err)
	}
}

type cancelAfter struct {
	data   []byte
	n      int
	cancel func()
}

func (c *cancelAfter) Read(p []byte) (int, error) {
	if c.n > 0 {
		c.cancel()
	}
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(len(p), 4096)], c.data)
	c.data = c.data[n:]
	c.n++
	return n, nil
}

// Puts racing with DeletePrefix/Delete (which prune empty directories) must
// not fail because a parent directory vanished under them.
func TestPutRacesWithPruning(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = s.DeletePrefix(ctx, "race/")
			_ = s.Delete(ctx, "race/a/b/c/d.txt")
		}
	}()
	for i := range 300 {
		if _, err := s.Put(ctx, "race/a/b/c/d.txt", strings.NewReader("x"), 1, ""); err != nil {
			t.Errorf("Put %d: %v", i, err)
			break
		}
	}
	close(stop)
	<-done
}

// When the fresh mtime equals the replaced file's (coarse timestamps or a
// file rsync'ed with a future mtime), Put bumps it so the ETag still changes.
func TestPutBumpsCollidingMTime(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	p := filepath.Join(dir, "same.txt")
	if err := os.WriteFile(p, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	before, err := s.Stat(ctx, "same.txt")
	if err != nil {
		t.Fatal(err)
	}
	// Force the next stamp to collide with the existing file's mtime.
	saved := lastMTime.Load()
	t.Cleanup(func() { lastMTime.Store(saved) })
	lastMTime.Store(future.UnixNano() - 1)
	info, err := s.Put(ctx, "same.txt", strings.NewReader("bbbb"), 4, "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ETag == before.ETag || info.ModTime.Equal(before.ModTime) {
		t.Errorf("ETag/mtime not bumped: before %+v after %+v", before, info)
	}
	if st, _ := s.Stat(ctx, "same.txt"); st.ETag != info.ETag {
		t.Errorf("Stat ETag %q != Put ETag %q", st.ETag, info.ETag)
	}
}

// Keys the fs backend cannot represent faithfully are invalid keys: names
// reserved for temp files and segments longer than NAME_MAX. A request for
// an over-long name is then a clean "invalid"/"not found", not an I/O error.
func TestFSKeyRestrictions(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	for _, key := range []string{
		tmpPrefix + "x",
		"a/" + tmpPrefix + "x/b.txt",
		"a/" + strings.Repeat("n", 256),
		strings.Repeat("d", 256) + "/x.txt",
	} {
		if _, err := s.Put(ctx, key, strings.NewReader("x"), 1, ""); !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("Put(%.40q) err = %v, want ErrInvalidKey", key, err)
		}
		if _, err := s.Stat(ctx, key); !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("Stat(%.40q) err = %v, want ErrInvalidKey", key, err)
		}
		if _, _, err := s.Get(ctx, key); !errors.Is(err, blob.ErrInvalidKey) {
			t.Errorf("Get(%.40q) err = %v, want ErrInvalidKey", key, err)
		}
	}
	ok := "a/" + strings.Repeat("n", 255)
	if _, err := s.Put(ctx, ok, strings.NewReader("x"), 1, ""); err != nil {
		t.Errorf("Put(255-byte name): %v", err)
	}
	// Names merely containing the marker are fine.
	if _, err := s.Put(ctx, "a/x"+tmpPrefix, strings.NewReader("x"), 1, ""); err != nil {
		t.Errorf("Put(name containing the temp marker): %v", err)
	}
}

// Symlinks that point at a reserved temp-file name are not followed.
func TestSymlinkToTempFileIgnored(t *testing.T) {
	s, dir := newStore(t)
	if err := os.WriteFile(filepath.Join(dir, tmpPrefix+"inflight"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, tmpPrefix+"inflight", filepath.Join(dir, "peek.txt"))
	if _, err := s.Stat(context.Background(), "peek.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat(link to temp file) err = %v, want ErrNotFound", err)
	}
}
