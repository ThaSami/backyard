package identity

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/thasami/backyard/internal/config"
)

// Default claim names, used for fields left empty in a config.ClaimsMapping.
const (
	DefaultEmailClaim  = "email"
	DefaultNameClaim   = "name"
	DefaultTitleClaim  = "title"
	DefaultTeamClaim   = "department"
	DefaultSlackClaim  = "slack"
	DefaultAvatarClaim = "picture"
	DefaultGroupsClaim = "groups"
)

// MapClaims builds a User from IdP claims according to mapping. The id is the
// lowercased email when present, else "sub:"+sub. Admin is NOT set here.
//
// Claim names may address nested objects with dots ("custom.team") when no
// top-level claim of that exact name exists. The groups claim may be a JSON
// array or a comma-separated string. The name falls back to given_name +
// family_name, then to the local part of the email, then to
// preferred_username and finally to sub. Claims listed in extra are copied
// verbatim into User.Extra. When neither a valid email nor a sub is present
// the returned user has an empty ID, which callers must reject.
//
// When the email is read from the standard "email" claim and the claims carry
// "email_verified": false, the email is ignored (the user is identified by
// sub): an unverified address must not satisfy allowed_domains or match an
// admin entry.
func MapClaims(claims map[string]any, mapping config.ClaimsMapping, extra []string) *User {
	m := withDefaults(mapping)
	email := NormalizeEmail(claimString(claims, m.Email))
	if m.Email == DefaultEmailClaim && emailUnverified(claims) {
		email = ""
	}
	u := &User{
		Email:     email,
		Name:      claimString(claims, m.Name),
		Title:     claimString(claims, m.Title),
		Team:      claimString(claims, m.Team),
		Slack:     claimString(claims, m.Slack),
		AvatarURL: claimString(claims, m.Avatar),
		Groups:    claimStrings(claims, m.Groups),
	}
	sub := claimString(claims, "sub")
	switch {
	case u.Email != "":
		u.ID = u.Email
	case sub != "":
		u.ID = "sub:" + sub
	}
	if u.Name == "" {
		given, family := claimString(claims, "given_name"), claimString(claims, "family_name")
		u.Name = strings.TrimSpace(given + " " + family)
	}
	if u.Name == "" && u.Email != "" {
		u.Name = emailLocalPart(u.Email)
	}
	if u.Name == "" {
		u.Name = claimString(claims, "preferred_username")
	}
	if u.Name == "" {
		u.Name = sub
	}
	for _, name := range extra {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		v, ok := lookupClaim(claims, name)
		if !ok || v == nil {
			continue
		}
		if u.Extra == nil {
			u.Extra = make(map[string]any)
		}
		u.Extra[name] = cloneValue(v)
	}
	return u
}

// emailUnverified reports whether claims explicitly mark the email as
// unverified (boolean false or the string "false").
func emailUnverified(claims map[string]any) bool {
	switch v := claims["email_verified"].(type) {
	case bool:
		return !v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "false")
	}
	return false
}

func withDefaults(m config.ClaimsMapping) config.ClaimsMapping {
	def := func(p *string, v string) {
		if strings.TrimSpace(*p) == "" {
			*p = v
		}
	}
	def(&m.Email, DefaultEmailClaim)
	def(&m.Name, DefaultNameClaim)
	def(&m.Title, DefaultTitleClaim)
	def(&m.Team, DefaultTeamClaim)
	def(&m.Slack, DefaultSlackClaim)
	def(&m.Avatar, DefaultAvatarClaim)
	def(&m.Groups, DefaultGroupsClaim)
	return m
}

// lookupClaim returns claims[name], or, when absent and name contains dots,
// the value at that path through nested objects.
func lookupClaim(claims map[string]any, name string) (any, bool) {
	if claims == nil || name == "" {
		return nil, false
	}
	if v, ok := claims[name]; ok {
		return v, true
	}
	if !strings.Contains(name, ".") {
		return nil, false
	}
	var cur any = claims
	for _, seg := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[seg]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// claimString returns a claim as a trimmed string. Numbers and booleans are
// formatted; an array yields its first non-empty string element.
func claimString(claims map[string]any, name string) string {
	v, ok := lookupClaim(claims, name)
	if !ok {
		return ""
	}
	return scalarString(v)
}

func scalarString(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		return strconv.FormatBool(x)
	case []string:
		for _, s := range x {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// claimStrings returns a list claim: a JSON array (strings or scalars) or a
// comma-separated string. Entries are trimmed and de-duplicated.
func claimStrings(claims map[string]any, name string) []string {
	v, ok := lookupClaim(claims, name)
	if !ok {
		return nil
	}
	return ParseList(v)
}

// ParseList converts a claim value into a list of strings: []string, []any
// of scalars, or a comma-separated string. Empty and (case-insensitively)
// duplicate entries are dropped; the order of first occurrence is preserved.
func ParseList(v any) []string {
	var out []string
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		out = strings.Split(x, ",")
	case []string:
		out = append(out, x...)
	case []any:
		for _, e := range x {
			if s := scalarString(e); s != "" {
				if _, isList := e.([]any); !isList {
					out = append(out, s)
				}
			}
		}
	default:
		if s := scalarString(x); s != "" {
			out = []string{s}
		}
	}
	return dedupeFold(out)
}
