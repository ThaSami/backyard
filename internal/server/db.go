package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/ratelimit"
	"github.com/thasami/backyard/internal/realtime"
)

// publishTimeout bounds one realtime publish after a write.
const publishTimeout = 5 * time.Second

// docBodyLimit is the maximum request body of a document write: JSON with
// whitespace may be larger than the re-encoded data checked by PrepareData.
func (s *Server) docBodyLimit() int64 {
	if n := s.cfg.Database.MaxDocBytes; n > 0 {
		return int64(n)*2 + 64<<10
	}
	return 8 << 20
}

// dbError maps docstore errors to API errors.
func (s *Server) dbError(err error) *apiError {
	switch {
	case errors.Is(err, docstore.ErrNotFound):
		return errNotFound("document not found")
	case errors.Is(err, docstore.ErrConflict):
		return newError(http.StatusConflict, codeConflict, "a document with this id already exists")
	case errors.Is(err, docstore.ErrTooLarge):
		if n := s.cfg.Database.MaxDocBytes; n > 0 {
			return newError(http.StatusRequestEntityTooLarge, codeTooLarge, "document exceeds the maximum size of %d bytes", n)
		}
		return newError(http.StatusRequestEntityTooLarge, codeTooLarge, "document too large")
	case errors.Is(err, docstore.ErrQuota):
		return newError(http.StatusRequestEntityTooLarge, codeTooLarge,
			"this site has reached its document quota (%d documents)", s.cfg.Database.MaxDocsPerSite)
	case errors.Is(err, docstore.ErrInvalid):
		return errBadRequest("%s", invalidMessage(err))
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return errContext(err, "database request")
	}
	return errInternal(err, "database error")
}

// invalidMessage strips the docstore sentinel prefix from a validation error.
func invalidMessage(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, docstore.ErrInvalid.Error()+": "); i >= 0 {
		msg = msg[i+len(docstore.ErrInvalid.Error())+2:]
	}
	return msg
}

// dbResult is the metrics label of a docstore outcome.
func dbResult(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, docstore.ErrNotFound):
		return "not_found"
	case errors.Is(err, docstore.ErrConflict):
		return "conflict"
	case errors.Is(err, docstore.ErrTooLarge):
		return "too_large"
	case errors.Is(err, docstore.ErrQuota):
		return "quota"
	case errors.Is(err, docstore.ErrInvalid):
		return "invalid"
	}
	return "error"
}

func (s *Server) observeDB(op string, err error) {
	s.metrics.dbOps.WithLabelValues(op, dbResult(err)).Inc()
}

// collectionParam validates the {c} path value (user collections only).
func collectionParam(r *http.Request) (string, *apiError) {
	c := r.PathValue("c")
	if strings.HasPrefix(c, "_") {
		return "", errBadRequest("collection names starting with '_' are reserved for the system")
	}
	if err := docstore.ValidateCollection(c, false); err != nil {
		return "", errBadRequest("%s", invalidMessage(err))
	}
	return c, nil
}

// idParam validates the {id} path value.
func idParam(r *http.Request) (string, *apiError) {
	id := r.PathValue("id")
	if err := docstore.ValidateID(id); err != nil {
		return "", errBadRequest("%s", invalidMessage(err))
	}
	return id, nil
}

// bodyID extracts an optional "id" from a write body.
func bodyID(body map[string]any) (string, *apiError) {
	v, ok := body["id"]
	if !ok || v == nil {
		return "", nil
	}
	id, ok := v.(string)
	if !ok {
		return "", errBadRequest(`"id" must be a string`)
	}
	if id == "" {
		return "", nil
	}
	if err := docstore.ValidateID(id); err != nil {
		return "", errBadRequest("%s", invalidMessage(err))
	}
	return id, nil
}

// publish fans out a committed change; failures are logged only (the write
// itself succeeded).
func (s *Server) publish(ctx context.Context, site string, ev realtime.DBEvent) {
	if s.deps.Realtime == nil {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()
	if err := s.deps.Realtime.PublishDB(pctx, site, ev); err != nil {
		s.log.WarnContext(ctx, "realtime publish failed", "site", site, "collection", ev.Collection, "op", ev.Op, "error", err)
	}
}

type docsResponse struct {
	Docs    []*docstore.Doc `json:"docs"`
	HasMore bool            `json:"has_more"`
}

func pageResponse(p *docstore.Page) docsResponse {
	out := docsResponse{Docs: []*docstore.Doc{}}
	if p != nil {
		if p.Docs != nil {
			out.Docs = p.Docs
		}
		out.HasMore = p.HasMore
	}
	return out
}

// handleCollections serves GET /db.
func (s *Server) handleCollections(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := opCtx(r)
	defer cancel()
	names, err := s.docs.Collections(ctx, info(r).site)
	s.observeDB("collections", err)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if !strings.HasPrefix(n, "_") {
			out = append(out, n)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"collections": out})
}

// handleDBCreate serves POST /db/{c}.
func (s *Server) handleDBCreate(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, ratelimit.DBWrite) {
		return
	}
	c, e := collectionParam(r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	body, e := decodeObject(w, r, s.docBodyLimit())
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	id, e := bodyID(body)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	data, err := docstore.PrepareData(body, s.cfg.Database.MaxDocBytes)
	if err != nil {
		s.observeDB("create", err)
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	ri := info(r)
	ctx, cancel := opCtx(r)
	defer cancel()
	doc, err := s.docs.Create(ctx, ri.site, c, id, data, ri.user.Actor())
	s.observeDB("create", err)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	s.publish(ctx, ri.site, realtime.DBEvent{Collection: c, Op: realtime.OpCreate, DocID: doc.ID, Doc: doc})
	writeJSON(w, http.StatusCreated, doc)
}

// parseListQuery parses GET /db/{c}?limit=&offset=&order=a:asc,b:desc.
func parseListQuery(r *http.Request) (docstore.Query, *apiError) {
	var q docstore.Query
	v := r.URL.Query()
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return q, errBadRequest("limit must be a non-negative integer")
		}
		q.Limit = n
	}
	if s := v.Get("offset"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return q, errBadRequest("offset must be a non-negative integer")
		}
		q.Offset = n
	}
	// Spec form: order=field[:dir],field2[:dir]. Also accepted:
	// orderBy=field[,field2]&order=asc|desc (a bare direction).
	spec, defDir := strings.TrimSpace(v.Get("order")), ""
	if ob := strings.TrimSpace(firstNonEmpty(v.Get("orderBy"), v.Get("order_by"))); ob != "" {
		defDir = strings.ToLower(strings.TrimSpace(firstNonEmpty(v.Get("dir"), spec)))
		spec = ob
	}
	switch defDir {
	case "", "asc", "desc":
	default:
		return q, errBadRequest("order direction must be asc or desc, got %q", defDir)
	}
	if spec != "" {
		for _, item := range strings.Split(spec, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			field, dir, _ := strings.Cut(item, ":")
			field = strings.TrimSpace(field)
			dir = strings.ToLower(strings.TrimSpace(dir))
			if dir == "" {
				dir = defDir
			}
			switch dir {
			case "", "asc", "desc":
			default:
				return q, errBadRequest("order direction must be asc or desc, got %q", dir)
			}
			if field == "" {
				return q, errBadRequest("order field is empty")
			}
			q.OrderBy = append(q.OrderBy, docstore.Order{Field: field, Dir: dir, Desc: dir == "desc"})
		}
	}
	return q, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// handleDBList serves GET /db/{c}.
func (s *Server) handleDBList(w http.ResponseWriter, r *http.Request) {
	c, e := collectionParam(r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	q, e := parseListQuery(r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	s.runQuery(w, r, c, q)
}

// handleDBQuery serves POST /db/{c}/query.
func (s *Server) handleDBQuery(w http.ResponseWriter, r *http.Request) {
	c, e := collectionParam(r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	var q docstore.Query
	if e := decodeJSON(w, r, smallBodyLimit, &q, true); e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	s.runQuery(w, r, c, q)
}

func (s *Server) runQuery(w http.ResponseWriter, r *http.Request, c string, q docstore.Query) {
	if q.Limit < 0 {
		s.writeAPIError(w, r, errBadRequest("limit must be >= 0"))
		return
	}
	if err := q.Normalize(s.cfg.Database.MaxList); err != nil {
		s.observeDB("query", err)
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	page, err := s.docs.Query(ctx, info(r).site, c, q)
	s.observeDB("query", err)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	writeJSON(w, http.StatusOK, pageResponse(page))
}

// handleDBGet serves GET /db/{c}/{id}.
func (s *Server) handleDBGet(w http.ResponseWriter, r *http.Request) {
	c, e := collectionParam(r)
	if e == nil {
		var id string
		if id, e = idParam(r); e == nil {
			ctx, cancel := opCtx(r)
			defer cancel()
			doc, err := s.docs.Get(ctx, info(r).site, c, id)
			s.observeDB("get", err)
			if err != nil {
				s.writeAPIError(w, r, s.dbError(err))
				return
			}
			writeJSON(w, http.StatusOK, doc)
			return
		}
	}
	s.writeAPIError(w, r, e)
}

// handleDBPut serves PUT /db/{c}/{id} (create or replace).
func (s *Server) handleDBPut(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, ratelimit.DBWrite) {
		return
	}
	c, id, body, e := s.writeArgs(w, r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	if bid, e := bodyID(body); e != nil {
		s.writeAPIError(w, r, e)
		return
	} else if bid != "" && bid != id {
		s.writeAPIError(w, r, errBadRequest(`"id" in the body (%q) does not match the URL (%q)`, bid, id))
		return
	}
	data, err := docstore.PrepareData(body, s.cfg.Database.MaxDocBytes)
	if err != nil {
		s.observeDB("put", err)
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	ri := info(r)
	ctx, cancel := opCtx(r)
	defer cancel()
	doc, err := s.docs.Put(ctx, ri.site, c, id, data, ri.user.Actor())
	s.observeDB("put", err)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	op := realtime.OpUpdate
	if doc.Version <= 1 {
		op = realtime.OpCreate
	}
	s.publish(ctx, ri.site, realtime.DBEvent{Collection: c, Op: op, DocID: doc.ID, Doc: doc})
	writeJSON(w, http.StatusOK, doc)
}

// handleDBPatch serves PATCH /db/{c}/{id} (shallow merge; null deletes).
func (s *Server) handleDBPatch(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, ratelimit.DBWrite) {
		return
	}
	c, id, body, e := s.writeArgs(w, r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	patch, err := docstore.PrepareData(body, s.cfg.Database.MaxDocBytes)
	if err != nil {
		s.observeDB("update", err)
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	ri := info(r)
	ctx, cancel := opCtx(r)
	defer cancel()
	doc, err := s.docs.Update(ctx, ri.site, c, id, patch, ri.user.Actor())
	s.observeDB("update", err)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	s.publish(ctx, ri.site, realtime.DBEvent{Collection: c, Op: realtime.OpUpdate, DocID: doc.ID, Doc: doc})
	writeJSON(w, http.StatusOK, doc)
}

// writeArgs validates collection, id and the JSON object body of a write.
func (s *Server) writeArgs(w http.ResponseWriter, r *http.Request) (string, string, map[string]any, *apiError) {
	c, e := collectionParam(r)
	if e != nil {
		return "", "", nil, e
	}
	id, e := idParam(r)
	if e != nil {
		return "", "", nil, e
	}
	body, e := decodeObject(w, r, s.docBodyLimit())
	if e != nil {
		return "", "", nil, e
	}
	return c, id, body, nil
}

// handleDBDelete serves DELETE /db/{c}/{id}; 204 also when missing.
func (s *Server) handleDBDelete(w http.ResponseWriter, r *http.Request) {
	if !s.allow(w, r, ratelimit.DBWrite) {
		return
	}
	c, e := collectionParam(r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	id, e := idParam(r)
	if e != nil {
		s.writeAPIError(w, r, e)
		return
	}
	ri := info(r)
	ctx, cancel := opCtx(r)
	defer cancel()
	deleted, err := s.docs.Delete(ctx, ri.site, c, id)
	s.observeDB("delete", err)
	if err != nil {
		s.writeAPIError(w, r, s.dbError(err))
		return
	}
	if deleted {
		s.publish(ctx, ri.site, realtime.DBEvent{Collection: c, Op: realtime.OpDelete, DocID: id})
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}
