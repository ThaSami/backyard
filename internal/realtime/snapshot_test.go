package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/memory"
)

// recordingBroker records every payload published on room topics and
// counts all publishes.
type recordingBroker struct {
	pubsub.Broker
	mu    sync.Mutex
	pubs  []gossip
	max   int
	total int
}

func (r *recordingBroker) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

func (r *recordingBroker) Publish(ctx context.Context, topic string, payload []byte) error {
	r.mu.Lock()
	r.total++
	r.mu.Unlock()
	if strings.Contains(topic, ".room.") {
		var g gossip
		if err := json.Unmarshal(payload, &g); err == nil {
			r.mu.Lock()
			r.pubs = append(r.pubs, g)
			r.max = max(r.max, len(payload))
			r.mu.Unlock()
		}
	}
	return r.Broker.Publish(ctx, topic, payload)
}

func (r *recordingBroker) snapshots() []gossip {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []gossip
	for _, g := range r.pubs {
		if g.K == gossipMembers {
			out = append(out, g)
		}
	}
	return out
}

func bigMembers(n, stateBytes int) []member {
	ms := make([]member, n)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range ms {
		ms[i] = member{
			CID:      fmt.Sprintf("c_%040x", i+1),
			User:     identityFor(fmt.Sprintf("user%d@x", i)).Summary(),
			State:    json.RawMessage(`{"pad":"` + strings.Repeat("p", stateBytes) + `"}`),
			JoinedAt: at.Add(time.Duration(i) * time.Millisecond),
		}
	}
	return ms
}

func TestSplitMembers(t *testing.T) {
	if parts := splitMembers(nil, 100); len(parts) != 1 || len(parts[0]) != 0 {
		t.Fatalf("empty: %v", parts)
	}
	ms := bigMembers(10, 100)
	one := len(mustJSON(ms[0])) + 1
	for _, tc := range []struct {
		budget, wantParts int
	}{
		{one * 10, 1},
		{one * 5, 2},
		{one*3 + 1, 4},
		{1, 10}, // every member alone, even if larger than the budget
	} {
		parts := splitMembers(ms, tc.budget)
		if len(parts) != tc.wantParts {
			t.Errorf("budget %d: %d parts, want %d", tc.budget, len(parts), tc.wantParts)
		}
		var n int
		for _, p := range parts {
			n += len(p)
		}
		if n != len(ms) {
			t.Errorf("budget %d: %d members in parts, want %d", tc.budget, n, len(ms))
		}
	}
}

// largeRoom sets up instance A with a real member plus n fake local members
// (state of stateBytes) in room "lobby", then joins the room from instance B
// and waits until B's client has seen every member. It returns A's
// recording broker and B.
func largeRoom(t *testing.T, n, stateBytes int) (*recordingBroker, *testHub) {
	t.Helper()
	root := memory.New()
	t.Cleanup(func() { _ = root.Close() })
	rb := &recordingBroker{Broker: root.NewPeer()}
	a := newTestHub(t, rb, Options{InstanceID: "A"})
	b := newTestHub(t, root.NewPeer(), Options{InstanceID: "B"})
	ah := a.Hub.(*hub)

	// A has a local member so that it answers B's sync request.
	c1 := dial(t, a, "alice@x")
	joinRoom(c1, "lobby", "1", nil)

	fake := bigMembers(n, stateBytes)
	sink := &conn{} // a connection that discards everything
	sink.closing.Store(true)
	s := ah.site("site1")
	s.mu.Lock()
	r := s.rooms["lobby"]
	for _, m := range fake {
		r.local[m.CID] = &localMember{c: sink, m: m}
	}
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		for _, m := range fake {
			delete(r.local, m.CID)
		}
		s.mu.Unlock()
	})

	c2 := dial(t, b, "bob@x")
	joinRoom(c2, "lobby", "2", nil)
	seen := map[string]bool{}
	for len(seen) < len(fake)+1 {
		f := c2.waitFor("presence", func(f frame) bool { return f.str("op") == "join" })
		seen[f.str("member", "cid")] = true
	}
	if !seen[c1.cid] {
		t.Fatal("c1 missing")
	}
	if st := b.Stats(); st.Connections != 1 {
		t.Fatalf("joining client was dropped: %+v", st)
	}
	return rb, b
}

// TestLargeRoomSnapshotIsSplit: a room whose member list exceeds the broker's
// payload limit (400 members of ~7 KiB state ≈ 2.9 MB) is announced in parts
// that each fit, and another instance reassembles it.
func TestLargeRoomSnapshotIsSplit(t *testing.T) {
	rb, _ := largeRoom(t, 400, 7000)
	snaps := rb.snapshots()
	if len(snaps) < 2 {
		t.Fatalf("expected a split snapshot, got %d messages", len(snaps))
	}
	for _, g := range snaps {
		if g.N < 2 || g.P < 1 || g.P > g.N || g.S != snaps[0].S {
			t.Fatalf("bad part: p=%d n=%d s=%d", g.P, g.N, g.S)
		}
	}
	rb.mu.Lock()
	biggest := rb.max
	rb.mu.Unlock()
	if biggest > pubsub.MaxPayloadBytes {
		t.Fatalf("published %d bytes (> %d)", biggest, pubsub.MaxPayloadBytes)
	}
}

// TestLargeRoomSyncBurst: the first local joiner of a big room receives one
// presence event per remote member at once; that burst must not get it
// disconnected as a slow consumer (which, on reconnect, would repeat).
func TestLargeRoomSyncBurst(t *testing.T) {
	rb, _ := largeRoom(t, 1500, 10)
	burst := 0
	for _, g := range rb.snapshots() {
		burst = max(burst, len(g.Ms))
	}
	if burst <= 256 {
		t.Fatalf("test does not exercise a burst (largest part: %d members)", burst)
	}
}

// TestSplitSnapshotDepartures: departures implied by a split snapshot are
// applied only once all of its parts arrived; parts of a superseded
// snapshot never remove anyone.
func TestSplitSnapshotDepartures(t *testing.T) {
	a, _ := twoInstances(t, nil)
	ah := a.Hub.(*hub)
	c1 := dial(t, a, "alice@x")
	joinRoom(c1, "lobby", "1", nil)
	inject := func(g gossip) { ah.onRoomMessage("site1", "lobby", mustJSON(g)) }
	ms := bigMembers(4, 10)
	x, y, z, w := ms[0], ms[1], ms[2], ms[3]

	inject(gossip{K: gossipMembers, I: "Z", S: 10, Ms: []member{x, y, z}})
	for _, m := range []member{x, y, z} {
		c1.waitFor("presence", presence("join", m.CID))
	}

	// Snapshot 11 in two parts lists x and w: y and z are gone, but only
	// once part 2 arrived (parts may come in any order).
	inject(gossip{K: gossipMembers, I: "Z", S: 11, Ms: []member{w}, P: 2, N: 2})
	c1.waitFor("presence", presence("join", w.CID))
	c1.expectNone("presence", func(f frame) bool { return f.str("op") == "leave" }, 50*time.Millisecond)
	inject(gossip{K: gossipMembers, I: "Z", S: 11, Ms: []member{x}, P: 1, N: 2})
	c1.waitFor("presence", presence("leave", y.CID))
	c1.waitFor("presence", presence("leave", z.CID))
	c1.expectNone("presence", presence("leave", x.CID), 50*time.Millisecond)
	c1.expectNone("presence", presence("leave", w.CID), 0)

	// An incomplete snapshot 12 superseded by complete snapshot 13: the
	// late part of 12 removes nobody.
	inject(gossip{K: gossipMembers, I: "Z", S: 12, Ms: []member{x}, P: 1, N: 3})
	inject(gossip{K: gossipMembers, I: "Z", S: 13, Ms: []member{x, w}})
	inject(gossip{K: gossipMembers, I: "Z", S: 12, Ms: nil, P: 2, N: 3})
	inject(gossip{K: gossipMembers, I: "Z", S: 12, Ms: nil, P: 3, N: 3})
	c1.expectNone("presence", func(f frame) bool { return f.str("op") == "leave" }, 50*time.Millisecond)

	// Invalid part numbers are ignored for departures.
	inject(gossip{K: gossipMembers, I: "Z", S: 14, Ms: nil, P: 3, N: 2})
	inject(gossip{K: gossipMembers, I: "Z", S: 15, Ms: nil, P: 1, N: maxSnapshotParts + 1})
	c1.expectNone("presence", func(f frame) bool { return f.str("op") == "leave" }, 50*time.Millisecond)

	// Stale partial snapshots are forgotten by expiry.
	inject(gossip{K: gossipMembers, I: "Z", S: 16, Ms: []member{x, w}, P: 1, N: 2})
	s := ah.site("site1")
	s.mu.Lock()
	n := len(s.rooms["lobby"].partial)
	s.mu.Unlock()
	if n != 1 {
		t.Fatalf("partial snapshots = %d", n)
	}
	ah.expire(time.Now().Add(2 * ah.opts.MemberTTL))
	s.mu.Lock()
	n = len(s.rooms["lobby"].partial)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("partial snapshots after expiry = %d", n)
	}
}

// TestSendQueueBytes: the per-connection byte bound disconnects a client
// that does not read, even when few frames are queued.
func TestSendQueueBytes(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) {
		o.SendQueueBytes = 256 << 10
		o.WriteTimeout = 200 * time.Millisecond
	})
	ws, _, err := dialRaw(t, a, "slow@x", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"t":"sub","id":"s","collection":"posts"}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "subscribed", func() bool { return a.Stats().Subs == 1 })
	big := map[string]any{"blob": strings.Repeat("z", 100<<10)}
	for i := 0; i < 50 && a.Stats().Connections > 0; i++ {
		if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpCreate, Doc: testDoc("d", big, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "slow consumer dropped", func() bool { return a.Stats().Connections == 0 })
}

func TestOutQueue(t *testing.T) {
	q := outQueue{maxFrames: 3, maxBytes: 10, wake: make(chan struct{}, 1)}
	if q.peek() != nil {
		t.Fatal("empty queue peek")
	}
	q.pop() // no-op
	if !q.push([]byte("0123456789abc")) {
		t.Fatal("oversized frame rejected by an empty queue")
	}
	if q.push([]byte("x")) {
		t.Fatal("byte bound not enforced")
	}
	if string(q.peek()) != "0123456789abc" {
		t.Fatal("peek")
	}
	q.pop()
	if q.bytes != 0 || q.frames != nil {
		t.Fatalf("after pop: bytes=%d frames=%v", q.bytes, q.frames)
	}
	for i := range 3 {
		if !q.push([]byte{byte('a' + i)}) {
			t.Fatalf("push %d", i)
		}
	}
	if q.push([]byte("d")) {
		t.Fatal("frame bound not enforced")
	}
	select {
	case <-q.wake:
	default:
		t.Fatal("no wake-up")
	}
	for _, want := range []string{"a", "b", "c"} {
		if got := string(q.peek()); got != want {
			t.Fatalf("got %q want %q", got, want)
		}
		q.pop()
	}
}

// TestHandlerPanicCleansUp: a panic while handling a client's frame (here
// provoked through the OnFrame hook) tears the connection down like a
// disconnect: its rooms are left and its slots released.
func TestHandlerPanicCleansUp(t *testing.T) {
	var armed atomic.Bool
	a, _ := twoInstances(t, func(o *Options) {
		o.OnFrame = func(dir string) {
			if dir == "in" && armed.Load() {
				armed.Store(false)
				panic("boom")
			}
		}
	})
	c1, c2 := dial(t, a, "alice@x"), dial(t, a, "bob@x")
	joinRoom(c1, "lobby", "1", nil)
	joinRoom(c2, "lobby", "2", nil)
	c1.waitFor("presence", presence("join", c2.cid))

	armed.Store(true)
	c2.send(map[string]any{"t": "ping"})
	c1.waitFor("presence", presence("leave", c2.cid))
	c2.waitClosed()
	eventually(t, "connection released", func() bool { return a.Stats().Connections == 1 })
	c1.send(map[string]any{"t": "ping"})
	c1.waitFor("pong", nil)
}
