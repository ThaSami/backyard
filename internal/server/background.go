package server

import (
	"context"
	"sync"
)

// maxBackgroundTouches bounds concurrent user-cache writes started by
// requests; further touches are skipped (they are retried on a later request
// once the throttle allows it) so that a burst of new users cannot create an
// unbounded number of goroutines and database writes.
const maxBackgroundTouches = 64

// background tracks fire-and-forget work started by request handlers (user
// cache touches, audit records) so that Close can wait for it.
//
// Unlike sync.WaitGroup it may be waited on while new work is started
// concurrently (Close does not stop the handler).
type background struct {
	mu   sync.Mutex
	n    int           // running tasks
	idle chan struct{} // closed while n == 0
}

func newBackground() *background {
	idle := make(chan struct{})
	close(idle)
	return &background{idle: idle}
}

// run starts fn in a goroutine. With limit > 0, fn is not started (and run
// returns false) when limit tasks are already running.
func (b *background) run(limit int, fn func()) bool {
	b.mu.Lock()
	if limit > 0 && b.n >= limit {
		b.mu.Unlock()
		return false
	}
	if b.n == 0 {
		b.idle = make(chan struct{})
	}
	b.n++
	b.mu.Unlock()
	go func() {
		defer b.done()
		fn()
	}()
	return true
}

func (b *background) done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n--
	if b.n == 0 {
		close(b.idle)
	}
}

// running returns the number of running tasks.
func (b *background) running() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

// wait blocks until no task is running or ctx is done.
func (b *background) wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		idle, n := b.idle, b.n
		b.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-idle:
			// Tasks started after the channel closed get a new channel;
			// loop to wait for those as well.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
