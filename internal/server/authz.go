package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/ratelimit"
)

// authenticate resolves the user of r. On failure it writes the response
// (redirect to login, 401, 403 or 500) and returns ok=false. api selects JSON
// errors and disables the login redirect.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, api bool) (*identity.User, *http.Request, bool) {
	ri := info(r)
	u, err := s.deps.Auth.Authenticate(w, r)
	if err == nil && u != nil {
		ri.user = u
		return u, r.WithContext(identity.WithUser(r.Context(), u)), true
	}
	if err == nil {
		err = auth.ErrUnauthenticated
	}
	switch {
	case errors.Is(err, auth.ErrForbidden):
		s.failAPI(w, r, errForbidden("You are signed in but not allowed to access Backyard."), api)
	case errors.Is(err, auth.ErrUnauthenticated):
		if !api && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			if login := s.deps.Auth.LoginURL(ri.originalURL()); login != "" {
				w.Header().Set("Cache-Control", "no-store")
				http.Redirect(w, r, login, http.StatusFound)
				return nil, r, false
			}
		}
		returnTo := ri.originalURL()
		if api {
			returnTo = s.apiReturnTo(r)
		}
		e := newError(http.StatusUnauthorized, codeUnauthenticated, "Authentication required.")
		e.LoginURL = s.deps.Auth.LoginURL(returnTo)
		s.failAPI(w, r, e, api)
	default:
		e := errInternal(err, "authentication failed")
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			e = errContext(err, "authentication")
		}
		s.failAPI(w, r, e, api)
	}
	return nil, r, false
}

// apiReturnTo is where a browser should come back to after logging in on
// behalf of an API call: the calling page (Referer) when it is on the same
// origin, else the site root.
func (s *Server) apiReturnTo(r *http.Request) string {
	ri := info(r)
	if ref := r.Header.Get("Referer"); ref != "" && parseOrigin(ref) == ri.origin {
		return ref
	}
	return ri.siteRoot()
}

// checkCSRF enforces SPEC §5: non-GET/HEAD requests need "X-Backyard: 1" and,
// when an Origin header is present, the request's own origin (path mode:
// the hub origin).
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return true
	}
	if r.Header.Get("X-Backyard") != "1" {
		s.writeAPIError(w, r, errForbidden(`missing "X-Backyard: 1" header`))
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		ri := info(r)
		po := parseOrigin(o)
		if po == "" || (po != ri.origin && !(s.pathMode && po == s.hubOrigin)) {
			s.writeAPIError(w, r, errForbidden("cross-origin request rejected"))
			return false
		}
	}
	return true
}

// rateKey is the rate-limit key of the current user.
func rateKey(r *http.Request) string {
	ri := info(r)
	if ri.user == nil {
		return ratelimit.Key("", true, ri.clientIP)
	}
	return ratelimit.Key(ri.user.ID, ri.user.Anonymous, ri.clientIP)
}

// allow consumes one request of cat; on rejection it writes 429.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, cat ratelimit.Category) bool {
	ok, wait := s.limiter.Allow(cat, rateKey(r))
	if ok {
		return true
	}
	s.metrics.rateLimited.WithLabelValues(string(cat)).Inc()
	e := newError(http.StatusTooManyRequests, codeRateLimited, "rate limit exceeded (%s); retry later", cat)
	e.RetryAfter = wait
	s.writeAPIError(w, r, e)
	return false
}

// apiGate authenticates an API request, enforces CSRF and the general API
// rate limit and records the user in the user cache.
func (s *Server) apiGate(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	u, r, ok := s.authenticate(w, r, true)
	if !ok {
		return r, false
	}
	if !s.checkCSRF(w, r) || !s.allow(w, r, ratelimit.API) {
		return r, false
	}
	s.touchUser(r.Context(), u)
	return r, true
}

// touchThrottle limits how often this instance asks the user cache to record
// a user (the cache throttles writes itself; this avoids a goroutine per
// request).
type touchThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

const (
	touchEvery    = time.Minute
	touchMaxUsers = 100_000
)

func newTouchThrottle() *touchThrottle { return &touchThrottle{last: make(map[string]time.Time)} }

func (t *touchThrottle) due(id string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if last, ok := t.last[id]; ok && now.Sub(last) < touchEvery {
		return false
	}
	if len(t.last) >= touchMaxUsers {
		for k, v := range t.last {
			if now.Sub(v) >= touchEvery {
				delete(t.last, k)
			}
		}
		if len(t.last) >= touchMaxUsers {
			clear(t.last)
		}
	}
	t.last[id] = now
	return true
}

// forget clears the throttle state of id so that its next request touches
// the user cache again.
func (t *touchThrottle) forget(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, id)
}

// touchUser records u in the user cache in the background.
func (s *Server) touchUser(ctx context.Context, u *identity.User) {
	if s.deps.Users == nil || u == nil || u.Anonymous || u.Email == "" ||
		strings.HasPrefix(u.ID, "anon:") || strings.HasPrefix(u.ID, "token:") {
		return
	}
	if !s.touches.due(u.ID, time.Now()) {
		return
	}
	uc := u.Clone()
	bctx := context.WithoutCancel(ctx)
	if !s.bg.run(maxBackgroundTouches, func() { s.deps.Users.Touch(bctx, uc) }) {
		// Too many writes in flight: let a later request record the user.
		s.touches.forget(u.ID)
	}
}

// requireMethods writes 405 unless r.Method is one of allowed (GET implies
// HEAD).
func (s *Server) requireMethods(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m || (m == http.MethodGet && r.Method == http.MethodHead) {
			return true
		}
	}
	all := allowed
	for _, m := range allowed {
		if m == http.MethodGet {
			all = append(append([]string(nil), allowed...), http.MethodHead)
			break
		}
	}
	w.Header().Set("Allow", strings.Join(all, ", "))
	s.fail(w, r, methodNotAllowed(all...))
	return false
}
