package docstore

import (
	"cmp"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// This file is the reference implementation of the query semantics of
// SPEC §5.2. The memory and dynamodb backends evaluate queries with it
// directly; the SQL backends translate the same rules into SQL and are held to
// identical results by the conformance suite.
//
// Rules:
//
//   - A comparison only matches values of the same JSON type: "3" never equals
//     3, true never equals 1, and an ordering operator on a string field with a
//     number value matches nothing. This applies to every operator, including
//     "!=" (so "!= null" matches nothing) and "not-in".
//   - A missing field never matches any operator. "== null" matches an explicit
//     null only.
//   - "in" matches when the field equals any of the values; "not-in" matches
//     when the field is != every value (and hence has the same type as each).
//   - "contains" matches when the field is an array with an element of the
//     same type equal to the value.
//   - "<=" and ">=" with a null value match an explicit null (null equals
//     null); "<" and ">" with null match nothing.
//   - The timestamp system fields compare chronologically against RFC 3339
//     strings (see ParseTime); other strings compare by byte order.
//   - Ordering: values are ranked missing/null < boolean < number < string <
//     array < object (cross-type order is unspecified by the spec but identical
//     across Backyard backends), then within a type numbers compare
//     numerically, strings by bytes, false < true; arrays and objects tie.
//     Descending order reverses the whole comparison, so missing/null sort
//     first ascending and last descending. Remaining ties are broken by id
//     ascending.

// Kind is the JSON type of a value as seen by the query engine.
type Kind uint8

// Value kinds. KindMissing marks an absent field; KindTime is used for the
// timestamp system fields; KindInvalid marks a filter value that can never
// match (for example an unparseable timestamp).
const (
	KindMissing Kind = iota
	KindNull
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
	KindTime
	KindInvalid
)

// String returns the kind's name.
func (k Kind) String() string {
	switch k {
	case KindMissing:
		return "missing"
	case KindNull:
		return "null"
	case KindBool:
		return "boolean"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	case KindObject:
		return "object"
	case KindTime:
		return "timestamp"
	}
	return "invalid"
}

// Rank is the cross-type sort rank of a kind (see the ordering rules above).
// SQL backends emit the same ranks.
func (k Kind) Rank() int {
	switch k {
	case KindMissing, KindNull:
		return 0
	case KindBool:
		return 1
	case KindNumber:
		return 2
	case KindString:
		return 3
	case KindArray:
		return 4
	case KindObject:
		return 5
	case KindTime:
		return 6
	}
	return 7
}

// KindOf returns the kind of a JSON-like Go value.
func KindOf(v any) Kind {
	switch x := v.(type) {
	case nil:
		return KindNull
	case bool:
		return KindBool
	case float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return KindNumber
	case json.Number:
		if _, err := x.Float64(); err != nil {
			return KindInvalid
		}
		return KindNumber
	case string:
		return KindString
	case []any:
		return KindArray
	case map[string]any:
		return KindObject
	case time.Time:
		return KindTime
	}
	return KindInvalid
}

// FieldKind returns the kind of a system field ("id", "_meta.*") and true, or
// (0, false) for user data paths.
func FieldKind(field string) (Kind, bool) {
	switch field {
	case "id", "_meta.created_by", "_meta.updated_by":
		return KindString, true
	case "_meta.created_at", "_meta.updated_at":
		return KindTime, true
	case "_meta.version":
		return KindNumber, true
	}
	return 0, false
}

// Operand converts a filter value for field into the value and kind used for
// comparisons. For the timestamp system fields an RFC 3339 string becomes a
// KindTime time.Time; any other value for those fields is KindInvalid.
func Operand(field string, v any) (any, Kind) {
	if IsTimeField(field) {
		if t, ok := ParseTime(v); ok {
			return t, KindTime
		}
		return nil, KindInvalid
	}
	k := KindOf(v)
	switch k {
	case KindNumber:
		f, _ := toFloat(v)
		return f, k
	case KindArray, KindObject, KindTime:
		// Filter values must be scalars; a composite never compares equal.
		return nil, KindInvalid
	}
	return v, k
}

// FieldValue returns the value addressed by path in d and whether it exists.
// path is a dot path into d.Data or one of the system fields: "id" (string),
// "_meta.created_at" / "_meta.updated_at" (time.Time), "_meta.created_by" /
// "_meta.updated_by" (string) and "_meta.version" (float64).
func FieldValue(d *Doc, path string) (any, bool) {
	if d == nil {
		return nil, false
	}
	switch path {
	case "id":
		return d.ID, true
	case "_meta.created_at":
		return d.CreatedAt, true
	case "_meta.updated_at":
		return d.UpdatedAt, true
	case "_meta.created_by":
		return d.CreatedBy, true
	case "_meta.updated_by":
		return d.UpdatedBy, true
	case "_meta.version":
		return float64(d.Version), true
	}
	if path == "" {
		return nil, false
	}
	cur := d.Data
	rest := path
	for {
		seg, tail, more := strings.Cut(rest, ".")
		v, ok := cur[seg]
		if !ok {
			return nil, false
		}
		if !more {
			return v, true
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, rest = m, tail
	}
}

// Match reports whether d satisfies every filter (logical AND). Filters should
// come from a query run through PrepareQuery, which validates them and
// canonicalises their operands exactly as the backends do.
func Match(d *Doc, filters []Filter) bool {
	for i := range filters {
		if !MatchFilter(d, filters[i]) {
			return false
		}
	}
	return true
}

// MatchFilter reports whether d satisfies one filter.
func MatchFilter(d *Doc, f Filter) bool {
	v, ok := FieldValue(d, f.Field)
	if !ok {
		return false
	}
	fk := KindOf(v)
	switch f.Op {
	case OpEq, OpNe, OpLt, OpLte, OpGt, OpGte:
		val, vk := Operand(f.Field, f.Value)
		return compareOp(v, fk, f.Op, val, vk)
	case OpIn:
		for _, e := range asList(f.Value) {
			val, vk := Operand(f.Field, e)
			if compareOp(v, fk, OpEq, val, vk) {
				return true
			}
		}
		return false
	case OpNotIn:
		list := asList(f.Value)
		if len(list) == 0 {
			return false
		}
		for _, e := range list {
			val, vk := Operand(f.Field, e)
			if !compareOp(v, fk, OpNe, val, vk) {
				return false
			}
		}
		return true
	case OpContains:
		arr, ok := v.([]any)
		if !ok {
			return false
		}
		val, vk := Operand(f.Field, f.Value)
		for _, e := range arr {
			if compareOp(e, KindOf(e), OpEq, val, vk) {
				return true
			}
		}
		return false
	}
	return false
}

// compareOp applies a binary comparison between a field value and an operand
// under the same-type rule.
func compareOp(field any, fk Kind, op Op, val any, vk Kind) bool {
	if fk != vk {
		return false
	}
	switch fk {
	case KindNull, KindBool, KindNumber, KindString, KindTime:
	default:
		return false
	}
	c := compareSame(field, val, fk)
	switch op {
	case OpEq:
		return c == 0
	case OpNe:
		return c != 0
	case OpLt:
		return c < 0
	case OpLte:
		return c <= 0
	case OpGt:
		return c > 0
	case OpGte:
		return c >= 0
	}
	return false
}

// compareSame compares two values of the same kind k.
func compareSame(a, b any, k Kind) int {
	switch k {
	case KindBool:
		ab, _ := a.(bool)
		bb, _ := b.(bool)
		switch {
		case ab == bb:
			return 0
		case !ab:
			return -1
		}
		return 1
	case KindNumber:
		fa, _ := toFloat(a)
		fb, _ := toFloat(b)
		return cmp.Compare(fa, fb)
	case KindString:
		sa, _ := a.(string)
		sb, _ := b.(string)
		return strings.Compare(sa, sb)
	case KindTime:
		ta, _ := a.(time.Time)
		tb, _ := b.(time.Time)
		return ta.Compare(tb)
	}
	// null == null; arrays and objects are not ordered and tie.
	return 0
}

// CompareValues orders two field values (either may be missing) for one sort
// key in ascending direction.
func CompareValues(a any, aok bool, b any, bok bool) int {
	ka, kb := KindMissing, KindMissing
	if aok {
		ka = KindOf(a)
	}
	if bok {
		kb = KindOf(b)
	}
	if c := cmp.Compare(ka.Rank(), kb.Rank()); c != 0 {
		return c
	}
	if ka != kb {
		// Same rank, different kinds: missing vs null (or two invalid kinds).
		return 0
	}
	return compareSame(a, b, ka)
}

// CompareDocs orders two documents by order (which Normalize ends with the id
// tiebreaker). If every key ties, documents are ordered by id ascending.
func CompareDocs(a, b *Doc, order []Order) int {
	for _, o := range order {
		va, aok := FieldValue(a, o.Field)
		vb, bok := FieldValue(b, o.Field)
		c := CompareValues(va, aok, vb, bok)
		if IsDesc(o) {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return strings.Compare(a.ID, b.ID)
}

// Less reports whether a sorts before b under order.
func Less(a, b *Doc, order []Order) bool {
	return CompareDocs(a, b, order) < 0
}

// SortDocs sorts docs in place by order (stable).
func SortDocs(docs []*Doc, order []Order) {
	slices.SortStableFunc(docs, func(a, b *Doc) int { return CompareDocs(a, b, order) })
}

// IsDesc reports whether o sorts descending (Desc, or Dir "desc" on a query
// that was not normalised).
func IsDesc(o Order) bool {
	return o.Desc || strings.EqualFold(o.Dir, "desc")
}

func asList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	}
	if l, ok := NormalizeValue(v).([]any); ok {
		return l
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}
