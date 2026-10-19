package deploy

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

// listingEnv returns a gateway with ownership on, sites.listing set to
// listing, carol an admin through auth.admin_groups, and these sites:
// alices (owned by alice), bobs (bob), cis (the static token ci), legacy
// (no owners, created by alice) and synced (files only, no meta).
func listingEnv(t *testing.T, listing string) *env {
	t.Helper()
	e := newEnv(t, withConfig(func(c *config.Config) {
		c.Sites.Ownership = "owner"
		c.Sites.Listing = listing
		c.Auth.AdminGroups = []string{"OPS"}
	}))
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>x</title>"})
	for site, tok := range map[string]string{"alices": "alice", "bobs": "bob", "cis": "ci"} {
		if _, err := e.client(tok).Deploy(ctx, site, dir, DeployOptions{}); err != nil {
			t.Fatalf("deploy %s: %v", site, err)
		}
	}
	now := time.Now().UTC()
	if err := e.reg.PutMeta(ctx, &sites.Meta{Name: "legacy", CreatedAt: now, CreatedBy: "Alice@Acme.com", UpdatedAt: now,
		UpdatedBy: "alice@acme.com", Owners: []string{}}); err != nil {
		t.Fatal(err)
	}
	for _, site := range []string{"legacy", "synced"} {
		if _, err := e.store.Put(ctx, sites.SiteKey(site, "index.html"), strings.NewReader("x"), 1, "text/html"); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func siteNames(list []sites.Meta) []string {
	names := make([]string, 0, len(list))
	for _, m := range list {
		names = append(names, m.Name)
	}
	slices.Sort(names)
	return names
}

func TestListSitesListing(t *testing.T) {
	every := []string{"alices", "bobs", "cis", "legacy", "synced"}
	cases := []struct {
		token      string // "" = guest without a token
		all, owner []string
	}{
		{"alice", every, []string{"alices", "legacy"}}, // owner, and creator of the ownerless legacy
		{"bob", every, []string{"bobs"}},
		{"ci", every, []string{"cis"}}, // a static token without admin sees what it owns
		{"admin", every, every},        // admin from the identity
		{"carol", every, every},        // admin from auth.admin_groups
		{"guest", every, []string{}},   // a guest's API token
		{"", every, []string{}},        // a guest (auth mode none)
		{"forbidden", nil, nil},        // the gate still applies (nil: refused)
		{"bogus", nil, nil},            // an invalid token is refused
	}
	for _, listing := range []string{"all", "owners"} {
		e := listingEnv(t, listing)
		for _, tc := range cases {
			want := tc.all
			if listing == "owners" {
				want = tc.owner
			}
			name := listing + "/" + tc.token
			if want == nil {
				resp, body := e.do("GET", "/sites", tc.token, nil)
				if resp.StatusCode != 401 && resp.StatusCode != 403 {
					t.Errorf("%s: status %d: %s", name, resp.StatusCode, body)
				}
				continue
			}
			list, err := e.client(tc.token).ListSites(context.Background())
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if got := siteNames(list); !slices.Equal(got, want) {
				t.Errorf("%s: listed %v, want %v", name, got, want)
			}
		}
		// Nothing else in the answer changes: a hidden-from-everyone list
		// is still an empty array.
		_, body := e.do("GET", "/sites", "guest", nil)
		if listing == "owners" && strings.TrimSpace(string(body)) != `{"sites":[]}` {
			t.Errorf("%s: guest body = %s", listing, body)
		}
		// Lookups by name are not filtered.
		for _, site := range every {
			if _, err := e.client("bob").GetSite(context.Background(), site); err != nil {
				t.Errorf("%s: bob GET /sites/%s: %v", listing, site, err)
			}
			if _, err := e.client("bob").SiteFiles(context.Background(), site); err != nil {
				t.Errorf("%s: bob GET /sites/%s/files: %v", listing, site, err)
			}
		}
		resp, body := e.do("GET", "/sites/alices/files/index.html", "", nil)
		wantStatus(t, resp, body, 200, "")
	}
}

func TestInfoSiteListing(t *testing.T) {
	for _, tc := range []struct{ listing, want string }{{"", "all"}, {"all", "all"}, {"owners", "owners"}} {
		e := newEnv(t, withConfig(func(c *config.Config) { c.Sites.Listing = tc.listing }))
		inf, err := e.client("").Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if inf.SiteListing != tc.want || e.gw.Info().SiteListing != tc.want {
			t.Errorf("listing %q: site_listing = %q, Info() = %q, want %q", tc.listing, inf.SiteListing, e.gw.Info().SiteListing, tc.want)
		}
		_, body := e.do("GET", "/info", "", nil)
		if !strings.Contains(string(body), `"site_listing":"`+tc.want+`"`) {
			t.Errorf("listing %q: /info = %s", tc.listing, body)
		}
	}
}
