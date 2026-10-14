package bigquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	bq "cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
)

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		panic(s)
	}
	return r
}

func TestConvertRow(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	schema := bq.Schema{
		{Name: "s", Type: bq.StringFieldType},
		{Name: "i", Type: bq.IntegerFieldType},
		{Name: "f", Type: bq.FloatFieldType},
		{Name: "nan", Type: bq.FloatFieldType},
		{Name: "inf", Type: bq.FloatFieldType},
		{Name: "b", Type: bq.BooleanFieldType},
		{Name: "by", Type: bq.BytesFieldType},
		{Name: "num", Type: bq.NumericFieldType},
		{Name: "bignum", Type: bq.BigNumericFieldType},
		{Name: "ts", Type: bq.TimestampFieldType},
		{Name: "d", Type: bq.DateFieldType},
		{Name: "t", Type: bq.TimeFieldType},
		{Name: "dt", Type: bq.DateTimeFieldType},
		{Name: "geo", Type: bq.GeographyFieldType},
		{Name: "j", Type: bq.JSONFieldType},
		{Name: "badj", Type: bq.JSONFieldType},
		{Name: "iv", Type: bq.IntervalFieldType},
		{Name: "rg", Type: bq.RangeFieldType, RangeElementType: &bq.RangeElementType{Type: bq.DateFieldType}},
		{Name: "arr", Type: bq.IntegerFieldType, Repeated: true},
		{Name: "empty", Type: bq.StringFieldType, Repeated: true},
		{Name: "rec", Type: bq.RecordFieldType, Schema: bq.Schema{
			{Name: "a", Type: bq.IntegerFieldType},
			{Name: "tags", Type: bq.StringFieldType, Repeated: true},
			{Name: "when", Type: bq.DateFieldType},
		}},
		{Name: "recs", Type: bq.RecordFieldType, Repeated: true, Schema: bq.Schema{{Name: "x", Type: bq.FloatFieldType}}},
		{Name: "null", Type: bq.StringFieldType},
		{Name: "s", Type: bq.StringFieldType}, // duplicate name
	}
	vals := []bq.Value{
		"hello", int64(9007199254740993), 1.5, math.NaN(), math.Inf(-1), true, []byte{0xde, 0xad, 0xbe, 0xef},
		rat("12.25"), rat("123456789012345678901234567890.123456789"),
		ts, civil.Date{Year: 2026, Month: 1, Day: 2},
		civil.Time{Hour: 3, Minute: 4, Second: 5, Nanosecond: 500000000},
		civil.DateTime{Date: civil.Date{Year: 2026, Month: 1, Day: 2}, Time: civil.Time{Hour: 3, Minute: 4, Second: 5}},
		"POINT(1 2)", `{"a":[1,"x"]}`, `not json`,
		&bq.IntervalValue{Years: 1, Months: 2, Days: 3, Hours: 4},
		&bq.RangeValue{Start: civil.Date{Year: 2026, Month: 1, Day: 1}},
		[]bq.Value{int64(1), int64(2)},
		nil,
		[]bq.Value{int64(7), []bq.Value{"x", "y"}, civil.Date{Year: 2026, Month: 3, Day: 4}},
		[]bq.Value{[]bq.Value{1.25}, []bq.Value{nil}},
		nil,
		"dup",
	}
	keys := columnKeys(schema)
	row, err := convertRow(vals, schema, keys)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"s": "hello", "i": int64(9007199254740993), "f": 1.5, "nan": "NaN", "inf": "-Infinity", "b": true,
		"by": "3q2+7w==", "num": 12.25, "bignum": "123456789012345678901234567890.123456789",
		"ts": "2026-01-02T03:04:05.123456Z", "d": "2026-01-02", "t": "03:04:05.500000000", "dt": "2026-01-02T03:04:05",
		"geo": "POINT(1 2)", "j": map[string]any{"a": []any{1.0, "x"}}, "badj": "not json",
		"iv":    "1-2 3 4:0:0",
		"rg":    map[string]any{"start": "2026-01-01", "end": nil},
		"arr":   []any{int64(1), int64(2)},
		"empty": []any{},
		"rec":   map[string]any{"a": int64(7), "tags": []any{"x", "y"}, "when": "2026-03-04"},
		"recs":  []any{map[string]any{"x": 1.25}, map[string]any{"x": nil}},
		"null":  nil,
		"s_2":   "dup",
	}
	if len(row) != len(want) {
		t.Errorf("row has %d keys, want %d: %v", len(row), len(want), row)
	}
	for k, w := range want {
		if got := row[k]; !reflect.DeepEqual(got, w) {
			t.Errorf("%s = %#v, want %#v", k, got, w)
		}
	}
	if _, err := json.Marshal(row); err != nil {
		t.Fatalf("row is not JSON-encodable: %v", err)
	}
	if _, err := convertRow(vals[:2], schema, keys); err == nil {
		t.Error("short row accepted")
	}
}

func TestTypeNames(t *testing.T) {
	schema := bq.Schema{
		{Name: "a", Type: bq.IntegerFieldType},
		{Name: "b", Type: bq.FloatFieldType},
		{Name: "c", Type: bq.BooleanFieldType},
		{Name: "d", Type: bq.StringFieldType, Repeated: true},
		{Name: "e", Type: bq.RecordFieldType, Schema: bq.Schema{{Name: "x", Type: bq.IntegerFieldType}, {Name: "y", Type: bq.DateFieldType, Repeated: true}}},
		{Name: "f", Type: bq.RangeFieldType, RangeElementType: &bq.RangeElementType{Type: bq.TimestampFieldType}},
		{Name: "g", Type: bq.NumericFieldType},
		{Name: "a", Type: bq.DateFieldType},
	}
	got := columns(schema)
	want := []warehouse.Column{
		{Name: "a", Type: "INT64"}, {Name: "b", Type: "FLOAT64"}, {Name: "c", Type: "BOOL"},
		{Name: "d", Type: "ARRAY<STRING>"}, {Name: "e", Type: "STRUCT<x INT64, y ARRAY<DATE>>"},
		{Name: "f", Type: "RANGE<TIMESTAMP>"}, {Name: "g", Type: "NUMERIC"}, {Name: "a_2", Type: "DATE"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("columns = %+v", got)
	}
}

func TestParamValue(t *testing.T) {
	cases := []struct {
		in   any
		want any
		err  bool
	}{
		{nil, bq.NullString{}, false},
		{"x", "x", false},
		{int64(3), int64(3), false},
		{1.5, 1.5, false},
		{true, true, false},
		{json.Number("4"), int64(4), false},
		{json.Number("-2.5e3"), -2500.0, false},
		{json.Number("NaN"), nil, true},
		{json.Number("Inf"), nil, true},
		{json.Number("1e999"), nil, true},
		{json.Number("0x1p3"), nil, true},
		{math.NaN(), nil, true},
		{math.Inf(-1), nil, true},
		{[]any{1}, nil, true},
	}
	for _, c := range cases {
		got, err := paramValue(c.in)
		if (err != nil) != c.err || (!c.err && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("paramValue(%#v) = %#v, %v", c.in, got, err)
		}
	}
}

func TestClassify(t *testing.T) {
	ctx := context.Background()
	gerr := func(code int, reason string) error {
		e := &googleapi.Error{Code: code, Message: "msg " + reason}
		if reason != "" {
			e.Errors = []googleapi.ErrorItem{{Reason: reason, Message: "item"}}
		}
		return fmt.Errorf("wrapped: %w", e)
	}
	cases := []struct {
		err  error
		want error
	}{
		{gerr(400, "invalidQuery"), warehouse.ErrBadRequest},
		{gerr(404, "notFound"), warehouse.ErrBadRequest},
		{gerr(403, "accessDenied"), warehouse.ErrForbidden},
		{gerr(400, "bytesBilledLimitExceeded"), warehouse.ErrForbidden},
		{gerr(403, "quotaExceeded"), warehouse.ErrUpstream},
		{gerr(500, "backendError"), warehouse.ErrUpstream},
		{gerr(400, "timeout"), warehouse.ErrTimeout},
		{gerr(400, ""), warehouse.ErrBadRequest},
		{gerr(403, ""), warehouse.ErrForbidden},
		{gerr(503, ""), warehouse.ErrUpstream},
		{&bq.Error{Reason: "invalidQuery", Message: "Syntax error"}, warehouse.ErrBadRequest},
		{errors.New("dial tcp: refused"), warehouse.ErrUpstream},
	}
	for _, c := range cases {
		got := classify(ctx, c.err)
		if !errors.Is(got, c.want) {
			t.Errorf("classify(%v) = %v, want %v", c.err, got, c.want)
		}
		var g *googleapi.Error
		if errors.As(c.err, &g) && !errors.As(got, &g) {
			t.Errorf("classify(%v) lost the googleapi error", c.err)
		}
	}
	if got := classify(ctx, gerr(400, "invalidQuery")).Error(); !strings.Contains(got, "msg invalidQuery (invalidQuery)") || strings.Count(got, "msg invalidQuery") != 1 {
		t.Errorf("message = %q", got)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if got := classify(cctx, errors.New("x")); !errors.Is(got, context.Canceled) {
		t.Errorf("canceled: %v", got)
	}
	dctx, cancel2 := context.WithTimeout(ctx, -time.Second)
	defer cancel2()
	if got := classify(dctx, errors.New("x")); !errors.Is(got, warehouse.ErrTimeout) {
		t.Errorf("deadline: %v", got)
	}
}

func TestCredentialsType(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if ct, err := credentialsType(write("sa.json", `{"type":"service_account"}`)); err != nil || ct != option.ServiceAccount {
		t.Errorf("service account: %v %v", ct, err)
	}
	if ct, err := credentialsType(write("wif.json", `{"type":"external_account"}`)); err != nil || ct != option.ExternalAccount {
		t.Errorf("external account: %v %v", ct, err)
	}
	if _, err := credentialsType(write("bad.json", `{"type":"weird"}`)); err == nil {
		t.Error("unknown type accepted")
	}
	if _, err := credentialsType(write("broken.json", `{`)); err == nil {
		t.Error("broken JSON accepted")
	}
	if _, err := credentialsType(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing file accepted")
	}
	if _, err := New(context.Background(), "x", config.WarehouseSource{}, Options{}); err == nil {
		t.Error("missing project accepted")
	}
}

// ---------------------------------------------------------------------------
// Fake BigQuery REST API

type fakeBQ struct {
	t *testing.T

	mu       sync.Mutex
	inserted []map[string]any // job configurations
	canceled []string
	pages    []string // pageToken of each getQueryResults page read
	pending  bool     // jobs never complete

	schema map[string]any
	rows   [][]any // cell values (already JSON-encoded form)
}

func (f *fakeBQ) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/bigquery/v2/projects/proj/")
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && path == "jobs":
		var job map[string]any
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		f.inserted = append(f.inserted, job)
		state := "DONE"
		if f.pending {
			state = "RUNNING"
		}
		job["status"] = map[string]any{"state": state}
		_ = json.NewEncoder(w).Encode(job)

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "jobs/"), "/cancel")
		f.canceled = append(f.canceled, id)
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "bigquery#jobCancelResponse"})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "queries/"):
		id := strings.TrimPrefix(path, "queries/")
		resp := map[string]any{
			"kind":         "bigquery#getQueryResultsResponse",
			"jobReference": map[string]any{"projectId": "proj", "jobId": id, "location": "EU"},
			"jobComplete":  !f.pending,
		}
		if f.pending {
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		resp["schema"] = f.schema
		resp["totalRows"] = strconv.Itoa(len(f.rows))
		max, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		if r.URL.Query().Has("maxResults") && max == 0 {
			_ = json.NewEncoder(w).Encode(resp) // completion probe
			return
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
		f.pages = append(f.pages, r.URL.Query().Get("pageToken"))
		if max <= 0 {
			max = len(f.rows)
		}
		end := min(start+max, len(f.rows))
		var rows []any
		for _, cells := range f.rows[start:end] {
			var fs []any
			for _, c := range cells {
				fs = append(fs, map[string]any{"v": c})
			}
			rows = append(rows, map[string]any{"f": fs})
		}
		resp["rows"] = rows
		if end < len(f.rows) {
			resp["pageToken"] = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(resp)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "jobs/"):
		id := strings.TrimPrefix(path, "jobs/")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jobReference":  map[string]any{"projectId": "proj", "jobId": id, "location": "EU"},
			"status":        map[string]any{"state": "DONE"},
			"configuration": map[string]any{"query": map[string]any{"query": "x"}},
		})

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.Error(w, "not found", 404)
	}
}

func newFake(t *testing.T, f *fakeBQ, src config.WarehouseSource) *Source {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	src.ProjectID = "proj"
	s, err := New(context.Background(), "bq", src, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		ClientOptions: []option.ClientOption{
			option.WithEndpoint(srv.URL + "/bigquery/v2/"),
			option.WithoutAuthentication(),
			option.WithHTTPClient(srv.Client()),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestQueryAgainstFakeAPI(t *testing.T) {
	f := &fakeBQ{
		schema: map[string]any{"fields": []any{
			map[string]any{"name": "day", "type": "DATE"},
			map[string]any{"name": "n", "type": "INTEGER"},
			map[string]any{"name": "ts", "type": "TIMESTAMP"},
			map[string]any{"name": "tags", "type": "STRING", "mode": "REPEATED"},
			map[string]any{"name": "amount", "type": "NUMERIC"},
		}},
	}
	for i := range 5 {
		f.rows = append(f.rows, []any{
			fmt.Sprintf("2026-01-0%d", i+1), strconv.Itoa(i), "1767323045123456",
			[]any{map[string]any{"v": "a"}}, "1.10",
		})
	}
	s := newFake(t, f, config.WarehouseSource{Location: "EU", MaxBytesBilled: 1 << 30})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := s.Query(ctx, "SELECT day, n FROM t WHERE team = ? AND n > ? AND x = ? AND y = ?", []any{"growth", int64(1), 2.5, nil}, warehouse.Limits{MaxRows: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 3 || len(res.Rows) != 3 || !res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	wantRow := map[string]any{"day": "2026-01-01", "n": int64(0), "ts": "2026-01-02T03:04:05.123456Z", "tags": []any{"a"}, "amount": 1.1}
	if !reflect.DeepEqual(res.Rows[0], wantRow) {
		t.Errorf("row 0 = %#v", res.Rows[0])
	}
	wantCols := []warehouse.Column{{Name: "day", Type: "DATE"}, {Name: "n", Type: "INT64"}, {Name: "ts", Type: "TIMESTAMP"}, {Name: "tags", Type: "ARRAY<STRING>"}, {Name: "amount", Type: "NUMERIC"}}
	if !reflect.DeepEqual(res.Columns, wantCols) {
		t.Errorf("columns = %+v", res.Columns)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.inserted) != 1 {
		t.Fatalf("inserted %d jobs", len(f.inserted))
	}
	job := f.inserted[0]
	ref := job["jobReference"].(map[string]any)
	if ref["location"] != "EU" || ref["projectId"] != "proj" {
		t.Errorf("jobReference = %v", ref)
	}
	cfg := job["configuration"].(map[string]any)
	if cfg["jobTimeoutMs"] == nil {
		t.Errorf("no jobTimeoutMs: %v", cfg)
	}
	q := cfg["query"].(map[string]any)
	if q["query"] != "SELECT day, n FROM t WHERE team = ? AND n > ? AND x = ? AND y = ?" || q["useLegacySql"] != false ||
		q["maximumBytesBilled"] != strconv.Itoa(1<<30) {
		t.Errorf("query config = %v", q)
	}
	params, _ := json.Marshal(q["queryParameters"])
	for _, want := range []string{`"type":"STRING"`, `"value":"growth"`, `"type":"INT64"`, `"value":"1"`, `"type":"FLOAT64"`, `"value":"2.5"`} {
		if !strings.Contains(string(params), want) {
			t.Errorf("queryParameters %s missing %s", params, want)
		}
	}
	if len(f.pages) == 0 || len(f.pages) > 2 {
		t.Errorf("pages read = %v", f.pages)
	}
}

func TestQueryCancelsJobOnTimeout(t *testing.T) {
	f := &fakeBQ{pending: true}
	s := newFake(t, f, config.WarehouseSource{})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := s.Query(ctx, "SELECT 1", nil, warehouse.Limits{MaxRows: 10})
	if !errors.Is(err, warehouse.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.canceled) != 1 {
		t.Errorf("canceled jobs = %v", f.canceled)
	}
}

func TestQueryBadParam(t *testing.T) {
	s := newFake(t, &fakeBQ{}, config.WarehouseSource{})
	if _, err := s.Query(context.Background(), "SELECT ?", []any{[]any{1}}, warehouse.Limits{MaxRows: 10}); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
	if s.Placeholder(3) != "?" {
		t.Error("placeholder")
	}
}

// TestQueryResultSizeCap: a result stops at the last row that fits in
// Limits.MaxBytes, well before the row cap.
func TestQueryResultSizeCap(t *testing.T) {
	f := &fakeBQ{schema: map[string]any{"fields": []any{
		map[string]any{"name": "i", "type": "INTEGER"},
		map[string]any{"name": "pad", "type": "STRING"},
	}}}
	pad := strings.Repeat("x", 300000)
	for i := range 10 {
		f.rows = append(f.rows, []any{strconv.Itoa(i), pad})
	}
	s := newFake(t, f, config.WarehouseSource{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const limit = 1 << 20
	res, err := s.Query(ctx, "SELECT i, pad FROM t", nil, warehouse.Limits{MaxRows: 100, MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || res.RowCount != 3 || !res.Truncated || res.Rows[2]["i"] != int64(2) {
		t.Fatalf("rows = %d truncated = %v", len(res.Rows), res.Truncated)
	}
	if b, _ := json.Marshal(res.Rows); len(b) > limit {
		t.Errorf("rows encode to %d bytes, cap %d", len(b), limit)
	}

	// Without a cap the default (32 MiB) applies; all 3 MB fit.
	res, err = s.Query(ctx, "SELECT i, pad FROM t", nil, warehouse.Limits{MaxRows: 100})
	if err != nil || len(res.Rows) != 10 || res.Truncated {
		t.Fatalf("default cap: %d rows, truncated %v, %v", len(res.Rows), res != nil && res.Truncated, err)
	}
}
