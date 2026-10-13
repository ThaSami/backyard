package clientconfig

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/deploy"
)

// clearEnv unsets every override variable for the test.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvServer, EnvGateway, EnvToken, EnvProfile, EnvDeployMethod} {
		t.Setenv(k, "")
	}
}

func TestDefaultPathUsesXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "backyard", "config.json"); got != want {
		t.Fatalf("DefaultPath = %q, want %q", got, want)
	}
	// Relative XDG paths are ignored per the XDG spec.
	t.Setenv("XDG_CONFIG_HOME", "relative/dir")
	t.Setenv("HOME", dir)
	got, err = DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if want := filepath.Join(dir, ".config", "backyard", "config.json"); got != want {
			t.Fatalf("DefaultPath = %q, want %q", got, want)
		}
	}
}

func TestLoadMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	f, err := Load(filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Profiles == nil || len(f.Profiles) != 0 || f.Current != "" {
		t.Fatalf("unexpected config %+v", f)
	}
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(empty); err != nil {
		t.Fatalf("empty file: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(bad); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSaveRoundTripAndPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	root := t.TempDir()
	path := filepath.Join(root, "cfg", "backyard", "config.json")
	exp := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	f := &File{Current: "work"}
	f.Set("work", &Profile{
		Server: "https://backyard.acme.com", Token: "tok", TokenExpiresAt: exp, Method: "s3",
		RefreshToken: "rt", Device: &Device{Issuer: "https://idp", ClientID: "cid", Scopes: []string{"openid"}},
		Login: LoginDevice, User: &User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice"},
		Info: &deploy.Info{Name: "Backyard", Methods: []string{"api", "s3"}},
	})
	f.Set("default", &Profile{Server: "http://localhost:8080"})
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", st.Mode().Perm())
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", dst.Mode().Perm())
	}
	// The spec's JSON shape.
	raw, _ := os.ReadFile(path)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	work := generic["profiles"].(map[string]any)["work"].(map[string]any)
	for _, k := range []string{"server", "token", "token_expires_at", "method", "refresh_token", "device"} {
		if _, ok := work[k]; !ok {
			t.Errorf("profile JSON lacks %q: %s", k, raw)
		}
	}
	if _, ok := generic["profiles"].(map[string]any)["default"].(map[string]any)["token_expires_at"]; ok {
		t.Errorf("zero expiry should be omitted: %s", raw)
	}

	g, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := g.Profile("work")
	if p == nil || p.Token != "tok" || !p.TokenExpiresAt.Equal(exp) || p.Device.ClientID != "cid" || p.User.Name != "Alice" || p.Info.Methods[1] != "s3" {
		t.Fatalf("round trip mismatch: %+v", p)
	}
	if g.Current != "work" || strings.Join(g.Names(), ",") != "default,work" {
		t.Fatalf("current/names: %q %v", g.Current, g.Names())
	}

	// Loosened permissions are tightened on the next save.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := g.Save(path); err != nil {
		t.Fatal(err)
	}
	st, _ = os.Stat(path)
	dst, _ = os.Stat(filepath.Dir(path))
	if st.Mode().Perm() != 0o600 || dst.Mode().Perm() != 0o700 {
		t.Fatalf("modes after re-save: file %v dir %v", st.Mode().Perm(), dst.Mode().Perm())
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestProfileHelpers(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := &Profile{Token: "t", TokenExpiresAt: now.Add(time.Minute)}
	if p.TokenExpired(now) {
		t.Fatal("not yet expired")
	}
	if !p.TokenExpired(now.Add(time.Minute)) {
		t.Fatal("expired at expiry")
	}
	if !p.TokenExpiresWithin(now, 2*time.Minute) || p.TokenExpiresWithin(now, 30*time.Second) {
		t.Fatal("TokenExpiresWithin")
	}
	if (&Profile{Token: "t"}).TokenExpired(now) {
		t.Fatal("unknown expiry never expires")
	}
	c := (&Profile{Device: &Device{Scopes: []string{"a"}}, User: &User{ID: "x"}}).Clone()
	c.Device.Scopes[0] = "b"
	c.User.ID = "y"
	p.Device = &Device{ClientID: "c", Issuer: "https://i"}
	p.RefreshToken = "r"
	if !p.CanRefresh() {
		t.Fatal("CanRefresh")
	}
	p.ClearCredentials()
	if p.Token != "" || p.RefreshToken != "" || p.Device != nil || !p.TokenExpiresAt.IsZero() {
		t.Fatalf("ClearCredentials left %+v", p)
	}
	u := &User{ID: "alice@acme.com", Email: "Alice@acme.com"}
	if !u.Is("ALICE@acme.com") || u.Is("bob@acme.com") || u.Is("") || (*User)(nil).Is("x") {
		t.Fatal("User.Is")
	}
}

func TestResolvePrecedence(t *testing.T) {
	stored := &File{Current: "work", Profiles: map[string]*Profile{
		"work":  {Server: "https://backyard.acme.com", Token: "profile-token", Method: "s3", Gateway: "https://gw.acme.com/"},
		"other": {Server: "backyard.other.com", Token: "other-token"},
	}}
	tests := []struct {
		name       string
		env        map[string]string
		o          Overrides
		wantName   string
		wantServer string
		wantGW     string
		wantToken  string
		wantMethod string
		wantTokSrc Source
		wantErr    string
	}{
		{name: "profile", wantName: "work", wantServer: "https://backyard.acme.com", wantGW: "https://gw.acme.com",
			wantToken: "profile-token", wantMethod: "s3", wantTokSrc: SourceProfile},
		{name: "env profile", env: map[string]string{EnvProfile: "other"}, wantName: "other",
			wantServer: "https://backyard.other.com", wantGW: "https://backyard.other.com/__backyard/api/deploy",
			wantToken: "other-token", wantMethod: "auto", wantTokSrc: SourceProfile},
		{name: "flag beats env", env: map[string]string{EnvProfile: "other", EnvToken: "env-token", EnvDeployMethod: "api"},
			o: Overrides{Profile: "work", Token: "flag-token"}, wantName: "work", wantServer: "https://backyard.acme.com",
			wantGW: "https://gw.acme.com", wantToken: "flag-token", wantMethod: "api", wantTokSrc: SourceFlag},
		{name: "env token", env: map[string]string{EnvToken: "env-token"}, wantName: "work",
			wantServer: "https://backyard.acme.com", wantGW: "https://gw.acme.com", wantToken: "env-token",
			wantMethod: "s3", wantTokSrc: SourceEnv},
		{name: "env gateway", env: map[string]string{EnvGateway: "https://lambda.example.com/"}, wantName: "work",
			wantServer: "https://backyard.acme.com", wantGW: "https://lambda.example.com", wantToken: "profile-token",
			wantMethod: "s3", wantTokSrc: SourceProfile},
		{name: "other server drops profile credentials", o: Overrides{Server: "http://localhost:9000"}, wantName: "work",
			wantServer: "http://localhost:9000", wantGW: "http://localhost:9000/__backyard/api/deploy",
			wantToken: "", wantMethod: "s3", wantTokSrc: SourceNone},
		{name: "same server keeps credentials", env: map[string]string{EnvServer: "https://backyard.acme.com/"}, wantName: "work",
			wantServer: "https://backyard.acme.com", wantGW: "https://gw.acme.com", wantToken: "profile-token",
			wantMethod: "s3", wantTokSrc: SourceProfile},
		{name: "missing profile", o: Overrides{Profile: "new"}, wantName: "new", wantMethod: "auto"},
		{name: "bad method", o: Overrides{Method: "ftp"}, wantErr: "unknown deploy method"},
		{name: "bad server", o: Overrides{Server: "ftp://x"}, wantErr: "server URL (from flag)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			r, err := stored.Resolve(tt.o)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.ProfileName != tt.wantName || r.Server != tt.wantServer || r.Gateway != tt.wantGW ||
				r.Token != tt.wantToken || r.Method != tt.wantMethod || r.TokenSource != tt.wantTokSrc {
				t.Fatalf("got name=%q server=%q gw=%q token=%q method=%q src=%q", r.ProfileName, r.Server, r.Gateway, r.Token, r.Method, r.TokenSource)
			}
			if r.Profile == nil {
				t.Fatal("nil profile")
			}
		})
	}
	// Resolve never mutates the stored profile.
	if stored.Profiles["work"].Token != "profile-token" {
		t.Fatal("stored profile mutated")
	}
}

func TestNormalizeURL(t *testing.T) {
	tests := []struct{ in, want, err string }{
		{in: "backyard.acme.com", want: "https://backyard.acme.com"},
		{in: "https://backyard.acme.com/", want: "https://backyard.acme.com"},
		{in: "localhost:8080", want: "http://localhost:8080"},
		{in: "127.0.0.1:4321/x/", want: "http://127.0.0.1:4321/x"},
		{in: "foo.localhost", want: "http://foo.localhost"},
		{in: "http://example.com/a?b=c#d", want: "http://example.com/a"},
		{in: " https://gw.example.com/prod/ ", want: "https://gw.example.com/prod"},
		{in: "", err: "empty"},
		{in: "ftp://x", err: "scheme"},
		{in: "https://", err: "missing host"},
		{in: "https://user:secret@x.com", err: "credentials"},
	}
	for _, tt := range tests {
		got, err := NormalizeURL(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("NormalizeURL(%q) err = %v, want %q", tt.in, err, tt.err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Errorf("error leaks credentials: %v", err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestGatewayAndSiteURLs(t *testing.T) {
	if g := GatewayURL("https://b.acme.com/", ""); g != "https://b.acme.com/__backyard/api/deploy" {
		t.Errorf("GatewayURL derived = %q", g)
	}
	if g := GatewayURL("https://b.acme.com", "https://gw.acme.com/"); g != "https://gw.acme.com" {
		t.Errorf("GatewayURL explicit = %q", g)
	}
	if g := GatewayURL("", ""); g != "" {
		t.Errorf("GatewayURL empty = %q", g)
	}
	host := &deploy.Info{PublicURL: "https://backyard.acme.com", URLMode: "host"}
	path := &deploy.Info{PublicURL: "https://x.workers.dev/", URLMode: "path"}
	local := &deploy.Info{PublicURL: "http://localhost:8080", URLMode: "host"}
	tests := []struct {
		info         *deploy.Info
		server, site string
		want         string
	}{
		{host, "", "lunch", "https://lunch.backyard.acme.com/"},
		{path, "", "lunch", "https://x.workers.dev/s/lunch/"},
		{local, "", "demo", "http://demo.localhost:8080/"},
		{nil, "https://b.example.com", "demo", "https://demo.b.example.com/"},
		{nil, "", "demo", ""},
	}
	for _, tt := range tests {
		if got := SiteURL(tt.info, tt.server, tt.site); got != tt.want {
			t.Errorf("SiteURL(%+v, %q, %q) = %q, want %q", tt.info, tt.server, tt.site, got, tt.want)
		}
	}
	if h := HubURL(path, "x"); h != "https://x.workers.dev/" {
		t.Errorf("HubURL = %q", h)
	}
	if h := HubURL(nil, "https://b.example.com"); h != "https://b.example.com/" {
		t.Errorf("HubURL fallback = %q", h)
	}
}

func TestIsLocalHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "localhost:80": true, "a.localhost": true, "127.0.0.1:9": true, "[::1]:8080": true,
		"example.com": false, "10.0.0.1": false, "localhost.example.com": false,
	} {
		if got := IsLocalHost(host); got != want {
			t.Errorf("IsLocalHost(%q) = %v", host, got)
		}
	}
}

func TestSanitizeSiteName(t *testing.T) {
	tests := map[string]string{
		"Lunch Vote":                   "lunch-vote",
		"my_site.v2":                   "my-site-v2",
		"--x--":                        "x",
		"ÜBER":                         "ber",
		"...":                          "",
		strings.Repeat("a", 70):        strings.Repeat("a", 63),
		strings.Repeat("a", 62) + "-b": strings.Repeat("a", 62),
	}
	for in, want := range tests {
		if got := SanitizeSiteName(in); got != want {
			t.Errorf("SanitizeSiteName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SiteNameFromDir(filepath.Join(t.TempDir(), "My Project")); got != "my-project" {
		t.Errorf("SiteNameFromDir = %q", got)
	}
}

func TestFindProject(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if pf, err := FindProject(deep); err != nil || pf != nil {
		t.Fatalf("expected none, got %+v %v", pf, err)
	}
	if err := SaveProject(filepath.Join(root, "a"), Project{Site: "lunch", Dir: "dist", Ignore: []string{"*.map"}}); err != nil {
		t.Fatal(err)
	}
	pf, err := FindProject(deep)
	if err != nil || pf == nil {
		t.Fatalf("FindProject: %v %v", pf, err)
	}
	if pf.Site != "lunch" || pf.Root() != filepath.Join(root, "a") || pf.DeployDir() != filepath.Join(root, "a", "dist") || pf.Ignore[0] != "*.map" {
		t.Fatalf("unexpected project %+v root=%s dir=%s", pf, pf.Root(), pf.DeployDir())
	}
	// A file path starts the search in its directory.
	f := filepath.Join(deep, "x.txt")
	_ = os.WriteFile(f, nil, 0o644)
	if pf, _ := FindProject(f); pf == nil || pf.Site != "lunch" {
		t.Fatal("search from file path")
	}
	// Closest wins.
	if err := os.WriteFile(filepath.Join(deep, ProjectFileName), []byte(`{"site":"inner"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if pf, _ := FindProject(deep); pf == nil || pf.Site != "inner" || pf.DeployDir() != deep {
		t.Fatalf("closest project: %+v", pf)
	}
	if err := os.WriteFile(filepath.Join(deep, ProjectFileName), []byte(`{"method":"ftp"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FindProject(deep); err == nil || !strings.Contains(err.Error(), "ftp") {
		t.Fatalf("invalid method error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(deep, ProjectFileName), []byte(`{`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FindProject(deep); err == nil {
		t.Fatal("expected parse error")
	}
}

// makeJWT builds an unsigned compact JWT with the given claims.
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	b, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".c2ln"
}

func TestParseJWTUnverified(t *testing.T) {
	exp := time.Date(2031, 5, 6, 7, 8, 9, 0, time.UTC)
	tok := makeJWT(t, map[string]any{"exp": exp.Unix(), "aud": []string{"a", "b"}, "azp": "z"})
	c, ok := ParseJWTUnverified(tok)
	if !ok || c.Azp != "z" || strings.Join(c.Audiences(), ",") != "a,b" {
		t.Fatalf("claims = %+v ok=%v", c, ok)
	}
	if got, ok := TokenExpiry(tok); !ok || !got.Equal(exp) {
		t.Fatalf("TokenExpiry = %v %v", got, ok)
	}
	single := makeJWT(t, map[string]any{"aud": "only"})
	if c, _ := ParseJWTUnverified(single); strings.Join(c.Audiences(), ",") != "only" {
		t.Fatal("single aud")
	}
	for _, bad := range []string{"", "opaque-token", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("[1]")) + ".c"} {
		if _, ok := ParseJWTUnverified(bad); ok {
			t.Errorf("ParseJWTUnverified(%q) ok", bad)
		}
	}
	if _, ok := TokenExpiry(single); ok {
		t.Fatal("no exp")
	}
}
