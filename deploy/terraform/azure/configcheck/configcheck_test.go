package configcheck

import (
	"sort"
	"testing"

	"github.com/thasami/backyard/scripts/tfrender"
)

// TestRenderedConfigsLoad renders the module's test scenarios and loads the
// Container App's configuration of each one.
func TestRenderedConfigsLoad(t *testing.T) {
	tf := tfrender.Terraform(t)
	runs := tfrender.Render(t, tf, tfrender.Options{Module: ".."})

	names := make([]string, 0, len(runs))
	for name := range runs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		run := runs[name]
		t.Run(name, func(t *testing.T) { checkContainerApp(t, run) })
	}
}

// checkContainerApp loads BACKYARD_CONFIG_YAML of the backyard container
// with its plain and secret-backed environment variables.
func checkContainerApp(t *testing.T, run *tfrender.Run) {
	app := run.Resource("azurerm_container_app.this")
	if app == nil {
		t.Fatal("no azurerm_container_app.this in the state")
	}
	vars, _ := tfrender.Get(app, "template", 0, "container", 0, "env").([]any)
	env := map[string]string{}
	for _, v := range vars {
		name, _ := tfrender.Get(v, "name").(string)
		value, _ := tfrender.Get(v, "value").(string)
		if secret, _ := tfrender.Get(v, "secret_name").(string); secret != "" {
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
		t.Fatalf("config.Load rejected the Container App's config: %v\n--- backyard.yaml ---\n%s", err, raw)
	}
	if cfg.Server.Secret != tfrender.PlaceholderSecret {
		t.Error("server.secret does not come from the BACKYARD_SECRET secret")
	}
	if account, _ := run.Output("storage_account_name"); cfg.Storage.Type != "azure" || cfg.Storage.Azure.Account != account {
		t.Errorf("storage = %s/%s, want azure/%s", cfg.Storage.Type, cfg.Storage.Azure.Account, account)
	}
	if cfg.Database.Type != "postgres" || cfg.PubSub.Type != "postgres" {
		t.Errorf("database/pubsub = %s/%s, want postgres/postgres (replicas share both)", cfg.Database.Type, cfg.PubSub.Type)
	}
}
