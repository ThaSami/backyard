package identity

import (
	"context"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
)

// MapClaims builds a User from IdP claims according to mapping. The id is the
// lowercased email when present, else "sub:"+sub. Admin is NOT set here.
// STUB: replaced by the identity implementation.
func MapClaims(claims map[string]any, mapping config.ClaimsMapping, extra []string) *User {
	panic("identity: not implemented")
}

// NewDirectory loads the configured static directory (nil, nil when type is "").
// STUB: replaced by the identity implementation.
func NewDirectory(cfg config.DirectoryConfig) (Directory, error) {
	panic("identity: not implemented")
}

// UserCache stores profiles of users who have used Backyard in the docstore
// (site "_system", collection "_users", doc id = lowercased email) so that
// GET /users/{email} can resolve colleagues. Writes are throttled per user.
type UserCache struct {
	store docstore.Store
}

// NewUserCache returns a cache backed by store. fallback (optional) is
// consulted first (e.g. the static directory).
// STUB: replaced by the identity implementation.
func NewUserCache(store docstore.Store, fallback Directory) *UserCache {
	panic("identity: not implemented")
}

// Touch records u (no-op for anonymous users or users without email; throttled).
// STUB: replaced by the identity implementation.
func (c *UserCache) Touch(ctx context.Context, u *User) {
	panic("identity: not implemented")
}

// Lookup implements Directory.
// STUB: replaced by the identity implementation.
func (c *UserCache) Lookup(ctx context.Context, email string) (*User, error) {
	panic("identity: not implemented")
}
