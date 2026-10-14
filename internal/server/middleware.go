package server

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"
)

// statusRecorder wraps a ResponseWriter to observe the status code and the
// number of body bytes. It forwards Flush (and FlushError), Hijack and
// ReadFrom and supports http.ResponseController through Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
	hijacked    bool
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.wroteHeader || w.hijacked {
		return
	}
	// Informational responses other than 101 may precede the final one.
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		w.ResponseWriter.WriteHeader(code)
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom lets io.Copy use the underlying writer's optimized path.
func (w *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := io.Copy(w.ResponseWriter, src)
	w.bytes += n
	return n, err
}

// Flush implements http.Flusher.
func (w *statusRecorder) Flush() { _ = w.FlushError() }

// FlushError flushes buffered data to the client and reports write errors
// (used by http.ResponseController.Flush, so streaming handlers notice a
// client that went away).
func (w *statusRecorder) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Hijack implements http.Hijacker (websockets).
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
		if !w.wroteHeader {
			w.status = http.StatusSwitchingProtocols
			w.wroteHeader = true
		}
	}
	return c, brw, err
}

// Unwrap supports http.ResponseController.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// middleware wraps the router with request ids, security headers, metrics,
// the access log and panic recovery.
func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ri := &reqInfo{start: time.Now(), area: "other"}
		ri.requestID = requestID(r.Header.Get("X-Request-Id"))
		s.resolveClient(ri, r)
		ri.origURI = r.URL.RequestURI()

		h := w.Header()
		h.Set("X-Request-Id", ri.requestID)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")

		rec := &statusRecorder{ResponseWriter: w}
		ri.rec = rec
		r = r.WithContext(withInfo(r.Context(), ri))

		defer s.observe(rec, r, ri)
		s.serveRecover(rec, r, next)
	})
}

// serveRecover runs next and turns panics into 500 responses.
func (s *Server) serveRecover(w *statusRecorder, r *http.Request, next http.Handler) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			panic(v)
		}
		ri := info(r)
		s.log.ErrorContext(r.Context(), "panic serving request",
			"panic", fmt.Sprint(v), "stack", string(debug.Stack()),
			"method", r.Method, "path", r.URL.Path, "request_id", ri.requestID)
		if w.hijacked {
			return
		}
		if w.wroteHeader {
			// The response is already on its way; abort the connection so
			// the client sees a truncated response rather than a bogus one.
			panic(http.ErrAbortHandler)
		}
		resetHeaders(w.Header(), ri.requestID)
		e := errInternal(nil, "internal server error")
		e.logged = true
		s.fail(w, r, e)
	}()
	next.ServeHTTP(w, r)
}

// resetHeaders clears handler-set headers, keeping the global ones.
func resetHeaders(h http.Header, requestID string) {
	for k := range h {
		delete(h, k)
	}
	h.Set("X-Request-Id", requestID)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
}

// observe records metrics and the access log entry of a finished request.
// It runs deferred, so it also sees aborted requests (http.ErrAbortHandler
// re-panicked by serveRecover), which are counted as 500 and re-panicked.
func (s *Server) observe(rec *statusRecorder, r *http.Request, ri *reqInfo) {
	p := recover()
	status := rec.status
	switch {
	case p != nil:
		status = http.StatusInternalServerError
	case !rec.wroteHeader:
		status = http.StatusOK
	}
	s.record(r, ri, status, rec.bytes)
	if p != nil {
		panic(p)
	}
}

func (s *Server) record(r *http.Request, ri *reqInfo, status int, bytes int64) {
	d := time.Since(ri.start)
	s.metrics.httpRequests.WithLabelValues(ri.area, strconv.Itoa(status)).Inc()
	s.metrics.httpDuration.WithLabelValues(ri.area).Observe(d.Seconds())
	if !s.cfg.Telemetry.AccessLog {
		return
	}
	attrs := []slog.Attr{
		slog.String("method", r.Method),
		slog.String("host", ri.hostPort),
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Int64("bytes", bytes),
		slog.Duration("duration", d),
		slog.String("area", ri.area),
		slog.String("ip", ri.clientIP),
		slog.String("request_id", ri.requestID),
	}
	if ri.site != "" {
		attrs = append(attrs, slog.String("site", ri.site))
	}
	if ri.user != nil {
		attrs = append(attrs, slog.String("user", ri.user.ID))
	}
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "http request", attrs...)
}

// requestID returns a valid incoming id or a fresh random one.
func requestID(in string) string {
	if n := len(in); n >= 8 && n <= 128 {
		ok := true
		for i := 0; i < n; i++ {
			c := in[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
				ok = false
				break
			}
		}
		if ok {
			return in
		}
	}
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
