// Package conformance is the shared semantic test-suite for docstore.Store
// implementations. Every backend runs it from its own tests:
//
//	func TestConformance(t *testing.T) {
//		conformance.Run(t, func(t *testing.T) docstore.Store { return memory.New() })
//	}
//
// The suite pins down SPEC §5.2 (filters, ordering, missing vs null, type
// strictness, pagination) plus the Store contract (CRUD, versions,
// timestamps, collections, counts, site isolation, atomic updates, value
// canonicalisation and the docstore.WithLimits wrapper), so that all backends
// behave identically.
//
// The data-only query cases also live in querycases.json (QueryCasesJSON),
// a language-neutral fixture of wire-format queries and expected results that
// the Cloudflare edition and the black-box conformance/ suite can run too, so
// every Backyard edition is held to the same semantics.
package conformance

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/docstore"
)

// Run executes the suite. newStore is called once per subtest (subtests run
// in parallel) and must return a ready store; it may return the same shared
// store every time, because each subtest works in its own randomly named
// sites. Run never closes stores: newStore should register cleanup with
// t.Cleanup when needed.
func Run(t *testing.T, newStore func(t *testing.T) docstore.Store) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, s docstore.Store)
	}{
		{"CreateGet", testCreateGet},
		{"GeneratedIDs", testGeneratedIDs},
		{"ExplicitIDAndConflict", testExplicitIDAndConflict},
		{"GetNotFound", testGetNotFound},
		{"ReservedKeysStripped", testReservedKeysStripped},
		{"RoundTripTypes", testRoundTripTypes},
		{"NoAliasing", testNoAliasing},
		{"Put", testPut},
		{"Update", testUpdate},
		{"UpdateNotFound", testUpdateNotFound},
		{"Delete", testDelete},
		{"Filters", testFilters},
		{"SystemFieldFilters", testSystemFieldFilters},
		{"OrderNumbers", testOrderNumbers},
		{"NumberEdgeCases", testNumberEdgeCases},
		{"OrderStrings", testOrderStrings},
		{"OrderBooleans", testOrderBooleans},
		{"OrderCrossType", testOrderCrossType},
		{"OrderMultiKey", testOrderMultiKey},
		{"OrderNested", testOrderNested},
		{"OrderSystemFields", testOrderSystemFields},
		{"Pagination", testPagination},
		{"DefaultOrderIsCreationOrder", testDefaultOrder},
		{"UnnormalizedQuery", testUnnormalizedQuery},
		{"EmptyCollection", testEmptyCollection},
		{"Collections", testCollections},
		{"Count", testCount},
		{"DeleteSite", testDeleteSite},
		{"SiteIsolation", testSiteIsolation},
		{"ConcurrentUpdates", testConcurrentUpdates},
		{"ConcurrentCreateSameID", testConcurrentCreateSameID},
		{"Unicode", testUnicode},
		{"LargeDoc", testLargeDoc},
		{"WithLimits", testWithLimits},
		{"WithLimitsConcurrentQuota", testWithLimitsConcurrentQuota},
		{"WithLimitsSystemSiteExempt", testWithLimitsSystemSiteExempt},
		{"CanonicalValues", testCanonicalValues},
		{"NestingDepth", testNestingDepth},
		{"InvalidFilterOperands", testInvalidFilterOperands},
		{"SharedQueryCases", testSharedQueryCases},
		{"InvalidInput", testInvalidInput},
		{"SystemNamespace", testSystemNamespace},
		{"CanceledContext", testCanceledContext},
		{"Ping", testPing},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			if s == nil {
				t.Fatal("newStore returned nil")
			}
			tc.fn(t, s)
		})
	}
}

// ---------------------------------------------------------------------------
// helpers

var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func newCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// newSite returns a random, valid site name unique to this test.
func newSite(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("cf-%08x", rand.Uint32())
}

type doc = map[string]any

func mustCreate(t *testing.T, s docstore.Store, site, coll, id string, data doc, actor string) *docstore.Doc {
	t.Helper()
	d, err := s.Create(newCtx(t), site, coll, id, data, actor)
	if err != nil {
		t.Fatalf("Create(%s/%s/%q): %v", site, coll, id, err)
	}
	return d
}

func mustGet(t *testing.T, s docstore.Store, site, coll, id string) *docstore.Doc {
	t.Helper()
	d, err := s.Get(newCtx(t), site, coll, id)
	if err != nil {
		t.Fatalf("Get(%s/%s/%q): %v", site, coll, id, err)
	}
	return d
}

func query(t *testing.T, s docstore.Store, site, coll string, q docstore.Query) *docstore.Page {
	t.Helper()
	if err := q.Normalize(1000); err != nil {
		t.Fatalf("Normalize(%+v): %v", q, err)
	}
	p, err := s.Query(newCtx(t), site, coll, q)
	if err != nil {
		t.Fatalf("Query(%s/%s, %+v): %v", site, coll, q, err)
	}
	if p == nil {
		t.Fatalf("Query(%s/%s) returned nil page", site, coll)
	}
	return p
}

func ids(p *docstore.Page) []string {
	out := make([]string, 0, len(p.Docs))
	for _, d := range p.Docs {
		out = append(out, d.ID)
	}
	return out
}

func norm(d doc) doc {
	out, _ := docstore.NormalizeValue(d).(map[string]any)
	if out == nil {
		out = doc{}
	}
	return out
}

func assertData(t *testing.T, what string, got map[string]any, want doc) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: Data is nil, want non-nil map", what)
		return
	}
	if !reflect.DeepEqual(got, norm(want)) {
		t.Errorf("%s: data mismatch\n got: %#v\nwant: %#v", what, got, norm(want))
	}
}

func assertIs(t *testing.T, what string, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: err = %v, want %v", what, err, target)
	}
}

func assertTime(t *testing.T, what string, ts time.Time) {
	t.Helper()
	if ts.IsZero() {
		t.Errorf("%s: zero time", what)
		return
	}
	if ts.Location() != time.UTC {
		t.Errorf("%s: location %v, want UTC", what, ts.Location())
	}
	if ts.Nanosecond()%int(docstore.TimePrecision) != 0 {
		t.Errorf("%s: %v is not truncated to %v", what, ts, docstore.TimePrecision)
	}
	if d := time.Since(ts); d > time.Hour || d < -time.Minute {
		t.Errorf("%s: %v is not close to now", what, ts)
	}
}

func timeStr(ts time.Time) string { return ts.UTC().Format(time.RFC3339Nano) }

// ---------------------------------------------------------------------------
// CRUD

func testCreateGet(t *testing.T, s docstore.Store) {
	site := newSite(t)
	data := doc{"title": "Hello", "votes": 3, "tags": []any{"a", "b"}}
	d := mustCreate(t, s, site, "posts", "", data, "alice@acme.com")
	if !ulidRe.MatchString(d.ID) {
		t.Errorf("generated id %q is not a ULID", d.ID)
	}
	if d.Site != site || d.Collection != "posts" {
		t.Errorf("Create returned site/collection %q/%q", d.Site, d.Collection)
	}
	if d.Version != 1 {
		t.Errorf("Create version = %d, want 1", d.Version)
	}
	if d.CreatedBy != "alice@acme.com" || d.UpdatedBy != "alice@acme.com" {
		t.Errorf("Create actors = %q/%q", d.CreatedBy, d.UpdatedBy)
	}
	assertTime(t, "CreatedAt", d.CreatedAt)
	if !d.UpdatedAt.Equal(d.CreatedAt) {
		t.Errorf("UpdatedAt %v != CreatedAt %v on create", d.UpdatedAt, d.CreatedAt)
	}
	assertData(t, "Create", d.Data, data)

	g := mustGet(t, s, site, "posts", d.ID)
	assertData(t, "Get", g.Data, data)
	if g.ID != d.ID || g.Site != site || g.Collection != "posts" || g.Version != 1 ||
		!g.CreatedAt.Equal(d.CreatedAt) || !g.UpdatedAt.Equal(d.UpdatedAt) ||
		g.CreatedBy != d.CreatedBy || g.UpdatedBy != d.UpdatedBy {
		t.Errorf("Get = %+v, want metadata of %+v", g, d)
	}
	assertTime(t, "Get CreatedAt", g.CreatedAt)

	// Empty and nil bodies are valid documents.
	e := mustCreate(t, s, site, "posts", "empty", nil, "")
	assertData(t, "Create(nil)", e.Data, doc{})
	assertData(t, "Get(empty)", mustGet(t, s, site, "posts", "empty").Data, doc{})
}

func testGeneratedIDs(t *testing.T, s docstore.Store) {
	site := newSite(t)
	prev := ""
	for i := range 50 {
		d := mustCreate(t, s, site, "items", "", doc{"i": i}, "")
		if !ulidRe.MatchString(d.ID) {
			t.Fatalf("id %q is not a ULID", d.ID)
		}
		if d.ID <= prev {
			t.Fatalf("ids not strictly increasing: %q after %q", d.ID, prev)
		}
		prev = d.ID
	}
}

func testExplicitIDAndConflict(t *testing.T, s docstore.Store) {
	site := newSite(t)
	const id = "a:b.c-d_E9"
	d := mustCreate(t, s, site, "c", id, doc{"v": 1}, "")
	if d.ID != id {
		t.Errorf("explicit id = %q, want %q", d.ID, id)
	}
	_, err := s.Create(newCtx(t), site, "c", id, doc{"v": 2}, "")
	assertIs(t, "duplicate Create", err, docstore.ErrConflict)
	assertData(t, "after conflict", mustGet(t, s, site, "c", id).Data, doc{"v": 1})

	// The same id is independent in another collection and another site.
	mustCreate(t, s, site, "other", id, doc{"v": 3}, "")
	mustCreate(t, s, newSite(t), "c", id, doc{"v": 4}, "")
	// Max-length id.
	long := strings.Repeat("x", 128)
	mustCreate(t, s, site, "c", long, doc{}, "")
	mustGet(t, s, site, "c", long)
}

func testGetNotFound(t *testing.T, s docstore.Store) {
	site := newSite(t)
	_, err := s.Get(newCtx(t), site, "nothing", "nope")
	assertIs(t, "Get on empty site", err, docstore.ErrNotFound)
	mustCreate(t, s, site, "c", "a", doc{}, "")
	_, err = s.Get(newCtx(t), site, "c", "b")
	assertIs(t, "Get missing id", err, docstore.ErrNotFound)
	_, err = s.Get(newCtx(t), site, "d", "a")
	assertIs(t, "Get id from other collection", err, docstore.ErrNotFound)
}

func testReservedKeysStripped(t *testing.T, s docstore.Store) {
	site := newSite(t)
	d := mustCreate(t, s, site, "c", "x1", doc{"id": "evil", "_meta": doc{"version": 99}, "a": 1}, "")
	if d.ID != "x1" || d.Version != 1 {
		t.Errorf("reserved keys leaked into metadata: id=%q version=%d", d.ID, d.Version)
	}
	assertData(t, "Create", d.Data, doc{"a": 1})
	p, err := s.Put(newCtx(t), site, "c", "x1", doc{"id": "evil", "_meta": "x", "b": 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	assertData(t, "Put", p.Data, doc{"b": 2})
	u, err := s.Update(newCtx(t), site, "c", "x1", doc{"id": "evil", "_meta": nil, "c": 3}, "")
	if err != nil {
		t.Fatal(err)
	}
	assertData(t, "Update", u.Data, doc{"b": 2, "c": 3})
	assertData(t, "Get", mustGet(t, s, site, "c", "x1").Data, doc{"b": 2, "c": 3})
}

func testRoundTripTypes(t *testing.T, s docstore.Store) {
	site := newSite(t)
	in := doc{
		"int":      42,
		"int64":    int64(-7),
		"float":    3.25,
		"zero":     0,
		"big":      1e21,
		"tiny":     1e-7,
		"maxsafe":  float64(1<<53 - 1),
		"t":        true,
		"f":        false,
		"null":     nil,
		"str":      "hello",
		"empty":    "",
		"arr":      []any{1, "two", true, nil, []any{3}, doc{"four": 4}},
		"strs":     []string{"a", "b"},
		"emptyArr": []any{},
		"obj":      doc{"nested": doc{"deeper": doc{"x": 1.5}}, "nil": nil},
		"emptyObj": doc{},
	}
	want := doc{
		"int": 42.0, "int64": -7.0, "float": 3.25, "zero": 0.0, "big": 1e21, "tiny": 1e-7,
		"maxsafe": float64(1<<53 - 1), "t": true, "f": false, "null": nil, "str": "hello", "empty": "",
		"arr":      []any{1.0, "two", true, nil, []any{3.0}, map[string]any{"four": 4.0}},
		"strs":     []any{"a", "b"},
		"emptyArr": []any{},
		"obj":      map[string]any{"nested": map[string]any{"deeper": map[string]any{"x": 1.5}}, "nil": nil},
		"emptyObj": map[string]any{},
	}
	d := mustCreate(t, s, site, "types", "t1", in, "")
	assertData(t, "Create", d.Data, want)
	assertData(t, "Get", mustGet(t, s, site, "types", "t1").Data, want)
	p := query(t, s, site, "types", docstore.Query{})
	if len(p.Docs) != 1 {
		t.Fatalf("Query returned %d docs", len(p.Docs))
	}
	assertData(t, "Query", p.Docs[0].Data, want)
}

func testNoAliasing(t *testing.T, s docstore.Store) {
	site := newSite(t)
	nested := doc{"k": "v"}
	arr := []any{"x"}
	in := doc{"nested": nested, "arr": arr, "top": 1}
	d := mustCreate(t, s, site, "c", "a", in, "")
	// Mutating the input after the call must not affect the stored doc.
	nested["k"] = "changed"
	arr[0] = "changed"
	in["top"] = 2
	want := doc{"nested": doc{"k": "v"}, "arr": []any{"x"}, "top": 1}
	assertData(t, "after mutating input", mustGet(t, s, site, "c", "a").Data, want)
	// Mutating returned docs must not affect the stored doc either.
	d.Data["top"] = 99
	d.Data["nested"].(map[string]any)["k"] = "bad"
	g := mustGet(t, s, site, "c", "a")
	g.Data["arr"].([]any)[0] = "bad"
	g.Data["new"] = true
	p := query(t, s, site, "c", docstore.Query{})
	p.Docs[0].Data["top"] = 100
	assertData(t, "after mutating outputs", mustGet(t, s, site, "c", "a").Data, want)

	u, err := s.Update(newCtx(t), site, "c", "a", doc{"nested": nested}, "")
	if err != nil {
		t.Fatal(err)
	}
	nested["k"] = "after-update"
	u.Data["nested"].(map[string]any)["k"] = "bad"
	assertData(t, "after update", mustGet(t, s, site, "c", "a").Data,
		doc{"nested": doc{"k": "changed"}, "arr": []any{"x"}, "top": 1})
}

func testPut(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	d1, err := s.Put(ctx, site, "c", "p1", doc{"a": 1, "b": 2}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if d1.Version != 1 || d1.CreatedBy != "alice" || d1.UpdatedBy != "alice" || !d1.CreatedAt.Equal(d1.UpdatedAt) {
		t.Errorf("Put(new) = %+v", d1)
	}
	assertTime(t, "Put CreatedAt", d1.CreatedAt)

	d2, err := s.Put(ctx, site, "c", "p1", doc{"c": 3}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Version != 2 {
		t.Errorf("Put(replace) version = %d, want 2", d2.Version)
	}
	if !d2.CreatedAt.Equal(d1.CreatedAt) || d2.CreatedBy != "alice" {
		t.Errorf("Put(replace) changed created_*: %v %q", d2.CreatedAt, d2.CreatedBy)
	}
	if d2.UpdatedBy != "bob" || !d2.UpdatedAt.After(d1.UpdatedAt) {
		t.Errorf("Put(replace) updated_* = %v %q (previous %v)", d2.UpdatedAt, d2.UpdatedBy, d1.UpdatedAt)
	}
	assertData(t, "Put(replace)", d2.Data, doc{"c": 3})
	g := mustGet(t, s, site, "c", "p1")
	assertData(t, "Get after Put", g.Data, doc{"c": 3})
	if g.Version != 2 || !g.CreatedAt.Equal(d1.CreatedAt) || !g.UpdatedAt.Equal(d2.UpdatedAt) || g.UpdatedBy != "bob" {
		t.Errorf("Get after Put = %+v", g)
	}

	d3, err := s.Put(ctx, site, "c", "p1", doc{}, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if d3.Version != 3 {
		t.Errorf("third Put version = %d", d3.Version)
	}
	assertData(t, "Put(empty)", d3.Data, doc{})

	// Delete then Put starts over.
	if ok, err := s.Delete(ctx, site, "c", "p1"); err != nil || !ok {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	d4, err := s.Put(ctx, site, "c", "p1", doc{"z": 1}, "dave")
	if err != nil {
		t.Fatal(err)
	}
	if d4.Version != 1 || d4.CreatedBy != "dave" || !d4.CreatedAt.After(d1.CreatedAt) {
		t.Errorf("Put after Delete = %+v", d4)
	}
}

func testUpdate(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	c := mustCreate(t, s, site, "c", "u1", doc{"a": 1, "b": doc{"x": 1}, "c": "keep", "d": "drop"}, "alice")
	u, err := s.Update(ctx, site, "c", "u1", doc{"a": 2, "b": doc{"y": 2}, "d": nil, "e": []any{1}, "missing": nil}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	want := doc{"a": 2, "b": doc{"y": 2}, "c": "keep", "e": []any{1}}
	assertData(t, "Update", u.Data, want)
	if u.Version != 2 || u.CreatedBy != "alice" || u.UpdatedBy != "bob" ||
		!u.CreatedAt.Equal(c.CreatedAt) || !u.UpdatedAt.After(c.UpdatedAt) {
		t.Errorf("Update metadata = %+v (created %+v)", u, c)
	}
	g := mustGet(t, s, site, "c", "u1")
	assertData(t, "Get after Update", g.Data, want)
	if g.Version != 2 || !g.UpdatedAt.Equal(u.UpdatedAt) || g.UpdatedBy != "bob" {
		t.Errorf("Get after Update = %+v", g)
	}

	// An empty patch still counts as a write.
	u2, err := s.Update(ctx, site, "c", "u1", doc{}, "carol")
	if err != nil {
		t.Fatal(err)
	}
	if u2.Version != 3 || u2.UpdatedBy != "carol" {
		t.Errorf("empty Update = %+v", u2)
	}
	assertData(t, "empty Update", u2.Data, want)

	// Nested nulls are stored, not deleted (shallow merge).
	u3, err := s.Update(ctx, site, "c", "u1", doc{"b": doc{"y": nil}, "nul": []any{nil}}, "")
	if err != nil {
		t.Fatal(err)
	}
	want = doc{"a": 2, "b": doc{"y": nil}, "c": "keep", "e": []any{1}, "nul": []any{nil}}
	assertData(t, "nested null Update", u3.Data, want)
	assertData(t, "Get after nested null", mustGet(t, s, site, "c", "u1").Data, want)

	// Deleting every key leaves an empty document.
	u4, err := s.Update(ctx, site, "c", "u1", doc{"a": nil, "b": nil, "c": nil, "e": nil, "nul": nil}, "")
	if err != nil {
		t.Fatal(err)
	}
	assertData(t, "delete-all Update", u4.Data, doc{})
}

func testUpdateNotFound(t *testing.T, s docstore.Store) {
	site := newSite(t)
	_, err := s.Update(newCtx(t), site, "c", "nope", doc{"a": 1}, "")
	assertIs(t, "Update missing", err, docstore.ErrNotFound)
	_, err = s.Get(newCtx(t), site, "c", "nope")
	assertIs(t, "Update must not create", err, docstore.ErrNotFound)
	mustCreate(t, s, site, "c", "x", doc{}, "")
	if ok, err := s.Delete(newCtx(t), site, "c", "x"); !ok || err != nil {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	_, err = s.Update(newCtx(t), site, "c", "x", doc{"a": 1}, "")
	assertIs(t, "Update deleted", err, docstore.ErrNotFound)
}

func testDelete(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	mustCreate(t, s, site, "c", "d1", doc{"a": 1}, "")
	mustCreate(t, s, site, "c", "d2", doc{"a": 2}, "")
	ok, err := s.Delete(ctx, site, "c", "d1")
	if err != nil || !ok {
		t.Fatalf("Delete existing = %v, %v", ok, err)
	}
	ok, err = s.Delete(ctx, site, "c", "d1")
	if err != nil || ok {
		t.Fatalf("Delete again = %v, %v; want false, nil", ok, err)
	}
	ok, err = s.Delete(ctx, newSite(t), "nothing", "x")
	if err != nil || ok {
		t.Fatalf("Delete in unknown site = %v, %v; want false, nil", ok, err)
	}
	_, err = s.Get(ctx, site, "c", "d1")
	assertIs(t, "Get deleted", err, docstore.ErrNotFound)
	mustGet(t, s, site, "c", "d2")
	if got := ids(query(t, s, site, "c", docstore.Query{})); !slices.Equal(got, []string{"d2"}) {
		t.Errorf("Query after delete = %v", got)
	}
	// Re-creating a deleted id works.
	d := mustCreate(t, s, site, "c", "d1", doc{"again": true}, "")
	if d.Version != 1 {
		t.Errorf("re-created version = %d", d.Version)
	}
}

// ---------------------------------------------------------------------------
// Filters

// filterFixture is created in order f01..f10 (creation order == id order), so
// expected results are listed in default order.
var filterFixture = []struct {
	id    string
	actor string
	data  doc
}{
	{"f01", "alice", doc{"n": 1, "s": "a", "b": true, "tags": []any{"x", "y"}, "nested": doc{"k": "v1", "num": 10}, "my-field": "dash", "_under": 1}},
	{"f02", "bob", doc{"n": 2, "s": "b", "b": false, "tags": []any{"y"}, "nested": doc{"k": "v2"}}},
	{"f03", "alice", doc{"n": "3", "s": "c", "b": 1, "tags": "x"}},
	{"f04", "bob", doc{"n": nil, "s": nil, "b": nil, "tags": nil, "nested": nil}},
	{"f05", "alice", doc{}},
	{"f06", "bob", doc{"n": 3.5, "s": "B", "b": "true", "tags": []any{1, "1", true, nil, []any{"x"}, doc{"x": 1}}, "nested": "notobj"}},
	{"f07", "alice", doc{"n": -10, "s": "", "tags": []any{}, "nested": doc{"k": nil}}},
	{"f08", "bob", doc{"n": 2.0, "s": "é", "b": false, "tags": []any{"X"}, "nested": doc{"k": "v1", "deep": doc{"a": doc{"b": doc{"c": doc{"d": doc{"e": doc{"f": "bottom"}}}}}}}}},
	{"f09", "alice", doc{"n": 1e300, "s": "ab", "b": []any{true}, "tags": doc{"0": "x"}}},
	{"f10", "bob", doc{"n": []any{1}, "s": []any{"a"}, "b": doc{"b": true}}},
}

func createFilterFixture(t *testing.T, s docstore.Store, site string) map[string]*docstore.Doc {
	t.Helper()
	out := make(map[string]*docstore.Doc, len(filterFixture))
	for _, f := range filterFixture {
		out[f.id] = mustCreate(t, s, site, "fx", f.id, f.data, f.actor)
	}
	return out
}

func w(field string, op docstore.Op, value any) docstore.Filter {
	return docstore.Filter{Field: field, Op: op, Value: value}
}

type filterCase struct {
	name  string
	where []docstore.Filter
	want  []string
}

func runFilterCases(t *testing.T, s docstore.Store, site, coll string, all []*docstore.Doc, cases []filterCase) {
	t.Helper()
	for _, tc := range cases {
		q := docstore.Query{Where: tc.where, Limit: 1000}
		got := ids(query(t, s, site, coll, q))
		want := tc.want
		if want == nil {
			want = []string{}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, want)
		}
		// The reference evaluator must agree with the backend.
		if err := q.Normalize(1000); err != nil {
			t.Fatal(err)
		}
		ref := ids(docstore.EvalQuery(slices.Clone(all), q))
		if !slices.Equal(ref, want) {
			t.Errorf("%s: reference evaluator got %v, want %v", tc.name, ref, want)
		}
	}
}

func testFilters(t *testing.T, s docstore.Store) {
	site := newSite(t)
	fx := createFilterFixture(t, s, site)
	all := make([]*docstore.Doc, 0, len(fx))
	for _, d := range fx {
		all = append(all, d)
	}
	l := func(ids ...string) []string { return ids }
	cases := []filterCase{
		// numbers & type strictness
		{"n == 1", []docstore.Filter{w("n", "==", 1)}, l("f01")},
		{"n == 2 (int and 2.0)", []docstore.Filter{w("n", "==", 2)}, l("f02", "f08")},
		{"n == 3 (not \"3\")", []docstore.Filter{w("n", "==", 3)}, nil},
		{"n == \"3\"", []docstore.Filter{w("n", "==", "3")}, l("f03")},
		{"n == 3.5", []docstore.Filter{w("n", "==", 3.5)}, l("f06")},
		{"n == 1e300", []docstore.Filter{w("n", "==", 1e300)}, l("f09")},
		{"n == null", []docstore.Filter{w("n", "==", nil)}, l("f04")},
		{"n != 1", []docstore.Filter{w("n", "!=", 1)}, l("f02", "f06", "f07", "f08", "f09")},
		{"n != null", []docstore.Filter{w("n", "!=", nil)}, nil},
		{"n != \"3\"", []docstore.Filter{w("n", "!=", "3")}, nil},
		{"n != \"x\"", []docstore.Filter{w("n", "!=", "x")}, l("f03")},
		{"n < 2", []docstore.Filter{w("n", "<", 2)}, l("f01", "f07")},
		{"n <= 2", []docstore.Filter{w("n", "<=", 2)}, l("f01", "f02", "f07", "f08")},
		{"n > 2", []docstore.Filter{w("n", ">", 2)}, l("f06", "f09")},
		{"n >= 3.5", []docstore.Filter{w("n", ">=", 3.5)}, l("f06", "f09")},
		{"n < \"4\"", []docstore.Filter{w("n", "<", "4")}, l("f03")},
		{"n > \"3\"", []docstore.Filter{w("n", ">", "3")}, nil},
		{"n < true", []docstore.Filter{w("n", "<", true)}, nil},
		{"n <= null", []docstore.Filter{w("n", "<=", nil)}, l("f04")},
		{"n >= null", []docstore.Filter{w("n", ">=", nil)}, l("f04")},
		{"n < null", []docstore.Filter{w("n", "<", nil)}, nil},
		{"n > null", []docstore.Filter{w("n", ">", nil)}, nil},
		{"n in [1,2,\"3\"]", []docstore.Filter{w("n", "in", []any{1, 2, "3"})}, l("f01", "f02", "f03", "f08")},
		{"n in [null]", []docstore.Filter{w("n", "in", []any{nil})}, l("f04")},
		{"n in [\"1\"]", []docstore.Filter{w("n", "in", []any{"1"})}, nil},
		{"n in [true, 1]", []docstore.Filter{w("n", "in", []any{true, 1})}, l("f01")},
		{"n not-in [1,2]", []docstore.Filter{w("n", "not-in", []any{1, 2})}, l("f06", "f07", "f09")},
		{"n not-in [\"3\"]", []docstore.Filter{w("n", "not-in", []any{"3"})}, nil},
		{"n not-in [\"x\"]", []docstore.Filter{w("n", "not-in", []any{"x"})}, l("f03")},
		{"n not-in [1,\"x\"] (mixed types)", []docstore.Filter{w("n", "not-in", []any{1, "x"})}, nil},
		{"n not-in [null]", []docstore.Filter{w("n", "not-in", []any{nil})}, nil},
		{"n contains 1", []docstore.Filter{w("n", "contains", 1)}, l("f10")},
		// strings: byte order, case sensitivity
		{"s == \"a\"", []docstore.Filter{w("s", "==", "a")}, l("f01")},
		{"s == \"\"", []docstore.Filter{w("s", "==", "")}, l("f07")},
		{"s == \"A\"", []docstore.Filter{w("s", "==", "A")}, nil},
		{"s > \"a\"", []docstore.Filter{w("s", ">", "a")}, l("f02", "f03", "f08", "f09")},
		{"s < \"a\"", []docstore.Filter{w("s", "<", "a")}, l("f06", "f07")},
		{"s >= \"\"", []docstore.Filter{w("s", ">=", "")}, l("f01", "f02", "f03", "f06", "f07", "f08", "f09")},
		{"s < \"b\"", []docstore.Filter{w("s", "<", "b")}, l("f01", "f06", "f07", "f09")},
		{"s > \"z\" (multibyte)", []docstore.Filter{w("s", ">", "z")}, l("f08")},
		{"s < 1", []docstore.Filter{w("s", "<", 1)}, nil},
		{"s contains \"a\"", []docstore.Filter{w("s", "contains", "a")}, l("f10")},
		{"s in [\"a\",\"é\",\"zz\"]", []docstore.Filter{w("s", "in", []any{"a", "é", "zz"})}, l("f01", "f08")},
		{"s not-in [\"a\",\"b\"]", []docstore.Filter{w("s", "not-in", []any{"a", "b"})}, l("f03", "f06", "f07", "f08", "f09")},
		// booleans
		{"b == true", []docstore.Filter{w("b", "==", true)}, l("f01")},
		{"b == 1", []docstore.Filter{w("b", "==", 1)}, l("f03")},
		{"b == \"true\"", []docstore.Filter{w("b", "==", "true")}, l("f06")},
		{"b == false", []docstore.Filter{w("b", "==", false)}, l("f02", "f08")},
		{"b == 0", []docstore.Filter{w("b", "==", 0)}, nil},
		{"b != true", []docstore.Filter{w("b", "!=", true)}, l("f02", "f08")},
		{"b < true", []docstore.Filter{w("b", "<", true)}, l("f02", "f08")},
		{"b > false", []docstore.Filter{w("b", ">", false)}, l("f01")},
		{"b <= true", []docstore.Filter{w("b", "<=", true)}, l("f01", "f02", "f08")},
		{"b >= false", []docstore.Filter{w("b", ">=", false)}, l("f01", "f02", "f08")},
		{"b in [true, 1]", []docstore.Filter{w("b", "in", []any{true, 1})}, l("f01", "f03")},
		{"b not-in [false]", []docstore.Filter{w("b", "not-in", []any{false})}, l("f01")},
		{"b == null", []docstore.Filter{w("b", "==", nil)}, l("f04")},
		{"b contains true", []docstore.Filter{w("b", "contains", true)}, l("f09")},
		// arrays / contains
		{"tags contains \"x\"", []docstore.Filter{w("tags", "contains", "x")}, l("f01")},
		{"tags contains \"y\"", []docstore.Filter{w("tags", "contains", "y")}, l("f01", "f02")},
		{"tags contains 1", []docstore.Filter{w("tags", "contains", 1)}, l("f06")},
		{"tags contains \"1\"", []docstore.Filter{w("tags", "contains", "1")}, l("f06")},
		{"tags contains true", []docstore.Filter{w("tags", "contains", true)}, l("f06")},
		{"tags contains null", []docstore.Filter{w("tags", "contains", nil)}, l("f06")},
		{"tags contains false", []docstore.Filter{w("tags", "contains", false)}, nil},
		{"tags contains \"X\"", []docstore.Filter{w("tags", "contains", "X")}, l("f08")},
		{"tags == \"x\"", []docstore.Filter{w("tags", "==", "x")}, l("f03")},
		{"tags == null", []docstore.Filter{w("tags", "==", nil)}, l("f04")},
		{"tags != \"x\" (arrays never compare)", []docstore.Filter{w("tags", "!=", "x")}, nil},
		{"tags in [\"x\"]", []docstore.Filter{w("tags", "in", []any{"x"})}, l("f03")},
		// nested paths
		{"nested.k == \"v1\"", []docstore.Filter{w("nested.k", "==", "v1")}, l("f01", "f08")},
		{"nested.k != \"v1\"", []docstore.Filter{w("nested.k", "!=", "v1")}, l("f02")},
		{"nested.k == null", []docstore.Filter{w("nested.k", "==", nil)}, l("f07")},
		{"nested.k < \"v2\"", []docstore.Filter{w("nested.k", "<", "v2")}, l("f01", "f08")},
		{"nested.num >= 10", []docstore.Filter{w("nested.num", ">=", 10)}, l("f01")},
		{"nested == null", []docstore.Filter{w("nested", "==", nil)}, l("f04")},
		{"nested == \"notobj\"", []docstore.Filter{w("nested", "==", "notobj")}, l("f06")},
		{"nested.x == null (missing)", []docstore.Filter{w("nested.x", "==", nil)}, nil},
		{"depth 8", []docstore.Filter{w("nested.deep.a.b.c.d.e.f", "==", "bottom")}, l("f08")},
		{"through non-object", []docstore.Filter{w("s.length", ">=", 0)}, nil},
		{"tags.0 (no array indexing)", []docstore.Filter{w("tags.x", "==", 1)}, nil},
		{"my-field", []docstore.Filter{w("my-field", "==", "dash")}, l("f01")},
		{"_under", []docstore.Filter{w("_under", "==", 1)}, l("f01")},
		{"missing field !=", []docstore.Filter{w("nope", "!=", 1)}, nil},
		{"missing field not-in", []docstore.Filter{w("nope", "not-in", []any{1})}, nil},
		{"missing field == null", []docstore.Filter{w("nope", "==", nil)}, nil},
		// AND of several filters
		{"n >= 1 && b == true", []docstore.Filter{w("n", ">=", 1), w("b", "==", true)}, l("f01")},
		{"n >= 1 && b == false", []docstore.Filter{w("n", ">=", 1), w("b", "==", false)}, l("f02", "f08")},
		{"s > a && n == 2", []docstore.Filter{w("s", ">", "a"), w("n", "==", 2)}, l("f02", "f08")},
		{"n in [1,2] && tags contains y", []docstore.Filter{w("n", "in", []any{1, 2}), w("tags", "contains", "y")}, l("f01", "f02")},
		{"range 1 <= n < 3", []docstore.Filter{w("n", ">=", 1), w("n", "<", 3)}, l("f01", "f02", "f08")},
		{"contradiction", []docstore.Filter{w("n", "==", 1), w("n", "==", 2)}, nil},
	}
	runFilterCases(t, s, site, "fx", all, cases)
}

func testSystemFieldFilters(t *testing.T, s docstore.Store) {
	site := newSite(t)
	fx := createFilterFixture(t, s, site)
	all := make([]*docstore.Doc, 0, len(fx))
	for _, d := range fx {
		all = append(all, d)
	}
	l := func(ids ...string) []string { return ids }
	every := l("f01", "f02", "f03", "f04", "f05", "f06", "f07", "f08", "f09", "f10")
	t5 := fx["f05"].CreatedAt
	cases := []filterCase{
		{"id ==", []docstore.Filter{w("id", "==", "f03")}, l("f03")},
		{"id in", []docstore.Filter{w("id", "in", []any{"f01", "f10", "nope"})}, l("f01", "f10")},
		{"id >", []docstore.Filter{w("id", ">", "f08")}, l("f09", "f10")},
		{"id !=", []docstore.Filter{w("id", "!=", "f01")}, every[1:]},
		{"id not-in", []docstore.Filter{w("id", "not-in", []any{"f01", "f02"})}, every[2:]},
		{"id == 3", []docstore.Filter{w("id", "==", 3)}, nil},
		{"id == null", []docstore.Filter{w("id", "==", nil)}, nil},
		{"id contains", []docstore.Filter{w("id", "contains", "f")}, nil},
		{"version == 1", []docstore.Filter{w("_meta.version", "==", 1)}, every},
		{"version > 1", []docstore.Filter{w("_meta.version", ">", 1)}, nil},
		{"version == \"1\"", []docstore.Filter{w("_meta.version", "==", "1")}, nil},
		{"created_by ==", []docstore.Filter{w("_meta.created_by", "==", "alice")}, l("f01", "f03", "f05", "f07", "f09")},
		{"created_by in", []docstore.Filter{w("_meta.created_by", "in", []any{"bob"})}, l("f02", "f04", "f06", "f08", "f10")},
		{"updated_by !=", []docstore.Filter{w("_meta.updated_by", "!=", "alice")}, l("f02", "f04", "f06", "f08", "f10")},
		{"updated_by > a", []docstore.Filter{w("_meta.updated_by", ">", "alice")}, l("f02", "f04", "f06", "f08", "f10")},
		{"created_at >=", []docstore.Filter{w("_meta.created_at", ">=", timeStr(t5))}, every[4:]},
		{"created_at >", []docstore.Filter{w("_meta.created_at", ">", timeStr(t5))}, every[5:]},
		{"created_at ==", []docstore.Filter{w("_meta.created_at", "==", timeStr(t5))}, l("f05")},
		{"created_at !=", []docstore.Filter{w("_meta.created_at", "!=", timeStr(t5))}, l("f01", "f02", "f03", "f04", "f06", "f07", "f08", "f09", "f10")},
		{"created_at <", []docstore.Filter{w("_meta.created_at", "<", timeStr(t5))}, every[:4]},
		{"created_at <=", []docstore.Filter{w("_meta.created_at", "<=", timeStr(t5))}, every[:5]},
		{"created_at == other zone", []docstore.Filter{w("_meta.created_at", "==", t5.In(time.FixedZone("IST", 5*3600+1800)).Format(time.RFC3339Nano))}, l("f05")},
		{"created_at in", []docstore.Filter{w("_meta.created_at", "in", []any{timeStr(fx["f02"].CreatedAt), timeStr(fx["f09"].CreatedAt), "garbage"})}, l("f02", "f09")},
		{"created_at not-in", []docstore.Filter{w("_meta.created_at", "not-in", []any{timeStr(fx["f02"].CreatedAt)})}, l("f01", "f03", "f04", "f05", "f06", "f07", "f08", "f09", "f10")},
		{"created_at == garbage", []docstore.Filter{w("_meta.created_at", "==", "garbage")}, nil},
		{"created_at != garbage", []docstore.Filter{w("_meta.created_at", "!=", "garbage")}, nil},
		{"created_at == 123", []docstore.Filter{w("_meta.created_at", "==", 123)}, nil},
		{"created_at == null", []docstore.Filter{w("_meta.created_at", "==", nil)}, nil},
		{"created_at > far past", []docstore.Filter{w("_meta.created_at", ">", "1000-01-01T00:00:00Z")}, every},
		{"created_at < far future", []docstore.Filter{w("_meta.created_at", "<", "9999-12-31T23:59:59.999999999Z")}, every},
		{"updated_at > last", []docstore.Filter{w("_meta.updated_at", ">", timeStr(fx["f10"].UpdatedAt))}, nil},
		{"updated_at <= last", []docstore.Filter{w("_meta.updated_at", "<=", timeStr(fx["f10"].UpdatedAt))}, every},
		{"mixed system + data", []docstore.Filter{w("_meta.created_by", "==", "bob"), w("n", "==", 2)}, l("f02", "f08")},
	}
	runFilterCases(t, s, site, "fx", all, cases)
}

// ---------------------------------------------------------------------------
// Ordering

func createAll(t *testing.T, s docstore.Store, site, coll string, docs []struct {
	id   string
	data doc
}) []*docstore.Doc {
	t.Helper()
	out := make([]*docstore.Doc, 0, len(docs))
	for _, d := range docs {
		out = append(out, mustCreate(t, s, site, coll, d.id, d.data, ""))
	}
	return out
}

type orderCase struct {
	name  string
	order []docstore.Order
	where []docstore.Filter
	want  []string
}

func runOrderCases(t *testing.T, s docstore.Store, site, coll string, all []*docstore.Doc, cases []orderCase) {
	t.Helper()
	for _, tc := range cases {
		q := docstore.Query{Where: tc.where, OrderBy: tc.order, Limit: 1000}
		got := ids(query(t, s, site, coll, q))
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if err := q.Normalize(1000); err != nil {
			t.Fatal(err)
		}
		if ref := ids(docstore.EvalQuery(slices.Clone(all), q)); !slices.Equal(ref, tc.want) {
			t.Errorf("%s: reference evaluator got %v, want %v", tc.name, ref, tc.want)
		}
	}
}

func asc(f string) docstore.Order  { return docstore.Order{Field: f, Dir: "asc"} }
func desc(f string) docstore.Order { return docstore.Order{Field: f, Dir: "desc"} }

type idDoc = struct {
	id   string
	data doc
}

func testOrderNumbers(t *testing.T, s docstore.Store) {
	site := newSite(t)
	all := createAll(t, s, site, "ord", []idDoc{
		{"o01", doc{"v": 3}}, {"o02", doc{"v": 1}}, {"o03", doc{}}, {"o04", doc{"v": nil}},
		{"o05", doc{"v": 2}}, {"o06", doc{"v": 1}}, {"o07", doc{"v": 10}}, {"o08", doc{"v": -1.5}},
		{"o09", doc{"v": 1e21}}, {"o10", doc{"v": 0.5}},
	})
	runOrderCases(t, s, site, "ord", all, []orderCase{
		{"asc", []docstore.Order{asc("v")}, nil, []string{"o03", "o04", "o08", "o10", "o02", "o06", "o05", "o01", "o07", "o09"}},
		{"desc", []docstore.Order{desc("v")}, nil, []string{"o09", "o07", "o01", "o05", "o02", "o06", "o10", "o08", "o03", "o04"}},
		{"Desc flag", []docstore.Order{{Field: "v", Desc: true}}, nil, []string{"o09", "o07", "o01", "o05", "o02", "o06", "o10", "o08", "o03", "o04"}},
		{"asc filtered", []docstore.Order{asc("v")}, []docstore.Filter{w("v", ">", 0)}, []string{"o10", "o02", "o06", "o05", "o01", "o07", "o09"}},
		{"desc then id desc", []docstore.Order{desc("v"), desc("id")}, nil, []string{"o09", "o07", "o01", "o05", "o06", "o02", "o10", "o08", "o04", "o03"}},
	})
}

// sumPointOneTwo is 0.1+0.2 computed in float64 (0.30000000000000004); a
// constant expression would be folded to exactly 0.3.
var sumPointOneTwo = func() float64 { a, b := 0.1, 0.2; return a + b }()

func testNumberEdgeCases(t *testing.T, s docstore.Store) {
	site := newSite(t)
	all := createAll(t, s, site, "nums", []idDoc{
		{"e1", doc{"v": 1e20}}, {"e2", doc{"v": float64(1 << 63)}}, {"e3", doc{"v": float64(1 << 53)}},
		{"e4", doc{"v": math.Copysign(0, -1)}}, {"e5", doc{"v": 1e-300}}, {"e6", doc{"v": -1e20}},
		{"e7", doc{"v": 0}}, {"e8", doc{"v": sumPointOneTwo}},
	})
	runFilterCases(t, s, site, "nums", all, []filterCase{
		{"== 1e20", []docstore.Filter{w("v", "==", 1e20)}, []string{"e1"}},
		{"> 9e18", []docstore.Filter{w("v", ">", 9e18)}, []string{"e1", "e2"}},
		{"== 2^63", []docstore.Filter{w("v", "==", float64(1<<63))}, []string{"e2"}},
		{"== 2^53", []docstore.Filter{w("v", "==", float64(1<<53))}, []string{"e3"}},
		{"== 0 matches -0", []docstore.Filter{w("v", "==", 0)}, []string{"e4", "e7"}},
		{"> 0", []docstore.Filter{w("v", ">", 0)}, []string{"e1", "e2", "e3", "e5", "e8"}},
		{"< 0", []docstore.Filter{w("v", "<", 0)}, []string{"e6"}},
		{"== 0.1+0.2 exactly", []docstore.Filter{w("v", "==", sumPointOneTwo)}, []string{"e8"}},
		{"== 0.3", []docstore.Filter{w("v", "==", 0.3)}, nil},
	})
	runOrderCases(t, s, site, "nums", all, []orderCase{
		{"asc", []docstore.Order{asc("v")}, nil, []string{"e6", "e4", "e7", "e5", "e8", "e3", "e2", "e1"}},
	})
	assertData(t, "0.1+0.2 round trip", mustGet(t, s, site, "nums", "e8").Data, doc{"v": sumPointOneTwo})
	assertData(t, "2^63 round trip", mustGet(t, s, site, "nums", "e2").Data, doc{"v": float64(1 << 63)})
}

func testOrderStrings(t *testing.T, s docstore.Store) {
	site := newSite(t)
	all := createAll(t, s, site, "ords", []idDoc{
		{"s01", doc{"v": "b"}}, {"s02", doc{"v": "B"}}, {"s03", doc{"v": "a"}}, {"s04", doc{"v": ""}},
		{"s05", doc{"v": "é"}}, {"s06", doc{"v": "aa"}}, {"s07", doc{"v": "Z"}}, {"s08", doc{"v": "ä"}},
		{"s09", doc{"v": "~"}}, {"s10", doc{"v": "😀"}}, {"s11", doc{}}, {"s12", doc{"v": "a"}},
		{"s13", doc{"v": "10"}}, {"s14", doc{"v": "9"}},
	})
	runOrderCases(t, s, site, "ords", all, []orderCase{
		{"asc bytewise", []docstore.Order{asc("v")}, nil,
			[]string{"s11", "s04", "s13", "s14", "s02", "s07", "s03", "s12", "s06", "s01", "s09", "s08", "s05", "s10"}},
		{"desc bytewise", []docstore.Order{desc("v")}, nil,
			[]string{"s10", "s05", "s08", "s09", "s01", "s06", "s03", "s12", "s07", "s02", "s14", "s13", "s04", "s11"}},
	})
}

func testOrderBooleans(t *testing.T, s docstore.Store) {
	site := newSite(t)
	all := createAll(t, s, site, "ordb", []idDoc{
		{"b1", doc{"v": true}}, {"b2", doc{"v": false}}, {"b3", doc{}}, {"b4", doc{"v": true}},
		{"b5", doc{"v": false}}, {"b6", doc{"v": nil}},
	})
	runOrderCases(t, s, site, "ordb", all, []orderCase{
		{"asc", []docstore.Order{asc("v")}, nil, []string{"b3", "b6", "b2", "b5", "b1", "b4"}},
		{"desc", []docstore.Order{desc("v")}, nil, []string{"b1", "b4", "b2", "b5", "b3", "b6"}},
	})
}

// testOrderCrossType pins the cross-type order Backyard backends share
// (missing/null < boolean < number < string < array < object; arrays and
// objects tie). SPEC §5.2 leaves mixed-type ordering unspecified; Backyard
// keeps it identical across backends anyway.
func testOrderCrossType(t *testing.T, s docstore.Store) {
	site := newSite(t)
	all := createAll(t, s, site, "ordx", []idDoc{
		{"x1", doc{"v": "str"}}, {"x2", doc{"v": 5}}, {"x3", doc{"v": true}}, {"x4", doc{"v": nil}},
		{"x5", doc{"v": []any{9}}}, {"x6", doc{"v": doc{"a": 1}}}, {"x7", doc{}}, {"x8", doc{"v": []any{1}}},
		{"x9", doc{"v": doc{}}}, {"xa", doc{"v": false}}, {"xb", doc{"v": "a"}}, {"xc", doc{"v": -1}},
	})
	runOrderCases(t, s, site, "ordx", all, []orderCase{
		{"asc", []docstore.Order{asc("v")}, nil, []string{"x4", "x7", "xa", "x3", "xc", "x2", "xb", "x1", "x5", "x8", "x6", "x9"}},
		{"desc", []docstore.Order{desc("v")}, nil, []string{"x6", "x9", "x5", "x8", "x1", "xb", "x2", "xc", "x3", "xa", "x4", "x7"}},
	})
}

func testOrderMultiKey(t *testing.T, s docstore.Store) {
	site := newSite(t)
	all := createAll(t, s, site, "multi", []idDoc{
		{"m1", doc{"g": "a", "score": 5}}, {"m2", doc{"g": "b", "score": 7}}, {"m3", doc{"g": "a", "score": 9}},
		{"m4", doc{"g": "b", "score": 7}}, {"m5", doc{"score": 1}}, {"m6", doc{"g": "a"}},
	})
	runOrderCases(t, s, site, "multi", all, []orderCase{
		{"g asc, score desc", []docstore.Order{asc("g"), desc("score")}, nil, []string{"m5", "m3", "m1", "m6", "m2", "m4"}},
		{"g asc, id desc", []docstore.Order{asc("g"), desc("id")}, nil, []string{"m5", "m6", "m3", "m1", "m4", "m2"}},
		{"score asc, g desc", []docstore.Order{asc("score"), desc("g")}, nil, []string{"m6", "m5", "m1", "m2", "m4", "m3"}},
		{"g desc, score asc", []docstore.Order{desc("g"), asc("score")}, nil, []string{"m2", "m4", "m6", "m1", "m3", "m5"}},
		{"filtered", []docstore.Order{desc("score")}, []docstore.Filter{w("g", "==", "b")}, []string{"m2", "m4"}},
	})
}

func testOrderNested(t *testing.T, s docstore.Store) {
	site := newSite(t)
	all := createAll(t, s, site, "nest", []idDoc{
		{"n1", doc{"p": doc{"q": 2}}}, {"n2", doc{"p": doc{"q": 1}}}, {"n3", doc{"p": "flat"}},
		{"n4", doc{"p": doc{"q": nil}}}, {"n5", doc{"p": doc{"q": 3, "r": doc{"s": "z"}}}}, {"n6", doc{"p": doc{"r": doc{"s": "a"}}}},
	})
	runOrderCases(t, s, site, "nest", all, []orderCase{
		{"p.q asc", []docstore.Order{asc("p.q")}, nil, []string{"n3", "n4", "n6", "n2", "n1", "n5"}},
		{"p.q desc", []docstore.Order{desc("p.q")}, nil, []string{"n5", "n1", "n2", "n3", "n4", "n6"}},
		{"p.r.s desc", []docstore.Order{desc("p.r.s")}, nil, []string{"n5", "n6", "n1", "n2", "n3", "n4"}},
	})
}

func testOrderSystemFields(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	mustCreate(t, s, site, "sys", "c", doc{}, "zed")
	mustCreate(t, s, site, "sys", "a", doc{}, "amy")
	mustCreate(t, s, site, "sys", "b", doc{}, "mia")
	for _, id := range []string{"c", "c", "b"} {
		if _, err := s.Update(ctx, site, "sys", id, doc{"touched": true}, ""); err != nil {
			t.Fatal(err)
		}
	}
	// versions: a=1, b=2, c=3; update order: c, c, b
	var all []*docstore.Doc
	for _, id := range []string{"a", "b", "c"} {
		all = append(all, mustGet(t, s, site, "sys", id))
	}
	runOrderCases(t, s, site, "sys", all, []orderCase{
		{"default", nil, nil, []string{"c", "a", "b"}},
		{"created_at desc", []docstore.Order{desc("_meta.created_at")}, nil, []string{"b", "a", "c"}},
		{"id asc", []docstore.Order{asc("id")}, nil, []string{"a", "b", "c"}},
		{"id desc", []docstore.Order{desc("id")}, nil, []string{"c", "b", "a"}},
		{"updated_at asc", []docstore.Order{asc("_meta.updated_at")}, nil, []string{"a", "c", "b"}},
		{"updated_at desc", []docstore.Order{desc("_meta.updated_at")}, nil, []string{"b", "c", "a"}},
		{"version desc", []docstore.Order{desc("_meta.version")}, nil, []string{"c", "b", "a"}},
		{"created_by asc", []docstore.Order{asc("_meta.created_by")}, nil, []string{"a", "b", "c"}},
		{"updated_by desc", []docstore.Order{desc("_meta.updated_by"), asc("id")}, nil, []string{"a", "b", "c"}},
	})
}

// ---------------------------------------------------------------------------
// Pagination & listing

func testPagination(t *testing.T, s docstore.Store) {
	site := newSite(t)
	var all []string
	for i := range 25 {
		id := fmt.Sprintf("p%02d", i)
		mustCreate(t, s, site, "pg", id, doc{"n": i % 5}, "")
		all = append(all, id)
	}
	cases := []struct {
		name    string
		q       docstore.Query
		want    []string
		hasMore bool
	}{
		{"first page", docstore.Query{Limit: 10}, all[:10], true},
		{"second page", docstore.Query{Limit: 10, Offset: 10}, all[10:20], true},
		{"last partial page", docstore.Query{Limit: 10, Offset: 20}, all[20:], false},
		{"exact last page", docstore.Query{Limit: 5, Offset: 20}, all[20:], false},
		{"exact boundary has more", docstore.Query{Limit: 5, Offset: 15}, all[15:20], true},
		{"one past", docstore.Query{Limit: 6, Offset: 20}, all[20:], false},
		{"offset at end", docstore.Query{Limit: 10, Offset: 25}, []string{}, false},
		{"offset beyond end", docstore.Query{Limit: 10, Offset: 100}, []string{}, false},
		{"limit 1", docstore.Query{Limit: 1}, all[:1], true},
		{"default limit", docstore.Query{}, all, false},
		{"limit equals total", docstore.Query{Limit: 25}, all, false},
		{"filtered page", docstore.Query{Where: []docstore.Filter{w("n", "==", 0)}, Limit: 2, Offset: 2}, []string{"p10", "p15"}, true},
		{"filtered last", docstore.Query{Where: []docstore.Filter{w("n", "==", 0)}, Limit: 2, Offset: 4}, []string{"p20"}, false},
		{"ordered page", docstore.Query{OrderBy: []docstore.Order{desc("n")}, Limit: 3}, []string{"p04", "p09", "p14"}, true},
		{"ordered page 2", docstore.Query{OrderBy: []docstore.Order{desc("n")}, Limit: 3, Offset: 3}, []string{"p19", "p24", "p03"}, true},
	}
	for _, tc := range cases {
		p := query(t, s, site, "pg", tc.q)
		if got := ids(p); !slices.Equal(got, tc.want) || p.HasMore != tc.hasMore {
			t.Errorf("%s: got %v has_more=%v, want %v has_more=%v", tc.name, got, p.HasMore, tc.want, tc.hasMore)
		}
	}
	// Walking pages yields every document exactly once.
	var walked []string
	for off := 0; ; off += 7 {
		p := query(t, s, site, "pg", docstore.Query{Limit: 7, Offset: off})
		walked = append(walked, ids(p)...)
		if !p.HasMore {
			break
		}
	}
	if !slices.Equal(walked, all) {
		t.Errorf("paging walk = %v", walked)
	}
}

func testDefaultOrder(t *testing.T, s docstore.Store) {
	site := newSite(t)
	var created []string
	// Generated ids and explicit ids that are not in creation order.
	for i := range 30 {
		id := ""
		if i%3 == 0 {
			id = fmt.Sprintf("z-%02d", 30-i)
		}
		created = append(created, mustCreate(t, s, site, "seq", id, doc{"i": i}, "").ID)
	}
	got := ids(query(t, s, site, "seq", docstore.Query{}))
	if !slices.Equal(got, created) {
		t.Errorf("default order\n got %v\nwant %v", got, created)
	}
	got = ids(query(t, s, site, "seq", docstore.Query{OrderBy: []docstore.Order{desc("_meta.created_at")}}))
	rev := slices.Clone(created)
	slices.Reverse(rev)
	if !slices.Equal(got, rev) {
		t.Errorf("created_at desc\n got %v\nwant %v", got, rev)
	}
}

func testUnnormalizedQuery(t *testing.T, s docstore.Store) {
	// Backends must apply the Normalize defaults themselves (default order,
	// id tiebreaker, default limit) when handed a raw query.
	site := newSite(t)
	var created []string
	for i := range 120 {
		created = append(created, mustCreate(t, s, site, "raw", "", doc{"i": i, "even": i%2 == 0}, "").ID)
	}
	p, err := s.Query(newCtx(t), site, "raw", docstore.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(p); !slices.Equal(got, created[:100]) || !p.HasMore {
		t.Errorf("raw query: %d docs has_more=%v; want first 100 in creation order", len(got), p.HasMore)
	}
	p, err = s.Query(newCtx(t), site, "raw", docstore.Query{
		Where:   []docstore.Filter{{Field: "i", Op: docstore.OpLt, Value: int64(4)}},
		OrderBy: []docstore.Order{{Field: "i", Dir: "DESC"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{created[3], created[2], created[1], created[0]}
	if got := ids(p); !slices.Equal(got, want) {
		t.Errorf("raw filtered query = %v, want %v", got, want)
	}
}

func testEmptyCollection(t *testing.T, s docstore.Store) {
	site := newSite(t)
	p := query(t, s, site, "nothing", docstore.Query{Where: []docstore.Filter{w("a", "==", 1)}})
	if len(p.Docs) != 0 || p.HasMore || p.Docs == nil {
		t.Errorf("empty query = %#v; want non-nil empty Docs", p)
	}
	p = query(t, s, site, "nothing", docstore.Query{Offset: 10})
	if len(p.Docs) != 0 || p.HasMore {
		t.Errorf("empty query with offset = %#v", p)
	}
}

func testCollections(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	got, err := s.Collections(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Collections(empty site) = %v", got)
	}
	for _, c := range []string{"b", "a", "_files", "A", "a-b", "a_b", "b", "0"} {
		mustCreate(t, s, site, c, "", doc{"c": c}, "")
	}
	mustCreate(t, s, newSite(t), "other-site", "", doc{}, "")
	want := []string{"0", "A", "_files", "a", "a-b", "a_b", "b"}
	got, err = s.Collections(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("Collections = %v, want %v", got, want)
	}
	// A collection whose documents are all deleted disappears.
	p := query(t, s, site, "b", docstore.Query{})
	for _, d := range p.Docs {
		if _, err := s.Delete(ctx, site, "b", d.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err = s.Collections(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0", "A", "_files", "a", "a-b", "a_b"}; !slices.Equal(got, want) {
		t.Errorf("Collections after emptying b = %v, want %v", got, want)
	}
}

func testCount(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	n, err := s.Count(ctx, site)
	if err != nil || n != 0 {
		t.Fatalf("Count(empty) = %d, %v", n, err)
	}
	for i := range 3 {
		mustCreate(t, s, site, "c1", fmt.Sprintf("a%d", i), doc{}, "")
	}
	mustCreate(t, s, site, "c2", "b1", doc{}, "")
	if _, err := s.Put(ctx, site, "_files", "f1", doc{}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, site, "_files", "f1", doc{"again": true}, ""); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, s, newSite(t), "c1", "x", doc{}, "")
	if n, err := s.Count(ctx, site); err != nil || n != 5 {
		t.Errorf("Count = %d, %v; want 5", n, err)
	}
	if _, err := s.Delete(ctx, site, "c1", "a0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(ctx, site, "c1", "a0"); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count(ctx, site); err != nil || n != 4 {
		t.Errorf("Count after delete = %d, %v; want 4", n, err)
	}
}

func testDeleteSite(t *testing.T, s docstore.Store) {
	a, b := newSite(t), newSite(t)
	ctx := newCtx(t)
	for i := range 30 {
		mustCreate(t, s, a, fmt.Sprintf("c%d", i%3), "", doc{"i": i}, "")
	}
	mustCreate(t, s, a, "_files", "f", doc{}, "")
	mustCreate(t, s, a, "shared", "same", doc{"site": "a"}, "")
	mustCreate(t, s, b, "shared", "same", doc{"site": "b"}, "")
	mustCreate(t, s, b, "c0", "", doc{}, "")

	if err := s.DeleteSite(ctx, a); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count(ctx, a); err != nil || n != 0 {
		t.Errorf("Count(a) after DeleteSite = %d, %v", n, err)
	}
	if c, err := s.Collections(ctx, a); err != nil || len(c) != 0 {
		t.Errorf("Collections(a) after DeleteSite = %v, %v", c, err)
	}
	if _, err := s.Get(ctx, a, "shared", "same"); !errors.Is(err, docstore.ErrNotFound) {
		t.Errorf("Get(a) after DeleteSite: %v", err)
	}
	if p := query(t, s, a, "c0", docstore.Query{}); len(p.Docs) != 0 {
		t.Errorf("Query(a) after DeleteSite = %v", ids(p))
	}
	// Site b is untouched.
	if n, err := s.Count(ctx, b); err != nil || n != 2 {
		t.Errorf("Count(b) = %d, %v; want 2", n, err)
	}
	assertData(t, "Get(b)", mustGet(t, s, b, "shared", "same").Data, doc{"site": "b"})
	// The site can be used again, and deleting an empty site is fine.
	d := mustCreate(t, s, a, "shared", "same", doc{"site": "a2"}, "")
	if d.Version != 1 {
		t.Errorf("re-created doc version = %d", d.Version)
	}
	if n, err := s.Count(ctx, a); err != nil || n != 1 {
		t.Errorf("Count(a) after re-create = %d, %v", n, err)
	}
	if err := s.DeleteSite(ctx, newSite(t)); err != nil {
		t.Errorf("DeleteSite(empty) = %v", err)
	}
}

func testSiteIsolation(t *testing.T, s docstore.Store) {
	a, b := newSite(t), newSite(t)
	ctx := newCtx(t)
	mustCreate(t, s, a, "c", "x", doc{"who": "a"}, "")
	mustCreate(t, s, b, "c", "x", doc{"who": "b"}, "")
	if _, err := s.Update(ctx, b, "c", "x", doc{"extra": 1}, ""); err != nil {
		t.Fatal(err)
	}
	assertData(t, "site a", mustGet(t, s, a, "c", "x").Data, doc{"who": "a"})
	assertData(t, "site b", mustGet(t, s, b, "c", "x").Data, doc{"who": "b", "extra": 1})
	p := query(t, s, a, "c", docstore.Query{})
	if len(p.Docs) != 1 || p.Docs[0].Site != a || p.Docs[0].Collection != "c" {
		t.Errorf("Query(a) = %+v", p.Docs)
	}
	if ok, err := s.Delete(ctx, a, "c", "x"); !ok || err != nil {
		t.Fatalf("Delete(a) = %v, %v", ok, err)
	}
	mustGet(t, s, b, "c", "x")
}

// ---------------------------------------------------------------------------
// Concurrency

func testConcurrentUpdates(t *testing.T, s docstore.Store) {
	site := newSite(t)
	mustCreate(t, s, site, "c", "hot", doc{"base": true}, "")
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Go(func() {
			_, err := s.Update(newCtx(t), site, "c", "hot", doc{fmt.Sprintf("k%02d", i): i}, fmt.Sprintf("w%d", i))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Update: %v", err)
		}
	}
	d := mustGet(t, s, site, "c", "hot")
	want := doc{"base": true}
	for i := range n {
		want[fmt.Sprintf("k%02d", i)] = i
	}
	assertData(t, "after concurrent updates", d.Data, want)
	if d.Version != n+1 {
		t.Errorf("version = %d, want %d", d.Version, n+1)
	}
}

func testConcurrentCreateSameID(t *testing.T, s docstore.Store) {
	site := newSite(t)
	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, conflicts := 0, 0
	for i := range n {
		wg.Go(func() {
			_, err := s.Create(newCtx(t), site, "c", "only", doc{"i": i}, "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, docstore.ErrConflict):
				conflicts++
			default:
				t.Errorf("Create: %v", err)
			}
		})
	}
	wg.Wait()
	if ok != 1 || conflicts != n-1 {
		t.Errorf("concurrent Create: %d ok, %d conflicts; want 1 and %d", ok, conflicts, n-1)
	}
	if c, err := s.Count(newCtx(t), site); err != nil || c != 1 {
		t.Errorf("Count = %d, %v; want 1", c, err)
	}
}

// ---------------------------------------------------------------------------
// Content

func testUnicode(t *testing.T, s docstore.Store) {
	site := newSite(t)
	data := doc{
		"名前":                       "テスト",
		"emoji":                    "😀👍🏽",
		"rtl":                      "שלום עולם",
		"combining":                "e\u0301",
		"escapes":                  "quote\" backslash\\ slash/ newline\n tab\t <html>&amp; \u2028\u2029",
		"key with spaces and.dots": "ok",
		"nested":                   doc{"ключ": []any{"значение", "🎉"}},
	}
	d := mustCreate(t, s, site, "uni", "u1", data, "ünïcødé@acme.com")
	assertData(t, "Create", d.Data, data)
	g := mustGet(t, s, site, "uni", "u1")
	assertData(t, "Get", g.Data, data)
	if g.CreatedBy != "ünïcødé@acme.com" {
		t.Errorf("CreatedBy = %q", g.CreatedBy)
	}
	mustCreate(t, s, site, "uni", "u2", doc{"emoji": "😀"}, "")
	for _, f := range []docstore.Filter{
		w("emoji", "==", "😀👍🏽"),
		w("escapes", "==", data["escapes"]),
		w("combining", "==", "e\u0301"),
		w("emoji", ">", "😀"),
	} {
		if got := ids(query(t, s, site, "uni", docstore.Query{Where: []docstore.Filter{f}})); !slices.Equal(got, []string{"u1"}) {
			t.Errorf("filter %v: got %v", f, got)
		}
	}
	// Precomposed é is a different byte sequence from e + combining accent.
	if got := ids(query(t, s, site, "uni", docstore.Query{Where: []docstore.Filter{w("combining", "==", "é")}})); len(got) != 0 {
		t.Errorf("normalisation-sensitive match: %v", got)
	}
	u, err := s.Update(newCtx(t), site, "uni", "u1", doc{"key with spaces and.dots": nil, "名前": "更新"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Data["key with spaces and.dots"]; ok || u.Data["名前"] != "更新" {
		t.Errorf("unicode Update = %v", u.Data)
	}
}

func testLargeDoc(t *testing.T, s docstore.Store) {
	site := newSite(t)
	big := strings.Repeat("0123456789abcdef", 12*1024) // 192 KiB
	nums := make([]any, 2000)
	for i := range nums {
		nums[i] = float64(i)
	}
	deep := doc{"leaf": "bottom"}
	for range 40 {
		deep = doc{"d": deep}
	}
	many := doc{}
	for i := range 500 {
		many[fmt.Sprintf("key%03d", i)] = i
	}
	data := doc{"big": big, "nums": nums, "deep": deep, "many": many}
	mustCreate(t, s, site, "large", "l1", data, "")
	assertData(t, "Get large", mustGet(t, s, site, "large", "l1").Data, data)
	got := ids(query(t, s, site, "large", docstore.Query{Where: []docstore.Filter{w("nums", "contains", 1999), w("many.key499", "==", 499)}}))
	if !slices.Equal(got, []string{"l1"}) {
		t.Errorf("query on large doc = %v", got)
	}
	u, err := s.Update(newCtx(t), site, "large", "l1", doc{"big": nil, "small": "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.Data["big"]; ok || u.Data["small"] != "x" {
		t.Errorf("Update on large doc kept big or lost small")
	}
}

// ---------------------------------------------------------------------------
// Limits

func testWithLimits(t *testing.T, base docstore.Store) {
	s := docstore.WithLimits(base, 200, 3)
	site := newSite(t)
	ctx := newCtx(t)

	_, err := s.Create(ctx, site, "c", "big", doc{"x": strings.Repeat("y", 300)}, "")
	assertIs(t, "oversized Create", err, docstore.ErrTooLarge)
	_, err = s.Put(ctx, site, "c", "big", doc{"x": strings.Repeat("y", 300)}, "")
	assertIs(t, "oversized Put", err, docstore.ErrTooLarge)
	if _, err := base.Get(ctx, site, "c", "big"); !errors.Is(err, docstore.ErrNotFound) {
		t.Errorf("oversized doc was stored: %v", err)
	}

	for i := range 3 {
		if _, err := s.Create(ctx, site, "c", fmt.Sprintf("d%d", i), doc{"i": i}, ""); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	_, err = s.Create(ctx, site, "c", "d3", doc{}, "")
	assertIs(t, "Create over quota", err, docstore.ErrQuota)
	_, err = s.Create(ctx, site, "other", "", doc{}, "")
	assertIs(t, "Create over quota in another collection", err, docstore.ErrQuota)
	_, err = s.Put(ctx, site, "c", "new", doc{}, "")
	assertIs(t, "Put(new) over quota", err, docstore.ErrQuota)
	if _, err := s.Put(ctx, site, "c", "d0", doc{"replaced": true}, ""); err != nil {
		t.Errorf("Put(existing) at quota: %v", err)
	}
	if _, err := s.Update(ctx, site, "c", "d1", doc{"more": "data"}, ""); err != nil {
		t.Errorf("Update at quota: %v", err)
	}
	_, err = s.Update(ctx, site, "c", "d1", doc{"pad": strings.Repeat("z", 250)}, "")
	assertIs(t, "Update growing past max size", err, docstore.ErrTooLarge)
	_, err = s.Update(ctx, site, "c", "nope", doc{"a": 1}, "")
	assertIs(t, "Update missing via limits", err, docstore.ErrNotFound)
	if d, err := base.Get(ctx, site, "c", "d1"); err != nil {
		t.Fatal(err)
	} else if _, ok := d.Data["pad"]; ok {
		t.Errorf("oversized Update was applied")
	}

	// Other sites have their own quota.
	if _, err := s.Create(ctx, newSite(t), "c", "", doc{}, ""); err != nil {
		t.Errorf("Create in another site: %v", err)
	}
	// Deleting frees quota.
	if ok, err := s.Delete(ctx, site, "c", "d2"); err != nil || !ok {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	if _, err := s.Create(ctx, site, "c", "d4", doc{}, ""); err != nil {
		t.Errorf("Create after Delete freed quota: %v", err)
	}
	_, err = s.Create(ctx, site, "c", "d5", doc{}, "")
	assertIs(t, "Create over quota again", err, docstore.ErrQuota)
	if err := s.DeleteSite(ctx, site); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, site, "c", "d6", doc{}, ""); err != nil {
		t.Errorf("Create after DeleteSite: %v", err)
	}
	// Reads pass through.
	if _, err := s.Get(ctx, site, "c", "d6"); err != nil {
		t.Errorf("Get through limits: %v", err)
	}
}

// testWithLimitsConcurrentQuota checks that concurrent writers on one
// instance cannot overshoot the quota.
func testWithLimitsConcurrentQuota(t *testing.T, base docstore.Store) {
	const quota, writers = 5, 24
	s := docstore.WithLimits(base, 0, quota)
	site := newSite(t)
	var (
		wg              sync.WaitGroup
		mu              sync.Mutex
		ok, overQuota   int
		unexpectedError error
	)
	for i := range writers {
		wg.Go(func() {
			var err error
			if i%2 == 0 {
				_, err = s.Create(newCtx(t), site, "c", "", doc{"i": i}, "")
			} else {
				_, err = s.Put(newCtx(t), site, "c", fmt.Sprintf("put-%02d", i), doc{"i": i}, "")
			}
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, docstore.ErrQuota):
				overQuota++
			default:
				unexpectedError = err
			}
		})
	}
	wg.Wait()
	if unexpectedError != nil {
		t.Fatalf("concurrent write: %v", unexpectedError)
	}
	if ok != quota || overQuota != writers-quota {
		t.Errorf("concurrent writes: %d ok, %d over quota; want %d and %d", ok, overQuota, quota, writers-quota)
	}
	if n, err := base.Count(newCtx(t), site); err != nil || n != quota {
		t.Errorf("Count = %d, %v; want %d", n, err, quota)
	}
}

// testWithLimitsSystemSiteExempt checks that Backyard's own namespace (user
// cache, audit log) is not subject to the per-site quota, but still to the
// size limit.
func testWithLimitsSystemSiteExempt(t *testing.T, base docstore.Store) {
	s := docstore.WithLimits(base, 200, 2)
	ctx := newCtx(t)
	prefix := fmt.Sprintf("exempt-%08x-", rand.Uint32())
	for i := range 5 {
		id := fmt.Sprintf("%s%d", prefix, i)
		if _, err := s.Create(ctx, docstore.SystemSite, "_audit", id, doc{"i": i}, ""); err != nil {
			t.Fatalf("Create in %s #%d: %v", docstore.SystemSite, i, err)
		}
		t.Cleanup(func() { _, _ = base.Delete(context.Background(), docstore.SystemSite, "_audit", id) })
	}
	if _, err := s.Put(ctx, docstore.SystemSite, "_users", prefix+"x@acme.example", doc{"name": "X"}, ""); err != nil {
		t.Errorf("Put in %s: %v", docstore.SystemSite, err)
	}
	t.Cleanup(func() {
		_, _ = base.Delete(context.Background(), docstore.SystemSite, "_users", prefix+"x@acme.example")
	})
	_, err := s.Create(ctx, docstore.SystemSite, "_audit", prefix+"big", doc{"x": strings.Repeat("y", 300)}, "")
	assertIs(t, "oversized system document", err, docstore.ErrTooLarge)
}

// ---------------------------------------------------------------------------
// Canonical values

// testCanonicalValues pins how values that JSON or some engines cannot
// represent are handled, identically by every backend.
func testCanonicalValues(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	negZero := math.Copysign(0, -1)

	// -0 is stored as 0 (PostgreSQL's numeric has no negative zero).
	d := mustCreate(t, s, site, "c", "negzero", doc{"z": negZero, "arr": []any{negZero}, "obj": doc{"z": negZero}}, "")
	p := query(t, s, site, "c", docstore.Query{Where: []docstore.Filter{w("z", "==", negZero)}})
	if len(p.Docs) != 1 {
		t.Fatalf("query z == -0 = %v", ids(p))
	}
	for what, got := range map[string]*docstore.Doc{"Create": d, "Get": mustGet(t, s, site, "c", "negzero"), "Query": p.Docs[0]} {
		z, _ := got.Data["z"].(float64)
		a, _ := got.Data["arr"].([]any)
		o, _ := got.Data["obj"].(map[string]any)
		if math.Signbit(z) || len(a) != 1 || math.Signbit(a[0].(float64)) || math.Signbit(o["z"].(float64)) {
			t.Errorf("%s: -0 not stored as 0: %#v", what, got.Data)
		}
	}

	// Invalid UTF-8 is coerced the way encoding/json does (every invalid byte
	// becomes U+FFFD), in values, keys and actors; filters are coerced alike.
	bad, clean := "a\xffb\xc3", "a\ufffdb\ufffd"
	d = mustCreate(t, s, site, "c", "utf8", doc{"s": bad, "k\xff": []any{bad}, "nested": doc{"\xfe": bad}}, "actor\xff")
	want := doc{"s": clean, "k\ufffd": []any{clean}, "nested": doc{"\ufffd": clean}}
	assertData(t, "Create invalid UTF-8", d.Data, want)
	g := mustGet(t, s, site, "c", "utf8")
	assertData(t, "Get invalid UTF-8", g.Data, want)
	if g.CreatedBy != "actor\ufffd" || d.CreatedBy != g.CreatedBy {
		t.Errorf("actor with invalid UTF-8 stored as %q (returned %q)", g.CreatedBy, d.CreatedBy)
	}
	for _, v := range []string{bad, clean} {
		if got := ids(query(t, s, site, "c", docstore.Query{Where: []docstore.Filter{w("s", "==", v)}})); !slices.Equal(got, []string{"utf8"}) {
			t.Errorf("filter s == %q: %v", v, got)
		}
	}

	// U+0000 cannot be stored by PostgreSQL, so every backend rejects it.
	nul := "a\x00b"
	for _, tc := range []struct {
		name string
		data doc
	}{
		{"value", doc{"s": nul}},
		{"key", doc{nul: 1}},
		{"nested key", doc{"o": doc{nul: 1}}},
		{"array element", doc{"a": []any{"ok", nul}}},
	} {
		_, err := s.Create(ctx, site, "c", "", tc.data, "")
		assertIs(t, "Create with NUL in "+tc.name, err, docstore.ErrInvalid)
		_, err = s.Put(ctx, site, "c", "nul", tc.data, "")
		assertIs(t, "Put with NUL in "+tc.name, err, docstore.ErrInvalid)
		_, err = s.Update(ctx, site, "c", "utf8", tc.data, "")
		assertIs(t, "Update with NUL in "+tc.name, err, docstore.ErrInvalid)
	}
	_, err := s.Create(ctx, site, "c", "", doc{}, "act\x00or")
	assertIs(t, "Create with NUL in actor", err, docstore.ErrInvalid)
	if _, err := s.Get(ctx, site, "c", "nul"); !errors.Is(err, docstore.ErrNotFound) {
		t.Errorf("document with NUL was stored: %v", err)
	}
	assertData(t, "after rejected updates", mustGet(t, s, site, "c", "utf8").Data, want)

	// NaN and infinities are not JSON.
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		_, err := s.Create(ctx, site, "c", "", doc{"x": v}, "")
		assertIs(t, fmt.Sprintf("Create with %v", v), err, docstore.ErrInvalid)
		_, err = s.Update(ctx, site, "c", "utf8", doc{"x": []any{v}}, "")
		assertIs(t, fmt.Sprintf("Update with %v", v), err, docstore.ErrInvalid)
	}
}

// testNestingDepth checks docstore.MaxNestingDepth: deeper documents are
// rejected (SQLite's JSON functions, also in Cloudflare Durable Objects,
// cannot evaluate JSON nested ~1000 levels, so one such document would
// otherwise break every query of its collection).
func testNestingDepth(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	nest := func(levels int) any {
		var v any = "leaf"
		for i := range levels {
			if i%2 == 0 {
				v = []any{v}
			} else {
				v = doc{"k": v}
			}
		}
		return v
	}
	// The body is level 1; MaxNestingDepth-1 nested containers fit.
	okDoc := doc{"d": nest(docstore.MaxNestingDepth - 1), "n": 1}
	mustCreate(t, s, site, "deep", "ok", okDoc, "")
	assertData(t, "deepest allowed document", mustGet(t, s, site, "deep", "ok").Data, okDoc)
	tooDeep := doc{"d": nest(docstore.MaxNestingDepth), "n": 1}
	_, err := s.Create(ctx, site, "deep", "", tooDeep, "")
	assertIs(t, "Create too deep", err, docstore.ErrInvalid)
	_, err = s.Put(ctx, site, "deep", "ok", tooDeep, "")
	assertIs(t, "Put too deep", err, docstore.ErrInvalid)
	_, err = s.Update(ctx, site, "deep", "ok", doc{"x": nest(docstore.MaxNestingDepth)}, "")
	assertIs(t, "Update too deep", err, docstore.ErrInvalid)
	_, err = s.Create(ctx, site, "deep", "", doc{"x": strings.Repeat("[", 1500)}, "")
	if err != nil {
		t.Errorf("a string of brackets is not nesting: %v", err)
	}
	// Queries over the collection keep working.
	got := ids(query(t, s, site, "deep", docstore.Query{
		Where:   []docstore.Filter{w("n", "==", 1)},
		OrderBy: []docstore.Order{desc("d")},
	}))
	if !slices.Equal(got, []string{"ok"}) {
		t.Errorf("query over deep documents = %v", got)
	}
}

// testInvalidFilterOperands checks that operands no stored value can have
// are rejected identically by every backend, and that -0 equals 0.
func testInvalidFilterOperands(t *testing.T, s docstore.Store) {
	site := newSite(t)
	mustCreate(t, s, site, "c", "a", doc{"n": 0, "s": "x"}, "")
	for _, f := range []docstore.Filter{
		w("n", "<", math.NaN()),
		w("n", ">", math.Inf(1)),
		w("n", "in", []any{1, math.Inf(-1)}),
		w("s", "==", "x\x00"),
		w("s", "not-in", []any{"y", "\x00"}),
		w("s", "contains", "\x00"),
	} {
		q := docstore.Query{Where: []docstore.Filter{f}}
		if err := q.Normalize(1000); err != nil {
			t.Fatalf("Normalize(%v): %v", f, err)
		}
		_, err := s.Query(newCtx(t), site, "c", q)
		assertIs(t, fmt.Sprintf("filter %v %q %v", f.Field, f.Op, f.Value), err, docstore.ErrInvalid)
	}
	if got := ids(query(t, s, site, "c", docstore.Query{Where: []docstore.Filter{w("n", "==", math.Copysign(0, -1))}})); !slices.Equal(got, []string{"a"}) {
		t.Errorf("n == -0: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Errors

func testInvalidInput(t *testing.T, s docstore.Store) {
	site := newSite(t)
	ctx := newCtx(t)
	invalid := []struct {
		name string
		call func() error
	}{
		{"Create bad site", func() error { _, err := s.Create(ctx, "Bad_Site", "c", "", doc{}, ""); return err }},
		{"Create bad collection", func() error { _, err := s.Create(ctx, site, "has space", "", doc{}, ""); return err }},
		{"Create bad id", func() error { _, err := s.Create(ctx, site, "c", "a/b", doc{}, ""); return err }},
		{"Create long id", func() error { _, err := s.Create(ctx, site, "c", strings.Repeat("x", 129), doc{}, ""); return err }},
		{"Get bad id", func() error { _, err := s.Get(ctx, site, "c", "a b"); return err }},
		{"Put bad collection", func() error { _, err := s.Put(ctx, site, "", "x", doc{}, ""); return err }},
		{"Update bad id", func() error { _, err := s.Update(ctx, site, "c", "", doc{}, ""); return err }},
		{"Delete bad site", func() error { _, err := s.Delete(ctx, "-bad", "c", "x"); return err }},
		{"Query bad collection", func() error { _, err := s.Query(ctx, site, "../etc", docstore.Query{}); return err }},
		{"Query bad op", func() error {
			_, err := s.Query(ctx, site, "c", docstore.Query{Where: []docstore.Filter{{Field: "a", Op: "~=", Value: 1}}})
			return err
		}},
		{"Query bad field", func() error {
			_, err := s.Query(ctx, site, "c", docstore.Query{Where: []docstore.Filter{{Field: "a..b", Op: "==", Value: 1}}})
			return err
		}},
		{"Query injection field", func() error {
			_, err := s.Query(ctx, site, "c", docstore.Query{OrderBy: []docstore.Order{{Field: "a'); DROP TABLE docs; --"}}})
			return err
		}},
		{"Query non-scalar value", func() error {
			_, err := s.Query(ctx, site, "c", docstore.Query{Where: []docstore.Filter{{Field: "a", Op: "==", Value: map[string]any{"x": 1}}}})
			return err
		}},
		{"Collections bad site", func() error { _, err := s.Collections(ctx, "UPPER"); return err }},
		{"Count bad site", func() error { _, err := s.Count(ctx, ""); return err }},
		{"DeleteSite bad site", func() error { return s.DeleteSite(ctx, "a#b") }},
	}
	for _, tc := range invalid {
		assertIs(t, tc.name, tc.call(), docstore.ErrInvalid)
	}
	// The reserved "_system" namespace and system collections are valid.
	mustCreate(t, s, "_system", "_users", fmt.Sprintf("u-%08x", rand.Uint32()), doc{"email": "a@b.c"}, "")
	// Values that cannot be encoded as JSON are rejected, not stored.
	if _, err := s.Create(ctx, site, "c", "nan", doc{"x": func() {}}, ""); err == nil {
		t.Errorf("Create with an unencodable value succeeded")
	}
}

// testSystemNamespace covers data Backyard itself stores: the reserved
// "_system" site and "_"-prefixed collections, whose ids may be e-mail
// addresses (the user cache is keyed by lowercased e-mail).
func testSystemNamespace(t *testing.T, s docstore.Store) {
	ctx := newCtx(t)
	email := fmt.Sprintf("alice.smith+%08x@acme.example", rand.Uint32())
	d := mustCreate(t, s, "_system", "_users", email, doc{"name": "Alice"}, "")
	if d.ID != email {
		t.Errorf("id = %q", d.ID)
	}
	if _, err := s.Put(ctx, "_system", "_users", email, doc{"name": "Alice S."}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, "_system", "_users", email, doc{"team": "Platform"}, ""); err != nil {
		t.Fatal(err)
	}
	assertData(t, "user cache entry", mustGet(t, s, "_system", "_users", email).Data, doc{"name": "Alice S.", "team": "Platform"})
	got := ids(query(t, s, "_system", "_users", docstore.Query{Where: []docstore.Filter{w("id", "==", email)}}))
	if !slices.Equal(got, []string{email}) {
		t.Errorf("query by email id = %v", got)
	}
	if c, err := s.Collections(ctx, "_system"); err != nil || !slices.Contains(c, "_users") {
		t.Errorf("Collections(_system) = %v, %v", c, err)
	}
	if ok, err := s.Delete(ctx, "_system", "_users", email); err != nil || !ok {
		t.Errorf("Delete = %v, %v", ok, err)
	}
	// Site-level system collections are listed with user collections.
	site := newSite(t)
	mustCreate(t, s, site, "_files", "01HZX", doc{"name": "a.png"}, "")
	mustCreate(t, s, site, "_files", "file with spaces.png", doc{}, "")
	if c, err := s.Collections(ctx, site); err != nil || !slices.Equal(c, []string{"_files"}) {
		t.Errorf("Collections = %v, %v", c, err)
	}
	// User collections keep the strict id syntax.
	_, err := s.Create(ctx, site, "posts", "a@b.c", doc{}, "")
	assertIs(t, "e-mail id in a user collection", err, docstore.ErrInvalid)
	_, err = s.Create(ctx, site, "_files", "bad\nid", doc{}, "")
	assertIs(t, "control character in a system id", err, docstore.ErrInvalid)
}

func testCanceledContext(t *testing.T, s docstore.Store) {
	site := newSite(t)
	mustCreate(t, s, site, "c", "x", doc{}, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Get(ctx, site, "c", "x"); err == nil {
		t.Errorf("Get with canceled context succeeded")
	}
	if _, err := s.Create(ctx, site, "c", "y", doc{}, ""); err == nil {
		t.Errorf("Create with canceled context succeeded")
	}
	if _, err := s.Query(ctx, site, "c", docstore.Query{}); err == nil {
		t.Errorf("Query with canceled context succeeded")
	}
	if _, err := s.Get(newCtx(t), site, "c", "y"); !errors.Is(err, docstore.ErrNotFound) {
		t.Errorf("canceled Create stored a document: %v", err)
	}
}

func testPing(t *testing.T, s docstore.Store) {
	if err := s.Ping(newCtx(t)); err != nil {
		t.Errorf("Ping: %v", err)
	}
}
