package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"
)

// openInternal returns a broker on BACKYARD_TEST_REDIS_URL with a unique
// prefix, or skips the test.
func openInternal(t *testing.T) *Broker {
	t.Helper()
	url := os.Getenv("BACKYARD_TEST_REDIS_URL")
	if url == "" {
		t.Skip("BACKYARD_TEST_REDIS_URL not set")
	}
	var r [6]byte
	_, _ = rand.Read(r[:])
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := New(ctx, Options{URL: url, Prefix: "backyard-test:" + hex.EncodeToString(r[:]) + ":"})
	if err != nil {
		t.Skipf("redis unreachable: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func numSub(t *testing.T, b *Broker, topic string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n, err := b.client.PubSubNumSub(ctx, b.channel(topic)).Result()
	if err != nil {
		t.Fatal(err)
	}
	return n[b.channel(topic)]
}

// TestSubscribeHonorsContextWhileBusy: a Subscribe queued behind another
// SUBSCRIBE/UNSUBSCRIBE gives up when its context ends instead of waiting for
// the management lock indefinitely.
func TestSubscribeHonorsContextWhileBusy(t *testing.T) {
	b := openInternal(t)
	b.sem <- struct{}{} // simulate a slow command in progress
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := b.Subscribe(ctx, "busy", func([]byte) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Subscribe = %v, want deadline exceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Subscribe took %v", d)
	}
	<-b.sem
	if b.reg.Has("busy") {
		t.Fatal("failed Subscribe left a local subscription behind")
	}
}

// TestCancelDoesNotBlock: cancel never waits for the network; the channel is
// unsubscribed in the background once the management lock is free, unless a
// new local subscriber appeared meanwhile.
func TestCancelDoesNotBlock(t *testing.T) {
	b := openInternal(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()

	cancel, err := b.Subscribe(ctx, "t1", func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	if n := numSub(t, b, "t1"); n != 1 {
		t.Fatalf("NUMSUB after subscribe = %d", n)
	}

	b.sem <- struct{}{} // management busy
	done := make(chan struct{})
	go func() { cancel(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel blocked on the management lock")
	}
	if b.reg.Has("t1") {
		t.Fatal("local subscription not removed by cancel")
	}
	time.Sleep(50 * time.Millisecond)
	if n := numSub(t, b, "t1"); n != 1 {
		t.Fatalf("UNSUBSCRIBE sent while the lock was held (NUMSUB=%d)", n)
	}
	<-b.sem
	deadline := time.Now().Add(5 * time.Second)
	for numSub(t, b, "t1") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("channel not unsubscribed after cancel")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Resubscribing while an unsubscribe is pending keeps the channel.
	got := make(chan []byte, 1)
	cancel2, err := b.Subscribe(ctx, "t2", func(p []byte) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	b.sem <- struct{}{}
	cancel2()
	<-b.sem
	cancel3, err := b.Subscribe(ctx, "t2", func(p []byte) { got <- p })
	if err != nil {
		t.Fatal(err)
	}
	defer cancel3()
	time.Sleep(100 * time.Millisecond) // let the worker process the stale request
	if err := b.Publish(ctx, "t2", []byte("still-here")); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-got:
		if string(p) != "still-here" {
			t.Fatalf("got %q", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale unsubscribe dropped a live subscription")
	}
}

// TestCloseWithPendingUnsubscribe: Close does not hang on queued work.
func TestCloseWithPendingUnsubscribe(t *testing.T) {
	b := openInternal(t)
	ctx, cancelCtx := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCtx()
	cancel, err := b.Subscribe(ctx, "t", func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	b.sem <- struct{}{}
	cancel()
	done := make(chan error, 1)
	go func() { done <- b.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close hung")
	}
	if _, err := b.Subscribe(ctx, "t", func([]byte) {}); err == nil {
		t.Fatal("Subscribe after Close succeeded")
	}
}
