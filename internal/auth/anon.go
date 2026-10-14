package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
// same guest. Valid cookies of different guests fail closed, as session
// cookies do (see sessionFromCookies): a site's page can plant its author's
// guest cookie in visitors' browsers.
func (a *authenticator) authenticateAnon(w http.ResponseWriter, r *http.Request) (*identity.User, error) {
	guest := ""
	for _, c := range r.Cookies() {
		if c.Name != AnonCookie {
			continue
		}
		id, ok := a.anon.parse(c.Value)
		switch {
		case !ok:
		case guest == "":
			guest = id
		case id != guest:
			a.log.Warn("auth: request carries guest cookies of different guests; possibly planted by a site's page",
				"host", r.Host, "path", truncate(r.URL.Path, 128), "audit", true)
			a.expireForeignCookies(w, r, AnonCookie)
			return nil, fmt.Errorf("%w: %w", ErrUnauthenticated, ErrCookieConflict)
		}
	}
	if guest != "" {
		return a.finalize(r.Context(), anonUser(guest), srcAnon)
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	if w != nil {
		c := a.newCookie(AnonCookie, keys.Sign(a.anon.key, []byte(id)), a.cookieDomain, "/", anonCookieTTL)
		http.SetCookie(w, c)
		r.AddCookie(c) // a request Cookie header carries only name=value
	}
	return a.finalize(r.Context(), anonUser(id), srcAnon)
}
