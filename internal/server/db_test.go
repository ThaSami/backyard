package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/realtime"
)

const dbURL = "https://foo.backyard.test/__backyard/api/db"

func TestDBCreateGetFlattened(t *testing.T) {
	h := newHarness(t)
	w := h.api("POST", dbURL+"/posts", map[string]any{"title": "Hello", "votes": 3, "_meta": map[string]any{"x": 1}})
	expectStatus(t, w, http.StatusCreated)
	doc := decode[map[string]any](t, w)
	id, _ := doc["id"].(string)
	if len(id) != 26 {
		t.Fatalf("generated id = %q", id)
	}
	if doc["title"] != "Hello" || doc["votes"] != float64(3) {
		t.Fatalf("doc = %v", doc)
	}
	meta, _ := doc["_meta"].(map[string]any)
	if meta["created_by"] != "alice@acme.com" || meta["version"] != float64(1) || meta["created_at"] == nil {
		t.Fatalf("_meta = %v", meta)
	}
	if _, ok := meta["x"]; ok {
		t.Fatal("_meta from body stored")
	}

	w = h.api("GET", dbURL+"/posts/"+id, nil)
	expectStatus(t, w, 200)
	if got := decode[map[string]any](t, w); got["title"] != "Hello" || got["id"] != id {
		t.Fatalf("get = %v", got)
	}

	// Explicit id; duplicate → 409.
	w = h.api("POST", dbURL+"/posts", map[string]any{"id": "my-id:1", "a": 1})
	expectStatus(t, w, 201)
	if got := decode[map[string]any](t, w); got["id"] != "my-id:1" {
		t.Fatalf("id = %v", got["id"])
	}
	w = h.api("POST", dbURL+"/posts", map[string]any{"id": "my-id:1"})
	expectError(t, w, http.StatusConflict, "conflict")

	ev := h.hub.published()
	if len(ev) != 2 || ev[0].site != "foo" || ev[0].ev.Op != realtime.OpCreate || ev[0].ev.Collection != "posts" ||
		ev[0].ev.DocID != id || ev[0].ev.Doc == nil || ev[0].ev.Doc.Data["title"] != "Hello" {
		t.Fatalf("published = %+v", ev)
	}
}

func TestDBValidation(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name   string
		method string
		url    string
		body   any
		status int
		code   string
	}{
		{"system collection", "POST", dbURL + "/_files", map[string]any{"a": 1}, 400, "bad_request"},
		{"system collection get", "GET", dbURL + "/_users/alice", nil, 400, "bad_request"},
		{"bad collection", "POST", dbURL + "/bad%20name", map[string]any{}, 400, "bad_request"},
		{"long collection", "GET", dbURL + "/" + strings.Repeat("a", 65), nil, 400, "bad_request"},
		{"bad id", "GET", dbURL + "/posts/bad%20id", nil, 400, "bad_request"},
		{"bad body id", "POST", dbURL + "/posts", map[string]any{"id": "has space"}, 400, "bad_request"},
		{"numeric body id", "POST", dbURL + "/posts", map[string]any{"id": 5}, 400, "bad_request"},
		{"array body", "POST", dbURL + "/posts", "[1,2]", 400, "bad_request"},
		{"null body", "POST", dbURL + "/posts", "null", 400, "bad_request"},
		{"empty body", "POST", dbURL + "/posts", "", 400, "bad_request"},
		{"invalid json", "POST", dbURL + "/posts", "{nope", 400, "bad_request"},
		{"trailing data", "POST", dbURL + "/posts", `{"a":1} {"b":2}`, 400, "bad_request"},
		{"put id mismatch", "PUT", dbURL + "/posts/a", map[string]any{"id": "b"}, 400, "bad_request"},
		{"bad query field", "POST", dbURL + "/posts/query", map[string]any{"where": []any{map[string]any{"field": "a..b", "op": "==", "value": 1}}}, 400, "bad_request"},
		{"bad query op", "POST", dbURL + "/posts/query", map[string]any{"where": []any{map[string]any{"field": "a", "op": "~", "value": 1}}}, 400, "bad_request"},
		{"unknown query key", "POST", dbURL + "/posts/query", map[string]any{"orderBy": "x"}, 400, "bad_request"},
		{"bad list limit", "GET", dbURL + "/posts?limit=-1", nil, 400, "bad_request"},
		{"bad list offset", "GET", dbURL + "/posts?offset=x", nil, 400, "bad_request"},
		{"bad order dir", "GET", dbURL + "/posts?order=a:up", nil, 400, "bad_request"},
		{"bad order field", "GET", dbURL + "/posts?order=_meta.secret", nil, 400, "bad_request"},
		{"patch missing", "PATCH", dbURL + "/posts/missing", map[string]any{"a": 1}, 404, "not_found"},
		{"get missing", "GET", dbURL + "/posts/missing", nil, 404, "not_found"},
		{"method", "PUT", dbURL + "/posts", map[string]any{}, 405, "bad_request"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := h.api(tc.method, tc.url, tc.body)
			expectError(t, w, tc.status, tc.code)
		})
	}
	if ev := h.hub.published(); len(ev) != 0 {
		t.Fatalf("failed writes published events: %+v", ev)
	}
}

func TestDBListAndQuery(t *testing.T) {
	h := newHarness(t)
	for i := range 5 {
		w := h.api("POST", dbURL+"/items", map[string]any{"id": fmt.Sprintf("i%d", i), "n": i, "even": i%2 == 0})
		expectStatus(t, w, 201)
	}
	type page struct {
		Docs    []map[string]any `json:"docs"`
		HasMore bool             `json:"has_more"`
	}
	ids := func(p page) string {
		var out []string
		for _, d := range p.Docs {
			out = append(out, d["id"].(string))
		}
		return strings.Join(out, ",")
	}
	tests := []struct {
		name    string
		method  string
		url     string
		body    any
		want    string
		hasMore bool
	}{
		{"default order", "GET", dbURL + "/items", nil, "i0,i1,i2,i3,i4", false},
		{"limit", "GET", dbURL + "/items?limit=2", nil, "i0,i1", true},
		{"offset", "GET", dbURL + "/items?limit=2&offset=4", nil, "i4", false},
		{"order desc", "GET", dbURL + "/items?order=n:desc", nil, "i4,i3,i2,i1,i0", false},
		{"order multi", "GET", dbURL + "/items?order=even:desc,n:desc", nil, "i4,i2,i0,i3,i1", false},
		{"order default dir", "GET", dbURL + "/items?order=n", nil, "i0,i1,i2,i3,i4", false},
		{"orderBy and order", "GET", dbURL + "/items?orderBy=n&order=desc&limit=2", nil, "i4,i3", true},
		{"order_by and dir", "GET", dbURL + "/items?order_by=even,n&dir=desc", nil, "i4,i2,i0,i3,i1", false},
		{"query where", "POST", dbURL + "/items/query", map[string]any{
			"where":    []any{map[string]any{"field": "n", "op": ">=", "value": 2}},
			"order_by": []any{map[string]any{"field": "n", "dir": "desc"}},
			"limit":    2,
		}, "i4,i3", true},
		{"query in", "POST", dbURL + "/items/query", map[string]any{
			"where": []any{map[string]any{"field": "n", "op": "in", "value": []any{1, 3}}},
		}, "i1,i3", false},
		{"empty collection", "GET", dbURL + "/empty", nil, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := h.api(tc.method, tc.url, tc.body)
			expectStatus(t, w, 200)
			if !strings.Contains(w.Body.String(), `"docs":[`) {
				t.Fatalf("docs must be an array: %s", w.Body.String())
			}
			p := decode[page](t, w)
			if got := ids(p); got != tc.want || p.HasMore != tc.hasMore {
				t.Fatalf("got %q has_more=%v, want %q has_more=%v", got, p.HasMore, tc.want, tc.hasMore)
			}
		})
	}
	// The max_list cap applies.
	h.cfg.Database.MaxList = 3
	w := h.api("GET", dbURL+"/items?limit=50", nil)
	if p := decode[page](t, w); len(p.Docs) != 3 || !p.HasMore {
		t.Fatalf("capped page = %d has_more=%v", len(p.Docs), p.HasMore)
	}
}

func TestDBPutPatchDelete(t *testing.T) {
	h := newHarness(t)
	w := h.api("PUT", dbURL+"/notes/n1", map[string]any{"text": "a", "tags": []any{"x"}})
	expectStatus(t, w, 200)
	if d := decode[map[string]any](t, w); d["_meta"].(map[string]any)["version"] != float64(1) {
		t.Fatalf("put create = %v", d)
	}
	w = h.api("PUT", dbURL+"/notes/n1", map[string]any{"id": "n1", "text": "b"})
	expectStatus(t, w, 200)
	d := decode[map[string]any](t, w)
	if d["text"] != "b" || d["tags"] != nil || d["_meta"].(map[string]any)["version"] != float64(2) {
		t.Fatalf("put replace = %v", d)
	}
	w = h.api("PATCH", dbURL+"/notes/n1", map[string]any{"text": nil, "done": true})
	expectStatus(t, w, 200)
	d = decode[map[string]any](t, w)
	if _, ok := d["text"]; ok || d["done"] != true {
		t.Fatalf("patch = %v", d)
	}
	w = h.api("DELETE", dbURL+"/notes/n1", nil)
	expectStatus(t, w, http.StatusNoContent)
	w = h.api("DELETE", dbURL+"/notes/n1", nil)
	expectStatus(t, w, http.StatusNoContent)

	ev := h.hub.published()
	var ops []string
	for _, e := range ev {
		ops = append(ops, string(e.ev.Op))
		if e.ev.DocID != "n1" || e.ev.Collection != "notes" {
			t.Fatalf("event = %+v", e)
		}
	}
	if got := strings.Join(ops, ","); got != "create,update,update,delete" {
		t.Fatalf("ops = %s (second delete must not publish)", got)
	}
	if ev[3].ev.Doc != nil {
		t.Fatal("delete event carries a doc")
	}
}

func TestDBCollections(t *testing.T) {
	h := newHarness(t)
	h.api("POST", dbURL+"/b", map[string]any{"x": 1})
	h.api("POST", dbURL+"/a", map[string]any{"x": 1})
	if _, err := h.docs.Create(context.Background(), "foo", "_files", "f1", map[string]any{}, "x"); err != nil {
		t.Fatal(err)
	}
	w := h.api("GET", dbURL, nil)
	expectStatus(t, w, 200)
	if got := strings.TrimSpace(w.Body.String()); got != `{"collections":["a","b"]}` {
		t.Fatalf("collections = %s", got)
	}
	// Other sites are isolated.
	w = h.api("GET", "https://bar.backyard.test/__backyard/api/db", nil)
	if got := strings.TrimSpace(w.Body.String()); got != `{"collections":[]}` {
		t.Fatalf("collections of bar = %s", got)
	}
}

func TestDBLimitsAndErrorMapping(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.Database.MaxDocBytes = 100
		c.Database.MaxDocsPerSite = 2
	}))
	w := h.api("POST", dbURL+"/big", map[string]any{"s": strings.Repeat("x", 200)})
	e := expectError(t, w, http.StatusRequestEntityTooLarge, "too_large")
	if !strings.Contains(e.Error.Message, "100") {
		t.Fatalf("message = %q", e.Error.Message)
	}
	// Bodies far beyond the limit are cut off while reading.
	w = h.api("POST", dbURL+"/big", map[string]any{"s": strings.Repeat("x", 300<<10)})
	expectError(t, w, http.StatusRequestEntityTooLarge, "too_large")

	h.api("POST", dbURL+"/q", map[string]any{"a": 1})
	h.api("POST", dbURL+"/q", map[string]any{"a": 2})
	w = h.api("POST", dbURL+"/q", map[string]any{"a": 3})
	expectError(t, w, http.StatusRequestEntityTooLarge, "too_large")

	g := newHarness(t)
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{docstore.ErrNotFound, 404, "not_found"},
		{docstore.ErrConflict, 409, "conflict"},
		{docstore.ErrTooLarge, 413, "too_large"},
		{fmt.Errorf("wrap: %w", docstore.ErrQuota), 413, "too_large"},
		{fmt.Errorf("%w: field path %q", docstore.ErrInvalid, "x"), 400, "bad_request"},
		{context.DeadlineExceeded, 504, "upstream_error"},
		{errors.New("connection refused to db.internal"), 500, "internal"},
	}
	for _, tc := range tests {
		t.Run(tc.err.Error(), func(t *testing.T) {
			g.docs.fail("get", tc.err)
			w := g.api("GET", dbURL+"/posts/x", nil)
			e := expectError(t, w, tc.status, tc.code)
			if strings.Contains(e.Error.Message, "db.internal") {
				t.Fatalf("internal detail leaked: %q", e.Error.Message)
			}
		})
	}
	g.docs.fail("get", nil)
	if !strings.Contains(g.logText(), "db.internal") {
		t.Fatal("internal error not logged")
	}
}

func TestDBRateLimit(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.RateLimit.DBWritesPerMinute = 2
	}))
	for i := range 2 {
		w := h.api("POST", dbURL+"/r", map[string]any{"i": i})
		expectStatus(t, w, 201)
	}
	w := h.api("POST", dbURL+"/r", map[string]any{"i": 3})
	expectError(t, w, http.StatusTooManyRequests, "rate_limited")
	if ra := w.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("Retry-After = %q", ra)
	}
	// Reads are not db writes.
	w = h.api("GET", dbURL+"/r", nil)
	expectStatus(t, w, 200)
	w = h.api("POST", dbURL+"/r/query", map[string]any{})
	expectStatus(t, w, 200)
}
