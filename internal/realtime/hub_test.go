package realtime

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/pubsub/memory"
)

// testWriter forwards log output to t.Log until the test finishes.
type testWriter struct {
	t    *testing.T
	mu   sync.Mutex
	done bool
}

func (w *testWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.done {
		w.t.Log(strings.TrimRight(string(p), "\n"))
	}
	return len(p), nil
}

func testLogger(t *testing.T) *slog.Logger {
	w := &testWriter{t: t}
	t.Cleanup(func() {
		w.mu.Lock()
		w.done = true
		w.mu.Unlock()
	})
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// twoInstances returns two hubs ("A" and "B") sharing one memory broker.
func twoInstances(t *testing.T, mod func(*Options)) (*testHub, *testHub) {
	t.Helper()
	b := memory.New()
	t.Cleanup(func() { _ = b.Close() })
	optsA := Options{InstanceID: "A"}
	optsB := Options{InstanceID: "B"}
	if mod != nil {
		mod(&optsA)
		mod(&optsB)
	}
	return newTestHub(t, b, optsA), newTestHub(t, b, optsB)
}

func testDoc(id string, data map[string]any, version int64) *docstore.Doc {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return &docstore.Doc{
		Site: "site1", Collection: "posts", ID: id, Data: data,
		CreatedAt: ts, UpdatedAt: ts, CreatedBy: "alice@x", UpdatedBy: "alice@x", Version: version,
	}
}

func TestHello(t *testing.T) {
	a, _ := twoInstances(t, nil)
	c := dial(t, a, "alice@x")
	if !strings.HasPrefix(c.cid, "c_") || len(c.cid) < 10 {
		t.Fatalf("cid = %q", c.cid)
	}
	if got := c.hello.str("user", "id"); got != "alice@x" {
		t.Fatalf("hello user id = %q", got)
	}
	if got := c.hello.str("user", "name"); got != "Name of alice@x" {
		t.Fatalf("hello user name = %q", got)
	}
	if groups, _ := c.hello.get("user", "groups").([]any); len(groups) != 1 {
		t.Fatalf("hello carries the full user: %v", c.hello)
	}
	c2 := dial(t, a, "alice@x")
	if c2.cid == c.cid {
		t.Fatal("connection ids are not unique")
	}
	c.send(map[string]any{"t": "ping"})
	c.waitFor("pong", nil)
}

func TestDBSubscriptionsAcrossInstances(t *testing.T) {
	a, b := twoInstances(t, nil)
	ca := dial(t, a, "alice@x")
	cb := dial(t, b, "bob@x")
	other := dialWith(t, b, "user=carol@x&site=othersite", nil)

	ca.send(map[string]any{"t": "sub", "id": "s1", "collection": "posts"})
	ca.waitFor("ack", hasID("s1"))
	cb.send(map[string]any{"t": "sub", "id": 7, "collection": "posts"})
	cb.waitFor("ack", hasID(7))
	cb.send(map[string]any{"t": "sub", "id": "c", "collection": "comments"})
	cb.waitFor("ack", hasID("c"))
	other.send(map[string]any{"t": "sub", "id": "o", "collection": "posts"})
	other.waitFor("ack", hasID("o"))

	if st := a.Stats(); st.Subs != 1 || st.Connections != 1 {
		t.Fatalf("A stats = %+v", st)
	}
	if st := b.Stats(); st.Subs != 3 || st.Connections != 2 {
		t.Fatalf("B stats = %+v", st)
	}

	ctx := context.Background()
	doc := testDoc("p1", map[string]any{"title": "Hello", "votes": 3.0}, 1)
	if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpCreate, DocID: "p1", Doc: doc}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		c   *client
		sub any
	}{{ca, "s1"}, {cb, 7.0}} {
		f := tc.c.waitFor("db", nil)
		if f["sub"] != tc.sub || f.str("collection") != "posts" || f.str("op") != "create" || f.str("doc_id") != "p1" {
			t.Fatalf("db frame = %v", f)
		}
		if f.str("doc", "id") != "p1" || f.str("doc", "title") != "Hello" || f.get("doc", "votes") != 3.0 {
			t.Fatalf("doc not flattened: %v", f)
		}
		if f.get("doc", "_meta", "version") != 1.0 || f.str("doc", "_meta", "created_by") != "alice@x" ||
			f.str("doc", "_meta", "created_at") != "2026-01-02T03:04:05Z" {
			t.Fatalf("doc _meta = %v", f.get("doc", "_meta"))
		}
	}
	cb.expectNone("db", hasSub("c"), 100*time.Millisecond)
	other.expectNone("db", nil, 50*time.Millisecond)

	// Unsubscribe on A; events published on B only reach B.
	ca.send(map[string]any{"t": "unsub", "id": "s1"})
	ca.waitFor("ack", hasID("s1"))
	doc2 := testDoc("p1", map[string]any{"title": "Edited"}, 2)
	if err := b.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpUpdate, Doc: doc2}); err != nil {
		t.Fatal(err)
	}
	f := cb.waitFor("db", nil)
	if f.str("op") != "update" || f.str("doc_id") != "p1" || f.str("doc", "title") != "Edited" {
		t.Fatalf("update frame = %v", f)
	}
	ca.expectNone("db", nil, 100*time.Millisecond)

	if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpDelete, DocID: "p1"}); err != nil {
		t.Fatal(err)
	}
	f = cb.waitFor("db", nil)
	if f.str("op") != "delete" || f.str("doc_id") != "p1" {
		t.Fatalf("delete frame = %v", f)
	}
	if v, ok := f["doc"]; !ok || v != nil {
		t.Fatalf("delete frame must carry doc:null, got %v", f)
	}

	// The comments subscription gets comment events.
	if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "comments", Op: OpCreate, Doc: testDoc("k1", nil, 1)}); err != nil {
		t.Fatal(err)
	}
	if f := cb.waitFor("db", hasSub("c")); f.str("collection") != "comments" {
		t.Fatalf("comment frame = %v", f)
	}

	// System collections are never published.
	if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "_files", Op: OpCreate, DocID: "f"}); err != nil {
		t.Fatalf("system collection: %v", err)
	}
	cb.expectNone("db", func(f frame) bool { return f.str("collection") == "_files" }, 50*time.Millisecond)
}

func hasSub(id any) func(frame) bool {
	return func(f frame) bool { return f["sub"] == id }
}

func TestPublishDBValidation(t *testing.T) {
	a, _ := twoInstances(t, nil)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		site string
		ev   DBEvent
	}{
		"bad site":       {"Bad Site", DBEvent{Collection: "posts", Op: OpCreate, DocID: "x"}},
		"bad collection": {"site1", DBEvent{Collection: "no spaces", Op: OpCreate, DocID: "x"}},
		"bad op":         {"site1", DBEvent{Collection: "posts", Op: "upsert", DocID: "x"}},
		"missing id":     {"site1", DBEvent{Collection: "posts", Op: OpDelete}},
	} {
		if err := a.PublishDB(ctx, tc.site, tc.ev); err == nil {
			t.Errorf("%s: PublishDB succeeded", name)
		}
	}
	// System data is skipped silently (nothing could subscribe to it).
	rb := &recordingBroker{Broker: memory.New()}
	defer rb.Close()
	h := New(rb, Options{})
	defer h.Close()
	for _, tc := range []struct {
		site, collection string
	}{{"_system", "_users"}, {"_system", "posts"}, {"site1", "_files"}} {
		if err := h.PublishDB(ctx, tc.site, DBEvent{Collection: tc.collection, Op: OpCreate, DocID: "x"}); err != nil {
			t.Errorf("%s/%s: %v", tc.site, tc.collection, err)
		}
	}
	if n := rb.count(); n != 0 {
		t.Errorf("system events published: %d", n)
	}
}

func TestSubValidation(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) { o.Config.MaxSubsPerConn = 2 })
	c := dial(t, a, "alice@x")

	c.send(map[string]any{"t": "sub", "collection": "posts"})
	if f := c.waitFor("error", nil); errorCode(f) != "bad_request" || f["id"] != nil {
		t.Fatalf("missing id: %v", f)
	}
	c.send(map[string]any{"t": "sub", "id": "x", "collection": "bad name"})
	if f := c.waitFor("error", hasID("x")); errorCode(f) != "bad_request" {
		t.Fatalf("bad collection: %v", f)
	}
	c.send(map[string]any{"t": "sub", "id": "y", "collection": "_files"})
	if f := c.waitFor("error", hasID("y")); errorCode(f) != "forbidden" {
		t.Fatalf("system collection: %v", f)
	}
	c.send(map[string]any{"t": "sub", "id": "s1", "collection": "posts"})
	c.waitFor("ack", hasID("s1"))
	c.send(map[string]any{"t": "sub", "id": "s1", "collection": "posts"}) // idempotent
	c.waitFor("ack", hasID("s1"))
	c.send(map[string]any{"t": "sub", "id": "s2", "collection": "comments"})
	c.waitFor("ack", hasID("s2"))
	if st := a.Stats(); st.Subs != 2 {
		t.Fatalf("subs = %d, want 2", st.Subs)
	}
	c.send(map[string]any{"t": "sub", "id": "s3", "collection": "tags"})
	if f := c.waitFor("error", hasID("s3")); errorCode(f) != "rate_limited" {
		t.Fatalf("over limit: %v", f)
	}
	// Re-using an id for another collection replaces the subscription.
	c.send(map[string]any{"t": "sub", "id": "s2", "collection": "tags"})
	c.waitFor("ack", hasID("s2"))
	if err := a.PublishDB(context.Background(), "site1", DBEvent{Collection: "tags", Op: OpCreate, DocID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if f := c.waitFor("db", nil); f["sub"] != "s2" || f.str("collection") != "tags" {
		t.Fatalf("replaced sub: %v", f)
	}
	c.send(map[string]any{"t": "unsub", "id": "unknown"})
	c.waitFor("ack", hasID("unknown"))
	c.send(map[string]any{"t": "unsub"})
	if f := c.waitFor("error", nil); errorCode(f) != "bad_request" {
		t.Fatalf("unsub without id: %v", f)
	}

	c.close()
	eventually(t, "subs released", func() bool { return a.Stats().Subs == 0 && a.Stats().Connections == 0 })
}

// joinRoom joins and returns the joined frame.
func joinRoom(c *client, room, id string, state any) frame {
	c.t.Helper()
	msg := map[string]any{"t": "join", "id": id, "room": room}
	if state != nil {
		msg["state"] = state
	}
	c.send(msg)
	return c.waitFor("joined", hasID(id))
}

// knows waits until c has learned about member cid (from its joined list or a
// presence join event) and returns that member.
func (c *client) knows(joined frame, cid string) map[string]any {
	c.t.Helper()
	if list, ok := joined["members"].([]any); ok {
		for _, m := range list {
			if mm, _ := m.(map[string]any); mm["cid"] == cid {
				return mm
			}
		}
	}
	f := c.waitFor("presence", presence("join", cid))
	m, _ := f["member"].(map[string]any)
	return m
}

func TestRoomsAcrossInstances(t *testing.T) {
	a, b := twoInstances(t, nil)
	c1 := dial(t, a, "alice@x")
	c2 := dial(t, b, "bob@x")
	c3 := dial(t, a, "carol@x")

	j1 := joinRoom(c1, "lobby", "r1", map[string]any{"x": 1})
	if j1.str("room") != "lobby" || j1.str("self", "cid") != c1.cid || j1.get("self", "state", "x") != 1.0 {
		t.Fatalf("joined = %v", j1)
	}
	if j1.str("self", "user", "id") != "alice@x" || j1.str("self", "user", "email") != "alice@x" {
		t.Fatalf("self user summary = %v", j1.get("self", "user"))
	}
	if _, err := time.Parse(time.RFC3339, j1.str("self", "joined_at")); err != nil {
		t.Fatalf("joined_at: %v", err)
	}
	if list, ok := j1["members"].([]any); !ok || len(list) != 0 {
		t.Fatalf("first member must get members: [] (got %v)", j1["members"])
	}
	if groups := j1.get("self", "user", "groups"); groups != nil {
		t.Fatalf("member user must be a summary, got groups=%v", groups)
	}

	// c2 on the other instance learns about c1 (via the sync on first join),
	// and c1 learns about c2.
	j2 := joinRoom(c2, "lobby", "r2", map[string]any{"y": 2})
	if m := c2.knows(j2, c1.cid); m["state"].(map[string]any)["x"] != 1.0 {
		t.Fatalf("c2's view of c1: %v", m)
	}
	f := c1.waitFor("presence", presence("join", c2.cid))
	if f.str("room") != "lobby" || f.get("member", "state", "y") != 2.0 || f.str("member", "user", "id") != "bob@x" {
		t.Fatalf("presence join = %v", f)
	}

	// c3 (same instance as c1) sees both in its joined list.
	j3 := joinRoom(c3, "lobby", "r3", nil)
	if got := memberCIDs(j3["members"]); !slices.Contains(got, c1.cid) || !slices.Contains(got, c2.cid) || slices.Contains(got, c3.cid) {
		t.Fatalf("c3 joined members = %v (c1=%s c2=%s)", got, c1.cid, c2.cid)
	}
	if j3.get("self", "state") == nil {
		t.Fatalf("absent state must be {}: %v", j3)
	}
	c1.waitFor("presence", presence("join", c3.cid))
	c2.waitFor("presence", presence("join", c3.cid))
	c3.expectNone("presence", presence("join", c3.cid), 50*time.Millisecond)

	// State updates reach everyone else.
	c1.send(map[string]any{"t": "state", "room": "lobby", "state": map[string]any{"x": 2, "cursor": []int{1, 2}}})
	for _, c := range []*client{c2, c3} {
		f := c.waitFor("presence", presence("update", c1.cid))
		if f.get("member", "state", "x") != 2.0 {
			t.Fatalf("update = %v", f)
		}
	}
	c1.expectNone("presence", presence("update", c1.cid), 50*time.Millisecond)

	// Broadcast without echo.
	c1.send(map[string]any{"t": "send", "room": "lobby", "data": map[string]any{"hello": "world"}})
	for _, c := range []*client{c2, c3} {
		f := c.waitFor("msg", nil)
		if f.str("room") != "lobby" || f.str("from", "cid") != c1.cid || f.str("from", "user", "id") != "alice@x" ||
			f.str("data", "hello") != "world" {
			t.Fatalf("msg = %v", f)
		}
	}
	// Broadcast with echo, scalar data.
	c1.send(map[string]any{"t": "send", "room": "lobby", "data": "echoed", "echo": true, "id": "m2"})
	c1.waitFor("ack", hasID("m2"))
	if f := c1.waitFor("msg", nil); f["data"] != "echoed" {
		t.Fatalf("echo = %v", f)
	}
	c2.waitFor("msg", func(f frame) bool { return f["data"] == "echoed" })
	c3.waitFor("msg", func(f frame) bool { return f["data"] == "echoed" })
	// No data → null.
	c2.send(map[string]any{"t": "send", "room": "lobby"})
	if f := c1.waitFor("msg", nil); f["data"] != nil || f.str("from", "cid") != c2.cid {
		t.Fatalf("msg without data = %v", f)
	}
	c3.waitFor("msg", nil)

	// Direct message to a member on the other instance.
	c1.send(map[string]any{"t": "send", "room": "lobby", "to": c2.cid, "data": "secret"})
	if f := c2.waitFor("msg", nil); f["data"] != "secret" || f.str("from", "cid") != c1.cid {
		t.Fatalf("direct = %v", f)
	}
	// Sentinel broadcast proves c3 got nothing before it.
	c1.send(map[string]any{"t": "send", "room": "lobby", "data": "sentinel"})
	if f := c3.waitFor("msg", nil); f["data"] != "sentinel" {
		t.Fatalf("c3 received a direct message meant for c2: %v", f)
	}
	c2.waitFor("msg", func(f frame) bool { return f["data"] == "sentinel" })
	// Direct with echo: sender gets a copy.
	c2.send(map[string]any{"t": "send", "room": "lobby", "to": c3.cid, "data": "dm", "echo": true})
	c3.waitFor("msg", func(f frame) bool { return f["data"] == "dm" })
	c2.waitFor("msg", func(f frame) bool { return f["data"] == "dm" })

	// Errors.
	c1.send(map[string]any{"t": "send", "room": "lobby", "to": "c_0000", "data": 1, "id": "bad-to"})
	if f := c1.waitFor("error", hasID("bad-to")); errorCode(f) != "not_found" {
		t.Fatalf("unknown recipient: %v", f)
	}
	c1.send(map[string]any{"t": "send", "room": "elsewhere", "data": 1, "id": "not-joined"})
	if f := c1.waitFor("error", hasID("not-joined")); errorCode(f) != "not_found" {
		t.Fatalf("send to unjoined room: %v", f)
	}
	c1.send(map[string]any{"t": "state", "room": "elsewhere", "state": map[string]any{}, "id": "st"})
	if f := c1.waitFor("error", hasID("st")); errorCode(f) != "not_found" {
		t.Fatalf("state for unjoined room: %v", f)
	}

	if st := a.Stats(); st.Rooms != 1 || st.Connections != 2 {
		t.Fatalf("A stats = %+v", st)
	}

	// Explicit leave.
	c3.send(map[string]any{"t": "leave", "room": "lobby"})
	c1.waitFor("presence", presence("leave", c3.cid))
	c2.waitFor("presence", presence("leave", c3.cid))

	// Disconnect = leave all rooms (gossiped to the other instance).
	c2.close()
	if f := c1.waitFor("presence", presence("leave", c2.cid)); f.str("member", "user", "id") != "bob@x" {
		t.Fatalf("leave = %v", f)
	}
	eventually(t, "B has no rooms", func() bool { return b.Stats().Rooms == 0 && b.Stats().Connections == 0 })

	// A later joiner on B sees only c1.
	c4 := dial(t, b, "dave@x")
	j4 := joinRoom(c4, "lobby", "r4", nil)
	c4.knows(j4, c1.cid)
	c4.expectNone("presence", presence("join", c2.cid), 100*time.Millisecond)
	c4.expectNone("presence", presence("join", c3.cid), 0)
}

func TestRejoinReplacesState(t *testing.T) {
	a, _ := twoInstances(t, nil)
	c1, c2 := dial(t, a, "alice@x"), dial(t, a, "bob@x")
	joinRoom(c1, "lobby", "1", map[string]any{"v": 1})
	j := joinRoom(c2, "lobby", "2", nil)
	if got := memberCIDs(j["members"]); len(got) != 1 || got[0] != c1.cid {
		t.Fatalf("members = %v", got)
	}
	j = joinRoom(c1, "lobby", "3", map[string]any{"v": 2})
	if j.get("self", "state", "v") != 2.0 || len(memberCIDs(j["members"])) != 1 {
		t.Fatalf("re-join = %v", j)
	}
	if f := c2.waitFor("presence", presence("update", c1.cid)); f.get("member", "state", "v") != 2.0 {
		t.Fatalf("update = %v", f)
	}
	c2.expectNone("presence", presence("join", c1.cid), 50*time.Millisecond)
	if st := a.Stats(); st.Rooms != 1 {
		t.Fatalf("rooms = %d", st.Rooms)
	}
}

func TestRoomLimitsAndValidation(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) {
		o.Config.MaxRoomsPerConn = 2
		o.Config.MaxPresenceBytes = 64
	})
	c := dial(t, a, "alice@x")
	joinRoom(c, "r-1", "1", nil)
	joinRoom(c, "r:2.x_y", "2", nil)
	c.send(map[string]any{"t": "join", "id": "3", "room": "r3"})
	if f := c.waitFor("error", hasID("3")); errorCode(f) != "rate_limited" {
		t.Fatalf("room limit: %v", f)
	}
	c.send(map[string]any{"t": "leave", "room": "r-1", "id": "l"})
	c.waitFor("ack", hasID("l"))
	joinRoom(c, "r3", "4", nil)

	for _, tc := range []struct {
		frame map[string]any
		code  string
	}{
		{map[string]any{"t": "join", "id": "e1", "room": "bad room"}, "bad_request"},
		{map[string]any{"t": "join", "id": "e2", "room": ""}, "bad_request"},
		{map[string]any{"t": "join", "id": "e3", "room": strings.Repeat("r", 129)}, "bad_request"},
		{map[string]any{"t": "join", "id": "e4", "room": "r3", "state": []int{1}}, "bad_request"},
		{map[string]any{"t": "join", "id": "e5", "room": "r3", "state": "s"}, "bad_request"},
		{map[string]any{"t": "join", "id": "e6", "room": "r3", "state": map[string]any{"big": strings.Repeat("x", 100)}}, "too_large"},
		{map[string]any{"t": "state", "id": "e7", "room": "r3", "state": map[string]any{"big": strings.Repeat("x", 100)}}, "too_large"},
		{map[string]any{"t": "send", "id": "e8", "room": "r3", "to": "not-a-cid", "data": 1}, "bad_request"},
		{map[string]any{"t": "leave", "id": "e9", "room": "bad room"}, "bad_request"},
	} {
		c.send(tc.frame)
		id := tc.frame["id"].(string)
		if f := c.waitFor("error", hasID(id)); errorCode(f) != tc.code {
			t.Errorf("%v: got %v, want code %s", tc.frame, f, tc.code)
		}
	}
	// Leaving a room one is not in is harmless.
	c.send(map[string]any{"t": "leave", "room": "never-joined", "id": "nl"})
	c.waitFor("ack", hasID("nl"))
}

func TestRemoteMemberExpiry(t *testing.T) {
	root := memory.New()
	t.Cleanup(func() { _ = root.Close() })
	short := func(id string) Options {
		return Options{InstanceID: id, HeartbeatInterval: 40 * time.Millisecond, MemberTTL: 300 * time.Millisecond}
	}
	a := newTestHub(t, root.NewPeer(), short("A"))
	pb := &partitionBroker{Broker: root.NewPeer()}
	c := newTestHub(t, pb, short("C"))

	c1 := dial(t, a, "alice@x")
	c3 := dial(t, c, "carol@x")
	joinRoom(c1, "lobby", "1", nil)
	j3 := joinRoom(c3, "lobby", "3", nil)
	c3.knows(j3, c1.cid)
	c1.waitFor("presence", presence("join", c3.cid))

	// Heartbeats keep the remote member alive well past the TTL.
	c1.expectNone("presence", presence("leave", c3.cid), 900*time.Millisecond)

	// Instance C vanishes without a goodbye: its members expire on A.
	pb.cut.Store(true)
	start := time.Now()
	c1.waitFor("presence", presence("leave", c3.cid))
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Fatalf("member expired after %v, before the TTL", d)
	}
	// C still delivers A's traffic to c3 (only C's publishes are cut).
	c1.send(map[string]any{"t": "send", "room": "lobby", "data": "still here?"})
	c3.waitFor("msg", nil)
}

func TestMembersGossipDropsVanishedMembers(t *testing.T) {
	// An instance's heartbeat is authoritative: members it no longer lists
	// are removed even if their leave was lost.
	a, b := twoInstances(t, func(o *Options) { o.HeartbeatInterval = 40 * time.Millisecond })
	c1 := dial(t, a, "alice@x")
	joinRoom(c1, "lobby", "1", nil)
	c2 := dial(t, b, "bob@x")
	joinRoom(c2, "lobby", "2", nil)
	c1.waitFor("presence", presence("join", c2.cid))

	// Simulate a lost leave: inject a "members" announcement from B without c2.
	ah := a.Hub.(*hub)
	ah.onRoomMessage("site1", "lobby", mustJSON(gossip{K: gossipMembers, I: b.Hub.(*hub).instance}))
	c1.waitFor("presence", presence("leave", c2.cid))
	// B's next heartbeat brings c2 back.
	c1.waitFor("presence", presence("join", c2.cid))
}

func TestStaleSnapshotsAreIgnored(t *testing.T) {
	a, _ := twoInstances(t, nil)
	ah := a.Hub.(*hub)
	c1 := dial(t, a, "alice@x")
	joinRoom(c1, "lobby", "1", nil)
	inject := func(g gossip) { ah.onRoomMessage("site1", "lobby", mustJSON(g)) }
	x := member{CID: "c_00aa", User: identityFor("xavier@x").Summary(), State: json.RawMessage(`{"v":1}`), JoinedAt: time.Now()}

	inject(gossip{K: gossipJoin, I: "Z", S: 10, M: &x})
	c1.waitFor("presence", presence("join", x.CID))
	// A snapshot taken before the join (seq 9) arrives late: ignored.
	inject(gossip{K: gossipMembers, I: "Z", S: 9})
	c1.expectNone("presence", presence("leave", x.CID), 50*time.Millisecond)

	x2 := x
	x2.State = json.RawMessage(`{"v":2}`)
	inject(gossip{K: gossipUpdate, I: "Z", S: 12, M: &x2})
	if f := c1.waitFor("presence", presence("update", x.CID)); f.get("member", "state", "v") != 2.0 {
		t.Fatalf("update = %v", f)
	}
	// A stale snapshot must not revert the state.
	inject(gossip{K: gossipMembers, I: "Z", S: 11, Ms: []member{x}})
	c1.expectNone("presence", presence("update", x.CID), 50*time.Millisecond)

	// A newer snapshot without the member removes it.
	inject(gossip{K: gossipMembers, I: "Z", S: 13})
	c1.waitFor("presence", presence("leave", x.CID))
	// A leave for an unknown member, or from the wrong instance, is ignored.
	inject(gossip{K: gossipJoin, I: "Z", S: 14, M: &x})
	c1.waitFor("presence", presence("join", x.CID))
	inject(gossip{K: gossipLeave, I: "Y", S: 15, M: &x})
	c1.expectNone("presence", presence("leave", x.CID), 50*time.Millisecond)
	// A departure blocks stale snapshots from resurrecting the member.
	inject(gossip{K: gossipLeave, I: "Z", S: 20, M: &x})
	c1.waitFor("presence", presence("leave", x.CID))
	inject(gossip{K: gossipMembers, I: "Z", S: 18, Ms: []member{x}})
	c1.expectNone("presence", presence("join", x.CID), 50*time.Millisecond)
	// ...but a later join (same connection re-joining) is applied.
	inject(gossip{K: gossipJoin, I: "Z", S: 21, M: &x})
	c1.waitFor("presence", presence("join", x.CID))
	// A late sync answer still adds members it did not know about, without
	// removing members that joined after the snapshot was taken.
	y := member{CID: "c_00bb", User: identityFor("yan@x").Summary(), JoinedAt: time.Now()}
	inject(gossip{K: gossipMembers, I: "Z", S: 19, Ms: []member{y}})
	c1.waitFor("presence", presence("join", y.CID))
	c1.expectNone("presence", presence("leave", x.CID), 50*time.Millisecond)
	// A leave for a member we never saw is remembered too.
	w := member{CID: "c_00cc", User: identityFor("wu@x").Summary(), JoinedAt: time.Now()}
	inject(gossip{K: gossipLeave, I: "Z", S: 30, M: &w})
	inject(gossip{K: gossipMembers, I: "Z", S: 29, Ms: []member{w, x, y}})
	c1.expectNone("presence", presence("join", w.CID), 50*time.Millisecond)
	// Gossip from our own instance and garbage are ignored.
	inject(gossip{K: gossipLeave, I: ah.instance, S: 16, M: &x})
	ah.onRoomMessage("site1", "lobby", []byte("garbage"))
	ah.onRoomMessage("site1", "lobby", mustJSON(gossip{K: gossipJoin, I: "Z", M: &member{CID: "not-a-cid"}}))
	ah.onRoomMessage("nosuchsite", "lobby", mustJSON(gossip{K: gossipJoin, I: "Z", M: &x}))
	ah.onDBMessage("site1", []byte("garbage"))
	c1.expectNone("presence", nil, 50*time.Millisecond)
}

func identityFor(id string) *identity.User { return &identity.User{ID: id, Email: id, Name: id} }

func TestInstanceCloseRemovesItsMembers(t *testing.T) {
	a, b := twoInstances(t, nil)
	c1 := dial(t, a, "alice@x")
	c2 := dial(t, b, "bob@x")
	joinRoom(c1, "lobby", "1", nil)
	joinRoom(c2, "lobby", "2", nil)
	c1.waitFor("presence", presence("join", c2.cid))
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c2.waitClosed(); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (%v)", websocket.CloseStatus(err), err)
	}
	c1.waitFor("presence", presence("leave", c2.cid))
}

func TestRateLimit(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) {
		o.Config.MessagesPerSecond = 1
		o.Config.MessageBurst = 3
	})
	c := dial(t, a, "alice@x")
	for range 10 {
		c.send(map[string]any{"t": "ping"})
	}
	for range 3 {
		c.waitFor("pong", nil)
	}
	if f := c.waitFor("error", nil); errorCode(f) != "rate_limited" {
		t.Fatalf("got %v", f)
	}
	c.expectNone("pong", nil, 300*time.Millisecond)
	c.expectNone("error", nil, 0) // the error is reported once, not per dropped frame
	// Tokens refill.
	time.Sleep(1100 * time.Millisecond)
	c.send(map[string]any{"t": "ping"})
	c.waitFor("pong", nil)
}

func TestOversizedFrames(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) { o.Config.MaxMessageBytes = 1024 })
	c := dial(t, a, "alice@x")
	big := `{"t":"send","room":"x","id":"big","data":"` + strings.Repeat("a", 2000) + `"}`
	c.send(big)
	if f := c.waitFor("error", nil); errorCode(f) != "too_large" {
		t.Fatalf("got %v", f)
	}
	// The connection survives and still works.
	c.send(map[string]any{"t": "ping"})
	c.waitFor("pong", nil)
	// Far beyond the limit, the server aborts with 1009 (message too big).
	c.send(strings.Repeat("x", 8000))
	if err := c.waitClosed(); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("close status = %v (%v)", websocket.CloseStatus(err), err)
	}
	eventually(t, "connection gone", func() bool { return a.Stats().Connections == 0 })
}

func TestMalformedFrames(t *testing.T) {
	a, _ := twoInstances(t, nil)
	c := dial(t, a, "alice@x")
	for _, tc := range []struct {
		raw  string
		id   any
		code string
	}{
		{`not json`, nil, "bad_request"},
		{`{"t":`, nil, "bad_request"},
		{`[1,2,3]`, nil, "bad_request"},
		{`"string"`, nil, "bad_request"},
		{`null`, nil, "bad_request"},
		{`{}`, nil, "bad_request"},
		{`{"id":"z"}`, "z", "bad_request"},
		{`{"t":"bogus","id":"x"}`, "x", "bad_request"},
		{`{"t":123,"id":"n"}`, "n", "bad_request"},
		{`{"t":"join","room":123,"id":"k"}`, "k", "bad_request"},
		{`{"t":"join","room":"ok","id":{"a":1}}`, nil, "bad_request"},
		{`{"t":"join","room":"ok","id":true}`, nil, "bad_request"},
		{`{"t":"sub","id":"` + strings.Repeat("i", 200) + `","collection":"posts"}`, nil, "bad_request"},
		{`{"t":"send","room":"lobby","data":1}`, nil, "not_found"},
		{"\xff\xfe", nil, "bad_request"},
	} {
		c.send(tc.raw)
		f := c.waitFor("error", nil)
		if errorCode(f) != tc.code {
			t.Errorf("%q: code %q, want %q (%v)", tc.raw, errorCode(f), tc.code, f)
		}
		if tc.id != nil && f["id"] != tc.id {
			t.Errorf("%q: id %v, want %v", tc.raw, f["id"], tc.id)
		}
		if tc.id == nil && f["id"] != nil {
			t.Errorf("%q: unexpected id %v", tc.raw, f["id"])
		}
		if msg := f.str("error", "message"); msg == "" {
			t.Errorf("%q: empty error message", tc.raw)
		}
	}
	// Binary frames are rejected without closing the connection.
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if f := c.waitFor("error", nil); errorCode(f) != "bad_request" {
		t.Fatalf("binary: %v", f)
	}
	// Numeric ids are echoed as numbers.
	c.send(`{"t":"join","room":"lobby","id":42}`)
	c.waitFor("joined", hasID(42))
	c.send(map[string]any{"t": "ping"})
	c.waitFor("pong", nil)
	if st := a.Stats(); st.Connections != 1 {
		t.Fatalf("connections = %d", st.Connections)
	}
}

func TestOriginCheck(t *testing.T) {
	a, _ := twoInstances(t, nil)
	host := strings.TrimPrefix(a.srv.URL, "http://")
	try := func(origin string) int {
		h := http.Header{}
		if origin != "" {
			h.Set("Origin", origin)
		}
		ws, resp, err := dialRaw(t, a, "alice@x", h)
		if err == nil {
			ws.CloseNow()
			return http.StatusSwitchingProtocols
		}
		if resp == nil {
			t.Fatalf("dial with origin %q: %v", origin, err)
		}
		return resp.StatusCode
	}
	for _, tc := range []struct {
		origin  string
		allowed []string
		want    int
	}{
		{"", nil, 101},
		{"http://" + host, nil, 101},
		{"https://" + host, nil, 101},
		{"http://" + strings.ToUpper(host), nil, 101},
		{"http://evil.example", nil, 403},
		{"null", nil, 403},
		{"http://" + host + ".evil.example", nil, 403},
		{"file://" + host, nil, 403},
		{"https://app.example.com", []string{"https://app.example.com"}, 101},
		{"https://app.example.com", []string{"https://app.example.com/"}, 101},
		{"http://app.example.com", []string{"https://app.example.com"}, 403},
		{"https://other.example.com", []string{"https://app.example.com"}, 403},
		{"http://" + host, []string{"https://app.example.com"}, 101},
	} {
		a.origins = tc.allowed
		if got := try(tc.origin); got != tc.want {
			t.Errorf("origin %q allowed=%v: status %d, want %d", tc.origin, tc.allowed, got, tc.want)
		}
	}
}

func TestOriginAllowedDefaultPorts(t *testing.T) {
	for _, tc := range []struct {
		origin, host string
		want         bool
	}{
		{"https://foo.acme.com", "foo.acme.com", true},
		{"https://foo.acme.com", "foo.acme.com:443", true},
		{"https://foo.acme.com:443", "foo.acme.com", true},
		{"http://foo.acme.com:80", "foo.acme.com", true},
		{"https://foo.acme.com:8443", "foo.acme.com", false},
		{"https://foo.acme.com", "bar.acme.com", false},
		{"https://user@foo.acme.com", "foo.acme.com", false},
		{"https://foo.acme.com/path", "foo.acme.com", false},
		{"ws://foo.acme.com", "foo.acme.com", false},
		{"%%%", "foo.acme.com", false},
	} {
		r, _ := http.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		if got := originAllowed(r, nil); got != tc.want {
			t.Errorf("origin %q host %q: %v, want %v", tc.origin, tc.host, got, tc.want)
		}
	}
	// A request received over TLS only accepts https origins as same-origin.
	r, _ := http.NewRequest(http.MethodGet, "https://foo.acme.com/", nil)
	r.Host = "foo.acme.com"
	r.TLS = &tls.ConnectionState{}
	for origin, want := range map[string]bool{
		"https://foo.acme.com":    true,
		"http://foo.acme.com":     false,
		"http://foo.acme.com:443": false,
	} {
		r.Header.Set("Origin", origin)
		if got := originAllowed(r, nil); got != want {
			t.Errorf("TLS request, origin %q: %v, want %v", origin, got, want)
		}
	}
	// ...unless listed explicitly.
	r.Header.Set("Origin", "http://foo.acme.com")
	if !originAllowed(r, []string{"http://foo.acme.com"}) {
		t.Error("explicitly allowed origin rejected")
	}
}

func TestMaxConnsPerUser(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) { o.Config.MaxConnsPerUser = 2 })
	c1 := dial(t, a, "alice@x")
	dial(t, a, "alice@x")
	_, resp, err := dialRaw(t, a, "alice@x", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third connection: err=%v resp=%v", err, resp)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("missing Retry-After")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"rate_limited"`) {
		t.Errorf("body = %s", body)
	}
	// Other users are unaffected; closing a connection frees a slot.
	dial(t, a, "bob@x")
	c1.close()
	eventually(t, "slot freed", func() bool {
		ws, _, err := dialRaw(t, a, "alice@x", nil)
		if err != nil {
			return false
		}
		ws.CloseNow()
		return true
	})
}

func TestServeWSRejections(t *testing.T) {
	a, _ := twoInstances(t, nil)
	ctx := context.Background()
	for _, tc := range []struct {
		query string
		want  int
	}{
		{"", http.StatusUnauthorized},                 // no user
		{"user=", http.StatusUnauthorized},            // user without id
		{"user=a&site=Bad_Site", http.StatusNotFound}, // invalid site
		{"user=a&site=_system", http.StatusNotFound},  // system namespace
	} {
		_, resp, err := websocket.Dial(ctx, a.wsURL(tc.query), nil)
		if err == nil || resp == nil || resp.StatusCode != tc.want {
			t.Errorf("%q: err=%v status=%v, want %d", tc.query, err, resp, tc.want)
		}
	}
	// A plain HTTP request (no upgrade) is rejected by the websocket library.
	resp, err := http.Get(a.srv.URL + "/ws?user=a")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Errorf("non-upgrade request: status %d", resp.StatusCode)
	}
	eventually(t, "no leaked slots", func() bool {
		h := a.Hub.(*hub)
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.perUser) == 0 && len(h.sites) == 0
	})
}

func TestDisabled(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) { o.Config.Disabled = true })
	_, resp, err := dialRaw(t, a, "alice@x", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("err=%v resp=%v", err, resp)
	}
}

func TestCloseHub(t *testing.T) {
	a, _ := twoInstances(t, nil)
	c1 := dial(t, a, "alice@x")
	c1.send(map[string]any{"t": "sub", "id": "s", "collection": "posts"})
	c1.waitFor("ack", nil)
	joinRoom(c1, "lobby", "j", nil)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c1.waitClosed(); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (%v)", websocket.CloseStatus(err), err)
	}
	if st := a.Stats(); st != (Stats{}) {
		t.Fatalf("stats after close = %+v", st)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := a.PublishDB(context.Background(), "site1", DBEvent{Collection: "posts", Op: OpDelete, DocID: "x"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("PublishDB after close = %v", err)
	}
	_, resp, err := dialRaw(t, a, "alice@x", nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("dial after close: err=%v resp=%v", err, resp)
	}
	if n := a.Hub.(*hub).topics.len(); n != 0 {
		t.Fatalf("%d broker subscriptions left after close", n)
	}
}

func TestCloseWithTrafficInFlight(t *testing.T) {
	// Steady traffic (paced so that clients keep up and are not dropped as
	// slow consumers) must not turn the shutdown into abnormal closures.
	a, _ := twoInstances(t, func(o *Options) { o.SendQueue = 4096 })
	clients := make([]*client, 5)
	for i := range clients {
		clients[i] = dial(t, a, "user"+string(rune('a'+i))+"@x")
		clients[i].send(map[string]any{"t": "sub", "id": "s", "collection": "posts"})
		clients[i].waitFor("ack", nil)
	}
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpCreate, Doc: testDoc("d", map[string]any{"i": i}, 1)})
			time.Sleep(200 * time.Microsecond)
		}
	})
	time.Sleep(50 * time.Millisecond)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	close(stop)
	wg.Wait()
	for _, c := range clients {
		if err := c.waitClosed(); websocket.CloseStatus(err) != websocket.StatusGoingAway {
			t.Errorf("client %s: close status %v (%v)", c.cid, websocket.CloseStatus(err), err)
		}
	}
}

func TestPongTimeout(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) {
		o.PingInterval = 30 * time.Millisecond
		o.PongTimeout = 150 * time.Millisecond
	})
	// A responsive client (it reads, so it answers pings) stays connected.
	c := dial(t, a, "alice@x")
	time.Sleep(400 * time.Millisecond)
	c.send(map[string]any{"t": "ping"})
	c.waitFor("pong", nil)

	// A client that never reads never answers pings and gets dropped.
	ws, _, err := dialRaw(t, a, "bob@x", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	eventually(t, "unresponsive client dropped", func() bool { return a.Stats().Connections == 1 })
}

func TestSlowConsumerDisconnected(t *testing.T) {
	a, _ := twoInstances(t, func(o *Options) {
		o.SendQueue = 4
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
	big := map[string]any{"blob": strings.Repeat("z", 200<<10)}
	for i := 0; i < 200 && a.Stats().Connections > 0; i++ {
		if err := a.PublishDB(ctx, "site1", DBEvent{Collection: "posts", Op: OpCreate, Doc: testDoc("d", big, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "slow consumer dropped", func() bool { return a.Stats().Connections == 0 })
}

func TestBrokerFailures(t *testing.T) {
	th := newTestHub(t, failingBroker{}, Options{})
	c := dial(t, th, "alice@x")
	c.send(map[string]any{"t": "sub", "id": "s", "collection": "posts"})
	if f := c.waitFor("error", hasID("s")); errorCode(f) != "internal" {
		t.Fatalf("sub with broker down: %v", f)
	}
	c.send(map[string]any{"t": "join", "id": "j", "room": "lobby"})
	if f := c.waitFor("error", hasID("j")); errorCode(f) != "internal" {
		t.Fatalf("join with broker down: %v", f)
	}
	if err := th.PublishDB(context.Background(), "site1", DBEvent{Collection: "posts", Op: OpDelete, DocID: "x"}); !errors.Is(err, errBrokerDown) {
		t.Fatalf("PublishDB = %v", err)
	}
	if st := th.Stats(); st.Subs != 0 || st.Rooms != 0 {
		t.Fatalf("stats = %+v", st)
	}
	c.send(map[string]any{"t": "ping"})
	c.waitFor("pong", nil)
}

func TestSitesAreIsolated(t *testing.T) {
	a, b := twoInstances(t, nil)
	x := dialWith(t, a, "user=alice@x&site=alpha", nil)
	y := dialWith(t, b, "user=bob@x&site=beta", nil)
	z := dialWith(t, b, "user=carol@x&site=alpha", nil)
	joinRoom(x, "lobby", "1", nil)
	joinRoom(y, "lobby", "2", nil)
	jz := joinRoom(z, "lobby", "3", nil)
	z.knows(jz, x.cid)
	x.waitFor("presence", presence("join", z.cid))
	x.send(map[string]any{"t": "send", "room": "lobby", "data": "alpha only"})
	z.waitFor("msg", nil)
	y.expectNone("msg", nil, 100*time.Millisecond)
	y.expectNone("presence", nil, 0)
}

func TestOnFrameCounter(t *testing.T) {
	var in, out atomic.Int64
	a, _ := twoInstances(t, func(o *Options) {
		o.OnFrame = func(dir string) {
			switch dir {
			case "in":
				in.Add(1)
			case "out":
				out.Add(1)
			}
		}
	})
	c := dial(t, a, "alice@x") // hello: 1 out
	c.send(map[string]any{"t": "ping"})
	c.waitFor("pong", nil)
	eventually(t, "frame counts", func() bool { return in.Load() == 1 && out.Load() == 2 })
}

func TestNilBrokerUsesInProcessBus(t *testing.T) {
	th := newTestHub(t, nil, Options{})
	c1, c2 := dial(t, th, "alice@x"), dial(t, th, "bob@x")
	joinRoom(c1, "lobby", "1", nil)
	joinRoom(c2, "lobby", "2", nil)
	c1.waitFor("presence", presence("join", c2.cid))
	c2.send(map[string]any{"t": "send", "room": "lobby", "data": 1})
	c1.waitFor("msg", nil)
}

func TestDefaults(t *testing.T) {
	h := New(memory.New(), Options{Config: config.RealtimeConfig{}}).(*hub)
	defer h.Close()
	if h.cfg.MaxRoomsPerConn != 50 || h.cfg.MaxSubsPerConn != 100 || h.cfg.MessagesPerSecond != 60 ||
		h.cfg.MessageBurst != 120 || h.cfg.MaxMessageBytes != 64<<10 || h.cfg.MaxConnsPerUser != 50 ||
		h.cfg.MaxPresenceBytes != 8<<10 {
		t.Fatalf("config defaults = %+v", h.cfg)
	}
	if h.opts.PingInterval != 25*time.Second || h.opts.PongTimeout != 60*time.Second ||
		h.opts.HeartbeatInterval != 20*time.Second || h.opts.MemberTTL != 60*time.Second {
		t.Fatalf("option defaults = %+v", h.opts)
	}
	if !strings.HasPrefix(h.instance, "i~") || len(h.instance) != len("i~")+12 {
		t.Fatalf("instance id = %q", h.instance)
	}
	if h.cfg.MaxPresenceBytes > maxPresenceCap {
		t.Fatal("presence cap not applied")
	}
	h2 := New(memory.New(), Options{
		InstanceID: "pod-1", HeartbeatInterval: time.Second, MemberTTL: time.Second,
		Config: config.RealtimeConfig{MaxPresenceBytes: 4 << 20},
	}).(*hub)
	defer h2.Close()
	if !strings.HasPrefix(h2.instance, "pod-1~") {
		t.Fatalf("instance id = %q", h2.instance)
	}
	if h2.opts.MemberTTL != 3*time.Second {
		t.Fatalf("MemberTTL shorter than two heartbeats not raised: %v", h2.opts.MemberTTL)
	}
	if h2.cfg.MaxPresenceBytes != maxPresenceCap {
		t.Fatalf("MaxPresenceBytes = %d, want cap %d", h2.cfg.MaxPresenceBytes, maxPresenceCap)
	}
}

// TestSameInstanceIDStillGossips: two instances configured with the same
// InstanceID (e.g. a hostname that is "localhost" everywhere) must not
// mistake each other's gossip for their own.
func TestSameInstanceIDStillGossips(t *testing.T) {
	b := memory.New()
	t.Cleanup(func() { _ = b.Close() })
	a1 := newTestHub(t, b, Options{InstanceID: "localhost"})
	a2 := newTestHub(t, b.NewPeer(), Options{InstanceID: "localhost"})
	c1, c2 := dial(t, a1, "alice@x"), dial(t, a2, "bob@x")
	joinRoom(c1, "lobby", "1", nil)
	j2 := joinRoom(c2, "lobby", "2", nil)
	c2.knows(j2, c1.cid)
	c1.waitFor("presence", presence("join", c2.cid))
}

// TestJoinedAtFixedWidth: joined_at always has three fractional digits so
// that clients can sort members by string comparison.
func TestJoinedAtFixedWidth(t *testing.T) {
	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "2026-01-02T03:04:05.000Z"},
		{time.Date(2026, 1, 2, 3, 4, 5, 120_000_000, time.UTC), "2026-01-02T03:04:05.120Z"},
		{time.Date(2026, 1, 2, 3, 4, 5, 123_456_789, time.UTC), "2026-01-02T03:04:05.123Z"},
		{time.Date(2026, 1, 2, 4, 4, 5, 0, time.FixedZone("x", 3600)), "2026-01-02T03:04:05.000Z"},
	} {
		var got struct {
			JoinedAt string          `json:"joined_at"`
			State    json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(mustJSON(member{CID: "c_1", JoinedAt: tc.at}), &got); err != nil {
			t.Fatal(err)
		}
		if got.JoinedAt != tc.want {
			t.Errorf("joined_at(%v) = %q, want %q", tc.at, got.JoinedAt, tc.want)
		}
		if string(got.State) != "{}" {
			t.Errorf("nil state encoded as %s", got.State)
		}
		// Gossip round trip keeps the instant (to the millisecond).
		var back member
		if err := json.Unmarshal(mustJSON(member{CID: "c_1", JoinedAt: tc.at}), &back); err != nil {
			t.Fatal(err)
		}
		if !back.JoinedAt.Equal(tc.at.Truncate(time.Millisecond)) {
			t.Errorf("round trip %v → %v", tc.at, back.JoinedAt)
		}
	}
	// String order equals time order.
	a := string(mustJSON(member{JoinedAt: time.Date(2026, 1, 1, 0, 0, 0, 120_000_000, time.UTC)}))
	b := string(mustJSON(member{JoinedAt: time.Date(2026, 1, 1, 0, 0, 0, 123_000_000, time.UTC)}))
	if !(a < b) {
		t.Errorf("string order broken: %s vs %s", a, b)
	}
}
