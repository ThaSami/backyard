package identity

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/thasami/backyard/internal/config"
)

func TestMapClaims(t *testing.T) {
	defaults := config.ClaimsMapping{}
	custom := config.ClaimsMapping{
		Email: "upn", Name: "displayName", Title: "jobTitle", Team: "org.team",
		Slack: "slack_handle", Avatar: "photo", Groups: "roles",
	}
	tests := []struct {
		name    string
		claims  map[string]any
		mapping config.ClaimsMapping
		extra   []string
		want    *User
	}{
		{
			name: "defaults full profile",
			claims: map[string]any{
				"sub": "00u1", "email": "Alice@Acme.COM", "name": "Alice Smith", "title": "Staff Engineer",
				"department": "Platform", "slack": "@alice", "picture": "https://img/a.png",
				"groups": []any{"eng", "backyard-admins", "ENG", " "},
			},
			mapping: defaults,
			want: &User{
				ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice Smith", Title: "Staff Engineer",
				Team: "Platform", Slack: "@alice", AvatarURL: "https://img/a.png",
				Groups: []string{"eng", "backyard-admins"},
			},
		},
		{
			name:    "groups as []string",
			claims:  map[string]any{"email": "a@x.io", "groups": []string{"a", "b", "a"}},
			mapping: defaults,
			want:    &User{ID: "a@x.io", Email: "a@x.io", Name: "a", Groups: []string{"a", "b"}},
		},
		{
			name:    "groups as comma string",
			claims:  map[string]any{"email": "a@x.io", "groups": "a, b ,,c"},
			mapping: defaults,
			want:    &User{ID: "a@x.io", Email: "a@x.io", Name: "a", Groups: []string{"a", "b", "c"}},
		},
		{
			name:    "no email uses sub",
			claims:  map[string]any{"sub": "12345", "name": "Bob"},
			mapping: defaults,
			want:    &User{ID: "sub:12345", Name: "Bob"},
		},
		{
			name:    "invalid email ignored",
			claims:  map[string]any{"sub": "u1", "email": "not-an-email"},
			mapping: defaults,
			want:    &User{ID: "sub:u1", Name: "u1"},
		},
		{
			name:    "name from given and family",
			claims:  map[string]any{"email": "c@x.io", "given_name": "Carol", "family_name": "Jones"},
			mapping: defaults,
			want:    &User{ID: "c@x.io", Email: "c@x.io", Name: "Carol Jones"},
		},
		{
			name:    "name falls back to email local part",
			claims:  map[string]any{"email": "dave.lee@x.io"},
			mapping: defaults,
			want:    &User{ID: "dave.lee@x.io", Email: "dave.lee@x.io", Name: "dave.lee"},
		},
		{
			name:    "name falls back to preferred_username",
			claims:  map[string]any{"sub": "s", "preferred_username": "erin"},
			mapping: defaults,
			want:    &User{ID: "sub:s", Name: "erin"},
		},
		{
			name: "custom mapping with nested claim",
			claims: map[string]any{
				"sub": "s1", "upn": "Frank@Corp.example", "displayName": "Frank",
				"jobTitle": "PM", "org": map[string]any{"team": "Growth"}, "slack_handle": "@frank",
				"photo": "https://p", "roles": []any{"admin", float64(7)}, "email": "ignored@x.io",
			},
			mapping: custom,
			want: &User{
				ID: "frank@corp.example", Email: "frank@corp.example", Name: "Frank", Title: "PM",
				Team: "Growth", Slack: "@frank", AvatarURL: "https://p", Groups: []string{"admin", "7"},
			},
		},
		{
			name:    "extra claims",
			claims:  map[string]any{"email": "g@x.io", "location": "Ottawa", "meta": map[string]any{"floor": float64(3)}},
			mapping: defaults,
			extra:   []string{"location", "missing", "meta.floor", ""},
			want: &User{ID: "g@x.io", Email: "g@x.io", Name: "g",
				Extra: map[string]any{"location": "Ottawa", "meta.floor": float64(3)}},
		},
		{
			name:    "numeric and array scalars",
			claims:  map[string]any{"email": "h@x.io", "title": json.Number("42"), "department": []any{"Ops", "Other"}},
			mapping: defaults,
			want:    &User{ID: "h@x.io", Email: "h@x.io", Name: "h", Title: "42", Team: "Ops"},
		},
		{
			name:    "unverified email ignored",
			claims:  map[string]any{"sub": "g-1", "email": "ceo@acme.com", "email_verified": false, "name": "Mallory"},
			mapping: defaults,
			want:    &User{ID: "sub:g-1", Name: "Mallory"},
		},
		{
			name:    "unverified email as string",
			claims:  map[string]any{"sub": "g-2", "email": "ceo@acme.com", "email_verified": "False"},
			mapping: defaults,
			want:    &User{ID: "sub:g-2", Name: "g-2"},
		},
		{
			name:    "verified email kept",
			claims:  map[string]any{"sub": "g-3", "email": "a@acme.com", "email_verified": "true"},
			mapping: defaults,
			want:    &User{ID: "a@acme.com", Email: "a@acme.com", Name: "a"},
		},
		{
			name:    "email_verified ignored for custom email claim",
			claims:  map[string]any{"sub": "g-4", "upn": "u@acme.com", "email_verified": false},
			mapping: config.ClaimsMapping{Email: "upn"},
			want:    &User{ID: "u@acme.com", Email: "u@acme.com", Name: "u"},
		},
		{
			name:    "nothing usable",
			claims:  map[string]any{"name": "Nobody"},
			mapping: defaults,
			want:    &User{Name: "Nobody"},
		},
		{
			name:    "nil claims",
			claims:  nil,
			mapping: defaults,
			want:    &User{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MapClaims(tc.claims, tc.mapping, tc.extra)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MapClaims()\n got  %+v\n want %+v", got, tc.want)
			}
			if got.Admin {
				t.Fatal("MapClaims must never set Admin")
			}
		})
	}
}

func TestMapClaimsDoesNotAliasExtra(t *testing.T) {
	nested := map[string]any{"k": "v"}
	u := MapClaims(map[string]any{"email": "a@b.io", "obj": nested}, config.ClaimsMapping{}, []string{"obj"})
	u.Extra["obj"].(map[string]any)["k"] = "changed"
	if nested["k"] != "v" {
		t.Fatal("Extra aliases the claims map")
	}
}

func TestParseList(t *testing.T) {
	tests := []struct {
		in   any
		want []string
	}{
		{nil, nil},
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{[]any{"x", nil, map[string]any{}, true, []any{"nested"}}, []string{"x", "true"}},
		{[]string{"A", "a", "B"}, []string{"A", "B"}},
		{float64(3), []string{"3"}},
		{map[string]any{"a": 1}, nil},
	}
	for _, tc := range tests {
		if got := ParseList(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseList(%#v) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	tests := map[string]string{
		" Alice@Acme.com ":    "alice@acme.com",
		"a+tag@x.io":          "a+tag@x.io",
		"":                    "",
		"alice":               "",
		"@x.io":               "",
		"a@":                  "",
		"a b@x.io":            "",
		"Alice <a@x.io>":      "",
		"a@x..io":             "",
		"a@x.io.":             "",
		"a@.x.io":             "",
		"\"quoted\"@x.io":     "",
		"a@b@c.io":            "",
		"accounts.google.com": "",
	}
	for in, want := range tests {
		if got := NormalizeEmail(in); got != want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", in, got, want)
		}
	}
	if EmailDomain("a@Acme.COM") != "acme.com" || EmailDomain("nope") != "" {
		t.Fatal("EmailDomain")
	}
}

func TestCloneAndEnrich(t *testing.T) {
	u := &User{
		ID: "a@x.io", Email: "a@x.io", Name: "A", Title: "Eng", Groups: []string{"eng"},
		Admin: true, Extra: map[string]any{"k": []any{"v"}, "keep": "me"},
	}
	c := u.Clone()
	c.Groups[0] = "changed"
	c.Extra["k"].([]any)[0] = "changed"
	if u.Groups[0] != "eng" || u.Extra["k"].([]any)[0] != "v" {
		t.Fatal("Clone is shallow")
	}
	if (*User)(nil).Clone() != nil {
		t.Fatal("nil clone")
	}

	p := &User{
		ID: "other", Email: "other@x.io", Name: "Alice Smith", Team: "Platform", Slack: " ",
		AvatarURL: "https://a", Groups: []string{"ENG", "dir-group"}, Admin: false, Anonymous: true,
		Extra: map[string]any{"k": "dir"},
	}
	u.Enrich(p)
	want := &User{
		ID: "a@x.io", Email: "a@x.io", Name: "Alice Smith", Title: "Eng", Team: "Platform",
		AvatarURL: "https://a", Groups: []string{"eng", "dir-group"}, Admin: true,
		Extra: map[string]any{"k": "dir", "keep": "me"},
	}
	if !reflect.DeepEqual(u, want) {
		t.Fatalf("Enrich\n got  %+v\n want %+v", u, want)
	}
	u.Enrich(nil)
	(*User)(nil).Enrich(p)
}

func TestContractHelpers(t *testing.T) {
	u := &User{ID: "a@x.io", Email: "a@x.io", Name: "A", AvatarURL: "https://a", Groups: []string{"Eng"}}
	if s := u.Summary(); s != (Summary{ID: "a@x.io", Name: "A", Email: "a@x.io", AvatarURL: "https://a"}) {
		t.Fatalf("Summary = %+v", s)
	}
	if !u.InGroup("eng") || u.InGroup("ops") || (*User)(nil).InGroup("eng") {
		t.Fatal("InGroup")
	}
	if u.Actor() != "a@x.io" || (*User)(nil).Actor() != "unknown" || (&User{}).Actor() != "unknown" {
		t.Fatal("Actor")
	}
	if (*User)(nil).Summary() != (Summary{}) {
		t.Fatal("nil Summary")
	}
	ctx := WithUser(context.Background(), u)
	if FromContext(ctx) != u || FromContext(context.Background()) != nil {
		t.Fatal("context round trip")
	}
}

func TestScalarString(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{" s ", "s"},
		{json.Number("1.5"), "1.5"},
		{float64(2), "2"},
		{float32(2.5), "2.5"},
		{7, "7"},
		{int64(8), "8"},
		{true, "true"},
		{[]string{" ", "x"}, "x"},
		{[]string{}, ""},
		{[]any{1.0, " y "}, "y"},
		{map[string]any{}, ""},
		{nil, ""},
	}
	for _, tc := range tests {
		if got := scalarString(tc.in); got != tc.want {
			t.Errorf("scalarString(%#v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if emailLocalPart("nodomain") != "nodomain" {
		t.Fatal("emailLocalPart")
	}
}

func TestEnrichKeepsRealNameOverFallback(t *testing.T) {
	tests := []struct {
		name     string
		have     string
		dirName  string
		dirEmail string
		want     string
	}{
		{"fallback name does not replace a real name", "Alice Smith", "alice", "alice@acme.com", "Alice Smith"},
		{"fallback name check is case-insensitive", "Alice Smith", "Alice", "alice@acme.com", "Alice Smith"},
		{"fallback name fills an empty name", "", "alice", "alice@acme.com", "alice"},
		{"real directory name replaces", "alice", "Alice Directory", "alice@acme.com", "Alice Directory"},
		{"directory without email keeps its name", "Alice Smith", "alice", "", "alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := &User{ID: "alice@acme.com", Email: "alice@acme.com", Name: tt.have}
			u.Enrich(&User{Email: tt.dirEmail, Name: tt.dirName, Team: "Platform"})
			if u.Name != tt.want || u.Team != "Platform" {
				t.Fatalf("Enrich: name %q team %q, want name %q", u.Name, u.Team, tt.want)
			}
		})
	}
}
