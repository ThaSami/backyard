package sites

import (
	"context"

	"github.com/thasami/backyard/internal/blob"
)

// STUBS: replaced by the sites implementation.

// List returns all sites: meta files ∪ first-level prefixes under sites/,
// sorted by UpdatedAt descending (then name).
func (r *Registry) List(ctx context.Context) ([]Meta, error) { panic("sites: not implemented") }

// Get returns a site's metadata or ErrNotFound. For sites synced directly to
// storage without a meta file, a Meta is synthesized from the object listing.
func (r *Registry) Get(ctx context.Context, name string) (*Meta, error) {
	panic("sites: not implemented")
}

// Exists reports whether any object exists under sites/<name>/ or a meta file exists.
func (r *Registry) Exists(ctx context.Context, name string) (bool, error) {
	panic("sites: not implemented")
}

// PutMeta writes a site's metadata.
func (r *Registry) PutMeta(ctx context.Context, m *Meta) error { panic("sites: not implemented") }

// GetManifest returns the manifest, or an empty manifest when none exists.
func (r *Registry) GetManifest(ctx context.Context, name string) (*Manifest, error) {
	panic("sites: not implemented")
}

// PutManifest writes a site's manifest.
func (r *Registry) PutManifest(ctx context.Context, m *Manifest) error {
	panic("sites: not implemented")
}

// Files lists the objects of a site with keys made site-relative (no "sites/<name>/" prefix).
func (r *Registry) Files(ctx context.Context, name string) ([]blob.ObjectInfo, error) {
	panic("sites: not implemented")
}

// Delete removes a site's files, uploads, meta and manifest.
func (r *Registry) Delete(ctx context.Context, name string) error { panic("sites: not implemented") }

// Refresh rebuilds the manifest (sha256 of every file) and meta from storage
// after a direct sync; actor becomes UpdatedBy (and CreatedBy/owner if new).
func (r *Registry) Refresh(ctx context.Context, name, actor string) (*Meta, error) {
	panic("sites: not implemented")
}

// ExtractTitle returns the <title> text and <meta name="description"> content
// of an HTML document (best effort; only the first 64 KiB are considered).
func ExtractTitle(html []byte) (title, description string) { panic("sites: not implemented") }
