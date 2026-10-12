// Package backends constructs a pubsub.Broker from configuration.
package backends

import (
	"context"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/pubsub"
)

// Open returns the broker selected by cfg.Type (memory | redis | postgres).
// STUB: replaced by the pubsub implementation.
func Open(ctx context.Context, cfg config.PubSubConfig) (pubsub.Broker, error) {
	panic("pubsub/backends: not implemented")
}
