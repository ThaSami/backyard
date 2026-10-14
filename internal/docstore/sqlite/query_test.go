package sqlite

import (
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
)

// User-supplied values must only ever reach SQLite as bound parameters.
func TestBuildQueryBindsValues(t *testing.T) {
	evil := "x' OR 1=1; DROP TABLE docs; --"
	q, err := docstore.PrepareQuery(docstore.Query{
		Where: []docstore.Filter{
			{Field: "a", Op: docstore.OpEq, Value: evil},
			{Field: "b-c.d_e", Op: docstore.OpIn, Value: []any{evil, 1, true, nil}},
			{Field: "tags", Op: docstore.OpContains, Value: evil},
			{Field: "id", Op: docstore.OpNotIn, Value: []any{evil}},
			{Field: "_meta.created_at", Op: docstore.OpGte, Value: "2026-01-01T00:00:00Z"},
			{Field: "_meta.version", Op: docstore.OpEq, Value: "not a number"},
		},
		OrderBy: []docstore.Order{{Field: "b-c.d_e", Dir: "desc"}},
		Limit:   10, Offset: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	stmt, args := buildQuery("evil-site", "evil_coll", q)
	for _, s := range []string{evil, "evil-site", "evil_coll", "b-c", "2026"} {
		if strings.Contains(stmt, s) {
			t.Errorf("SQL contains user input %q:\n%s", s, stmt)
		}
	}
	if n := strings.Count(stmt, "?"); n != len(args) {
		t.Errorf("%d placeholders but %d args", n, len(args))
	}
	if args[len(args)-2] != 11 || args[len(args)-1] != 5 {
		t.Errorf("limit/offset args = %v, want limit+1=11, offset=5", args[len(args)-2:])
	}
	if !strings.Contains(stmt, "version = ?") && !strings.Contains(stmt, " AND 0") {
		t.Errorf("mistyped version filter should compile to FALSE:\n%s", stmt)
	}
}

func TestJSONPath(t *testing.T) {
	for in, want := range map[string]string{
		"a":         `$."a"`,
		"a.b-c.d_e": `$."a"."b-c"."d_e"`,
		"_x":        `$."_x"`,
	} {
		if got := jsonPath(in); got != want {
			t.Errorf("jsonPath(%q) = %s, want %s", in, got, want)
		}
	}
}
