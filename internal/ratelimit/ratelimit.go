// Package ratelimit implements Backyard's per-instance request limits
// (SPEC §11): token buckets per category and key, and a daily budget of AI
// tokens.
//
// Keys identify the caller: the user id for authenticated users, or the
// client IP for anonymous ones (see Key). Every category has its own bucket
// per key whose capacity is the per-minute limit and which refills at
// limit/60 tokens per second, so a client may burst up to one minute's worth
// of requests and then continues at the sustained rate.
//
// Buckets that have refilled completely are indistinguishable from new ones
// and are evicted lazily, so memory stays proportional to the number of
// recently active callers. The daily AI token counter resets at midnight UTC.
//
// A Limiter is safe for concurrent use. Limits are per instance: with N
// server instances behind a load balancer the effective limit is up to N
// times higher.
package ratelimit

import (
	"hash/maphash"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/thasami/backyard/internal/config"
)

// Category is a class of requests sharing one limit.
type Category string

// Rate-limited categories.
const (
	// API counts every site/hub API request (ratelimit.api_per_minute).
	API Category = "api"
	// DBWrite counts database writes (ratelimit.db_writes_per_minute).
	DBWrite Category = "db_write"
	// AI counts AI calls: chat, image, embed (ratelimit.ai_per_minute).
	AI Category = "ai"
	// Warehouse counts warehouse queries (ratelimit.warehouse_per_minute).
	Warehouse Category = "warehouse"
	// Upload counts file uploads (ratelimit.uploads_per_minute).
	Upload Category = "upload"
)

// Categories lists every category.
var Categories = []Category{API, DBWrite, AI, Warehouse, Upload}

// Limits are the configured limits. A per-minute limit <= 0 and an
// AITokensPerDay <= 0 mean "unlimited".
type Limits struct {
	PerMinute      map[Category]int
	AITokensPerDay int64
}

// LimitsFromConfig extracts the request limits from the configuration.
func LimitsFromConfig(c config.RateLimitConfig) Limits {
	return Limits{
		PerMinute: map[Category]int{
			API:       c.APIPerMinute,
			DBWrite:   c.DBWritesPerMinute,
			AI:        c.AIPerMinute,
			Warehouse: c.WarehousePerMinute,
			Upload:    c.UploadsPerMinute,
		},
		AITokensPerDay: c.AITokensPerDay,
	}
}

const (
	numShards = 32
	// sweepInterval is how often a shard is scanned for evictable entries.
	sweepInterval = time.Minute
	// DefaultMaxKeys bounds the number of tracked keys per category (and for
	// the daily token counter).
	DefaultMaxKeys = 1 << 20
)

// Option configures a Limiter.
type Option func(*Limiter)

// WithClock replaces the time source (tests).
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) {
		if now != nil {
			l.now = now
		}
	}
}

// WithMaxKeys bounds the number of keys tracked per category. When the bound
// is reached, idle entries are evicted first and then arbitrary ones (which
// only ever makes those callers less limited, never more).
func WithMaxKeys(n int) Option {
	return func(l *Limiter) {
		if n > 0 {
			l.maxKeys = n
		}
	}
}

// Limiter enforces Limits. The zero value is not usable; call New.
type Limiter struct {
	disabled bool
	limits   Limits
	now      func() time.Time
	maxKeys  int
	seed     maphash.Seed

	buckets map[Category]*bucketSet
	tokens  *dailyCounter
}

// New returns a limiter for cfg (cfg.Disabled turns every check into a no-op).
func New(cfg config.RateLimitConfig, opts ...Option) *Limiter {
	return NewWithLimits(LimitsFromConfig(cfg), cfg.Disabled, opts...)
}

// NewWithLimits returns a limiter for explicit limits.
func NewWithLimits(limits Limits, disabled bool, opts ...Option) *Limiter {
	l := &Limiter{
		disabled: disabled,
		limits:   limits,
		now:      time.Now,
		maxKeys:  DefaultMaxKeys,
		seed:     maphash.MakeSeed(),
		buckets:  make(map[Category]*bucketSet),
	}
	for _, o := range opts {
		o(l)
	}
	perShard := max(1, l.maxKeys/numShards)
	for cat, n := range limits.PerMinute {
		if n <= 0 {
			continue
		}
		bs := &bucketSet{
			rate:     rate.Limit(float64(n) / 60),
			burst:    n,
			perShard: perShard,
		}
		for i := range bs.shards {
			bs.shards[i].m = make(map[string]*rate.Limiter)
		}
		l.buckets[cat] = bs
	}
	l.tokens = &dailyCounter{perShard: perShard}
	for i := range l.tokens.shards {
		l.tokens.shards[i].m = make(map[string]dayUsage)
	}
	return l
}

// Enabled reports whether limits are enforced at all.
func (l *Limiter) Enabled() bool { return l != nil && !l.disabled }

// Limit returns the per-minute limit of a category (0 = unlimited).
func (l *Limiter) Limit(cat Category) int {
	if !l.Enabled() {
		return 0
	}
	if bs := l.buckets[cat]; bs != nil {
		return bs.burst
	}
	return 0
}

// Key returns the rate-limit key of a caller: the user id, or the client IP
// for anonymous users (anonymous ids are cheap to mint by dropping cookies).
func Key(userID string, anonymous bool, clientIP string) string {
	if anonymous || userID == "" {
		return "ip:" + clientIP
	}
	return "u:" + userID
}

// Allow consumes one token of cat for key. When the bucket is empty it
// returns false and how long the caller should wait before retrying.
func (l *Limiter) Allow(cat Category, key string) (bool, time.Duration) {
	if !l.Enabled() {
		return true, 0
	}
	bs := l.buckets[cat]
	if bs == nil {
		return true, 0
	}
	now := l.now()
	lim := bs.get(l.shard(key), key, now)
	r := lim.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Minute
	}
	d := r.DelayFrom(now)
	if d <= 0 {
		return true, 0
	}
	r.CancelAt(now)
	return false, d
}

// CheckTokens reports whether key still has AI token budget left today. When
// the budget is exhausted it returns false and the time until it resets
// (midnight UTC).
func (l *Limiter) CheckTokens(key string) (bool, time.Duration) {
	if !l.Enabled() || l.limits.AITokensPerDay <= 0 {
		return true, 0
	}
	now := l.now().UTC()
	if l.tokens.used(l.shard(key), key, now) < l.limits.AITokensPerDay {
		return true, 0
	}
	return false, untilMidnight(now)
}

// AddTokens records n AI tokens (input + output) consumed by key today.
func (l *Limiter) AddTokens(key string, n int64) {
	if !l.Enabled() || l.limits.AITokensPerDay <= 0 || n <= 0 {
		return
	}
	l.tokens.add(l.shard(key), key, n, l.now().UTC())
}

// TokensUsed returns the AI tokens consumed by key today.
func (l *Limiter) TokensUsed(key string) int64 {
	if !l.Enabled() {
		return 0
	}
	return l.tokens.used(l.shard(key), key, l.now().UTC())
}

// Tracked returns the number of keys currently tracked for cat (tests,
// diagnostics).
func (l *Limiter) Tracked(cat Category) int {
	if l == nil {
		return 0
	}
	bs := l.buckets[cat]
	if bs == nil {
		return 0
	}
	n := 0
	for i := range bs.shards {
		sh := &bs.shards[i]
		sh.mu.Lock()
		n += len(sh.m)
		sh.mu.Unlock()
	}
	return n
}

// RetryAfterSeconds converts a wait duration into a Retry-After header value
// (whole seconds, rounded up, at least 1).
func RetryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	s := int(math.Ceil(d.Seconds()))
	return max(s, 1)
}

func (l *Limiter) shard(key string) int {
	return int(maphash.String(l.seed, key) % numShards)
}

func untilMidnight(now time.Time) time.Duration {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(now)
}

// ---------------------------------------------------------------------------
// Token buckets

type bucketSet struct {
	rate     rate.Limit
	burst    int
	perShard int
	shards   [numShards]bucketShard
}

type bucketShard struct {
	mu        sync.Mutex
	m         map[string]*rate.Limiter
	lastSweep time.Time
}

// get returns the bucket of key, creating it if needed. Full buckets are
// evicted lazily: a bucket that has refilled completely behaves exactly like
// a new one.
func (bs *bucketSet) get(shard int, key string, now time.Time) *rate.Limiter {
	sh := &bs.shards[shard]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if lim, ok := sh.m[key]; ok {
		return lim
	}
	if now.Sub(sh.lastSweep) >= sweepInterval || len(sh.m) >= bs.perShard {
		sh.lastSweep = now
		full := float64(bs.burst)
		for k, lim := range sh.m {
			if lim.TokensAt(now) >= full {
				delete(sh.m, k)
			}
		}
		// Still at the bound: drop arbitrary entries (map order is random).
		for k := range sh.m {
			if len(sh.m) < bs.perShard {
				break
			}
			delete(sh.m, k)
		}
	}
	lim := rate.NewLimiter(bs.rate, bs.burst)
	sh.m[key] = lim
	return lim
}

// ---------------------------------------------------------------------------
// Daily AI token counter

type dayUsage struct {
	day  int64 // days since the Unix epoch (UTC)
	used int64
}

type dailyCounter struct {
	perShard int
	shards   [numShards]dayShard
}

type dayShard struct {
	mu      sync.Mutex
	m       map[string]dayUsage
	lastDay int64
}

func dayOf(t time.Time) int64 { return t.Unix() / 86400 }

func (c *dailyCounter) used(shard int, key string, now time.Time) int64 {
	sh := &c.shards[shard]
	day := dayOf(now)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	u, ok := sh.m[key]
	if !ok || u.day != day {
		return 0
	}
	return u.used
}

func (c *dailyCounter) add(shard int, key string, n int64, now time.Time) {
	sh := &c.shards[shard]
	day := dayOf(now)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if day != sh.lastDay {
		// New day: every older entry is stale.
		sh.lastDay = day
		for k, u := range sh.m {
			if u.day != day {
				delete(sh.m, k)
			}
		}
	}
	u := sh.m[key]
	if u.day != day {
		u = dayUsage{day: day}
	}
	if _, exists := sh.m[key]; !exists && len(sh.m) >= c.perShard {
		for k := range sh.m {
			if len(sh.m) < c.perShard {
				break
			}
			delete(sh.m, k)
		}
	}
	if u.used > math.MaxInt64-n {
		u.used = math.MaxInt64
	} else {
		u.used += n
	}
	sh.m[key] = u
}
