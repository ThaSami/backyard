// Package bigquery implements warehouse.Source for Google BigQuery
// (GoogleSQL) with cloud.google.com/go/bigquery.
//
// Queries run as query jobs in the configured project and location with
// positional ("?") parameters, maximum_bytes_billed (max_bytes_billed) and a
// server-side job timeout matching the context deadline; a job whose caller
// goes away is canceled. Credentials come from credentials_file (service
// account, authorized user, impersonated service account or workload
// identity federation JSON) or Application Default Credentials.
//
// Result values are converted to JSON-friendly Go values:
//
//	INT64              → int64
//	FLOAT64            → float64 ("NaN"/"Infinity"/"-Infinity" strings)
//	NUMERIC/BIGNUMERIC → float64 when lossless, else the exact decimal string
//	BOOL, STRING       → bool, string
//	BYTES              → base64 string
//	TIMESTAMP          → RFC 3339 string with nanoseconds, UTC
//	DATE               → "YYYY-MM-DD"
//	TIME               → "HH:MM:SS[.fffffffff]"
//	DATETIME           → "YYYY-MM-DDTHH:MM:SS[.fffffffff]"
//	JSON               → decoded value
//	GEOGRAPHY          → WKT string
//	INTERVAL           → canonical interval string ("Y-M D H:M:S")
//	RANGE<T>           → {"start": T|null, "end": T|null}
//	STRUCT             → object
//	ARRAY<T>           → array
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
	"os"
	"strings"
	"time"

	bq "cloud.google.com/go/bigquery"
	"cloud.google.com/go/civil"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
	"github.com/thasami/backyard/internal/warehouse/internal/conv"
)

const (
	// maxPageSize bounds the rows fetched per getQueryResults page.
	maxPageSize = 10000
	// cancelTimeout bounds the best-effort jobs.cancel call.
	cancelTimeout = 10 * time.Second
)

// Options are optional settings.
type Options struct {
	// Logger receives warnings (default slog.Default()).
	Logger *slog.Logger
	// ClientOptions are appended to the options derived from the source
	// configuration (endpoint overrides, HTTP clients, credentials).
	ClientOptions []option.ClientOption
}

// Source is a warehouse.Source backed by a BigQuery client. It is safe for
// concurrent use.
type Source struct {
	name           string
	client         *bq.Client
	location       string
	maxBytesBilled int64
	log            *slog.Logger
}

var _ warehouse.Source = (*Source)(nil)

// New creates a BigQuery client billed to src.ProjectID. ctx must stay valid
// for the lifetime of the source (credentials may be refreshed with it).
func New(ctx context.Context, name string, src config.WarehouseSource, opts Options) (*Source, error) {
	if src.ProjectID == "" {
		return nil, fmt.Errorf("bigquery %s: project_id is required", name)
	}
	var copts []option.ClientOption
	if src.CredentialsFile != "" {
		ct, err := credentialsType(src.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("bigquery %s: %w", name, err)
		}
		copts = append(copts, option.WithAuthCredentialsFile(ct, src.CredentialsFile))
	}
	copts = append(copts, opts.ClientOptions...)
	client, err := bq.NewClient(ctx, src.ProjectID, copts...)
	if err != nil {
		return nil, fmt.Errorf("bigquery %s: create client: %w", name, err)
	}
	if src.Location != "" {
		client.Location = src.Location
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Source{name: name, client: client, location: src.Location, maxBytesBilled: src.MaxBytesBilled, log: log}, nil
}

// credentialsType reads the "type" of a credentials JSON file so it can be
// loaded with an explicit, validated type.
func credentialsType(path string) (option.CredentialsType, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read credentials file: %w", err)
	}
	defer f.Close()
	var v struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&v); err != nil {
		return "", fmt.Errorf("parse credentials file %q: %w", path, err)
	}
	switch t := option.CredentialsType(v.Type); t {
	case option.ServiceAccount, option.AuthorizedUser, option.ImpersonatedServiceAccount, option.ExternalAccount,
		option.CredentialsType("external_account_authorized_user"):
		return t, nil
	default:
		return "", fmt.Errorf("credentials file %q has unsupported type %q", path, v.Type)
	}
}

// Placeholder returns "?" (BigQuery positional parameters).
func (s *Source) Placeholder(int) string { return "?" }

// Close closes the client.
func (s *Source) Close() error { return s.client.Close() }

// Query runs sql as a query job and returns at most maxRows rows.
func (s *Source) Query(ctx context.Context, sql string, args []any, maxRows int) (*warehouse.Result, error) {
	if maxRows <= 0 {
		maxRows = 1
	}
	q, err := s.newQuery(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	job, err := q.Run(ctx)
	if err != nil {
		return nil, classify(ctx, err)
	}
	it, err := job.Read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			s.cancel(ctx, job)
		}
		return nil, classify(ctx, err)
	}
	it.PageInfo().MaxSize = min(maxRows+1, maxPageSize)

	var (
		rows      = make([]map[string]any, 0, min(maxRows, 1024))
		keys      []string
		truncated bool
	)
	for {
		var vals []bq.Value
		err := it.Next(&vals)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, classify(ctx, err)
		}
		if len(rows) == maxRows {
			truncated = true
			break
		}
		if keys == nil {
			keys = columnKeys(it.Schema)
		}
		row, err := convertRow(vals, it.Schema, keys)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", warehouse.ErrUpstream, s.name, err)
		}
		rows = append(rows, row)
	}
	return &warehouse.Result{
		Columns:   columns(it.Schema),
		Rows:      rows,
		RowCount:  len(rows),
		Truncated: truncated,
	}, nil
}

// newQuery builds the job configuration for sql and args.
func (s *Source) newQuery(ctx context.Context, sql string, args []any) (*bq.Query, error) {
	q := s.client.Query(sql)
	q.Location = s.location
	q.MaxBytesBilled = s.maxBytesBilled
	if dl, ok := ctx.Deadline(); ok {
		// BigQuery stops the job server-side if we cannot.
		q.JobTimeout = max(time.Until(dl).Truncate(time.Millisecond), time.Millisecond)
	}
	for i, a := range args {
		v, err := paramValue(a)
		if err != nil {
			return nil, fmt.Errorf("%w: params[%d]: %w", warehouse.ErrBadRequest, i, err)
		}
		q.Parameters = append(q.Parameters, bq.QueryParameter{Value: v})
	}
	return q, nil
}

// paramValue maps a normalized JSON scalar to a BigQuery parameter value.
func paramValue(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return bq.NullString{}, nil
	case string, bool, int64:
		return x, nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("number %v is not finite", x)
		}
		return x, nil
	case int:
		return int64(x), nil
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		// ParseFloat also accepts "NaN", "Inf" and hex floats, which are
		// not JSON numbers.
		f, err := x.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || strings.ContainsAny(string(x), "xXnN_") {
			return nil, fmt.Errorf("invalid number %q", x.String())
		}
		return f, nil
	}
	return nil, fmt.Errorf("unsupported parameter type %T", v)
}

// cancel asks BigQuery to stop job (best effort, detached from ctx).
func (s *Source) cancel(ctx context.Context, job *bq.Job) {
	cctx, done := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer done()
	if err := job.Cancel(cctx); err != nil {
		s.log.Warn("bigquery: cancel job", "source", s.name, "job", job.ID(), "error", err)
	}
}

// columns describes the result schema with GoogleSQL type names.
func columns(schema bq.Schema) []warehouse.Column {
	keys := columnKeys(schema)
	cols := make([]warehouse.Column, len(schema))
	for i, f := range schema {
		cols[i] = warehouse.Column{Name: keys[i], Type: typeName(f)}
	}
	return cols
}

func columnKeys(schema bq.Schema) []string {
	names := make([]string, len(schema))
	for i, f := range schema {
		names[i] = f.Name
	}
	return conv.UniqueNames(names)
}

// typeName renders a field's GoogleSQL type: INT64, ARRAY<STRING>,
// STRUCT<a INT64, b STRING>, RANGE<DATE>, …
func typeName(f *bq.FieldSchema) string {
	t := scalarTypeName(f)
	if f.Repeated {
		return "ARRAY<" + t + ">"
	}
	return t
}

func scalarTypeName(f *bq.FieldSchema) string {
	switch f.Type {
	case bq.IntegerFieldType:
		return "INT64"
	case bq.FloatFieldType:
		return "FLOAT64"
	case bq.BooleanFieldType:
		return "BOOL"
	case bq.RecordFieldType:
		parts := make([]string, len(f.Schema))
		for i, sf := range f.Schema {
			parts[i] = sf.Name + " " + typeName(sf)
		}
		return "STRUCT<" + strings.Join(parts, ", ") + ">"
	case bq.RangeFieldType:
		if f.RangeElementType != nil {
			return "RANGE<" + string(f.RangeElementType.Type) + ">"
		}
		return "RANGE"
	}
	return string(f.Type)
}

// convertRow converts one row of values (ordered as schema) into a map.
func convertRow(vals []bq.Value, schema bq.Schema, keys []string) (map[string]any, error) {
	if len(vals) != len(schema) {
		return nil, fmt.Errorf("row has %d values for %d columns", len(vals), len(schema))
	}
	row := make(map[string]any, len(vals))
	for i, f := range schema {
		row[keys[i]] = convertField(vals[i], f)
	}
	return row, nil
}

// convertField converts the value of field f (repeated or not).
func convertField(v bq.Value, f *bq.FieldSchema) any {
	if v == nil {
		if f.Repeated {
			return []any{}
		}
		return nil
	}
	if f.Repeated {
		list, ok := v.([]bq.Value)
		if !ok {
			return convertValue(v, f)
		}
		out := make([]any, len(list))
		for i, e := range list {
			out[i] = convertValue(e, f)
		}
		return out
	}
	return convertValue(v, f)
}

// convertValue converts a single (non-repeated) value of field f.
func convertValue(v bq.Value, f *bq.FieldSchema) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		if f.Type == bq.JSONFieldType {
			var out any
			if err := json.Unmarshal([]byte(x), &out); err == nil {
				return out
			}
		}
		return x
	case bool, int64:
		return x
	case int:
		return int64(x)
	case float64:
		return conv.Float(x)
	case []byte:
		return conv.Bytes(x)
	case *big.Rat:
		if x == nil {
			return nil
		}
		return conv.Rat(x, "")
	case time.Time:
		return conv.Timestamp(x)
	case civil.Date:
		return x.String()
	case civil.Time:
		return x.String()
	case civil.DateTime:
		return x.String()
	case *bq.IntervalValue:
		if x == nil {
			return nil
		}
		return x.String()
	case *bq.RangeValue:
		if x == nil {
			return nil
		}
		elem := &bq.FieldSchema{Type: bq.DateFieldType}
		if f.RangeElementType != nil {
			elem.Type = f.RangeElementType.Type
		}
		return map[string]any{"start": convertValue(x.Start, elem), "end": convertValue(x.End, elem)}
	case []bq.Value:
		if f.Type == bq.RecordFieldType && len(x) == len(f.Schema) {
			keys := columnKeys(f.Schema)
			obj := make(map[string]any, len(x))
			for i, sf := range f.Schema {
				obj[keys[i]] = convertField(x[i], sf)
			}
			return obj
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = convertValue(e, f)
		}
		return out
	case map[string]bq.Value:
		obj := make(map[string]any, len(x))
		for k, e := range x {
			var sf *bq.FieldSchema
			for _, c := range f.Schema {
				if c.Name == k {
					sf = c
					break
				}
			}
			if sf == nil {
				sf = &bq.FieldSchema{Type: bq.StringFieldType}
			}
			obj[k] = convertField(e, sf)
		}
		return obj
	}
	return fmt.Sprint(v)
}

// classify wraps err with the warehouse sentinel matching its cause.
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: %w", warehouse.ErrTimeout, errors.Join(ctx.Err(), err))
		}
		return errors.Join(ctx.Err(), err)
	}
	var (
		reason, msg string
		code        int
	)
	var gErr *googleapi.Error
	var jErr *bq.Error
	switch {
	case errors.As(err, &gErr):
		code, msg = gErr.Code, gErr.Message
		if len(gErr.Errors) > 0 {
			reason = gErr.Errors[0].Reason
			if msg == "" {
				msg = gErr.Errors[0].Message
			}
		}
	case errors.As(err, &jErr):
		reason, msg = jErr.Reason, jErr.Message
	default:
		return fmt.Errorf("%w: %w", warehouse.ErrUpstream, err)
	}
	if msg == "" {
		msg = err.Error()
	}
	described := &apiError{msg: fmt.Sprintf("bigquery: %s (%s)", msg, reasonOrCode(reason, code)), err: err}
	switch reason {
	case "invalidQuery", "invalid", "notFound", "duplicate", "invalidUser", "responseTooLarge":
		return fmt.Errorf("%w: %w", warehouse.ErrBadRequest, described)
	case "accessDenied", "bytesBilledLimitExceeded", "billingTierLimitExceeded":
		return fmt.Errorf("%w: %w", warehouse.ErrForbidden, described)
	case "timeout":
		return fmt.Errorf("%w: %w", warehouse.ErrTimeout, described)
	case "":
		switch code {
		case 400, 404:
			return fmt.Errorf("%w: %w", warehouse.ErrBadRequest, described)
		case 403:
			return fmt.Errorf("%w: %w", warehouse.ErrForbidden, described)
		}
	}
	return fmt.Errorf("%w: %w", warehouse.ErrUpstream, described)
}

// apiError renders a BigQuery error concisely while keeping the original
// error reachable through errors.As.
type apiError struct {
	msg string
	err error
}

func (e *apiError) Error() string { return e.msg }
func (e *apiError) Unwrap() error { return e.err }

func reasonOrCode(reason string, code int) string {
	if reason != "" {
		return reason
	}
	return fmt.Sprintf("HTTP %d", code)
}
