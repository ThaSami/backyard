package mcpremote

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

// listResult is the structured result of backyard_list_sites.
type listResult struct {
	Sites []struct {
		Name string `json:"name"`
	} `json:"sites"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
}

func (r listResult) names() []string {
	n := make([]string, 0, len(r.Sites))
	for _, s := range r.Sites {
		n = append(n, s.Name)
	}
	slices.Sort(n)
	return n
}

func TestListSitesListing(t *testing.T) {
	every := []string{"legacy", "lunch-poll", "synced", "zeta"}
	cases := []struct {
		token      string
		all, owner []string
	}{
		{aliceToken, every, []string{"legacy", "lunch-poll"}}, // owner, and creator of the ownerless legacy
		{bobToken, every, []string{"zeta"}},
		{adminToken, every, every},
		{guestToken, every, []string{}},
	}
	for _, listing := range []string{"all", "owners"} {
		e := newTenv(t, tenvOption{cfg: func(c *config.Config) {
			c.Sites.Ownership = "owner"
			c.Sites.Listing = listing
		}})
		e.toolOK(aliceToken, "backyard_publish", map[string]any{"site": "lunch-poll", "files": files("index.html", "<title>Lunch</title>")}, nil)
		e.toolOK(bobToken, "backyard_publish", map[string]any{"site": "zeta", "files": files("index.html", "<title>Zeta</title>")}, nil)
		e.putObject("synced", "index.html", []byte("raw")) // no meta document: no owners, no creator
		now := time.Now().UTC()
		if err := e.reg.PutMeta(context.Background(), &sites.Meta{Name: "legacy", CreatedAt: now, CreatedBy: alice.ID, UpdatedAt: now, Owners: []string{}}); err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			want := tc.all
			if listing == "owners" {
				want = tc.owner
			}
			var out listResult
			text := e.toolOK(tc.token, "backyard_list_sites", nil, &out)
			if got := out.names(); !slices.Equal(got, want) || out.Total != len(want) || out.Truncated {
				t.Errorf("%s %s: %s, want %v", listing, tc.token, text, want)
			}
		}
		// The query matches the listed sites only.
		var out listResult
		e.toolOK(aliceToken, "backyard_list_sites", map[string]any{"query": "zet"}, &out)
		if wantTotal := map[string]int{"all": 1, "owners": 0}[listing]; out.Total != wantTotal {
			t.Errorf("%s: query zet total = %d, want %d", listing, out.Total, wantTotal)
		}
		if listing == "owners" {
			if text := e.toolOK(guestToken, "backyard_list_sites", nil, nil); text != `{"sites":[],"total":0,"truncated":false}` {
				t.Errorf("guest = %s", text)
			}
			// The read tools still answer for a site that is not listed.
			e.toolOK(bobToken, "backyard_site_files", map[string]any{"site": "lunch-poll"}, nil)
			e.toolOK(bobToken, "backyard_read_site_file", map[string]any{"site": "lunch-poll", "path": "index.html"}, nil)
		}
	}
}

// total and truncated count the listed sites only.
func TestListSitesListingCount(t *testing.T) {
	e := newTenv(t, tenvOption{cfg: func(c *config.Config) {
		c.Sites.Ownership = "owner"
		c.Sites.Listing = "owners"
	}})
	for i := range 205 {
		owner := "someone@acme.com"
		if i%100 == 0 {
			owner = alice.ID
		}
		if err := e.reg.PutMeta(context.Background(), &sites.Meta{Name: fmt.Sprintf("s%03d", i), UpdatedAt: time.Unix(int64(i), 0), Owners: []string{owner}}); err != nil {
			t.Fatal(err)
		}
	}
	var out listResult
	text := e.toolOK(aliceToken, "backyard_list_sites", nil, &out)
	if !slices.Equal(out.names(), []string{"s000", "s100", "s200"}) || out.Total != 3 || out.Truncated {
		t.Fatalf("alice = %s", text)
	}
	e.toolOK(adminToken, "backyard_list_sites", nil, &out)
	if out.Total != 205 || !out.Truncated || len(out.Sites) != 200 {
		t.Fatalf("admin: total %d truncated %v len %d", out.Total, out.Truncated, len(out.Sites))
	}
}
