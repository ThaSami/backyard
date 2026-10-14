// Package netguard guards outbound HTTP requests whose target comes from
// outside the server (an end user's image URL, an MCP client metadata
// document) against server-side request forgery: connections to loopback,
// private, link-local, multicast or otherwise non-public addresses are
// refused. Direct connections are checked on the dialled IP, so DNS
// rebinding cannot bypass the check; requests routed through the
// environment's proxy are checked on the target host's resolved addresses
// before they are handed to the proxy.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// ErrForbiddenAddress is returned when a target resolves to a non-public
// address.
var ErrForbiddenAddress = errors.New("netguard: address not allowed")

// NewTransport returns a guarded transport. proxy selects the proxy for a
// request (http.ProxyFromEnvironment in production). Direct connections may
// only reach public addresses (enforced on the dialled IP); a request that
// proxy routes through a proxy is refused unless every resolved address of
// its target host is public. NewTransport performs no I/O.
func NewTransport(proxy func(*http.Request) (*url.URL, error)) http.RoundTripper {
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
		return nil, fmt.Errorf("netguard: proxy: %w", err)
	}
	if p == nil {
		return g.direct.RoundTrip(req)
	}
	if err := CheckHost(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	return g.proxied.RoundTrip(req)
}

// CheckHost resolves host and rejects it (ErrForbiddenAddress) unless every
// address is public. An IP literal is checked as is.
func CheckHost(ctx context.Context, host string) error {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !Public(ip) {
			return fmt.Errorf("%w: %s", ErrForbiddenAddress, host)
		}
		return nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("netguard: resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("netguard: resolve %s: no addresses", host)
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
