package auth

import (
	"strings"
	"testing"

	"github.com/thasami/backyard/internal/config"
)

func TestSanitizeRedirect(t *testing.T) {
	a := newAuth(t, baseConfig("none"), Options{})
	hub := "https://backyard.test/"
	tests := []struct {
		in, want string
	}{
		{"", hub},
		{"   ", hub},
		{"https://backyard.test/", "https://backyard.test/"},
		{"https://backyard.test/docs?x=1#frag", "https://backyard.test/docs?x=1#frag"},
		{"https://foo.backyard.test/a/b?c=d", "https://foo.backyard.test/a/b?c=d"},
		{"HTTPS://Foo.Backyard.Test/", "https://Foo.Backyard.Test/"},
		{"https://foo.backyard.test:443/", "https://foo.backyard.test:443/"},
		{"https://foo.backyard.test./", "https://foo.backyard.test./"},
		{"/docs", "https://backyard.test/docs"},
		{"/s/foo/?q=1", "https://backyard.test/s/foo/?q=1"},
		// Open-redirect attempts.
		{"//evil.example/", hub},
		{"///evil.example/", hub},
		{"/\\evil.example/", hub},
		{"\\\\evil.example", hub},
		{"https:evil.example", hub},
		{"https://evil.example/", hub},
		{"https://backyard.test.evil.example/", hub},
		{"https://evilbackyard.test/", hub},
		{"https://a.b.backyard.test/", hub},
		{"https://-bad.backyard.test/", hub},
		{"https://foo.backyard.test:8443/", hub},
		{"http://foo.backyard.test/", hub},
		{"https://backyard.test@evil.example/", hub},
		{"https://user:pw@backyard.test/", hub},
		{"https://evil.example#.backyard.test", hub},
		{"https://evil.example?.backyard.test", hub},
		{"javascript:alert(1)", hub},
		{"data:text/html,hi", hub},
		{"evil.example", hub},
		{"foo/bar", hub},
		{"/\tevil", hub},
		{"https://backyard.test/\n", "https://backyard.test/"}, // trimmed
		{"https://foo.backyard.test/\x00", hub},
		{"https://" + strings.Repeat("a", 5000) + ".backyard.test/", hub},
		{"https://[::1]/", hub},
		{"https://127.0.0.1/", hub},
		{"https://foo.backyard.test%2e.evil.example/", hub},
		{"https://evil.example%2f.backyard.test/", hub},
		{"https://evil.example%23.backyard.test/", hub},
		{`https:\\evil.example`, hub},
		{`https://foo.backyard.test\@evil.example/`, hub},
		{"%2F%2Fevil.example", hub},
	}
	for _, tc := range tests {
		if got := a.sanitizeRedirect(tc.in); got != tc.want {
			t.Errorf("sanitizeRedirect(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeRedirectPortsAndBaseDomain(t *testing.T) {
	cfg := baseConfig("none")
	cfg.Server.BaseDomain = "localhost"
	cfg.Server.PublicURL = "http://localhost:8080"
	a := newAuth(t, cfg, Options{})
	tests := map[string]string{
		"http://localhost:8080/x":     "http://localhost:8080/x",
		"http://foo.localhost:8080/":  "http://foo.localhost:8080/",
		"http://foo.localhost/":       "http://localhost:8080/",
		"http://foo.localhost:9090/":  "http://localhost:8080/",
		"https://foo.localhost:8080/": "http://localhost:8080/",
		"/relative":                   "http://localhost:8080/relative",
	}
	for in, want := range tests {
		if got := a.sanitizeRedirect(in); got != want {
			t.Errorf("sanitizeRedirect(%q) = %q, want %q", in, got, want)
		}
	}

	// Public URL host differs from base_domain: both are accepted.
	cfg = baseConfig("none")
	cfg.Server.BaseDomain = "apps.acme.com"
	cfg.Server.PublicURL = "https://backyard.acme.com"
	a = newAuth(t, cfg, Options{})
	for in, want := range map[string]string{
		"https://foo.apps.acme.com/":     "https://foo.apps.acme.com/",
		"https://apps.acme.com/":         "https://apps.acme.com/",
		"https://foo.backyard.acme.com/": "https://foo.backyard.acme.com/",
		"https://acme.com/":              "https://backyard.acme.com/",
	} {
		if got := a.sanitizeRedirect(in); got != want {
			t.Errorf("sanitizeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeRedirectRejectsAuthFlowEndpoints(t *testing.T) {
	a := newOIDCEnv(t, func(c *config.Config) { c.Auth.OIDC.RedirectURL = "https://backyard.test/oauth2/callback" }).a
	hub := "https://backyard.test/"
	for in, want := range map[string]string{
		"/__backyard/auth/logout":                         hub,
		"/__backyard/auth/logout/":                        hub,
		"/__backyard/auth/./logout?x=1":                   hub,
		"/__backyard/auth/%6Cogout":                       hub,
		"/__backyard/auth/login?rd=/x":                    hub,
		"/__backyard/auth/callback?code=x&state=y":        hub,
		"/oauth2/callback?code=x":                         hub,
		"https://backyard.test/__backyard/auth/logout":    hub,
		"https://foo.backyard.test/__backyard/auth/login": hub,
		"/s/foo/__backyard/auth/logout":                   hub,
		// The CLI handoff resumes after login.
		"/__backyard/auth/cli?redirect_uri=http://127.0.0.1:9/cb&state=s": "https://backyard.test/__backyard/auth/cli?redirect_uri=http://127.0.0.1:9/cb&state=s",
		"/__backyard/authx/logout":                                        "https://backyard.test/__backyard/authx/logout",
	} {
		if got := a.sanitizeRedirect(in); got != want {
			t.Errorf("sanitizeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeRedirectPathMode(t *testing.T) {
	cfg := baseConfig("none")
	cfg.Server.URLMode = "path"
	a := newAuth(t, cfg, Options{})
	for in, want := range map[string]string{
		"https://backyard.test/s/foo/page": "https://backyard.test/s/foo/page",
		"/s/foo/":                          "https://backyard.test/s/foo/",
		// There are no site hosts in path mode: subdomains are someone else's.
		"https://foo.backyard.test/":     "https://backyard.test/",
		"https://evil.backyard.test/x?y": "https://backyard.test/",
	} {
		if got := a.sanitizeRedirect(in); got != want {
			t.Errorf("sanitizeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateLoopbackRedirect(t *testing.T) {
	ok := []string{
		"http://127.0.0.1:5555/callback",
		"http://localhost:1/",
		"http://LOCALHOST:65535/cb?x=1",
		"http://[::1]:8080/callback",
		"http://127.0.0.1:5555",
	}
	for _, s := range ok {
		if _, err := validateLoopbackRedirect(s); err != nil {
			t.Errorf("%q: %v", s, err)
		}
	}
	bad := []string{
		"",
		"https://127.0.0.1:5555/callback",
		"http://127.0.0.1/callback",
		"http://127.0.0.1:0/",
		"http://127.0.0.1:70000/",
		"http://127.0.0.2:5555/",
		"http://evil.example:5555/",
		"http://localhost.evil.example:5555/",
		"http://user@127.0.0.1:5555/",
		"http://127.0.0.1:5555/#frag",
		"http://127.0.0.1:5555\\@evil.example/",
		"javascript:alert(1)",
		"//127.0.0.1:5555/",
		"http://127.0.0.1:abc/",
		"http://[::2]:5555/",
		"http://127.0.0.1:5555/a\nb",
		"http://127.0.0.1:5555/a\x00",
	}
	for _, s := range bad {
		if _, err := validateLoopbackRedirect(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
