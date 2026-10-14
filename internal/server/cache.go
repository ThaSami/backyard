package server

import (
	"container/list"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/blob"
)

const (
	// maxCachedObject is the largest object whose bytes are cached (SPEC §3).
	maxCachedObject = 1 << 20
	// cacheEntryOverhead approximates the bookkeeping cost of an entry.
	cacheEntryOverhead = 256
)

// cacheEntry is an immutable snapshot of a blob lookup: a missing object, a
// small object with its bytes, or a large object's metadata only.
type cacheEntry struct {
	key      string
	info     blob.ObjectInfo
	data     []byte // nil for large objects and misses
	notFound bool

	expires time.Time
	cost    int64
	elem    *list.Element
}

// objectCache is a byte-bounded LRU with a TTL in front of the blob store.
// A nil *objectCache is a valid, disabled cache.
type objectCache struct {
	max int64
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	used int64
	ll   *list.List // front = most recently used
	m    map[string]*cacheEntry
	// gen counts invalidations (remove, purgePrefix). A fill that started
	// before an invalidation is not stored, so a deploy that purges a site
	// cannot be undone by a concurrent read of the old bytes.
	gen uint64

	flightMu sync.Mutex
	flight   map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	e   *cacheEntry
	err error
}

// newObjectCache returns nil (disabled) when maxBytes <= 0.
func newObjectCache(maxBytes int64, ttl time.Duration) *objectCache {
	if maxBytes <= 0 {
		return nil
	}
	return &objectCache{
		max:    maxBytes,
		ttl:    ttl,
		now:    time.Now,
		ll:     list.New(),
		m:      make(map[string]*cacheEntry),
		flight: make(map[string]*flightCall),
	}
}

// get returns a fresh entry.
func (c *objectCache) get(key string) (*cacheEntry, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil, false
	}
	if c.now().After(e.expires) {
		c.removeLocked(e)
		return nil, false
	}
	c.ll.MoveToFront(e.elem)
	return e, true
}

// put stores e (replacing any entry for its key) and evicts LRU entries.
func (c *objectCache) put(e *cacheEntry) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.putLocked(e)
}

// generation returns the current invalidation counter.
func (c *objectCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// putIfGen stores e unless the cache was invalidated since gen was read.
func (c *objectCache) putIfGen(e *cacheEntry, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	c.putLocked(e)
}

func (c *objectCache) putLocked(e *cacheEntry) {
	e.cost = int64(len(e.data)) + int64(len(e.key)) + cacheEntryOverhead
	if e.cost > c.max {
		return
	}
	e.expires = c.now().Add(c.ttl)
	if old, ok := c.m[e.key]; ok {
		c.removeLocked(old)
	}
	e.elem = c.ll.PushFront(e)
	c.m[e.key] = e
	c.used += e.cost
	for c.used > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeLocked(back.Value.(*cacheEntry))
	}
}

func (c *objectCache) removeLocked(e *cacheEntry) {
	if cur, ok := c.m[e.key]; ok && cur == e {
		delete(c.m, e.key)
	}
	if e.elem != nil {
		c.ll.Remove(e.elem)
		e.elem = nil
		c.used -= e.cost
	}
}

// remove drops a key.
func (c *objectCache) remove(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	if e, ok := c.m[key]; ok {
		c.removeLocked(e)
	}
}

// purgePrefix drops every key starting with prefix.
func (c *objectCache) purgePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	for k, e := range c.m {
		if strings.HasPrefix(k, prefix) {
			c.removeLocked(e)
		}
	}
}

// size returns the number of entries and the accounted bytes.
func (c *objectCache) size() (int, int64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m), c.used
}

// do runs fill once per key at a time; concurrent callers share the result.
func (c *objectCache) do(key string, fill func() (*cacheEntry, error)) (*cacheEntry, error) {
	c.flightMu.Lock()
	if call, ok := c.flight[key]; ok {
		c.flightMu.Unlock()
		call.wg.Wait()
		return call.e, call.err
	}
	call := &flightCall{}
	call.wg.Add(1)
	c.flight[key] = call
	c.flightMu.Unlock()
	gen := c.generation()

	defer func() {
		c.flightMu.Lock()
		delete(c.flight, key)
		c.flightMu.Unlock()
		call.wg.Done()
	}()
	call.e, call.err = fill()
	if call.err == nil && call.e != nil {
		c.putIfGen(call.e, gen)
	}
	return call.e, call.err
}

// existsCache remembers for a short time whether a site has any files.
type existsCache struct {
	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[string]existsEntry
	// gen counts forget calls; see objectCache.gen.
	gen uint64
}

type existsEntry struct {
	exists  bool
	expires time.Time
}

const maxExistsEntries = 10000

func newExistsCache(ttl time.Duration) *existsCache {
	return &existsCache{ttl: ttl, now: time.Now, m: make(map[string]existsEntry)}
}

func (c *existsCache) get(site string) (exists, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[site]
	if !ok || c.now().After(e.expires) {
		return false, false
	}
	return e.exists, true
}

// generation returns the current invalidation counter (read it before the
// lookup whose result is passed to put).
func (c *existsCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// put records a lookup result unless the cache was invalidated since gen.
func (c *existsCache) put(site string, exists bool, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return
	}
	if len(c.m) >= maxExistsEntries {
		clear(c.m)
	}
	c.m[site] = existsEntry{exists: exists, expires: c.now().Add(c.ttl)}
}

func (c *existsCache) forget(site string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	delete(c.m, site)
}
