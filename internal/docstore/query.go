package docstore

import (
	"fmt"
	"maps"
	"math"
)

// unlimitedList is the maxList PrepareQuery passes to Normalize: backends
// re-validate a query without lowering a limit the caller already capped.
const unlimitedList = math.MaxInt32

// PrepareQuery returns a validated, normalised deep copy of q for execution
// by a backend. It runs Normalize (filling defaults and appending the id
// tiebreaker) without capping the limit any further, so it is idempotent on
// queries the API layer already normalised with database.max_list. The
// caller's slices are never modified.
//
// Filter operands are canonicalised like stored data (see CleanString and
// CleanNumber): invalid UTF-8 is coerced, -0 becomes 0, and NaN, infinities
// and strings containing U+0000 are rejected with ErrInvalid, so every
// backend sees identical operands.
func PrepareQuery(q Query) (Query, error) {
	out := Query{Limit: q.Limit, Offset: q.Offset}
	if len(q.Where) > 0 {
		out.Where = make([]Filter, len(q.Where))
		copy(out.Where, q.Where)
	}
	if len(q.OrderBy) > 0 {
		out.OrderBy = make([]Order, len(q.OrderBy))
		copy(out.OrderBy, q.OrderBy)
	}
	if err := out.Normalize(unlimitedList); err != nil {
		return Query{}, err
	}
	for i := range out.Where {
		f := &out.Where[i]
		if list, ok := f.Value.([]any); ok {
			// Normalize already copied the list; clean it in place.
			for j, e := range list {
				c, err := cleanOperand(e)
				if err != nil {
					return Query{}, fmt.Errorf("filter on %q: %w", f.Field, err)
				}
				list[j] = c
			}
			continue
		}
		c, err := cleanOperand(f.Value)
		if err != nil {
			return Query{}, fmt.Errorf("filter on %q: %w", f.Field, err)
		}
		f.Value = c
	}
	return out, nil
}

// EvalQuery executes a prepared query over an in-memory candidate set (all
// documents of one collection): it filters with Match, sorts with SortDocs and
// applies offset/limit. docs is reordered in place; the returned page shares
// the *Doc pointers.
func EvalQuery(docs []*Doc, q Query) *Page {
	matched := docs[:0:0]
	for _, d := range docs {
		if Match(d, q.Where) {
			matched = append(matched, d)
		}
	}
	SortDocs(matched, q.OrderBy)
	page := &Page{Docs: []*Doc{}}
	if q.Offset >= len(matched) {
		return page
	}
	matched = matched[q.Offset:]
	limit := q.Limit
	if limit <= 0 {
		limit = len(matched)
	}
	if len(matched) > limit {
		page.HasMore = true
		matched = matched[:limit]
	}
	page.Docs = append(page.Docs, matched...)
	return page
}

// ApplyPatch returns the shallow merge of patch into data as a new map: keys
// of patch overwrite keys of data, and a nil value deletes the key. Neither
// argument is modified; values are not copied.
func ApplyPatch(data, patch map[string]any) map[string]any {
	out := make(map[string]any, len(data)+len(patch))
	maps.Copy(out, data)
	for k, v := range patch {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// CloneValue returns a deep copy of a JSON-like value (maps and slices are
// copied recursively; scalars are returned as is).
func CloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return CloneData(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CloneValue(e)
		}
		return out
	}
	return v
}

// CloneData returns a deep copy of a document body. A nil map yields an empty,
// non-nil map.
func CloneData(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = CloneValue(v)
	}
	return out
}

// CloneDoc returns a deep copy of d.
func CloneDoc(d *Doc) *Doc {
	if d == nil {
		return nil
	}
	c := *d
	c.Data = CloneData(d.Data)
	return &c
}
