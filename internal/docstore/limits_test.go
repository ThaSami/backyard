package docstore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/memory"
)

// countingStore wraps a store and counts Count/Get calls.
type countingStore struct {
	docstore.Store
	counts   atomic.Int64
	gets     atomic.Int64
	countErr error
}

func (c *countingStore) Count(ctx context.Context, site string) (int64, error) {
	c.counts.Add(1)
	if c.countErr != nil {
		return 0, c.countErr
	}
	return c.Store.Count(ctx, site)
}

func (c *countingStore) Get(ctx context.Context, site, collection, id string) (*docstore.Doc, error) {
	c.gets.Add(1)
	return c.Store.Get(ctx, site, collection, id)
}

func TestWithLimitsDisabled(t *testing.T) {
	m := memory.New()
	if s := docstore.WithLimits(m, 0, 0); s != docstore.Store(m) {
		t.Errorf("WithLimits(0, 0) wrapped the store")
	}
	s := docstore.WithLimits(m, 0, -1)
	if s != docstore.Store(m) {
		t.Errorf("WithLimits(0, -1) wrapped the store")
	}
}

func TestWithLimitsCachesCounts(t *testing.T) {
	cs := &countingStore{Store: memory.New()}
	s := docstore.WithLimits(cs, 0, 1000)
	ctx := context.Background()
	for range 50 {
		if _, err := s.Create(ctx, "site", "c", "", map[string]any{}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if n := cs.counts.Load(); n != 1 {
		t.Errorf("Count called %d times for 50 creates under quota; want 1 (cached)", n)
	}
	if u, ok := s.(interface{ Unwrap() docstore.Store }); !ok || u.Unwrap() != docstore.Store(cs) {
		t.Errorf("Unwrap did not return the wrapped store")
	}
}

func TestWithLimitsRechecksAtLimit(t *testing.T) {
	base := memory.New()
	cs := &countingStore{Store: base}
	s := docstore.WithLimits(cs, 0, 2)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if _, err := s.Create(ctx, "site", "c", id, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	// Another instance deletes a document behind the wrapper's back: the
	// cached count says "full", but the authoritative re-count allows it.
	if _, err := base.Delete(ctx, "site", "c", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "site", "c", "c", nil, ""); err != nil {
		t.Errorf("Create after out-of-band delete: %v", err)
	}
	if _, err := s.Create(ctx, "site", "c", "d", nil, ""); !errors.Is(err, docstore.ErrQuota) {
		t.Errorf("Create over quota = %v", err)
	}
}

func TestWithLimitsPropagatesCountErrors(t *testing.T) {
	boom := errors.New("boom")
	cs := &countingStore{Store: memory.New(), countErr: boom}
	s := docstore.WithLimits(cs, 0, 10)
	ctx := context.Background()
	if _, err := s.Create(ctx, "site", "c", "", nil, ""); !errors.Is(err, boom) {
		t.Errorf("Create with failing Count = %v, want boom", err)
	}
	if _, err := s.Put(ctx, "site", "c", "x", nil, ""); !errors.Is(err, boom) {
		t.Errorf("Put with failing Count = %v, want boom", err)
	}
}

func TestWithLimitsSizeOnly(t *testing.T) {
	cs := &countingStore{Store: memory.New()}
	s := docstore.WithLimits(cs, 64, 0)
	ctx := context.Background()
	if _, err := s.Create(ctx, "site", "c", "a", map[string]any{"x": strings.Repeat("a", 100)}, ""); !errors.Is(err, docstore.ErrTooLarge) {
		t.Errorf("Create too large = %v", err)
	}
	// Reserved keys do not count towards the size.
	d, err := s.Create(ctx, "site", "c", "a", map[string]any{"id": strings.Repeat("i", 100), "_meta": strings.Repeat("m", 100), "x": 1}, "")
	if err != nil {
		t.Fatalf("Create with large reserved keys: %v", err)
	}
	if _, ok := d.Data["id"]; ok {
		t.Error("reserved key stored")
	}
	if n := cs.counts.Load(); n != 0 {
		t.Errorf("Count called %d times without a quota", n)
	}
	if _, err := s.Update(ctx, "site", "c", "a", map[string]any{"x": strings.Repeat("b", 100)}, ""); !errors.Is(err, docstore.ErrTooLarge) {
		t.Errorf("Update too large = %v", err)
	}
	// A patch that shrinks the document is fine even if the patch itself
	// carries large deletions.
	if _, err := s.Update(ctx, "site", "c", "a", map[string]any{"x": nil, "y": "small"}, ""); err != nil {
		t.Errorf("Update shrinking: %v", err)
	}
}

func TestWithLimitsPutCountsNewDocs(t *testing.T) {
	s := docstore.WithLimits(memory.New(), 0, 2)
	ctx := context.Background()
	for _, id := range []string{"a", "a", "a", "b"} {
		if _, err := s.Put(ctx, "site", "c", id, nil, ""); err != nil {
			t.Fatalf("Put(%s): %v", id, err)
		}
	}
	if _, err := s.Put(ctx, "site", "c", "c", nil, ""); !errors.Is(err, docstore.ErrQuota) {
		t.Errorf("third distinct Put = %v, want ErrQuota", err)
	}
	if _, err := s.Put(ctx, "site", "c", "b", map[string]any{"v": 2}, ""); err != nil {
		t.Errorf("replacing Put at quota: %v", err)
	}
}

func TestWithLimitsSystemSiteExempt(t *testing.T) {
	cs := &countingStore{Store: memory.New()}
	s := docstore.WithLimits(cs, 64, 1)
	ctx := context.Background()
	for range 3 {
		if _, err := s.Create(ctx, docstore.SystemSite, "_audit", "", map[string]any{"a": 1}, ""); err != nil {
			t.Fatalf("Create in %s: %v", docstore.SystemSite, err)
		}
	}
	if _, err := s.Put(ctx, docstore.SystemSite, "_users", "a@b.c", nil, ""); err != nil {
		t.Fatalf("Put in %s: %v", docstore.SystemSite, err)
	}
	if n := cs.counts.Load(); n != 0 {
		t.Errorf("Count called %d times for the system site", n)
	}
	if _, err := s.Create(ctx, docstore.SystemSite, "_audit", "", map[string]any{"x": strings.Repeat("a", 100)}, ""); !errors.Is(err, docstore.ErrTooLarge) {
		t.Errorf("oversized system document = %v, want ErrTooLarge", err)
	}
	// Ordinary sites keep their quota.
	if _, err := s.Create(ctx, "site", "c", "", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "site", "c", "", nil, ""); !errors.Is(err, docstore.ErrQuota) {
		t.Errorf("second Create = %v, want ErrQuota", err)
	}
}

func TestWithLimitsConcurrentWriters(t *testing.T) {
	const quota, writers = 10, 100
	base := memory.New()
	s := docstore.WithLimits(base, 0, quota)
	ctx := context.Background()
	var ok, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			var err error
			if i%3 == 0 {
				_, err = s.Put(ctx, "site", "c", fmt.Sprintf("p%d", i), nil, "")
			} else {
				_, err = s.Create(ctx, "site", "c", "", nil, "")
			}
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, docstore.ErrQuota):
				rejected.Add(1)
			default:
				t.Errorf("write: %v", err)
			}
		})
	}
	wg.Wait()
	if ok.Load() != quota || rejected.Load() != writers-quota {
		t.Errorf("%d ok, %d rejected; want %d, %d", ok.Load(), rejected.Load(), quota, writers-quota)
	}
	if n, _ := base.Count(ctx, "site"); n != quota {
		t.Errorf("Count = %d, want %d", n, quota)
	}
	// Failed writes give their reserved slot back.
	if _, err := s.Create(ctx, "other", "c", "dup", nil, ""); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, err := s.Create(ctx, "other", "c", "dup", nil, ""); !errors.Is(err, docstore.ErrConflict) {
			t.Fatalf("duplicate Create = %v", err)
		}
	}
	for i := range quota - 1 {
		if _, err := s.Create(ctx, "other", "c", "", map[string]any{"i": i}, ""); err != nil {
			t.Fatalf("Create %d after failed writes: %v", i, err)
		}
	}
}

// staleCountStore makes the first Count block until released and then return
// the count as it was when the call started.
type staleCountStore struct {
	docstore.Store
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (s *staleCountStore) Count(ctx context.Context, site string) (int64, error) {
	n, err := s.Store.Count(ctx, site)
	if s.calls.Add(1) == 1 {
		close(s.started)
		<-s.release
	}
	return n, err
}

// A count read before concurrent writes committed must not overwrite the
// newer local count (which would let the quota be exceeded).
func TestWithLimitsStaleCountDoesNotOvershoot(t *testing.T) {
	base := &staleCountStore{Store: memory.New(), started: make(chan struct{}), release: make(chan struct{})}
	s := docstore.WithLimits(base, 0, 2)
	ctx := context.Background()
	errc := make(chan error, 1)
	go func() {
		_, err := s.Create(ctx, "site", "c", "slow", nil, "")
		errc <- err
	}()
	<-base.started // the slow writer has counted 0 documents
	for _, id := range []string{"a", "b"} {
		if _, err := s.Create(ctx, "site", "c", id, nil, ""); err != nil {
			t.Fatalf("Create(%s): %v", id, err)
		}
	}
	close(base.release)
	if err := <-errc; !errors.Is(err, docstore.ErrQuota) {
		t.Errorf("Create with a stale count = %v, want ErrQuota", err)
	}
	if n, _ := base.Store.Count(ctx, "site"); n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}
