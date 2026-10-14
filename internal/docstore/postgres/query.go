package postgres

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/docstore"
)

// This file translates a prepared docstore.Query into PostgreSQL with exactly
// the semantics of the reference evaluator (docstore/eval.go):
//
//   - data #> path (text[] parameter) is SQL NULL for a missing field;
//     jsonb_typeof() is 'null', 'boolean', 'number', 'string', 'array' or
//     'object'. Every comparison is guarded by a jsonb_typeof check so only
//     values of the filter's JSON type match.
//   - Numbers and booleans compare as jsonb (numeric order; false < true).
//     Strings compare as text with COLLATE "C" (byte order), never with the
//     database's default collation.
//   - contains uses jsonb containment of a one-element array (top-level
//     element match, type-strict).
//   - Ordering emits a type rank (missing/null 0, boolean 1, number 2,
//     string 3, array 4, object 5) followed by the scalar value; arrays and
//     objects tie.
//
// Field paths and values are always bound parameters; only fixed operator
// tokens and column names from a closed set are inlined.

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

const falseSQL = "FALSE"

type builder struct {
	sb   strings.Builder
	args []any
	err  error
}

func (b *builder) w(s string) { b.sb.WriteString(s) }

// p binds v and returns its placeholder.
func (b *builder) p(v any) string {
	b.args = append(b.args, v)
	return "$" + strconv.Itoa(len(b.args))
}

// buildQuery returns the SELECT statement and its arguments. It fetches
// q.Limit+1 rows so the caller can report has_more.
func buildQuery(table, site, collection string, q docstore.Query) (string, []any, error) {
	b := &builder{}
	b.w(`SELECT ` + docColumns + ` FROM ` + table + ` WHERE site = ` + b.p(site) + ` AND collection = ` + b.p(collection))
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
	b.w(" LIMIT " + b.p(q.Limit+1) + " OFFSET " + b.p(q.Offset))
	if b.err != nil {
		return "", nil, b.err
	}
	return b.sb.String(), b.args, nil
}

// path renders expressions for a validated dot path into data. The path is
// bound as a text[] parameter on first use only: PostgreSQL rejects
// parameters it cannot type, so a filter that compiles to FALSE must not bind
// one.
type path struct {
	b     *builder
	field string
	ph    string
}

func (b *builder) path(field string) *path { return &path{b: b, field: field} }

func (p *path) param() string {
	if p.ph == "" {
		p.ph = p.b.p(strings.Split(p.field, ".")) + "::text[]"
	}
	return p.ph
}

// val is the jsonb value (SQL NULL when missing).
func (p *path) val() string { return "(data #> " + p.param() + ")" }

// text is the value as text (unquoted for strings).
func (p *path) text() string { return "(data #>> " + p.param() + ")" }

// typ is the jsonb_typeof of the value (SQL NULL when missing).
func (p *path) typ() string { return "jsonb_typeof(data #> " + p.param() + ")" }

func (b *builder) jsonArg(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		b.err = fmt.Errorf("%w: filter value: %v", docstore.ErrInvalid, err)
	}
	return b.p(string(raw)) + "::jsonb"
}

func (b *builder) filter(f docstore.Filter) {
	if col, ok := systemColumns[f.Field]; ok {
		b.systemFilter(col, f)
		return
	}
	pa := b.path(f.Field)
	switch f.Op {
	case docstore.OpEq, docstore.OpNe, docstore.OpLt, docstore.OpLte, docstore.OpGt, docstore.OpGte:
		b.dataCompare(pa, f.Op, f.Value)
	case docstore.OpIn, docstore.OpNotIn:
		b.list(f, func(v any) { b.dataCompare(pa, elemOp(f.Op), v) })
	case docstore.OpContains:
		operand, kind := docstore.Operand(f.Field, f.Value)
		switch kind {
		case docstore.KindNull, docstore.KindBool, docstore.KindNumber, docstore.KindString:
			b.w("(" + pa.typ() + " = 'array' AND " + pa.val() + " @> " + b.jsonArg([]any{operand}) + ")")
		default:
			b.w(falseSQL)
		}
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
func (b *builder) dataCompare(pa *path, op docstore.Op, v any) {
	operand, kind := docstore.Operand(pa.field, v)
	sqlOp := sqlOps[op]
	switch kind {
	case docstore.KindNull:
		switch op {
		case docstore.OpEq, docstore.OpLte, docstore.OpGte:
			b.w(pa.typ() + " = 'null'")
		default:
			b.w(falseSQL)
		}
	case docstore.KindBool:
		b.w("(" + pa.typ() + " = 'boolean' AND " + pa.val() + " " + sqlOp + " " + b.jsonArg(operand) + ")")
	case docstore.KindNumber:
		b.w("(" + pa.typ() + " = 'number' AND " + pa.val() + " " + sqlOp + " " + b.jsonArg(operand) + ")")
	case docstore.KindString:
		b.w("(" + pa.typ() + " = 'string' AND " + pa.text() + ` COLLATE "C" ` + sqlOp + " " + b.p(operand) + "::text)")
	default:
		b.w(falseSQL)
	}
}

// systemFilter compares a system column. The operand must have the column's
// type (string, number or RFC 3339 timestamp), otherwise nothing matches.
func (b *builder) systemFilter(col string, f docstore.Filter) {
	colKind, _ := docstore.FieldKind(f.Field)
	cmp := func(op docstore.Op, v any) {
		operand, kind := docstore.Operand(f.Field, v)
		if kind != colKind {
			b.w(falseSQL)
			return
		}
		var ph string
		switch x := operand.(type) {
		case time.Time:
			ph = b.p(x) + "::timestamptz"
		case float64:
			ph = b.p(x) + "::float8"
		default:
			ph = b.p(operand) + "::text"
		}
		b.w(col + " " + sqlOps[op] + " " + ph)
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
	pa := b.path(o.Field)
	b.w("CASE " + pa.typ() + " WHEN 'boolean' THEN 1 WHEN 'number' THEN 2 WHEN 'string' THEN 3 " +
		"WHEN 'array' THEN 4 WHEN 'object' THEN 5 ELSE 0 END" + dir)
	b.w(", CASE WHEN " + pa.typ() + " IN ('boolean', 'number') THEN " + pa.val() + " END" + dir)
	b.w(", (CASE WHEN " + pa.typ() + " = 'string' THEN " + pa.text() + ` END) COLLATE "C"` + dir)
}
