package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxStateBytes bounds the CLI login state parameter.
const maxStateBytes = 1024

// Handler implements Authenticator. It serves /__backyard/auth/cli in every
// mode and, in oidc mode, /login, /callback (or the path of
// auth.oidc.redirect_url) and /logout. Everything else is 404.
func (a *authenticator) Handler() http.Handler {
	return http.HandlerFunc(a.serveAuth)
}

func (a *authenticator) serveAuth(w http.ResponseWriter, r *http.Request) {
	if a.bearerOnly {
		http.NotFound(w, r)
		return
	}
	p := r.URL.Path
	switch {
	case p == CLIPath:
		// GET only: minting a token on HEAD would be a pointless side effect.
		if !allowMethods(w, r, http.MethodGet) {
			return
		}
		a.serveCLI(w, r)
	case a.oidc != nil && p == LoginPath:
		if !allowMethods(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		a.oidc.login(w, r)
	case a.oidc != nil && (p == CallbackPath || p == a.oidc.callbackPath):
		if !allowMethods(w, r, http.MethodGet) {
			return
		}
		a.oidc.callback(w, r)
	case a.oidc != nil && p == LogoutPath:
		if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		a.oidc.logout(w, r)
	default:
		http.NotFound(w, r)
	}
}

// allowMethods writes 405 and returns false unless r.Method is allowed.
func allowMethods(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
	return false
}

// serveCLI implements the browser → CLI token handoff (SPEC §8.3):
//
//	/__backyard/auth/cli?redirect_uri=http://127.0.0.1:PORT/callback&state=…
//	→ 302 redirect_uri?token=…&expires_at=…&state=…
//	/__backyard/auth/cli?mode=display → HTML page showing the token.
//
// The browser must be authenticated by the mode's own mechanism (session
// cookie or proxy identity); bearer tokens are deliberately not accepted so
// a token cannot be used to renew itself.
func (a *authenticator) serveCLI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	display := q.Get("mode") == "display"
	state := q.Get("state")
	if len(state) > maxStateBytes {
		renderError(w, http.StatusBadRequest, "Invalid CLI login request", "The state parameter is too long.", "", "")
		return
	}
	var target *url.URL
	if !display {
		var err error
		target, err = validateLoopbackRedirect(q.Get("redirect_uri"))
		if err != nil {
			renderError(w, http.StatusBadRequest, "Invalid CLI login request",
				"This link was not created by the Backyard CLI: "+err.Error()+".", "", "")
			return
		}
		if state == "" {
			renderError(w, http.StatusBadRequest, "Invalid CLI login request", "The state parameter is missing.", "", "")
			return
		}
	}

	u, err := a.authenticateBrowser(w, r)
	if err != nil {
		switch {
		case !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrUnauthenticated):
			// An infrastructure failure: signing in again would not help (and
			// in oidc mode would loop through the IdP).
			a.log.Error("auth: CLI login could not authenticate the browser", "error", err)
			renderError(w, http.StatusServiceUnavailable, "Sign-in unavailable",
				"Backyard could not verify your identity right now. Please try again in a moment.", "", "")
		case errors.Is(err, ErrForbidden):
			renderPage(w, http.StatusForbidden, pageData{
				Title: "Access denied", Message: "Your account is not allowed to use Backyard.",
				LinkURL: a.logoutURL(), LinkText: "Sign out",
			})
		case a.oidc != nil:
			back := a.hub.String() + r.URL.RequestURI()
			noStore(w)
			http.Redirect(w, r, a.LoginURL(back), http.StatusFound)
		default:
			renderError(w, http.StatusUnauthorized, "Not signed in",
				"Your request was not authenticated by the identity-aware proxy in front of Backyard.", "", "")
		}
		return
	}
	if a.mode != ModeNone && (u.Anonymous || strings.HasPrefix(u.ID, tokenPrefix)) {
		renderError(w, http.StatusForbidden, "Cannot create a CLI token",
			"Anonymous and static-token identities cannot mint personal API tokens.", "", "")
		return
	}
	// Groups that grant admin rights or pass allowed_groups must survive if
	// the token has to drop some to stay within its size limit.
	minted := u.Clone()
	minted.Groups = prioritize(u.Groups, a.isImportantGroup)
	tok, exp, err := a.tokens.Issue(minted, a.cfg.Auth.TokenTTL.D())
	if err != nil {
		a.log.Error("auth: issue CLI token", "user", u.ID, "error", err)
		renderError(w, http.StatusInternalServerError, "Cannot create a CLI token", "The token could not be issued.", "", "")
		return
	}
	a.log.Info("auth: issued CLI token", "user", u.ID, "expires_at", exp.Format(time.RFC3339), "display", display, "audit", true)

	if display {
		cmd := "backyard login --token <token>"
		if a.hub != nil {
			cmd = "backyard login --server " + a.hub.String() + " --token " + tok
		}
		renderPage(w, http.StatusOK, pageData{
			Title:     "Your Backyard CLI token",
			Message:   "Signed in as " + displayID(u) + ".",
			Token:     tok,
			ExpiresAt: exp.UTC().Format("2006-01-02 15:04 MST"),
			Command:   cmd,
		})
		return
	}
	vals := target.Query()
	vals.Set("token", tok)
	vals.Set("expires_at", exp.UTC().Format(time.RFC3339))
	vals.Set("state", state)
	target.RawQuery = vals.Encode()
	noStore(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target.String(), http.StatusFound)
}
