package athena

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/athena/types"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
)

func TestConvertValue(t *testing.T) {
	cases := []struct {
		typ, in string
		want    any
	}{
		{"boolean", "true", true},
		{"boolean", "false", false},
		{"tinyint", "-5", int64(-5)},
		{"smallint", "300", int64(300)},
		{"integer", "70000", int64(70000)},
		{"bigint", "9007199254740993", int64(9007199254740993)},
		{"double", "1.5", 1.5},
		{"double", "NaN", "NaN"},
		{"double", "Infinity", "Infinity"},
		{"double", "-Infinity", "-Infinity"},
		{"real", "0.1", 0.1},
		{"float", "2.0E10", 2e10},
		{"decimal", "12.2500", 12.25},
		{"decimal(38,9)", "12345678901234567890.123456789", "12345678901234567890.123456789"},
		{"varchar", "hello", "hello"},
		{"char", "ab ", "ab "},
		{"date", "2026-01-02", "2026-01-02"},
		{"timestamp", "2026-01-02 03:04:05.123", "2026-01-02T03:04:05.123Z"},
		{"timestamp", "2026-01-02 03:04:05", "2026-01-02T03:04:05Z"},
		{"timestamp(6)", "2026-01-02 03:04:05.123456", "2026-01-02T03:04:05.123456Z"},
		{"timestamp with time zone", "2026-01-02 03:04:05.000 UTC", "2026-01-02T03:04:05Z"},
		{"timestamp with time zone", "2026-07-02 03:04:05.000 America/New_York", "2026-07-02T07:04:05Z"},
		{"timestamp with time zone", "2026-01-02 03:04:05.000 +05:30", "2026-01-01T21:34:05Z"},
		{"timestamp with time zone", "garbage", "garbage"},
		{"time", "03:04:05.000", "03:04:05.000"},
		{"varbinary", "68 65 6c 6c 6f", "aGVsbG8="},
		{"varbinary", "zz", "zz"},
		{"json", `{"a":[1,"x"]}`, map[string]any{"a": []any{1.0, "x"}}},
		{"json", `"str"`, "str"},
		{"json", `{bad`, `{bad`},
		{"array(integer)", "[1, 2, 3]", "[1, 2, 3]"},
		{"map(varchar,integer)", "{a=1}", "{a=1}"},
		{"row(a integer)", "{a=1}", "{a=1}"},
		{"bigint", "notanumber", "notanumber"},
		{"interval day to second", "1 02:03:04.000", "1 02:03:04.000"},
		{"timestamp(3) with time zone", "2026-01-02 03:04:05.000 UTC", "2026-01-02T03:04:05Z"},
		{" DECIMAL(10, 2) ", "1.50", 1.5},
		{"Timestamp(3)", "2026-01-02 03:04:05.5", "2026-01-02T03:04:05.5Z"},
		{"timestamp with time zone", "2026-01-02 03:04:05.000 Not/AZone", "2026-01-02 03:04:05.000 Not/AZone"},
		{"timestamp with time zone", "2026-01-02 03:04:05.000 ../../etc/passwd", "2026-01-02 03:04:05.000 ../../etc/passwd"},
	}
	for _, c := range cases {
		got := ConvertValue(c.typ, c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("ConvertValue(%q, %q) = %#v, want %#v", c.typ, c.in, got, c.want)
		}
	}
}

func TestLiteral(t *testing.T) {
	cases := []struct {
		in   any
		want string
		err  bool
	}{
		{nil, "NULL", false},
		{"growth", "'growth'", false},
		{"it's", "'it''s'", false},
		{`a\'; DROP TABLE x; --`, `'a\''; DROP TABLE x; --'`, false},
		{true, "true", false},
		{int64(-42), "-42", false},
		{1.5, "1.5", false},
		{1e21, "1e+21", false},
		{3.0, "3.0", false},
		{json.Number("12.5"), "12.5", false},
		{json.Number("x"), "", true},
		{json.Number("-1.5e3"), "-1.5e3", false},
		{json.Number("NaN"), "", true},
		{json.Number("Infinity"), "", true},
		{json.Number("0x10"), "", true},
		{json.Number("1_000"), "", true},
		{json.Number("1 OR 1=1"), "", true},
		{strings.Repeat("é", 1022), "'" + strings.Repeat("é", 1022) + "'", false},
		{strings.Repeat("x", 1023), "", true},
		{math.NaN(), "", true},
		{math.Inf(1), "", true},
		{[]any{}, "", true},
	}
	for _, c := range cases {
		got, err := Literal(c.in)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("Literal(%#v) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestIsHeader(t *testing.T) {
	info := []types.ColumnInfo{{Name: aws.String("a"), Label: aws.String("a")}, {Name: aws.String("b"), Label: aws.String("B")}}
	row := func(vals ...string) types.Row {
		var r types.Row
		for _, v := range vals {
			r.Data = append(r.Data, types.Datum{VarCharValue: aws.String(v)})
		}
		return r
	}
	if !isHeader(row("x", "y"), info, types.StatementTypeDml) {
		t.Error("DML first row is always the header")
	}
	if !isHeader(row("a", "B"), info, types.StatementTypeUtility) {
		t.Error("matching names not detected")
	}
	if isHeader(row("a", "c"), info, types.StatementTypeDdl) {
		t.Error("non-matching row treated as header")
	}
	if isHeader(row("a"), info, types.StatementTypeDdl) {
		t.Error("short row treated as header")
	}
}

func TestFailureAndClassify(t *testing.T) {
	cat := func(c int32) *types.AthenaError { return &types.AthenaError{ErrorCategory: aws.Int32(c)} }
	cases := []struct {
		st   types.QueryExecutionStatus
		want error
	}{
		{types.QueryExecutionStatus{StateChangeReason: aws.String("SYNTAX_ERROR: line 1:8"), AthenaError: cat(2)}, warehouse.ErrBadRequest},
		{types.QueryExecutionStatus{StateChangeReason: aws.String("Internal error"), AthenaError: cat(1)}, warehouse.ErrUpstream},
		{types.QueryExecutionStatus{StateChangeReason: aws.String("Access Denied (Service: Amazon S3)"), AthenaError: cat(2)}, warehouse.ErrForbidden},
		{types.QueryExecutionStatus{AthenaError: &types.AthenaError{ErrorMessage: aws.String("boom")}}, warehouse.ErrUpstream},
	}
	for _, c := range cases {
		err := failure("q1", &c.st)
		if !errors.Is(err, c.want) {
			t.Errorf("failure(%+v) = %v, want %v", c.st, err, c.want)
		}
	}
	if err := failure("q1", &types.QueryExecutionStatus{}); !strings.Contains(err.Error(), "unknown error") {
		t.Errorf("empty failure: %v", err)
	}

	ctx := context.Background()
	if err := classify(ctx, &types.InvalidRequestException{Message: aws.String("bad")}); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Errorf("invalid request: %v", err)
	}
	if err := classify(ctx, errors.New("network")); !errors.Is(err, warehouse.ErrUpstream) {
		t.Errorf("network: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Fake Athena (AWS JSON 1.1) API

type fakeAthena struct {
	t *testing.T

	mu        sync.Mutex
	starts    []map[string]any
	polls     int
	stops     []string
	pageReqs  []map[string]any
	runPolls  int    // polls answered with RUNNING before the final state
	final     string // SUCCEEDED (default), FAILED, CANCELLED, or "" to never finish
	failure   map[string]any
	startErr  string // __type of an error returned by StartQueryExecution
	pollErr   string // __type of an error returned by GetQueryExecution
	stmtType  string
	columns   []map[string]any
	rows      [][]any // nil entries are NULLs
	addHeader bool
}

func (f *fakeAthena) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonAthena.")
	var in map[string]any
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		// A client that gives up (deadline) mid-request leaves a truncated
		// body; anything else is a malformed request.
		if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			f.t.Errorf("%s: decode: %v", op, err)
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch op {
	case "StartQueryExecution":
		f.starts = append(f.starts, in)
		if f.startErr != "" {
			w.Header().Set("X-Amzn-ErrorType", f.startErr)
			w.WriteHeader(http.StatusBadRequest)
			reply(map[string]any{"__type": f.startErr, "Message": "line 1:1: mismatched input"})
			return
		}
		reply(map[string]any{"QueryExecutionId": "qid-1"})
	case "GetQueryExecution":
		f.polls++
		if f.pollErr != "" {
			w.Header().Set("X-Amzn-ErrorType", f.pollErr)
			w.WriteHeader(http.StatusBadRequest)
			reply(map[string]any{"__type": f.pollErr, "Message": "denied"})
			return
		}
		state := f.final
		if f.polls <= f.runPolls || f.final == "" {
			state = "RUNNING"
			if f.polls == 1 {
				state = "QUEUED"
			}
		}
		status := map[string]any{"State": state}
		if state == "FAILED" {
			for k, v := range f.failure {
				status[k] = v
			}
		}
		reply(map[string]any{"QueryExecution": map[string]any{
			"QueryExecutionId": in["QueryExecutionId"],
			"StatementType":    f.stmtType,
			"Status":           status,
		}})
	case "GetQueryResults":
		f.pageReqs = append(f.pageReqs, in)
		all := f.dataRows()
		start := 0
		if tok, ok := in["NextToken"].(string); ok {
			start, _ = strconv.Atoi(tok)
		}
		max := 1000
		if m, ok := in["MaxResults"].(float64); ok {
			max = int(m)
		}
		end := min(start+max, len(all))
		resp := map[string]any{"ResultSet": map[string]any{
			"Rows":              all[start:end],
			"ResultSetMetadata": map[string]any{"ColumnInfo": f.columns},
		}}
		if end < len(all) {
			resp["NextToken"] = strconv.Itoa(end)
		}
		reply(resp)
	case "StopQueryExecution":
		f.stops = append(f.stops, fmt.Sprint(in["QueryExecutionId"]))
		reply(map[string]any{})
	default:
		f.t.Errorf("unexpected operation %q", op)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeAthena) dataRows() []any {
	var out []any
	if f.addHeader {
		var data []any
		for _, c := range f.columns {
			data = append(data, map[string]any{"VarCharValue": c["Name"]})
		}
		out = append(out, map[string]any{"Data": data})
	}
	for _, r := range f.rows {
		var data []any
		for _, v := range r {
			if v == nil {
				data = append(data, map[string]any{})
			} else {
				data = append(data, map[string]any{"VarCharValue": v})
			}
		}
		out = append(out, map[string]any{"Data": data})
	}
	return out
}

func newFake(t *testing.T, f *fakeAthena) *Source {
	t.Helper()
	f.t = t
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("AKIDTEST", "SECRET", ""),
		HTTPClient:  srv.Client(),
	}
	s, err := New(context.Background(), "logs", config.WarehouseSource{
		Type: "athena", Region: "eu-west-1", Workgroup: "analysts", Database: "weblogs",
		Catalog: "AwsDataCatalog", OutputLocation: "s3://results/backyard/",
	}, Options{
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		AWSConfig:       &cfg,
		Endpoint:        srv.URL,
		PollInterval:    time.Millisecond,
		MaxPollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQueryAgainstFakeAPI(t *testing.T) {
	f := &fakeAthena{
		runPolls: 3, final: "SUCCEEDED", stmtType: "DML", addHeader: true,
		columns: []map[string]any{
			{"Name": "day", "Type": "date"},
			{"Name": "n", "Type": "bigint"},
			{"Name": "avg", "Type": "double"},
			{"Name": "note", "Type": "varchar"},
		},
	}
	for i := range 2500 {
		f.rows = append(f.rows, []any{"2026-01-02", strconv.Itoa(i), "1.5", nil})
	}
	s := newFake(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	sql := "SELECT day, n FROM t WHERE team = ? AND n > ? AND ok = ? AND x = ?"
	res, err := s.Query(ctx, sql, []any{"o'brien", int64(5), true, nil}, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1500 || len(res.Rows) != 1500 || !res.Truncated {
		t.Fatalf("rows = %d truncated = %v", len(res.Rows), res.Truncated)
	}
	if want := map[string]any{"day": "2026-01-02", "n": int64(0), "avg": 1.5, "note": nil}; !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("row 0 = %#v", res.Rows[0])
	}
	if res.Rows[1499]["n"] != int64(1499) {
		t.Errorf("row 1499 = %#v", res.Rows[1499])
	}
	wantCols := []warehouse.Column{{Name: "day", Type: "date"}, {Name: "n", Type: "bigint"}, {Name: "avg", Type: "double"}, {Name: "note", Type: "varchar"}}
	if !reflect.DeepEqual(res.Columns, wantCols) {
		t.Errorf("columns = %+v", res.Columns)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) != 1 {
		t.Fatalf("starts = %d", len(f.starts))
	}
	start := f.starts[0]
	if start["QueryString"] != sql || start["WorkGroup"] != "analysts" {
		t.Errorf("start = %v", start)
	}
	if got := fmt.Sprint(start["ExecutionParameters"]); got != "['o''brien' 5 true NULL]" {
		t.Errorf("ExecutionParameters = %s", got)
	}
	qec, _ := start["QueryExecutionContext"].(map[string]any)
	if qec["Database"] != "weblogs" || qec["Catalog"] != "AwsDataCatalog" {
		t.Errorf("QueryExecutionContext = %v", qec)
	}
	rc, _ := start["ResultConfiguration"].(map[string]any)
	if rc["OutputLocation"] != "s3://results/backyard/" {
		t.Errorf("ResultConfiguration = %v", rc)
	}
	if start["ClientRequestToken"] == nil {
		t.Error("no idempotency token")
	}
	if f.polls != 4 {
		t.Errorf("polls = %d, want 4", f.polls)
	}
	// Pages: 1000 (header + 999 rows) + 502 (501 rows + 1 to detect truncation).
	if len(f.pageReqs) != 2 || f.pageReqs[0]["MaxResults"] != 1000.0 || f.pageReqs[1]["MaxResults"] != 502.0 || f.pageReqs[1]["NextToken"] != "1000" {
		t.Errorf("page requests = %v", f.pageReqs)
	}
	if len(f.stops) != 0 {
		t.Errorf("stopped a finished query: %v", f.stops)
	}
}

func TestQuerySmallResult(t *testing.T) {
	f := &fakeAthena{
		final: "SUCCEEDED", stmtType: "UTILITY",
		columns: []map[string]any{{"Name": "tab_name", "Type": "string"}},
		rows:    [][]any{{"a"}, {"b"}},
	}
	s := newFake(t, f)
	res, err := s.Query(context.Background(), "SHOW TABLES", nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Truncated || !reflect.DeepEqual(res.Rows, []map[string]any{{"tab_name": "a"}, {"tab_name": "b"}}) {
		t.Fatalf("result = %+v", res)
	}
	if _, ok := f.starts[0]["ExecutionParameters"]; ok {
		t.Error("ExecutionParameters sent without params")
	}

	// Exactly maxRows rows is not truncated.
	res, err = s.Query(context.Background(), "SHOW TABLES", nil, 2)
	if err != nil || res.Truncated || len(res.Rows) != 2 {
		t.Fatalf("exact: %+v %v", res, err)
	}
}

func TestQueryStopsOnTimeout(t *testing.T) {
	f := &fakeAthena{final: ""}
	s := newFake(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := s.Query(ctx, "SELECT 1", nil, 10)
	if !errors.Is(err, warehouse.ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stops) != 1 || f.stops[0] != "qid-1" {
		t.Errorf("stops = %v", f.stops)
	}
}

func TestQueryStopsWhenPollingFails(t *testing.T) {
	f := &fakeAthena{pollErr: "AccessDeniedException"}
	s := newFake(t, f)
	_, err := s.Query(context.Background(), "SELECT 1", nil, 10)
	if !errors.Is(err, warehouse.ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stops) != 1 || f.stops[0] != "qid-1" {
		t.Errorf("stops = %v, want the abandoned query stopped", f.stops)
	}
}

func TestQueryFailures(t *testing.T) {
	f := &fakeAthena{final: "FAILED", failure: map[string]any{
		"StateChangeReason": "SYNTAX_ERROR: line 1:8: Column 'x' cannot be resolved",
		"AthenaError":       map[string]any{"ErrorCategory": 2, "ErrorType": 1006},
	}}
	s := newFake(t, f)
	_, err := s.Query(context.Background(), "SELECT x", nil, 10)
	if !errors.Is(err, warehouse.ErrBadRequest) || !strings.Contains(err.Error(), "cannot be resolved") {
		t.Fatalf("failed query: %v", err)
	}

	f = &fakeAthena{final: "CANCELLED"}
	s = newFake(t, f)
	if _, err := s.Query(context.Background(), "SELECT 1", nil, 10); !errors.Is(err, warehouse.ErrUpstream) {
		t.Fatalf("cancelled query: %v", err)
	}

	f = &fakeAthena{startErr: "InvalidRequestException"}
	s = newFake(t, f)
	if _, err := s.Query(context.Background(), "SELECT 1; SELECT 2", nil, 10); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Fatalf("invalid request: %v", err)
	}

	if _, err := s.Query(context.Background(), "SELECT ?", []any{[]any{1}}, 10); !errors.Is(err, warehouse.ErrBadRequest) {
		t.Fatalf("bad param: %v", err)
	}
	if s.Placeholder(0) != "?" || s.Close() != nil {
		t.Error("placeholder/close")
	}
}

func TestNewRequiresRegion(t *testing.T) {
	t.Setenv("AWS_REGION", "")
	t.Setenv("AWS_DEFAULT_REGION", "")
	t.Setenv("AWS_CONFIG_FILE", "/nonexistent")
	cfg := aws.Config{}
	if _, err := New(context.Background(), "x", config.WarehouseSource{}, Options{AWSConfig: &cfg}); err == nil {
		t.Error("missing region accepted")
	}
}
