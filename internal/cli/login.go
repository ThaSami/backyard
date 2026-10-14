package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
	"github.com/thasami/backyard/internal/identity"
)

type loginFlags struct {
	device    bool
	noBrowser bool
}

func (a *cliApp) newLoginCommand() *cobra.Command {
	var fl loginFlags
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in to a Backyard server",
		Long: `Sign in to a Backyard server and store the credentials in a CLI profile
(~/.config/backyard/config.json, readable only by you).

By default the CLI opens your browser, where your company login (Okta, Entra ID, Google…)
authorizes a personal API token. Alternatives:
  --token <t>     use an existing token (or "-" to read it from stdin), e.g. for CI
  --no-browser    print a URL to open on any device and paste the token shown there
  --device        OAuth device flow against your identity provider (if enabled)
Servers that allow anonymous deploys need no credentials.

If Backyard is behind an identity-aware proxy (IAP, ALB, Cloudflare Access…), pass the
proxy-exempt deploy gateway with --gateway.`,
		Example: `  backyard login --server https://backyard.acme.com
  backyard login --server https://backyard.acme.com --gateway https://deploy.backyard.acme.com
  echo "$BACKYARD_TOKEN" | backyard login --server https://backyard.acme.com --token -
  backyard login --profile staging --server https://backyard-staging.acme.com --device`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.runLogin(cmd.Context(), fl) },
	}
	cmd.Flags().BoolVar(&fl.device, "device", false, "use the OAuth device authorization flow")
	cmd.Flags().BoolVar(&fl.noBrowser, "no-browser", false, "do not open a browser; paste the token instead")
	return cmd
}

// loginResult is printed (and stored) after a successful login.
type loginResult struct {
	Profile   string         `json:"profile"`
	Server    string         `json:"server"`
	Gateway   string         `json:"gateway"`
	Login     string         `json:"login"`
	User      *identity.User `json:"user,omitempty"`
	ExpiresAt *time.Time     `json:"token_expires_at,omitempty"`
}

func (a *cliApp) runLogin(ctx context.Context, fl loginFlags) error {
	path, err := a.configPath()
	if err != nil {
		return err
	}
	f, err := clientconfig.Load(path)
	if err != nil {
		return err
	}
	// Credentials for login only come from --token, never from the
	// environment or the stored profile.
	o := a.overrides()
	o.Token = ""
	r, err := f.Resolve(o)
	if err != nil {
		return err
	}
	server, gatewayFlag := r.Server, ""
	if r.Gateway != "" && r.Gateway != clientconfig.GatewayURL(r.Server, "") {
		gatewayFlag = r.Gateway // explicit (flag, env or profile)
	}
	if server == "" && r.Gateway == "" {
		if !a.deps.stdinTTY() {
			return errors.New("no server given: run `backyard login --server https://backyard.example.com`")
		}
		a.errf("Backyard server URL: ")
		line, err := a.readLine(ctx)
		if err != nil {
			return err
		}
		if server, err = clientconfig.NormalizeURL(line); err != nil {
			return err
		}
	}
	gateway := clientconfig.GatewayURL(server, gatewayFlag)
	// The URLs the user chose (flag, environment, profile or prompt). Saved
	// credentials are only ever matched against these, never against what
	// the contacted server claims about itself in /info.
	chosen := []string{server}
	if g, ok := strings.CutSuffix(gatewayFlag, clientconfig.GatewayPath); ok {
		chosen = append(chosen, g)
	}

	info, err := a.fetchInfo(ctx, a.client(gateway, ""))
	if err != nil {
		return a.loginUnreachable(gateway, server, err)
	}
	if server == "" {
		server = strings.TrimSuffix(info.PublicURL, "/")
	}
	// Adopt an external gateway advertised by the server when none was
	// given explicitly and it is reachable.
	storedGateway := ""
	if gatewayFlag != "" {
		storedGateway = gatewayFlag
	} else if adv := strings.TrimSuffix(info.GatewayURL, "/"); adv != "" && adv != gateway {
		if advInfo, err := a.fetchInfo(ctx, a.client(adv, "")); err == nil {
			gateway, storedGateway, info = adv, adv, advInfo
		} else {
			a.log.Debug("advertised gateway unreachable; using the embedded one", "gateway", adv, "error", err)
		}
	}

	var (
		cred    credentials
		method  string
		profile = r.Profile.Clone()
	)
	switch {
	case a.g.token != "":
		method = clientconfig.LoginToken
		cred.token, err = a.tokenFromFlag(ctx)
		if err == nil {
			if exp, ok := clientconfig.TokenExpiry(cred.token); ok {
				cred.expiresAt = exp
			}
		}
	case fl.device:
		method = clientconfig.LoginDevice
		cred, err = a.deviceLogin(ctx, info, fl.noBrowser)
	case info.Auth.AnonymousDeploy:
		method = clientconfig.LoginAnonymous
	case info.Auth.Mode == "none":
		// No identity provider and no anonymous deploys: deploys need a
		// pre-shared token (auth.tokens, or DEPLOY_TOKEN on Cloudflare,
		// which `backyard cloudflare setup` saved on this machine, matched
		// by the Worker's URL or its workers.dev URL).
		method = clientconfig.LoginToken
		if tok, path := a.savedCloudflareToken(chosen...); tok != "" {
			a.errf("Using the deploy token saved by the Cloudflare setup (%s).\n", path)
			cred = credentials{token: tok}
		} else {
			cred, err = a.promptDeployToken(ctx)
		}
	case info.Auth.CLILoginURL != "" && fl.noBrowser:
		method = clientconfig.LoginBrowser
		cred, err = a.pasteLogin(ctx, info.Auth.CLILoginURL)
	case info.Auth.CLILoginURL != "":
		method = clientconfig.LoginBrowser
		cred, err = a.loopbackLogin(ctx, info.Auth.CLILoginURL)
	case info.Auth.Device != nil:
		method = clientconfig.LoginDevice
		cred, err = a.deviceLogin(ctx, info, fl.noBrowser)
	default:
		return fmt.Errorf("this server (auth mode %q) offers no interactive CLI login; create a token "+
			"(hub → CLI setup, or a static token in auth.tokens — see `backyard token hash`) and run "+
			"`backyard login --token <token>`", info.Auth.Mode)
	}
	if err != nil {
		return err
	}

	var user *identity.User
	if cred.token != "" || method == clientconfig.LoginAnonymous {
		mctx, cancel := context.WithTimeout(ctx, apiTimeout)
		user, err = a.client(gateway, cred.token).Me(mctx)
		cancel()
		if err != nil && cred.token != "" {
			return fmt.Errorf("the server rejected the token: %w", explain(err, nil))
		}
		if method == clientconfig.LoginAnonymous {
			user = nil // a throwaway guest identity
		}
	}

	if profile.Server != "" && !strings.EqualFold(profile.Server, server) {
		profile.Method = "" // a preference for another server
	}
	profile.ClearCredentials()
	profile.Server, profile.Gateway = server, storedGateway
	profile.Token, profile.TokenExpiresAt = cred.token, cred.expiresAt
	profile.RefreshToken, profile.Device = cred.refreshToken, cred.device
	profile.Login = method
	profile.Info = info
	if user != nil {
		profile.User = &clientconfig.User{ID: user.ID, Email: user.Email, Name: user.Name, Admin: user.Admin}
	}
	f, err = clientconfig.Load(path) // re-read: the browser flow may have taken minutes
	if err != nil {
		return err
	}
	f.Set(r.ProfileName, profile)
	f.Current = r.ProfileName
	if err := f.Save(path); err != nil {
		return err
	}

	res := loginResult{Profile: r.ProfileName, Server: server, Gateway: gateway, Login: method, User: user}
	if !cred.expiresAt.IsZero() {
		res.ExpiresAt = &cred.expiresAt
	}
	if a.g.json {
		return a.printJSON(res)
	}
	switch {
	case method == clientconfig.LoginAnonymous:
		a.outf("%s allows anonymous deploys; no login needed. Saved profile %q.\n", server, r.ProfileName)
	default:
		a.outf("Logged in to %s as %s (profile %q, via %s).\n", server, describeUser(user), r.ProfileName, method)
	}
	if !cred.expiresAt.IsZero() {
		a.errf("The token expires %s.\n", cred.expiresAt.Local().Format("2006-01-02 15:04 MST"))
	}
	return nil
}

// loginUnreachable explains a failed /info probe.
func (a *cliApp) loginUnreachable(gateway, server string, err error) error {
	msg := fmt.Sprintf("could not reach the Backyard deploy gateway at %s: %v", gateway, err)
	var ae *deploy.APIError
	proxied := errors.As(err, &ae) && (ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden ||
		strings.Contains(strings.ToLower(ae.Message), "<html") || strings.Contains(strings.ToLower(ae.Message), "<!doctype"))
	if !proxied && strings.Contains(err.Error(), "invalid character '<'") {
		proxied = true
	}
	if proxied || server != "" {
		hubHint := server
		if hubHint == "" {
			hubHint = "<hub-url>"
		}
		msg += "\n\nIf Backyard sits behind an identity-aware proxy (Google IAP, AWS ALB, Cloudflare Access, " +
			"oauth2-proxy…), the proxy answers the CLI with a login page. Ask your admin for the deploy gateway URL " +
			"the proxy exempts (or the standalone gateway / Lambda URL) and run:\n  backyard login --server " +
			hubHint + " --gateway <gateway-url>"
	}
	return errors.New(msg)
}

// describeUser formats a user for messages.
func describeUser(u *identity.User) string {
	if u == nil {
		return "an unknown user"
	}
	name := u.Name
	switch {
	case name != "" && u.Email != "" && !strings.EqualFold(name, u.Email):
		name += " <" + u.Email + ">"
	case name == "" && u.Email != "":
		name = u.Email
	case name == "":
		name = u.ID
	}
	if u.Admin {
		name += " (admin)"
	}
	if u.Anonymous {
		name += " (anonymous)"
	}
	return name
}

// credentials are what a login flow obtained.
type credentials struct {
	token        string
	expiresAt    time.Time
	refreshToken string
	device       *clientconfig.Device
}

// tokenFromFlag returns --token, reading stdin for "-".
func (a *cliApp) tokenFromFlag(ctx context.Context) (string, error) {
	t := strings.TrimSpace(a.g.token)
	if t != "-" {
		return t, nil
	}
	if a.deps.stdinTTY() {
		a.errf("Paste the token: ")
	}
	line, err := a.readSecret(ctx)
	if err != nil {
		return "", fmt.Errorf("read token from stdin: %w", err)
	}
	if t = strings.TrimSpace(line); t == "" {
		return "", errors.New("empty token on stdin")
	}
	return t, nil
}

// promptDeployToken asks for the deploy token of a server without user
// logins (auth mode none).
func (a *cliApp) promptDeployToken(ctx context.Context) (credentials, error) {
	const hint = "this server has no user login and requires a deploy token for deploys: run " +
		"`backyard login --token <token>` (a token from auth.tokens — see `backyard token hash` — or, for the " +
		"Cloudflare edition, the DEPLOY_TOKEN printed by `backyard cloudflare setup`)"
	if !a.deps.stdinTTY() {
		return credentials{}, errors.New(hint)
	}
	a.errf("This server requires a deploy token (for the Cloudflare edition: the DEPLOY_TOKEN printed by `backyard cloudflare setup`).\nDeploy token: ")
	line, err := a.readSecret(ctx)
	if err != nil {
		return credentials{}, fmt.Errorf("read token: %w", err)
	}
	tok := strings.TrimSpace(line)
	if tok == "" {
		return credentials{}, errors.New(hint)
	}
	return credentials{token: tok}, nil
}

// pasteLogin prints the display-mode login URL and reads the token.
func (a *cliApp) pasteLogin(ctx context.Context, loginURL string) (credentials, error) {
	u, err := url.Parse(loginURL)
	if err != nil {
		return credentials{}, fmt.Errorf("invalid cli_login_url %q: %w", loginURL, err)
	}
	q := u.Query()
	q.Set("mode", "display")
	u.RawQuery = q.Encode()
	a.errf("Open this URL in a browser where you can sign in, then paste the token shown there:\n\n  %s\n\n", u)
	a.errf("Token: ")
	line, err := a.readSecret(ctx)
	if err != nil {
		return credentials{}, fmt.Errorf("read token: %w", err)
	}
	tok := strings.TrimSpace(line)
	if tok == "" {
		return credentials{}, errors.New("no token entered")
	}
	c := credentials{token: tok}
	if exp, ok := clientconfig.TokenExpiry(tok); ok {
		c.expiresAt = exp
	}
	return c, nil
}

// loopbackLogin runs the browser → 127.0.0.1 callback flow (SPEC §8.3).
func (a *cliApp) loopbackLogin(ctx context.Context, loginURL string) (credentials, error) {
	lu, err := url.Parse(loginURL)
	if err != nil || (lu.Scheme != "http" && lu.Scheme != "https") {
		return credentials{}, fmt.Errorf("invalid cli_login_url %q", loginURL)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return credentials{}, fmt.Errorf("start login callback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	state, err := randomHex(16)
	if err != nil {
		ln.Close()
		return credentials{}, err
	}
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback", port)
	q := lu.Query()
	q.Set("redirect_uri", redirect)
	q.Set("state", state)
	lu.RawQuery = q.Encode()

	type outcome struct {
		cred credentials
		err  error
	}
	results := make(chan outcome, 1)
	var once sync.Once
	deliver := func(o outcome) { once.Do(func() { results <- o }) }

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			writeCallbackPage(w, http.StatusBadRequest, "Login failed",
				"This login link does not match the running <code>backyard login</code>. Start the login again.")
			return
		}
		if e := q.Get("error"); e != "" {
			writeCallbackPage(w, http.StatusBadRequest, "Login failed", html.EscapeString(e))
			deliver(outcome{err: fmt.Errorf("login failed: %s", e)})
			return
		}
		tok := strings.TrimSpace(q.Get("token"))
		if tok == "" {
			writeCallbackPage(w, http.StatusBadRequest, "Login failed", "The server did not return a token.")
			deliver(outcome{err: errors.New("login failed: no token in the callback")})
			return
		}
		c := credentials{token: tok}
		if exp := q.Get("expires_at"); exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				c.expiresAt = t.UTC()
			}
		}
		if c.expiresAt.IsZero() {
			if t, ok := clientconfig.TokenExpiry(tok); ok {
				c.expiresAt = t
			}
		}
		writeCallbackPage(w, http.StatusOK, "You are signed in",
			"The Backyard CLI is now logged in. You can close this tab and return to your terminal.")
		deliver(outcome{cred: c})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	a.errf("Opening your browser to sign in…\nIf it does not open, visit:\n\n  %s\n\n", lu)
	a.openURL(ctx, lu.String())
	a.errf("Waiting for the browser login (Ctrl+C to cancel; on a machine without a browser use --no-browser)…\n")

	timeout := a.deps.loginTimeout
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case o := <-results:
		return o.cred, o.err
	case <-t.C:
		return credentials{}, fmt.Errorf("timed out after %s waiting for the browser login; try again, or use --no-browser", timeout)
	case <-ctx.Done():
		return credentials{}, ctx.Err()
	}
}

// writeCallbackPage renders the loopback result page. The token is removed
// from the address bar so it does not linger on screen.
func writeCallbackPage(w http.ResponseWriter, status int, title, bodyHTML string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s · Backyard</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>body{font:16px/1.5 system-ui,sans-serif;margin:0;display:grid;place-items:center;min-height:100vh;background:#f6f7f4;color:#1d2a1f}
main{max-width:28rem;padding:2rem;background:#fff;border-radius:12px;box-shadow:0 1px 4px rgba(0,0,0,.08)}h1{margin-top:0;font-size:1.4rem}
@media (prefers-color-scheme:dark){body{background:#151a16;color:#e4ebe5}main{background:#1f2620}}</style></head>
<body><main><h1>%s</h1><p>%s</p></main><script>history.replaceState(null,"","/callback")</script></body></html>`,
		html.EscapeString(title), html.EscapeString(title), bodyHTML)
}

// deviceLogin runs the OAuth device authorization grant against the IdP
// advertised in info.auth.device.
func (a *cliApp) deviceLogin(ctx context.Context, info *deploy.Info, noBrowser bool) (credentials, error) {
	dev := info.Auth.Device
	if dev == nil || dev.Issuer == "" || dev.ClientID == "" {
		return credentials{}, errors.New("this server does not offer device login (auth.oidc.device_client_id is not configured); run `backyard login` without --device")
	}
	disc, err := clientconfig.Discover(ctx, a.deps.httpClient, dev.Issuer)
	if err != nil {
		return credentials{}, err
	}
	flow := &clientconfig.DeviceFlow{
		HTTP: a.deps.httpClient, ClientID: dev.ClientID, Scopes: dev.Scopes, Discovery: disc, PollUnit: a.deps.devicePollUnit,
	}
	da, err := flow.Start(ctx)
	if err != nil {
		return credentials{}, err
	}
	a.errf("To sign in, open:\n\n  %s\n\nand enter the code:  %s\n\n", da.VerificationURI, da.UserCode)
	if !noBrowser {
		target := da.VerificationURIComplete
		if target == "" {
			target = da.VerificationURI
		}
		a.openURL(ctx, target)
	}
	a.errf("Waiting for approval…\n")
	tok, err := flow.Poll(ctx, da)
	if err != nil {
		return credentials{}, err
	}
	token, exp := tok.Credential(dev.ClientID, a.deps.now())
	return credentials{
		token: token, expiresAt: exp, refreshToken: tok.RefreshToken,
		device: &clientconfig.Device{Issuer: dev.Issuer, ClientID: dev.ClientID, Scopes: dev.Scopes, TokenEndpoint: disc.TokenEndpoint},
	}, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func (a *cliApp) newLogoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove stored credentials",
		Long:  "Remove the credentials of the active profile (the server settings are kept).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := a.configPath()
			if err != nil {
				return err
			}
			f, err := clientconfig.Load(path)
			if err != nil {
				return err
			}
			name := f.ActiveName(a.g.profile)
			p := f.Profile(name)
			loggedIn := p != nil && (p.Token != "" || p.RefreshToken != "" || p.Login != "")
			if loggedIn {
				p.ClearCredentials()
				if err := f.Save(path); err != nil {
					return err
				}
			}
			switch {
			case a.g.json:
				server := ""
				if p != nil {
					server = p.Server
				}
				return a.printJSON(map[string]any{"profile": name, "server": server, "logged_out": loggedIn})
			case loggedIn:
				a.outf("Logged out of %s (profile %q).\n", p.Server, name)
			default:
				a.outf("Not logged in (profile %q).\n", name)
			}
			return nil
		},
	}
}

// whoamiOutput is the --json shape of whoami.
type whoamiOutput struct {
	User      *identity.User `json:"user"`
	Server    string         `json:"server"`
	Gateway   string         `json:"gateway"`
	Profile   string         `json:"profile"`
	Login     string         `json:"login"`
	ExpiresAt *time.Time     `json:"token_expires_at,omitempty"`
}

func (a *cliApp) newWhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the signed-in user and server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, c, err := a.gatewaySession(ctx)
			if err != nil {
				return err
			}
			mctx, cancel := context.WithTimeout(ctx, apiTimeout)
			defer cancel()
			u, err := c.Me(mctx)
			if err != nil {
				return explain(err, s)
			}
			login := s.Profile.Login
			switch s.TokenSource {
			case clientconfig.SourceFlag:
				login = "token (--token)"
			case clientconfig.SourceEnv:
				login = "token ($" + clientconfig.EnvToken + ")"
			case clientconfig.SourceNone:
				if login != clientconfig.LoginAnonymous {
					login = "none"
				}
			}
			out := whoamiOutput{User: u, Server: s.Server, Gateway: s.Gateway, Profile: s.ProfileName, Login: login}
			if s.TokenSource == clientconfig.SourceProfile && !s.Profile.TokenExpiresAt.IsZero() {
				exp := s.Profile.TokenExpiresAt
				out.ExpiresAt = &exp
			}
			if a.g.json {
				return a.printJSON(out)
			}
			a.outf("User:     %s\n", describeUser(u))
			if u.ID != "" && u.ID != u.Email {
				a.outf("ID:       %s\n", u.ID)
			}
			if s.Server != "" {
				a.outf("Server:   %s\n", s.Server)
			}
			if s.Server == "" || s.Gateway != clientconfig.GatewayURL(s.Server, "") {
				a.outf("Gateway:  %s\n", s.Gateway)
			}
			a.outf("Profile:  %s\n", s.ProfileName)
			line := login
			if out.ExpiresAt != nil {
				line += ", token expires " + out.ExpiresAt.Local().Format("2006-01-02 15:04 MST")
			}
			a.outf("Login:    %s\n", line)
			return nil
		},
	}
}

// cloudflareState is the part of the state the Cloudflare engine keeps per
// Worker in $XDG_CONFIG_HOME/backyard/cloudflare/<worker>.json (mode 0600)
// that this CLI reads.
type cloudflareState struct {
	// URL is the Worker's public hub URL.
	URL string `json:"url"`
	// DeployToken is the Worker's DEPLOY_TOKEN ("" when unknown).
	DeployToken string `json:"deployToken"`
	// WorkersDevURL is the Worker's workers.dev URL (also served next to a
	// custom domain unless workers.dev is turned off).
	WorkersDevURL string `json:"workersDevUrl"`
}

// cloudflareStateDir mirrors the engine: $XDG_CONFIG_HOME (a relative value
// resolves against base) or ~/.config, then backyard/cloudflare.
func (a *cliApp) cloudflareStateDir(base string) (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(absFrom(base, x), "backyard", "cloudflare"), nil
	}
	home, err := a.deps.homeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "backyard", "cloudflare"), nil
}

// savedCloudflareToken returns the deploy token the Cloudflare engine saved
// for a Worker served at one of urls (its public URL or its workers.dev
// URL), and the state file holding it, or "".
func (a *cliApp) savedCloudflareToken(urls ...string) (token, path string) {
	cwd, err := a.cwd()
	if err != nil {
		return "", ""
	}
	dir, err := a.cloudflareStateDir(cwd)
	if err != nil {
		return "", ""
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	for _, m := range matches {
		b, err := readSmallFile(m)
		if err != nil {
			continue
		}
		var st cloudflareState
		if json.Unmarshal(b, &st) != nil || strings.TrimSpace(st.DeployToken) == "" {
			continue
		}
		for _, raw := range []string{st.URL, st.WorkersDevURL} {
			saved, err := clientconfig.NormalizeURL(raw)
			if err != nil {
				continue
			}
			for _, u := range urls {
				if u != "" && strings.EqualFold(strings.TrimSuffix(u, "/"), saved) {
					return strings.TrimSpace(st.DeployToken), m
				}
			}
		}
	}
	return "", ""
}
