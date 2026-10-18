package deploy

// Publishing from memory for the remote MCP connector (SPEC §14.1,
// backyard_publish): the files arrive inside a tool call instead of through
// the create/upload/commit protocol of §7, so the gateway writes them to
// storage itself.
//
// A publish merges by default: the listed files are written, Delete removes
// paths, and the site's other files are kept. Replace makes the site exactly
// Files. Like a commit it is not atomic against concurrent CLI or hub
// deploys of the site (rsync semantics, SPEC §7.3); the connector serializes
// its own publishes of a site across replicas with a lease (internal/mcpremote).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

// PublishFile is one file of a publish call, already decoded.
type PublishFile struct {
	Path        string
	Data        []byte
	ContentType string
}

// PublishRequest is one backyard_publish call.
type PublishRequest struct {
	User      *identity.User
	Site      string
	Files     []PublishFile
	Delete    []string
	Replace   bool
	Overwrite bool
	// ClientIP, ClientName and GrantID go into the deploy.commit audit record.
	ClientIP, ClientName, GrantID string
}

// PublishResult describes the site after a publish.
type PublishResult struct {
	Site, URL                           string
	Created                             bool
	Files, Uploaded, Unchanged, Deleted int
	Bytes                               int64
	Warnings                            []string
}

// PublishError is a refusal or failure with its catalog id ("E2" … "E27",
// the tool errors of SPEC §14.1) and the tool error text.
type PublishError struct {
	ID, Message string
	RetryAfter  time.Duration
}

func (e *PublishError) Error() string { return e.Message }

const (
	// MaxPublishList bounds the files and the delete paths of one publish.
	MaxPublishList = 1000
	// putParallelism bounds concurrent object writes of one publish.
	putParallelism = 8
	// maxListedPaths bounds the paths named in one error message.
	maxListedPaths = 10
)

// errStoreText is the E21 text without a request id; the connector, which
// knows the request, replaces it.
const errStoreText = "Publishing failed: Backyard could not store the files. Publishing the same files again completes it."

func publishErr(id, format string, args ...any) *PublishError {
	return &PublishError{ID: id, Message: fmt.Sprintf(format, args...)}
}

// MaxPublishBytes returns the decoded file bytes one publish may carry
// (mcp.max_publish_bytes).
func MaxPublishBytes(cfg *config.Config) int64 {
	if cfg.MCP.MaxPublishBytes > 0 {
		return cfg.MCP.MaxPublishBytes
	}
	return config.DefaultMCPMaxPublishBytes
}

// FormatBytes renders a byte count for messages ("812 B", "1.2 MiB").
func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// CheckPublish runs the checks of Publish that need no storage, in their
// normative order (SPEC §14.1, backyard_publish steps 1–4), and fills in default
// content types. When decode is not nil it is called for Files[i] once the
// file's path checks passed and sets its Data, so an encoding error (E7) is
// reported in its place in the order; with a nil decode, Data is used as is.
func (g *Gateway) CheckPublish(req *PublishRequest, decode func(i int) ([]byte, *PublishError)) *PublishError {
	u, site := req.User, req.Site
	if u == nil {
		return &PublishError{ID: "E21", Message: errStoreText}
	}
	if u.Anonymous && !g.anonymousDeploy() {
		return publishErr("E17", "This Backyard has no sign-in and does not accept anonymous publishing, so this connection can only read. "+
			"Ask the Backyard admin to set up sign-in or allow anonymous deploys.")
	}
	if sites.ValidateName(site, nil) != nil {
		return publishErr("E2", `"%s" is not a valid site name: use lowercase letters, digits and '-' (at most 63 characters), e.g. "lunch-poll".`,
			truncate(site, 100))
	}
	if slices.ContainsFunc(g.cfg.Sites.Reserved, func(r string) bool { return strings.EqualFold(r, site) }) {
		return publishErr("E3", `"%s" is reserved on this Backyard; choose another name.`, site)
	}
	switch {
	case len(req.Files) == 0 && len(req.Delete) == 0:
		return publishErr("E4", "Nothing to publish: give files, delete, or both.")
	case req.Replace && len(req.Delete) > 0:
		return publishErr("E5", `Use either "replace" or "delete", not both.`)
	case req.Replace && len(req.Files) == 0:
		return publishErr("E6", "A site needs at least one file.")
	case len(req.Files) > MaxPublishList:
		return publishErr("E1", "Invalid arguments: files has %d entries; at most %d are allowed.", len(req.Files), MaxPublishList)
	case len(req.Delete) > MaxPublishList:
		return publishErr("E1", "Invalid arguments: delete has %d entries; at most %d are allowed.", len(req.Delete), MaxPublishList)
	}
	deletes := make(map[string]bool, len(req.Delete))
	for _, p := range req.Delete {
		deletes[p] = true
	}
	seen := make(map[string]bool, len(req.Files))
	var total int64
	for i := range req.Files {
		f := &req.Files[i]
		if perr := pathError(site, f.Path); perr != nil {
			return perr
		}
		if seen[f.Path] {
			return publishErr("E9", `"%s" appears more than once.`, truncate(f.Path, 200))
		}
		seen[f.Path] = true
		if deletes[f.Path] {
			return publishErr("E10", `"%s" is both in files and in delete.`, truncate(f.Path, 200))
		}
		if decode != nil {
			data, perr := decode(i)
			if perr != nil {
				return perr
			}
			f.Data = data
		}
		size := int64(len(f.Data))
		if lim := g.cfg.Sites.MaxFileBytes; lim > 0 && size > lim {
			return publishErr("E12", "%s is %s; files may be at most %s.", truncate(f.Path, 200), FormatBytes(size), FormatBytes(lim))
		}
		if !validContentType(f.ContentType) {
			return publishErr("E1", "Invalid arguments: files[%d].content_type is not a valid content type (at most %d characters, no control characters).",
				i, maxContentTypeLen)
		}
		f.ContentType = strings.TrimSpace(f.ContentType)
		total += size
	}
	if lim := MaxPublishBytes(g.cfg); total > lim {
		return publishErr("E11", "This call carries %s of file content; the limit is %s per call. "+
			"Send the files over several calls (files you do not list are kept).", FormatBytes(total), FormatBytes(lim))
	}
	for _, p := range req.Delete {
		if perr := pathError(site, p); perr != nil {
			return perr
		}
	}
	for i := range req.Files {
		if f := &req.Files[i]; f.ContentType == "" {
			f.ContentType = blob.DetectContentType(f.Path, f.Data[:min(len(f.Data), 512)])
		}
	}
	return nil
}

// pathError is E8 for a path that cannot be published.
func pathError(site, p string) *PublishError {
	why := ValidatePath(p)
	if why == "" && !blob.ValidKey(sites.SiteKey(site, p)) {
		why = "too long for storage"
	}
	if why == "" {
		return nil
	}
	return publishErr("E8", `"%s" is not an allowed path: %s.`, truncate(p, 200), why)
}

// publishEntry is one file of the site after a publish.
type publishEntry struct {
	path string
	size int64
	file int // index in PublishRequest.Files, or -1 for a kept file
}

// Publish writes req to the site: by default the listed files are added or
// replaced and the others kept; Delete removes paths; Replace makes the site
// exactly Files. It runs SPEC §14.1 backyard_publish steps 1–4 and 6–10
// (the caller holds the connector's lease, step 5): CheckPublish, the ownership policy,
// the overwrite guard, the deploys_per_hour quota (keyed user:<id>, guests
// included), the limits of the resulting site, then the writes, the
// manifest and meta switch, and deletions last.
func (g *Gateway) Publish(ctx context.Context, req PublishRequest) (*PublishResult, *PublishError) {
	if perr := g.CheckPublish(&req, nil); perr != nil {
		return nil, perr
	}
	u, site := req.User, req.Site
	unlock := g.lockSite(site)
	defer unlock()

	prev, deny, err := g.checkOwnership(ctx, u, site)
	if err != nil {
		return nil, g.publishFailed(ctx, site, "read the site metadata", err)
	}
	if deny != nil {
		owners := strings.Join(deny.owners, ", ")
		if deny.corrupt {
			owners = "owners who cannot be determined (its metadata is unreadable; an admin must republish it)"
		}
		return nil, publishErr("E15", "%s belongs to %s; you are not an owner. Ask an owner to add you, or publish under another name.", site, owners)
	}
	if prev != nil && !req.Overwrite && foreignPublisher(prev.UpdatedBy, u) {
		when := ""
		if !prev.UpdatedAt.IsZero() {
			when = " on " + prev.UpdatedAt.UTC().Format("2006-01-02 15:04") + " UTC"
		}
		return nil, publishErr("E16", `%s was last published by %s%s. Nothing was changed. Ask the user whether to replace it; `+
			`if so, call again with "overwrite": true, otherwise choose another name.`, site, prev.UpdatedBy, when)
	}
	if ok, wait := g.chargeKey("user:" + u.ID); !ok {
		mins := max(1, int(math.Ceil(wait.Minutes())))
		e := publishErr("E19", "Publishing limit reached (%d per hour). Try again in %d minutes.", g.cfg.RateLimit.DeploysPerHour, mins)
		e.RetryAfter = wait
		return nil, e
	}

	current, err := g.reg.Files(ctx, site)
	if err != nil {
		return nil, g.publishFailed(ctx, site, "list the site's files", err)
	}
	prevMan, err := g.reg.GetManifest(ctx, site)
	if err != nil {
		return nil, g.publishFailed(ctx, site, "read the site manifest", err)
	}
	existed := prev != nil || len(current) > 0
	stored := make(map[string]blob.ObjectInfo, len(current))
	for _, o := range current {
		stored[o.Key] = o
	}
	recorded := make(map[string]sites.ManifestFile, len(prevMan.Files))
	for _, f := range prevMan.Files {
		recorded[f.Path] = f
	}

	// The resulting file set F.
	listed := make(map[string]int, len(req.Files))
	for i, f := range req.Files {
		listed[f.Path] = i
	}
	var warnings []string
	var set []publishEntry
	var gone []string // stored paths that F drops
	if req.Replace {
		for _, o := range current {
			if _, ok := listed[o.Key]; !ok {
				gone = append(gone, o.Key)
			}
		}
	} else {
		deletes := make(map[string]bool, len(req.Delete))
		for _, p := range req.Delete {
			if deletes[p] {
				continue
			}
			deletes[p] = true
			if _, ok := stored[p]; ok {
				gone = append(gone, p)
			} else {
				warnings = append(warnings, fmt.Sprintf(`"%s" in delete does not exist.`, truncate(p, 200)))
			}
		}
		for _, o := range current {
			if _, ok := listed[o.Key]; !ok && !deletes[o.Key] {
				set = append(set, publishEntry{path: o.Key, size: o.Size, file: -1})
			}
		}
	}
	for i, f := range req.Files {
		set = append(set, publishEntry{path: f.Path, size: int64(len(f.Data)), file: i})
	}
	slices.SortFunc(set, func(a, b publishEntry) int { return strings.Compare(a.path, b.path) })
	var total int64
	paths := make(map[string]bool, len(set))
	for _, e := range set {
		total += e.size
		paths[e.path] = true
	}
	switch lim := g.cfg.Sites; {
	case len(set) == 0:
		return nil, publishErr("E6", "A site needs at least one file.")
	case lim.MaxFiles > 0 && len(set) > lim.MaxFiles:
		return nil, publishErr("E13", "The site would have %d files; this Backyard allows %d per site.", len(set), lim.MaxFiles)
	case lim.MaxSiteBytes > 0 && total > lim.MaxSiteBytes:
		return nil, publishErr("E13", "The site would be %s; this Backyard allows %s per site.", FormatBytes(total), FormatBytes(lim.MaxSiteBytes))
	}
	if both := fileFolderConflicts(paths); len(both) > 0 {
		return nil, publishErr("E14", "These paths would be both a file and a folder: %s.", listPaths(both))
	}

	// Write. On a real directory tree (fs) a stored file where F needs a
	// folder, or the reverse, must go first; F drops all of them anyway.
	if g.store.Describe().Type == "fs" {
		entries := make([]FileEntry, len(set))
		for i, e := range set {
			entries[i] = FileEntry{Path: e.path}
		}
		if stale := treeConflicts(entries, current); len(stale) > 0 {
			if err := g.deleteObjects(ctx, site, stale, "file/directory conflict"); err != nil {
				return nil, g.publishFailed(ctx, site, "clear file/directory conflicts", err)
			}
			g.auditCtx(ctx, req.ClientIP, u, "deploy.clear_conflicts", site, "deleted", len(stale), "via", "mcp")
		}
	}
	man := &sites.Manifest{Site: site, Files: make([]sites.ManifestFile, len(set))}
	var (
		mu                  sync.Mutex
		uploaded, unchanged int
		vanished            []int // kept files deleted since the listing
	)
	err = parallel(ctx, len(set), putParallelism, func(ctx context.Context, i int) error {
		e := set[i]
		o, inStore := stored[e.path]
		prevEntry, known := recorded[e.path]
		// trusted: storage still holds what the previous manifest recorded.
		trusted := inStore && known && prevEntry.ETag == o.ETag && prevEntry.Size == o.Size
		if e.file < 0 {
			if trusted {
				man.Files[i] = prevEntry
				return nil
			}
			// An object the manifest does not describe (a direct sync, or
			// an upload that was never committed): adopt it as refresh does.
			mf, err := g.reg.FileDigest(ctx, site, e.path)
			if errors.Is(err, blob.ErrNotFound) {
				mu.Lock()
				vanished = append(vanished, i)
				mu.Unlock()
				return nil
			}
			if err != nil {
				return fmt.Errorf("hash %q: %w", e.path, err)
			}
			man.Files[i] = *mf
			return nil
		}
		f := req.Files[e.file]
		sum := sha256.Sum256(f.Data)
		digest := hex.EncodeToString(sum[:])
		if trusted && prevEntry.SHA256 == digest && prevEntry.Size == int64(len(f.Data)) {
			man.Files[i] = prevEntry
			mu.Lock()
			unchanged++
			mu.Unlock()
			return nil
		}
		info, err := g.store.Put(ctx, sites.SiteKey(site, f.Path), bytes.NewReader(f.Data), int64(len(f.Data)), f.ContentType)
		if err != nil {
			return fmt.Errorf("store %q: %w", f.Path, err)
		}
		etag := ""
		if info != nil {
			etag = info.ETag
		}
		if etag == "" {
			st, err := g.store.Stat(ctx, sites.SiteKey(site, f.Path))
			if err != nil {
				return fmt.Errorf("stat %q: %w", f.Path, err)
			}
			etag = st.ETag
		}
		man.Files[i] = sites.ManifestFile{Path: f.Path, Size: int64(len(f.Data)), SHA256: digest, ETag: etag}
		mu.Lock()
		uploaded++
		mu.Unlock()
		return nil
	})
	if err != nil {
		return nil, g.publishFailed(ctx, site, "store the files", err)
	}
	if len(vanished) > 0 {
		slices.Sort(vanished)
		for _, i := range slices.Backward(vanished) {
			man.Files = slices.Delete(man.Files, i, i+1)
		}
	}

	// The switch and the deletions run to the end once the files are
	// stored, so a client that gives up does not leave a half-switched site.
	cctx := context.WithoutCancel(ctx)
	now := g.now().UTC()
	man.UpdatedAt = now
	meta := g.nextMeta(prev, site, u, now)
	meta.DeployID = shortDeployID(newNonce())
	meta.FileCount = len(man.Files)
	hasIndex := false
	for _, mf := range man.Files {
		meta.Bytes += mf.Size
		hasIndex = hasIndex || mf.Path == sites.IndexFile
	}
	var page sites.Page
	if hasIndex {
		if i, ok := listed[sites.IndexFile]; ok {
			data := req.Files[i].Data
			page = sites.ExtractPage(data[:min(len(data), sites.TitleScanBytes)])
		} else {
			page = g.readPage(cctx, site)
		}
	}
	meta.Title, meta.Description = page.Title, page.Description
	meta.Preview = sites.ChoosePreview(page.Image, man.Files)
	if err := g.commitSite(cctx, man, meta); err != nil {
		return nil, g.publishFailed(ctx, site, "write the site manifest", err)
	}
	// Deletions go last so the site is never emptier than both versions.
	if err := g.deleteObjects(cctx, site, gone, "removed by publish"); err != nil {
		return nil, g.publishFailed(ctx, site, "delete removed files", err)
	}

	g.auditCtx(ctx, req.ClientIP, u, "deploy.commit", site,
		"deploy_id", meta.DeployID, "files", meta.FileCount, "bytes", meta.Bytes, "deleted", len(gone),
		"uploaded", uploaded, "unchanged", unchanged, "replace", req.Replace,
		"via", "mcp", "client", req.ClientName, "grant", req.GrantID[:min(len(req.GrantID), 8)])
	url := g.cfg.SiteURL(site)
	if !hasIndex {
		warnings = append([]string{fmt.Sprintf("No index.html at the site root: %s answers 404 until one is published.", url)}, warnings...)
	}
	if warnings == nil {
		warnings = []string{}
	}
	return &PublishResult{
		Site: site, URL: url, Created: !existed,
		Files: meta.FileCount, Uploaded: uploaded, Unchanged: unchanged, Deleted: len(gone),
		Bytes: meta.Bytes, Warnings: warnings,
	}, nil
}

// foreignPublisher reports whether updatedBy names someone other than u.
func foreignPublisher(updatedBy string, u *identity.User) bool {
	if updatedBy == "" {
		return false
	}
	for _, id := range []string{u.ID, u.Email, u.Actor()} {
		if id != "" && strings.EqualFold(updatedBy, id) {
			return false
		}
	}
	return true
}

// publishFailed logs a storage failure and returns E21.
func (g *Gateway) publishFailed(ctx context.Context, site, what string, err error) *PublishError {
	g.log.ErrorContext(ctx, "deploy: publish failed: could not "+what, "site", site, "error", err)
	return &PublishError{ID: "E21", Message: errStoreText}
}

// fileFolderConflicts returns the paths of a file set that are also a
// folder of another path in it, sorted.
func fileFolderConflicts(paths map[string]bool) []string {
	both := map[string]bool{}
	for p := range paths {
		for i := strings.LastIndexByte(p, '/'); i > 0; i = strings.LastIndexByte(p[:i], '/') {
			if paths[p[:i]] {
				both[p[:i]] = true
			}
		}
	}
	out := make([]string, 0, len(both))
	for p := range both {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// listPaths joins paths for a message, naming at most maxListedPaths.
func listPaths(paths []string) string {
	named := make([]string, 0, min(len(paths), maxListedPaths)+1)
	for i, p := range paths {
		if i == maxListedPaths {
			named = append(named, fmt.Sprintf("and %d more", len(paths)-maxListedPaths))
			break
		}
		named = append(named, truncate(p, 200))
	}
	return strings.Join(named, ", ")
}
