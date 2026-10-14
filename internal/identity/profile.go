package identity

import (
	"net/mail"
	"strings"
)

// Clone returns a deep copy of u (nil for nil). Groups and Extra are copied
// so the clone can be modified without affecting u.
func (u *User) Clone() *User {
	if u == nil {
		return nil
	}
	c := *u
	if u.Groups != nil {
		c.Groups = append([]string(nil), u.Groups...)
	}
	if u.Extra != nil {
		c.Extra = make(map[string]any, len(u.Extra))
		for k, v := range u.Extra {
			c.Extra[k] = cloneValue(v)
		}
	}
	return &c
}

// Enrich overlays a profile p (typically a directory entry for the same
// email) onto u: non-empty Name, Title, Team, Slack and AvatarURL of p replace
// u's values, groups are merged (union, case-insensitive, u's order first) and
// Extra keys of p override u's. ID, Email, Anonymous and Admin are never
// changed. A nil u or p is a no-op.
//
// A name of p that is merely the local part of p's email (the fallback used
// for directory entries without a name) does not replace a name u already
// has, so a sparse directory never downgrades "Alice Smith" to "alice".
func (u *User) Enrich(p *User) {
	if u == nil || p == nil {
		return
	}
	set := func(dst *string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	if !(strings.TrimSpace(u.Name) != "" && isFallbackName(p)) {
		set(&u.Name, p.Name)
	}
	set(&u.Title, p.Title)
	set(&u.Team, p.Team)
	set(&u.Slack, p.Slack)
	set(&u.AvatarURL, p.AvatarURL)
	if len(p.Groups) > 0 {
		u.Groups = dedupeFold(append(append([]string(nil), u.Groups...), p.Groups...))
	}
	if len(p.Extra) > 0 {
		if u.Extra == nil {
			u.Extra = make(map[string]any, len(p.Extra))
		}
		for k, v := range p.Extra {
			u.Extra[k] = cloneValue(v)
		}
	}
}

// NormalizeEmail trims and lowercases s and returns it when it is a plain
// e-mail address (local@domain, no display name, no spaces); otherwise it
// returns "".
func NormalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || len(s) > 320 || strings.ContainsAny(s, " \t\r\n<>,;\"") {
		return ""
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return ""
	}
	if strings.Contains(s[at+1:], "..") || strings.HasPrefix(s[at+1:], ".") || strings.HasSuffix(s, ".") {
		return ""
	}
	if a, err := mail.ParseAddress(s); err != nil || !strings.EqualFold(a.Address, s) {
		return ""
	}
	return s
}

// EmailDomain returns the lowercased domain part of an e-mail address, or "".
func EmailDomain(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return ""
	}
	return strings.ToLower(email[at+1:])
}

// isFallbackName reports whether p's name is just the local part of its email.
func isFallbackName(p *User) bool {
	n := strings.TrimSpace(p.Name)
	return n != "" && p.Email != "" && strings.EqualFold(n, emailLocalPart(p.Email))
}

// emailLocalPart returns the part before the last '@'.
func emailLocalPart(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return email
	}
	return email[:at]
}

// dedupeFold removes empty and case-insensitively duplicated entries while
// preserving the order of first occurrence.
func dedupeFold(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		k := strings.ToLower(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// cloneValue deep-copies JSON-like values (maps and slices); other values are
// returned as is.
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = cloneValue(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = cloneValue(e)
		}
		return s
	case []string:
		return append([]string(nil), x...)
	default:
		return v
	}
}
