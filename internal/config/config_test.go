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

func TestValidateAllowedEmails(t *testing.T) {
	cfg, err := Parse([]byte("auth:\n  allowed_emails: [ana@gmail.com, \" Sam@Example.com \", gmail.com, \"Ana <ana@gmail.com>\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	err = cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	msg := err.Error()
	for _, want := range []string{`auth.allowed_emails[2]: "gmail.com"`, `auth.allowed_emails[3]: "Ana <ana@gmail.com>"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	for _, unwanted := range []string{"auth.allowed_emails[0]", "auth.allowed_emails[1]"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("valid address rejected (%s): %v", unwanted, err)
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

// mcpConfig parses yaml (plus a valid server section) and applies defaults.
func mcpConfig(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := Parse([]byte("server:\n  base_domain: backyard.acme.com\n  secret: " + strings.Repeat("s", 40) + "\n" + yaml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.ApplyDefaults()
	return cfg
}

func TestMCPDefaults(t *testing.T) {
	cfg := mcpConfig(t, "")
	m := cfg.MCP
	if m.Enabled || m.MaxPublishBytes != 4194304 {
		t.Fatalf("defaults: %+v", m)
	}
	if strings.Join(m.Clients, " ") != "https://claude.ai/oauth/mcp-oauth-client-metadata https://claude.ai/oauth/claude-code-client-metadata" {
		t.Fatalf("clients = %q", m.Clients)
	}
	if strings.Join(m.RedirectURIs, " ") != "https://claude.ai/api/mcp/auth_callback https://claude.com/api/mcp/auth_callback" {
		t.Fatalf("redirect_uris = %q", m.RedirectURIs)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	// The defaults are copies: changing them does not touch the package lists.
	m.Clients[0] = "x"
	if DefaultMCPClients[0] == "x" {
		t.Fatal("ApplyDefaults shares DefaultMCPClients")
	}

	// An explicit empty list stays empty (CIMD off / loopback only); a null
	// value selects the default.
	cfg = mcpConfig(t, "mcp:\n  enabled: true\n  clients: []\n  redirect_uris: []\n")
	if cfg.MCP.Clients == nil || len(cfg.MCP.Clients) != 0 || cfg.MCP.RedirectURIs == nil || len(cfg.MCP.RedirectURIs) != 0 {
		t.Fatalf("explicit []: clients=%#v redirect_uris=%#v", cfg.MCP.Clients, cfg.MCP.RedirectURIs)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty lists: %v", err)
	}
	cfg = mcpConfig(t, "mcp:\n  clients:\n  redirect_uris:\n")
	if len(cfg.MCP.Clients) != 2 || len(cfg.MCP.RedirectURIs) != 2 {
		t.Fatalf("null lists: %q %q", cfg.MCP.Clients, cfg.MCP.RedirectURIs)
	}

	// Entries are trimmed but their case is kept (comparison is exact).
	cfg = mcpConfig(t, "mcp:\n  clients: [\" https://Apps.Example.com/Client \"]\n  redirect_uris: [\" https://Apps.Example.com/CB \"]\n  max_publish_bytes: 65536\n")
	if cfg.MCP.Clients[0] != "https://Apps.Example.com/Client" || cfg.MCP.RedirectURIs[0] != "https://Apps.Example.com/CB" {
		t.Fatalf("entries: %q %q", cfg.MCP.Clients, cfg.MCP.RedirectURIs)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("trimmed entries: %v", err)
	}
}

func TestMCPUnknownKeyRejected(t *testing.T) {
	if _, err := Parse([]byte("mcp:\n  foo: 1\n")); err == nil || !strings.Contains(err.Error(), "foo") {
		t.Fatalf("mcp.foo: err = %v", err)
	}
}

func TestMCPValidation(t *testing.T) {
	const (
		rule1 = "is not a client metadata URL (an https URL with a path, without query, fragment or user info, at most 512 bytes)"
		rule2 = "is not an https redirect URI without fragment or user info (at most 512 bytes; loopback redirect URIs are always allowed and are not listed)"
		rule3 = "points at this Backyard or one of its sites"
		rule4 = "mcp.max_publish_bytes must be between 65536 and 67108864 and at most sites.max_site_bytes"
		rule5 = "mcp.enabled requires server.public_url to be https (http only on localhost, *.localhost or loopback addresses) without a path, query or fragment"
		rule6 = "auth.oidc.redirect_url must not be /__backyard/auth/authorize while mcp.enabled is true"
	)
	long := "https://apps.example.com/" + strings.Repeat("a", 512-len("https://apps.example.com/")+1)
	cases := []struct {
		name, yaml string
		want       []string // substrings of the joined error ("" = must validate)
	}{
		{"client without path", "mcp:\n  clients: [https://claude.ai]\n", []string{`mcp.clients: "https://claude.ai" ` + rule1}},
		{"client root path", "mcp:\n  clients: [https://claude.ai/]\n", []string{`mcp.clients: "https://claude.ai/" ` + rule1}},
		{"client http", "mcp:\n  clients: [http://claude.ai/x]\n", []string{`mcp.clients: "http://claude.ai/x" ` + rule1}},
		{"client ip", "mcp:\n  clients: [https://203.0.113.5/x]\n", []string{`mcp.clients: "https://203.0.113.5/x" ` + rule1}},
		{"client ipv6", "mcp:\n  clients: [\"https://[2001:db8::1]/x\"]\n", []string{rule1}},
		{"client query", "mcp:\n  clients: [\"https://claude.ai/x?a=1\"]\n", []string{rule1}},
		{"client empty query", "mcp:\n  clients: [\"https://claude.ai/x?\"]\n", []string{rule1}},
		{"client fragment", "mcp:\n  clients: [\"https://claude.ai/x#f\"]\n", []string{rule1}},
		{"client userinfo", "mcp:\n  clients: [\"https://u@claude.ai/x\"]\n", []string{rule1}},
		{"client dot segment", "mcp:\n  clients: [https://claude.ai/a/../x]\n", []string{rule1}},
		{"client too long", "mcp:\n  clients: [" + long + "]\n", []string{rule1}},
		{"client empty", "mcp:\n  clients: [\"\"]\n", []string{rule1}},
		{"redirect http loopback", "mcp:\n  redirect_uris: [http://localhost/x]\n", []string{`mcp.redirect_uris: "http://localhost/x" ` + rule2}},
		{"redirect http", "mcp:\n  redirect_uris: [http://claude.ai/cb]\n", []string{rule2}},
		{"redirect fragment", "mcp:\n  redirect_uris: [\"https://claude.ai/cb#x\"]\n", []string{rule2}},
		{"redirect userinfo", "mcp:\n  redirect_uris: [\"https://u:p@claude.ai/cb\"]\n", []string{rule2}},
		{"redirect too long", "mcp:\n  redirect_uris: [" + long + "]\n", []string{rule2}},
		{"redirect custom scheme", "mcp:\n  redirect_uris: [\"cursor://cb\"]\n", []string{rule2}},
		{"redirect hub", "mcp:\n  redirect_uris: [https://backyard.acme.com/cb]\n", []string{`mcp.redirect_uris: "https://backyard.acme.com/cb" ` + rule3}},
		{"redirect site", "mcp:\n  redirect_uris: [https://evil.backyard.acme.com/cb]\n", []string{`mcp.redirect_uris: "https://evil.backyard.acme.com/cb" ` + rule3}},
		{"redirect site uppercase", "mcp:\n  redirect_uris: [https://Evil.Backyard.Acme.com/cb]\n", []string{rule3}},
		{"redirect other domain", "mcp:\n  redirect_uris: [https://notbackyard.acme.com/cb, https://claude.ai/api/mcp/auth_callback/x]\n", nil},
		{"publish too small", "mcp:\n  max_publish_bytes: 65535\n", []string{rule4}},
		{"publish too big", "mcp:\n  max_publish_bytes: 67108865\n", []string{rule4}},
		{"publish over site", "sites:\n  max_site_bytes: 100000\nmcp:\n  enabled: true\n  max_publish_bytes: 100001\n", []string{rule4}},
		{"default publish over a small site, enabled", "sites:\n  max_site_bytes: 1048576\nmcp:\n  enabled: true\n", []string{rule4}},
		// Upgrades: a small sites.max_site_bytes stays valid while the connector is off.
		{"default publish over a small site, disabled", "sites:\n  max_site_bytes: 1048576\n", nil},
		{"publish bounds", "mcp:\n  max_publish_bytes: 67108864\n", nil},
		{"enabled https", "mcp:\n  enabled: true\n", nil},
		{"enabled with path", "  public_url: https://backyard.acme.com/hub\nmcp:\n  enabled: true\n", []string{rule5}},
		{"enabled with query", "  public_url: \"https://backyard.acme.com?x=1\"\nmcp:\n  enabled: true\n", []string{rule5}},
		{"enabled http", "  public_url: http://backyard.acme.com\nmcp:\n  enabled: true\n", []string{rule5}},
		{"enabled http loopback ip", "  public_url: http://127.0.0.1:8080\nmcp:\n  enabled: true\n", nil},
		{"enabled http localhost", "  public_url: http://localhost:8080\nmcp:\n  enabled: true\n", nil},
		{"enabled http dot localhost", "  public_url: http://hub.localhost:8080\nmcp:\n  enabled: true\n", nil},
		{"enabled http ipv6 loopback", "  public_url: \"http://[::1]:8080\"\nmcp:\n  enabled: true\n", nil},
		{"enabled http lan", "  public_url: http://10.0.0.5:8080\nmcp:\n  enabled: true\n", []string{rule5}},
		{"disabled http", "  public_url: http://backyard.acme.com/hub\n", nil},
		{"oidc authorize callback", "auth:\n  mode: oidc\n  oidc: {issuer: https://idp.example.com, client_id: x, redirect_url: https://backyard.acme.com/__backyard/auth/authorize}\nmcp:\n  enabled: true\n", []string{rule6}},
		{"oidc authorize callback disabled", "auth:\n  mode: oidc\n  oidc: {issuer: https://idp.example.com, client_id: x, redirect_url: https://backyard.acme.com/__backyard/auth/authorize}\n", nil},
		{"oidc normal callback", "auth:\n  mode: oidc\n  oidc: {issuer: https://idp.example.com, client_id: x}\nmcp:\n  enabled: true\n", nil},
		{"all problems together", "mcp:\n  enabled: true\n  clients: [https://claude.ai]\n  redirect_uris: [http://localhost/x, https://backyard.acme.com/cb]\n  max_publish_bytes: 1\n",
			[]string{rule1, rule2, rule3, rule4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := tc.yaml
			if strings.HasPrefix(y, "  public_url:") {
				// A server key: splice it into the server section.
				line, rest, _ := strings.Cut(y, "\n")
				cfg, err := Parse([]byte("server:\n  base_domain: backyard.acme.com\n  secret: " + strings.Repeat("s", 40) + "\n" + line + "\n" + rest))
				if err != nil {
					t.Fatal(err)
				}
				cfg.ApplyDefaults()
				check(t, cfg.Validate(), tc.want)
				return
			}
			check(t, mcpConfig(t, y).Validate(), tc.want)
		})
	}
}

func check(t *testing.T, err error, want []string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("no error, want %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not contain %q", err, w)
		}
	}
}

func TestExampleMCPSection(t *testing.T) {
	cfg, err := Parse([]byte(Example))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCP.Enabled || cfg.MCP.Clients != nil || cfg.MCP.RedirectURIs != nil {
		t.Fatalf("example mcp section: %+v", cfg.MCP)
	}
	if !strings.Contains(Example, "\nmcp:\n") || !strings.Contains(Example, "# max_publish_bytes: 4194304") {
		t.Fatal("example.yaml lacks the mcp section")
	}
}

func TestLoopbackHost(t *testing.T) {
	for h, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "hub.localhost": true, "127.0.0.1": true, "127.9.8.7": true,
		"::1": true, "[::1]": true, "localhost.": true,
		"localhost.example.com": false, "10.0.0.1": false, "backyard.acme.com": false, "": false, "::": false,
	} {
		if got := LoopbackHost(h); got != want {
			t.Errorf("LoopbackHost(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestSitesListingDefaults(t *testing.T) {
	cases := []struct {
		name, yaml         string
		listing, ownership string
	}{
		{"unset", "", "all", "none"},
		{"all", "sites:\n  listing: all\n", "all", "none"},
		{"all with owner", "sites:\n  listing: all\n  ownership: owner\n", "all", "owner"},
		{"owner without listing", "sites:\n  ownership: owner\n", "all", "owner"},
		// Owners-only listing implies ownership.
		{"owners", "sites:\n  listing: owners\n", "owners", "owner"},
		{"owners with owner", "sites:\n  listing: owners\n  ownership: owner\n", "owners", "owner"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mcpConfig(t, tc.yaml)
			if cfg.Sites.Listing != tc.listing || cfg.Sites.Ownership != tc.ownership {
				t.Fatalf("listing=%q ownership=%q, want %q %q", cfg.Sites.Listing, cfg.Sites.Ownership, tc.listing, tc.ownership)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}
			// ApplyDefaults is idempotent.
			cfg.ApplyDefaults()
			if cfg.Sites.Listing != tc.listing || cfg.Sites.Ownership != tc.ownership {
				t.Fatalf("second ApplyDefaults: listing=%q ownership=%q", cfg.Sites.Listing, cfg.Sites.Ownership)
			}
		})
	}
}

func TestSitesListingValidation(t *testing.T) {
	const (
		badListing = "sites.listing must be all or owners"
		needsOwner = "sites.listing: owners requires sites.ownership: owner (its default with this option)"
	)
	cases := []struct {
		name, yaml string
		want       []string // substrings of the joined error
		unwanted   []string
	}{
		{"explicit ownership none", "sites:\n  listing: owners\n  ownership: none\n", []string{needsOwner}, []string{badListing}},
		{"unknown value", "sites:\n  listing: mine\n", []string{badListing}, []string{needsOwner}},
		{"case matters", "sites:\n  listing: Owners\n", []string{badListing}, []string{needsOwner}},
		{"unknown value with ownership none", "sites:\n  listing: private\n  ownership: none\n", []string{badListing}, []string{needsOwner}},
		{"bad ownership", "sites:\n  listing: owners\n  ownership: owners\n", []string{"sites.ownership must be none or owner"}, []string{badListing}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := mcpConfig(t, tc.yaml).Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			for _, w := range tc.unwanted {
				if strings.Contains(err.Error(), w) {
					t.Errorf("error %q contains %q", err, w)
				}
			}
		})
	}
}

func TestExampleSitesListing(t *testing.T) {
	if !strings.Contains(Example, "\n  # listing: owners\n") {
		t.Fatal("example.yaml lacks the commented sites.listing line")
	}
	cfg, err := Parse([]byte(Example))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sites.Listing != "" {
		t.Fatalf("example sets sites.listing = %q", cfg.Sites.Listing)
	}
}
