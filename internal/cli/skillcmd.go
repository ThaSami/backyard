package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/cli/scaffold"
)

func (a *cliApp) newSkillCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage the Backyard skill for Claude",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newSkillInstallCommand())
	return cmd
}

func (a *cliApp) newSkillInstallCommand() *cobra.Command {
	var global, project, yes bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the Backyard skill for Claude Code",
		Long: `Install the Backyard skill (API reference, recipes and deploy instructions for coding
agents) into ~/.claude/skills/backyard (--global, the default) or
./.claude/skills/backyard (--project). An existing copy is replaced after confirmation.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if global && project {
				return errors.New("choose either --global or --project")
			}
			var base string
			if project {
				cwd, err := a.cwd()
				if err != nil {
					return err
				}
				base = cwd
			} else {
				home, err := a.deps.homeDir()
				if err != nil {
					return fmt.Errorf("locate home directory: %w", err)
				}
				base = home
			}
			target := filepath.Join(base, ".claude", "skills", "backyard")
			src, err := a.deps.skillFS()
			if err != nil {
				return fmt.Errorf("load the embedded skill: %w", err)
			}
			files, err := readTree(src)
			if err != nil {
				return fmt.Errorf("load the embedded skill: %w", err)
			}
			if len(files) == 0 {
				return errors.New("the embedded skill is empty")
			}
			if entries, err := os.ReadDir(target); err == nil && len(entries) > 0 {
				if !yes {
					ok, err := a.confirm(cmd.Context(), fmt.Sprintf("%s already exists. Replace it?", target))
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("cancelled")
					}
				}
				if err := os.RemoveAll(target); err != nil {
					return fmt.Errorf("remove the old skill: %w", err)
				}
			}
			res, err := scaffold.Write(target, files, true)
			if err != nil {
				return err
			}
			if a.g.json {
				return a.printJSON(map[string]any{"dir": target, "files": res.Created})
			}
			a.outf("Installed the Backyard skill (%d files) to %s\n", len(res.Created)+len(res.Overwritten), target)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&global, "global", false, "install into ~/.claude/skills (default)")
	f.BoolVar(&project, "project", false, "install into ./.claude/skills")
	f.BoolVarP(&yes, "yes", "y", false, "replace an existing installation without asking")
	return cmd
}

// readTree reads every regular file of fsys keyed by its slash path.
func readTree(fsys fs.FS) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[p] = b
		return nil
	})
	return files, err
}
