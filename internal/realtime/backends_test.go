package realtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/postgres"
	"github.com/thasami/backyard/internal/pubsub/redis"
)

// crossInstanceScenario exercises db events, rooms, presence, messages and
// disconnects between two hubs whose brokers share a real backend.
func crossInstanceScenario(t *testing.T, ba, bb pubsub.Broker) {
	a := newTestHub(t, ba, Options{InstanceID: "A"})
	b := newTestHub(t, bb, Options{InstanceID: "B"})
	c1 := dial(t, a, "alice@x")
	c2 := dial(t, b, "bob@x")

	c2.send(map[string]any{"t": "sub", "id": "s", "collection": "posts"})
	c2.waitFor("ack", nil)
	big := strings.Repeat("y", 100<<10) // exercises transport limits (NOTIFY spill)
	doc := testDoc("p1", map[string]any{"title": "hi", "body": big}, 1)
	if err := a.PublishDB(context.Background(), "site1", DBEvent{Collection: "posts", Op: OpCreate, Doc: doc}); err != nil {
		t.Fatal(err)
	}
	if f := c2.waitFor("db", nil); f.str("doc", "title") != "hi" || len(f.str("doc", "body")) != len(big) {
		t.Fatalf("db frame over backend: %.200v", f)
	}

	joinRoom(c1, "lobby", "1", map[string]any{"n": 1})
	j2 := joinRoom(c2, "lobby", "2", map[string]any{"n": 2})
	c2.knows(j2, c1.cid)
	c1.waitFor("presence", presence("join", c2.cid))

	c1.send(map[string]any{"t": "state", "room": "lobby", "state": map[string]any{"n": 3}})
	if f := c2.waitFor("presence", presence("update", c1.cid)); f.get("member", "state", "n") != 3.0 {
		t.Fatalf("update = %v", f)
	}
	c2.send(map[string]any{"t": "send", "room": "lobby", "data": map[string]any{"text": "hello"}})
	if f := c1.waitFor("msg", nil); f.str("data", "text") != "hello" || f.str("from", "cid") != c2.cid {
		t.Fatalf("msg = %v", f)
	}
	c1.send(map[string]any{"t": "send", "room": "lobby", "to": c2.cid, "data": "dm"})
	if f := c2.waitFor("msg", nil); f["data"] != "dm" {
		t.Fatalf("direct = %v", f)
	}
	c2.close()
	c1.waitFor("presence", presence("leave", c2.cid))
}

func TestCrossInstanceOverRedis(t *testing.T) {
	url := os.Getenv("BACKYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("BACKYARD_TEST_REDIS_URL not set")
	}
	prefix := "backyard-rt-test:" + randHex() + ":"
	open := func() pubsub.Broker {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		b, err := redis.New(ctx, redis.Options{URL: url, Prefix: prefix})
		if err != nil {
			t.Skipf("redis unavailable: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	crossInstanceScenario(t, open(), open())
}

func TestCrossInstanceOverPostgres(t *testing.T) {
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	schema := "backyard_rt_test_" + randHex()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if conn, err := pgx.Connect(ctx, dsn); err == nil {
			_, _ = conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			_ = conn.Close(ctx)
		}
	})
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	isolated := dsn + sep + "search_path=" + schema
	open := func() pubsub.Broker {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		b, err := postgres.New(ctx, postgres.Options{DSN: isolated, Channel: schema})
		if err != nil {
			t.Fatalf("postgres broker: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	crossInstanceScenario(t, open(), open())
}

func randHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
