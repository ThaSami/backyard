package deploy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/auth"
	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/sites/sitestest"
)

// TestRealAuthenticators runs the gateway with the auth package's own
// authenticators in mode none (full, as embedded in the server, and
// bearer-only, as in the Lambda / standalone gateway).
func TestRealAuthenticators(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"index.html": "<title>Real</title>"})

	tokens := auth.NewTokens(testSecret, time.Hour)
	mint := func(u *identity.User) string {
		t.Helper()
		tok, _, err := tokens.Issue(u, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	aliceTok := mint(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com", Name: "Alice"})
	guestTok := mint(&identity.User{ID: "anon:0123456789abcdef", Name: "Guest 0123", Anonymous: true})

	for _, tc := range []struct {
		name       string
		bearerOnly bool
		allowAnon  bool
	}{
		{"full authenticator", false, false},
		{"full authenticator, anonymous deploys", false, true},
		{"bearer-only authenticator", true, false},
		{"bearer-only authenticator, anonymous deploys", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig(t)
			cfg.Deploy.AllowAnonymous = tc.allowAnon
			var (
				a   auth.Authenticator
				err error
			)
			if tc.bearerOnly {
				a, err = auth.NewBearer(ctx, cfg, auth.Options{})
			} else {
				a, err = auth.New(ctx, cfg, auth.Options{})
			}
			if err != nil {
				t.Fatal(err)
			}
			srv := startGateway(t, cfg, a)

			// Tokens of real users deploy.
			res, err := NewClient(srv.URL, aliceTok).Deploy(ctx, "real", dir, DeployOptions{})
			if err != nil || res.Site.UpdatedBy != "alice@acme.com" || res.Site.Title != "Real" {
				t.Fatalf("token deploy = %+v, %v", res, err)
			}
			// Garbage is rejected, never downgraded to a guest.
			_, err = NewClient(srv.URL, "not-a-token").Me(ctx)
			apiErr(t, err, http.StatusUnauthorized, CodeUnauthenticated)
			_, err = NewClient(srv.URL, aliceTok[:len(aliceTok)-2]+"xx").ListSites(ctx)
			apiErr(t, err, http.StatusUnauthorized, CodeUnauthenticated)

			// No token: a guest who reads.
			me, err := NewClient(srv.URL, "").Me(ctx)
			if err != nil || !me.Anonymous {
				t.Fatalf("guest /me = %+v, %v", me, err)
			}
			if _, err := NewClient(srv.URL, "").SiteFiles(ctx, "real"); err != nil {
				t.Errorf("guest read: %v", err)
			}
			// A guest's own API token is that guest.
			me, err = NewClient(srv.URL, guestTok).Me(ctx)
			if err != nil || !me.Anonymous || me.ID != "anon:0123456789abcdef" {
				t.Fatalf("guest token /me = %+v, %v", me, err)
			}

			for _, tok := range []string{"", guestTok} {
				c := NewClient(srv.URL, tok)
				res, err := c.Deploy(ctx, "guest-site", dir, DeployOptions{})
				if tc.allowAnon {
					if err != nil || !strings.HasPrefix(res.Site.UpdatedBy, "anon:") {
						t.Errorf("guest deploy (token %v) = %+v, %v", tok != "", res, err)
					}
				} else {
					apiErr(t, err, http.StatusUnauthorized, CodeUnauthenticated)
				}
				apiErr(t, c.DeleteSite(ctx, "real"), http.StatusUnauthorized, CodeUnauthenticated)
			}
			if err := NewClient(srv.URL, aliceTok).DeleteSite(ctx, "real"); err != nil {
				t.Errorf("token delete: %v", err)
			}
		})
	}
}

// startGateway serves a gateway over an in-memory store with authenticator a.
func startGateway(t *testing.T, cfg *config.Config, a auth.Authenticator) *httptest.Server {
	t.Helper()
	var gw *Gateway
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { gw.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	var err error
	gw, err = NewGateway(GatewayOptions{Config: cfg, Store: sitestest.NewMemStore(), Auth: a, BaseURL: srv.URL, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return srv
}
