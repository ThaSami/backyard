// Package clitest provides an in-memory fake of a Backyard server's client
// surface (the deploy gateway protocol of SPEC §7, the CLI login handoff of
// §8.3 and POST /__backyard/api/tokens) for testing the CLI and the MCP
// server without running the real server.
package clitest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites"
)

// GatewayPath is where the fake mounts the gateway (as the real server).
const GatewayPath = "/__backyard/api/deploy"

// Site is a site stored by the fake.
type Site struct {
	Meta  sites.Meta
	Files map[string][]byte
}

// Request records one request received by the fake.
type Request struct {
	Method string
	Path   string
	Auth   string
	CSRF   string
	Query  url.Values
}

// TokenRequest records a POST /__backyard/api/tokens call.
type TokenRequest struct {
	Auth string
	Body map[string]any
}

// Gateway is a fake Backyard server. Configure its exported fields before
// issuing requests; use Lock/Unlock (or the helpers) when reading state
// while requests may be in flight.
type Gateway struct {
	Server *httptest.Server
	// URL is the server root (hub URL).
	URL string
	// Info is returned by GET /info (PublicURL and GatewayURL are filled
	// in by NewGateway).
	Info deploy.Info
	// Tokens maps valid bearer tokens to users.
	Tokens map[string]identity.User
	// Anonymous accepts requests without a token (as a guest).
	Anonymous bool
	// HTML makes every gateway route answer with an HTML login page, like
	// an identity-aware proxy in front of the server.
	HTML bool
	// CLIToken is issued by /__backyard/auth/cli (with CLITokenExpiry).
	CLIToken       string
	CLITokenExpiry time.Time
	// MintedToken is returned by POST /__backyard/api/tokens.
	MintedToken string

	mu       sync.Mutex
	sites    map[string]*Site
	requests []Request
	tokenReq []TokenRequest
	uploads  map[string]pendingUpload
	deploys  map[string]string // deploy id → site
	nextID   int
	putCount int
}

type pendingUpload struct {
	site, path, sha string
	size            int64
}

// NewGateway starts a fake server; it is closed when the test ends.
func NewGateway(t testing.TB) *Gateway {
	g := &Gateway{
		Tokens:         map[string]identity.User{},
		sites:          map[string]*Site{},
		uploads:        map[string]pendingUpload{},
		deploys:        map[string]string{},
		CLIToken:       "cli-token-from-browser",
		CLITokenExpiry: time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC),
		MintedToken:    "minted-api-token",
	}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.Server.Close)
	g.URL = g.Server.URL
	g.Info = deploy.Info{
		Name: "Backyard", Version: "test", PublicURL: g.URL, BaseDomain: "127.0.0.1", URLMode: "path",
		GatewayURL:  g.URL + GatewayPath,
		Auth:        deploy.AuthInfo{Mode: "oidc", CLILoginURL: g.URL + "/__backyard/auth/cli"},
		Methods:     []string{"api"},
		Limits:      deploy.Limits{MaxSiteBytes: 500 << 20, MaxFiles: 10000, MaxFileBytes: 100 << 20},
		SiteListing: "all",
	}
	return g
}

// AddToken registers a valid bearer token (safe while requests are in flight).
func (g *Gateway) AddToken(token string, u identity.User) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Tokens[token] = u
}

// GatewayURL returns the embedded gateway URL.
func (g *Gateway) GatewayURL() string { return g.URL + GatewayPath }

// SiteURL returns the path-mode URL of a site.
func (g *Gateway) SiteURL(site string) string { return g.URL + "/s/" + site + "/" }

// AddSite stores a site with files (path → content), last deployed by updatedBy.
func (g *Gateway) AddSite(name string, files map[string]string, updatedBy string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := &Site{Files: map[string][]byte{}}
	for p, c := range files {
		s.Files[p] = []byte(c)
	}
	s.Meta = sites.Meta{Name: name, Title: strings.ToUpper(name[:1]) + name[1:], CreatedBy: updatedBy, UpdatedBy: updatedBy,
		CreatedAt: time.Now().Add(-48 * time.Hour).UTC(), UpdatedAt: time.Now().Add(-time.Hour).UTC(), Owners: []string{}}
	g.updateMetaLocked(name, s, updatedBy)
	g.sites[name] = s
}

// SetFileRaw stores a file under any path (even an invalid one), to test
// client-side path validation.
func (g *Gateway) SetFileRaw(site, path string, content []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.sites[site]; s != nil {
		s.Files[path] = content
	}
}

// Site returns a copy of a stored site's files (nil when missing).
func (g *Gateway) SiteFiles(name string) map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sites[name]
	if s == nil {
		return nil
	}
	out := make(map[string]string, len(s.Files))
	for p, b := range s.Files {
		out[p] = string(b)
	}
	return out
}

// Meta returns a stored site's metadata.
func (g *Gateway) Meta(name string) (sites.Meta, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sites[name]
	if s == nil {
		return sites.Meta{}, false
	}
	return s.Meta, true
}

// Requests returns the recorded requests.
func (g *Gateway) Requests() []Request {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.requests)
}

// Count returns how many recorded requests match method and path suffix.
func (g *Gateway) Count(method, pathSuffix string) int {
	n := 0
	for _, r := range g.Requests() {
		if r.Method == method && strings.HasSuffix(r.Path, pathSuffix) {
			n++
		}
	}
	return n
}

// Uploads returns the number of PUT uploads received.
func (g *Gateway) Uploads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.putCount
}

// TokenRequests returns the recorded token-mint requests.
func (g *Gateway) TokenRequests() []TokenRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.tokenReq)
}

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, Request{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"),
		CSRF: r.Header.Get("X-Backyard"), Query: r.URL.Query()})
	g.mu.Unlock()

	switch {
	case r.URL.Path == "/__backyard/auth/cli":
		g.serveCLILogin(w, r)
		return
	case r.URL.Path == "/__backyard/api/tokens":
		g.serveTokens(w, r)
		return
	case strings.HasPrefix(r.URL.Path, GatewayPath+"/") || r.URL.Path == GatewayPath:
	default:
		http.NotFound(w, r)
		return
	}
	if g.HTML {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<!doctype html><html><body>Sign in with your company account</body></html>")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, GatewayPath)
	if rest == "/info" && r.Method == http.MethodGet {
		g.mu.Lock()
		info := g.Info
		g.mu.Unlock()
		writeJSON(w, 200, info)
		return
	}
	if strings.HasPrefix(rest, "/uploads/") && r.Method == http.MethodPut {
		g.serveUpload(w, r, strings.TrimPrefix(rest, "/uploads/"))
		return
	}
	u, ok := g.user(r)
	if !ok {
		writeErr(w, 401, "unauthenticated", "authentication required")
		return
	}
	if r.Method != http.MethodGet && r.Header.Get("X-Backyard") != "1" {
		writeErr(w, 403, "forbidden", "missing X-Backyard header")
		return
	}
	if rest == "/me" && r.Method == http.MethodGet {
		writeJSON(w, 200, u)
		return
	}
	if rest == "/sites" && r.Method == http.MethodGet {
		g.mu.Lock()
		list := []sites.Meta{}
		for _, s := range g.sites {
			list = append(list, s.Meta)
		}
		g.mu.Unlock()
		slices.SortFunc(list, func(a, b sites.Meta) int { return strings.Compare(a.Name, b.Name) })
		writeJSON(w, 200, map[string]any{"sites": list})
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(rest, "/sites/"), "/", 2)
	if !strings.HasPrefix(rest, "/sites/") || parts[0] == "" {
		writeErr(w, 404, "not_found", "no such route")
		return
	}
	site, err := url.PathUnescape(parts[0])
	if err != nil {
		writeErr(w, 400, "bad_request", "bad site")
		return
	}
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	g.serveSite(w, r, u, site, sub)
}

func (g *Gateway) user(r *http.Request) (*identity.User, bool) {
	auth := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
		g.mu.Lock()
		u, found := g.Tokens[tok]
		g.mu.Unlock()
		if found {
			return &u, true
		}
		return nil, false
	}
	if g.Anonymous {
		return &identity.User{ID: "anon:0123456789abcdef", Name: "Guest 0123", Anonymous: true}, true
	}
	return nil, false
}

func (g *Gateway) serveSite(w http.ResponseWriter, r *http.Request, u *identity.User, site, sub string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sites[site]
	switch {
	case sub == "" && r.Method == http.MethodGet:
		if s == nil {
			writeErr(w, 404, "not_found", "site not found")
			return
		}
		writeJSON(w, 200, s.Meta)
	case sub == "" && r.Method == http.MethodDelete:
		delete(g.sites, site)
		w.WriteHeader(http.StatusNoContent)
	case sub == "files" && r.Method == http.MethodGet:
		if s == nil {
			writeErr(w, 404, "not_found", "site not found")
			return
		}
		files := []deploy.SiteFile{}
		for p, b := range s.Files {
			files = append(files, deploy.SiteFile{Path: p, Size: int64(len(b)), SHA256: sha(b), UpdatedAt: s.Meta.UpdatedAt})
		}
		slices.SortFunc(files, func(a, b deploy.SiteFile) int { return strings.Compare(a.Path, b.Path) })
		writeJSON(w, 200, map[string]any{"files": files})
	case strings.HasPrefix(sub, "files/") && r.Method == http.MethodGet:
		p, err := url.PathUnescape(strings.TrimPrefix(sub, "files/"))
		if err == nil {
			if why := deploy.ValidatePath(p); why != "" {
				writeErr(w, 400, "bad_request", why) // as the real gateway (e.g. any .git segment)
				return
			}
		}
		if err != nil || s == nil || s.Files[p] == nil {
			writeErr(w, 404, "not_found", "file not found")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(s.Files[p])
	case sub == "refresh" && r.Method == http.MethodPost:
		if s == nil {
			s = &Site{Files: map[string][]byte{}, Meta: sites.Meta{Name: site, CreatedAt: time.Now().UTC(), CreatedBy: u.ID}}
			g.sites[site] = s
		}
		g.updateMetaLocked(site, s, u.ID)
		writeJSON(w, 200, s.Meta)
	case sub == "deploys" && r.Method == http.MethodPost:
		var req deploy.CreateDeployRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "bad_request", err.Error())
			return
		}
		g.nextID++
		id := fmt.Sprintf("dep-%d", g.nextID)
		g.deploys[id] = site
		resp := deploy.CreateDeployResponse{DeployID: id, Uploads: []deploy.Upload{}, Unchanged: []string{}, ExpiresAt: time.Now().Add(time.Hour)}
		for _, f := range req.Files {
			if why := deploy.ValidatePath(f.Path); why != "" {
				writeErr(w, 400, "bad_request", why)
				return
			}
			if s != nil && s.Files[f.Path] != nil && sha(s.Files[f.Path]) == f.SHA256 {
				resp.Unchanged = append(resp.Unchanged, f.Path)
				continue
			}
			g.nextID++
			upID := fmt.Sprintf("up-%d", g.nextID)
			g.uploads[upID] = pendingUpload{site: site, path: f.Path, sha: f.SHA256, size: f.Size}
			upURL := g.GatewayURL() + "/uploads/" + upID
			if g.nextID%2 == 0 {
				upURL = "uploads/" + upID // relative URLs resolve against the gateway
			}
			resp.Uploads = append(resp.Uploads, deploy.Upload{Path: f.Path, URL: upURL, Method: http.MethodPut,
				Headers: map[string]string{"Content-Type": f.ContentType}})
		}
		writeJSON(w, 200, resp)
	case strings.HasPrefix(sub, "deploys/") && strings.HasSuffix(sub, "/commit") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(sub, "deploys/"), "/commit")
		if g.deploys[id] != site {
			writeErr(w, 400, "bad_request", "unknown deploy")
			return
		}
		var req deploy.CommitRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "bad_request", err.Error())
			return
		}
		if s == nil {
			s = &Site{Files: map[string][]byte{}, Meta: sites.Meta{Name: site, CreatedAt: time.Now().UTC(), CreatedBy: u.ID, Owners: []string{}}}
			g.sites[site] = s
		}
		keep := map[string]bool{}
		for _, f := range req.Files {
			b := s.Files[f.Path]
			if b == nil || sha(b) != f.SHA256 {
				writeErr(w, 409, "conflict", "file not uploaded: "+f.Path)
				return
			}
			keep[f.Path] = true
		}
		deleted := 0
		for p := range s.Files {
			if !keep[p] {
				delete(s.Files, p)
				deleted++
			}
		}
		g.updateMetaLocked(site, s, u.ID)
		s.Meta.DeployID = id
		delete(g.deploys, id)
		writeJSON(w, 200, deploy.CommitResponse{Site: s.Meta, URL: s.Meta.URL, Deleted: deleted})
	default:
		writeErr(w, 404, "not_found", "no such route")
	}
}

func (g *Gateway) serveUpload(w http.ResponseWriter, r *http.Request, id string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	up, ok := g.uploads[id]
	if !ok {
		writeErr(w, 404, "not_found", "unknown upload")
		return
	}
	if int64(len(body)) != up.size || sha(body) != up.sha {
		writeErr(w, 400, "bad_request", "size or checksum mismatch")
		return
	}
	g.putCount++
	s := g.sites[up.site]
	if s == nil {
		s = &Site{Files: map[string][]byte{}, Meta: sites.Meta{Name: up.site, CreatedAt: time.Now().UTC(), Owners: []string{}}}
		g.sites[up.site] = s
	}
	s.Files[up.path] = body
	delete(g.uploads, id)
	w.WriteHeader(http.StatusOK)
}

func (g *Gateway) updateMetaLocked(name string, s *Site, actor string) {
	s.Meta.Name = name
	s.Meta.URL = g.SiteURL(name)
	s.Meta.FileCount = len(s.Files)
	s.Meta.Bytes = 0
	for _, b := range s.Files {
		s.Meta.Bytes += int64(len(b))
	}
	s.Meta.UpdatedBy = actor
	s.Meta.UpdatedAt = time.Now().UTC()
	if s.Meta.Owners == nil {
		s.Meta.Owners = []string{}
	}
}

func (g *Gateway) serveCLILogin(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("mode") == "display" {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "<html><body><code>%s</code></body></html>", g.CLIToken)
		return
	}
	target, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || target.Hostname() != "127.0.0.1" || q.Get("state") == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	vals := target.Query()
	vals.Set("token", g.CLIToken)
	vals.Set("expires_at", g.CLITokenExpiry.Format(time.RFC3339))
	vals.Set("state", q.Get("state"))
	target.RawQuery = vals.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (g *Gateway) serveTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 405, "bad_request", "POST only")
		return
	}
	if r.Header.Get("X-Backyard") != "1" {
		writeErr(w, 403, "forbidden", "missing X-Backyard header")
		return
	}
	if _, ok := g.user(r); !ok || r.Header.Get("Authorization") == "" {
		writeErr(w, 401, "unauthenticated", "authentication required")
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	g.mu.Lock()
	g.tokenReq = append(g.tokenReq, TokenRequest{Auth: r.Header.Get("Authorization"), Body: body})
	g.mu.Unlock()
	hours, _ := body["ttl_hours"].(float64)
	writeJSON(w, 200, map[string]any{"token": g.MintedToken, "expires_at": time.Now().Add(time.Duration(hours) * time.Hour).UTC()})
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	var eb deploy.ErrorBody
	eb.Error.Code, eb.Error.Message = code, msg
	writeJSON(w, status, eb)
}
