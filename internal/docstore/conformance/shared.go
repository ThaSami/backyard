package conformance

import (
	_ "embed" // querycases.json
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/thasami/backyard/internal/docstore"
)

// QueryCasesJSON is the shared, language-neutral query-semantics suite
// (querycases.json in this directory). It pins SPEC §5.2 with wire-format
// queries and expected results so that every implementation of the Backyard
// database — the Go backends here, the Cloudflare edition's SiteDO and the
// black-box conformance/ suite — can run the very same cases.
//
// Format: "collections" maps a collection name to its documents
// ({"id", "data"}), which must be created in the listed order; each entry of
// "cases" names a collection, a Query in the JSON shape of
// POST /__backyard/api/db/{c}/query, the expected document ids in order
// ("want") and the expected "has_more".
//
//go:embed querycases.json
var QueryCasesJSON []byte

// QueryCases is the decoded form of QueryCasesJSON.
type QueryCases struct {
	Version     int                  `json:"version"`
	Collections map[string][]CaseDoc `json:"collections"`
	Cases       []QueryCase          `json:"cases"`
}

// CaseDoc is one fixture document.
type CaseDoc struct {
	ID   string         `json:"id"`
	Data map[string]any `json:"data"`
}

// QueryCase is one query with its expected result.
type QueryCase struct {
	Name       string         `json:"name"`
	Collection string         `json:"collection"`
	Query      docstore.Query `json:"query"`
	Want       []string       `json:"want"`
	HasMore    bool           `json:"has_more"`
}

// LoadQueryCases decodes QueryCasesJSON.
func LoadQueryCases() (*QueryCases, error) {
	var qc QueryCases
	if err := json.Unmarshal(QueryCasesJSON, &qc); err != nil {
		return nil, fmt.Errorf("conformance: decoding querycases.json: %w", err)
	}
	for _, c := range qc.Cases {
		if _, ok := qc.Collections[c.Collection]; !ok {
			return nil, fmt.Errorf("conformance: case %q uses unknown collection %q", c.Name, c.Collection)
		}
	}
	return &qc, nil
}

// testSharedQueryCases runs querycases.json against the store and against the
// reference evaluator.
func testSharedQueryCases(t *testing.T, s docstore.Store) {
	qc, err := LoadQueryCases()
	if err != nil {
		t.Fatal(err)
	}
	if len(qc.Cases) == 0 {
		t.Fatal("querycases.json has no cases")
	}
	site := newSite(t)
	created := make(map[string][]*docstore.Doc, len(qc.Collections))
	names := make([]string, 0, len(qc.Collections))
	for name := range qc.Collections {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, d := range qc.Collections[name] {
			created[name] = append(created[name], mustCreate(t, s, site, name, d.ID, d.Data, "conformance"))
		}
	}
	for _, tc := range qc.Cases {
		q := tc.Query
		if err := q.Normalize(1000); err != nil {
			t.Errorf("%s: Normalize: %v", tc.Name, err)
			continue
		}
		want := tc.Want
		if want == nil {
			want = []string{}
		}
		p, err := s.Query(newCtx(t), site, tc.Collection, q)
		if err != nil {
			t.Errorf("%s: Query: %v", tc.Name, err)
			continue
		}
		if got := ids(p); !slices.Equal(got, want) || p.HasMore != tc.HasMore {
			t.Errorf("%s: got %v has_more=%v, want %v has_more=%v", tc.Name, got, p.HasMore, want, tc.HasMore)
		}
		pq, err := docstore.PrepareQuery(q)
		if err != nil {
			t.Errorf("%s: PrepareQuery: %v", tc.Name, err)
			continue
		}
		ref := docstore.EvalQuery(slices.Clone(created[tc.Collection]), pq)
		if got := ids(ref); !slices.Equal(got, want) || ref.HasMore != tc.HasMore {
			t.Errorf("%s: reference evaluator got %v has_more=%v, want %v has_more=%v", tc.Name, got, ref.HasMore, want, tc.HasMore)
		}
	}
}
