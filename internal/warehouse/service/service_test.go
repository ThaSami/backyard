package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
	"github.com/thasami/backyard/internal/warehouse/internal/conv"
)

// fakeSource records calls and returns canned results.
type fakeSource struct {
	typ string

	mu     sync.Mutex
	calls  []fakeCall
	closed bool

	// query, if set, handles Query; otherwise a one-row result is returned.
	query func(ctx context.Context, sql string, args []any, maxRows int) (*warehouse.Result, error)
}

type fakeCall struct {
	sql         string
	args        []any
	maxRows     int
	maxBytes    int
	hasDeadline bool
	deadline    time.Duration
}

func (f *fakeSource) Query(ctx context.Context, sql string, args []any, lim warehouse.Limits) (*warehouse.Result, error) {
	maxRows := lim.MaxRows
	call := fakeCall{sql: sql, args: args, maxRows: maxRows, maxBytes: lim.MaxBytes}
	if dl, ok := ctx.Deadline(); ok {
		call.hasDeadline = true
		call.deadline = time.Until(dl)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
	if f.query != nil {
		return f.query(ctx, sql, args, maxRows)
	}
	return &warehouse.Result{
		Columns: []warehouse.Column{{Name: "n", Type: "int8"}},
		Rows:    []map[string]any{{"n": int64(1)}},
	}, nil
}

func (f *fakeSource) Placeholder(i int) string {
	if f.typ == "postgres" {
		return "$" + strconv.Itoa(i+1)
	}
	return "?"
}

func (f *fakeSource) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeSource) lastCall(t *testing.T) fakeCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("source was not called")
	}
	return f.calls[len(f.calls)-1]
}

// harness builds a service whose sources are fakes.
type harness struct {
	svc   *Service
	mu    sync.Mutex
	fakes map[string]*fakeSource
	opens atomic.Int32
	// openErr, if set, fails the next opens.
	openErr atomic.Pointer[error]
	// configure customises each fake as it is opened.
	configure func(name string, f *fakeSource)
}

func newHarness(t *testing.T, cfg config.WarehouseConfig, configure ...func(string, *fakeSource)) *harness {
	t.Helper()
	h := &harness{fakes: map[string]*fakeSource{}}
	if len(configure) > 0 {
		h.configure = configure[0]
	}
	svc, err := NewWithOptions(context.Background(), cfg, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func(ctx context.Context, name string, src config.WarehouseSource) (warehouse.Source, error) {
			h.opens.Add(1)
			if e := h.openErr.Load(); e != nil {
				return nil, *e
			}
			f := &fakeSource{typ: src.Type}
			if h.configure != nil {
				h.configure(name, f)
			}
			h.mu.Lock()
			h.fakes[name] = f
			h.mu.Unlock()
			return f, nil
		},
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	h.svc = svc
	return h
}

func (h *harness) fake(t *testing.T, name string) *fakeSource {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	f, ok := h.fakes[name]
	if !ok {
		t.Fatalf("source %q was not opened", name)
	}
	return f
}

func baseConfig() config.WarehouseConfig {
	return config.WarehouseConfig{
		Sources: map[string]config.WarehouseSource{
			"default": {Type: "postgres", Description: "main db", DSN: "postgres://x"},
			"bq":      {Type: "bigquery", ProjectID: "p"},
			"logs":    {Type: "athena", Region: "us-east-1", Workgroup: "primary"},
		},
		DefaultSource: "default",
		AllowRawSQL:   true,
		MaxRows:       100,
		Timeout:       config.Duration(5 * time.Second),
		Queries: map[string]config.NamedQuery{
			"wau": {
				SQL:         "SELECT day, n FROM wau WHERE team = @team AND day >= @since AND note <> '@team' -- @team\n AND alt = @team;",
				Description: "weekly active users",
				Params:      []string{"team", "since"},
			},
			"bq_daily": {
				Source: "bq",
				SQL:    "SELECT @@project_id AS p, x FROM t WHERE d = @day",
				Params: []string{"day"},
			},
			"tables": {SQL: "SHOW TABLES"},
		},
	}
}

func TestRawSQLTranslation(t *testing.T) {
	h := newHarness(t, baseConfig())
	ctx := context.Background()

	cases := []struct {
		source, sql string
		params      any
		wantSQL     string
		wantArgs    []any
	}{
		{"", "SELECT * FROM t WHERE a = ? AND b = '?' -- ?\n AND c = ?;", []any{"x", 2.0},
			"SELECT * FROM t WHERE a = $1 AND b = '?' -- ?\n AND c = $2", []any{"x", int64(2)}},
		{"default", "SELECT ?, ?, ?, ?", []any{1.5, true, nil, json.Number("7")},
			"SELECT $1, $2, $3, $4", []any{1.5, true, nil, int64(7)}},
		{"bq", "SELECT ? AS a, \"?\" AS b # ?\n", []any{"x"}, "SELECT ? AS a, \"?\" AS b", []any{"x"}},
		{"logs", "SELECT * FROM t WHERE a = ?", []string{"x"}, "SELECT * FROM t WHERE a = ?", []any{"x"}},
		{"", "SELECT 1", nil, "SELECT 1", nil},
		{"", "SELECT 1", []any{}, "SELECT 1", []any{}},
		{"", "SELECT ?, ?", json.RawMessage(`["a", 1.5]`), "SELECT $1, $2", []any{"a", 1.5}},
		{"", "SELECT 1", json.RawMessage(` `), "SELECT 1", nil},
	}
	for _, c := range cases {
		res, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: c.sql, Params: c.params, Source: c.source})
		if err != nil {
			t.Fatalf("Query(%q): %v", c.sql, err)
		}
		if res.RowCount != 1 || len(res.Rows) != 1 {
			t.Fatalf("result = %+v", res)
		}
		src := c.source
		if src == "" {
			src = "default"
		}
		call := h.fake(t, src).lastCall(t)
		if call.sql != c.wantSQL {
			t.Errorf("sql = %q, want %q", call.sql, c.wantSQL)
		}
		if len(call.args) != len(c.wantArgs) || (len(c.wantArgs) > 0 && !reflect.DeepEqual(call.args, c.wantArgs)) {
			t.Errorf("args = %#v, want %#v", call.args, c.wantArgs)
		}
		if call.maxRows != 100 || call.maxBytes != warehouse.DefaultMaxResultBytes {
			t.Errorf("limits = %d rows, %d bytes", call.maxRows, call.maxBytes)
		}
		if !call.hasDeadline || call.deadline > 5*time.Second || call.deadline < 4*time.Second {
			t.Errorf("deadline = %v (%v)", call.deadline, call.hasDeadline)
		}
	}
}

func TestRawSQLErrors(t *testing.T) {
	h := newHarness(t, baseConfig())
	ctx := context.Background()
	cases := []struct {
		name string
		req  warehouse.QueryRequest
		want error
	}{
		{"both", warehouse.QueryRequest{SQL: "SELECT 1", Query: "wau"}, warehouse.ErrBadRequest},
		{"neither", warehouse.QueryRequest{}, warehouse.ErrBadRequest},
		{"unknown source", warehouse.QueryRequest{SQL: "SELECT 1", Source: "nope"}, warehouse.ErrNotFound},
		{"too few params", warehouse.QueryRequest{SQL: "SELECT ?, ?", Params: []any{1}}, warehouse.ErrBadRequest},
		{"too many params", warehouse.QueryRequest{SQL: "SELECT '?'", Params: []any{1}}, warehouse.ErrBadRequest},
		{"params object", warehouse.QueryRequest{SQL: "SELECT ?", Params: map[string]any{"a": 1}}, warehouse.ErrBadRequest},
		{"params string", warehouse.QueryRequest{SQL: "SELECT ?", Params: "x"}, warehouse.ErrBadRequest},
		{"params bytes", warehouse.QueryRequest{SQL: "SELECT ?", Params: []byte("x")}, warehouse.ErrBadRequest},
		{"nested param", warehouse.QueryRequest{SQL: "SELECT ?", Params: []any{[]any{1}}}, warehouse.ErrBadRequest},
		{"object param", warehouse.QueryRequest{SQL: "SELECT ?", Params: []any{map[string]any{}}}, warehouse.ErrBadRequest},
		{"bad number", warehouse.QueryRequest{SQL: "SELECT ?", Params: []any{json.Number("1x")}}, warehouse.ErrBadRequest},
		{"uint overflow", warehouse.QueryRequest{SQL: "SELECT ?", Params: []any{uint64(1 << 63)}}, warehouse.ErrBadRequest},
		{"empty", warehouse.QueryRequest{SQL: " -- nothing\n;"}, warehouse.ErrBadRequest},
		{"unterminated", warehouse.QueryRequest{SQL: "SELECT 'x"}, warehouse.ErrBadRequest},
		{"guard", warehouse.QueryRequest{SQL: "DELETE FROM t"}, warehouse.ErrForbidden},
		{"multi", warehouse.QueryRequest{SQL: "SELECT 1; DROP TABLE t"}, warehouse.ErrForbidden},
		{"cr comment", warehouse.QueryRequest{SQL: "--\rDELETE FROM t WHERE true; --\nSELECT 1", Source: "bq"}, warehouse.ErrForbidden},
		{"nul byte", warehouse.QueryRequest{SQL: "SELECT 1 -- \x00"}, warehouse.ErrBadRequest},
		{"invalid utf8", warehouse.QueryRequest{SQL: "SELECT '\xff'"}, warehouse.ErrBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := h.svc.Query(ctx, c.req)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
	if n := h.opens.Load(); n != 0 {
		t.Errorf("rejected requests opened %d sources", n)
	}
}

func TestRawSQLPolicy(t *testing.T) {
	ctx := context.Background()

	cfg := baseConfig()
	cfg.AllowRawSQL = false
	h := newHarness(t, cfg)
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1"}); !errors.Is(err, warehouse.ErrForbidden) {
		t.Fatalf("raw SQL disabled: err = %v", err)
	}
	// Named queries still work.
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{Query: "tables"}); err != nil {
		t.Fatalf("named query with raw SQL disabled: %v", err)
	}

	cfg = baseConfig()
	cfg.AllowAnyStatement = true
	h = newHarness(t, cfg)
	for _, sql := range []string{"DELETE FROM t WHERE id = ?", "SELECT 1; SELECT ?", "EXPLAIN SELECT ?"} {
		if _, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: sql, Params: []any{1}}); err != nil {
			t.Fatalf("allow_any_statement %q: %v", sql, err)
		}
	}
	if got := h.fake(t, "default").lastCall(t).sql; got != "EXPLAIN SELECT $1" {
		t.Errorf("sql = %q", got)
	}
}

func TestNamedQueries(t *testing.T) {
	h := newHarness(t, baseConfig())
	ctx := context.Background()

	_, err := h.svc.Query(ctx, warehouse.QueryRequest{Query: "wau", Params: map[string]any{"team": "growth", "since": "2026-01-01"}})
	if err != nil {
		t.Fatal(err)
	}
	call := h.fake(t, "default").lastCall(t)
	wantSQL := "SELECT day, n FROM wau WHERE team = $1 AND day >= $2 AND note <> '@team' -- @team\n AND alt = $3"
	if call.sql != wantSQL {
		t.Errorf("sql = %q, want %q", call.sql, wantSQL)
	}
	if want := []any{"growth", "2026-01-01", "growth"}; !reflect.DeepEqual(call.args, want) {
		t.Errorf("args = %#v, want %#v", call.args, want)
	}

	// BigQuery: @@system variables are untouched, @day becomes "?".
	_, err = h.svc.Query(ctx, warehouse.QueryRequest{Query: "bq_daily", Params: map[string]any{"day": 3.0}, Source: "bq"})
	if err != nil {
		t.Fatal(err)
	}
	call = h.fake(t, "bq").lastCall(t)
	if call.sql != "SELECT @@project_id AS p, x FROM t WHERE d = ?" || !reflect.DeepEqual(call.args, []any{int64(3)}) {
		t.Errorf("bq call = %q %#v", call.sql, call.args)
	}

	// Named queries bypass the statement guard and accept no/empty params.
	for _, p := range []any{nil, map[string]any{}, []any{}, map[string]string{}} {
		if _, err := h.svc.Query(ctx, warehouse.QueryRequest{Query: "tables", Params: p}); err != nil {
			t.Fatalf("tables(%#v): %v", p, err)
		}
	}
	if got := h.fake(t, "default").lastCall(t).sql; got != "SHOW TABLES" {
		t.Errorf("sql = %q", got)
	}

	// Undecoded JSON is accepted too.
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{Query: "wau", Params: json.RawMessage(`{"team":"a","since":7}`)}); err != nil {
		t.Fatal(err)
	}
	if got := h.fake(t, "default").lastCall(t).args; !reflect.DeepEqual(got, []any{"a", int64(7), "a"}) {
		t.Errorf("raw JSON args = %#v", got)
	}
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{Query: "wau", Params: json.RawMessage(`{`)}); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Errorf("broken raw JSON: %v", err)
	}

	// Typed maps are accepted.
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{Query: "wau", Params: map[string]string{"team": "a", "since": "b"}}); err != nil {
		t.Fatal(err)
	}

	errCases := []struct {
		name string
		req  warehouse.QueryRequest
		want error
	}{
		{"unknown query", warehouse.QueryRequest{Query: "nope"}, warehouse.ErrNotFound},
		{"missing param", warehouse.QueryRequest{Query: "wau", Params: map[string]any{"team": "x"}}, warehouse.ErrBadRequest},
		{"no params", warehouse.QueryRequest{Query: "wau"}, warehouse.ErrBadRequest},
		{"unknown param", warehouse.QueryRequest{Query: "wau", Params: map[string]any{"team": "x", "since": "y", "extra": 1}}, warehouse.ErrBadRequest},
		{"array params", warehouse.QueryRequest{Query: "wau", Params: []any{"x", "y"}}, warehouse.ErrBadRequest},
		{"non-scalar", warehouse.QueryRequest{Query: "wau", Params: map[string]any{"team": []any{"x"}, "since": "y"}}, warehouse.ErrBadRequest},
		{"wrong source", warehouse.QueryRequest{Query: "wau", Params: map[string]any{"team": "x", "since": "y"}, Source: "bq"}, warehouse.ErrBadRequest},
	}
	for _, c := range errCases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := h.svc.Query(ctx, c.req); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
		})
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	src := map[string]config.WarehouseSource{"default": {Type: "postgres", DSN: "postgres://x"}}
	cases := []struct {
		name string
		cfg  config.WarehouseConfig
	}{
		{"unknown type", config.WarehouseConfig{Sources: map[string]config.WarehouseSource{"x": {Type: "mysql"}}}},
		{"bad default", config.WarehouseConfig{Sources: src, DefaultSource: "nope"}},
		{"query source", config.WarehouseConfig{Sources: src, Queries: map[string]config.NamedQuery{"q": {SQL: "SELECT 1", Source: "nope"}}}},
		{"param name", config.WarehouseConfig{Sources: src, Queries: map[string]config.NamedQuery{"q": {SQL: "SELECT @a", Params: []string{"a-b"}}}}},
		{"duplicate param", config.WarehouseConfig{Sources: src, Queries: map[string]config.NamedQuery{"q": {SQL: "SELECT @a", Params: []string{"a", "a"}}}}},
		{"unterminated", config.WarehouseConfig{Sources: src, Queries: map[string]config.NamedQuery{"q": {SQL: "SELECT '@a"}}}},
		{"empty sql", config.WarehouseConfig{Sources: src, Queries: map[string]config.NamedQuery{"q": {SQL: " -- x"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := New(ctx, c.cfg); err == nil {
				t.Fatal("New succeeded")
			}
		})
	}

	// A single source is the default even when not named "default".
	svc, err := NewWithOptions(ctx, config.WarehouseConfig{
		Sources: map[string]config.WarehouseSource{"only": {Type: "athena"}},
		Queries: map[string]config.NamedQuery{"q": {SQL: "SELECT 1"}},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, queries := svc.Describe(); queries[0].Source != "only" {
		t.Errorf("query source = %q", queries[0].Source)
	}
}

func TestSourceSelection(t *testing.T) {
	ctx := context.Background()
	cfg := baseConfig()
	cfg.DefaultSource = ""
	cfg.Queries = nil
	h := newHarness(t, cfg)
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1"}); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Fatalf("no default: err = %v", err)
	}
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1", Source: "logs"}); err != nil {
		t.Fatal(err)
	}
	h.fake(t, "logs")
}

// TestMaxResultBytes: the size cap is passed to the source, and a result
// over it is cut after the last row that fits even when the source ignores
// the cap.
func TestMaxResultBytes(t *testing.T) {
	wide := func(i, n int) map[string]any {
		return map[string]any{"i": int64(i), "pad": strings.Repeat("x", n)}
	}
	per := conv.Size(wide(0, 1000)) + 1
	f := &fakeSource{typ: "postgres", query: func(ctx context.Context, sql string, args []any, maxRows int) (*warehouse.Result, error) {
		n := 1000
		if sql == "SELECT huge" {
			n = 10 * per
		}
		rows := make([]map[string]any, 10)
		for i := range rows {
			rows[i] = wide(i, n)
		}
		return &warehouse.Result{Columns: []warehouse.Column{{Name: "i"}, {Name: "pad"}}, Rows: rows, RowCount: len(rows)}, nil
	}}
	svc, err := NewWithOptions(context.Background(), baseConfig(), Options{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxResultBytes: 3*per + per/2,
		Open: func(context.Context, string, config.WarehouseSource) (warehouse.Source, error) {
			return f, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	ctx := context.Background()

	res, err := svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT wide"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || res.RowCount != 3 || !res.Truncated || res.Rows[2]["i"] != int64(2) {
		t.Fatalf("rows = %d row_count = %d truncated = %v", len(res.Rows), res.RowCount, res.Truncated)
	}
	if b, _ := json.Marshal(res.Rows); len(b) > 3*per+per/2 {
		t.Errorf("JSON rows are %d bytes, cap %d", len(b), 3*per+per/2)
	}
	if call := f.lastCall(t); call.maxBytes != 3*per+per/2 || call.maxRows != 100 {
		t.Errorf("limits passed = %d rows, %d bytes", call.maxRows, call.maxBytes)
	}

	// A first row larger than the cap leaves no rows.
	res, err = svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT huge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 0 || res.RowCount != 0 || !res.Truncated || res.Rows == nil {
		t.Fatalf("huge: %+v", res)
	}
}

func TestMaxRows(t *testing.T) {
	cfg := baseConfig()
	cfg.MaxRows = 3
	h := newHarness(t, cfg, func(name string, f *fakeSource) {
		f.query = func(ctx context.Context, sql string, args []any, maxRows int) (*warehouse.Result, error) {
			switch sql {
			case "SELECT misbehaving":
				// A source ignoring maxRows is capped by the service.
				rows := make([]map[string]any, 5)
				for i := range rows {
					rows[i] = map[string]any{"i": int64(i)}
				}
				return &warehouse.Result{Rows: rows, RowCount: 5}, nil
			case "SELECT nil":
				return nil, nil
			default:
				return &warehouse.Result{Rows: []map[string]any{{"i": 1}, {"i": 2}, {"i": 3}}, RowCount: 99, Truncated: true}, nil
			}
		}
	})
	ctx := context.Background()

	res, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT misbehaving"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || res.RowCount != 3 || !res.Truncated || res.Columns == nil {
		t.Fatalf("result = %+v", res)
	}
	if got := h.fake(t, "default").lastCall(t).maxRows; got != 3 {
		t.Errorf("maxRows passed = %d", got)
	}

	res, err = h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT capped"})
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 3 || !res.Truncated {
		t.Fatalf("result = %+v", res)
	}

	res, err = h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT nil"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if string(b) != `{"columns":[],"rows":[],"row_count":0,"truncated":false}` {
		t.Fatalf("empty result JSON = %s", b)
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	cfg := baseConfig()
	cfg.Timeout = config.Duration(50 * time.Millisecond)
	h := newHarness(t, cfg, func(name string, f *fakeSource) {
		f.query = func(ctx context.Context, sql string, args []any, maxRows int) (*warehouse.Result, error) {
			<-ctx.Done()
			return nil, fmt.Errorf("engine: %w", ctx.Err())
		}
	})

	start := time.Now()
	_, err := h.svc.Query(context.Background(), warehouse.QueryRequest{SQL: "SELECT pg_sleep(10)"})
	if !errors.Is(err, warehouse.ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err = h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT pg_sleep(10)"})
	if !errors.Is(err, context.Canceled) || errors.Is(err, warehouse.ErrTimeout) || errors.Is(err, warehouse.ErrUpstream) {
		t.Fatalf("canceled: err = %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	errs := map[string]error{
		"SELECT plain":     errors.New("connection refused"),
		"SELECT bad":       fmt.Errorf("%w: syntax error", warehouse.ErrBadRequest),
		"SELECT forbidden": fmt.Errorf("%w: read-only", warehouse.ErrForbidden),
		"SELECT timeout":   fmt.Errorf("%w: statement timeout", warehouse.ErrTimeout),
		"SELECT deadline":  context.DeadlineExceeded,
	}
	h := newHarness(t, baseConfig(), func(name string, f *fakeSource) {
		f.query = func(ctx context.Context, sql string, args []any, maxRows int) (*warehouse.Result, error) {
			return nil, errs[sql]
		}
	})
	want := map[string]error{
		"SELECT plain":     warehouse.ErrUpstream,
		"SELECT bad":       warehouse.ErrBadRequest,
		"SELECT forbidden": warehouse.ErrForbidden,
		"SELECT timeout":   warehouse.ErrTimeout,
		"SELECT deadline":  warehouse.ErrTimeout,
	}
	for sql, w := range want {
		_, err := h.svc.Query(context.Background(), warehouse.QueryRequest{SQL: sql})
		if !errors.Is(err, w) {
			t.Errorf("%s: err = %v, want %v", sql, err, w)
		}
		if errors.Is(err, errs[sql]) == false {
			t.Errorf("%s: original error not wrapped: %v", sql, err)
		}
	}
}

func TestLazyOpen(t *testing.T) {
	h := newHarness(t, baseConfig())
	ctx := context.Background()
	if n := h.opens.Load(); n != 0 {
		t.Fatalf("opened %d sources at construction", n)
	}

	boom := errors.New("bad credentials")
	h.openErr.Store(&boom)
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1"}); !errors.Is(err, warehouse.ErrUpstream) || !errors.Is(err, boom) {
		t.Fatalf("open failure: err = %v", err)
	}
	h.openErr.Store(nil)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := h.opens.Load(); n != 2 { // one failure + one success
		t.Fatalf("opens = %d, want 2", n)
	}

	f := h.fake(t, "default")
	if err := h.svc.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.closed {
		t.Error("source not closed")
	}
	if _, err := h.svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("after close: err = %v", err)
	}
	if err := h.svc.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestDescribeAndDisabled(t *testing.T) {
	h := newHarness(t, baseConfig())
	if !h.svc.Enabled() {
		t.Fatal("not enabled")
	}
	sources, queries := h.svc.Describe()
	wantSources := []warehouse.SourceInfo{
		{Name: "bq", Type: "bigquery"},
		{Name: "default", Type: "postgres", Description: "main db"},
		{Name: "logs", Type: "athena"},
	}
	if !reflect.DeepEqual(sources, wantSources) {
		t.Errorf("sources = %+v", sources)
	}
	wantQueries := []warehouse.QueryInfo{
		{Name: "bq_daily", Params: []string{"day"}, Source: "bq"},
		{Name: "tables", Params: []string{}, Source: "default"},
		{Name: "wau", Description: "weekly active users", Params: []string{"team", "since"}, Source: "default"},
	}
	if !reflect.DeepEqual(queries, wantQueries) {
		t.Errorf("queries = %+v", queries)
	}
	b, _ := json.Marshal(queries[1])
	if string(b) != `{"name":"tables","params":[],"source":"default"}` {
		t.Errorf("query JSON = %s", b)
	}
	// Describe returns copies.
	queries[2].Params[0] = "mutated"
	if _, again := h.svc.Describe(); again[2].Params[0] != "team" {
		t.Error("Describe exposes internal state")
	}

	svc, err := New(context.Background(), config.WarehouseConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Enabled() {
		t.Error("empty config enabled")
	}
	if _, err := svc.Query(context.Background(), warehouse.QueryRequest{SQL: "SELECT 1"}); !errors.Is(err, warehouse.ErrDisabled) {
		t.Errorf("disabled: err = %v", err)
	}
	if s, q := svc.Describe(); len(s) != 0 || len(q) != 0 {
		t.Error("disabled Describe not empty")
	}
	if err := svc.Close(); err != nil {
		t.Error(err)
	}
}

func TestNormalizeParam(t *testing.T) {
	cases := []struct {
		in   any
		want any
		err  bool
	}{
		{nil, nil, false},
		{"s", "s", false},
		{true, true, false},
		{3.0, int64(3), false},
		{math.Copysign(0, -1), int64(0), false},
		{3.5, 3.5, false},
		{1e300, 1e300, false},
		{float64(1 << 53), int64(1 << 53), false},
		{float64(1<<53) * 2, float64(1<<53) * 2, false},
		{float32(2), int64(2), false},
		{json.Number("12"), int64(12), false},
		{json.Number("1.25"), 1.25, false},
		{json.Number("99999999999999999999"), 1e20, false},
		{int(7), int64(7), false},
		{int8(-7), int64(-7), false},
		{uint32(7), int64(7), false},
		{uint64(7), int64(7), false},
		{[]any{1}, nil, true},
		{map[string]any{}, nil, true},
		{struct{}{}, nil, true},
	}
	for _, c := range cases {
		got, err := normalizeParam(c.in)
		if (err != nil) != c.err {
			t.Errorf("normalizeParam(%#v) err = %v", c.in, err)
			continue
		}
		if !c.err && !reflect.DeepEqual(got, c.want) {
			t.Errorf("normalizeParam(%#v) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

func TestObserve(t *testing.T) {
	type obs struct{ source, result string }
	var (
		mu  sync.Mutex
		got []obs
	)
	cfg := baseConfig()
	svc, err := NewWithOptions(context.Background(), cfg, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Open: func(ctx context.Context, name string, src config.WarehouseSource) (warehouse.Source, error) {
			return &fakeSource{typ: src.Type, query: func(ctx context.Context, sql string, args []any, maxRows int) (*warehouse.Result, error) {
				if sql == "SELECT fail" {
					return nil, errors.New("boom")
				}
				return &warehouse.Result{}, nil
			}}, nil
		},
		Observe: func(source, result string, elapsed time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, obs{source, result})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	ctx := context.Background()
	_, _ = svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1", Source: "bq"})
	_, _ = svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT fail"})
	_, _ = svc.Query(ctx, warehouse.QueryRequest{SQL: "DELETE FROM t"})
	_, _ = svc.Query(ctx, warehouse.QueryRequest{Query: "bq_daily"})
	_, _ = svc.Query(ctx, warehouse.QueryRequest{SQL: "SELECT 1", Source: "nope"})
	want := []obs{
		{"bq", "ok"}, {"default", "upstream"}, {"default", "forbidden"}, {"bq", "bad_request"}, {"", "not_found"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("observations = %v, want %v", got, want)
	}
}
