package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/cli/scaffold"
	"github.com/thasami/backyard/internal/clientconfig"
)

func (a *cliApp) newInitCommand() *cobra.Command {
	var (
		dir      string
		agents   []string
		template string
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "init [site]",
		Short: "Create a new site with agent docs",
		Long: `Create a new Backyard site: index.html, .backyard.json, CLAUDE.md, AGENTS.md and the
Backyard skill for coding agents (.claude/skills/backyard). Existing files are never
overwritten unless --force is given.

With a site name and no --dir, the site is created in ./<site>; without a site name the
current folder is initialized and its name becomes the site name.`,
		Example: `  backyard init lunch-vote
  backyard init --agents claude,cursor,copilot
  backyard init dashboard --template app --dir ./web`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := a.cwd()
			if err != nil {
				return err
			}
			site := ""
			if len(args) == 1 {
				site = strings.TrimSpace(args[0])
			}
			target := absFrom(cwd, dir)
			if dir == "" && site != "" {
				target = filepath.Join(cwd, site)
			}
			if site == "" {
				site = defaultSiteName(target)
				if site == "" {
					return fmt.Errorf("cannot derive a site name from %s; pass one: backyard init <site>", target)
				}
			}
			server := a.currentServer()
			res, err := scaffold.Init(scaffold.Options{
				Dir: target, Site: site, Server: server, Agents: agents, Template: template,
				Force: force, Render: a.deps.render,
			})
			if err != nil {
				return err
			}
			if a.g.json {
				return a.printJSON(res)
			}
			a.printInitResult(cwd, res, server)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&dir, "dir", "d", "", "directory to initialize (default ./<site>, or . without a site name)")
	f.StringSliceVar(&agents, "agents", nil, "agent integrations: claude,codex,cursor,copilot (default claude,codex)")
	f.StringVarP(&template, "template", "t", "", "template: blank (default) or app")
	f.BoolVarP(&force, "force", "f", false, "overwrite existing files")
	return cmd
}

// currentServer returns the hub URL of the active profile ("" if none).
func (a *cliApp) currentServer() string {
	path, err := a.configPath()
	if err != nil {
		return ""
	}
	f, err := clientconfig.Load(path)
	if err != nil {
		return ""
	}
	r, err := f.Resolve(a.overrides())
	if err != nil {
		return ""
	}
	return r.Server
}

func (a *cliApp) printInitResult(cwd string, res *scaffold.Result, server string) {
	rel, err := filepath.Rel(cwd, res.Dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = res.Dir
	}
	a.outf("Initialized site %q in %s\n", res.Site, res.Dir)
	for _, p := range res.Created {
		a.outf("  + %s\n", p)
	}
	for _, p := range res.Overwritten {
		a.outf("  ~ %s (overwritten)\n", p)
	}
	for _, p := range res.Skipped {
		a.outf("  = %s (exists, kept)\n", p)
	}
	if len(res.Skipped) > 0 {
		a.outf("Existing files were kept; use --force to overwrite them.\n")
	}
	a.outf("\nNext steps:\n")
	if rel != "." {
		a.outf("  cd %s\n", shellQuote(rel))
	}
	a.outf("  backyard dev       # preview at http://%s.localhost:4321/\n", res.Site)
	if server == "" {
		a.outf("  backyard login --server https://backyard.example.com\n")
	}
	a.outf("  backyard deploy    # publish\n")
}
