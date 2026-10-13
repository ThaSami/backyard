// Package gcs implements blob.Store on Google Cloud Storage.
//
// Authentication uses Application Default Credentials (Workload Identity,
// Cloud Run / GCE metadata, GOOGLE_APPLICATION_CREDENTIALS, gcloud user
// credentials) unless a credentials file is configured. An endpoint override
// targets emulators (fake-gcs-server); plain-http endpoints are used without
// authentication, and presigning is then unavailable.
//
// ETags are derived from the object generation, which changes on every
// write, so Get, GetRange, Stat, List and Put report identical ETags for the
// same object version.
//
// Presigned URLs are V4 signed URLs: with a service-account key they are
// signed locally, otherwise (Cloud Run, GCE, GKE Workload Identity) through
// the IAM Credentials SignBlob API as cfg.SignerEmail (or the detected
// default service account), which needs roles/iam.serviceAccountTokenCreator
// on that account.
package gcs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/internal/blobutil"
	"github.com/thasami/backyard/internal/config"
)

const (
	// deleteWorkers bounds concurrent deletes in DeletePrefix.
	deleteWorkers = 16
	// deletePage is how many names DeletePrefix collects before deleting.
	deletePage = 1000
	// maxChunkSize is the resumable-upload chunk (and buffer) size.
	maxChunkSize = 16 << 20
	// unknownSizeChunk is used when the upload size is not known.
	unknownSizeChunk = 8 << 20
	// minChunk is the granularity GCS rounds chunk sizes to.
	minChunk = 256 << 10
)

// Option configures a Store.
type Option func(*options)

type options struct {
	clientOpts []option.ClientOption
	logger     *slog.Logger
}

// WithClientOptions adds Google API client options (applied last).
func WithClientOptions(opts ...option.ClientOption) Option {
	return func(o *options) { o.clientOpts = append(o.clientOpts, opts...) }
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// Store is a blob.Store backed by a GCS bucket (optionally under a key
// prefix). It is safe for concurrent use.
type Store struct {
	client   *storage.Client
	bucket   *storage.BucketHandle
	name     string
	prefix   string
	endpoint string
	signer   string
	canSign  bool
	log      *slog.Logger

	closeOnce sync.Once
	closeErr  error
}

var _ blob.Store = (*Store)(nil)

// New creates a Store from configuration. It does not contact GCS; use Ping
// to verify access.
func New(ctx context.Context, cfg config.GCSConfig, opts ...Option) (*Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("blob/gcs: bucket is required")
	}
	o := options{logger: slog.Default()}
	for _, fn := range opts {
		fn(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	var copts []option.ClientOption
	canSign := os.Getenv("STORAGE_EMULATOR_HOST") == ""
	if cfg.CredentialsFile != "" {
		ct, err := credentialsType(cfg.CredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("blob/gcs: %w", err)
		}
		copts = append(copts, option.WithAuthCredentialsFile(ct, cfg.CredentialsFile))
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("blob/gcs: invalid endpoint %q", blobutil.RedactURL(endpoint))
		}
		copts = append(copts, option.WithEndpoint(endpoint))
		if u.Scheme == "http" {
			// Emulators speak plain HTTP; never send credentials in clear
			// text. Reads use the JSON API, which emulators implement
			// fully.
			if cfg.CredentialsFile == "" {
				copts = append(copts, option.WithoutAuthentication())
			}
			copts = append(copts, storage.WithJSONReads())
			canSign = false
		}
	}
	copts = append(copts, o.clientOpts...)
	client, err := storage.NewClient(ctx, copts...)
	if err != nil {
		return nil, fmt.Errorf("blob/gcs: create client: %w", err)
	}
	return &Store{
		client:   client,
		bucket:   client.Bucket(cfg.Bucket),
		name:     cfg.Bucket,
		prefix:   blobutil.NormPrefix(cfg.Prefix),
		endpoint: endpoint,
		signer:   cfg.SignerEmail,
		canSign:  canSign,
		log:      o.logger.With("component", "blob", "backend", "gcs", "bucket", cfg.Bucket),
	}, nil
}

// credentialsType reads the "type" of a credentials JSON file so it can be
// loaded with an explicit, validated type.
func credentialsType(path string) (option.CredentialsType, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read credentials file: %w", err)
	}
	defer f.Close()
	var v struct {
		Type string `json:"type"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&v); err != nil {
		return "", fmt.Errorf("parse credentials file %q: %w", path, err)
	}
	switch t := option.CredentialsType(v.Type); t {
	case option.ServiceAccount, option.AuthorizedUser, option.ImpersonatedServiceAccount, option.ExternalAccount:
		return t, nil
	default:
		return "", fmt.Errorf("credentials file %q has unsupported type %q", path, v.Type)
	}
}

// Client exposes the underlying storage client (for tests and tooling).
func (s *Store) Client() *storage.Client { return s.client }

func (s *Store) object(key string) *storage.ObjectHandle {
	return s.bucket.Object(s.prefix + key)
}

// checkKey validates key, including its length once prefixed.
func (s *Store) checkKey(key string) error { return blobutil.CheckFullKey(s.prefix, key) }

// genETag renders an object generation as an ETag.
func genETag(gen, size int64, mod time.Time) string {
	if gen == 0 {
		return blobutil.SyntheticETag(size, mod)
	}
	return strconv.FormatInt(gen, 10)
}

func errCode(err error) int {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code
	}
	return 0
}

func (s *Store) wrap(ctx context.Context, op, key string, err error) error {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return fmt.Errorf("%w: %q", blob.ErrNotFound, key)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("blob/gcs: %s %q: %w", op, key, err)
}

func infoFromAttrs(key string, a *storage.ObjectAttrs) *blob.ObjectInfo {
	return &blob.ObjectInfo{
		Key:         key,
		Size:        a.Size,
		ETag:        genETag(a.Generation, a.Size, a.Updated),
		ContentType: a.ContentType,
		ModTime:     a.Updated.UTC(),
	}
}

// Get opens an object for reading.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, *blob.ObjectInfo, error) {
	return s.get(ctx, key, 0, -1)
}

// GetRange reads length bytes from offset (length < 0 = to the end). The
// returned ObjectInfo describes the whole object; ranges past the end yield
// an empty body.
func (s *Store) GetRange(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := blobutil.CheckRange(offset); err != nil {
		return nil, nil, err
	}
	return s.get(ctx, key, offset, length)
}

func (s *Store) get(ctx context.Context, key string, offset, length int64) (io.ReadCloser, *blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, nil, err
	}
	if length == 0 {
		info, err := s.Stat(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		return blobutil.EmptyBody(), info, nil
	}
	obj := s.object(key)
	var (
		r   *storage.Reader
		err error
	)
	if offset == 0 && length < 0 {
		r, err = obj.NewReader(ctx)
	} else {
		r, err = obj.NewRangeReader(ctx, offset, length)
	}
	if err != nil {
		if errCode(err) == http.StatusRequestedRangeNotSatisfiable {
			info, serr := s.Stat(ctx, key)
			if serr != nil {
				return nil, nil, serr
			}
			if offset >= info.Size {
				return blobutil.EmptyBody(), info, nil
			}
		}
		return nil, nil, s.wrap(ctx, "get", key, err)
	}
	a := r.Attrs
	return r, &blob.ObjectInfo{
		Key:         key,
		Size:        a.Size,
		ETag:        genETag(a.Generation, a.Size, a.LastModified),
		ContentType: a.ContentType,
		ModTime:     a.LastModified.UTC(),
	}, nil
}

// Stat returns object metadata or blob.ErrNotFound.
func (s *Store) Stat(ctx context.Context, key string) (*blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	a, err := s.object(key).Attrs(ctx)
	if err != nil {
		return nil, s.wrap(ctx, "stat", key, err)
	}
	return infoFromAttrs(key, a), nil
}

func chunkSizeFor(size int64) int {
	switch {
	case size < 0:
		return unknownSizeChunk
	case size >= maxChunkSize:
		return maxChunkSize
	default:
		// One request, with a buffer no larger than the object needs.
		n := (size/minChunk + 1) * minChunk
		return int(n)
	}
}

// Put writes an object. When contentType is empty it is derived from the key
// extension or content.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("blob/gcs: put: nil reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := blobutil.NewBody(blobutil.ContextReader(ctx, blobutil.ExactSize(r, size)))
	ct, err := body.ContentType(key, contentType)
	if err != nil {
		return nil, fmt.Errorf("blob/gcs: put %q: %w", key, err)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Overwriting a whole object is idempotent, so uploads may be retried.
	obj := s.object(key).Retryer(storage.WithPolicy(storage.RetryAlways))
	w := obj.NewWriter(wctx)
	w.ContentType = ct
	w.ChunkSize = chunkSizeFor(size)
	if _, err := io.Copy(w, body.Reader()); err != nil {
		cancel() // abort the upload so no partial object is committed
		_ = w.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("blob/gcs: put %q: %w", key, err)
	}
	if err := w.Close(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("blob/gcs: put %q: %w", key, err)
	}
	a := w.Attrs()
	if a == nil {
		return s.Stat(ctx, key)
	}
	return infoFromAttrs(key, a), nil
}

// Delete removes an object; deleting a missing object is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.checkKey(key); err != nil {
		return err
	}
	if err := s.object(key).Delete(ctx); err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("blob/gcs: delete %q: %w", key, err)
	}
	return nil
}

// DeletePrefix removes every object whose key starts with prefix (deletes
// run concurrently). An empty prefix is rejected.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	if err := blobutil.CheckDeletePrefix(prefix); err != nil {
		return err
	}
	q := &storage.Query{Prefix: s.prefix + prefix}
	if err := q.SetAttrSelection([]string{"Name"}); err != nil {
		return fmt.Errorf("blob/gcs: delete prefix %q: %w", prefix, err)
	}
	del := func(ctx context.Context, name string) error {
		err := s.bucket.Object(name).Delete(ctx)
		if err != nil && !errors.Is(err, storage.ErrObjectNotExist) {
			return fmt.Errorf("delete %q: %w", name, err)
		}
		return nil
	}
	it := s.bucket.Objects(ctx, q)
	names := make([]string, 0, deletePage)
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("blob/gcs: delete prefix %q: list: %w", prefix, err)
		}
		if a.Name == "" {
			continue
		}
		names = append(names, a.Name)
		if len(names) == deletePage {
			if err := blobutil.ForEach(ctx, names, deleteWorkers, del); err != nil {
				return fmt.Errorf("blob/gcs: delete prefix %q: %w", prefix, err)
			}
			names = names[:0]
		}
	}
	if err := blobutil.ForEach(ctx, names, deleteWorkers, del); err != nil {
		return fmt.Errorf("blob/gcs: delete prefix %q: %w", prefix, err)
	}
	return nil
}

// List returns all objects under prefix, sorted by key. With delimiter "/",
// keys below the next "/" are folded into Prefixes.
func (s *Store) List(ctx context.Context, prefix, delimiter string) (*blob.ListResult, error) {
	if err := blobutil.CheckPrefix(prefix); err != nil {
		return nil, err
	}
	if err := blobutil.CheckDelimiter(delimiter); err != nil {
		return nil, err
	}
	q := &storage.Query{Prefix: s.prefix + prefix, Delimiter: delimiter}
	if err := q.SetAttrSelection([]string{"Name", "Size", "ContentType", "Updated", "Generation"}); err != nil {
		return nil, fmt.Errorf("blob/gcs: list %q: %w", prefix, err)
	}
	res := &blob.ListResult{}
	it := s.bucket.Objects(ctx, q)
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("blob/gcs: list %q: %w", prefix, err)
		}
		if a.Prefix != "" {
			if p, ok := strings.CutPrefix(a.Prefix, s.prefix); ok && p != "" {
				res.Prefixes = append(res.Prefixes, p)
			}
			continue
		}
		key, ok := strings.CutPrefix(a.Name, s.prefix)
		if !ok || !blobutil.ListableKey(key) {
			continue
		}
		res.Objects = append(res.Objects, *infoFromAttrs(key, a))
	}
	blobutil.SortListResult(res)
	return res, nil
}

func (s *Store) signedURL(key, method, contentType string, ttl time.Duration) (string, time.Time, error) {
	if !s.canSign {
		return "", time.Time{}, fmt.Errorf("%w: GCS signed URLs are unavailable without credentials (emulator endpoint)", blob.ErrNotSupported)
	}
	ttl = blobutil.ClampTTL(ttl)
	expires := time.Now().Add(ttl)
	u, err := s.bucket.SignedURL(s.prefix+key, &storage.SignedURLOptions{
		GoogleAccessID: s.signer,
		Method:         method,
		ContentType:    contentType,
		Expires:        expires,
		Scheme:         storage.SigningSchemeV4,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return u, expires.UTC(), nil
}

// PresignPut returns a V4 signed URL accepting a PUT of the object's bytes.
// Headers carries the signed Content-Type, which must be sent verbatim.
func (s *Store) PresignPut(_ context.Context, key, contentType string, ttl time.Duration) (*blob.PresignedRequest, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	ct := blobutil.PresignContentType(key, contentType)
	u, exp, err := s.signedURL(key, http.MethodPut, ct, ttl)
	if err != nil {
		if errors.Is(err, blob.ErrNotSupported) {
			return nil, err
		}
		return nil, fmt.Errorf("blob/gcs: presign put %q: %w", key, err)
	}
	return &blob.PresignedRequest{
		Method:    http.MethodPut,
		URL:       u,
		Headers:   map[string]string{"Content-Type": ct},
		ExpiresAt: exp,
	}, nil
}

// PresignGet returns a V4 signed download URL.
func (s *Store) PresignGet(_ context.Context, key string, ttl time.Duration) (*blob.PresignedRequest, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	u, exp, err := s.signedURL(key, http.MethodGet, "", ttl)
	if err != nil {
		if errors.Is(err, blob.ErrNotSupported) {
			return nil, err
		}
		return nil, fmt.Errorf("blob/gcs: presign get %q: %w", key, err)
	}
	return &blob.PresignedRequest{Method: http.MethodGet, URL: u, ExpiresAt: exp}, nil
}

// Ping checks bucket access: bucket metadata, falling back to listing one
// object when the identity may read objects but not bucket metadata (e.g.
// roles/storage.objectAdmin).
func (s *Store) Ping(ctx context.Context) error {
	_, err := s.bucket.Attrs(ctx)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errCode(err) == http.StatusForbidden {
		it := s.bucket.Objects(ctx, &storage.Query{Prefix: s.prefix})
		it.PageInfo().MaxSize = 1
		_, lerr := it.Next()
		if lerr == nil || errors.Is(lerr, iterator.Done) {
			return nil
		}
		err = lerr
	}
	return fmt.Errorf("blob/gcs: ping bucket %q: %w", s.name, err)
}

// Describe returns the public storage description.
func (s *Store) Describe() blob.Description {
	return blob.Description{Type: "gcs", Bucket: s.name, Prefix: s.prefix, Endpoint: blobutil.PublicEndpoint(s.endpoint)}
}

// Close releases the client. It is safe to call more than once.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.client.Close() })
	return s.closeErr
}
