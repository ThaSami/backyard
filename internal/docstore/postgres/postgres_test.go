package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/conformance"
	"github.com/thasami/backyard/internal/docstore/postgres"
)

// testDSN returns the test database DSN or skips the test.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	_ = conn.Close(ctx)
	return dsn
}

// testSchema returns a unique schema name that is dropped after the test.
func testSchema(t *testing.T, dsn string) string {
	t.Helper()
	schema := fmt.Sprintf("backyard_test_%08x", rand.Uint32())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Errorf("cleanup connect: %v", err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`); err != nil {
			t.Errorf("cleanup drop schema: %v", err)
		}
	})
	return schema
}

func openStore(t *testing.T, cfg config.PostgresConfig) *postgres.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := postgres.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func boolPtr(b bool) *bool { return &b }

func TestConformance(t *testing.T) {
	dsn := testDSN(t)
	s := openStore(t, config.PostgresConfig{DSN: dsn, Schema: testSchema(t, dsn), MaxConns: 20, AutoMigrate: boolPtr(true)})
	conformance.Run(t, func(t *testing.T) docstore.Store { return s })
}

func TestMigrationsConcurrentAndIdempotent(t *testing.T) {
	dsn := testDSN(t)
	schema := testSchema(t, dsn)
	cfg := config.PostgresConfig{DSN: dsn, Schema: schema, MaxConns: 4}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for range 6 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			s, err := postgres.Open(ctx, cfg)
			if err == nil {
				err = s.Close()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Open: %v", err)
		}
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var v int
	if err := conn.QueryRow(ctx, `SELECT version FROM `+pgx.Identifier{schema, "backyard_schema_version"}.Sanitize()+
		` WHERE component = 'docstore'`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Errorf("schema version = %d, want 1", v)
	}
	// Collation of the key columns must be "C" (byte-order sorting).
	var coll string
	if err := conn.QueryRow(ctx, `SELECT collation_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'backyard_docs' AND column_name = 'id'`, schema).Scan(&coll); err != nil {
		t.Fatal(err)
	}
	if coll != "C" {
		t.Errorf("id collation = %q, want C", coll)
	}
	// With auto_migrate off, an up-to-date schema opens fine.
	s := openStore(t, config.PostgresConfig{DSN: dsn, Schema: schema, AutoMigrate: boolPtr(false)})
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAutoMigrateDisabledRequiresSchema(t *testing.T) {
	dsn := testDSN(t)
	schema := testSchema(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := postgres.Open(ctx, config.PostgresConfig{DSN: dsn, Schema: schema, AutoMigrate: boolPtr(false)})
	if err == nil {
		t.Fatal("Open with auto_migrate=false on an empty schema succeeded")
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := postgres.Open(ctx, config.PostgresConfig{}); err == nil {
		t.Error("Open without DSN succeeded")
	}
	_, err := postgres.Open(ctx, config.PostgresConfig{DSN: "postgres://user:s3cret@[::1"})
	if err == nil {
		t.Fatal("Open with malformed DSN succeeded")
	}
	if got := err.Error(); strings.Contains(got, "s3cret") {
		t.Errorf("error leaks the password: %q", got)
	}
}

func TestNULCharacterRejected(t *testing.T) {
	dsn := testDSN(t)
	s := openStore(t, config.PostgresConfig{DSN: dsn, Schema: testSchema(t, dsn)})
	ctx := context.Background()
	_, err := s.Create(ctx, "site", "c", "nul", map[string]any{"s": "a\x00b"}, "")
	if !errors.Is(err, docstore.ErrInvalid) {
		t.Errorf("Create with NUL = %v, want ErrInvalid", err)
	}
}

func TestSharedPoolNotClosed(t *testing.T) {
	dsn := testDSN(t)
	schema := testSchema(t, dsn)
	openStore(t, config.PostgresConfig{DSN: dsn, Schema: schema})
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	s, err := postgres.New(ctx, pool, schema, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Errorf("pool closed by Store.Close: %v", err)
	}
}
