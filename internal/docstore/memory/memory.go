// Package memory is an in-process docstore.Store backed by maps. It is meant
// for tests, `backyard dev` and single-instance demos: data is lost when the
// process exits.
//
// Stored documents are immutable values (writes replace them), and data is
// deep-copied on the way in and out, so callers never alias internal state.
// Queries are evaluated with the reference implementation in package docstore
// (docstore.EvalQuery).
package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/docstore/internal/dsutil"
)

// ErrClosed is returned by operations on a closed store.
var ErrClosed = errors.New("docstore/memory: store closed")

// Store is an in-memory document store. The zero value is not usable; call New.
type Store struct {
	mu     sync.RWMutex
	sites  map[string]map[string]map[string]*docstore.Doc // site → collection → id → doc
	closed bool
}

var _ docstore.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{sites: make(map[string]map[string]map[string]*docstore.Doc)}
}

func (s *Store) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return ErrClosed
	}
	return nil
}

// collection returns the collection map, creating it when create is true.
// Callers hold s.mu (write lock when create is true).
func (s *Store) collection(site, collection string, create bool) map[string]*docstore.Doc {
	colls := s.sites[site]
	if colls == nil {
		if !create {
			return nil
		}
		colls = make(map[string]map[string]*docstore.Doc)
		s.sites[site] = colls
	}
	docs := colls[collection]
	if docs == nil && create {
		docs = make(map[string]*docstore.Doc)
		colls[collection] = docs
	}
	return docs
}

// Create inserts a new document; see docstore.Store.
func (s *Store) Create(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	if id == "" {
		id = docstore.NewID()
	}
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	body, err := dsutil.Canonical(data)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	docs := s.collection(site, collection, true)
	if _, exists := docs[id]; exists {
		return nil, fmt.Errorf("%w: document %q already exists in %s", docstore.ErrConflict, id, collection)
	}
	now := docstore.Now()
	d := &docstore.Doc{
		Site: site, Collection: collection, ID: id, Data: body,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor, Version: 1,
	}
	docs[id] = d
	return docstore.CloneDoc(d), nil
}

// Get returns a document; see docstore.Store.
func (s *Store) Get(ctx context.Context, site, collection, id string) (*docstore.Doc, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	d, ok := s.collection(site, collection, false)[id]
	if !ok {
		return nil, docstore.ErrNotFound
	}
	return docstore.CloneDoc(d), nil
}

// Put creates or replaces a document; see docstore.Store.
func (s *Store) Put(ctx context.Context, site, collection, id string, data map[string]any, actor string) (*docstore.Doc, error) {
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	body, err := dsutil.Canonical(data)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	docs := s.collection(site, collection, true)
	now := docstore.Now()
	d := &docstore.Doc{
		Site: site, Collection: collection, ID: id, Data: body,
		CreatedAt: now, UpdatedAt: now, CreatedBy: actor, UpdatedBy: actor, Version: 1,
	}
	if old, ok := docs[id]; ok {
		d.CreatedAt, d.CreatedBy, d.Version = old.CreatedAt, old.CreatedBy, old.Version+1
	}
	docs[id] = d
	return docstore.CloneDoc(d), nil
}

// Update shallow-merges patch into an existing document; see docstore.Store.
func (s *Store) Update(ctx context.Context, site, collection, id string, patch map[string]any, actor string) (*docstore.Doc, error) {
	return s.update(ctx, site, collection, id, nil, patch, actor)
}

// UpdateIf is Update while the document is at rev; see docstore.Store.
func (s *Store) UpdateIf(ctx context.Context, site, collection, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	return s.update(ctx, site, collection, id, &rev, patch, actor)
}

// update merges patch, if rev is set only into the document at rev.
func (s *Store) update(ctx context.Context, site, collection, id string, rev *docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	actor, err := dsutil.CheckWrite(site, collection, id, actor)
	if err != nil {
		return nil, err
	}
	p, err := dsutil.PreparePatch(patch)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	docs := s.collection(site, collection, false)
	old, ok := docs[id]
	if !ok {
		return nil, docstore.ErrNotFound
	}
	if rev != nil && !old.At(*rev) {
		return nil, fmt.Errorf("%w: document %q has changed", docstore.ErrConflict, id)
	}
	// Stored documents are never mutated: build a new one. Sharing values
	// with old.Data is safe for the same reason, and the patch values are
	// fresh copies made by PreparePatch.
	d := *old
	d.Data = docstore.ApplyPatch(old.Data, p)
	d.UpdatedAt = docstore.Now()
	d.UpdatedBy = actor
	d.Version = old.Version + 1
	docs[id] = &d
	return docstore.CloneDoc(&d), nil
}

// Delete removes a document; see docstore.Store.
func (s *Store) Delete(ctx context.Context, site, collection, id string) (bool, error) {
	return s.delete(ctx, site, collection, id, nil)
}

// DeleteIf removes a document while it is at rev; see docstore.Store.
func (s *Store) DeleteIf(ctx context.Context, site, collection, id string, rev docstore.Rev) (bool, error) {
	return s.delete(ctx, site, collection, id, &rev)
}

// delete removes a document, if rev is set only while it is at rev.
func (s *Store) delete(ctx context.Context, site, collection, id string, rev *docstore.Rev) (bool, error) {
	if err := dsutil.CheckKey(site, collection, id); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return false, err
	}
	docs := s.collection(site, collection, false)
	if d, ok := docs[id]; !ok || (rev != nil && !d.At(*rev)) {
		return false, nil
	}
	delete(docs, id)
	if len(docs) == 0 {
		delete(s.sites[site], collection)
		if len(s.sites[site]) == 0 {
			delete(s.sites, site)
		}
	}
	return true, nil
}

// Query returns matching documents; see docstore.Store.
func (s *Store) Query(ctx context.Context, site, collection string, q docstore.Query) (*docstore.Page, error) {
	if err := dsutil.CheckNamespace(site, collection); err != nil {
		return nil, err
	}
	q, err := docstore.PrepareQuery(q)
	if err != nil {
		return nil, err
	}
	s.mu.RLock()
	if err := s.check(ctx); err != nil {
		s.mu.RUnlock()
		return nil, err
	}
	docs := s.collection(site, collection, false)
	candidates := make([]*docstore.Doc, 0, len(docs))
	for _, d := range docs {
		candidates = append(candidates, d)
	}
	s.mu.RUnlock()

	// Stored docs are immutable, so they can be evaluated outside the lock.
	page := docstore.EvalQuery(candidates, q)
	for i, d := range page.Docs {
		page.Docs[i] = docstore.CloneDoc(d)
	}
	return page, nil
}

// Collections lists the non-empty collections of a site, sorted.
func (s *Store) Collections(ctx context.Context, site string) ([]string, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	out := []string{}
	for name, docs := range s.sites[site] {
		if len(docs) > 0 {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Count returns the number of documents of a site.
func (s *Store) Count(ctx context.Context, site string) (int64, error) {
	if err := docstore.ValidateSite(site); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	var n int64
	for _, docs := range s.sites[site] {
		n += int64(len(docs))
	}
	return n, nil
}

// DeleteSite removes every document of a site.
func (s *Store) DeleteSite(ctx context.Context, site string) error {
	if err := docstore.ValidateSite(site); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return err
	}
	delete(s.sites, site)
	return nil
}

// Ping reports whether the store is open.
func (s *Store) Ping(ctx context.Context) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.check(ctx)
}

// Close releases the data; later calls fail with ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.sites = nil
	return nil
}
