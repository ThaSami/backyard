package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
)

// `backyard cloudflare setup|domain|access|status` runs backyard-cloudflare,
// the engine of the Workers edition (deploy/cloudflare, Node.js), and
// afterwards logs this CLI in to the Worker. The engine is a checkout's
// deploy/cloudflare or the package of this release (cloudflare_engine.go).
// It reports what it did in a result file (BACKYARD_CF_RESULT_FILE), so this
// CLI never guesses from the state files which Worker was deployed.

const (
	// cloudflareRel is the Workers edition inside the repository.
	cloudflareRel = "deploy/cloudflare"
	// cloudflareDocs documents the Workers edition.
	cloudflareDocs = "https://github.com/thasami/backyard/tree/main/deploy/cloudflare#readme"
	// cloudflarePackageName is the npm package name of the engine.
	cloudflarePackageName = "backyard-cloudflare"
	// cloudflareEntry is the engine's entry point, relative to its package.
	cloudflareEntry = "bin/backyard-cloudflare.mjs"
	// cloudflareInvokedAs is how the engine's hints name these commands
	// (BACKYARD_CF_INVOKED_AS).
	cloudflareInvokedAs = "backyard cloudflare"
	// cloudflareNextStep is what to do once setup has logged this CLI in.
	cloudflareNextStep = "backyard init my-site && cd my-site && backyard deploy"
	// cloudflareResultSchema is the result file schema this CLI reads.
	cloudflareResultSchema = 1
)

// cloudflareFlags are the flags this CLI handles itself; everything else
// goes to the engine.
type cloudflareFlags struct {
	dir         string
	pkg         string
	skipInstall bool
	noLogin     bool
	// profile and verbose are the root flags --profile and -v/--verbose
	// (cobra does not parse flags of these commands).
	profile string
	verbose bool
}

// cloudflareGroups are the engine's grouped commands and their subcommands.
var cloudflareGroups = map[string][]string{"domain": {"add", "remove"}, "access": {"enable", "disable"}}

func (a *cliApp) newCloudflareCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloudflare",
		Short: "Run Backyard on your Cloudflare account (free plan): setup, domain, access, status",
		Long: `Run Backyard on your own Cloudflare account, on the free plan: one Worker with SQLite-backed
Durable Objects, Workers AI and an optional R2 bucket. No checkout, build or Terraform needed.

  setup                   deploy or upgrade (re-running keeps every choice)
  domain add <host>       your own domain (--host-mode: sites at https://<site>.<host>/)
  domain remove           back to workers.dev
  access enable|disable   company login with Cloudflare Access
  status                  read-only live check

These commands run backyard-cloudflare, the engine of the Workers edition: the package of
this CLI release, installed once into your user cache and checked against the SHA-256 built
into this CLI, or the deploy/cloudflare of a Backyard checkout with --dir (a CLI built from
source uses the checkout it runs in). Afterwards this CLI is logged in to the Worker with
its deploy token, so "backyard init" and "backyard deploy" work right away.

Requires Node.js 22 or later and npm.`,
		Example: `  backyard cloudflare setup
  backyard cloudflare domain add backyard.acme.com
  backyard cloudflare access enable --allow-email-domain acme.com
  backyard cloudflare status`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.SetUsageTemplate(cloudflareUsageTemplate)
	for _, s := range cloudflareSubcommands() {
		sub := &cobra.Command{
			Use:                   s.use,
			Short:                 s.short,
			Long:                  cloudflareLong(s.intro, s.engine...),
			Example:               s.example,
			DisableFlagParsing:    true,
			DisableFlagsInUseLine: true,
			RunE: func(c *cobra.Command, args []string) error {
				return a.runCloudflare(c.Context(), c, s.name, args)
			},
		}
		sub.SetHelpTemplate(cloudflareHelpTemplate)
		cmd.AddCommand(sub)
	}
	return cmd
}

// cloudflareUsageTemplate is the usage of `backyard cloudflare` itself:
// the root's --server, --gateway and --token do not apply to it.
const cloudflareUsageTemplate = `Usage:
  {{.CommandPath}} <command> [options]
{{- if .HasExample}}

Examples:
{{.Example}}{{end}}

Commands:{{range .Commands}}{{if .IsAvailableCommand}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

Run "{{.CommandPath}} <command> --help" for the options of a command.
`

// cloudflareHelpTemplate prints the usage line first, then the long help
// (which lists the options itself: cobra parses no flags of these commands).
const cloudflareHelpTemplate = `Usage:
  {{.UseLine}}

{{.Long | trimTrailingWhitespaces}}
{{- if .HasExample}}

Examples:
{{.Example}}{{end}}
`

// splitCloudflareArgs separates the flags this CLI handles (--dir,
// --package, --skip-install, --no-login, and the root flags --profile and
// -v/--verbose) from the engine's arguments. It stops at a literal "--":
// everything after it goes to the engine verbatim (the "--" itself is
// dropped). help reports -h/--help before "--": the caller prints this
// CLI's help and runs nothing.
func splitCloudflareArgs(args []string) (fl cloudflareFlags, rest []string, help bool, err error) {
	rest = []string{}
	var firstErr error
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if !strings.HasPrefix(arg, "--") {
			name, hasValue = arg, false
		}
		// takeValue returns the flag's value (--flag=v or --flag v).
		takeValue := func() (string, error) {
			if hasValue {
				if value == "" {
					return "", fmt.Errorf("%s needs a value", name)
				}
				return value, nil
			}
			if i+1 >= len(args) || args[i+1] == "--" {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return args[i], nil
		}
		noValue := func() error {
			if hasValue {
				return fmt.Errorf("%s takes no value", name)
			}
			return nil
		}
		switch name {
		case "-h", "--help":
			help = true
		case "--dir":
			fl.dir, err = takeValue()
		case "--package":
			fl.pkg, err = takeValue()
		case "--profile":
			fl.profile, err = takeValue()
		case "--skip-install":
			err = noValue()
			fl.skipInstall = true
		case "--no-login":
			err = noValue()
			fl.noLogin = true
		case "-v", "--verbose":
			err = noValue()
			fl.verbose = true
		case "--server", "--gateway", "--token":
			err = fmt.Errorf("%s is not used by backyard cloudflare", name)
		default:
			rest = append(rest, arg)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
		err = nil
	}
	switch {
	case help: // help wins over every other problem of the line
		return fl, rest, true, nil
	case firstErr != nil:
		return cloudflareFlags{}, nil, false, firstErr
	}
	return fl, rest, false, nil
}

// runCloudflare runs the engine command engineCmd ("setup", "domain",
// "access" or "status") with args (this CLI's flags removed), then applies
// the engine's result: profiles of the Worker's old URL move to the new one,
// and setup/domain log this CLI in with the deploy token. The engine's exit
// code is kept (an *ExitError for 2, 3, 4…), also after a login.
func (a *cliApp) runCloudflare(ctx context.Context, cmd *cobra.Command, engineCmd string, args []string) error {
	fl, rest, help, err := splitCloudflareArgs(args)
	if err != nil {
		return &ExitError{Code: 2, Err: fmt.Errorf("%w (see backyard cloudflare %s --help)", err, engineCmd)}
	}
	if help {
		return cmd.Help()
	}
	if fl.profile != "" {
		a.g.profile = fl.profile
	}
	if fl.verbose {
		a.g.verbose = true
		a.log = slog.New(slog.NewTextHandler(a.streams.ErrOut, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	if subs, grouped := cloudflareGroups[engineCmd]; grouped {
		if len(rest) == 0 {
			if err := cmd.Help(); err != nil {
				return err
			}
			return &ExitError{Code: 2, Err: errSilent}
		}
		if !slices.Contains(subs, rest[0]) {
			return &ExitError{Code: 2, Err: fmt.Errorf("%q is not a command: use %s (see backyard cloudflare %s --help)",
				engineCmd+" "+rest[0], engineCmd+" "+strings.Join(subs, " or "+engineCmd+" "), engineCmd)}
		}
	}
	if fl.dir != "" && fl.pkg != "" {
		return &ExitError{Code: 2, Err: errors.New("--dir and --package cannot be combined: --dir runs a checkout's deploy/cloudflare, --package an installed package")}
	}
	if err := a.requireNode(ctx); err != nil {
		return err
	}
	eng, err := a.resolveCloudflareEngine(ctx, fl)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "backyard-cf-*")
	if err != nil {
		return fmt.Errorf("create a temporary directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	resultPath := filepath.Join(tmp, "result.json")
	env := []string{"BACKYARD_CF_RESULT_FILE=" + resultPath, "BACKYARD_CF_INVOKED_AS=" + cloudflareInvokedAs}
	if !fl.noLogin {
		// This CLI logs itself in with the deploy token afterwards and prints
		// the next step: setup leaves its own `backyard login` line out.
		env = append(env, "BACKYARD_CF_CLI_LOGIN=1")
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && !filepath.IsAbs(x) {
		cwd, err := a.cwd()
		if err != nil {
			return err
		}
		// The engine may run in another directory: resolve it here.
		env = append(env, "XDG_CONFIG_HOME="+absFrom(cwd, x))
	}
	run := Command{Name: "node", Args: append([]string{eng.entry, engineCmd}, rest...), Dir: eng.dir, Env: env,
		Stdin: a.streams.In, Stdout: a.streams.Out, Stderr: a.streams.ErrOut}
	a.log.Debug("running the Cloudflare engine", "command", run.String(), "dir", run.Dir)
	code := 0
	if err := a.deps.runner.Run(ctx, run); err != nil {
		var ee *ExitError
		if !errors.As(err, &ee) {
			return fmt.Errorf("run backyard-cloudflare: %w", err)
		}
		if ee.Code < 0 {
			return fmt.Errorf("backyard-cloudflare did not finish: %w", err)
		}
		code = ee.Code // the engine printed its own message
	}
	if (code == 0 || code == 4) && !fl.noLogin {
		res, err := readCloudflareResult(resultPath)
		switch {
		case err != nil:
			a.warnf("could not update this CLI's profiles: %v", err)
			if engineCmd == "setup" {
				a.cloudflareLoginHint("", nil)
			}
		case res != nil:
			if err := a.applyCloudflareResult(ctx, res); err != nil {
				if res.Command == "setup" {
					a.cloudflareLoginHint(res.URL, res)
				}
				if code == 0 {
					return err
				}
				a.warnf("%v", err)
			}
		}
	}
	if code != 0 {
		return &ExitError{Code: code, Err: errSilent}
	}
	return nil
}

// cloudflareResult is what the engine writes to BACKYARD_CF_RESULT_FILE
// after a command that saved its state (schema 1, no secrets).
type cloudflareResult struct {
	Schema        int    `json:"schema"`
	Command       string `json:"command"`
	Worker        string `json:"worker"`
	StatePath     string `json:"statePath"`
	URL           string `json:"url"`
	PreviousURL   string `json:"previousUrl"`
	WorkersDevURL string `json:"workersDevUrl"`
	// AuthMode is "none" or "access".
	AuthMode string `json:"authMode"`
	// DeployToken says where the deploy token is: "state" (the state
	// file), "env" ($DEPLOY_TOKEN) or "unknown".
	DeployToken string `json:"deployToken"`
}

// readCloudflareResult reads the engine's result file (nil when the command
// wrote none, e.g. --help, setup --dry-run or status).
func readCloudflareResult(path string) (*cloudflareResult, error) {
	b, err := readSmallFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the engine's result: %w", err)
	}
	var r cloudflareResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse the engine's result: %w", err)
	}
	if r.Schema != cloudflareResultSchema {
		return nil, fmt.Errorf("the engine's result has schema %d, this CLI knows %d (update the CLI)", r.Schema, cloudflareResultSchema)
	}
	return &r, nil
}

// readSmallFile reads a file of at most 1 MiB.
func readSmallFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	return b, nil
}

// applyCloudflareResult updates this CLI after an engine command: the
// profiles of the Worker's previous URLs move to its new one; setup and
// domain add/remove log in with the deploy token, access enable/disable
// refresh the server info of the profiles on the Worker. After setup it
// prints the next step once the current profile is the Worker's.
func (a *cliApp) applyCloudflareResult(ctx context.Context, res *cloudflareResult) error {
	server, err := clientconfig.NormalizeURL(res.URL)
	if err != nil {
		a.cloudflareLoginHint("", res)
		return nil
	}
	if err := a.repointCloudflareProfiles(server, res.PreviousURL, res.WorkersDevURL); err != nil {
		return err
	}
	switch res.Command {
	case "setup", "domain add", "domain remove":
		current, err := a.cloudflareLoginFromResult(ctx, server, res)
		if err == nil && current && res.Command == "setup" {
			a.errf("Next: %s\n", cloudflareNextStep)
		}
		return err
	case "access enable", "access disable":
		return a.refreshCloudflareProfiles(ctx, server, res.AuthMode == "access")
	}
	return nil
}

// cloudflareTokenRef names where the deploy token of res is, for a
// `backyard login --token` hint (never the token itself).
func cloudflareTokenRef(res *cloudflareResult) string {
	if res != nil {
		switch res.DeployToken {
		case "state":
			if res.StatePath != "" {
				return fmt.Sprintf(`<"deployToken" in %s>`, res.StatePath)
			}
		case "env":
			return "$DEPLOY_TOKEN"
		}
	}
	return "<DEPLOY_TOKEN>"
}

// cloudflareLoginHint says how to connect this CLI by hand when it could not
// log itself in. It is self-contained: with BACKYARD_CF_CLI_LOGIN=1 the
// engine prints no `backyard login` line of its own.
func (a *cliApp) cloudflareLoginHint(server string, res *cloudflareResult) {
	server = strings.TrimSpace(server)
	if _, err := clientconfig.NormalizeURL(server); err != nil {
		server = "<hub URL>"
	}
	a.errf("\nTo connect this CLI, run\n  backyard login --server %s --token %s\n", server, cloudflareTokenRef(res))
	if res == nil || server == "<hub URL>" {
		a.errf("(`%s status` shows the hub URL and the state file that holds the deploy token.)\n", cloudflareInvokedAs)
	}
}

// sameServer compares two (normalized) server URLs.
func sameServer(a, b string) bool {
	return a != "" && strings.EqualFold(strings.TrimSuffix(a, "/"), strings.TrimSuffix(b, "/"))
}

// profileServer returns a profile's server, normalized ("" when none).
func profileServer(p *clientconfig.Profile) string {
	if p == nil || p.Server == "" {
		return ""
	}
	s, err := clientconfig.NormalizeURL(p.Server)
	if err != nil {
		return ""
	}
	return s
}

// repointCloudflareProfiles points every profile on one of the Worker's
// previous URLs (previousUrl, workersDevUrl) at its URL now. Credentials
// are kept (they belong to the same Worker); the gateway and the cached
// server info are cleared.
func (a *cliApp) repointCloudflareProfiles(server string, old ...string) error {
	var from []string
	for _, u := range old {
		if n, err := clientconfig.NormalizeURL(u); err == nil && !sameServer(n, server) {
			from = append(from, n)
		}
	}
	if len(from) == 0 {
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
	changed := false
	for _, name := range f.Names() {
		p := f.Profile(name)
		ps := profileServer(p)
		if ps == "" || !slices.ContainsFunc(from, func(o string) bool { return sameServer(o, ps) }) {
			continue
		}
		a.errf("Profile %q now points to %s (was %s).\n", name, server, p.Server)
		p.Server, p.Gateway, p.Info = server, "", nil
		changed = true
	}
	if !changed {
		return nil
	}
	return f.Save(path)
}

// personalLogin reports whether p, on server, holds someone's own login
// rather than the deploy token: a browser or device login, or a personal API
// token (`backyard login --server <hub> --token <t>`, as the Worker's CLI
// token page prints it), whose user is a real identity. A profile whose
// token differs from the deploy token is not enough: after --rotate-token it
// may hold the revoked one.
func personalLogin(p *clientconfig.Profile, server string) bool {
	if p == nil || !sameServer(profileServer(p), server) {
		return false
	}
	switch p.Login {
	case clientconfig.LoginBrowser, clientconfig.LoginDevice:
		return true
	case clientconfig.LoginToken:
		return p.User != nil && p.User.ID != "" && !strings.HasPrefix(p.User.ID, "token:") && !strings.HasPrefix(p.User.ID, "anon:")
	}
	return false
}

// personalProfileFor returns the name of a profile on server holding a
// personal login (personalLogin), or "".
func personalProfileFor(f *clientconfig.File, server string) string {
	for _, name := range f.Names() {
		if personalLogin(f.Profile(name), server) {
			return name
		}
	}
	return ""
}

// keptPersonalLogin explains why a personal login was not replaced.
func (a *cliApp) keptPersonalLogin(name, server string) {
	a.errf("\nKept profile %q, your own login to %s (personal login: backyard login --server %s).\n", name, server, server)
}

// cloudflareLoginFromResult logs this CLI in with the deploy token the
// result points to. With Cloudflare Access on, an existing personal login
// for the Worker is left alone; a profile named with --profile (or
// $BACKYARD_PROFILE) is still logged in unless it is that personal login.
// current reports whether the CLI's current profile is now one for the
// Worker (logged in or kept).
func (a *cliApp) cloudflareLoginFromResult(ctx context.Context, server string, res *cloudflareResult) (current bool, err error) {
	if res.AuthMode == "access" {
		path, err := a.configPath()
		if err != nil {
			return false, err
		}
		f, err := clientconfig.Load(path)
		if err != nil {
			return false, err
		}
		explicit := strings.TrimSpace(a.g.profile)
		if explicit == "" {
			explicit = strings.TrimSpace(os.Getenv(clientconfig.EnvProfile))
		}
		kept := ""
		switch {
		case explicit != "":
			if personalLogin(f.Profile(explicit), server) {
				kept = explicit
			}
		default:
			kept = personalProfileFor(f, server)
		}
		if kept != "" {
			a.keptPersonalLogin(kept, server)
			return sameServer(profileServer(f.Profile(f.ActiveName(""))), server), nil
		}
	}
	token := ""
	switch res.DeployToken {
	case "state":
		if b, err := readSmallFile(res.StatePath); err == nil {
			var st cloudflareState
			if json.Unmarshal(b, &st) == nil {
				token = strings.TrimSpace(st.DeployToken)
			}
		} else {
			a.log.Debug("read the Cloudflare state", "path", res.StatePath, "error", err)
		}
	case "env":
		token = strings.TrimSpace(os.Getenv("DEPLOY_TOKEN"))
	}
	if token == "" {
		a.errf("\nThe deploy token is not saved on this machine: to connect this CLI, run\n  backyard login --server %s --token <DEPLOY_TOKEN>\n", server)
		return false, nil
	}
	return a.cloudflareLogin(ctx, server, token, res)
}

// refreshCloudflareProfiles refreshes the cached server info of the
// profiles on server (after Cloudflare Access was turned on or off).
func (a *cliApp) refreshCloudflareProfiles(ctx context.Context, server string, access bool) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	f, err := clientconfig.Load(path)
	if err != nil {
		return err
	}
	var names []string
	for _, name := range f.Names() {
		if sameServer(profileServer(f.Profile(name)), server) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	info, err := a.fetchInfo(ctx, a.client(clientconfig.GatewayURL(server, ""), ""))
	if err != nil {
		a.warnf("could not refresh the server info of %s (%v); it is fetched again on the next command", server, err)
		info = nil
	}
	for _, name := range names {
		f.Profile(name).Info = info
	}
	if err := f.Save(path); err != nil {
		return err
	}
	if access {
		if name := personalProfileFor(f, server); name != "" {
			a.keptPersonalLogin(name, server)
		}
	}
	return nil
}

// cloudflareLogin stores a CLI profile for the Worker at server with its
// deploy token, so `backyard deploy` works right away (the OpenQuick
// experience: setup, init, deploy). Problems reaching the server are
// warnings: the platform itself is already deployed. The profile becomes the
// current one (current is true), except when the current profile is for
// another server and the Worker already has a profile of its own (a setup
// re-run or a domain change): that one is refreshed, and the current profile
// stays.
func (a *cliApp) cloudflareLogin(ctx context.Context, server, token string, res *cloudflareResult) (current bool, err error) {
	path, err := a.configPath()
	if err != nil {
		return false, err
	}
	f, err := clientconfig.Load(path)
	if err != nil {
		return false, err
	}
	worker := res.Worker
	name := f.ActiveName(a.g.profile)
	active, makeCurrent := name, true
	if a.g.profile == "" && strings.TrimSpace(os.Getenv(clientconfig.EnvProfile)) == "" {
		// Keep an existing profile for another server: use one named after
		// the Worker instead.
		if p := f.Profile(name); p != nil && p.Server != "" && !strings.EqualFold(p.Server, server) {
			name = profileNameFor(worker)
			if p := f.Profile(name); p != nil && p.Server != "" && !strings.EqualFold(p.Server, server) {
				name = "cloudflare-" + name
			}
			// The Worker already has its profile (possibly just re-pointed):
			// refresh it, but do not switch away from the other server.
			if p := f.Profile(name); p != nil && sameServer(profileServer(p), server) {
				makeCurrent = false
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
			a.warnf("the new server did not accept its deploy token (%v); this CLI was not logged in", explain(err, nil))
			a.cloudflareLoginHint(server, res)
			return false, nil
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
	if makeCurrent {
		f.Current = name
	}
	if err := f.Save(path); err != nil {
		return false, err
	}
	if !makeCurrent {
		a.errf("\nUpdated profile %q for %s with the deploy token; the current profile is still %q (another server): "+
			"use --profile %s, or `backyard login --profile %s` to switch.\n", name, server, active, name, name)
		return false, nil
	}
	a.errf("\nThis CLI is now logged in to %s (profile %q) with the deploy token.\n", server, name)
	return true, nil
}

// profileNameFor turns a Worker name into a CLI profile name.
func profileNameFor(worker string) string {
	if worker != "" && clientconfig.ValidProfileName(worker) {
		return worker
	}
	return "cloudflare"
}

// cfFlag is one option in the help of `backyard cloudflare …`. The engine's
// options are copied exactly from deploy/cloudflare/scripts/lib/commands.json
// (TestCloudflareHelpMatchesEngine keeps the copy in sync).
type cfFlag struct {
	Name  string
	Alias []string
	Short string
	// Value is the placeholder of a string flag ("" for a bool flag).
	Value      string
	Repeatable bool
	Help       string
}

// cfCommand is an engine command (commands.json "commands").
type cfCommand struct {
	Name    string
	Args    []string
	Summary string
	Flags   []cfFlag
}

// cfGoFlags are the flags this CLI handles itself (splitCloudflareArgs).
var cfGoFlags = []cfFlag{
	{Name: "dir", Value: "path", Help: "path to deploy/cloudflare of a Backyard checkout (default: found from the current directory in builds from source)"},
	{Name: "package", Value: "path|url", Help: "a backyard-cloudflare .tgz (local file or https URL) to use instead of this release's package"},
	{Name: "skip-install", Help: "checkout only: skip npm ci (skipped anyway when node_modules is newer than package-lock.json)"},
	{Name: "no-login", Help: "do not log this CLI in or update its profiles afterwards"},
	{Name: "profile", Value: "name", Help: "CLI profile to log in (env BACKYARD_PROFILE)"},
	{Name: "verbose", Short: "v", Help: "verbose logging"},
}

// cfGlobalFlags are the engine's global options (commands.json "global").
var cfGlobalFlags = []cfFlag{
	{Name: "worker-name", Alias: []string{"name"}, Value: "name", Help: `Worker name (default "backyard")`},
	{Name: "yes", Short: "y", Help: "answer yes to confirmations (never implies --take-over, --replace-dns-record or --confirm-auth-change)"},
	{Name: "non-interactive", Help: "never prompt (also when CI is set or there is no terminal)"},
	{Name: "help", Short: "h", Help: "show help"},
}

// cfCommands are the engine's commands (commands.json "commands").
var cfCommands = []cfCommand{
	{Name: "setup", Summary: "Deploy or upgrade Backyard on your Cloudflare account (Workers free plan); re-running keeps every choice", Flags: []cfFlag{
		{Name: "r2", Help: "use an R2 bucket for big files (default when R2 is enabled)"},
		{Name: "no-r2", Help: "never use R2 (remembered)"},
		{Name: "r2-bucket", Value: "name", Help: "bucket name (default <worker>-files)"},
		{Name: "ai", Help: "serve AI with the Workers AI binding (default; turns AI back on after --no-ai)"},
		{Name: "no-ai", Help: "turn AI off: no Workers AI binding, AI calls answer 501 (remembered)"},
		{Name: "var", Value: "KEY:VALUE", Repeatable: true, Help: "override a Worker variable (remembered; KEY: drops it)"},
		{Name: "rotate-token", Help: "issue a new DEPLOY_TOKEN"},
		{Name: "rotate-secret", Help: "replace BACKYARD_SECRET (revokes every API token)"},
		{Name: "skip-deploy", Help: "only (re)set secrets"},
		{Name: "workers-subdomain", Value: "name", Help: "workers.dev subdomain to register when the account has none"},
		{Name: "workers-dev", Help: "serve on workers.dev (default)"},
		{Name: "no-workers-dev", Help: "stop serving on workers.dev (needs a custom domain)"},
		{Name: "confirm-auth-change", Help: "allow a deploy that turns Cloudflare Access off or changes its team"},
		{Name: "dry-run", Help: "build and validate only (no login, no changes)"},
	}},
	{Name: "domain add", Args: []string{"host"}, Summary: "Serve Backyard on your own domain (path mode), or sites on their own hosts (--host-mode)", Flags: []cfFlag{
		{Name: "host-mode", Help: "sites at https://<site>.<host>/ (wildcard route and DNS record)"},
		{Name: "zone", Value: "zone", Help: "Cloudflare zone of <host> (default: found automatically)"},
		{Name: "take-over", Help: "move <host> or the *.<host>/* route from another Worker to this one"},
		{Name: "replace-dns-record", Help: "replace existing DNS records at <host>"},
		{Name: "wait", Value: "seconds", Help: "how long to wait for DNS, certificate and readiness (default 300)"},
	}},
	{Name: "domain remove", Summary: "Remove the custom domain this tool added: back to workers.dev"},
	{Name: "access enable", Summary: "Turn on company login with Cloudflare Access (needs a custom domain)", Flags: []cfFlag{
		{Name: "allow-email", Value: "email", Repeatable: true, Help: "allow this email"},
		{Name: "allow-email-domain", Value: "domain", Repeatable: true, Help: "allow every email at this domain"},
		{Name: "team", Value: "team", Help: "Zero Trust team of existing Access applications"},
		{Name: "aud", Value: "tag", Help: "AUD tag of the existing hub application"},
		{Name: "wait", Value: "seconds", Help: "how long to wait for Access (default 120; 900 while waiting for dashboard steps)"},
	}},
	{Name: "access disable", Summary: "Turn company login (Cloudflare Access) off", Flags: []cfFlag{
		{Name: "delete-apps", Help: "also delete the Access applications and policies this tool created (needs BACKYARD_CF_API_TOKEN)"},
	}},
	{Name: "status", Summary: "Read-only live check of the Worker, its domain and Cloudflare Access", Flags: []cfFlag{
		{Name: "json", Help: "machine-readable output"},
	}},
}

// cfSubcommand describes one `backyard cloudflare` subcommand.
type cfSubcommand struct {
	// name is the engine command it runs ("domain" for "domain add|remove").
	name    string
	use     string
	short   string
	intro   string
	example string
	// engine lists the engine commands whose options it documents.
	engine []string
}

func cloudflareSubcommands() []cfSubcommand {
	return []cfSubcommand{
		{
			name: "setup", use: "setup [options]", engine: []string{"setup"},
			short: "Deploy or upgrade Backyard on your Cloudflare account (Workers free plan)",
			intro: `Deploy or upgrade Backyard on your Cloudflare account (Workers free plan); re-running keeps
every choice: secrets, the deploy token, R2, AI, the domain, Access and the --var overrides.

It logs in with wrangler (in the browser) when needed, asks which account to use when the
login reaches several, registers the account's workers.dev subdomain when it has none,
creates the R2 bucket when R2 is enabled, sets the secrets and deploys. A machine without
saved setup state adopts the deployed configuration first, and a deploy that would turn a
live Cloudflare Access off is refused. Afterwards this CLI is logged in to the Worker with
its deploy token (unless --no-login).`,
			example: `  backyard cloudflare setup
  backyard cloudflare setup --worker-name team --no-r2
  backyard cloudflare setup --no-ai                                      # an account without Workers AI
  backyard cloudflare setup --var ADMINS:alice@acme.com
  backyard cloudflare setup --non-interactive --workers-subdomain acme   # CI
  backyard cloudflare setup --package ./dist-npm/backyard-cloudflare-0.1.0.tgz
  backyard cloudflare setup -- --help                                    # the engine's own help`,
		},
		{
			name: "domain", use: "domain (add <host> | remove) [options]", engine: []string{"domain add", "domain remove"},
			short: "Serve Backyard on your own domain (domain add <host>), or back on workers.dev (domain remove)",
			intro: `Serve Backyard on your own domain. "domain add <host>" attaches <host> to the Worker as a
Workers custom domain (Cloudflare creates its DNS record and certificate) and moves the hub
there: sites at https://<host>/s/<site>/. With --host-mode sites get their own hosts,
https://<site>.<host>/: it also adds the route *.<host>/* and a proxied wildcard DNS record,
created with $BACKYARD_CF_API_TOKEN (a token with Zone · DNS · Edit) or added by you in the
dashboard from the printed values (Backyard detects it). Existing DNS records and other
Workers' domains are never replaced without --replace-dns-record or --take-over.

"domain remove" detaches the domain again: the hub moves back to workers.dev. Afterwards the
profiles of this CLI on the old URL point to the new one.`,
			example: `  backyard cloudflare domain add backyard.acme.com
  backyard cloudflare domain add acme-sites.dev --host-mode
  backyard cloudflare domain remove`,
		},
		{
			name: "access", use: "access (enable | disable) [options]", engine: []string{"access enable", "access disable"},
			short: "Turn company login with Cloudflare Access on (access enable) or off (access disable)",
			intro: `Company login with Cloudflare Access; it needs a custom domain (domain add). "access enable"
puts two self-hosted Access applications in front of the hub: one for people (who may sign
in: --allow-email, --allow-email-domain) and one that lets the machine endpoints the Worker
authenticates itself (deploys, token minting, health checks) through. With
$BACKYARD_CF_API_TOKEN (Access: Apps and Policies · Edit and Access: Organizations, Identity
Providers, and Groups · Read) Backyard creates them; otherwise it prints the dashboard steps
and detects the result (--team and --aud name applications you already have). Access is
verified at the edge before the Worker switches to it.

"access disable" switches the Worker back to deploy tokens (--delete-apps also deletes the
applications this tool created). Teammates sign in with "backyard login --server <hub>".`,
			example: `  backyard cloudflare access enable --allow-email-domain acme.com
  BACKYARD_CF_API_TOKEN=… backyard cloudflare access enable --allow-email alice@acme.com --non-interactive
  backyard cloudflare access enable --team acme.cloudflareaccess.com --aud <AUD tag>
  backyard cloudflare access disable --delete-apps`,
		},
		{
			name: "status", use: "status [options]", engine: []string{"status"},
			short: "Read-only live check of the Worker, its domain and Cloudflare Access",
			intro: `Read-only live check of the Worker, its domain and Cloudflare Access: versions, URLs, the R2
bucket, workers.dev, the custom domain (and its route, DNS record and certificate in host
mode), Access and the deploy token. Each check prints ok, pending or failed. It never logs
in: without a wrangler login the live Cloudflare checks are skipped. Exit code 0 when every
check is ok, 4 when one is pending or failed, 1 when the Worker does not exist.`,
			example: `  backyard cloudflare status
  backyard cloudflare status --json --worker-name team`,
		},
	}
}

// cloudflareEngineNote explains where the engine comes from.
const cloudflareEngineNote = `Everything else is passed to the engine, backyard-cloudflare, unchanged, and so is everything
after a literal "--" ("backyard cloudflare setup -- --help" shows the engine's own help).
With --dir, or when a CLI built from source runs inside a checkout of the Backyard repository,
the engine is that deploy/cloudflare (after npm ci, unless node_modules is newer than
package-lock.json). Otherwise it is the package of this CLI release, downloaded once from
$BACKYARD_GITHUB_URL (default https://github.com) into your user cache and checked against
the SHA-256 built into this CLI. Requires Node.js 22 or later and npm.`

// cloudflareExitCodes are the exit codes of the engine and of these commands.
const cloudflareExitCodes = `Exit codes:
  0    done, or nothing to do
  1    a Cloudflare, wrangler, network or file operation failed (a re-run resumes)
  2    usage error, or input or confirmation needed in non-interactive mode
  3    refused for safety (conflict, foreign object, auth downgrade…): nothing was changed
  4    applied, but verification (DNS, certificate, readiness, Access) is pending or failed:
       re-running the same command resumes
  130  interrupted`

// findCFCommand returns the engine command name, or nil.
func findCFCommand(name string) *cfCommand {
	for i := range cfCommands {
		if cfCommands[i].Name == name {
			return &cfCommands[i]
		}
	}
	return nil
}

// cfFlagLabel renders `--zone <zone>` or `-y, --yes` (like the engine's help).
func cfFlagLabel(f cfFlag) string {
	l := "--" + f.Name
	if f.Value != "" {
		l += " <" + f.Value + ">"
	}
	if f.Short != "" {
		l = "-" + f.Short + ", " + l
	}
	return l
}

// cfFlagHelp renders a flag's help with its aliases and repeatability.
func cfFlagHelp(f cfFlag) string {
	var extra []string
	if len(f.Alias) > 0 {
		aliases := make([]string, len(f.Alias))
		for i, al := range f.Alias {
			aliases[i] = "--" + al
		}
		extra = append(extra, "alias "+strings.Join(aliases, ", "))
	}
	if f.Repeatable {
		extra = append(extra, "repeatable")
	}
	if len(extra) == 0 {
		return f.Help
	}
	return f.Help + " (" + strings.Join(extra, "; ") + ")"
}

// cloudflareLong builds the long help of a subcommand: intro, the options of
// its engine commands, the global options, this CLI's flags, where the
// engine comes from, and the exit codes.
func cloudflareLong(intro string, engine ...string) string {
	cmds := make([]*cfCommand, 0, len(engine))
	all := slices.Concat(cfGlobalFlags, cfGoFlags)
	for _, name := range engine {
		c := findCFCommand(name)
		if c == nil {
			panic("cloudflare help: unknown engine command " + name)
		}
		cmds = append(cmds, c)
		all = append(all, c.Flags...)
	}
	width := 0
	for _, f := range all {
		width = max(width, len(cfFlagLabel(f))+2)
	}
	width = min(width, 32)
	table := func(b *strings.Builder, flags []cfFlag) {
		for _, f := range flags {
			l := cfFlagLabel(f)
			if len(l)+2 > width {
				fmt.Fprintf(b, "  %s\n  %s%s\n", l, strings.Repeat(" ", width), cfFlagHelp(f))
			} else {
				fmt.Fprintf(b, "  %-*s%s\n", width, l, cfFlagHelp(f))
			}
		}
	}
	var b strings.Builder
	b.WriteString(intro + "\n\n")
	for _, c := range cmds {
		if len(cmds) == 1 {
			b.WriteString("Options:\n")
		} else {
			fmt.Fprintf(&b, "Options of %s%s:\n", c.Name, cfArgs(c))
		}
		if len(c.Flags) == 0 {
			b.WriteString("  (only the global options)\n")
		}
		table(&b, c.Flags)
		b.WriteString("\n")
	}
	b.WriteString("Global options:\n")
	table(&b, cfGlobalFlags)
	b.WriteString("\nFlags of this CLI (not passed to the engine):\n")
	table(&b, cfGoFlags)
	b.WriteString("\n" + cloudflareEngineNote + "\n\n" + cloudflareExitCodes)
	return b.String()
}

// cfArgs renders a command's positional arguments (" <host>").
func cfArgs(c *cfCommand) string {
	var s string
	for _, a := range c.Args {
		s += " <" + a + ">"
	}
	return s
}
