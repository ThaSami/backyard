package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
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

// presignedS3 makes gofakes3 (which ignores authentication) check presigned
// requests the way S3 does: the SigV4 query signature over the signed
// headers as received, so a body whose Content-Length differs from a signed
// one fails with 403 SignatureDoesNotMatch; the expiry; and a signed
// x-amz-checksum-sha256 against the body (400 BadDigest). gofakes3 itself
// verifies Content-MD5. Like S3, it keeps the SHA-256 checksum of an object
// uploaded with one and returns it (x-amz-checksum-sha256, type FULL_OBJECT)
// from a HeadObject with x-amz-checksum-mode: ENABLED, for as long as that
// object version (its ETag) is current.
func presignedS3(secret string) func(http.Handler) http.Handler {
	type checksum struct{ sha, etag string }
	var (
		mu        sync.Mutex
		checksums = map[string]checksum{} // by request path
	)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Has("X-Amz-Signature") {
				if err := verifyPresignedSigV4(r, secret, time.Now()); err != nil {
					writeS3Error(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error())
					return
				}
			}
			want := r.Header.Get("X-Amz-Checksum-Sha256")
			if want != "" {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					writeS3Error(w, http.StatusBadRequest, "IncompleteBody", err.Error())
					return
				}
				sum := sha256.Sum256(body)
				if got := base64.StdEncoding.EncodeToString(sum[:]); got != want {
					writeS3Error(w, http.StatusBadRequest, "BadDigest", "The SHA256 you specified did not match the calculated checksum.")
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			switch {
			case r.Method == http.MethodPut && want != "":
				rec := httptest.NewRecorder()
				next.ServeHTTP(rec, r)
				if rec.Code == http.StatusOK {
					mu.Lock()
					checksums[r.URL.Path] = checksum{sha: want, etag: rec.Header().Get("ETag")}
					mu.Unlock()
				}
				copyRecorded(w, rec)
			case r.Method == http.MethodHead || r.Method == http.MethodGet:
				rec := httptest.NewRecorder()
				next.ServeHTTP(rec, r)
				// gofakes3 echoes every x-amz-* header of the request that
				// created an object as metadata, and keeps them across
				// overwrites: drop the checksums and report only the one S3
				// would keep for the current object version.
				for k := range rec.Header() {
					if strings.HasPrefix(strings.ToLower(k), "x-amz-checksum-") {
						rec.Header().Del(k)
					}
				}
				mu.Lock()
				c, ok := checksums[r.URL.Path]
				mu.Unlock()
				if ok && rec.Code == http.StatusOK && c.etag != "" && rec.Header().Get("ETag") == c.etag &&
					strings.EqualFold(r.Header.Get("X-Amz-Checksum-Mode"), "ENABLED") {
					rec.Header().Set("X-Amz-Checksum-Sha256", c.sha)
					rec.Header().Set("X-Amz-Checksum-Type", "FULL_OBJECT")
				}
				copyRecorded(w, rec)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// copyRecorded replays a recorded response.
func copyRecorded(w http.ResponseWriter, rec *httptest.ResponseRecorder) {
	maps.Copy(w.Header(), rec.Header())
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func writeS3Error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, msg)
}

// verifyPresignedSigV4 recomputes a presigned (query-string) SigV4 signature
// from the request as received, independently of the SDK's signer.
func verifyPresignedSigV4(r *http.Request, secret string, now time.Time) error {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != "AWS4-HMAC-SHA256" {
		return fmt.Errorf("unsupported algorithm %q", q.Get("X-Amz-Algorithm"))
	}
	cred := strings.Split(q.Get("X-Amz-Credential"), "/")
	if len(cred) != 5 || cred[4] != "aws4_request" {
		return fmt.Errorf("malformed credential %q", q.Get("X-Amz-Credential"))
	}
	amzDate := q.Get("X-Amz-Date")
	signedAt, err := time.Parse("20060102T150405Z", amzDate)
	if err != nil {
		return fmt.Errorf("malformed X-Amz-Date: %w", err)
	}
	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires <= 0 || expires > 7*24*3600 {
		return fmt.Errorf("malformed X-Amz-Expires %q", q.Get("X-Amz-Expires"))
	}
	if now.After(signedAt.Add(time.Duration(expires) * time.Second)) {
		return errors.New("request has expired")
	}
	signedHeaders := q.Get("X-Amz-SignedHeaders")
	var canonHeaders strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		var v string
		switch name {
		case "host":
			v = r.Host
		case "content-length":
			if r.ContentLength < 0 {
				return errors.New("signed content-length missing (chunked body)")
			}
			v = strconv.FormatInt(r.ContentLength, 10)
		default:
			v = strings.Join(r.Header.Values(name), ",")
		}
		canonHeaders.WriteString(name + ":" + strings.Join(strings.Fields(v), " ") + "\n")
	}
	sig := q.Get("X-Amz-Signature")
	q.Del("X-Amz-Signature")
	path, _, _ := strings.Cut(r.RequestURI, "?")
	creq := strings.Join([]string{
		r.Method, path, strings.ReplaceAll(q.Encode(), "+", "%20"),
		canonHeaders.String(), signedHeaders, "UNSIGNED-PAYLOAD",
	}, "\n")
	creqSum := sha256.Sum256([]byte(creq))
	scope := strings.Join(cred[1:], "/")
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(creqSum[:])
	mac := func(key []byte, data string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(data))
		return h.Sum(nil)
	}
	key := []byte("AWS4" + secret)
	for _, part := range cred[1:] {
		key = mac(key, part)
	}
	if want := hex.EncodeToString(mac(key, sts)); !hmac.Equal([]byte(want), []byte(sig)) {
		return errors.New("the request signature does not match the signed headers")
	}
	return nil
}

// chain applies wrappers outermost first.
func chain(wraps ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		for i := len(wraps) - 1; i >= 0; i-- {
			h = wraps[i](h)
		}
		return h
	}
}

// S3-compatible endpoints (MinIO, R2, Ceph...) get only headers such stores
// understand; presigned uploads still pin the size (signed Content-Length or
// Content-MD5), but not the SHA-256 (no flexible checksums).
func TestConformanceStrictCompatible(t *testing.T) {
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "")
	f := newFakeWith(t, chain(strictCompat(t), presignedS3(testSecret)))
	blobtest.Run(t, func(t *testing.T) blob.Store { return newTestStore(t, f, "r2") }, blobtest.PresignEnforcesSize())
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
	if _, err := s.PresignPut(ctx, long, blob.PresignPutOptions{Size: -1}, time.Minute); !errors.Is(err, blob.ErrInvalidKey) {
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
		SecretAccessKey: testSecret,
	}
}

const testSecret = "test-secret"

func newTestStore(t *testing.T, f *fakeS3, prefix string, opts ...Option) *Store {
	t.Helper()
	s, err := New(t.Context(), f.cfg(f.bucket(t), prefix), append([]Option{WithPartSize(MinPartSize)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) {
	f := newFakeWith(t, presignedS3(testSecret))
	blobtest.Run(t, func(t *testing.T) blob.Store { return newTestStore(t, f, "") }, blobtest.PresignEnforcesSize())
}

func TestConformanceWithPrefix(t *testing.T) {
	f := newFakeWith(t, presignedS3(testSecret))
	blobtest.Run(t, func(t *testing.T) blob.Store { return newTestStore(t, f, "/tenant/a") }, blobtest.PresignEnforcesSize())
}

// The default AWS checksum mode (CRC32 on uploads and multipart parts) must
// also work end to end; presigned uploads then also pin the SHA-256.
func TestConformanceWithFlexibleChecksums(t *testing.T) {
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_supported")
	t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_supported")
	f := newFakeWith(t, presignedS3(testSecret))
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s := newTestStore(t, f, "")
		if !s.checksums {
			t.Fatal("expected flexible checksums to be enabled")
		}
		return s
	}, blobtest.PresignEnforcesSize(), blobtest.PresignEnforcesSHA256(), blobtest.ReportsPresignedDigests())
}

// StatSHA256 reads the SHA-256 that S3 verified for a presigned upload (a
// HeadObject in checksum mode) and never sends the checksum-mode header to
// S3-compatible endpoints without flexible checksums.
func TestStatSHA256(t *testing.T) {
	ctx := context.Background()
	t.Run("flexible checksums", func(t *testing.T) {
		t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_supported")
		t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "when_supported")
		s := newTestStore(t, newFakeWith(t, presignedS3(testSecret)), "pre/fix")
		data := []byte("<title>verified</title>")
		sum := sha256.Sum256(data)
		req, err := s.PresignPut(ctx, "site/index.html", blob.PresignPutOptions{ContentType: "text/html", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		hr, err := http.NewRequest(req.Method, req.URL, bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range req.Headers {
			hr.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(hr)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("presigned PUT status %d", resp.StatusCode)
		}
		info, got, err := s.StatSHA256(ctx, "site/index.html")
		if err != nil || got != hex.EncodeToString(sum[:]) {
			t.Fatalf("StatSHA256 = %q, %v; want %x", got, err, sum)
		}
		if st, _ := s.Stat(ctx, "site/index.html"); info.ETag != st.ETag || info.Size != int64(len(data)) || info.Key != "site/index.html" {
			t.Errorf("StatSHA256 info = %+v, Stat = %+v", info, st)
		}
		if _, _, err := s.StatSHA256(ctx, "site/missing.html"); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("StatSHA256(missing) err = %v", err)
		}
	})
	t.Run("custom endpoint without flexible checksums", func(t *testing.T) {
		t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "")
		t.Setenv("AWS_RESPONSE_CHECKSUM_VALIDATION", "")
		var requests atomic.Int64
		f := newFakeWith(t, chain(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				next.ServeHTTP(w, r)
			})
		}, strictCompat(t)))
		s := newTestStore(t, f, "")
		if _, err := s.Put(ctx, "a.txt", strings.NewReader("a"), 1, ""); err != nil {
			t.Fatal(err)
		}
		before := requests.Load()
		if _, _, err := s.StatSHA256(ctx, "a.txt"); !errors.Is(err, blob.ErrNotSupported) {
			t.Errorf("StatSHA256 err = %v, want ErrNotSupported", err)
		}
		if n := requests.Load() - before; n != 0 {
			t.Errorf("StatSHA256 sent %d requests", n)
		}
	})
}

func TestFullObjectSHA256(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	for _, tc := range []struct {
		out  awss3.HeadObjectOutput
		want string
	}{
		{awss3.HeadObjectOutput{ChecksumSHA256: aws.String(b64), ChecksumType: types.ChecksumTypeFullObject}, hex.EncodeToString(sum[:])},
		{awss3.HeadObjectOutput{ChecksumSHA256: aws.String(b64)}, hex.EncodeToString(sum[:])},
		{awss3.HeadObjectOutput{ChecksumSHA256: aws.String(b64), ChecksumType: types.ChecksumTypeComposite}, ""},
		{awss3.HeadObjectOutput{ChecksumSHA256: aws.String(b64 + "-3")}, ""},
		{awss3.HeadObjectOutput{ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString([]byte("short")))}, ""},
		{awss3.HeadObjectOutput{ChecksumCRC32: aws.String("AAAAAA==")}, ""},
	} {
		if got := fullObjectSHA256(&tc.out); got != tc.want {
			t.Errorf("fullObjectSHA256(%+v) = %q, want %q", tc.out, got, tc.want)
		}
	}
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
	if !s.checksums || s.client.Options().UsePathStyle {
		t.Errorf("AWS defaults not kept: checksums=%v pathStyle=%v", s.checksums, s.client.Options().UsePathStyle)
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
	if !o.UsePathStyle || o.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired || r2.checksums {
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
	req, err := s.PresignPut(ctx, "sites/foo/index.html", blob.PresignPutOptions{Size: -1}, time.Hour)
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

// Presigned uploads sign the declared size (and on AWS the SHA-256), and
// return exactly the headers the client must send.
func TestPresignBindsSizeAndChecksum(t *testing.T) {
	ctx := context.Background()
	const sha = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" // "hello"
	const shaB64 = "LPJNul+wow4m6DsqxbninhsWHlwfp0JecwQzYpOLmCQ="
	aws, err := New(ctx, config.S3Config{Bucket: "acme", Region: "us-east-1", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "")
	compat, err := New(ctx, config.S3Config{Bucket: "acme", Endpoint: "https://acct.r2.cloudflarestorage.com", AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		s           *Store
		opts        blob.PresignPutOptions
		wantSigned  []string
		wantHeaders map[string]string
		notSigned   []string
	}{
		{
			name:        "aws with sha256",
			s:           aws,
			opts:        blob.PresignPutOptions{ContentType: "text/plain", Size: 5, SHA256: sha},
			wantSigned:  []string{"content-length", "content-type", "host", "x-amz-checksum-sha256"},
			wantHeaders: map[string]string{"Content-Length": "5", "Content-Type": "text/plain", "X-Amz-Checksum-Sha256": shaB64},
		},
		{
			name:        "aws without sha256",
			s:           aws,
			opts:        blob.PresignPutOptions{ContentType: "text/plain", Size: 5},
			wantSigned:  []string{"content-length", "content-type", "host"},
			wantHeaders: map[string]string{"Content-Length": "5", "Content-Type": "text/plain"},
			notSigned:   []string{"x-amz-checksum-sha256"},
		},
		{
			name:        "compatible endpoint: size only",
			s:           compat,
			opts:        blob.PresignPutOptions{ContentType: "text/plain", Size: 5, SHA256: strings.ToUpper(sha)},
			wantSigned:  []string{"content-length", "content-type", "host"},
			wantHeaders: map[string]string{"Content-Length": "5", "Content-Type": "text/plain"},
			notSigned:   []string{"x-amz-checksum-sha256"},
		},
		{
			name:        "empty body",
			s:           compat,
			opts:        blob.PresignPutOptions{ContentType: "text/plain", Size: 0},
			wantSigned:  []string{"content-md5", "content-type", "host"},
			wantHeaders: map[string]string{"Content-Md5": emptyMD5, "Content-Type": "text/plain"},
			notSigned:   []string{"content-length"},
		},
		{
			name:        "unknown size",
			s:           aws,
			opts:        blob.PresignPutOptions{ContentType: "text/plain", Size: -1},
			wantSigned:  []string{"content-type", "host"},
			wantHeaders: map[string]string{"Content-Type": "text/plain"},
			notSigned:   []string{"content-length", "content-md5"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := tc.s.PresignPut(ctx, "sites/foo/a.txt", tc.opts, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(req.URL)
			if err != nil {
				t.Fatal(err)
			}
			signed := strings.Split(u.Query().Get("X-Amz-SignedHeaders"), ";")
			for _, h := range tc.wantSigned {
				if !slices.Contains(signed, h) {
					t.Errorf("X-Amz-SignedHeaders = %q, missing %s", signed, h)
				}
			}
			for _, h := range tc.notSigned {
				if slices.Contains(signed, h) {
					t.Errorf("X-Amz-SignedHeaders = %q, must not contain %s", signed, h)
				}
			}
			if !maps.Equal(req.Headers, tc.wantHeaders) {
				t.Errorf("Headers = %v, want %v", req.Headers, tc.wantHeaders)
			}
		})
	}
	for name, opts := range map[string]blob.PresignPutOptions{
		"negative size": {Size: -5},
		"bad sha256":    {Size: 5, SHA256: "xyz"},
	} {
		if _, err := aws.PresignPut(ctx, "a.txt", opts, time.Minute); err == nil {
			t.Errorf("%s: PresignPut succeeded", name)
		}
	}
}

// The presigned-URL verifier used by the conformance tests rejects what S3
// rejects: a different body size, an altered signed header, a dropped
// checksum header and an expired URL.
func TestPresignedUploadRejections(t *testing.T) {
	t.Setenv("AWS_REQUEST_CHECKSUM_CALCULATION", "when_supported")
	f := newFakeWith(t, presignedS3(testSecret))
	s := newTestStore(t, f, "p")
	ctx := context.Background()
	data := []byte("twelve bytes")
	sum := sha256.Sum256(data)
	req, err := s.PresignPut(ctx, "k.txt", blob.PresignPutOptions{Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	send := func(body []byte, mutate func(*http.Request)) (int, string) {
		t.Helper()
		r, err := http.NewRequestWithContext(ctx, req.Method, req.URL, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range req.Headers {
			r.Header.Set(k, v)
		}
		if mutate != nil {
			mutate(r)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		mutate func(*http.Request)
		status int
		code   string
	}{
		{"larger body", []byte("thirteen byte"), nil, 403, "SignatureDoesNotMatch"},
		{"smaller body", []byte("eleven byte"), nil, 403, "SignatureDoesNotMatch"},
		{"chunked body", data, func(r *http.Request) { r.ContentLength = -1; r.Body = io.NopCloser(bytes.NewReader(data)) }, 403, "SignatureDoesNotMatch"},
		{"other content type", data, func(r *http.Request) { r.Header.Set("Content-Type", "text/html") }, 403, "SignatureDoesNotMatch"},
		{"checksum header dropped", data, func(r *http.Request) { r.Header.Del("X-Amz-Checksum-Sha256") }, 403, "SignatureDoesNotMatch"},
		{"same size, other bytes", []byte("TWELVE BYTES"), nil, 400, "BadDigest"},
	} {
		code, body := send(tc.body, tc.mutate)
		if code != tc.status || !strings.Contains(body, tc.code) {
			t.Errorf("%s: status %d %s, want %d %s", tc.name, code, body, tc.status, tc.code)
		}
	}
	if _, err := s.Stat(ctx, "k.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("object stored by a rejected upload: %v", err)
	}
	if code, body := send(data, nil); code != http.StatusOK {
		t.Fatalf("exact upload: %d %s", code, body)
	}
	if err := verifyPresignedSigV4(httptest.NewRequest(http.MethodPut, req.URL, nil), testSecret, time.Now().Add(2*time.Minute)); err == nil {
		t.Error("expired URL verified")
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
