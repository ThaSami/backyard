package netguard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
)

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
		{"203.0.113.7", false},         // documentation range
	}
	for _, tt := range tests {
		if got := Public(netip.MustParseAddr(tt.ip)); got != tt.want {
			t.Errorf("Public(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
	if Public(netip.Addr{}) {
		t.Error("the zero address is not public")
	}
}

// TestDialTimeRefusal: a direct connection to a loopback server is refused
// when the connection is dialled, before any byte is sent.
func TestDialTimeRefusal(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	c := &http.Client{Transport: NewTransport(func(*http.Request) (*url.URL, error) { return nil, nil })}
	resp, err := c.Get(srv.URL + "/x")
	if err == nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrForbiddenAddress) {
		t.Fatalf("want ErrForbiddenAddress, got %v", err)
	}
	if hits != 0 {
		t.Fatalf("the server was reached %d times", hits)
	}
}

func TestProxiedChecksTarget(t *testing.T) {
	var hits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	g := NewTransport(func(*http.Request) (*url.URL, error) { return pu, nil })
	for _, target := range []string{"http://10.0.0.5/a", "http://169.254.169.254/latest/meta-data", "http://[::1]/x", "http://localhost/x"} {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		if _, err := g.RoundTrip(req); !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("%s: want ErrForbiddenAddress, got %v", target, err)
		}
	}
	if hits != 0 {
		t.Errorf("proxy was contacted %d times", hits)
	}
	req, _ := http.NewRequest(http.MethodGet, "http://1.1.1.1/a", nil)
	resp, err := g.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Errorf("a public target should be proxied, hits=%d", hits)
	}
	// A proxy selection error is reported, nothing is sent.
	bad := NewTransport(func(*http.Request) (*url.URL, error) { return nil, errors.New("boom") })
	if _, err := bad.RoundTrip(req); err == nil {
		t.Error("proxy errors must fail the request")
	}
}

func TestCheckHost(t *testing.T) {
	ctx := context.Background()
	for _, h := range []string{"127.0.0.1", "10.0.0.1", "::1", "localhost"} {
		if err := CheckHost(ctx, h); !errors.Is(err, ErrForbiddenAddress) {
			t.Errorf("CheckHost(%q) = %v, want ErrForbiddenAddress", h, err)
		}
	}
	if err := CheckHost(ctx, "8.8.8.8"); err != nil {
		t.Errorf("CheckHost(8.8.8.8) = %v", err)
	}
	if err := CheckHost(ctx, "no-such-host.invalid"); err == nil || errors.Is(err, ErrForbiddenAddress) {
		t.Errorf("an unresolvable host is a resolution error, got %v", err)
	}
}
