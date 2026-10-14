// Package identity defines the user profile shared by authentication, the
// identity API (backyard.id), presence and audit logging.
package identity

import (
	"context"
	"strings"
)

// User is the authenticated (or anonymous) principal of a request. Its JSON
// shape is part of the public client API (see docs/spec/SPEC.md §4).
type User struct {
	// ID is stable: lowercased email when known, else "sub:<issuer-sub>",
	// "token:<name>" for static tokens, or "anon:<16 hex>" when anonymous.
	ID        string         `json:"id"`
	Email     string         `json:"email,omitempty"`
	Name      string         `json:"name"`
	Title     string         `json:"title,omitempty"`
	Team      string         `json:"team,omitempty"`
	Slack     string         `json:"slack,omitempty"`
	AvatarURL string         `json:"avatar_url,omitempty"`
	Groups    []string       `json:"groups,omitempty"`
	Anonymous bool           `json:"anonymous"`
	Admin     bool           `json:"admin"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// Summary is the compact form embedded in realtime presence and messages.
type Summary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
}

// Summary returns the compact representation of u.
func (u *User) Summary() Summary {
	if u == nil {
		return Summary{}
	}
	return Summary{ID: u.ID, Name: u.Name, Email: u.Email, AvatarURL: u.AvatarURL}
}

// InGroup reports whether the user belongs to group g (case-insensitive).
func (u *User) InGroup(g string) bool {
	if u == nil {
		return false
	}
	for _, x := range u.Groups {
		if strings.EqualFold(x, g) {
			return true
		}
	}
	return false
}

// Actor returns the identifier recorded in created_by/updated_by fields.
func (u *User) Actor() string {
	if u == nil || u.ID == "" {
		return "unknown"
	}
	return u.ID
}

// Directory looks up people profiles (static file, user cache, …).
type Directory interface {
	// Lookup returns the profile for an email, or (nil, nil) if unknown.
	Lookup(ctx context.Context, email string) (*User, error)
}

type ctxKey struct{}

// WithUser returns a context carrying u.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// FromContext returns the user stored by WithUser, or nil.
func FromContext(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}
