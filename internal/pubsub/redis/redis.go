// Package redis implements pubsub.Broker on Redis (or Valkey, ElastiCache,
// Memorystore) PUBLISH/SUBSCRIBE.
//
// A Broker holds one dedicated pub/sub connection on which channels are
// subscribed and unsubscribed dynamically as local subscribers come and go;
// incoming messages are dispatched to local handlers through per-subscription
// queues. go-redis health-checks that connection and transparently reconnects
// and resubscribes after failures (messages published while disconnected are
// lost, which matches the at-most-once contract). Publishing uses the client's
// regular connection pool.
//
// Redis pub/sub channels are global to a server (not per logical database), so
// every topic is prefixed (default "backyard:", or "backyard:<db>:" when the
// URL selects a database other than 0) to keep deployments that share a
// server apart.
package redis

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/internal/fanout"
)

// Defaults for Options.
const (
	DefaultSubscribeTimeout = 10 * time.Second
	defaultOpTimeout        = 5 * time.Second
	defaultChannelSize      = 4096
)

// Options configure a Broker.
type Options struct {
	// URL is a redis:// or rediss:// URL (see redis.ParseURL). Ignored when
	// Client is set.
	URL string
	// Client, if set, is used instead of dialing URL. It is not closed by
	// Broker.Close. Use this to share a client or to connect to a cluster.
	Client goredis.UniversalClient
	// Prefix is prepended to every topic to form the Redis channel name.
	// Default "backyard:" (plus "<db>:" when the URL selects db > 0).
	Prefix string
	// SubscribeTimeout bounds how long Subscribe waits for Redis to confirm a
	// new channel subscription when the caller's context has no earlier
	// deadline (default 10s).
	SubscribeTimeout time.Duration
	// Logger receives operational logs (default slog.Default()).
	Logger *slog.Logger
	// QueueLen and QueueBytes bound the undelivered messages per local
	// subscription (defaults: 10000 messages, 64 MiB).
	QueueLen   int
	QueueBytes int
}

// Broker is a Redis-backed pubsub.Broker.
type Broker struct {
	client     goredis.UniversalClient
	ownsClient bool
	ps         *goredis.PubSub
	prefix     string
	subTimeout time.Duration
	log        *slog.Logger
	reg        *fanout.Registry

	// sem (capacity 1) serializes SUBSCRIBE/UNSUBSCRIBE decisions and
	// commands so that the order of commands on the wire matches the local
	// first-subscribe/last-cancel state. It is a channel rather than a mutex
	// so that Subscribe can give up when its context ends.
	sem chan struct{}

	// Channels whose last local subscriber left are unsubscribed by a
	// background worker, so cancel never blocks on the network.
	unsubMu   sync.Mutex
	unsubSet  map[string]struct{}
	unsubWake chan struct{}

	waitMu  sync.Mutex
	waiters map[string][]chan struct{}

	closed     atomic.Bool
	closeOnce  sync.Once
	stop       chan struct{} // closed by Close
	readerDone chan struct{}
	workerDone chan struct{}
}

var _ pubsub.Broker = (*Broker)(nil)

// New connects to Redis, verifies connectivity with PING and starts the
// subscription reader.
func New(ctx context.Context, opts Options) (*Broker, error) {
	client := opts.Client
	owns := false
	db := 0
	if client == nil {
		if opts.URL == "" {
			return nil, errors.New("pubsub/redis: url is required")
		}
		ro, err := goredis.ParseURL(opts.URL)
		if err != nil {
			// The URL may embed a password: do not echo it.
			return nil, fmt.Errorf("pubsub/redis: invalid url: %w", redactURLError(err))
		}
		db = ro.DB
		client = goredis.NewClient(ro)
		owns = true
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "backyard:"
		if db > 0 {
			prefix = fmt.Sprintf("backyard:%d:", db)
		}
	}
	if opts.SubscribeTimeout <= 0 {
		opts.SubscribeTimeout = DefaultSubscribeTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	pctx, cancel := context.WithTimeout(ctx, defaultOpTimeout)
	err := client.Ping(pctx).Err()
	cancel()
	if err != nil {
		if owns {
			_ = client.Close()
		}
		return nil, fmt.Errorf("pubsub/redis: ping: %w", err)
	}

	b := &Broker{
		client:     client,
		ownsClient: owns,
		prefix:     prefix,
		subTimeout: opts.SubscribeTimeout,
		log:        opts.Logger.With("component", "pubsub", "backend", "redis"),
		reg: fanout.New(fanout.Options{
			QueueLen: opts.QueueLen, QueueBytes: opts.QueueBytes, Logger: opts.Logger,
		}),
		sem:        make(chan struct{}, 1),
		unsubSet:   make(map[string]struct{}),
		unsubWake:  make(chan struct{}, 1),
		waiters:    make(map[string][]chan struct{}),
		stop:       make(chan struct{}),
		readerDone: make(chan struct{}),
		workerDone: make(chan struct{}),
	}
	// A PubSub without channels connects lazily; the reader goroutine below
	// establishes the connection and keeps it healthy (periodic PING,
	// reconnect + resubscribe on failure).
	b.ps = client.Subscribe(context.Background())
	ch := b.ps.ChannelWithSubscriptions(goredis.WithChannelSize(defaultChannelSize))
	go b.read(ch)
	go b.unsubscriber()
	return b, nil
}

func (b *Broker) channel(topic string) string { return b.prefix + topic }

func (b *Broker) read(ch <-chan any) {
	defer close(b.readerDone)
	for msg := range ch {
		switch m := msg.(type) {
		case *goredis.Message:
			topic, ok := strings.CutPrefix(m.Channel, b.prefix)
			if !ok {
				continue
			}
			b.reg.Dispatch(topic, []byte(m.Payload))
		case *goredis.Subscription:
			if m.Kind == "subscribe" {
				b.notifySubscribed(m.Channel)
			}
		}
	}
}

func (b *Broker) notifySubscribed(channel string) {
	b.waitMu.Lock()
	ws := b.waiters[channel]
	delete(b.waiters, channel)
	b.waitMu.Unlock()
	for _, w := range ws {
		close(w)
	}
}

func (b *Broker) addWaiter(channel string) chan struct{} {
	w := make(chan struct{})
	b.waitMu.Lock()
	b.waiters[channel] = append(b.waiters[channel], w)
	b.waitMu.Unlock()
	return w
}

func (b *Broker) removeWaiter(channel string, w chan struct{}) {
	b.waitMu.Lock()
	defer b.waitMu.Unlock()
	ws := b.waiters[channel]
	for i, x := range ws {
		if x == w {
			ws = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(ws) == 0 {
		delete(b.waiters, channel)
	} else {
		b.waiters[channel] = ws
	}
}

// Publish sends payload to every subscriber of topic on all instances.
func (b *Broker) Publish(ctx context.Context, topic string, payload []byte) error {
	if b.closed.Load() {
		return pubsub.ErrClosed
	}
	if err := pubsub.ValidatePublish(topic, payload); err != nil {
		return err
	}
	if err := b.client.Publish(ctx, b.channel(topic), payload).Err(); err != nil {
		return fmt.Errorf("pubsub/redis: publish %q: %w", topic, err)
	}
	return nil
}

// Subscribe registers h for topic. When this is the first local subscriber of
// topic, Subscribe returns only after Redis confirmed the channel
// subscription, so a message published after Subscribe returns is delivered.
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
	select {
	case b.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("pubsub/redis: subscribe %q: %w", topic, ctx.Err())
	case <-b.stop:
		return nil, pubsub.ErrClosed
	}
	defer func() { <-b.sem }()
	s, first, err := b.reg.Add(topic, h)
	if err != nil {
		return nil, err
	}
	if first {
		if err := b.subscribeChannel(ctx, b.channel(topic)); err != nil {
			if b.reg.Remove(s) && !errors.Is(err, pubsub.ErrClosed) {
				// go-redis remembers the channel even when the command
				// failed; make sure it is dropped again.
				b.scheduleUnsubscribe(topic)
			}
			return nil, fmt.Errorf("pubsub/redis: subscribe %q: %w", topic, err)
		}
	}
	var once sync.Once
	return func() { once.Do(func() { b.cancel(s) }) }, nil
}

// subscribeChannel issues SUBSCRIBE and waits for the confirmation. Called
// while holding sem.
func (b *Broker) subscribeChannel(ctx context.Context, channel string) error {
	ctx, cancel := context.WithTimeout(ctx, b.subTimeout)
	defer cancel()
	w := b.addWaiter(channel)
	if err := b.ps.Subscribe(ctx, channel); err != nil {
		if errors.Is(err, goredis.ErrClosed) {
			b.removeWaiter(channel, w)
			return pubsub.ErrClosed
		}
		// go-redis remembers the channel and resubscribes once its reader
		// re-establishes the connection; keep waiting for the confirmation.
		b.log.Debug("subscribe command failed; waiting for reconnect", "err", err)
	}
	select {
	case <-w:
		return nil
	case <-b.readerDone:
		b.removeWaiter(channel, w)
		return pubsub.ErrClosed
	case <-ctx.Done():
		b.removeWaiter(channel, w)
		return fmt.Errorf("waiting for subscription confirmation: %w", ctx.Err())
	}
}

// cancel removes one local subscription. When it was the topic's last one,
// the Redis channel is unsubscribed in the background.
func (b *Broker) cancel(s *fanout.Sub) {
	if b.reg.Remove(s) {
		b.scheduleUnsubscribe(s.Topic())
	}
}

func (b *Broker) scheduleUnsubscribe(topic string) {
	if b.closed.Load() {
		return
	}
	b.unsubMu.Lock()
	b.unsubSet[topic] = struct{}{}
	b.unsubMu.Unlock()
	select {
	case b.unsubWake <- struct{}{}:
	default:
	}
}

func (b *Broker) nextUnsubscribe() (string, bool) {
	b.unsubMu.Lock()
	defer b.unsubMu.Unlock()
	for topic := range b.unsubSet {
		delete(b.unsubSet, topic)
		return topic, true
	}
	return "", false
}

// unsubscriber issues UNSUBSCRIBE for channels whose last local subscriber
// left. The decision is re-checked under sem: a topic that got a new local
// subscriber meanwhile stays subscribed.
func (b *Broker) unsubscriber() {
	defer close(b.workerDone)
	for {
		select {
		case <-b.stop:
			return
		case <-b.unsubWake:
		}
		for {
			topic, ok := b.nextUnsubscribe()
			if !ok {
				break
			}
			select {
			case b.sem <- struct{}{}:
			case <-b.stop:
				return
			}
			if !b.reg.Has(topic) {
				b.unsubscribeChannel(b.channel(topic))
			}
			<-b.sem
		}
	}
}

// unsubscribeChannel issues UNSUBSCRIBE (best effort). Called while holding
// sem.
func (b *Broker) unsubscribeChannel(channel string) {
	if b.closed.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultOpTimeout)
	defer cancel()
	// go-redis forgets the channel even if the command cannot be sent, so a
	// later reconnect will not resubscribe it.
	if err := b.ps.Unsubscribe(ctx, channel); err != nil && !errors.Is(err, goredis.ErrClosed) {
		b.log.Debug("unsubscribe failed", "err", err)
	}
}

// Close stops all subscriptions, closes the pub/sub connection and, when the
// broker created the client from a URL, the client. It is idempotent.
func (b *Broker) Close() error {
	var err error
	b.closeOnce.Do(func() {
		b.closed.Store(true)
		close(b.stop)
		b.reg.Close()
		if e := b.ps.Close(); e != nil && !errors.Is(e, goredis.ErrClosed) {
			err = fmt.Errorf("pubsub/redis: close pubsub: %w", e)
		}
		// The deadline covers both waits (a context's Done channel stays
		// closed once it fires).
		wctx, cancel := context.WithTimeout(context.Background(), defaultOpTimeout)
		defer cancel()
		select {
		case <-b.readerDone:
		case <-wctx.Done():
			b.log.Warn("subscription reader did not stop in time")
		}
		select {
		case <-b.workerDone:
		case <-wctx.Done():
			b.log.Warn("unsubscribe worker did not stop in time")
		}
		if b.ownsClient {
			if e := b.client.Close(); e != nil && err == nil {
				err = fmt.Errorf("pubsub/redis: close client: %w", e)
			}
		}
	})
	return err
}

// redactURLError strips anything that looks like a URL from a parse error so
// that credentials embedded in the URL never reach logs.
func redactURLError(err error) error {
	msg := err.Error()
	if i := strings.Index(msg, "://"); i >= 0 {
		return errors.New("malformed redis url")
	}
	return errors.New(msg)
}
