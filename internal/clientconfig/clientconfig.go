// Package clientconfig manages the local state of the Backyard CLI and MCP
// server (SPEC §13): the per-user config file with named profiles
// ($XDG_CONFIG_HOME/backyard/config.json, mode 0600), environment overrides,
// the per-project .backyard.json file, gateway URL resolution, token expiry
// awareness and the OAuth device-flow helpers used to obtain and refresh
// IdP-issued tokens.
//
// The package performs no terminal I/O; callers decide how to prompt.
package clientconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/thasami/backyard/internal/deploy"
)

// Environment variables that override profile settings.
const (
	EnvServer       = "BACKYARD_SERVER"
	EnvGateway      = "BACKYARD_GATEWAY"
	EnvToken        = "BACKYARD_TOKEN"
	EnvProfile      = "BACKYARD_PROFILE"
	EnvDeployMethod = "BACKYARD_DEPLOY_METHOD"
)

// DefaultProfile is the profile used when none is selected.
const DefaultProfile = "default"

// Login methods recorded in Profile.Login.
const (
	LoginToken     = "token"
	LoginBrowser   = "browser"
	LoginDevice    = "device"
	LoginAnonymous = "anonymous"
)

// Deploy methods (SPEC §13).
const (
	MethodAuto  = "auto"
	MethodAPI   = "api"
	MethodS3    = "s3"
	MethodGCS   = "gcs"
	MethodAzure = "azure"
	MethodRsync = "rsync"
)

// Methods lists every valid deploy method, "auto" first.
var Methods = []string{MethodAuto, MethodAPI, MethodS3, MethodGCS, MethodAzure, MethodRsync}

// ValidMethod reports whether m is a known deploy method ("" counts as auto).
func ValidMethod(m string) bool { return m == "" || slices.Contains(Methods, m) }

// maxConfigBytes bounds the config file read.
const maxConfigBytes = 4 << 20

// File is the on-disk CLI configuration document.
type File struct {
	// Current is the active profile name.
	Current string `json:"current"`
	// Profiles by name.
	Profiles map[string]*Profile `json:"profiles"`
}

// Profile is one Backyard server the CLI talks to, with its credentials.
type Profile struct {
	// Server is the hub URL, e.g. https://backyard.acme.com.
	Server string `json:"server,omitempty"`
	// Gateway is an explicit deploy gateway URL (standalone gateway, or the
	// proxy-exempt path); empty means Server + "/__backyard/api/deploy".
	Gateway string `json:"gateway,omitempty"`
	// Token is the bearer token (Backyard API token, static token or IdP JWT).
	Token string `json:"token,omitempty"`
	// TokenExpiresAt is when Token expires (zero when unknown or never).
	TokenExpiresAt time.Time `json:"token_expires_at,omitzero"`
	// Method is the preferred deploy method (auto|api|s3|gcs|azure|rsync).
	Method string `json:"method,omitempty"`
	// RefreshToken is the IdP refresh token of a device-flow login.
	RefreshToken string `json:"refresh_token,omitempty"`
	// Device holds what is needed to refresh a device-flow token.
	Device *Device `json:"device,omitempty"`
	// Login records how the credentials were obtained (token, browser,
	// device or anonymous).
	Login string `json:"login,omitempty"`
	// User is the identity behind the credentials at login time.
	User *User `json:"user,omitempty"`
	// Info caches the gateway's /info document from the last successful
	// contact so direct-sync deploys work while the gateway is unreachable.
	Info *deploy.Info `json:"info,omitempty"`
}

// Device describes the IdP used for a device-flow login.
type Device struct {
	Issuer        string   `json:"issuer"`
	ClientID      string   `json:"client_id"`
	Scopes        []string `json:"scopes,omitempty"`
	TokenEndpoint string   `json:"token_endpoint,omitempty"`
}

// User is the compact identity remembered for a profile.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
	Admin bool   `json:"admin,omitempty"`
}

// Is reports whether actor (a SiteMeta created_by/updated_by value) denotes
// this user.
func (u *User) Is(actor string) bool {
	if u == nil || actor == "" {
		return false
	}
	return strings.EqualFold(actor, u.ID) || (u.Email != "" && strings.EqualFold(actor, u.Email))
}

// Clone returns a deep copy of p (nil-safe).
func (p *Profile) Clone() *Profile {
	if p == nil {
		return nil
	}
	c := *p
	if p.Device != nil {
		d := *p.Device
		d.Scopes = slices.Clone(p.Device.Scopes)
		c.Device = &d
	}
	if p.User != nil {
		u := *p.User
		c.User = &u
	}
	if p.Info != nil {
		b, err := json.Marshal(p.Info)
		if err == nil {
			var info deploy.Info
			if json.Unmarshal(b, &info) == nil {
				c.Info = &info
			}
		}
	}
	return &c
}

// ClearCredentials removes every secret and identity from the profile while
// keeping its server settings.
func (p *Profile) ClearCredentials() {
	p.Token = ""
	p.TokenExpiresAt = time.Time{}
	p.RefreshToken = ""
	p.Device = nil
	p.Login = ""
	p.User = nil
}

// GatewayURL returns the profile's effective gateway URL.
func (p *Profile) GatewayURL() string {
	if p == nil {
		return ""
	}
	return GatewayURL(p.Server, p.Gateway)
}

// DefaultPath returns the config file location:
// $XDG_CONFIG_HOME/backyard/config.json, falling back to
// ~/.config/backyard/config.json (%AppData%\backyard\config.json on Windows).
func DefaultPath() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "backyard", "config.json"), nil
	}
	if runtime.GOOS == "windows" {
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("clientconfig: locate config dir: %w", err)
		}
		return filepath.Join(dir, "backyard", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("clientconfig: locate home dir: %w", err)
	}
	return filepath.Join(home, ".config", "backyard", "config.json"), nil
}

// Load reads the config file at path. A missing file yields an empty config.
func Load(path string) (*File, error) {
	f := &File{Profiles: map[string]*Profile{}}
	b, err := readLimited(path, maxConfigBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("clientconfig: read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return f, nil
	}
	if err := json.Unmarshal(b, f); err != nil {
		return nil, fmt.Errorf("clientconfig: parse %s: %w", path, err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]*Profile{}
	}
	for name, p := range f.Profiles {
		if p == nil {
			delete(f.Profiles, name)
		}
	}
	return f, nil
}

// Save writes the config atomically with mode 0600, creating the directory
// with mode 0700 when needed. Wider permissions on an existing file or
// directory are tightened.
func (f *File) Save(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("clientconfig: create %s: %w", dir, err)
	}
	if st, err := os.Stat(dir); err == nil && st.Mode().Perm()&0o077 != 0 && filepath.Base(dir) == "backyard" {
		_ = os.Chmod(dir, 0o700)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("clientconfig: encode: %w", err)
	}
	b = append(b, '\n')
	return writeFileAtomic(path, b, 0o600)
}

// writeFileAtomic writes data to a temp file in the same directory and
// renames it over path.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("clientconfig: write %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after a successful rename
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return fmt.Errorf("clientconfig: chmod %s: %w", name, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("clientconfig: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("clientconfig: sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("clientconfig: close %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("clientconfig: replace %s: %w", path, err)
	}
	return nil
}

// readLimited reads at most limit bytes from path.
func readLimited(path string, limit int64) ([]byte, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("file is larger than %d bytes", limit)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(fh); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ActiveName returns the profile selected by flag, else $BACKYARD_PROFILE,
// else the file's current profile, else "default".
func (f *File) ActiveName(flag string) string {
	if n := strings.TrimSpace(flag); n != "" {
		return n
	}
	if n := strings.TrimSpace(os.Getenv(EnvProfile)); n != "" {
		return n
	}
	if f != nil && f.Current != "" {
		return f.Current
	}
	return DefaultProfile
}

// Profile returns the named profile, or nil.
func (f *File) Profile(name string) *Profile {
	if f == nil || f.Profiles == nil {
		return nil
	}
	return f.Profiles[name]
}

// Set stores p under name.
func (f *File) Set(name string, p *Profile) {
	if f.Profiles == nil {
		f.Profiles = map[string]*Profile{}
	}
	f.Profiles[name] = p
}

// Delete removes the named profile; the current selection is cleared when it
// pointed at it.
func (f *File) Delete(name string) {
	delete(f.Profiles, name)
	if f.Current == name {
		f.Current = ""
	}
}

// Names returns the profile names, sorted.
func (f *File) Names() []string {
	names := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// ValidProfileName reports whether name is usable as a profile name.
func ValidProfileName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
