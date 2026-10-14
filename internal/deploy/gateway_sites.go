package deploy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/sites"
)

func (g *Gateway) handleInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, g.info(g.requestBase(r)))
}

func (g *Gateway) handleMe(w http.ResponseWriter, r *http.Request) {
	u, aerr := g.authenticate(w, r, accessRead)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (g *Gateway) handleListSites(w http.ResponseWriter, r *http.Request) {
	if _, aerr := g.authenticate(w, r, accessRead); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	list, err := g.reg.List(r.Context())
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	if list == nil {
		list = []sites.Meta{}
	}
	writeJSON(w, http.StatusOK, struct {
		Sites []sites.Meta `json:"sites"`
	}{list})
}

func (g *Gateway) handleGetSite(w http.ResponseWriter, r *http.Request) {
	if _, aerr := g.authenticate(w, r, accessRead); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	site, aerr := g.siteName(r, false)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	m, err := g.reg.Get(r.Context(), site)
	if err != nil {
		writeError(w, r, g.log, registryErr(site, err))
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (g *Gateway) handleFiles(w http.ResponseWriter, r *http.Request) {
	if _, aerr := g.authenticate(w, r, accessRead); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	site, aerr := g.siteName(r, false)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	ctx := r.Context()
	objs, err := g.reg.Files(ctx, site)
	if err != nil {
		writeError(w, r, g.log, registryErr(site, err))
		return
	}
	if len(objs) == 0 {
		ok, err := g.reg.Exists(ctx, site)
		if err != nil {
			writeError(w, r, g.log, internalErr(err))
			return
		}
		if !ok {
			writeError(w, r, g.log, notFound("site %q not found", site))
			return
		}
	}
	man, err := g.reg.GetManifest(ctx, site)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	known := make(map[string]sites.ManifestFile, len(man.Files))
	for _, f := range man.Files {
		known[f.Path] = f
	}
	files := make([]SiteFile, 0, len(objs))
	for _, o := range objs {
		sf := SiteFile{Path: o.Key, Size: o.Size, UpdatedAt: o.ModTime.UTC()}
		if mf, ok := known[o.Key]; ok && mf.ETag == o.ETag && mf.Size == o.Size {
			sf.SHA256 = mf.SHA256
		}
		files = append(files, sf)
	}
	writeJSON(w, http.StatusOK, struct {
		Files []SiteFile `json:"files"`
	}{files})
}

func (g *Gateway) handleReadFile(w http.ResponseWriter, r *http.Request) {
	if _, aerr := g.authenticate(w, r, accessRead); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	site, aerr := g.siteName(r, false)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	p := r.PathValue("path")
	if aerr := validSitePath(site, p); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	rc, info, err := g.store.Get(r.Context(), sites.SiteKey(site, p))
	if errors.Is(err, blob.ErrNotFound) {
		writeError(w, r, g.log, notFound("file %q not found in site %q", truncate(p, 200), site))
		return
	}
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	defer rc.Close()
	h := w.Header()
	ct := info.ContentType
	if ct == "" {
		ct = blob.DetectContentType(p, nil)
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.FormatInt(info.Size, 10))
	if info.ETag != "" {
		h.Set("ETag", info.ETag)
	}
	if !info.ModTime.IsZero() {
		h.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	}
	// The gateway may share the hub's origin: never let site content
	// execute there.
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Content-Disposition", "attachment")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		g.log.WarnContext(r.Context(), "deploy: read file interrupted", "site", site, "path", p, "error", err)
	}
}

func (g *Gateway) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	u, aerr := g.authenticate(w, r, accessManage)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	site, aerr := g.siteName(r, false)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	ctx := r.Context()
	unlock := g.lockSite(site)
	defer unlock()
	if _, aerr := g.authorizeSite(ctx, u, site); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	ok, err := g.reg.Exists(ctx, site)
	if err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	if !ok {
		writeError(w, r, g.log, notFound("site %q not found", site))
		return
	}
	if err := g.reg.Delete(ctx, site); err != nil {
		writeError(w, r, g.log, registryErr(site, err))
		return
	}
	g.audit(r, u, "site.delete", site)
	if g.opts.OnDeleteSite != nil {
		if err := g.opts.OnDeleteSite(ctx, site); err != nil {
			writeError(w, r, g.log, internalErr(fmt.Errorf("site %q deleted but cleanup failed: %w", site, err)))
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) handleOwners(w http.ResponseWriter, r *http.Request) {
	u, aerr := g.authenticate(w, r, accessManage)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	if !g.ownershipOn() {
		writeError(w, r, g.log, errf(http.StatusNotImplemented, CodeUnsupported, "site ownership is disabled on this server (sites.ownership: none)"))
		return
	}
	site, aerr := g.siteName(r, false)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	var req OwnersRequest
	if aerr := decodeJSON(w, r, maxOwnersBody, &req); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	add, aerr := normalizeOwnerList(req.Add)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	remove, aerr := normalizeOwnerList(req.Remove)
	if aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}

	ctx := r.Context()
	unlock := g.lockSite(site)
	defer unlock()
	if _, aerr := g.authorizeSite(ctx, u, site); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	m, err := g.reg.Get(ctx, site) // synthesized for direct-synced sites
	if err != nil {
		writeError(w, r, g.log, registryErr(site, err))
		return
	}
	owners := slices.Clone(m.Owners)
	for _, o := range add {
		if !slices.ContainsFunc(owners, func(x string) bool { return strings.EqualFold(x, o) }) {
			owners = append(owners, o)
		}
	}
	owners = slices.DeleteFunc(owners, func(x string) bool {
		return slices.ContainsFunc(remove, func(o string) bool { return strings.EqualFold(x, o) })
	})
	if len(owners) > maxOwners {
		writeError(w, r, g.log, badRequest("a site can have at most %d owners", maxOwners))
		return
	}
	m.Owners = owners
	if err := g.reg.PutMeta(ctx, m); err != nil {
		writeError(w, r, g.log, internalErr(err))
		return
	}
	g.audit(r, u, "site.owners", site, "added", add, "removed", remove, "owners", owners)
	writeJSON(w, http.StatusOK, m)
}

// normalizeOwnerList trims, lowercases and validates owner identifiers.
func normalizeOwnerList(in []string) ([]string, *apiError) {
	if len(in) > maxOwners {
		return nil, badRequest("too many owners in one request (max %d)", maxOwners)
	}
	out := make([]string, 0, len(in))
	for _, o := range in {
		o = strings.ToLower(strings.TrimSpace(o))
		if o == "" || len(o) > 254 || !utf8.ValidString(o) || strings.ContainsFunc(o, func(r rune) bool {
			return r <= ' ' || r == 0x7f || r == ','
		}) {
			return nil, badRequest("invalid owner %q (want an email or user id)", truncate(o, 100))
		}
		out = append(out, o)
	}
	return out, nil
}

func (g *Gateway) handleRefresh(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	unlock := g.lockSite(site)
	defer unlock()
	if _, aerr := g.authorizeSite(ctx, u, site); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	// A refresh hashes every file of the site: charge it like a deploy so
	// it cannot be used to make the server re-read large sites in a loop.
	if aerr := g.chargeDeploy(r, u); aerr != nil {
		writeError(w, r, g.log, aerr)
		return
	}
	extendDeadlines(w, longRequestTimeout)
	m, err := g.reg.Refresh(ctx, site, ownerID(u))
	if err != nil {
		writeError(w, r, g.log, registryErr(site, err))
		return
	}
	g.audit(r, u, "site.refresh", site, "files", m.FileCount, "bytes", m.Bytes)
	writeJSON(w, http.StatusOK, m)
}
