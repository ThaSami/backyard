package ratelimit

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
)

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestAllowBurstThenRefill(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	l := NewWithLimits(Limits{PerMinute: map[Category]int{API: 6}}, false, WithClock(clk.Now))

	for i := range 6 {
		if ok, _ := l.Allow(API, "u:a"); !ok {
			t.Fatalf("request %d rejected within burst", i)
		}
	}
	ok, retry := l.Allow(API, "u:a")
	if ok {
		t.Fatal("request beyond burst allowed")
	}
	// 6/min = one token every 10s.
	if retry <= 0 || retry > 10*time.Second {
		t.Fatalf("retry = %v, want (0, 10s]", retry)
	}
	if got := RetryAfterSeconds(retry); got != 10 {
		t.Fatalf("RetryAfterSeconds = %d, want 10", got)
	}
	// A rejected request must not consume a token.
	clk.Advance(10 * time.Second)
	if ok, _ := l.Allow(API, "u:a"); !ok {
		t.Fatal("request after refill rejected")
	}
	if ok, _ := l.Allow(API, "u:a"); ok {
		t.Fatal("second request after a single refill allowed")
	}
	// Other keys and categories are independent.
	if ok, _ := l.Allow(API, "u:b"); !ok {
		t.Fatal("other key limited")
	}
	if ok, _ := l.Allow(DBWrite, "u:a"); !ok {
		t.Fatal("unconfigured category limited")
	}
}

func TestDisabledAndUnlimited(t *testing.T) {
	tests := []struct {
		name     string
		limits   Limits
		disabled bool
	}{
		{"disabled", Limits{PerMinute: map[Category]int{API: 1}, AITokensPerDay: 1}, true},
		{"zero limits", Limits{PerMinute: map[Category]int{API: 0}, AITokensPerDay: 0}, false},
		{"negative limits", Limits{PerMinute: map[Category]int{API: -1}, AITokensPerDay: -1}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := NewWithLimits(tc.limits, tc.disabled)
			for range 100 {
				if ok, _ := l.Allow(API, "k"); !ok {
					t.Fatal("limited")
				}
			}
			l.AddTokens("k", 1000)
			if ok, _ := l.CheckTokens("k"); !ok {
				t.Fatal("token budget enforced")
			}
		})
	}
	var nilLimiter *Limiter
	if ok, _ := nilLimiter.Allow(API, "k"); !ok || nilLimiter.Enabled() {
		t.Fatal("nil limiter must allow everything")
	}
}

func TestFromConfig(t *testing.T) {
	cfg := config.RateLimitConfig{APIPerMinute: 1, DBWritesPerMinute: 2, AIPerMinute: 3, WarehousePerMinute: 4, UploadsPerMinute: 5, AITokensPerDay: 6}
	l := New(cfg)
	want := map[Category]int{API: 1, DBWrite: 2, AI: 3, Warehouse: 4, Upload: 5}
	for _, c := range Categories {
		if got := l.Limit(c); got != want[c] {
			t.Errorf("Limit(%s) = %d, want %d", c, got, want[c])
		}
	}
	cfg.Disabled = true
	if New(cfg).Enabled() {
		t.Fatal("disabled config produced an enabled limiter")
	}
}

func TestDailyTokens(t *testing.T) {
	clk := newClock(time.Date(2026, 5, 6, 23, 0, 0, 0, time.UTC))
	l := NewWithLimits(Limits{AITokensPerDay: 100}, false, WithClock(clk.Now))

	if ok, _ := l.CheckTokens("u:a"); !ok {
		t.Fatal("fresh key over budget")
	}
	l.AddTokens("u:a", 60)
	if ok, _ := l.CheckTokens("u:a"); !ok {
		t.Fatal("under budget rejected")
	}
	l.AddTokens("u:a", 40)
	ok, retry := l.CheckTokens("u:a")
	if ok {
		t.Fatal("exhausted budget allowed")
	}
	if retry != time.Hour {
		t.Fatalf("retry = %v, want 1h (until midnight UTC)", retry)
	}
	if got := l.TokensUsed("u:a"); got != 100 {
		t.Fatalf("TokensUsed = %d", got)
	}
	if ok, _ := l.CheckTokens("u:b"); !ok {
		t.Fatal("other key affected")
	}
	// Ignored inputs.
	l.AddTokens("u:b", -5)
	l.AddTokens("u:b", 0)
	if got := l.TokensUsed("u:b"); got != 0 {
		t.Fatalf("TokensUsed(u:b) = %d", got)
	}
	// Midnight resets.
	clk.Advance(time.Hour)
	if ok, _ := l.CheckTokens("u:a"); !ok {
		t.Fatal("budget not reset at midnight")
	}
	if got := l.TokensUsed("u:a"); got != 0 {
		t.Fatalf("TokensUsed after reset = %d", got)
	}
	l.AddTokens("u:a", 1)
	if got := l.TokensUsed("u:a"); got != 1 {
		t.Fatalf("TokensUsed = %d, want 1", got)
	}
}

func TestEviction(t *testing.T) {
	clk := newClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	l := NewWithLimits(Limits{PerMinute: map[Category]int{API: 60}}, false, WithClock(clk.Now))
	for i := range 1000 {
		l.Allow(API, fmt.Sprintf("k%d", i))
	}
	if got := l.Tracked(API); got != 1000 {
		t.Fatalf("Tracked = %d, want 1000", got)
	}
	// After the buckets refill (1s for one token at 60/min) and the sweep
	// interval passes, new traffic evicts the full buckets.
	clk.Advance(2 * time.Minute)
	for i := range 2 * numShards * 4 {
		l.Allow(API, fmt.Sprintf("new%d", i))
	}
	if got := l.Tracked(API); got > 2*numShards*4 {
		t.Fatalf("Tracked = %d after sweep, want <= %d", got, 2*numShards*4)
	}
}

func TestMaxKeysBound(t *testing.T) {
	l := NewWithLimits(Limits{PerMinute: map[Category]int{API: 1}, AITokensPerDay: 10}, false, WithMaxKeys(numShards*2))
	for i := range 5000 {
		k := fmt.Sprintf("k%d", i)
		l.Allow(API, k)
		l.AddTokens(k, 1)
	}
	if got := l.Tracked(API); got > numShards*2 {
		t.Fatalf("Tracked = %d, want <= %d", got, numShards*2)
	}
	n := 0
	for i := range l.tokens.shards {
		n += len(l.tokens.shards[i].m)
	}
	if n > numShards*2 {
		t.Fatalf("daily counter tracks %d keys, want <= %d", n, numShards*2)
	}
}

func TestKey(t *testing.T) {
	tests := []struct {
		id   string
		anon bool
		ip   string
		want string
	}{
		{"alice@acme.com", false, "10.0.0.1", "u:alice@acme.com"},
		{"anon:abcd", true, "10.0.0.1", "ip:10.0.0.1"},
		{"", false, "::1", "ip:::1"},
	}
	for _, tc := range tests {
		if got := Key(tc.id, tc.anon, tc.ip); got != tc.want {
			t.Errorf("Key(%q,%v,%q) = %q, want %q", tc.id, tc.anon, tc.ip, got, tc.want)
		}
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want int
	}{
		{0, 1}, {-time.Second, 1}, {time.Millisecond, 1}, {time.Second, 1},
		{1500 * time.Millisecond, 2}, {59 * time.Second, 59},
	}
	for _, tc := range tests {
		if got := RetryAfterSeconds(tc.d); got != tc.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}

func TestConcurrentAllow(t *testing.T) {
	l := NewWithLimits(Limits{PerMinute: map[Category]int{API: 100}}, false)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				if ok, _ := l.Allow(API, "shared"); ok {
					allowed.Add(1)
				}
				l.AddTokens("shared", 1)
			}
		}()
	}
	wg.Wait()
	// 100 burst plus at most a few tokens refilled during the test.
	if got := allowed.Load(); got < 100 || got > 110 {
		t.Fatalf("allowed = %d, want ~100", got)
	}
}
