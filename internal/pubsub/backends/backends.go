// Package backends constructs a pubsub.Broker from configuration.
package backends

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/pubsub"
	"github.com/thasami/backyard/internal/pubsub/memory"
	"github.com/thasami/backyard/internal/pubsub/postgres"
	"github.com/thasami/backyard/internal/pubsub/redis"
)

// Open returns the broker selected by cfg.Type (memory | redis | postgres).
// An empty type selects memory. Network backends verify connectivity before
// returning; ctx bounds that startup work only.
func Open(ctx context.Context, cfg config.PubSubConfig) (pubsub.Broker, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "", "memory":
		return memory.New(), nil
	case "redis":
		if cfg.Redis.URL == "" {
			return nil, errors.New("pubsub: pubsub.redis.url is required for pubsub.type=redis")
		}
		b, err := redis.New(ctx, redis.Options{URL: cfg.Redis.URL})
		if err != nil {
			return nil, err
		}
		return b, nil
	case "postgres":
		if cfg.Postgres.DSN == "" {
			return nil, errors.New("pubsub: pubsub.postgres.dsn is required for pubsub.type=postgres")
		}
		b, err := postgres.New(ctx, postgres.Options{DSN: cfg.Postgres.DSN, Channel: cfg.Postgres.Channel})
		if err != nil {
			return nil, err
		}
		return b, nil
	default:
		return nil, fmt.Errorf("pubsub: unknown type %q (want memory, redis or postgres)", cfg.Type)
	}
}
