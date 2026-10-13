package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/sites"
)

// patterned returns n bytes that differ at every offset modulo 251.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestResponseLengthCapDetection(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	if h := newHarness(t); h.srv.maxFixedLength != 0 {
		t.Fatalf("cap without K_SERVICE = %d", h.srv.maxFixedLength)
	}
	t.Setenv("K_SERVICE", "backyard")
	if h := newHarness(t); h.srv.maxFixedLength != 32<<20 {
		t.Fatalf("cap on Cloud Run = %d, want 32 MiB", h.srv.maxFixedLength)
	}
}

// TestCloudRunLargeStaticOverHTTP1 serves a file just above 32 MiB through a
// real HTTP/1.1 server with K_SERVICE set.
func TestCloudRunLargeStaticOverHTTP1(t *testing.T) {
	t.Setenv("K_SERVICE", "backyard")
	h := newHarness(t)
	const size = 32<<20 + 3
	big := patterned(size)
	h.blob.set(sites.SiteKey("foo", "big.bin"), big, time.Now())
	ts := httptest.NewServer(h.h)
	defer ts.Close()
	do := func(method string, hdr ...string) (*http.Response, []byte) {
		t.Helper()
		r, err := http.NewRequest(method, ts.URL+"/big.bin", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = "foo.backyard.test"
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := ts.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s: reading body: %v", method, err)
		}
		return resp, b
	}

	// Full GET: chunked, complete.
	resp, b := do("GET")
	if resp.StatusCode != 200 || resp.ContentLength != -1 || len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("GET: status %d, Content-Length %d, Transfer-Encoding %v", resp.StatusCode, resp.ContentLength, resp.TransferEncoding)
	}
	if !bytes.Equal(b, big) {
		t.Fatalf("GET body: %d bytes, want %d", len(b), size)
	}
	if resp.Header.Get("ETag") == "" || resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("GET headers: %v", resp.Header)
	}

	// HEAD mirrors GET's headers: no Content-Length either.
	resp, _ = do("HEAD")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Length") != "" {
		t.Fatalf("HEAD: status %d, Content-Length %q", resp.StatusCode, resp.Header.Get("Content-Length"))
	}

	// Open-ended ranges are shortened to 32 MiB.
	resp, b = do("GET", "Range", "bytes=0-")
	if resp.StatusCode != 206 || resp.ContentLength != 32<<20 || resp.Header.Get("Content-Range") != "bytes 0-33554431/33554435" {
		t.Fatalf("bytes=0-: status %d, Content-Length %d, Content-Range %q", resp.StatusCode, resp.ContentLength, resp.Header.Get("Content-Range"))
	}
	if !bytes.Equal(b, big[:32<<20]) {
		t.Fatal("bytes=0-: body mismatch")
	}
	// The client continues where the shortened range ended.
	resp, b = do("GET", "Range", "bytes=33554432-")
	if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 33554432-33554434/33554435" || !bytes.Equal(b, big[32<<20:]) {
		t.Fatalf("continuation: status %d, Content-Range %q, %d bytes", resp.StatusCode, resp.Header.Get("Content-Range"), len(b))
	}
	resp, b = do("GET", "Range", "bytes=1-")
	if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != "bytes 1-33554432/33554435" || !bytes.Equal(b, big[1:1+32<<20]) {
		t.Fatalf("bytes=1-: status %d, Content-Range %q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
	// Small ranges are untouched.
	resp, b = do("GET", "Range", "bytes=-1000")
	if resp.StatusCode != 206 || resp.ContentLength != 1000 || !bytes.Equal(b, big[size-1000:]) {
		t.Fatalf("suffix range: status %d, Content-Length %d", resp.StatusCode, resp.ContentLength)
	}
}

func TestResponseLengthCapBoundaries(t *testing.T) {
	const limit = 4096
	h := newHarness(t)
	h.srv.maxFixedLength = limit
	exact := patterned(limit)
	over := patterned(limit + 1)
	h.blob.set(sites.SiteKey("foo", "exact.bin"), exact, time.Now())
	h.blob.set(sites.SiteKey("foo", "over.bin"), over, time.Now())
	h.deploy("bar", map[string]string{"index.html": "bar", "404.html": string(over)})

	w := h.get("https://foo.backyard.test/exact.bin")
	expectStatus(t, w, 200)
	if w.Header().Get("Content-Length") != strconv.Itoa(limit) {
		t.Fatalf("at the limit: Content-Length %q", w.Header().Get("Content-Length"))
	}
	w = h.get("https://foo.backyard.test/over.bin")
	expectStatus(t, w, 200)
	if w.Header().Get("Content-Length") != "" || !bytes.Equal(w.Body.Bytes(), over) {
		t.Fatalf("above the limit: Content-Length %q, %d bytes", w.Header().Get("Content-Length"), w.Body.Len())
	}
	// Custom 404 pages are full responses too.
	w = h.get("https://bar.backyard.test/missing")
	expectStatus(t, w, 404)
	if w.Header().Get("Content-Length") != "" || w.Body.Len() != limit+1 {
		t.Fatalf("404.html: Content-Length %q, %d bytes", w.Header().Get("Content-Length"), w.Body.Len())
	}
	w = h.get("https://foo.backyard.test/over.bin", "Range", "bytes=0-")
	expectStatus(t, w, 206)
	if w.Header().Get("Content-Range") != "bytes 0-4095/4097" || w.Header().Get("Content-Length") != "4096" || !bytes.Equal(w.Body.Bytes(), over[:limit]) {
		t.Fatalf("range: %v, %d bytes", w.Header(), w.Body.Len())
	}
	w = h.get("https://foo.backyard.test/over.bin", "Range", "bytes=0-4095")
	expectStatus(t, w, 206)
	if w.Header().Get("Content-Range") != "bytes 0-4095/4097" {
		t.Fatalf("range at the limit: Content-Range %q", w.Header().Get("Content-Range"))
	}

	// HTTP/2 has no such limit.
	r := httptest.NewRequest("GET", "https://foo.backyard.test/over.bin", nil)
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
	w = serveRaw(h, r)
	expectStatus(t, w, 200)
	if w.Header().Get("Content-Length") != strconv.Itoa(limit+1) {
		t.Fatalf("HTTP/2: Content-Length %q", w.Header().Get("Content-Length"))
	}
	r = httptest.NewRequest("GET", "https://foo.backyard.test/over.bin", nil)
	r.Proto, r.ProtoMajor, r.ProtoMinor = "HTTP/2.0", 2, 0
	r.Header.Set("Range", "bytes=0-")
	w = serveRaw(h, r)
	if w.Code != 206 || w.Header().Get("Content-Range") != "bytes 0-4096/4097" {
		t.Fatalf("HTTP/2 range: %d %q", w.Code, w.Header().Get("Content-Range"))
	}

	// Without a cap, nothing changes.
	h.srv.maxFixedLength = 0
	w = h.get("https://foo.backyard.test/over.bin")
	if w.Header().Get("Content-Length") != strconv.Itoa(limit+1) {
		t.Fatalf("no cap: Content-Length %q", w.Header().Get("Content-Length"))
	}
}

func TestResponseLengthCapUploads(t *testing.T) {
	const limit = 4096
	h := newHarness(t)
	h.srv.maxFixedLength = limit
	content := string(patterned(3 * limit))
	res := h.upload(filesURL, part{field: "file", filename: "big.bin", ctype: "application/octet-stream", content: content})
	if res.code != 201 {
		t.Fatalf("upload: %d %s", res.code, res.body)
	}
	var f fileJSON
	if err := jsonUnmarshal(res.body, &f); err != nil {
		t.Fatal(err)
	}
	u := "https://foo.backyard.test" + f.URL
	w := h.get(u)
	expectStatus(t, w, 200)
	if w.Header().Get("Content-Length") != "" || w.Body.String() != content {
		t.Fatalf("download: Content-Length %q, %d bytes", w.Header().Get("Content-Length"), w.Body.Len())
	}
	w = h.get(u, "Range", "bytes=100-")
	expectStatus(t, w, 206)
	if w.Header().Get("Content-Range") != "bytes 100-4195/12288" || w.Body.String() != content[100:100+limit] {
		t.Fatalf("range: Content-Range %q, %d bytes", w.Header().Get("Content-Range"), w.Body.Len())
	}
}

// failingReader returns n bytes of data, then err.
type failingReader struct {
	n   int
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n == 0 {
		return 0, f.err
	}
	k := min(len(p), f.n)
	for i := range p[:k] {
		p[i] = 'x'
	}
	f.n -= k
	return k, nil
}

func (f *failingReader) Close() error { return nil }

// TestChunkedStreamFailureAborts checks that a storage failure in the middle
// of a chunked response aborts the connection instead of ending the body as
// if it were complete.
func TestChunkedStreamFailureAborts(t *testing.T) {
	for _, limit := range []int64{4096, 0} {
		h := newHarness(t)
		h.srv.maxFixedLength = limit
		obj := &object{
			name: "big.bin", size: 1 << 20, etag: `"e"`, contentType: "application/octet-stream",
			open: func(context.Context, int64, int64) (io.ReadCloser, error) {
				return &failingReader{n: 100 << 10, err: errors.New("storage connection reset")}, nil
			},
		}
		ts := httptest.NewServer(h.srv.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.srv.serveObject(w, r, obj, serveOptions{})
		})))
		resp, err := ts.Client().Get(ts.URL + "/big.bin")
		if err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		ts.Close()
		if err == nil {
			t.Fatalf("limit %d: truncated body (%d of %d bytes) read without error", limit, n, obj.size)
		}
		if n >= obj.size {
			t.Fatalf("limit %d: read %d bytes", limit, n)
		}
	}
}
