package postgres_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/postgres"
	"github.com/thasami/backyard/internal/pubsub/pubsubtest"
)

// testDSN returns BACKYARD_TEST_POSTGRES_DSN or skips the test.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	_ = conn.Close(ctx)
	return dsn
}

func randomName(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// isolatedDSN creates a unique schema (dropped after the test) and returns a
// DSN whose search_path points at it, so the spill table is private to the
// test.
func isolatedDSN(t *testing.T) (dsn, schema string) {
	t.Helper()
	base := testDSN(t)
	schema = randomName("backyard_pubsub_test_")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	if strings.Contains(base, "://") {
		sep := "?"
		if strings.Contains(base, "?") {
			sep = "&"
		}
		return base + sep + "search_path=" + schema, schema
	}
	return base + " search_path=" + schema, schema
}

func open(t *testing.T, opts postgres.Options) *postgres.Broker {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b, err := postgres.New(ctx, opts)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestConformance(t *testing.T) {
	dsn, _ := isolatedDSN(t)
	channel := randomName("backyard_test_")
	pubsubtest.Run(t, func(t *testing.T) pubsub.Broker {
		return open(t, postgres.Options{DSN: dsn, Channel: channel})
	})
}

func TestChannelIsolation(t *testing.T) {
	dsn, _ := isolatedDSN(t)
	a := open(t, postgres.Options{DSN: dsn, Channel: randomName("backyard_test_")})
	b := open(t, postgres.Options{DSN: dsn, Channel: randomName("backyard_test_")})
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
		t.Fatalf("broker on another channel received %q", p)
	case <-time.After(300 * time.Millisecond):
	}
}

func spillRows(t *testing.T, dsn string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+postgres.SpillTable).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSpillAndJanitor(t *testing.T) {
	dsn, _ := isolatedDSN(t)
	channel := randomName("backyard_test_")
	b := open(t, postgres.Options{
		DSN: dsn, Channel: channel,
		SpillTTL: 500 * time.Millisecond, JanitorInterval: 100 * time.Millisecond,
	})
	ctx, cancelCtx := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelCtx()
	got := make(chan []byte, 4)
	cancel, err := b.Subscribe(ctx, "big", func(p []byte) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	// Just below and just above the inline limit (base64 grows by 4/3).
	for _, size := range []int{5000, 6000, 100 << 10} {
		payload := make([]byte, size)
		_, _ = rand.Read(payload)
		before := spillRows(t, dsn)
		if err := b.Publish(ctx, "big", payload); err != nil {
			t.Fatal(err)
		}
		select {
		case p := <-got:
			if string(p) != string(payload) {
				t.Fatalf("size %d: payload mismatch", size)
			}
		case <-ctx.Done():
			t.Fatalf("size %d: not delivered", size)
		}
		spilled := spillRows(t, dsn) > before
		if want := size > 5500; spilled != want {
			// The janitor may already have removed the row; only a missing
			// spill for a large payload within the TTL is an error.
			if want {
				t.Logf("size %d: spill row already purged (rows=%d)", size, spillRows(t, dsn))
			} else {
				t.Fatalf("size %d: unexpectedly spilled", size)
			}
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for spillRows(t, dsn) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("janitor did not delete expired spill rows")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestReconnectAfterBackendTerminated(t *testing.T) {
	dsn, _ := isolatedDSN(t)
	channel := randomName("backyard_test_")
	b := open(t, postgres.Options{DSN: dsn, Channel: channel})
	pub := open(t, postgres.Options{DSN: dsn, Channel: channel})
	ctx, cancelCtx := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelCtx()
	got := make(chan string, 256)
	cancel, err := b.Subscribe(ctx, "reconnect", func(p []byte) { got <- string(p) })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()

	conn, err := pgx.Connect(ctx, testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var killed int
	err = conn.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE query = $1`, "LISTEN "+pgx.Identifier{channel}.Sanitize()).Scan(&killed)
	if err != nil {
		t.Fatal(err)
	}
	if killed == 0 {
		t.Fatal("no listener backend found to terminate")
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := pub.Publish(ctx, "reconnect", []byte("ping")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-got:
			// Both brokers' listeners were terminated; both recover.
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatal("listener did not recover after its backend was terminated")
}

func TestIdleHealthCheck(t *testing.T) {
	dsn, _ := isolatedDSN(t)
	channel := randomName("backyard_test_")
	b := open(t, postgres.Options{DSN: dsn, Channel: channel, HealthInterval: 50 * time.Millisecond})
	ctx, cancelCtx := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelCtx()
	got := make(chan string, 8)
	cancel, err := b.Subscribe(ctx, "idle", func(p []byte) { got <- string(p) })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for i := range 3 {
		// Let several health checks run between messages.
		time.Sleep(250 * time.Millisecond)
		if err := b.Publish(ctx, "idle", []byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
		select {
		case p := <-got:
			if p != string(rune('a'+i)) {
				t.Fatalf("got %q", p)
			}
		case <-ctx.Done():
			t.Fatalf("message %d not delivered after idle period", i)
		}
	}
}

func TestNewErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := postgres.New(ctx, postgres.Options{}); err == nil {
		t.Error("New without DSN succeeded")
	}
	if _, err := postgres.New(ctx, postgres.Options{DSN: "postgres://x@127.0.0.1:1/db", Channel: strings.Repeat("c", 64)}); err == nil {
		t.Error("New with a 64-byte channel succeeded")
	}
	_, err := postgres.New(ctx, postgres.Options{DSN: "postgres://user:hunter2@127.0.0.1:1/db?connect_timeout=1"})
	if err == nil {
		t.Fatal("New against unreachable server succeeded")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error leaks password: %v", err)
	}
	if errors.Is(err, pubsub.ErrClosed) {
		t.Errorf("unexpected error kind: %v", err)
	}
}
