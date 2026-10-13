package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/app"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/sites"
)

func (a *cliApp) newServeCommand() *cobra.Command {
	var cfgPath string
	var dev bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Backyard server",
		Long: `Run the Backyard server: hub, sites, site APIs, websockets and the embedded deploy gateway.

Configuration comes from --config, $BACKYARD_CONFIG_YAML, $BACKYARD_CONFIG or ./backyard.yaml.
--dev fills in a zero-dependency local setup (sqlite + files under ./.backyard-data, no auth).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath, dev)
			if err != nil {
				return err
			}
			return a.runServer(cmd.Context(), cfg, app.Options{})
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "config file (default $BACKYARD_CONFIG or ./backyard.yaml)")
	cmd.Flags().BoolVar(&dev, "dev", false, "developer defaults: localhost, no auth, local storage")
	return cmd
}

func (a *cliApp) newGatewayCommand() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Run only the deploy gateway",
		Long: `Run only the deploy gateway (SPEC §7) as a standalone service: no sites, no site APIs,
bearer-token authentication only. Use it when the hub sits behind an identity-aware proxy
that CLIs cannot pass.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath, false)
			if err != nil {
				return err
			}
			return a.runServer(cmd.Context(), cfg, app.Options{GatewayOnly: true})
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "", "config file (default $BACKYARD_CONFIG or ./backyard.yaml)")
	return cmd
}

// runServer builds and runs the app until ctx is cancelled.
func (a *cliApp) runServer(ctx context.Context, cfg *config.Config, opts app.Options) error {
	if opts.Logger == nil {
		opts.Logger = newServerLogger(cfg.Telemetry, a.streams.ErrOut, a.g.verbose)
	}
	if a.deps.runServer != nil {
		return a.deps.runServer(ctx, cfg, opts)
	}
	slog.SetDefault(opts.Logger)
	application, err := app.New(ctx, cfg, opts)
	if err != nil {
		return err
	}
	defer func() {
		if err := application.Close(); err != nil {
			opts.Logger.Warn("shutdown: closing resources failed", "error", err)
		}
	}()
	return application.Run(ctx)
}

// newServerLogger builds the server logger from telemetry settings.
func newServerLogger(t config.TelemetryConfig, w io.Writer, verbose bool) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(t.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	if verbose {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if t.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func (a *cliApp) newDevCommand() *cobra.Command {
	var (
		port    int
		site    string
		noOpen  bool
		cfgPath string
	)
	cmd := &cobra.Command{
		Use:   "dev [dir]",
		Short: "Preview a site locally with a local backend",
		Long: `Serve a folder as a live site at http://<site>.localhost:<port>/ with a complete local
backend (database, realtime, files, AI if configured) — edits show up on reload.

Data is kept in ./.backyard-data. Pass --config to use AI providers or a warehouse from a
config file; the server still runs in dev mode on localhost.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if port < 1 || port > 65535 {
				return fmt.Errorf("invalid --port %d", port)
			}
			cwd, err := a.cwd()
			if err != nil {
				return err
			}
			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			pf, err := clientconfig.FindProject(absFrom(cwd, arg))
			if err != nil {
				return err
			}
			dir := absFrom(cwd, arg)
			if arg == "" && pf != nil {
				if dir, err = pf.DeployDir(); err != nil {
					return err
				}
			}
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			if site == "" && pf != nil {
				site = pf.Site
			}
			if site == "" {
				if pf != nil {
					site = defaultSiteName(pf.Root())
				} else {
					site = defaultSiteName(dir)
				}
			}
			if err := sites.ValidateName(site, config.DefaultReserved); err != nil {
				return fmt.Errorf("%w (choose another with --site)", err)
			}
			cfgFile := ""
			if cfgPath != "" {
				cfgFile = absFrom(cwd, cfgPath)
			}
			cfg, err := devConfig(cfgFile, port, cwd)
			if err != nil {
				return err
			}
			siteURL := fmt.Sprintf("http://%s.localhost:%d/", site, port)
			a.errf("Backyard dev server\n")
			a.errf("  site:  %s\n", siteURL)
			a.errf("  hub:   %s/\n", cfg.Server.PublicURL)
			// Outside browsers and curl, *.localhost often does not
			// resolve (containers, CI): name the site by Host header.
			a.errf("  check: curl -s -H 'Host: %s.localhost:%d' http://127.0.0.1:%d/\n", site, port, port)
			a.errf("  dir:   %s (served live)\n", dir)
			if cfg.Storage.Type == "fs" {
				a.errf("  data:  %s\n", cfg.Storage.FS.Root)
			}
			a.errf("Press Ctrl+C to stop.\n\n")
			if a.g.json {
				_ = a.printJSON(map[string]string{"site": site, "url": siteURL, "hub": cfg.Server.PublicURL + "/", "dir": dir})
			} else {
				a.outf("%s\n", siteURL)
			}
			if !noOpen {
				go a.openWhenReady(ctx, port, siteURL)
			}
			return a.runServer(ctx, cfg, app.Options{DevSites: map[string]string{site: dir}})
		},
	}
	f := cmd.Flags()
	f.IntVarP(&port, "port", "p", 4321, "port to listen on (127.0.0.1)")
	f.StringVar(&site, "site", "", "site name (default: .backyard.json site or the folder name)")
	f.BoolVar(&noOpen, "no-open", false, "do not open the browser")
	f.StringVarP(&cfgPath, "config", "c", "", "optional config file (AI providers, warehouse…)")
	return cmd
}

// devConfig builds the dev-mode configuration: an optional config file,
// forced onto localhost:<port> in host mode with dev defaults; relative
// local storage paths are resolved against cwd.
func devConfig(path string, port int, cwd string) (*config.Config, error) {
	var raw []byte
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		raw = b
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	cfg.Server.Listen = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	cfg.Server.BaseDomain = "localhost"
	cfg.Server.PublicURL = ""
	cfg.Server.URLMode = "host"
	cfg.ApplyDevDefaults()
	cfg.ApplyEnvOverrides()
	cfg.Server.PublicURL = "" // dev URLs are always local
	if cfg.Storage.Type == "fs" && cfg.Storage.FS.Root != "" && !filepath.IsAbs(cfg.Storage.FS.Root) {
		cfg.Storage.FS.Root = filepath.Join(cwd, cfg.Storage.FS.Root)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// openWhenReady waits for the dev server's health endpoint, then opens url.
func (a *cliApp) openWhenReady(ctx context.Context, port int, url string) {
	health := fmt.Sprintf("http://127.0.0.1:%d/__backyard/healthz", port)
	hc := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, health, nil)
		if err != nil {
			return
		}
		if resp, err := hc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				a.openURL(ctx, url)
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
