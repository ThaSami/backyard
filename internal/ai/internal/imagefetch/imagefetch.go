// Package imagefetch downloads remote images referenced by URL in chat
// requests for providers that only accept inline bytes (Gemini, Bedrock,
// Claude on Vertex AI/Bedrock, Ollama and other self-hosted OpenAI-compatible
// servers). Because the URL comes from an end user, fetching is guarded
// against server-side request forgery: only http(s) URLs are allowed and every
// connection to a loopback, private, link-local, multicast or otherwise
// non-public address is refused (checked on the dialled IP, so DNS rebinding
// cannot bypass it). Each image is capped at MaxBytes, a per-request total can
// be set with WithBudget, timeouts are bounded, and error messages never
// contain the URL's query string or credentials.
package imagefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// MaxBytes is the largest image Fetch downloads (matches the router's limit).
const MaxBytes = 20 << 20

// ErrBudgetExceeded is returned when a download would exceed the byte budget
// attached to the context with WithBudget.
var ErrBudgetExceeded = errors.New("imagefetch: total size of remote images exceeds the per-request limit")

type budgetKey struct{}

// WithBudget returns a context that limits the total number of bytes all
// downloads made with it (by Get and Fetcher.Fetch) may read. One chat request
// may reference many image URLs; the budget bounds the memory and bandwidth a
// single request can consume. The budget is shared by every download using
// the returned context and is safe for concurrent use.
func WithBudget(ctx context.Context, maxBytes int64) context.Context {
	b := &atomic.Int64{}
	b.Store(maxBytes)
	return context.WithValue(ctx, budgetKey{}, b)
}

// budget returns the remaining budget of ctx, or nil when it has none.
func budget(ctx context.Context) *atomic.Int64 {
	b, _ := ctx.Value(budgetKey{}).(*atomic.Int64)
	return b
}

// ErrForbiddenAddress is returned when a URL resolves to a non-public address.
var ErrForbiddenAddress = errors.New("imagefetch: address not allowed")

// Func fetches an image and returns its bytes and media type. Providers take
// a Func so tests can substitute a fake.
type Func func(ctx context.Context, rawURL string) (data []byte, mediaType string, err error)

// Fetcher downloads images with SSRF protection. It is safe for concurrent
// use.
type Fetcher struct {
	client *http.Client
}

// New returns a Fetcher with a guarded transport. New performs no I/O.
//
// Direct connections may only reach public addresses (enforced on the
// dialled IP). When the standard proxy environment variables route a request
// through a proxy, the target host's resolved addresses are checked before
// the request is handed to the proxy.
func New() *Fetcher {
	return &Fetcher{client: &http.Client{
		Transport: newTransport(http.ProxyFromEnvironment),
		Timeout:   30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("imagefetch: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("imagefetch: redirect to unsupported scheme %q", req.URL.Scheme)
			}
			return nil
		},
	}}
}

// newTransport builds the guarded transport; proxy selects the proxy for a
// request (http.ProxyFromEnvironment in production).
func newTransport(proxy func(*http.Request) (*url.URL, error)) guardedTransport {
	base := func() *http.Transport {
		return &http.Transport{
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          16,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		}
	}
	direct := base()
	direct.Proxy = nil
	direct.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", ErrForbiddenAddress, address)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || !Public(ip) {
				return fmt.Errorf("%w: %s", ErrForbiddenAddress, address)
			}
			return nil
		},
	}).DialContext
	proxied := base()
	proxied.Proxy = proxy
	proxied.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	return guardedTransport{proxy: proxy, direct: direct, proxied: proxied}
}

// guardedTransport routes each request either directly (dial-time IP
// check) or through the environment's proxy (pre-request DNS check).
type guardedTransport struct {
	proxy           func(*http.Request) (*url.URL, error)
	direct, proxied *http.Transport
}

// RoundTrip implements http.RoundTripper.
func (g guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	p, err := g.proxy(req)
	if err != nil {
		return nil, fmt.Errorf("imagefetch: proxy: %w", err)
	}
	if p == nil {
		return g.direct.RoundTrip(req)
	}
	if err := checkHost(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	return g.proxied.RoundTrip(req)
}

// Fetch downloads rawURL. It implements Func.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	return Get(ctx, f.client, rawURL)
}

// Get downloads an image with client (no SSRF guard of its own; use a
// Fetcher for user-supplied URLs). The media type comes from the response
// Content-Type or, when missing or generic, from content sniffing; non-image
// content is rejected. Errors never contain the URL's query string or
// credentials. A budget attached with WithBudget is honoured.
func Get(ctx context.Context, client *http.Client, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, "", fmt.Errorf("imagefetch: invalid image URL")
	}
	limit := int64(MaxBytes)
	b := budget(ctx)
	if b != nil {
		if rem := b.Load(); rem < limit {
			limit = rem
		}
		if limit <= 0 {
			return nil, "", ErrBudgetExceeded
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("imagefetch: %w", err)
	}
	req.Header.Set("Accept", "image/*")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("imagefetch: fetch %s: %w", redact(u), stripURL(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("imagefetch: fetch %s: HTTP %d", redact(u), resp.StatusCode)
	}
	if resp.ContentLength > MaxBytes {
		return nil, "", fmt.Errorf("imagefetch: image larger than %d bytes", MaxBytes)
	}
	if resp.ContentLength > limit {
		return nil, "", ErrBudgetExceeded
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("imagefetch: read %s: %w", redact(u), stripURL(err))
	}
	if int64(len(data)) > limit {
		if limit < MaxBytes {
			return nil, "", ErrBudgetExceeded
		}
		return nil, "", fmt.Errorf("imagefetch: image larger than %d bytes", MaxBytes)
	}
	if b != nil && b.Add(-int64(len(data))) < 0 {
		return nil, "", ErrBudgetExceeded // concurrent downloads raced past the budget
	}
	mt := ""
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		if parsed, _, err := mime.ParseMediaType(ct); err == nil {
			mt = strings.ToLower(parsed)
		}
	}
	if !strings.HasPrefix(mt, "image/") {
		mt = http.DetectContentType(data)
		if i := strings.IndexByte(mt, ';'); i >= 0 {
			mt = mt[:i]
		}
	}
	if !strings.HasPrefix(mt, "image/") {
		return nil, "", fmt.Errorf("imagefetch: %s is not an image (%s)", redact(u), mt)
	}
	if mt == "image/jpg" {
		mt = "image/jpeg"
	}
	return data, mt, nil
}

// RequestError converts a failed download of a user-supplied image into the
// error a provider returns: when ctx is done (the request was cancelled or
// timed out) the context error is returned so callers can tell a timeout
// from a bad request; anything else wraps badRequest (ai.ErrBadRequest).
func RequestError(ctx context.Context, badRequest, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("fetch image: %w", ctxErr)
	}
	return fmt.Errorf("%w: %v", badRequest, err)
}

// stripURL removes the full request URL (including its query string) that
// *url.Error embeds in its message, keeping the underlying cause.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// checkHost resolves host and rejects it unless every address is public.
func checkHost(ctx context.Context, host string) error {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !Public(ip) {
			return fmt.Errorf("%w: %s", ErrForbiddenAddress, host)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("imagefetch: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("imagefetch: resolve %s: no addresses", host)
	}
	for _, a := range addrs {
		if !Public(a) {
			return fmt.Errorf("%w: %s", ErrForbiddenAddress, host)
		}
	}
	return nil
}

// nonPublic lists special-purpose ranges not covered by the netip helpers.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),        // deprecated IPv4-compatible IPv6
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can map to private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),  // discard-only
	netip.MustParsePrefix("2001::/32"), // Teredo (embeds an IPv4 address)
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), // 6to4 (embeds an IPv4 address)
	netip.MustParsePrefix("fc00::/7"),
}

// Public reports whether ip is a globally routable unicast address.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || !ip.IsGlobalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// redact drops credentials and the query string from u for error messages.
func redact(u *url.URL) string {
	c := *u
	c.User = nil
	c.RawQuery = ""
	c.Fragment = ""
	return c.String()
}
