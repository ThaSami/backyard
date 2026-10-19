package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

// listingHarness returns a hub with sites.listing set to listing,
// root@acme.com an admin, a dev site "local", and these sites, each with a
// preview: alices (owned by alice), bobs (bob), legacy (no owners, created
// by alice), shadow (bob's, shadowed by a dev site of the same name) and
// synced (files only, no meta).
func listingHarness(t *testing.T, listing string) *harness {
	t.Helper()
	h := newHarness(t,
		withConfig(func(c *config.Config) {
			c.Sites.Listing = listing
			c.Sites.Ownership = "owner"
			c.Auth.Admins = []string{"Root@Acme.com"}
		}),
		func(h *harness, d *Deps) { d.DevSites = map[string]string{"local": t.TempDir(), "shadow": t.TempDir()} })
	ctx := context.Background()
	now := time.Now().UTC()
	for _, m := range []sites.Meta{
		{Name: "alices", Owners: []string{"alice@acme.com"}, CreatedBy: "alice@acme.com"},
		{Name: "bobs", Owners: []string{"bob@acme.com"}, CreatedBy: "alice@acme.com"},
		{Name: "legacy", Owners: []string{}, CreatedBy: "ALICE@acme.com"},
		{Name: "shadow", Owners: []string{"bob@acme.com"}, CreatedBy: "bob@acme.com"},
	} {
		h.deploy(m.Name, map[string]string{"index.html": "x", "preview.svg": previewSVG})
		m.Preview, m.CreatedAt, m.UpdatedAt, m.FileCount = "preview.svg", now, now, 2
		if err := h.srv.deps.Sites.PutMeta(ctx, &m); err != nil {
			t.Fatal(err)
		}
	}
	h.deploy("synced", map[string]string{"index.html": "x"})
	return h
}

// hubSiteNames returns the sorted names of the hub's site list.
func hubSiteNames(t *testing.T, h *harness) []string {
	t.Helper()
	w := h.api("GET", "https://backyard.test/__backyard/api/sites", nil)
	expectStatus(t, w, http.StatusOK)
	var resp struct {
		Sites []sites.Meta `json:"sites"`
	}
	mustJSON(t, w.Body.Bytes(), &resp)
	if resp.Sites == nil {
		t.Fatalf("sites is not an array: %s", w.Body.String())
	}
	names := make([]string, 0, len(resp.Sites))
	for _, m := range resp.Sites {
		names = append(names, m.Name)
	}
	slices.Sort(names)
	return names
}

func TestHubSitesListing(t *testing.T) {
	var (
		bob  = &identity.User{ID: "bob@acme.com", Email: "bob@acme.com", Name: "Bob"}
		root = &identity.User{ID: "root@acme.com", Email: "root@acme.com", Name: "Root"}
		// A guest whose id is in auth.admins is flagged admin by the
		// authenticator, but is never listed every site.
		flagged = &identity.User{ID: guest.ID, Name: guest.Name, Anonymous: true, Admin: true}
	)
	every := []string{"alices", "bobs", "legacy", "local", "shadow", "synced"}
	cases := []struct {
		name       string
		u          *identity.User
		all, owner []string
	}{
		// Dev sites (local, shadow) are always listed.
		{"owner and creator", alice, every, []string{"alices", "legacy", "local", "shadow"}},
		{"owner", bob, every, []string{"bobs", "local", "shadow"}},
		{"admin", root, every, every},
		{"guest", guest, every, []string{"local", "shadow"}},
		{"guest flagged admin", flagged, every, []string{"local", "shadow"}},
	}
	for _, listing := range []string{"all", "owners"} {
		h := listingHarness(t, listing)
		for _, tc := range cases {
			want, name := tc.all, listing+"/"+tc.name
			if listing == "owners" {
				want = tc.owner
			}
			h.auth.set(tc.u, nil)
			if got := hubSiteNames(t, h); !slices.Equal(got, want) {
				t.Errorf("%s: listed %v, want %v", name, got, want)
			}
			// The preview route answers for the listed sites only, and a
			// hidden site answers like one without a preview.
			for _, site := range []string{"alices", "bobs", "legacy"} {
				w := h.get(previewURL(site))
				if slices.Contains(want, site) {
					expectStatus(t, w, http.StatusOK)
				} else {
					expectError(t, w, http.StatusNotFound, "not_found")
					if !strings.Contains(w.Body.String(), `site \"`+site+`\" has no preview`) {
						t.Errorf("%s: %s preview 404 = %s", name, site, w.Body.String())
					}
					expectStatus(t, h.serve(req{method: http.MethodHead, url: previewURL(site)}), http.StatusNotFound)
				}
			}
		}
	}
}

func TestHubSitesListingEmpty(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) {
		c.Sites.Listing = "owners"
		c.Sites.Ownership = "owner"
	}))
	h.deploy("synced", map[string]string{"index.html": "x"})
	h.auth.set(guest, nil)
	w := h.api("GET", "https://backyard.test/__backyard/api/sites", nil)
	if strings.TrimSpace(w.Body.String()) != `{"sites":[]}` {
		t.Fatalf("guest sites = %s", w.Body.String())
	}
	// The site itself stays reachable: listing is not access control.
	expectStatus(t, h.get("https://synced.backyard.test/"), http.StatusOK)
}
