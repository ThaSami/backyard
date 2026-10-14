package sites

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/blob"
)

const (
	// maxMetaBytes caps the size of a meta document read from storage.
	maxMetaBytes = 1 << 20
	// maxManifestBytes caps the size of a manifest read from storage
	// (10 000 files of ~1 KiB paths fit comfortably).
	maxManifestBytes = 64 << 20
	// TitleScanBytes is how much of an index.html is inspected for its title.
	TitleScanBytes = 64 << 10
	// ioParallelism bounds concurrent storage calls made by one registry call.
	ioParallelism = 8
	// IndexFile is the site-relative path whose <title> describes the site.
	IndexFile = "index.html"
)

// ErrCorrupt is returned (wrapped) when a stored meta document or manifest
// cannot be decoded or exceeds its size bound. List and Get then fall back
// to metadata synthesized from the object listing, GetManifest to an empty
// manifest and Refresh rebuilds both; GetMeta reports it so that
// authorization checks can fail closed.
var ErrCorrupt = errors.New("sites: corrupt metadata")

// MetaKey returns the blob key of a site's metadata document.
func MetaKey(site string) string { return MetaPrefix + site + ".json" }

// ManifestKey returns the blob key of a site's manifest.
func ManifestKey(site string) string { return ManifestPrefix + site + ".json" }

// checkName validates the DNS-label pattern only (the reserved list is a
// deploy policy, not a storage concern).
func checkName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidName, name)
	}
	return nil
}

// List returns all sites: meta files ∪ first-level prefixes under sites/,
// sorted by UpdatedAt descending (then name).
func (r *Registry) List(ctx context.Context) ([]Meta, error) {
	metaList, err := r.store.List(ctx, MetaPrefix, "")
	if err != nil {
		return nil, fmt.Errorf("sites: list metadata: %w", err)
	}
	siteList, err := r.store.List(ctx, SitesPrefix, "/")
	if err != nil {
		return nil, fmt.Errorf("sites: list sites: %w", err)
	}

	names := map[string]bool{} // name → has meta file
	for _, o := range metaList.Objects {
		name, ok := strings.CutSuffix(strings.TrimPrefix(o.Key, MetaPrefix), ".json")
		if !ok || checkName(name) != nil {
			continue
		}
		names[name] = true
	}
	for _, p := range siteList.Prefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(p, SitesPrefix), "/")
		if checkName(name) != nil {
			continue
		}
		if _, ok := names[name]; !ok {
			names[name] = false
		}
	}

	ordered := make([]string, 0, len(names))
	for n := range names {
		ordered = append(ordered, n)
	}
	slices.Sort(ordered)

	out := make([]*Meta, len(ordered))
	err = forEach(ctx, len(ordered), ioParallelism, func(ctx context.Context, i int) error {
		name := ordered[i]
		var m *Meta
		var err error
		if names[name] {
			m, err = r.readMeta(ctx, name)
			if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
				// Deleted concurrently, or unreadable: fall back to the
				// listing rather than hiding every site.
				m, err = nil, nil
			}
			if err != nil {
				return err
			}
		}
		if m == nil {
			m, err = r.synthesize(ctx, name)
			if errors.Is(err, ErrNotFound) {
				return nil // vanished between the listings
			}
			if err != nil {
				return err
			}
		}
		out[i] = m
		return nil
	})
	if err != nil {
		return nil, err
	}

	res := make([]Meta, 0, len(out))
	for _, m := range out {
		if m != nil {
			res = append(res, *m)
		}
	}
	sortMetas(res)
	return res, nil
}

// sortMetas orders by UpdatedAt descending, then name ascending.
func sortMetas(ms []Meta) {
	slices.SortStableFunc(ms, func(a, b Meta) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

// Get returns a site's metadata or ErrNotFound. For sites synced directly to
// storage without a meta file, a Meta is synthesized from the object listing.
func (r *Registry) Get(ctx context.Context, name string) (*Meta, error) {
	if err := checkName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	m, err := r.readMeta(ctx, name)
	if err == nil {
		return m, nil
	}
	if !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCorrupt) {
		return nil, err
	}
	return r.synthesize(ctx, name)
}

// GetMeta returns the stored meta document of a site, without synthesizing
// one from the object listing. It returns ErrNotFound when none exists and
// an error matching ErrCorrupt when it cannot be decoded.
func (r *Registry) GetMeta(ctx context.Context, name string) (*Meta, error) {
	if err := checkName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return r.readMeta(ctx, name)
}

// Exists reports whether any object exists under sites/<name>/ or a meta file exists.
func (r *Registry) Exists(ctx context.Context, name string) (bool, error) {
	if checkName(name) != nil {
		return false, nil
	}
	if _, err := r.store.Stat(ctx, MetaKey(name)); err == nil {
		return true, nil
	} else if !errors.Is(err, blob.ErrNotFound) {
		return false, fmt.Errorf("sites: stat meta %q: %w", name, err)
	}
	res, err := r.store.List(ctx, SitesPrefix+name+"/", "/")
	if err != nil {
		return false, fmt.Errorf("sites: list %q: %w", name, err)
	}
	for _, o := range res.Objects {
		if !strings.HasSuffix(o.Key, "/") {
			return true, nil
		}
	}
	return len(res.Prefixes) > 0, nil
}

// PutMeta writes a site's metadata.
func (r *Registry) PutMeta(ctx context.Context, m *Meta) error {
	if m == nil {
		return errors.New("sites: nil meta")
	}
	if err := checkName(m.Name); err != nil {
		return err
	}
	c := *m
	c.URL = r.cfg.SiteURL(c.Name)
	c.Owners = normalizeOwners(c.Owners)
	b, err := json.Marshal(&c)
	if err != nil {
		return fmt.Errorf("sites: encode meta: %w", err)
	}
	if _, err := r.store.Put(ctx, MetaKey(c.Name), bytes.NewReader(b), int64(len(b)), "application/json"); err != nil {
		return fmt.Errorf("sites: write meta %q: %w", c.Name, err)
	}
	return nil
}

// GetManifest returns the manifest, or an empty manifest when none exists.
// A corrupt manifest is treated as missing: it only caches digests, so the
// next deploy re-uploads every file once and writes a fresh one.
func (r *Registry) GetManifest(ctx context.Context, name string) (*Manifest, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	var m Manifest
	err := r.readJSON(ctx, ManifestKey(name), maxManifestBytes, &m)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt) {
		return &Manifest{Site: name, Files: []ManifestFile{}}, nil
	}
	if err != nil {
		return nil, err
	}
	m.Site = name
	if m.Files == nil {
		m.Files = []ManifestFile{}
	}
	return &m, nil
}

// PutManifest writes a site's manifest.
func (r *Registry) PutManifest(ctx context.Context, m *Manifest) error {
	if m == nil {
		return errors.New("sites: nil manifest")
	}
	if err := checkName(m.Site); err != nil {
		return err
	}
	c := *m
	if c.Files == nil {
		c.Files = []ManifestFile{}
	}
	b, err := json.Marshal(&c)
	if err != nil {
		return fmt.Errorf("sites: encode manifest: %w", err)
	}
	if _, err := r.store.Put(ctx, ManifestKey(c.Site), bytes.NewReader(b), int64(len(b)), "application/json"); err != nil {
		return fmt.Errorf("sites: write manifest %q: %w", c.Site, err)
	}
	return nil
}

// Files lists the objects of a site with keys made site-relative (no "sites/<name>/" prefix).
// "Directory marker" objects (keys ending in "/") are omitted.
func (r *Registry) Files(ctx context.Context, name string) ([]blob.ObjectInfo, error) {
	if err := checkName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	prefix := SitesPrefix + name + "/"
	res, err := r.store.List(ctx, prefix, "")
	if err != nil {
		return nil, fmt.Errorf("sites: list files of %q: %w", name, err)
	}
	out := make([]blob.ObjectInfo, 0, len(res.Objects))
	for _, o := range res.Objects {
		rel, ok := strings.CutPrefix(o.Key, prefix)
		if !ok || rel == "" || strings.HasSuffix(rel, "/") {
			continue
		}
		o.Key = rel
		out = append(out, o)
	}
	return out, nil
}

// Delete removes a site's files, uploads, meta and manifest. The meta
// document goes last so an interrupted delete can be retried.
func (r *Registry) Delete(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := r.store.DeletePrefix(ctx, SitesPrefix+name+"/"); err != nil {
		return fmt.Errorf("sites: delete files of %q: %w", name, err)
	}
	if err := r.store.DeletePrefix(ctx, UploadsPrefix+name+"/"); err != nil {
		return fmt.Errorf("sites: delete uploads of %q: %w", name, err)
	}
	if err := r.store.Delete(ctx, ManifestKey(name)); err != nil {
		return fmt.Errorf("sites: delete manifest of %q: %w", name, err)
	}
	if err := r.store.Delete(ctx, MetaKey(name)); err != nil {
		return fmt.Errorf("sites: delete meta of %q: %w", name, err)
	}
	return nil
}

// Refresh rebuilds the manifest (sha256 of every file) and meta from storage
// after a direct sync; actor becomes UpdatedBy (and CreatedBy/owner if new).
//
// CreatedAt, CreatedBy and Owners of an existing meta document are kept. When
// the site is new and sites.ownership is "owner", a non-anonymous actor
// becomes its first owner.
func (r *Registry) Refresh(ctx context.Context, name, actor string) (*Meta, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	files, err := r.Files(ctx, name)
	if err != nil {
		return nil, err
	}
	prev, err := r.readMeta(ctx, name)
	switch {
	case errors.Is(err, ErrNotFound) || errors.Is(err, ErrCorrupt):
		prev = nil // rebuilt from scratch (refresh repairs a corrupt document)
	case err != nil:
		return nil, err
	}
	if len(files) == 0 && prev == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}

	entries := make([]*ManifestFile, len(files))
	var (
		headMu sync.Mutex
		head   []byte
	)
	err = forEach(ctx, len(files), ioParallelism, func(ctx context.Context, i int) error {
		f := files[i]
		mf, h, err := r.hashObject(ctx, SiteKey(name, f.Key), f.Key == IndexFile)
		if errors.Is(err, blob.ErrNotFound) {
			return nil // deleted while refreshing
		}
		if err != nil {
			return err
		}
		mf.Path = f.Key
		entries[i] = mf
		if h != nil {
			headMu.Lock()
			head = h
			headMu.Unlock()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	man := &Manifest{Site: name, UpdatedAt: now, Files: make([]ManifestFile, 0, len(entries))}
	var total int64
	for _, e := range entries {
		if e == nil {
			continue
		}
		man.Files = append(man.Files, *e)
		total += e.Size
	}

	m := &Meta{Name: name, CreatedAt: now, CreatedBy: actor}
	if prev != nil {
		m.CreatedAt, m.CreatedBy, m.Owners = prev.CreatedAt, prev.CreatedBy, prev.Owners
		if m.CreatedAt.IsZero() {
			m.CreatedAt = now
		}
	}
	if len(m.Owners) == 0 && r.cfg.Sites.Ownership == "owner" && actor != "" && !isAnonymousActor(actor) {
		m.Owners = []string{actor}
	}
	m.UpdatedAt, m.UpdatedBy = now, actor
	m.FileCount, m.Bytes = len(man.Files), total
	if head != nil {
		m.Title, m.Description = ExtractTitle(head)
	}

	if err := r.PutManifest(ctx, man); err != nil {
		return nil, err
	}
	if err := r.PutMeta(ctx, m); err != nil {
		return nil, err
	}
	m.URL = r.cfg.SiteURL(name)
	m.Owners = normalizeOwners(m.Owners)
	return m, nil
}

// FileDigest returns the manifest entry of the object currently stored for
// path in site: its size, SHA-256 and etag, where the digest is always that
// of the bytes the etag identifies. It uses a digest the storage service
// verified itself when the store reports one (blob.DigestReporter, e.g. S3
// objects uploaded with x-amz-checksum-sha256) and otherwise reads and
// hashes the object. A missing object yields an error wrapping
// blob.ErrNotFound.
func (r *Registry) FileDigest(ctx context.Context, site, path string) (*ManifestFile, error) {
	if err := checkName(site); err != nil {
		return nil, err
	}
	key := SiteKey(site, path)
	if dr, ok := r.store.(blob.DigestReporter); ok {
		info, sum, err := dr.StatSHA256(ctx, key)
		switch {
		case err == nil && sum != "":
			return &ManifestFile{Path: path, Size: info.Size, SHA256: sum, ETag: info.ETag}, nil
		case errors.Is(err, blob.ErrNotFound):
			return nil, err
		case err != nil && ctx.Err() != nil:
			return nil, ctx.Err()
		}
		// No verified digest for this object (or none available): read it.
	}
	mf, _, err := r.hashObject(ctx, key, false)
	if err != nil {
		return nil, err
	}
	mf.Path = path
	return mf, nil
}

// hashObject reads an object fully, returning its manifest entry (without
// Path) and, when keepHead is set, its first TitleScanBytes bytes.
func (r *Registry) hashObject(ctx context.Context, key string, keepHead bool) (*ManifestFile, []byte, error) {
	rc, info, err := r.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("sites: read %q: %w", key, err)
	}
	defer rc.Close()
	h := sha256.New()
	var head *headBuffer
	var w io.Writer = h
	if keepHead {
		head = &headBuffer{max: TitleScanBytes}
		w = io.MultiWriter(h, head)
	}
	n, err := io.Copy(w, rc)
	if err != nil {
		return nil, nil, fmt.Errorf("sites: read %q: %w", key, err)
	}
	mf := &ManifestFile{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	if info != nil {
		mf.ETag = info.ETag
	}
	if head != nil {
		return mf, head.buf, nil
	}
	return mf, nil, nil
}

// headBuffer keeps the first max bytes written to it.
type headBuffer struct {
	buf []byte
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// readMeta reads and decodes a meta document (ErrNotFound when missing).
func (r *Registry) readMeta(ctx context.Context, name string) (*Meta, error) {
	var m Meta
	if err := r.readJSON(ctx, MetaKey(name), maxMetaBytes, &m); err != nil {
		return nil, err
	}
	m.Name = name
	m.URL = r.cfg.SiteURL(name)
	m.Owners = normalizeOwners(m.Owners)
	return &m, nil
}

// readJSON reads key (at most limit bytes) into v. It maps blob.ErrNotFound
// to ErrNotFound.
func (r *Registry) readJSON(ctx context.Context, key string, limit int64, v any) error {
	rc, _, err := r.store.Get(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return fmt.Errorf("sites: read %s: %w", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return fmt.Errorf("sites: read %s: %w", key, err)
	}
	if int64(len(b)) > limit {
		return fmt.Errorf("%w: %s exceeds %d bytes", ErrCorrupt, key, limit)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: decode %s: %w", ErrCorrupt, key, err)
	}
	return nil
}

// synthesize builds a Meta from the object listing of a site that has no
// meta document (direct sync). It returns ErrNotFound when the site is empty.
func (r *Registry) synthesize(ctx context.Context, name string) (*Meta, error) {
	files, err := r.Files(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	m := &Meta{Name: name, URL: r.cfg.SiteURL(name), FileCount: len(files), Owners: []string{}}
	for _, f := range files {
		m.Bytes += f.Size
		mt := f.ModTime.UTC()
		if mt.After(m.UpdatedAt) {
			m.UpdatedAt = mt
		}
		if !mt.IsZero() && (m.CreatedAt.IsZero() || mt.Before(m.CreatedAt)) {
			m.CreatedAt = mt
		}
	}
	return m, nil
}

// normalizeOwners returns a non-nil owners slice (JSON "[]", never null).
func normalizeOwners(o []string) []string {
	if o == nil {
		return []string{}
	}
	return o
}

// isAnonymousActor reports whether an actor id denotes an anonymous guest.
func isAnonymousActor(actor string) bool { return strings.HasPrefix(actor, "anon:") }

// forEach runs fn(i) for i in [0,n) with at most par concurrent calls,
// returning the first error (and cancelling the remaining calls).
func forEach(ctx context.Context, n, par int, fn func(ctx context.Context, i int) error) error {
	if n == 0 {
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, max(1, par))
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := fn(ctx, i); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
