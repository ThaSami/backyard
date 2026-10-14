// Package athena implements warehouse.Source for Amazon Athena (Trino SQL)
// with the AWS SDK for Go v2.
//
// A query is submitted with StartQueryExecution (workgroup, database,
// catalog, output location and "?" parameters as ExecutionParameters), polled
// with GetQueryExecution using exponential backoff until it SUCCEEDS, FAILS
// or is CANCELLED, and read with paginated GetQueryResults. When the caller's
// context ends first the query is stopped with StopQueryExecution so it does
// not keep scanning (and billing). Credentials and region come from the
// default AWS chain; region in the source config takes precedence.
//
// Parameters are sent as SQL literals (Athena substitutes them server-side):
// strings are single-quoted with quotes doubled, numbers and booleans are
// written verbatim, nil becomes NULL.
//
// Result values are converted from Athena's textual results using the
// column types in ResultSetMetadata:
//
//	boolean                        → bool
//	tinyint/smallint/integer/bigint → int64
//	real/float/double              → float64 ("NaN"/"Infinity"/"-Infinity" strings)
//	decimal                        → float64 when lossless, else the exact decimal string
//	timestamp [with time zone]     → RFC 3339 string with nanoseconds, UTC
//	date                           → "YYYY-MM-DD"
//	varbinary                      → base64 string
//	json                           → decoded value
//	everything else                → string (array, map and row values keep
//	                                 Athena's display format; CAST them to JSON
//	                                 for structured values)
package athena

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	sdk "github.com/aws/aws-sdk-go-v2/service/athena"
	"github.com/aws/aws-sdk-go-v2/service/athena/types"
	"github.com/aws/smithy-go"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
	"github.com/thasami/backyard/internal/warehouse/internal/conv"
)

const (
	defaultPollInterval    = 250 * time.Millisecond
	defaultMaxPollInterval = 2 * time.Second
	// maxPageSize is GetQueryResults' maximum MaxResults.
	maxPageSize = 1000
	// stopTimeout bounds the best-effort StopQueryExecution call.
	stopTimeout = 10 * time.Second
)

// API is the subset of the Athena client used by Source.
type API interface {
	StartQueryExecution(ctx context.Context, in *sdk.StartQueryExecutionInput, optFns ...func(*sdk.Options)) (*sdk.StartQueryExecutionOutput, error)
	GetQueryExecution(ctx context.Context, in *sdk.GetQueryExecutionInput, optFns ...func(*sdk.Options)) (*sdk.GetQueryExecutionOutput, error)
	GetQueryResults(ctx context.Context, in *sdk.GetQueryResultsInput, optFns ...func(*sdk.Options)) (*sdk.GetQueryResultsOutput, error)
	StopQueryExecution(ctx context.Context, in *sdk.StopQueryExecutionInput, optFns ...func(*sdk.Options)) (*sdk.StopQueryExecutionOutput, error)
}

var _ API = (*sdk.Client)(nil)

// Options are optional settings.
type Options struct {
	// Logger receives warnings (default slog.Default()).
	Logger *slog.Logger
	// Client replaces the SDK client entirely (tests, custom middleware).
	Client API
	// AWSConfig replaces config.LoadDefaultConfig (custom credential
	// chains). The source's region still takes precedence.
	AWSConfig *aws.Config
	// Endpoint overrides the Athena endpoint URL (VPC endpoints, tests).
	Endpoint string
	// PollInterval is the first GetQueryExecution delay (default 250ms);
	// it grows by 1.5× up to MaxPollInterval (default 2s).
	PollInterval    time.Duration
	MaxPollInterval time.Duration
}

// Source is a warehouse.Source backed by Athena. It is safe for concurrent
// use.
type Source struct {
	name           string
	api            API
	workgroup      string
	database       string
	catalog        string
	outputLocation string
	pollMin        time.Duration
	pollMax        time.Duration
	log            *slog.Logger
}

var _ warehouse.Source = (*Source)(nil)

// New creates an Athena source. Loading the AWS configuration reads only
// local sources (environment, shared files); credentials are resolved per
// request.
func New(ctx context.Context, name string, src config.WarehouseSource, opts Options) (*Source, error) {
	s := &Source{
		name:           name,
		api:            opts.Client,
		workgroup:      src.Workgroup,
		database:       src.Database,
		catalog:        src.Catalog,
		outputLocation: src.OutputLocation,
		pollMin:        opts.PollInterval,
		pollMax:        opts.MaxPollInterval,
		log:            opts.Logger,
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.pollMin <= 0 {
		s.pollMin = defaultPollInterval
	}
	if s.pollMax < s.pollMin {
		s.pollMax = max(defaultMaxPollInterval, s.pollMin)
	}
	if s.api != nil {
		return s, nil
	}

	var cfg aws.Config
	if opts.AWSConfig != nil {
		cfg = opts.AWSConfig.Copy()
	} else {
		var load []func(*awsconfig.LoadOptions) error
		if src.Region != "" {
			load = append(load, awsconfig.WithRegion(src.Region))
		}
		var err error
		cfg, err = awsconfig.LoadDefaultConfig(ctx, load...)
		if err != nil {
			return nil, fmt.Errorf("athena %s: load AWS config: %w", name, err)
		}
	}
	if src.Region != "" {
		cfg.Region = src.Region
	}
	if cfg.Region == "" {
		return nil, fmt.Errorf("athena %s: region is required (or set AWS_REGION)", name)
	}
	s.api = sdk.NewFromConfig(cfg, func(o *sdk.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(strings.TrimRight(opts.Endpoint, "/"))
		}
	})
	return s, nil
}

// Placeholder returns "?" (Athena execution parameters).
func (s *Source) Placeholder(int) string { return "?" }

// Close is a no-op: the SDK client holds no resources that need releasing.
func (s *Source) Close() error { return nil }

// Query runs sql and returns the rows that fit within lim. No further pages
// are fetched once the size cap is reached.
func (s *Source) Query(ctx context.Context, sql string, args []any, lim warehouse.Limits) (*warehouse.Result, error) {
	lim = lim.WithDefaults()
	in, err := s.startInput(sql, args)
	if err != nil {
		return nil, err
	}
	out, err := s.api.StartQueryExecution(ctx, in)
	if err != nil {
		return nil, classify(ctx, err)
	}
	if out.QueryExecutionId == nil {
		return nil, fmt.Errorf("%w: athena: StartQueryExecution returned no query execution id", warehouse.ErrUpstream)
	}
	id := *out.QueryExecutionId
	exec, err := s.wait(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.results(ctx, id, exec.StatementType, lim)
}

// startInput builds the StartQueryExecution request.
func (s *Source) startInput(sql string, args []any) (*sdk.StartQueryExecutionInput, error) {
	in := &sdk.StartQueryExecutionInput{QueryString: aws.String(sql)}
	if s.workgroup != "" {
		in.WorkGroup = aws.String(s.workgroup)
	}
	if s.database != "" || s.catalog != "" {
		in.QueryExecutionContext = &types.QueryExecutionContext{}
		if s.database != "" {
			in.QueryExecutionContext.Database = aws.String(s.database)
		}
		if s.catalog != "" {
			in.QueryExecutionContext.Catalog = aws.String(s.catalog)
		}
	}
	if s.outputLocation != "" {
		in.ResultConfiguration = &types.ResultConfiguration{OutputLocation: aws.String(s.outputLocation)}
	}
	for i, a := range args {
		lit, err := Literal(a)
		if err != nil {
			return nil, fmt.Errorf("%w: params[%d]: %w", warehouse.ErrBadRequest, i, err)
		}
		in.ExecutionParameters = append(in.ExecutionParameters, lit)
	}
	return in, nil
}

// maxParamLength is Athena's limit on the length of one ExecutionParameters
// value.
const maxParamLength = 1024

// jsonNumberRE matches a JSON number (RFC 8259), which is also a valid Trino
// numeric literal. strconv.ParseFloat alone would accept "NaN", "Inf" and hex
// floats, which Trino would read as identifiers or reject.
var jsonNumberRE = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// Literal renders a JSON scalar as an Athena (Trino) SQL literal for
// ExecutionParameters. Strings are single-quoted with embedded quotes
// doubled (Trino strings have no backslash escapes), so a value can never
// leave its literal. Literals longer than Athena's 1024-character limit are
// rejected.
func Literal(v any) (string, error) {
	lit, err := literal(v)
	if err != nil {
		return "", err
	}
	if n := utf8.RuneCountInString(lit); n > maxParamLength {
		return "", fmt.Errorf("value is too long for an Athena parameter (%d characters, at most %d)", n, maxParamLength)
	}
	return lit, nil
}

func literal(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'", nil
	case bool:
		return strconv.FormatBool(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case int:
		return strconv.Itoa(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", fmt.Errorf("number %v is not finite", x)
		}
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".e") {
			s += ".0"
		}
		return s, nil
	case json.Number:
		if !jsonNumberRE.MatchString(string(x)) {
			return "", fmt.Errorf("invalid number %q", x.String())
		}
		return x.String(), nil
	}
	return "", fmt.Errorf("unsupported parameter type %T", v)
}

// wait polls the query until it reaches a terminal state.
func (s *Source) wait(ctx context.Context, id string) (*types.QueryExecution, error) {
	delay := s.pollMin
	for {
		out, err := s.api.GetQueryExecution(ctx, &sdk.GetQueryExecutionInput{QueryExecutionId: aws.String(id)})
		if err != nil {
			// The query would keep running (and scanning) after we give up.
			s.stop(ctx, id)
			return nil, classify(ctx, err)
		}
		exec := out.QueryExecution
		if exec == nil || exec.Status == nil {
			s.stop(ctx, id)
			return nil, fmt.Errorf("%w: athena: query %s: missing status", warehouse.ErrUpstream, id)
		}
		switch exec.Status.State {
		case types.QueryExecutionStateSucceeded:
			return exec, nil
		case types.QueryExecutionStateFailed:
			return nil, failure(id, exec.Status)
		case types.QueryExecutionStateCancelled:
			if ctx.Err() != nil {
				return nil, classify(ctx, ctx.Err())
			}
			return nil, fmt.Errorf("%w: athena: query %s was cancelled", warehouse.ErrUpstream, id)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.stop(ctx, id)
			return nil, classify(ctx, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*3/2, s.pollMax)
	}
}

// stop cancels a running query (best effort, detached from ctx).
func (s *Source) stop(ctx context.Context, id string) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()
	if _, err := s.api.StopQueryExecution(sctx, &sdk.StopQueryExecutionInput{QueryExecutionId: aws.String(id)}); err != nil {
		s.log.Warn("athena: stop query", "source", s.name, "query_execution_id", id, "error", err)
	}
}

// results pages through GetQueryResults.
func (s *Source) results(ctx context.Context, id string, stmt types.StatementType, lim warehouse.Limits) (*warehouse.Result, error) {
	maxRows := lim.MaxRows
	var (
		cols      []warehouse.Column
		colTypes  []string
		rows      = make([]map[string]any, 0, min(maxRows, 1024))
		truncated bool
		token     *string
		first     = true
		budget    = conv.NewBudget(lim.MaxBytes)
	)
	for {
		want := maxRows - len(rows) + 1 // one extra row detects truncation
		if first {
			want++ // header row
		}
		in := &sdk.GetQueryResultsInput{
			QueryExecutionId: aws.String(id),
			NextToken:        token,
			MaxResults:       aws.Int32(int32(min(max(want, 1), maxPageSize))),
		}
		out, err := s.api.GetQueryResults(ctx, in)
		if err != nil {
			return nil, classify(ctx, err)
		}
		if out.ResultSet == nil {
			break
		}
		data := out.ResultSet.Rows
		if first {
			first = false
			var info []types.ColumnInfo
			if out.ResultSet.ResultSetMetadata != nil {
				info = out.ResultSet.ResultSetMetadata.ColumnInfo
			}
			cols, colTypes = columns(info)
			if len(data) > 0 && isHeader(data[0], info, stmt) {
				data = data[1:]
			}
		}
		for _, r := range data {
			if len(rows) == maxRows {
				truncated = true
				break
			}
			row := convertRow(r, cols, colTypes)
			if !budget.Add(row) {
				truncated = true
				break
			}
			rows = append(rows, row)
		}
		if truncated || out.NextToken == nil || *out.NextToken == "" {
			break
		}
		token = out.NextToken
	}
	if cols == nil {
		cols = []warehouse.Column{}
	}
	return &warehouse.Result{Columns: cols, Rows: rows, RowCount: len(rows), Truncated: truncated}, nil
}

// columns derives result columns (with unique names) and lower-cased types.
func columns(info []types.ColumnInfo) ([]warehouse.Column, []string) {
	names := make([]string, len(info))
	typs := make([]string, len(info))
	for i, c := range info {
		names[i] = aws.ToString(c.Name)
		if names[i] == "" {
			names[i] = aws.ToString(c.Label)
		}
		typs[i] = strings.ToLower(strings.TrimSpace(aws.ToString(c.Type)))
	}
	keys := conv.UniqueNames(names)
	cols := make([]warehouse.Column, len(info))
	for i := range info {
		cols[i] = warehouse.Column{Name: keys[i], Type: typs[i]}
	}
	return cols, typs
}

// isHeader reports whether row is the column-header row Athena prepends to
// the first page of SELECT (DML) results.
func isHeader(row types.Row, info []types.ColumnInfo, stmt types.StatementType) bool {
	if stmt == types.StatementTypeDml {
		return true
	}
	if len(row.Data) != len(info) || len(info) == 0 {
		return false
	}
	for i, d := range row.Data {
		v := aws.ToString(d.VarCharValue)
		if v != aws.ToString(info[i].Name) && v != aws.ToString(info[i].Label) {
			return false
		}
	}
	return true
}

// convertRow converts one result row.
func convertRow(r types.Row, cols []warehouse.Column, colTypes []string) map[string]any {
	row := make(map[string]any, len(cols))
	for i, c := range cols {
		if i >= len(r.Data) || r.Data[i].VarCharValue == nil {
			row[c.Name] = nil
			continue
		}
		row[c.Name] = ConvertValue(colTypes[i], *r.Data[i].VarCharValue)
	}
	return row
}

// ConvertValue converts Athena's textual rendering s of a value of type typ
// (as reported in ColumnInfo.Type) to a JSON-friendly value. Values that do
// not parse as their declared type are returned as strings.
func ConvertValue(typ, s string) any {
	switch baseType(typ) {
	case "boolean":
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	case "tinyint", "smallint", "integer", "int", "bigint":
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n
		}
	case "real", "float", "double":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return conv.Float(f)
		}
	case "decimal":
		return conv.Decimal(s)
	case "timestamp":
		if t, ok := parseTimestamp(s, time.UTC); ok {
			return conv.Timestamp(t)
		}
	case "timestamp with time zone":
		if t, ok := parseTimestampTZ(s); ok {
			return conv.Timestamp(t)
		}
	case "varbinary":
		if b, err := hex.DecodeString(strings.ReplaceAll(s, " ", "")); err == nil {
			return base64.StdEncoding.EncodeToString(b)
		}
	case "json":
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v
		}
	}
	return s
}

const timestampLayout = "2006-01-02 15:04:05.999999999"

func parseTimestamp(s string, loc *time.Location) (time.Time, bool) {
	for _, layout := range []string{timestampLayout, "2006-01-02T15:04:05.999999999"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseTimestampTZ parses "2026-01-02 03:04:05.000 UTC",
// "… America/New_York" or "… +05:30".
func parseTimestampTZ(s string) (time.Time, bool) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return time.Time{}, false
	}
	local, zone := s[:i], s[i+1:]
	if strings.HasPrefix(zone, "+") || strings.HasPrefix(zone, "-") {
		if t, err := time.Parse(timestampLayout+" -07:00", s); err == nil {
			return t, true
		}
		return time.Time{}, false
	}
	loc, err := loadLocation(zone)
	if err != nil {
		return time.Time{}, false
	}
	return parseTimestamp(local, loc)
}

// zones caches time zones by name: time.LoadLocation reads the zone database
// on every call, and a result can hold thousands of zoned timestamps. Only
// successfully loaded (hence IANA-named, bounded) zones are cached.
var zones sync.Map // string → *time.Location

func loadLocation(name string) (*time.Location, error) {
	if loc, ok := zones.Load(name); ok {
		return loc.(*time.Location), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	zones.Store(name, loc)
	return loc, nil
}

// baseType lower-cases a column type and drops its parameter lists and
// redundant spaces: "timestamp(3) with time zone" → "timestamp with time
// zone", "decimal(38,9)" → "decimal", "array(integer)" → "array".
func baseType(typ string) string {
	var b strings.Builder
	depth := 0
	for _, r := range strings.ToLower(typ) {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth = max(depth-1, 0)
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// failure converts a FAILED query status into an error.
func failure(id string, st *types.QueryExecutionStatus) error {
	reason := aws.ToString(st.StateChangeReason)
	category := int32(0)
	if ae := st.AthenaError; ae != nil {
		if ae.ErrorCategory != nil {
			category = *ae.ErrorCategory
		}
		if reason == "" {
			reason = aws.ToString(ae.ErrorMessage)
		}
	}
	if reason == "" {
		reason = "unknown error"
	}
	sentinel := warehouse.ErrUpstream
	lower := strings.ToLower(reason)
	switch {
	case strings.Contains(lower, "access denied") || strings.Contains(lower, "accessdenied") || strings.Contains(lower, "not authorized"):
		sentinel = warehouse.ErrForbidden
	case category == 2: // user error
		sentinel = warehouse.ErrBadRequest
	}
	return fmt.Errorf("%w: athena: query %s failed: %s", sentinel, id, reason)
}

// classify wraps an API error with the matching warehouse sentinel.
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", warehouse.ErrTimeout, errors.Join(ctx.Err(), err))
		}
		return errors.Join(ctx.Err(), err)
	}
	var invalid *types.InvalidRequestException
	if errors.As(err, &invalid) {
		return fmt.Errorf("%w: athena: %s", warehouse.ErrBadRequest, aws.ToString(invalid.Message))
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "AccessDeniedException":
			return fmt.Errorf("%w: athena: %w", warehouse.ErrForbidden, err)
		}
	}
	return fmt.Errorf("%w: athena: %w", warehouse.ErrUpstream, err)
}
