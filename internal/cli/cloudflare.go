package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/cli/scaffold"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/version"
)

const (
	// cloudflareRel is the Workers edition inside the repository.
	cloudflareRel = "deploy/cloudflare"
	// cloudflareDocs documents the Workers edition.
	cloudflareDocs = "https://github.com/thasami/backyard/tree/main/deploy/cloudflare#readme"
	// repoURL is the Backyard source repository.
	repoURL = "https://github.com/thasami/backyard"

	maxArchiveBytes = 512 << 20
	maxArchiveFile  = 64 << 20
	maxArchiveFiles = 50000
)

type cloudflareFlags struct {
	dir         string
	download    bool
	ref         string
	skipInstall bool
	noLogin     bool
}

func (a *cliApp) newCloudflareCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloudflare",
		Short: "Deploy Backyard on Cloudflare's free tier",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var fl cloudflareFlags
	setup := &cobra.Command{
		Use:   "setup [-- setup-args...]",
		Short: "Provision or upgrade the Workers edition on your Cloudflare account",
		Long: `Provision (or upgrade — re-running is safe) the Backyard Workers edition on your own
Cloudflare account, on the free plan: one Worker plus SQLite-backed Durable Objects, Workers
AI, and an optional R2 bucket for big files. It runs "npm ci" and "npm run setup" in
deploy/cloudflare, which logs in with wrangler (browser) if needed, creates secrets and
deploys. Afterwards this CLI is logged in to the new server with its deploy token (unless
--no-login), so "backyard init" and "backyard deploy" work right away.

The folder is found relative to the current directory (a checkout of the Backyard repository)
or given with --dir. Without a checkout, --download fetches the matching source release from
GitHub into your user cache. Arguments after "--" are passed to the setup script.

Requires Node.js 20+ and npm.`,
		Example: `  backyard cloudflare setup
  backyard cloudflare setup --download
  backyard cloudflare setup --dir ~/src/backyard/deploy/cloudflare -- --worker-name backyard`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
				return fmt.Errorf("unexpected arguments %q: pass setup-script arguments after \"--\"", args)
			}
			return a.runCloudflareSetup(cmd.Context(), fl, args)
		},
	}
	f := setup.Flags()
	f.StringVar(&fl.dir, "dir", "", "path to deploy/cloudflare (default: found from the current directory)")
	f.BoolVar(&fl.download, "download", false, "download the Backyard source from GitHub when no checkout is found")
	f.StringVar(&fl.ref, "ref", "", "git tag or branch to download (default: this CLI's version, or main for dev builds)")
	f.BoolVar(&fl.skipInstall, "skip-install", false, "skip npm ci")
	f.BoolVar(&fl.noLogin, "no-login", false, "do not log this CLI in to the new server afterwards")
	cmd.AddCommand(setup)
	return cmd
}

func (a *cliApp) runCloudflareSetup(ctx context.Context, fl cloudflareFlags, args []string) error {
	for _, tool := range []string{"node", "npm"} {
		if _, err := a.deps.lookPath(tool); err != nil {
			return fmt.Errorf("%s not found in PATH: the Cloudflare setup needs Node.js 20+ and npm (https://nodejs.org/)", tool)
		}
	}
	dir, err := a.locateCloudflare(ctx, fl)
	if err != nil {
		return err
	}
	a.errf("Using the Cloudflare edition in %s\n", dir)
	if !fl.skipInstall {
		install := []string{"ci"}
		if !fileExists(filepath.Join(dir, "package-lock.json")) {
			install = []string{"install"}
		}
		a.errf("$ npm %s\n", strings.Join(install, " "))
		if err := a.deps.runner.Run(ctx, Command{Name: "npm", Args: install, Dir: dir,
			Stdin: a.streams.In, Stdout: a.streams.Out, Stderr: a.streams.ErrOut}); err != nil {
			return fmt.Errorf("npm %s: %w", install[0], err)
		}
	}
	setupArgs := []string{"run", "setup"}
	if len(args) > 0 {
		setupArgs = append(append(setupArgs, "--"), args...)
	}
	started := a.deps.now()
	a.errf("$ npm %s\n", strings.Join(setupArgs, " "))
	if err := a.deps.runner.Run(ctx, Command{Name: "npm", Args: setupArgs, Dir: dir,
		Stdin: a.streams.In, Stdout: a.streams.Out, Stderr: a.streams.ErrOut}); err != nil {
		return fmt.Errorf("cloudflare setup: %w", err)
	}
	if fl.noLogin {
		return nil
	}
	return a.cloudflareLogin(ctx, dir, args, started)
}

// locateCloudflare finds (or downloads) the deploy/cloudflare folder.
func (a *cliApp) locateCloudflare(ctx context.Context, fl cloudflareFlags) (string, error) {
	cwd, err := a.cwd()
	if err != nil {
		return "", err
	}
	if fl.dir != "" {
		dir := absFrom(cwd, fl.dir)
		if _, hasSetup := readCloudflarePackage(dir); !hasSetup {
			return "", fmt.Errorf("%s does not contain the Cloudflare edition (no package.json with a \"setup\" script); docs: %s", dir, cloudflareDocs)
		}
		return dir, nil
	}
	if dir := findCloudflareDir(cwd); dir != "" {
		return dir, nil
	}
	ref := fl.ref
	if ref == "" {
		ref = defaultSourceRef(version.Version)
	}
	if !validRef(ref) {
		return "", fmt.Errorf("invalid --ref %q", ref)
	}
	cacheRoot, cacheErr := a.deps.cacheDir()
	var cached string
	if cacheErr == nil {
		cached = filepath.Join(cacheRoot, "backyard", "src", strings.ReplaceAll(ref, "/", "_"))
		if dir := filepath.Join(cached, filepath.FromSlash(cloudflareRel)); isBackyardCloudflare(dir) && !fl.download {
			return dir, nil
		}
	}
	notFound := fmt.Sprintf("could not find the Backyard Cloudflare edition (%s).\n"+
		"Run this command inside a checkout of %s, pass --dir <path>, or let the CLI fetch the source:\n"+
		"  backyard cloudflare setup --download\n"+
		"Docs: %s", cloudflareRel, repoURL, cloudflareDocs)
	if !fl.download {
		if !a.deps.stdinTTY() {
			return "", errors.New(notFound)
		}
		ok, err := a.confirm(ctx, fmt.Sprintf("No checkout found. Download Backyard %s from %s?", ref, repoURL))
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errors.New(notFound)
		}
	}
	if cacheErr != nil {
		return "", fmt.Errorf("locate the user cache directory: %w", cacheErr)
	}
	if err := a.downloadSource(ctx, ref, cached); err != nil {
		return "", err
	}
	dir := filepath.Join(cached, filepath.FromSlash(cloudflareRel))
	if !isBackyardCloudflare(dir) {
		return "", fmt.Errorf("the downloaded source (%s) has no %s; try --ref main. Docs: %s", ref, cloudflareRel, cloudflareDocs)
	}
	return dir, nil
}

// cloudflarePackageName is the npm package name of deploy/cloudflare.
const cloudflarePackageName = "backyard-cloudflare"

// readCloudflarePackage reads dir/package.json, returning its name and
// whether it defines the "setup" script.
func readCloudflarePackage(dir string) (name string, hasSetup bool) {
	f, err := os.Open(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", false
	}
	defer f.Close()
	var pkg struct {
		Name    string            `json:"name"`
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.NewDecoder(io.LimitReader(f, 1<<20)).Decode(&pkg); err != nil {
		return "", false
	}
	return pkg.Name, strings.TrimSpace(pkg.Scripts["setup"]) != ""
}

// isBackyardCloudflare reports whether dir holds the Backyard Workers
// edition (and not some other project's deploy/cloudflare: its npm
// scripts are about to be run).
func isBackyardCloudflare(dir string) bool {
	name, hasSetup := readCloudflarePackage(dir)
	return name == cloudflarePackageName && hasSetup
}

// findCloudflareDir searches start and its parents for the Backyard
// deploy/cloudflare folder (or start itself being that folder).
func findCloudflareDir(start string) string {
	dir := start
	for {
		if filepath.Base(dir) == "cloudflare" && filepath.Base(filepath.Dir(dir)) == "deploy" && isBackyardCloudflare(dir) {
			return dir
		}
		cand := filepath.Join(dir, filepath.FromSlash(cloudflareRel))
		if isBackyardCloudflare(cand) {
			return cand
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

var releaseRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// defaultSourceRef maps a CLI version to the git ref of its source.
func defaultSourceRef(v string) string {
	if releaseRe.MatchString(v) {
		return "v" + strings.TrimPrefix(v, "v")
	}
	return "main"
}

var refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

func validRef(ref string) bool { return refRe.MatchString(ref) && !strings.Contains(ref, "..") }

// downloadSource fetches the repository tarball for ref and extracts it
// into dest (replacing a previous download).
func (a *cliApp) downloadSource(ctx context.Context, ref, dest string) error {
	u := strings.TrimSuffix(a.deps.downloadBase, "/") + "/" + ref
	a.errf("Downloading %s …\n", u)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	req.Header.Set("User-Agent", "backyard-cli/"+version.Version)
	hc := a.deps.httpClient
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download source %s: HTTP %d (does the ref %q exist? try --ref main)", u, resp.StatusCode, ref)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return fmt.Errorf("download source: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := extractTarGz(io.LimitReader(resp.Body, maxArchiveBytes+1), tmp); err != nil {
		return fmt.Errorf("extract source: %w", err)
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("replace %s: %w", dest, err)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return fmt.Errorf("replace %s: %w", dest, err)
	}
	a.errf("Saved the source to %s\n", dest)
	return nil
}

// extractTarGz extracts a GitHub source tarball into dest, stripping the
// top-level folder. Only regular files and directories are extracted;
// entries escaping dest, links and oversized archives are rejected.
func extractTarGz(r io.Reader, dest string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var total int64
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean(strings.TrimPrefix(h.Name, "./")), "/")
		_, rest, ok := strings.Cut(name, "/")
		if !ok || rest == "" {
			continue // the top-level folder or pax headers
		}
		switch h.Typeflag {
		case tar.TypeDir:
			// Checked as the parent of a placeholder so that a directory
			// created earlier for a file inside it is accepted.
			child, err := scaffold.SafeJoin(dest, rest+"/.dir")
			if err != nil {
				return err
			}
			target := filepath.Dir(child)
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			files++
			if files > maxArchiveFiles {
				return errors.New("archive has too many files")
			}
			if h.Size > maxArchiveFile {
				return fmt.Errorf("archive entry %s is too large", rest)
			}
			total += h.Size
			if total > maxArchiveBytes {
				return errors.New("archive is too large")
			}
			target, err := scaffold.SafeJoin(dest, rest)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, h.Size)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Symlinks, hard links and devices are skipped.
		}
	}
	return nil
}

// cloudflareState is what deploy/cloudflare/scripts/setup.mjs remembers per
// Worker in $XDG_CONFIG_HOME/backyard/cloudflare/<worker>.json (mode 0600).
type cloudflareState struct {
	URL         string `json:"url"`
	DeployToken string `json:"deployToken"`
}

// cloudflareStateDir mirrors setup.mjs: $XDG_CONFIG_HOME (relative values
// resolve against the setup script's working directory, cfDir) or
// ~/.config, then backyard/cloudflare.
func (a *cliApp) cloudflareStateDir(cfDir string) (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(absFrom(cfDir, x), "backyard", "cloudflare"), nil
	}
	home, err := a.deps.homeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "backyard", "cloudflare"), nil
}

// setupWorkerName returns the --worker-name/--name passed to the setup
// script ("" when absent).
func setupWorkerName(args []string) string {
	for i, arg := range args {
		for _, flag := range []string{"--worker-name", "--name"} {
			if v, ok := strings.CutPrefix(arg, flag+"="); ok {
				return v
			}
			if arg == flag && i+1 < len(args) {
				return args[i+1]
			}
		}
	}
	return ""
}

// freshCloudflareState finds the state file the setup run just wrote: the
// named Worker's, else the most recently written one, modified at or after
// since (a run that only printed --help writes nothing).
func freshCloudflareState(dir, worker string, since time.Time) (*cloudflareState, string, error) {
	since = since.Add(-2 * time.Second) // coarse file system timestamps
	var candidates []string
	if worker != "" {
		candidates = []string{filepath.Join(dir, worker+".json")}
	} else {
		matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
		if err != nil {
			return nil, "", err
		}
		candidates = matches
	}
	var (
		best     string
		bestTime time.Time
	)
	for _, c := range candidates {
		st, err := os.Stat(c)
		if err != nil || !st.Mode().IsRegular() || st.ModTime().Before(since) {
			continue
		}
		if best == "" || st.ModTime().After(bestTime) {
			best, bestTime = c, st.ModTime()
		}
	}
	if best == "" {
		return nil, "", nil
	}
	b, err := os.ReadFile(best)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", best, err)
	}
	var st cloudflareState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", best, err)
	}
	return &st, strings.TrimSuffix(filepath.Base(best), ".json"), nil
}

// cloudflareLogin stores a CLI profile for the Worker the setup script just
// deployed, using its deploy token, so `backyard deploy` works right away
// (the OpenQuick experience: setup, init, deploy). Problems are reported
// as warnings: the platform itself is already deployed.
func (a *cliApp) cloudflareLogin(ctx context.Context, cfDir string, args []string, since time.Time) error {
	stateDir, err := a.cloudflareStateDir(cfDir)
	if err != nil {
		a.warnf("could not log the CLI in automatically: %v", err)
		return nil
	}
	st, worker, err := freshCloudflareState(stateDir, setupWorkerName(args), since)
	if err != nil {
		a.warnf("could not log the CLI in automatically: %v", err)
		return nil
	}
	if st == nil {
		return nil // e.g. "-- --help": nothing was deployed
	}
	token := strings.TrimSpace(st.DeployToken)
	server, err := clientconfig.NormalizeURL(st.URL)
	if err != nil || token == "" {
		a.errf("\nTo connect this CLI, run the `backyard login --server <url> --token <token>` command printed above.\n")
		return nil
	}

	path, err := a.configPath()
	if err != nil {
		return err
	}
	f, err := clientconfig.Load(path)
	if err != nil {
		return err
	}
	name := f.ActiveName(a.g.profile)
	if a.g.profile == "" && strings.TrimSpace(os.Getenv(clientconfig.EnvProfile)) == "" {
		// Keep an existing profile for another server: use one named after
		// the Worker instead.
		if p := f.Profile(name); p != nil && p.Server != "" && !strings.EqualFold(p.Server, server) {
			name = profileNameFor(worker)
			if p := f.Profile(name); p != nil && p.Server != "" && !strings.EqualFold(p.Server, server) {
				name = "cloudflare-" + name
			}
		}
	}

	gateway := clientconfig.GatewayURL(server, "")
	var info *deploy.Info
	for attempt := 0; ; attempt++ {
		if info, err = a.fetchInfo(ctx, a.client(gateway, "")); err == nil || attempt == 2 || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(a.deps.retryDelay):
		}
	}
	var user *identity.User
	if err != nil {
		a.warnf("%s is not answering yet (%v); saving the login anyway — check it later with `backyard whoami`", server, err)
	} else {
		mctx, cancel := context.WithTimeout(ctx, apiTimeout)
		user, err = a.client(gateway, token).Me(mctx)
		cancel()
		if err != nil {
			a.warnf("the new server did not accept its deploy token (%v); run the `backyard login` command printed above", explain(err, nil))
			return nil
		}
	}

	p := f.Profile(name).Clone()
	if p == nil {
		p = &clientconfig.Profile{}
	}
	if p.Server != "" && !strings.EqualFold(p.Server, server) {
		p.Method = ""
	}
	p.ClearCredentials()
	p.Server, p.Gateway, p.Token, p.Login, p.Info = server, "", token, clientconfig.LoginToken, info
	if user != nil {
		p.User = &clientconfig.User{ID: user.ID, Email: user.Email, Name: user.Name, Admin: user.Admin}
	}
	f.Set(name, p)
	f.Current = name
	if err := f.Save(path); err != nil {
		return err
	}
	a.errf("\nThis CLI is now logged in to %s (profile %q) with the deploy token.\n", server, name)
	a.errf("Next: backyard init my-site && cd my-site && backyard deploy\n")
	return nil
}

// profileNameFor turns a Worker name into a CLI profile name.
func profileNameFor(worker string) string {
	if worker != "" && clientconfig.ValidProfileName(worker) {
		return worker
	}
	return "cloudflare"
}

// savedCloudflareToken returns the deploy token that setup.mjs saved for a
// Worker served at one of urls (and the state file holding it), or "".
func (a *cliApp) savedCloudflareToken(urls ...string) (token, path string) {
	cwd, err := a.cwd()
	if err != nil {
		return "", ""
	}
	dir, err := a.cloudflareStateDir(cwd)
	if err != nil {
		return "", ""
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var st cloudflareState
		if json.Unmarshal(b, &st) != nil || strings.TrimSpace(st.DeployToken) == "" {
			continue
		}
		saved, err := clientconfig.NormalizeURL(st.URL)
		if err != nil {
			continue
		}
		for _, u := range urls {
			if u != "" && strings.EqualFold(strings.TrimSuffix(u, "/"), saved) {
				return strings.TrimSpace(st.DeployToken), m
			}
		}
	}
	return "", ""
}
