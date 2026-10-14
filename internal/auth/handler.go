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

// Handler implements Authenticator. It serves /__backyard/auth/cli and
// /login in every mode and, in oidc mode, /callback (or the path of
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
		// GET, or POST from the confirmation page. Not HEAD: minting a token
		// on HEAD would be a pointless side effect.
		if !allowMethods(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		a.serveCLI(w, r)
	case p == LoginPath:
		if !allowMethods(w, r, http.MethodGet, http.MethodHead) {
			return
		}
		if a.oidc != nil {
			a.oidc.login(w, r)
			return
		}
		a.serveProxyLogin(w, r)
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

// serveProxyLogin answers /login when sign-in is not handled by Backyard
// (modes none, iap, alb and header), as the Cloudflare edition does: the
// identity-aware proxy in front of Backyard authenticates page loads itself
// (and intercepts the target when its session has expired), so signing in
// is just going to the target. rd follows the rules of the oidc login (no
// open redirects; the login endpoints themselves become the hub).
func (a *authenticator) serveProxyLogin(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	http.Redirect(w, r, a.sanitizeRedirect(r.URL.Query().Get("rd")), http.StatusFound)
}

// cliRequest is a validated CLI login request.
type cliRequest struct {
	redirectURI string   // raw redirect_uri ("" in display mode)
	target      *url.URL // parsed redirect_uri (nil in display mode)
	state       string
	display     bool
}

// query re-encodes the request as the query of a CLI login URL.
func (c cliRequest) query() url.Values {
	v := url.Values{}
	if c.display {
		v.Set("mode", "display")
	}
	if c.redirectURI != "" {
		v.Set("redirect_uri", c.redirectURI)
	}
	if c.state != "" {
		v.Set("state", c.state)
	}
	return v
}

// parseCLIRequest validates the parameters of a CLI login (from the query
// string, or from the confirmation form). On failure it renders a 400 page
// and returns false.
func parseCLIRequest(w http.ResponseWriter, q url.Values) (cliRequest, bool) {
	c := cliRequest{display: q.Get("mode") == "display", state: q.Get("state")}
	if len(c.state) > maxStateBytes {
		renderError(w, http.StatusBadRequest, "Invalid CLI login request", "The state parameter is too long.", "", "")
		return c, false
	}
	if c.display {
		return c, true
	}
	target, err := validateLoopbackRedirect(q.Get("redirect_uri"))
	if err != nil {
		renderError(w, http.StatusBadRequest, "Invalid CLI login request",
			"This link was not created by the Backyard CLI: "+err.Error()+".", "", "")
		return c, false
	}
	if c.state == "" {
		renderError(w, http.StatusBadRequest, "Invalid CLI login request", "The state parameter is missing.", "", "")
		return c, false
	}
	c.redirectURI, c.target = q.Get("redirect_uri"), target
	return c, true
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
//
// A request that another site may have started (see crossSiteRequest) does
// not mint anything: it gets a confirmation page whose form POSTs back here
// with a CSRF token bound to the user and the browser session. Otherwise any
// web page could navigate a signed-in browser here and have a token for its
// user delivered to a loopback port (or minted for display) — a local
// process listening there, not the CLI the user started, would receive it.
func (a *authenticator) serveCLI(w http.ResponseWriter, r *http.Request) {
	confirmed := r.Method == http.MethodPost
	q := r.URL.Query()
	if confirmed {
		r.Body = http.MaxBytesReader(w, r.Body, maxCLIFormBytes)
		if err := r.ParseForm(); err != nil {
			renderError(w, http.StatusBadRequest, "Invalid CLI login request", "The confirmation form could not be read.", "", "")
			return
		}
		q = r.PostForm
	}
	req, ok := parseCLIRequest(w, q)
	if !ok {
		return
	}
	if confirmed && !a.confirmationRequestOK(r) {
		renderError(w, http.StatusForbidden, "Cannot create a CLI token",
			"The confirmation was not submitted from Backyard's own page.", "", "")
		return
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
		case errors.Is(err, ErrCookieConflict):
			// Signing in again would not tell the accounts apart.
			renderError(w, http.StatusUnauthorized, "Conflicting sign-in cookies",
				"Your browser sent Backyard sign-in cookies of more than one account, so Backyard cannot tell who you are. "+
					"Cookies that Backyard did not set have been removed where possible: reload this page. "+
					"If this keeps happening, clear this browser's cookies for this domain.", "", "")
		case a.oidc != nil:
			back := a.hub.String() + r.URL.RequestURI()
			if confirmed {
				// Come back to the GET form of this request: the
				// confirmation is asked for again once signed in.
				back = a.hub.String() + CLIPath + "?" + req.query().Encode()
			}
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
	switch {
	case confirmed:
		if !a.verifyCLIConfirmation(r, u, req, q.Get(cliCSRFField)) {
			a.log.Warn("auth: CLI login confirmation rejected (expired, or not issued for this user and session)",
				"user", u.ID, "audit", true)
			renderError(w, http.StatusForbidden, "Confirmation expired",
				"This confirmation is no longer valid. Run backyard login again to get a new one.", "", "")
			return
		}
	case a.crossSiteRequest(r):
		a.log.Info("auth: CLI login needs confirmation (request may come from another site)",
			"user", u.ID, "sec_fetch_site", truncate(r.Header.Get("Sec-Fetch-Site"), 32))
		a.renderCLIConfirmation(w, r, u, req)
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
	a.log.Info("auth: issued CLI token", "user", u.ID, "expires_at", exp.Format(time.RFC3339), "display", req.display,
		"confirmed", confirmed, "audit", true)

	if req.display {
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
	target := *req.target
	vals := target.Query()
	vals.Set("token", tok)
	vals.Set("expires_at", exp.UTC().Format(time.RFC3339))
	vals.Set("state", req.state)
	target.RawQuery = vals.Encode()
	noStore(w)
	w.Header().Set("Referrer-Policy", "no-referrer")
	// A 303 turns the confirmation POST into a GET of the loopback URL.
	status := http.StatusFound
	if confirmed {
		status = http.StatusSeeOther
	}
	http.Redirect(w, r, target.String(), status)
}
