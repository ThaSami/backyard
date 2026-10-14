package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/memory"
)

// payloadBroker records the last and the largest payload published on db
// topics.
type payloadBroker struct {
	pubsub.Broker
	mu     sync.Mutex
	maxDB  int
	lastDB []byte
}

func (b *payloadBroker) Publish(ctx context.Context, topic string, payload []byte) error {
	if strings.HasSuffix(topic, ".db") {
		b.mu.Lock()
		b.maxDB = max(b.maxDB, len(payload))
		b.lastDB = append([]byte(nil), payload...)
		b.mu.Unlock()
	}
	return b.Broker.Publish(ctx, topic, payload)
}

func (b *payloadBroker) last() (lastDB []byte, maxDB int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastDB, b.maxDB
}

// Database events of large documents still reach subscribers: documents are
// encoded without HTML escaping (markup does not grow sixfold), and a change
// whose document does not fit in one broker message is published without the
// document (doc: null) instead of being dropped.
func TestPublishDBLargeDocuments(t *testing.T) {
	b := &payloadBroker{Broker: memory.New()}
	t.Cleanup(func() { _ = b.Close() })
	a := newTestHub(t, b, Options{InstanceID: "A"})
	other := newTestHub(t, b, Options{InstanceID: "B"})
	ca := dial(t, a, "alice@x")
	cb := dial(t, other, "bob@x")
	for _, c := range []*client{ca, cb} {
		c.send(map[string]any{"t": "sub", "id": "s1", "collection": "posts"})
		c.waitFor("ack", hasID("s1"))
	}
	ctx := context.Background()

	// 400 KiB of markup: 2 MiB if '<', '>' and '&' were escaped as <….
	html := strings.Repeat("<p>a &amp; b</p>", (400<<10)/16)
	doc := testDoc("h1", map[string]any{"html": html}, 1)
	if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpCreate, Doc: doc}); err != nil {
		t.Fatalf("PublishDB(400 KiB of HTML): %v", err)
	}
	for _, c := range []*client{ca, cb} {
		f := c.waitFor("db", nil)
		if f.str("op") != "create" || f.str("doc_id") != "h1" || f.str("doc", "id") != "h1" || f.str("doc", "html") != html {
			t.Fatalf("HTML document event = %.300v", f)
		}
	}
	if payload, _ := b.last(); len(payload) > len(html)+1024 || !strings.Contains(string(payload), "<p>a &amp; b</p>") {
		t.Errorf("db event payload is %d bytes for %d bytes of HTML (escaped?)", len(payload), len(html))
	}

	// A document larger than a broker message: the event carries doc: null.
	big := strings.Repeat("x", pubsub.MaxPayloadBytes)
	doc = testDoc("b1", map[string]any{"body": big}, 3)
	if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpUpdate, Doc: doc}); err != nil {
		t.Fatalf("PublishDB(1 MiB document): %v", err)
	}
	for _, c := range []*client{ca, cb} {
		f := c.waitFor("db", nil)
		if f["sub"] != "s1" || f.str("collection") != "posts" || f.str("op") != "update" || f.str("doc_id") != "b1" {
			t.Fatalf("oversized document event = %.300v", f)
		}
		if v, ok := f["doc"]; !ok || v != nil {
			t.Fatalf("oversized document event must carry doc:null, got %.300v", f)
		}
	}
	if _, maxDB := b.last(); maxDB > pubsub.MaxPayloadBytes {
		t.Errorf("published a %d-byte db event (broker limit %d)", maxDB, pubsub.MaxPayloadBytes)
	}

	// Events of small documents are unaffected.
	doc = testDoc("p1", map[string]any{"title": "<b>hi</b>"}, 1)
	if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpCreate, Doc: doc}); err != nil {
		t.Fatal(err)
	}
	if f := cb.waitFor("db", nil); f.str("doc", "title") != "<b>hi</b>" || f.get("doc", "_meta", "version") != 1.0 {
		t.Fatalf("small document event = %v", f)
	}
}

// A subscription is counted (Stats().Subs) before its ack is queued, so a
// client that has received the ack never sees a stale count.
func TestSubCountedBeforeAck(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs two Ps to observe the ack while handleSub runs")
	}
	const n = 300
	hi := New(nil, Options{Logger: testLogger(t), Config: config.RealtimeConfig{MaxSubsPerConn: n}})
	t.Cleanup(func() { _ = hi.Close() })
	h := hi.(*hub)
	s, refused := h.admit("site1", "alice@x")
	if refused != "" {
		t.Fatalf("admit: %s", refused)
	}
	c := h.newConn(nil, s, identityFor("alice@x"))
	t.Cleanup(func() {
		c.cleanup()
		h.leaveHub(nil, s, "alice@x")
	})
	for i := range n {
		id := json.RawMessage(fmt.Sprintf(`"s%d"`, i))
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.handleSub(&inFrame{T: frameSub, Collection: "posts"}, id)
		}()
		// Act like the client: as soon as the ack is out, look at the stats.
		for c.out.peek() == nil {
			runtime.Gosched()
		}
		got := h.Stats().Subs
		<-done
		if f := c.out.peek(); string(f) != `{"t":"ack","id":`+string(id)+`}` {
			t.Fatalf("queued frame = %s, want the ack of %s", f, id)
		}
		c.out.pop()
		if got != i+1 {
			t.Fatalf("Stats().Subs = %d right after the ack of subscription %d, want %d", got, i+1, i+1)
		}
	}
}
