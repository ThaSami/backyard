package docstore

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDocMarshalJSONWireShape(t *testing.T) {
	ts := time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.FixedZone("x", 3600))
	d := &Doc{ID: "01J", Data: map[string]any{"title": "Hello", "n": 1.0}, CreatedAt: ts, UpdatedAt: ts,
		CreatedBy: "a@b.c", UpdatedBy: "d@e.f", Version: 2}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"id": "01J", "title": "Hello", "n": 1.0,
		"_meta": map[string]any{
			"created_at": "2026-03-04T04:06:07.123456Z", "updated_at": "2026-03-04T04:06:07.123456Z",
			"created_by": "a@b.c", "updated_by": "d@e.f", "version": 2.0,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("wire shape = %v\nwant %v", got, want)
	}
	// The _meta.created_at string a client sees filters back to the doc.
	meta := got["_meta"].(map[string]any)
	if !MatchFilter(d, Filter{Field: "_meta.created_at", Op: OpEq, Value: meta["created_at"]}) {
		t.Error("filtering by the rendered created_at does not match the document")
	}
}
