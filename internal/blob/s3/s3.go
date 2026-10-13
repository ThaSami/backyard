// Package s3 implements blob.Store on Amazon S3 and S3-compatible object
// stores (MinIO, Cloudflare R2, Ceph RGW, Backblaze B2, ...).
//
// Credentials come from the default AWS chain (environment, shared config,
// web identity / IRSA, ECS and EC2 roles) unless static keys are configured.
// Cloudflare R2 (the bucket option on Cloudflare's free tier) is configured
// with endpoint https://<account>.r2.cloudflarestorage.com and an R2 API
// token's access key pair; the region then defaults to "auto".
//
// A custom endpoint switches to path-style addressing and, unless
// AWS_REQUEST_CHECKSUM_CALCULATION / AWS_RESPONSE_CHECKSUM_VALIDATION say
// otherwise, only sends checksums where the API requires them (many
// S3-compatible stores reject the newer flexible-checksum headers).
//
// Uploads of unknown or large size are sent as multipart uploads; ETags are
// the store's ETags with quotes removed. Writes use SSE-KMS when a key id is
// configured (presigned uploads then carry the required headers).
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	smithymiddleware "github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/internal/blobutil"
	"github.com/thasami/backyard/internal/config"
)

const (
	// DefaultPartSize is the multipart chunk size (and the largest object
	// sent with a single PutObject).
	DefaultPartSize = 8 << 20
	// MinPartSize is S3's minimum size for all but the last part.
	MinPartSize = 5 << 20
	maxParts    = 10000
	// deleteBatch is the DeleteObjects limit.
	deleteBatch = 1000
	// cleanupTimeout bounds best-effort cleanup (aborting uploads).
	cleanupTimeout = 30 * time.Second
)

// Option configures a Store.
type Option func(*options)

type options struct {
	httpClient *http.Client
	partSize   int64
	logger     *slog.Logger
	optFns     []func(*awss3.Options)
}

// WithHTTPClient sets the HTTP client used for S3 requests.
func WithHTTPClient(c *http.Client) Option { return func(o *options) { o.httpClient = c } }

// WithPartSize sets the multipart part size (minimum 5 MiB).
func WithPartSize(n int64) Option {
	return func(o *options) { o.partSize = max(n, MinPartSize) }
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithClientOptions adds raw aws-sdk-go-v2 S3 client options (applied last).
func WithClientOptions(fns ...func(*awss3.Options)) Option {
	return func(o *options) { o.optFns = append(o.optFns, fns...) }
}

// Store is a blob.Store backed by an S3 bucket (optionally under a key
// prefix). It is safe for concurrent use.
type Store struct {
	client        *awss3.Client
	presign       *awss3.PresignClient
	bucket        string
	prefix        string
	region        string
	endpoint      string
	kmsKeyID      string
	partSize      int64
	partChecksums bool
	log           *slog.Logger
}

var _ blob.Store = (*Store)(nil)

// New creates a Store from configuration. It does not contact S3; use Ping
// to verify access.
func New(ctx context.Context, cfg config.S3Config, opts ...Option) (*Store, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("blob/s3: bucket is required")
	}
	o := options{partSize: DefaultPartSize, logger: slog.Default()}
	for _, fn := range opts {
		fn(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("blob/s3: invalid endpoint %q (want an absolute URL such as https://<account>.r2.cloudflarestorage.com)", blobutil.RedactURL(endpoint))
		}
	}

	var loadOpts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
			return nil, errors.New("blob/s3: access_key_id and secret_access_key must be set together")
		}
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("blob/s3: load AWS config: %w", err)
	}
	switch {
	case cfg.Region != "":
		// Explicitly configured.
	case isR2(endpoint):
		// R2 only accepts "auto" (or its aliases ""/us-east-1); an ambient
		// AWS region (AWS_REGION, ~/.aws/config) would break signing.
		awsCfg.Region = "auto"
	case awsCfg.Region == "":
		awsCfg.Region = defaultRegion(endpoint)
	}

	client := awss3.NewFromConfig(awsCfg, func(so *awss3.Options) {
		if endpoint != "" {
			so.BaseEndpoint = aws.String(endpoint)
			if os.Getenv("AWS_REQUEST_CHECKSUM_CALCULATION") == "" {
				so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			}
			if os.Getenv("AWS_RESPONSE_CHECKSUM_VALIDATION") == "" {
				so.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
			}
		}
		so.UsePathStyle = cfg.ForcePathStyle || endpoint != ""
		if o.httpClient != nil {
			so.HTTPClient = o.httpClient
		}
		so.Logger = sdkLogger{o.logger}
		so.DisableLogOutputChecksumValidationSkipped = true
	}, func(so *awss3.Options) {
		for _, fn := range o.optFns {
			fn(so)
		}
	})
	s := &Store{
		client:        client,
		presign:       awss3.NewPresignClient(client),
		bucket:        cfg.Bucket,
		prefix:        blobutil.NormPrefix(cfg.Prefix),
		region:        awsCfg.Region,
		endpoint:      endpoint,
		kmsKeyID:      cfg.SSEKMSKeyID,
		partSize:      o.partSize,
		partChecksums: client.Options().RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired,
		log:           o.logger.With("component", "blob", "backend", "s3", "bucket", cfg.Bucket),
	}
	return s, nil
}

// defaultRegion picks a region when none is configured or discoverable:
// "auto" for Cloudflare R2, else us-east-1 (S3's global default).
func defaultRegion(endpoint string) string {
	if isR2(endpoint) {
		return "auto"
	}
	return "us-east-1"
}

// isR2 reports whether endpoint is a Cloudflare R2 S3 API endpoint
// (https://<account>.r2.cloudflarestorage.com, including jurisdiction
// subdomains such as <account>.eu.r2.cloudflarestorage.com).
func isR2(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Hostname()), ".r2.cloudflarestorage.com")
}

// sdkLogger routes aws-sdk-go-v2 log output to slog.
type sdkLogger struct{ l *slog.Logger }

func (a sdkLogger) Logf(c logging.Classification, format string, v ...any) {
	lvl := slog.LevelDebug
	if c == logging.Warn {
		lvl = slog.LevelWarn
	}
	a.l.Log(context.Background(), lvl, fmt.Sprintf(format, v...), "component", "blob", "backend", "s3")
}

// Client exposes the underlying S3 client (for tests and tooling).
func (s *Store) Client() *awss3.Client { return s.client }

func (s *Store) fullKey(key string) string { return s.prefix + key }

// checkKey validates key, including its length once prefixed.
func (s *Store) checkKey(key string) error { return blobutil.CheckFullKey(s.prefix, key) }

// isNotFound reports whether err means the object (not the bucket) is
// missing.
func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		case "NoSuchBucket":
			return false
		}
	}
	var respErr *smithyhttp.ResponseError
	if errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusNotFound {
		return apiErr == nil || apiErr.ErrorCode() != "NoSuchBucket"
	}
	return false
}

func isInvalidRange(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidRange" {
		return true
	}
	var respErr *smithyhttp.ResponseError
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable
}

func (s *Store) wrap(op, key string, err error) error {
	if isNotFound(err) {
		return fmt.Errorf("%w: %q", blob.ErrNotFound, key)
	}
	return fmt.Errorf("blob/s3: %s %q: %w", op, key, err)
}

func serverTime(md smithymiddleware.Metadata) time.Time {
	if t, ok := awsmiddleware.GetServerTime(md); ok && !t.IsZero() {
		return t.UTC()
	}
	return time.Now().UTC()
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
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
	in := &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.fullKey(key))}
	ranged := offset > 0 || length > 0
	if ranged {
		if length > 0 {
			in.Range = aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		} else {
			in.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
		}
	}
	var optFns []func(*awss3.Options)
	if ranged {
		// Whole-object checksums cannot validate a partial body.
		optFns = append(optFns, func(o *awss3.Options) {
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		})
	}
	out, err := s.client.GetObject(ctx, in, optFns...)
	if err != nil {
		if ranged && isInvalidRange(err) {
			info, serr := s.Stat(ctx, key)
			if serr != nil {
				return nil, nil, serr
			}
			if offset >= info.Size {
				return blobutil.EmptyBody(), info, nil
			}
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, s.wrap("get", key, err)
	}
	size := deref(out.ContentLength)
	total, partial := blobutil.ParseContentRangeTotal(deref(out.ContentRange))
	if partial {
		size = total
	}
	mod := deref(out.LastModified)
	info := &blob.ObjectInfo{
		Key:         key,
		Size:        size,
		ETag:        blobutil.ETagOr(deref(out.ETag), size, mod),
		ContentType: deref(out.ContentType),
		ModTime:     mod.UTC(),
	}
	body := out.Body
	if ranged && !partial {
		// Some S3-compatible stores (and caches in front of them) ignore
		// Range and send the whole object: cut the range out locally.
		if offset >= size {
			body.Close()
			return blobutil.EmptyBody(), info, nil
		}
		if _, err := io.CopyN(io.Discard, body, offset); err != nil {
			body.Close()
			return nil, nil, s.wrap("get", key, err)
		}
		body = blobutil.LimitedReadCloser(body, length)
	}
	return body, info, nil
}

// Stat returns object metadata or blob.ErrNotFound.
func (s *Store) Stat(ctx context.Context, key string) (*blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.fullKey(key))})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, s.wrap("stat", key, err)
	}
	size := deref(out.ContentLength)
	mod := deref(out.LastModified)
	return &blob.ObjectInfo{
		Key:         key,
		Size:        size,
		ETag:        blobutil.ETagOr(deref(out.ETag), size, mod),
		ContentType: deref(out.ContentType),
		ModTime:     mod.UTC(),
	}, nil
}

// Put writes an object. Bodies up to the part size are sent with a single
// PutObject; larger or unknown-size bodies use a multipart upload. When
// contentType is empty it is derived from the key extension or content.
func (s *Store) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (*blob.ObjectInfo, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("blob/s3: put: nil reader")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body := blobutil.NewBody(blobutil.ContextReader(ctx, blobutil.ExactSize(r, size)))
	ct, err := body.ContentType(key, contentType)
	if err != nil {
		return nil, fmt.Errorf("blob/s3: put %q: %w", key, err)
	}
	partSize := s.partSize
	if size > partSize*maxParts {
		partSize = (size + maxParts - 1) / maxParts
	}
	chunk, last, err := body.NextChunk(partSize, size)
	if err != nil {
		return nil, fmt.Errorf("blob/s3: put %q: %w", key, err)
	}
	if last {
		return s.putSingle(ctx, key, chunk, ct)
	}
	return s.putMultipart(ctx, key, body, chunk, partSize, size, ct)
}

func (s *Store) putSingle(ctx context.Context, key string, data []byte, ct string) (*blob.ObjectInfo, error) {
	in := &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.fullKey(key)),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(ct),
	}
	if s.kmsKeyID != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(s.kmsKeyID)
	}
	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("blob/s3: put %q: %w", key, err)
	}
	mod := serverTime(out.ResultMetadata)
	n := int64(len(data))
	return &blob.ObjectInfo{Key: key, Size: n, ETag: blobutil.ETagOr(deref(out.ETag), n, mod), ContentType: ct, ModTime: mod}, nil
}

func (s *Store) putMultipart(ctx context.Context, key string, body *blobutil.Body, first []byte, partSize, size int64, ct string) (_ *blob.ObjectInfo, retErr error) {
	full := s.fullKey(key)
	cin := &awss3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(full),
		ContentType: aws.String(ct),
	}
	if s.kmsKeyID != "" {
		cin.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		cin.SSEKMSKeyId = aws.String(s.kmsKeyID)
	}
	if s.partChecksums {
		cin.ChecksumAlgorithm = types.ChecksumAlgorithmCrc32
	}
	created, err := s.client.CreateMultipartUpload(ctx, cin)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("blob/s3: put %q: create multipart upload: %w", key, err)
	}
	uploadID := created.UploadId
	defer func() {
		if retErr == nil {
			return
		}
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		if _, err := s.client.AbortMultipartUpload(actx, &awss3.AbortMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(full), UploadId: uploadID,
		}); err != nil {
			s.log.Warn("aborting multipart upload failed", "key", key, "err", err)
		}
	}()

	var (
		parts []types.CompletedPart
		total int64
		chunk = first
		last  bool
	)
	for num := int32(1); ; num++ {
		if num > maxParts {
			return nil, fmt.Errorf("blob/s3: put %q: object exceeds %d parts", key, maxParts)
		}
		pin := &awss3.UploadPartInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(full),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(num),
			Body:          bytes.NewReader(chunk),
			ContentLength: aws.Int64(int64(len(chunk))),
		}
		if s.partChecksums {
			pin.ChecksumAlgorithm = types.ChecksumAlgorithmCrc32
		}
		out, err := s.client.UploadPart(ctx, pin)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("blob/s3: put %q: upload part %d: %w", key, num, err)
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num), ChecksumCRC32: out.ChecksumCRC32})
		total += int64(len(chunk))
		if last {
			break
		}
		chunk, last, err = body.NextChunk(partSize, size-total)
		if err != nil {
			return nil, fmt.Errorf("blob/s3: put %q: %w", key, err)
		}
		if len(chunk) == 0 {
			break
		}
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &awss3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(full),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("blob/s3: put %q: complete multipart upload: %w", key, err)
	}
	// Some S3-compatible stores report a different ETag on reads than in
	// the completion response; HEAD gives the authoritative metadata.
	info, err := s.Stat(ctx, key)
	if err == nil {
		return info, nil
	}
	s.log.Debug("stat after multipart upload failed", "key", key, "err", err)
	mod := serverTime(out.ResultMetadata)
	return &blob.ObjectInfo{Key: key, Size: total, ETag: blobutil.ETagOr(deref(out.ETag), total, mod), ContentType: ct, ModTime: mod}, nil
}

// Delete removes an object; deleting a missing object is not an error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.checkKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.fullKey(key))})
	if err != nil && !isNotFound(err) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("blob/s3: delete %q: %w", key, err)
	}
	return nil
}

// DeletePrefix removes every object whose key starts with prefix using
// batched DeleteObjects calls. An empty prefix is rejected.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	if err := blobutil.CheckDeletePrefix(prefix); err != nil {
		return err
	}
	p := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.fullKey(prefix)),
	})
	batch := make([]types.ObjectIdentifier, 0, deleteBatch)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		in := &awss3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		}
		if s.partChecksums {
			in.ChecksumAlgorithm = types.ChecksumAlgorithmCrc32
		}
		var optFns []func(*awss3.Options)
		if s.endpoint != "" {
			// Older S3-compatible stores insist on Content-MD5 for
			// DeleteObjects and ignore the flexible checksum headers.
			optFns = append(optFns, func(o *awss3.Options) {
				o.APIOptions = append(o.APIOptions, smithyhttp.AddContentChecksumMiddleware)
			})
		}
		out, err := s.client.DeleteObjects(ctx, in, optFns...)
		if err != nil {
			return err
		}
		for _, e := range out.Errors {
			if deref(e.Code) == "NoSuchKey" {
				continue
			}
			return fmt.Errorf("deleting %q: %s: %s", deref(e.Key), deref(e.Code), deref(e.Message))
		}
		batch = batch[:0]
		return nil
	}
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("blob/s3: delete prefix %q: list: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			batch = append(batch, types.ObjectIdentifier{Key: obj.Key})
			if len(batch) == deleteBatch {
				if err := flush(); err != nil {
					return fmt.Errorf("blob/s3: delete prefix %q: %w", prefix, err)
				}
			}
		}
	}
	if err := flush(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("blob/s3: delete prefix %q: %w", prefix, err)
	}
	return nil
}

// List returns all objects under prefix, sorted by key. With delimiter "/",
// keys below the next "/" are folded into Prefixes. S3 listings carry no
// content type, so ObjectInfo.ContentType is empty.
func (s *Store) List(ctx context.Context, prefix, delimiter string) (*blob.ListResult, error) {
	if err := blobutil.CheckPrefix(prefix); err != nil {
		return nil, err
	}
	if err := blobutil.CheckDelimiter(delimiter); err != nil {
		return nil, err
	}
	in := &awss3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(s.fullKey(prefix))}
	if delimiter != "" {
		in.Delimiter = aws.String(delimiter)
	}
	res := &blob.ListResult{}
	p := awss3.NewListObjectsV2Paginator(s.client, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("blob/s3: list %q: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			key, ok := strings.CutPrefix(deref(obj.Key), s.prefix)
			if !ok || !blobutil.ListableKey(key) {
				continue
			}
			size := deref(obj.Size)
			mod := deref(obj.LastModified)
			res.Objects = append(res.Objects, blob.ObjectInfo{
				Key:     key,
				Size:    size,
				ETag:    blobutil.ETagOr(deref(obj.ETag), size, mod),
				ModTime: mod.UTC(),
			})
		}
		for _, cp := range page.CommonPrefixes {
			if pfx, ok := strings.CutPrefix(deref(cp.Prefix), s.prefix); ok && pfx != "" {
				res.Prefixes = append(res.Prefixes, pfx)
			}
		}
	}
	blobutil.SortListResult(res)
	return res, nil
}

// PresignPut returns a URL accepting a PUT of the object's bytes. The
// returned Headers (Content-Type, and SSE headers when SSE-KMS is enabled)
// are part of the signature and must be sent verbatim.
func (s *Store) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (*blob.PresignedRequest, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	ttl = blobutil.ClampTTL(ttl)
	ct := blobutil.PresignContentType(key, contentType)
	in := &awss3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(s.fullKey(key)),
		ContentType: aws.String(ct),
	}
	if s.kmsKeyID != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(s.kmsKeyID)
	}
	now := time.Now()
	req, err := s.presign.PresignPutObject(ctx, in, awss3.WithPresignExpires(ttl))
	if err != nil {
		return nil, fmt.Errorf("blob/s3: presign put %q: %w", key, err)
	}
	headers := signedHeaders(req.SignedHeader)
	headers["Content-Type"] = ct
	return &blob.PresignedRequest{Method: http.MethodPut, URL: req.URL, Headers: headers, ExpiresAt: now.Add(ttl).UTC()}, nil
}

// PresignGet returns a time-limited download URL.
func (s *Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (*blob.PresignedRequest, error) {
	if err := s.checkKey(key); err != nil {
		return nil, err
	}
	ttl = blobutil.ClampTTL(ttl)
	now := time.Now()
	req, err := s.presign.PresignGetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	}, awss3.WithPresignExpires(ttl))
	if err != nil {
		return nil, fmt.Errorf("blob/s3: presign get %q: %w", key, err)
	}
	out := &blob.PresignedRequest{Method: http.MethodGet, URL: req.URL, ExpiresAt: now.Add(ttl).UTC()}
	if h := signedHeaders(req.SignedHeader); len(h) > 0 {
		out.Headers = h
	}
	return out, nil
}

// signedHeaders flattens the signed headers a client must send, minus Host
// (which HTTP clients derive from the URL).
func signedHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if strings.EqualFold(k, "Host") || len(v) == 0 {
			continue
		}
		out[http.CanonicalHeaderKey(k)] = strings.Join(v, ",")
	}
	return out
}

// Ping checks that the bucket is reachable with the configured credentials
// (HeadBucket).
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.client.HeadBucket(ctx, &awss3.HeadBucketInput{Bucket: aws.String(s.bucket)}); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("blob/s3: ping bucket %q: %w", s.bucket, err)
	}
	return nil
}

// Describe returns the public storage description.
func (s *Store) Describe() blob.Description {
	return blob.Description{
		Type:     "s3",
		Bucket:   s.bucket,
		Prefix:   s.prefix,
		Region:   s.region,
		Endpoint: blobutil.PublicEndpoint(s.endpoint),
	}
}

// Close releases resources; the S3 client needs no explicit shutdown.
func (s *Store) Close() error { return nil }
