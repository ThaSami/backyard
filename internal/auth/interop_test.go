package auth

import (
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/thasami/backyard/internal/keys"
)

// Cross-edition vectors (SPEC §16): the Cloudflare Worker
// (deploy/cloudflare/test/auth.test.ts) pins these exact values, produced by
// this implementation. A change here breaks interoperability between the Go
// server and the Worker: tokens minted by one would be rejected by the other.
const (
	interopSecret     = "test-secret-0123456789-abcdefghijklmnopqrstuvwxyz"
	interopAPIKey     = "e58c2bef702141f096282bc961cd322f05301e296df59ea5b9065346f1a43902"
	interopAnonKey    = "a06f2f691de8c0b6caed952a49e36545c7b8f007c233b329a63dbfff3799e139"
	interopAnonSigned = "MDEyMzQ1Njc4OWFiY2RlZg.mkgCtfBExPrR9N7htxNoiA5vMDAzHdgktsV6snsAf0w"
	interopToken      = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhbGljZUBhY21lLmNvbSIsImVtYWlsIjoiYWxpY2VAYWNtZS5jb20iLCJuYW1lIjoiQWxpY2UgU21pdGgiLCJncm91cHMiOlsiZW5nIiwiYmFja3lhcmQtYWRtaW5zIl0sInNjb3BlIjoiYXBpIiwiaXNzIjoiYmFja3lhcmQiLCJqdGkiOiI0NzMwNzhmZDJlMjEwMGM3OTkxYTliOTI5MWEwMGRmOSIsImlhdCI6MTc5MTg0MjkzMCwiZXhwIjo0MzE0NzIyOTMwfQ.4He0vGvXox6fZtuJwHnSAUrY6x6sNOxvMq1X10fxRug"
)

func TestCloudflareInteropVectors(t *testing.T) {
	if got := hex.EncodeToString(keys.Derive(interopSecret, keys.APIToken)); got != interopAPIKey {
		t.Fatalf("api token key = %s", got)
	}
	if got := hex.EncodeToString(keys.Derive(interopSecret, keys.Anon)); got != interopAnonKey {
		t.Fatalf("anon key = %s", got)
	}

	// The token minted by the Go server verifies, with the documented claims.
	tk := NewTokens(interopSecret, 0)
	tk.now = func() time.Time { return time.Unix(1791842930, 0) }
	u, err := tk.Verify(interopToken)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if u.ID != "alice@acme.com" || u.Email != "alice@acme.com" || u.Name != "Alice Smith" ||
		strings.Join(u.Groups, ",") != "eng,backyard-admins" || u.Admin || u.Anonymous {
		t.Fatalf("user = %+v", u)
	}

	// The claim layout (and order) the Worker expects of Go-minted tokens.
	tok, _, err := tk.Issue(u, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := decodeSegment(strings.Split(tok, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	last := -1
	for _, k := range []string{"sub", "email", "name", "groups", "scope", "iss", "jti", "iat", "exp"} {
		i := strings.Index(string(payload), `"`+k+`":`)
		if i <= last {
			t.Fatalf("claim %q missing or out of order in %s", k, payload)
		}
		last = i
	}
	if h, _ := decodeSegment(strings.Split(tok, ".")[0]); string(h) != `{"alg":"HS256","typ":"JWT"}` {
		t.Fatalf("header = %s", h)
	}

	// The anonymous cookie minted by the Worker resolves to the same guest here.
	cfg := baseConfig("none")
	cfg.Server.Secret = interopSecret
	a := newAuth(t, cfg, Options{})
	r := req("GET", "https://backyard.test/")
	r.AddCookie(&http.Cookie{Name: AnonCookie, Value: interopAnonSigned})
	rec := httptest.NewRecorder()
	g, err := a.Authenticate(rec, r)
	if err != nil || g.ID != "anon:0123456789abcdef" || g.Name != "Guest 0123" || !g.Anonymous {
		t.Fatalf("guest = %+v, %v", g, err)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatal("a valid anonymous cookie was replaced")
	}
}
