package realtime

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/thasami/backyard/internal/pubsub"
)

// topicSet keeps one broker subscription per topic for the whole hub,
// reference-counted by the local users of that topic (db subscriptions, room
// memberships). The first acquire subscribes and waits until the broker
// confirms; the last release cancels.
type topicSet struct {
	broker pubsub.Broker

	mu     sync.Mutex
	m      map[string]*topicEntry
	closed bool
}

type topicEntry struct {
	topic  string
	refs   int // guarded by topicSet.mu
	ready  chan struct{}
	err    error  // set before ready is closed
	cancel func() // set before ready is closed (nil on error)
	// dead is set when the entry is retired; a retired subscription's
	// handler ignores messages so that a brief overlap with its replacement
	// cannot deliver duplicates.
	dead atomic.Bool
}

func newTopicSet(b pubsub.Broker) *topicSet {
	return &topicSet{broker: b, m: make(map[string]*topicEntry)}
}

// acquire takes a reference on topic, subscribing with handler if this is the
// first one. On success the caller must call release exactly once.
func (t *topicSet) acquire(ctx context.Context, topic string, handler pubsub.Handler) (*topicEntry, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	e := t.m[topic]
	creator := e == nil
	if creator {
		e = &topicEntry{topic: topic, ready: make(chan struct{})}
		t.m[topic] = e
	}
	e.refs++
	t.mu.Unlock()

	if creator {
		cancel, err := t.broker.Subscribe(ctx, topic, func(p []byte) {
			if !e.dead.Load() {
				handler(p)
			}
		})
		t.mu.Lock()
		if err == nil && t.closed {
			err = ErrClosed
			defer cancel()
			cancel = nil
		}
		e.cancel, e.err = cancel, err
		if err != nil {
			e.dead.Store(true)
			if t.m[topic] == e {
				delete(t.m, topic)
			}
		}
		close(e.ready)
		t.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return e, nil
	}

	select {
	case <-e.ready:
	case <-ctx.Done():
		t.release(e)
		return nil, ctx.Err()
	}
	if e.err != nil {
		return nil, e.err
	}
	return e, nil
}

// release drops one reference; the last one cancels the broker subscription.
func (t *topicSet) release(e *topicEntry) {
	if e == nil {
		return
	}
	t.mu.Lock()
	e.refs--
	if e.refs > 0 || e.dead.Load() {
		t.mu.Unlock()
		return
	}
	e.dead.Store(true)
	if t.m[e.topic] == e {
		delete(t.m, e.topic)
	}
	cancel := e.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// close cancels every subscription; later acquires fail with ErrClosed.
func (t *topicSet) close() {
	t.mu.Lock()
	t.closed = true
	var cancels []func()
	for topic, e := range t.m {
		select {
		case <-e.ready:
		default:
			// Still subscribing: the creator sees t.closed and cancels.
			continue
		}
		e.dead.Store(true)
		if e.cancel != nil {
			cancels = append(cancels, e.cancel)
		}
		delete(t.m, topic)
	}
	t.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

// len returns the number of subscribed topics.
func (t *topicSet) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.m)
}
