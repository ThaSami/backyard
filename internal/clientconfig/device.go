package clientconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	// maxOAuthBody bounds IdP responses.
	maxOAuthBody = 1 << 20
	// defaultDeviceInterval is the RFC 8628 default polling interval (seconds).
	defaultDeviceInterval = 5
	// defaultDeviceExpiry applies when the IdP omits expires_in (seconds).
	defaultDeviceExpiry = 15 * 60
	// RefreshSkew is how long before expiry a refreshable token is renewed.
	RefreshSkew = 2 * time.Minute
)

// DefaultHTTPClient returns the HTTP client used for IdP calls when none is
// supplied: 30 s overall timeout.
func DefaultHTTPClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Discovery is the subset of OpenID Connect discovery metadata the device
// flow needs.
type Discovery struct {
	Issuer                      string `json:"issuer"`
	DeviceAuthorizationEndpoint string `json:"device_authorization_endpoint"`
	TokenEndpoint               string `json:"token_endpoint"`
}

// Discover fetches <issuer>/.well-known/openid-configuration.
func Discover(ctx context.Context, hc *http.Client, issuer string) (*Discovery, error) {
	if hc == nil {
		hc = DefaultHTTPClient()
	}
	issuer = strings.TrimSuffix(strings.TrimSpace(issuer), "/")
	if err := checkEndpoint(issuer); err != nil {
		return nil, fmt.Errorf("clientconfig: issuer: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, fmt.Errorf("clientconfig: discovery: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clientconfig: discovery: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody))
	if err != nil {
		return nil, fmt.Errorf("clientconfig: discovery: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clientconfig: discovery at %s: HTTP %d", issuer, resp.StatusCode)
	}
	var d Discovery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("clientconfig: discovery at %s: %w", issuer, err)
	}
	if d.TokenEndpoint == "" {
		return nil, fmt.Errorf("clientconfig: discovery at %s: no token_endpoint", issuer)
	}
	if err := checkEndpoint(d.TokenEndpoint); err != nil {
		return nil, fmt.Errorf("clientconfig: token_endpoint: %w", err)
	}
	if d.DeviceAuthorizationEndpoint != "" {
		if err := checkEndpoint(d.DeviceAuthorizationEndpoint); err != nil {
			return nil, fmt.Errorf("clientconfig: device_authorization_endpoint: %w", err)
		}
	}
	return &d, nil
}

// checkEndpoint requires https (http only for loopback hosts).
func checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid URL %q", raw)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && IsLocalHost(u.Host):
		return nil
	}
	return fmt.Errorf("%q must use https", raw)
}

// DeviceAuthorization is an RFC 8628 §3.2 device authorization response.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval,omitempty"`
}

// UnmarshalJSON accepts the "verification_url" spelling used by some IdPs.
func (d *DeviceAuthorization) UnmarshalJSON(b []byte) error {
	type plain DeviceAuthorization
	aux := struct {
		*plain
		VerificationURL string `json:"verification_url"`
	}{plain: (*plain)(d)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if d.VerificationURI == "" {
		d.VerificationURI = aux.VerificationURL
	}
	return nil
}

// TokenResponse is an OAuth 2.0 token endpoint response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
}

// OAuthError is an error response from an OAuth endpoint.
type OAuthError struct {
	Status      int
	Code        string
	Description string
}

func (e *OAuthError) Error() string {
	msg := "oauth error " + e.Code
	if e.Description != "" {
		msg += ": " + e.Description
	}
	if e.Status != 0 && e.Status != http.StatusOK {
		msg += fmt.Sprintf(" (HTTP %d)", e.Status)
	}
	return msg
}

// ErrDeviceExpired is returned when the user did not approve the device
// code in time.
var ErrDeviceExpired = errors.New("the device code expired before the login was approved")

// ErrDeviceDenied is returned when the user denied the device login.
var ErrDeviceDenied = errors.New("the device login was denied")

// DeviceFlow performs the OAuth 2.0 device authorization grant (RFC 8628).
type DeviceFlow struct {
	// HTTP is the client for IdP calls (default DefaultHTTPClient).
	HTTP *http.Client
	// ClientID is the public client id (info.auth.device.client_id).
	ClientID string
	// Scopes requested (info.auth.device.scopes).
	Scopes []string
	// Discovery holds the IdP endpoints (see Discover).
	Discovery *Discovery
	// PollUnit is the length of one interval second (default time.Second);
	// tests shorten it.
	PollUnit time.Duration
}

func (f *DeviceFlow) httpClient() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return DefaultHTTPClient()
}

// Start requests a device and user code.
func (f *DeviceFlow) Start(ctx context.Context) (*DeviceAuthorization, error) {
	if f.Discovery == nil || f.Discovery.DeviceAuthorizationEndpoint == "" {
		return nil, errors.New("clientconfig: the identity provider does not support the device flow (no device_authorization_endpoint)")
	}
	form := url.Values{"client_id": {f.ClientID}}
	if len(f.Scopes) > 0 {
		form.Set("scope", strings.Join(f.Scopes, " "))
	}
	var da DeviceAuthorization
	if err := postForm(ctx, f.httpClient(), f.Discovery.DeviceAuthorizationEndpoint, form, &da); err != nil {
		return nil, fmt.Errorf("clientconfig: device authorization: %w", err)
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return nil, errors.New("clientconfig: device authorization: incomplete response from the identity provider")
	}
	if da.ExpiresIn <= 0 {
		da.ExpiresIn = defaultDeviceExpiry
	}
	if da.Interval <= 0 {
		da.Interval = defaultDeviceInterval
	}
	return &da, nil
}

// Poll polls the token endpoint until the user approves, denies or the code
// expires, honouring the interval and slow_down responses.
func (f *DeviceFlow) Poll(ctx context.Context, da *DeviceAuthorization) (*TokenResponse, error) {
	unit := f.PollUnit
	if unit <= 0 {
		unit = time.Second
	}
	interval := da.Interval
	if interval <= 0 {
		interval = defaultDeviceInterval
	}
	expires := da.ExpiresIn
	if expires <= 0 {
		expires = defaultDeviceExpiry
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(expires)*unit)
	defer cancel()
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {da.DeviceCode},
		"client_id":   {f.ClientID},
	}
	for {
		t := time.NewTimer(time.Duration(interval) * unit)
		select {
		case <-ctx.Done():
			t.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrDeviceExpired
			}
			return nil, ctx.Err()
		case <-t.C:
		}
		var tok TokenResponse
		err := postForm(ctx, f.httpClient(), f.Discovery.TokenEndpoint, form, &tok)
		if err == nil {
			if tok.AccessToken == "" && tok.IDToken == "" {
				return nil, errors.New("clientconfig: token response without a token")
			}
			return &tok, nil
		}
		var oe *OAuthError
		if !errors.As(err, &oe) {
			if ctx.Err() != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil, ErrDeviceExpired
				}
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("clientconfig: device token: %w", err)
		}
		switch oe.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5
		case "access_denied":
			return nil, ErrDeviceDenied
		case "expired_token":
			return nil, ErrDeviceExpired
		default:
			return nil, fmt.Errorf("clientconfig: device token: %w", oe)
		}
	}
}

// Refresh exchanges a refresh token at tokenEndpoint.
func Refresh(ctx context.Context, hc *http.Client, tokenEndpoint, clientID, refreshToken string) (*TokenResponse, error) {
	if hc == nil {
		hc = DefaultHTTPClient()
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	var tok TokenResponse
	if err := postForm(ctx, hc, tokenEndpoint, form, &tok); err != nil {
		return nil, fmt.Errorf("clientconfig: refresh token: %w", err)
	}
	if tok.AccessToken == "" && tok.IDToken == "" {
		return nil, errors.New("clientconfig: refresh token: response without a token")
	}
	return &tok, nil
}

// Credential picks the bearer token to present to Backyard and its expiry.
// The access token is used when it is a JWT issued for clientID (aud, azp
// or cid); otherwise the ID token (whose audience is the client) when
// present; otherwise the access token as is.
func (t *TokenResponse) Credential(clientID string, now time.Time) (string, time.Time) {
	chosen := t.AccessToken
	if c, ok := ParseJWTUnverified(t.AccessToken); !ok || !issuedFor(c, clientID) {
		if t.IDToken != "" {
			chosen = t.IDToken
		}
	}
	if chosen == "" {
		chosen = t.IDToken
	}
	if exp, ok := TokenExpiry(chosen); ok {
		return chosen, exp
	}
	if t.ExpiresIn > 0 && chosen == t.AccessToken {
		return chosen, now.Add(time.Duration(t.ExpiresIn) * time.Second).UTC()
	}
	return chosen, time.Time{}
}

func issuedFor(c *JWTClaims, clientID string) bool {
	if clientID == "" {
		return false
	}
	return c.Azp == clientID || c.Cid == clientID || slices.Contains(c.Audiences(), clientID)
}

// RefreshProfile renews p's token in place when it is refreshable and
// expires within skew of now. It reports whether p changed (and so should
// be saved).
func RefreshProfile(ctx context.Context, hc *http.Client, p *Profile, now time.Time, skew time.Duration) (bool, error) {
	if !p.CanRefresh() || (p.Token != "" && !p.TokenExpiresAt.IsZero() && !p.TokenExpiresWithin(now, skew)) {
		return false, nil
	}
	if p.Token != "" && p.TokenExpiresAt.IsZero() {
		return false, nil // no known expiry: nothing to anticipate
	}
	endpoint := p.Device.TokenEndpoint
	if endpoint == "" {
		d, err := Discover(ctx, hc, p.Device.Issuer)
		if err != nil {
			return false, err
		}
		endpoint = d.TokenEndpoint
		p.Device.TokenEndpoint = endpoint
	}
	tok, err := Refresh(ctx, hc, endpoint, p.Device.ClientID, p.RefreshToken)
	if err != nil {
		return false, err
	}
	p.Token, p.TokenExpiresAt = tok.Credential(p.Device.ClientID, now)
	if tok.RefreshToken != "" {
		p.RefreshToken = tok.RefreshToken
	}
	return true, nil
}

// postForm POSTs form and decodes a JSON success into out, or returns an
// *OAuthError for OAuth error responses (some IdPs answer errors with 200).
func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthBody))
	if err != nil {
		return err
	}
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Error != "" {
		return &OAuthError{Status: resp.StatusCode, Code: e.Error, Description: e.Description}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &OAuthError{Status: resp.StatusCode, Code: "http_error", Description: http.StatusText(resp.StatusCode)}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
