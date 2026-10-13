package s3

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/blobtest"
	"github.com/thasami/backyard/internal/config"
)

type fakeS3 struct {
	url     string
	backend *s3mem.Backend
	n       atomic.Int64
}

func newFake(t *testing.T) *fakeS3 { return newFakeWith(t, nil) }

// newFakeWith starts gofakes3 behind wrap (if non-nil).
func newFakeWith(t *testing.T, wrap func(http.Handler) http.Handler) *fakeS3 {
	t.Helper()
	backend := s3mem.New()
	var h http.Handler = gofakes3.New(backend, gofakes3.WithLogger(gofakes3.DiscardLog())).Server()
	if wrap != nil {
		h = wrap(h)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &fakeS3{url: ts.URL, backend: backend}
}

// strictCompat enforces what conservative S3-compatible stores (older MinIO
// and Ceph RGW, R2, B2) accept: no aws-chunked payloads or trailers, no
// flexible-checksum headers or checksum mode (except on DeleteObjects,
// which must carry a correct Content-MD5).
func strictCompat(t *testing.T) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reject := func(msg string) {
				t.Errorf("%s %s: %s", r.Method, r.URL.Path, msg)
				http.Error(w, msg, http.StatusNotImplemented)
			}
			deleteObjects := r.Method == http.MethodPost && r.URL.Query().Has("delete")
			if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") ||
				strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") ||
				r.Header.Get("X-Amz-Trailer") != "" || r.Header.Get("X-Amz-Decoded-Content-Length") != "" {
				reject("aws-chunked payload")
				return
			}
			for k := range r.Header {
				lk := strings.ToLower(k)
				if lk == "x-amz-checksum-mode" ||
					(!deleteObjects && (strings.HasPrefix(lk, "x-amz-checksum-") || lk == "x-amz-sdk-checksum-algorithm")) {
					reject("unsupported header " + k)
					return
				}
			}
			if deleteObjects {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					reject(err.Error())
					return
				}
				sum := md5.Sum(body)
				if r.Header.Get("Content-Md5") != base64.StdEncoding.EncodeToString(sum[:]) {
					reject("DeleteObjects without a valid Content-MD5")
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// S3-compatible endpoints (MinIO, R2, Ceph...) get only headers such stores
// understand.
func TestConformanceStrictCompatible(t *testing.T) {
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "")
	f := newFakeWith(t, strictCompat(t))
	blobtest.Run(t, func(t *testing.T) blob.Store { return newTestStore(t, f, "r2") })
}

// Stores (or caches) that ignore Range still yield the requested bytes.
func TestRangeIgnoredByServer(t *testing.T) {
	f := newFakeWith(t, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Del("Range")
			next.ServeHTTP(w, r)
		})
	})
	s := newTestStore(t, f, "")
	ctx := context.Background()
	if _, err := s.Put(ctx, "d.txt", strings.NewReader("0123456789"), 10, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		off, n int64
		want   string
	}{{2, 3, "234"}, {5, -1, "56789"}, {0, 4, "0123"}, {9, 10, "9"}, {10, -1, ""}, {12, 2, ""}} {
		rc, info, err := s.GetRange(ctx, "d.txt", tc.off, tc.n)
		if err != nil {
			t.Errorf("GetRange(%d, %d): %v", tc.off, tc.n, err)
			continue
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || string(b) != tc.want || info.Size != 10 {
			t.Errorf("GetRange(%d, %d) = %q (size %d, err %v), want %q", tc.off, tc.n, b, info.Size, err, tc.want)
		}
	}
}

// Keys that only become too long once the prefix is added are invalid keys,
// not remote errors.
func TestPrefixedKeyLength(t *testing.T) {
	f := newFake(t)
	s := newTestStore(t, f, "some/prefix")
	ctx := context.Background()
	long := strings.Repeat("k", 1020)
	if _, err := s.Stat(ctx, long); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("Stat(long) err = %v, want ErrInvalidKey", err)
	}
	if _, err := s.PresignPut(ctx, long, "", time.Minute); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("PresignPut(long) err = %v, want ErrInvalidKey", err)
	}
	if _, err := s.Put(ctx, strings.Repeat("k", 1012), strings.NewReader("x"), 1, ""); err != nil {
		t.Errorf("Put(key at the limit): %v", err)
	}
}

func (f *fakeS3) bucket(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("bucket-%d", f.n.Add(1))
	if err := f.backend.CreateBucket(name); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return name
}

func (f *fakeS3) cfg(bucket, prefix string) config.S3Config {
	return config.S3Config{
		Bucket:          bucket,
		Region:          "us-east-1",
		Prefix:          prefix,
		Endpoint:        f.url,
		AccessKeyID:     "test-key",
		SecretAccessKey: "test-secret",
	}
}

func newTestStore(t *testing.T, f *fakeS3, prefix string, opts ...Option) *Store {
	t.Helper()
	s, err := New(t.Context(), f.cfg(f.bucket(t), prefix), append([]Option{WithPartSize(MinPartSize)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) {
	f := newFake(t)
	blobtest.Run(t, func(t *testing.T) blob.Store { return newTestStore(t, f, "") })
}

func TestConformanceWithPrefix(t *testing.T) {
	f := newFake(t)
	blobtest.Run(t, func(t *testing.T) blob.Store { return newTestStore(t, f, "/tenant/a") })
}

// The default AWS checksum mode (CRC32 on uploads and multipart parts) must
// also work end to end.
func TestConformanceWithFlexibleChecksums(t *testing.T) {
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_supported")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_supported")
	f := newFake(t)
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s := newTestStore(t, f, "")
		if !s.partChecksums {
			t.Fatal("expected part checksums to be enabled")
		}
		return s
	})
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  config.S3Config
	}{
		{"no bucket", config.S3Config{}},
		{"half credentials", config.S3Config{Bucket: "b", AccessKeyID: "x"}},
		{"bad endpoint", config.S3Config{Bucket: "b", Endpoint: "not a url"}},
	} {
		if _, err := New(ctx, tc.cfg); err == nil {
			t.Errorf("%s: New succeeded", tc.name)
		}
	}
	// Endpoint errors never echo credentials.
	_, err := New(ctx, config.S3Config{Bucket: "b", Endpoint: "//user:hunter2@?sig=topsecret"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("bad endpoint error = %v", err)
	}
}

func TestDescribeAndDefaults(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.S3Config{
		Bucket: "acme", Prefix: "backyard", Region: "eu-west-1",
		AccessKeyID: "a", SecretAccessKey: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := blob.Description{Type: "s3", Bucket: "acme", Prefix: "backyard/", Region: "eu-west-1"}
	if d := s.Describe(); d != want {
		t.Errorf("Describe = %+v, want %+v", d, want)
	}
	if !s.partChecksums || s.client.Options().UsePathStyle {
		t.Errorf("AWS defaults not kept: checksums=%v pathStyle=%v", s.partChecksums, s.client.Options().UsePathStyle)
	}
	r2, err := New(ctx, config.S3Config{
		Bucket: "site", Endpoint: "https://0123abcd.r2.cloudflarestorage.com",
		AccessKeyID: "a", SecretAccessKey: "b",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r2.Describe().Endpoint != "https://0123abcd.r2.cloudflarestorage.com" {
		t.Errorf("endpoint not described: %+v", r2.Describe())
	}
	o := r2.client.Options()
	if !o.UsePathStyle || o.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired || r2.partChecksums {
		t.Errorf("custom endpoint options: pathStyle=%v checksum=%v", o.UsePathStyle, o.RequestChecksumCalculation)
	}
	// R2 always signs for "auto" unless a region is configured, even when
	// the host has an ambient AWS region.
	t.Setenv("AWS_REGION", "eu-west-1")
	for _, ep := range []string{"https://0123abcd.r2.cloudflarestorage.com", "https://0123abcd.eu.r2.cloudflarestorage.com/"} {
		r, err := New(ctx, config.S3Config{Bucket: "site", Endpoint: ep, AccessKeyID: "a", SecretAccessKey: "b"})
		if err != nil {
			t.Fatal(err)
		}
		if got := r.Describe().Region; got != "auto" {
			t.Errorf("R2 %s region = %q, want auto", ep, got)
		}
	}
	if r, _ := New(ctx, config.S3Config{Bucket: "site", Region: "us-east-1", Endpoint: "https://a.r2.cloudflarestorage.com", AccessKeyID: "a", SecretAccessKey: "b"}); r.Describe().Region != "us-east-1" {
		t.Errorf("explicit region not kept: %q", r.Describe().Region)
	}
	if m, _ := New(ctx, config.S3Config{Bucket: "b", Endpoint: "http://minio:9000", AccessKeyID: "a", SecretAccessKey: "b"}); m.Describe().Region != "eu-west-1" {
		t.Errorf("ambient region not used for non-R2 endpoint: %q", m.Describe().Region)
	}
	if isR2("https://evil.example/x.r2.cloudflarestorage.com") || isR2("::") {
		t.Error("isR2 matched a non-R2 endpoint")
	}
	if got := defaultRegion("https://x.r2.cloudflarestorage.com"); got != "auto" {
		t.Errorf("defaultRegion(r2) = %q", got)
	}
	if got := defaultRegion("http://minio:9000"); got != "us-east-1" {
		t.Errorf("defaultRegion(minio) = %q", got)
	}
}

func TestPrefixIsolation(t *testing.T) {
	f := newFake(t)
	bucket := f.bucket(t)
	ctx := context.Background()
	a, err := New(ctx, f.cfg(bucket, "tenant-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(ctx, f.cfg(bucket, "tenant-b/"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Put(ctx, "x.txt", strings.NewReader("a"), 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx, "x.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("tenant-b sees tenant-a object: %v", err)
	}
	out, err := a.Client().ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 1 || aws.ToString(out.Contents[0].Key) != "tenant-a/x.txt" {
		t.Errorf("raw keys = %+v", out.Contents)
	}
	if err := b.DeletePrefix(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Stat(ctx, "x.txt"); err != nil {
		t.Errorf("DeletePrefix crossed prefixes: %v", err)
	}
}

// Directory markers and keys the store would reject are not listed.
func TestListSkipsMarkersAndInvalidKeys(t *testing.T) {
	f := newFake(t)
	s := newTestStore(t, f, "")
	ctx := context.Background()
	for _, k := range []string{"dir/", "dir/ok.txt", "dir//double.txt"} {
		if _, err := s.Client().PutObject(ctx, &awss3.PutObjectInput{
			Bucket: aws.String(s.bucket), Key: aws.String(k), Body: strings.NewReader("x"),
		}); err != nil {
			t.Fatalf("raw put %q: %v", k, err)
		}
	}
	res, err := s.List(ctx, "dir/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Objects[0].Key != "dir/ok.txt" {
		t.Errorf("List = %+v", res.Objects)
	}
	// DeletePrefix removes everything under the prefix, markers included.
	if err := s.DeletePrefix(ctx, "dir/"); err != nil {
		t.Fatal(err)
	}
	out, err := s.Client().ListObjectsV2(ctx, &awss3.ListObjectsV2Input{Bucket: aws.String(s.bucket)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 0 {
		t.Errorf("objects left: %d", len(out.Contents))
	}
}

type countingTransport struct {
	mu    sync.Mutex
	calls []string
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	q := r.URL.Query()
	switch {
	case q.Has("uploads"):
		c.calls = append(c.calls, "create")
	case q.Has("partNumber"):
		c.calls = append(c.calls, "part")
	case q.Has("uploadId") && r.Method == http.MethodPost:
		c.calls = append(c.calls, "complete")
	case q.Has("uploadId") && r.Method == http.MethodDelete:
		c.calls = append(c.calls, "abort")
	case r.Method == http.MethodPut:
		c.calls = append(c.calls, "put")
	}
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

func (c *countingTransport) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = nil
	return out
}

func TestMultipartUpload(t *testing.T) {
	f := newFake(t)
	ct := &countingTransport{}
	s := newTestStore(t, f, "mp", WithHTTPClient(&http.Client{Transport: ct}))
	ctx := context.Background()

	small := bytes.Repeat([]byte("s"), 1000)
	if _, err := s.Put(ctx, "small.bin", bytes.NewReader(small), -1, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ct.take(), ","); got != "put" {
		t.Errorf("small upload calls = %s", got)
	}

	big := make([]byte, 2*MinPartSize+17)
	for i := range big {
		big[i] = byte(i % 251)
	}
	info, err := s.Put(ctx, "big.bin", struct{ io.Reader }{bytes.NewReader(big)}, -1, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ct.take(), ","); got != "create,part,part,part,complete" {
		t.Errorf("multipart calls = %s", got)
	}
	if info.Size != int64(len(big)) {
		t.Errorf("Size = %d", info.Size)
	}
	rc, ginfo, err := s.Get(ctx, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, big) || ginfo.ETag != info.ETag {
		t.Errorf("round trip mismatch: %d bytes, etag %q vs %q", len(got), ginfo.ETag, info.ETag)
	}

	// Exactly one part's worth stays a single PutObject.
	exact := make([]byte, MinPartSize)
	if _, err := s.Put(ctx, "exact.bin", bytes.NewReader(exact), int64(len(exact)), ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ct.take(), ","); got != "put" {
		t.Errorf("exact-size upload calls = %s", got)
	}

	// A body that fails mid-way aborts the multipart upload.
	failing := io.MultiReader(bytes.NewReader(big[:MinPartSize+10]), errReader{})
	if _, err := s.Put(ctx, "fail.bin", failing, -1, ""); err == nil {
		t.Fatal("failing upload succeeded")
	}
	if got := strings.Join(ct.take(), ","); got != "create,part,abort" {
		t.Errorf("failed upload calls = %s", got)
	}
	if _, err := s.Stat(ctx, "fail.bin"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat(fail.bin) = %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk on fire") }

func TestPresignDetails(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.S3Config{
		Bucket: "acme", Region: "us-east-1", Prefix: "p",
		AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret",
		SSEKMSKeyID: "arn:aws:kms:us-east-1:111122223333:key/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.PresignPut(ctx, "sites/foo/index.html", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "acme.s3.us-east-1.amazonaws.com" || u.Path != "/p/sites/foo/index.html" {
		t.Errorf("URL = %s", req.URL)
	}
	q := u.Query()
	if q.Get("X-Amz-Expires") != "3600" {
		t.Errorf("X-Amz-Expires = %q", q.Get("X-Amz-Expires"))
	}
	if sh := q.Get("X-Amz-SignedHeaders"); !strings.Contains(sh, "content-type") || !strings.Contains(sh, "x-amz-server-side-encryption") {
		t.Errorf("SignedHeaders = %q", sh)
	}
	for k := range q {
		if strings.Contains(strings.ToLower(k), "checksum") {
			t.Errorf("presigned URL carries checksum parameter %q", k)
		}
	}
	if req.Headers["Content-Type"] != "text/html; charset=utf-8" {
		t.Errorf("Content-Type header = %q", req.Headers["Content-Type"])
	}
	if req.Headers["X-Amz-Server-Side-Encryption"] != "aws:kms" ||
		req.Headers["X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"] != "arn:aws:kms:us-east-1:111122223333:key/abc" {
		t.Errorf("SSE headers = %v", req.Headers)
	}
	if _, ok := req.Headers["Host"]; ok {
		t.Error("Host header returned")
	}
	if d := time.Until(req.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("ExpiresAt in %v", d)
	}
	// TTLs are clamped to S3's 7 day maximum.
	long, err := s.PresignGet(ctx, "a.txt", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	lu, _ := url.Parse(long.URL)
	if lu.Query().Get("X-Amz-Expires") != "604800" {
		t.Errorf("clamped X-Amz-Expires = %q", lu.Query().Get("X-Amz-Expires"))
	}
}

func TestErrorClassification(t *testing.T) {
	resp404 := &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 404}}, Err: errors.New("x")}
	resp416 := &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 416}}, Err: errors.New("x")}
	for _, tc := range []struct {
		err          error
		notFound     bool
		invalidRange bool
	}{
		{&smithy.GenericAPIError{Code: "NoSuchKey"}, true, false},
		{&smithy.GenericAPIError{Code: "NotFound"}, true, false},
		{&smithy.GenericAPIError{Code: "NoSuchBucket"}, false, false},
		{&smithy.GenericAPIError{Code: "InvalidRange"}, false, true},
		{&smithy.GenericAPIError{Code: "AccessDenied"}, false, false},
		{resp404, true, false},
		{resp416, false, true},
		{errors.New("boom"), false, false},
	} {
		if got := isNotFound(tc.err); got != tc.notFound {
			t.Errorf("isNotFound(%v) = %v", tc.err, got)
		}
		if got := isInvalidRange(tc.err); got != tc.invalidRange {
			t.Errorf("isInvalidRange(%v) = %v", tc.err, got)
		}
	}
}

func TestPingMissingBucket(t *testing.T) {
	f := newFake(t)
	s, err := New(context.Background(), f.cfg("does-not-exist", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ping(context.Background()); err == nil {
		t.Error("Ping on missing bucket succeeded")
	}
	// A missing bucket is a configuration error, not a missing object
	// (only detectable on requests with an error body).
	if _, _, err := s.Get(context.Background(), "x"); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get on missing bucket err = %v", err)
	}
}
