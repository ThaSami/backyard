//go:build linux && (amd64 || arm64)

package fs

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"unsafe"
)

// sysRenameat2 is the renameat2(2) syscall number (the frozen syscall
// package does not define it on every architecture).
var sysRenameat2 = map[string]uintptr{"amd64": 316, "arm64": 276}[runtime.GOARCH]

// renameExchange atomically swaps the paths a and b (RENAME_EXCHANGE).
func renameExchange(a, b string) error {
	pa, err := syscall.BytePtrFromString(a)
	if err != nil {
		return err
	}
	pb, err := syscall.BytePtrFromString(b)
	if err != nil {
		return err
	}
	const exchange = 1 << 1 // RENAME_EXCHANGE
	cwd := -100             // AT_FDCWD
	_, _, errno := syscall.Syscall6(sysRenameat2,
		uintptr(cwd), uintptr(unsafe.Pointer(pa)),
		uintptr(cwd), uintptr(unsafe.Pointer(pb)),
		exchange, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// startSwapper keeps atomically exchanging the entries a and b (a real
// directory and a symlink), as an attacker with write access below the root
// could do (e.g. through rsync), until stop is called.
func startSwapper(t *testing.T, a, b string) (stop func()) {
	t.Helper()
	if err := renameExchange(a, b); err != nil {
		t.Skipf("renameat2(RENAME_EXCHANGE) unsupported: %v", err)
	}
	var done atomic.Bool
	var wg sync.WaitGroup
	wg.Go(func() {
		for !done.Load() {
			_ = renameExchange(a, b)
		}
	})
	return func() { done.Store(true); wg.Wait() }
}

// A directory racing between "real directory" and "symlink to the root"
// must never redirect writes, reads, listings or deletes to the root, where
// the default SQLite database lives.
func TestSymlinkSwapRace(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	db := filepath.Join(dir, "backyard.db")
	if err := os.WriteFile(db, []byte("DATABASE"), 0o644); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(dir, "sites", "evil")
	sub := filepath.Join(site, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../..", filepath.Join(site, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	stop := startSwapper(t, sub, filepath.Join(site, "link"))
	defer stop()

	rootIntact := func() string {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err.Error()
		}
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if !slices.Equal(names, []string{"backyard.db", "sites"}) {
			return "root entries " + strings.Join(names, ",")
		}
		if b, err := os.ReadFile(db); err != nil || !bytes.Equal(b, []byte("DATABASE")) {
			return "database changed: " + string(b)
		}
		return ""
	}
	var wg sync.WaitGroup
	var failed atomic.Bool
	for w := range 4 {
		wg.Go(func() {
			for i := 0; i < 1500 && !failed.Load(); i++ {
				switch (i + w) % 6 {
				case 0:
					_, _ = s.Put(ctx, "sites/evil/sub/new.db", strings.NewReader("PWNED"), 5, "")
				case 1:
					_, _ = s.Put(ctx, "sites/evil/sub/backyard.db", strings.NewReader("PWNED"), 5, "")
				case 2:
					if rc, _, err := s.Get(ctx, "sites/evil/sub/backyard.db"); err == nil {
						b, _ := io.ReadAll(rc)
						rc.Close()
						if string(b) == "DATABASE" {
							failed.Store(true)
							t.Errorf("read the root database through a swapped directory")
						}
					}
				case 3:
					if res, err := s.List(ctx, "sites/", ""); err == nil {
						for _, o := range res.Objects {
							if strings.HasSuffix(o.Key, "/backyard.db") && o.Size == int64(len("DATABASE")) {
								failed.Store(true)
								t.Errorf("listed the root database as %q", o.Key)
							}
						}
					}
				case 4:
					_ = s.Delete(ctx, "sites/evil/sub/backyard.db")
					_ = s.Delete(ctx, "sites/evil/sub/new.db")
				case 5:
					_ = s.DeletePrefix(ctx, "sites/evil/sub/")
				}
				if msg := rootIntact(); msg != "" {
					failed.Store(true)
					t.Errorf("worker %d op %d: %s", w, i, msg)
				}
			}
		})
	}
	wg.Wait()
}
