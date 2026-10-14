package imagefetch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

// 1x1 PNG.
var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func TestPublic(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false},
		{"fe80::1", false},
		{"fd00:ec2::254", false},
		{"0.0.0.0", false},
		{"::", false},
		{"100.64.0.1", false},
		{"224.0.0.1", false},
		{"::ffff:127.0.0.1", false},
		{"::ffff:8.8.8.8", true},
		{"64:ff9b::a9fe:a9fe", false},
		{"255.255.255.255", false},
		{"::7f00:1", false},            // IPv4-compatible 127.0.0.1
		{"2002:7f00:1::1", false},      // 6to4 of 127.0.0.1
		{"2001:0:4136:e378::1", false}, // Teredo
		{"100::1", false},              // discard-only
		{"2001:4860:4860::8888", true}, // Google DNS (outside Teredo /32)
	}
	for _, tt := range tests {
		if got := Public(netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("Public(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestFetcherRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()
	_, _, err := New().Fetch(context.Background(), srv.URL+"/a.png")
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("want ErrForbiddenAddress, got %v", err)
	}
}

func TestFetcherProxiedChecksTarget(t *testing.T) {
	// A proxy that would happily serve anything: the guard must refuse a
	// private target before the request reaches it.
	var hits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	g := newTransport(func(*http.Request) (*url.URL, error) { return pu, nil })
	for _, target := range []string{"http://10.0.0.5/a.png", "http://169.254.169.254/latest/meta-data", "http://[::1]/x", "http://localhost/x"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := g.RoundTrip(req); !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("%s: want ErrForbiddenAddress, got %v", target, err)
		}
	}
	if hits != 0 {
		t.Errorf("proxy was contacted %d times", hits)
	}
	// Public targets go through the proxy.
	req, _ := http.NewRequest(http.MethodGet, "http://1.1.1.1/a.png", nil)
	resp, err := g.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Errorf("public target should be proxied, hits=%d", hits)
	}
}

func TestFetcherRejectsBadURLs(t *testing.T) {
	f := New()
	for _, u := range []string{"file:///etc/passwd", "gopher://x", "ftp://example.com/a.png", "not a url", "http://"} {
		if _, _, err := f.Fetch(context.Background(), u); err == nil {
			t.Errorf("Fetch(%q): want error", u)
		}
	}
}

func TestGet(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/typed.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write(pngBytes)
	})
	mux.HandleFunc("/sniffed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(pngBytes)
	})
	mux.HandleFunc("/jpg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpg")
		_, _ = w.Write([]byte("\xff\xd8\xff\xe0 fake jpeg"))
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	})
	mux.HandleFunc("/missing", http.NotFound)
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
		_, _ = w.Write([]byte(strings.Repeat("x", MaxBytes)))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		path    string
		wantMT  string
		wantErr bool
	}{
		{"/typed.png", "image/png", false},
		{"/sniffed", "image/png", false},
		{"/jpg", "image/jpeg", false},
		{"/html", "", true},
		{"/missing", "", true},
		{"/huge", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			data, mt, err := Get(context.Background(), srv.Client(), srv.URL+tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mt != tt.wantMT || len(data) == 0 {
				t.Fatalf("got %q (%d bytes), want %q", mt, len(data), tt.wantMT)
			}
		})
	}
}

func TestErrorsDoNotLeakQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, _, err := Get(context.Background(), srv.Client(), srv.URL+"/x.png?sig=secret-token")
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error should not contain query: %v", err)
	}
}

func TestTransportErrorsDoNotLeakQuery(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/x.png?sig=secret-token"
	srv.Close() // connection refused
	_, _, err := Get(context.Background(), &http.Client{}, u)
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("transport error should not contain the query: %v", err)
	}
	if !strings.Contains(err.Error(), "/x.png") {
		t.Errorf("error should name the (redacted) URL: %v", err)
	}
}

func TestBudget(t *testing.T) {
	big := append(append([]byte{}, pngBytes...), make([]byte, 1000)...)
	chunked := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if chunked {
			w.(http.Flusher).Flush() // no Content-Length
		}
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	ctx := WithBudget(context.Background(), int64(2*len(big)+10))
	for i := range 2 {
		if _, _, err := Get(ctx, srv.Client(), srv.URL); err != nil {
			t.Fatalf("download %d within budget: %v", i, err)
		}
	}
	if _, _, err := Get(ctx, srv.Client(), srv.URL); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("third download: %v, want ErrBudgetExceeded", err)
	}

	// Without Content-Length the limit is enforced while reading.
	chunked = true
	ctx = WithBudget(context.Background(), int64(len(big)-1))
	if _, _, err := Get(ctx, srv.Client(), srv.URL); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("streamed download over budget: %v", err)
	}

	// An exhausted budget fails before any request is made.
	ctx = WithBudget(context.Background(), 0)
	if _, _, err := Get(ctx, srv.Client(), "http://203.0.113.1/never"); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("empty budget: %v", err)
	}

	// No budget: only the per-image cap applies.
	if _, _, err := Get(context.Background(), srv.Client(), srv.URL); err != nil {
		t.Fatal(err)
	}
}

func TestRequestError(t *testing.T) {
	bad := errors.New("bad request")
	err := RequestError(context.Background(), bad, ErrForbiddenAddress)
	if !errors.Is(err, bad) || !strings.Contains(err.Error(), "address not allowed") {
		t.Errorf("err = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()
	err = RequestError(ctx, bad, errors.New("i/o timeout"))
	if errors.Is(err, bad) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a timed-out fetch must surface the context error, got %v", err)
	}
}
