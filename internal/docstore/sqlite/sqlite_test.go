package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/conformance"
	"github.com/thasami/backyard/internal/docstore/sqlite"
)

func open(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) docstore.Store {
		return open(t, filepath.Join(t.TempDir(), "backyard.db"))
	})
}

func TestConformanceSharedFile(t *testing.T) {
	// One database shared by every subtest (they use distinct sites).
	s := open(t, filepath.Join(t.TempDir(), "shared.db"))
	conformance.Run(t, func(t *testing.T) docstore.Store { return s })
}

func TestConformanceInMemory(t *testing.T) {
	conformance.Run(t, func(t *testing.T) docstore.Store { return open(t, ":memory:") })
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "re.db")
	ctx := context.Background()
	s1, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Create(ctx, "site", "c", "a", map[string]any{"v": 1}, "me"); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := open(t, path) // migrations are idempotent
	d, err := s2.Get(ctx, "site", "c", "a")
	if err != nil || d.Data["v"] != 1.0 || d.CreatedBy != "me" {
		t.Fatalf("Get after reopen = %+v, %v", d, err)
	}
}

// Two independent handles on one file behave like two processes: WAL plus
// busy_timeout and BEGIN IMMEDIATE keep concurrent updates atomic.
func TestTwoHandlesConcurrentUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "multi.db")
	a, b := open(t, path), open(t, path)
	ctx := context.Background()
	if _, err := a.Create(ctx, "site", "c", "hot", nil, ""); err != nil {
		t.Fatal(err)
	}
	const n = 40
	var wg sync.WaitGroup
	for i := range n {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			if _, err := s.Update(ctx, "site", "c", "hot", map[string]any{fmt.Sprintf("k%d", i): i}, ""); err != nil {
				t.Errorf("Update: %v", err)
			}
		})
	}
	wg.Wait()
	d, err := b.Get(ctx, "site", "c", "hot")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Data) != n || d.Version != n+1 {
		t.Errorf("after concurrent updates: %d keys, version %d; want %d, %d", len(d.Data), d.Version, n, n+1)
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	for _, p := range []string{"", "/tmp/x.db?mode=ro", filepath.Join(t.TempDir(), "missing-dir", "x.db")} {
		if s, err := sqlite.Open(ctx, p); err == nil {
			_ = s.Close()
			t.Errorf("Open(%q) succeeded", p)
		}
	}
}

func TestNewDatabaseIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	s := open(t, path)
	if _, err := s.Create(context.Background(), "site", "c", "a", map[string]any{"secret": true}, ""); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("%s has mode %v, want no group/other access", filepath.Base(p), perm)
		}
	}
	// An existing file keeps its permissions.
	shared := filepath.Join(t.TempDir(), "shared.db")
	if err := os.WriteFile(shared, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	open(t, shared)
	if fi, err := os.Stat(shared); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("existing database mode = %v, %v; want 0640", fi.Mode().Perm(), err)
	}
}
