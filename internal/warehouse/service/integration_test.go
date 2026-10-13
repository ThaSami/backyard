package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
)

// TestPostgresIntegration runs the full service (default source opener,
// sqlsource) against BACKYARD_TEST_POSTGRES_DSN.
func TestPostgresIntegration(t *testing.T) {
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "wh_svc_" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = admin.Close(ctx)
	})
	if _, err := admin.Exec(ctx, `
CREATE SCHEMA `+schema+`;
CREATE TABLE `+schema+`.events (id int, team text, day date, n int);
INSERT INTO `+schema+`.events VALUES
  (1, 'growth', '2026-01-01', 3), (2, 'growth', '2026-01-02', 5), (3, 'infra', '2026-01-02', 7);
`); err != nil {
		t.Fatal(err)
	}
	count := func() int64 {
		var n int64
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+schema+".events").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	cfg := config.WarehouseConfig{
		Sources:     map[string]config.WarehouseSource{"default": {Type: "postgres", DSN: dsn}},
		AllowRawSQL: true,
		MaxRows:     2,
		Timeout:     config.Duration(10 * time.Second),
		Queries: map[string]config.NamedQuery{
			"by_team": {
				SQL:    "SELECT day, n FROM " + schema + ".events WHERE team = @team AND day >= @since ORDER BY day",
				Params: []string{"team", "since"},
			},
		},
	}
	open := func(cfg config.WarehouseConfig) *Service {
		svc, err := NewWithOptions(context.Background(), cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = svc.Close() })
		return svc
	}
	svc := open(cfg)

	// Raw SQL with "?" placeholders (and a "?" inside a literal).
	res, err := svc.Query(ctx, warehouse.QueryRequest{
		SQL:    "SELECT id, team, '?' AS q FROM " + schema + ".events WHERE team = ? AND n > ? ORDER BY id;",
		Params: []any{"growth", 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"id": int64(1), "team": "growth", "q": "?"}, {"id": int64(2), "team": "growth", "q": "?"}}
	if !reflect.DeepEqual(res.Rows, want) || res.Truncated || res.RowCount != 2 {
		t.Fatalf("raw result = %+v", res)
	}

	// Row cap.
	res, err = svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT id FROM " + schema + ".events ORDER BY id"})
	if err != nil || !res.Truncated || res.RowCount != 2 {
		t.Fatalf("capped result = %+v, %v", res, err)
	}

	// Named query with @params.
	res, err = svc.Query(ctx, warehouse.QueryRequest{Query: "by_team", Params: map[string]any{"team": "growth", "since": "2026-01-02"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Rows, []map[string]any{{"day": "2026-01-02", "n": int64(5)}}) {
		t.Fatalf("named result = %+v", res.Rows)
	}
	if !reflect.DeepEqual(res.Columns, []warehouse.Column{{Name: "day", Type: "date"}, {Name: "n", Type: "int4"}}) {
		t.Fatalf("columns = %+v", res.Columns)
	}

	// The guard rejects writes before they reach the database.
	if _, err := svc.Query(ctx, warehouse.QueryRequest{SQL: "INSERT INTO " + schema + ".events VALUES (9, 'x', '2026-01-01', 1)"}); !errors.Is(err, warehouse.ErrForbidden) {
		t.Fatalf("guarded insert: %v", err)
	}

	// With allow_any_statement the READ ONLY transaction still refuses.
	cfg.AllowAnyStatement = true
	anySvc := open(cfg)
	for _, sql := range []string{
		"INSERT INTO " + schema + ".events VALUES (9, 'x', '2026-01-01', 1)",
		"UPDATE " + schema + ".events SET n = 0",
		"DELETE FROM " + schema + ".events",
		"DROP TABLE " + schema + ".events",
		"TRUNCATE " + schema + ".events",
	} {
		if _, err := anySvc.Query(ctx, warehouse.QueryRequest{SQL: sql}); !errors.Is(err, warehouse.ErrForbidden) {
			t.Errorf("allow_any_statement %q: err = %v, want ErrForbidden", sql, err)
		}
	}
	if _, err := anySvc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1; DELETE FROM " + schema + ".events"}); err == nil {
		t.Error("multi-statement accepted")
	}
	if n := count(); n != 3 {
		t.Fatalf("table modified: %d rows", n)
	}

	// Results are capped by size (32 MiB by default) as well as by row
	// count: of four 20 MiB rows only the first fits.
	res, err = svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT g, repeat('x', 20 * 1024 * 1024) AS pad FROM generate_series(1, 4) g"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 1 || res.RowCount != 1 || !res.Truncated || res.Rows[0]["g"] != int64(1) {
		t.Fatalf("wide rows: %d rows, truncated %v", len(res.Rows), res.Truncated)
	}

	// Engine errors are classified.
	if _, err := svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT * FROM " + schema + ".nope"}); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Errorf("missing table: %v", err)
	}
	cfg.AllowAnyStatement = false
	cfg.Timeout = config.Duration(300 * time.Millisecond)
	slow := open(cfg)
	if _, err := slow.Query(ctx, warehouse.QueryRequest{SQL: "SELECT pg_sleep(5)"}); !errors.Is(err, warehouse.ErrTimeout) {
		t.Errorf("timeout: %v", err)
	}
}
