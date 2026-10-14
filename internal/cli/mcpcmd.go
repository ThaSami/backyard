package cli

import (
	"io"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/mcpserver"
)

func (a *cliApp) newMCPCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Run the MCP server for coding agents (stdio)",
		Long: `Run a Model Context Protocol server over stdin/stdout so coding agents (Claude Code,
Cursor, Codex…) can read the Backyard docs, scaffold and deploy sites, and browse other sites'
code, using this CLI's login. Logs go to stderr.

  claude mcp add backyard -- backyard mcp`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := a.configPath()
			if err != nil {
				return err
			}
			level := slog.LevelWarn
			if a.g.verbose {
				level = slog.LevelDebug
			}
			o := mcpserver.Options{
				ConfigPath: path,
				Overrides:  a.overrides(),
				HTTP:       a.deps.httpClient,
				Render:     a.deps.render,
				Getwd:      a.deps.getwd,
				Now:        a.deps.now,
				Logger:     slog.New(slog.NewTextHandler(a.streams.ErrOut, &slog.HandlerOptions{Level: level})),
			}
			if a.streams.In != os.Stdin || a.streams.Out != os.Stdout {
				o.Transport = &mcp.IOTransport{Reader: io.NopCloser(a.streams.In), Writer: nopWriteCloser{a.streams.Out}}
			}
			return mcpserver.Run(cmd.Context(), o)
		},
	}
}

// nopWriteCloser adds a no-op Close to a writer the command does not own.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
