package main

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot is the repository root relative to this package.
const repoRoot = "../.."

// TestMain makes the tests independent of the caller's environment: these
// variables are read by config.ApplyEnvOverrides and would change results.
func TestMain(m *testing.M) {
	for _, k := range []string{"BACKYARD_SECRET", "BACKYARD_BASE_DOMAIN", "BACKYARD_PUBLIC_URL", "DATABASE_URL", "PORT",
		"BACKYARD_CONFIG", "BACKYARD_CONFIG_YAML"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const minimalConfig = `
server:
  base_domain: backyard.example.com
  name: "${TEST_CHECKCONFIG_NAME}"
deploy:
  rsync_target: "${TEST_CHECKCONFIG_RSYNC_TARGET}"
`

func TestRun(t *testing.T) {
	valid := writeTemp(t, "valid.yaml", minimalConfig)
	unknownKey := writeTemp(t, "unknown.yaml", "server:\n  base_domain: a.example.com\n  colour: blue\n")
	badYAML := writeTemp(t, "bad.yaml", "server: [\n")
	invalid := writeTemp(t, "invalid.yaml", "server:\n  base_domain: a.example.com\nauth:\n  mode: oidc\n")
	// An unquoted reference that expands to a value ending in ":" is a YAML error.
	unquoted := writeTemp(t, "unquoted.yaml", "server:\n  base_domain: a.example.com\ndeploy:\n  rsync_target: ${TEST_CHECKCONFIG_RSYNC_TARGET}\n")
	devOnly := writeTemp(t, "dev.yaml", "telemetry:\n  log_level: debug\n")
	manifests := writeTemp(t, "rendered.yaml", `---
apiVersion: v1
kind: Service
metadata:
  name: backyard
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: backyard
data:
  backyard.yaml: |
    server:
      base_domain: backyard.example.com
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: other
data:
  something.txt: hi
`)
	noConfigMap := writeTemp(t, "none.yaml", "apiVersion: v1\nkind: Service\nmetadata:\n  name: x\n")

	tests := []struct {
		name     string
		args     []string
		stdin    string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{name: "valid with placeholders", args: []string{valid}, wantCode: 0, wantOut: "ok   " + valid},
		{name: "quiet", args: []string{"-q", valid}, wantCode: 0},
		{name: "unknown key", args: []string{unknownKey}, wantCode: 1, wantErr: "field colour not found"},
		{name: "bad yaml", args: []string{badYAML}, wantCode: 1, wantErr: "FAIL " + badYAML},
		{name: "validation errors", args: []string{invalid}, wantCode: 1, wantErr: "auth.oidc.issuer and auth.oidc.client_id are required"},
		{name: "unquoted rsync target", args: []string{unquoted}, wantCode: 1, wantErr: "mapping values are not allowed"},
		{name: "env flag wins over placeholder", args: []string{"-env", "TEST_CHECKCONFIG_RSYNC_TARGET=rsync.example.com/sites", unquoted}, wantCode: 0},
		{name: "missing base domain without dev", args: []string{devOnly}, wantCode: 1, wantErr: "server.base_domain is required"},
		{name: "dev defaults", args: []string{"-dev", devOnly}, wantCode: 0, wantOut: "public_url=http://localhost:8080"},
		{name: "missing file", args: []string{filepath.Join(t.TempDir(), "nope.yaml")}, wantCode: 1, wantErr: "no such file"},
		{name: "stdin", args: []string{"-"}, stdin: minimalConfig, wantCode: 0, wantOut: "ok   <stdin>"},
		{name: "configmap", args: []string{"-configmap", manifests}, wantCode: 0, wantOut: "(ConfigMap backyard)"},
		{name: "configmap missing", args: []string{"-configmap", noConfigMap}, wantCode: 1, wantErr: "no ConfigMap"},
		{name: "several files, one bad", args: []string{valid, invalid}, wantCode: 1, wantOut: "ok   " + valid, wantErr: "FAIL " + invalid},
		{name: "no args", args: nil, wantCode: 2, wantErr: "usage"},
		{name: "bad env flag", args: []string{"-env", "NOEQUALS", valid}, wantCode: 2, wantErr: "want KEY=VALUE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tt.args, strings.NewReader(tt.stdin), &stdout, &stderr)
			if code != tt.wantCode {
				t.Fatalf("exit code = %d, want %d\nstdout: %s\nstderr: %s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if tt.wantOut != "" && !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestRunRestoresEnvironment(t *testing.T) {
	t.Setenv("TEST_CHECKCONFIG_KEEP", "original")
	// t.Setenv restores the variables after the test; then unset them.
	for _, k := range []string{"TEST_CHECKCONFIG_NAME", "BACKYARD_SECRET"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	p := writeTemp(t, "c.yaml", minimalConfig)
	var out bytes.Buffer
	if code := run([]string{"-env", "TEST_CHECKCONFIG_KEEP=changed", p}, strings.NewReader(""), &out, &out); code != 0 {
		t.Fatalf("exit code %d: %s", code, out.String())
	}
	if got := os.Getenv("TEST_CHECKCONFIG_KEEP"); got != "original" {
		t.Errorf("TEST_CHECKCONFIG_KEEP = %q after run, want restored", got)
	}
	for _, k := range []string{"TEST_CHECKCONFIG_NAME", "BACKYARD_SECRET"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Errorf("%s leaked into the environment", k)
		}
	}
}

func TestPlaceholders(t *testing.T) {
	t.Setenv("TEST_CHECKCONFIG_SET", "value")
	got := placeholders(`a: ${TEST_CHECKCONFIG_SET}
b: ${TEST_CHECKCONFIG_UNSET}
c: ${TEST_CHECKCONFIG_UNSET}
d: ${TEST_CHECKCONFIG_DEFAULTED:-x}
e: ${TEST_CHECKCONFIG_EMPTY_DEFAULT:-}
f: $${TEST_CHECKCONFIG_ESCAPED}
g: ${lower_case_ok}`)
	want := []string{
		"TEST_CHECKCONFIG_UNSET=" + placeholderSecret,
		"lower_case_ok=" + placeholderSecret,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("placeholders = %q, want %q", got, want)
	}
}

func TestPlaceholderFor(t *testing.T) {
	tests := map[string]string{
		"DATABASE_URL":          "postgres://backyard:placeholder@postgres:5432/backyard?sslmode=disable",
		"PUBSUB_DSN":            "postgres://backyard:placeholder@postgres:5432/backyard?sslmode=disable",
		"REDIS_URL":             "redis://redis:6379/0",
		"BACKYARD_DOMAIN":       "backyard.example.com",
		"DB_HOST":               "backyard.example.com",
		"BACKYARD_PUBLIC_URL":   "https://backyard.example.com",
		"OIDC_ISSUER":           "https://backyard.example.com",
		"ACME_EMAIL":            "admin@example.com",
		"BACKYARD_RSYNC_TARGET": "backyard@backyard.example.com:",
		"BACKYARD_NAME":         "Acme: Backyard #1",
		"BACKYARD_SECRET":       placeholderSecret,
		"OIDC_CLIENT_SECRET":    placeholderSecret,
	}
	for name, want := range tests {
		if got := placeholderFor(name); got != want {
			t.Errorf("placeholderFor(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestSampleConfigs keeps the configurations shipped under deploy/ valid
// against internal/config.
func TestSampleConfigs(t *testing.T) {
	samples := []string{
		"deploy/docker-compose/backyard.yaml",
		"deploy/systemd/backyard.yaml",
	}
	for _, rel := range samples {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
			if err != nil {
				t.Fatalf("read sample: %v", err)
			}
			cfg, err := check(raw, false)
			if err != nil {
				t.Fatalf("%s is not a valid config:\n%v", rel, err)
			}
			// The kits run without SSO by default: deploys must still be
			// token-gated (the installers create the "deploy" token), never
			// anonymous.
			if cfg.Deploy.AllowAnonymous {
				t.Errorf("%s allows anonymous deploys by default", rel)
			}
			hasDeploy := false
			for _, tok := range cfg.Auth.Tokens {
				hasDeploy = hasDeploy || tok.Name == "deploy"
			}
			if !hasDeploy {
				t.Errorf("%s has no static \"deploy\" token: nobody could deploy after installing", rel)
			}
		})
	}
}

// TestComposeSampleWithEnvExample validates the compose config with the
// values from .env.example (as docker compose would pass them).
func TestComposeSampleWithEnvExample(t *testing.T) {
	dir := filepath.Join(repoRoot, "deploy/docker-compose")
	f, err := os.Open(filepath.Join(dir, ".env.example"))
	if err != nil {
		t.Fatalf("open .env.example: %v", err)
	}
	defer f.Close()
	var pairs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf(".env.example: malformed line %q", line)
		}
		if v != "" {
			pairs = append(pairs, k+"="+v)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for _, kv := range pairs {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	// docker-compose.yml derives the public URL from the domain.
	t.Setenv("BACKYARD_PUBLIC_URL", "https://"+os.Getenv("BACKYARD_DOMAIN"))
	raw, err := os.ReadFile(filepath.Join(dir, "backyard.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := check(raw, false)
	if err != nil {
		t.Fatalf("compose backyard.yaml with .env.example values: %v", err)
	}
	if cfg.Server.BaseDomain != "localhost" || cfg.Server.PublicURL != "https://localhost" {
		t.Errorf("base_domain/public_url = %q/%q, want localhost/https://localhost", cfg.Server.BaseDomain, cfg.Server.PublicURL)
	}
	if cfg.Deploy.AllowAnonymous {
		t.Error("the compose sample must not allow anonymous deploys by default")
	}
}

// TestHelmValues merges the chart's default and production `config` values
// the way Helm does and validates the results.
func TestHelmValues(t *testing.T) {
	chart := filepath.Join(repoRoot, "deploy/helm/backyard")
	base := readValues(t, filepath.Join(chart, "values.yaml"))
	prod := readValues(t, filepath.Join(chart, "values-production.yaml"))

	cases := map[string]map[string]any{
		"values.yaml":            base,
		"values-production.yaml": mergeValues(base, prod),
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			cfgValue, ok := values["config"].(map[string]any)
			if !ok {
				t.Fatalf("%s: no config map", name)
			}
			raw, err := yaml.Marshal(cfgValue)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := check(raw, false); err != nil {
				t.Fatalf("%s: config is invalid:\n%v\n---\n%s", name, err, raw)
			}
		})
	}
}

// TestHelmTemplate renders the chart with helm (when installed) and checks
// the ConfigMaps end to end.
func TestHelmTemplate(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	chart := filepath.Join(repoRoot, "deploy/helm/backyard")
	for _, extra := range [][]string{nil, {"-f", filepath.Join(chart, "values-production.yaml")}} {
		args := append([]string{"template", "backyard", chart}, extra...)
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm %v: %v\n%s", args, err, out)
		}
		var stdout, stderr bytes.Buffer
		if code := run([]string{"-configmap", "-"}, bytes.NewReader(out), &stdout, &stderr); code != 0 {
			t.Fatalf("helm %v: rendered config invalid:\n%s", args, stderr.String())
		}
	}
}

func readValues(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

// mergeValues deep-merges override into base like Helm: maps merge, other
// values (lists included) replace, and null deletes a key.
func mergeValues(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range override {
		if v == nil {
			delete(out, k)
			continue
		}
		if om, ok := v.(map[string]any); ok {
			if bm, ok := out[k].(map[string]any); ok {
				out[k] = mergeValues(bm, om)
				continue
			}
		}
		out[k] = v
	}
	return out
}

func TestMergeValues(t *testing.T) {
	base := map[string]any{"a": map[string]any{"x": 1, "y": 2}, "l": []any{1, 2}, "d": "gone"}
	over := map[string]any{"a": map[string]any{"y": 3}, "l": []any{9}, "d": nil}
	got := mergeValues(base, over)
	a := got["a"].(map[string]any)
	if a["x"] != 1 || a["y"] != 3 {
		t.Errorf("maps not merged: %v", a)
	}
	if l := got["l"].([]any); len(l) != 1 || l[0] != 9 {
		t.Errorf("lists not replaced: %v", l)
	}
	if _, ok := got["d"]; ok {
		t.Error("null did not delete the key")
	}
}
