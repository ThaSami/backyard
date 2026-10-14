package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
)

var warehouseResultFixture = warehouse.Result{
	Columns:  []warehouse.Column{{Name: "day", Type: "DATE"}, {Name: "n", Type: "INT64"}},
	Rows:     []map[string]any{{"day": "2026-01-01", "n": float64(3)}},
	RowCount: 1,
}

const whURL = "https://foo.backyard.test/__backyard/api/warehouse"

func TestWarehouseQuery(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.Warehouse.Sources = map[string]config.WarehouseSource{"default": {Type: "postgres"}}
		c.Warehouse.DefaultSource = "default"
	}))
	w := h.api("POST", whURL+"/query", map[string]any{"sql": "SELECT 1 WHERE team = ?", "params": []any{"growth"}, "source": "default"})
	expectStatus(t, w, 200)
	res := decode[map[string]any](t, w)
	if res["row_count"] != float64(1) || res["truncated"] != false || len(res["rows"].([]any)) != 1 || len(res["columns"].([]any)) != 2 {
		t.Fatalf("result = %v", res)
	}
	h.wh.mu.Lock()
	got := h.wh.reqs[0]
	h.wh.mu.Unlock()
	if got.SQL != "SELECT 1 WHERE team = ?" || got.Source != "default" || fmt.Sprint(got.Params) != "[growth]" {
		t.Fatalf("request = %+v", got)
	}
	w = h.api("POST", whURL+"/query", map[string]any{"query": "wau", "params": map[string]any{"team": "growth"}})
	expectStatus(t, w, 200)

	// Empty results are arrays, not null.
	h.wh.res = &warehouse.Result{}
	w = h.api("POST", whURL+"/query", map[string]any{"sql": "SELECT 1"})
	if body := w.Body.String(); !strings.Contains(body, `"rows":[]`) || !strings.Contains(body, `"columns":[]`) {
		t.Fatalf("body = %s", body)
	}

	tests := []struct {
		name   string
		body   any
		err    error
		status int
		code   string
	}{
		{"neither", map[string]any{}, nil, 400, "bad_request"},
		{"both", map[string]any{"sql": "x", "query": "y"}, nil, 400, "bad_request"},
		{"bad request", map[string]any{"sql": "x"}, fmt.Errorf("%w: syntax error near x", warehouse.ErrBadRequest), 400, "bad_request"},
		{"forbidden", map[string]any{"sql": "DROP TABLE x"}, fmt.Errorf("%w: only SELECT", warehouse.ErrForbidden), 403, "forbidden"},
		{"not found", map[string]any{"query": "nope"}, fmt.Errorf("%w: named query \"nope\"", warehouse.ErrNotFound), 404, "not_found"},
		{"timeout", map[string]any{"sql": "x"}, fmt.Errorf("%w: 60s", warehouse.ErrTimeout), 504, "upstream_error"},
		{"upstream", map[string]any{"sql": "x"}, fmt.Errorf("%w: connection reset", warehouse.ErrUpstream), 502, "upstream_error"},
		{"disabled", map[string]any{"sql": "x"}, warehouse.ErrDisabled, 501, "unsupported"},
		{"deadline", map[string]any{"sql": "x"}, context.DeadlineExceeded, 504, "upstream_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.wh.err = tc.err
			w := h.api("POST", whURL+"/query", tc.body)
			e := expectError(t, w, tc.status, tc.code)
			if tc.name == "forbidden" && e.Error.Message != "only SELECT" {
				t.Fatalf("message = %q", e.Error.Message)
			}
		})
	}
	h.wh.err = nil

	w = h.api("GET", whURL, nil)
	expectStatus(t, w, 200)
	desc := w.Body.String()
	if !strings.Contains(desc, `"sources":[{"name":"default","type":"postgres"}]`) || !strings.Contains(desc, `"params":[]`) {
		t.Fatalf("describe = %s", desc)
	}
}

func TestWarehouseDisabledAndRateLimited(t *testing.T) {
	h := newHarness(t, func(h *harness, d *Deps) { d.Warehouse = nil })
	expectError(t, h.api("POST", whURL+"/query", map[string]any{"sql": "x"}), http.StatusNotImplemented, "unsupported")
	expectError(t, h.api("GET", whURL, nil), http.StatusNotImplemented, "unsupported")

	g := newHarness(t, withConfig(func(c *config.Config) { c.RateLimit.WarehousePerMinute = 1 }))
	expectStatus(t, g.api("POST", whURL+"/query", map[string]any{"sql": "x"}), 200)
	expectError(t, g.api("POST", whURL+"/query", map[string]any{"sql": "x"}), http.StatusTooManyRequests, "rate_limited")
}

func TestWarehouseServerErrorsDoNotLeak(t *testing.T) {
	h := newHarness(t)
	const rid = "req-warehouse-0001"
	pgErr := errors.New(`failed to connect to ` + "`" + `host=db.internal.acme user=analytics database=prod_dw` + "`" + `: dial error (dial tcp 10.1.2.3:5432: connect: connection refused)`)
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"upstream", fmt.Errorf("%w: source %q: %w", warehouse.ErrUpstream, "default", pgErr), 502, "upstream_error"},
		{"unclassified", fmt.Errorf("source %q: %w", "default", pgErr), 500, "internal"},
		{"timeout", fmt.Errorf("%w: %w", warehouse.ErrTimeout, pgErr), 504, "upstream_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h.wh.err = tc.err
			w := h.api("POST", whURL+"/query", map[string]any{"sql": "SELECT 1"}, "X-Request-Id", rid)
			e := expectError(t, w, tc.status, tc.code)
			for _, secret := range []string{"db.internal.acme", "analytics", "prod_dw", "10.1.2.3", "5432", "connection refused"} {
				if strings.Contains(e.Error.Message, secret) {
					t.Fatalf("message %q leaks %q", e.Error.Message, secret)
				}
			}
			if tc.status != 504 && !strings.Contains(e.Error.Message, rid) {
				t.Fatalf("message %q does not name the request id", e.Error.Message)
			}
			if w.Header().Get("X-Request-Id") != rid {
				t.Fatalf("X-Request-Id = %q", w.Header().Get("X-Request-Id"))
			}
		})
	}
	// The detail is logged with the request id.
	logs := h.logText()
	if !strings.Contains(logs, "db.internal.acme") || !strings.Contains(logs, "request_id="+rid) {
		t.Fatalf("detail not logged: %s", logs)
	}
	// Client errors keep their (useful, caller-caused) message.
	h.wh.err = fmt.Errorf("%w: column \"nope\" does not exist", warehouse.ErrBadRequest)
	e := expectError(t, h.api("POST", whURL+"/query", map[string]any{"sql": "SELECT nope"}), 400, "bad_request")
	if e.Error.Message != `column "nope" does not exist` {
		t.Fatalf("message = %q", e.Error.Message)
	}
}

func TestWarehouseLargeIntegerParamsStayExact(t *testing.T) {
	h := newHarness(t)
	w := h.api("POST", whURL+"/query", `{"sql":"SELECT * FROM t WHERE id = ? AND x = ? AND y = ?","params":[9007199254740993,1.5,-12]}`)
	expectStatus(t, w, 200)
	w = h.api("POST", whURL+"/query", `{"query":"wau","params":{"id":18446744073709551615,"n":3}}`)
	expectStatus(t, w, 200)
	h.wh.mu.Lock()
	reqs := slices.Clone(h.wh.reqs)
	h.wh.mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	pos, ok := reqs[0].Params.([]any)
	if !ok || len(pos) != 3 {
		t.Fatalf("positional params = %#v", reqs[0].Params)
	}
	for i, want := range []string{"9007199254740993", "1.5", "-12"} {
		n, ok := pos[i].(json.Number)
		if !ok || n.String() != want {
			t.Fatalf("param %d = %#v, want json.Number %s", i, pos[i], want)
		}
	}
	named, ok := reqs[1].Params.(map[string]any)
	if !ok || named["id"] != json.Number("18446744073709551615") || named["n"] != json.Number("3") {
		t.Fatalf("named params = %#v", reqs[1].Params)
	}
}
