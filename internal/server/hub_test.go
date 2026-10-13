package server

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/skills"
)

func TestHubPages(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		name   string
		url    string
		status int
		ctype  string
		body   string
	}{
		{"index", "https://backyard.test/", 200, "text/html; charset=utf-8", "hub index"},
		{"index explicit", "https://backyard.test/index.html", 200, "text/html; charset=utf-8", "hub index"},
		{"asset", "https://backyard.test/assets/app.js", 200, "text/javascript; charset=utf-8", "console.log('hub')"},
		{"css", "https://backyard.test/assets/app.css", 200, "text/css; charset=utf-8", "body{}"},
		{"subdir index", "https://backyard.test/guide/", 200, "text/html; charset=utf-8", "guide"},
		{"docs", "https://backyard.test/docs", 200, "text/html; charset=utf-8", "api docs"},
		{"docs slash", "https://backyard.test/docs/", 200, "text/html; charset=utf-8", "api docs"},
		{"api md", "https://backyard.test/docs/api.md", 200, "text/markdown; charset=utf-8", "# API reference"},
		{"skill md", "https://backyard.test/docs/SKILL.md", 200, "text/markdown; charset=utf-8", "# Backyard skill"},
		{"missing md", "https://backyard.test/docs/nope.md", 404, "text/html; charset=utf-8", "no such document"},
		{"bad md name", "https://backyard.test/docs/a.b.md", 404, "", ""},
		{"traversal md", "https://backyard.test/docs/..%2fx.md", 301, "", ""},
		{"llms", "https://backyard.test/llms.txt", 200, "text/plain; charset=utf-8", "> https://backyard.test/llms-full.txt"},
		{"llms full", "https://backyard.test/llms-full.txt", 200, "text/plain; charset=utf-8", "# Backyard full\nhttps://backyard.test"},
		{"missing", "https://backyard.test/nope", 404, "text/html; charset=utf-8", "Page not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := h.get(tc.url)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if tc.ctype != "" && w.Header().Get("Content-Type") != tc.ctype {
				t.Fatalf("Content-Type = %q, want %q", w.Header().Get("Content-Type"), tc.ctype)
			}
			if !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.body)
			}
		})
	}
	w := h.get("https://backyard.test/guide")
	if w.Code != 301 || w.Header().Get("Location") != "/guide/" {
		t.Fatalf("dir redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	// Hub pages require authentication.
	h.auth.set(nil, auth.ErrUnauthenticated)
	for _, u := range []string{"https://backyard.test/", "https://backyard.test/llms.txt", "https://backyard.test/docs/api.md"} {
		if w := h.get(u); w.Code != http.StatusFound {
			t.Errorf("%s: status %d, want redirect to login", u, w.Code)
		}
	}
}

func TestHubInfoAndDeployForwarding(t *testing.T) {
	h := newHarness(t)
	// info and deploy do not authenticate in the server (the gateway does).
	h.auth.set(nil, auth.ErrUnauthenticated)
	w := h.get("https://backyard.test/__backyard/api/info")
	expectStatus(t, w, 200)
	if w.Body.String() != `{"path":"/info"}` {
		t.Fatalf("info body = %s", w.Body.String())
	}
	w = h.api("POST", "https://backyard.test/__backyard/api/deploy/sites/foo/deploys", map[string]any{})
	expectStatus(t, w, 200)
	w = h.get("https://backyard.test/__backyard/api/deploy")
	expectStatus(t, w, 200)
	if got := h.gw.seen(); !slices.Equal(got, []string{"GET /info", "POST /sites/foo/deploys", "GET /"}) {
		t.Fatalf("gateway saw %v", got)
	}
	// Not on site hosts: there it is just an unknown (authenticated) API path.
	w = h.get("https://foo.backyard.test/__backyard/api/deploy/info")
	expectError(t, w, http.StatusUnauthorized, "unauthenticated")
	h.auth.set(alice, nil)
	w = h.get("https://foo.backyard.test/__backyard/api/deploy/info")
	expectError(t, w, http.StatusNotFound, "not_found")
	if n := len(h.gw.seen()); n != 3 {
		t.Fatalf("gateway called %d times", n)
	}
}

func TestHubDeployCommitPurgesAndCounts(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "v1"})
	if w := h.get("https://foo.backyard.test/"); w.Body.String() != "v1" {
		t.Fatal("setup")
	}
	h.deploy("foo", map[string]string{"index.html": "v2"})
	w := h.api("POST", "https://backyard.test/__backyard/api/deploy/sites/foo/deploys/abc/commit", map[string]any{})
	expectStatus(t, w, 200)
	if w := h.get("https://foo.backyard.test/"); w.Body.String() != "v2" {
		t.Fatalf("cache not purged after commit: %q", w.Body.String())
	}
	h.gw.status = 500
	h.api("POST", "https://backyard.test/__backyard/api/deploy/sites/foo/refresh", map[string]any{})
	metrics := scrape(t, h)
	for _, want := range []string{
		`backyard_deploys_total{method="api",result="ok"} 1`,
		`backyard_deploys_total{method="sync",result="error"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestHubGatewayDisabled(t *testing.T) {
	nilGW := newHarness(t, func(h *harness, d *Deps) { d.Gateway = nil })
	expectError(t, nilGW.get("https://backyard.test/__backyard/api/info"), 404, "not_found")
	expectError(t, nilGW.get("https://backyard.test/__backyard/api/deploy/sites"), 404, "not_found")

	f := false
	off := newHarness(t, withConfig(func(c *config.Config) { c.Deploy.EmbeddedGateway = &f }))
	expectStatus(t, off.get("https://backyard.test/__backyard/api/info"), 200)
	expectError(t, off.get("https://backyard.test/__backyard/api/deploy/sites"), 404, "not_found")
}

func TestHubSitesAndMe(t *testing.T) {
	h := newHarness(t)
	h.deploy("foo", map[string]string{"index.html": "<title>Foo</title>"})
	h.deploy("bar", map[string]string{"index.html": "x"})
	w := h.api("GET", "https://backyard.test/__backyard/api/sites", nil)
	expectStatus(t, w, 200)
	var resp struct {
		Sites []map[string]any `json:"sites"`
	}
	mustJSON(t, w.Body.Bytes(), &resp)
	var names []string
	for _, s := range resp.Sites {
		names = append(names, s["name"].(string))
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"bar", "foo"}) {
		t.Fatalf("sites = %v", names)
	}
	w = h.api("GET", "https://backyard.test/__backyard/api/me", nil)
	expectStatus(t, w, 200)
	if decode[map[string]any](t, w)["id"] != "alice@acme.com" {
		t.Fatalf("me = %s", w.Body.String())
	}
	e := newHarness(t)
	w = e.api("GET", "https://backyard.test/__backyard/api/sites", nil)
	if strings.TrimSpace(w.Body.String()) != `{"sites":[]}` {
		t.Fatalf("empty sites = %s", w.Body.String())
	}
}

func TestHubTokens(t *testing.T) {
	h := newHarness(t, withConfig(func(c *config.Config) { c.Auth.TokenTTL = config.Duration(48 * time.Hour) }))
	tokens := auth.NewTokens(h.cfg.Server.Secret, 0)
	const u = "https://backyard.test/__backyard/api/tokens"

	w := h.api("POST", u, map[string]any{"name": "laptop", "ttl_hours": 24}, "Origin", "https://backyard.test")
	expectStatus(t, w, 200)
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	mustJSON(t, w.Body.Bytes(), &resp)
	user, err := tokens.Verify(resp.Token)
	if err != nil || user.ID != "alice@acme.com" {
		t.Fatalf("token verify: %v %+v", err, user)
	}
	if d := time.Until(resp.ExpiresAt); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("expires in %v", d)
	}
	// Default TTL = auth.token_ttl; empty body accepted.
	w = h.serve(req{method: "POST", url: u, hdr: map[string]string{"X-Backyard": "1"}})
	expectStatus(t, w, 200)
	mustJSON(t, w.Body.Bytes(), &resp)
	if d := time.Until(resp.ExpiresAt); d < 47*time.Hour || d > 49*time.Hour {
		t.Fatalf("default expiry in %v", d)
	}
	// A chunked, empty body is fine too.
	w = h.serve(req{method: "POST", url: u, body: io.NopCloser(strings.NewReader("")), hdr: map[string]string{"X-Backyard": "1"}})
	expectStatus(t, w, 200)
	waitBackground(t, h)
	if !strings.Contains(h.logText(), "token.issue") || h.docs.collectionLen(identity.SystemSite, "_audit") != 3 {
		t.Fatalf("audit missing: %s", h.logText())
	}

	expectError(t, h.api("POST", u, map[string]any{"ttl_hours": 49}), 400, "bad_request")
	expectError(t, h.api("POST", u, map[string]any{"ttl_hours": -1}), 400, "bad_request")
	expectError(t, h.api("POST", u, map[string]any{"name": strings.Repeat("x", 101)}), 400, "bad_request")
	expectError(t, h.api("GET", u, nil), 405, "bad_request")

	h.auth.set(guest, nil)
	expectError(t, h.api("POST", u, map[string]any{}), 403, "forbidden")
	h.auth.set(&identity.User{ID: "token:ci", Name: "ci"}, nil)
	expectError(t, h.api("POST", u, map[string]any{}), 403, "forbidden")

	n := newHarness(t, func(h *harness, d *Deps) { d.Tokens = nil })
	n.srv.deps.Tokens = nil
	expectError(t, n.api("POST", u, map[string]any{}), 501, "unsupported")
}

// TestHubTokensCannotRenewThemselves: with the real authenticator, a token
// minted by a request that presents a Backyard API token never outlives that
// token (no self-renewal, also not by chaining), while a proxy identity (a
// fresh IdP authentication) still gets the full auth.token_ttl.
func TestHubTokensCannotRenewThemselves(t *testing.T) {
	h := newHarness(t, func(h *harness, d *Deps) {
		h.cfg.Auth.Mode = "header"
		h.cfg.Auth.Header.EmailHeader = "X-Email"
		h.cfg.Auth.AdminGroups = []string{"backyard-admins"}
		h.cfg.Server.TrustedProxies = []string{"192.0.2.0/24"} // httptest's RemoteAddr
		a, err := auth.New(t.Context(), h.cfg, auth.Options{})
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
		d.Auth = a
	})
	tokens := auth.NewTokens(h.cfg.Server.Secret, 0)
	const u = "https://backyard.test/__backyard/api/tokens"
	type minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	mint := func(body any, hdr ...string) minted {
		t.Helper()
		w := h.api("POST", u, body, hdr...)
		expectStatus(t, w, 200)
		var m minted
		mustJSON(t, w.Body.Bytes(), &m)
		if _, err := tokens.Verify(m.Token); err != nil {
			t.Fatalf("minted token does not verify: %v", err)
		}
		return m
	}

	// A proxy identity is authenticated by the IdP: full lifetime.
	full := mint(map[string]any{}, "X-Email", "alice@acme.com")
	if d := time.Until(full.ExpiresAt); d < 719*time.Hour {
		t.Fatalf("proxy identity: token expires in %v, want the full 720h", d)
	}

	// A one-hour token, carrying a group that grants admin.
	short, shortExp, err := tokens.Issue(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice",
		Groups: []string{"backyard-admins"}}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []any{map[string]any{}, map[string]any{"ttl_hours": 720}, map[string]any{"ttl_hours": 24}} {
		m := mint(body, "Authorization", "Bearer "+short)
		if m.ExpiresAt.After(shortExp) || time.Until(m.ExpiresAt) < 55*time.Minute {
			t.Fatalf("%v: minted token expires %v, presented token %v", body, m.ExpiresAt, shortExp)
		}
		// Chaining does not extend it either.
		next := mint(map[string]any{}, "Authorization", "Bearer "+m.Token)
		if next.ExpiresAt.After(shortExp) {
			t.Fatalf("chained token expires %v, after the original %v", next.ExpiresAt, shortExp)
		}
	}
	// A shorter request is honoured as is.
	if m := mint(map[string]any{"ttl_hours": 0.5}, "Authorization", "Bearer "+short); time.Until(m.ExpiresAt) > 31*time.Minute {
		t.Fatalf("ttl_hours 0.5: expires in %v", time.Until(m.ExpiresAt))
	}
	// The bearer token is the request's credential even when proxy headers
	// are present too.
	if m := mint(map[string]any{}, "Authorization", "Bearer "+short, "X-Email", "alice@acme.com"); m.ExpiresAt.After(shortExp) {
		t.Fatalf("bearer + proxy headers: expires %v, after %v", m.ExpiresAt, shortExp)
	}

	// A token about to expire cannot mint at all.
	dying, _, err := tokens.Issue(alice, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	expectError(t, h.api("POST", u, map[string]any{}, "Authorization", "Bearer "+dying), 401, "unauthenticated")

	waitBackground(t, h)
	if !strings.Contains(h.logText(), "from_api_token=true") {
		t.Fatalf("audit does not record the bounded issue: %s", h.logText())
	}
}

func TestPresentedAPIToken(t *testing.T) {
	tokens := auth.NewTokens(testSecret, 0)
	tok, exp, err := tokens.Issue(alice, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	jwt := func(claims string) string {
		enc := base64.RawURLEncoding.EncodeToString
		return enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(claims)) + ".sig"
	}
	tests := []struct {
		name    string
		authz   string
		user    *identity.User
		bounded bool
		exp     time.Time
	}{
		{"api token", "Bearer " + tok, alice, true, exp},
		{"scheme case", "bearer  " + tok + " ", alice, true, exp},
		{"other user", "Bearer " + tok, &identity.User{ID: "bob@acme.com"}, false, time.Time{}},
		{"no header", "", alice, false, time.Time{}},
		{"basic auth", "Basic " + tok, alice, false, time.Time{}},
		{"idp jwt", "Bearer " + jwt(`{"iss":"https://idp.example","sub":"alice@acme.com","exp":4102444800}`), alice, false, time.Time{}},
		{"static token", "Bearer s3cr3t-static", alice, false, time.Time{}},
		{"garbage payload", "Bearer a.%%%.c", alice, false, time.Time{}},
		{"no exp", "Bearer " + jwt(`{"iss":"backyard","sub":"alice@acme.com"}`), alice, true, time.Unix(0, 0)},
		{"nil user", "Bearer " + tok, nil, false, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "https://backyard.test/__backyard/api/tokens", nil)
			if tt.authz != "" {
				r.Header.Set("Authorization", tt.authz)
			}
			got, ok := presentedAPIToken(r, tt.user)
			if ok != tt.bounded || !got.Equal(tt.exp) {
				t.Fatalf("presentedAPIToken = %v, %v; want %v, %v", got, ok, tt.exp, tt.bounded)
			}
		})
	}
}

// useDocs substitutes the embedded skill documents for one test.
func useDocs(t *testing.T, skill func() string, ref func(string) string) {
	t.Helper()
	oldSkill, oldRef := skillMarkdown, skillReference
	skillMarkdown, skillReference = skill, ref
	t.Cleanup(func() { skillMarkdown, skillReference = oldSkill, oldRef })
}

func TestHubDocsReferencesDirAndLocalize(t *testing.T) {
	useDocs(t,
		func() string {
			return "# Skill\nHub: " + skills.HubURLPlaceholder + "/docs\nSite: https://my-app.backyard.example.com/\nSee [API](references/api.md).\n"
		},
		func(name string) string {
			if name == "api" {
				return "# API\nPOST " + skills.HubURLPlaceholder + "/__backyard/api/deploy\nSee [deploy](deploy.md).\n"
			}
			return ""
		})
	h := newHarness(t)
	tests := []struct {
		name   string
		url    string
		status int
		want   []string
	}{
		{"skill localized", "https://backyard.test/docs/skill.md", 200,
			[]string{"Hub: https://backyard.test/docs\n", "Site: https://my-app.backyard.test/\n", "(references/api.md)"}},
		{"skill upper", "https://backyard.test/docs/SKILL.md", 200, []string{"Hub: https://backyard.test/docs\n"}},
		{"reference", "https://backyard.test/docs/api.md", 200, []string{"POST https://backyard.test/__backyard/api/deploy\n"}},
		{"reference dir", "https://backyard.test/docs/references/api.md", 200,
			[]string{"POST https://backyard.test/__backyard/api/deploy\n", "(deploy.md)"}},
		{"skill is no reference", "https://backyard.test/docs/references/skill.md", 404, []string{"no such document"}},
		{"missing reference", "https://backyard.test/docs/references/nope.md", 404, []string{"no such document"}},
		{"nested", "https://backyard.test/docs/references/x/api.md", 404, nil},
		{"double prefix", "https://backyard.test/docs/references/references/api.md", 404, nil},
		{"other dir", "https://backyard.test/docs/other/api.md", 404, nil},
		{"dotted", "https://backyard.test/docs/references/a.b.md", 404, nil},
		{"dot dot", "https://backyard.test/docs/references/...md", 404, nil},
		{"encoded traversal", "https://backyard.test/docs/references/..%2fapi.md", 301, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := h.get(tc.url)
			expectStatus(t, w, tc.status)
			body := w.Body.String()
			if tc.status == 200 {
				if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
					t.Fatalf("Content-Type = %q", ct)
				}
				if strings.Contains(body, skills.HubURLPlaceholder) || strings.Contains(body, "backyard.example.com") {
					t.Fatalf("placeholder not localized: %q", body)
				}
			}
			for _, s := range tc.want {
				if !strings.Contains(body, s) {
					t.Fatalf("body = %q, want %q", body, s)
				}
			}
		})
	}
	// The traversal redirect never leaves /docs/references/ for a document.
	if loc := h.get("https://backyard.test/docs/references/..%2fapi.md").Header().Get("Location"); loc != "/docs/api.md" {
		t.Fatalf("traversal Location = %q", loc)
	}
}

// docLinkTargetRe matches relative markdown links to sibling documents.
var docLinkTargetRe = regexp.MustCompile(`\]\(((?:\./)?(?:references/)?[A-Za-z0-9_-]+\.md)(?:#[^)\s]*)?\)`)

// TestHubDocsRelativeLinksResolve follows every relative link between the
// real embedded documents as a browser or agent would (relative to the
// document's URL) and expects each one to be served, localized.
func TestHubDocsRelativeLinksResolve(t *testing.T) {
	useDocs(t, skills.SkillMarkdown, skills.Reference)
	h := newHarness(t)
	start := []string{"/docs/skill.md"}
	for _, name := range skills.References() {
		start = append(start, "/docs/"+name+".md", "/docs/references/"+name+".md")
	}
	seen := map[string]bool{}
	queue := slices.Clone(start)
	links := 0
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		w := h.get("https://backyard.test" + p)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", p, w.Code)
			continue
		}
		if strings.Contains(w.Body.String(), skills.HubURLPlaceholder) {
			t.Errorf("GET %s: hub placeholder not localized", p)
		}
		base, err := url.Parse("https://backyard.test" + p)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range docLinkTargetRe.FindAllStringSubmatch(w.Body.String(), -1) {
			ref, err := url.Parse(m[1])
			if err != nil {
				t.Fatal(err)
			}
			links++
			queue = append(queue, base.ResolveReference(ref).Path)
		}
	}
	if links == 0 {
		t.Fatal("no relative links found in the embedded documents")
	}
	if !seen["/docs/references/api.md"] {
		t.Fatalf("SKILL.md's references/api.md link was not followed: %v", seen)
	}
}
