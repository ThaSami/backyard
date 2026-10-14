package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/thasami/backyard/internal/app"
	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/templates"
)

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// fakeRunner records commands instead of executing them.
type fakeRunner struct {
	mu    sync.Mutex
	calls []Command
	fn    func(Command) error
}

func (r *fakeRunner) Run(_ context.Context, c Command) error {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	fn := r.fn
	r.mu.Unlock()
	if fn != nil {
		return fn(c)
	}
	return nil
}

func (r *fakeRunner) Calls() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// servedCall records a runServer invocation.
type servedCall struct {
	cfg  *config.Config
	opts app.Options
}

// harness runs the command tree with fake dependencies.
type harness struct {
	t          *testing.T
	cwd        string
	home       string
	cache      string
	configPath string
	runner     *fakeRunner
	tools      map[string]bool
	stdin      string
	stdinTTY   bool
	render     func(templates.InitOptions) (map[string][]byte, error)
	skills     fs.FS
	now        time.Time
	// releaseBase is the release download base of `backyard cloudflare`;
	// httpClient, when set, replaces the HTTP client (e.g. a TLS test server's).
	releaseBase string
	httpClient  *http.Client

	mu       sync.Mutex
	opened   []string
	openFn   func(url string) error
	served   []servedCall
	timeout  time.Duration
	pollUnit time.Duration

	out syncBuffer
	err syncBuffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	for _, k := range []string{clientconfig.EnvServer, clientconfig.EnvGateway, clientconfig.EnvToken,
		clientconfig.EnvProfile, clientconfig.EnvDeployMethod, "BACKYARD_CONFIG", "BACKYARD_CONFIG_YAML", "BACKYARD_SECRET"} {
		t.Setenv(k, "")
	}
	root := t.TempDir()
	h := &harness{
		t:          t,
		cwd:        filepath.Join(root, "work"),
		home:       filepath.Join(root, "home"),
		cache:      filepath.Join(root, "cache"),
		configPath: filepath.Join(root, "home", ".config", "backyard", "config.json"),
		runner:     &fakeRunner{},
		tools:      map[string]bool{},
		render:     fakeRender,
		skills: fstest.MapFS{
			"SKILL.md":                {Data: []byte("# Backyard skill\n")},
			"references/api.md":       {Data: []byte("# API\n")},
			"references/recipes.md":   {Data: []byte("# Recipes\n")},
			"references/deploy.md":    {Data: []byte("# Deploy\n")},
			"references/sub/extra.md": {Data: []byte("extra")},
		},
		now:         time.Now(),
		releaseBase: "http://127.0.0.1:1/",
		timeout:     5 * time.Second,
		pollUnit:    time.Millisecond,
	}
	for _, d := range []string{h.cwd, h.home, h.cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h.home, ".config"))
	t.Setenv("HOME", h.home)
	return h
}

func (h *harness) deps() *deps {
	return &deps{
		runner: h.runner,
		lookPath: func(name string) (string, error) {
			if h.tools[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		openBrowser: func(_ context.Context, u string) error {
			h.mu.Lock()
			h.opened = append(h.opened, u)
			fn := h.openFn
			h.mu.Unlock()
			if fn != nil {
				return fn(u)
			}
			return nil
		},
		render:  h.render,
		skillFS: func() (fs.FS, error) { return h.skills, nil },
		runServer: func(_ context.Context, cfg *config.Config, opts app.Options) error {
			h.mu.Lock()
			h.served = append(h.served, servedCall{cfg: cfg, opts: opts})
			h.mu.Unlock()
			return nil
		},
		now:            func() time.Time { return h.now },
		getwd:          func() (string, error) { return h.cwd, nil },
		homeDir:        func() (string, error) { return h.home, nil },
		cacheDir:       func() (string, error) { return h.cache, nil },
		stdinTTY:       func() bool { return h.stdinTTY },
		stderrTTY:      func() bool { return false },
		configPath:     h.configPath,
		loginTimeout:   h.timeout,
		devicePollUnit: h.pollUnit,
		releaseBase:    h.releaseBase,
		httpClient:     h.httpClient,
		retryDelay:     time.Millisecond,
	}
}

// run executes the CLI and returns the exit code.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	streams := IOStreams{In: strings.NewReader(h.stdin), Out: &h.out, ErrOut: &h.err}
	root := newRootCommand(streams, h.deps())
	return execute(context.Background(), root, streams, args)
}

// mustRun runs the CLI and fails the test on a non-zero exit.
func (h *harness) mustRun(args ...string) string {
	h.t.Helper()
	if code := h.run(args...); code != 0 {
		h.t.Fatalf("backyard %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, h.out.String(), h.err.String())
	}
	return h.out.String()
}

// mustFail runs the CLI expecting failure and returns stderr.
func (h *harness) mustFail(args ...string) string {
	h.t.Helper()
	if code := h.run(args...); code == 0 {
		h.t.Fatalf("backyard %s: expected failure\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), h.out.String(), h.err.String())
	}
	return h.err.String()
}

// openedURLs returns the URLs passed to the browser opener.
func (h *harness) openedURLs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.opened)
}

// writeProfile stores a profile in the CLI config.
func (h *harness) writeProfile(name string, p *clientconfig.Profile, current bool) {
	h.t.Helper()
	f, err := clientconfig.Load(h.configPath)
	if err != nil {
		h.t.Fatal(err)
	}
	f.Set(name, p)
	if current {
		f.Current = name
	}
	if err := f.Save(h.configPath); err != nil {
		h.t.Fatal(err)
	}
}

// loadConfig reads the CLI config.
func (h *harness) loadConfig() *clientconfig.File {
	h.t.Helper()
	f, err := clientconfig.Load(h.configPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return f
}

// writeFiles creates files (relative to base) with content.
func writeFiles(t *testing.T, base string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(base, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeRender stands in for templates.Render.
func fakeRender(o templates.InitOptions) (map[string][]byte, error) {
	proj, _ := json.Marshal(map[string]string{"site": o.Site})
	files := map[string][]byte{
		".backyard.json":                   proj,
		".claude/skills/backyard/SKILL.md": []byte("skill"),
	}
	agents := o.Agents
	if len(agents) == 0 {
		agents = []string{"claude", "codex"}
	}
	for _, a := range agents {
		switch a {
		case "claude":
			files["CLAUDE.md"] = []byte("# " + o.Site + " (server " + o.Server + ")")
		case "codex":
			files["AGENTS.md"] = []byte("# agents")
		case "cursor":
			files[".cursor/rules/backyard.mdc"] = []byte("rules")
		case "copilot":
			files[".github/copilot-instructions.md"] = []byte("copilot")
		}
	}
	if o.Template == "broken" {
		return nil, errors.New("unknown template")
	}
	if !o.SkipIndex {
		files["index.html"] = []byte("<title>" + o.Site + "</title>")
	}
	return files, nil
}

// alice is the default test user.
var alice = identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice Smith"}

// loggedIn returns a fake gateway with alice's token and a stored profile.
func loggedIn(t *testing.T, h *harness) *clitest.Gateway {
	t.Helper()
	g := clitest.NewGateway(t)
	g.Tokens["alice-token"] = alice
	h.writeProfile("default", &clientconfig.Profile{
		Server: g.URL, Token: "alice-token", Login: clientconfig.LoginBrowser,
		User: &clientconfig.User{ID: alice.ID, Email: alice.Email, Name: alice.Name},
	}, true)
	return g
}
