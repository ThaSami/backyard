package cli

import (
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/version"
)

func (a *cliApp) newConfigCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Print an example server config or validate one",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "example",
		Short: "Print a fully commented example backyard.yaml",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.outf("%s", config.Example)
			if !strings.HasSuffix(config.Example, "\n") {
				a.outf("\n")
			}
			return nil
		},
	})
	var dev bool
	validate := &cobra.Command{
		Use:   "validate [file]",
		Short: "Check a server config file",
		Long: `Load a server config (file argument, else $BACKYARD_CONFIG_YAML, $BACKYARD_CONFIG or
./backyard.yaml) with defaults and environment expansion applied, and report every problem.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 1 {
				path = args[0]
			}
			cfg, err := config.Load(path, dev)
			if err != nil {
				if a.g.json {
					_ = a.printJSON(map[string]any{"valid": false, "errors": splitErrors(err)})
					return &ExitError{Code: 1, Err: errSilent}
				}
				a.errf("Invalid configuration:\n")
				for _, e := range splitErrors(err) {
					a.errf("  - %s\n", e)
				}
				return &ExitError{Code: 1, Err: errSilent}
			}
			if a.g.json {
				return a.printJSON(map[string]any{
					"valid": true, "public_url": cfg.Server.PublicURL, "url_mode": cfg.Server.URLMode,
					"auth": cfg.Auth.Mode, "storage": cfg.Storage.Type, "database": cfg.Database.Type,
					"pubsub": cfg.PubSub.Type, "deploy_methods": cfg.Deploy.Methods,
				})
			}
			a.outf("Configuration OK\n")
			a.outf("  public URL:  %s (%s mode)\n", cfg.Server.PublicURL, cfg.Server.URLMode)
			a.outf("  auth:        %s\n", cfg.Auth.Mode)
			a.outf("  storage:     %s\n", cfg.Storage.Type)
			a.outf("  database:    %s\n", cfg.Database.Type)
			a.outf("  pubsub:      %s\n", cfg.PubSub.Type)
			a.outf("  deploy:      %s\n", strings.Join(cfg.Deploy.Methods, ", "))
			if cfg.Server.Dev {
				a.outf("  dev mode:    on (not for production)\n")
			}
			return nil
		},
	}
	validate.Flags().BoolVar(&dev, "dev", false, "apply dev defaults before validating")
	cmd.AddCommand(validate)
	return cmd
}

// splitErrors flattens joined errors into messages.
func splitErrors(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		for line := range strings.SplitSeq(e.Error(), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
	}
	walk(err)
	if len(out) == 0 {
		out = append(out, err.Error())
	}
	return out
}

func (a *cliApp) newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.g.json {
				return a.printJSON(map[string]string{
					"version": version.Version, "commit": version.Commit, "date": version.Date,
					"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
				})
			}
			a.outf("backyard %s %s/%s %s\n", version.String(), runtime.GOOS, runtime.GOARCH, runtime.Version())
			return nil
		},
	}
}
