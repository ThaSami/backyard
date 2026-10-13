// Package sitestest provides test helpers for code built on the sites
// registry and the deploy gateway: an in-memory blob.Store with optional
// presigned uploads served by an http.Handler. Like S3, the handler rejects
// a body whose size or SHA-256 differs from the PresignPutOptions (unless
// SetLaxPresign makes it accept anything, like Azure SAS URLs).
//
// It is meant for tests only; production code never imports it.
package sitestest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/blob"
)

type memObject struct {
	data []byte
	info blob.ObjectInfo
}

type presignGrant struct {
	key         string
	contentType string
	size        int64  // -1 = any
	sha256      string // lowercase hex, "" = any
	expires     time.Time
}

// MemStore is a concurrency-safe in-memory blob.Store.
type MemStore struct {
	mu      sync.RWMutex
	objs    map[string]*memObject
	grants  map[string]presignGrant
	presign string // base URL for presigned PUTs ("" = unsupported)
	lax     bool   // presigned PUTs accept any size and content
	puts    int
	gets    int
	now     func() time.Time

	// Desc is returned by Describe.
	Desc blob.Description
	// FailPut, when set, is consulted before every Put; a non-nil error
	// fails the Put.
	FailPut func(key string) error
}

var _ blob.Store = (*MemStore)(nil)

// NewMemStore returns an empty store without presign support.
func NewMemStore() *MemStore {
	return &MemStore{
		objs:   map[string]*memObject{},
		grants: map[string]presignGrant{},
		now:    time.Now,
		Desc:   blob.Description{Type: "memory"},
	}
}

// EnablePresign makes PresignPut return URLs under baseURL (the URL of a
// server running PresignHandler). An empty baseURL disables presigning.
func (s *MemStore) EnablePresign(baseURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.presign = strings.TrimSuffix(baseURL, "/")
}

// SetLaxPresign makes presigned URLs accept a body of any size and content
// (like Azure SAS URLs, which cannot bind them); by default PresignHandler
// enforces the declared size and SHA-256 (like S3 with flexible checksums).
func (s *MemStore) SetLaxPresign(lax bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lax = lax
}

// PresignHandler serves PUTs to URLs returned by PresignPut, mimicking a
// cloud storage endpoint: the signed Content-Type must be sent verbatim, a
// body of another size than the declared one is rejected with 403 (as S3
// does for a signed Content-Length) and one with another SHA-256 with 400
// (S3's BadDigest), unless SetLaxPresign(true) was called.
func (s *MemStore) PresignHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		tok := r.URL.Query().Get("sig")
		s.mu.Lock()
		g, ok := s.grants[tok]
		lax := s.lax
		s.mu.Unlock()
		if !ok || s.now().After(g.expires) {
			http.Error(w, "invalid signature", http.StatusForbidden)
			return
		}
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "only one auth mechanism allowed", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Content-Type") != g.contentType {
			http.Error(w, "content type does not match signature", http.StatusForbidden)
			return
		}
		if !lax && g.size >= 0 && r.ContentLength != g.size {
			http.Error(w, "SignatureDoesNotMatch: Content-Length does not match the signed size", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if sum := sha256.Sum256(body); !lax && g.sha256 != "" && hex.EncodeToString(sum[:]) != g.sha256 {
			http.Error(w, "BadDigest: the SHA256 you specified did not match the calculated checksum", http.StatusBadRequest)
			return
		}
		if _, err := s.Put(r.Context(), g.key, bytes.NewReader(body), int64(len(body)), g.contentType); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// PutCount returns the number of successful Put calls so far.
func (s *MemStore) PutCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.puts
}

// Keys returns every stored key, sorted.
func (s *MemStore) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.objs))
	for k := range s.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Bytes returns a copy of an object's content, or nil when missing.
func (s *MemStore) Bytes(key string) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if o, ok := s.objs[key]; ok {
		return bytes.Clone(o.data)
	}
	return nil
}

// SetModTime overrides an object's modification time.
func (s *MemStore) SetModTime(key string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.objs[key]; ok {
		o.info.ModTime = t
	}
}

func checkKey(key string) error {
	if !blob.ValidKey(key) || strings.HasSuffix(key, "/") {
		return fmt.Errorf("%w: %q", blob.ErrInvalidKey, key)
	}
	return nil
}

// Get implements blob.Store.
func (s *MemStore) Get(ctx context.Context, key string) (io.ReadCloser, *blob.ObjectInfo, error) {
	return s.GetRange(ctx, key, 0, -1)
}

// GetRange implements blob.Store.
func (s *MemStore) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := checkKey(key); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	o, ok := s.objs[key]
	s.gets++
	s.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
	}
	data := o.data
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	data = data[offset:]
	if length >= 0 && length < int64(len(data)) {
		data = data[:length]
	}
	info := o.info
	return io.NopCloser(bytes.NewReader(data)), &info, nil
}

// Stat implements blob.Store.
func (s *MemStore) Stat(ctx context.Context, key string) (*blob.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkKey(key); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objs[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", blob.ErrNotFound, key)
	}
	info := o.info
	return &info, nil
}

// Put implements blob.Store.
func (s *MemStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*blob.ObjectInfo, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if s.FailPut != nil {
		if err := s.FailPut(key); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		return nil, fmt.Errorf("memstore: read body: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size >= 0 && int64(buf.Len()) != size {
		return nil, fmt.Errorf("memstore: body has %d bytes, declared %d", buf.Len(), size)
	}
	if contentType == "" {
		contentType = blob.DetectContentType(key, buf.Bytes())
	}
	sum := sha256.Sum256(buf.Bytes())
	o := &memObject{data: buf.Bytes(), info: blob.ObjectInfo{
		Key:         key,
		Size:        int64(buf.Len()),
		ETag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
		ContentType: contentType,
		ModTime:     s.now().UTC(),
	}}
	s.mu.Lock()
	s.objs[key] = o
	s.puts++
	s.mu.Unlock()
	info := o.info
	return &info, nil
}

// Delete implements blob.Store.
func (s *MemStore) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkKey(key); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs, key)
	return nil
}

// DeletePrefix implements blob.Store.
func (s *MemStore) DeletePrefix(ctx context.Context, prefix string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if prefix == "" {
		return fmt.Errorf("%w: empty prefix", blob.ErrInvalidKey)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.objs {
		if strings.HasPrefix(k, prefix) {
			delete(s.objs, k)
		}
	}
	return nil
}

// List implements blob.Store.
func (s *MemStore) List(ctx context.Context, prefix, delimiter string) (*blob.ListResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if delimiter != "" && delimiter != "/" {
		return nil, blob.ErrNotSupported
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := &blob.ListResult{}
	seen := map[string]bool{}
	for k, o := range s.objs {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		if delimiter != "" {
			if i := strings.Index(rest, delimiter); i >= 0 {
				p := prefix + rest[:i+1]
				if !seen[p] {
					seen[p] = true
					res.Prefixes = append(res.Prefixes, p)
				}
				continue
			}
		}
		res.Objects = append(res.Objects, o.info)
	}
	sort.Slice(res.Objects, func(i, j int) bool { return res.Objects[i].Key < res.Objects[j].Key })
	sort.Strings(res.Prefixes)
	return res, nil
}

// PresignPut implements blob.Store. It returns ErrNotSupported unless
// EnablePresign was called. The URL accepts exactly opts.Size bytes (any
// size when -1) whose SHA-256 is opts.SHA256 (any content when "").
func (s *MemStore) PresignPut(ctx context.Context, key string, opts blob.PresignPutOptions, ttl time.Duration) (*blob.PresignedRequest, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if opts.Size < -1 {
		return nil, fmt.Errorf("memstore: invalid upload size %d", opts.Size)
	}
	sha := strings.ToLower(opts.SHA256)
	if sum, err := hex.DecodeString(sha); err != nil || (sha != "" && len(sum) != sha256.Size) {
		return nil, fmt.Errorf("memstore: invalid upload sha256 %q", opts.SHA256)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presign == "" {
		return nil, blob.ErrNotSupported
	}
	contentType := opts.ContentType
	if contentType == "" {
		contentType = blob.DetectContentType(key, nil)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	exp := s.now().Add(ttl)
	s.grants[tok] = presignGrant{key: key, contentType: contentType, size: opts.Size, sha256: sha, expires: exp}
	return &blob.PresignedRequest{
		Method:    http.MethodPut,
		URL:       s.presign + "/" + url.PathEscape(key) + "?sig=" + tok,
		Headers:   map[string]string{"Content-Type": contentType},
		ExpiresAt: exp,
	}, nil
}

// PresignGet implements blob.Store (always ErrNotSupported).
func (s *MemStore) PresignGet(context.Context, string, time.Duration) (*blob.PresignedRequest, error) {
	return nil, blob.ErrNotSupported
}

// Ping implements blob.Store.
func (s *MemStore) Ping(ctx context.Context) error { return ctx.Err() }

// Describe implements blob.Store.
func (s *MemStore) Describe() blob.Description { return s.Desc }

// Close implements blob.Store.
func (s *MemStore) Close() error { return nil }

// ErrInjected is a convenience error for FailPut hooks.
var ErrInjected = errors.New("memstore: injected failure")
