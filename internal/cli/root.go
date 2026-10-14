// Package cli implements the `backyard` command line (SPEC §13): the server
// entry points (serve, gateway, dev), the developer workflow (init, deploy,
// login, sites, open, pull, token, skill), the MCP server for agents, and
// `backyard cloudflare setup|domain|access|status` for the Workers edition.
//
// NewRootCommand builds the cobra command tree; every side effect (process
// execution, browser launch, templates, the embedded skill, the server
// runtime) goes through an injectable dependency so commands are testable
// end to end against a fake gateway.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/app"
	"github.com/thasami/backyard/internal/cli/scaffold"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/version"
	"github.com/thasami/backyard/skills"
	"github.com/thasami/backyard/templates"
)

// IOStreams are the standard streams of a command invocation.
type IOStreams struct {
	In     io.Reader
	Out    io.Writer
	ErrOut io.Writer
}

// StdStreams returns the process's stdin, stdout and stderr.
func StdStreams() IOStreams { return IOStreams{In: os.Stdin, Out: os.Stdout, ErrOut: os.Stderr} }

// ExitError carries a specific process exit code.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// errSilent marks an error whose message was already printed.
var errSilent = errors.New("")

// deps are the side effects commands perform; tests replace them.
type deps struct {
	runner      Runner
	lookPath    func(string) (string, error)
	openBrowser func(ctx context.Context, url string) error
	render      scaffold.Renderer
	skillFS     func() (fs.FS, error)
	runServer   func(ctx context.Context, cfg *config.Config, opts app.Options) error
	// httpClient, when set, replaces the HTTP client of gateway and IdP calls.
	httpClient *http.Client
	now        func() time.Time
	getwd      func() (string, error)
	homeDir    func() (string, error)
	cacheDir   func() (string, error)
	// stdinTTY / stderrTTY report whether the streams are terminals.
	stdinTTY  func() bool
	stderrTTY func() bool
	// configPath overrides the CLI config file location.
	configPath     string
	loginTimeout   time.Duration
	devicePollUnit time.Duration
	// releaseBase is where `backyard cloudflare` downloads the
	// backyard-cloudflare package of its release:
	// <base>v<version>/backyard-cloudflare-<version>.tgz.
	releaseBase string
	// retryDelay spaces out retries of a freshly deployed server's /info.
	retryDelay time.Duration
}

// defaultDeps returns the production dependencies for streams.
func defaultDeps(streams IOStreams) *deps {
	return &deps{
		runner:      execRunner{},
		lookPath:    lookPath,
		openBrowser: openInBrowser,
		render:      templates.Render,
		skillFS: func() (fs.FS, error) {
			return fs.Sub(skills.FS(), "backyard")
		},
		now:            time.Now,
		getwd:          os.Getwd,
		homeDir:        os.UserHomeDir,
		cacheDir:       os.UserCacheDir,
		stdinTTY:       func() bool { return isTerminal(streams.In) },
		stderrTTY:      func() bool { return isTerminal(streams.ErrOut) },
		loginTimeout:   5 * time.Minute,
		devicePollUnit: time.Second,
		releaseBase:    cloudflareReleaseBase(os.Getenv("BACKYARD_GITHUB_URL")),
		retryDelay:     3 * time.Second,
	}
}

// globalOpts are the persistent flags.
type globalOpts struct {
	profile string
	server  string
	gateway string
	token   string
	json    bool
	verbose bool
}

// cliApp is the state shared by the commands of one invocation.
type cliApp struct {
	streams IOStreams
	deps    *deps
	g       globalOpts
	log     *slog.Logger

	inOnce sync.Once
	in     *bufio.Reader
}

// NewRootCommand builds the `backyard` command tree writing to streams.
func NewRootCommand(streams IOStreams) *cobra.Command {
	return newRootCommand(streams, defaultDeps(streams))
}

func newRootCommand(streams IOStreams, d *deps) *cobra.Command {
	if streams.In == nil {
		streams.In = strings.NewReader("")
	}
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.ErrOut == nil {
		streams.ErrOut = io.Discard
	}
	a := &cliApp{streams: streams, deps: d, log: slog.New(slog.DiscardHandler)}
	root := &cobra.Command{
		Use:   "backyard",
		Short: "Drop a folder of HTML, get an internal URL with a zero-config backend",
		Long: `Backyard turns a folder of HTML into a URL that only people inside your company can
see. Every site gets a zero-config client API: database + realtime, file uploads,
AI, data warehouse queries, websockets and identity.

Get started:
  backyard login --server https://backyard.example.com
  backyard init my-site && cd my-site
  backyard dev          # local preview with a local backend
  backyard deploy       # publish

No server yet? Run your own on Cloudflare's free tier (logs this CLI in when done):
  backyard cloudflare setup`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			level := slog.LevelWarn
			if a.g.verbose {
				level = slog.LevelDebug
			}
			a.log = slog.New(slog.NewTextHandler(a.streams.ErrOut, &slog.HandlerOptions{Level: level}))
		},
	}
	root.SetIn(streams.In)
	root.SetOut(streams.Out)
	root.SetErr(streams.ErrOut)
	root.SetVersionTemplate("backyard {{.Version}}\n")
	pf := root.PersistentFlags()
	pf.StringVar(&a.g.profile, "profile", "", "CLI profile to use (env BACKYARD_PROFILE)")
	pf.StringVar(&a.g.server, "server", "", "Backyard server (hub) URL (env BACKYARD_SERVER)")
	pf.StringVar(&a.g.gateway, "gateway", "", "deploy gateway URL, if not <server>/__backyard/api/deploy (env BACKYARD_GATEWAY)")
	pf.StringVar(&a.g.token, "token", "", "API token (env BACKYARD_TOKEN)")
	pf.BoolVar(&a.g.json, "json", false, "machine-readable JSON output")
	pf.BoolVarP(&a.g.verbose, "verbose", "v", false, "verbose logging")

	root.AddGroup(
		&cobra.Group{ID: "dev", Title: "Build and publish sites:"},
		&cobra.Group{ID: "account", Title: "Account and sites:"},
		&cobra.Group{ID: "server", Title: "Run and operate Backyard:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("dev", a.newInitCommand(), a.newDevCommand(), a.newDeployCommand(), a.newPullCommand(),
		a.newSkillCommand(), a.newMCPCommand())
	add("account", a.newLoginCommand(), a.newLogoutCommand(), a.newWhoamiCommand(), a.newSitesCommand(),
		a.newOpenCommand(), a.newTokenCommand())
	add("server", a.newServeCommand(), a.newGatewayCommand(), a.newConfigCommand(), a.newCloudflareCommand(),
		a.newVersionCommand())
	return root
}

// Execute runs the command line args with streams and returns the process
// exit code. The first SIGINT/SIGTERM cancels the command's context (so
// servers shut down gracefully and transfers stop cleanly); a second one
// terminates the process with the default signal behaviour.
func Execute(ctx context.Context, streams IOStreams, args []string) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop() // restore the default handlers: a second signal kills
	}()
	return execute(ctx, newRootCommand(streams, defaultDeps(streams)), streams, args)
}

func execute(ctx context.Context, root *cobra.Command, streams IOStreams, args []string) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(streams.ErrOut, "Error: internal error: %v\n", r)
			if slices.Contains(args, "--verbose") || slices.Contains(args, "-v") {
				fmt.Fprintf(streams.ErrOut, "%s\n", debug.Stack())
			} else {
				fmt.Fprintln(streams.ErrOut, "Run again with --verbose for details, and please report this bug.")
			}
			code = 2
		}
	}()
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	code = 1
	var ee *ExitError
	if errors.As(err, &ee) {
		code = ee.Code
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		fmt.Fprintln(streams.ErrOut, "interrupted")
		return 130
	}
	if msg := err.Error(); msg != "" && !errors.Is(err, errSilent) {
		fmt.Fprintln(streams.ErrOut, "Error: "+msg)
		if strings.HasPrefix(msg, "unknown command") || strings.HasPrefix(msg, "unknown flag") ||
			strings.HasPrefix(msg, "unknown shorthand flag") || strings.Contains(msg, "accepts ") {
			fmt.Fprintln(streams.ErrOut, "Run 'backyard --help' for usage.")
		}
	}
	return code
}

// Main is the entry point used by cmd/backyard.
func Main() int { return Execute(context.Background(), StdStreams(), os.Args[1:]) }
