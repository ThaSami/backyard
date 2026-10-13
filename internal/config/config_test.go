package config

import (
	"strings"
	"testing"
	"time"
)

func TestExampleParsesAndValidates(t *testing.T) {
	t.Setenv("BACKYARD_SECRET", strings.Repeat("x", 40))
	cfg, err := Parse([]byte(Example))
	if err != nil {
		t.Fatalf("parse example: %v", err)
	}
	cfg.ApplyEnvOverrides()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate example: %v", err)
	}
	if cfg.Auth.Mode != "oidc" || cfg.AI.DefaultModel != "smart" {
		t.Fatalf("unexpected: mode=%s default=%s", cfg.Auth.Mode, cfg.AI.DefaultModel)
	}
	if cfg.AI.DefaultImageModel != "image" || cfg.AI.DefaultEmbeddingModel != "embed" {
		t.Fatalf("defaults: image=%q embed=%q", cfg.AI.DefaultImageModel, cfg.AI.DefaultEmbeddingModel)
	}
}

func TestDevDefaults(t *testing.T) {
	t.Setenv("BACKYARD_CONFIG_YAML", "")
	t.Setenv("BACKYARD_CONFIG", "")
	t.Chdir(t.TempDir())
	cfg, err := Load("", true)
	if err != nil {
		t.Fatalf("load dev: %v", err)
	}
	if cfg.Server.BaseDomain != "localhost" || cfg.Auth.Mode != "none" || cfg.Storage.Type != "fs" || cfg.Database.Type != "sqlite" {
		t.Fatalf("unexpected dev config: %+v", cfg.Server)
	}
	if !cfg.Deploy.AllowAnonymous {
		t.Fatal("dev should allow anonymous deploys")
	}
	if cfg.Server.PublicURL != "http://localhost:8080" {
		t.Fatalf("public url = %q", cfg.Server.PublicURL)
	}
	if got := cfg.SiteURL("demo"); got != "http://demo.localhost:8080/" {
		t.Fatalf("site url = %q", got)
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("BY_A", "alpha")
	cases := map[string]string{
		"${BY_A}":             "alpha",
		"${BY_MISSING:-dflt}": "dflt",
		"${BY_MISSING}":       "",
		"$${BY_A}":            "${BY_A}",
		"x-${BY_A}-y":         "x-alpha-y",
	}
	for in, want := range cases {
		if got := ExpandEnv(in); got != want {
			t.Errorf("ExpandEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDuration(t *testing.T) {
	cfg, err := Parse([]byte("auth:\n  token_ttl: 2d\n  oidc:\n    session_ttl: 3600\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.TokenTTL.D() != 48*time.Hour || cfg.Auth.OIDC.SessionTTL.D() != time.Hour {
		t.Fatalf("durations: %v %v", cfg.Auth.TokenTTL.D(), cfg.Auth.OIDC.SessionTTL.D())
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	if _, err := Parse([]byte("server:\n  bogus: 1\n")); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestValidateErrors(t *testing.T) {
	cfg, _ := Parse([]byte("server:\n  base_domain: x.example.com\n  secret: short\nauth:\n  mode: oidc\nstorage:\n  type: s3\n"))
	cfg.ApplyDefaults()
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	for _, want := range []string{"server.secret", "auth.oidc.issuer", "storage.s3.bucket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestAliasesAndAdmin(t *testing.T) {
	cfg, _ := Parse([]byte("auth:\n  mode: okta\n  admins: [Root@Example.com]\n  admin_groups: [ops]\n"))
	cfg.ApplyDefaults()
	if cfg.Auth.Mode != "oidc" {
		t.Fatalf("alias not normalised: %s", cfg.Auth.Mode)
	}
	if !cfg.Auth.IsAdmin("root@example.com", "", nil) || !cfg.Auth.IsAdmin("x", "", []string{"OPS"}) || cfg.Auth.IsAdmin("y", "y@e.com", []string{"dev"}) {
		t.Fatal("IsAdmin mismatch")
	}
}

func TestPathModeSiteURL(t *testing.T) {
	cfg := &Config{Server: ServerConfig{BaseDomain: "by.example.com", URLMode: "path"}}
	cfg.ApplyDefaults()
	if got := cfg.SiteURL("demo"); got != "https://by.example.com/s/demo/" {
		t.Fatalf("got %q", got)
	}
}

func TestSiteURLUsesBaseDomain(t *testing.T) {
	cases := []struct {
		name, base, public, mode, want string
	}{
		{"same host", "backyard.acme.com", "", "", "https://demo.backyard.acme.com/"},
		{"hub elsewhere", "apps.acme.com", "https://hub.acme.com", "", "https://demo.apps.acme.com/"},
		{"hub is a subdomain", "apps.acme.com", "https://hub.apps.acme.com/", "", "https://demo.apps.acme.com/"},
		{"public port kept", "apps.acme.com", "https://hub.acme.com:8443", "", "https://demo.apps.acme.com:8443/"},
		{"public scheme kept", "apps.acme.com", "http://hub.acme.com", "", "http://demo.apps.acme.com/"},
		{"explicit default port kept", "apps.acme.com", "https://hub.acme.com:443", "", "https://demo.apps.acme.com:443/"},
		{"base normalized", " Apps.ACME.com. ", "https://hub.acme.com", "", "https://demo.apps.acme.com/"},
		{"public path ignored", "apps.acme.com", "https://hub.acme.com/backyard", "", "https://demo.apps.acme.com/"},
		{"dev localhost", "localhost", "http://localhost:8080", "", "http://demo.localhost:8080/"},
		{"path mode uses public url", "apps.acme.com", "https://hub.acme.com", "path", "https://hub.acme.com/s/demo/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Server: ServerConfig{BaseDomain: tc.base, PublicURL: tc.public, URLMode: tc.mode}}
			cfg.ApplyDefaults()
			if got := cfg.SiteURL("demo"); got != tc.want {
				t.Fatalf("SiteURL = %q, want %q", got, tc.want)
			}
		})
	}
	// Without ApplyDefaults (raw values) the base domain still wins.
	raw := &Config{Server: ServerConfig{BaseDomain: "Apps.Acme.com:8443", PublicURL: "https://hub.acme.com:8443", URLMode: "host"}}
	if got := raw.SiteURL("demo"); got != "https://demo.apps.acme.com:8443/" {
		t.Fatalf("raw SiteURL = %q", got)
	}
	// No base domain: fall back to the public URL's host.
	nb := &Config{Server: ServerConfig{PublicURL: "https://hub.acme.com:8443", URLMode: "host"}}
	if got := nb.SiteURL("demo"); got != "https://demo.hub.acme.com:8443/" {
		t.Fatalf("no-base SiteURL = %q", got)
	}
}

func TestCookieDomainCapable(t *testing.T) {
	for h, want := range map[string]bool{
		"backyard.acme.com": true, "lvh.me": true, "Apps.Acme.COM.": true, "acme.com:8443": true,
		"localhost": false, "LOCALHOST": false, "app.localhost": false, "127.0.0.1": false, "10.1.2.3": false,
		"::1": false, "[::1]": false, "[::1]:8080": false, "127.0.0.1:8080": false, "intranet": false, "": false,
	} {
		if got := CookieDomainCapable(h); got != want {
			t.Errorf("CookieDomainCapable(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestValidateOIDCHostModeNeedsCookieDomain(t *testing.T) {
	build := func(mode, base, public, urlMode string, dev bool) *Config {
		cfg := &Config{}
		cfg.Server.BaseDomain, cfg.Server.PublicURL, cfg.Server.URLMode = base, public, urlMode
		cfg.Server.Secret = strings.Repeat("s", 40)
		cfg.Auth.Mode = mode
		cfg.Auth.OIDC.Issuer, cfg.Auth.OIDC.ClientID = "https://idp.example.com", "backyard"
		if dev {
			cfg.ApplyDevDefaults()
		}
		cfg.ApplyDefaults()
		return cfg
	}
	bad := []struct{ name, base, public, want string }{
		{"localhost", "localhost", "", "cannot carry a cookie Domain"},
		{"dot localhost", "by.localhost", "", "cannot carry a cookie Domain"},
		{"ipv4", "127.0.0.1", "", "cannot carry a cookie Domain"},
		{"lan ip", "10.0.0.5", "http://10.0.0.5:8080", "cannot carry a cookie Domain"},
		{"ipv6", "::1", "http://[::1]:8080", "cannot carry a cookie Domain"},
		{"single label", "intranet", "https://intranet", "cannot carry a cookie Domain"},
		{"hub outside base", "apps.acme.com", "https://hub.acme.com", `public_url host "hub.acme.com"`},
		{"hub on an ip", "apps.acme.com", "http://10.0.0.5:8080", `public_url host "10.0.0.5"`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			for _, dev := range []bool{false, true} {
				err := build("okta", tc.base, tc.public, "", dev).Validate()
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("dev=%v: err = %v, want %q", dev, err, tc.want)
				}
				if tc.want == "cannot carry a cookie Domain" &&
					(!strings.Contains(err.Error(), "url_mode: path") || !strings.Contains(err.Error(), "lvh.me")) {
					t.Fatalf("error does not suggest a fix: %v", err)
				}
			}
		})
	}
	good := []struct{ name, mode, base, public, urlMode string }{
		{"real domain", "oidc", "backyard.acme.com", "", ""},
		{"lvh.me", "oidc", "lvh.me", "", ""},
		{"hub is a subdomain", "oidc", "apps.acme.com", "https://hub.apps.acme.com", ""},
		{"path mode on localhost", "oidc", "localhost", "", "path"},
		{"path mode on an ip", "oidc", "127.0.0.1", "http://127.0.0.1:8080", "path"},
		{"proxy mode on localhost", "header", "localhost", "", ""},
		{"none on localhost", "none", "localhost", "", ""},
		{"proxy hub outside base", "iap", "apps.acme.com", "https://hub.acme.com", ""},
	}
	for _, tc := range good {
		t.Run(tc.name, func(t *testing.T) {
			cfg := build(tc.mode, tc.base, tc.public, tc.urlMode, false)
			cfg.Auth.IAP.Audience = "/projects/1/global/backendServices/2"
			cfg.Auth.Header.EmailHeader = "X-Email"
			cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
	// The dev defaults (localhost, auth none, host mode) stay valid.
	t.Setenv("BACKYARD_CONFIG_YAML", "")
	t.Setenv("BACKYARD_CONFIG", "")
	t.Chdir(t.TempDir())
	if _, err := Load("", true); err != nil {
		t.Fatalf("dev load: %v", err)
	}
}
