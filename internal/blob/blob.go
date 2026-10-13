// Package blob defines the object storage abstraction that holds site files,
// metadata and user uploads. Implementations live in sub-packages (fs, s3,
// gcs, azure); backends.Open builds one from configuration.
//
// Keys are "/"-separated, never start with "/", and are relative to the
// configured bucket prefix (implementations add/strip the prefix).
package blob

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

var (
	// ErrNotFound is returned when an object does not exist.
	ErrNotFound = errors.New("blob: not found")
	// ErrNotSupported is returned by optional operations (e.g. presigning on fs).
	ErrNotSupported = errors.New("blob: operation not supported")
	// ErrInvalidKey is returned for keys that are empty, absolute or contain "..".
	ErrInvalidKey = errors.New("blob: invalid key")
)

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	ETag        string    `json:"etag"` // opaque, changes whenever content changes; never empty
	ContentType string    `json:"content_type,omitempty"`
	ModTime     time.Time `json:"mod_time"`
}

// ListResult is the result of a List call.
type ListResult struct {
	Objects []ObjectInfo
	// Prefixes are the "directories" (ending in "/") found when a delimiter is used.
	Prefixes []string
}

// PresignPutOptions describes the object a presigned upload URL accepts.
//
// What a backend can bind into the URL differs (see each implementation):
//
//   - s3: Content-Type and the size are signed (Content-Length, or
//     Content-MD5 of the empty body for Size 0), so S3 rejects any other
//     size; SHA256 is signed as x-amz-checksum-sha256, which S3 verifies
//     against the body, wherever flexible checksums are enabled (AWS S3 by
//     default; custom endpoints only with AWS_REQUEST_CHECKSUM_CALCULATION).
//   - gcs: Content-Type and x-goog-content-length-range: Size,Size are
//     signed (V4), so GCS rejects any other size; GCS cannot verify SHA-256.
//   - azure: a SAS cannot restrict the size or content of an upload; only
//     the blob is fixed. Callers must verify uploads afterwards (the deploy
//     gateway compares sizes at commit and deletes mismatching objects).
//
// The zero value is not "anything goes": Size 0 means an empty body. Use
// Size -1 when the size is unknown.
type PresignPutOptions struct {
	// ContentType is signed into the URL and stored with the object; ""
	// derives it from the key's extension.
	ContentType string
	// Size is the exact body size in bytes, or -1 when unknown.
	Size int64
	// SHA256 is the lowercase or uppercase hex SHA-256 of the body; "" when
	// unknown. Ignored by backends that cannot enforce it.
	SHA256 string
}

// PresignedRequest is an upload/download URL usable without credentials.
type PresignedRequest struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"` // must be sent verbatim
	ExpiresAt time.Time         `json:"expires_at"`
}

// Store is an object store. Implementations must be safe for concurrent use.
type Store interface {
	// Get opens an object for reading. The caller must close the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, *ObjectInfo, error)
	// GetRange reads length bytes starting at offset (length < 0 = to end).
	GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *ObjectInfo, error)
	// Stat returns object metadata or ErrNotFound.
	Stat(ctx context.Context, key string) (*ObjectInfo, error)
	// Put writes an object, replacing any existing one. size may be -1 if unknown.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*ObjectInfo, error)
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every object whose key starts with prefix.
	DeletePrefix(ctx context.Context, prefix string) error
	// List returns all objects under prefix (all pages). With delimiter "/",
	// objects in "sub-directories" are folded into Prefixes; with "" the
	// listing is recursive. Results are sorted by key.
	List(ctx context.Context, prefix, delimiter string) (*ListResult, error)
	// PresignPut returns a URL that accepts a PUT of the object's bytes as
	// described by opts (content type, and as far as the backend can enforce
	// them, size and SHA-256). The returned Headers must be sent verbatim.
	// Returns ErrNotSupported when the backend cannot presign.
	PresignPut(ctx context.Context, key string, opts PresignPutOptions, ttl time.Duration) (*PresignedRequest, error)
	// PresignGet returns a time-limited download URL or ErrNotSupported.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (*PresignedRequest, error)
	// Ping checks connectivity/permissions cheaply (used by /readyz).
	Ping(ctx context.Context) error
	// Describe returns non-secret details advertised to CLIs for direct sync
	// (type, bucket/container, account, prefix, region).
	Describe() Description
	// Close releases resources.
	Close() error
}

// DigestReporter is optionally implemented by stores whose service keeps a
// SHA-256 it verified itself for some objects, so callers can learn an
// object's digest without reading it: S3 objects uploaded with
// x-amz-checksum-sha256 (as presigned uploads are where flexible checksums
// are enabled, see PresignPutOptions).
type DigestReporter interface {
	// StatSHA256 returns the object's metadata (as Stat) together with the
	// lowercase hex SHA-256 of its full content as verified by the storage
	// service, or "" when the service holds no such digest for this object
	// version. A digest always describes the object the returned info
	// describes. It returns ErrNotFound for a missing object and
	// ErrNotSupported when the store cannot report digests at all (callers
	// then read the object).
	StatSHA256(ctx context.Context, key string) (*ObjectInfo, string, error)
}

// Description is the public storage description included in /info.
type Description struct {
	Type      string `json:"type"`
	Bucket    string `json:"bucket,omitempty"`
	Container string `json:"container,omitempty"`
	Account   string `json:"account,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Region    string `json:"region,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// ValidKey reports whether key is acceptable to every backend.
func ValidKey(key string) bool {
	if key == "" || key[0] == '/' || len(key) > 1024 {
		return false
	}
	start := 0
	for i := 0; i <= len(key); i++ {
		if i == len(key) || key[i] == '/' {
			seg := key[start:i]
			if seg == "." || seg == ".." || (seg == "" && i != len(key)) {
				return false
			}
			start = i + 1
		} else if key[i] == 0 || key[i] == '\\' {
			return false
		}
	}
	return true
}

// DetectContentType guesses a content type from a file name (extension) and
// falls back to sniffing the first 512 bytes when head is non-nil.
func DetectContentType(name string, head []byte) string {
	if ct := ContentTypeByExt(name); ct != "" {
		return ct
	}
	if len(head) > 0 {
		return http.DetectContentType(head)
	}
	return "application/octet-stream"
}
