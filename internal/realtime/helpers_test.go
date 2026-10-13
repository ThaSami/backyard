package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/pubsub"
)

const waitTimeout = 5 * time.Second

// testHub is a Hub behind an httptest server. Clients pick their identity
// with ?user=<id> and their site with ?site=<name> (default "site1").
type testHub struct {
	Hub
	srv     *httptest.Server
	origins []string
}

func newTestHub(t *testing.T, b pubsub.Broker, opts Options) *testHub {
	t.Helper()
	if opts.Logger == nil {
		opts.Logger = testLogger(t)
	}
	th := &testHub{Hub: New(b, opts)}
	th.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		site := q.Get("site")
		if site == "" {
			site = "site1"
		}
		var user *identity.User
		if q.Has("user") { // "user=" yields a user without an id
			id := q.Get("user")
			user = &identity.User{ID: id, Email: id, Name: "Name of " + id, Groups: []string{"eng"}}
		}
		th.ServeWS(w, r, site, user, th.origins)
	}))
	t.Cleanup(func() {
		_ = th.Close()
		th.srv.Close()
	})
	return th
}

func (th *testHub) wsURL(query string) string {
	return "ws" + strings.TrimPrefix(th.srv.URL, "http") + "/ws?" + query
}

// frame is a decoded server frame.
type frame map[string]any

func (f frame) t() string { s, _ := f["t"].(string); return s }

func (f frame) str(path ...string) string {
	v := f.get(path...)
	s, _ := v.(string)
	return s
}

func (f frame) get(path ...string) any {
	var cur any = map[string]any(f)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

func (f frame) String() string {
	b, _ := json.Marshal(map[string]any(f))
	return string(b)
}

// client is a test websocket client that reads continuously (so it answers
// pings) and buffers frames.
type client struct {
	t     *testing.T
	ws    *websocket.Conn
	in    chan frame
	done  chan struct{}
	err   atomic.Value // error that ended the read loop
	mu    sync.Mutex
	queue []frame // frames received but not yet consumed
	cid   string
	hello frame
}

func dialRaw(t *testing.T, th *testHub, user string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	return websocket.Dial(ctx, th.wsURL("user="+user), &websocket.DialOptions{HTTPHeader: header})
}

func dial(t *testing.T, th *testHub, user string) *client {
	t.Helper()
	return dialWith(t, th, "user="+user, nil)
}

func dialWith(t *testing.T, th *testHub, query string, header http.Header) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	ws, resp, err := websocket.Dial(ctx, th.wsURL(query), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", query, err, status)
	}
	ws.SetReadLimit(4 << 20)
	c := &client{t: t, ws: ws, in: make(chan frame, 1024), done: make(chan struct{})}
	go c.readLoop()
	t.Cleanup(func() { c.ws.CloseNow() })
	c.hello = c.waitFor("hello", nil)
	c.cid = c.hello.str("cid")
	return c
}

func (c *client) readLoop() {
	defer close(c.done)
	for {
		typ, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.err.Store(err)
			return
		}
		if typ != websocket.MessageText {
			c.err.Store(fmt.Errorf("unexpected binary frame"))
			return
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			c.err.Store(fmt.Errorf("server sent invalid JSON %q: %w", data, err))
			return
		}
		c.in <- f
	}
}

// send writes v as a JSON text frame.
func (c *client) send(v any) {
	c.t.Helper()
	var data []byte
	switch x := v.(type) {
	case string:
		data = []byte(x)
	case []byte:
		data = x
	default:
		var err error
		if data, err = json.Marshal(v); err != nil {
			c.t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// waitFor returns the first frame (buffered or new) of type typ that matches
// pred (nil = any). Non-matching frames stay buffered for later calls.
func (c *client) waitFor(typ string, pred func(frame) bool) frame {
	c.t.Helper()
	match := func(f frame) bool { return f.t() == typ && (pred == nil || pred(f)) }
	c.mu.Lock()
	for i, f := range c.queue {
		if match(f) {
			c.queue = append(c.queue[:i], c.queue[i+1:]...)
			c.mu.Unlock()
			return f
		}
	}
	c.mu.Unlock()
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	for {
		select {
		case f := <-c.in:
			if match(f) {
				return f
			}
			c.mu.Lock()
			c.queue = append(c.queue, f)
			c.mu.Unlock()
		case <-c.done:
			// Drain frames that arrived before the connection ended.
			for {
				select {
				case f := <-c.in:
					if match(f) {
						return f
					}
					c.mu.Lock()
					c.queue = append(c.queue, f)
					c.mu.Unlock()
					continue
				default:
				}
				break
			}
			c.t.Fatalf("connection %s ended while waiting for %q: %v (buffered: %v)", c.cid, typ, c.err.Load(), c.buffered())
			return nil
		case <-timer.C:
			c.t.Fatalf("client %s: timed out waiting for %q frame (buffered: %v)", c.cid, typ, c.buffered())
			return nil
		}
	}
}

// expectNone fails if a frame of type typ matching pred arrives within d (or
// is already buffered).
func (c *client) expectNone(typ string, pred func(frame) bool, d time.Duration) {
	c.t.Helper()
	match := func(f frame) bool { return f.t() == typ && (pred == nil || pred(f)) }
	deadline := time.After(d)
	for {
		c.mu.Lock()
		for _, f := range c.queue {
			if match(f) {
				c.mu.Unlock()
				c.t.Fatalf("client %s: unexpected frame %v", c.cid, f)
			}
		}
		c.mu.Unlock()
		select {
		case f := <-c.in:
			c.mu.Lock()
			c.queue = append(c.queue, f)
			c.mu.Unlock()
		case <-deadline:
			return
		case <-c.done:
			return
		}
	}
}

func (c *client) buffered() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.queue))
	for i, f := range c.queue {
		out[i] = f.String()
	}
	return out
}

// waitClosed waits for the server to end the connection and returns the
// close error.
func (c *client) waitClosed() error {
	c.t.Helper()
	for {
		select {
		case f := <-c.in:
			c.mu.Lock()
			c.queue = append(c.queue, f)
			c.mu.Unlock()
		case <-c.done:
			err, _ := c.err.Load().(error)
			return err
		case <-time.After(waitTimeout):
			c.t.Fatalf("client %s: connection not closed by server", c.cid)
			return nil
		}
	}
}

func (c *client) close() {
	_ = c.ws.Close(websocket.StatusNormalClosure, "bye")
	<-c.done
}

func errorCode(f frame) string { return f.str("error", "code") }

func hasID(id any) func(frame) bool {
	return func(f frame) bool { return fmt.Sprint(f["id"]) == fmt.Sprint(id) }
}

func presence(op, cid string) func(frame) bool {
	return func(f frame) bool { return f.str("op") == op && f.str("member", "cid") == cid }
}

func memberCIDs(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, m := range list {
		if mm, ok := m.(map[string]any); ok {
			s, _ := mm["cid"].(string)
			out = append(out, s)
		}
	}
	return out
}

// eventually polls cond until it holds or the timeout expires.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// partitionBroker drops every Publish while cut is set (simulating an
// instance that lost its broker connection or crashed without saying
// goodbye).
type partitionBroker struct {
	pubsub.Broker
	cut atomic.Bool
}

func (p *partitionBroker) Publish(ctx context.Context, topic string, payload []byte) error {
	if p.cut.Load() {
		return nil
	}
	return p.Broker.Publish(ctx, topic, payload)
}

// failingBroker fails every operation.
type failingBroker struct{}

var errBrokerDown = errors.New("broker down")

func (failingBroker) Publish(context.Context, string, []byte) error { return errBrokerDown }
func (failingBroker) Subscribe(context.Context, string, pubsub.Handler) (func(), error) {
	return nil, errBrokerDown
}
func (failingBroker) Close() error { return nil }
