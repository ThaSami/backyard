// Command checkconfig validates Backyard server configuration files exactly
// the way `backyard serve` loads them (strict YAML with ${ENV} expansion,
// environment overrides, defaults and validation, see internal/config), but
// without needing the real secrets. It keeps the sample configurations
// shipped under deploy/ in sync with the schema (CI runs it, and its tests
// cover the samples).
//
//	go run ./scripts/checkconfig deploy/systemd/backyard.yaml
//	go run ./scripts/checkconfig -env BACKYARD_DOMAIN=acme.dev deploy/docker-compose/backyard.yaml
//	helm template backyard deploy/helm/backyard | go run ./scripts/checkconfig -configmap -
//
// Environment variables referenced as ${VAR} (without a ":-default") that are
// unset get a placeholder of the right shape for the duration of the check:
// a domain for *_DOMAIN, a URL for *_URL, a DSN for *_DSN / DATABASE_URL, an
// rsync target ending in ":" for *RSYNC_TARGET, a name with YAML-special
// characters for *_NAME (so unquoted references fail) and a 64-character hex
// string for everything else. BACKYARD_SECRET, which the server reads from the
// environment directly, gets one too when the file sets no server.secret.
// Real values passed with -env or present in the environment always win.
//
// With -configmap, the input is a stream of Kubernetes manifests (for example
// `helm template` output) and the "backyard.yaml" key of every ConfigMap is
// checked; "-" reads standard input.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/thasami/backyard/internal/config"
)

// configKey is the ConfigMap key holding the server configuration.
const configKey = "backyard.yaml"

// envRef matches ${VAR} and ${VAR:-default} like config.ExpandEnv does.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// placeholderSecret is long enough for server.secret (≥ 32 characters).
const placeholderSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// envList collects repeated -env KEY=VALUE flags.
type envList []string

func (e *envList) String() string { return strings.Join(*e, ",") }

func (e *envList) Set(v string) error {
	k, _, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("want KEY=VALUE, got %q", v)
	}
	*e = append(*e, v)
	return nil
}

// options control one run.
type options struct {
	dev       bool
	configMap bool
	quiet     bool
	env       envList
}

// source is one configuration document to check.
type source struct {
	name string
	raw  []byte
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run executes the command and returns the process exit code.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var opts options
	fs := flag.NewFlagSet("checkconfig", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&opts.dev, "dev", false, "apply dev defaults first (like `backyard serve --dev`)")
	fs.BoolVar(&opts.configMap, "configmap", false, "inputs are Kubernetes manifests; check each ConfigMap's backyard.yaml")
	fs.BoolVar(&opts.quiet, "q", false, "print only problems")
	fs.Var(&opts.env, "env", "set an environment variable for the check (KEY=VALUE, repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: checkconfig [-dev] [-configmap] [-env KEY=VALUE]... file|- ...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	restore := setEnv(opts.env)
	defer restore()

	failed := false
	for _, arg := range fs.Args() {
		sources, err := readSources(arg, stdin, opts.configMap)
		if err != nil {
			fmt.Fprintf(stderr, "FAIL %s: %v\n", arg, err)
			failed = true
			continue
		}
		for _, src := range sources {
			cfg, err := check(src.raw, opts.dev)
			if err != nil {
				failed = true
				fmt.Fprintf(stderr, "FAIL %s:\n", src.name)
				for _, line := range strings.Split(err.Error(), "\n") {
					if line = strings.TrimSpace(line); line != "" {
						fmt.Fprintf(stderr, "  - %s\n", line)
					}
				}
				continue
			}
			if !opts.quiet {
				fmt.Fprintf(stdout, "ok   %s (%s)\n", src.name, summary(cfg))
			}
		}
	}
	if failed {
		return 1
	}
	return 0
}

// readSources loads the documents named by arg ("-" is stdin).
func readSources(arg string, stdin io.Reader, configMap bool) ([]source, error) {
	var raw []byte
	var err error
	name := arg
	if arg == "-" {
		name = "<stdin>"
		raw, err = io.ReadAll(stdin)
	} else {
		raw, err = os.ReadFile(arg)
	}
	if err != nil {
		return nil, err
	}
	if !configMap {
		return []source{{name: name, raw: raw}}, nil
	}
	return configMapSources(name, raw)
}

// configMapSources extracts backyard.yaml from every ConfigMap in a manifest
// stream.
func configMapSources(name string, raw []byte) ([]source, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out []source
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data map[string]string `yaml:"data"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse manifests: %w", err)
		}
		if doc.Kind != "ConfigMap" {
			continue
		}
		if v, ok := doc.Data[configKey]; ok {
			out = append(out, source{name: fmt.Sprintf("%s (ConfigMap %s)", name, doc.Metadata.Name), raw: []byte(v)})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ConfigMap with a %q key found", configKey)
	}
	return out, nil
}

// check loads one document the way config.Load does, with placeholders for
// unset environment references.
func check(raw []byte, dev bool) (*config.Config, error) {
	restore := setEnv(placeholders(string(raw)))
	defer restore()

	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	if cfg.Server.Secret == "" && os.Getenv("BACKYARD_SECRET") == "" {
		restoreSecret := setEnv([]string{"BACKYARD_SECRET=" + placeholderSecret})
		defer restoreSecret()
	}
	if dev {
		cfg.Server.Dev = true
	}
	if cfg.Server.Dev {
		cfg.ApplyDevDefaults()
	}
	cfg.ApplyEnvOverrides()
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// placeholders returns KEY=VALUE pairs for every ${VAR} reference in s that
// has no default and is unset (or empty) in the environment.
func placeholders(s string) []string {
	s = strings.ReplaceAll(s, "$${", "")
	seen := map[string]bool{}
	var out []string
	for _, m := range envRef.FindAllStringSubmatch(s, -1) {
		name, hasDefault := m[1], m[2] != ""
		if hasDefault || seen[name] || os.Getenv(name) != "" {
			continue
		}
		seen[name] = true
		out = append(out, name+"="+placeholderFor(name))
	}
	return out
}

// placeholderFor returns a value of the right shape for an environment
// variable, judged by its name.
func placeholderFor(name string) string {
	n := strings.ToUpper(name)
	switch {
	case strings.HasSuffix(n, "RSYNC_TARGET"):
		// The trailing ":" breaks unquoted YAML values: keep samples honest.
		return "backyard@backyard.example.com:"
	case strings.HasSuffix(n, "_NAME"):
		return "Acme: Backyard #1"
	case n == "DATABASE_URL" || strings.HasSuffix(n, "_DSN"):
		return "postgres://backyard:placeholder@postgres:5432/backyard?sslmode=disable"
	case strings.HasSuffix(n, "REDIS_URL"):
		return "redis://redis:6379/0"
	case strings.HasSuffix(n, "_DOMAIN") || strings.HasSuffix(n, "_HOST"):
		return "backyard.example.com"
	case strings.HasSuffix(n, "_URL") || strings.HasSuffix(n, "_ISSUER"):
		return "https://backyard.example.com"
	case strings.HasSuffix(n, "_EMAIL"):
		return "admin@example.com"
	default:
		return placeholderSecret
	}
}

// setEnv applies KEY=VALUE pairs and returns a function restoring the
// previous environment.
func setEnv(pairs []string) func() {
	type prev struct {
		key   string
		value string
		set   bool
	}
	var saved []prev
	for _, kv := range pairs {
		k, v, _ := strings.Cut(kv, "=")
		old, ok := os.LookupEnv(k)
		saved = append(saved, prev{k, old, ok})
		_ = os.Setenv(k, v)
	}
	return func() {
		for i := len(saved) - 1; i >= 0; i-- {
			p := saved[i]
			if p.set {
				_ = os.Setenv(p.key, p.value)
			} else {
				_ = os.Unsetenv(p.key)
			}
		}
	}
}

// summary describes the effective configuration in one line.
func summary(c *config.Config) string {
	return fmt.Sprintf("public_url=%s url_mode=%s auth=%s storage=%s database=%s pubsub=%s deploy=%s",
		c.Server.PublicURL, c.Server.URLMode, c.Auth.Mode, c.Storage.Type, c.Database.Type,
		c.PubSub.Type, strings.Join(c.Deploy.Methods, ","))
}
