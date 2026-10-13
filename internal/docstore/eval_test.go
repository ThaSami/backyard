package docstore

import (
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func testDoc(id string, data map[string]any) *Doc {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 6000, time.UTC)
	return &Doc{
		Site: "s", Collection: "c", ID: id, Data: data,
		CreatedAt: t0, UpdatedAt: t0.Add(time.Second), CreatedBy: "alice", UpdatedBy: "bob", Version: 3,
	}
}

func TestFieldValue(t *testing.T) {
	d := testDoc("doc1", map[string]any{
		"a":     1.0,
		"n":     nil,
		"obj":   map[string]any{"b": map[string]any{"c": "deep"}, "s": "x"},
		"str":   "flat",
		"arr":   []any{1.0},
		"_meta": "never set by stores, but harmless",
	})
	cases := []struct {
		path   string
		want   any
		wantOK bool
	}{
		{"id", "doc1", true},
		{"_meta.created_at", d.CreatedAt, true},
		{"_meta.updated_at", d.UpdatedAt, true},
		{"_meta.created_by", "alice", true},
		{"_meta.updated_by", "bob", true},
		{"_meta.version", 3.0, true},
		{"a", 1.0, true},
		{"n", nil, true},
		{"obj.b.c", "deep", true},
		{"obj.s", "x", true},
		{"obj.missing", nil, false},
		{"str.length", nil, false},
		{"arr.x", nil, false},
		{"missing", nil, false},
		{"", nil, false},
	}
	for _, tc := range cases {
		got, ok := FieldValue(d, tc.path)
		if ok != tc.wantOK || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("FieldValue(%q) = %#v, %v; want %#v, %v", tc.path, got, ok, tc.want, tc.wantOK)
		}
	}
	if _, ok := FieldValue(nil, "id"); ok {
		t.Error("FieldValue(nil doc) reported a value")
	}
	if _, ok := FieldValue(&Doc{}, "a"); ok {
		t.Error("FieldValue on nil Data reported a value")
	}
}

func TestKindOf(t *testing.T) {
	cases := []struct {
		v    any
		want Kind
	}{
		{nil, KindNull}, {true, KindBool}, {1.5, KindNumber}, {3, KindNumber}, {int64(3), KindNumber},
		{uint8(1), KindNumber}, {float32(1), KindNumber}, {json.Number("4"), KindNumber}, {json.Number("x"), KindInvalid},
		{"s", KindString}, {[]any{}, KindArray}, {map[string]any{}, KindObject}, {time.Now(), KindTime},
		{struct{}{}, KindInvalid}, {[]string{"a"}, KindInvalid},
	}
	for _, tc := range cases {
		if got := KindOf(tc.v); got != tc.want {
			t.Errorf("KindOf(%#v) = %v, want %v", tc.v, got, tc.want)
		}
	}
	// Ranks implement missing/null < bool < number < string < array < object.
	order := []Kind{KindNull, KindBool, KindNumber, KindString, KindArray, KindObject}
	for i := 1; i < len(order); i++ {
		if order[i-1].Rank() >= order[i].Rank() {
			t.Errorf("rank(%v) >= rank(%v)", order[i-1], order[i])
		}
	}
	if KindMissing.Rank() != KindNull.Rank() {
		t.Error("missing and null must share a rank")
	}
	for k := KindMissing; k <= KindInvalid; k++ {
		if k.String() == "" {
			t.Errorf("Kind(%d).String() is empty", k)
		}
	}
}

func TestOperand(t *testing.T) {
	ts := "2026-01-02T03:04:05.123456789+01:00"
	v, k := Operand("_meta.created_at", ts)
	want := time.Date(2026, 1, 2, 2, 4, 5, 123456000, time.UTC)
	if k != KindTime || !v.(time.Time).Equal(want) {
		t.Errorf("Operand(time) = %v, %v; want %v (truncated to µs)", v, k, want)
	}
	for _, bad := range []any{"2026-01-02", "garbage", 123, nil, true} {
		if _, k := Operand("_meta.updated_at", bad); k != KindInvalid {
			t.Errorf("Operand(time, %#v) kind = %v, want invalid", bad, k)
		}
	}
	if v, k := Operand("n", 3); k != KindNumber || v != 3.0 {
		t.Errorf("Operand(3) = %#v, %v", v, k)
	}
	if v, k := Operand("n", json.Number("2.5")); k != KindNumber || v != 2.5 {
		t.Errorf("Operand(json.Number) = %#v, %v", v, k)
	}
	if _, k := Operand("n", []any{1}); k != KindInvalid {
		t.Errorf("Operand(array) kind = %v", k)
	}
	if _, k := Operand("n", time.Now()); k != KindInvalid {
		t.Errorf("Operand(time.Time on data field) kind = %v", k)
	}
}

func TestMatchFilter(t *testing.T) {
	d := testDoc("d1", map[string]any{
		"num": 3.0, "str": "abc", "t": true, "f": false, "null": nil,
		"arr": []any{1.0, "1", true, nil, []any{"x"}}, "obj": map[string]any{"k": "v"}, "zero": 0.0,
	})
	cases := []struct {
		f    Filter
		want bool
	}{
		{Filter{"num", OpEq, 3.0}, true},
		{Filter{"num", OpEq, 3}, true},
		{Filter{"num", OpEq, int64(3)}, true},
		{Filter{"num", OpEq, json.Number("3")}, true},
		{Filter{"num", OpEq, "3"}, false},
		{Filter{"num", OpNe, 4}, true},
		{Filter{"num", OpNe, 3}, false},
		{Filter{"num", OpNe, "x"}, false},
		{Filter{"num", OpLt, 4}, true},
		{Filter{"num", OpLte, 3}, true},
		{Filter{"num", OpGt, 3}, false},
		{Filter{"num", OpGte, 3}, true},
		{Filter{"num", OpLt, "4"}, false},
		{Filter{"str", OpEq, "abc"}, true},
		{Filter{"str", OpGt, "ab"}, true},
		{Filter{"str", OpLt, "abd"}, true},
		{Filter{"str", OpLt, "B"}, false},
		{Filter{"t", OpEq, true}, true},
		{Filter{"t", OpEq, 1}, false},
		{Filter{"t", OpGt, false}, true},
		{Filter{"f", OpLt, true}, true},
		{Filter{"f", OpNe, true}, true},
		{Filter{"zero", OpEq, false}, false},
		{Filter{"null", OpEq, nil}, true},
		{Filter{"null", OpNe, nil}, false},
		{Filter{"null", OpLte, nil}, true},
		{Filter{"null", OpGte, nil}, true},
		{Filter{"null", OpLt, nil}, false},
		{Filter{"null", OpEq, 0}, false},
		{Filter{"missing", OpEq, nil}, false},
		{Filter{"missing", OpNe, 1}, false},
		{Filter{"missing", OpNotIn, []any{1}}, false},
		{Filter{"num", OpIn, []any{1, 2, 3}}, true},
		{Filter{"num", OpIn, []any{"3"}}, false},
		{Filter{"num", OpIn, []string{"3"}}, false},
		{Filter{"str", OpIn, []string{"abc"}}, true},
		{Filter{"num", OpIn, []any{}}, false},
		{Filter{"num", OpNotIn, []any{1, 2}}, true},
		{Filter{"num", OpNotIn, []any{1, "x"}}, false},
		{Filter{"num", OpNotIn, []any{3}}, false},
		{Filter{"num", OpNotIn, []any{}}, false},
		{Filter{"arr", OpContains, 1}, true},
		{Filter{"arr", OpContains, "1"}, true},
		{Filter{"arr", OpContains, true}, true},
		{Filter{"arr", OpContains, nil}, true},
		{Filter{"arr", OpContains, "x"}, false},
		{Filter{"arr", OpContains, 2}, false},
		{Filter{"str", OpContains, "a"}, false},
		{Filter{"arr", OpEq, 1}, false},
		{Filter{"obj", OpEq, "v"}, false},
		{Filter{"obj.k", OpEq, "v"}, true},
		{Filter{"num", "~", 3}, false},
		{Filter{"id", OpEq, "d1"}, true},
		{Filter{"id", OpGt, "d0"}, true},
		{Filter{"id", OpEq, 1}, false},
		{Filter{"_meta.version", OpEq, 3}, true},
		{Filter{"_meta.version", OpEq, "3"}, false},
		{Filter{"_meta.created_by", OpIn, []any{"alice", "bob"}}, true},
		{Filter{"_meta.created_at", OpEq, "2026-01-02T03:04:05.000006Z"}, true},
		{Filter{"_meta.created_at", OpEq, "2026-01-02T04:04:05.000006+01:00"}, true},
		{Filter{"_meta.created_at", OpGt, "2026-01-02T03:04:05Z"}, true},
		{Filter{"_meta.created_at", OpLt, "2026-01-02T03:04:05Z"}, false},
		{Filter{"_meta.created_at", OpNe, "bogus"}, false},
		{Filter{"_meta.updated_at", OpGt, "2026-01-02T03:04:05.000006Z"}, true},
		{Filter{"_meta.created_at", OpContains, "x"}, false},
	}
	for _, tc := range cases {
		if got := MatchFilter(d, tc.f); got != tc.want {
			t.Errorf("MatchFilter(%v %s %#v) = %v, want %v", tc.f.Field, tc.f.Op, tc.f.Value, got, tc.want)
		}
	}
	if !Match(d, nil) {
		t.Error("Match with no filters must be true")
	}
	if !Match(d, []Filter{{"num", OpEq, 3}, {"t", OpEq, true}}) {
		t.Error("Match AND of true filters = false")
	}
	if Match(d, []Filter{{"num", OpEq, 3}, {"t", OpEq, false}}) {
		t.Error("Match AND with a false filter = true")
	}
}

func TestSortDocs(t *testing.T) {
	mk := func(id string, v any, has bool) *Doc {
		data := map[string]any{}
		if has {
			data["v"] = v
		}
		return testDoc(id, data)
	}
	docs := []*Doc{
		mk("a", 2.0, true), mk("b", nil, true), mk("c", nil, false), mk("d", 1.0, true),
		mk("e", "s", true), mk("f", true, true), mk("g", 1.0, true), mk("h", []any{}, true), mk("i", map[string]any{}, true),
	}
	ids := func(ds []*Doc) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return out
	}
	SortDocs(docs, []Order{{Field: "v"}, {Field: "id"}})
	if got, want := ids(docs), []string{"b", "c", "f", "d", "g", "a", "e", "h", "i"}; !slices.Equal(got, want) {
		t.Errorf("asc = %v, want %v", got, want)
	}
	SortDocs(docs, []Order{{Field: "v", Desc: true}, {Field: "id"}})
	if got, want := ids(docs), []string{"i", "h", "e", "a", "d", "g", "f", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("desc = %v, want %v", got, want)
	}
	// Dir "desc" works on unnormalised orders; the id fallback applies when
	// the order has no id key.
	SortDocs(docs, []Order{{Field: "v", Dir: "desc"}})
	if got, want := ids(docs), []string{"i", "h", "e", "a", "d", "g", "f", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("Dir desc = %v, want %v", got, want)
	}
	if !Less(docs[1], docs[0], []Order{{Field: "id"}}) || Less(docs[0], docs[1], []Order{{Field: "id"}}) {
		t.Error("Less by id disagrees with byte order of h and i")
	}
	if c := CompareValues(nil, false, nil, true); c != 0 {
		t.Errorf("missing vs null = %d, want 0", c)
	}
	t1 := time.Unix(1, 0)
	if c := CompareValues(t1, true, t1.Add(time.Microsecond), true); c != -1 {
		t.Errorf("time compare = %d", c)
	}
}

func TestEvalQuery(t *testing.T) {
	var docs []*Doc
	for i := range 10 {
		d := testDoc(string(rune('a'+i)), map[string]any{"i": float64(i), "even": i%2 == 0})
		d.CreatedAt = d.CreatedAt.Add(time.Duration(i) * time.Microsecond)
		docs = append(docs, d)
	}
	q := Query{Where: []Filter{{"even", OpEq, true}}, Limit: 2, Offset: 1}
	if err := q.Normalize(100); err != nil {
		t.Fatal(err)
	}
	p := EvalQuery(slices.Clone(docs), q)
	if len(p.Docs) != 2 || p.Docs[0].ID != "c" || p.Docs[1].ID != "e" || !p.HasMore {
		t.Errorf("page = %v has_more=%v", p.Docs, p.HasMore)
	}
	q.Offset = 3
	p = EvalQuery(slices.Clone(docs), q)
	if len(p.Docs) != 2 || p.Docs[0].ID != "g" || p.HasMore {
		t.Errorf("last page = %v has_more=%v", p.Docs, p.HasMore)
	}
	q.Offset = 50
	p = EvalQuery(slices.Clone(docs), q)
	if p.Docs == nil || len(p.Docs) != 0 || p.HasMore {
		t.Errorf("offset past end = %#v", p)
	}
	p = EvalQuery(nil, Query{})
	if p.Docs == nil || len(p.Docs) != 0 {
		t.Errorf("empty input = %#v", p)
	}
	// Limit 0 (unnormalised) returns everything.
	if p := EvalQuery(slices.Clone(docs), Query{}); len(p.Docs) != 10 || p.HasMore {
		t.Errorf("limit 0 = %d docs", len(p.Docs))
	}
}

func TestPrepareQuery(t *testing.T) {
	orig := Query{
		Where:   []Filter{{"n", OpEq, 3}},
		OrderBy: []Order{{Field: "n", Dir: "DESC"}},
		Limit:   5000,
	}
	q, err := PrepareQuery(orig)
	if err != nil {
		t.Fatal(err)
	}
	if q.Limit != 5000 {
		t.Errorf("limit = %d, want 5000 (not capped again)", q.Limit)
	}
	if len(q.OrderBy) != 2 || q.OrderBy[1].Field != "id" || !q.OrderBy[0].Desc {
		t.Errorf("order = %+v", q.OrderBy)
	}
	if q.Where[0].Value != 3.0 {
		t.Errorf("value not normalised: %#v", q.Where[0].Value)
	}
	if orig.Where[0].Value != 3 || len(orig.OrderBy) != 1 || orig.OrderBy[0].Dir != "DESC" {
		t.Errorf("caller's query was modified: %+v", orig)
	}
	q, err = PrepareQuery(Query{})
	if err != nil || q.Limit != 100 || len(q.OrderBy) != 2 || q.OrderBy[0].Field != "_meta.created_at" {
		t.Errorf("defaults = %+v, %v", q, err)
	}
	for _, bad := range []Query{
		{Where: []Filter{{"a", "like", 1}}},
		{Where: []Filter{{"a..b", OpEq, 1}}},
		{Where: []Filter{{"a", OpIn, 1}}},
		{OrderBy: []Order{{Field: "a", Dir: "up"}}},
		{Offset: -1},
	} {
		if _, err := PrepareQuery(bad); err == nil {
			t.Errorf("PrepareQuery(%+v) succeeded", bad)
		}
	}
}

func TestApplyPatchAndClone(t *testing.T) {
	data := map[string]any{"a": 1.0, "b": map[string]any{"x": 1.0}, "c": "keep"}
	patch := map[string]any{"a": 2.0, "b": map[string]any{"y": 2.0}, "c": nil, "d": []any{1.0}}
	got := ApplyPatch(data, patch)
	want := map[string]any{"a": 2.0, "b": map[string]any{"y": 2.0}, "d": []any{1.0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyPatch = %v, want %v", got, want)
	}
	if !reflect.DeepEqual(data, map[string]any{"a": 1.0, "b": map[string]any{"x": 1.0}, "c": "keep"}) {
		t.Errorf("ApplyPatch modified data: %v", data)
	}
	if len(ApplyPatch(nil, nil)) != 0 {
		t.Error("ApplyPatch(nil, nil) not empty")
	}

	d := testDoc("x", map[string]any{"m": map[string]any{"k": []any{"v"}}})
	c := CloneDoc(d)
	c.Data["m"].(map[string]any)["k"].([]any)[0] = "changed"
	c.Data["new"] = true
	if d.Data["m"].(map[string]any)["k"].([]any)[0] != "v" || d.Data["new"] != nil {
		t.Error("CloneDoc shares data with the original")
	}
	if CloneDoc(nil) != nil {
		t.Error("CloneDoc(nil) != nil")
	}
	if m := CloneData(nil); m == nil || len(m) != 0 {
		t.Errorf("CloneData(nil) = %#v", m)
	}
}

func TestNowMonotonic(t *testing.T) {
	prev := Now()
	for range 10000 {
		n := Now()
		if !n.After(prev) {
			t.Fatalf("Now not strictly increasing: %v after %v", n, prev)
		}
		if n.Location() != time.UTC || n.Nanosecond()%1000 != 0 {
			t.Fatalf("Now = %v not UTC/µs", n)
		}
		prev = n
	}
	if d := time.Since(prev); d < -time.Second || d > time.Second {
		t.Errorf("Now drifted from wall clock by %v", d)
	}
	// Concurrent callers never see the same value.
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 1000 {
				v := Now().UnixMicro()
				mu.Lock()
				if seen[v] {
					t.Errorf("duplicate timestamp %d", v)
				}
				seen[v] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
}

func TestParseTime(t *testing.T) {
	cases := []struct {
		in   any
		ok   bool
		want time.Time
	}{
		{"2026-01-02T03:04:05Z", true, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"2026-01-02T03:04:05.9999999Z", true, time.Date(2026, 1, 2, 3, 4, 5, 999999000, time.UTC)},
		{"2026-01-02T03:04:05-05:00", true, time.Date(2026, 1, 2, 8, 4, 5, 0, time.UTC)},
		{"2026-01-02", false, time.Time{}},
		{"", false, time.Time{}},
		{12, false, time.Time{}},
	}
	for _, tc := range cases {
		got, ok := ParseTime(tc.in)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("ParseTime(%#v) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
		if ok && got.Location() != time.UTC {
			t.Errorf("ParseTime(%#v) location = %v", tc.in, got.Location())
		}
	}
	if !IsTimeField("_meta.created_at") || !IsTimeField("_meta.updated_at") || IsTimeField("created_at") {
		t.Error("IsTimeField")
	}
}

func TestFieldKind(t *testing.T) {
	for field, want := range map[string]Kind{
		"id": KindString, "_meta.created_by": KindString, "_meta.updated_by": KindString,
		"_meta.created_at": KindTime, "_meta.updated_at": KindTime, "_meta.version": KindNumber,
	} {
		if k, ok := FieldKind(field); !ok || k != want {
			t.Errorf("FieldKind(%q) = %v, %v", field, k, ok)
		}
		if !SystemFields[field] {
			t.Errorf("%q missing from SystemFields", field)
		}
	}
	if _, ok := FieldKind("data"); ok {
		t.Error("FieldKind(data) reported a system field")
	}
}
