package sitestest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func put(t *testing.T, pr *blob.PresignedRequest, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(pr.Method, pr.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range pr.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func TestPresignedUploadsEnforceSizeAndDigest(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	if _, err := s.PresignPut(ctx, "a.txt", blob.PresignPutOptions{Size: -1}, time.Minute); !errors.Is(err, blob.ErrNotSupported) {
		t.Fatalf("PresignPut before EnablePresign err = %v", err)
	}
	ts := httptest.NewServer(s.PresignHandler())
	defer ts.Close()
	s.EnablePresign(ts.URL)

	data := []byte("declared")
	opts := blob.PresignPutOptions{Size: int64(len(data)), SHA256: sha256Hex(data)}
	pr, err := s.PresignPut(ctx, "a.txt", opts, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Headers["Content-Type"] != "text/plain; charset=utf-8" {
		t.Errorf("headers = %v", pr.Headers)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{"declared and more", http.StatusForbidden},
		{"decl", http.StatusForbidden},
		{"DECLARED", http.StatusBadRequest},
	} {
		if got := put(t, pr, []byte(tc.body)); got != tc.status {
			t.Errorf("PUT %q: status %d, want %d", tc.body, got, tc.status)
		}
	}
	if s.Bytes("a.txt") != nil {
		t.Fatal("a rejected upload was stored")
	}
	if got := put(t, pr, data); got != http.StatusOK || string(s.Bytes("a.txt")) != "declared" {
		t.Fatalf("exact upload: status %d, stored %q", got, s.Bytes("a.txt"))
	}

	// Unknown size and digest: anything goes.
	unsized, err := s.PresignPut(ctx, "b.bin", blob.PresignPutOptions{Size: -1}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := put(t, unsized, []byte("whatever")); got != http.StatusOK {
		t.Errorf("unsized upload status %d", got)
	}

	// Lax mode (Azure-like) accepts other sizes and content.
	s.SetLaxPresign(true)
	if got := put(t, pr, []byte("much larger than declared")); got != http.StatusOK || string(s.Bytes("a.txt")) != "much larger than declared" {
		t.Errorf("lax upload: status %d, stored %q", got, s.Bytes("a.txt"))
	}

	for name, bad := range map[string]blob.PresignPutOptions{
		"negative size": {Size: -2},
		"bad sha256":    {Size: 1, SHA256: "xyz"},
		"short sha256":  {Size: 1, SHA256: "abcd"},
	} {
		if _, err := s.PresignPut(ctx, "c.txt", bad, time.Minute); err == nil || errors.Is(err, blob.ErrNotSupported) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
