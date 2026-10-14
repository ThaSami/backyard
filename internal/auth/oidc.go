package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

// exchangeTimeout bounds the token exchange and userinfo calls of a callback.
const exchangeTimeout = 30 * time.Second

// oauthErrorCodeRe matches the error codes of RFC 6749 §4.1.2.1 (and common
// extensions) that are safe to echo.
var oauthErrorCodeRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// claimsNotFromUserInfo are ID-token claims userinfo may not override.
var claimsNotFromUserInfo = map[string]bool{
	"iss": true, "sub": true, "aud": true, "exp": true, "iat": true, "nbf": true,
	"nonce": true, "azp": true, "at_hash": true, "c_hash": true, "auth_time": true,
}

// oidcLogin implements the browser login of auth.mode=oidc (SPEC §8.1).
type oidcLogin struct {
	a            *authenticator
	provider     *idpProvider
	verifier     *oidc.IDTokenVerifier
	oauth        oauth2.Config
	endSession   string
	stateKey     []byte
	callbackPath string
	stateCookie  string
	statePath    string
	useUserInfo  bool
	sessionTTL   time.Duration
}

// newOIDCLogin builds the login flow from a discovered provider.
func (a *authenticator) newOIDCLogin(p *idpProvider) (*oidcLogin, error) {
	o := a.cfg.Auth.OIDC
	if a.hub == nil {
		return nil, errors.New("auth: server.public_url is required for auth.mode=oidc")
	}
	redirect := strings.TrimSpace(o.RedirectURL)
	if redirect == "" {
		redirect = a.hub.String() + CallbackPath
	}
	ru, err := url.Parse(redirect)
	if err != nil || ru.Host == "" || (ru.Scheme != "http" && ru.Scheme != "https") {
		return nil, fmt.Errorf("auth: auth.oidc.redirect_url must be an absolute URL, got %q", redirect)
	}
	// The login state cookie is set by /login on the hub (host-only), so the
	// IdP must send the browser back to the hub as well.
	if !strings.EqualFold(ru.Host, a.hub.Host) || !strings.EqualFold(ru.Scheme, a.hub.Scheme) {
		return nil, fmt.Errorf("auth: auth.oidc.redirect_url %q must be on the hub (%s)", redirect, a.hub.String())
	}
	scopes := append([]string(nil), o.Scopes...)
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}
	ttl := o.SessionTTL.D()
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	l := &oidcLogin{
		a:        a,
		provider: p,
		verifier: p.verifier(o.ClientID, o.InsecureSkipIssuerCheck, a.now),
		oauth: oauth2.Config{
			ClientID:     o.ClientID,
			ClientSecret: o.ClientSecret,
			Endpoint:     p.provider.Endpoint(),
			RedirectURL:  redirect,
			Scopes:       scopes,
		},
		stateKey:     keys.Derive(a.cfg.Server.Secret, keys.OAuthState),
		callbackPath: ru.Path,
		stateCookie:  OAuthCookie,
		statePath:    "/",
		useUserInfo:  o.UseUserInfo,
		sessionTTL:   ttl,
	}
	if l.useUserInfo && p.provider.UserInfoEndpoint() == "" {
		a.log.Warn("auth: auth.oidc.use_userinfo is set but the identity provider has no userinfo endpoint; ignoring it",
			"issuer", p.issuer)
		l.useUserInfo = false
	}
	switch {
	case a.secure:
		// In host mode every site's page can set cookies for the cookie
		// domain, which the hub receives too. A "__Host-" cookie can only be
		// set by the hub itself, so no site can plant its author's login
		// state to sign visitors in as the author (login CSRF). The prefix
		// requires Secure, Path=/ and no Domain.
		l.stateCookie = OAuthHostCookie
	case strings.HasPrefix(ru.Path, PathPrefix):
		l.statePath = PathPrefix
	}
	if !a.secure && a.cookieDomain != "" {
		a.log.Warn("auth: server.public_url is not https, so the login state cookie cannot use the __Host- prefix and "+
			"a page on one site could plant it to sign visitors in to another account; use https outside local tests",
			"public_url", a.hub.String())
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	if err := p.provider.Claims(&meta); err == nil {
		if u, err := url.Parse(meta.EndSession); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			l.endSession = meta.EndSession
		}
	}
	return l, nil
}

// login starts the authorization-code + PKCE flow.
func (l *oidcLogin) login(w http.ResponseWriter, r *http.Request) {
	a := l.a
	st := oauthState{
		State:    randomString(32),
		Verifier: oauth2.GenerateVerifier(),
		Nonce:    randomString(32),
		RD:       a.sanitizeRedirect(r.URL.Query().Get("rd")),
		Exp:      a.now().Add(oauthStateTTL).Unix(),
	}
	v, err := encodeState(l.stateKey, st)
	if err != nil {
		a.log.Error("auth: login state", "error", err)
		renderError(w, http.StatusInternalServerError, "Sign-in failed", "Could not start the sign-in flow.", "", "")
		return
	}
	http.SetCookie(w, a.newCookie(l.stateCookie, v, "", l.statePath, oauthStateTTL))
	target := l.oauth.AuthCodeURL(st.State, oauth2.S256ChallengeOption(st.Verifier), oidc.Nonce(st.Nonce))
	noStore(w)
	http.Redirect(w, r, target, http.StatusFound)
}

// callback completes the flow and sets the session cookie.
func (l *oidcLogin) callback(w http.ResponseWriter, r *http.Request) {
	a := l.a
	q := r.URL.Query()
	st, stErr := l.readState(r)
	// The state cookie is single-use.
	http.SetCookie(w, a.newCookie(l.stateCookie, "", "", l.statePath, 0))

	retry := a.LoginURL(a.hubURL())
	if st != nil {
		retry = a.LoginURL(st.RD)
	}
	if e := q.Get("error"); e != "" {
		status := http.StatusBadRequest
		if e == "access_denied" || e == "unauthorized_client" || e == "login_required" {
			status = http.StatusForbidden
		}
		msg := "The identity provider did not complete the sign-in."
		if oauthErrorCodeRe.MatchString(e) {
			msg = "The identity provider did not complete the sign-in (" + e + ")."
		}
		// The description is free text from the query string: show it only
		// to a browser that really started a sign-in, so a crafted link
		// cannot put arbitrary text on this domain.
		detail := ""
		if stErr == nil && equalConstantTime(q.Get("state"), st.State) {
			detail = truncate(q.Get("error_description"), 300)
		}
		renderPage(w, status, pageData{
			Title: "Sign-in was not completed", Message: msg, Detail: detail,
			LinkURL: retry, LinkText: "Try again",
		})
		return
	}
	if stErr != nil {
		a.log.Info("auth: login callback without valid state", "error", stErr)
		renderError(w, http.StatusBadRequest, "Sign-in expired",
			"Your sign-in attempt expired or your browser blocked cookies. Please try again.", retry, "Try again")
		return
	}
	if !equalConstantTime(q.Get("state"), st.State) {
		renderError(w, http.StatusBadRequest, "Sign-in failed",
			"The sign-in response did not match this browser session (state mismatch). Please try again.", retry, "Try again")
		return
	}
	code := q.Get("code")
	if code == "" {
		renderError(w, http.StatusBadRequest, "Sign-in failed", "The identity provider returned no authorization code.", retry, "Try again")
		return
	}

	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), a.client), exchangeTimeout)
	defer cancel()
	tok, err := l.oauth.Exchange(ctx, code, oauth2.VerifierOption(st.Verifier))
	if err != nil {
		a.log.Warn("auth: authorization code exchange failed", "error", err)
		renderError(w, http.StatusBadGateway, "Sign-in failed",
			"Backyard could not complete the sign-in with your identity provider.", retry, "Try again")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	if rawID == "" {
		a.log.Warn("auth: token response without id_token")
		renderError(w, http.StatusBadGateway, "Sign-in failed", "The identity provider returned no ID token.", retry, "Try again")
		return
	}
	idt, err := l.verifier.Verify(ctx, rawID)
	if err != nil {
		a.log.Warn("auth: id token verification failed", "error", err)
		renderError(w, http.StatusBadGateway, "Sign-in failed", "The identity provider's ID token could not be verified.", retry, "Try again")
		return
	}
	if !equalConstantTime(idt.Nonce, st.Nonce) {
		a.log.Warn("auth: id token nonce mismatch")
		renderError(w, http.StatusBadRequest, "Sign-in failed", "The ID token does not belong to this sign-in (nonce mismatch).", retry, "Try again")
		return
	}
	if idt.AccessTokenHash != "" {
		if err := idt.VerifyAccessToken(tok.AccessToken); err != nil {
			a.log.Warn("auth: access token hash mismatch", "error", err)
			renderError(w, http.StatusBadGateway, "Sign-in failed", "The identity provider's response is inconsistent.", retry, "Try again")
			return
		}
	}
	claims := map[string]any{}
	if err := idt.Claims(&claims); err != nil {
		a.log.Warn("auth: decode id token claims", "error", err)
		renderError(w, http.StatusBadGateway, "Sign-in failed", "The ID token could not be read.", retry, "Try again")
		return
	}
	if err := checkAuthorizedParty(idt.Audience, claims, l.oauth.ClientID); err != nil {
		a.log.Warn("auth: id token authorized party", "error", err)
		renderError(w, http.StatusBadGateway, "Sign-in failed", "The ID token was not issued to Backyard.", retry, "Try again")
		return
	}
	if l.useUserInfo {
		if err := l.mergeUserInfo(ctx, tok, idt.Subject, claims); err != nil {
			a.log.Warn("auth: userinfo", "error", err)
			renderError(w, http.StatusBadGateway, "Sign-in failed", "Your profile could not be loaded from the identity provider.", retry, "Try again")
			return
		}
	}

	base := a.mapClaims(claims)
	u, err := a.finalize(r.Context(), base.Clone(), srcIDP)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			detail := "Ask an administrator for access, or sign in with a different account."
			if unverified := a.unverifiedEmail(claims, base); unverified != "" {
				// MapClaims ignored an email the IdP marked as unverified
				// (email_verified=false), so domain/admin rules could not
				// match it. Tell both the user and the operator why.
				a.log.Warn("auth: the identity provider marked the user's email as unverified (email_verified=false); "+
					"it is ignored for allowed_domains and admins — verify emails at the IdP (Keycloak: \"Trust Email\") "+
					"or map identity.claims.email to another claim", "subject", base.ID)
				detail = "Your identity provider has not verified the email address " + unverified +
					", so Backyard cannot use it. Ask your administrator to verify it."
			}
			a.log.Info("auth: login denied by allowed_domains/allowed_groups", "user", displayID(base), "audit", true)
			renderPage(w, http.StatusForbidden, pageData{
				Title:   "Access denied",
				Message: "You signed in as " + displayID(base) + ", but this account is not allowed to use Backyard.",
				Detail:  detail,
				LinkURL: a.logoutURL(), LinkText: "Sign out",
			})
			return
		}
		renderError(w, http.StatusForbidden, "Sign-in failed",
			"Your identity provider did not share an email address or subject for your account.", "", "")
		return
	}
	value, err := a.sess.seal(base, a.now().Add(l.sessionTTL), a.isImportantGroup)
	if err != nil {
		a.log.Error("auth: seal session", "user", displayID(u), "error", err)
		renderError(w, http.StatusInternalServerError, "Sign-in failed", "Your session could not be created.", retry, "Try again")
		return
	}
	http.SetCookie(w, a.sessionCookie(value, l.sessionTTL))
	a.log.Info("auth: login", "user", u.ID, "audit", true)
	noStore(w)
	http.Redirect(w, r, st.RD, http.StatusFound)
}

// checkAuthorizedParty applies OpenID Connect Core §3.1.3.7 (steps 4–5): an
// ID token with several audiences must name the client in azp, and an azp
// claim, when present, must be the client.
func checkAuthorizedParty(aud []string, claims map[string]any, clientID string) error {
	azp, _ := claims["azp"].(string)
	switch {
	case azp != "" && azp != clientID:
		return fmt.Errorf("id token authorized party is %q, not %q", truncate(azp, 128), clientID)
	case azp == "" && len(aud) > 1:
		return errors.New("id token has several audiences but no authorized party")
	}
	return nil
}

// readState returns the verified login state from the login state cookie
// (__Host-backyard_oauth over https, backyard_oauth over http).
func (l *oidcLogin) readState(r *http.Request) (*oauthState, error) {
	lastErr := errors.New("no login state cookie")
	for _, c := range r.Cookies() {
		if c.Name != l.stateCookie {
			continue
		}
		st, err := decodeState(l.stateKey, c.Value, l.a.now())
		if err == nil {
			return st, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// mergeUserInfo fetches userinfo and merges it into claims (profile claims
// only; the subject must match the ID token's).
func (l *oidcLogin) mergeUserInfo(ctx context.Context, tok *oauth2.Token, sub string, claims map[string]any) error {
	ui, err := l.provider.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		return fmt.Errorf("fetch userinfo: %w", err)
	}
	if ui.Subject != sub {
		return errors.New("userinfo subject does not match the id token")
	}
	var extra map[string]any
	if err := ui.Claims(&extra); err != nil {
		return fmt.Errorf("decode userinfo: %w", err)
	}
	for k, v := range extra {
		if !claimsNotFromUserInfo[k] && v != nil {
			claims[k] = v
		}
	}
	return nil
}

// logout clears the session and ends the IdP session when supported.
func (l *oidcLogin) logout(w http.ResponseWriter, r *http.Request) {
	a := l.a
	a.clearSessionCookies(w)
	target := a.hubURL()
	if l.endSession != "" {
		if u, err := url.Parse(l.endSession); err == nil {
			q := u.Query()
			q.Set("post_logout_redirect_uri", a.hubURL())
			q.Set("client_id", l.oauth.ClientID)
			u.RawQuery = q.Encode()
			target = u.String()
		}
	}
	noStore(w)
	http.Redirect(w, r, target, http.StatusFound)
}

// unverifiedEmail returns the address of the standard "email" claim when
// claim mapping dropped it because the IdP marked it unverified, else "".
func (a *authenticator) unverifiedEmail(claims map[string]any, mapped *identity.User) string {
	if mapped.Email != "" {
		return ""
	}
	if m := strings.TrimSpace(a.cfg.Identity.Claims.Email); m != "" && m != identity.DefaultEmailClaim {
		return ""
	}
	raw, _ := claims[identity.DefaultEmailClaim].(string)
	email := identity.NormalizeEmail(raw)
	if email == "" {
		return ""
	}
	switch v := claims["email_verified"].(type) {
	case bool:
		if !v {
			return email
		}
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "false") {
			return email
		}
	}
	return ""
}

// logoutURL is the hub's logout endpoint ("" when login is not handled here).
func (a *authenticator) logoutURL() string {
	if a.oidc == nil || a.hub == nil {
		return ""
	}
	return a.hub.String() + LogoutPath
}

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
