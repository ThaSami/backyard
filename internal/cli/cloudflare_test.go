package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/version"
)

// cfPackageJSON is a minimal deploy/cloudflare/package.json.
const cfPackageJSON = `{"name":"backyard-cloudflare","scripts":{"setup":"node bin/backyard-cloudflare.mjs setup"}}`

// cfCheckout creates a deploy/cloudflare folder of a Backyard checkout
// under base and returns it.
func cfCheckout(t *testing.T, base string) string {
	t.Helper()
	cf := filepath.Join(base, "deploy", "cloudflare")
	writeFiles(t, cf, map[string]string{
		"package.json":                cfPackageJSON,
		"package-lock.json":           "{}",
		"bin/backyard-cloudflare.mjs": "#!/usr/bin/env node\n",
	})
	return cf
}

// cloudflareWorker is a fake Workers-edition server: no user login, deploys
// gated by DEPLOY_TOKEN (SPEC §16 defaults).
func cloudflareWorker(t *testing.T) *clitest.Gateway {
	t.Helper()
	g := clitest.NewGateway(t)
	g.Info.Auth = deploy.AuthInfo{Mode: "none"}
	g.Tokens["byd_secret"] = identity.User{ID: "token:deploy", Name: "deploy", Admin: true}
	return g
}

// cfEngine makes the fake runner act as node and npm for `backyard
// cloudflare`: node --version, npm installs (a package install creates a
// backyard-cloudflare package under its --prefix) and the engine itself.
type cfEngine struct {
	h           *harness
	nodeVersion string
	// run handles an engine run (node <entry> <command> …); env is its Env.
	run func(c Command, env map[string]string) error
	// install handles `npm install … --prefix <prefix> <tgz>`.
	install func(c Command, prefix string) error
}

func newCFEngine(h *harness) *cfEngine {
	e := &cfEngine{h: h, nodeVersion: "v22.11.0"}
	h.tools["node"], h.tools["npm"] = true, true
	h.runner.fn = e.handle
	return e
}

func (e *cfEngine) handle(c Command) error {
	switch {
	case c.Name == "node" && slices.Equal(c.Args, []string{"--version"}):
		fmt.Fprintln(c.Stdout, e.nodeVersion)
	case c.Name == "npm" && slices.Contains(c.Args, "--prefix"):
		prefix := c.Args[slices.Index(c.Args, "--prefix")+1]
		if e.install != nil {
			return e.install(c, prefix)
		}
		return fakePackageInstall(prefix, cloudflarePackageName, "1.2.3")
	case c.Name == "node" && e.run != nil:
		env := map[string]string{}
		for _, kv := range c.Env {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		return e.run(c, env)
	}
	return nil
}

// fakePackageInstall creates what npm installs from the package.
func fakePackageInstall(prefix, name, ver string) error {
	pkg := filepath.Join(prefix, "node_modules", "backyard-cloudflare")
	if err := os.MkdirAll(filepath.Join(pkg, "bin"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"`+name+`","version":"`+ver+`"}`), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(pkg, "bin", "backyard-cloudflare.mjs"), []byte("#!/usr/bin/env node\n"), 0o755)
}

// engineRuns returns the engine invocations the runner saw.
func engineRuns(h *harness) []Command {
	var runs []Command
	for _, c := range h.runner.Calls() {
		if c.Name == "node" && !slices.Equal(c.Args, []string{"--version"}) {
			runs = append(runs, c)
		}
	}
	return runs
}

// npmRuns returns the npm invocations the runner saw.
func npmRuns(h *harness) []Command {
	var runs []Command
	for _, c := range h.runner.Calls() {
		if c.Name == "npm" {
			runs = append(runs, c)
		}
	}
	return runs
}

// cfStateDir is where the engine keeps its state in the harness.
func cfStateDir(h *harness) string { return filepath.Join(h.home, ".config", "backyard", "cloudflare") }

// engineResult makes the engine save a state and write its result file.
// state may be nil (no state written); exit is the engine's exit code.
func engineResult(t *testing.T, e *cfEngine, res cloudflareResult, state map[string]any, exit int) {
	t.Helper()
	e.run = func(_ Command, env map[string]string) error {
		if res.StatePath == "" && res.Worker != "" {
			res.StatePath = filepath.Join(cfStateDir(e.h), res.Worker+".json")
		}
		if state != nil {
			if err := os.MkdirAll(filepath.Dir(res.StatePath), 0o700); err != nil {
				return err
			}
			b, _ := json.Marshal(state)
			if err := os.WriteFile(res.StatePath, b, 0o600); err != nil {
				return err
			}
		}
		if res.Schema == 0 {
			res.Schema = 1
		}
		b, _ := json.Marshal(res)
		if err := os.WriteFile(env["BACKYARD_CF_RESULT_FILE"], b, 0o600); err != nil {
			return err
		}
		if exit != 0 {
			return &ExitError{Code: exit, Err: fmt.Errorf("node failed: exit status %d", exit)}
		}
		return nil
	}
}

// withCloudflareRelease makes this binary a release build of v with the
// package digest sum.
func withCloudflareRelease(t *testing.T, v, sum string) {
	t.Helper()
	oldV, oldSum := version.Version, cloudflarePackageSHA256
	version.Version, cloudflarePackageSHA256 = v, sum
	t.Cleanup(func() { version.Version, cloudflarePackageSHA256 = oldV, oldSum })
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestSplitCloudflareArgs(t *testing.T) {
	for _, tc := range []struct {
		in   string
		fl   cloudflareFlags
		rest string
		help bool
	}{
		{in: "", rest: ""},
		{in: "--r2 --dir x --worker-name team", fl: cloudflareFlags{dir: "x"}, rest: "--r2 --worker-name team"},
		{in: "--dir=x --package=p.tgz --skip-install --no-login --profile edge -v --json",
			fl: cloudflareFlags{dir: "x", pkg: "p.tgz", skipInstall: true, noLogin: true, profile: "edge", verbose: true}, rest: "--json"},
		{in: "add backyard.acme.com --host-mode --package ./x.tgz --profile=edge --verbose",
			fl: cloudflareFlags{pkg: "./x.tgz", profile: "edge", verbose: true}, rest: "add backyard.acme.com --host-mode"},
		{in: "--worker-name team -- --dir x --help --server y", rest: "--worker-name team --dir x --help --server y"},
		{in: "-- --worker-name x", rest: "--worker-name x"},
		{in: "--var A:-- --yes -y", rest: "--var A:-- --yes -y"},
		{in: "enable --allow-email-domain acme.com -h", help: true},
		{in: "--help", help: true},
		{in: "--server x --skip-install=1 --help", help: true}, // help wins over every other problem
	} {
		fl, rest, help, err := splitCloudflareArgs(strings.Fields(tc.in))
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if help != tc.help || !help && (fl != tc.fl || strings.Join(rest, " ") != tc.rest) {
			t.Errorf("%q: flags %+v rest %q help %v; want %+v %q %v", tc.in, fl, rest, help, tc.fl, tc.rest, tc.help)
		}
	}
	for in, want := range map[string]string{
		"--server https://x":      "--server is not used by backyard cloudflare",
		"--gateway=https://x":     "--gateway is not used by backyard cloudflare",
		"--r2 --token t":          "--token is not used by backyard cloudflare",
		"--dir":                   "--dir needs a value",
		"--dir=":                  "--dir needs a value",
		"--package -- x.tgz":      "--package needs a value",
		"--skip-install=true":     "--skip-install takes no value",
		"--no-login=1 --server x": "--no-login takes no value",
		"--profile":               "--profile needs a value",
	} {
		if _, _, _, err := splitCloudflareArgs(strings.Fields(in)); err == nil || err.Error() != want {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
	// Every flag the help lists as this CLI's is handled by the splitter.
	for _, f := range cfGoFlags {
		args := []string{"--" + f.Name}
		if f.Value != "" {
			args = append(args, "v")
		}
		if fl, rest, _, err := splitCloudflareArgs(args); err != nil || len(rest) != 0 || fl == (cloudflareFlags{}) {
			t.Errorf("--%s: flags %+v rest %q err %v", f.Name, fl, rest, err)
		}
	}
}

func TestCloudflareHelpRunsNothing(t *testing.T) {
	h := newHarness(t)
	newCFEngine(h)
	for _, args := range [][]string{
		{"cloudflare", "setup", "--help"},
		{"cloudflare", "setup", "--dir", "nowhere", "-h"},
		{"cloudflare", "domain", "add", "x.acme.com", "--help"},
		{"cloudflare", "access", "enable", "-h"},
		{"cloudflare", "status", "--json", "--help"},
		{"help", "cloudflare", "domain"},
		{"cloudflare"},
		{"cloudflare", "--help"},
	} {
		out := h.mustRun(args...)
		if !strings.Contains(out, "backyard cloudflare") || !strings.Contains(out, "Usage:") {
			t.Errorf("%v: help = %s", args, out)
		}
	}
	if c := h.runner.Calls(); len(c) != 0 {
		t.Fatalf("help ran %+v", c)
	}
	// A group without (or with an unknown) subcommand: usage, exit 2, nothing run.
	if code := h.run("cloudflare", "domain"); code != 2 || !strings.Contains(h.out.String(), "domain (add <host> | remove)") {
		t.Fatalf("exit %d: %s", code, h.out.String())
	}
	if code := h.run("cloudflare", "access", "list"); code != 2 || !strings.Contains(h.err.String(), `"access list" is not a command: use access enable or access disable`) {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if code := h.run("cloudflare", "setup", "--server", "https://x"); code != 2 || !strings.Contains(h.err.String(), "--server is not used by backyard cloudflare") {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if code := h.run("cloudflare", "setup", "--dir", "a", "--package", "b.tgz"); code != 2 || !strings.Contains(h.err.String(), "cannot be combined") {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if c := h.runner.Calls(); len(c) != 0 {
		t.Fatalf("ran %+v", c)
	}
}

func TestCloudflareNeedsNode22(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "node not found") || !strings.Contains(errOut, "Node.js 22") ||
		!strings.Contains(errOut, "https://nodejs.org/") {
		t.Fatalf("stderr = %s", errOut)
	}
	e := newCFEngine(h)
	e.nodeVersion = "v20.11.0"
	if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "Node.js 22 or later is needed (found v20.11.0); https://nodejs.org/") {
		t.Fatalf("stderr = %s", errOut)
	}
	e.nodeVersion = "garbage"
	if errOut := h.mustFail("cloudflare", "status"); !strings.Contains(errOut, `node --version printed "garbage"`) {
		t.Fatalf("stderr = %s", errOut)
	}
	if len(npmRuns(h)) != 0 || len(engineRuns(h)) != 0 {
		t.Fatalf("calls = %+v", h.runner.Calls())
	}
	// npm is needed for npm ci (not with --skip-install).
	e.nodeVersion = "v24.1.0"
	h.tools["npm"] = false
	if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "npm not found in PATH") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.mustRun("cloudflare", "setup", "--skip-install")
	if len(engineRuns(h)) != 1 {
		t.Fatalf("calls = %+v", h.runner.Calls())
	}
}

func TestCloudflareCheckoutForwardsArguments(t *testing.T) {
	h := newHarness(t)
	cf := cfCheckout(t, h.cwd)
	newCFEngine(h)
	h.cwd = filepath.Join(h.cwd, "deploy")
	for _, tc := range []struct{ args, engine string }{
		{"setup --worker-name team", "setup --worker-name team"},
		{"setup -- --worker-name team", "setup --worker-name team"},
		{"domain add x.acme.com --host-mode", "domain add x.acme.com --host-mode"},
		{"domain remove --yes --no-login", "domain remove --yes"},
		{"access enable --allow-email-domain acme.com", "access enable --allow-email-domain acme.com"},
		{"access disable --delete-apps --profile edge", "access disable --delete-apps"},
		{"status --json -v", "status --json"},
		{"setup -- --help", "setup --help"},
	} {
		before := len(h.runner.Calls())
		h.mustRun(append([]string{"cloudflare"}, strings.Fields(tc.args)...)...)
		calls := h.runner.Calls()[before:]
		if len(calls) != 3 || calls[1].Name != "npm" || strings.Join(calls[1].Args, " ") != "ci" || calls[1].Dir != cf || calls[1].Stdout == nil {
			t.Fatalf("%s: calls = %+v", tc.args, calls)
		}
		run := calls[2]
		if run.Name != "node" || strings.Join(run.Args, " ") != "bin/backyard-cloudflare.mjs "+tc.engine || run.Dir != cf ||
			run.Stdin == nil || run.Stdout == nil || run.Stderr == nil {
			t.Fatalf("%s: engine = %+v", tc.args, run)
		}
	}
	if !strings.Contains(h.err.String(), "Using the Cloudflare engine in "+cf+" (checkout)") {
		t.Fatalf("stderr = %s", h.err.String())
	}
}

func TestCloudflareSetupIgnoresOtherProjects(t *testing.T) {
	h := newHarness(t)
	newCFEngine(h)
	// Another project's deploy/cloudflare: its npm scripts must not run.
	writeFiles(t, filepath.Join(h.cwd, "deploy", "cloudflare"), map[string]string{
		"package.json":                `{"name":"someone-elses-worker","scripts":{"setup":"rm -rf ~"}}`,
		"bin/backyard-cloudflare.mjs": "",
	})
	if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "no Cloudflare package pinned") {
		t.Fatalf("stderr = %s", errOut)
	}
	if len(npmRuns(h))+len(engineRuns(h)) != 0 {
		t.Fatalf("ran %+v", h.runner.Calls())
	}
	// An explicit --dir needs the setup script and the engine's entry point.
	writeFiles(t, filepath.Join(h.cwd, "nosetup"), map[string]string{"package.json": `{"name":"backyard-cloudflare"}`})
	if errOut := h.mustFail("cloudflare", "setup", "--dir", "nosetup"); !strings.Contains(errOut, `"setup" script`) {
		t.Fatalf("stderr = %s", errOut)
	}
	writeFiles(t, filepath.Join(h.cwd, "old"), map[string]string{"package.json": `{"name":"backyard-cloudflare","scripts":{"setup":"node scripts/setup.mjs"}}`})
	if errOut := h.mustFail("cloudflare", "setup", "--dir", "old"); !strings.Contains(errOut, "no bin/backyard-cloudflare.mjs: the checkout predates") {
		t.Fatalf("stderr = %s", errOut)
	}
	if len(npmRuns(h))+len(engineRuns(h)) != 0 {
		t.Fatalf("ran %+v", h.runner.Calls())
	}
	// The real package in this repository is recognized.
	if !isBackyardCloudflare(filepath.Join("..", "..", "deploy", "cloudflare")) {
		t.Fatal("deploy/cloudflare of this repository not recognized")
	}
	if profileNameFor("team-board") != "team-board" || profileNameFor("") != "cloudflare" || profileNameFor("a b") != "cloudflare" {
		t.Fatal("profileNameFor")
	}
}

// TestCloudflareReleaseIgnoresParentCheckouts: a release build runs its
// verified package, never a deploy/cloudflare that sits in a parent of the
// working directory (/tmp/deploy/cloudflare, C:\deploy\cloudflare, a
// "deploy" account's home); --dir still selects a checkout explicitly.
func TestCloudflareReleaseIgnoresParentCheckouts(t *testing.T) {
	h := newHarness(t)
	newCFEngine(h)
	sum := sha256Hex([]byte("the package of release 1.2.3"))
	withCloudflareRelease(t, "1.2.3", sum)
	installed := filepath.Join(h.cache, "backyard", "cloudflare-engine", sum)
	if err := fakePackageInstall(installed, cloudflarePackageName, "1.2.3"); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, installed, map[string]string{".complete": `{"sha256":"` + sum + `","version":"1.2.3"}`})
	planted := cfCheckout(t, h.cwd)
	h.cwd = filepath.Join(h.cwd, "alice", "work", "site")
	if err := os.MkdirAll(h.cwd, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{"status"}, {"setup", "--dry-run"}} {
		h.mustRun(append([]string{"cloudflare"}, args...)...)
	}
	if npm := npmRuns(h); len(npm) != 0 {
		t.Fatalf("npm ran: %+v", npm)
	}
	for _, run := range engineRuns(h) {
		if run.Args[0] != cloudflareInstalledEntry(installed) || run.Dir != h.cwd {
			t.Fatalf("engine = %+v", run)
		}
	}
	if strings.Contains(h.err.String(), planted) {
		t.Fatalf("stderr = %s", h.err.String())
	}

	h.mustRun("cloudflare", "status", "--dir", planted)
	runs := engineRuns(h)
	if run := runs[len(runs)-1]; run.Dir != planted || run.Args[0] != filepath.FromSlash(cloudflareEntry) {
		t.Fatalf("engine = %+v", run)
	}
	if npm := npmRuns(h); len(npm) != 1 || npm[0].Dir != planted {
		t.Fatalf("npm = %+v", npm)
	}
}

// TestFindCloudflareDirStaysInTheRepository: builds from source find the
// checkout they run in, but the search never leaves the repository of the
// working directory.
func TestFindCloudflareDirStaysInTheRepository(t *testing.T) {
	base := t.TempDir()
	outside := cfCheckout(t, base)
	proj := filepath.Join(base, "proj")
	writeFiles(t, proj, map[string]string{".git/HEAD": "ref: refs/heads/main\n", "site/index.html": "x"})
	for _, start := range []string{proj, filepath.Join(proj, "site")} {
		if got := findCloudflareDir(start); got != "" {
			t.Errorf("findCloudflareDir(%s) = %q, outside its repository", start, got)
		}
	}
	// The Backyard checkout itself (here a worktree, whose .git is a file).
	repo := filepath.Join(base, "backyard")
	cf := cfCheckout(t, repo)
	writeFiles(t, repo, map[string]string{".git": "gitdir: /elsewhere\n", "internal/cli/x.go": "package cli"})
	for _, start := range []string{repo, filepath.Join(repo, "internal", "cli"), cf, filepath.Join(cf, "src")} {
		if got := findCloudflareDir(start); got != cf {
			t.Errorf("findCloudflareDir(%s) = %q, want %s", start, got, cf)
		}
	}
	// Outside any repository the search goes up.
	if got := findCloudflareDir(filepath.Join(base, "loose", "dir")); got != outside {
		t.Errorf("findCloudflareDir = %q, want %s", got, outside)
	}
}

// TestCloudflareCheckoutSkipsCurrentDependencies: npm ci deletes
// node_modules (and with it a running engine's wrangler, or `wrangler dev`):
// it runs only when node_modules is missing or older than package-lock.json.
func TestCloudflareCheckoutSkipsCurrentDependencies(t *testing.T) {
	h := newHarness(t)
	cf := cfCheckout(t, h.cwd)
	newCFEngine(h)
	writeFiles(t, cf, map[string]string{"node_modules/.package-lock.json": "{}"})
	lock, hidden := filepath.Join(cf, "package-lock.json"), filepath.Join(cf, "node_modules", ".package-lock.json")
	touch := func(path string, at time.Time) {
		t.Helper()
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	touch(lock, now.Add(-time.Hour))
	touch(hidden, now)

	h.tools["npm"] = false // not even needed
	for _, args := range [][]string{{"status"}, {"setup", "--", "--help"}, {"domain", "remove"}} {
		h.mustRun(append([]string{"cloudflare"}, args...)...)
	}
	h.tools["npm"] = true
	if len(npmRuns(h)) != 0 || len(engineRuns(h)) != 3 || !strings.Contains(h.err.String(), "npm ci skipped") {
		t.Fatalf("calls = %+v; stderr = %s", h.runner.Calls(), h.err.String())
	}

	// package-lock.json changed since the last install (git pull): npm ci.
	touch(lock, now.Add(time.Minute))
	h.mustRun("cloudflare", "status")
	if npm := npmRuns(h); len(npm) != 1 || strings.Join(npm[0].Args, " ") != "ci" || npm[0].Dir != cf {
		t.Fatalf("npm = %+v", npm)
	}
	// No node_modules at all: npm ci.
	if err := os.RemoveAll(filepath.Join(cf, "node_modules")); err != nil {
		t.Fatal(err)
	}
	h.mustRun("cloudflare", "status")
	if len(npmRuns(h)) != 2 {
		t.Fatalf("calls = %+v", h.runner.Calls())
	}
}

func TestCloudflareEngineEnvironment(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)
	var env map[string]string
	e.run = func(_ Command, en map[string]string) error {
		env = en
		if _, err := os.Stat(filepath.Dir(en["BACKYARD_CF_RESULT_FILE"])); err != nil {
			return err
		}
		return nil
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join("rel", "cfg"))
	t.Setenv("BACKYARD_CF_API_TOKEN", "admin-secret")
	t.Setenv("DEPLOY_TOKEN", "byd_secret")
	t.Setenv("CLOUDFLARE_API_TOKEN", "cf-secret")
	h.mustRun("cloudflare", "status")
	// npm (and the install scripts it runs) never sees the Cloudflare
	// credentials; the engine inherits them.
	if npm := npmRuns(h); len(npm) != 1 || !slices.Contains(npm[0].Env, "BACKYARD_CF_API_TOKEN=") ||
		!slices.Contains(npm[0].Env, "DEPLOY_TOKEN=") || !slices.Contains(npm[0].Env, "CLOUDFLARE_API_TOKEN=") {
		t.Fatalf("npm = %+v", npm)
	}
	if _, set := env["BACKYARD_CF_API_TOKEN"]; set {
		t.Fatalf("env = %v", env)
	}
	result := env["BACKYARD_CF_RESULT_FILE"]
	if !filepath.IsAbs(result) || filepath.Base(result) != "result.json" || env["BACKYARD_CF_INVOKED_AS"] != "backyard cloudflare" ||
		env["XDG_CONFIG_HOME"] != filepath.Join(h.cwd, "rel", "cfg") || env["BACKYARD_CF_CLI_LOGIN"] != "1" {
		t.Fatalf("env = %v", env)
	}
	if _, err := os.Stat(filepath.Dir(result)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the private temp dir was not removed: %v", err)
	}
	// An absolute XDG_CONFIG_HOME is inherited as is.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(h.home, ".config"))
	h.mustRun("cloudflare", "status", "--skip-install")
	if _, ok := env["XDG_CONFIG_HOME"]; ok {
		t.Fatalf("env = %v", env)
	}
	// With --no-login this CLI logs nothing in, so the engine prints its own
	// `backyard login` line (no BACKYARD_CF_CLI_LOGIN).
	h.mustRun("cloudflare", "setup", "--skip-install", "--no-login")
	if _, ok := env["BACKYARD_CF_CLI_LOGIN"]; ok {
		t.Fatalf("env = %v", env)
	}
}

func TestCloudflarePackageMode(t *testing.T) {
	tgz := []byte("backyard-cloudflare 1.2.3 (fake tgz)")
	sum := sha256Hex(tgz)
	var hits atomic.Int32
	var gotUA atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotUA.Store(r.UserAgent())
		switch r.URL.Path {
		case "/v1.2.3/backyard-cloudflare-1.2.3.tgz":
			_, _ = w.Write(tgz)
		case "/redirect":
			http.Redirect(w, r, "http://example.com/backyard-cloudflare.tgz", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	h := newHarness(t)
	h.releaseBase = srv.URL + "/"
	e := newCFEngine(h)
	var tgzAtInstall []byte
	e.install = func(c Command, prefix string) error {
		tgzAtInstall, _ = os.ReadFile(filepath.Join(prefix, "backyard-cloudflare.tgz"))
		return fakePackageInstall(prefix, cloudflarePackageName, "1.2.3")
	}
	withCloudflareRelease(t, "1.2.3", sum)
	root := filepath.Join(h.cache, "backyard", "cloudflare-engine")
	dir := filepath.Join(root, sum)
	entry := filepath.Join(dir, "node_modules", "backyard-cloudflare", "bin", "backyard-cloudflare.mjs")

	h.mustRun("cloudflare", "setup", "--worker-name", "team")
	npm := npmRuns(h)
	if len(npm) != 1 || hits.Load() != 1 || gotUA.Load() != "backyard-cli/1.2.3" || string(tgzAtInstall) != string(tgz) {
		t.Fatalf("npm = %+v, hits %d, ua %v", npm, hits.Load(), gotUA.Load())
	}
	tmp := npm[0].Dir
	if filepath.Dir(tmp) != root || !strings.HasPrefix(filepath.Base(tmp), ".install-") ||
		strings.Join(npm[0].Args, " ") != "install --no-audit --no-fund --no-save --omit=dev --prefix "+tmp+" "+filepath.Join(tmp, "backyard-cloudflare.tgz") {
		t.Fatalf("npm = %+v", npm[0])
	}
	if _, err := os.Stat(tmp); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the install dir was not moved: %v", err)
	}
	var complete cloudflareComplete
	if b, err := os.ReadFile(filepath.Join(dir, ".complete")); err != nil || json.Unmarshal(b, &complete) != nil ||
		complete.SHA256 != sum || complete.Version != "1.2.3" || complete.InstalledAt.IsZero() {
		t.Fatalf(".complete = %+v (%v)", complete, err)
	}
	runs := engineRuns(h)
	if len(runs) != 1 || strings.Join(runs[0].Args, " ") != entry+" setup --worker-name team" || runs[0].Dir != h.cwd {
		t.Fatalf("engine = %+v", runs)
	}

	// The second run uses the installed engine: no download, no npm.
	h.mustRun("cloudflare", "domain", "add", "x.acme.com", "--skip-install")
	if hits.Load() != 1 || len(npmRuns(h)) != 1 || len(engineRuns(h)) != 2 || !strings.Contains(h.err.String(), "--skip-install applies to a checkout only") {
		t.Fatalf("hits %d, calls %+v, stderr %s", hits.Load(), h.runner.Calls(), h.err.String())
	}

	// --package with the release's own bytes reuses the install too.
	writeFiles(t, h.cwd, map[string]string{"pkg/same.tgz": string(tgz)})
	h.mustRun("cloudflare", "status", "--package", filepath.Join("pkg", "same.tgz"))
	if len(npmRuns(h)) != 1 || !strings.Contains(h.err.String(), "sha256 "+sum+": the package of this CLI release") {
		t.Fatalf("calls %+v, stderr %s", h.runner.Calls(), h.err.String())
	}

	// A local package of its own: installed under its digest, unverified.
	other := []byte("a locally built package")
	writeFiles(t, h.cwd, map[string]string{"pkg/local.tgz": string(other)})
	h.mustRun("cloudflare", "setup", "--package", "./pkg/local.tgz")
	if !strings.Contains(h.err.String(), "Using ./pkg/local.tgz (sha256 "+sha256Hex(other)+"; not verified against this CLI release)") ||
		!cloudflareInstalled(filepath.Join(root, sha256Hex(other)), sha256Hex(other)) || len(npmRuns(h)) != 2 {
		t.Fatalf("stderr = %s", h.err.String())
	}
	if run := engineRuns(h)[3]; run.Args[0] != cloudflareInstalledEntry(filepath.Join(root, sha256Hex(other))) || run.Dir != h.cwd {
		t.Fatalf("engine = %+v", run)
	}

	// Refused before anything is fetched or run.
	for spec, want := range map[string]string{
		"http://example.com/backyard-cloudflare.tgz": "refusing to download http://example.com/backyard-cloudflare.tgz over plain http",
		"file:///tmp/x.tgz":                          "only a local file or an https URL",
		"https://user:pw@example.com/x.tgz":          "credentials are not supported",
		"./missing.tgz":                              "missing.tgz",
		srv.URL + "/redirect":                        "refusing to download http://example.com/backyard-cloudflare.tgz",
		srv.URL + "/v9.9.9/x.tgz":                    "HTTP 404",
	} {
		before := len(h.runner.Calls())
		if errOut := h.mustFail("cloudflare", "setup", "--package", spec); !strings.Contains(errOut, want) {
			t.Errorf("--package %s: stderr = %s", spec, errOut)
		}
		if c := h.runner.Calls()[before:]; len(c) != 1 { // node --version only
			t.Errorf("--package %s ran %+v", spec, c)
		}
	}
}

func TestCloudflarePackageOverHTTPS(t *testing.T) {
	tgz := []byte("served over https")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	defer srv.Close()
	h := newHarness(t)
	h.httpClient = srv.Client()
	newCFEngine(h)
	h.mustRun("cloudflare", "setup", "--package", srv.URL+"/backyard-cloudflare-2.0.0.tgz")
	if dir := filepath.Join(h.cache, "backyard", "cloudflare-engine", sha256Hex(tgz)); !cloudflareInstalled(dir, sha256Hex(tgz)) ||
		len(engineRuns(h)) != 1 || engineRuns(h)[0].Args[0] != cloudflareInstalledEntry(dir) {
		t.Fatalf("calls = %+v; stderr = %s", h.runner.Calls(), h.err.String())
	}
}

// TestCloudflarePrunesOldEngines: a fresh install removes older engines
// from the cache, keeping the previous one and this release's, and the
// leftovers of killed runs; reusing an install touches nothing.
func TestCloudflarePrunesOldEngines(t *testing.T) {
	release := []byte("backyard-cloudflare 1.2.3")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(release) }))
	defer srv.Close()
	h := newHarness(t)
	h.releaseBase = srv.URL + "/"
	newCFEngine(h)
	sum := sha256Hex(release)
	withCloudflareRelease(t, "1.2.3", sum)
	root := filepath.Join(h.cache, "backyard", "cloudflare-engine")
	now := h.now
	stale := func(name string) {
		t.Helper()
		old := now.Add(-48 * time.Hour)
		if err := os.Chtimes(filepath.Join(root, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	// The engines of three earlier releases, a broken leftover (no
	// .complete), the install dir of a killed run and one in progress.
	var olds []string
	for i, age := range []time.Duration{72 * time.Hour, 48 * time.Hour, 24 * time.Hour} {
		s := sha256Hex(fmt.Appendf(nil, "engine %d", i))
		olds = append(olds, s)
		writeFiles(t, filepath.Join(root, s), map[string]string{
			".complete": fmt.Sprintf(`{"sha256":%q,"version":"1.0.%d","installedAt":%q}`, s, i, now.Add(-age).UTC().Format(time.RFC3339Nano)),
		})
	}
	broken := sha256Hex([]byte("broken"))
	writeFiles(t, filepath.Join(root, broken), map[string]string{"node_modules/half-written": "x"})
	writeFiles(t, root, map[string]string{".install-killed/backyard-cloudflare.tgz": "x", ".install-running/backyard-cloudflare.tgz": "x"})
	stale(broken)
	stale(".install-killed")
	want := func(names ...string) {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		slices.Sort(got)
		slices.Sort(names)
		if !slices.Equal(got, names) {
			t.Fatalf("cache = %v, want %v", got, names)
		}
	}

	h.mustRun("cloudflare", "setup")
	want(sum, olds[2], ".install-running")

	// Reusing the installed engine removes nothing.
	writeFiles(t, root, map[string]string{".install-killed-2/x": "x"})
	stale(".install-killed-2")
	h.mustRun("cloudflare", "status")
	want(sum, olds[2], ".install-running", ".install-killed-2")

	// A --package build: the previous engine (this release's) stays.
	writeFiles(t, h.cwd, map[string]string{"a.tgz": "package a", "b.tgz": "package b"})
	a, b := sha256Hex([]byte("package a")), sha256Hex([]byte("package b"))
	h.now = now.Add(time.Hour)
	h.mustRun("cloudflare", "setup", "--package", "a.tgz")
	want(a, sum, ".install-running")
	// The next one keeps the previous (a) and this release's package.
	h.now = now.Add(2 * time.Hour)
	h.mustRun("cloudflare", "setup", "--package", "b.tgz")
	want(b, a, sum, ".install-running")
	if len(npmRuns(h)) != 3 {
		t.Fatalf("npm = %+v", npmRuns(h))
	}
}

func TestCloudflarePackageFailures(t *testing.T) {
	served := []byte("the bytes on the server")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) }))
	defer srv.Close()
	setup := func(t *testing.T) (*harness, *cfEngine, string) {
		h := newHarness(t)
		h.releaseBase = srv.URL + "/"
		return h, newCFEngine(h), filepath.Join(h.cache, "backyard", "cloudflare-engine")
	}

	t.Run("development build without a checkout", func(t *testing.T) {
		h, _, _ := setup(t)
		withCloudflareRelease(t, "0.1.0-dev", "")
		code := h.run("cloudflare", "setup")
		if errOut := h.err.String(); code != 2 || !strings.Contains(errOut, "this development build has no Cloudflare package pinned") ||
			!strings.Contains(errOut, "--dir <checkout>/deploy/cloudflare") || !strings.Contains(errOut, "--package <path or https URL") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		// The ways out that need no checkout: a release build, npx, a release's package.
		for _, want := range []string{"`go install`", "https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh",
			"npx backyard-cloudflare@latest setup", "backyard login --server <hub> --token <token>",
			srv.URL + "/v<v>/backyard-cloudflare-<v>.tgz"} {
			if errOut := h.err.String(); !strings.Contains(errOut, want) {
				t.Errorf("the error does not mention %q: %s", want, errOut)
			}
		}
		if len(npmRuns(h))+len(engineRuns(h)) != 0 {
			t.Fatalf("calls = %+v", h.runner.Calls())
		}
	})

	t.Run("digest mismatch", func(t *testing.T) {
		h, _, root := setup(t)
		withCloudflareRelease(t, "1.2.3", sha256Hex([]byte("what the release pinned")))
		if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "integrity check failed") ||
			!strings.Contains(errOut, "its sha256 is "+sha256Hex(served)) || !strings.Contains(errOut, "nothing was installed") {
			t.Fatalf("stderr = %s", errOut)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 || len(npmRuns(h))+len(engineRuns(h)) != 0 {
			t.Fatalf("installed %v, calls %+v", entries, h.runner.Calls())
		}
	})

	t.Run("npm is checked before downloading", func(t *testing.T) {
		h, _, _ := setup(t)
		withCloudflareRelease(t, "1.2.3", sha256Hex(served))
		h.tools["npm"] = false
		if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "npm not found in PATH") {
			t.Fatalf("stderr = %s", errOut)
		}
	})

	t.Run("not a backyard-cloudflare package", func(t *testing.T) {
		h, e, root := setup(t)
		withCloudflareRelease(t, "1.2.3", sha256Hex(served))
		e.install = func(_ Command, prefix string) error { return fakePackageInstall(prefix, "something-else", "1.0.0") }
		if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "is not a backyard-cloudflare package") {
			t.Fatalf("stderr = %s", errOut)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 || len(engineRuns(h)) != 0 {
			t.Fatalf("installed %v, calls %+v", entries, h.runner.Calls())
		}
		e.install = func(Command, string) error { return &ExitError{Code: 1, Err: os.ErrPermission} }
		if errOut := h.mustFail("cloudflare", "setup"); !strings.Contains(errOut, "npm install") {
			t.Fatalf("stderr = %s", errOut)
		}
		if entries, _ := os.ReadDir(root); len(entries) != 0 {
			t.Fatalf("left %v", entries)
		}
	})

	t.Run("a concurrent install wins the race", func(t *testing.T) {
		h, e, root := setup(t)
		sum := sha256Hex(served)
		withCloudflareRelease(t, "1.2.3", sum)
		target := filepath.Join(root, sum)
		e.install = func(_ Command, prefix string) error {
			// Another run finishes first, while this one installs.
			if err := fakePackageInstall(target, cloudflarePackageName, "1.2.3"); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(target, ".complete"), []byte(`{"sha256":"`+sum+`","version":"1.2.3"}`), 0o644); err != nil {
				return err
			}
			return fakePackageInstall(prefix, cloudflarePackageName, "1.2.3")
		}
		h.mustRun("cloudflare", "setup")
		entries, _ := os.ReadDir(root)
		if len(entries) != 1 || entries[0].Name() != sum || engineRuns(h)[0].Args[0] != cloudflareInstalledEntry(target) {
			t.Fatalf("cache = %v, calls %+v", entries, h.runner.Calls())
		}
	})

	t.Run("a broken leftover is replaced", func(t *testing.T) {
		h, _, root := setup(t)
		sum := sha256Hex(served)
		withCloudflareRelease(t, "1.2.3", sum)
		writeFiles(t, filepath.Join(root, sum), map[string]string{"node_modules/half-written": "x"})
		h.mustRun("cloudflare", "setup")
		if !cloudflareInstalled(filepath.Join(root, sum), sum) || fileExists(filepath.Join(root, sum, "node_modules", "half-written")) {
			t.Fatal("leftover not replaced")
		}
	})
}

func TestCloudflareReleaseBase(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "https://github.com/thasami/backyard/releases/download/",
		"https://github.example.com": "https://github.example.com/thasami/backyard/releases/download/",
		"http://127.0.0.1:9/ ":       "http://127.0.0.1:9/thasami/backyard/releases/download/",
	} {
		if got := cloudflareReleaseBase(in); got != want {
			t.Errorf("cloudflareReleaseBase(%q) = %q", in, got)
		}
	}
	h := newHarness(t)
	h.releaseBase = cloudflareReleaseBase("")
	withCloudflareRelease(t, "v1.2.3", strings.Repeat("a", 64))
	a := &cliApp{deps: h.deps()}
	if got := a.cloudflareReleaseURL(); got != "https://github.com/thasami/backyard/releases/download/v1.2.3/backyard-cloudflare-1.2.3.tgz" {
		t.Fatalf("release URL = %s", got)
	}
}

// setupResult is the result file of a setup of worker at url.
func setupResult(worker, url string) cloudflareResult {
	return cloudflareResult{Command: "setup", Worker: worker, URL: url, WorkersDevURL: url, AuthMode: "none", DeployToken: "state"}
}

func TestCloudflareSetupLogsTheCLIIn(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)
	g := cloudflareWorker(t)
	engineResult(t, e, setupResult("backyard", g.URL+"/"), map[string]any{"url": g.URL + "/", "deployToken": "byd_secret"}, 0)

	h.mustRun("cloudflare", "setup")
	// The engine leaves its login line and next step out (BACKYARD_CF_CLI_LOGIN): they are here.
	if !strings.Contains(h.err.String(), "now logged in to "+g.URL) || !strings.Contains(h.err.String(), "Next: "+cloudflareNextStep) {
		t.Fatalf("stderr = %s", h.err.String())
	}
	f := h.loadConfig()
	p := f.Profile("default")
	if f.Current != "default" || p == nil || p.Server != g.URL || p.Token != "byd_secret" || p.Login != clientconfig.LoginToken ||
		p.User == nil || p.User.ID != "token:deploy" || p.Info == nil || p.Info.Auth.Mode != "none" {
		t.Fatalf("config = %+v / %+v", f, p)
	}

	// The OpenQuick flow: init, then deploy with no login step.
	writeFiles(t, h.cwd, map[string]string{"site/index.html": "<title>x</title>"})
	h.mustRun("deploy", "site", "--site", "lunch")
	if m, ok := g.Meta("lunch"); !ok || m.UpdatedBy != "token:deploy" {
		t.Fatalf("site not deployed with the deploy token: %+v", m)
	}

	// A profile for another server is kept: the Worker gets its own profile.
	h.writeProfile("default", &clientconfig.Profile{Server: "https://backyard.acme.com", Token: "corp"}, true)
	engineResult(t, e, setupResult("team-board", g.URL), map[string]any{"url": g.URL, "deployToken": "byd_secret"}, 0)
	h.mustRun("cloudflare", "setup", "--", "--worker-name", "team-board")
	f = h.loadConfig()
	if f.Current != "team-board" || f.Profile("team-board").Server != g.URL || f.Profile("default").Token != "corp" {
		t.Fatalf("config = %+v", f)
	}

	// --profile picks the profile; --no-login skips the login.
	h.mustRun("cloudflare", "setup", "--profile", "edge", "--", "--name=team-board")
	if f = h.loadConfig(); f.Current != "edge" || f.Profile("edge").Token != "byd_secret" {
		t.Fatalf("config = %+v", f)
	}
	h.writeProfile("default", &clientconfig.Profile{Server: "https://backyard.acme.com", Token: "corp"}, true)
	h.mustRun("cloudflare", "setup", "--no-login")
	if f = h.loadConfig(); f.Current != "default" {
		t.Fatalf("--no-login changed the current profile: %+v", f)
	}

	// The token from $DEPLOY_TOKEN.
	t.Setenv("DEPLOY_TOKEN", "byd_secret")
	res := setupResult("env-worker", g.URL)
	res.DeployToken = "env"
	engineResult(t, e, res, nil, 0)
	h.mustRun("cloudflare", "setup", "--profile", "from-env")
	if p := h.loadConfig().Profile("from-env"); p == nil || p.Token != "byd_secret" {
		t.Fatalf("profile = %+v", p)
	}
}

func TestCloudflareSetupLoginEdgeCases(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)

	// No result file (--help, --dry-run, status): no login, no error, even
	// with state files around.
	writeFiles(t, cfStateDir(h), map[string]string{"old.json": `{"url":"https://old.example.workers.dev","deployToken":"x"}`})
	for _, args := range [][]string{{"setup", "--", "--help"}, {"setup", "--dry-run"}, {"status"}} {
		h.mustRun(append([]string{"cloudflare"}, args...)...)
	}
	if f := h.loadConfig(); len(f.Profiles) != 0 {
		t.Fatalf("profiles = %+v", f.Profiles)
	}

	// The server rejects the token: warn, keep the config untouched, and say
	// how to log in by hand (the engine printed no login line of its own).
	g := cloudflareWorker(t)
	engineResult(t, e, setupResult("backyard", g.URL), map[string]any{"url": g.URL, "deployToken": "wrong"}, 0)
	h.mustRun("cloudflare", "setup")
	statePath := filepath.Join(cfStateDir(h), "backyard.json")
	if errOut := h.err.String(); !strings.Contains(errOut, "did not accept its deploy token") ||
		!strings.Contains(errOut, "backyard login --server "+g.URL+` --token <"deployToken" in `+statePath+">") ||
		strings.Contains(errOut, "printed above") || strings.Contains(errOut, "Next:") || strings.Contains(errOut, "wrong") {
		t.Fatalf("stderr = %s", errOut)
	}
	if f := h.loadConfig(); len(f.Profiles) != 0 {
		t.Fatalf("profiles = %+v", f.Profiles)
	}

	// Not answering yet (workers.dev propagation): saved with a warning.
	notYet := httptest.NewServer(http.NotFoundHandler())
	defer notYet.Close()
	engineResult(t, e, setupResult("backyard", notYet.URL), map[string]any{"url": notYet.URL, "deployToken": "byd_secret"}, 0)
	h.mustRun("cloudflare", "setup")
	if !strings.Contains(h.err.String(), "not answering yet") {
		t.Fatalf("stderr = %s", h.err.String())
	}
	if p := h.loadConfig().Profile("default"); p == nil || p.Server != notYet.URL || p.Token != "byd_secret" {
		t.Fatalf("profile = %+v", p)
	}

	// No URL, or no known token: point at the login command.
	engineResult(t, e, setupResult("backyard", ""), map[string]any{"deployToken": "byd_secret"}, 0)
	h.mustRun("cloudflare", "setup")
	if errOut := h.err.String(); !strings.Contains(errOut, `backyard login --server <hub URL> --token <"deployToken" in `+statePath+">") ||
		!strings.Contains(errOut, "`backyard cloudflare status` shows the hub URL") || strings.Contains(errOut, "printed above") {
		t.Fatalf("stderr = %s", errOut)
	}
	res := setupResult("backyard", g.URL)
	res.DeployToken = "unknown"
	engineResult(t, e, res, nil, 0)
	h.mustRun("cloudflare", "setup")
	if !strings.Contains(h.err.String(), "backyard login --server "+g.URL+" --token <DEPLOY_TOKEN>") {
		t.Fatalf("stderr = %s", h.err.String())
	}

	// A result of an unknown schema: warn, change nothing, say how to log in.
	res = setupResult("backyard", g.URL)
	res.Schema = 2
	engineResult(t, e, res, map[string]any{"url": g.URL, "deployToken": "byd_secret"}, 0)
	h.mustRun("cloudflare", "setup")
	if errOut := h.err.String(); !strings.Contains(errOut, "schema 2") || h.loadConfig().Profile("default").Server != notYet.URL ||
		!strings.Contains(errOut, "backyard login --server <hub URL> --token <DEPLOY_TOKEN>") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestCloudflareExitCodes(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)
	g := cloudflareWorker(t)
	state := map[string]any{"url": g.URL, "deployToken": "byd_secret"}

	// 3 (refused): the engine's code, no login, no message of our own.
	engineResult(t, e, setupResult("backyard", g.URL), state, 3)
	if code := h.run("cloudflare", "setup"); code != 3 || strings.Contains(h.err.String(), "Error:") {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if f := h.loadConfig(); len(f.Profiles) != 0 {
		t.Fatalf("profiles = %+v", f.Profiles)
	}
	for _, code := range []int{1, 2, 130} {
		engineResult(t, e, setupResult("backyard", g.URL), state, code)
		if got := h.run("cloudflare", "setup"); got != code || len(h.loadConfig().Profiles) != 0 {
			t.Fatalf("exit %d, want %d", got, code)
		}
	}
	// 4 (applied, verification pending): logged in, then exit 4.
	engineResult(t, e, setupResult("backyard", g.URL), state, 4)
	if code := h.run("cloudflare", "setup"); code != 4 || !strings.Contains(h.err.String(), "now logged in") {
		t.Fatalf("exit %d: %s", code, h.err.String())
	}
	if p := h.loadConfig().Profile("default"); p == nil || p.Token != "byd_secret" {
		t.Fatalf("profile = %+v", p)
	}
	// The engine could not be started at all.
	e.run = func(Command, map[string]string) error { return os.ErrNotExist }
	if errOut := h.mustFail("cloudflare", "status"); !strings.Contains(errOut, "run backyard-cloudflare") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestCloudflareDomainMovesProfiles(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)
	g := cloudflareWorker(t)  // the Worker on its custom domain
	wd := cloudflareWorker(t) // the same Worker on workers.dev
	workersDev := wd.URL
	h.writeProfile("default", &clientconfig.Profile{Server: workersDev, Token: "byd_secret", Login: clientconfig.LoginToken,
		Info: &deploy.Info{PublicURL: workersDev}}, true)
	h.writeProfile("ci", &clientconfig.Profile{Server: workersDev + "/", Gateway: workersDev + "/__backyard/api/deploy", Token: "byd_ci",
		Login: clientconfig.LoginToken, Method: "api"}, false)
	h.writeProfile("corp", &clientconfig.Profile{Server: "https://backyard.corp.example", Token: "corp"}, false)
	engineResult(t, e, cloudflareResult{Command: "domain add", Worker: "backyard", URL: g.URL, PreviousURL: workersDev,
		WorkersDevURL: workersDev, AuthMode: "none", DeployToken: "state"}, map[string]any{"url": g.URL, "deployToken": "byd_secret"}, 0)

	h.mustRun("cloudflare", "domain", "add", "backyard.acme.com")
	f := h.loadConfig()
	def, ci, corp := f.Profile("default"), f.Profile("ci"), f.Profile("corp")
	if def.Server != g.URL || def.Token != "byd_secret" || def.Info == nil || def.Info.PublicURL != g.Info.PublicURL || f.Current != "default" {
		t.Fatalf("default = %+v", def)
	}
	if ci.Server != g.URL || ci.Gateway != "" || ci.Token != "byd_ci" || ci.Info != nil || ci.Method != "api" {
		t.Fatalf("ci = %+v", ci)
	}
	if corp.Server != "https://backyard.corp.example" || corp.Token != "corp" {
		t.Fatalf("corp = %+v", corp)
	}
	// The next step (init, deploy) belongs to setup, not to a domain change.
	if !strings.Contains(h.err.String(), `Profile "ci" now points to `+g.URL) || !strings.Contains(h.err.String(), "now logged in to "+g.URL) ||
		strings.Contains(h.err.String(), "Next:") {
		t.Fatalf("stderr = %s", h.err.String())
	}

	// domain remove moves them back; --no-login leaves every profile alone.
	engineResult(t, e, cloudflareResult{Command: "domain remove", Worker: "backyard", URL: workersDev, PreviousURL: g.URL,
		WorkersDevURL: workersDev, AuthMode: "none", DeployToken: "state"}, map[string]any{"url": workersDev, "deployToken": "byd_secret"}, 0)
	h.mustRun("cloudflare", "domain", "remove", "--no-login")
	if f := h.loadConfig(); f.Profile("ci").Server != g.URL || f.Profile("default").Server != g.URL {
		t.Fatalf("--no-login changed profiles: %+v", f.Profiles)
	}
	h.mustRun("cloudflare", "domain", "remove")
	if f := h.loadConfig(); f.Profile("ci").Server != workersDev || f.Profile("ci").Token != "byd_ci" ||
		f.Profile("default").Server != workersDev || f.Profile("default").Token != "byd_secret" {
		t.Fatalf("profiles = %+v", f.Profiles)
	}
}

// TestCloudflareKeepsTheCurrentProfileOfAnotherServer: a domain change or
// a setup re-run refreshes the Worker's own profile, but does not switch the
// CLI away from a current profile for another server (the next `backyard
// deploy` of a company project must not go to the personal Worker).
func TestCloudflareKeepsTheCurrentProfileOfAnotherServer(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)
	g := cloudflareWorker(t)  // the Worker on its custom domain
	wd := cloudflareWorker(t) // the same Worker on workers.dev
	h.writeProfile("corp", &clientconfig.Profile{Server: "https://backyard.corp.example", Login: clientconfig.LoginBrowser, Token: "corp"}, true)
	h.writeProfile("backyard", &clientconfig.Profile{Server: wd.URL, Token: "byd_secret", Login: clientconfig.LoginToken}, false)
	state := map[string]any{"url": g.URL, "deployToken": "byd_secret"}

	engineResult(t, e, cloudflareResult{Command: "domain add", Worker: "backyard", URL: g.URL, PreviousURL: wd.URL,
		WorkersDevURL: wd.URL, AuthMode: "none", DeployToken: "state"}, state, 0)
	h.mustRun("cloudflare", "domain", "add", "backyard.acme.com")
	f := h.loadConfig()
	if p := f.Profile("backyard"); f.Current != "corp" || p.Server != g.URL || p.Token != "byd_secret" || p.Info == nil ||
		p.User == nil || p.User.ID != "token:deploy" || f.Profile("corp").Token != "corp" {
		t.Fatalf("config = %+v; backyard = %+v", f, p)
	}
	if !strings.Contains(h.err.String(), `Updated profile "backyard"`) || !strings.Contains(h.err.String(), `the current profile is still "corp"`) {
		t.Fatalf("stderr = %s", h.err.String())
	}

	// A setup re-run (an upgrade) does the same, with no "Next: … backyard
	// deploy" that would deploy to the current, other server.
	engineResult(t, e, setupResult("backyard", g.URL), state, 0)
	h.mustRun("cloudflare", "setup")
	if f = h.loadConfig(); f.Current != "corp" || f.Profile("backyard").Server != g.URL || strings.Contains(h.err.String(), "Next:") {
		t.Fatalf("config = %+v; stderr = %s", f, h.err.String())
	}
	// An explicit --profile still switches, as does a first login of another Worker.
	h.mustRun("cloudflare", "setup", "--profile", "backyard")
	if f = h.loadConfig(); f.Current != "backyard" {
		t.Fatalf("config = %+v", f)
	}
	h.writeProfile("corp", f.Profile("corp"), true)
	engineResult(t, e, setupResult("team-board", wd.URL), map[string]any{"url": wd.URL, "deployToken": "byd_secret"}, 0)
	h.mustRun("cloudflare", "setup", "--", "--worker-name", "team-board")
	if f = h.loadConfig(); f.Current != "team-board" || f.Profile("team-board").Server != wd.URL {
		t.Fatalf("config = %+v", f)
	}
}

// TestCloudflareAccessKeepsPersonalTokenLogins: with Access on, a personal
// API token pasted from the Worker's CLI token page (`backyard login --server
// <hub> --token <t>`) is a personal login too; an explicitly named profile
// is still logged in with the deploy token.
func TestCloudflareAccessKeepsPersonalTokenLogins(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)
	g := cloudflareWorker(t)
	g.Info.Auth = deploy.AuthInfo{Mode: "access"}
	g.Tokens["alice-token"] = alice
	h.writeProfile("default", &clientconfig.Profile{Server: g.URL, Token: "alice-token", Login: clientconfig.LoginToken,
		User: &clientconfig.User{ID: alice.ID, Email: alice.Email, Name: alice.Name}}, true)
	res := setupResult("backyard", g.URL)
	res.AuthMode = "access"
	engineResult(t, e, res, map[string]any{"url": g.URL, "deployToken": "byd_secret"}, 0)

	h.mustRun("cloudflare", "setup")
	if p := h.loadConfig().Profile("default"); p.Token != "alice-token" || p.User == nil || p.User.ID != alice.ID ||
		!strings.Contains(h.err.String(), `Kept profile "default"`) || !strings.Contains(h.err.String(), "Next: "+cloudflareNextStep) {
		t.Fatalf("default = %+v; stderr = %s", p, h.err.String())
	}

	// --profile ci: ci gets the deploy token, the personal login stays.
	h.mustRun("cloudflare", "setup", "--profile", "ci")
	f := h.loadConfig()
	if ci := f.Profile("ci"); ci == nil || ci.Token != "byd_secret" || ci.Server != g.URL || f.Current != "ci" {
		t.Fatalf("config = %+v; ci = %+v", f, ci)
	}
	if p := f.Profile("default"); p.Token != "alice-token" {
		t.Fatalf("default = %+v", p)
	}
	// --profile naming the personal login itself keeps it.
	t.Setenv(clientconfig.EnvProfile, "default")
	h.mustRun("cloudflare", "setup")
	if p := h.loadConfig().Profile("default"); p.Token != "alice-token" || !strings.Contains(h.err.String(), `Kept profile "default"`) {
		t.Fatalf("default = %+v; stderr = %s", p, h.err.String())
	}

	// The kept login while the current profile is for another server: no
	// next step (a `backyard deploy` would go there).
	t.Setenv(clientconfig.EnvProfile, "")
	h.writeProfile("corp", &clientconfig.Profile{Server: "https://backyard.corp.example", Token: "corp"}, true)
	h.mustRun("cloudflare", "setup")
	if f := h.loadConfig(); f.Current != "corp" || !strings.Contains(h.err.String(), `Kept profile "default"`) || strings.Contains(h.err.String(), "Next:") {
		t.Fatalf("config = %+v; stderr = %s", f, h.err.String())
	}

	// The deploy token's own profile (token:deploy, e.g. holding the token
	// from before --rotate-token) is not a personal login: it is refreshed.
	h.writeProfile("default", &clientconfig.Profile{Server: g.URL, Token: "byd_revoked", Login: clientconfig.LoginToken,
		User: &clientconfig.User{ID: "token:deploy", Name: "deploy", Admin: true}}, true)
	h.mustRun("cloudflare", "setup")
	if p := h.loadConfig().Profile("default"); p.Token != "byd_secret" {
		t.Fatalf("default = %+v; stderr = %s", p, h.err.String())
	}
}

func TestCloudflareAccessKeepsPersonalLogins(t *testing.T) {
	h := newHarness(t)
	cfCheckout(t, h.cwd)
	e := newCFEngine(h)
	g := cloudflareWorker(t)
	g.Info.Auth = deploy.AuthInfo{Mode: "access"}
	g.Tokens["alice-token"] = alice
	h.writeProfile("default", &clientconfig.Profile{Server: g.URL, Token: "alice-token", Login: clientconfig.LoginBrowser,
		Info: &deploy.Info{Auth: deploy.AuthInfo{Mode: "none"}}}, true)
	h.writeProfile("deployer", &clientconfig.Profile{Server: g.URL, Token: "byd_secret", Login: clientconfig.LoginToken}, false)
	state := map[string]any{"url": g.URL, "deployToken": "byd_secret"}

	engineResult(t, e, cloudflareResult{Command: "access enable", Worker: "backyard", URL: g.URL, PreviousURL: g.URL,
		WorkersDevURL: "https://backyard.acme.workers.dev", AuthMode: "access", DeployToken: "state"}, state, 0)
	h.mustRun("cloudflare", "access", "enable", "--allow-email-domain", "acme.com")
	f := h.loadConfig()
	if p := f.Profile("default"); p.Token != "alice-token" || p.Login != clientconfig.LoginBrowser || p.Info == nil || p.Info.Auth.Mode != "access" {
		t.Fatalf("default = %+v", p)
	}
	if p := f.Profile("deployer"); p.Token != "byd_secret" || p.Info == nil || p.Info.Auth.Mode != "access" {
		t.Fatalf("deployer = %+v", p)
	}
	if !strings.Contains(h.err.String(), "personal login: backyard login --server "+g.URL) {
		t.Fatalf("stderr = %s", h.err.String())
	}
	for _, r := range g.Requests() {
		if strings.Contains(r.Auth, "byd_secret") {
			t.Fatalf("access enable logged in: %s %s", r.Method, r.Path)
		}
	}

	// A setup while Access is on does not replace the personal login.
	res := setupResult("backyard", g.URL)
	res.AuthMode = "access"
	engineResult(t, e, res, state, 0)
	h.mustRun("cloudflare", "setup")
	if p := h.loadConfig().Profile("default"); p.Token != "alice-token" || p.Login != clientconfig.LoginBrowser ||
		!strings.Contains(h.err.String(), `Kept profile "default"`) {
		t.Fatalf("default = %+v; stderr = %s", p, h.err.String())
	}

	// access disable refreshes the info of the profiles on the Worker.
	g.Info.Auth = deploy.AuthInfo{Mode: "none"}
	engineResult(t, e, cloudflareResult{Command: "access disable", Worker: "backyard", URL: g.URL, PreviousURL: g.URL,
		AuthMode: "none", DeployToken: "state"}, state, 0)
	h.mustRun("cloudflare", "access", "disable")
	if p := h.loadConfig().Profile("default"); p.Info == nil || p.Info.Auth.Mode != "none" || p.Token != "alice-token" {
		t.Fatalf("default = %+v", p)
	}
}

func TestSavedCloudflareTokenByWorkersDevURL(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.Info.Auth = deploy.AuthInfo{Mode: "none"}
	g.Tokens["byd_wd"] = alice
	writeFiles(t, cfStateDir(h), map[string]string{
		"backyard.json": `{"version":2,"url":"https://backyard.acme.com","workersDevUrl":"` + g.URL + `/","deployToken":"byd_wd"}`,
	})
	h.mustRun("login", "--server", g.URL)
	if p := h.loadConfig().Profile("default"); p == nil || p.Token != "byd_wd" || !strings.Contains(h.err.String(), "saved by the Cloudflare setup") {
		t.Fatalf("profile = %+v; stderr = %s", p, h.err.String())
	}
	a := &cliApp{deps: h.deps()}
	if tok, _ := a.savedCloudflareToken("https://backyard.acme.com"); tok != "byd_wd" {
		t.Fatalf("by url: %q", tok)
	}
	if tok, _ := a.savedCloudflareToken("https://other.example"); tok != "" {
		t.Fatalf("other: %q", tok)
	}
}

// engineSpec is deploy/cloudflare/scripts/lib/commands.json.
type engineSpec struct {
	Schema   int              `json:"schema"`
	Global   []engineSpecFlag `json:"global"`
	Commands []struct {
		Name    string           `json:"name"`
		Args    []string         `json:"args"`
		Summary string           `json:"summary"`
		Flags   []engineSpecFlag `json:"flags"`
	} `json:"commands"`
}

type engineSpecFlag struct {
	Name       string   `json:"name"`
	Alias      []string `json:"alias"`
	Short      string   `json:"short"`
	Type       string   `json:"type"`
	Value      string   `json:"value"`
	Repeatable bool     `json:"repeatable"`
	Help       string   `json:"help"`
}

// asSpec turns a help entry into its commands.json form.
func asSpec(f cfFlag) engineSpecFlag {
	s := engineSpecFlag{Name: f.Name, Alias: f.Alias, Short: f.Short, Type: "bool", Value: f.Value, Repeatable: f.Repeatable, Help: f.Help}
	if f.Value != "" {
		s.Type = "string"
	}
	return s
}

func normalizeSpec(fs []engineSpecFlag) []engineSpecFlag {
	out := make([]engineSpecFlag, 0, len(fs))
	for _, f := range fs {
		if len(f.Alias) == 0 {
			f.Alias = nil
		}
		out = append(out, f)
	}
	return out
}

// TestCloudflareHelpMatchesEngine: the help of `backyard cloudflare …`
// documents exactly the engine's options (commands.json is their source).
func TestCloudflareHelpMatchesEngine(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "cloudflare", "scripts", "lib", "commands.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec engineSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Schema != 1 || len(spec.Commands) == 0 {
		t.Fatalf("commands.json: schema %d, %d commands", spec.Schema, len(spec.Commands))
	}
	toSpec := func(fs []cfFlag) []engineSpecFlag {
		out := make([]engineSpecFlag, 0, len(fs))
		for _, f := range fs {
			out = append(out, asSpec(f))
		}
		return normalizeSpec(out)
	}
	diff := func(where string, got, want []engineSpecFlag) {
		t.Helper()
		for i := range max(len(got), len(want)) {
			switch {
			case i >= len(got):
				t.Errorf("%s: --%s (commands.json) is missing from the help", where, want[i].Name)
			case i >= len(want):
				t.Errorf("%s: --%s is in the help but not in commands.json", where, got[i].Name)
			case !reflect.DeepEqual(got[i], want[i]):
				t.Errorf("%s: option %d differs from commands.json:\n help %+v\n json %+v", where, i, got[i], want[i])
			}
		}
	}
	diff("global options", toSpec(cfGlobalFlags), normalizeSpec(spec.Global))
	if len(cfCommands) != len(spec.Commands) {
		t.Errorf("%d commands in the help, %d in commands.json", len(cfCommands), len(spec.Commands))
	}
	for i, sc := range spec.Commands {
		c := findCFCommand(sc.Name)
		if c == nil {
			t.Errorf("engine command %q is missing from the help", sc.Name)
			continue
		}
		if i < len(cfCommands) && cfCommands[i].Name != sc.Name {
			t.Errorf("command %d: %q, commands.json has %q", i, cfCommands[i].Name, sc.Name)
		}
		if !slices.Equal(c.Args, sc.Args) || c.Summary != sc.Summary {
			t.Errorf("%s: args %q summary %q; commands.json %q %q", sc.Name, c.Args, c.Summary, sc.Args, sc.Summary)
		}
		diff(sc.Name, toSpec(c.Flags), normalizeSpec(sc.Flags))
	}

	// The rendered help of each subcommand mentions every option of its
	// engine commands, and its option sections name nothing else.
	h := newHarness(t)
	covered := map[string]bool{}
	optionRe := regexp.MustCompile(`--[a-z0-9][a-z0-9-]*`)
	columnRe := regexp.MustCompile(`\s{2,}`)
	goFlags := map[string]bool{}
	for _, f := range cfGoFlags {
		goFlags["--"+f.Name] = true
	}
	for _, sub := range cloudflareSubcommands() {
		help := h.mustRun("cloudflare", sub.name, "--help")
		allowed := map[string]bool{}
		for _, f := range cfGlobalFlags {
			allowed["--"+f.Name] = true
			for _, al := range f.Alias {
				allowed["--"+al] = true
			}
		}
		for _, name := range sub.engine {
			covered[name] = true
			for _, f := range append(slices.Clone(findCFCommand(name).Flags), cfGlobalFlags...) {
				if !strings.Contains(help, cfFlagLabel(f)) || !strings.Contains(help, cfFlagHelp(f)) {
					t.Errorf("backyard cloudflare %s --help does not document %s (%s)", sub.name, cfFlagLabel(f), name)
				}
				for _, al := range f.Alias {
					if !strings.Contains(help, "--"+al) {
						t.Errorf("backyard cloudflare %s --help does not mention --%s", sub.name, al)
					}
				}
				allowed["--"+f.Name] = true
			}
		}
		section := ""
		for _, line := range strings.Split(help, "\n") {
			switch {
			case line == "":
				section = ""
				continue
			case !strings.HasPrefix(line, " "):
				section = line
				continue
			case strings.HasPrefix(line, "   "):
				continue // the help of a wrapped row
			}
			label := columnRe.Split(strings.TrimSpace(line), 2)[0] // "-y, --yes", "--zone <zone>"
			for _, tok := range optionRe.FindAllString(label, -1) {
				switch {
				case strings.HasPrefix(section, "Options") || strings.HasPrefix(section, "Global options"):
					if !allowed[tok] {
						t.Errorf("backyard cloudflare %s --help, %s: %s is not an option of the engine (commands.json)", sub.name, section, tok)
					}
				case strings.HasPrefix(section, "Flags of this CLI"):
					if !goFlags[tok] {
						t.Errorf("backyard cloudflare %s --help, %s: %s is not a flag of this CLI", sub.name, section, tok)
					}
				}
			}
		}
		for f := range goFlags {
			if !strings.Contains(help, f) {
				t.Errorf("backyard cloudflare %s --help does not list %s", sub.name, f)
			}
		}
	}
	for _, sc := range spec.Commands {
		if !covered[sc.Name] {
			t.Errorf("no backyard cloudflare subcommand documents %q", sc.Name)
		}
	}
}
