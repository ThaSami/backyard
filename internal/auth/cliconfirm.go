package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/identity"
)

const (
	// cliCSRFField is the confirmation form's CSRF token field.
	cliCSRFField = "csrf"
	// cliConfirmTTL is how long a CLI login confirmation page stays valid.
	cliConfirmTTL = 10 * time.Minute
	// maxCLIFormBytes bounds the confirmation form body (redirect_uri and
	// state are at most 4 KiB and 1 KiB).
	maxCLIFormBytes = 16 << 10
)

// cliConfirmForm feeds the confirmation form of pageTemplate.
type cliConfirmForm struct {
	Action      string
	RedirectURI string
	State       string
	Mode        string
	CSRF        string
	CancelURL   string
}

// crossSiteRequest reports whether a CLI login request may have been started
// by another site, so that it must be confirmed by the user. Per Fetch
// Metadata, only a same-origin request or a navigation the user started
// (Sec-Fetch-Site: none — e.g. the CLI opening the URL in the browser)
// proceeds; "same-site" counts as cross-site because Backyard sites are
// subdomains of the hub and their pages are not trusted. Browsers without
// Fetch Metadata are judged by the Referer: one from another origin than the
// hub (or the request's own) needs confirmation.
func (a *authenticator) crossSiteRequest(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "none", "same-origin":
		return false
	case "":
		ref := strings.TrimSpace(r.Header.Get("Referer"))
		return ref != "" && !a.isOwnOrigin(ref, r)
	default:
		return true
	}
}

// isOwnOrigin reports whether raw (a Referer or Origin) has the hub's origin
// or the origin of r itself (the hub may be reached under base_domain).
func (a *authenticator) isOwnOrigin(raw string, r *http.Request) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port := defaultPort(u.Port(), u.Scheme)
	if a.hub != nil && strings.EqualFold(u.Scheme, a.hub.Scheme) && host == a.hub.Hostname() &&
		port == defaultPort(a.hub.Port(), a.hub.Scheme) {
		return true
	}
	// The request's own host (with the port of the Referer's scheme when
	// the Host header has none), on the hub's scheme.
	if a.hub != nil && !strings.EqualFold(u.Scheme, a.hub.Scheme) {
		return false
	}
	reqHost, reqPort := r.Host, ""
	if h, p, err := net.SplitHostPort(reqHost); err == nil {
		reqHost, reqPort = h, p
	}
	reqHost = strings.ToLower(strings.TrimSuffix(strings.Trim(reqHost, "[]"), "."))
	return reqHost != "" && host == reqHost && port == defaultPort(reqPort, u.Scheme)
}

// confirmationRequestOK applies the browser's own signals to a POSTed
// confirmation (defense in depth; the CSRF token is the actual check): when
// present, Sec-Fetch-Site must be same-origin and Origin must be the hub
// (or "null", which the confirmation page's no-referrer policy produces).
func (a *authenticator) confirmationRequestOK(r *http.Request) bool {
	if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs != "" && sfs != "same-origin" {
		return false
	}
	switch o := strings.TrimSpace(r.Header.Get("Origin")); o {
	case "", "null":
		return true
	default:
		return a.isOwnOrigin(o, r)
	}
}

// browserSessionBinding returns what ties a confirmation to the browser
// session: in oidc mode the session cookie that authenticates the request
// (a new sign-in invalidates older confirmations). Other modes have no
// Backyard session — the proxy (or the guest cookie, which is the user id in
// mode none) identifies the browser on every request — so the user id alone
// binds the confirmation.
func (a *authenticator) browserSessionBinding(r *http.Request) string {
	if a.mode != ModeOIDC || a.sess == nil {
		return ""
	}
	// The same cookie authenticateSession reads (cookies of different users
	// fail closed there and bind nothing here).
	_, value, err := a.sessionFromCookies(r)
	if err != nil {
		return ""
	}
	return value
}

// cliConfirmMAC authenticates a confirmation for user, session binding,
// request and expiry.
func (a *authenticator) cliConfirmMAC(u *identity.User, binding string, req cliRequest, exp int64) []byte {
	mode := ""
	if req.display {
		mode = "display"
	}
	// A JSON array encodes the fields unambiguously.
	msg, _ := json.Marshal([]string{"cli-confirm", strconv.FormatInt(exp, 10), u.ID, binding, req.redirectURI, req.state, mode})
	m := hmac.New(sha256.New, a.confirmKey)
	m.Write(msg)
	return m.Sum(nil)
}

// cliCSRFToken returns the CSRF token of a confirmation form:
// "<unix expiry>.<base64url HMAC>".
func (a *authenticator) cliCSRFToken(u *identity.User, binding string, req cliRequest) string {
	exp := a.now().Add(cliConfirmTTL).Unix()
	return strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(a.cliConfirmMAC(u, binding, req, exp))
}

// verifyCLIConfirmation checks a posted CSRF token against the current user,
// browser session and request.
func (a *authenticator) verifyCLIConfirmation(r *http.Request, u *identity.User, req cliRequest, token string) bool {
	expStr, macStr, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || len(token) > 128 {
		return false
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return false
	}
	now := a.now().Unix()
	if exp <= now || exp > now+int64(cliConfirmTTL/time.Second)+60 {
		return false
	}
	mac, err := base64.RawURLEncoding.DecodeString(macStr)
	if err != nil {
		return false
	}
	return hmac.Equal(mac, a.cliConfirmMAC(u, a.browserSessionBinding(r), req, exp))
}

// renderCLIConfirmation asks the user whether the CLI may sign in as them.
func (a *authenticator) renderCLIConfirmation(w http.ResponseWriter, r *http.Request, u *identity.User, req cliRequest) {
	who := displayID(u)
	until := a.now().Add(a.cfg.Auth.TokenTTL.D()).UTC().Format("2006-01-02 15:04 MST")
	form := &cliConfirmForm{
		Action:      CLIPath,
		RedirectURI: req.redirectURI,
		State:       req.state,
		CSRF:        a.cliCSRFToken(u, a.browserSessionBinding(r), req),
		CancelURL:   a.hubURL(),
	}
	d := pageData{Confirm: form}
	if req.display {
		form.Mode = "display"
		d.Title = "Create a Backyard CLI token for " + who + "?"
		d.Message = "A page asked Backyard to create a personal API token for your account and show it on the next page."
		d.Detail = "Only continue if you are signing in the Backyard CLI yourself. The token can act as you until " + until + "."
	} else {
		d.Title = "Allow the Backyard CLI on this computer to sign in as " + who + "?"
		d.Message = "A page asked Backyard to hand a personal API token for your account to " + loopbackOrigin(req.target) +
			" on this computer."
		d.Detail = "Only allow this if you just ran backyard login in a terminal on this computer. " +
			"The token can act as you until " + until + "."
		d.formAction = formActionSource(req.target)
	}
	renderPage(w, http.StatusOK, d)
}

// loopbackOrigin renders the scheme, host and port of a loopback redirect.
func loopbackOrigin(u *url.URL) string {
	if u == nil {
		return "the CLI"
	}
	return u.Scheme + "://" + u.Host
}

// formActionSource is the CSP form-action source that lets the confirmation
// form's response redirect to the CLI's loopback address (browsers apply
// form-action to the redirects that follow a form submission). CSP cannot
// express IPv6 hosts, so [::1] needs the scheme source "http:".
func formActionSource(u *url.URL) string {
	if u == nil {
		return ""
	}
	if strings.Contains(u.Hostname(), ":") {
		return "http:"
	}
	return "http://" + strings.ToLower(u.Host)
}
