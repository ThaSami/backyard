package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
	"github.com/thasami/backyard/skills"
)

// auditTimeout bounds the write of one audit record.
const auditTimeout = 5 * time.Second

// serveInfo forwards GET /__backyard/api/info to the gateway's /info.
func (s *Server) serveInfo(w http.ResponseWriter, r *http.Request) {
	if s.deps.Gateway == nil {
		s.writeAPIError(w, r, errNotFound("server info is not available (no deploy gateway)"))
		return
	}
	s.deps.Gateway.ServeHTTP(w, withPath(r, "/info", ""))
}

var (
	commitPathRe  = regexp.MustCompile(`^/sites/([a-z0-9-]+)/deploys/[^/]+/commit$`)
	refreshPathRe = regexp.MustCompile(`^/sites/([a-z0-9-]+)/refresh$`)
	sitePathRe    = regexp.MustCompile(`^/sites/([a-z0-9-]+)$`)
)

// serveDeploy mounts the embedded deploy gateway (it authenticates and
// enforces CSRF by itself). Successful commits, refreshes and deletions
// purge the site from the static caches and are counted in deploys_total.
func (s *Server) serveDeploy(w http.ResponseWriter, r *http.Request) {
	if !s.gatewayOn {
		s.writeAPIError(w, r, errNotFound("the embedded deploy gateway is disabled"))
		return
	}
	r2 := stripPrefix(r, deployRoot)
	p := r2.URL.Path
	var site, method string
	switch {
	case r.Method == http.MethodPost && commitPathRe.MatchString(p):
		site, method = commitPathRe.FindStringSubmatch(p)[1], "api"
	case r.Method == http.MethodPost && refreshPathRe.MatchString(p):
		site, method = refreshPathRe.FindStringSubmatch(p)[1], "sync"
	case r.Method == http.MethodDelete && sitePathRe.MatchString(p):
		site = sitePathRe.FindStringSubmatch(p)[1]
	}
	ri := info(r)
	ri.site = site
	s.deps.Gateway.ServeHTTP(w, r2)
	if site == "" {
		return
	}
	ok := ri.rec != nil && ri.rec.status >= 200 && ri.rec.status < 300
	if ok {
		s.PurgeSite(site)
	}
	if method != "" {
		result := "ok"
		if !ok {
			result = "error"
		}
		s.metrics.deploys.WithLabelValues(method, result).Inc()
	}
}

// serveHubMetrics exposes Prometheus metrics on the hub (authenticated; use
// a static bearer token for scrapers, or telemetry.metrics_listen).
func (s *Server) serveHubMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethods(w, r, http.MethodGet) {
		return
	}
	if _, _, ok := s.authenticate(w, r, true); !ok {
		return
	}
	s.metrics.handler.ServeHTTP(w, r)
}

// docNameRe matches the document name of /docs/<name>.md and
// /docs/references/<name>.md: one path segment of word characters, so no
// "/", "." or ".." can reach the document lookup.
var docNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// docReferencesDir is the optional directory of /docs/references/<name>.md.
// SKILL.md links to its references as "references/<name>.md", which a
// browser or agent resolves against /docs/skill.md to
// /docs/references/<name>.md; the references link to each other by bare
// name, which resolves within the same directory.
const docReferencesDir = "references/"

// hubDocument returns the embedded document for the path below /docs/
// without its ".md" suffix ("skill", "api", "references/api"), or "" when
// there is none. SKILL.md is not a reference, so "references/skill" does
// not exist.
func hubDocument(rel string) string {
	name, isRef := strings.CutPrefix(rel, docReferencesDir)
	switch {
	case !docNameRe.MatchString(name):
		return ""
	case strings.EqualFold(name, "skill"):
		if isRef {
			return ""
		}
		return skillMarkdown()
	default:
		return skillReference(name)
	}
}

// serveHubPage serves the hub UI, docs and llms.txt (authenticated).
func (s *Server) serveHubPage(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethods(w, r, http.MethodGet) {
		return
	}
	_, r, ok := s.authenticate(w, r, false)
	if !ok {
		return
	}
	p := r.URL.Path
	switch {
	case p == "/llms.txt":
		ri := info(r)
		ri.area = "docs"
		s.serveText(w, r, "llms.txt", "text/plain; charset=utf-8", llmsTxt(s.publicURL))
	case p == "/llms-full.txt":
		info(r).area = "docs"
		s.serveText(w, r, "llms-full.txt", "text/plain; charset=utf-8", llmsFullTxt(s.publicURL))
	case p == "/docs" || p == "/docs/":
		info(r).area = "docs"
		s.serveHubAsset(w, r, "docs.html")
	case strings.HasPrefix(p, "/docs/") && strings.HasSuffix(p, ".md"):
		info(r).area = "docs"
		rel := strings.TrimSuffix(strings.TrimPrefix(p, "/docs/"), ".md")
		md := hubDocument(rel)
		if md == "" {
			s.fail(w, r, errNotFound("no such document: %s", truncate(rel, 80)))
			return
		}
		// Like /llms-full.txt: the documents use skills.HubURLPlaceholder
		// for "this hub" and its example site hosts.
		s.serveText(w, r, path.Base(rel)+".md", "text/markdown; charset=utf-8", skills.Localize(md, s.publicURL))
	default:
		name := strings.TrimPrefix(p, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			name += "index.html"
		}
		s.serveHubAsset(w, r, name)
	}
}

// serveText serves a generated text document.
func (s *Server) serveText(w http.ResponseWriter, r *http.Request, name, ct, body string) {
	data := []byte(body)
	s.serveObject(w, r, &object{
		name: name, size: int64(len(data)), etag: contentETag(data), contentType: ct, data: data,
	}, serveOptions{cacheControl: "no-cache"})
}

// serveHubAsset serves a file of the embedded hub UI.
func (s *Server) serveHubAsset(w http.ResponseWriter, r *http.Request, name string) {
	fsys := hubFS()
	if !fs.ValidPath(name) {
		s.fail(w, r, errNotFound("The page you are looking for does not exist."))
		return
	}
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		// A directory with an index.html: redirect to the slash form.
		if _, ierr := fs.Stat(fsys, name+"/index.html"); ierr == nil {
			loc := r.URL.EscapedPath() + "/"
			if r.URL.RawQuery != "" {
				loc += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, loc, http.StatusMovedPermanently)
			return
		}
		if errors.Is(err, fs.ErrNotExist) || isDirErr(fsys, name) {
			s.fail(w, r, errNotFound("The page you are looking for does not exist."))
			return
		}
		s.fail(w, r, errInternal(err, "reading hub asset"))
		return
	}
	var mod time.Time
	if st, err := fs.Stat(fsys, name); err == nil {
		mod = st.ModTime()
	}
	s.serveObject(w, r, &object{
		name: name, size: int64(len(data)), etag: contentETag(data), modTime: mod,
		contentType: contentTypeFor(name, ""), data: data,
	}, serveOptions{cacheControl: "no-cache"})
}

func isDirErr(fsys fs.FS, name string) bool {
	st, err := fs.Stat(fsys, name)
	return err == nil && st.IsDir()
}

// handleSites serves GET /__backyard/api/sites (hub).
func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := opCtx(r)
	defer cancel()
	list, err := s.deps.Sites.List(ctx)
	if err != nil {
		if ctx.Err() != nil {
			s.writeAPIError(w, r, errContext(err, "listing sites"))
			return
		}
		s.writeAPIError(w, r, errInternal(err, "listing sites failed"))
		return
	}
	if list == nil {
		list = []sites.Meta{}
	}
	if len(s.devSites) > 0 {
		seen := make(map[string]bool, len(list))
		for _, m := range list {
			seen[m.Name] = true
		}
		var dev []string
		for name := range s.devSites {
			if !seen[name] {
				dev = append(dev, name)
			}
		}
		sort.Strings(dev)
		for _, name := range dev {
			list = append(list, sites.Meta{Name: name, URL: s.cfg.SiteURL(name), Title: name + " (dev)"})
		}
	}
	for i := range list {
		if list[i].Owners == nil {
			list[i].Owners = []string{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": list})
}

type tokenRequest struct {
	Name     string  `json:"name"`
	TTLHours float64 `json:"ttl_hours"`
}

type tokenResponse struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	Name      string    `json:"name,omitempty"`
}

// handleTokens serves POST /__backyard/api/tokens: mint a personal API token
// for the current (non-anonymous, non-static-token) user.
//
// A request authenticated with a Backyard API token gets a token that
// expires no later than the one it presented (a token cannot renew itself;
// see presentedAPIToken). A browser session, a proxy identity or an
// IdP-issued bearer JWT (device flow) is a fresh authentication by the IdP
// and gets the full lifetime.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	ri := info(r)
	u := ri.user
	if s.deps.Tokens == nil {
		s.writeAPIError(w, r, errUnsupported("API tokens are not available on this server"))
		return
	}
	if u == nil || u.Anonymous || strings.HasPrefix(u.ID, "anon:") {
		s.writeAPIError(w, r, errForbidden("anonymous users cannot create API tokens"))
		return
	}
	if strings.HasPrefix(u.ID, "token:") {
		s.writeAPIError(w, r, errForbidden("static tokens cannot create API tokens"))
		return
	}
	// Read the presented token's expiry before the body: how long the body
	// takes to arrive must not matter.
	limit, bounded := presentedAPIToken(r, u)
	var req tokenRequest
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		s.writeAPIError(w, r, jsonError(err, 64<<10))
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeAPIError(w, r, jsonError(err, 64<<10))
			return
		}
	}
	req.Name = strings.TrimSpace(req.Name)
	if len(req.Name) > 100 {
		s.writeAPIError(w, r, errBadRequest("name must be at most 100 characters"))
		return
	}
	maxTTL := s.cfg.Auth.TokenTTL.D()
	if maxTTL <= 0 {
		maxTTL = 720 * time.Hour
	}
	var ttl time.Duration
	switch {
	case req.TTLHours < 0:
		s.writeAPIError(w, r, errBadRequest("ttl_hours must be positive"))
		return
	case req.TTLHours > 0:
		if req.TTLHours > maxTTL.Hours() {
			s.writeAPIError(w, r, errBadRequest("ttl_hours must be at most %g", maxTTL.Hours()))
			return
		}
		ttl = time.Duration(req.TTLHours * float64(time.Hour))
		if ttl < time.Minute {
			ttl = time.Minute
		}
	}
	if bounded {
		// Whole seconds (token times are), minus one second for the clock
		// Issue reads a moment later: the new token never outlives the
		// presented one, so chaining tokens cannot extend access.
		left := time.Until(limit).Truncate(time.Second) - time.Second
		if left <= 0 {
			s.writeAPIError(w, r, newError(http.StatusUnauthorized, codeUnauthenticated,
				"the API token used for this request has expired; sign in again"))
			return
		}
		if ttl <= 0 || ttl > left {
			ttl = left
		}
	}
	tok, exp, err := s.deps.Tokens.Issue(u, ttl)
	if err == nil && bounded && exp.After(limit) {
		err = errors.New("server: minted token would outlive the presented token")
	}
	if err != nil {
		s.writeAPIError(w, r, errInternal(err, "issuing the token failed"))
		return
	}
	fields := map[string]any{"name": req.Name, "expires_at": exp.UTC().Format(time.RFC3339)}
	if bounded {
		fields["from_api_token"] = true
	}
	s.audit(r.Context(), u, "token.issue", fields)
	writeJSON(w, http.StatusOK, tokenResponse{Token: tok, ExpiresAt: exp.UTC(), Name: req.Name})
}

// presentedAPIToken returns the expiry of the Backyard API token that r
// presents as its bearer credential for u, and whether there is one.
//
// A token minted for such a request must not outlive that token. Otherwise
// a token could renew itself forever, and keep a snapshot of the user's
// groups and admin rights after the user leaves the IdP. Only rotating
// server.secret would end that.
//
// The token is not verified again here, because Authenticate already did. A
// bearer that did not authenticate the request can only shorten the token
// the caller mints for itself: a forged one, or an expired one when a
// session or proxy identity authenticated the request instead. IdP-issued
// JWTs (iss is the IdP) and static tokens are not Backyard API tokens.
func presentedAPIToken(r *http.Request, u *identity.User) (time.Time, bool) {
	tok, ok := bearerCredential(r)
	if !ok || u == nil {
		return time.Time{}, false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var c struct {
		Iss string   `json:"iss"`
		Sub string   `json:"sub"`
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(raw, &c) != nil || c.Iss != auth.TokenIssuer || strings.TrimSpace(c.Sub) != u.ID {
		return time.Time{}, false
	}
	if c.Exp == nil || math.IsNaN(*c.Exp) || math.IsInf(*c.Exp, 0) {
		// Verify requires exp; treat a token without one as expired.
		return time.Unix(0, 0), true
	}
	sec, frac := math.Modf(*c.Exp)
	return time.Unix(int64(sec), int64(frac*1e9)), true
}

// bearerCredential returns the token of an "Authorization: Bearer" header
// (parsed like the authenticator does).
func bearerCredential(r *http.Request) (string, bool) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok := strings.TrimSpace(rest)
	return tok, tok != ""
}

// audit logs an audit event and stores it in _system/_audit (best effort,
// in the background).
func (s *Server) audit(ctx context.Context, u *identity.User, action string, fields map[string]any) {
	ri := infoFromContext(ctx)
	attrs := []any{"audit", true, "action", action, "user", u.Actor()}
	if ri != nil {
		attrs = append(attrs, "ip", ri.clientIP, "request_id", ri.requestID)
	}
	for k, v := range fields {
		attrs = append(attrs, k, v)
	}
	s.log.InfoContext(ctx, "audit", attrs...)

	data := map[string]any{"action": action, "user": u.Actor(), "at": time.Now().UTC().Format(time.RFC3339Nano)}
	if ri != nil {
		data["ip"] = ri.clientIP
		data["request_id"] = ri.requestID
	}
	for k, v := range fields {
		data[k] = v
	}
	bctx := context.WithoutCancel(ctx)
	s.bg.run(0, func() {
		actx, cancel := context.WithTimeout(bctx, auditTimeout)
		defer cancel()
		if _, err := s.deps.Docs.Create(actx, identity.SystemSite, "_audit", "", data, u.Actor()); err != nil {
			s.log.Warn("audit record write failed", "action", action, "error", err)
		}
	})
}

func infoFromContext(ctx context.Context) *reqInfo {
	ri, _ := ctx.Value(reqInfoKey{}).(*reqInfo)
	return ri
}
