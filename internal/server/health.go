package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// readyTimeout bounds the readiness probe's backend pings.
const readyTimeout = 5 * time.Second

func writePlain(w http.ResponseWriter, r *http.Request, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// handleHealthz is the liveness probe (no auth, any host).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writePlain(w, r, http.StatusOK, "ok\n")
}

// handleReadyz pings the document store and the blob store (no auth, any
// host). Failure details are logged, not returned.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
	defer cancel()
	type check struct {
		name string
		ping func(context.Context) error
	}
	checks := []check{
		{"database", s.deps.Docs.Ping},
		{"storage", s.deps.Blob.Ping},
	}
	errs := make([]error, len(checks))
	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.ping(ctx)
		}()
	}
	wg.Wait()
	var failed []string
	for i, err := range errs {
		if err != nil {
			failed = append(failed, checks[i].name)
			s.log.WarnContext(r.Context(), "readiness check failed", "check", checks[i].name, "error", err)
		}
	}
	if len(failed) > 0 {
		writePlain(w, r, http.StatusServiceUnavailable, "not ready: "+strings.Join(failed, ", ")+"\n")
		return
	}
	writePlain(w, r, http.StatusOK, "ok\n")
}
