package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/thasami/backyard/internal/sites"
)

// fileSource is where a site's static files come from: the blob store (with
// the cache) or, for dev sites, a local directory.
type fileSource interface {
	// lookup returns the file at a site-relative path or errObjectNotFound.
	lookup(ctx context.Context, rel string) (*object, error)
	// exists reports whether the site has any files at all.
	exists(ctx context.Context) (bool, error)
}

func (s *Server) source(site string) fileSource {
	if dir, ok := s.devSites[site]; ok {
		return dirSource{dir: dir}
	}
	return blobSource{s: s, site: site}
}

type blobSource struct {
	s    *Server
	site string
}

func (b blobSource) lookup(ctx context.Context, rel string) (*object, error) {
	return b.s.loadBlob(ctx, sites.SiteKey(b.site, rel))
}

func (b blobSource) exists(ctx context.Context) (bool, error) {
	return b.s.siteExists(ctx, b.site)
}

// siteExists reports (with a short-lived cache) whether a site has files.
func (s *Server) siteExists(ctx context.Context, site string) (bool, error) {
	if _, dev := s.devSites[site]; dev {
		return true, nil
	}
	if v, ok := s.siteCache.get(site); ok {
		return v, nil
	}
	gen := s.siteCache.generation()
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	ok, err := s.deps.Sites.Exists(ctx, site)
	if err != nil {
		return false, err
	}
	s.siteCache.put(site, ok, gen)
	return ok, nil
}

// dirSource serves a local directory live (backyard dev). Lookups go through
// os.Root, so neither ".." nor symlinks can escape the directory. Hidden
// files and directories (any path segment starting with ".", except
// ".well-known") are never served: deploys skip them too (SPEC §13), and
// they hold things like .env and .git.
type dirSource struct {
	dir string
}

// hiddenPath reports whether a site-relative path has a segment that
// deploys ignore by default (dotfiles and dot-directories other than
// .well-known).
func hiddenPath(rel string) bool {
	for seg := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(seg, ".") && seg != ".well-known" {
			return true
		}
	}
	return false
}

func (d dirSource) lookup(_ context.Context, rel string) (*object, error) {
	if rel == "" || strings.HasSuffix(rel, "/") || hiddenPath(rel) || strings.ContainsRune(rel, '\\') {
		return nil, errObjectNotFound
	}
	root, err := os.OpenRoot(d.dir)
	if err != nil {
		return nil, errObjectNotFound
	}
	defer root.Close()
	name := filepath.FromSlash(rel)
	fi, err := root.Stat(name)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, errObjectNotFound
	}
	base := path.Base(rel)
	dir := d.dir
	return &object{
		name:        base,
		size:        fi.Size(),
		modTime:     fi.ModTime(),
		etag:        formatETag("", fi.Size(), fi.ModTime()),
		contentType: contentTypeFor(base, ""),
		open: func(_ context.Context, offset, length int64) (io.ReadCloser, error) {
			root, err := os.OpenRoot(dir)
			if err != nil {
				return nil, errObjectNotFound
			}
			defer root.Close()
			f, err := root.Open(name)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return nil, errObjectNotFound
				}
				return nil, err
			}
			if offset > 0 {
				if _, err := f.Seek(offset, io.SeekStart); err != nil {
					f.Close()
					return nil, err
				}
			}
			return limitedFile{Reader: io.LimitReader(f, length), f: f}, nil
		},
	}, nil
}

func (d dirSource) exists(context.Context) (bool, error) { return true, nil }

type limitedFile struct {
	io.Reader
	f *os.File
}

func (l limitedFile) Close() error { return l.f.Close() }

// serveStatic implements SPEC §3 for a site; r's path is site-relative and
// already canonical.
func (s *Server) serveStatic(w http.ResponseWriter, r *http.Request, site string) {
	if !s.requireMethods(w, r, http.MethodGet) {
		return
	}
	_, r, ok := s.authenticate(w, r, false)
	if !ok {
		return
	}
	ri := info(r)
	ctx := r.Context()
	src := s.source(site)

	find := func(rel string) (*object, error) {
		o, err := src.lookup(ctx, rel)
		if errors.Is(err, errObjectNotFound) {
			return nil, nil
		}
		return o, err
	}

	p := r.URL.Path
	rel := strings.TrimPrefix(p, "/")
	var (
		obj *object
		err error
	)
	if p == "/" || strings.HasSuffix(p, "/") {
		obj, err = find(rel + "index.html")
	} else {
		obj, err = find(rel)
		if obj == nil && err == nil {
			var idx *object
			idx, err = find(rel + "/index.html")
			if idx != nil {
				loc := ri.origURIPath() + "/"
				if r.URL.RawQuery != "" {
					loc += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, loc, http.StatusMovedPermanently)
				return
			}
			if err == nil {
				obj, err = find(rel + ".html")
			}
		}
	}
	if err != nil {
		s.fail(w, r, s.storageError(err))
		return
	}
	if obj != nil {
		s.serveObject(w, r, obj, serveOptions{})
		return
	}

	// Fallbacks: SPA 200.html, custom 404.html, built-in pages.
	if spa, err := find("200.html"); err != nil {
		s.fail(w, r, s.storageError(err))
		return
	} else if spa != nil {
		s.serveObject(w, r, spa, serveOptions{})
		return
	}
	if nf, err := find("404.html"); err != nil {
		s.fail(w, r, s.storageError(err))
		return
	} else if nf != nil {
		s.serveObject(w, r, nf, serveOptions{status: http.StatusNotFound, cacheControl: "no-store"})
		return
	}
	exists, err := src.exists(ctx)
	if err != nil {
		s.fail(w, r, s.storageError(err))
		return
	}
	if !exists {
		s.fail(w, r, siteNotFound(site))
		return
	}
	s.fail(w, r, pageNotFound())
}
