package auth

import (
	"net/http"
	"strconv"
	"time"

	"github.com/thasami/backyard/internal/ratelimit"
)

// The rate limit of the connector's sign-in endpoints, authorize and token
// (SPEC §11): a request that identifies a person — a signed-in browser at
// authorize, a code or refresh token naming a grant at the token endpoint —
// is charged to that person (ratelimit.Key: per client IP for guests);
// every other request is charged to its client IP once it is answered, and
// answered 429 instead when that budget is spent — except a valid request of
// a signed-out browser in oidc mode, whose way to the login is never
// charged. Requests that identify no one (junk from one address behind a
// shared proxy) so never exhaust the budget of the signed-in people who
// connect from the same address, nor block a browser on its way to the
// login; guests (mode none) are keyed by that address and share its budget.

// limitWriter applies the limit to one request.
type limitWriter struct {
	http.ResponseWriter
	m       *MCP
	r       *http.Request
	tooMany func(w http.ResponseWriter) // writes the endpoint's 429
	charged bool                        // a budget was charged
	dropped bool                        // the answer was replaced by the 429
}

// limited wraps w for a request of the authorize or token endpoint.
func (m *MCP) limited(w http.ResponseWriter, r *http.Request, tooMany func(http.ResponseWriter)) *limitWriter {
	return &limitWriter{ResponseWriter: w, m: m, r: r, tooMany: tooMany}
}

// chargeUser charges the person behind a request through w's limiter (true
// when w has none). On refusal it has answered 429 and returns false.
func chargeUser(w http.ResponseWriter, userID string, anonymous bool) bool {
	l, ok := w.(*limitWriter)
	if !ok || l.charged {
		return true
	}
	l.charged = true
	allowed, wait := l.m.limit(ratelimit.Key(userID, anonymous, l.m.a.clientIP(l.r)))
	if !allowed {
		l.reject(wait)
	}
	return allowed
}

// exemptLimit lets a valid authorization request of a signed-out browser in
// oidc mode through uncharged: its answer (the way to the login) is cheap,
// identifies no one and only leads to the IdP, so junk from the same
// address never keeps people from signing in.
func exemptLimit(w http.ResponseWriter) {
	if l, ok := w.(*limitWriter); ok {
		l.charged = true
	}
}

// WriteHeader charges the client IP for an answer to a request that
// identified no one, and answers 429 instead once that budget is spent.
func (l *limitWriter) WriteHeader(status int) {
	if l.dropped {
		return
	}
	if !l.charged {
		l.charged = true
		if ok, wait := l.m.limit(ratelimit.Key("", true, l.m.a.clientIP(l.r))); !ok {
			l.reject(wait)
			return
		}
	}
	l.ResponseWriter.WriteHeader(status)
}

// Write sends body bytes (none of an answer replaced by the 429).
func (l *limitWriter) Write(b []byte) (int, error) {
	if !l.charged {
		l.WriteHeader(http.StatusOK)
	}
	if l.dropped {
		return len(b), nil
	}
	return l.ResponseWriter.Write(b)
}

// Unwrap returns the underlying writer (http.ResponseController).
func (l *limitWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// reject replaces the answer by the endpoint's 429.
func (l *limitWriter) reject(wait time.Duration) {
	l.dropped = true
	h := l.ResponseWriter.Header()
	for _, k := range []string{"Location", "WWW-Authenticate", "Set-Cookie"} {
		h.Del(k)
	}
	h.Set("Retry-After", strconv.Itoa(ratelimit.RetryAfterSeconds(wait)))
	l.tooMany(l.ResponseWriter)
}

// oauthTooMany is the token endpoint's 429 (RFC 6749 §5.2 format).
func oauthTooMany(w http.ResponseWriter) {
	writeOAuthError(w, http.StatusTooManyRequests, "invalid_request", "too many requests")
}

// authorizeTooMany is the authorize endpoint's 429 page.
func authorizeTooMany(w http.ResponseWriter) {
	renderPage(w, http.StatusTooManyRequests, pageData{
		Title:   "Too many requests",
		Message: "Too many sign-in requests. Wait a minute, then go back to the app and connect again.",
	})
}

// allowAll is the limit without Options.MCPAllow.
func allowAll(string) (bool, time.Duration) { return true, 0 }
