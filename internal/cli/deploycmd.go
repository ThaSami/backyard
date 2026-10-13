package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/sites"
)

type deployFlags struct {
	site        string
	method      string
	message     string
	dryRun      bool
	yes         bool
	open        bool
	concurrency int
}

func (a *cliApp) newDeployCommand() *cobra.Command {
	var fl deployFlags
	cmd := &cobra.Command{
		Use:   "deploy [dir]",
		Short: "Publish a folder as a site",
		Long: `Publish a folder as a site. Deploys are diffed (unchanged files are not uploaded) and
overwrite the site in place, like rsync: files missing locally are deleted.

The site name comes from --site, .backyard.json or the folder name. Methods:
  api    upload through the Backyard deploy gateway (default when logged in)
  s3     aws s3 sync straight into the server's bucket
  gcs    gcloud storage rsync (or gsutil rsync)
  azure  az storage blob sync
  rsync  rsync to the server's deploy.rsync_target
  auto   api if the gateway is reachable and you are logged in (or anonymous deploys
         are allowed), else the first direct method the server advertises whose tool
         is installed

Files ignored by default: .git/, node_modules/, .claude/, .cursor/, .github/, CLAUDE.md,
AGENTS.md, .backyard.json, dotfiles (except .well-known/) plus .backyardignore lines.`,
		Example: `  backyard deploy
  backyard deploy dist --site lunch-vote
  backyard deploy --dry-run
  backyard deploy --method s3 -y`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var dir string
			if len(args) == 1 {
				dir = args[0]
			}
			return a.runDeploy(cmd.Context(), dir, fl)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&fl.site, "site", "s", "", "site name (default: .backyard.json site or the folder name)")
	f.StringVarP(&fl.method, "method", "m", "", "deploy method: auto|api|s3|gcs|azure|rsync (env BACKYARD_DEPLOY_METHOD)")
	f.StringVar(&fl.message, "message", "", "deploy message (api method)")
	f.BoolVarP(&fl.dryRun, "dry-run", "n", false, "show what would be deployed without changing anything")
	f.BoolVarP(&fl.yes, "yes", "y", false, "do not ask before overwriting a site last deployed by someone else")
	f.BoolVar(&fl.open, "open", false, "open the site in the browser afterwards")
	f.IntVar(&fl.concurrency, "concurrency", deploy.DefaultConcurrency, "parallel uploads (api method)")
	return cmd
}

// deployPlan is everything resolved before a deploy starts.
type deployPlan struct {
	dir      string
	site     string
	ignore   []string
	session  *clientconfig.Session
	client   *deploy.Client
	info     *deploy.Info // fresh, or cached when the gateway is unreachable
	infoErr  error        // why the gateway could not be reached
	method   string
	tool     string // executable for direct methods
	patterns []string
	// offline marks a dry run without a reachable gateway: only the local
	// files are listed.
	offline bool
}

// deployOutput is the --json result of a deploy.
type deployOutput struct {
	Site      string             `json:"site"`
	URL       string             `json:"url,omitempty"`
	Method    string             `json:"method"`
	DryRun    bool               `json:"dry_run,omitempty"`
	Uploaded  int                `json:"uploaded"`
	Unchanged int                `json:"unchanged"`
	Deleted   int                `json:"deleted"`
	Bytes     int64              `json:"bytes"`
	Files     []deployOutputFile `json:"files,omitempty"`
	Command   []string           `json:"command,omitempty"`
	Meta      *sites.Meta        `json:"meta,omitempty"`
}

type deployOutputFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (a *cliApp) runDeploy(ctx context.Context, dirArg string, fl deployFlags) error {
	p, err := a.planDeploy(ctx, dirArg, fl)
	if err != nil {
		return err
	}
	if !a.g.json {
		a.errf("Deploying %s to site %q via %s\n", p.dir, p.site, p.method)
	}
	if !fl.dryRun && p.infoErr == nil && p.session.Token != "" {
		if err := a.checkOverwrite(ctx, p, fl.yes); err != nil {
			return err
		}
	}
	var out *deployOutput
	if p.method == clientconfig.MethodAPI {
		out, err = a.deployAPI(ctx, p, fl)
	} else {
		out, err = a.deployDirect(ctx, p, fl)
	}
	if err != nil {
		return explain(err, p.session)
	}
	if a.g.json {
		if err := a.printJSON(out); err != nil {
			return err
		}
	} else {
		a.printDeployResult(out)
	}
	if fl.open && !fl.dryRun && out.URL != "" {
		a.openURL(ctx, out.URL)
	}
	return nil
}

// planDeploy resolves folder, site, credentials and method.
func (a *cliApp) planDeploy(ctx context.Context, dirArg string, fl deployFlags) (*deployPlan, error) {
	cwd, err := a.cwd()
	if err != nil {
		return nil, err
	}
	pf, err := clientconfig.FindProject(absFrom(cwd, dirArg))
	if err != nil {
		return nil, err
	}
	p := &deployPlan{dir: absFrom(cwd, dirArg)}
	if dirArg == "" && pf != nil {
		p.dir = pf.DeployDir()
	}
	if st, err := os.Stat(p.dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", p.dir)
	}
	p.site = strings.TrimSpace(fl.site)
	if p.site == "" && pf != nil {
		p.site = pf.Site
	}
	if p.site == "" {
		base := p.dir
		if pf != nil {
			base = pf.Root()
		}
		p.site = defaultSiteName(base)
	}
	if err := sites.ValidateName(p.site, nil); err != nil {
		return nil, fmt.Errorf("%w; choose a name with --site", err)
	}
	if pf != nil {
		p.ignore = pf.Ignore
	}

	o := a.overrides()
	o.Method = fl.method
	if o.Method == "" && os.Getenv(clientconfig.EnvDeployMethod) == "" && pf != nil {
		o.Method = pf.Method
	}
	s, err := a.session(ctx, o)
	if err != nil {
		return nil, err
	}
	p.session = s
	if s.Gateway != "" {
		p.client = a.client(s.Gateway, s.Token)
		p.info, p.infoErr = a.fetchInfo(ctx, p.client)
		if p.infoErr == nil {
			s.CacheInfo(p.info)
		} else {
			a.log.Debug("gateway unreachable", "gateway", s.Gateway, "error", p.infoErr)
			p.info = s.Profile.Info
		}
	} else {
		p.infoErr = clientconfig.ErrNoServer
	}
	if p.patterns, err = deploy.IgnorePatterns(p.dir, p.ignore); err != nil {
		return nil, err
	}
	if err := a.chooseMethod(p, s.Method); err != nil {
		// A dry run only needs the local files: list them even when no
		// server is configured or reachable (e.g. to check ignore rules).
		if !fl.dryRun || p.infoErr == nil || (s.Method != clientconfig.MethodAuto && s.Method != clientconfig.MethodAPI) {
			return nil, err
		}
		p.method, p.offline = clientconfig.MethodAPI, true
	}
	return p, nil
}

// chooseMethod applies the method selection rules of SPEC §13.
func (a *cliApp) chooseMethod(p *deployPlan, want string) error {
	s := p.session
	switch want {
	case clientconfig.MethodAPI:
		if p.infoErr != nil {
			return fmt.Errorf("the deploy gateway is not reachable: %w", explain(p.infoErr, s))
		}
		p.method = want
		return nil
	case clientconfig.MethodS3, clientconfig.MethodGCS, clientconfig.MethodAzure, clientconfig.MethodRsync:
		if why := directReady(want, p.info); why != "" {
			return fmt.Errorf("cannot deploy with %s: %s", want, why)
		}
		tool, ok := a.findTool(want)
		if !ok {
			return fmt.Errorf("cannot deploy with %s: %s not found in PATH (%s)", want,
				strings.Join(directTools[want], " or "), toolInstallHints[want])
		}
		p.method, p.tool = want, tool
		return nil
	}
	// auto
	var reasons []string
	if p.infoErr != nil {
		reasons = append(reasons, "api: gateway unreachable ("+p.infoErr.Error()+")")
	} else {
		apiAdvertised := len(p.info.Methods) == 0 || slices.Contains(p.info.Methods, clientconfig.MethodAPI)
		switch {
		case !apiAdvertised:
			reasons = append(reasons, "api: not offered by this server")
		case s.Token != "" || p.info.Auth.AnonymousDeploy:
			p.method = clientconfig.MethodAPI
			return nil
		default:
			reasons = append(reasons, "api: not logged in (run `backyard login`)")
		}
	}
	if p.info != nil {
		for _, m := range p.info.Methods {
			if m == clientconfig.MethodAPI || directTools[m] == nil {
				continue
			}
			if why := directReady(m, p.info); why != "" {
				reasons = append(reasons, m+": "+why)
				continue
			}
			tool, ok := a.findTool(m)
			if !ok {
				reasons = append(reasons, m+": "+strings.Join(directTools[m], "/")+" not installed")
				continue
			}
			p.method, p.tool = m, tool
			return nil
		}
	}
	if s.Gateway == "" {
		return clientconfig.ErrNoServer
	}
	return fmt.Errorf("no usable deploy method:\n  - %s", strings.Join(reasons, "\n  - "))
}

// findTool returns the first installed executable for a direct method.
func (a *cliApp) findTool(method string) (string, bool) {
	for _, t := range directTools[method] {
		if _, err := a.deps.lookPath(t); err == nil {
			return t, true
		}
	}
	return "", false
}

// checkOverwrite warns (and asks, on a terminal) before overwriting a site
// last deployed by someone else.
func (a *cliApp) checkOverwrite(ctx context.Context, p *deployPlan, yes bool) error {
	cctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	meta, err := p.client.GetSite(cctx, p.site)
	var ae *deploy.APIError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return nil // new site
	case err != nil:
		a.log.Debug("site lookup failed", "site", p.site, "error", err)
		return nil // the deploy itself will report real problems
	}
	if meta.UpdatedBy == "" {
		return nil
	}
	me := p.session.Profile.User
	if p.session.TokenSource != clientconfig.SourceProfile || me == nil {
		if u, err := p.client.Me(cctx); err == nil {
			me = &clientconfig.User{ID: u.ID, Email: u.Email, Name: u.Name}
		}
	}
	if me.Is(meta.UpdatedBy) {
		return nil
	}
	a.warnf("site %q was last deployed by %s (%s); this deploy replaces its files.",
		p.site, meta.UpdatedBy, ago(a.deps.now(), meta.UpdatedAt))
	if yes || !a.deps.stdinTTY() {
		return nil
	}
	ok, err := a.confirm(ctx, "Overwrite it?")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("deploy cancelled")
	}
	return nil
}

// deployAPI deploys through the gateway protocol with a progress line.
func (a *cliApp) deployAPI(ctx context.Context, p *deployPlan, fl deployFlags) (*deployOutput, error) {
	if p.offline {
		return a.localDryRun(p)
	}
	prog := newProgressPrinter(a, !a.g.json)
	res, err := p.client.Deploy(ctx, p.site, p.dir, deploy.DeployOptions{
		Ignore:      p.ignore,
		Message:     fl.message,
		Concurrency: fl.concurrency,
		DryRun:      fl.dryRun,
		Progress:    prog.update,
	})
	prog.finish()
	if err != nil {
		return nil, err
	}
	out := &deployOutput{
		Site: p.site, URL: res.URL, Method: p.method, DryRun: fl.dryRun,
		Uploaded: res.Uploaded, Unchanged: res.Unchanged, Deleted: res.Deleted, Bytes: res.Bytes,
	}
	if out.URL == "" {
		out.URL = clientconfig.SiteURL(p.info, p.session.Server, p.site)
	}
	if fl.dryRun {
		out.Files = outputFiles(res.Files)
	} else if res.Site.Name != "" {
		meta := res.Site
		out.Meta = &meta
	}
	return out, nil
}

// deployDirect runs the storage tool, then asks the gateway to refresh the
// site's metadata (best effort).
func (a *cliApp) deployDirect(ctx context.Context, p *deployPlan, fl deployFlags) (*deployOutput, error) {
	var st = p.info.Storage
	cmd, err := buildSyncCommand(syncTarget{
		Method: p.method, Dir: p.dir, Site: p.site, Storage: st, RsyncTarget: p.info.RsyncTarget, Patterns: p.patterns,
	}, p.tool)
	if err != nil {
		return nil, err
	}
	out := &deployOutput{Site: p.site, Method: p.method, DryRun: fl.dryRun, Command: cmd.Argv(),
		URL: clientconfig.SiteURL(p.info, p.session.Server, p.site)}
	if fl.dryRun {
		files, err := deploy.CollectFiles(p.dir, p.ignore)
		if err != nil {
			return nil, err
		}
		out.Files = outputFiles(files)
		for _, f := range files {
			out.Bytes += f.Size
		}
		out.Uploaded = len(files)
		return out, nil
	}
	if !a.g.json {
		a.errf("$ %s\n", cmd.String())
	}
	cmd.Stdout, cmd.Stderr = a.streams.Out, a.streams.ErrOut
	if a.g.json {
		cmd.Stdout = a.streams.ErrOut // keep stdout machine-readable
	}
	if err := a.deps.runner.Run(ctx, cmd); err != nil {
		return nil, fmt.Errorf("%s sync failed: %w", p.method, err)
	}
	if p.infoErr == nil && (p.session.Token != "" || p.info.Auth.AnonymousDeploy) {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		meta, err := p.client.Refresh(rctx, p.site)
		cancel()
		if err != nil {
			a.warnf("files were synced but refreshing the site metadata failed: %v", err)
		} else {
			out.Meta = meta
			out.Bytes = meta.Bytes
			if meta.URL != "" {
				out.URL = meta.URL
			}
		}
	} else if !a.g.json {
		a.errf("Note: the hub will show updated site details after the next refresh (gateway not reachable).\n")
	}
	return out, nil
}

// localDryRun lists what a deploy would upload when the gateway cannot be
// asked which files changed.
func (a *cliApp) localDryRun(p *deployPlan) (*deployOutput, error) {
	files, err := deploy.CollectFiles(p.dir, p.ignore)
	if err != nil {
		return nil, err
	}
	if !a.g.json {
		a.errf("Note: the deploy gateway is not reachable (%v); listing the local files only.\n", p.infoErr)
	}
	out := &deployOutput{Site: p.site, Method: p.method, DryRun: true, Uploaded: len(files), Files: outputFiles(files),
		URL: clientconfig.SiteURL(p.info, p.session.Server, p.site)}
	for _, f := range files {
		out.Bytes += f.Size
	}
	return out, nil
}

func outputFiles(files []deploy.LocalFile) []deployOutputFile {
	out := make([]deployOutputFile, len(files))
	for i, f := range files {
		out[i] = deployOutputFile{Path: f.Path, Size: f.Size}
	}
	return out
}

func (a *cliApp) printDeployResult(out *deployOutput) {
	if out.DryRun {
		a.outf("Dry run: nothing was changed.\n")
		for _, f := range out.Files {
			a.outf("  %s (%s)\n", f.Path, humanBytes(f.Size))
		}
		if out.Method == clientconfig.MethodAPI {
			a.outf("%d files: %d to upload (%s), %d unchanged, %d to delete\n",
				len(out.Files), out.Uploaded, humanBytes(out.Bytes), out.Unchanged, out.Deleted)
		} else {
			a.outf("%d files (%s) would be synced with:\n  %s\n", len(out.Files), humanBytes(out.Bytes),
				Command{Name: out.Command[0], Args: out.Command[1:]}.String())
		}
		if out.URL != "" {
			a.outf("Site URL: %s\n", out.URL)
		}
		return
	}
	if out.Method == clientconfig.MethodAPI {
		a.errf("Uploaded %d files (%s), %d unchanged, %d deleted.\n", out.Uploaded, humanBytes(out.Bytes), out.Unchanged, out.Deleted)
	}
	a.errf("Deployed %s\n", out.Site)
	a.outf("%s\n", out.URL)
}

// progressPrinter renders deploy progress on stderr: a live, rewritten line
// on terminals, phase lines otherwise.
type progressPrinter struct {
	a       *cliApp
	enabled bool
	tty     bool
	last    string
	phase   string
	dirty   bool
	lastAt  time.Time
}

func newProgressPrinter(a *cliApp, enabled bool) *progressPrinter {
	return &progressPrinter{a: a, enabled: enabled, tty: enabled && a.deps.stderrTTY()}
}

func (p *progressPrinter) update(e deploy.ProgressEvent) {
	if !p.enabled {
		return
	}
	var line string
	switch e.Phase {
	case "scan":
		if e.Total == 0 {
			line = "Scanning files…"
		} else {
			line = fmt.Sprintf("Found %d files (%s)", e.Total, humanBytes(e.Bytes))
		}
	case "plan":
		line = fmt.Sprintf("%d to upload, %d unchanged", e.Total, e.Done)
	case "upload":
		line = fmt.Sprintf("Uploading %d/%d (%s) %s", e.Done, e.Total, humanBytes(e.Bytes), truncate(e.Path, 40))
	case "commit":
		line = "Committing…"
	case "done":
		line = ""
	default:
		return
	}
	if p.tty {
		now := time.Now()
		if e.Phase == "upload" && e.Done < e.Total && now.Sub(p.lastAt) < 50*time.Millisecond {
			return // throttle redraws
		}
		p.lastAt = now
		p.a.errf("\r\033[K%s", line)
		p.dirty = line != ""
		return
	}
	// Non-terminal: one line per phase change (and the final upload count).
	if e.Phase == p.phase && !(e.Phase == "upload" && e.Done == e.Total) {
		return
	}
	p.phase = e.Phase
	if line != "" && line != p.last {
		p.a.errf("%s\n", line)
		p.last = line
	}
}

func (p *progressPrinter) finish() {
	if p.tty && p.dirty {
		p.a.errf("\r\033[K")
		p.dirty = false
	}
}
