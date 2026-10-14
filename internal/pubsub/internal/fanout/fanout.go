// Package fanout is the local delivery engine shared by the pubsub brokers.
//
// A Registry maps topics to subscriptions. Every subscription owns a goroutine
// and a bounded FIFO queue, so handlers run sequentially per subscription, in
// the order messages were dispatched, and a slow handler never blocks the
// publisher or the transport reader that calls Dispatch. When a queue is full
// new messages for that subscription are dropped and a (rate-limited) warning
// is logged.
package fanout

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thasami/backyard/internal/pubsub"
)

// Defaults for Options.
const (
	DefaultQueueLen   = 10000
	DefaultQueueBytes = 64 << 20
)

// warnEvery rate-limits "queue overflow" warnings per subscription.
const warnEvery = 10 * time.Second

// Options configure a Registry.
type Options struct {
	// QueueLen is the maximum number of undelivered messages per subscription
	// (default DefaultQueueLen).
	QueueLen int
	// QueueBytes is the maximum number of undelivered payload bytes per
	// subscription (default DefaultQueueBytes). A single message larger than
	// this is still accepted into an empty queue.
	QueueBytes int
	// Logger receives overflow warnings and handler panics (default slog.Default()).
	Logger *slog.Logger
}

// Registry tracks subscriptions by topic. It is safe for concurrent use.
type Registry struct {
	queueLen   int
	queueBytes int
	log        *slog.Logger

	mu     sync.RWMutex
	topics map[string]map[*Sub]struct{}
	closed bool
}

// New returns an empty registry.
func New(opts Options) *Registry {
	if opts.QueueLen <= 0 {
		opts.QueueLen = DefaultQueueLen
	}
	if opts.QueueBytes <= 0 {
		opts.QueueBytes = DefaultQueueBytes
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Registry{
		queueLen:   opts.QueueLen,
		queueBytes: opts.QueueBytes,
		log:        opts.Logger,
		topics:     make(map[string]map[*Sub]struct{}),
	}
}

// Sub is one registered handler.
type Sub struct {
	reg   *Registry
	topic string
	h     pubsub.Handler

	stopped atomic.Bool
	wake    chan struct{}
	done    chan struct{}

	mu       sync.Mutex
	queue    [][]byte
	bytes    int
	dropped  int64
	lastWarn time.Time
}

// Topic returns the subscription's topic.
func (s *Sub) Topic() string { return s.topic }

// Add registers h for topic and starts its delivery goroutine. first reports
// whether topic had no subscribers before. It returns pubsub.ErrClosed after
// Close.
func (r *Registry) Add(topic string, h pubsub.Handler) (s *Sub, first bool, err error) {
	s = &Sub{
		reg:   r,
		topic: topic,
		h:     h,
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, false, pubsub.ErrClosed
	}
	set := r.topics[topic]
	if set == nil {
		set = make(map[*Sub]struct{})
		r.topics[topic] = set
		first = true
	}
	set[s] = struct{}{}
	r.mu.Unlock()
	go s.run()
	return s, first, nil
}

// Remove unregisters s and stops its goroutine (undelivered messages are
// discarded; a handler call in progress completes). last reports whether the
// topic has no subscribers left. Removing an already removed subscription is a
// no-op that reports false.
func (r *Registry) Remove(s *Sub) (last bool) {
	if s == nil {
		return false
	}
	r.mu.Lock()
	set, ok := r.topics[s.topic]
	if ok {
		if _, member := set[s]; member {
			delete(set, s)
			if len(set) == 0 {
				delete(r.topics, s.topic)
				last = true
			}
		}
	}
	r.mu.Unlock()
	s.stop()
	return last
}

// Has reports whether topic has at least one subscriber.
func (r *Registry) Has(topic string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.topics[topic]) > 0
}

// Topics returns the topics that currently have subscribers.
func (r *Registry) Topics() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.topics))
	for t := range r.topics {
		out = append(out, t)
	}
	return out
}

// Dispatch queues payload for every subscriber of topic and returns how many
// subscriptions accepted it. It never blocks on handlers. The payload is shared
// between subscriptions and must not be modified afterwards.
func (r *Registry) Dispatch(topic string, payload []byte) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for s := range r.topics[topic] {
		if s.enqueue(payload) {
			n++
		}
	}
	return n
}

// Close removes every subscription. Later Add calls fail with pubsub.ErrClosed.
func (r *Registry) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	topics := r.topics
	r.topics = make(map[string]map[*Sub]struct{})
	r.mu.Unlock()
	for _, set := range topics {
		for s := range set {
			s.stop()
		}
	}
}

func (s *Sub) stop() {
	if s.stopped.CompareAndSwap(false, true) {
		close(s.done)
		s.mu.Lock()
		s.queue = nil
		s.bytes = 0
		s.mu.Unlock()
	}
}

func (s *Sub) enqueue(p []byte) bool {
	if s.stopped.Load() {
		return false
	}
	s.mu.Lock()
	if len(s.queue) >= s.reg.queueLen || (s.bytes > 0 && s.bytes+len(p) > s.reg.queueBytes) {
		s.dropped++
		now := time.Now()
		warn := now.Sub(s.lastWarn) >= warnEvery
		var dropped int64
		if warn {
			s.lastWarn = now
			dropped = s.dropped
			s.dropped = 0
		}
		s.mu.Unlock()
		if warn {
			s.reg.log.Warn("pubsub: subscriber queue full, dropping messages",
				"topic", s.topic, "dropped", dropped, "queue_len", s.reg.queueLen)
		}
		return false
	}
	s.queue = append(s.queue, p)
	s.bytes += len(p)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return true
}

func (s *Sub) run() {
	for {
		select {
		case <-s.done:
			return
		case <-s.wake:
		}
		for {
			s.mu.Lock()
			batch := s.queue
			s.queue = nil
			s.bytes = 0
			s.mu.Unlock()
			if len(batch) == 0 {
				break
			}
			for i, p := range batch {
				if s.stopped.Load() {
					return
				}
				s.invoke(p)
				batch[i] = nil
			}
		}
	}
}

func (s *Sub) invoke(p []byte) {
	defer func() {
		if v := recover(); v != nil {
			s.reg.log.Error("pubsub: subscriber handler panicked", "topic", s.topic, "panic", v)
		}
	}()
	s.h(p)
}
