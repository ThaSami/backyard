package fanout

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/pubsub"
)

func TestFirstAndLast(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	h := func([]byte) {}
	s1, first, err := r.Add("t", h)
	if err != nil || !first {
		t.Fatalf("first Add: first=%v err=%v", first, err)
	}
	s2, first, _ := r.Add("t", h)
	if first {
		t.Fatal("second Add reported first")
	}
	if !r.Has("t") || len(r.Topics()) != 1 {
		t.Fatal("topic not registered")
	}
	if r.Remove(s1) {
		t.Fatal("removing one of two reported last")
	}
	if r.Remove(s1) {
		t.Fatal("double remove reported last")
	}
	if !r.Remove(s2) {
		t.Fatal("removing the last did not report last")
	}
	if r.Has("t") {
		t.Fatal("topic still registered")
	}
	if r.Remove(nil) {
		t.Fatal("Remove(nil) reported last")
	}
}

func TestDispatchOrderAndCount(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	_, _, _ = r.Add("t", func(p []byte) {
		mu.Lock()
		got = append(got, string(p))
		n := len(got)
		mu.Unlock()
		if n == 100 {
			close(done)
		}
	})
	if n := r.Dispatch("other", []byte("x")); n != 0 {
		t.Fatalf("dispatch to unknown topic accepted by %d", n)
	}
	for i := range 100 {
		if n := r.Dispatch("t", []byte{byte(i)}); n != 1 {
			t.Fatalf("dispatch accepted by %d", n)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("not delivered")
	}
	for i, p := range got {
		if p[0] != byte(i) {
			t.Fatalf("out of order at %d", i)
		}
	}
}

func TestByteLimit(t *testing.T) {
	r := New(Options{QueueBytes: 10})
	defer r.Close()
	block := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	_, _, _ = r.Add("t", func([]byte) {
		once.Do(func() { close(started) })
		<-block
	})
	r.Dispatch("t", []byte("first")) // taken by the handler
	<-started
	if n := r.Dispatch("t", make([]byte, 50)); n != 1 {
		t.Fatal("an oversized message must be accepted into an empty queue")
	}
	if n := r.Dispatch("t", []byte("x")); n != 0 {
		t.Fatal("queue over its byte limit accepted a message")
	}
	close(block)
}

func TestClose(t *testing.T) {
	r := New(Options{})
	s, _, _ := r.Add("t", func([]byte) {})
	r.Close()
	r.Close()
	if r.Has("t") {
		t.Fatal("subscription survived Close")
	}
	if _, _, err := r.Add("t", func([]byte) {}); !errors.Is(err, pubsub.ErrClosed) {
		t.Fatalf("Add after Close = %v", err)
	}
	if r.Remove(s) {
		t.Fatal("Remove after Close reported last")
	}
	if n := r.Dispatch("t", nil); n != 0 {
		t.Fatal("dispatch after Close accepted")
	}
}
