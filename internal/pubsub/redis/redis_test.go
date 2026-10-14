package redis_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/pubsubtest"
	"github.com/thasami/backyard/internal/pubsub/redis"
)

// testURL returns BACKYARD_TEST_REDIS_URL or skips the test.
func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("BACKYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("BACKYARD_TEST_REDIS_URL not set")
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		t.Fatalf("BACKYARD_TEST_REDIS_URL: %v", err)
	}
	c := goredis.NewClient(opt)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	return url
}

func uniquePrefix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "backyard-test:" + hex.EncodeToString(b[:]) + ":"
}

func open(t *testing.T, opts redis.Options) *redis.Broker {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := redis.New(ctx, opts)
	if err != nil {
		t.Fatalf("redis.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestConformance(t *testing.T) {
	url := testURL(t)
	prefix := uniquePrefix()
	pubsubtest.Run(t, func(t *testing.T) pubsub.Broker {
		return open(t, redis.Options{URL: url, Prefix: prefix})
	})
}

func TestPrefixIsolation(t *testing.T) {
	url := testURL(t)
	a := open(t, redis.Options{URL: url, Prefix: uniquePrefix()})
	b := open(t, redis.Options{URL: url, Prefix: uniquePrefix()})
	ctx := context.Background()
	got := make(chan []byte, 1)
	cancel, err := b.Subscribe(ctx, "topic", func(p []byte) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := a.Publish(ctx, "topic", []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		t.Fatalf("broker with a different prefix received %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestUsesPrefixOnTheWire(t *testing.T) {
	url := testURL(t)
	prefix := uniquePrefix()
	b := open(t, redis.Options{URL: url, Prefix: prefix})
	opt, _ := goredis.ParseURL(url)
	raw := goredis.NewClient(opt)
	defer raw.Close()
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()

	got := make(chan []byte, 1)
	cancel, err := b.Subscribe(ctx, "rt.site.db", func(p []byte) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// A plain PUBLISH on the prefixed channel reaches the subscriber.
	if err := raw.Publish(ctx, prefix+"rt.site.db", "external").Err(); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if string(p) != "external" {
			t.Fatalf("got %q", p)
		}
	case <-ctx.Done():
		t.Fatal("message published with raw client not received")
	}
	// The channel is unsubscribed on the server once the last local
	// subscriber cancels.
	n, err := raw.PubSubNumSub(ctx, prefix+"rt.site.db").Result()
	if err != nil {
		t.Fatal(err)
	}
	if n[prefix+"rt.site.db"] != 1 {
		t.Fatalf("NUMSUB = %d, want 1", n[prefix+"rt.site.db"])
	}
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := raw.PubSubNumSub(ctx, prefix+"rt.site.db").Result()
		if err != nil {
			t.Fatal(err)
		}
		if n[prefix+"rt.site.db"] == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("channel still subscribed after cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestResubscribesAfterConnectionKilled(t *testing.T) {
	url := testURL(t)
	prefix := uniquePrefix()
	// Name the subscriber's connections so that only they are killed (the
	// Redis server may be shared with other tests).
	name := "backyard-test-" + strings.Trim(strings.TrimPrefix(prefix, "backyard-test:"), ":")
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	b := open(t, redis.Options{URL: url + sep + "client_name=" + name, Prefix: prefix})
	pub := open(t, redis.Options{URL: url, Prefix: prefix})
	opt, _ := goredis.ParseURL(url)
	raw := goredis.NewClient(opt)
	defer raw.Close()
	ctx, cancelCtx := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCtx()

	got := make(chan string, 64)
	cancel, err := b.Subscribe(ctx, "reconnect", func(p []byte) { got <- string(p) })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	// Kill every pub/sub connection subscribed to our channel.
	clients, err := raw.ClientList(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	killed := 0
	for _, line := range strings.Split(clients, "\n") {
		if !strings.Contains(line, " name="+name+" ") || !strings.Contains(line, " sub=1 ") {
			continue
		}
		for _, f := range strings.Fields(line) {
			if id, ok := strings.CutPrefix(f, "id="); ok {
				if err := raw.Do(ctx, "CLIENT", "KILL", "ID", id).Err(); err == nil {
					killed++
				}
			}
		}
	}
	if killed == 0 {
		t.Skip("could not identify the pub/sub connection to kill")
	}

	// Keep publishing until the subscriber is back (go-redis health-checks
	// the connection and resubscribes).
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := pub.Publish(ctx, "reconnect", []byte("ping")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-got:
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("subscription did not recover after the connection was killed")
}

func TestNewErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := redis.New(ctx, redis.Options{}); err == nil {
		t.Error("New without URL succeeded")
	}
	_, err := redis.New(ctx, redis.Options{URL: "http://user:secret@example.com"})
	if err == nil {
		t.Fatal("New with invalid scheme succeeded")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks credentials: %v", err)
	}
	// Nothing listens on port 1.
	if _, err := redis.New(ctx, redis.Options{URL: "redis://127.0.0.1:1/0"}); err == nil {
		t.Error("New against unreachable server succeeded")
	}
}
