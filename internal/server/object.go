package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/blob"
)

// errObjectNotFound reports a missing object from any source.
var errObjectNotFound = errors.New("server: object not found")

// object is something servable: a site file, an upload, a hub asset.
type object struct {
	name        string // file name (content type by extension, logs)
	size        int64
	etag        string // quoted strong ETag
	modTime     time.Time
	contentType string
	data        []byte // full content when loaded; else open is used
	// open returns length bytes starting at offset.
	open func(ctx context.Context, offset, length int64) (io.ReadCloser, error)
}

// fetchTimeout bounds blob metadata/small reads made for one request.
const fetchTimeout = 30 * time.Second

// loadBlob looks a blob up through the cache. It returns errObjectNotFound
// for missing objects and invalid keys.
func (s *Server) loadBlob(ctx context.Context, key string) (*object, error) {
	if !blob.ValidKey(key) {
		return nil, errObjectNotFound
	}
	var e *cacheEntry
	if s.cache != nil {
		if hit, ok := s.cache.get(key); ok {
			e = hit
		} else {
			var err error
			e, err = s.cache.do(key, func() (*cacheEntry, error) {
				// Detached from the first caller: other callers share the result.
				fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
				defer cancel()
				return s.fetchEntry(fctx, key, true)
			})
			if err != nil {
				return nil, err
			}
		}
	} else {
		var err error
		e, err = s.fetchEntry(ctx, key, false)
		if err != nil {
			return nil, err
		}
	}
	if e == nil {
		return nil, fmt.Errorf("server: lookup of %q failed", key)
	}
	if e.notFound {
		return nil, errObjectNotFound
	}
	return s.blobObject(key, e), nil
}

// fetchEntry stats key and, for small objects when loadSmall is set, reads
// its bytes.
func (s *Server) fetchEntry(ctx context.Context, key string, loadSmall bool) (*cacheEntry, error) {
	info, err := s.deps.Blob.Stat(ctx, key)
	if errors.Is(err, blob.ErrNotFound) || errors.Is(err, blob.ErrInvalidKey) {
		return &cacheEntry{key: key, notFound: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", key, err)
	}
	e := &cacheEntry{key: key, info: *info}
	if !loadSmall || info.Size > maxCachedObject {
		return e, nil
	}
	rc, ginfo, err := s.deps.Blob.Get(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return &cacheEntry{key: key, notFound: true}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", key, err)
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxCachedObject+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", key, err)
	}
	if ginfo != nil {
		// Get's metadata describes the bytes actually read.
		e.info = *ginfo
	}
	if int64(len(data)) > maxCachedObject {
		// Grew between Stat and Get: serve it as a large object.
		return e, nil
	}
	e.info.Size = int64(len(data))
	if data == nil {
		data = []byte{}
	}
	e.data = data
	return e, nil
}

// blobObject turns a cache entry into a servable object.
func (s *Server) blobObject(key string, e *cacheEntry) *object {
	name := key[strings.LastIndexByte(key, '/')+1:]
	o := &object{
		name:        name,
		size:        e.info.Size,
		etag:        formatETag(e.info.ETag, e.info.Size, e.info.ModTime),
		modTime:     e.info.ModTime,
		contentType: contentTypeFor(name, e.info.ContentType),
		data:        e.data,
	}
	if o.data == nil {
		store := s.deps.Blob
		size := o.size
		o.open = func(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
			var (
				rc  io.ReadCloser
				err error
			)
			if offset == 0 && length == size {
				rc, _, err = store.Get(ctx, key)
			} else {
				rc, _, err = store.GetRange(ctx, key, offset, length)
			}
			if errors.Is(err, blob.ErrNotFound) {
				return nil, errObjectNotFound
			}
			return rc, err
		}
	}
	return o
}

// contentTypeFor picks a Content-Type: by extension, else the stored type,
// else application/octet-stream; text types get a UTF-8 charset.
func contentTypeFor(name, stored string) string {
	ct := blob.ContentTypeByExt(name)
	if ct == "" {
		ct = strings.TrimSpace(stored)
	}
	if ct == "" {
		ct = "application/octet-stream"
	}
	if strings.HasPrefix(ct, "text/") && !strings.Contains(strings.ToLower(ct), "charset") {
		ct += "; charset=utf-8"
	}
	return ct
}

// formatETag renders a blob etag as a quoted strong ETag. Unusable values
// are replaced by a hash of size and modification time.
func formatETag(raw string, size int64, mod time.Time) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "W/")
	raw = strings.Trim(raw, `"`)
	if raw == "" || len(raw) > 200 || !etagChars(raw) {
		var b [16]byte
		binary.BigEndian.PutUint64(b[:8], uint64(size))
		binary.BigEndian.PutUint64(b[8:], uint64(mod.UnixNano()))
		sum := sha256.Sum256(b[:])
		raw = hex.EncodeToString(sum[:12])
	}
	return `"` + raw + `"`
}

// contentETag is an ETag derived from content bytes.
func contentETag(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return `"` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
}

func etagChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c == '"' || c == 0x7f {
			return false
		}
	}
	return true
}

// serveOptions tune serveObject.
type serveOptions struct {
	status       int    // 200 (default) enables conditional and range handling
	cacheControl string // default "no-cache"
}

// serveObject writes obj with validators, conditional GET (304), HEAD and
// single byte-range support (206/416).
//
// On platforms that limit the Content-Length of HTTP/1 responses
// (Server.maxFixedLength; Cloud Run: 32 MiB), larger bodies are adapted
// for HTTP/1 requests (HTTP/2 has no such limit):
//
//   - a full response (200, or a custom 404.html) is sent without a
//     Content-Length, so net/http streams it with chunked transfer encoding
//     (HTTP/1.0 clients read until the connection closes). A failure while
//     streaming aborts the connection, so a truncated body never looks
//     complete;
//   - a range response (206) is shortened to the first maxFixedLength bytes
//     of the requested range and keeps its Content-Length. Content-Range
//     names the bytes actually sent, which RFC 9110 §15.3.7 obliges
//     clients to inspect to "determine what parts are enclosed and whether
//     additional requests are needed"; browsers' media elements and
//     download managers request the rest.
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, obj *object, opt serveOptions) {
	status := opt.status
	if status == 0 {
		status = http.StatusOK
	}
	h := w.Header()
	if opt.cacheControl != "" {
		h.Set("Cache-Control", opt.cacheControl)
	} else if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache")
	}

	offset, length := int64(0), obj.size
	if status == http.StatusOK {
		if obj.etag != "" {
			h.Set("ETag", obj.etag)
		}
		if !obj.modTime.IsZero() {
			h.Set("Last-Modified", obj.modTime.UTC().Format(http.TimeFormat))
		}
		h.Set("Accept-Ranges", "bytes")
		switch checkPreconditions(r, obj) {
		case http.StatusNotModified:
			h.Del("Content-Type")
			h.Del("Content-Length")
			w.WriteHeader(http.StatusNotModified)
			return
		case http.StatusPreconditionFailed:
			s.fail(w, r, newError(http.StatusPreconditionFailed, codeBadRequest, "precondition failed"))
			return
		}
		if rh := r.Header.Get("Range"); rh != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) && ifRangeMatches(r, obj) {
			start, n, res := parseRange(rh, obj.size)
			switch res {
			case rangeUnsatisfiable:
				h.Set("Content-Range", "bytes */"+strconv.FormatInt(obj.size, 10))
				h.Set("Content-Type", "text/plain; charset=utf-8")
				h.Del("ETag")
				h.Del("Last-Modified")
				msg := "requested range not satisfiable\n"
				h.Set("Content-Length", strconv.Itoa(len(msg)))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				if r.Method != http.MethodHead {
					_, _ = io.WriteString(w, msg)
				}
				return
			case rangeOK:
				offset, length = start, n
				status = http.StatusPartialContent
			}
		}
	}

	chunked := false
	if limit := s.maxFixedLength; limit > 0 && length > limit && r.ProtoMajor < 2 {
		if status == http.StatusPartialContent {
			length = limit
		} else {
			chunked = true
		}
	}
	if status == http.StatusPartialContent {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, obj.size))
	}

	var body io.Reader
	if r.Method != http.MethodHead {
		if obj.data != nil {
			body = bytes.NewReader(obj.data[offset : offset+length])
		} else if length > 0 {
			rc, err := obj.open(r.Context(), offset, length)
			if err != nil {
				if errors.Is(err, errObjectNotFound) {
					s.fail(w, r, pageNotFound())
					return
				}
				s.fail(w, r, s.storageError(err))
				return
			}
			defer rc.Close()
			body = rc
			if length > maxCachedObject {
				// Large downloads may outlive server.write_timeout.
				_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Hour))
			}
		}
	}

	h.Set("Content-Type", obj.contentType)
	if chunked {
		h.Del("Content-Length")
	} else {
		h.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if _, err := io.CopyN(w, body, length); err != nil {
		if r.Context().Err() == nil {
			s.log.WarnContext(r.Context(), "streaming object failed", "name", obj.name, "error", err)
		}
		if chunked {
			// Returning normally would end the chunked body cleanly and
			// the client would take the truncated file for the whole one.
			// (With a Content-Length, net/http closes the connection after
			// a short body by itself.)
			panic(http.ErrAbortHandler)
		}
	}
}

// storageError maps a blob failure to an API error.
func (s *Server) storageError(err error) *apiError {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errContext(err, "storage request")
	}
	return errInternal(err, "storage error")
}

// checkPreconditions evaluates If-Match, If-Unmodified-Since, If-None-Match
// and If-Modified-Since (RFC 9110 §13.2.2). It returns 0 to proceed, 304 or
// 412.
func checkPreconditions(r *http.Request, obj *object) int {
	if im := r.Header.Get("If-Match"); im != "" {
		if !etagListMatch(im, obj.etag, true) {
			return http.StatusPreconditionFailed
		}
	} else if ius := r.Header.Get("If-Unmodified-Since"); ius != "" && !obj.modTime.IsZero() {
		if t, err := http.ParseTime(ius); err == nil && obj.modTime.Truncate(time.Second).After(t) {
			return http.StatusPreconditionFailed
		}
	}
	get := r.Method == http.MethodGet || r.Method == http.MethodHead
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagListMatch(inm, obj.etag, false) {
			if get {
				return http.StatusNotModified
			}
			return http.StatusPreconditionFailed
		}
		return 0
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && get && !obj.modTime.IsZero() {
		if t, err := http.ParseTime(ims); err == nil && !obj.modTime.Truncate(time.Second).After(t) {
			return http.StatusNotModified
		}
	}
	return 0
}

// etagListMatch reports whether a comma-separated If-(None-)Match list
// matches etag ("*" matches anything). strong disables weak comparison.
func etagListMatch(list, etag string, strong bool) bool {
	if etag == "" {
		return false
	}
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "*" {
			return true
		}
		weak := strings.HasPrefix(item, "W/")
		if weak && strong {
			continue
		}
		if strings.TrimPrefix(item, "W/") == etag {
			return true
		}
	}
	return false
}

// ifRangeMatches evaluates If-Range: the range applies only when the
// validator still matches.
func ifRangeMatches(r *http.Request, obj *object) bool {
	ir := strings.TrimSpace(r.Header.Get("If-Range"))
	if ir == "" {
		return true
	}
	if strings.HasPrefix(ir, `"`) {
		return ir == obj.etag
	}
	if strings.HasPrefix(ir, "W/") {
		return false
	}
	t, err := http.ParseTime(ir)
	return err == nil && !obj.modTime.IsZero() && obj.modTime.Truncate(time.Second).Equal(t)
}

type rangeResult int

const (
	rangeIgnore rangeResult = iota // absent, malformed or multiple ranges: serve everything
	rangeOK
	rangeUnsatisfiable
)

// parseRange parses a single "bytes=" range against size.
func parseRange(h string, size int64) (start, length int64, res rangeResult) {
	unit, spec, ok := strings.Cut(strings.TrimSpace(h), "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") {
		return 0, 0, rangeIgnore
	}
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, rangeIgnore
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, rangeIgnore
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		// Suffix range: the last n bytes.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, rangeIgnore
		}
		if n == 0 || size == 0 {
			return 0, 0, rangeUnsatisfiable
		}
		n = min(n, size)
		return size - n, n, rangeOK
	}
	a, err := strconv.ParseInt(first, 10, 64)
	if err != nil || a < 0 {
		return 0, 0, rangeIgnore
	}
	end := size - 1
	if last != "" {
		b, err := strconv.ParseInt(last, 10, 64)
		if err != nil || b < a {
			return 0, 0, rangeIgnore
		}
		end = min(b, size-1)
	}
	if a >= size {
		return 0, 0, rangeUnsatisfiable
	}
	return a, end - a + 1, rangeOK
}
