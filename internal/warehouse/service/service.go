// Package service implements warehouse.Service on top of the configured
// sources (SPEC §5.5). It owns every policy decision so that sources only
// execute SQL:
//
//   - Sources are constructed lazily, on their first query, and reused.
//   - Raw SQL ({"sql": …}) is accepted only with warehouse.allow_raw_sql.
//   - Unless warehouse.allow_any_statement, raw SQL must be a single
//     statement starting with SELECT or WITH (after whitespace, comments and
//     opening parentheses). The check uses the engine's lexical rules (string
//     literals, quoted identifiers, comments; line comments end at CR or
//     LF), and for Postgres sources the statement must pass under every
//     combination of backslash-escaping strings and dollar quoting found in
//     Postgres-compatible engines (PostgreSQL with or without
//     standard_conforming_strings, Redshift, CockroachDB). Block comments
//     containing "/*", NUL bytes, invalid UTF-8 and number-adjacent E'…'
//     literals are rejected because engines disagree on them. Postgres
//     sources additionally run every query in a READ ONLY transaction (see
//     sqlsource).
//   - "?" placeholders in raw SQL (outside literals, quoted identifiers and
//     comments) are translated to the engine's positional syntax ("$1" for
//     Postgres, "?" for BigQuery/Athena); params must be a JSON array of
//     scalars whose length matches the placeholder count.
//   - Named queries (warehouse.queries) are pre-approved by the operator and
//     are not subject to the statement guard. Their "@name" placeholders are
//     bound from the params object; every declared parameter is required and
//     unknown parameters are rejected.
//   - Results are capped at warehouse.max_rows and, by default, at 32 MiB
//     of row data (their JSON encoding plus a small per-value overhead;
//     Truncated reports either cap), and queries are bounded by
//     warehouse.timeout.
//     Sources enforce the size cap while they read, so a few very wide rows
//     cannot make the server hold an arbitrarily large result.
//
// Trailing semicolons and comments are removed before a statement is sent.
// Parameter values and SQL text are never logged.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
	"github.com/thasami/backyard/internal/warehouse/athena"
	"github.com/thasami/backyard/internal/warehouse/bigquery"
	"github.com/thasami/backyard/internal/warehouse/internal/conv"
	"github.com/thasami/backyard/internal/warehouse/sqlsource"
)

const (
	defaultMaxRows = 10000
	defaultTimeout = 60 * time.Second
)

// ErrClosed is returned by Query after Close.
var ErrClosed = errors.New("warehouse: service closed")

// OpenFunc constructs the source called name. ctx is not canceled when the
// triggering request ends, so it may be retained by long-lived clients.
type OpenFunc func(ctx context.Context, name string, src config.WarehouseSource) (warehouse.Source, error)

// Options customise NewWithOptions.
type Options struct {
	// Logger receives one record per query (default slog.Default()).
	Logger *slog.Logger
	// Open replaces the default source constructor (bigquery, sqlsource,
	// athena), e.g. to inject fakes or pre-configured clients.
	Open OpenFunc
	// Observe, if set, is called once per Query call that gets past the
	// enabled/closed checks, e.g. to feed the
	// warehouse_queries_total{source,result} metric. source is the selected
	// source ("" if none could be resolved); result is "ok",
	// "bad_request", "forbidden", "not_found", "timeout", "canceled" or
	// "upstream"; elapsed is the time spent in the source (0 for requests
	// rejected before reaching it).
	Observe func(source, result string, elapsed time.Duration)
	// MaxResultBytes overrides the result-size cap (default
	// warehouse.DefaultMaxResultBytes; see warehouse.Limits.MaxBytes).
	MaxResultBytes int
}

// Service is the policy-enforcing warehouse.Service. It is safe for
// concurrent use.
type Service struct {
	log     *slog.Logger
	open    OpenFunc
	observe func(source, result string, elapsed time.Duration)
	baseCtx context.Context

	allowRaw bool
	allowAny bool
	maxRows  int
	maxBytes int
	timeout  time.Duration

	defaultSource string
	sources       map[string]*sourceEntry
	queries       map[string]*namedQuery
	sourceInfo    []warehouse.SourceInfo
	queryInfo     []warehouse.QueryInfo

	closed atomic.Bool
}

var _ warehouse.Service = (*Service)(nil)

// sourceEntry is one configured source and its lazily opened connection.
type sourceEntry struct {
	name     string
	cfg      config.WarehouseSource
	dialects []dialect

	mu  sync.Mutex
	src warehouse.Source
}

// namedQuery is a configured query parsed into a template.
type namedQuery struct {
	name   string
	source *sourceEntry
	params []string
	tmpl   template
}

var paramNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// New connects to every configured source lazily (on first query) and
// returns the policy-enforcing service. With no sources, Enabled() is false
// and Query returns warehouse.ErrDisabled.
func New(ctx context.Context, cfg config.WarehouseConfig) (warehouse.Service, error) {
	s, err := NewWithOptions(ctx, cfg, Options{})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// NewWithOptions is New with injectable dependencies. Configuration errors
// (unknown source types, named queries referring to unknown sources or with
// malformed SQL/parameter declarations) are reported here; connection errors
// surface on the first query of each source.
func NewWithOptions(ctx context.Context, cfg config.WarehouseConfig, opts Options) (*Service, error) {
	s := &Service{
		log:           opts.Logger,
		open:          opts.Open,
		observe:       opts.Observe,
		baseCtx:       context.WithoutCancel(ctx),
		allowRaw:      cfg.AllowRawSQL,
		allowAny:      cfg.AllowAnyStatement,
		maxRows:       cfg.MaxRows,
		maxBytes:      opts.MaxResultBytes,
		timeout:       cfg.Timeout.D(),
		defaultSource: cfg.DefaultSource,
		sources:       make(map[string]*sourceEntry, len(cfg.Sources)),
		queries:       make(map[string]*namedQuery, len(cfg.Queries)),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.open == nil {
		s.open = openSource
	}
	if s.maxRows <= 0 {
		s.maxRows = defaultMaxRows
	}
	if s.maxBytes <= 0 {
		s.maxBytes = warehouse.DefaultMaxResultBytes
	}
	if s.timeout <= 0 {
		s.timeout = defaultTimeout
	}
	if s.defaultSource == "" && len(cfg.Sources) == 1 {
		for name := range cfg.Sources {
			s.defaultSource = name
		}
	}

	for name, src := range cfg.Sources {
		switch src.Type {
		case "postgres", "bigquery", "athena":
		default:
			return nil, fmt.Errorf("warehouse: source %q: unsupported type %q (want bigquery, postgres or athena)", name, src.Type)
		}
		s.sources[name] = &sourceEntry{name: name, cfg: src, dialects: dialectsFor(src.Type)}
		s.sourceInfo = append(s.sourceInfo, warehouse.SourceInfo{Name: name, Type: src.Type, Description: src.Description})
	}
	if s.defaultSource != "" {
		if _, ok := s.sources[s.defaultSource]; !ok {
			return nil, fmt.Errorf("warehouse: default_source %q is not defined", s.defaultSource)
		}
	}
	sort.Slice(s.sourceInfo, func(i, j int) bool { return s.sourceInfo[i].Name < s.sourceInfo[j].Name })

	for name, q := range cfg.Queries {
		nq, err := s.parseNamedQuery(name, q)
		if err != nil {
			return nil, err
		}
		s.queries[name] = nq
		s.queryInfo = append(s.queryInfo, warehouse.QueryInfo{
			Name:        name,
			Description: q.Description,
			Params:      append([]string{}, nq.params...),
			Source:      nq.source.name,
		})
	}
	sort.Slice(s.queryInfo, func(i, j int) bool { return s.queryInfo[i].Name < s.queryInfo[j].Name })
	return s, nil
}

func (s *Service) parseNamedQuery(name string, q config.NamedQuery) (*namedQuery, error) {
	srcName := q.Source
	if srcName == "" {
		srcName = s.defaultSource
	}
	entry, ok := s.sources[srcName]
	if !ok {
		return nil, fmt.Errorf("warehouse: query %q: source %q is not defined", name, srcName)
	}
	declared := make(map[string]bool, len(q.Params))
	for _, p := range q.Params {
		if !paramNameRE.MatchString(p) {
			return nil, fmt.Errorf("warehouse: query %q: invalid parameter name %q", name, p)
		}
		if declared[p] {
			return nil, fmt.Errorf("warehouse: query %q: duplicate parameter %q", name, p)
		}
		declared[p] = true
	}
	sc, err := scanSQL(q.SQL, entry.dialects[0])
	if err != nil {
		return nil, fmt.Errorf("warehouse: query %q: %w", name, err)
	}
	end := sc.contentEnd()
	if end == 0 {
		return nil, fmt.Errorf("warehouse: query %q: sql is empty", name)
	}
	var slots []slot
	used := make(map[string]bool)
	for _, sl := range sc.atSlots(end) {
		if declared[sl.name] {
			slots = append(slots, sl)
			used[sl.name] = true
			continue
		}
		s.log.Warn("warehouse: named query uses an undeclared @parameter; it is sent to the engine verbatim",
			"query", name, "parameter", sl.name)
	}
	for _, p := range q.Params {
		if !used[p] {
			s.log.Warn("warehouse: named query declares a parameter it does not use", "query", name, "parameter", p)
		}
	}
	return &namedQuery{
		name:   name,
		source: entry,
		params: append([]string{}, q.Params...),
		tmpl:   newTemplate(q.SQL, end, slots),
	}, nil
}

// Enabled reports whether at least one source is configured.
func (s *Service) Enabled() bool { return len(s.sources) > 0 }

// Describe lists the configured sources and named queries, sorted by name.
func (s *Service) Describe() ([]warehouse.SourceInfo, []warehouse.QueryInfo) {
	sources := append([]warehouse.SourceInfo{}, s.sourceInfo...)
	queries := make([]warehouse.QueryInfo, len(s.queryInfo))
	for i, q := range s.queryInfo {
		q.Params = append([]string{}, q.Params...)
		queries[i] = q
	}
	return sources, queries
}

// Close closes every source opened so far. Queries after Close fail with
// ErrClosed.
func (s *Service) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	var errs []error
	for _, e := range s.sources {
		e.mu.Lock()
		if e.src != nil {
			if err := e.src.Close(); err != nil {
				errs = append(errs, fmt.Errorf("warehouse: close source %q: %w", e.name, err))
			}
			e.src = nil
		}
		e.mu.Unlock()
	}
	return errors.Join(errs...)
}

// plan is a validated request ready to execute.
type plan struct {
	entry *sourceEntry
	kind  string // "sql" | "named"
	query string // named query name
	tmpl  template
	args  []any
}

// Query validates req against the policy, runs it on the selected source and
// returns at most warehouse.max_rows rows of at most MaxResultBytes.
func (s *Service) Query(ctx context.Context, req warehouse.QueryRequest) (*warehouse.Result, error) {
	if !s.Enabled() {
		return nil, warehouse.ErrDisabled
	}
	if s.closed.Load() {
		return nil, ErrClosed
	}
	p, err := s.plan(req)
	if err != nil {
		s.observeResult(s.requestedSource(req), err, 0)
		return nil, err
	}
	src, err := s.source(p.entry)
	if errors.Is(err, ErrClosed) {
		return nil, err
	}
	if err != nil {
		s.log.LogAttrs(ctx, slog.LevelError, "warehouse source unavailable",
			slog.String("source", p.entry.name), slog.String("error", err.Error()))
		s.observeResult(p.entry.name, err, 0)
		return nil, err
	}
	sql := p.tmpl.render(src.Placeholder)

	qctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	start := time.Now()
	res, err := src.Query(qctx, sql, p.args, warehouse.Limits{MaxRows: s.maxRows, MaxBytes: s.maxBytes})
	elapsed := time.Since(start)
	if err != nil {
		err = s.classify(ctx, qctx, p.entry.name, err)
		s.logQuery(ctx, p, len(sql), elapsed, nil, 0, err)
		s.observeResult(p.entry.name, err, elapsed)
		return nil, err
	}
	res, size := s.finish(res)
	s.logQuery(ctx, p, len(sql), elapsed, res, size, nil)
	s.observeResult(p.entry.name, nil, elapsed)
	return res, nil
}

// requestedSource is the source a rejected request would have used, if it
// names a configured one.
func (s *Service) requestedSource(req warehouse.QueryRequest) string {
	if q, ok := s.queries[req.Query]; ok && req.Query != "" {
		return q.source.name
	}
	name := req.Source
	if name == "" {
		name = s.defaultSource
	}
	if _, ok := s.sources[name]; ok {
		return name
	}
	return ""
}

func (s *Service) observeResult(source string, err error, elapsed time.Duration) {
	if s.observe == nil {
		return
	}
	result := "ok"
	if err != nil {
		result = errorKind(err)
	}
	s.observe(source, result, elapsed)
}

func (s *Service) plan(req warehouse.QueryRequest) (*plan, error) {
	switch {
	case req.SQL != "" && req.Query != "":
		return nil, fmt.Errorf("%w: specify either sql or query, not both", warehouse.ErrBadRequest)
	case req.Query != "":
		return s.planNamed(req)
	case req.SQL != "":
		return s.planRaw(req)
	default:
		return nil, fmt.Errorf("%w: sql or query is required", warehouse.ErrBadRequest)
	}
}

func (s *Service) planNamed(req warehouse.QueryRequest) (*plan, error) {
	q, ok := s.queries[req.Query]
	if !ok {
		return nil, fmt.Errorf("%w: unknown query %q", warehouse.ErrNotFound, req.Query)
	}
	if req.Source != "" && req.Source != q.source.name {
		return nil, fmt.Errorf("%w: query %q runs on source %q, not %q", warehouse.ErrBadRequest, q.name, q.source.name, req.Source)
	}
	values, err := namedParams(req.Params, q.params)
	if err != nil {
		return nil, err
	}
	args := make([]any, len(q.tmpl.names))
	for i, name := range q.tmpl.names {
		args[i] = values[name]
	}
	return &plan{entry: q.source, kind: "named", query: q.name, tmpl: q.tmpl, args: args}, nil
}

func (s *Service) planRaw(req warehouse.QueryRequest) (*plan, error) {
	if !s.allowRaw {
		return nil, fmt.Errorf("%w: raw SQL is disabled (warehouse.allow_raw_sql); use a named query", warehouse.ErrForbidden)
	}
	name := req.Source
	if name == "" {
		name = s.defaultSource
	}
	if name == "" {
		return nil, fmt.Errorf("%w: source is required (no default source is configured)", warehouse.ErrBadRequest)
	}
	entry, ok := s.sources[name]
	if !ok {
		return nil, fmt.Errorf("%w: unknown source %q", warehouse.ErrNotFound, name)
	}
	// Engines disagree on NUL bytes (the Postgres protocol truncates at
	// them) and on invalid UTF-8 (replaced or rejected), so neither reaches
	// the lexer whose reading the guard relies on.
	if strings.IndexByte(req.SQL, 0) >= 0 {
		return nil, fmt.Errorf("%w: sql contains a NUL byte", warehouse.ErrBadRequest)
	}
	if !utf8.ValidString(req.SQL) {
		return nil, fmt.Errorf("%w: sql is not valid UTF-8", warehouse.ErrBadRequest)
	}

	sc, err := scanSQL(req.SQL, entry.dialects[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", warehouse.ErrBadRequest, err)
	}
	end := sc.contentEnd()
	if end == 0 {
		return nil, fmt.Errorf("%w: sql is empty", warehouse.ErrBadRequest)
	}
	if !s.allowAny {
		if err := guard(req.SQL, entry.dialects); err != nil {
			return nil, err
		}
	}
	slots := sc.questionSlots(end)
	args, err := positionalParams(req.Params)
	if err != nil {
		return nil, err
	}
	if len(args) != len(slots) {
		return nil, fmt.Errorf("%w: sql has %d placeholder(s) but %d param(s) were given", warehouse.ErrBadRequest, len(slots), len(args))
	}
	return &plan{entry: entry, kind: "sql", tmpl: newTemplate(req.SQL, end, slots), args: args}, nil
}

// guard enforces the read-only statement policy: under every lexical reading
// of the source's dialect the statement must start with SELECT or WITH and
// contain no statement separator before its trailing semicolons.
func guard(sql string, dialects []dialect) error {
	for _, d := range dialects {
		sc, err := scanSQL(sql, d)
		if err != nil {
			return fmt.Errorf("%w: %v (as %s)", warehouse.ErrBadRequest, err, d.name)
		}
		if sc.nestedOpen {
			return fmt.Errorf("%w: nested block comments are not allowed", warehouse.ErrForbidden)
		}
		if sc.ambiguousPrefix {
			return fmt.Errorf("%w: ambiguous string literal: separate a number from a following E'…' literal", warehouse.ErrForbidden)
		}
		switch kw := sc.leadingKeyword(); kw {
		case "SELECT", "WITH":
		case "":
			return fmt.Errorf("%w: only SELECT or WITH statements are allowed", warehouse.ErrForbidden)
		default:
			return fmt.Errorf("%w: only SELECT or WITH statements are allowed (got %s)", warehouse.ErrForbidden, truncate(kw, 32))
		}
		if sc.hasStatementSeparator(sc.contentEnd()) {
			return fmt.Errorf("%w: multiple statements are not allowed", warehouse.ErrForbidden)
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// source returns the entry's source, opening it on first use. Failed opens
// are not cached: the next query retries.
func (s *Service) source(e *sourceEntry) (warehouse.Source, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.closed.Load() {
		return nil, ErrClosed
	}
	if e.src != nil {
		return e.src, nil
	}
	src, err := s.open(s.baseCtx, e.name, e.cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: source %q: %w", warehouse.ErrUpstream, e.name, err)
	}
	if src == nil {
		return nil, fmt.Errorf("%w: source %q: constructor returned no source", warehouse.ErrUpstream, e.name)
	}
	e.src = src
	return src, nil
}

// sentinels are the errors Query passes through unchanged.
var sentinels = []error{
	warehouse.ErrBadRequest, warehouse.ErrForbidden, warehouse.ErrNotFound,
	warehouse.ErrTimeout, warehouse.ErrUpstream, warehouse.ErrDisabled,
}

// classify makes sure a source error wraps exactly one warehouse sentinel
// (or context.Canceled when the caller went away).
func (s *Service) classify(ctx, qctx context.Context, source string, err error) error {
	if ctx.Err() != nil && !errors.Is(err, warehouse.ErrBadRequest) && !errors.Is(err, warehouse.ErrForbidden) {
		// The caller canceled (or its own deadline passed): not our timeout.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			if errors.Is(err, warehouse.ErrTimeout) {
				return fmt.Errorf("source %q: %w", source, err)
			}
			return fmt.Errorf("%w: source %q: %w", warehouse.ErrTimeout, source, err)
		}
		return fmt.Errorf("warehouse: source %q: %w", source, context.Canceled)
	}
	for _, sentinel := range sentinels {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("source %q: %w", source, err)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(qctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: source %q: query exceeded %s: %w", warehouse.ErrTimeout, source, s.timeout, err)
	}
	return fmt.Errorf("%w: source %q: %w", warehouse.ErrUpstream, source, err)
}

// finish enforces the result invariants: non-nil slices, at most maxRows
// rows of at most maxBytes (estimated as the sources do), RowCount ==
// len(Rows). It also returns the estimated size of the rows.
func (s *Service) finish(res *warehouse.Result) (*warehouse.Result, int) {
	if res == nil {
		res = &warehouse.Result{}
	}
	if res.Columns == nil {
		res.Columns = []warehouse.Column{}
	}
	if res.Rows == nil {
		res.Rows = []map[string]any{}
	}
	if len(res.Rows) > s.maxRows {
		res.Rows = res.Rows[:s.maxRows]
		res.Truncated = true
	}
	budget := conv.NewBudget(s.maxBytes)
	for i, row := range res.Rows {
		if !budget.Add(row) {
			res.Rows = res.Rows[:i]
			res.Truncated = true
			break
		}
	}
	res.RowCount = len(res.Rows)
	return res, budget.Used()
}

func (s *Service) logQuery(ctx context.Context, p *plan, sqlBytes int, elapsed time.Duration, res *warehouse.Result, resultBytes int, err error) {
	attrs := []slog.Attr{
		slog.String("source", p.entry.name),
		slog.String("kind", p.kind),
		slog.Int("sql_bytes", sqlBytes),
		slog.Int("params", len(p.args)),
		slog.Duration("duration", elapsed),
	}
	if p.query != "" {
		attrs = append(attrs, slog.String("query", p.query))
	}
	level := slog.LevelInfo
	if err != nil {
		kind := errorKind(err)
		attrs = append(attrs, slog.String("result", kind))
		// Client errors are expected and their engine messages may quote
		// parameter values, so only infrastructure errors are logged in full.
		if kind == "upstream" || kind == "timeout" {
			level = slog.LevelWarn
			attrs = append(attrs, slog.String("error", err.Error()))
		}
	} else {
		attrs = append(attrs, slog.String("result", "ok"))
		attrs = append(attrs, slog.Int("rows", res.RowCount), slog.Int("result_bytes", resultBytes), slog.Bool("truncated", res.Truncated))
	}
	s.log.LogAttrs(ctx, level, "warehouse query", attrs...)
}

// errorKind classifies a Query error for logs.
func errorKind(err error) string {
	switch {
	case errors.Is(err, warehouse.ErrBadRequest):
		return "bad_request"
	case errors.Is(err, warehouse.ErrForbidden):
		return "forbidden"
	case errors.Is(err, warehouse.ErrNotFound):
		return "not_found"
	case errors.Is(err, warehouse.ErrTimeout):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "upstream"
	}
}

// openSource is the default OpenFunc.
func openSource(ctx context.Context, name string, src config.WarehouseSource) (warehouse.Source, error) {
	switch src.Type {
	case "postgres":
		return sqlsource.New(ctx, name, src, sqlsource.Options{})
	case "bigquery":
		return bigquery.New(ctx, name, src, bigquery.Options{})
	case "athena":
		return athena.New(ctx, name, src, athena.Options{})
	}
	return nil, fmt.Errorf("unsupported source type %q", src.Type)
}
