package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/sites"
)

// With sites.listing: owners, the real authenticator's identities are
// listed what they own: a static token with admin: true every site, one
// without its own, a guest nothing; ownership follows by default.
func TestSiteListingOwners(t *testing.T) {
	t.Setenv("BACKYARD_CONFIG_YAML", "")
	t.Setenv("BACKYARD_CONFIG", "")
	const deployTok, ciTok = "deploy-token-0123456789abcdef", "ci-token-0123456789abcdef"
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:" + freePort(t)
	cfg.Storage.FS.Root = filepath.Join(dir, "data")
	cfg.Database.SQLite.Path = filepath.Join(dir, "db", "backyard.db")
	cfg.Sites.Listing = "owners"
	cfg.Auth.Tokens = []config.StaticToken{{Name: "deploy", SHA256: sum(deployTok), Admin: true}, {Name: "ci", SHA256: sum(ciTok)}}
	cfg.ApplyDevDefaults()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Sites.Ownership != "owner" {
		t.Fatalf("ownership = %q, want owner (implied by listing owners)", cfg.Sites.Ownership)
	}
	base := start(t, cfg, Options{})
	gw := base + deploy.EmbeddedPath

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "index.html"), []byte("<title>x</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for site, tok := range map[string]string{"admins": deployTok, "cis": ciTok, "guests": ""} {
		if _, err := deploy.NewClient(gw, tok).Deploy(ctx, site, src, deploy.DeployOptions{}); err != nil {
			t.Fatalf("deploy %s: %v", site, err)
		}
	}

	hubList := func(tok string) []string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+"/__backyard/api/sites", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct {
			Sites []sites.Meta `json:"sites"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 || out.Sites == nil {
			t.Fatalf("hub sites: %d %v", resp.StatusCode, err)
		}
		return names(out.Sites)
	}
	for _, tc := range []struct {
		tok  string
		want []string
	}{
		{deployTok, []string{"admins", "cis", "guests"}},
		{ciTok, []string{"cis"}},
		{"", []string{}},
	} {
		list, err := deploy.NewClient(gw, tc.tok).ListSites(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(list); !slices.Equal(got, tc.want) {
			t.Errorf("G/sites as %q: %v, want %v", tc.tok, got, tc.want)
		}
		if got := hubList(tc.tok); !slices.Equal(got, tc.want) {
			t.Errorf("hub sites as %q: %v, want %v", tc.tok, got, tc.want)
		}
	}
	// Lookups by name still answer, and /info reports the listing.
	if _, err := deploy.NewClient(gw, ciTok).GetSite(ctx, "admins"); err != nil {
		t.Errorf("GET G/sites/admins as ci: %v", err)
	}
	inf, err := deploy.NewClient(gw, "").Info(ctx)
	if err != nil || inf.SiteListing != "owners" {
		t.Errorf("info site_listing = %q, %v", inf.SiteListing, err)
	}
}

func names(list []sites.Meta) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.Name)
	}
	slices.Sort(out)
	return out
}
