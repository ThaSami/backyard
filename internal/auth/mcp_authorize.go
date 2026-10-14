package auth

import (
	"crypto/hmac"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thasami/backyard/internal/identity"
)

// The authorize endpoint (SPEC §8.4.5): GET validates the request, signs the
// browser in through the auth mode and renders the consent page; the
// consent form POSTs back here, and only an explicit Allow creates a grant
// and a code. Errors before the redirect URI is validated render a page and
// never redirect.

var challengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// authorizeParams are the parameters that must not be repeated.
var authorizeParams = []string{
	"response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method",
	"resource", "scope", "state", "response_mode",
}

// authzRequest is a validated authorization request.
type authzRequest struct {
	client      *mcpClient
	redirectURI string
	state       string // "" when absent
	challenge   string
	scope       string // the granted scope string
}

// fields returns the hidden fields of the consent form (and the GET form of
// the request) without csrf.
func (q *authzRequest) fields(resource string) []formField {
	f := []formField{
		{"response_type", "code"},
		{"client_id", q.client.id},
		{"redirect_uri", q.redirectURI},
	}
	if q.state != "" {
		f = append(f, formField{"state", q.state})
	}
	return append(f,
		formField{"code_challenge", q.challenge},
		formField{"code_challenge_method", "S256"},
		formField{"scope", q.scope},
		formField{"resource", resource},
	)
}

// serveAuthorize serves AuthorizePath (GET, POST). a.mcp is nil when the
// connector is disabled.
func (a *authenticator) serveAuthorize(w http.ResponseWriter, r *http.Request) {
	m := a.mcp
	if m == nil {
		renderError(w, http.StatusNotFound, "Claude connector not enabled", "The Claude connector is not enabled on this server.", "", "")
		return
	}
	if !m.onCanonicalHost(r) {
		renderError(w, http.StatusNotFound, "Wrong address", "Use "+m.resource+" to connect an app to this Backyard.", "", "")
		return
	}
	switch r.Method {
	case http.MethodGet:
		m.authorizeGET(w, r)
	case http.MethodPost:
		m.authorizePOST(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		renderError(w, http.StatusMethodNotAllowed, "Method not allowed", "Use GET or POST.", "", "")
	}
}

// badAuthorizeRequest renders the 400 page of a malformed request.
func badAuthorizeRequest(w http.ResponseWriter, detail string) {
	renderPage(w, http.StatusBadRequest, pageData{
		Title: "Invalid authorization request", Message: "This sign-in link is malformed. Go back to the app and connect again.",
		Detail: detail,
	})
}

// authorizeGET implements GET AuthorizePath (SPEC §8.4.5).
func (m *MCP) authorizeGET(w http.ResponseWriter, r *http.Request) {
	uri := r.URL.RequestURI() // path + raw query, also for absolute-form request lines
	if len(uri) > maxAuthorizeURI {
		badAuthorizeRequest(w, "The request is longer than 2048 bytes.")
		return
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		badAuthorizeRequest(w, "The query string cannot be parsed.")
		return
	}
	req, ok := m.parseAuthorize(w, r, q, http.StatusFound, false)
	if !ok {
		return
	}
	u, ok := m.consentUser(w, r, m.issuer+uri)
	if !ok {
		return
	}
	m.renderConsent(w, r, u, req)
}

// authorizePOST implements the consent decision (SPEC §8.4.5).
func (m *MCP) authorizePOST(w http.ResponseWriter, r *http.Request) {
	form, ok := readConsentForm(w, r)
	if !ok {
		badAuthorizeRequest(w, "The approval form could not be read.")
		return
	}
	if !m.a.consentSignalsOK(r) {
		m.audit(r.Context(), r, slog.LevelWarn, "mcp.consent_rejected", "unknown", nil, false,
			"reason", "browser signals", "sec_fetch_site", truncate(r.Header.Get("Sec-Fetch-Site"), 32))
		renderPage(w, http.StatusForbidden, pageData{
			Title:   "This approval was not sent from the Backyard page",
			Message: "Backyard only accepts an approval submitted from its own consent page. Go back to the app and connect again.",
		})
		return
	}
	req, ok := m.parseAuthorize(w, r, form, http.StatusSeeOther, true)
	if !ok {
		return
	}
	resource := form.Get("resource")
	// Signed out meanwhile (oidc): come back to the GET form of the
	// request, which asks for consent again once signed in.
	back := m.issuer + AuthorizePath + "?" + encodeFields(req.fields(resource))
	u, ok := m.consentUser(w, r, back)
	if !ok {
		return
	}
	pseudo := req.auditRecord()
	if !m.verifyConsentCSRF(r, u, req, form.Get("csrf")) {
		m.audit(r.Context(), r, slog.LevelWarn, "mcp.consent_rejected", u.ID, pseudo, false, "reason", "csrf")
		renderPage(w, http.StatusForbidden, pageData{
			Title:   "Approval expired",
			Message: "This approval page expired or was not issued to you. Go back to the app and connect again.",
		})
		return
	}
	switch form.Get("decision") {
	case "deny":
		m.audit(r.Context(), r, slog.LevelInfo, "mcp.deny", u.ID, pseudo, false)
		m.redirectError(w, req.redirectURI, req.state, "access_denied", "the user did not allow the connection", http.StatusSeeOther)
	case "allow":
		m.allow(w, r, u, req)
	default:
		badAuthorizeRequest(w, "The approval form has no valid decision.")
	}
}

// allow creates the grant and redirects with the code.
func (m *MCP) allow(w http.ResponseWriter, r *http.Request, u *identity.User, req *authzRequest) {
	now := m.now().Unix()
	g := &grantRecord{
		ID: newGrantID(),
		User: grantUser{
			ID: u.ID, Email: u.Email, Name: u.Name,
			Groups: append([]string(nil), u.Groups...), Anonymous: u.Anonymous,
		},
		ClientID: req.client.id, ClientName: req.client.name, ClientKind: req.client.kind,
		RedirectURI: req.redirectURI, CodeChallenge: req.challenge, Scope: req.scope, Resource: m.resource,
		CreatedAt: now, CodeExp: now + int64(mcpCodeTTL/time.Second),
	}
	g.Exp = g.CodeExp + int64(mcpPendingRetention/time.Second)
	if err := m.createGrant(r.Context(), g); err != nil {
		m.log.Error("auth: mcp grant could not be stored", "user", u.ID, "error", err)
		renderError(w, http.StatusServiceUnavailable, "Connection unavailable",
			"Backyard could not save this connection right now. Please try again in a moment.", "", "")
		return
	}
	code := m.signCode(g.ID, g.CodeExp)
	m.audit(r.Context(), r, slog.LevelInfo, "mcp.authorize", u.ID, g, true, "scope", g.Scope)
	m.redirectWith(w, req.redirectURI, http.StatusSeeOther, "code", code, "state", req.state)
}

// auditRecord is a grant-shaped view of a request for audit fields.
func (q *authzRequest) auditRecord() *grantRecord {
	return &grantRecord{ClientID: q.client.id, ClientName: q.client.name, ClientKind: q.client.kind, RedirectURI: q.redirectURI}
}

// readConsentForm reads the urlencoded, UTF-8 consent form (≤ 16 KiB).
func readConsentForm(w http.ResponseWriter, r *http.Request) (url.Values, bool) {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxOAuthBody))
	if err != nil || !utf8.Valid(body) {
		return nil, false
	}
	form, err := url.ParseQuery(string(body))
	return form, err == nil
}

// consentSignalsOK applies the browser's own signals to a consent POST
// (defence in depth; the CSRF token is the actual check).
func (a *authenticator) consentSignalsOK(r *http.Request) bool {
	h := r.Header
	if v := strings.ToLower(strings.TrimSpace(h.Get("Sec-Fetch-Site"))); v != "" && v != "same-origin" {
		return false
	}
	if v := strings.ToLower(strings.TrimSpace(h.Get("Sec-Fetch-Mode"))); v != "" && v != "navigate" {
		return false
	}
	if v := strings.ToLower(strings.TrimSpace(h.Get("Sec-Fetch-Dest"))); v != "" && v != "document" {
		return false
	}
	switch o := strings.TrimSpace(h.Get("Origin")); o {
	case "", "null":
		return true
	default:
		return a.isOwnOrigin(o, r)
	}
}

// parseAuthorize validates an authorization request (SPEC §8.4.5 steps 2–5) from
// the query (GET) or the consent form (POST). On failure it writes the
// error page, or the error redirect with status redirect, and returns false.
func (m *MCP) parseAuthorize(w http.ResponseWriter, r *http.Request, q url.Values, redirect int, post bool) (*authzRequest, bool) {
	for _, k := range authorizeParams {
		if len(q[k]) > 1 {
			badAuthorizeRequest(w, "The parameter "+k+" appears more than once.")
			return nil, false
		}
	}
	client, cerr := m.resolveClient(r.Context(), q.Get("client_id"))
	if cerr != nil {
		renderPage(w, cerr.status, pageData{Title: cerr.title, Message: cerr.msg, Detail: cerr.detail})
		return nil, false
	}
	redirectURI := q.Get("redirect_uri")
	if !m.clientRedirect(client, redirectURI) {
		renderPage(w, http.StatusBadRequest, pageData{
			Title:   "Redirect address not allowed",
			Message: "The app asked Backyard to send the approval to an address that is not registered for it or not allowed on this Backyard.",
			Detail:  "Allowed are the addresses in mcp.redirect_uris and http loopback addresses on this computer.",
		})
		return nil, false
	}
	// From here on, errors go back to the app.
	state := q.Get("state")
	stateOK := len(state) <= maxOAuthState
	fail := func(code, desc string) (*authzRequest, bool) {
		s := state
		if !stateOK {
			s = ""
		}
		m.redirectError(w, redirectURI, s, code, desc, redirect)
		return nil, false
	}
	if rt := q.Get("response_type"); rt != "code" {
		return fail("unsupported_response_type", "response_type must be code")
	}
	if _, ok := q["request"]; ok {
		return fail("request_not_supported", "request objects are not supported")
	}
	if _, ok := q["request_uri"]; ok {
		return fail("request_uri_not_supported", "request_uri is not supported")
	}
	if rm, ok := q["response_mode"]; ok && rm[0] != "query" {
		return fail("invalid_request", "response_mode must be query")
	}
	if q.Get("code_challenge_method") != "S256" {
		return fail("invalid_request", "PKCE with S256 is required")
	}
	challenge := q.Get("code_challenge")
	if !challengeRe.MatchString(challenge) {
		return fail("invalid_request", "code_challenge must be a base64url SHA-256 value (43 characters)")
	}
	if res, ok := q["resource"]; ok {
		if !m.resourceOK(res[0]) {
			return fail("invalid_target", "resource must be "+m.resource)
		}
	} else if post {
		return fail("invalid_request", "resource is required")
	}
	if !stateOK {
		return fail("invalid_request", "state is limited to 512 bytes")
	}
	scope := scopeMCP
	if containsString(strings.Split(q.Get("scope"), " "), scopeOffline) {
		scope += " " + scopeOffline
	}
	return &authzRequest{client: client, redirectURI: redirectURI, state: state, challenge: challenge, scope: scope}, true
}

// consentUser authenticates the browser at the authorize endpoint (SPEC §8.4.5 step
// 6) with the mode's own credentials, never a bearer token. back is where
// an oidc login returns to. On failure it writes the response.
func (m *MCP) consentUser(w http.ResponseWriter, r *http.Request, back string) (*identity.User, bool) {
	a := m.a
	u, err := a.authenticateBrowser(w, r)
	if err != nil {
		switch {
		case !errors.Is(err, ErrForbidden) && !errors.Is(err, ErrUnauthenticated):
			a.log.Error("auth: authorize could not authenticate the browser", "error", err)
			renderError(w, http.StatusServiceUnavailable, "Sign-in unavailable",
				"Backyard could not verify your identity right now. Please try again in a moment.", "", "")
		case errors.Is(err, ErrForbidden):
			renderPage(w, http.StatusForbidden, pageData{
				Title: "Access denied", Message: "Your account is not allowed to use Backyard.",
				LinkURL: a.logoutURL(), LinkText: "Sign out",
			})
		case errors.Is(err, ErrCookieConflict):
			renderError(w, http.StatusUnauthorized, "Conflicting sign-in cookies",
				"Your browser sent Backyard sign-in cookies of more than one account, so Backyard cannot tell who you are. "+
					"Cookies that Backyard did not set have been removed where possible: reload this page. "+
					"If this keeps happening, clear this browser's cookies for this domain.", "", "")
		case a.oidc != nil:
			noStore(w)
			http.Redirect(w, r, a.LoginURL(back), http.StatusFound)
		default:
			renderError(w, http.StatusUnauthorized, "Sign-in required",
				"The identity proxy did not authenticate this request; "+AuthorizePath+" must stay behind it.", "", "")
		}
		return nil, false
	}
	if !canConnectApps(a.mode, u) {
		renderError(w, http.StatusForbidden, "This sign-in cannot connect apps",
			"Anonymous and static-token identities cannot connect apps to Backyard. Sign in with your own account.", "", "")
		return nil, false
	}
	return u, true
}

// canConnectApps reports whether u may consent: never a static-token
// principal, and a guest only in mode none.
func canConnectApps(mode string, u *identity.User) bool {
	return !strings.HasPrefix(u.ID, tokenPrefix) && (mode == ModeNone || (!u.Anonymous && !strings.HasPrefix(u.ID, anonPrefix)))
}

// consentMAC authenticates a consent form for user, browser session,
// request and expiry (SPEC §8.4.5).
func (m *MCP) consentMAC(u *identity.User, binding string, req *authzRequest, exp int64) []byte {
	return formMAC(m.consentKey, []any{"mcp-consent", exp, u.ID, binding, req.client.id, req.redirectURI,
		req.state, req.challenge, req.scope, m.resource})
}

// verifyConsentCSRF checks the consent form's CSRF token (expired tokens are
// accepted for 60 s of clock skew).
func (m *MCP) verifyConsentCSRF(r *http.Request, u *identity.User, req *authzRequest, token string) bool {
	exp, mac, ok := splitFormToken(token)
	if !ok {
		return false
	}
	now := m.now().Unix()
	if exp < now-int64(mcpConsentSkew/time.Second) || exp > now+int64((mcpConsentTTL+mcpConsentSkew)/time.Second) {
		return false
	}
	return hmac.Equal(mac, m.consentMAC(u, m.a.browserSessionBinding(r), req, exp))
}

// renderConsent renders the consent page (SPEC §8.4.5). It is shown on every
// authorization, whatever Sec-Fetch-Site says.
func (m *MCP) renderConsent(w http.ResponseWriter, r *http.Request, u *identity.User, req *authzRequest) {
	target, _ := url.Parse(req.redirectURI)
	_, loopback := parseLoopbackURI(req.redirectURI)
	exp := m.now().Add(mcpConsentTTL).Unix()
	form := &consentForm{
		Who:        consentWho(u),
		Guest:      m.a.mode == ModeNone && u.Anonymous,
		SignOutURL: m.a.logoutURL(),
		ClientName: req.client.name,
		Verified:   req.client.kind == clientKindCIMD,
		ClientHost: req.client.host,
		Origin:     strings.ToLower(target.Scheme + "://" + target.Host),
		Loopback:   loopback,
		Host:       m.host,
		Until:      m.now().Add(m.family).UTC().Format("2006-01-02"),
		Fields: append(req.fields(m.resource),
			formField{"csrf", formToken(exp, m.consentMAC(u, m.a.browserSessionBinding(r), req, exp))}),
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	renderPage(w, http.StatusOK, pageData{Title: "Connect an app to Backyard", Consent: form, formAction: formActionSource(target)})
}

// consentWho names the signed-in user: email, else name, else id.
func consentWho(u *identity.User) string {
	switch {
	case u.Anonymous && u.Name != "":
		return u.Name
	case u.Email != "":
		return u.Email
	case u.Name != "":
		return u.Name
	}
	return u.ID
}

// redirectError sends an authorization error back to the client.
func (m *MCP) redirectError(w http.ResponseWriter, redirectURI, state, code, desc string, status int) {
	m.redirectWith(w, redirectURI, status, "error", code, "error_description", desc, "state", state)
}

// redirectWith redirects to redirectURI with the given parameters (pairs;
// empty values are skipped) and iss appended to its query.
func (m *MCP) redirectWith(w http.ResponseWriter, redirectURI string, status int, kv ...string) {
	var b strings.Builder
	b.WriteString(redirectURI)
	sep := "?"
	if i := strings.IndexByte(redirectURI, '?'); i >= 0 {
		sep = "&"
		if strings.HasSuffix(redirectURI, "?") || strings.HasSuffix(redirectURI, "&") {
			sep = ""
		}
	}
	add := func(k, v string) {
		b.WriteString(sep)
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(v))
		sep = "&"
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			add(kv[i], kv[i+1])
		}
	}
	add("iss", m.issuer)
	h := w.Header()
	noStore(w)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Location", b.String())
	w.WriteHeader(status)
}

// encodeFields renders form fields as a query string, in order.
func encodeFields(fields []formField) string {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(f.Name))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(f.Value))
	}
	return b.String()
}
