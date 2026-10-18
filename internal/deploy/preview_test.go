package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/sites"
)

// The preview (SiteMeta.preview, SPEC §7.2) is recomputed from the site's
// files at every write, like title and description. The resolution rules
// themselves are tested in internal/sites.

func TestCommitPreview(t *testing.T) {
	e := newEnv(t)
	c := e.client("alice")
	ctx := context.Background()
	dir := t.TempDir()
	deploy := func(want string) {
		t.Helper()
		res, err := c.Deploy(ctx, "demo", dir, DeployOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Site.Preview != want {
			t.Errorf("commit response preview = %q, want %q", res.Site.Preview, want)
		}
		if m := e.meta("demo"); m.Preview != want {
			t.Errorf("stored preview = %q, want %q", m.Preview, want)
		}
		if m, err := c.GetSite(ctx, "demo"); err != nil || m.Preview != want {
			t.Errorf("GET site preview = %+v, %v; want %q", m, err, want)
		}
	}
	remove := func(p string) {
		t.Helper()
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			t.Fatal(err)
		}
	}

	// og:image after title and description, site-root relative with a query.
	writeTree(t, dir, map[string]string{
		"index.html":    `<title>Demo</title><meta name="description" content="D"><meta property="og:image" content="/img/cover.png?v=1">`,
		"img/cover.png": "png",
		"preview.svg":   "<svg/>",
	})
	deploy("img/cover.png")
	if m := e.meta("demo"); m.Title != "Demo" || m.Description != "D" {
		t.Errorf("title/description = %q / %q", m.Title, m.Description)
	}

	// The og:image file is gone: fall through to preview.*.
	remove("img/cover.png")
	deploy("preview.svg")

	// A preview.* over 1 MiB is skipped; the next name in order is taken.
	writeTree(t, dir, map[string]string{
		"preview.png":  strings.Repeat("x", sites.MaxPreviewBytes+1),
		"preview.webp": "webp",
	})
	deploy("preview.webp")

	// An og:image with an unsupported extension falls through as well.
	writeTree(t, dir, map[string]string{
		"index.html": `<meta name="og:image" content="cover.bmp">`,
		"cover.bmp":  "bmp",
	})
	deploy("preview.webp")

	// Without index.html the fallback still applies.
	remove("index.html")
	deploy("preview.webp")

	// A redeploy without any preview removes it.
	for _, p := range []string{"preview.svg", "preview.png", "preview.webp"} {
		remove(p)
	}
	writeTree(t, dir, map[string]string{"index.html": "<title>Plain</title>"})
	deploy("")
	if m := e.meta("demo"); m.Title != "Plain" {
		t.Errorf("title = %q", m.Title)
	}
}

func TestRefreshPreviewEndpoint(t *testing.T) {
	e := newEnv(t)
	c := e.client("alice")
	ctx := context.Background()
	e.putObject("demo", "index.html", `<meta property="og:image" content="./shot.jpg"><title>Synced</title>`)
	e.putObject("demo", "shot.jpg", "jpg")
	m, err := c.Refresh(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if m.Preview != "shot.jpg" || m.Title != "Synced" || e.meta("demo").Preview != "shot.jpg" {
		t.Errorf("refresh = %+v", m)
	}
	// A direct sync that drops the image, then a refresh, removes it.
	if err := e.blobStore.Delete(ctx, sites.SiteKey("demo", "shot.jpg")); err != nil {
		t.Fatal(err)
	}
	m, err = c.Refresh(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if m.Preview != "" || e.meta("demo").Preview != "" {
		t.Errorf("refresh after removal = %+v", m)
	}
	// And a refresh restores it from preview.*.
	e.putObject("demo", "preview.gif", "gif")
	if m, err = c.Refresh(ctx, "demo"); err != nil || m.Preview != "preview.gif" {
		t.Errorf("refresh with preview.gif = %+v, %v", m, err)
	}
}

func TestPublishPreview(t *testing.T) {
	publishStores(t, func(t *testing.T, mk func(...envOption) *env) {
		e := mk()
		check := func(want string) {
			t.Helper()
			if m := e.meta("lunch"); m.Preview != want {
				t.Errorf("preview = %q, want %q", m.Preview, want)
			}
		}

		// index.html in the call: its og:image, read from memory.
		e.publish(pub(alice, "lunch",
			"index.html", `<title>Lunch</title><meta name="description" content="Vote"><meta property="og:image" content="img/a%20b.png">`,
			"img/a b.png", "png", "preview.svg", "<svg/>"))
		check("img/a b.png")

		// A merge without index.html reads the stored one; the og:image file
		// is deleted, so preview.svg takes over.
		req := pub(alice, "lunch", "app.js", "1")
		req.Delete = []string{"img/a b.png"}
		e.publish(req)
		check("preview.svg")

		// A merge that adds the og:image target brings it back.
		e.publish(pub(alice, "lunch", "img/a b.png", "png2"))
		check("img/a b.png")

		// An og:image pointing elsewhere is never used.
		e.publish(pub(alice, "lunch", "index.html", `<meta property="og:image" content="https://cdn.example.com/a.png">`))
		check("preview.svg")

		// Replace without any preview file removes it.
		req = pub(alice, "lunch", "index.html", "<title>Bare</title>")
		req.Replace = true
		e.publish(req)
		check("")
		if m := e.meta("lunch"); m.Title != "Bare" {
			t.Errorf("title = %q", m.Title)
		}

		// A site without index.html still gets preview.*.
		e.publish(pub(alice, "pics", "preview.png", "png"))
		if m := e.meta("pics"); m.Preview != "preview.png" {
			t.Errorf("pics preview = %q", m.Preview)
		}
	})
}
