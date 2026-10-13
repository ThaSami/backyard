package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

const (
	// jwksMinRefresh is the minimum time between two JWKS downloads.
	jwksMinRefresh = 15 * time.Second
	// jwksMaxAge forces a refresh of an old key set (picks up revocations).
	jwksMaxAge = time.Hour
	// jwksFetchTimeout bounds one JWKS download.
	jwksFetchTimeout = 10 * time.Second
	// maxJWKSBytes bounds a JWKS document.
	maxJWKSBytes = 1 << 20
	// minRSABits is the smallest accepted RSA modulus.
	minRSABits = 2048
)

// jwksKeySet is an oidc.KeySet backed by a JWKS URL.
//
// Unlike oidc.RemoteKeySet, which re-downloads the key set after every failed
// signature check, it only refreshes when a token names an unknown key id (or
// the set is older than jwksMaxAge), and never more often than every
// jwksMinRefresh. Forged tokens therefore cannot drive unbounded outbound
// traffic to the key server. Signature verification itself is delegated to
// go-oidc (oidc.StaticKeySet); algorithm checks happen in the
// oidc.IDTokenVerifier before the key set is consulted.
type jwksKeySet struct {
	url    string
	client *http.Client
	now    func() time.Time
	log    *slog.Logger

	refreshMu  sync.Mutex  // serializes downloads
	refreshing atomic.Bool // a background refresh is running
	bg         sync.WaitGroup

	mu          sync.RWMutex
	keys        []jwksKey
	fetchedAt   time.Time
	lastAttempt time.Time
	lastErr     error // error of the most recent download (nil after a success)
	fetches     int
}

type jwksKey struct {
	kid string
	key crypto.PublicKey
}

var _ oidc.KeySet = (*jwksKeySet)(nil)

// errKeysUnavailable marks verification failures caused by an unreachable or
// broken key server (an infrastructure error, not bad credentials).
var errKeysUnavailable = errors.New("auth: signing keys unavailable")

func newJWKSKeySet(rawURL string, client *http.Client, now func() time.Time, log *slog.Logger) *jwksKeySet {
	return &jwksKeySet{url: rawURL, client: client, now: now, log: log}
}

// redactedURL is the key set URL with any password removed (for logs).
func (s *jwksKeySet) redactedURL() string {
	if u, err := url.Parse(s.url); err == nil {
		return u.Redacted()
	}
	return "(invalid url)"
}

// VerifySignature implements oidc.KeySet.
//
// An unknown kid waits for a (throttled) download. A known kid in a set older
// than jwksMaxAge is verified with the cached keys while the set is refreshed
// in the background, so a slow key server never stalls requests that already
// have their key.
func (s *jwksKeySet) VerifySignature(ctx context.Context, raw string) ([]byte, error) {
	kid := jwtKeyID(raw)
	keys, fresh := s.candidates(kid)
	switch {
	case len(keys) == 0:
		s.refresh(ctx)
		keys, _ = s.candidates(kid)
	case !fresh:
		s.refreshInBackground(ctx)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no signing key matches kid %q", truncate(kid, 64))
	}
	return (&oidc.StaticKeySet{PublicKeys: keys}).VerifySignature(ctx, raw)
}

// candidates returns the keys for kid (all keys when kid is empty) and
// whether the cached set is younger than jwksMaxAge.
func (s *jwksKeySet) candidates(kid string) ([]crypto.PublicKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []crypto.PublicKey
	for _, k := range s.keys {
		if kid == "" || k.kid == kid {
			out = append(out, k.key)
		}
	}
	return out, !s.fetchedAt.IsZero() && s.now().Sub(s.fetchedAt) < jwksMaxAge
}

// refreshInBackground starts a refresh unless one is already running.
func (s *jwksKeySet) refreshInBackground(ctx context.Context) {
	if !s.refreshing.CompareAndSwap(false, true) {
		return
	}
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.refreshing.Store(false)
		s.refresh(context.WithoutCancel(ctx))
	}()
}

// refresh downloads the key set unless that happened less than
// jwksMinRefresh ago. Failures keep the previous keys.
func (s *jwksKeySet) refresh(ctx context.Context) {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	s.mu.Lock()
	if !s.lastAttempt.IsZero() && s.now().Sub(s.lastAttempt) < jwksMinRefresh {
		s.mu.Unlock()
		return
	}
	s.lastAttempt = s.now()
	s.fetches++
	s.mu.Unlock()

	keys, err := s.fetch(ctx)
	if err != nil {
		s.log.Warn("auth: JWKS download failed; keeping previous keys", "url", s.redactedURL(), "error", err)
		s.mu.Lock()
		s.lastErr = err
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.keys = keys
	s.fetchedAt = s.now()
	s.lastErr = nil
	s.mu.Unlock()
}

// unavailable reports why the key for raw's kid could not be obtained when
// that is an infrastructure problem: no cached key matches and the most
// recent download failed. It returns nil when keys are available (the token
// is then simply invalid).
func (s *jwksKeySet) unavailable(raw string) error {
	keys, _ := s.candidates(jwtKeyID(raw))
	s.mu.RLock()
	err := s.lastErr
	s.mu.RUnlock()
	if len(keys) == 0 && err != nil {
		return fmt.Errorf("%w: %s: %w", errKeysUnavailable, s.redactedURL(), err)
	}
	return nil
}

// fetch downloads and parses the JWKS document.
func (s *jwksKeySet) fetch(ctx context.Context) ([]jwksKey, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("key server returned %s", resp.Status)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("key set contains no usable signing keys")
	}
	return keys, nil
}

// jwk is the subset of RFC 7517/7518 members needed for signature keys.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parseJWKS parses RSA, EC (P-256/384/521) and Ed25519 signature keys;
// encryption keys and unsupported or invalid keys are skipped.
func parseJWKS(b []byte) ([]jwksKey, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("decode key set: %w", err)
	}
	var out []jwksKey
	for _, raw := range doc.Keys {
		var k jwk
		if json.Unmarshal(raw, &k) != nil || strings.EqualFold(k.Use, "enc") {
			continue
		}
		pub, err := k.publicKey()
		if err != nil {
			continue
		}
		out = append(out, jwksKey{kid: k.Kid, key: pub})
	}
	return out, nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nb, err := decodeSegment(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := decodeSegment(k.E)
		if err != nil || len(eb) == 0 || len(eb) > 4 {
			return nil, errors.New("bad exponent")
		}
		n := new(big.Int).SetBytes(nb)
		e := int(new(big.Int).SetBytes(eb).Int64())
		if n.BitLen() < minRSABits || e < 3 || e%2 == 0 {
			return nil, errors.New("weak or invalid RSA key")
		}
		return &rsa.PublicKey{N: n, E: e}, nil
	case "EC":
		var curve elliptic.Curve
		var size int
		switch k.Crv {
		case "P-256":
			curve, size = elliptic.P256(), 32
		case "P-384":
			curve, size = elliptic.P384(), 48
		case "P-521":
			curve, size = elliptic.P521(), 66
		default:
			return nil, errors.New("unsupported curve")
		}
		x, err1 := decodeSegment(k.X)
		y, err2 := decodeSegment(k.Y)
		if err1 != nil || err2 != nil || len(x) != size || len(y) != size {
			return nil, errors.New("bad EC coordinates")
		}
		return ecdsa.ParseUncompressedPublicKey(curve, append(append([]byte{4}, x...), y...))
	case "OKP":
		x, err := decodeSegment(k.X)
		if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("unsupported OKP key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, errors.New("unsupported key type")
}

// jwtKeyID returns the unverified "kid" header of a compact JWS, or "".
func jwtKeyID(raw string) string {
	h, _, ok := strings.Cut(raw, ".")
	if !ok || len(h) > 4096 {
		return ""
	}
	b, err := decodeSegment(h)
	if err != nil {
		return ""
	}
	var hdr struct {
		Kid string `json:"kid"`
	}
	if json.Unmarshal(b, &hdr) != nil {
		return ""
	}
	return hdr.Kid
}
