package sqlsource

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
)

// ---------------------------------------------------------------------------
// Unit tests

func TestPlaceholder(t *testing.T) {
	s := &Source{}
	for i, want := range []string{"$1", "$2", "$10"} {
		if i == 2 {
			i = 9
		}
		if got := s.Placeholder(i); got != want {
			t.Errorf("Placeholder(%d) = %q, want %q", i, got, want)
		}
	}
}

func TestIsoInterval(t *testing.T) {
	cases := []struct {
		iv   pgtype.Interval
		want string
	}{
		{pgtype.Interval{Valid: true}, "PT0S"},
		{pgtype.Interval{Months: 14, Days: 3, Microseconds: 4*3600e6 + 5*60e6 + 6.5e6, Valid: true}, "P1Y2M3DT4H5M6.5S"},
		{pgtype.Interval{Days: 1, Valid: true}, "P1D"},
		{pgtype.Interval{Microseconds: -3600e6, Valid: true}, "PT-1H"},
		{pgtype.Interval{Microseconds: -1500000, Valid: true}, "PT-1.5S"},
		{pgtype.Interval{Microseconds: -500000, Valid: true}, "PT-0.5S"},
		{pgtype.Interval{Months: -13, Valid: true}, "P-1Y-1M"},
		{pgtype.Interval{Microseconds: 1, Valid: true}, "PT0.000001S"},
	}
	for _, c := range cases {
		if got := isoInterval(c.iv); got != c.want {
			t.Errorf("isoInterval(%+v) = %q, want %q", c.iv, got, c.want)
		}
	}
}

func TestNumericValue(t *testing.T) {
	n := func(i int64, exp int32) pgtype.Numeric {
		return pgtype.Numeric{Int: big.NewInt(i), Exp: exp, Valid: true}
	}
	huge, _ := new(big.Int).SetString("12345678901234567890123", 10)
	cases := []struct {
		in   pgtype.Numeric
		want any
	}{
		{n(1225, -2), 12.25},
		{n(150, -2), 1.5},
		{n(1, -1), 0.1},
		{n(5, 3), 5000.0},
		{n(-7, -3), -0.007},
		{n(0, 0), 0.0},
		{pgtype.Numeric{Int: huge, Exp: -3, Valid: true}, "12345678901234567890.123"},
		{pgtype.Numeric{Int: huge, Valid: true}, "12345678901234567890123"},
		{pgtype.Numeric{NaN: true, Valid: true}, "NaN"},
		{pgtype.Numeric{InfinityModifier: pgtype.Infinity, Valid: true}, "Infinity"},
		{pgtype.Numeric{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, "-Infinity"},
		{pgtype.Numeric{}, nil},
	}
	for _, c := range cases {
		if got := numericValue(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("numericValue(%v e%d) = %#v, want %#v", c.in.Int, c.in.Exp, got, c.want)
		}
	}
}

func TestUUIDAndTime(t *testing.T) {
	u := [16]byte{0xa0, 0xee, 0xbc, 0x99, 0x9c, 0x0b, 0x4e, 0xf8, 0xbb, 0x6d, 0x6b, 0xb9, 0xbd, 0x38, 0x0a, 0x11}
	if got := uuidString(u); got != "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11" {
		t.Errorf("uuid = %s", got)
	}
	if got := timeOfDay(12*3600e6 + 34*60e6 + 56e6 + 789000); got != "12:34:56.789" {
		t.Errorf("time = %s", got)
	}
	if got := timeOfDay(0); got != "00:00:00" {
		t.Errorf("time = %s", got)
	}
}

func TestNewErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := New(ctx, "x", config.WarehouseSource{}, Options{}); err == nil {
		t.Error("empty DSN accepted")
	}
	_, err := New(ctx, "x", config.WarehouseSource{DSN: "postgres://u:s3cret@host:notaport/db"}, Options{})
	if err == nil {
		t.Fatal("bad DSN accepted")
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("error leaks password: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Integration tests (BACKYARD_TEST_POSTGRES_DSN)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BACKYARD_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("BACKYARD_TEST_POSTGRES_DSN not set")
	}
	return dsn
}

// fixture creates a unique schema with a table of many types and returns a
// Source on a single-connection pool (so connection reuse is exercised).
func fixture(t *testing.T) (*Source, string) {
	t.Helper()
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	schema := "wh_test_" + hex.EncodeToString(b[:])
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		_ = admin.Close(ctx)
	})
	ddl := strings.ReplaceAll(`
CREATE SCHEMA S;
CREATE TYPE S.mood AS ENUM ('sad', 'happy');
CREATE TABLE S.all_types (
  id int PRIMARY KEY,
  i2 smallint, i4 integer, i8 bigint, f4 real, f8 double precision,
  num numeric, num_big numeric, num_scale numeric(10,2),
  t text, vc varchar(10), ch char(3), b boolean,
  ts timestamp, tstz timestamptz, d date, u uuid, j json, jb jsonb, by bytea,
  arr int[], arr2 int[][], tarr text[], darr date[], empty_arr int[],
  iv interval, mood S.mood, moods S.mood[], ip inet, tm time, nan_f float8, inf_ts timestamptz,
  pt point, oid_col oid, rng int4range, qc "char"
);
INSERT INTO S.all_types VALUES (
  1, 1, 2, 9007199254740993, 0.1, 1.5,
  12.25, 12345678901234567890.123456789, 1.50,
  'hello', 'vc', 'ab', true,
  '2026-01-02 03:04:05.123456', '2026-01-02 03:04:05.123456+02', '2026-01-02',
  'a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11', '{"a":[1,2]}', '{"b":true}', '\xdeadbeef',
  '{1,2,NULL}', '{{1,2},{3,4}}', '{"a","b c"}', '{2026-01-01}', '{}',
  '1 year 2 mons 3 days 04:05:06.5', 'happy', '{sad,happy}', '192.168.0.1/24', '12:34:56.789', 'NaN', 'infinity',
  '(1,2)', 42, '[1,5)', 'x'
);
INSERT INTO S.all_types (id) VALUES (2);
CREATE TABLE S.counter (n int);
INSERT INTO S.counter VALUES (1);
CREATE SEQUENCE S.seq;
`, "S.", schema+".")
	ddl = strings.ReplaceAll(ddl, "CREATE SCHEMA S;", "CREATE SCHEMA "+schema+";")
	if _, err := admin.Exec(ctx, ddl); err != nil {
		t.Fatalf("setup: %v", err)
	}
	src, err := New(context.Background(), "pg", config.WarehouseSource{Type: "postgres", DSN: dsn}, Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		ConfigurePool: func(c *pgxpool.Config) { c.MaxConns = 1 },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	return src, schema
}

func query(t *testing.T, src *Source, sql string, args ...any) *warehouse.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := src.Query(ctx, sql, args, 1000)
	if err != nil {
		t.Fatalf("Query(%q): %v", sql, err)
	}
	return res
}

func TestIntegrationTypes(t *testing.T) {
	src, schema := fixture(t)
	res := query(t, src, "SELECT * FROM "+schema+".all_types ORDER BY id")
	if res.RowCount != 2 || len(res.Rows) != 2 || res.Truncated {
		t.Fatalf("result: %+v", res)
	}
	wantTypes := map[string]string{
		"id": "int4", "i2": "int2", "i4": "int4", "i8": "int8", "f4": "float4", "f8": "float8",
		"num": "numeric", "num_big": "numeric", "num_scale": "numeric",
		"t": "text", "vc": "varchar", "ch": "bpchar", "b": "bool",
		"ts": "timestamp", "tstz": "timestamptz", "d": "date", "u": "uuid", "j": "json", "jb": "jsonb", "by": "bytea",
		"arr": "int4[]", "arr2": "int4[]", "tarr": "text[]", "darr": "date[]", "empty_arr": "int4[]",
		"iv": "interval", "mood": "mood", "moods": "mood[]", "ip": "inet", "tm": "time", "nan_f": "float8", "inf_ts": "timestamptz",
		"pt": "point", "oid_col": "oid", "rng": "int4range", "qc": "char",
	}
	if len(res.Columns) != len(wantTypes) {
		t.Fatalf("columns: %+v", res.Columns)
	}
	for _, c := range res.Columns {
		if wantTypes[c.Name] != c.Type {
			t.Errorf("column %s type = %q, want %q", c.Name, c.Type, wantTypes[c.Name])
		}
	}
	if res.Columns[0].Name != "id" || res.Columns[1].Name != "i2" {
		t.Errorf("column order: %+v", res.Columns[:2])
	}

	want := map[string]any{
		"id": int64(1), "i2": int64(1), "i4": int64(2), "i8": int64(9007199254740993),
		"f4": 0.1, "f8": 1.5,
		"num": 12.25, "num_big": "12345678901234567890.123456789", "num_scale": 1.5,
		"t": "hello", "vc": "vc", "ch": "ab ", "b": true,
		"ts":   "2026-01-02T03:04:05.123456Z",
		"tstz": "2026-01-02T01:04:05.123456Z",
		"d":    "2026-01-02",
		"u":    "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		"j":    map[string]any{"a": []any{1.0, 2.0}},
		"jb":   map[string]any{"b": true},
		"by":   "3q2+7w==",
		"arr":  []any{int64(1), int64(2), nil},
		"arr2": []any{[]any{int64(1), int64(2)}, []any{int64(3), int64(4)}},
		"tarr": []any{"a", "b c"}, "darr": []any{"2026-01-01"}, "empty_arr": []any{},
		"iv": "P1Y2M3DT4H5M6.5S", "mood": "happy", "moods": []any{"sad", "happy"}, "ip": "192.168.0.1/24", "tm": "12:34:56.789",
		"nan_f": "NaN", "inf_ts": "infinity",
		"pt": "(1,2)", "oid_col": int64(42), "rng": "[1,5)", "qc": "x",
	}
	row := res.Rows[0]
	for k, w := range want {
		if got := row[k]; !reflect.DeepEqual(got, w) {
			t.Errorf("%s = %#v (%T), want %#v", k, got, got, w)
		}
	}
	for k, v := range res.Rows[1] {
		if k != "id" && v != nil {
			t.Errorf("NULL column %s = %#v", k, v)
		}
	}
}

func TestIntegrationParams(t *testing.T) {
	src, _ := fixture(t)
	res := query(t, src, "SELECT $1::text AS a, $2::int8 AS b, $3::float8 AS c, $4::bool AS d, $5::text IS NULL AS e, $6::date AS f, $7::numeric AS g",
		"x", int64(5), 1.5, true, nil, "2026-03-04", int64(7))
	want := map[string]any{"a": "x", "b": int64(5), "c": 1.5, "d": true, "e": true, "f": "2026-03-04", "g": 7.0}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("row = %#v", res.Rows[0])
	}
	// Strings are sent in text format and coerced by the server.
	res = query(t, src, "SELECT $1::int AS n", "42")
	if res.Rows[0]["n"] != int64(42) {
		t.Errorf("n = %#v", res.Rows[0]["n"])
	}

	// Duplicate and anonymous column names.
	res = query(t, src, "SELECT 1 AS a, 2 AS a, 3, 4")
	var names []string
	for _, c := range res.Columns {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "a,a_2,?column?,?column?_2" {
		t.Errorf("names = %v", names)
	}
	if len(res.Rows[0]) != 4 {
		t.Errorf("row = %#v", res.Rows[0])
	}

	// Empty results keep their columns.
	res = query(t, src, "SELECT 1 AS x WHERE false")
	if len(res.Rows) != 0 || len(res.Columns) != 1 || res.Columns[0].Name != "x" {
		t.Errorf("empty result = %+v", res)
	}
}

func TestIntegrationTruncation(t *testing.T) {
	src, _ := fixture(t)
	ctx := context.Background()

	res, err := src.Query(ctx, "SELECT g FROM generate_series(1, 10) g", nil, 10)
	if err != nil || res.Truncated || len(res.Rows) != 10 {
		t.Fatalf("exact: %+v %v", res, err)
	}
	res, err = src.Query(ctx, "SELECT g FROM generate_series(1, 100) g", nil, 10)
	if err != nil || !res.Truncated || len(res.Rows) != 10 || res.Rows[9]["g"] != int64(10) {
		t.Fatalf("small overshoot: %+v %v", res, err)
	}

	// A huge result is canceled server-side instead of streamed.
	start := time.Now()
	res, err = src.Query(ctx, "SELECT generate_series(1, 200000000) AS g, repeat('x', 100) AS pad", nil, 5)
	if err != nil || !res.Truncated || len(res.Rows) != 5 {
		t.Fatalf("huge: %+v %v", res, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("huge result took %v", d)
	}
	// The (single) pooled connection is still healthy.
	res = query(t, src, "SELECT 1 AS ok")
	if res.Rows[0]["ok"] != int64(1) {
		t.Fatalf("after cancel: %+v", res)
	}
}

func TestIntegrationReadOnly(t *testing.T) {
	src, schema := fixture(t)
	ctx := context.Background()
	stmts := []string{
		"INSERT INTO " + schema + ".counter VALUES (2)",
		"UPDATE " + schema + ".counter SET n = 5",
		"DELETE FROM " + schema + ".counter",
		"WITH x AS (DELETE FROM " + schema + ".counter RETURNING n) SELECT * FROM x",
		"CREATE TABLE " + schema + ".evil (a int)",
		"SELECT nextval('" + schema + ".seq')",
		"SELECT * INTO " + schema + ".copy FROM " + schema + ".counter",
	}
	for _, sql := range stmts {
		_, err := src.Query(ctx, sql, nil, 10)
		if !errors.Is(err, warehouse.ErrForbidden) {
			t.Errorf("%s: err = %v, want ErrForbidden", sql, err)
		}
	}
	// Switching the transaction to read-write is refused, and COMMIT only
	// ends the current read-only transaction.
	if _, err := src.Query(ctx, "SET TRANSACTION READ WRITE", nil, 10); err == nil {
		t.Error("SET TRANSACTION READ WRITE succeeded")
	}
	if _, err := src.Query(ctx, "COMMIT", nil, 10); err != nil {
		t.Logf("COMMIT: %v", err)
	}
	if _, err := src.Query(ctx, "INSERT INTO "+schema+".counter VALUES (3)", nil, 10); !errors.Is(err, warehouse.ErrForbidden) {
		t.Errorf("insert after COMMIT: %v", err)
	}
	// Multiple statements are impossible on the extended protocol.
	if _, err := src.Query(ctx, "SELECT 1; INSERT INTO "+schema+".counter VALUES (4)", nil, 10); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Errorf("multi-statement: %v", err)
	}
	res := query(t, src, "SELECT count(*) AS n, sum(n) AS s FROM "+schema+".counter")
	if res.Rows[0]["n"] != int64(1) || res.Rows[0]["s"] != int64(1) {
		t.Fatalf("table modified: %+v", res.Rows)
	}
}

func TestIntegrationSimpleProtocolOverride(t *testing.T) {
	dsn := testDSN(t)
	_, schema := fixture(t)
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" {
		t.Skip("DSN is not a URL")
	}
	q := u.Query()
	q.Set("default_query_exec_mode", "simple_protocol")
	u.RawQuery = q.Encode()
	src, err := New(context.Background(), "simple", config.WarehouseSource{DSN: u.String()}, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	_, err = src.Query(context.Background(), "SELECT 1; COMMIT; INSERT INTO "+schema+".counter VALUES (9)", nil, 10)
	if err == nil {
		t.Fatal("multi-statement string executed")
	}
	res := query(t, src, "SELECT count(*) AS n FROM "+schema+".counter")
	if res.Rows[0]["n"] != int64(1) {
		t.Fatalf("table modified: %+v", res.Rows)
	}
}

func TestIntegrationErrors(t *testing.T) {
	src, schema := fixture(t)
	ctx := context.Background()
	cases := []struct {
		sql  string
		args []any
		want error
	}{
		{"SELEC 1", nil, warehouse.ErrBadRequest},
		{"SELECT * FROM " + schema + ".missing", nil, warehouse.ErrBadRequest},
		{"SELECT nope FROM " + schema + ".counter", nil, warehouse.ErrBadRequest},
		{"SELECT 1/0", nil, warehouse.ErrBadRequest},
		{"SELECT $1::int", []any{"abc"}, warehouse.ErrBadRequest},
		{"SELECT $1::int4", []any{1.5}, warehouse.ErrBadRequest},
		{"SELECT $1::int, $2::int", []any{int64(1)}, warehouse.ErrBadRequest},
	}
	for _, c := range cases {
		_, err := src.Query(ctx, c.sql, c.args, 10)
		if !errors.Is(err, c.want) {
			t.Errorf("%s %v: err = %v, want %v", c.sql, c.args, err, c.want)
		}
	}
	// The connection survives every error.
	query(t, src, "SELECT 1")
}

func TestIntegrationTimeoutAndCancel(t *testing.T) {
	src, _ := fixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := src.Query(ctx, "SELECT pg_sleep(10)", nil, 10)
	if !errors.Is(err, warehouse.ErrTimeout) {
		t.Fatalf("timeout: err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("timeout took %v", d)
	}
	query(t, src, "SELECT 1")

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start = time.Now()
	_, err = src.Query(ctx, "SELECT pg_sleep(10)", nil, 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("cancel took %v", d)
	}
	query(t, src, "SELECT 1")
}
