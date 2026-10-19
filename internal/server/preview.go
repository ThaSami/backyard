package server

import (
	"errors"
	"net/http"

	"github.com/thasami/backyard/internal/sites"
)

const (
	// previewCacheVersioned is the Cache-Control of a preview requested
	// with a version (?v=…): the hub changes the URL when the site changes.
	previewCacheVersioned = "private, max-age=31536000, immutable"
	// previewCacheUnversioned revalidates every use.
	previewCacheUnversioned = "private, no-cache"
)

// handleSitePreview serves GET /__backyard/api/sites/{site}/preview (hub):
// the image named by the site's SiteMeta.preview (SPEC §7.2), from the
// hub's own origin so that it loads with the credentials of the site list
// in every URL and auth mode. The file is site content on the hub's
// origin: its type comes from its extension alone and it is sandboxed
// (sites.PreviewCSP), so an SVG can neither run script nor load anything.
// Dev sites have no stored preview, and a site the site list does not show
// to the viewer (sites.listing) answers as one without a preview.
func (s *Server) handleSitePreview(w http.ResponseWriter, r *http.Request) {
	site := r.PathValue("site")
	noPreview := func() {
		s.writeAPIError(w, r, errNotFound("site %q has no preview", truncate(site, 100)))
	}
	if _, dev := s.devSites[site]; dev {
		noPreview()
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	m, err := s.deps.Sites.GetMeta(ctx, site)
	switch {
	case errors.Is(err, sites.ErrNotFound) || errors.Is(err, sites.ErrCorrupt):
		noPreview()
		return
	case err != nil && ctx.Err() != nil:
		s.writeAPIError(w, r, errContext(err, "site lookup"))
		return
	case err != nil:
		s.writeAPIError(w, r, errInternal(err, "site lookup failed"))
		return
	}
	ct := sites.PreviewType(m.Preview)
	if ct == "" || !sites.Listed(s.cfg, info(r).user, m) {
		noPreview()
		return
	}
	// A versioned response is kept for a year, so it is read from storage
	// after the meta: uploads precede the commit that changed the version,
	// while the object cache may still hold the previous bytes (another
	// instance's or a standalone gateway's deploy does not purge it, and a
	// local purge runs only after the meta is written).
	key := sites.SiteKey(site, m.Preview)
	versioned := r.URL.Query().Get("v") != ""
	var obj *object
	if versioned {
		obj, err = s.loadBlobFresh(ctx, key)
	} else {
		obj, err = s.loadBlob(ctx, key)
	}
	switch {
	case errors.Is(err, errObjectNotFound):
		noPreview()
		return
	case err != nil:
		s.writeAPIError(w, r, s.storageError(err))
		return
	}
	// The 1 MiB cap holds when serving too: an upload of a deploy that was
	// never committed, or a direct sync, replaces the file in place (SPEC
	// §7.3) while the meta still names it. The object holds no open body
	// (serveObject opens it), so there is nothing to release.
	if obj.size > sites.MaxPreviewBytes {
		noPreview()
		return
	}
	obj.contentType = ct
	cc := previewCacheUnversioned
	if versioned {
		cc = previewCacheVersioned
	}
	w.Header().Set("Content-Security-Policy", sites.PreviewCSP)
	s.serveObject(w, r, obj, serveOptions{cacheControl: cc})
}
