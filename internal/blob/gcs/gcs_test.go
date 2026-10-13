package gcs

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	if _, err := s.PresignPut(context.Background(), "a.txt", "", time.Minute); !errors.Is(err, blob.ErrNotSupported) {
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
	return path, email
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
	req, err := s.PresignPut(ctx, "sites/foo/app.js", "", 2*time.Hour)
	if err != nil {
		t.Fatalf("PresignPut: %v", err)
	}
	if req.Method != http.MethodPut || req.Headers["Content-Type"] != "text/javascript; charset=utf-8" {
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
		!strings.Contains(q.Get("X-Goog-SignedHeaders"), "content-type") {
		t.Errorf("signed URL query = %v", q)
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
	if _, err := s.PresignPut(ctx, "../x", "", time.Minute); !errors.Is(err, blob.ErrInvalidKey) {
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

// replay sends a presigned request to the fake server instead of
// storage.googleapis.com, keeping the signed Host.
func replay(t *testing.T, f *fakeGCS, pr *blob.PresignedRequest, body string) (int, string) {
	t.Helper()
	u, err := url.Parse(pr.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "storage.googleapis.com" {
		t.Fatalf("signed URL %s does not target storage.googleapis.com over https", pr.URL)
	}
	fake, _ := url.Parse(f.srv.URL())
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// Signed URLs address the prefixed object (with escaping) and carry the
// signed Content-Type; replayed against fake-gcs-server (which does not
// verify signatures) they store and return the object.
func TestPresignRoundTripWithServiceAccount(t *testing.T) {
	f := newFake(t)
	f.srv.CreateBucketWithOpts(fakestorage.CreateBucketOpts{Name: "acme"})
	path, _ := writeServiceAccount(t)
	ctx := context.Background()
	s, err := New(ctx, config.GCSConfig{Bucket: "acme", Prefix: "pfx", CredentialsFile: path})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const key = "presign/up load ü+%.txt"
	put, err := s.PresignPut(ctx, key, "text/plain; charset=utf-8", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := replay(t, f, put, "signed bytes"); code != http.StatusOK {
		t.Fatalf("signed PUT status %d: %s", code, body)
	}
	obj, err := f.srv.GetObject("acme", "pfx/"+key)
	if err != nil {
		t.Fatalf("object not stored under the prefixed name: %v", err)
	}
	if string(obj.Content) != "signed bytes" || obj.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("stored object = %q (%s)", obj.Content, obj.ContentType)
	}
	get, err := s.PresignGet(ctx, key, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(get.Headers) != 0 {
		t.Errorf("PresignGet headers = %v, want none", get.Headers)
	}
	if code, body := replay(t, f, get, ""); code != http.StatusOK || body != "signed bytes" {
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
