package mcpserver

// Safety rails for tools driven by a coding agent: which local folders the
// agent may scaffold into or publish, which remote files it may read, and
// keeping credentials out of everything a tool returns.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thasami/backyard/internal/clientconfig"
	"github.com/thasami/backyard/internal/deploy"
)

// ---------------------------------------------------------------------------
// Local folders

// resolveDir resolves the folder an agent passed to backyard_init or
// backyard_deploy. The agent works in the project it was started in, so a
// relative path must stay inside the working directory tree — also once
// symlinks are resolved. A folder anywhere else is accepted only when the
// agent names it explicitly by its absolute path (or ~/…) and it already
// exists. The filesystem root and the home directory are never accepted:
// publishing them would upload far more than a site.
func (s *server) resolveDir(raw string) (string, error) {
	dir := strings.TrimSpace(raw)
	if dir == "" {
		return "", errors.New("dir is required")
	}
	if strings.ContainsRune(dir, 0) {
		return "", errors.New("dir contains a NUL byte")
	}
	cwd, err := s.cwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine the working directory: %w", err)
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("the working directory %q is not absolute", cwd)
	}
	cwd = filepath.Clean(cwd)

	explicit := false
	switch {
	case dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, "~"+string(filepath.Separator)):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand %q: %w", raw, err)
		}
		dir, explicit = filepath.Join(home, dir[1:]), true
	case filepath.IsAbs(dir):
		dir, explicit = filepath.Clean(dir), true
	default:
		dir = filepath.Join(cwd, dir)
	}

	real := realPath(dir)
	if !within(realPath(cwd), real) {
		if !explicit {
			return "", fmt.Errorf("%q is outside the working directory %s: Backyard tools only scaffold and deploy "+
				"inside the folder the agent was started in. To use another folder, pass its absolute path "+
				"(it must already exist)", raw, cwd)
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			return "", fmt.Errorf("%s is outside the working directory %s and is not an existing directory: "+
				"create it first, or use a folder inside the working directory", dir, cwd)
		}
	}
	if filepath.Dir(real) == real {
		return "", fmt.Errorf("refusing to use the filesystem root %s as a site folder: pass the project folder", dir)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && realPath(home) == real {
		return "", fmt.Errorf("refusing to use the home directory %s as a site folder: pass the project folder", dir)
	}
	if why := s.sensitiveDir(dir, real, cwd, explicit && !within(realPath(cwd), real)); why != "" {
		return "", fmt.Errorf("refusing to use %s as a site folder: %s. Pass the project folder", dir, why)
	}
	return dir, nil
}

// sensitiveDir reports why a folder must never be scaffolded into or
// published ("" when it may). dir is the folder as named, real the same
// with symlinks resolved; outside reports an explicit folder outside the
// working tree. The default ignore rules only look at paths below the
// deployed folder, so publishing ~/.ssh itself would upload the private keys
// in it; a prompt-injected or confused agent must not be able to do that:
//
//   - the Backyard CLI's own config folder (login and refresh tokens, the
//     Cloudflare deploy tokens), anything in it, and any folder whose deploy
//     would include its config file;
//   - hidden folders directly in the home directory (~/.ssh, ~/.aws,
//     ~/.config, ~/.gnupg, ~/.kube…) and the platform equivalents ~/Library
//     and ~/AppData, unless the agent was started inside that same folder;
//   - outside the working tree, any folder with a path segment starting
//     with "." (other than .well-known).
func (s *server) sensitiveDir(dir, real, cwd string, outside bool) string {
	if def := defaultConfigPath(); def != "" {
		// The default config folder is Backyard's own: everything in it
		// (config.json, the Cloudflare setup's deploy tokens) is secret.
		if cfgDir := filepath.Dir(def); within(cfgDir, real) || within(realPath(cfgDir), real) {
			return "it holds the Backyard CLI's credentials (" + cfgDir + ")"
		}
	}
	for _, cfg := range s.configFiles() {
		if within(real, cfg) {
			rel, err := filepath.Rel(real, cfg)
			if err == nil && !defaultIgnored(filepath.ToSlash(rel)) {
				return "a deploy would include the Backyard CLI's credentials (" + cfg + ")"
			}
		}
	}
	realCwd := realPath(cwd)
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, h := range []struct{ home, p string }{{home, dir}, {realPath(home), real}} {
			rel, err := filepath.Rel(h.home, h.p)
			if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
			if !strings.HasPrefix(first, ".") && first != "Library" && first != "AppData" {
				continue
			}
			if tmp := os.TempDir(); first == "AppData" && (within(tmp, h.p) || within(realPath(tmp), h.p)) {
				continue // Windows' temporary folder lives in AppData
			}
			top := filepath.Join(h.home, first)
			if !within(realPath(top), realCwd) && !within(top, cwd) {
				return "~/" + first + " holds keys, credentials and settings, not a site"
			}
		}
	}
	if outside {
		for _, p := range []string{dir, real} {
			for seg := range strings.SplitSeq(filepath.ToSlash(p), "/") {
				if strings.HasPrefix(seg, ".") && seg != ".well-known" {
					return "outside the working directory, hidden folders (" + seg + ") are never used"
				}
			}
		}
	}
	return ""
}

// configFiles returns the CLI config files holding credentials: the one
// this server uses and the default one (absolute, symlinks resolved).
func (s *server) configFiles() []string {
	var out []string
	for _, p := range []string{s.configPath(), defaultConfigPath()} {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = realPath(abs)
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	return out
}

// defaultConfigPath returns clientconfig.DefaultPath(), absolute ("" if
// unknown).
func defaultConfigPath() string {
	p, err := clientconfig.DefaultPath()
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// defaultIgnored reports whether a deploy never uploads the relative path
// rel ("/"-separated) under the default ignore rules. A folder's
// .backyardignore or its project's "ignore" list may re-include some of
// them (e.g. "!.*"), so deploys check again with those (configPublished).
func defaultIgnored(rel string) bool {
	return deploy.NewMatcher(deploy.DefaultIgnore()).Ignored(rel, false)
}

// configPublished returns a CLI config file holding credentials that a
// deploy of dir with the project's ignore lines would upload ("" when
// none): sensitiveDir only applies the default rules, which "!" lines in
// dir/.backyardignore or .backyard.json can override for dotfiles (the
// protected rules cover .git and the project files only).
func (s *server) configPublished(dir string, ignore []string) (string, error) {
	real := realPath(dir)
	var m *deploy.Matcher
	for _, cfg := range s.configFiles() {
		if !within(real, cfg) {
			continue
		}
		rel, err := filepath.Rel(real, cfg)
		if err != nil {
			return cfg, nil
		}
		if m == nil {
			patterns, err := deploy.IgnorePatterns(real, ignore)
			if err != nil {
				return "", err
			}
			m = deploy.NewMatcher(patterns)
		}
		if !m.Ignored(filepath.ToSlash(rel), false) {
			return cfg, nil
		}
	}
	return "", nil
}

// realPath resolves the symlinks of the longest existing prefix of the
// absolute, clean path p and appends the rest unchanged.
func realPath(p string) string {
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within reports whether p is root or inside it (both absolute and clean).
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ---------------------------------------------------------------------------
// Remote files

// binaryExts are file types backyard_read_site_file refuses without
// downloading them.
var binaryExts = []string{
	".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".ico", ".bmp", ".tif", ".tiff", ".heic", ".psd",
	".pdf", ".zip", ".gz", ".tgz", ".bz2", ".xz", ".zst", ".br", ".7z", ".rar", ".tar", ".jar",
	".wasm", ".woff", ".woff2", ".ttf", ".otf", ".eot",
	".mp3", ".mp4", ".m4a", ".m4v", ".wav", ".ogg", ".oga", ".webm", ".mov", ".avi", ".mkv", ".flac", ".aac",
	".glb", ".bin", ".exe", ".dll", ".so", ".dylib", ".class", ".parquet", ".sqlite", ".db",
	".xls", ".xlsx", ".doc", ".docx", ".ppt", ".pptx",
}

// binaryExt reports whether p has the extension of a binary format.
func binaryExt(p string) bool {
	return slices.Contains(binaryExts, strings.ToLower(path.Ext(p)))
}

// looksBinary reports whether b is not readable text: invalid UTF-8, any
// NUL byte, or more than 1% control characters other than tab, newlines,
// form feed and escape.
func looksBinary(b []byte) bool {
	if !utf8.Valid(b) {
		return true
	}
	ctrl := 0
	for _, c := range b {
		switch {
		case c == 0:
			return true
		case c == '\t', c == '\n', c == '\r', c == '\f', c == '\v', c == 0x1b:
		case c < 0x20, c == 0x7f:
			ctrl++
		}
	}
	return ctrl > 0 && ctrl*100 > len(b)
}

// ---------------------------------------------------------------------------
// Credentials

// minSecretLen is the shortest credential redacted by value; shorter
// strings would match ordinary text.
const minSecretLen = 8

// redacted replaces credentials in tool results.
const redacted = "[REDACTED]"

// remember records the credentials of a session so they are redacted from
// tool results even after the config file changed (a refreshed token).
func (s *server) remember(sess *clientconfig.Session) {
	if sess == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	for _, v := range []string{sess.Token, sess.Profile.Token, sess.Profile.RefreshToken} {
		if len(v) >= minSecretLen {
			s.seen[v] = true
		}
	}
}

// secrets returns every credential the server may know — flag and
// environment tokens, every profile's tokens and those of past sessions —
// longest first.
func (s *server) secrets() []string {
	set := map[string]bool{}
	add := func(v string) {
		if v = strings.TrimSpace(v); len(v) >= minSecretLen {
			set[v] = true
		}
	}
	add(s.o.Overrides.Token)
	add(os.Getenv(clientconfig.EnvToken))
	s.mu.Lock()
	for v := range s.seen {
		add(v)
	}
	s.mu.Unlock()
	if p := s.configPath(); p != "" {
		if f, err := clientconfig.Load(p); err == nil {
			for _, prof := range f.Profiles {
				if prof != nil {
					add(prof.Token)
					add(prof.RefreshToken)
				}
			}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// configPath returns the CLI config file path ("" if unknown).
func (s *server) configPath() string {
	if s.o.ConfigPath != "" {
		return s.o.ConfigPath
	}
	p, err := clientconfig.DefaultPath()
	if err != nil {
		return ""
	}
	return p
}

// redactSecrets replaces every occurrence of a known credential in text.
func redactSecrets(text string, secrets []string) string {
	for _, v := range secrets {
		text = strings.ReplaceAll(text, v, redacted)
	}
	return text
}

var (
	// authSchemeRe matches HTTP credentials ("Bearer <token>"); 16+
	// characters so prose such as "bearer credentials" is left alone.
	authSchemeRe = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{16,}`)
	// tokenParamRe matches token parameters and JSON fields.
	tokenParamRe = regexp.MustCompile(`(?i)\b((?:access_|refresh_|id_)?token)=[A-Za-z0-9._~+/%=-]{8,}`)
	tokenJSONRe  = regexp.MustCompile(`(?i)"((?:access_|refresh_|id_)?token)"\s*:\s*"[^"]{8,}"`)
	jwtRe        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	urlRe        = regexp.MustCompile("https?://[^\\s\"'<>`]+")
)

// redactError removes credentials from an error message: known tokens,
// bearer credentials, token parameters and JWTs, and the capability parts
// of URLs (query strings such as presigned signatures, userinfo, and the
// upload tokens in gateway upload paths).
func redactError(msg string, secrets []string) string {
	msg = redactSecrets(msg, secrets)
	msg = urlRe.ReplaceAllStringFunc(msg, redactURL)
	msg = authSchemeRe.ReplaceAllString(msg, "$1 "+redacted)
	msg = tokenParamRe.ReplaceAllString(msg, "$1="+redacted)
	msg = tokenJSONRe.ReplaceAllString(msg, `"$1":"`+redacted+`"`)
	return jwtRe.ReplaceAllString(msg, redacted)
}

// redactURL strips the userinfo, query, fragment and upload token of a URL
// found in a message; other URLs are returned unchanged.
func redactURL(raw string) string {
	trail := ""
	for raw != "" && strings.ContainsRune(":,.;)]}", rune(raw[len(raw)-1])) {
		trail, raw = raw[len(raw)-1:]+trail, raw[:len(raw)-1]
	}
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			return raw[:i] + "?" + redacted + trail
		}
		return raw + trail
	}
	i := strings.Index(u.Path, "/uploads/")
	if u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && i < 0 {
		return raw + trail
	}
	q := ""
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		q = "?" + redacted
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	upload := ""
	if i >= 0 {
		u.Path, u.RawPath, upload = u.Path[:i], "", "/uploads/"+redacted
	}
	return u.String() + upload + q + trail
}

// scrub keeps credentials out of a tool's result, structured output and
// error.
func scrub[Out any](s *server, res *mcp.CallToolResult, out Out, err error) (*mcp.CallToolResult, Out, error) {
	secrets := s.secrets()
	if err != nil {
		if msg := redactError(err.Error(), secrets); msg != err.Error() {
			err = errors.New(msg)
		}
	}
	if len(secrets) == 0 {
		return res, out, err
	}
	if res != nil {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				tc.Text = redactSecrets(tc.Text, secrets)
			}
		}
	}
	b, mErr := json.Marshal(out)
	if mErr != nil || !containsSecret(b, secrets) {
		return res, out, err
	}
	var clean Out
	if uErr := json.Unmarshal(redactJSON(b, secrets), &clean); uErr != nil {
		var zero Out
		return nil, zero, errors.New("the result was withheld because it contained a credential")
	}
	return res, clean, err
}

// jsonForms returns the forms a secret takes inside JSON text.
func jsonForms(v string) []string {
	forms := []string{v}
	if b, err := json.Marshal(v); err == nil {
		if e := string(b[1 : len(b)-1]); e != v {
			forms = append(forms, e)
		}
	}
	return forms
}

func containsSecret(b []byte, secrets []string) bool {
	for _, v := range secrets {
		for _, f := range jsonForms(v) {
			if bytes.Contains(b, []byte(f)) {
				return true
			}
		}
	}
	return false
}

func redactJSON(b []byte, secrets []string) []byte {
	for _, v := range secrets {
		for _, f := range jsonForms(v) {
			b = bytes.ReplaceAll(b, []byte(f), []byte(redacted))
		}
	}
	return b
}
