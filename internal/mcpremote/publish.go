package mcpremote

// backyard_publish (SPEC §14.1): decoding, the per-call cap, the tool
// errors and the cross-replica publish lease (step 5). The
// publish itself is deploy.Gateway.Publish.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/mcpserver"
)

const (
	// leaseTTL is how long a publish lease holds when its holder vanishes.
	leaseTTL = 300 * time.Second
	// releaseMargin: a publish that ends less than this before its lease
	// expires leaves the lease to expire, as a taker may already be taking
	// it over (no release lands on a lease taken over meanwhile).
	releaseMargin = 30 * time.Second
	// lockCollection holds the leases, one document per site.
	lockCollection = "_mcp_locks"
	// takeoverCollection holds the takeover claims of expired leases, one
	// per lease state taken over (and per claimant that died before it
	// wrote): <site>.<version>.<created_at µs>.<k>.
	takeoverCollection = "_mcp_lock_takeovers"
	// maxTakeoverClaims bounds the claims of one lease state.
	maxTakeoverClaims = 100
	// prunePageTakeovers bounds the expired claims one takeover prunes.
	prunePageTakeovers = 100
)

type publishArgs struct {
	Site      string        `json:"site"`
	Files     []publishFile `json:"files"`
	Delete    []string      `json:"delete"`
	Replace   bool          `json:"replace"`
	Overwrite bool          `json:"overwrite"`
}

type publishFile struct {
	Path        string `json:"path"`
	Content     string `json:"content"`
	Encoding    string `json:"encoding"`
	ContentType string `json:"content_type"`
}

// publishOutput is the result object of backyard_publish.
type publishOutput struct {
	Site      string   `json:"site"`
	URL       string   `json:"url"`
	Created   bool     `json:"created"`
	Files     int      `json:"files"`
	Uploaded  int      `json:"uploaded"`
	Unchanged int      `json:"unchanged"`
	Deleted   int      `json:"deleted"`
	Bytes     int64    `json:"bytes"`
	Warnings  []string `json:"warnings"`
}

func (h *handler) toolPublish(ctx context.Context, c *call, args json.RawMessage) toolResult {
	if h.gw == nil || h.publish == nil {
		return failure("Publishing through this Backyard's hub is turned off (deploy.embedded_gateway: false).")
	}
	var a publishArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return failure("Invalid arguments: " + truncate(err.Error(), maxDetail))
	}
	req := deploy.PublishRequest{
		User: c.user, Site: a.Site, Files: make([]deploy.PublishFile, len(a.Files)), Delete: a.Delete,
		Replace: a.Replace, Overwrite: a.Overwrite,
		ClientIP: h.clientIP(c.r), ClientName: c.grant.ClientName, GrantID: c.grant.ID,
	}
	for i, f := range a.Files {
		req.Files[i] = deploy.PublishFile{Path: f.Path, ContentType: f.ContentType}
	}
	var textBinary []string // W2
	decode := func(i int) ([]byte, *deploy.PublishError) {
		f := &a.Files[i]
		var data []byte
		if f.Encoding == "base64" {
			var ok bool
			if data, ok = decodeBase64(f.Content); !ok {
				return nil, &deploy.PublishError{ID: "E7", Message: fmt.Sprintf(`files[%d] (%s): "content" is not valid base64.`, i, truncate(f.Path, 200))}
			}
		} else {
			data = []byte(f.Content)
			if mcpserver.BinaryExt(f.Path) {
				textBinary = append(textBinary, fmt.Sprintf(`%s was sent as text but looks binary; binary files need "encoding": "base64".`, truncate(f.Path, 200)))
			}
		}
		f.Content = "" // the decoded copy is the one kept
		return data, nil
	}
	if perr := h.gw.CheckPublish(&req, decode); perr != nil {
		return h.publishError(ctx, c, perr)
	}
	release, perr := h.acquireLease(ctx, req.Site, c.user.Actor())
	if perr != nil {
		return h.publishError(ctx, c, perr)
	}
	res, perr := func() (*deploy.PublishResult, *deploy.PublishError) {
		defer release()
		return h.publish(ctx, req)
	}()
	if h.o.OnPublish != nil {
		h.o.OnPublish(req.Site, perr == nil)
	}
	if perr != nil {
		return h.publishError(ctx, c, perr)
	}
	warnings := make([]string, 0, len(res.Warnings)+len(textBinary))
	// W1 first, then W2 (from the files), then the gateway's other warnings.
	rest := res.Warnings
	if len(rest) > 0 && strings.HasPrefix(rest[0], "No index.html") {
		warnings, rest = append(warnings, rest[0]), rest[1:]
	}
	warnings = append(append(warnings, textBinary...), rest...)
	return okJSON(c, publishOutput{
		Site: res.Site, URL: res.URL, Created: res.Created, Files: res.Files, Uploaded: res.Uploaded,
		Unchanged: res.Unchanged, Deleted: res.Deleted, Bytes: res.Bytes, Warnings: warnings,
	})
}

// publishError turns a refusal into the tool error; storage failures (E21)
// name the request.
func (h *handler) publishError(ctx context.Context, c *call, perr *deploy.PublishError) toolResult {
	if perr.ID == "E21" {
		h.log.WarnContext(ctx, "mcp: publish failed", "request_id", c.reqID)
		return failure("Publishing failed: Backyard could not store the files (request " + c.reqID + "). Publishing the same files again completes it.")
	}
	return publishFailure(perr)
}

// decodeBase64 decodes standard or URL-safe base64, padding optional, no
// whitespace.
func decodeBase64(s string) ([]byte, bool) {
	if strings.ContainsAny(s, " \t\r\n\f\v") {
		return nil, false
	}
	body := strings.TrimRight(s, "=")
	if pad := len(s) - len(body); pad > 2 || (pad > 0 && len(s)%4 != 0) {
		return nil, false
	}
	std, url := strings.ContainsAny(body, "+/"), strings.ContainsAny(body, "-_")
	if std && url {
		return nil, false
	}
	enc := base64.RawStdEncoding
	if url {
		enc = base64.RawURLEncoding
	}
	b, err := enc.DecodeString(body)
	return b, err == nil
}

// acquireLease takes the publish lease of site (SPEC §14.1), which
// serializes the connector's publishes of one site across replicas: CLI
// and hub deploys do not take it. The returned release must be called once
// the publish returned. Without a document store there is no lease.
//
// The lease document carries a random holder. An expired lease is taken
// over only by the taker that first claims the state it read (its version
// and creation time) with an atomic Create of a takeover claim: the others
// answer E20 without writing, so no taker deletes or renames a lease
// another one holds.
func (h *handler) acquireLease(ctx context.Context, site, actor string) (func(), *deploy.PublishError) {
	if h.docs == nil {
		return func() {}, nil
	}
	exp := h.now().Add(leaseTTL).Unix()
	holder := newHolder()
	lease := map[string]any{"exp": exp, "holder": holder}
	release := func() { h.releaseLease(ctx, site, holder) }
	busy := &deploy.PublishError{ID: "E20", Message: fmt.Sprintf("Another publish to %s is in progress; call backyard_publish again when it has finished.", site)}
	failed := func(err error) (func(), *deploy.PublishError) {
		h.log.ErrorContext(ctx, "mcp: publish lease", "site", site, "error", err)
		return nil, &deploy.PublishError{ID: "E21"}
	}
	for attempt := 0; ; attempt++ {
		_, err := h.docs.Create(ctx, docstore.SystemSite, lockCollection, site, lease, actor)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, docstore.ErrConflict) {
			return failed(err)
		}
		if attempt > 0 {
			return nil, busy
		}
		doc, err := h.docs.Get(ctx, docstore.SystemSite, lockCollection, site)
		switch {
		case errors.Is(err, docstore.ErrNotFound):
			continue // released in between: try once more
		case err != nil:
			return failed(err)
		case leaseExp(doc) >= h.now().Unix():
			return nil, busy
		}
		// Expired: its holder died. Claim the state read, so that of the
		// takers that read it exactly one writes the lease.
		claimed, err := h.claimTakeover(ctx, site, doc, exp, actor)
		switch {
		case err != nil:
			return failed(err)
		case !claimed:
			return nil, busy // another taker claimed it first
		}
		taken, err := h.docs.Update(ctx, docstore.SystemSite, lockCollection, site, lease, actor)
		switch {
		case errors.Is(err, docstore.ErrNotFound):
			continue // released in between: try once more
		case err != nil:
			return failed(err)
		case taken.Version != doc.Version+1 || !taken.CreatedAt.Equal(doc.CreatedAt):
			return nil, busy
		}
		h.pruneTakeovers(ctx)
		return release, nil
	}
}

// claimTakeover claims the takeover of the expired lease doc with an atomic
// Create of a claim (exp: the new lease's): only the taker whose Create
// succeeds may write the lease. A claim is kept until it expires, after any
// taker that read that state. A claim that expired while the lease is still
// in the state read lost its claimant before it wrote: the next claim of
// the chain may be taken.
func (h *handler) claimTakeover(ctx context.Context, site string, doc *docstore.Doc, exp int64, actor string) (bool, error) {
	for k := 1; k <= maxTakeoverClaims; k++ {
		id := takeoverID(site, doc, k)
		_, err := h.docs.Create(ctx, docstore.SystemSite, takeoverCollection, id, map[string]any{"exp": exp}, actor)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, docstore.ErrConflict) {
			return false, err
		}
		claim, err := h.docs.Get(ctx, docstore.SystemSite, takeoverCollection, id)
		switch {
		case errors.Is(err, docstore.ErrNotFound):
			continue // pruned: it had expired
		case err != nil:
			return false, err
		case leaseExp(claim) >= h.now().Unix():
			return false, nil // a live claim: another taker is taking it over
		}
	}
	return false, nil
}

// takeoverID is the id of claim k of the takeover of a lease state:
// <site>.<version>.<created_at µs>.<k>.
func takeoverID(site string, doc *docstore.Doc, k int) string {
	return site + "." + strconv.FormatInt(doc.Version, 10) + "." + strconv.FormatInt(doc.CreatedAt.UnixMicro(), 10) + "." + strconv.Itoa(k)
}

// pruneTakeovers deletes a page of expired takeover claims (best effort;
// takeovers are rare, so are claims).
func (h *handler) pruneTakeovers(ctx context.Context) {
	q := docstore.Query{Where: []docstore.Filter{{Field: "exp", Op: docstore.OpLt, Value: float64(h.now().Unix())}}, Limit: prunePageTakeovers}
	if err := q.Normalize(prunePageTakeovers); err != nil {
		return
	}
	page, err := h.docs.Query(ctx, docstore.SystemSite, takeoverCollection, q)
	if err != nil {
		h.log.DebugContext(ctx, "mcp: prune takeover claims", "error", err)
		return
	}
	for _, d := range page.Docs {
		if _, err := h.docs.Delete(ctx, docstore.SystemSite, takeoverCollection, d.ID); err != nil {
			h.log.DebugContext(ctx, "mcp: prune takeover claims", "error", err)
		}
	}
}

// newHolder returns a random lease holder id.
func newHolder() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// releaseLease deletes the lease of site if this publish still holds it and
// it expires no sooner than releaseMargin from now (an expired one may be
// taken over at any moment, and a delete must not land after that: such a
// lease is left to expire).
func (h *handler) releaseLease(ctx context.Context, site, holder string) {
	ctx = context.WithoutCancel(ctx)
	doc, err := h.docs.Get(ctx, docstore.SystemSite, lockCollection, site)
	switch {
	case errors.Is(err, docstore.ErrNotFound):
		return
	case err != nil:
		h.log.WarnContext(ctx, "mcp: release publish lease (it expires by itself)", "site", site, "error", err)
		return
	case doc.Data["holder"] != holder || leaseExp(doc) < h.now().Add(releaseMargin).Unix():
		return
	}
	if _, err := h.docs.Delete(ctx, docstore.SystemSite, lockCollection, site); err != nil {
		h.log.WarnContext(ctx, "mcp: release publish lease (it expires by itself)", "site", site, "error", err)
	}
}

// leaseExp returns the expiry of a lease document (0 when unreadable, so a
// damaged lease counts as expired).
func leaseExp(doc *docstore.Doc) int64 {
	if doc == nil {
		return 0
	}
	switch v := doc.Data["exp"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
}
