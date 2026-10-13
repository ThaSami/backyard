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
	"os"
	"strings"
	"sync"

	"github.com/thasami/backyard/internal/sites"
)

// DefaultConcurrency is the default number of parallel uploads.
const DefaultConcurrency = 8

// maxConcurrency bounds DeployOptions.Concurrency.
const maxConcurrency = 64

// DeployOptions tune Client.Deploy.
type DeployOptions struct {
	Ignore      []string // extra gitignore-style patterns
	Message     string
	Concurrency int // parallel uploads (default 8)
	DryRun      bool
	// Progress receives progress events. Calls are serialized, so the
	// callback need not be safe for concurrent use.
	Progress func(ProgressEvent)
}

// ProgressEvent reports deploy progress.
type ProgressEvent struct {
	Phase string // "scan" | "plan" | "upload" | "commit" | "done"
	Path  string
	Done  int
	Total int
	Bytes int64
}

// DeployResult summarizes a deploy.
type DeployResult struct {
	Site      sites.Meta
	URL       string
	Uploaded  int
	Unchanged int
	Deleted   int
	Bytes     int64
	Files     []LocalFile // the collected files (also filled for DryRun)
}

// progress serializes calls to a ProgressEvent callback.
type progress struct {
	mu sync.Mutex
	fn func(ProgressEvent)
}

func (p *progress) emit(e ProgressEvent) {
	if p.fn == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fn(e)
}

// Deploy collects files from dir (CollectFiles), creates a deploy, uploads
// changed files concurrently and commits.
//
// Uploads are retried (3 times, with backoff) on network errors, throttling
// and 5xx responses. Bytes in the result counts uploaded bytes (for DryRun:
// bytes that would be uploaded). DryRun never changes anything on the
// server: it compares the local files with GET /sites/{site}/files when the
// gateway is reachable, and otherwise reports every file as an upload.
func (c *Client) Deploy(ctx context.Context, site, dir string, opts DeployOptions) (*DeployResult, error) {
	if err := sites.ValidateName(site, nil); err != nil {
		return nil, err
	}
	prog := &progress{fn: opts.Progress}
	prog.emit(ProgressEvent{Phase: "scan"})
	files, err := CollectFiles(dir, opts.Ignore)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("deploy: no files to deploy in %s (check your ignore rules)", dir)
	}
	var total int64
	entries := make([]FileEntry, len(files))
	byPath := make(map[string]*LocalFile, len(files))
	for i := range files {
		f := &files[i]
		total += f.Size
		entries[i] = FileEntry{Path: f.Path, Size: f.Size, SHA256: f.SHA256, ContentType: f.ContentType}
		byPath[f.Path] = f
	}
	prog.emit(ProgressEvent{Phase: "scan", Done: len(files), Total: len(files), Bytes: total})

	res := &DeployResult{Site: sites.Meta{Name: site, Owners: []string{}}, Files: files}
	if opts.DryRun {
		return c.dryRun(ctx, site, files, res, prog)
	}

	created, err := c.createDeploy(ctx, site, CreateDeployRequest{Files: entries, Message: opts.Message})
	if err != nil {
		return nil, err
	}
	for _, up := range created.Uploads {
		if byPath[up.Path] == nil {
			return nil, fmt.Errorf("deploy: gateway requested an upload of unknown file %q", up.Path)
		}
	}
	res.Unchanged = len(created.Unchanged)
	prog.emit(ProgressEvent{Phase: "plan", Done: len(created.Unchanged), Total: len(created.Uploads)})

	conc := opts.Concurrency
	if conc <= 0 {
		conc = DefaultConcurrency
	}
	conc = min(conc, maxConcurrency)
	uploaded, err := c.uploadAll(ctx, created.Uploads, byPath, conc, prog)
	if err != nil {
		return nil, err
	}
	res.Uploaded, res.Bytes = len(created.Uploads), uploaded

	prog.emit(ProgressEvent{Phase: "commit", Total: len(entries)})
	committed, err := c.commitDeploy(ctx, site, created.DeployID, CommitRequest{Files: entries})
	if err != nil {
		return nil, err
	}
	res.Site, res.URL, res.Deleted = committed.Site, committed.URL, committed.Deleted
	if res.URL == "" {
		res.URL = committed.Site.URL
	}
	prog.emit(ProgressEvent{Phase: "done", Done: res.Uploaded, Total: len(entries), Bytes: res.Bytes})
	return res, nil
}

// dryRun fills res with the plan without changing anything.
func (c *Client) dryRun(ctx context.Context, site string, files []LocalFile, res *DeployResult, prog *progress) (*DeployResult, error) {
	remote, err := c.SiteFiles(ctx, site)
	known := map[string]SiteFile{}
	var ae *APIError
	switch {
	case err == nil:
		for _, f := range remote {
			known[f.Path] = f
		}
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		// New site: everything is uploaded.
	case ctx.Err() != nil:
		return nil, ctx.Err()
	default:
		remote = nil // unreachable or not allowed: report a full upload
	}
	local := make(map[string]bool, len(files))
	for _, f := range files {
		local[f.Path] = true
		if rf, ok := known[f.Path]; ok && rf.SHA256 == f.SHA256 && rf.Size == f.Size {
			res.Unchanged++
			continue
		}
		res.Uploaded++
		res.Bytes += f.Size
	}
	for _, rf := range remote {
		if !local[rf.Path] {
			res.Deleted++
		}
	}
	if m, err := c.GetSite(ctx, site); err == nil {
		res.Site, res.URL = *m, m.URL
	}
	prog.emit(ProgressEvent{Phase: "plan", Done: res.Unchanged, Total: res.Uploaded, Bytes: res.Bytes})
	prog.emit(ProgressEvent{Phase: "done", Total: len(files), Bytes: res.Bytes})
	return res, nil
}

// uploadAll performs uploads with conc workers, stopping at the first
// failure. It returns the number of bytes uploaded.
func (c *Client) uploadAll(ctx context.Context, ups []Upload, files map[string]*LocalFile, conc int, prog *progress) (int64, error) {
	if len(ups) == 0 {
		return 0, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		mu       sync.Mutex
		firstErr error
		done     int
		bytes    int64
		wg       sync.WaitGroup
	)
	work := make(chan Upload)
	for range min(conc, len(ups)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for up := range work {
				f := files[up.Path]
				err := c.upload(ctx, up, f)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("deploy: upload %s: %w", up.Path, err)
						cancel()
					}
					mu.Unlock()
					continue
				}
				done++
				bytes += f.Size
				ev := ProgressEvent{Phase: "upload", Path: up.Path, Done: done, Total: len(ups), Bytes: bytes}
				mu.Unlock()
				prog.emit(ev)
			}
		}()
	}
feed:
	for _, up := range ups {
		select {
		case work <- up:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if firstErr != nil {
		return bytes, firstErr
	}
	if err := ctx.Err(); err != nil {
		return bytes, err
	}
	return bytes, nil
}

// upload PUTs one file, retrying transient failures. Credentials are sent
// only to the gateway's own origin, never to presigned storage URLs.
func (c *Client) upload(ctx context.Context, up Upload, f *LocalFile) error {
	target, err := c.resolve(up.URL)
	if err != nil {
		return err
	}
	method := up.Method
	if method == "" {
		method = http.MethodPut
	}
	toGateway := c.sameOrigin(target)
	changed := func() error {
		return &permanentError{fmt.Errorf("%s changed since it was scanned; run the deploy again", f.Path)}
	}
	// sent hashes the bytes of the latest attempt, so content edited in
	// place after the scan (same size, new bytes) is detected even when the
	// target (a presigned storage URL) cannot verify the digest itself.
	var sent *hashingBody
	err = c.withRetry(ctx, retryUpload, func() (*http.Response, error) {
		st, err := os.Stat(f.Abs)
		if err != nil {
			return nil, &permanentError{err}
		}
		if st.Size() != f.Size {
			return nil, changed()
		}
		var body io.ReadCloser = http.NoBody
		sent = nil
		if f.Size > 0 {
			if sent, err = openHashing(f.Abs); err != nil {
				return nil, &permanentError{err}
			}
			body = sent
		}
		req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
		if err != nil {
			body.Close()
			return nil, &permanentError{err}
		}
		req.ContentLength = f.Size
		if f.Size > 0 {
			req.GetBody = func() (io.ReadCloser, error) {
				hb, err := openHashing(f.Abs)
				if err == nil {
					sent = hb
				}
				return hb, err
			}
		}
		for k, v := range up.Headers {
			req.Header.Set(k, v)
		}
		if toGateway {
			c.decorate(req)
		} else if c.UserAgent != "" {
			req.Header.Set("User-Agent", c.UserAgent)
		}
		return c.httpClient().Do(req)
	}, func(resp *http.Response) error {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		if n, sum := sent.digest(); n == f.Size && sum != f.SHA256 {
			return changed()
		}
		return nil
	})
	if err != nil {
		// The gateway's own upload endpoint rejects a digest mismatch with
		// 400; explain the likely cause instead.
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
			if n, sum := sent.digest(); n == f.Size && sum != f.SHA256 {
				return changed()
			}
		}
		return err
	}
	return nil
}

// hashingBody is an upload body that hashes what the transport reads.
type hashingBody struct {
	f  *os.File
	mu sync.Mutex // the transport may read in another goroutine
	h  hash.Hash
	n  int64
}

func openHashing(name string) (*hashingBody, error) {
	fh, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return &hashingBody{f: fh, h: sha256.New()}, nil
}

func (b *hashingBody) Read(p []byte) (int, error) {
	n, err := b.f.Read(p)
	b.mu.Lock()
	b.h.Write(p[:n])
	b.n += int64(n)
	b.mu.Unlock()
	return n, err
}

func (b *hashingBody) Close() error { return b.f.Close() }

// digest returns how many bytes were read and their sha256 (hex). A nil
// body (empty file) reports zero bytes of the empty digest.
func (b *hashingBody) digest() (int64, string) {
	if b == nil {
		return 0, emptySHA256
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n, hex.EncodeToString(b.h.Sum(nil))
}

// emptySHA256 is the digest of zero bytes.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// permanentError marks a local failure that must not be retried.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// resolve makes an upload URL absolute relative to the gateway base URL.
func (c *Client) resolve(raw string) (*url.URL, error) {
	ref, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("deploy: invalid upload URL: %w", err)
	}
	if ref.IsAbs() {
		if ref.Scheme != "http" && ref.Scheme != "https" {
			return nil, fmt.Errorf("deploy: unsupported upload URL scheme %q", ref.Scheme)
		}
		return ref, nil
	}
	base, err := url.Parse(c.base() + "/")
	if err != nil {
		return nil, fmt.Errorf("deploy: invalid gateway URL: %w", err)
	}
	return base.ResolveReference(ref), nil
}

// sameOrigin reports whether u has the gateway's scheme and host.
func (c *Client) sameOrigin(u *url.URL) bool {
	base, err := url.Parse(c.base())
	if err != nil {
		return false
	}
	return strings.EqualFold(base.Scheme, u.Scheme) && strings.EqualFold(base.Host, u.Host)
}
