package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/version"
)

// How `backyard cloudflare` gets its engine (design §2.5):
//
//  1. Checkout mode: --dir, or (in builds from source only) the Backyard
//     deploy/cloudflare found from the current directory, within its
//     repository. npm ci (npm install without a lockfile) unless
//     --skip-install or node_modules is newer than package-lock.json, then
//     `node bin/backyard-cloudflare.mjs …` in that folder. Release builds never
//     pick up a checkout on their own: whatever sits in a parent directory
//     must not replace the verified package.
//  2. Package mode: --package <file or https URL>, else the package of this
//     release (<releaseBase>v<version>/backyard-cloudflare-<version>.tgz),
//     whose SHA-256 is built into release binaries (cloudflarePackageSHA256).
//     It is installed with npm into <user cache>/backyard/cloudflare-engine/<sha256>
//     and run from the user's directory. A complete install of the expected
//     digest is reused without any network access. After a fresh install,
//     older engines are removed (the previous one and this release's are kept).

// cloudflarePackageSHA256 is the SHA-256 (lowercase hex) of this release's
// backyard-cloudflare package, set when the release is built:
//
//	-ldflags "-X github.com/thasami/backyard/internal/cli.cloudflarePackageSHA256=<hex>"
//
// Development builds have none: they use a checkout, --dir or --package.
var cloudflarePackageSHA256 string

const (
	// minNodeMajor is the oldest Node.js the engine (and wrangler) runs on.
	minNodeMajor = 22
	// maxCloudflarePackage bounds the package download.
	maxCloudflarePackage = 64 << 20
	// cloudflareDownloadTimeout bounds the package download.
	cloudflareDownloadTimeout = 5 * time.Minute
	// cloudflareCompleteFile marks a finished install in the cache.
	cloudflareCompleteFile = ".complete"
	// cloudflareStaleInstall is the age after which an unfinished install in
	// the cache (.install-*, or a digest dir without .complete) is a leftover
	// of a killed run, not a concurrent install still in progress.
	cloudflareStaleInstall = 24 * time.Hour
)

var (
	sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// releaseTagRe matches the module version of `go install …@vX.Y.Z`.
	releaseTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// cloudflareReleaseBase returns the base URL of the release downloads:
// $BACKYARD_GITHUB_URL (default https://github.com) + /thasami/backyard/releases/download/.
func cloudflareReleaseBase(github string) string {
	github = strings.TrimSuffix(strings.TrimSpace(github), "/")
	if github == "" {
		github = "https://github.com"
	}
	return github + "/thasami/backyard/releases/download/"
}

// cloudflareReleaseURL is the package of this CLI release.
func (a *cliApp) cloudflareReleaseURL() string {
	v := strings.TrimPrefix(version.Version, "v")
	return a.deps.releaseBase + "v" + v + "/" + cloudflarePackageName + "-" + v + ".tgz"
}

// errNodeMissing and errNpmMissing explain the prerequisites.
var (
	errNodeMissing = errors.New("node not found in PATH: backyard cloudflare needs Node.js 22 or later and npm (https://nodejs.org/)")
	errNpmMissing  = errors.New("npm not found in PATH: backyard cloudflare needs Node.js 22 or later and npm (https://nodejs.org/)")
)

// requireNode checks that node is on PATH and at least version 22.
func (a *cliApp) requireNode(ctx context.Context) error {
	if _, err := a.deps.lookPath("node"); err != nil {
		return errNodeMissing
	}
	major, raw, err := a.nodeMajorVersion(ctx)
	if err != nil {
		return err
	}
	if major < minNodeMajor {
		return fmt.Errorf("the Cloudflare engine cannot run here: Node.js %d or later is needed (found %s); https://nodejs.org/", minNodeMajor, raw)
	}
	return nil
}

// npmSecretEnv are the Cloudflare credentials npm never needs: its runs
// (and the install scripts of the engine's dependencies) get them blanked.
var npmSecretEnv = []string{"BACKYARD_CF_API_TOKEN", "CLOUDFLARE_API_TOKEN", "DEPLOY_TOKEN"}

// npmEnv blanks the credentials of npmSecretEnv that are set.
func npmEnv() []string {
	var env []string
	for _, k := range npmSecretEnv {
		if _, ok := os.LookupEnv(k); ok {
			env = append(env, k+"=")
		}
	}
	return env
}

// requireNpm checks that npm is on PATH (needed to install the engine).
func (a *cliApp) requireNpm() error {
	if _, err := a.deps.lookPath("npm"); err != nil {
		return errNpmMissing
	}
	return nil
}

// nodeMajorVersion runs `node --version` and returns the major version and
// the version as printed (v22.11.0).
func (a *cliApp) nodeMajorVersion(ctx context.Context) (major int, raw string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := a.deps.runner.Run(ctx, Command{Name: "node", Args: []string{"--version"}, Stdout: &out, Stderr: io.Discard}); err != nil {
		return 0, "", fmt.Errorf("node --version: %w", err)
	}
	raw = strings.TrimSpace(out.String())
	head, _, _ := strings.Cut(strings.TrimPrefix(raw, "v"), ".")
	major, convErr := strconv.Atoi(head)
	if convErr != nil || major <= 0 {
		return 0, raw, fmt.Errorf("could not read the Node.js version (node --version printed %q); backyard cloudflare needs Node.js %d or later", raw, minNodeMajor)
	}
	return major, raw, nil
}

// cloudflareEngine is a runnable backyard-cloudflare.
type cloudflareEngine struct {
	// entry is the script node runs (relative to dir in a checkout).
	entry string
	// dir is the working directory of the engine.
	dir string
}

// resolveCloudflareEngine finds the checkout or installs the package
// (design §2.5).
func (a *cliApp) resolveCloudflareEngine(ctx context.Context, fl cloudflareFlags) (*cloudflareEngine, error) {
	cwd, err := a.cwd()
	if err != nil {
		return nil, err
	}
	if fl.pkg == "" {
		dir := ""
		if fl.dir != "" {
			dir = absFrom(cwd, fl.dir)
			if problem := cloudflareCheckoutProblem(dir); problem != "" {
				return nil, fmt.Errorf("%s does not contain the Backyard Cloudflare edition (%s); docs: %s", dir, problem, cloudflareDocs)
			}
		} else if strings.TrimSpace(cloudflarePackageSHA256) == "" {
			// Builds from source only: a release build runs its verified
			// package unless --dir names a checkout explicitly.
			dir = findCloudflareDir(cwd)
		}
		if dir != "" {
			return a.prepareCloudflareCheckout(ctx, dir, fl.skipInstall)
		}
	}
	if fl.skipInstall {
		a.errf("Note: --skip-install applies to a checkout only; ignored.\n")
	}
	spec, expected := fl.pkg, ""
	if spec == "" {
		expected = strings.ToLower(strings.TrimSpace(cloudflarePackageSHA256))
		if expected == "" {
			return nil, &ExitError{Code: 2, Err: a.errNoCloudflarePackage()}
		}
		if !sha256HexRe.MatchString(expected) {
			return nil, fmt.Errorf("this CLI was built with an invalid Cloudflare package digest %q; reinstall it", expected)
		}
		spec = a.cloudflareReleaseURL()
	}
	dir, err := a.installCloudflarePackage(ctx, spec, expected)
	if err != nil {
		return nil, err
	}
	return &cloudflareEngine{entry: cloudflareInstalledEntry(dir), dir: cwd}, nil
}

// errNoCloudflarePackage explains what a build without a pinned package
// (`go install`, make build) can do instead outside a checkout.
func (a *cliApp) errNoCloudflarePackage() error {
	v := goInstallVersion()
	if v == "" {
		v = "<v>"
	}
	return fmt.Errorf("this development build has no Cloudflare package pinned (a `go install` or source build). Any of these works:\n"+
		"  - install a release build of backyard: curl -fsSL https://raw.githubusercontent.com/thasami/backyard/main/scripts/install.sh | sh\n"+
		"  - with Node.js only, once a Backyard release has published it to npm: npx backyard-cloudflare@latest setup "+
		"(or domain, access, status), then the `backyard login --server <hub> --token <token>` it prints\n"+
		"  - run inside a checkout of the Backyard repository, or pass --dir <checkout>/deploy/cloudflare\n"+
		"  - pass --package <path or https URL of backyard-cloudflare-<v>.tgz>, e.g. a release's %s "+
		"(make cloudflare-pack builds one)",
		a.deps.releaseBase+"v"+v+"/"+cloudflarePackageName+"-"+v+".tgz")
}

// goInstallVersion is the module version of a `go install …@vX.Y.Z` build
// without the "v" ("" otherwise). It only names a release in a hint: there is
// no digest to verify a download against.
func goInstallVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok || !releaseTagRe.MatchString(bi.Main.Version) {
		return ""
	}
	return strings.TrimPrefix(bi.Main.Version, "v")
}

// cloudflareDepsCurrent reports whether a checkout's node_modules is at least
// as new as its package-lock.json (npm writes node_modules/.package-lock.json
// at the end of every successful install). npm ci would delete node_modules
// out from under a running engine or `wrangler dev` for nothing.
func cloudflareDepsCurrent(dir string) bool {
	lock, err := os.Stat(filepath.Join(dir, "package-lock.json"))
	if err != nil {
		return false
	}
	hidden, err := os.Stat(filepath.Join(dir, "node_modules", ".package-lock.json"))
	return err == nil && !hidden.ModTime().Before(lock.ModTime())
}

// prepareCloudflareCheckout installs the dependencies of a checkout's
// deploy/cloudflare (npm ci, or npm install without a lockfile) unless they
// are current.
func (a *cliApp) prepareCloudflareCheckout(ctx context.Context, dir string, skipInstall bool) (*cloudflareEngine, error) {
	a.errf("Using the Cloudflare engine in %s (checkout)\n", dir)
	if !skipInstall && cloudflareDepsCurrent(dir) {
		a.errf("Dependencies are up to date (node_modules is newer than package-lock.json): npm ci skipped.\n")
		skipInstall = true
	}
	if !skipInstall {
		if err := a.requireNpm(); err != nil {
			return nil, err
		}
		install := []string{"ci"}
		if !fileExists(filepath.Join(dir, "package-lock.json")) {
			install = []string{"install"}
		}
		a.errf("$ npm %s\n", strings.Join(install, " "))
		// npm's output goes to stderr: stdout is the engine's (status --json).
		if err := a.deps.runner.Run(ctx, Command{Name: "npm", Args: install, Dir: dir, Env: npmEnv(),
			Stdout: a.streams.ErrOut, Stderr: a.streams.ErrOut}); err != nil {
			return nil, fmt.Errorf("npm %s in %s: %w", install[0], dir, err)
		}
	}
	return &cloudflareEngine{entry: filepath.FromSlash(cloudflareEntry), dir: dir}, nil
}

// readCloudflarePackage reads dir/package.json, returning its name and
// whether it defines the "setup" script.
func readCloudflarePackage(dir string) (name string, hasSetup bool) {
	pkg, err := readPackageJSON(dir)
	if err != nil {
		return "", false
	}
	return pkg.Name, strings.TrimSpace(pkg.Scripts["setup"]) != ""
}

// packageJSON is the part of a package.json this CLI reads.
type packageJSON struct {
	Name    string            `json:"name"`
	Version string            `json:"version"`
	Scripts map[string]string `json:"scripts"`
}

func readPackageJSON(dir string) (*packageJSON, error) {
	b, err := readSmallFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var pkg packageJSON
	if err := json.Unmarshal(b, &pkg); err != nil {
		return nil, err
	}
	return &pkg, nil
}

// cloudflareCheckoutProblem says why dir is not the Backyard Workers edition
// ("" when it is). Its npm scripts are about to be run, so another project's
// deploy/cloudflare must not pass.
func cloudflareCheckoutProblem(dir string) string {
	name, hasSetup := readCloudflarePackage(dir)
	switch {
	case name != cloudflarePackageName || !hasSetup:
		return `no package.json named "backyard-cloudflare" with a "setup" script`
	case !fileExists(filepath.Join(dir, filepath.FromSlash(cloudflareEntry))):
		return "no " + cloudflareEntry + ": the checkout predates the backyard-cloudflare engine, update it"
	}
	return ""
}

// isBackyardCloudflare reports whether dir holds the Backyard Workers
// edition with its engine.
func isBackyardCloudflare(dir string) bool { return cloudflareCheckoutProblem(dir) == "" }

// findCloudflareDir searches start and its parents for the Backyard
// deploy/cloudflare folder (or start itself being that folder). It never
// leaves the repository start is in: the search ends at the first directory
// holding .git.
func findCloudflareDir(start string) string {
	dir := start
	for {
		if filepath.Base(dir) == "cloudflare" && filepath.Base(filepath.Dir(dir)) == "deploy" && isBackyardCloudflare(dir) {
			return dir
		}
		if cand := filepath.Join(dir, filepath.FromSlash(cloudflareRel)); isBackyardCloudflare(cand) {
			return cand
		}
		if fileExists(filepath.Join(dir, ".git")) {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// cloudflareComplete is the .complete marker of a finished install.
type cloudflareComplete struct {
	SHA256      string    `json:"sha256"`
	Version     string    `json:"version"`
	InstalledAt time.Time `json:"installedAt"`
}

// cloudflareInstalledEntry is the engine's entry point in an install dir.
func cloudflareInstalledEntry(dir string) string {
	return filepath.Join(dir, "node_modules", cloudflarePackageName, filepath.FromSlash(cloudflareEntry))
}

// readCloudflareComplete reads the .complete marker of an install dir.
func readCloudflareComplete(dir string) (cloudflareComplete, bool) {
	var c cloudflareComplete
	b, err := readSmallFile(filepath.Join(dir, cloudflareCompleteFile))
	if err != nil {
		return c, false
	}
	return c, json.Unmarshal(b, &c) == nil
}

// cloudflareInstalled reports whether dir is a complete install of the
// package with digest sum.
func cloudflareInstalled(dir, sum string) bool {
	c, ok := readCloudflareComplete(dir)
	return ok && c.SHA256 == sum && fileExists(cloudflareInstalledEntry(dir))
}

// pruneCloudflareCache removes older engines from the cache root after a
// fresh install of sum. It keeps sum, this release's package and the newest
// other complete install (a CLI of another version may be running it right
// now, or the user may go back to it), and removes unfinished installs older
// than cloudflareStaleInstall (a younger one may be a concurrent install).
// Best effort: problems are only logged.
func (a *cliApp) pruneCloudflareCache(root, sum string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		a.log.Debug("prune the Cloudflare engine cache", "dir", root, "error", err)
		return
	}
	now := a.deps.now()
	pinned := strings.ToLower(strings.TrimSpace(cloudflarePackageSHA256))
	remove := func(name string) {
		a.log.Debug("removing an old Cloudflare engine", "dir", filepath.Join(root, name))
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			a.log.Debug("prune the Cloudflare engine cache", "dir", filepath.Join(root, name), "error", err)
		}
	}
	type install struct {
		name string
		at   time.Time
	}
	var older []install
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name == sum {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		stale := now.Sub(info.ModTime()) > cloudflareStaleInstall
		switch {
		case strings.HasPrefix(name, ".install-"):
			if stale {
				remove(name)
			}
		case sha256HexRe.MatchString(name):
			if c, ok := readCloudflareComplete(filepath.Join(root, name)); ok {
				older = append(older, install{name: name, at: c.InstalledAt})
			} else if stale {
				remove(name) // a broken leftover
			}
		}
	}
	slices.SortFunc(older, func(x, y install) int { return y.at.Compare(x.at) }) // newest first
	for i, in := range older {
		if i > 0 && in.name != pinned {
			remove(in.name)
		}
	}
}

// installCloudflarePackage returns the install dir of the package at spec (a
// file or an https URL), installing it into the user cache when needed. With
// an expected digest (this release's package) a complete install is reused
// without any network access, and a package with another digest is refused
// before anything is installed.
func (a *cliApp) installCloudflarePackage(ctx context.Context, spec, expected string) (string, error) {
	cache, err := a.deps.cacheDir()
	if err != nil {
		return "", fmt.Errorf("locate the user cache directory: %w", err)
	}
	root := filepath.Join(cache, "backyard", "cloudflare-engine")
	if expected != "" {
		if dir := filepath.Join(root, expected); cloudflareInstalled(dir, expected) {
			a.log.Debug("using the installed Cloudflare engine", "dir", dir)
			return dir, nil
		}
		if err := a.requireNpm(); err != nil {
			return "", err
		}
	}
	data, err := a.fetchCloudflarePackage(ctx, spec)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	sum := hex.EncodeToString(digest[:])
	switch {
	case expected != "" && sum != expected:
		return "", fmt.Errorf("integrity check failed for %s: its sha256 is %s, but this CLI release expects %s; nothing was installed", spec, sum, expected)
	case expected == "" && sum == strings.ToLower(cloudflarePackageSHA256):
		a.errf("Using %s (sha256 %s: the package of this CLI release)\n", spec, sum)
	case expected == "":
		a.errf("Using %s (sha256 %s; not verified against this CLI release)\n", spec, sum)
	}
	dir := filepath.Join(root, sum)
	if cloudflareInstalled(dir, sum) {
		return dir, nil
	}
	if err := a.requireNpm(); err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", root, err)
	}
	tmp, err := os.MkdirTemp(root, ".install-*")
	if err != nil {
		return "", fmt.Errorf("install the Cloudflare engine: %w", err)
	}
	defer func() {
		if tmp != "" {
			_ = os.RemoveAll(tmp)
		}
	}()
	tgz := filepath.Join(tmp, cloudflarePackageName+".tgz")
	if err := os.WriteFile(tgz, data, 0o600); err != nil {
		return "", fmt.Errorf("install the Cloudflare engine: %w", err)
	}
	args := []string{"install", "--no-audit", "--no-fund", "--no-save", "--omit=dev", "--prefix", tmp, tgz}
	a.errf("Installing the Cloudflare engine into %s\n$ npm %s\n", dir, strings.Join(args, " "))
	// Dir = tmp: its (absent) .npmrc is the project config, not the user's cwd.
	if err := a.deps.runner.Run(ctx, Command{Name: "npm", Args: args, Dir: tmp, Env: npmEnv(),
		Stdout: a.streams.ErrOut, Stderr: a.streams.ErrOut}); err != nil {
		return "", fmt.Errorf("npm install %s: %w", spec, err)
	}
	pkg, err := readPackageJSON(filepath.Join(tmp, "node_modules", cloudflarePackageName))
	if err != nil || pkg.Name != cloudflarePackageName || !fileExists(cloudflareInstalledEntry(tmp)) {
		return "", fmt.Errorf("%s is not a %s package (no node_modules/%s with %s after npm install); nothing was installed",
			spec, cloudflarePackageName, cloudflarePackageName, cloudflareEntry)
	}
	marker, err := json.Marshal(cloudflareComplete{SHA256: sum, Version: pkg.Version, InstalledAt: a.deps.now().UTC()})
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, cloudflareCompleteFile), append(marker, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("install the Cloudflare engine: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if cloudflareInstalled(dir, sum) {
			a.pruneCloudflareCache(root, sum)
			return dir, nil // a concurrent run installed it first
		}
		// A broken leftover (no .complete): replace it.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return "", fmt.Errorf("install the Cloudflare engine into %s: %w", dir, err)
		}
		if err := os.Rename(tmp, dir); err != nil {
			return "", fmt.Errorf("install the Cloudflare engine into %s: %w", dir, err)
		}
	}
	tmp = ""
	a.pruneCloudflareCache(root, sum)
	return dir, nil
}

// fetchCloudflarePackage reads the package at spec: a local file (relative
// to the current directory) or an https URL (http only for loopback hosts,
// e.g. tests), at most 64 MiB.
func (a *cliApp) fetchCloudflarePackage(ctx context.Context, spec string) ([]byte, error) {
	lower := strings.ToLower(spec)
	if !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		if strings.Contains(spec, "://") {
			return nil, fmt.Errorf("--package %s: only a local file or an https URL is supported", spec)
		}
		cwd, err := a.cwd()
		if err != nil {
			return nil, err
		}
		f, err := os.Open(absFrom(cwd, spec))
		if err != nil {
			return nil, fmt.Errorf("--package: %w", err)
		}
		defer f.Close()
		return readPackageBytes(f, spec)
	}
	u, err := url.Parse(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid package URL: %w", err)
	}
	if err := checkPackageURL(u); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, cloudflareDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", spec, err)
	}
	req.Header.Set("User-Agent", "backyard-cli/"+version.Version)
	hc := http.Client{}
	if a.deps.httpClient != nil {
		hc = *a.deps.httpClient
	}
	hc.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return checkPackageURL(r.URL)
	}
	a.errf("Downloading %s\n", spec)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", spec, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", spec, resp.StatusCode)
	}
	return readPackageBytes(resp.Body, spec)
}

// readPackageBytes reads at most maxCloudflarePackage bytes.
func readPackageBytes(r io.Reader, spec string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxCloudflarePackage+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", spec, err)
	}
	if len(data) > maxCloudflarePackage {
		return nil, fmt.Errorf("%s is larger than %d MiB: not a backyard-cloudflare package", spec, maxCloudflarePackage>>20)
	}
	return data, nil
}

// checkPackageURL accepts https URLs, and http URLs of loopback hosts only.
func checkPackageURL(u *url.URL) error {
	switch {
	case u.User != nil:
		return errors.New("package URLs with credentials are not supported")
	case u.Host == "":
		return fmt.Errorf("invalid package URL %s: no host", u.Redacted())
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && isLoopbackHost(u.Hostname()):
		return nil
	case u.Scheme == "http":
		return fmt.Errorf("refusing to download %s over plain http: use https", u.Redacted())
	}
	return fmt.Errorf("unsupported package URL %s: use https", u.Redacted())
}

// isLoopbackHost reports whether host is localhost or a loopback address.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
