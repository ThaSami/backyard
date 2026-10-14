package backends_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/backends"
	"github.com/thasami/backyard/internal/docstore/conformance"
)

func boolPtr(b bool) *bool { return &b }

func open(t *testing.T, cfg config.DatabaseConfig) docstore.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := backends.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", cfg.Type, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// checkLimits verifies that Open applied the configured limits.
func checkLimits(t *testing.T, s docstore.Store) {
	t.Helper()
	ctx := context.Background()
	site := fmt.Sprintf("lim-%08x", rand.Uint32())
	_, err := s.Create(ctx, site, "c", "big", map[string]any{"x": strings.Repeat("a", 2048)}, "")
	if !errors.Is(err, docstore.ErrTooLarge) {
		t.Errorf("oversized Create = %v, want ErrTooLarge", err)
	}
	for i := range 2 {
		if _, err := s.Create(ctx, site, "c", "", map[string]any{"i": i}, ""); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if _, err := s.Create(ctx, site, "c", "", nil, ""); !errors.Is(err, docstore.ErrQuota) {
		t.Errorf("Create over quota = %v, want ErrQuota", err)
	}
	if err := s.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestOpenMemory(t *testing.T) {
	checkLimits(t, open(t, config.DatabaseConfig{Type: "memory", MaxDocBytes: 1024, MaxDocsPerSite: 2}))
}

func TestOpenSQLiteCreatesDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "backyard.db")
	s := open(t, config.DatabaseConfig{Type: "sqlite", SQLite: config.SQLiteConfig{Path: path}, MaxDocBytes: 1024, MaxDocsPerSite: 2})
	checkLimits(t, s)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file not created: %v", err)
	}
}

func TestOpenDefaultsToSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	s := open(t, config.DatabaseConfig{SQLite: config.SQLiteConfig{Path: path}})
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []config.DatabaseConfig{
		{Type: "mongodb"},
		{Type: "sqlite"},
		{Type: "postgres"},
	} {
		if s, err := backends.Open(ctx, cfg); err == nil {
			_ = s.Close()
			t.Errorf("Open(%+v) succeeded", cfg)
		}
	}
}

// The full conformance suite through Open with production-like limits.
func TestConformanceThroughOpen(t *testing.T) {
	cfg := config.DatabaseConfig{MaxDocBytes: 256 << 10, MaxDocsPerSite: 100000}
	t.Run("memory", func(t *testing.T) {
		c := cfg
		c.Type = "memory"
		s := open(t, c)
		conformance.Run(t, func(t *testing.T) docstore.Store { return s })
	})
	t.Run("sqlite", func(t *testing.T) {
		c := cfg
		c.Type = "sqlite"
		c.SQLite.Path = filepath.Join(t.TempDir(), "db", "backyard.db")
		s := open(t, c)
		conformance.Run(t, func(t *testing.T) docstore.Store { return s })
	})
}

func TestOpenPostgres(t *testing.T) {
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
	schema := fmt.Sprintf("backyard_test_%08x", rand.Uint32())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = conn.Exec(ctx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_ = conn.Close(ctx)
	})
	s := open(t, config.DatabaseConfig{
		Type:        "postgres",
		Postgres:    config.PostgresConfig{DSN: dsn, Schema: schema, MaxConns: 4, AutoMigrate: boolPtr(true)},
		MaxDocBytes: 1024, MaxDocsPerSite: 2,
	})
	checkLimits(t, s)
}

func TestOpenDynamoDB(t *testing.T) {
	ep := os.Getenv("BACKYARD_TEST_DYNAMODB_ENDPOINT")
	if ep == "" {
		t.Skip("BACKYARD_TEST_DYNAMODB_ENDPOINT not set")
	}
	hc := &http.Client{Timeout: 3 * time.Second}
	resp, err := hc.Get(ep)
	if err != nil {
		t.Skipf("DynamoDB endpoint unreachable: %v", err)
	}
	_ = resp.Body.Close()
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	table := fmt.Sprintf("backyard-test-%08x", rand.Uint32())
	t.Cleanup(func() {
		client := awsddb.New(awsddb.Options{
			Region:       "us-east-1",
			BaseEndpoint: aws.String(ep),
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = client.DeleteTable(ctx, &awsddb.DeleteTableInput{TableName: aws.String(table)})
	})
	s := open(t, config.DatabaseConfig{
		Type:        "dynamodb",
		DynamoDB:    config.DynamoDBConfig{Table: table, Region: "us-east-1", Endpoint: ep, CreateTable: true, MaxScan: 100},
		MaxDocBytes: 1024, MaxDocsPerSite: 2,
	})
	checkLimits(t, s)
}
