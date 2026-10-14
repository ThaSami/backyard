package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

// longRequestTimeout bounds create/commit/refresh requests on big sites.
const longRequestTimeout = 15 * time.Minute

// maxDeployBody returns the JSON body limit for create/commit requests.
func (g *Gateway) maxDeployBody() int64 {
	n := int64(g.cfg.Sites.MaxFiles)
	if n <= 0 {
		n = 10000
	}
	return 1<<20 + n*2048
}

// deployFiles decodes a create/commit body into v and validates (and
// normalizes in place) its file list, returning the total size.
func (g *Gateway) deployFiles(w http.ResponseWriter, r *http.Request, site string, v any, files *[]FileEntry) (int64, *apiError) {
	if aerr := decodeJSON(w, r, g.maxDeployBody(), v); aerr != nil {
		return 0, aerr
	}
	norm, total, aerr := validateFiles(site, *files, g.cfg.Sites)
	if aerr != nil {
		return 0, aerr
	}
	*files = norm
	return total, nil
}

func (g *Gateway) handleCreateDeploy(w http.ResponseWriter, r *http.Request) {
	u, aerr := g.authenticate(w, r, accessDeploy)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	site, aerr := g.siteName(r, true)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	extendDeadlines(w, longRequestTimeout)
	var req CreateDeployRequest
	total, aerr := g.deployFiles(w, r, site, &req, &req.Files)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	ctx := r.Context()
	if _, aerr := g.authorizeSite(ctx, u, site); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	if aerr := g.chargeDeploy(r, u); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}

	man, err := g.reg.GetManifest(ctx, site)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	current, err := g.reg.Files(ctx, site)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	stored := make(map[string]blob.ObjectInfo, len(current))
	for _, o := range current {
		stored[o.Key] = o
	}
	recorded := make(map[string]sites.ManifestFile, len(man.Files))
	for _, f := range man.Files {
		recorded[f.Path] = f
	}
	if g.store.Describe().Type == "fs" {
		if aerr := g.clearTreeConflicts(r, u, site, req.Files, current); aerr != nil {
			writeError(w, r, g.log, aerr)
			return
		}
	}

	now := g.now()
	exp := now.Add(DeployTTL)
	claims := deployClaims{Site: site, Actor: u.Actor(), Exp: exp.Unix(), Nonce: newNonce()}
	deployID, err := signClaims(g.deployKey, claims)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}

	resp := CreateDeployResponse{DeployID: deployID, Unchanged: []string{}, ExpiresAt: exp.UTC()}
	var changed []FileEntry
	var uploadBytes int64
	newFiles := 0 // uploads that would be orphans if never committed
	for _, f := range req.Files {
		mf, ok := recorded[f.Path]
		if ok && mf.SHA256 == f.SHA256 && mf.Size == f.Size {
			if o, ok := stored[f.Path]; ok && o.ETag == mf.ETag && o.Size == mf.Size {
				resp.Unchanged = append(resp.Unchanged, f.Path)
				continue
			}
		}
		if !ok {
			newFiles++
		}
		changed = append(changed, f)
		uploadBytes += f.Size
	}
	resp.Uploads, err = g.uploadsFor(ctx, g.requestBase(r), site, changed, claims, exp)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	g.audit(r, u, "deploy.create", site,
		"deploy_id", shortDeployID(claims.Nonce), "files", len(req.Files), "bytes", total,
		"uploads", len(resp.Uploads), "upload_bytes", uploadBytes, "new_files", newFiles,
		"message", truncate(req.Message, maxMessageBytes))
	writeJSON(w, http.StatusOK, resp)
}

// treeConflicts returns the stored site files that cannot coexist with the
// new file list in a directory tree: a stored file where the new list needs
// a directory ("docs" vs "docs/index.html") and stored files below a path
// the new list makes a file ("docs/index.html" vs "docs"). None of them is
// in the new list, so the commit would delete them anyway.
func treeConflicts(files []FileEntry, current []blob.ObjectInfo) []string {
	paths := make(map[string]bool, len(files))
	dirs := make(map[string]bool)
	for _, f := range files {
		paths[f.Path] = true
		for i := strings.IndexByte(f.Path, '/'); i > 0; i = nextSlash(f.Path, i) {
			dirs[f.Path[:i]] = true
		}
	}
	var out []string
	for _, o := range current {
		if paths[o.Key] {
			continue
		}
		conflict := dirs[o.Key]
		for i := strings.IndexByte(o.Key, '/'); !conflict && i > 0; i = nextSlash(o.Key, i) {
			conflict = paths[o.Key[:i]]
		}
		if conflict {
			out = append(out, o.Key)
		}
	}
	return out
}

// nextSlash returns the index of the next "/" in s after i, or -1.
func nextSlash(s string, i int) int {
	j := strings.IndexByte(s[i+1:], '/')
	if j < 0 {
		return -1
	}
	return i + 1 + j
}

// clearTreeConflicts deletes stored files that would make uploads fail on
// a store backed by a real directory tree (fs), where a path cannot be both
// a file and a directory. Object stores keep both until the commit removes
// the stale one, preserving "deletions last".
func (g *Gateway) clearTreeConflicts(r *http.Request, u *identity.User, site string, files []FileEntry, current []blob.ObjectInfo) *apiError {
	stale := treeConflicts(files, current)
	if len(stale) == 0 {
		return nil
	}
	unlock := g.lockSite(site)
	defer unlock()
	if err := g.deleteObjects(r.Context(), site, stale, "file/directory conflict"); err != nil {
		return internalErr(err)
	}
	g.audit(r, u, "deploy.clear_conflicts", site, "deleted", len(stale))
	return nil
}

// deleteObjects deletes files of site in parallel; why says in errors why
// they are deleted.
func (g *Gateway) deleteObjects(ctx context.Context, site string, paths []string, why string) error {
	return parallel(ctx, len(paths), deleteParallelism, func(ctx context.Context, i int) error {
		if err := g.store.Delete(ctx, sites.SiteKey(site, paths[i])); err != nil {
			return fmt.Errorf("delete %q (%s): %w", paths[i], why, err)
		}
		return nil
	})
}

// nextMeta returns the meta document of site after a write by u at now: the
// creation fields and owners of prev (the stored document, nil when none)
// are kept and, with sites.ownership: owner, a first writer that is not
// anonymous becomes the owner. The caller fills in the content fields.
func (g *Gateway) nextMeta(prev *sites.Meta, site string, u *identity.User, now time.Time) *sites.Meta {
	meta := &sites.Meta{Name: site, CreatedAt: now, CreatedBy: u.Actor()}
	if prev != nil {
		meta.CreatedAt, meta.CreatedBy, meta.Owners = prev.CreatedAt, prev.CreatedBy, prev.Owners
		if meta.CreatedAt.IsZero() {
			meta.CreatedAt = now
		}
	}
	if g.ownershipOn() && len(meta.Owners) == 0 && !u.Anonymous {
		meta.Owners = []string{ownerID(u)}
	}
	meta.UpdatedAt, meta.UpdatedBy = now, u.Actor()
	return meta
}

// commitSite switches a site to a new version: the manifest first, then the
// meta document (the site list shows a version once its manifest exists).
func (g *Gateway) commitSite(ctx context.Context, man *sites.Manifest, meta *sites.Meta) error {
	if err := g.reg.PutManifest(ctx, man); err != nil {
		return err
	}
	return g.reg.PutMeta(ctx, meta)
}

// presignParallelism bounds concurrent PresignPut calls (signing may be a
// remote call, e.g. IAM signBlob for GCS on Cloud Run).
const presignParallelism = 16

// uploadsFor returns one upload per changed file: presigned storage URLs
// when enabled and supported (probed with the first file), otherwise
// self-hosted gateway URLs.
func (g *Gateway) uploadsFor(ctx context.Context, base, site string, changed []FileEntry, dc deployClaims, exp time.Time) ([]Upload, error) {
	ups := make([]Upload, len(changed))
	ttl := time.Until(exp)
	presign := len(changed) > 0 && (g.cfg.Deploy.Presign == nil || *g.cfg.Deploy.Presign)
	if presign {
		up, ok := g.presignUpload(ctx, site, changed[0], ttl)
		presign = ok
		if ok {
			ups[0] = *up
		}
	}
	if presign {
		err := parallel(ctx, len(changed)-1, presignParallelism, func(ctx context.Context, i int) error {
			f := changed[i+1]
			up, ok := g.presignUpload(ctx, site, f, ttl)
			if !ok {
				var err error
				if up, err = g.selfUpload(base, site, f, dc, exp); err != nil {
					return err
				}
			}
			ups[i+1] = *up
			return nil
		})
		return ups, err
	}
	for i, f := range changed {
		up, err := g.selfUpload(base, site, f, dc, exp)
		if err != nil {
			return nil, err
		}
		ups[i] = *up
	}
	return ups, nil
}

// presignUpload asks the store for a presigned PUT bound to the file's
// declared size and SHA-256 (as far as the backend can enforce them; see
// blob.PresignPutOptions). It returns (nil, false) when the store cannot
// presign, so the caller stops trying for this deploy.
func (g *Gateway) presignUpload(ctx context.Context, site string, f FileEntry, ttl time.Duration) (*Upload, bool) {
	pr, err := g.store.PresignPut(ctx, sites.SiteKey(site, f.Path), blob.PresignPutOptions{
		ContentType: f.ContentType,
		Size:        f.Size,
		SHA256:      f.SHA256,
	}, ttl)
	if err != nil {
		if !errors.Is(err, blob.ErrNotSupported) {
			g.log.WarnContext(ctx, "deploy: presign failed; using gateway uploads", "site", site, "error", err)
		}
		return nil, false
	}
	method := pr.Method
	if method == "" {
		method = http.MethodPut
	}
	return &Upload{Path: f.Path, URL: pr.URL, Method: method, Headers: pr.Headers}, true
}

// selfUpload builds a gateway upload URL carrying a signed token.
func (g *Gateway) selfUpload(base, site string, f FileEntry, dc deployClaims, exp time.Time) (*Upload, error) {
	tok, err := signClaims(g.uploadKey, uploadClaims{
		Site:        site,
		Path:        f.Path,
		Size:        f.Size,
		SHA256:      f.SHA256,
		ContentType: f.ContentType,
		DeployID:    shortDeployID(dc.Nonce),
		Exp:         exp.Unix(),
	})
	if err != nil {
		return nil, err
	}
	return &Upload{
		Path:    f.Path,
		URL:     base + "/uploads/" + url.PathEscape(tok),
		Method:  http.MethodPut,
		Headers: map[string]string{"Content-Type": f.ContentType},
	}, nil
}

// verifyDeployID checks a deploy id against the site and user.
func (g *Gateway) verifyDeployID(id, site string, u *identity.User) (*deployClaims, *apiError) {
	var c deployClaims
	if err := verifyClaims(g.deployKey, id, &c); err != nil {
		return nil, badRequest("invalid deploy id")
	}
	if g.now().Unix() >= c.Exp {
		return nil, badRequest("deploy id expired; start a new deploy")
	}
	if c.Site != site {
		return nil, badRequest("deploy id belongs to another site")
	}
	sameActor := c.Actor == u.Actor() || (u.Anonymous && strings.HasPrefix(c.Actor, "anon:"))
	if !sameActor {
		return nil, forbidden("deploy id belongs to another user")
	}
	return &c, nil
}

func (g *Gateway) handleCommit(w http.ResponseWriter, r *http.Request) {
	u, aerr := g.authenticate(w, r, accessDeploy)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	site, aerr := g.siteName(r, true)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	claims, aerr := g.verifyDeployID(r.PathValue("id"), site, u)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	extendDeadlines(w, longRequestTimeout)
	var req CommitRequest
	total, aerr := g.deployFiles(w, r, site, &req, &req.Files)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}

	ctx := r.Context()
	unlock := g.lockSite(site)
	defer unlock()
	prev, aerr := g.authorizeSite(ctx, u, site)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	prevMan, err := g.reg.GetManifest(ctx, site)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	recorded := make(map[string]sites.ManifestFile, len(prevMan.Files))
	for _, f := range prevMan.Files {
		recorded[f.Path] = f
	}
	current, err := g.reg.Files(ctx, site)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	stored := make(map[string]blob.ObjectInfo, len(current))
	for _, o := range current {
		stored[o.Key] = o
	}

	var problems []commitProblem
	man := &sites.Manifest{Site: site, UpdatedAt: g.now().UTC(), Files: make([]sites.ManifestFile, 0, len(req.Files))}
	listed := make(map[string]bool, len(req.Files))
	var changed []int // man.Files entries whose stored object the previous manifest does not describe
	for _, f := range req.Files {
		listed[f.Path] = true
		o, ok := stored[f.Path]
		prev, known := recorded[f.Path]
		kept := known && ok && prev.ETag == o.ETag && prev.Size == o.Size
		switch {
		case !ok:
			problems = append(problems, commitProblem{path: f.Path, detail: "missing"})
			continue
		case o.Size != f.Size:
			problems = append(problems, g.wrongSize(f, o.Size, kept))
			continue
		}
		mf := sites.ManifestFile{Path: f.Path, Size: f.Size, SHA256: f.SHA256, ETag: o.ETag}
		if kept {
			// Storage still holds the object the previous manifest
			// described, so its recorded digest is the truth. A different
			// declared digest means this client skipped an upload (a
			// concurrent deploy changed the file after its plan, or a
			// buggy client): recording the claim would make later deploys
			// of that content wrongly "unchanged".
			mf.SHA256 = prev.SHA256
		} else {
			changed = append(changed, len(man.Files))
		}
		man.Files = append(man.Files, mf)
	}
	if len(problems) > 0 {
		g.rejectCommit(w, r, u, site, claims, problems)
		return
	}
	superseded, problems, err := g.verifyDigests(ctx, site, man.Files, changed)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	if len(problems) > 0 {
		g.rejectCommit(w, r, u, site, claims, problems)
		return
	}

	meta := g.nextMeta(prev, site, u, g.now().UTC())
	meta.DeployID = shortDeployID(claims.Nonce)
	meta.FileCount, meta.Bytes = len(req.Files), total
	if listed[sites.IndexFile] {
		meta.Title, meta.Description = g.readTitle(ctx, site)
	}
	if err := g.commitSite(ctx, man, meta); err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}

	// Deletions go last so the site is never emptier than both versions.
	var stale []string
	for _, o := range current {
		if !listed[o.Key] {
			stale = append(stale, o.Key)
		}
	}
	if err := g.deleteObjects(ctx, site, stale, "stale"); err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	deleted := len(stale)

	g.audit(r, u, "deploy.commit", site,
		"deploy_id", meta.DeployID, "files", meta.FileCount, "bytes", meta.Bytes, "deleted", deleted,
		"verified", len(changed), "superseded", superseded)
	meta.URL = g.cfg.SiteURL(site)
	if meta.Owners == nil {
		meta.Owners = []string{}
	}
	writeJSON(w, http.StatusOK, CommitResponse{Site: *meta, URL: meta.URL, Deleted: deleted})
}

// commitProblem is a listed file that storage does not hold as declared.
type commitProblem struct {
	path   string
	detail string
	// untrusted marks a stored object of the wrong size that may be an
	// unverified upload (see wrongSize): it must not stay live.
	untrusted bool
	deleted   bool
}

// uploadsBindSize reports whether every upload URL the gateway hands out
// binds the declared size: its own upload endpoint verifies size and
// digest, and S3 and GCS presigned URLs sign the size, but an Azure SAS URL
// cannot restrict the body at all.
func (g *Gateway) uploadsBindSize() bool {
	presign := g.cfg.Deploy.Presign == nil || *g.cfg.Deploy.Presign
	return !presign || g.store.Describe().Type != "azure"
}

// wrongSize describes a listed file stored with another size than this
// deploy declared; kept means storage still holds the version the previous
// manifest recorded (this deploy did not replace it).
//
// Where upload URLs bind the size, any other object at the path is the
// verified upload of another deploy of the site (deploys may overlap, see
// the package doc) or a direct sync: it stays, as rsync would leave it,
// and only this commit fails. Where they cannot (presigned Azure SAS URLs),
// an object other than the recorded version may be an upload of any size
// that nothing verified, so it is deleted.
func (g *Gateway) wrongSize(f FileEntry, size int64, kept bool) commitProblem {
	return commitProblem{
		path:      f.Path,
		detail:    fmt.Sprintf("%d bytes in storage, expected %d", size, f.Size),
		untrusted: !kept && !g.uploadsBindSize(),
	}
}

// verifyParallelism bounds concurrent digest lookups during a commit.
const verifyParallelism = 16

// verifyDigests sets the digest and etag of files[i], for each i in idx
// (listed files whose stored object the previous manifest does not
// describe), to those of the bytes stored now: a digest the store verified
// itself where it reports one, otherwise the hash of the object as read.
//
// The declared digest only says what this deploy uploaded. Another deploy
// of the site may have replaced the object since (same size, other
// bytes); pairing the declared digest with that object's etag would make
// later deploys of the declared content wrongly "unchanged". It returns how
// many files hold other bytes than declared, and problems for files that
// vanished or changed size since the listing.
func (g *Gateway) verifyDigests(ctx context.Context, site string, files []sites.ManifestFile, idx []int) (int, []commitProblem, error) {
	var (
		mu         sync.Mutex
		problems   []commitProblem
		superseded []string
	)
	err := parallel(ctx, len(idx), verifyParallelism, func(ctx context.Context, i int) error {
		mf := &files[idx[i]]
		got, err := g.reg.FileDigest(ctx, site, mf.Path)
		switch {
		case errors.Is(err, blob.ErrNotFound):
			mu.Lock()
			problems = append(problems, commitProblem{path: mf.Path, detail: "missing"})
			mu.Unlock()
			return nil
		case err != nil:
			return fmt.Errorf("verify %q: %w", mf.Path, err)
		case got.Size != mf.Size:
			mu.Lock()
			problems = append(problems, g.wrongSize(FileEntry{Path: mf.Path, Size: mf.Size}, got.Size, false))
			mu.Unlock()
			return nil
		case got.SHA256 != mf.SHA256:
			mu.Lock()
			superseded = append(superseded, mf.Path)
			mu.Unlock()
		}
		mf.SHA256, mf.ETag = got.SHA256, got.ETag
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	slices.SortFunc(problems, func(a, b commitProblem) int { return strings.Compare(a.path, b.path) })
	if len(superseded) > 0 {
		slices.Sort(superseded)
		g.log.WarnContext(ctx, "deploy: files hold other content than this deploy declared (replaced by another deploy or a sync since the upload); the manifest records the stored content",
			"site", site, "files", len(superseded), "paths", superseded[:min(len(superseded), maxReportedProblems)])
	}
	return len(superseded), problems, nil
}

// maxReportedProblems bounds the paths named in a failed commit's message.
const maxReportedProblems = 10

// rejectCommit fails a commit whose files are missing or of the wrong size
// with 409, naming the paths. Untrusted objects of the wrong size are
// deleted first, so a commit never leaves unverified bytes of the wrong
// size behind (objects not listed are only removed by a successful commit,
// see the package doc).
func (g *Gateway) rejectCommit(w http.ResponseWriter, r *http.Request, u *identity.User, site string, claims *deployClaims, problems []commitProblem) {
	ctx := r.Context()
	var untrusted []int
	for i, p := range problems {
		if p.untrusted {
			untrusted = append(untrusted, i)
		}
	}
	// Deletion must happen even if the client gives up waiting.
	_ = parallel(context.WithoutCancel(ctx), len(untrusted), deleteParallelism, func(ctx context.Context, i int) error {
		p := &problems[untrusted[i]]
		if err := g.store.Delete(ctx, sites.SiteKey(site, p.path)); err != nil {
			g.log.ErrorContext(ctx, "deploy: delete object of the wrong size", "site", site, "path", p.path, "error", err)
			return nil // keep deleting the others
		}
		p.deleted = true
		return nil
	})
	if len(untrusted) > 0 {
		deleted := 0
		for _, i := range untrusted {
			if problems[i].deleted {
				deleted++
			}
		}
		g.audit(r, u, "deploy.reject_uploads", site,
			"deploy_id", shortDeployID(claims.Nonce), "wrong_size", len(untrusted), "deleted", deleted)
	}
	msgs := make([]string, 0, min(len(problems), maxReportedProblems+1))
	for i, p := range problems {
		if i == maxReportedProblems {
			msgs = append(msgs, fmt.Sprintf("… and %d more", len(problems)-maxReportedProblems))
			break
		}
		detail := p.detail
		switch {
		case p.deleted:
			detail += "; deleted"
		case p.untrusted:
			detail += "; could not be deleted"
		}
		msgs = append(msgs, fmt.Sprintf("%s (%s)", truncate(p.path, 200), detail))
	}
	writeError(w, r, g.log, errf(http.StatusConflict, CodeConflict, "%d file(s) were not uploaded correctly: %s", len(problems), strings.Join(msgs, ", ")))
}

// readTitle extracts the title/description of a site's index.html.
func (g *Gateway) readTitle(ctx context.Context, site string) (string, string) {
	rc, _, err := g.store.GetRange(ctx, sites.SiteKey(site, sites.IndexFile), 0, sites.TitleScanBytes)
	if err != nil {
		g.log.WarnContext(ctx, "deploy: read index.html for title", "site", site, "error", err)
		return "", ""
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, sites.TitleScanBytes))
	if err != nil {
		g.log.WarnContext(ctx, "deploy: read index.html for title", "site", site, "error", err)
		return "", ""
	}
	return sites.ExtractTitle(b)
}

// ---------------------------------------------------------------------------
// Self-hosted uploads

var (
	errUploadTooLarge = errors.New("upload is larger than declared")
	errUploadMismatch = errors.New("upload does not match the declared size and sha256")
)

// verifyingReader hashes and counts what it reads, failing as soon as the
// body exceeds the declared size or, once complete, when the digest differs.
// Failing inside Read makes the store abort the write, so a bad upload never
// replaces a good file.
type verifyingReader struct {
	r    io.Reader
	h    hash.Hash
	n    int64
	size int64
	want string
	err  error // sticky verification failure
	ok   bool  // all bytes read and the digest matched
}

func newVerifyingReader(r io.Reader, size int64, sha string) *verifyingReader {
	return &verifyingReader{r: r, h: sha256.New(), size: size, want: sha}
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	if v.ok {
		// Everything was verified; anything further must be EOF.
		var one [1]byte
		if n, _ := v.r.Read(one[:]); n > 0 {
			v.ok, v.err = false, errUploadTooLarge
			return 0, v.err
		}
		return 0, io.EOF
	}
	if rem := v.size - v.n; int64(len(p)) > rem+1 {
		p = p[:rem+1] // read at most one byte past the declared size
	}
	n, err := v.r.Read(p)
	v.n += int64(n)
	if v.n > v.size {
		v.err = errUploadTooLarge
		return 0, v.err
	}
	v.h.Write(p[:n])
	if v.n == v.size {
		if hex.EncodeToString(v.h.Sum(nil)) != v.want {
			v.err = errUploadMismatch
			return n, v.err
		}
		v.ok = true
		return n, err
	}
	if errors.Is(err, io.EOF) {
		v.err = errUploadMismatch // short body
		return n, v.err
	}
	return n, err
}

// extendDeadlines lifts the server's read/write timeouts for a request that
// legitimately takes long (large uploads, commits of big sites). It is a
// no-op when the ResponseWriter does not support deadlines (e.g. Lambda);
// middleware wrapping the writer must implement Unwrap for it to apply.
func extendDeadlines(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	deadline := time.Now().Add(d)
	_ = rc.SetReadDeadline(deadline)
	_ = rc.SetWriteDeadline(deadline)
}

// uploadTimeout allows a slow (~64 KiB/s) client to finish an upload.
func uploadTimeout(size int64) time.Duration {
	return min(5*time.Minute+time.Duration(size/(64<<10))*time.Second, 2*time.Hour)
}

func (g *Gateway) handleUpload(w http.ResponseWriter, r *http.Request) {
	var c uploadClaims
	if err := verifyClaims(g.uploadKey, r.PathValue("token"), &c); err != nil {
		writeError(w, r, g.log, forbidden("invalid upload token"))
		return
	}
	if g.now().Unix() >= c.Exp {
		writeError(w, r, g.log, forbidden("upload token expired; start a new deploy"))
		return
	}
	if err := sites.ValidateName(c.Site, nil); err != nil {
		writeError(w, r, g.log, badRequest("invalid upload token"))
		return
	}
	if aerr := validSitePath(c.Site, c.Path); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	if c.Size < 0 || !isSHA256Hex(c.SHA256) {
		writeError(w, r, g.log, badRequest("invalid upload token"))
		return
	}
	if r.ContentLength >= 0 && r.ContentLength != c.Size {
		writeError(w, r, g.log, badRequest("Content-Length %d does not match the declared size %d", r.ContentLength, c.Size))
		return
	}
	extendDeadlines(w, uploadTimeout(c.Size))
	ct := c.ContentType
	if ct == "" {
		ct = blob.DetectContentType(c.Path, nil)
	}
	key := sites.SiteKey(c.Site, c.Path)
	vr := newVerifyingReader(r.Body, c.Size, strings.ToLower(c.SHA256))
	info, err := g.store.Put(r.Context(), key, vr, c.Size, ct)
	if err == nil && !vr.ok && vr.err == nil {
		// The store accepted the body without reading it to the end (e.g. an
		// empty object): verify the remainder now.
		_ = vr.finish()
	}
	if vr.err != nil || (err == nil && !vr.ok) {
		if err == nil {
			// Stored despite failing verification: remove it so no
			// unverified object survives.
			if derr := g.store.Delete(context.WithoutCancel(r.Context()), key); derr != nil {
				g.log.ErrorContext(r.Context(), "deploy: delete unverified upload", "key", key, "error", derr)
			}
		}
		if errors.Is(vr.err, errUploadTooLarge) {
			writeError(w, r, g.log, tooLarge("upload of %q is larger than the declared %d bytes", truncate(c.Path, 200), c.Size))
			return
		}
		writeError(w, r, g.log, badRequest("upload of %q does not match the declared size and sha256", truncate(c.Path, 200)))
		return
	}
	if err != nil {
		if r.Context().Err() != nil {
			g.log.WarnContext(r.Context(), "deploy: upload aborted by client", "site", c.Site, "path", c.Path)
		}
		writeError(w, r, g.log, internalErr(fmt.Errorf("store upload %q: %w", key, err)))
		return
	}
	g.log.DebugContext(r.Context(), "deploy: upload stored", "site", c.Site, "path", c.Path, "size", info.Size, "deploy_id", c.DeployID)
	writeJSON(w, http.StatusOK, map[string]any{"path": c.Path, "size": info.Size, "sha256": c.SHA256})
}

// finish drains the remainder of a partially consumed body through the
// verifier so that ok/err reflect the complete upload.
func (v *verifyingReader) finish() error {
	_, err := io.Copy(io.Discard, v)
	return err
}

// deleteParallelism bounds concurrent deletions during a commit.
const deleteParallelism = 16

// parallel runs fn(i) for i in [0,n) with at most par concurrent calls and
// returns the first error (cancelling the calls not yet started).
func parallel(ctx context.Context, n, par int, fn func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, max(1, par))
loop:
	for i := range n {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := fn(ctx, i); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
