package mcpremote

// backyard_publish (SPEC §14.1): decoding, the per-call cap, the tool
// errors and the cross-replica publish lease (step 5). The
// publish itself is deploy.Gateway.Publish.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/mcpserver"
)

const (
	// leaseTTL is how long a publish lease holds when its holder vanishes.
	leaseTTL = 300 * time.Second
	// lockCollection holds the leases, one document per site.
	lockCollection = "_mcp_locks"
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
func (h *handler) acquireLease(ctx context.Context, site, actor string) (func(), *deploy.PublishError) {
	if h.docs == nil {
		return func() {}, nil
	}
	exp := h.now().Add(leaseTTL).Unix()
	busy := &deploy.PublishError{ID: "E20", Message: fmt.Sprintf("Another publish to %s is in progress; call backyard_publish again when it has finished.", site)}
	failed := func(err error) (func(), *deploy.PublishError) {
		h.log.ErrorContext(ctx, "mcp: publish lease", "site", site, "error", err)
		return nil, &deploy.PublishError{ID: "E21"}
	}
	for attempt := 0; ; attempt++ {
		_, err := h.docs.Create(ctx, docstore.SystemSite, lockCollection, site, map[string]any{"exp": exp}, actor)
		if err == nil {
			return func() { h.releaseLease(ctx, site, exp) }, nil
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
		// Expired: its holder died. Take it over.
		if _, err := h.docs.Delete(ctx, docstore.SystemSite, lockCollection, site); err != nil {
			return failed(err)
		}
	}
}

// releaseLease deletes the lease of site if it is still the one taken with
// exp (one that expired and was taken over belongs to another publish).
func (h *handler) releaseLease(ctx context.Context, site string, exp int64) {
	ctx = context.WithoutCancel(ctx)
	doc, err := h.docs.Get(ctx, docstore.SystemSite, lockCollection, site)
	switch {
	case errors.Is(err, docstore.ErrNotFound):
		return
	case err == nil && leaseExp(doc) != exp:
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
