package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
)

// TestOIDCEndToEndOverHTTP drives a real browser-like client (cookie jar,
// redirects) through hub and site hosts: site → login → IdP → callback →
// site, then a second site without a new login, then logout. The cookie jar
// enforces the Domain/Secure/Path attributes the way browsers do.
func TestOIDCEndToEndOverHTTP(t *testing.T) {
	e := newOIDCEnv(t, func(c *config.Config) { c.Auth.Admins, c.Auth.AdminGroups = nil, nil })
	a := e.a

	// What the server does for any site request: authenticate, or send
	// browsers to the login URL with the original URL as rd.
	site := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Authenticate(w, r)
		switch {
		case errors.Is(err, ErrUnauthenticated):
			http.Redirect(w, r, a.LoginURL("https://"+r.Host+r.URL.RequestURI()), http.StatusFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusForbidden)
		default:
			_, _ = io.WriteString(w, r.Host+" "+u.ID)
		}
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, PathPrefix) {
			if r.Host != "backyard.test" {
				t.Errorf("auth endpoint reached on %s", r.Host)
			}
			a.Handler().ServeHTTP(w, r)
			return
		}
		site.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	var dialer net.Dialer
	client := &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			// *.backyard.test resolves to the test server; the IdP is reached directly.
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if host, _, _ := net.SplitHostPort(addr); host == "backyard.test" || strings.HasSuffix(host, ".backyard.test") {
					addr = srv.Listener.Addr().String()
				}
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed httptest certificate
		},
	}
	get := func(u string) (string, string) {
		t.Helper()
		res, err := client.Get(u)
		if err != nil {
			t.Fatalf("GET %s: %v", u, err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %d %s", u, res.StatusCode, b)
		}
		return res.Request.URL.String(), string(b)
	}
	authorizations := func() int {
		e.idp.mu.Lock()
		defer e.idp.mu.Unlock()
		return len(e.idp.authorizeReqs)
	}

	final, got := get("https://foo.backyard.test/page?x=1")
	if got != "foo.backyard.test alice@acme.com" || final != "https://foo.backyard.test/page?x=1" {
		t.Fatalf("after login: %q at %s", got, final)
	}
	if authorizations() != 1 {
		t.Fatalf("authorize requests = %d", authorizations())
	}
	// Another site shares the session: no second trip to the IdP.
	if _, got := get("https://bar.backyard.test/"); got != "bar.backyard.test alice@acme.com" {
		t.Fatalf("second site: %q", got)
	}
	if authorizations() != 1 {
		t.Fatalf("second site went back to the IdP (%d)", authorizations())
	}

	// Logout ends at the IdP's end_session endpoint and clears the session.
	res, err := client.Get("https://backyard.test" + LogoutPath)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !strings.HasPrefix(res.Request.URL.String(), e.idp.issuer()+"/logout?") {
		t.Fatalf("logout ended at %s", res.Request.URL)
	}
	for _, c := range jar.Cookies(mustParseURL(t, "https://foo.backyard.test/")) {
		if c.Name == SessionCookie {
			t.Fatal("session cookie survived logout")
		}
	}
	// The next visit signs in again.
	if _, got := get("https://foo.backyard.test/"); got != "foo.backyard.test alice@acme.com" || authorizations() != 2 {
		t.Fatalf("after logout: %q (%d authorizations)", got, authorizations())
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
