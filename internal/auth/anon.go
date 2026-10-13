package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/thasami/backyard/internal/identity"
	"github.com/thasami/backyard/internal/keys"
)

// anonCookieTTL is the lifetime of the backyard_anon cookie.
const anonCookieTTL = 365 * 24 * time.Hour

// anonCodec signs anonymous guest ids (auth.mode=none).
type anonCodec struct {
	key []byte
}

func newAnonCodec(secret string) *anonCodec {
	return &anonCodec{key: keys.Derive(secret, keys.Anon)}
}

// parse returns the 16-hex-digit id of a signed cookie value.
func (c *anonCodec) parse(v string) (string, bool) {
	b, err := keys.Verify(c.key, v)
	if err != nil || len(b) != 16 {
		return "", false
	}
	for _, ch := range b {
		if !('0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f') {
			return "", false
		}
	}
	return string(b), true
}

// anonUser returns the guest identity for a 16-hex id.
func anonUser(id string) *identity.User {
	return &identity.User{ID: anonPrefix + id, Name: "Guest " + id[:4], Anonymous: true}
}

// authenticateAnon returns the anonymous guest identified by the
// backyard_anon cookie, minting a new id (and setting the cookie when w is
// non-nil) when the cookie is missing or invalid. The new cookie is also
// added to r so later Authenticate calls for the same request resolve the
// same guest.
func (a *authenticator) authenticateAnon(w http.ResponseWriter, r *http.Request) (*identity.User, error) {
	for _, c := range r.Cookies() {
		if c.Name != AnonCookie {
			continue
		}
		if id, ok := a.anon.parse(c.Value); ok {
			return a.finalize(r.Context(), anonUser(id), srcAnon)
		}
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	if w != nil {
		c := a.newCookie(AnonCookie, keys.Sign(a.anon.key, []byte(id)), a.cookieDomain, "/", anonCookieTTL)
		http.SetCookie(w, c)
		r.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	return a.finalize(r.Context(), anonUser(id), srcAnon)
}
