package postgres

import (
	"strconv"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
)

// User-supplied values must only ever reach PostgreSQL as bound parameters.
func TestBuildQueryBindsValues(t *testing.T) {
	evil := "x' OR 1=1; DROP TABLE backyard_docs; --"
	q, err := docstore.PrepareQuery(docstore.Query{
		Where: []docstore.Filter{
			{Field: "a", Op: docstore.OpEq, Value: evil},
			{Field: "b-c.d_e", Op: docstore.OpIn, Value: []any{evil, 1, true, nil}},
			{Field: "tags", Op: docstore.OpContains, Value: evil},
			{Field: "id", Op: docstore.OpNotIn, Value: []any{evil}},
			{Field: "n", Op: docstore.OpNe, Value: nil}, // compiles to FALSE, binds nothing
			{Field: "_meta.updated_at", Op: docstore.OpLt, Value: "2026-01-01T00:00:00Z"},
		},
		OrderBy: []docstore.Order{{Field: "b-c.d_e", Dir: "desc"}},
		Limit:   10,
	})
	if err != nil {
		t.Fatal(err)
	}
	stmt, args, err := buildQuery(`"s"."backyard_docs"`, "evil-site", "evil_coll", q)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{evil, "evil-site", "evil_coll", "b-c", "2026", "DROP"} {
		if strings.Contains(stmt, s) {
			t.Errorf("SQL contains user input %q:\n%s", s, stmt)
		}
	}
	// Every bound parameter is referenced (PostgreSQL rejects untyped,
	// unreferenced parameters) and no placeholder is out of range.
	for i := 1; i <= len(args); i++ {
		if !strings.Contains(stmt, "$"+strconv.Itoa(i)+"::") && !strings.Contains(stmt, "$"+strconv.Itoa(i)+" ") &&
			!strings.HasSuffix(stmt, "$"+strconv.Itoa(i)) {
			t.Errorf("parameter $%d is not referenced:\n%s", i, stmt)
		}
	}
	if strings.Contains(stmt, "$"+strconv.Itoa(len(args)+1)) {
		t.Errorf("placeholder beyond %d args:\n%s", len(args), stmt)
	}
}
