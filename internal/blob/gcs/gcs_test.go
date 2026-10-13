package gcs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsouza/fake-gcs-server/fakestorage"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/blobtest"
	"github.com/thasami/backyard/internal/config"
)

type fakeGCS struct {
	srv *fakestorage.Server
	n   atomic.Int64
}

func newFake(t *testing.T) *fakeGCS {
	t.Helper()
	srv, err := fakestorage.NewServerWithOptions(fakestorage.Options{
		Scheme: "http",
		Host:   "127.0.0.1",
		Writer: io.Discard,
	})
	if err != nil {
		t.Fatalf("fake gcs: %v", err)
	}
	t.Cleanup(srv.Stop)
	return &fakeGCS{srv: srv}
}

func (f *fakeGCS) endpoint() string { return f.srv.URL() + "/storage/v1/" }

func (f *fakeGCS) newStore(t *testing.T, prefix string) *Store {
	t.Helper()
	bucket := fmt.Sprintf("bucket-%d", f.n.Add(1))
	f.srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: bucket})
	s, err := New(t.Context(), config.GCSConfig{Bucket: bucket, Prefix: prefix, Endpoint: f.endpoint()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) {
	f := newFake(t)
	blobtest.Run(t, func(t *testing.T) blob.Store { return f.newStore(t, "") })
}

func TestConformanceWithPrefix(t *testing.T) {
	f := newFake(t)
	blobtest.Run(t, func(t *testing.T) blob.Store { return f.newStore(t, "env/prod/") })
}

func TestEmulatorCannotPresign(t *testing.T) {
	f := newFake(t)
	s := f.newStore(t, "")
	defer s.Close()
	if _, err := s.PresignPut(context.Background(), "a.txt", blob.PresignPutOptions{Size: -1}, time.Minute); !errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("PresignPut err = %v", err)
	}
	if _, err := s.PresignGet(context.Background(), "a.txt", time.Minute); !errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("PresignGet err = %v", err)
	}
	if d := s.Describe(); d.Type != "gcs" || d.Bucket == "" || d.Endpoint != f.endpoint() {
		t.Errorf("Describe = %+v", d)
	}
}

func TestPrefixIsolation(t *testing.T) {
	f := newFake(t)
	f.srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "shared"})
	ctx := context.Background()
	a, err := New(ctx, config.GCSConfig{Bucket: "shared", Prefix: "a", Endpoint: f.endpoint()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := New(ctx, config.GCSConfig{Bucket: "shared", Prefix: "b/", Endpoint: f.endpoint()})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.Put(ctx, "x.txt", strings.NewReader("a"), 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Stat(ctx, "x.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("prefix b sees a's object: %v", err)
	}
	if _, err := a.Client().Bucket("shared").Object("a/x.txt").Attrs(ctx); err != nil {
		t.Errorf("raw object a/x.txt: %v", err)
	}
	if err := b.DeletePrefix(ctx, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Stat(ctx, "x.txt"); err != nil {
		t.Errorf("DeletePrefix crossed prefixes: %v", err)
	}
}

func TestNewValidation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	badType := filepath.Join(dir, "bad.json")
	os.WriteFile(badType, []byte(`{"type":"something_else"}`), 0o600)
	notJSON := filepath.Join(dir, "notjson.json")
	os.WriteFile(notJSON, []byte(`nope`), 0o600)
	for _, tc := range []struct {
		name string
		cfg  config.GCSConfig
	}{
		{"no bucket", config.GCSConfig{}},
		{"bad endpoint", config.GCSConfig{Bucket: "b", Endpoint: "::nope"}},
		{"missing credentials file", config.GCSConfig{Bucket: "b", CredentialsFile: filepath.Join(dir, "missing.json")}},
		{"unsupported credentials type", config.GCSConfig{Bucket: "b", CredentialsFile: badType}},
		{"invalid credentials json", config.GCSConfig{Bucket: "b", CredentialsFile: notJSON}},
	} {
		if s, err := New(ctx, tc.cfg); err == nil {
			s.Close()
			t.Errorf("%s: New succeeded", tc.name)
		}
	}
}

func TestChunkSizeFor(t *testing.T) {
	for _, tc := range []struct {
		size int64
		want int
	}{
		{-1, unknownSizeChunk},
		{0, minChunk},
		{1, minChunk},
		{minChunk - 1, minChunk},
		{minChunk, 2 * minChunk},
		{5 << 20, 5<<20 + minChunk},
		{maxChunkSize, maxChunkSize},
		{1 << 30, maxChunkSize},
	} {
		if got := chunkSizeFor(tc.size); got != tc.want {
			t.Errorf("chunkSizeFor(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}
}

func TestGenETag(t *testing.T) {
	if got := genETag(1700000000123456, 10, time.Unix(1, 0)); got != "1700000000123456" {
		t.Errorf("genETag = %q", got)
	}
	if got := genETag(0, 10, time.Unix(1, 0)); got == "" || got == "0" {
		t.Errorf("genETag fallback = %q", got)
	}
}

func writeServiceAccount(t *testing.T) (path, email string) {
	t.Helper()
	path, email, _ = writeServiceAccountKey(t)
	return path, email
}

// writeServiceAccountKey writes a service-account key file and also returns
// the generated key (to verify signatures).
func writeServiceAccountKey(t *testing.T) (path, email string, key *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	email = "backyard@test-project.iam.gserviceaccount.com"
	sa := map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": "abc123",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   email,
		"client_id":      "1234567890",
		"token_uri":      "https://oauth2.googleapis.com/token",
	}
	b, _ := json.Marshal(sa)
	path = filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, email, key
}

// With a service-account key, URLs are V4-signed locally; the signed
// Content-Type is returned in Headers.
func TestPresignWithServiceAccountKey(t *testing.T) {
	path, email := writeServiceAccount(t)
	ctx := context.Background()
	s, err := New(ctx, config.GCSConfig{Bucket: "acme-backyard", Prefix: "pfx", CredentialsFile: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	req, err := s.PresignPut(ctx, "sites/foo/app.js", blob.PresignPutOptions{Size: 1234}, 2*time.Hour)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if req.Method != http.MethodPut || !maps.Equal(req.Headers, map[string]string{
		"Content-Type":                "text/javascript; charset=utf-8",
		"x-goog-content-length-range": "1234,1234",
	}) {
		t.Errorf("PresignPut = %+v", req)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "storage.googleapis.com" || u.Path != "/acme-backyard/pfx/sites/foo/app.js" {
		t.Errorf("URL = %s", req.URL)
	}
	q := u.Query()
	if exp := q.Get("X-Goog-Expires"); exp != "7200" && exp != "7199" {
		t.Errorf("X-Goog-Expires = %q", exp)
	}
	if q.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" ||
		!strings.HasPrefix(q.Get("X-Goog-Credential"), email+"/") ||
		q.Get("X-Goog-SignedHeaders") != "content-type;host;x-goog-content-length-range" {
		t.Errorf("signed URL query = %v", q)
	}
	// Without a size, any body size is accepted.
	unsized, err := s.PresignPut(ctx, "sites/foo/app.js", blob.PresignPutOptions{Size: -1}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	uu, _ := url.Parse(unsized.URL)
	if sh := uu.Query().Get("X-Goog-SignedHeaders"); sh != "content-type;host" || len(unsized.Headers) != 1 {
		t.Errorf("unsized PresignPut: signed %q, headers %v", sh, unsized.Headers)
	}
	if _, err := s.PresignPut(ctx, "a.txt", blob.PresignPutOptions{Size: -2}, time.Minute); err == nil || errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("PresignPut(negative size) err = %v", err)
	}
	if d := time.Until(req.ExpiresAt); d < 119*time.Minute || d > 121*time.Minute {
		t.Errorf("ExpiresAt in %v", d)
	}
	get, err := s.PresignGet(ctx, "sites/foo/app.js", 0)
	if err != nil {
		t.Fatalf("PresignGet: %v", err)
	}
	gu, _ := url.Parse(get.URL)
	if exp := gu.Query().Get("X-Goog-Expires"); get.Method != http.MethodGet || (exp != "900" && exp != "899") {
		t.Errorf("PresignGet = %+v", get)
	}
	if _, err := s.PresignPut(ctx, "../x", blob.PresignPutOptions{Size: -1}, time.Minute); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("PresignPut(invalid) err = %v", err)
	}
}

// Identities with object access but no storage.buckets.get still pass Ping.
func TestPingFallsBackToListing(t *testing.T) {
	var listed atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/b/locked/o"):
			listed.Store(true)
			io.WriteString(w, `{"kind":"storage#objects"}`)
		case strings.HasSuffix(r.URL.Path, "/b/locked"):
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":403,"message":"no storage.buckets.get"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":404,"message":"not found"}}`)
		}
	}))
	defer ts.Close()
	ctx := context.Background()
	s, err := New(ctx, config.GCSConfig{Bucket: "locked", Endpoint: ts.URL + "/storage/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if !listed.Load() {
		t.Error("Ping did not fall back to listing")
	}
	other, err := New(ctx, config.GCSConfig{Bucket: "missing", Endpoint: ts.URL + "/storage/v1/"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := other.Ping(ctx); err == nil {
		t.Error("Ping on missing bucket succeeded")
	}
}

// signedGCS fronts fake-gcs-server (which ignores authentication) with what
// GCS checks on V4 signed URLs: the GOOG4-RSA-SHA256 signature over the
// signed headers as received (verified with the service account's public
// key), the expiry, and x-goog-content-length-range against the body size.
func signedGCS(t *testing.T, f *fakeGCS, pub *rsa.PublicKey) string {
	t.Helper()
	target, err := url.Parse(f.srv.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := verifyGOOG4(r, pub, time.Now()); err != nil {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, "<Error><Code>SignatureDoesNotMatch</Code><Message>%s</Message></Error>", err)
			return
		}
		if rng := r.Header.Get("X-Goog-Content-Length-Range"); rng != "" {
			lo, hi, _ := strings.Cut(rng, ",")
			minLen, err1 := strconv.ParseInt(lo, 10, 64)
			maxLen, err2 := strconv.ParseInt(hi, 10, 64)
			if err1 != nil || err2 != nil || r.ContentLength < minLen || r.ContentLength > maxLen {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, "<Error><Code>EntityTooLarge</Code><Message>Content-Length %d outside %s</Message></Error>", r.ContentLength, rng)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// verifyGOOG4 recomputes a V4 signed URL's canonical request from the
// request as received, independently of the storage library.
func verifyGOOG4(r *http.Request, pub *rsa.PublicKey, now time.Time) error {
	q := r.URL.Query()
	if q.Get("X-Goog-Algorithm") != "GOOG4-RSA-SHA256" {
		return fmt.Errorf("unsupported algorithm %q", q.Get("X-Goog-Algorithm"))
	}
	cred := strings.Split(q.Get("X-Goog-Credential"), "/")
	if len(cred) != 5 || cred[4] != "goog4_request" {
		return fmt.Errorf("malformed credential %q", q.Get("X-Goog-Credential"))
	}
	date := q.Get("X-Goog-Date")
	signedAt, err := time.Parse("20060102T150405Z", date)
	if err != nil {
		return err
	}
	expires, err := strconv.Atoi(q.Get("X-Goog-Expires"))
	if err != nil || now.After(signedAt.Add(time.Duration(expires)*time.Second)) {
		return errors.New("expired")
	}
	signedHeaders := q.Get("X-Goog-SignedHeaders")
	var headers []string
	for _, name := range strings.Split(signedHeaders, ";") {
		v := strings.Join(r.Header.Values(name), ",")
		if name == "host" {
			v = r.Host
		}
		headers = append(headers, name+":"+strings.Join(strings.Fields(v), " "))
	}
	sig, err := hex.DecodeString(q.Get("X-Goog-Signature"))
	if err != nil {
		return err
	}
	q.Del("X-Goog-Signature")
	path, _, _ := strings.Cut(r.RequestURI, "?")
	creq := strings.Join([]string{
		r.Method, path, strings.ReplaceAll(q.Encode(), "+", "%20"),
		strings.Join(headers, "\n") + "\n", signedHeaders, "UNSIGNED-PAYLOAD",
	}, "\n")
	creqSum := sha256.Sum256([]byte(creq))
	sts := "GOOG4-RSA-SHA256\n" + date + "\n" + strings.Join(cred[1:], "/") + "\n" + hex.EncodeToString(creqSum[:])
	stsSum := sha256.Sum256([]byte(sts))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, stsSum[:], sig); err != nil {
		return errors.New("the request signature does not match the signed headers")
	}
	return nil
}

// replay sends a presigned request to server (the fake, or signedGCS in
// front of it) instead of storage.googleapis.com, keeping the signed Host.
func replay(t *testing.T, server string, pr *blob.PresignedRequest, body string) (int, string) {
	t.Helper()
	return replayWith(t, server, pr, body, nil)
}

func replayWith(t *testing.T, server string, pr *blob.PresignedRequest, body string, mutate func(*http.Request)) (int, string) {
	t.Helper()
	u, err := url.Parse(pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "storage.googleapis.com" {
		t.Fatalf("signed URL %s does not target storage.googleapis.com over https", pr.URL)
	}
	fake, _ := url.Parse(server)
	u.Scheme, u.Host = fake.Scheme, fake.Host
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(pr.Method, u.String(), rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "storage.googleapis.com"
	for k, v := range pr.Headers {
		req.Header.Set(k, v)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Signed URLs address the prefixed object (with escaping) and carry the
// signed Content-Type and size range; replayed against fake-gcs-server
// behind a GOOG4 verifier they store and return the object, and bodies of
// another size or altered signed headers are rejected.
func TestPresignRoundTripWithServiceAccount(t *testing.T) {
	f := newFake(t)
	f.srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "acme"})
	path, _, key := writeServiceAccountKey(t)
	gcs := signedGCS(t, f, &key.PublicKey)
	ctx := context.Background()
	s, err := New(ctx, config.GCSConfig{Bucket: "acme", Prefix: "pfx", CredentialsFile: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const name = "presign/up load ü+%.txt"
	put, err := s.PresignPut(ctx, name, blob.PresignPutOptions{ContentType: "text/plain; charset=utf-8", Size: 12}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		what   string
		body   string
		mutate func(*http.Request)
		status int
	}{
		{"larger body", "signed bytes!", nil, http.StatusBadRequest},
		{"smaller body", "signed", nil, http.StatusBadRequest},
		{"widened range", "signed bytes and more", func(r *http.Request) { r.Header.Set("X-Goog-Content-Length-Range", "0,100") }, http.StatusForbidden},
		{"range dropped", "signed bytes and more", func(r *http.Request) { r.Header.Del("X-Goog-Content-Length-Range") }, http.StatusForbidden},
		{"other content type", "signed bytes", func(r *http.Request) { r.Header.Set("Content-Type", "text/html") }, http.StatusForbidden},
	} {
		if code, body := replayWith(t, gcs, put, tc.body, tc.mutate); code != tc.status {
			t.Errorf("%s: status %d (%s), want %d", tc.what, code, body, tc.status)
		}
	}
	if _, err := f.srv.GetObject("acme", "pfx/"+name); err == nil {
		t.Fatal("a rejected upload stored the object")
	}
	if code, body := replay(t, gcs, put, "signed bytes"); code != http.StatusOK {
		t.Fatalf("signed PUT status %d: %s", code, body)
	}
	empty, err := s.PresignPut(ctx, "presign/empty", blob.PresignPutOptions{Size: 0}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Headers["x-goog-content-length-range"] != "0,0" {
		t.Errorf("empty upload headers = %v", empty.Headers)
	}
	if code, _ := replay(t, gcs, empty, "x"); code != http.StatusBadRequest {
		t.Errorf("body for a 0-byte upload: status %d", code)
	}
	obj, err := f.srv.GetObject("acme", "pfx/"+name)
	if err != nil {
		t.Fatalf("object not stored under the prefixed name: %v", err)
	}
	if string(obj.Content) != "signed bytes" || obj.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("stored object = %q (%s)", obj.Content, obj.ContentType)
	}
	get, err := s.PresignGet(ctx, name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(get.Headers) != 0 {
		t.Errorf("PresignGet headers = %v, want none", get.Headers)
	}
	if code, body := replay(t, gcs, get, ""); code != http.StatusOK || body != "signed bytes" {
		t.Errorf("signed GET = %d %q", code, body)
	}
}

// Keys that only become too long once the prefix is added are invalid keys,
// not remote errors.
func TestPrefixedKeyLength(t *testing.T) {
	f := newFake(t)
	s := f.newStore(t, "some/prefix")
	defer s.Close()
	ctx := context.Background()
	long := strings.Repeat("k", 1020)
	if _, err := s.Stat(ctx, long); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("Stat(long) err = %v, want ErrInvalidKey", err)
	}
	if _, err := s.Put(ctx, long, strings.NewReader("x"), 1, ""); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("Put(long) err = %v, want ErrInvalidKey", err)
	}
}
