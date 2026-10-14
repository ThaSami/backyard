package clientconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/thasami/backyard/internal/deploy"
)

// ErrNoServer is returned when no server or gateway is configured.
var ErrNoServer = errors.New("no Backyard server configured: run `backyard login --server https://backyard.example.com` (or set BACKYARD_SERVER)")

// SessionOptions configure OpenSession.
type SessionOptions struct {
	// ConfigPath is the config file (default DefaultPath()).
	ConfigPath string
	// Overrides are flag values.
	Overrides Overrides
	// HTTP is used for IdP token refreshes (default DefaultHTTPClient()).
	HTTP *http.Client
	// Now returns the current time (default time.Now).
	Now func() time.Time
	// Logger receives debug/warn logs (default: discard).
	Logger *slog.Logger
}

// Session is the effective client state for one command: the config file,
// the resolved settings and helpers to persist profile changes.
type Session struct {
	*Resolved
	// Path is the config file path.
	Path string
	// File is the loaded config file.
	File *File
	// Warnings collects non-fatal problems worth showing the user (expired
	// tokens, failed refreshes).
	Warnings []string

	now func() time.Time
	log *slog.Logger
}

// OpenSession loads the config, resolves flags/env/profile and, when the
// stored device-flow token is about to expire, refreshes and persists it.
func OpenSession(ctx context.Context, o SessionOptions) (*Session, error) {
	path := o.ConfigPath
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	f, err := Load(path)
	if err != nil {
		return nil, err
	}
	r, err := f.Resolve(o.Overrides)
	if err != nil {
		return nil, err
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Session{Resolved: r, Path: path, File: f, now: now, log: log}

	if r.TokenSource == SourceProfile && r.Profile.CanRefresh() {
		p := r.Profile.Clone()
		changed, err := RefreshProfile(ctx, o.HTTP, p, now(), RefreshSkew)
		switch {
		case err != nil:
			log.Warn("token refresh failed", "profile", r.ProfileName, "error", err)
			s.Warnings = append(s.Warnings, fmt.Sprintf("could not refresh the login token (%v); run `backyard login --device` again", err))
		case changed:
			log.Debug("refreshed device token", "profile", r.ProfileName, "expires_at", p.TokenExpiresAt)
			r.Profile, r.Token = p, p.Token
			if err := s.UpdateProfile(func(stored *Profile) {
				stored.Token, stored.TokenExpiresAt, stored.RefreshToken = p.Token, p.TokenExpiresAt, p.RefreshToken
				if stored.Device != nil && p.Device != nil {
					stored.Device.TokenEndpoint = p.Device.TokenEndpoint
				}
			}); err != nil {
				log.Warn("saving refreshed token failed", "error", err)
			}
		}
	}
	switch {
	case r.TokenSource != SourceProfile:
	case r.Profile.TokenExpired(now()):
		s.Warnings = append(s.Warnings, fmt.Sprintf("your login for %s expired at %s; run `backyard login` again",
			displayServer(r), r.Profile.TokenExpiresAt.Local().Format(time.RFC1123)))
	case !r.Profile.CanRefresh() && r.Profile.TokenExpiresWithin(now(), ExpiryWarning):
		s.Warnings = append(s.Warnings, fmt.Sprintf("your login for %s expires at %s; run `backyard login` to renew it",
			displayServer(r), r.Profile.TokenExpiresAt.Local().Format(time.RFC1123)))
	}
	return s, nil
}

// ExpiryWarning is how long before a (non-refreshable) stored token
// expires that sessions start warning about it.
const ExpiryWarning = 24 * time.Hour

func displayServer(r *Resolved) string {
	if r.Server != "" {
		return r.Server
	}
	if r.Gateway != "" {
		return r.Gateway
	}
	return "the server"
}

// Now returns the session clock's current time.
func (s *Session) Now() time.Time { return s.now() }

// RequireGateway returns ErrNoServer when no gateway URL is known.
func (s *Session) RequireGateway() error {
	if s.Gateway == "" {
		return ErrNoServer
	}
	return nil
}

// Client returns a deploy gateway client authenticated with the session's
// token. hc replaces the client's HTTP client when non-nil.
func (s *Session) Client(hc *http.Client) *deploy.Client {
	c := deploy.NewClient(s.Gateway, s.Token)
	if hc != nil {
		c.HTTP = hc
	}
	return c
}

// UpdateProfile re-reads the config file, applies mutate to the selected
// profile (created when missing) and saves it, so concurrent CLI processes
// do not clobber unrelated changes. The session's copy is updated too.
func (s *Session) UpdateProfile(mutate func(p *Profile)) error {
	f, err := Load(s.Path)
	if err != nil {
		return err
	}
	p := f.Profile(s.ProfileName)
	if p == nil {
		p = &Profile{}
		f.Set(s.ProfileName, p)
	}
	mutate(p)
	if err := f.Save(s.Path); err != nil {
		return err
	}
	s.File = f
	s.Exists = true
	mutate(s.Profile)
	return nil
}

// CacheInfo stores info in the profile (best effort) when the session uses
// the stored profile's own server and gateway (see CachedInfo).
func (s *Session) CacheInfo(info *deploy.Info) {
	if info == nil || !s.UsesProfileServer() || reflect.DeepEqual(info, s.Profile.Info) {
		return
	}
	if err := s.UpdateProfile(func(p *Profile) { p.Info = info }); err != nil {
		s.log.Debug("caching server info failed", "error", err)
	}
}
