package azure

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/blob/blobtest"
	"github.com/thasami/backyard/internal/config"
)

type azurite struct {
	cs    string
	conn  connString
	svc   *service.Client
	runID string
	n     atomic.Int64
}

// newAzurite connects to the Azurite instance named by
// BACKYARD_TEST_AZURITE, skipping the test when unset or unreachable.
func newAzurite(t *testing.T) *azurite {
	t.Helper()
	cs := os.Getenv("BACKYARD_TEST_AZURITE")
	if cs == "" {
		t.Skip("BACKYARD_TEST_AZURITE not set")
	}
	conn, err := parseConnectionString(cs)
	if err != nil {
		t.Fatalf("BACKYARD_TEST_AZURITE: %v", err)
	}
	svc, err := service.NewClientFromConnectionString(cs, nil)
	if err != nil {
		t.Fatalf("azurite client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := svc.GetProperties(ctx, nil); err != nil {
		t.Skipf("Azurite unreachable: %v", err)
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return &azurite{cs: cs, conn: conn, svc: svc, runID: hex.EncodeToString(b[:])}
}

func (a *azurite) container(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("byt-%s-%d", a.runID, a.n.Add(1))
	if _, err := a.svc.CreateContainer(context.Background(), name, nil); err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := a.svc.DeleteContainer(ctx, name, nil); err != nil {
			t.Logf("delete container %s: %v", name, err)
		}
	})
	return name
}

func TestConformanceConnectionString(t *testing.T) {
	a := newAzurite(t)
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s, err := New(t.Context(), config.AzureConfig{Container: a.container(t), ConnectionString: a.cs})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return s
	})
}

func TestConformanceAccountKeyWithPrefix(t *testing.T) {
	a := newAzurite(t)
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s, err := New(t.Context(), config.AzureConfig{
			Account:    a.conn.accountName,
			AccountKey: a.conn.accountKey,
			Endpoint:   a.conn.serviceURL,
			Container:  a.container(t),
			Prefix:     "/team/backyard/",
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return s
	})
}

func TestPrefixIsolationAndDescribe(t *testing.T) {
	a := newAzurite(t)
	ctx := context.Background()
	cont := a.container(t)
	x, err := New(ctx, config.AzureConfig{Container: cont, ConnectionString: a.cs, Prefix: "x"})
	if err != nil {
		t.Fatal(err)
	}
	y, err := New(ctx, config.AzureConfig{Container: cont, ConnectionString: a.cs, Prefix: "y/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.Put(ctx, "f.txt", strings.NewReader("x"), 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := y.Stat(ctx, "f.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("y sees x's object: %v", err)
	}
	if _, err := x.Client().NewBlobClient("x/f.txt").GetProperties(ctx, nil); err != nil {
		t.Errorf("raw blob x/f.txt: %v", err)
	}
	if err := y.DeletePrefix(ctx, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.Stat(ctx, "f.txt"); err != nil {
		t.Errorf("DeletePrefix crossed prefixes: %v", err)
	}
	d := x.Describe()
	if d.Type != "azure" || d.Container != cont || d.Account != a.conn.accountName || d.Prefix != "x/" || d.Endpoint == "" {
		t.Errorf("Describe = %+v", d)
	}
	if err := x.Ping(ctx); err != nil {
		t.Errorf("Ping: %v", err)
	}
	missing, err := New(ctx, config.AzureConfig{Container: "does-not-exist-" + a.runID, ConnectionString: a.cs})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Ping(ctx); err == nil {
		t.Error("Ping on missing container succeeded")
	}
	if _, _, err := missing.Get(ctx, "f.txt"); err == nil || errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get on missing container err = %v, want a non-NotFound error", err)
	}
}

// ADLS Gen2 directory placeholders are not objects.
func TestFolderPlaceholdersHidden(t *testing.T) {
	a := newAzurite(t)
	ctx := context.Background()
	s, err := New(ctx, config.AzureConfig{Container: a.container(t), ConnectionString: a.cs})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Client().NewBlockBlobClient("site/dir").UploadBuffer(ctx, nil, &blockblob.UploadBufferOptions{
		Metadata: map[string]*string{"hdi_isfolder": to.Ptr("true")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "site/dir/a.txt", strings.NewReader("a"), 1, ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.List(ctx, "site/", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Objects) != 1 || res.Objects[0].Key != "site/dir/a.txt" {
		t.Errorf("List = %+v", res.Objects)
	}
	if err := s.DeletePrefix(ctx, "site/"); err != nil {
		t.Fatal(err)
	}
	pager := s.Client().NewListBlobsFlatPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(page.Segment.BlobItems); n != 0 {
			t.Errorf("%d blobs left after DeletePrefix", n)
		}
	}
}

func TestParseConnectionString(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    connString
		wantErr bool
	}{
		{
			in:   "DefaultEndpointsProtocol=https;AccountName=acme;AccountKey=a2V5;EndpointSuffix=core.windows.net",
			want: connString{serviceURL: "https://acme.blob.core.windows.net", accountName: "acme", accountKey: "a2V5"},
		},
		{
			in:   "AccountName=acme;AccountKey=a2V5==;EndpointSuffix=core.usgovcloudapi.net;",
			want: connString{serviceURL: "https://acme.blob.core.usgovcloudapi.net", accountName: "acme", accountKey: "a2V5=="},
		},
		{
			in:   "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;AccountKey=k;BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;",
			want: connString{serviceURL: "http://127.0.0.1:10000/devstoreaccount1", accountName: "devstoreaccount1", accountKey: "k"},
		},
		{
			in:   "BlobEndpoint=https://acme.blob.core.windows.net/;SharedAccessSignature=sv=2022-11-02&sig=abc%3D",
			want: connString{serviceURL: "https://acme.blob.core.windows.net/", sas: "sv=2022-11-02&sig=abc%3D"},
		},
		{
			in:   "UseDevelopmentStorage=true",
			want: connString{serviceURL: azuriteEndpoint, accountName: azuriteAccount, accountKey: azuriteKey},
		},
		{in: "", wantErr: true},
		{in: "garbage", wantErr: true},
		{in: "AccountKey=k", wantErr: true},
		{in: "AccountName=acme", wantErr: true},
	} {
		got, err := parseConnectionString(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parse(%q) err = %v", tc.in, err)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parse(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestNewValidationAndDescribe(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		cfg  config.AzureConfig
	}{
		{"no container", config.AzureConfig{Account: "a"}},
		{"no account", config.AzureConfig{Container: "c"}},
		{"key without account", config.AzureConfig{Container: "c", AccountKey: "a2V5"}},
		{"bad connection string", config.AzureConfig{Container: "c", ConnectionString: "nope"}},
		{"bad key", config.AzureConfig{Container: "c", Account: "a", AccountKey: "%%%not-base64"}},
	} {
		if _, err := New(ctx, tc.cfg); err == nil {
			t.Errorf("%s: New succeeded", tc.name)
		}
	}
	s, err := New(ctx, config.AzureConfig{Container: "sites", Account: "acme", AccountKey: "a2V5", Prefix: "p"})
	if err != nil {
		t.Fatal(err)
	}
	want := blob.Description{Type: "azure", Container: "sites", Account: "acme", Prefix: "p/"}
	if d := s.Describe(); d != want {
		t.Errorf("Describe = %+v, want %+v", d, want)
	}
	req, err := s.PresignPut(ctx, "sites/foo/index.html", blob.PresignPutOptions{Size: 10}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(req.URL)
	q := u.Query()
	if u.Host != "acme.blob.core.windows.net" || u.Path != "/sites/p/sites/foo/index.html" ||
		q.Get("sp") != "cw" || q.Get("sr") != "b" || q.Get("spr") != "https" || q.Get("sig") == "" {
		t.Errorf("SAS URL = %s", req.URL)
	}
	// A SAS cannot bind the size: only the blob type and content type are
	// sent (the deploy gateway verifies sizes at commit).
	if len(req.Headers) != 2 || req.Headers["x-ms-blob-type"] != "BlockBlob" || req.Headers["Content-Type"] != "text/html; charset=utf-8" {
		t.Errorf("Headers = %v", req.Headers)
	}
	if d := time.Until(req.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("ExpiresAt in %v", d)
	}
	if _, err := s.PresignPut(ctx, "a.txt", blob.PresignPutOptions{Size: 1, SHA256: "nothex"}, time.Hour); err == nil || errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("PresignPut(bad sha256) err = %v", err)
	}
	get, err := s.PresignGet(ctx, "a b.txt", 0)
	if err != nil {
		t.Fatal(err)
	}
	gu, _ := url.Parse(get.URL)
	if gu.Query().Get("sp") != "r" || gu.Path != "/sites/p/a b.txt" {
		t.Errorf("GET SAS URL = %s", get.URL)
	}
	// Keys that only become too long with the prefix are invalid keys.
	if _, err := s.Stat(ctx, strings.Repeat("k", 1023)); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("Stat(prefixed key too long) err = %v, want ErrInvalidKey", err)
	}
	if _, err := s.PresignGet(ctx, strings.Repeat("k", 1023), time.Minute); !errors.Is(err, blob.ErrInvalidKey) {
		t.Errorf("PresignGet(prefixed key too long) err = %v, want ErrInvalidKey", err)
	}
	// Endpoint errors never echo credentials.
	if _, err := New(ctx, config.AzureConfig{Container: "c", Account: "a", AccountKey: "a2V5", Endpoint: "//:?sig=topsecret"}); err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("bad endpoint error = %v", err)
	}
	// Account-SAS connection strings cannot mint new SAS URLs.
	sasOnly, err := New(ctx, config.AzureConfig{Container: "c", ConnectionString: "BlobEndpoint=https://acme.blob.core.windows.net;SharedAccessSignature=sv=2022-11-02&sig=x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sasOnly.PresignPut(ctx, "a.txt", blob.PresignPutOptions{Size: -1}, time.Minute); !errors.Is(err, blob.ErrNotSupported) {
		t.Errorf("PresignPut with account SAS err = %v", err)
	}
}

type fakeCred struct{ calls atomic.Int64 }

func (f *fakeCred) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.calls.Add(1)
	return azcore.AccessToken{Token: "fake-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// Without a shared key, presigning uses a cached user delegation key.
func TestUserDelegationSAS(t *testing.T) {
	var keyRequests atomic.Int64
	var lastExpiry atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("comp") != "userdelegationkey" || r.Method != http.MethodPost {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fake-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if i := strings.Index(string(body), "<Expiry>"); i >= 0 {
			lastExpiry.Store(string(body[i+8 : i+28]))
		}
		keyRequests.Add(1)
		w.Header().Set("Content-Type", "application/xml")
		now := time.Now().UTC()
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?><UserDelegationKey><SignedOid>11111111-1111-1111-1111-111111111111</SignedOid><SignedTid>22222222-2222-2222-2222-222222222222</SignedTid><SignedStart>%s</SignedStart><SignedExpiry>%s</SignedExpiry><SignedService>b</SignedService><SignedVersion>2025-01-05</SignedVersion><Value>c2VjcmV0LWtleS1ieXRlcy0wMTIzNDU2Nzg5YWJjZGVm</Value></UserDelegationKey>`,
			now.Format(time.RFC3339), now.Add(24*time.Hour).Format(time.RFC3339))
	}))
	defer ts.Close()
	cred := &fakeCred{}
	ctx := context.Background()
	s, err := New(ctx, config.AzureConfig{Account: "acme", Container: "sites", Endpoint: ts.URL},
		WithCredential(cred),
		WithClientOptions(func(o *service.ClientOptions) { o.InsecureAllowCredentialWithHTTP = true }))
	if err != nil {
		t.Fatal(err)
	}
	if !s.tokenAuth || s.sharedKey != nil {
		t.Fatalf("expected token auth")
	}
	if d := s.Describe(); d.Endpoint == "" || d.Account != "acme" {
		t.Errorf("Describe = %+v", d)
	}
	for range 3 {
		req, err := s.PresignPut(ctx, "up.txt", blob.PresignPutOptions{ContentType: "text/plain", Size: -1}, 15*time.Minute)
		if err != nil {
			t.Fatalf("PresignPut: %v", err)
		}
		q, _ := url.ParseQuery(req.URL[strings.IndexByte(req.URL, '?')+1:])
		if q.Get("skoid") != "11111111-1111-1111-1111-111111111111" || q.Get("sktid") == "" ||
			q.Get("sp") != "cw" || q.Get("spr") != "https,http" || q.Get("sig") == "" {
			t.Errorf("user delegation SAS = %s", req.URL)
		}
	}
	if n := keyRequests.Load(); n != 1 {
		t.Errorf("delegation key fetched %d times, want 1 (cached)", n)
	}
	// A SAS outliving the cached key needs a new, longer key.
	req, err := s.PresignGet(ctx, "up.txt", 3*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n := keyRequests.Load(); n != 2 {
		t.Errorf("delegation key fetched %d times, want 2", n)
	}
	if d := time.Until(req.ExpiresAt); d < 71*time.Hour || d > 73*time.Hour {
		t.Errorf("ExpiresAt in %v", d)
	}
	exp, _ := time.Parse(time.RFC3339, lastExpiry.Load().(string))
	if exp.Before(req.ExpiresAt) {
		t.Errorf("delegation key expiry %v before SAS expiry %v", exp, req.ExpiresAt)
	}
	// The 7-day cap applies to both the key and the SAS.
	long, err := s.PresignGet(ctx, "up.txt", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(long.ExpiresAt); d > 7*24*time.Hour {
		t.Errorf("SAS valid for %v, beyond 7 days", d)
	}
}

func TestErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&azcore.ResponseError{StatusCode: 404, ErrorCode: "BlobNotFound"}, true},
		{&azcore.ResponseError{StatusCode: 404}, true},
		{&azcore.ResponseError{StatusCode: 404, ErrorCode: "ContainerNotFound"}, false},
		{&azcore.ResponseError{StatusCode: 403, ErrorCode: "AuthorizationFailure"}, false},
		{errors.New("x"), false},
	} {
		if got := isNotFound(tc.err); got != tc.want {
			t.Errorf("isNotFound(%v) = %v", tc.err, got)
		}
	}
}

// Bodies above singleUploadMax (or of unknown size beyond it) are staged as
// blocks; the result must match a single-shot upload.
func TestStagedBlockUpload(t *testing.T) {
	a := newAzurite(t)
	ctx := context.Background()
	s, err := New(ctx, config.AzureConfig{Container: a.container(t), ConnectionString: a.cs})
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, singleUploadMax+3*blockSize/2+7)
	for i := range data {
		data[i] = byte(i * 7)
	}
	for _, size := range []int64{int64(len(data)), -1} {
		key := fmt.Sprintf("big/%d.bin", size)
		info, err := s.Put(ctx, key, struct{ io.Reader }{bytes.NewReader(data)}, size, "")
		if err != nil {
			t.Fatalf("Put(size=%d): %v", size, err)
		}
		if info.Size != int64(len(data)) || info.ContentType != "application/octet-stream" {
			t.Errorf("Put info = %+v", info)
		}
		st, err := s.Stat(ctx, key)
		if err != nil || st.ETag != info.ETag || st.Size != info.Size {
			t.Errorf("Stat = %+v, %v; Put = %+v", st, err, info)
		}
		rc, _, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Errorf("round trip mismatch (%d bytes, %v)", len(got), err)
		}
	}
	// A body that is shorter than declared must not commit a blob.
	short := io.LimitReader(bytes.NewReader(data), int64(singleUploadMax+10))
	if _, err := s.Put(ctx, "big/short.bin", short, int64(len(data)), ""); err == nil {
		t.Fatal("short staged upload succeeded")
	}
	if _, err := s.Stat(ctx, "big/short.bin"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat(short) = %v", err)
	}
}
