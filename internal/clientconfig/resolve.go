package clientconfig

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/deploy"
)

// GatewayPath is where the server embeds the deploy gateway.
const GatewayPath = deploy.EmbeddedPath

// Overrides are explicit settings (command-line flags) that take precedence
// over the environment and the stored profile.
type Overrides struct {
	Profile string
	Server  string
	Gateway string
	Token   string
	Method  string
}

// Source names where a resolved setting came from.
type Source string

// Setting sources, in precedence order.
const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceProfile Source = "profile"
	SourceNone    Source = ""
)

// Resolved is the effective client configuration for one invocation.
type Resolved struct {
	// ProfileName is the selected profile (it may not exist yet).
	ProfileName string
	// Profile is a copy of the stored profile (never nil).
	Profile *Profile
	// Exists reports whether the profile is stored in the file.
	Exists bool

	Server  string
	Gateway string // effective gateway URL ("" when no server is known)
	Token   string
	Method  string

	ServerSource Source
	TokenSource  Source
}

// Resolve merges flags > environment > profile into the effective settings.
// URLs are normalized; invalid URLs yield an error naming their source.
func (f *File) Resolve(o Overrides) (*Resolved, error) {
	name := f.ActiveName(o.Profile)
	stored := f.Profile(name)
	r := &Resolved{ProfileName: name, Exists: stored != nil, Profile: stored.Clone()}
	if r.Profile == nil {
		r.Profile = &Profile{}
	}
	p := r.Profile

	server, serverSrc := pick(o.Server, EnvServer, p.Server)
	gateway, gatewaySrc := pick(o.Gateway, EnvGateway, p.Gateway)
	token, tokenSrc := pick(o.Token, EnvToken, p.Token)
	method, _ := pick(o.Method, EnvDeployMethod, p.Method)

	var err error
	if server != "" {
		if server, err = NormalizeURL(server); err != nil {
			return nil, fmt.Errorf("server URL (from %s): %w", serverSrc, err)
		}
	}
	if gateway != "" {
		if gateway, err = NormalizeURL(gateway); err != nil {
			return nil, fmt.Errorf("gateway URL (from %s): %w", gatewaySrc, err)
		}
	}
	// A server given explicitly (flag/env) for a different host than the
	// profile's must not inherit the profile's credentials or gateway.
	if serverSrc != SourceProfile && p.Server != "" && server != "" && !sameURL(server, p.Server) {
		if gatewaySrc == SourceProfile {
			gateway = ""
		}
		if tokenSrc == SourceProfile {
			token, tokenSrc = "", SourceNone
		}
	}
	if method == "" {
		method = MethodAuto
	}
	if !ValidMethod(method) {
		return nil, fmt.Errorf("unknown deploy method %q (want one of %s)", method, strings.Join(Methods, ", "))
	}
	r.Server, r.ServerSource = server, serverSrc
	r.Gateway = GatewayURL(server, gateway)
	r.Token, r.TokenSource = token, tokenSrc
	r.Method = method
	return r, nil
}

// pick returns the first non-empty of flag, $env, stored with its source.
func pick(flag, env, stored string) (string, Source) {
	if v := strings.TrimSpace(flag); v != "" {
		return v, SourceFlag
	}
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, SourceEnv
	}
	if v := strings.TrimSpace(stored); v != "" {
		return v, SourceProfile
	}
	return "", SourceNone
}

func sameURL(a, b string) bool {
	return strings.EqualFold(strings.TrimSuffix(a, "/"), strings.TrimSuffix(b, "/"))
}

// UsesProfileServer reports whether the effective server and gateway are the
// stored profile's own, i.e. no flag or environment variable pointed this
// invocation at another server or gateway.
func (r *Resolved) UsesProfileServer() bool {
	if r == nil || !r.Exists || r.Profile == nil {
		return false
	}
	p := r.Profile
	return sameURL(r.Server, p.Server) && sameURL(r.Gateway, GatewayURL(p.Server, p.Gateway))
}

// CachedInfo returns the server info cached in the profile, or nil when this
// invocation targets another server or gateway than the profile's: the cache
// describes the profile's server only (its storage, URLs and methods), and
// applying it to another one would, for example, point a direct-sync deploy
// at the wrong bucket.
func (r *Resolved) CachedInfo() *deploy.Info {
	if !r.UsesProfileServer() {
		return nil
	}
	return r.Profile.Info
}

// GatewayURL returns gateway if set, else server + "/__backyard/api/deploy"
// ("" when both are empty).
func GatewayURL(server, gateway string) string {
	if g := strings.TrimSuffix(strings.TrimSpace(gateway), "/"); g != "" {
		return g
	}
	if s := strings.TrimSuffix(strings.TrimSpace(server), "/"); s != "" {
		return s + GatewayPath
	}
	return ""
}

// NormalizeURL validates an http(s) base URL, adding a scheme when missing
// (http:// for localhost and loopback addresses, https:// otherwise) and
// dropping the trailing slash, query and fragment.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("empty URL")
	}
	if !strings.Contains(raw, "://") {
		host := raw
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		if IsLocalHost(host) {
			raw = "http://" + raw
		} else {
			raw = "https://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid URL %q: missing host", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid URL %q: credentials in URLs are not supported", redactUserinfo(u))
	}
	u.RawQuery, u.Fragment, u.RawFragment = "", "", ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}

func redactUserinfo(u *url.URL) string {
	c := *u
	c.User = url.User("REDACTED")
	return c.String()
}

// IsLocalHost reports whether host (optionally with a port) is localhost, a
// *.localhost name or a loopback IP.
func IsLocalHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// SiteURL returns the public URL of site for a server described by info. In
// host mode that is <scheme>://<site>.<base_domain>[:<port>]/ with the scheme
// and port of the public URL (base_domain may differ from the public URL's
// host; without one the public URL's host is used), in path mode
// <public_url>/s/<site>/. server is used when info lacks a public URL.
func SiteURL(info *deploy.Info, server, site string) string {
	public, mode, base := server, "host", ""
	if info != nil {
		if info.PublicURL != "" {
			public = info.PublicURL
		}
		if info.URLMode != "" {
			mode = info.URLMode
		}
		base = info.BaseDomain
	}
	public = strings.TrimSuffix(public, "/")
	if public == "" {
		return ""
	}
	if mode == "path" {
		return public + "/s/" + url.PathEscape(site) + "/"
	}
	u, err := url.Parse(public)
	if err != nil || u.Host == "" {
		return ""
	}
	host := bareHost(base)
	if host == "" {
		host = bareHost(u.Host)
	}
	if host == "" {
		return ""
	}
	host = site + "." + host
	if p := u.Port(); p != "" {
		host = net.JoinHostPort(host, p)
	}
	return (&url.URL{Scheme: strings.ToLower(u.Scheme), Host: host, Path: "/"}).String()
}

// bareHost lowercases a host name and drops a port, IPv6 brackets and a
// trailing dot.
func bareHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimSuffix(strings.Trim(h, "[]"), ".")
}

// HubURL returns the hub URL for a server described by info.
func HubURL(info *deploy.Info, server string) string {
	if info != nil && info.PublicURL != "" {
		return strings.TrimSuffix(info.PublicURL, "/") + "/"
	}
	if server == "" {
		return ""
	}
	return strings.TrimSuffix(server, "/") + "/"
}

// TokenExpired reports whether the profile's token has a known expiry that
// is not after now.
func (p *Profile) TokenExpired(now time.Time) bool {
	return p != nil && p.Token != "" && !p.TokenExpiresAt.IsZero() && !now.Before(p.TokenExpiresAt)
}

// TokenExpiresWithin reports whether the token has a known expiry within d
// of now (expired tokens included).
func (p *Profile) TokenExpiresWithin(now time.Time, d time.Duration) bool {
	return p != nil && p.Token != "" && !p.TokenExpiresAt.IsZero() && now.Add(d).After(p.TokenExpiresAt)
}

// CanRefresh reports whether the profile holds a device-flow refresh token.
func (p *Profile) CanRefresh() bool {
	return p != nil && p.RefreshToken != "" && p.Device != nil && p.Device.ClientID != "" &&
		(p.Device.TokenEndpoint != "" || p.Device.Issuer != "")
}

// JWTClaims is the subset of unverified JWT claims the CLI inspects.
type JWTClaims struct {
	Exp   int64           `json:"exp"`
	Iss   string          `json:"iss"`
	Sub   string          `json:"sub"`
	Email string          `json:"email"`
	Aud   json.RawMessage `json:"aud"`
	Azp   string          `json:"azp"`
	Cid   string          `json:"cid"`
}

// Audiences returns the "aud" claim as a list.
func (c *JWTClaims) Audiences() []string {
	if len(c.Aud) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(c.Aud, &many)
	return many
}

// ParseJWTUnverified decodes the claims of a compact JWT without verifying
// it. It is only used for local decisions (expiry display, token choice);
// servers always verify. ok is false for anything that is not a JWT.
func ParseJWTUnverified(token string) (claims *JWTClaims, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[1] == "" || len(token) > 64<<10 {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var c JWTClaims
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, false
	}
	return &c, true
}

// TokenExpiry returns the "exp" of a JWT token (ok=false for opaque tokens
// or JWTs without exp).
func TokenExpiry(token string) (time.Time, bool) {
	c, ok := ParseJWTUnverified(token)
	if !ok || c.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(c.Exp, 0).UTC(), true
}
