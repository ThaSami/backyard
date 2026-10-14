package mcpremote

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/memory"
)

// hookDocs runs its hook before every write but the Create a lease
// acquisition starts with: the hook sees the operation and the collection,
// may block, run other publishes or fail the write. A hook that fires
// clears itself (set(nil)) before it does anything else.
type hookDocs struct {
	docstore.Store
	mu   sync.Mutex
	hook func(ctx context.Context, op, collection string) error
}

func (h *hookDocs) set(f func(ctx context.Context, op, collection string) error) {
	h.mu.Lock()
	h.hook = f
	h.mu.Unlock()
}

func (h *hookDocs) at(ctx context.Context, op, collection string) error {
	if op == "create" && collection == lockCollection {
		return nil
	}
	h.mu.Lock()
	f := h.hook
	h.mu.Unlock()
	if f == nil {
		return nil
	}
	return f(ctx, op, collection)
}

func (h *hookDocs) Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	if err := h.at(ctx, "create", collection); err != nil {
		return nil, err
	}
	return h.Store.Create(ctx, site, collection, id, data, actor)
}

func (h *hookDocs) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	if err := h.at(ctx, "update", collection); err != nil {
		return nil, err
	}
	return h.Store.Update(ctx, site, collection, id, patch, actor)
}

func (h *hookDocs) UpdateIf(ctx context.Context, site, collection, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	if err := h.at(ctx, "update", collection); err != nil {
		return nil, err
	}
	return h.Store.UpdateIf(ctx, site, collection, id, rev, patch, actor)
}

func (h *hookDocs) Delete(ctx context.Context, site, collection, id string) (bool, error) {
	if err := h.at(ctx, "delete", collection); err != nil {
		return false, err
	}
	return h.Store.Delete(ctx, site, collection, id)
}

func (h *hookDocs) DeleteIf(ctx context.Context, site, collection, id string, rev docstore.Rev) (bool, error) {
	if err := h.at(ctx, "delete", collection); err != nil {
		return false, err
	}
	return h.Store.DeleteIf(ctx, site, collection, id, rev)
}

// replicas returns n endpoints over one store and one clock.
func replicas(t *testing.T, docs docstore.Store, now *time.Time, n int) []*tenv {
	t.Helper()
	out := make([]*tenv, n)
	for i := range out {
		out[i] = newTenv(t, tenvOption{opts: func(o *Options) {
			o.Docs = docs
			o.Now = func() time.Time { return *now }
		}})
	}
	return out
}

// A taker that read an expired lease and died at its write (its client
// went away) leaves nothing that a later takeover could trip over: of two
// takers that read the lease after that, the one that writes second
// answers E20 and writes nothing, and the winner's release frees the site.
func TestLeaseTakeoverAfterDeadTaker(t *testing.T) {
	docs := &hookDocs{Store: memory.New()}
	now := time.Unix(1_800_000_000, 0)
	r := replicas(t, docs, &now, 3)
	ctx := context.Background()
	if _, err := docs.Create(ctx, docstore.SystemSite, lockCollection, "s", map[string]any{"exp": now.Unix() - 1}, "x"); err != nil {
		t.Fatal(err)
	}
	docs.set(func(_ context.Context, op, collection string) error {
		if collection != lockCollection {
			return nil
		}
		docs.set(nil)
		return context.Canceled
	})
	if _, err := r[0].h.acquireLease(ctx, "s", "dave"); err == nil || err.ID != "E21" {
		t.Fatalf("the dying taker = %v, want E21", err)
	}
	docs.set(nil)
	now = now.Add(leaseTTL + time.Second) // whatever it left has expired

	// r2 reads the lease and stalls before its first write; r1 takes over.
	blocked, proceed := make(chan struct{}), make(chan struct{})
	docs.set(func(_ context.Context, op, collection string) error {
		docs.set(nil)
		close(blocked)
		<-proceed
		return nil
	})
	slow := make(chan *deploy.PublishError, 1)
	go func() {
		_, err := r[2].h.acquireLease(ctx, "s", "bob")
		slow <- err
	}()
	<-blocked
	release1, err := r[1].h.acquireLease(ctx, "s", "alice")
	if err != nil {
		t.Fatalf("r1 = %v", err)
	}
	won, gerr := docs.Get(ctx, docstore.SystemSite, lockCollection, "s")
	if gerr != nil {
		t.Fatal(gerr)
	}
	close(proceed)
	if err := <-slow; err == nil || err.ID != "E20" {
		t.Fatalf("r2 = %v, want E20", err)
	}
	if d, err := docs.Get(ctx, docstore.SystemSite, lockCollection, "s"); err != nil || d.Version != won.Version || !d.UpdatedAt.Equal(won.UpdatedAt) || leaseExp(d) != leaseExp(won) {
		t.Fatalf("r2 rewrote the lease r1 holds: %+v, %v (r1's: %+v)", d, err, won)
	}
	release1()
	release, err := r[1].h.acquireLease(ctx, "s", "carol")
	if err != nil {
		t.Fatalf("the next publish after r1's release = %v", err)
	}
	release()
}

// A release whose delete the store holds up until the lease has expired
// and been taken over leaves the new holder's lease: the site stays
// serialized.
func TestLeaseLateReleaseKeepsTakenOverLease(t *testing.T) {
	docs := &hookDocs{Store: memory.New()}
	now := time.Unix(1_800_000_000, 0)
	r := replicas(t, docs, &now, 3)
	ctx := context.Background()
	release1, err := r[0].h.acquireLease(ctx, "s", "alice")
	if err != nil {
		t.Fatal(err)
	}
	var taken *deploy.PublishError
	release2 := func() {}
	docs.set(func(_ context.Context, op, collection string) error {
		if op != "delete" || collection != lockCollection {
			return nil
		}
		docs.set(nil)
		now = now.Add(leaseTTL + time.Second)
		release2, taken = r[1].h.acquireLease(ctx, "s", "bob")
		return nil
	})
	release1()
	if taken != nil {
		t.Fatalf("r2's takeover = %v", taken)
	}
	if _, err := docs.Get(ctx, docstore.SystemSite, lockCollection, "s"); err != nil {
		t.Fatalf("r1's late release removed r2's lease: %v", err)
	}
	if _, err := r[2].h.acquireLease(ctx, "s", "carol"); err == nil || err.ID != "E20" {
		t.Fatalf("a third publish while r2 holds the lease = %v, want E20", err)
	}
	release2()
	if _, err := docs.Get(ctx, docstore.SystemSite, lockCollection, "s"); !errors.Is(err, docstore.ErrNotFound) {
		t.Fatalf("r2's release left its lease: %v", err)
	}
}

// A release is bounded: one that the store holds up is abandoned (the
// lease expires by itself), also when the publish's client went away.
func TestLeaseReleaseIsBounded(t *testing.T) {
	docs := &hookDocs{Store: memory.New()}
	now := time.Unix(1_800_000_000, 0)
	e := replicas(t, docs, &now, 1)[0]
	e.h.releaseTimeout = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	release, err := e.h.acquireLease(ctx, "s", "alice")
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	docs.set(func(ctx context.Context, op, collection string) error {
		docs.set(nil)
		<-ctx.Done() // a store that never answers
		return ctx.Err()
	})
	done := make(chan struct{})
	go func() {
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled release was not abandoned")
	}
	if _, err := docs.Get(context.Background(), docstore.SystemSite, lockCollection, "s"); err != nil {
		t.Fatalf("the lease left behind: %v (it expires by itself)", err)
	}
}
