package sites

import (
	"strings"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
)

// Values of sites.listing (SPEC §7.2): which sites the site directories
// list — the hub's GET /__backyard/api/sites (and its preview route), the
// gateway's GET /sites and the connector's backyard_list_sites. Listing is
// not access control: lookups by name, the files and the site itself stay
// reachable as without it.
const (
	ListingAll    = "all"    // every site is listed to everyone who may list
	ListingOwners = "owners" // a site is listed to its owners and to admins
)

// Listing returns cfg's sites.listing, "all" when unset.
func Listing(cfg *config.Config) string {
	if cfg != nil && cfg.Sites.Listing == ListingOwners {
		return ListingOwners
	}
	return ListingAll
}

// Listed reports whether m appears in u's site listings. With
// sites.listing "all" every site does. With "owners" a site is listed only
// to a caller who is not anonymous and is an admin (the gateway's rule:
// u.Admin or auth.admins/auth.admin_groups), one of the site's owners (by id
// or email, case-insensitive), or — while the site has no owners — the one
// who created it (created_by, by id or email, case-insensitive), so that
// sites from before the option stay listed to their creator.
func Listed(cfg *config.Config, u *identity.User, m *Meta) bool {
	if Listing(cfg) != ListingOwners {
		return true
	}
	if u == nil || u.Anonymous || m == nil {
		return false
	}
	if u.Admin || cfg.Auth.IsAdmin(u.ID, u.Email, u.Groups) {
		return true
	}
	if len(m.Owners) > 0 {
		return m.IsOwner(u.ID, u.Email)
	}
	c := m.CreatedBy
	return c != "" && ((u.ID != "" && strings.EqualFold(c, u.ID)) || (u.Email != "" && strings.EqualFold(c, u.Email)))
}

// FilterListed returns the sites of list that are Listed to u, in order. It
// returns list itself when every site is listed, and a new slice otherwise.
func FilterListed(cfg *config.Config, u *identity.User, list []Meta) []Meta {
	if Listing(cfg) != ListingOwners {
		return list
	}
	out := make([]Meta, 0, len(list))
	for i := range list {
		if Listed(cfg, u, &list[i]) {
			out = append(out, list[i])
		}
	}
	return out
}
