package mcpremote

// The read tools: backyard_list_sites, backyard_site_files and
// backyard_read_site_file (SPEC §14.1).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/mcpserver"
	"github.com/thasami/backyard/internal/sites"
)

const (
	// maxListedSites caps backyard_list_sites.
	maxListedSites = 200
	// maxListedFiles caps backyard_site_files.
	maxListedFiles = 2000
	// binarySniffBytes is how much of a file is checked for NUL bytes.
	binarySniffBytes = 8 << 10
)

// internalFailure logs err and returns the read tools' internal error.
func (h *handler) internalFailure(ctx context.Context, c *call, what string, err error) toolResult {
	h.log.ErrorContext(ctx, "mcp: "+what, "error", err, "request_id", c.reqID)
	return failure("Internal error (request " + c.reqID + ")")
}

func (h *handler) toolListSites(ctx context.Context, c *call, args json.RawMessage) toolResult {
	var a struct {
		Query string `json:"query"`
	}
	_ = json.Unmarshal(args, &a)
	if h.reg == nil {
		return h.internalFailure(ctx, c, "list sites", errors.New("no site registry"))
	}
	list, err := h.reg.List(ctx) // updated_at descending, then name
	if err != nil {
		return h.internalFailure(ctx, c, "list sites", err)
	}
	type site struct {
		Name        string   `json:"name"`
		URL         string   `json:"url"`
		Title       string   `json:"title,omitempty"`
		Description string   `json:"description,omitempty"`
		UpdatedAt   string   `json:"updated_at,omitempty"`
		UpdatedBy   string   `json:"updated_by,omitempty"`
		FileCount   int      `json:"file_count"`
		Bytes       int64    `json:"bytes"`
		Owners      []string `json:"owners,omitempty"`
	}
	out := struct {
		Sites     []site `json:"sites"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
	}{Sites: []site{}}
	q := strings.ToLower(strings.TrimSpace(a.Query))
	ownership := h.cfg.Sites.Ownership == "owner"
	for _, m := range list {
		if q != "" && !strings.Contains(strings.ToLower(m.Name), q) && !strings.Contains(strings.ToLower(m.Title), q) &&
			!strings.Contains(strings.ToLower(m.Description), q) {
			continue
		}
		out.Total++
		if len(out.Sites) == maxListedSites {
			out.Truncated = true
			continue
		}
		s := site{Name: m.Name, URL: m.URL, Title: m.Title, Description: m.Description, UpdatedAt: rfc3339(m.UpdatedAt),
			UpdatedBy: m.UpdatedBy, FileCount: m.FileCount, Bytes: m.Bytes}
		if s.URL == "" {
			s.URL = h.cfg.SiteURL(m.Name)
		}
		if ownership {
			s.Owners = m.Owners
		}
		out.Sites = append(out.Sites, s)
	}
	return okJSON(c, out)
}

// siteExists reports whether site exists; a failure is a tool result.
func (h *handler) siteExists(ctx context.Context, c *call, site string) (bool, *toolResult) {
	if h.reg == nil {
		res := h.internalFailure(ctx, c, "look up a site", errors.New("no site registry"))
		return false, &res
	}
	ok, err := h.reg.Exists(ctx, site)
	if err != nil {
		res := h.internalFailure(ctx, c, "look up a site", err)
		return false, &res
	}
	return ok, nil
}

func noSite(site string) toolResult {
	return failure(fmt.Sprintf(`There is no site named "%s" on this Backyard.`, site))
}

func (h *handler) toolSiteFiles(ctx context.Context, c *call, args json.RawMessage) toolResult {
	var a struct {
		Site string `json:"site"`
	}
	_ = json.Unmarshal(args, &a)
	if ok, res := h.siteExists(ctx, c, a.Site); res != nil {
		return *res
	} else if !ok {
		return noSite(a.Site)
	}
	files, err := h.reg.Files(ctx, a.Site)
	if err != nil {
		return h.internalFailure(ctx, c, "list a site's files", err)
	}
	slices.SortFunc(files, func(x, y blob.ObjectInfo) int { return strings.Compare(x.Key, y.Key) })
	type file struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	out := struct {
		Site      string `json:"site"`
		URL       string `json:"url"`
		Files     []file `json:"files"`
		Total     int    `json:"total"`
		Truncated bool   `json:"truncated"`
	}{Site: a.Site, URL: h.cfg.SiteURL(a.Site), Files: []file{}, Total: len(files), Truncated: len(files) > maxListedFiles}
	for _, f := range files[:min(len(files), maxListedFiles)] {
		out.Files = append(out.Files, file{f.Key, f.Size})
	}
	return okJSON(c, out)
}

func (h *handler) toolReadSiteFile(ctx context.Context, c *call, args json.RawMessage) toolResult {
	var a struct {
		Site string `json:"site"`
		Path string `json:"path"`
	}
	_ = json.Unmarshal(args, &a)
	site, p := a.Site, a.Path
	if ok, res := h.siteExists(ctx, c, site); res != nil {
		return *res
	} else if !ok {
		return noSite(site)
	}
	why := deploy.ValidatePath(p)
	if why == "" && !blob.ValidKey(sites.SiteKey(site, p)) {
		why = "too long for storage"
	}
	if why != "" {
		return failure(fmt.Sprintf(`"%s" is not an allowed path: %s.`, truncate(p, 200), why))
	}
	binary := failure(fmt.Sprintf("%s is a binary file and cannot be read as text; link to it by URL instead.", p))
	if mcpserver.BinaryExt(p) {
		return binary
	}
	key := sites.SiteKey(site, p)
	info, err := h.store.Stat(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return failure(fmt.Sprintf(`%s has no file "%s".`, site, p))
	}
	if err != nil {
		return h.internalFailure(ctx, c, "read a site file", err)
	}
	tooBig := func(n int64) toolResult {
		return failure(fmt.Sprintf("%s is %s; files up to 128 KiB can be read.", p, deploy.FormatBytes(n)))
	}
	if info.Size > maxReadBytes {
		return tooBig(info.Size)
	}
	rc, _, err := h.store.Get(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return failure(fmt.Sprintf(`%s has no file "%s".`, site, p))
	}
	if err != nil {
		return h.internalFailure(ctx, c, "read a site file", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxReadBytes+1))
	if err != nil {
		return h.internalFailure(ctx, c, "read a site file", err)
	}
	if len(b) > maxReadBytes { // grew since the Stat
		return tooBig(int64(len(b)))
	}
	if bytes.IndexByte(b[:min(len(b), binarySniffBytes)], 0) >= 0 {
		return binary
	}
	return ok(strings.ToValidUTF8(string(b), "�"))
}
