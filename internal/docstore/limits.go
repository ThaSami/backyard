package docstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// quotaCacheTTL bounds how long a per-site document count is trusted before
// WithLimits asks the backend again.
const quotaCacheTTL = 5 * time.Second

// maxQuotaCacheSites bounds the per-site count cache.
const maxQuotaCacheSites = 10000

// WithLimits wraps s so that writes enforce a maximum JSON size per document
// (ErrTooLarge; maxDocBytes <= 0 disables the check) and a maximum number of
// documents per site (ErrQuota; maxDocsPerSite <= 0 disables it).
//
// The size limit applies to Create and Put bodies and to the merged result of
// Update. The quota applies to Create and to a Put that creates a new
// document; Backyard's own namespace (SystemSite: the user cache and the
// audit log) is exempt. Per-site counts come from s.Count, are cached for a
// few seconds and kept current locally: every quota check reserves a slot
// (released again if the write fails or replaces an existing document), so
// concurrent writers on one instance cannot overshoot the quota, and the
// count is always re-read before a write is rejected. Writers on several
// instances may still overshoot it slightly (a soft limit). If both limits
// are disabled s is returned unchanged.
func WithLimits(s Store, maxDocBytes int, maxDocsPerSite int) Store {
	if maxDocBytes <= 0 && maxDocsPerSite <= 0 {
		return s
	}
	return &limitedStore{
		Store:       s,
		maxDocBytes: maxDocBytes,
		maxDocs:     int64(maxDocsPerSite),
		counts:      make(map[string]siteCount),
	}
}

// siteCount is the cached document count of one site.
type siteCount struct {
	// n is the count last read from the store plus the documents this
	// instance has added or removed since.
	n int64
	// pending counts quota slots reserved by in-flight writes.
	pending int64
	// created counts documents this instance has created (monotonic); it
	// detects writes that finish while the store is being counted.
	created uint64
	// at is when n was last read from the store (zero: never).
	at time.Time
}

type limitedStore struct {
	Store
	maxDocBytes int
	maxDocs     int64

	mu     sync.Mutex
	counts map[string]siteCount
}

// Unwrap returns the underlying store.
func (l *limitedStore) Unwrap() Store { return l.Store }

// quotaApplies reports whether the document quota is enforced for site.
func (l *limitedStore) quotaApplies(site string) bool {
	return l.maxDocs > 0 && site != SystemSite
}

func (l *limitedStore) quotaError(site string) error {
	return fmt.Errorf("%w: site %q has reached %d documents", ErrQuota, site, l.maxDocs)
}

func (l *limitedStore) Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*Doc, error) {
	data, err := PrepareData(data, l.maxDocBytes)
	if err != nil {
		return nil, err
	}
	if !l.quotaApplies(site) {
		return l.Store.Create(ctx, site, collection, id, data, actor)
	}
	ok, err := l.reserve(ctx, site)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, l.quotaError(site)
	}
	d, err := l.Store.Create(ctx, site, collection, id, data, actor)
	l.finish(site, true, err == nil)
	return d, err
}

func (l *limitedStore) Put(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*Doc, error) {
	data, err := PrepareData(data, l.maxDocBytes)
	if err != nil {
		return nil, err
	}
	if !l.quotaApplies(site) {
		return l.Store.Put(ctx, site, collection, id, data, actor)
	}
	reserved, err := l.reserve(ctx, site)
	if err != nil {
		return nil, err
	}
	if !reserved {
		// At the limit: replacing an existing document is still allowed.
		if _, err := l.Store.Get(ctx, site, collection, id); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, l.quotaError(site)
			}
			return nil, err
		}
	}
	d, err := l.Store.Put(ctx, site, collection, id, data, actor)
	// Version 1 after a Put means the document did not exist before.
	l.finish(site, reserved, err == nil && d.Version == 1)
	return d, err
}

func (l *limitedStore) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*Doc, error) {
	patch, err := PrepareData(patch, 0)
	if err != nil {
		return nil, err
	}
	if l.maxDocBytes > 0 {
		// Check the merged size up front. A concurrent update may still grow
		// the document slightly past the limit (soft limit).
		cur, err := l.Store.Get(ctx, site, collection, id)
		if err != nil {
			return nil, err
		}
		if _, err := PrepareData(ApplyPatch(cur.Data, patch), l.maxDocBytes); err != nil {
			return nil, err
		}
	}
	return l.Store.Update(ctx, site, collection, id, patch, actor)
}

func (l *limitedStore) UpdateIf(ctx context.Context, site, collection, id string, rev Rev, patch map[string]any, actor string) (*Doc, error) {
	patch, err := PrepareData(patch, 0)
	if err != nil {
		return nil, err
	}
	if l.maxDocBytes > 0 {
		// The document must be at rev anyway, so the size checked is exact.
		cur, err := l.Store.Get(ctx, site, collection, id)
		if err != nil {
			return nil, err
		}
		if !cur.At(rev) {
			return nil, fmt.Errorf("%w: document %q has changed", ErrConflict, id)
		}
		if _, err := PrepareData(ApplyPatch(cur.Data, patch), l.maxDocBytes); err != nil {
			return nil, err
		}
	}
	return l.Store.UpdateIf(ctx, site, collection, id, rev, patch, actor)
}

func (l *limitedStore) Delete(ctx context.Context, site, collection, id string) (bool, error) {
	ok, err := l.Store.Delete(ctx, site, collection, id)
	l.deleted(site, ok, err)
	return ok, err
}

func (l *limitedStore) DeleteIf(ctx context.Context, site, collection, id string, rev Rev) (bool, error) {
	ok, err := l.Store.DeleteIf(ctx, site, collection, id, rev)
	l.deleted(site, ok, err)
	return ok, err
}

// deleted keeps the cached count current after a delete.
func (l *limitedStore) deleted(site string, ok bool, err error) {
	if err == nil && ok {
		l.mu.Lock()
		if c, cached := l.counts[site]; cached && c.n > 0 {
			c.n--
			l.counts[site] = c
		}
		l.mu.Unlock()
	}
}

func (l *limitedStore) DeleteSite(ctx context.Context, site string) error {
	err := l.Store.DeleteSite(ctx, site)
	l.mu.Lock()
	delete(l.counts, site)
	l.mu.Unlock()
	return err
}

// maxRecounts bounds how often reserve re-reads a count that raced with
// local writes before it rejects a write.
const maxRecounts = 3

// reserve claims one document slot of site's quota and reports whether one
// was available. A fresh cached count with room left is trusted; otherwise the
// count is re-read from the store first, so a write is never rejected on a
// stale count. Every successful reserve must be paired with finish.
func (l *limitedStore) reserve(ctx context.Context, site string) (bool, error) {
	for attempt := 1; ; attempt++ {
		l.mu.Lock()
		c, ok := l.counts[site]
		if ok && !c.at.IsZero() && time.Since(c.at) < quotaCacheTTL && c.n+c.pending < l.maxDocs {
			c.pending++
			l.counts[site] = c
			l.mu.Unlock()
			return true, nil
		}
		if !ok {
			if len(l.counts) >= maxQuotaCacheSites {
				l.evictLocked()
			}
			// Track writes that finish while counting.
			l.counts[site] = c
		}
		created0 := c.created
		l.mu.Unlock()

		n, err := l.Store.Count(ctx, site)
		if err != nil {
			return false, fmt.Errorf("docstore: counting documents of site %q: %w", site, err)
		}

		l.mu.Lock()
		c = l.counts[site]
		// Documents created by this instance while counting may or may not be
		// included in n; assume they are not, so the count never falls short.
		// Slots reserved by in-flight writes stay reserved.
		var raced uint64
		if c.created > created0 {
			raced = c.created - created0
		}
		c.n, c.at = n+int64(raced), time.Now()
		if c.n+c.pending < l.maxDocs {
			c.pending++
			l.counts[site] = c
			l.mu.Unlock()
			return true, nil
		}
		l.counts[site] = c
		l.mu.Unlock()
		if raced == 0 || attempt >= maxRecounts {
			return false, nil
		}
		// The count raced with local writes and may be too high: count again
		// before rejecting.
	}
}

// finish completes a write: it releases a slot taken by reserve (reserved)
// and records a document the write added (created).
func (l *limitedStore) finish(site string, reserved, created bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.counts[site]
	if !ok {
		return // evicted or reset by DeleteSite; the next reserve re-counts
	}
	if reserved && c.pending > 0 {
		c.pending--
	}
	if created {
		c.n++
		c.created++
	}
	l.counts[site] = c
}

// evictLocked bounds the cache: it drops entries without in-flight writes
// (all of them are re-read on demand). Callers hold l.mu.
func (l *limitedStore) evictLocked() {
	for site, c := range l.counts {
		if c.pending == 0 {
			delete(l.counts, site)
		}
	}
}
