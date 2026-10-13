// Package blobutil holds helpers shared by the blob.Store implementations:
// key/prefix validation, content-type detection on streams, exact-size
// enforcement, chunked reading for multi-part uploads, synthetic ETags,
// presign TTL clamping and small concurrency utilities.
//
// It is internal to internal/blob so the backends stay consistent without
// widening the public blob API.
package blobutil

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/blob"
)

const (
	// DefaultPresignTTL is used when a caller passes a non-positive TTL.
	DefaultPresignTTL = 15 * time.Minute
	// MaxPresignTTL is the longest validity any backend supports (SigV4 and
	// GCS V4 signatures are limited to 7 days).
	MaxPresignTTL = 7 * 24 * time.Hour
	// SniffLen is the number of bytes http.DetectContentType looks at.
	SniffLen = 512
	// MaxKeyLen is the longest full object name (store prefix + key) that
	// S3, GCS and Azure accept, in bytes.
	MaxKeyLen = 1024
	// maxContentTypeLen bounds caller-supplied content types.
	maxContentTypeLen = 256
)

// ErrSizeMismatch is returned when a Put body is shorter or longer than the
// size the caller declared.
var ErrSizeMismatch = errors.New("blob: body size does not match declared size")

// CheckKey validates an object key. Beyond blob.ValidKey it also rejects keys
// that end in "/" (directories), are not valid UTF-8 or contain ASCII control
// characters, so every backend accepts exactly the same key space.
func CheckKey(key string) error {
	if !blob.ValidKey(key) || strings.HasSuffix(key, "/") || !cleanChars(key) {
		return fmt.Errorf("%w: %q", blob.ErrInvalidKey, truncate(key))
	}
	return nil
}

// CheckFullKey validates key with CheckKey and also rejects keys that become
// longer than MaxKeyLen once the store prefix is prepended (remote stores
// reject those with an error that would otherwise not be ErrInvalidKey).
func CheckFullKey(prefix, key string) error {
	if err := CheckKey(key); err != nil {
		return err
	}
	if len(prefix)+len(key) > MaxKeyLen {
		return fmt.Errorf("%w: %q is longer than %d bytes with the store prefix", blob.ErrInvalidKey, truncate(key), MaxKeyLen)
	}
	return nil
}

// CheckPrefix validates a List/DeletePrefix prefix. The empty prefix is
// accepted; anything else must be a valid key or a valid key followed by "/".
func CheckPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !blob.ValidKey(prefix) || !cleanChars(prefix) {
		return fmt.Errorf("%w: prefix %q", blob.ErrInvalidKey, truncate(prefix))
	}
	return nil
}

// CheckDeletePrefix validates a DeletePrefix prefix. The empty prefix is
// rejected: deleting the whole store is never what a caller means.
func CheckDeletePrefix(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("%w: refusing to delete with an empty prefix", blob.ErrInvalidKey)
	}
	return CheckPrefix(prefix)
}

// CheckDelimiter accepts "" (recursive listing) and "/".
func CheckDelimiter(delimiter string) error {
	if delimiter != "" && delimiter != "/" {
		return fmt.Errorf("%w: delimiter %q (only \"\" and \"/\" are supported)", blob.ErrNotSupported, delimiter)
	}
	return nil
}

// CheckRange validates GetRange arguments.
func CheckRange(offset int64) error {
	if offset < 0 {
		return fmt.Errorf("blob: invalid range: negative offset %d", offset)
	}
	return nil
}

func cleanChars(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func truncate(s string) string {
	const max = 128
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// CleanContentType returns contentType without surrounding whitespace, or
// "" when it cannot be used as an HTTP header value (control or non-ASCII
// characters, or longer than 256 bytes). Callers then fall back to
// detection, so a bad caller-supplied type never breaks a request or ends up
// in a signed header.
func CleanContentType(contentType string) string {
	ct := strings.TrimSpace(contentType)
	if len(ct) > maxContentTypeLen {
		return ""
	}
	for i := 0; i < len(ct); i++ {
		if c := ct[i]; c < 0x20 || c >= 0x7f {
			return ""
		}
	}
	return ct
}

// RedactURL renders raw for error messages without user info, query or
// fragment (which may carry credentials such as SAS tokens).
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable URL)"
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// NormPrefix normalizes a store prefix the same way config does: no leading
// or trailing slashes, then a single trailing "/" unless empty.
func NormPrefix(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// PublicEndpoint strips user info, query and fragment from an endpoint URL so
// it can be advertised without leaking credentials. Unparseable input yields
// "".
func PublicEndpoint(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return u.String()
}

// ClampTTL returns ttl bounded to (0, MaxPresignTTL], defaulting to
// DefaultPresignTTL.
func ClampTTL(ttl time.Duration) time.Duration {
	switch {
	case ttl <= 0:
		return DefaultPresignTTL
	case ttl > MaxPresignTTL:
		return MaxPresignTTL
	default:
		return ttl
	}
}

// PresignContentType is the content type to sign into an upload URL: the
// caller's choice (if usable, see CleanContentType), else one derived from
// the key's extension.
func PresignContentType(key, contentType string) string {
	if ct := CleanContentType(contentType); ct != "" {
		return ct
	}
	return blob.DetectContentType(key, nil)
}

// PresignSpec is a validated blob.PresignPutOptions.
type PresignSpec struct {
	// ContentType is the content type to sign (see PresignContentType).
	ContentType string
	// Size is the exact body size, or -1 when unknown.
	Size int64
	// SHA256 is the raw SHA-256 digest of the body, nil when unknown.
	SHA256 []byte
}

// emptySHA256 is the SHA-256 digest of the empty body.
var emptySHA256 = sha256.Sum256(nil)

// CheckPresignPut validates the options of a presigned upload to key: the
// size must be >= 0 or -1 (unknown), the SHA-256 (optional) 64 hex digits,
// and a 0-byte upload's SHA-256 that of the empty body.
func CheckPresignPut(key string, opts blob.PresignPutOptions) (PresignSpec, error) {
	spec := PresignSpec{ContentType: PresignContentType(key, opts.ContentType), Size: opts.Size}
	if opts.Size < -1 {
		return spec, fmt.Errorf("invalid upload size %d (want >= 0, or -1 when unknown)", opts.Size)
	}
	if opts.SHA256 == "" {
		return spec, nil
	}
	sum, err := hex.DecodeString(opts.SHA256)
	if err != nil || len(sum) != sha256.Size {
		return spec, fmt.Errorf("invalid upload sha256 %q (want 64 hex digits)", truncate(opts.SHA256))
	}
	if opts.Size == 0 && !bytes.Equal(sum, emptySHA256[:]) {
		return spec, fmt.Errorf("upload sha256 %s cannot be the digest of a 0-byte body", opts.SHA256)
	}
	spec.SHA256 = sum
	return spec, nil
}

// SyntheticETag derives a stable opaque ETag from size and modification time
// (nanoseconds). It is used by the fs backend and as a fallback when a remote
// store returns no ETag.
func SyntheticETag(size int64, mod time.Time) string {
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[:8], uint64(size))
	binary.BigEndian.PutUint64(buf[8:], uint64(mod.UnixNano()))
	sum := sha256.Sum256(buf[:])
	return hex.EncodeToString(sum[:8])
}

// CleanETag strips quotes and a weak-validator prefix from an HTTP ETag.
func CleanETag(etag string) string {
	etag = strings.TrimPrefix(strings.TrimSpace(etag), "W/")
	return strings.Trim(etag, `"`)
}

// ETagOr returns CleanETag(etag), or a synthetic ETag when it is empty.
func ETagOr(etag string, size int64, mod time.Time) string {
	if e := CleanETag(etag); e != "" {
		return e
	}
	return SyntheticETag(size, mod)
}

// ParseContentRangeTotal extracts the complete length from a Content-Range
// header such as "bytes 0-9/100". ok is false when absent or unknown ("*").
func ParseContentRangeTotal(cr string) (total int64, ok bool) {
	i := strings.LastIndexByte(cr, '/')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(cr[i+1:]), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// EmptyBody returns a reader with no content.
func EmptyBody() io.ReadCloser { return io.NopCloser(bytes.NewReader(nil)) }

// LimitedReadCloser limits rc to n bytes (n < 0 = unlimited) while keeping
// rc's Close.
func LimitedReadCloser(rc io.ReadCloser, n int64) io.ReadCloser {
	if n < 0 {
		return rc
	}
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(rc, n), rc}
}

// SortListResult sorts objects by key and prefixes lexically, in place, and
// removes duplicate prefixes.
func SortListResult(res *blob.ListResult) {
	sort.Slice(res.Objects, func(i, j int) bool { return res.Objects[i].Key < res.Objects[j].Key })
	sort.Strings(res.Prefixes)
	if len(res.Prefixes) > 1 {
		out := res.Prefixes[:1]
		for _, p := range res.Prefixes[1:] {
			if p != out[len(out)-1] {
				out = append(out, p)
			}
		}
		res.Prefixes = out
	}
}

// ListableKey reports whether a key returned by a remote listing can be
// surfaced as an object (it must be a key the store would accept, so callers
// can Get it; directory markers such as "foo/" are dropped).
func ListableKey(key string) bool { return CheckKey(key) == nil }

// ExactSize wraps r so that reading fails unless exactly size bytes are
// produced: a short body yields io.ErrUnexpectedEOF and a long one
// ErrSizeMismatch. A negative size returns r unchanged.
func ExactSize(r io.Reader, size int64) io.Reader {
	if size < 0 {
		return r
	}
	return &exactReader{r: r, remaining: size, declared: size}
}

type exactReader struct {
	r         io.Reader
	remaining int64
	declared  int64
	err       error
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.remaining == 0 {
		e.err = e.checkExhausted()
		return 0, e.err
	}
	if int64(len(p)) > e.remaining {
		p = p[:e.remaining]
	}
	n, err := e.r.Read(p)
	e.remaining -= int64(n)
	if err == io.EOF {
		if e.remaining > 0 {
			e.err = fmt.Errorf("%w: got %d of %d bytes: %w", ErrSizeMismatch, e.declared-e.remaining, e.declared, io.ErrUnexpectedEOF)
			return n, e.err
		}
		e.err = io.EOF
		return n, nil
	}
	return n, err
}

// checkExhausted verifies the source has no bytes beyond the declared size.
func (e *exactReader) checkExhausted() error {
	var one [1]byte
	for range 100 { // guard against readers that return (0, nil) forever
		n, err := e.r.Read(one[:])
		if n > 0 {
			return fmt.Errorf("%w: more than %d bytes", ErrSizeMismatch, e.declared)
		}
		if err == io.EOF {
			return io.EOF
		}
		if err != nil {
			return err
		}
	}
	return io.ErrNoProgress
}

// ContextReader returns a reader that fails with ctx.Err() once ctx is done.
func ContextReader(ctx context.Context, r io.Reader) io.Reader {
	if ctx.Done() == nil {
		return r
	}
	return &ctxReader{ctx: ctx, r: r}
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Body is a buffered upload body that can report its content type (sniffing
// the first bytes without consuming them) and be read in fixed-size chunks.
type Body struct {
	br  *bufio.Reader
	buf bytes.Buffer
}

// NewBody wraps r (already size-checked by the caller if desired).
func NewBody(r io.Reader) *Body {
	return &Body{br: bufio.NewReaderSize(r, SniffLen)}
}

// Reader exposes the buffered stream (after any peeks).
func (b *Body) Reader() io.Reader { return b.br }

// ContentType returns contentType (if usable, see CleanContentType), or a
// type derived from the key's extension, or one sniffed from the first bytes
// of the body.
func (b *Body) ContentType(key, contentType string) (string, error) {
	if ct := CleanContentType(contentType); ct != "" {
		return ct, nil
	}
	if ct := blob.ContentTypeByExt(key); ct != "" {
		return ct, nil
	}
	head, err := b.br.Peek(SniffLen)
	if err != nil && err != io.EOF && !errors.Is(err, bufio.ErrBufferFull) {
		return "", err
	}
	return blob.DetectContentType(key, head), nil
}

// NextChunk reads up to max bytes. last reports whether the body is now
// exhausted. The returned slice is only valid until the next call. hint, when
// positive, pre-sizes the buffer.
func (b *Body) NextChunk(max int64, hint int64) (chunk []byte, last bool, err error) {
	b.buf.Reset()
	if hint > 0 {
		if hint > max {
			hint = max
		}
		b.buf.Grow(int(hint))
	}
	n, err := io.CopyN(&b.buf, b.br, max)
	if errors.Is(err, io.EOF) {
		return b.buf.Bytes(), true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if n < max {
		return b.buf.Bytes(), true, nil
	}
	if _, err := b.br.Peek(1); err == io.EOF {
		return b.buf.Bytes(), true, nil
	} else if err != nil {
		return nil, false, err
	}
	return b.buf.Bytes(), false, nil
}

// ForEach runs fn for every item with at most workers goroutines and returns
// the first error (all items are still attempted unless ctx is cancelled).
func ForEach[T any](ctx context.Context, items []T, workers int, fn func(context.Context, T) error) error {
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
		next     int
	)
	take := func() (T, bool) {
		mu.Lock()
		defer mu.Unlock()
		var zero T
		if next >= len(items) || ctx.Err() != nil {
			return zero, false
		}
		it := items[next]
		next++
		return it, true
	}
	for range workers {
		wg.Go(func() {
			for {
				it, ok := take()
				if !ok {
					return
				}
				if err := fn(ctx, it); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
