package app

// End-to-end tests of the Claude connector (SPEC §8.4, §14.1) with the
// go-sdk MCP client: discovery from the 401 challenge, registration (DCR or
// a client ID metadata document), authorize → consent → code, the token
// exchange with PKCE and resource, tool calls over the stateless endpoint,
// refresh, and disconnecting on the hub.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/mcpremote"
)

const (
	flowRedirect = "http://127.0.0.1:9/callback"
	// pngB64 is a 1×1 PNG.
	pngB64       = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	modeNoneWarn = "mcp: auth.mode none: every connector connection is an anonymous guest"
)

// mcpFlowConfig is a production-like (not dev) hub on 127.0.0.1:port in
// path mode, auth.mode none with anonymous deploys, sqlite and fs storage,
// and the connector enabled.
func mcpFlowConfig(t *testing.T, port string, mutate func(*config.Config)) *config.Config {
	t.Helper()
	t.Setenv("BACKYARD_CONFIG_YAML", "")
	t.Setenv("BACKYARD_CONFIG", "")
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Server.Listen = "127.0.0.1:" + port
	cfg.Server.BaseDomain = "127.0.0.1"
	cfg.Server.PublicURL = "http://127.0.0.1:" + port
	cfg.Server.URLMode = "path"
	cfg.Server.Secret = "flow-test-secret-0123456789abcdef0123456789"
	cfg.Auth.Mode = "none"
	cfg.Deploy.AllowAnonymous = true
	cfg.Storage.Type = "fs"
	cfg.Storage.FS.Root = filepath.Join(dir, "data")
	cfg.Database.Type = "sqlite"
	cfg.Database.SQLite.Path = filepath.Join(dir, "db", "backyard.db")
	cfg.PubSub.Type = "memory"
	cfg.Telemetry.Metrics = true
	cfg.MCP.Enabled = true
	if mutate != nil {
		mutate(cfg)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// startApp runs an App like start and returns it with its base URL.
func startApp(t *testing.T, cfg *config.Config, opts Options) (*App, string) {
	t.Helper()
	ln, err := net.Listen("tcp", cfg.Server.Listen)
	if err != nil {
		t.Fatal(err)
	}
	opts.Listener = ln
	listening := make(chan struct{})
	opts.OnListen = func(net.Addr) { close(listening) }
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, cfg, opts)
	if err != nil {
		cancel()
		_ = ln.Close()
		t.Fatalf("New: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("Run did not return after cancel")
		}
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	select {
	case <-listening:
	case err := <-done:
		done <- nil // for the cleanup
		t.Fatalf("Run: %v", err)
	}
	return a, "http://" + ln.Addr().String()
}

// logBuffer collects log output from concurrent goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// flowClock is the authorization server's clock.
type flowClock struct{ ns atomic.Int64 }

func newFlowClock() *flowClock {
	c := &flowClock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}

func (c *flowClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *flowClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// browser is the person's browser: a cookie jar, no automatic redirects.
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
	calls  atomic.Int32 // authorizations driven
	mu     sync.Mutex
	pages  []string // consent pages seen
}

func newBrowser(t *testing.T, base string) *browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{t: t, base: base, client: &http.Client{
		Jar:           jar,
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

var hiddenInputRe = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// fetcher is the go-sdk AuthorizationCodeFetcher: it opens the
// authorization URL, approves the consent page like a person would and
// returns the parameters of the redirect to the app (which nobody listens
// on).
func (b *browser) fetcher(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
	b.calls.Add(1)
	if !strings.HasPrefix(args.URL, b.base+auth.AuthorizePath+"?") {
		b.t.Errorf("authorization URL = %q", args.URL)
	}
	u, _ := url.Parse(args.URL)
	if q := u.Query(); q.Get("resource") != b.base+auth.MCPPath || q.Get("code_challenge_method") != "S256" {
		b.t.Errorf("authorization request = %v", q)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, args.URL, nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Dest", "document")
	res, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	page, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b.t.Errorf("consent page: %d %s", res.StatusCode, page)
		return nil, io.ErrUnexpectedEOF
	}
	b.mu.Lock()
	b.pages = append(b.pages, string(page))
	b.mu.Unlock()
	form := url.Values{}
	for _, m := range hiddenInputRe.FindAllStringSubmatch(string(page), -1) {
		form.Add(m[1], html.UnescapeString(m[2]))
	}
	form.Set("decision", "allow")
	post, _ := http.NewRequestWithContext(ctx, http.MethodPost, b.base+auth.AuthorizePath, strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("Sec-Fetch-Site", "same-origin")
	post.Header.Set("Sec-Fetch-Mode", "navigate")
	post.Header.Set("Sec-Fetch-Dest", "document")
	post.Header.Set("Origin", b.base)
	res, err = b.client.Do(post)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	loc, err := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusSeeOther || err != nil || loc.Query().Get("code") == "" {
		b.t.Errorf("consent answer: %d %q", res.StatusCode, res.Header.Get("Location"))
		return nil, io.ErrUnexpectedEOF
	}
	q := loc.Query()
	return &sdkauth.AuthorizationResult{Code: q.Get("code"), State: q.Get("state"), Iss: q.Get("iss")}, nil
}

// lastPage returns the last consent page.
func (b *browser) lastPage() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pages) == 0 {
		return ""
	}
	return b.pages[len(b.pages)-1]
}

// do sends a hub request with the browser's cookies.
func (b *browser) do(method, path string, hdr map[string]string) (int, []byte) {
	b.t.Helper()
	req, _ := http.NewRequest(method, b.base+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := b.client.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, body
}

// tokenSource lets a test make the client see its access token as expired,
// so its next request refreshes. It records the tokens it hands out.
type tokenSource struct {
	mu        sync.Mutex
	cfg       *oauth2.Config
	ctx       context.Context
	src       oauth2.TokenSource
	tok       *oauth2.Token
	expire    bool
	refreshes int
}

// newSource is AuthorizationCodeHandlerConfig.NewTokenSource.
func (s *tokenSource) newSource(ctx context.Context, cfg *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg, s.ctx, s.tok = cfg, ctx, tok
	s.src = cfg.TokenSource(ctx, tok)
	return s, nil
}

func (s *tokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expire {
		s.expire = false
		stale := *s.tok
		stale.Expiry = time.Now().Add(-time.Hour)
		s.src = s.cfg.TokenSource(s.ctx, &stale)
	}
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	if t.AccessToken != s.tok.AccessToken {
		s.refreshes++
	}
	s.tok = t
	return t, nil
}

func (s *tokenSource) current() oauth2.Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.tok
}

// callTool calls a tool and returns the text of a successful result.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	if args == nil {
		args = map[string]any{} // a nil map would be sent as "arguments": null
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("%s: content %T", name, res.Content[0])
	}
	if res.IsError {
		t.Fatalf("%s failed: %s", name, text.Text)
	}
	return text.Text
}

// rawMCP posts a JSON-RPC ping with a bearer token.
func rawMCP(t *testing.T, base, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+auth.MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode, res.Header.Get("WWW-Authenticate")
}

// fetchBytes GETs a URL.
func fetchBytes(t *testing.T, u string) (int, string, []byte) {
	t.Helper()
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header.Get("Content-Type"), b
}

// catalogNames returns the tool names of tools.json, in order.
func catalogNames(t *testing.T) []string {
	t.Helper()
	var cat struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(mcpremote.ToolsJSON(), &cat); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tl := range cat.Tools {
		out = append(out, tl.Name)
	}
	return out
}

// dcrHandler is a go-sdk OAuth handler that registers dynamically (as
// "Flow test", loopback redirect) and asks for a refresh token.
func dcrHandler(t *testing.T, b *browser, ts *tokenSource) *sdkauth.AuthorizationCodeHandler {
	t.Helper()
	h, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &sdkauth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName:              "Flow test",
			RedirectURIs:            []string{flowRedirect},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			TokenEndpointAuthMethod: "none",
		}},
		RedirectURL:              flowRedirect,
		AuthorizationCodeFetcher: b.fetcher,
		RequestRefreshToken:      true,
		NewTokenSource:           ts.newSource,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// connect opens a go-sdk client session over the connector.
func connect(t *testing.T, base string, h *sdkauth.AuthorizationCodeHandler) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "backyard-flow-test", Version: "1.0.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: base + auth.MCPPath, OAuthHandler: h, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestMCPFlowDCR(t *testing.T) {
	port := freePort(t)
	cfg := mcpFlowConfig(t, port, nil)
	logs := &logBuffer{}
	clock := newFlowClock()
	a, base := startApp(t, cfg, Options{
		Logger:      slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		authOptions: func(o *auth.Options) { o.Now = clock.Now },
	})
	if !strings.Contains(logs.String(), modeNoneWarn) {
		t.Errorf("no mode-none warning in:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "mcp_url="+base+auth.MCPPath) {
		t.Errorf("the startup line lacks mcp_url:\n%s", logs.String())
	}

	// The hub advertises the connector.
	code, body := get(t, base+"/__backyard/api/info", "")
	var info struct {
		MCPURL string `json:"mcp_url"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &info) != nil || info.MCPURL != base+auth.MCPPath {
		t.Fatalf("info: %d %s", code, body)
	}

	b := newBrowser(t, base)
	ts := &tokenSource{}
	cs := connect(t, base, dcrHandler(t, b, ts))

	// The SDK's 2026-07-28 probe is refused; it falls back to initialize.
	if v := cs.InitializeResult().ProtocolVersion; v != "2025-11-25" {
		t.Errorf("negotiated protocol %q", v)
	}
	if si := cs.InitializeResult().ServerInfo; si == nil || si.Name != "backyard" {
		t.Errorf("serverInfo = %+v", si)
	}
	if n := b.calls.Load(); n != 1 {
		t.Fatalf("authorizations = %d", n)
	}
	page := b.lastPage()
	for _, want := range []string{"Flow test", "not verified", "an app on this computer", "Guest "} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	tok := ts.current()
	if !strings.HasPrefix(tok.AccessToken, "byat_") || !strings.HasPrefix(tok.RefreshToken, "byrt_") {
		t.Fatalf("tokens = %q / %q", tok.AccessToken[:min(8, len(tok.AccessToken))], tok.RefreshToken[:min(8, len(tok.RefreshToken))])
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	if got, want := strings.Join(names, ","), strings.Join(catalogNames(t), ","); got != want {
		t.Fatalf("tools = %s, want %s", got, want)
	}

	var who struct {
		User struct {
			ID        string `json:"id"`
			Anonymous bool   `json:"anonymous"`
		} `json:"user"`
		HubURL     string `json:"hub_url"`
		Template   string `json:"site_url_template"`
		CanPublish bool   `json:"can_publish"`
		Connection struct {
			Client string `json:"client"`
		} `json:"connection"`
	}
	if err := json.Unmarshal([]byte(callTool(t, cs, "backyard_whoami", nil)), &who); err != nil {
		t.Fatal(err)
	}
	if !who.User.Anonymous || !strings.HasPrefix(who.User.ID, "anon:") || who.HubURL != base || !who.CanPublish ||
		who.Template != base+"/s/{site}/" || who.Connection.Client != "Flow test" {
		t.Fatalf("whoami = %+v", who)
	}

	// Publish, then read the site back over HTTP.
	var pub struct {
		Site     string   `json:"site"`
		URL      string   `json:"url"`
		Created  bool     `json:"created"`
		Files    int      `json:"files"`
		Uploaded int      `json:"uploaded"`
		Warnings []string `json:"warnings"`
	}
	text := callTool(t, cs, "backyard_publish", map[string]any{
		"site": "flow-site",
		"files": []any{
			map[string]any{"path": "index.html", "content": "<!doctype html><title>Flow</title><h1>v1</h1>"},
			map[string]any{"path": "logo.png", "content": pngB64, "encoding": "base64"},
		},
	})
	if err := json.Unmarshal([]byte(text), &pub); err != nil {
		t.Fatal(err)
	}
	if pub.URL != base+"/s/flow-site/" || !pub.Created || pub.Files != 2 || pub.Uploaded != 2 || len(pub.Warnings) != 0 {
		t.Fatalf("publish = %s", text)
	}
	if code, _, got := fetchBytes(t, pub.URL); code != 200 || string(got) != "<!doctype html><title>Flow</title><h1>v1</h1>" {
		t.Fatalf("site: %d %q", code, got)
	}
	png, _ := base64.StdEncoding.DecodeString(pngB64)
	if code, ct, got := fetchBytes(t, pub.URL+"logo.png"); code != 200 || ct != "image/png" || !bytes.Equal(got, png) {
		t.Fatalf("logo: %d %q %d bytes", code, ct, len(got))
	}
	// A change is served at once (the static cache is purged), and the
	// merge keeps the other file.
	text = callTool(t, cs, "backyard_publish", map[string]any{
		"site":  "flow-site",
		"files": []any{map[string]any{"path": "index.html", "content": "<h1>v2</h1>"}},
	})
	if err := json.Unmarshal([]byte(text), &pub); err != nil || pub.Created || pub.Files != 2 || pub.Uploaded != 1 {
		t.Fatalf("second publish = %s", text)
	}
	if code, _, got := fetchBytes(t, pub.URL); code != 200 || string(got) != "<h1>v2</h1>" {
		t.Fatalf("site after the change: %d %q", code, got)
	}
	if !strings.Contains(callTool(t, cs, "backyard_list_sites", nil), `"name":"flow-site"`) {
		t.Error("list_sites lacks the site")
	}
	if got := callTool(t, cs, "backyard_read_site_file", map[string]any{"site": "flow-site", "path": "index.html"}); got != "<h1>v2</h1>" {
		t.Errorf("read_site_file = %q", got)
	}
	rec := httptest.NewRecorder()
	a.MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, want := range []string{
		`backyard_deploys_total{method="mcp",result="ok"} 2`,
		`backyard_http_requests_total{area="mcp",code="200"}`,
		`backyard_http_requests_total{area="mcp",code="401"}`,
		`backyard_http_requests_total{area="oauth",code="201"}`,
		`backyard_http_requests_total{area="auth",code="303"}`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// An hour later the access token has expired; the client refreshes
	// (rotating the refresh token) without asking the person again.
	old := ts.current()
	clock.advance(2 * time.Hour)
	if code, chal := rawMCP(t, base, old.AccessToken); code != 401 || !strings.Contains(chal, `error_description="token expired"`) {
		t.Fatalf("expired token: %d %q", code, chal)
	}
	ts.mu.Lock()
	ts.expire = true
	ts.mu.Unlock()
	callTool(t, cs, "backyard_whoami", nil)
	cur := ts.current()
	if ts.refreshes != 1 || cur.AccessToken == old.AccessToken || cur.RefreshToken == old.RefreshToken || cur.RefreshToken == "" {
		t.Fatalf("refresh: %d refreshes, rotated=%v", ts.refreshes, cur.RefreshToken != old.RefreshToken)
	}
	if n := b.calls.Load(); n != 1 {
		t.Fatalf("the refresh asked the person again (%d authorizations)", n)
	}

	// The person's connection is on the hub (the browser is the same guest);
	// disconnecting it makes the client connect again from scratch.
	code, raw := b.do(http.MethodGet, "/__backyard/api/mcp/grants", nil)
	var grants struct {
		Grants []struct {
			ID           string `json:"id"`
			ClientName   string `json:"client_name"`
			ClientKind   string `json:"client_kind"`
			RedirectHost string `json:"redirect_host"`
			User         struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"grants"`
	}
	if code != 200 || json.Unmarshal(raw, &grants) != nil || len(grants.Grants) != 1 {
		t.Fatalf("grants: %d %s", code, raw)
	}
	g := grants.Grants[0]
	if g.ClientName != "Flow test" || g.ClientKind != "registered" || g.RedirectHost != "127.0.0.1:9" || g.User.ID != who.User.ID {
		t.Fatalf("grant = %+v", g)
	}
	if code, raw := b.do(http.MethodDelete, "/__backyard/api/mcp/grants/"+g.ID, map[string]string{"X-Backyard": "1"}); code != http.StatusNoContent {
		t.Fatalf("disconnect: %d %s", code, raw)
	}
	if code, chal := rawMCP(t, base, cur.AccessToken); code != 401 || !strings.Contains(chal, `error_description="token revoked"`) {
		t.Fatalf("after disconnect: %d %q", code, chal)
	}
	callTool(t, cs, "backyard_whoami", nil)
	if n := b.calls.Load(); n != 2 {
		t.Fatalf("authorizations after disconnecting = %d, want 2", n)
	}
}

func TestMCPFlowCIMD(t *testing.T) {
	// The client's metadata document, served over TLS at a name the test
	// transport resolves to the test server.
	const clientURL = "https://example.com/oauth/backyard-flow-client.json"
	var fetches atomic.Int32
	doc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/backyard-flow-client.json" {
			http.NotFound(w, r)
			return
		}
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "max-age=600")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"client_id":                  clientURL,
			"client_name":                "Flow CIMD app",
			"redirect_uris":              []string{"http://127.0.0.1/callback", "https://evil.example/cb"},
			"grant_types":                []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:jwt-bearer"},
			"token_endpoint_auth_method": "none",
		})
	}))
	defer doc.Close()
	tlsConf := doc.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cimdClient := &http.Client{Transport: &http.Transport{
		TLSClientConfig: tlsConf,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, doc.Listener.Addr().String())
		},
	}}

	port := freePort(t)
	cfg := mcpFlowConfig(t, port, func(c *config.Config) { c.MCP.Clients = []string{clientURL} })
	_, base := startApp(t, cfg, Options{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		authOptions: func(o *auth.Options) { o.MCPHTTPClient = cimdClient },
	})

	code, body := get(t, base+auth.ASMetadataPath, "")
	if code != 200 || !strings.Contains(body, `"client_id_metadata_document_supported":true`) {
		t.Fatalf("AS metadata: %d %s", code, body)
	}

	b := newBrowser(t, base)
	h, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &sdkauth.ClientIDMetadataDocumentConfig{URL: clientURL},
		// The registered loopback redirect has no port: any port matches.
		RedirectURL:              "http://127.0.0.1:53682/callback",
		AuthorizationCodeFetcher: b.fetcher,
		RequestRefreshToken:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, base, h)
	page := b.lastPage()
	for _, want := range []string{"Flow CIMD app", "verified by", "example.com", "http://127.0.0.1:53682"} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	if strings.Contains(page, "not verified") {
		t.Error("a metadata-document client is shown as unverified")
	}
	var who struct {
		Connection struct {
			Client string `json:"client"`
		} `json:"connection"`
	}
	if err := json.Unmarshal([]byte(callTool(t, cs, "backyard_whoami", nil)), &who); err != nil || who.Connection.Client != "Flow CIMD app" {
		t.Fatalf("whoami connection = %+v (%v)", who.Connection, err)
	}
	text := callTool(t, cs, "backyard_publish", map[string]any{
		"site":  "cimd-site",
		"files": []any{map[string]any{"path": "index.html", "content": "cimd"}},
	})
	if !strings.Contains(text, `"url":"`+base+`/s/cimd-site/"`) {
		t.Fatalf("publish = %s", text)
	}
	if code, _, got := fetchBytes(t, base+"/s/cimd-site/"); code != 200 || string(got) != "cimd" {
		t.Fatalf("site: %d %q", code, got)
	}

	code, raw := b.do(http.MethodGet, "/__backyard/api/mcp/grants", nil)
	var grants struct {
		Grants []struct {
			ClientKind   string `json:"client_kind"`
			ClientHost   string `json:"client_host"`
			RedirectHost string `json:"redirect_host"`
		} `json:"grants"`
	}
	if code != 200 || json.Unmarshal(raw, &grants) != nil || len(grants.Grants) != 1 {
		t.Fatalf("grants: %d %s", code, raw)
	}
	if g := grants.Grants[0]; g.ClientKind != "metadata_document" || g.ClientHost != "example.com" || g.RedirectHost != "127.0.0.1:53682" {
		t.Fatalf("grant = %+v", g)
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("client metadata fetched %d times, want 1 (cached)", n)
	}
}

// TestMCPFlowUnlistedCIMD: an app on the user's computer that names itself
// by a client metadata document no deployment lists (opencode 2.x, Codex
// CLI, Hermes Agent) connects with the default mcp.clients: its URL is the
// client_id, the sign-in returns to a loopback address, and the document is
// never fetched (SPEC §8.4.3).
func TestMCPFlowUnlistedCIMD(t *testing.T) {
	const clientURL = "https://opencode.ai/oauth/opencode/client.json"
	var fetches atomic.Int32
	cimdClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		fetches.Add(1)
		return nil, io.ErrUnexpectedEOF
	})}
	port := freePort(t)
	_, base := startApp(t, mcpFlowConfig(t, port, nil), Options{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		authOptions: func(o *auth.Options) { o.MCPHTTPClient = cimdClient },
	})

	b := newBrowser(t, base)
	h, err := sdkauth.NewAuthorizationCodeHandler(&sdkauth.AuthorizationCodeHandlerConfig{
		ClientIDMetadataDocumentConfig: &sdkauth.ClientIDMetadataDocumentConfig{URL: clientURL},
		RedirectURL:                    "http://127.0.0.1:53682/callback",
		AuthorizationCodeFetcher:       b.fetcher,
		RequestRefreshToken:            true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, base, h)
	page := b.lastPage()
	if !strings.Contains(page, "<strong>"+clientURL+"</strong> (address provided by the app, not verified)") ||
		!strings.Contains(page, "an app on this computer (http://127.0.0.1:53682)") || strings.Contains(page, "verified by") {
		t.Errorf("consent page:\n%s", page)
	}
	var who struct {
		Connection struct {
			Client string `json:"client"`
		} `json:"connection"`
	}
	if err := json.Unmarshal([]byte(callTool(t, cs, "backyard_whoami", nil)), &who); err != nil || who.Connection.Client != clientURL {
		t.Fatalf("whoami connection = %+v (%v)", who.Connection, err)
	}

	code, raw := b.do(http.MethodGet, "/__backyard/api/mcp/grants", nil)
	var grants struct {
		Grants []struct {
			ClientName   string `json:"client_name"`
			ClientKind   string `json:"client_kind"`
			ClientHost   string `json:"client_host"`
			RedirectHost string `json:"redirect_host"`
		} `json:"grants"`
	}
	if code != 200 || json.Unmarshal(raw, &grants) != nil || len(grants.Grants) != 1 {
		t.Fatalf("grants: %d %s", code, raw)
	}
	if g := grants.Grants[0]; g.ClientName != clientURL || g.ClientKind != "unlisted_metadata_document" || g.ClientHost != "" ||
		g.RedirectHost != "127.0.0.1:53682" {
		t.Fatalf("grant = %+v (%s)", g, raw)
	}
	if !strings.Contains(string(raw), `"client_host":""`) {
		t.Errorf("client_host is not an empty string: %s", raw)
	}
	if n := fetches.Load(); n != 0 {
		t.Errorf("the unlisted client metadata document was fetched %d times", n)
	}
}

// roundTripFunc is an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestMCPFlowLimits: the connector without the hub's embedded gateway can
// read but not publish, and the endpoint charges api_per_minute per user.
func TestMCPFlowLimits(t *testing.T) {
	port := freePort(t)
	cfg := mcpFlowConfig(t, port, func(c *config.Config) {
		off := false
		c.Deploy.EmbeddedGateway = &off
		c.RateLimit.APIPerMinute = 20
	})
	_, base := startApp(t, cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	code, body := get(t, base+"/__backyard/api/info", "")
	if code != 200 || !strings.Contains(body, `"mcp_url":"`+base+auth.MCPPath+`"`) {
		t.Fatalf("info: %d %s", code, body)
	}
	ts := &tokenSource{}
	cs := connect(t, base, dcrHandler(t, newBrowser(t, base), ts))
	if !strings.Contains(callTool(t, cs, "backyard_whoami", nil), `"can_publish":false`) {
		t.Error("whoami: can_publish without the embedded gateway")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "backyard_publish", Arguments: map[string]any{
		"site": "nope", "files": []any{map[string]any{"path": "index.html", "content": "x"}},
	}})
	if err != nil || !res.IsError || len(res.Content) != 1 ||
		!strings.Contains(res.Content[0].(*mcp.TextContent).Text, "deploy.embedded_gateway: false") {
		t.Fatalf("publish without the gateway: %v %+v", err, res)
	}

	// The per-user budget runs out (the session used part of it already).
	tok := ts.current().AccessToken
	for i := 0; ; i++ {
		req, _ := http.NewRequest(http.MethodPost, base+auth.MCPPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			if i >= 20 {
				t.Fatal("no per-user limit")
			}
			continue
		}
		if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" ||
			!strings.Contains(string(b), `"code":"rate_limited"`) {
			t.Fatalf("limited: %d %v %s", res.StatusCode, res.Header, b)
		}
		break
	}
	// The per-IP budget of the OAuth endpoints is separate.
	reg, err := http.Post(base+auth.OAuthRegisterPath, "application/json",
		strings.NewReader(`{"client_name":"x","redirect_uris":["`+flowRedirect+`"]}`))
	if err != nil {
		t.Fatal(err)
	}
	reg.Body.Close()
	if reg.StatusCode != http.StatusCreated {
		t.Errorf("register after the per-user limit: %d", reg.StatusCode)
	}
}

func TestMCPDisabled(t *testing.T) {
	port := freePort(t)
	cfg := devConfig(t, port)
	logs := &logBuffer{}
	base := start(t, cfg, Options{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	code, body := get(t, base+"/__backyard/api/info", "")
	if code != 200 || strings.Contains(body, "mcp_url") {
		t.Fatalf("info: %d %s", code, body)
	}
	for _, p := range []string{auth.ASMetadataPath, auth.ResourceMetadataPath} {
		if code, body := get(t, base+p, ""); code != 404 || !strings.Contains(body, "not enabled") {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	res, err := http.Post(base+auth.MCPPath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 404 || !strings.Contains(string(b), "the Claude connector is not enabled on this server") {
		t.Errorf("mcp: %d %s", res.StatusCode, b)
	}
	if strings.Contains(logs.String(), modeNoneWarn) {
		t.Error("mode-none warning without the connector")
	}
}

func TestMCPModeNoneWarningOnlyOutsideDev(t *testing.T) {
	cfg := devConfig(t, freePort(t))
	cfg.MCP.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	logs := &logBuffer{}
	a, err := New(context.Background(), cfg, Options{Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if strings.Contains(logs.String(), modeNoneWarn) {
		t.Error("mode-none warning in dev mode")
	}
}
