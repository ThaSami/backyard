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
