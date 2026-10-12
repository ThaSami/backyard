// Package backends constructs a blob.Store from configuration.
package backends

import (
	"context"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
)

// Open returns the store selected by cfg.Type (fs | s3 | gcs | azure).
// STUB: replaced by the blob implementation.
func Open(ctx context.Context, cfg config.StorageConfig) (blob.Store, error) {
	panic("blob/backends: not implemented")
}
