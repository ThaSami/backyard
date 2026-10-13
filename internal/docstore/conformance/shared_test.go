package conformance

import (
	"testing"

	"github.com/thasami/backyard/internal/docstore"
)

// TestQueryCasesWellFormed validates querycases.json itself: other editions
// load it, so it must be consistent on its own.
func TestQueryCasesWellFormed(t *testing.T) {
	qc, err := LoadQueryCases()
	if err != nil {
		t.Fatal(err)
	}
	if qc.Version != 1 {
		t.Errorf("version = %d, want 1", qc.Version)
	}
	docIDs := make(map[string]map[string]bool, len(qc.Collections))
	for name, docs := range qc.Collections {
		if err := docstore.ValidateCollection(name, false); err != nil {
			t.Errorf("collection %q: %v", name, err)
		}
		if len(docs) == 0 {
			t.Errorf("collection %q is empty", name)
		}
		docIDs[name] = make(map[string]bool, len(docs))
		for _, d := range docs {
			if err := docstore.ValidateID(d.ID); err != nil {
				t.Errorf("collection %q: %v", name, err)
			}
			if docIDs[name][d.ID] {
				t.Errorf("collection %q: duplicate id %q", name, d.ID)
			}
			docIDs[name][d.ID] = true
			if _, ok := d.Data["id"]; ok {
				t.Errorf("%s/%s: data must not contain the reserved key \"id\"", name, d.ID)
			}
			if _, ok := d.Data["_meta"]; ok {
				t.Errorf("%s/%s: data must not contain the reserved key \"_meta\"", name, d.ID)
			}
		}
	}
	names := make(map[string]bool, len(qc.Cases))
	for _, c := range qc.Cases {
		if c.Name == "" || names[c.Name] {
			t.Errorf("case name %q is empty or not unique", c.Name)
		}
		names[c.Name] = true
		q := c.Query
		if err := q.Normalize(1000); err != nil {
			t.Errorf("%s: invalid query: %v", c.Name, err)
		}
		if c.Query.Limit <= 0 || c.Query.Limit > 1000 {
			t.Errorf("%s: limit %d must be explicit and within the default max_list", c.Name, c.Query.Limit)
		}
		if c.Want == nil {
			t.Errorf("%s: want must be a list (use [] for no results)", c.Name)
		}
		seen := map[string]bool{}
		for _, id := range c.Want {
			if !docIDs[c.Collection][id] {
				t.Errorf("%s: want references unknown document %q", c.Name, id)
			}
			if seen[id] {
				t.Errorf("%s: want lists %q twice", c.Name, id)
			}
			seen[id] = true
		}
	}
}
