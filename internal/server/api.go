package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

const (
	// apiTimeout bounds database/storage calls of ordinary API requests.
	apiTimeout = 30 * time.Second
	// smallBodyLimit bounds JSON bodies of small requests.
	smallBodyLimit = 1 << 20
)

// siteRoutes is the site API (SPEC §5), relative to /__backyard/api.
func (s *Server) siteRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me", s.handleMe)
	mux.HandleFunc("GET /users/{email}", s.handleUserLookup)
	mux.HandleFunc("GET /site", s.handleSite)

	mux.HandleFunc("GET /db", s.handleCollections)
	mux.HandleFunc("POST /db/{c}", s.handleDBCreate)
	mux.HandleFunc("GET /db/{c}", s.handleDBList)
	mux.HandleFunc("POST /db/{c}/query", s.handleDBQuery)
	mux.HandleFunc("GET /db/{c}/{id}", s.handleDBGet)
	mux.HandleFunc("PUT /db/{c}/{id}", s.handleDBPut)
	mux.HandleFunc("PATCH /db/{c}/{id}", s.handleDBPatch)
	mux.HandleFunc("DELETE /db/{c}/{id}", s.handleDBDelete)

	mux.HandleFunc("POST /files", s.handleFileUpload)
	mux.HandleFunc("GET /files", s.handleFileList)
	mux.HandleFunc("GET /files/{id}", s.handleFileGet)
	mux.HandleFunc("DELETE /files/{id}", s.handleFileDelete)

	mux.HandleFunc("POST /ai/chat", s.handleAIChat)
	mux.HandleFunc("POST /ai/image", s.handleAIImage)
	mux.HandleFunc("POST /ai/embed", s.handleAIEmbed)
	mux.HandleFunc("GET /ai/models", s.handleAIModels)

	mux.HandleFunc("POST /warehouse/query", s.handleWarehouseQuery)
	mux.HandleFunc("GET /warehouse", s.handleWarehouseDescribe)

	mux.HandleFunc("GET /ws", s.handleWS)
	return mux
}

// hubRoutes is the hub API (SPEC §2), relative to /__backyard/api.
func (s *Server) hubRoutes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me", s.handleMe)
	mux.HandleFunc("GET /sites", s.handleSites)
	mux.HandleFunc("GET /sites/{site}/preview", s.handleSitePreview)
	mux.HandleFunc("POST /tokens", s.handleTokens)

	// The Claude connector's connections (SPEC §8.4.7).
	mux.HandleFunc("GET /mcp/grants", s.handleMCPGrants)
	mux.HandleFunc("DELETE /mcp/grants", s.handleMCPRevokeGrants)
	mux.HandleFunc("DELETE /mcp/grants/{id}", s.handleMCPRevokeGrant)
	return mux
}

func (s *Server) serveSiteAPI(w http.ResponseWriter, r *http.Request) {
	r, ok := s.apiGate(w, r)
	if !ok {
		return
	}
	s.dispatch(s.siteMux, w, stripPrefix(r, apiRoot))
}

func (s *Server) serveHubAPI(w http.ResponseWriter, r *http.Request) {
	r, ok := s.apiGate(w, r)
	if !ok {
		return
	}
	s.dispatch(s.hubMux, w, stripPrefix(r, apiRoot))
}

// dispatch serves r with mux, rendering "no route" outcomes as JSON 404/405.
func (s *Server) dispatch(mux *http.ServeMux, w http.ResponseWriter, r *http.Request) {
	if _, pattern := mux.Handler(r); pattern != "" {
		mux.ServeHTTP(w, r)
		return
	}
	// Let the mux decide between 404 and 405 (and compute Allow) without
	// writing to the client.
	h, _ := mux.Handler(r)
	cw := &captureWriter{header: http.Header{}}
	h.ServeHTTP(cw, r)
	if cw.status == http.StatusMethodNotAllowed {
		allow := cw.header.Get("Allow")
		w.Header().Set("Allow", allow)
		s.writeAPIError(w, r, methodNotAllowed(allow))
		return
	}
	s.writeAPIError(w, r, errNotFound("no such endpoint: %s %s", r.Method, truncate(info(r).origURIPath(), 200)))
}

// captureWriter records status and headers and discards the body.
type captureWriter struct {
	header http.Header
	status int
}

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return len(b), nil
}
func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

// extendDeadlines pushes the connection's read/write deadlines out for slow
// requests (uploads, AI calls) that would otherwise hit server.read_timeout /
// server.write_timeout. A zero duration leaves that deadline unchanged.
func extendDeadlines(w http.ResponseWriter, read, write time.Duration) {
	rc := http.NewResponseController(w)
	now := time.Now()
	if read > 0 {
		_ = rc.SetReadDeadline(now.Add(read))
	}
	if write > 0 {
		_ = rc.SetWriteDeadline(now.Add(write))
	}
}

// opCtx bounds backend calls of a request.
func opCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), apiTimeout)
}

// decodeJSON reads one JSON value from the body (at most limit bytes).
func decodeJSON(w http.ResponseWriter, r *http.Request, limit int64, v any, strict bool) *apiError {
	return decodeJSONWith(w, r, limit, v, jsonOptions{strict: strict})
}

// jsonOptions tune decodeJSONWith.
type jsonOptions struct {
	// strict rejects unknown object fields.
	strict bool
	// useNumber decodes numbers held in interface values (any, []any,
	// map[string]any) as json.Number instead of float64, so integers
	// beyond 2^53 keep every digit.
	useNumber bool
}

// decodeJSONWith reads exactly one JSON value from the body (at most limit
// bytes) into v.
func decodeJSONWith(w http.ResponseWriter, r *http.Request, limit int64, v any, opt jsonOptions) *apiError {
	body := http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(body)
	if opt.strict {
		dec.DisallowUnknownFields()
	}
	if opt.useNumber {
		dec.UseNumber()
	}
	if err := dec.Decode(v); err != nil {
		return jsonError(err, limit)
	}
	// Exactly one value.
	if err := dec.Decode(&json.RawMessage{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errBadRequest("request body must contain a single JSON value")
		}
		return jsonError(err, limit)
	}
	return nil
}

func jsonError(err error, limit int64) *apiError {
	var mbe *http.MaxBytesError
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &mbe):
		return newError(http.StatusRequestEntityTooLarge, codeTooLarge, "request body exceeds %d bytes", limit)
	case errors.Is(err, io.EOF):
		return errBadRequest("request body is empty; expected JSON")
	case errors.As(err, &se):
		return errBadRequest("invalid JSON at offset %d: %v", se.Offset, se)
	case errors.As(err, &te):
		if te.Field != "" {
			return errBadRequest("invalid JSON: field %q must be %s", te.Field, jsonKind(te.Type.Kind().String()))
		}
		return errBadRequest("invalid JSON: expected %s, got %s", jsonKind(te.Type.Kind().String()), te.Value)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return errBadRequest("invalid JSON: unexpected end of input")
	}
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "json: ")
	return errBadRequest("invalid JSON: %s", msg)
}

func jsonKind(k string) string {
	switch k {
	case "map", "struct":
		return "an object"
	case "slice", "array":
		return "an array"
	case "string":
		return "a string"
	case "bool":
		return "a boolean"
	case "int", "int64", "int32", "float64", "float32", "uint", "uint64":
		return "a number"
	}
	return k
}

// decodeObject reads a JSON object body.
func decodeObject(w http.ResponseWriter, r *http.Request, limit int64) (map[string]any, *apiError) {
	var m map[string]any
	if e := decodeJSON(w, r, limit, &m, false); e != nil {
		return nil, e
	}
	if m == nil {
		return nil, errBadRequest("request body must be a JSON object")
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Identity

// handleMe serves GET /me (site and hub).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, info(r).user)
}

// handleUserLookup serves GET /users/{email}.
func (s *Server) handleUserLookup(w http.ResponseWriter, r *http.Request) {
	email := identity.NormalizeEmail(r.PathValue("email"))
	if email == "" || !strings.Contains(email, "@") {
		s.writeAPIError(w, r, errBadRequest("invalid email address"))
		return
	}
	if s.deps.Users == nil {
		s.writeAPIError(w, r, errNotFound("unknown user"))
		return
	}
	ctx, cancel := opCtx(r)
	defer cancel()
	u, err := s.deps.Users.Lookup(ctx, email)
	switch {
	case err != nil && ctx.Err() != nil:
		s.writeAPIError(w, r, errContext(err, "user lookup"))
		return
	case err != nil:
		s.writeAPIError(w, r, errInternal(err, "user lookup failed"))
		return
	case u == nil:
		s.writeAPIError(w, r, errNotFound("unknown user"))
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// handleSite serves GET /site.
func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	site := info(r).site
	ctx, cancel := opCtx(r)
	defer cancel()
	m, err := s.deps.Sites.Get(ctx, site)
	if errors.Is(err, sites.ErrNotFound) {
		if _, dev := s.devSites[site]; dev {
			m, err = &sites.Meta{Name: site, URL: s.cfg.SiteURL(site)}, nil
		}
	}
	switch {
	case errors.Is(err, sites.ErrNotFound):
		s.writeAPIError(w, r, errNotFound("site %q has not been deployed", site))
		return
	case err != nil && ctx.Err() != nil:
		s.writeAPIError(w, r, errContext(err, "site lookup"))
		return
	case err != nil:
		s.writeAPIError(w, r, errInternal(err, "site lookup failed"))
		return
	}
	if m.Owners == nil {
		m.Owners = []string{}
	}
	writeJSON(w, http.StatusOK, m)
}
