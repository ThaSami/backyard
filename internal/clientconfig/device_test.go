package clientconfig

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/blob"
	"github.com/thasami/backyard/internal/deploy"
)

// fakeIdP is a minimal OIDC provider supporting the device grant.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	script      []string // token endpoint answers per poll: "pending", "slow", "denied", "expired", "ok"
	polls       int
	pollTimes   []time.Time
	refreshes   int
	lastForm    map[string]string
	accessToken string
	idToken     string
	verifURLKey string // "verification_uri" or "verification_url"
	tokenErr    string // refresh error code
}

func newFakeIdP(t *testing.T) *fakeIdP {
	f := &fakeIdP{t: t, verifURLKey: "verification_uri"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, map[string]string{
			"issuer":                        f.srv.URL,
			"device_authorization_endpoint": f.srv.URL + "/device",
			"token_endpoint":                f.srv.URL + "/token",
		})
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "cli-client" || r.Form.Get("scope") != "openid offline_access" {
			writeJSONResp(w, 400, map[string]string{"error": "invalid_client"})
			return
		}
		writeJSONResp(w, 200, map[string]any{
			"device_code": "dev-code", "user_code": "ABCD-EFGH",
			f.verifURLKey: f.srv.URL + "/activate", "expires_in": 60, "interval": 1,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		f.lastForm = map[string]string{}
		for k := range r.Form {
			f.lastForm[k] = r.Form.Get(k)
		}
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			f.refreshes++
			if f.tokenErr != "" {
				writeJSONResp(w, 400, map[string]string{"error": f.tokenErr, "error_description": "nope"})
				return
			}
			writeJSONResp(w, 200, map[string]any{"access_token": f.accessToken, "id_token": f.idToken, "refresh_token": "rotated-refresh", "expires_in": 3600})
			return
		case "urn:ietf:params:oauth:grant-type:device_code":
		default:
			writeJSONResp(w, 400, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		f.pollTimes = append(f.pollTimes, time.Now())
		step := "ok"
		if f.polls < len(f.script) {
			step = f.script[f.polls]
		}
		f.polls++
		switch step {
		case "pending":
			writeJSONResp(w, 400, map[string]string{"error": "authorization_pending"})
		case "slow":
			writeJSONResp(w, 400, map[string]string{"error": "slow_down"})
		case "denied":
			writeJSONResp(w, 400, map[string]string{"error": "access_denied"})
		case "expired":
			writeJSONResp(w, 400, map[string]string{"error": "expired_token"})
		case "weird200":
			writeJSONResp(w, 200, map[string]string{"error": "server_error", "error_description": "boom"})
		default:
			writeJSONResp(w, 200, map[string]any{"access_token": f.accessToken, "id_token": f.idToken, "refresh_token": "refresh-1", "token_type": "Bearer", "expires_in": 3600})
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestDeviceFlowSuccessWithPendingAndSlowDown(t *testing.T) {
	idp := newFakeIdP(t)
	idp.verifURLKey = "verification_url" // Google's spelling
	exp := time.Now().Add(time.Hour).Truncate(time.Second).UTC()
	idp.idToken = makeJWT(t, map[string]any{"aud": "cli-client", "exp": exp.Unix(), "email": "alice@acme.com"})
	idp.accessToken = "opaque-access-token"
	idp.script = []string{"pending", "slow", "pending", "ok"}

	ctx := context.Background()
	disc, err := Discover(ctx, nil, idp.srv.URL+"/")
	if err != nil {
		t.Fatal(err)
	}
	unit := 10 * time.Millisecond
	flow := &DeviceFlow{ClientID: "cli-client", Scopes: []string{"openid", "offline_access"}, Discovery: disc, PollUnit: unit}
	da, err := flow.Start(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if da.UserCode != "ABCD-EFGH" || !strings.HasSuffix(da.VerificationURI, "/activate") {
		t.Fatalf("device authorization = %+v", da)
	}
	tok, err := flow.Poll(ctx, da)
	if err != nil {
		t.Fatal(err)
	}
	if idp.polls != 4 {
		t.Fatalf("polls = %d, want 4", idp.polls)
	}
	// After slow_down the interval grows from 1 to 6 units.
	gap := idp.pollTimes[2].Sub(idp.pollTimes[1])
	if gap < 5*unit {
		t.Fatalf("slow_down not honoured: gap %v", gap)
	}
	if idp.lastForm["device_code"] != "dev-code" || idp.lastForm["client_id"] != "cli-client" {
		t.Fatalf("token form = %v", idp.lastForm)
	}
	// Opaque access token → the ID token is used.
	cred, credExp := tok.Credential("cli-client", time.Now())
	if cred != idp.idToken || !credExp.Equal(exp) {
		t.Fatalf("credential = %q exp %v", cred, credExp)
	}
}

func TestDeviceFlowFailures(t *testing.T) {
	tests := []struct {
		script []string
		want   error
		substr string
	}{
		{script: []string{"pending", "denied"}, want: ErrDeviceDenied},
		{script: []string{"expired"}, want: ErrDeviceExpired},
		{script: []string{"weird200"}, substr: "server_error"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.script, ","), func(t *testing.T) {
			idp := newFakeIdP(t)
			idp.script = tt.script
			disc, err := Discover(context.Background(), nil, idp.srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			flow := &DeviceFlow{ClientID: "cli-client", Scopes: []string{"openid", "offline_access"}, Discovery: disc, PollUnit: time.Millisecond}
			da, err := flow.Start(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			_, err = flow.Poll(context.Background(), da)
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if tt.substr != "" && (err == nil || !strings.Contains(err.Error(), tt.substr)) {
				t.Fatalf("err = %v, want %q", err, tt.substr)
			}
		})
	}
}

func TestDeviceFlowExpiresWhilePending(t *testing.T) {
	idp := newFakeIdP(t)
	idp.script = []string{"pending", "pending", "pending", "pending", "pending", "pending", "pending", "pending"}
	disc, _ := Discover(context.Background(), nil, idp.srv.URL)
	flow := &DeviceFlow{ClientID: "cli-client", Discovery: disc, PollUnit: time.Millisecond}
	_, err := flow.Poll(context.Background(), &DeviceAuthorization{DeviceCode: "x", ExpiresIn: 5, Interval: 1})
	if !errors.Is(err, ErrDeviceExpired) {
		t.Fatalf("err = %v, want expired", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := flow.Poll(ctx, &DeviceAuthorization{DeviceCode: "x", ExpiresIn: 60, Interval: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled poll err = %v", err)
	}
}

func TestDiscoverRejectsInsecureEndpoints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, 200, map[string]string{"token_endpoint": "http://evil.example.com/token"})
	}))
	defer srv.Close()
	if _, err := Discover(context.Background(), nil, srv.URL); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Discover(context.Background(), nil, "http://idp.example.com"); err == nil {
		t.Fatal("plain http issuer accepted")
	}
	if _, err := (&DeviceFlow{Discovery: &Discovery{TokenEndpoint: "x"}}).Start(context.Background()); err == nil {
		t.Fatal("missing device endpoint accepted")
	}
}

func TestCredentialChoice(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	id := makeJWT(t, map[string]any{"aud": "cid", "exp": now.Add(time.Hour).Unix()})
	accessFor := makeJWT(t, map[string]any{"cid": "cid", "aud": "api://default", "exp": now.Add(2 * time.Hour).Unix()})
	accessAzp := makeJWT(t, map[string]any{"azp": "cid", "aud": "account"})
	accessOther := makeJWT(t, map[string]any{"aud": "https://graph.microsoft.com"})
	tests := []struct {
		name    string
		tok     TokenResponse
		want    string
		wantExp time.Time
	}{
		{"okta access jwt", TokenResponse{AccessToken: accessFor, IDToken: id}, accessFor, now.Add(2 * time.Hour)},
		{"keycloak azp", TokenResponse{AccessToken: accessAzp, IDToken: id, ExpiresIn: 60}, accessAzp, now.Add(time.Minute)},
		{"foreign audience", TokenResponse{AccessToken: accessOther, IDToken: id}, id, now.Add(time.Hour)},
		{"opaque", TokenResponse{AccessToken: "opaque", IDToken: id}, id, now.Add(time.Hour)},
		{"opaque only", TokenResponse{AccessToken: "opaque", ExpiresIn: 30}, "opaque", now.Add(30 * time.Second)},
		{"id only", TokenResponse{IDToken: id}, id, now.Add(time.Hour)},
	}
	for _, tt := range tests {
		got, exp := tt.tok.Credential("cid", now)
		if got != tt.want || !exp.Equal(tt.wantExp) {
			t.Errorf("%s: got %q exp %v, want exp %v", tt.name, got, exp, tt.wantExp)
		}
	}
}

func TestRefreshProfile(t *testing.T) {
	idp := newFakeIdP(t)
	now := time.Now().UTC().Truncate(time.Second)
	idp.idToken = makeJWT(t, map[string]any{"aud": "cli-client", "exp": now.Add(time.Hour).Unix()})
	idp.accessToken = "opaque"

	fresh := &Profile{Token: "t", TokenExpiresAt: now.Add(time.Hour), RefreshToken: "r",
		Device: &Device{Issuer: idp.srv.URL, ClientID: "cli-client"}}
	if changed, err := RefreshProfile(context.Background(), nil, fresh, now, RefreshSkew); err != nil || changed {
		t.Fatalf("fresh token refreshed: %v %v", changed, err)
	}
	noExpiry := &Profile{Token: "t", RefreshToken: "r", Device: &Device{Issuer: idp.srv.URL, ClientID: "cli-client"}}
	if changed, _ := RefreshProfile(context.Background(), nil, noExpiry, now, RefreshSkew); changed {
		t.Fatal("token without expiry refreshed")
	}
	stale := &Profile{Token: "old", TokenExpiresAt: now.Add(time.Minute), RefreshToken: "r",
		Device: &Device{Issuer: idp.srv.URL, ClientID: "cli-client"}}
	changed, err := RefreshProfile(context.Background(), nil, stale, now, RefreshSkew)
	if err != nil || !changed {
		t.Fatalf("refresh: %v %v", changed, err)
	}
	if stale.Token != idp.idToken || stale.RefreshToken != "rotated-refresh" || stale.Device.TokenEndpoint != idp.srv.URL+"/token" {
		t.Fatalf("after refresh: %+v", stale)
	}
	if idp.lastForm["refresh_token"] != "r" || idp.lastForm["client_id"] != "cli-client" {
		t.Fatalf("refresh form: %v", idp.lastForm)
	}
	idp.tokenErr = "invalid_grant"
	stale.TokenExpiresAt = now
	if _, err := RefreshProfile(context.Background(), nil, stale, now, RefreshSkew); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("refresh error = %v", err)
	}
}

func TestOpenSessionRefreshesAndPersists(t *testing.T) {
	clearEnv(t)
	idp := newFakeIdP(t)
	now := time.Now().UTC().Truncate(time.Second)
	idp.idToken = makeJWT(t, map[string]any{"aud": "cli-client", "exp": now.Add(time.Hour).Unix()})
	idp.accessToken = "opaque"
	path := filepath.Join(t.TempDir(), "backyard", "config.json")
	f := &File{Current: "corp"}
	f.Set("corp", &Profile{Server: "https://b.acme.com", Token: "old", TokenExpiresAt: now.Add(30 * time.Second),
		RefreshToken: "r", Login: LoginDevice, Device: &Device{Issuer: idp.srv.URL, ClientID: "cli-client"}})
	f.Set("other", &Profile{Server: "https://x", Token: "keep"})
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSession(context.Background(), SessionOptions{ConfigPath: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if s.Token != idp.idToken || len(s.Warnings) != 0 {
		t.Fatalf("session token %q warnings %v", s.Token, s.Warnings)
	}
	g, _ := Load(path)
	if g.Profile("corp").Token != idp.idToken || g.Profile("corp").RefreshToken != "rotated-refresh" || g.Profile("other").Token != "keep" {
		t.Fatalf("persisted: %+v", g.Profiles)
	}
	if s.Client(nil).Token != idp.idToken || s.Client(nil).BaseURL != "https://b.acme.com/__backyard/api/deploy" {
		t.Fatal("client")
	}

	// A failed refresh of an expired token yields warnings, not an error.
	idp.tokenErr = "invalid_grant"
	later := now.Add(2 * time.Hour)
	s, err = OpenSession(context.Background(), SessionOptions{ConfigPath: path, Now: func() time.Time { return later }})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Warnings) != 2 || !strings.Contains(s.Warnings[1], "expired") {
		t.Fatalf("warnings = %v", s.Warnings)
	}
}

func TestSessionUpdateProfileAndCacheInfo(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	s, err := OpenSession(context.Background(), SessionOptions{ConfigPath: path, Overrides: Overrides{Profile: "p1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequireGateway(); !errors.Is(err, ErrNoServer) {
		t.Fatalf("RequireGateway = %v", err)
	}
	if err := s.UpdateProfile(func(p *Profile) { p.Server = "https://b.example.com" }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	s, _ = OpenSession(context.Background(), SessionOptions{ConfigPath: path, Overrides: Overrides{Profile: "p1"}})
	if !s.Exists || s.Server != "https://b.example.com" {
		t.Fatalf("session = %+v", s.Resolved)
	}
}

// TestCachedInfoOnlyForTheProfilesServer: the server info cached in a profile
// (storage bucket, URLs, methods) is used only while the invocation targets
// that profile's server and gateway. With --server/BACKYARD_SERVER (or a
// gateway override) pointing elsewhere it describes another server, and a
// direct-sync deploy using it would write into the wrong bucket.
func TestCachedInfoOnlyForTheProfilesServer(t *testing.T) {
	clearEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	f := &File{}
	cached := &deploy.Info{PublicURL: "https://backyard.prod.example.com",
		Storage: &blob.Description{Type: "s3", Bucket: "prod-bucket"}}
	f.Set("default", &Profile{Server: "https://backyard.prod.example.com", Token: "t", Info: cached})
	f.Current = "default"
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	open := func(o Overrides) *Session {
		t.Helper()
		s, err := OpenSession(context.Background(), SessionOptions{ConfigPath: path, Overrides: o})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	for name, o := range map[string]Overrides{
		"profile":              {},
		"same server flag":     {Server: "https://backyard.prod.example.com/"},
		"same server, no case": {Server: "HTTPS://BACKYARD.PROD.EXAMPLE.COM"},
	} {
		if s := open(o); s.CachedInfo() == nil || s.CachedInfo().Storage.Bucket != "prod-bucket" {
			t.Errorf("%s: CachedInfo = %+v", name, s.CachedInfo())
		}
	}
	for name, o := range map[string]Overrides{
		"other server flag":  {Server: "https://backyard.staging.example.com"},
		"other gateway flag": {Gateway: "https://deploy.staging.example.com"},
		"other profile":      {Profile: "staging"},
	} {
		if s := open(o); s.CachedInfo() != nil {
			t.Errorf("%s: CachedInfo = %+v, want nil", name, s.CachedInfo())
		}
	}
	t.Setenv(EnvServer, "https://backyard.staging.example.com")
	s := open(Overrides{})
	if s.CachedInfo() != nil {
		t.Fatalf("BACKYARD_SERVER: CachedInfo = %+v, want nil", s.CachedInfo())
	}
	// Nor is the other server's info written into the profile's cache.
	s.CacheInfo(&deploy.Info{PublicURL: "https://backyard.staging.example.com"})
	t.Setenv(EnvServer, "")
	if got := open(Overrides{}).CachedInfo(); got == nil || got.PublicURL != "https://backyard.prod.example.com" {
		t.Fatalf("cache overwritten by another server's info: %+v", got)
	}
	// The profile's own server refreshes it.
	s = open(Overrides{})
	s.CacheInfo(&deploy.Info{PublicURL: "https://backyard.prod.example.com", Name: "fresh"})
	if got := open(Overrides{}).CachedInfo(); got == nil || got.Name != "fresh" {
		t.Fatalf("cache not refreshed: %+v", got)
	}
}

func TestSessionExpiryWarnings(t *testing.T) {
	clearEnv(t)
	now := time.Date(2030, 5, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		profile Profile
		env     string
		want    string
	}{
		{"valid", Profile{Server: "https://b.example.com", Token: "t", TokenExpiresAt: now.Add(72 * time.Hour)}, "", ""},
		{"no expiry", Profile{Server: "https://b.example.com", Token: "t"}, "", ""},
		{"soon", Profile{Server: "https://b.example.com", Token: "t", TokenExpiresAt: now.Add(3 * time.Hour)}, "", "expires at"},
		{"expired", Profile{Server: "https://b.example.com", Token: "t", TokenExpiresAt: now.Add(-time.Minute)}, "", "expired at"},
		{"env token wins", Profile{Server: "https://b.example.com", Token: "t", TokenExpiresAt: now.Add(-time.Minute)}, "env-token", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvToken, tt.env)
			path := filepath.Join(t.TempDir(), "config.json")
			f := &File{Current: "default"}
			p := tt.profile
			f.Set("default", &p)
			if err := f.Save(path); err != nil {
				t.Fatal(err)
			}
			s, err := OpenSession(context.Background(), SessionOptions{ConfigPath: path, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(s.Warnings, "\n")
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Fatalf("warnings = %q, want %q", got, tt.want)
			}
		})
	}
}
