// Package pubsubtest is the conformance suite every pubsub.Broker
// implementation must pass. Backend packages call Run from their tests:
//
//	func TestConformance(t *testing.T) {
//		pubsubtest.Run(t, func(t *testing.T) pubsub.Broker { return newBroker(t) })
//	}
package pubsubtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/pubsub"
)

// deliveryTimeout bounds how long the suite waits for an expected message.
const deliveryTimeout = 15 * time.Second

// Run executes the conformance suite. newBroker must return a new, open broker
// each time it is called; all brokers returned during one Run must share a
// backend (the same Redis server and prefix, the same Postgres database and
// channel, or memory peers of one bus) so that cross-broker delivery can be
// tested. The suite closes every broker it opens; Close must be idempotent.
// Topics are unique per subtest, so brokers may share a backend with other
// tests running concurrently.
func Run(t *testing.T, newBroker func(t *testing.T) pubsub.Broker) {
	open := func(t *testing.T) pubsub.Broker {
		t.Helper()
		b := newBroker(t)
		if b == nil {
			t.Fatal("newBroker returned nil")
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}

	t.Run("PublishSubscribe", func(t *testing.T) {
		b := open(t)
		topic := uniqueTopic("basic")
		c := newCollector()
		subscribe(t, b, topic, c.handle)
		publish(t, b, topic, []byte("hello"))
		c.expect(t, []byte("hello"))
	})

	t.Run("MultipleSubscribers", func(t *testing.T) {
		b := open(t)
		topic := uniqueTopic("multi")
		c1, c2, c3 := newCollector(), newCollector(), newCollector()
		subscribe(t, b, topic, c1.handle)
		subscribe(t, b, topic, c2.handle)
		subscribe(t, b, topic, c3.handle)
		publish(t, b, topic, []byte("one"))
		publish(t, b, topic, []byte("two"))
		for i, c := range []*collector{c1, c2, c3} {
			if got := c.next(t); string(got) != "one" {
				t.Fatalf("subscriber %d: first message %q, want %q", i, got, "one")
			}
			if got := c.next(t); string(got) != "two" {
				t.Fatalf("subscriber %d: second message %q, want %q", i, got, "two")
			}
		}
	})

	t.Run("TopicIsolation", func(t *testing.T) {
		b := open(t)
		topicA, topicB := uniqueTopic("iso-a"), uniqueTopic("iso-b")
		ca, cb := newCollector(), newCollector()
		subscribe(t, b, topicA, ca.handle)
		subscribe(t, b, topicB, cb.handle)
		publish(t, b, topicA, []byte("for-a"))
		publish(t, b, topicB, []byte("for-b"))
		ca.expect(t, []byte("for-a"))
		cb.expect(t, []byte("for-b"))
		// Topics are exact strings: a prefix is a different topic.
		cp := newCollector()
		subscribe(t, b, topicA[:len(topicA)-1], cp.handle)
		publish(t, b, topicA, []byte("again"))
		ca.expect(t, []byte("again"))
		cp.expectNone(t, 200*time.Millisecond)
	})

	t.Run("CrossBroker", func(t *testing.T) {
		b1, b2 := open(t), open(t)
		topic := uniqueTopic("cross")
		c1, c2 := newCollector(), newCollector()
		subscribe(t, b1, topic, c1.handle)
		subscribe(t, b2, topic, c2.handle)

		publish(t, b1, topic, []byte("from-1"))
		c1.expect(t, []byte("from-1")) // the publishing instance receives its own messages
		c2.expect(t, []byte("from-1"))

		publish(t, b2, topic, []byte("from-2"))
		c1.expect(t, []byte("from-2"))
		c2.expect(t, []byte("from-2"))
	})

	t.Run("Cancel", func(t *testing.T) {
		b1, b2 := open(t), open(t)
		topic := uniqueTopic("cancel")
		gone, stay, remote := newCollector(), newCollector(), newCollector()
		cancel := subscribe(t, b1, topic, gone.handle)
		subscribe(t, b1, topic, stay.handle)
		subscribe(t, b2, topic, remote.handle)

		publish(t, b2, topic, []byte("before"))
		gone.expect(t, []byte("before"))
		stay.expect(t, []byte("before"))
		remote.expect(t, []byte("before"))

		cancel()
		cancel() // idempotent

		publish(t, b2, topic, []byte("after"))
		stay.expect(t, []byte("after"))
		remote.expect(t, []byte("after"))
		gone.expectNone(t, 100*time.Millisecond)
	})

	t.Run("CancelLastSubscriber", func(t *testing.T) {
		b1, b2 := open(t), open(t)
		topic := uniqueTopic("cancel-last")
		c := newCollector()
		cancel := subscribe(t, b1, topic, c.handle)
		cancel()
		witness := newCollector()
		subscribe(t, b2, topic, witness.handle)
		publish(t, b2, topic, []byte("nobody-on-b1"))
		witness.expect(t, []byte("nobody-on-b1"))
		c.expectNone(t, 100*time.Millisecond)

		c2 := newCollector()
		subscribe(t, b1, topic, c2.handle)
		publish(t, b2, topic, []byte("back"))
		c2.expect(t, []byte("back"))
	})

	t.Run("CancelFromHandler", func(t *testing.T) {
		b := open(t)
		topic := uniqueTopic("cancel-in-handler")
		var (
			cancel   func()
			mu       sync.Mutex
			calls    int
			canceled = make(chan struct{})
		)
		ready := make(chan struct{})
		h := func([]byte) {
			<-ready
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				cancel()
				close(canceled)
			}
		}
		cancel = subscribe(t, b, topic, h)
		close(ready)
		witness := newCollector()
		subscribe(t, b, topic, witness.handle)
		publish(t, b, topic, []byte("first"))
		select {
		case <-canceled:
		case <-time.After(deliveryTimeout):
			t.Fatal("handler did not run / cancel from handler blocked")
		}
		witness.expect(t, []byte("first"))
		publish(t, b, topic, []byte("second"))
		witness.expect(t, []byte("second"))
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if calls != 1 {
			t.Fatalf("handler called %d times after canceling itself, want 1", calls)
		}
	})

	t.Run("LargePayload", func(t *testing.T) {
		b1, b2 := open(t), open(t)
		topic := uniqueTopic("large")
		c1, c2 := newCollector(), newCollector()
		subscribe(t, b1, topic, c1.handle)
		subscribe(t, b2, topic, c2.handle)
		for _, size := range []int{200 << 10, pubsub.MaxPayloadBytes} {
			payload := randomBytes(t, size)
			publish(t, b1, topic, payload)
			c1.expect(t, payload)
			c2.expect(t, payload)
		}
	})

	t.Run("BinaryAndEmptyPayloads", func(t *testing.T) {
		b1, b2 := open(t), open(t)
		topic := uniqueTopic("binary")
		c := newCollector()
		subscribe(t, b2, topic, c.handle)
		payloads := [][]byte{
			{},
			{0},
			{0xff, 0xfe, 0x00, 0x80, '"', '\\', '\n'},
			[]byte(`{"json":"payload","n":[1,2,3]}`),
			[]byte("unicode: héllo wörld ✓"),
		}
		for _, p := range payloads {
			publish(t, b1, topic, p)
		}
		for _, p := range payloads {
			c.expect(t, p)
		}
	})

	t.Run("PayloadIsCopied", func(t *testing.T) {
		b := open(t)
		topic := uniqueTopic("copy")
		c := newCollector()
		subscribe(t, b, topic, c.handle)
		buf := []byte("original")
		publish(t, b, topic, buf)
		copy(buf, "mutated!")
		c.expect(t, []byte("original"))
	})

	t.Run("OrderingPerPublisher", func(t *testing.T) {
		b1, b2 := open(t), open(t)
		topic := uniqueTopic("order")
		const n = 300
		local, remote := newCollector(), newCollector()
		subscribe(t, b1, topic, local.handle)
		subscribe(t, b2, topic, remote.handle)
		for i := range n {
			size := 8
			if i%25 == 0 {
				size = 20 << 10 // interleave large messages (transport spill paths)
			}
			p := make([]byte, size)
			binary.BigEndian.PutUint64(p, uint64(i))
			publish(t, b1, topic, p)
		}
		for name, c := range map[string]*collector{"local": local, "remote": remote} {
			for i := range n {
				got := c.next(t)
				if len(got) < 8 {
					t.Fatalf("%s: message %d too short (%d bytes)", name, i, len(got))
				}
				if seq := binary.BigEndian.Uint64(got); seq != uint64(i) {
					t.Fatalf("%s: message %d has sequence %d (out of order)", name, i, seq)
				}
			}
		}
	})

	t.Run("SequentialHandler", func(t *testing.T) {
		b := open(t)
		topic := uniqueTopic("sequential")
		const n = 40
		var active, maxActive, count atomic.Int32
		done := make(chan struct{})
		subscribe(t, b, topic, func([]byte) {
			a := active.Add(1)
			for {
				m := maxActive.Load()
				if a <= m || maxActive.CompareAndSwap(m, a) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			if count.Add(1) == n {
				close(done)
			}
		})
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { publish(t, b, topic, fmt.Appendf(nil, "m%d", i)) })
		}
		wg.Wait()
		select {
		case <-done:
		case <-time.After(deliveryTimeout):
			t.Fatalf("received %d of %d messages", count.Load(), n)
		}
		if m := maxActive.Load(); m != 1 {
			t.Fatalf("handler ran %d times concurrently for one subscription, want 1", m)
		}
	})

	t.Run("SlowHandlerDoesNotBlock", func(t *testing.T) {
		b := open(t)
		topic := uniqueTopic("slow")
		release := make(chan struct{})
		slow := newCollector()
		subscribe(t, b, topic, func(p []byte) {
			<-release
			slow.handle(p)
		})
		fast := newCollector()
		subscribe(t, b, topic, fast.handle)
		const n = 50
		start := time.Now()
		for i := range n {
			publish(t, b, topic, fmt.Appendf(nil, "%d", i))
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("publishing %d messages with a blocked subscriber took %v", n, d)
		}
		// The fast subscriber is not held back by the blocked one.
		for i := range n {
			fast.expect(t, fmt.Appendf(nil, "%d", i))
		}
		close(release)
		for i := range n {
			slow.expect(t, fmt.Appendf(nil, "%d", i))
		}
	})

	t.Run("Validation", func(t *testing.T) {
		b := open(t)
		ctx := testContext(t)
		topic := uniqueTopic("validation")
		if err := b.Publish(ctx, topic, make([]byte, pubsub.MaxPayloadBytes+1)); !errors.Is(err, pubsub.ErrTooLarge) {
			t.Errorf("Publish(oversized) = %v, want ErrTooLarge", err)
		}
		if err := b.Publish(ctx, "", []byte("x")); !errors.Is(err, pubsub.ErrInvalidTopic) {
			t.Errorf("Publish(empty topic) = %v, want ErrInvalidTopic", err)
		}
		if _, err := b.Subscribe(ctx, "", func([]byte) {}); !errors.Is(err, pubsub.ErrInvalidTopic) {
			t.Errorf("Subscribe(empty topic) = %v, want ErrInvalidTopic", err)
		}
		if _, err := b.Subscribe(ctx, topic, nil); !errors.Is(err, pubsub.ErrNilHandler) {
			t.Errorf("Subscribe(nil handler) = %v, want ErrNilHandler", err)
		}
		// The broker keeps working after rejected calls.
		c := newCollector()
		subscribe(t, b, topic, c.handle)
		publish(t, b, topic, []byte("still-works"))
		c.expect(t, []byte("still-works"))
	})

	t.Run("Close", func(t *testing.T) {
		b1, b2 := open(t), open(t)
		ctx := testContext(t)
		topic := uniqueTopic("close")
		c1, c2 := newCollector(), newCollector()
		cancel1 := subscribe(t, b1, topic, c1.handle)
		subscribe(t, b2, topic, c2.handle)

		if err := b1.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := b1.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		cancel1() // canceling after Close is harmless
		if err := b1.Publish(ctx, topic, []byte("x")); !errors.Is(err, pubsub.ErrClosed) {
			t.Errorf("Publish after Close = %v, want ErrClosed", err)
		}
		if _, err := b1.Subscribe(ctx, topic, func([]byte) {}); !errors.Is(err, pubsub.ErrClosed) {
			t.Errorf("Subscribe after Close = %v, want ErrClosed", err)
		}

		// Other brokers on the backend are unaffected.
		publish(t, b2, topic, []byte("after-close"))
		c2.expect(t, []byte("after-close"))
		c1.expectNone(t, 200*time.Millisecond)
	})
}

// collector records payloads delivered to a handler.
type collector struct {
	ch chan []byte
}

func newCollector() *collector { return &collector{ch: make(chan []byte, 4096)} }

func (c *collector) handle(p []byte) {
	cp := make([]byte, len(p))
	copy(cp, p)
	c.ch <- cp
}

func (c *collector) next(t *testing.T) []byte {
	t.Helper()
	select {
	case p := <-c.ch:
		return p
	case <-time.After(deliveryTimeout):
		t.Fatal("timed out waiting for a message")
		return nil
	}
}

func (c *collector) expect(t *testing.T, want []byte) {
	t.Helper()
	got := c.next(t)
	if !bytes.Equal(got, want) {
		t.Fatalf("received %s, want %s", describe(got), describe(want))
	}
}

func (c *collector) expectNone(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case p := <-c.ch:
		t.Fatalf("unexpected message %s", describe(p))
	case <-time.After(wait):
	}
}

func describe(p []byte) string {
	if len(p) > 64 {
		return fmt.Sprintf("%d bytes (%x…)", len(p), p[:16])
	}
	return fmt.Sprintf("%q", p)
}

func subscribe(t *testing.T, b pubsub.Broker, topic string, h pubsub.Handler) func() {
	t.Helper()
	cancel, err := b.Subscribe(testContext(t), topic, h)
	if err != nil {
		t.Fatalf("Subscribe(%q): %v", topic, err)
	}
	if cancel == nil {
		t.Fatalf("Subscribe(%q) returned a nil cancel func", topic)
	}
	t.Cleanup(cancel)
	return cancel
}

func publish(t *testing.T, b pubsub.Broker, topic string, payload []byte) {
	t.Helper()
	if err := b.Publish(testContext(t), topic, payload); err != nil {
		t.Errorf("Publish(%q, %d bytes): %v", topic, len(payload), err)
	}
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), deliveryTimeout)
	t.Cleanup(cancel)
	return ctx
}

func uniqueTopic(name string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("pubsubtest.%s.%x", name, b)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	p := make([]byte, n)
	if _, err := rand.Read(p); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return p
}
