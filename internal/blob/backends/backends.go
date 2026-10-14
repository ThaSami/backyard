// Package backends constructs a blob.Store from configuration.
package backends

import (
	"context"
	"fmt"
	"strings"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/azure"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/blob/gcs"
	"github.com/thasami/backyard/internal/blob/s3"
	"github.com/thasami/backyard/internal/config"
)

// Open returns the store selected by cfg.Type (fs | s3 | gcs | azure; empty
// means fs, matching config defaults). It validates configuration and builds
// clients but does not contact remote services; call Ping for that.
func Open(ctx context.Context, cfg config.StorageConfig) (blob.Store, error) {
	switch typ := strings.ToLower(strings.TrimSpace(cfg.Type)); typ {
	case "fs", "":
		return fs.New(cfg.FS.Root)
	case "s3":
		return s3.New(ctx, cfg.S3)
	case "gcs":
		return gcs.New(ctx, cfg.GCS)
	case "azure":
		return azure.New(ctx, cfg.Azure)
	default:
		return nil, fmt.Errorf("blob: unknown storage type %q (want fs, s3, gcs or azure)", cfg.Type)
	}
}
