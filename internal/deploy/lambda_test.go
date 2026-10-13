package deploy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
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
