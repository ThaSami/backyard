// Package sites owns site naming, metadata and manifests stored in the blob
// store (SPEC §7.2, §9).
package sites

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
)

// Blob key prefixes (relative to the storage prefix).
const (
	SitesPrefix    = "sites/"
	MetaPrefix     = "_meta/sites/"
	ManifestPrefix = "_meta/manifests/"
	UploadsPrefix  = "uploads/"
)

var (
	// ErrNotFound: the site does not exist.
	ErrNotFound = errors.New("sites: not found")
	// ErrInvalidName: name fails the pattern or is reserved.
	ErrInvalidName = errors.New("sites: invalid site name")
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateName checks a site name against the DNS-label pattern and the
// reserved list.
func ValidateName(name string, reserved []string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: %q (lowercase letters, digits and '-', 1-63 chars, no leading/trailing '-')", ErrInvalidName, name)
	}
	for _, r := range reserved {
		if strings.EqualFold(r, name) {
			return fmt.Errorf("%w: %q is reserved", ErrInvalidName, name)
		}
	}
	return nil
}

// SiteKey returns the blob key of a file inside a site.
func SiteKey(site, path string) string { return SitesPrefix + site + "/" + path }

// UploadKey returns the blob key of an uploaded file.
func UploadKey(site, id, name string) string { return UploadsPrefix + site + "/" + id + "/" + name }

// Meta is the public description of a site (SiteMeta in the spec).
type Meta struct {
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	DeployID    string    `json:"deploy_id,omitempty"`
	FileCount   int       `json:"file_count"`
	Bytes       int64     `json:"bytes"`
	Owners      []string  `json:"owners"`
}

// IsOwner reports whether id/email is listed as an owner.
func (m *Meta) IsOwner(id, email string) bool {
	for _, o := range m.Owners {
		if strings.EqualFold(o, id) || (email != "" && strings.EqualFold(o, email)) {
			return true
		}
	}
	return false
}

// ManifestFile is one entry of a site manifest.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	ETag   string `json:"etag"` // storage etag observed after upload
}

// Manifest records the content hashes of the last API deploy/refresh.
type Manifest struct {
	Site      string         `json:"site"`
	UpdatedAt time.Time      `json:"updated_at"`
	Files     []ManifestFile `json:"files"`
}

// Registry reads and writes site metadata in the blob store.
type Registry struct {
	store blob.Store
	cfg   *config.Config
}

// NewRegistry returns a registry over store.
func NewRegistry(store blob.Store, cfg *config.Config) *Registry {
	return &Registry{store: store, cfg: cfg}
}

// Store returns the underlying blob store.
func (r *Registry) Store() blob.Store { return r.store }

// Config returns the configuration.
func (r *Registry) Config() *config.Config { return r.cfg }
