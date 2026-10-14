package identity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thasami/backyard/internal/config"
)

// maxDirectoryBytes bounds the size of a directory file.
const maxDirectoryBytes = 256 << 20

// NewDirectory loads the configured static directory (nil, nil when type is "").
//
// Type "csv" expects a header row containing an "email" column; recognised
// columns are name, title, team (alias department), slack, avatar_url
// (aliases avatar, picture) and groups (';'-separated). Any other non-empty
// column is copied into User.Extra. Type "json" expects an array of user
// objects (or {"users": [...]}) with the same field names; groups may be an
// array or a ';'/','-separated string, and an "extra" object as well as any
// unknown keys end up in User.Extra.
//
// Lookups are case-insensitive on the email. When cfg.Reload is positive the
// file is re-checked (modification time and size) at most once per interval,
// on lookup, and re-read in the background when it changed; a failed reload
// keeps the previous contents.
func NewDirectory(cfg config.DirectoryConfig) (Directory, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Type))
	switch kind {
	case "", "none":
		return nil, nil
	case "csv", "json":
	default:
		return nil, fmt.Errorf("identity: directory type must be csv or json, got %q", cfg.Type)
	}
	if strings.TrimSpace(cfg.Path) == "" {
		return nil, errors.New("identity: identity.directory.path is required")
	}
	d := &staticDirectory{
		path:   cfg.Path,
		kind:   kind,
		reload: cfg.Reload.D(),
		log:    slog.Default(),
		now:    time.Now,
	}
	if err := d.load(); err != nil {
		return nil, err
	}
	d.lastCheck.Store(d.now().UnixNano())
	return d, nil
}

// staticDirectory is a Directory backed by a CSV or JSON file.
type staticDirectory struct {
	path   string
	kind   string
	reload time.Duration
	log    *slog.Logger
	now    func() time.Time

	mu      sync.RWMutex
	users   map[string]*User
	modTime time.Time
	size    int64

	lastCheck atomic.Int64 // unix nanoseconds of the last change check
	reloading atomic.Bool
	wg        sync.WaitGroup // background reloads (tests wait on it)
}

var _ Directory = (*staticDirectory)(nil)

// Lookup implements Directory. It returns a copy of the stored profile.
func (d *staticDirectory) Lookup(_ context.Context, email string) (*User, error) {
	key := strings.ToLower(strings.TrimSpace(email))
	if key == "" {
		return nil, nil
	}
	d.maybeReload()
	d.mu.RLock()
	u := d.users[key]
	d.mu.RUnlock()
	return u.Clone(), nil
}

// Len returns the number of profiles currently loaded.
func (d *staticDirectory) Len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.users)
}

// maybeReload starts a background reload when the reload interval elapsed
// since the last check and the file changed.
func (d *staticDirectory) maybeReload() {
	if d.reload <= 0 {
		return
	}
	now := d.now().UnixNano()
	last := d.lastCheck.Load()
	if now-last < int64(d.reload) || !d.lastCheck.CompareAndSwap(last, now) {
		return
	}
	if !d.reloading.CompareAndSwap(false, true) {
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer d.reloading.Store(false)
		if err := d.reloadIfChanged(); err != nil {
			d.log.Warn("identity: directory reload failed; keeping previous contents",
				"path", d.path, "error", err)
		}
	}()
}

// reloadIfChanged re-reads the file when its size or modification time changed.
func (d *staticDirectory) reloadIfChanged() error {
	fi, err := os.Stat(d.path)
	if err != nil {
		return fmt.Errorf("identity: stat directory: %w", err)
	}
	d.mu.RLock()
	same := fi.ModTime().Equal(d.modTime) && fi.Size() == d.size
	d.mu.RUnlock()
	if same {
		return nil
	}
	return d.load()
}

// load reads and parses the file and swaps the contents in.
func (d *staticDirectory) load() error {
	f, err := os.Open(d.path)
	if err != nil {
		return fmt.Errorf("identity: open directory: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("identity: stat directory: %w", err)
	}
	if fi.Size() > maxDirectoryBytes {
		return fmt.Errorf("identity: directory %s is larger than %d bytes", d.path, maxDirectoryBytes)
	}
	r := io.LimitReader(f, maxDirectoryBytes+1)
	var users map[string]*User
	switch d.kind {
	case "csv":
		users, err = ParseDirectoryCSV(r)
	default:
		users, err = ParseDirectoryJSON(r)
	}
	if err != nil {
		return fmt.Errorf("identity: load directory %s: %w", d.path, err)
	}
	d.mu.Lock()
	d.users = users
	d.modTime = fi.ModTime()
	d.size = fi.Size()
	d.mu.Unlock()
	d.log.Debug("identity: directory loaded", "path", d.path, "users", len(users))
	return nil
}

// csvColumn maps normalised CSV header names to profile fields.
var csvColumn = map[string]string{
	"email": "email", "mail": "email", "e_mail": "email", "email_address": "email",
	"name": "name", "full_name": "name", "display_name": "name", "displayname": "name",
	"title": "title", "job_title": "title",
	"team": "team", "department": "team",
	"slack": "slack", "slack_handle": "slack",
	"avatar_url": "avatar_url", "avatar": "avatar_url", "picture": "avatar_url", "photo": "avatar_url",
	"groups": "groups",
}

func normalizeHeader(h string) string {
	h = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\ufeff")))
	return strings.NewReplacer(" ", "_", "-", "_").Replace(h)
}

// ParseDirectoryCSV parses a CSV people directory (see NewDirectory) into a
// map keyed by lowercased email. Rows without a valid email are skipped; a
// later row for the same email replaces an earlier one.
func ParseDirectoryCSV(r io.Reader) (map[string]*User, error) {
	cr := csv.NewReader(bufio.NewReader(r))
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	cr.Comment = '#'
	header, err := cr.Read()
	if err == io.EOF {
		return nil, errors.New("empty csv (a header row with an email column is required)")
	}
	if err != nil {
		return nil, fmt.Errorf("read csv header: %w", err)
	}
	fields := make([]string, len(header))
	extraNames := make([]string, len(header))
	emailCol := -1
	for i, h := range header {
		n := normalizeHeader(h)
		if f, ok := csvColumn[n]; ok {
			fields[i] = f
			if f == "email" && emailCol < 0 {
				emailCol = i
			}
		} else if n != "" {
			extraNames[i] = strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))
		}
	}
	if emailCol < 0 {
		return nil, errors.New("csv header must include an \"email\" column")
	}
	users := make(map[string]*User)
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read csv: %w", err)
		}
		if emailCol >= len(rec) {
			continue
		}
		email := NormalizeEmail(rec[emailCol])
		if email == "" {
			continue
		}
		u := &User{ID: email, Email: email}
		for i, v := range rec {
			if i >= len(fields) {
				break
			}
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			switch fields[i] {
			case "name":
				u.Name = v
			case "title":
				u.Title = v
			case "team":
				u.Team = v
			case "slack":
				u.Slack = v
			case "avatar_url":
				u.AvatarURL = v
			case "groups":
				u.Groups = dedupeFold(strings.Split(v, ";"))
			case "":
				if extraNames[i] != "" {
					if u.Extra == nil {
						u.Extra = make(map[string]any)
					}
					u.Extra[extraNames[i]] = v
				}
			}
		}
		if u.Name == "" {
			u.Name = emailLocalPart(email)
		}
		users[email] = u
	}
	return users, nil
}

// ParseDirectoryJSON parses a JSON people directory: an array of user objects
// or an object with a "users" array (see NewDirectory).
func ParseDirectoryJSON(r io.Reader) (map[string]*User, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read json: %w", err)
	}
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte("\ufeff")))
	var entries []map[string]any
	switch {
	case len(raw) == 0:
		return nil, errors.New("empty json (an array of users is required)")
	case raw[0] == '[':
		if err := json.Unmarshal(raw, &entries); err != nil {
			return nil, fmt.Errorf("decode json: %w", err)
		}
	case raw[0] == '{':
		var wrap struct {
			Users []map[string]any `json:"users"`
		}
		if err := json.Unmarshal(raw, &wrap); err != nil {
			return nil, fmt.Errorf("decode json: %w", err)
		}
		entries = wrap.Users
	default:
		return nil, errors.New("json directory must be an array of users")
	}
	users := make(map[string]*User, len(entries))
	for _, e := range entries {
		u := userFromObject(e)
		if u == nil {
			continue
		}
		users[u.Email] = u
	}
	return users, nil
}

// userFromObject converts a JSON directory entry. It returns nil when the
// entry has no valid email.
func userFromObject(e map[string]any) *User {
	email := ""
	for _, k := range []string{"email", "mail"} {
		if s, ok := e[k].(string); ok {
			if email = NormalizeEmail(s); email != "" {
				break
			}
		}
	}
	if email == "" {
		return nil
	}
	u := &User{ID: email, Email: email}
	str := func(keys ...string) string {
		for _, k := range keys {
			if s := scalarString(e[k]); s != "" {
				if _, isList := e[k].([]any); !isList {
					return s
				}
			}
		}
		return ""
	}
	u.Name = str("name", "full_name", "display_name")
	u.Title = str("title", "job_title")
	u.Team = str("team", "department")
	u.Slack = str("slack")
	u.AvatarURL = str("avatar_url", "avatar", "picture")
	switch g := e["groups"].(type) {
	case string:
		u.Groups = dedupeFold(strings.FieldsFunc(g, func(r rune) bool { return r == ';' || r == ',' }))
	default:
		u.Groups = ParseList(g)
	}
	known := map[string]bool{
		"email": true, "mail": true, "id": true, "anonymous": true, "admin": true, "extra": true,
		"name": true, "full_name": true, "display_name": true, "title": true, "job_title": true,
		"team": true, "department": true, "slack": true, "avatar_url": true, "avatar": true,
		"picture": true, "groups": true,
	}
	if ex, ok := e["extra"].(map[string]any); ok {
		for k, v := range ex {
			if u.Extra == nil {
				u.Extra = make(map[string]any)
			}
			u.Extra[k] = v
		}
	}
	for k, v := range e {
		if known[k] || v == nil {
			continue
		}
		if u.Extra == nil {
			u.Extra = make(map[string]any)
		}
		u.Extra[k] = v
	}
	if u.Name == "" {
		u.Name = emailLocalPart(email)
	}
	return u
}
