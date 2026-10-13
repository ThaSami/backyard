package deploy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/version"
)

func TestNewClient(t *testing.T) {
	c := NewClient(" https://x.example.com/gw/ ", " tok ")
	if c.BaseURL != "https://x.example.com/gw" || c.Token != "tok" {
		t.Errorf("client = %+v", c)
	}
	if c.UserAgent != "backyard-cli/"+version.Version {
		t.Errorf("UA = %q", c.UserAgent)
	}
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout == 0 || tr.TLSHandshakeTimeout == 0 || c.HTTP.Jar == nil {
		t.Errorf("transport = %+v", c.HTTP.Transport)
	}
}

func TestClientHeaders(t *testing.T) {
	var got http.Header
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, method = r.Header.Clone(), r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"x","owners":[]}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "secret")
	if _, err := c.Refresh(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if method != "POST" || got.Get("Authorization") != "Bearer secret" || got.Get("X-Backyard") != "1" ||
		!strings.HasPrefix(got.Get("User-Agent"), "backyard-cli/") || got.Get("Content-Type") != "application/json" {
		t.Errorf("headers = %v", got)
	}
	if _, err := c.GetSite(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if got.Get("X-Backyard") != "" {
		t.Error("X-Backyard sent on GET")
	}
	c.Token = ""
	if _, err := c.GetSite(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "" {
		t.Error("Authorization sent without a token")
	}
}

func TestClientErrorParsing(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantMsg    string
		wantLogin  string
		wantStatus int
	}{
		{"envelope", 401, `{"error":{"code":"unauthenticated","message":"log in","login_url":"https://l"}}`, "unauthenticated", "log in", "https://l", 401},
		{"plain text 404", 404, "404 page not found", "not_found", "HTTP 404: 404 page not found", "", 404},
		{"empty 403", 403, "", "forbidden", "HTTP 403: Forbidden", "", 403},
		{"html 413", 413, "<html><body>Too big</body></html>", "too_large", "HTTP 413: <html><body>Too big</body></html>", "", 413},
		{"unknown 418", 418, "teapot", "bad_request", "HTTP 418: teapot", "", 418},
		{"500", 500, `{"error":{"code":"internal","message":"internal error"}}`, "internal", "internal error", "", 500},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			_, err := NewClient(srv.URL, "").ListSites(context.Background())
			ae := apiErr(t, err, tc.wantStatus, tc.wantCode)
			if ae.Message != tc.wantMsg || ae.LoginURL != tc.wantLogin {
				t.Errorf("APIError = %+v", ae)
			}
			if ae.Error() != tc.wantCode+": "+tc.wantMsg {
				t.Errorf("Error() = %q", ae.Error())
			}
		})
	}
}

func TestClientRetriesTransientGatewayErrors(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			_, _ = io.WriteString(w, `{"sites":[]}`)
		}
	}))
	defer srv.Close()
	if _, err := NewClient(srv.URL, "").ListSites(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}

	// The gateway's own 500 is deterministic: no retry.
	calls.Store(0)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()
	_, err := NewClient(srv2.URL, "").ListSites(context.Background())
	apiErr(t, err, 500, CodeInternal)
	if calls.Load() != 1 {
		t.Errorf("500 retried: calls = %d", calls.Load())
	}

	// Persistent 503: gives up after maxAttempts.
	calls.Store(0)
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv3.Close()
	_, err = NewClient(srv3.URL, "").ListSites(context.Background())
	apiErr(t, err, 503, CodeUpstream)
	if calls.Load() != maxAttempts {
		t.Errorf("calls = %d, want %d", calls.Load(), maxAttempts)
	}
}

func TestClientNetworkErrorAndCancel(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // connection refused from now on
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := NewClient(url, "").Info(ctx)
	if err == nil {
		t.Fatal("expected a network error")
	}
	var ae *APIError
	if errors.As(err, &ae) {
		t.Errorf("network error became an APIError: %v", err)
	}
	if time.Since(start) < 3*retryBase {
		t.Errorf("no backoff between retries (%v)", time.Since(start))
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if _, err := NewClient(url, "").Info(ctx2); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: %v", err)
	}
}

func TestUploadRetriesAndPresignedErrors(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	ps := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, "<Error><Code>SlowDown</Code></Error>")
			return
		}
		e.store.PresignHandler().ServeHTTP(w, r)
	}))
	defer ps.Close()
	e.store.EnablePresign(ps.URL)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "retry me"})
	if _, err := e.client("alice").Deploy(context.Background(), "retry", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("upload attempts = %d, want 3", calls.Load())
	}

	// A permanent storage error (403) fails the deploy without committing.
	e2 := newEnv(t)
	ps2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>")
	}))
	defer ps2.Close()
	e2.store.EnablePresign(ps2.URL)
	_, err := e2.client("alice").Deploy(context.Background(), "denied", dir, DeployOptions{})
	ae := apiErr(t, err, 403, CodeForbidden)
	if !strings.Contains(ae.Message, "SignatureDoesNotMatch") {
		t.Errorf("message = %q", ae.Message)
	}
	if !strings.Contains(err.Error(), "index.html") {
		t.Errorf("error does not name the file: %v", err)
	}
	if ok, _ := e2.reg.Exists(context.Background(), "denied"); ok {
		t.Error("site exists after a failed deploy")
	}
}

func TestFileChangedDuringDeploy(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "original"})
	c := e.client("alice")
	gwURL := e.srv.URL
	// Change the file between the plan and the upload.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/deploys") {
			writeTree(t, dir, map[string]string{"index.html": "changed, and longer"})
		}
		req, _ := http.NewRequest(r.Method, gwURL+r.URL.Path, r.Body)
		req.Header = r.Header
		req.ContentLength = r.ContentLength
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer resp.Body.Close()
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()
	c.BaseURL = proxy.URL
	_, err := c.Deploy(context.Background(), "changing", dir, DeployOptions{})
	if err == nil || !strings.Contains(err.Error(), "changed since it was scanned") {
		t.Errorf("deploy with a changing file: %v", err)
	}
}

func TestDryRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>Dry</title>", "a.txt": "a", "b.txt": "b"})
	c := e.client("alice")

	// New site: everything would be uploaded; nothing is written.
	puts := e.store.PutCount()
	res, err := c.Deploy(ctx, "dry", dir, DeployOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 3 || res.Unchanged != 0 || res.Deleted != 0 || len(res.Files) != 3 || res.Bytes == 0 {
		t.Errorf("dry run (new site) = %+v", res)
	}
	if e.store.PutCount() != puts || len(e.store.Keys()) != 0 {
		t.Error("dry run wrote to storage")
	}

	if _, err := c.Deploy(ctx, "dry", dir, DeployOptions{}); err != nil {
		t.Fatal(err)
	}
	writeTree(t, dir, map[string]string{"a.txt": "A!"})
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	puts = e.store.PutCount()
	res, err = c.Deploy(ctx, "dry", dir, DeployOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 1 || res.Unchanged != 1 || res.Deleted != 1 || res.Bytes != 2 {
		t.Errorf("dry run (existing site) = %+v", res)
	}
	if res.URL != "https://dry.backyard.test/" || res.Site.Title != "Dry" {
		t.Errorf("dry run site = %+v / %q", res.Site, res.URL)
	}
	if e.store.PutCount() != puts {
		t.Error("dry run wrote to storage")
	}

	// Unreachable gateway: a full-upload plan, no error.
	off := NewClient("http://127.0.0.1:1", "")
	ctx2, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err = off.Deploy(ctx2, "dry", dir, DeployOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Uploaded != 2 || res.Unchanged != 0 {
		t.Errorf("offline dry run = %+v", res)
	}
}

func TestResolveAndSameOrigin(t *testing.T) {
	c := NewClient("https://hub.example.com/__backyard/api/deploy", "")
	tests := []struct {
		raw, want  string
		sameOrigin bool
	}{
		{"https://bucket.s3.amazonaws.com/sites/x/a?X-Amz=1", "https://bucket.s3.amazonaws.com/sites/x/a?X-Amz=1", false},
		{"https://hub.example.com/__backyard/api/deploy/uploads/tok", "https://hub.example.com/__backyard/api/deploy/uploads/tok", true},
		{"uploads/tok", "https://hub.example.com/__backyard/api/deploy/uploads/tok", true},
		{"/other/uploads/tok", "https://hub.example.com/other/uploads/tok", true},
		{"https://HUB.example.com/x", "https://HUB.example.com/x", true},
		{"http://hub.example.com/x", "http://hub.example.com/x", false},
	}
	for _, tc := range tests {
		u, err := c.resolve(tc.raw)
		if err != nil {
			t.Fatalf("resolve(%q): %v", tc.raw, err)
		}
		if u.String() != tc.want || c.sameOrigin(u) != tc.sameOrigin {
			t.Errorf("resolve(%q) = %s (same origin %v)", tc.raw, u, c.sameOrigin(u))
		}
	}
	if _, err := c.resolve("ftp://x/y"); err == nil {
		t.Error("ftp upload URL accepted")
	}
}

func TestMain(m *testing.M) {
	retryBase = 5 * time.Millisecond // keep retry tests fast
	os.Exit(m.Run())
}

func TestBackoff(t *testing.T) {
	saved := retryBase
	retryBase = 500 * time.Millisecond
	defer func() { retryBase = saved }()
	for attempt := 1; attempt <= 6; attempt++ {
		d := backoff(attempt)
		base := min(500*time.Millisecond<<(attempt-1), 8*time.Second)
		if d < base*3/4 || d > base*5/4 {
			t.Errorf("backoff(%d) = %v, want ~%v", attempt, d, base)
		}
	}
}
