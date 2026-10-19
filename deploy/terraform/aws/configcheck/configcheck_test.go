package configcheck

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/scripts/tfrender"
)

// ownersListing names the runs of tests/module.tftest.hcl that turn on the
// owners-only site listing (sites.listing: owners through config_overrides);
// every other run keeps the default.
var ownersListing = map[string]bool{"owners_only_site_listing": true}

// TestRenderedConfigsLoad renders the module's test scenarios and loads the
// ECS service's and the Lambda gateway's configuration of each one.
func TestRenderedConfigsLoad(t *testing.T) {
	tf := tfrender.Terraform(t)
	runs := tfrender.Render(t, tf, tfrender.Options{Module: ".."})

	names := make([]string, 0, len(runs))
	for name := range runs {
		names = append(names, name)
	}
	sort.Strings(names)
	gateways := 0
	for _, name := range names {
		run := runs[name]
		t.Run(name, func(t *testing.T) {
			checkListing(t, "ECS service", ownersListing[name], checkService(t, run))
			if fn := run.Resource("aws_lambda_function.gateway[0]"); fn != nil {
				gateways++
				checkListing(t, "Lambda gateway", ownersListing[name], checkGateway(t, run, fn))
			}
		})
	}
	if gateways == 0 {
		t.Error("no scenario created the Lambda deploy gateway: its configuration went unchecked")
	}
	for name := range ownersListing {
		if _, ok := runs[name]; !ok {
			t.Errorf("tests/module.tftest.hcl has no run %q: the owners-only site listing went unchecked", name)
		}
	}
}

// checkListing checks sites.listing as loaded, and that owners implies
// ownership (the hub and the gateway list the same sites).
func checkListing(t *testing.T, what string, owners bool, cfg *config.Config) {
	t.Helper()
	switch {
	case owners && (cfg.Sites.Listing != "owners" || cfg.Sites.Ownership != "owner"):
		t.Errorf("%s: sites.listing/ownership = %q/%q, want owners/owner", what, cfg.Sites.Listing, cfg.Sites.Ownership)
	case !owners && cfg.Sites.Listing != "all":
		t.Errorf("%s: sites.listing = %q, want the default all", what, cfg.Sites.Listing)
	}
}

// checkService loads BACKYARD_CONFIG_YAML of the ECS task definition with
// the container's environment and secrets.
func checkService(t *testing.T, run *tfrender.Run) *config.Config {
	td := run.Resource("aws_ecs_task_definition.this")
	if td == nil {
		t.Fatal("no aws_ecs_task_definition.this in the state")
	}
	var containers []struct {
		Name        string `json:"name"`
		Environment []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"environment"`
		Secrets []struct {
			Name string `json:"name"`
		} `json:"secrets"`
	}
	defs, _ := td["container_definitions"].(string)
	if err := json.Unmarshal([]byte(defs), &containers); err != nil || len(containers) == 0 {
		t.Fatalf("container_definitions: %v (%q)", err, defs)
	}
	c := containers[0]
	env := map[string]string{}
	for _, e := range c.Environment {
		env[e.Name] = e.Value
	}
	for _, s := range c.Secrets {
		env[s.Name] = tfrender.Placeholder(s.Name)
	}
	raw := env["BACKYARD_CONFIG_YAML"]
	delete(env, "BACKYARD_CONFIG_YAML")
	if raw == "" {
		t.Fatalf("container %q has no BACKYARD_CONFIG_YAML", c.Name)
	}
	if out, _ := run.Output("backyard_config_yaml"); out != raw {
		t.Errorf("output backyard_config_yaml differs from the container's BACKYARD_CONFIG_YAML")
	}
	if missing := tfrender.Missing(raw, env); len(missing) > 0 {
		t.Errorf("backyard.yaml references %v, which the container does not set", missing)
	}
	cfg, err := tfrender.Load(t, raw, env)
	if err != nil {
		t.Fatalf("config.Load rejected the ECS service's config: %v\n--- backyard.yaml ---\n%s", err, raw)
	}
	if cfg.Server.Secret != tfrender.PlaceholderSecret {
		t.Error("server.secret does not come from the BACKYARD_SECRET secret")
	}
	if bucket, _ := run.Output("bucket"); cfg.Storage.Type != "s3" || cfg.Storage.S3.Bucket != bucket {
		t.Errorf("storage = %s/%s, want s3/%s", cfg.Storage.Type, cfg.Storage.S3.Bucket, bucket)
	}
	return cfg
}

// checkGateway loads the Lambda gateway's BACKYARD_CONFIG_YAML the way
// cmd/backyard-lambda does: server.secret comes from the Secrets Manager
// secret named by BACKYARD_SECRET_ARN (here: a placeholder).
func checkGateway(t *testing.T, run *tfrender.Run, fn map[string]any) *config.Config {
	vars, _ := tfrender.Get(fn, "environment", 0, "variables").(map[string]any)
	env := map[string]string{}
	for k, v := range vars {
		s, _ := v.(string)
		env[k] = s
	}
	raw := env["BACKYARD_CONFIG_YAML"]
	if raw == "" {
		t.Fatal("the Lambda gateway has no BACKYARD_CONFIG_YAML")
	}
	if env["BACKYARD_SECRET_ARN"] == "" {
		t.Fatal("the Lambda gateway has no BACKYARD_SECRET_ARN to read server.secret from")
	}
	for _, k := range []string{"BACKYARD_CONFIG_YAML", "BACKYARD_SECRET_ARN", "BACKYARD_SECRET_VERSION_ID"} {
		delete(env, k)
	}
	if _, ok := env["BACKYARD_SECRET"]; ok {
		t.Error("the Lambda gateway gets BACKYARD_SECRET next to BACKYARD_SECRET_ARN (cmd/backyard-lambda refuses both)")
	}
	if missing := tfrender.Missing(raw, env); len(missing) > 0 {
		t.Errorf("the gateway's backyard.yaml references %v, which the function does not set", missing)
	}
	env["BACKYARD_SECRET"] = tfrender.PlaceholderSecret // what the cold start fetches
	cfg, err := tfrender.Load(t, raw, env)
	if err != nil {
		t.Fatalf("config.Load rejected the Lambda gateway's config: %v\n--- backyard.yaml ---\n%s", err, raw)
	}
	if bucket, _ := run.Output("bucket"); cfg.Storage.S3.Bucket != bucket {
		t.Errorf("gateway storage.s3.bucket = %q, want %q", cfg.Storage.S3.Bucket, bucket)
	}
	if cfg.Deploy.AllowAnonymous {
		t.Error("the public Lambda gateway allows anonymous deploys")
	}
	return cfg
}
