// Package tfrender renders Backyard's Terraform modules for tests. It runs a
// module's `terraform test` scenarios (providers mocked, nothing created) in
// a scratch copy, returns what each run left in its state, and loads a
// rendered backyard.yaml with internal/config the way the server does at
// startup (strict YAML, ${ENV} expansion, environment overrides, defaults and
// validation).
//
// The configcheck tests under deploy/terraform/{aws,gcp,azure}/configcheck
// use it, so CI catches a rendered configuration that the server would refuse
// before anyone deploys it (an ECS task, Cloud Run revision or Container App
// that never becomes ready).
//
// Terraform only runs when BACKYARD_TEST_TERRAFORM is set ("1" for terraform
// on PATH, or the path of a terraform binary); otherwise the tests skip.
// Provider downloads honour TF_PLUGIN_CACHE_DIR; without it the module's own
// .terraform directory is reused as a cache when `terraform init` ran there.
// Terraform's plugin cache is not safe for concurrent use, so run several
// configcheck packages with `go test -p 1` (make tf-configcheck does).
package tfrender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
)

// EnvVar names the variable that enables the Terraform-backed tests.
const EnvVar = "BACKYARD_TEST_TERRAFORM"

// Terraform returns the terraform binary to use, or skips the test when
// BACKYARD_TEST_TERRAFORM is unset. A set variable with no binary to be found
// fails the test: CI must not pass by skipping.
func Terraform(t testing.TB) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(EnvVar))
	switch strings.ToLower(v) {
	case "", "0", "false", "no":
		t.Skipf("set %s=1 (or the path of a terraform binary) to render the module with terraform test", EnvVar)
	case "1", "true", "yes":
		v = "terraform"
	}
	p, err := exec.LookPath(v)
	if err != nil {
		t.Fatalf("%s is set but terraform was not found: %v", EnvVar, err)
	}
	return p
}

// Options select what Render runs.
type Options struct {
	// Module is the Terraform module directory.
	Module string
	// TestDir is the test directory, relative to the module, whose
	// *.tftest.hcl files run ("tests" when empty).
	TestDir string
	// Extra copies more files into the scratch copy: destination (relative
	// to the copy's root) → source path.
	Extra map[string]string
	// Timeout bounds init and test together (20 minutes when zero).
	Timeout time.Duration
}

// Run is what one `run` block of a test file left in its state. Only runs
// that apply (command = apply, the default) have one.
type Run struct {
	Name string
	// File is the test file the run belongs to.
	File string
	// Outputs are the module's output values, decoded from JSON.
	Outputs map[string]any
	// Resources are the resources' attribute values by address (child
	// modules included, as module.<name>.<address>).
	Resources map[string]map[string]any
}

// Output returns a string output.
func (r *Run) Output(name string) (string, bool) {
	s, ok := r.Outputs[name].(string)
	return s, ok
}

// Resource returns a resource's attributes by address, or nil.
func (r *Run) Resource(address string) map[string]any { return r.Resources[address] }

// Render copies the module (without .terraform, examples or configcheck
// directories) to a temporary directory, runs `terraform init` and
// `terraform test -json -verbose` there, and returns the runs that left a
// state, by run name. Failed runs and error diagnostics fail the test; a
// failed init (usually an unreachable provider registry) skips it.
func Render(t testing.TB, tf string, o Options) map[string]*Run {
	t.Helper()
	module, err := filepath.Abs(o.Module)
	if err != nil {
		t.Fatal(err)
	}
	testDir := o.TestDir
	if testDir == "" {
		testDir = "tests"
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Minute
	}
	work := t.TempDir()
	if err := copyModule(module, work); err != nil {
		t.Fatalf("copy %s: %v", module, err)
	}
	for dst, src := range o.Extra {
		if err := copyFile(src, filepath.Join(work, dst)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	env := environ(module)
	initCmd := exec.CommandContext(ctx, tf, "init", "-backend=false", "-input=false", "-no-color")
	initCmd.Dir, initCmd.Env = work, env
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Skipf("terraform init failed (providers unavailable?): %v\n%s", err, tail(string(out), 2000))
	}

	cmd := exec.CommandContext(ctx, tf, "test", "-test-directory="+testDir, "-json", "-verbose", "-no-color")
	cmd.Dir, cmd.Env = work, env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("terraform test: %v", err)
	}
	runs, problems, perr := Parse(stdout)
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	for _, p := range problems {
		t.Error(p)
	}
	if perr != nil {
		t.Fatalf("read terraform test output: %v", perr)
	}
	if werr != nil {
		t.Fatalf("terraform test: %v\n%s", werr, tail(stderr.String(), 4000))
	}
	if len(runs) == 0 {
		t.Fatalf("terraform test in %s/%s left no state: no run applied", module, testDir)
	}
	return runs
}

// Parse reads the machine-readable output of `terraform test -json
// -verbose` and returns the runs that left a state, plus one problem per
// failed run with its error diagnostics. (Runs with expect_failures report
// the expected errors as diagnostics too, but pass.)
func Parse(r io.Reader) (map[string]*Run, []string, error) {
	type resource struct {
		Address string         `json:"address"`
		Values  map[string]any `json:"values"`
	}
	type module struct {
		Resources    []resource `json:"resources"`
		ChildModules []module   `json:"child_modules"`
	}
	// Each line is one message; verbose state messages embed the provider
	// schemas and can be tens of megabytes, so decode as a stream and only
	// the fields needed.
	type message struct {
		File    string `json:"@testfile"`
		Run     string `json:"@testrun"`
		TestRun *struct {
			Status string `json:"status"`
		} `json:"test_run"`
		TestState *struct {
			Outputs map[string]struct {
				Value json.RawMessage `json:"value"`
			} `json:"outputs"`
			RootModule module `json:"root_module"`
		} `json:"test_state"`
		Diagnostic *struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
		} `json:"diagnostic"`
	}
	runs := map[string]*Run{}
	var failed []string                  // runs that failed, in order
	status := map[string]string{}        // run → fail | error
	diagnostics := map[string][]string{} // run (or "" for the file) → error diagnostics
	problems := func() []string {
		var out []string
		for _, name := range failed {
			p := fmt.Sprintf("terraform test run %q: %s", name, status[name])
			for _, d := range diagnostics[name] {
				p += "\n  " + d
			}
			out = append(out, p)
		}
		if len(failed) == 0 {
			// Errors outside any run (a broken test file) fail the command.
			for _, d := range diagnostics[""] {
				out = append(out, "terraform test: "+d)
			}
		}
		return out
	}
	dec := json.NewDecoder(r)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return runs, problems(), nil
			}
			return runs, problems(), err
		}
		switch {
		case m.Diagnostic != nil && m.Diagnostic.Severity == "error":
			diagnostics[m.Run] = append(diagnostics[m.Run], m.Diagnostic.Summary+": "+m.Diagnostic.Detail)
		case m.TestRun != nil && (m.TestRun.Status == "fail" || m.TestRun.Status == "error"):
			if _, seen := status[m.Run]; !seen {
				failed = append(failed, m.Run)
			}
			status[m.Run] = m.TestRun.Status
		case m.TestState != nil && m.Run != "":
			if prev, ok := runs[m.Run]; ok && prev.File != m.File {
				return runs, problems(), fmt.Errorf("run %q appears in both %s and %s: run names must be unique across test files", m.Run, prev.File, m.File)
			}
			run := &Run{Name: m.Run, File: m.File, Outputs: map[string]any{}, Resources: map[string]map[string]any{}}
			for name, o := range m.TestState.Outputs {
				var v any
				if len(o.Value) > 0 {
					if err := json.Unmarshal(o.Value, &v); err != nil {
						return runs, problems(), fmt.Errorf("run %q: output %s: %w", m.Run, name, err)
					}
				}
				run.Outputs[name] = v
			}
			var walk func(module)
			walk = func(mod module) {
				for _, res := range mod.Resources {
					run.Resources[res.Address] = res.Values
				}
				for _, c := range mod.ChildModules {
					walk(c)
				}
			}
			walk(m.TestState.RootModule)
			runs[m.Run] = run
		}
	}
}

// Get walks decoded JSON: string keys index objects, ints index arrays. It
// returns nil when the path does not exist.
func Get(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k < 0 || k >= len(l) {
				return nil
			}
			v = l[k]
		default:
			return nil
		}
	}
	return v
}

// envRef matches ${VAR} and ${VAR:-default} like config.ExpandEnv.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)

// Refs returns the variables that raw references as ${VAR} without a
// default (escaped $${…} sequences excluded), sorted.
func Refs(raw string) []string {
	raw = strings.ReplaceAll(raw, "$${", "")
	seen := map[string]bool{}
	var out []string
	for _, m := range envRef.FindAllStringSubmatch(raw, -1) {
		if m[2] != "" || seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// Missing returns the variables raw references (see Refs) that env does not
// set: the server would expand them to empty strings.
func Missing(raw string, env map[string]string) []string {
	var out []string
	for _, name := range Refs(raw) {
		if env[name] == "" {
			out = append(out, name)
		}
	}
	return out
}

// PlaceholderSecret is a 64-character value, long enough for server.secret.
const PlaceholderSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// Placeholder returns a value of the right shape for a secret-backed
// environment variable, judged by its name (a DSN for DATABASE_URL and
// *_DSN, a URL for *_URL, a random-looking string otherwise).
func Placeholder(name string) string {
	n := strings.ToUpper(name)
	switch {
	case n == "DATABASE_URL" || strings.HasSuffix(n, "_DSN"):
		return "postgres://backyard:placeholder@db.internal:5432/backyard?sslmode=require"
	case strings.Contains(n, "REDIS") && strings.HasSuffix(n, "_URL"):
		return "rediss://:placeholder@cache.internal:6379/0"
	case strings.HasSuffix(n, "_URL"):
		return "https://placeholder.example.com"
	default:
		return PlaceholderSecret
	}
}

// configEnv are the variables config.Load reads besides ${VAR} references;
// Load clears them so the caller's environment cannot change the result.
var configEnv = []string{"BACKYARD_CONFIG_YAML", "BACKYARD_CONFIG", "BACKYARD_SECRET", "BACKYARD_BASE_DOMAIN", "BACKYARD_PUBLIC_URL", "DATABASE_URL", "PORT"}

// Load writes raw to a file and loads it with config.Load in exactly env:
// the variables config.Load reads and every variable raw references are
// cleared first, then env is applied (t.Setenv, so tests using Load cannot
// run in parallel).
func Load(t testing.TB, raw string, env map[string]string) (*config.Config, error) {
	t.Helper()
	for _, k := range configEnv {
		t.Setenv(k, "")
	}
	for _, k := range Refs(raw) {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	path := filepath.Join(t.TempDir(), "backyard.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(path, false)
}

// environ is the environment for terraform: non-interactive, and reusing
// the module's own provider installation as a plugin cache when no cache is
// configured (its layout matches TF_PLUGIN_CACHE_DIR's).
func environ(module string) []string {
	env := append(os.Environ(), "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
	if os.Getenv("TF_PLUGIN_CACHE_DIR") == "" {
		cache := filepath.Join(module, ".terraform", "providers")
		if fi, err := os.Stat(cache); err == nil && fi.IsDir() {
			env = append(env, "TF_PLUGIN_CACHE_DIR="+cache)
		}
	}
	return env
}

// skipDirs are module subdirectories that are not part of the module or its
// tests.
var skipDirs = map[string]bool{".terraform": true, "configcheck": true, "examples": true}

// copyModule copies the module's files (configuration, templates, tests, the
// lock file) from src to dst.
func copyModule(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasSuffix(d.Name(), ".tfstate") || strings.HasPrefix(d.Name(), "terraform.tfstate") {
			return nil
		}
		return copyFile(path, filepath.Join(dst, rel))
	})
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
