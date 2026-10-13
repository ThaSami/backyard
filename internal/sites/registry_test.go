package sites_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/fs"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/sites/sitestest"
)

func testConfig(t *testing.T, ownership string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("server:\n  base_domain: backyard.test\n  secret: 0123456789abcdef0123456789abcdef\nsites:\n  ownership: " + ownership + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func put(t *testing.T, s blob.Store, key, data string) *blob.ObjectInfo {
	t.Helper()
	info, err := s.Put(context.Background(), key, strings.NewReader(data), int64(len(data)), "")
	if err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
	return info
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestValidateName(t *testing.T) {
	reserved := config.DefaultReserved
	tests := []struct {
		name string
		ok   bool
	}{
		{"a", true},
		{"lunch-vote", true},
		{"a1", true},
		{strings.Repeat("a", 63), true},
		{strings.Repeat("a", 64), false},
		{"", false},
		{"-a", false},
		{"a-", false},
		{"A", false},
		{"a_b", false},
		{"a.b", false},
		{"a/b", false},
		{"www", false},
		{"admin", false},
		{"s", false},
	}
	for _, tc := range tests {
		err := sites.ValidateName(tc.name, reserved)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateName(%q) = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && !errors.Is(err, sites.ErrInvalidName) {
			t.Errorf("ValidateName(%q) error %v does not wrap ErrInvalidName", tc.name, err)
		}
	}
}

func TestPutGetMeta(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "none"))

	if _, err := reg.Get(ctx, "nope"); !errors.Is(err, sites.ErrNotFound) {
		t.Fatalf("Get missing: err = %v, want ErrNotFound", err)
	}
	if _, err := reg.Get(ctx, "Bad_Name"); !errors.Is(err, sites.ErrNotFound) || !errors.Is(err, sites.ErrInvalidName) {
		t.Fatalf("Get invalid: err = %v, want ErrNotFound+ErrInvalidName", err)
	}
	if ok, err := reg.Exists(ctx, "nope"); err != nil || ok {
		t.Fatalf("Exists missing = %v, %v", ok, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	m := &sites.Meta{Name: "lunch", Title: "Lunch", CreatedAt: now, UpdatedAt: now, CreatedBy: "a@x", FileCount: 2, Bytes: 10}
	if err := reg.PutMeta(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Get(ctx, "lunch")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://lunch.backyard.test/" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.Owners == nil {
		t.Error("Owners is nil, want empty slice")
	}
	if got.Title != "Lunch" || got.FileCount != 2 || !got.CreatedAt.Equal(now) {
		t.Errorf("Get = %+v", got)
	}
	if ok, err := reg.Exists(ctx, "lunch"); err != nil || !ok {
		t.Fatalf("Exists(meta only) = %v, %v", ok, err)
	}
	if err := reg.PutMeta(ctx, &sites.Meta{Name: "../x"}); !errors.Is(err, sites.ErrInvalidName) {
		t.Errorf("PutMeta invalid name: %v", err)
	}
}

func TestListMergesMetaAndPrefixes(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "none"))

	old := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, m := range []*sites.Meta{
		{Name: "alpha", UpdatedAt: old},
		{Name: "beta", UpdatedAt: newer},
		{Name: "gamma", UpdatedAt: old},
	} {
		if err := reg.PutMeta(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	put(t, store, "sites/alpha/index.html", "<title>a</title>")
	// Direct-synced site without meta.
	put(t, store, "sites/direct/index.html", "hello")
	put(t, store, "sites/direct/assets/app.js", "console.log(1)")
	mid := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	store.SetModTime("sites/direct/index.html", mid.Add(-time.Hour))
	store.SetModTime("sites/direct/assets/app.js", mid)
	// Junk that must be ignored.
	put(t, store, "sites/Bad_Name/x.html", "x")
	put(t, store, "_meta/sites/not a site.json", "{}")
	put(t, store, "sites/stray.txt", "x")

	list, err := reg.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range list {
		names = append(names, m.Name)
	}
	want := []string{"beta", "direct", "alpha", "gamma"}
	if !slices.Equal(names, want) {
		t.Fatalf("List names = %v, want %v", names, want)
	}
	d := list[1]
	if d.FileCount != 2 || d.Bytes != int64(len("hello")+len("console.log(1)")) {
		t.Errorf("synthesized meta = %+v", d)
	}
	if !d.UpdatedAt.Equal(mid) || !d.CreatedAt.Equal(mid.Add(-time.Hour)) {
		t.Errorf("synthesized times = %v / %v", d.CreatedAt, d.UpdatedAt)
	}
	if d.URL != "https://direct.backyard.test/" || d.Owners == nil {
		t.Errorf("synthesized URL/owners = %q / %v", d.URL, d.Owners)
	}

	g, err := reg.Get(ctx, "direct")
	if err != nil || g.FileCount != 2 {
		t.Fatalf("Get(direct) = %+v, %v", g, err)
	}
	if ok, err := reg.Exists(ctx, "direct"); err != nil || !ok {
		t.Fatalf("Exists(direct) = %v, %v", ok, err)
	}
}

func TestListPathMode(t *testing.T) {
	cfg := testConfig(t, "none")
	cfg.Server.URLMode = "path"
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, cfg)
	put(t, store, "sites/foo/index.html", "x")
	list, err := reg.List(context.Background())
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v", list, err)
	}
	if list[0].URL != "https://backyard.test/s/foo/" {
		t.Errorf("URL = %q", list[0].URL)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	ctx := context.Background()
	reg := sites.NewRegistry(sitestest.NewMemStore(), testConfig(t, "none"))
	m, err := reg.GetManifest(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if m.Site != "site" || m.Files == nil || len(m.Files) != 0 {
		t.Fatalf("empty manifest = %+v", m)
	}
	in := &sites.Manifest{Site: "site", UpdatedAt: time.Now().UTC(), Files: []sites.ManifestFile{{Path: "a.html", Size: 3, SHA256: sha("abc"), ETag: `"e"`}}}
	if err := reg.PutManifest(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := reg.GetManifest(ctx, "site")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files) != 1 || out.Files[0] != in.Files[0] {
		t.Errorf("manifest = %+v", out)
	}
	if err := reg.PutManifest(ctx, &sites.Manifest{Site: "UP"}); !errors.Is(err, sites.ErrInvalidName) {
		t.Errorf("PutManifest invalid: %v", err)
	}
}

// TestCorruptMetadata checks that one unreadable document neither hides
// every site nor makes its site unrepairable.
func TestCorruptMetadata(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "owner"))
	put(t, store, sites.MetaKey("broken"), "{not json")
	put(t, store, "sites/broken/index.html", "<title>Broken</title>")
	put(t, store, sites.ManifestKey("broken"), `{"files": [`)
	if err := reg.PutMeta(ctx, &sites.Meta{Name: "fine"}); err != nil {
		t.Fatal(err)
	}

	// GetMeta reports it, so authorization can fail closed.
	if _, err := reg.GetMeta(ctx, "broken"); !errors.Is(err, sites.ErrCorrupt) || errors.Is(err, sites.ErrNotFound) {
		t.Errorf("GetMeta corrupt meta: err = %v, want ErrCorrupt", err)
	}
	// Get and List fall back to the object listing.
	m, err := reg.Get(ctx, "broken")
	if err != nil || m.FileCount != 1 || len(m.Owners) != 0 {
		t.Errorf("Get corrupt meta = %+v, %v", m, err)
	}
	list, err := reg.List(ctx)
	if err != nil || len(list) != 2 {
		t.Errorf("List with a corrupt meta = %+v, %v", list, err)
	}
	// A corrupt manifest reads as empty.
	man, err := reg.GetManifest(ctx, "broken")
	if err != nil || len(man.Files) != 0 || man.Site != "broken" {
		t.Errorf("GetManifest corrupt = %+v, %v", man, err)
	}
	// Refresh repairs both.
	m, err = reg.Refresh(ctx, "broken", "admin@acme.com")
	if err != nil || m.Title != "Broken" || m.CreatedBy != "admin@acme.com" {
		t.Fatalf("Refresh = %+v, %v", m, err)
	}
	if m, err := reg.GetMeta(ctx, "broken"); err != nil || m.Title != "Broken" {
		t.Errorf("GetMeta after refresh = %+v, %v", m, err)
	}
	if man, err := reg.GetManifest(ctx, "broken"); err != nil || len(man.Files) != 1 {
		t.Errorf("manifest after refresh = %+v, %v", man, err)
	}
}

// TestStorageErrorsAreNotCorruption keeps transient storage failures
// distinct from corrupt documents.
func TestStorageErrorsAreNotCorruption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reg := sites.NewRegistry(sitestest.NewMemStore(), testConfig(t, "none"))
	if _, err := reg.GetMeta(ctx, "x"); err == nil || errors.Is(err, sites.ErrCorrupt) {
		t.Errorf("GetMeta with a failing store: %v", err)
	}
	if _, err := reg.GetManifest(ctx, "x"); err == nil || errors.Is(err, sites.ErrCorrupt) {
		t.Errorf("GetManifest with a failing store: %v", err)
	}
}

func TestFilesAndDelete(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "none"))
	put(t, store, "sites/foo/index.html", "x")
	put(t, store, "sites/foo/a/b.css", "y")
	put(t, store, "sites/foobar/index.html", "other site")
	put(t, store, "uploads/foo/id1/pic.png", "png")
	put(t, store, "uploads/foobar/id2/pic.png", "png")
	if err := reg.PutMeta(ctx, &sites.Meta{Name: "foo"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.PutManifest(ctx, &sites.Manifest{Site: "foo"}); err != nil {
		t.Fatal(err)
	}

	files, err := reg.Files(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, f := range files {
		keys = append(keys, f.Key)
	}
	if !slices.Equal(keys, []string{"a/b.css", "index.html"}) {
		t.Errorf("Files = %v", keys)
	}
	if _, err := reg.Files(ctx, "../etc"); !errors.Is(err, sites.ErrInvalidName) {
		t.Errorf("Files invalid name: %v", err)
	}

	if err := reg.Delete(ctx, "foo"); err != nil {
		t.Fatal(err)
	}
	left := store.Keys()
	want := []string{"sites/foobar/index.html", "uploads/foobar/id2/pic.png"}
	if !slices.Equal(left, want) {
		t.Errorf("keys after delete = %v, want %v", left, want)
	}
	if ok, _ := reg.Exists(ctx, "foo"); ok {
		t.Error("site still exists after delete")
	}
}

func TestRefresh(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "owner"))

	if _, err := reg.Refresh(ctx, "empty", "a@x"); !errors.Is(err, sites.ErrNotFound) {
		t.Fatalf("Refresh empty site: %v", err)
	}

	index := `<html><head><title> Hello &amp; welcome </title><meta name="description" content="A demo"></head></html>`
	put(t, store, "sites/demo/index.html", index)
	appInfo := put(t, store, "sites/demo/js/app.js", "let x = 1")

	m, err := reg.Refresh(ctx, "demo", "alice@acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "Hello & welcome" || m.Description != "A demo" {
		t.Errorf("title/description = %q / %q", m.Title, m.Description)
	}
	if m.FileCount != 2 || m.Bytes != int64(len(index)+len("let x = 1")) {
		t.Errorf("counts = %d / %d", m.FileCount, m.Bytes)
	}
	if m.CreatedBy != "alice@acme.com" || m.UpdatedBy != "alice@acme.com" || !slices.Equal(m.Owners, []string{"alice@acme.com"}) {
		t.Errorf("actors = %+v", m)
	}
	if m.URL != "https://demo.backyard.test/" {
		t.Errorf("URL = %q", m.URL)
	}

	man, err := reg.GetManifest(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Files) != 2 {
		t.Fatalf("manifest files = %+v", man.Files)
	}
	byPath := map[string]sites.ManifestFile{}
	for _, f := range man.Files {
		byPath[f.Path] = f
	}
	if f := byPath["js/app.js"]; f.SHA256 != sha("let x = 1") || f.ETag != appInfo.ETag || f.Size != 9 {
		t.Errorf("manifest entry = %+v", f)
	}

	// A second refresh by someone else keeps creation data and owners.
	createdAt := m.CreatedAt
	time.Sleep(10 * time.Millisecond)
	put(t, store, "sites/demo/new.txt", "new")
	m2, err := reg.Refresh(ctx, "demo", "bob@acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if !m2.CreatedAt.Equal(createdAt) || m2.CreatedBy != "alice@acme.com" || m2.UpdatedBy != "bob@acme.com" {
		t.Errorf("second refresh = %+v", m2)
	}
	if !slices.Equal(m2.Owners, []string{"alice@acme.com"}) || m2.FileCount != 3 {
		t.Errorf("second refresh owners/count = %v / %d", m2.Owners, m2.FileCount)
	}
	stored, err := reg.Get(ctx, "demo")
	if err != nil || stored.FileCount != 3 || stored.UpdatedBy != "bob@acme.com" {
		t.Errorf("stored meta = %+v, %v", stored, err)
	}
}

func TestRefreshAnonymousNotOwner(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "owner"))
	put(t, store, "sites/x/index.html", "no title here")
	m, err := reg.Refresh(ctx, "x", "anon:0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Owners) != 0 || m.Title != "" {
		t.Errorf("anonymous refresh = %+v", m)
	}
}

func TestRefreshOwnershipNone(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "none"))
	put(t, store, "sites/x/index.html", "x")
	m, err := reg.Refresh(ctx, "x", "a@x")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Owners) != 0 {
		t.Errorf("owners in ownership=none: %v", m.Owners)
	}
}

func TestRefreshManySmallFiles(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "none"))
	var total int64
	for i := range 200 {
		data := strings.Repeat("x", i)
		total += int64(i)
		put(t, store, fmt.Sprintf("sites/many/f%03d.txt", i), data)
	}
	m, err := reg.Refresh(ctx, "many", "a@x")
	if err != nil {
		t.Fatal(err)
	}
	if m.FileCount != 200 || m.Bytes != total {
		t.Errorf("count/bytes = %d/%d", m.FileCount, m.Bytes)
	}
}

func TestRefreshPropagatesStorageErrors(t *testing.T) {
	ctx := context.Background()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "none"))
	put(t, store, "sites/x/index.html", "x")
	store.FailPut = func(string) error { return sitestest.ErrInjected }
	if _, err := reg.Refresh(ctx, "x", "a@x"); !errors.Is(err, sitestest.ErrInjected) {
		t.Errorf("Refresh with failing store: %v", err)
	}
}

func TestContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := sitestest.NewMemStore()
	reg := sites.NewRegistry(store, testConfig(t, "none"))
	if _, err := reg.List(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("List cancelled: %v", err)
	}
}

// TestRegistryFSStore runs the registry against the real filesystem store.
func TestRegistryFSStore(t *testing.T) {
	store, err := fs.New(t.TempDir())
	if err != nil {
		t.Skipf("fs store unavailable: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	reg := sites.NewRegistry(store, testConfig(t, "owner"))
	put(t, store, "sites/fsite/index.html", "<title>FS</title>")
	put(t, store, "sites/fsite/a/b.txt", "b")
	put(t, store, "sites/other/x.txt", "x")
	put(t, store, "uploads/fsite/id/f.png", "png")

	m, err := reg.Refresh(ctx, "fsite", "dev@acme.com")
	if err != nil {
		t.Fatal(err)
	}
	if m.Title != "FS" || m.FileCount != 2 || !slices.Equal(m.Owners, []string{"dev@acme.com"}) {
		t.Errorf("refresh = %+v", m)
	}
	list, err := reg.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "fsite" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := reg.Delete(ctx, "fsite"); err != nil {
		t.Fatal(err)
	}
	if ok, err := reg.Exists(ctx, "fsite"); err != nil || ok {
		t.Errorf("exists after delete = %v, %v", ok, err)
	}
	if ok, _ := reg.Exists(ctx, "other"); !ok {
		t.Error("other site deleted")
	}
	res, err := store.List(ctx, "uploads/", "")
	if err != nil || len(res.Objects) != 0 {
		t.Errorf("uploads left: %+v, %v", res, err)
	}
}
