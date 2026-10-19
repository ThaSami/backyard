package sites_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

// listingConfig returns a validated config with sites.listing set to
// listing ("" = unset), root@x.com (and one guest id) in auth.admins and the
// group ops in auth.admin_groups.
func listingConfig(t *testing.T, listing string) *config.Config {
	t.Helper()
	yaml := "server:\n  base_domain: backyard.test\n  secret: 0123456789abcdef0123456789abcdef\n" +
		"auth:\n  mode: oidc\n  oidc:\n    issuer: https://idp.test\n    client_id: c\n    client_secret: s\n" +
		"  admins: [Root@X.com, anon:00000000000000aa]\n  admin_groups: [ops]\n"
	if listing != "" {
		yaml += "sites:\n  listing: " + listing + "\n"
	}
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestListing(t *testing.T) {
	if got := sites.Listing(nil); got != "all" {
		t.Fatalf("Listing(nil) = %q", got)
	}
	if got := sites.Listing(&config.Config{}); got != "all" {
		t.Fatalf("Listing(unset) = %q", got)
	}
	if got := sites.Listing(listingConfig(t, "")); got != "all" {
		t.Fatalf("Listing(default) = %q", got)
	}
	if got := sites.Listing(listingConfig(t, "owners")); got != "owners" {
		t.Fatalf("Listing(owners) = %q", got)
	}
}

func TestListed(t *testing.T) {
	var (
		bob      = &identity.User{ID: "bob@x.com", Email: "bob@x.com", Name: "Bob"}
		bobSub   = &identity.User{ID: "sub:Bob123", Email: "Bob@X.com", Name: "Bob"}
		carol    = &identity.User{ID: "carol@x.com", Email: "carol@x.com", Name: "Carol"}
		noEmail  = &identity.User{ID: "sub:abc", Name: "No Email"}
		root     = &identity.User{ID: "root@x.com", Email: "root@x.com"}                          // auth.admins (other case)
		flagged  = &identity.User{ID: "dave@x.com", Email: "dave@x.com", Admin: true}             // admin from the identity
		grouped  = &identity.User{ID: "erin@x.com", Email: "erin@x.com", Groups: []string{"OPS"}} // auth.admin_groups
		deployer = &identity.User{ID: "token:deploy", Name: "deploy", Admin: true}                // static token, admin: true
		ci       = &identity.User{ID: "token:ci", Name: "ci"}                                     // static token, not admin
		guest    = &identity.User{ID: "anon:0123456789abcdef", Anonymous: true}
		// A guest whose id is in auth.admins reports admin on /me, but is
		// never an admin to the gateway, nor here.
		anonAdmin = &identity.User{ID: "anon:00000000000000aa", Anonymous: true, Admin: true}
	)
	var (
		bobs      = &sites.Meta{Name: "bobs", Owners: []string{"bob@x.com"}, CreatedBy: "bob@x.com"}
		bobsBySub = &sites.Meta{Name: "bobs-sub", Owners: []string{"sub:bob123"}, CreatedBy: "carol@x.com"}
		shared    = &sites.Meta{Name: "shared", Owners: []string{"carol@x.com", "BOB@X.COM"}}
		cis       = &sites.Meta{Name: "cis", Owners: []string{"token:ci"}, CreatedBy: "token:ci"}
		legacy    = &sites.Meta{Name: "legacy", Owners: []string{}, CreatedBy: "bob@x.com"}
		legacySub = &sites.Meta{Name: "legacy-sub", CreatedBy: "SUB:ABC"}
		claimed   = &sites.Meta{Name: "claimed", Owners: []string{"carol@x.com"}, CreatedBy: "bob@x.com"}
		synced    = &sites.Meta{Name: "synced", Owners: []string{}} // direct sync without meta: no creator
		byGuest   = &sites.Meta{Name: "by-guest", Owners: []string{}, CreatedBy: "anon:0123456789abcdef"}
		guestSite = &sites.Meta{Name: "guest-owned", Owners: []string{"anon:0123456789abcdef"}}
	)
	all := []*sites.Meta{bobs, bobsBySub, shared, cis, legacy, legacySub, claimed, synced, byGuest, guestSite}
	cases := []struct {
		name string
		u    *identity.User
		want []*sites.Meta // listed with sites.listing: owners
	}{
		{"owner by id and email", bob, []*sites.Meta{bobs, shared, legacy}},
		{"owner by id, email in another case", bobSub, []*sites.Meta{bobs, bobsBySub, shared, legacy}},
		{"other owner", carol, []*sites.Meta{shared, claimed}},
		{"creator of an ownerless site without email", noEmail, []*sites.Meta{legacySub}},
		{"admin from auth.admins", root, all},
		{"admin from the identity", flagged, all},
		{"admin from auth.admin_groups", grouped, all},
		{"static token with admin: true", deployer, all},
		{"static token without admin", ci, []*sites.Meta{cis}},
		{"guest", guest, nil},
		{"guest flagged admin", anonAdmin, nil},
		{"no user", nil, nil},
	}
	owners, every := listingConfig(t, "owners"), listingConfig(t, "all")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, m := range all {
				want := slices.Contains(tc.want, m)
				if got := sites.Listed(owners, tc.u, m); got != want {
					t.Errorf("owners: Listed(%s) = %v, want %v", m.Name, got, want)
				}
				if !sites.Listed(every, tc.u, m) {
					t.Errorf("all: %s not listed", m.Name)
				}
			}
		})
	}
	if sites.Listed(owners, bob, nil) {
		t.Error("a nil site is listed")
	}
	// A site with an empty creator is not listed to a user without an id.
	if sites.Listed(owners, &identity.User{Name: "Nobody"}, synced) {
		t.Error("an ownerless site without creator is listed to a user without id")
	}
}

func TestFilterListed(t *testing.T) {
	list := []sites.Meta{
		{Name: "a", Owners: []string{"bob@x.com"}},
		{Name: "b", Owners: []string{"carol@x.com"}},
		{Name: "c", Owners: []string{}, CreatedBy: "Bob@x.com"},
		{Name: "d", Owners: []string{}},
	}
	bob := &identity.User{ID: "bob@x.com", Email: "bob@x.com"}
	names := func(l []sites.Meta) string {
		var n []string
		for _, m := range l {
			n = append(n, m.Name)
		}
		return strings.Join(n, ",")
	}
	if got := sites.FilterListed(listingConfig(t, "all"), bob, list); names(got) != "a,b,c,d" || &got[0] != &list[0] {
		t.Fatalf("all: %s (want the list itself)", names(got))
	}
	got := sites.FilterListed(listingConfig(t, "owners"), bob, list)
	if names(got) != "a,c" {
		t.Fatalf("owners: %s", names(got))
	}
	if names(list) != "a,b,c,d" {
		t.Fatalf("the input changed: %s", names(list))
	}
	got = sites.FilterListed(listingConfig(t, "owners"), &identity.User{ID: "anon:1", Anonymous: true}, list)
	if got == nil || len(got) != 0 {
		t.Fatalf("guest: %#v (want an empty, non-nil list)", got)
	}
}
