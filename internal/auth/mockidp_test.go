package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// mockIdP is an in-process OpenID Connect provider: discovery, JWKS,
// authorization endpoint (immediately redirects back with a code, as if the
// user had signed in), token endpoint with PKCE validation issuing RS256 ID
// tokens, userinfo and end_session.
type mockIdP struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	clientID string
	secret   string
	clock    *fakeClock

	mu            sync.Mutex
	codes         map[string]*authCode
	access        map[string]map[string]any // access token → userinfo claims
	user          map[string]any            // claims of the next signed-in user
	userinfo      map[string]any            // extra claims only returned by userinfo
	userinfoSub   string                    // overrides userinfo sub when set
	wrongNonce    bool
	noEndSession  bool
	noUserInfo    bool
	tokenRequests int
	authorizeReqs []url.Values
}

type authCode struct {
	clientID, redirectURI, nonce, challenge string
	claims                                  map[string]any
}

func newMockIdP(t *testing.T, clock *fakeClock) *mockIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockIdP{
		t: t, key: key, kid: "idp-key-1", clientID: "backyard-web", secret: "web-secret",
		clock: clock, codes: map[string]*authCode{}, access: map[string]map[string]any{},
		user: map[string]any{"sub": "00u-alice", "email": "Alice@Acme.com", "name": "Alice Smith",
			"groups": []any{"eng", "backyard-admins"}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", m.discovery)
	mux.HandleFunc("GET /jwks", m.jwks)
	mux.HandleFunc("GET /authorize", m.authorize)
	mux.HandleFunc("POST /token", m.token)
	mux.HandleFunc("GET /userinfo", m.userInfo)
	mux.HandleFunc("GET /logout", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockIdP) issuer() string { return m.srv.URL }

func (m *mockIdP) setUser(claims map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.user = claims
}

func (m *mockIdP) discovery(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	noEnd := m.noEndSession
	m.mu.Unlock()
	doc := map[string]any{
		"issuer":                                m.issuer(),
		"authorization_endpoint":                m.issuer() + "/authorize",
		"token_endpoint":                        m.issuer() + "/token",
		"userinfo_endpoint":                     m.issuer() + "/userinfo",
		"jwks_uri":                              m.issuer() + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	}
	if !noEnd {
		doc["end_session_endpoint"] = m.issuer() + "/logout"
	}
	m.mu.Lock()
	if m.noUserInfo {
		delete(doc, "userinfo_endpoint")
	}
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, doc)
}

func (m *mockIdP) jwks(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": []any{rsaJWK(&m.key.PublicKey, m.kid)}})
}

func (m *mockIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m.mu.Lock()
	m.authorizeReqs = append(m.authorizeReqs, q)
	m.mu.Unlock()
	switch {
	case q.Get("response_type") != "code",
		q.Get("client_id") != m.clientID,
		!strings.Contains(" "+q.Get("scope")+" ", " openid "),
		q.Get("state") == "", q.Get("nonce") == "",
		q.Get("code_challenge") == "", q.Get("code_challenge_method") != "S256":
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	code := randomString(16)
	m.mu.Lock()
	claims := map[string]any{}
	for k, v := range m.user {
		claims[k] = v
	}
	m.codes[code] = &authCode{
		clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"),
		nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), claims: claims,
	}
	m.mu.Unlock()
	u, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (m *mockIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	id, sec, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		sec, _ = url.QueryUnescape(sec)
	} else {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != m.clientID || sec != m.secret {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_client"})
		return
	}
	m.mu.Lock()
	m.tokenRequests++
	code := m.codes[r.PostForm.Get("code")]
	delete(m.codes, r.PostForm.Get("code"))
	wrongNonce := m.wrongNonce
	m.mu.Unlock()
	if r.PostForm.Get("grant_type") != "authorization_code" || code == nil ||
		code.redirectURI != r.PostForm.Get("redirect_uri") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != code.challenge {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE verification failed"})
		return
	}
	access := randomString(24)
	atHash := sha256.Sum256([]byte(access))
	now := m.clock.Now()
	claims := map[string]any{
		"iss": m.issuer(), "aud": m.clientID, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
		"nonce": code.nonce, "at_hash": base64.RawURLEncoding.EncodeToString(atHash[:16]),
	}
	if wrongNonce {
		claims["nonce"] = "not-the-nonce"
	}
	for k, v := range code.claims {
		claims[k] = v
	}
	m.mu.Lock()
	ui := map[string]any{"sub": code.claims["sub"]}
	for k, v := range m.userinfo {
		ui[k] = v
	}
	if m.userinfoSub != "" {
		ui["sub"] = m.userinfoSub
	}
	m.access[access] = ui
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 600,
		"id_token": m.sign(claims),
	})
}

func (m *mockIdP) userInfo(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	m.mu.Lock()
	claims, ok := m.access[tok]
	m.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_token"})
		return
	}
	writeJSON(w, http.StatusOK, claims)
}

// sign signs claims with the IdP's key.
func (m *mockIdP) sign(claims map[string]any) string {
	return signJWT(m.t, jwt.SigningMethodRS256, m.key, m.kid, claims)
}

// accessToken mints a device-flow style JWT from the IdP.
func (m *mockIdP) accessToken(extra map[string]any) string {
	now := m.clock.Now()
	claims := map[string]any{
		"iss": m.issuer(), "sub": "00u-carol", "email": "carol@acme.com", "name": "Carol",
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		if v == nil {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}
	return m.sign(claims)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
