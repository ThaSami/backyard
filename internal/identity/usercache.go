package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/docstore"
)

// Location of the user cache in the document store (SPEC §10).
const (
	// SystemSite is the reserved docstore namespace for system data.
	SystemSite = "_system"
	// UsersCollection holds one document per user, keyed by lowercased email.
	UsersCollection = "_users"
)

const (
	// TouchInterval is the minimum time between two cache writes for the same
	// user on one instance.
	TouchInterval = 10 * time.Minute
	// touchRetry is how soon a failed write may be retried.
	touchRetry = time.Minute
	// touchTimeout bounds one cache write.
	touchTimeout = 5 * time.Second
	// sweepThreshold is the number of throttle entries above which expired
	// entries are pruned.
	sweepThreshold = 4096
)

// UserCache stores profiles of users who have used Backyard in the docstore
// (site "_system", collection "_users", doc id = lowercased email) so that
// GET /users/{email} can resolve colleagues. Writes are throttled per user.
//
// The cache never stores anonymous users, static-token principals or users
// without an email. Admin flags are not persisted (they are derived from the
// configuration at request time). A UserCache is safe for concurrent use.
type UserCache struct {
	store    docstore.Store
	fallback Directory
	log      *slog.Logger
	now      func() time.Time
	interval time.Duration

	mu        sync.Mutex
	seen      map[string]time.Time // email → last write (or attempt)
	lastSweep time.Time
}

var _ Directory = (*UserCache)(nil)

// NewUserCache returns a cache backed by store. fallback (optional) is
// consulted first (e.g. the static directory).
func NewUserCache(store docstore.Store, fallback Directory) *UserCache {
	return &UserCache{
		store:    store,
		fallback: fallback,
		log:      slog.Default(),
		now:      time.Now,
		interval: TouchInterval,
		seen:     make(map[string]time.Time),
	}
}

// Touch records u (no-op for anonymous users or users without email; throttled).
//
// The write happens synchronously (bounded by a short timeout and detached
// from ctx cancellation) at most once per TouchInterval per user and
// instance; failures are logged and retried after a minute.
func (c *UserCache) Touch(ctx context.Context, u *User) {
	if c == nil || c.store == nil || u == nil || u.Anonymous {
		return
	}
	if strings.HasPrefix(u.ID, "anon:") || strings.HasPrefix(u.ID, "token:") {
		return
	}
	email := NormalizeEmail(u.Email)
	if email == "" {
		return
	}
	now := c.now()
	c.mu.Lock()
	if last, ok := c.seen[email]; ok && now.Sub(last) < c.interval {
		c.mu.Unlock()
		return
	}
	c.seen[email] = now
	c.sweepLocked(now)
	c.mu.Unlock()

	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), touchTimeout)
	defer cancel()
	data := profileData(u, email, now)
	_, err := c.store.Put(wctx, SystemSite, UsersCollection, email, data, u.ID)
	if permanentWriteError(err) && data["extra"] != nil {
		// Extra claims are the only unbounded part of a profile: store the
		// rest rather than nothing.
		delete(data, "extra")
		_, err = c.store.Put(wctx, SystemSite, UsersCollection, email, data, u.ID)
	}
	if err == nil {
		return
	}
	c.log.Warn("identity: user cache write failed", "user", email, "error", err)
	if permanentWriteError(err) {
		return // retrying sooner would fail the same way
	}
	c.mu.Lock()
	// Allow a retry after touchRetry instead of a full interval.
	c.seen[email] = now.Add(-c.interval + touchRetry)
	c.mu.Unlock()
}

// permanentWriteError reports whether a docstore write failed because of the
// document itself (invalid or too large) rather than the store.
func permanentWriteError(err error) bool {
	return errors.Is(err, docstore.ErrInvalid) || errors.Is(err, docstore.ErrTooLarge)
}

// sweepLocked prunes expired throttle entries when the map grows large.
func (c *UserCache) sweepLocked(now time.Time) {
	if len(c.seen) < sweepThreshold || now.Sub(c.lastSweep) < c.interval {
		return
	}
	c.lastSweep = now
	for k, t := range c.seen {
		if now.Sub(t) >= c.interval {
			delete(c.seen, k)
		}
	}
}

// Lookup implements Directory.
//
// The fallback directory is consulted first; the cached profile (if any) is
// then enriched with the directory entry, so directory data wins over data
// last seen from the IdP. It returns (nil, nil) for unknown emails.
func (c *UserCache) Lookup(ctx context.Context, email string) (*User, error) {
	if c == nil {
		return nil, nil
	}
	// The email usually comes from a URL (GET /users/{email}); anything that
	// is not a plain address cannot be cached, so it is simply unknown.
	key := NormalizeEmail(email)
	if key == "" {
		return nil, nil
	}
	var fromDir *User
	if c.fallback != nil {
		u, err := c.fallback.Lookup(ctx, key)
		if err != nil {
			c.log.Warn("identity: directory lookup failed", "user", key, "error", err)
		} else if u != nil {
			fromDir = u.Clone()
		}
	}
	if c.store == nil {
		return fromDir, nil
	}
	doc, err := c.store.Get(ctx, SystemSite, UsersCollection, key)
	switch {
	case errors.Is(err, docstore.ErrNotFound):
		return fromDir, nil
	case err != nil:
		if fromDir != nil {
			c.log.Warn("identity: user cache lookup failed", "user", key, "error", err)
			return fromDir, nil
		}
		return nil, fmt.Errorf("identity: user cache lookup: %w", err)
	}
	cached := userFromDoc(key, doc)
	if cached == nil {
		return fromDir, nil
	}
	cached.Enrich(fromDir)
	return cached, nil
}

// profileData is the stored document body for u.
func profileData(u *User, email string, now time.Time) map[string]any {
	d := map[string]any{
		"email":     email,
		"name":      u.Name,
		"last_seen": now.UTC().Format(time.RFC3339),
	}
	put := func(k, v string) {
		if v != "" {
			d[k] = v
		}
	}
	put("title", u.Title)
	put("team", u.Team)
	put("slack", u.Slack)
	put("avatar_url", u.AvatarURL)
	if len(u.Groups) > 0 {
		g := make([]any, len(u.Groups))
		for i, s := range u.Groups {
			g[i] = s
		}
		d["groups"] = g
	}
	if len(u.Extra) > 0 {
		d["extra"] = cloneValue(u.Extra)
	}
	return d
}

// userFromDoc converts a cached document back into a User.
func userFromDoc(email string, doc *docstore.Doc) *User {
	if doc == nil {
		return nil
	}
	data := doc.Data
	str := func(k string) string {
		s, _ := data[k].(string)
		return strings.TrimSpace(s)
	}
	u := &User{
		ID:        email,
		Email:     email,
		Name:      str("name"),
		Title:     str("title"),
		Team:      str("team"),
		Slack:     str("slack"),
		AvatarURL: str("avatar_url"),
		Groups:    ParseList(data["groups"]),
	}
	if ex, ok := data["extra"].(map[string]any); ok && len(ex) > 0 {
		u.Extra, _ = cloneValue(ex).(map[string]any)
	}
	if u.Name == "" {
		u.Name = emailLocalPart(email)
	}
	return u
}
