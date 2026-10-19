package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/cli/scaffold"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/sites"
)

func (a *cliApp) newSitesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sites",
		Short: "List, inspect and delete sites",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.newSitesListCommand(), a.newSitesInfoCommand(), a.newSitesDeleteCommand())
	return cmd
}

func (a *cliApp) newSitesListCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "list [filter]",
		Aliases: []string{"ls"},
		Short:   "List sites",
		Long: "List the sites you can see (optionally only those whose name, title or description contains filter). " +
			"That is every site, unless the server lists each site only to its owners and admins; " +
			"`backyard sites info <name>` finds any site by name.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), apiTimeout)
			defer cancel()
			s, c, err := a.gatewaySession(ctx)
			if err != nil {
				return err
			}
			list, err := c.ListSites(ctx)
			if err != nil {
				return explain(err, s)
			}
			if len(args) == 1 {
				list = filterSites(list, args[0])
			}
			slices.SortFunc(list, func(x, y sites.Meta) int { return strings.Compare(x.Name, y.Name) })
			if a.g.json {
				if list == nil {
					list = []sites.Meta{}
				}
				return a.printJSON(map[string]any{"sites": list})
			}
			if len(list) == 0 {
				// The server may list only the caller's own sites (SPEC §7.2).
				if inf, err := a.fetchInfo(ctx, c); err == nil && inf.SiteListing == sites.ListingOwners {
					a.errf("You don't own any sites yet: this server lists each site only to its owners and admins " +
						"(`backyard sites info <name>` finds any site). Create one with `backyard init` and publish it with `backyard deploy`.\n")
					return nil
				}
				a.errf("No sites yet. Create one with `backyard init` and publish it with `backyard deploy`.\n")
				return nil
			}
			tw := tabwriter.NewWriter(a.streams.Out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTITLE\tUPDATED\tBY\tFILES\tSIZE\tURL")
			now := a.deps.now()
			for _, m := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n", m.Name, orDash(truncate(m.Title, 40)),
					ago(now, m.UpdatedAt), orDash(m.UpdatedBy), m.FileCount, humanBytes(m.Bytes), m.URL)
			}
			return tw.Flush()
		},
	}
}

// filterSites returns the sites whose name, title or description contains
// query (case-insensitive).
func filterSites(list []sites.Meta, query string) []sites.Meta {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return list
	}
	var out []sites.Meta
	for _, m := range list {
		if strings.Contains(strings.ToLower(m.Name), q) || strings.Contains(strings.ToLower(m.Title), q) ||
			strings.Contains(strings.ToLower(m.Description), q) {
			out = append(out, m)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *cliApp) newSitesInfoCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "info <site>",
		Short: "Show a site's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), apiTimeout)
			defer cancel()
			s, c, err := a.gatewaySession(ctx)
			if err != nil {
				return err
			}
			m, err := c.GetSite(ctx, args[0])
			if err != nil {
				return explain(err, s)
			}
			if a.g.json {
				return a.printJSON(m)
			}
			tw := tabwriter.NewWriter(a.streams.Out, 0, 4, 2, ' ', 0)
			row := func(k, v string) {
				if v != "" {
					fmt.Fprintf(tw, "%s:\t%s\n", k, v)
				}
			}
			row("Name", m.Name)
			row("URL", m.URL)
			row("Title", m.Title)
			row("Description", m.Description)
			row("Files", fmt.Sprintf("%d (%s)", m.FileCount, humanBytes(m.Bytes)))
			if !m.CreatedAt.IsZero() {
				row("Created", m.CreatedAt.Local().Format("2006-01-02 15:04")+" by "+orDash(m.CreatedBy))
			}
			if !m.UpdatedAt.IsZero() {
				row("Updated", m.UpdatedAt.Local().Format("2006-01-02 15:04")+" by "+orDash(m.UpdatedBy))
			}
			if len(m.Owners) > 0 {
				row("Owners", strings.Join(m.Owners, ", "))
			}
			return tw.Flush()
		},
	}
}

func (a *cliApp) newSitesDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete <site>",
		Aliases: []string{"rm"},
		Short:   "Delete a site and its files",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			site := args[0]
			s, c, err := a.gatewaySession(ctx)
			if err != nil {
				return err
			}
			if !yes {
				ok, err := a.confirm(ctx, fmt.Sprintf("Delete site %q and all of its files? This cannot be undone.", site))
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("cancelled")
				}
			}
			dctx, cancel := context.WithTimeout(ctx, apiTimeout)
			defer cancel()
			if err := c.DeleteSite(dctx, site); err != nil {
				return explain(err, s)
			}
			if a.g.json {
				return a.printJSON(map[string]any{"deleted": site})
			}
			a.outf("Deleted site %s.\n", site)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func (a *cliApp) newOpenCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "open [site]",
		Short: "Open a site (or the hub) in the browser",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			s, err := a.session(ctx, a.overrides())
			if err != nil {
				return err
			}
			if err := s.RequireGateway(); err != nil {
				return err
			}
			info := s.CachedInfo()
			if info == nil {
				if fresh, err := a.fetchInfo(ctx, a.client(s.Gateway, s.Token)); err == nil {
					info = fresh
				}
			}
			var u string
			if len(args) == 1 {
				if err := sites.ValidateName(args[0], nil); err != nil {
					return err
				}
				u = clientconfig.SiteURL(info, s.Server, args[0])
			} else {
				u = clientconfig.HubURL(info, s.Server)
			}
			if u == "" {
				return errors.New("the server URL is unknown; run `backyard login --server <url>`")
			}
			if a.g.json {
				return a.printJSON(map[string]string{"url": u})
			}
			a.outf("%s\n", u)
			a.openURL(ctx, u)
			return nil
		},
	}
}

func (a *cliApp) newPullCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "pull <site> [dir]",
		Short: "Download a site's files",
		Long: `Download the deployed files of a site into dir (default ./<site>) — handy to fork or
remix another site. Local files that differ from the site are not overwritten unless --force.

Files a deploy never publishes are skipped (and listed), even if the site holds them: dotfiles
and dot-directories (.git/, .claude/, .env…; .well-known/ is kept), node_modules/,
.backyard.json, .backyardignore, CLAUDE.md and AGENTS.md. A site cannot plant repository
metadata, project settings or agent instructions in your folder.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			site := args[0]
			if err := sites.ValidateName(site, nil); err != nil {
				return err
			}
			cwd, err := a.cwd()
			if err != nil {
				return err
			}
			dir := filepath.Join(cwd, site)
			if len(args) == 2 {
				dir = absFrom(cwd, args[1])
			}
			return a.runPull(cmd.Context(), site, dir, force)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite local files that differ")
	return cmd
}

// pullResult is the --json output of pull.
type pullResult struct {
	Site       string   `json:"site"`
	Dir        string   `json:"dir"`
	Downloaded []string `json:"downloaded"`
	Unchanged  []string `json:"unchanged"`
	// Skipped lists site files pull never writes (see pullSkips).
	Skipped []string `json:"skipped"`
}

// pullIgnore matches, case-insensitively, what deploys never publish
// (deploy.DefaultIgnore): VCS metadata, the project and ignore files, agent
// instructions and settings, node_modules and every other dotfile except
// .well-known/.
var pullIgnore = sync.OnceValue(func() *deploy.Matcher {
	patterns := deploy.DefaultIgnore()
	for i, p := range patterns {
		patterns[i] = strings.ToLower(p)
	}
	return deploy.NewMatcher(patterns)
})

// windowsShortName matches an 8.3 short-name alias ("GIT~1", "CLAUDE~1.MD"),
// which Windows resolves to an existing long name.
var windowsShortName = regexp.MustCompile(`~[0-9]`)

// pullSkips reports whether pull must not write the site file p: anything a
// deploy never publishes. A site is written by whoever deployed it, possibly
// through the raw gateway API, so without this a pulled folder could carry a
// crafted .git/config (git runs commands named there, e.g. core.fsmonitor,
// on the next git command or git-aware shell prompt), a .backyard.json
// redirecting later deploys, or instructions and settings for coding
// agents. Segments are compared the way case-insensitive and Windows file
// systems resolve them ("CLAUDE.md." and "claude.md" are CLAUDE.md there).
func pullSkips(p string) bool {
	segs := strings.Split(p, "/")
	for i, seg := range segs {
		if j := strings.IndexByte(seg, ':'); j >= 0 {
			seg = seg[:j] // NTFS alternate data streams ("AGENTS.md::$DATA")
		}
		seg = strings.TrimRight(seg, ". ") // Windows drops trailing dots and spaces
		if seg == "" || (runtime.GOOS == "windows" && windowsShortName.MatchString(seg)) {
			return true
		}
		segs[i] = strings.ToLower(seg)
	}
	return pullIgnore().Ignored(strings.Join(segs, "/"), false)
}

// pullMaxFile caps a single pulled file.
const pullMaxFile = 1 << 30

// pullStreamAbove is the size above which pulled files are streamed to disk
// rather than read into memory (a variable so tests can lower it).
var pullStreamAbove int64 = deploy.DefaultReadFileMax

func (a *cliApp) runPull(ctx context.Context, site, dir string, force bool) error {
	s, c, err := a.gatewaySession(ctx)
	if err != nil {
		return err
	}
	lctx, cancel := context.WithTimeout(ctx, apiTimeout)
	files, err := c.SiteFiles(lctx, site)
	cancel()
	if err != nil {
		return explain(err, s)
	}
	type job struct {
		file   deploy.SiteFile
		target string
	}
	var (
		jobs      []job
		unchanged []string
		conflicts []string
		skipped   []string
	)
	for _, f := range files {
		// The syntax only: a site may still hold paths that today's gateway
		// rules reject (a .git/ written before them, or by a direct sync),
		// and those are skipped below like every path deploys never publish.
		if why := deploy.ValidatePathSyntax(f.Path); why != "" {
			return fmt.Errorf("refusing to pull %q: %s", f.Path, why)
		}
		if pullSkips(f.Path) {
			skipped = append(skipped, f.Path)
			continue
		}
		target, err := scaffold.SafeJoin(dir, f.Path)
		if err != nil {
			return fmt.Errorf("refusing to pull %q: %w", f.Path, err)
		}
		if st, err := os.Stat(target); err == nil {
			if st.Size() == f.Size && f.SHA256 != "" {
				if sum, err := fileSHA256(target); err == nil && strings.EqualFold(sum, f.SHA256) {
					unchanged = append(unchanged, f.Path)
					continue
				}
			}
			if !force {
				conflicts = append(conflicts, f.Path)
				continue
			}
		}
		jobs = append(jobs, job{file: f, target: target})
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%d local files differ from the site and were not overwritten (use --force):\n  %s",
			len(conflicts), strings.Join(conflicts, "\n  "))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	var (
		mu         sync.Mutex
		firstErr   error
		downloaded []string
		wg         sync.WaitGroup
	)
	ctx, cancelAll := context.WithCancel(ctx)
	defer cancelAll()
	fallback := sync.OnceValue(func() *deploy.Client { return a.embeddedGatewayFallback(ctx, s, c) })
	work := make(chan job)
	for range min(4, max(1, len(jobs))) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				err := a.pullOne(ctx, c, site, dir, j.file, j.target)
				if responseTooLarge(err) {
					err = a.pullThroughEmbedded(ctx, fallback(), site, dir, j.file, j.target, err)
				}
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
					cancelAll()
				} else if err == nil {
					downloaded = append(downloaded, j.file.Path)
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, j := range jobs {
		select {
		case work <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return explain(firstErr, s)
	}
	slices.Sort(downloaded)
	slices.Sort(skipped)
	if a.g.json {
		if downloaded == nil {
			downloaded = []string{}
		}
		if unchanged == nil {
			unchanged = []string{}
		}
		if skipped == nil {
			skipped = []string{}
		}
		return a.printJSON(pullResult{Site: site, Dir: dir, Downloaded: downloaded, Unchanged: unchanged, Skipped: skipped})
	}
	a.outf("Pulled %s into %s: %d files downloaded, %d unchanged.\n", site, dir, len(downloaded), len(unchanged))
	if len(skipped) > 0 {
		const maxListed = 20
		list := skipped[:min(len(skipped), maxListed)]
		more := ""
		if len(skipped) > maxListed {
			more = fmt.Sprintf("\n  … and %d more", len(skipped)-maxListed)
		}
		a.errf("Skipped %d files that deploys never publish (dotfiles, .git/, node_modules/, .backyard.json, CLAUDE.md, AGENTS.md…):\n  %s%s\n",
			len(skipped), strings.Join(list, "\n  "), more)
	}
	return nil
}

// responseTooLarge reports whether err is a gateway's 413 too_large: on a
// file read, that is a gateway that cannot return the file (an AWS Lambda
// gateway's 6 MB response cap), not a limit of the site.
func responseTooLarge(err error) bool {
	var ae *deploy.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusRequestEntityTooLarge && ae.Code == deploy.CodeTooLarge
}

// embeddedGatewayFallback returns a client for the server's embedded gateway
// (<public URL>/__backyard/api/deploy, which has no response size cap and
// accepts the same tokens) when the session uses another gateway, or nil.
// The public URL is the session's server, else the public_url the
// gateway's info names (the profile's cached copy, or asked once). The
// token never moves from an https gateway to a plain-http one.
func (a *cliApp) embeddedGatewayFallback(ctx context.Context, s *clientconfig.Session, c *deploy.Client) *deploy.Client {
	server := s.Server
	if server == "" {
		info := s.CachedInfo()
		if info == nil {
			info, _ = a.fetchInfo(ctx, c)
		}
		if info != nil {
			server = info.PublicURL
		}
	}
	if server != "" {
		var err error
		if server, err = clientconfig.NormalizeURL(server); err != nil {
			return nil
		}
	}
	emb := clientconfig.GatewayURL(server, "")
	if emb == "" || strings.EqualFold(emb, strings.TrimSuffix(s.Gateway, "/")) {
		return nil
	}
	if hasScheme(s.Gateway, "https") && !hasScheme(emb, "https") {
		return nil
	}
	return a.client(emb, s.Token)
}

// hasScheme reports whether the URL raw uses scheme (case-insensitively).
func hasScheme(raw, scheme string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(u.Scheme, scheme)
}

// pullThroughEmbedded retries a download the gateway answered with 413
// too_large (first) through the server's embedded gateway fb. Without fb it
// returns first, whose message (from the Lambda gateway) already names the
// embedded gateway when the gateway knows the server's public URL; when the
// retry fails too, both errors and a hint naming fb's URL.
func (a *cliApp) pullThroughEmbedded(ctx context.Context, fb *deploy.Client, site, dir string, f deploy.SiteFile, target string, first error) error {
	if fb == nil {
		return first // the gateway's message names the embedded gateway when it knows it
	}
	if err := a.pullOne(ctx, fb, site, dir, f, target); err != nil {
		return fmt.Errorf("%w\n  retrying through the server's embedded gateway %s failed too: %v\n"+
			"  hint: this deploy gateway cannot return files this large (an AWS Lambda gateway returns at most 6 MB); "+
			"pull from where the server is reachable: `backyard pull %s --gateway %s`", first, fb.BaseURL, err, site, fb.BaseURL)
	}
	if !a.g.json {
		a.errf("Note: %s is too large for the deploy gateway; downloaded it through the server's embedded gateway %s.\n", f.Path, fb.BaseURL)
	}
	return nil
}

// pullOne downloads one file, verifies its size and hash and writes it
// atomically. Files above pullStreamAbove are streamed to disk instead of
// being held in memory.
func (a *cliApp) pullOne(ctx context.Context, c *deploy.Client, site, dir string, f deploy.SiteFile, target string) error {
	if f.Size > pullMaxFile {
		return fmt.Errorf("download %s: %s is larger than the %s limit", f.Path, humanBytes(f.Size), humanBytes(pullMaxFile))
	}
	// Re-check right before writing: the tree may have changed meanwhile.
	if _, err := scaffold.SafeJoin(dir, f.Path); err != nil {
		return fmt.Errorf("refusing to write %q: %w", f.Path, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", f.Path, err)
	}
	if f.Size > pullStreamAbove {
		return streamSiteFile(ctx, c, site, f, target)
	}
	b, err := c.ReadFile(ctx, site, f.Path, deploy.DefaultReadFileMax)
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	if err := verifyDownload(f, int64(len(b)), sha256.Sum256(b)); err != nil {
		return err
	}
	if err := scaffold.WriteFileAtomic(target, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	return nil
}

// verifyDownload checks a downloaded file against its listing entry.
func verifyDownload(f deploy.SiteFile, size int64, sum [sha256.Size]byte) error {
	if size != f.Size {
		return fmt.Errorf("download %s: got %d bytes, the site lists %d (did it change during the pull? try again)", f.Path, size, f.Size)
	}
	if f.SHA256 != "" && !strings.EqualFold(hex.EncodeToString(sum[:]), f.SHA256) {
		return fmt.Errorf("download %s: checksum mismatch", f.Path)
	}
	return nil
}

// streamSiteFile downloads GET G/sites/{site}/files/{path} into a temporary
// file next to target, hashing on the fly, and renames it into place.
func streamSiteFile(ctx context.Context, c *deploy.Client, site string, f deploy.SiteFile, target string) error {
	segs := strings.Split(f.Path, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	u := strings.TrimSuffix(c.BaseURL, "/") + "/sites/" + url.PathEscape(site) + "/files/" + strings.Join(segs, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("download %s: %w", f.Path, apiErrorFrom(resp.StatusCode, body))
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".backyard-pull-*.tmp")
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after the rename
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, f.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", f.Path, err)
	}
	var sum [sha256.Size]byte
	copy(sum[:], h.Sum(nil))
	if err := verifyDownload(f, n, sum); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	if err := os.Rename(name, target); err != nil {
		return fmt.Errorf("write %s: %w", f.Path, err)
	}
	return nil
}

// apiErrorFrom builds a *deploy.APIError from a non-2xx response body (the
// standard error envelope, or a summary of whatever else came back).
func apiErrorFrom(status int, body []byte) *deploy.APIError {
	ae := &deploy.APIError{Status: status}
	var eb deploy.ErrorBody
	if json.Unmarshal(body, &eb) == nil && eb.Error.Code != "" {
		ae.Code, ae.Message, ae.LoginURL = eb.Error.Code, eb.Error.Message, eb.Error.LoginURL
		return ae
	}
	ae.Code = "http_error"
	ae.Message = fmt.Sprintf("HTTP %d: %s", status, truncate(strings.Join(strings.Fields(string(body)), " "), 200))
	return ae
}

func fileSHA256(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
