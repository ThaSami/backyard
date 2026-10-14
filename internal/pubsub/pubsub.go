// Package pubsub is the cross-instance message bus used by realtime to fan
// out database events, room messages and presence between server instances.
// Implementations: memory (single instance), redis (PUBLISH/SUBSCRIBE),
// postgres (LISTEN/NOTIFY). backends.Open builds one from configuration.
package pubsub

import (
	"context"
	"errors"
)

// ErrClosed is returned after Close.
var ErrClosed = errors.New("pubsub: closed")

// Handler receives a published payload. It is called sequentially per
// subscription (never concurrently for the same subscription) and must not
// block for long. The payload may be shared with other subscriptions of the
// same message and must be treated as read-only.
//
// Each subscription has a bounded queue: a handler that falls far behind does
// not slow down publishers, but messages that overflow its queue are dropped
// (and logged).
type Handler func(payload []byte)

// Broker publishes payloads to topics and delivers them to subscribers on all
// instances (including the publishing instance). Delivery is at-most-once and
// best-effort; ordering is preserved per topic per publisher.
type Broker interface {
	// Publish sends payload to every current subscriber of topic. Payloads may
	// be up to 1 MiB (implementations handle transport limits transparently).
	Publish(ctx context.Context, topic string, payload []byte) error
	// Subscribe registers h for topic until cancel is called. Topics are exact
	// strings (no wildcards), e.g. "rt.mysite.room.lobby".
	Subscribe(ctx context.Context, topic string, h Handler) (cancel func(), err error)
	// Close stops all subscriptions and releases resources.
	Close() error
}
