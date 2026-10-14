package cli

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/cli/clitest"
	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
)

// fakeJWT builds an unsigned JWT with claims (the fake gateway does not verify).
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestLoginBrowserLoopback(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.Tokens[g.CLIToken] = alice
	var (
		mu     sync.Mutex
		status int
		page   string
	)
	h.openFn = func(u string) error {
		// Simulate the browser: the server's /auth/cli redirects to the
		// loopback callback, which shows the success page.
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		mu.Lock()
		status, page = resp.StatusCode, string(b)
		mu.Unlock()
		return nil
	}
	out := h.mustRun("login", "--server", g.URL)
	if !strings.Contains(out, "Logged in to "+g.URL+" as Alice Smith <alice@acme.com>") {
		t.Fatalf("stdout = %s", out)
	}
	mu.Lock()
	if status != 200 || !strings.Contains(page, "You are signed in") || strings.Contains(page, g.CLIToken) {
		t.Fatalf("callback page: %d %s", status, page)
	}
	mu.Unlock()

	opened := h.openedURLs()
	if len(opened) != 1 {
		t.Fatalf("opened = %v", opened)
	}
	lu, _ := url.Parse(opened[0])
	if lu.Path != "/__backyard/auth/cli" || len(lu.Query().Get("state")) != 32 ||
		!strings.HasPrefix(lu.Query().Get("redirect_uri"), "http://127.0.0.1:") || !strings.HasSuffix(lu.Query().Get("redirect_uri"), "/callback") {
		t.Fatalf("login URL = %s", opened[0])
	}

	f := h.loadConfig()
	p := f.Profile("default")
	if f.Current != "default" || p == nil || p.Token != g.CLIToken || !p.TokenExpiresAt.Equal(g.CLITokenExpiry) ||
		p.Login != clientconfig.LoginBrowser || p.User == nil || p.User.ID != alice.ID || p.Server != g.URL || p.Gateway != "" || p.Info == nil {
		t.Fatalf("stored profile = %+v (current %q)", p, f.Current)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(h.configPath)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("config mode = %v %v", st.Mode().Perm(), err)
		}
	}

	// whoami uses the stored credentials.
	out = h.mustRun("whoami")
	for _, want := range []string{"Alice Smith <alice@acme.com>", "Server:   " + g.URL, "Profile:  default", "browser, token expires"} {
		if !strings.Contains(out, want) {
			t.Errorf("whoami lacks %q:\n%s", want, out)
		}
	}
}

func TestLoginLoopbackRejectsForgedCallback(t *testing.T) {
	h := newHarness(t)
	h.timeout = 500 * time.Millisecond
	g := clitest.NewGateway(t)
	var forged int
	h.openFn = func(u string) error {
		lu, _ := url.Parse(u)
		cb := lu.Query().Get("redirect_uri") + "?token=evil&state=not-the-state"
		resp, err := http.Get(cb)
		if err != nil {
			return err
		}
		resp.Body.Close()
		forged = resp.StatusCode
		return nil
	}
	errOut := h.mustFail("login", "--server", g.URL)
	if forged != http.StatusBadRequest || !strings.Contains(errOut, "timed out") {
		t.Fatalf("forged=%d stderr=%s", forged, errOut)
	}
	if p := h.loadConfig().Profile("default"); p != nil {
		t.Fatalf("profile stored after failed login: %+v", p)
	}
}

func TestLoginWithToken(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	jwt := fakeJWT(t, map[string]any{"sub": "alice", "exp": exp.Unix()})
	g.Tokens[jwt] = alice
	g.Tokens["static-ci"] = alice

	h.mustRun("login", "--server", g.URL, "--token", jwt)
	p := h.loadConfig().Profile("default")
	if p.Token != jwt || !p.TokenExpiresAt.Equal(exp) || p.Login != clientconfig.LoginToken {
		t.Fatalf("profile = %+v", p)
	}
	if len(h.openedURLs()) != 0 {
		t.Fatal("token login opened a browser")
	}
	// Rejected token: nothing stored.
	errOut := h.mustFail("login", "--server", g.URL, "--token", "wrong", "--profile", "other")
	if !strings.Contains(errOut, "rejected") || h.loadConfig().Profile("other") != nil {
		t.Fatalf("stderr = %s", errOut)
	}
	// Token from stdin into a second profile, which becomes current.
	h.stdin = "static-ci\n"
	h.mustRun("login", "--server", g.URL, "--token", "-", "--profile", "ci", "--json")
	var res loginResult
	if err := json.Unmarshal([]byte(h.out.String()), &res); err != nil {
		t.Fatal(err)
	}
	f := h.loadConfig()
	if res.Profile != "ci" || res.User.ID != alice.ID || f.Current != "ci" || f.Profile("ci").Token != "static-ci" || f.Profile("default").Token != jwt {
		t.Fatalf("result=%+v current=%q", res, f.Current)
	}
}

func TestLoginNoBrowserPaste(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.Tokens["pasted"] = alice
	h.stdin = "  pasted  \n"
	h.mustRun("login", "--server", g.URL, "--no-browser")
	if !strings.Contains(h.err.String(), "/__backyard/auth/cli?mode=display") {
		t.Fatalf("stderr = %s", h.err.String())
	}
	if len(h.openedURLs()) != 0 {
		t.Fatal("--no-browser opened a browser")
	}
	if p := h.loadConfig().Profile("default"); p.Token != "pasted" || p.Login != clientconfig.LoginBrowser {
		t.Fatalf("profile = %+v", p)
	}
}

func TestLoginAnonymousServer(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.Anonymous = true
	g.Info.Auth = deploy.AuthInfo{Mode: "none", AnonymousDeploy: true}
	out := h.mustRun("login", "--server", g.URL)
	if !strings.Contains(out, "allows anonymous deploys") {
		t.Fatalf("stdout = %s", out)
	}
	p := h.loadConfig().Profile("default")
	if p.Token != "" || p.Login != clientconfig.LoginAnonymous || p.Server != g.URL || p.User != nil {
		t.Fatalf("profile = %+v", p)
	}
	out = h.mustRun("whoami")
	if !strings.Contains(out, "(anonymous)") || !strings.Contains(out, "Login:    anonymous") {
		t.Fatalf("whoami = %s", out)
	}
}

func TestLoginBehindProxyExplainsGateway(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.HTML = true
	errOut := h.mustFail("login", "--server", g.URL)
	if !strings.Contains(errOut, "identity-aware proxy") || !strings.Contains(errOut, "--gateway") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestLoginWithExplicitGateway(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.Tokens["tok"] = alice
	hub := "https://backyard.example.invalid"
	h.mustRun("login", "--server", hub, "--gateway", g.GatewayURL(), "--token", "tok")
	p := h.loadConfig().Profile("default")
	if p.Server != hub || p.Gateway != g.GatewayURL() {
		t.Fatalf("profile = %+v", p)
	}
	out := h.mustRun("whoami")
	if !strings.Contains(out, "Gateway:  "+g.GatewayURL()) {
		t.Fatalf("whoami = %s", out)
	}
}

func TestLoginAdoptsAdvertisedGateway(t *testing.T) {
	h := newHarness(t)
	hub := clitest.NewGateway(t)
	ext := clitest.NewGateway(t)
	ext.Tokens["tok"] = alice
	hub.Info.GatewayURL = ext.GatewayURL()
	h.mustRun("login", "--server", hub.URL, "--token", "tok")
	if p := h.loadConfig().Profile("default"); p.Gateway != ext.GatewayURL() || p.Server != hub.URL {
		t.Fatalf("profile = %+v", p)
	}
	// An unreachable advertised gateway is ignored.
	h2 := newHarness(t)
	hub2 := clitest.NewGateway(t)
	hub2.Tokens["tok"] = alice
	hub2.Info.GatewayURL = "http://127.0.0.1:1/gw"
	h2.mustRun("login", "--server", hub2.URL, "--token", "tok")
	if p := h2.loadConfig().Profile("default"); p.Gateway != "" {
		t.Fatalf("profile = %+v", p)
	}
}

func TestLoginWithoutServer(t *testing.T) {
	h := newHarness(t)
	if errOut := h.mustFail("login"); !strings.Contains(errOut, "--server") {
		t.Fatalf("stderr = %s", errOut)
	}
	// Interactive: the server is asked for.
	g := clitest.NewGateway(t)
	g.Tokens["tok"] = alice
	h.stdinTTY, h.stdin = true, strings.TrimPrefix(g.URL, "http://")+"\n"
	h.mustRun("login", "--token", "tok")
	if p := h.loadConfig().Profile("default"); p.Server != g.URL {
		t.Fatalf("profile = %+v", p)
	}
}

// fakeIdP is a minimal OIDC provider for the device flow.
func fakeIdP(t *testing.T, idToken func() string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	var mu sync.Mutex
	polls := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL,
				"device_authorization_endpoint": srv.URL + "/device", "token_endpoint": srv.URL + "/token"})
		case "/device":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "WXYZ-1234",
				"verification_uri": srv.URL + "/activate", "verification_uri_complete": srv.URL + "/activate?code=WXYZ-1234",
				"expires_in": 600, "interval": 1})
		case "/token":
			_ = r.ParseForm()
			mu.Lock()
			defer mu.Unlock()
			if r.Form.Get("grant_type") == "refresh_token" {
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque2", "id_token": idToken(), "refresh_token": "refresh-2"})
				return
			}
			polls++
			if polls < 2 {
				w.WriteHeader(400)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "opaque", "id_token": idToken(), "refresh_token": "refresh-1", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginDeviceFlowAndAutoRefresh(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	var mu sync.Mutex
	n := 0
	tokens := map[string]bool{}
	idToken := func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		tok := fakeJWT(t, map[string]any{"aud": "cli-client", "n": n, "exp": h.now.Add(time.Hour).Unix()})
		g.AddToken(tok, alice)
		tokens[tok] = true
		return tok
	}
	idp := fakeIdP(t, idToken)
	g.Info.Auth = deploy.AuthInfo{Mode: "oidc", Device: &deploy.DeviceInfo{Issuer: idp.URL, ClientID: "cli-client", Scopes: []string{"openid", "offline_access"}}}

	h.mustRun("login", "--server", g.URL, "--device")
	if !strings.Contains(h.err.String(), "WXYZ-1234") || !strings.Contains(h.err.String(), idp.URL+"/activate") {
		t.Fatalf("stderr = %s", h.err.String())
	}
	if o := h.openedURLs(); len(o) != 1 || !strings.Contains(o[0], "code=WXYZ-1234") {
		t.Fatalf("opened = %v", o)
	}
	p := h.loadConfig().Profile("default")
	first := p.Token
	if !tokens[first] || p.RefreshToken != "refresh-1" || p.Login != clientconfig.LoginDevice || p.Device == nil ||
		p.Device.ClientID != "cli-client" || p.Device.TokenEndpoint != idp.URL+"/token" || p.TokenExpiresAt.IsZero() {
		t.Fatalf("profile = %+v", p)
	}

	// Later, close to expiry: the next command refreshes and persists.
	h.now = h.now.Add(59 * time.Minute)
	h.mustRun("whoami")
	p = h.loadConfig().Profile("default")
	if p.Token == first || !tokens[p.Token] || p.RefreshToken != "refresh-2" {
		t.Fatalf("token not refreshed: %+v", p)
	}
	last := g.Requests()[len(g.Requests())-1]
	if last.Auth != "Bearer "+p.Token {
		t.Fatalf("whoami used %q", last.Auth)
	}
}

func TestLoginDeviceNotOffered(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	if errOut := h.mustFail("login", "--server", g.URL, "--device"); !strings.Contains(errOut, "device login") {
		t.Fatalf("stderr = %s", errOut)
	}
	g.Info.Auth = deploy.AuthInfo{Mode: "header"}
	if errOut := h.mustFail("login", "--server", g.URL); !strings.Contains(errOut, "--token") {
		t.Fatalf("stderr = %s", errOut)
	}
}

func TestLogoutAndEnvOverrides(t *testing.T) {
	h := newHarness(t)
	g := loggedIn(t, h)
	g.Tokens["env-token"] = alice

	// BACKYARD_TOKEN overrides the stored token.
	t.Setenv(clientconfig.EnvToken, "env-token")
	h.mustRun("whoami", "--json")
	var who whoamiOutput
	if err := json.Unmarshal([]byte(h.out.String()), &who); err != nil {
		t.Fatal(err)
	}
	if who.Login != "token ($BACKYARD_TOKEN)" || who.User.ID != alice.ID {
		t.Fatalf("whoami = %+v", who)
	}
	reqs := g.Requests()
	if reqs[len(reqs)-1].Auth != "Bearer env-token" {
		t.Fatalf("auth = %q", reqs[len(reqs)-1].Auth)
	}
	// --token beats the environment.
	h.mustRun("whoami", "--token", "alice-token")
	reqs = g.Requests()
	if reqs[len(reqs)-1].Auth != "Bearer alice-token" {
		t.Fatalf("auth = %q", reqs[len(reqs)-1].Auth)
	}
	t.Setenv(clientconfig.EnvToken, "")

	// BACKYARD_SERVER for another server drops the stored credentials.
	other := clitest.NewGateway(t)
	t.Setenv(clientconfig.EnvServer, other.URL)
	h.mustFail("whoami")
	if r := other.Requests(); len(r) == 0 || r[len(r)-1].Auth != "" {
		t.Fatalf("credentials leaked to another server: %+v", r)
	}
	t.Setenv(clientconfig.EnvServer, "")

	out := h.mustRun("logout")
	if !strings.Contains(out, "Logged out") {
		t.Fatalf("stdout = %s", out)
	}
	p := h.loadConfig().Profile("default")
	if p.Token != "" || p.User != nil || p.Server != g.URL {
		t.Fatalf("after logout: %+v", p)
	}
	if out := h.mustRun("logout"); !strings.Contains(out, "Not logged in") {
		t.Fatalf("second logout = %s", out)
	}
	if out := h.mustRun("--json", "logout"); !strings.Contains(out, `"logged_out": false`) {
		t.Fatalf("logout --json = %s", out)
	}
	if errOut := h.mustFail("whoami"); !strings.Contains(errOut, "backyard login") {
		t.Fatalf("whoami after logout: %s", errOut)
	}
}

func TestExpiredTokenWarning(t *testing.T) {
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.Tokens["old"] = alice
	h.writeProfile("default", &clientconfig.Profile{Server: g.URL, Token: "old", TokenExpiresAt: h.now.Add(-time.Hour)}, true)
	h.mustRun("whoami")
	if !strings.Contains(h.err.String(), "expired") {
		t.Fatalf("stderr = %s", h.err.String())
	}
}

func TestLoginDeployTokenServer(t *testing.T) {
	// auth mode none without anonymous deploys (the Cloudflare edition's
	// default): login asks for the deploy token instead of a browser login.
	h := newHarness(t)
	g := clitest.NewGateway(t)
	g.Info.Auth = deploy.AuthInfo{Mode: "none", CLILoginURL: g.URL + "/__backyard/auth/cli"}
	g.Tokens["deploy-secret"] = alice
	if errOut := h.mustFail("login", "--server", g.URL); !strings.Contains(errOut, "DEPLOY_TOKEN") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.stdinTTY, h.stdin = true, "deploy-secret\n"
	h.mustRun("login", "--server", g.URL)
	if p := h.loadConfig().Profile("default"); p.Token != "deploy-secret" || p.Login != clientconfig.LoginToken {
		t.Fatalf("profile = %+v", p)
	}
	if len(h.openedURLs()) != 0 {
		t.Fatal("opened a browser")
	}

	// On the machine that ran `backyard cloudflare setup`, the saved deploy
	// token is used without asking (non-interactive too).
	h.stdinTTY, h.stdin = false, ""
	g.Tokens["saved-token"] = alice
	writeFiles(t, filepath.Join(h.home, ".config", "backyard", "cloudflare"), map[string]string{
		"other.json":    `{"url":"https://other.example.workers.dev","deployToken":"nope"}`,
		"broken.json":   `{`,
		"backyard.json": `{"url":"` + g.URL + `/","deployToken":"saved-token"}`,
	})
	h.mustRun("login", "--server", g.URL)
	if p := h.loadConfig().Profile("default"); p.Token != "saved-token" || !strings.Contains(h.err.String(), "saved by the Cloudflare setup") {
		t.Fatalf("profile = %+v, stderr = %s", p, h.err.String())
	}
}

// TestLoginNeverSendsASavedDeployTokenToAnotherServer: the deploy token
// saved by `backyard cloudflare setup` is matched against the server the
// user chose, never against the public_url a server reports in /info: a
// server claiming to be the user's Worker must not receive its token.
func TestLoginNeverSendsASavedDeployTokenToAnotherServer(t *testing.T) {
	h := newHarness(t)
	evil := clitest.NewGateway(t)
	evil.Info.Auth = deploy.AuthInfo{Mode: "none"}
	evil.Info.PublicURL = "https://backyard.acme.workers.dev"
	evil.Tokens["byd_saved"] = alice // it would happily accept it
	writeFiles(t, filepath.Join(h.home, ".config", "backyard", "cloudflare"), map[string]string{
		"backyard.json": `{"url":"https://backyard.acme.workers.dev","deployToken":"byd_saved"}`,
	})
	errOut := h.mustFail("login", "--server", evil.URL)
	if strings.Contains(errOut, "saved by the Cloudflare setup") || !strings.Contains(errOut, "DEPLOY_TOKEN") {
		t.Fatalf("stderr = %s", errOut)
	}
	h.mustFail("login", "--gateway", evil.GatewayURL())
	for _, r := range evil.Requests() {
		if strings.Contains(r.Auth, "byd_saved") {
			t.Fatalf("saved token sent to %s %s", r.Method, r.Path)
		}
	}
	if p := h.loadConfig().Profile("default"); p != nil && p.Token != "" {
		t.Fatalf("profile = %+v", p)
	}

	// The Worker the token was saved for is still logged in without
	// asking, also when only its gateway is named.
	g := clitest.NewGateway(t)
	g.Info.Auth = deploy.AuthInfo{Mode: "none"}
	g.Tokens["byd_mine"] = alice
	writeFiles(t, filepath.Join(h.home, ".config", "backyard", "cloudflare"), map[string]string{
		"mine.json": `{"url":"` + g.URL + `","deployToken":"byd_mine"}`,
	})
	for _, args := range [][]string{{"login", "--server", g.URL}, {"login", "--gateway", g.GatewayURL()}} {
		h.mustRun(args...)
		if p := h.loadConfig().Profile("default"); p == nil || p.Token != "byd_mine" {
			t.Fatalf("%v: profile = %+v", args, p)
		}
	}
}
