package configcheck

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/scripts/tfrender"
)

const (
	// Values of the Secret Manager-backed environment variables Cloud Run
	// sets next to BACKYARD_CONFIG_YAML (for the scenarios with detailed
	// expectations).
	testSecret     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testDSN        = "host=/cloudsql/acme-backyard:us-central1:backyard-pg-abc123 dbname=backyard user=backyard password=pw sslmode=disable application_name=backyard"
	testOIDCSecret = "oidc-secret-from-env"
	testAnthropic  = "sk-ant-from-env"
	testOpenAI     = "sk-openai-from-env"
	testDirectory  = "/etc/backyard/people.csv"

	// cloudRunBodyLimit mirrors local.cloud_run_body_limit in main.tf.
	cloudRunBodyLimit = 31 << 20
)

// TestRenderedConfigLoads renders the scenarios of testdata/render.tftest.hcl
// next to the module's own tests/module.tftest.hcl and checks that Backyard
// accepts each configuration as-is: the scenarios with detailed expectations,
// every module test run with the environment its Cloud Run container gets.
func TestRenderedConfigLoads(t *testing.T) {
	tf := tfrender.Terraform(t)
	runs := tfrender.Render(t, tf, tfrender.Options{
		Module: "..",
		Extra:  map[string]string{"tests/" + scenariosFile: filepath.Join("testdata", "render.tftest.hcl")},
	})

	checks := map[string]func(t *testing.T, c *config.Config){
		"iap_vertex":      checkIAPVertex,
		"oidc_full":       checkOIDCFull,
		"none_path":       checkNonePath,
		"warehouse_extra": checkWarehouseExtra,
	}
	names := make([]string, 0, len(runs))
	for name := range runs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		run := runs[name]
		if filepath.Base(run.File) != scenariosFile {
			t.Run("module/"+name, func(t *testing.T) { checkCloudRun(t, run) })
			continue
		}
		check, ok := checks[name]
		if !ok {
			t.Errorf("no expectations for scenario %q", name)
			continue
		}
		raw, ok := run.Output("backyard_config_yaml")
		if !ok {
			t.Errorf("scenario %q produced no backyard_config_yaml output", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			cfg := load(t, raw)
			checkCommon(t, cfg)
			check(t, cfg)
		})
	}
	for name := range checks {
		if run := runs[name]; run == nil || filepath.Base(run.File) != scenariosFile {
			t.Errorf("scenario %q did not run", name)
		}
	}
}

// scenariosFile is the name testdata/render.tftest.hcl gets in the copy.
const scenariosFile = "configcheck.tftest.hcl"

// checkCloudRun loads BACKYARD_CONFIG_YAML of the Cloud Run container with
// its plain and Secret Manager-backed environment variables.
func checkCloudRun(t *testing.T, run *tfrender.Run) {
	svc := run.Resource("google_cloud_run_v2_service.backyard")
	if svc == nil {
		t.Fatal("no google_cloud_run_v2_service.backyard in the state")
	}
	vars, _ := tfrender.Get(svc, "template", 0, "containers", 0, "env").([]any)
	env := map[string]string{"PORT": "8080"} // set by Cloud Run
	for _, v := range vars {
		name, _ := tfrender.Get(v, "name").(string)
		value, _ := tfrender.Get(v, "value").(string)
		if tfrender.Get(v, "value_source", 0, "secret_key_ref", 0) != nil {
			value = tfrender.Placeholder(name)
		}
		env[name] = value
	}
	raw := env["BACKYARD_CONFIG_YAML"]
	delete(env, "BACKYARD_CONFIG_YAML")
	if raw == "" {
		t.Fatal("the container has no BACKYARD_CONFIG_YAML")
	}
	if out, _ := run.Output("backyard_config_yaml"); out != raw {
		t.Error("output backyard_config_yaml differs from the container's BACKYARD_CONFIG_YAML")
	}
	if missing := tfrender.Missing(raw, env); len(missing) > 0 {
		t.Errorf("backyard.yaml references %v, which the container does not set", missing)
	}
	cfg, err := tfrender.Load(t, raw, env)
	if err != nil {
		t.Fatalf("config.Load rejected the Cloud Run config: %v\n--- backyard.yaml ---\n%s", err, raw)
	}
	if cfg.Server.Secret != tfrender.PlaceholderSecret {
		t.Error("server.secret does not come from the BACKYARD_SECRET secret")
	}
	if bucket, _ := run.Output("bucket"); cfg.Storage.Type != "gcs" || cfg.Storage.GCS.Bucket != bucket {
		t.Errorf("storage = %s/%s, want gcs/%s", cfg.Storage.Type, cfg.Storage.GCS.Bucket, bucket)
	}
}

// load reads raw with config.Load in a Cloud Run-like environment.
func load(t *testing.T, raw string) *config.Config {
	t.Helper()
	cfg, err := tfrender.Load(t, raw, map[string]string{
		"BACKYARD_SECRET":    testSecret,
		"DATABASE_URL":       testDSN,
		"OIDC_CLIENT_SECRET": testOIDCSecret,
		"ANTHROPIC_API_KEY":  testAnthropic,
		"OPENAI_API_KEY":     testOpenAI,
		"DIRECTORY_PATH":     testDirectory,
		"PORT":               "8080",
	})
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
	eq(t, "auth.allowed_emails", strings.Join(c.Auth.AllowedEmails, ","), "pat@contractor.example")
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

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}
