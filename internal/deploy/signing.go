package deploy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/keys"
)

// DeployTTL is how long a deploy id (and its upload URLs) stays valid.
const DeployTTL = time.Hour

// maxSignedLen bounds signed values accepted from clients.
const maxSignedLen = 8 << 10

// deployClaims is the payload of a deploy id.
type deployClaims struct {
	Site  string `json:"site"`
	Actor string `json:"actor"`
	Exp   int64  `json:"exp"`
	Nonce string `json:"nonce"`
}

// uploadClaims is the payload of a self-hosted upload token.
type uploadClaims struct {
	Site        string `json:"site"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
	DeployID    string `json:"deploy_id"` // the deploy's nonce (short id)
	Exp         int64  `json:"exp"`
}

var (
	errBadToken     = errors.New("deploy: invalid token")
	errExpiredToken = errors.New("deploy: token expired")
)

// newNonce returns a random 128-bit hex id.
func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// signClaims marshals and signs v.
func signClaims(key []byte, v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return keys.Sign(key, b), nil
}

// verifyClaims checks the signature and decodes the payload into v.
func verifyClaims(key []byte, signed string, v any) error {
	if signed == "" || len(signed) > maxSignedLen || strings.ContainsAny(signed, " /") {
		return errBadToken
	}
	b, err := keys.Verify(key, signed)
	if err != nil {
		return errBadToken
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errBadToken
	}
	return nil
}

// shortDeployID is the public, non-secret identifier of a deploy (stored in
// SiteMeta.DeployID and in upload tokens).
func shortDeployID(nonce string) string { return "d_" + nonce }
