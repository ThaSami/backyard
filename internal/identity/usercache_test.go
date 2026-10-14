package identity

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/docstore"
)

// fakeStore is a minimal in-memory docstore.Store for UserCache tests.
type fakeStore struct {
	mu     sync.Mutex
	docs   map[string]*docstore.Doc // site/collection/id
	puts   int
	putErr error
	putFn  func(data map[string]any) error // optional per-write failure
	getErr error
	ctxErr error // ctx.Err() observed by the last Put
}

func newFakeStore() *fakeStore { return &fakeStore{docs: map[string]*docstore.Doc{}} }

func key(site, coll, id string) string { return site + "/" + coll + "/" + id }

func (s *fakeStore) Create(ctx context.Context, site, coll, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	return nil, errors.New("not used")
}

func (s *fakeStore) Get(ctx context.Context, site, coll, id string) (*docstore.Doc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	d, ok := s.docs[key(site, coll, id)]
	if !ok {
		return nil, docstore.ErrNotFound
	}
	cp := *d
	return &cp, nil
}

func (s *fakeStore) Put(ctx context.Context, site, coll, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.puts++
	s.ctxErr = ctx.Err()
	if s.putErr != nil {
		return nil, s.putErr
	}
	if s.putFn != nil {
		if err := s.putFn(data); err != nil {
			return nil, err
		}
	}
	d := &docstore.Doc{Site: site, Collection: coll, ID: id, Data: data, CreatedBy: actor, UpdatedBy: actor, Version: 1}
	s.docs[key(site, coll, id)] = d
	return d, nil
}

func (s *fakeStore) Update(ctx context.Context, site, coll, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	return nil, errors.New("not used")
}
func (s *fakeStore) UpdateIf(ctx context.Context, site, coll, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	return nil, errors.New("not used")
}
func (s *fakeStore) Delete(ctx context.Context, site, coll, id string) (bool, error) {
	return false, errors.New("not used")
}
func (s *fakeStore) DeleteIf(ctx context.Context, site, coll, id string, rev docstore.Rev) (bool, error) {
	return false, errors.New("not used")
}
func (s *fakeStore) Query(ctx context.Context, site, coll string, q docstore.Query) (*docstore.Page, error) {
	return nil, errors.New("not used")
}
func (s *fakeStore) Collections(ctx context.Context, site string) ([]string, error) { return nil, nil }
func (s *fakeStore) Count(ctx context.Context, site string) (int64, error)          { return 0, nil }
func (s *fakeStore) DeleteSite(ctx context.Context, site string) error              { return nil }
func (s *fakeStore) Ping(ctx context.Context) error                                 { return nil }
func (s *fakeStore) Close() error                                                   { return nil }

func (s *fakeStore) putCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts
}

// mapDir is a Directory backed by a map.
type mapDir struct {
	users map[string]*User
	err   error
}

func (m mapDir) Lookup(_ context.Context, email string) (*User, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.users[email].Clone(), nil
}

func TestUserCacheTouchAndLookup(t *testing.T) {
	st := newFakeStore()
	c := NewUserCache(st, nil)
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	alice := &User{
		ID: "alice@acme.com", Email: "Alice@Acme.com", Name: "Alice", Title: "Eng", Team: "Platform",
		Slack: "@alice", AvatarURL: "https://a", Groups: []string{"eng"}, Admin: true,
		Extra: map[string]any{"location": "Ottawa"},
	}
	c.Touch(ctx, alice)
	if st.putCount() != 1 {
		t.Fatalf("puts = %d, want 1", st.putCount())
	}
	doc := st.docs[key(SystemSite, UsersCollection, "alice@acme.com")]
	if doc == nil {
		t.Fatal("doc not stored under lowercased email")
	}
	if _, ok := doc.Data["admin"]; ok {
		t.Fatal("admin must not be persisted")
	}
	if doc.Data["last_seen"] != "2026-01-01T12:00:00Z" {
		t.Fatalf("last_seen = %v", doc.Data["last_seen"])
	}

	got, err := c.Lookup(ctx, " ALICE@acme.com ")
	if err != nil {
		t.Fatal(err)
	}
	want := &User{
		ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice", Title: "Eng", Team: "Platform",
		Slack: "@alice", AvatarURL: "https://a", Groups: []string{"eng"},
		Extra: map[string]any{"location": "Ottawa"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Lookup\n got  %+v\n want %+v", got, want)
	}

	// Throttled within the interval.
	now = now.Add(9 * time.Minute)
	c.Touch(ctx, alice)
	if st.putCount() != 1 {
		t.Fatalf("throttle: puts = %d", st.putCount())
	}
	now = now.Add(2 * time.Minute)
	c.Touch(ctx, alice)
	if st.putCount() != 2 {
		t.Fatalf("after interval: puts = %d", st.putCount())
	}

	if u, err := c.Lookup(ctx, "nobody@acme.com"); u != nil || err != nil {
		t.Fatalf("unknown = %v, %v", u, err)
	}
	if u, err := c.Lookup(ctx, ""); u != nil || err != nil {
		t.Fatalf("empty = %v, %v", u, err)
	}
}

func TestUserCacheSkips(t *testing.T) {
	st := newFakeStore()
	c := NewUserCache(st, nil)
	ctx := context.Background()
	for _, u := range []*User{
		nil,
		{ID: "anon:0123456789abcdef", Name: "Guest 0123", Anonymous: true},
		{ID: "anon:0123456789abcdef", Email: "x@y.io", Name: "Guest"},
		{ID: "token:ci", Name: "ci", Email: "ci@acme.com"},
		{ID: "sub:123", Name: "No Email"},
		{ID: "x", Email: "bogus", Name: "Bad Email"},
	} {
		c.Touch(ctx, u)
	}
	if st.putCount() != 0 {
		t.Fatalf("puts = %d, want 0", st.putCount())
	}
	var nilCache *UserCache
	nilCache.Touch(ctx, &User{ID: "a@b.io", Email: "a@b.io"})
	if u, err := nilCache.Lookup(ctx, "a@b.io"); u != nil || err != nil {
		t.Fatal("nil cache lookup")
	}
}

func TestUserCacheWriteFailureRetries(t *testing.T) {
	st := newFakeStore()
	st.putErr = errors.New("boom")
	c := NewUserCache(st, nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // request already finished: the write must still go through
	u := &User{ID: "a@x.io", Email: "a@x.io", Name: "A"}
	c.Touch(ctx, u)
	if st.ctxErr != nil {
		t.Fatalf("write context inherited cancellation: %v", st.ctxErr)
	}
	c.Touch(ctx, u)
	if st.putCount() != 1 {
		t.Fatalf("immediate retry: puts = %d", st.putCount())
	}
	now = now.Add(touchRetry + time.Second)
	st.mu.Lock()
	st.putErr = nil
	st.mu.Unlock()
	c.Touch(ctx, u)
	if st.putCount() != 2 {
		t.Fatalf("retry after a minute: puts = %d", st.putCount())
	}
}

func TestUserCacheFallback(t *testing.T) {
	st := newFakeStore()
	dir := mapDir{users: map[string]*User{
		"alice@acme.com": {ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice Directory", Title: "Director"},
		"dora@acme.com":  {ID: "dora@acme.com", Email: "dora@acme.com", Name: "Dora"},
	}}
	c := NewUserCache(st, dir)
	ctx := context.Background()
	c.Touch(ctx, &User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice IdP", AvatarURL: "https://a"})

	got, err := c.Lookup(ctx, "alice@acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Alice Directory" || got.Title != "Director" || got.AvatarURL != "https://a" {
		t.Fatalf("merged = %+v", got)
	}
	// Directory-only user.
	if got, _ := c.Lookup(ctx, "dora@acme.com"); got == nil || got.Name != "Dora" {
		t.Fatalf("directory only = %+v", got)
	}
	// Store failure: directory result still returned; otherwise error.
	st.getErr = errors.New("db down")
	if got, err := c.Lookup(ctx, "dora@acme.com"); err != nil || got == nil {
		t.Fatalf("store down with dir hit = %v, %v", got, err)
	}
	if _, err := c.Lookup(ctx, "zed@acme.com"); err == nil {
		t.Fatal("expected error when store fails and directory misses")
	}
	// Directory failure is tolerated.
	st.getErr = nil
	c2 := NewUserCache(st, mapDir{err: errors.New("dir down")})
	if got, err := c2.Lookup(ctx, "alice@acme.com"); err != nil || got == nil || got.Name != "Alice IdP" {
		t.Fatalf("dir down = %+v, %v", got, err)
	}
	// No store at all.
	c3 := NewUserCache(nil, dir)
	if got, _ := c3.Lookup(ctx, "dora@acme.com"); got == nil {
		t.Fatal("nil store with directory")
	}
	c3.Touch(ctx, &User{ID: "dora@acme.com", Email: "dora@acme.com"})
}

func TestUserCacheSweep(t *testing.T) {
	st := newFakeStore()
	c := NewUserCache(st, nil)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.mu.Lock()
	for i := 0; i < sweepThreshold; i++ {
		c.seen[string(rune('a'+i%26))+time.Duration(i).String()+"@x.io"] = now.Add(-time.Hour)
	}
	c.mu.Unlock()
	c.Touch(context.Background(), &User{ID: "new@x.io", Email: "new@x.io", Name: "N"})
	c.mu.Lock()
	n := len(c.seen)
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("after sweep: %d entries, want 1", n)
	}
}

func TestUserCacheConcurrent(t *testing.T) {
	st := newFakeStore()
	c := NewUserCache(st, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Touch(context.Background(), &User{ID: "a@x.io", Email: "a@x.io", Name: "A"})
			_, _ = c.Lookup(context.Background(), "a@x.io")
		}()
	}
	wg.Wait()
	if st.putCount() != 1 {
		t.Fatalf("puts = %d, want 1", st.putCount())
	}
}

func TestUserCacheLookupNormalizesEmail(t *testing.T) {
	st := newFakeStore()
	c := NewUserCache(st, nil)
	ctx := context.Background()
	c.Touch(ctx, &User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice"})
	if got, err := c.Lookup(ctx, "  Alice@ACME.com "); err != nil || got == nil || got.Name != "Alice" {
		t.Fatalf("mixed-case lookup = %+v, %v", got, err)
	}
	// Values that are not plain addresses never reach the store, even when it
	// is failing: they are simply unknown.
	st.getErr = errors.New("db down")
	for _, bad := range []string{"", "not-an-email", "a@b@c", "Alice <alice@acme.com>", "../../etc/passwd", "a@x\x00.io"} {
		if got, err := c.Lookup(ctx, bad); err != nil || got != nil {
			t.Errorf("Lookup(%q) = %+v, %v; want nil, nil", bad, got, err)
		}
	}
}

func TestUserCacheDirectoryFallbackNameDoesNotDowngrade(t *testing.T) {
	st := newFakeStore()
	// A sparse directory entry (no name column) carries the derived name.
	dir := mapDir{users: map[string]*User{"alice@acme.com": {ID: "alice@acme.com", Email: "alice@acme.com", Name: "alice", Team: "Platform"}}}
	c := NewUserCache(st, dir)
	ctx := context.Background()
	c.Touch(ctx, &User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice Smith"})
	got, err := c.Lookup(ctx, "alice@acme.com")
	if err != nil || got == nil || got.Name != "Alice Smith" || got.Team != "Platform" {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
}

func TestUserCachePermanentWriteErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("oversized extra is dropped", func(t *testing.T) {
		st := newFakeStore()
		st.putFn = func(data map[string]any) error {
			if data["extra"] != nil {
				return fmt.Errorf("%w: 300 KiB", docstore.ErrTooLarge)
			}
			return nil
		}
		c := NewUserCache(st, nil)
		c.Touch(ctx, &User{ID: "a@x.io", Email: "a@x.io", Name: "A", Team: "T", Extra: map[string]any{"blob": "huge"}})
		got, err := c.Lookup(ctx, "a@x.io")
		if err != nil || got == nil || got.Team != "T" || got.Extra != nil {
			t.Fatalf("lookup = %+v, %v", got, err)
		}
		if st.putCount() != 2 {
			t.Fatalf("puts = %d, want 2 (full, then without extra)", st.putCount())
		}
	})
	t.Run("invalid profile is not retried every minute", func(t *testing.T) {
		st := newFakeStore()
		st.putFn = func(map[string]any) error { return fmt.Errorf("%w: NUL in string", docstore.ErrInvalid) }
		c := NewUserCache(st, nil)
		now := time.Now()
		c.now = func() time.Time { return now }
		u := &User{ID: "a@x.io", Email: "a@x.io", Name: "A"}
		c.Touch(ctx, u)
		now = now.Add(touchRetry + time.Second)
		c.Touch(ctx, u)
		if st.putCount() != 1 {
			t.Fatalf("puts = %d, want 1", st.putCount())
		}
		now = now.Add(TouchInterval)
		c.Touch(ctx, u)
		if st.putCount() != 2 {
			t.Fatalf("puts after the interval = %d, want 2", st.putCount())
		}
	})
}
