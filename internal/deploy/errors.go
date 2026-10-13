package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error codes shared with the site API (SPEC §5).
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthenticated = "unauthenticated"
	CodeForbidden       = "forbidden"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeTooLarge        = "too_large"
	CodeRateLimited     = "rate_limited"
	CodeUnsupported     = "unsupported"
	CodeUpstream        = "upstream_error"
	CodeInternal        = "internal"
)

// apiError is a gateway error with its HTTP mapping.
type apiError struct {
	status     int
	code       string
	msg        string
	loginURL   string
	retryAfter time.Duration
	cause      error // logged, never sent to clients
}

func (e *apiError) Error() string { return e.code + ": " + e.msg }

func (e *apiError) Unwrap() error { return e.cause }

func errf(status int, code, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

func badRequest(format string, args ...any) *apiError {
	return errf(http.StatusBadRequest, CodeBadRequest, format, args...)
}

func notFound(format string, args ...any) *apiError {
	return errf(http.StatusNotFound, CodeNotFound, format, args...)
}

func forbidden(format string, args ...any) *apiError {
	return errf(http.StatusForbidden, CodeForbidden, format, args...)
}

func tooLarge(format string, args ...any) *apiError {
	return errf(http.StatusRequestEntityTooLarge, CodeTooLarge, format, args...)
}

// internalErr wraps an unexpected error; the cause is logged, not exposed.
func internalErr(cause error) *apiError {
	return &apiError{status: http.StatusInternalServerError, code: CodeInternal, msg: "internal error", cause: cause}
}

// writeJSON writes v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = jsonEncode(w, v)
}

// jsonEncode writes v as JSON without HTML escaping.
func jsonEncode(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// writeError writes the JSON error envelope for err, logging server-side
// failures.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = internalErr(err)
	}
	if ae.status >= 500 {
		log.ErrorContext(r.Context(), "deploy gateway error",
			"method", r.Method, "path", logPath(r.URL.Path), "status", ae.status, "error", errOr(ae.cause, ae))
	}
	if ae.retryAfter > 0 {
		secs := int64((ae.retryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	}
	var body ErrorBody
	body.Error.Code = ae.code
	body.Error.Message = ae.msg
	body.Error.LoginURL = ae.loginURL
	writeJSON(w, ae.status, body)
}

// logPath returns a request path safe to log: upload URLs carry a signed
// capability token, which must not end up in logs.
func logPath(p string) string {
	if strings.HasPrefix(p, "/uploads/") {
		return "/uploads/[redacted]"
	}
	return p
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

// codeForStatus maps an HTTP status to the closest SPEC error code (used by
// the client when a response carries no JSON envelope).
func codeForStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return CodeUnauthenticated
	case status == http.StatusForbidden:
		return CodeForbidden
	case status == http.StatusNotFound:
		return CodeNotFound
	case status == http.StatusConflict:
		return CodeConflict
	case status == http.StatusRequestEntityTooLarge:
		return CodeTooLarge
	case status == http.StatusTooManyRequests:
		return CodeRateLimited
	case status == http.StatusNotImplemented:
		return CodeUnsupported
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		return CodeUpstream
	case status >= 500:
		return CodeInternal
	default:
		return CodeBadRequest
	}
}
