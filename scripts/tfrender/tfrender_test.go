package tfrender

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	stream := strings.Join([]string{
		`{"@level":"info","type":"version","terraform":"1.16.5"}`,
		`{"@testfile":"tests/m.tftest.hcl","@testrun":"apply_run","type":"test_state","test_state":{"outputs":{"backyard_config_yaml":{"value":"server:\n  base_domain: a.example.com\n"},"n":{"value":3}},"root_module":{"resources":[{"address":"aws_ecs_task_definition.this","values":{"container_definitions":"[]"}}],"child_modules":[{"resources":[{"address":"module.x.random_id.y","values":{"hex":"ab"}}]}]},"provider_schemas":{"huge":{}}}}`,
		`{"@testrun":"apply_run","type":"test_run","test_run":{"path":"tests/m.tftest.hcl","run":"apply_run","progress":"complete","status":"pass"}}`,
		`{"@testrun":"plan_run","type":"test_plan","test_plan":{"output_changes":{}}}`,
		`{"@testrun":"broken","type":"test_run","test_run":{"status":"fail"}}`,
		`{"@testrun":"broken","type":"diagnostic","diagnostic":{"severity":"error","summary":"Test assertion failed","detail":"boom"}}`,
		`{"@testrun":"noisy","type":"diagnostic","diagnostic":{"severity":"warning","summary":"Check block assertion failed"}}`,
		`{"@testrun":"expected","type":"diagnostic","diagnostic":{"severity":"error","summary":"Resource precondition failed","detail":"as the run expects"}}`,
		`{"@testrun":"expected","type":"test_run","test_run":{"status":"pass"}}`,
	}, "\n")
	runs, problems, err := Parse(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs["apply_run"] == nil {
		t.Fatalf("runs = %v, want only apply_run (plan runs leave no state)", runs)
	}
	r := runs["apply_run"]
	if y, ok := r.Output("backyard_config_yaml"); !ok || !strings.Contains(y, "base_domain") {
		t.Errorf("backyard_config_yaml = %q, %v", y, ok)
	}
	if _, ok := r.Output("n"); ok {
		t.Error("a number output read as a string")
	}
	if got := r.Resource("aws_ecs_task_definition.this")["container_definitions"]; got != "[]" {
		t.Errorf("resource values = %v", got)
	}
	if r.Resource("module.x.random_id.y") == nil {
		t.Error("child module resources are missing")
	}
	want := []string{"terraform test run \"broken\": fail\n  Test assertion failed: boom"}
	if !reflect.DeepEqual(problems, want) {
		t.Errorf("problems = %q, want %q (warnings and expected failures are not problems)", problems, want)
	}
	_, problems, _ = Parse(strings.NewReader(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"Invalid test file","detail":"x"}}`))
	if !reflect.DeepEqual(problems, []string{"terraform test: Invalid test file: x"}) {
		t.Errorf("file-level error: problems = %q", problems)
	}
	if r.File != "tests/m.tftest.hcl" {
		t.Errorf("File = %q", r.File)
	}
	dup := `{"@testfile":"tests/a.tftest.hcl","@testrun":"same","type":"test_state","test_state":{}}
{"@testfile":"tests/b.tftest.hcl","@testrun":"same","type":"test_state","test_state":{}}`
	if _, _, err := Parse(strings.NewReader(dup)); err == nil || !strings.Contains(err.Error(), "unique") {
		t.Errorf("duplicate run names across files: err = %v", err)
	}
	if _, _, err := Parse(strings.NewReader(`{"type":`)); err == nil {
		t.Error("truncated output parsed without error")
	}
}

func TestGet(t *testing.T) {
	v := map[string]any{"a": []any{map[string]any{"b": "c"}}}
	if got := Get(v, "a", 0, "b"); got != "c" {
		t.Errorf("Get = %v", got)
	}
	for _, path := range [][]any{{"x"}, {"a", 1}, {"a", -1}, {"a", "b"}, {"a", 0, "b", "c"}, {1.5}} {
		if got := Get(v, path...); got != nil {
			t.Errorf("Get(%v) = %v, want nil", path, got)
		}
	}
}

func TestRefsAndMissing(t *testing.T) {
	raw := `a: ${DATABASE_URL}
b: "${OIDC_CLIENT_SECRET}"
c: ${WITH_DEFAULT:-x}
d: $${ESCAPED}
e: ${DATABASE_URL}
f: ${lower_ok}`
	if got, want := Refs(raw), []string{"DATABASE_URL", "OIDC_CLIENT_SECRET", "lower_ok"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Refs = %q, want %q", got, want)
	}
	env := map[string]string{"DATABASE_URL": "postgres://x", "lower_ok": "", "UNUSED": "1"}
	if got, want := Missing(raw, env), []string{"OIDC_CLIENT_SECRET", "lower_ok"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Missing = %q, want %q", got, want)
	}
}

func TestPlaceholder(t *testing.T) {
	for name, want := range map[string]string{
		"DATABASE_URL":       "postgres://backyard:placeholder@db.internal:5432/backyard?sslmode=require",
		"PUBSUB_DSN":         "postgres://backyard:placeholder@db.internal:5432/backyard?sslmode=require",
		"REDIS_URL":          "rediss://:placeholder@cache.internal:6379/0",
		"OPENAI_BASE_URL":    "https://placeholder.example.com",
		"BACKYARD_SECRET":    PlaceholderSecret,
		"OIDC_CLIENT_SECRET": PlaceholderSecret,
	} {
		if got := Placeholder(name); got != want {
			t.Errorf("Placeholder(%q) = %q, want %q", name, got, want)
		}
	}
	if len(PlaceholderSecret) < 32 {
		t.Error("the placeholder secret is too short for server.secret")
	}
}

func TestLoadUsesOnlyTheGivenEnvironment(t *testing.T) {
	t.Setenv("BACKYARD_PUBLIC_URL", "https://leaked.example.com")
	t.Setenv("TFRENDER_TEST_REF", "leaked")
	raw := "server:\n  base_domain: backyard.example.com\n  name: \"${TFRENDER_TEST_REF}\"\n"
	cfg, err := Load(t, raw, map[string]string{"BACKYARD_SECRET": PlaceholderSecret})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.PublicURL != "https://backyard.example.com" {
		t.Errorf("public_url = %q: the caller's BACKYARD_PUBLIC_URL leaked in", cfg.Server.PublicURL)
	}
	if cfg.Server.Name == "leaked" {
		t.Error("an unset reference was expanded from the caller's environment")
	}
	if _, err := Load(t, "server:\n  base_domain: b.example.com\n  colour: blue\n", map[string]string{"BACKYARD_SECRET": PlaceholderSecret}); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Errorf("unknown key: err = %v, want strict decoding to refuse it", err)
	}
}

func TestCopyModule(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	for _, f := range []string{"main.tf", ".terraform.lock.hcl", "templates/a.tftpl", "tests/m.tftest.hcl",
		".terraform/providers/x", "configcheck/c_test.go", "examples/basic/main.tf", "terraform.tfstate"} {
		p := filepath.Join(src, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyModule(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"main.tf", ".terraform.lock.hcl", "templates/a.tftpl", "tests/m.tftest.hcl"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err != nil {
			t.Errorf("%s was not copied: %v", f, err)
		}
	}
	for _, f := range []string{".terraform", "configcheck", "examples", "terraform.tfstate"} {
		if _, err := os.Stat(filepath.Join(dst, f)); err == nil {
			t.Errorf("%s was copied", f)
		}
	}
}

func TestTerraformSkipsWhenDisabled(t *testing.T) {
	t.Setenv(EnvVar, "")
	ran := false
	t.Run("skip", func(t *testing.T) {
		Terraform(t)
		ran = true
	})
	if ran {
		t.Error("Terraform did not skip with BACKYARD_TEST_TERRAFORM unset")
	}
}
