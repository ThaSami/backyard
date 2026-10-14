package memory_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/memory"
	"github.com/thasami/backyard/internal/pubsub/pubsubtest"
)

func TestConformance(t *testing.T) {
	root := memory.New()
	t.Cleanup(func() { _ = root.Close() })
	pubsubtest.Run(t, func(t *testing.T) pubsub.Broker { return root.NewPeer() })
}

func TestStandaloneBrokersAreIsolated(t *testing.T) {
	a, b := memory.New(), memory.New()
	defer a.Close()
	defer b.Close()
	ctx := context.Background()
	got := make(chan []byte, 1)
	cancel, err := b.Subscribe(ctx, "t", func(p []byte) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := a.Publish(ctx, "t", []byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		t.Fatalf("broker on a different bus received %q", p)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestQueueOverflowDrops(t *testing.T) {
	b := memory.New(memory.WithQueueLimits(3, 0))
	defer b.Close()
	ctx := context.Background()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var delivered atomic.Int32
	cancel, err := b.Subscribe(ctx, "t", func([]byte) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		delivered.Add(1)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// The first message is taken by the handler (which blocks); the queue then
	// holds 3 more and the rest are dropped.
	if err := b.Publish(ctx, "t", []byte("0")); err != nil {
		t.Fatal(err)
	}
	<-started
	for range 10 {
		if err := b.Publish(ctx, "t", []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for delivered.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if n := delivered.Load(); n != 4 {
		t.Fatalf("delivered %d messages, want 4 (1 in flight + 3 queued)", n)
	}
}

func TestHandlerPanicIsContained(t *testing.T) {
	b := memory.New()
	defer b.Close()
	ctx := context.Background()
	got := make(chan string, 2)
	cancel, err := b.Subscribe(ctx, "t", func(p []byte) {
		if string(p) == "boom" {
			panic("handler bug")
		}
		got <- string(p)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	for _, m := range []string{"boom", "ok"} {
		if err := b.Publish(ctx, "t", []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case m := <-got:
		if m != "ok" {
			t.Fatalf("got %q", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription died after a handler panic")
	}
}

func TestCanceledContext(t *testing.T) {
	b := memory.New()
	defer b.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Publish(ctx, "t", nil); err == nil {
		t.Error("Publish with canceled context succeeded")
	}
	if _, err := b.Subscribe(ctx, "t", func([]byte) {}); err == nil {
		t.Error("Subscribe with canceled context succeeded")
	}
}
