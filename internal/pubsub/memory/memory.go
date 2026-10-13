// Package memory is the in-process pubsub.Broker used for single-instance
// deployments, development and tests.
//
// Publish copies the payload once and queues it for every matching
// subscription; each subscription has its own goroutine and bounded queue, so
// handlers run sequentially per subscription and a slow handler never blocks
// publishers (overflowing messages are dropped with a warning).
//
// Brokers created with NewPeer share one in-process bus, which simulates
// several server instances connected to the same backend: a message published
// on any peer is delivered to the subscribers of every open peer.
package memory

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/internal/fanout"
)

// Option configures a Broker.
type Option func(*fanout.Options)

// WithLogger sets the logger used for overflow warnings and handler panics.
func WithLogger(l *slog.Logger) Option {
	return func(o *fanout.Options) { o.Logger = l }
}

// WithQueueLimits bounds the undelivered messages (count and bytes) kept per
// subscription. Non-positive values keep the defaults (10000 messages, 64 MiB).
func WithQueueLimits(messages, bytes int) Option {
	return func(o *fanout.Options) {
		o.QueueLen = messages
		o.QueueBytes = bytes
	}
}

type bus struct {
	mu      sync.RWMutex
	members map[*Broker]struct{}
}

// Broker is an in-process pubsub.Broker. The zero value is not usable; call New.
type Broker struct {
	bus    *bus
	opts   []Option
	reg    *fanout.Registry
	closed atomic.Bool
}

var _ pubsub.Broker = (*Broker)(nil)

// New returns a broker on a fresh in-process bus.
func New(opts ...Option) *Broker {
	return newOnBus(&bus{members: make(map[*Broker]struct{})}, opts)
}

// NewPeer returns another broker attached to the same bus as b, as if it were
// a second server instance sharing b's backend. It inherits b's options.
// Closing one peer does not affect the others.
func (b *Broker) NewPeer() *Broker {
	return newOnBus(b.bus, b.opts)
}

func newOnBus(bs *bus, opts []Option) *Broker {
	var fo fanout.Options
	for _, o := range opts {
		o(&fo)
	}
	b := &Broker{bus: bs, opts: opts, reg: fanout.New(fo)}
	bs.mu.Lock()
	bs.members[b] = struct{}{}
	bs.mu.Unlock()
	return b
}

// Publish delivers payload to the subscribers of topic on every open peer.
func (b *Broker) Publish(ctx context.Context, topic string, payload []byte) error {
	if b.closed.Load() {
		return pubsub.ErrClosed
	}
	if err := pubsub.ValidatePublish(topic, payload); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p := make([]byte, len(payload))
	copy(p, payload)
	b.bus.mu.RLock()
	defer b.bus.mu.RUnlock()
	for m := range b.bus.members {
		m.reg.Dispatch(topic, p)
	}
	return nil
}

// Subscribe registers h for topic until cancel is called. cancel is idempotent
// and safe to call from within h.
func (b *Broker) Subscribe(ctx context.Context, topic string, h pubsub.Handler) (func(), error) {
	if b.closed.Load() {
		return nil, pubsub.ErrClosed
	}
	if h == nil {
		return nil, pubsub.ErrNilHandler
	}
	if err := pubsub.ValidateTopic(topic); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, _, err := b.reg.Add(topic, h)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { b.reg.Remove(s) }) }, nil
}

// Close detaches the broker from its bus and stops all of its subscriptions.
// It is idempotent.
func (b *Broker) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	b.bus.mu.Lock()
	delete(b.bus.members, b)
	b.bus.mu.Unlock()
	b.reg.Close()
	return nil
}
