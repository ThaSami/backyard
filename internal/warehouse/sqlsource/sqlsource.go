// Package sqlsource implements warehouse.Source for PostgreSQL and
// Postgres-compatible engines (Amazon Redshift, AlloyDB, Aurora, Neon,
// CockroachDB, …) on a pgx/v5 connection pool.
//
// Every query runs in its own READ ONLY transaction that is always rolled
// back, with statement_timeout derived from the context deadline. Queries use
// the extended protocol (a single statement per query, never the simple
// protocol, whose multi-statement strings could COMMIT the read-only
// transaction), with "$1"-style placeholders. Unless the DSN sets
// default_query_exec_mode, statements are described on every execution
// rather than cached as server-side prepared statements, which suits ad-hoc
// analytics SQL and transaction-pooling proxies (PgBouncer, RDS Proxy).
// Parameters are sent in text format, so PostgreSQL parses them for the
// inferred parameter type exactly as it would parse a quoted literal.
//
// When a result exceeds the row cap, up to 10 000 surplus rows (and at most
// the size cap's worth of data) are discarded and larger results are
// canceled server-side rather than streamed. A result that reaches the size
// cap (warehouse.Limits.MaxBytes) is canceled at once. pgx reads every
// protocol message whole, so the connection refuses messages larger than
// what is left of the size cap (at least 64 KiB): such a row ends the result
// (Truncated) and the connection is closed instead of the row being
// buffered. When that happens to the first row, the result has no columns
// either (pgx learns them with the first row).
//
// Result values are converted to JSON-friendly Go values:
//
//	int2/int4/int8/oid          → int64
//	float4/float8               → float64 ("NaN"/"Infinity"/"-Infinity" strings)
//	numeric                     → float64 when lossless, else the exact decimal string
//	text/varchar/char/name/enum → string
//	bool                        → bool
//	timestamptz/timestamp       → RFC 3339 string with nanoseconds, UTC ("infinity" stays a string)
//	date                        → "YYYY-MM-DD"
//	uuid                        → canonical string
//	json/jsonb                  → decoded value
//	bytea                       → base64 string
//	time                        → "HH:MM:SS[.ffffff]"
//	interval                    → ISO 8601 duration as PostgreSQL's iso_8601 style prints it ("P1DT2H")
//	arrays                      → []any (nested for multi-dimensional arrays)
//	anything else               → PostgreSQL's text representation (arrays of such types → []any of strings)
//
// Use a database role with read-only privileges: READ ONLY transactions do
// not stop side effects of functions such as dblink_exec or pg_terminate_backend.
package sqlsource

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/warehouse"
	"github.com/thasami/backyard/internal/warehouse/internal/conv"
)

const (
	// drainLimit is how many surplus rows are read (and discarded) after
	// the row cap before the server is asked to cancel the statement. Small
	// overshoots finish cleanly; huge results are canceled.
	drainLimit = 10000
	// minBodyLen is the smallest protocol message size limit set on a
	// connection, so ordinary rows, errors and notices are always read
	// even when little of the size cap is left.
	minBodyLen = 64 << 10
	// rollbackTimeout bounds the ROLLBACK issued after every query.
	rollbackTimeout = 5 * time.Second
	// cancelDeadlineDelay is how long a canceled query may take to stop
	// after the cancel request before its connection is closed.
	cancelDeadlineDelay = 5 * time.Second
	// applicationName is reported in pg_stat_activity unless the DSN sets one.
	applicationName = "backyard-warehouse"
)

// Options are optional settings.
type Options struct {
	// Logger receives warnings (default slog.Default()).
	Logger *slog.Logger
	// ConfigurePool, if set, may adjust the pool configuration parsed from
	// the DSN before the pool is created (pool sizes, TLS, hooks).
	ConfigurePool func(*pgxpool.Config)
}

// Source is a warehouse.Source backed by a pgx pool. It is safe for
// concurrent use.
type Source struct {
	name string
	pool *pgxpool.Pool
	log  *slog.Logger

	mu        sync.RWMutex
	typeNames map[uint32]string // names of OIDs unknown to pgtype, from pg_type

	formatsOnce sync.Once
	formats     pgx.QueryResultFormatsByOID
}

var _ warehouse.Source = (*Source)(nil)

// New creates a pool for src.DSN. No connection is made until the first query.
func New(ctx context.Context, name string, src config.WarehouseSource, opts Options) (*Source, error) {
	if src.DSN == "" {
		return nil, fmt.Errorf("sqlsource %s: dsn is required", name)
	}
	pcfg, err := pgxpool.ParseConfig(src.DSN)
	if err != nil {
		// pgconn redacts the password in parse errors.
		return nil, fmt.Errorf("sqlsource %s: parse dsn: %w", name, err)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	cc := pcfg.ConnConfig
	if !strings.Contains(src.DSN, "default_query_exec_mode") {
		cc.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	}
	if cc.RuntimeParams == nil {
		cc.RuntimeParams = map[string]string{}
	}
	if cc.RuntimeParams["application_name"] == "" {
		cc.RuntimeParams["application_name"] = applicationName
	}
	// Cancel queries server-side when their context ends (client gone,
	// timeout, row cap) instead of closing the connection.
	cc.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: cancelDeadlineDelay}
	}
	if opts.ConfigurePool != nil {
		opts.ConfigurePool(pcfg)
	}
	if pcfg.ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeSimpleProtocol {
		log.Warn("sqlsource: simple_protocol is not allowed for warehouse sources; using describe_exec", "source", name)
		pcfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	}
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		return nil, fmt.Errorf("sqlsource %s: create pool: %w", name, err)
	}
	return &Source{name: name, pool: pool, log: log, typeNames: map[uint32]string{}}, nil
}

// Placeholder returns "$<i+1>".
func (s *Source) Placeholder(i int) string { return "$" + strconv.Itoa(i+1) }

// Close closes the pool, waiting for in-flight queries to release their
// connections.
func (s *Source) Close() error {
	s.pool.Close()
	return nil
}

// Query runs sql in a READ ONLY transaction and returns the rows that fit
// within lim.
func (s *Source) Query(ctx context.Context, sql string, args []any, lim warehouse.Limits) (*warehouse.Result, error) {
	lim = lim.WithDefaults()
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, classify(ctx, fmt.Errorf("acquire connection: %w", err))
	}
	defer conn.Release()

	// pgx buffers each protocol message whole (by default up to 1 GiB);
	// no message may be larger than the result is allowed to be.
	fe := conn.Conn().PgConn().Frontend()
	fe.SetMaxBodyLen(bodyLimit(lim.MaxBytes))
	res, fds, err := s.run(ctx, conn, sql, args, lim)
	if err != nil {
		return nil, err
	}
	fe.SetMaxBodyLen(bodyLimit(lim.MaxBytes))
	// Types unknown to pgtype (enums, extension types) are named from
	// pg_type on the same connection, after the read-only transaction.
	var unknown []int
	for i, c := range res.Columns {
		if c.Type == unknownType {
			unknown = append(unknown, i)
		}
	}
	if len(unknown) > 0 {
		s.resolveTypeNames(ctx, conn, res, fds, unknown)
	}
	return res, nil
}

// bodyLimit is the protocol message size limit while budget bytes of the
// size cap are left.
func bodyLimit(budget int) int { return max(budget, minBodyLen) }

// run executes sql inside a READ ONLY transaction that is always rolled back.
func (s *Source) run(ctx context.Context, conn *pgxpool.Conn, sql string, args []any, lim warehouse.Limits) (*warehouse.Result, []pgconn.FieldDescription, error) {
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, classify(ctx, fmt.Errorf("begin read-only transaction: %w", err))
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		// A failed rollback makes pgx close the connection, so the pool
		// never reuses a connection in an unknown state.
		_ = tx.Rollback(rctx)
	}()

	// The statement timeout is set with a SELECT (one round trip, simple
	// protocol for this fixed internal statement). Running a query first
	// also takes the transaction snapshot, after which PostgreSQL refuses
	// to switch the transaction to READ WRITE.
	setup := "SELECT current_setting('statement_timeout')"
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		// Leave a margin so the server's timeout (which keeps the
		// connection healthy) fires before the context's.
		remaining -= min(remaining/10, time.Second)
		ms := max(remaining.Milliseconds(), 1)
		setup = "SELECT set_config('statement_timeout', '" + strconv.FormatInt(ms, 10) + "', true)"
	}
	if _, err := tx.Exec(ctx, setup, pgx.QueryExecModeSimpleProtocol); err != nil {
		return nil, nil, classify(ctx, fmt.Errorf("set statement_timeout: %w", err))
	}

	qctx, cancelQuery := context.WithCancel(ctx)
	defer cancelQuery()
	tm := conn.Conn().TypeMap()
	qargs := make([]any, 0, len(args)+1)
	qargs = append(qargs, s.resultFormats(tm))
	for i, a := range args {
		v, err := textArg(a)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: params[%d]: %w", warehouse.ErrBadRequest, i, err)
		}
		qargs = append(qargs, v)
	}
	rows, err := tx.Query(qctx, sql, qargs...)
	if err != nil {
		if tooLarge(err) {
			// Refused before the first row could be read (see bodyLimit).
			return &warehouse.Result{Columns: []warehouse.Column{}, Rows: []map[string]any{}, Truncated: true}, nil, nil
		}
		return nil, nil, classify(ctx, err)
	}
	defer rows.Close()

	fds := append([]pgconn.FieldDescription(nil), rows.FieldDescriptions()...)
	cols := make([]warehouse.Column, len(fds))
	names := make([]string, len(fds))
	for i, fd := range fds {
		names[i] = fd.Name
		cols[i].Type = typeName(tm, fd.DataTypeOID)
	}
	keys := conv.UniqueNames(names)
	for i := range cols {
		cols[i].Name = keys[i]
	}

	fe := conn.Conn().PgConn().Frontend()
	budget := conv.NewBudget(lim.MaxBytes)
	out := make([]map[string]any, 0, min(lim.MaxRows, 1024))
	truncated, full := false, false
	for {
		// A row larger than what is left of the budget is refused by pgx
		// (which closes the connection) instead of being read.
		fe.SetMaxBodyLen(bodyLimit(budget.Remaining()))
		if !rows.Next() {
			break
		}
		if len(out) == lim.MaxRows {
			truncated = true
			break
		}
		row, err := convertRow(tm, fds, keys, rows)
		if err != nil {
			// Cancel before closing: rows.Close would otherwise read the
			// rest of a possibly huge result.
			cancelQuery()
			rows.Close()
			return nil, nil, fmt.Errorf("%w: %s: %w", warehouse.ErrUpstream, s.name, err)
		}
		if !budget.Add(row) {
			truncated, full = true, true
			break
		}
		out = append(out, row)
	}
	switch {
	case full:
		// The rows are large: stop the statement rather than read on.
		cancelQuery()
		rows.Close()
	case truncated:
		// Discard a bounded number of surplus rows; if the result is
		// larger, cancel the statement rather than stream it all.
		if !drain(rows, drainLimit, lim.MaxBytes) {
			cancelQuery()
		}
		rows.Close()
	default:
		rows.Close()
		if err := rows.Err(); err != nil {
			if !tooLarge(err) {
				return nil, nil, classify(ctx, err)
			}
			// The next row did not fit in the budget; pgx refused it
			// and closed the connection.
			truncated = true
		}
	}
	return &warehouse.Result{Columns: cols, Rows: out, RowCount: len(out), Truncated: truncated}, fds, nil
}

// drain reads and discards up to maxRows rows holding at most about maxBytes
// of data, reporting whether the result ended within those bounds.
func drain(rows pgx.Rows, maxRows, maxBytes int) bool {
	n, size := 0, 0
	for n < maxRows && size < maxBytes {
		if !rows.Next() {
			return true
		}
		n++
		for _, v := range rows.RawValues() {
			size += len(v)
		}
	}
	return false
}

// tooLarge reports whether err is pgx refusing a protocol message larger
// than the connection's limit (see bodyLimit).
func tooLarge(err error) bool {
	var e *pgproto3.ExceededMaxBodyLenErr
	return errors.As(err, &e)
}

// convertRow converts the current row into a map keyed by column name.
func convertRow(tm *pgtype.Map, fds []pgconn.FieldDescription, keys []string, rows pgx.Rows) (map[string]any, error) {
	raw := rows.RawValues()
	row := make(map[string]any, len(fds))
	for i, fd := range fds {
		v, err := convertColumn(tm, fd, raw[i])
		if err != nil {
			return nil, fmt.Errorf("decode column %q: %w", fd.Name, err)
		}
		row[keys[i]] = v
	}
	return row, nil
}

// convertColumn decodes one raw column value and converts it to a
// JSON-friendly value. Natively handled types arrive in pgx's preferred
// format and are decoded by pgtype; all other types arrive in text format
// and are returned as PostgreSQL prints them (arrays as []any of strings).
func convertColumn(tm *pgtype.Map, fd pgconn.FieldDescription, raw []byte) (any, error) {
	if raw == nil {
		return nil, nil
	}
	oid := fd.DataTypeOID
	t, ok := tm.TypeForOID(oid)
	if !ok || (fd.Format == pgtype.TextFormatCode && !isNative(tm, oid)) {
		if fd.Format != pgtype.TextFormatCode {
			return conv.Bytes(raw), nil
		}
		str := string(raw)
		if ok {
			if _, isArray := t.Codec.(*pgtype.ArrayCodec); isArray {
				if arr, ok := parseTextArray(str); ok {
					return arr, nil
				}
			}
		}
		return str, nil
	}
	if ac, ok := t.Codec.(*pgtype.ArrayCodec); ok && ac.ElementType != nil {
		return convertArray(tm, oid, ac.ElementType.OID, fd.Format, raw)
	}
	v, err := t.Codec.DecodeValue(tm, oid, fd.Format, raw)
	if err != nil {
		return nil, err
	}
	return convertValue(tm, oid, v), nil
}

// nativeOIDs are the scalar types converted from pgtype's decoded values.
var nativeOIDs = map[uint32]bool{
	pgtype.BoolOID: true, pgtype.Int2OID: true, pgtype.Int4OID: true, pgtype.Int8OID: true,
	pgtype.OIDOID: true, pgtype.Float4OID: true, pgtype.Float8OID: true, pgtype.NumericOID: true,
	pgtype.TextOID: true, pgtype.VarcharOID: true, pgtype.BPCharOID: true, pgtype.NameOID: true,
	pgtype.QCharOID: true, pgtype.UUIDOID: true, pgtype.JSONOID: true, pgtype.JSONBOID: true,
	pgtype.ByteaOID: true, pgtype.DateOID: true, pgtype.TimestampOID: true,
	pgtype.TimestamptzOID: true, pgtype.IntervalOID: true, pgtype.TimeOID: true,
}

// isNative reports whether oid is a native scalar or an array of one.
func isNative(tm *pgtype.Map, oid uint32) bool {
	if nativeOIDs[oid] {
		return true
	}
	t, ok := tm.TypeForOID(oid)
	if !ok {
		return false
	}
	ac, ok := t.Codec.(*pgtype.ArrayCodec)
	return ok && ac.ElementType != nil && nativeOIDs[ac.ElementType.OID]
}

// resultFormats returns the per-OID result formats: pgx's preferred format
// for native types (and arrays of them), text for everything else (pgx
// treats OIDs missing from the map as text).
func (s *Source) resultFormats(tm *pgtype.Map) pgx.QueryResultFormatsByOID {
	s.formatsOnce.Do(func() {
		m := pgx.QueryResultFormatsByOID{}
		// Built-in type OIDs are below FirstNormalObjectId (16384).
		for oid := uint32(1); oid < 16384; oid++ {
			if isNative(tm, oid) {
				m[oid] = tm.FormatCodeForOID(oid)
			}
		}
		s.formats = m
	})
	return s.formats
}

// textArg converts a normalized JSON scalar into a query argument. Numbers
// and booleans are sent as text so PostgreSQL parses them for the inferred
// parameter type exactly like a literal (1.5 for an integer parameter is an
// error, not silently rounded; numerics keep their decimal digits).
func textArg(v any) (any, error) {
	switch x := v.(type) {
	case nil, string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case int:
		return strconv.Itoa(x), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("number %v is not finite", x)
		}
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case json.Number:
		return x.String(), nil
	}
	return nil, fmt.Errorf("unsupported parameter type %T", v)
}

// convertArray decodes an array value and nests it by dimension.
func convertArray(tm *pgtype.Map, oid, elem uint32, format int16, raw []byte) (any, error) {
	var arr pgtype.Array[any]
	if err := tm.Scan(oid, format, raw, &arr); err != nil {
		return nil, err
	}
	items := make([]any, len(arr.Elements))
	for i, e := range arr.Elements {
		items[i] = convertValue(tm, elem, e)
	}
	if len(arr.Dims) <= 1 {
		return items, nil
	}
	return nest(items, arr.Dims), nil
}

// nest reshapes the flat, row-major elements of a multi-dimensional array.
func nest(items []any, dims []pgtype.ArrayDimension) []any {
	if len(dims) == 1 {
		return items
	}
	n := int(dims[0].Length)
	if n == 0 {
		return []any{}
	}
	size := len(items) / n
	out := make([]any, n)
	for i := range n {
		out[i] = nest(items[i*size:(i+1)*size], dims[1:])
	}
	return out
}

// convertValue converts a value decoded by pgtype into a JSON-friendly one.
func convertValue(tm *pgtype.Map, oid uint32, v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string, bool, int64:
		return x
	case int16:
		return int64(x)
	case int32:
		if oid == pgtype.QCharOID {
			return string(x) // "char" decodes as a rune (int32)
		}
		return int64(x)
	case int8:
		return int64(x)
	case int:
		return int64(x)
	case uint32:
		return int64(x)
	case float64:
		return conv.Float(x)
	case float32:
		return conv.Float32(x)
	case pgtype.Numeric:
		return numericValue(x)
	case time.Time:
		if oid == pgtype.DateOID {
			return conv.Date(x)
		}
		return conv.Timestamp(x)
	case pgtype.InfinityModifier:
		return x.String()
	case [16]byte:
		if oid == pgtype.UUIDOID {
			return uuidString(x)
		}
	case []byte:
		return conv.Bytes(x)
	case pgtype.Interval:
		if x.Valid {
			return isoInterval(x)
		}
		return nil
	case pgtype.Time:
		if x.Valid {
			return timeOfDay(x.Microseconds)
		}
		return nil
	case map[string]any, []any:
		if oid == pgtype.JSONOID || oid == pgtype.JSONBOID {
			return x
		}
		if list, ok := x.([]any); ok { // record
			items := make([]any, len(list))
			for i, e := range list {
				items[i] = convertValue(tm, 0, e)
			}
			return items
		}
		return x
	}
	if oid == pgtype.JSONOID || oid == pgtype.JSONBOID {
		return v // decoded JSON scalar (float64, string, bool)
	}
	return textValue(tm, oid, v)
}

// textValue renders v in PostgreSQL's text format via pgtype, falling back
// to fmt for values pgtype cannot encode.
func textValue(tm *pgtype.Map, oid uint32, v any) any {
	if oid != 0 {
		if buf, err := tm.Encode(oid, pgtype.TextFormatCode, v, nil); err == nil {
			if buf == nil {
				return nil
			}
			return string(buf)
		}
	}
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}
	return fmt.Sprint(v)
}

// numericValue renders a numeric as float64 when lossless, else as the exact
// decimal string.
func numericValue(n pgtype.Numeric) any {
	switch {
	case !n.Valid:
		return nil
	case n.NaN:
		return "NaN"
	case n.InfinityModifier == pgtype.Infinity:
		return "Infinity"
	case n.InfinityModifier == pgtype.NegativeInfinity:
		return "-Infinity"
	case n.Int == nil:
		return int64(0)
	}
	return conv.Decimal(numericString(n))
}

// numericString renders Int×10^Exp as a plain decimal string.
func numericString(n pgtype.Numeric) string {
	digits := n.Int.String()
	neg := strings.HasPrefix(digits, "-")
	digits = strings.TrimPrefix(digits, "-")
	switch {
	case n.Exp > 0:
		digits += strings.Repeat("0", int(n.Exp))
	case n.Exp < 0:
		scale := int(-n.Exp)
		if len(digits) <= scale {
			digits = strings.Repeat("0", scale-len(digits)+1) + digits
		}
		digits = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if neg {
		return "-" + digits
	}
	return digits
}

func uuidString(u [16]byte) string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:], u[10:])
	return string(b[:])
}

// isoInterval formats an interval like PostgreSQL's IntervalStyle iso_8601.
func isoInterval(iv pgtype.Interval) string {
	years, months := iv.Months/12, iv.Months%12
	days := iv.Days
	us := iv.Microseconds
	hours := us / 3_600_000_000
	us -= hours * 3_600_000_000
	minutes := us / 60_000_000
	us -= minutes * 60_000_000
	secs := us / 1_000_000
	frac := us - secs*1_000_000

	if years == 0 && months == 0 && days == 0 && hours == 0 && minutes == 0 && secs == 0 && frac == 0 {
		return "PT0S"
	}
	var b strings.Builder
	b.WriteByte('P')
	part := func(v int64, unit byte) {
		if v != 0 {
			b.WriteString(strconv.FormatInt(v, 10))
			b.WriteByte(unit)
		}
	}
	part(int64(years), 'Y')
	part(int64(months), 'M')
	part(int64(days), 'D')
	if hours != 0 || minutes != 0 || secs != 0 || frac != 0 {
		b.WriteByte('T')
	}
	part(hours, 'H')
	part(minutes, 'M')
	if secs != 0 || frac != 0 {
		if secs < 0 || frac < 0 {
			b.WriteByte('-')
		}
		b.WriteString(strconv.FormatInt(abs(secs), 10))
		if frac != 0 {
			f := fmt.Sprintf("%06d", abs(frac))
			b.WriteByte('.')
			b.WriteString(strings.TrimRight(f, "0"))
		}
		b.WriteByte('S')
	}
	return b.String()
}

// timeOfDay formats microseconds since midnight as PostgreSQL prints a time:
// "HH:MM:SS" with a fractional part only when non-zero.
func timeOfDay(us int64) string {
	h := us / 3_600_000_000
	us -= h * 3_600_000_000
	m := us / 60_000_000
	us -= m * 60_000_000
	sec := us / 1_000_000
	frac := us - sec*1_000_000
	out := fmt.Sprintf("%02d:%02d:%02d", h, m, sec)
	if frac != 0 {
		out += "." + strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
	}
	return out
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// unknownType is the column type reported for OIDs that cannot be named.
const unknownType = "unknown"

// typeName returns the display name of oid ("int4", "text[]"), or
// unknownType if pgtype does not know it.
func typeName(tm *pgtype.Map, oid uint32) string {
	t, ok := tm.TypeForOID(oid)
	if !ok {
		return unknownType
	}
	if ac, ok := t.Codec.(*pgtype.ArrayCodec); ok && ac.ElementType != nil {
		if name := typeName(tm, ac.ElementType.OID); name != unknownType {
			return name + "[]"
		}
	}
	return t.Name
}

// resolveTypeNames names the columns at idx (OIDs unknown to pgtype: enums,
// extension types, arrays of those) from pg_type, caching the names, and
// decodes the text-format values of array columns into []any. Lookup
// failures leave the type "unknown" and the values as strings.
func (s *Source) resolveTypeNames(ctx context.Context, conn *pgxpool.Conn, res *warehouse.Result, fds []pgconn.FieldDescription, idx []int) {
	var missing []any
	s.mu.RLock()
	for _, i := range idx {
		if _, ok := s.typeNames[fds[i].DataTypeOID]; !ok {
			missing = append(missing, fds[i].DataTypeOID)
		}
	}
	s.mu.RUnlock()
	if len(missing) > 0 && ctx.Err() == nil {
		if err := s.lookupTypes(ctx, conn, missing); err != nil {
			s.log.Debug("sqlsource: resolve type names", "source", s.name, "error", err)
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, i := range idx {
		name, ok := s.typeNames[fds[i].DataTypeOID]
		if !ok {
			continue
		}
		res.Columns[i].Type = name
		if !strings.HasSuffix(name, "[]") || fds[i].Format != pgtype.TextFormatCode {
			continue
		}
		key := res.Columns[i].Name
		for _, row := range res.Rows {
			if str, ok := row[key].(string); ok {
				if arr, ok := parseTextArray(str); ok {
					row[key] = arr
				}
			}
		}
	}
}

func (s *Source) lookupTypes(ctx context.Context, conn *pgxpool.Conn, oids []any) error {
	lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ph := make([]string, len(oids))
	for i := range oids {
		ph[i] = "$" + strconv.Itoa(i+1)
	}
	rows, err := conn.Query(lctx, "SELECT oid, typname FROM pg_catalog.pg_type WHERE oid IN ("+strings.Join(ph, ",")+")", oids...)
	if err != nil {
		return err
	}
	found := map[uint32]string{}
	var (
		oid  uint32
		name string
	)
	if _, err := pgx.ForEachRow(rows, []any{&oid, &name}, func() error {
		if strings.HasPrefix(name, "_") {
			name = name[1:] + "[]"
		}
		found[oid] = name
		return nil
	}); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range found {
		s.typeNames[k] = v
	}
	return nil
}

// parseTextArray parses PostgreSQL's text representation of an array
// ("{a,\"b c\",NULL,{d}}", optionally prefixed by bounds "[0:1]=") into
// nested []any of strings and nils.
func parseTextArray(s string) ([]any, bool) {
	if strings.HasPrefix(s, "[") {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, false
		}
		s = s[eq+1:]
	}
	p := &arrayParser{s: s}
	if !p.consume('{') {
		return nil, false
	}
	out, ok := p.list()
	if !ok {
		return nil, false
	}
	p.skipSpace()
	return out, p.i == len(p.s)
}

type arrayParser struct {
	s string
	i int
}

func (p *arrayParser) skipSpace() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *arrayParser) consume(c byte) bool {
	p.skipSpace()
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

// list parses elements after an opening '{' up to the matching '}'.
func (p *arrayParser) list() ([]any, bool) {
	out := []any{}
	if p.consume('}') {
		return out, true
	}
	for {
		p.skipSpace()
		if p.i >= len(p.s) {
			return nil, false
		}
		switch p.s[p.i] {
		case '{':
			p.i++
			sub, ok := p.list()
			if !ok {
				return nil, false
			}
			out = append(out, sub)
		case '"':
			p.i++
			var b strings.Builder
			for {
				if p.i >= len(p.s) {
					return nil, false
				}
				c := p.s[p.i]
				p.i++
				if c == '\\' && p.i < len(p.s) {
					b.WriteByte(p.s[p.i])
					p.i++
					continue
				}
				if c == '"' {
					break
				}
				b.WriteByte(c)
			}
			out = append(out, b.String())
		default:
			start := p.i
			for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != '}' {
				p.i++
			}
			v := strings.TrimSpace(p.s[start:p.i])
			if strings.EqualFold(v, "NULL") {
				out = append(out, nil)
			} else {
				out = append(out, v)
			}
		}
		if p.consume(',') {
			continue
		}
		if p.consume('}') {
			return out, true
		}
		return nil, false
	}
}

// classify wraps err with the warehouse sentinel matching its cause.
func classify(ctx context.Context, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		code := pgErr.Code
		switch {
		case code == "57014" && strings.Contains(pgErr.Message, "statement timeout"):
			return fmt.Errorf("%w: %w", warehouse.ErrTimeout, err)
		case code == "57014" && ctx.Err() != nil:
			return contextErr(ctx, err)
		case code == "25006" || code == "42501":
			// read_only_sql_transaction, insufficient_privilege
			return fmt.Errorf("%w: %w", warehouse.ErrForbidden, err)
		case strings.HasPrefix(code, "42"), strings.HasPrefix(code, "22"),
			strings.HasPrefix(code, "21"), strings.HasPrefix(code, "0A"),
			strings.HasPrefix(code, "2F"), strings.HasPrefix(code, "38"),
			strings.HasPrefix(code, "39"), strings.HasPrefix(code, "P0"),
			code == "54001", code == "54023":
			// syntax/semantic errors, data exceptions, cardinality,
			// unsupported features, routine exceptions, statement too complex
			return fmt.Errorf("%w: %w", warehouse.ErrBadRequest, err)
		}
		return fmt.Errorf("%w: %w", warehouse.ErrUpstream, err)
	}
	if ctx.Err() != nil {
		return contextErr(ctx, err)
	}
	if strings.Contains(err.Error(), "failed to encode args[") || strings.Contains(err.Error(), "expected ") && strings.Contains(err.Error(), " arguments, got ") {
		return fmt.Errorf("%w: %w", warehouse.ErrBadRequest, err)
	}
	return fmt.Errorf("%w: %w", warehouse.ErrUpstream, err)
}

func contextErr(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", warehouse.ErrTimeout, errors.Join(ctx.Err(), err))
	}
	return errors.Join(ctx.Err(), err)
}
