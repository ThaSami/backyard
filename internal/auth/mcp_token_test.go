package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/thasami/backyard/internal/config"
	"github.com/thasami/backyard/internal/docstore"
	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

// storeVariants runs f on the memory and the sqlite docstore.
func storeVariants(t *testing.T, f func(t *testing.T, opts ...func(*Options))) {
	t.Run("memory", func(t *testing.T) { f(t) })
	t.Run("sqlite", func(t *testing.T) { f(t, sqliteDocs(t)) })
}

func TestMCPTokenExchange(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		id := e.register("App", loopbackCB)
		f := newFlow(id, loopbackCB)
		b := newBrowser()
		code := e.code(b, f)

		// A wrong verifier does not consume the code.
		other, _ := pkce()
		res := e.exchange(f, code, url.Values{"code_verifier": {other}})
		oauthErr(t, res, http.StatusBadRequest, "invalid_grant")
		res = e.exchange(f, code, url.Values{"code_verifier": {"short"}})
		oauthErr(t, res, http.StatusBadRequest, "invalid_grant")
		// A different redirect_uri, a foreign resource, another client.
		oauthErr(t, e.exchange(f, code, url.Values{"redirect_uri": {"http://127.0.0.1:10/callback"}}), http.StatusBadRequest, "invalid_grant")
		oauthErr(t, e.exchange(f, code, url.Values{"resource": {"https://other.example/__backyard/mcp"}}), http.StatusBadRequest, "invalid_target")
		otherClient := e.register("Other", loopbackCB)
		oauthErr(t, e.exchange(f, code, url.Values{"client_id": {otherClient}}), http.StatusBadRequest, "invalid_grant")
		if e.grantCount() != 1 {
			t.Fatal("a failed exchange removed the grant")
		}

		// redirect_uri may be omitted; resource may be canonically equal.
		res = e.exchange(f, code, url.Values{"redirect_uri": nil, "resource": {"HTTPS://backyard.test:443/__backyard/mcp/"}})
		if res.StatusCode != http.StatusOK {
			t.Fatalf("exchange: %d %s", res.StatusCode, body(t, res))
		}
		if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("Pragma") != "no-cache" || res.Header.Get("Content-Type") != "application/json" {
			t.Errorf("token headers = %v", res.Header)
		}
		m := jsonBody(t, res)
		at, rt := m["access_token"].(string), m["refresh_token"].(string)
		if !strings.HasPrefix(at, "byat_") || !strings.HasPrefix(rt, "byrt_") || m["token_type"] != "Bearer" ||
			m["expires_in"] != float64(3600) || m["scope"] != "mcp offline_access" || len(m) != 5 || len(at) > 4096 {
			t.Fatalf("token response = %v", m)
		}
		if rt != e.m.signRefresh(gidOfAccess(t, e, at), 1) {
			t.Error("the first refresh token must be RT(1)")
		}
		u, g, err := e.m.VerifyAccessToken(context.Background(), at)
		if err != nil || !u.Anonymous || g.ClientID != id || g.ClientKind != clientKindDCR || g.ClientName != "App" ||
			g.RedirectURI != loopbackCB || g.Scope != "mcp offline_access" || g.Resource != testResource {
			t.Fatalf("VerifyAccessToken = %+v %+v %v", u, g, err)
		}
		if !g.ExpiresAt.Equal(time.Unix(e.clock.Now().Unix(), 0).Add(720*time.Hour)) || !g.CreatedAt.Equal(time.Unix(e.clock.Now().Unix(), 0)) {
			t.Errorf("grant times %v %v", g.CreatedAt, g.ExpiresAt)
		}
		if acts := e.auditActions(); strings.Join(acts, ",") != "mcp.authorize" {
			t.Errorf("stored audit = %v", acts)
		}

		// Replay with a valid verifier: invalid_grant, the grant is revoked,
		// and the tokens of the first exchange stop working.
		oauthErr(t, e.exchange(f, code, nil), http.StatusBadRequest, "invalid_grant")
		if _, _, err := e.m.VerifyAccessToken(context.Background(), at); !isDesc(err, descRevoked) {
			t.Errorf("access token after replay: %v", err)
		}
		oauthErr(t, e.refresh(f, rt, nil), http.StatusBadRequest, "invalid_grant")
		if acts := strings.Join(e.auditActions(), ","); !strings.Contains(acts, "mcp.code_replay") || !strings.Contains(acts, "mcp.revoke:replay") {
			t.Errorf("audit after replay = %v", acts)
		}
		// Another fresh exchange; the code expires after 300 s.
		f2 := newFlow(id, loopbackCB)
		code2 := e.code(b, f2)
		e.clock.Advance(301 * time.Second)
		oauthErr(t, e.exchange(f2, code2, nil), http.StatusBadRequest, "invalid_grant")
	})
}

// gidOfAccess returns the grant id of an access token.
func gidOfAccess(t *testing.T, e *mcpEnv, at string) string {
	t.Helper()
	p, ok := e.m.parseAccess(at)
	if !ok {
		t.Fatalf("not an access token: %q", at)
	}
	return p.G
}

func isDesc(err error, desc string) bool {
	var te *tokenError
	return errors.Is(err, ErrInvalidToken) && errors.As(err, &te) && te.desc == desc
}

func TestMCPTokenFamilyCap(t *testing.T) {
	e := newMCPEnv(t, ModeNone, func(c *config.Config) { c.Auth.TokenTTL = config.Duration(30 * time.Minute) })
	id := e.register("App", loopbackCB)
	f := newFlow(id, loopbackCB)
	code := e.code(newBrowser(), f)
	e.clock.Advance(60 * time.Second)
	m := jsonBody(t, e.exchange(f, code, nil))
	// The family lasts 30 min from consent; 1 min has passed.
	if m["expires_in"] != float64(29*60) {
		t.Fatalf("expires_in = %v", m["expires_in"])
	}
	_, g, err := e.m.VerifyAccessToken(context.Background(), m["access_token"].(string))
	if err != nil || g.ExpiresAt.Sub(g.CreatedAt) != 30*time.Minute {
		t.Fatalf("grant %+v %v", g, err)
	}
	// Refreshing never extends the family.
	e.clock.Advance(20 * time.Minute)
	r := jsonBody(t, e.refresh(f, m["refresh_token"].(string), nil))
	if r["expires_in"] != float64(9*60) {
		t.Fatalf("refresh expires_in = %v", r["expires_in"])
	}
	e.clock.Advance(10 * time.Minute)
	oauthErr(t, e.refresh(f, r["refresh_token"].(string), nil), http.StatusBadRequest, "invalid_grant")
	if _, _, err := e.m.VerifyAccessToken(context.Background(), r["access_token"].(string)); !isDesc(err, descExpired) {
		t.Fatalf("expired family: %v", err)
	}
}

func TestMCPTokenRequestRules(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("App", loopbackCB)
	f := newFlow(id, loopbackCB)
	code := e.code(newBrowser(), f)
	base := func() url.Values {
		return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {f.verifier}, "client_id": {id}}
	}

	// JSON body, repeated parameter, missing parameters, unknown grant type.
	r := postJSON(OAuthTokenPath, `{"grant_type":"authorization_code"}`)
	oauthErr(t, e.do(r), http.StatusBadRequest, "invalid_request")
	dup := base()
	dup.Add("code", code)
	oauthErr(t, e.do(oauthPost(OAuthTokenPath, dup)), http.StatusBadRequest, "invalid_request")
	for _, k := range []string{"code", "code_verifier", "grant_type"} {
		v := base()
		v.Del(k)
		oauthErr(t, e.do(oauthPost(OAuthTokenPath, v)), http.StatusBadRequest, "invalid_request")
	}
	v := base()
	v.Set("grant_type", "client_credentials")
	oauthErr(t, e.do(oauthPost(OAuthTokenPath, v)), http.StatusBadRequest, "unsupported_grant_type")
	big := base()
	big.Set("pad", strings.Repeat("x", 16<<10))
	oauthErr(t, e.do(oauthPost(OAuthTokenPath, big)), http.StatusBadRequest, "invalid_request")
	// A BOM stays part of the first name, so grant_type is missing (as on the Worker).
	bom := oauthPost(OAuthTokenPath, base())
	noGT := base()
	noGT.Del("grant_type")
	bom.Body = io.NopCloser(strings.NewReader("\ufeffgrant_type=authorization_code&" + noGT.Encode()))
	bom.ContentLength = -1
	if m := oauthErr(t, e.do(bom), http.StatusBadRequest, "invalid_request"); m["error_description"] != "grant_type is required" {
		t.Errorf("BOM: %v", m)
	}
	// Bodies url.ParseQuery refuses (the Worker refuses the same ones).
	for _, tail := range []string{"&pad=%zz", "&pad=a;b"} {
		r := oauthPost(OAuthTokenPath, base())
		r.Body = io.NopCloser(strings.NewReader(base().Encode() + tail))
		r.ContentLength = -1
		oauthErr(t, e.do(r), http.StatusBadRequest, "invalid_request")
	}

	// Client authentication.
	for name, mut := range map[string]func(r *http.Request){
		"client_secret": func(r *http.Request) {
			v := base()
			v.Set("client_secret", "x")
			r.Body = io.NopCloser(strings.NewReader(v.Encode()))
		},
		"basic with password": func(r *http.Request) { r.SetBasicAuth(url.QueryEscape(id), "secret") },
		"bearer":              func(r *http.Request) { r.Header.Set("Authorization", "Bearer x") },
		// Exactly one space and padded base64 (the Worker parses the same way).
		"basic unpadded":   func(r *http.Request) { r.Header.Set("Authorization", unpaddedBasic(id)) },
		"basic two spaces": func(r *http.Request) { r.Header.Set("Authorization", "Basic  "+basic(id, "")) },
		"basic tab":        func(r *http.Request) { r.Header.Set("Authorization", "Basic\t"+basic(id, "")) },
		"two headers": func(r *http.Request) {
			r.Header.Add("Authorization", "Basic "+basic(id, ""))
			r.Header.Add("Authorization", "Basic "+basic(id, ""))
		},
	} {
		r := oauthPost(OAuthTokenPath, base())
		mut(r)
		res := e.do(r)
		oauthErr(t, res, http.StatusUnauthorized, "invalid_client")
		if res.Header.Get("WWW-Authenticate") != `Basic realm="Backyard"` {
			t.Errorf("%s: WWW-Authenticate = %q", name, res.Header.Get("WWW-Authenticate"))
		}
	}
	v = base()
	v.Del("client_id")
	oauthErr(t, e.do(oauthPost(OAuthTokenPath, v)), http.StatusUnauthorized, "invalid_client")
	v.Set("client_id", "bydc_forged")
	oauthErr(t, e.do(oauthPost(OAuthTokenPath, v)), http.StatusUnauthorized, "invalid_client")
	v.Set("client_id", "https://evil.example/client")
	oauthErr(t, e.do(oauthPost(OAuthTokenPath, v)), http.StatusUnauthorized, "invalid_client")
	// Basic and body disagree.
	other := e.register("Other", loopbackCB)
	r = oauthPost(OAuthTokenPath, base())
	r.SetBasicAuth(url.QueryEscape(other), "")
	oauthErr(t, e.do(r), http.StatusBadRequest, "invalid_request")
	// Basic with an empty password, without client_id in the body: ok.
	v = base()
	v.Del("client_id")
	r = oauthPost(OAuthTokenPath, v)
	r.SetBasicAuth(url.QueryEscape(id), "")
	if res := e.do(r); res.StatusCode != http.StatusOK {
		t.Fatalf("basic with empty password: %d %s", res.StatusCode, body(t, res))
	}
	// Wrong method; cookies are irrelevant and never set.
	if res := e.do(req("GET", testIssuer+OAuthTokenPath)); res.StatusCode != http.StatusMethodNotAllowed || res.Header.Get("Allow") != "POST" {
		t.Errorf("GET token = %d", res.StatusCode)
	}
}

func basic(user, pass string) string {
	r, _ := http.NewRequest("GET", "/", nil)
	r.SetBasicAuth(url.QueryEscape(user), pass)
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Basic ")
}

// unpaddedBasic is Basic for a public client with the base64 padding
// stripped: leading characters of the id are %-escaped (the server
// unescapes them) until the padded form has padding to strip.
func unpaddedBasic(id string) string {
	for k := 0; k < 3; k++ {
		user := ""
		for _, c := range []byte(id[:k]) {
			user += fmt.Sprintf("%%%02x", c)
		}
		if b := base64.StdEncoding.EncodeToString([]byte(user + id[k:] + ":")); strings.HasSuffix(b, "=") {
			return "Basic " + strings.TrimRight(b, "=")
		}
	}
	panic("unreachable")
}

func TestMCPRefresh(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		id := e.register("App", loopbackCB)
		f := newFlow(id, loopbackCB)
		_, r1 := e.tokens(newBrowser(), f)

		m2 := jsonBody(t, e.refresh(f, r1, url.Values{"resource": {testResource}}))
		r2 := m2["refresh_token"].(string)
		if r2 == r1 || m2["scope"] != "mcp offline_access" || m2["expires_in"] != float64(3600) {
			t.Fatalf("rotation: %v", m2)
		}
		// Grace: R1 again right away returns the identical R2.
		g := jsonBody(t, e.refresh(f, r1, nil))
		if g["refresh_token"] != r2 {
			t.Fatalf("grace = %v, want %s", g["refresh_token"], r2)
		}
		// A narrower scope only changes the response; a wider one fails.
		m3 := jsonBody(t, e.refresh(f, r2, url.Values{"scope": {"mcp"}}))
		r3 := m3["refresh_token"].(string)
		if m3["scope"] != "mcp" {
			t.Errorf("narrowed scope = %v", m3["scope"])
		}
		oauthErr(t, e.refresh(f, r3, url.Values{"scope": {"mcp admin"}}), http.StatusBadRequest, "invalid_scope")
		oauthErr(t, e.refresh(f, r3, url.Values{"resource": {"https://evil.example/__backyard/mcp"}}), http.StatusBadRequest, "invalid_target")
		// Forged MAC, unknown grant, another client: invalid_grant, no revocation.
		forged := r3[:len(r3)-3] + "AAA"
		oauthErr(t, e.refresh(f, forged, nil), http.StatusBadRequest, "invalid_grant")
		oauthErr(t, e.refresh(f, e.m.signRefresh(newGrantID(), 1), nil), http.StatusBadRequest, "invalid_grant")
		oauthErr(t, e.refresh(f, e.m.signRefresh(gidOfAccess(t, e, m3["access_token"].(string)), 0), nil), http.StatusBadRequest, "invalid_grant")
		other := &flow{clientID: e.register("Other", loopbackCB)}
		oauthErr(t, e.refresh(other, r3, nil), http.StatusBadRequest, "invalid_grant")
		oauthErr(t, e.refresh(f, "", nil), http.StatusBadRequest, "invalid_request")
		if e.grantCount() != 1 {
			t.Fatal("a rejected refresh revoked the grant")
		}
		// R3 is still good (no reuse was triggered by the failures above).
		m4 := jsonBody(t, e.refresh(f, r3, nil))
		if m4["refresh_token"] == nil {
			t.Fatalf("R3 → R4: %v", m4)
		}
		// An old generation (R1, whose successor R2 was used): reuse → the
		// whole grant is revoked, R4 fails too.
		oauthErr(t, e.refresh(f, r1, nil), http.StatusBadRequest, "invalid_grant")
		oauthErr(t, e.refresh(f, m4["refresh_token"].(string), nil), http.StatusBadRequest, "invalid_grant")
		if _, _, err := e.m.VerifyAccessToken(context.Background(), m4["access_token"].(string)); !isDesc(err, descRevoked) {
			t.Errorf("access token after reuse: %v", err)
		}
		if acts := strings.Join(e.auditActions(), ","); !strings.Contains(acts, "mcp.refresh_reuse") {
			t.Errorf("audit = %v", acts)
		}
	})
}

// A lower auth.token_ttl after redemption must not shorten an existing
// family: its exp is fixed at redemption, so pruning keeps the grant (and
// its generation) and a replayed rotated refresh token still revokes.
func TestMCPRefreshReuseAfterTTLChange(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		id := e.register("App", loopbackCB)
		f := newFlow(id, loopbackCB)
		at, r1 := e.tokens(newBrowser(), f)
		gid := gidOfAccess(t, e, at)
		e.m.waitBackground()
		g, err := e.m.loadGrant(context.Background(), gid)
		if err != nil || g == nil {
			t.Fatalf("grant: %v %v", g, err)
		}

		e.clock.Advance(48 * time.Hour)
		e.m.family = 24 * time.Hour // a restart with a lower auth.token_ttl
		r2 := jsonBody(t, e.refresh(f, r1, nil))["refresh_token"].(string)
		e.clock.Advance(2 * time.Minute)
		if m := jsonBody(t, e.refresh(f, r2, nil)); m["refresh_token"] == nil {
			t.Fatalf("R2 → R3: %v", m)
		}
		e.code(newBrowser(), newFlow(id, loopbackCB)) // triggers a prune
		e.m.waitBackground()

		if g2, err := e.m.loadGrant(context.Background(), gid); err != nil || g2 == nil || g2.Exp != g.Exp || g2.Gen != 3 {
			t.Fatalf("grant after pruning = %+v, %v; want exp %d (fixed at redemption) and gen 3", g2, err, g.Exp)
		}

		e.clock.Advance(2 * time.Minute)
		oauthErr(t, e.refresh(f, r2, nil), http.StatusBadRequest, "invalid_grant")
		if g, _ := e.m.loadGrant(context.Background(), gid); g != nil {
			t.Fatal("a replayed rotated refresh token did not revoke the grant")
		}
		if acts := strings.Join(e.auditActions(), ","); !strings.Contains(acts, "mcp.refresh_reuse") {
			t.Errorf("audit = %v", acts)
		}
	})
}

// Rotation keeps O(1) state per grant: the grant record itself holds the
// newest generation. Reuse detection and the grace window work as ever, and
// a replayed code revokes after any number of refreshes.
func TestMCPRefreshKeepsOneRecord(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		f := newFlow(e.register("App", loopbackCB), loopbackCB)
		code := e.code(newBrowser(), f)
		m := jsonBody(t, e.exchange(f, code, nil))
		gid := gidOfAccess(t, e, m["access_token"].(string))
		rts := []string{"", m["refresh_token"].(string)} // rts[n] = RT(n)
		for n := 1; n <= 20; n++ {
			rts = append(rts, jsonBody(t, e.refresh(f, rts[n], nil))["refresh_token"].(string))
		}
		colls, err := e.docs.Collections(context.Background(), docstore.SystemSite)
		if err != nil || !slices.Equal(colls, []string{auditCollection, grantsCollection}) {
			t.Fatalf("system collections after 20 refreshes = %v, %v; want the audit log and the grant", colls, err)
		}
		if g, _ := e.m.loadGrant(context.Background(), gid); g == nil || g.Gen != 21 || e.grantCount() != 1 {
			t.Fatalf("grant after 20 refreshes = %+v (%d grants), want gen 21", g, e.grantCount())
		}
		// RT(20) again within the grace window: the same RT(21).
		if g := jsonBody(t, e.refresh(f, rts[20], nil)); g["refresh_token"] != rts[21] {
			t.Fatalf("grace = %v, want RT(21)", g["refresh_token"])
		}
		// The code again: a replay.
		oauthErr(t, e.exchange(f, code, nil), http.StatusBadRequest, "invalid_grant")
		if e.grantCount() != 0 {
			t.Fatal("a replayed code did not revoke the grant")
		}
		if acts := strings.Join(e.auditActions(), ","); !strings.Contains(acts, "mcp.code_replay") {
			t.Errorf("audit = %v", acts)
		}

		// An old generation is a reuse.
		f2 := newFlow(f.clientID, loopbackCB)
		_, r1 := e.tokens(newBrowser(), f2)
		r := r1
		for range 3 {
			r = jsonBody(t, e.refresh(f2, r, nil))["refresh_token"].(string)
		}
		oauthErr(t, e.refresh(f2, r1, nil), http.StatusBadRequest, "invalid_grant")
		oauthErr(t, e.refresh(f2, r, nil), http.StatusBadRequest, "invalid_grant")
		if acts := strings.Join(e.auditActions(), ","); !strings.Contains(acts, "mcp.refresh_reuse") {
			t.Errorf("audit = %v", acts)
		}
	})
}

// A rotation that read the grant before another transition landed decides
// again on what that one wrote: RT(n) that lost to one rotation of RT(n) gets
// the grace answer, one that lost to two rotations is a reuse, and one whose
// grant was revoked in between is missing.
func TestMCPRotateDecidesAgain(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		f := newFlow(e.register("App", loopbackCB), loopbackCB)
		ctx := context.Background()
		connect := func() (gid string) {
			_, r1 := e.tokens(newBrowser(), f)
			p, _ := e.m.parseRefresh(r1)
			e.m.waitBackground() // the prune of the consent reads m.docs
			return p.G
		}
		raw := e.m.docs
		defer func() { e.m.docs = raw }()
		race := func(gid string, before func()) (string, *grantRecord) {
			e.m.docs = &beforeUpdateIf{Store: raw, hook: before}
			defer func() { e.m.docs = raw }()
			out, g, err := e.m.rotate(ctx, gid, 1, true)
			if err != nil {
				t.Fatal(err)
			}
			return out, g
		}
		rotate := func(gid string, from int64) {
			if out, _, err := e.m.rotate(ctx, gid, from, true); err != nil || out != rotateOK {
				t.Fatalf("rotate(%d) = %q, %v", from, out, err)
			}
		}

		gid := connect()
		if out, g := race(gid, func() { rotate(gid, 1) }); out != rotateGrace || g == nil || g.Gen != 2 {
			t.Fatalf("lost to one rotation = %q %+v, want grace at gen 2", out, g)
		}
		gid = connect()
		if out, _ := race(gid, func() { rotate(gid, 1); rotate(gid, 2) }); out != rotateReuse {
			t.Fatalf("lost to two rotations = %q, want reuse", out)
		}
		if g, _ := e.m.loadGrant(ctx, gid); g != nil {
			t.Fatal("the grant survived a reuse")
		}
		gid = connect()
		if out, _ := race(gid, func() {
			if _, err := e.m.deleteGrant(ctx, gid); err != nil {
				t.Fatal(err)
			}
		}); out != rotateMissing {
			t.Fatalf("revoked in between = %q, want missing", out)
		}
	})
}

// beforeUpdateIf runs its hook once, before the first guarded update.
type beforeUpdateIf struct {
	docstore.Store
	hook func()
}

func (b *beforeUpdateIf) UpdateIf(ctx context.Context, site, collection, id string, rev docstore.Rev, patch map[string]any, actor string) (*docstore.Doc, error) {
	if f := b.hook; f != nil {
		b.hook = nil
		f()
	}
	return b.Store.UpdateIf(ctx, site, collection, id, rev, patch, actor)
}

func TestMCPRefreshGraceWindow(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	f := newFlow(e.register("App", loopbackCB), loopbackCB)
	_, r1 := e.tokens(newBrowser(), f)
	r2 := jsonBody(t, e.refresh(f, r1, nil))["refresh_token"]
	e.clock.Advance(60 * time.Second)
	if g := jsonBody(t, e.refresh(f, r1, nil)); g["refresh_token"] != r2 {
		t.Fatalf("at 60 s: %v", g)
	}
	e.clock.Advance(time.Second)
	oauthErr(t, e.refresh(f, r1, nil), http.StatusBadRequest, "invalid_grant")
	if e.grantCount() != 0 {
		t.Fatal("reuse after the grace window must revoke")
	}
}

func TestMCPRegate(t *testing.T) {
	// A user removed from auth.allowed_emails loses the connection at the
	// next refresh: invalid_grant and the grant is deleted.
	e := newMCPEnv(t, ModeHeader, func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
		c.Auth.Header.EmailHeader = "X-Forwarded-Email"
		c.Auth.AllowedEmails = []string{"alice@gmail.com", "bob@gmail.com"}
	})
	f := newFlow(e.register("App", loopbackCB), loopbackCB)
	b := newBrowser()
	b.remote = "10.0.0.7:1"
	b.headers.Set("X-Forwarded-Email", "alice@gmail.com")
	at, rt := e.tokens(b, f)
	if u, _, err := e.m.VerifyAccessToken(context.Background(), at); err != nil || u.ID != "alice@gmail.com" || u.Admin {
		t.Fatalf("verify: %+v %v", u, err)
	}
	e.cfg.Auth.AllowedEmails = []string{"bob@gmail.com"}
	m := oauthErr(t, e.refresh(f, rt, nil), http.StatusBadRequest, "invalid_grant")
	if m["error_description"] != "access removed by policy" || e.grantCount() != 0 {
		t.Fatalf("policy refresh: %v, grants %d", m, e.grantCount())
	}
	if acts := strings.Join(e.auditActions(), ","); !strings.Contains(acts, "mcp.revoke:policy") {
		t.Errorf("audit = %v", acts)
	}

	// The access token path: the next MCP request fails and deletes the grant.
	e.cfg.Auth.AllowedEmails = []string{"alice@gmail.com"}
	f2 := newFlow(e.register("App", loopbackCB), loopbackCB)
	at2, _ := e.tokens(b, f2)
	e.cfg.Auth.AllowedEmails = []string{"bob@gmail.com"}
	if _, _, err := e.m.VerifyAccessToken(context.Background(), at2); !isDesc(err, descPolicy) {
		t.Fatalf("policy verify: %v", err)
	}
	if _, _, err := e.m.VerifyAccessToken(context.Background(), at2); !isDesc(err, descRevoked) {
		t.Fatalf("after policy: %v", err)
	}

	// At code exchange as well.
	e.cfg.Auth.AllowedEmails = []string{"alice@gmail.com"}
	f3 := newFlow(e.register("App", loopbackCB), loopbackCB)
	code := e.code(b, f3)
	e.cfg.Auth.AllowedEmails = []string{"bob@gmail.com"}
	if m := oauthErr(t, e.exchange(f3, code, nil), http.StatusBadRequest, "invalid_grant"); m["error_description"] != "access removed by policy" {
		t.Fatalf("policy exchange: %v", m)
	}

	// Admin is recomputed from the configuration on every use.
	e.cfg.Auth.AllowedEmails = []string{"alice@gmail.com"}
	f4 := newFlow(e.register("App", loopbackCB), loopbackCB)
	at4, _ := e.tokens(b, f4)
	e.cfg.Auth.Admins = []string{"alice@gmail.com"}
	if u, _, err := e.m.VerifyAccessToken(context.Background(), at4); err != nil || !u.Admin {
		t.Fatalf("admin recomputed: %+v %v", u, err)
	}
}

func TestMCPRedirectPolicyAtTokenTime(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	f := newFlow(e.register("Claude", claudeCB), claudeCB)
	code := e.code(newBrowser(), f)
	f2 := newFlow(f.clientID, claudeCB)
	_, rt := e.tokens(newBrowser(), f2)
	// The admin removes claude.ai from mcp.redirect_uris (same secret and store).
	strict := newMCPEnv(t, ModeNone, func(c *config.Config) { c.MCP.RedirectURIs = []string{"https://claude.com/api/mcp/auth_callback"} },
		func(o *Options) { o.Docs = e.docs })
	oauthErr(t, strict.exchange(f, code, nil), http.StatusBadRequest, "invalid_grant")
	m := oauthErr(t, strict.refresh(f2, rt, nil), http.StatusBadRequest, "invalid_grant")
	if m["error_description"] != errRedirectGone {
		t.Errorf("description = %v", m["error_description"])
	}
	// Nothing was consumed: the old configuration still works.
	if res := e.exchange(f, code, nil); res.StatusCode != http.StatusOK {
		t.Errorf("exchange under the old policy: %d", res.StatusCode)
	}
}

func TestMCPRegateModeSwitch(t *testing.T) {
	// A guest grant from mode none is refused once the server runs another
	// mode with the same secret and store.
	none := newMCPEnv(t, ModeNone, nil)
	f := newFlow(none.register("App", loopbackCB), loopbackCB)
	at, rt := none.tokens(newBrowser(), f)
	header := newMCPEnv(t, ModeHeader, func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
		c.Auth.Header.EmailHeader = "X-Forwarded-Email"
	}, func(o *Options) { o.Docs = none.docs })
	if _, _, err := header.m.VerifyAccessToken(context.Background(), at); !isDesc(err, descPolicy) {
		t.Fatalf("guest token in header mode: %v", err)
	}
	oauthErr(t, header.refresh(f, rt, nil), http.StatusBadRequest, "invalid_grant")
	if none.grantCount() != 0 {
		t.Fatal("the guest grant survived the mode switch")
	}
	// A static-token principal never holds a grant.
	g := &grantRecord{ID: newGrantID(), User: grantUser{ID: "token:ci"}}
	if _, err := header.m.regate(context.Background(), g); !errors.Is(err, errPolicy) {
		t.Fatalf("token: subject: %v", err)
	}
}

func TestMCPVerifyAccessToken(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	f := newFlow(e.register("App", loopbackCB), loopbackCB)
	at, _ := e.tokens(newBrowser(), f)
	ctx := context.Background()
	gid := gidOfAccess(t, e, at)

	// Malformed: wrong prefix, bad MAC, too long, other credentials.
	apiTok, _, _ := e.a.tokens.Issue(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com"}, time.Hour)
	idpJWT := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "https://idp.example", "sub": "x"})
	idpTok, _ := idpJWT.SignedString([]byte("k"))
	for name, raw := range map[string]string{
		"empty": "", "prefix": "byrt_" + strings.TrimPrefix(at, "byat_"), "mac": at[:len(at)-2] + "AA",
		"too long": at + strings.Repeat("A", 4096), "api token": apiTok, "idp jwt": idpTok, "static": "ci-static-token",
		"other key": "byat_" + keys.Sign(e.m.refreshKey, []byte(`{"g":"`+gid+`","i":1,"x":9999999999}`)),
		"bad json":  "byat_" + keys.Sign(e.m.accessKey, []byte(`{"g":"`+gid+`","i":"1","x":9999999999}`)),
		"bad gid":   "byat_" + keys.Sign(e.m.accessKey, []byte(`{"g":"../x","i":1,"x":9999999999}`)),
		"code":      e.m.signCode(gid, 9999999999),
	} {
		if _, _, err := e.m.VerifyAccessToken(ctx, raw); !isDesc(err, descMalformed) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Audience: a grant made for another resource (e.g. public_url changed).
	moved := newMCPEnv(t, ModeNone, func(c *config.Config) { c.Server.PublicURL = "https://new.backyard.test" },
		func(o *Options) { o.Docs = e.docs })
	if _, _, err := moved.m.VerifyAccessToken(ctx, at); !isDesc(err, descAudience) {
		t.Errorf("moved hub: %v", err)
	}
	// Expired (1 h).
	e.clock.Advance(3600 * time.Second)
	if _, _, err := e.m.VerifyAccessToken(ctx, at); !isDesc(err, descExpired) {
		t.Errorf("expired: %v", err)
	}
	// Revoked: deleted grant; unredeemed grant.
	at2, _ := e.tokens(newBrowser(), newFlow(f.clientID, loopbackCB))
	if ok, err := e.m.Revoke(ctx, gidOfAccess(t, e, at2), nil, "hub"); !ok || err != nil {
		t.Fatalf("Revoke: %v %v", ok, err)
	}
	if _, _, err := e.m.VerifyAccessToken(ctx, at2); !isDesc(err, descRevoked) {
		t.Errorf("revoked: %v", err)
	}
	f3 := newFlow(f.clientID, loopbackCB)
	code := e.code(newBrowser(), f3)
	cp, _ := e.m.parseCode(code)
	pending, _ := e.m.loadGrant(ctx, cp.G)
	fake, _ := e.m.issueAccess(pending)
	if _, _, err := e.m.VerifyAccessToken(ctx, fake); !isDesc(err, descRevoked) {
		t.Errorf("unredeemed grant: %v", err)
	}
	// A store failure is not an invalid token.
	broken := newMCPEnv(t, ModeNone, nil, func(o *Options) { o.Docs = failingStore{e.docs} })
	if _, _, err := broken.m.VerifyAccessToken(ctx, at2); err == nil || errors.Is(err, ErrInvalidToken) {
		t.Errorf("store failure: %v", err)
	}
}

// failingStore fails every read.
type failingStore struct{ docstore.Store }

func (failingStore) Get(context.Context, string, string, string) (*docstore.Doc, error) {
	return nil, errors.New("store down")
}

func TestMCPTokenIsolation(t *testing.T) {
	// An MCP access token is not an API credential.
	sum := sha256.Sum256([]byte("ci-static-token"))
	e := newMCPEnv(t, ModeHeader, func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
		c.Auth.Header.EmailHeader = "X-Forwarded-Email"
		c.Auth.Tokens = []config.StaticToken{{Name: "ci", SHA256: hex.EncodeToString(sum[:])}}
	})
	f := newFlow(e.register("App", loopbackCB), loopbackCB)
	b := newBrowser()
	b.remote = "10.0.0.7:1"
	b.headers.Set("X-Forwarded-Email", "alice@acme.com")
	at, rt := e.tokens(b, f)
	for _, tok := range []string{at, rt} {
		if _, err := e.a.authenticateBearer(context.Background(), tok); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("authenticateBearer(%s…) = %v", tok[:5], err)
		}
		r := req("GET", testIssuer+"/__backyard/api/me")
		r.Header.Set("Authorization", "Bearer "+tok)
		if _, err := e.a.Authenticate(nil, r); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("Authenticate with %s…: %v", tok[:5], err)
		}
	}
	// And API and static tokens are not MCP tokens.
	apiTok, _, _ := e.a.tokens.Issue(&identity.User{ID: "alice@acme.com", Email: "alice@acme.com"}, time.Hour)
	for _, tok := range []string{apiTok, "ci-static-token"} {
		if _, _, err := e.m.VerifyAccessToken(context.Background(), tok); !isDesc(err, descMalformed) {
			t.Errorf("VerifyAccessToken(%q): %v", tok[:5], err)
		}
	}
}

func TestMCPRotateConcurrency(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		f := newFlow(e.register("App", loopbackCB), loopbackCB)
		code := e.code(newBrowser(), f)
		cp, _ := e.m.parseCode(code)
		ctx := context.Background()

		// 50 redemptions of one code: exactly one ok, the grant is revoked.
		var mu sync.Mutex
		counts := map[string]int{}
		var wg sync.WaitGroup
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				out, _, err := e.m.rotate(ctx, cp.G, 0, false)
				if err != nil {
					out = "error: " + err.Error()
				}
				mu.Lock()
				counts[out]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		if counts[rotateOK] != 1 || counts[rotateOK]+counts[rotateReuse]+counts[rotateMissing] != 50 {
			t.Fatalf("code redemptions: %v", counts)
		}
		if g, _ := e.m.loadGrant(ctx, cp.G); g != nil {
			t.Fatal("the grant survived concurrent redemptions")
		}

		// 50 refreshes of one refresh token: all ok or grace, never reuse,
		// and all of them hand out the same successor.
		f2 := newFlow(f.clientID, loopbackCB)
		_, r1 := e.tokens(newBrowser(), f2)
		results := make(chan string, 50)
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res := e.refresh(f2, r1, nil)
				if res.StatusCode != http.StatusOK {
					results <- "status " + res.Status + body(t, res)
					return
				}
				results <- jsonBody(t, res)["refresh_token"].(string)
			}()
		}
		wg.Wait()
		close(results)
		seen := map[string]int{}
		for r := range results {
			seen[r]++
		}
		if len(seen) != 1 || seen[e.m.signRefresh(gidOfAccess(t, e, mustAccess(t, e, r1)), 2)] != 50 {
			t.Fatalf("concurrent refreshes: %v", seen)
		}
	})
}

// mustAccess finds an access token's grant via a refresh token (both carry
// the gid) and returns an access token for it.
func mustAccess(t *testing.T, e *mcpEnv, rt string) string {
	t.Helper()
	p, ok := e.m.parseRefresh(rt)
	if !ok {
		t.Fatal("not a refresh token")
	}
	g, err := e.m.loadGrant(context.Background(), p.G)
	if err != nil || g == nil {
		t.Fatalf("grant: %v", err)
	}
	at, _ := e.m.issueAccess(g)
	return at
}

func TestMCPRevokeEndpoint(t *testing.T) {
	e := newMCPEnv(t, ModeNone, nil)
	id := e.register("App", loopbackCB)
	f := newFlow(id, loopbackCB)
	at, rt := e.tokens(newBrowser(), f)
	ok200 := func(name string, form url.Values) {
		t.Helper()
		res := e.do(oauthPost(OAuthRevokePath, form))
		if b := body(t, res); res.StatusCode != http.StatusOK || b != "" {
			t.Errorf("%s: %d %q", name, res.StatusCode, b)
		}
	}
	// Unknown, malformed or foreign tokens: 200, nothing revoked.
	other := e.register("Other", loopbackCB)
	ok200("unknown", url.Values{"token": {e.m.signRefresh(newGrantID(), 1)}, "client_id": {id}})
	ok200("garbage", url.Values{"token": {"garbage"}, "client_id": {id}, "token_type_hint": {"refresh_token"}})
	ok200("other client", url.Values{"token": {rt}, "client_id": {other}})
	if _, _, err := e.m.VerifyAccessToken(context.Background(), at); err != nil {
		t.Fatalf("a foreign revocation revoked: %v", err)
	}
	// Errors.
	oauthErr(t, e.do(oauthPost(OAuthRevokePath, url.Values{"client_id": {id}})), http.StatusBadRequest, "invalid_request")
	oauthErr(t, e.do(oauthPost(OAuthRevokePath, url.Values{"token": {rt}})), http.StatusUnauthorized, "invalid_client")
	oauthErr(t, e.do(oauthPost(OAuthRevokePath, url.Values{"token": {rt}, "client_id": {id}, "client_secret": {"s"}})), http.StatusUnauthorized, "invalid_client")
	// A refresh token revokes the grant.
	ok200("refresh token", url.Values{"token": {rt}, "client_id": {id}})
	if _, _, err := e.m.VerifyAccessToken(context.Background(), at); !isDesc(err, descRevoked) {
		t.Fatalf("after revocation: %v", err)
	}
	oauthErr(t, e.refresh(f, rt, nil), http.StatusBadRequest, "invalid_grant")
	// An access token revokes the grant too.
	at2, rt2 := e.tokens(newBrowser(), newFlow(id, loopbackCB))
	ok200("access token", url.Values{"token": {at2}, "client_id": {id}})
	oauthErr(t, e.refresh(f, rt2, nil), http.StatusBadRequest, "invalid_grant")
	if acts := strings.Join(e.auditActions(), ","); strings.Count(acts, "mcp.revoke:rfc7009") != 2 {
		t.Errorf("audit = %v", acts)
	}
}

func TestMCPGrantsAPI(t *testing.T) {
	e := newMCPEnv(t, ModeHeader, func(c *config.Config) {
		c.Server.TrustedProxies = []string{"10.0.0.0/8"}
		c.Auth.Header.EmailHeader = "X-Forwarded-Email"
	})
	ctx := context.Background()
	as := func(email string) *browser {
		b := newBrowser()
		b.remote = "10.0.0.7:1"
		b.headers.Set("X-Forwarded-Email", email)
		return b
	}
	cimdLike := e.register("Claude", claudeCB)
	for i := range 3 {
		f := newFlow(cimdLike, claudeCB)
		e.tokens(as("alice@acme.com"), f)
		e.clock.Advance(time.Duration(i+1) * time.Second)
	}
	e.tokens(as("bob@acme.com"), newFlow(cimdLike, claudeCB))
	// A pending grant (consent without exchange) is not listed.
	e.code(as("alice@acme.com"), newFlow(cimdLike, claudeCB))

	gs, err := e.m.Grants(ctx, "alice@acme.com")
	if err != nil || len(gs) != 3 {
		t.Fatalf("Grants = %d %v", len(gs), err)
	}
	for i := 1; i < len(gs); i++ {
		if gs[i].CreatedAt.After(gs[i-1].CreatedAt) {
			t.Fatal("grants are not newest first")
		}
	}
	g := gs[0]
	if g.User.Email != "alice@acme.com" || g.ClientName != "Claude" || g.ClientKind != clientKindDCR || g.ClientHost != "" ||
		g.RedirectURI != claudeCB || g.Scope != "mcp offline_access" || len(g.ID) != 32 {
		t.Fatalf("grant = %+v", g)
	}
	if got, err := e.m.GrantByID(ctx, g.ID); err != nil || got == nil || got.ID != g.ID {
		t.Fatalf("GrantByID = %+v %v", got, err)
	}
	for _, bad := range []string{newGrantID(), "nope", "", "../x"} {
		if got, err := e.m.GrantByID(ctx, bad); got != nil || err != nil {
			t.Errorf("GrantByID(%q) = %+v %v", bad, got, err)
		}
	}
	if gs, _ := e.m.Grants(ctx, "nobody@acme.com"); len(gs) != 0 {
		t.Fatal("grants of nobody")
	}
	admin := &identity.User{ID: "boss@acme.com", Email: "boss@acme.com", Admin: true}
	if ok, err := e.m.Revoke(ctx, g.ID, admin, "hub"); !ok || err != nil {
		t.Fatalf("Revoke = %v %v", ok, err)
	}
	if ok, err := e.m.Revoke(ctx, g.ID, admin, "hub"); ok || err != nil {
		t.Fatalf("second Revoke = %v %v", ok, err)
	}
	n, err := e.m.RevokeUser(ctx, "alice@acme.com", admin, "hub")
	if err != nil || n != 3 { // two live, one pending
		t.Fatalf("RevokeUser = %d %v", n, err)
	}
	if gs, _ := e.m.Grants(ctx, "alice@acme.com"); len(gs) != 0 {
		t.Fatal("RevokeUser left grants")
	}
	if gs, _ := e.m.Grants(ctx, "bob@acme.com"); len(gs) != 1 {
		t.Fatal("RevokeUser touched another user")
	}
	// The stored audit records name the actor and never full ids or tokens.
	q := docstore.Query{Limit: 100}
	_ = q.Normalize(100)
	page, _ := e.docs.Query(ctx, docstore.SystemSite, auditCollection, q)
	var revokes int
	for _, d := range page.Docs {
		for k, v := range d.Data {
			if s, ok := v.(string); ok && (strings.Contains(s, g.ID) || strings.Contains(s, "byat_") || strings.Contains(s, "bydc_")) {
				t.Errorf("audit field %s leaks %q", k, s)
			}
		}
		if d.Data["action"] == "mcp.revoke" {
			revokes++
			if d.Data["user"] != "boss@acme.com" || d.Data["via"] != "hub" {
				t.Errorf("revoke record = %v", d.Data)
			}
		}
		if d.Data["action"] == "mcp.authorize" && (d.Data["ip"] != "10.0.0.7" || d.Data["redirect_host"] != "claude.ai" ||
			d.Data["client_kind"] != clientKindDCR || len(d.Data["grant"].(string)) != 8 || d.Data["at"] == nil) {
			t.Errorf("authorize record = %v", d.Data)
		}
	}
	if revokes != 2 {
		t.Errorf("revoke records = %d", revokes)
	}
}

func TestMCPPrune(t *testing.T) {
	storeVariants(t, func(t *testing.T, opts ...func(*Options)) {
		e := newMCPEnv(t, ModeNone, nil, opts...)
		id := e.register("App", loopbackCB)
		// A pending grant (expires 900 s after consent) and a live one.
		e.code(newBrowser(), newFlow(id, loopbackCB))
		f := newFlow(id, loopbackCB)
		_, rt := e.tokens(newBrowser(), f)
		e.m.waitBackground()
		if e.grantCount() != 2 {
			t.Fatalf("grants = %d", e.grantCount())
		}
		e.clock.Advance(901 * time.Second)
		e.code(newBrowser(), newFlow(id, loopbackCB)) // triggers a prune
		e.m.waitBackground()
		if e.grantCount() != 2 { // the live one and the new pending one
			t.Fatalf("after pruning: %d grants", e.grantCount())
		}
		// Expired families go as well.
		e.clock.Advance(721 * time.Hour)
		e.code(newBrowser(), newFlow(id, loopbackCB))
		e.m.waitBackground()
		if e.grantCount() != 1 {
			t.Fatalf("after the family expired: %d grants", e.grantCount())
		}
		_ = rt
	})
}
