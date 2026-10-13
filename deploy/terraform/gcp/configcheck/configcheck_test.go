package configcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
)

const (
	// Values of the Secret Manager-backed environment variables Cloud Run
	// sets next to BACKYARD_CONFIG_YAML.
	testSecret     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testDSN        = "host=/cloudsql/acme-backyard:us-central1:backyard-pg-abc123 dbname=backyard user=backyard password=pw sslmode=disable application_name=backyard"
	testOIDCSecret = "oidc-secret-from-env"
	testAnthropic  = "sk-ant-from-env"
	testOpenAI     = "sk-openai-from-env"
	testDirectory  = "/etc/backyard/people.csv"

	// cloudRunBodyLimit mirrors local.cloud_run_body_limit in main.tf.
	cloudRunBodyLimit = 31 << 20

	testsDir = "configcheck"
)

// TestRenderedConfigLoads renders the scenarios of testdata/render.tftest.hcl
// and checks that Backyard accepts each configuration as-is.
func TestRenderedConfigLoads(t *testing.T) {
	tf := terraformBinary(t)
	moduleDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	work := copyModule(t, moduleDir)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	env := terraformEnv(moduleDir)
	if out, err := run(ctx, work, env, tf, "init", "-backend=false", "-input=false", "-no-color"); err != nil {
		// Almost always the provider registry being unreachable.
		t.Skipf("terraform init failed (providers unavailable?): %v\n%s", err, tail(out, 2000))
	}

	yamls := renderScenarios(ctx, t, work, env, tf)

	checks := map[string]func(t *testing.T, c *config.Config){
		"iap_vertex":      checkIAPVertex,
		"oidc_full":       checkOIDCFull,
		"none_path":       checkNonePath,
		"warehouse_extra": checkWarehouseExtra,
	}
	for name := range checks {
		if _, ok := yamls[name]; !ok {
			t.Errorf("run %q produced no backyard_config_yaml output", name)
		}
	}
	for name, raw := range yamls {
		check, ok := checks[name]
		if !ok {
			t.Errorf("no expectations for run %q", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			cfg := load(t, raw)
			checkCommon(t, cfg)
			check(t, cfg)
		})
	}
}

// load reads raw with config.Load in a Cloud Run-like environment.
func load(t *testing.T, raw string) *config.Config {
	t.Helper()
	for k, v := range map[string]string{
		"BACKYARD_CONFIG_YAML": "", // Load(path) reads the file, but keep the env clean
		"BACKYARD_CONFIG":      "",
		"BACKYARD_BASE_DOMAIN": "",
		"BACKYARD_PUBLIC_URL":  "",
		"BACKYARD_SECRET":      testSecret,
		"DATABASE_URL":         testDSN,
		"OIDC_CLIENT_SECRET":   testOIDCSecret,
		"ANTHROPIC_API_KEY":    testAnthropic,
		"OPENAI_API_KEY":       testOpenAI,
		"DIRECTORY_PATH":       testDirectory,
		"PORT":                 "8080",
	} {
		t.Setenv(k, v)
	}
	path := filepath.Join(t.TempDir(), "backyard.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, false)
	if err != nil {
		t.Fatalf("config.Load rejected the rendered config: %v\n--- backyard.yaml ---\n%s", err, raw)
	}
	return cfg
}

func checkCommon(t *testing.T, c *config.Config) {
	t.Helper()
	eq(t, "server.base_domain", c.Server.BaseDomain, "backyard.example.com")
	eq(t, "server.public_url", c.Server.PublicURL, "https://backyard.example.com")
	eq(t, "server.listen", c.Server.Listen, ":8080")
	eq(t, "server.secret", c.Server.Secret, testSecret)
	eq(t, "server.shutdown_timeout", c.Server.ShutdownTimeout.D(), 8*time.Second)
	eq(t, "storage.type", c.Storage.Type, "gcs")
	eq(t, "storage.gcs.bucket", c.Storage.GCS.Bucket, "acme-backyard-backyard-sites")
	eq(t, "storage.gcs.signer_email", c.Storage.GCS.SignerEmail, "backyard-runtime@acme-backyard.iam.gserviceaccount.com")
	eq(t, "database.type", c.Database.Type, "postgres")
	eq(t, "database.postgres.dsn", c.Database.Postgres.DSN, testDSN)
	eq(t, "database.postgres.max_conns", c.Database.Postgres.MaxConns, 4)
	eq(t, "pubsub.type", c.PubSub.Type, "postgres")
	eq(t, "pubsub.postgres.dsn", c.PubSub.Postgres.DSN, testDSN)
	eq(t, "files.max_upload_bytes", c.Files.MaxUploadBytes, int64(cloudRunBodyLimit))
	eq(t, "telemetry.log_format", c.Telemetry.LogFormat, "json")
	if c.Deploy.AllowAnonymous {
		t.Error("deploy.allow_anonymous must stay off")
	}
}

func checkIAPVertex(t *testing.T, c *config.Config) {
	eq(t, "server.name", c.Server.Name, "Backyard")
	eq(t, "server.url_mode", c.Server.URLMode, "host")
	eq(t, "auth.mode", c.Auth.Mode, "iap")
	eq(t, "auth.iap.audience", c.Auth.IAP.Audience, "/projects/123456789012/global/backendServices/4242424242")
	eq(t, "sites.max_file_bytes", c.Sites.MaxFileBytes, int64(cloudRunBodyLimit))
	eq(t, "deploy.methods", strings.Join(c.Deploy.Methods, ","), "api")
	eq(t, "deploy.expose_storage", *c.Deploy.ExposeStorage, false)
	if c.AI.Enabled == nil || !*c.AI.Enabled {
		t.Fatal("ai must be enabled with Vertex AI")
	}
	eq(t, "ai.default_model", c.AI.DefaultModel, "smart")
	eq(t, "ai.default_image_model", c.AI.DefaultImageModel, "image")
	eq(t, "ai.default_embedding_model", c.AI.DefaultEmbeddingModel, "embed")
	claude := c.AI.Providers["claude"]
	eq(t, "claude.type", claude.Type, "anthropic")
	eq(t, "claude.platform", claude.Platform, "vertex")
	eq(t, "claude.project_id", claude.ProjectID, "acme-backyard")
	eq(t, "claude.location", claude.Location, "global")
	vertex := c.AI.Providers["vertex"]
	eq(t, "vertex.type", vertex.Type, "gemini")
	eq(t, "vertex.project_id", vertex.ProjectID, "acme-backyard")
	eq(t, "vertex.api_key", vertex.APIKey, "")
	eq(t, "models.smart", c.AI.Models["smart"].Model, "claude-opus-5-5")
	eq(t, "models.image.kind", c.AI.Models["image"].Kind, "image")
	eq(t, "models.embed.kind", c.AI.Models["embed"].Kind, "embedding")
	if len(c.Warehouse.Sources) != 0 {
		t.Errorf("warehouse sources = %v, want none", c.Warehouse.Sources)
	}
}

func checkOIDCFull(t *testing.T, c *config.Config) {
	// The literal "${HOME}" survives Backyard's environment expansion.
	eq(t, "server.name", c.Server.Name, `Acme's "Yard" ${HOME}: #1`)
	eq(t, "server.trusted_proxies", strings.Join(c.Server.TrustedProxies, ","), "10.0.0.0/8")
	eq(t, "auth.mode", c.Auth.Mode, "oidc")
	eq(t, "auth.oidc.issuer", c.Auth.OIDC.Issuer, "https://acme.okta.com/oauth2/default")
	eq(t, "auth.oidc.client_id", c.Auth.OIDC.ClientID, "0oa123")
	eq(t, "auth.oidc.client_secret", c.Auth.OIDC.ClientSecret, testOIDCSecret)
	eq(t, "auth.oidc.scopes", strings.Join(c.Auth.OIDC.Scopes, " "), "openid profile email groups")
	eq(t, "auth.oidc.use_userinfo", c.Auth.OIDC.UseUserInfo, true)
	eq(t, "auth.oidc.device_client_id", c.Auth.OIDC.DeviceClientID, "0oa456")
	eq(t, "auth.oidc.api_audiences", strings.Join(c.Auth.OIDC.APIAudiences, ","), "api://backyard")
	eq(t, "auth.oidc.redirect_url", c.Auth.OIDC.RedirectURL, "https://backyard.example.com/__backyard/auth/callback")
	eq(t, "auth.admins", strings.Join(c.Auth.Admins, ","), "alice@example.com")
	eq(t, "auth.admin_groups", strings.Join(c.Auth.AdminGroups, ","), "backyard-admins")
	eq(t, "auth.allowed_domains", strings.Join(c.Auth.AllowedDomains, ","), "example.com")
	eq(t, "auth.allowed_groups", strings.Join(c.Auth.AllowedGroups, ","), "eng")
	if len(c.Auth.Tokens) != 1 || c.Auth.Tokens[0].Name != "ci" || !c.Auth.Tokens[0].Admin {
		t.Errorf("auth.tokens = %+v, want one admin token named ci", c.Auth.Tokens)
	}
	eq(t, "auth.iap.audience", c.Auth.IAP.Audience, "")
	eq(t, "storage.gcs.prefix", c.Storage.GCS.Prefix, "backyard/")
	eq(t, "telemetry.log_level", c.Telemetry.LogLevel, "debug")
	eq(t, "telemetry.access_log", c.Telemetry.AccessLog, true)

	eq(t, "ai.default_model", c.AI.DefaultModel, "deep")
	anthropic := c.AI.Providers["anthropic"]
	eq(t, "anthropic.type", anthropic.Type, "anthropic")
	eq(t, "anthropic.platform", anthropic.Platform, "anthropic")
	eq(t, "anthropic.api_key", anthropic.APIKey, testAnthropic)
	openai := c.AI.Providers["openai"]
	eq(t, "openai.type", openai.Type, "openai")
	eq(t, "openai.api_key", openai.APIKey, testOpenAI)
	eq(t, "openai.base_url", openai.BaseURL, "https://llm.internal.example.com/v1")
	if _, ok := c.AI.Providers["vertex"]; ok {
		t.Error("vertex provider configured with vertex_ai = false")
	}
	deep := c.AI.Models["deep"]
	eq(t, "models.deep.effort", deep.Effort, "high")
	eq(t, "models.deep.max_tokens", deep.MaxTokens, 32000)
	eq(t, "models.gpt.description", c.AI.Models["gpt"].Description, "GPT: ${not_expanded}")
	eq(t, "models.smart.model", c.AI.Models["smart"].Model, "claude-opus-5-5")
	eq(t, "models.fast.model", c.AI.Models["fast"].Model, "claude-haiku-5-5")
}

func checkNonePath(t *testing.T, c *config.Config) {
	eq(t, "auth.mode", c.Auth.Mode, "none")
	eq(t, "server.url_mode", c.Server.URLMode, "path")
	if c.AI.Enabled == nil || *c.AI.Enabled {
		t.Error("ai must be disabled without providers")
	}
	if len(c.AI.Providers) != 0 || len(c.AI.Models) != 0 {
		t.Errorf("ai providers/models = %v/%v, want none", c.AI.Providers, c.AI.Models)
	}
}

func checkWarehouseExtra(t *testing.T, c *config.Config) {
	eq(t, "deploy.methods", strings.Join(c.Deploy.Methods, ","), "api,gcs")
	eq(t, "deploy.expose_storage", *c.Deploy.ExposeStorage, true)
	eq(t, "warehouse.default_source", c.Warehouse.DefaultSource, "bigquery")
	eq(t, "warehouse.allow_raw_sql", c.Warehouse.AllowRawSQL, true)
	src := c.Warehouse.Sources["bigquery"]
	eq(t, "bigquery.type", src.Type, "bigquery")
	eq(t, "bigquery.project_id", src.ProjectID, "acme-analytics")
	eq(t, "bigquery.location", src.Location, "US")
	eq(t, "bigquery.max_bytes_billed", src.MaxBytesBilled, int64(10<<30))
	q := c.Warehouse.Queries["weekly_active_users"]
	eq(t, "query.sql", q.SQL, "SELECT day, users, '${literal}' AS tag FROM events.wau WHERE team = @team")
	eq(t, "query.source", q.Source, "bigquery")
	eq(t, "query.params", strings.Join(q.Params, ","), "team")
	// extra_config_yaml: appended verbatim, its references expanded.
	eq(t, "identity.directory.path", c.Identity.Directory.Path, testDirectory)
	eq(t, "sites.ownership", c.Sites.Ownership, "owner")
	eq(t, "sites.max_file_bytes", c.Sites.MaxFileBytes, int64(10<<20))
	eq(t, "ratelimit.ai_per_minute", c.RateLimit.AIPerMinute, 5)
}

// renderScenarios runs the scenarios and returns each run's rendered YAML.
func renderScenarios(ctx context.Context, t *testing.T, work string, env []string, tf string) map[string]string {
	t.Helper()
	cmd := exec.CommandContext(ctx, tf, "test", "-test-directory="+testsDir, "-json", "-verbose", "-no-color")
	cmd.Dir = work
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("terraform test: %v", err)
	}

	// Each line is one JSON message; verbose state messages embed the
	// provider schemas and can be tens of megabytes, so decode as a stream.
	type message struct {
		Type    string `json:"type"`
		Message string `json:"@message"`
		Run     string `json:"@testrun"`
		TestRun *struct {
			Status string `json:"status"`
		} `json:"test_run"`
		TestState *struct {
			Outputs map[string]struct {
				Value json.RawMessage `json:"value"`
			} `json:"outputs"`
		} `json:"test_state"`
		Diagnostic *struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
		} `json:"diagnostic"`
	}
	yamls := map[string]string{}
	dec := json.NewDecoder(stdout)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Errorf("decode terraform test output: %v", err)
			}
			break
		}
		switch {
		case m.Diagnostic != nil && m.Diagnostic.Severity == "error":
			t.Errorf("terraform test %s: %s: %s", m.Run, m.Diagnostic.Summary, m.Diagnostic.Detail)
		case m.TestRun != nil && (m.TestRun.Status == "fail" || m.TestRun.Status == "error"):
			t.Errorf("terraform test run %q: %s", m.Run, m.TestRun.Status)
		case m.TestState != nil:
			out, ok := m.TestState.Outputs["backyard_config_yaml"]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(out.Value, &s); err != nil {
				t.Errorf("run %q: backyard_config_yaml is not a string: %v", m.Run, err)
				continue
			}
			yamls[m.Run] = s
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("terraform test: %v\n%s", err, tail(stderr.String(), 4000))
	}
	return yamls
}

// terraformBinary returns the terraform to use or skips the test.
func terraformBinary(t *testing.T) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv("BACKYARD_TEST_TERRAFORM"))
	switch strings.ToLower(v) {
	case "", "0", "false", "no":
		t.Skip("set BACKYARD_TEST_TERRAFORM=1 (or the path of a terraform binary) to render the GCP module's config")
	case "1", "true", "yes":
		v = "terraform"
	}
	p, err := exec.LookPath(v)
	if err != nil {
		t.Skipf("terraform not found: %v", err)
	}
	return p
}

// terraformEnv is the environment for terraform commands: non-interactive,
// and reusing the module's own provider installation as a plugin cache when
// no cache is configured (its layout matches TF_PLUGIN_CACHE_DIR's).
func terraformEnv(moduleDir string) []string {
	env := append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
	if os.Getenv("TF_PLUGIN_CACHE_DIR") == "" {
		cache := filepath.Join(moduleDir, ".terraform", "providers")
		if fi, err := os.Stat(cache); err == nil && fi.IsDir() {
			env = append(env, "TF_PLUGIN_CACHE_DIR="+cache)
		}
	}
	return env
}

// copyModule copies the module's configuration (not its .terraform state)
// and the scenarios into a temporary directory.
func copyModule(t *testing.T, moduleDir string) string {
	t.Helper()
	work := t.TempDir()
	var files []string
	for _, pattern := range []string{"*.tf", ".terraform.lock.hcl", "templates/*"} {
		m, err := filepath.Glob(filepath.Join(moduleDir, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if !slices.ContainsFunc(files, func(f string) bool { return filepath.Base(f) == "main.tf" }) {
		t.Fatalf("no Terraform module in %s", moduleDir)
	}
	for _, src := range files {
		rel, err := filepath.Rel(moduleDir, src)
		if err != nil {
			t.Fatal(err)
		}
		copyFile(t, src, filepath.Join(work, rel))
	}
	copyFile(t, filepath.Join("testdata", "render.tftest.hcl"), filepath.Join(work, testsDir, "render.tftest.hcl"))
	return work
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
