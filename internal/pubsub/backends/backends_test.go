package backends_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/backends"
	"github.com/thasami/backyard/internal/pubsub/memory"
	"github.com/thasami/backyard/internal/pubsub/postgres"
	"github.com/thasami/backyard/internal/pubsub/redis"
)

func roundTrip(t *testing.T, b pubsub.Broker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := make(chan []byte, 1)
	stop, err := b.Subscribe(ctx, "backends-test", func(p []byte) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := b.Publish(ctx, "backends-test", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if string(p) != "hi" {
			t.Fatalf("got %q", p)
		}
	case <-ctx.Done():
		t.Fatal("message not delivered")
	}
}

func TestOpenMemory(t *testing.T) {
	for _, typ := range []string{"", "memory", "MEMORY"} {
		b, err := backends.Open(context.Background(), config.PubSubConfig{Type: typ})
		if err != nil {
			t.Fatalf("Open(%q): %v", typ, err)
		}
		if _, ok := b.(*memory.Broker); !ok {
			t.Fatalf("Open(%q) = %T, want *memory.Broker", typ, b)
		}
		roundTrip(t, b)
		_ = b.Close()
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	for name, cfg := range map[string]config.PubSubConfig{
		"unknown":         {Type: "kafka"},
		"redis no url":    {Type: "redis"},
		"postgres no dsn": {Type: "postgres"},
	} {
		if b, err := backends.Open(ctx, cfg); err == nil {
			_ = b.Close()
			t.Errorf("%s: Open succeeded", name)
		}
	}
}

func TestOpenRedis(t *testing.T) {
	url := os.Getenv("BACKYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("BACKYARD_TEST_REDIS_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := backends.Open(ctx, config.PubSubConfig{Type: "redis", Redis: config.RedisConfig{URL: url}})
	if err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	defer b.Close()
	if _, ok := b.(*redis.Broker); !ok {
		t.Fatalf("got %T", b)
	}
	roundTrip(t, b)
}

func TestOpenPostgres(t *testing.T) {
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Keep the spill table out of the shared public schema.
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer conn.Close(context.Background())
	schema := fmt.Sprintf("backyard_backends_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") }()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	b, err := backends.Open(ctx, config.PubSubConfig{Type: "postgres",
		Postgres: config.PostgresPubSub{DSN: dsn + sep + "search_path=" + schema, Channel: schema}})
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	defer b.Close()
	if _, ok := b.(*postgres.Broker); !ok {
		t.Fatalf("got %T", b)
	}
	roundTrip(t, b)
}
