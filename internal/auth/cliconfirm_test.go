package auth

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/identity"
)

// TestProxyModeLogin: outside oidc mode /login is a plain redirect to the
// sanitized rd (the proxy in front of Backyard signs the browser in), as in
// the Cloudflare edition — not a 404 that strands the hub's re-login.
func TestProxyModeLogin(t *testing.T) {
	header := baseConfig("header")
	header.Server.TrustedProxies = []string{"10.0.0.0/8"}
	header.Auth.Header.EmailHeader = "X-Forwarded-Email"
	auths := map[string]*authenticator{
		"none":   newAuth(t, baseConfig("none"), Options{}),
		"header": newAuth(t, header, Options{}),
		"iap":    newIAPEnv(t).a,
		"alb":    newALBEnv(t, albARN).a,
	}
	const hub = "https://backyard.test/"
	cases := map[string]string{
		"":                                   hub,
		"/docs?doc=recipes#x":                "https://backyard.test/docs?doc=recipes#x",
		"https://foo.backyard.test/page?a=1": "https://foo.backyard.test/page?a=1",
		"https://backyard.test/__backyard/auth/cli?mode=display": "https://backyard.test/__backyard/auth/cli?mode=display",
		"https://evil.example/":                                  hub,
		"//evil.example/":                                        hub,
		"/\\evil.example":                                        hub,
		"https://foo.bar.backyard.test/":                         hub,
		"http://foo.backyard.test/":                              hub,
		"https://backyard.test:8443/":                            hub,
		"javascript:alert(1)":                                    hub,
		"/__backyard/auth/login?rd=/x":                           hub,
		"https://foo.backyard.test/__backyard/auth/login":        hub,
	}
	for mode, a := range auths {
		for rd, want := range cases {
			target := "https://backyard.test" + LoginPath
			if rd != "" {
				target += "?rd=" + url.QueryEscape(rd)
			}
			for _, m := range []string{"GET", "HEAD"} {
				res := serve(a.Handler(), req(m, target))
				if res.StatusCode != http.StatusFound || res.Header.Get("Location") != want {
					t.Errorf("%s %s rd=%q = %d %q, want %q", mode, m, rd, res.StatusCode, res.Header.Get("Location"), want)
				}
				if !strings.Contains(res.Header.Get("Cache-Control"), "no-store") {
					t.Errorf("%s: login redirect is cacheable", mode)
				}
			}
		}
		if res := serve(a.Handler(), req("POST", "https://backyard.test"+LoginPath)); res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s POST login = %d", mode, res.StatusCode)
		}
	}
}

var (
	csrfFieldRe = regexp.MustCompile(`name="csrf" value="([^"]*)"`)
	hiddenRe    = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)
)

// confirmationForm checks that res is the CLI confirmation page and returns
// its form fields.
func confirmationForm(t *testing.T, res *http.Response) url.Values {
	t.Helper()
	b := body(t, res)
	if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" {
		t.Fatalf("expected the confirmation page, got %d %q\n%s", res.StatusCode, res.Header.Get("Location"), b)
	}
	if !strings.Contains(b, `<form method="post" action="/__backyard/auth/cli">`) || !csrfFieldRe.MatchString(b) {
		t.Fatalf("no confirmation form:\n%s", b)
	}
	if strings.Contains(b, "eyJ") || strings.Contains(b, "<textarea") {
		t.Fatalf("confirmation page shows a token:\n%s", b)
	}
	h := res.Header
	if h.Get("X-Frame-Options") != "DENY" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		!strings.Contains(h.Get("Cache-Control"), "no-store") {
		t.Fatalf("confirmation page headers = %v", h)
	}
	v := url.Values{}
	for _, m := range hiddenRe.FindAllStringSubmatch(b, -1) {
		v.Set(m[1], html.UnescapeString(m[2]))
	}
	return v
}

// postForm submits the confirmation form like the browser does from the
// hub page.
func postForm(form url.Values, cookies ...*http.Cookie) *http.Request {
	enc := form.Encode()
	r := req("POST", "https://backyard.test"+CLIPath, cookies...)
	r.Body = io.NopCloser(strings.NewReader(enc))
	r.ContentLength = int64(len(enc))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Origin", "null")
	return r
}

func withHeaders(r *http.Request, kv ...string) *http.Request {
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Set(kv[i], kv[i+1])
	}
	return r
}

func with(v url.Values, key, value string) url.Values {
	c := url.Values{}
	for k, vs := range v {
		c[k] = append([]string(nil), vs...)
	}
	if value == "" {
		c.Del(key)
	} else {
		c.Set(key, value)
	}
	return c
}

// TestCLICrossSiteNeedsConfirmation: a page on another site (or a Backyard
// site, which is same-site) that navigates a signed-in browser to the CLI
// endpoint gets a confirmation page instead of a token for that user sent
// to a loopback port.
func TestCLICrossSiteNeedsConfirmation(t *testing.T) {
	e := newOIDCEnv(t, nil)
	sess := e.session()
	target := cliURL("http://127.0.0.1:5555/callback?keep=1", "st-123")

	// The CLI opening the URL (Sec-Fetch-Site: none), same-origin
	// navigations and browsers without Fetch Metadata and a foreign Referer
	// proceed at once.
	direct := [][]string{
		{"Sec-Fetch-Site", "none"},
		{"Sec-Fetch-Site", "same-origin"},
		{"Sec-Fetch-Site", "same-origin", "Referer", "https://evil.example/"},
		{},
		{"Referer", "https://backyard.test/docs"},
		{"Referer", "https://BACKYARD.test:443/"},
	}
	for _, hdrs := range direct {
		res := serve(e.a.Handler(), withHeaders(req("GET", target, sess), hdrs...))
		if res.StatusCode != http.StatusFound || !strings.HasPrefix(res.Header.Get("Location"), "http://127.0.0.1:5555/callback?") {
			t.Fatalf("%v = %d %q", hdrs, res.StatusCode, res.Header.Get("Location"))
		}
	}

	// Cross-site, same-site (a Backyard site) and legacy browsers with a
	// foreign Referer must confirm.
	confirm := [][]string{
		{"Sec-Fetch-Site", "cross-site"},
		{"Sec-Fetch-Site", "same-site"},
		{"Sec-Fetch-Site", "Cross-Site", "Referer", "https://backyard.test/"},
		{"Sec-Fetch-Site", "bogus"},
		{"Referer", "https://evil.example/attack"},
		{"Referer", "https://foo.backyard.test/"},
		{"Referer", "http://backyard.test/"},
		{"Referer", "https://backyard.test:8443/"},
		{"Referer", "not a url"},
	}
	for _, hdrs := range confirm {
		res := serve(e.a.Handler(), withHeaders(req("GET", target, sess), hdrs...))
		if res.StatusCode != http.StatusOK || res.Header.Get("Location") != "" {
			t.Fatalf("%v = %d %q (want the confirmation page)", hdrs, res.StatusCode, res.Header.Get("Location"))
		}
	}

	res := serve(e.a.Handler(), withHeaders(req("GET", target, sess), "Sec-Fetch-Site", "cross-site"))
	b := body(t, res)
	if !strings.Contains(b, "Allow the Backyard CLI on this computer to sign in as alice@acme.com?") ||
		!strings.Contains(b, "http://127.0.0.1:5555") {
		t.Fatalf("confirmation text:\n%s", b)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' http://127.0.0.1:5555") {
		t.Fatalf("CSP = %q", csp)
	}
	res = serve(e.a.Handler(), withHeaders(req("GET", target, sess), "Sec-Fetch-Site", "cross-site"))
	form := confirmationForm(t, res)
	if form.Get("redirect_uri") != "http://127.0.0.1:5555/callback?keep=1" || form.Get("state") != "st-123" ||
		form.Has("mode") || form.Get("csrf") == "" {
		t.Fatalf("form = %v", form)
	}

	// Allow: the confirmation mints the token and hands it to the CLI.
	ok := serve(e.a.Handler(), postForm(form, sess))
	if ok.StatusCode != http.StatusSeeOther || ok.Header.Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(ok.Header.Get("Cache-Control"), "no-store") {
		t.Fatalf("confirm = %d %v: %s", ok.StatusCode, ok.Header, body(t, ok))
	}
	dest, _ := url.Parse(ok.Header.Get("Location"))
	if dest.Host != "127.0.0.1:5555" || dest.Path != "/callback" || dest.Query().Get("keep") != "1" || dest.Query().Get("state") != "st-123" {
		t.Fatalf("redirect = %s", dest)
	}
	tk := NewTokens(testSecret, 0)
	tk.now = e.clock.Now
	if u, err := tk.Verify(dest.Query().Get("token")); err != nil || u.ID != "alice@acme.com" {
		t.Fatalf("token = %+v, %v", u, err)
	}
	// The browser's own Origin is fine too.
	if res := serve(e.a.Handler(), withHeaders(postForm(form, sess), "Origin", "https://backyard.test")); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("origin hub = %d", res.StatusCode)
	}

	// Forged or replayed confirmations mint nothing.
	other, err := e.a.sess.seal(&identity.User{ID: "bob@acme.com", Email: "bob@acme.com"}, e.clock.Now().Add(time.Hour), nil)
	if err != nil {
		t.Fatal(err)
	}
	bob := &http.Cookie{Name: SessionCookie, Value: other}
	tampered := form.Get("csrf")
	tampered = tampered[:len(tampered)-2] + "AA"
	if strings.HasSuffix(form.Get("csrf"), "AA") {
		tampered = tampered[:len(tampered)-2] + "BB"
	}
	exp, _, _ := strings.Cut(form.Get("csrf"), ".")
	rejects := map[string]*http.Request{
		"no csrf":           postForm(with(form, "csrf", ""), sess),
		"tampered csrf":     postForm(with(form, "csrf", tampered), sess),
		"garbage csrf":      postForm(with(form, "csrf", "x.y"), sess),
		"longer expiry":     postForm(with(form, "csrf", "9"+form.Get("csrf")), sess),
		"other port":        postForm(with(form, "redirect_uri", "http://127.0.0.1:6666/callback?keep=1"), sess),
		"other state":       postForm(with(form, "state", "st-999"), sess),
		"display instead":   postForm(with(form, "mode", "display"), sess),
		"other user":        postForm(form, bob),
		"other session":     postForm(form, e.session()),
		"cross-site post":   withHeaders(postForm(form, sess), "Sec-Fetch-Site", "cross-site"),
		"same-site post":    withHeaders(postForm(form, sess), "Sec-Fetch-Site", "same-site"),
		"foreign origin":    withHeaders(postForm(form, sess), "Origin", "https://evil.example"),
		"site origin":       withHeaders(postForm(form, sess), "Origin", "https://foo.backyard.test"),
		"expiry only":       postForm(with(form, "csrf", exp+"."), sess),
		"oversized csrf":    postForm(with(form, "csrf", form.Get("csrf")+strings.Repeat("A", 200)), sess),
		"query, not a form": withHeaders(req("POST", target+"&csrf="+url.QueryEscape(form.Get("csrf")), sess), "Sec-Fetch-Site", "same-origin"),
	}
	for name, r := range rejects {
		res := serve(e.a.Handler(), r)
		want := http.StatusForbidden
		if name == "query, not a form" {
			want = http.StatusBadRequest // the parameters must come from the form
		}
		if res.StatusCode != want || res.Header.Get("Location") != "" {
			t.Errorf("%s: %d %q, want %d", name, res.StatusCode, res.Header.Get("Location"), want)
		}
	}

	// Confirmations expire.
	e.clock.Advance(cliConfirmTTL + time.Second)
	if res := serve(e.a.Handler(), postForm(form, sess)); res.StatusCode != http.StatusForbidden {
		t.Fatalf("expired confirmation = %d", res.StatusCode)
	}

	// Signed out meanwhile: back through the login to the GET form (which
	// asks again).
	res = serve(e.a.Handler(), postForm(form))
	loc, _ := url.Parse(res.Header.Get("Location"))
	if res.StatusCode != http.StatusFound || loc.Path != LoginPath {
		t.Fatalf("signed out = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if rd, _ := url.Parse(loc.Query().Get("rd")); rd.Path != CLIPath || rd.Query().Get("redirect_uri") != form.Get("redirect_uri") ||
		rd.Query().Get("state") != "st-123" || rd.Query().Has("csrf") {
		t.Fatalf("rd = %q", loc.Query().Get("rd"))
	}

	// An oversized form is refused.
	big := postForm(with(form, "state", strings.Repeat("s", maxCLIFormBytes)), sess)
	if res := serve(e.a.Handler(), big); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized form = %d", res.StatusCode)
	}
}

// TestCLIDisplayNeedsConfirmation: mode=display follows the same rule.
func TestCLIDisplayNeedsConfirmation(t *testing.T) {
	e := newOIDCEnv(t, nil)
	sess := e.session()
	target := cliURL("", "", "mode", "display")
	res := serve(e.a.Handler(), withHeaders(req("GET", target, sess), "Sec-Fetch-Site", "cross-site"))
	if csp := res.Header.Get("Content-Security-Policy"); !strings.HasSuffix(csp, "form-action 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	form := confirmationForm(t, res)
	if form.Get("mode") != "display" || form.Has("redirect_uri") || form.Has("state") {
		t.Fatalf("form = %v", form)
	}
	ok := serve(e.a.Handler(), postForm(form, sess))
	b := body(t, ok)
	if ok.StatusCode != http.StatusOK || !strings.Contains(b, "Your Backyard CLI token") || !strings.Contains(b, "--token ey") {
		t.Fatalf("display confirm = %d\n%s", ok.StatusCode, b)
	}
	// The display confirmation does not authorize a loopback redirect.
	redirect := with(with(form, "mode", ""), "redirect_uri", "http://127.0.0.1:5555/cb")
	if res := serve(e.a.Handler(), postForm(with(redirect, "state", "s"), sess)); res.StatusCode != http.StatusForbidden {
		t.Fatalf("display csrf for redirect = %d", res.StatusCode)
	}
	// Same-origin (a link on the hub) shows the token directly.
	res = serve(e.a.Handler(), withHeaders(req("GET", target, sess), "Sec-Fetch-Site", "same-origin"))
	if b := body(t, res); res.StatusCode != http.StatusOK || !strings.Contains(b, "Your Backyard CLI token") {
		t.Fatalf("same-origin display = %d\n%s", res.StatusCode, b)
	}
}

// TestCLIConfirmationProxyAndNoneModes: without a Backyard session the
// confirmation is bound to the identity the proxy (or guest cookie) asserts.
func TestCLIConfirmationProxyAndNoneModes(t *testing.T) {
	cfg := baseConfig("header")
	cfg.Server.TrustedProxies = []string{"10.0.0.0/8"}
	cfg.Auth.Header.EmailHeader = "X-Forwarded-Email"
	a := newAuth(t, cfg, Options{})
	as := func(r *http.Request, email string) *http.Request {
		r.RemoteAddr = "10.1.2.3:5000"
		r.Header.Set("X-Forwarded-Email", email)
		return r
	}
	target := cliURL("http://[::1]:5555/cb", "s")
	res := serve(a.Handler(), as(withHeaders(req("GET", target), "Sec-Fetch-Site", "cross-site"), "erin@acme.com"))
	if csp := res.Header.Get("Content-Security-Policy"); !strings.HasSuffix(csp, "form-action 'self' http:") {
		t.Fatalf("IPv6 CSP = %q", csp)
	}
	form := confirmationForm(t, res)
	if res := serve(a.Handler(), as(postForm(form), "mallory@acme.com")); res.StatusCode != http.StatusForbidden {
		t.Fatalf("other proxy identity = %d", res.StatusCode)
	}
	res = serve(a.Handler(), as(postForm(form), "erin@acme.com"))
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "http://[::1]:5555/cb?") {
		t.Fatalf("header confirm = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	// Unauthenticated POST in a proxy mode: 401, nothing minted.
	if res := serve(a.Handler(), postForm(form)); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated header confirm = %d", res.StatusCode)
	}

	// Mode none: the guest cookie identifies the browser.
	n := newAuth(t, baseConfig("none"), Options{})
	res = serve(n.Handler(), withHeaders(req("GET", cliURL("http://localhost:4000/cb", "xyz")), "Sec-Fetch-Site", "cross-site"))
	anon := cookieNamed(res, AnonCookie)
	if anon == nil {
		t.Fatal("no guest cookie")
	}
	form = confirmationForm(t, res)
	if res := serve(n.Handler(), postForm(form)); res.StatusCode != http.StatusForbidden {
		t.Fatalf("other guest = %d", res.StatusCode)
	}
	res = serve(n.Handler(), postForm(form, anon))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("guest confirm = %d", res.StatusCode)
	}
	dest, _ := url.Parse(res.Header.Get("Location"))
	if u, err := NewTokens(testSecret, 0).Verify(dest.Query().Get("token")); err != nil || !u.Anonymous {
		t.Fatalf("guest token = %+v, %v", u, err)
	}
}
