package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/ratelimit"
)

// Error codes of the JSON API (SPEC §5).
const (
	codeBadRequest      = "bad_request"
	codeUnauthenticated = "unauthenticated"
	codeForbidden       = "forbidden"
	codeNotFound        = "not_found"
	codeConflict        = "conflict"
	codeTooLarge        = "too_large"
	codeRateLimited     = "rate_limited"
	codeUnsupported     = "unsupported"
	codeUpstream        = "upstream_error"
	codeInternal        = "internal"
)

// apiError is an HTTP error rendered as {"error":{...}} for API clients or as
// an HTML page for browsers.
type apiError struct {
	Status     int
	Code       string
	Message    string
	LoginURL   string
	RetryAfter time.Duration
	// Title overrides the HTML page heading (default derived from Status).
	Title string
	// Err is the underlying cause, logged for 5xx responses; never sent.
	Err error
	// logged suppresses the server-side log line (already logged).
	logged bool
}

func (e *apiError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%d %s: %s: %v", e.Status, e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

func newError(status int, code, format string, a ...any) *apiError {
	return &apiError{Status: status, Code: code, Message: fmt.Sprintf(format, a...)}
}

func errBadRequest(format string, a ...any) *apiError {
	return newError(http.StatusBadRequest, codeBadRequest, format, a...)
}

func errNotFound(format string, a ...any) *apiError {
	return newError(http.StatusNotFound, codeNotFound, format, a...)
}

func errForbidden(format string, a ...any) *apiError {
	return newError(http.StatusForbidden, codeForbidden, format, a...)
}

func errUnsupported(format string, a ...any) *apiError {
	return newError(http.StatusNotImplemented, codeUnsupported, format, a...)
}

func errInternal(err error, format string, a ...any) *apiError {
	e := newError(http.StatusInternalServerError, codeInternal, format, a...)
	e.Err = err
	return e
}

// errContext maps context errors of backend calls.
func errContext(err error, what string) *apiError {
	if errors.Is(err, context.DeadlineExceeded) {
		e := newError(http.StatusGatewayTimeout, codeUpstream, "%s timed out", what)
		e.Err = err
		return e
	}
	// The client went away; the status is never seen.
	e := newError(http.StatusServiceUnavailable, codeInternal, "request canceled")
	e.Err = err
	return e
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	LoginURL string `json:"login_url,omitempty"`
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(`{"error":{"code":"internal","message":"failed to encode response"}}`)
		status = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-store")
	}
	h.Set("Content-Length", strconv.Itoa(len(b)+1))
	w.WriteHeader(status)
	_, _ = w.Write(append(b, '\n'))
}

// prepareError logs server-side failures and sets common error headers.
func (s *Server) prepareError(w http.ResponseWriter, r *http.Request, e *apiError) {
	if e.Status >= 500 && e.Status != http.StatusNotImplemented && !e.logged {
		ri := info(r)
		if !errors.Is(e.Err, context.Canceled) {
			s.log.ErrorContext(r.Context(), "request failed",
				"status", e.Status, "code", e.Code, "message", e.Message, "error", e.Err,
				"method", r.Method, "path", r.URL.Path, "request_id", ri.requestID)
		}
	}
	h := w.Header()
	if e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(e.RetryAfter)))
	}
	// Drop validators/representation headers a handler may have set before
	// failing.
	for _, k := range []string{"ETag", "Last-Modified", "Content-Range", "Content-Disposition", "Content-Length", "Accept-Ranges"} {
		h.Del(k)
	}
	h.Set("Cache-Control", "no-store")
}

// writeAPIError renders e as JSON.
func (s *Server) writeAPIError(w http.ResponseWriter, r *http.Request, e *apiError) {
	s.prepareError(w, r, e)
	writeJSON(w, e.Status, errorBody{Error: errorDetail{Code: e.Code, Message: e.Message, LoginURL: e.LoginURL}})
}

// fail renders e as JSON for API/JSON clients and as an HTML page for
// browsers.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, e *apiError) {
	if wantsJSON(r) {
		s.writeAPIError(w, r, e)
		return
	}
	s.prepareError(w, r, e)
	s.writePage(w, r, e.Status, pageForError(e, s.publicURL))
}

// failAPI renders e as JSON when api is true, else like fail.
func (s *Server) failAPI(w http.ResponseWriter, r *http.Request, e *apiError, api bool) {
	if api {
		s.writeAPIError(w, r, e)
		return
	}
	s.fail(w, r, e)
}

// wantsJSON reports whether an error for r should be JSON: requests under
// the reserved prefix except file downloads, and clients that ask for JSON
// rather than HTML.
func wantsJSON(r *http.Request) bool {
	ri := info(r)
	rest := strings.TrimPrefix(ri.origURIPath(), ri.prefix)
	if strings.HasPrefix(rest, "/__backyard/") &&
		!strings.HasPrefix(rest, "/__backyard/files/") && !strings.HasPrefix(rest, "/__backyard/auth/") {
		return true
	}
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")
}

// origURIPath returns the path part of the original request URI.
func (ri *reqInfo) origURIPath() string {
	p, _, _ := strings.Cut(ri.origURI, "?")
	return p
}

func methodNotAllowed(allow ...string) *apiError {
	e := newError(http.StatusMethodNotAllowed, codeBadRequest, "method not allowed (allowed: %s)", strings.Join(allow, ", "))
	return e
}
