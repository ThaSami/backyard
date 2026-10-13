// Package backends constructs a docstore.Store from configuration.
package backends

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/dynamodb"
	"github.com/thasami/backyard/internal/docstore/memory"
	"github.com/thasami/backyard/internal/docstore/postgres"
	"github.com/thasami/backyard/internal/docstore/sqlite"
)

// dynamoItemLimit is DynamoDB's maximum item size; document bodies must stay
// below it with room for keys and metadata.
const dynamoItemLimit = 400 * 1024

// Open returns the store selected by cfg.Type (sqlite | postgres | dynamodb | memory),
// running migrations / table creation as configured. Size and quota limits
// from cfg (MaxDocBytes, MaxDocsPerSite) are enforced by the returned store.
//
// For sqlite the parent directory of cfg.SQLite.Path is created if needed.
// The caller owns the returned store and must Close it.
func Open(ctx context.Context, cfg config.DatabaseConfig) (docstore.Store, error) {
	var (
		s   docstore.Store
		err error
	)
	typ := strings.ToLower(strings.TrimSpace(cfg.Type))
	switch typ {
	case "memory":
		s = memory.New()
	case "sqlite", "":
		typ = "sqlite"
		s, err = openSQLite(ctx, cfg.SQLite)
	case "postgres", "postgresql":
		typ = "postgres"
		var ps *postgres.Store
		ps, err = postgres.Open(ctx, cfg.Postgres)
		if err == nil {
			s = ps
		}
	case "dynamodb":
		if cfg.MaxDocBytes <= 0 || cfg.MaxDocBytes > dynamoItemLimit-8*1024 {
			slog.Warn("docstore/dynamodb: database.max_doc_bytes exceeds what fits in a DynamoDB item (~392 KiB); large documents will be rejected",
				"max_doc_bytes", cfg.MaxDocBytes)
		}
		var ds *dynamodb.Store
		ds, err = dynamodb.Open(ctx, cfg.DynamoDB)
		if err == nil {
			s = ds
		}
	default:
		return nil, fmt.Errorf("docstore: unknown database.type %q (want sqlite, postgres, dynamodb or memory)", cfg.Type)
	}
	if err != nil {
		return nil, err
	}
	slog.Info("docstore: opened", "type", typ, "max_doc_bytes", cfg.MaxDocBytes, "max_docs_per_site", cfg.MaxDocsPerSite)
	return docstore.WithLimits(s, cfg.MaxDocBytes, cfg.MaxDocsPerSite), nil
}

func openSQLite(ctx context.Context, cfg config.SQLiteConfig) (docstore.Store, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("docstore: database.sqlite.path is required")
	}
	if cfg.Path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o750); err != nil {
			return nil, fmt.Errorf("docstore: creating directory for %s: %w", cfg.Path, err)
		}
	}
	s, err := sqlite.Open(ctx, cfg.Path)
	if err != nil {
		return nil, err
	}
	return s, nil
}
