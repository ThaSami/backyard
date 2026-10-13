package deploy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/internal/sites/sitestest"
)

func lambdaEvent(method, rawPath, query string, headers map[string]string, body []byte, b64 bool) events.LambdaFunctionURLRequest {
	ev := events.LambdaFunctionURLRequest{
		Version:        "2.0",
		RawPath:        rawPath,
		RawQueryString: query,
		Headers:        map[string]string{"host": "abc.lambda-url.us-east-1.on.aws", "x-forwarded-proto": "https"},
		RequestContext: events.LambdaFunctionURLRequestContext{
			DomainName: "abc.lambda-url.us-east-1.on.aws",
			HTTP: events.LambdaFunctionURLRequestContextHTTPDescription{
				Method:   method,
				Path:     rawPath,
				SourceIP: "203.0.113.7",
			},
		},
	}
	for k, v := range headers {
		ev.Headers[k] = v
	}
	if b64 {
		ev.Body, ev.IsBase64Encoded = base64.StdEncoding.EncodeToString(body), true
	} else {
		ev.Body = string(body)
	}
	return ev
}

func lambdaBody(t *testing.T, resp events.LambdaFunctionURLResponse) []byte {
	t.Helper()
	if !resp.IsBase64Encoded {
		return []byte(resp.Body)
	}
	b, err := base64.StdEncoding.DecodeString(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLambdaAdapterRequestAndResponse(t *testing.T) {
	binary := []byte{0x00, 0xff, 0xfe, 0x89, 'P', 'N', 'G', 0x0d, 0x0a}
	var seen *http.Request
	var seenBody []byte
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Add("Set-Cookie", "a=1; Path=/")
		w.Header().Add("Set-Cookie", "b=2; Path=/")
		w.Header().Add("Vary", "Accept")
		w.Header().Add("Vary", "Origin")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "9")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(binary)
	})
	ev := lambdaEvent("PUT", "/uploads/a%2Fb/c%20d", "x=1&y=%20z", map[string]string{"content-type": "application/octet-stream", "x-custom": "v1,v2"}, binary, true)
	ev.Cookies = []string{"backyard_anon=abc", "other=1"}
	resp, err := LambdaHandler(h)(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}

	if seen.Method != "PUT" || seen.URL.EscapedPath() != "/uploads/a%2Fb/c%20d" || seen.URL.Path != "/uploads/a/b/c d" {
		t.Errorf("method/path = %s %s (%s)", seen.Method, seen.URL.EscapedPath(), seen.URL.Path)
	}
	if seen.URL.RawQuery != "x=1&y=%20z" || seen.URL.Query().Get("y") != " z" {
		t.Errorf("query = %q", seen.URL.RawQuery)
	}
	if seen.Host != "abc.lambda-url.us-east-1.on.aws" || seen.RemoteAddr != "203.0.113.7:0" {
		t.Errorf("host/remote = %s / %s", seen.Host, seen.RemoteAddr)
	}
	if seen.Header.Get("X-Custom") != "v1,v2" || seen.Header.Get("Content-Type") != "application/octet-stream" || seen.Header.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("headers = %v", seen.Header)
	}
	if c, err := seen.Cookie("backyard_anon"); err != nil || c.Value != "abc" {
		t.Errorf("cookie = %v, %v", c, err)
	}
	if !bytes.Equal(seenBody, binary) || seen.ContentLength != int64(len(binary)) {
		t.Errorf("body = %x (len %d)", seenBody, seen.ContentLength)
	}

	if resp.StatusCode != 201 || !resp.IsBase64Encoded || !bytes.Equal(lambdaBody(t, resp), binary) {
		t.Errorf("response = %+v", resp)
	}
	if !slices.Equal(resp.Cookies, []string{"a=1; Path=/", "b=2; Path=/"}) {
		t.Errorf("cookies = %v", resp.Cookies)
	}
	if resp.Headers["Vary"] != "Accept,Origin" || resp.Headers["Set-Cookie"] != "" ||
		resp.Headers["Content-Length"] != "" || resp.Headers["Connection"] != "" {
		t.Errorf("headers = %v", resp.Headers)
	}
}

func TestLambdaAdapterTextAndDefaults(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != http.NoBody {
			t.Errorf("empty body is %T", r.Body)
		}
		_, _ = io.WriteString(w, `{"ok":true,"ü":"✓"}`)
	})
	ev := lambdaEvent("", "", "", nil, nil, false)
	ev.RequestContext.HTTP.Path = "/info"
	resp, err := LambdaHandler(h)(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.IsBase64Encoded || resp.Body != `{"ok":true,"ü":"✓"}` {
		t.Errorf("response = %+v", resp)
	}
	if !strings.HasPrefix(resp.Headers["Content-Type"], "text/plain") {
		t.Errorf("sniffed content type = %q", resp.Headers["Content-Type"])
	}
	// Text content types with invalid UTF-8 are still base64-encoded.
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte{0xff, 0xfe})
	})
	resp, _ = LambdaHandler(h2)(context.Background(), lambdaEvent("GET", "/", "", nil, nil, false))
	if !resp.IsBase64Encoded {
		t.Error("invalid UTF-8 text returned unencoded")
	}
}

func TestLambdaAdapterErrors(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := make([]byte, 1<<20)
		for range 7 {
			_, _ = w.Write(chunk)
		}
	})
	resp, _ := LambdaHandler(h)(context.Background(), lambdaEvent("GET", "/big", "", nil, nil, false))
	if resp.StatusCode != 413 || !strings.Contains(resp.Body, CodeTooLarge) {
		t.Errorf("oversized response = %d %s", resp.StatusCode, resp.Body)
	}
	ev := lambdaEvent("PUT", "/x", "", nil, nil, false)
	ev.Body, ev.IsBase64Encoded = "!!!not base64", true
	resp, _ = LambdaHandler(h)(context.Background(), ev)
	if resp.StatusCode != 400 || !strings.Contains(resp.Body, CodeBadRequest) {
		t.Errorf("bad base64 = %d %s", resp.StatusCode, resp.Body)
	}
}

func TestIsTextContent(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html; charset=utf-8": true,
		"application/json":         true,
		"application/problem+json": true,
		"image/svg+xml":            true,
		"application/javascript":   true,
		"image/png":                false,
		"application/octet-stream": false,
		"":                         false,
		"garbage;;":                false,
	} {
		if got := isTextContent(ct); got != want {
			t.Errorf("isTextContent(%q) = %v", ct, got)
		}
	}
}

// TestLambdaGatewayRoundTrip runs the whole deploy protocol through the
// Lambda adapter, including a binary self-hosted upload.
func TestLambdaGatewayRoundTrip(t *testing.T) {
	e := newEnv(t)
	handler := LambdaHandler(e.gw)
	ctx := context.Background()
	call := func(method, path string, body []byte, b64 bool) events.LambdaFunctionURLResponse {
		t.Helper()
		u, err := url.Parse(path)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := handler(ctx, lambdaEvent(method, u.EscapedPath(), u.RawQuery, map[string]string{"authorization": "Bearer alice", "content-type": "application/json"}, body, b64))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x00, 0xff, 0x10}, 1000)...)
	html := []byte("<title>Lambda</title>")
	files := []FileEntry{
		{Path: "index.html", Size: int64(len(html)), SHA256: sha256Hex(html)},
		{Path: "img/logo.png", Size: int64(len(png)), SHA256: sha256Hex(png)},
	}
	reqBody, _ := json.Marshal(CreateDeployRequest{Files: files})
	resp := call("POST", "/sites/lam/deploys", reqBody, false)
	if resp.StatusCode != 200 {
		t.Fatalf("create = %d %s", resp.StatusCode, resp.Body)
	}
	var cr CreateDeployResponse
	if err := json.Unmarshal(lambdaBody(t, resp), &cr); err != nil {
		t.Fatal(err)
	}
	content := map[string][]byte{"index.html": html, "img/logo.png": png}
	for _, up := range cr.Uploads {
		path := strings.TrimPrefix(up.URL, e.srv.URL)
		resp := call(up.Method, path, content[up.Path], true)
		if resp.StatusCode != 200 {
			t.Fatalf("upload %s = %d %s", up.Path, resp.StatusCode, resp.Body)
		}
	}
	commitBody, _ := json.Marshal(CommitRequest{Files: files})
	resp = call("POST", "/sites/lam/deploys/"+cr.DeployID+"/commit", commitBody, false)
	if resp.StatusCode != 200 {
		t.Fatalf("commit = %d %s", resp.StatusCode, resp.Body)
	}
	var out CommitResponse
	_ = json.Unmarshal(lambdaBody(t, resp), &out)
	if out.Site.Title != "Lambda" || out.Site.FileCount != 2 {
		t.Errorf("commit response = %+v", out)
	}

	resp = call("GET", "/sites/lam/files/img/logo.png", nil, false)
	if resp.StatusCode != 200 || !resp.IsBase64Encoded || !bytes.Equal(lambdaBody(t, resp), png) {
		t.Errorf("read back png: status %d, base64 %v", resp.StatusCode, resp.IsBase64Encoded)
	}
	resp = call("GET", "/sites/lam/files/index.html", nil, false)
	if resp.IsBase64Encoded || resp.Body != string(html) {
		t.Errorf("read back html = %+v", resp)
	}
	resp = call("GET", "/info", nil, false)
	var inf Info
	_ = json.Unmarshal(lambdaBody(t, resp), &inf)
	if inf.Name != "Backyard" {
		t.Errorf("info via lambda = %+v", inf)
	}
}

// lambdaMaxPayload is the response payload limit AWS enforces (6 MiB).
const lambdaMaxPayload = 6 << 20

// functionURL serves h the way an AWS Lambda function URL in BUFFERED mode
// does: requests become events (header names lowercased, bodies base64),
// and a response whose JSON payload exceeds 6 MiB becomes a bare 502.
func functionURL(t *testing.T, h func(context.Context, events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hdr := map[string]string{"host": r.Host}
		for k, vs := range r.Header {
			if !strings.EqualFold(k, "Cookie") {
				hdr[strings.ToLower(k)] = strings.Join(vs, ",")
			}
		}
		ev := lambdaEvent(r.Method, r.URL.EscapedPath(), r.URL.RawQuery, hdr, body, true)
		for _, c := range r.Cookies() {
			ev.Cookies = append(ev.Cookies, c.String())
		}
		resp, err := h(r.Context(), ev)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		if n := lambdaPayloadSize(resp); n > lambdaMaxPayload {
			t.Errorf("%s %s: response payload of %d bytes exceeds Lambda's limit", r.Method, r.URL.Path, n)
			http.Error(w, "Internal Server Error", http.StatusBadGateway)
			return
		}
		out := []byte(resp.Body)
		if resp.IsBase64Encoded {
			if out, err = base64.StdEncoding.DecodeString(resp.Body); err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		for k, v := range resp.Headers {
			w.Header().Set(k, v)
		}
		for _, c := range resp.Cookies {
			w.Header().Add("Set-Cookie", c)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// s3LikePresign hands out presigned URLs as long as real S3 ones signed with
// role (session) credentials and SSE-KMS (~2 KB each, mostly the same
// session token), still served by the MemStore's presign handler.
type s3LikePresign struct{ *sitestest.MemStore }

func (s s3LikePresign) PresignPut(ctx context.Context, key string, opts blob.PresignPutOptions, ttl time.Duration) (*blob.PresignedRequest, error) {
	pr, err := s.MemStore.PresignPut(ctx, key, opts, ttl)
	if err != nil {
		return nil, err
	}
	sig := sha256.Sum256([]byte(pr.URL))
	digest, _ := hex.DecodeString(opts.SHA256)
	q := url.Values{
		"X-Amz-Algorithm":              {"AWS4-HMAC-SHA256"},
		"X-Amz-Credential":             {"ASIAEXAMPLEEXAMPLE12/20261013/us-east-1/s3/aws4_request"},
		"X-Amz-Date":                   {"20261013T120000Z"},
		"X-Amz-Expires":                {"3600"},
		"X-Amz-Security-Token":         {strings.Repeat("IQoJb3JpZ2luX2VjEJr//////////wEaCXVzLWVhc3QtMSJHMEUCIQD", 20)},
		"X-Amz-SignedHeaders":          {"content-length;content-type;host;x-amz-checksum-sha256;x-amz-server-side-encryption;x-amz-server-side-encryption-aws-kms-key-id"},
		"x-amz-sdk-checksum-algorithm": {"SHA256"},
		"X-Amz-Signature":              {hex.EncodeToString(sig[:])},
	}
	pr.URL += "&" + q.Encode()
	pr.Headers["Content-Length"] = strconv.FormatInt(opts.Size, 10)
	pr.Headers["X-Amz-Checksum-Sha256"] = base64.StdEncoding.EncodeToString(digest)
	pr.Headers["X-Amz-Server-Side-Encryption"] = "aws:kms"
	pr.Headers["X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"] = "arn:aws:kms:us-east-1:123456789012:key/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	return pr, nil
}

// The upload list of a deploy of thousands of files (each a long presigned
// S3 URL) exceeds Lambda's 6 MB response limit unless compressed; the CLI
// deploys such a site through the Lambda gateway.
func TestLambdaGatewayLargeDeploy(t *testing.T) {
	ms := sitestest.NewMemStore()
	ps := httptest.NewServer(ms.PresignHandler())
	t.Cleanup(ps.Close)
	ms.EnablePresign(ps.URL)
	e := newEnv(t, withStore(s3LikePresign{ms}))
	lambdaURL := functionURL(t, LambdaHandler(e.gw))

	const n = 4000
	tree := map[string]string{"index.html": "<title>Docs</title>"}
	for i := range n - 1 {
		tree[fmt.Sprintf("docs/section-%02d/page-%04d.html", i%40, i)] = fmt.Sprintf("<p>page %d</p>", i)
	}
	dir := t.TempDir()
	writeTree(t, dir, tree)

	// Without compression the plan alone is far over the limit.
	cr := e.createDeploy("alice", "handbook", map[string]string{"index.html": tree["index.html"]})
	if l := len(cr.Uploads[0].URL); l < 1500 {
		t.Fatalf("presigned URL is only %d bytes; the test needs realistic ones", l)
	}

	c := NewClient(lambdaURL.URL, "alice")
	res, err := c.Deploy(context.Background(), "handbook", dir, DeployOptions{Concurrency: 16})
	if err != nil {
		t.Fatalf("deploy of %d files through the Lambda gateway: %v", n, err)
	}
	if res.Uploaded != n || res.Site.FileCount != n || res.Site.Title != "Docs" {
		t.Errorf("result = uploaded %d, files %d, title %q", res.Uploaded, res.Site.FileCount, res.Site.Title)
	}

	// A client that does not accept gzip gets a 413 that names the
	// embedded gateway rather than an opaque 502.
	body, _ := json.Marshal(CreateDeployRequest{Files: entriesFor(tree)})
	ev := lambdaEvent("POST", "/sites/handbook/deploys", "", map[string]string{"authorization": "Bearer alice", "content-type": "application/json"}, body, false)
	resp, err := LambdaHandler(e.gw)(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	// Every file is unchanged now: a small plan.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("small plan without gzip = %d %s", resp.StatusCode, resp.Body)
	}
	files := entriesFor(tree)
	for i := range files {
		files[i].SHA256 = sha256Hex([]byte(files[i].Path)) // all changed
	}
	body, _ = json.Marshal(CreateDeployRequest{Files: files})
	ev = lambdaEvent("POST", "/sites/handbook/deploys", "", map[string]string{"authorization": "Bearer alice", "content-type": "application/json"}, body, false)
	resp, _ = LambdaHandler(e.gw)(context.Background(), ev)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(resp.Body, "--gateway https://backyard.test"+EmbeddedPath) {
		t.Errorf("large plan without gzip = %d %s", resp.StatusCode, resp.Body)
	}
	if n := lambdaPayloadSize(resp); n > lambdaMaxPayload {
		t.Errorf("413 payload is %d bytes", n)
	}
}

// File reads through the Lambda gateway: binary bodies grow by a third as
// base64, so the limit applies to the encoded payload; text is gzipped.
func TestLambdaGatewayReadFileLimits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	lambdaURL := functionURL(t, LambdaHandler(e.gw))
	random := func(n int) []byte {
		b := make([]byte, n)
		_, _ = rand.Read(b)
		return b
	}
	pngOK := append([]byte("\x89PNG\r\n\x1a\n"), random(4_000_000)...)
	pngBig := append([]byte("\x89PNG\r\n\x1a\n"), random(5_000_000)...)
	text := bytes.Repeat([]byte("<p>a long and repetitive page</p>\n"), 300_000) // ~10 MB
	for p, b := range map[string][]byte{"ok.png": pngOK, "big.png": pngBig, "big.html": text} {
		if _, err := e.store.Put(ctx, sites.SiteKey("pull", p), bytes.NewReader(b), int64(len(b)), ""); err != nil {
			t.Fatal(err)
		}
	}
	c := NewClient(lambdaURL.URL, "alice")
	if got, err := c.ReadFile(ctx, "pull", "ok.png", 0); err != nil || !bytes.Equal(got, pngOK) {
		t.Errorf("ReadFile(4 MB png): %d bytes, %v", len(got), err)
	}
	got, err := c.ReadFile(ctx, "pull", "big.html", 0)
	if err != nil || !bytes.Equal(got, text) {
		t.Errorf("ReadFile(10 MB html): %d bytes, %v", len(got), err)
	}
	_, err = c.ReadFile(ctx, "pull", "big.png", 0)
	ae := apiErr(t, err, http.StatusRequestEntityTooLarge, CodeTooLarge)
	if !strings.Contains(ae.Message, "backyard login --server https://backyard.test --gateway https://backyard.test"+EmbeddedPath) {
		t.Errorf("413 message = %q", ae.Message)
	}

	// Compressed responses carry the encoding, a weak ETag and Vary.
	resp, _ := LambdaHandler(e.gw)(ctx, lambdaEvent("GET", "/sites/pull/files/big.html", "", map[string]string{"authorization": "Bearer alice", "accept-encoding": "br, gzip;q=0.5"}, nil, false))
	if resp.StatusCode != 200 || resp.Headers["Content-Encoding"] != "gzip" || !strings.HasPrefix(resp.Headers["Etag"], `W/"`) ||
		!strings.Contains(resp.Headers["Vary"], "Accept-Encoding") || !resp.IsBase64Encoded {
		t.Errorf("gzipped read: %d %v", resp.StatusCode, resp.Headers)
	}
	zr, err := gzip.NewReader(bytes.NewReader(lambdaBody(t, resp)))
	if err != nil {
		t.Fatal(err)
	}
	if b, err := io.ReadAll(zr); err != nil || !bytes.Equal(b, text) {
		t.Errorf("gunzipped body: %d bytes, %v", len(b), err)
	}
	// Without Accept-Encoding the same file is too large.
	resp, _ = LambdaHandler(e.gw)(ctx, lambdaEvent("GET", "/sites/pull/files/big.html", "", map[string]string{"authorization": "Bearer alice"}, nil, false))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("uncompressed 10 MB read = %d", resp.StatusCode)
	}
	// Small responses stay uncompressed text.
	resp, _ = LambdaHandler(e.gw)(ctx, lambdaEvent("GET", "/info", "", map[string]string{"accept-encoding": "gzip"}, nil, false))
	if resp.StatusCode != 200 || resp.IsBase64Encoded || resp.Headers["Content-Encoding"] != "" {
		t.Errorf("small response: %d %v base64=%v", resp.StatusCode, resp.Headers, resp.IsBase64Encoded)
	}
}

func TestAcceptsGzip(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want bool
	}{
		{nil, false},
		{[]string{"gzip"}, true},
		{[]string{"br, GZIP"}, true},
		{[]string{"deflate", "x-gzip"}, true},
		{[]string{"gzip;q=0"}, false},
		{[]string{"gzip; q=0.000, br"}, false},
		{[]string{"gzip;q=0.1"}, true},
		{[]string{"identity"}, false},
		{[]string{"*"}, false},
		{[]string{"gzipx"}, false},
	} {
		if got := acceptsGzip(tc.in); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
