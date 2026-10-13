package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/ratelimit"
	"github.com/thasami/backyard/internal/warehouse"
)

// warehouseEnabled writes 501 when no warehouse is configured.
func (s *Server) warehouseEnabled(w http.ResponseWriter, r *http.Request) bool {
	if s.deps.Warehouse == nil || !s.deps.Warehouse.Enabled() {
		s.writeAPIError(w, r, errUnsupported("the data warehouse is not configured on this server"))
		return false
	}
	return true
}

// warehouseError maps warehouse errors (SPEC §5.5).
func warehouseError(err error) *apiError {
	msg := func(sentinel error) string {
		m := err.Error()
		if i := strings.Index(m, sentinel.Error()+": "); i >= 0 {
			return m[i+len(sentinel.Error())+2:]
		}
		return strings.TrimPrefix(m, "warehouse: ")
	}
	switch {
	case errors.Is(err, warehouse.ErrDisabled):
		return errUnsupported("the data warehouse is not configured on this server")
	case errors.Is(err, warehouse.ErrBadRequest):
		return errBadRequest("%s", truncate(msg(warehouse.ErrBadRequest), 1000))
	case errors.Is(err, warehouse.ErrForbidden):
		return errForbidden("%s", truncate(msg(warehouse.ErrForbidden), 1000))
	case errors.Is(err, warehouse.ErrNotFound):
		return errNotFound("%s", truncate(msg(warehouse.ErrNotFound), 1000))
	case errors.Is(err, warehouse.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		e := newError(http.StatusGatewayTimeout, codeUpstream, "warehouse query timed out")
		e.Err = err
		return e
	case errors.Is(err, context.Canceled):
		return errContext(err, "warehouse query")
	case errors.Is(err, warehouse.ErrUpstream):
		e := newError(http.StatusBadGateway, codeUpstream, "%s", truncate(msg(warehouse.ErrUpstream), 1000))
		e.Err = err
		return e
	}
	return errInternal(err, "warehouse query failed")
}

// sourceLabel bounds the cardinality of the source metric label.
func (s *Server) sourceLabel(req warehouse.QueryRequest) string {
	src := req.Source
	if req.Query != "" {
		if q, ok := s.cfg.Warehouse.Queries[req.Query]; ok && q.Source != "" {
			src = q.Source
		} else if src == "" {
			src = s.cfg.Warehouse.DefaultSource
		}
	}
	if src == "" {
		src = s.cfg.Warehouse.DefaultSource
	}
	if _, ok := s.cfg.Warehouse.Sources[src]; ok {
		return src
	}
	return "unknown"
}

// handleWarehouseQuery serves POST /warehouse/query.
func (s *Server) handleWarehouseQuery(w http.ResponseWriter, r *http.Request) {
	if !s.warehouseEnabled(w, r) || !s.allow(w, r, ratelimit.Warehouse) {
		return
	}
	extendDeadlines(w, 0, max(s.cfg.Warehouse.Timeout.D(), time.Minute)+time.Minute)
	var req warehouse.QueryRequest
	if e := decodeJSON(w, r, smallBodyLimit, &req, false); e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	if strings.TrimSpace(req.SQL) == "" && strings.TrimSpace(req.Query) == "" {
		s.writeAPIError(w, r, errBadRequest(`"sql" or "query" is required`))
		return
	}
	if req.SQL != "" && req.Query != "" {
		s.writeAPIError(w, r, errBadRequest(`use either "sql" or "query", not both`))
		return
	}
	res, err := s.deps.Warehouse.Query(r.Context(), req)
	result := "ok"
	if err != nil {
		result = warehouseError(err).Code
	}
	s.metrics.warehouseQueries.WithLabelValues(s.sourceLabel(req), result).Inc()
	if err != nil {
		s.writeAPIError(w, r, warehouseError(err))
		return
	}
	if res == nil {
		res = &warehouse.Result{}
	}
	if res.Columns == nil {
		res.Columns = []warehouse.Column{}
	}
	if res.Rows == nil {
		res.Rows = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, res)
}

// handleWarehouseDescribe serves GET /warehouse.
func (s *Server) handleWarehouseDescribe(w http.ResponseWriter, r *http.Request) {
	if !s.warehouseEnabled(w, r) {
		return
	}
	sources, queries := s.deps.Warehouse.Describe()
	if sources == nil {
		sources = []warehouse.SourceInfo{}
	}
	if queries == nil {
		queries = []warehouse.QueryInfo{}
	}
	for i := range queries {
		if queries[i].Params == nil {
			queries[i].Params = []string{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources, "queries": queries})
}
