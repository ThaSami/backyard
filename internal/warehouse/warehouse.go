// Package warehouse provides read-only analytics queries (backyard.warehouse)
// against BigQuery, Postgres-compatible databases (incl. Redshift) and Athena.
// Sources live in sub-packages; service.Service enforces policy (raw SQL
// gating, statement guard, named queries, row and size caps, timeouts).
package warehouse

import (
	"context"
	"errors"
)

var (
	// ErrDisabled: no warehouse configured.
	ErrDisabled = errors.New("warehouse: not configured")
	// ErrForbidden: raw SQL disabled, or statement not allowed.
	ErrForbidden = errors.New("warehouse: query not allowed")
	// ErrNotFound: unknown source or named query.
	ErrNotFound = errors.New("warehouse: not found")
	// ErrBadRequest: invalid parameters.
	ErrBadRequest = errors.New("warehouse: bad request")
)

// Column describes a result column.
type Column struct {
	Name string `json:"name"`
	Type string `json:"type"` // engine type name, e.g. STRING, INT64, DATE, int4, varchar
}

// Result is a query result. Values are JSON-friendly: strings, float64/int64,
// bool, nil, RFC3339 strings for timestamps, ISO dates for dates. Truncated
// reports that the query had more rows than the result holds (row or size
// cap).
type Result struct {
	Columns   []Column         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	RowCount  int              `json:"row_count"`
	Truncated bool             `json:"truncated"`
}

// DefaultMaxResultBytes is the default result-size cap (Limits.MaxBytes):
// 32 MiB of estimated row data.
const DefaultMaxResultBytes = 32 << 20

// Limits bound the result of Source.Query.
type Limits struct {
	// MaxRows is the maximum number of rows returned (values <= 0 mean 1).
	MaxRows int
	// MaxBytes caps the estimated size of the returned rows: their JSON
	// encoding plus a fixed per-value overhead for holding them in memory
	// (values <= 0 mean DefaultMaxResultBytes). Sources stop at the first
	// row that does not fit and never buffer a row much larger than what
	// is left, so one query cannot make the server hold an arbitrarily
	// large result.
	MaxBytes int
}

// WithDefaults returns l with its zero or negative fields replaced by their
// defaults.
func (l Limits) WithDefaults() Limits {
	if l.MaxRows <= 0 {
		l.MaxRows = 1
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxResultBytes
	}
	return l
}

// Source executes SQL against one engine. sql uses the engine's native
// placeholder syntax already (the service translates "?" / "@name").
type Source interface {
	// Query runs sql with positional args and returns the rows that fit
	// within lim (Truncated=true if more were available).
	Query(ctx context.Context, sql string, args []any, lim Limits) (*Result, error)
	// Placeholder returns the engine's positional placeholder for argument i
	// (0-based): "$1" for postgres, "?" for bigquery/athena.
	Placeholder(i int) string
	// Close releases resources.
	Close() error
}

// QueryRequest is the client request (SPEC §5.5).
type QueryRequest struct {
	SQL    string         `json:"sql,omitempty"`
	Query  string         `json:"query,omitempty"`  // named query
	Params any            `json:"params,omitempty"` // []any for sql, map[string]any for named
	Source string         `json:"source,omitempty"`
	Extra  map[string]any `json:"-"`
}

// SourceInfo / QueryInfo are returned by GET /warehouse.
type SourceInfo struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// QueryInfo describes a named query.
type QueryInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Params      []string `json:"params"`
	Source      string   `json:"source"`
}

// Service is what the HTTP layer uses. Implemented by service.Service.
type Service interface {
	Query(ctx context.Context, req QueryRequest) (*Result, error)
	Describe() (sources []SourceInfo, queries []QueryInfo)
	Enabled() bool
	Close() error
}
