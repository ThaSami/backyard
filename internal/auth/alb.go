package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/thasami/backyard/internal/identity"
)

const (
	// ALBHeader carries the ALB-signed user claims.
	ALBHeader = "X-Amzn-Oidc-Data"
	// albKeyFetchTimeout bounds one public key download.
	albKeyFetchTimeout = 10 * time.Second
	// albNegativeTTL suppresses re-fetching a key id that failed recently.
	albNegativeTTL = time.Minute
	// albMaxKeys bounds the key cache.
	albMaxKeys = 256
	// maxALBTokenBytes bounds the accepted header size.
	maxALBTokenBytes = 16 << 10
)

var albKidRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// albVerifier verifies AWS ALB OIDC user claims tokens (SPEC §8.2).
type albVerifier struct {
	keyURL string
	signer string
	client *http.Client
	now    func() time.Time
	log    *slog.Logger

	fetchMu sync.Mutex // serializes key downloads

	mu       sync.RWMutex
	keys     map[string]*ecdsa.PublicKey
	failures map[string]albFailure
	fetches  int // number of key downloads (tests)
}

// albFailure is a negatively cached key download failure.
type albFailure struct {
	at  time.Time
	err error
}

func (a *authenticator) newALBVerifier() (*albVerifier, error) {
	c := a.cfg.Auth.ALB
	keyURL := strings.TrimRight(strings.TrimSpace(c.KeyURL), "/")
	if keyURL == "" && strings.TrimSpace(c.Region) != "" {
		keyURL = "https://public-keys.auth.elb." + strings.TrimSpace(c.Region) + ".amazonaws.com"
	}
	if keyURL == "" {
		return nil, errors.New("auth: auth.alb.region (or key_url) is required for auth.mode=alb")
	}
	if u, err := url.Parse(keyURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("auth: auth.alb.key_url must be an absolute URL, got %q", keyURL)
	}
	if strings.TrimSpace(c.SignerARN) == "" {
		a.log.Warn("auth: auth.alb.signer_arn is not set; any ALB in the region can mint identities for this server")
	}
	return &albVerifier{
		keyURL:   keyURL,
		signer:   strings.TrimSpace(c.SignerARN),
		client:   a.client,
		now:      a.now,
		log:      a.log,
		keys:     make(map[string]*ecdsa.PublicKey),
		failures: make(map[string]albFailure),
	}, nil
}

type albHeaderFields struct {
	Alg    string `json:"alg"`
	Kid    string `json:"kid"`
	Signer string `json:"signer"`
	Exp    *int64 `json:"exp"`
}

// verify validates an X-Amzn-Oidc-Data token and returns its claims.
func (v *albVerifier) verify(ctx context.Context, raw string) (map[string]any, error) {
	if len(raw) > maxALBTokenBytes {
		return nil, errors.New("token too large")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("malformed token")
	}
	hb, err := decodeSegment(parts[0])
	if err != nil {
		return nil, errors.New("malformed token header")
	}
	var hdr albHeaderFields
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, errors.New("malformed token header")
	}
	if hdr.Alg != "ES256" {
		return nil, fmt.Errorf("unexpected algorithm %q", hdr.Alg)
	}
	if !albKidRe.MatchString(hdr.Kid) {
		return nil, errors.New("missing or malformed kid")
	}
	if v.signer != "" && hdr.Signer != v.signer {
		return nil, errors.New("token signed by an unexpected load balancer")
	}
	sig, err := decodeSegment(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("malformed signature")
	}
	pub, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	// The signature covers the raw (possibly padded) header.payload bytes.
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	rr, ss := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], rr, ss) {
		return nil, errors.New("invalid signature")
	}
	pb, err := decodeSegment(parts[1])
	if err != nil {
		return nil, errors.New("malformed payload")
	}
	claims, err := decodeClaims(pb)
	if err != nil {
		return nil, errors.New("malformed payload")
	}
	var exp int64
	switch e := claims["exp"].(type) {
	case float64:
		exp = int64(e)
	default:
		if hdr.Exp != nil {
			exp = *hdr.Exp
		}
	}
	if exp == 0 {
		return nil, errors.New("token without exp")
	}
	if v.now().Add(-clockSkew).Unix() >= exp {
		return nil, errors.New("token expired")
	}
	return claims, nil
}

// key returns the public key for kid, downloading it on first use.
func (v *albVerifier) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	v.mu.RLock()
	k := v.keys[kid]
	v.mu.RUnlock()
	if k != nil {
		return k, nil
	}
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	v.mu.RLock()
	k, failed := v.keys[kid], v.failures[kid]
	v.mu.RUnlock()
	if k != nil {
		return k, nil
	}
	if !failed.at.IsZero() && v.now().Sub(failed.at) < albNegativeTTL {
		return nil, failed.err
	}
	k, err := v.fetch(ctx, kid)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.fetches++
	if err != nil {
		if len(v.failures) >= albMaxKeys {
			clear(v.failures)
		}
		err = fmt.Errorf("public key %q: %w", kid, err)
		v.failures[kid] = albFailure{at: v.now(), err: err}
		v.log.Warn("auth: ALB public key fetch failed", "kid", kid, "error", err)
		return nil, err
	}
	if len(v.keys) >= albMaxKeys {
		clear(v.keys)
	}
	v.keys[kid] = k
	delete(v.failures, kid)
	return k, nil
}

// fetch downloads and parses the PEM public key <keyURL>/<kid>. Network
// errors and server-side failures (5xx, 429) wrap errKeysUnavailable; a 4xx
// answer means the kid does not exist (a forged token).
func (v *albVerifier) fetch(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), albKeyFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.keyURL+"/"+url.PathEscape(kid), nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errKeysUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errKeysUnavailable, err)
	}
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: key server returned %s", errKeysUnavailable, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("key server returned %s", resp.Status)
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("no PEM block in key response")
	}
	pk, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	ec, ok := pk.(*ecdsa.PublicKey)
	if !ok || ec.Curve != elliptic.P256() {
		return nil, errors.New("public key is not an ECDSA P-256 key")
	}
	return ec, nil
}

// authenticateALB authenticates with the ALB user claims header.
func (a *authenticator) authenticateALB(r *http.Request) (*identity.User, error) {
	raw := strings.TrimSpace(r.Header.Get(ALBHeader))
	if raw == "" {
		return nil, fmt.Errorf("%w: no %s header", ErrUnauthenticated, ALBHeader)
	}
	claims, err := a.alb.verify(r.Context(), raw)
	if errors.Is(err, errKeysUnavailable) {
		return nil, fmt.Errorf("auth: cannot verify ALB user claims: %w", err)
	}
	if err != nil {
		a.log.Warn("auth: invalid ALB user claims", "remote", r.RemoteAddr, "error", err)
		return nil, fmt.Errorf("%w: invalid ALB user claims: %w", ErrUnauthenticated, err)
	}
	return a.finalize(r.Context(), a.mapClaims(claims), srcIDP)
}
