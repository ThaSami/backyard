package identity

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewDirectoryNone(t *testing.T) {
	d, err := NewDirectory(config.DirectoryConfig{})
	if err != nil || d != nil {
		t.Fatalf("NewDirectory(empty) = %v, %v; want nil, nil", d, err)
	}
}

func TestNewDirectoryErrors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		cfg  config.DirectoryConfig
		want string
	}{
		{"unknown type", config.DirectoryConfig{Type: "ldap", Path: "x"}, "csv or json"},
		{"missing path", config.DirectoryConfig{Type: "csv"}, "path is required"},
		{"missing file", config.DirectoryConfig{Type: "csv", Path: filepath.Join(dir, "nope.csv")}, "open directory"},
		{"no email column", config.DirectoryConfig{Type: "csv", Path: writeFile(t, dir, "a.csv", "name,title\nA,B\n")}, "email"},
		{"empty csv", config.DirectoryConfig{Type: "csv", Path: writeFile(t, dir, "b.csv", "")}, "empty csv"},
		{"bad json", config.DirectoryConfig{Type: "json", Path: writeFile(t, dir, "c.json", "{nope")}, "decode json"},
		{"json scalar", config.DirectoryConfig{Type: "json", Path: writeFile(t, dir, "d.json", `"x"`)}, "array"},
		{"empty json", config.DirectoryConfig{Type: "json", Path: writeFile(t, dir, "e.json", " ")}, "empty json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewDirectory(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestDirectoryCSV(t *testing.T) {
	dir := t.TempDir()
	csv := "\ufeffEmail,Name,Title,Department,Slack,Avatar URL,Groups,Location\n" +
		"# comment line\n" +
		"Alice@Acme.com,Alice Smith,Staff Engineer,Platform,@alice,https://a.png,eng; backyard-admins ;,Ottawa\n" +
		"bob@acme.com,,,,,,,\n" +
		"not-an-email,X,,,,,,\n" +
		"short@acme.com,Short\n" +
		"\"quoted@acme.com\",\"Quoted, Person\",,,,,,\n"
	p := writeFile(t, dir, "people.csv", csv)
	d, err := NewDirectory(config.DirectoryConfig{Type: "CSV", Path: p})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	tests := []struct {
		email string
		want  *User
	}{
		{"ALICE@acme.com", &User{
			ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice Smith", Title: "Staff Engineer",
			Team: "Platform", Slack: "@alice", AvatarURL: "https://a.png",
			Groups: []string{"eng", "backyard-admins"}, Extra: map[string]any{"Location": "Ottawa"},
		}},
		{"bob@acme.com", &User{ID: "bob@acme.com", Email: "bob@acme.com", Name: "bob"}},
		{"short@acme.com", &User{ID: "short@acme.com", Email: "short@acme.com", Name: "Short"}},
		{"quoted@acme.com", &User{ID: "quoted@acme.com", Email: "quoted@acme.com", Name: "Quoted, Person"}},
		{"nobody@acme.com", nil},
		{"", nil},
	}
	for _, tc := range tests {
		got, err := d.Lookup(ctx, tc.email)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Lookup(%q)\n got  %+v\n want %+v", tc.email, got, tc.want)
		}
	}
	// Returned profiles are copies.
	a, _ := d.Lookup(ctx, "alice@acme.com")
	a.Groups[0] = "mutated"
	b, _ := d.Lookup(ctx, "alice@acme.com")
	if b.Groups[0] != "eng" {
		t.Fatal("Lookup returned shared state")
	}
}

func TestDirectoryJSON(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	arr := `[
	  {"email": "Alice@Acme.com", "name": "Alice", "department": "Platform", "groups": ["eng", "ops"],
	   "picture": "https://a", "extra": {"location": "Ottawa"}, "pronouns": "she/her", "admin": true},
	  {"email": "bob@acme.com", "groups": "a; b, c", "title": "PM"},
	  {"name": "no email"},
	  {"email": 42}
	]`
	d, err := NewDirectory(config.DirectoryConfig{Type: "json", Path: writeFile(t, dir, "people.json", arr)})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := d.Lookup(ctx, "alice@acme.com")
	want := &User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice", Team: "Platform",
		AvatarURL: "https://a", Groups: []string{"eng", "ops"},
		Extra: map[string]any{"location": "Ottawa", "pronouns": "she/her"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alice\n got  %+v\n want %+v", got, want)
	}
	got, _ = d.Lookup(ctx, "bob@acme.com")
	want = &User{ID: "bob@acme.com", Email: "bob@acme.com", Name: "bob", Title: "PM", Groups: []string{"a", "b", "c"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bob\n got  %+v\n want %+v", got, want)
	}
	if n := d.(*staticDirectory).Len(); n != 2 {
		t.Fatalf("Len = %d, want 2", n)
	}

	wrapped := `{"users": [{"email": "c@acme.com", "name": "Carol"}]}`
	d, err = NewDirectory(config.DirectoryConfig{Type: "json", Path: writeFile(t, dir, "wrapped.json", wrapped)})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := d.Lookup(ctx, "c@acme.com"); got == nil || got.Name != "Carol" {
		t.Fatalf("wrapped lookup = %+v", got)
	}
}

func TestDirectoryReload(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "people.csv", "email,name\na@x.io,Old\n")
	d, err := NewDirectory(config.DirectoryConfig{Type: "csv", Path: p, Reload: config.Duration(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	sd := d.(*staticDirectory)
	now := time.Now()
	sd.now = func() time.Time { return now }
	sd.lastCheck.Store(now.UnixNano())
	ctx := context.Background()

	// Rewrite with a different size and mtime.
	if err := os.WriteFile(p, []byte("email,name\na@x.io,New Name\nb@x.io,B\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatal(err)
	}
	// Not due yet: still old data.
	if u, _ := d.Lookup(ctx, "a@x.io"); u.Name != "Old" {
		t.Fatalf("before interval: %q", u.Name)
	}
	// Due: the reload runs in the background.
	now = now.Add(2 * time.Minute)
	_, _ = d.Lookup(ctx, "a@x.io")
	sd.wg.Wait()
	if u, _ := d.Lookup(ctx, "a@x.io"); u.Name != "New Name" {
		t.Fatalf("after reload: %q", u.Name)
	}
	if u, _ := d.Lookup(ctx, "b@x.io"); u == nil {
		t.Fatal("b not loaded")
	}

	// A broken file keeps the previous contents.
	if err := os.WriteFile(p, []byte("name\nx\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	future = future.Add(time.Hour)
	_ = os.Chtimes(p, future, future)
	now = now.Add(2 * time.Minute)
	_, _ = d.Lookup(ctx, "a@x.io")
	sd.wg.Wait()
	if u, _ := d.Lookup(ctx, "a@x.io"); u == nil || u.Name != "New Name" {
		t.Fatalf("after failed reload: %+v", u)
	}

	// Unchanged file: no reload needed (reloadIfChanged is a no-op).
	if err := sd.reloadIfChanged(); err == nil {
		// File is still broken but changed relative to loaded state, so an
		// error is expected here.
		t.Fatal("expected reload error for broken file")
	}
}

func TestDirectoryNoReloadWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "people.json", `[{"email":"a@x.io","name":"A"}]`)
	d, err := NewDirectory(config.DirectoryConfig{Type: "json", Path: p, Reload: config.Duration(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	sd := d.(*staticDirectory)
	before := sd.users
	if err := sd.reloadIfChanged(); err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(sd.users).Pointer() != reflect.ValueOf(before).Pointer() {
		t.Fatal("unchanged file was re-read")
	}
}
