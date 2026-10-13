package auth

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/keys"
)

var anonIDRe = regexp.MustCompile(`^anon:[0-9a-f]{16}$`)

func TestAnonymousCookie(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*config.Config)
		wantDomain string
		wantSecure bool
	}{
		{"host mode", nil, "backyard.test", true},
		{"path mode", func(c *config.Config) { c.Server.URLMode = "path" }, "", true},
		{"localhost", func(c *config.Config) {
			c.Server.BaseDomain, c.Server.PublicURL = "localhost", "http://localhost:8080"
		}, "", false},
		{"dot localhost", func(c *config.Config) {
			c.Server.BaseDomain, c.Server.PublicURL = "backyard.localhost", "http://backyard.localhost"
		}, "", false},
		{"ip", func(c *config.Config) { c.Server.BaseDomain, c.Server.PublicURL = "10.0.0.5", "http://10.0.0.5" }, "", false},
		{"single label", func(c *config.Config) { c.Server.BaseDomain, c.Server.PublicURL = "backyard", "https://backyard" }, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig("none")
			if tc.mutate != nil {
				tc.mutate(cfg)
			}
			a := newAuth(t, cfg, Options{})
			if a.Mode() != ModeNone || a.LoginURL("x") != "" {
				t.Fatal("mode none has no login")
			}
			rec := httptest.NewRecorder()
			r := req("GET", "https://foo.backyard.test/")
			u, err := a.Authenticate(rec, r)
			if err != nil {
				t.Fatal(err)
			}
			if !anonIDRe.MatchString(u.ID) || u.Name != "Guest "+u.ID[5:9] || !u.Anonymous || u.Admin || u.Email != "" {
				t.Fatalf("user = %+v", u)
			}
			c := cookieNamed(rec.Result(), AnonCookie)
			if c == nil {
				t.Fatal("cookie not set")
			}
			if strings.TrimPrefix(c.Domain, ".") != tc.wantDomain || c.Secure != tc.wantSecure || !c.HttpOnly ||
				c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != int((365*24*time.Hour).Seconds()) {
				t.Fatalf("cookie = %+v", c)
			}
			// Same request again: same guest, no second cookie.
			rec2 := httptest.NewRecorder()
			u2, _ := a.Authenticate(rec2, r)
			if u2.ID != u.ID || len(rec2.Result().Cookies()) != 0 {
				t.Fatalf("second call = %+v cookies=%v", u2, rec2.Result().Cookies())
			}
			// New request with the cookie: stable identity.
			u3 := mustAuth(t, a, req("GET", "https://bar.backyard.test/", c))
			if u3.ID != u.ID {
				t.Fatalf("stable id: %q != %q", u3.ID, u.ID)
			}
		})
	}
}

func TestAnonymousCookieForgery(t *testing.T) {
	a := newAuth(t, baseConfig("none"), Options{})
	forged := []*http.Cookie{
		{Name: AnonCookie, Value: "0123456789abcdef"},
		{Name: AnonCookie, Value: keys.Sign([]byte("wrong-key"), []byte("0123456789abcdef"))},
		{Name: AnonCookie, Value: keys.Sign(keys.Derive(testSecret, keys.Session), []byte("0123456789abcdef"))},
		{Name: AnonCookie, Value: keys.Sign(keys.Derive(testSecret, keys.Anon), []byte("0123456789ABCDEF"))},
		{Name: AnonCookie, Value: keys.Sign(keys.Derive(testSecret, keys.Anon), []byte("short"))},
	}
	for i, c := range forged {
		rec := httptest.NewRecorder()
		u, err := a.Authenticate(rec, req("GET", "https://backyard.test/", c))
		if err != nil {
			t.Fatal(err)
		}
		if u.ID == "anon:0123456789abcdef" || u.ID == "anon:0123456789ABCDEF" {
			t.Fatalf("case %d: forged id accepted", i)
		}
		if cookieNamed(rec.Result(), AnonCookie) == nil {
			t.Fatalf("case %d: no replacement cookie", i)
		}
	}
	// A correctly signed cookie is accepted.
	good := &http.Cookie{Name: AnonCookie, Value: keys.Sign(keys.Derive(testSecret, keys.Anon), []byte("0123456789abcdef"))}
	if u := mustAuth(t, a, req("GET", "https://backyard.test/", good)); u.ID != "anon:0123456789abcdef" || u.Name != "Guest 0123" {
		t.Fatalf("user = %+v", u)
	}
	// Without a ResponseWriter an ephemeral guest is returned.
	u, err := a.Authenticate(nil, req("GET", "https://backyard.test/"))
	if err != nil || !u.Anonymous {
		t.Fatalf("nil writer = %+v, %v", u, err)
	}
	// Admins may list an anonymous id (dev setups).
	cfg := baseConfig("none")
	cfg.Auth.Admins = []string{"anon:0123456789abcdef"}
	a = newAuth(t, cfg, Options{})
	if u := mustAuth(t, a, req("GET", "https://backyard.test/", good)); !u.Admin {
		t.Fatal("anon admin")
	}
}
