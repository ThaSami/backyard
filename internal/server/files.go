package server

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/ratelimit"
	"github.com/thasami/backyard/internal/sites"
)

// filesCollection is the system collection holding upload metadata (SPEC §5.3).
const filesCollection = "_files"

const (
	// maxFileName bounds sanitized file names (bytes).
	maxFileName = 200
	// uploadReadTimeout is the minimum read deadline for upload bodies.
	uploadReadTimeout = 30 * time.Minute
	// minUploadRate (bytes/s) is the slowest sustained upload the read
	// deadline allows for: bodies above uploadReadTimeout × minUploadRate
	// (450 MiB) get proportionally more time.
	minUploadRate = 256 << 10
	// maxUploadTimeout bounds the read deadline of any upload body.
	maxUploadTimeout = 24 * time.Hour
	// multipartOverhead allows for multipart framing and small fields.
	multipartOverhead = 1 << 20
	// maxFieldBytes bounds non-file form fields.
	maxFieldBytes = 4 << 10
)

// fileInfo is the FileInfo wire shape.
type fileInfo struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	URL         string    `json:"url"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
}

// fileMeta is the decoded metadata document of an upload.
type fileMeta struct {
	id          string
	name        string
	key         string
	size        int64
	contentType string
	createdAt   time.Time
	createdBy   string
}

// metaFromDoc decodes the metadata document of an upload of site. Documents
// whose blob key is not under uploads/<site>/ are rejected, so metadata can
// never expose another site's (or a site's static) objects.
func metaFromDoc(site string, d *docstore.Doc) (*fileMeta, bool) {
	if d == nil {
		return nil, false
	}
	m := &fileMeta{id: d.ID, createdAt: d.CreatedAt, createdBy: d.CreatedBy}
	m.name, _ = d.Data["name"].(string)
	m.key, _ = d.Data["key"].(string)
	m.contentType, _ = d.Data["content_type"].(string)
	switch v := d.Data["size"].(type) {
	case float64:
		m.size = int64(v)
	case int64:
		m.size = v
	case int:
		m.size = int64(v)
	}
	if m.key == "" || !blob.ValidKey(m.key) || !strings.HasPrefix(m.key, sites.UploadsPrefix+site+"/") {
		return nil, false
	}
	if m.name == "" {
		m.name = path.Base(m.key)
	}
	if m.contentType == "" {
		m.contentType = "application/octet-stream"
	}
	return m, true
}

// fileURL is the site-relative URL of an upload (path mode: with /s/<site>).
func (s *Server) fileURL(site, id, name string) string {
	prefix := ""
	if s.pathMode {
		prefix = sitePathPfx + site
	}
	return prefix + filesPfx + id + "/" + url.PathEscape(name)
}

func (s *Server) toFileInfo(site string, m *fileMeta) fileInfo {
	return fileInfo{
		ID: m.id, Name: m.name, URL: s.fileURL(site, m.id, m.name), Size: m.size,
		ContentType: m.contentType, CreatedAt: m.createdAt.UTC(), CreatedBy: m.createdBy,
	}
}

// sanitizeFileName makes a client-supplied name safe for blob keys, URLs and
// Content-Disposition: base name only, no control or separator characters,
// bounded length (keeping the extension).
func sanitizeFileName(name string) string {
	name = strings.ToValidUTF8(name, "")
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), r == '"', r == '<', r == '>', r == '|',
			r == '*', r == '?', r == ':', r == ' ', r == ' ', unicode.Is(unicode.Bidi_Control, r):
			b.WriteRune('_')
		case unicode.IsSpace(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.Trim(strings.TrimSpace(b.String()), ". ")
	if len(name) > maxFileName {
		ext := path.Ext(name)
		if len(ext) > 20 || !utf8.ValidString(ext) {
			ext = ""
		}
		stem := name[:maxFileName-len(ext)]
		for !utf8.ValidString(stem) && len(stem) > 0 {
			stem = stem[:len(stem)-1]
		}
		name = strings.TrimRight(stem, ". ") + ext
	}
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

// passiveUploadTypes are the media types an upload is served as without a
// sandbox: types browsers only display (raster images, plain text, PDF,
// JSON) or download. audio/* and video/* are passive too (see
// activeContent).
var passiveUploadTypes = map[string]bool{
	"image/png": true, "image/apng": true, "image/jpeg": true, "image/gif": true,
	"image/webp": true, "image/avif": true, "image/bmp": true,
	"image/x-icon": true, "image/vnd.microsoft.icon": true,
	"text/plain": true, "text/csv": true,
	"application/pdf": true, "application/json": true, "application/octet-stream": true,
}

// activeContent reports whether an upload of type ct is served with
// "Content-Security-Policy: sandbox". It uses an allowlist: every type that
// is not known to be passive is sandboxed. That covers HTML, SVG and XML in
// all its forms (text/xsl, */*+xml, …), which browsers render as documents
// that run script with the site's origin, and any other type a browser
// might treat that way. The type comes from the uploading client, so it
// cannot be trusted. audio/* and video/* subtypes are passive unless they
// carry a structured-syntax suffix: a browser renders "video/x+xml" as XML.
func activeContent(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return true
	}
	if passiveUploadTypes[mt] {
		return false
	}
	if kind, sub, _ := strings.Cut(mt, "/"); (kind == "audio" || kind == "video") && sub != "" && !strings.Contains(sub, "+") {
		return false
	}
	return true
}

// normalizeContentType validates a client-supplied media type.
func normalizeContentType(ct string) string {
	if ct == "" {
		return ""
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.Contains(mt, "/") {
		return ""
	}
	if cs, ok := params["charset"]; ok {
		return mime.FormatMediaType(mt, map[string]string{"charset": cs})
	}
	return mt
}

// errUploadTooLarge is returned by limitReader when the limit is exceeded.
var errUploadTooLarge = errors.New("upload too large")

// limitReader fails with errUploadTooLarge after more than max bytes.
type limitReader struct {
	r   io.Reader
	n   int64
	max int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > l.max {
		return n, errUploadTooLarge
	}
	return n, err
}

// uploadTimeout is how long reading an upload body of up to n bytes may
// take: uploadReadTimeout, or longer for bodies too large to arrive in that
// time at minUploadRate.
func uploadTimeout(n int64) time.Duration {
	secs := n / minUploadRate
	if secs >= int64(maxUploadTimeout/time.Second) {
		return maxUploadTimeout
	}
	return max(uploadReadTimeout, time.Duration(secs)*time.Second)
}

func (s *Server) maxUpload() int64 {
	if n := s.cfg.Files.MaxUploadBytes; n > 0 {
		return n
	}
	return 100 << 20
}

// storedFile is the result of writing an upload to the blob store.
type storedFile struct {
	id, key, name, contentType string
	size                       int64
}

// putUpload streams r to uploads/<site>/<id>/<name>.
func (s *Server) putUpload(ctx context.Context, site, name, clientType string, r io.Reader) (*storedFile, error) {
	name = sanitizeFileName(name)
	id := docstore.NewID()
	key := sites.UploadKey(site, id, name)
	br := bufio.NewReaderSize(r, 512)
	head, _ := br.Peek(512)
	ct := normalizeContentType(clientType)
	if ct == "" || ct == "application/octet-stream" {
		ct = blob.DetectContentType(name, head)
	}
	lr := &limitReader{r: br, max: s.maxUpload()}
	info, err := s.deps.Blob.Put(ctx, key, lr, -1, ct)
	if err != nil {
		s.deleteBlobQuietly(key)
		if errors.Is(err, errUploadTooLarge) || lr.n > lr.max {
			return nil, errUploadTooLarge
		}
		return nil, err
	}
	size := lr.n
	if info != nil && info.Size > 0 {
		size = info.Size
	}
	return &storedFile{id: id, key: key, name: name, contentType: ct, size: size}, nil
}

func (s *Server) deleteBlobQuietly(key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.deps.Blob.Delete(ctx, key); err != nil {
		s.log.Warn("cleanup of upload failed", "key", key, "error", err)
	}
}

// recordUpload writes the metadata document of a stored file.
func (s *Server) recordUpload(ctx context.Context, site string, user *identity.User, f *storedFile, displayName string) (*fileMeta, error) {
	if displayName == "" {
		displayName = f.name
	}
	data := map[string]any{
		"name":         displayName,
		"key":          f.key,
		"size":         f.size,
		"content_type": f.contentType,
	}
	doc, err := s.docs.Create(ctx, site, filesCollection, f.id, data, user.Actor())
	s.observeDB("create", err)
	if err != nil {
		s.deleteBlobQuietly(f.key)
		return nil, err
	}
	m, ok := metaFromDoc(site, doc)
	if !ok {
		return nil, fmt.Errorf("server: invalid file metadata for %q", f.id)
	}
	return m, nil
}

// storeBytes stores data as a site file (used for generated images).
func (s *Server) storeBytes(ctx context.Context, site string, user *identity.User, name, contentType string, data []byte) (*fileMeta, error) {
	f, err := s.putUpload(ctx, site, name, contentType, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return s.recordUpload(ctx, site, user, f, "")
}

// handleFileUpload serves POST /files (multipart/form-data, field "file",
// optional "name"), streaming the bytes to the blob store.
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, ratelimit.Upload) {
		return
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/form-data" {
		s.writeAPIError(w, r, errBadRequest("expected a multipart/form-data body with a \"file\" field"))
		return
	}
	limit := s.maxUpload()
	if r.ContentLength > limit+multipartOverhead {
		s.writeAPIError(w, r, newError(http.StatusRequestEntityTooLarge, codeTooLarge, "file exceeds the maximum upload size of %d bytes", limit))
		return
	}
	// Large uploads outlive server.read_timeout (which net/http applies to
	// the whole request, body included). Move the read deadline before
	// anything reads the body: the multipart reader below parses it lazily,
	// part by part, so every read of the upload happens after this point.
	// The response is written after the body, so the write deadline moves
	// too.
	n := limit + multipartOverhead
	if r.ContentLength > 0 {
		n = r.ContentLength
	}
	timeout := uploadTimeout(n)
	extendDeadlines(w, timeout, timeout+time.Minute)
	r.Body = http.MaxBytesReader(w, r.Body, limit+multipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeAPIError(w, r, errBadRequest("invalid multipart body: %v", err))
		return
	}
	ri := info(r)
	ctx := r.Context()
	var (
		stored    *storedFile
		nameField string
	)
	fail := func(e *apiError) {
		if stored != nil {
			s.deleteBlobQuietly(stored.key)
		}
		s.writeAPIError(w, r, e)
	}
	tooLarge := func() *apiError {
		return newError(http.StatusRequestEntityTooLarge, codeTooLarge, "file exceeds the maximum upload size of %d bytes", limit)
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				fail(tooLarge())
				return
			}
			fail(errBadRequest("invalid multipart body: %v", err))
			return
		}
		switch part.FormName() {
		case "file":
			if stored != nil {
				part.Close()
				fail(errBadRequest("only one file per request"))
				return
			}
			name := nameField
			if name == "" {
				name = part.FileName()
			}
			stored, err = s.putUpload(ctx, ri.site, name, part.Header.Get("Content-Type"), part)
			if err != nil {
				part.Close()
				var mbe *http.MaxBytesError
				switch {
				case errors.Is(err, errUploadTooLarge), errors.As(err, &mbe):
					fail(tooLarge())
				case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
					fail(errContext(err, "upload"))
				default:
					fail(s.storageError(err))
				}
				return
			}
		case "name":
			b, err := io.ReadAll(io.LimitReader(part, maxFieldBytes+1))
			if err != nil || len(b) > maxFieldBytes {
				part.Close()
				fail(errBadRequest(`invalid "name" field`))
				return
			}
			nameField = strings.TrimSpace(string(b))
		default:
			// Ignore unknown fields (bounded).
			if _, err := io.Copy(io.Discard, io.LimitReader(part, maxFieldBytes)); err != nil {
				part.Close()
				fail(errBadRequest("invalid multipart body: %v", err))
				return
			}
		}
		part.Close()
	}
	if stored == nil {
		s.writeAPIError(w, r, errBadRequest(`missing "file" field`))
		return
	}
	display := stored.name
	if nameField != "" {
		display = sanitizeFileName(nameField)
	}
	mctx, cancel := opCtx(r)
	defer cancel()
	m, err := s.recordUpload(mctx, ri.site, ri.user, stored, display)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	writeJSON(w, http.StatusCreated, s.toFileInfo(ri.site, m))
}

// handleFileList serves GET /files?limit=&offset= (newest first).
func (s *Server) handleFileList(w http.ResponseWriter, r *http.Request) {
	q, e := parseListQuery(r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	q.OrderBy = []docstore.Order{{Field: "_meta.created_at", Dir: "desc"}, {Field: "id", Dir: "desc"}}
	if err := q.Normalize(s.cfg.Database.MaxList); err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	site := info(r).site
	ctx, cancel := opCtx(r)
	defer cancel()
	page, err := s.docs.Query(ctx, site, filesCollection, q)
	s.observeDB("query", err)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	files := make([]fileInfo, 0, len(page.Docs))
	for _, d := range page.Docs {
		if m, ok := metaFromDoc(site, d); ok {
			files = append(files, s.toFileInfo(site, m))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "has_more": page.HasMore})
}

// lookupFile loads upload metadata by id.
func (s *Server) lookupFile(ctx context.Context, site, id string) (*fileMeta, *apiError) {
	if err := docstore.ValidateID(id); err != nil {
		return nil, errNotFound("file not found")
	}
	doc, err := s.docs.Get(ctx, site, filesCollection, id)
	s.observeDB("get", err)
	if errors.Is(err, docstore.ErrNotFound) {
		return nil, errNotFound("file not found")
	}
	if err != nil {
		return nil, s.dbError(err)
	}
	m, ok := metaFromDoc(site, doc)
	if !ok {
		return nil, errNotFound("file not found")
	}
	return m, nil
}

// handleFileGet serves GET /files/{id}.
func (s *Server) handleFileGet(w http.ResponseWriter, r *http.Request) {
	site := info(r).site
	ctx, cancel := opCtx(r)
	defer cancel()
	m, e := s.lookupFile(ctx, site, r.PathValue("id"))
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	writeJSON(w, http.StatusOK, s.toFileInfo(site, m))
}

// handleFileDelete serves DELETE /files/{id} (204 also when missing).
func (s *Server) handleFileDelete(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, ratelimit.DBWrite) {
		return
	}
	site := info(r).site
	ctx, cancel := opCtx(r)
	defer cancel()
	m, e := s.lookupFile(ctx, site, r.PathValue("id"))
	if e != nil && e.Status != http.StatusNotFound {
		s.writeAPIError(w, r, e)
		return
	}
	if m != nil {
		// Metadata first: once it is gone the bytes can no longer be
		// served or listed, even if removing the blob fails below.
		_, err := s.docs.Delete(ctx, site, filesCollection, m.id)
		s.observeDB("delete", err)
		if err != nil {
			s.writeAPIError(w, r, s.dbError(err))
			return
		}
		s.cache.remove(m.key)
		if err := s.deps.Blob.Delete(ctx, m.key); err != nil && !errors.Is(err, blob.ErrNotFound) {
			// The file is deleted as far as the API is concerned; only
			// storage space leaks.
			s.log.WarnContext(r.Context(), "deleting upload bytes failed", "site", site, "key", m.key, "error", err)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// serveUploadedFile serves /__backyard/files/{id}/{name}.
func (s *Server) serveUploadedFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethods(w, r, http.MethodGet) {
		return
	}
	_, r, ok := s.authenticate(w, r, false)
	if !ok {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, filesPfx)
	id, _, _ := strings.Cut(rest, "/")
	site := info(r).site
	ctx, cancel := context.WithTimeout(r.Context(), fetchTimeout)
	m, e := s.lookupFile(ctx, site, id)
	if e != nil {
		cancel()
		s.fail(w, r, e)
		return
	}
	obj, err := s.loadBlob(ctx, m.key)
	cancel()
	if errors.Is(err, errObjectNotFound) {
		s.fail(w, r, errNotFound("file not found"))
		return
	}
	if err != nil {
		s.fail(w, r, s.storageError(err))
		return
	}
	obj.name = m.name
	obj.contentType = m.contentType
	if strings.HasPrefix(obj.contentType, "text/") && !strings.Contains(obj.contentType, "charset") {
		obj.contentType += "; charset=utf-8"
	}
	h := w.Header()
	h.Set("Content-Disposition", contentDisposition("inline", m.name))
	if activeContent(obj.contentType) {
		// Uploaded documents must not run script with the site's origin.
		h.Set("Content-Security-Policy", "sandbox")
	}
	s.serveObject(w, r, obj, serveOptions{cacheControl: "private, no-cache"})
}

// contentDisposition renders a Content-Disposition header with an ASCII
// fallback and an RFC 5987 UTF-8 file name.
func contentDisposition(kind, name string) string {
	if v := mime.FormatMediaType(kind, map[string]string{"filename": name}); v != "" {
		return v
	}
	return kind
}
