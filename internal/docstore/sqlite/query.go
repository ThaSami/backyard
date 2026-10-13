package sqlite

import (
	"math"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/docstore"
)

// This file translates a prepared docstore.Query into SQLite SQL with exactly
// the semantics of the reference evaluator (docstore/eval.go):
//
//   - json_type(data, path) is NULL for a missing field, 'null' for JSON null,
//     'true'/'false' for booleans, 'integer'/'real' for numbers, 'text' for
//     strings and 'array'/'object' for composites. Every comparison is guarded
//     by a json_type check so that only values of the filter's JSON type match.
//   - json_extract returns booleans as 0/1 integers, numbers as INTEGER/REAL
//     (compared numerically) and strings as TEXT (BINARY collation, i.e. byte
//     order).
//   - Ordering emits a type rank (missing/null 0, boolean 1, number 2, string 3,
//     array 4, object 5) followed by the scalar value, both in the requested
//     direction; arrays and objects tie.
//
// User input never becomes SQL text: field paths (already validated against
// ^[A-Za-z_][A-Za-z0-9_-]*$ segments) and values are bound as parameters.
// Only fixed operator tokens and column names from a closed set are inlined.

// systemColumns maps system fields to columns.
var systemColumns = map[string]string{
	"id":               "id",
	"_meta.created_at": "created_at",
	"_meta.updated_at": "updated_at",
	"_meta.created_by": "created_by",
	"_meta.updated_by": "updated_by",
	"_meta.version":    "version",
}

var sqlOps = map[docstore.Op]string{
	docstore.OpEq: "=", docstore.OpNe: "<>",
	docstore.OpLt: "<", docstore.OpLte: "<=", docstore.OpGt: ">", docstore.OpGte: ">=",
}

const falseSQL = "0"

type builder struct {
	sb   strings.Builder
	args []any
}

func (b *builder) w(s string)             { b.sb.WriteString(s) }
func (b *builder) arg(v ...any)           { b.args = append(b.args, v...) }
func (b *builder) sql(s string, a ...any) { b.w(s); b.arg(a...) }

// buildQuery returns the SELECT statement and its arguments. It fetches
// q.Limit+1 rows so the caller can report has_more.
func buildQuery(site, collection string, q docstore.Query) (string, []any) {
	b := &builder{}
	b.sql(`SELECT `+docColumns+` FROM docs WHERE site = ? AND collection = ?`, site, collection)
	for _, f := range q.Where {
		b.w(" AND ")
		b.filter(f)
	}
	b.w(" ORDER BY ")
	for i, o := range q.OrderBy {
		if i > 0 {
			b.w(", ")
		}
		b.order(o)
	}
	b.sql(" LIMIT ? OFFSET ?", q.Limit+1, q.Offset)
	return b.sb.String(), b.args
}

// jsonPath converts a validated dot path into an SQLite JSON path with quoted
// segments ($."a"."b-c").
func jsonPath(field string) string {
	var sb strings.Builder
	sb.WriteString("$")
	for seg := range strings.SplitSeq(field, ".") {
		sb.WriteString(`."`)
		sb.WriteString(seg)
		sb.WriteString(`"`)
	}
	return sb.String()
}

func (b *builder) filter(f docstore.Filter) {
	if col, ok := systemColumns[f.Field]; ok {
		b.systemFilter(col, f)
		return
	}
	path := jsonPath(f.Field)
	switch f.Op {
	case docstore.OpEq, docstore.OpNe, docstore.OpLt, docstore.OpLte, docstore.OpGt, docstore.OpGte:
		b.dataCompare(path, f.Field, f.Op, f.Value)
	case docstore.OpIn, docstore.OpNotIn:
		b.list(f, func(v any) { b.dataCompare(path, f.Field, elemOp(f.Op), v) })
	case docstore.OpContains:
		b.contains(path, f.Field, f.Value)
	default:
		b.w(falseSQL)
	}
}

// elemOp is the per-element operator of in (==) and not-in (!=).
func elemOp(op docstore.Op) docstore.Op {
	if op == docstore.OpNotIn {
		return docstore.OpNe
	}
	return docstore.OpEq
}

// list emits "in" as an OR of == and "not-in" as an AND of != comparisons.
func (b *builder) list(f docstore.Filter, emit func(v any)) {
	vals, _ := f.Value.([]any)
	if len(vals) == 0 {
		b.w(falseSQL)
		return
	}
	sep := " OR "
	if f.Op == docstore.OpNotIn {
		sep = " AND "
	}
	b.w("(")
	for i, v := range vals {
		if i > 0 {
			b.w(sep)
		}
		emit(v)
	}
	b.w(")")
}

// dataCompare emits a type-guarded comparison of a data field with a scalar.
func (b *builder) dataCompare(path, field string, op docstore.Op, v any) {
	val, kind := docstore.Operand(field, v)
	sqlOp := sqlOps[op]
	switch kind {
	case docstore.KindNull:
		// null equals only null; there is no order within null.
		switch op {
		case docstore.OpEq, docstore.OpLte, docstore.OpGte:
			b.sql(`json_type(data, ?) = 'null'`, path)
		default:
			b.w(falseSQL)
		}
	case docstore.KindBool:
		bv, _ := val.(bool)
		switch op {
		case docstore.OpEq:
			b.sql(`json_type(data, ?) = ?`, path, boolType(bv))
		case docstore.OpNe:
			b.sql(`json_type(data, ?) = ?`, path, boolType(!bv))
		default:
			b.sql(`(json_type(data, ?) IN ('true', 'false') AND json_extract(data, ?) `+sqlOp+` ?)`, path, path, boolInt(bv))
		}
	case docstore.KindNumber:
		b.sql(`(json_type(data, ?) IN ('integer', 'real') AND json_extract(data, ?) `+sqlOp+` ?)`, path, path, val)
	case docstore.KindString:
		b.sql(`(json_type(data, ?) = 'text' AND json_extract(data, ?) `+sqlOp+` ?)`, path, path, val)
	default:
		b.w(falseSQL)
	}
}

// contains emits "field is an array with an element equal to v".
func (b *builder) contains(path, field string, v any) {
	val, kind := docstore.Operand(field, v)
	var cond string
	var args []any
	switch kind {
	case docstore.KindNull:
		cond = `e.type = 'null'`
	case docstore.KindBool:
		bv, _ := val.(bool)
		cond, args = `e.type = ?`, []any{boolType(bv)}
	case docstore.KindNumber:
		cond, args = `e.type IN ('integer', 'real') AND e.value = ?`, []any{val}
	case docstore.KindString:
		cond, args = `e.type = 'text' AND e.value = ?`, []any{val}
	default:
		b.w(falseSQL)
		return
	}
	b.sql(`(json_type(data, ?) = 'array' AND EXISTS (SELECT 1 FROM json_each(docs.data, ?) AS e WHERE `+cond+`))`, path, path)
	b.arg(args...)
}

// systemFilter compares a system column. The operand must have the column's
// type (string, number or RFC 3339 timestamp), otherwise nothing matches.
func (b *builder) systemFilter(col string, f docstore.Filter) {
	colKind, _ := docstore.FieldKind(f.Field)
	cmp := func(op docstore.Op, v any) {
		val, kind := docstore.Operand(f.Field, v)
		if kind != colKind {
			b.w(falseSQL)
			return
		}
		if t, ok := val.(time.Time); ok {
			val = clampNanos(t)
		}
		b.sql(col+" "+sqlOps[op]+" ?", val)
	}
	switch f.Op {
	case docstore.OpEq, docstore.OpNe, docstore.OpLt, docstore.OpLte, docstore.OpGt, docstore.OpGte:
		cmp(f.Op, f.Value)
	case docstore.OpIn, docstore.OpNotIn:
		b.list(f, func(v any) { cmp(elemOp(f.Op), v) })
	default: // contains: system fields are never arrays
		b.w(falseSQL)
	}
}

func (b *builder) order(o docstore.Order) {
	dir := " ASC"
	if docstore.IsDesc(o) {
		dir = " DESC"
	}
	if col, ok := systemColumns[o.Field]; ok {
		b.w(col + dir)
		return
	}
	path := jsonPath(o.Field)
	b.sql(`CASE json_type(data, ?) WHEN 'true' THEN 1 WHEN 'false' THEN 1 WHEN 'integer' THEN 2 WHEN 'real' THEN 2 `+
		`WHEN 'text' THEN 3 WHEN 'array' THEN 4 WHEN 'object' THEN 5 ELSE 0 END`+dir, path)
	b.sql(`, CASE WHEN json_type(data, ?) IN ('true', 'false', 'integer', 'real', 'text') THEN json_extract(data, ?) END`+dir, path, path)
}

func boolType(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

var (
	minNanoTime = time.Unix(0, math.MinInt64)
	maxNanoTime = time.Unix(0, math.MaxInt64)
)

// clampNanos converts t to unix nanoseconds, saturating outside the int64
// range (years 1678-2262) so that comparisons stay correct.
func clampNanos(t time.Time) int64 {
	switch {
	case t.Before(minNanoTime):
		return math.MinInt64
	case t.After(maxNanoTime):
		return math.MaxInt64
	}
	return t.UnixNano()
}
