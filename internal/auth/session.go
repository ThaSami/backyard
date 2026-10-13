package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

const (
	// maxSessionCookieBytes keeps the session cookie value well below the
	// common 4 KiB per-cookie browser limit.
	maxSessionCookieBytes = 3800
	// sessionAAD binds sealed values to their purpose.
	sessionAAD = "backyard_session"
	// oauthStateTTL is the lifetime of the login state cookie.
	oauthStateTTL = 10 * time.Minute
)

// sessionCodec seals and opens browser sessions: base64url(nonce ||
// AES-256-GCM(JSON{user, exp})) keyed with keys.Session.
type sessionCodec struct {
	aead cipher.AEAD
	now  func() time.Time
}

// sessionUser is the compact profile stored in the session cookie. Admin is
// not stored: it is recomputed from the configuration on every request.
type sessionUser struct {
	ID        string         `json:"id"`
	Email     string         `json:"email,omitempty"`
	Name      string         `json:"name,omitempty"`
	Title     string         `json:"title,omitempty"`
	Team      string         `json:"team,omitempty"`
	Slack     string         `json:"slack,omitempty"`
	AvatarURL string         `json:"avatar_url,omitempty"`
	Groups    []string       `json:"groups,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"`
}

type sessionPayload struct {
	User sessionUser `json:"user"`
	Exp  int64       `json:"exp"`
}

func newSessionCodec(secret string, now func() time.Time) (*sessionCodec, error) {
	block, err := aes.NewCipher(keys.Derive(secret, keys.Session))
	if err != nil {
		return nil, fmt.Errorf("auth: session cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("auth: session cipher: %w", err)
	}
	return &sessionCodec{aead: aead, now: now}, nil
}

// maxPlaintext is the largest JSON payload whose sealed, encoded form fits
// in maxSessionCookieBytes.
func (c *sessionCodec) maxPlaintext() int {
	return base64.RawURLEncoding.DecodedLen(maxSessionCookieBytes) - c.aead.NonceSize() - c.aead.Overhead()
}

// seal encrypts u with expiry exp. When the profile does not fit in a cookie
// its groups are truncated (groups for which important returns true are kept
// first), then Extra, the avatar URL and the remaining optional fields are
// dropped.
func (c *sessionCodec) seal(u *identity.User, exp time.Time, important func(string) bool) (string, error) {
	if u == nil || u.ID == "" {
		return "", errors.New("auth: cannot seal a session without a user id")
	}
	su := sessionUser{
		ID: u.ID, Email: u.Email, Name: u.Name, Title: u.Title, Team: u.Team, Slack: u.Slack,
		AvatarURL: u.AvatarURL, Groups: prioritize(u.Groups, important), Extra: u.Extra,
	}
	p := sessionPayload{User: su, Exp: exp.Unix()}
	limit := c.maxPlaintext()
	enc := func(p sessionPayload) []byte {
		b, err := json.Marshal(p)
		if err != nil {
			return nil
		}
		return b
	}
	plain := enc(p)
	if plain == nil {
		return "", errors.New("auth: session encode failed")
	}
	if len(plain) > limit {
		// Largest number of groups that fits (binary search).
		all := p.User.Groups
		lo, hi := 0, len(all)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			p.User.Groups = all[:mid]
			if b := enc(p); b != nil && len(b) <= limit {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		p.User.Groups = all[:lo]
		drops := []func(){
			func() {},
			func() { p.User.Extra = nil },
			func() { p.User.AvatarURL = "" },
			func() { p.User.Title, p.User.Team, p.User.Slack = "", "", "" },
		}
		plain = nil
		for _, drop := range drops {
			drop()
			if b := enc(p); b != nil && len(b) <= limit {
				plain = b
				break
			}
		}
		if plain == nil {
			return "", errors.New("auth: user profile too large for a session cookie")
		}
	}
	nonce := make([]byte, c.aead.NonceSize(), c.aead.NonceSize()+len(plain)+c.aead.Overhead())
	_, _ = rand.Read(nonce)
	sealed := c.aead.Seal(nonce, nonce, plain, []byte(sessionAAD))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

// open decrypts and validates a sealed session.
func (c *sessionCodec) open(v string) (*identity.User, error) {
	if v == "" || len(v) > 2*maxSessionCookieBytes {
		return nil, errors.New("malformed session")
	}
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil || len(raw) < c.aead.NonceSize()+c.aead.Overhead() {
		return nil, errors.New("malformed session")
	}
	ns := c.aead.NonceSize()
	plain, err := c.aead.Open(nil, raw[:ns], raw[ns:], []byte(sessionAAD))
	if err != nil {
		return nil, errors.New("session authentication failed")
	}
	var p sessionPayload
	if err := json.Unmarshal(plain, &p); err != nil {
		return nil, errors.New("malformed session payload")
	}
	if p.Exp <= c.now().Unix() {
		return nil, errors.New("session expired")
	}
	if strings.TrimSpace(p.User.ID) == "" {
		return nil, errors.New("session without user")
	}
	su := p.User
	return &identity.User{
		ID: su.ID, Email: su.Email, Name: su.Name, Title: su.Title, Team: su.Team, Slack: su.Slack,
		AvatarURL: su.AvatarURL, Groups: su.Groups, Extra: su.Extra,
	}, nil
}

// prioritize returns groups with the important ones first (each part keeps
// its original order).
func prioritize(groups []string, important func(string) bool) []string {
	if len(groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(groups))
	var rest []string
	for _, g := range groups {
		if important != nil && important(g) {
			out = append(out, g)
		} else {
			rest = append(rest, g)
		}
	}
	return append(out, rest...)
}

// isImportantGroup reports whether g is referenced by admin_groups or
// allowed_groups (those must survive session truncation).
func (a *authenticator) isImportantGroup(g string) bool {
	return a.important[strings.ToLower(strings.TrimSpace(g))]
}

// authenticateSession reads the backyard_session cookie (oidc mode). Every
// cookie with that name is tried, so a stale host-only cookie does not shadow
// a valid domain cookie.
func (a *authenticator) authenticateSession(r *http.Request) (*identity.User, error) {
	found := false
	for _, c := range r.Cookies() {
		if c.Name != SessionCookie {
			continue
		}
		found = true
		u, err := a.sess.open(c.Value)
		if err != nil {
			a.log.Debug("auth: session cookie rejected", "error", err)
			continue
		}
		return a.finalize(r.Context(), u, srcIdP)
	}
	if found {
		return nil, fmt.Errorf("%w: invalid or expired session", ErrUnauthenticated)
	}
	return nil, fmt.Errorf("%w: no session", ErrUnauthenticated)
}

// sessionCookie builds the session cookie for value.
func (a *authenticator) sessionCookie(value string, ttl time.Duration) *http.Cookie {
	return a.newCookie(SessionCookie, value, a.cookieDomain, "/", ttl)
}

// clearSessionCookies expires the session cookie (domain and host-only
// variants).
func (a *authenticator) clearSessionCookies(w http.ResponseWriter) {
	http.SetCookie(w, a.newCookie(SessionCookie, "", a.cookieDomain, "/", 0))
	if a.cookieDomain != "" {
		http.SetCookie(w, a.newCookie(SessionCookie, "", "", "/", 0))
	}
}

// oauthState is the content of the signed backyard_oauth cookie.
type oauthState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	RD       string `json:"rd"`
	Exp      int64  `json:"exp"`
}

// encodeState signs st with the OAuth state key.
func encodeState(key []byte, st oauthState) (string, error) {
	b, err := json.Marshal(st)
	if err != nil {
		return "", fmt.Errorf("auth: encode login state: %w", err)
	}
	return keys.Sign(key, b), nil
}

// decodeState verifies a signed login state and its expiry.
func decodeState(key []byte, v string, now time.Time) (*oauthState, error) {
	b, err := keys.Verify(key, v)
	if err != nil {
		return nil, err
	}
	var st oauthState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("malformed login state: %w", err)
	}
	if st.Exp <= now.Unix() {
		return nil, errors.New("login state expired")
	}
	if st.State == "" || st.Verifier == "" || st.Nonce == "" {
		return nil, errors.New("incomplete login state")
	}
	return &st, nil
}
