package server

import (
	"context"
	"fmt"
	"net/http"
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
