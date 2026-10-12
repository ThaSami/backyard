// Package backends constructs a docstore.Store from configuration.
package backends

import (
	"context"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
)

// Open returns the store selected by cfg.Type (sqlite | postgres | dynamodb | memory),
// running migrations / table creation as configured. Size and quota limits
// from cfg (MaxDocBytes, MaxDocsPerSite) are enforced by the returned store.
// STUB: replaced by the docstore implementation.
func Open(ctx context.Context, cfg config.DatabaseConfig) (docstore.Store, error) {
	panic("docstore/backends: not implemented")
}
